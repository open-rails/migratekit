package migratekit

import (
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"sort"
	"strings"
)

// A constraint added over a table that already has rows is a bet that the rows
// comply. When the bet loses it does not fail in CI — it fails at
// ApplyMigrations on a live database, mid-boot, and the service stays down
// until somebody hand-repairs rows through psql. The migration is always one
// line away from being safe: the DML that makes the data satisfy the
// constraint, above the constraint. Nothing asks for it.
//
// CheckRepairTotality asks for it. A constraint over a PRE-EXISTING table must
// be preceded, in the same file, by either repair DML on that table or an
// explicit `-- Repair: none-needed <reason>` waiver. A table CREATEd in the
// same file is exempt: no pre-existing row can exist.
//
// Position is the rule, not a detail. A repair below the constraint runs after
// the constraint has already refused the rows.
//
// This is a lexical scan, not a Postgres parser. The set of DDL shapes that can
// refuse stored rows is small and closed, and a lexical pass has one property a
// parser does not: when it cannot understand a statement it still SEES it. A
// statement whose table cannot be resolved is reported as UNKNOWN and requires
// the waiver — the failure direction is "ask the author", never "assume there
// is nothing here".

// ConstraintKind classifies WHY a statement can refuse rows the migration did
// not write.
type ConstraintKind string

const (
	ConstraintCheck       ConstraintKind = "add-check"           // ADD CONSTRAINT ... CHECK, validating
	ConstraintValidate    ConstraintKind = "validate-constraint" // VALIDATE CONSTRAINT <name>
	ConstraintForeignKey  ConstraintKind = "add-foreign-key"     // ADD CONSTRAINT ... FOREIGN KEY, validating
	ConstraintUnique      ConstraintKind = "add-unique"          // ADD CONSTRAINT ... UNIQUE / PRIMARY KEY
	ConstraintUniqueIndex ConstraintKind = "create-unique-index" // CREATE UNIQUE INDEX on a pre-existing table
	ConstraintNotNull     ConstraintKind = "set-not-null"        // ALTER COLUMN ... SET NOT NULL
	ConstraintColumnType  ConstraintKind = "alter-column-type"   // ALTER COLUMN ... TYPE — the cast can fail
	ConstraintNotNullCol  ConstraintKind = "add-not-null-column" // ADD COLUMN ... NOT NULL with no DEFAULT
)

// Constraint is one detected statement that must hold over rows the migration
// did not write. Table is empty when the lexical scan could not resolve it —
// that is the UNKNOWN case, and it is reported rather than dropped.
type Constraint struct {
	Line  int
	Kind  ConstraintKind
	Table string
	Name  string
}

