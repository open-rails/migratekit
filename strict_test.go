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

func TestAnalyze(t *testing.T) {
	mig := func(name, content string) Migration { return Migration{Name: name, Content: content} }
	only := func(ds []Discrepancy) Discrepancy {
		t.Helper()
		if len(ds) != 1 {
			t.Fatalf("want exactly one discrepancy, got %d: %v", len(ds), ds)
		}
		return ds[0]
	}

	t.Run("number applied by a different file is an error", func(t *testing.T) {
		applied := map[string]AppliedRecord{
			"2": {Key: "2", Filename: "0002_lane_a.up.sql", Digest: ContentDigest("A")},
		}
		d := only(analyze([]Migration{mig("0002_lane_b.up.sql", "B")}, applied, checkOptions{}))
		if d.Kind != KindNumberMismatch || d.Severity != SeverityError {
			t.Fatalf("the silent-skip case must stay a hard error, got %s/%s", d.Kind, d.Severity)
		}
		for _, want := range []string{"0002_lane_a.up.sql", "0002_lane_b.up.sql", "Renumber", "repair adopt", statusHint} {
			if !contains(d.String(), want) {
				t.Errorf("refusal must mention %q; got:\n%s", want, d.String())
			}
		}
	})

	t.Run("same file same content passes", func(t *testing.T) {
		applied := map[string]AppliedRecord{
			"2": {Key: "2", Filename: "0002_lane_a.up.sql", Digest: ContentDigest("A")},
		}
		if ds := analyze([]Migration{mig("0002_lane_a.up.sql", "A")}, applied, checkOptions{}); len(ds) != 0 {
			t.Fatalf("want clean, got: %v", ds)
		}
	})

	// Paul, 2026-08-11: an operator who edited an applied migration may have
	// no way back to the old bytes, so this warns and boots. WithStrictContent
	// is the opt-in for consumers who would rather not boot.
	t.Run("edited applied migration warns by default and errors under strict content", func(t *testing.T) {
		applied := map[string]AppliedRecord{
			"2": {Key: "2", Filename: "0002_lane_a.up.sql", Digest: ContentDigest("A")},
		}
		chain := []Migration{mig("0002_lane_a.up.sql", "A-edited")}

		d := only(analyze(chain, applied, checkOptions{}))
		if d.Kind != KindContentDrift || d.Severity != SeverityWarning {
			t.Fatalf("content drift must be a WARNING by default, got %s/%s", d.Kind, d.Severity)
		}
		if firstError([]Discrepancy{d}) != nil {
			t.Fatal("a warning must not block the apply")
		}
		for _, want := range []string{"EDITED", "diverge", "repair accept-content", statusHint} {
			if !contains(d.String(), want) {
				t.Errorf("warning must mention %q; got:\n%s", want, d.String())
			}
		}

		strict := only(analyze(chain, applied, checkOptions{strictContent: true}))
		if strict.Severity != SeverityError {
			t.Fatal("WithStrictContent must restore the hard error")
		}
	})

	t.Run("legacy row without identity is not a mismatch", func(t *testing.T) {
		applied := map[string]AppliedRecord{"2": {Key: "2"}}
		if ds := analyze([]Migration{mig("0002_anything.up.sql", "whatever")}, applied, checkOptions{strictContent: true}); len(ds) != 0 {
			t.Fatalf("pre-v1.5.0 rows carry no identity and must not fail: %v", ds)
		}
	})

	t.Run("ordering is opt-in and exemptible", func(t *testing.T) {
		applied := map[string]AppliedRecord{
			"5": {Key: "5", Filename: "0005_later.up.sql", Digest: ContentDigest("L")},
		}
		chain := []Migration{mig("0003_late.up.sql", "X"), mig("0005_later.up.sql", "L")}
		if ds := analyze(chain, applied, checkOptions{}); len(ds) != 0 {
			t.Fatalf("default must stay permissive: %v", ds)
		}
		d := only(analyze(chain, applied, checkOptions{strictOrdering: true}))
		if d.Kind != KindOrderViolation || d.Severity != SeverityError {
			t.Fatalf("want an ordering error, got %s/%s", d.Kind, d.Severity)
		}
		if !contains(d.String(), "allow-below-applied") {
			t.Errorf("the ordering refusal must name the one-shot exception; got:\n%s", d.String())
		}
		exempt := analyze(chain, applied, checkOptions{strictOrdering: true, allowBelow: map[string]bool{"3": true}})
		if len(exempt) != 0 {
			t.Fatalf("an explicit exception must clear the violation, got: %v", exempt)
		}
	})

	t.Run("ledger rows with no file are reported as info", func(t *testing.T) {
		applied := map[string]AppliedRecord{
			"9": {Key: "9", Filename: "0009_gone.up.sql", Digest: ContentDigest("G")},
		}
		if ds := analyze(nil, applied, checkOptions{}); len(ds) != 0 {
			t.Fatalf("apply must not be noisy about ledger-only rows: %v", ds)
		}
		d := only(analyze(nil, applied, checkOptions{includeLedgerOnly: true}))
		if d.Kind != KindLedgerOnly || d.Severity != SeverityInfo {
			t.Fatalf("want an informational ledger-only row, got %s/%s", d.Kind, d.Severity)
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
