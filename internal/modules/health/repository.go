package health

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
)

type Repository interface {
	Ping(ctx context.Context) error
}

type Postgres struct {
	pool *pgxpool.Pool
}

func NewPostgres(pool *pgxpool.Pool) *Postgres {
	return &Postgres{pool: pool}
}

func (r *Postgres) Ping(ctx context.Context) error {
	return r.pool.Ping(ctx)
}
