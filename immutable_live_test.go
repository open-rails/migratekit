package migratekit

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
)

// statementLog records every statement sent on the connections it traces.
type statementLog struct {
	mu  sync.Mutex
	sql []string
}

func (l *statementLog) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	l.mu.Lock()
	l.sql = append(l.sql, data.SQL)
	l.mu.Unlock()
	return ctx
}

func (l *statementLog) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func (l *statementLog) take() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := l.sql
	l.sql = nil
	return out
}

// The embedded host's path: a migrator from a pgx pool, with strict integrity
// and a renderer also declared, so precedence is exercised rather than assumed.
func TestImmutableMigrationsRefuseAnEditWithoutARebuild(t *testing.T) {
	admin := freshNumericDatabase(t)
	log := &statementLog{}
	_, pool := singleConnectionPool(t, admin, "mk_immutable", log)
	ctx := singleConnectionContext(t)
	migrator := func() *Postgres {
		t.Helper()
		m, err := NewPostgresFromPGXPool(pool, "immutable")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { m.Close() })
		return m.WithSchema("app_data").WithStrictIntegrity().WithImmutableMigrations().
			WithRender(func(string) ([]Migration, error) {
				t.Error("immutable migrations rendered the chain for a rebuild")
				return nil, errors.New("not rendered under immutability")
			})
	}
	ledger := func() map[string]string {
		t.Helper()
		rows, err := admin.QueryContext(ctx, `SELECT sequence::text, filename || ' ' || content_sha256 || ' ' || COALESCE(semantic_sha256, '')
			FROM public.migrations WHERE app = 'immutable'`)
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
	exists := func(table string) bool {
		t.Helper()
		var ok bool
		if err := admin.QueryRowContext(ctx, `SELECT to_regclass($1) IS NOT NULL`, "app_data."+table).Scan(&ok); err != nil {
			t.Fatal(err)
		}
		return ok
	}

	released := []Migration{
		{Name: "0001_accounts.up.sql", Content: "CREATE TABLE accounts (id bigint PRIMARY KEY, name text NOT NULL);"},
		{Name: "0002_accounts_email.up.sql", Content: "ALTER TABLE accounts ADD COLUMN email text;"},
	}
	if err := migrator().ApplyMigrations(ctx, released); err != nil {
		t.Fatalf("fresh apply: %v", err)
	}
	log.take()
	if err := migrator().ApplyMigrations(ctx, released); err != nil {
		t.Fatalf("unchanged chain refused: %v", err)
	}
	for _, s := range log.take() {
		if strings.HasPrefix(s, "SELECT pg_advisory_lock(") {
			t.Fatal("an unchanged chain with nothing pending took the migration lock")
		}
	}

	withNotes := append(append([]Migration(nil), released...),
		Migration{Name: "0003_notes.up.sql", Content: "CREATE TABLE notes (id int);"})
	if err := migrator().ApplyMigrations(ctx, withNotes); err != nil || !exists("notes") {
		t.Fatalf("pending migration not applied: %v", err)
	}

	newer := append(append([]Migration(nil), withNotes...),
		Migration{Name: "0004_newer.up.sql", Content: "CREATE TABLE newer (id int);"})
	if err := migrator().ApplyMigrations(ctx, newer); err != nil {
		t.Fatalf("newer build: %v", err)
	}
	if err := migrator().ApplyMigrations(ctx, withNotes); err != nil {
		t.Fatalf("an older build refused a newer build's migration: %v", err)
	}
	if _, ok := ledger()["4"]; !ok || !exists("newer") {
		t.Fatal("the newer build's migration was not left alone")
	}

	// A comment-only edit (bytes only) and a DDL edit, a pending migration
	// behind them, and the newer build's row still in the ledger.
	edited := []Migration{
		{Name: released[0].Name, Content: "-- reworded\n" + released[0].Content},
		{Name: released[1].Name, Content: "ALTER TABLE accounts ADD COLUMN email varchar(64);"},
		withNotes[2],
		{Name: "0005_later.up.sql", Content: "CREATE TABLE later (id int);"},
	}
	before := ledger()
	log.take()
	err := migrator().ApplyMigrations(ctx, edited)
	statements := log.take()
	t.Log(err)
	if !errors.Is(err, ErrMigrationEdited) || errors.Is(err, ErrSchemaMismatch) {
		t.Fatalf("err = %v, want ErrMigrationEdited and not ErrSchemaMismatch", err)
	}
	for i := range 2 {
		for _, want := range []string{released[i].Name, shortDigest(ContentDigest(released[i].Content)), shortDigest(ContentDigest(edited[i].Content))} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("refusal lacks %q:\n%v", want, err)
			}
		}
	}
	for _, want := range []string{"never change", "new migration", "rebuild this database"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal lacks %q:\n%v", want, err)
		}
	}
	if strings.Contains(err.Error(), "Conversion") {
		t.Errorf("refusal suggests a Conversion:\n%v", err)
	}

	readLedger := false
	for _, s := range statements {
		readLedger = readLedger || strings.Contains(s, "FROM public.migrations")
		for _, forbidden := range []string{"migratekit_scratch", "SAVEPOINT", "SELECT pg_advisory_lock(",
			"UPDATE public.migrations", "INSERT INTO public.migrations", "INSERT INTO public.migration_repairs"} {
			if strings.Contains(s, forbidden) {
				t.Errorf("refusal ran %q:\n%s", forbidden, s)
			}
		}
	}
	if !readLedger {
		t.Fatalf("statement trace saw no ledger read: %q", statements)
	}
	if after := ledger(); !reflect.DeepEqual(after, before) {
		t.Fatalf("ledger changed:\nbefore %v\nafter  %v", before, after)
	}
	if exists("later") {
		t.Fatal("a pending migration was applied behind an edit")
	}
	var repairs int
	if err := admin.QueryRowContext(ctx, `SELECT count(*) FROM public.migration_repairs`).Scan(&repairs); err != nil || repairs != 0 {
		t.Fatalf("repair audit rows = %d, %v", repairs, err)
	}

	st, err := migrator().Status(ctx, edited)
	if err != nil {
		t.Fatal(err)
	}
	var reported []string
	for _, d := range st.Discrepancies {
		if d.Kind == KindContentDrift && d.Severity == SeverityError {
			reported = append(reported, d.File)
		}
	}
	if !st.HasErrors() || !reflect.DeepEqual(reported, []string{released[0].Name, released[1].Name}) {
		t.Fatalf("status reports edits %v, want both as errors: %+v", reported, st.Discrepancies)
	}
}

