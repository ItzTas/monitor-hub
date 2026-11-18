package app

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/ItzTas/monitor-hub/src/internal/core/coretest"
)

var testmonitorBinary string

func TestMain(m *testing.M) {
	os.Exit(runTests(m))
}

func runTests(m *testing.M) int {
	dir, err := os.MkdirTemp("", "monitor-hub-root")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer func() { _ = os.RemoveAll(dir) }()
	testmonitorBinary = filepath.Join(dir, "testmonitor")
	output, err := exec.CommandContext(context.Background(), "go", "build", "-o", testmonitorBinary, "github.com/ItzTas/monitor-hub/src/internal/testmonitor").CombinedOutput()
	if err != nil {
		fmt.Fprintf(os.Stderr, "build the test monitor: %v\n%s", err, output)
		return 1
	}
	return coretest.Run(m)
}
