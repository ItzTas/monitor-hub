package notify

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/ItzTas/monitor-hub/internal/core"
)

const amqpPassword = "s3cr3t-Pw-9f2"

type fakeLink struct {
	mu        sync.Mutex
	exchanges []string
	messages  []amqpMessage
	err       error
	hold      bool
	dead      bool
	closes    int
	active    atomic.Int32
	overlap   atomic.Bool
}

func (l *fakeLink) publish(ctx context.Context, exchange string, msg amqpMessage) error {
	if l.active.Add(1) > 1 {
		l.overlap.Store(true)
	}
	defer l.active.Add(-1)
	time.Sleep(time.Millisecond)
	if l.hold {
		<-ctx.Done()
		return ctx.Err()
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.err != nil {
		return l.err
	}
	l.exchanges = append(l.exchanges, exchange)
	l.messages = append(l.messages, msg)
	return nil
}

func (l *fakeLink) closed() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.dead
}

func (l *fakeLink) close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.closes++
	return nil
}

func (l *fakeLink) sent() []amqpMessage {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]amqpMessage(nil), l.messages...)
}

type fakeBroker struct {
	mu      sync.Mutex
	dials   int
	addrs   []string
	links   []*fakeLink
	dialErr error
	block   bool
	prepare func(*fakeLink)
}

func (b *fakeBroker) dial(ctx context.Context, address string) (amqpLink, error) {
	b.mu.Lock()
	b.dials++
	b.addrs = append(b.addrs, address)
	err, block := b.dialErr, b.block
	b.mu.Unlock()
	if block {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if err != nil {
		return nil, err
	}
	link := &fakeLink{}
	if b.prepare != nil {
		b.prepare(link)
	}
	b.mu.Lock()
	b.links = append(b.links, link)
	b.mu.Unlock()
	return link, nil
}

func (b *fakeBroker) dialCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.dials
}

func (b *fakeBroker) link(i int) *fakeLink {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.links[i]
}

func testAMQP(address string, broker *fakeBroker) *amqpChannel {
	a := newAMQP(core.AMQP{URL: core.NewSecret(address), Exchange: "hub-test"})
	if broker != nil {
		a.dial = broker.dial
	}
	return a
}

func seiMessage(kinds ...string) Message {
	events := make([]Event, 0, len(kinds))
	for _, kind := range kinds {
		events = append(events, Event{Plugin: "sei", DisplayName: "SEI", Kind: kind, Title: "Title " + kind, OccurredAt: t0})
	}
	return Compose("SEI", events)
}

func TestAMQPRoutingKey(t *testing.T) {
	cases := map[string]struct {
		msg  Message
		key  string
		kind string
	}{
		"one event is plugin.kind":                {seiMessage("return_overdue"), "sei.return_overdue", "return_overdue"},
		"several events are plugin.batch":         {seiMessage("activity", "document"), "sei.batch", "batch"},
		"a dot in the kind cannot split the key":  {seiMessage("a.b"), "sei.a_b", "a.b"},
		"a space and a star cannot break the key": {seiMessage("new *thing"), "sei.new__thing", "new *thing"},
		"an empty kind keeps the segment":         {seiMessage(""), "sei._", ""},
		"the test message is test":                {NewTestMessage(), "test", "test"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := newAMQPMessage(tc.msg)
			if err != nil {
				t.Fatal(err)
			}
			if got.key != tc.key || got.publishing.Type != tc.kind {
				t.Errorf("key %q, type %q, want %q and %q", got.key, got.publishing.Type, tc.key, tc.kind)
			}
		})
	}
}

