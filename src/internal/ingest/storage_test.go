package ingest

import (
	"context"
	"reflect"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/ItzTas/monitor-hub/sdk-go/pluginpb"
	"github.com/ItzTas/monitor-hub/src/internal/core/coretest"
)

func TestEventFieldsAreStored(t *testing.T) {
	pool := coretest.Pool(t)
	payload, err := structpb.NewStruct(map[string]any{"process": "123", "count": 2})
	if err != nil {
		t.Fatal(err)
	}
	full := &pluginpb.Event{
		Seq:        7,
		Id:         "process-123",
		Kind:       "process-moved",
		Severity:   pluginpb.Severity_SEVERITY_URGENT,
		OccurredAt: timestamppb.New(occurredAt),
		Title:      "Process 123 moved",
		Body:       "Now in the archive",
		Url:        "https://sei.example.com/123",
		Payload:    payload,
	}
	newService(pool, plugin("sei", full, event(8, "bare"))).PollOnce(t.Context())

	type stored struct {
		pluginSeq  int64
		kind       string
		severity   int16
		occurredAt time.Time
		title      string
		body       string
		url        string
		payload    *string
	}
	read := func(id string) stored {
		var s stored
		err := pool.QueryRow(t.Context(), `
			SELECT plugin_seq, kind, severity, occurred_at, title, body, url, payload::text
			FROM events WHERE plugin = 'sei' AND event_id = $1`, id).
			Scan(&s.pluginSeq, &s.kind, &s.severity, &s.occurredAt, &s.title, &s.body, &s.url, &s.payload)
		if err != nil {
			t.Fatalf("read event %s: %v", id, err)
		}
		s.occurredAt = s.occurredAt.UTC()
		return s
	}

	wantPayload := `{"count": 2, "process": "123"}`
	if got, want := read("process-123"), (stored{
		pluginSeq: 7, kind: "process-moved", severity: 2, occurredAt: occurredAt,
		title: "Process 123 moved", body: "Now in the archive", url: "https://sei.example.com/123", payload: &wantPayload,
	}); !reflect.DeepEqual(got, want) {
		t.Errorf("event = %+v\nwant    %+v", got, want)
	}
	if got := read("bare"); got.payload != nil {
		t.Errorf("payload of an event without one = %q, want NULL", *got.payload)
	}
}

func TestPluginRowRecordsDescriptionAndStatus(t *testing.T) {
	pool := coretest.Pool(t)
	started := time.Date(2026, 11, 10, 11, 0, 0, 0, time.UTC)
	succeeded := time.Date(2026, 11, 10, 10, 0, 0, 0, time.UTC)
	p := plugin("sei")
	p.status = &pluginpb.StatusResponse{
		State:              pluginpb.State_STATE_FAILING,
		Message:            "login rejected",
		LastCheckStartedAt: timestamppb.New(started),
		LastSuccessAt:      timestamppb.New(succeeded),
	}
	s := newService(pool, p)
	s.PollOnce(t.Context())
	p.version = "1.1.0"
	s.PollOnce(t.Context())

	var (
		displayName, version, message            string
		state                                    int16
		lastCheckStarted, lastSuccess, nextCheck *time.Time
		statusSeen                               time.Time
	)
	err := pool.QueryRow(t.Context(), `
		SELECT display_name, version, state, message, last_check_started_at, last_success_at, next_check_at, status_seen_at
		FROM plugins WHERE name = 'sei'`).
		Scan(&displayName, &version, &state, &message, &lastCheckStarted, &lastSuccess, &nextCheck, &statusSeen)
	if err != nil {
		t.Fatal(err)
	}
	if displayName != "SEI" || version != "1.1.0" {
		t.Errorf("description = %q %q, want SEI 1.1.0", displayName, version)
	}
	if state != int16(pluginpb.State_STATE_FAILING) || message != "login rejected" {
		t.Errorf("status = %d %q, want %d \"login rejected\"", state, message, pluginpb.State_STATE_FAILING)
	}
	if lastCheckStarted == nil || !lastCheckStarted.Equal(started) || lastSuccess == nil || !lastSuccess.Equal(succeeded) || nextCheck != nil {
		t.Errorf("status times = %v %v %v, want %v %v <nil>", lastCheckStarted, lastSuccess, nextCheck, started, succeeded)
	}
	if !statusSeen.Equal(fixedNow) {
		t.Errorf("status seen at %v, want %v", statusSeen, fixedNow)
	}
}

