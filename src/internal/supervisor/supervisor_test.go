package supervisor

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/ItzTas/monitor-hub/sdk-go/monitorpb"
	"github.com/ItzTas/monitor-hub/sdk-go/settings"
	"github.com/ItzTas/monitor-hub/src/internal/catalog"
)

var monitorDir string

func TestMain(m *testing.M) {
	os.Exit(run(m))
}

func run(m *testing.M) int {
	dir, err := os.MkdirTemp("", "supervisor")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer func() { _ = os.RemoveAll(dir) }()
	monitorDir = dir
	out, err := exec.CommandContext(context.Background(), "go", "build", "-o", filepath.Join(dir, "testmonitor"), "../testmonitor").CombinedOutput()
	if err != nil {
		fmt.Fprintf(os.Stderr, "build failed: %v\n%s", err, out)
		return 1
	}
	return m.Run()
}

func entry(name, dir string, command ...string) catalog.Entry {
	return catalog.Entry{
		Manifest: catalog.Manifest{ProtocolVersion: 2, Name: name, Command: command},
		Dir:      dir,
	}
}

func startSupervisor(t *testing.T, environ []string) (*Supervisor, string, func()) {
	t.Helper()
	return startLoggingSupervisor(t, environ, slog.New(slog.DiscardHandler))
}

func startLoggingSupervisor(t *testing.T, environ []string, logger *slog.Logger) (*Supervisor, string, func()) {
	t.Helper()
	s, dataRoot := newTestSupervisor(t, environ, logger)
	return s, dataRoot, launch(t, s)
}

func newTestSupervisor(t *testing.T, environ []string, logger *slog.Logger) (*Supervisor, string) {
	t.Helper()
	dataRoot := t.TempDir()
	s := New([]catalog.Entry{entry("testmonitor", monitorDir, "testmonitor")}, environ, dataRoot, logger)
	s.restartDelays = []time.Duration{10 * time.Millisecond}
	return s, dataRoot
}

// launch runs s until the test ends or the returned function is called.
func launch(t *testing.T, s *Supervisor) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	stop := sync.OnceFunc(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Run returned %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Error("Run did not return within 10s of cancellation")
			return
		}
		if _, ok := s.Client("testmonitor"); ok {
			t.Error("client still available after Run returned")
		}
	})
	t.Cleanup(stop)
	return stop
}

func waitDescribe(t *testing.T, s *Supervisor, timeout time.Duration) monitorpb.MonitorClient {
	t.Helper()
	return waitReplaced(t, s, "testmonitor", nil, timeout)
}

