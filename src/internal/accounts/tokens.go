package accounts

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"golang.org/x/sync/errgroup"

	"github.com/ItzTas/monitor-hub/src/internal/access"
	"github.com/ItzTas/monitor-hub/src/internal/db"
)

const tokenPrefix = "mht_"

type TokenRequest struct {
	Label       string
	Permissions []access.Permission
	ExpiresAt   *time.Time
}

type Token struct {
	ID          int64
	Owner       string
	Label       string
	Permissions []access.Permission
	CreatedAt   time.Time
	ExpiresAt   *time.Time
	LastUsedAt  *time.Time
}

type NotHeldError struct{ Permission access.Permission }

func (e *NotHeldError) Error() string {
	return "you do not have " + e.Permission.String()
}

var (
	ErrInvalidLabel  = errors.New("use a label of 1 to 64 characters without control characters")
	ErrPastExpiry    = errors.New("expires_at must be in the future")
	ErrTokenCarries  = errors.New("a token carries only view, check and hook of monitors")
	ErrNoPermissions = errors.New("choose at least one permission")
	ErrUnknownToken  = errors.New("no token has this id")
)

func checkLabel(label string) error {
	if label == "" || utf8.RuneCountInString(label) > 64 || strings.ContainsFunc(label, unicode.IsControl) {
		return ErrInvalidLabel
	}
	return nil
}

// checkRequest runs the checks after the label, in order. Covers is false for
// anything but a session, so only a signed-in person creates tokens.
func (s *Service) checkRequest(owner access.Principal, req TokenRequest, carries []access.Action, refused error) error {
	if len(req.Permissions) == 0 {
		return ErrNoPermissions
	}
	for _, p := range req.Permissions {
		if !slices.Contains(carries, p.Action) {
			return refused
		}
	}
	if req.ExpiresAt != nil && !req.ExpiresAt.After(s.now()) {
		return ErrPastExpiry
	}
	for _, p := range req.Permissions {
		if !owner.Covers(p) {
			return &NotHeldError{Permission: p}
		}
	}
	return nil
}

func (s *Service) CreateToken(ctx context.Context, owner access.Principal, req TokenRequest) (Token, string, error) {
	if err := checkLabel(req.Label); err != nil {
		return Token{}, "", err
	}
	if err := s.checkRequest(owner, req, []access.Action{access.View, access.Check, access.Hook}, ErrTokenCarries); err != nil {
		return Token{}, "", err
	}
	secret, hash, err := NewSecret(tokenPrefix)
	if err != nil {
		return Token{}, "", err
	}
	token := newToken(owner, req, s.now())
	err = s.inTx(ctx, func(q *db.Queries) error {
		id, err := q.InsertToken(ctx, db.InsertTokenParams{
			UserID: owner.UserID(), Label: req.Label, Hash: hash, CreatedAt: token.CreatedAt, ExpiresAt: req.ExpiresAt,
		})
		if err != nil {
			return err
		}
		token.ID = id
		for _, p := range req.Permissions {
			err := q.InsertTokenGrant(ctx, db.InsertTokenGrantParams{TokenID: token.ID, Monitor: grantMonitor(p), Action: string(p.Action)})
			if err != nil {
				return unknownMonitor(err)
			}
		}
		return nil
	})
	if err != nil {
		return Token{}, "", err
	}
	return token, secret, nil
}

func newToken(owner access.Principal, req TokenRequest, now time.Time) Token {
	return Token{
		Owner:       owner.Login(),
		Label:       req.Label,
		Permissions: slices.Clone(req.Permissions),
		CreatedAt:   now,
		ExpiresAt:   req.ExpiresAt,
	}
}

// Tokens lists the tokens of userID, or of everyone when userID is 0.
func (s *Service) Tokens(ctx context.Context, userID int64) ([]Token, error) {
	rows, err := s.queries.ListTokens(ctx, userID)
	if err != nil {
		return nil, err
	}
	ids := make([]int64, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.ID)
	}
	grants, err := s.queries.TokenGrants(ctx, ids)
	if err != nil {
		return nil, err
	}
	permissions := make(map[int64][]access.Permission, len(rows))
	for _, g := range grants {
		permissions[g.TokenID] = append(permissions[g.TokenID], permission(g.Monitor, g.Action))
	}
	tokens := make([]Token, 0, len(rows))
	for _, r := range rows {
		tokens = append(tokens, Token{
			ID:          r.ID,
			Owner:       r.Login,
			Label:       r.Label,
			Permissions: permissions[r.ID],
			CreatedAt:   r.CreatedAt,
			ExpiresAt:   r.ExpiresAt,
			LastUsedAt:  r.LastUsedAt,
		})
	}
	return tokens, nil
}

// RevokeToken deletes the token id of userID, or of anyone when userID is 0.
func (s *Service) RevokeToken(ctx context.Context, id, userID int64) error {
	deleted, err := s.queries.DeleteToken(ctx, db.DeleteTokenParams{ID: id, UserID: userID})
	if err != nil {
		return err
	}
	if deleted == 0 {
		return ErrUnknownToken
	}
	return nil
}

func (s *Service) ByToken(ctx context.Context, secret string) (access.Principal, error) {
	if !strings.HasPrefix(secret, tokenPrefix) {
		return access.Principal{}, ErrUnauthenticated
	}
	now := s.now()
	row, err := s.queries.TokenPerson(ctx, db.TokenPersonParams{Hash: HashSecret(secret), Now: now})
	if errors.Is(err, pgx.ErrNoRows) {
		return access.Principal{}, ErrUnauthenticated
	}
	if err != nil {
		return access.Principal{}, err
	}
	var (
		person access.Person
		grants []db.TokenGrant
	)
	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() (err error) { person, err = s.person(gctx, row.UserID, row.Login, row.Admin); return err })
	g.Go(func() (err error) { grants, err = s.queries.TokenGrants(gctx, []int64{row.ID}); return err })
	g.Go(func() error { return s.queries.TouchToken(gctx, db.TouchTokenParams{ID: row.ID, Now: now}) })
	if err := g.Wait(); err != nil {
		return access.Principal{}, err
	}
	carried := make([]access.Permission, 0, len(grants))
	for _, grant := range grants {
		carried = append(carried, permission(grant.Monitor, grant.Action))
	}
	return access.NewToken(person, carried), nil
}
