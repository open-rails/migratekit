package migratekit

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"

	"github.com/open-rails/migratekit/internal/coremigrate"
)

// Postgres handles PostgreSQL migrations
type Postgres struct {
	db  *sql.DB
	app string
	// ownedDB is non-nil only for Postgres values created by
	// NewPostgresFromPGXPool. NewPostgres never owns its *sql.DB and therefore
	// Close is a no-op for values created through that constructor.
	ownedDB *sql.DB

	// schema optionally sets a schema-qualified search_path for executing
	// migrations, similar to River's `rivermigrate.Config.Schema`.
	//
	// When set, migrations run under:
	//   SET LOCAL search_path = "<schema>", public
	//
	// Migration tracking remains in public.migrations.
	schema string
	// schemaRewriteFrom optionally rewrites references to canonical schema
	// names in migration SQL before execution. This is for migrations authored
	// with hard-qualified app-owned DDL, e.g. openrails.foo, that should relocate
	// with WithSchema("tenant", "openrails").
	schemaRewriteFrom []string

	// lockConn pins the session that holds the advisory lock. Session
	// advisory locks belong to the CONNECTION that acquired them, so Lock and
	// Unlock must run on the same pinned *sql.Conn — issuing them through the
	// *sql.DB pool can acquire on one pooled connection and "release" on
	// another, leaking the real lock until the first connection is recycled.
	lockMu   sync.Mutex
	lockConn *sql.Conn

	// strictOrdering refuses to apply a migration that sorts below one
	// already applied. See WithStrictOrdering.
	strictOrdering bool
	// warn is the sink for warning-severity discrepancies. See WithWarnFunc.
	warn func(Discrepancy)
}

// NewPostgres creates a Postgres migrator
func NewPostgres(db *sql.DB, app string) *Postgres {
	return &Postgres{db: db, app: app}
}

// NewPostgresFromPGXPool creates a migration handle from a host pgx pool
// without using or mutating that pool. Migrations use connection-local session
// state (search_path, lock_timeout, statement_timeout) and pin one connection
// for the advisory lock while applying on another, so sharing the host pool
// could leak migration state into ordinary application queries.
//
// The returned Postgres owns a separate *sql.DB. Call Close when the migration
// or validation operation is complete. The pool's connection configuration is
// copied; BeforeConnect and AfterConnect are preserved, while the host pool's
// acquisition/release lifecycle is not involved.
func NewPostgresFromPGXPool(pool *pgxpool.Pool, app string) (*Postgres, error) {
	if pool == nil {
		return nil, fmt.Errorf("migratekit: a non-nil *pgxpool.Pool is required")
	}
	cfg := pool.Config()
	if cfg == nil || cfg.ConnConfig == nil {
		return nil, fmt.Errorf("migratekit: pgx pool has no connection configuration")
	}

	var opts []stdlib.OptionOpenDB
	if cfg.BeforeConnect != nil {
		opts = append(opts, stdlib.OptionBeforeConnect(cfg.BeforeConnect))
	}
	if cfg.AfterConnect != nil {
		opts = append(opts, stdlib.OptionAfterConnect(cfg.AfterConnect))
	}
	db := stdlib.OpenDB(*cfg.ConnConfig.Copy(), opts...)
	// migrate() pins one connection for the advisory lock and applies on a
	// second connection. Keep the owned adapter bounded to that requirement.
	db.SetMaxOpenConns(2)
	db.SetMaxIdleConns(2)
	return &Postgres{db: db, app: app, ownedDB: db}, nil
}

// Close releases the dedicated database handle owned by
// NewPostgresFromPGXPool. It never closes a *sql.DB supplied to NewPostgres.
func (p *Postgres) Close() error {
	if p == nil || p.ownedDB == nil {
		return nil
	}
	return p.ownedDB.Close()
}

// WithSchema configures the schema to target for migrations.
//
// Unqualified DDL/DML runs under:
//
//	SET LOCAL search_path = "<schema>", public
//
// Passing one or more canonical schema names also rewrites those schema
// references in migration SQL before execution. This lets apps author portable
// hard-qualified DDL against a default schema and relocate it at runtime:
//
//	migratekit.NewPostgres(db, "app").WithSchema(cfg.Schema, "openrails")
//
// Tracking always stays in public.migrations.
func (p *Postgres) WithSchema(schema string, rewriteFrom ...string) *Postgres {
	p.schema = schema
	p.schemaRewriteFrom = append([]string(nil), rewriteFrom...)
	return p
}

func quoteIdent(ident string) (string, error) {
	ident = strings.TrimSpace(ident)
	if ident == "" {
		return "", fmt.Errorf("empty identifier")
	}
	for _, r := range ident {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' {
			continue
		}
		return "", fmt.Errorf("invalid identifier %q", ident)
	}
	return `"` + ident + `"`, nil
}

