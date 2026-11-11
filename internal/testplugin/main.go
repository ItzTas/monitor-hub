package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ItzTas/monitor-hub/sdk"
	"github.com/ItzTas/monitor-hub/sdk/pluginpb"
	"github.com/hashicorp/go-plugin"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const defaultLimit = 100

var (
	baseTime = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	started  = time.Now()
	worker   sync.Mutex
)

type server struct {
	pluginpb.UnimplementedPluginServer
}

func main() {
	if os.Getenv("SPAWN_CHILD") == "1" {
		spawnChild()
	}
	if lines := os.Getenv("STDERR_LINES"); lines != "" {
		writeStderr(lines)
	}
	if os.Getenv("EXIT_BEFORE_HANDSHAKE") == "1" {
		os.Exit(1)
	}
	if os.Getenv("STALL_STATUS") != "" {
		serveOneAtATime()
		return
	}
	sdk.Serve(&server{})
}

func serveOneAtATime() {
	plugin.Serve(&plugin.ServeConfig{
		HandshakeConfig: sdk.Handshake,
		Plugins:         plugin.PluginSet{sdk.PluginKey: &sdk.GRPCPlugin{Impl: &server{}}},
		GRPCServer: func(opts []grpc.ServerOption) *grpc.Server {
			return plugin.DefaultGRPCServer(append(opts, grpc.ChainUnaryInterceptor(oneAtATime)))
		},
	})
}

func oneAtATime(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	worker.Lock()
	defer worker.Unlock()
	return handler(ctx, req)
}

func spawnChild() {
	cmd := exec.CommandContext(context.Background(), "sleep", "86400")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	_ = os.Setenv("CHILD_PID", strconv.Itoa(cmd.Process.Pid))
}

func writeStderr(lines string) {
	for line := range strings.SplitSeq(lines, "\x1f") {
		_, _ = fmt.Fprintln(os.Stderr, line)
	}
}

func (*server) Describe(context.Context, *pluginpb.DescribeRequest) (*pluginpb.DescribeResponse, error) {
	return &pluginpb.DescribeResponse{
		Name:        "testplugin",
		DisplayName: "Test plugin",
		Version:     "0.0.0",
		Kinds:       []*pluginpb.EventKind{{Id: "ping", Description: "A test ping"}},
	}, nil
}

func (*server) Status(context.Context, *pluginpb.StatusRequest) (*pluginpb.StatusResponse, error) {
	if stall, err := time.ParseDuration(os.Getenv("STALL_STATUS")); err == nil {
		time.Sleep(stall)
	}
	return &pluginpb.StatusResponse{
		State:   pluginpb.State_STATE_IDLE,
		Message: os.Getenv(os.Getenv("ECHO")),
	}, nil
}

func (*server) Check(context.Context, *pluginpb.CheckRequest) (*pluginpb.CheckResponse, error) {
	if os.Getenv("CRASH_ON_CHECK") == "1" {
		os.Exit(1)
	}
	return &pluginpb.CheckResponse{Accepted: true}, nil
}

func (*server) Events(_ context.Context, req *pluginpb.EventsRequest) (*pluginpb.EventsResponse, error) {
	if unavailableFor, err := time.ParseDuration(os.Getenv("UNAVAILABLE_FOR")); err == nil && time.Since(started) < unavailableFor {
		return nil, status.Error(codes.Unavailable, "not ready yet")
	}
	limit := int(req.GetLimit())
	if limit <= 0 {
		limit = defaultLimit
	}
	var events []*pluginpb.Event
	for n := max(req.GetAfter(), 0) + 1; n <= eventCount() && len(events) < limit; n++ {
		events = append(events, &pluginpb.Event{
			Seq:        n,
			Id:         "e" + strconv.FormatInt(n, 10),
			Kind:       "ping",
			Severity:   pluginpb.Severity_SEVERITY_INFO,
			OccurredAt: timestamppb.New(baseTime.Add(time.Duration(n) * time.Minute)),
			Title:      "ping " + strconv.FormatInt(n, 10),
		})
	}
	return &pluginpb.EventsResponse{Events: events}, nil
}

func eventCount() int64 {
	raw := os.Getenv("EVENTS")
	if dir := os.Getenv(sdk.DataDirEnv); dir != "" {
		if data, err := readEventsFile(dir); err == nil {
			raw = data
		}
	}
	n, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil {
		return 0
	}
	return n
}

func readEventsFile(dir string) (string, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return "", err
	}
	defer func() { _ = root.Close() }()
	data, err := root.ReadFile("events")
	return string(data), err
}
