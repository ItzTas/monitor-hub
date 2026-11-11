package core

import (
	"io/fs"
	"log/slog"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

func TestLoadReadsEveryVariable(t *testing.T) {
	cfg, err := Load([]string{
		"DATABASE_URL=postgres://hub@db/hub",
		"HTTP_ADDRESS=127.0.0.1:9000",
		"PUBLIC_URL=https://hub.example.com/",
		"API_TOKEN=api-secret",
		"FEED_TOKEN=feed-secret",
		"PLUGINS_DIR=/srv/plugins/",
		"DATA_DIR=/srv/data",
		"LOG_LEVEL=debug",
		"UNRELATED=x=y",
	}, noFiles)
	if err != nil {
		t.Fatal(err)
	}
	want := Config{
		DatabaseURL: "postgres://hub@db/hub",
		HTTPAddress: "127.0.0.1:9000",
		PublicURL:   "https://hub.example.com",
		APIToken:    "api-secret",
		FeedToken:   "feed-secret",
		PluginsDir:  "/srv/plugins",
		DataDir:     "/srv/data",
		LogLevel:    slog.LevelDebug,
	}
	if !reflect.DeepEqual(cfg, want) {
		t.Errorf("config = %+v\nwant     %+v", cfg, want)
	}
}

func TestLoadFillsDefaults(t *testing.T) {
	cfg, err := Load([]string{"DATABASE_URL=postgres://db", "API_TOKEN=secret"}, noFiles)
	if err != nil {
		t.Fatal(err)
	}
	want := Config{
		DatabaseURL: "postgres://db",
		HTTPAddress: ":8080",
		APIToken:    "secret",
		PluginsDir:  "/opt/monitor-hub/plugins",
		DataDir:     "/data",
		LogLevel:    slog.LevelInfo,
	}
	if !reflect.DeepEqual(cfg, want) {
		t.Errorf("config = %+v\nwant     %+v", cfg, want)
	}
}

func TestLoadRejectsInvalidValues(t *testing.T) {
	cases := map[string][]string{
		"no database":          {"API_TOKEN=secret"},
		"no api token":         {"DATABASE_URL=postgres://db"},
		"relative plugins dir": {"DATABASE_URL=postgres://db", "API_TOKEN=secret", "PLUGINS_DIR=plugins"},
		"relative data dir":    {"DATABASE_URL=postgres://db", "API_TOKEN=secret", "DATA_DIR=./data"},
		"unknown log level":    {"DATABASE_URL=postgres://db", "API_TOKEN=secret", "LOG_LEVEL=loud"},
	}
	for name, environ := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Load(environ, noFiles); err == nil {
				t.Error("Load accepted it")
			}
		})
	}
}

func TestLoadDatabaseURLNeedsNothingElse(t *testing.T) {
	cases := map[string]struct {
		environ []string
		files   map[string]string
		want    string
		problem string
	}{
		"direct":    {[]string{"DATABASE_URL=postgres://hub@db/hub", "SMTP_HOST=smtp.example.org"}, nil, "postgres://hub@db/hub", ""},
		"from file": {[]string{"DATABASE_URL_FILE=/run/secrets/db"}, map[string]string{"/run/secrets/db": "postgres://hub:pw@db/hub\n"}, "postgres://hub:pw@db/hub", ""},
		"missing":   {[]string{"API_TOKEN=secret"}, nil, "", "DATABASE_URL is empty: set the Postgres address (or set DATABASE_URL_FILE)"},
		"both":      {[]string{"DATABASE_URL=postgres://db", "DATABASE_URL_FILE=/run/secrets/db"}, nil, "", "set either DATABASE_URL or DATABASE_URL_FILE, not both"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := LoadDatabaseURL(tc.environ, files(tc.files))
			if tc.problem == "" && (err != nil || got != tc.want) {
				t.Errorf("LoadDatabaseURL = %q, %v; want %q", got, err, tc.want)
			}
			if tc.problem != "" && (err == nil || !strings.Contains(err.Error(), tc.problem)) {
				t.Errorf("LoadDatabaseURL = %q, %v; want an error with %q", got, err, tc.problem)
			}
		})
	}
}

func noFiles(path string) ([]byte, error) {
	return nil, &fs.PathError{Op: "open", Path: path, Err: fs.ErrNotExist}
}

func files(contents map[string]string) func(string) ([]byte, error) {
	return func(path string) ([]byte, error) {
		content, ok := contents[path]
		if !ok {
			return noFiles(path)
		}
		return []byte(content), nil
	}
}

func environ(extra ...string) []string {
	return append([]string{"DATABASE_URL=postgres://db", "API_TOKEN=secret"}, extra...)
}

