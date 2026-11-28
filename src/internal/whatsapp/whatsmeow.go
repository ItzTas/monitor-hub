package whatsapp

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
)

const (
	loginTimeout = 30 * time.Second
	retryAfter   = time.Minute
)

type Messenger interface {
	Paired() bool
	IsConnected() bool
	ConnectContext(ctx context.Context) error
	WaitForConnection(timeout time.Duration) bool
	IsOnWhatsApp(ctx context.Context, phones []string) ([]types.IsOnWhatsAppResponse, error)
	SendMessage(ctx context.Context, to types.JID, message *waE2E.Message, extra ...whatsmeow.SendRequestExtra) (whatsmeow.SendResponse, error)
}

// Whatsmeow is the gateway that runs WhatsApp inside the hub, with the
// session kept in the hub's database.
type Whatsmeow struct {
	messenger    Messenger
	loginTimeout time.Duration
	retryAfter   time.Duration
	now          func() time.Time
	login        chan struct{}
	mu           sync.Mutex
	failure      error
	retryAt      time.Time
}

func NewWhatsmeow(messenger Messenger) *Whatsmeow {
	return &Whatsmeow{
		messenger:    messenger,
		loginTimeout: loginTimeout,
		retryAfter:   retryAfter,
		now:          time.Now,
		login:        make(chan struct{}, 1),
	}
}

func (w *Whatsmeow) Ready(context.Context) bool {
	return w.messenger.Paired()
}

func (w *Whatsmeow) Connect(ctx context.Context) error {
	if err := w.recentFailure(); err != nil {
		return err
	}
	timer := time.NewTimer(w.loginTimeout)
	defer timer.Stop()
	select {
	case w.login <- struct{}{}:
	case <-ctx.Done():
		return stopped(ctx)
	case <-timer.C:
		return w.failed(w.loginExpired())
	}
	done := make(chan error, 1)
	go func() {
		defer func() { <-w.login }()
		done <- w.logIn(ctx)
	}()
	select {
	case err := <-done:
		return w.settle(err)
	case <-ctx.Done():
		return stopped(ctx)
	case <-timer.C:
		return w.failed(w.loginExpired())
	}
}

func (w *Whatsmeow) Lookup(ctx context.Context, number string) (string, error) {
	found, err := w.messenger.IsOnWhatsApp(ctx, []string{"+" + number})
	if err != nil {
		return "", fmt.Errorf("could not look the number up: %w", err)
	}
	if len(found) == 0 || !found[0].IsIn {
		return "", ErrNotOnWhatsApp
	}
	return found[0].JID.String(), nil
}

func (w *Whatsmeow) SendText(ctx context.Context, chat, text string) error {
	to, err := types.ParseJID(chat)
	if err != nil {
		return err
	}
	_, err = w.messenger.SendMessage(ctx, to, &waE2E.Message{Conversation: &text})
	return err
}

func (w *Whatsmeow) recentFailure() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.failure == nil || !w.now().Before(w.retryAt) {
		return nil
	}
	return w.failure
}

func (w *Whatsmeow) settle(err error) error {
	if err != nil {
		return w.failed(err)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.failure = nil
	return nil
}

func (w *Whatsmeow) failed(err error) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.failure, w.retryAt = err, w.now().Add(w.retryAfter)
	return err
}

func (w *Whatsmeow) loginExpired() error {
	return fmt.Errorf("the WhatsApp session did not log in within %s", w.loginTimeout)
}

func (w *Whatsmeow) logIn(ctx context.Context) error {
	if !w.messenger.IsConnected() {
		if err := w.messenger.ConnectContext(ctx); err != nil && !errors.Is(err, whatsmeow.ErrAlreadyConnected) {
			return fmt.Errorf("could not connect to WhatsApp: %w", err)
		}
	}
	if !w.messenger.WaitForConnection(w.loginTimeout) {
		return w.loginExpired()
	}
	return nil
}

func stopped(ctx context.Context) error {
	return fmt.Errorf("could not connect to WhatsApp: %w", ctx.Err())
}
