package whatsapp

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/ItzTas/monitor-hub/src/internal/notify"
)

const (
	channelName = "whatsapp"
	groupSuffix = "@g.us"
	sendTimeout = 2 * time.Minute
)

type Channel struct {
	gateway     Gateway
	recipients  []string
	sendTimeout time.Duration
	mu          sync.Mutex
	chats       map[string]string
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

func NewChannel(gateway Gateway, recipients []string) *Channel {
	return &Channel{
		gateway:     gateway,
		recipients:  recipients,
		sendTimeout: sendTimeout,
		chats:       map[string]string{},
	}
}

func (c *Channel) Name() string {
	return channelName
}

func (c *Channel) Ready(ctx context.Context) bool {
	return c.gateway.Ready(ctx)
}

func (c *Channel) Send(ctx context.Context, msg notify.Message) error {
	if err := c.gateway.Connect(ctx); err != nil {
		return redacted(err)
	}
	ctx, cancel := context.WithTimeout(ctx, c.sendTimeout)
	defer cancel()
	text := "monitor-hub: " + msg.Subject + "\n\n" + msg.Body
	var errs []error
	for i, recipient := range c.recipients {
		if err := c.sendTo(ctx, recipient, text); err != nil {
			errs = append(errs, fmt.Errorf("recipient %d of %d: %w", i+1, len(c.recipients), err))
		}
	}
	return redacted(errors.Join(errs...))
}

func (c *Channel) sendTo(ctx context.Context, recipient, text string) error {
	chat, err := c.chat(ctx, recipient)
	if err != nil {
		return err
	}
	return c.gateway.SendText(ctx, chat, text)
}

func (c *Channel) chat(ctx context.Context, recipient string) (string, error) {
	if strings.HasSuffix(recipient, groupSuffix) {
		return recipient, nil
	}
	c.mu.Lock()
	chat, known := c.chats[recipient]
	c.mu.Unlock()
	if known {
		return chat, nil
	}
	chat, err := c.gateway.Lookup(ctx, recipient)
	if err != nil {
		return "", err
	}
	c.mu.Lock()
	c.chats[recipient] = chat
	c.mu.Unlock()
	return chat, nil
}

func redacted(err error) error {
	if err == nil {
		return nil
	}
	return &redactedError{text: redact(err.Error()), cause: err}
}
