package notify

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ItzTas/monitor-hub/internal/db"
	"github.com/ItzTas/monitor-hub/sdk/pluginpb"
)

const (
	deliveryPending = "pending"
	deliverySent    = "sent"
	deliveryFailed  = "failed"
)

type Delivery struct {
	EventSeq      int64
	Channel       string
	Status        string
	Attempts      int32
	NextAttemptAt time.Time
	SentAt        *time.Time
	LastError     string
}

type DueDelivery struct {
	Delivery
	Event Event
}

type Outbox interface {
	DueDeliveries(ctx context.Context, channel string, now time.Time, limit int) ([]DueDelivery, error)
	UpdateDeliveries(ctx context.Context, deliveries []Delivery) error
}

type postgresOutbox struct {
	pool    *pgxpool.Pool
	queries *db.Queries
}

func NewOutbox(pool *pgxpool.Pool) Outbox {
	return &postgresOutbox{pool: pool, queries: db.New(pool)}
}

func (o *postgresOutbox) DueDeliveries(ctx context.Context, channel string, now time.Time, limit int) ([]DueDelivery, error) {
	rows, err := o.queries.DueDeliveries(ctx, db.DueDeliveriesParams{Channel: channel, Now: now, MaxRows: int64(limit)})
	if err != nil {
		return nil, fmt.Errorf("notify: read the due %s deliveries: %w", channel, err)
	}
	due := make([]DueDelivery, 0, len(rows))
	for _, row := range rows {
		due = append(due, dueDelivery(row))
	}
	return due, nil
}

func dueDelivery(row db.DueDeliveriesRow) DueDelivery {
	return DueDelivery{
		Delivery: Delivery{
			EventSeq:      row.EventSeq,
			Channel:       row.Channel,
			Status:        row.Status,
			Attempts:      row.Attempts,
			NextAttemptAt: row.NextAttemptAt.UTC(),
			SentAt:        utc(row.SentAt),
			LastError:     row.LastError,
		},
		Event: Event{
			Plugin:      row.Plugin,
			DisplayName: cmp.Or(row.DisplayName, row.Plugin),
			Kind:        row.Kind,
			Title:       row.Title,
			Body:        row.Body,
			URL:         row.Url,
			Urgent:      pluginpb.Severity(row.Severity) == pluginpb.Severity_SEVERITY_URGENT,
			OccurredAt:  row.OccurredAt.UTC(),
		},
	}
}

func (o *postgresOutbox) UpdateDeliveries(ctx context.Context, deliveries []Delivery) (err error) {
	tx, err := o.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("notify: begin a transaction: %w", err)
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, tx.Rollback(context.WithoutCancel(ctx)))
		}
	}()
	q := o.queries.WithTx(tx)
	for _, d := range deliveries {
		err := q.UpdateDelivery(ctx, db.UpdateDeliveryParams{
			EventSeq:      d.EventSeq,
			Channel:       d.Channel,
			Status:        d.Status,
			Attempts:      d.Attempts,
			NextAttemptAt: d.NextAttemptAt,
			LastError:     d.LastError,
			SentAt:        d.SentAt,
		})
		if err != nil {
			return fmt.Errorf("notify: update the %s delivery of event %d: %w", d.Channel, d.EventSeq, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("notify: commit the delivery updates: %w", err)
	}
	return nil
}

func utc(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	value := t.UTC()
	return &value
}
