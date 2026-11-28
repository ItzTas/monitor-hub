package supervisor

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/ItzTas/monitor-hub/sdk-go"
	"github.com/ItzTas/monitor-hub/sdk-go/monitorpb"
	"github.com/ItzTas/monitor-hub/sdk-go/settings"
	"github.com/ItzTas/monitor-hub/src/internal/catalog"
	"github.com/ItzTas/monitor-hub/src/internal/memfile"
	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/go-plugin"
	"golang.org/x/sys/unix"
)

const (
	exitPollInterval  = 500 * time.Millisecond
	stopGrace         = 3 * time.Second
	groupExitLimit    = 2 * time.Second
	groupPollInterval = 5 * time.Millisecond
)

var ErrUnknownMonitor = errors.New("no monitor has this name")

type Phase string

const (
	PhaseNotStarted Phase = "not_started"
	PhaseStarting   Phase = "starting"
	// PhaseRunning: the handshake, the health check and Dispense are done.
	PhaseRunning Phase = "running"
	// PhaseFailed: the process exited or failed before running.
	PhaseFailed Phase = "failed"
	// PhaseExited: the process exited after running.
	PhaseExited Phase = "exited"
)

// Attempt is how the current start of a monitor goes. Request is the restart request it serves;
// Settled is the highest request whose start got to running, failed or exited (a start a restart
// or the hub's end interrupts settles nothing). Reason is from the last start that failed or
// exited, and stays while the next one starts.
type Attempt struct {
	Request uint64
	Phase   Phase
	Settled uint64
	Reason  string
}

// Delivery is what one start of a monitor receives: its environment and, for a monitor with a
// declaration, its configuration document.
type Delivery struct {
	Env      []string
	Document []byte
}

type Documents interface {
	Prepare(ctx context.Context, name string) (Delivery, error)
}

type Option func(*Supervisor)

func WithDocuments(d Documents) Option {
	return func(s *Supervisor) { s.documents = d }
}

// environment gives every monitor only its prefixed variables, and no document.
type environment struct {
	environ  []string
	dataRoot string
}

func (e environment) Prepare(_ context.Context, name string) (Delivery, error) {
	return Delivery{Env: catalog.Environment(name, e.environ, e.dataRoot, catalog.Layer{})}, nil
}

type Supervisor struct {
	entries       []catalog.Entry
	documents     Documents
	log           *slog.Logger
	restartDelays []time.Duration
	stableAfter   time.Duration
	groupMembers  func(pgid int) ([]int, error)
	after         func(time.Duration) <-chan time.Time
	// restart holds one channel per monitor, made in New and never replaced.
	restart map[string]chan struct{}

	mu       sync.RWMutex
	clients  map[string]monitorpb.MonitorClient
	changes  map[string]chan struct{}
	requests map[string]uint64
	attempts map[string]Attempt
}

func New(entries []catalog.Entry, environ []string, dataRoot string, log *slog.Logger, options ...Option) *Supervisor {
	s := &Supervisor{
		entries:       entries,
		documents:     environment{environ: environ, dataRoot: dataRoot},
		log:           log,
		restartDelays: []time.Duration{time.Second, 5 * time.Second, 30 * time.Second, 2 * time.Minute, 5 * time.Minute},
		stableAfter:   10 * time.Minute,
		groupMembers:  liveMembers,
		after:         time.After,
		restart:       map[string]chan struct{}{},
		clients:       map[string]monitorpb.MonitorClient{},
		changes:       map[string]chan struct{}{},
		requests:      map[string]uint64{},
		attempts:      map[string]Attempt{},
	}
	for _, entry := range entries {
		s.restart[entry.Manifest.Name] = make(chan struct{}, 1)
	}
	for _, option := range options {
		option(s)
	}
	return s
}

func (s *Supervisor) Run(ctx context.Context) error {
	var wg sync.WaitGroup
	for _, entry := range s.entries {
		wg.Go(func() { s.supervise(ctx, entry) })
	}
	wg.Wait()
	return nil
}

func (s *Supervisor) Client(name string) (monitorpb.MonitorClient, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	c, ok := s.clients[name]
	return c, ok
}

func (s *Supervisor) Track(name string) (monitorpb.MonitorClient, bool, <-chan struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.clients[name]
	changed, known := s.changes[name]
	if !known {
		changed = make(chan struct{})
		s.changes[name] = changed
	}
	return c, ok, changed
}

