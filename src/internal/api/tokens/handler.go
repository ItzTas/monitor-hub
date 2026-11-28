// Package tokens serves the personal tokens and credentials of the signed-in person.
package tokens

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ItzTas/monitor-hub/src/internal/access"
	"github.com/ItzTas/monitor-hub/src/internal/accounts"
	"github.com/ItzTas/monitor-hub/src/internal/api/apitime"
	"github.com/ItzTas/monitor-hub/src/internal/api/gate"
)

type Accounts interface {
	CreateToken(ctx context.Context, owner access.Principal, req accounts.TokenRequest) (accounts.Token, string, error)
	Tokens(ctx context.Context, userID int64) ([]accounts.Token, error)
	RevokeToken(ctx context.Context, id, userID int64) error
	CreateCredential(ctx context.Context, owner access.Principal, username string, req accounts.TokenRequest) (accounts.Credential, string, error)
	Credentials(ctx context.Context, userID int64) ([]accounts.Credential, error)
	RevokeCredential(ctx context.Context, id, userID int64) error
}

func refusal(ctx context.Context, err error) error {
	var notHeld *accounts.NotHeldError
	if errors.As(err, &notHeld) {
		return huma.Error403Forbidden(err.Error())
	}
	statuses := map[error]int{
		access.ErrInvalid:             http.StatusUnprocessableEntity,
		accounts.ErrInvalidLabel:      http.StatusUnprocessableEntity,
		accounts.ErrPastExpiry:        http.StatusUnprocessableEntity,
		accounts.ErrTokenCarries:      http.StatusUnprocessableEntity,
		accounts.ErrCredentialCarries: http.StatusUnprocessableEntity,
		accounts.ErrInvalidUsername:   http.StatusUnprocessableEntity,
		accounts.ErrNoPermissions:     http.StatusUnprocessableEntity,
		accounts.ErrUsernameTaken:     http.StatusConflict,
		accounts.ErrUnknownMonitor:    http.StatusNotFound,
		accounts.ErrUnknownToken:      http.StatusNotFound,
		accounts.ErrUnknownCredential: http.StatusNotFound,
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

// listScope is the owner a list is limited to; 0, for an admin only, lists everyone's.
func listScope(ctx context.Context, everyone bool) (int64, error) {
	if !everyone {
		return access.FromContext(ctx).UserID(), nil
	}
	return 0, gate.NeedAdmin(ctx)
}

// revokeScope lets an admin revoke anyone's; anyone else finds only their own.
func revokeScope(ctx context.Context) int64 {
	p := access.FromContext(ctx)
	if p.Admin() {
		return 0
	}
	return p.UserID()
}

func request(label string, slugs []string, expiresAt *time.Time) (accounts.TokenRequest, error) {
	perms := make([]access.Permission, 0, len(slugs))
	for _, slug := range slugs {
		p, err := access.Parse(slug)
		if err != nil {
			return accounts.TokenRequest{}, err
		}
		perms = append(perms, p)
	}
	return accounts.TokenRequest{Label: label, Permissions: perms, ExpiresAt: expiresAt}, nil
}

func token(t accounts.Token) Token {
	slugs := make([]string, 0, len(t.Permissions))
	for _, p := range t.Permissions {
		slugs = append(slugs, p.String())
	}
	return Token{
		ID:          t.ID,
		Owner:       t.Owner,
		Label:       t.Label,
		Permissions: slugs,
		CreatedAt:   t.CreatedAt.UTC(),
		ExpiresAt:   apitime.UTC(t.ExpiresAt),
		LastUsedAt:  apitime.UTC(t.LastUsedAt),
	}
}

func credential(c accounts.Credential) Credential {
	return Credential{Token: token(c.Token), Username: c.Username}
}

func (h *Handler) ListTokens(ctx context.Context, input *ListInput) (*ListTokensOutput, error) {
	owner, err := listScope(ctx, input.Everyone)
	if err != nil {
		return nil, err
	}
	all, err := h.accounts.Tokens(ctx, owner)
	if err != nil {
		return nil, refusal(ctx, err)
	}
	output := &ListTokensOutput{}
	output.Body.Items = make([]Token, 0, len(all))
	for _, t := range all {
		output.Body.Items = append(output.Body.Items, token(t))
	}
	return output, nil
}

func (h *Handler) CreateToken(ctx context.Context, input *CreateTokenInput) (*CreateTokenOutput, error) {
	req, err := request(input.Body.Label, input.Body.Permissions, input.Body.ExpiresAt)
	if err != nil {
		return nil, refusal(ctx, err)
	}
	created, secret, err := h.accounts.CreateToken(ctx, access.FromContext(ctx), req)
	if err != nil {
		return nil, refusal(ctx, err)
	}
	return &CreateTokenOutput{Body: CreatedToken{Token: token(created), Secret: secret}}, nil
}

func (h *Handler) RevokeToken(ctx context.Context, input *RevokeInput) (*struct{}, error) {
	if err := h.accounts.RevokeToken(ctx, input.ID, revokeScope(ctx)); err != nil {
		return nil, refusal(ctx, err)
	}
	return nil, nil
}

func (h *Handler) ListCredentials(ctx context.Context, input *ListInput) (*ListCredentialsOutput, error) {
	owner, err := listScope(ctx, input.Everyone)
	if err != nil {
		return nil, err
	}
	all, err := h.accounts.Credentials(ctx, owner)
	if err != nil {
		return nil, refusal(ctx, err)
	}
	output := &ListCredentialsOutput{}
	output.Body.Items = make([]Credential, 0, len(all))
	for _, c := range all {
		output.Body.Items = append(output.Body.Items, credential(c))
	}
	return output, nil
}

func (h *Handler) CreateCredential(ctx context.Context, input *CreateCredentialInput) (*CreateCredentialOutput, error) {
	req, err := request(input.Body.Label, input.Body.Permissions, input.Body.ExpiresAt)
	if err != nil {
		return nil, refusal(ctx, err)
	}
	created, secret, err := h.accounts.CreateCredential(ctx, access.FromContext(ctx), input.Body.Username, req)
	if err != nil {
		return nil, refusal(ctx, err)
	}
	return &CreateCredentialOutput{Body: CreatedCredential{Credential: credential(created), Password: secret}}, nil
}

func (h *Handler) RevokeCredential(ctx context.Context, input *RevokeInput) (*struct{}, error) {
	if err := h.accounts.RevokeCredential(ctx, input.ID, revokeScope(ctx)); err != nil {
		return nil, refusal(ctx, err)
	}
	return nil, nil
}
