package feed

import (
	"encoding/xml"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ItzTas/monitor-hub/internal/core/coretest"
	"github.com/ItzTas/monitor-hub/internal/db"
)

var t0 = time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)

type event struct {
	plugin, id, title, body, url string
}

func seed(t *testing.T, pool *pgxpool.Pool, events ...event) {
	t.Helper()
	queries := db.New(pool)
	for i, e := range events {
		description := db.SavePluginDescriptionParams{Name: e.plugin, DisplayName: strings.ToUpper(e.plugin), Version: "1.0.0"}
		if err := queries.SavePluginDescription(t.Context(), description); err != nil {
			t.Fatal(err)
		}
		_, err := queries.InsertEvent(t.Context(), db.InsertEventParams{
			Plugin: e.plugin, EventID: e.id, PluginSeq: int64(i + 1), Kind: "ping", Severity: 1,
			OccurredAt: t0.Add(time.Duration(i) * time.Minute), Title: e.title, Body: e.body, Url: e.url,
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

func serveFeed(t *testing.T, pool *pgxpool.Pool, opts Options, target string) *httptest.ResponseRecorder {
	t.Helper()
	mux := http.NewServeMux()
	Configure(mux, pool, opts, slog.New(slog.DiscardHandler))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, target, nil))
	return rec
}

func decode(t *testing.T, rec *httptest.ResponseRecorder) RSS {
	t.Helper()
	if rec.Code != http.StatusOK || !strings.HasPrefix(rec.Header().Get("Content-Type"), "application/rss+xml") {
		t.Fatalf("feed = %d %q: %s", rec.Code, rec.Header().Get("Content-Type"), rec.Body.String())
	}
	var document RSS
	if err := xml.Unmarshal(rec.Body.Bytes(), &document); err != nil {
		t.Fatal(err)
	}
	return document
}

func guids(document RSS) []string {
	values := []string{}
	for _, item := range document.Channel.Items {
		values = append(values, item.GUID.Value)
	}
	return values
}

func TestFeedRequiresTokenWhenSet(t *testing.T) {
	pool := coretest.Pool(t)
	seed(t, pool, event{plugin: "sei", id: "e1", title: "ping 1"})
	cases := map[string]struct {
		token  string
		target string
		want   int
	}{
		"token set, none given":    {"feed-token", "/feed.xml", http.StatusNotFound},
		"token set, wrong one":     {"feed-token", "/feed.xml?token=feed-tokem", http.StatusNotFound},
		"token set, right one":     {"feed-token", "/feed.xml?token=feed-token", http.StatusOK},
		"no token set, none given": {"", "/feed.xml", http.StatusOK},
		"no token set, one given":  {"", "/feed.xml?token=anything", http.StatusOK},
		"token set, prefix of it":  {"feed-token", "/feed.xml?token=feed", http.StatusNotFound},
		"token set, empty one":     {"feed-token", "/feed.xml?token=", http.StatusNotFound},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			rec := serveFeed(t, pool, Options{Token: tc.token}, tc.target)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d", rec.Code, tc.want)
			}
			if tc.want == http.StatusNotFound && strings.Contains(rec.Body.String(), "ping 1") {
				t.Errorf("a refused request got the events: %s", rec.Body.String())
			}
		})
	}
}

func TestFeedListsTheNewestEventsFirst(t *testing.T) {
	pool := coretest.Pool(t)
	seed(t, pool,
		event{plugin: "sei", id: "a", title: "first", body: "line one\nline two", url: "https://sei.example.org/p/1"},
		event{plugin: "diario", id: "b", title: "second"},
	)

	document := decode(t, serveFeed(t, pool, Options{PublicURL: "https://hub.example.org"}, "/feed.xml"))

	want := Channel{
		Title:       "monitor-hub",
		Link:        "https://hub.example.org",
		Description: "Events from every monitor-hub plugin",
		Items: []Item{
			{Title: "DIARIO: second", GUID: GUID{IsPermaLink: "false", Value: "diario:b"}, PubDate: "Tue, 10 Nov 2026 12:01:00 +0000", Category: "ping"},
			{Title: "SEI: first", Link: "https://sei.example.org/p/1", GUID: GUID{IsPermaLink: "false", Value: "sei:a"}, PubDate: "Tue, 10 Nov 2026 12:00:00 +0000", Category: "ping", Description: "line one\nline two"},
		},
	}
	if !reflect.DeepEqual(document.Channel, want) {
		t.Errorf("channel = %+v\nwant      %+v", document.Channel, want)
	}
}

func TestFeedFiltersByPlugin(t *testing.T) {
	pool := coretest.Pool(t)
	seed(t, pool, event{plugin: "sei", id: "a", title: "one"}, event{plugin: "diario", id: "b", title: "two"}, event{plugin: "sei", id: "c", title: "three"})
	cases := map[string]struct {
		target string
		title  string
		want   []string
	}{
		"one plugin":     {"/feed.xml?plugin=sei", "monitor-hub: sei", []string{"sei:c", "sei:a"}},
		"unknown plugin": {"/feed.xml?plugin=nobody", "monitor-hub: nobody", []string{}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			document := decode(t, serveFeed(t, pool, Options{}, tc.target))
			if got := guids(document); !reflect.DeepEqual(got, tc.want) || document.Channel.Title != tc.title {
				t.Errorf("feed %q lists %v, want %q listing %v", document.Channel.Title, got, tc.title, tc.want)
			}
		})
	}
}

func TestFeedKeepsTheNewest100Events(t *testing.T) {
	pool := coretest.Pool(t)
	events := make([]event, 0, 101)
	for i := 1; i <= 101; i++ {
		events = append(events, event{plugin: "sei", id: fmt.Sprintf("e%d", i), title: "ping"})
	}
	seed(t, pool, events...)

	got := guids(decode(t, serveFeed(t, pool, Options{}, "/feed.xml")))

	if len(got) != 100 || got[0] != "sei:e101" || got[99] != "sei:e2" {
		t.Errorf("feed lists %d items from %s to %s, want 100 from sei:e101 to sei:e2", len(got), got[0], got[len(got)-1])
	}
}

func TestFeedEscapesPluginText(t *testing.T) {
	pool := coretest.Pool(t)
	title := `<script>alert(1)</script> & ]]> "x"`
	body := `<b>bold</b> & ]]><![CDATA[ more`
	link := `https://example.org/?a=1&b=<2>"`
	seed(t, pool,
		event{plugin: "sei", id: "<id>&", title: title, body: body, url: link},
		event{plugin: "sei", id: "js", title: "script link", url: "javascript:alert(1)"},
	)

	rec := serveFeed(t, pool, Options{}, "/feed.xml")
	raw := rec.Body.String()
	for _, unsafe := range []string{"<script>", "<b>", "]]>", "<![CDATA[", "javascript:"} {
		if strings.Contains(raw, unsafe) {
			t.Errorf("the feed carries %q unescaped:\n%s", unsafe, raw)
		}
	}
	items := decode(t, rec).Channel.Items
	if len(items) != 2 {
		t.Fatalf("got %d items, want 2", len(items))
	}
	escaped := items[1]
	if escaped.Title != "SEI: "+title || escaped.Description != body || escaped.Link != link || escaped.GUID.Value != "sei:<id>&" {
		t.Errorf("item = %+v, want the plugin's text back unchanged", escaped)
	}
	if items[0].Link != "" {
		t.Errorf("a javascript: address became the item link %q", items[0].Link)
	}
}
