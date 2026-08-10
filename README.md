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

    // Run migrations (2 lines; ApplyMigrations ensures the tracking table)
    m := migratekit.NewPostgres(db, "doujins")
    m.ApplyMigrations(ctx, migrations)

    // Optional: target a configured schema. Unqualified migration SQL runs
    // under SET LOCAL search_path = "<schema>", public.
    m = migratekit.NewPostgres(db, "doujins").WithSchema(cfg.DB.Schema)

    // Optional: if migrations are authored with hard-qualified canonical DDL,
    // rewrite that app-owned schema to the configured schema while applying.
    m = migratekit.NewPostgres(db, "openrails").WithSchema(cfg.DB.Schema, "openrails")
}
```

### ClickHouse (Complete Example)

ClickHouse support lives in its own subpackage, `migratekit/chmigrate`, which
imports `github.com/ClickHouse/clickhouse-go/v2`. The root `migratekit`
package has zero ClickHouse imports: if you only need the Postgres migrator,
`go mod tidy` never pulls in the ClickHouse client (Go's module graph pruning
drops it once nothing in your module imports `migratekit/chmigrate`).

```go
package main

import (
    "context"
    "database/sql"
    "embed"

    "github.com/open-rails/migratekit"
    "github.com/open-rails/migratekit/chmigrate"
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
    m := chmigrate.New(&chmigrate.Config{
        ClientAddr: "clickhouse:9000",
        Database:   "analytics",
        Username:   "analytics_user",
        Password:   "analytics_password",
        App:        "doujins",

        // ClickHouse migrations are tracked in Postgres public.migrations
        // (database='clickhouse') and use Postgres advisory locks.
        PostgresDB: pg,
    })
    m.ApplyMigrations(ctx, migrations)
}
```

### Startup validation (read-only)

```go
// Fail app startup if any migration is pending. Never creates tables.
err := migratekit.ValidatePostgresMigrations(ctx, db,
    migratekit.MigrationSource{App: "authkit", FS: authkitFS},
    migratekit.MigrationSource{App: "billing", FS: billingFS},
)

