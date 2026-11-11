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
		decoder := json.NewDecoder(strings.NewReader(line))
		decoder.UseNumber()
		var record map[string]any
		if err := decoder.Decode(&record); err != nil {
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

func plain(line string) map[string]any {
	return map[string]any{"level": "DEBUG", "msg": line, "plugin": "sei"}
}

func TestPluginLogLinesBecomeRecords(t *testing.T) {
	cases := map[string]struct {
		line string
		want map[string]any
	}{
		"trace": {
			`{"@level":"trace","@message":"tick"}`,
			map[string]any{"level": "DEBUG", "msg": "tick", "plugin": "sei"},
		},
		"debug": {
			`{"@level":"debug","@message":"plugin address","@module":"sei.sei","@timestamp":"2026-11-10T12:00:00.000000-03:00","address":"/tmp/plugin1","network":"unix"}`,
			map[string]any{"level": "DEBUG", "msg": "plugin address", "plugin": "sei", "module": "sei.sei", "address": "/tmp/plugin1", "network": "unix"},
		},
		"info": {
			`{"@level":"info","@message":"logged in","@module":"sei","units":3}`,
			map[string]any{"level": "INFO", "msg": "logged in", "plugin": "sei", "module": "sei", "units": json.Number("3")},
		},
		"warn, timestamp in milliseconds": {
			`{"@level":"warn","@message":"slow answer","@timestamp":"2026-11-10T12:00:00.000Z"}`,
			map[string]any{"level": "WARN", "msg": "slow answer", "plugin": "sei"},
		},
		"error, timestamp not a string": {
			`{"@level":"error","@message":"login failed","@timestamp":7,"reason":{"code":401}}`,
			map[string]any{"level": "ERROR", "msg": "login failed", "plugin": "sei", "reason": map[string]any{"code": json.Number("401")}},
		},
		"unknown level": {
			`{"@level":"loud","@message":"hello"}`,
			map[string]any{"level": "DEBUG", "msg": "hello", "plugin": "sei"},
		},
		"large numbers keep their digits": {
			`{"@level":"info","@message":"ids","id":12345678901234567890,"ratio":0.1000000000000000055511151231257827}`,
			map[string]any{"level": "INFO", "msg": "ids", "plugin": "sei", "id": json.Number("12345678901234567890"), "ratio": json.Number("0.1000000000000000055511151231257827")},
		},
		"fields named like the record's own": {
			`{"@level":"info","@message":"plugin process exited","@module":"sei","plugin":"/opt/plugins/sei/sei","msg":"x","id":7}`,
			map[string]any{"level": "INFO", "msg": "plugin process exited", "plugin": "sei", "module": "sei", "field_plugin": "/opt/plugins/sei/sei", "field_msg": "x", "id": json.Number("7")},
		},
		"no level":                   {`{"@message":"hello"}`, plain(`{"@message":"hello"}`)},
		"no message":                 {`{"@level":"warn"}`, plain(`{"@level":"warn"}`)},
		"message not a string":       {`{"@message":1}`, plain(`{"@message":1}`)},
		"level not a string":         {`{"@level":5,"@message":"x"}`, plain(`{"@level":5,"@message":"x"}`)},
		"plain text":                 {`panic: something broke`, plain(`panic: something broke`)},
		"json that is not an object": {`[1,2]`, plain(`[1,2]`)},
		"null":                       {`null`, plain(`null`)},
		"object then more data":      {`{"@level":"warn","@message":"x"} {}`, plain(`{"@level":"warn","@message":"x"} {}`)},
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

func TestPluginLogLineOverTheLimitIsTruncated(t *testing.T) {
	logger, buf := capture()
	w := newLogWriter(logger, "sei")
	long := strings.Repeat("a", maxLogLine)
	for _, part := range []string{long, "bbbb", strings.Repeat("c", 3*maxLogLine), "\nnext\n"} {
		if _, err := w.Write([]byte(part)); err != nil {
			t.Fatal(err)
		}
		if len(w.pending) > maxLogLine {
			t.Fatalf("the writer holds %d bytes, over the %d limit", len(w.pending), maxLogLine)
		}
	}
	want := []map[string]any{
		{"level": "DEBUG", "msg": long, "plugin": "sei", "truncated": true},
		plain("next"),
	}
	if got := buf.records(t); !reflect.DeepEqual(got, want) {
		t.Errorf("got %d records, want a truncated one of %d bytes and then next", len(got), maxLogLine)
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

func FuzzPluginLogLines(f *testing.F) {
	seeds := []string{
		"\xff\xfe\xfd",
		`{"@level":"info","@message":"` + "\xff" + `"}`,
		`{"@level":"info","@message":"x","` + "\xc3" + `":1}`,
		`{"@level":"info","@message":"x","a":{"b":[1,{"c":null}]}}`,
		`{"@level":"info","@message":"x","@module":["a"]}`,
		`{"@level":null,"@message":null,"@timestamp":null}`,
		`{"@level":"info","@message":"x","n":1e999}`,
		strings.Repeat("[", 20000),
		strings.Repeat(`{"a":`, 20000),
		`{"@level":"info"`,
		"\x00\x01\x02",
		"\r",
		`"just a string"`,
		`{}`,
	}
	for _, seed := range seeds {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, line string) {
		line = strings.ReplaceAll(line, "\n", " ")
		logger, buf := capture()
		w := newLogWriter(logger, "sei")
		if _, err := w.Write([]byte(line + "\n")); err != nil {
			t.Fatal(err)
		}
		want := 1
		if strings.TrimSpace(strings.TrimSuffix(line, "\r")) == "" {
			want = 0
		}
		if got := len(buf.records(t)); got != want {
			t.Errorf("%q gave %d records, want %d", line, got, want)
		}
	})
}
