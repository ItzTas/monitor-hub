package ingest

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ItzTas/monitor-hub/internal/core/coretest"
	"github.com/ItzTas/monitor-hub/sdk/pluginpb"
)

type finishedPoll struct {
	name string
	req  pollRequest
}

type running struct {
	clock  *fakeClock
	polls  chan finishedPoll
	cancel context.CancelFunc
	done   chan struct{}
	err    error
}

func start(t *testing.T, s *Service) *running {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	r := &running{
		clock:  newFakeClock(),
		polls:  make(chan finishedPoll, 64),
		cancel: cancel,
		done:   make(chan struct{}),
	}
	s.after = r.clock.after
	s.onPolled = func(name string, req pollRequest) { r.polls <- finishedPoll{name: name, req: req} }
	go func() {
		r.err = s.Run(ctx)
		close(r.done)
	}()
	t.Cleanup(func() { r.stop(t) })
	return r
}

func (r *running) stop(t *testing.T) {
	t.Helper()
	r.cancel()
	select {
	case <-r.done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run still running 5s after the cancel")
	}
	if r.err != nil {
		t.Errorf("Run = %v, want nil", r.err)
	}
}

func (r *running) nextPoll(t *testing.T) finishedPoll {
	t.Helper()
	select {
	case p := <-r.polls:
		return p
	case <-time.After(5 * time.Second):
		t.Fatal("no poll finished within 5s")
		return finishedPoll{}
	}
}

func (r *running) expectNoPoll(t *testing.T) {
	t.Helper()
	select {
	case p := <-r.polls:
		t.Fatalf("unexpected poll of %s with %+v", p.name, p.req)
	default:
	}
}

func waitFor(t *testing.T, signal <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(5 * time.Second):
		t.Fatalf("%s did not happen within 5s", what)
	}
}

