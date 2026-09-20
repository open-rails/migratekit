package chmigrate

import (
	"context"
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
	mock.ExpectQuery("SELECT sequence FROM public\\.migrations").
		WithArgs("doujins", "clickhouse").
		WillReturnRows(sqlmock.NewRows([]string{"sequence"}).AddRow(int64(1)).AddRow(int64(10)))

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
