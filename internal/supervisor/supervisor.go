package supervisor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/ItzTas/monitor-hub/internal/catalog"
	"github.com/ItzTas/monitor-hub/sdk"
	"github.com/ItzTas/monitor-hub/sdk/pluginpb"
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

type Supervisor struct {
	entries       []catalog.Entry
	environ       []string
	dataRoot      string
	log           *slog.Logger
	restartDelays []time.Duration
	stableAfter   time.Duration
	groupMembers  func(pgid int) ([]int, error)

	mu      sync.RWMutex
	clients map[string]pluginpb.PluginClient
}

func New(entries []catalog.Entry, environ []string, dataRoot string, log *slog.Logger) *Supervisor {
	return &Supervisor{
		entries:       entries,
		environ:       environ,
		dataRoot:      dataRoot,
		log:           log,
		restartDelays: []time.Duration{time.Second, 5 * time.Second, 30 * time.Second, 2 * time.Minute, 5 * time.Minute},
		stableAfter:   10 * time.Minute,
		groupMembers:  liveMembers,
		clients:       map[string]pluginpb.PluginClient{},
	}
}

func (s *Supervisor) Run(ctx context.Context) error {
	var wg sync.WaitGroup
	for _, entry := range s.entries {
		wg.Go(func() { s.supervise(ctx, entry) })
	}
	wg.Wait()
	return nil
}

func (s *Supervisor) Client(name string) (pluginpb.PluginClient, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	c, ok := s.clients[name]
	return c, ok
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
		uptime, err := s.runOnce(ctx, entry)
		if ctx.Err() != nil {
			return
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
		case <-time.After(delay):
		}
	}
}

func (s *Supervisor) reportStop(name string, uptime, delay time.Duration, startErr error) {
	if startErr != nil {
		s.log.Error("plugin failed to start", "plugin", name, "error", startErr, "retry_in", delay)
		return
	}
	s.log.Warn("plugin exited", "plugin", name, "uptime", uptime.Round(time.Millisecond), "restart_in", delay)
}

func (s *Supervisor) runOnce(ctx context.Context, entry catalog.Entry) (time.Duration, error) {
	name := entry.Manifest.Name
	var ready atomic.Bool
	client, cmd, err := s.newClient(ctx, entry, &ready)
	if err != nil {
		return 0, err
	}
	defer closePidfd(cmd)
	rpc, err := dispense(client)
	if err != nil {
		s.stop(name, client, cmd, false)
		return 0, err
	}
	ready.Store(true)
	s.setClient(name, rpc)
	s.log.Info("plugin started", "plugin", name)
	started := time.Now()
	exited := watchExit(client, *cmd.SysProcAttr.PidFD)
	select {
	case <-ctx.Done():
	case <-exited:
	}
	uptime := time.Since(started)
	s.dropClient(name)
	s.stop(name, client, cmd, ctx.Err() != nil)
	<-exited
	if ctx.Err() != nil {
		s.log.Info("plugin stopped", "plugin", name)
	}
	return uptime, nil
}

func (s *Supervisor) newClient(ctx context.Context, entry catalog.Entry, ready *atomic.Bool) (*plugin.Client, *exec.Cmd, error) {
	env := catalog.Environment(entry.Manifest.Name, s.environ, s.dataRoot)
	if err := os.MkdirAll(envValue(env, sdk.DataDirEnv), 0o700); err != nil {
		return nil, nil, fmt.Errorf("create data dir: %w", err)
	}
	path, err := resolveCommand(entry, env)
	if err != nil {
		return nil, nil, err
	}
	cmd := exec.CommandContext(ctx, path, entry.Manifest.Command[1:]...)
	cmd.Dir = entry.Dir
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
		Plugins:          sdk.PluginSet(),
		Cmd:              cmd,
		AllowedProtocols: []plugin.Protocol{plugin.ProtocolGRPC},
		SkipHostEnv:      true,
		Stderr:           newLogWriter(s.log, entry.Manifest.Name),
		Logger:           hclog.New(&hclog.LoggerOptions{Output: io.Discard, Level: hclog.Off}),
	}), cmd, nil
}

func dispense(client *plugin.Client) (pluginpb.PluginClient, error) {
	rpc, err := client.Client()
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	if err := rpc.Ping(); err != nil {
		return nil, fmt.Errorf("health check: %w", err)
	}
	raw, err := rpc.Dispense(sdk.PluginKey)
	if err != nil {
		return nil, fmt.Errorf("dispense: %w", err)
	}
	return raw.(pluginpb.PluginClient), nil
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
		s.log.Warn("plugin processes still running after the kill; continuing", "plugin", name, "process_group", pgid, "pids", left, "waited", groupExitLimit)
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

func (s *Supervisor) setClient(name string, c pluginpb.PluginClient) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.clients[name] = c
}

func (s *Supervisor) dropClient(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.clients, name)
}

func resolveCommand(entry catalog.Entry, env []string) (string, error) {
	command := entry.Manifest.Command[0]
	local := filepath.Join(entry.Dir, command)
	if filepath.IsLocal(command) && isExecutable(local) {
		return local, nil
	}
	if path, ok := lookPath(command, envValue(env, "PATH")); ok {
		return path, nil
	}
	return "", fmt.Errorf("plugin %s: command %q not found", entry.Manifest.Name, command)
}

func lookPath(command, pathList string) (string, bool) {
	if filepath.IsAbs(command) {
		return command, isExecutable(command)
	}
	if strings.ContainsRune(command, filepath.Separator) {
		return "", false
	}
	for _, dir := range filepath.SplitList(pathList) {
		candidate := filepath.Join(dir, command)
		if filepath.IsAbs(dir) && isExecutable(candidate) {
			return candidate, true
		}
	}
	return "", false
}

func isExecutable(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0
}

func envValue(env []string, key string) string {
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok && k == key {
			return v
		}
	}
	return ""
}
