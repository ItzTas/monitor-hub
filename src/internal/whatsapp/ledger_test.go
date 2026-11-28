package whatsapp

import (
	"errors"
	"fmt"
	"reflect"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ItzTas/monitor-hub/src/internal/core/coretest"
	"github.com/ItzTas/monitor-hub/src/internal/notify"
)

func queuedEvents(t *testing.T, pool *pgxpool.Pool, n int) []int64 {
	t.Helper()
	if _, err := pool.Exec(t.Context(), `INSERT INTO monitors (name, display_name, version) VALUES ('sei', 'SEI', '')`); err != nil {
		t.Fatalf("insert the monitor: %v", err)
	}
	seqs := make([]int64, 0, n)
	for i := range n {
		var seq int64
		err := pool.QueryRow(t.Context(), `INSERT INTO events (monitor, event_id, monitor_seq, kind, severity, occurred_at, title, body, url)
			VALUES ('sei', $1, $2, 'document', 1, now(), 't', '', '') RETURNING seq`, fmt.Sprint(i+1), i+1).Scan(&seq)
		if err != nil {
			t.Fatalf("insert event %d: %v", i+1, err)
		}
		if _, err := pool.Exec(t.Context(), `INSERT INTO deliveries (event_seq, channel, next_attempt_at) VALUES ($1, 'whatsapp', now())`, seq); err != nil {
			t.Fatalf("queue event %d: %v", i+1, err)
		}
		seqs = append(seqs, seq)
	}
	return seqs
}

func TestLedgerRemembersWhoGotEachEvent(t *testing.T) {
	pool := coretest.Pool(t)
	seqs := queuedEvents(t, pool, 3)
	ledger := NewLedger(pool)

	errs := errors.Join(ledger.Record(t.Context(), number, seqs[:2]), ledger.Record(t.Context(), number, seqs[:2]))
	reached, err := ledger.Reached(t.Context(), seqs)

	want := map[string]map[int64]bool{number: {seqs[0]: true, seqs[1]: true}}
	if errs != nil || err != nil || !reflect.DeepEqual(reached, want) {
		t.Errorf("Record = %v, Reached = %v; %d recipients reached, as expected: %v", errs, err, len(reached), reflect.DeepEqual(reached, want))
	}
}

func TestANewChannelSkipsRecipientsTheLedgerHas(t *testing.T) {
	pool := coretest.Pool(t)
	seqs := queuedEvents(t, pool, 1)
	event := event1
	event.Seq = seqs[0]
	msg := notify.Compose("SEI", []notify.Event{event})
	before := gateway()
	before.failOnce = map[string]error{otherChat: errors.New("server returned error 500")}
	after := gateway()

	first := NewChannel(before, []string{number, other}, NewLedger(pool)).Send(t.Context(), msg)
	second := NewChannel(after, []string{number, other}, NewLedger(pool)).Send(t.Context(), msg)

	if first == nil || second != nil {
		t.Errorf("Send = %v, then %v; want an error, then nil", first, second)
	}
	if !slices.Equal(chatsOf(before.sent), []string{numberChat}) || !slices.Equal(chatsOf(after.sent), []string{otherChat}) {
		t.Errorf("the first channel reached %d chats and the second %d; as expected: %v, %v", len(before.sent), len(after.sent), slices.Equal(chatsOf(before.sent), []string{numberChat}), slices.Equal(chatsOf(after.sent), []string{otherChat}))
	}
}
