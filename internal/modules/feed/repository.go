package feed

import (
	"context"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ItzTas/monitor-hub/internal/db"
)

type Repository interface {
	RecentEvents(ctx context.Context, plugin string, limit int) ([]Event, error)
}

type Postgres struct {
	queries *db.Queries
}

func NewPostgres(pool *pgxpool.Pool) *Postgres {
	return &Postgres{queries: db.New(pool)}
}

func (r *Postgres) RecentEvents(ctx context.Context, plugin string, limit int) ([]Event, error) {
	rows, err := r.queries.ListEvents(ctx, db.ListEventsParams{
		Plugin:  pgtype.Text{String: plugin, Valid: plugin != ""},
		MaxRows: int64(limit),
	})
	if err != nil {
		return nil, err
	}
	events := make([]Event, 0, len(rows))
	for _, row := range rows {
		events = append(events, Event{
			Plugin:      row.Plugin,
			DisplayName: row.DisplayName,
			ID:          row.EventID,
			Kind:        row.Kind,
			Title:       row.Title,
			Body:        row.Body,
			URL:         row.Url,
			OccurredAt:  row.OccurredAt,
		})
	}
	return events, nil
}
