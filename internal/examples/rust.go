package examples

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

const buildTimeout = 8 * time.Minute

var (
	buildsMu sync.Mutex
	builds   = map[string]func() (string, error){}
)

type cargoMessage struct {
	Reason string `json:"reason"`
	Target struct {
		Name string   `json:"name"`
		Kind []string `json:"kind"`
	} `json:"target"`
	Executable string `json:"executable"`
}

func buildRustExample(t *testing.T, name string, features ...string) string {
	t.Helper()
	requireTool(t, "cargo")
	path, err := exampleBuild(name, features)()
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func requireTool(t *testing.T, name string) {
	t.Helper()
	if _, err := exec.LookPath(name); err == nil {
		return
	}
	if os.Getenv("CI") != "" {
		t.Fatalf("%s is required in CI but is not on PATH", name)
	}
	t.Skipf("%s is not on PATH", name)
}

func exampleBuild(name string, features []string) func() (string, error) {
	key := strings.Join(append([]string{name}, features...), " ")
	buildsMu.Lock()
	defer buildsMu.Unlock()
	if build, ok := builds[key]; ok {
		return build
	}
	build := sync.OnceValues(func() (string, error) { return cargoBuild(name, features) })
	builds[key] = build
	return build
}

func cargoBuild(name string, features []string) (string, error) {
	dir, err := filepath.Abs(filepath.Join("..", "..", "sdk-rust"))
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), buildTimeout)
	defer cancel()
	args := []string{"build", "--example", name, "--message-format=json-render-diagnostics"}
	if len(features) > 0 {
		args = append(args, "--features", strings.Join(features, ","))
	}
	cmd := exec.CommandContext(ctx, "cargo", args...)
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("cargo build --example %s: %w\n%s", name, err, stderr.String())
	}
	return executable(out, name)
}

func executable(out []byte, name string) (string, error) {
	for line := range bytes.Lines(out) {
		var msg cargoMessage
		if err := json.Unmarshal(line, &msg); err != nil {
			return "", fmt.Errorf("parse cargo output %q: %w", line, err)
		}
		if msg.Reason == "compiler-artifact" && msg.Target.Name == name && slices.Contains(msg.Target.Kind, "example") && msg.Executable != "" {
			return msg.Executable, nil
		}
	}
	return "", fmt.Errorf("cargo build did not report an executable for example %s", name)
}