func TestRunPollsEachPluginOnItsOwn(t *testing.T) {
	pool := coretest.Pool(t)
	gate := make(chan struct{})
	a := plugin("a", numbered(1, 2)...)
	a.respond = func(ctx context.Context, req *pluginpb.EventsRequest) (*pluginpb.EventsResponse, error) {
		select {
		case <-gate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return a.page(req), nil
	}
	b := plugin("b", numbered(1, 3)...)
	r := start(t, newService(pool, a, b))

	if got := r.nextPoll(t); got.name != "b" || got.req != (pollRequest{full: true}) {
		t.Fatalf("first poll = %+v, want b with a full request", got)
	}
	r.clock.waitArmed(t, PollInterval)
	r.clock.waitArmed(t, PollInterval)
	r.clock.fire(t, PollInterval)
	if got := r.nextPoll(t); got.name != "b" || got.req != (pollRequest{full: true}) {
		t.Fatalf("second poll = %+v, want b with a full request", got)
	}
	if inFlight := a.eventsRunning.Load(); inFlight != 1 {
		t.Fatalf("Events calls of a in flight = %d, want 1", inFlight)
	}
	close(gate)
	if got := r.nextPoll(t); got.name != "a" {
		t.Fatalf("third poll = %+v, want a", got)
	}

	if calls := b.eventsCalls.Load(); calls != 2 {
		t.Errorf("Events calls of b = %d, want 2", calls)
	}
	expectStored(t, pool, "a", 2, 0, 2, true)
	expectStored(t, pool, "b", 3, 0, 3, true)
}

func TestRequestsDuringAPollCollapseIntoOneDelayedFollowUp(t *testing.T) {
	pool := coretest.Pool(t)
	gate := make(chan struct{})
	started := make(chan struct{})
	var once sync.Once
	p := plugin("sei", numbered(1, 3)...)
	p.respond = func(ctx context.Context, req *pluginpb.EventsRequest) (*pluginpb.EventsResponse, error) {
		once.Do(func() {
			close(started)
			select {
			case <-gate:
			case <-ctx.Done():
			}
		})
		return p.page(req), nil
	}
	s := newService(pool, p)
	r := start(t, s)

	waitFor(t, started, "the first Events call")
	for n := int64(1); n <= 10; n++ {
		s.schedules["sei"].request(pollRequest{latestSeq: n})
	}
	close(gate)
	if got := r.nextPoll(t); got.req != (pollRequest{full: true}) {
		t.Fatalf("first poll = %+v, want a full request", got)
	}
	r.expectNoPoll(t)
	if calls := p.describeCalls.Load(); calls != 1 {
		t.Fatalf("Describe calls before the follow-up delay = %d, want 1", calls)
	}
	r.clock.fire(t, followUpDelay)
	if got := r.nextPoll(t); got.req != (pollRequest{latestSeq: 10}) {
		t.Fatalf("follow-up poll = %+v, want latestSeq 10 only", got)
	}
	r.clock.waitArmed(t, PollInterval)
	r.clock.waitArmed(t, PollInterval)
	r.expectNoPoll(t)
	if calls := p.describeCalls.Load(); calls != 2 {
		t.Fatalf("Describe calls after the follow-up = %d, want 2", calls)
	}

	s.schedules["sei"].request(pollRequest{latestSeq: 11})
	if got := r.nextPoll(t); got.req != (pollRequest{latestSeq: 11}) {
		t.Fatalf("poll of an idle loop = %+v, want latestSeq 11", got)
	}
	if most := p.eventsMost.Load(); most != 1 {
		t.Errorf("most Events calls at once = %d, want 1", most)
	}
}

func TestAHintAtOrBelowTheCursorSkipsEvents(t *testing.T) {
	pool := coretest.Pool(t)
	p := plugin("sei", numbered(1, 3)...)
	s := newService(pool, p)
	r := start(t, s)
	r.nextPoll(t)
	r.clock.waitArmed(t, PollInterval)
	r.clock.waitArmed(t, PollInterval)
	expectStored(t, pool, "sei", 3, 0, 3, true)

	statusBefore, eventsBefore := p.statusCalls.Load(), p.eventsCalls.Load()
	s.schedules["sei"].request(pollRequest{latestSeq: 3})
	r.nextPoll(t)
	r.clock.waitArmed(t, PollInterval)
	if calls := p.statusCalls.Load(); calls != statusBefore+1 {
		t.Errorf("Status calls = %d, want %d", calls, statusBefore+1)
	}
	if calls := p.eventsCalls.Load(); calls != eventsBefore {
		t.Errorf("Events calls = %d, want %d", calls, eventsBefore)
	}

	p.add(event(4, "event-4"))
	s.schedules["sei"].request(pollRequest{latestSeq: 4})
	r.nextPoll(t)
	expectStored(t, pool, "sei", 4, 2, 4, true)
}

func TestASignalAheadOfEventsStoresNothingAndALaterOneCatchesUp(t *testing.T) {
	pool := coretest.Pool(t)
	p := plugin("sei", numbered(1, 3)...)
	s := newService(pool, p)
	var wakes atomic.Int64
	s.wake = func() { wakes.Add(1) }
	r := start(t, s)
	r.nextPoll(t)
	r.clock.waitArmed(t, PollInterval)
	r.clock.waitArmed(t, PollInterval)

	eventsBefore := p.eventsCalls.Load()
	s.schedules["sei"].request(pollRequest{latestSeq: 4})
	r.nextPoll(t)
	r.clock.waitArmed(t, PollInterval)
	if calls := p.eventsCalls.Load(); calls != eventsBefore+1 {
		t.Errorf("Events calls = %d, want %d", calls, eventsBefore+1)
	}
	expectStored(t, pool, "sei", 3, 0, 3, true)

	p.add(event(4, "event-4"))
	s.schedules["sei"].request(pollRequest{latestSeq: 4})
	r.nextPoll(t)
	expectStored(t, pool, "sei", 4, 2, 4, true)
	if got := wakes.Load(); got != 1 {
		t.Errorf("wakes = %d, want 1", got)
	}
}

func TestRunReturnsWhenTheContextEnds(t *testing.T) {
	pool := coretest.Pool(t)
	r := start(t, newService(pool, plugin("sei", numbered(1, 3)...)))
	r.nextPoll(t)
	r.clock.waitArmed(t, PollInterval)
	r.clock.waitArmed(t, PollInterval)

	r.stop(t)
}

func TestAWatchedScheduleWaitsTheSafetyInterval(t *testing.T) {
	sch := newSchedule()
	if got := sch.interval(); got != PollInterval {
		t.Errorf("interval of a new schedule = %s, want %s", got, PollInterval)
	}
	sch.setWatching(true)
	if got := sch.interval(); got != SafetyInterval {
		t.Errorf("interval while watching = %s, want %s", got, SafetyInterval)
	}
	sch.setWatching(false)
	if got := sch.interval(); got != PollInterval {
		t.Errorf("interval after watching stops = %s, want %s", got, PollInterval)
	}
}

func TestRequestsMergeIntoOnePendingPoll(t *testing.T) {
	sch := newSchedule()
	sch.request(pollRequest{full: true})
	sch.request(pollRequest{latestSeq: 7})
	sch.request(pollRequest{latestSeq: 3})

	if req, ok := sch.take(); !ok || req != (pollRequest{full: true, latestSeq: 7}) {
		t.Errorf("take = %+v, %v; want {full: true, latestSeq: 7}, true", req, ok)
	}
	if req, ok := sch.take(); ok {
		t.Errorf("second take = %+v, %v; want nothing pending", req, ok)
	}
}

func TestWhenAPollReadsEvents(t *testing.T) {
	cases := []struct {
		name   string
		req    pollRequest
		cursor int64
		known  bool
		want   bool
	}{
		{name: "full", req: pollRequest{full: true}, cursor: 5, known: true, want: true},
		{name: "unknown plugin", req: pollRequest{latestSeq: 0}, cursor: 5, known: false, want: true},
		{name: "hint above the cursor", req: pollRequest{latestSeq: 6}, cursor: 5, known: true, want: true},
		{name: "hint at the cursor", req: pollRequest{latestSeq: 5}, cursor: 5, known: true, want: false},
		{name: "hint below the cursor", req: pollRequest{latestSeq: 4}, cursor: 5, known: true, want: false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.req.readsEvents(c.cursor, c.known); got != c.want {
				t.Errorf("%+v.readsEvents(%d, %v) = %v, want %v", c.req, c.cursor, c.known, got, c.want)
			}
		})
	}
}