func TestAMQPPublishesTheWebhookDocumentAsPersistentJSON(t *testing.T) {
	for name, msg := range map[string]Message{"one event": seiMessage("activity"), "a batch": seiMessage("activity", "document"), "a test": NewTestMessage()} {
		t.Run(name, func(t *testing.T) {
			want, err := json.Marshal(webhookPayload{Test: msg.Test, Events: msg.Events})
			if err != nil {
				t.Fatal(err)
			}
			broker := &fakeBroker{}
			a := testAMQP("amqp://guest:guest@broker/", broker)

			err = a.Send(t.Context(), msg)

			sent := broker.link(0).sent()
			if err != nil || len(sent) != 1 {
				t.Fatalf("Send = %v, %d messages", err, len(sent))
			}
			got := sent[0].publishing
			if string(got.Body) != string(want) || got.ContentType != "application/json" || got.DeliveryMode != amqp.Persistent {
				t.Errorf("body %s, content type %q, delivery mode %d", got.Body, got.ContentType, got.DeliveryMode)
			}
			if exchanges := broker.link(0).exchanges; len(exchanges) != 1 || exchanges[0] != "hub-test" {
				t.Errorf("exchanges = %v", exchanges)
			}
		})
	}
}

func TestAMQPMessageIdIsTheStartOfTheBodyHash(t *testing.T) {
	first, second, other := seiMessage("activity"), seiMessage("activity"), seiMessage("document")
	ids := map[string]string{}
	for name, msg := range map[string]Message{"first": first, "second": second, "other": other} {
		got, err := newAMQPMessage(msg)
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(got.publishing.Body)
		if want := hex.EncodeToString(sum[:])[:32]; got.publishing.MessageId != want {
			t.Errorf("%s: MessageId %q, want %q", name, got.publishing.MessageId, want)
		}
		ids[name] = got.publishing.MessageId
	}
	if ids["first"] != ids["second"] || ids["first"] == ids["other"] {
		t.Errorf("ids = %v", ids)
	}
}

func TestAMQPChannelIsNamedAndNeedsNoGate(t *testing.T) {
	a := testAMQP("amqp://guest:guest@broker/", nil)
	if _, gated := Channel(a).(gate); gated || a.Name() != "amqp" {
		t.Errorf("name %q, gated %t", a.Name(), gated)
	}
}

func TestAMQPReusesTheConnectionAndReconnectsAfterAFailedPublish(t *testing.T) {
	broker := &fakeBroker{}
	a := testAMQP("amqp://guest:guest@broker/", broker)
	clock := time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC)
	a.now = func() time.Time { return clock }
	for range 2 {
		if err := a.Send(t.Context(), seiMessage("activity")); err != nil {
			t.Fatal(err)
		}
	}
	if broker.dialCount() != 1 || len(broker.link(0).sent()) != 2 {
		t.Fatalf("dials %d, messages %d", broker.dialCount(), len(broker.link(0).sent()))
	}

	broker.link(0).err = errors.New("channel closed")
	if err := a.Send(t.Context(), seiMessage("activity")); err == nil {
		t.Fatal("Send accepted a failed publish")
	}
	if broker.link(0).closes != 1 {
		t.Errorf("the failed connection was closed %d times, want 1", broker.link(0).closes)
	}

	clock = clock.Add(a.retryAfter)
	if err := a.Send(t.Context(), seiMessage("activity")); err != nil {
		t.Fatal(err)
	}
	if broker.dialCount() != 2 || len(broker.link(1).sent()) != 1 {
		t.Errorf("dials %d, messages on the new connection %d", broker.dialCount(), len(broker.link(1).sent()))
	}
}

func TestAMQPFailsFastAfterAFailedPublishAndAPublishClearsTheWindow(t *testing.T) {
	broker := &fakeBroker{prepare: func(l *fakeLink) { l.err = errors.New("exchange declared differently") }}
	a := testAMQP("amqp://hub-user:"+amqpPassword+"@broker/", broker)
	clock := time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC)
	a.now = func() time.Time { return clock }
	first := a.Send(t.Context(), seiMessage("activity"))
	second := a.Send(t.Context(), seiMessage("activity"))

	if first == nil || second == nil || second.Error() != first.Error() || broker.dialCount() != 1 {
		t.Fatalf("first %v, second %v, dials %d", first, second, broker.dialCount())
	}
	assertNoSecret(t, second, "amqp://hub-user:"+amqpPassword+"@broker/")

	clock = clock.Add(a.retryAfter)
	broker.mu.Lock()
	broker.prepare = nil
	broker.mu.Unlock()
	if err := a.Send(t.Context(), seiMessage("activity")); err != nil {
		t.Fatal(err)
	}
	broker.link(1).dead = true
	if err := a.Send(t.Context(), seiMessage("activity")); err != nil || broker.dialCount() != 3 {
		t.Errorf("after a publish succeeded: Send = %v, dials %d", err, broker.dialCount())
	}
}

