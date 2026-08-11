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

## When boot refuses: operator runbook

The identity checks stop a migration from silently never running. That is worth a boot
refusal — but only if there is a way out of it that is not `psql` and a hand-written
`UPDATE public.migrations`. Since v1.6.0 there is:

```bash
go run github.com/open-rails/migratekit/cmd/migratekit@latest status \
  -app tensorhub -dir migrations/postgres -dsn "$DATABASE_URL"
```

`status` prints, for every discrepancy, WHAT is wrong (file, number, both digests), the
LIKELY CAUSES, and the command that resolves each one. Every boot refusal ends by
pointing at it.

| What you see | What it means | Resolution |
|---|---|---|
| `number "N" was already applied by a DIFFERENT file` **(error)** | Two lanes claimed N; the other one merged first. Your file would be recorded as already applied and its DDL would never run. | Renumber your file to the next free number. Nothing in the database changes. |
| `migration X was EDITED after it was applied` **(warning — boot proceeds)** | X's bytes changed since it ran here. This database has the old schema, a fresh one gets the new. | Cosmetic edit: `repair accept-content N --reason "…"`. Otherwise revert the file and add a new migration. |
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
| `ContentDigest(content string) string` | *(v1.5.0)* The sha256 the ledger records for a migration. **Since v1.7.0 it hashes the CANONICAL BODY** — the file with its own `-- parent:` header removed — so adding a parent line to an applied migration changes no ledger digest. A headerless file hashes exactly as it did in v1.5.0. |
| `type AppliedRecord struct { Key, Filename, Digest, Status, Error string }` | One ledger row. `Filename`/`Digest` are empty for rows written by ≤v1.4.0; `Status`/`Error` (v1.7.0) are empty for rows written by ≤v1.6.0, which are applied by construction. |
| `Load(fsys fs.FS, dir string, opts ...LoadOption) ([]Migration, error)` | *(v1.7.0)* `LoadFromFS` plus options. `RequireParentLinks()` makes a headerless migration an error; `WithChainWarnFunc(fn)` redirects the tolerance warnings. |
| `VerifyChain(migrations []Migration, opts ...LoadOption) error` | *(v1.7.0)* The parent-link check on an already-loaded chain. |
| `CheckChainFS(fsys fs.FS, dir string, requireLinks bool) error` | *(v1.7.0)* The CI gate: `CheckChain`'s numbering rules plus parent-link validation, which needs the bytes and not just the names. |
| `CheckRepairTotality(fsys fs.FS, dir string) error` | *(v1.7.0)* Refuses a constraint over pre-existing data that carries no repair. Pure file analysis. |
| `Relink(dir string, RelinkOptions) ([]RelinkChange, error)` | *(v1.7.0)* Rewrites parent lines to match the current order. Files only — no database, no audit, CI-safe. |

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
| `(*Postgres) WithStrictContent() *Postgres` | *(v1.6.0)* Turn content drift back into a hard error. Default is a warning. |
| `(*Postgres) WithWarnFunc(func(Discrepancy)) *Postgres` | *(v1.6.0)* Replace the warning sink. Default logs through `slog.Default()` at warn level; never silent unless you make it so. |
| `(*Postgres) Status(ctx, []Migration) (Status, error)` | *(v1.6.0)* Applied set, pending set, every discrepancy with cause and resolution, and the repair history. Read-only apart from `Setup`. |
| `(*Postgres) RepairAdopt(ctx, Migration, RepairRequest) (RepairResult, error)` | *(v1.6.0)* Bind the file in the tree as the applied identity for its number. For a ledger that is the stale side. |
| `(*Postgres) RepairAdoptAllUnmatched(ctx, []Migration, RepairRequest) ([]RepairResult, error)` | *(v1.6.0)* The same for every mismatched row at once — the restored-backup shape. |
| `(*Postgres) RepairAcceptContent(ctx, Migration, RepairRequest) (RepairResult, error)` | *(v1.6.0)* Re-stamp the digest after a verified edit; clears the drift warning. Refuses on an identity mismatch. |
| `(*Postgres) ApplyWithOrderingException(ctx, []Migration, allowBelow []string, RepairRequest) error` | *(v1.6.0)* Apply with a one-shot exemption from the ordering rule. Identity checks are not relaxed. |
| `(*Postgres) RepairHistory(ctx) ([]RepairRecord, error)` | *(v1.6.0)* The audit trail, newest first. |
| `type RepairRequest struct { Reason, Operator string; DryRun bool }` | *(v1.6.0)* `Reason` is required. Every repair refuses under CI (`DetectCI`). |
| `type Status`, `type Discrepancy`, `type RepairResult`, `type RepairRecord`, `Severity`, `DiscrepancyKind` | *(v1.6.0)* Reporting types. `Discrepancy.String()` is the full explanation; `OneLine()` is the log-line form. |
| `DetectCI() (string, bool)` | *(v1.6.0)* Names the CI environment variable that is set, if any. |

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

