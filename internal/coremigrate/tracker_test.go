package coremigrate

import (
	"context"
	"errors"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestEnsurePublicMigrationsTableRollsBackSetup(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	mock.ExpectBegin()
	mock.ExpectExec("SELECT pg_advisory_xact_lock\\(\\$1\\)").
		WithArgs(MigrationSetupLockKey).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("CREATE TABLE IF NOT EXISTS public\\.migrations").
		WillReturnError(errors.New("injected setup failure"))
	mock.ExpectRollback()

	if err := EnsurePublicMigrationsTable(context.Background(), db); err == nil {
		t.Fatal("expected setup failure")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("sql expectations: %v", err)
	}
}

func TestTracker_Basics(t *testing.T) {
	ctx := context.Background()

	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	tr := NewTracker(db)

	mock.ExpectBegin()
	mock.ExpectExec("SELECT pg_advisory_xact_lock\\(\\$1\\)").
		WithArgs(MigrationSetupLockKey).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("CREATE TABLE IF NOT EXISTS public\\.migrations").
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectCommit()
	if err := tr.Setup(ctx); err != nil {
		t.Fatalf("Setup: %v", err)
	}

	mock.ExpectExec("SELECT pg_advisory_lock\\(\\$1\\)").
		WithArgs(int64(123)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := tr.Lock(ctx, 123); err != nil {
		t.Fatalf("Lock: %v", err)
	}

	mock.ExpectQuery("SELECT pg_advisory_unlock\\(\\$1\\)").
		WithArgs(int64(123)).
		WillReturnRows(sqlmock.NewRows([]string{"pg_advisory_unlock"}).AddRow(true))
	if err := tr.Unlock(ctx, 123); err != nil {
		t.Fatalf("Unlock: %v", err)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("sql expectations: %v", err)
	}
}
