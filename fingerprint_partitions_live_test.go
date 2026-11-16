package migratekit

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
)

// An application that partitions by time makes and drops partitions at run
// time, so a database never has the partitions a fresh build makes today.

// partitionedParent is a partitioned table with everything its partitions
// inherit, and a table whose foreign key references it.
const partitionedParent = `
CREATE TABLE tenants (id bigint PRIMARY KEY);
CREATE TABLE events (
	id bigint NOT NULL,
	tenant_id bigint NOT NULL REFERENCES tenants (id),
	at timestamptz NOT NULL,
	kind text NOT NULL,
	CONSTRAINT events_pkey PRIMARY KEY (id, at),
	CONSTRAINT events_kind_check CHECK (kind <> '')
) PARTITION BY RANGE (at);
CREATE INDEX events_tenant_idx ON events (tenant_id, at);
CREATE TABLE event_notes (
	event_id bigint NOT NULL,
	event_at timestamptz NOT NULL,
	CONSTRAINT event_notes_event_fkey FOREIGN KEY (event_id, event_at) REFERENCES events (id, at)
);
CREATE FUNCTION events_touch() RETURNS trigger LANGUAGE plpgsql SET search_path FROM CURRENT AS $$ BEGIN RETURN NEW; END $$;
CREATE TRIGGER events_touch AFTER INSERT ON events FOR EACH ROW EXECUTE FUNCTION events_touch();
ALTER TABLE events ENABLE ROW LEVEL SECURITY;
CREATE POLICY events_tenant ON events USING (tenant_id > 0);
`

// partitionsToday is what a baseline that partitions around now() makes on the
// day it runs.
const partitionsToday = `
CREATE TABLE events_y2026m10 PARTITION OF events FOR VALUES FROM ('2026-10-01 00:00:00+00') TO ('2026-11-01 00:00:00+00');
CREATE TABLE events_y2026m11 PARTITION OF events FOR VALUES FROM ('2026-11-01 00:00:00+00') TO ('2026-12-01 00:00:00+00');
`

var partitionedChain = []Migration{{Name: "0001_schema.up.sql", Content: partitionedParent + partitionsToday}}

func buildChain(t *testing.T, db *sql.DB, app, schema string, chain []Migration) {
	t.Helper()
	liveScope(t, db, app, schema)
	if err := NewPostgres(db, app).WithSchema(schema).ApplyMigrations(context.Background(), chain); err != nil {
		t.Fatalf("build %s: %v", schema, err)
	}
}

