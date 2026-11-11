package plugins

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ItzTas/monitor-hub/internal/db"
)

type Repository interface {
	ListPlugins(ctx context.Context) ([]stored, error)
}

type Postgres struct {
	queries *db.Queries
}

func NewPostgres(pool *pgxpool.Pool) *Postgres {
	return &Postgres{queries: db.New(pool)}
}

func (r *Postgres) ListPlugins(ctx context.Context) ([]stored, error) {
	rows, err := r.queries.ListPlugins(ctx)
	if err != nil {
		return nil, err
	}
	plugins := make([]stored, 0, len(rows))
	for _, row := range rows {
		plugins = append(plugins, stored{
			Plugin: Plugin{
				Name:               row.Name,
				DisplayName:        row.DisplayName,
				Version:            row.Version,
				Message:            row.Message,
				LastCheckStartedAt: utc(row.LastCheckStartedAt),
				LastSuccessAt:      utc(row.LastSuccessAt),
				NextCheckAt:        utc(row.NextCheckAt),
				StatusSeenAt:       utc(row.StatusSeenAt),
			},
			State: row.State,
		})
	}
	return plugins, nil
}

func utc(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	value := t.UTC()
	return &value
}
