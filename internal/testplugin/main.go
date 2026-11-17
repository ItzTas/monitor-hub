package main

import (
	"context"
	"fmt"
	"math"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
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
	if slices.Equal(os.Args[1:], []string{"validate-settings"}) {
		os.Exit(validateSettings())
	}
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

func validateSettings() int {
	mode := "valid"
	if data, err := os.ReadFile("validator-mode"); err == nil {
		mode = strings.TrimSpace(string(data))
	}
	answers := map[string]struct {
		stdout string
		code   int
	}{
		"problems":      {`{"problems":[{"name":"EVENTS","message":"set EVENTS"}]}`, 1},
		"log-first":     {"starting\n" + noProblems, 0},
		"exit-2":        {noProblems, 2},
		"huge":          {strings.Repeat(" ", 65537) + noProblems, 0},
		"huge-exit-2":   {strings.Repeat(" ", 65537) + noProblems, 2},
		"not-object":    {`[]`, 0},
		"trailing-data": {noProblems + "\n{}", 0},
		"no-problems":   {`{}`, 0},
		"wrong-type":    {`{"problems":"none"}`, 0},
		"no-message":    {`{"problems":[{"name":"EVENTS"}]}`, 1},
		"empty-message": {`{"problems":[{"name":"EVENTS","message":""}]}`, 1},
		"mismatch":      {`{"problems":[{"name":"EVENTS","message":"set EVENTS"}]}`, 0},
		"unlisted":      {noProblems, 1},
	}
	if answer, ok := answers[mode]; ok {
		fmt.Println(answer.stdout)
		return answer.code
	}
	background := exec.CommandContext(context.Background(), "/bin/sleep", "3601")
	detached := exec.CommandContext(context.Background(), "/bin/sleep", "3602")
	detached.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	actions := map[string]func() int{
		"valid":         validAnswer,
		"needs-cookie":  needsCookie,
		"hang":          hang,
		"killed":        killed,
		"log-stderr":    logStderr,
		"background":    func() int { return withChild(background, answerNoProblems) },
		"detached":      func() int { return withChild(detached, answerNoProblems) },
		"detached-hang": func() int { return withChild(detached, hang) },
	}
	action, ok := actions[mode]
	if !ok {
		fmt.Fprintf(os.Stderr, "unknown validator mode %q\n", mode)
		return 4
	}
	return action()
}

const noProblems = `{"problems":[]}`

func answerNoProblems() int {
	fmt.Println(noProblems)
	return 0
}

func validAnswer() int {
	if len(os.Environ()) != 0 {
		return 3
	}
	if _, err := os.Stat("settings.json"); err != nil {
		return 3
	}
	return answerNoProblems()
}

func needsCookie() int {
	if os.Getenv(sdk.Handshake.MagicCookieKey) == sdk.Handshake.MagicCookieValue {
		return answerNoProblems()
	}
	fmt.Fprintln(os.Stderr, "this binary is a monitor-hub plugin")
	return 1
}

func logStderr() int {
	fmt.Fprint(os.Stderr, "validating settings")
	return answerNoProblems()
}

func hang() int {
	time.Sleep(time.Hour)
	return 0
}

func killed() int {
	_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
	return hang()
}

func withChild(cmd *exec.Cmd, then func() int) int {
	cmd.Stdout = os.Stdout
	if err := cmd.Start(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 4
	}
	return then()
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
	limit := pageLimit(req.GetLimit())
	var events []*pluginpb.Event
	for n := max(req.GetAfter(), 0) + 1; n <= eventCount() && int64(len(events)) < limit; n++ {
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

func pageLimit(requested int32) int64 {
	if os.Getenv("IGNORE_LIMIT") == "1" {
		return math.MaxInt64
	}
	if requested > 0 {
		return int64(requested)
	}
	if os.Getenv("NO_DEFAULT_LIMIT") == "1" {
		return math.MaxInt64
	}
	return defaultLimit
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
