package consumers

import (
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ItzTas/monitor-hub/internal/consumers"
	"github.com/ItzTas/monitor-hub/internal/stream"
)

func Configure(api huma.API, service *consumers.Service, registry *stream.Registry) {
	RegisterRoutes(api, &Handler{service: service, registry: registry})
}

func RegisterRoutes(api huma.API, h *Handler) {
	huma.Register(api, huma.Operation{
		OperationID: "list-consumers",
		Method:      http.MethodGet,
		Path:        "/api/consumers",
		Summary:     "List the consumers",
		Description: "Every consumer of the notification stream, sorted by name: how far behind it is and whether it holds a stream open now.",
		Tags:        []string{"consumers"},
		Errors:      []int{http.StatusUnauthorized},
	}, h.List)
}
