package events

import (
	"context"

	"github.com/danielgtaylor/huma/v2"
)

type Handler struct {
	service *Service
}

func (h *Handler) List(ctx context.Context, input *ListInput) (*ListOutput, error) {
	page, err := h.service.List(ctx, *input)
	if err != nil {
		return nil, huma.Error500InternalServerError("internal error", err)
	}
	return &ListOutput{Body: page}, nil
}
