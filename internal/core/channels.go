package core

import (
	"cmp"
	"errors"
	"fmt"
	"net/mail"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

const (
	TLSStartTLS = "starttls"
	TLSImplicit = "tls"
	TLSNone     = "none"

	minWebhookSecretLength = 16
)

type Channels struct {
	Email    *Email
	Telegram *Telegram
	Ntfy     *Ntfy
	Webhook  *Webhook
}

type Email struct {
	Host     string
	Port     int
	Username string
	Password Secret
	From     string
	To       []string
	TLS      string
}

type Telegram struct {
	Token  Secret
	ChatID string
}

type Ntfy struct {
	URL   string
	Token Secret
}

type Webhook struct {
	URL    string
	Secret Secret
}

type channelReader struct {
	env      map[string]string
	problems []string
}

func loadChannels(env map[string]string) (Channels, error) {
	r := &channelReader{env: env}
	channels := Channels{Email: r.email(), Telegram: r.telegram(), Ntfy: r.ntfy(), Webhook: r.webhook()}
	if len(r.problems) > 0 {
		return Channels{}, errors.New("invalid notification channels:\n  - " + strings.Join(r.problems, "\n  - "))
	}
	return channels, nil
}

func (r *channelReader) problem(format string, args ...any) {
	r.problems = append(r.problems, fmt.Sprintf(format, args...))
}

func (r *channelReader) text(name string) string {
	return strings.TrimSpace(r.env[name])
}

func (r *channelReader) required(name, hint string) string {
	value := r.text(name)
	if value == "" {
		r.problem("%s is required%s", name, hint)
	}
	return value
}

func (r *channelReader) secret(name string) Secret {
	value := r.env[name]
	if strings.TrimSpace(value) == "" {
		return Secret{}
	}
	return NewSecret(value)
}

func (r *channelReader) integer(name string, fallback, low, high int) int {
	value := r.text(name)
	if value == "" {
		return fallback
	}
	n, err := strconv.Atoi(value)
	if err != nil || n < low || n > high {
		r.problem("%s: use a whole number from %d to %d (got %q)", name, low, high, value)
		return fallback
	}
	return n
}

func (r *channelReader) parseURL(name, value string, allowQuery bool) (*url.URL, bool) {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		r.problem("%s: use a full address starting with https://", name)
		return nil, false
	}
	if parsed.User != nil {
		r.problem("%s: remove the username and password from the address", name)
		return nil, false
	}
	if parsed.Fragment != "" || (!allowQuery && parsed.RawQuery != "") {
		r.problem("%s: remove everything after ? or #", name)
		return nil, false
	}
	return parsed, true
}

func (r *channelReader) email() *Email {
	host := r.text("SMTP_HOST")
	if host == "" {
		return nil
	}
	mode := strings.ToLower(cmp.Or(r.text("SMTP_TLS"), TLSStartTLS))
	ports := map[string]int{TLSStartTLS: 587, TLSImplicit: 465, TLSNone: 25}
	port, ok := ports[mode]
	if !ok {
		r.problem("SMTP_TLS: use starttls, tls or none (got %q)", mode)
	}
	email := &Email{
		Host:     host,
		Port:     r.integer("SMTP_PORT", port, 1, 65535),
		Username: r.text("SMTP_USERNAME"),
		Password: r.secret("SMTP_PASSWORD"),
		From:     r.address("SMTP_FROM"),
		To:       r.addresses("SMTP_TO"),
		TLS:      mode,
	}
	if (email.Username == "") != email.Password.IsZero() {
		r.problem("SMTP_USERNAME and SMTP_PASSWORD go together: set both or neither")
	}
	if mode == TLSNone && !email.Password.IsZero() {
		r.problem("SMTP_TLS=none would send the password unencrypted; use starttls or tls")
	}
	return email
}

func (r *channelReader) address(name string) string {
	value := r.required(name, "")
	if value == "" {
		return ""
	}
	if _, err := mail.ParseAddress(value); err != nil {
		r.problem("%s: invalid email address %q", name, value)
		return ""
	}
	return value
}

func (r *channelReader) addresses(name string) []string {
	value := r.required(name, "")
	if value == "" {
		return nil
	}
	list, err := mail.ParseAddressList(value)
	if err != nil {
		r.problem("%s: invalid email address list %q", name, value)
		return nil
	}
	addresses := make([]string, 0, len(list))
	for _, address := range list {
		addresses = append(addresses, address.Address)
	}
	return addresses
}

func (r *channelReader) telegram() *Telegram {
	token := r.secret("TELEGRAM_TOKEN")
	if token.IsZero() {
		return nil
	}
	if !regexp.MustCompile(`^\d+:[A-Za-z0-9_-]{20,}$`).MatchString(token.Reveal()) {
		r.problem("TELEGRAM_TOKEN: this does not look like a bot token (format 123456789:AAE...)")
	}
	chatID := r.required("TELEGRAM_CHAT_ID", " (numeric chat id or @channel)")
	if chatID != "" && !regexp.MustCompile(`^(-?\d+|@[A-Za-z][A-Za-z0-9_]{3,})$`).MatchString(chatID) {
		r.problem("TELEGRAM_CHAT_ID: use the numeric chat id (e.g. -1001234567890) or @channel_name")
	}
	return &Telegram{Token: token, ChatID: chatID}
}

func (r *channelReader) ntfy() *Ntfy {
	value := r.text("NTFY_URL")
	if value == "" {
		return nil
	}
	parsed, ok := r.parseURL("NTFY_URL", value, false)
	if ok && strings.Trim(parsed.Path, "/") == "" {
		r.problem("NTFY_URL: end the address with the topic (e.g. https://ntfy.sh/my-topic)")
	}
	return &Ntfy{URL: strings.TrimRight(value, "/"), Token: r.secret("NTFY_TOKEN")}
}

func (r *channelReader) webhook() *Webhook {
	value := r.text("WEBHOOK_URL")
	if value == "" {
		return nil
	}
	r.parseURL("WEBHOOK_URL", value, true)
	secret := r.secret("WEBHOOK_SECRET")
	if secret.IsZero() {
		r.problem("WEBHOOK_SECRET is required; it signs the webhook body")
	}
	if !secret.IsZero() && len(secret.Reveal()) < minWebhookSecretLength {
		r.problem("WEBHOOK_SECRET: use at least %d characters", minWebhookSecretLength)
	}
	return &Webhook{URL: value, Secret: secret}
}
