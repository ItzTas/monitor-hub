package core

import (
	"errors"
	"io/fs"
	"reflect"
	"strings"
	"testing"
)

func TestLoadReadsSecretsFromTheirFiles(t *testing.T) {
	read := files(map[string]string{
		"/run/secrets/database": "postgres://hub:pw@db/hub\n",
		"/run/secrets/api":      "api-token\r\n",
		"/run/secrets/feed":     "feed-token",
		"/run/secrets/telegram": "123456789:AAEabcdefghijklmnopqrstuvwx\n",
		"/run/secrets/ntfy":     "tk_0123456789\n\n",
		"/run/secrets/smtp":     " smtp pw \n",
		"/run/secrets/webhook":  "0123456789abcdef\n",
	})

	cfg, err := Load([]string{
		"DATABASE_URL_FILE=/run/secrets/database",
		"API_TOKEN_FILE=/run/secrets/api",
		"FEED_TOKEN_FILE=/run/secrets/feed",
		"TELEGRAM_TOKEN_FILE=/run/secrets/telegram",
		"TELEGRAM_CHAT_ID=-1001234567890",
		"NTFY_URL=https://ntfy.example.org/hub-topic",
		"NTFY_TOKEN_FILE=/run/secrets/ntfy",
		"SMTP_HOST=smtp.example.org",
		"SMTP_USERNAME=monitor",
		"SMTP_PASSWORD_FILE=/run/secrets/smtp",
		"SMTP_FROM=a@example.org",
		"SMTP_TO=b@example.org",
		"WEBHOOK_URL=https://hooks.example.org/in",
		"WEBHOOK_SECRET_FILE=/run/secrets/webhook",
	}, read)
	if err != nil {
		t.Fatal(err)
	}

	got := map[string]string{
		"DATABASE_URL":   cfg.DatabaseURL,
		"API_TOKEN":      cfg.APIToken,
		"FEED_TOKEN":     cfg.FeedToken,
		"TELEGRAM_TOKEN": cfg.Channels.Telegram.Token.Reveal(),
		"NTFY_TOKEN":     cfg.Channels.Ntfy.Token.Reveal(),
		"SMTP_PASSWORD":  cfg.Channels.Email.Password.Reveal(),
		"WEBHOOK_SECRET": cfg.Channels.Webhook.Secret.Reveal(),
	}
	want := map[string]string{
		"DATABASE_URL":   "postgres://hub:pw@db/hub",
		"API_TOKEN":      "api-token",
		"FEED_TOKEN":     "feed-token",
		"TELEGRAM_TOKEN": "123456789:AAEabcdefghijklmnopqrstuvwx",
		"NTFY_TOKEN":     "tk_0123456789\n",
		"SMTP_PASSWORD":  " smtp pw ",
		"WEBHOOK_SECRET": "0123456789abcdef",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("secrets = %q\nwant      %q", got, want)
	}
}

func TestLoadRejectsBadSecretFiles(t *testing.T) {
	read := func(path string) ([]byte, error) {
		contents := map[string]string{"/run/secrets/empty": "\n", "/run/secrets/api": "api-token"}
		if content, ok := contents[path]; ok {
			return []byte(content), nil
		}
		if path == "/run/secrets/locked" {
			return nil, &fs.PathError{Op: "open", Path: path, Err: fs.ErrPermission}
		}
		if path == "/run/secrets/dir" {
			return nil, &fs.PathError{Op: "read", Path: path, Err: errors.New("is a directory")}
		}
		return noFiles(path)
	}
	cases := map[string]struct {
		environ []string
		want    string
	}{
		"both set":          {environ("API_TOKEN_FILE=/run/secrets/api"), "set either API_TOKEN or API_TOKEN_FILE, not both"},
		"both set, channel": {environ("TELEGRAM_TOKEN=123456789:AAEabcdefghijklmnopqrstuvwx", "TELEGRAM_TOKEN_FILE=/run/secrets/api", "TELEGRAM_CHAT_ID=42"), "set either TELEGRAM_TOKEN or TELEGRAM_TOKEN_FILE, not both"},
		"empty file":        {[]string{"DATABASE_URL=postgres://db", "API_TOKEN_FILE=/run/secrets/empty"}, "API_TOKEN_FILE: /run/secrets/empty is empty"},
		"missing file":      {[]string{"DATABASE_URL_FILE=/run/secrets/database", "API_TOKEN=secret"}, "DATABASE_URL_FILE: /run/secrets/database does not exist"},
		"unreadable file":   {environ("FEED_TOKEN_FILE=/run/secrets/locked"), "FEED_TOKEN_FILE: no permission to read /run/secrets/locked"},
		"not a file":        {environ("WEBHOOK_URL=https://hooks.example.org/in", "WEBHOOK_SECRET_FILE=/run/secrets/dir"), "WEBHOOK_SECRET_FILE: could not read /run/secrets/dir"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Load(tc.environ, read)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestSecretFileProblemsNeverShowSecrets(t *testing.T) {
	read := files(map[string]string{
		"/run/secrets/database": "database-file-secret-1",
		"/run/secrets/telegram": "telegram-file-secret-2",
		"/run/secrets/webhook":  "hook-file-3",
	})

	_, err := Load([]string{
		"DATABASE_URL=database-direct-secret-4",
		"DATABASE_URL_FILE=/run/secrets/database",
		"API_TOKEN_FILE=/run/secrets/missing",
		"TELEGRAM_TOKEN_FILE=/run/secrets/telegram",
		"TELEGRAM_CHAT_ID=42",
		"WEBHOOK_URL=https://hooks.example.org/in",
		"WEBHOOK_SECRET_FILE=/run/secrets/webhook",
	}, read)
	if err == nil {
		t.Fatal("want an error")
	}
	for _, want := range []string{"DATABASE_URL", "API_TOKEN_FILE", "TELEGRAM_TOKEN", "WEBHOOK_SECRET"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not report %s:\n%v", want, err)
		}
	}
	for _, value := range []string{"database-file-secret-1", "telegram-file-secret-2", "hook-file-3", "database-direct-secret-4"} {
		if strings.Contains(err.Error(), value) {
			t.Errorf("the error shows %q:\n%v", value, err)
		}
	}
}
