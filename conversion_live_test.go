package migratekit

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"strings"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
)

func convDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("MIGRATEKIT_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("MIGRATEKIT_TEST_DATABASE_URL not set; skipping live DB test")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// liveScope gives each test its own app key and schema.
func liveScope(t *testing.T, db *sql.DB, app, schema string) {
	t.Helper()
	clean := func() {
		ctx := context.Background()
		_, _ = db.ExecContext(ctx, `DROP SCHEMA IF EXISTS "`+schema+`" CASCADE`)
		_, _ = db.ExecContext(ctx, `DELETE FROM public.migrations WHERE app = $1`, app)
		_, _ = db.ExecContext(ctx, `DELETE FROM public.migration_repairs WHERE app = $1`, app)
	}
	clean()
	t.Cleanup(clean)
}

func same(content string) Render {
	return func(string) ([]Migration, error) {
		return []Migration{{Name: "0001_schema.up.sql", Content: content}}, nil
	}
}

const retiredBaseline = `
CREATE TABLE accounts (id bigint PRIMARY KEY, name text NOT NULL, legacy_flag boolean NOT NULL DEFAULT false);
CREATE TABLE erasures (account_id bigint PRIMARY KEY REFERENCES accounts(id));
CREATE INDEX accounts_name_idx ON accounts (name);
`

const currentBaseline = `
CREATE TABLE accounts (id bigint PRIMARY KEY, name text NOT NULL, created_at timestamptz NOT NULL DEFAULT now());
CREATE TABLE deletions (id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY, account_id bigint NOT NULL REFERENCES accounts(id), CONSTRAINT deletions_one UNIQUE (account_id));
CREATE INDEX accounts_name_idx ON accounts (name);
CREATE FUNCTION touch() RETURNS trigger LANGUAGE plpgsql SET search_path FROM CURRENT AS $$ BEGIN RETURN NEW; END $$;
CREATE TRIGGER accounts_touch BEFORE UPDATE ON accounts FOR EACH ROW EXECUTE FUNCTION touch();
`

const conversionSQL = `
DO $$ BEGIN
  IF EXISTS (SELECT 1 FROM erasures) THEN
    RAISE EXCEPTION 'in-flight erasures need the retired runtime';
  END IF;
END $$;
DROP TABLE erasures;
ALTER TABLE accounts DROP COLUMN legacy_flag;
ALTER TABLE accounts ADD COLUMN created_at timestamptz NOT NULL DEFAULT now();
CREATE TABLE deletions (id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY, account_id bigint NOT NULL REFERENCES accounts(id), CONSTRAINT deletions_one UNIQUE (account_id));
CREATE FUNCTION touch() RETURNS trigger LANGUAGE plpgsql SET search_path FROM CURRENT AS $$ BEGIN RETURN NEW; END $$;
CREATE TRIGGER accounts_touch BEFORE UPDATE ON accounts FOR EACH ROW EXECUTE FUNCTION touch();
`

func testConversion(sqlBody string) Conversion {
	return Conversion{
		Name:     "retired-v1",
		Retired:  same(retiredBaseline),
		Replaces: 1,
		SQL:      func(string) (string, error) { return sqlBody, nil },
		Fallback: "the v1 release",
	}
}

var currentChain = []Migration{
	{Name: "0001_schema.up.sql", Content: currentBaseline},
	{Name: "0002_accounts_email.up.sql", Content: `ALTER TABLE accounts ADD COLUMN email text;`},
}

func applyRetired(t *testing.T, db *sql.DB, app, schema string) {
	t.Helper()
	if err := NewPostgres(db, app).WithSchema(schema).ApplyMigrations(context.Background(),
		[]Migration{{Name: "0001_schema.up.sql", Content: retiredBaseline}}); err != nil {
		t.Fatalf("apply retired chain: %v", err)
	}
}

func convLedger(t *testing.T, db *sql.DB, app string) map[string]string {
	t.Helper()
	rows, err := db.Query(`SELECT sequence::text, filename || ' ' || left(content_sha256, 12) FROM public.migrations WHERE app = $1`, app)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			t.Fatal(err)
		}
		out[k] = v
	}
	return out
}

func liveFingerprint(t *testing.T, db *sql.DB, schema string) fingerprint {
	t.Helper()
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	fp, err := schemaFingerprint(context.Background(), tx, schema)
	if err != nil {
		t.Fatal(err)
	}
	return fp
}

