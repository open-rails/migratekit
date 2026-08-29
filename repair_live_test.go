package migratekit

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// v1.5.0 made three failures visible; v1.6.0 makes them resolvable. Each test
// here walks the whole operator loop: break the ledger the way a real incident
// breaks it, watch the boot refuse (or warn), run ONE repair verb, and boot
// clean — with the repair itself left behind as an audit row.

func repairTestDB(t *testing.T, app string, tables ...string) (*sql.DB, context.Context) {
	t.Helper()
	dsn := os.Getenv("MIGRATEKIT_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("MIGRATEKIT_TEST_DATABASE_URL not set; skipping live DB test")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	ctx := context.Background()
	clean := func() {
		_, _ = db.ExecContext(ctx, `DELETE FROM public.migrations WHERE app = $1`, app)
		_, _ = db.ExecContext(ctx, `DELETE FROM public.migration_repairs WHERE app = $1`, app)
		for _, tbl := range tables {
			_, _ = db.ExecContext(ctx, `DROP TABLE IF EXISTS `+tbl)
		}
	}
	if err := NewPostgres(db, app).Setup(ctx); err != nil {
		t.Fatalf("setup: %v", err)
	}
	clean()
	t.Cleanup(func() { clean(); db.Close() })
	return db, ctx
}

// stampLedger rewrites a ledger row's recorded identity, which is exactly what
// a restored backup, an adopted database or a hand-fix leaves behind.
func stampLedger(t *testing.T, db *sql.DB, ctx context.Context, app, key, filename, digest string) {
	t.Helper()
	res, err := db.ExecContext(ctx,
		`UPDATE public.migrations SET filename = $1, content_sha256 = $2
		  WHERE app = $3 AND database = 'postgres' AND schema = '' AND name = $4`,
		filename, digest, app, key)
	if err != nil {
		t.Fatalf("stamp ledger: %v", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		t.Fatalf("stamp ledger touched %d rows, want 1", n)
	}
}

func auditRows(t *testing.T, db *sql.DB, ctx context.Context, app string) []RepairRecord {
	t.Helper()
	records, err := NewPostgres(db, app).RepairHistory(ctx)
	if err != nil {
		t.Fatalf("repair history: %v", err)
	}
	return records
}

// TestRepair_AdoptBindsARestoredLedger is the fourth runbook row: the FILES are
// right and the LEDGER is the wrong side. A restored backup remembers a tree
// that no longer exists, so every identity check fires on a chain that is
// perfectly fine. Before v1.6.0 the only way out was a hand-written UPDATE.
func TestRepair_AdoptBindsARestoredLedger(t *testing.T) {
	const app = "mk_repair_adopt"
	clearCI(t)
	db, ctx := repairTestDB(t, app, "mk_repair_adopt_a")

	chain := []Migration{{Name: "0001_init.up.sql", Content: `CREATE TABLE mk_repair_adopt_a (id int)`}}
	if err := NewPostgres(db, app).ApplyMigrations(ctx, chain); err != nil {
		t.Fatalf("initial apply: %v", err)
	}
	// The restore: the ledger names a file this tree has never had.
	stampLedger(t, db, ctx, app, "1", "0001_init_from_the_backup.up.sql", ContentDigest("whatever the old tree held"))

	if err := NewPostgres(db, app).ApplyMigrations(ctx, chain); err == nil {
		t.Fatal("a ledger naming a different file must refuse")
	} else if !strings.Contains(err.Error(), "repair adopt") {
		t.Errorf("the refusal must name the verb that resolves it; got: %v", err)
	}

	res, err := NewPostgres(db, app).RepairAdopt(ctx, chain[0],
		RepairRequest{Reason: "restored the 2026-08-10 backup; the chain is unchanged", Operator: "paul"})
	if err != nil {
		t.Fatalf("repair adopt: %v", err)
	}
	if res.OldFilename != "0001_init_from_the_backup.up.sql" || res.NewFilename != "0001_init.up.sql" {
		t.Fatalf("the result must show both identities: %+v", res)
	}

	if err := NewPostgres(db, app).ApplyMigrations(ctx, chain); err != nil {
		t.Fatalf("boot must be clean after the repair: %v", err)
	}

	records := auditRows(t, db, ctx, app)
	if len(records) != 1 {
		t.Fatalf("want exactly one audit row, got %d", len(records))
	}
	r := records[0]
	switch {
	case r.Verb != "repair adopt":
		t.Errorf("verb = %q", r.Verb)
	case r.Key != "1":
		t.Errorf("key = %q", r.Key)
	case !strings.Contains(r.Reason, "restored the 2026-08-10 backup"):
		t.Errorf("reason must be recorded verbatim, got %q", r.Reason)
	case r.Operator != "paul":
		t.Errorf("operator = %q", r.Operator)
	case r.OSUser == "" || r.OSUser == "unknown":
		t.Errorf("the OS user must be recorded even when --operator is given, got %q", r.OSUser)
	case r.Host == "":
		t.Errorf("host must be recorded")
	case r.OldFilename != "0001_init_from_the_backup.up.sql" || r.NewFilename != "0001_init.up.sql":
		t.Errorf("the audit row must carry both identities: %+v", r)
	case r.OldDigest == "" || r.NewDigest == "" || r.OldDigest == r.NewDigest:
		t.Errorf("the audit row must carry both digests: %+v", r)
	case time.Since(r.At) > time.Hour:
		t.Errorf("repaired_at = %v", r.At)
	}
}

// TestRepair_AdoptAllUnmatched is the realistic restore: one dump, one restore,
// and EVERY row disagrees at once. Repairing them one at a time is the same
// headache in a nicer costume.
func TestRepair_AdoptAllUnmatched(t *testing.T) {
	const app = "mk_repair_adopt_all"
	clearCI(t)
	db, ctx := repairTestDB(t, app, "mk_repair_all_a", "mk_repair_all_b", "mk_repair_all_c")

	chain := []Migration{
		{Name: "0001_a.up.sql", Content: `CREATE TABLE mk_repair_all_a (id int)`},
		{Name: "0002_b.up.sql", Content: `CREATE TABLE mk_repair_all_b (id int)`},
		{Name: "0003_c.up.sql", Content: `CREATE TABLE mk_repair_all_c (id int)`},
	}
	if err := NewPostgres(db, app).ApplyMigrations(ctx, chain); err != nil {
		t.Fatalf("initial apply: %v", err)
	}
	for _, key := range []string{"1", "2", "3"} {
		stampLedger(t, db, ctx, app, key, "000"+key+"_old_tree.up.sql", ContentDigest("old "+key))
	}
	if err := NewPostgres(db, app).ApplyMigrations(ctx, chain); err == nil {
		t.Fatal("a wholly mismatched ledger must refuse")
	}

	results, err := NewPostgres(db, app).RepairAdoptAllUnmatched(ctx, chain,
		RepairRequest{Reason: "restored from the nightly dump of another cluster"})
	if err != nil {
		t.Fatalf("repair adopt --all-unmatched: %v", err)
	}
	if len(results) != 3 {
		t.Fatalf("want 3 rows adopted, got %d", len(results))
	}
	if err := NewPostgres(db, app).ApplyMigrations(ctx, chain); err != nil {
		t.Fatalf("boot must be clean after the batch repair: %v", err)
	}
	if got := len(auditRows(t, db, ctx, app)); got != 3 {
		t.Fatalf("a batch repair is still three facts: want 3 audit rows, got %d", got)
	}
	// Running it again has nothing to do, and says so rather than writing
	// three more audit rows.
	if _, err := NewPostgres(db, app).RepairAdoptAllUnmatched(ctx, chain,
		RepairRequest{Reason: "again"}); err == nil {
		t.Fatal("a second batch repair must report that there is nothing to adopt")
	}
	if got := len(auditRows(t, db, ctx, app)); got != 3 {
		t.Fatalf("a no-op repair must not write audit rows, got %d", got)
	}
}

// TestRepair_AcceptContentClearsTheDriftWarning is Paul's 2026-08-11 ruling in
// full: a semantic edit to an applied migration BOOTS, loudly. The operator who cannot
// restore the old bytes is not locked out — and the acknowledgement is a
// recorded act, not a silence.
func TestRepair_AcceptContentClearsTheDriftWarning(t *testing.T) {
	const app = "mk_repair_content"
	clearCI(t)
	db, ctx := repairTestDB(t, app, "mk_repair_content_a")

	orig := []Migration{{Name: "0001_init.up.sql", Content: `CREATE TABLE mk_repair_content_a (id int)`}}
	if err := NewPostgres(db, app).ApplyMigrations(ctx, orig); err != nil {
		t.Fatalf("initial apply: %v", err)
	}

	// An actual token/DDL change to a migration that already ran.
	edited := []Migration{{Name: "0001_init.up.sql", Content: `CREATE TABLE mk_repair_content_a (id int, note text)`}}

	var warnings []Discrepancy
	err := NewPostgres(db, app).
		WithWarnFunc(func(d Discrepancy) { warnings = append(warnings, d) }).
		ApplyMigrations(ctx, edited)
	if err != nil {
		t.Fatalf("content drift must NOT block the boot: %v", err)
	}
	if len(warnings) != 1 {
		t.Fatalf("want exactly one warning, got %d", len(warnings))
	}
	w := warnings[0]
	if w.Kind != KindContentDrift || w.Severity != SeverityWarning {
		t.Fatalf("want a content-drift warning, got %s/%s", w.Kind, w.Severity)
	}
	for _, want := range []string{"0001_init.up.sql", shortDigest(w.LedgerDigest), shortDigest(w.FileDigest), "diverge", "repair accept-content"} {
		if !strings.Contains(w.String(), want) {
			t.Errorf("the warning must name %q; got:\n%s", want, w.String())
		}
	}

	// status shows it too, at warning severity, without failing.
	st, err := NewPostgres(db, app).Status(ctx, edited)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if st.HasErrors() {
		t.Fatal("content drift alone must not make status report errors")
	}
	if len(st.Discrepancies) != 1 || st.Discrepancies[0].Kind != KindContentDrift {
		t.Fatalf("status must surface the drift: %+v", st.Discrepancies)
	}

	res, err := NewPostgres(db, app).RepairAcceptContent(ctx, edited[0],
		RepairRequest{Reason: "reviewed intended baseline change; existing database reconciled separately"})
	if err != nil {
		t.Fatalf("repair accept-content: %v", err)
	}
	if res.NewDigest != ContentDigest(edited[0].Content) {
		t.Fatalf("accept-content must re-stamp the digest: %+v", res)
	}

	warnings = nil
	if err := NewPostgres(db, app).
		WithWarnFunc(func(d Discrepancy) { warnings = append(warnings, d) }).
		ApplyMigrations(ctx, edited); err != nil {
		t.Fatalf("the warning must be cleared: %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("accept-content must SILENCE the warning, got %d", len(warnings))
	}

	records := auditRows(t, db, ctx, app)
	if len(records) != 1 || records[0].Verb != "repair accept-content" {
		t.Fatalf("the acknowledgement must be recorded: %+v", records)
	}
	if records[0].OldDigest == records[0].NewDigest {
		t.Errorf("the audit row must show which digest was replaced: %+v", records[0])
	}
}

// TestRepair_AcceptContentRefusesAnIdentityMismatch keeps the verbs honest:
// accept-content re-stamps a digest, and using it on a ledger row that names a
// DIFFERENT file would bind the ledger to a file it never ran.
func TestRepair_AcceptContentRefusesAnIdentityMismatch(t *testing.T) {
	const app = "mk_repair_wrongverb"
	clearCI(t)
	db, ctx := repairTestDB(t, app, "mk_repair_wrongverb_a")

	chain := []Migration{{Name: "0001_init.up.sql", Content: `CREATE TABLE mk_repair_wrongverb_a (id int)`}}
	if err := NewPostgres(db, app).ApplyMigrations(ctx, chain); err != nil {
		t.Fatalf("initial apply: %v", err)
	}
	stampLedger(t, db, ctx, app, "1", "0001_something_else.up.sql", ContentDigest("other"))

	_, err := NewPostgres(db, app).RepairAcceptContent(ctx, chain[0], RepairRequest{Reason: "wrong verb on purpose"})
	if err == nil {
		t.Fatal("accept-content must refuse an identity mismatch")
	}
	if !strings.Contains(err.Error(), "repair adopt") {
		t.Errorf("the refusal must point at the right verb; got: %v", err)
	}
	if got := len(auditRows(t, db, ctx, app)); got != 0 {
		t.Fatalf("a refused repair must write nothing, got %d audit rows", got)
	}
}

// TestRepair_ApplyAllowBelowApplied is the ordering escape: a genuine backport
// that has to land below the high-water mark, applied ONCE, with the deviation
// on the record instead of a permanently loosened rule.
func TestRepair_ApplyAllowBelowApplied(t *testing.T) {
	const app = "mk_repair_order"
	clearCI(t)
	db, ctx := repairTestDB(t, app, "mk_repair_order_a", "mk_repair_order_b")

	first := []Migration{{Name: "0005_later.up.sql", Content: `CREATE TABLE mk_repair_order_a (id int)`}}
	if err := NewPostgres(db, app).WithStrictOrdering().ApplyMigrations(ctx, first); err != nil {
		t.Fatalf("first apply: %v", err)
	}

	late := []Migration{
		{Name: "0003_backport.up.sql", Content: `CREATE TABLE mk_repair_order_b (id int)`},
		first[0],
	}
	if err := NewPostgres(db, app).WithStrictOrdering().ApplyMigrations(ctx, late); err == nil {
		t.Fatal("strict ordering must still refuse by default")
	} else if !strings.Contains(err.Error(), "allow-below-applied") {
		t.Errorf("the refusal must name the exception; got: %v", err)
	}

	if err := NewPostgres(db, app).WithStrictOrdering().ApplyWithOrderingException(
		ctx, late, []string{"3"}, RepairRequest{Reason: "backport of the th#1712 index; verified independent of 0005"},
	); err != nil {
		t.Fatalf("ordering exception: %v", err)
	}

	var exists bool
	if err := db.QueryRowContext(ctx, `SELECT to_regclass('public.mk_repair_order_b') IS NOT NULL`).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if !exists {
		t.Fatal("the exempted migration's DDL must actually run")
	}

	records := auditRows(t, db, ctx, app)
	if len(records) != 1 {
		t.Fatalf("the deviation must be recorded exactly once, got %d", len(records))
	}
	if records[0].Verb != "apply --allow-below-applied" || records[0].Key != "3" {
		t.Fatalf("audit row = %+v", records[0])
	}
	if records[0].NewFilename != "0003_backport.up.sql" {
		t.Errorf("the audit row must name what was applied out of order: %+v", records[0])
	}

	// The exception was one-shot: it applied a migration, it did not disable
	// the rule. A SECOND late arrival is refused again.
	later := append([]Migration{{Name: "0002_another.up.sql", Content: `SELECT 1`}}, late...)
	if err := NewPostgres(db, app).WithStrictOrdering().ApplyMigrations(ctx, later); err == nil {
		t.Fatal("the ordering rule must still be in force after an exception")
	}
}

// TestRepair_DryRunWritesNothing: every verb can be rehearsed. An operator
// running a repair on a production ledger should be able to see the exact
// before/after first.
func TestRepair_DryRunWritesNothing(t *testing.T) {
	const app = "mk_repair_dryrun"
	clearCI(t)
	db, ctx := repairTestDB(t, app, "mk_repair_dryrun_a")

	chain := []Migration{{Name: "0001_init.up.sql", Content: `CREATE TABLE mk_repair_dryrun_a (id int)`}}
	if err := NewPostgres(db, app).ApplyMigrations(ctx, chain); err != nil {
		t.Fatalf("initial apply: %v", err)
	}
	stampLedger(t, db, ctx, app, "1", "0001_from_backup.up.sql", ContentDigest("old"))

	res, err := NewPostgres(db, app).RepairAdopt(ctx, chain[0],
		RepairRequest{Reason: "rehearsing the restore fix", DryRun: true})
	if err != nil {
		t.Fatalf("dry-run adopt: %v", err)
	}
	if !res.DryRun || !strings.Contains(res.String(), "[dry-run]") {
		t.Fatalf("a dry run must announce itself: %+v", res)
	}

	records, err := NewPostgres(db, app).AppliedRecords(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if records["1"].Filename != "0001_from_backup.up.sql" {
		t.Fatalf("a dry run must not touch the ledger, got %+v", records["1"])
	}
	if got := len(auditRows(t, db, ctx, app)); got != 0 {
		t.Fatalf("a dry run must not write audit rows, got %d", got)
	}
}

// TestRepair_NeverTouchesSchema pins the boundary: repairs rewrite ledger
// identity and nothing else.
func TestRepair_NeverTouchesSchema(t *testing.T) {
	const app = "mk_repair_schema"
	clearCI(t)
	db, ctx := repairTestDB(t, app, "mk_repair_schema_a")

	chain := []Migration{{Name: "0001_init.up.sql", Content: `CREATE TABLE mk_repair_schema_a (id int)`}}
	if err := NewPostgres(db, app).ApplyMigrations(ctx, chain); err != nil {
		t.Fatalf("initial apply: %v", err)
	}
	before := columnsOf(t, db, ctx, "mk_repair_schema_a")

	stampLedger(t, db, ctx, app, "1", "0001_from_backup.up.sql", ContentDigest("old"))
	if _, err := NewPostgres(db, app).RepairAdopt(ctx, chain[0], RepairRequest{Reason: "restore"}); err != nil {
		t.Fatalf("repair adopt: %v", err)
	}

	if after := columnsOf(t, db, ctx, "mk_repair_schema_a"); after != before {
		t.Fatalf("a repair changed the schema: %q -> %q", before, after)
	}
}

func columnsOf(t *testing.T, db *sql.DB, ctx context.Context, table string) string {
	t.Helper()
	rows, err := db.QueryContext(ctx,
		`SELECT column_name, data_type FROM information_schema.columns
		  WHERE table_schema = 'public' AND table_name = $1 ORDER BY ordinal_position`, table)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name, typ string
		if err := rows.Scan(&name, &typ); err != nil {
			t.Fatal(err)
		}
		out = append(out, name+":"+typ)
	}
	return strings.Join(out, ",")
}

// TestStatus_ExplainsEveryDiscrepancy is the verb the boot errors point at. It
// has to answer all three questions — what, why, and what do I type — without
// the operator opening the source.
func TestStatus_ExplainsEveryDiscrepancy(t *testing.T) {
	const app = "mk_status"
	clearCI(t)
	db, ctx := repairTestDB(t, app, "mk_status_a")

	applied := []Migration{{Name: "0001_init.up.sql", Content: `CREATE TABLE mk_status_a (id int)`}}
	if err := NewPostgres(db, app).ApplyMigrations(ctx, applied); err != nil {
		t.Fatalf("initial apply: %v", err)
	}
	stampLedger(t, db, ctx, app, "1", "0001_from_backup.up.sql", ContentDigest("old"))
	// A ledger row with no file behind it, and a pending migration.
	if _, err := db.ExecContext(ctx,
		`INSERT INTO public.migrations (app, database, schema, name, filename, content_sha256)
		 VALUES ($1, 'postgres', '', '9', '0009_deleted.up.sql', 'deadbeef')`, app); err != nil {
		t.Fatal(err)
	}
	chain := append(applied, Migration{Name: "0002_pending.up.sql", Content: `SELECT 1`})

	st, err := NewPostgres(db, app).Status(ctx, chain)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if !st.HasErrors() {
		t.Fatal("an identity mismatch must make status report errors")
	}
	if len(st.Pending) != 1 || st.Pending[0] != "0002_pending.up.sql" {
		t.Fatalf("status must list what has not run: %v", st.Pending)
	}

	kinds := map[DiscrepancyKind]bool{}
	for _, d := range st.Discrepancies {
		kinds[d.Kind] = true
	}
	if !kinds[KindNumberMismatch] || !kinds[KindLedgerOnly] {
		t.Fatalf("status must report both the mismatch and the orphaned row: %+v", st.Discrepancies)
	}
	// Errors sort first, so an operator reads the blocker before the notes.
	if st.Discrepancies[0].Severity != SeverityError {
		t.Fatalf("errors must sort first, got %s", st.Discrepancies[0].Severity)
	}

	report := st.Report()
	for _, want := range []string{
		"0001_init.up.sql",        // WHAT: the file
		"0001_from_backup.up.sql", // WHAT: the identity the ledger holds
		"LIKELY CAUSE",            // WHY
		"RESOLUTION",              // WHAT DO I TYPE
		"migratekit repair adopt", // ...specifically
		"Renumber",                // ...and the other cause's fix
		"0002_pending.up.sql",     // pending set
	} {
		if !strings.Contains(report, want) {
			t.Errorf("status report must contain %q; got:\n%s", want, report)
		}
	}

	// After the repair, the report says so.
	if _, err := NewPostgres(db, app).RepairAdopt(ctx, applied[0],
		RepairRequest{Reason: "restored backup", Operator: "paul"}); err != nil {
		t.Fatalf("repair adopt: %v", err)
	}
	st, err = NewPostgres(db, app).Status(ctx, chain)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if st.HasErrors() {
		t.Fatalf("the repair must clear the blocker: %+v", st.Discrepancies)
	}
	if !strings.Contains(st.Report(), "REPAIR HISTORY") || !strings.Contains(st.Report(), "restored backup") {
		t.Errorf("status must show the repair trail:\n%s", st.Report())
	}
}
