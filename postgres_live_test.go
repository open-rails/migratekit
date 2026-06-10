package migratekit

import (
	"context"
	"database/sql"
	"os"
	"sync"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// TestPostgres_ConcurrentApply is a live-database smoke test for the advisory
// lock. It races several "replicas" (separate pools, separate migrator
// instances) applying the same migration set. The migration body contains a
// deliberately NON-idempotent INSERT: if the lock fails to serialize
// appliers (e.g. the pre-fix bug where pg_advisory_lock/unlock ran on
// different pooled connections), the counter table ends up with more than
// one row and the test fails.
//
// Skipped unless MIGRATEKIT_TEST_DATABASE_URL is set.
func TestPostgres_ConcurrentApply(t *testing.T) {
	dsn := os.Getenv("MIGRATEKIT_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("MIGRATEKIT_TEST_DATABASE_URL not set; skipping live DB test")
	}
	ctx := context.Background()

	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer admin.Close()

	const app = "migratekit-smoketest"
	cleanup := func() {
		_, _ = admin.ExecContext(ctx, `DROP TABLE IF EXISTS mk_smoke`)
		_, _ = admin.ExecContext(ctx, `DELETE FROM public.migrations WHERE app = $1`, app)
	}
	cleanup()
	defer cleanup()

	migrations := []Migration{{
		Name: "001_smoke.up.sql",
		Content: `
			CREATE TABLE IF NOT EXISTS mk_smoke (n int);
			INSERT INTO mk_smoke VALUES (1);
		`,
	}}

	const replicas = 8
	var wg sync.WaitGroup
	errs := make([]error, replicas)
	for i := 0; i < replicas; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			db, err := sql.Open("pgx", dsn)
			if err != nil {
				errs[i] = err
				return
			}
			defer db.Close()
			errs[i] = NewPostgres(db, app).ApplyMigrations(ctx, migrations)
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("replica %d: %v", i, err)
		}
	}

	var rows int
	if err := admin.QueryRowContext(ctx, `SELECT count(*) FROM mk_smoke`).Scan(&rows); err != nil {
		t.Fatalf("count mk_smoke: %v", err)
	}
	if rows != 1 {
		t.Fatalf("migration executed %d times, want exactly 1 (advisory lock failed to serialize appliers)", rows)
	}

	var recorded int
	if err := admin.QueryRowContext(ctx, `SELECT count(*) FROM public.migrations WHERE app = $1`, app).Scan(&recorded); err != nil {
		t.Fatalf("count tracking rows: %v", err)
	}
	if recorded != 1 {
		t.Fatalf("tracking rows = %d, want 1", recorded)
	}

	// Idempotency: a rerun applies nothing (the INSERT would add a row if it ran).
	if err := NewPostgres(admin, app).ApplyMigrations(ctx, migrations); err != nil {
		t.Fatalf("rerun: %v", err)
	}
	if err := admin.QueryRowContext(ctx, `SELECT count(*) FROM mk_smoke`).Scan(&rows); err != nil {
		t.Fatalf("recount: %v", err)
	}
	if rows != 1 {
		t.Fatalf("rerun re-executed the migration (rows=%d)", rows)
	}

	// ValidateAllApplied agrees.
	if err := NewPostgres(admin, app).ValidateAllApplied(ctx, migrations); err != nil {
		t.Fatalf("ValidateAllApplied: %v", err)
	}
}

// TestPostgres_LockUnlockPinned verifies Lock/Unlock round-trip on the
// pinned connection, including the double-lock and double-unlock guards.
func TestPostgres_LockUnlockPinned(t *testing.T) {
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

	p := NewPostgres(db, "migratekit-locktest")
	if err := p.lock(ctx); err != nil {
		t.Fatalf("lock: %v", err)
	}
	if err := p.lock(ctx); err == nil {
		t.Fatal("double lock should error")
	}
	if err := p.unlock(ctx); err != nil {
		t.Fatalf("unlock: %v", err)
	}
	if err := p.unlock(ctx); err == nil {
		t.Fatal("double unlock should error")
	}

	// A cancelled context must not prevent release (Unlock uses WithoutCancel).
	if err := p.lock(ctx); err != nil {
		t.Fatalf("relock: %v", err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err := p.unlock(cancelled); err != nil {
		t.Fatalf("unlock with cancelled ctx: %v", err)
	}
}

// TestPostgres_LockSurvivesPoolInterleaving reproduces the production
// failure mode of the pre-pinning implementation: when the migrator shares
// a *sql.DB with other traffic, queries interleaving between Lock and
// Unlock cause the pool to hand Unlock a DIFFERENT connection than the one
// holding the advisory lock — Unlock then reports "advisory lock was not
// held" and the real lock leaks. With the pinned connection this can never
// happen.
func TestPostgres_LockSurvivesPoolInterleaving(t *testing.T) {
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
	db.SetMaxOpenConns(3)

	p := NewPostgres(db, "migratekit-interleavetest")

	stop := make(chan struct{})
	var churn sync.WaitGroup
	churn.Add(1)
	go func() { // concurrent application traffic on the same pool
		defer churn.Done()
		for {
			select {
			case <-stop:
				return
			default:
				_, _ = db.ExecContext(ctx, `SELECT 1`)
			}
		}
	}()

	for i := 0; i < 50; i++ {
		if err := p.lock(ctx); err != nil {
			t.Fatalf("iteration %d: lock: %v", i, err)
		}
		// Interleave pool usage while the lock is held.
		_, _ = db.ExecContext(ctx, `SELECT 1`)
		if err := p.unlock(ctx); err != nil {
			t.Fatalf("iteration %d: unlock: %v (lock/unlock landed on different pooled connections)", i, err)
		}
	}
	close(stop)
	churn.Wait()
}
