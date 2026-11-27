// Package people lets an admin manage people and the permissions given to them.
package people

import (
	"context"
	"errors"
	"net/http"
	"slices"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ItzTas/monitor-hub/src/internal/access"
	"github.com/ItzTas/monitor-hub/src/internal/accounts"
	"github.com/ItzTas/monitor-hub/src/internal/api/gate"
)

type Accounts interface {
	ListUsers(ctx context.Context) ([]accounts.User, error)
	CreateUser(ctx context.Context, login, password string, admin bool) error
	UpdateUser(ctx context.Context, login string, change accounts.UserChange) error
	SetPassword(ctx context.Context, login, password string) error
	SetPermissions(ctx context.Context, login string, perms []access.Permission) error
	DeleteUser(ctx context.Context, login string) error
}

func refusal(ctx context.Context, err error) error {
	statuses := map[error]int{
		accounts.ErrInvalidLogin:   http.StatusUnprocessableEntity,
		accounts.ErrWeakPassword:   http.StatusUnprocessableEntity,
		access.ErrInvalid:          http.StatusUnprocessableEntity,
		accounts.ErrUserExists:     http.StatusConflict,
		accounts.ErrLastAdmin:      http.StatusConflict,
		accounts.ErrUnknownUser:    http.StatusNotFound,
		accounts.ErrUnknownMonitor: http.StatusNotFound,
	}
	for known, status := range statuses {
		if errors.Is(err, known) {
			return huma.NewError(status, err.Error())
		}
	}
	return gate.Failure(ctx, err)
}

type Handler struct {
	accounts Accounts
}

func NewHandler(accounts Accounts) *Handler {
	return &Handler{accounts: accounts}
}

func user(u accounts.User) User {
	slugs := make([]string, 0, len(u.Permissions))
	for _, p := range u.Permissions {
		slugs = append(slugs, p.String())
	}
	// The database orders by monitor name, so even under C it puts sei before sei-b; as slugs, sei-b comes first.
	slices.Sort(slugs)
	return User{Login: u.Login, Admin: u.Admin, Disabled: u.Disabled, CreatedAt: u.CreatedAt, Permissions: slugs}
}

// find takes login out of the list: there are few people, so it needs no query of its own.
func (h *Handler) find(ctx context.Context, login string) (*UserOutput, error) {
	all, err := h.accounts.ListUsers(ctx)
	if err != nil {
		return nil, refusal(ctx, err)
	}
	i := slices.IndexFunc(all, func(u accounts.User) bool { return u.Login == login })
	if i < 0 {
		return nil, refusal(ctx, accounts.ErrUnknownUser)
	}
	return &UserOutput{Body: user(all[i])}, nil
}

func (h *Handler) List(ctx context.Context, _ *struct{}) (*ListOutput, error) {
	if err := gate.NeedAdmin(ctx); err != nil {
		return nil, err
	}
	all, err := h.accounts.ListUsers(ctx)
	if err != nil {
		return nil, refusal(ctx, err)
	}
	output := &ListOutput{}
	output.Body.Items = make([]User, 0, len(all))
	for _, u := range all {
		output.Body.Items = append(output.Body.Items, user(u))
	}
	return output, nil
}

func (h *Handler) Create(ctx context.Context, input *CreateInput) (*UserOutput, error) {
	if err := gate.NeedAdmin(ctx); err != nil {
		return nil, err
	}
	if err := h.accounts.CreateUser(ctx, input.Body.Login, input.Body.Password, input.Body.Admin); err != nil {
		return nil, refusal(ctx, err)
	}
	return h.find(ctx, input.Body.Login)
}

func (h *Handler) Update(ctx context.Context, input *UpdateInput) (*UserOutput, error) {
	if err := gate.NeedAdmin(ctx); err != nil {
		return nil, err
	}
	change := accounts.UserChange{Admin: input.Body.Admin, Disabled: input.Body.Disabled}
	if err := h.accounts.UpdateUser(ctx, input.Login, change); err != nil {
		return nil, refusal(ctx, err)
	}
	return h.find(ctx, input.Login)
}

func (h *Handler) SetPassword(ctx context.Context, input *PasswordInput) (*struct{}, error) {
	if err := gate.NeedAdmin(ctx); err != nil {
		return nil, err
	}
	if err := h.accounts.SetPassword(ctx, input.Login, input.Body.Password); err != nil {
		return nil, refusal(ctx, err)
	}
	return nil, nil
}

func (h *Handler) SetPermissions(ctx context.Context, input *PermissionsInput) (*UserOutput, error) {
	if err := gate.NeedAdmin(ctx); err != nil {
		return nil, err
	}
	perms := make([]access.Permission, 0, len(input.Body.Permissions))
	for _, slug := range input.Body.Permissions {
		p, err := access.Parse(slug)
		if err != nil {
			return nil, refusal(ctx, err)
		}
		perms = append(perms, p)
	}
	if err := h.accounts.SetPermissions(ctx, input.Login, perms); err != nil {
		return nil, refusal(ctx, err)
	}
	return h.find(ctx, input.Login)
}

func (h *Handler) Delete(ctx context.Context, input *DeleteInput) (*struct{}, error) {
	if err := gate.NeedAdmin(ctx); err != nil {
		return nil, err
	}
	if err := h.accounts.DeleteUser(ctx, input.Login); err != nil {
		return nil, refusal(ctx, err)
	}
	return nil, nil
}
