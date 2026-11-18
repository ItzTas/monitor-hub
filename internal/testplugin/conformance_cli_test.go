package main

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ItzTas/monitor-hub/sdk/pluginpb"
	"github.com/ItzTas/monitor-hub/sdk/plugintest"
)

const (
	defaultSettings = `{"settings_version":1,"fields":[{"name":"EVENTS","label":"Events","type":"integer"}]}`
	badSettings     = `{"settings_version":1,"fields":[{"name":"events","label":"Events","type":"integer"}]}`
	badSettingsLine = `settings.json: fields[0]: name "events" does not match ^[A-Z][A-Z0-9_]*$`
)

func conformance(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	return conformanceWithin(t, time.Minute, args...)
}

func conformanceWithin(t *testing.T, limit time.Duration, args ...string) (int, string, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), limit)
	defer cancel()
	cmd := exec.CommandContext(ctx, conformer, args...)
	cmd.WaitDelay = time.Second
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if ctx.Err() != nil {
		t.Fatalf("the conformance command did not return within %s; stderr: %s", limit, stderr.String())
	}
	if exitErr, ok := errors.AsType[*exec.ExitError](err); ok {
		return exitErr.ExitCode(), stdout.String(), stderr.String()
	}
	if err != nil {
		t.Fatalf("run conformance command: %v", err)
	}
	return 0, stdout.String(), stderr.String()
}

func ownLines(stderr string) []string {
	var own []string
	for line := range strings.Lines(stderr) {
		if !strings.HasPrefix(line, "plugin: ") {
			own = append(own, strings.TrimSuffix(line, "\n"))
		}
	}
	return own
}

func isLine(lines []string, want string, prefix bool) bool {
	if len(lines) != 1 {
		return false
	}
	if prefix {
		return strings.HasPrefix(lines[0], want)
	}
	return lines[0] == want
}

