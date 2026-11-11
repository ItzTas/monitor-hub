package core

import (
	"cmp"
	"net/mail"
	"net/url"
	"regexp"
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
	WhatsApp *WhatsApp
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

type WhatsApp struct {
	Recipients []string
}

func (r *reader) channels() Channels {
	return Channels{Email: r.email(), Telegram: r.telegram(), Ntfy: r.ntfy(), Webhook: r.webhook(), WhatsApp: r.whatsapp()}
}

func (r *reader) parseURL(name, value string, allowQuery bool) (*url.URL, bool) {
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

func (r *reader) email() *Email {
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

func (r *reader) address(name string) string {
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

func (r *reader) addresses(name string) []string {
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

func (r *reader) telegram() *Telegram {
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

func (r *reader) ntfy() *Ntfy {
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

func (r *reader) webhook() *Webhook {
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

func (r *reader) whatsapp() *WhatsApp {
	value := r.text("WHATSAPP_RECIPIENTS")
	if value == "" {
		return nil
	}
	var entries []string
	for item := range strings.SplitSeq(value, ",") {
		if item = strings.TrimSpace(item); item != "" {
			entries = append(entries, item)
		}
	}
	whatsapp := &WhatsApp{}
	if len(entries) == 0 {
		r.problem("WHATSAPP_RECIPIENTS: list at least one phone number or group id")
		return whatsapp
	}
	for i, entry := range entries {
		recipient, ok := whatsappRecipient(entry)
		if !ok {
			r.problem("WHATSAPP_RECIPIENTS: entry %d of %d is neither a phone number with country and area code (e.g. 5595991234567) nor a group id (ending in @g.us)", i+1, len(entries))
			continue
		}
		whatsapp.Recipients = append(whatsapp.Recipients, recipient)
	}
	return whatsapp
}

func whatsappRecipient(item string) (string, bool) {
	if strings.HasSuffix(item, "@g.us") {
		return item, regexp.MustCompile(`^\d+(-\d+)?@g\.us$`).MatchString(item)
	}
	if !regexp.MustCompile(`^\+?[\d\s().-]+$`).MatchString(item) {
		return "", false
	}
	digits := strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' {
			return r
		}
		return -1
	}, item)
	return digits, len(digits) >= 10 && len(digits) <= 15
}
