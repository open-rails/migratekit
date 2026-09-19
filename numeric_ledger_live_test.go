package migratekit

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

// Each test owns its database: bootstrap failures must be exercised before
// another test has created the shared tracker tables.
func freshNumericDatabase(t *testing.T) *sql.DB {
	t.Helper()
	raw := os.Getenv("MIGRATEKIT_TEST_DATABASE_URL")
	if raw == "" {
		t.Skip("MIGRATEKIT_TEST_DATABASE_URL not set")
	}
	cfg, err := pgx.ParseConfig(raw)
	if err != nil {
		t.Fatal(err)
	}
	admin := stdlib.OpenDB(*cfg)
	name := fmt.Sprintf("mk_numeric_%d_%d", os.Getpid(), time.Now().UnixNano())
	ident := pgx.Identifier{name}.Sanitize()
	if _, err = admin.ExecContext(t.Context(), "CREATE DATABASE "+ident); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	cfg.Database = name
	db := stdlib.OpenDB(*cfg)
	t.Cleanup(func() {
		db.Close()
		if _, err := admin.ExecContext(context.Background(), "DROP DATABASE "+ident); err != nil {
			t.Error(err)
		}
		admin.Close()
	})
	return db
}

func TestNumericLedgerFreshSchemaOrderAndAudit(t *testing.T) {
	db := freshNumericDatabase(t)
	p := NewPostgres(db, "numeric-ledger")
	var ms []Migration
	for _, name := range []string{"0000_zero.up.sql", "2_two.up.sql", "10_ten.up.sql", "9223372036854775807_max.up.sql"} {
		ms = append(ms, Migration{Name: name, Content: "SELECT 1"})
	}
	if err := p.ApplyMigrations(t.Context(), ms); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"migrations", "migration_repairs"} {
		var typ string
		if err := db.QueryRowContext(t.Context(), `SELECT data_type FROM information_schema.columns WHERE table_schema='public' AND table_name=$1 AND column_name='sequence'`, table).Scan(&typ); err != nil {
			t.Fatal(err)
		}
		if typ != "bigint" {
			t.Fatalf("%s.sequence type %q", table, typ)
		}
	}
	got, err := p.Applied(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"0", "2", "10", "9223372036854775807"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("numeric order: got %v, want %v", got, want)
	}
	if err := p.ApplyMigrations(t.Context(), ms); err != nil {
		t.Fatal(err)
	}
	clearCI(t)
	edited := ms[len(ms)-1]
	edited.Content = "SELECT 2"
	if _, err := p.RepairAcceptContent(t.Context(), edited, RepairRequest{Reason: "numeric audit test"}); err != nil {
		t.Fatal(err)
	}
	history, err := p.RepairHistory(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 1 || history[0].Key != "9223372036854775807" {
		t.Fatalf("audit: %+v", history)
	}
}

func TestNumericBootstrapRollsBackBothTables(t *testing.T) {
	db := freshNumericDatabase(t)
	// This prevents the repair-table index from being created after the first
	// CREATE TABLE. Its error must roll back the first table too.
	if _, err := db.ExecContext(t.Context(), `CREATE VIEW public.migration_repairs AS SELECT 1 AS id`); err != nil {
		t.Fatal(err)
	}
	if err := NewPostgres(db, "numeric-bootstrap").ApplyMigrations(t.Context(), nil); err == nil {
		t.Fatal("expected bootstrap failure")
	}
	var absent bool
	if err := db.QueryRowContext(t.Context(), `SELECT to_regclass('public.migrations') IS NULL`).Scan(&absent); err != nil {
		t.Fatal(err)
	}
	if !absent {
		t.Fatal("partial bootstrap left public.migrations behind")
	}
}

func TestNumericInvalidBatchLeavesFreshDatabaseUntouched(t *testing.T) {
	db := freshNumericDatabase(t)
	ms := []Migration{{Name: "1_first.up.sql", Content: "CREATE TABLE must_not_run(id int)"}, {Name: "+2_bad.up.sql", Content: "SELECT 1"}}
	if err := NewPostgres(db, "numeric-invalid").ApplyMigrations(t.Context(), ms); err == nil {
		t.Fatal("expected invalid sequence")
	}
	var clean bool
	if err := db.QueryRowContext(t.Context(), `SELECT to_regclass('public.migrations') IS NULL AND to_regclass('public.must_not_run') IS NULL`).Scan(&clean); err != nil {
		t.Fatal(err)
	}
	if !clean {
		t.Fatal("invalid batch mutated database")
	}
}