var (
	rtCreateTable = regexp.MustCompile(`(?is)CREATE\s+TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?([\w.]+)`)
	rtAlterTable  = regexp.MustCompile(`(?is)ALTER\s+TABLE\s+(?:IF\s+EXISTS\s+)?(?:ONLY\s+)?([\w.]+)`)

	rtValidate    = regexp.MustCompile(`(?is)VALIDATE\s+CONSTRAINT\s+([\w.]+)`)
	rtAddCheck    = regexp.MustCompile(`(?is)ADD\s+CONSTRAINT\s+([\w.]+)\s+CHECK\s*`)
	rtAddFK       = regexp.MustCompile(`(?is)ADD\s+CONSTRAINT\s+([\w.]+)\s+FOREIGN\s+KEY\s*\(`)
	rtAddUnique   = regexp.MustCompile(`(?is)ADD\s+CONSTRAINT\s+([\w.]+)\s+(?:UNIQUE|PRIMARY\s+KEY)\s*\(`)
	rtUniqueIndex = regexp.MustCompile(`(?is)CREATE\s+UNIQUE\s+INDEX\s+(?:CONCURRENTLY\s+)?(?:IF\s+NOT\s+EXISTS\s+)?([\w.]+)\s+ON\s+(?:ONLY\s+)?([\w.]+)\s*\(`)
	rtSetNotNull  = regexp.MustCompile(`(?is)ALTER\s+(?:COLUMN\s+)?([\w.]+)\s+SET\s+NOT\s+NULL`)
	rtColumnType  = regexp.MustCompile(`(?is)ALTER\s+(?:COLUMN\s+)?([\w.]+)\s+(?:SET\s+DATA\s+)?TYPE\s+`)
	rtAddColumn   = regexp.MustCompile(`(?is)ADD\s+COLUMN\s+(?:IF\s+NOT\s+EXISTS\s+)?([\w.]+)\s+([^,;]*)`)
	rtNotValid    = regexp.MustCompile(`(?is)\bNOT\s+VALID\b`)
	rtNotNullWord = regexp.MustCompile(`(?is)\bNOT\s+NULL\b`)
	rtFilled      = regexp.MustCompile(`(?is)\b(DEFAULT|GENERATED)\b`)

	// Repair DML. A statement of any of these shapes, targeting the table
	// being constrained, at a lower line, is the repair.
	rtDML = regexp.MustCompile(`(?is)\b(?:DELETE\s+FROM|UPDATE|INSERT\s+INTO|MERGE\s+INTO)\s+(?:ONLY\s+)?([\w.]+)`)

	// The waiver, and only this exact shape. Bare `-- Repair: <prose>` is
	// deliberately NOT a waiver: prose promising a repair that is not in the
	// file is the failure this gate exists to catch.
	rtWaiver = regexp.MustCompile(`(?im)^\s*--\s*Repair:\s*none-needed\s+\S`)
)

// rtStripComments blanks `-- ...` tails, /* */ blocks and $$ ... $$ function
// bodies while PRESERVING byte offsets, so every match still maps to its real
// line and a DELETE inside a comment is not mistaken for a repair.
func rtStripComments(s string) string {
	b := []byte(s)
	blank := func(i int) {
		if b[i] != '\n' {
			b[i] = ' '
		}
	}
	for i := 0; i < len(b); i++ {
		switch {
		case b[i] == '-' && i+1 < len(b) && b[i+1] == '-':
			for ; i < len(b) && b[i] != '\n'; i++ {
				b[i] = ' '
			}
		case b[i] == '/' && i+1 < len(b) && b[i+1] == '*':
			for ; i < len(b); i++ {
				if b[i] == '*' && i+1 < len(b) && b[i+1] == '/' {
					b[i], b[i+1] = ' ', ' '
					i++
					break
				}
				blank(i)
			}
		case b[i] == '$' && i+1 < len(b) && b[i+1] == '$':
			// A function body is not DDL against stored rows.
			end := strings.Index(string(b[i+2:]), "$$")
			last := len(b)
			if end >= 0 {
				last = i + 2 + end + 2
			}
			for k := i; k < last; k++ {
				blank(k)
			}
			i = last - 1
		case b[i] == '\'':
			// Skip string literals so their contents cannot look like DDL.
			for i++; i < len(b); i++ {
				if b[i] == '\'' {
					break
				}
			}
		}
	}
	return string(b)
}

func rtLineOf(s string, off int) int { return 1 + strings.Count(s[:off], "\n") }

// rtStatementAt returns the text from off to the next `;` — which is what
// decides whether a constraint is added NOT VALID.
func rtStatementAt(s string, off int) string {
	if end := strings.IndexByte(s[off:], ';'); end >= 0 {
		return s[off : off+end]
	}
	return s[off:]
}

// rtTableBefore finds the table of the innermost enclosing ALTER TABLE, and is
// bounded by the previous `;`: an ALTER COLUMN belongs to the ALTER TABLE of
// its OWN statement, never to one three statements up.
func rtTableBefore(s string, off int) string {
	start := 0
	if i := strings.LastIndexByte(s[:off], ';'); i >= 0 {
		start = i + 1
	}
	locs := rtAlterTable.FindAllStringSubmatchIndex(s[start:off], -1)
	if len(locs) == 0 {
		return ""
	}
	l := locs[len(locs)-1]
	return s[start+l[2] : start+l[3]]
}

