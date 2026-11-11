package supervisor

import (
	"context"
	"fmt"
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

	"github.com/ItzTas/monitor-hub/internal/catalog"
	"github.com/ItzTas/monitor-hub/sdk/pluginpb"
)

var pluginDir string

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
	pluginDir = dir
	out, err := exec.CommandContext(context.Background(), "go", "build", "-o", filepath.Join(dir, "testplugin"), "../testplugin").CombinedOutput()
	if err != nil {
		fmt.Fprintf(os.Stderr, "build failed: %v\n%s", err, out)
		return 1
	}
	return m.Run()
}

func entry(name, dir string, command ...string) catalog.Entry {
	return catalog.Entry{
		Manifest: catalog.Manifest{ProtocolVersion: 1, Name: name, Command: command},
		Dir:      dir,
	}
}

func startSupervisor(t *testing.T, environ []string) (*Supervisor, string, func()) {
	t.Helper()
	return startLoggingSupervisor(t, environ, slog.New(slog.DiscardHandler))
}

func startLoggingSupervisor(t *testing.T, environ []string, logger *slog.Logger) (*Supervisor, string, func()) {
	t.Helper()
	dataRoot := t.TempDir()
	s := New([]catalog.Entry{entry("testplugin", pluginDir, "testplugin")}, environ, dataRoot, logger)
	s.restartDelays = []time.Duration{10 * time.Millisecond}
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
		if _, ok := s.Client("testplugin"); ok {
			t.Error("client still available after Run returned")
		}
	})
	t.Cleanup(stop)
	return s, dataRoot, stop
}

