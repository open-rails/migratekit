package migratekit

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/open-rails/migratekit/internal/coremigrate"
)

// A chain that rebaselines leaves every database built by the old chain with a
// ledger the new tree cannot use: its rows name files that no longer exist, or
// record content the new baseline no longer has. Applying later migrations on
// top of that schema is how a database silently diverges.
//
// A Conversion is the chain's own, reviewed answer: "a database built by THIS
// retired chain becomes our baseline by running THIS SQL". migratekit runs it
// only when both the ledger and the live schema prove the database is exactly
// that retired chain, and commits it only when the result is exactly a fresh
// build of the new baseline. Everything happens in one transaction under the
// migration lock, including the ledger rewrite and its audit rows, so a
// conversion either lands completely or not at all.
//
// "Exactly" means equal schema fingerprints (see fingerprint.go): the same
// tables, columns, types, constraints, indexes, triggers, functions, views,
// sequences and policies. The reference shapes are built by running the
// migrations themselves in a throwaway schema inside the same transaction and
// rolling it back, so nothing is hard-coded and nothing persists.

// Render renders a migration chain for a target schema. Migrations that are
// schema-relative (they run under search_path) can return their content
// unchanged; hard-qualified migrations relocate their qualifiers. Rendered for
// the migrator's own schema, a chain must reproduce the ledger's digests.
type Render func(schema string) ([]Migration, error)

// Conversion upgrades a database built by a retired chain to the leading
// migrations of the current chain.
type Conversion struct {
	// Name identifies the conversion in errors and in the audit trail.
	Name string
	// Retired renders the retired chain. A database whose ledger records
	// exactly these migrations (or whose ledger is empty but whose schema has
	// exactly their shape) is converted.
	Retired Render
	// Replaces is how many leading migrations of the current chain the
	// converted schema satisfies; they are recorded as applied.
	Replaces int
	// SQL renders the conversion for a schema. It runs under the same
	// search_path and schema relocation as a migration.
	SQL func(schema string) (string, error)
	// Fallback tells an operator what to run instead if the conversion
	// refuses, e.g. "authkit v0.124.0, the last release of that chain".
	Fallback string
}

// ErrSchemaMismatch is returned when a schema is not what its ledger claims,
// so continuing would apply migrations to a schema they were not written for.
var ErrSchemaMismatch = errors.New("migratekit: schema does not match its ledger")

// WithConversions declares retired chains this migrator can convert from.
func (p *Postgres) WithConversions(conversions ...Conversion) *Postgres {
	p.conversions = append(p.conversions, conversions...)
	return p
}

// WithRender declares how the current chain renders for another schema. It is
// needed only when migrations are hard-qualified to the migrator's schema;
// schema-relative migrations relocate by search_path alone.
func (p *Postgres) WithRender(render Render) *Postgres {
	p.render = render
	return p
}

// WithStrictIntegrity refuses to apply migrations over an applied migration
// whose content changed, unless the live schema still equals a fresh build of
// the tree's applied migrations. An edit that changes nothing a database does
// (a comment, formatting, an index rename) is re-stamped on the record and the
// apply proceeds; an edit that changes the schema refuses with the migration,
// the schema diff and the way out. Without it, content drift is a warning.
func (p *Postgres) WithStrictIntegrity() *Postgres {
	p.strictIntegrity = true
	return p
}

// reconcile runs before any migration is applied. It converts a retired chain
// or, under strict integrity, settles content drift. It takes the migration
// lock only when there is something to decide.
func (p *Postgres) reconcile(ctx context.Context, migrations []Migration) (err error) {
	if len(p.conversions) == 0 && !p.strictIntegrity {
		return nil
	}
	records, err := p.AppliedRecords(ctx)
	if err != nil {
		return err
	}
	need, err := p.reconcileNeeded(ctx, migrations, records)
	if err != nil || !need {
		return err
	}
	if err := p.lock(ctx); err != nil {
		return err
	}
	defer func() {
		if unlockErr := p.unlock(ctx); unlockErr != nil {
			err = errors.Join(err, unlockErr)
		}
	}()
	if records, err = p.AppliedRecords(ctx); err != nil {
		return err
	}
	for _, c := range p.conversions {
		retired, err := c.Retired(p.schema)
		if err != nil {
			return fmt.Errorf("migratekit: render retired chain %s: %w", c.Name, err)
		}
		if ledgerRecords(records, retired) {
			return p.convert(ctx, c, migrations, records, false)
		}
	}
	if len(records) == 0 && len(p.conversions) > 0 {
		for _, c := range p.conversions {
			err := p.convert(ctx, c, migrations, records, true)
			if errors.Is(err, errShapeDiffers) {
				continue
			}
			return err
		}
		return nil
	}
	if p.strictIntegrity {
		return p.settleDrift(ctx, migrations, records)
	}
	return nil
}

