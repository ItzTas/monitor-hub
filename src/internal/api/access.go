package api

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"

	"github.com/ItzTas/monitor-hub/src/internal/accounts"
	"github.com/ItzTas/monitor-hub/src/internal/api/gate"
	"github.com/ItzTas/monitor-hub/src/internal/api/session"
)

type AccessDeps struct {
	Accounts  *accounts.Service
	Log       *slog.Logger
	PublicURL string
	After     func(time.Duration) <-chan time.Time
}

func RegisterAccess(mux *http.ServeMux, deps AccessDeps) huma.API {
	api := humago.New(mux, HumaConfig(deps.Log,
		"Sign-in, personal tokens and credentials, and people with their permissions. Most routes need the session cookie from POST /api/session.",
		"A personal token (mht_), shown once when it is created.",
	))
	api.UseMiddleware(gate.Middleware(api, deps.Accounts, deps.PublicURL))
	session.Configure(api, deps.Accounts, deps.After)
	return api
}
