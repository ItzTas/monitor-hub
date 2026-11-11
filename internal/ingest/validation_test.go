package ingest

import (
	"testing"

	"github.com/ItzTas/monitor-hub/internal/core/coretest"
	"github.com/ItzTas/monitor-hub/sdk/pluginpb"
)

func rejectAfterBaseline(t *testing.T, page []*pluginpb.Event) {
	t.Helper()
	pool := coretest.Pool(t)
	p := plugin("sei", numbered(1, 3)...)
	s := newService(pool, p)
	s.PollOnce(t.Context())

	p.respond = respondWith(page...)
	s.PollOnce(t.Context())

	expectStored(t, pool, "sei", 3, 0, 3, true)
}

func TestOutOfOrderSeqIsRejected(t *testing.T) {
	cases := map[string][]*pluginpb.Event{
		"decreasing":           {event(6, "event-6"), event(5, "event-5")},
		"repeated":             {event(4, "event-4"), event(4, "event-4b")},
		"not after the cursor": {event(3, "event-3b"), event(4, "event-4")},
	}
	for name, page := range cases {
		t.Run(name, func(t *testing.T) {
			rejectAfterBaseline(t, page)
		})
	}
}

func TestMalformedPageIsRejected(t *testing.T) {
	withoutTime := event(5, "event-5")
	withoutTime.OccurredAt = nil
	hugeSeverity := event(5, "event-5")
	hugeSeverity.Severity = pluginpb.Severity(70000)
	cases := map[string][]*pluginpb.Event{
		"event without id":          {event(4, "event-4"), event(5, "")},
		"event without occurred_at": {event(4, "event-4"), withoutTime},
		"severity out of range":     {event(4, "event-4"), hugeSeverity},
		"longer than the limit":     numbered(4, 4+EventsPageSize),
	}
	for name, page := range cases {
		t.Run(name, func(t *testing.T) {
			rejectAfterBaseline(t, page)
		})
	}
}