func TestLoadReadsTheChannels(t *testing.T) {
	cfg, err := Load(environ(
		"SMTP_HOST=smtp.example.org",
		"SMTP_USERNAME=monitor",
		"SMTP_PASSWORD=smtp-pw",
		"SMTP_FROM=monitor-hub <monitor@example.org>",
		"SMTP_TO=ana@example.org, Bia <bia@example.org>",
		"SMTP_TLS=TLS",
		"TELEGRAM_TOKEN=123456789:AAEabcdefghijklmnopqrstuvwx",
		"TELEGRAM_CHAT_ID=-1001234567890",
		"NTFY_URL=https://ntfy.example.org/hub-topic/",
		"NTFY_TOKEN=tk_0123456789",
		"WEBHOOK_URL=https://hooks.example.org/in?key=abc",
		"WEBHOOK_SECRET=0123456789abcdef",
		"WHATSAPP_RECIPIENTS=+55 (95) 99123-4567, 120363012345678901@g.us,, 120363012345678901-1600000000@g.us",
	), noFiles)
	if err != nil {
		t.Fatal(err)
	}
	want := Channels{
		Email: &Email{
			Host: "smtp.example.org", Port: 465, Username: "monitor", Password: NewSecret("smtp-pw"),
			From: "monitor-hub <monitor@example.org>", To: []string{"ana@example.org", "bia@example.org"}, TLS: TLSImplicit,
		},
		Telegram: &Telegram{Token: NewSecret("123456789:AAEabcdefghijklmnopqrstuvwx"), ChatID: "-1001234567890"},
		Ntfy:     &Ntfy{URL: "https://ntfy.example.org/hub-topic", Token: NewSecret("tk_0123456789")},
		Webhook:  &Webhook{URL: "https://hooks.example.org/in?key=abc", Secret: NewSecret("0123456789abcdef")},
		WhatsApp: &WhatsApp{Recipients: []string{"5595991234567", "120363012345678901@g.us", "120363012345678901-1600000000@g.us"}},
	}
	if !reflect.DeepEqual(cfg.Channels, want) {
		t.Errorf("channels = %+v\nwant       %+v", cfg.Channels, want)
	}
}

func TestLoadPicksTheSMTPPortFromTheTLSMode(t *testing.T) {
	cases := map[string]struct {
		extra []string
		tls   string
		port  int
	}{
		"default":     {nil, TLSStartTLS, 587},
		"implicit":    {[]string{"SMTP_TLS=tls"}, TLSImplicit, 465},
		"plain":       {[]string{"SMTP_TLS=none"}, TLSNone, 25},
		"set by hand": {[]string{"SMTP_TLS=starttls", "SMTP_PORT=2525"}, TLSStartTLS, 2525},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			cfg, err := Load(environ(append([]string{"SMTP_HOST=smtp.example.org", "SMTP_FROM=a@example.org", "SMTP_TO=b@example.org"}, tc.extra...)...), noFiles)
			if err != nil {
				t.Fatal(err)
			}
			if email := cfg.Channels.Email; email.TLS != tc.tls || email.Port != tc.port {
				t.Errorf("email = %+v, want TLS %s on port %d", email, tc.tls, tc.port)
			}
		})
	}
}

func TestLoadRejectsInvalidChannels(t *testing.T) {
	smtp := []string{"SMTP_HOST=smtp.example.org", "SMTP_FROM=a@example.org", "SMTP_TO=b@example.org"}
	cases := map[string]struct {
		extra []string
		want  []string
	}{
		"smtp without recipients":    {[]string{"SMTP_HOST=smtp.example.org", "SMTP_FROM=a@example.org"}, []string{"SMTP_TO is required"}},
		"smtp without sender":        {[]string{"SMTP_HOST=smtp.example.org", "SMTP_TO=b@example.org"}, []string{"SMTP_FROM is required"}},
		"smtp sender not an address": {append(smtp, "SMTP_FROM=not an address"), []string{`SMTP_FROM: invalid email address "not an address"`}},
		"smtp recipients not a list": {append(smtp, "SMTP_TO=b@example.org;;c"), []string{"SMTP_TO: invalid email address list"}},
		"smtp username alone":        {append(smtp, "SMTP_USERNAME=monitor"), []string{"SMTP_USERNAME and SMTP_PASSWORD go together: set both or neither"}},
		"smtp password alone":        {append(smtp, "SMTP_PASSWORD=pw"), []string{"SMTP_USERNAME and SMTP_PASSWORD go together: set both or neither"}},
		"smtp unknown tls mode":      {append(smtp, "SMTP_TLS=ssl"), []string{`SMTP_TLS: use starttls, tls or none (got "ssl")`}},
		"smtp password in the clear": {append(smtp, "SMTP_TLS=none", "SMTP_USERNAME=monitor", "SMTP_PASSWORD=pw"), []string{"SMTP_TLS=none would send the password unencrypted; use starttls or tls"}},
		"smtp port out of range":     {append(smtp, "SMTP_PORT=70000"), []string{`SMTP_PORT: use a whole number from 1 to 65535 (got "70000")`}},
		"telegram token malformed":   {[]string{"TELEGRAM_TOKEN=abc", "TELEGRAM_CHAT_ID=42"}, []string{"TELEGRAM_TOKEN: this does not look like a bot token"}},
		"telegram without chat":      {[]string{"TELEGRAM_TOKEN=123456789:AAEabcdefghijklmnopqrstuvwx"}, []string{"TELEGRAM_CHAT_ID is required"}},
		"telegram chat malformed":    {[]string{"TELEGRAM_TOKEN=123456789:AAEabcdefghijklmnopqrstuvwx", "TELEGRAM_CHAT_ID=chat"}, []string{"TELEGRAM_CHAT_ID: use the numeric chat id"}},
		"ntfy without topic":         {[]string{"NTFY_URL=https://ntfy.example.org/"}, []string{"NTFY_URL: end the address with the topic"}},
		"ntfy not http":              {[]string{"NTFY_URL=ftp://ntfy.example.org/topic"}, []string{"NTFY_URL: use a full address starting with https://"}},
		"ntfy with credentials":      {[]string{"NTFY_URL=https://user:pw@ntfy.example.org/topic"}, []string{"NTFY_URL: remove the username and password from the address"}},
		"ntfy with a query":          {[]string{"NTFY_URL=https://ntfy.example.org/topic?x=1"}, []string{"NTFY_URL: remove everything after ? or #"}},
		"webhook without secret":     {[]string{"WEBHOOK_URL=https://hooks.example.org/in"}, []string{"WEBHOOK_SECRET is required; it signs the webhook body"}},
		"webhook secret too short":   {[]string{"WEBHOOK_URL=https://hooks.example.org/in", "WEBHOOK_SECRET=short"}, []string{"WEBHOOK_SECRET: use at least 16 characters"}},
		"webhook with a fragment":    {[]string{"WEBHOOK_URL=https://hooks.example.org/in#x", "WEBHOOK_SECRET=0123456789abcdef"}, []string{"WEBHOOK_URL: remove everything after ? or #"}},
		"every problem at once":      {[]string{"TELEGRAM_TOKEN=abc", "NTFY_URL=https://ntfy.example.org"}, []string{"TELEGRAM_TOKEN: this does not look like a bot token", "TELEGRAM_CHAT_ID is required", "NTFY_URL: end the address with the topic"}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Load(environ(tc.extra...), noFiles)
			if err == nil {
				t.Fatal("Load accepted it")
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not mention %q", err, want)
				}
			}
		})
	}
}