// reconcileNeeded answers from the ledger alone, without the lock.
func (p *Postgres) reconcileNeeded(ctx context.Context, migrations []Migration, records map[string]AppliedRecord) (bool, error) {
	for _, c := range p.conversions {
		retired, err := c.Retired(p.schema)
		if err != nil {
			return false, fmt.Errorf("migratekit: render retired chain %s: %w", c.Name, err)
		}
		if ledgerRecords(records, retired) {
			return true, nil
		}
	}
	if len(records) == 0 && len(p.conversions) > 0 && strings.TrimSpace(p.schema) != "" {
		var populated bool
		err := p.db.QueryRowContext(ctx, `SELECT EXISTS (
			SELECT 1 FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
			 WHERE n.nspname = $1 AND c.relkind IN ('r','p'))`, p.schema).Scan(&populated)
		if err != nil {
			return false, err
		}
		return populated, nil
	}
	return p.strictIntegrity && len(drifted(migrations, records)) > 0, nil
}

// ledgerRecords reports whether the ledger holds exactly the retired chain,
// every row finished and recording the retired file's content.
func ledgerRecords(records map[string]AppliedRecord, retired []Migration) bool {
	if len(records) == 0 || len(records) != len(retired) {
		return false
	}
	for _, m := range retired {
		rec, ok := records[Prefix(m.Name)]
		if !ok || rec.isDirty() || rec.Filename != m.Name {
			return false
		}
		if rec.Digest != ContentDigest(m.Content) && rec.SemanticDigest != SemanticContentDigest(m.Content) {
			return false
		}
	}
	return true
}

var errShapeDiffers = errors.New("schema shape differs from the retired chain")

