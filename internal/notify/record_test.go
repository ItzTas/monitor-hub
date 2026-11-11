package notify

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ItzTas/monitor-hub/internal/core/coretest"
)

type readOnlyOutbox struct {
	Outbox
	mu       sync.Mutex
	readOnly map[string]bool
}

func (o *readOnlyOutbox) UpdateDeliveries(ctx context.Context, deliveries []Delivery) error {
	o.mu.Lock()
	refused := len(deliveries) > 0 && o.readOnly[deliveries[0].Channel]
	o.mu.Unlock()
	if refused {
		return errors.New("cannot execute UPDATE in a read-only transaction")
	}
	return o.Outbox.UpdateDeliveries(ctx, deliveries)
}

func (o *readOnlyOutbox) set(channel string, readOnly bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.readOnly[channel] = readOnly
}

func newReadOnlyDispatcher(pool *pgxpool.Pool, now *time.Time, readOnly []string, channels ...Channel) (*Dispatcher, *readOnlyOutbox) {
	outbox := &readOnlyOutbox{Outbox: NewOutbox(pool), readOnly: map[string]bool{}}
	for _, channel := range readOnly {
		outbox.set(channel, true)
	}
	clock := WithClock(func() time.Time { return *now }, time.After)
	return NewDispatcher(outbox, channels, slog.New(slog.DiscardHandler), clock), outbox
}

func TestSentGroupIsNotResentWhileItsOutcomeCannotBeRecorded(t *testing.T) {
	pool := coretest.Pool(t)
	queue(t, pool, []string{"telegram"}, t0, seiEvent("Process 1 moved", t0))
	now := t0
	telegram := &fakeChannel{name: "telegram"}
	dispatcher, outbox := newReadOnlyDispatcher(pool, &now, []string{"telegram"}, telegram)

	for _, at := range []time.Duration{0, 30 * time.Second, time.Minute, 10 * time.Minute} {
		now = t0.Add(at)
		dispatcher.RunOnce(t.Context())
	}
	held := deliveriesOf(t, pool, "telegram")["Process 1 moved"]
	outbox.set("telegram", false)
	now = t0.Add(11 * time.Minute)
	dispatcher.RunOnce(t.Context())
	now = t0.Add(12 * time.Minute)
	dispatcher.RunOnce(t.Context())

	if held.status != "pending" || held.attempts != 0 {
		t.Errorf("row while the database refused writes = %+v", held)
	}
	d := deliveriesOf(t, pool, "telegram")["Process 1 moved"]
	if len(telegram.messages()) != 1 || d.status != "sent" || d.attempts != 1 || d.sentAt == nil || !d.sentAt.Equal(t0) {
		t.Errorf("%d sends, delivery %+v", len(telegram.messages()), d)
	}
}

func TestHeldOutcomeIsRecordedAfterTheChannelStopsBeingReady(t *testing.T) {
	pool := coretest.Pool(t)
	queue(t, pool, []string{"whatsapp"}, t0, seiEvent("Process 1 moved", t0))
	now := t0
	whatsapp := &gatedChannel{fakeChannel: fakeChannel{name: "whatsapp"}, ready: true}
	dispatcher, outbox := newReadOnlyDispatcher(pool, &now, []string{"whatsapp"}, whatsapp)

	dispatcher.RunOnce(t.Context())
	whatsapp.ready = false
	outbox.set("whatsapp", false)
	now = t0.Add(30 * time.Second)
	dispatcher.RunOnce(t.Context())

	d := deliveriesOf(t, pool, "whatsapp")["Process 1 moved"]
	if len(whatsapp.messages()) != 1 || d.status != "sent" || d.attempts != 1 || d.sentAt == nil || !d.sentAt.Equal(t0) {
		t.Errorf("%d sends, delivery %+v", len(whatsapp.messages()), d)
	}
}

