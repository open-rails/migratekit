package migratekit

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"os/user"
	"strings"
	"time"
)

// Repairs rewrite the ledger's IDENTITY columns — filename and content digest —
// and nothing else. They never touch a schema, never run DDL, and never mark
// an unapplied migration applied. Every one of them writes an audit row, so a
// repaired ledger is visible history rather than a ledger that quietly agrees
// with whatever the tree says today.
//
// They are also operator-runtime verbs. A pipeline that needs one is
// mis-designed: the fix for a chain that every database will trip over is a
// change to the chain, in the repository. So the verbs refuse to run when they
// detect CI.

// ErrRepairInCI is returned when a repair verb is invoked in CI.
var ErrRepairInCI = errors.New("migratekit: repairs are operator-runtime verbs and refuse to run in CI")

// ErrNothingToRepair is returned when the ledger already matches the tree.
var ErrNothingToRepair = errors.New("migratekit: nothing to repair")

// ciEnvVars are set by the CI systems we know about. Any one of them is
// enough to refuse.
var ciEnvVars = []string{
	"CI",
	"CONTINUOUS_INTEGRATION",
	"GITHUB_ACTIONS",
	"GITLAB_CI",
	"CIRCLECI",
	"TRAVIS",
	"BUILDKITE",
	"DRONE",
	"TEAMCITY_VERSION",
	"TF_BUILD",
	"JENKINS_URL",
	"HUDSON_URL",
	"BITBUCKET_BUILD_NUMBER",
	"CODEBUILD_BUILD_ID",
	"APPVEYOR",
	"SEMAPHORE",
	"WOODPECKER_CI",
}

// DetectCI reports the name of the CI environment variable that is set, if
// any. An empty-string value does not count — some shells export CI="".
func DetectCI() (string, bool) {
	for _, name := range ciEnvVars {
		if v, ok := os.LookupEnv(name); ok && v != "" && !strings.EqualFold(v, "false") && v != "0" {
			return name, true
		}
	}
	return "", false
}

func refuseInCI(verb string) error {
	name, inCI := DetectCI()
	if !inCI {
		return nil
	}
	return fmt.Errorf("%w: `%s` was invoked with %s set.\n"+
		"  A repair rewrites ONE database's ledger after a human has looked at the diff. A pipeline\n"+
		"  cannot look, and a repair that runs on every deploy hides the thing it was meant to reveal.\n"+
		"  If every database trips over this, the chain is wrong: fix it in the repository (renumber the\n"+
		"  file, or revert the edit) so no database needs a repair.",
		ErrRepairInCI, verb, name)
}

// RepairRequest is the operator's authorization for a repair.
type RepairRequest struct {
	// Reason is required and recorded verbatim. "Why" is the only part of a
	// repair a future reader cannot reconstruct.
	Reason string
	// Operator optionally names the human, alongside the OS user the audit
	// row records anyway.
	Operator string
	// DryRun computes and returns the repair without writing anything.
	DryRun bool
}

func (r RepairRequest) validate(verb string) error {
	if strings.TrimSpace(r.Reason) == "" {
		return fmt.Errorf("migratekit: `%s` requires a --reason; a ledger repair with no recorded reason is indistinguishable from tampering", verb)
	}
	return refuseInCI(verb)
}

// RepairResult describes one ledger identity row before and after a repair.
type RepairResult struct {
	Verb        string
	Key         string
	OldFilename string
	OldDigest   string
	NewFilename string
	NewDigest   string
	DryRun      bool
}

func (r RepairResult) String() string {
	prefix := ""
	if r.DryRun {
		prefix = "[dry-run] "
	}
	return fmt.Sprintf("%s%s key %s: filename %s -> %s, digest %s -> %s",
		prefix, r.Verb, r.Key,
		orNone(r.OldFilename), orNone(r.NewFilename),
		orNone(shortDigest(r.OldDigest)), orNone(shortDigest(r.NewDigest)))
}

func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}

// RepairRecord is one row of the repair audit trail.
type RepairRecord struct {
	ID          int64
	App         string
	Schema      string
	Key         string
	Verb        string
	Reason      string
	Operator    string
	OSUser      string
	Host        string
	OldFilename string
	OldDigest   string
	NewFilename string
	NewDigest   string
	At          time.Time
}

// Line renders one audit row for a terminal.
func (r RepairRecord) Line() string {
	who := r.OSUser
	if r.Operator != "" {
		who = r.Operator + " (" + r.OSUser + ")"
	}
	return fmt.Sprintf("%s  %-30s key %-4s  %s -> %s  [%s@%s]  %q",
		r.At.UTC().Format(time.RFC3339), r.Verb, r.Key,
		identity(r.OldFilename, r.OldDigest), identity(r.NewFilename, r.NewDigest),
		who, r.Host, r.Reason)
}

