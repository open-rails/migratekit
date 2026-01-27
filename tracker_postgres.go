package migratekit

import (
	"context"
	"database/sql"
	"fmt"
	"hash/fnv"
)

const (
	clickhouseTrackerDatabase = "clickhouse"
)

type postgresTracker struct {
	db *sql.DB
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

func (t *postgresTracker) Lock(ctx context.Context, key int64) error {
	if t == nil || t.db == nil {
		return fmt.Errorf("postgres tracker: db is nil")
	}
	_, err := t.db.ExecContext(ctx, `SELECT pg_advisory_lock($1)`, key)
	if err != nil {
		return fmt.Errorf("acquire postgres advisory lock: %w", err)
	}
	return nil
}

func (t *postgresTracker) Unlock(ctx context.Context, key int64) error {
	if t == nil || t.db == nil {
		return fmt.Errorf("postgres tracker: db is nil")
	}
	var unlocked bool
	err := t.db.QueryRowContext(ctx, `SELECT pg_advisory_unlock($1)`, key).Scan(&unlocked)
	if err != nil {
		return fmt.Errorf("release postgres advisory lock: %w", err)
	}
	if !unlocked {
		return fmt.Errorf("advisory lock was not held")
	}
	return nil
}

func advisoryLockKeyFromString(s string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte("migratekit:"))
	_, _ = h.Write([]byte(s))
	// Keep it non-negative for readability in debugging/SQL.
	return int64(h.Sum64() & 0x7fffffffffffffff)
}
