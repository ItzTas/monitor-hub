package monitorsettings

import (
	"context"
	"errors"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ItzTas/monitor-hub/src/internal/access"
	"github.com/ItzTas/monitor-hub/src/internal/api/apitime"
	"github.com/ItzTas/monitor-hub/src/internal/api/gate"
	"github.com/ItzTas/monitor-hub/src/internal/api/monitors"
	"github.com/ItzTas/monitor-hub/src/internal/monitorconfig"
)

type Service interface {
	Read(ctx context.Context, name string) (monitorconfig.View, error)
	Save(ctx context.Context, name string, change monitorconfig.Change) (monitorconfig.SaveResult, error)
	Progress(ctx context.Context, name string) (monitorconfig.Progress, error)
}

// Handler refuses before it looks the name up, so a refusal never tells whether a monitor exists.
type Handler struct {
	service Service
}

func NewHandler(service Service) *Handler {
	return &Handler{service: service}
}

func (h *Handler) Read(ctx context.Context, input *NameInput) (*ReadOutput, error) {
	if !access.FromContext(ctx).Can(access.Configure, input.Name) {
		return nil, gate.Refuse(ctx)
	}
	view, err := h.service.Read(ctx, input.Name)
	if err != nil {
		return nil, failure(ctx, err)
	}
	return &ReadOutput{Body: settingsOf(view)}, nil
}

func (h *Handler) Save(ctx context.Context, input *SaveInput) (*SaveOutput, error) {
	if !access.FromContext(ctx).Can(access.Configure, input.Name) {
		return nil, gate.Refuse(ctx)
	}
	result, err := h.service.Save(ctx, input.Name, changeOf(input.Body))
	if err != nil {
		return nil, failure(ctx, err)
	}
	output := &SaveOutput{Status: http.StatusOK, Body: SaveResult{Changed: orEmpty(result.Changed), Restarted: result.Restarted, Request: result.Request}}
	if result.Restarted {
		output.Status = http.StatusAccepted
	}
	return output, nil
}

func (h *Handler) Progress(ctx context.Context, input *NameInput) (*ProgressOutput, error) {
	if !access.FromContext(ctx).Can(access.Configure, input.Name) {
		return nil, gate.Refuse(ctx)
	}
	p, err := h.service.Progress(ctx, input.Name)
	if err != nil {
		return nil, failure(ctx, err)
	}
	body := Progress{
		Request:     p.Request,
		State:       string(p.State),
		RequestedAt: apitime.UTC(p.RequestedAt),
		Phase:       string(p.Phase),
		Message:     p.Message,
		Reason:      p.Reason,
	}
	if p.State == monitorconfig.ProgressUp {
		body.MonitorState = monitors.StateName(p.MonitorState)
	}
	return &ProgressOutput{Body: body}, nil
}

func failure(ctx context.Context, err error) error {
	// A client that left also cancels the database calls; that is no database failure.
	if ctx.Err() != nil {
		return gate.Failure(ctx, err)
	}
	var problems *monitorconfig.ProblemsError
	if errors.As(err, &problems) {
		return &ProblemsResponse{Problems: problemsOf(problems.Problems)}
	}
	var validator *monitorconfig.ValidatorError
	if errors.As(err, &validator) {
		return huma.Error502BadGateway(validator.Error())
	}
	known := []struct {
		err    error
		status int
		detail string
	}{
		{monitorconfig.ErrUnknown, http.StatusNotFound, "no monitor has this name"},
		{monitorconfig.ErrBusy, http.StatusConflict, "another save of this monitor is in progress"},
		{monitorconfig.ErrRestarting, http.StatusConflict, "the monitor is still restarting after the last save; read the progress and try again once it settles"},
		{monitorconfig.ErrStale, http.StatusConflict, "the saved configuration changed since this revision; read it again"},
		{monitorconfig.ErrDatabase, http.StatusServiceUnavailable, "the hub's database cannot be reached"},
	}
	for _, k := range known {
		if !errors.Is(err, k.err) {
			continue
		}
		// Only a 5xx carries the cause: the API logs it and leaves it out of the answer.
		if k.status >= http.StatusInternalServerError {
			return huma.NewError(k.status, k.detail, err)
		}
		return huma.NewError(k.status, k.detail)
	}
	return gate.Failure(ctx, err)
}

func changeOf(body SaveBody) monitorconfig.Change {
	change := monitorconfig.Change{
		Document:      body.Document,
		Secrets:       body.Secrets,
		DeleteIgnored: body.DeleteIgnored,
		Revision:      body.Revision,
	}
	if body.Variables != nil {
		change.Variables = monitorconfig.VariableChange{Set: body.Variables.Set, Unset: body.Variables.Unset, Secret: body.Variables.Secret}
	}
	return change
}

func orEmpty[T any](items []T) []T {
	if items == nil {
		return []T{}
	}
	return items
}
