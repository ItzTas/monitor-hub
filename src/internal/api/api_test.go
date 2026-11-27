package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
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
	"github.com/ItzTas/monitor-hub/src/internal/api/gate"
	"github.com/ItzTas/monitor-hub/src/internal/api/monitors"
	"github.com/ItzTas/monitor-hub/src/internal/consumers"
	"github.com/ItzTas/monitor-hub/src/internal/core/coretest"
	"github.com/ItzTas/monitor-hub/src/internal/ingest"
	"github.com/ItzTas/monitor-hub/src/internal/stream"
)

const token = "api-token-0123456789"

type noMonitors struct{}

func (noMonitors) Names() []string {
	return nil
}

func (noMonitors) Client(string) (monitorpb.MonitorClient, bool) {
	return nil, false
}

func deps(pool *pgxpool.Pool, supervisor monitors.Supervisor, log *slog.Logger) Deps {
	return Deps{Pool: pool, Monitors: supervisor, Token: token, Log: log, Consumers: consumers.NewService(pool), Registry: stream.NewRegistry()}
}

func TestEveryRouteRequiresTheToken(t *testing.T) {
	mux := http.NewServeMux()
	api := Register(mux, deps(coretest.Pool(t), noMonitors{}, slog.New(slog.DiscardHandler)))

	routes := 0
	for path, item := range api.OpenAPI().Paths {
		for method, operation := range map[string]*huma.Operation{http.MethodGet: item.Get, http.MethodPost: item.Post, http.MethodPut: item.Put, http.MethodPatch: item.Patch, http.MethodDelete: item.Delete} {
			if operation == nil {
				continue
			}
			if !strings.HasPrefix(path, "/api/") {
				t.Errorf("%s %s is outside /api/", method, path)
			}
			target := strings.NewReplacer("{name}", "sei").Replace(path)
			for name, header := range map[string]string{"no token": "", "wrong token": "Bearer api-token-0123456780", "no scheme": token, "basic": "Basic " + token} {
				routes++
				request := httptest.NewRequestWithContext(t.Context(), method, target, nil)
				if header != "" {
					request.Header.Set("Authorization", header)
				}
				rec := httptest.NewRecorder()
				mux.ServeHTTP(rec, request)
				if rec.Code != http.StatusUnauthorized || rec.Header().Get("WWW-Authenticate") != "Bearer" {
					t.Errorf("%s %s with %s = %d, want 401 with WWW-Authenticate: Bearer", method, target, name, rec.Code)
				}
			}
		}
	}
	if routes == 0 {
		t.Fatal("no route registered")
	}
}

func TestTheRightTokenIsAccepted(t *testing.T) {
	mux := http.NewServeMux()
	Register(mux, deps(coretest.Pool(t), noMonitors{}, slog.New(slog.DiscardHandler)))

	for _, target := range []string{"/api/monitors", "/api/events"} {
		request := httptest.NewRequestWithContext(t.Context(), http.MethodGet, target, nil)
		request.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, request)
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s = %d: %s", target, rec.Code, rec.Body.String())
		}
	}
}

type failingClient struct {
	monitorpb.MonitorClient
}

func (failingClient) Check(context.Context, *monitorpb.CheckRequest, ...grpc.CallOption) (*monitorpb.CheckResponse, error) {
	return nil, errors.New("rpc error: code = Unavailable desc = dial unix /tmp/monitor123")
}

type oneMonitor struct{}

func (oneMonitor) Names() []string {
	return []string{"sei"}
}

func (oneMonitor) Client(string) (monitorpb.MonitorClient, bool) {
	return failingClient{}, true
}

func TestServerErrorsAreLoggedButNotAnswered(t *testing.T) {
	var logs bytes.Buffer
	mux := http.NewServeMux()
	Register(mux, deps(coretest.Pool(t), oneMonitor{}, slog.New(slog.NewTextHandler(&logs, nil))))
	request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/monitors/sei/check", nil)
	request.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()

	mux.ServeHTTP(rec, request)

	if rec.Code != http.StatusBadGateway || strings.Contains(rec.Body.String(), "monitor123") {
		t.Errorf("check = %d %s, want 502 without the RPC error", rec.Code, rec.Body.String())
	}
	if !strings.Contains(logs.String(), "monitor123") || !strings.Contains(logs.String(), "level=WARN") {
		t.Errorf("the logs miss the cause:\n%s", logs.String())
	}
}

func TestConsumersAreListedWithTheirLag(t *testing.T) {
	pool := coretest.Pool(t)
	mux := http.NewServeMux()
	Register(mux, deps(pool, noMonitors{}, slog.New(slog.DiscardHandler)))
	if _, err := consumers.NewService(pool).Add(t.Context(), "desk"); err != nil {
		t.Fatal(err)
	}
	repo := ingest.NewPostgres(pool)
	if err := repo.SaveDescription(t.Context(), &monitorpb.DescribeResponse{Name: "sei", DisplayName: "SEI", Version: "1"}); err != nil {
		t.Fatal(err)
	}
	events := []*monitorpb.Event{
		{Seq: 1, Id: "e1", Kind: "ping", OccurredAt: timestamppb.Now(), Title: "ping 1"},
		{Seq: 2, Id: "e2", Kind: "ping", OccurredAt: timestamppb.Now(), Title: "ping 2"},
	}
	if _, err := repo.StoreEvents(t.Context(), ingest.Batch{Monitor: "sei", Events: events}); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/consumers", nil)
	request.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()

	mux.ServeHTTP(rec, request)

	var page struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("consumers = %d %s (%v)", rec.Code, rec.Body.String(), err)
	}
	want := []map[string]any{{"name": "desk", "position": 0.0, "behind": 2.0, "acked_at": nil, "seen_at": nil, "connected": false}}
	if !reflect.DeepEqual(page.Items, want) {
		t.Errorf("consumers = %v, want %v", page.Items, want)
	}
}

func TestEveryRouteDeclaresAGate(t *testing.T) {
	api := RegisterAccess(http.NewServeMux(), AccessDeps{
		Accounts: accounts.NewService(coretest.Pool(t), time.Now), Log: slog.New(slog.DiscardHandler), After: time.After,
	})
	levels := map[any]bool{gate.Anyone: true, gate.Identified: true, gate.Session: true}

	routes := 0
	for path, item := range api.OpenAPI().Paths {
		for method, operation := range map[string]*huma.Operation{http.MethodGet: item.Get, http.MethodPost: item.Post, http.MethodPut: item.Put, http.MethodPatch: item.Patch, http.MethodDelete: item.Delete} {
			if operation == nil {
				continue
			}
			routes++
			level := operation.Metadata["gate"]
			if !levels[level] {
				t.Errorf("%s %s declares the gate %v, want anyone, identified or session", method, path, level)
			}
			if level == gate.Anyone && operation.OperationID != "create-session" {
				t.Errorf("%s %s is open to anyone; only create-session is", method, path)
			}
		}
	}
	if routes == 0 {
		t.Fatal("no route registered")
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
