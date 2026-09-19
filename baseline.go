package migratekit

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
)

// Baselining is NOT a repair. The repair verbs rewrite a row's identity and
// nothing else, and the package header promises they never mark an unapplied
// migration applied. Baseline does exactly that, on purpose, which is why it
// is its own verb with its own contract:
//
// A database whose schema was built some other way — restored from a dump,
// created by a retired chain, or hand-built before this tool existed — carries
// a ledger that cannot be reconciled by any repair. Rows name files the tree no
// longer has, and the files the tree does have were never recorded. Applying
// them is not an option: the DDL is already there, and a fresh-install
// migration that guards against re-running will refuse.
//
// Baseline states, once, that the schema already matches the tree, and writes
// the ledger to say so. It runs no DDL. It cannot tell whether the claim is
// true: nothing in this package knows what a migration builds. That is why
// BaselineRequest.SchemaVerified exists and why it is not defaulted — the
// caller has to assert it, and an operator has to type it.
//
// One more thing this package cannot see: a ledger row is keyed by app, not by
// process. Where several services share one app key, they each validate the row
// against their own pinned copy of the tree, and a baseline written from one
// version makes the others refuse. Bring them to the same version first.

// ErrBaselineUnsafe is returned when the ledger holds something a baseline must
// not overwrite.
var ErrBaselineUnsafe = errors.New("migratekit: refusing to baseline")

// BaselineRequest authorizes a baseline.
type BaselineRequest struct {
	RepairRequest
	// SchemaVerified is the caller's assertion that every object the tree's
	// migrations build is already present. Baseline refuses without it,
	// because a ledger that claims migrations ran over a schema they did not
	// build is worse than the unreconciled ledger it replaces.
	SchemaVerified bool
}

// BaselinePlan is what Baseline would write, in the order it would write it.
type BaselinePlan struct {
	// Record are migrations in the tree with no ledger row; they become applied.
	Record []Migration
	// Retire are ledger rows whose key the tree no longer carries and which sort
	// below everything it does; they are removed.
	Retire []AppliedRecord
	// Untouched are rows that already agree with the tree.
	Untouched []AppliedRecord
}

// Empty reports whether a baseline would change nothing.
func (p BaselinePlan) Empty() bool { return len(p.Record) == 0 && len(p.Retire) == 0 }

func keyOrder(key string) (int, bool) {
	n, err := strconv.Atoi(key)
	return n, err == nil
}

// PlanBaseline classifies the ledger against the tree and refuses anything a
// baseline must not decide on its own. It reads nothing and writes nothing, so
// a caller can show the plan before asking for it.
func PlanBaseline(applied map[string]AppliedRecord, migrations []Migration) (BaselinePlan, error) {
	var plan BaselinePlan
	if len(migrations) == 0 {
		return plan, fmt.Errorf("%w: the tree carries no migrations, so there is nothing to baseline against", ErrBaselineUnsafe)
	}

	tree := make(map[string]Migration, len(migrations))
	highest, haveHighest := 0, false
	for _, m := range migrations {
		key := Prefix(m.Name)
		tree[key] = m
		if n, ok := keyOrder(key); ok && (!haveHighest || n > highest) {
			highest, haveHighest = n, true
		}
	}

	keys := make([]string, 0, len(applied))
	for key := range applied {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		a, aok := keyOrder(keys[i])
		b, bok := keyOrder(keys[j])
		if aok && bok {
			return a < b
		}
		return keys[i] < keys[j]
	})

	for _, key := range keys {
		record := applied[key]
		if record.isDirty() {
			return BaselinePlan{}, fmt.Errorf("%w: key %q is %s, so a migration stopped with its work half done. "+
				"Baselining would erase that and claim it finished. Resolve it first with `repair resolve`", ErrBaselineUnsafe, key, record.Status)
		}
		m, inTree := tree[key]
		if !inTree {
			n, ok := keyOrder(key)
			if !ok || !haveHighest {
				return BaselinePlan{}, fmt.Errorf("%w: key %q is applied, the tree does not carry it, and it cannot be ordered against the tree. "+
					"Retiring a row we cannot place would forget DDL that may have run; say what it is and remove it by hand", ErrBaselineUnsafe, key)
			}
			if n > highest {
				return BaselinePlan{}, fmt.Errorf("%w: key %q is applied and sorts above every migration this tree carries (highest %d). "+
					"That is a tree older than the database, and removing the row would forget DDL that ran. Baseline from the version that owns it", ErrBaselineUnsafe, key, highest)
			}
			plan.Retire = append(plan.Retire, record)
			continue
		}
		if record.Filename != m.Name ||
			record.Digest != ContentDigest(m.Content) ||
			record.SemanticDigest != SemanticContentDigest(m.Content) {
			return BaselinePlan{}, fmt.Errorf("%w: key %q is applied but records a different file or digest than the tree. "+
				"Rebinding an identity is `repair adopt`, not a baseline", ErrBaselineUnsafe, key)
		}
		plan.Untouched = append(plan.Untouched, record)
	}

	for _, m := range migrations {
		if _, ok := applied[Prefix(m.Name)]; !ok {
			plan.Record = append(plan.Record, m)
		}
	}
	return plan, nil
}

