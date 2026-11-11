package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/ItzTas/monitor-hub/internal/core"
)

const (
	amqpName       = "amqp"
	amqpTimeout    = 30 * time.Second
	amqpRetryAfter = time.Minute
	amqpHeartbeat  = 10 * time.Second
	amqpCloseWait  = 2 * time.Second
	amqpTestKey    = "test"
	amqpBatchKey   = "batch"
	maxKindLength  = 200
	redactedText   = "[redacted]"
)

var errAMQPClosed = errors.New("the AMQP channel is closed")

type amqpMessage struct {
	key        string
	publishing amqp.Publishing
}

type amqpLink interface {
	publish(ctx context.Context, exchange string, msg amqpMessage) error
	closed() bool
	close() error
}

type amqpChannel struct {
	url        core.Secret
	exchange   string
	limit      time.Duration
	retryAfter time.Duration
	now        func() time.Time
	dial       func(ctx context.Context, address string) (amqpLink, error)
	slot       chan struct{}
	stop       context.Context
	halt       context.CancelFunc
	link       amqpLink
	failure    error
	retryAt    time.Time
	shut       bool
}

type amqpError struct {
	text  string
	cause error
}

func (e *amqpError) Error() string {
	return e.text
}

func (e *amqpError) Unwrap() error {
	return e.cause
}

func newAMQP(cfg core.AMQP) *amqpChannel {
	stop, halt := context.WithCancel(context.Background())
	return &amqpChannel{
		url:        cfg.URL,
		exchange:   cfg.Exchange,
		limit:      amqpTimeout,
		retryAfter: amqpRetryAfter,
		now:        time.Now,
		dial:       dialBroker,
		slot:       make(chan struct{}, 1),
		stop:       stop,
		halt:       halt,
	}
}

func (a *amqpChannel) Name() string {
	return amqpName
}

func (a *amqpChannel) Send(ctx context.Context, msg Message) error {
	message, err := newAMQPMessage(msg)
	if err != nil {
		return err
	}
	parent := ctx
	ctx, cancel := context.WithTimeout(ctx, a.limit)
	defer cancel()
	defer context.AfterFunc(a.stop, cancel)()
	select {
	case a.slot <- struct{}{}:
	case <-ctx.Done():
		return a.redacted("could not reach the broker", ctx.Err())
	}
	defer func() { <-a.slot }()
	if a.shut {
		return errAMQPClosed
	}
	link, err := a.connection(ctx, parent)
	if err != nil {
		return err
	}
	if err := link.publish(ctx, a.exchange, message); err != nil {
		_ = a.drop()
		return a.redacted("", err)
	}
	return nil
}

func (a *amqpChannel) Close() error {
	a.halt()
	a.slot <- struct{}{}
	defer func() { <-a.slot }()
	a.shut = true
	return a.drop()
}

func (a *amqpChannel) connection(ctx, parent context.Context) (amqpLink, error) {
	if a.link != nil && a.link.closed() {
		_ = a.drop()
	}
	if a.link != nil {
		return a.link, nil
	}
	if a.failure != nil && a.now().Before(a.retryAt) {
		return nil, a.failure
	}
	link, err := a.dial(ctx, a.url.Reveal())
	if err != nil {
		err = a.redacted("could not connect to the broker", err)
		if parent.Err() == nil && a.stop.Err() == nil {
			a.failure, a.retryAt = err, a.now().Add(a.retryAfter)
		}
		return nil, err
	}
	a.failure, a.link = nil, link
	return link, nil
}

func (a *amqpChannel) drop() error {
	if a.link == nil {
		return nil
	}
	err := a.link.close()
	a.link = nil
	if err != nil {
		return a.redacted("could not close the broker connection", err)
	}
	return nil
}

func (a *amqpChannel) redacted(prefix string, err error) error {
	text := a.scrub(err.Error())
	if prefix != "" {
		text = prefix + ": " + text
	}
	var cause error
	for _, sentinel := range []error{context.Canceled, context.DeadlineExceeded} {
		if errors.Is(err, sentinel) {
			cause = sentinel
		}
	}
	return &amqpError{text: text, cause: cause}
}

func (a *amqpChannel) scrub(text string) string {
	address := a.url.Reveal()
	secrets := []string{address}
	if parsed, err := url.Parse(address); err == nil && parsed.User != nil {
		password, _ := parsed.User.Password()
		secrets = append(secrets, parsed.User.String(), password, url.QueryEscape(password), parsed.User.Username())
	}
	for _, secret := range secrets {
		if secret != "" {
			text = strings.ReplaceAll(text, secret, redactedText)
		}
	}
	return text
}

