package migratekit

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"strconv"
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
	numericPart := migrationPrefix(name)

	// Normalize by removing leading zeros
	// "001" -> "1", "0042" -> "42", "1" -> "1"
	normalized := strings.TrimLeft(numericPart, "0")
	if normalized == "" {
		// All zeros case: "000" -> "0"
		return "0"
	}
	return normalized
}

func migrationPrefix(name string) string {
	name = strings.TrimSuffix(strings.TrimSuffix(name, ".up.sql"), ".down.sql")
	if i := strings.IndexAny(name, "_-"); i >= 0 {
		return name[:i]
	}
	return name
}

// Sequence parses an unsigned decimal migration prefix in the BIGINT range.
// Both separators (0001_schema and 0001-schema) and leading zeros are allowed.
func Sequence(name string) (int64, error) {
	prefix := migrationPrefix(name)
	if prefix == "" {
		return 0, fmt.Errorf("migration %q requires a numeric sequence", name)
	}
	for _, c := range prefix {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("migration %q requires an unsigned decimal sequence", name)
		}
	}
	sequence, err := strconv.ParseInt(normalizeNumber(prefix), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("migration %q has a sequence outside the BIGINT range: %w", name, err)
	}
	return sequence, nil
}

// ValidateSequences checks numeric identities and increasing order before any
// migrations run. Zero and gaps are allowed; duplicate, signed, nonnumeric,
// and overflowing sequences are rejected. LoadFromFS returns this order.
func ValidateSequences(migrations []Migration) error {
	seen := make(map[int64]string, len(migrations))
	var previous int64
	for i, migration := range migrations {
		sequence, err := Sequence(migration.Name)
		if err != nil {
			return err
		}
		if prior, ok := seen[sequence]; ok {
			return fmt.Errorf("duplicate migration prefix %d: %s and %s", sequence, prior, migration.Name)
		}
		if i > 0 && sequence < previous {
			return fmt.Errorf("migrations must be in numeric sequence order: %s follows %s", migration.Name, migrations[i-1].Name)
		}
		seen[sequence] = migration.Name
		previous = sequence
	}
	return nil
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
