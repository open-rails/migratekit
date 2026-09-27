package migratekit

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// A schema fingerprint describes what a schema DOES, not how its DDL was
// spelled or which order it ran in. Two schemas with equal fingerprints accept
// and reject the same rows, fire the same triggers in the same order, and
// answer the same queries.
//
// Deliberately excluded, because none of it changes behaviour and all of it
// legitimately differs between a converted and a fresh schema: column order,
// comments, ownership and grants, the names of plain indexes, and the values
// of sequences. Constraint and trigger names are kept: application code
// matches on constraint names, and trigger names decide firing order.
// Objects that belong to an extension are skipped.

type fingerprint []string

// schemaFingerprint reads the catalog for one schema. It runs with
// search_path=pg_catalog so every name outside pg_catalog renders
// schema-qualified, then replaces the schema's own name with a placeholder so
// the same objects in two schemas compare equal.
func schemaFingerprint(ctx context.Context, tx *sql.Tx, schema string) (fingerprint, error) {
	if _, err := tx.ExecContext(ctx, "SET LOCAL search_path = pg_catalog"); err != nil {
		return nil, fmt.Errorf("fingerprint %s: %w", schema, err)
	}
	var oid uint32
	err := tx.QueryRowContext(ctx, `SELECT oid FROM pg_namespace WHERE nspname = $1`, schema).Scan(&oid)
	if err == sql.ErrNoRows {
		return fingerprint{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("fingerprint %s: %w", schema, err)
	}
	norm := newSchemaNormalizer(schema)
	var lines []string
	for _, q := range fingerprintQueries {
		rows, err := tx.QueryContext(ctx, q.sql, oid)
		if err != nil {
			return nil, fmt.Errorf("fingerprint %s (%s): %w", schema, q.kind, err)
		}
		for rows.Next() {
			var parts []sql.NullString
			cols, err := rows.Columns()
			if err != nil {
				rows.Close()
				return nil, err
			}
			parts = make([]sql.NullString, len(cols))
			dest := make([]any, len(cols))
			for i := range parts {
				dest[i] = &parts[i]
			}
			if err := rows.Scan(dest...); err != nil {
				rows.Close()
				return nil, fmt.Errorf("fingerprint %s (%s): %w", schema, q.kind, err)
			}
			fields := make([]string, len(parts))
			for i, p := range parts {
				fields[i] = norm.apply(p.String)
			}
			if q.post != nil {
				fields = q.post(fields)
			}
			lines = append(lines, q.kind+" "+strings.Join(fields, " | "))
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}
	sort.Strings(lines)
	return fingerprint(lines), nil
}

type fingerprintQuery struct {
	kind string
	sql  string
	post func([]string) []string
}

// notExtensionMember filters objects created by CREATE EXTENSION.
const notExtensionMember = `NOT EXISTS (SELECT 1 FROM pg_depend d WHERE d.objid = %s AND d.deptype = 'e')`

func extFilter(expr string) string { return fmt.Sprintf(notExtensionMember, expr) }

var indexName = regexp.MustCompile(`^(CREATE (?:UNIQUE )?INDEX )\S+( ON )`)

var fingerprintQueries = []fingerprintQuery{
	{kind: "relation", sql: `
		SELECT c.relname, c.relkind::text, c.relpersistence::text,
		       c.relrowsecurity::text, c.relforcerowsecurity::text,
		       CASE WHEN c.relkind = 'p' THEN pg_get_partkeydef(c.oid) END,
		       pg_get_expr(c.relpartbound, c.oid)
		  FROM pg_class c
		 WHERE c.relnamespace = $1 AND c.relkind IN ('r','p','v','m','f','c','S')
		   AND ` + extFilter("c.oid")},
	{kind: "column", sql: `
		SELECT c.relname, a.attname, format_type(a.atttypid, a.atttypmod),
		       a.attnotnull::text, pg_get_expr(ad.adbin, ad.adrelid),
		       a.attidentity::text, a.attgenerated::text,
		       CASE WHEN a.attcollation <> t.typcollation THEN co.collname END
		  FROM pg_attribute a
		  JOIN pg_class c ON c.oid = a.attrelid
		  JOIN pg_type t ON t.oid = a.atttypid
		  LEFT JOIN pg_attrdef ad ON ad.adrelid = a.attrelid AND ad.adnum = a.attnum
		  LEFT JOIN pg_collation co ON co.oid = a.attcollation
		 WHERE c.relnamespace = $1 AND c.relkind IN ('r','p','v','m','f','c')
		   AND a.attnum > 0 AND NOT a.attisdropped
		   AND ` + extFilter("c.oid")},
	{kind: "constraint", sql: `
		SELECT COALESCE(cl.relname, ty.typname), con.conname, con.contype::text,
		       pg_get_constraintdef(con.oid, true)
		  FROM pg_constraint con
		  LEFT JOIN pg_class cl ON cl.oid = con.conrelid
		  LEFT JOIN pg_type ty ON ty.oid = con.contypid
		 WHERE con.connamespace = $1 AND ` + extFilter("con.oid")},
	{kind: "index", sql: `
		SELECT tc.relname, pg_get_indexdef(i.indexrelid)
		  FROM pg_index i
		  JOIN pg_class ic ON ic.oid = i.indexrelid
		  JOIN pg_class tc ON tc.oid = i.indrelid
		 WHERE ic.relnamespace = $1
		   AND NOT EXISTS (SELECT 1 FROM pg_constraint k WHERE k.conindid = i.indexrelid AND k.contype IN ('p','u','x'))
		   AND ` + extFilter("i.indexrelid"),
		post: func(f []string) []string {
			f[1] = indexName.ReplaceAllString(f[1], "${1}_${2}")
			return f
		}},
	{kind: "trigger", sql: `
		SELECT c.relname, t.tgname, pg_get_triggerdef(t.oid, true), t.tgenabled::text
		  FROM pg_trigger t JOIN pg_class c ON c.oid = t.tgrelid
		 WHERE c.relnamespace = $1 AND NOT t.tgisinternal`},
	{kind: "function", sql: `
		SELECT p.proname, pg_get_function_identity_arguments(p.oid),
		       CASE WHEN p.prokind IN ('f','p') THEN pg_get_functiondef(p.oid) ELSE p.prokind::text END
		  FROM pg_proc p
		 WHERE p.pronamespace = $1 AND ` + extFilter("p.oid")},
	{kind: "view", sql: `
		SELECT c.relname, pg_get_viewdef(c.oid, true)
		  FROM pg_class c
		 WHERE c.relnamespace = $1 AND c.relkind IN ('v','m') AND ` + extFilter("c.oid")},
	{kind: "sequence", sql: `
		SELECT c.relname, format_type(s.seqtypid, NULL), s.seqstart::text, s.seqincrement::text,
		       s.seqmax::text, s.seqmin::text, s.seqcache::text, s.seqcycle::text,
		       (SELECT tc.relname || '.' || a.attname
		          FROM pg_depend d
		          JOIN pg_class tc ON tc.oid = d.refobjid
		          JOIN pg_attribute a ON a.attrelid = d.refobjid AND a.attnum = d.refobjsubid
		         WHERE d.classid = 'pg_class'::regclass AND d.objid = c.oid
		           AND d.refclassid = 'pg_class'::regclass AND d.deptype IN ('a','i'))
		  FROM pg_sequence s JOIN pg_class c ON c.oid = s.seqrelid
		 WHERE c.relnamespace = $1 AND ` + extFilter("c.oid")},
	{kind: "type", sql: `
		SELECT t.typname, t.typtype::text,
		       CASE t.typtype
		         WHEN 'e' THEN (SELECT string_agg(quote_literal(e.enumlabel), ',' ORDER BY e.enumsortorder) FROM pg_enum e WHERE e.enumtypid = t.oid)
		         WHEN 'd' THEN format_type(t.typbasetype, t.typtypmod) || ' notnull=' || t.typnotnull::text || ' default=' || COALESCE(t.typdefault, '')
		         WHEN 'r' THEN (SELECT format_type(r.rngsubtype, NULL) FROM pg_range r WHERE r.rngtypid = t.oid)
		       END
		  FROM pg_type t
		 WHERE t.typnamespace = $1 AND t.typtype IN ('e','d','r') AND ` + extFilter("t.oid")},
	{kind: "policy", sql: `
		SELECT c.relname, pol.polname, pol.polcmd::text, pol.polpermissive::text,
		       (SELECT string_agg(n, ',' ORDER BY n)
		          FROM (SELECT CASE WHEN r = 0 THEN 'public' ELSE pg_get_userbyid(r)::text END AS n FROM unnest(pol.polroles) r) roles),
		       pg_get_expr(pol.polqual, pol.polrelid), pg_get_expr(pol.polwithcheck, pol.polrelid)
		  FROM pg_policy pol JOIN pg_class c ON c.oid = pol.polrelid
		 WHERE c.relnamespace = $1`},
	{kind: "rule", sql: `
		SELECT c.relname, r.rulename, pg_get_ruledef(r.oid, true)
		  FROM pg_rewrite r JOIN pg_class c ON c.oid = r.ev_class
		 WHERE c.relnamespace = $1 AND r.rulename <> '_RETURN'`},
}

// schemaNormalizer replaces a schema's own name wherever the catalog prints
// it as a qualifier or as a search_path entry.
type schemaNormalizer struct {
	qualified  *regexp.Regexp
	regclass   *regexp.Regexp
	searchPath *regexp.Regexp
	entry      *regexp.Regexp
}

const schemaPlaceholder = "<schema>"

func newSchemaNormalizer(schema string) schemaNormalizer {
	q := regexp.QuoteMeta(schema)
	return schemaNormalizer{
		qualified:  regexp.MustCompile(`(^|[^A-Za-z0-9_$."])(?:"` + q + `"|` + q + `)\.`),
		regclass:   regexp.MustCompile(`'` + q + `\.`),
		searchPath: regexp.MustCompile(`(?im)search_path\s*(?:TO|=)[^\n;]*`),
		entry:      regexp.MustCompile(`(^|[^A-Za-z0-9_$])(?:'` + q + `'|"` + q + `"|` + q + `)([^A-Za-z0-9_$]|$)`),
	}
}

func (n schemaNormalizer) apply(s string) string {
	if s == "" {
		return s
	}
	s = n.qualified.ReplaceAllString(s, "${1}"+schemaPlaceholder+".")
	s = n.regclass.ReplaceAllString(s, "'"+schemaPlaceholder+".")
	return n.searchPath.ReplaceAllStringFunc(s, func(clause string) string {
		// Entries are separated by ", ", so each match consumes at most one
		// neighbour; two passes cover adjacent entries.
		for i := 0; i < 2; i++ {
			clause = n.entry.ReplaceAllString(clause, "${1}"+schemaPlaceholder+"${2}")
		}
		return clause
	})
}

// diff lists lines present in only one fingerprint: "-" for the live schema,
// "+" for the reference build.
func (f fingerprint) diff(reference fingerprint) []string {
	live := map[string]int{}
	for _, l := range f {
		live[l]++
	}
	ref := map[string]int{}
	for _, l := range reference {
		ref[l]++
	}
	var out []string
	for _, l := range f {
		if ref[l] > 0 {
			ref[l]--
			continue
		}
		out = append(out, "- "+l)
	}
	for _, l := range reference {
		if live[l] > 0 {
			live[l]--
			continue
		}
		out = append(out, "+ "+l)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i][2:] < out[j][2:] })
	return out
}

// renderDiff formats a fingerprint diff for an operator, capped so a boot
// error stays readable.
func renderDiff(diff []string, liveLabel, refLabel string) string {
	const max = 40
	var b strings.Builder
	fmt.Fprintf(&b, "    (- only in %s, + only in %s)\n", liveLabel, refLabel)
	for i, l := range diff {
		if i == max {
			fmt.Fprintf(&b, "    ... %d more\n", len(diff)-max)
			break
		}
		b.WriteString("    " + truncateLine(l, 400) + "\n")
	}
	return b.String()
}

func truncateLine(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
