package supervisor

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"maps"
	"slices"
	"sync"
)

type logWriter struct {
	log     *slog.Logger
	mu      sync.Mutex
	pending []byte
}

func newLogWriter(log *slog.Logger, plugin string) *logWriter {
	return &logWriter{log: log.With("plugin", plugin)}
}

func (w *logWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.pending = append(w.pending, p...)
	rest := w.pending
	for {
		line, after, found := bytes.Cut(rest, []byte{'\n'})
		if !found {
			break
		}
		w.emit(bytes.TrimSuffix(line, []byte{'\r'}))
		rest = after
	}
	w.pending = append(w.pending[:0], rest...)
	return len(p), nil
}

func (w *logWriter) emit(line []byte) {
	if len(bytes.TrimSpace(line)) == 0 {
		return
	}
	var fields map[string]any
	if err := json.Unmarshal(line, &fields); err != nil || fields == nil {
		w.log.LogAttrs(context.Background(), slog.LevelDebug, string(line))
		return
	}
	levels := map[string]slog.Level{
		"trace": slog.LevelDebug,
		"debug": slog.LevelDebug,
		"info":  slog.LevelInfo,
		"warn":  slog.LevelWarn,
		"error": slog.LevelError,
	}
	name, _ := fields["@level"].(string)
	level, known := levels[name]
	if !known {
		level = slog.LevelDebug
	}
	message, _ := fields["@message"].(string)
	var attrs []slog.Attr
	if module, ok := fields["@module"]; ok {
		attrs = append(attrs, slog.Any("module", module))
	}
	for _, key := range []string{"@level", "@message", "@module", "@timestamp"} {
		delete(fields, key)
	}
	reserved := map[string]bool{"plugin": true, "module": true, slog.TimeKey: true, slog.LevelKey: true, slog.MessageKey: true, slog.SourceKey: true}
	for _, key := range slices.Sorted(maps.Keys(fields)) {
		name := key
		if reserved[key] {
			name = "field_" + key
		}
		attrs = append(attrs, slog.Any(name, fields[key]))
	}
	w.log.LogAttrs(context.Background(), level, message, attrs...)
}
