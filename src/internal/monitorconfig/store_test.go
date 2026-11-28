package monitorconfig

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ItzTas/monitor-hub/src/internal/core/coretest"
	"github.com/ItzTas/monitor-hub/src/internal/db"
)

var savedAt = time.Date(2026, 11, 27, 10, 30, 0, 0, time.UTC)

func fullSaved() Saved {
	return Saved{
		Document:  map[string]json.RawMessage{"interval": json.RawMessage(`"10m"`), "mail": json.RawMessage(`{"host":"smtp.example.test","port":587}`)},
		Secrets:   map[string]string{"mail.password": "s3cret-value"},
		Ignored:   []IgnoredValue{{Path: "old.token", Value: json.RawMessage(`"hidden-value"`), Secret: true, Reason: ReasonReplaced}},
		Variables: map[string]Variable{"EXTRA": {Value: "1", Secret: false}},
		UpdatedAt: savedAt,
	}
}

func newStore(t *testing.T) *Postgres {
	t.Helper()
	return NewPostgres(coretest.Pool(t))
}

// same compares without printing the values: a stored document holds secrets.
func same(t *testing.T, got, want Saved) {
	t.Helper()
	if !got.UpdatedAt.Equal(want.UpdatedAt) {
		t.Errorf("UpdatedAt = %v; want %v", got.UpdatedAt, want.UpdatedAt)
	}
	got.UpdatedAt, want.UpdatedAt = time.Time{}, time.Time{}
	if !reflect.DeepEqual(asJSON(t, got), asJSON(t, want)) {
		t.Error("the saved configuration differs from what was stored (values not shown)")
	}
}

// asJSON reads a Saved as plain JSON values, so key order and spacing do not count.
func asJSON(t *testing.T, saved Saved) any {
	t.Helper()
	data, err := json.Marshal(saved)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	var plain any
	if err := json.Unmarshal(data, &plain); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return plain
}

func empty() Saved {
	return Saved{Document: map[string]json.RawMessage{}, Secrets: map[string]string{}, Variables: map[string]Variable{}}
}

func TestGetWithoutARowIsEmptyAtRevisionZero(t *testing.T) {
	got, err := newStore(t).Get(t.Context(), "sei")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	same(t, got, empty())
	if got.Revision != 0 {
		t.Errorf("Revision = %d; want 0", got.Revision)
	}
}

func TestStoreKeepsEveryPart(t *testing.T) {
	store := newStore(t)
	want := fullSaved()
	revision, err := store.Put(t.Context(), "sei", want, 0)
	if err != nil || revision != 1 {
		t.Fatalf("Put = %d, %v; want 1", revision, err)
	}
	got, err := store.Get(t.Context(), "sei")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	want.Revision = 1
	same(t, got, want)

	next := fullSaved()
	next.Secrets = map[string]string{}
	revision, err = store.Put(t.Context(), "sei", next, 1)
	if err != nil || revision != 2 {
		t.Fatalf("second Put = %d, %v; want 2", revision, err)
	}
	got, err = store.Get(t.Context(), "sei")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	next.Revision = 2
	same(t, got, next)
}

func TestAStaleRevisionWritesNothing(t *testing.T) {
	store := newStore(t)
	other := fullSaved()
	if _, err := store.Put(t.Context(), "diario", other, 0); err != nil {
		t.Fatalf("Put diario: %v", err)
	}
	if _, err := store.Put(t.Context(), "sei", fullSaved(), 0); err != nil {
		t.Fatalf("Put sei: %v", err)
	}
	before, err := store.Get(t.Context(), "sei")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	changed := empty()
	changed.UpdatedAt = savedAt.Add(time.Hour)
	for _, previous := range []int64{0, 2, 7} {
		if _, err := store.Put(t.Context(), "sei", changed, previous); !errors.Is(err, ErrStale) {
			t.Errorf("Put with previous %d: %v; want ErrStale", previous, err)
		}
	}
	after, err := store.Get(t.Context(), "sei")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	same(t, after, before)

	if _, err := store.Put(t.Context(), "anhanguera", changed, 3); !errors.Is(err, ErrStale) {
		t.Errorf("Put without a row and previous 3: %v; want ErrStale", err)
	}
	created, err := store.Get(t.Context(), "anhanguera")
	if err != nil || created.Revision != 0 {
		t.Errorf("Get anhanguera = revision %d, %v; want 0 (no row)", created.Revision, err)
	}

	untouched, err := store.Get(t.Context(), "diario")
	if err != nil {
		t.Fatalf("Get diario: %v", err)
	}
	other.Revision = 1
	same(t, untouched, other)
}

