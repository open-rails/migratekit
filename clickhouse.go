package migratekit

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// ClickHouseConfig holds configuration for ClickHouse migrations
type ClickHouseConfig struct {
	ClientAddr string // Native protocol address (e.g., clickhouse:9000)
	Database   string
	Username   string
	Password   string
	App        string
	LockID     string // Optional; uses DefaultLockID() if empty
	Cluster    string // Optional; if specified, uses ON CLUSTER for DDL statements
}

// ClickHouse handles ClickHouse migrations via native protocol
type ClickHouse struct {
	conn    driver.Conn // ClickHouse native connection
	addr    string
	db      string
	user    string
	pass    string
	app     string
	lockID  string
	cluster string // Optional cluster name for ON CLUSTER DDL
}

// NewClickHouse creates a ClickHouse migrator from config.
// If config.LockID is empty, uses DefaultLockID().
// Uses native protocol for all connections.
func NewClickHouse(config *ClickHouseConfig) *ClickHouse {
	lockID := config.LockID
	if lockID == "" {
		lockID = DefaultLockID()
	}

	return &ClickHouse{
		addr:    config.ClientAddr,
		db:      config.Database,
		user:    config.Username,
		pass:    config.Password,
		app:     config.App,
		lockID:  lockID,
		cluster: config.Cluster,
	}
}

// exec executes SQL using native protocol
func (c *ClickHouse) exec(ctx context.Context, sql string) error {
	// Lazy connect on first use
	if c.conn == nil {
		conn, err := clickhouse.Open(&clickhouse.Options{
			Addr: []string{c.addr},
			Auth: clickhouse.Auth{
				Database: c.db,
				Username: c.user,
				Password: c.pass,
			},
			DialTimeout: 30 * time.Second,
			Compression: &clickhouse.Compression{
				Method: clickhouse.CompressionLZ4,
			},
		})
		if err != nil {
			return fmt.Errorf("failed to connect to ClickHouse: %w", err)
		}
		c.conn = conn
	}

	return c.conn.Exec(ctx, sql)
}

// query returns first column as strings using native protocol
func (c *ClickHouse) query(ctx context.Context, sql string) ([]string, error) {
	// Lazy connect on first use
	if c.conn == nil {
		conn, err := clickhouse.Open(&clickhouse.Options{
			Addr: []string{c.addr},
			Auth: clickhouse.Auth{
				Database: c.db,
				Username: c.user,
				Password: c.pass,
			},
			DialTimeout: 30 * time.Second,
			Compression: &clickhouse.Compression{
				Method: clickhouse.CompressionLZ4,
			},
		})
		if err != nil {
			return nil, fmt.Errorf("failed to connect to ClickHouse: %w", err)
		}
		c.conn = conn
	}

	rows, err := c.conn.Query(ctx, sql)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	// Get column metadata for proper type handling
	columnTypes := rows.ColumnTypes()

	// Create properly-typed variables using reflection
	vars := make([]any, len(columnTypes))
	for i := range columnTypes {
		vars[i] = reflect.New(columnTypes[i].ScanType()).Interface()
	}

	var out []string
	for rows.Next() {
		// Scan into properly-typed variables
		if err := rows.Scan(vars...); err != nil {
			return nil, err
		}
		// Convert each value to string
		for _, v := range vars {
			// Dereference the pointer and convert to string
			out = append(out, fmt.Sprintf("%v", reflect.ValueOf(v).Elem().Interface()))
		}
	}
	return out, rows.Err()
}

// Setup ensures database and tables exist
func (c *ClickHouse) Setup(ctx context.Context) error {
	// NOTE: Database creation is handled by bootstrap migrations, not by migratekit.
	// Bootstrap migrations run as default user with CREATE DATABASE permissions.
	// App migrations run as analytics_user which only has permissions within the analytics database.

	// Build ON CLUSTER clause if cluster is specified
	onCluster := ""
	if c.cluster != "" {
		onCluster = " ON CLUSTER " + c.cluster
	}

	// Create migrations table
	createMigrationsSQL := `CREATE TABLE IF NOT EXISTS migrations` + onCluster + ` (
		app String,
		name String,
		migrated_at DateTime DEFAULT now()
	) ENGINE = ReplacingMergeTree(migrated_at) ORDER BY (app, name)`

	if err := c.exec(ctx, createMigrationsSQL); err != nil {
		return err
	}

	// Create migration_locks table
	// Note: Uses a global lock (lock_name='global') so only ONE app can migrate at a time
	// The 'app' field records which app acquired the lock (for debugging)
	createLocksSQL := `CREATE TABLE IF NOT EXISTS migration_locks` + onCluster + ` (
		lock_name String,
		app String,
		locked_at DateTime DEFAULT now(),
		locked_by String,
		expires_at DateTime
	) ENGINE = ReplacingMergeTree(locked_at) ORDER BY lock_name`

	return c.exec(ctx, createLocksSQL)
}