// ClickHouse equivalent, in the chmigrate subpackage:
err = chmigrate.ValidateMigrations(ctx, &chmigrate.Config{App: "doujins", PostgresDB: pg}, clickhouseFS)
```

## Stable API (v1)

Everything in this section is the v1 compatibility boundary. Within v1.x it
will only grow — no removals, no signature changes, no breaking behavior
changes to the documented contracts below. Anything NOT listed here
(unexported helpers, exact error message text, internal locking mechanics)
is an implementation detail and may change in any release.

### Loading

| Symbol | Contract |
|---|---|
| `type Migration struct { Name, Content string }` | One SQL migration: filename + raw file content. |
| `LoadFromFS(fsys fs.FS, dir ...string) ([]Migration, error)` | Loads every `*.up.sql` in `dir` (default `"."`), ordered by numeric prefix. Errors on duplicate normalized prefixes. Only the first `dir` element is used. |
| `Prefix(name string) string` | Normalized numeric prefix of a migration filename (`"001_x.up.sql"` → `"1"`). This is the tracking key stored in `public.migrations.name`. |
| `CheckChain(names []string) error` | *(v1.5.0)* Validates a chain as a file listing — duplicate numbers, gaps, monotonicity — with no database. For a CI gate on the merge boundary. |
| `ContentDigest(content string) string` | *(v1.5.0)* The sha256 the ledger records for a migration's bytes. |
| `type AppliedRecord struct { Key, Filename, Digest string }` | *(v1.5.0)* One ledger row. `Filename`/`Digest` are empty for rows written by ≤v1.4.0. |

### Postgres

| Symbol | Contract |
|---|---|
| `NewPostgres(db *sql.DB, app string) *Postgres` | Migrator for one app's migrations. Never closes `db`. |
| `(*Postgres) WithSchema(schema string, rewriteFrom ...string) *Postgres` | Migrations run under `SET LOCAL search_path = "<schema>", public`. Optional `rewriteFrom` canonical schema names are rewritten to `schema` in migration SQL before execution, for portable hard-qualified app DDL such as `openrails.foo`. Tracking stays in `public.migrations`. |
| `(*Postgres) ApplyMigrations(ctx, []Migration) error` | The one-call path: ensures the tracking table, applies every unapplied migration in order under the advisory lock (lock taken only when there is work), records each by `Prefix`. Each migration runs in its own transaction. |
| `(*Postgres) Applied(ctx) ([]string, error)` | Recorded migration names (normalized prefixes) for this app, `database='postgres'`. |
| `(*Postgres) Setup(ctx) error` | Ensures `public.migrations` exists (idempotent). `ApplyMigrations` calls it for you. |
| `(*Postgres) ValidateAllApplied(ctx, []Migration) error` | Read-only startup gate: error naming pending migrations, never creates tables. |
| `(*Postgres) WithStrictOrdering() *Postgres` | *(v1.5.0)* Refuse a pending migration that sorts below one already applied. Opt-in. |
| `(*Postgres) AppliedRecords(ctx) (map[string]AppliedRecord, error)` | *(v1.5.0)* Ledger keyed by tracking key, carrying the recorded filename and content digest. |

### ClickHouse (`migratekit/chmigrate`, since v1.4.0)

ClickHouse is a separate subpackage so the root package never imports
`github.com/ClickHouse/clickhouse-go/v2`. Its stable surface:

| Symbol | Contract |
|---|---|
| `type Config struct { ClientAddr, Database, Username, Password, App, Cluster string; PostgresDB *sql.DB }` | `PostgresDB` is required: tracking rows live in Postgres `public.migrations` (`database='clickhouse'`) and locking uses Postgres advisory locks. `Cluster` enables `{{ON_CLUSTER}}` expansion. |
| `New(*Config) *ClickHouse` | Migrator; connects to ClickHouse lazily via native protocol. |
| `(*ClickHouse) ApplyMigrations(ctx, []migratekit.Migration) error` | Same shape as Postgres. Statements run individually (no transactions) with up-to-30s retry on transient distributed-DDL errors. |
| `(*ClickHouse) Applied(ctx) ([]string, error)` | Recorded names for this app, `database='clickhouse'`. |
| `(*ClickHouse) Setup(ctx) error` | Ensures the Postgres tracker is ready. |
| `(*ClickHouse) ValidateAllApplied(ctx, []migratekit.Migration) error` | Read-only startup gate. |
| `(*ClickHouse) Close() error` | Closes only the native ClickHouse connection the migrator itself opened (never `PostgresDB`). |
| `ValidateMigrations(ctx, *Config, fs.FS) error` | `LoadFromFS` + `ValidateAllApplied` in one call, mirroring `ValidatePostgresMigrations` for a single ClickHouse app. |

### Multi-source startup validation

| Symbol | Contract |
|---|---|
| `type MigrationSource struct { App string; FS fs.FS }` | One app's migration filesystem. |
| `ValidatePostgresMigrations(ctx, db, ...MigrationSource) error` | `ValidateAllApplied` across several apps in one call. `MigrationSource.Schema` and `RewriteFrom` mirror `WithSchema` for schema-aware callers. |

### Frozen behavioral contracts

These behaviors are part of the API and will not change within v1.x:

1. **Tracking table**: `public.migrations (id, app, database, schema, name, filename, content_sha256, migrated_at, UNIQUE(app, database, schema, name))` with `database` ∈ {`postgres`, `clickhouse`}. Operators may query and (carefully) repair it; its shape is stable.
2. **Tracking key**: the normalized numeric prefix (`Prefix`), not the filename — renaming `001_users.up.sql` to `001_accounts.up.sql` does not re-apply it. Since v1.5.0 the row also records the full filename and a content digest, so a number applied by a *different* file, or an edit to a migration that already ran, is a hard error instead of a silent skip.
3. **Discovery**: only `*.up.sql` files; `*.down.sql` is reserved; numeric-prefix ordering; duplicate prefixes are a load error.
4. **Locking**: appliers are serialized by Postgres advisory locks held on a dedicated pinned connection for the duration of the apply; the lock is taken only when unapplied migrations exist; process death releases the lock with the connection.
5. **Templates**: `{{VAR}}` / `${VAR}` substitute from the environment at apply time; an unset variable is an error; an explicitly-empty variable substitutes as-is; `{{ON_CLUSTER}}`/`${ON_CLUSTER}` expand from `chmigrate.Config.Cluster` (empty → removed).
6. **Postgres atomicity**: one migration = one transaction (DDL + tracking row commit together).
7. **Postgres schema targeting**: `WithSchema(schema)` sets a per-migration transaction search path for unqualified SQL. `WithSchema(schema, "canonical")` also rewrites app-owned canonical schema references to `schema` before execution; use this for portable hard-qualified DDL, not for shared schemas like `public`.
7. **ClickHouse non-atomicity**: statements are split (quote-aware) and run individually; a partial failure leaves earlier statements applied and the migration unrecorded — every statement must be individually idempotent.
8. **Ownership**: migrators never close a `*sql.DB` you pass in.

### Added in v1.5.0 (migration identity)

The applied-migrations ledger was keyed by the migration NUMBER alone, which is
not an identity: two different files that each claim number N normalize to the
same key, so once one is applied the other is reported "already applied" and
its DDL never runs — no error, clean boot, green suite. `LoadFromFS` rejects two
colliding files in one tree, but the damaging case never has both in one tree:
lane A's N is applied to a live database, lane B renumbers or reverts, and B's N
is skipped forever.

v1.5.0 records `filename` and `content_sha256` next to the key and checks them
on every apply:

- **Identity** — number N applied by a different file is a hard error naming
  both files and demanding a renumber.
- **Integrity** — a migration edited after it ran is a hard error.

Both are always on and cannot fire spuriously: rows written by ≤v1.4.0 carry no
identity, and unknown reads as unknown, never as a mismatch. The two new columns
are added by `Setup()`; no consumer action is required.

- **Ordering** — `WithStrictOrdering()` additionally refuses a pending migration
  that sorts below one already applied. Opt-in, because existing chains
  legitimately carry gaps and late arrivals that predate the rule.
- `CheckChain(names)` validates a chain as a plain file listing (duplicates,
  gaps, monotonicity) with no database, for a CI gate on the merge boundary.

Execution is unchanged: appliers are serialized by the advisory lock and
migrations run one at a time, in order.

### Removed in v1.0 (was public in v0.x)

- `Postgres.Apply`, `Postgres.Lock`, `Postgres.Unlock`, `ClickHouse.Apply`, `ClickHouse.Lock`, `ClickHouse.Unlock` — the manual lock/apply path bypassed the applied-set check and made stateful locking part of the surface; `ApplyMigrations` is the supported path. (No known consumer used these.)
- `Postgres.Close` — closed the caller's `*sql.DB`, which the migrator never owned.
- `MigrationSource.FS` and the `ValidateClickHouseMigrations` filesystem parameter are now `fs.FS` instead of `embed.FS` (source-compatible: `embed.FS` satisfies `fs.FS`).

### Changed in v1.4.0 (ClickHouse moved to its own subpackage)

Everything ClickHouse-related moved from the root package into
`migratekit/chmigrate`, to get `github.com/ClickHouse/clickhouse-go/v2` (and
its `ch-go` dependency) out of every Postgres-only consumer's build:

- `migratekit.ClickHouseConfig` → `chmigrate.Config` (same fields).
- `migratekit.NewClickHouse` → `chmigrate.New` (same signature, returns `*chmigrate.ClickHouse`).
- `migratekit.ClickHouse` → `chmigrate.ClickHouse` (same methods).
- `migratekit.ValidateClickHouseMigrations` → `chmigrate.ValidateMigrations`.

**Migration for existing ClickHouse consumers:** change the import to add
`"github.com/open-rails/migratekit/chmigrate"`, and replace
`migratekit.NewClickHouse(&migratekit.ClickHouseConfig{...})` with
`chmigrate.New(&chmigrate.Config{...})`. `migratekit.Migration` and
`migratekit.Prefix` are unchanged and still used for migration content.

Postgres-only consumers need no code changes; running `go mod tidy` is
enough to drop `clickhouse-go`/`ch-go` from `go.mod`/`go.sum`.

### Compatibility policy

- v1.x releases may **add** symbols, struct fields with useful zero values, and optional behavior — never remove or change what is documented above.
- Exact error message **text** is not part of the API; only the documented error conditions are. (Typed sentinel errors may be added additively later.)
- The module follows Go module semver: any future break means v2 with a new import path. The bar for v2 is intentionally very high.

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
  `chmigrate.Config.PostgresDB`.

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
is special-cased for ClickHouse (expanded from `chmigrate.Config.Cluster`).
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
