package monitors

import (
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ItzTas/monitor-hub/src/internal/api/gate"
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
		Description: "Every configured monitor, plus any monitor the database still remembers, sorted by name. You see only the monitors you can view.",
		Tags:        []string{"monitors"},
		Metadata:    gate.Meta(gate.Identified),
		Errors:      []int{http.StatusUnauthorized, http.StatusForbidden},
	}, h.List)
	huma.Register(api, huma.Operation{
		OperationID: "check-monitor",
		Method:      http.MethodPost,
		Path:        "/api/monitors/{name}/check",
		Summary:     "Ask a monitor to check now",
		Description: "Calls the monitor's Check and answers with what it said. The monitor decides whether to start a check. You need the check permission on the monitor. Without it you get 403, even for a name no monitor has.",
		Tags:        []string{"monitors"},
		Metadata:    gate.Meta(gate.Identified),
		Errors:      []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusBadGateway, http.StatusServiceUnavailable},
	}, h.Check)
}