func newAMQPMessage(msg Message) (amqpMessage, error) {
	if len(msg.Events) == 0 && !msg.Test {
		return amqpMessage{}, errors.New("there are no events to publish")
	}
	body, err := json.Marshal(webhookPayload{Test: msg.Test, Events: msg.Events})
	if err != nil {
		return amqpMessage{}, fmt.Errorf("could not encode the events: %w", err)
	}
	key, kind := amqpTestKey, amqpTestKey
	if !msg.Test {
		key, kind = amqpRouting(msg.Events)
	}
	return amqpMessage{
		key: key,
		publishing: amqp.Publishing{
			ContentType:  "application/json",
			DeliveryMode: amqp.Persistent,
			Type:         kind,
			Body:         body,
		},
	}, nil
}

func amqpRouting(events []Event) (key, kind string) {
	if len(events) > 1 {
		return events[0].Plugin + "." + amqpBatchKey, amqpBatchKey
	}
	return events[0].Plugin + "." + routingSegment(events[0].Kind), events[0].Kind
}

func routingSegment(kind string) string {
	segment := strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-' {
			return r
		}
		return '_'
	}, kind)
	if segment == "" {
		return "_"
	}
	return segment[:min(len(segment), maxKindLength)]
}

type brokerLink struct {
	conn    *amqp.Connection
	channel *amqp.Channel
}

func dialBroker(ctx context.Context, address string) (amqpLink, error) {
	var raw net.Conn
	var unwatch func() bool
	settings := amqp.Config{
		Heartbeat:  amqpHeartbeat,
		Properties: amqp.NewConnectionProperties(),
		Dial: func(network, addr string) (net.Conn, error) {
			conn, err := new(net.Dialer).DialContext(ctx, network, addr)
			if err != nil {
				return nil, err
			}
			raw = conn
			deadline, _ := ctx.Deadline()
			if err := conn.SetDeadline(deadline); err != nil {
				return nil, errors.Join(err, conn.Close())
			}
			unwatch = context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Now()) })
			return conn, nil
		},
	}
	settings.Properties.SetClientConnectionName("monitor-hub")
	conn, err := amqp.DialConfig(address, settings)
	if err == nil && unwatch != nil && !unwatch() {
		err = errors.Join(ctx.Err(), conn.CloseDeadline(time.Now()))
	}
	if err != nil {
		if raw != nil {
			_ = raw.Close()
		}
		return nil, err
	}
	link, err := openConfirming(ctx, conn)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	return link, nil
}

func openConfirming(ctx context.Context, conn *amqp.Connection) (*brokerLink, error) {
	defer context.AfterFunc(ctx, func() { _ = conn.CloseDeadline(time.Now()) })()
	channel, err := conn.Channel()
	if err != nil {
		return nil, fmt.Errorf("could not open a channel: %w", err)
	}
	if err := channel.Confirm(false); err != nil {
		return nil, fmt.Errorf("could not turn on publisher confirms: %w", err)
	}
	return &brokerLink{conn: conn, channel: channel}, nil
}

func (l *brokerLink) publish(ctx context.Context, exchange string, msg amqpMessage) error {
	defer context.AfterFunc(ctx, func() { _ = l.conn.CloseDeadline(time.Now()) })()
	if err := l.channel.ExchangeDeclare(exchange, amqp.ExchangeTopic, true, false, false, false, nil); err != nil {
		return fmt.Errorf("could not declare the exchange %s: %w", exchange, err)
	}
	confirmation, err := l.channel.PublishWithDeferredConfirmWithContext(ctx, exchange, msg.key, false, false, msg.publishing)
	if err != nil {
		return fmt.Errorf("could not publish: %w", err)
	}
	acked, err := confirmation.WaitContext(ctx)
	if err != nil {
		return fmt.Errorf("the broker did not confirm the message in time: %w", err)
	}
	if !acked {
		return errors.New("the broker did not take the message")
	}
	return nil
}

func (l *brokerLink) closed() bool {
	return l.conn.IsClosed()
}

func (l *brokerLink) close() error {
	if err := l.conn.CloseDeadline(time.Now().Add(amqpCloseWait)); !errors.Is(err, amqp.ErrClosed) {
		return err
	}
	return nil
}
