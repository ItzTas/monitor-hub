package coretest

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// WaitForAdvisoryWaiter returns once a backend of pool's database waits on an
// advisory lock, or done is closed. It fails t if neither happens in 30 s.
func WaitForAdvisoryWaiter(t testing.TB, pool *pgxpool.Pool, done <-chan struct{}) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	for {
		select {
		case <-done:
			return
		default:
		}
		var waiting int
		err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_locks
			WHERE locktype = 'advisory' AND NOT granted
			AND database = (SELECT oid FROM pg_database WHERE datname = current_database())`).Scan(&waiting)
		if err != nil {
			t.Fatalf("coretest: no backend waited on an advisory lock: %v", err)
		}
		if waiting > 0 {
			return
		}
	}
}
