package migratekit

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
)

// These regressions exercise public migration operations using the same pgx
// adapter as an embedded host, with one physical connection. Every test owns
// its database, including the shared public ledger.
func singleConnectionPool(t *testing.T, admin *sql.DB, name string, tracer pgx.QueryTracer) (*sql.DB, *pgxpool.Pool) {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(os.Getenv("MIGRATEKIT_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	if err := admin.QueryRowContext(t.Context(), "SELECT current_database()").Scan(&cfg.ConnConfig.Database); err != nil {
		t.Fatal(err)
	}
	cfg.MaxConns = 1
	cfg.ConnConfig.RuntimeParams["application_name"] = name
	cfg.ConnConfig.RuntimeParams["search_path"] = "public"
	cfg.ConnConfig.Tracer = tracer
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	db := stdlib.OpenDBFromPool(pool)
	db.SetMaxOpenConns(1)
	t.Cleanup(func() {
		db.Close()
		pool.Close()
	})
	return db, pool
}

// Keep one idle physical connection, unlike OpenDBFromPool's zero-idle SQL
// adapter. This exposes poisoned sessions accidentally returned to a pool.
func singleConnectionSQLPool(t *testing.T, admin *sql.DB, name, searchPath string) *sql.DB {
	t.Helper()
	cfg, err := pgx.ParseConfig(os.Getenv("MIGRATEKIT_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	if err := admin.QueryRowContext(t.Context(), "SELECT current_database()").Scan(&cfg.Database); err != nil {
		t.Fatal(err)
	}
	cfg.RuntimeParams["application_name"] = name
	cfg.RuntimeParams["search_path"] = searchPath
	db := stdlib.OpenDB(*cfg)
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	t.Cleanup(func() { db.Close() })
	return db
}

func assertSQLConnectionRecovered(t *testing.T, ctx context.Context, admin, db *sql.DB, oldPID int, searchPath string) int {
	t.Helper()
	var path string
	var pid int
	if err := db.QueryRowContext(ctx, "SELECT current_setting('search_path'), pg_backend_pid()").Scan(&path, &pid); err != nil || path != searchPath {
		t.Fatalf("pool session not recovered: path=%q want=%q err=%v", path, searchPath, err)
	}
	var locked bool
	if err := admin.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM pg_locks WHERE pid=$1 AND locktype='advisory')", oldPID).Scan(&locked); err != nil || locked {
		t.Fatalf("failed session retained advisory locks: locked=%v err=%v", locked, err)
	}
	if err := NewPostgres(db, "after_failure").WithSchema("recovered_data").ApplyMigrations(ctx,
		[]Migration{{Name: "0001_after.up.sql", Content: "CREATE TABLE after_failure (id int)"}}); err != nil {
		t.Fatalf("same one-connection pool cannot migrate after failure: %v", err)
	}
	return pid
}

func singleConnectionContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestPostgresSingleConnectionFreshReplay(t *testing.T) {
	admin := freshNumericDatabase(t)
	db, pool := singleConnectionPool(t, admin, "mk11_fresh", nil)
	ctx := singleConnectionContext(t)
	migrations := []Migration{
		{Name: "0001_rows.up.sql", Content: "CREATE TABLE rows (id int PRIMARY KEY, label text); INSERT INTO rows VALUES (1, 'once')"},
		{Name: "0002_index.up.sql", Content: "-- migratekit:no-transaction\nCREATE INDEX CONCURRENTLY rows_label_idx ON rows(label)"},
		{Name: "0003_more.up.sql", Content: "INSERT INTO rows VALUES (2, 'once')"},
	}
	p := NewPostgres(db, "single").WithSchema("app_data")
	for i := 0; i < 2; i++ {
		if err := p.ApplyMigrations(ctx, migrations); err != nil {
			t.Fatalf("apply/replay %d with one connection: %v", i, err)
		}
	}
	if err := p.ValidateAllApplied(ctx, migrations); err != nil {
		t.Fatal(err)
	}
	var rows, records int
	if err := admin.QueryRowContext(ctx, "SELECT count(*) FROM app_data.rows").Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if err := admin.QueryRowContext(ctx, "SELECT count(*) FROM public.migrations WHERE app='single'").Scan(&records); err != nil {
		t.Fatal(err)
	}
	if rows != 2 || records != 3 {
		t.Fatalf("rows=%d records=%d; want 2/3 after replay", rows, records)
	}
	assertSingleConnectionAvailable(t, ctx, db, pool)
}

