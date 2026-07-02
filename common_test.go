package migratekit

import (
	"strings"
	"testing"
)

func TestPrefix(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		// Original format with underscores
		{"001_create_users.up.sql", "1"},
		{"002_add_indexes.up.sql", "2"},
		{"042_add_field.up.sql", "42"},

		// Variable number of leading zeros
		{"1_create_users.up.sql", "1"},
		{"01_create_users.up.sql", "1"},
		{"0001_create_users.up.sql", "1"},

		// Hyphen separator
		{"001-create-users.up.sql", "1"},
		{"1-create-users.up.sql", "1"},
		{"42-add-feature.up.sql", "42"},

		// Down migrations
		{"001_rollback.down.sql", "1"},
		{"42-rollback.down.sql", "42"},

		// Edge cases
		{"0_init.up.sql", "0"},
		{"000_init.up.sql", "0"},
		{"100_milestone.up.sql", "100"},

		// No separator (just number)
		{"001.up.sql", "1"},
		{"42.up.sql", "42"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			result := Prefix(tt.input)
			if result != tt.expected {
				t.Errorf("Prefix(%q) = %q, want %q", tt.input, result, tt.expected)
			}
		})
	}
}

func TestSubstituteTemplates(t *testing.T) {
	t.Setenv("MKIT_SET_VAR", "value1")
	t.Setenv("MKIT_EMPTY_VAR", "")

	// Set vars substitute (both styles); explicitly-empty vars are allowed.
	got, err := substituteTemplates("a ${MKIT_SET_VAR} b {{MKIT_SET_VAR}} c {{MKIT_EMPTY_VAR}} d")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "a value1 b value1 c  d" {
		t.Fatalf("got %q", got)
	}

	// Unset vars are an error, not a silent empty substitution.
	if _, err := substituteTemplates("PASSWORD '{{MKIT_DEFINITELY_UNSET_VAR}}'"); err == nil {
		t.Fatal("expected error for unset template variable")
	} else if !strings.Contains(err.Error(), "MKIT_DEFINITELY_UNSET_VAR") {
		t.Fatalf("error should name the variable: %v", err)
	}

	// ON_CLUSTER placeholders pass through untouched; empty templates are skipped.
	got, err = substituteTemplates("CREATE TABLE t {{ON_CLUSTER}} (${ON_CLUSTER}) ${} {{}}")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "CREATE TABLE t {{ON_CLUSTER}} (${ON_CLUSTER}) ${} {{}}" {
		t.Fatalf("got %q", got)
	}
}

func TestRewriteSchemaRefs(t *testing.T) {
	in := strings.Join([]string{
		"CREATE SCHEMA IF NOT EXISTS openrails;",
		"CREATE TABLE openrails.merchants (id uuid REFERENCES openrails.merchants);",
		"DROP INDEX IF EXISTS openrails.idx_reconciliation_findings_open;",
		"GRANT USAGE ON SCHEMA openrails TO openrails_app;",
	}, "\n")

	got, err := rewriteSchemaRefs(in, "shop", []string{"openrails"})
	if err != nil {
		t.Fatalf("rewriteSchemaRefs: %v", err)
	}
	want := strings.Join([]string{
		"CREATE SCHEMA IF NOT EXISTS shop;",
		"CREATE TABLE shop.merchants (id uuid REFERENCES shop.merchants);",
		"DROP INDEX IF EXISTS shop.idx_reconciliation_findings_open;",
		"GRANT USAGE ON SCHEMA shop TO openrails_app;",
	}, "\n")
	if got != want {
		t.Fatalf("rewrite mismatch:\n got: %s\nwant: %s", got, want)
	}

	unchanged, err := rewriteSchemaRefs(in, "openrails", []string{"openrails"})
	if err != nil {
		t.Fatalf("same-schema rewrite: %v", err)
	}
	if unchanged != in {
		t.Fatalf("same-schema rewrite changed SQL:\n%s", unchanged)
	}

	if _, err := rewriteSchemaRefs(in, "bad-schema", []string{"openrails"}); err == nil {
		t.Fatal("invalid target schema should error")
	}
	if _, err := rewriteSchemaRefs(in, "shop", []string{"bad-schema"}); err == nil {
		t.Fatal("invalid source schema should error")
	}
}

func TestSplitSQL_QuoteAware(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{
			"semicolon in string literal",
			`INSERT INTO t VALUES ('a;b'); SELECT 1`,
			[]string{`INSERT INTO t VALUES ('a;b')`, `SELECT 1`},
		},
		{
			"comment markers inside strings survive",
			`INSERT INTO t VALUES ('-- not a comment', '/* not a comment */')`,
			[]string{`INSERT INTO t VALUES ('-- not a comment', '/* not a comment */')`},
		},
		{
			"real comments stripped",
			"-- leading comment\nSELECT 1; /* block */ SELECT 2 -- trailing\n;",
			[]string{"SELECT 1", "SELECT 2"},
		},
		{
			"doubled-quote escape",
			`SELECT 'it''s; fine'`,
			[]string{`SELECT 'it''s; fine'`},
		},
		{
			"backslash escape in string",
			`SELECT 'a\'; still string'; SELECT 2`,
			[]string{`SELECT 'a\'; still string'`, `SELECT 2`},
		},
		{
			"backtick identifier with semicolon",
			"SELECT `weird;col` FROM t",
			[]string{"SELECT `weird;col` FROM t"},
		},
		{
			"unterminated block comment truncates",
			"SELECT 1; /* never closed SELECT 2",
			[]string{"SELECT 1"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := splitSQL(tc.in)
			if len(got) != len(tc.want) {
				t.Fatalf("got %d statements %q, want %d %q", len(got), got, len(tc.want), tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("stmt %d = %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}
