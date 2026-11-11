package notify

import (
	"context"
	"errors"
	"log/slog"
	"reflect"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ItzTas/monitor-hub/internal/core/coretest"
)

type fakeChannel struct {
	name string
	mu   sync.Mutex
	got  []Message
	errs []error
	sent chan Message
	send func(ctx context.Context) error
}

func (c *fakeChannel) Name() string {
	return c.name
}

func (c *fakeChannel) Send(ctx context.Context, msg Message) error {
	c.mu.Lock()
	c.got = append(c.got, msg)
	var err error
	if len(c.errs) > 0 {
		err, c.errs = c.errs[0], c.errs[1:]
	}
	c.mu.Unlock()
	if c.sent != nil {
		c.sent <- msg
	}
	if c.send != nil {
		return c.send(ctx)
	}
	return err
}

func (c *fakeChannel) messages() []Message {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.got)
}

func newDispatcher(pool *pgxpool.Pool, now *time.Time, channels ...Channel) *Dispatcher {
	clock := WithClock(func() time.Time { return *now }, time.After)
	return NewDispatcher(NewOutbox(pool), channels, slog.New(slog.DiscardHandler), clock)
}

func seiEvent(title string, at time.Time) Event {
	return Event{Plugin: "sei", DisplayName: "SEI", Kind: "activity", Title: title, OccurredAt: at}
}

func diarioEvent(title string, at time.Time) Event {
	return Event{Plugin: "diario", DisplayName: "ALE/RR Official Gazette", Kind: "match", Title: title, OccurredAt: at}
}

func titles(msg Message) []string {
	var list []string
	for _, e := range msg.Events {
		list = append(list, e.Title)
	}
	return list
}

func TestDispatcherSendsOneMessagePerPlugin(t *testing.T) {
	pool := coretest.Pool(t)
	queue(t, pool, []string{"telegram"}, t0,
		seiEvent("Process 1 moved", t0.Add(-3*time.Hour)),
		diarioEvent("Edition 12 names you", t0.Add(-2*time.Hour)),
		seiEvent("Process 2 moved", t0.Add(-time.Hour)),
	)
	now := t0
	telegram := &fakeChannel{name: "telegram"}
	dispatcher := newDispatcher(pool, &now, telegram)

	dispatcher.RunOnce(t.Context())
	now = t0.Add(time.Hour)
	dispatcher.RunOnce(t.Context())

	got := telegram.messages()
	if len(got) != 2 {
		t.Fatalf("%d messages: %+v", len(got), got)
	}
	if got[0].Subject != "2 updates from SEI" || !slices.Equal(titles(got[0]), []string{"Process 1 moved", "Process 2 moved"}) {
		t.Errorf("first message = %q with %v", got[0].Subject, titles(got[0]))
	}
	if got[1].Subject != "1 update from ALE/RR Official Gazette" || !slices.Equal(titles(got[1]), []string{"Edition 12 names you"}) {
		t.Errorf("second message = %q with %v", got[1].Subject, titles(got[1]))
	}
	for title, d := range deliveriesOf(t, pool, "telegram") {
		if d.status != "sent" || d.attempts != 1 || d.sentAt == nil || !d.sentAt.Equal(t0) || d.lastError != "" {
			t.Errorf("%s delivery = %+v", title, d)
		}
	}
}

func TestFailureStaysWithItsPluginAndChannel(t *testing.T) {
	pool := coretest.Pool(t)
	queue(t, pool, []string{"telegram", "webhook"}, t0,
		seiEvent("Process 1 moved", t0.Add(-2*time.Hour)),
		diarioEvent("Edition 12 names you", t0.Add(-time.Hour)),
	)
	now := t0
	telegram := &fakeChannel{name: "telegram", errs: []error{errors.New("the server answered 500")}}
	webhook := &fakeChannel{name: "webhook"}

	newDispatcher(pool, &now, telegram, webhook).RunOnce(t.Context())

	sentAt := t0
	want := map[string]map[string]stored{
		"telegram": {
			"Process 1 moved":      {title: "Process 1 moved", status: "pending", attempts: 1, nextAttemptAt: t0.Add(time.Minute), lastError: "the server answered 500"},
			"Edition 12 names you": {title: "Edition 12 names you", status: "sent", attempts: 1, nextAttemptAt: t0, sentAt: &sentAt},
		},
		"webhook": {
			"Process 1 moved":      {title: "Process 1 moved", status: "sent", attempts: 1, nextAttemptAt: t0, sentAt: &sentAt},
			"Edition 12 names you": {title: "Edition 12 names you", status: "sent", attempts: 1, nextAttemptAt: t0, sentAt: &sentAt},
		},
	}
	for channel, deliveries := range want {
		if got := deliveriesOf(t, pool, channel); !reflect.DeepEqual(got, deliveries) {
			t.Errorf("%s deliveries = %+v\nwant %+v", channel, got, deliveries)
		}
	}
	if len(telegram.messages()) != 2 || len(webhook.messages()) != 2 {
		t.Errorf("telegram got %d messages, webhook %d", len(telegram.messages()), len(webhook.messages()))
	}
}

