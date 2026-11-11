package supervisor

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
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

func startSupervisor(t *testing.T, environ []string) (*Supervisor, string) {
	t.Helper()
	dataRoot := t.TempDir()
	s := New([]catalog.Entry{entry("testplugin", pluginDir, "testplugin")}, environ, dataRoot, slog.New(slog.DiscardHandler))
	s.restartDelays = []time.Duration{10 * time.Millisecond}
	s.logOutput = io.Discard
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Run returned %v", err)
		}
		if _, ok := s.Client("testplugin"); ok {
			t.Error("client still available after Run returned")
		}
	})
	return s, dataRoot
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
		s, _ := startSupervisor(t, []string{"TELEGRAM_TOKEN=secret", "TESTPLUGIN__ECHO=TELEGRAM_TOKEN"})
		if got := statusMessage(t, waitDescribe(t, s, 10*time.Second)); got != "" {
			t.Fatalf("plugin saw TELEGRAM_TOKEN=%q", got)
		}
	})

	t.Run("data dir", func(t *testing.T) {
		s, dataRoot := startSupervisor(t, []string{"TELEGRAM_TOKEN=secret", "TESTPLUGIN__ECHO=MONITOR_HUB_DATA_DIR"})
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
	s, _ := startSupervisor(t, []string{"TESTPLUGIN__CRASH_ON_CHECK=1"})
	c := waitDescribe(t, s, 10*time.Second)
	if _, err := c.Check(t.Context(), &pluginpb.CheckRequest{}); err == nil {
		t.Fatal("Check succeeded, want the plugin to crash")
	}
	waitDescribe(t, s, 5*time.Second)
}

func TestUnknownPluginIsAbsent(t *testing.T) {
	s, _ := startSupervisor(t, nil)
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
