package plugins

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

	"github.com/ItzTas/monitor-hub/internal/core/coretest"
	"github.com/ItzTas/monitor-hub/internal/db"
	"github.com/ItzTas/monitor-hub/sdk/pluginpb"
)

type fakeClient struct {
	pluginpb.PluginClient
	answer   *pluginpb.CheckResponse
	err      error
	deadline time.Duration
}

func (c *fakeClient) Check(ctx context.Context, _ *pluginpb.CheckRequest, _ ...grpc.CallOption) (*pluginpb.CheckResponse, error) {
	if deadline, ok := ctx.Deadline(); ok {
		c.deadline = time.Until(deadline)
	}
	return c.answer, c.err
}

type fakeSupervisor struct {
	names   []string
	clients map[string]pluginpb.PluginClient
}

func (s fakeSupervisor) Names() []string {
	return s.names
}

func (s fakeSupervisor) Client(name string) (pluginpb.PluginClient, bool) {
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

func TestCheckUnknownPluginIs404(t *testing.T) {
	supervisor := fakeSupervisor{names: []string{"sei"}, clients: map[string]pluginpb.PluginClient{"sei": &fakeClient{answer: &pluginpb.CheckResponse{Accepted: true}}}}

	code, body := serve(t, nil, supervisor, http.MethodPost, "/api/plugins/diario/check")

	if code != http.StatusNotFound {
		t.Errorf("status = %d %v, want 404", code, body)
	}
}

func TestCheckStoppedPluginIs503(t *testing.T) {
	supervisor := fakeSupervisor{names: []string{"sei"}}

	code, body := serve(t, nil, supervisor, http.MethodPost, "/api/plugins/sei/check")

	if code != http.StatusServiceUnavailable {
		t.Errorf("status = %d %v, want 503", code, body)
	}
}

func TestCheckAnswersWhatThePluginSays(t *testing.T) {
	client := &fakeClient{answer: &pluginpb.CheckResponse{Accepted: false, Message: "a check is already running"}}
	supervisor := fakeSupervisor{names: []string{"sei"}, clients: map[string]pluginpb.PluginClient{"sei": client}}

	code, body := serve(t, nil, supervisor, http.MethodPost, "/api/plugins/sei/check")

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
	supervisor := fakeSupervisor{names: []string{"sei"}, clients: map[string]pluginpb.PluginClient{"sei": client}}

	code, body := serve(t, nil, supervisor, http.MethodPost, "/api/plugins/sei/check")

	if code != http.StatusBadGateway {
		t.Errorf("status = %d %v, want 502", code, body)
	}
}

func TestListShowsConfiguredAndStoredPlugins(t *testing.T) {
	pool := coretest.Pool(t)
	queries := db.New(pool)
	seen := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	success := seen.Add(-time.Hour)
	for _, p := range []db.SavePluginDescriptionParams{{Name: "sei", DisplayName: "SEI", Version: "2.1.0"}, {Name: "removed", DisplayName: "Removed", Version: "0.1.0"}} {
		if err := queries.SavePluginDescription(t.Context(), p); err != nil {
			t.Fatal(err)
		}
	}
	statuses := []db.SavePluginStatusParams{
		{Name: "sei", State: int16(pluginpb.State_STATE_FAILING), Message: "login refused", LastSuccessAt: &success, StatusSeenAt: &seen},
		{Name: "removed", State: 42, StatusSeenAt: &seen},
	}
	for _, status := range statuses {
		if err := queries.SavePluginStatus(t.Context(), status); err != nil {
			t.Fatal(err)
		}
	}
	supervisor := fakeSupervisor{
		names:   []string{"diario", "sei"},
		clients: map[string]pluginpb.PluginClient{"diario": &fakeClient{}},
	}

	code, body := serve(t, pool, supervisor, http.MethodGet, "/api/plugins")

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
		t.Errorf("plugins = %d %v\nwant          %v", code, body, want)
	}
}

func TestStateNames(t *testing.T) {
	cases := map[pluginpb.State]string{
		pluginpb.State_STATE_UNSPECIFIED: "unknown",
		pluginpb.State_STATE_IDLE:        "idle",
		pluginpb.State_STATE_CHECKING:    "checking",
		pluginpb.State_STATE_PAUSED:      "paused",
		pluginpb.State_STATE_FAILING:     "failing",
		pluginpb.State(42):               "unknown",
	}
	for state, want := range cases {
		if got := stateName(int16(state)); got != want {
			t.Errorf("stateName(%d) = %q, want %q", state, got, want)
		}
	}
}
