package whatsapp

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/store/sqlstore"
)

var (
	ErrNotPaired     = errors.New("not paired with WhatsApp; run monitor-hub whatsapp-pair")
	ErrAlreadyPaired = errors.New("this database already has a paired WhatsApp session")
	errCodeExpired   = errors.New("the pairing code expired before it was used; run whatsapp-pair again")
)

type Session struct {
	*whatsmeow.Client
}

func Open(ctx context.Context, db *sql.DB, log *slog.Logger) (*Session, error) {
	container := sqlstore.NewWithDB(db, "postgres", newLogger(log, "whatsapp/store"))
	if err := container.Upgrade(ctx); err != nil {
		return nil, fmt.Errorf("whatsapp: prepare the session tables: %w", err)
	}
	device, err := container.GetFirstDevice(ctx)
	if err != nil {
		return nil, fmt.Errorf("whatsapp: read the session: %w", err)
	}
	client := whatsmeow.NewClient(device, newLogger(log, "whatsapp"))
	client.SetWebsocketHTTPClient(&http.Client{Transport: http.DefaultTransport.(*http.Transport).Clone(), Timeout: loginTimeout})
	client.AddEventHandler(linkAlerts(log))
	return &Session{Client: client}, nil
}

func (s *Session) Paired() bool {
	return s.Store.ID != nil
}

func (s *Session) ConnectContext(ctx context.Context) error {
	if !s.Paired() {
		return ErrNotPaired
	}
	return s.Client.ConnectContext(ctx)
}

func (s *Session) Close() {
	s.Disconnect()
}

func (s *Session) Pair(ctx context.Context, phone string, show func(code string)) error {
	if s.Paired() {
		return ErrAlreadyPaired
	}
	events, err := s.GetQRChannel(ctx)
	if err != nil {
		return fmt.Errorf("whatsapp: prepare the pairing: %w", err)
	}
	if err := s.Client.ConnectContext(ctx); err != nil {
		return fmt.Errorf("whatsapp: connect: %w", err)
	}
	shown := false
	for item := range events {
		switch {
		case item.Event == whatsmeow.QRChannelEventCode && !shown:
			code, err := s.PairPhone(ctx, phone, true, whatsmeow.PairClientChrome, "Chrome (Linux)")
			if err != nil {
				return fmt.Errorf("whatsapp: ask for a pairing code: %w", err)
			}
			show(code)
			shown = true
		case item == whatsmeow.QRChannelSuccess:
			return nil
		case item == whatsmeow.QRChannelTimeout:
			return errCodeExpired
		case item.Event == whatsmeow.QRChannelEventError:
			return fmt.Errorf("whatsapp: pairing failed: %w", item.Error)
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return errors.New("whatsapp: the connection closed before the pairing finished")
}
