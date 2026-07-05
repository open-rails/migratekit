// Package chmigrate is migratekit's ClickHouse migration driver. It lives in
// its own package (and imports github.com/ClickHouse/clickhouse-go/v2) so that
// consumers who only need migratekit's Postgres migrator never pull in the
// ClickHouse client: Go's module graph pruning drops clickhouse-go/ch-go from
// a consumer's build list once nothing in that consumer imports this package.
//
// See the root package's README for the full migration-file/template-variable
// contract; this package covers only what's specific to ClickHouse.
package chmigrate

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"regexp"
	"strings"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"

	"github.com/open-rails/migratekit"
	"github.com/open-rails/migratekit/internal/coremigrate"
)

const clickhouseTrackerDatabase = "clickhouse"

// Config holds configuration for ClickHouse migrations.
type Config struct {
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

// ClickHouse handles ClickHouse migrations via native protocol.
type ClickHouse struct {
	conn    driver.Conn // ClickHouse native connection
	addr    string
	db      string
	user    string
	pass    string
	app     string
	cluster string // Optional cluster name for ON CLUSTER DDL

	tracker *coremigrate.Tracker
}

// New creates a ClickHouse migrator from config.
// Uses native protocol for all connections.
func New(config *Config) *ClickHouse {
	var tr *coremigrate.Tracker
	if config.PostgresDB != nil {
		tr = coremigrate.NewTracker(config.PostgresDB)
	}

	return &ClickHouse{
		addr:    config.ClientAddr,
		db:      config.Database,
		user:    config.Username,
		pass:    config.Password,
		app:     config.App,
		cluster: config.Cluster,
		tracker: tr,
	}
}

func (c *ClickHouse) requireTracker(ctx context.Context) error {
	if c.tracker == nil {
		return fmt.Errorf("clickhouse migrations require PostgresDB for tracking/locking")
	}
	if err := c.tracker.Setup(ctx); err != nil {
		return fmt.Errorf("clickhouse migrations require PostgresDB for tracking/locking: %w", err)
	}
	return nil
}

// connect lazily opens the native connection on first use.
func (c *ClickHouse) connect() error {
	if c.conn != nil {
		return nil
	}
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
	return nil
}

// exec executes SQL using native protocol
func (c *ClickHouse) exec(ctx context.Context, sql string) error {
	if err := c.connect(); err != nil {
		return err
	}
	return c.conn.Exec(ctx, sql)
}

// Setup ensures database and tables exist
func (c *ClickHouse) Setup(ctx context.Context) error {
	return c.requireTracker(ctx)
}

// Applied returns list of applied migrations
func (c *ClickHouse) Applied(ctx context.Context) ([]string, error) {
	if err := c.requireTracker(ctx); err != nil {
		return nil, err
	}
	return c.tracker.Applied(ctx, c.app, clickhouseTrackerDatabase)
}

// lock acquires a global database-wide migration lock.
// All apps share the same lock to prevent concurrent ClickHouse migrations.
// This is necessary because ON CLUSTER operations modify distributed DDL queue across all nodes.
func (c *ClickHouse) lock(ctx context.Context) error {
	if err := c.requireTracker(ctx); err != nil {
		return err
	}
	key := coremigrate.AdvisoryLockKey("clickhouse:migrations:" + c.addr + ":" + c.db + ":" + c.cluster)
	return c.tracker.Lock(ctx, key)
}

// unlock releases the global lock
func (c *ClickHouse) unlock(ctx context.Context) error {
	if err := c.requireTracker(ctx); err != nil {
		return err
	}
	key := coremigrate.AdvisoryLockKey("clickhouse:migrations:" + c.addr + ":" + c.db + ":" + c.cluster)
	return c.tracker.Unlock(ctx, key)
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

// staleReplicaPattern extracts the table znode path and replica name from a
// REPLICA_ALREADY_EXISTS message, e.g.
//
//	DB::Exception: Replica /clickhouse/tables/analytics/session_links/replicas/replica1 already exists.
var staleReplicaPattern = regexp.MustCompile(`Replica\s+(\S+)/replicas/([^\s/]+)\s+already exists`)

// staleReplicaInfo reports whether err is a ClickHouse REPLICA_ALREADY_EXISTS
// (code 253) error and, if so, returns the table's Keeper znode path and the
// replica name it collided on.
func staleReplicaInfo(err error) (zkPath, replica string, ok bool) {
	if err == nil {
		return "", "", false
	}
	var ex *clickhouse.Exception
	if errors.As(err, &ex) {
		if ex.Code != 253 {
			return "", "", false
		}
	} else if !strings.Contains(err.Error(), "REPLICA_ALREADY_EXISTS") {
		return "", "", false
	}
	m := staleReplicaPattern.FindStringSubmatch(err.Error())
	if m == nil {
		return "", "", false
	}
	return m[1], m[2], true
}

// isAccessDenied reports a ClickHouse privileges error (code 497).
func isAccessDenied(err error) bool {
	if err == nil {
		return false
	}
	var ex *clickhouse.Exception
	if errors.As(err, &ex) && ex.Code == 497 {
		return true
	}
	return strings.Contains(err.Error(), "ACCESS_DENIED") ||
		strings.Contains(err.Error(), "Not enough privileges")
}

// replicaOwner returns the live local table (as "db.table") that currently
// owns the given Keeper replica registration, or "" if no live table owns it.
func (c *ClickHouse) replicaOwner(ctx context.Context, zkPath, replica string) (string, error) {
	if err := c.connect(); err != nil {
		return "", err
	}
	rows, err := c.conn.Query(ctx,
		"SELECT database, table FROM system.replicas WHERE zookeeper_path = ? AND replica_name = ?",
		zkPath, replica)
	if err != nil {
		return "", fmt.Errorf("check system.replicas for %s: %w", zkPath, err)
	}
	defer rows.Close()
	if rows.Next() {
		var db, table string
		if scanErr := rows.Scan(&db, &table); scanErr != nil {
			return "(unknown)", nil
		}
		return db + "." + table, nil
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("check system.replicas for %s: %w", zkPath, err)
	}
	return "", nil
}

// dropStaleReplica removes an orphaned Keeper replica registration so the
// caller can retry the CREATE that collided with it (REPLICA_ALREADY_EXISTS).
//
// This situation arises when ClickHouse's local state and Keeper's get out of
// sync — e.g. the ClickHouse data volume was wiped (or the migration tracker
// was reset and migrations re-ran) while Keeper kept the old znodes, or an
// earlier non-SYNC DROP left the znode to Atomic-database delayed cleanup and
// a later migration re-creates the table inside that window. It is safe to
// self-heal because the replica name is THIS server's own identity (the
// {replica} macro): no other live server can legitimately hold it. Callers
// must have verified no live local table owns the registration (replicaOwner);
// ClickHouse itself also refuses SYSTEM DROP REPLICA for a replica backed by a
// local table, so the live case stays fail-closed.
func (c *ClickHouse) dropStaleReplica(ctx context.Context, zkPath, replica string) error {
	esc := func(s string) string { return strings.ReplaceAll(s, "'", "\\'") }
	log.Printf("migratekit: dropping stale ClickHouse replica %q at %s (no local table owns it)", replica, zkPath)
	return c.exec(ctx, fmt.Sprintf("SYSTEM DROP REPLICA '%s' FROM ZKPATH '%s'", esc(replica), esc(zkPath)))
}

// execStatements runs every statement of an already-templated migration with
// the retry policy:
//   - exponential backoff for transient distributed-DDL errors,
//   - REPLICA_ALREADY_EXISTS (253) with NO live owner: a stale Keeper znode
//     (partial volume wipe, or a non-SYNC DROP's delayed cleanup) — drop our
//     own orphaned registration once and retry immediately,
//   - REPLICA_ALREADY_EXISTS with a live owner: another writer just created
//     the (shared) table — back off and retry; IF NOT EXISTS no-ops once the
//     winner's table is visible.
func (c *ClickHouse) execStatements(ctx context.Context, name, content string) error {
	// Snuba retries up to 30s for synchronization issues
	const maxRetryDuration = 30 * time.Second
	for _, stmt := range splitSQL(content) {
		startTime := time.Now()
		backoff := 1 * time.Second
		droppedStaleReplica := false

		for {
			err := c.exec(ctx, stmt)
			if err == nil {
				break // Success
			}

			if zkPath, replica, ok := staleReplicaInfo(err); ok {
				owner, ownerErr := c.replicaOwner(ctx, zkPath, replica)
				if ownerErr != nil {
					if !isAccessDenied(ownerErr) {
						return fmt.Errorf("migration %s: %w (stale-replica recovery failed: %v)", name, err, ownerErr)
					}
					// No SELECT on system.replicas — skip the courtesy check and
					// rely on ClickHouse's own guard: SYSTEM DROP REPLICA refuses
					// replicas backed by a live local table.
					owner = ""
				}
				if owner == "" {
					// Ghost znode. Drop it once and retry the statement; a
					// second 253 after that is a real problem.
					if droppedStaleReplica {
						return fmt.Errorf("migration %s: %w (replica still exists after dropping it once)", name, err)
					}
					droppedStaleReplica = true
					if dropErr := c.dropStaleReplica(ctx, zkPath, replica); dropErr != nil {
						return fmt.Errorf("migration %s: %w (stale-replica recovery failed: %v)", name, err, dropErr)
					}
					continue
				}
				// Live owner: fall through to the backoff path below and retry.
				log.Printf("migratekit: replica %q at %s is owned by live table %s; retrying (concurrent create?)", replica, zkPath, owner)
			} else if !isTransientError(err) {
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
	return nil
}

// applyOne applies a migration with exponential backoff retry for transient
// errors. Callers must hold the lock and filter applied migrations first
// (see ApplyMigrations).
func (c *ClickHouse) applyOne(ctx context.Context, m migratekit.Migration) error {
	// First apply generic template substitution (environment variables, etc.)
	content, err := coremigrate.SubstituteTemplates(m.Content)
	if err != nil {
		return fmt.Errorf("migration %s: %w", m.Name, err)
	}

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

	if err := c.execStatements(ctx, m.Name, content); err != nil {
		return err
	}

	if err := c.requireTracker(ctx); err != nil {
		return err
	}
	return c.tracker.RecordApplied(ctx, c.app, clickhouseTrackerDatabase, migratekit.Prefix(m.Name))
}

// ApplyMigrations applies all unapplied migrations (only locks if needed)
// Automatically calls Setup() to ensure migration tables exist before proceeding.
func (c *ClickHouse) ApplyMigrations(ctx context.Context, migrations []migratekit.Migration) (err error) {
	if err := c.Setup(ctx); err != nil {
		return err
	}

	applied, err := c.Applied(ctx)
	if err != nil {
		return err
	}

	var toApply []migratekit.Migration
	for _, mig := range migrations {
		if !coremigrate.Contains(applied, migratekit.Prefix(mig.Name)) {
			toApply = append(toApply, mig)
		}
	}
	if len(toApply) == 0 {
		return nil
	}

	if err := c.lock(ctx); err != nil {
		return err
	}
	defer func() {
		if unlockErr := c.unlock(ctx); unlockErr != nil {
			err = errors.Join(err, unlockErr)
		}
	}()

	// Double-check under lock in case another process applied some since our first read
	applied, err = c.Applied(ctx)
	if err != nil {
		return err
	}
	toApply = toApply[:0]
	for _, mig := range migrations {
		if !coremigrate.Contains(applied, migratekit.Prefix(mig.Name)) {
			toApply = append(toApply, mig)
		}
	}
	if len(toApply) == 0 {
		return nil
	}

	for _, mig := range toApply {
		if err := c.applyOne(ctx, mig); err != nil {
			return err
		}
	}
	return nil
}

// ValidateAllApplied checks if all provided migrations have been applied.
// Returns an error listing any pending migrations if validation fails.
// This is intended for use during application startup to ensure the database
// schema is up-to-date before the app starts serving requests.
func (c *ClickHouse) ValidateAllApplied(ctx context.Context, migrations []migratekit.Migration) error {
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
		if !appliedMap[migratekit.Prefix(mig.Name)] {
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
