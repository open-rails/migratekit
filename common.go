package migratekit

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
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

// substituteTemplates replaces template variables in SQL with environment variable values.
// Supports two template formats:
//   - {{VAR_NAME}} (Handlebars/Mustache style)
//   - ${VAR_NAME} (Shell/JS template literal style)
//
// Empty templates like ${} or {{}} are skipped (no substitution).
// Example: {{CLICKHOUSE_PASSWORD}} -> os.Getenv("CLICKHOUSE_PASSWORD")
func substituteTemplates(sql string) string {
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

		// Get environment variable value
		value := os.Getenv(varName)

		// Replace template with value
		result = result[:openIdx] + value + result[closeIdx+closeLen:]

		// Move start position forward
		start = openIdx + len(value)
	}

	return result
}

// splitSQL splits SQL into statements, removing comments
func splitSQL(sql string) []string {
	sql = strings.ReplaceAll(sql, "\r\n", "\n")
	sql = strings.ReplaceAll(sql, "\r", "\n")

	// Remove block comments
	for {
		if i := strings.Index(sql, "/*"); i >= 0 {
			if j := strings.Index(sql[i+2:], "*/"); j >= 0 {
				sql = sql[:i] + sql[i+j+4:]
			} else {
				sql = sql[:i]
				break
			}
		} else {
			break
		}
	}

	// Remove line comments
	var b strings.Builder
	for _, line := range strings.Split(sql, "\n") {
		if t := strings.TrimSpace(line); !strings.HasPrefix(t, "--") && t != "" {
			b.WriteString(line)
			b.WriteByte('\n')
		}
	}

	// Split by semicolon
	var out []string
	for _, stmt := range strings.Split(b.String(), ";") {
		if s := strings.TrimSpace(stmt); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// MigrationSource represents a migration source with an app name and embedded filesystem
type MigrationSource struct {
	App string
	FS  embed.FS
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
		if err := migrator.ValidateAllApplied(ctx, migrations); err != nil {
			return fmt.Errorf("%s migrations not applied: %w", source.App, err)
		}
	}
	return nil
}

// ValidateClickHouseMigrations validates ClickHouse migrations.
// Returns an error if any migrations are pending.
func ValidateClickHouseMigrations(ctx context.Context, config *ClickHouseConfig, fs embed.FS) error {
	migrations, err := LoadFromFS(fs)
	if err != nil {
		return fmt.Errorf("failed to load %s migrations: %w", config.App, err)
	}

	migrator := NewClickHouse(config)
	if err := migrator.ValidateAllApplied(ctx, migrations); err != nil {
		return fmt.Errorf("%s migrations not applied: %w", config.App, err)
	}
	return nil
}