func storeBatch(t *testing.T, repo Repository, batch Batch) error {
	t.Helper()
	if err := repo.SaveDescription(t.Context(), &pluginpb.DescribeResponse{Name: batch.Plugin, DisplayName: batch.Plugin, Version: "1"}); err != nil {
		t.Fatal(err)
	}
	_, err := repo.StoreEvents(t.Context(), batch)
	return err
}

func TestBaselineEventsAreMarked(t *testing.T) {
	pool := coretest.Pool(t)
	repo := NewPostgres(pool)
	if err := storeBatch(t, repo, Batch{Plugin: "sei", Baseline: true, Events: []*pluginpb.Event{event(1, "a"), event(2, "b")}}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.StoreEvents(t.Context(), Batch{Plugin: "sei", After: 2, Events: []*pluginpb.Event{event(3, "c")}}); err != nil {
		t.Fatal(err)
	}
	rows, err := pool.Query(t.Context(), `SELECT baseline FROM events ORDER BY seq`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []bool
	for rows.Next() {
		var b bool
		if err := rows.Scan(&b); err != nil {
			t.Fatal(err)
		}
		got = append(got, b)
	}
	if want := []bool{true, true, false}; !reflect.DeepEqual(got, want) {
		t.Errorf("baseline flags by seq = %v, want %v", got, want)
	}
}

func TestStoreEventsWaitsForTheEventOrderLock(t *testing.T) {
	pool := coretest.Pool(t)
	repo := NewPostgres(pool)
	if err := repo.SaveDescription(t.Context(), &pluginpb.DescribeResponse{Name: "sei", DisplayName: "sei", Version: "1"}); err != nil {
		t.Fatal(err)
	}
	allocated := func() (seq *int64) {
		t.Helper()
		if err := pool.QueryRow(t.Context(), `SELECT pg_sequence_last_value(pg_get_serial_sequence('events', 'seq')::regclass)`).Scan(&seq); err != nil {
			t.Fatal(err)
		}
		return seq
	}
	stored := func() (count int) {
		t.Helper()
		if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM events`).Scan(&count); err != nil {
			t.Fatal(err)
		}
		return count
	}
	tx, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(t.Context())) }()
	if _, err := tx.Exec(t.Context(), `SELECT pg_advisory_xact_lock($1)`, eventOrderLock); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := repo.StoreEvents(t.Context(), Batch{Plugin: "sei", Events: []*pluginpb.Event{event(1, "a"), event(2, "b")}})
		done <- err
	}()

	deadline := time.Now().Add(5 * time.Second)
	for {
		var waiting int
		if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM pg_locks WHERE locktype = 'advisory' AND objid = $1 AND NOT granted`, eventOrderLock).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("StoreEvents never waited for the event order lock")
		}
		time.Sleep(10 * time.Millisecond)
	}
	select {
	case err := <-done:
		t.Fatalf("StoreEvents returned (%v) while the lock was held", err)
	default:
	}
	if seq := allocated(); seq != nil {
		t.Errorf("seq %d was allocated while StoreEvents waited for the lock, want none", *seq)
	}
	if count := stored(); count != 0 {
		t.Errorf("%d events are stored while StoreEvents waits for the lock, want none", count)
	}

	if err := tx.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("StoreEvents = %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("StoreEvents did not finish after the lock was released")
	}
	if count := stored(); count != 2 {
		t.Errorf("stored events = %d, want 2", count)
	}
	if seq := allocated(); seq == nil || *seq != 2 {
		t.Errorf("last allocated seq = %v, want 2", seq)
	}
}