func TestFailedDeliveryRetriesOnTheScheduleThenGivesUp(t *testing.T) {
	pool := coretest.Pool(t)
	queue(t, pool, []string{"telegram"}, t0, seiEvent("Process 1 moved", t0))
	failure := errors.New("the server answered 500")
	telegram := &fakeChannel{name: "telegram", errs: slices.Repeat([]error{failure}, 10)}
	now := t0
	dispatcher := newDispatcher(pool, &now, telegram)

	attemptAt := t0
	for i, delay := range []time.Duration{0, time.Minute, 5 * time.Minute, 30 * time.Minute, 2 * time.Hour, 6 * time.Hour} {
		attemptAt = attemptAt.Add(delay)
		now = attemptAt.Add(-time.Second)
		dispatcher.RunOnce(t.Context())
		now = attemptAt
		dispatcher.RunOnce(t.Context())
		if got := len(telegram.messages()); got != i+1 {
			t.Fatalf("attempt %d at %s: %d messages so far", i+1, attemptAt, got)
		}
	}
	now = attemptAt.Add(24 * time.Hour)
	dispatcher.RunOnce(t.Context())

	d := deliveriesOf(t, pool, "telegram")["Process 1 moved"]
	if len(telegram.messages()) != 6 || d.status != "failed" || d.attempts != 6 || d.lastError != "the server answered 500" || d.sentAt != nil {
		t.Errorf("after giving up: %d messages, delivery %+v", len(telegram.messages()), d)
	}
}

func TestDeliveryThatFailedOnceIsSentOnTheRetry(t *testing.T) {
	pool := coretest.Pool(t)
	queue(t, pool, []string{"telegram"}, t0, seiEvent("Process 1 moved", t0))
	telegram := &fakeChannel{name: "telegram", errs: []error{errors.New("could not reach the server: connection refused")}}
	now := t0
	dispatcher := newDispatcher(pool, &now, telegram)

	dispatcher.RunOnce(t.Context())
	now = t0.Add(time.Minute)
	dispatcher.RunOnce(t.Context())

	d := deliveriesOf(t, pool, "telegram")["Process 1 moved"]
	if d.status != "sent" || d.attempts != 2 || d.lastError != "" || d.sentAt == nil || !d.sentAt.Equal(now) {
		t.Errorf("delivery = %+v", d)
	}
}

func TestShutdownDuringASendLeavesTheDeliveriesPending(t *testing.T) {
	pool := coretest.Pool(t)
	queue(t, pool, []string{"telegram"}, t0,
		seiEvent("Process 1 moved", t0.Add(-time.Hour)),
		diarioEvent("Edition 12 names you", t0),
	)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	telegram := &fakeChannel{name: "telegram", send: func(ctx context.Context) error {
		cancel()
		return ctx.Err()
	}}
	now := t0

	newDispatcher(pool, &now, telegram).RunOnce(ctx)

	if got := telegram.messages(); len(got) != 1 {
		t.Errorf("%d messages after the shutdown", len(got))
	}
	for title, d := range deliveriesOf(t, pool, "telegram") {
		if d.status != "pending" || d.attempts != 0 || d.lastError != "" || !d.nextAttemptAt.Equal(t0) {
			t.Errorf("%s delivery after the shutdown = %+v", title, d)
		}
	}
}

func TestRunDeliversEveryPollIntervalUntilTheContextEnds(t *testing.T) {
	pool := coretest.Pool(t)
	queue(t, pool, []string{"telegram"}, t0, seiEvent("Process 1 moved", t0))
	telegram := &fakeChannel{name: "telegram", sent: make(chan Message, 1)}
	ticks := make(chan time.Time)
	var waits []time.Duration
	after := func(d time.Duration) <-chan time.Time {
		waits = append(waits, d)
		return ticks
	}
	dispatcher := NewDispatcher(NewOutbox(pool), []Channel{telegram}, slog.New(slog.DiscardHandler), WithClock(func() time.Time { return t0 }, after))
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error)

	go func() { done <- dispatcher.Run(ctx) }()
	first := <-telegram.sent
	queue(t, pool, []string{"telegram"}, t0, diarioEvent("Edition 12 names you", t0))
	ticks <- t0
	second := <-telegram.sent
	cancel()
	err := <-done

	if err != nil || first.Subject != "1 update from SEI" || second.Subject != "1 update from ALE/RR Official Gazette" {
		t.Errorf("Run = %v, first %q, second %q", err, first.Subject, second.Subject)
	}
	if !slices.Equal(waits, []time.Duration{pollInterval, pollInterval}) {
		t.Errorf("waits = %v", waits)
	}
}

func TestRunWithoutChannelsReturnsAtOnce(t *testing.T) {
	dispatcher := NewDispatcher(nil, nil, slog.New(slog.DiscardHandler))

	if err := dispatcher.Run(t.Context()); err != nil {
		t.Errorf("Run = %v", err)
	}
}