func TestConversionUpgradesRetiredChainThenAppliesTheRest(t *testing.T) {
	db := convDB(t)
	ctx := context.Background()
	liveScope(t, db, "mk-conv-upgrade", "mk_conv_upgrade")
	liveScope(t, db, "mk-conv-fresh", "mk_conv_fresh")
	applyRetired(t, db, "mk-conv-upgrade", "mk_conv_upgrade")
	if _, err := db.Exec(`INSERT INTO mk_conv_upgrade.accounts (id, name) VALUES (7, 'kept')`); err != nil {
		t.Fatal(err)
	}

	err := NewPostgres(db, "mk-conv-upgrade").WithSchema("mk_conv_upgrade").
		WithConversions(testConversion(conversionSQL)).WithStrictIntegrity().
		ApplyMigrations(ctx, currentChain)
	if err != nil {
		t.Fatalf("convert and apply: %v", err)
	}
	if err := NewPostgres(db, "mk-conv-fresh").WithSchema("mk_conv_fresh").ApplyMigrations(ctx, currentChain); err != nil {
		t.Fatal(err)
	}
	if d, err := SchemaDiff(ctx, db, "mk_conv_upgrade", "mk_conv_fresh"); err != nil || len(d) > 0 {
		t.Fatalf("converted schema differs from fresh (%v):\n%s", err, strings.Join(d, "\n"))
	}
	var name string
	if err := db.QueryRow(`SELECT name FROM mk_conv_upgrade.accounts WHERE id = 7`).Scan(&name); err != nil || name != "kept" {
		t.Fatalf("row after conversion: %q, %v", name, err)
	}
	got := convLedger(t, db, "mk-conv-upgrade")
	want := convLedger(t, db, "mk-conv-fresh")
	if len(got) != 2 || got["1"] != want["1"] || got["2"] != want["2"] {
		t.Fatalf("ledger = %v, want %v", got, want)
	}
	var audits int
	if err := db.QueryRow(`SELECT count(*) FROM public.migration_repairs WHERE app = $1 AND verb = 'convert' AND reason LIKE 'verified conversion retired-v1%'`, "mk-conv-upgrade").Scan(&audits); err != nil || audits != 1 {
		t.Fatalf("convert audit rows = %d, %v", audits, err)
	}
	// Idempotent: a second boot has nothing to convert.
	if err := NewPostgres(db, "mk-conv-upgrade").WithSchema("mk_conv_upgrade").
		WithConversions(testConversion(conversionSQL)).WithStrictIntegrity().ApplyMigrations(ctx, currentChain); err != nil {
		t.Fatalf("second boot: %v", err)
	}
}

func TestConversionDetectsRetiredShapeWithoutLedger(t *testing.T) {
	db := convDB(t)
	liveScope(t, db, "mk-conv-ledgerless", "mk_conv_ledgerless")
	if _, err := db.Exec(`CREATE SCHEMA mk_conv_ledgerless; SET search_path = mk_conv_ledgerless, public;` + retiredBaseline + `RESET search_path;`); err != nil {
		t.Fatal(err)
	}
	err := NewPostgres(db, "mk-conv-ledgerless").WithSchema("mk_conv_ledgerless").
		WithConversions(testConversion(conversionSQL)).ApplyMigrations(context.Background(), currentChain)
	if err != nil {
		t.Fatalf("ledger-less conversion: %v", err)
	}
	if got := convLedger(t, db, "mk-conv-ledgerless"); len(got) != 2 {
		t.Fatalf("ledger = %v", got)
	}
}

func TestConversionRefusesSchemaChangedOutsideItsChain(t *testing.T) {
	db := convDB(t)
	liveScope(t, db, "mk-conv-drifted", "mk_conv_drifted")
	applyRetired(t, db, "mk-conv-drifted", "mk_conv_drifted")
	if _, err := db.Exec(`ALTER TABLE mk_conv_drifted.accounts ADD COLUMN hand_added int`); err != nil {
		t.Fatal(err)
	}
	before := convLedger(t, db, "mk-conv-drifted")
	err := NewPostgres(db, "mk-conv-drifted").WithSchema("mk_conv_drifted").
		WithConversions(testConversion(conversionSQL)).ApplyMigrations(context.Background(), currentChain)
	t.Log(err)
	if !errors.Is(err, ErrSchemaMismatch) {
		t.Fatalf("err = %v, want ErrSchemaMismatch", err)
	}
	for _, want := range []string{"retired-v1", "hand_added", "RESOLUTION", "the v1 release"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("refusal lacks %q:\n%v", want, err)
		}
	}
	if after := convLedger(t, db, "mk-conv-drifted"); after["1"] != before["1"] || len(after) != 1 {
		t.Fatalf("ledger changed: %v -> %v", before, after)
	}
}

