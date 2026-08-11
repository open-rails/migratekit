package migratekit

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// The whole point of the directive, proved both ways on a real database:
// CREATE INDEX CONCURRENTLY is refused inside a transaction (SQLSTATE 25001)
// and succeeds outside one.
func TestNoTransaction_LiveBothPaths(t *testing.T) {
	db, app := liveDB(t, "migratekit-notx")
	ctx := context.Background()

	base := Migration{Name: "0001_base.up.sql", Content: `
CREATE TABLE IF NOT EXISTS mk_notx (id BIGINT PRIMARY KEY, c TEXT);
INSERT INTO mk_notx (id, c) VALUES (1, 'a'), (2, 'b');
`}
	cic := `CREATE INDEX CONCURRENTLY mk_notx_c_idx ON mk_notx (c);`

	t.Run("inside a transaction it is refused", func(t *testing.T) {
		p := NewPostgres(db, app)
		err := p.ApplyMigrations(ctx, []Migration{base, {Name: "0002_idx.up.sql", Content: cic}})
		if err == nil {
			t.Fatal("expected Postgres to refuse CREATE INDEX CONCURRENTLY in a transaction")
		}
		if !strings.Contains(err.Error(), "CONCURRENTLY") && !strings.Contains(err.Error(), "25001") {
			t.Fatalf("unexpected error: %v", err)
		}
		// The transactional path is atomic: nothing recorded.
		if recordOf(t, db, app, "2") != nil {
			t.Fatal("a rolled-back migration must leave no ledger row")
		}
	})

	t.Run("with the directive it succeeds", func(t *testing.T) {
		p := NewPostgres(db, app)
		m := Migration{Name: "0002_idx.up.sql", Content: "-- migratekit:no-transaction\n" + cic + "\n"}
		if err := p.ApplyMigrations(ctx, []Migration{base, m}); err != nil {
			t.Fatalf("no-transaction apply failed: %v", err)
		}
		rec := recordOf(t, db, app, "2")
		if rec == nil || rec.Status != StatusApplied {
			t.Fatalf("want an applied row, got %+v", rec)
		}
		if rec.Digest != ContentDigest(m.Content) {
			t.Fatal("the digest must be recorded on success")
		}
		var n int
		if err := db.QueryRowContext(ctx,
			`SELECT count(*) FROM pg_indexes WHERE indexname = 'mk_notx_c_idx'`).Scan(&n); err != nil || n != 1 {
			t.Fatalf("the index was not created: n=%d err=%v", n, err)
		}
		// Re-applying is a no-op, not a second attempt.
		if err := p.ApplyMigrations(ctx, []Migration{base, m}); err != nil {
			t.Fatalf("re-apply must be a no-op: %v", err)
		}
	})
}

// A failed no-transaction migration may be partially applied. It must be
// recorded FAILED — not absent (which silently re-runs half-applied DDL) and
// not applied (which silently skips the other half) — and boot must refuse
// until an operator resolves it.
func TestNoTransaction_LiveFailureIsDirtyAndBlocksBoot(t *testing.T) {
	db, app := liveDB(t, "migratekit-notx-fail")
	ctx := context.Background()

	base := Migration{Name: "0001_base.up.sql", Content: "CREATE TABLE IF NOT EXISTS mk_notx_f (id BIGINT PRIMARY KEY);\n"}
	bad := Migration{Name: "0002_bad.up.sql", Content: "-- migratekit:no-transaction\nCREATE INDEX CONCURRENTLY mk_bad_idx ON mk_notx_f (no_such_column);\n"}
	set := []Migration{base, bad}

	p := NewPostgres(db, app)
	if err := p.ApplyMigrations(ctx, set); err == nil {
		t.Fatal("expected the broken migration to fail")
	}

	rec := recordOf(t, db, app, "2")
	if rec == nil {
		t.Fatal("a failed no-transaction migration must leave a ledger row, not vanish")
	}
	if rec.Status != StatusFailed {
		t.Fatalf("want status %q, got %q", StatusFailed, rec.Status)
	}
	if rec.Error == "" {
		t.Fatal("the recorded row must carry the cause")
	}

	// Boot refuses, and says what to run.
	err := NewPostgres(db, app).ApplyMigrations(ctx, set)
	if err == nil {
		t.Fatal("boot must refuse while a migration is dirty")
	}
	for _, want := range []string{"0002_bad.up.sql", "failed", "migratekit status"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("refusal must mention %q:\n%v", want, err)
		}
	}

	// ...and Status reports it.
	report, err := NewPostgres(db, app).Status(ctx, set)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if !strings.Contains(report.Report(), "0002_bad.up.sql") {
		t.Fatalf("status must report the dirty migration:\n%s", report.Report())
	}
}

