package plugintest

import (
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"slices"
	"testing"
	"time"

	"github.com/ItzTas/monitor-hub/sdk"
	"github.com/ItzTas/monitor-hub/sdk/pluginpb"
	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/go-plugin"
	"google.golang.org/protobuf/proto"
)

var namePattern = regexp.MustCompile(`^[a-z][a-z0-9-]{1,30}$`)

func Dial(t *testing.T, cmd *exec.Cmd) pluginpb.PluginClient {
	t.Helper()
	client := plugin.NewClient(&plugin.ClientConfig{
		HandshakeConfig:  sdk.Handshake,
		Plugins:          sdk.PluginSet(),
		Cmd:              cmd,
		AllowedProtocols: []plugin.Protocol{plugin.ProtocolGRPC},
		SkipHostEnv:      true,
		Logger:           hclog.NewNullLogger(),
	})
	t.Cleanup(client.Kill)
	rpc, err := client.Client()
	if err != nil {
		t.Fatalf("dial plugin: %v", err)
	}
	raw, err := rpc.Dispense(sdk.PluginKey)
	if err != nil {
		t.Fatalf("dispense plugin: %v", err)
	}
	return raw.(pluginpb.PluginClient)
}

func Run(t *testing.T, cmd *exec.Cmd, minEvents int) {
	t.Helper()
	for _, violation := range check(Dial(t, cmd), minEvents) {
		t.Errorf("conformance: %s", violation)
	}
}

func check(c pluginpb.PluginClient, minEvents int) []string {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var out []string
	kinds, violations := checkDescribe(ctx, c)
	out = append(out, violations...)
	out = append(out, checkStatus(ctx, c)...)
	events, violations := checkEvents(ctx, c, minEvents, kinds)
	out = append(out, violations...)
	out = append(out, checkPaging(ctx, c, events)...)
	return append(out, checkCheck(ctx, c)...)
}

func checkDescribe(ctx context.Context, c pluginpb.PluginClient) ([]string, []string) {
	d, err := c.Describe(ctx, &pluginpb.DescribeRequest{})
	if err != nil {
		return nil, []string{fmt.Sprintf("Describe failed: %v", err)}
	}
	var out, kinds []string
	if !namePattern.MatchString(d.GetName()) {
		out = append(out, fmt.Sprintf("name %q does not match %s", d.GetName(), namePattern))
	}
	if d.GetDisplayName() == "" {
		out = append(out, "display_name is empty")
	}
	for _, k := range d.GetKinds() {
		if k.GetId() == "" {
			out = append(out, "kind id is empty")
		}
		if slices.Contains(kinds, k.GetId()) {
			out = append(out, "duplicate kind id")
		}
		kinds = append(kinds, k.GetId())
	}
	return kinds, out
}

func checkStatus(ctx context.Context, c pluginpb.PluginClient) []string {
	s, err := c.Status(ctx, &pluginpb.StatusRequest{})
	if err != nil {
		return []string{fmt.Sprintf("Status failed: %v", err)}
	}
	if s.GetState() == pluginpb.State_STATE_UNSPECIFIED {
		return []string{"status state is unspecified"}
	}
	return nil
}

func checkEvents(ctx context.Context, c pluginpb.PluginClient, minEvents int, kinds []string) ([]*pluginpb.Event, []string) {
	resp, err := c.Events(ctx, &pluginpb.EventsRequest{After: 0, Limit: 1000})
	if err != nil {
		return nil, []string{fmt.Sprintf("Events failed: %v", err)}
	}
	events := resp.GetEvents()
	var out []string
	if len(events) < minEvents {
		out = append(out, fmt.Sprintf("got %d events, want at least %d", len(events), minEvents))
	}
	seen := map[string]bool{}
	var last int64
	for i, e := range events {
		if e.GetSeq() <= 0 {
			out = append(out, "seq must be positive")
		}
		if i > 0 && e.GetSeq() <= last {
			out = append(out, "seq not strictly increasing")
		}
		last = e.GetSeq()
		if e.GetId() == "" {
			out = append(out, "event id is empty")
		}
		if seen[e.GetId()] {
			out = append(out, "duplicate event id")
		}
		seen[e.GetId()] = true
		out = append(out, checkEventFields(e, kinds)...)
	}
	return events, out
}

func checkEventFields(e *pluginpb.Event, kinds []string) []string {
	var out []string
	if e.GetTitle() == "" {
		out = append(out, "event title is empty")
	}
	if !slices.Contains(kinds, e.GetKind()) {
		out = append(out, fmt.Sprintf("event kind %q not declared by Describe", e.GetKind()))
	}
	if e.GetSeverity() == pluginpb.Severity_SEVERITY_UNSPECIFIED {
		out = append(out, "event severity is unspecified")
	}
	if e.GetOccurredAt() == nil {
		out = append(out, "event occurred_at is missing")
	}
	return out
}

func checkPaging(ctx context.Context, c pluginpb.PluginClient, events []*pluginpb.Event) []string {
	if len(events) == 0 {
		return nil
	}
	var out []string
	tail, err := c.Events(ctx, &pluginpb.EventsRequest{After: events[len(events)-1].GetSeq(), Limit: 1000})
	if err != nil {
		return []string{fmt.Sprintf("Events after last seq failed: %v", err)}
	}
	if len(tail.GetEvents()) != 0 {
		out = append(out, "events after the last seq must be empty")
	}
	rest, err := c.Events(ctx, &pluginpb.EventsRequest{After: events[0].GetSeq(), Limit: 1000})
	if err != nil {
		return append(out, fmt.Sprintf("Events after first seq failed: %v", err))
	}
	if !slices.EqualFunc(rest.GetEvents(), events[1:], func(a, b *pluginpb.Event) bool { return proto.Equal(a, b) }) {
		out = append(out, "events after the first seq must equal the list without the first event")
	}
	return out
}

func checkCheck(ctx context.Context, c pluginpb.PluginClient) []string {
	if _, err := c.Check(ctx, &pluginpb.CheckRequest{}); err != nil {
		return []string{fmt.Sprintf("Check failed: %v", err)}
	}
	return nil
}