// Restart stops the monitor's current start or process and starts it again at once, without
// counting a failure. It returns the number of this request.
func (s *Supervisor) Restart(name string) (uint64, error) {
	restart, ok := s.restart[name]
	if !ok {
		return 0, ErrUnknownMonitor
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests[name]++
	select {
	case restart <- struct{}{}:
	default:
	}
	return s.requests[name], nil
}

func (s *Supervisor) LastAttempt(name string) Attempt {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if attempt, ok := s.attempts[name]; ok {
		return attempt
	}
	return Attempt{Phase: PhaseNotStarted}
}

func (s *Supervisor) Names() []string {
	names := make([]string, 0, len(s.entries))
	for _, entry := range s.entries {
		names = append(names, entry.Manifest.Name)
	}
	slices.Sort(names)
	return names
}

func (s *Supervisor) supervise(ctx context.Context, entry catalog.Entry) {
	name := entry.Manifest.Name
	failures := 0
	for {
		uptime, restarted, err := s.runOnce(ctx, entry)
		if ctx.Err() != nil {
			return
		}
		if restarted {
			continue
		}
		if uptime >= s.stableAfter {
			failures = 0
		}
		delay := s.restartDelays[min(failures, len(s.restartDelays)-1)]
		failures++
		s.reportStop(name, uptime, delay, err)
		select {
		case <-ctx.Done():
			return
		case <-s.restart[name]:
		case <-s.after(delay):
		}
	}
}

func (s *Supervisor) reportStop(name string, uptime, delay time.Duration, startErr error) {
	if startErr != nil {
		s.log.Error("monitor failed to start", "monitor", name, "error", startErr, "retry_in", delay)
		return
	}
	s.log.Warn("monitor exited", "monitor", name, "uptime", uptime.Round(time.Millisecond), "restart_in", delay)
}

// runOnce makes one start of the monitor and, once it runs, waits for it to exit. A restart
// request interrupts both and makes restarted true.
func (s *Supervisor) runOnce(ctx context.Context, entry catalog.Entry) (time.Duration, bool, error) {
	name := entry.Manifest.Name
	s.beginAttempt(name)
	attemptCtx, cancel := context.WithCancel(ctx)
	var restarted atomic.Bool
	watched := make(chan struct{})
	go func() {
		defer close(watched)
		select {
		case <-s.restart[name]:
			restarted.Store(true)
			cancel()
		case <-attemptCtx.Done():
		}
	}()
	uptime, err := s.attempt(ctx, attemptCtx, entry)
	cancel()
	<-watched
	return uptime, restarted.Load(), err
}

// attempt runs one start under attemptCtx, which only a restart request or the end of ctx cancels.
func (s *Supervisor) attempt(ctx, attemptCtx context.Context, entry catalog.Entry) (time.Duration, error) {
	name := entry.Manifest.Name
	var ready atomic.Bool
	stderr := newLogWriter(s.log, name)
	client, cmd, err := s.newClient(attemptCtx, entry, &ready, stderr)
	if err != nil {
		s.end(attemptCtx, name, PhaseFailed, err.Error())
		return 0, err
	}
	defer closePidfd(cmd)
	rpc, err := dispense(client)
	// The child holds its own copy of the document's descriptor once it runs.
	for _, f := range cmd.ExtraFiles {
		_ = f.Close()
	}
	if err != nil {
		s.stop(name, client, cmd, false)
		s.end(attemptCtx, name, PhaseFailed, cmp.Or(stderr.Reason(), err.Error()))
		return 0, err
	}
	ready.Store(true)
	s.update(name, func(a *Attempt) {
		a.Phase = PhaseRunning
		a.Settled = a.Request
	})
	s.setClient(name, rpc)
	s.log.Info("monitor started", "monitor", name)
	started := time.Now()
	exited := watchExit(client, *cmd.SysProcAttr.PidFD)
	select {
	case <-attemptCtx.Done():
	case <-exited:
	}
	uptime := time.Since(started)
	s.dropClient(name)
	if ctx.Err() == nil && attemptCtx.Err() != nil {
		s.log.Info("monitor restarting", "monitor", name)
	}
	s.stop(name, client, cmd, attemptCtx.Err() != nil)
	<-exited
	if ctx.Err() != nil {
		s.log.Info("monitor stopped", "monitor", name)
	}
	s.end(attemptCtx, name, PhaseExited, stderr.Reason())
	return uptime, nil
}

// beginAttempt drops any pending restart request: the start that begins here serves it.
func (s *Supervisor) beginAttempt(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	select {
	case <-s.restart[name]:
	default:
	}
	attempt := s.attempts[name]
	attempt.Request = s.requests[name]
	attempt.Phase = PhaseStarting
	s.attempts[name] = attempt
}

// end settles the start in phase, unless a restart or the hub's end interrupted it.
func (s *Supervisor) end(attemptCtx context.Context, name string, phase Phase, reason string) {
	if attemptCtx.Err() != nil {
		return
	}
	s.update(name, func(a *Attempt) {
		a.Phase = phase
		a.Settled = a.Request
		a.Reason = reason
	})
}

func (s *Supervisor) update(name string, change func(*Attempt)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	attempt := s.attempts[name]
	change(&attempt)
	s.attempts[name] = attempt
}

func (s *Supervisor) newClient(ctx context.Context, entry catalog.Entry, ready *atomic.Bool, stderr *logWriter) (*plugin.Client, *exec.Cmd, error) {
	delivery, err := s.documents.Prepare(ctx, entry.Manifest.Name)
	if err != nil {
		return nil, nil, err
	}
	env := delivery.Env
	if err := os.MkdirAll(catalog.EnvValue(env, sdk.DataDirEnv), 0o700); err != nil {
		return nil, nil, fmt.Errorf("create data dir: %w", err)
	}
	path, err := catalog.ResolveCommand(entry, env)
	if err != nil {
		return nil, nil, err
	}
	cmd := exec.CommandContext(ctx, path, entry.Manifest.Command[1:]...)
	cmd.Dir = entry.Dir
	if delivery.Document != nil {
		f, err := memfile.New("monitor-hub-config", delivery.Document)
		if err != nil {
			return nil, nil, fmt.Errorf("hand over the configuration document: %w", err)
		}
		cmd.ExtraFiles = []*os.File{f}
		env = append(slices.Clip(env), settings.ConfigFDEnv+"=3")
	}
	cmd.Env = env
	pidfd := -1
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, PidFD: &pidfd}
	cmd.Cancel = func() error {
		if ready.Load() {
			return os.ErrProcessDone
		}
		killGroup(cmd)
		return nil
	}
	return plugin.NewClient(&plugin.ClientConfig{
		HandshakeConfig:  sdk.Handshake,
		Plugins:          sdk.MonitorSet(),
		Cmd:              cmd,
		AllowedProtocols: []plugin.Protocol{plugin.ProtocolGRPC},
		SkipHostEnv:      true,
		Stderr:           stderr,
		Logger:           hclog.New(&hclog.LoggerOptions{Output: io.Discard, Level: hclog.Off}),
	}), cmd, nil
}

