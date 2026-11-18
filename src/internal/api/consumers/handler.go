package consumers

import (
	"context"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ItzTas/monitor-hub/src/internal/consumers"
	"github.com/ItzTas/monitor-hub/src/internal/stream"
)

type Handler struct {
	service  *consumers.Service
	registry *stream.Registry
}

func (h *Handler) List(ctx context.Context, _ *struct{}) (*ListOutput, error) {
	all, err := h.service.List(ctx)
	if err != nil {
		return nil, huma.Error500InternalServerError("internal error", err)
	}
	output := &ListOutput{}
	output.Body.Items = make([]Consumer, 0, len(all))
	for _, c := range all {
		output.Body.Items = append(output.Body.Items, Consumer{
			Name:      c.Name,
			Position:  c.Position,
			Behind:    c.Behind,
			AckedAt:   c.AckedAt,
			SeenAt:    c.SeenAt,
			Connected: h.registry.Connected(c.Name),
		})
	}
	return output, nil
}
