package ingest

import (
	"context"
	"io"
	"log/slog"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/ItzTas/monitor-hub/sdk-go/pluginpb"
	"github.com/ItzTas/monitor-hub/src/internal/core/coretest"
)

func expectPoll(t *testing.T, r *running, want pollRequest) {
	t.Helper()
	if got := r.nextPoll(t); got.req != want {
		t.Fatalf("poll = %+v, want %+v", got.req, want)
	}
}

func restarting() error {
	return status.Error(codes.Unavailable, "the plugin is restarting")
}

func TestASignalPollsThePluginAtOnce(t *testing.T) {
	pool := coretest.Pool(t)
	p := watched(plugin("sei", numbered(1, 3)...))
	s := serviceOf(pool, newSwitchingClients(p))
	var wakes atomic.Int64
	s.wake = func() { wakes.Add(1) }
	r := start(t, s)
	r.clock.waitArmed(t, PollInterval)

	stream := p.nextStream(t)
	stream.signal(t, 3)
	expectPoll(t, r, pollRequest{full: true})
	expectStored(t, pool, "sei", 3, 0, 3, true)
	r.clock.waitArmed(t, SafetyInterval)

	p.add(event(4, "event-4"))
	stream.signal(t, 4)
	expectPoll(t, r, pollRequest{latestSeq: 4})
	expectStored(t, pool, "sei", 4, 2, 4, true)
	if got := wakes.Load(); got != 1 {
		t.Errorf("wakes = %d, want 1", got)
	}

	statusBefore, eventsBefore := p.statusCalls.Load(), p.eventsCalls.Load()
	stream.signal(t, 4)
	expectPoll(t, r, pollRequest{latestSeq: 4})
	if calls := p.statusCalls.Load(); calls != statusBefore+1 {
		t.Errorf("Status calls after the heartbeat = %d, want %d", calls, statusBefore+1)
	}
	if calls := p.eventsCalls.Load(); calls != eventsBefore {
		t.Errorf("Events calls after the heartbeat = %d, want %d", calls, eventsBefore)
	}
}

func TestHeartbeatsDoNotPostponeTheSafetyPoll(t *testing.T) {
	pool := coretest.Pool(t)
	p := watched(plugin("sei", numbered(1, 3)...))
	r := start(t, serviceOf(pool, newSwitchingClients(p)))
	r.clock.waitArmed(t, PollInterval)

	stream := p.nextStream(t)
	stream.signal(t, 3)
	expectPoll(t, r, pollRequest{full: true})

	statusBefore, eventsBefore := p.statusCalls.Load(), p.eventsCalls.Load()
	for range 3 {
		stream.signal(t, 3)
		expectPoll(t, r, pollRequest{latestSeq: 3})
	}
	if calls := p.statusCalls.Load(); calls != statusBefore+3 {
		t.Errorf("Status calls after the heartbeats = %d, want %d", calls, statusBefore+3)
	}
	if calls := p.eventsCalls.Load(); calls != eventsBefore {
		t.Errorf("Events calls after the heartbeats = %d, want %d", calls, eventsBefore)
	}
	if armed := r.clock.armedCount(SafetyInterval); armed != 1 {
		t.Errorf("after(%s) calls = %d, want 1", SafetyInterval, armed)
	}

	p.add(event(4, "event-4"))
	r.clock.fire(t, SafetyInterval)
	expectPoll(t, r, pollRequest{full: true})
	if calls := p.eventsCalls.Load(); calls != eventsBefore+1 {
		t.Errorf("Events calls after the safety poll = %d, want %d", calls, eventsBefore+1)
	}
	expectStored(t, pool, "sei", 4, 2, 4, true)
}