func (p *Postgres) convert(ctx context.Context, c Conversion, migrations []Migration, records map[string]AppliedRecord, ledgerless bool) error {
	if c.Replaces < 1 || c.Replaces > len(migrations) {
		return fmt.Errorf("migratekit: conversion %s replaces %d migrations, but the tree carries %d", c.Name, c.Replaces, len(migrations))
	}
	replaced := migrations[:c.Replaces]
	fallback := ""
	if c.Fallback != "" {
		fallback = " Alternatively run " + c.Fallback + "."
	}

	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	live, err := schemaFingerprint(ctx, tx, p.schema)
	if err != nil {
		return err
	}
	retired, err := p.scratchFingerprint(ctx, tx, c.Retired)
	if err != nil {
		return fmt.Errorf("migratekit: conversion %s: build the retired chain for comparison: %w", c.Name, err)
	}
	if d := live.diff(retired); len(d) > 0 {
		if ledgerless {
			return errShapeDiffers
		}
		return fmt.Errorf("%w: the ledger for app %q schema %q records the retired chain %s, but the schema is not what that chain builds:\n%s"+
			"  Proceeding would convert and migrate a schema nobody wrote migrations for, so nothing was changed.\n"+
			"  RESOLUTION: make the listed objects match the retired chain (they were changed outside its migrations), then restart; the conversion then runs by itself.%s",
			ErrSchemaMismatch, p.app, p.schema, c.Name, renderDiff(d, "database", c.Name), fallback)
	}

	body, err := c.SQL(p.schema)
	if err != nil {
		return fmt.Errorf("migratekit: render conversion %s: %w", c.Name, err)
	}
	if err := p.execIn(ctx, tx, p.schema, body); err != nil {
		return fmt.Errorf("migratekit: conversion %s refused or failed, nothing was changed: %w.%s", c.Name, err, fallback)
	}

	converted, err := schemaFingerprint(ctx, tx, p.schema)
	if err != nil {
		return err
	}
	fresh, err := p.scratchFingerprint(ctx, tx, p.currentRender(replaced))
	if err != nil {
		return fmt.Errorf("migratekit: conversion %s: build the current baseline for comparison: %w", c.Name, err)
	}
	if d := converted.diff(fresh); len(d) > 0 {
		return fmt.Errorf("%w: conversion %s does not produce a fresh %s schema, so it was rolled back and nothing changed:\n%s"+
			"  This is a defect in the conversion, not in the database.%s",
			ErrSchemaMismatch, c.Name, replaced[len(replaced)-1].Name, renderDiff(d, "converted", "fresh build"), fallback)
	}

	// The ledger now tells the truth: the retired rows go, the replaced
	// migrations are applied, and each change has an audit row.
	req := RepairRequest{Reason: "verified conversion " + c.Name, Operator: "migratekit"}
	replacedKeys := map[string]Migration{}
	for _, m := range replaced {
		replacedKeys[Prefix(m.Name)] = m
	}
	for key, rec := range records {
		seq, err := strconv.ParseInt(key, 10, 64)
		if err != nil {
			return fmt.Errorf("migratekit: invalid ledger sequence %q: %w", key, err)
		}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM public.migrations WHERE app = $1 AND database = $2 AND schema = $3 AND sequence = $4`,
			p.app, postgresDriver, p.schema, seq); err != nil {
			return err
		}
		if _, replacedToo := replacedKeys[key]; !replacedToo {
			if err := p.insertAudit(ctx, tx, "convert", key, rec.Filename, rec.Digest, "", "", req); err != nil {
				return err
			}
		}
	}
	for _, m := range replaced {
		key := Prefix(m.Name)
		seq, err := Sequence(m.Name)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO public.migrations (app, database, schema, sequence, filename, content_sha256, semantic_sha256)
			 VALUES ($1, $2, $3, $4, $5, $6, $7)`,
			p.app, postgresDriver, p.schema, seq, m.Name, ContentDigest(m.Content), SemanticContentDigest(m.Content)); err != nil {
			return err
		}
		old := records[key]
		if err := p.insertAudit(ctx, tx, "convert", key, old.Filename, old.Digest, m.Name, ContentDigest(m.Content), req); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// settleDrift decides content drift on the schema, not the bytes: if the live
// schema equals a fresh build of the applied migrations, the edit changed
// nothing a database does and the digests are re-stamped on the record.
func (p *Postgres) settleDrift(ctx context.Context, migrations []Migration, records map[string]AppliedRecord) error {
	edited := drifted(migrations, records)
	if len(edited) == 0 {
		return nil
	}
	var names []string
	for _, m := range edited {
		rec := records[Prefix(m.Name)]
		names = append(names, fmt.Sprintf("%s (applied %s, file %s)", m.Name, shortDigest(rec.Digest), shortDigest(ContentDigest(m.Content))))
	}
	what := strings.Join(names, ", ")
	resolution := "  RESOLUTION: revert the file to the content this database ran and put the change in a NEW migration, " +
		"or, if the chain was rebaselined on purpose, declare a Conversion for the retired chain so databases built by it are converted.\n" +
		"  Until then run the release that shipped the applied content."
	if strings.TrimSpace(p.schema) == "" {
		return fmt.Errorf("%w: applied migration edited: %s. Strict integrity compares schemas, which needs a dedicated schema (WithSchema); this migrator has none, so nothing was applied.\n%s",
			ErrSchemaMismatch, what, resolution)
	}
	var applied []Migration
	for _, m := range migrations {
		if _, ok := records[Prefix(m.Name)]; ok {
			applied = append(applied, m)
		}
	}

	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	live, err := schemaFingerprint(ctx, tx, p.schema)
	if err != nil {
		return err
	}
	fresh, err := p.scratchFingerprint(ctx, tx, p.currentRender(applied))
	if err != nil {
		return fmt.Errorf("%w: applied migration edited: %s, and the applied migrations could not be rebuilt for comparison (%v), so nothing was applied.\n%s",
			ErrSchemaMismatch, what, err, resolution)
	}
	if d := live.diff(fresh); len(d) > 0 {
		return fmt.Errorf("%w: applied migration edited: %s. The schema differs from a fresh build of the applied migrations:\n%s"+
			"  Applying later migrations here would build a schema no fresh database has, so nothing was applied.\n%s",
			ErrSchemaMismatch, what, renderDiff(d, "database", "fresh build"), resolution)
	}
	req := RepairRequest{Reason: "content edit verified to leave the schema unchanged", Operator: "migratekit"}
	for _, m := range edited {
		rec := records[Prefix(m.Name)]
		digest := ContentDigest(m.Content)
		seq, err := Sequence(m.Name)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE public.migrations SET content_sha256 = $1, semantic_sha256 = $2
			  WHERE app = $3 AND database = $4 AND schema = $5 AND sequence = $6`,
			digest, SemanticContentDigest(m.Content), p.app, postgresDriver, p.schema, seq); err != nil {
			return err
		}
		if err := p.insertAudit(ctx, tx, "verify-content", rec.Key, rec.Filename, rec.Digest, m.Name, digest, req); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// drifted lists applied migrations whose recorded content differs from the
// tree, identity intact (a different filename is a hard error elsewhere).
func drifted(migrations []Migration, records map[string]AppliedRecord) []Migration {
	var out []Migration
	for _, m := range migrations {
		rec, ok := records[Prefix(m.Name)]
		if !ok || rec.isDirty() || (rec.Filename != "" && rec.Filename != m.Name) || rec.Digest == "" {
			continue
		}
		if rec.Digest != ContentDigest(m.Content) &&
			(rec.SemanticDigest == "" || rec.SemanticDigest != SemanticContentDigest(m.Content)) {
			out = append(out, m)
		}
	}
	return out
}

// currentRender renders the given leading migrations of the current chain for
// a scratch schema.
func (p *Postgres) currentRender(want []Migration) Render {
	return func(schema string) ([]Migration, error) {
		if p.render == nil {
			return want, nil
		}
		all, err := p.render(schema)
		if err != nil {
			return nil, err
		}
		byName := map[string]Migration{}
		for _, m := range all {
			byName[m.Name] = m
		}
		out := make([]Migration, 0, len(want))
		for _, m := range want {
			r, ok := byName[m.Name]
			if !ok {
				return nil, fmt.Errorf("render has no %s", m.Name)
			}
			out = append(out, r)
		}
		return out, nil
	}
}

// scratchFingerprint builds a chain in a throwaway schema inside tx, reads its
// fingerprint and rolls the build back, leaving tx as it was.
func (p *Postgres) scratchFingerprint(ctx context.Context, tx *sql.Tx, render Render) (fp fingerprint, err error) {
	var raw [6]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return nil, err
	}
	scratch := "migratekit_scratch_" + hex.EncodeToString(raw[:])
	if _, err := tx.ExecContext(ctx, "SAVEPOINT migratekit_scratch"); err != nil {
		return nil, err
	}
	defer func() {
		if _, rbErr := tx.ExecContext(ctx, "ROLLBACK TO SAVEPOINT migratekit_scratch"); rbErr != nil {
			err = errors.Join(err, rbErr)
			return
		}
		if _, relErr := tx.ExecContext(ctx, "RELEASE SAVEPOINT migratekit_scratch"); relErr != nil {
			err = errors.Join(err, relErr)
		}
	}()
	if _, err := tx.ExecContext(ctx, `CREATE SCHEMA "`+scratch+`"`); err != nil {
		return nil, err
	}
	chain, err := render(scratch)
	if err != nil {
		return nil, err
	}
	for _, m := range chain {
		if hasNoTransactionDirective(m.Content) {
			return nil, fmt.Errorf("%s is a no-transaction migration and cannot be rebuilt inside a transaction", m.Name)
		}
		if err := p.execIn(ctx, tx, scratch, m.Content); err != nil {
			return nil, fmt.Errorf("%s: %w", m.Name, err)
		}
	}
	return schemaFingerprint(ctx, tx, scratch)
}

// execIn runs migration-style SQL against a schema: templates substituted,
// canonical schema names relocated, search_path set, exactly as applyOne does.
func (p *Postgres) execIn(ctx context.Context, tx *sql.Tx, schema, body string) error {
	quoted, err := quoteIdent(schema)
	if err != nil {
		return fmt.Errorf("invalid schema %q: %w", schema, err)
	}
	body, err = coremigrate.SubstituteTemplates(body)
	if err != nil {
		return err
	}
	body, err = rewriteSchemaRefs(body, schema, p.schemaRewriteFrom)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "SET LOCAL search_path = "+quoted+", public"); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, body)
	return err
}