func TestAMQPReconnectsWhenTheBrokerDroppedTheConnection(t *testing.T) {
	broker := &fakeBroker{}
	a := testAMQP("amqp://guest:guest@broker/", broker)
	if err := a.Send(t.Context(), seiMessage("activity")); err != nil {
		t.Fatal(err)
	}
	broker.link(0).dead = true

	if err := a.Send(t.Context(), seiMessage("activity")); err != nil {
		t.Fatal(err)
	}

	if broker.dialCount() != 2 || broker.link(0).closes != 1 {
		t.Errorf("dials %d, closes of the dropped connection %d", broker.dialCount(), broker.link(0).closes)
	}
}

func TestAMQPSendsOneAtATimeOnTheSameConnection(t *testing.T) {
	broker := &fakeBroker{}
	a := testAMQP("amqp://guest:guest@broker/", broker)
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			if err := a.Send(t.Context(), seiMessage("activity")); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if broker.link(0).overlap.Load() || len(broker.link(0).sent()) != 20 || broker.dialCount() != 1 {
		t.Errorf("overlap %t, messages %d, dials %d", broker.link(0).overlap.Load(), len(broker.link(0).sent()), broker.dialCount())
	}
}

type fakeConfirmation struct {
	acked bool
	err   error
	hold  bool
}

func (c fakeConfirmation) wait(ctx context.Context) (bool, error) {
	if c.hold {
		<-ctx.Done()
		return false, ctx.Err()
	}
	return c.acked, c.err
}

type fakeWire struct {
	declareErr error
	sendErr    error
	confirm    fakeConfirmation
	published  []string
	interrupts atomic.Int32
}

func (w *fakeWire) declare(string) error {
	return w.declareErr
}

func (w *fakeWire) send(_ context.Context, exchange string, msg amqpMessage) (amqpConfirmation, error) {
	if w.sendErr != nil {
		return nil, w.sendErr
	}
	w.published = append(w.published, exchange+"/"+msg.key)
	return w.confirm, nil
}

func publishWithin(t *testing.T, wire *fakeWire, limit time.Duration) error {
	t.Helper()
	link := &brokerLink{wire: wire, interrupt: func() { wire.interrupts.Add(1) }}
	ctx, cancel := context.WithTimeout(context.Background(), limit)
	defer cancel()
	msg, err := newAMQPMessage(seiMessage("activity"))
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- link.publish(ctx, "hub-test", msg) }()
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		t.Fatalf("publish still running after 5s with a limit of %s", limit)
		return nil
	}
}

