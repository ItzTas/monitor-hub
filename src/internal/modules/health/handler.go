package health

import (
	"encoding/json"
	"log/slog"
	"net/http"
)

type Handler struct {
	service *Service
	log     *slog.Logger
}

func (h *Handler) Health(w http.ResponseWriter, r *http.Request) {
	err := h.service.Check(r.Context())
	if err != nil {
		h.log.Error("the health check could not reach the database", "error", err)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(map[bool]int{true: http.StatusOK, false: http.StatusServiceUnavailable}[err == nil])
	_ = json.NewEncoder(w).Encode(Status{OK: err == nil})
}
