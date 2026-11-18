package ingest

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/ItzTas/monitor-hub/sdk-go/monitorpb"
)

var (
	fixedNow   = time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	occurredAt = time.Date(2026, 11, 9, 8, 30, 0, 0, time.UTC)
)

type fakePlugin struct {
	monitorpb.MonitorClient
	name          string
	version       string
	mu            sync.Mutex
	status        *monitorpb.StatusResponse
	events        []*monitorpb.Event
	respond       func(ctx context.Context, req *monitorpb.EventsRequest) (*monitorpb.EventsResponse, error)
	describeCalls atomic.Int64
	statusCalls   atomic.Int64
	eventsCalls   atomic.Int64
	eventsRunning atomic.Int64
	eventsMost    atomic.Int64
	watch         func(ctx context.Context) (grpc.ServerStreamingClient[monitorpb.WatchResponse], error)
	streams       chan *fakeStream
	watchCalls    atomic.Int64
	release       chan struct{}
	stallStatus   chan struct{}
}

func plugin(name string, events ...*monitorpb.Event) *fakePlugin {
	return &fakePlugin{
		name:    name,
		version: "1.0.0",
		status:  &monitorpb.StatusResponse{State: monitorpb.State_STATE_IDLE},
		events:  events,
	}
}

func (p *fakePlugin) Describe(context.Context, *monitorpb.DescribeRequest, ...grpc.CallOption) (*monitorpb.DescribeResponse, error) {
	p.describeCalls.Add(1)
	return &monitorpb.DescribeResponse{Name: p.name, DisplayName: strings.ToUpper(p.name), Version: p.version}, nil
}

func (p *fakePlugin) Status(ctx context.Context, _ *monitorpb.StatusRequest, _ ...grpc.CallOption) (*monitorpb.StatusResponse, error) {
	p.statusCalls.Add(1)
	if p.stallStatus != nil {
		p.stallStatus <- struct{}{}
		<-ctx.Done()
		return nil, status.FromContextError(ctx.Err()).Err()
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.status, nil
}

func (p *fakePlugin) Events(ctx context.Context, req *monitorpb.EventsRequest, _ ...grpc.CallOption) (*monitorpb.EventsResponse, error) {
	p.eventsCalls.Add(1)
	p.noteRunningEvents(p.eventsRunning.Add(1))
	defer p.eventsRunning.Add(-1)
	if p.respond != nil {
		return p.respond(ctx, req)
	}
	return p.page(req), nil
}

func (p *fakePlugin) Watch(ctx context.Context, _ *monitorpb.WatchRequest, _ ...grpc.CallOption) (grpc.ServerStreamingClient[monitorpb.WatchResponse], error) {
	p.watchCalls.Add(1)
	if p.watch == nil {
		return nil, status.Error(codes.Unimplemented, "method Watch not implemented")
	}
	return p.watch(ctx)
}

func watched(p *fakePlugin) *fakePlugin {
	p.streams = make(chan *fakeStream, 16)
	p.release = make(chan struct{})
	close(p.release)
	p.watch = func(ctx context.Context) (grpc.ServerStreamingClient[monitorpb.WatchResponse], error) {
		stream := &fakeStream{
			ctx:       ctx,
			signals:   make(chan int64),
			fail:      make(chan error),
			receiving: make(chan struct{}, 64),
			cancelled: make(chan struct{}),
			release:   p.release,
			ended:     make(chan struct{}),
		}
		p.streams <- stream
		return stream, nil
	}
	return p
}

func (p *fakePlugin) nextStream(t *testing.T) *fakeStream {
	t.Helper()
	select {
	case stream := <-p.streams:
		waitFor(t, stream.receiving, "the first Recv of "+p.name+"'s stream")
		return stream
	case <-time.After(5 * time.Second):
		t.Fatalf("%s's Watch was not opened within 5s", p.name)
		return nil
	}
}

type fakeStream struct {
	grpc.ClientStream
	ctx       context.Context
	signals   chan int64
	fail      chan error
	receiving chan struct{}
	cancelled chan struct{}
	release   chan struct{}
	ended     chan struct{}
	endOnce   sync.Once
}

func (f *fakeStream) Context() context.Context {
	return f.ctx
}

func (f *fakeStream) Recv() (*monitorpb.WatchResponse, error) {
	f.receiving <- struct{}{}
	select {
	case seq := <-f.signals:
		return &monitorpb.WatchResponse{LatestSeq: seq}, nil
	case err := <-f.fail:
		return nil, err
	case <-f.ctx.Done():
		f.endOnce.Do(func() {
			close(f.cancelled)
			<-f.release
			close(f.ended)
		})
		return nil, status.FromContextError(f.ctx.Err()).Err()
	}
}

func (f *fakeStream) signal(t *testing.T, seq int64) {
	t.Helper()
	select {
	case f.signals <- seq:
	case <-time.After(5 * time.Second):
		t.Fatalf("signal %d was not received within 5s", seq)
	}
	waitFor(t, f.receiving, fmt.Sprintf("the Recv after signal %d", seq))
}

func (f *fakeStream) failWith(t *testing.T, err error) {
	t.Helper()
	select {
	case f.fail <- err:
	case <-time.After(5 * time.Second):
		t.Fatalf("the stream did not take the error %v within 5s", err)
	}
}

func (p *fakePlugin) noteRunningEvents(running int64) {
	for {
		most := p.eventsMost.Load()
		if running <= most || p.eventsMost.CompareAndSwap(most, running) {
			return
		}
	}
}

func (p *fakePlugin) add(events ...*monitorpb.Event) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.events = append(p.events, events...)
}

