package accounts

import (
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ItzTas/monitor-hub/src/internal/core/coretest"
)

func TestMain(m *testing.M) {
	os.Exit(coretest.Run(m))
}

var testNow = time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)

func newService(t *testing.T) (*Service, *pgxpool.Pool) {
	t.Helper()
	pool := coretest.Pool(t)
	return NewService(pool, func() time.Time { return testNow }), pool
}

func exec(t *testing.T, pool *pgxpool.Pool, statement string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(t.Context(), statement, args...); err != nil {
		t.Fatalf("%s: %v", statement, err)
	}
}
