package migratekit

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// A boot refusal that only names the problem leaves the operator with psql and
// a hand-written UPDATE. Status is the other half: for every discrepancy it
// prints WHAT is wrong (file, key, both digests), the LIKELY CAUSE — usually
// more than one, and they have different fixes — and the SUGGESTED RESOLUTION,
// which is either a rename in the repository or one audited repair verb.

// Severity ranks a discrepancy.
type Severity string

const (
	// SeverityError blocks the apply.
	SeverityError Severity = "error"
	// SeverityWarning is reported and logged; the apply proceeds.
	SeverityWarning Severity = "warning"
	// SeverityInfo is worth an operator's attention but is not a defect.
	SeverityInfo Severity = "info"
)

// DiscrepancyKind names a class of ledger/tree disagreement.
type DiscrepancyKind string

const (
	// KindNumberMismatch: the ledger says this number was applied by a
	// different file. Hard error — the file in the tree would never run.
	KindNumberMismatch DiscrepancyKind = "number_applied_by_different_file"
	// KindContentDrift: an applied migration's file has been edited since it
	// ran. Warning by default; error under WithStrictContent.
	KindContentDrift DiscrepancyKind = "applied_migration_edited"
	// KindOrderViolation: a pending migration sorts below one already
	// applied. Error under WithStrictOrdering.
	KindOrderViolation DiscrepancyKind = "pending_sorts_below_applied"
	// KindLedgerOnly: a ledger row with no migration file behind it.
	KindLedgerOnly DiscrepancyKind = "ledger_row_without_file"
	// KindDirtyMigration: a no-transaction migration that failed or was
	// interrupted, so it may be partially applied. Always a hard error.
	KindDirtyMigration DiscrepancyKind = "no_transaction_migration_unfinished"
)

// Discrepancy is one disagreement between the migration files and the ledger,
// with everything an operator needs to resolve it.
type Discrepancy struct {
	Kind     DiscrepancyKind
	Severity Severity

	// Key is the ledger key (the bare migration number).
	Key string
	// File is the migration file in the tree. Empty for KindLedgerOnly.
	File string
	// LedgerFilename is the filename the ledger recorded, if any.
	LedgerFilename string
	// LedgerDigest is the content digest the ledger recorded, if any.
	LedgerDigest string
	// FileDigest is the digest of the file in the tree, if there is one.
	FileDigest string
	// Blocker is the already-applied migration a KindOrderViolation sorts
	// below.
	Blocker string
	// LedgerStatus is the recorded apply state for a KindDirtyMigration.
	LedgerStatus string
	// LedgerError is the recorded failure cause for a KindDirtyMigration.
	LedgerError string
}

// Headline is the one-sentence WHAT.
func (d Discrepancy) Headline() string {
	switch d.Kind {
	case KindNumberMismatch:
		return fmt.Sprintf("migration %s cannot be applied: number %q was already applied by a DIFFERENT file (%s)",
			d.File, d.Key, d.LedgerFilename)
	case KindContentDrift:
		return fmt.Sprintf("migration %s was EDITED after it was applied (file digest %s, ledger has %s)",
			d.File, shortDigest(d.FileDigest), shortDigest(d.LedgerDigest))
	case KindOrderViolation:
		return fmt.Sprintf("migration %s sorts BEFORE %s, which is already applied", d.File, d.Blocker)
	case KindLedgerOnly:
		who := d.LedgerFilename
		if who == "" {
			who = "an unrecorded file"
		}
		return fmt.Sprintf("ledger key %q was applied by %s, which is not in this migration directory", d.Key, who)
	case KindDirtyMigration:
		s := fmt.Sprintf("migration %s is %s, not applied: it ran OUTSIDE a transaction and may be PARTIALLY applied", d.File, d.LedgerStatus)
		if d.LedgerError != "" {
			return s + fmt.Sprintf(" (recorded cause: %s)", oneLine(d.LedgerError))
		}
		return s + " (no cause recorded — the process did not survive to write one)"
	}
	return fmt.Sprintf("%s (%s)", d.Kind, d.Key)
}