func TestPostgresSingleConnectionDedicatedAdapter(t *testing.T) {
	admin := freshNumericDatabase(t)
	db, pool := singleConnectionPool(t, admin, "mk11_dedicated", nil)
	ctx := singleConnectionContext(t)
	// Exhaust the host pool: the migration adapter must own a separate session,
	// without changing the host pool's limit or acquisition lifecycle.
	host, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer host.Release()
	p, err := NewPostgresFromPGXPool(pool, "dedicated")
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	p.WithSchema("app_data")
	ms := []Migration{{Name: "0001_owned.up.sql", Content: "CREATE TABLE owned (id int)"}}
	if err := p.ApplyMigrations(ctx, ms); err != nil {
		t.Fatal(err)
	}
	if err := p.ValidateAllApplied(ctx, ms); err != nil {
		t.Fatal(err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	if pool.Stat().AcquiredConns() != 1 || pool.Config().MaxConns != 1 {
		t.Fatal("dedicated adapter used or reconfigured the host pool")
	}
	host.Release()
	assertSingleConnectionAvailable(t, ctx, db, pool)
}

func assertSingleConnectionAvailable(t *testing.T, ctx context.Context, db *sql.DB, pool *pgxpool.Pool) {
	t.Helper()
	var searchPath string
	if err := db.QueryRowContext(ctx, "SHOW search_path").Scan(&searchPath); err != nil {
		t.Fatalf("connection was not returned: %v", err)
	}
	if searchPath != "public" {
		t.Fatalf("migration leaked search_path %q into host pool", searchPath)
	}
	if pool.Stat().AcquiredConns() != 0 || pool.Stat().TotalConns() > 1 {
		t.Fatalf("connection not released or one-connection limit exceeded: %v", pool.Stat())
	}
}

// The driver trace gates both appliers AFTER their first ledger reads but
// BEFORE the session advisory lock. Releasing the designated winner first
// makes the loser's second identity/ordering check necessary, rather than
// relying on scheduler timing to happen to exercise it.
type migrationLockGate struct {
	ready, release, acquired chan struct{}
	readyOnce, acquiredOnce  sync.Once
}

type migrationLockTraceKey struct{}

func newMigrationLockGate() *migrationLockGate {
	return &migrationLockGate{ready: make(chan struct{}), release: make(chan struct{}), acquired: make(chan struct{})}
}

func (g *migrationLockGate) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if !strings.HasPrefix(data.SQL, "SELECT pg_advisory_lock(") {
		return ctx
	}
	g.readyOnce.Do(func() { close(g.ready) })
	select {
	case <-g.release:
	case <-ctx.Done():
	}
	return context.WithValue(ctx, migrationLockTraceKey{}, g)
}

func (g *migrationLockGate) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	if ctx.Value(migrationLockTraceKey{}) == g && data.Err == nil {
		g.acquiredOnce.Do(func() { close(g.acquired) })
	}
}

func waitMigrationSignal(t *testing.T, ctx context.Context, signal <-chan struct{}, description string) {
	t.Helper()
	select {
	case <-signal:
	case <-ctx.Done():
		t.Fatalf("waiting for %s: %v", description, ctx.Err())
	}
}

