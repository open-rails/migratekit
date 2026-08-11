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

COMMON FLAGS
  -dsn string       Postgres DSN (default $DATABASE_URL)
  -app string       application name, as passed to NewPostgres (required)
  -dir string       directory holding *.up.sql (default ".")
  -schema string    target schema, as passed to WithSchema
  -strict-ordering  refuse a pending migration that sorts below an applied one
  -strict-content   treat an edited applied migration as an error, not a warning

REPAIR FLAGS
  --reason string    why (required; recorded verbatim in the audit row)
  --operator string  who (optional; the OS user is recorded regardless)
  --dry-run          compute and print the repair, write nothing
  --all-unmatched    adopt every mismatched row (repair adopt only)

APPLY FLAGS
  --allow-below-applied <number>   one-shot ordering exception for a genuine
                                   backport; repeatable; requires --reason

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
	strictContent  bool

	reason       string
	operator     string
	dryRun       bool
	allUnmatched bool
	allowBelow   multiFlag
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
			return fmt.Errorf("repair needs a verb: adopt or accept-content")
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
	fs.BoolVar(&o.strictContent, "strict-content", false, "treat an edited applied migration as an error")
	fs.StringVar(&o.reason, "reason", "", "why this repair is being made")
	fs.StringVar(&o.operator, "operator", "", "who is making this repair")
	fs.BoolVar(&o.dryRun, "dry-run", false, "compute the repair, write nothing")
	fs.BoolVar(&o.allUnmatched, "all-unmatched", false, "adopt every mismatched ledger row")
	fs.Var(&o.allowBelow, "allow-below-applied", "exempt this migration from the ordering rule (repeatable)")

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
	if o.strictContent {
		m = m.WithStrictContent()
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
	}

	return fmt.Errorf("unknown command %q; run `migratekit help`", command)
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
