package supervisor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"maps"
	"slices"
	"sync"
)

const maxLogLine = 64 << 10

type logWriter struct {
	log      *slog.Logger
	mu       sync.Mutex
	pending  []byte
	dropping bool
}

func newLogWriter(log *slog.Logger, plugin string) *logWriter {
	return &logWriter{log: log.With("plugin", plugin)}
}

func (w *logWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	rest := p
	for {
		chunk, after, found := bytes.Cut(rest, []byte{'\n'})
		w.collect(chunk)
		if !found {
			return len(p), nil
		}
		w.finishLine()
		rest = after
	}
}

func (w *logWriter) collect(chunk []byte) {
	if w.dropping {
		return
	}
	if len(w.pending)+len(chunk) <= maxLogLine {
		w.pending = append(w.pending, chunk...)
		return
	}
	w.pending = append(w.pending, chunk[:maxLogLine-len(w.pending)]...)
	w.log.LogAttrs(context.Background(), slog.LevelDebug, string(w.pending), slog.Bool("truncated", true))
	w.pending = w.pending[:0]
	w.dropping = true
}

func (w *logWriter) finishLine() {
	line := bytes.TrimSuffix(w.pending, []byte{'\r'})
	if !w.dropping && len(bytes.TrimSpace(line)) > 0 {
		w.emit(line)
	}
	w.pending = w.pending[:0]
	w.dropping = false
}

func (w *logWriter) emit(line []byte) {
	fields, ok := decodeObject(line)
	level, levelOK := fields["@level"].(string)
	message, messageOK := fields["@message"].(string)
	if !ok || !levelOK || !messageOK {
		w.log.LogAttrs(context.Background(), slog.LevelDebug, string(line))
		return
	}
	w.log.LogAttrs(context.Background(), levelOf(level), message, attributes(fields)...)
}

func decodeObject(line []byte) (map[string]any, bool) {
	decoder := json.NewDecoder(bytes.NewReader(line))
	decoder.UseNumber()
	var fields map[string]any
	if err := decoder.Decode(&fields); err != nil || fields == nil {
		return nil, false
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return nil, false
	}
	return fields, true
}

func levelOf(name string) slog.Level {
	levels := map[string]slog.Level{
		"trace": slog.LevelDebug,
		"debug": slog.LevelDebug,
		"info":  slog.LevelInfo,
		"warn":  slog.LevelWarn,
		"error": slog.LevelError,
	}
	if level, known := levels[name]; known {
		return level
	}
	return slog.LevelDebug
}

func attributes(fields map[string]any) []slog.Attr {
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
	return attrs
}
