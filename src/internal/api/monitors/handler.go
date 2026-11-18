package monitors

import (
	"context"
	"errors"

	"github.com/danielgtaylor/huma/v2"
)

type Handler struct {
	service *Service
}

func (h *Handler) List(ctx context.Context, _ *struct{}) (*ListOutput, error) {
	monitors, err := h.service.List(ctx)
	if err != nil {
		return nil, huma.Error500InternalServerError("internal error", err)
	}
	output := &ListOutput{}
	output.Body.Items = monitors
	return output, nil
}

func (h *Handler) Check(ctx context.Context, input *CheckInput) (*CheckOutput, error) {
	result, err := h.service.Check(ctx, input.Name)
	if err == nil {
		return &CheckOutput{Body: result}, nil
	}
	if errors.Is(err, ErrUnknown) {
		return nil, huma.Error404NotFound(err.Error())
	}
	if errors.Is(err, ErrNotRunning) {
		return nil, huma.Error503ServiceUnavailable(err.Error())
	}
	return nil, huma.Error502BadGateway("the monitor did not answer the check", err)
}
