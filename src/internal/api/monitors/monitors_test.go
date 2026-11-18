package monitors

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc"

	"github.com/ItzTas/monitor-hub/sdk-go/monitorpb"
	"github.com/ItzTas/monitor-hub/src/internal/core/coretest"
	"github.com/ItzTas/monitor-hub/src/internal/db"
)

type fakeClient struct {
	monitorpb.MonitorClient
	answer   *monitorpb.CheckResponse
	err      error
	deadline time.Duration
}

func (c *fakeClient) Check(ctx context.Context, _ *monitorpb.CheckRequest, _ ...grpc.CallOption) (*monitorpb.CheckResponse, error) {
	if deadline, ok := ctx.Deadline(); ok {
		c.deadline = time.Until(deadline)
	}
	return c.answer, c.err
}

type fakeSupervisor struct {
	names   []string
	clients map[string]monitorpb.MonitorClient
}

func (s fakeSupervisor) Names() []string {
	return s.names
}

func (s fakeSupervisor) Client(name string) (monitorpb.MonitorClient, bool) {
	c, ok := s.clients[name]
	return c, ok
}

func serve(t *testing.T, pool *pgxpool.Pool, supervisor Supervisor, method, target string) (int, map[string]any) {
	t.Helper()
	mux := http.NewServeMux()
	config := huma.DefaultConfig("test", "1.0.0")
	config.CreateHooks = nil
	Configure(humago.New(mux, config), pool, supervisor)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), method, target, nil))
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("%s %s = %d %q: %v", method, target, rec.Code, rec.Body.String(), err)
	}
	return rec.Code, body
}

func TestCheckUnknownMonitorIs404(t *testing.T) {
	supervisor := fakeSupervisor{names: []string{"sei"}, clients: map[string]monitorpb.MonitorClient{"sei": &fakeClient{answer: &monitorpb.CheckResponse{Accepted: true}}}}

	code, body := serve(t, nil, supervisor, http.MethodPost, "/api/monitors/diario/check")

	if code != http.StatusNotFound {
		t.Errorf("status = %d %v, want 404", code, body)
	}
}

func TestCheckStoppedMonitorIs503(t *testing.T) {
	supervisor := fakeSupervisor{names: []string{"sei"}}

	code, body := serve(t, nil, supervisor, http.MethodPost, "/api/monitors/sei/check")

	if code != http.StatusServiceUnavailable {
		t.Errorf("status = %d %v, want 503", code, body)
	}
}

func TestCheckAnswersWhatTheMonitorSays(t *testing.T) {
	client := &fakeClient{answer: &monitorpb.CheckResponse{Accepted: false, Message: "a check is already running"}}
	supervisor := fakeSupervisor{names: []string{"sei"}, clients: map[string]monitorpb.MonitorClient{"sei": client}}

	code, body := serve(t, nil, supervisor, http.MethodPost, "/api/monitors/sei/check")

	want := map[string]any{"accepted": false, "message": "a check is already running"}
	if code != http.StatusOK || !reflect.DeepEqual(body, want) {
		t.Errorf("check = %d %v, want 200 %v", code, body, want)
	}
	if client.deadline <= 0 || client.deadline > checkTimeout {
		t.Errorf("the Check call had %s left, want a deadline of at most %s", client.deadline, checkTimeout)
	}
}

func TestCheckFailureIs502(t *testing.T) {
	client := &fakeClient{err: errors.New("connection refused")}
	supervisor := fakeSupervisor{names: []string{"sei"}, clients: map[string]monitorpb.MonitorClient{"sei": client}}

	code, body := serve(t, nil, supervisor, http.MethodPost, "/api/monitors/sei/check")

	if code != http.StatusBadGateway {
		t.Errorf("status = %d %v, want 502", code, body)
	}
}

func TestListShowsConfiguredAndStoredMonitors(t *testing.T) {
	pool := coretest.Pool(t)
	queries := db.New(pool)
	seen := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	success := seen.Add(-time.Hour)
	for _, p := range []db.SaveMonitorDescriptionParams{{Name: "sei", DisplayName: "SEI", Version: "2.1.0"}, {Name: "removed", DisplayName: "Removed", Version: "0.1.0"}} {
		if err := queries.SaveMonitorDescription(t.Context(), p); err != nil {
			t.Fatal(err)
		}
	}
	statuses := []db.SaveMonitorStatusParams{
		{Name: "sei", State: int16(monitorpb.State_STATE_FAILING), Message: "login refused", LastSuccessAt: &success, StatusSeenAt: &seen},
		{Name: "removed", State: 42, StatusSeenAt: &seen},
	}
	for _, status := range statuses {
		if err := queries.SaveMonitorStatus(t.Context(), status); err != nil {
			t.Fatal(err)
		}
	}
	supervisor := fakeSupervisor{
		names:   []string{"diario", "sei"},
		clients: map[string]monitorpb.MonitorClient{"diario": &fakeClient{}},
	}

	code, body := serve(t, pool, supervisor, http.MethodGet, "/api/monitors")

	want := map[string]any{"items": []any{
		map[string]any{
			"name": "diario", "display_name": "", "version": "", "running": true, "state": "unknown", "message": "",
			"last_check_started_at": nil, "last_success_at": nil, "next_check_at": nil, "status_seen_at": nil,
		},
		map[string]any{
			"name": "removed", "display_name": "Removed", "version": "0.1.0", "running": false, "state": "unknown", "message": "",
			"last_check_started_at": nil, "last_success_at": nil, "next_check_at": nil, "status_seen_at": "2026-11-10T12:00:00Z",
		},
		map[string]any{
			"name": "sei", "display_name": "SEI", "version": "2.1.0", "running": false, "state": "failing", "message": "login refused",
			"last_check_started_at": nil, "last_success_at": "2026-11-10T11:00:00Z", "next_check_at": nil, "status_seen_at": "2026-11-10T12:00:00Z",
		},
	}}
	if code != http.StatusOK || !reflect.DeepEqual(body, want) {
		t.Errorf("monitors = %d %v\nwant          %v", code, body, want)
	}
}

func TestStateNames(t *testing.T) {
	cases := map[monitorpb.State]string{
		monitorpb.State_STATE_UNSPECIFIED: "unknown",
		monitorpb.State_STATE_IDLE:        "idle",
		monitorpb.State_STATE_CHECKING:    "checking",
		monitorpb.State_STATE_PAUSED:      "paused",
		monitorpb.State_STATE_FAILING:     "failing",
		monitorpb.State(42):               "unknown",
	}
	for state, want := range cases {
		if got := stateName(int16(state)); got != want {
			t.Errorf("stateName(%d) = %q, want %q", state, got, want)
		}
	}
}
