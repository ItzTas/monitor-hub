package notify

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/ItzTas/monitor-hub/internal/core"
)

const (
	amqpName        = "amqp"
	amqpTimeout     = 30 * time.Second
	amqpRetryAfter  = time.Minute
	amqpHeartbeat   = 10 * time.Second
	amqpCloseWait   = 2 * time.Second
	amqpTestKey     = "test"
	amqpBatchKey    = "batch"
	maxKindLength   = 200
	redactedText    = "[redacted]"
	minBarePassword = 8
)

var (
	errAMQPClosed = errors.New("the AMQP channel is closed")
	percentEscape = regexp.MustCompile(`%[0-9A-Fa-f]{2}`)
)

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
		return a.failed(parent, a.redacted("", err))
	}
	a.failure = nil
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
		return nil, a.failed(parent, a.redacted("could not connect to the broker", err))
	}
	a.link = link
	return link, nil
}

func (a *amqpChannel) failed(parent context.Context, err error) error {
	_ = a.drop()
	if parent.Err() == nil && a.stop.Err() == nil {
		a.failure, a.retryAt = err, a.now().Add(a.retryAfter)
	}
	return err
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
	return credentialRedactor(a.url.Reveal()).Replace(text)
}

func credentialRedactor(address string) *strings.Replacer {
	replacements := map[string]string{address: redactedText}
	_, rest, _ := strings.Cut(address, "://")
	if end := strings.IndexAny(rest, "/?#"); end >= 0 {
		rest = rest[:end]
	}
	userinfo := rest[:max(strings.LastIndexByte(rest, '@'), 0)]
	user, written, _ := strings.Cut(userinfo, ":")
	if written != "" {
		addPasswordForms(replacements, user, written)
	}
	olds := slices.SortedFunc(maps.Keys(replacements), func(a, b string) int {
		return cmp.Or(cmp.Compare(len(b), len(a)), strings.Compare(a, b))
	})
	pairs := make([]string, 0, 2*len(olds))
	for _, old := range olds {
		pairs = append(pairs, old, replacements[old])
	}
	return strings.NewReplacer(pairs...)
}

func addPasswordForms(replacements map[string]string, user, written string) {
	spellings := []string{written}
	password := written
	if decoded, err := url.PathUnescape(written); err == nil {
		password = decoded
		spellings = append(spellings, decoded, url.QueryEscape(decoded), url.PathEscape(decoded))
	}
	bare := utf8.RuneCountInString(password) >= minBarePassword
	for _, spelling := range spellings {
		for _, form := range []string{spelling, percentEscape.ReplaceAllStringFunc(spelling, strings.ToUpper), percentEscape.ReplaceAllStringFunc(spelling, strings.ToLower)} {
			replacements[user+":"+form+"@"] = redactedText + "@"
			replacements[":"+form+"@"] = ":" + redactedText + "@"
			if bare {
				replacements[form] = redactedText
			}
		}
	}
}

func newAMQPMessage(msg Message) (amqpMessage, error) {
	if len(msg.Events) == 0 && !msg.Test {
		return amqpMessage{}, errors.New("there are no events to publish")
	}
	body, err := encodeEvents(msg)
	if err != nil {
		return amqpMessage{}, err
	}
	key, kind := amqpTestKey, amqpTestKey
	if !msg.Test {
		key, kind = amqpRouting(msg.Events)
	}
	sum := sha256.Sum256(body)
	return amqpMessage{
		key: key,
		publishing: amqp.Publishing{
			ContentType:  "application/json",
			DeliveryMode: amqp.Persistent,
			Type:         kind,
			MessageId:    hex.EncodeToString(sum[:])[:32],
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

type amqpConfirmation interface {
	wait(ctx context.Context) (bool, error)
}

type amqpWire interface {
	declare(exchange string) error
	send(ctx context.Context, exchange string, msg amqpMessage) (amqpConfirmation, error)
}

type brokerLink struct {
	conn      *amqp.Connection
	wire      amqpWire
	interrupt func()
}

type libraryWire struct {
	channel *amqp.Channel
}

func (w libraryWire) declare(exchange string) error {
	return w.channel.ExchangeDeclare(exchange, amqp.ExchangeTopic, true, false, false, false, nil)
}

func (w libraryWire) send(ctx context.Context, exchange string, msg amqpMessage) (amqpConfirmation, error) {
	confirmation, err := w.channel.PublishWithDeferredConfirmWithContext(ctx, exchange, msg.key, false, false, msg.publishing)
	if err != nil {
		return nil, err
	}
	return libraryConfirmation{confirmation}, nil
}

type libraryConfirmation struct {
	confirmation *amqp.DeferredConfirmation
}

func (c libraryConfirmation) wait(ctx context.Context) (bool, error) {
	return c.confirmation.WaitContext(ctx)
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
	return &brokerLink{
		conn:      conn,
		wire:      libraryWire{channel},
		interrupt: func() { _ = conn.CloseDeadline(time.Now()) },
	}, nil
}

func (l *brokerLink) publish(ctx context.Context, exchange string, msg amqpMessage) error {
	defer context.AfterFunc(ctx, l.interrupt)()
	if err := l.wire.declare(exchange); err != nil {
		return fmt.Errorf("could not declare the exchange %s: %w", exchange, err)
	}
	confirmation, err := l.wire.send(ctx, exchange, msg)
	if err != nil {
		return fmt.Errorf("could not publish: %w", err)
	}
	acked, err := confirmation.wait(ctx)
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
