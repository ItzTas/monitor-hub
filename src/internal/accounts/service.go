package accounts

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ItzTas/monitor-hub/src/internal/db"
)

var (
	ErrInvalidLogin = errors.New("logins use lowercase letters, digits and single hyphens, 2 to 31 characters, starting with a letter")
	ErrWeakPassword = errors.New("use a password of at least 12 characters and at most 256 bytes")
	ErrUserExists   = errors.New("a person with this login already exists")
	ErrUnknownUser  = errors.New("no person has this login")
	ErrLastAdmin    = errors.New("this is the last person with admin")
)

type Service struct {
	pool    *pgxpool.Pool
	queries *db.Queries
	now     func() time.Time
}

func NewService(pool *pgxpool.Pool, now func() time.Time) *Service {
	return &Service{pool: pool, queries: db.New(pool), now: now}
}

func (s *Service) inTx(ctx context.Context, run func(q *db.Queries) error) (err error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, tx.Rollback(context.WithoutCancel(ctx)))
		}
	}()
	if err := run(s.queries.WithTx(tx)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func unknownUser(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrUnknownUser
	}
	return err
}
