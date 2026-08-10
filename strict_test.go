package migratekit

import "testing"

func TestCheckChain(t *testing.T) {
	tests := []struct {
		name    string
		files   []string
		wantErr string
	}{
		{
			name:  "clean linear chain",
			files: []string{"0001_schema.up.sql", "0002_thing.up.sql", "0003_other.up.sql"},
		},
		{
			name:  "down files are ignored",
			files: []string{"0001_schema.up.sql", "0001_schema.down.sql", "0002_thing.up.sql"},
		},
		{
			name:    "duplicate number",
			files:   []string{"0001_schema.up.sql", "0002_lane_a.up.sql", "0002_lane_b.up.sql"},
			wantErr: "duplicate migration number",
		},
		{
			name:    "gap",
			files:   []string{"0001_schema.up.sql", "0003_thing.up.sql"},
			wantErr: "gap in migration chain",
		},
		{
			name:    "must start at one",
			files:   []string{"0002_thing.up.sql", "0003_other.up.sql"},
			wantErr: "must start at 1",
		},
		{
			name:    "non-numeric name",
			files:   []string{"0001_schema.up.sql", "20260803T121544Z-3f9a1c2b_thing.up.sql"},
			wantErr: "no numeric prefix",
		},
		{
			name:  "unsorted input is fine",
			files: []string{"0003_c.up.sql", "0001_a.up.sql", "0002_b.up.sql"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckChain(tc.files)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("want clean chain, got: %v", err)
			case tc.wantErr != "" && err == nil:
				t.Fatalf("want error containing %q, got nil", tc.wantErr)
			case tc.wantErr != "" && !contains(err.Error(), tc.wantErr):
				t.Fatalf("want error containing %q, got: %v", tc.wantErr, err)
			}
		})
	}
}

func TestVerifyIdentity(t *testing.T) {
	mig := func(name, content string) Migration { return Migration{Name: name, Content: content} }

	t.Run("number applied by a different file is refused", func(t *testing.T) {
		applied := map[string]AppliedRecord{
			"2": {Key: "2", Filename: "0002_lane_a.up.sql", Digest: ContentDigest("A")},
		}
		err := verifyIdentity([]Migration{mig("0002_lane_b.up.sql", "B")}, applied, false)
		if err == nil {
			t.Fatal("want refusal; this is the silent-skip case")
		}
	})

	t.Run("same file same content passes", func(t *testing.T) {
		applied := map[string]AppliedRecord{
			"2": {Key: "2", Filename: "0002_lane_a.up.sql", Digest: ContentDigest("A")},
		}
		if err := verifyIdentity([]Migration{mig("0002_lane_a.up.sql", "A")}, applied, false); err != nil {
			t.Fatalf("want pass, got: %v", err)
		}
	})

	t.Run("edited applied migration is refused", func(t *testing.T) {
		applied := map[string]AppliedRecord{
			"2": {Key: "2", Filename: "0002_lane_a.up.sql", Digest: ContentDigest("A")},
		}
		err := verifyIdentity([]Migration{mig("0002_lane_a.up.sql", "A-edited")}, applied, false)
		if err == nil {
			t.Fatal("want refusal for an edit to an applied migration")
		}
	})

	t.Run("legacy row without identity is not a mismatch", func(t *testing.T) {
		applied := map[string]AppliedRecord{"2": {Key: "2"}}
		if err := verifyIdentity([]Migration{mig("0002_anything.up.sql", "whatever")}, applied, false); err != nil {
			t.Fatalf("pre-v1.5.0 rows carry no identity and must not fail: %v", err)
		}
	})

	t.Run("ordering is opt-in", func(t *testing.T) {
		applied := map[string]AppliedRecord{
			"5": {Key: "5", Filename: "0005_later.up.sql", Digest: ContentDigest("L")},
		}
		chain := []Migration{mig("0003_late.up.sql", "X"), mig("0005_later.up.sql", "L")}
		if err := verifyIdentity(chain, applied, false); err != nil {
			t.Fatalf("default must stay permissive: %v", err)
		}
		if err := verifyIdentity(chain, applied, true); err == nil {
			t.Fatal("strict ordering must refuse a late arrival below the high-water mark")
		}
	})
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (haystack == needle ||
		len(needle) == 0 || indexOf(haystack, needle) >= 0)
}

func indexOf(h, n string) int {
	for i := 0; i+len(n) <= len(h); i++ {
		if h[i:i+len(n)] == n {
			return i
		}
	}
	return -1
}