func TestAPluginWithoutWatchIsAskedOncePerProcess(t *testing.T) {
	pool := coretest.Pool(t)
	p1 := plugin("sei", numbered(1, 3)...)
	clients := newSwitchingClients(p1)
	r := start(t, serviceOf(pool, clients))

	expectPoll(t, r, pollRequest{full: true})
	if calls := p1.watchCalls.Load(); calls != 1 {
		t.Fatalf("Watch calls = %d, want 1", calls)
	}
	r.clock.waitArmed(t, PollInterval)
	for range 2 {
		r.clock.fire(t, PollInterval)
		expectPoll(t, r, pollRequest{full: true})
	}
	if calls := p1.watchCalls.Load(); calls != 1 {
		t.Errorf("Watch calls after two timed polls = %d, want 1", calls)
	}

	statusBefore := p1.statusCalls.Load()
	p2 := plugin("sei", numbered(1, 3)...)
	clients.drop("sei")
	clients.set("sei", p2)
	expectPoll(t, r, pollRequest{full: true})
	if calls := p2.watchCalls.Load(); calls != 1 {
		t.Errorf("Watch calls on the new process = %d, want 1", calls)
	}
	if calls := p2.statusCalls.Load(); calls != 1 {
		t.Errorf("Status calls on the new process = %d, want 1", calls)
	}
	if calls := p1.statusCalls.Load(); calls != statusBefore {
		t.Errorf("Status calls on the old process = %d, want %d", calls, statusBefore)
	}
	if calls := p1.watchCalls.Load(); calls != 1 {
		t.Errorf("Watch calls on the old process = %d, want 1", calls)
	}
	for _, delay := range reopenDelays {
		if armed := r.clock.armedCount(delay); armed != 0 {
			t.Errorf("after(%s) calls = %d, want no back-off for a plugin without Watch", delay, armed)
		}
	}
}

func TestAFailedWatchIsRetriedWithBackOff(t *testing.T) {
	pool := coretest.Pool(t)
	p := watched(plugin("sei", numbered(1, 3)...))
	s := serviceOf(pool, newSwitchingClients(p))
	wall := withSteppingClock(s)
	r := start(t, s)
	r.clock.waitArmed(t, PollInterval)

	p.nextStream(t).failWith(t, restarting())
	expectPoll(t, r, pollRequest{full: true})
	r.clock.waitArmed(t, PollInterval)
	r.clock.fire(t, time.Second)

	second := p.nextStream(t)
	second.signal(t, 3)
	expectPoll(t, r, pollRequest{full: true})
	r.clock.waitArmed(t, SafetyInterval)
	second.signal(t, 3)
	expectPoll(t, r, pollRequest{latestSeq: 3})
	wall.advance(EstablishedAfter)

	second.failWith(t, restarting())
	expectPoll(t, r, pollRequest{full: true})
	r.clock.waitArmed(t, PollInterval)
	r.clock.fire(t, time.Second)

	p.nextStream(t).failWith(t, restarting())
	expectPoll(t, r, pollRequest{full: true})
	r.clock.fire(t, 5*time.Second)

	p.nextStream(t)
	if calls := p.watchCalls.Load(); calls != 4 {
		t.Errorf("Watch calls = %d, want 4", calls)
	}
}

func TestAStreamThatSignalsOnceAndEndsWalksTheBackOff(t *testing.T) {
	pool := coretest.Pool(t)
	p := watched(plugin("sei", numbered(1, 3)...))
	s := serviceOf(pool, newSwitchingClients(p))
	wall := withSteppingClock(s)
	r := start(t, s)

	for _, delay := range []time.Duration{time.Second, 5 * time.Second, 30 * time.Second, 2 * time.Minute, 2 * time.Minute} {
		stream := p.nextStream(t)
		stream.signal(t, 3)
		expectPoll(t, r, pollRequest{full: true})
		wall.advance(EstablishedAfter)
		stream.failWith(t, io.EOF)
		expectPoll(t, r, pollRequest{full: true})
		r.clock.fire(t, delay)
	}
	p.nextStream(t)
	if calls := p.watchCalls.Load(); calls != 6 {
		t.Errorf("Watch calls = %d, want 6", calls)
	}
}

