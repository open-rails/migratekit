package chmigrate

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/open-rails/migratekit"
)

// Uses only uniquely owned target databases and app rows on a shared test stack.
func TestLiveTargetIdentity(t *testing.T) {
	addr, dsn := os.Getenv("MIGRATEKIT_TEST_CLICKHOUSE_ADDR"), os.Getenv("MIGRATEKIT_TEST_DATABASE_URL")
	if addr == "" || dsn == "" {
		t.Skip("live ClickHouse/Postgres endpoints not configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	pg, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pg.Close()
	pg.SetMaxOpenConns(1)
	user, pass := os.Getenv("MIGRATEKIT_TEST_CLICKHOUSE_USER"), os.Getenv("MIGRATEKIT_TEST_CLICKHOUSE_PASS")
	ch, err := clickhouse.Open(&clickhouse.Options{Addr: []string{addr}, Auth: clickhouse.Auth{Username: user, Password: pass}})
	if err != nil {
		t.Fatal(err)
	}
	defer ch.Close()
	app := fmt.Sprintf("mk_identity_%d", time.Now().UnixNano())
	t.Cleanup(func() {
		_, _ = pg.ExecContext(context.Background(), "DELETE FROM public.migrations WHERE app = $1", app)
	})
	newMigrator := func(db, address string) *ClickHouse {
		m := New(&Config{App: app, Database: db, ClientAddr: address, Username: user, Password: pass, PostgresDB: pg})
		t.Cleanup(func() { _ = m.Close() })
		return m
	}
	var targets []string
	for _, suffix := range []string{"a", "b"} {
		db := app + suffix
		targets = append(targets, db)
		if err := ch.Exec(ctx, "CREATE DATABASE "+db); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = ch.Exec(context.Background(), "DROP DATABASE "+db+" SYNC") })
	}
	migrations := []migratekit.Migration{{Name: "0_baseline.up.sql", Content: "CREATE TABLE proof (id UInt64) ENGINE=Memory; INSERT INTO proof VALUES (1)"}}
	a, b := newMigrator(targets[0], addr), newMigrator(targets[1], addr)
	for _, m := range []*ClickHouse{a, b} {
		if err := m.ApplyMigrations(ctx, migrations); err != nil {
			t.Fatal(err)
		}
		if err := m.ApplyMigrations(ctx, migrations); err != nil {
			t.Fatal(err)
		}
		if err := m.ValidateAllApplied(ctx, migrations); err != nil {
			t.Fatal(err)
		}
	}
	for _, db := range targets {
		var n uint64
		if err := ch.QueryRow(ctx, "SELECT count() FROM "+db+".proof").Scan(&n); err != nil || n != 1 {
			t.Fatalf("%s count=%d err=%v", db, n, err)
		}
	}
	// An alias pointing at the same server must take the same lock and re-read the ledger.
	alias := strings.Replace(addr, "127.0.0.1", "localhost", 1)
	concurrent := append(append([]migratekit.Migration{}, migrations...), migratekit.Migration{Name: "1_once.up.sql", Content: "SELECT sleep(0.3); INSERT INTO proof VALUES (2)"})
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, address := range []string{addr, alias} {
		m := newMigrator(targets[0], address)
		wg.Add(1)
		go func() { defer wg.Done(); errs <- m.ApplyMigrations(ctx, concurrent) }()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	var n uint64
	if err := ch.QueryRow(ctx, "SELECT count() FROM "+targets[0]+".proof").Scan(&n); err != nil || n != 2 {
		t.Fatalf("concurrent count=%d err=%v", n, err)
	}
	// Identity mismatches anywhere in the batch refuse both validation and all pending DDL.
	for _, variant := range []string{"filename", "content", "missing"} {
		bad := append([]migratekit.Migration{}, concurrent...)
		switch variant {
		case "filename":
			bad[1].Name = "1_replaced.up.sql"
		case "content":
			bad[1].Content += "; SELECT 2"
		case "missing":
			bad = bad[:1]
		}
		bad = append(bad, migratekit.Migration{Name: "2_never.up.sql", Content: "CREATE TABLE must_not_run (id UInt64) ENGINE=Memory"})
		if err := a.ValidateAllApplied(ctx, bad); err == nil {
			t.Fatalf("validate accepted %s drift", variant)
		}
		if err := a.ApplyMigrations(ctx, bad); err == nil {
			t.Fatalf("apply accepted %s drift", variant)
		}
	}
	var exists uint8
	if err := ch.QueryRow(ctx, "EXISTS TABLE "+targets[0]+".must_not_run").Scan(&exists); err != nil || exists != 0 {
		t.Fatalf("drift wrote DDL: %d %v", exists, err)
	}
	// A partial idempotent migration keeps identity; an explicit retry after fixing its external prerequisite succeeds.
	partial := append(append([]migratekit.Migration{}, concurrent...), migratekit.Migration{Name: "2_partial.up.sql", Content: "CREATE TABLE IF NOT EXISTS partial (id UInt64) ENGINE=Memory; SELECT throwIf((SELECT count() FROM proof) != 3)"})
	if err := a.ApplyMigrations(ctx, partial); err == nil {
		t.Fatal("expected partial failure")
	}
	if err := a.ValidateAllApplied(ctx, partial); err == nil {
		t.Fatal("incomplete migration validated")
	}
	changed := append([]migratekit.Migration{}, partial...)
	changed[2].Content += "; SELECT 1"
	if err := a.ApplyMigrations(ctx, changed); err == nil {
		t.Fatal("partial identity drift accepted")
	}
	if err := ch.Exec(ctx, "INSERT INTO "+targets[0]+".proof VALUES (3)"); err != nil {
		t.Fatal(err)
	}
	if err := a.ApplyMigrations(ctx, partial); err != nil {
		t.Fatal(err)
	}
	if err := a.ValidateAllApplied(ctx, partial); err != nil {
		t.Fatal(err)
	}
	// Unbound old history is not adopted or mistaken for a fresh target.
	if _, err := pg.ExecContext(ctx, `INSERT INTO public.migrations(app,database,sequence) VALUES($1,'clickhouse',9)`, app); err != nil {
		t.Fatal(err)
	}
	for _, m := range []*ClickHouse{a, b} {
		if err := m.ApplyMigrations(ctx, migrations); err == nil || !strings.Contains(err.Error(), "unscoped") {
			t.Fatalf("old ledger: %v", err)
		}
	}
}
