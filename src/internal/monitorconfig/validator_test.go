package monitorconfig

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ItzTas/monitor-hub/src/internal/catalog"
)

const validatorFields = `[{"name":"events","label":"Events","type":"integer","env":"EVENTS"}]`

func testValidator() *Validator {
	return &Validator{Limit: 200 * time.Millisecond, WaitDelay: time.Second, AnswerLimit: 65536, StderrLimit: 4096}
}

// validatorEntry is a monitor directory holding the test monitor, its settings.json and, unless
// mode is empty, its validator-mode.
func validatorEntry(t *testing.T, mode string) catalog.Entry {
	t.Helper()
	dir := t.TempDir()
	t.Cleanup(func() { killProcessesIn(t, dir) })
	if err := os.Symlink(testmonitorBinary, filepath.Join(dir, "testmonitor")); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{"settings.json": `{"settings_version":2,"fields":` + validatorFields + `}`}
	if mode != "" {
		files["validator-mode"] = mode
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return catalog.Entry{
		Manifest: catalog.Manifest{Name: "testmonitor", Command: []string{"testmonitor"}},
		Dir:      dir,
		Settings: declaration(t, validatorFields),
	}
}

func validatorEnv() []string {
	return catalog.Environment("testmonitor", nil, "/data", catalog.Layer{})
}

// processesIn lists the command lines of the processes whose working directory is dir.
func processesIn(t *testing.T, dir string) []string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		t.Fatal(err)
	}
	var found []string
	for _, entry := range entries {
		if _, err := strconv.Atoi(entry.Name()); err != nil {
			continue
		}
		if cwd, err := os.Readlink(filepath.Join("/proc", entry.Name(), "cwd")); err != nil || cwd != resolved {
			continue
		}
		cmdline, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "cmdline"))
		if err != nil {
			continue
		}
		found = append(found, strings.ReplaceAll(strings.TrimSuffix(string(cmdline), "\x00"), "\x00", " "))
	}
	return found
}

