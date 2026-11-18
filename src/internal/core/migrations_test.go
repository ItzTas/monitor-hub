package core

import (
	"database/sql"
	"errors"
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pressly/goose/v3"

	"github.com/ItzTas/monitor-hub/sql/schema"
	"github.com/ItzTas/monitor-hub/src/internal/core/coretest"
)

func emptyDatabase(t *testing.T) (string, *sql.DB) {
	t.Helper()
	address := coretest.Database(t)
	db, err := sql.Open("pgx", address)
	if err != nil {
		t.Fatalf("open the database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.ExecContext(t.Context(), "DROP SCHEMA public CASCADE; CREATE SCHEMA public"); err != nil {
		t.Fatalf("empty the database: %v", err)
	}
	return address, db
}

func provider(t *testing.T, db *sql.DB) *goose.Provider {
	t.Helper()
	p, err := goose.NewProvider(goose.DialectPostgres, db, schema.Files, goose.WithDisableGlobalRegistry(true))
	if err != nil {
		t.Fatalf("prepare the migrations: %v", err)
	}
	return p
}

func seed(t *testing.T, db *sql.DB) *goose.Provider {
	t.Helper()
	p := provider(t, db)
	if _, err := p.UpTo(t.Context(), 3); err != nil {
		t.Fatalf("migrate to 3: %v", err)
	}
	statements := []string{
		`INSERT INTO plugins (name, display_name, version, cursor, baselined_at, state, message,
			last_check_started_at, last_success_at, next_check_at, status_seen_at) VALUES
			('sei', 'SEI', '2.1.0', 2, '2026-01-02 03:04:05+00', 4, 'blocked by captcha',
			 '2026-01-03 00:00:01+00', '2026-01-03 00:00:02+00', '2026-01-03 00:00:03+00', '2026-01-03 00:00:04+00')`,
		`INSERT INTO plugins (name, display_name, version) VALUES ('diario', 'Diario', '1.0.0')`,
		`INSERT INTO events (plugin, event_id, plugin_seq, kind, severity, occurred_at, detected_at, title, body, url, payload, baseline)
			VALUES ('sei', 'e1', 1, 'process.updated', 1, '2026-02-01 10:00:00+00', '2026-02-01 10:00:05+00', 'First', 'Body one', 'https://sei.example/1', '{"n":1}', true)`,
		`INSERT INTO events (plugin, event_id, plugin_seq, kind, severity, occurred_at, detected_at, title, body, url, payload)
			VALUES ('sei', 'e2', 2, 'process.created', 2, '2026-02-02 10:00:00+00', '2026-02-02 10:00:05+00', 'Second', '', '', NULL)`,
		`INSERT INTO deliveries (event_seq, channel, status, attempts, next_attempt_at, sent_at, last_error)
			VALUES (2, 'telegram', 'sent', 1, '2026-02-02 10:01:00+00', '2026-02-02 10:01:30+00', '')`,
		`INSERT INTO deliveries (event_seq, channel, status, attempts, next_attempt_at, last_error)
			VALUES (2, 'ntfy', 'pending', 2, '2026-02-02 10:05:00+00', 'timeout')`,
		`INSERT INTO consumers (name, token_hash, position, created_at) VALUES ('desk', 'hash', 2, '2026-02-03 00:00:00+00')`,
	}
	for _, statement := range statements {
		if _, err := db.ExecContext(t.Context(), statement); err != nil {
			t.Fatalf("seed %q: %v", statement, err)
		}
	}
	return p
}

func rows(t *testing.T, db *sql.DB, query string) []string {
	t.Helper()
	result, err := db.QueryContext(t.Context(), "SELECT row("+query)
	if err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	defer func() { _ = result.Close() }()
	var out []string
	for result.Next() {
		var line string
		if err := result.Scan(&line); err != nil {
			t.Fatalf("scan %q: %v", query, err)
		}
		out = append(out, line)
	}
	if err := result.Err(); err != nil {
		t.Fatalf("read %q: %v", query, err)
	}
	return out
}

func column(t *testing.T, db *sql.DB, query string) []string {
	t.Helper()
	result, err := db.QueryContext(t.Context(), query)
	if err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	defer func() { _ = result.Close() }()
	var out []string
	for result.Next() {
		var value string
		if err := result.Scan(&value); err != nil {
			t.Fatalf("scan %q: %v", query, err)
		}
		out = append(out, value)
	}
	if err := result.Err(); err != nil {
		t.Fatalf("read %q: %v", query, err)
	}
	return out
}

type snapshot struct{ monitors, events, deliveries, consumers []string }

func take(t *testing.T, db *sql.DB, monitorTable, monitorColumn, seqColumn string) snapshot {
	t.Helper()
	return snapshot{
		monitors:   rows(t, db, "name, display_name, version, cursor, baselined_at, state, message, last_check_started_at, last_success_at, next_check_at, status_seen_at)::text FROM "+monitorTable+" ORDER BY name"),
		events:     rows(t, db, "seq, "+monitorColumn+", event_id, "+seqColumn+", kind, severity, occurred_at, detected_at, title, body, url, payload, baseline)::text FROM events ORDER BY seq"),
		deliveries: rows(t, db, "d.*)::text FROM deliveries d ORDER BY event_seq, channel"),
		consumers:  rows(t, db, "c.*)::text FROM consumers c ORDER BY name"),
	}
}

func constraints(t *testing.T, db *sql.DB, table string) []string {
	t.Helper()
	return column(t, db, "SELECT conname FROM pg_constraint WHERE conrelid IN ('"+table+"'::regclass, 'events'::regclass) AND contype IN ('p', 'f', 'u') ORDER BY conname")
}

func tables(t *testing.T, db *sql.DB) []string {
	t.Helper()
	return column(t, db, "SELECT tablename FROM pg_tables WHERE schemaname = 'public' ORDER BY tablename")
}

func expect(t *testing.T, what string, got, want any) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%s:\n got %v\nwant %v", what, got, want)
	}
}