// waitReplaced waits for a client of name other than old that answers Describe.
func waitReplaced(t *testing.T, s *Supervisor, name string, old monitorpb.MonitorClient, timeout time.Duration) monitorpb.MonitorClient {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if c, ok := s.Client(name); ok && c != old && describes(t, c) {
			return c
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("monitor %s did not answer Describe within %s", name, timeout)
	return nil
}

func describes(t *testing.T, c monitorpb.MonitorClient) bool {
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	_, err := c.Describe(ctx, &monitorpb.DescribeRequest{})
	return err == nil
}

func statusMessage(t *testing.T, c monitorpb.MonitorClient) string {
	t.Helper()
	resp, err := c.Status(t.Context(), &monitorpb.StatusRequest{})
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	return resp.GetMessage()
}

func TestMonitorSeesOnlyItsEnvironment(t *testing.T) {
	t.Setenv("TELEGRAM_TOKEN", "secret")

	t.Run("hub secret", func(t *testing.T) {
		s, _, _ := startSupervisor(t, []string{"TELEGRAM_TOKEN=secret", "TESTMONITOR__ECHO=TELEGRAM_TOKEN"})
		if got := statusMessage(t, waitDescribe(t, s, 10*time.Second)); got != "" {
			t.Fatalf("monitor saw TELEGRAM_TOKEN=%q", got)
		}
	})

	t.Run("data dir", func(t *testing.T) {
		s, dataRoot, _ := startSupervisor(t, []string{"TELEGRAM_TOKEN=secret", "TESTMONITOR__ECHO=MONITOR_HUB_DATA_DIR"})
		want := dataRoot + "/monitors/testmonitor"
		if got := statusMessage(t, waitDescribe(t, s, 10*time.Second)); got != want {
			t.Fatalf("MONITOR_HUB_DATA_DIR = %q, want %q", got, want)
		}
		info, err := os.Stat(want)
		if err != nil {
			t.Fatal(err)
		}
		if !info.IsDir() || info.Mode().Perm() != 0o700 {
			t.Fatalf("data dir mode = %v, want directory with 0700", info.Mode())
		}
	})
}

func TestCrashedMonitorIsRestarted(t *testing.T) {
	s, _, _ := startSupervisor(t, []string{"TESTMONITOR__CRASH_ON_CHECK=1"})
	c := waitDescribe(t, s, 10*time.Second)
	if _, err := c.Check(t.Context(), &monitorpb.CheckRequest{}); err == nil {
		t.Fatal("Check succeeded, want the monitor to crash")
	}
	waitDescribe(t, s, 5*time.Second)
}

func trackDescribing(t *testing.T, s *Supervisor) (monitorpb.MonitorClient, <-chan struct{}) {
	t.Helper()
	for {
		c, ok, changed := s.Track("testmonitor")
		if ok {
			if !describes(t, c) {
				t.Fatal("the tracked client does not answer Describe")
			}
			return c, changed
		}
		select {
		case <-changed:
		case <-time.After(10 * time.Second):
			t.Fatal("Track's channel did not close within 10s while no client was running")
		}
	}
}

func closed(changed <-chan struct{}) bool {
	select {
	case <-changed:
		return true
	default:
		return false
	}
}

func TestTrackClosesItsChannelOnEveryProcessChange(t *testing.T) {
	s, _, stop := startSupervisor(t, []string{"TESTMONITOR__CRASH_ON_CHECK=1"})
	c1, changed := trackDescribing(t, s)
	if _, err := c1.Check(t.Context(), &monitorpb.CheckRequest{}); err == nil {
		t.Fatal("Check succeeded, want the monitor to crash")
	}
	select {
	case <-changed:
	case <-time.After(10 * time.Second):
		t.Fatal("Track's channel did not close within 10s of the crash")
	}

	c2, last := trackDescribing(t, s)
	if c2 == c1 {
		t.Fatal("Track returned the client of the crashed process")
	}
	if c, ok, absent := s.Track("absent"); ok || c != nil || closed(absent) {
		t.Errorf("Track(\"absent\") = %v, %v, closed %v; want nil, false and an open channel", c, ok, closed(absent))
	}

	stop()
	if !closed(last) {
		t.Error("Track's channel is still open after Run returned")
	}
}

func childEnviron(extra ...string) []string {
	return append([]string{"PATH=" + os.Getenv("PATH"), "TESTMONITOR__SPAWN_CHILD=1", "TESTMONITOR__ECHO=CHILD_PID"}, extra...)
}

func childPID(t *testing.T, c monitorpb.MonitorClient) int {
	t.Helper()
	pid, err := strconv.Atoi(statusMessage(t, c))
	if err != nil {
		t.Fatalf("child pid: %v", err)
	}
	return pid
}

func running(pid int) bool {
	state, _, ok := readStat(pid)
	return ok && state != 'Z' && state != 'X'
}

func TestCrashWithChildHoldingStdioIsRestarted(t *testing.T) {
	s, _, _ := startSupervisor(t, childEnviron("TESTMONITOR__CRASH_ON_CHECK=1"))
	c := waitDescribe(t, s, 10*time.Second)
	child := childPID(t, c)
	if _, err := c.Check(t.Context(), &monitorpb.CheckRequest{}); err == nil {
		t.Fatal("Check succeeded, want the monitor to crash")
	}
	waitDescribe(t, s, 5*time.Second)
	if running(child) {
		t.Fatalf("child %d of the crashed monitor is still running", child)
	}
}

func TestShutdownKillsMonitorProcessGroup(t *testing.T) {
	s, _, stop := startSupervisor(t, childEnviron())
	child := childPID(t, waitDescribe(t, s, 10*time.Second))
	_, pgid, ok := readStat(child)
	if !ok {
		t.Fatalf("child %d not found", child)
	}
	if pgid == syscall.Getpgrp() {
		t.Errorf("monitor shares the hub's process group %d", pgid)
	}
	stop()
	for _, pid := range []int{pgid, child} {
		if running(pid) {
			t.Errorf("process %d still running after Run returned", pid)
		}
	}
}

func startGroup(t *testing.T) int {
	t.Helper()
	cmd := exec.CommandContext(context.Background(), "sleep", "60")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	return cmd.Process.Pid
}

func TestAwaitGroupExitReportsMembersStillRunningAtTheLimit(t *testing.T) {
	pgid := startGroup(t)

	left, gone := awaitGroupExit(liveMembers, pgid, 50*time.Millisecond)

	if gone || !slices.Equal(left, []int{pgid}) {
		t.Errorf("awaitGroupExit = %v, %t; want [%d], false", left, gone, pgid)
	}
}

func TestAwaitGroupExitReturnsOnceTheMembersAreKilledEvenIfNobodyReapsThem(t *testing.T) {
	pgid := startGroup(t)
	delay := 100 * time.Millisecond
	time.AfterFunc(delay, func() { _ = syscall.Kill(-pgid, syscall.SIGKILL) })
	started := time.Now()

	left, gone := awaitGroupExit(liveMembers, pgid, 10*time.Second)

	waited := time.Since(started)
	if !gone || left != nil || waited < delay || waited > 5*time.Second {
		t.Errorf("awaitGroupExit = %v, %t after %s; want nil, true soon after the kill at %s", left, gone, waited, delay)
	}
	if state, _, ok := readStat(pgid); !ok || state != 'Z' {
		t.Errorf("member %d is in state %q, want an unreaped zombie still in the group", pgid, state)
	}
}

func TestRunReturnsOnlyOnceTheMonitorGroupHasNoLiveMember(t *testing.T) {
	linger := 300 * time.Millisecond
	var (
		mu    sync.Mutex
		first time.Time
	)
	s := New([]catalog.Entry{entry("testmonitor", monitorDir, "testmonitor")}, nil, t.TempDir(), slog.New(slog.DiscardHandler))
	s.groupMembers = func(pgid int) ([]int, error) {
		mu.Lock()
		defer mu.Unlock()
		if first.IsZero() {
			first = time.Now()
		}
		if time.Since(first) < linger {
			return []int{pgid}, nil
		}
		return liveMembers(pgid)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	waitDescribe(t, s, 10*time.Second)

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run returned %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return within 10s of cancellation")
	}
	returned := time.Now()

	mu.Lock()
	defer mu.Unlock()
	if first.IsZero() {
		t.Fatal("Run returned without checking the monitor's process group")
	}
	if waited := returned.Sub(first); waited < linger || waited > linger+time.Second {
		t.Errorf("Run returned %s after the first group check, want right after the member left at %s", waited, linger)
	}
}

func TestSlowMonitorIsNotRestarted(t *testing.T) {
	stall := 6 * time.Second
	s, _, _ := startSupervisor(t, []string{"TESTMONITOR__STALL_STATUS=" + stall.String()})
	c := waitDescribe(t, s, 10*time.Second)
	ctx, cancel := context.WithTimeout(t.Context(), stall+5*time.Second)
	defer cancel()
	if _, err := c.Status(ctx, &monitorpb.StatusRequest{}); err != nil {
		t.Fatalf("stalled Status failed: %v", err)
	}
	if now, ok := s.Client("testmonitor"); !ok || now != c {
		t.Fatal("the stalled monitor was replaced or dropped")
	}
}

func TestUnknownMonitorIsAbsent(t *testing.T) {
	s, _, _ := startSupervisor(t, nil)
	waitDescribe(t, s, 10*time.Second)
	if _, ok := s.Client("nope"); ok {
		t.Fatal("Client(\"nope\") reported a monitor")
	}
}

func TestNames(t *testing.T) {
	s := New([]catalog.Entry{entry("zeta", "", "z"), entry("alpha", "", "a")}, nil, t.TempDir(), slog.New(slog.DiscardHandler))
	if got := s.Names(); !slices.Equal(got, []string{"alpha", "zeta"}) {
		t.Fatalf("Names() = %v", got)
	}
}

type fakeDocuments struct {
	mu       sync.Mutex
	delivery Delivery
	err      error
	calls    int
	// held, when set, is closed by the next Prepare, which then returns only once its start is
	// cancelled, and returns its delivery anyway.
	held chan struct{}
}

func (f *fakeDocuments) Prepare(ctx context.Context, name string) (Delivery, error) {
	f.mu.Lock()
	f.calls++
	delivery, err, held := f.delivery, f.err, f.held
	f.held = nil
	f.mu.Unlock()
	if name != "testmonitor" {
		return Delivery{}, fmt.Errorf("Prepare(%q), want testmonitor", name)
	}
	if held != nil {
		close(held)
		<-ctx.Done()
	}
	return delivery, err
}

func (f *fakeDocuments) setDocument(document string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.delivery.Document = []byte(document)
}

func (f *fakeDocuments) prepared() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// startWithDocuments gives the supervisor no environment of its own: the monitor gets environ
// only through docs.
func startWithDocuments(t *testing.T, environ []string, logger *slog.Logger, docs *fakeDocuments) (*Supervisor, string) {
	t.Helper()
	s, dataDir := newWithDocuments(t, environ, logger, docs)
	launch(t, s)
	return s, dataDir
}

func newWithDocuments(t *testing.T, environ []string, logger *slog.Logger, docs *fakeDocuments) (*Supervisor, string) {
	t.Helper()
	dataRoot := t.TempDir()
	docs.delivery.Env = catalog.Environment("testmonitor", environ, dataRoot, catalog.Layer{})
	s := New([]catalog.Entry{entry("testmonitor", monitorDir, "testmonitor")}, nil, dataRoot, logger, WithDocuments(docs))
	s.restartDelays = []time.Duration{10 * time.Millisecond}
	return s, filepath.Join(dataRoot, "monitors", "testmonitor")
}

func TestEachStartGetsAFreshDocument(t *testing.T) {
	docs := &fakeDocuments{delivery: Delivery{Document: []byte(`{"mark":"1"}`)}}
	environ := []string{"TESTMONITOR__RECORD_FD=1", "TESTMONITOR__RECORD_CONFIG=1", "TESTMONITOR__CRASH_ON_CHECK=1", "TESTMONITOR__ECHO=MARK"}
	s, dataDir := startWithDocuments(t, environ, slog.New(slog.DiscardHandler), docs)
	first := waitDescribe(t, s, 10*time.Second)
	if got := statusMessage(t, first); got != "1" {
		t.Fatalf("the first process read mark %q, want 1", got)
	}

	docs.setDocument(`{"mark":"2"}`)
	if _, err := first.Check(t.Context(), &monitorpb.CheckRequest{}); err == nil {
		t.Fatal("Check succeeded, want the monitor to crash")
	}
	if got := statusMessage(t, waitDescribe(t, s, 10*time.Second)); got != "2" {
		t.Errorf("the restarted process read mark %q, want 2", got)
	}

	if config, err := os.ReadFile(filepath.Join(dataDir, "config.json")); err != nil || string(config) != `{"mark":"2"}` {
		t.Errorf("config.json = %q, %v; want {\"mark\":\"2\"}", config, err)
	}
	if fd, err := os.ReadFile(filepath.Join(dataDir, "fd")); err != nil || !strings.HasPrefix(string(fd), "/memfd:monitor-hub-config") {
		t.Errorf("the descriptor pointed at %q (%v), want /memfd:monitor-hub-config", fd, err)
	}
	if got := docs.prepared(); got != 2 {
		t.Errorf("Prepare ran %d times for two starts", got)
	}
}

func TestWithoutDocumentsTheEnvironmentIsTodays(t *testing.T) {
	s, _, _ := startSupervisor(t, []string{"TESTMONITOR__ECHO=" + settings.ConfigFDEnv})
	if got := statusMessage(t, waitDescribe(t, s, 10*time.Second)); got != "" {
		t.Fatalf("the monitor got %s=%q, want no descriptor", settings.ConfigFDEnv, got)
	}
}

func TestAFailedPrepareMeansNoProcess(t *testing.T) {
	const reason = `n: N is not an integer ("three")`
	logger, buf := capture()
	docs := &fakeDocuments{delivery: Delivery{Document: []byte(`{}`)}, err: errors.New(reason)}
	s, dataDir := startWithDocuments(t, []string{"TESTMONITOR__RECORD_START=MONITOR_HUB_DATA_DIR"}, logger, docs)

	records := waitForRecords(t, buf, 1, lifecycle)
	if len(records) == 0 || records[0]["msg"] != "monitor failed to start" || records[0]["error"] != reason {
		t.Fatalf("lifecycle records = %v, want monitor failed to start with error %q", records, reason)
	}
	if _, ok := s.Client("testmonitor"); ok {
		t.Error("a client is available although Prepare failed")
	}
	if _, err := os.Stat(filepath.Join(dataDir, "starts")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("stat starts: %v, want no process to have started", err)
	}
}

// fakeAfter records every back-off wait and never ends one.
type fakeAfter struct {
	asked chan time.Duration
}

func newFakeAfter() *fakeAfter {
	return &fakeAfter{asked: make(chan time.Duration, 64)}
}

func (f *fakeAfter) after(d time.Duration) <-chan time.Time {
	f.asked <- d
	return nil
}

func (f *fakeAfter) next(t *testing.T) time.Duration {
	t.Helper()
	select {
	case d := <-f.asked:
		return d
	case <-time.After(10 * time.Second):
		t.Fatal("the supervisor did not start a back-off wait within 10s")
		return 0
	}
}

func (f *fakeAfter) none(t *testing.T) {
	t.Helper()
	if n := len(f.asked); n != 0 {
		t.Errorf("the supervisor started %d back-off waits, want none", n)
	}
}

type start struct {
	pid   int
	value string
}

// readStarts reads the lines RECORD_START appended to the starts file of a monitor's data dir.
func readStarts(t *testing.T, dataDir string) []start {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dataDir, "starts"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var starts []start
	for line := range strings.Lines(string(raw)) {
		pid, value, _ := strings.Cut(strings.TrimSuffix(line, "\n"), " ")
		n, err := strconv.Atoi(pid)
		if err != nil {
			t.Fatalf("starts line %q: %v", line, err)
		}
		starts = append(starts, start{pid: n, value: value})
	}
	return starts
}

func waitStarts(t *testing.T, dataDir string, want int) []start {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		starts := readStarts(t, dataDir)
		if len(starts) >= want {
			return starts
		}
		if time.Now().After(deadline) {
			t.Fatalf("the monitor started %d times within 10s, want %d", len(starts), want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func values(starts []start) []string {
	var values []string
	for _, s := range starts {
		values = append(values, s.value)
	}
	return values
}

func restart(t *testing.T, s *Supervisor, want uint64) {
	t.Helper()
	if got, err := s.Restart("testmonitor"); err != nil || got != want {
		t.Fatalf("Restart = %d, %v; want %d", got, err, want)
	}
}

func TestARestartStartsTheMonitorOnceWithTheNewDocument(t *testing.T) {
	clock := newFakeAfter()
	docs := &fakeDocuments{delivery: Delivery{Document: []byte(`{"mark":"1"}`)}}
	s, dataDir := newWithDocuments(t, []string{"TESTMONITOR__RECORD_START=MARK"}, slog.New(slog.DiscardHandler), docs)
	s.after = clock.after
	launch(t, s)
	c1, changed := trackDescribing(t, s)

	docs.setDocument(`{"mark":"2"}`)
	restart(t, s, 1)
	select {
	case <-changed:
	case <-time.After(10 * time.Second):
		t.Fatal("Track's channel did not close within 10s of the restart")
	}

	if c2, _ := trackDescribing(t, s); c2 == c1 {
		t.Fatal("Track returned the client of the stopped process")
	}
	if got, want := s.LastAttempt("testmonitor"), (Attempt{Request: 1, Phase: PhaseRunning, Settled: 1}); got != want {
		t.Errorf("LastAttempt = %+v, want %+v", got, want)
	}
	if got := values(readStarts(t, dataDir)); !slices.Equal(got, []string{"1", "2"}) {
		t.Errorf("the starts read marks %q, want 1 then 2", got)
	}
	clock.none(t)
}

func TestARestartDuringTheBackOffSkipsTheWaitOnce(t *testing.T) {
	clock := newFakeAfter()
	docs := &fakeDocuments{}
	s, dataDir := newWithDocuments(t, []string{"TESTMONITOR__EXIT_BEFORE_HANDSHAKE=1", "TESTMONITOR__RECORD_START=MARK"}, slog.New(slog.DiscardHandler), docs)
	s.restartDelays = []time.Duration{time.Second, 5 * time.Second, 30 * time.Second}
	s.after = clock.after
	launch(t, s)

	if d := clock.next(t); d != time.Second {
		t.Fatalf("the first back-off wait is %s, want 1s", d)
	}
	if got := s.LastAttempt("testmonitor"); got.Phase != PhaseFailed {
		t.Fatalf("LastAttempt = %+v, want failed", got)
	}

	restart(t, s, 1)
	if d := clock.next(t); d != 5*time.Second {
		t.Fatalf("the back-off wait after the restarted start is %s, want 5s", d)
	}
	failed := s.LastAttempt("testmonitor")
	if failed.Request != 1 || failed.Phase != PhaseFailed || failed.Settled != 1 || failed.Reason == "" {
		t.Fatalf("LastAttempt = %+v, want request 1 failed and settled with a reason", failed)
	}
	if got := len(readStarts(t, dataDir)); got != 2 {
		t.Fatalf("the monitor started %d times, want 2", got)
	}

	docs.setDocument(`{"exit_before_handshake":"0"}`)
	restart(t, s, 2)
	waitDescribe(t, s, 10*time.Second)
	if got := len(readStarts(t, dataDir)); got != 3 {
		t.Errorf("the monitor started %d times, want 3", got)
	}
	if got, want := s.LastAttempt("testmonitor"), (Attempt{Request: 2, Phase: PhaseRunning, Settled: 2, Reason: failed.Reason}); got != want {
		t.Errorf("LastAttempt = %+v, want %+v", got, want)
	}
}

func TestARestartDuringTheHandshakeStartsAgainAtOnce(t *testing.T) {
	clock := newFakeAfter()
	docs := &fakeDocuments{}
	environ := []string{"TESTMONITOR__HANDSHAKE_DELAY=1h", "TESTMONITOR__RECORD_START=MARK", "TESTMONITOR__MARK=env"}
	s, dataDir := newWithDocuments(t, environ, slog.New(slog.DiscardHandler), docs)
	s.after = clock.after
	launch(t, s)
	first := waitStarts(t, dataDir, 1)[0]

	docs.setDocument(`{"handshake_delay":"","mark":"new"}`)
	restart(t, s, 1)
	waitDescribe(t, s, 10*time.Second)

	if left, err := liveMembers(first.pid); err != nil || len(left) != 0 {
		t.Errorf("the group of the first process still has %v (%v)", left, err)
	}
	if got := values(readStarts(t, dataDir)); !slices.Equal(got, []string{"env", "new"}) {
		t.Errorf("the starts read marks %q, want env then new", got)
	}
	if got, want := s.LastAttempt("testmonitor"), (Attempt{Request: 1, Phase: PhaseRunning, Settled: 1}); got != want {
		t.Errorf("LastAttempt = %+v, want %+v", got, want)
	}
	clock.none(t)
}

// openMemfds lists the configuration descriptors the test process holds.
func openMemfds(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatal(err)
	}
	var open []string
	for _, entry := range entries {
		if link, err := os.Readlink("/proc/self/fd/" + entry.Name()); err == nil && strings.HasPrefix(link, "/memfd:monitor-hub-config") {
			open = append(open, link)
		}
	}
	return open
}

func TestARestartDuringPrepareStartsAgainAtOnce(t *testing.T) {
	clock := newFakeAfter()
	held := make(chan struct{})
	docs := &fakeDocuments{delivery: Delivery{Document: []byte(`{"mark":"doc"}`)}, held: held}
	s, dataDir := newWithDocuments(t, []string{"TESTMONITOR__RECORD_START=MARK"}, slog.New(slog.DiscardHandler), docs)
	s.after = clock.after
	launch(t, s)
	select {
	case <-held:
	case <-time.After(10 * time.Second):
		t.Fatal("Prepare did not run within 10s")
	}

	restart(t, s, 1)
	waitDescribe(t, s, 10*time.Second)

	if got := values(readStarts(t, dataDir)); !slices.Equal(got, []string{"doc"}) {
		t.Errorf("the starts read marks %q, want one start", got)
	}
	if open := openMemfds(t); len(open) != 0 {
		t.Errorf("the hub still holds %d configuration descriptors", len(open))
	}
	if got := docs.prepared(); got != 2 {
		t.Errorf("Prepare ran %d times, want 2", got)
	}
	if got, want := s.LastAttempt("testmonitor"), (Attempt{Request: 1, Phase: PhaseRunning, Settled: 1}); got != want {
		t.Errorf("LastAttempt = %+v, want %+v", got, want)
	}
	clock.none(t)
}

func TestARestartLeavesTheOtherMonitorAlone(t *testing.T) {
	dataRoot := t.TempDir()
	entries := []catalog.Entry{entry("a", monitorDir, "testmonitor"), entry("b", monitorDir, "testmonitor")}
	s := New(entries, []string{"A__RECORD_START=MONITOR_HUB_DATA_DIR", "B__RECORD_START=MONITOR_HUB_DATA_DIR"}, dataRoot, slog.New(slog.DiscardHandler))
	launch(t, s)
	a := waitReplaced(t, s, "a", nil, 10*time.Second)
	b := waitReplaced(t, s, "b", nil, 10*time.Second)

	if got, err := s.Restart("a"); err != nil || got != 1 {
		t.Fatalf("Restart(\"a\") = %d, %v; want 1", got, err)
	}
	waitReplaced(t, s, "a", a, 10*time.Second)

	if now, ok := s.Client("b"); !ok || now != b || !describes(t, b) {
		t.Error("the client of b changed or stopped answering when a restarted")
	}
	monitors := filepath.Join(dataRoot, "monitors")
	if got := len(readStarts(t, filepath.Join(monitors, "b"))); got != 1 {
		t.Errorf("b started %d times, want once", got)
	}
	if got := len(readStarts(t, filepath.Join(monitors, "a"))); got != 2 {
		t.Errorf("a started %d times, want twice", got)
	}
}

func TestAttemptKeepsTheStartUpReason(t *testing.T) {
	clock := newFakeAfter()
	environ := []string{"TESTMONITOR__EXIT_BEFORE_HANDSHAKE=1", stderrLines(`{"@level":"error","@message":"fatal","error":"invalid configuration:\n  - EVENTS is wrong"}`)}
	s, _ := newTestSupervisor(t, environ, slog.New(slog.DiscardHandler))
	s.after = clock.after
	launch(t, s)
	clock.next(t)

	want := Attempt{Phase: PhaseFailed, Reason: "invalid configuration:\n  - EVENTS is wrong"}
	if got := s.LastAttempt("testmonitor"); got != want {
		t.Errorf("LastAttempt = %+v, want %+v", got, want)
	}
}

func TestAPrepareFailureIsTheAttemptReason(t *testing.T) {
	const reason = `n: N is not an integer ("three")`
	clock := newFakeAfter()
	docs := &fakeDocuments{err: errors.New(reason)}
	s, _ := newWithDocuments(t, nil, slog.New(slog.DiscardHandler), docs)
	s.after = clock.after
	launch(t, s)
	clock.next(t)

	if got, want := s.LastAttempt("testmonitor"), (Attempt{Phase: PhaseFailed, Reason: reason}); got != want {
		t.Errorf("LastAttempt = %+v, want %+v", got, want)
	}
}

func TestLastAttemptBeforeTheFirstStart(t *testing.T) {
	s := New([]catalog.Entry{entry("testmonitor", monitorDir, "testmonitor")}, nil, t.TempDir(), slog.New(slog.DiscardHandler))
	if got, want := s.LastAttempt("testmonitor"), (Attempt{Phase: PhaseNotStarted}); got != want {
		t.Errorf("LastAttempt = %+v, want %+v", got, want)
	}
}

func TestRestartOfAnUnknownMonitor(t *testing.T) {
	s := New([]catalog.Entry{entry("testmonitor", monitorDir, "testmonitor")}, nil, t.TempDir(), slog.New(slog.DiscardHandler))
	if got, err := s.Restart("absent"); !errors.Is(err, ErrUnknownMonitor) || got != 0 {
		t.Errorf("Restart(\"absent\") = %d, %v; want 0, %v", got, err, ErrUnknownMonitor)
	}
}