// rtSameTable compares two table references. An unqualified reference matches a
// qualified one on its last component, because a migration legitimately writes
// `DELETE FROM users` under a search_path and `ALTER TABLE app.users` beside it.
func rtSameTable(a, b string) bool {
	a, b = strings.ToLower(a), strings.ToLower(b)
	if a == b {
		return true
	}
	qa, qb := strings.Contains(a, "."), strings.Contains(b, ".")
	if qa == qb {
		return false
	}
	last := func(s string) string { return s[strings.LastIndexByte(s, '.')+1:] }
	return last(a) == last(b)
}

// rtConstraints detects every statement in code that can refuse pre-existing
// rows. created is the set of tables the file CREATEs, which are exempt.
func rtConstraints(code string, created map[string]bool) []Constraint {
	var out []Constraint
	add := func(c Constraint) {
		if c.Table != "" && created[strings.ToLower(c.Table)] {
			return // a table this file creates is provably empty
		}
		out = append(out, c)
	}

	for _, l := range rtValidate.FindAllStringSubmatchIndex(code, -1) {
		add(Constraint{Line: rtLineOf(code, l[0]), Kind: ConstraintValidate,
			Table: rtTableBefore(code, l[0]), Name: strings.ToLower(code[l[2]:l[3]])})
	}
	for _, l := range rtAddCheck.FindAllStringSubmatchIndex(code, -1) {
		if rtNotValid.MatchString(rtStatementAt(code, l[0])) {
			continue // NOT VALID asserts nothing about stored rows
		}
		add(Constraint{Line: rtLineOf(code, l[0]), Kind: ConstraintCheck,
			Table: rtTableBefore(code, l[0]), Name: strings.ToLower(code[l[2]:l[3]])})
	}
	for _, l := range rtAddFK.FindAllStringSubmatchIndex(code, -1) {
		if rtNotValid.MatchString(rtStatementAt(code, l[0])) {
			continue
		}
		add(Constraint{Line: rtLineOf(code, l[0]), Kind: ConstraintForeignKey,
			Table: rtTableBefore(code, l[0]), Name: strings.ToLower(code[l[2]:l[3]])})
	}
	for _, l := range rtAddUnique.FindAllStringSubmatchIndex(code, -1) {
		if rtNotValid.MatchString(rtStatementAt(code, l[0])) {
			continue
		}
		add(Constraint{Line: rtLineOf(code, l[0]), Kind: ConstraintUnique,
			Table: rtTableBefore(code, l[0]), Name: strings.ToLower(code[l[2]:l[3]])})
	}
	for _, l := range rtUniqueIndex.FindAllStringSubmatchIndex(code, -1) {
		add(Constraint{Line: rtLineOf(code, l[0]), Kind: ConstraintUniqueIndex,
			Table: code[l[4]:l[5]], Name: strings.ToLower(code[l[2]:l[3]])})
	}
	for _, l := range rtSetNotNull.FindAllStringSubmatchIndex(code, -1) {
		add(Constraint{Line: rtLineOf(code, l[0]), Kind: ConstraintNotNull,
			Table: rtTableBefore(code, l[0]), Name: code[l[2]:l[3]]})
	}
	for _, l := range rtColumnType.FindAllStringSubmatchIndex(code, -1) {
		add(Constraint{Line: rtLineOf(code, l[0]), Kind: ConstraintColumnType,
			Table: rtTableBefore(code, l[0]), Name: code[l[2]:l[3]]})
	}
	for _, l := range rtAddColumn.FindAllStringSubmatchIndex(code, -1) {
		tail := code[l[4]:l[5]]
		if !rtNotNullWord.MatchString(tail) || rtFilled.MatchString(tail) {
			continue // a DEFAULT (or a generated expression) fills every row
		}
		add(Constraint{Line: rtLineOf(code, l[0]), Kind: ConstraintNotNullCol,
			Table: rtTableBefore(code, l[0]), Name: code[l[2]:l[3]]})
	}

	sort.SliceStable(out, func(i, j int) bool { return out[i].Line < out[j].Line })
	return out
}

