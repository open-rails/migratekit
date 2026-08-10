package chmigrate

import (
	"context"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestClickHouse_PostgresTrackerMode_SkipsClickHouseTables(t *testing.T) {
	ctx := context.Background()

	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	// Setup() must succeed without touching ClickHouse migration tables when PostgresDB is provided.
	mock.ExpectExec("CREATE TABLE IF NOT EXISTS public\\.migrations").
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec("ALTER TABLE public\\.migrations ADD COLUMN IF NOT EXISTS schema").
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec("migrations_app_database_schema_name_key").
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec("ADD COLUMN IF NOT EXISTS filename").
		WillReturnResult(sqlmock.NewResult(0, 0))

	ch := New(&Config{
		ClientAddr: "invalid:0",
		Database:   "analytics",
		Username:   "user",
		Password:   "pass",
		App:        "doujins",
		PostgresDB: db,
		Cluster:    "",
	})

	if err := ch.Setup(ctx); err != nil {
		t.Fatalf("Setup: %v", err)
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

	if err := ch.Setup(ctx); err == nil {
		t.Fatalf("expected error, got nil")
	}
}