func rewriteSchemaRefs(sqlText, target string, from []string) (string, error) {
	target = strings.TrimSpace(target)
	if target == "" || len(from) == 0 {
		return sqlText, nil
	}
	if _, err := quoteIdent(target); err != nil {
		return "", fmt.Errorf("invalid target schema %q: %w", target, err)
	}
	out := sqlText
	for _, schema := range from {
		schema = strings.TrimSpace(schema)
		if schema == "" || schema == target {
			continue
		}
		if _, err := quoteIdent(schema); err != nil {
			return "", fmt.Errorf("invalid source schema %q: %w", schema, err)
		}
		re := regexp.MustCompile(`\b` + regexp.QuoteMeta(schema) + `\b`)
		out = re.ReplaceAllString(out, target)
	}
	return out, nil
}

// Setup ensures migration tables exist (idempotent)
func (p *Postgres) ensureSetup(ctx context.Context) error {
	return coremigrate.EnsurePublicMigrationsTable(ctx, p.db)
}

// Applied returns list of applied migration names
func (p *Postgres) Applied(ctx context.Context) ([]string, error) {
	// Exact schema match only. schema='' is the stamp for no-WithSchema groups,
	// never a wildcard: a WithSchema group must not accept schema-less rows as
	// proof of application (that silently skips migrations for the new schema).
	rows, err := p.db.QueryContext(ctx,
		// COALESCE(status,'applied'): a row that is still running, or that
		// failed half-applied, is NOT proof of application — see notx.go.
		`SELECT name FROM public.migrations
		  WHERE app = $1 AND database = $2 AND schema = $3
		    AND COALESCE(status, 'applied') = 'applied'
		  ORDER BY name`,
		p.app, postgresDriver, p.schema)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		names = append(names, name)
	}
	return names, rows.Err()
}

// lock acquires the global advisory lock for migrations on a dedicated,
// pinned connection. It blocks until the lock is available. The pinned
// connection is held until unlock; if the process dies, Postgres releases
// the session lock when the connection drops.
func (p *Postgres) lock(ctx context.Context) error {
	p.lockMu.Lock()
	defer p.lockMu.Unlock()
	if p.lockConn != nil {
		return fmt.Errorf("migration advisory lock already held")
	}
	conn, err := p.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("acquire connection for migration advisory lock: %w", err)
	}
	if _, err := conn.ExecContext(ctx, `SELECT pg_advisory_lock($1)`, coremigrate.MigrationSetupLockKey); err != nil {
		_ = conn.Close()
		return fmt.Errorf("acquire global migration advisory lock: %w", err)
	}
	p.lockConn = conn
	return nil
}

// unlock releases the global advisory lock and the pinned connection. It
// runs with a non-cancellable context so a cancelled migration still
// releases the lock; closing the pinned connection is the safety net that
// releases the session lock even if pg_advisory_unlock itself fails.
func (p *Postgres) unlock(ctx context.Context) error {
	p.lockMu.Lock()
	defer p.lockMu.Unlock()
	if p.lockConn == nil {
		return fmt.Errorf("migration advisory lock not held")
	}
	ctx = context.WithoutCancel(ctx)

	var unlocked bool
	err := p.lockConn.QueryRowContext(ctx,
		`SELECT pg_advisory_unlock($1)`, coremigrate.MigrationSetupLockKey).Scan(&unlocked)
	closeErr := p.lockConn.Close() // releases the session lock even on unlock failure
	p.lockConn = nil

	if err != nil {
		return errors.Join(fmt.Errorf("release global migration advisory lock: %w", err), closeErr)
	}
	if !unlocked {
		return errors.Join(fmt.Errorf("advisory lock was not held"), closeErr)
	}
	return closeErr
}

