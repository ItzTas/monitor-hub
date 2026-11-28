package monitorconfig

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ItzTas/monitor-hub/src/internal/catalog"
)

// flakyStore reads through Store until it is down, and fails every read without one.
type flakyStore struct {
	Store
	mu       sync.Mutex
	down     bool
	gets     int
	deadline time.Time
}

func (s *flakyStore) Get(ctx context.Context, monitor string) (Saved, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gets++
	s.deadline, _ = ctx.Deadline()
	if s.down || s.Store == nil {
		return Saved{}, errors.New("dial tcp 127.0.0.1:5432: connection refused")
	}
	return s.Store.Get(ctx, monitor)
}

func (s *flakyStore) fail() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.down = true
}

func logRecords(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var records []map[string]any
	for line := range strings.Lines(buf.String()) {
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("decode a log record: %v", err)
		}
		delete(record, "time")
		records = append(records, record)
	}
	return records
}

const integerFields = `[{"name":"n","label":"N","type":"integer","env":"N"},{"name":"p","label":"P","type":"string","secret":true}]`

func TestPrepareForAMonitorWithoutDeclarationReadsNothing(t *testing.T) {
	store := &flakyStore{down: true}
	environ := []string{"TESTMONITOR__N=3", "TESTMONITOR__MONITOR_HUB_CONFIG_FD=9", "PATH=/bin", "HUB_ONLY=1"}
	d := NewDocuments([]catalog.Entry{entryWith(nil)}, environ, "/data", store, slog.New(slog.DiscardHandler))

	got, err := d.Prepare(t.Context(), "testmonitor")
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if store.gets != 0 {
		t.Errorf("Prepare read the database %d times", store.gets)
	}
	if want := catalog.Environment("testmonitor", environ, "/data", catalog.Layer{}); !slices.Equal(got.Env, want) || got.Document != nil {
		t.Errorf("Prepare = %q with document %s, want %q and none", got.Env, got.Document, want)
	}
}

func TestPrepareUsesTheLastReadConfigurationWhenTheDatabaseFails(t *testing.T) {
	saved := Saved{Document: raws(t, `{"n":7}`), Secrets: map[string]string{"p": "saved-pw-9"}, UpdatedAt: savedAt}
	postgres := newStore(t)
	if _, err := postgres.Put(t.Context(), "testmonitor", saved, 0); err != nil {
		t.Fatalf("Put: %v", err)
	}
	store := &flakyStore{Store: postgres}
	var buf bytes.Buffer
	d := NewDocuments([]catalog.Entry{entryWith(declaration(t, integerFields))}, []string{"TESTMONITOR__N=3"}, "/data", store, slog.New(slog.NewJSONHandler(&buf, nil)))

	asked := time.Now()
	first, err := d.Prepare(t.Context(), "testmonitor")
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if store.deadline.IsZero() || store.deadline.After(asked.Add(5*time.Second+time.Second)) {
		t.Errorf("the read had deadline %v, want at most 5s after %v", store.deadline, asked)
	}
	if doc := decodeDocument(t, first.Document); doc["n"] != json.Number("7") || doc["p"] != "saved-pw-9" {
		t.Fatal("the document is not the saved configuration (values not shown)")
	}

	store.fail()
	second, err := d.Prepare(t.Context(), "testmonitor")
	if err != nil {
		t.Fatalf("Prepare with the database down: %v", err)
	}
	if !bytes.Equal(second.Document, first.Document) || !slices.Equal(second.Env, first.Env) {
		t.Error("with the database down, the delivery differs from the one last read (values not shown)")
	}
	want := []map[string]any{{
		"level":   "WARN",
		"msg":     "the hub's database is unreachable; starting the monitor with the configuration it last read",
		"monitor": "testmonitor",
		"error":   "dial tcp 127.0.0.1:5432: connection refused",
	}}
	if got := logRecords(t, &buf); !reflect.DeepEqual(got, want) {
		t.Errorf("log records = %v, want %v", got, want)
	}

	stopping, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := d.Prepare(stopping, "testmonitor"); !errors.Is(err, context.Canceled) {
		t.Errorf("Prepare while the hub stops = %v, want context.Canceled", err)
	}
	if got := len(logRecords(t, &buf)); got != 1 {
		t.Errorf("%d log records after a start the hub cancelled, want the one warning", got)
	}
}

func TestPrepareWithoutAnyReadFails(t *testing.T) {
	d := NewDocuments([]catalog.Entry{entryWith(declaration(t, integerFields))}, nil, "/data", &flakyStore{down: true}, slog.New(slog.DiscardHandler))
	_, err := d.Prepare(t.Context(), "testmonitor")
	if err == nil || !strings.HasPrefix(err.Error(), "read the saved configuration: ") {
		t.Fatalf("Prepare error = %v, want one that starts with \"read the saved configuration: \"", err)
	}
}

func TestPrepareRefusesTextThatDoesNotConvert(t *testing.T) {
	tests := []struct {
		name    string
		fields  string
		environ []string
		saved   Saved
		want    string
	}{
		{
			name:    "one field",
			fields:  integerFields,
			environ: []string{"TESTMONITOR__N=three"},
			want:    `n: N is not an integer ("three")`,
		},
		{
			name:    "two fields",
			fields:  `[{"name":"n","label":"N","type":"integer","env":"N"},{"name":"m","label":"M","type":"boolean","env":"M"}]`,
			environ: []string{"TESTMONITOR__N=three", "TESTMONITOR__M=yes"},
			want:    `n: N is not an integer ("three"); m: M is not true or false ("yes")`,
		},
		{
			name:   "a problem without a path",
			fields: `[{"name":"s","label":"S","type":"string"}]`,
			saved:  Saved{Document: map[string]json.RawMessage{"s": json.RawMessage(`"` + strings.Repeat("x", 70<<10) + `"`)}, UpdatedAt: savedAt},
			want:   "the configuration document is larger than 65536 bytes",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			postgres := newStore(t)
			if tt.saved.Document != nil {
				if _, err := postgres.Put(t.Context(), "testmonitor", tt.saved, 0); err != nil {
					t.Fatalf("Put: %v", err)
				}
			}
			d := NewDocuments([]catalog.Entry{entryWith(declaration(t, tt.fields))}, tt.environ, "/data", postgres, slog.New(slog.DiscardHandler))
			_, err := d.Prepare(t.Context(), "testmonitor")
			if err == nil || err.Error() != tt.want {
				t.Fatalf("Prepare error = %v, want %q", err, tt.want)
			}
		})
	}
}
