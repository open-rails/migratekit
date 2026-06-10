# migratekit

Minimal database migration library with app-scoped migrations and automatic locking.

## Install

```bash
go get github.com/open-rails/migratekit
```

## Usage

### PostgreSQL (Complete Example)

```go
package main

import (
    "context"
    "database/sql"
    "embed"

    "github.com/open-rails/migratekit"
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

    "github.com/open-rails/migratekit"
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
- `Lock(ctx)` - Acquire the advisory lock on a dedicated pinned connection (blocks until available)
- `Unlock(ctx)` - Release the advisory lock and the pinned connection (safe on cancelled contexts)
- `Applied(ctx)` - List of applied migration names (`[]string`)
- `Apply(ctx, Migration)` - Apply a single migration
- `ValidateAllApplied(ctx, []Migration)` - Error if any migration is pending (startup gate)

Note: the migrator never owns or closes the `*sql.DB` you pass in (the former
`Postgres.Close()` was removed — it surprisingly closed the caller's pool).
`ClickHouse.Close()` still exists and closes only the lazily-opened native
connection the migrator itself created.

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
- Postgres uses advisory locks (no lock table). The lock is acquired and
  released on a single dedicated connection pinned for the lock's lifetime —
  session advisory locks belong to the connection that took them, so going
  through the pool would acquire on one connection and "release" on another.
  If the process dies mid-migration, Postgres releases the lock when the
  pinned connection drops.
- ClickHouse uses Postgres advisory locks (same pinning) and requires
  `ClickHouseConfig.PostgresDB`.

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

**Ordering:** migrations apply in numeric-prefix order (`2_x` before `10_x`),
not lexical filename order, so unpadded prefixes are safe.

**Duplicate prefixes are rejected:** because tracking is keyed by the
normalized prefix, two files sharing a prefix (`002_users.up.sql` +
`002_roles.up.sql`, or `0042_x` vs `42_y`) would mean the second silently
never runs. `LoadFromFS` returns an error instead.

## Template Variables

Migration SQL may reference environment variables as `{{VAR_NAME}}` or
`${VAR_NAME}`; they are substituted at execution time. A referenced variable
that is **not set** is an error — silent empty-string substitution previously
meant a typo'd `{{CLICKHOUSE_PASWORD}}` shipped an empty password into DDL. A
variable explicitly set to the empty string is substituted as-is. `{{ON_CLUSTER}}`
is special-cased for ClickHouse (expanded from `ClickHouseConfig.Cluster`).
Avoid `${...}`/`{{...}}` sequences in migration SQL that are not meant as
templates.

## ClickHouse Semantics

ClickHouse has no transactional DDL. A multi-statement migration that fails
partway leaves the earlier statements applied and the migration **unrecorded**,
so the rerun re-executes them. Every statement in a ClickHouse migration must
therefore be individually idempotent (`CREATE TABLE IF NOT EXISTS`,
`DROP ... IF EXISTS`, etc.). Statements are split on `;` with full awareness
of string literals and comments, so semicolons inside quoted strings are safe.

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
