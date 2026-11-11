package events

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ItzTas/monitor-hub/internal/core/coretest"
	"github.com/ItzTas/monitor-hub/internal/db"
	"github.com/ItzTas/monitor-hub/sdk/pluginpb"
)

var t0 = time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)

type page struct {
	Items      []map[string]any `json:"items"`
	NextBefore *int64           `json:"next_before"`
}

func seed(t *testing.T, pool *pgxpool.Pool, events ...db.InsertEventParams) {
	t.Helper()
	queries := db.New(pool)
	for i, e := range events {
		if err := queries.SavePluginDescription(t.Context(), db.SavePluginDescriptionParams{Name: e.Plugin, DisplayName: "Plugin " + e.Plugin, Version: "1.0.0"}); err != nil {
			t.Fatal(err)
		}
		e.PluginSeq = int64(i + 1)
		e.OccurredAt = t0.Add(time.Duration(i) * time.Minute)
		if e.EventID == "" {
			e.EventID = fmt.Sprintf("e%d", i+1)
		}
		if e.Kind == "" {
			e.Kind = "ping"
		}
		if e.Severity == 0 {
			e.Severity = int16(pluginpb.Severity_SEVERITY_INFO)
		}
		if _, err := queries.InsertEvent(t.Context(), e); err != nil {
			t.Fatal(err)
		}
	}
}

func list(t *testing.T, pool *pgxpool.Pool, query string) (int, page) {
	t.Helper()
	mux := http.NewServeMux()
	config := huma.DefaultConfig("test", "1.0.0")
	config.CreateHooks = nil
	Configure(humago.New(mux, config), pool)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/events"+query, nil))
	var body page
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
	}
	return rec.Code, body
}

func seqs(p page) []float64 {
	values := []float64{}
	for _, item := range p.Items {
		values = append(values, item["seq"].(float64))
	}
	return values
}

func TestEventsShowWhatThePluginSent(t *testing.T) {
	pool := coretest.Pool(t)
	seed(t, pool, db.InsertEventParams{
		Plugin: "sei", EventID: "activity-50001", Kind: "activity_added", Severity: int16(pluginpb.Severity_SEVERITY_URGENT),
		Title: "New activity", Body: "Processo recebido", Url: "https://sei.example.org/p/1", Payload: []byte(`{"unit":"NUPROC","count":2}`),
	})

	code, body := list(t, pool, "")

	if code != http.StatusOK || len(body.Items) != 1 {
		t.Fatalf("events = %d %+v", code, body)
	}
	item := body.Items[0]
	delete(item, "detected_at")
	want := map[string]any{
		"seq": float64(1), "plugin": "sei", "display_name": "Plugin sei", "id": "activity-50001", "kind": "activity_added",
		"severity": "urgent", "occurred_at": "2026-11-10T12:00:00Z", "title": "New activity", "body": "Processo recebido",
		"url": "https://sei.example.org/p/1", "payload": map[string]any{"unit": "NUPROC", "count": float64(2)},
	}
	if !reflect.DeepEqual(item, want) || body.NextBefore != nil {
		t.Errorf("event = %v, next_before %v\nwant    %v, next_before nil", item, body.NextBefore, want)
	}
}

func TestEventsPageNewestFirst(t *testing.T) {
	pool := coretest.Pool(t)
	seed(t, pool,
		db.InsertEventParams{Plugin: "sei"},
		db.InsertEventParams{Plugin: "diario", Kind: "match"},
		db.InsertEventParams{Plugin: "sei", Kind: "match"},
		db.InsertEventParams{Plugin: "sei"},
		db.InsertEventParams{Plugin: "diario"},
	)
	cases := map[string]struct {
		query string
		seqs  []float64
		next  *int64
	}{
		"everything":          {"", []float64{5, 4, 3, 2, 1}, nil},
		"first page":          {"?limit=2", []float64{5, 4}, new(int64(4))},
		"second page":         {"?limit=2&before=4", []float64{3, 2}, new(int64(2))},
		"last page":           {"?limit=2&before=2", []float64{1}, nil},
		"one plugin":          {"?plugin=sei", []float64{4, 3, 1}, nil},
		"one kind":            {"?kind=match", []float64{3, 2}, nil},
		"plugin and kind":     {"?plugin=sei&kind=match", []float64{3}, nil},
		"unknown plugin":      {"?plugin=nobody", []float64{}, nil},
		"before the first":    {"?before=1", []float64{}, nil},
		"filter while paging": {"?plugin=sei&limit=1&before=4", []float64{3}, new(int64(3))},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			code, body := list(t, pool, tc.query)
			if code != http.StatusOK || !reflect.DeepEqual(seqs(body), tc.seqs) || !reflect.DeepEqual(body.NextBefore, tc.next) {
				t.Errorf("events%s = %d %v next %v, want %v next %v", tc.query, code, seqs(body), body.NextBefore, tc.seqs, tc.next)
			}
		})
	}
}

func TestEventsLimit(t *testing.T) {
	pool := coretest.Pool(t)
	events := make([]db.InsertEventParams, 0, 205)
	for range 205 {
		events = append(events, db.InsertEventParams{Plugin: "sei"})
	}
	seed(t, pool, events...)
	cases := map[string]struct {
		query string
		code  int
		count int
	}{
		"default":      {"", http.StatusOK, 50},
		"maximum":      {"?limit=200", http.StatusOK, 200},
		"over maximum": {"?limit=1000", http.StatusOK, 200},
		"zero":         {"?limit=0", http.StatusUnprocessableEntity, 0},
		"negative":     {"?limit=-1", http.StatusUnprocessableEntity, 0},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			code, body := list(t, pool, tc.query)
			if code != tc.code || len(body.Items) != tc.count {
				t.Errorf("events%s = %d with %d items, want %d with %d", tc.query, code, len(body.Items), tc.code, tc.count)
			}
		})
	}
}
