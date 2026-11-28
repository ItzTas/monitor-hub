package core

import (
	"database/sql"
	"errors"
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ItzTas/monitor-hub/src/internal/core/coretest"
)

func migratedDatabase(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("pgx", coretest.Database(t))
	if err != nil {
		t.Fatalf("open the database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func execAll(t *testing.T, db *sql.DB, statements ...string) {
	t.Helper()
	for _, statement := range statements {
		if _, err := db.ExecContext(t.Context(), statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
}

func count(t *testing.T, db *sql.DB, table string) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(t.Context(), "SELECT count(*) FROM "+table).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

func expect(t *testing.T, what string, got, want any) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%s:\n got %v\nwant %v", what, got, want)
	}
}

func pgError(t *testing.T, what string, err error) *pgconn.PgError {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Errorf("%s: err = %v; want *pgconn.PgError", what, err)
		return nil
	}
	return pgErr
}

func expectCode(t *testing.T, db *sql.DB, what, statement, code string) {
	t.Helper()
	_, err := db.ExecContext(t.Context(), statement)
	if pgErr := pgError(t, what, err); pgErr != nil && pgErr.Code != code {
		t.Errorf("%s: code = %s; want %s", what, pgErr.Code, code)
	}
}

func TestEventsAreNumberedAndKeyedByMonitorAndID(t *testing.T) {
	db := migratedDatabase(t)
	execAll(t, db, `INSERT INTO monitors (name, display_name, version) VALUES ('sei', 'SEI', '')`)
	insert := `INSERT INTO events (monitor, event_id, monitor_seq, kind, severity, occurred_at, title, body, url)
		VALUES ('sei', $1, $2, 'k', 1, now(), 't', '', '') ON CONFLICT (monitor, event_id) DO NOTHING RETURNING seq`

	for i, id := range []string{"e1", "e2"} {
		var seq int64
		if err := db.QueryRowContext(t.Context(), insert, id, i+1).Scan(&seq); err != nil || seq != int64(i+1) {
			t.Errorf("event %s: seq = %d, %v; want %d", id, seq, err, i+1)
		}
	}

	var seq int64
	err := db.QueryRowContext(t.Context(), insert, "e2", 9).Scan(&seq)
	if !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("duplicate (sei, e2): err = %v; want no row", err)
	}

	_, err = db.ExecContext(t.Context(), `INSERT INTO events (monitor, event_id, monitor_seq, kind, severity, occurred_at, title, body, url)
		VALUES ('nobody', 'e1', 1, 'k', 1, now(), 't', '', '')`)
	if pgErr := pgError(t, "unknown monitor", err); pgErr != nil {
		expect(t, "foreign key code", pgErr.Code, "23503")
		expect(t, "foreign key constraint", pgErr.ConstraintName, "events_monitor_fkey")
	}
}

