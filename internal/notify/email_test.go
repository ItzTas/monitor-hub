package notify

import (
	"slices"
	"strings"
	"testing"

	"github.com/ItzTas/monitor-hub/internal/core"
	"github.com/ItzTas/monitor-hub/internal/notify/notifytest"
)

func emailConfig(server *notifytest.SMTPServer, security string) core.Email {
	return core.Email{
		Host: server.Host, Port: server.Port, TLS: security,
		From: "monitor-hub@example.org", To: []string{"fulano@example.org", "beltrano@example.org"},
	}
}

func TestEmailOverStartTLSWithLogin(t *testing.T) {
	server := notifytest.NewSMTPServer(t, notifytest.SMTPStartTLS)
	cfg := emailConfig(server, core.TLSStartTLS)
	cfg.Username, cfg.Password = "fulano", core.NewSecret("senha-smtp")
	msg := Compose("SEI", []Event{
		{Plugin: "sei", Kind: "activity", Title: "New activity on process 00000.000001/2026-00", Body: "Processo recebido na unidade", OccurredAt: t0},
		{Plugin: "sei", Kind: "document", Title: "New document on process 00000.000002/2026-00", OccurredAt: t0},
	})

	err := newEmail(cfg, "monitor-hub/test", server.ClientTLS).Send(t.Context(), msg)

	mails := server.Mails()
	if err != nil || len(mails) != 1 {
		t.Fatalf("Send = %v, %d mails", err, len(mails))
	}
	header, text, err := mails[0].Parse()
	if err != nil {
		t.Fatal(err)
	}
	if mails[0].From != cfg.From || !slices.Equal(mails[0].To, cfg.To) || header.Get("Subject") != "[monitor-hub] 2 updates from SEI" || strings.TrimSpace(text) != msg.Body {
		t.Errorf("mail from %s to %v, subject %q, text %q", mails[0].From, mails[0].To, header.Get("Subject"), text)
	}
	if logins := server.Logins(); !slices.Equal(logins, []notifytest.Login{{Username: "fulano", Password: "senha-smtp"}}) {
		t.Errorf("logins = %+v", logins)
	}
}

func TestEmailOverImplicitTLSAndWithoutTLS(t *testing.T) {
	tests := map[string]struct {
		mode     notifytest.SMTPMode
		security string
	}{
		"implicit TLS": {notifytest.SMTPImplicitTLS, core.TLSImplicit},
		"no TLS":       {notifytest.SMTPPlain, core.TLSNone},
	}
	for name, tt := range tests {
		server := notifytest.NewSMTPServer(t, tt.mode)

		err := newEmail(emailConfig(server, tt.security), "monitor-hub/test", server.ClientTLS).Send(t.Context(), NewTestMessage())

		mails := server.Mails()
		if err != nil || len(mails) != 1 {
			t.Fatalf("%s: Send = %v, %d mails", name, err, len(mails))
		}
		if header, _, err := mails[0].Parse(); err != nil || header.Get("Subject") != "[monitor-hub] test message" {
			t.Errorf("%s: subject %q (%v)", name, header.Get("Subject"), err)
		}
	}
}

func TestEmailRefusesAServerWithoutStartTLS(t *testing.T) {
	server := notifytest.NewSMTPServer(t, notifytest.SMTPPlain)
	cfg := emailConfig(server, core.TLSStartTLS)
	cfg.Username, cfg.Password = "fulano", core.NewSecret("senha-smtp")

	err := newEmail(cfg, "monitor-hub/test", server.ClientTLS).Send(t.Context(), NewTestMessage())

	if err == nil || len(server.Mails()) != 0 || len(server.Logins()) != 0 || strings.Contains(err.Error(), "senha-smtp") {
		t.Errorf("Send = %v, mails %d, logins %d", err, len(server.Mails()), len(server.Logins()))
	}
}