// Applied returns list of applied migrations
func (c *ClickHouse) Applied(ctx context.Context) ([]string, error) {
	sql := fmt.Sprintf("SELECT name FROM migrations WHERE app = '%s' ORDER BY name",
		strings.ReplaceAll(c.app, "'", "''"))
	rows, err := c.query(ctx, sql)
	if err != nil {
		return nil, err
	}
	return rows, nil
}

// Lock acquires a global database-wide migration lock
// All apps share the same lock (lock_name='global') to prevent concurrent ClickHouse migrations
// This is necessary because ON CLUSTER operations modify distributed DDL queue across all nodes
func (c *ClickHouse) Lock(ctx context.Context) error {
	maxRetries := int(lockAcquireTimeout / lockRetryInterval)
	for i := 0; i < maxRetries; i++ {
		// Check global lock (not per-app)
		sql := `SELECT app, locked_by, expires_at FROM migration_locks
			WHERE lock_name = 'global' ORDER BY locked_at DESC LIMIT 1`
		rows, _ := c.query(ctx, sql)

		canAcquire := len(rows) == 0
		if len(rows) >= 3 {
			if lockExpired(rows[:3], time.Now()) {
				canAcquire = true
			}
		}

		if canAcquire {
			// Acquire global lock, but record which app acquired it
			sql := fmt.Sprintf(`INSERT INTO migration_locks (lock_name, app, locked_by, locked_at, expires_at)
				VALUES ('global', '%s', '%s', now(), '%s')`,
				strings.ReplaceAll(c.app, "'", "''"),
				strings.ReplaceAll(c.lockID, "'", "''"),
				time.Now().Add(lockTTL).Format(clickhouseTimeLayout))
			if err := c.exec(ctx, sql); err != nil {
				return err
			}
			return nil
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(lockRetryInterval):
		}
	}
	return fmt.Errorf("lock timeout")
}

// Unlock releases the global lock
func (c *ClickHouse) Unlock(ctx context.Context) error {
	// Delete global lock where this app/lockID acquired it
	sql := fmt.Sprintf("DELETE FROM migration_locks WHERE lock_name = 'global' AND app = '%s' AND locked_by = '%s'",
		strings.ReplaceAll(c.app, "'", "''"),
		strings.ReplaceAll(c.lockID, "'", "''"))
	return c.exec(ctx, sql)
}

// lockExpired determines whether the provided lock row (app, locked_by, expires_at)
// has passed its expiration time.
func lockExpired(row []string, now time.Time) bool {
	if len(row) < 3 {
		return false
	}
	expiresAt, err := time.Parse(clickhouseTimeLayout, row[2])
	if err != nil {
		return false
	}
	return now.After(expiresAt)
}

// isTransientError checks if an error is likely due to distributed DDL propagation delays
func isTransientError(err error) bool {
	if err == nil {
		return false
	}
	errStr := err.Error()
	// Errors indicating table/view hasn't propagated yet across cluster nodes
	return strings.Contains(errStr, "UNKNOWN_TABLE") ||
		strings.Contains(errStr, "Unknown table") ||
		strings.Contains(errStr, "doesn't exist") ||
		strings.Contains(errStr, "does not exist") ||
		strings.Contains(errStr, "Table") && strings.Contains(errStr, "doesn't exist")
}

