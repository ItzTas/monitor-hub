package events

import (
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"github.com/jackc/pgx/v5/pgxpool"
)

func Configure(api huma.API, pool *pgxpool.Pool) {
	RegisterRoutes(api, &Handler{service: NewService(NewPostgres(pool))})
}

func RegisterRoutes(api huma.API, h *Handler) {
	huma.Register(api, huma.Operation{
		OperationID: "list-events",
		Method:      http.MethodGet,
		Path:        "/api/events",
		Summary:     "List the events",
		Description: "The events every plugin reported, newest first. Page with before and limit.",
		Tags:        []string{"events"},
		Errors:      []int{http.StatusUnauthorized, http.StatusUnprocessableEntity},
	}, h.List)
}