// applyOne applies a single migration without checking the applied set or
// taking the lock; callers must hold the lock and filter applied
// migrations first (see ApplyMigrations). A non-nil audit records an
// ordering-exception audit row in the same transaction as the DDL, so the
// deviation and the schema change are one atomic fact.
func (p *Postgres) applyOne(ctx context.Context, m Migration, audit *RepairRequest) error {
	if hasNoTransactionDirective(m.Content) {
		return p.applyOneNoTx(ctx, m, audit)
	}
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if strings.TrimSpace(p.schema) != "" {
		quoted, err := quoteIdent(p.schema)
		if err != nil {
			return fmt.Errorf("invalid schema %q: %w", p.schema, err)
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf("SET LOCAL search_path = %s, public", quoted)); err != nil {
			return fmt.Errorf("set search_path: %w", err)
		}
	}

	// Apply template substitution (environment variables) at execution time
	sql, err := coremigrate.SubstituteTemplates(m.Content)
	if err != nil {
		return fmt.Errorf("migration %s: %w", m.Name, err)
	}
	sql, err = rewriteSchemaRefs(sql, p.schema, p.schemaRewriteFrom)
	if err != nil {
		return fmt.Errorf("migration %s: %w", m.Name, err)
	}
	if _, err := tx.ExecContext(ctx, sql); err != nil {
		return err
	}

	// `name` stays Prefix(m.Name) — it is the ledger key every existing
	// database is written with. filename/content_sha256 carry the identity
	// that key cannot express (see verifyIdentity).
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO public.migrations (app, database, schema, name, filename, content_sha256, semantic_sha256)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)
		 ON CONFLICT (app, database, schema, name) DO NOTHING`,
		p.app, postgresDriver, p.schema, Prefix(m.Name), m.Name,
		ContentDigest(m.Content), SemanticContentDigest(m.Content)); err != nil {
		return err
	}

	if audit != nil {
		if err := p.insertAudit(ctx, tx, "apply --allow-below-applied", Prefix(m.Name),
			"", "", m.Name, ContentDigest(m.Content), *audit); err != nil {
			return err
		}
	}

	return tx.Commit()
}

// ApplyMigrations applies all unapplied migrations (only locks if needed).
// Setup() always runs first under migratekit's bootstrap lock, so there is no
// missing-table special case or caller-owned setup retry loop.
func (p *Postgres) ApplyMigrations(ctx context.Context, migrations []Migration) error {
	return p.applyMigrations(ctx, migrations, nil, nil)
}

// applyMigrations is the shared apply path. allowBelow exempts ledger keys
// from the ordering rule and audit, when non-nil, records the exception.
func (p *Postgres) applyMigrations(ctx context.Context, migrations []Migration, allowBelow map[string]bool, audit *RepairRequest) (err error) {
	if err := p.ensureSetup(ctx); err != nil {
		return err
	}

	opts := checkOptions{
		strictOrdering: p.strictOrdering,
		allowBelow:     allowBelow,
	}

	// Identity is checked BEFORE the lock so a corrupt chain fails fast, and
	// again under it so a racing process cannot slip a claim in between.
	records, err := p.AppliedRecords(ctx)
	if err != nil {
		return err
	}
	if err := p.backfillSemanticDigests(ctx, migrations, records); err != nil {
		return err
	}
	discrepancies := analyze(migrations, records, opts)
	p.emit(discrepancies) // warnings once, on the pass that always runs
	if err := firstError(discrepancies); err != nil {
		return err
	}

	var toApply []Migration
	for _, mig := range migrations {
		if _, ok := records[Prefix(mig.Name)]; !ok {
			toApply = append(toApply, mig)
		}
	}
	if len(toApply) == 0 {
		return nil // Nothing to do, no lock needed
	}

	// Acquire lock only when we have work to do
	if err := p.lock(ctx); err != nil {
		return err
	}
	defer func() {
		if unlockErr := p.unlock(ctx); unlockErr != nil {
			err = errors.Join(err, unlockErr)
		}
	}()

	// Double-check under lock in case another process applied some since our first read
	records, err = p.AppliedRecords(ctx)
	if err != nil {
		return err
	}
	if err := p.backfillSemanticDigests(ctx, migrations, records); err != nil {
		return err
	}
	if err := firstError(analyze(migrations, records, opts)); err != nil {
		return err
	}
	toApply = toApply[:0]
	for _, mig := range migrations {
		if _, ok := records[Prefix(mig.Name)]; !ok {
			toApply = append(toApply, mig)
		}
	}

	// The target schema must exist BEFORE search_path is set: Postgres silently
	// drops missing schemas from search_path, which would land unqualified DDL
	// in public instead of erroring. Under the advisory lock, so replicas can't
	// race the create.
	if len(toApply) > 0 && strings.TrimSpace(p.schema) != "" {
		quoted, err := quoteIdent(p.schema)
		if err != nil {
			return fmt.Errorf("invalid schema %q: %w", p.schema, err)
		}
		if _, err := p.db.ExecContext(ctx, "CREATE SCHEMA IF NOT EXISTS "+quoted); err != nil {
			return fmt.Errorf("ensure schema %s: %w", p.schema, err)
		}
	}

	for _, mig := range toApply {
		var rowAudit *RepairRequest
		if audit != nil && allowBelow[Prefix(mig.Name)] {
			rowAudit = audit
		}
		if err := p.applyOne(ctx, mig, rowAudit); err != nil {
			return err
		}
	}

	return nil
}

// ValidateAllApplied checks if all provided migrations have been applied.
// Returns an error listing any pending migrations if validation fails.
// This is intended for use during application startup to ensure the database
// schema is up-to-date before the app starts serving requests.
func (p *Postgres) ValidateAllApplied(ctx context.Context, migrations []Migration) error {
	applied, err := p.Applied(ctx)
	if err != nil {
		// If the migrations table doesn't exist, no migrations have been applied
		if isUndefinedTable(err) {
			if len(migrations) == 0 {
				return nil // No migrations expected, validation passes
			}
			return fmt.Errorf("migration table does not exist - %d migrations need to be applied", len(migrations))
		}
		return fmt.Errorf("failed to get applied migrations: %w", err)
	}

	// Convert applied list to map for quick lookup
	appliedMap := make(map[string]bool)
	for _, name := range applied {
		appliedMap[name] = true
	}

	// Check which migrations are pending
	var pending []string
	for _, mig := range migrations {
		if !appliedMap[Prefix(mig.Name)] {
			pending = append(pending, mig.Name)
		}
	}

	if len(pending) > 0 {
		return fmt.Errorf("%d pending migrations must be applied: %v", len(pending), pending)
	}

	return nil
}
