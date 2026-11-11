package main

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ItzTas/monitor-hub/sdk/pluginpb"
	"github.com/ItzTas/monitor-hub/sdk/plugintest"
)

func conformance(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), conformer, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if exitErr, ok := errors.AsType[*exec.ExitError](err); ok {
		return exitErr.ExitCode(), stdout.String(), stderr.String()
	}
	if err != nil {
		t.Fatalf("run conformance command: %v", err)
	}
	return 0, stdout.String(), stderr.String()
}

func TestConformancePasses(t *testing.T) {
	code, stdout, stderr := conformance(t, "-min-events", "3", "-env", "EVENTS=3", "--", binary)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr: %s", code, stderr)
	}
	if strings.Count(stdout, "\n") != 1 {
		t.Fatalf("stdout = %q, want one line", stdout)
	}
}

func TestConformanceReportsViolations(t *testing.T) {
	code, stdout, stderr := conformance(t, "-min-events", "5", "-env", "EVENTS=3", "--", binary)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if stdout != "" {
		t.Fatalf("stdout = %q, want empty", stdout)
	}
	if !strings.Contains(stderr, "got 3 events, want at least 5") {
		t.Fatalf("stderr = %q, want the event count violation", stderr)
	}
}

func TestConformanceWaitsForReadiness(t *testing.T) {
	code, _, stderr := conformance(t, "-env", "EVENTS=3", "-env", "UNAVAILABLE_FOR=1s", "-ready-timeout", "5s", "--", binary)
	if code != 0 {
		t.Fatalf("with -ready-timeout: exit code = %d, want 0; stderr: %s", code, stderr)
	}
	code, _, _ = conformance(t, "-env", "EVENTS=3", "-env", "UNAVAILABLE_FOR=1s", "--", binary)
	if code != 1 {
		t.Fatalf("without -ready-timeout: exit code = %d, want 1", code)
	}
}

func TestConformancePassesEnvThroughFlag(t *testing.T) {
	code, _, stderr := conformance(t, "-min-events", "4", "-env", "EVENTS=4", "--", binary)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr: %s", code, stderr)
	}
	code, _, _ = conformance(t, "-min-events", "4", "--", binary)
	if code != 1 {
		t.Fatalf("without -env: exit code = %d, want 1", code)
	}
}

func TestConformanceKeepsValueAfterFirstEquals(t *testing.T) {
	code, _, stderr := conformance(t, "-min-events", "1", "-env", "EVENTS=1", "-env", "A=b=c", "--", binary)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr: %s", code, stderr)
	}
}

func TestConformanceUsage(t *testing.T) {
	tests := map[string][]string{
		"no separator":       {"-min-events", "1"},
		"no command":         {"-min-events", "1", "--"},
		"command no -- sep.": {binary},
		"env without equals": {"-env", "EVENTS", "--", binary},
	}
	for name, args := range tests {
		t.Run(name, func(t *testing.T) {
			if code, _, _ := conformance(t, args...); code != 2 {
				t.Fatalf("exit code = %d, want 2", code)
			}
		})
	}
}

func TestConformanceStartFailure(t *testing.T) {
	code, stdout, stderr := conformance(t, "--", "/nonexistent/plugin")
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if stdout != "" || strings.Count(stderr, "\n") != 1 {
		t.Fatalf("stdout = %q, stderr = %q, want empty stdout and one stderr line", stdout, stderr)
	}
}

func TestConformancePrefixesPluginStderrBeforeItsOwnLines(t *testing.T) {
	code, stdout, stderr := conformance(t, "-env", "STDERR_LINES=cannot open the database\x1fgiving up", "-env", "EXIT_BEFORE_HANDSHAKE=1", "--", binary)
	if code != 1 || stdout != "" {
		t.Fatalf("exit code = %d, stdout = %q, want 1 and empty", code, stdout)
	}
	lines := strings.Split(strings.TrimSuffix(stderr, "\n"), "\n")
	want := []string{"plugin: cannot open the database", "plugin: giving up"}
	if len(lines) != 3 || !slices.Equal(lines[:2], want) || !strings.HasPrefix(lines[2], "plugin-conformance: ") {
		t.Fatalf("stderr = %q, want the plugin's two lines prefixed with \"plugin: \", then the command's own line", stderr)
	}
}

func TestConformancePassesOnlyDeclaredEnv(t *testing.T) {
	t.Setenv("LEAK", "x")
	cmd := exec.CommandContext(t.Context(), binary)
	cmd.Env = []string{"ECHO=LEAK"}
	status, err := plugintest.Dial(t, cmd).Status(t.Context(), &pluginpb.StatusRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if status.GetMessage() != "" {
		t.Fatalf("status message = %q, want empty", status.GetMessage())
	}
}

func TestConformanceStopsPluginOnInterrupt(t *testing.T) {
	named := filepath.Join(t.TempDir(), "interrupted")
	if err := os.Symlink(binary, named); err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), conformer, "-env", "UNAVAILABLE_FOR=1h", "-ready-timeout", "1h", "--", named)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Second)
	if err := cmd.Process.Signal(syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	err := cmd.Wait()
	if exitErr, ok := errors.AsType[*exec.ExitError](err); !ok || exitErr.ExitCode() != 1 {
		t.Fatalf("wait = %v, want exit code 1", err)
	}
	if out, _ := exec.CommandContext(t.Context(), "pgrep", "-x", "interrupted").Output(); len(out) != 0 {
		t.Fatalf("plugin still running: %s", out)
	}
}
