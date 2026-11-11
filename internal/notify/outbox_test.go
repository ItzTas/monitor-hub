package notify

import (
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ItzTas/monitor-hub/internal/core/coretest"
	"github.com/ItzTas/monitor-hub/internal/db"
	"github.com/ItzTas/monitor-hub/sdk/pluginpb"
)

type stored struct {
	title         string
	status        string
	attempts      int32
	nextAttemptAt time.Time
	sentAt        *time.Time
	lastError     string
}

func queue(t *testing.T, pool *pgxpool.Pool, channels []string, at time.Time, events ...Event) []int64 {
	t.Helper()
	q := db.New(pool)
	seqs := make([]int64, 0, len(events))
	for i, e := range events {
		err := q.SavePluginDescription(t.Context(), db.SavePluginDescriptionParams{Name: e.Plugin, DisplayName: e.DisplayName, Version: "1.0.0"})
		if err != nil {
			t.Fatal(err)
		}
		severity := int16(pluginpb.Severity_SEVERITY_INFO)
		if e.Urgent {
			severity = int16(pluginpb.Severity_SEVERITY_URGENT)
		}
		seq, err := q.InsertEvent(t.Context(), db.InsertEventParams{
			Plugin: e.Plugin, EventID: e.Title, PluginSeq: int64(i + 1), Kind: e.Kind, Severity: severity,
			OccurredAt: e.OccurredAt, Title: e.Title, Body: e.Body, Url: e.URL,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := q.QueueDeliveries(t.Context(), db.QueueDeliveriesParams{EventSeq: seq, Channels: channels, NextAttemptAt: at}); err != nil {
			t.Fatal(err)
		}
		seqs = append(seqs, seq)
	}
	return seqs
}

func deliveriesOf(t *testing.T, pool *pgxpool.Pool, channel string) map[string]stored {
	t.Helper()
	rows, err := pool.Query(t.Context(), `
		SELECT e.title, d.status, d.attempts, d.next_attempt_at, d.sent_at, d.last_error
		FROM deliveries d JOIN events e ON e.seq = d.event_seq
		WHERE d.channel = $1`, channel)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	found := map[string]stored{}
	for rows.Next() {
		var s stored
		if err := rows.Scan(&s.title, &s.status, &s.attempts, &s.nextAttemptAt, &s.sentAt, &s.lastError); err != nil {
			t.Fatal(err)
		}
		s.nextAttemptAt = s.nextAttemptAt.UTC()
		if s.sentAt != nil {
			sentAt := s.sentAt.UTC()
			s.sentAt = &sentAt
		}
		found[s.title] = s
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return found
}

func TestDueDeliveriesReturnsWhatIsDueInOrder(t *testing.T) {
	pool := coretest.Pool(t)
	urgent := Event{Plugin: "sei", DisplayName: "SEI", Kind: "return_overdue", Title: "Return overdue", Body: "Process 1\nUnit X", URL: "https://sei.example.org/1", Urgent: true, OccurredAt: t0.Add(-time.Hour)}
	unnamed := Event{Plugin: "bare", Kind: "ping", Title: "Ping", OccurredAt: t0.Add(-2 * time.Hour)}
	later := Event{Plugin: "sei", DisplayName: "SEI", Kind: "activity", Title: "Not due yet", OccurredAt: t0.Add(-3 * time.Hour)}
	delivered := Event{Plugin: "sei", DisplayName: "SEI", Kind: "activity", Title: "Already sent on telegram", OccurredAt: t0.Add(-4 * time.Hour)}
	due := t0.Add(-time.Minute)
	seqs := queue(t, pool, []string{"telegram", "webhook"}, due, urgent, unnamed, delivered)
	queue(t, pool, []string{"telegram"}, t0.Add(time.Second), later)
	if _, err := pool.Exec(t.Context(), `UPDATE deliveries SET status = 'sent' WHERE event_seq = $1 AND channel = 'telegram'`, seqs[2]); err != nil {
		t.Fatal(err)
	}
	outbox := NewOutbox(pool)
	pending := func(seq int64, channel string, e Event) DueDelivery {
		if e.DisplayName == "" {
			e.DisplayName = e.Plugin
		}
		return DueDelivery{Delivery: Delivery{EventSeq: seq, Channel: channel, Status: deliveryPending, NextAttemptAt: due}, Event: e}
	}

	telegram, err := outbox.DueDeliveries(t.Context(), "telegram", t0, 10)
	if err != nil {
		t.Fatal(err)
	}
	webhook, err := outbox.DueDeliveries(t.Context(), "webhook", t0, 10)
	if err != nil {
		t.Fatal(err)
	}
	first, err := outbox.DueDeliveries(t.Context(), "webhook", t0, 1)
	if err != nil {
		t.Fatal(err)
	}

	if want := []DueDelivery{pending(seqs[1], "telegram", unnamed), pending(seqs[0], "telegram", urgent)}; !reflect.DeepEqual(telegram, want) {
		t.Errorf("telegram due = %+v\nwant           %+v", telegram, want)
	}
	if want := []DueDelivery{pending(seqs[2], "webhook", delivered), pending(seqs[1], "webhook", unnamed), pending(seqs[0], "webhook", urgent)}; !reflect.DeepEqual(webhook, want) {
		t.Errorf("webhook due = %+v\nwant          %+v", webhook, want)
	}
	if len(first) != 1 || first[0].EventSeq != seqs[2] {
		t.Errorf("webhook due with limit 1 = %+v", first)
	}
}

func TestUpdateDeliveriesPersistsTheOutcome(t *testing.T) {
	pool := coretest.Pool(t)
	queue(t, pool, []string{"telegram"}, t0,
		Event{Plugin: "sei", Kind: "activity", Title: "Sent", OccurredAt: t0},
		Event{Plugin: "sei", Kind: "activity", Title: "Retried", OccurredAt: t0},
		Event{Plugin: "sei", Kind: "activity", Title: "Given up", OccurredAt: t0},
	)
	outbox := NewOutbox(pool)
	due, err := outbox.DueDeliveries(t.Context(), "telegram", t0, 10)
	if err != nil || len(due) != 3 {
		t.Fatalf("due = %+v, %v", due, err)
	}
	sentAt := t0.Add(time.Second)
	outcome := map[string]func(Delivery) Delivery{
		"Sent": func(d Delivery) Delivery {
			d.Status, d.Attempts, d.SentAt = deliverySent, 1, &sentAt
			return d
		},
		"Retried": func(d Delivery) Delivery {
			d.Attempts, d.NextAttemptAt, d.LastError = 2, t0.Add(5*time.Minute), "the server answered 502"
			return d
		},
		"Given up": func(d Delivery) Delivery {
			d.Status, d.Attempts, d.LastError = deliveryFailed, 6, "the server answered 500"
			return d
		},
	}
	updates := make([]Delivery, 0, len(due))
	for _, d := range due {
		updates = append(updates, outcome[d.Event.Title](d.Delivery))
	}

	if err := outbox.UpdateDeliveries(t.Context(), updates); err != nil {
		t.Fatal(err)
	}

	want := map[string]stored{
		"Sent":     {title: "Sent", status: "sent", attempts: 1, nextAttemptAt: t0, sentAt: &sentAt},
		"Retried":  {title: "Retried", status: "pending", attempts: 2, nextAttemptAt: t0.Add(5 * time.Minute), lastError: "the server answered 502"},
		"Given up": {title: "Given up", status: "failed", attempts: 6, nextAttemptAt: t0, lastError: "the server answered 500"},
	}
	if got := deliveriesOf(t, pool, "telegram"); !reflect.DeepEqual(got, want) {
		t.Errorf("deliveries = %+v\nwant         %+v", got, want)
	}
	later, err := outbox.DueDeliveries(t.Context(), "telegram", t0.Add(24*time.Hour), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(later) != 1 || later[0].Event.Title != "Retried" {
		t.Errorf("due a day later = %+v", later)
	}
}
