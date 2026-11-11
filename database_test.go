package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/ItzTas/monitor-hub/internal/core/coretest"
)

var testpluginBinary string

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
	testpluginBinary = filepath.Join(dir, "testplugin")
	output, err := exec.CommandContext(context.Background(), "go", "build", "-o", testpluginBinary, "./internal/testplugin").CombinedOutput()
	if err != nil {
		fmt.Fprintf(os.Stderr, "build the test plugin: %v\n%s", err, output)
		return 1
	}
	return coretest.Run(m)
}