func TestNoTransaction_LiveRepairResolve(t *testing.T) {
	db, app := liveDB(t, "migratekit-notx-resolve")
	ctx := context.Background()

	base := Migration{Name: "0001_base.up.sql", Content: "CREATE TABLE IF NOT EXISTS mk_notx_r (id BIGINT PRIMARY KEY);\n"}
	bad := Migration{Name: "0002_bad.up.sql", Content: "-- migratekit:no-transaction\nCREATE INDEX CONCURRENTLY mk_r_idx ON mk_notx_r (nope);\n"}
	set := []Migration{base, bad}

	if err := NewPostgres(db, app).ApplyMigrations(ctx, set); err == nil {
		t.Fatal("precondition: the migration must fail")
	}
	req := RepairRequest{Reason: "dropped the invalid index by hand; the column never existed"}

	t.Run("dry-run is inert", func(t *testing.T) {
		if _, err := NewPostgres(db, app).RepairResolve(ctx, bad, ResolveRerun,
			RepairRequest{Reason: req.Reason, DryRun: true}); err != nil {
			t.Fatalf("dry-run: %v", err)
		}
		if rec := recordOf(t, db, app, "2"); rec == nil || rec.Status != StatusFailed {
			t.Fatalf("dry-run must not change the ledger, got %+v", rec)
		}
	})

	t.Run("a reason is required", func(t *testing.T) {
		if _, err := NewPostgres(db, app).RepairResolve(ctx, bad, ResolveRerun, RepairRequest{}); err == nil {
			t.Fatal("expected a missing --reason to be refused")
		}
	})

	t.Run("--rerun deletes the row so the next boot re-runs it", func(t *testing.T) {
		if _, err := NewPostgres(db, app).RepairResolve(ctx, bad, ResolveRerun, req); err != nil {
			t.Fatalf("resolve --rerun: %v", err)
		}
		if rec := recordOf(t, db, app, "2"); rec != nil {
			t.Fatalf("--rerun must remove the row, got %+v", rec)
		}
		if !auditedVerb(t, db, app, "2", "repair resolve --rerun") {
			t.Fatal("the repair must be audited")
		}
	})

	t.Run("--applied marks it done and boot proceeds", func(t *testing.T) {
		if err := NewPostgres(db, app).ApplyMigrations(ctx, set); err == nil {
			t.Fatal("precondition: it fails again")
		}
		if _, err := NewPostgres(db, app).RepairResolve(ctx, bad, ResolveApplied, req); err != nil {
			t.Fatalf("resolve --applied: %v", err)
		}
		rec := recordOf(t, db, app, "2")
		if rec == nil || rec.Status != StatusApplied {
			t.Fatalf("want an applied row, got %+v", rec)
		}
		if err := NewPostgres(db, app).ApplyMigrations(ctx, set); err != nil {
			t.Fatalf("boot must proceed after the resolve: %v", err)
		}
	})
}

// --- helpers ----------------------------------------------------------------

func liveDB(t *testing.T, app string) (*sql.DB, string) {
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
		for _, s := range []string{
			`DROP TABLE IF EXISTS mk_notx, mk_notx_f, mk_notx_r`,
			`DELETE FROM public.migrations WHERE app = $1`,
			`DELETE FROM public.migration_repairs WHERE app = $1`,
		} {
			if strings.Contains(s, "$1") {
				_, _ = db.ExecContext(ctx, s, app)
			} else {
				_, _ = db.ExecContext(ctx, s)
			}
		}
	}
	_ = NewPostgres(db, app).Setup(ctx)
	clean()
	t.Cleanup(func() { clean(); db.Close() })
	return db, app
}

func recordOf(t *testing.T, db *sql.DB, app, key string) *AppliedRecord {
	t.Helper()
	recs, err := NewPostgres(db, app).AppliedRecords(context.Background())
	if err != nil {
		t.Fatalf("applied records: %v", err)
	}
	rec, ok := recs[key]
	if !ok {
		return nil
	}
	return &rec
}

func auditedVerb(t *testing.T, db *sql.DB, app, key, verb string) bool {
	t.Helper()
	var n int
	err := db.QueryRowContext(context.Background(),
		`SELECT count(*) FROM public.migration_repairs WHERE app = $1 AND name = $2 AND verb = $3`,
		app, key, verb).Scan(&n)
	if err != nil {
		t.Fatalf("audit query: %v", err)
	}
	return n > 0
}
