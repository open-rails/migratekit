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
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"regexp"
	"strconv"
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
	Database   string // Required explicit target; also identifies the ledger scope
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

func (c *ClickHouse) validateConfig() error {
	if strings.TrimSpace(c.db) == "" {
		return fmt.Errorf("clickhouse migrations require an explicit Database")
	}
	if c.tracker == nil {
		return fmt.Errorf("clickhouse migrations require PostgresDB for tracking/locking")
	}
	return nil
}

func (c *ClickHouse) requireTracker(ctx context.Context) error {
	if err := c.validateConfig(); err != nil {
		return err
	}
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

// Applied returns list of applied migrations
func (c *ClickHouse) Applied(ctx context.Context) ([]string, error) {
	if err := c.requireTracker(ctx); err != nil {
		return nil, err
	}
	records, err := c.records(ctx)
	if err != nil {
		return nil, err
	}
	var applied []string
	for _, r := range records {
		if r.Status == "applied" {
			applied = append(applied, strconv.FormatInt(r.Sequence, 10))
		}
	}
	return applied, nil
}

// lock serializes the target database across apps and connection aliases.
// One Postgres ledger owns one logical ClickHouse deployment.
func (c *ClickHouse) lock(ctx context.Context) error {
	key := coremigrate.AdvisoryLockKey("clickhouse:migrations:" + c.db)
	return c.tracker.Lock(ctx, key)
}

// unlock releases the global lock
func (c *ClickHouse) unlock(ctx context.Context) error {
	key := coremigrate.AdvisoryLockKey("clickhouse:migrations:" + c.db)
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
	sequence, err := migratekit.Sequence(m.Name)
	if err != nil {
		return err
	}
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

	record := coremigrate.TrackedMigration{Sequence: sequence, Filename: m.Name, Digest: contentDigest(m.Content), Status: "running"}
	if err := c.tracker.RecordState(ctx, c.app, clickhouseTrackerDatabase, c.db, record); err != nil {
		return err
	}
	if err := c.execStatements(ctx, m.Name, content); err != nil {
		return err
	}

	record.Status = "applied"
	return c.tracker.RecordState(ctx, c.app, clickhouseTrackerDatabase, c.db, record)
}

// ApplyMigrations validates identity and applies pending migrations under the target lock.
// Tracking tables are initialized automatically before proceeding.
func (c *ClickHouse) ApplyMigrations(ctx context.Context, migrations []migratekit.Migration) (err error) {
	if err := migratekit.ValidateSequences(migrations); err != nil {
		return err
	}
	if err := c.requireTracker(ctx); err != nil {
		return err
	}

	if err := c.lock(ctx); err != nil {
		return err
	}
	defer func() {
		if unlockErr := c.unlock(ctx); unlockErr != nil {
			err = errors.Join(err, unlockErr)
		}
	}()

	// Validate every recorded identity before any ClickHouse statement or ledger write.
	toApply, err := c.pending(ctx, migrations)
	if err != nil {
		return err
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
	if err := migratekit.ValidateSequences(migrations); err != nil {
		return err
	}
	pending, err := c.pending(ctx, migrations)
	if err != nil {
		return err
	}
	if len(pending) > 0 {
		return fmt.Errorf("%d pending or incomplete migrations must be applied: %v", len(pending), pendingNames(pending))
	}
	return nil
}

func contentDigest(content string) string { return fmt.Sprintf("%x", sha256.Sum256([]byte(content))) }

func (c *ClickHouse) records(ctx context.Context) ([]coremigrate.TrackedMigration, error) {
	if err := c.validateConfig(); err != nil {
		return nil, err
	}
	return c.tracker.Records(ctx, c.app, clickhouseTrackerDatabase, c.db)
}

func (c *ClickHouse) pending(ctx context.Context, migrations []migratekit.Migration) ([]migratekit.Migration, error) {
	records, err := c.records(ctx)
	if err != nil {
		return nil, err
	}
	bySequence := make(map[int64]migratekit.Migration, len(migrations))
	for _, m := range migrations {
		sequence, _ := migratekit.Sequence(m.Name)
		bySequence[sequence] = m
	}
	applied := make(map[int64]bool, len(records))
	for _, r := range records {
		m, ok := bySequence[r.Sequence]
		if !ok {
			return nil, fmt.Errorf("clickhouse database %q: recorded migration %d (%s) missing from source", c.db, r.Sequence, r.Filename)
		}
		if r.Filename != m.Name || r.Digest != contentDigest(m.Content) {
			return nil, fmt.Errorf("clickhouse database %q: migration %d identity mismatch (filename or content digest)", c.db, r.Sequence)
		}
		if r.Status != "applied" && r.Status != "running" {
			return nil, fmt.Errorf("clickhouse migration %s has unsupported status %q", m.Name, r.Status)
		}
		applied[r.Sequence] = r.Status == "applied"
	}
	var pending []migratekit.Migration
	for _, m := range migrations {
		sequence, _ := migratekit.Sequence(m.Name)
		if !applied[sequence] {
			pending = append(pending, m)
		}
	}
	return pending, nil
}

func pendingNames(migrations []migratekit.Migration) []string {
	names := make([]string, len(migrations))
	for i, m := range migrations {
		names[i] = m.Name
	}
	return names
}

// Close closes the connection
func (c *ClickHouse) Close() error {
	if c.conn != nil {
		return c.conn.Close()
	}
	return nil
}
