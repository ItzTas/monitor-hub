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
	"github.com/ItzTas/monitor-hub/src/internal/access"
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

var admin = access.NewSession(access.Person{ID: 1, Login: "paulo-santos", Admin: true})

func person(granted ...access.Permission) access.Principal {
	return access.NewSession(access.Person{ID: 2, Login: "ana-lima", Granted: granted})
}

func serve(t *testing.T, pool *pgxpool.Pool, supervisor Supervisor, p access.Principal, method, target string) (int, map[string]any) {
	t.Helper()
	mux := http.NewServeMux()
	config := huma.DefaultConfig("test", "1.0.0")
	config.CreateHooks = nil
	api := humago.New(mux, config)
	api.UseMiddleware(func(ctx huma.Context, next func(huma.Context)) {
		next(huma.WithContext(ctx, access.WithPrincipal(ctx.Context(), p)))
	})
	Configure(api, pool, supervisor)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), method, target, nil))
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("%s %s = %d %q: %v", method, target, rec.Code, rec.Body.String(), err)
	}
	return rec.Code, body
}

func TestCheckWithoutPermissionIs403BeforeTheName(t *testing.T) {
	supervisor := fakeSupervisor{names: []string{"sei"}, clients: map[string]monitorpb.MonitorClient{"sei": &fakeClient{answer: &monitorpb.CheckResponse{Accepted: true}}}}
	cases := map[string]struct {
		principal access.Principal
		target    string
		want      int
	}{
		"nobody":                      {access.Principal{}, "/api/monitors/sei/check", http.StatusUnauthorized},
		"view but no check":           {person(access.Permission{Monitor: "sei", Action: access.View}), "/api/monitors/sei/check", http.StatusForbidden},
		"check on another monitor":    {person(access.Permission{Monitor: "sei", Action: access.Check}), "/api/monitors/nope/check", http.StatusForbidden},
		"no permission, unknown name": {person(), "/api/monitors/nope/check", http.StatusForbidden},
		"check on every monitor":      {person(access.Permission{Monitor: access.Every, Action: access.Check}), "/api/monitors/nope/check", http.StatusNotFound},
		"an admin, unknown name":      {admin, "/api/monitors/nope/check", http.StatusNotFound},
		"check on the monitor":        {person(access.Permission{Monitor: "sei", Action: access.Check}), "/api/monitors/sei/check", http.StatusOK},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			code, body := serve(t, nil, supervisor, tc.principal, http.MethodPost, tc.target)
			if code != tc.want {
				t.Errorf("POST %s = %d %v, want %d", tc.target, code, body, tc.want)
			}
		})
	}
}

func TestCheckStoppedMonitorIs503(t *testing.T) {
	supervisor := fakeSupervisor{names: []string{"sei"}}

	code, body := serve(t, nil, supervisor, admin, http.MethodPost, "/api/monitors/sei/check")

	if code != http.StatusServiceUnavailable {
		t.Errorf("status = %d %v, want 503", code, body)
	}
}

func TestCheckAnswersWhatTheMonitorSays(t *testing.T) {
	client := &fakeClient{answer: &monitorpb.CheckResponse{Accepted: false, Message: "a check is already running"}}
	supervisor := fakeSupervisor{names: []string{"sei"}, clients: map[string]monitorpb.MonitorClient{"sei": client}}

	code, body := serve(t, nil, supervisor, admin, http.MethodPost, "/api/monitors/sei/check")

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

	code, body := serve(t, nil, supervisor, admin, http.MethodPost, "/api/monitors/sei/check")

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

	code, body := serve(t, pool, supervisor, admin, http.MethodGet, "/api/monitors")

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

func TestListShowsOnlyWhatThePrincipalViews(t *testing.T) {
	pool := coretest.Pool(t)
	if err := db.New(pool).SaveMonitorDescription(t.Context(), db.SaveMonitorDescriptionParams{Name: "removed", DisplayName: "Removed", Version: "0.1.0"}); err != nil {
		t.Fatal(err)
	}
	supervisor := fakeSupervisor{names: []string{"diario", "sei"}}
	cases := map[string]struct {
		principal access.Principal
		want      []string
	}{
		"an admin":              {admin, []string{"diario", "removed", "sei"}},
		"view on every monitor": {person(access.Permission{Monitor: access.Every, Action: access.View}), []string{"diario", "removed", "sei"}},
		"view on one monitor":   {person(access.Permission{Monitor: "sei", Action: access.View}), []string{"sei"}},
		"check without view":    {person(access.Permission{Monitor: "sei", Action: access.Check}), []string{}},
		"a token carrying view": {access.NewToken(access.Person{Granted: []access.Permission{{Monitor: access.Every, Action: access.View}}}, []access.Permission{{Monitor: "diario", Action: access.View}}), []string{"diario"}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			code, body := serve(t, pool, supervisor, tc.principal, http.MethodGet, "/api/monitors")
			names := []string{}
			items, _ := body["items"].([]any)
			for _, item := range items {
				names = append(names, item.(map[string]any)["name"].(string))
			}
			if code != http.StatusOK || !reflect.DeepEqual(names, tc.want) {
				t.Errorf("monitors = %d %v, want %v", code, names, tc.want)
			}
		})
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
		if got := StateName(state); got != want {
			t.Errorf("StateName(%d) = %q, want %q", state, got, want)
		}
	}
}