1. **Tracking table**: `public.migrations (id, app, database, schema, name, filename, content_sha256, status, error, migrated_at, UNIQUE(app, database, schema, name))` with `database` ∈ {`postgres`, `clickhouse`}. Its shape is stable. Since v1.6.0 `public.migration_repairs` records every repair; prefer `migratekit repair` over editing either table by hand, because only the verbs write the audit row.
2. **Tracking key**: the normalized numeric prefix (`Prefix`), not the filename — renaming `001_users.up.sql` to `001_accounts.up.sql` does not re-apply it. Since v1.5.0 the row also records the full filename and a content digest: a number applied by a *different* file is a hard error instead of a silent skip, and an edit to a migration that already ran is reported (a warning since v1.6.0, an error under `WithStrictContent`).
3. **Discovery**: only `*.up.sql` files; `*.down.sql` is reserved; numeric-prefix ordering; duplicate prefixes are a load error.
4. **Locking**: appliers are serialized by Postgres advisory locks held on a dedicated pinned connection for the duration of the apply; the lock is taken only when unapplied migrations exist; process death releases the lock with the connection.
5. **Templates**: `{{VAR}}` / `${VAR}` substitute from the environment at apply time; an unset variable is an error; an explicitly-empty variable substitutes as-is; `{{ON_CLUSTER}}`/`${ON_CLUSTER}` expand from `chmigrate.Config.Cluster` (empty → removed).
6. **Postgres atomicity**: one migration = one transaction — the DDL and its ledger row commit together, so a failed migration applies nothing and records nothing. **The one exception is explicit**: a migration whose leading comment block carries `-- migratekit:no-transaction` runs outside a transaction and can fail half-applied; the ledger then records it `failed` (or `running` after a crash) and boot refuses until an operator resolves it.
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

### Changed in v1.6.0 (the resolution path)

v1.5.0 made three failures visible. It did not make any of them *resolvable*: the boot
refused, named the problem, and left the operator with `psql`. v1.6.0 is the other half —
`status`, four audited repair verbs, and one relaxation.

**Content drift is now a WARNING, not a boot error.** An operator who edited a migration
that already ran often cannot restore the old bytes, and a database held down over a
comment change is a worse outcome than the divergence the check guards against. The boot
proceeds and emits a warning naming the file, both digests, the risk, and
`repair accept-content`. `WithStrictContent()` restores the v1.5.0 refusal.

The other two are unchanged. A number applied by a different file is still a hard error —
it is the silent-never-runs killer, and its fix (renumber, or adopt) is always available.
`WithStrictOrdering()` behaves exactly as before.

Everything else is additive: `Status`, `WithWarnFunc`, the repair verbs, the
`public.migration_repairs` audit table (created by `Setup`), and the `cmd/migratekit` CLI.
No consumer action is required, and no call site changes.

### Added in v1.7.0 (parent links, no-transaction, repair totality)

**Parent-hash links.** Every migration carries its parent as its first non-blank line:

```sql
-- parent: 5 sha256:9f2c…e1
```

and the first migration of the chain carries `-- parent: root`. The chain is verified at
LOAD — pure file reading, no database — so boot and CI both inherit it, and there is no
second verification point to keep in sync.

Two things follow. An ordering conflict becomes structurally impossible: the lane that
merges second has a parent line pointing at a file that is no longer its predecessor,
which is a deterministic refusal in its own PR rather than a boot refusal in production.
And the directory becomes a hash chain: tampering with history breaks every later link,
which is `atlas.sum`'s property with no sum file to maintain.

Refused, each naming both files: a missing parent, a hash mismatch, two files claiming the
same parent, two roots, a root that is not the lowest-numbered file, a link that points
forward, and a link that skips the immediate predecessor (the stale-after-renumber shape).

**The digest excludes the parent line.** `ContentDigest` hashes the canonical body — the
file with its own header removed — and that is also what `content_sha256` stores. Adding
parent lines to already-applied migrations therefore changes no ledger digest: adoption is
silent, every digest v1.5.0 wrote is still correct, and there is nothing to backfill.

**Renumbering** is now `git mv` PLUS updating your parent line. The error message says so,
and `migratekit relink` does it in one command:

```bash
migratekit relink -dir migrations/postgres            # fix the parent lines
migratekit relink -dir migrations/postgres --check    # CI: exit 1 if any are stale
```

