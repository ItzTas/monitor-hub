package examples

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/ItzTas/monitor-hub/sdk/plugintest"
)

const (
	pythonEnv   = "MONITOR_HUB_EXAMPLE_PYTHON"
	magicCookie = "MONITOR_HUB_PLUGIN=7c0e5a52-monitor-hub"
)

func pythonInterpreter(t *testing.T) string {
	t.Helper()
	name := os.Getenv(pythonEnv)
	if name == "" {
		name = "python3"
	}
	path, err := exec.LookPath(name)
	if err != nil {
		skipOrFail(t, "%s is not on PATH", name)
	}
	probe := exec.CommandContext(t.Context(), path, "-c", "import grpc, google.protobuf")
	if out, err := probe.CombinedOutput(); err != nil {
		skipOrFail(t, "%s cannot import grpc and protobuf: %s", path, out)
	}
	return path
}

func skipOrFail(t *testing.T, format string, args ...any) {
	t.Helper()
	if os.Getenv("CI") != "" {
		t.Fatalf(format+" (required in CI)", args...)
	}
	t.Skipf(format, args...)
}

func pythonScript(t *testing.T) string {
	t.Helper()
	script, err := filepath.Abs(filepath.Join("..", "..", "examples", "python-plugin", "main.py"))
	if err != nil {
		t.Fatal(err)
	}
	return script
}

func TestPythonPluginConformance(t *testing.T) {
	python := pythonInterpreter(t)
	script := pythonScript(t)
	dataDir := t.TempDir()
	socketDir, err := os.MkdirTemp("", "py")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })

	cmd := exec.CommandContext(t.Context(), python, script)
	cmd.Dir = filepath.Dir(script)
	cmd.Env = []string{magicCookie, "MONITOR_HUB_DATA_DIR=" + dataDir, "TMPDIR=" + socketDir}

	violations, err := plugintest.Check(t.Context(), cmd, plugintest.Options{MinEvents: 1})
	if err != nil {
		t.Fatal(err)
	}
	for _, violation := range violations {
		t.Errorf("conformance: %s", violation)
	}

	assertNoProcess(t, script)
	entries, err := os.ReadDir(socketDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("socket dir not empty after shutdown: %v", entries)
	}
}

func TestPythonPluginRefusesToRunByHand(t *testing.T) {
	python := pythonInterpreter(t)
	script := pythonScript(t)

	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(t.Context(), python, script)
	cmd.Env = []string{"MONITOR_HUB_DATA_DIR=" + t.TempDir(), "TMPDIR=" + t.TempDir()}
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()

	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 1 {
		t.Fatalf("exit = %v, want exit status 1", err)
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want empty", stdout.String())
	}
	if stderr.Len() == 0 {
		t.Error("stderr is empty, want a message")
	}
}