func waitMigrationResult(t *testing.T, ctx context.Context, results <-chan error) error {
	t.Helper()
	select {
	case err := <-results:
		return err
	case <-ctx.Done():
		t.Fatalf("migration did not finish: %v", ctx.Err())
		return ctx.Err()
	}
}

func TestPostgresSingleConnectionCompetingMigrators(t *testing.T) {
	for _, kind := range []string{"same migration executes once", "different filename is refused", "lower sequence is refused"} {
		t.Run(kind, func(t *testing.T) {
			admin := freshNumericDatabase(t)
			ctx := singleConnectionContext(t)
			base := Migration{Name: "0000_base.up.sql", Content: "CREATE TABLE results (value text)"}
			if err := NewPostgres(admin, "race").WithSchema("app_data").ApplyMigrations(ctx, []Migration{base}); err != nil {
				t.Fatal(err)
			}
			loser := Migration{Name: "0001_winner.up.sql", Content: "INSERT INTO results VALUES ('once')"}
			winner := loser
			switch kind {
			case "different filename is refused":
				loser = Migration{Name: "0001_conflict.up.sql", Content: "CREATE TABLE forbidden (id int)"}
			case "lower sequence is refused":
				loser = Migration{Name: "0003_early.up.sql", Content: "CREATE TABLE forbidden (id int)"}
				winner.Name = "0005_late.up.sql"
			}
			ga, gb := newMigrationLockGate(), newMigrationLockGate()
			da, pa := singleConnectionPool(t, admin, "mk11_race_loser", ga)
			db, pb := singleConnectionPool(t, admin, "mk11_race_winner", gb)
			a, b := make(chan error, 1), make(chan error, 1)
			loserChain := []Migration{base, loser}
			if kind == "lower sequence is refused" {
				loserChain = append(loserChain, winner)
			}
			go func() {
				a <- NewPostgres(da, "race").WithSchema("app_data").WithStrictOrdering().ApplyMigrations(ctx, loserChain)
			}()
			go func() {
				b <- NewPostgres(db, "race").WithSchema("app_data").WithStrictOrdering().ApplyMigrations(ctx, []Migration{base, winner})
			}()
			waitMigrationSignal(t, ctx, ga.ready, "loser pre-lock read")
			waitMigrationSignal(t, ctx, gb.ready, "winner pre-lock read")
			close(gb.release)
			waitMigrationSignal(t, ctx, gb.acquired, "winner lock acquisition")
			close(ga.release)
			if err := waitMigrationResult(t, ctx, b); err != nil {
				t.Fatalf("winner: %v", err)
			}
			err := waitMigrationResult(t, ctx, a)
			if kind == "same migration executes once" {
				if err != nil {
					t.Fatalf("same-identity replica: %v", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), loser.Name) {
				t.Fatalf("loser should refuse %s under lock, got %v", loser.Name, err)
			}
			var n int
			var forbidden bool
			if err := admin.QueryRowContext(ctx, "SELECT count(*) FROM app_data.results").Scan(&n); err != nil {
				t.Fatal(err)
			}
			if err := admin.QueryRowContext(ctx, "SELECT to_regclass('app_data.forbidden') IS NOT NULL").Scan(&forbidden); err != nil {
				t.Fatal(err)
			}
			if n != 1 || forbidden {
				t.Fatalf("winner executes exactly once and loser DDL never runs: rows=%d forbidden=%v", n, forbidden)
			}
			assertSingleConnectionAvailable(t, ctx, da, pa)
			assertSingleConnectionAvailable(t, ctx, db, pb)
		})
	}
}

func waitMigrationActivity(t *testing.T, ctx context.Context, admin *sql.DB, app, event string) {
	t.Helper()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		var ready bool
		if err := admin.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE application_name=$1 AND wait_event=$2)", app, event).Scan(&ready); err != nil {
			t.Fatal(err)
		}
		if ready {
			return
		}
		select {
		case <-tick.C:
		case <-ctx.Done():
			t.Fatalf("waiting for %s/%s: %v", app, event, ctx.Err())
		}
	}
}

