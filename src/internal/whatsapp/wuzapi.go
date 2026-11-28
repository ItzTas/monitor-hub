package whatsapp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/ItzTas/monitor-hub/src/internal/core"
	"github.com/ItzTas/monitor-hub/src/internal/notify"
)

const (
	wuzapiReadyTimeout   = 5 * time.Second
	maxWuzAPIErrorLength = 200
)

var errUnreadable = errors.New("WuzAPI sent an answer monitor-hub could not read")

// WuzAPI is the gateway that sends through a WuzAPI server over HTTP. The
// WhatsApp session lives in WuzAPI, so Connect has nothing to do.
type WuzAPI struct {
	url          string
	token        core.Secret
	userAgent    string
	client       *http.Client
	log          *slog.Logger
	readyTimeout time.Duration
	retryAfter   time.Duration
	now          func() time.Time
	mu           sync.Mutex
	ready        bool
	retryAt      time.Time
}

type wuzapiEnvelope struct {
	Data    json.RawMessage `json:"data"`
	Success bool            `json:"success"`
	Error   string          `json:"error"`
}

func NewWuzAPI(cfg core.WuzAPI, userAgent string, log *slog.Logger) *WuzAPI {
	return &WuzAPI{
		url:          cfg.URL,
		token:        cfg.Token,
		userAgent:    userAgent,
		client:       notify.NewHTTPClient(),
		log:          log,
		readyTimeout: wuzapiReadyTimeout,
		retryAfter:   retryAfter,
		now:          time.Now,
		ready:        true,
	}
}

// Ready asks WuzAPI for its session, and after a failure answers false
// without asking again until retryAfter has passed.
func (w *WuzAPI) Ready(ctx context.Context) bool {
	w.mu.Lock()
	waiting := w.now().Before(w.retryAt)
	w.mu.Unlock()
	if waiting {
		return false
	}
	err := w.sessionReady(ctx)
	w.mu.Lock()
	defer w.mu.Unlock()
	if err != nil {
		w.retryAt = w.now().Add(w.retryAfter)
	}
	if w.ready == (err == nil) {
		return w.ready
	}
	w.ready = err == nil
	if err != nil {
		w.log.Warn("WhatsApp notifications wait because WuzAPI is not ready", "reason", err.Error())
		return false
	}
	w.log.Info("WuzAPI is ready; WhatsApp notifications resume")
	return true
}

func (w *WuzAPI) sessionReady(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, w.readyTimeout)
	defer cancel()
	var session struct {
		Connected bool `json:"connected"`
		LoggedIn  bool `json:"loggedIn"`
	}
	if err := w.do(ctx, http.MethodGet, "/session/status", nil, "", &session); err != nil {
		return err
	}
	if !session.Connected {
		return errors.New("the WuzAPI session is not connected")
	}
	if !session.LoggedIn {
		return errors.New("the WuzAPI session is not logged in; pair the number in WuzAPI")
	}
	return nil
}

func (w *WuzAPI) Connect(context.Context) error {
	return nil
}

func (w *WuzAPI) Lookup(ctx context.Context, number string) (string, error) {
	var found struct {
		Users []struct {
			IsInWhatsapp bool
			JID          string
		}
	}
	if err := w.do(ctx, http.MethodPost, "/user/check", struct{ Phone []string }{[]string{"+" + number}}, "", &found); err != nil {
		return "", fmt.Errorf("could not look the number up: %w", err)
	}
	if len(found.Users) == 0 || !found.Users[0].IsInWhatsapp {
		return "", ErrNotOnWhatsApp
	}
	return found.Users[0].JID, nil
}

func (w *WuzAPI) SendText(ctx context.Context, chat, text string) error {
	var sent struct{}
	return w.do(ctx, http.MethodPost, "/chat/send/text", struct{ Phone, Body string }{chat, text}, text, &sent)
}

// do sends body as JSON and decodes the data of WuzAPI's answer into into.
// sent is the message text, kept out of any error.
func (w *WuzAPI) do(ctx context.Context, method, path string, body any, sent string, into any) error {
	var payload io.Reader = http.NoBody
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("could not encode the request: %w", err)
		}
		payload = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, w.url+path, payload)
	if err != nil {
		return err
	}
	request.Header.Set("Token", w.token.Reveal())
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("User-Agent", w.userAgent)
	status, answer, err := notify.Do(w.client, request)
	if err != nil {
		return err
	}
	var reply wuzapiEnvelope
	readable := json.Unmarshal(answer, &reply) == nil
	if status == http.StatusUnauthorized {
		return errors.New("WuzAPI refused the token (401); check WHATSAPP_WUZAPI_TOKEN")
	}
	if status < 200 || status > 299 {
		return fmt.Errorf("WuzAPI answered %d%s", status, w.detail(reply.Error, sent))
	}
	if !readable {
		return errUnreadable
	}
	if !reply.Success {
		return fmt.Errorf("WuzAPI answered %d but reported a failure%s", status, w.detail(reply.Error, sent))
	}
	if len(reply.Data) == 0 || string(reply.Data) == "null" || json.Unmarshal(reply.Data, into) != nil {
		return errUnreadable
	}
	return nil
}

// detail is WuzAPI's error text, without the token, the message text or
// phone numbers, cut to maxWuzAPIErrorLength bytes.
func (w *WuzAPI) detail(text, sent string) string {
	if text == "" {
		return ""
	}
	for _, secret := range []string{w.token.Reveal(), sent} {
		if secret != "" {
			text = strings.ReplaceAll(text, secret, "[redacted]")
		}
	}
	text = redact(text)
	if len(text) > maxWuzAPIErrorLength {
		text = strings.ToValidUTF8(text[:maxWuzAPIErrorLength], "")
	}
	return ": " + text
}
