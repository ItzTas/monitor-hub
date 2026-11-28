package monitorconfig

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ItzTas/monitor-hub/src/internal/db"
)

var ErrStale = errors.New("the saved configuration changed since this revision")

type Store interface {
	Get(ctx context.Context, monitor string) (Saved, error)
	// Put writes saved over revision previous, 0 for a monitor with nothing stored, and
	// returns the new revision. It returns ErrStale when previous is not the stored revision.
	Put(ctx context.Context, monitor string, saved Saved, previous int64) (int64, error)
}

type Postgres struct {
	queries *db.Queries
}

func NewPostgres(pool *pgxpool.Pool) *Postgres {
	return &Postgres{queries: db.New(pool)}
}

func (p *Postgres) Get(ctx context.Context, monitor string) (Saved, error) {
	row, err := p.queries.GetMonitorConfig(ctx, monitor)
	if errors.Is(err, pgx.ErrNoRows) {
		return Saved{Document: map[string]json.RawMessage{}, Secrets: map[string]string{}, Variables: map[string]Variable{}}, nil
	}
	if err != nil {
		return Saved{}, fmt.Errorf("read the configuration of monitor %s: %w", monitor, err)
	}
	saved := Saved{Revision: row.Revision, UpdatedAt: row.UpdatedAt}
	for _, part := range []struct {
		name string
		data []byte
		into any
	}{
		{"document", row.Document, &saved.Document},
		{"secrets", row.Secrets, &saved.Secrets},
		{"ignored", row.Ignored, &saved.Ignored},
		{"variables", row.Variables, &saved.Variables},
	} {
		if err := json.Unmarshal(part.data, part.into); err != nil {
			return Saved{}, fmt.Errorf("decode the %s of monitor %s: %w", part.name, monitor, err)
		}
	}
	if len(saved.Ignored) == 0 {
		saved.Ignored = nil
	}
	return compacted(saved)
}

func (p *Postgres) Put(ctx context.Context, monitor string, saved Saved, previous int64) (int64, error) {
	params := db.PutMonitorConfigParams{Monitor: monitor, UpdatedAt: saved.UpdatedAt, Previous: previous}
	ignored := saved.Ignored
	if ignored == nil {
		ignored = []IgnoredValue{}
	}
	for _, part := range []struct {
		value any
		into  *[]byte
	}{
		{orEmpty(saved.Document), &params.Document},
		{orEmpty(saved.Secrets), &params.Secrets},
		{ignored, &params.Ignored},
		{orEmpty(saved.Variables), &params.Variables},
	} {
		data, err := json.Marshal(part.value)
		if err != nil {
			return 0, fmt.Errorf("encode the configuration of monitor %s: %w", monitor, err)
		}
		*part.into = data
	}
	rows, err := p.queries.PutMonitorConfig(ctx, params)
	if err != nil {
		return 0, fmt.Errorf("save the configuration of monitor %s: %w", monitor, withoutValues(err))
	}
	if rows == 0 {
		return 0, ErrStale
	}
	return previous + 1, nil
}

// compacted drops the spaces jsonb adds.
func compacted(saved Saved) (Saved, error) {
	var buf bytes.Buffer
	shrink := func(raw json.RawMessage) (json.RawMessage, error) {
		buf.Reset()
		if err := json.Compact(&buf, raw); err != nil {
			return nil, err
		}
		return bytes.Clone(buf.Bytes()), nil
	}
	var err error
	for name, raw := range saved.Document {
		if saved.Document[name], err = shrink(raw); err != nil {
			return Saved{}, fmt.Errorf("decode the document: %w", err)
		}
	}
	for i, value := range saved.Ignored {
		if saved.Ignored[i].Value, err = shrink(value.Value); err != nil {
			return Saved{}, fmt.Errorf("decode the ignored values: %w", err)
		}
	}
	return saved, nil
}

func orEmpty[V any](m map[string]V) map[string]V {
	if m == nil {
		return map[string]V{}
	}
	return m
}

// withoutValues drops what Postgres quotes from the rejected JSON, which holds secrets,
// and keeps only the error code.
func withoutValues(err error) error {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return err
	}
	return fmt.Errorf("database error %s", pgErr.Code)
}