func TestAValueWithNULIsRefusedByTheDatabase(t *testing.T) {
	store := newStore(t)
	if _, err := store.Put(t.Context(), "sei", fullSaved(), 0); err != nil {
		t.Fatalf("Put: %v", err)
	}
	before, err := store.Get(t.Context(), "sei")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	bad := fullSaved()
	bad.Secrets = map[string]string{"mail.password": "zq-before\x00zq-after"}
	_, err = store.Put(t.Context(), "sei", bad, 1)
	if err == nil {
		t.Fatal("Put with a NUL in a secret succeeded")
	}
	if errors.Is(err, ErrStale) {
		t.Errorf("Put = %v; want a database error, not ErrStale", err)
	}
	if strings.Contains(err.Error(), "zq-") {
		t.Error("the error repeats a stored value (not shown)")
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		t.Error("the error keeps the Postgres error, whose context quotes the value")
	}
	after, err := store.Get(t.Context(), "sei")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	same(t, after, before)
}

// waitForBlockedBackend returns once a backend of pool's database waits on a lock, or done is
// closed. It fails t after 30 s. pg_locks is cluster-wide and a transactionid lock carries no
// database, so the backend's database comes from pg_stat_activity.
func waitForBlockedBackend(t *testing.T, pool *pgxpool.Pool, done <-chan struct{}) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	for {
		select {
		case <-done:
			return
		default:
		}
		var blocked bool
		err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_locks l
			JOIN pg_stat_activity a ON a.pid = l.pid
			WHERE NOT l.granted AND a.datname = current_database())`).Scan(&blocked)
		if err != nil {
			t.Fatalf("no backend waited on a lock: %v", err)
		}
		if blocked {
			return
		}
	}
}

// A Put that waits on a concurrent writer of the same row loses once that writer commits.
func TestOnlyOneOfTwoConcurrentWritesWins(t *testing.T) {
	cases := []struct {
		name     string
		existing bool
		previous int64
	}{
		{"an existing row", true, 1},
		{"a missing row", false, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pool := coretest.Pool(t)
			store := NewPostgres(pool)
			if tc.existing {
				if _, err := store.Put(t.Context(), "sei", fullSaved(), 0); err != nil {
					t.Fatalf("Put: %v", err)
				}
			}

			tx, err := pool.Begin(t.Context())
			if err != nil {
				t.Fatalf("begin: %v", err)
			}
			defer func() { _ = tx.Rollback(context.Background()) }()
			rows, err := db.New(tx).PutMonitorConfig(t.Context(), db.PutMonitorConfigParams{
				Monitor: "sei", Document: []byte(`{}`), Secrets: []byte(`{}`), Ignored: []byte(`[]`),
				Variables: []byte(`{}`), UpdatedAt: savedAt, Previous: tc.previous,
			})
			if err != nil || rows != 1 {
				t.Fatalf("Put inside the transaction = %d, %v; want 1", rows, err)
			}

			done := make(chan struct{})
			var putErr error
			go func() {
				defer close(done)
				_, putErr = store.Put(t.Context(), "sei", fullSaved(), tc.previous)
			}()
			waitForBlockedBackend(t, pool, done)
			if err := tx.Commit(t.Context()); err != nil {
				t.Fatalf("commit: %v", err)
			}
			<-done

			if !errors.Is(putErr, ErrStale) {
				t.Errorf("the waiting Put = %v; want ErrStale", putErr)
			}
			got, err := store.Get(t.Context(), "sei")
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if want := tc.previous + 1; got.Revision != want {
				t.Errorf("Revision = %d; want %d", got.Revision, want)
			}
		})
	}
}
