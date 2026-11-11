package whatsapp

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

func TestWhatsmeowLogsKeepTheirLevelAndHideNumbers(t *testing.T) {
	var out bytes.Buffer
	log := newLogger(slog.New(slog.NewTextHandler(&out, &slog.HandlerOptions{Level: slog.LevelDebug, ReplaceAttr: dropTime})), "whatsapp").Sub("Client")

	log.Errorf("failed to decrypt message from %s", "5595991234567@s.whatsapp.net")
	log.Warnf("keepalive failed for %s:%d", "559591234567", 7)
	log.Infof("connected as %s", "123456789012345@lid")
	log.Debugf("sending frame to %s", "5595991234567@s.whatsapp.net")

	want := strings.Join([]string{
		`level=ERROR msg="failed to decrypt message from [redacted]@s.whatsapp.net" module=whatsapp/Client`,
		`level=WARN msg="keepalive failed for [redacted]:7" module=whatsapp/Client`,
		`level=DEBUG msg="connected as [redacted]@lid" module=whatsapp/Client`,
	}, "\n") + "\n"
	if out.String() != want {
		t.Errorf("logs:\n%s\nwant:\n%s", out.String(), want)
	}
}

func dropTime(_ []string, attr slog.Attr) slog.Attr {
	if attr.Key == slog.TimeKey {
		return slog.Attr{}
	}
	return attr
}
