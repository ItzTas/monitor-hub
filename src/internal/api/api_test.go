package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/ItzTas/monitor-hub/sdk-go/monitorpb"
	"github.com/ItzTas/monitor-hub/src/internal/accounts"
	"github.com/ItzTas/monitor-hub/src/internal/accounts/accountstest"
	"github.com/ItzTas/monitor-hub/src/internal/api/gate"
	"github.com/ItzTas/monitor-hub/src/internal/api/monitors"
	"github.com/ItzTas/monitor-hub/src/internal/consumers"
	"github.com/ItzTas/monitor-hub/src/internal/core/coretest"
	"github.com/ItzTas/monitor-hub/src/internal/ingest"
	"github.com/ItzTas/monitor-hub/src/internal/stream"
)

type fakeSupervisor struct {
	names  []string
	client monitorpb.MonitorClient
}

func (s fakeSupervisor) Names() []string {
	return s.names
}

func (s fakeSupervisor) Client(string) (monitorpb.MonitorClient, bool) {
	return s.client, s.client != nil
}

type bench struct {
	mux  *http.ServeMux
	pool *pgxpool.Pool
	svc  *accounts.Service
	apis []huma.API
}

func newBench(t *testing.T, supervisor monitors.Supervisor, log *slog.Logger) *bench {
	t.Helper()
	pool := coretest.Pool(t)
	b := &bench{mux: http.NewServeMux(), pool: pool, svc: accounts.NewService(pool, time.Now)}
	b.apis = []huma.API{
		Register(b.mux, Deps{Pool: pool, Monitors: supervisor, Accounts: b.svc, Log: log, Consumers: consumers.NewService(pool), Registry: stream.NewRegistry()}),
		RegisterAccess(b.mux, AccessDeps{Accounts: b.svc, Log: log, After: time.After}),
	}
	return b
}

func (b *bench) call(t *testing.T, method, target string, header http.Header) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequestWithContext(t.Context(), method, target, nil)
	maps.Copy(request.Header, header)
	rec := httptest.NewRecorder()
	b.mux.ServeHTTP(rec, request)
	return rec
}

func bearer(token string) http.Header {
	return http.Header{"Authorization": {"Bearer " + token}}
}

type route struct {
	method    string
	path      string
	operation *huma.Operation
}

func (b *bench) routes(t *testing.T) []route {
	t.Helper()
	var all []route
	for _, api := range b.apis {
		for path, item := range api.OpenAPI().Paths {
			for method, operation := range map[string]*huma.Operation{http.MethodGet: item.Get, http.MethodPost: item.Post, http.MethodPut: item.Put, http.MethodPatch: item.Patch, http.MethodDelete: item.Delete} {
				if operation != nil {
					all = append(all, route{method, path, operation})
				}
			}
		}
	}
	if len(all) == 0 {
		t.Fatal("no route registered")
	}
	return all
}

func TestEveryRouteDeclaresAGate(t *testing.T) {
	b := newBench(t, fakeSupervisor{}, slog.New(slog.DiscardHandler))
	levels := map[any]bool{gate.Anyone: true, gate.Identified: true, gate.Session: true}

	for _, r := range b.routes(t) {
		if !strings.HasPrefix(r.path, "/api/") {
			t.Errorf("%s %s is outside /api/", r.method, r.path)
		}
		level := r.operation.Metadata["gate"]
		if !levels[level] {
			t.Errorf("%s %s declares the gate %v, want anyone, identified or session", r.method, r.path, level)
		}
		if level == gate.Anyone && r.operation.OperationID != "create-session" {
			t.Errorf("%s %s is open to anyone; only create-session is", r.method, r.path)
		}
	}
}

func TestNobodyGetsInWithoutSigningIn(t *testing.T) {
	b := newBench(t, fakeSupervisor{names: []string{"sei"}}, slog.New(slog.DiscardHandler))
	params := strings.NewReplacer("{name}", "sei", "{login}", "paulo-santos", "{id}", "1")

	for _, r := range b.routes(t) {
		if r.operation.OperationID == "create-session" {
			continue
		}
		target := params.Replace(r.path)
		rec := b.call(t, r.method, target, nil)
		if rec.Code != http.StatusUnauthorized || rec.Header().Get("WWW-Authenticate") != "Bearer" {
			t.Errorf("%s %s without a session or token = %d, want 401 with WWW-Authenticate: Bearer", r.method, target, rec.Code)
		}
	}
}

func storeEvents(t *testing.T, pool *pgxpool.Pool, monitor string, titles ...string) {
	t.Helper()
	repo := ingest.NewPostgres(pool)
	if err := repo.SaveDescription(t.Context(), &monitorpb.DescribeResponse{Name: monitor, DisplayName: strings.ToUpper(monitor), Version: "1"}); err != nil {
		t.Fatal(err)
	}
	events := make([]*monitorpb.Event, 0, len(titles))
	for i, title := range titles {
		events = append(events, &monitorpb.Event{Seq: int64(i + 1), Id: title, Kind: "ping", OccurredAt: timestamppb.Now(), Title: title})
	}
	if _, err := repo.StoreEvents(t.Context(), ingest.Batch{Monitor: monitor, Events: events}); err != nil {
		t.Fatal(err)
	}
}