// RepairTotalityFindings returns the lint's complaints about one migration,
// empty when it is clean. Every message names the statement AND both ways to
// satisfy it, because a gate whose output does not say what to write is a gate
// people route around.
func RepairTotalityFindings(filePath, body string) []string {
	code := rtStripComments(body)

	created := map[string]bool{}
	for _, m := range rtCreateTable.FindAllStringSubmatch(code, -1) {
		created[strings.ToLower(m[1])] = true
	}

	// Waivers are comments, so they come from the RAW body; offsets are
	// preserved by rtStripComments, so the line numbers are comparable.
	var waiverLines []int
	for _, l := range rtWaiver.FindAllStringIndex(body, -1) {
		waiverLines = append(waiverLines, rtLineOf(body, l[0]))
	}

	type dml struct {
		table string
		line  int
	}
	var repairs []dml
	for _, l := range rtDML.FindAllStringSubmatchIndex(code, -1) {
		repairs = append(repairs, dml{table: code[l[2]:l[3]], line: rtLineOf(code, l[0])})
	}

	base := path.Base(filePath)
	var out []string
	for _, c := range rtConstraints(code, created) {
		waived := false
		for _, w := range waiverLines {
			if w < c.Line {
				waived = true
				break
			}
		}
		if waived {
			continue
		}
		if c.Table != "" {
			repaired := false
			for _, r := range repairs {
				if r.line < c.Line && rtSameTable(r.table, c.Table) {
					repaired = true
					break
				}
			}
			if repaired {
				continue
			}
			out = append(out, fmt.Sprintf(
				"%s:%d: %s on %s constrains rows this migration did not write, and nothing above it repairs them.\n"+
					"  Put the DML that makes %s satisfy it ABOVE the constraint — below is too late, the constraint has already refused the rows —\n"+
					"  or state why no row can violate it: `-- Repair: none-needed <reason>`.",
				base, c.Line, c.Kind, c.Table, c.Table))
			continue
		}
		out = append(out, fmt.Sprintf(
			"%s:%d: %s on an UNKNOWN table — this scan is lexical and could not resolve which table %q targets, so it cannot tell whether the rows are repaired.\n"+
				"  Qualify the statement so the table is visible, or state why no row can violate it: `-- Repair: none-needed <reason>`.",
			base, c.Line, c.Kind, c.Name))
	}
	return out
}

// CheckRepairTotality lints every *.up.sql under dir. Pure file analysis — no
// database, no configuration, no deployment names. It belongs on the merge
// boundary beside CheckChain: a database that has already applied a migration
// cannot be helped by discovering it lacked a repair.
// If dir is empty it defaults to "." (root of the filesystem).
func CheckRepairTotality(fsys fs.FS, dir string) error {
	if dir == "" {
		dir = "."
	}
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return fmt.Errorf("read dir %s: %w", dir, err)
	}
	var all []string
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".up.sql") {
			continue
		}
		p := entry.Name()
		if dir != "." {
			p = dir + "/" + entry.Name()
		}
		content, err := fs.ReadFile(fsys, p)
		if err != nil {
			return fmt.Errorf("read %s: %w", p, err)
		}
		all = append(all, RepairTotalityFindings(entry.Name(), string(content))...)
	}
	if len(all) == 0 {
		return nil
	}
	return fmt.Errorf("%d migration(s) add a constraint over pre-existing data with no repair:\n\n%s",
		len(all), strings.Join(all, "\n\n"))
}
