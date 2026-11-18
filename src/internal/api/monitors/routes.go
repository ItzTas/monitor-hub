package monitors

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
		OperationID: "list-monitors",
		Method:      http.MethodGet,
		Path:        "/api/monitors",
		Summary:     "List the monitors",
		Description: "Every configured monitor, plus any monitor the database still remembers, sorted by name.",
		Tags:        []string{"monitors"},
		Errors:      []int{http.StatusUnauthorized},
	}, h.List)
	huma.Register(api, huma.Operation{
		OperationID: "check-monitor",
		Method:      http.MethodPost,
		Path:        "/api/monitors/{name}/check",
		Summary:     "Ask a monitor to check now",
		Description: "Calls the monitor's Check and answers with what it said. The monitor decides whether to start a check.",
		Tags:        []string{"monitors"},
		Errors:      []int{http.StatusUnauthorized, http.StatusNotFound, http.StatusBadGateway, http.StatusServiceUnavailable},
	}, h.Check)
}
