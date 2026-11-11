package health

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ItzTas/monitor-hub/internal/core/coretest"
	"github.com/ItzTas/monitor-hub/internal/db"
	"github.com/ItzTas/monitor-hub/sdk/pluginpb"
)

func serveHealth(t *testing.T, pool *pgxpool.Pool) *httptest.ResponseRecorder {
	t.Helper()
	mux := http.NewServeMux()
	Configure(mux, pool, slog.New(slog.DiscardHandler))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/healthz", nil))
	return rec
}

func TestHealthIgnoresPluginState(t *testing.T) {
	pool := coretest.Pool(t)
	queries := db.New(pool)
	if err := queries.SavePluginDescription(t.Context(), db.SavePluginDescriptionParams{Name: "sei", DisplayName: "SEI", Version: "1.0.0"}); err != nil {
		t.Fatal(err)
	}
	seen := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	if err := queries.SavePluginStatus(t.Context(), db.SavePluginStatusParams{Name: "sei", State: int16(pluginpb.State_STATE_FAILING), Message: "login refused", StatusSeenAt: &seen}); err != nil {
		t.Fatal(err)
	}

	rec := serveHealth(t, pool)

	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != `{"ok":true}` {
		t.Errorf("health = %d %q, want 200 {\"ok\":true}", rec.Code, rec.Body.String())
	}
}

func TestHealthIs503WhenTheDatabaseIsUnreachable(t *testing.T) {
	pool := coretest.Pool(t)
	pool.Close()

	rec := serveHealth(t, pool)

	if rec.Code != http.StatusServiceUnavailable || strings.TrimSpace(rec.Body.String()) != `{"ok":false}` {
		t.Errorf("health = %d %q, want 503 {\"ok\":false}", rec.Code, rec.Body.String())
	}
}
