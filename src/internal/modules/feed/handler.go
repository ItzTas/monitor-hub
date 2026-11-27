package feed

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/ItzTas/monitor-hub/src/internal/access"
	"github.com/ItzTas/monitor-hub/src/internal/accounts"
)

type Tokens interface {
	ByToken(ctx context.Context, secret string) (access.Principal, error)
}

type Handler struct {
	service *Service
	repo    Repository
	tokens  Tokens
	log     *slog.Logger
}

// Feed reads the personal token from ?token=, since feed readers cannot send a header.
func (h *Handler) Feed(w http.ResponseWriter, r *http.Request) {
	p, err := h.tokens.ByToken(r.Context(), r.URL.Query().Get("token"))
	if errors.Is(err, accounts.ErrUnauthenticated) {
		http.Error(w, "missing, invalid or expired token", http.StatusUnauthorized)
		return
	}
	if err != nil {
		h.fail(w, "could not check the feed token", err)
		return
	}
	names, err := h.repo.MonitorNames(r.Context())
	if err != nil {
		h.fail(w, "could not list the monitors for the feed", err)
		return
	}
	body, err := h.service.Render(r.Context(), r.URL.Query().Get("monitor"), access.Visible(p, names))
	if err != nil {
		h.fail(w, "could not render the feed", err)
		return
	}
	w.Header().Set("Content-Type", "application/rss+xml; charset=utf-8")
	_, _ = w.Write(body)
}

func (h *Handler) fail(w http.ResponseWriter, message string, err error) {
	h.log.Error(message, "error", err)
	http.Error(w, "internal error", http.StatusInternalServerError)
}