// oneLine flattens a multi-line Postgres error so it fits a log line.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// risk is the one-sentence why-you-should-care.
func (d Discrepancy) risk() string {
	switch d.Kind {
	case KindNumberMismatch:
		return "the ledger is keyed by the number, so this file would be recorded as already applied and its DDL would never run"
	case KindContentDrift:
		return "this database ran the OLD content and a fresh one will run the new, so the two schemas can diverge with nothing to notice"
	case KindOrderViolation:
		return "a fresh database runs the two in the opposite order, so this schema would be one nothing can reproduce"
	case KindLedgerOnly:
		return "the ledger is a superset of the chain, which is harmless today and a surprise later"
	case KindDirtyMigration:
		return "re-running it could repeat DDL that already landed, and skipping it would leave the rest of the file unapplied — only a human can tell which"
	}
	return ""
}

// OneLine is the whole warning in one line: what, the risk, and the verb that
// resolves it. This is what a log line has room for.
func (d Discrepancy) OneLine() string {
	s := d.Headline()
	if r := d.risk(); r != "" {
		s += " — " + r
	}
	if c := d.causes(); len(c) > 0 {
		s += ". " + c[0][1]
	}
	return s + " " + statusHint
}

// causes are the plausible explanations, each paired with its resolution.
func (d Discrepancy) causes() [][2]string {
	switch d.Kind {
	case KindNumberMismatch:
		return [][2]string{
			{
				fmt.Sprintf("two lanes claimed number %q and %s merged first — %s is the newcomer.", d.Key, d.LedgerFilename, d.File),
				fmt.Sprintf("Renumber %s to the next unclaimed number and redeploy. Nothing in the database changes.", d.File),
			},
			{
				"the LEDGER is the wrong side — a restored backup, an adopted database, or a hand-edited row. The files are right; the ledger remembers a tree that no longer exists.",
				fmt.Sprintf("migratekit repair adopt %s --reason \"...\"  (or `repair adopt --all-unmatched` when every row mismatches, which is the usual shape of a restore)", d.Key),
			},
		}
	case KindContentDrift:
		return [][2]string{
			{
				"the edit was intentional and cosmetic (a comment, whitespace, a reformat).",
				fmt.Sprintf("migratekit repair accept-content %s --reason \"...\"  — re-stamps the digest and silences this warning, on the record.", d.Key),
			},
			{
				"the edit was accidental, or it changes DDL.",
				"Revert the file to the applied content and add a NEW migration for the change. This database already ran the old bytes; a fresh one will run the new.",
			},
		}
	case KindOrderViolation:
		return [][2]string{
			{
				"a lane branched below the high-water mark and merged late.",
				fmt.Sprintf("Renumber %s above %s. This is the normal fix.", d.File, d.Blocker),
			},
			{
				"a genuine backport that must land below the high-water mark.",
				fmt.Sprintf("migratekit apply --allow-below-applied %s --reason \"...\"  — applies it once and records the deviation.", d.Key),
			},
		}
	case KindLedgerOnly:
		return [][2]string{
			{
				"the migration file was deleted or renamed, or the chain was squashed without re-basing the ledger.",
				"Nothing is broken today: the ledger is a superset. Confirm the DDL is represented in the current chain before a fresh database is built from it.",
			},
			{
				"this database is AHEAD of the checkout (an older tree pointed at a newer database).",
				"Update the checkout. Do not repair the ledger to match an old tree.",
			},
		}
	case KindDirtyMigration:
		return [][2]string{
			{
				"the DDL actually landed and the process died before the ledger caught up.",
				fmt.Sprintf("Confirm the change is present, then: migratekit repair resolve %s --applied --reason \"...\"", d.Key),
			},
			{
				"the statement failed and left the table half-changed. A failed CREATE INDEX CONCURRENTLY leaves an INVALID index behind, which keeps being maintained and is never used.",
				fmt.Sprintf("Undo the partial work first — for a failed concurrent index that is `DROP INDEX <name>` — then: migratekit repair resolve %s --rerun --reason \"...\"  (only safe if the migration is idempotent).", d.Key),
			},
		}
	}
	return nil
}

// String renders the full operator-facing explanation: what, why, and what to
// do about it, ending with the pointer at `migratekit status`.
func (d Discrepancy) String() string { return d.explain(true) }