`relink` is an AUTHORING verb and deliberately does NOT carry the repair verbs' guardrails
— no `--reason`, no audit row, no CI refusal. The repair verbs mutate a production ledger:
state that is invisible, shared, and has no history of its own. `relink` mutates files,
and files already have an audit log — git. A `--reason` would duplicate the commit
message, an audit table cannot record a change that may never be committed, and refusing
to run in CI would be wrong because `relink --check` *is* the CI gate.

**A squash resets the chain root**, with no special case in the code: a squash deletes the
files it replaces, so the squash file is the lowest-numbered file present and legitimately
carries `-- parent: root`. One rule — exactly one root, and it must be the lowest-numbered
file — covers squashes and ordinary chains alike.

**Adoption.** Headerless migrations are tolerated with a warning for one minor version.
Mixed chains work: a headed file verifies against a headerless parent (hashing a parent
does not require the parent to have a header), so a repo adopts by heading its newest file
and letting the rest follow. `Load(fsys, dir, RequireParentLinks())` makes a missing header
an error. The default flips no earlier than v1.8.0.

**`-- migratekit:no-transaction`.** Migrations are transactional by default and the ledger
row commits with the DDL. `CREATE INDEX CONCURRENTLY` — the only index build that does not
take an `ACCESS EXCLUSIVE` lock on a live table — cannot live inside that, so a migration
whose leading comment block carries the directive runs outside a transaction, one
statement at a time, still under the advisory lock.

Such a migration may be PARTIALLY applied, and no design makes it atomic. So the ledger
holds the state instead of hiding it: `running` before it executes, `applied` on success,
`failed` with the Postgres error text on failure, and still `running` if the process dies.
**Boot refuses on any row that is not `applied`** — not absent, which would silently re-run
half-applied DDL, and not applied, which would silently skip the other half. The operator
clears it with the audited `migratekit repair resolve N --applied|--rerun --reason "…"`.

**Repair totality.** A constraint added over a table that already has rows is a bet that
the rows comply, and when the bet loses it fails on a live database mid-boot. So:

> A constraint over a PRE-EXISTING table must be preceded, in the same file, by either the
> repair DML that makes the data satisfy it, or `-- Repair: none-needed <reason>`.

Position is the rule, not a detail: a repair below the constraint runs after the
constraint has already refused the rows. A table `CREATE TABLE`d in the same file is
exempt — no pre-existing row can exist. Detection is a conservative lexical scan of the
closed set of DDL that can refuse stored rows (`ADD CONSTRAINT … CHECK` / `FOREIGN KEY` /
`UNIQUE`, `VALIDATE CONSTRAINT`, `CREATE UNIQUE INDEX`, `SET NOT NULL`, `ALTER COLUMN …
TYPE`, `ADD COLUMN … NOT NULL` with no default); when it cannot resolve which table a
statement targets it reports UNKNOWN and requires the waiver rather than guessing.

Measuring the real data is how you *choose* the repair — delete versus backfill is a
semantic call you cannot make without looking at the rows — but nothing requires or parses
a measurement. `CheckRepairTotality` enforces the repair.

```bash
migratekit check -dir migrations/postgres --require-links   # numbering + links + repair rule
```

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

migratekit creates two tables in the `public` schema on first `Setup()`:

```sql
CREATE TABLE public.migrations (
    id BIGSERIAL PRIMARY KEY,
    app TEXT NOT NULL,
    database TEXT NOT NULL,
    schema TEXT NOT NULL DEFAULT '',
    name TEXT NOT NULL,             -- the ledger KEY: Prefix(filename)
    filename TEXT,                  -- v1.5.0 identity; NULL on older rows
    content_sha256 TEXT,            -- v1.5.0 integrity; NULL on older rows
    status TEXT NOT NULL DEFAULT 'applied',  -- v1.7.0: applied | running | failed
    "error" TEXT,                   -- v1.7.0: why a no-transaction apply failed
    migrated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE(app, database, schema, name)
);

-- v1.6.0: every `migratekit repair` lands here, in the same transaction as the
-- identity change it made. The checks are only worth having if there is a way
-- past them; this is what keeps that way honest.
CREATE TABLE public.migration_repairs (
    id BIGSERIAL PRIMARY KEY,
    app TEXT NOT NULL,
    database TEXT NOT NULL,
    schema TEXT NOT NULL DEFAULT '',
    name TEXT NOT NULL,             -- the ledger key repaired
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

**Parent line (v1.7.0):** the first non-blank line of a migration is
`-- parent: <number> sha256:<digest-of-the-parent's-canonical-body>`, or
`-- parent: root` for the lowest-numbered file. The whole chain is verified at load.
Renumbering means `git mv` *and* updating that line — `migratekit relink` does both parts
of the second half. Headerless files are tolerated with a warning for one minor version.

**Repair rule (v1.7.0):** a migration that adds a constraint over a table that already has
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
