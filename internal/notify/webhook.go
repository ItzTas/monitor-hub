package notify

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/ItzTas/monitor-hub/internal/core"
)

const signatureHeader = "X-Monitor-Hub-Signature"

type webhook struct {
	url       string
	secret    core.Secret
	userAgent string
	client    *http.Client
}

type webhookPayload struct {
	Test   bool    `json:"test,omitempty"`
	Events []Event `json:"events"`
}

func newWebhook(cfg core.Webhook, userAgent string) *webhook {
	return &webhook{url: cfg.URL, secret: cfg.Secret, userAgent: userAgent, client: newHTTPClient()}
}

func encodeEvents(msg Message) ([]byte, error) {
	body, err := json.Marshal(webhookPayload{Test: msg.Test, Events: msg.Events})
	if err != nil {
		return nil, fmt.Errorf("could not encode the events: %w", err)
	}
	return body, nil
}

func (w *webhook) Name() string {
	return "webhook"
}

func (w *webhook) Send(ctx context.Context, msg Message) error {
	body, err := encodeEvents(msg)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, w.url, bytes.NewReader(body))
	if err != nil {
		return errBadRequest
	}
	mac := hmac.New(sha256.New, []byte(w.secret.Reveal()))
	mac.Write(body)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("User-Agent", w.userAgent)
	request.Header.Set(signatureHeader, "sha256="+hex.EncodeToString(mac.Sum(nil)))
	status, _, err := post(w.client, request)
	if err != nil {
		return err
	}
	return expectSuccess(status)
}
