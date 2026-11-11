package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/ItzTas/monitor-hub/internal/core"
)

const (
	telegramAPI      = "https://api.telegram.org"
	telegramMaxBytes = 4000
)

type telegram struct {
	api       string
	token     core.Secret
	chatID    string
	userAgent string
	client    *http.Client
}

type telegramMessage struct {
	ChatID      string `json:"chat_id"`
	Text        string `json:"text"`
	LinkPreview struct {
		Disabled bool `json:"is_disabled"`
	} `json:"link_preview_options"`
}

func newTelegram(cfg core.Telegram, api, userAgent string) *telegram {
	return &telegram{api: api, token: cfg.Token, chatID: cfg.ChatID, userAgent: userAgent, client: newHTTPClient()}
}

func (t *telegram) Name() string {
	return "telegram"
}

func (t *telegram) Send(ctx context.Context, msg Message) error {
	payload := telegramMessage{ChatID: t.chatID, Text: cut("monitor-hub: "+msg.Subject+"\n\n"+msg.Body, telegramMaxBytes)}
	payload.LinkPreview.Disabled = true
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("could not encode the message: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, t.api+"/bot"+t.token.Reveal()+"/sendMessage", bytes.NewReader(body))
	if err != nil {
		return errBadRequest
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("User-Agent", t.userAgent)
	status, answer, err := post(t.client, request)
	if err != nil {
		return t.redact(err)
	}
	if status >= 200 && status < 300 {
		return nil
	}
	var reply struct {
		Description string `json:"description"`
	}
	if json.Unmarshal(answer, &reply) != nil || reply.Description == "" {
		return expectSuccess(status)
	}
	return t.redact(fmt.Errorf("the server answered %d: %s", status, cut(reply.Description, 200)))
}

func (t *telegram) redact(err error) error {
	token := t.token.Reveal()
	if token == "" || !strings.Contains(err.Error(), token) {
		return err
	}
	return errors.New(strings.ReplaceAll(err.Error(), token, "[redacted]"))
}