// Baseline records the tree's migrations as applied and removes ledger rows the
// tree has retired, in one transaction, without running any DDL. Like the
// repair verbs it requires a reason, refuses to run in CI, and writes an audit
// row per change. Unlike them it takes the migration lock before it reads,
// because it decides the same rows ApplyMigrations does.
func (p *Postgres) Baseline(ctx context.Context, migrations []Migration, req BaselineRequest) (results []RepairResult, err error) {
	const verb = "baseline"
	if err := req.validate(verb); err != nil {
		return nil, err
	}
	if !req.SchemaVerified {
		return nil, fmt.Errorf("%w: SchemaVerified is not set. A baseline is a claim that the schema already matches the tree, "+
			"and this package cannot check it. Verify the objects the migrations build, then say so", ErrBaselineUnsafe)
	}
	if err := p.Setup(ctx); err != nil {
		return nil, err
	}
	// A preview reads without the lock: it is advisory, and a global migration
	// lock is too heavy a price for a question. The write takes the lock BEFORE
	// it reads, so the plan it acts on is the plan it wrote.
	if !req.DryRun {
		if err := p.lock(ctx); err != nil {
			return nil, err
		}
		defer func() {
			if unlockErr := p.unlock(ctx); unlockErr != nil {
				err = errors.Join(err, unlockErr)
			}
		}()
	}
	applied, err := p.AppliedRecords(ctx)
	if err != nil {
		return nil, err
	}
	plan, err := PlanBaseline(applied, migrations)
	if err != nil {
		return nil, err
	}
	if plan.Empty() {
		return nil, fmt.Errorf("%w: the ledger already records this tree", ErrNothingToRepair)
	}

	for _, record := range plan.Retire {
		results = append(results, RepairResult{
			Verb: verb, Key: record.Key,
			OldFilename: record.Filename, OldDigest: record.Digest,
			DryRun: req.DryRun,
		})
	}
	for _, m := range plan.Record {
		results = append(results, RepairResult{
			Verb: verb, Key: Prefix(m.Name),
			NewFilename: m.Name, NewDigest: ContentDigest(m.Content),
			DryRun: req.DryRun,
		})
	}
	if req.DryRun {
		return results, nil
	}

	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	for _, record := range plan.Retire {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM public.migrations WHERE app = $1 AND database = $2 AND schema = $3 AND name = $4`,
			p.app, postgresDriver, p.schema, record.Key); err != nil {
			return nil, err
		}
		if err := p.insertAudit(ctx, tx, verb, record.Key, record.Filename, record.Digest, "", "", req.RepairRequest); err != nil {
			return nil, err
		}
	}
	for _, m := range plan.Record {
		key, digest := Prefix(m.Name), ContentDigest(m.Content)
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO public.migrations (app, database, schema, name, filename, content_sha256, semantic_sha256, status)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
			p.app, postgresDriver, p.schema, key, m.Name, digest, SemanticContentDigest(m.Content), StatusApplied); err != nil {
			return nil, err
		}
		if err := p.insertAudit(ctx, tx, verb, key, "", "", m.Name, digest, req.RepairRequest); err != nil {
			return nil, err
		}
	}
	return results, tx.Commit()
}
