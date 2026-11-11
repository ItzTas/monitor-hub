package api

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"google.golang.org/grpc"

	"github.com/ItzTas/monitor-hub/internal/core/coretest"
	"github.com/ItzTas/monitor-hub/sdk/pluginpb"
)

const token = "api-token-0123456789"

type noPlugins struct{}

func (noPlugins) Names() []string {
	return nil
}

func (noPlugins) Client(string) (pluginpb.PluginClient, bool) {
	return nil, false
}

func TestEveryRouteRequiresTheToken(t *testing.T) {
	mux := http.NewServeMux()
	api := Register(mux, Deps{Pool: coretest.Pool(t), Plugins: noPlugins{}, Token: token, Log: slog.New(slog.DiscardHandler)})

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
	Register(mux, Deps{Pool: coretest.Pool(t), Plugins: noPlugins{}, Token: token, Log: slog.New(slog.DiscardHandler)})

	for _, target := range []string{"/api/plugins", "/api/events"} {
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
	pluginpb.PluginClient
}

func (failingClient) Check(context.Context, *pluginpb.CheckRequest, ...grpc.CallOption) (*pluginpb.CheckResponse, error) {
	return nil, errors.New("rpc error: code = Unavailable desc = dial unix /tmp/plugin123")
}

type onePlugin struct{}

func (onePlugin) Names() []string {
	return []string{"sei"}
}

func (onePlugin) Client(string) (pluginpb.PluginClient, bool) {
	return failingClient{}, true
}

func TestServerErrorsAreLoggedButNotAnswered(t *testing.T) {
	var logs bytes.Buffer
	mux := http.NewServeMux()
	Register(mux, Deps{Pool: coretest.Pool(t), Plugins: onePlugin{}, Token: token, Log: slog.New(slog.NewTextHandler(&logs, nil))})
	request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/plugins/sei/check", nil)
	request.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()

	mux.ServeHTTP(rec, request)

	if rec.Code != http.StatusBadGateway || strings.Contains(rec.Body.String(), "plugin123") {
		t.Errorf("check = %d %s, want 502 without the RPC error", rec.Code, rec.Body.String())
	}
	if !strings.Contains(logs.String(), "plugin123") || !strings.Contains(logs.String(), "level=WARN") {
		t.Errorf("the logs miss the cause:\n%s", logs.String())
	}
}
