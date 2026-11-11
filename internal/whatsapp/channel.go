package whatsapp

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"

	"github.com/ItzTas/monitor-hub/internal/notify"
)

const (
	loginTimeout = 30 * time.Second
	sendTimeout  = 2 * time.Minute
)

var errLoginTimeout = fmt.Errorf("the WhatsApp session did not log in within %s", loginTimeout)

type Messenger interface {
	Paired() bool
	IsConnected() bool
	ConnectContext(ctx context.Context) error
	WaitForConnection(timeout time.Duration) bool
	IsOnWhatsApp(ctx context.Context, phones []string) ([]types.IsOnWhatsAppResponse, error)
	SendMessage(ctx context.Context, to types.JID, message *waE2E.Message, extra ...whatsmeow.SendRequestExtra) (whatsmeow.SendResponse, error)
}

type Channel struct {
	messenger  Messenger
	recipients []string
	login      chan struct{}
	mu         sync.Mutex
	numbers    map[string]types.JID
}

type redactedError struct {
	text  string
	cause error
}

func (e *redactedError) Error() string {
	return e.text
}

func (e *redactedError) Unwrap() error {
	return e.cause
}

func NewChannel(messenger Messenger, recipients []string) *Channel {
	return &Channel{messenger: messenger, recipients: recipients, login: make(chan struct{}, 1), numbers: map[string]types.JID{}}
}

func (c *Channel) Name() string {
	return "whatsapp"
}

func (c *Channel) Ready() bool {
	return c.messenger.Paired()
}

func (c *Channel) Send(ctx context.Context, msg notify.Message) error {
	if err := c.connect(ctx); err != nil {
		return redacted(err)
	}
	ctx, cancel := context.WithTimeout(ctx, sendTimeout)
	defer cancel()
	text := new("monitor-hub: " + msg.Subject + "\n\n" + msg.Body)
	var errs []error
	for i, recipient := range c.recipients {
		to, err := c.resolve(ctx, recipient)
		if err == nil {
			_, err = c.messenger.SendMessage(ctx, to, &waE2E.Message{Conversation: text})
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("recipient %d of %d: %w", i+1, len(c.recipients), err))
		}
	}
	return redacted(errors.Join(errs...))
}

func (c *Channel) connect(ctx context.Context) error {
	timer := time.NewTimer(loginTimeout)
	defer timer.Stop()
	select {
	case c.login <- struct{}{}:
	case <-ctx.Done():
		return stopped(ctx)
	case <-timer.C:
		return errLoginTimeout
	}
	done := make(chan error, 1)
	go func() {
		defer func() { <-c.login }()
		done <- c.logIn(ctx)
	}()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return stopped(ctx)
	case <-timer.C:
		return errLoginTimeout
	}
}

func (c *Channel) logIn(ctx context.Context) error {
	if !c.messenger.IsConnected() {
		if err := c.messenger.ConnectContext(ctx); err != nil && !errors.Is(err, whatsmeow.ErrAlreadyConnected) {
			return fmt.Errorf("could not connect to WhatsApp: %w", err)
		}
	}
	if !c.messenger.WaitForConnection(loginTimeout) {
		return errLoginTimeout
	}
	return nil
}

func stopped(ctx context.Context) error {
	return fmt.Errorf("could not connect to WhatsApp: %w", ctx.Err())
}

func (c *Channel) resolve(ctx context.Context, recipient string) (types.JID, error) {
	if strings.HasSuffix(recipient, "@"+types.GroupServer) {
		return types.ParseJID(recipient)
	}
	c.mu.Lock()
	jid, known := c.numbers[recipient]
	c.mu.Unlock()
	if known {
		return jid, nil
	}
	found, err := c.messenger.IsOnWhatsApp(ctx, []string{"+" + recipient})
	if err != nil {
		return types.JID{}, fmt.Errorf("could not look the number up: %w", err)
	}
	if len(found) == 0 || !found[0].IsIn {
		return types.JID{}, errors.New("the number is not on WhatsApp")
	}
	c.mu.Lock()
	c.numbers[recipient] = found[0].JID
	c.mu.Unlock()
	return found[0].JID, nil
}

func redacted(err error) error {
	if err == nil {
		return nil
	}
	return &redactedError{text: redact(err.Error()), cause: err}
}
