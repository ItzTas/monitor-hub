package consumers

import (
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ItzTas/monitor-hub/src/internal/api/gate"
	"github.com/ItzTas/monitor-hub/src/internal/consumers"
	"github.com/ItzTas/monitor-hub/src/internal/stream"
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
		Description: "Every consumer of the notification stream, sorted by name: how far behind it is and whether it holds a stream open now. Only an admin can list consumers.",
		Tags:        []string{"consumers"},
		Metadata:    gate.Meta(gate.Session),
		Errors:      []int{http.StatusUnauthorized, http.StatusForbidden},
	}, h.List)
}
