package migratekit

import (
	"context"
	"database/sql"
)

// ensurePublicMigrationsTable creates/upgrades the tracker table. The tracker
// identity includes `schema` because WithSchema places tables in different
// schemas of the SAME database: without it, the same app applied to two schemas
// (e.g. doujins.* and hentai0.* sharing one DB) would record under one identity
// and the second schema would never get its tables. schema=” is the stamp for
// no-WithSchema groups only — Applied() matches schemas exactly, no wildcard.
func ensurePublicMigrationsTable(ctx context.Context, db *sql.DB) error {
	// Fresh installs get the full shape (constraint auto-named
	// migrations_app_database_schema_name_key).
	if _, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS public.migrations (
			id BIGSERIAL PRIMARY KEY,
			app TEXT NOT NULL,
			database TEXT NOT NULL,
			name TEXT NOT NULL,
			schema TEXT NOT NULL DEFAULT '',
			migrated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			UNIQUE(app, database, schema, name)
		);
	`); err != nil {
		return err
	}
	// Upgrade path for tables created before the schema column existed.
	if _, err := db.ExecContext(ctx,
		`ALTER TABLE public.migrations ADD COLUMN IF NOT EXISTS schema TEXT NOT NULL DEFAULT ''`); err != nil {
		return err
	}
	// Widen the unique key from (app, database, name) to include schema.
	// Idempotent: swap only if the old constraint is still present.
	_, err := db.ExecContext(ctx, `
		DO $$
		BEGIN
			IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'migrations_app_database_schema_name_key') THEN
				ALTER TABLE public.migrations
					ADD CONSTRAINT migrations_app_database_schema_name_key UNIQUE (app, database, schema, name);
			END IF;
			IF EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'migrations_app_database_name_key') THEN
				ALTER TABLE public.migrations DROP CONSTRAINT migrations_app_database_name_key;
			END IF;
		END $$;
	`)
	return err
}
