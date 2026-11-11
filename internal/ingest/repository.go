package ingest

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/ItzTas/monitor-hub/internal/db"
	"github.com/ItzTas/monitor-hub/sdk/pluginpb"
)

var errCursorMoved = errors.New("ingest: the stored cursor no longer matches the page's after")

type Repository interface {
	Cursor(ctx context.Context, plugin string) (cursor int64, known bool, err error)
	SaveDescription(ctx context.Context, description *pluginpb.DescribeResponse) error
	SaveStatus(ctx context.Context, plugin string, status *pluginpb.StatusResponse, seenAt time.Time) error
	StoreEvents(ctx context.Context, batch Batch) (inserted int, err error)
	CompleteBaseline(ctx context.Context, plugin string) error
}

type postgres struct {
	pool    *pgxpool.Pool
	queries *db.Queries
}

func NewPostgres(pool *pgxpool.Pool) Repository {
	return &postgres{pool: pool, queries: db.New(pool)}
}

func (r *postgres) Cursor(ctx context.Context, plugin string) (int64, bool, error) {
	row, err := r.queries.PluginCursor(ctx, plugin)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return row.Cursor, row.BaselinedAt != nil, nil
}

func (r *postgres) SaveDescription(ctx context.Context, description *pluginpb.DescribeResponse) error {
	return r.queries.SavePluginDescription(ctx, db.SavePluginDescriptionParams{
		Name:        description.GetName(),
		DisplayName: description.GetDisplayName(),
		Version:     description.GetVersion(),
	})
}

func (r *postgres) SaveStatus(ctx context.Context, plugin string, status *pluginpb.StatusResponse, seenAt time.Time) error {
	state, err := smallint("state", status.GetState())
	if err != nil {
		return err
	}
	return r.queries.SavePluginStatus(ctx, db.SavePluginStatusParams{
		Name:               plugin,
		State:              state,
		Message:            status.GetMessage(),
		LastCheckStartedAt: optionalTime(status.GetLastCheckStartedAt()),
		LastSuccessAt:      optionalTime(status.GetLastSuccessAt()),
		NextCheckAt:        optionalTime(status.GetNextCheckAt()),
		StatusSeenAt:       &seenAt,
	})
}

func (r *postgres) StoreEvents(ctx context.Context, batch Batch) (int, error) {
	if err := batch.validate(); err != nil {
		return 0, err
	}
	if len(batch.Events) == 0 {
		return 0, nil
	}
	inserted := 0
	err := r.inTx(ctx, func(q *db.Queries) error {
		moved, err := q.AdvancePluginCursor(ctx, db.AdvancePluginCursorParams{
			Name:  batch.Plugin,
			After: batch.After,
			Next:  batch.Events[len(batch.Events)-1].GetSeq(),
		})
		if err != nil {
			return err
		}
		if moved == 0 {
			return errCursorMoved
		}
		for _, e := range batch.Events {
			stored, err := storeEvent(ctx, q, batch, e)
			if err != nil {
				return err
			}
			if stored {
				inserted++
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return inserted, nil
}

func storeEvent(ctx context.Context, q *db.Queries, batch Batch, e *pluginpb.Event) (bool, error) {
	params, err := insertParams(batch.Plugin, e)
	if err != nil {
		return false, err
	}
	seq, err := q.InsertEvent(ctx, params)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if batch.Baseline {
		return true, nil
	}
	return true, q.QueueDeliveries(ctx, db.QueueDeliveriesParams{
		EventSeq:      seq,
		Channels:      batch.Channels,
		NextAttemptAt: batch.Now,
	})
}

func (r *postgres) CompleteBaseline(ctx context.Context, plugin string) error {
	return r.queries.CompletePluginBaseline(ctx, plugin)
}

func (r *postgres) inTx(ctx context.Context, run func(*db.Queries) error) (err error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("ingest: begin a transaction: %w", err)
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, tx.Rollback(context.WithoutCancel(ctx)))
		}
	}()
	if err := run(r.queries.WithTx(tx)); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("ingest: commit: %w", err)
	}
	return nil
}

func insertParams(plugin string, e *pluginpb.Event) (db.InsertEventParams, error) {
	severity, err := smallint("severity", e.GetSeverity())
	if err != nil {
		return db.InsertEventParams{}, fmt.Errorf("event seq %d: %w", e.GetSeq(), err)
	}
	params := db.InsertEventParams{
		Plugin:     plugin,
		EventID:    e.GetId(),
		PluginSeq:  e.GetSeq(),
		Kind:       e.GetKind(),
		Severity:   severity,
		OccurredAt: e.GetOccurredAt().AsTime(),
		Title:      e.GetTitle(),
		Body:       e.GetBody(),
		Url:        e.GetUrl(),
	}
	if e.GetPayload() == nil {
		return params, nil
	}
	payload, err := protojson.Marshal(e.GetPayload())
	if err != nil {
		return db.InsertEventParams{}, fmt.Errorf("ingest: encode the payload of event seq %d: %w", e.GetSeq(), err)
	}
	params.Payload = payload
	return params, nil
}

func smallint[E ~int32](field string, value E) (int16, error) {
	if value < math.MinInt16 || value > math.MaxInt16 {
		return 0, fmt.Errorf("ingest: %s %d is out of range", field, value)
	}
	return int16(value), nil
}

func optionalTime(ts *timestamppb.Timestamp) *time.Time {
	if ts == nil {
		return nil
	}
	t := ts.AsTime()
	return &t
}
