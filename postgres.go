package migratekit

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"

	"github.com/open-rails/migratekit/internal/coremigrate"
)

const (
	// globalMigrationLockKey is the advisory lock key used for all migrations
	// across all apps. This ensures only one migration process runs at a time,
	// preventing race conditions when multiple services start simultaneously.
	globalMigrationLockKey = 7592348109 // Arbitrary constant
)

// Postgres handles PostgreSQL migrations
type Postgres struct {
	db  *sql.DB
	app string

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
}

// NewPostgres creates a Postgres migrator
func NewPostgres(db *sql.DB, app string) *Postgres {
	return &Postgres{db: db, app: app}
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
func (p *Postgres) Setup(ctx context.Context) error {
	return coremigrate.EnsurePublicMigrationsTable(ctx, p.db)
}

// Applied returns list of applied migration names
func (p *Postgres) Applied(ctx context.Context) ([]string, error) {
	// Exact schema match only. schema='' is the stamp for no-WithSchema groups,
	// never a wildcard: a WithSchema group must not accept schema-less rows as
	// proof of application (that silently skips migrations for the new schema).
	rows, err := p.db.QueryContext(ctx,
		`SELECT name FROM public.migrations WHERE app = $1 AND database = $2 AND schema = $3 ORDER BY name`,
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
	if _, err := conn.ExecContext(ctx, `SELECT pg_advisory_lock($1)`, int64(globalMigrationLockKey)); err != nil {
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
		`SELECT pg_advisory_unlock($1)`, int64(globalMigrationLockKey)).Scan(&unlocked)
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
// migrations first (see ApplyMigrations).
func (p *Postgres) applyOne(ctx context.Context, m Migration) error {
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

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO public.migrations (app, database, schema, name) VALUES ($1, $2, $3, $4)
		 ON CONFLICT (app, database, schema, name) DO NOTHING`,
		p.app, postgresDriver, p.schema, Prefix(m.Name)); err != nil {
		return err
	}

	return tx.Commit()
}

// ApplyMigrations applies all unapplied migrations (only locks if needed).
// Setup() always runs first, so there is no missing-table special case.
func (p *Postgres) ApplyMigrations(ctx context.Context, migrations []Migration) (err error) {
	// CREATE TABLE IF NOT EXISTS is cheap and almost always a no-op. Two
	// replicas racing the very first Setup can hit Postgres's known
	// concurrent-create race (duplicate key on pg_type/pg_class); one retry
	// resolves it because the loser then sees the winner's table.
	if err := p.Setup(ctx); err != nil {
		if err = p.Setup(ctx); err != nil {
			return err
		}
	}

	applied, err := p.Applied(ctx)
	if err != nil {
		return err
	}

	var toApply []Migration
	for _, mig := range migrations {
		if !coremigrate.Contains(applied, Prefix(mig.Name)) {
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
	applied, err = p.Applied(ctx)
	if err != nil {
		return err
	}
	toApply = toApply[:0]
	for _, mig := range migrations {
		if !coremigrate.Contains(applied, Prefix(mig.Name)) {
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
		if err := p.applyOne(ctx, mig); err != nil {
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
