package examples

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/ItzTas/monitor-hub/internal/catalog"
	"github.com/ItzTas/monitor-hub/internal/supervisor"
)

const rustPlugin = "example-rust"

type record struct {
	level   slog.Level
	message string
	attrs   map[string]slog.Value
}

type records struct {
	mu   sync.Mutex
	list []record
}

type recorder struct {
	records *records
	attrs   []slog.Attr
}

func (recorder) Enabled(context.Context, slog.Level) bool { return true }

func (h recorder) Handle(_ context.Context, r slog.Record) error {
	attrs := map[string]slog.Value{}
	for _, attr := range h.attrs {
		attrs[attr.Key] = attr.Value
	}
	r.Attrs(func(attr slog.Attr) bool {
		attrs[attr.Key] = attr.Value
		return true
	})
	h.records.mu.Lock()
	defer h.records.mu.Unlock()
	h.records.list = append(h.records.list, record{level: r.Level, message: r.Message, attrs: attrs})
	return nil
}

func (h recorder) WithAttrs(attrs []slog.Attr) slog.Handler {
	h.attrs = append(slices.Clone(h.attrs), attrs...)
	return h
}

func (h recorder) WithGroup(string) slog.Handler { return h }

func (r *records) snapshot() []record {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.list)
}

func (r *records) waitFor(timeout time.Duration, done func([]record) bool) []record {
	deadline := time.Now().Add(timeout)
	for {
		got := r.snapshot()
		if done(got) || time.Now().After(deadline) {
			return got
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func fromPlugin(r record) bool {
	return r.attrs["plugin"].String() == rustPlugin
}

func isHello(r record) bool {
	answer, ok := r.attrs["answer"].Any().(json.Number)
	return fromPlugin(r) && r.level == slog.LevelInfo && r.message == "hello from rust" && ok && answer == "42"
}

func isPluginError(r record) bool {
	return fromPlugin(r) && r.level == slog.LevelError
}

func parsedFromJSON(r record) bool {
	_, ok := r.attrs["module"]
	return ok
}

func isLifecycle(r record) bool {
	return slices.Contains([]string{"plugin started", "plugin exited", "plugin stopped", "plugin failed to start"}, r.message)
}

func TestRustLogsKeepTheirLevels(t *testing.T) {
	binary := buildRustExample(t, "fixed")
	pluginDir := t.TempDir()
	command := filepath.Join(pluginDir, rustPlugin)
	if err := os.Symlink(binary, command); err != nil {
		t.Fatal(err)
	}
	entry := catalog.Entry{
		Manifest: catalog.Manifest{ProtocolVersion: 1, Name: rustPlugin, Command: []string{rustPlugin}},
		Dir:      pluginDir,
	}
	captured := &records{}
	s := supervisor.New([]catalog.Entry{entry}, []string{"EXAMPLE_RUST__LOG_DEMO=1"}, t.TempDir(), slog.New(recorder{records: captured}))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()

	got := captured.waitFor(10*time.Second, func(rs []record) bool {
		return slices.ContainsFunc(rs, isHello) && slices.ContainsFunc(rs, isPluginError)
	})
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run returned %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return within 10s of cancellation")
	}

	if !slices.ContainsFunc(got, isHello) {
		t.Error("no info record \"hello from rust\" with plugin=example-rust and answer=42")
	}
	if !slices.ContainsFunc(got, isPluginError) {
		t.Error("no error record from the plugin")
	}
	for _, r := range captured.snapshot() {
		if fromPlugin(r) && !isLifecycle(r) && !parsedFromJSON(r) {
			t.Errorf("a plugin line was logged as plain text: %s %q", r.level, r.message)
		}
	}
	assertNoProcess(t, command)
}
