package chmigrate

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestClickHouse_AppliedInitializesPostgresTracker(t *testing.T) {
	ctx := context.Background()

	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	// A normal operation initializes the tracker without connecting to ClickHouse.
	mock.ExpectBegin()
	mock.ExpectExec("SELECT pg_advisory_xact_lock\\(\\$1\\)").
		WithArgs(int64(7592348109)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("CREATE TABLE IF NOT EXISTS public\\.migrations").
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectCommit()
	mock.ExpectQuery("SELECT sequence, .*FROM public\\.migrations").
		WithArgs("doujins", "clickhouse", "analytics").
		WillReturnRows(sqlmock.NewRows([]string{"sequence", "filename", "content_sha256", "status", "schema"}).AddRow(int64(1), "1_a.sql", "digest", "applied", "analytics").AddRow(int64(10), "10_b.sql", "digest", "applied", "analytics"))

	ch := New(&Config{
		ClientAddr: "invalid:0",
		Database:   "analytics",
		Username:   "user",
		Password:   "pass",
		App:        "doujins",
		PostgresDB: db,
		Cluster:    "",
	})

	applied, err := ch.Applied(ctx)
	if err != nil {
		t.Fatalf("Applied: %v", err)
	}
	if len(applied) != 2 || applied[0] != "1" || applied[1] != "10" {
		t.Fatalf("applied sequences: %v", applied)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("sql expectations: %v", err)
	}
}

func TestClickHouse_RequiresPostgresDB(t *testing.T) {
	ctx := context.Background()

	ch := New(&Config{
		ClientAddr: "invalid:0",
		Database:   "analytics",
		Username:   "user",
		Password:   "pass",
		App:        "doujins",
	})

	if _, err := ch.Applied(ctx); err == nil {
		t.Fatalf("expected error, got nil")
	}
}

// Startup validation may run with SELECT-only credentials and must not bootstrap.
func TestValidateAllAppliedReadOnlyMissingLedger(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.ExpectQuery("SELECT sequence, .*FROM public\\.migrations").WithArgs("app", "clickhouse", "analytics").WillReturnError(fmt.Errorf("relation public.migrations does not exist"))
	c := New(&Config{Database: "analytics", App: "app", PostgresDB: db})
	if err := c.ValidateAllApplied(context.Background(), nil); err == nil {
		t.Fatal("missing ledger accepted")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestExplicitDatabaseRequired(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	c := New(&Config{PostgresDB: db, Database: "  "})
	for _, call := range []func() error{
		func() error { return c.ApplyMigrations(context.Background(), nil) },
		func() error { return c.ValidateAllApplied(context.Background(), nil) },
	} {
		if err := call(); err == nil || !strings.Contains(err.Error(), "explicit Database") {
			t.Fatalf("got %v", err)
		}
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
