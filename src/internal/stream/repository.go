package stream

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ItzTas/monitor-hub/sdk-go/pluginpb"
	"github.com/ItzTas/monitor-hub/src/internal/db"
)

type Repository interface {
	After(ctx context.Context, after int64, limit int) ([]Notification, error)
	Latest(ctx context.Context) (int64, error)
}

type postgres struct {
	q *db.Queries
}

func NewPostgres(pool *pgxpool.Pool) Repository {
	return &postgres{q: db.New(pool)}
}

func (p *postgres) After(ctx context.Context, after int64, limit int) ([]Notification, error) {
	rows, err := p.q.NotificationsAfter(ctx, db.NotificationsAfterParams{After: after, MaxRows: int64(limit)})
	if err != nil {
		return nil, fmt.Errorf("read events after %d: %w", after, err)
	}
	out := make([]Notification, 0, len(rows))
	for _, row := range rows {
		out = append(out, Notification{
			Seq:         row.Seq,
			Plugin:      row.Plugin,
			DisplayName: row.DisplayName,
			Kind:        row.Kind,
			Urgent:      pluginpb.Severity(row.Severity) == pluginpb.Severity_SEVERITY_URGENT,
			Title:       row.Title,
			Body:        row.Body,
			URL:         row.Url,
			OccurredAt:  row.OccurredAt.UTC(),
			DetectedAt:  row.DetectedAt.UTC(),
		})
	}
	return out, nil
}

func (p *postgres) Latest(ctx context.Context) (int64, error) {
	seq, err := p.q.LatestEventSeq(ctx)
	if err != nil {
		return 0, fmt.Errorf("read the latest event: %w", err)
	}
	return seq, nil
}
