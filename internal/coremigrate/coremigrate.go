// Package coremigrate holds implementation primitives shared by migratekit's
// Postgres migrator (the root package) and its ClickHouse migrator
// (migratekit/chmigrate): the migrations-tracking table DDL and environment
// template substitution. It is not part of migratekit's public API.
package coremigrate

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
)

// EnsurePublicMigrationsTable creates/upgrades the tracker table. The tracker
// identity includes `schema` because WithSchema places tables in different
// schemas of the SAME database: without it, the same app applied to two schemas
// (e.g. doujins.* and hentai0.* sharing one DB) would record under one identity
// and the second schema would never get its tables. schema=” is the stamp for
// no-WithSchema groups only — Applied() matches schemas exactly, no wildcard.
func EnsurePublicMigrationsTable(ctx context.Context, db *sql.DB) error {
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

// SubstituteTemplates replaces template variables in SQL with environment
// variable values. Supports two template formats:
//   - {{VAR_NAME}} (Handlebars/Mustache style)
//   - ${VAR_NAME} (Shell/JS template literal style)
//
// A referenced variable that is NOT SET in the environment is an error — a
// silent empty-string substitution would ship e.g. an empty password into
// DDL on a typo'd name. A variable explicitly set to the empty string is
// substituted as-is (assumed intentional).
//
// Empty templates like ${} or {{}} are skipped (no substitution), and
// ON_CLUSTER placeholders are left intact for the ClickHouse driver to expand
// from its own Cluster config.
func SubstituteTemplates(sql string) (string, error) {
	result := sql
	start := 0

	for start < len(result) {
		// Look for both template styles
		dollarIdx := strings.Index(result[start:], "${")
		braceIdx := strings.Index(result[start:], "{{")

		// Determine which comes first (or if neither exists)
		var openIdx, closeIdx int
		var openLen, closeLen int

		if dollarIdx >= 0 && (braceIdx < 0 || dollarIdx < braceIdx) {
			// ${VAR} style
			openIdx = start + dollarIdx
			closeIdx = strings.Index(result[openIdx+2:], "}")
			if closeIdx < 0 {
				break
			}
			closeIdx += openIdx + 2
			openLen = 2
			closeLen = 1
		} else if braceIdx >= 0 {
			// {{VAR}} style
			openIdx = start + braceIdx
			closeIdx = strings.Index(result[openIdx+2:], "}}")
			if closeIdx < 0 {
				break
			}
			closeIdx += openIdx + 2
			openLen = 2
			closeLen = 2
		} else {
			// No more templates found
			break
		}

		// Extract variable name
		varName := result[openIdx+openLen : closeIdx]

		// Skip empty templates like ${} or {{}}
		if varName == "" {
			start = closeIdx + closeLen
			continue
		}

		// Special-case: leave ON_CLUSTER placeholders intact.
		// These are handled later by the ClickHouse driver based on its Cluster config.
		if varName == "ON_CLUSTER" {
			start = closeIdx + closeLen
			continue
		}

		// Get environment variable value; unset is an error (see doc comment).
		value, set := os.LookupEnv(varName)
		if !set {
			return "", fmt.Errorf("template variable %s is referenced but the environment variable is not set", varName)
		}

		// Replace template with value
		result = result[:openIdx] + value + result[closeIdx+closeLen:]

		// Move start position forward
		start = openIdx + len(value)
	}

	return result, nil
}

// Contains reports whether item is present in slice.
func Contains(slice []string, item string) bool {
	for _, s := range slice {
		if s == item {
			return true
		}
	}
	return false
}