func TestAShortStreamThatSignalsTwiceAndEndsWalksTheBackOff(t *testing.T) {
	pool := coretest.Pool(t)
	p := watched(plugin("sei", numbered(1, 3)...))
	s := serviceOf(pool, newSwitchingClients(p))
	wall := withSteppingClock(s)
	r := start(t, s)

	for _, delay := range []time.Duration{time.Second, 5 * time.Second, 30 * time.Second, 2 * time.Minute} {
		stream := p.nextStream(t)
		stream.signal(t, 3)
		expectPoll(t, r, pollRequest{full: true})
		stream.signal(t, 3)
		expectPoll(t, r, pollRequest{latestSeq: 3})
		wall.advance(EstablishedAfter - time.Second)
		stream.failWith(t, io.EOF)
		expectPoll(t, r, pollRequest{full: true})
		r.clock.fire(t, delay)
	}
	p.nextStream(t)
	if calls := p.watchCalls.Load(); calls != 5 {
		t.Errorf("Watch calls = %d, want 5", calls)
	}
}

func TestAProcessChangeDuringTheBackOffStartsItAgain(t *testing.T) {
	pool := coretest.Pool(t)
	p1 := watched(plugin("sei", numbered(1, 3)...))
	clients := newSwitchingClients(p1)
	r := start(t, serviceOf(pool, clients))

	p1.nextStream(t).failWith(t, restarting())
	r.clock.fire(t, time.Second)
	p1.nextStream(t).failWith(t, restarting())
	r.clock.waitArmed(t, 5*time.Second)

	p2 := watched(plugin("sei", numbered(1, 3)...))
	clients.set("sei", p2)
	p2.nextStream(t).failWith(t, restarting())
	r.clock.fire(t, time.Second)
	p2.nextStream(t)
	if calls := p2.watchCalls.Load(); calls != 2 {
		t.Errorf("Watch calls on the new process = %d, want 2", calls)
	}
}

func TestASilentWatchFallsBackToPollingAndReopens(t *testing.T) {
	pool := coretest.Pool(t)
	p := watched(plugin("sei", numbered(1, 3)...))
	r := start(t, serviceOf(pool, newSwitchingClients(p)))
	r.clock.waitArmed(t, PollInterval)

	first := p.nextStream(t)
	first.signal(t, 0)
	expectPoll(t, r, pollRequest{full: true})
	r.clock.waitArmed(t, SafetyInterval)

	r.clock.waitArmed(t, HeartbeatLimit)
	r.clock.fire(t, HeartbeatLimit)
	waitFor(t, first.ended, "the end of the silent stream")
	expectPoll(t, r, pollRequest{full: true})
	r.clock.waitArmed(t, PollInterval)
	r.clock.fire(t, time.Second)

	p.nextStream(t)
	if calls := p.watchCalls.Load(); calls != 2 {
		t.Errorf("Watch calls = %d, want 2", calls)
	}
}

func TestARestartedPluginIsWatchedOnItsNewProcess(t *testing.T) {
	pool := coretest.Pool(t)
	p1 := watched(plugin("sei", numbered(1, 3)...))
	clients := newSwitchingClients(p1)
	r := start(t, serviceOf(pool, clients))

	old := p1.nextStream(t)
	old.signal(t, 3)
	expectPoll(t, r, pollRequest{full: true})

	clients.drop("sei")
	waitFor(t, old.ended, "the end of the stopped process's stream")
	expectPoll(t, r, pollRequest{full: true})
	describes, statuses, events, watches := p1.describeCalls.Load(), p1.statusCalls.Load(), p1.eventsCalls.Load(), p1.watchCalls.Load()

	p2 := watched(plugin("sei", numbered(1, 4)...))
	clients.set("sei", p2)
	p2.nextStream(t).signal(t, 4)
	expectPoll(t, r, pollRequest{full: true})
	if calls := p2.eventsCalls.Load(); calls != 1 {
		t.Errorf("Events calls on the new process = %d, want 1", calls)
	}
	expectStored(t, pool, "sei", 4, 2, 4, true)

	after := []int64{p1.describeCalls.Load(), p1.statusCalls.Load(), p1.eventsCalls.Load(), p1.watchCalls.Load()}
	if want := []int64{describes, statuses, events, watches}; after[0] != want[0] || after[1] != want[1] || after[2] != want[2] || after[3] != want[3] {
		t.Errorf("calls on the old process after the drop (Describe, Status, Events, Watch) = %v, want %v", after, want)
	}
}