func TestWhatsAppRecipientProblemsNameThePositionOnly(t *testing.T) {
	cases := map[string]struct {
		value string
		want  []string
	}{
		"every entry malformed": {"8007770, grupo@g.us, 88 77 6655-ABCD", []string{
			"WHATSAPP_RECIPIENTS: entry 1 of 3 is neither a phone number with country and area code (e.g. 5595991234567) nor a group id (ending in @g.us)",
			"WHATSAPP_RECIPIENTS: entry 2 of 3 is neither a phone number with country and area code (e.g. 5595991234567) nor a group id (ending in @g.us)",
			"WHATSAPP_RECIPIENTS: entry 3 of 3 is neither a phone number with country and area code (e.g. 5595991234567) nor a group id (ending in @g.us)",
		}},
		"one malformed among valid ones": {"+55 (95) 99123-4567, 120363012345678901@g.us, 4433221", []string{
			"WHATSAPP_RECIPIENTS: entry 3 of 3 is neither a phone number with country and area code (e.g. 5595991234567) nor a group id (ending in @g.us)",
		}},
		"no entry": {", ,", []string{"WHATSAPP_RECIPIENTS: list at least one phone number or group id"}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Load(environ("WHATSAPP_RECIPIENTS="+tc.value), noFiles)
			if err == nil {
				t.Fatal("Load accepted it")
			}
			want := "invalid configuration:\n  - " + strings.Join(tc.want, "\n  - ")
			if err.Error() != want {
				t.Errorf("error:\n%v\nwant:\n%s", err, want)
			}
			for _, digits := range regexp.MustCompile(`\d+`).FindAllString(tc.value, -1) {
				if len(digits) > 1 && strings.Contains(strings.ReplaceAll(err.Error(), "5595991234567", ""), digits) {
					t.Errorf("the error shows %q from the input:\n%v", digits, err)
				}
			}
		})
	}
}

func TestChannelErrorsNeverShowSecrets(t *testing.T) {
	_, err := Load(environ(
		"SMTP_HOST=smtp.example.org",
		"SMTP_TLS=none",
		"SMTP_USERNAME=monitor",
		"SMTP_PASSWORD=smtp-secret-1",
		"SMTP_FROM=bad",
		"TELEGRAM_TOKEN=telegram-secret-2",
		"TELEGRAM_CHAT_ID=bad",
		"NTFY_URL=https://user:ntfy-secret-3@ntfy.example.org/topic",
		"NTFY_TOKEN=ntfy-secret-4",
		"WEBHOOK_URL=ftp://hooks.example.org/in?key=webhook-secret-5",
		"WEBHOOK_SECRET=hook-6",
	), noFiles)
	if err == nil {
		t.Fatal("want an error")
	}
	for _, value := range []string{"smtp-secret-1", "telegram-secret-2", "ntfy-secret-3", "ntfy-secret-4", "webhook-secret-5", "hook-6"} {
		if strings.Contains(err.Error(), value) {
			t.Errorf("the error shows %q:\n%v", value, err)
		}
	}
}
