package migratekit

import "testing"

func TestNewPostgresFromPGXPoolRequiresPool(t *testing.T) {
	p, err := NewPostgresFromPGXPool(nil, "test")
	if err == nil {
		t.Fatal("expected nil pgx pool to be rejected")
	}
	if p != nil {
		t.Fatal("expected no migrator on constructor error")
	}
}

func TestNewPostgresCloseDoesNotOwnSQLDB(t *testing.T) {
	p := NewPostgres(nil, "test")
	if err := p.Close(); err != nil {
		t.Fatalf("Close returned error for non-owned database: %v", err)
	}
}
