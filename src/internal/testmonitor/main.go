package main

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/ItzTas/monitor-hub/sdk-go"
	"github.com/ItzTas/monitor-hub/sdk-go/monitorpb"
	"github.com/ItzTas/monitor-hub/sdk-go/settings"
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
	// hubEnviron is the environment the hub passed, before the document adds to it.
	hubEnviron []string
)

type server struct {
	monitorpb.UnimplementedMonitorServer
}

func main() {
	hubEnviron = os.Environ()
	if fd := os.Getenv(settings.ConfigFDEnv); fd != "" {
		exitOnError(readDocument(fd))
	}
	if slices.Equal(os.Args[1:], []string{"validate-settings"}) {
		os.Exit(validateSettings())
	}
	if name := os.Getenv("RECORD_START"); name != "" {
		exitOnError(appendToDataFile("starts", fmt.Sprintf("%d %s\n", os.Getpid(), os.Getenv(name))))
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

func exitOnError(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// readDocument reads the configuration document from the descriptor fd and sets each top-level
// text, number or boolean as the variable named after its key in capitals.
func readDocument(fd string) error {
	if os.Getenv("RECORD_FD") == "1" {
		link, err := os.Readlink("/proc/self/fd/" + fd)
		if err != nil {
			return err
		}
		if err := writeDataFile("fd", []byte(link)); err != nil {
			return err
		}
	}
	n, err := strconv.Atoi(fd)
	if err != nil {
		return fmt.Errorf("%s: %w", settings.ConfigFDEnv, err)
	}
	f := os.NewFile(uintptr(n), "config")
	data, err := io.ReadAll(f)
	_ = f.Close()
	if err != nil {
		return fmt.Errorf("read the configuration document: %w", err)
	}
	if os.Getenv("RECORD_CONFIG") == "1" {
		if err := writeDataFile("config.json", data); err != nil {
			return err
		}
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var document map[string]any
	if err := decoder.Decode(&document); err != nil {
		return fmt.Errorf("decode the configuration document: %w", err)
	}
	for key, value := range document {
		switch value.(type) {
		case string, json.Number, bool:
			_ = os.Setenv(strings.ToUpper(key), fmt.Sprint(value))
		}
	}
	return nil
}

func dataRoot() (*os.Root, error) {
	return os.OpenRoot(os.Getenv(sdk.DataDirEnv))
}

func writeDataFile(name string, data []byte) error {
	root, err := dataRoot()
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	return root.WriteFile(name, data, 0o600)
}

func appendToDataFile(name, line string) error {
	root, err := dataRoot()
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	f, err := root.OpenFile(name, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(line); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
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
		"problems":                 {`{"problems":[{"path":"events","message":"set events"}]}`, 1},
		"log-first":                {"starting\n" + noProblems, 0},
		"exit-2":                   {noProblems, 2},
		"huge":                     {strings.Repeat(" ", 65537) + noProblems, 0},
		"huge-exit-2":              {strings.Repeat(" ", 65537) + noProblems, 2},
		"not-object":               {`[]`, 0},
		"trailing-data":            {noProblems + "\n{}", 0},
		"no-problems":              {`{}`, 0},
		"wrong-type":               {`{"problems":"none"}`, 0},
		"no-message":               {`{"problems":[{"path":"events"}]}`, 1},
		"blank-message":            {`{"problems":[{"path":"events","message":" \t"}]}`, 1},
		"empty-message":            {`{"problems":[{"path":"events","message":""}]}`, 1},
		"mismatch":                 {`{"problems":[{"path":"events","message":"set events"}]}`, 0},
		"unlisted":                 {noProblems, 1},
		"undeclared":               {`{"problems":[{"path":"events","message":"set events"},{"path":"","message":"together"},{"path":"nope[0]","message":"m"}]}`, 1},
		"undeclared-0":             {`{"problems":[{"path":"nope[0]","message":"m"}]}`, 0},
		"undeclared-empty-message": {`{"problems":[{"path":"nope[0]","message":"m"},{"path":"events","message":""}]}`, 1},
		"version-1":                {`{"problems":[{"name":"EVENTS","message":"m"}]}`, 1},
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
	if len(hubEnviron) != 0 {
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
	fmt.Fprintln(os.Stderr, "this binary is a monitor-hub monitor")
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
		Plugins:         plugin.PluginSet{sdk.MonitorKey: &sdk.GRPCMonitor{Impl: &server{}}},
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

func (*server) Describe(context.Context, *monitorpb.DescribeRequest) (*monitorpb.DescribeResponse, error) {
	return &monitorpb.DescribeResponse{
		Name:        "testmonitor",
		DisplayName: "Test monitor",
		Version:     "0.0.0",
		Kinds:       []*monitorpb.EventKind{{Id: "ping", Description: "A test ping"}},
	}, nil
}

func (*server) Status(context.Context, *monitorpb.StatusRequest) (*monitorpb.StatusResponse, error) {
	if stall, err := time.ParseDuration(os.Getenv("STALL_STATUS")); err == nil {
		time.Sleep(stall)
	}
	return &monitorpb.StatusResponse{
		State:   monitorpb.State_STATE_IDLE,
		Message: os.Getenv(os.Getenv("ECHO")),
	}, nil
}

func (*server) Check(context.Context, *monitorpb.CheckRequest) (*monitorpb.CheckResponse, error) {
	if os.Getenv("CRASH_ON_CHECK") == "1" {
		os.Exit(1)
	}
	return &monitorpb.CheckResponse{Accepted: true}, nil
}

func (s *server) Watch(req *monitorpb.WatchRequest, stream grpc.ServerStreamingServer[monitorpb.WatchResponse]) error {
	mode := cmp.Or(os.Getenv("WATCH"), "follow")
	modes := map[string]func(grpc.ServerStreamingServer[monitorpb.WatchResponse]) error{
		"follow": watchFollow,
		"silent": func(stream grpc.ServerStreamingServer[monitorpb.WatchResponse]) error {
			<-stream.Context().Done()
			return nil
		},
		"close": func(stream grpc.ServerStreamingServer[monitorpb.WatchResponse]) error {
			return stream.Send(&monitorpb.WatchResponse{LatestSeq: eventCount()})
		},
		"fail": func(grpc.ServerStreamingServer[monitorpb.WatchResponse]) error {
			return status.Error(codes.Internal, "watch failed")
		},
		"unimplemented": func(stream grpc.ServerStreamingServer[monitorpb.WatchResponse]) error {
			return s.UnimplementedMonitorServer.Watch(req, stream)
		},
	}
	watch, ok := modes[mode]
	if !ok {
		return status.Errorf(codes.InvalidArgument, "unknown WATCH mode %q", mode)
	}
	return watch(stream)
}

func watchFollow(stream grpc.ServerStreamingServer[monitorpb.WatchResponse]) error {
	last := eventCount()
	if err := stream.Send(&monitorpb.WatchResponse{LatestSeq: last}); err != nil {
		return err
	}
	poll := time.NewTicker(10 * time.Millisecond)
	defer poll.Stop()
	beat := time.NewTicker(30 * time.Second)
	defer beat.Stop()
	for {
		select {
		case <-stream.Context().Done():
			return nil
		case <-poll.C:
			current := eventCount()
			if current == last {
				continue
			}
			last = current
			if err := stream.Send(&monitorpb.WatchResponse{LatestSeq: current}); err != nil {
				return err
			}
		case <-beat.C:
			if err := stream.Send(&monitorpb.WatchResponse{LatestSeq: last}); err != nil {
				return err
			}
		}
	}
}

func (*server) Events(_ context.Context, req *monitorpb.EventsRequest) (*monitorpb.EventsResponse, error) {
	if unavailableFor, err := time.ParseDuration(os.Getenv("UNAVAILABLE_FOR")); err == nil && time.Since(started) < unavailableFor {
		return nil, status.Error(codes.Unavailable, "not ready yet")
	}
	limit := pageLimit(req.GetLimit())
	var events []*monitorpb.Event
	for n := max(req.GetAfter(), 0) + 1; n <= eventCount() && int64(len(events)) < limit; n++ {
		events = append(events, &monitorpb.Event{
			Seq:        n,
			Id:         "e" + strconv.FormatInt(n, 10),
			Kind:       "ping",
			Severity:   monitorpb.Severity_SEVERITY_INFO,
			OccurredAt: timestamppb.New(baseTime.Add(time.Duration(n) * time.Minute)),
			Title:      "ping " + strconv.FormatInt(n, 10),
		})
	}
	return &monitorpb.EventsResponse{Events: events}, nil
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
