package plugintest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ItzTas/monitor-hub/sdk"
	"github.com/ItzTas/monitor-hub/sdk/pluginpb"
	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/go-plugin"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

var errNotReady = errors.New("events unavailable")

var namePattern = regexp.MustCompile(`^[a-z][a-z0-9-]{1,30}$`)

type Options struct {
	MinEvents    int
	ReadyTimeout time.Duration
	Stderr       io.Writer
}

const (
	suiteTimeout  = 30 * time.Second
	readyInterval = 200 * time.Millisecond
	defaultLimit  = 100
)

func start(ctx context.Context, cmd *exec.Cmd, stderr io.Writer) (*plugin.Client, pluginpb.PluginClient, error) {
	client := plugin.NewClient(&plugin.ClientConfig{
		HandshakeConfig:  sdk.Handshake,
		Plugins:          sdk.PluginSet(),
		Cmd:              cmd,
		AllowedProtocols: []plugin.Protocol{plugin.ProtocolGRPC},
		SkipHostEnv:      true,
		Logger:           hclog.NewNullLogger(),
		Stderr:           stderr,
	})
	stop := context.AfterFunc(ctx, client.Kill)
	defer stop()
	rpc, err := client.Client()
	if err != nil {
		client.Kill()
		return nil, nil, fmt.Errorf("dial plugin: %w", err)
	}
	raw, err := rpc.Dispense(sdk.PluginKey)
	if err != nil {
		client.Kill()
		return nil, nil, fmt.Errorf("dispense plugin: %w", err)
	}
	c, ok := raw.(pluginpb.PluginClient)
	if !ok {
		client.Kill()
		return nil, nil, fmt.Errorf("dispense plugin: unexpected client type %T", raw)
	}
	return client, c, nil
}

func Dial(t *testing.T, cmd *exec.Cmd) pluginpb.PluginClient {
	t.Helper()
	client, c, err := start(context.Background(), cmd, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Kill)
	return c
}

func Check(ctx context.Context, cmd *exec.Cmd, opts Options) ([]string, error) {
	client, c, err := start(ctx, cmd, opts.Stderr)
	if err != nil {
		return nil, err
	}
	defer client.Kill()
	return check(ctx, c, opts), nil
}

func Run(t *testing.T, cmd *exec.Cmd, minEvents int) {
	t.Helper()
	for _, violation := range check(t.Context(), Dial(t, cmd), Options{MinEvents: minEvents}) {
		t.Errorf("conformance: %s", violation)
	}
}

func check(ctx context.Context, c pluginpb.PluginClient, opts Options) []string {
	ctx, cancel := context.WithTimeout(ctx, suiteTimeout+opts.ReadyTimeout)
	defer cancel()

	var out []string
	kinds, violations := checkDescribe(ctx, c)
	out = append(out, violations...)
	out = append(out, checkStatus(ctx, c)...)
	events, violations := checkEvents(ctx, c, opts, kinds)
	out = append(out, violations...)
	out = append(out, checkPaging(ctx, c, events)...)
	out = append(out, checkLimits(ctx, c, events)...)
	return append(out, checkCheck(ctx, c)...)
}

func validName(name string) bool {
	return namePattern.MatchString(name) && !strings.HasSuffix(name, "-") && !strings.Contains(name, "--")
}

func checkDescribe(ctx context.Context, c pluginpb.PluginClient) ([]string, []string) {
	d, err := c.Describe(ctx, &pluginpb.DescribeRequest{})
	if err != nil {
		return nil, []string{fmt.Sprintf("Describe failed: %v", err)}
	}
	var out, kinds []string
	if !validName(d.GetName()) {
		out = append(out, fmt.Sprintf("name %q is invalid: it must match %s, must not end with \"-\" and must not contain \"--\"", d.GetName(), namePattern))
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

func firstEvents(ctx context.Context, c pluginpb.PluginClient, ready time.Duration) (*pluginpb.EventsResponse, error) {
	req := &pluginpb.EventsRequest{After: 0, Limit: 1000}
	resp, err := c.Events(ctx, req)
	if status.Code(err) != codes.Unavailable || ready <= 0 {
		return resp, err
	}
	waitCtx, cancel := context.WithTimeout(ctx, ready)
	defer cancel()
	ticker := time.NewTicker(readyInterval)
	defer ticker.Stop()
	for {
		select {
		case <-waitCtx.Done():
			return nil, notReady(ctx, ready)
		case <-ticker.C:
		}
		resp, err = c.Events(waitCtx, req)
		if status.Code(err) == codes.Unavailable {
			continue
		}
		if err != nil && waitCtx.Err() != nil {
			return nil, notReady(ctx, ready)
		}
		return resp, err
	}
}

func notReady(parent context.Context, ready time.Duration) error {
	if parent.Err() != nil {
		return parent.Err()
	}
	return fmt.Errorf("%w after %s", errNotReady, ready)
}

func checkEvents(ctx context.Context, c pluginpb.PluginClient, opts Options, kinds []string) ([]*pluginpb.Event, []string) {
	resp, err := firstEvents(ctx, c, opts.ReadyTimeout)
	if errors.Is(err, errNotReady) {
		return nil, []string{err.Error()}
	}
	if err != nil {
		return nil, []string{fmt.Sprintf("Events failed: %v", err)}
	}
	events := resp.GetEvents()
	var out []string
	if len(events) < opts.MinEvents {
		out = append(out, fmt.Sprintf("got %d events, want at least %d", len(events), opts.MinEvents))
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

func checkLimits(ctx context.Context, c pluginpb.PluginClient, events []*pluginpb.Event) []string {
	if len(events) < 2 {
		return nil
	}
	var out []string
	one, err := c.Events(ctx, &pluginpb.EventsRequest{After: 0, Limit: 1})
	if err != nil {
		out = append(out, fmt.Sprintf("Events(after = 0, limit = 1) failed: %v", err))
	}
	if err == nil {
		out = append(out, checkLimitOne(one.GetEvents(), events[0])...)
	}
	page, err := c.Events(ctx, &pluginpb.EventsRequest{After: 0, Limit: 0})
	if err != nil {
		return append(out, fmt.Sprintf("Events(after = 0, limit = 0) failed: %v", err))
	}
	if n := len(page.GetEvents()); n > defaultLimit {
		out = append(out, fmt.Sprintf("Events(after = 0, limit = 0) returned %d events, want at most %d: a limit of 0 or less means %d", n, defaultLimit, defaultLimit))
	}
	return out
}

func checkLimitOne(got []*pluginpb.Event, first *pluginpb.Event) []string {
	const want = "Events(after = 0, limit = 1) must return exactly the first event"
	if len(got) != 1 {
		return []string{fmt.Sprintf("%s, got %s", want, countEvents(len(got)))}
	}
	if proto.Equal(got[0], first) {
		return nil
	}
	if got[0].GetSeq() == first.GetSeq() {
		return []string{fmt.Sprintf("%s (seq %d), got an event with that seq whose fields differ", want, first.GetSeq())}
	}
	return []string{fmt.Sprintf("%s (seq %d), got the event with seq %d", want, first.GetSeq(), got[0].GetSeq())}
}

func countEvents(n int) string {
	if text, ok := map[int]string{0: "no events", 1: "1 event"}[n]; ok {
		return text
	}
	return fmt.Sprintf("%d events", n)
}

func checkCheck(ctx context.Context, c pluginpb.PluginClient) []string {
	if _, err := c.Check(ctx, &pluginpb.CheckRequest{}); err != nil {
		return []string{fmt.Sprintf("Check failed: %v", err)}
	}
	return nil
}