func waitDescribe(t *testing.T, s *Supervisor, timeout time.Duration) pluginpb.PluginClient {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if c, ok := s.Client("testplugin"); ok && describes(t, c) {
			return c
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("plugin did not answer Describe within %s", timeout)
	return nil
}

func describes(t *testing.T, c pluginpb.PluginClient) bool {
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	_, err := c.Describe(ctx, &pluginpb.DescribeRequest{})
	return err == nil
}

func statusMessage(t *testing.T, c pluginpb.PluginClient) string {
	t.Helper()
	resp, err := c.Status(t.Context(), &pluginpb.StatusRequest{})
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	return resp.GetMessage()
}

func TestPluginSeesOnlyItsEnvironment(t *testing.T) {
	t.Setenv("TELEGRAM_TOKEN", "secret")

	t.Run("hub secret", func(t *testing.T) {
		s, _, _ := startSupervisor(t, []string{"TELEGRAM_TOKEN=secret", "TESTPLUGIN__ECHO=TELEGRAM_TOKEN"})
		if got := statusMessage(t, waitDescribe(t, s, 10*time.Second)); got != "" {
			t.Fatalf("plugin saw TELEGRAM_TOKEN=%q", got)
		}
	})

	t.Run("data dir", func(t *testing.T) {
		s, dataRoot, _ := startSupervisor(t, []string{"TELEGRAM_TOKEN=secret", "TESTPLUGIN__ECHO=MONITOR_HUB_DATA_DIR"})
		want := dataRoot + "/plugins/testplugin"
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

func TestCrashedPluginIsRestarted(t *testing.T) {
	s, _, _ := startSupervisor(t, []string{"TESTPLUGIN__CRASH_ON_CHECK=1"})
	c := waitDescribe(t, s, 10*time.Second)
	if _, err := c.Check(t.Context(), &pluginpb.CheckRequest{}); err == nil {
		t.Fatal("Check succeeded, want the plugin to crash")
	}
	waitDescribe(t, s, 5*time.Second)
}

func childEnviron(extra ...string) []string {
	return append([]string{"PATH=" + os.Getenv("PATH"), "TESTPLUGIN__SPAWN_CHILD=1", "TESTPLUGIN__ECHO=CHILD_PID"}, extra...)
}

func childPID(t *testing.T, c pluginpb.PluginClient) int {
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
	s, _, _ := startSupervisor(t, childEnviron("TESTPLUGIN__CRASH_ON_CHECK=1"))
	c := waitDescribe(t, s, 10*time.Second)
	child := childPID(t, c)
	if _, err := c.Check(t.Context(), &pluginpb.CheckRequest{}); err == nil {
		t.Fatal("Check succeeded, want the plugin to crash")
	}
	waitDescribe(t, s, 5*time.Second)
	if running(child) {
		t.Fatalf("child %d of the crashed plugin is still running", child)
	}
}

func TestShutdownKillsPluginProcessGroup(t *testing.T) {
	s, _, stop := startSupervisor(t, childEnviron())
	child := childPID(t, waitDescribe(t, s, 10*time.Second))
	_, pgid, ok := readStat(child)
	if !ok {
		t.Fatalf("child %d not found", child)
	}
	if pgid == syscall.Getpgrp() {
		t.Errorf("plugin shares the hub's process group %d", pgid)
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

func TestRunReturnsOnlyOnceThePluginGroupHasNoLiveMember(t *testing.T) {
	linger := 300 * time.Millisecond
	var (
		mu    sync.Mutex
		first time.Time
	)
	s := New([]catalog.Entry{entry("testplugin", pluginDir, "testplugin")}, nil, t.TempDir(), slog.New(slog.DiscardHandler))
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
		t.Fatal("Run returned without checking the plugin's process group")
	}
	if waited := returned.Sub(first); waited < linger || waited > linger+time.Second {
		t.Errorf("Run returned %s after the first group check, want right after the member left at %s", waited, linger)
	}
}

func TestSlowPluginIsNotRestarted(t *testing.T) {
	stall := 6 * time.Second
	s, _, _ := startSupervisor(t, []string{"TESTPLUGIN__STALL_STATUS=" + stall.String()})
	c := waitDescribe(t, s, 10*time.Second)
	ctx, cancel := context.WithTimeout(t.Context(), stall+5*time.Second)
	defer cancel()
	if _, err := c.Status(ctx, &pluginpb.StatusRequest{}); err != nil {
		t.Fatalf("stalled Status failed: %v", err)
	}
	if now, ok := s.Client("testplugin"); !ok || now != c {
		t.Fatal("the stalled plugin was replaced or dropped")
	}
}

func TestUnknownPluginIsAbsent(t *testing.T) {
	s, _, _ := startSupervisor(t, nil)
	waitDescribe(t, s, 10*time.Second)
	if _, ok := s.Client("nope"); ok {
		t.Fatal("Client(\"nope\") reported a plugin")
	}
}

func TestNames(t *testing.T) {
	s := New([]catalog.Entry{entry("zeta", "", "z"), entry("alpha", "", "a")}, nil, t.TempDir(), slog.New(slog.DiscardHandler))
	if got := s.Names(); !slices.Equal(got, []string{"alpha", "zeta"}) {
		t.Fatalf("Names() = %v", got)
	}
}

func writeExecutable(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestResolveCommand(t *testing.T) {
	dir := t.TempDir()
	pathDir := t.TempDir()
	writeExecutable(t, filepath.Join(dir, "tool"))
	writeExecutable(t, filepath.Join(pathDir, "tool"))
	writeExecutable(t, filepath.Join(pathDir, "other"))
	writeExecutable(t, filepath.Join(filepath.Dir(dir), "escape"))
	env := []string{"PATH=" + pathDir}

	found := map[string]struct{ command, want string }{
		"file in the directory wins": {command: "tool", want: filepath.Join(dir, "tool")},
		"falls back to PATH":         {command: "other", want: filepath.Join(pathDir, "other")},
	}
	for name, tt := range found {
		t.Run(name, func(t *testing.T) {
			got, err := resolveCommand(entry("probe", dir, tt.command), env)
			if err != nil || got != tt.want {
				t.Fatalf("got %q, %v; want %q", got, err, tt.want)
			}
		})
	}

	for name, command := range map[string]string{"unknown command": "missing", "escape through ..": "../escape"} {
		t.Run(name, func(t *testing.T) {
			_, err := resolveCommand(entry("probe", dir, command), env)
			if err == nil || !strings.Contains(err.Error(), "probe") {
				t.Fatalf("got error %v, want one naming the plugin", err)
			}
		})
	}
}