func TestAMQPPublishSucceedsOnlyWhenTheBrokerConfirms(t *testing.T) {
	cases := map[string]struct {
		wire *fakeWire
		want string
	}{
		"the broker acks":                      {&fakeWire{confirm: fakeConfirmation{acked: true}}, ""},
		"the broker nacks":                     {&fakeWire{confirm: fakeConfirmation{acked: false}}, "the broker did not take the message"},
		"the connection closes before the ack": {&fakeWire{confirm: fakeConfirmation{err: amqp.ErrClosed}}, "the broker did not confirm the message in time: "},
		"no confirm comes before the limit":    {&fakeWire{confirm: fakeConfirmation{hold: true}}, "the broker did not confirm the message in time: context deadline exceeded"},
		"the exchange cannot be declared":      {&fakeWire{declareErr: errors.New("406")}, "could not declare the exchange hub-test: 406"},
		"the publish itself is refused":        {&fakeWire{sendErr: amqp.ErrClosed}, "could not publish: "},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			err := publishWithin(t, tc.wire, 50*time.Millisecond)

			if tc.want == "" && err != nil || tc.want != "" && (err == nil || !strings.HasPrefix(err.Error(), tc.want)) {
				t.Errorf("publish = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestAMQPPublishPassesTheExchangeAndKeyToTheWire(t *testing.T) {
	wire := &fakeWire{confirm: fakeConfirmation{acked: true}}

	err := publishWithin(t, wire, time.Second)

	if err != nil || len(wire.published) != 1 || wire.published[0] != "hub-test/sei.activity" {
		t.Errorf("publish = %v, published %v", err, wire.published)
	}
}

func TestAMQPSendEndsWithinItsOwnLimitWhenTheContextHasNoDeadline(t *testing.T) {
	cases := map[string]*fakeBroker{
		"a connect that never answers": {block: true},
		"a publish that never confirms": {prepare: func(l *fakeLink) {
			l.hold = true
		}},
	}
	for name, broker := range cases {
		t.Run(name, func(t *testing.T) {
			a := testAMQP("amqp://guest:guest@broker/", broker)
			a.limit = 50 * time.Millisecond
			done := make(chan error, 1)
			go func() { done <- a.Send(context.Background(), seiMessage("activity")) }()

			select {
			case err := <-done:
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Errorf("Send = %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("Send still running after 5s with a limit of 50ms")
			}
		})
	}
}

func TestAMQPSendEndsWhenTheDispatcherContextEnds(t *testing.T) {
	broker := &fakeBroker{block: true}
	a := testAMQP("amqp://guest:guest@broker/", broker)
	ctx, cancel := context.WithCancel(t.Context())
	time.AfterFunc(20*time.Millisecond, cancel)
	started := time.Now()

	err := a.Send(ctx, seiMessage("activity"))

	if !errors.Is(err, context.Canceled) || time.Since(started) > 2*time.Second {
		t.Errorf("Send = %v after %s", err, time.Since(started))
	}
}

func TestAMQPWaitingForTheConnectionRespectsTheContext(t *testing.T) {
	broker := &fakeBroker{block: true}
	a := testAMQP("amqp://guest:guest@broker/", broker)
	go func() { _ = a.Send(t.Context(), seiMessage("activity")) }()
	for broker.dialCount() == 0 {
		time.Sleep(time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()

	err := a.Send(ctx, seiMessage("activity"))

	if !errors.Is(err, context.DeadlineExceeded) || broker.dialCount() != 1 {
		t.Errorf("Send = %v, dials %d", err, broker.dialCount())
	}
}

func TestAMQPFailsFastForAWindowAfterAFailedConnect(t *testing.T) {
	broker := &fakeBroker{dialErr: errors.New("connection refused")}
	a := testAMQP("amqp://guest:guest@broker/", broker)
	clock := time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC)
	a.now = func() time.Time { return clock }
	first := a.Send(t.Context(), seiMessage("activity"))

	clock = clock.Add(a.retryAfter - time.Second)
	within := a.Send(t.Context(), seiMessage("activity"))
	dialsInside := broker.dialCount()

	clock = clock.Add(2 * time.Second)
	broker.mu.Lock()
	broker.dialErr = nil
	broker.mu.Unlock()
	after := a.Send(t.Context(), seiMessage("activity"))

	if first == nil || within == nil || within.Error() != first.Error() || dialsInside != 1 {
		t.Errorf("first %v, within the window %v, dials %d", first, within, dialsInside)
	}
	if after != nil || broker.dialCount() != 2 {
		t.Errorf("after the window: %v, dials %d", after, broker.dialCount())
	}
}

func TestAMQPErrorsNeverShowTheURLOrItsPassword(t *testing.T) {
	password := "p@ss/wörd 9:f+x"
	written := "p%40ss%2fw%c3%b6rd%209%3af+x"
	address := "amqp://hub-user:" + written + "@broker.example.org:5672/vhost"
	cases := map[string]struct{ shown, want string }{
		"the address":                               {fmt.Sprintf("parse %q: invalid port", address), `parse "[redacted]": invalid port`},
		"the user and password as written":          {"hub-user:" + written + "@broker.example.org", "[redacted]@broker.example.org"},
		"the password as written":                   {written, "[redacted]"},
		"the password as written in upper case hex": {"p%40ss%2Fw%C3%B6rd%209%3Af+x", "[redacted]"},
		"the password decoded":                      {password, "[redacted]"},
		"the password query escaped":                {url.QueryEscape(password), "[redacted]"},
		"the password query escaped in lower hex":   {"p%40ss%2fw%c3%b6rd+9%3af%2bx", "[redacted]"},
		"the password path escaped":                 {url.PathEscape(password), "[redacted]"},
		"the password path escaped in lower hex":    {"p@ss%2fw%c3%b6rd%209:f+x", "[redacted]"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			a := testAMQP(address, &fakeBroker{dialErr: fmt.Errorf("refused %s for hub-user", tc.shown)})

			err := a.Send(t.Context(), seiMessage("activity"))

			want := "could not connect to the broker: refused " + tc.want + " for hub-user"
			if err == nil || err.Error() != want {
				t.Errorf("error = %v, want %s", err, want)
			}
		})
	}
}

func TestAMQPErrorsRedactAShortPasswordOnlyWhereAURLShowsIt(t *testing.T) {
	password := "m/n:o p"
	written := "m%2fn%3ao%20p"
	echoed := func(user, spelling string) string {
		return "amqp://" + user + ":" + spelling + "@broker.example.org/vhost"
	}
	cases := map[string]struct{ shown, want string }{
		"the address as written":                         {echoed("hub-user", written), "[redacted]"},
		"the address in upper case hex":                  {echoed("hub-user", "m%2Fn%3Ao%20p"), "amqp://[redacted]@broker.example.org/vhost"},
		"the address with the password decoded":          {echoed("hub-user", password), "amqp://[redacted]@broker.example.org/vhost"},
		"the address with the password query escaped":    {echoed("hub-user", url.QueryEscape(password)), "amqp://[redacted]@broker.example.org/vhost"},
		"the address query escaped in lower hex":         {echoed("hub-user", "m%2fn%3ao+p"), "amqp://[redacted]@broker.example.org/vhost"},
		"the address with the password path escaped":     {echoed("hub-user", url.PathEscape(password)), "amqp://[redacted]@broker.example.org/vhost"},
		"the address path escaped in lower hex":          {echoed("hub-user", "m%2fn:o%20p"), "amqp://[redacted]@broker.example.org/vhost"},
		"the address with the user spelled differently":  {echoed("hub%2Duser", written), "amqp://hub%2Duser:[redacted]@broker.example.org/vhost"},
		"the password alone, outside any address":        {password, password},
		"the password alone, as written outside any url": {written, written},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			a := testAMQP(echoed("hub-user", written), &fakeBroker{dialErr: fmt.Errorf("refused %s for hub-user", tc.shown)})

			err := a.Send(t.Context(), seiMessage("activity"))

			want := "could not connect to the broker: refused " + tc.want + " for hub-user"
			if err == nil || err.Error() != want {
				t.Errorf("error = %v, want %s", err, want)
			}
		})
	}
}

func TestAMQPErrorsRedactTheBarePasswordFromEightCharacters(t *testing.T) {
	cases := map[string]struct{ password, word, want string }{
		"1 character":  {"a", "banana", "banana"},
		"2 characters": {"an", "banana", "banana"},
		"7 characters": {"monitor", "monitor-hub", "monitor-hub"},
		"8 characters": {"exchange", "exchanges", "[redacted]s"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			address := "amqp://hub-user:" + tc.password + "@broker/"
			a := testAMQP(address, &fakeBroker{dialErr: fmt.Errorf("refused %s in %s", address, tc.word)})

			err := a.Send(t.Context(), seiMessage("activity"))

			want := "could not connect to the broker: refused [redacted] in " + tc.want
			if err == nil || err.Error() != want {
				t.Errorf("error = %v, want %s", err, want)
			}
		})
	}
}

func TestAMQPErrorsKeepTheBrokerUserWhereItIsPartOfTheExchangeName(t *testing.T) {
	reason := "PRECONDITION_FAILED - inequivalent arg 'type' for exchange 'monitor-hub' in vhost '/': received 'topic' but current is 'fanout'"
	failure := fmt.Errorf("could not declare the exchange monitor-hub: %w", &amqp.Error{Code: amqp.PreconditionFailed, Reason: reason, Server: true})
	cases := map[string]string{
		"a password of its own":            "amqp://monitor:" + amqpPassword + "@broker/",
		"a short password equal to a word": "amqp://monitor:monitor@broker/",
	}
	for name, address := range cases {
		t.Run(name, func(t *testing.T) {
			a := newAMQP(core.AMQP{URL: core.NewSecret(address), Exchange: "monitor-hub"})
			a.dial = (&fakeBroker{prepare: func(l *fakeLink) { l.err = failure }}).dial

			err := a.Send(t.Context(), seiMessage("activity"))

			if err == nil || err.Error() != failure.Error() {
				t.Errorf("error = %v, want %v", err, failure)
			}
		})
	}
}

func assertNoSecret(t *testing.T, err error, address string) {
	t.Helper()
	if err == nil {
		t.Fatal("want an error")
	}
	for _, shown := range []string{err.Error(), fmt.Sprintf("%+v", err), fmt.Sprintf("%#v", err), fmt.Sprintf("%v", errors.Unwrap(err))} {
		for _, secret := range []string{amqpPassword, address} {
			if strings.Contains(shown, secret) {
				t.Errorf("the error shows %q: %s", secret, shown)
			}
		}
	}
}

func refusedAddress(t *testing.T) string {
	t.Helper()
	listener, err := new(net.ListenConfig).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	return address
}

func TestAMQPRealDialErrorsLeaveTheURLAndPasswordOut(t *testing.T) {
	cases := map[string]string{
		"a broker that refuses connections": "amqp://hub-user:" + amqpPassword + "@" + refusedAddress(t) + "/vhost",
		"a port that is not a number":       "amqp://hub-user:" + amqpPassword + "@broker.example.org:port/vhost",
		"a scheme that is not amqp":         "http://hub-user:" + amqpPassword + "@broker.example.org/vhost",
	}
	for name, address := range cases {
		t.Run(name, func(t *testing.T) {
			a := testAMQP(address, nil)
			a.limit = 2 * time.Second

			err := a.Send(t.Context(), seiMessage("activity"))

			if err != nil && !strings.HasPrefix(err.Error(), "could not connect to the broker: ") {
				t.Errorf("error = %v", err)
			}
			assertNoSecret(t, err, address)
		})
	}
}

func amqpGoroutines() int {
	buf := make([]byte, 1<<20)
	return strings.Count(string(buf[:runtime.Stack(buf, true)]), "amqp091-go")
}

func silentBroker(t *testing.T) (string, func() int) {
	t.Helper()
	listener, err := new(net.ListenConfig).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var held sync.WaitGroup
	accepted := 0
	accepting := make(chan struct{})
	go func() {
		defer close(accepting)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			accepted++
			held.Go(func() {
				buf := make([]byte, 64)
				for {
					if _, err := conn.Read(buf); err != nil {
						_ = conn.Close()
						return
					}
				}
			})
		}
	}()
	stop := sync.OnceValue(func() int {
		_ = listener.Close()
		<-accepting
		held.Wait()
		return accepted
	})
	t.Cleanup(func() { stop() })
	return listener.Addr().String(), stop
}

