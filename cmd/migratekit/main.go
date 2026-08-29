// Command migratekit is the operator's side of migration identity: it
// explains a boot refusal and resolves it.
//
// The library refuses to apply a chain whose ledger disagrees with the files,
// which is right — the alternative is a migration that silently never runs.
// But a refusal with no resolution path is a headache: the operator is left
// with psql and a hand-written UPDATE against public.migrations. This command
// is the missing half.
//
//	migratekit status  -app tensorhub -dir migrations/postgres
//	migratekit repair adopt 42 --reason "restored 2026-08-10 backup"
//	migratekit repair accept-content 7 --reason "comment typo, DDL unchanged"
//	migratekit apply --allow-below-applied 91 --reason "backport of th#1712"
//	migratekit history -app tensorhub
//
// Every repair writes an audit row (who, when, verb, reason, old and new
// identity) and touches only the ledger's identity columns — never schema.
package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/open-rails/migratekit"
)

const usage = `migratekit — migration ledger status and repair

USAGE
  migratekit <command> [flags]

COMMANDS
  status                        what is applied, what is pending, and every discrepancy
                                with its likely cause and resolution
  apply                         apply pending migrations
  history                       the repair audit trail for this app
  repair adopt <number>         bind the file in the tree as the applied identity for
                                <number> — for a restored or adopted database whose
                                ledger is the stale side
  repair adopt --all-unmatched  the same, for EVERY row that disagrees (the usual shape
                                of a restore)
  repair accept-content <number>
                                re-stamp the digest after a verified, cosmetic edit —
                                acknowledges and silences the content-drift warning
  repair resolve <number>       clear a no-transaction migration left failed or running:
                                --applied (you finished it) or --rerun (drop the row so
                                the next boot runs it again — idempotent migrations only)

DATABASE-FREE COMMANDS (no -dsn, no -app; safe and intended for CI)
  check                         lint the directory: numbering, parent links, and the
                                repair-totality rule
  relink                        rewrite each parent line to the file that actually
                                precedes it — the one-command fix after a renumber

COMMON FLAGS
  -dsn string       Postgres DSN (default $DATABASE_URL)
  -app string       application name, as passed to NewPostgres (required)
  -dir string       directory holding *.up.sql (default ".")
  -schema string    target schema, as passed to WithSchema
  -strict-ordering  refuse a pending migration that sorts below an applied one

REPAIR FLAGS
  --reason string    why (required; recorded verbatim in the audit row)
  --operator string  who (optional; the OS user is recorded regardless)
  --dry-run          compute and print the repair, write nothing
  --all-unmatched    adopt every mismatched row (repair adopt only)

APPLY FLAGS
  --allow-below-applied <number>   one-shot ordering exception for a genuine
                                   backport; repeatable; requires --reason

RESOLVE FLAGS
  --applied   the DDL landed; mark the row applied
  --rerun     the DDL did not land (or you undid it); drop the row

CHECK / RELINK FLAGS
  --require-links   a migration with no "-- parent:" line is an error (check)
  --check           report what relink WOULD rewrite and exit 1 if anything (relink)
  --from <number>   relink only migrations at or above this number

Repairs refuse to run in CI. They rewrite one database's ledger after a human
has read the diff; a pipeline that needs one has a chain problem to fix in the
repository instead.

EXIT CODES
  0  clean
  1  the command failed
  2  status found error-severity discrepancies (warnings alone still exit 0)
`

type opts struct {
	dsn            string
	app            string
	dir            string
	schema         string
	strictOrdering bool

	reason       string
	operator     string
	dryRun       bool
	allUnmatched bool
	allowBelow   multiFlag

	resolveApplied bool
	resolveRerun   bool
	requireLinks   bool
	checkOnly      bool
	from           string
}

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

func main() {
	if err := run(os.Args[1:]); err != nil {
		var ec exitCode
		if errors.As(err, &ec) {
			os.Exit(int(ec))
		}
		msg := err.Error()
		if !strings.HasPrefix(msg, "migratekit") {
			msg = "migratekit: " + msg
		}
		fmt.Fprintln(os.Stderr, msg)
		os.Exit(1)
	}
}

type exitCode int

func (e exitCode) Error() string { return fmt.Sprintf("exit %d", int(e)) }

