package chmigrate

import "testing"

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
