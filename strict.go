package migratekit

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"sort"
	"strings"
)

// The applied-migrations ledger is keyed by Prefix(filename) — the bare
// migration number. That key is not an identity: two DIFFERENT files that each
// claim number N normalize to the same key, so once one of them is applied the
// other is reported as "already applied" and never runs. Nothing in the chain
// notices, because a green suite and a clean boot both look exactly the same
// as they do when the migration really did run.
//
// A sequential number is claimed when a lane BRANCHES and, before v1.5.0, was
// never re-checked when it merged. LoadFromFS rejects two colliding files in
// ONE tree, but the damaging case never has both files in one tree: lane A's
// N is applied to a live database, lane B renumbers or reverts, and B's N —
// different DDL, same key — is silently skipped forever.
//
// v1.5.0 records the full filename and a content digest alongside the key, so
// the ledger can answer two questions it previously could not:
//
//	IDENTITY  was key N applied by THIS file, or by a different one?
//	INTEGRITY is the file that claims to have been applied still byte-identical?
//
// IDENTITY is a hard error: it is the silent-never-runs killer, and the fix
// (renumber, or adopt a restored ledger) is always available.
//
// INTEGRITY is a WARNING by default (Paul, 2026-08-11). The operator who
// edited an applied migration may have no way back to the old bytes, and
// refusing to boot over a cosmetic edit is worse than the divergence it warns
// about. WithStrictContent() restores the v1.5.0 hard error for consumers that
// want it. Either way the drift is reported by Status and cleared, on the
// record, by `repair accept-content`.
//
// ORDERING is separate and opt-in via WithStrictOrdering(). It refuses to
// apply a pending migration that sorts below one already applied — an
// out-of-order apply produces a schema that no fresh database will ever
// reproduce. It is opt-in because existing chains legitimately carry gaps and
// late arrivals that predate the rule; a consumer adopts it when its chain is
// clean.
//
// None of the checks can fire on a legacy ledger: rows written by <=v1.4.0
// carry no filename or digest, and an unknown identity is treated as unknown,
// never as a mismatch.

// statusHint is appended to every refusal. A boot error that names a problem
// and stops is a headache; one that names the verb that resolves it is not.
const statusHint = "Run `migratekit status` for resolution guidance."

// AppliedRecord is one row of the applied-migrations ledger.
type AppliedRecord struct {
	// Key is the ledger key — Prefix(filename).
	Key string
	// Filename is the full migration filename that claimed Key. Empty for
	// rows written before v1.5.0.
	Filename string
	// Digest is the sha256 of the applied migration's content. Empty for
	// rows written before v1.5.0.
	Digest string
}

// ContentDigest is the ledger's digest of a migration's bytes.
func ContentDigest(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

// WithStrictOrdering refuses to apply a pending migration that sorts below a
// migration already applied. Off by default; see the ordering note above.
func (p *Postgres) WithStrictOrdering() *Postgres {
	p.strictOrdering = true
	return p
}

// WithStrictContent turns content drift — an applied migration whose file has
// been edited since it ran — back into a hard error. The default is a warning
// (see the integrity note above); this is for consumers whose chain must be
// byte-reproducible and who would rather not boot than diverge.
func (p *Postgres) WithStrictContent() *Postgres {
	p.strictContent = true
	return p
}

// WithWarnFunc replaces the sink for warning-severity discrepancies. The
// default logs them through slog.Default() at warn level. A warning nobody
// sees is the failure mode this whole package exists to prevent, so the sink
// is never silent by default — pass func(Discrepancy){} to silence it
// deliberately.
func (p *Postgres) WithWarnFunc(fn func(Discrepancy)) *Postgres {
	p.warn = fn
	return p
}

func (p *Postgres) emit(discrepancies []Discrepancy) {
	warn := p.warn
	if warn == nil {
		warn = defaultWarn
	}
	for _, d := range discrepancies {
		if d.Severity == SeverityWarning {
			warn(d)
		}
	}
}

func defaultWarn(d Discrepancy) {
	slog.Default().Warn("migratekit: "+d.OneLine(),
		"kind", string(d.Kind),
		"migration", d.File,
		"key", d.Key,
		"ledger_digest", shortDigest(d.LedgerDigest),
		"file_digest", shortDigest(d.FileDigest),
	)
}

// AppliedRecords returns the applied-migrations ledger keyed by ledger key.
func (p *Postgres) AppliedRecords(ctx context.Context) (map[string]AppliedRecord, error) {
	rows, err := p.db.QueryContext(ctx,
		`SELECT name, COALESCE(filename, ''), COALESCE(content_sha256, '')
		   FROM public.migrations
		  WHERE app = $1 AND database = $2 AND schema = $3`,
		p.app, postgresDriver, p.schema)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string]AppliedRecord{}
	for rows.Next() {
		var rec AppliedRecord
		if err := rows.Scan(&rec.Key, &rec.Filename, &rec.Digest); err != nil {
			return nil, err
		}
		out[rec.Key] = rec
	}
	return out, rows.Err()
}

// checkOptions selects which discrepancies analyze reports and how severe
// each one is.
type checkOptions struct {
	strictOrdering bool
	strictContent  bool
	// allowBelow holds ledger keys exempted from the ordering rule by an
	// explicit operator exception (see ApplyWithOrderingException).
	allowBelow map[string]bool
	// includeLedgerOnly reports ledger rows with no file behind them. Useful
	// in Status, noise during apply.
	includeLedgerOnly bool
}

