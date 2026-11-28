package session

import (
	"context"
	"errors"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ItzTas/monitor-hub/src/internal/access"
	"github.com/ItzTas/monitor-hub/src/internal/accounts"
	"github.com/ItzTas/monitor-hub/src/internal/api/gate"
)

type Accounts interface {
	Login(ctx context.Context, login, password string) (accounts.Session, access.Principal, error)
	Logout(ctx context.Context, sessionID string) error
	Me(ctx context.Context, p access.Principal) (accounts.Me, error)
}

// Logins lets one sign-in through at a time. A wrong one keeps its place for a
// second before it answers, so guessing passwords is slow for everyone.
type Logins struct {
	slot    chan struct{}
	after   func(time.Duration) <-chan time.Time
	waiting func()
}

// NewLogins takes waiting for tests only: it runs when a sign-in finds the slot taken.
func NewLogins(after func(time.Duration) <-chan time.Time, waiting func()) *Logins {
	return &Logins{slot: make(chan struct{}, 1), after: after, waiting: waiting}
}

func (l *Logins) Run(ctx context.Context, login func() error) error {
	if err := l.take(ctx); err != nil {
		return err
	}
	defer func() { <-l.slot }()
	err := login()
	if !errors.Is(err, accounts.ErrWrongPassword) {
		return err
	}
	select {
	case <-l.after(time.Second):
	case <-ctx.Done():
	}
	return err
}

func (l *Logins) take(ctx context.Context) error {
	select {
	case l.slot <- struct{}{}:
		return nil
	default:
	}
	if l.waiting != nil {
		l.waiting()
	}
	select {
	case l.slot <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

type Handler struct {
	accounts Accounts
	logins   *Logins
}

func NewHandler(accounts Accounts, logins *Logins) *Handler {
	return &Handler{accounts: accounts, logins: logins}
}

func (h *Handler) Create(ctx context.Context, input *CreateInput) (*CreateOutput, error) {
	var (
		session   accounts.Session
		principal access.Principal
	)
	err := h.logins.Run(ctx, func() (err error) {
		session, principal, err = h.accounts.Login(ctx, input.Body.Login, input.Body.Password)
		return err
	})
	if errors.Is(err, accounts.ErrWrongPassword) {
		return nil, huma.ErrorWithHeaders(huma.Error401Unauthorized(err.Error()), gate.Challenge())
	}
	if err != nil {
		return nil, gate.Failure(ctx, err)
	}
	me, err := h.accounts.Me(ctx, principal)
	if err != nil {
		return nil, gate.Failure(ctx, err)
	}
	return &CreateOutput{SetCookie: gate.Cookie(input.ctx, session.ID, session.ExpiresAt), Body: me}, nil
}

func (h *Handler) Delete(ctx context.Context, input *DeleteInput) (*DeleteOutput, error) {
	if err := h.accounts.Logout(ctx, input.Session); err != nil {
		return nil, gate.Failure(ctx, err)
	}
	return &DeleteOutput{SetCookie: gate.ClearCookie(input.ctx)}, nil
}

func (h *Handler) Me(ctx context.Context, _ *struct{}) (*MeOutput, error) {
	me, err := h.accounts.Me(ctx, access.FromContext(ctx))
	if err != nil {
		return nil, gate.Failure(ctx, err)
	}
	return &MeOutput{Body: me}, nil
}
