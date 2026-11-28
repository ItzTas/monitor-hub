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
	channelName   = "whatsapp"
	groupSuffix   = "@g.us"
	sendTimeout   = 2 * time.Minute
	recordTimeout = 10 * time.Second
)

type Channel struct {
	gateway     Gateway
	recipients  []string
	ledger      Ledger
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

func NewChannel(gateway Gateway, recipients []string, ledger Ledger) *Channel {
	return &Channel{
		gateway:     gateway,
		recipients:  recipients,
		ledger:      ledger,
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
	if len(msg.Events) == 0 {
		return c.sendToAll(ctx, text(msg))
	}
	return c.sendMissed(ctx, msg)
}

func (c *Channel) sendToAll(ctx context.Context, body string) error {
	if err := c.gateway.Connect(ctx); err != nil {
		return redacted(err)
	}
	ctx, cancel := context.WithTimeout(ctx, c.sendTimeout)
	defer cancel()
	var errs []error
	for i, recipient := range c.recipients {
		if err := c.sendTo(ctx, recipient, body); err != nil {
			errs = append(errs, fmt.Errorf("recipient %d of %d: %w", i+1, len(c.recipients), err))
		}
	}
	return redacted(errors.Join(errs...))
}

// sendMissed sends each recipient only the events the ledger has no record of
// it getting, and records each send right after it succeeds.
func (c *Channel) sendMissed(ctx context.Context, msg notify.Message) error {
	seqs := eventSeqs(msg.Events)
	reached, err := c.ledger.Reached(ctx, seqs)
	if err != nil {
		return redacted(fmt.Errorf("could not read which recipients already got these notifications: %w", err))
	}
	missed := make([][]notify.Event, len(c.recipients))
	pending := false
	for i, recipient := range c.recipients {
		missed[i] = missing(msg.Events, reached[recipient])
		pending = pending || len(missed[i]) > 0
	}
	if !pending {
		return nil
	}
	if err := c.gateway.Connect(ctx); err != nil {
		return c.failed(redacted(err), seqs, reached, make([]bool, len(c.recipients)))
	}
	sendCtx, cancel := context.WithTimeout(ctx, c.sendTimeout)
	defer cancel()
	got := make([]bool, len(c.recipients))
	var errs []error
	for i, recipient := range c.recipients {
		if len(missed[i]) == 0 {
			continue
		}
		if err := c.sendTo(sendCtx, recipient, text(messageFor(msg, missed[i]))); err != nil {
			errs = append(errs, fmt.Errorf("recipient %d of %d: %w", i+1, len(c.recipients), err))
			continue
		}
		if err := c.record(sendCtx, recipient, missed[i]); err != nil {
			errs = append(errs, fmt.Errorf("recipient %d of %d got the message, but monitor-hub could not record it, so a retry may send it again: %w", i+1, len(c.recipients), err))
			break
		}
		got[i] = true
	}
	if len(errs) == 0 {
		return nil
	}
	return c.failed(redacted(errors.Join(errs...)), seqs, reached, got)
}

// failed returns err as a PartialError when some events reached every
// recipient anyway, through the ledger or in this send.
func (c *Channel) failed(err error, seqs []int64, reached map[string]map[int64]bool, got []bool) error {
	done := c.reachedByAll(seqs, reached, got)
	if len(done) == 0 {
		return err
	}
	return &notify.PartialError{Done: done, Err: err}
}

func (c *Channel) record(ctx context.Context, recipient string, events []notify.Event) error {
	// Detached from the send deadline: a message that went out right at the
	// deadline still has to be recorded, or the retry sends it again.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), recordTimeout)
	defer cancel()
	return c.ledger.Record(ctx, recipient, eventSeqs(events))
}

func (c *Channel) reachedByAll(seqs []int64, reached map[string]map[int64]bool, got []bool) []int64 {
	var done []int64
	for _, seq := range seqs {
		all := true
		for i, recipient := range c.recipients {
			all = all && (got[i] || reached[recipient][seq])
		}
		if all {
			done = append(done, seq)
		}
	}
	return done
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

func text(msg notify.Message) string {
	return "monitor-hub: " + msg.Subject + "\n\n" + msg.Body
}

// messageFor is msg itself when the recipient missed every event, and otherwise a
// message about the events it missed.
func messageFor(msg notify.Message, missed []notify.Event) notify.Message {
	if len(missed) == len(msg.Events) {
		return msg
	}
	return notify.Compose(missed[0].DisplayName, missed)
}

func missing(events []notify.Event, reached map[int64]bool) []notify.Event {
	var missed []notify.Event
	for _, e := range events {
		if !reached[e.Seq] {
			missed = append(missed, e)
		}
	}
	return missed
}

func eventSeqs(events []notify.Event) []int64 {
	seqs := make([]int64, 0, len(events))
	for _, e := range events {
		seqs = append(seqs, e.Seq)
	}
	return seqs
}