func run(args []string) error {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		fmt.Print(usage)
		return nil
	}

	command := args[0]
	rest := args[1:]
	// `repair adopt` / `repair accept-content` are two words.
	if command == "repair" {
		if len(rest) == 0 {
			return fmt.Errorf("repair needs a verb: adopt, accept-content or resolve")
		}
		command = "repair " + rest[0]
		rest = rest[1:]
	}

	var o opts
	fs := flag.NewFlagSet(command, flag.ContinueOnError)
	fs.Usage = func() { fmt.Print(usage) }
	fs.StringVar(&o.dsn, "dsn", os.Getenv("DATABASE_URL"), "Postgres DSN")
	fs.StringVar(&o.app, "app", "", "application name")
	fs.StringVar(&o.dir, "dir", ".", "directory holding *.up.sql")
	fs.StringVar(&o.schema, "schema", "", "target schema")
	fs.BoolVar(&o.strictOrdering, "strict-ordering", false, "refuse a pending migration below an applied one")
	fs.StringVar(&o.reason, "reason", "", "why this repair is being made")
	fs.StringVar(&o.operator, "operator", "", "who is making this repair")
	fs.BoolVar(&o.dryRun, "dry-run", false, "compute the repair, write nothing")
	fs.BoolVar(&o.allUnmatched, "all-unmatched", false, "adopt every mismatched ledger row")
	fs.Var(&o.allowBelow, "allow-below-applied", "exempt this migration from the ordering rule (repeatable)")
	fs.BoolVar(&o.resolveApplied, "applied", false, "resolve a dirty migration as applied")
	fs.BoolVar(&o.resolveRerun, "rerun", false, "resolve a dirty migration by dropping its ledger row")
	fs.BoolVar(&o.requireLinks, "require-links", false, "a migration with no parent link is an error")
	fs.BoolVar(&o.checkOnly, "check", false, "report what relink would rewrite; write nothing")
	fs.StringVar(&o.from, "from", "", "relink only migrations at or above this number")

	// Positional arguments may precede flags (`repair adopt 42 --reason x`).
	var positional []string
	for len(rest) > 0 {
		if strings.HasPrefix(rest[0], "-") {
			if err := fs.Parse(rest); err != nil {
				return err
			}
			rest = fs.Args()
			continue
		}
		positional = append(positional, rest[0])
		rest = rest[1:]
	}

	// `check` and `relink` read and write FILES. They take no database, which
	// is what makes them the CI gate and the authoring fix respectively.
	switch command {
	case "check":
		return runCheck(o)
	case "relink":
		return runRelink(o)
	}

	if o.app == "" {
		return fmt.Errorf("-app is required (the name passed to NewPostgres)")
	}
	if o.dsn == "" {
		return fmt.Errorf("-dsn is required (or set DATABASE_URL)")
	}

	ctx := context.Background()
	migrations, err := migratekit.LoadFromFS(os.DirFS(o.dir))
	if err != nil {
		return fmt.Errorf("load %s: %w", o.dir, err)
	}

	db, err := sql.Open("pgx", o.dsn)
	if err != nil {
		return err
	}
	defer db.Close()

	m := migratekit.NewPostgres(db, o.app)
	if o.schema != "" {
		m = m.WithSchema(o.schema)
	}
	if o.strictOrdering {
		m = m.WithStrictOrdering()
	}

	req := migratekit.RepairRequest{Reason: o.reason, Operator: o.operator, DryRun: o.dryRun}

	switch command {
	case "status":
		st, err := m.Status(ctx, migrations)
		if err != nil {
			return err
		}
		fmt.Print(st.Report())
		if st.HasErrors() {
			return exitCode(2)
		}
		return nil

	case "history":
		records, err := m.RepairHistory(ctx)
		if err != nil {
			return err
		}
		if len(records) == 0 {
			fmt.Printf("no repairs recorded for app %s\n", o.app)
			return nil
		}
		for _, r := range records {
			fmt.Println(r.Line())
		}
		return nil

	case "apply":
		if len(o.allowBelow) > 0 {
			if err := m.ApplyWithOrderingException(ctx, migrations, o.allowBelow, req); err != nil {
				return err
			}
			fmt.Printf("applied with an ordering exception for %s (recorded in the audit trail)\n", o.allowBelow.String())
			return nil
		}
		if o.dryRun {
			return fmt.Errorf("--dry-run on a plain apply is `migratekit status`; use that")
		}
		if err := m.ApplyMigrations(ctx, migrations); err != nil {
			return err
		}
		fmt.Printf("%s: chain applied\n", o.app)
		return nil

	case "repair adopt":
		if o.allUnmatched {
			results, err := m.RepairAdoptAllUnmatched(ctx, migrations, req)
			if err != nil {
				return err
			}
			for _, r := range results {
				fmt.Println(r.String())
			}
			fmt.Printf("%d ledger row(s) adopted\n", len(results))
			return nil
		}
		mig, err := pick(migrations, positional, "repair adopt")
		if err != nil {
			return err
		}
		res, err := m.RepairAdopt(ctx, mig, req)
		if err != nil {
			return err
		}
		fmt.Println(res.String())
		return nil

	case "repair accept-content":
		mig, err := pick(migrations, positional, "repair accept-content")
		if err != nil {
			return err
		}
		res, err := m.RepairAcceptContent(ctx, mig, req)
		if err != nil {
			return err
		}
		fmt.Println(res.String())
		return nil

	case "repair resolve":
		mig, err := pick(migrations, positional, "repair resolve")
		if err != nil {
			return err
		}
		if o.resolveApplied == o.resolveRerun {
			return fmt.Errorf("repair resolve needs exactly one of --applied or --rerun.\n" +
				"  --applied: the DDL landed and only the ledger is behind.\n" +
				"  --rerun:   it did not land (or you undid the partial work), so the next boot should run it again")
		}
		mode := migratekit.ResolveApplied
		if o.resolveRerun {
			mode = migratekit.ResolveRerun
		}
		res, err := m.RepairResolve(ctx, mig, mode, req)
		if err != nil {
			return err
		}
		fmt.Println(res.String())
		return nil
	}

	return fmt.Errorf("unknown command %q; run `migratekit help`", command)
}

