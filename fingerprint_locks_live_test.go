package migratekit

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

// The lock table is one budget for the whole server: about
// max_locks_per_transaction × max_connections entries, shared by every
// session. A comparison must not spend it in proportion to the schema it reads.

type lockReader interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

// heldLocks counts the locks the session holds, the one on pg_locks included.
func heldLocks(t *testing.T, q lockReader) int {
	t.Helper()
	var n int
	if err := q.QueryRowContext(context.Background(),
		`SELECT count(*) FROM pg_catalog.pg_locks WHERE pid = pg_catalog.pg_backend_pid()`).Scan(&n); err != nil {
		t.Fatalf("count locks: %v", err)
	}
	return n
}

// lockKinds lists the session's locks by kind, the most numerous first, after
// the number of entries they take in the shared lock table: one per locked
// object, and none for a fast-path lock.
func lockKinds(t *testing.T, q lockReader) string {
	t.Helper()
	var shared int
	if err := q.QueryRowContext(context.Background(), `
		SELECT count(DISTINCT concat_ws('/', locktype, database, relation, page, tuple, virtualxid, transactionid, classid, objid, objsubid))
		  FROM pg_catalog.pg_locks WHERE pid = pg_catalog.pg_backend_pid() AND NOT fastpath`).Scan(&shared); err != nil {
		t.Fatalf("count lock table entries: %v", err)
	}
	rows, err := q.QueryContext(context.Background(), `
		SELECT locktype || ' ' || mode || CASE WHEN fastpath THEN ' (fast path)' ELSE '' END, count(*)
		  FROM pg_catalog.pg_locks WHERE pid = pg_catalog.pg_backend_pid()
		 GROUP BY 1 ORDER BY 2 DESC, 1`)
	if err != nil {
		t.Fatalf("list locks: %v", err)
	}
	defer rows.Close()
	var kinds []string
	for rows.Next() {
		var kind string
		var n int
		if err := rows.Scan(&kind, &n); err != nil {
			t.Fatal(err)
		}
		kinds = append(kinds, fmt.Sprintf("%d %s", n, kind))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("%d shared lock table entries; %s", shared, strings.Join(kinds, "; "))
}

// lockHeavyBaseline is a schema of ordinary shape: each table has a primary
// key, five CHECKs, five defaults and two plain indexes, which is twelve
// canonicalizations and six relations (table, TOAST table and its index,
// three indexes). A partitioned table, a view and a policy come with it.
func lockHeavyBaseline(tables int) string {
	var b strings.Builder
	for i := 0; i < tables; i++ {
		fmt.Fprintf(&b, `
CREATE TABLE t%[1]d (
	id bigint PRIMARY KEY,
	a text NOT NULL DEFAULT 'a' CONSTRAINT t%[1]d_a_check CHECK (a <> ''),
	b text NOT NULL DEFAULT 'b' CONSTRAINT t%[1]d_b_check CHECK (b IN ('b', 'c')),
	c integer NOT NULL DEFAULT 0 CONSTRAINT t%[1]d_c_check CHECK (c >= 0),
	d integer NOT NULL DEFAULT 1 CONSTRAINT t%[1]d_d_check CHECK (d >= 0),
	e timestamptz NOT NULL DEFAULT now(),
	CONSTRAINT t%[1]d_cd_check CHECK (c + d < 1000000)
);
CREATE INDEX t%[1]d_a_idx ON t%[1]d (a);
CREATE INDEX t%[1]d_cd_idx ON t%[1]d (c, d);
`, i)
	}
	b.WriteString(`
CREATE TABLE events (
	id bigint NOT NULL,
	at timestamptz NOT NULL,
	kind text NOT NULL DEFAULT 'k' CONSTRAINT events_kind_check CHECK (kind <> ''),
	CONSTRAINT events_pkey PRIMARY KEY (id, at)
) PARTITION BY RANGE (at);
CREATE TABLE events_y2026m10 PARTITION OF events FOR VALUES FROM ('2026-10-01 00:00:00+00') TO ('2026-11-01 00:00:00+00');
CREATE TABLE events_y2026m11 PARTITION OF events FOR VALUES FROM ('2026-11-01 00:00:00+00') TO ('2026-12-01 00:00:00+00');
CREATE VIEW recent AS SELECT id, a FROM t0 WHERE c > 0;
ALTER TABLE t0 ENABLE ROW LEVEL SECURITY;
CREATE POLICY t0_positive ON t0 USING (c >= 0);
`)
	return b.String()
}

// renamedIndex is an edit that changes a migration's content and nothing a
// database does.
func renamedIndex(baseline string) string {
	return strings.Replace(baseline, "t0_a_idx", "t0_by_a", 1)
}

func recordedDigest(t *testing.T, db *sql.DB, app string) string {
	t.Helper()
	var digest string
	if err := db.QueryRow(`SELECT content_sha256 FROM public.migrations WHERE app = $1 AND sequence = 1`, app).Scan(&digest); err != nil {
		t.Fatal(err)
	}
	return digest
}

// A canonicalizer builds its expression on a scratch table. Whatever it
// locked to do that must be released when it returns, not when the
// fingerprint ends: a schema has one call per CHECK, default, index, policy
// and view.
func TestCanonicalizersReleaseTheirLocks(t *testing.T) {
	db := convDB(t)
	ctx := context.Background()
	buildChain(t, db, "mk-locks-canon", "mk_locks_canon", []Migration{{Name: "0001_schema.up.sql", Content: `
CREATE TABLE t (id bigint PRIMARY KEY, name text NOT NULL DEFAULT 'x', n integer NOT NULL DEFAULT 0, CONSTRAINT t_name_check CHECK (name <> ''));
CREATE INDEX t_name_idx ON t (name);
`}})
	// A call may leave this many locks behind in total, however often it runs.
	const slack = 8
	for _, c := range []struct{ name, call string }{
		{"mk_canon_check", `pg_temp.mk_canon_check('mk_locks_canon.t'::regclass, $c$CHECK (name <> '')$c$)`},
		{"mk_canon_default", `pg_temp.mk_canon_default('mk_locks_canon.t'::regclass, 'name', $c$'x'$c$)`},
		{"mk_canon_index", `pg_temp.mk_canon_index('mk_locks_canon.t'::regclass, pg_get_indexdef('mk_locks_canon.t_name_idx'::regclass))`},
		{"mk_canon_policy", `pg_temp.mk_canon_policy('mk_locks_canon.t'::regclass, 'n > 0', 'using')`},
		{"mk_canon_view", `pg_temp.mk_canon_view('SELECT id, name FROM mk_locks_canon.t')`},
	} {
		func() {
			tx, err := db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			if _, err := tx.ExecContext(ctx, "SET LOCAL search_path = pg_catalog"); err != nil {
				t.Fatal(err)
			}
			if _, err := tx.ExecContext(ctx, canonicalizers); err != nil {
				t.Fatalf("install canonicalizers: %v", err)
			}
			// Every call must still return the canonical form, the same one.
			run := func(calls int) {
				t.Helper()
				var returned, distinct int
				if err := tx.QueryRowContext(ctx, fmt.Sprintf(
					`SELECT count(x), count(DISTINCT x) FROM (SELECT %s AS x FROM generate_series(1, %d)) calls`, c.call, calls)).
					Scan(&returned, &distinct); err != nil {
					t.Fatalf("%s: %v", c.name, err)
				}
				if returned != calls || distinct != 1 {
					t.Fatalf("%s: %d of %d calls returned a canonical form, %d distinct", c.name, returned, calls, distinct)
				}
			}
			before := heldLocks(t, tx)
			run(1)
			one := heldLocks(t, tx)
			run(100)
			hundred := heldLocks(t, tx)
			t.Logf("%s: locks held: %d before, %d after 1 call, %d after 101: %s", c.name, before, one, hundred, lockKinds(t, tx))
			if hundred-before > slack {
				t.Errorf("%s keeps %d locks after 101 calls; each call must release what it took", c.name, hundred-before)
				return
			}
			run(1000)
			thousand := heldLocks(t, tx)
			t.Logf("%s: %d locks held after 1101 calls", c.name, thousand)
			if thousand-before > slack {
				t.Errorf("%s keeps %d locks after 1101 calls; each call must release what it took", c.name, thousand-before)
			}
		}()
	}
}

// The schema is large enough that locks kept per canonicalization exhaust a
// default lock table (64 × 100): 400 tables are 4800 canonicalizations. Read
// correctly, a fingerprint holds what reading the catalog takes and no more.
func TestFingerprintLocksDoNotGrowWithTheSchema(t *testing.T) {
	db := convDB(t)
	ctx := context.Background()
	const app, schema, tables = "mk-locks-big", "mk_locks_big", 400
	// The system catalogs a fingerprint reads, their indexes, and the
	// relations a view's query names.
	const catalogLocks = 200

	var perTx, conns, prepared int
	if err := db.QueryRow(`SELECT current_setting('max_locks_per_transaction')::int, current_setting('max_connections')::int,
		current_setting('max_prepared_transactions')::int`).Scan(&perTx, &conns, &prepared); err != nil {
		t.Fatal(err)
	}
	t.Logf("server: max_locks_per_transaction=%d max_connections=%d max_prepared_transactions=%d, a lock table of about %d",
		perTx, conns, prepared, perTx*(conns+prepared))

	baseline := lockHeavyBaseline(tables)
	buildChain(t, db, app, schema, []Migration{{Name: "0001_schema.up.sql", Content: baseline}})

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	before := heldLocks(t, tx)
	fp, err := readFingerprint(ctx, tx, schema)
	if err != nil {
		_ = tx.Rollback()
		t.Fatalf("fingerprint of %d tables: %v", tables, err)
	}
	held := heldLocks(t, tx)
	t.Logf("fingerprint of %d tables (%d lines): %d locks held before, %d after: %s", tables, len(fp), before, held, lockKinds(t, tx))
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if held-before > catalogLocks {
		t.Fatalf("a fingerprint of %d tables holds %d locks; it may hold %d however large the schema", tables, held-before, catalogLocks)
	}

	// What stays proportional to the schema is the reference build: it holds
	// a lock on every relation it creates until it is rolled back.
	p := NewPostgres(db, app).WithSchema(schema)
	if tx, err = db.BeginTx(ctx, nil); err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `CREATE SCHEMA mk_locks_big_build`); err != nil {
		t.Fatal(err)
	}
	before = heldLocks(t, tx)
	if err := p.execIn(ctx, tx, "mk_locks_big_build", baseline); err != nil {
		t.Fatalf("build %d tables in one transaction: %v", tables, err)
	}
	built := heldLocks(t, tx) - before
	kinds := lockKinds(t, tx)
	var relations int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) + 2 * count(*) FILTER (WHERE reltoastrelid <> 0)
		FROM pg_catalog.pg_class WHERE relnamespace = 'mk_locks_big_build'::regnamespace`).Scan(&relations); err != nil {
		t.Fatal(err)
	}
	t.Logf("building %d tables: %d relations (tables, TOAST tables, indexes, a view), %d locks held until the build is rolled back: %s",
		tables, relations, built, kinds)
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if built > 2*relations {
		t.Fatalf("building %d relations holds %d locks; expected fewer than two each", relations, built)
	}

	// Strict integrity reads the live schema, builds the tree and reads that.
	edited := []Migration{{Name: "0001_schema.up.sql", Content: renamedIndex(baseline)}}
	if err := p.WithStrictIntegrity().ApplyMigrations(ctx, edited); err != nil {
		t.Fatalf("strict integrity could not settle an edit on %d tables: %v", tables, err)
	}
	if recordedDigest(t, db, app) != ContentDigest(edited[0].Content) {
		t.Fatal("the verified edit was not re-stamped")
	}
}

// When the server does run out of locks, PostgreSQL says only "out of shared
// memory". The refusal has to say what was being done and which setting
// bounds it.
func TestLockTableExhaustionIsExplained(t *testing.T) {
	db := convDB(t)
	ctx := context.Background()
	const app, schema, tables = "mk-locks-full", "mk_locks_full", 80
	baseline := lockHeavyBaseline(tables)
	buildChain(t, db, app, schema, []Migration{{Name: "0001_schema.up.sql", Content: baseline}})
	edited := []Migration{{Name: "0001_schema.up.sql", Content: renamedIndex(baseline)}}
	strict := func() error {
		return NewPostgres(db, app).WithSchema(schema).WithStrictIntegrity().ApplyMigrations(ctx, edited)
	}
	explained := func(doing string, err error) {
		t.Helper()
		t.Logf("%s: %v", doing, err)
		if err == nil {
			t.Fatalf("%s succeeded without the locks it needs", doing)
		}
		if !strings.Contains(err.Error(), "max_locks_per_transaction") {
			t.Fatalf("%s: a full lock table must name the setting that bounds it:\n%v", doing, err)
		}
	}

	// Both sessions connect first: with no lock left the server cannot start
	// a backend either.
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	// Another session takes every lock entry the server has.
	hog, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	released := false
	release := func() {
		if released {
			return
		}
		released = true
		if _, err := hog.ExecContext(ctx, `SELECT pg_advisory_unlock_all()`); err != nil {
			t.Errorf("release the lock table: %v", err)
		}
		_ = discardMigrationConn(hog)
		_ = hog.Close()
	}
	t.Cleanup(release)
	for _, from := range []int{1, 10_000_001} {
		_, err := hog.ExecContext(ctx, fmt.Sprintf(`SELECT count(pg_advisory_lock(g)) FROM generate_series(%d, %d) g`, from, from+9_999_999))
		if err == nil || !strings.Contains(err.Error(), "out of shared memory") {
			t.Fatalf("the lock table did not fill: %v", err)
		}
	}
	var taken int
	if err := hog.QueryRowContext(ctx, `SELECT count(*) FROM pg_catalog.pg_locks WHERE pid = pg_catalog.pg_backend_pid() AND locktype = 'advisory'`).Scan(&taken); err != nil {
		t.Fatal(err)
	}
	t.Logf("the lock table was full after another session took %d locks", taken)

	// With none left, a fingerprint cannot start.
	_, err = schemaFingerprint(ctx, tx, schema)
	_ = tx.Rollback()
	explained("reading a fingerprint with no lock left", err)
	if !strings.Contains(err.Error(), schema) {
		t.Fatalf("the refusal must name the schema being read:\n%v", err)
	}

	// With 200 left there is room to read the schema, not to rebuild its 80
	// tables at eight lock table entries each.
	if _, err := hog.ExecContext(ctx, `SELECT count(pg_advisory_unlock(g)) FROM generate_series(1, 200) g`); err != nil {
		t.Fatal(err)
	}
	explained("settling an edit with 200 locks left", strict())
	if recordedDigest(t, db, app) != ContentDigest(baseline) {
		t.Fatal("a refused apply changed the ledger")
	}

	release()
	if err := strict(); err != nil {
		t.Fatalf("with the lock table free again: %v", err)
	}
	if recordedDigest(t, db, app) != ContentDigest(edited[0].Content) {
		t.Fatal("the verified edit was not re-stamped")
	}
}
