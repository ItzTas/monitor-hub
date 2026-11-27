package accounts

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"golang.org/x/sync/errgroup"

	"github.com/ItzTas/monitor-hub/src/internal/access"
	"github.com/ItzTas/monitor-hub/src/internal/db"
)

const SessionLifetime = 12 * time.Hour

type Session struct {
	ID        string // shown once, in the cookie
	ExpiresAt time.Time
}

var (
	ErrWrongPassword   = errors.New("wrong login or password")
	ErrUnauthenticated = errors.New("invalid or expired credentials")
)

// unknownLoginHash gives an unknown login the same work as a real login.
const unknownLoginHash = "$argon2id$v=19$m=65536,t=3,p=4$qBtRiA6Az1jxMXmER3njzQ$JdOxufyF3Psc25cZ02kpXXVI5g9Ra9cFXHsR+dhPn2Q"

func (s *Service) Login(ctx context.Context, login, password string) (Session, access.Principal, error) {
	user, err := s.queries.UserForLogin(ctx, login)
	if errors.Is(err, pgx.ErrNoRows) {
		_, _ = VerifyPassword(unknownLoginHash, password)
		return Session{}, access.Principal{}, ErrWrongPassword
	}
	if err != nil {
		return Session{}, access.Principal{}, err
	}
	ok, err := VerifyPassword(user.PasswordHash, password)
	if err != nil {
		return Session{}, access.Principal{}, err
	}
	if !ok || user.DisabledAt != nil {
		return Session{}, access.Principal{}, ErrWrongPassword
	}

	id, hash, err := NewSecret("")
	if err != nil {
		return Session{}, access.Principal{}, err
	}
	now := s.now()
	session := Session{ID: id, ExpiresAt: now.Add(SessionLifetime)}
	var person access.Person
	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error { return s.queries.DeleteExpiredSessions(gctx, now) })
	g.Go(func() error {
		return s.queries.InsertSession(gctx, db.InsertSessionParams{IDHash: hash, UserID: user.ID, ExpiresAt: session.ExpiresAt})
	})
	g.Go(func() (err error) { person, err = s.person(gctx, user.ID, user.Login, user.Admin); return err })
	if err := g.Wait(); err != nil {
		return Session{}, access.Principal{}, err
	}
	return session, access.NewSession(person), nil
}

func (s *Service) Logout(ctx context.Context, sessionID string) error {
	return s.queries.DeleteSession(ctx, HashSecret(sessionID))
}

func (s *Service) BySession(ctx context.Context, sessionID string) (access.Principal, error) {
	user, err := s.queries.SessionPerson(ctx, db.SessionPersonParams{IDHash: HashSecret(sessionID), Now: s.now()})
	if errors.Is(err, pgx.ErrNoRows) {
		return access.Principal{}, ErrUnauthenticated
	}
	if err != nil {
		return access.Principal{}, err
	}
	person, err := s.person(ctx, user.ID, user.Login, user.Admin)
	if err != nil {
		return access.Principal{}, err
	}
	return access.NewSession(person), nil
}
