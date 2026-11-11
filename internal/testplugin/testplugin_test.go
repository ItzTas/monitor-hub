package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/ItzTas/monitor-hub/sdk/plugintest"
)

var (
	binary    string
	conformer string
)

func TestMain(m *testing.M) {
	os.Exit(run(m))
}

func run(m *testing.M) int {
	dir, err := os.MkdirTemp("", "testplugin")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer func() { _ = os.RemoveAll(dir) }()
	binary = filepath.Join(dir, "testplugin")
	out, err := exec.CommandContext(context.Background(), "go", "build", "-o", binary, ".").CombinedOutput()
	if err != nil {
		fmt.Fprintf(os.Stderr, "build failed: %v\n%s", err, out)
		return 1
	}
	conformer = filepath.Join(dir, "plugin-conformance")
	out, err = exec.CommandContext(context.Background(), "go", "build", "-o", conformer, "github.com/ItzTas/monitor-hub/sdk/cmd/plugin-conformance").CombinedOutput()
	if err != nil {
		fmt.Fprintf(os.Stderr, "build conformance command failed: %v\n%s", err, out)
		return 1
	}
	return m.Run()
}

func TestTestPluginConformance(t *testing.T) {
	cmd := exec.CommandContext(t.Context(), binary)
	cmd.Env = []string{"EVENTS=3"}
	plugintest.Run(t, cmd, 3)
}
