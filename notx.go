package migratekit

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/open-rails/migratekit/internal/coremigrate"
)

// Migrations are TRANSACTIONAL by default, and the ledger row commits in the
// SAME transaction as the DDL (see applyOne). A failed migration is therefore
// atomic: nothing applied, nothing recorded, the rerun runs it cleanly. That is
// a frozen contract.
//
// One statement cannot live inside it. `CREATE INDEX CONCURRENTLY` — the only
// index build that does not take an ACCESS EXCLUSIVE lock on a live table — is
// refused by Postgres inside a transaction block (SQLSTATE 25001). Without an
// escape hatch, migratekit cannot express the safe way to index a production
// table, and the work happens in psql where the ledger never learns about it.
//
//	-- migratekit:no-transaction
//	CREATE INDEX CONCURRENTLY idx_requests_created_at ON requests (created_at);
//
// A file carrying the directive in its LEADING COMMENT BLOCK runs outside a
// transaction, still under the global advisory lock, one statement at a time.
//
// FAILURE SEMANTICS. Such a migration may be PARTIALLY applied, and no amount
// of design makes it atomic. So the honest move is to make the partial state
// impossible to ignore:
//
//	before executing  the row is written status='running', committed on its own
//	on success        status='applied', digest recorded
//	on failure        status='failed', with the Postgres error text
//	on a crash        the row stays 'running' — also unresolved
//
// Boot REFUSES on any row that is not 'applied'. Not absent, which would
// silently re-run half-applied DDL; not applied, which would silently skip the
// other half. The operator resolves it with `migratekit repair resolve`, which
// is audited like every other repair verb.

// Ledger statuses. An EMPTY status is 'applied': every row written before this
// existed, and every transactional insert since, is complete by construction.
const (
	StatusApplied = "applied"
	StatusRunning = "running"
	StatusFailed  = "failed"
)

// noTransactionDirective marks a migration that must run outside a transaction.
const noTransactionDirective = "-- migratekit:no-transaction"

var reNoTransaction = regexp.MustCompile(`^[ \t]*--[ \t]*migratekit:no-transaction[ \t]*$`)

// hasNoTransactionDirective reports whether the LEADING comment block carries
// the directive. Restricting it to the header keeps it out of string literals,
// DDL bodies and trailing prose — a directive that can be smuggled in by a
// quoted string is not a directive.
func hasNoTransactionDirective(content string) bool {
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if !strings.HasPrefix(trimmed, "--") {
			return false // first statement reached; the header block is over
		}
		if reNoTransaction.MatchString(line) {
			return true
		}
	}
	return false
}

// isDirty reports whether a ledger row represents an unfinished apply.
func (r AppliedRecord) isDirty() bool {
	return r.Status != "" && r.Status != StatusApplied
}

