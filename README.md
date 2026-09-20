# migratekit

Minimal database migration library with app-scoped migrations and automatic locking.

## Release baseline

**v1.0.4 is the supported v1 baseline.** It replaces the retired pre-launch v1
releases and intentionally breaks their public API and database contracts.
There are no compatibility aliases or legacy-ledger conversions. Reset existing
application databases before adopting it.

Both database drivers initialize tracking automatically. Neither exposes a
`Setup` method; use normal operations such as `ApplyMigrations`. Startup validation
remains read-only. New breaking public API changes require a new major version.

Old public Go downloads cannot be erased or overwritten. Retraction metadata
excludes the retired versions from normal version queries; see
[release policy](RELEASING.md).

## Install

```bash
go get github.com/open-rails/migratekit@v1.0.4
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

## When boot refuses: operator runbook

The identity checks stop a migration from silently never running. That is worth a boot
refusal — but only if there is a way out of it that is not `psql` and a hand-written
`UPDATE public.migrations`. Use the CLI:

```bash
go run github.com/open-rails/migratekit/cmd/migratekit status \
  -app tensorhub -dir migrations/postgres -dsn "$DATABASE_URL"
```

Run the command within the application module to use its pinned migratekit
version.

`status` prints, for every discrepancy, WHAT is wrong (file, number, both digests), the
LIKELY CAUSES, and the command that resolves each one. Every boot refusal ends by
pointing at it.

| What you see | What it means | Resolution |
|---|---|---|
| `number "N" was already applied by a DIFFERENT file` **(error)** | Two lanes claimed N; the other one merged first. Your file would be recorded as already applied and its DDL would never run. | Renumber your file to the next free number. Nothing in the database changes. |
| `migration X was EDITED after it was applied` **(warning — boot proceeds)** | X's SQL tokens changed since it ran here. This database has the old schema, a fresh one gets the new. | Inspect the diff. Acknowledged intentional change: `repair accept-content N --reason "…"`; otherwise revert and add a new migration. Whitespace/comments are ignored automatically. |
| `migration X sorts BEFORE Y, which is already applied` **(error, `WithStrictOrdering` only)** | A lane branched below the high-water mark and merged late. | Renumber X above Y. Genuine backport: `apply --allow-below-applied N --reason "…"`, which applies it once and records the deviation. |
| `migration X is failed, not applied` **(error)** | A `-- migratekit:no-transaction` migration failed or was interrupted, so it may be HALF applied. The ledger holds the state rather than guessing. | Undo the partial work (a failed `CREATE INDEX CONCURRENTLY` leaves an INVALID index — `DROP INDEX` it), then `repair resolve N --rerun --reason "…"`. If the DDL actually landed, `repair resolve N --applied --reason "…"`. |
| Row 1's error, but **your files are right and the ledger is the wrong side** | A restored backup, an adopted database, or a hand-fixed row: the ledger remembers a tree that no longer exists. | `repair adopt N --reason "…"` — or `repair adopt --all-unmatched --reason "…"` when every row mismatches, which is what a restore actually looks like. |

Rows 1 and 4 are the same error with opposite fixes, which is why `status` prints both
causes and you pick. The question to ask is *which side is stale, the files or the
ledger?* Renumber when a colliding file really exists; adopt when it does not.

### The repair verbs

```bash
migratekit repair adopt 42 --reason "restored the 2026-08-10 backup"
migratekit repair adopt --all-unmatched --reason "adopted the doujins cluster"
migratekit repair accept-content 7 --reason "comment typo; DDL byte-identical"
migratekit apply --allow-below-applied 91 --reason "backport of the th#1712 index"
migratekit history          # everything anyone has ever repaired here
```

Every one of them:

- **requires `--reason`**, recorded verbatim — a ledger repair with no recorded reason is
  indistinguishable from tampering;
- **writes an audit row** to `public.migration_repairs` (verb, reason, `--operator`, the OS
  user, the host, and the old and new identity) **in the same transaction** as the change.
  A repaired ledger is visible history, not an erased one;
- **touches the ledger's identity columns only** — never schema, never DDL, and never marks
  an unapplied migration applied;
- **supports `--dry-run`**, which prints the exact before/after and writes nothing;
- **refuses to run in CI.** A repair rewrites one database's ledger after a human has read
  the diff. If every database trips over the same thing, the chain is wrong and the fix
  belongs in the repository.

The same verbs are available to Go callers as `(*Postgres).RepairAdopt`,
`RepairAdoptAllUnmatched`, `RepairAcceptContent`, `ApplyWithOrderingException` and
`RepairHistory`, all taking a `RepairRequest{Reason, Operator, DryRun}`.

## Stable API (v1)

This section describes the supported API starting at v1.0.4. It does not promise
compatibility with the retired releases. Future changes follow Go semantic
versioning; breaking this public contract requires a new major version.
Unexported helpers and exact error-message wording are implementation details.

### Loading

| Symbol | Contract |
|---|---|
| `type Migration struct { Name, Content string }` | One SQL migration: filename + raw file content. |
| `LoadFromFS(fsys fs.FS, dir ...string) ([]Migration, error)` | Loads every `*.up.sql` in `dir` (default `"."`), ordered by numeric prefix. Errors on duplicate normalized prefixes. Only the first `dir` element is used. |
| `Prefix(name string) string` | Decimal display form of a migration prefix (`"001_x.up.sql"` → `"1"`). |
| `Sequence(name string) (int64, error)` | Validates and parses the prefix stored as `BIGINT`; accepts zero through 9223372036854775807. |
| `ValidateSequences([]Migration) error` | Validates numeric, unique, increasing sequence numbers before database operations. Gaps are allowed. |
| `CheckChain(names []string) error` | Validates a chain as a file listing — duplicate numbers, gaps, monotonicity — with no database. For a CI gate on the merge boundary. |
| `ContentDigest(content string) string` | The SHA-256 of the migration body, excluding its own `-- parent:` header. |
| `SemanticContentDigest(content string) string` | Token-level PostgreSQL digest: ignores comments, whitespace, and unquoted-identifier case while preserving quoted/dollar-quoted content exactly. |
| `type AppliedRecord struct { Key, Filename, Digest, SemanticDigest, Status, Error string }` | One current ledger row. `Key` is the decimal representation of the BIGINT `sequence` column. |
| `Load(fsys fs.FS, dir string, opts ...LoadOption) ([]Migration, error)` | `LoadFromFS` plus options. `RequireParentLinks()` makes a headerless migration an error; `WithChainWarnFunc(fn)` redirects the tolerance warnings. |
| `VerifyChain(migrations []Migration, opts ...LoadOption) error` | The parent-link check on an already-loaded chain. |
| `CheckChainFS(fsys fs.FS, dir string, requireLinks bool) error` | The CI gate: `CheckChain`'s numbering rules plus parent-link validation, which needs the bytes and not just the names. |
| `CheckRepairTotality(fsys fs.FS, dir string) error` | Refuses a constraint over pre-existing data that carries no repair. Pure file analysis. |
| `Relink(dir string, RelinkOptions) ([]RelinkChange, error)` | Rewrites parent lines to match the current order. Files only — no database, no audit, CI-safe. |

### Postgres

| Symbol | Contract |
|---|---|
| `NewPostgres(db *sql.DB, app string) *Postgres` | Migrator for one app's migrations. Never closes `db`. |
| `NewPostgresFromPGXPool(pool *pgxpool.Pool, app string) (*Postgres, error)` | Creates an isolated, two-connection migration handle from a host pgx pool. The returned migrator owns that handle; call `Close` when done. The host pool is never used or mutated. |
| `(*Postgres) Close() error` | Closes the isolated database handle created by `NewPostgresFromPGXPool`; no-op for `NewPostgres` values. |
| `(*Postgres) WithSchema(schema string, rewriteFrom ...string) *Postgres` | Migrations run under `SET LOCAL search_path = "<schema>", public`. Optional `rewriteFrom` canonical schema names are rewritten to `schema` in migration SQL before execution, for portable hard-qualified app DDL such as `openrails.foo`. Tracking stays in `public.migrations`. |
| `(*Postgres) ApplyMigrations(ctx, []Migration) error` | The one-call path: atomically initializes the tracking tables under the global bootstrap lock, then applies every unapplied migration in order under the migration advisory lock (lock taken only when there is work), records each by `Prefix`. Each migration runs in its own transaction. |
| `(*Postgres) Applied(ctx) ([]string, error)` | Recorded sequences in decimal form, numerically ordered for this app, `database='postgres'`. |
| `(*Postgres) ValidateAllApplied(ctx, []Migration) error` | Read-only startup gate: error naming pending migrations, never creates tables. |
| `(*Postgres) WithStrictOrdering() *Postgres` | Refuse a pending migration that sorts below one already applied. Opt-in. |
| `(*Postgres) AppliedRecords(ctx) (map[string]AppliedRecord, error)` | Ledger keyed by tracking key, carrying the recorded filename and content digest. |
| `(*Postgres) WithWarnFunc(func(Discrepancy)) *Postgres` | Replace the warning sink. Default logs through `slog.Default()` at warn level; never silent unless you make it so. |
| `(*Postgres) Status(ctx, []Migration) (Status, error)` | Applied set, pending set, every discrepancy with cause and resolution, and the repair history. Tracker tables are initialized automatically. |
| `(*Postgres) RepairAdopt(ctx, Migration, RepairRequest) (RepairResult, error)` | Bind the file in the tree as the applied identity for its number. For a ledger that is the stale side. |
| `(*Postgres) RepairAdoptAllUnmatched(ctx, []Migration, RepairRequest) ([]RepairResult, error)` | The same for every mismatched row at once — the restored-backup shape. |
| `(*Postgres) RepairAcceptContent(ctx, Migration, RepairRequest) (RepairResult, error)` | Re-stamp the digest after a verified edit; clears the drift warning. Refuses on an identity mismatch. |
| `(*Postgres) ApplyWithOrderingException(ctx, []Migration, allowBelow []string, RepairRequest) error` | Apply with a one-shot exemption from the ordering rule. Identity checks are not relaxed. |
| `(*Postgres) RepairHistory(ctx) ([]RepairRecord, error)` | The audit trail, newest first. |
| `type RepairRequest struct { Reason, Operator string; DryRun bool }` | `Reason` is required. Every repair refuses under CI (`DetectCI`). |
| `type Status`, `type Discrepancy`, `type RepairResult`, `type RepairRecord`, `Severity`, `DiscrepancyKind` | Reporting types. `Discrepancy.String()` is the full explanation; `OneLine()` is the log-line form. |
| `DetectCI() (string, bool)` | Names the CI environment variable that is set, if any. |

### ClickHouse (`migratekit/chmigrate`)

ClickHouse is a separate subpackage so the root package never imports
`github.com/ClickHouse/clickhouse-go/v2`. Its stable surface:

| Symbol | Contract |
|---|---|
| `type Config struct { ClientAddr, Database, Username, Password, App, Cluster string; PostgresDB *sql.DB }` | `PostgresDB` is required: tracking rows live in Postgres `public.migrations` (`database='clickhouse'`) and locking uses Postgres advisory locks. `Cluster` enables `{{ON_CLUSTER}}` expansion. |
| `New(*Config) *ClickHouse` | Migrator; connects to ClickHouse lazily via native protocol. |
| `(*ClickHouse) ApplyMigrations(ctx, []migratekit.Migration) error` | Same shape as Postgres. Statements run individually (no transactions) with up-to-30s retry on transient distributed-DDL errors. |
| `(*ClickHouse) Applied(ctx) ([]string, error)` | Recorded sequences for this app, `database='clickhouse'`; initializes the Postgres tracker automatically. |
| `(*ClickHouse) ValidateAllApplied(ctx, []migratekit.Migration) error` | Read-only startup gate. |
| `(*ClickHouse) Close() error` | Closes only the native ClickHouse connection the migrator itself opened (never `PostgresDB`). |
| `ValidateMigrations(ctx, *Config, fs.FS) error` | `LoadFromFS` + `ValidateAllApplied` in one call, mirroring `ValidatePostgresMigrations` for a single ClickHouse app. |

### Multi-source startup validation

| Symbol | Contract |
|---|---|
| `type MigrationSource struct { App string; FS fs.FS }` | One app's migration filesystem. |
| `ValidatePostgresMigrations(ctx, db, ...MigrationSource) error` | `ValidateAllApplied` across several apps in one call. `MigrationSource.Schema` and `RewriteFrom` mirror `WithSchema` for schema-aware callers. |

### Frozen behavioral contracts

These behaviors define the current API:

1. **Tracking table**: `public.migrations (id, app, database, schema, sequence, filename, content_sha256, semantic_sha256, status, error, migrated_at, UNIQUE(app, database, schema, sequence))` with `database` ∈ {`postgres`, `clickhouse`}. `public.migration_repairs` records every repair.
2. **Tracking key**: the normalized numeric prefix (`Prefix`), not the filename. A different file claiming an applied number is a hard error. Edited SQL is always an operator warning; comment/format-only edits compare cleanly through `semantic_sha256`.
3. **Discovery**: only `*.up.sql` files; `*.down.sql` is reserved; numeric-prefix ordering; duplicate prefixes are a load error.
4. **Locking**: appliers are serialized by Postgres advisory locks held on a dedicated pinned connection for the duration of the apply; the lock is taken only when unapplied migrations exist; process death releases the lock with the connection.
5. **Templates**: `{{VAR}}` / `${VAR}` substitute from the environment at apply time; an unset variable is an error; an explicitly-empty variable substitutes as-is; `{{ON_CLUSTER}}`/`${ON_CLUSTER}` expand from `chmigrate.Config.Cluster` (empty → removed).
6. **Postgres atomicity**: one migration = one transaction — the DDL and its ledger row commit together, so a failed migration applies nothing and records nothing. **The one exception is explicit**: a migration whose leading comment block carries `-- migratekit:no-transaction` runs outside a transaction and can fail half-applied; the ledger then records it `failed` (or `running` after a crash) and boot refuses until an operator resolves it.
7. **Postgres schema targeting**: `WithSchema(schema)` sets a per-migration transaction search path for unqualified SQL. `WithSchema(schema, "canonical")` also rewrites app-owned canonical schema references to `schema` before execution; use this for portable hard-qualified DDL, not for shared schemas like `public`.
8. **ClickHouse non-atomicity**: statements are split (quote-aware) and run individually; a partial failure leaves earlier statements applied and the migration unrecorded — every statement must be individually idempotent.
9. **Ownership**: migrators never close a `*sql.DB` you pass in.

### Identity, content drift, and repairs

Each migration is identified by its numeric sequence within its application,
database driver, and configured schema. A different filename claiming an applied
sequence is an error. Edited SQL emits a warning; comments, whitespace, and
unquoted-identifier case are ignored by the semantic digest. Content drift does
not block startup. `WithStrictOrdering` optionally rejects pending migrations
below an already-applied sequence.

`Status` explains discrepancies. The CLI repair commands and their corresponding
Go methods record the operator's reason and ledger change together in
`public.migration_repairs`. They do not reconstruct application tables or convert
an older ledger schema.

### Parent links and authoring checks

A migration may declare its immediate predecessor on the first non-blank line:

```sql
-- parent: 5 sha256:<digest-of-the-parent-body>
```

The first migration declares `-- parent: root`. `Load` verifies declared links
while reading files, before any database access. `RequireParentLinks()` also
rejects files without a header. Parent links must match the preceding migration's
number and canonical content digest. The digest excludes the migration's own
parent header.

After renumbering or editing migrations, update their parent links with:

```bash
migratekit relink -dir migrations/postgres
migratekit relink -dir migrations/postgres --check
```

`relink` edits source files only. It does not write to the database or need a
repair reason. A squashed chain starts with a new root file.

`CheckRepairTotality` and `migratekit check` inspect constraints on pre-existing
tables. Repair DML must precede the constraint, or the file must explain why no
repair is needed with `-- Repair: none-needed <reason>`. Tables created in the same
migration are exempt. Unresolved statement targets require an explicit waiver.

```bash
migratekit check -dir migrations/postgres --require-links
```

### Non-transactional PostgreSQL migrations

Every PostgreSQL migration runs in a transaction unless its leading comment
block explicitly contains:

```sql
-- migratekit:no-transaction
```

Use this for commands such as `CREATE INDEX CONCURRENTLY` that cannot run inside
a transaction. Migratekit does not infer transaction support by scanning SQL.
Statements execute individually under the migration lock. The ledger records
`running`, then `applied` on success or `failed` with the error on failure. A crash
may leave `running`.

An unfinished non-transactional migration requires operator resolution before
another apply. Inspect and repair the database, then use
`migratekit repair resolve N --applied|--rerun --reason "..."`. Ordinary
transactional failures roll back both the migration and its ledger row.

### Compatibility policy

The numeric-ledger release is an intentional pre-launch database hard cut.
All controlled applications must reset their databases before adopting it.
It retains the Go module path and public string keys used by reporting APIs;
the database columns and bound sequence values are BIGINT/int64. There is no
legacy ledger conversion path.


## Schema

migratekit creates two tables in the `public` schema automatically on the first
operation that needs tracking. This is a fresh-database hard cut: reset the application databases before
adopting it. No table renames, column upgrades, or digest backfills run.
Initialization never drops or converts existing tables.

```sql
CREATE TABLE public.migrations (
    id BIGSERIAL PRIMARY KEY,
    app TEXT NOT NULL,
    database TEXT NOT NULL,
    schema TEXT NOT NULL DEFAULT '',
    sequence BIGINT NOT NULL CHECK (sequence >= 0),       -- the numeric ledger key: Prefix(filename)
    filename TEXT,
    content_sha256 TEXT,
    semantic_sha256 TEXT,
    status TEXT NOT NULL DEFAULT 'applied',  -- applied | running | failed
    "error" TEXT,                   -- why a no-transaction apply failed
    migrated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE(app, database, schema, sequence)
);