// identity renders one side of a repair: the filename and the digest are two
// halves of one fact, and a repair that only moved the digest is unreadable
// without both.
func identity(filename, digest string) string {
	switch {
	case filename == "" && digest == "":
		return "(none)"
	case filename == "":
		return "?@" + shortDigest(digest)
	case digest == "":
		return filename + "@?"
	}
	return filename + "@" + shortDigest(digest)
}

// RepairAdopt binds the file in the tree as the applied identity for its
// ledger key: filename and content digest both become this file's.
//
// This is the verb for a ledger that is the WRONG SIDE — a restored backup, a
// database adopted from somewhere else, a row somebody fixed by hand. The
// files are right and the ledger remembers a tree that no longer exists. It is
// NOT the verb for a lane collision: if two files genuinely claim one number,
// adopting one of them silently drops the other's DDL, which is exactly the
// failure this package exists to prevent. Read the diff first.
func (p *Postgres) RepairAdopt(ctx context.Context, m Migration, req RepairRequest) (RepairResult, error) {
	if err := req.validate("repair adopt"); err != nil {
		return RepairResult{}, err
	}
	if err := p.Setup(ctx); err != nil {
		return RepairResult{}, err
	}
	applied, err := p.AppliedRecords(ctx)
	if err != nil {
		return RepairResult{}, err
	}
	key := Prefix(m.Name)
	rec, ok := applied[key]
	if !ok {
		return RepairResult{}, fmt.Errorf("migratekit: key %q is not in the ledger for app %q — there is nothing to adopt. A migration that never ran is applied, not repaired", key, p.app)
	}
	digest := ContentDigest(m.Content)
	if rec.Filename == m.Name && rec.Digest == digest {
		return RepairResult{}, fmt.Errorf("%w: key %q already records %s at digest %s", ErrNothingToRepair, key, m.Name, shortDigest(digest))
	}
	return p.writeRepair(ctx, "repair adopt", rec, m.Name, digest, SemanticContentDigest(m.Content), req)
}

// RepairAdoptAllUnmatched adopts EVERY ledger row whose identity disagrees
// with the file in the tree that carries its number. This is the restored-
// backup shape: one dump, one restore, and every row mismatches at once.
// Rows that already match, and rows with no file behind them, are left alone.
func (p *Postgres) RepairAdoptAllUnmatched(ctx context.Context, migrations []Migration, req RepairRequest) ([]RepairResult, error) {
	if err := req.validate("repair adopt --all-unmatched"); err != nil {
		return nil, err
	}
	if err := p.Setup(ctx); err != nil {
		return nil, err
	}
	applied, err := p.AppliedRecords(ctx)
	if err != nil {
		return nil, err
	}

	var results []RepairResult
	for _, m := range migrations {
		rec, ok := applied[Prefix(m.Name)]
		if !ok {
			continue
		}
		digest := ContentDigest(m.Content)
		if rec.Filename == m.Name && rec.Digest == digest {
			continue
		}
		res, err := p.writeRepair(ctx, "repair adopt --all-unmatched", rec, m.Name, digest, SemanticContentDigest(m.Content), req)
		if err != nil {
			return results, err
		}
		results = append(results, res)
	}
	if len(results) == 0 {
		return nil, fmt.Errorf("%w: every ledger row already matches the file that carries its number", ErrNothingToRepair)
	}
	return results, nil
}

// RepairAcceptContent re-stamps the recorded digest of an applied migration
// after a verified edit. It is the "I looked at the diff and it is cosmetic"
// verb: it acknowledges the content-drift warning and silences it, on the
// record. It refuses when the ledger names a DIFFERENT file, because that is
// an identity problem and `repair adopt` is the verb that says so.
func (p *Postgres) RepairAcceptContent(ctx context.Context, m Migration, req RepairRequest) (RepairResult, error) {
	if err := req.validate("repair accept-content"); err != nil {
		return RepairResult{}, err
	}
	if err := p.Setup(ctx); err != nil {
		return RepairResult{}, err
	}
	applied, err := p.AppliedRecords(ctx)
	if err != nil {
		return RepairResult{}, err
	}
	key := Prefix(m.Name)
	rec, ok := applied[key]
	if !ok {
		return RepairResult{}, fmt.Errorf("migratekit: key %q is not in the ledger for app %q — nothing has been applied to accept", key, p.app)
	}
	if rec.Filename != "" && rec.Filename != m.Name {
		return RepairResult{}, fmt.Errorf(
			"migratekit: key %q was applied by %s, not %s — that is an IDENTITY mismatch, not content drift.\n"+
				"  Accepting the content here would bind the ledger to a file it never ran. Use `repair adopt` if this\n"+
				"  database's ledger is the stale side, or renumber %s if two lanes claimed the number.",
			key, rec.Filename, m.Name, m.Name)
	}
	digest := ContentDigest(m.Content)
	if rec.Digest == digest {
		return RepairResult{}, fmt.Errorf("%w: key %q already records digest %s", ErrNothingToRepair, key, shortDigest(digest))
	}
	return p.writeRepair(ctx, "repair accept-content", rec, m.Name, digest, SemanticContentDigest(m.Content), req)
}

