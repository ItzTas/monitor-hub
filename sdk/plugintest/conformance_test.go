package plugintest

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ItzTas/monitor-hub/sdk/pluginpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type fakeClient struct {
	events    []*pluginpb.Event
	eventsErr error
	failures  int
	calls     int
}

func (f *fakeClient) Describe(context.Context, *pluginpb.DescribeRequest, ...grpc.CallOption) (*pluginpb.DescribeResponse, error) {
	return &pluginpb.DescribeResponse{
		Name:        "fake",
		DisplayName: "Fake",
		Kinds:       []*pluginpb.EventKind{{Id: "ping"}},
	}, nil
}

func (f *fakeClient) Status(context.Context, *pluginpb.StatusRequest, ...grpc.CallOption) (*pluginpb.StatusResponse, error) {
	return &pluginpb.StatusResponse{State: pluginpb.State_STATE_IDLE}, nil
}

func (f *fakeClient) Check(context.Context, *pluginpb.CheckRequest, ...grpc.CallOption) (*pluginpb.CheckResponse, error) {
	return &pluginpb.CheckResponse{Accepted: true}, nil
}

func (f *fakeClient) Events(_ context.Context, req *pluginpb.EventsRequest, _ ...grpc.CallOption) (*pluginpb.EventsResponse, error) {
	f.calls++
	if f.eventsErr != nil && (f.failures < 0 || f.calls <= f.failures) {
		return nil, f.eventsErr
	}
	var out []*pluginpb.Event
	for _, e := range f.events {
		if e.Seq > req.After {
			out = append(out, e)
		}
	}
	return &pluginpb.EventsResponse{Events: out}, nil
}

func event(seq int64, id string) *pluginpb.Event {
	return &pluginpb.Event{
		Seq:        seq,
		Id:         id,
		Kind:       "ping",
		Severity:   pluginpb.Severity_SEVERITY_INFO,
		OccurredAt: timestamppb.Now(),
		Title:      "ping " + id,
	}
}

func TestCheckAcceptsValidPlugin(t *testing.T) {
	c := &fakeClient{events: []*pluginpb.Event{event(1, "a"), event(2, "b"), event(3, "c")}}
	if got := check(t.Context(), c, Options{MinEvents: 3}); len(got) != 0 {
		t.Fatalf("violations = %v, want none", got)
	}
}

func TestCheckCatchesBadSeq(t *testing.T) {
	c := &fakeClient{events: []*pluginpb.Event{event(2, "a"), event(1, "b")}}
	if got := check(t.Context(), c, Options{MinEvents: 2}); !slices.Contains(got, "seq not strictly increasing") {
		t.Fatalf("violations = %v, want seq not strictly increasing", got)
	}
}

func TestCheckCatchesDuplicateID(t *testing.T) {
	c := &fakeClient{events: []*pluginpb.Event{event(1, "a"), event(2, "a")}}
	if got := check(t.Context(), c, Options{MinEvents: 2}); !slices.Contains(got, "duplicate event id") {
		t.Fatalf("violations = %v, want duplicate event id", got)
	}
}

func TestValidName(t *testing.T) {
	tests := map[string]bool{
		"diario":       true,
		"diario-alerr": true,
		"sei-":         false,
		"ab--c":        false,
		"Diario":       false,
		"x":            false,
	}
	for name, want := range tests {
		if got := validName(name); got != want {
			t.Errorf("validName(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestCheckWaitsWhileUnavailable(t *testing.T) {
	c := &fakeClient{
		events:    []*pluginpb.Event{event(1, "a"), event(2, "b"), event(3, "c")},
		eventsErr: status.Error(codes.Unavailable, "starting"),
		failures:  2,
	}
	got := check(t.Context(), c, Options{MinEvents: 3, ReadyTimeout: 2 * time.Second})
	if len(got) != 0 {
		t.Fatalf("violations = %v, want none", got)
	}
}

func TestCheckReportsUnavailableAfterTimeout(t *testing.T) {
	c := &fakeClient{eventsErr: status.Error(codes.Unavailable, "starting"), failures: -1}
	got := check(t.Context(), c, Options{ReadyTimeout: 300 * time.Millisecond})
	if !slices.Contains(got, "events unavailable after 300ms") {
		t.Fatalf("violations = %v, want events unavailable after 300ms", got)
	}
}

func TestCheckDoesNotWaitOnOtherErrors(t *testing.T) {
	c := &fakeClient{eventsErr: status.Error(codes.Internal, "boom"), failures: -1}
	begin := time.Now()
	got := check(t.Context(), c, Options{ReadyTimeout: 2 * time.Second})
	if elapsed := time.Since(begin); elapsed > 500*time.Millisecond {
		t.Fatalf("took %v, want under 500ms", elapsed)
	}
	if !slices.ContainsFunc(got, func(v string) bool { return strings.HasPrefix(v, "Events failed") }) {
		t.Fatalf("violations = %v, want an Events failed violation", got)
	}
}

func TestCheckDoesNotWaitWithoutReadyTimeout(t *testing.T) {
	c := &fakeClient{eventsErr: status.Error(codes.Unavailable, "starting"), failures: -1}
	got := check(t.Context(), c, Options{})
	if !slices.ContainsFunc(got, func(v string) bool { return strings.HasPrefix(v, "Events failed") }) || c.calls != 1 {
		t.Fatalf("violations = %v after %d calls, want one Events failed violation after one call", got, c.calls)
	}
}