func TestCallersSeeOnlyTheMonitorsTheyView(t *testing.T) {
	b := newBench(t, fakeSupervisor{names: []string{"diario", "sei"}}, slog.New(slog.DiscardHandler))
	storeEvents(t, b.pool, "sei", "sei 1")
	storeEvents(t, b.pool, "diario", "diario 1")
	accountstest.Person(t, b.svc, "ana-lima", "monitor:sei:view")
	token := accountstest.Token(t, b.svc, "ana-lima", "monitor:sei:view")

	var listed struct {
		Items []struct {
			Name    string `json:"name"`
			Monitor string `json:"monitor"`
			Title   string `json:"title"`
		} `json:"items"`
	}
	monitorList := b.call(t, http.MethodGet, "/api/monitors", bearer(token))
	if err := json.Unmarshal(monitorList.Body.Bytes(), &listed); err != nil || monitorList.Code != http.StatusOK || len(listed.Items) != 1 || listed.Items[0].Name != "sei" {
		t.Errorf("monitors = %d %s (%v), want only sei", monitorList.Code, monitorList.Body.String(), err)
	}
	listed.Items = nil
	eventList := b.call(t, http.MethodGet, "/api/events", bearer(token))
	if err := json.Unmarshal(eventList.Body.Bytes(), &listed); err != nil || eventList.Code != http.StatusOK || len(listed.Items) != 1 || listed.Items[0].Title != "sei 1" {
		t.Errorf("events = %d %s (%v), want only sei 1", eventList.Code, eventList.Body.String(), err)
	}
	session := http.Header{"Cookie": {gate.CookieName + "=" + accountstest.Session(t, b.svc, "ana-lima")}}
	for _, refused := range []struct {
		name, method, target string
		header               http.Header
	}{
		{"token", http.MethodPost, "/api/monitors/sei/check", bearer(token)},
		{"token", http.MethodGet, "/api/consumers", bearer(token)},
		{"session", http.MethodGet, "/api/consumers", session},
	} {
		if rec := b.call(t, refused.method, refused.target, refused.header); rec.Code != http.StatusForbidden {
			t.Errorf("%s %s with a %s = %d %s, want 403", refused.method, refused.target, refused.name, rec.Code, rec.Body.String())
		}
	}
}

type failingClient struct {
	monitorpb.MonitorClient
}

func (failingClient) Check(context.Context, *monitorpb.CheckRequest, ...grpc.CallOption) (*monitorpb.CheckResponse, error) {
	return nil, errors.New("rpc error: code = Unavailable desc = dial unix /tmp/monitor123")
}

func TestServerErrorsAreLoggedButNotAnswered(t *testing.T) {
	var logs bytes.Buffer
	b := newBench(t, fakeSupervisor{names: []string{"sei"}, client: failingClient{}}, slog.New(slog.NewTextHandler(&logs, nil)))
	accountstest.Person(t, b.svc, "ana-lima", "monitor:sei:check")
	token := accountstest.Token(t, b.svc, "ana-lima", "monitor:sei:check")

	rec := b.call(t, http.MethodPost, "/api/monitors/sei/check", bearer(token))

	if rec.Code != http.StatusBadGateway || strings.Contains(rec.Body.String(), "monitor123") {
		t.Errorf("check = %d %s, want 502 without the RPC error", rec.Code, rec.Body.String())
	}
	if !strings.Contains(logs.String(), "monitor123") || !strings.Contains(logs.String(), "level=WARN") {
		t.Errorf("the logs miss the cause:\n%s", logs.String())
	}
}

func TestConsumersAreListedWithTheirLag(t *testing.T) {
	b := newBench(t, fakeSupervisor{}, slog.New(slog.DiscardHandler))
	service := consumers.NewService(b.pool)
	token, err := service.Add(t.Context(), "desk")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Touch(t.Context(), "desk", consumers.HashToken(token)); err != nil {
		t.Fatal(err)
	}
	storeEvents(t, b.pool, "sei", "ping 1", "ping 2")
	accountstest.Admin(t, b.svc, "paulo-santos")
	session := accountstest.Session(t, b.svc, "paulo-santos")

	rec := b.call(t, http.MethodGet, "/api/consumers", http.Header{"Cookie": {gate.CookieName + "=" + session}})

	var page struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("consumers = %d %s (%v)", rec.Code, rec.Body.String(), err)
	}
	if len(page.Items) != 1 {
		t.Fatalf("consumers = %v, want desk alone", page.Items)
	}
	// TestMain runs the package in a zone other than UTC.
	if seen, _ := page.Items[0]["seen_at"].(string); !strings.HasSuffix(seen, "Z") {
		t.Errorf("seen_at = %q, want a time in UTC", seen)
	}
	delete(page.Items[0], "seen_at")
	want := []map[string]any{{"name": "desk", "position": 0.0, "behind": 2.0, "acked_at": nil, "connected": false}}
	if !reflect.DeepEqual(page.Items, want) {
		t.Errorf("consumers = %v, want %v", page.Items, want)
	}
}

func TestNoErrorEchoesARequestValue(t *testing.T) {
	const value = "a value from the request"
	mux := http.NewServeMux()
	humaAPI := humago.New(mux, HumaConfig(slog.New(slog.DiscardHandler), "test", "test"))
	huma.Register(humaAPI, huma.Operation{OperationID: "refuse", Method: http.MethodGet, Path: "/refuse"}, func(context.Context, *struct{}) (*struct{}, error) {
		return nil, huma.Error422UnprocessableEntity("refused", &huma.ErrorDetail{Message: "not accepted", Location: "body.field", Value: value})
	})
	rec := httptest.NewRecorder()

	mux.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/refuse", nil))

	var model struct {
		Errors []map[string]any `json:"errors"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &model); err != nil || rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("refuse = %d (%v), want 422", rec.Code, err)
	}
	want := []map[string]any{{"message": "not accepted", "location": "body.field"}}
	if !reflect.DeepEqual(model.Errors, want) || strings.Contains(rec.Body.String(), value) {
		t.Errorf("the error details = %v, want %v without the value", model.Errors, want)
	}
}
