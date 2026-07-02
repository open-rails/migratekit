package migratekit

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
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

// contains checks if a string is in a slice
func contains(slice []string, item string) bool {
	for _, s := range slice {
		if s == item {
			return true
		}
	}
	return false
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

// substituteTemplates replaces template variables in SQL with environment
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
// ON_CLUSTER placeholders are left intact for ClickHouse.Apply to expand.
func substituteTemplates(sql string) (string, error) {
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
		// These are handled later by ClickHouse.Apply based on ClickHouseConfig.Cluster.
		if varName == "ON_CLUSTER" {
			// Skip over this placeholder without substituting so that
			// ClickHouse.Apply can expand {{ON_CLUSTER}} or ${ON_CLUSTER}.
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

// splitSQL splits SQL into statements and strips comments. It is
// quote-aware: semicolons and comment markers inside '...' strings,
// "..." identifiers, and `...` identifiers are preserved verbatim
// (including ” doubled-quote and backslash escapes inside strings).
func splitSQL(sql string) []string {
	sql = strings.ReplaceAll(sql, "\r\n", "\n")
	sql = strings.ReplaceAll(sql, "\r", "\n")

	var out []string
	var b strings.Builder
	i, n := 0, len(sql)

	flush := func() {
		if s := strings.TrimSpace(b.String()); s != "" {
			out = append(out, s)
		}
		b.Reset()
	}

	for i < n {
		c := sql[i]
		switch {
		case c == '\'' || c == '"' || c == '`':
			// Copy the quoted region verbatim.
			quote := c
			b.WriteByte(c)
			i++
			for i < n {
				ch := sql[i]
				b.WriteByte(ch)
				i++
				if ch == '\\' && quote == '\'' && i < n {
					// Backslash escape (ClickHouse-style strings).
					b.WriteByte(sql[i])
					i++
					continue
				}
				if ch == quote {
					if i < n && sql[i] == quote {
						// Doubled quote escape ('' or "" or ``).
						b.WriteByte(sql[i])
						i++
						continue
					}
					break
				}
			}
		case c == '-' && i+1 < n && sql[i+1] == '-':
			// Line comment: skip to end of line.
			for i < n && sql[i] != '\n' {
				i++
			}
		case c == '/' && i+1 < n && sql[i+1] == '*':
			// Block comment: skip to closing */ (or end of input).
			i += 2
			for i+1 < n && !(sql[i] == '*' && sql[i+1] == '/') {
				i++
			}
			if i+1 < n {
				i += 2
			} else {
				i = n
			}
		case c == ';':
			flush()
			i++
		default:
			b.WriteByte(c)
			i++
		}
	}
	flush()
	return out
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

// ValidateClickHouseMigrations validates ClickHouse migrations.
// Returns an error if any migrations are pending.
func ValidateClickHouseMigrations(ctx context.Context, config *ClickHouseConfig, fsys fs.FS) error {
	migrations, err := LoadFromFS(fsys)
	if err != nil {
		return fmt.Errorf("failed to load %s migrations: %w", config.App, err)
	}

	migrator := NewClickHouse(config)
	if err := migrator.ValidateAllApplied(ctx, migrations); err != nil {
		return fmt.Errorf("%s migrations not applied: %w", config.App, err)
	}
	return nil
}