func TestAccessTablesCascadeAndCheckTheirValues(t *testing.T) {
	db := migratedDatabase(t)
	execAll(t, db,
		`INSERT INTO monitors (name, display_name, version) VALUES ('sei', 'SEI', ''), ('diario', 'Diario', '')`,
		`INSERT INTO users (login, password_hash) VALUES ('paulo-santos', 'hash')`,
		`INSERT INTO user_grants (user_id, monitor, action) SELECT id, 'sei', 'view' FROM users`,
		`INSERT INTO user_grants (user_id, monitor, action) SELECT id, NULL, 'view' FROM users`,
		`INSERT INTO tokens (user_id, label, hash) SELECT id, 'rss', 'token-hash' FROM users`,
		`INSERT INTO token_grants (token_id, monitor, action) SELECT id, 'sei', 'view' FROM tokens`,
		`INSERT INTO credentials (user_id, label, username, secret_hash) SELECT id, 'hook', 'sei-hook', 'secret-hash' FROM users`,
		`INSERT INTO credential_grants (credential_id, monitor, action) SELECT id, 'sei', 'hook' FROM credentials`,
		`INSERT INTO sessions (id_hash, user_id, expires_at) SELECT 'session-hash', id, now() + interval '12 hours' FROM users`,
	)
	var admin bool
	if err := db.QueryRowContext(t.Context(), `SELECT admin FROM users`).Scan(&admin); err != nil || admin {
		t.Errorf("admin = %v, %v; want false", admin, err)
	}

	expectCode(t, db, "read on user_grants", `INSERT INTO user_grants (user_id, monitor, action) SELECT id, 'diario', 'read' FROM users`, "23514")
	expectCode(t, db, "configure on token_grants", `INSERT INTO token_grants (token_id, monitor, action) SELECT id, 'diario', 'configure' FROM tokens`, "23514")
	expectCode(t, db, "view on credential_grants", `INSERT INTO credential_grants (credential_id, monitor, action) SELECT id, 'diario', 'view' FROM credentials`, "23514")
	expectCode(t, db, "same user grant twice", `INSERT INTO user_grants (user_id, monitor, action) SELECT id, 'sei', 'view' FROM users`, "23505")
	expectCode(t, db, "same user grant for all twice", `INSERT INTO user_grants (user_id, monitor, action) SELECT id, NULL, 'view' FROM users`, "23505")
	expectCode(t, db, "same token grant twice", `INSERT INTO token_grants (token_id, monitor, action) SELECT id, 'sei', 'view' FROM tokens`, "23505")
	expectCode(t, db, "same token grant for all twice", `INSERT INTO token_grants (token_id, monitor, action) SELECT id, NULL, 'view' FROM tokens UNION ALL SELECT id, NULL, 'view' FROM tokens`, "23505")
	expectCode(t, db, "same credential grant twice", `INSERT INTO credential_grants (credential_id, monitor, action) SELECT id, 'sei', 'hook' FROM credentials`, "23505")
	expectCode(t, db, "same credential grant for all twice", `INSERT INTO credential_grants (credential_id, monitor, action) SELECT id, NULL, 'hook' FROM credentials UNION ALL SELECT id, NULL, 'hook' FROM credentials`, "23505")
	expectCode(t, db, "same login twice", `INSERT INTO users (login, password_hash) VALUES ('paulo-santos', 'other-hash')`, "23505")
	expectCode(t, db, "same token hash twice", `INSERT INTO tokens (user_id, label, hash) SELECT id, 'feed', 'token-hash' FROM users`, "23505")
	expectCode(t, db, "same credential username twice", `INSERT INTO credentials (user_id, label, username, secret_hash) SELECT id, 'other', 'sei-hook', 'other-hash' FROM users`, "23505")
	expectCode(t, db, "grant on a missing monitor", `INSERT INTO user_grants (user_id, monitor, action) SELECT id, 'nobody', 'view' FROM users`, "23503")

	execAll(t, db, `DELETE FROM monitors WHERE name = 'sei'`)
	expect(t, "user grants", count(t, db, "user_grants"), 1)
	expect(t, "token grants", count(t, db, "token_grants"), 0)
	expect(t, "credential grants", count(t, db, "credential_grants"), 0)

	execAll(t, db,
		`INSERT INTO credential_grants (credential_id, monitor, action) SELECT id, NULL, 'hook' FROM credentials`,
		`DELETE FROM credentials`,
	)
	expect(t, "credential grants of a deleted credential", count(t, db, "credential_grants"), 0)

	execAll(t, db,
		`INSERT INTO token_grants (token_id, monitor, action) SELECT id, NULL, 'check' FROM tokens`,
		`INSERT INTO credentials (user_id, label, username, secret_hash) SELECT id, 'hook', 'sei-hook', 'secret-hash' FROM users`,
		`INSERT INTO credential_grants (credential_id, monitor, action) SELECT id, NULL, 'hook' FROM credentials`,
		`DELETE FROM users`,
	)
	for _, table := range []string{"user_grants", "tokens", "token_grants", "credentials", "credential_grants", "sessions"} {
		expect(t, table, count(t, db, table), 0)
	}
}
