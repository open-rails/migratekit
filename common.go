package migratekit

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"strings"
)

const (
	postgresDriver = "postgres"
)

// Migration is a single SQL migration
type Migration struct {
	Name    string
	Content string
}

// Prefix extracts numeric prefix from migration filenames and normalizes it.
// Supports both underscore and hyphen separators.
// Examples:
//
//	"001_create_users.up.sql" -> "1"
//	"1-create-users.up.sql"   -> "1"
//	"0042_add_field.up.sql"   -> "42"
func Prefix(name string) string {
	name = strings.TrimSuffix(name, ".up.sql")
	name = strings.TrimSuffix(name, ".down.sql")

	// Find separator (underscore or hyphen)
	sepIdx := -1
	if i := strings.IndexByte(name, '_'); i > 0 {
		sepIdx = i
	} else if i := strings.IndexByte(name, '-'); i > 0 {
		sepIdx = i
	}

	var numericPart string
	if sepIdx > 0 {
		numericPart = name[:sepIdx]
	} else {
		numericPart = name
	}

	// Normalize by removing leading zeros
	// "001" -> "1", "0042" -> "42", "1" -> "1"
	normalized := strings.TrimLeft(numericPart, "0")
	if normalized == "" {
		// All zeros case: "000" -> "0"
		return "0"
	}
	return normalized
}

// isUndefinedTable reports whether err is Postgres SQLSTATE 42P01
// (undefined_table). Both lib/pq (*pq.Error) and pgx (*pgconn.PgError)
// expose SQLState(); the message-substring check remains as a fallback for
// drivers that don't (and is what older migratekit versions relied on).
func isUndefinedTable(err error) bool {
	if err == nil {
		return false
	}
	var stateErr interface{ SQLState() string }
	if errors.As(err, &stateErr) {
		return stateErr.SQLState() == "42P01"
	}
	return strings.Contains(err.Error(), "does not exist")
}

// MigrationSource represents a migration source with an app name and a
// migration filesystem (any fs.FS; embed.FS satisfies it).
type MigrationSource struct {
	App string
	FS  fs.FS
	// Schema optionally mirrors Postgres.WithSchema for validation helpers.
	// RewriteFrom carries canonical schema names to rewrite when applying via
	// Postgres.WithSchema(schema, rewriteFrom...).
	Schema      string
	RewriteFrom []string
}

// ValidatePostgresMigrations validates multiple Postgres migration sources at once.
// Returns an error if any migrations are pending.
func ValidatePostgresMigrations(ctx context.Context, db *sql.DB, sources ...MigrationSource) error {
	for _, source := range sources {
		migrations, err := LoadFromFS(source.FS)
		if err != nil {
			return fmt.Errorf("failed to load %s migrations: %w", source.App, err)
		}

		migrator := NewPostgres(db, source.App)
		if source.Schema != "" || len(source.RewriteFrom) > 0 {
			migrator.WithSchema(source.Schema, source.RewriteFrom...)
		}
		if err := migrator.ValidateAllApplied(ctx, migrations); err != nil {
			return fmt.Errorf("%s migrations not applied: %w", source.App, err)
		}
	}
	return nil
}