func TestAMQPAbandonsABrokerThatAcceptsTCPAndNeverSpeaksAndLeavesNothingRunning(t *testing.T) {
	address, stop := silentBroker(t)
	before := amqpGoroutines()
	a := testAMQP("amqp://hub-user:"+amqpPassword+"@"+address+"/", nil)
	a.limit = 300 * time.Millisecond
	started := time.Now()

	err := a.Send(context.Background(), seiMessage("activity"))

	if err == nil || time.Since(started) > 3*time.Second {
		t.Fatalf("Send = %v after %s", err, time.Since(started))
	}
	assertNoSecret(t, err, "amqp://hub-user:"+amqpPassword+"@"+address+"/")
	deadline := time.Now().Add(3 * time.Second)
	for amqpGoroutines() > before && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if after := amqpGoroutines(); after > before {
		t.Errorf("%d client goroutine frames remain after the timed-out dial, %d before", after, before)
	}
	if connections := stop(); connections != 1 {
		t.Errorf("the broker accepted %d connections, want 1", connections)
	}
}

func TestAMQPCloseClosesTheConnectionAndRefusesLaterSends(t *testing.T) {
	broker := &fakeBroker{}
	a := testAMQP("amqp://guest:guest@broker/", broker)
	if err := a.Send(t.Context(), seiMessage("activity")); err != nil {
		t.Fatal(err)
	}

	closeErr := a.Close()
	again := a.Close()
	sendErr := a.Send(t.Context(), seiMessage("activity"))

	if closeErr != nil || again != nil || broker.link(0).closes != 1 {
		t.Errorf("Close = %v then %v, closes %d", closeErr, again, broker.link(0).closes)
	}
	if sendErr == nil || broker.dialCount() != 1 {
		t.Errorf("Send after Close = %v, dials %d", sendErr, broker.dialCount())
	}
}

