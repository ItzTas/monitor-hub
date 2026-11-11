package ingest

import (
	"reflect"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/ItzTas/monitor-hub/internal/core/coretest"
	"github.com/ItzTas/monitor-hub/sdk/pluginpb"
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
