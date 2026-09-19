package chmigrate

import (
	"context"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/open-rails/migratekit"
	"strings"
	"testing"
)

func TestRejectInvalidBatchBeforeTrackerOrClickHouseAccess(t *testing.T) {
	for _, names := range [][]string{{"1_valid.up.sql", "oops.up.sql"}, {"1_valid.up.sql", "9223372036854775808_bad.up.sql"}, {"001_first.up.sql", "1_second.up.sql"}, {"+1_first.up.sql", "1_second.up.sql"}, {"10_later.up.sql", "2_earlier.up.sql"}} {
		t.Run(strings.Join(names, ","), func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			c := New(&Config{PostgresDB: db, ClientAddr: "127.0.0.1:1", App: "numeric"})
			var ms []migratekit.Migration
			for _, name := range names {
				ms = append(ms, migratekit.Migration{Name: name, Content: "CREATE TABLE must_not_run (id Int64) ENGINE=Memory"})
			}
			want := migratekit.ValidateSequences(ms)
			if want == nil {
				t.Fatal("invalid fixture accepted")
			}
			err = c.ApplyMigrations(context.Background(), ms)
			if err == nil || err.Error() != want.Error() {
				t.Fatalf("got %v, want %v", err, want)
			}
			if c.conn != nil {
				t.Fatal("opened ClickHouse before validating all sequences")
			}
			if err = mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}