func execEach(t *testing.T, db *sql.DB, statements ...string) {
	t.Helper()
	for _, s := range statements {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
}

func mustSchemaDiff(t *testing.T, db *sql.DB, schema, reference string) []string {
	t.Helper()
	d, err := SchemaDiff(context.Background(), db, schema, reference)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestFingerprintIgnoresPartitionChildren(t *testing.T) {
	db := convDB(t)
	buildChain(t, db, "mk-part-old", "mk_part_old", partitionedChain)
	buildChain(t, db, "mk-part-new", "mk_part_new", partitionedChain)
	diff := func() []string { return mustSchemaDiff(t, db, "mk_part_old", "mk_part_new") }
	if d := diff(); len(d) > 0 {
		t.Fatalf("two builds of one chain differ:\n%s", strings.Join(d, "\n"))
	}

	// Retention detaches the recent partition before dropping it. While it is
	// only detached it is an ordinary table no migration made.
	execEach(t, db, `ALTER TABLE mk_part_old.events DETACH PARTITION mk_part_old.events_y2026m11`)
	detached := false
	for _, l := range diff() {
		detached = detached || strings.HasPrefix(l, "- relation events_y2026m11 | r |")
	}
	if !detached {
		t.Fatalf("a detached partition is a table and must be counted:\n%s", strings.Join(diff(), "\n"))
	}

	execEach(t, db,
		`DROP TABLE mk_part_old.events_y2026m11`,
		// An older partition, with objects of its own.
		`CREATE TABLE mk_part_old.events_y2026m01 PARTITION OF mk_part_old.events FOR VALUES FROM ('2026-01-01 00:00:00+00') TO ('2026-02-01 00:00:00+00')`,
		`CREATE INDEX events_y2026m01_kind_idx ON mk_part_old.events_y2026m01 (kind)`,
		`ALTER TABLE mk_part_old.events_y2026m01 ADD CONSTRAINT events_y2026m01_id_check CHECK (id > 0)`,
		`CREATE TRIGGER events_y2026m01_only AFTER UPDATE ON mk_part_old.events_y2026m01 FOR EACH ROW EXECUTE FUNCTION mk_part_old.events_touch()`,
		`CREATE POLICY events_y2026m01_only ON mk_part_old.events_y2026m01 USING (true)`,
		`CREATE RULE events_y2026m01_only AS ON DELETE TO mk_part_old.events_y2026m01 DO INSTEAD NOTHING`,
		`CREATE SEQUENCE mk_part_old.events_y2026m01_seq OWNED BY mk_part_old.events_y2026m01.id`,
		// A sub-partitioned child and a default partition are children too.
		`CREATE TABLE mk_part_old.events_y2025 PARTITION OF mk_part_old.events FOR VALUES FROM ('2025-01-01 00:00:00+00') TO ('2026-01-01 00:00:00+00') PARTITION BY RANGE (at)`,
		`CREATE TABLE mk_part_old.events_y2025h1 PARTITION OF mk_part_old.events_y2025 FOR VALUES FROM ('2025-01-01 00:00:00+00') TO ('2025-07-01 00:00:00+00')`,
		`CREATE TABLE mk_part_old.events_default PARTITION OF mk_part_old.events DEFAULT`,
	)
	if d := diff(); len(d) > 0 {
		t.Fatalf("partition children counted:\n%s", strings.Join(d, "\n"))
	}

	// The parent is still compared: each change shows as itself and nothing
	// else, although every partition inherits it.
	execEach(t, db, `ALTER TABLE mk_part_old.events ADD COLUMN extra text`)
	if d := diff(); len(d) != 1 || !strings.HasPrefix(d[0], "- column events | extra | text |") {
		t.Fatalf("added parent column: want one line for events.extra, got:\n%s", strings.Join(d, "\n"))
	}
	execEach(t, db,
		`ALTER TABLE mk_part_old.events DROP COLUMN extra`,
		`CREATE INDEX events_kind_idx ON mk_part_old.events (kind)`,
	)
	if d := diff(); len(d) != 1 || !strings.HasPrefix(d[0], "- index events | ") || !strings.Contains(d[0], "(kind)") {
		t.Fatalf("extra parent index: want one line for events (kind), got:\n%s", strings.Join(d, "\n"))
	}
	execEach(t, db, `DROP INDEX mk_part_old.events_kind_idx`)

	keyed := []Migration{{Name: "0001_schema.up.sql", Content: strings.Replace(partitionedParent, "PARTITION BY RANGE (at)", "PARTITION BY RANGE (id)", 1)}}
	buildChain(t, db, "mk-part-key", "mk_part_key", keyed)
	d := mustSchemaDiff(t, db, "mk_part_old", "mk_part_key")
	if all := strings.Join(d, "\n"); len(d) != 2 ||
		!strings.Contains(all, "- relation events | p |") || !strings.Contains(all, "RANGE (at)") ||
		!strings.Contains(all, "+ relation events | p |") || !strings.Contains(all, "RANGE (id)") {
		t.Fatalf("changed partition key: want the two relation lines for events, got:\n%s", all)
	}
}

func TestStrictIntegrityIgnoresPartitionChildren(t *testing.T) {
	db := convDB(t)
	ctx := context.Background()
	const app, schema = "mk-part-strict", "mk_part_strict"
	buildChain(t, db, app, schema, partitionedChain)
	// A month on: retention dropped a partition and maintenance made the next,
	// while a fresh build of the same baseline still makes the original two.
	// Someone also altered the parent by hand.
	execEach(t, db,
		`ALTER TABLE mk_part_strict.events DETACH PARTITION mk_part_strict.events_y2026m10`,
		`DROP TABLE mk_part_strict.events_y2026m10`,
		`CREATE TABLE mk_part_strict.events_y2026m12 PARTITION OF mk_part_strict.events FOR VALUES FROM ('2026-12-01 00:00:00+00') TO ('2027-01-01 00:00:00+00')`,
		`ALTER TABLE mk_part_strict.events ADD COLUMN hand_added int`,
	)
	// The edit renames a plain index: new content, same behaviour.
	edited := []Migration{
		{Name: "0001_schema.up.sql", Content: strings.Replace(partitionedChain[0].Content, "events_tenant_idx", "events_by_tenant", 1)},
		{Name: "0002_more.up.sql", Content: `CREATE TABLE more (id int);`},
	}
	strict := func() error {
		return NewPostgres(db, app).WithSchema(schema).WithStrictIntegrity().ApplyMigrations(ctx, edited)
	}
	recorded := func() string {
		t.Helper()
		var digest string
		if err := db.QueryRow(`SELECT content_sha256 FROM public.migrations WHERE app = $1 AND sequence = 1`, app).Scan(&digest); err != nil {
			t.Fatal(err)
		}
		return digest
	}
	later := func() bool {
		t.Helper()
		var applied bool
		if err := db.QueryRow(`SELECT to_regclass('mk_part_strict.more') IS NOT NULL`).Scan(&applied); err != nil {
			t.Fatal(err)
		}
		return applied
	}

	err := strict()
	t.Log(err)
	if !errors.Is(err, ErrSchemaMismatch) || !strings.Contains(err.Error(), "hand_added") {
		t.Fatalf("a changed parent must refuse and name the change, got: %v", err)
	}
	if strings.Contains(err.Error(), "events_y2026m") {
		t.Fatalf("refusal lists partition children:\n%v", err)
	}
	if recorded() != ContentDigest(partitionedChain[0].Content) || later() {
		t.Fatal("a refused apply changed the ledger or applied a later migration")
	}

	execEach(t, db, `ALTER TABLE mk_part_strict.events DROP COLUMN hand_added`)
	if err := strict(); err != nil {
		t.Fatalf("an edit that differs from the database only by partition children was refused: %v", err)
	}
	if recorded() != ContentDigest(edited[0].Content) || !later() {
		t.Fatal("the verified edit was not re-stamped, or the later migration did not apply")
	}
	var verified int
	if err := db.QueryRow(`SELECT count(*) FROM public.migration_repairs WHERE app = $1 AND verb = 'verify-content'`, app).Scan(&verified); err != nil || verified != 1 {
		t.Fatalf("verify-content audit rows = %d, %v", verified, err)
	}
	if err := strict(); err != nil {
		t.Fatalf("second boot: %v", err)
	}
}