func pluginDir(t *testing.T, settings, mode string) string {
	t.Helper()
	dir := t.TempDir()
	t.Cleanup(func() { killProcessesIn(t, dir) })
	if settings == "" {
		settings = defaultSettings
	}
	files := map[string]string{"settings.json": settings}
	if mode != "" {
		files["validator-mode"] = mode
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func killProcessesIn(t *testing.T, dir string) {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Logf("cannot look for processes left in %s: %v", dir, err)
		return
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		t.Logf("cannot look for processes left in %s: %v", dir, err)
		return
	}
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		if cwd, err := os.Readlink(filepath.Join("/proc", entry.Name(), "cwd")); err == nil && cwd == resolved {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	}
}

func settingsArgs(dir string) []string {
	return []string{"-plugin-dir", dir, "-min-events", "1", "-env", "EVENTS=1", "--", binary}
}

func TestConformanceChecksSettings(t *testing.T) {
	t.Setenv("LEAK", "x")
	for name, mode := range map[string]string{"default mode": "", "problems": "problems"} {
		t.Run(name, func(t *testing.T) {
			code, stdout, stderr := conformance(t, settingsArgs(pluginDir(t, "", mode))...)
			if code != 0 {
				t.Fatalf("exit code = %d, want 0; stderr: %s", code, stderr)
			}
			if strings.Count(stdout, "\n") != 1 {
				t.Fatalf("stdout = %q, want one line", stdout)
			}
		})
	}
}

func TestConformanceWithoutSettingsFile(t *testing.T) {
	dir := t.TempDir()
	t.Cleanup(func() { killProcessesIn(t, dir) })
	if err := os.WriteFile(filepath.Join(dir, "validator-mode"), []byte("exit-2"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, want, _ := conformance(t, "-min-events", "1", "-env", "EVENTS=1", "--", binary)
	code, stdout, stderr := conformance(t, settingsArgs(dir)...)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr: %s", code, stderr)
	}
	if stdout != want {
		t.Fatalf("stdout = %q, want %q as without -plugin-dir", stdout, want)
	}
}

func TestConformanceReportsValidatorViolations(t *testing.T) {
	tests := map[string]struct {
		want   string
		prefix bool
		limit  time.Duration
	}{
		"log-first":                {want: "validate-settings did not print a single JSON object on stdout: ", prefix: true},
		"needs-cookie":             {want: "validate-settings printed nothing on stdout; it runs with an empty environment and without MONITOR_HUB_PLUGIN"},
		"hang":                     {want: "validate-settings did not finish within 5s", limit: 15 * time.Second},
		"exit-2":                   {want: "validate-settings exited with code 2, want 0 or 1"},
		"killed":                   {want: "validate-settings was killed by signal 9"},
		"huge":                     {want: "validate-settings wrote more than 65536 bytes on stdout"},
		"huge-exit-2":              {want: "validate-settings exited with code 2, want 0 or 1"},
		"not-object":               {want: "validate-settings did not print a single JSON object on stdout: the answer is not an object"},
		"trailing-data":            {want: "validate-settings did not print a single JSON object on stdout: trailing data after the JSON object"},
		"no-problems":              {want: "validate-settings answer is invalid: problems is missing"},
		"wrong-type":               {want: "validate-settings answer is invalid: ", prefix: true},
		"no-message":               {want: "validate-settings answer is invalid: problem 0 has no message"},
		"blank-message":            {want: "validate-settings answer is invalid: problem 0 has an empty message"},
		"empty-message":            {want: "validate-settings answer is invalid: problem 0 has an empty message"},
		"mismatch":                 {want: "validate-settings exited with code 0 but listed problems"},
		"unlisted":                 {want: "validate-settings exited with code 1 but listed no problems"},
		"undeclared":               {want: `validate-settings answer is invalid: problem 2 names "NOT_DECLARED", which settings.json does not declare`},
		"undeclared-0":             {want: "validate-settings exited with code 0 but listed problems"},
		"undeclared-empty-message": {want: "validate-settings answer is invalid: problem 1 has an empty message"},
	}
	for mode, tt := range tests {
		t.Run(mode, func(t *testing.T) {
			limit := cmp.Or(tt.limit, time.Minute)
			code, stdout, stderr := conformanceWithin(t, limit, settingsArgs(pluginDir(t, "", mode))...)
			if code != 1 || stdout != "" {
				t.Fatalf("exit code = %d, stdout = %q, want 1 and empty; stderr: %s", code, stdout, stderr)
			}
			own := ownLines(stderr)
			if !isLine(own, tt.want, tt.prefix) {
				t.Fatalf("the command's own stderr lines = %q, want one line %q (prefix: %t)", own, tt.want, tt.prefix)
			}
		})
	}
}

func TestConformanceReportsValidatorStartFailure(t *testing.T) {
	code, stdout, stderr := conformance(t, "-plugin-dir", pluginDir(t, "", ""), "--", "/nonexistent/plugin")
	if code != 1 || stdout != "" {
		t.Fatalf("exit code = %d, stdout = %q, want 1 and empty", code, stdout)
	}
	own := ownLines(stderr)
	if len(own) != 2 || !strings.HasPrefix(own[0], "validate-settings could not be started: ") || !strings.HasPrefix(own[1], "plugin-conformance: ") {
		t.Fatalf("the command's own stderr lines = %q, want the validator start failure, then the plugin start failure", own)
	}
}

func TestConformanceValidatorBackgroundChild(t *testing.T) {
	requirePgrep(t)
	code, _, stderr := conformanceWithin(t, 4*time.Second, settingsArgs(pluginDir(t, "", "background"))...)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr: %s", code, stderr)
	}
	if left := processes(t, "-f", "^/bin/sleep 3601$"); len(left) != 0 {
		t.Fatalf("the validator's background child is still running: %v", left)
	}
}

func TestConformanceValidatorDetachedChild(t *testing.T) {
	tests := map[string]struct {
		code  int
		own   []string
		limit time.Duration
	}{
		"detached":      {code: 0, limit: 4 * time.Second},
		"detached-hang": {code: 1, own: []string{"validate-settings did not finish within 5s"}, limit: 9 * time.Second},
	}
	for mode, tt := range tests {
		t.Run(mode, func(t *testing.T) {
			code, _, stderr := conformanceWithin(t, tt.limit, settingsArgs(pluginDir(t, "", mode))...)
			if code != tt.code {
				t.Fatalf("exit code = %d, want %d; stderr: %s", code, tt.code, stderr)
			}
			if own := ownLines(stderr); !slices.Equal(own, tt.own) {
				t.Fatalf("the command's own stderr lines = %q, want %q", own, tt.own)
			}
		})
	}
}

func TestConformanceReportsSettingsFileViolations(t *testing.T) {
	tests := map[string]struct {
		dir    func(t *testing.T) string
		want   string
		prefix bool
	}{
		"bad name": {
			dir: func(t *testing.T) string {
				return pluginDir(t, badSettings, "")
			},
			want: badSettingsLine,
		},
		"unreadable": {
			dir: func(t *testing.T) string {
				dir := t.TempDir()
				t.Cleanup(func() { killProcessesIn(t, dir) })
				if err := os.Mkdir(filepath.Join(dir, "settings.json"), 0o700); err != nil {
					t.Fatal(err)
				}
				return dir
			},
			want:   "settings.json: cannot read: ",
			prefix: true,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			code, stdout, stderr := conformance(t, settingsArgs(tt.dir(t))...)
			if code != 1 || stdout != "" {
				t.Fatalf("exit code = %d, stdout = %q, want 1 and empty; stderr: %s", code, stdout, stderr)
			}
			own := ownLines(stderr)
			if !isLine(own, tt.want, tt.prefix) {
				t.Fatalf("the command's own stderr lines = %q, want one line %q (prefix: %t)", own, tt.want, tt.prefix)
			}
		})
	}
}

