package examples

import (
	"bytes"
	"database/sql"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"

	"github.com/ItzTas/monitor-hub/internal/core/coretest"
)

const (
	migrateFixture  = "migrate_fixture"
	postgresFeature = "monitor-hub-plugin/postgres"
	migrationA      = "00001_a.sql"
	migrationB      = "00002_b.sql"
)

func TestMain(m *testing.M) {
	os.Exit(coretest.Run(m))
}

func TestGooseAcceptsADatabaseMigratedByRust(t *testing.T) {
	fixture := buildRustExample(t, migrateFixture, postgresFeature)
	address := emptyDatabase(t)

	if got := runFixture(t, fixture, address, migrations(t, migrationA, migrationB)); got != "1\n2\n" {
		t.Fatalf("migrate_fixture printed %q, want the versions 1 and 2", got)
	}

	provider := newProvider(t, openDatabase(t, address), migrations(t, migrationA))
	results, err := provider.Up(t.Context())
	if err != nil {
		t.Fatalf("goose up: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("goose applied %d migrations, want none", len(results))
	}
	version, err := provider.GetDBVersion(t.Context())
	if err != nil {
		t.Fatalf("goose version: %v", err)
	}
	if version != 2 {
		t.Fatalf("goose reports version %d, want 2", version)
	}
}

func TestRustContinuesADatabaseMigratedByGoose(t *testing.T) {
	fixture := buildRustExample(t, migrateFixture, postgresFeature)
	address := emptyDatabase(t)
	db := openDatabase(t, address)

	results, err := newProvider(t, db, migrations(t, migrationA)).Up(t.Context())
	if err != nil || len(results) != 1 {
		t.Fatalf("goose up with the first migration: %d results, %v", len(results), err)
	}

	if got := runFixture(t, fixture, address, migrations(t, migrationA, migrationB)); got != "2\n" {
		t.Fatalf("migrate_fixture printed %q, want only the version 2", got)
	}

	results, err = newProvider(t, db, migrations(t, migrationA, migrationB)).Up(t.Context())
	if err != nil {
		t.Fatalf("goose up with both migrations: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("goose applied %d migrations, want none", len(results))
	}
}

func TestGooseFindsADatabaseMigratedByRustFromEmptyUpToDate(t *testing.T) {
	fixture := buildRustExample(t, migrateFixture, postgresFeature)
	both := migrations(t, migrationA, migrationB)
	rustAddress := emptyDatabase(t)
	rustDB := openDatabase(t, rustAddress)
	gooseDB := openDatabase(t, emptyDatabase(t))

	runFixture(t, fixture, rustAddress, both)
	if _, err := newProvider(t, gooseDB, both).Up(t.Context()); err != nil {
		t.Fatalf("goose up on its own database: %v", err)
	}
	before := snapshot(t, rustDB)

	results, err := newProvider(t, rustDB, both).Up(t.Context())
	if err != nil {
		t.Fatalf("goose up after Rust: %v", err)
	}

	if len(results) != 0 {
		t.Fatalf("goose applied %d migrations, want none", len(results))
	}
	if after := snapshot(t, rustDB); !slices.Equal(after, before) {
		t.Fatalf("goose changed the database:\nbefore %q\nafter  %q", before, after)
	}
	if want := snapshot(t, gooseDB); !slices.Equal(before, want) {
		t.Fatalf("Rust left the database as\n%q\ngoose leaves it as\n%q", before, want)
	}
}

func TestGooseStatusReportsRustMigrationsAsApplied(t *testing.T) {
	fixture := buildRustExample(t, migrateFixture, postgresFeature)
	address := emptyDatabase(t)
	both := migrations(t, migrationA, migrationB)

	runFixture(t, fixture, address, both)

	statuses, err := newProvider(t, openDatabase(t, address), both).Status(t.Context())
	if err != nil {
		t.Fatalf("goose status: %v", err)
	}
	if len(statuses) != 2 {
		t.Fatalf("goose status lists %d migrations, want 2", len(statuses))
	}
	for _, status := range statuses {
		if status.State != goose.StateApplied || status.AppliedAt.IsZero() {
			t.Errorf("version %d: state %q, applied at %v", status.Source.Version, status.State, status.AppliedAt)
		}
	}
}

func emptyDatabase(t *testing.T) string {
	t.Helper()
	address := coretest.Database(t)
	if _, err := openDatabase(t, address).ExecContext(t.Context(), "DROP SCHEMA public CASCADE; CREATE SCHEMA public"); err != nil {
		t.Fatalf("empty the test database: %v", err)
	}
	return address
}

func openDatabase(t *testing.T, address string) *sql.DB {
	t.Helper()
	db, err := sql.Open("pgx", address)
	if err != nil {
		t.Fatalf("open the test database: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close the test database: %v", err)
		}
	})
	return db
}

func migrations(t *testing.T, names ...string) string {
	t.Helper()
	files := map[string]string{
		migrationA: "-- +goose Up\nCREATE TABLE a (id integer);\n\n-- +goose Down\nDROP TABLE a;\n",
		migrationB: "-- +goose Up\n-- +goose StatementBegin\nCREATE FUNCTION b() RETURNS integer AS $$\nBEGIN\n    RETURN 2;\nEND;\n$$ LANGUAGE plpgsql;\n-- +goose StatementEnd\n\n-- +goose Down\nDROP FUNCTION b();\n",
	}
	dir := t.TempDir()
	for _, name := range names {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(files[name]), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	return dir
}

func runFixture(t *testing.T, fixture, address, dir string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), fixture, address, dir)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("migrate_fixture: %v\n%s", err, stderr.String())
	}
	return string(out)
}

func newProvider(t *testing.T, db *sql.DB, dir string) *goose.Provider {
	t.Helper()
	locker, err := lock.NewPostgresSessionLocker()
	if err != nil {
		t.Fatalf("goose locker: %v", err)
	}
	provider, err := goose.NewProvider(goose.DialectPostgres, db, os.DirFS(dir),
		goose.WithDisableGlobalRegistry(true),
		goose.WithSessionLocker(locker),
	)
	if err != nil {
		t.Fatalf("goose provider: %v", err)
	}
	return provider
}

func snapshot(t *testing.T, db *sql.DB) []string {
	t.Helper()
	queries := []string{
		"SELECT format('table %s.%s', schemaname, tablename) FROM pg_tables WHERE schemaname NOT IN ('pg_catalog', 'information_schema') ORDER BY 1",
		"SELECT format('column %s %s not null %s default %s identity %s', a.attname, format_type(a.atttypid, a.atttypmod), a.attnotnull, pg_get_expr(d.adbin, d.adrelid), a.attidentity) FROM pg_attribute a LEFT JOIN pg_attrdef d ON d.adrelid = a.attrelid AND d.adnum = a.attnum WHERE a.attrelid = 'goose_db_version'::regclass AND a.attnum > 0 AND NOT a.attisdropped ORDER BY a.attnum",
		"SELECT format('index %s', indexdef) FROM pg_indexes WHERE tablename = 'goose_db_version' ORDER BY 1",
		"SELECT format('version %s applied %s', version_id, is_applied) FROM goose_db_version ORDER BY id",
	}
	var lines []string
	for _, query := range queries {
		lines = append(lines, queryLines(t, db, query)...)
	}
	return lines
}

func queryLines(t *testing.T, db *sql.DB, query string) []string {
	t.Helper()
	rows, err := db.QueryContext(t.Context(), query)
	if err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	defer func() {
		if err := rows.Close(); err != nil {
			t.Errorf("close rows: %v", err)
		}
	}()
	var lines []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
		lines = append(lines, line)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return lines
}
