package supervisor

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"reflect"
	"strings"
	"sync"
	"testing"
)

type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *logBuffer) records(t *testing.T) []map[string]any {
	t.Helper()
	b.mu.Lock()
	defer b.mu.Unlock()
	var records []map[string]any
	for line := range strings.Lines(b.buf.String()) {
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("decode a log record %q: %v", line, err)
		}
		delete(record, "time")
		records = append(records, record)
	}
	return records
}

func capture() (*slog.Logger, *logBuffer) {
	buf := &logBuffer{}
	return slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})), buf
}

func TestPluginLogLinesBecomeRecords(t *testing.T) {
	cases := map[string]struct {
		line string
		want map[string]any
	}{
		"trace": {
			`{"@level":"trace","@message":"tick","@timestamp":"2026-11-10T12:00:00.000Z"}`,
			map[string]any{"level": "DEBUG", "msg": "tick", "plugin": "sei"},
		},
		"debug": {
			`{"@level":"debug","@message":"plugin address","@module":"sei.sei","@timestamp":"2026-11-10T12:00:00.000Z","address":"/tmp/plugin1","network":"unix"}`,
			map[string]any{"level": "DEBUG", "msg": "plugin address", "plugin": "sei", "module": "sei.sei", "address": "/tmp/plugin1", "network": "unix"},
		},
		"info": {
			`{"@level":"info","@message":"logged in","@module":"sei","units":3}`,
			map[string]any{"level": "INFO", "msg": "logged in", "plugin": "sei", "module": "sei", "units": float64(3)},
		},
		"warn": {
			`{"@level":"warn","@message":"slow answer","@timestamp":"2026-11-10T12:00:00.000Z"}`,
			map[string]any{"level": "WARN", "msg": "slow answer", "plugin": "sei"},
		},
		"error": {
			`{"@level":"error","@message":"login failed","reason":{"code":401}}`,
			map[string]any{"level": "ERROR", "msg": "login failed", "plugin": "sei", "reason": map[string]any{"code": float64(401)}},
		},
		"unknown level": {
			`{"@level":"loud","@message":"hello"}`,
			map[string]any{"level": "DEBUG", "msg": "hello", "plugin": "sei"},
		},
		"no level": {
			`{"@message":"hello"}`,
			map[string]any{"level": "DEBUG", "msg": "hello", "plugin": "sei"},
		},
		"fields named like the record's own": {
			`{"@level":"info","@message":"plugin process exited","@module":"sei","plugin":"/opt/plugins/sei/sei","msg":"x","id":7}`,
			map[string]any{"level": "INFO", "msg": "plugin process exited", "plugin": "sei", "module": "sei", "field_plugin": "/opt/plugins/sei/sei", "field_msg": "x", "id": float64(7)},
		},
		"plain text": {
			`panic: something broke`,
			map[string]any{"level": "DEBUG", "msg": "panic: something broke", "plugin": "sei"},
		},
		"json that is not an object": {
			`[1,2]`,
			map[string]any{"level": "DEBUG", "msg": "[1,2]", "plugin": "sei"},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			logger, buf := capture()
			w := newLogWriter(logger, "sei")
			if _, err := w.Write([]byte(tc.line + "\n")); err != nil {
				t.Fatal(err)
			}
			got := buf.records(t)
			if !reflect.DeepEqual(got, []map[string]any{tc.want}) {
				t.Errorf("records = %v\nwant      %v", got, tc.want)
			}
		})
	}
}

func TestPluginLogLineSplitAcrossWritesIsOneRecord(t *testing.T) {
	logger, buf := capture()
	w := newLogWriter(logger, "sei")
	for _, part := range []string{`{"@level":"info","@mes`, `sage":"one"}` + "\n" + `{"@level":"warn",`, `"@message":"two"}` + "\n"} {
		if _, err := w.Write([]byte(part)); err != nil {
			t.Fatal(err)
		}
	}
	want := []map[string]any{
		{"level": "INFO", "msg": "one", "plugin": "sei"},
		{"level": "WARN", "msg": "two", "plugin": "sei"},
	}
	if got := buf.records(t); !reflect.DeepEqual(got, want) {
		t.Errorf("records = %v\nwant      %v", got, want)
	}
}

func TestPluginLogWritesAreSafeConcurrently(t *testing.T) {
	logger, buf := capture()
	w := newLogWriter(logger, "sei")
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			_, _ = w.Write([]byte(`{"@level":"info","@message":"same"}` + "\n"))
		})
	}
	wg.Wait()
	if got := len(buf.records(t)); got != 20 {
		t.Errorf("got %d records, want 20", got)
	}
}