-- Each repair is recorded in the same transaction as its ledger change.
CREATE TABLE public.migration_repairs (
    id BIGSERIAL PRIMARY KEY,
    app TEXT NOT NULL,
    database TEXT NOT NULL,
    schema TEXT NOT NULL DEFAULT '',
    sequence BIGINT NOT NULL CHECK (sequence >= 0),       -- the numeric ledger key repaired
    verb TEXT NOT NULL,
    reason TEXT NOT NULL,
    operator TEXT NOT NULL DEFAULT '',
    os_user TEXT NOT NULL DEFAULT '',
    host TEXT NOT NULL DEFAULT '',
    old_filename TEXT, old_digest TEXT,
    new_filename TEXT, new_digest TEXT,
    repaired_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
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

- **Number**: An integer from 0 through 9223372036854775807 (leading zeros are optional)
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

Files without the `.up.sql` suffix are ignored. A `.up.sql` file with an invalid
numeric prefix is rejected.

❌ **Invalid:**
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
- `001`, `01`, `1` all become integer `1`
- `042`, `42` both become integer `42`

**Ordering:** migrations apply in numeric-prefix order (`2_x` before `10_x`),
not lexical filename order, so unpadded prefixes are safe.

**Duplicate prefixes are rejected:** because tracking is keyed by the
normalized prefix, two files sharing a prefix (`002_users.up.sql` +
`002_roles.up.sql`, or `0042_x` vs `42_y`) would mean the second silently
never runs. `LoadFromFS` returns an error instead.

**Parent line:** the first non-blank line of a migration is
`-- parent: <number> sha256:<digest-of-the-parent's-canonical-body>`, or
`-- parent: root` for the lowest-numbered file. The whole chain is verified at load.
Renumbering means `git mv` *and* updating that line — `migratekit relink` does both parts
of the second half. Use `RequireParentLinks()` to require headers on every file.

**Repair rule:** a migration that adds a constraint over a table that already has
rows must carry, ABOVE the constraint, the DML that repairs the offending rows — or
`-- Repair: none-needed <reason>`. Tables created in the same file are exempt. Measuring
the real data first is how you decide between deleting and backfilling; the gate enforces
the repair, not the measurement.

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
- **Resolvable**: every refusal has a `migratekit status` explanation and an audited repair verb
- **Self-contained**: Creates own tables on first run
- **Minimal**: small codebase + minimal dependencies

## Design

### Why lock only when needed?
Checking what's applied (`SELECT`) doesn't need a lock. Only write operations need locks. This allows multiple services to check migrations concurrently without blocking.

### Why per-app scoping?
Different apps (doujins, hentai0, billing) have independent migration sequences and can migrate concurrently.

### Why single table?
Easy to query "show all migrations" and simpler permissions.
