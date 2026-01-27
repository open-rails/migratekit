package migratekit

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// ClickHouseConfig holds configuration for ClickHouse migrations
type ClickHouseConfig struct {
	ClientAddr string // Native protocol address (e.g., clickhouse:9000)
	Database   string
	Username   string
	Password   string
	App        string
	Cluster    string // Optional; if specified, uses ON CLUSTER for DDL statements

	// Required. ClickHouse migration tracking + locking is done in Postgres:
	// - tracking: Postgres public.migrations with database='clickhouse'
	// - locking: Postgres advisory locks
	//
	// This intentionally avoids ClickHouse-based migration tables (`migrations`, `migration_locks`)
	// which are awkward to restore/merge and are not a good fit for authoritative state.
	PostgresDB *sql.DB
}

// ClickHouse handles ClickHouse migrations via native protocol
type ClickHouse struct {
	conn    driver.Conn // ClickHouse native connection
	addr    string
	db      string
	user    string
	pass    string
	app     string
	cluster string // Optional cluster name for ON CLUSTER DDL

	pgTracker *postgresTracker
}

// NewClickHouse creates a ClickHouse migrator from config.
// Uses native protocol for all connections.
func NewClickHouse(config *ClickHouseConfig) *ClickHouse {
	var pgT *postgresTracker
	if config.PostgresDB != nil {
		pgT = newPostgresTracker(config.PostgresDB)
	}

	return &ClickHouse{
		addr:      config.ClientAddr,
		db:        config.Database,
		user:      config.Username,
		pass:      config.Password,
		app:       config.App,
		cluster:   config.Cluster,
		pgTracker: pgT,
	}
}

func (c *ClickHouse) requirePostgresTracker(ctx context.Context) error {
	if c.pgTracker == nil {
		return fmt.Errorf("clickhouse migrations require PostgresDB for tracking/locking")
	}
	if err := c.pgTracker.Setup(ctx); err != nil {
		return fmt.Errorf("clickhouse migrations require PostgresDB for tracking/locking: %w", err)
	}
	return nil
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

// Setup ensures database and tables exist
func (c *ClickHouse) Setup(ctx context.Context) error {
	return c.requirePostgresTracker(ctx)
}

// Applied returns list of applied migrations
func (c *ClickHouse) Applied(ctx context.Context) ([]string, error) {
	if err := c.requirePostgresTracker(ctx); err != nil {
		return nil, err
	}
	return c.pgTracker.Applied(ctx, c.app, clickhouseTrackerDatabase)
}

// Lock acquires a global database-wide migration lock
// All apps share the same lock (lock_name='global') to prevent concurrent ClickHouse migrations
// This is necessary because ON CLUSTER operations modify distributed DDL queue across all nodes
func (c *ClickHouse) Lock(ctx context.Context) error {
	if err := c.requirePostgresTracker(ctx); err != nil {
		return err
	}
	key := advisoryLockKeyFromString("clickhouse:migrations:" + c.addr + ":" + c.db + ":" + c.cluster)
	return c.pgTracker.Lock(ctx, key)
}

// Unlock releases the global lock
func (c *ClickHouse) Unlock(ctx context.Context) error {
	if err := c.requirePostgresTracker(ctx); err != nil {
		return err
	}
	key := advisoryLockKeyFromString("clickhouse:migrations:" + c.addr + ":" + c.db + ":" + c.cluster)
	return c.pgTracker.Unlock(ctx, key)
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

	if err := c.requirePostgresTracker(ctx); err != nil {
		return err
	}
	return c.pgTracker.RecordApplied(ctx, c.app, clickhouseTrackerDatabase, Prefix(m.Name))
}

// ApplyMigrations applies all unapplied migrations (only locks if needed)
// Automatically calls Setup() to ensure migration tables exist before proceeding.
func (c *ClickHouse) ApplyMigrations(ctx context.Context, migrations []Migration) error {
	if err := c.Setup(ctx); err != nil {
		return err
	}

	applied, err := c.Applied(ctx)
	if err != nil {
		return err
	}

	var toApply []Migration
	for _, mig := range migrations {
		if !contains(applied, Prefix(mig.Name)) {
			toApply = append(toApply, mig)
		}
	}
	if len(toApply) == 0 {
		return nil
	}

	if err := c.Lock(ctx); err != nil {
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