// Apply applies a migration with exponential backoff retry for transient errors
func (c *ClickHouse) Apply(ctx context.Context, m Migration) error {
	// First apply generic template substitution (environment variables, etc.)
	content := substituteTemplates(m.Content)

	// Inject ON_CLUSTER template variable for user migrations.
	// This allows migrations to use {{ON_CLUSTER}} or ${ON_CLUSTER} which expands
	// to " ON CLUSTER <cluster>" or empty string, depending on configuration.
	if c.cluster != "" {
		content = strings.ReplaceAll(content, "{{ON_CLUSTER}}", " ON CLUSTER "+c.cluster)
		content = strings.ReplaceAll(content, "${ON_CLUSTER}", " ON CLUSTER "+c.cluster)
	} else {
		content = strings.ReplaceAll(content, "{{ON_CLUSTER}}", "")
		content = strings.ReplaceAll(content, "${ON_CLUSTER}", "")
	}

	// Execute statements with retry logic for transient errors
	// Snuba retries up to 30s for synchronization issues
	const maxRetryDuration = 30 * time.Second
	for _, stmt := range splitSQL(content) {
		startTime := time.Now()
		backoff := 1 * time.Second

		for {
			err := c.exec(ctx, stmt)
			if err == nil {
				break // Success
			}

			// Check if this is a transient error worth retrying
			if !isTransientError(err) {
				return err // Permanent error, don't retry
			}

			// Check if we've exceeded total retry duration
			if time.Since(startTime) >= maxRetryDuration {
				return fmt.Errorf("migration failed after %v of retries: %w", maxRetryDuration, err)
			}

			// Wait with exponential backoff (1s, 2s, 4s, 8s, 16s)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoff):
			}

			backoff *= 2
			if backoff > 16*time.Second {
				backoff = 16 * time.Second // Cap at 16 seconds
			}
		}
	}

	// Check if migration already recorded (handles concurrent migrations)
	checkSQL := fmt.Sprintf("SELECT EXISTS(SELECT 1 FROM migrations WHERE app = '%s' AND name = '%s')",
		strings.ReplaceAll(c.app, "'", "''"),
		strings.ReplaceAll(Prefix(m.Name), "'", "''"))

	rows, err := c.query(ctx, checkSQL)
	if err != nil {
		return err
	}

	// If already recorded by another app, skip insertion
	if len(rows) > 0 && rows[0] != "0" {
		return nil
	}

	sql := fmt.Sprintf("INSERT INTO migrations (app, name) VALUES ('%s', '%s')",
		strings.ReplaceAll(c.app, "'", "''"),
		strings.ReplaceAll(Prefix(m.Name), "'", "''"))
	return c.exec(ctx, sql)
}

// ApplyMigrations applies all unapplied migrations (only locks if needed)
// Automatically calls Setup() to ensure migration tables exist before proceeding.
func (c *ClickHouse) ApplyMigrations(ctx context.Context, migrations []Migration) error {
	// Ensure migration tables exist (must happen before checking applied migrations)
	// This runs outside the lock initially to allow concurrent readers
	applied, err := c.Applied(ctx)
	if err != nil {
		// If migrations table doesn't exist, set up first (CREATE TABLE IF NOT EXISTS is safe for concurrent execution)
		errLower := strings.ToLower(err.Error())
		if strings.Contains(errLower, "doesn't exist") || strings.Contains(errLower, "unknown table") || strings.Contains(errLower, "unknown_table") {
			// Create the tables first using IF NOT EXISTS (safe for concurrent execution)
			if err = c.Setup(ctx); err != nil {
				return err
			}

			// Now acquire lock to apply migrations
			if err = c.Lock(ctx); err != nil {
				return err
			}
			defer c.Unlock(ctx)

			// After setup, check applied again (still under lock)
			applied, err = c.Applied(ctx)
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
				if err = c.Apply(ctx, mig); err != nil {
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
	if err = c.Lock(ctx); err != nil {
		return err
	}
	defer c.Unlock(ctx)

	// Double-check under lock in case another process applied some since our first read
	applied, err = c.Applied(ctx)
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

	// Apply migrations (retry logic handled within Apply() method)
	for _, mig := range toApply {
		if err := c.Apply(ctx, mig); err != nil {
			return err
		}
	}

	return nil
}

// ValidateAllApplied checks if all provided migrations have been applied.
// Returns an error listing any pending migrations if validation fails.
// This is intended for use during application startup to ensure the database
// schema is up-to-date before the app starts serving requests.
func (c *ClickHouse) ValidateAllApplied(ctx context.Context, migrations []Migration) error {
	applied, err := c.Applied(ctx)
	if err != nil {
		// If query fails, assume table doesn't exist and no migrations applied
		errLower := strings.ToLower(err.Error())
		if strings.Contains(errLower, "doesn't exist") || strings.Contains(errLower, "unknown table") || strings.Contains(errLower, "unknown_table") {
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

// Close closes the connection
func (c *ClickHouse) Close() error {
	if c.conn != nil {
		return c.conn.Close()
	}
	return nil
}