func TestAMQPCloseWithoutAConnectionIsAQuietNoOp(t *testing.T) {
	if err := testAMQP("amqp://guest:guest@broker/", nil).Close(); err != nil {
		t.Errorf("Close = %v", err)
	}
}

func TestAMQPCloseEndsASendThatIsWaitingOnTheBroker(t *testing.T) {
	broker := &fakeBroker{prepare: func(l *fakeLink) { l.hold = true }}
	a := testAMQP("amqp://guest:guest@broker/", broker)
	done := make(chan error, 1)
	go func() { done <- a.Send(context.Background(), seiMessage("activity")) }()
	for broker.dialCount() == 0 {
		time.Sleep(time.Millisecond)
	}

	closeErr := a.Close()

	select {
	case err := <-done:
		if err == nil || closeErr != nil {
			t.Errorf("Send = %v, Close = %v", err, closeErr)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Send is still waiting after Close")
	}
}

type testBroker struct {
	url      string
	exchange string
	channel  *amqp.Channel
}

func newTestBroker(t *testing.T) *testBroker {
	t.Helper()
	address := os.Getenv("TEST_AMQP_URL")
	if address == "" {
		t.Skip("RabbitMQ unavailable: set TEST_AMQP_URL, such as amqp://guest:guest@localhost:5672/")
	}
	conn, err := amqp.Dial(address)
	if err != nil {
		t.Fatalf("connect to the test broker: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	channel, err := conn.Channel()
	if err != nil {
		t.Fatalf("open a channel on the test broker: %v", err)
	}
	broker := &testBroker{url: address, exchange: fmt.Sprintf("monitor-hub-test-%d", time.Now().UnixNano()), channel: channel}
	t.Cleanup(func() {
		if err := channel.ExchangeDelete(broker.exchange, false, false); err != nil {
			t.Errorf("delete the test exchange %s: %v", broker.exchange, err)
		}
	})
	return broker
}

func (b *testBroker) sender(t *testing.T, address string) *amqpChannel {
	t.Helper()
	a := newAMQP(core.AMQP{URL: core.NewSecret(address), Exchange: b.exchange})
	t.Cleanup(func() { _ = a.Close() })
	return a
}

func (b *testBroker) queue(t *testing.T, keys ...string) string {
	t.Helper()
	if err := b.channel.ExchangeDeclare(b.exchange, amqp.ExchangeTopic, true, false, false, false, nil); err != nil {
		t.Fatalf("declare the exchange: %v", err)
	}
	queue, err := b.channel.QueueDeclare("", false, true, true, false, nil)
	if err != nil {
		t.Fatalf("declare a queue: %v", err)
	}
	for _, key := range keys {
		if err := b.channel.QueueBind(queue.Name, key, b.exchange, false, nil); err != nil {
			t.Fatalf("bind %s: %v", key, err)
		}
	}
	return queue.Name
}

func (b *testBroker) drain(t *testing.T, queue string) []amqp.Delivery {
	t.Helper()
	var deliveries []amqp.Delivery
	for {
		delivery, ok, err := b.channel.Get(queue, true)
		if err != nil {
			t.Fatalf("read %s: %v", queue, err)
		}
		if !ok {
			return deliveries
		}
		deliveries = append(deliveries, delivery)
	}
}

func TestAMQPBrokerRoutesEachNotificationUnderItsPluginAndKind(t *testing.T) {
	broker := newTestBroker(t)
	everything := broker.queue(t, "#")
	overdue := broker.queue(t, "sei.return_overdue")
	batches := broker.queue(t, "sei.batch")
	sender := broker.sender(t, broker.url)
	single, batch := seiMessage("return_overdue"), seiMessage("activity", "document")

	errs := []error{sender.Send(t.Context(), single), sender.Send(t.Context(), batch)}

	if errs[0] != nil || errs[1] != nil {
		t.Fatalf("Send = %v", errs)
	}
	all := broker.drain(t, everything)
	if len(all) != 2 {
		t.Fatalf("the queue bound to # got %d messages, want 2", len(all))
	}
	for i, msg := range []Message{single, batch} {
		body, err := json.Marshal(webhookPayload{Events: msg.Events})
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(body)
		if all[i].MessageId != hex.EncodeToString(sum[:])[:32] {
			t.Errorf("message %d: id %q", i, all[i].MessageId)
		}
		if all[i].ContentType != "application/json" || all[i].DeliveryMode != amqp.Persistent || string(all[i].Body) != string(body) {
			t.Errorf("message %d: content type %q, delivery mode %d, body %s", i, all[i].ContentType, all[i].DeliveryMode, all[i].Body)
		}
	}
	if got := broker.drain(t, overdue); len(got) != 1 || got[0].RoutingKey != "sei.return_overdue" {
		t.Errorf("the queue bound to sei.return_overdue got %+v", got)
	}
	if got := broker.drain(t, batches); len(got) != 1 || got[0].Type != "batch" {
		t.Errorf("the queue bound to sei.batch got %+v", got)
	}
}

func TestAMQPBrokerPublishesTheTestMessageUnderTest(t *testing.T) {
	broker := newTestBroker(t)
	queue := broker.queue(t, "test")

	err := broker.sender(t, broker.url).Send(t.Context(), NewTestMessage())

	got := broker.drain(t, queue)
	if err != nil || len(got) != 1 || string(got[0].Body) != `{"test":true,"events":[]}` || got[0].ContentType != "application/json" {
		t.Fatalf("Send = %v, messages %+v", err, got)
	}
}

func TestAMQPBrokerErrorsLeavePasswordOut(t *testing.T) {
	broker := newTestBroker(t)
	wrongPassword, err := url.Parse(broker.url)
	if err != nil {
		t.Fatal(err)
	}
	wrongPassword.User = url.UserPassword(wrongPassword.User.Username(), amqpPassword)
	if err := broker.channel.ExchangeDeclare(broker.exchange, amqp.ExchangeFanout, false, false, false, false, nil); err != nil {
		t.Fatalf("declare a fanout exchange: %v", err)
	}
	tests := map[string]struct {
		url  string
		want string
	}{
		"wrong password":         {wrongPassword.String(), "could not connect to the broker: "},
		"exchange of other type": {broker.url, "could not declare the exchange " + broker.exchange + ": "},
	}
	for name, tt := range tests {
		err := broker.sender(t, tt.url).Send(t.Context(), NewTestMessage())
		if err == nil || !strings.HasPrefix(err.Error(), tt.want) {
			t.Errorf("%s: error = %v, want it to start with %q", name, err, tt.want)
			continue
		}
		for _, secret := range brokerCredentials(t, tt.url) {
			if strings.Contains(err.Error(), secret) {
				t.Errorf("%s: the error shows %q: %v", name, secret, err)
			}
		}
	}
}

func brokerCredentials(t *testing.T, address string) []string {
	t.Helper()
	parsed, err := url.Parse(address)
	if err != nil {
		t.Fatal(err)
	}
	password, _ := parsed.User.Password()
	secrets := []string{address, parsed.User.Username() + ":" + password + "@", ":" + password + "@"}
	if utf8.RuneCountInString(password) >= minBarePassword {
		secrets = append(secrets, password)
	}
	return secrets
}
