package core

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

func TestSecretNeverPrintsItsValue(t *testing.T) {
	const value = "s3cret-value-123"
	secret := NewSecret(value)
	type exported struct{ Password Secret }
	type unexported struct{ password Secret }

	outputs := map[string]string{}
	for _, format := range []string{"%v", "%+v", "%#v", "%s", "%q"} {
		outputs["secret "+format] = fmt.Sprintf(format, secret)
		outputs["exported field "+format] = fmt.Sprintf(format, exported{secret})
		outputs["unexported field "+format] = fmt.Sprintf(format, unexported{password: secret})
		outputs["channels "+format] = fmt.Sprintf(format, Channels{Telegram: &Telegram{Token: secret}, Webhook: &Webhook{Secret: secret}})
	}
	marshaled, err := json.Marshal(exported{secret})
	if err != nil {
		t.Fatal(err)
	}
	outputs["json"] = string(marshaled)
	text, err := secret.MarshalText()
	if err != nil {
		t.Fatal(err)
	}
	outputs["text"] = string(text)
	var textLog, jsonLog bytes.Buffer
	slog.New(slog.NewTextHandler(&textLog, nil)).Info("x", "secret", secret, "exported", exported{secret}, "unexported", unexported{password: secret})
	slog.New(slog.NewJSONHandler(&jsonLog, nil)).Info("x", "secret", secret, "exported", exported{secret}, "telegram", Telegram{Token: secret})
	outputs["slog text"] = textLog.String()
	outputs["slog json"] = jsonLog.String()

	for name, output := range outputs {
		if strings.Contains(output, value) {
			t.Errorf("%s shows the value: %s", name, output)
		}
	}
	if secret.Reveal() != value {
		t.Errorf("Reveal() = %q, want %q", secret.Reveal(), value)
	}
	if !strings.Contains(outputs["secret %v"], redacted) {
		t.Errorf("%%v = %q, want %q", outputs["secret %v"], redacted)
	}
}

func TestZeroSecret(t *testing.T) {
	for _, secret := range []Secret{{}, NewSecret("")} {
		if !secret.IsZero() || secret.Reveal() != "" {
			t.Errorf("secret = IsZero %v, Reveal %q; want an empty secret", secret.IsZero(), secret.Reveal())
		}
	}
}
