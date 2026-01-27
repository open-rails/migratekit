package migratekit

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
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
}

// NewPostgres creates a Postgres migrator
func NewPostgres(db *sql.DB, app string) *Postgres {
	return &Postgres{db: db, app: app}
}

// WithSchema configures the schema to target for unqualified DDL/DML in
// migrations (via SET LOCAL search_path). This allows embedded subsystems to
// create tables in the host application's schema (River-style).
func (p *Postgres) WithSchema(schema string) *Postgres {
	p.schema = schema
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

// Setup ensures migration tables exist (idempotent)
func (p *Postgres) Setup(ctx context.Context) error {
	return ensurePublicMigrationsTable(ctx, p.db)
}

// Applied returns list of applied migration names
func (p *Postgres) Applied(ctx context.Context) ([]string, error) {
	rows, err := p.db.QueryContext(ctx,
		`SELECT name FROM public.migrations WHERE app = $1 AND database = $2 ORDER BY name`,
		p.app, postgresDriver)
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

// Lock acquires a global advisory lock for migrations
// This blocks until the lock is available (no polling needed)
// The lock is automatically released when the connection closes
func (p *Postgres) Lock(ctx context.Context) error {
	_, err := p.db.ExecContext(ctx, `SELECT pg_advisory_lock($1)`, globalMigrationLockKey)
	if err != nil {
		return fmt.Errorf("acquire global migration advisory lock: %w", err)
	}
	return nil
}

// Unlock releases the global advisory lock
func (p *Postgres) Unlock(ctx context.Context) error {
	var unlocked bool
	err := p.db.QueryRowContext(ctx,
		`SELECT pg_advisory_unlock($1)`, globalMigrationLockKey).Scan(&unlocked)
	if err != nil {
		return fmt.Errorf("release global migration advisory lock: %w", err)
	}
	if !unlocked {
		return fmt.Errorf("advisory lock was not held")
	}
	return nil
}

// Apply applies a single migration
func (p *Postgres) Apply(ctx context.Context, m Migration) error {
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
	sql := substituteTemplates(m.Content)
	if _, err := tx.ExecContext(ctx, sql); err != nil {
		return err
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO public.migrations (app, database, name) VALUES ($1, $2, $3)
		 ON CONFLICT (app, database, name) DO NOTHING`,
		p.app, postgresDriver, Prefix(m.Name)); err != nil {
		return err
	}

	return tx.Commit()
}

// ApplyMigrations applies all unapplied migrations (only locks if needed)
// Automatically calls Setup() to ensure migration tables exist before proceeding.
func (p *Postgres) ApplyMigrations(ctx context.Context, migrations []Migration) error {
	// Ensure migration tables exist (must happen before checking applied migrations)
	// This runs outside the lock initially to allow concurrent readers
	applied, err := p.Applied(ctx)
	if err != nil {
		// If migrations table doesn't exist, set up first (CREATE TABLE IF NOT EXISTS is safe for concurrent execution)
		if strings.Contains(err.Error(), "does not exist") {
			// Create the tables first using IF NOT EXISTS (safe for concurrent execution)
			if err := p.Setup(ctx); err != nil {
				return err
			}

			// Now acquire lock to apply migrations
			if err := p.Lock(ctx); err != nil {
				return err
			}
			defer p.Unlock(ctx)

			// After setup, check applied again (still under lock)
			applied, err = p.Applied(ctx)
			if err != nil {
				return err
			}

			// Filter to only unapplied migrations
			var toApply []Migration
			for _, mig := range migrations {
				if !contains(applied, Prefix(mig.Name)) {
					toApply = append(toApply, mig)
				}
			}

			// Apply migrations (still under lock from setup)
			for _, mig := range toApply {
				if err := p.Apply(ctx, mig); err != nil {
					return err
				}
			}

			return nil
		}
		return err
	}

	// Normal path: tables exist, check what needs to be applied
	var toApply []Migration
	for _, mig := range migrations {
		if !contains(applied, Prefix(mig.Name)) {
			toApply = append(toApply, mig)
		}
	}

	if len(toApply) == 0 {
		return nil // Nothing to do, no lock needed
	}

	// Acquire lock only when we have work to do
	if err := p.Lock(ctx); err != nil {
		return err
	}
	defer p.Unlock(ctx)

	// Double-check under lock in case another process applied some since our first read
	applied, err = p.Applied(ctx)
	if err != nil {
		return err
	}
	toApply = toApply[:0]
	for _, mig := range migrations {
		if !contains(applied, Prefix(mig.Name)) {
			toApply = append(toApply, mig)
		}
	}
	if len(toApply) == 0 {
		return nil
	}

	// Apply migrations
	for _, mig := range toApply {
		if err := p.Apply(ctx, mig); err != nil {
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
		if strings.Contains(err.Error(), "does not exist") {
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

// Close closes the database connection
func (p *Postgres) Close() error {
	return p.db.Close()
}