// explain renders the same thing; hint is false when the reader is already
// looking at `migratekit status` output.
func (d Discrepancy) explain(hint bool) string {
	var b strings.Builder
	b.WriteString(d.Headline())
	b.WriteString(".\n")
	switch d.Kind {
	case KindNumberMismatch:
		fmt.Fprintf(&b, "  The ledger is keyed by the number, so %s would be recorded as already applied and its DDL would NEVER run.\n", d.File)
	case KindContentDrift:
		b.WriteString("  This database ran the OLD content and a fresh one will run the new, so the two schemas can diverge with nothing to notice.\n")
	case KindOrderViolation:
		b.WriteString("  Applying it now would build a schema no fresh database can reproduce, because a fresh database runs the two in the opposite order.\n")
	case KindDirtyMigration:
		b.WriteString("  A no-transaction migration is not atomic. Boot refuses rather than guess whether the DDL landed.\n")
	}
	for _, c := range d.causes() {
		fmt.Fprintf(&b, "  LIKELY CAUSE: %s\n", c[0])
		fmt.Fprintf(&b, "    RESOLUTION: %s\n", c[1])
	}
	if hint {
		b.WriteString("  " + statusHint)
	}
	return strings.TrimRight(b.String(), "\n")
}

// Status is the whole picture for one app: what is applied, what is pending,
// what disagrees, and every repair anybody has ever run here.
type Status struct {
	App    string
	Schema string
	// Applied is the ledger, in apply order.
	Applied []AppliedRecord
	// Pending names the migrations that have not run.
	Pending []string
	// Discrepancies is every disagreement, most severe first.
	Discrepancies []Discrepancy
	// Repairs is the audit trail, newest first.
	Repairs []RepairRecord
}

// HasErrors reports whether any discrepancy would block an apply.
func (s Status) HasErrors() bool {
	for _, d := range s.Discrepancies {
		if d.Severity == SeverityError {
			return true
		}
	}
	return false
}

// Report renders Status for a terminal.
func (s Status) Report() string {
	var b strings.Builder
	scope := s.App
	if s.Schema != "" {
		scope += " (schema " + s.Schema + ")"
	}
	fmt.Fprintf(&b, "migratekit status — app %s\n", scope)
	fmt.Fprintf(&b, "  applied: %d    pending: %d    discrepancies: %d\n", len(s.Applied), len(s.Pending), len(s.Discrepancies))

	if len(s.Pending) > 0 {
		b.WriteString("\nPENDING\n")
		for _, name := range s.Pending {
			fmt.Fprintf(&b, "  %s\n", name)
		}
	}

	if len(s.Discrepancies) > 0 {
		b.WriteString("\nDISCREPANCIES\n")
		for _, d := range s.Discrepancies {
			fmt.Fprintf(&b, "\n[%s] %s\n", strings.ToUpper(string(d.Severity)), d.explain(false))
		}
	} else {
		b.WriteString("\nNo discrepancies: every applied migration matches the file that applied it.\n")
	}

	if len(s.Repairs) > 0 {
		b.WriteString("\nREPAIR HISTORY\n")
		for _, r := range s.Repairs {
			fmt.Fprintf(&b, "  %s\n", r.Line())
		}
	}
	return b.String()
}

// Status reports the ledger, the pending set, every discrepancy and the repair
// history for this app. It is read-only apart from ensuring the tracking
// tables exist, and it never fails on a discrepancy — reporting one is the
// whole point.
func (p *Postgres) Status(ctx context.Context, migrations []Migration) (Status, error) {
	st := Status{App: p.app, Schema: p.schema}

	if err := p.Setup(ctx); err != nil {
		return st, err
	}
	applied, err := p.AppliedRecords(ctx)
	if err != nil {
		return st, err
	}

	// Ledger in apply order: the chain first, then any key with no file.
	seen := map[string]bool{}
	for _, m := range migrations {
		key := Prefix(m.Name)
		if rec, ok := applied[key]; ok {
			st.Applied = append(st.Applied, rec)
			seen[key] = true
		} else {
			st.Pending = append(st.Pending, m.Name)
		}
	}
	var extra []string
	for key := range applied {
		if !seen[key] {
			extra = append(extra, key)
		}
	}
	sort.Slice(extra, func(i, j int) bool { return keyLess(extra[i], extra[j]) })
	for _, key := range extra {
		st.Applied = append(st.Applied, applied[key])
	}

	st.Discrepancies = analyze(migrations, applied, checkOptions{
		strictOrdering:    p.strictOrdering,
		strictContent:     p.strictContent,
		includeLedgerOnly: true,
	})
	sort.SliceStable(st.Discrepancies, func(i, j int) bool {
		return severityRank(st.Discrepancies[i].Severity) < severityRank(st.Discrepancies[j].Severity)
	})

	st.Repairs, err = p.RepairHistory(ctx)
	if err != nil {
		return st, err
	}
	return st, nil
}

func severityRank(s Severity) int {
	switch s {
	case SeverityError:
		return 0
	case SeverityWarning:
		return 1
	default:
		return 2
	}
}