func TestPostgresSingleConnectionCancellation(t *testing.T) {
	t.Run("during transaction rolls back and releases session", func(t *testing.T) {
		admin := freshNumericDatabase(t)
		db, pool := singleConnectionPool(t, admin, "mk11_cancel_ddl", nil)
		ctx := singleConnectionContext(t)
		cancelled, cancel := context.WithCancel(ctx)
		defer cancel()
		p := NewPostgres(db, "cancel").WithSchema("app_data")
		result := make(chan error, 1)
		go func() {
			result <- p.ApplyMigrations(cancelled, []Migration{{Name: "0001_body.up.sql", Content: "CREATE TABLE atomic_rows (id int); SELECT pg_sleep(60)"}})
		}()
		waitMigrationActivity(t, ctx, admin, "mk11_cancel_ddl", "PgSleep")
		cancel()
		if err := waitMigrationResult(t, ctx, result); err == nil {
			t.Fatal("cancelled migration succeeded")
		}
		var clean bool
		if err := admin.QueryRowContext(ctx, "SELECT to_regclass('app_data.atomic_rows') IS NULL AND NOT EXISTS(SELECT 1 FROM public.migrations WHERE app='cancel')").Scan(&clean); err != nil || !clean {
			t.Fatalf("cancelled transactional DDL and ledger were not rolled back: clean=%v err=%v", clean, err)
		}
		if err := p.ApplyMigrations(ctx, []Migration{{Name: "0001_body.up.sql", Content: "CREATE TABLE atomic_rows (id int)"}}); err != nil {
			t.Fatalf("same handle cannot migrate after cancellation: %v", err)
		}
		assertSingleConnectionAvailable(t, ctx, db, pool)
	})

	t.Run("during nontransactional work retains dirty evidence and recovers session", func(t *testing.T) {
		admin := freshNumericDatabase(t)
		const path = "pg_catalog, public"
		db := singleConnectionSQLPool(t, admin, "mk11_cancel_no_tx", path)
		ctx := singleConnectionContext(t)
		var pid int
		if err := db.QueryRowContext(ctx, "SELECT pg_backend_pid()").Scan(&pid); err != nil {
			t.Fatal(err)
		}
		cancelled, cancel := context.WithCancel(ctx)
		defer cancel()
		ms := []Migration{{Name: "0001_partial.up.sql", Content: "-- migratekit:no-transaction\nCREATE TABLE partial (id int); SELECT pg_sleep(60)"}}
		p := NewPostgres(db, "cancel_partial").WithSchema("app_data")
		result := make(chan error, 1)
		go func() { result <- p.ApplyMigrations(cancelled, ms) }()
		waitMigrationActivity(t, ctx, admin, "mk11_cancel_no_tx", "PgSleep")
		cancel()
		if err := waitMigrationResult(t, ctx, result); err == nil {
			t.Fatal("cancelled nontransactional migration succeeded")
		}
		var state string
		var partial bool
		if err := admin.QueryRowContext(ctx, "SELECT status FROM public.migrations WHERE app='cancel_partial'").Scan(&state); err != nil || state != StatusRunning && state != StatusFailed {
			t.Fatalf("cancelled nontransactional migration lost dirty evidence: state=%q err=%v", state, err)
		}
		if err := admin.QueryRowContext(ctx, "SELECT to_regclass('app_data.partial') IS NOT NULL").Scan(&partial); err != nil || !partial {
			t.Fatalf("nontransactional DDL should remain applied: exists=%v err=%v", partial, err)
		}
		assertSQLConnectionRecovered(t, ctx, admin, db, pid, path)
		if err := p.ApplyMigrations(ctx, ms); err == nil || !strings.Contains(err.Error(), state) {
			t.Fatalf("dirty migration must refuse replay after cancellation: %v", err)
		}
	})

	t.Run("while waiting for advisory lock releases connection", func(t *testing.T) {
		admin := freshNumericDatabase(t)
		ctx := singleConnectionContext(t)
		control, err := admin.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer control.Close()
		key := time.Now().UnixNano()
		if _, err := control.ExecContext(ctx, "SELECT pg_advisory_lock($1)", key); err != nil {
			t.Fatal(err)
		}
		defer control.ExecContext(context.Background(), "SELECT pg_advisory_unlock($1)", key)
		ga, gb := newMigrationLockGate(), newMigrationLockGate()
		da, pa := singleConnectionPool(t, admin, "mk11_cancel_waiter", ga)
		db, pb := singleConnectionPool(t, admin, "mk11_cancel_holder", gb)
		cancelled, cancel := context.WithCancel(ctx)
		defer cancel()
		a, b := make(chan error, 1), make(chan error, 1)
		ms := []Migration{{Name: "0001_once.up.sql", Content: fmt.Sprintf("CREATE TABLE once (id int); SELECT pg_advisory_xact_lock(%d)", key)}}
		p := NewPostgres(da, "wait").WithSchema("app_data")
		go func() { a <- p.ApplyMigrations(cancelled, ms) }()
		go func() { b <- NewPostgres(db, "wait").WithSchema("app_data").ApplyMigrations(ctx, ms) }()
		waitMigrationSignal(t, ctx, ga.ready, "waiter pre-lock read")
		waitMigrationSignal(t, ctx, gb.ready, "holder pre-lock read")
		close(gb.release)
		waitMigrationSignal(t, ctx, gb.acquired, "holder advisory lock")
		close(ga.release)
		waitMigrationActivity(t, ctx, admin, "mk11_cancel_waiter", "advisory")
		cancel()
		if err := waitMigrationResult(t, ctx, a); err == nil {
			t.Fatal("cancelled lock waiter succeeded")
		}
		if _, err := control.ExecContext(ctx, "SELECT pg_advisory_unlock($1)", key); err != nil {
			t.Fatal(err)
		}
		if err := waitMigrationResult(t, ctx, b); err != nil {
			t.Fatalf("holder: %v", err)
		}
		if err := p.ApplyMigrations(ctx, ms); err != nil {
			t.Fatalf("cancelled waiter cannot replay after holder completes: %v", err)
		}
		assertSingleConnectionAvailable(t, ctx, da, pa)
		assertSingleConnectionAvailable(t, ctx, db, pb)
	})
}

