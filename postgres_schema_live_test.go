package migratekit

import (
	"context"
	"database/sql"
	"os"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// Two schemas in ONE database, same app: each must get its own tables. Before
// the tracker learned about schemas, the second Apply would see the first's
// rows and skip, leaving the second schema empty. Skipped unless
// MIGRATEKIT_TEST_DATABASE_URL is set.
func TestPostgres_SchemaAwareTracking(t *testing.T) {
	dsn := os.Getenv("MIGRATEKIT_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("MIGRATEKIT_TEST_DATABASE_URL not set; skipping live DB test")
	}
	ctx := context.Background()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	const app = "mk-schema-test"
	const s1, s2, s3 = "mk_sch_a", "mk_sch_b", "mk_sch_c"
	cleanup := func() {
		for _, s := range []string{s1, s2, s3} {
			_, _ = db.ExecContext(ctx, `DROP SCHEMA IF EXISTS `+s+` CASCADE`)
		}
		_, _ = db.ExecContext(ctx, `DELETE FROM public.migrations WHERE app = $1`, app)
	}
	cleanup()
	defer cleanup()
	for _, s := range []string{s1, s2, s3} {
		if _, err := db.ExecContext(ctx, `CREATE SCHEMA `+s); err != nil {
			t.Fatalf("create schema %s: %v", s, err)
		}
	}

	// Same migration (unqualified table) applied to two schemas under one app.
	mig := []Migration{{Name: "001_widget.up.sql", Content: `CREATE TABLE widget (id int);`}}
	if err := NewPostgres(db, app).WithSchema(s1).ApplyMigrations(ctx, mig); err != nil {
		t.Fatalf("apply s1: %v", err)
	}
	if err := NewPostgres(db, app).WithSchema(s2).ApplyMigrations(ctx, mig); err != nil {
		t.Fatalf("apply s2: %v", err)
	}
	// Both schemas must physically have the table (the regression).
	for _, s := range []string{s1, s2} {
		if !tableExists(t, db, s, "widget") {
			t.Fatalf("schema %s missing widget table — tracker collided across schemas", s)
		}
	}
	// Idempotent: a second apply to s1 is a clean no-op.
	if err := NewPostgres(db, app).WithSchema(s1).ApplyMigrations(ctx, mig); err != nil {
		t.Fatalf("re-apply s1: %v", err)
	}

	// Backward compatibility: a legacy schema-less row (predates the column)
	// must be honored as applied, so the migration is NOT re-run for s3.
	if _, err := db.ExecContext(ctx,
		`INSERT INTO public.migrations (app, database, name) VALUES ($1, 'postgres', '1')`, app); err != nil {
		t.Fatalf("seed legacy row: %v", err)
	}
	if err := NewPostgres(db, app).WithSchema(s3).ApplyMigrations(ctx, mig); err != nil {
		t.Fatalf("apply s3: %v", err)
	}
	if tableExists(t, db, s3, "widget") {
		t.Fatal("s3 got the table despite a legacy applied-row — compat clause not honored")
	}
}

func tableExists(t *testing.T, db *sql.DB, schema, table string) bool {
	t.Helper()
	var ok bool
	if err := db.QueryRow(
		`SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_schema=$1 AND table_name=$2)`,
		schema, table).Scan(&ok); err != nil {
		t.Fatalf("table check %s.%s: %v", schema, table, err)
	}
	return ok
}