// applyOneNoTx applies a migration outside a transaction. Callers must hold the
// advisory lock. The statements are executed individually: a single Exec of a
// multi-statement string is wrapped in an implicit transaction by Postgres,
// which is exactly what the directive exists to avoid.
func (p *Postgres) applyOneNoTx(ctx context.Context, m Migration, audit *RepairRequest) error {
	key := Prefix(m.Name)
	digest := ContentDigest(m.Content)

	sqlText, err := coremigrate.SubstituteTemplates(m.Content)
	if err != nil {
		return fmt.Errorf("migration %s: %w", m.Name, err)
	}
	sqlText, err = rewriteSchemaRefs(sqlText, p.schema, p.schemaRewriteFrom)
	if err != nil {
		return fmt.Errorf("migration %s: %w", m.Name, err)
	}
	// The splitter is quote-aware but does not understand dollar-quoting, and a
	// mis-split $$ body outside a transaction is unrecoverable. A function body
	// has no reason to avoid a transaction anyway.
	if strings.Contains(sqlText, "$$") {
		return fmt.Errorf("migration %s carries %s and a $$-quoted body; the no-transaction path splits statements on `;` and cannot parse dollar-quoting. Move the function body to an ordinary (transactional) migration",
			m.Name, noTransactionDirective)
	}

	// Claim the row BEFORE executing. A process killed mid-run must leave
	// evidence; an absent row would look like a migration that never started.
	if _, err := p.db.ExecContext(ctx,
		`INSERT INTO public.migrations (app, database, schema, name, filename, content_sha256, status)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)
		 ON CONFLICT (app, database, schema, name)
		 DO UPDATE SET status = EXCLUDED.status, filename = EXCLUDED.filename, "error" = NULL`,
		p.app, postgresDriver, p.schema, key, m.Name, digest, StatusRunning); err != nil {
		return fmt.Errorf("claim %s: %w", m.Name, err)
	}

	execErr := p.execOutsideTransaction(ctx, sqlText)
	if execErr != nil {
		_, updErr := p.db.ExecContext(ctx,
			`UPDATE public.migrations SET status = $5, "error" = $6
			  WHERE app = $1 AND database = $2 AND schema = $3 AND name = $4`,
			p.app, postgresDriver, p.schema, key, StatusFailed, execErr.Error())
		return errors.Join(fmt.Errorf(
			"migration %s failed OUTSIDE a transaction and may be PARTIALLY applied: %w\n"+
				"  It is recorded %s in the ledger and boot will refuse until an operator resolves it. %s",
			m.Name, execErr, StatusFailed, statusHint), updErr)
	}

	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx,
		`UPDATE public.migrations SET status = $5, "error" = NULL, content_sha256 = $6
		  WHERE app = $1 AND database = $2 AND schema = $3 AND name = $4`,
		p.app, postgresDriver, p.schema, key, StatusApplied, digest); err != nil {
		return err
	}
	if audit != nil {
		if err := p.insertAudit(ctx, tx, "apply --allow-below-applied", key, "", "", m.Name, digest, *audit); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// execOutsideTransaction runs each statement on one pinned connection with no
// BeginTx. The connection is discarded afterwards, so a session-level
// search_path cannot leak back into the pool.
func (p *Postgres) execOutsideTransaction(ctx context.Context, sqlText string) error {
	conn, err := p.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()

	if strings.TrimSpace(p.schema) != "" {
		quoted, err := quoteIdent(p.schema)
		if err != nil {
			return fmt.Errorf("invalid schema %q: %w", p.schema, err)
		}
		if _, err := conn.ExecContext(ctx, fmt.Sprintf("SET search_path = %s, public", quoted)); err != nil {
			return fmt.Errorf("set search_path: %w", err)
		}
	}
	for _, stmt := range coremigrate.SplitStatements(sqlText) {
		if _, err := conn.ExecContext(ctx, stmt); err != nil {
			return err
		}
	}
	return nil
}

// --- resolving a dirty row --------------------------------------------------

// ResolveMode is how an operator disposes of a migration left dirty by a
// failed or interrupted no-transaction apply.
type ResolveMode string

const (
	// ResolveApplied marks it done: the operator finished or verified the work.
	ResolveApplied ResolveMode = "applied"
	// ResolveRerun deletes the row so the next boot runs the migration again.
	// Only safe when the migration is idempotent.
	ResolveRerun ResolveMode = "rerun"
)

// RepairResolve clears a dirty ledger row. Like every repair verb it requires a
// reason, refuses to run in CI, writes an audit row in the same transaction as
// the change, and never touches a schema — resolving is the operator's
// statement about work they did by hand, not a re-run of the DDL.
func (p *Postgres) RepairResolve(ctx context.Context, m Migration, mode ResolveMode, req RepairRequest) (RepairResult, error) {
	verb := "repair resolve --" + string(mode)
	switch mode {
	case ResolveApplied, ResolveRerun:
	default:
		return RepairResult{}, fmt.Errorf("migratekit: unknown resolve mode %q; use --applied or --rerun", mode)
	}
	if err := req.validate(verb); err != nil {
		return RepairResult{}, err
	}
	if err := p.Setup(ctx); err != nil {
		return RepairResult{}, err
	}
	applied, err := p.AppliedRecords(ctx)
	if err != nil {
		return RepairResult{}, err
	}
	key := Prefix(m.Name)
	rec, ok := applied[key]
	if !ok {
		return RepairResult{}, fmt.Errorf("%w: key %q is not in the ledger — a migration that never started has nothing to resolve", ErrNothingToRepair, key)
	}
	if !rec.isDirty() {
		return RepairResult{}, fmt.Errorf("%w: key %q is already %s", ErrNothingToRepair, key, StatusApplied)
	}

	digest := ContentDigest(m.Content)
	result := RepairResult{
		Verb: verb, Key: key,
		OldFilename: rec.Filename, OldDigest: rec.Digest,
		DryRun: req.DryRun,
	}
	if mode == ResolveApplied {
		result.NewFilename, result.NewDigest = m.Name, digest
	}
	if req.DryRun {
		return result, nil
	}

	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return RepairResult{}, err
	}
	defer tx.Rollback()

	if mode == ResolveApplied {
		_, err = tx.ExecContext(ctx,
			`UPDATE public.migrations SET status = $5, "error" = NULL, filename = $6, content_sha256 = $7
			  WHERE app = $1 AND database = $2 AND schema = $3 AND name = $4`,
			p.app, postgresDriver, p.schema, key, StatusApplied, m.Name, digest)
	} else {
		_, err = tx.ExecContext(ctx,
			`DELETE FROM public.migrations
			  WHERE app = $1 AND database = $2 AND schema = $3 AND name = $4`,
			p.app, postgresDriver, p.schema, key)
	}
	if err != nil {
		return RepairResult{}, err
	}
	if err := p.insertAudit(ctx, tx, verb, key, rec.Filename, rec.Digest, result.NewFilename, result.NewDigest, req); err != nil {
		return RepairResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return RepairResult{}, err
	}
	return result, nil
}

var _ = sql.ErrNoRows