func TestPostgresSingleConnectionNoTransactionFailure(t *testing.T) {
	t.Run("ordinary SQL failure restores exact path", func(t *testing.T) {
		admin := freshNumericDatabase(t)
		const path = "pg_catalog, public"
		db := singleConnectionSQLPool(t, admin, "mk11_no_tx_fail", path)
		ctx := singleConnectionContext(t)
		var pid int
		if err := db.QueryRowContext(ctx, "SELECT pg_backend_pid()").Scan(&pid); err != nil {
			t.Fatal(err)
		}
		ms := []Migration{{Name: "0001_partial.up.sql", Content: "-- migratekit:no-transaction\nCREATE TABLE partial (id int); SELECT missing_column FROM partial"}}
		p := NewPostgres(db, "partial").WithSchema("app_data")
		if err := p.ApplyMigrations(ctx, ms); err == nil {
			t.Fatal("broken no-transaction migration succeeded")
		}
		records, err := p.AppliedRecords(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if records["1"].Status != StatusFailed || records["1"].Error == "" {
			t.Fatalf("partial migration lost dirty evidence: %+v", records)
		}
		var partial bool
		if err := admin.QueryRowContext(ctx, "SELECT to_regclass('app_data.partial') IS NOT NULL").Scan(&partial); err != nil || !partial {
			t.Fatalf("no-transaction DDL should remain partially applied: exists=%v err=%v", partial, err)
		}
		if err := p.ApplyMigrations(ctx, ms); err == nil || !strings.Contains(err.Error(), "failed") {
			t.Fatalf("dirty migration should block replay, got %v", err)
		}
		if got := assertSQLConnectionRecovered(t, ctx, admin, db, pid, path); got != pid {
			t.Fatal("healthy session was discarded instead of restoring its exact search_path")
		}
	})
	t.Run("explicit transaction failure discards lock session", func(t *testing.T) {
		for _, scope := range []struct{ name, schema string }{{"schema-scoped reset failure", "app_data"}, {"unscoped unlock failure", ""}} {
			t.Run(scope.name, func(t *testing.T) {
				admin := freshNumericDatabase(t)
				const path = "pg_catalog, public"
				db := singleConnectionSQLPool(t, admin, "mk11_poisoned_no_tx", path)
				ctx := singleConnectionContext(t)
				var pid int
				if err := db.QueryRowContext(ctx, "SELECT pg_backend_pid()").Scan(&pid); err != nil {
					t.Fatal(err)
				}
				ms := []Migration{{Name: "0001_poison.up.sql", Content: "-- migratekit:no-transaction\nCREATE TABLE public.partial (id int); BEGIN; SELECT 1/0"}}
				p := NewPostgres(db, "poison").WithSchema(scope.schema)
				err := p.ApplyMigrations(ctx, ms)
				var pgErr *pgconn.PgError
				if !errors.As(err, &pgErr) || pgErr.Code != "22012" {
					t.Fatalf("original division-by-zero error identity was not preserved: %v", err)
				}
				var state string
				if err := admin.QueryRowContext(ctx, "SELECT status FROM public.migrations WHERE app='poison'").Scan(&state); err != nil || state != StatusRunning && state != StatusFailed {
					t.Fatalf("partial migration lost dirty evidence: state=%q err=%v", state, err)
				}
				if got := assertSQLConnectionRecovered(t, ctx, admin, db, pid, path); got == pid {
					t.Fatal("poisoned session was pooled instead of physically discarded")
				}
				if err := p.ApplyMigrations(ctx, ms); err == nil || !strings.Contains(err.Error(), state) {
					t.Fatalf("dirty migration must refuse replay after poisoned session cleanup: %v", err)
				}
			})
		}
	})
	t.Run("uncertain unlock discards an idle nested pgxpool session", func(t *testing.T) {
		admin := freshNumericDatabase(t)
		db, pool := singleConnectionPool(t, admin, "mk11_nested_pool", nil)
		ctx := singleConnectionContext(t)
		// Fault-inject an unconfirmed unlock while the driver session is idle,
		// not aborted: pgxpool.Release would otherwise recycle it. This owned
		// function is visible only to the one session whose path we change.
		if _, err := admin.ExecContext(ctx, "CREATE SCHEMA unlock_fault; CREATE FUNCTION unlock_fault.pg_advisory_unlock(bigint) RETURNS boolean LANGUAGE sql AS 'SELECT false'"); err != nil {
			t.Fatal(err)
		}
		var oldPID int
		if err := db.QueryRowContext(ctx, "SELECT pg_backend_pid()").Scan(&oldPID); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, "SET search_path = unlock_fault, pg_catalog, public"); err != nil {
			t.Fatal(err)
		}
		p := NewPostgres(db, "uncertain_unlock").WithSchema("app_data")
		ms := []Migration{{Name: "0001_rows.up.sql", Content: "-- migratekit:no-transaction\nCREATE TABLE rows (id int)"}}
		if err := p.ApplyMigrations(ctx, ms); err == nil || !strings.Contains(err.Error(), "advisory lock was not held") {
			t.Fatalf("expected injected unconfirmed unlock, got %v", err)
		}
		assertSingleConnectionAvailable(t, ctx, db, pool)
		var newPID int
		if err := db.QueryRowContext(ctx, "SELECT pg_backend_pid()").Scan(&newPID); err != nil || newPID == oldPID {
			t.Fatalf("nested pool recycled uncertain session: old=%d new=%d err=%v", oldPID, newPID, err)
		}
		var locked bool
		if err := admin.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM pg_locks WHERE pid=$1 AND locktype='advisory')", oldPID).Scan(&locked); err != nil || locked {
			t.Fatalf("discarded nested session retained advisory lock: locked=%v err=%v", locked, err)
		}
		if err := p.ValidateAllApplied(ctx, ms); err != nil {
			t.Fatalf("successful DDL/ledger commit was lost during cleanup: %v", err)
		}
		if err := NewPostgres(db, "after_unlock").WithSchema("next_data").ApplyMigrations(ctx,
			[]Migration{{Name: "0001_next.up.sql", Content: "CREATE TABLE next (id int)"}}); err != nil {
			t.Fatalf("same nested pool cannot migrate after uncertain unlock: %v", err)
		}
	})
}