func TestConversionRefusalFromItsSQLChangesNothing(t *testing.T) {
	db := convDB(t)
	liveScope(t, db, "mk-conv-inflight", "mk_conv_inflight")
	applyRetired(t, db, "mk-conv-inflight", "mk_conv_inflight")
	if _, err := db.Exec(`INSERT INTO mk_conv_inflight.accounts (id, name) VALUES (1, 'a'); INSERT INTO mk_conv_inflight.erasures VALUES (1)`); err != nil {
		t.Fatal(err)
	}
	before := liveFingerprint(t, db, "mk_conv_inflight")
	err := NewPostgres(db, "mk-conv-inflight").WithSchema("mk_conv_inflight").
		WithConversions(testConversion(conversionSQL)).ApplyMigrations(context.Background(), currentChain)
	t.Log(err)
	if err == nil || !strings.Contains(err.Error(), "in-flight erasures") || !strings.Contains(err.Error(), "nothing was changed") {
		t.Fatalf("err = %v", err)
	}
	if d := liveFingerprint(t, db, "mk_conv_inflight").diff(before); len(d) > 0 {
		t.Fatalf("schema changed after refusal: %v", d)
	}
}

func TestIncompleteConversionRollsBack(t *testing.T) {
	db := convDB(t)
	liveScope(t, db, "mk-conv-incomplete", "mk_conv_incomplete")
	applyRetired(t, db, "mk-conv-incomplete", "mk_conv_incomplete")
	incomplete := strings.Replace(conversionSQL, "CREATE TRIGGER accounts_touch BEFORE UPDATE ON accounts FOR EACH ROW EXECUTE FUNCTION touch();", "", 1)
	before := liveFingerprint(t, db, "mk_conv_incomplete")
	err := NewPostgres(db, "mk-conv-incomplete").WithSchema("mk_conv_incomplete").
		WithConversions(testConversion(incomplete)).ApplyMigrations(context.Background(), currentChain)
	t.Log(err)
	if !errors.Is(err, ErrSchemaMismatch) || !strings.Contains(err.Error(), "accounts_touch") {
		t.Fatalf("err = %v", err)
	}
	if d := liveFingerprint(t, db, "mk_conv_incomplete").diff(before); len(d) > 0 {
		t.Fatalf("schema changed after rollback: %v", d)
	}
	if got := convLedger(t, db, "mk-conv-incomplete"); len(got) != 1 {
		t.Fatalf("ledger changed: %v", got)
	}
}

func TestStrictIntegritySettlesEditsOnTheSchema(t *testing.T) {
	db := convDB(t)
	ctx := context.Background()
	liveScope(t, db, "mk-strict", "mk_strict")
	if err := NewPostgres(db, "mk-strict").WithSchema("mk_strict").ApplyMigrations(ctx, currentChain[:1]); err != nil {
		t.Fatal(err)
	}
	// Cosmetic: a comment and a renamed plain index change no behaviour.
	cosmetic := []Migration{
		{Name: "0001_schema.up.sql", Content: "-- reworded\n" + strings.Replace(currentBaseline, "accounts_name_idx", "accounts_by_name", 1)},
		currentChain[1],
	}
	if err := NewPostgres(db, "mk-strict").WithSchema("mk_strict").WithStrictIntegrity().ApplyMigrations(ctx, cosmetic); err != nil {
		t.Fatalf("cosmetic edit refused: %v", err)
	}
	var verified int
	if err := db.QueryRow(`SELECT count(*) FROM public.migration_repairs WHERE app = 'mk-strict' AND verb = 'verify-content'`).Scan(&verified); err != nil || verified != 1 {
		t.Fatalf("verify-content audit rows = %d, %v", verified, err)
	}

	// Semantic: a changed column type changes what the database accepts.
	semantic := []Migration{
		{Name: "0001_schema.up.sql", Content: strings.Replace(cosmetic[0].Content, "name text NOT NULL", "name varchar(8) NOT NULL", 1)},
		currentChain[1],
		{Name: "0003_more.up.sql", Content: `CREATE TABLE more (id int);`},
	}
	err := NewPostgres(db, "mk-strict").WithSchema("mk_strict").WithStrictIntegrity().ApplyMigrations(ctx, semantic)
	t.Log(err)
	if !errors.Is(err, ErrSchemaMismatch) {
		t.Fatalf("err = %v, want ErrSchemaMismatch", err)
	}
	for _, want := range []string{"0001_schema.up.sql", "character varying(8)", "RESOLUTION", "Conversion"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("refusal lacks %q:\n%v", want, err)
		}
	}
	var more bool
	if err := db.QueryRow(`SELECT to_regclass('mk_strict.more') IS NOT NULL`).Scan(&more); err != nil || more {
		t.Fatalf("later migration applied after refusal: %v %v", more, err)
	}
	// Without strict integrity the old behaviour stands: a warning, then apply.
	if err := NewPostgres(db, "mk-strict").WithSchema("mk_strict").WithWarnFunc(func(Discrepancy) {}).ApplyMigrations(ctx, semantic); err != nil {
		t.Fatalf("default mode: %v", err)
	}
}