func TestABurstOfSignalsNeverOverlapsPolls(t *testing.T) {
	pool := coretest.Pool(t)
	gate := make(chan struct{})
	held := make(chan struct{})
	p := watched(plugin("sei", numbered(1, 3)...))
	p.respond = func(ctx context.Context, req *pluginpb.EventsRequest) (*pluginpb.EventsResponse, error) {
		if req.GetAfter() != 3 {
			return p.page(req), nil
		}
		close(held)
		select {
		case <-gate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return p.page(req), nil
	}
	r := start(t, serviceOf(pool, newSwitchingClients(p)))

	stream := p.nextStream(t)
	stream.signal(t, 3)
	expectPoll(t, r, pollRequest{full: true})

	p.add(event(4, "event-4"))
	stream.signal(t, 4)
	waitFor(t, held, "the Events call after 3")
	for seq := int64(5); seq <= 24; seq++ {
		stream.signal(t, seq)
	}
	close(gate)
	expectPoll(t, r, pollRequest{latestSeq: 4})
	r.clock.fire(t, followUpDelay)
	expectPoll(t, r, pollRequest{latestSeq: 24})
	r.expectNoPoll(t)

	if armed := r.clock.armedCount(followUpDelay); armed != 1 {
		t.Errorf("after(%s) calls = %d, want 1", followUpDelay, armed)
	}
	if most := p.eventsMost.Load(); most != 1 {
		t.Errorf("most Events calls at once = %d, want 1", most)
	}
	expectStored(t, pool, "sei", 4, 2, 4, true)
}

func TestRunEndsEveryStreamBeforeItReturns(t *testing.T) {
	pool := coretest.Pool(t)
	a := watched(plugin("a", numbered(1, 2)...))
	b := plugin("b", numbered(1, 2)...)
	c := watched(plugin("c", numbered(1, 2)...))
	a.release = make(chan struct{})
	release := sync.OnceFunc(func() { close(a.release) })
	r := start(t, serviceOf(pool, newSwitchingClients(a, b, c)))
	t.Cleanup(release)

	open := a.nextStream(t)
	open.signal(t, 2)
	failed := c.nextStream(t)
	failed.failWith(t, restarting())
	for range 3 {
		r.nextPoll(t)
	}
	r.clock.waitArmed(t, time.Second)
	if calls := b.watchCalls.Load(); calls != 1 {
		t.Fatalf("Watch calls of b = %d, want 1", calls)
	}

	r.cancel()
	waitFor(t, open.cancelled, "a's stream seeing the cancel")
	for range 1000 {
		select {
		case <-r.done:
			t.Fatal("Run returned while a's stream was still reading")
		default:
			runtime.Gosched()
		}
	}
	release()
	r.stop(t)
	select {
	case <-open.ended:
	default:
		t.Error("Run returned while a's stream was still reading")
	}
	for name, stream := range map[string]*fakeStream{"a": open, "c": failed} {
		if stream.ctx.Err() == nil {
			t.Errorf("the context of %s's stream is still live after Run returned", name)
		}
	}
}

func TestPollsCutShortByTheEndOfRunLogNoWarning(t *testing.T) {
	pool := coretest.Pool(t)
	entered := make(chan struct{})
	p := plugin("sei", numbered(1, 3)...)
	p.stallStatus = make(chan struct{}, 1)
	p.respond = func(ctx context.Context, _ *pluginpb.EventsRequest) (*pluginpb.EventsResponse, error) {
		close(entered)
		<-ctx.Done()
		return nil, status.FromContextError(ctx.Err()).Err()
	}
	failing := plugin("tjsp", numbered(1, 3)...)
	failing.respond = unavailable
	s := newService(pool, p, failing)
	logs := &recordedLogs{}
	s.log = slog.New(logs)
	r := start(t, s)
	if got := r.nextPoll(t); got.name != "tjsp" {
		t.Fatalf("first poll = %+v, want tjsp", got)
	}
	waitFor(t, entered, "the Events call")
	waitFor(t, p.stallStatus, "the Status call")

	r.stop(t)
	if warnings := logs.atLeast(slog.LevelWarn); len(warnings) != 1 || warnings[0] != "poll a plugin" {
		t.Errorf("warnings = %q, want only the failed poll of tjsp before the end of Run", warnings)
	}
}
