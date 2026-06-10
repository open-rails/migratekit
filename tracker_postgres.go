package migratekit

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"hash/fnv"
	"sync"
)

const (
	clickhouseTrackerDatabase = "clickhouse"
)

type postgresTracker struct {
	db *sql.DB

	// lockConn pins the session holding the advisory lock; see Postgres.lockConn.
	lockMu   sync.Mutex
	lockConn *sql.Conn
	lockKey  int64
}

func newPostgresTracker(db *sql.DB) *postgresTracker {
	return &postgresTracker{db: db}
}

func (t *postgresTracker) Setup(ctx context.Context) error {
	if t == nil || t.db == nil {
		return fmt.Errorf("postgres tracker: db is nil")
	}
	return ensurePublicMigrationsTable(ctx, t.db)
}

func (t *postgresTracker) Applied(ctx context.Context, app string, database string) ([]string, error) {
	if t == nil || t.db == nil {
		return nil, fmt.Errorf("postgres tracker: db is nil")
	}
	rows, err := t.db.QueryContext(ctx,
		`SELECT name FROM public.migrations WHERE app = $1 AND database = $2 ORDER BY name`,
		app, database,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		names = append(names, name)
	}
	return names, rows.Err()
}

func (t *postgresTracker) RecordApplied(ctx context.Context, app string, database string, name string) error {
	if t == nil || t.db == nil {
		return fmt.Errorf("postgres tracker: db is nil")
	}
	_, err := t.db.ExecContext(ctx,
		`INSERT INTO public.migrations (app, database, name) VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`,
		app, database, name,
	)
	return err
}

// Lock acquires the advisory lock for key on a dedicated, pinned connection
// (session advisory locks belong to the connection that took them; going
// through the pool would acquire and release on different connections).
func (t *postgresTracker) Lock(ctx context.Context, key int64) error {
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
func (t *postgresTracker) Unlock(ctx context.Context, key int64) error {
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

func advisoryLockKeyFromString(s string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte("migratekit:"))
	_, _ = h.Write([]byte(s))
	// Keep it non-negative for readability in debugging/SQL.
	return int64(h.Sum64() & 0x7fffffffffffffff)
}