func TestPostgresSingleConnectionLedgerOperations(t *testing.T) {
	t.Run("baseline", func(t *testing.T) {
		clearCI(t)
		admin := freshNumericDatabase(t)
		db, pool := singleConnectionPool(t, admin, "mk11_baseline", nil)
		ctx := singleConnectionContext(t)
		p := NewPostgres(db, "baseline").WithSchema("app_data")
		ms := []Migration{{Name: "0001_preexisting.up.sql", Content: "CREATE TABLE preexisting (id int)"}}
		if _, err := admin.ExecContext(ctx, "CREATE SCHEMA app_data; CREATE TABLE app_data.preexisting (id int)"); err != nil {
			t.Fatal(err)
		}
		results, err := p.Baseline(ctx, ms, BaselineRequest{SchemaVerified: true, RepairRequest: RepairRequest{Reason: "verified owned test schema"}})
		if err != nil || len(results) != 1 {
			t.Fatalf("baseline: results=%v err=%v", results, err)
		}
		if err := p.ApplyMigrations(ctx, ms); err != nil {
			t.Fatal(err)
		}
		assertSingleConnectionAvailable(t, ctx, db, pool)
	})
	t.Run("conversion and strict integrity", func(t *testing.T) {
		admin := freshNumericDatabase(t)
		db, pool := singleConnectionPool(t, admin, "mk11_conversion", nil)
		ctx := singleConnectionContext(t)
		p := NewPostgres(db, "conversion").WithSchema("app_data")
		old := []Migration{{Name: "0001_schema.up.sql", Content: "CREATE TABLE rows (id int)"}}
		current := []Migration{{Name: "0001_schema.up.sql", Content: "CREATE TABLE rows (id int, label text)"}}
		if err := p.ApplyMigrations(ctx, old); err != nil {
			t.Fatal(err)
		}
		p.WithConversions(Conversion{Name: "add-label", Replaces: 1,
			Retired: func(string) ([]Migration, error) { return old, nil },
			SQL:     func(string) (string, error) { return "ALTER TABLE rows ADD COLUMN label text", nil },
		})
		if err := p.ApplyMigrations(ctx, current); err != nil {
			t.Fatalf("conversion on one session: %v", err)
		}
		// An index rename is a genuine byte/token edit with identical schema
		// semantics, so strict integrity must enter its locked verification path.
		indexed := append(current, Migration{Name: "0002_index.up.sql", Content: "CREATE INDEX before_idx ON rows(label)"})
		if err := p.ApplyMigrations(ctx, indexed); err != nil {
			t.Fatal(err)
		}
		edited := append([]Migration(nil), indexed...)
		edited[1].Content = "CREATE INDEX after_idx ON rows(label)"
		if err := p.WithStrictIntegrity().ApplyMigrations(ctx, edited); err != nil {
			t.Fatalf("strict integrity on one session: %v", err)
		}
		assertSingleConnectionAvailable(t, ctx, db, pool)
	})
}
