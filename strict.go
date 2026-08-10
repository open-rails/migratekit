package migratekit

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
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
// Both checks are always on. They can only fire on a genuine mismatch: rows
// written by <=v1.4.0 carry no filename or digest, and an unknown identity is
// treated as unknown, never as a mismatch.
//
// ORDERING is separate and opt-in via WithStrictOrdering(). It refuses to
// apply a pending migration that sorts below one already applied — an
// out-of-order apply produces a schema that no fresh database will ever
// reproduce. It is opt-in because existing chains legitimately carry gaps and
// late arrivals that predate the rule; a consumer adopts it when its chain is
// clean.

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

// verifyIdentity checks every loaded migration against the ledger and reports
// the first violation. migrations must be in apply order.
func verifyIdentity(migrations []Migration, applied map[string]AppliedRecord, strictOrdering bool) error {
	for _, m := range migrations {
		rec, ok := applied[Prefix(m.Name)]
		if !ok {
			continue
		}
		// A number applied by a DIFFERENT file: this migration would be
		// skipped forever, and no later run would ever reconsider it.
		if rec.Filename != "" && rec.Filename != m.Name {
			return fmt.Errorf(
				"migration %s cannot be applied: number %q was already applied by a DIFFERENT file (%s).\n"+
					"  The ledger is keyed by the number, so %s would be recorded as already applied and its DDL would NEVER run.\n"+
					"  Renumber %s to an unclaimed number.",
				m.Name, rec.Key, rec.Filename, m.Name, m.Name)
		}
		// The same file, edited after it ran. Every database that already
		// applied it now diverges from a fresh one.
		if rec.Digest != "" && rec.Digest != ContentDigest(m.Content) {
			return fmt.Errorf(
				"migration %s was EDITED after it was applied (content digest %s, ledger has %s).\n"+
					"  Its DDL has already executed here, so the edit will never run and this database\n"+
					"  now differs from any database built from the current file. Revert the edit and\n"+
					"  add a new migration instead.",
				m.Name, ContentDigest(m.Content)[:12], rec.Digest[:12])
		}
	}

	if !strictOrdering {
		return nil
	}

	// Highest applied migration, by the same order LoadFromFS applies in.
	highest, haveHighest := "", false
	for _, m := range migrations {
		if _, ok := applied[Prefix(m.Name)]; ok {
			highest, haveHighest = m.Name, true
		}
	}
	if !haveHighest {
		return nil
	}
	for _, m := range migrations {
		if _, ok := applied[Prefix(m.Name)]; ok {
			continue
		}
		if orderBefore(m.Name, highest) {
			return fmt.Errorf(
				"migration %s sorts BEFORE %s, which is already applied.\n"+
					"  Applying it now would build a schema no fresh database can reproduce, because a\n"+
					"  fresh database runs the two in the opposite order. Renumber %s above %s.",
				m.Name, highest, m.Name, highest)
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
