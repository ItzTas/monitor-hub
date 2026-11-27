package feed

import (
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5/pgxpool"
)

func Configure(mux *http.ServeMux, pool *pgxpool.Pool, opts Options, tokens Tokens, log *slog.Logger) {
	repo := NewPostgres(pool)
	RegisterRoutes(mux, &Handler{service: NewService(repo, opts), repo: repo, tokens: tokens, log: log})
}

func RegisterRoutes(mux *http.ServeMux, h *Handler) {
	mux.HandleFunc("GET /feed.xml", h.Feed)
}
