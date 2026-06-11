package migratekit

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// TestLiveStaleReplicaRecovery reproduces the partial-wipe failure mode against
// a real ClickHouse + Keeper + Postgres and asserts ApplyMigrations self-heals:
//
//  1. create a ReplicatedMergeTree table, then DETACH ... PERMANENTLY — its
//     replica registration survives in Keeper with no live local table (the
//     same state a wiped ClickHouse volume + surviving Keeper volume leaves),
//  2. apply a migration that creates a table over the same znode path —
//     without recovery this dies with code 253 REPLICA_ALREADY_EXISTS,
//  3. assert the migration succeeded and the table exists.
//
// Skipped unless the live-stack env vars are set, e.g. (doujins dev stack):
//
//	MIGRATEKIT_TEST_CLICKHOUSE_ADDR=localhost:9002 \
//	MIGRATEKIT_TEST_CLICKHOUSE_USER=analytics_user \
//	MIGRATEKIT_TEST_CLICKHOUSE_PASS=analytics_password \
//	MIGRATEKIT_TEST_CLICKHOUSE_DB=analytics \
//	MIGRATEKIT_TEST_DATABASE_URL=postgres://admin:admin_password@localhost:5432/doujins_db \
//	go test -run TestLiveStaleReplicaRecovery -v
func TestLiveStaleReplicaRecovery(t *testing.T) {
	addr := os.Getenv("MIGRATEKIT_TEST_CLICKHOUSE_ADDR")
	pgDSN := os.Getenv("MIGRATEKIT_TEST_DATABASE_URL")
	if addr == "" || pgDSN == "" {
		t.Skip("MIGRATEKIT_TEST_CLICKHOUSE_ADDR / MIGRATEKIT_TEST_DATABASE_URL not set; skipping live test")
	}
	user := os.Getenv("MIGRATEKIT_TEST_CLICKHOUSE_USER")
	pass := os.Getenv("MIGRATEKIT_TEST_CLICKHOUSE_PASS")
	db := os.Getenv("MIGRATEKIT_TEST_CLICKHOUSE_DB")
	if db == "" {
		db = "default"
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	conn, err := clickhouse.Open(&clickhouse.Options{
		Addr:        []string{addr},
		Auth:        clickhouse.Auth{Database: db, Username: user, Password: pass},
		DialTimeout: 10 * time.Second,
	})
	if err != nil {
		t.Fatalf("connect clickhouse: %v", err)
	}
	defer conn.Close()

	// Unique znode path per run so reruns never collide with their own residue.
	runID := time.Now().UnixNano()
	zkPath := fmt.Sprintf("/migratekit_test/stale_replica/%d", runID)
	probe := fmt.Sprintf("mk_stale_probe_%d", runID)
	target := fmt.Sprintf("mk_stale_target_%d", runID)

	// 1) Fabricate the ghost: probe registers replica 'r1' at zkPath, then a
	// permanent detach leaves the Keeper registration behind with no live table.
	createProbe := fmt.Sprintf(
		"CREATE TABLE %s (id UInt64) ENGINE = ReplicatedMergeTree('%s', 'r1') ORDER BY id", probe, zkPath)
	if err := conn.Exec(ctx, createProbe); err != nil {
		t.Fatalf("create probe table: %v", err)
	}
	if err := conn.Exec(ctx, fmt.Sprintf("DETACH TABLE %s PERMANENTLY", probe)); err != nil {
		t.Fatalf("detach probe table: %v", err)
	}

	// 2) Apply a migration that must collide with the ghost, via the real
	// migrator (postgres tracker + lock, like the consumers use).
	pgDB, err := sql.Open("pgx", pgDSN)
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	defer pgDB.Close()

	m := NewClickHouse(&ClickHouseConfig{
		ClientAddr: addr,
		Database:   db,
		Username:   user,
		Password:   pass,
		App:        fmt.Sprintf("migratekit-live-test-%d", runID),
		PostgresDB: pgDB,
	})
	defer m.Close()

	migration := Migration{
		Name: "001_stale_replica_probe.up.sql",
		Content: fmt.Sprintf(
			"CREATE TABLE IF NOT EXISTS %s (id UInt64) ENGINE = ReplicatedMergeTree('%s', 'r1') ORDER BY id", target, zkPath),
	}
	if err := m.ApplyMigrations(ctx, []Migration{migration}); err != nil {
		t.Fatalf("ApplyMigrations should have self-healed the stale replica, got: %v", err)
	}

	// 3) The target table must exist.
	var exists uint8
	row := conn.QueryRow(ctx, fmt.Sprintf("EXISTS TABLE %s", target))
	if err := row.Scan(&exists); err != nil {
		t.Fatalf("EXISTS TABLE: %v", err)
	}
	if exists != 1 {
		t.Fatal("target table does not exist after migration")
	}

	// Cleanup (best-effort): drop target synchronously (clears its znodes),
	// re-attach the probe so it can be dropped through SQL, delete tracker rows.
	if err := conn.Exec(ctx, fmt.Sprintf("DROP TABLE IF EXISTS %s SYNC", target)); err != nil {
		t.Logf("cleanup: drop target: %v", err)
	}
	if err := conn.Exec(ctx, fmt.Sprintf("ATTACH TABLE %s", probe)); err != nil {
		t.Logf("cleanup: attach probe (may be readonly, fine): %v", err)
	}
	if err := conn.Exec(ctx, fmt.Sprintf("DROP TABLE IF EXISTS %s SYNC", probe)); err != nil {
		t.Logf("cleanup: drop probe: %v", err)
	}
	if _, err := pgDB.ExecContext(ctx,
		"DELETE FROM public.migrations WHERE app = $1", fmt.Sprintf("migratekit-live-test-%d", runID)); err != nil {
		t.Logf("cleanup: tracker rows: %v", err)
	}
}
