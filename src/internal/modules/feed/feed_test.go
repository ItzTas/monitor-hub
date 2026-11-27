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

	"github.com/ItzTas/monitor-hub/src/internal/access"
	"github.com/ItzTas/monitor-hub/src/internal/accounts"
	"github.com/ItzTas/monitor-hub/src/internal/accounts/accountstest"
	"github.com/ItzTas/monitor-hub/src/internal/consumers"
	"github.com/ItzTas/monitor-hub/src/internal/core/coretest"
	"github.com/ItzTas/monitor-hub/src/internal/db"
)

var t0 = time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)

type event struct {
	monitor, id, title, body, url string
}

func seed(t *testing.T, pool *pgxpool.Pool, events ...event) {
	t.Helper()
	queries := db.New(pool)
	for i, e := range events {
		description := db.SaveMonitorDescriptionParams{Name: e.monitor, DisplayName: strings.ToUpper(e.monitor), Version: "1.0.0"}
		if err := queries.SaveMonitorDescription(t.Context(), description); err != nil {
			t.Fatal(err)
		}
		_, err := queries.InsertEvent(t.Context(), db.InsertEventParams{
			Monitor: e.monitor, EventID: e.id, MonitorSeq: int64(i + 1), Kind: "ping", Severity: 1,
			OccurredAt: t0.Add(time.Duration(i) * time.Minute), Title: e.title, Body: e.body, Url: e.url,
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

func serveFeed(t *testing.T, pool *pgxpool.Pool, opts Options, tokens Tokens, target string) *httptest.ResponseRecorder {
	t.Helper()
	mux := http.NewServeMux()
	Configure(mux, pool, opts, tokens, slog.New(slog.DiscardHandler))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, target, nil))
	return rec
}

type reader func(t *testing.T, opts Options, target string) *httptest.ResponseRecorder

// viewAll reads the feed with the token of an admin that views every monitor.
func viewAll(t *testing.T, pool *pgxpool.Pool) reader {
	t.Helper()
	svc := accounts.NewService(pool, time.Now)
	accountstest.Admin(t, svc, "paulo-santos")
	token := accountstest.Token(t, svc, "paulo-santos", "monitor:*:view")
	return func(t *testing.T, opts Options, target string) *httptest.ResponseRecorder {
		t.Helper()
		separator := "?"
		if strings.Contains(target, "?") {
			separator = "&"
		}
		return serveFeed(t, pool, opts, svc, target+separator+"token="+token)
	}
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

func TestTheFeedFollowsTheToken(t *testing.T) {
	pool := coretest.Pool(t)
	seed(t, pool, event{monitor: "sei", id: "a", title: "one"}, event{monitor: "diario", id: "b", title: "two"}, event{monitor: "sei", id: "c", title: "three"})
	svc := accounts.NewService(pool, time.Now)
	accountstest.Admin(t, svc, "paulo-santos")
	accountstest.Person(t, svc, "ana-lima", "monitor:*:view")
	everything := accountstest.Token(t, svc, "paulo-santos", "monitor:*:view")
	sei := accountstest.Token(t, svc, "ana-lima", "monitor:sei:view")
	diario := accountstest.Token(t, svc, "ana-lima", "monitor:diario:view")

	_, owner, err := svc.Login(t.Context(), "ana-lima", accountstest.Password)
	if err != nil {
		t.Fatal(err)
	}
	expiry := time.Now().Add(time.Hour)
	_, expiring, err := svc.CreateToken(t.Context(), owner, accounts.TokenRequest{Label: "rss", Permissions: []access.Permission{{Monitor: "sei", Action: access.View}}, ExpiresAt: &expiry})
	if err != nil {
		t.Fatal(err)
	}
	revokedToken, revoked, err := svc.CreateToken(t.Context(), owner, accounts.TokenRequest{Label: "old", Permissions: []access.Permission{{Monitor: "sei", Action: access.View}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.RevokeToken(t.Context(), revokedToken.ID, owner.UserID()); err != nil {
		t.Fatal(err)
	}
	consumer, err := consumers.NewService(pool).Add(t.Context(), "desk")
	if err != nil {
		t.Fatal(err)
	}
	later := accounts.NewService(pool, func() time.Time { return expiry.Add(time.Minute) })

	t.Run("allowed", func(t *testing.T) {
		cases := map[string]struct {
			target string
			want   []string
		}{
			"view on sei":                    {"/feed.xml?token=" + sei, []string{"sei:c", "sei:a"}},
			"view on diario, asking for sei": {"/feed.xml?monitor=sei&token=" + diario, []string{}},
			"view on every monitor":          {"/feed.xml?token=" + everything, []string{"sei:c", "diario:b", "sei:a"}},
			"an expiring token in time":      {"/feed.xml?token=" + expiring, []string{"sei:c", "sei:a"}},
		}
		for name, tc := range cases {
			t.Run(name, func(t *testing.T) {
				if got := guids(decode(t, serveFeed(t, pool, Options{}, svc, tc.target))); !reflect.DeepEqual(got, tc.want) {
					t.Errorf("the feed lists %v, want %v", got, tc.want)
				}
			})
		}
	})
	t.Run("refused", func(t *testing.T) {
		cases := map[string]struct {
			tokens Tokens
			target string
		}{
			"no token":         {svc, "/feed.xml"},
			"an empty token":   {svc, "/feed.xml?token="},
			"an invalid token": {svc, "/feed.xml?token=mht_" + strings.Repeat("x", 43)},
			"an expired token": {later, "/feed.xml?token=" + expiring},
			"a revoked token":  {svc, "/feed.xml?token=" + revoked},
			"a consumer token": {svc, "/feed.xml?token=" + consumer},
		}
		for name, tc := range cases {
			t.Run(name, func(t *testing.T) {
				rec := serveFeed(t, pool, Options{}, tc.tokens, tc.target)
				if rec.Code != http.StatusUnauthorized || strings.TrimSpace(rec.Body.String()) != "missing, invalid or expired token" {
					t.Errorf("feed = %d %q, want 401 missing, invalid or expired token", rec.Code, rec.Body.String())
				}
			})
		}
	})
}

func TestFeedListsTheNewestEventsFirst(t *testing.T) {
	pool := coretest.Pool(t)
	seed(t, pool,
		event{monitor: "sei", id: "a", title: "first", body: "line one\nline two", url: "https://sei.example.org/p/1"},
		event{monitor: "diario", id: "b", title: "second"},
	)

	document := decode(t, viewAll(t, pool)(t, Options{PublicURL: "https://hub.example.org"}, "/feed.xml"))

	want := Channel{
		Title:       "monitor-hub",
		Link:        "https://hub.example.org",
		Description: "Events from every monitor-hub monitor",
		Items: []Item{
			{Title: "DIARIO: second", GUID: GUID{IsPermaLink: "false", Value: "diario:b"}, PubDate: "Tue, 10 Nov 2026 12:01:00 +0000", Category: "ping"},
			{Title: "SEI: first", Link: "https://sei.example.org/p/1", GUID: GUID{IsPermaLink: "false", Value: "sei:a"}, PubDate: "Tue, 10 Nov 2026 12:00:00 +0000", Category: "ping", Description: "line one\nline two"},
		},
	}
	if !reflect.DeepEqual(document.Channel, want) {
		t.Errorf("channel = %+v\nwant      %+v", document.Channel, want)
	}
}

func TestFeedFiltersByMonitor(t *testing.T) {
	pool := coretest.Pool(t)
	seed(t, pool, event{monitor: "sei", id: "a", title: "one"}, event{monitor: "diario", id: "b", title: "two"}, event{monitor: "sei", id: "c", title: "three"})
	read := viewAll(t, pool)
	cases := map[string]struct {
		target string
		title  string
		want   []string
	}{
		"one monitor":     {"/feed.xml?monitor=sei", "monitor-hub: sei", []string{"sei:c", "sei:a"}},
		"unknown monitor": {"/feed.xml?monitor=nobody", "monitor-hub: nobody", []string{}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			document := decode(t, read(t, Options{}, tc.target))
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
		events = append(events, event{monitor: "sei", id: fmt.Sprintf("e%d", i), title: "ping"})
	}
	seed(t, pool, events...)

	got := guids(decode(t, viewAll(t, pool)(t, Options{}, "/feed.xml")))

	if len(got) != 100 || got[0] != "sei:e101" || got[99] != "sei:e2" {
		t.Errorf("feed lists %d items from %s to %s, want 100 from sei:e101 to sei:e2", len(got), got[0], got[len(got)-1])
	}
}

func TestFeedEscapesMonitorText(t *testing.T) {
	pool := coretest.Pool(t)
	title := `<script>alert(1)</script> & ]]> "x"`
	body := `<b>bold</b> & ]]><![CDATA[ more`
	link := `https://example.org/?a=1&b=<2>"`
	seed(t, pool,
		event{monitor: "sei", id: "<id>&", title: title, body: body, url: link},
		event{monitor: "sei", id: "js", title: "script link", url: "javascript:alert(1)"},
	)

	rec := viewAll(t, pool)(t, Options{}, "/feed.xml")
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
		t.Errorf("item = %+v, want the monitor's text back unchanged", escaped)
	}
	if items[0].Link != "" {
		t.Errorf("a javascript: address became the item link %q", items[0].Link)
	}
}
