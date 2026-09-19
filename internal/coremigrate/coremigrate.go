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

// MigrationSetupLockKey serializes initialization and upgrades of the shared
// public.migrations tracker. Postgres migrations use the same key for their
// application lock, but setup must acquire it first because the tracker may
// not exist yet.
const MigrationSetupLockKey int64 = 7592348109

// EnsurePublicMigrationsTable atomically creates the tracker tables under the
// shared bootstrap advisory lock. This is a destructive v2 schema: callers
// must start with a fresh migration database. The tracker
// identity includes `schema` because WithSchema places tables in different
// schemas of the SAME database: without it, the same app applied to two schemas
// (e.g. doujins.* and hentai0.* sharing one DB) would record under one identity
// and the second schema would never get its tables. schema=” is the stamp for
// no-WithSchema groups only — Applied() matches schemas exactly, no wildcard.
func EnsurePublicMigrationsTable(ctx context.Context, db *sql.DB) error {
	if db == nil {
		return fmt.Errorf("migration tracker: db is nil")
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, MigrationSetupLockKey); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS public.migrations (
			id BIGSERIAL PRIMARY KEY,
			app TEXT NOT NULL,
			database TEXT NOT NULL,
			sequence BIGINT NOT NULL,
			schema TEXT NOT NULL DEFAULT '',
			filename TEXT,
			content_sha256 TEXT,
			semantic_sha256 TEXT,
			status TEXT NOT NULL DEFAULT 'applied',
			"error" TEXT,
			migrated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			UNIQUE(app, database, schema, sequence)
		);

		CREATE TABLE IF NOT EXISTS public.migration_repairs (
			id BIGSERIAL PRIMARY KEY,
			app TEXT NOT NULL,
			database TEXT NOT NULL,
			schema TEXT NOT NULL DEFAULT '',
			sequence BIGINT NOT NULL,
			verb TEXT NOT NULL,
			reason TEXT NOT NULL,
			operator TEXT NOT NULL DEFAULT '',
			os_user TEXT NOT NULL DEFAULT '',
			host TEXT NOT NULL DEFAULT '',
			old_filename TEXT,
			old_digest TEXT,
			new_filename TEXT,
			new_digest TEXT,
			repaired_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		);
		CREATE INDEX IF NOT EXISTS migration_repairs_scope_idx
			ON public.migration_repairs (app, database, schema, repaired_at DESC);
	`); err != nil {
		return err
	}
	return tx.Commit()
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