// Conversions run before the immutability check: a retired chain's ledger
// records other content by design.
func TestImmutableMigrationsStillConvert(t *testing.T) {
	db := convDB(t)
	ctx := context.Background()
	liveScope(t, db, "mk-immutable-conv", "mk_immutable_conv")
	liveScope(t, db, "mk-immutable-fresh", "mk_immutable_fresh")
	applyRetired(t, db, "mk-immutable-conv", "mk_immutable_conv")

	migrator := func() *Postgres {
		return NewPostgres(db, "mk-immutable-conv").WithSchema("mk_immutable_conv").
			WithImmutableMigrations().WithConversions(testConversion(conversionSQL))
	}
	if err := migrator().ApplyMigrations(ctx, currentChain); err != nil {
		t.Fatalf("convert and apply: %v", err)
	}
	if err := NewPostgres(db, "mk-immutable-fresh").WithSchema("mk_immutable_fresh").ApplyMigrations(ctx, currentChain); err != nil {
		t.Fatal(err)
	}
	if got, want := convLedger(t, db, "mk-immutable-conv"), convLedger(t, db, "mk-immutable-fresh"); !reflect.DeepEqual(got, want) {
		t.Fatalf("ledger = %v, want %v", got, want)
	}
	if err := migrator().ApplyMigrations(ctx, currentChain); err != nil {
		t.Fatalf("second boot: %v", err)
	}
	edited := []Migration{{Name: currentChain[0].Name, Content: currentChain[0].Content + "\n-- edited"}, currentChain[1]}
	if err := migrator().ApplyMigrations(ctx, edited); !errors.Is(err, ErrMigrationEdited) {
		t.Fatalf("edit after conversion: err = %v, want ErrMigrationEdited", err)
	}
}
