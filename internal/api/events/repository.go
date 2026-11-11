package events

import (
	"context"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ItzTas/monitor-hub/internal/db"
	"github.com/ItzTas/monitor-hub/sdk/pluginpb"
)

type Repository interface {
	ListEvents(ctx context.Context, filter Filter) ([]Event, error)
}

type Postgres struct {
	queries *db.Queries
}

func NewPostgres(pool *pgxpool.Pool) *Postgres {
	return &Postgres{queries: db.New(pool)}
}

func (r *Postgres) ListEvents(ctx context.Context, filter Filter) ([]Event, error) {
	rows, err := r.queries.ListEvents(ctx, db.ListEventsParams{
		Plugin:  pgtype.Text{String: filter.Plugin, Valid: filter.Plugin != ""},
		Kind:    pgtype.Text{String: filter.Kind, Valid: filter.Kind != ""},
		Before:  pgtype.Int8{Int64: filter.Before, Valid: filter.Before > 0},
		MaxRows: int64(filter.Limit),
	})
	if err != nil {
		return nil, err
	}
	events := make([]Event, 0, len(rows))
	for _, row := range rows {
		events = append(events, Event{
			Seq:         row.Seq,
			Plugin:      row.Plugin,
			DisplayName: row.DisplayName,
			ID:          row.EventID,
			Kind:        row.Kind,
			Severity:    severityName(row.Severity),
			OccurredAt:  row.OccurredAt.UTC(),
			DetectedAt:  row.DetectedAt.UTC(),
			Title:       row.Title,
			Body:        row.Body,
			URL:         row.Url,
			Payload:     row.Payload,
		})
	}
	return events, nil
}

func severityName(severity int16) string {
	if pluginpb.Severity(severity) == pluginpb.Severity_SEVERITY_URGENT {
		return "urgent"
	}
	return "info"
}
