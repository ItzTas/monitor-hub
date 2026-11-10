package plugintest

import (
	"context"
	"slices"
	"testing"

	"github.com/ItzTas/monitor-hub/sdk/pluginpb"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type fakeClient struct {
	events []*pluginpb.Event
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
	if got := check(c, 3); len(got) != 0 {
		t.Fatalf("violations = %v, want none", got)
	}
}

func TestCheckCatchesBadSeq(t *testing.T) {
	c := &fakeClient{events: []*pluginpb.Event{event(2, "a"), event(1, "b")}}
	if got := check(c, 2); !slices.Contains(got, "seq not strictly increasing") {
		t.Fatalf("violations = %v, want seq not strictly increasing", got)
	}
}

func TestCheckCatchesDuplicateID(t *testing.T) {
	c := &fakeClient{events: []*pluginpb.Event{event(1, "a"), event(2, "a")}}
	if got := check(c, 2); !slices.Contains(got, "duplicate event id") {
		t.Fatalf("violations = %v, want duplicate event id", got)
	}
}