// analyze compares a loaded chain against the ledger and reports every
// discrepancy it finds, in apply order. migrations must be in apply order.
func analyze(migrations []Migration, applied map[string]AppliedRecord, opts checkOptions) []Discrepancy {
	var out []Discrepancy

	for _, m := range migrations {
		rec, ok := applied[Prefix(m.Name)]
		if !ok {
			continue
		}
		// A number applied by a DIFFERENT file: this migration would be
		// skipped forever, and no later run would ever reconsider it.
		if rec.Filename != "" && rec.Filename != m.Name {
			out = append(out, Discrepancy{
				Kind:           KindNumberMismatch,
				Severity:       SeverityError,
				Key:            rec.Key,
				File:           m.Name,
				LedgerFilename: rec.Filename,
				LedgerDigest:   rec.Digest,
				FileDigest:     ContentDigest(m.Content),
			})
			continue
		}
		// The same file, edited after it ran. Every database that already
		// applied it now diverges from a fresh one.
		if rec.Digest != "" && rec.Digest != ContentDigest(m.Content) {
			severity := SeverityWarning
			if opts.strictContent {
				severity = SeverityError
			}
			out = append(out, Discrepancy{
				Kind:           KindContentDrift,
				Severity:       severity,
				Key:            rec.Key,
				File:           m.Name,
				LedgerFilename: rec.Filename,
				LedgerDigest:   rec.Digest,
				FileDigest:     ContentDigest(m.Content),
			})
		}
	}

	if opts.includeLedgerOnly {
		inTree := map[string]bool{}
		for _, m := range migrations {
			inTree[Prefix(m.Name)] = true
		}
		var orphans []string
		for key := range applied {
			if !inTree[key] {
				orphans = append(orphans, key)
			}
		}
		sort.Slice(orphans, func(i, j int) bool { return keyLess(orphans[i], orphans[j]) })
		for _, key := range orphans {
			rec := applied[key]
			out = append(out, Discrepancy{
				Kind:           KindLedgerOnly,
				Severity:       SeverityInfo,
				Key:            key,
				LedgerFilename: rec.Filename,
				LedgerDigest:   rec.Digest,
			})
		}
	}

	if !opts.strictOrdering {
		return out
	}

	// Highest applied migration, by the same order LoadFromFS applies in.
	highest, haveHighest := "", false
	for _, m := range migrations {
		if _, ok := applied[Prefix(m.Name)]; ok {
			highest, haveHighest = m.Name, true
		}
	}
	if !haveHighest {
		return out
	}
	for _, m := range migrations {
		if _, ok := applied[Prefix(m.Name)]; ok {
			continue
		}
		if !orderBefore(m.Name, highest) {
			continue
		}
		if opts.allowBelow[Prefix(m.Name)] {
			continue
		}
		out = append(out, Discrepancy{
			Kind:     KindOrderViolation,
			Severity: SeverityError,
			Key:      Prefix(m.Name),
			File:     m.Name,
			Blocker:  highest,
		})
	}
	return out
}

// firstError returns the first error-severity discrepancy as an error.
func firstError(discrepancies []Discrepancy) error {
	for _, d := range discrepancies {
		if d.Severity == SeverityError {
			return fmt.Errorf("%s", d.String())
		}
	}
	return nil
}

// orderBefore reports whether migration a applies before b under the same
// rule LoadFromFS sorts by: numeric prefixes numerically and ahead of
// non-numeric ones, ties and non-numeric names lexically.
func orderBefore(a, b string) bool {
	na, aNum := numericPrefix(a)
	nb, bNum := numericPrefix(b)
	switch {
	case aNum && bNum:
		if na != nb {
			return na < nb
		}
		return a < b
	case aNum:
		return true
	case bNum:
		return false
	default:
		return a < b
	}
}

// keyLess orders two ledger keys numerically where possible.
func keyLess(a, b string) bool {
	na, aNum := numericPrefix(a)
	nb, bNum := numericPrefix(b)
	switch {
	case aNum && bNum:
		if na != nb {
			return na < nb
		}
		return a < b
	case aNum:
		return true
	case bNum:
		return false
	default:
		return a < b
	}
}

func shortDigest(d string) string {
	if len(d) > 12 {
		return d[:12]
	}
	return d
}

// CheckChain validates a migration chain as a FILE LISTING — no database.
// It reports duplicate numbers, gaps and non-monotonic numbering, which is
// what a CI gate on the merge boundary needs. names may be in any order.
func CheckChain(names []string) error {
	var nums []int64
	seen := map[string]string{}
	for _, name := range names {
		if !strings.HasSuffix(name, ".up.sql") {
			continue
		}
		key := Prefix(name)
		if prior, dup := seen[key]; dup {
			return fmt.Errorf("duplicate migration number %q: %s and %s — the ledger is keyed by the number, so one of them would never run", key, prior, name)
		}
		seen[key] = name
		n, ok := numericPrefix(name)
		if !ok {
			return fmt.Errorf("migration %s has no numeric prefix; the chain is linearly numbered", name)
		}
		nums = append(nums, n)
	}
	if len(nums) == 0 {
		return nil
	}
	sort.Slice(nums, func(i, j int) bool { return nums[i] < nums[j] })
	if nums[0] != 1 {
		return fmt.Errorf("migration chain starts at %d; it must start at 1", nums[0])
	}
	for i := 1; i < len(nums); i++ {
		if nums[i] != nums[i-1]+1 {
			return fmt.Errorf("gap in migration chain: %d is followed by %d", nums[i-1], nums[i])
		}
	}
	return nil
}