// runCheck is the merge-boundary gate: numbering, parent links and the
// repair-totality rule, all from file content alone.
func runCheck(o opts) error {
	fsys := os.DirFS(o.dir)
	var failures []string
	if err := migratekit.CheckChainFS(fsys, ".", o.requireLinks); err != nil {
		failures = append(failures, err.Error())
	}
	if err := migratekit.CheckRepairTotality(fsys, "."); err != nil {
		failures = append(failures, err.Error())
	}
	if len(failures) > 0 {
		return fmt.Errorf("migratekit check failed in %s:\n\n%s", o.dir, strings.Join(failures, "\n\n"))
	}
	fmt.Printf("migratekit check: %s is clean\n", o.dir)
	return nil
}

// runRelink rewrites parent lines. It is an AUTHORING verb: no reason, no audit
// row and no CI refusal, because it changes files (which git already records)
// rather than a ledger. `--check` is the CI form.
func runRelink(o opts) error {
	changes, err := migratekit.Relink(o.dir, migratekit.RelinkOptions{
		DryRun: o.checkOnly || o.dryRun,
		From:   o.from,
	})
	if err != nil {
		return err
	}
	if len(changes) == 0 {
		fmt.Printf("migratekit relink: %s is already linked\n", o.dir)
		return nil
	}
	for _, c := range changes {
		from := c.From
		if from == "" {
			from = "(no parent line)"
		}
		fmt.Printf("%s\n  - %s\n  + %s\n", c.File, from, c.To)
	}
	if o.checkOnly {
		return fmt.Errorf("%d parent line(s) are stale; run `migratekit relink -dir %s`", len(changes), o.dir)
	}
	if o.dryRun {
		fmt.Printf("[dry-run] %d parent line(s) would be rewritten\n", len(changes))
		return nil
	}
	fmt.Printf("%d parent line(s) rewritten\n", len(changes))
	return nil
}

// pick resolves a positional migration number (or filename) to the file in the
// tree that carries it.
func pick(migrations []migratekit.Migration, positional []string, verb string) (migratekit.Migration, error) {
	if len(positional) != 1 {
		return migratekit.Migration{}, fmt.Errorf("%s takes exactly one migration number, e.g. `%s 42`", verb, verb)
	}
	key := migratekit.Prefix(positional[0])
	for _, m := range migrations {
		if migratekit.Prefix(m.Name) == key {
			return m, nil
		}
	}
	return migratekit.Migration{}, fmt.Errorf("no migration numbered %q in this directory — %s binds the ledger to a FILE, so the file has to be here", key, verb)
}
