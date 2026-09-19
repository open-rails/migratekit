package migratekit

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestSequenceRangeAndCanonicalIdentity(t *testing.T) {
	for name, want := range map[string]int64{"0000_init.up.sql": 0, "0001_schema.up.sql": 1, "001-add_user.up.sql": 1, "9223372036854775807_max.up.sql": 9223372036854775807} {
		n, err := Sequence(name)
		if err != nil || n != want {
			t.Fatalf("%q: got %d, %v; want %d", name, n, err, want)
		}
	}
	for _, name := range []string{"", "_empty.up.sql", "init.up.sql", "+1_init.up.sql", "-0_init.up.sql", "-1_init.up.sql", " 1_init.up.sql", "9223372036854775808_overflow.up.sql"} {
		if _, err := Sequence(name); err == nil {
			t.Errorf("accepted %q", name)
		}
	}
}

func TestNumericSequenceLoadAndValidate(t *testing.T) {
	fsys := fstest.MapFS{}
	for _, name := range []string{"1_first.up.sql", "10_tenth.up.sql", "2_second.up.sql"} {
		fsys[name] = &fstest.MapFile{Data: []byte("SELECT 1;")}
	}
	ms, err := Load(fsys, ".", WithChainWarnFunc(func(string) {}))
	if err != nil {
		t.Fatal(err)
	}
	names := []string{ms[0].Name, ms[1].Name, ms[2].Name}
	if !reflect.DeepEqual(names, []string{"1_first.up.sql", "2_second.up.sql", "10_tenth.up.sql"}) {
		t.Fatal(names)
	}
	for _, name := range []string{"+3_bad.up.sql", "words_bad.up.sql", "9223372036854775808_bad.up.sql"} {
		fsys[name] = &fstest.MapFile{Data: []byte("SELECT 1;")}
		if _, err := LoadFromFS(fsys); err == nil {
			t.Errorf("loaded %q", name)
		}
		delete(fsys, name)
	}
}

func TestPostgresRejectsWholeInvalidBatchBeforeDatabaseAccess(t *testing.T) {
	for _, names := range [][]string{{"1_valid.up.sql", "oops.up.sql"}, {"1_valid.up.sql", "9223372036854775808_bad.up.sql"}, {"001_first.up.sql", "1_second.up.sql"}, {"+1_first.up.sql", "1_second.up.sql"}, {"10_later.up.sql", "2_earlier.up.sql"}} {
		t.Run(strings.Join(names, ","), func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			var ms []Migration
			for _, name := range names {
				ms = append(ms, Migration{Name: name, Content: "CREATE TABLE must_not_run (id int)"})
			}
			want := ValidateSequences(ms)
			if want == nil {
				t.Fatal("invalid fixture accepted")
			}
			err = NewPostgres(db, "numeric").ApplyMigrations(context.Background(), ms)
			if err == nil || err.Error() != want.Error() {
				t.Fatalf("got %v, want %v", err, want)
			}
			if err = mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestBaselineSupportsFullBigintRange(t *testing.T) {
	m := Migration{Name: "9223372036854775807_last.up.sql", Content: "SELECT 1"}
	plan, err := PlanBaseline(map[string]AppliedRecord{"2147483648": {Key: "2147483648", Status: StatusApplied}}, []Migration{m})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Retire) != 1 || len(plan.Record) != 1 {
		t.Fatalf("unexpected plan %+v", plan)
	}
}