func TestConformanceReportsFileViolationsBeforeTheValidators(t *testing.T) {
	code, stdout, stderr := conformance(t, settingsArgs(pluginDir(t, badSettings, "exit-2"))...)
	if code != 1 || stdout != "" {
		t.Fatalf("exit code = %d, stdout = %q, want 1 and empty; stderr: %s", code, stdout, stderr)
	}
	want := []string{badSettingsLine, "validate-settings exited with code 2, want 0 or 1"}
	if own := ownLines(stderr); !slices.Equal(own, want) {
		t.Fatalf("the command's own stderr lines = %q, want %q", own, want)
	}
}

func TestConformanceChecksProblemNamesOnlyAgainstAValidFile(t *testing.T) {
	code, stdout, stderr := conformance(t, settingsArgs(pluginDir(t, badSettings, "undeclared"))...)
	if code != 1 || stdout != "" {
		t.Fatalf("exit code = %d, stdout = %q, want 1 and empty; stderr: %s", code, stdout, stderr)
	}
	if own := ownLines(stderr); !slices.Equal(own, []string{badSettingsLine}) {
		t.Fatalf("the command's own stderr lines = %q, want only %q", own, badSettingsLine)
	}
}

func TestConformanceSettingsViolationsComeBeforeStartFailure(t *testing.T) {
	dir := pluginDir(t, badSettings, "log-stderr")
	code, stdout, stderr := conformance(t, "-plugin-dir", dir, "-env", "STDERR_LINES=cannot open the database", "-env", "EXIT_BEFORE_HANDSHAKE=1", "--", binary)
	if code != 1 || stdout != "" {
		t.Fatalf("exit code = %d, stdout = %q, want 1 and empty", code, stdout)
	}
	lines := strings.Split(strings.TrimSuffix(stderr, "\n"), "\n")
	want := []string{"plugin: validating settings", "plugin: cannot open the database", badSettingsLine}
	if len(lines) != 4 || !slices.Equal(lines[:3], want) || !strings.HasPrefix(lines[3], "plugin-conformance: ") {
		t.Fatalf("stderr lines = %q, want %q, then the start failure", lines, want)
	}
}

func TestConformancePasses(t *testing.T) {
	for _, events := range []string{"3", "150"} {
		t.Run(events+" events", func(t *testing.T) {
			code, stdout, stderr := conformance(t, "-min-events", events, "-env", "EVENTS="+events, "--", binary)
			if code != 0 {
				t.Fatalf("exit code = %d, want 0; stderr: %s", code, stderr)
			}
			if strings.Count(stdout, "\n") != 1 {
				t.Fatalf("stdout = %q, want one line", stdout)
			}
		})
	}
}