func dispense(client *plugin.Client) (monitorpb.MonitorClient, error) {
	rpc, err := client.Client()
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	if err := rpc.Ping(); err != nil {
		return nil, fmt.Errorf("health check: %w", err)
	}
	raw, err := rpc.Dispense(sdk.MonitorKey)
	if err != nil {
		return nil, fmt.Errorf("dispense: %w", err)
	}
	return raw.(monitorpb.MonitorClient), nil
}

func watchExit(client *plugin.Client, pidfd int) <-chan struct{} {
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		if pidfd >= 0 && waitPidfd(pidfd) {
			return
		}
		for !client.Exited() {
			time.Sleep(exitPollInterval)
		}
	}()
	return exited
}

func waitPidfd(pidfd int) bool {
	var info unix.Siginfo
	for {
		err := unix.Waitid(unix.P_PIDFD, pidfd, &info, unix.WEXITED|unix.WNOWAIT, nil)
		if !errors.Is(err, unix.EINTR) {
			return err == nil || errors.Is(err, unix.ECHILD)
		}
	}
}

func closePidfd(cmd *exec.Cmd) {
	if pidfd := *cmd.SysProcAttr.PidFD; pidfd >= 0 {
		_ = unix.Close(pidfd)
	}
}

func (s *Supervisor) stop(name string, client *plugin.Client, cmd *exec.Cmd, graceful bool) {
	done := make(chan struct{})
	go func() {
		client.Kill()
		close(done)
	}()
	if graceful {
		select {
		case <-done:
		case <-time.After(stopGrace):
		}
	}
	killGroup(cmd)
	<-done
	pgid, ok := processGroup(cmd)
	if !ok {
		return
	}
	if left, gone := awaitGroupExit(s.groupMembers, pgid, groupExitLimit); !gone {
		s.log.Warn("monitor processes still running after the kill; continuing", "monitor", name, "process_group", pgid, "pids", left, "waited", groupExitLimit)
	}
}

func processGroup(cmd *exec.Cmd) (int, bool) {
	if cmd.Process == nil || cmd.Process.Pid <= 0 {
		return 0, false
	}
	return cmd.Process.Pid, true
}

func killGroup(cmd *exec.Cmd) {
	pgid, ok := processGroup(cmd)
	if !ok {
		return
	}
	_ = syscall.Kill(-pgid, syscall.SIGKILL)
}

func awaitGroupExit(members func(int) ([]int, error), pgid int, limit time.Duration) ([]int, bool) {
	deadline := time.Now().Add(limit)
	for {
		left, err := members(pgid)
		if err == nil && len(left) == 0 {
			return nil, true
		}
		if time.Now().After(deadline) {
			return left, false
		}
		time.Sleep(groupPollInterval)
	}
}

func liveMembers(pgid int) ([]int, error) {
	if errors.Is(unix.Kill(-pgid, 0), unix.ESRCH) {
		return nil, nil
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	var live []int
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		if state, group, ok := readStat(pid); ok && group == pgid && state != 'Z' && state != 'X' {
			live = append(live, pid)
		}
	}
	return live, nil
}

func readStat(pid int) (byte, int, bool) {
	raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0, 0, false
	}
	fields := strings.Fields(string(raw[bytes.LastIndexByte(raw, ')')+1:]))
	if len(fields) < 3 || fields[0] == "" {
		return 0, 0, false
	}
	pgid, err := strconv.Atoi(fields[2])
	return fields[0][0], pgid, err == nil
}

func (s *Supervisor) setClient(name string, c monitorpb.MonitorClient) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.clients[name] = c
	s.announce(name)
}

func (s *Supervisor) dropClient(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.clients, name)
	s.announce(name)
}

func (s *Supervisor) announce(name string) {
	if changed, ok := s.changes[name]; ok {
		close(changed)
	}
	s.changes[name] = make(chan struct{})
}
