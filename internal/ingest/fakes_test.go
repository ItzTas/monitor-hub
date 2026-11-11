package ingest

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/ItzTas/monitor-hub/sdk/pluginpb"
)

var (
	fixedNow   = time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	occurredAt = time.Date(2026, 11, 9, 8, 30, 0, 0, time.UTC)
)

type fakePlugin struct {
	pluginpb.PluginClient
	name    string
	version string
	status  *pluginpb.StatusResponse
	events  []*pluginpb.Event
	respond func(ctx context.Context, req *pluginpb.EventsRequest) (*pluginpb.EventsResponse, error)
}

func plugin(name string, events ...*pluginpb.Event) *fakePlugin {
	return &fakePlugin{
		name:    name,
		version: "1.0.0",
		status:  &pluginpb.StatusResponse{State: pluginpb.State_STATE_IDLE},
		events:  events,
	}
}

func (p *fakePlugin) Describe(context.Context, *pluginpb.DescribeRequest, ...grpc.CallOption) (*pluginpb.DescribeResponse, error) {
	return &pluginpb.DescribeResponse{Name: p.name, DisplayName: strings.ToUpper(p.name), Version: p.version}, nil
}

func (p *fakePlugin) Status(context.Context, *pluginpb.StatusRequest, ...grpc.CallOption) (*pluginpb.StatusResponse, error) {
	return p.status, nil
}

func (p *fakePlugin) Events(ctx context.Context, req *pluginpb.EventsRequest, _ ...grpc.CallOption) (*pluginpb.EventsResponse, error) {
	if p.respond != nil {
		return p.respond(ctx, req)
	}
	return p.page(req), nil
}

func (p *fakePlugin) page(req *pluginpb.EventsRequest) *pluginpb.EventsResponse {
	var page []*pluginpb.Event
	for _, e := range p.events {
		if e.GetSeq() > req.GetAfter() && len(page) < int(req.GetLimit()) {
			page = append(page, e)
		}
	}
	return &pluginpb.EventsResponse{Events: page}
}

func respondWith(page ...*pluginpb.Event) func(context.Context, *pluginpb.EventsRequest) (*pluginpb.EventsResponse, error) {
	return func(context.Context, *pluginpb.EventsRequest) (*pluginpb.EventsResponse, error) {
		return &pluginpb.EventsResponse{Events: page}, nil
	}
}

func unavailable(context.Context, *pluginpb.EventsRequest) (*pluginpb.EventsResponse, error) {
	return nil, status.Error(codes.Unavailable, "not logged in yet")
}

type fakeClients map[string]*fakePlugin

func (c fakeClients) Names() []string {
	return slices.Sorted(maps.Keys(c))
}

func (c fakeClients) Client(name string) (pluginpb.PluginClient, bool) {
	p, ok := c[name]
	if !ok {
		return nil, false
	}
	return p, true
}

func newService(pool *pgxpool.Pool, plugins ...*fakePlugin) *Service {
	clients := fakeClients{}
	for _, p := range plugins {
		clients[p.name] = p
	}
	return NewService(NewPostgres(pool), clients, []string{"telegram", "ntfy"},
		slog.New(slog.DiscardHandler), func() time.Time { return fixedNow })
}

func event(seq int64, id string) *pluginpb.Event {
	return &pluginpb.Event{
		Seq:        seq,
		Id:         id,
		Kind:       "ping",
		Severity:   pluginpb.Severity_SEVERITY_INFO,
		OccurredAt: timestamppb.New(occurredAt),
		Title:      fmt.Sprintf("ping %d", seq),
	}
}

func numbered(from, to int64) []*pluginpb.Event {
	events := make([]*pluginpb.Event, 0, to-from+1)
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
	err := pool.QueryRow(t.Context(), "SELECT cursor, baselined_at IS NOT NULL FROM plugins WHERE name = $1", name).
		Scan(&cursor, &baselined)
	if err != nil {
		t.Fatalf("read the cursor of %s: %v", name, err)
	}
	return cursor, baselined
}

func expectStored(t *testing.T, pool *pgxpool.Pool, name string, events, deliveries int, cursor int64, baselined bool) {
	t.Helper()
	if got := countRows(t, pool, "SELECT count(*) FROM events WHERE plugin = $1", name); got != events {
		t.Errorf("%s events = %d, want %d", name, got, events)
	}
	if got := countRows(t, pool, "SELECT count(*) FROM deliveries d JOIN events e ON e.seq = d.event_seq WHERE e.plugin = $1", name); got != deliveries {
		t.Errorf("%s deliveries = %d, want %d", name, got, deliveries)
	}
	gotCursor, gotBaselined := cursorOf(t, pool, name)
	if gotCursor != cursor || gotBaselined != baselined {
		t.Errorf("%s cursor = %d (baselined %v), want %d (baselined %v)", name, gotCursor, gotBaselined, cursor, baselined)
	}
}