func TestFailedSendKeepsItsBackoffWhileItsOutcomeCannotBeRecorded(t *testing.T) {
	pool := coretest.Pool(t)
	queue(t, pool, []string{"telegram"}, t0, seiEvent("Process 1 moved", t0))
	now := t0
	telegram := &fakeChannel{name: "telegram", errs: []error{errors.New("the server answered 500")}}
	dispatcher, outbox := newReadOnlyDispatcher(pool, &now, []string{"telegram"}, telegram)

	dispatcher.RunOnce(t.Context())
	now = t0.Add(30 * time.Second)
	dispatcher.RunOnce(t.Context())
	outbox.set("telegram", false)
	now = t0.Add(40 * time.Second)
	dispatcher.RunOnce(t.Context())

	want := stored{title: "Process 1 moved", status: "pending", attempts: 1, nextAttemptAt: t0.Add(time.Minute), lastError: "the server answered 500"}
	if d := deliveriesOf(t, pool, "telegram")["Process 1 moved"]; len(telegram.messages()) != 1 || d != want {
		t.Errorf("%d sends, delivery %+v", len(telegram.messages()), d)
	}

	now = t0.Add(time.Minute)
	dispatcher.RunOnce(t.Context())

	d := deliveriesOf(t, pool, "telegram")["Process 1 moved"]
	if len(telegram.messages()) != 2 || d.status != "sent" || d.attempts != 2 || d.lastError != "" {
		t.Errorf("after the backoff: %d sends, delivery %+v", len(telegram.messages()), d)
	}
}

func TestRecordFailureStopsOnlyItsOwnChannel(t *testing.T) {
	pool := coretest.Pool(t)
	queue(t, pool, []string{"telegram", "webhook"}, t0,
		seiEvent("Process 1 moved", t0.Add(-2*time.Hour)),
		diarioEvent("Edition 12 names you", t0.Add(-time.Hour)),
	)
	now := t0
	telegram, webhook := &fakeChannel{name: "telegram"}, &fakeChannel{name: "webhook"}
	dispatcher, outbox := newReadOnlyDispatcher(pool, &now, []string{"telegram"}, telegram, webhook)

	dispatcher.RunOnce(t.Context())
	now = t0.Add(30 * time.Second)
	dispatcher.RunOnce(t.Context())

	if got := telegram.messages(); len(got) != 1 || titles(got[0])[0] != "Process 1 moved" {
		t.Errorf("telegram while its writes fail: %d sends", len(telegram.messages()))
	}
	for title, d := range deliveriesOf(t, pool, "webhook") {
		if len(webhook.messages()) != 2 || d.status != "sent" {
			t.Errorf("webhook: %d sends, %s delivery %+v", len(webhook.messages()), title, d)
		}
	}

	outbox.set("telegram", false)
	now = t0.Add(time.Minute)
	dispatcher.RunOnce(t.Context())

	for title, d := range deliveriesOf(t, pool, "telegram") {
		if len(telegram.messages()) != 2 || d.status != "sent" || d.attempts != 1 {
			t.Errorf("telegram after recovery: %d sends, %s delivery %+v", len(telegram.messages()), title, d)
		}
	}
}

func TestErrorWithANulByteIsStoredAndBacksOff(t *testing.T) {
	pool := coretest.Pool(t)
	queue(t, pool, []string{"email"}, t0, seiEvent("Process 1 moved", t0))
	now := t0
	email := &fakeChannel{name: "email", errs: []error{errors.New("could not send the email: 554 \x00rejected\xff")}}
	dispatcher := newDispatcher(pool, &now, email)

	dispatcher.RunOnce(t.Context())
	now = t0.Add(30 * time.Second)
	dispatcher.RunOnce(t.Context())

	want := stored{title: "Process 1 moved", status: "pending", attempts: 1, nextAttemptAt: t0.Add(time.Minute), lastError: "could not send the email: 554 rejected"}
	if d := deliveriesOf(t, pool, "email")["Process 1 moved"]; len(email.messages()) != 1 || d != want {
		t.Errorf("%d sends, delivery %+v", len(email.messages()), d)
	}
}
