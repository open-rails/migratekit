package migratekit

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// These are the v1.5.0 identity invariants. Each one reproduces a failure that
// v1.4.0 accepts SILENTLY — under v1.4.0 semantics (ledger keyed by number
// only, no filename, no digest) every one of these tests fails, because the
// migrator returns nil and the DDL never runs.

func strictTestDB(t *testing.T, app string) (*sql.DB, context.Context) {
	t.Helper()
	dsn := os.Getenv("MIGRATEKIT_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("MIGRATEKIT_TEST_DATABASE_URL not set; skipping live DB test")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	ctx := context.Background()
	clean := func() {
		_, _ = db.ExecContext(ctx, `DELETE FROM public.migrations WHERE app = $1`, app)
		_, _ = db.ExecContext(ctx, `DROP TABLE IF EXISTS `+app+`_a`)
		_, _ = db.ExecContext(ctx, `DROP TABLE IF EXISTS `+app+`_b`)
	}
	m := NewPostgres(db, app)
	if err := m.Setup(ctx); err != nil {
		t.Fatalf("setup: %v", err)
	}
	clean()
	t.Cleanup(func() { clean(); db.Close() })
	return db, ctx
}

// TestStrict_NumberReusedByDifferentFile is the th#1564 incident: lane A's
// 0002 is applied, lane B merges a DIFFERENT 0002. v1.4.0 reports "already
// applied" and B's DDL never runs, forever, with no error anywhere.
func TestStrict_NumberReusedByDifferentFile(t *testing.T) {
	const app = "mk_strict_reuse"
	db, ctx := strictTestDB(t, app)

	laneA := []Migration{{Name: "0002_lane_a.up.sql", Content: `CREATE TABLE mk_strict_reuse_a (id int)`}}
	if err := NewPostgres(db, app).ApplyMigrations(ctx, laneA); err != nil {
		t.Fatalf("lane A apply: %v", err)
	}

	laneB := []Migration{{Name: "0002_lane_b.up.sql", Content: `CREATE TABLE mk_strict_reuse_b (id int)`}}
	err := NewPostgres(db, app).ApplyMigrations(ctx, laneB)
	if err == nil {
		t.Fatal("number reuse was accepted: lane B's migration would be silently skipped forever (this is the v1.4.0 behaviour)")
	}
	for _, want := range []string{"0002_lane_b.up.sql", "0002_lane_a.up.sql", "Renumber"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error must name %q for the author to act on it; got: %v", want, err)
		}
	}
}

// TestStrict_AppliedMigrationEdited catches an edit to a migration that has
// already run. v1.4.0 cannot see it: the number is applied, so the file is
// skipped and this database silently diverges from a fresh one.
func TestStrict_AppliedMigrationEdited(t *testing.T) {
	const app = "mk_strict_edit"
	db, ctx := strictTestDB(t, app)

	orig := []Migration{{Name: "0001_init.up.sql", Content: `CREATE TABLE mk_strict_edit_a (id int)`}}
	if err := NewPostgres(db, app).ApplyMigrations(ctx, orig); err != nil {
		t.Fatalf("initial apply: %v", err)
	}

	edited := []Migration{{Name: "0001_init.up.sql", Content: `CREATE TABLE mk_strict_edit_a (id int, extra text)`}}
	err := NewPostgres(db, app).ApplyMigrations(ctx, edited)
	if err == nil {
		t.Fatal("edit to an applied migration was accepted; this database now differs from a fresh one with no signal")
	}
	if !strings.Contains(err.Error(), "EDITED") {
		t.Errorf("error should say the migration was edited; got: %v", err)
	}
}

// TestStrict_OutOfOrderRefused covers the opt-in ordering rule: a migration
// that arrives below the high-water mark builds a schema no fresh database
// reproduces, because a fresh database runs the two the other way round.
func TestStrict_OutOfOrderRefused(t *testing.T) {
	const app = "mk_strict_order"
	db, ctx := strictTestDB(t, app)

	first := []Migration{{Name: "0005_later.up.sql", Content: `CREATE TABLE mk_strict_order_a (id int)`}}
	if err := NewPostgres(db, app).WithStrictOrdering().ApplyMigrations(ctx, first); err != nil {
		t.Fatalf("first apply: %v", err)
	}

	late := []Migration{
		{Name: "0003_late_arrival.up.sql", Content: `CREATE TABLE mk_strict_order_b (id int)`},
		{Name: "0005_later.up.sql", Content: `CREATE TABLE mk_strict_order_a (id int)`},
	}
	err := NewPostgres(db, app).WithStrictOrdering().ApplyMigrations(ctx, late)
	if err == nil {
		t.Fatal("out-of-order migration was accepted under WithStrictOrdering")
	}
	if !strings.Contains(err.Error(), "0003_late_arrival.up.sql") {
		t.Errorf("error should name the offending file; got: %v", err)
	}

	// Default (non-strict) stays permissive, so existing consumers keep working.
	if err := NewPostgres(db, app).ApplyMigrations(ctx, late); err != nil {
		t.Fatalf("ordering must be opt-in, but the default path rejected a late arrival: %v", err)
	}
}

// TestStrict_LegacyRowsAreNotMismatches pins the compatibility rule: rows
// written by <=v1.4.0 carry no filename or digest, and an unknown identity
// must read as unknown, never as a violation.
func TestStrict_LegacyRowsAreNotMismatches(t *testing.T) {
	const app = "mk_strict_legacy"
	db, ctx := strictTestDB(t, app)

	// A v1.4.0-shaped row: key only, no identity columns.
	if _, err := db.ExecContext(ctx,
		`INSERT INTO public.migrations (app, database, schema, name) VALUES ($1, 'postgres', '', '1')`, app); err != nil {
		t.Fatalf("seed legacy row: %v", err)
	}

	migrations := []Migration{{Name: "0001_init.up.sql", Content: `CREATE TABLE mk_strict_legacy_a (id int)`}}
	if err := NewPostgres(db, app).ApplyMigrations(ctx, migrations); err != nil {
		t.Fatalf("legacy ledger row must not be treated as a mismatch: %v", err)
	}
}