func killProcessesIn(t *testing.T, dir string) {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
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

// waitForProcesses waits until the processes left in dir, other than those keep accepts, are
// none, and fails after 5 s.
func waitForProcesses(t *testing.T, dir string, keep func(string) bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		left := slices.DeleteFunc(processesIn(t, dir), keep)
		if len(left) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("processes still running in the monitor directory: %q", left)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func keepNone(string) bool { return false }

func TestValidatorAnswers(t *testing.T) {
	tests := []struct {
		name     string
		mode     string
		document string
		want     []Problem
	}{
		{"events refused", "events", `{"events":-1}`, []Problem{{"events", "use a whole number of 0 or more"}}},
		{"events accepted", "events", `{"events":3}`, []Problem{}},
		{"events absent", "events", `{}`, []Problem{}},
		{"problems", "problems", `{"events":3}`, []Problem{{"events", "set events"}}},
		{"echo-events", "echo-events", `{"events":7}`, []Problem{{"events", "EVENTS is 7"}}},
		{"a path that is null or absent is the whole document", "path-null", `{}`, []Problem{{"", "m"}, {"", "n"}}},
		{"valid", "", `{"events":3}`, []Problem{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := testValidator().Validate(t.Context(), validatorEntry(t, tt.mode), validatorEnv(), []byte(tt.document))
			if err != nil {
				t.Fatalf("Validate: %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("Validate = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestValidatorReceivesTheCandidateDocument(t *testing.T) {
	t.Parallel()
	got, err := testValidator().Validate(t.Context(), validatorEntry(t, "report-document"), validatorEnv(), []byte(`{"events":3}`))
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if want := []Problem{{"", `{"events":3}`}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Validate = %#v, want %#v", got, want)
	}
}

func TestValidatorEnvironment(t *testing.T) {
	t.Parallel()
	entry := validatorEntry(t, "report-env")
	environ := []string{"TESTMONITOR__EVENTS=1", "TESTMONITOR__FREE=v", "PATH=" + os.Getenv("PATH"), "TZ=UTC", "HUB_SECRET=x"}
	a := Assemble(entry, environ, "/data", Saved{})
	if len(a.Problems) > 0 {
		t.Fatalf("Assemble problems: %v", a.Problems)
	}

	got, err := testValidator().Validate(t.Context(), entry, a.Env, a.Document)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	want := []Problem{{"", "FREE,MONITOR_HUB_CONFIG_FD,MONITOR_HUB_DATA_DIR,PATH,TMPDIR,TZ"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("the validator's environment = %#v, want %#v", got, want)
	}
}

func TestValidatorFailures(t *testing.T) {
	isDetachedSleep := func(cmdline string) bool { return cmdline == "/bin/sleep 3602" }
	tests := []struct {
		mode    string
		command string
		reason  string
		prefix  bool
		keep    func(string) bool
	}{
		{mode: "hang", reason: "it did not finish within 200ms", keep: keepNone},
		{mode: "detached-hang", reason: "it did not finish within 200ms", keep: isDetachedSleep},
		{mode: "killed", reason: "it was killed by signal 9"},
		{mode: "exit-2", reason: "it exited with code 2"},
		{mode: "huge", reason: "it wrote more than 65536 bytes on stdout"},
		{mode: "bad-utf8", reason: "its answer is not UTF-8"},
		{mode: "log-first", reason: "its answer is not a single JSON object"},
		{mode: "not-object", reason: "its answer is not a single JSON object"},
		{mode: "trailing-data", reason: "its answer is not a single JSON object"},
		{mode: "no-problems", reason: "its answer has no problems list"},
		{mode: "upper-keys", reason: "its answer has no problems list"},
		{mode: "wrong-type", reason: "its answer has no problems list"},
		{mode: "no-message", reason: "problem 0 has no message"},
		{mode: "blank-message", reason: "problem 0 has no message"},
		{mode: "path-not-text", reason: "problem 1 has a path that is not text"},
		{mode: "undeclared", reason: `problem 2 names the path "nope[0]", which settings.json does not declare`},
		{mode: "mismatch", reason: "it exited with code 0 but listed problems"},
		{mode: "unlisted", reason: "it exited with code 1 but listed no problems"},
		{mode: "absent program", command: "absent", reason: "its program could not be found: ", prefix: true},
		{mode: "not a program", command: "not-a-program", reason: "it could not be started: ", prefix: true},
	}
	for _, tt := range tests {
		t.Run(tt.mode, func(t *testing.T) {
			t.Parallel()
			entry := validatorEntry(t, tt.mode)
			if tt.command != "" {
				entry.Manifest.Command = []string{tt.command}
			}
			writeNotAProgram(t, entry.Dir)
			began := time.Now()

			got, err := testValidator().Validate(t.Context(), entry, validatorEnv(), []byte(`{"events":3}`))

			var verr *ValidatorError
			if !errors.As(err, &verr) {
				t.Fatalf("Validate = %#v, %v, want a ValidatorError", got, err)
			}
			if tt.prefix && !strings.HasPrefix(verr.Reason, tt.reason) || !tt.prefix && verr.Reason != tt.reason {
				t.Fatalf("Reason = %q, want %q (prefix: %t)", verr.Reason, tt.reason, tt.prefix)
			}
			if verr.Error() != "the monitor could not validate the values: "+verr.Reason {
				t.Fatalf("Error() = %q", verr.Error())
			}
			if tt.keep == nil {
				return
			}
			if elapsed := time.Since(began); elapsed >= 2*time.Second {
				t.Fatalf("Validate returned after %s, want less than 2s", elapsed)
			}
			waitForProcesses(t, entry.Dir, tt.keep)
		})
	}
}

func writeNotAProgram(t *testing.T, dir string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "not-a-program"), []byte("not a program"), 0o700); err != nil {
		t.Fatal(err)
	}
}

func TestValidatorLeavesNoChildBehind(t *testing.T) {
	for _, mode := range []string{"background", "detached"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			entry := validatorEntry(t, mode)
			began := time.Now()

			got, err := testValidator().Validate(t.Context(), entry, validatorEnv(), []byte(`{"events":3}`))
			if err != nil || len(got) != 0 {
				t.Fatalf("Validate = %#v, %v, want no problems", got, err)
			}
			if elapsed := time.Since(began); elapsed >= 3*time.Second {
				t.Fatalf("Validate returned after %s, want less than 3s", elapsed)
			}
			waitForProcesses(t, entry.Dir, func(cmdline string) bool { return cmdline != "/bin/sleep 3601" })
		})
	}
}

func TestValidatorKeepsOnlyTheWholeLinesAtTheEndOfStderr(t *testing.T) {
	t.Parallel()
	v := testValidator()
	entry := validatorEntry(t, "stderr-file-fail")
	secret := "fictitious-validator-password"
	// The window of StderrLimit bytes starts 10 bytes before the end of the secret.
	kept := strings.Repeat("x", v.StderrLimit-len("\nlast line\n")-11) + "\nlast line\n"
	stderr := "connecting with " + secret + "\n" + kept
	if err := os.WriteFile(filepath.Join(entry.Dir, "validator-stderr"), []byte(stderr), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := v.Validate(t.Context(), entry, validatorEnv(), []byte(`{}`))
	var verr *ValidatorError
	if !errors.As(err, &verr) || verr.Reason != "it exited with code 2" {
		t.Fatalf("Validate error = %v, want a ValidatorError for exit code 2", err)
	}
	if strings.Contains(verr.Stderr, secret[len(secret)-4:]) {
		t.Fatal("stderr holds the end of the validator secret")
	}
	if verr.Stderr != kept {
		t.Fatalf("stderr is %d bytes, want the %d bytes of the whole lines after the cut", len(verr.Stderr), len(kept))
	}

	got, err := v.Validate(t.Context(), validatorEntry(t, "flood-stderr"), validatorEnv(), []byte(`{}`))
	if err != nil || len(got) != 0 {
		t.Fatalf("Validate = %#v, %v, want no problems", got, err)
	}
}

func TestValidatorFindsTheProgramInTheMonitorDirectoryFirst(t *testing.T) {
	t.Parallel()
	bin := t.TempDir()
	script := "#!/bin/sh\necho '{\"problems\":[{\"path\":\"\",\"message\":\"from PATH\"}]}'\nexit 1\n"
	if err := os.WriteFile(filepath.Join(bin, "testmonitor"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	env := catalog.Environment("testmonitor", []string{"PATH=" + bin}, "/data", catalog.Layer{})

	got, err := testValidator().Validate(t.Context(), validatorEntry(t, "events"), env, []byte(`{"events":1}`))
	if err != nil || len(got) != 0 {
		t.Fatalf("Validate = %#v, %v, want the answer of the monitor directory's program", got, err)
	}
}

// openMemfiles counts the descriptors of this process that point at a configuration memfile.
func openMemfiles(t *testing.T) int {
	t.Helper()
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, entry := range entries {
		if link, err := os.Readlink(filepath.Join("/proc/self/fd", entry.Name())); err == nil && strings.HasPrefix(link, "/memfd:monitor-hub-config") {
			n++
		}
	}
	return n
}

// Not parallel: openMemfiles counts the memfiles of the whole test process.
func TestValidatorClosesTheDocument(t *testing.T) {
	ended := map[string]struct {
		mode    string
		command string
	}{
		"after an answer":     {mode: "report-document"},
		"after a time-out":    {mode: "hang"},
		"after a kill":        {mode: "killed"},
		"after a start error": {command: "not-a-program"},
	}
	for name, tt := range ended {
		t.Run(name, func(t *testing.T) {
			entry := validatorEntry(t, tt.mode)
			if tt.command != "" {
				entry.Manifest.Command = []string{tt.command}
				writeNotAProgram(t, entry.Dir)
			}
			_, _ = testValidator().Validate(t.Context(), entry, validatorEnv(), []byte(`{"events":3}`))
			if n := openMemfiles(t); n != 0 {
				t.Fatalf("%d configuration memfiles are still open", n)
			}
		})
	}

	t.Run("while it runs, until the caller gives up", func(t *testing.T) {
		entry := validatorEntry(t, "hang")
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		v := testValidator()
		v.Limit = time.Minute
		done := make(chan error, 1)
		go func() {
			_, err := v.Validate(ctx, entry, validatorEnv(), []byte(`{"events":3}`))
			done <- err
		}()
		deadline := time.Now().Add(5 * time.Second)
		for len(processesIn(t, entry.Dir)) == 0 || openMemfiles(t) != 0 {
			if time.Now().After(deadline) {
				t.Fatal("the validator did not start, or the hub kept its memfile, within 5s")
			}
			time.Sleep(10 * time.Millisecond)
		}

		cancel()
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("Validate error = %v, want context.Canceled", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("Validate did not return within 5s of the cancel")
		}
		waitForProcesses(t, entry.Dir, keepNone)
	})

	t.Run("before it starts", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		_, err := testValidator().Validate(ctx, validatorEntry(t, "report-document"), validatorEnv(), []byte(`{"events":3}`))
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Validate error = %v, want context.Canceled", err)
		}
		if n := openMemfiles(t); n != 0 {
			t.Fatalf("%d configuration memfiles are still open", n)
		}
	})
}
