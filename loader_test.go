package migratekit

import (
	"strings"
	"testing"
	"testing/fstest"
)

func TestLoadFromFS_NumericOrder(t *testing.T) {
	// Unpadded prefixes must apply in numeric order: lexically "10" < "2",
	// which is exactly the bug this guards against.
	fsys := fstest.MapFS{
		"10_ten.up.sql":  {Data: []byte("SELECT 10")},
		"2_two.up.sql":   {Data: []byte("SELECT 2")},
		"1_one.up.sql":   {Data: []byte("SELECT 1")},
		"001.down.sql":   {Data: []byte("ignored")},
		"notes.md":       {Data: []byte("ignored")},
		"9_nine.up.sql":  {Data: []byte("SELECT 9")},
		"100_big.up.sql": {Data: []byte("SELECT 100")},
	}
	migrations, err := LoadFromFS(fsys)
	if err != nil {
		t.Fatalf("LoadFromFS: %v", err)
	}
	var names []string
	for _, m := range migrations {
		names = append(names, m.Name)
	}
	want := []string{"1_one.up.sql", "2_two.up.sql", "9_nine.up.sql", "10_ten.up.sql", "100_big.up.sql"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("order = %v, want %v", names, want)
	}
}

func TestLoadFromFS_ZeroPaddedOrderUnchanged(t *testing.T) {
	fsys := fstest.MapFS{
		"001_a.up.sql": {Data: []byte("a")},
		"002_b.up.sql": {Data: []byte("b")},
		"010_c.up.sql": {Data: []byte("c")},
	}
	migrations, err := LoadFromFS(fsys)
	if err != nil {
		t.Fatalf("LoadFromFS: %v", err)
	}
	if migrations[0].Name != "001_a.up.sql" || migrations[1].Name != "002_b.up.sql" || migrations[2].Name != "010_c.up.sql" {
		t.Fatalf("unexpected order: %v", migrations)
	}
}

func TestLoadFromFS_DuplicatePrefixRejected(t *testing.T) {
	cases := []fstest.MapFS{
		{
			"002_users.up.sql": {Data: []byte("a")},
			"002_roles.up.sql": {Data: []byte("b")},
		},
		{
			"0042_x.up.sql": {Data: []byte("a")},
			"42_y.up.sql":   {Data: []byte("b")},
		},
	}
	for _, fsys := range cases {
		if _, err := LoadFromFS(fsys); err == nil {
			t.Fatalf("expected duplicate-prefix error for %v, got nil", fsys)
		} else if !strings.Contains(err.Error(), "duplicate migration prefix") {
			t.Fatalf("unexpected error: %v", err)
		}
	}
}
