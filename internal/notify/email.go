package notify

import (
	"context"
	"crypto/tls"
	"fmt"

	"github.com/wneessen/go-mail"

	"github.com/ItzTas/monitor-hub/internal/core"
)

type email struct {
	cfg       core.Email
	userAgent string
	tlsConfig *tls.Config
}

func newEmail(cfg core.Email, userAgent string, tlsConfig *tls.Config) *email {
	return &email{cfg: cfg, userAgent: userAgent, tlsConfig: tlsConfig}
}

func (e *email) Name() string {
	return "email"
}

func (e *email) Send(ctx context.Context, msg Message) error {
	m := mail.NewMsg()
	if err := m.From(e.cfg.From); err != nil {
		return fmt.Errorf("invalid SMTP_FROM: %w", err)
	}
	if err := m.To(e.cfg.To...); err != nil {
		return fmt.Errorf("invalid SMTP_TO: %w", err)
	}
	m.Subject("[monitor-hub] " + msg.Subject)
	m.SetDate()
	m.SetMessageID()
	m.SetUserAgent(e.userAgent)
	m.SetBodyString(mail.TypeTextPlain, msg.Body)
	client, err := mail.NewClient(e.cfg.Host, e.options()...)
	if err != nil {
		return fmt.Errorf("could not set up the SMTP client: %w", err)
	}
	if err := client.DialAndSendWithContext(ctx, m); err != nil {
		return fmt.Errorf("could not send the email: %w", err)
	}
	return nil
}

func (e *email) options() []mail.Option {
	security := map[string]mail.Option{
		core.TLSStartTLS: mail.WithTLSPolicy(mail.TLSMandatory),
		core.TLSImplicit: mail.WithSSL(),
		core.TLSNone:     mail.WithTLSPolicy(mail.NoTLS),
	}
	options := []mail.Option{mail.WithPort(e.cfg.Port), mail.WithTimeout(httpTimeout), security[e.cfg.TLS]}
	if e.tlsConfig != nil {
		options = append(options, mail.WithTLSConfig(e.tlsConfig))
	}
	if e.cfg.Username != "" {
		options = append(options, mail.WithSMTPAuth(mail.SMTPAuthAutoDiscover), mail.WithUsername(e.cfg.Username), mail.WithPassword(e.cfg.Password.Reveal()))
	}
	return options
}
