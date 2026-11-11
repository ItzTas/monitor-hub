package plugins

import (
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"github.com/jackc/pgx/v5/pgxpool"
)

func Configure(api huma.API, pool *pgxpool.Pool, supervisor Supervisor) {
	RegisterRoutes(api, &Handler{service: NewService(NewPostgres(pool), supervisor)})
}

func RegisterRoutes(api huma.API, h *Handler) {
	huma.Register(api, huma.Operation{
		OperationID: "list-plugins",
		Method:      http.MethodGet,
		Path:        "/api/plugins",
		Summary:     "List the plugins",
		Description: "Every configured plugin, plus any plugin the database still remembers, sorted by name.",
		Tags:        []string{"plugins"},
		Errors:      []int{http.StatusUnauthorized},
	}, h.List)
	huma.Register(api, huma.Operation{
		OperationID: "check-plugin",
		Method:      http.MethodPost,
		Path:        "/api/plugins/{name}/check",
		Summary:     "Ask a plugin to check now",
		Description: "Calls the plugin's Check and answers with what it said. The plugin decides whether to start a check.",
		Tags:        []string{"plugins"},
		Errors:      []int{http.StatusUnauthorized, http.StatusNotFound, http.StatusBadGateway, http.StatusServiceUnavailable},
	}, h.Check)
}
