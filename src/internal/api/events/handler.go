package events

import (
	"context"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ItzTas/monitor-hub/src/internal/access"
)

type Handler struct {
	service *Service
}

func (h *Handler) List(ctx context.Context, input *ListInput) (*ListOutput, error) {
	page, err := h.service.List(ctx, access.FromContext(ctx), *input)
	if err != nil {
		return nil, huma.Error500InternalServerError("internal error", err)
	}
	return &ListOutput{Body: page}, nil
}