func (p *fakePlugin) page(req *monitorpb.EventsRequest) *monitorpb.EventsResponse {
	p.mu.Lock()
	defer p.mu.Unlock()
	var page []*monitorpb.Event
	for _, e := range p.events {
		if e.GetSeq() > req.GetAfter() && len(page) < int(req.GetLimit()) {
			page = append(page, e)
		}
	}
	return &monitorpb.EventsResponse{Events: page}
}

func respondWith(page ...*monitorpb.Event) func(context.Context, *monitorpb.EventsRequest) (*monitorpb.EventsResponse, error) {
	return func(context.Context, *monitorpb.EventsRequest) (*monitorpb.EventsResponse, error) {
		return &monitorpb.EventsResponse{Events: page}, nil
	}
}

func unavailable(context.Context, *monitorpb.EventsRequest) (*monitorpb.EventsResponse, error) {
	return nil, status.Error(codes.Unavailable, "not logged in yet")
}

type fakeClients map[string]*fakePlugin

func (c fakeClients) Names() []string {
	return slices.Sorted(maps.Keys(c))
}

func (c fakeClients) Client(name string) (monitorpb.MonitorClient, bool) {
	p, ok := c[name]
	if !ok {
		return nil, false
	}
	return p, true
}

func (c fakeClients) Track(name string) (monitorpb.MonitorClient, bool, <-chan struct{}) {
	client, ok := c.Client(name)
	return client, ok, nil
}

type switchingClients struct {
	mu      sync.Mutex
	names   []string
	current map[string]*fakePlugin
	changes map[string]chan struct{}
}

func newSwitchingClients(plugins ...*fakePlugin) *switchingClients {
	c := &switchingClients{current: map[string]*fakePlugin{}, changes: map[string]chan struct{}{}}
	for _, p := range plugins {
		c.names = append(c.names, p.name)
		c.current[p.name] = p
	}
	slices.Sort(c.names)
	return c
}

func (c *switchingClients) Names() []string {
	return c.names
}

func (c *switchingClients) Client(name string) (monitorpb.MonitorClient, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	p, ok := c.current[name]
	if !ok {
		return nil, false
	}
	return p, true
}

func (c *switchingClients) Track(name string) (monitorpb.MonitorClient, bool, <-chan struct{}) {
	c.mu.Lock()
	defer c.mu.Unlock()
	changed, known := c.changes[name]
	if !known {
		changed = make(chan struct{})
		c.changes[name] = changed
	}
	p, ok := c.current[name]
	if !ok {
		return nil, false, changed
	}
	return p, true, changed
}

func (c *switchingClients) set(name string, p *fakePlugin) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.current[name] = p
	c.announce(name)
}

func (c *switchingClients) drop(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.current, name)
	c.announce(name)
}

func (c *switchingClients) announce(name string) {
	if changed, ok := c.changes[name]; ok {
		close(changed)
	}
	c.changes[name] = make(chan struct{})
}

func newService(pool *pgxpool.Pool, plugins ...*fakePlugin) *Service {
	clients := fakeClients{}
	for _, p := range plugins {
		clients[p.name] = p
	}
	return serviceOf(pool, clients)
}

func serviceOf(pool *pgxpool.Pool, clients Clients) *Service {
	return NewService(NewPostgres(pool), clients, []string{"telegram", "ntfy"},
		slog.New(slog.DiscardHandler), func() time.Time { return fixedNow })
}

