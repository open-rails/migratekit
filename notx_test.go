package migratekit

import (
	"strings"
	"testing"
)

func TestNoTransactionDirective(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want bool
	}{
		{name: "absent", body: "CREATE INDEX i ON t (c);\n"},
		{name: "alone", body: "-- migratekit:no-transaction\nCREATE INDEX CONCURRENTLY i ON t (c);\n", want: true},
		{
			name: "after other header comments",
			body: "-- parent: 4 sha256:x\n-- why: the table is live\n-- migratekit:no-transaction\nCREATE INDEX CONCURRENTLY i ON t (c);\n",
			want: true,
		},
		{name: "blank lines in the block", body: "-- one\n\n-- migratekit:no-transaction\nSELECT 1;\n", want: true},
		{name: "loose spacing", body: "--   migratekit:no-transaction  \nSELECT 1;\n", want: true},
		// Only the LEADING comment block counts, so the directive cannot hide
		// inside DDL, a string literal, or a trailing note.
		{name: "after the first statement", body: "SELECT 1;\n-- migratekit:no-transaction\nSELECT 2;\n"},
		{name: "inside a string literal", body: "INSERT INTO t VALUES ('-- migratekit:no-transaction');\n"},
		{name: "trailing prose", body: "SELECT 1;\n-- we considered -- migratekit:no-transaction here\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := hasNoTransactionDirective(tc.body); got != tc.want {
				t.Fatalf("hasNoTransactionDirective = %v, want %v", got, tc.want)
			}
		})
	}
}

// A dirty ledger row is an ERROR, and the message has to say what the operator
// is looking at: which migration, what state, and the recorded cause.
func TestAnalyze_DirtyRowIsAnError(t *testing.T) {
	migs := []Migration{{Name: "0001_a.up.sql", Content: "SELECT 1;"}}
	applied := map[string]AppliedRecord{
		"1": {Key: "1", Filename: "0001_a.up.sql", Status: StatusFailed, Error: "boom"},
	}
	ds := analyze(migs, applied, checkOptions{})
	if len(ds) != 1 {
		t.Fatalf("want one discrepancy, got %+v", ds)
	}
	if ds[0].Kind != KindDirtyMigration || ds[0].Severity != SeverityError {
		t.Fatalf("want a dirty-migration error, got %+v", ds[0])
	}
	line := ds[0].OneLine()
	for _, want := range []string{"0001_a.up.sql", "failed", "boom"} {
		if !strings.Contains(line, want) {
			t.Fatalf("message must mention %q: %s", want, line)
		}
	}
}

// A crash mid-run leaves `running`, which is also unresolved: a partial apply
// nobody watched is not more trustworthy than one that reported its error.
func TestAnalyze_RunningRowIsAlsoAnError(t *testing.T) {
	migs := []Migration{{Name: "0001_a.up.sql", Content: "SELECT 1;"}}
	applied := map[string]AppliedRecord{
		"1": {Key: "1", Filename: "0001_a.up.sql", Status: StatusRunning},
	}
	ds := analyze(migs, applied, checkOptions{})
	if len(ds) != 1 || ds[0].Severity != SeverityError {
		t.Fatalf("a running row must block the apply, got %+v", ds)
	}
}

// A legacy row (no status recorded) is applied, not dirty.
func TestAnalyze_EmptyStatusIsApplied(t *testing.T) {
	migs := []Migration{{Name: "0001_a.up.sql", Content: "SELECT 1;"}}
	applied := map[string]AppliedRecord{"1": {Key: "1", Filename: "0001_a.up.sql"}}
	if ds := analyze(migs, applied, checkOptions{}); len(ds) != 0 {
		t.Fatalf("a legacy row must not be dirty: %+v", ds)
	}
}
