package coremigrate

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"hash/fnv"
	"sync"
)

// Tracker records applied migrations and takes advisory locks in Postgres,
// for migrators whose target database is not Postgres itself (e.g.
// ClickHouse): the durable record and locking live in Postgres regardless of
// what's being migrated.
type Tracker struct {
	db *sql.DB

	// lockConn pins the session holding the advisory lock; see Tracker.Lock.
	lockMu   sync.Mutex
	lockConn *sql.Conn
	lockKey  int64
}

func NewTracker(db *sql.DB) *Tracker {
	return &Tracker{db: db}
}

func (t *Tracker) Setup(ctx context.Context) error {
	if t == nil || t.db == nil {
		return fmt.Errorf("postgres tracker: db is nil")
	}
	return EnsurePublicMigrationsTable(ctx, t.db)
}

// TrackedMigration is the identity and completion state of a non-Postgres migration.
type TrackedMigration struct {
	Sequence                 int64
	Filename, Digest, Status string
}

// executor uses the pinned lock session so even a one-connection pool can migrate.
func (t *Tracker) executor() interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	ExecContext(context.Context, string, ...any) (sql.Result, error)
} {
	t.lockMu.Lock()
	defer t.lockMu.Unlock()
	if t.lockConn != nil {
		return t.lockConn
	}
	return t.db
}

// Records reads only. Empty-schema rows are refused because their target is unknown.
func (t *Tracker) Records(ctx context.Context, app, database, schema string) ([]TrackedMigration, error) {
	rows, err := t.executor().QueryContext(ctx, `SELECT sequence, COALESCE(filename, ''), COALESCE(content_sha256, ''), status, schema
 FROM public.migrations WHERE app = $1 AND database = $2 AND (schema = $3 OR schema = '') ORDER BY sequence`, app, database, schema)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TrackedMigration
	for rows.Next() {
		var r TrackedMigration
		var target string
		if err := rows.Scan(&r.Sequence, &r.Filename, &r.Digest, &r.Status, &target); err != nil {
			return nil, err
		}
		if target == "" {
			return nil, fmt.Errorf("unscoped %s migration ledger for app %q; use a fresh database and ledger", database, app)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// RecordState is called under the target's advisory lock, after identity validation.
func (t *Tracker) RecordState(ctx context.Context, app, database, schema string, r TrackedMigration) error {
	_, err := t.executor().ExecContext(ctx, `INSERT INTO public.migrations (app, database, schema, sequence, filename, content_sha256, status)
 VALUES ($1, $2, $3, $4, $5, $6, $7)
 ON CONFLICT (app, database, schema, sequence) DO UPDATE SET status = EXCLUDED.status`,
		app, database, schema, r.Sequence, r.Filename, r.Digest, r.Status)
	return err
}

// Lock acquires the advisory lock for key on a dedicated, pinned connection
// (session advisory locks belong to the connection that took them; going
// through the pool would acquire and release on different connections).
func (t *Tracker) Lock(ctx context.Context, key int64) error {
	if t == nil || t.db == nil {
		return fmt.Errorf("postgres tracker: db is nil")
	}
	t.lockMu.Lock()
	defer t.lockMu.Unlock()
	if t.lockConn != nil {
		return fmt.Errorf("postgres tracker: advisory lock already held (key %d)", t.lockKey)
	}
	conn, err := t.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("acquire connection for postgres advisory lock: %w", err)
	}
	if _, err := conn.ExecContext(ctx, `SELECT pg_advisory_lock($1)`, key); err != nil {
		_ = conn.Close()
		return fmt.Errorf("acquire postgres advisory lock: %w", err)
	}
	t.lockConn = conn
	t.lockKey = key
	return nil
}

// Unlock releases the advisory lock on the pinned connection, then closes it.
// Runs with a non-cancellable context; closing the connection releases the
// session lock even if pg_advisory_unlock fails.
func (t *Tracker) Unlock(ctx context.Context, key int64) error {
	if t == nil || t.db == nil {
		return fmt.Errorf("postgres tracker: db is nil")
	}
	t.lockMu.Lock()
	defer t.lockMu.Unlock()
	if t.lockConn == nil {
		return fmt.Errorf("postgres tracker: advisory lock not held")
	}
	if key != t.lockKey {
		return fmt.Errorf("postgres tracker: unlock key %d does not match held key %d", key, t.lockKey)
	}
	ctx = context.WithoutCancel(ctx)

	var unlocked bool
	err := t.lockConn.QueryRowContext(ctx, `SELECT pg_advisory_unlock($1)`, key).Scan(&unlocked)
	closeErr := t.lockConn.Close()
	t.lockConn = nil
	t.lockKey = 0

	if err != nil {
		return errors.Join(fmt.Errorf("release postgres advisory lock: %w", err), closeErr)
	}
	if !unlocked {
		return errors.Join(fmt.Errorf("advisory lock was not held"), closeErr)
	}
	return closeErr
}

// AdvisoryLockKey derives a stable advisory-lock key from an arbitrary string.
func AdvisoryLockKey(s string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte("migratekit:"))
	_, _ = h.Write([]byte(s))
	// Keep it non-negative for readability in debugging/SQL.
	return int64(h.Sum64() & 0x7fffffffffffffff)
}
