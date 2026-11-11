package examples

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/ItzTas/monitor-hub/sdk/plugintest"
)

func TestRustExampleConformance(t *testing.T) {
	command := filepath.Join(t.TempDir(), "example-rust")
	if err := os.Symlink(buildRustExample(t, "fixed"), command); err != nil {
		t.Fatal(err)
	}
	socketDir := t.TempDir()
	cmd := exec.CommandContext(t.Context(), command)
	cmd.Env = []string{"EVENTS=3", "TMPDIR=" + socketDir}

	violations, err := plugintest.Check(t.Context(), cmd, plugintest.Options{MinEvents: 3})
	if err != nil {
		t.Fatal(err)
	}
	for _, violation := range violations {
		t.Errorf("conformance: %s", violation)
	}

	assertNoProcess(t, command)
	entries, err := os.ReadDir(socketDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("socket dir not empty after shutdown: %v", entries)
	}
}

func assertNoProcess(t *testing.T, binary string) {
	t.Helper()
	if _, err := exec.LookPath("pgrep"); err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal("pgrep is required in CI but is not on PATH")
		}
		t.Log("pgrep is not on PATH, leftover process check skipped")
		return
	}
	out, err := exec.CommandContext(t.Context(), "pgrep", "-f", binary).Output()
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 {
		return
	}
	if err != nil {
		t.Fatalf("pgrep: %v", err)
	}
	t.Errorf("example processes still running: %s", out)
}
