package ingest

import (
	"context"
	"reflect"
	"slices"
	"testing"
	"time"

	"google.golang.org/grpc/status"

	"github.com/ItzTas/monitor-hub/internal/core/coretest"
	"github.com/ItzTas/monitor-hub/sdk/pluginpb"
)

func TestFirstIngestStoresWithoutDeliveries(t *testing.T) {
	pool := coretest.Pool(t)
	newService(pool, plugin("sei", numbered(1, 3)...)).PollOnce(t.Context())

	expectStored(t, pool, "sei", 3, 0, 3, true)
}

func TestNewEventsQueueOneDeliveryPerChannel(t *testing.T) {
	pool := coretest.Pool(t)
	p := plugin("sei", numbered(1, 3)...)
	s := newService(pool, p)
	s.PollOnce(t.Context())

	p.events = append(p.events, event(4, "event-4"))
	s.PollOnce(t.Context())

	type delivery struct {
		pluginSeq     int64
		channel       string
		status        string
		attempts      int32
		nextAttemptAt time.Time
	}
	rows, err := pool.Query(t.Context(), `
		SELECT e.plugin_seq, d.channel, d.status, d.attempts, d.next_attempt_at
		FROM deliveries d JOIN events e ON e.seq = d.event_seq
		ORDER BY d.channel`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []delivery
	for rows.Next() {
		var d delivery
		if err := rows.Scan(&d.pluginSeq, &d.channel, &d.status, &d.attempts, &d.nextAttemptAt); err != nil {
			t.Fatal(err)
		}
		d.nextAttemptAt = d.nextAttemptAt.UTC()
		got = append(got, d)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	want := []delivery{
		{pluginSeq: 4, channel: "ntfy", status: "pending", nextAttemptAt: fixedNow},
		{pluginSeq: 4, channel: "telegram", status: "pending", nextAttemptAt: fixedNow},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("deliveries = %+v\nwant         %+v", got, want)
	}
	expectStored(t, pool, "sei", 4, 2, 4, true)
}

func TestResentEventsAreStoredOnce(t *testing.T) {
	pool := coretest.Pool(t)
	p := plugin("sei", numbered(1, 3)...)
	s := newService(pool, p)
	s.PollOnce(t.Context())

	p.events = []*pluginpb.Event{event(4, "event-3"), event(5, "event-5")}
	s.PollOnce(t.Context())

	expectStored(t, pool, "sei", 4, 2, 5, true)
	notified := countRows(t, pool, `
		SELECT count(*) FROM deliveries d JOIN events e ON e.seq = d.event_seq
		WHERE e.event_id = 'event-5' AND e.plugin_seq = 5`)
	if notified != 2 {
		t.Errorf("deliveries of event-5 = %d, want 2", notified)
	}
}

func TestFailingPluginDoesNotStopOthers(t *testing.T) {
	pool := coretest.Pool(t)
	failing := plugin("a")
	failing.respond = unavailable
	newService(pool, failing, plugin("b", numbered(1, 3)...)).PollOnce(t.Context())

	expectStored(t, pool, "b", 3, 0, 3, true)
	expectStored(t, pool, "a", 0, 0, 0, false)
}

func TestHungPluginDoesNotStallOthers(t *testing.T) {
	pool := coretest.Pool(t)
	hung := plugin("a")
	hung.respond = func(ctx context.Context, _ *pluginpb.EventsRequest) (*pluginpb.EventsResponse, error) {
		<-ctx.Done()
		return nil, status.FromContextError(ctx.Err()).Err()
	}
	s := newService(pool, hung, plugin("b", numbered(1, 3)...))
	s.callTimeout = 100 * time.Millisecond

	done := make(chan struct{})
	go func() {
		s.PollOnce(t.Context())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("PollOnce did not return while a plugin hung")
	}

	expectStored(t, pool, "b", 3, 0, 3, true)
	expectStored(t, pool, "a", 0, 0, 0, false)
}

func TestPagesUntilShortPage(t *testing.T) {
	pool := coretest.Pool(t)
	p := plugin("sei", numbered(1, 1200)...)
	var afters []int64
	var baselinedWhenAsked []bool
	p.respond = func(ctx context.Context, req *pluginpb.EventsRequest) (*pluginpb.EventsResponse, error) {
		var baselined bool
		err := pool.QueryRow(ctx, "SELECT baselined_at IS NOT NULL FROM plugins WHERE name = 'sei'").Scan(&baselined)
		if err != nil {
			return nil, err
		}
		afters = append(afters, req.GetAfter())
		baselinedWhenAsked = append(baselinedWhenAsked, baselined)
		return p.page(req), nil
	}
	newService(pool, p).PollOnce(t.Context())

	if want := []int64{0, 500, 1000}; !slices.Equal(afters, want) {
		t.Errorf("Events was asked after %v, want %v", afters, want)
	}
	if slices.Contains(baselinedWhenAsked, true) {
		t.Errorf("baseline completed before the last page was stored: %v", baselinedWhenAsked)
	}
	expectStored(t, pool, "sei", 1200, 0, 1200, true)
}

func TestBaselineSurvivesFailedFirstRead(t *testing.T) {
	pool := coretest.Pool(t)
	p := plugin("sei")
	p.respond = unavailable
	s := newService(pool, p)
	s.PollOnce(t.Context())
	expectStored(t, pool, "sei", 0, 0, 0, false)

	p.respond = nil
	p.events = numbered(1, 3)
	s.PollOnce(t.Context())
	expectStored(t, pool, "sei", 3, 0, 3, true)

	p.events = append(p.events, event(4, "event-4"))
	s.PollOnce(t.Context())
	expectStored(t, pool, "sei", 4, 2, 4, true)
}