func TestFingerprintIsSemantic(t *testing.T) {
	db := convDB(t)
	liveScope(t, db, "mk-fp-a", "mk_fp_a")
	liveScope(t, db, "mk-fp-b", "mk_fp_b")
	build := func(schema, body string) fingerprint {
		t.Helper()
		if _, err := db.Exec(`DROP SCHEMA IF EXISTS ` + schema + ` CASCADE; CREATE SCHEMA ` + schema + `; SET search_path = ` + schema + `, public;` + body + `RESET search_path;`); err != nil {
			t.Fatal(err)
		}
		return liveFingerprint(t, db, schema)
	}
	base := `CREATE TABLE t (a int NOT NULL, b text, CONSTRAINT t_a_key UNIQUE (a));
	         CREATE INDEX t_b_idx ON t (b);
	         CREATE FUNCTION f() RETURNS int LANGUAGE sql SET search_path FROM CURRENT AS 'SELECT 1';`
	a := build("mk_fp_a", base)
	cosmetic := build("mk_fp_b", `CREATE TABLE t (b text, a int NOT NULL, CONSTRAINT t_a_key UNIQUE (a));
	         CREATE INDEX other_name ON t (b); COMMENT ON TABLE t IS 'x';
	         CREATE FUNCTION f() RETURNS int LANGUAGE sql SET search_path FROM CURRENT AS 'SELECT 1';
	         REVOKE ALL ON t FROM PUBLIC;`)
	if d := a.diff(cosmetic); len(d) > 0 {
		t.Fatalf("cosmetic differences counted: %v", d)
	}
	// A pg_dump round trip reprints a varchar IN-list; the constraint is the same.
	original := build("mk_fp_a", `CREATE TABLE m (method varchar(8) NOT NULL CHECK (method IN ('email','sms')));`)
	restored := build("mk_fp_b", `CREATE TABLE m (method varchar(8) NOT NULL,
	         CONSTRAINT m_method_check CHECK (((method)::text = ANY ((ARRAY['email'::character varying, 'sms'::character varying])::text[]))));`)
	if d := original.diff(restored); len(d) > 0 {
		t.Fatalf("dump round trip counted as a difference: %v", d)
	}
	// A string literal that happens to start with the schema's name is data.
	literalA := build("mk_fp_a", `CREATE FUNCTION g() RETURNS text LANGUAGE sql AS $$ SELECT current_setting('mk_fp_a.decision', true) $$;`)
	literalB := build("mk_fp_b", `CREATE FUNCTION g() RETURNS text LANGUAGE sql AS $$ SELECT current_setting('mk_fp_a.decision', true) $$;`)
	if d := literalA.diff(literalB); len(d) > 0 {
		t.Fatalf("string literal treated as a qualifier: %v", d)
	}
	a = build("mk_fp_a", base)
	for name, body := range map[string]string{
		"check values":    strings.Replace(base, "b text", "b text CHECK (b IN ('x'))", 1),
		"nullability":     strings.Replace(base, "b text", "b text NOT NULL", 1),
		"constraint name": strings.Replace(base, "t_a_key", "t_a_unique", 1),
		"index columns":   strings.Replace(base, "t (b)", "t (b, a)", 1),
		"function body":   strings.Replace(base, "'SELECT 1'", "'SELECT 2'", 1),
	} {
		if d := a.diff(build("mk_fp_b", body)); len(d) == 0 {
			t.Fatalf("%s change not detected", name)
		}
	}
}