// writeRepair updates one ledger identity row and records the audit row in the
// SAME transaction. A repair that lands without its audit row, or an audit row
// for a repair that did not land, would both be worse than no repair at all.
func (p *Postgres) writeRepair(ctx context.Context, verb string, old AppliedRecord,
	filename, digest, semantic string, req RepairRequest,
) (RepairResult, error) {
	res := RepairResult{
		Verb:        verb,
		Key:         old.Key,
		OldFilename: old.Filename,
		OldDigest:   old.Digest,
		NewFilename: filename,
		NewDigest:   digest,
		DryRun:      req.DryRun,
	}
	if req.DryRun {
		return res, nil
	}

	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return res, err
	}
	defer tx.Rollback()

	tag, err := tx.ExecContext(ctx,
		`UPDATE public.migrations SET filename = $1, content_sha256 = $2, semantic_sha256 = $3
		  WHERE app = $4 AND database = $5 AND schema = $6 AND name = $7`,
		filename, digest, semantic, p.app, postgresDriver, p.schema, old.Key)
	if err != nil {
		return res, err
	}
	if n, err := tag.RowsAffected(); err == nil && n != 1 {
		return res, fmt.Errorf("migratekit: repair updated %d ledger rows for key %q, want exactly 1", n, old.Key)
	}
	if err := p.insertAudit(ctx, tx, verb, old.Key, old.Filename, old.Digest, filename, digest, req); err != nil {
		return res, err
	}
	return res, tx.Commit()
}

func (p *Postgres) insertAudit(ctx context.Context, tx *sql.Tx, verb, key, oldFile, oldDigest, newFile, newDigest string, req RepairRequest) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO public.migration_repairs
		   (app, database, schema, name, verb, reason, operator, os_user, host,
		    old_filename, old_digest, new_filename, new_digest)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`,
		p.app, postgresDriver, p.schema, key, verb, strings.TrimSpace(req.Reason),
		req.Operator, osUser(), hostname(),
		nullable(oldFile), nullable(oldDigest), nullable(newFile), nullable(newDigest))
	return err
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// RepairHistory returns this app's repair audit trail, newest first.
func (p *Postgres) RepairHistory(ctx context.Context) ([]RepairRecord, error) {
	if err := p.Setup(ctx); err != nil {
		return nil, err
	}
	rows, err := p.db.QueryContext(ctx,
		`SELECT id, name, verb, reason, COALESCE(operator,''), COALESCE(os_user,''), COALESCE(host,''),
		        COALESCE(old_filename,''), COALESCE(old_digest,''), COALESCE(new_filename,''), COALESCE(new_digest,''),
		        repaired_at
		   FROM public.migration_repairs
		  WHERE app = $1 AND database = $2 AND schema = $3
		  ORDER BY repaired_at DESC, id DESC`,
		p.app, postgresDriver, p.schema)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []RepairRecord
	for rows.Next() {
		r := RepairRecord{App: p.app, Schema: p.schema}
		if err := rows.Scan(&r.ID, &r.Key, &r.Verb, &r.Reason, &r.Operator, &r.OSUser, &r.Host,
			&r.OldFilename, &r.OldDigest, &r.NewFilename, &r.NewDigest, &r.At); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ApplyWithOrderingException applies the chain while exempting the named
// migrations from the strict-ordering rule — the one-shot escape for a genuine
// backport that must land below the high-water mark. allowBelow entries may be
// bare numbers or filenames. Every exemption that actually applies a migration
// records an audit row.
//
// The identity checks are NOT relaxed: this exempts ordering only.
func (p *Postgres) ApplyWithOrderingException(ctx context.Context, migrations []Migration, allowBelow []string, req RepairRequest) error {
	if len(allowBelow) == 0 {
		return fmt.Errorf("migratekit: ApplyWithOrderingException needs at least one migration to exempt; plain ApplyMigrations is the unexceptional path")
	}
	if err := req.validate("apply --allow-below-applied"); err != nil {
		return err
	}
	exempt := map[string]bool{}
	for _, a := range allowBelow {
		exempt[Prefix(a)] = true
	}
	if req.DryRun {
		if err := p.Setup(ctx); err != nil {
			return err
		}
		records, err := p.AppliedRecords(ctx)
		if err != nil {
			return err
		}
		return firstError(analyze(migrations, records, checkOptions{
			strictOrdering: p.strictOrdering,
			allowBelow:     exempt,
		}))
	}
	return p.applyMigrations(ctx, migrations, exempt, &req)
}

func osUser() string {
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	for _, k := range []string{"USER", "LOGNAME", "USERNAME"} {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return "unknown"
}

func hostname() string {
	if h, err := os.Hostname(); err == nil && h != "" {
		return h
	}
	return "unknown"
}
