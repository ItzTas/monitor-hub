package feed

import (
	"crypto/subtle"
	"log/slog"
	"net/http"
)

type Handler struct {
	service *Service
	token   string
	log     *slog.Logger
}

func (h *Handler) Feed(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(r.URL.Query().Get("token")) {
		http.NotFound(w, r)
		return
	}
	body, err := h.service.Render(r.Context(), r.URL.Query().Get("plugin"))
	if err != nil {
		h.log.Error("could not render the feed", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/rss+xml; charset=utf-8")
	_, _ = w.Write(body)
}

func (h *Handler) authorized(token string) bool {
	return h.token == "" || subtle.ConstantTimeCompare([]byte(token), []byte(h.token)) == 1
}