func TestMigration00004KeepsEveryRow(t *testing.T) {
	address, db := emptyDatabase(t)
	p := seed(t, db)
	before := take(t, db, "plugins", "plugin", "plugin_seq")

	if err := schema.Migrate(t.Context(), address); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	after := take(t, db, "monitors", "monitor", "monitor_seq")
	expect(t, "monitors", after.monitors, before.monitors)
	expect(t, "events", after.events, before.events)
	expect(t, "deliveries", after.deliveries, before.deliveries)
	expect(t, "consumers", after.consumers, before.consumers)
	expect(t, "tables", tables(t, db), []string{"consumers", "deliveries", "events", "goose_db_version", "monitors"})
	expect(t, "constraints", constraints(t, db, "monitors"),
		[]string{"events_monitor_event_id_key", "events_monitor_fkey", "events_pkey", "monitors_pkey"})
	version, err := p.GetDBVersion(t.Context())
	if err != nil || version != 4 {
		t.Errorf("version = %d, %v; want 4", version, err)
	}

	var seq int64
	err = db.QueryRowContext(t.Context(), `INSERT INTO events (monitor, event_id, monitor_seq, kind, severity, occurred_at, title, body, url)
		VALUES ('sei', 'e3', 3, 'k', 1, now(), 't', '', '') RETURNING seq`).Scan(&seq)
	if err != nil || seq != 3 {
		t.Errorf("new event seq = %d, %v; want 3", seq, err)
	}

	err = db.QueryRowContext(t.Context(), `INSERT INTO events (monitor, event_id, monitor_seq, kind, severity, occurred_at, title, body, url)
		VALUES ('sei', 'e2', 9, 'k', 1, now(), 't', '', '') ON CONFLICT (monitor, event_id) DO NOTHING RETURNING seq`).Scan(&seq)
	if !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("duplicate (sei, e2): err = %v; want no row", err)
	}

	_, err = db.ExecContext(t.Context(), `INSERT INTO events (monitor, event_id, monitor_seq, kind, severity, occurred_at, title, body, url)
		VALUES ('nobody', 'e1', 1, 'k', 1, now(), 't', '', '')`)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("unknown monitor: err = %v; want *pgconn.PgError", err)
	}
	expect(t, "foreign key code", pgErr.Code, "23503")
	expect(t, "foreign key constraint", pgErr.ConstraintName, "events_monitor_fkey")
}

func TestMigration00004DownKeepsEveryRow(t *testing.T) {
	address, db := emptyDatabase(t)
	p := seed(t, db)
	before := take(t, db, "plugins", "plugin", "plugin_seq")

	if err := schema.Migrate(t.Context(), address); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if _, err := p.DownTo(t.Context(), 3); err != nil {
		t.Fatalf("migrate down: %v", err)
	}

	after := take(t, db, "plugins", "plugin", "plugin_seq")
	expect(t, "plugins", after.monitors, before.monitors)
	expect(t, "events", after.events, before.events)
	expect(t, "deliveries", after.deliveries, before.deliveries)
	expect(t, "consumers", after.consumers, before.consumers)
	expect(t, "tables", tables(t, db), []string{"consumers", "deliveries", "events", "goose_db_version", "plugins"})
	expect(t, "constraints", constraints(t, db, "plugins"),
		[]string{"events_pkey", "events_plugin_event_id_key", "events_plugin_fkey", "plugins_pkey"})
}