func event(seq int64, id string) *monitorpb.Event {
	return &monitorpb.Event{
		Seq:        seq,
		Id:         id,
		Kind:       "ping",
		Severity:   monitorpb.Severity_SEVERITY_INFO,
		OccurredAt: timestamppb.New(occurredAt),
		Title:      fmt.Sprintf("ping %d", seq),
	}
}

func numbered(from, to int64) []*monitorpb.Event {
	events := make([]*monitorpb.Event, 0, to-from+1)
	for seq := from; seq <= to; seq++ {
		events = append(events, event(seq, fmt.Sprintf("event-%d", seq)))
	}
	return events
}

func countRows(t *testing.T, pool *pgxpool.Pool, query string, args ...any) int {
	t.Helper()
	var count int
	if err := pool.QueryRow(t.Context(), query, args...).Scan(&count); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return count
}

func cursorOf(t *testing.T, pool *pgxpool.Pool, name string) (cursor int64, baselined bool) {
	t.Helper()
	err := pool.QueryRow(t.Context(), "SELECT cursor, baselined_at IS NOT NULL FROM monitors WHERE name = $1", name).
		Scan(&cursor, &baselined)
	if err != nil {
		t.Fatalf("read the cursor of %s: %v", name, err)
	}
	return cursor, baselined
}

func expectStored(t *testing.T, pool *pgxpool.Pool, name string, events, deliveries int, cursor int64, baselined bool) {
	t.Helper()
	if got := countRows(t, pool, "SELECT count(*) FROM events WHERE monitor = $1", name); got != events {
		t.Errorf("%s events = %d, want %d", name, got, events)
	}
	if got := countRows(t, pool, "SELECT count(*) FROM deliveries d JOIN events e ON e.seq = d.event_seq WHERE e.monitor = $1", name); got != deliveries {
		t.Errorf("%s deliveries = %d, want %d", name, got, deliveries)
	}
	gotCursor, gotBaselined := cursorOf(t, pool, name)
	if gotCursor != cursor || gotBaselined != baselined {
		t.Errorf("%s cursor = %d (baselined %v), want %d (baselined %v)", name, gotCursor, gotBaselined, cursor, baselined)
	}
}

type fakeTimer struct {
	d     time.Duration
	c     chan time.Time
	fired bool
	seen  bool
}

type fakeClock struct {
	mu     sync.Mutex
	timers []*fakeTimer
	armed  chan struct{}
}

func newFakeClock() *fakeClock {
	return &fakeClock{armed: make(chan struct{}, 1)}
}

func (c *fakeClock) after(d time.Duration) <-chan time.Time {
	timer := &fakeTimer{d: d, c: make(chan time.Time, 1)}
	c.mu.Lock()
	c.timers = append(c.timers, timer)
	c.mu.Unlock()
	select {
	case c.armed <- struct{}{}:
	default:
	}
	return timer.c
}

func (c *fakeClock) fire(t *testing.T, d time.Duration) {
	t.Helper()
	c.waitArmed(t, d)
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, timer := range c.timers {
		if timer.d != d || timer.fired {
			continue
		}
		timer.fired = true
		timer.c <- fixedNow
	}
}

func (c *fakeClock) waitArmed(t *testing.T, d time.Duration) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for !c.consume(d) {
		select {
		case <-c.armed:
		case <-deadline:
			t.Fatalf("no after(%s) armed within 5s", d)
		}
	}
}

func (c *fakeClock) consume(d time.Duration) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, timer := range c.timers {
		if timer.d == d && !timer.fired && !timer.seen {
			timer.seen = true
			return true
		}
	}
	return false
}

func (c *fakeClock) armedCount(d time.Duration) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	count := 0
	for _, timer := range c.timers {
		if timer.d == d {
			count++
		}
	}
	return count
}

type steppingClock struct {
	mu sync.Mutex
	at time.Time
}

func (c *steppingClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *steppingClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(d)
}

func withSteppingClock(s *Service) *steppingClock {
	clock := &steppingClock{at: fixedNow}
	s.now = clock.now
	return clock
}

type recordedLogs struct {
	mu      sync.Mutex
	records []slog.Record
}

func (l *recordedLogs) Enabled(context.Context, slog.Level) bool {
	return true
}

func (l *recordedLogs) Handle(_ context.Context, r slog.Record) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.records = append(l.records, r)
	return nil
}

func (l *recordedLogs) WithAttrs([]slog.Attr) slog.Handler {
	return l
}

func (l *recordedLogs) WithGroup(string) slog.Handler {
	return l
}

func (l *recordedLogs) atLeast(level slog.Level) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var messages []string
	for _, r := range l.records {
		if r.Level >= level {
			messages = append(messages, r.Message)
		}
	}
	return messages
}
