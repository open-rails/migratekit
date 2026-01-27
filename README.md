# migratekit

Minimal database migration library with app-scoped migrations and automatic locking.

## Install

```bash
go get github.com/doujins-org/migratekit
```

## Usage

### PostgreSQL (Complete Example)

```go
package main

import (
    "context"
    "database/sql"
    "embed"

    "github.com/doujins-org/migratekit"
    _ "github.com/lib/pq"
)

//go:embed migrations/postgres/*.sql
var postgresFS embed.FS

func main() {
    ctx := context.Background()
    db, _ := sql.Open("postgres", "postgres://...")

    // Load migrations from embedded FS
    migrations, _ := migratekit.LoadFromFS(postgresFS, "migrations/postgres")

    // Run migrations (3 lines)
    m := migratekit.NewPostgres(db, "doujins")
    m.Setup(ctx)
    m.ApplyMigrations(ctx, migrations)
}
```

### ClickHouse (Complete Example)

```go
package main

import (
    "context"
    "database/sql"
    "embed"

    "github.com/doujins-org/migratekit"
    _ "github.com/lib/pq"
)

//go:embed migrations/clickhouse/*.sql
var clickhouseFS embed.FS

func main() {
    ctx := context.Background()
    pg, _ := sql.Open("postgres", "postgres://...")

    // Load migrations from embedded FS
    migrations, _ := migratekit.LoadFromFS(clickhouseFS, "migrations/clickhouse")

    // Run migrations (3 lines)
    m := migratekit.NewClickHouse(&migratekit.ClickHouseConfig{
        ClientAddr: "clickhouse:9000",
        Database:   "analytics",
        Username:   "analytics_user",
        Password:   "analytics_password",
        App:        "doujins",

        // ClickHouse migrations are tracked in Postgres public.migrations
        // (database='clickhouse') and use Postgres advisory locks.
        PostgresDB: pg,
    })
    m.Setup(ctx)
    m.ApplyMigrations(ctx, migrations)
}
```

### Advanced (Manual Control)

```go
m := migratekit.NewPostgres(db, "doujins")
m.Setup(ctx)

applied, _ := m.Applied(ctx)  // Get list of applied migrations (no lock)

// Only lock if you have work to do
var toApply []Migration
for _, mig := range migrations {
    if !contains(applied, prefix(mig.Name)) {
        toApply = append(toApply, mig)
    }
}

if len(toApply) > 0 {
    m.Lock(ctx)
    defer m.Unlock(ctx)
    for _, mig := range toApply {
        m.Apply(ctx, mig)
    }
}
```

## API

### Primary Methods
- `LoadFromFS(fsys, dir)` - Load migrations from embedded filesystem
- `NewPostgres(db, app)` - Create Postgres migrator
- `NewClickHouse(config)` - Create ClickHouse migrator
- `Setup(ctx)` - Create migration tables (idempotent)
- `ApplyMigrations(ctx, []Migration)` - Apply all pending migrations (recommended)

### Advanced Methods
- `Lock(ctx)` - Acquire lock (waits up to 200s)
- `Unlock(ctx)` - Release lock
- `Applied(ctx)` - List of applied migration names (`[]string`)
- `Apply(ctx, Migration)` - Apply a single migration
- `Close()` - Cleanup

## Schema

migratekit creates one table in `public` schema on first `Setup()`:

```sql
CREATE TABLE public.migrations (
    id BIGSERIAL PRIMARY KEY,
    app TEXT NOT NULL,
    database TEXT NOT NULL,
    name TEXT NOT NULL,
    migrated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE(app, database, name)
);
```

Locking:
- Postgres uses advisory locks (no lock table).
- ClickHouse uses Postgres advisory locks and requires `ClickHouseConfig.PostgresDB`.

## Migration Files

**Naming Convention**

Files **must** follow this pattern: `{number}{separator}{description}.up.sql`

- **Number**: Any positive integer (leading zeros optional: `1`, `01`, `001` all work)
- **Separator**: Underscore `_` or hyphen `-`
- **Suffix**: Must end with `.up.sql`

✅ **Valid:**
```
001_create_users.up.sql
1_create_users.up.sql
01-add-indexes.up.sql
42-add-timestamps.up.sql
0003_migrations.up.sql
```

❌ **Invalid (will be skipped):**
```
001_create_users.sql        # Missing .up.sql
create_users.up.sql         # Missing numeric prefix
001.create.users.up.sql     # Invalid separator (use _ or -)
```

**Why `.up.sql` is required:**
- Standard convention used by golang-migrate, bun, etc.
- Reserves `.down.sql` for future rollback support
- Prevents accidental execution of non-migration SQL files

**What gets stored:**
Numeric prefixes are normalized (leading zeros removed) before storage:
- `001`, `01`, `1` all become `"1"`
- `042`, `42` both become `"42"`

## Features

- **Smart locking**: Only locks when there's work to do
- **App-scoped**: Each app has independent migration sequences
- **Correct Postgres locking**: Uses Postgres advisory locks (no lock table)
- **ClickHouse compatibility**: Runs ClickHouse DDL via native protocol; tracks applied migrations in Postgres
- **Self-contained**: Creates own tables on first run
- **Minimal**: small codebase + minimal dependencies

## Design

### Why lock only when needed?
Checking what's applied (`SELECT`) doesn't need a lock. Only write operations need locks. This allows multiple services to check migrations concurrently without blocking.

### Why per-app scoping?
Different apps (doujins, hentai0, billing) have independent migration sequences and can migrate concurrently.

### Why single table?
Easy to query "show all migrations" and simpler permissions.
