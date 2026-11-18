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

	"github.com/ItzTas/monitor-hub/sdk-go/monitorpb"
	"github.com/ItzTas/monitor-hub/src/internal/db"
)

const eventOrderLock = 1836019304

var errCursorMoved = errors.New("ingest: the stored cursor no longer matches the page's after")

type Repository interface {
	Cursor(ctx context.Context, monitor string) (cursor int64, known bool, err error)
	SaveDescription(ctx context.Context, description *monitorpb.DescribeResponse) error
	SaveStatus(ctx context.Context, monitor string, status *monitorpb.StatusResponse, seenAt time.Time) error
	StoreEvents(ctx context.Context, batch Batch) (inserted int, err error)
	CompleteBaseline(ctx context.Context, monitor string) error
}

type postgres struct {
	pool    *pgxpool.Pool
	queries *db.Queries
}

func NewPostgres(pool *pgxpool.Pool) Repository {
	return &postgres{pool: pool, queries: db.New(pool)}
}

func (r *postgres) Cursor(ctx context.Context, monitor string) (int64, bool, error) {
	row, err := r.queries.MonitorCursor(ctx, monitor)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return row.Cursor, row.BaselinedAt != nil, nil
}

func (r *postgres) SaveDescription(ctx context.Context, description *monitorpb.DescribeResponse) error {
	return r.queries.SaveMonitorDescription(ctx, db.SaveMonitorDescriptionParams{
		Name:        description.GetName(),
		DisplayName: description.GetDisplayName(),
		Version:     description.GetVersion(),
	})
}

func (r *postgres) SaveStatus(ctx context.Context, monitor string, status *monitorpb.StatusResponse, seenAt time.Time) error {
	state, err := smallint("state", status.GetState())
	if err != nil {
		return err
	}
	return r.queries.SaveMonitorStatus(ctx, db.SaveMonitorStatusParams{
		Name:               monitor,
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
		if err := q.LockEventOrder(ctx); err != nil {
			return err
		}
		moved, err := q.AdvanceMonitorCursor(ctx, db.AdvanceMonitorCursorParams{
			Name:  batch.Monitor,
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

func storeEvent(ctx context.Context, q *db.Queries, batch Batch, e *monitorpb.Event) (bool, error) {
	params, err := insertParams(batch.Monitor, e, batch.Baseline)
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

func (r *postgres) CompleteBaseline(ctx context.Context, monitor string) error {
	return r.queries.CompleteMonitorBaseline(ctx, monitor)
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

func insertParams(monitor string, e *monitorpb.Event, baseline bool) (db.InsertEventParams, error) {
	severity, err := smallint("severity", e.GetSeverity())
	if err != nil {
		return db.InsertEventParams{}, fmt.Errorf("event seq %d: %w", e.GetSeq(), err)
	}
	params := db.InsertEventParams{
		Monitor:    monitor,
		EventID:    e.GetId(),
		MonitorSeq: e.GetSeq(),
		Kind:       e.GetKind(),
		Severity:   severity,
		OccurredAt: e.GetOccurredAt().AsTime(),
		Title:      e.GetTitle(),
		Body:       e.GetBody(),
		Url:        e.GetUrl(),
		Baseline:   baseline,
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