func TestConformanceChecksWatch(t *testing.T) {
	tests := map[string]struct {
		code  int
		want  []string
		limit time.Duration
	}{
		"follow": {code: 0},
		"silent": {code: 1, want: []string{"Watch sent no signal within 5s"}, limit: 20 * time.Second},
		"close":  {code: 1, want: []string{"Watch ended after its first signal; it must stay open while the plugin runs"}},
		"fail":   {code: 1, want: []string{"Watch failed: rpc error: code = Internal desc = watch failed"}},
	}
	for mode, tt := range tests {
		t.Run(mode, func(t *testing.T) {
			limit := cmp.Or(tt.limit, time.Minute)
			code, stdout, stderr := conformanceWithin(t, limit, "-env", "EVENTS=3", "-env", "WATCH="+mode, "--", binary)
			if code != tt.code {
				t.Fatalf("exit code = %d, want %d; stderr: %s", code, tt.code, stderr)
			}
			if tt.code == 0 {
				if strings.Count(stdout, "\n") != 1 {
					t.Fatalf("stdout = %q, want one line", stdout)
				}
				return
			}
			if own := ownLines(stderr); !slices.Equal(own, tt.want) {
				t.Fatalf("the command's own stderr lines = %q, want %q", own, tt.want)
			}
		})
	}
}

func TestConformanceReportsPagingViolations(t *testing.T) {
	tests := map[string]struct {
		env  []string
		want string
	}{
		"limit ignored": {
			env:  []string{"EVENTS=3", "IGNORE_LIMIT=1"},
			want: "Events(after = 0, limit = 1) must return exactly the first event, got 3 events",
		},
		"no default limit": {
			env:  []string{"EVENTS=150", "NO_DEFAULT_LIMIT=1"},
			want: "Events(after = 0, limit = 0) returned 150 events, want at most 100: a limit of 0 or less means 100",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			var args []string
			for _, env := range tt.env {
				args = append(args, "-env", env)
			}
			code, stdout, stderr := conformance(t, append(args, "--", binary)...)
			if code != 1 || stdout != "" {
				t.Fatalf("exit code = %d, stdout = %q, want 1 and empty", code, stdout)
			}
			if got := ownLines(stderr); !slices.Equal(got, []string{tt.want}) {
				t.Fatalf("the command's own stderr lines = %q, want %q", got, tt.want)
			}
		})
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
	code, _, _ = conformance(t, "-env", "EVENTS=3", "-env", "UNAVAILABLE_FOR=1h", "--", binary)
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
		"no separator":         {"-min-events", "1"},
		"no command":           {"-min-events", "1", "--"},
		"command no -- sep.":   {binary},
		"env without equals":   {"-env", "EVENTS", "--", binary},
		"plugin dir is a file": {"-plugin-dir", binary, "--", binary},
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

func requirePgrep(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("pgrep"); err == nil {
		return
	}
	if os.Getenv("CI") != "" {
		t.Fatal("pgrep is required when CI is set")
	}
	t.Skip("pgrep is not on PATH")
}

func processes(t *testing.T, match, pattern string) []string {
	t.Helper()
	out, err := exec.CommandContext(t.Context(), "pgrep", match, pattern).Output()
	if exitErr, ok := errors.AsType[*exec.ExitError](err); ok && exitErr.ExitCode() == 1 {
		return nil
	}
	if err != nil {
		t.Fatalf("pgrep: %v", err)
	}
	return strings.Fields(string(out))
}

func TestConformanceStopsPluginOnInterrupt(t *testing.T) {
	requirePgrep(t)
	named := filepath.Join(t.TempDir(), "interrupted")
	if err := os.Symlink(binary, named); err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), conformer, "-env", "UNAVAILABLE_FOR=1h", "-ready-timeout", "1h", "--", named)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	var started []string
	t.Cleanup(func() {
		for _, pid := range started {
			_ = exec.CommandContext(context.Background(), "kill", "-KILL", pid).Run()
		}
	})
	deadline := time.Now().Add(10 * time.Second)
	for len(started) == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
		started = processes(t, "-x", "interrupted")
	}
	if len(started) == 0 {
		t.Fatal("the plugin did not start within 10s")
	}

	if err := cmd.Process.Signal(syscall.SIGINT); err != nil {
		t.Fatal(err)
	}

	err := cmd.Wait()
	if exitErr, ok := errors.AsType[*exec.ExitError](err); !ok || exitErr.ExitCode() != 1 {
		t.Fatalf("wait = %v, want exit code 1", err)
	}
	if left := processes(t, "-x", "interrupted"); len(left) != 0 {
		t.Fatalf("plugin still running: %v", left)
	}
}
