package api

import (
	"crypto/subtle"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
	"github.com/jackc/pgx/v5/pgxpool"

	consumerlist "github.com/ItzTas/monitor-hub/src/internal/api/consumers"
	"github.com/ItzTas/monitor-hub/src/internal/api/events"
	"github.com/ItzTas/monitor-hub/src/internal/api/plugins"
	"github.com/ItzTas/monitor-hub/src/internal/consumers"
	"github.com/ItzTas/monitor-hub/src/internal/stream"
)

const (
	version        = "1.0.0"
	securityScheme = "bearer"
)

type Deps struct {
	Pool      *pgxpool.Pool
	Plugins   plugins.Supervisor
	Token     string
	Log       *slog.Logger
	Consumers *consumers.Service
	Registry  *stream.Registry
}

func Register(mux *http.ServeMux, deps Deps) huma.API {
	api := humago.New(mux, HumaConfig(deps.Log,
		"The plugins monitor-hub runs, the events they reported and the consumers that read them. Every route needs the header Authorization: Bearer <API_TOKEN>.",
		"The API_TOKEN of the monitor-hub configuration.",
	))
	api.UseMiddleware(bearerAuth(api, deps.Token))
	plugins.Configure(api, deps.Pool, deps.Plugins)
	events.Configure(api, deps.Pool)
	consumerlist.Configure(api, deps.Consumers, deps.Registry)
	return api
}

func HumaConfig(log *slog.Logger, description, credential string) huma.Config {
	cfg := huma.DefaultConfig("monitor-hub API", version)
	cfg.OpenAPIPath, cfg.DocsPath, cfg.SchemasPath = "", "", ""
	cfg.CreateHooks = nil
	cfg.Info.Description = description
	cfg.Components.SecuritySchemes = map[string]*huma.SecurityScheme{
		securityScheme: {Type: "http", Scheme: "bearer", Description: credential},
	}
	cfg.Security = []map[string][]string{{securityScheme: {}}}
	cfg.Transformers = append(cfg.Transformers, serverErrors(log))
	return cfg
}

func bearerAuth(api huma.API, token string) func(huma.Context, func(huma.Context)) {
	expected := []byte("Bearer " + token)
	return func(ctx huma.Context, next func(huma.Context)) {
		if token == "" || subtle.ConstantTimeCompare([]byte(ctx.Header("Authorization")), expected) != 1 {
			ctx.SetHeader("WWW-Authenticate", "Bearer")
			_ = huma.WriteErr(api, ctx, http.StatusUnauthorized, "missing or invalid API token")
			return
		}
		next(ctx)
	}
}

func serverErrors(log *slog.Logger) huma.Transformer {
	return func(ctx huma.Context, statusCode string, v any) (any, error) {
		code, err := strconv.Atoi(statusCode)
		if err != nil || code < http.StatusInternalServerError {
			return v, nil
		}
		model, ok := v.(*huma.ErrorModel)
		if !ok {
			return v, nil
		}
		attrs := []any{"status", code, "method", ctx.Method(), "route", ctx.Operation().Path}
		for _, cause := range model.Errors {
			attrs = append(attrs, "cause", cause.Message)
		}
		level := slog.LevelWarn
		if code == http.StatusInternalServerError {
			level = slog.LevelError
		}
		log.Log(ctx.Context(), level, "API request failed", attrs...)
		clean := *model
		clean.Errors = nil
		return &clean, nil
	}
}
