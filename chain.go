package migratekit

import (
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// A migration's number is claimed when a lane BRANCHES, and nothing in the
// FILES records what it branched from. v1.5.0 made a collision detectable, but
// only against a live ledger and only after one of the two lanes had already
// deployed: two lanes that each write 0006 merge into one green tree, and the
// second one finds out from a boot refusal in production.
//
// So every migration carries a parent link as its first non-blank line:
//
//	-- parent: 5 sha256:9f2c...e1
//	-- parent: root                 (the first migration of the chain)
//
// Verified at LOAD — pure file reading, no database, no configuration. Boot and
// CI both inherit it, because both go through here. There is deliberately no
// second verification point to keep in sync: the chain is a property of the
// FILES.
//
// Two things follow. An ordering conflict becomes structurally impossible: the
// lane that merges second has a parent line pointing at a file that is no
// longer its predecessor, which is a deterministic refusal in its own PR. And
// the directory becomes a hash chain — tampering with history breaks every
// later link, which is atlas.sum's property with no sum file to maintain.
//
// THE DIGEST EXCLUDES THE PARENT LINE. The canonical body of a migration is the
// file with its own header removed, and that is what both this chain and
// public.migrations.content_sha256 hash. Adding a parent line to an
// already-applied migration therefore changes no ledger digest — adoption is
// silent, every digest v1.5.0 ever wrote is still correct, and there is nothing
// to backfill. Tampering with a parent's HEADER is caught by that file's own
// link to ITS parent.

// ParentLink is the parsed `-- parent:` header of a migration.
type ParentLink struct {
	// Present is false for a headerless (legacy) migration.
	Present bool
	// IsRoot is true for `-- parent: root`.
	IsRoot bool
	// Parent is the normalized ledger key of the parent migration.
	Parent string
	// Digest is the claimed sha256 of the parent's canonical body.
	Digest string
}

const parentHeaderForm = `"-- parent: <number> sha256:<64 hex digits>" or "-- parent: root"`

var (
	reParentHeader = regexp.MustCompile(`^[ \t]*--[ \t]*parent:[ \t]*(.*?)[ \t]*$`)
	reParentValue  = regexp.MustCompile(`^([0-9]+)[ \t]+sha256:([0-9a-f]{64})$`)
)

// firstContentLine returns the first non-blank line and the byte range it
// occupies, including its trailing newline.
func firstContentLine(content string) (line string, start, end int, ok bool) {
	off := 0
	for off < len(content) {
		nl := strings.IndexByte(content[off:], '\n')
		lineEnd := len(content)
		next := len(content)
		if nl >= 0 {
			lineEnd = off + nl
			next = off + nl + 1
		}
		if strings.TrimSpace(content[off:lineEnd]) != "" {
			return content[off:lineEnd], off, next, true
		}
		off = next
	}
	return "", 0, 0, false
}

// parseParentLink reads the header. A headerless migration is not an error
// here — legacy tolerance is a policy decision made by the caller.
func parseParentLink(content string) (ParentLink, error) {
	line, _, _, ok := firstContentLine(content)
	if !ok {
		return ParentLink{}, nil
	}
	m := reParentHeader.FindStringSubmatch(line)
	if m == nil {
		return ParentLink{}, nil
	}
	value := m[1]
	if value == "root" {
		return ParentLink{Present: true, IsRoot: true}, nil
	}
	v := reParentValue.FindStringSubmatch(value)
	if v == nil {
		return ParentLink{}, fmt.Errorf("malformed parent header %q; expected %s", line, parentHeaderForm)
	}
	return ParentLink{Present: true, Parent: normalizeNumber(v[1]), Digest: v[2]}, nil
}

// normalizeNumber strips leading zeros the same way Prefix does, so a header
// may be written 0004 or 4.
func normalizeNumber(s string) string {
	n := strings.TrimLeft(s, "0")
	if n == "" {
		return "0"
	}
	return n
}

// canonicalBody is the migration without its own parent header — what both the
// chain and the ledger hash. Only the FIRST non-blank line counts as a header;
// a `-- parent:` further down is ordinary prose and stays in the body.
func canonicalBody(content string) string {
	line, start, end, ok := firstContentLine(content)
	if !ok || !reParentHeader.MatchString(line) {
		return content
	}
	return content[:start] + content[end:]
}

// LoadOption configures Load.
type LoadOption func(*loadConfig)

type loadConfig struct {
	require bool
	warn    func(string)
}

// RequireParentLinks makes a headerless migration an error instead of a
// warning. Off by default for one minor version so existing consumers keep
// booting; adopt it once your chain carries headers.
func RequireParentLinks() LoadOption { return func(c *loadConfig) { c.require = true } }

// WithChainWarnFunc redirects legacy-tolerance warnings. Defaults to the standard
// logger.
func WithChainWarnFunc(fn func(string)) LoadOption {
	return func(c *loadConfig) {
		if fn != nil {
			c.warn = fn
		}
	}
}

func newLoadConfig(opts []LoadOption) loadConfig {
	cfg := loadConfig{warn: func(s string) { log.Println("migratekit:", s) }}
	for _, o := range opts {
		o(&cfg)
	}
	return cfg
}

// Load reads migrations and verifies the parent-link chain. LoadFromFS is this
// with default options.
func Load(fsys fs.FS, dir string, opts ...LoadOption) ([]Migration, error) {
	migrations, err := loadFiles(fsys, dir)
	if err != nil {
		return nil, err
	}
	if err := VerifyChain(migrations, opts...); err != nil {
		return nil, err
	}
	return migrations, nil
}

// VerifyChain validates the parent links of migrations, which must be in apply
// order (as returned by loadFiles). It reports the FIRST violation, naming both
// files involved, because a chain error is always about a relationship.
func VerifyChain(migrations []Migration, opts ...LoadOption) error {
	cfg := newLoadConfig(opts)

	links := make([]ParentLink, len(migrations))
	index := make(map[string]int, len(migrations))
	for i, m := range migrations {
		link, err := parseParentLink(m.Content)
		if err != nil {
			return fmt.Errorf("migration %s: %w", m.Name, err)
		}
		links[i] = link
		index[Prefix(m.Name)] = i
	}

	// Legacy tolerance. A headerless file takes no part in the root and fork
	// checks, but its BYTES are still hashable, so a headed child verifies
	// against it — which is how a repo adopts incrementally.
	for i, m := range migrations {
		if links[i].Present {
			continue
		}
		if cfg.require {
			return fmt.Errorf(
				"migration %s carries no parent link.\n"+
					"  Add %s as its first line, or run `migratekit relink`.",
				m.Name, parentHeaderForm)
		}
		cfg.warn(fmt.Sprintf("migration %s carries no parent link; it is excluded from chain verification. Run `migratekit relink` to adopt.", m.Name))
	}

	// Exactly one root, and it must be the lowest-numbered file. A squash needs
	// no special case: it DELETES the files it replaces, so the squash file is
	// the lowest-numbered file present and legitimately claims root.
	root := -1
	for i := range migrations {
		if !links[i].IsRoot {
			continue
		}
		if root >= 0 {
			return fmt.Errorf(
				"migrations %s and %s both claim `-- parent: root`.\n"+
					"  A chain has exactly one root. If %s is a squash, delete the files it replaces; otherwise link it to its predecessor.",
				migrations[root].Name, migrations[i].Name, migrations[i].Name)
		}
		root = i
	}
	if root > 0 {
		return fmt.Errorf(
			"migration %s claims `-- parent: root`, but %s sorts before it.\n"+
				"  The root is the lowest-numbered migration present. Link %s to its predecessor, or (if this is a squash) delete the migrations it replaces.",
			migrations[root].Name, migrations[0].Name, migrations[root].Name)
	}
	if cfg.require && root < 0 && len(migrations) > 0 {
		return fmt.Errorf(
			"no migration claims `-- parent: root`; %s is the lowest-numbered one and must.",
			migrations[0].Name)
	}

	// Two files claiming the same parent is a fork — the shape a lane that
	// merged without rebasing its parent line leaves behind.
	claimed := map[string]int{}
	for i := range migrations {
		if !links[i].Present || links[i].IsRoot {
			continue
		}
		if prior, dup := claimed[links[i].Parent]; dup {
			return fmt.Errorf(
				"migrations %s and %s both claim parent %q — the chain forks.\n"+
					"  One of them branched before the other merged; rebase its parent line (`migratekit relink`).",
				migrations[prior].Name, migrations[i].Name, links[i].Parent)
		}
		claimed[links[i].Parent] = i
	}

	for i, m := range migrations {
		link := links[i]
		if !link.Present || link.IsRoot {
			continue
		}
		j, ok := index[link.Parent]
		if !ok {
			return fmt.Errorf(
				"migration %s claims parent %q, but no migration numbered %s is present.\n"+
					"  Its parent was renumbered or removed; rebase the parent line (`migratekit relink`).",
				m.Name, link.Parent, link.Parent)
		}
		if j >= i {
			return fmt.Errorf(
				"migration %s claims parent %q (%s), which does NOT sort before it.\n"+
					"  A parent link points backwards; %s cannot descend from a migration that applies after it.",
				m.Name, link.Parent, migrations[j].Name, m.Name)
		}
		if j != i-1 {
			return fmt.Errorf(
				"migration %s claims parent %q (%s), but %s is what immediately precedes it.\n"+
					"  This is the stale link a lane leaves when it renumbers without rebasing: renumbering is `git mv` PLUS updating the parent line. Run `migratekit relink`.",
				m.Name, link.Parent, migrations[j].Name, migrations[i-1].Name)
		}
		if actual := ContentDigest(migrations[j].Content); actual != link.Digest {
			return fmt.Errorf(
				"migration %s claims parent %s sha256:%s, but %s hashes to sha256:%s.\n"+
					"  Either %s was edited after %s was written, or %s branched from a different %s. Every later link breaks with it.",
				m.Name, link.Parent, link.Digest[:12], migrations[j].Name, actual[:12],
				migrations[j].Name, m.Name, m.Name, link.Parent)
		}
	}
	return nil
}

// CheckChainFS is the CI gate on a migration directory: the numbering rules of
// CheckChain plus parent-link validation, which needs the bytes and not just
// the names. requireLinks refuses a headerless migration.
func CheckChainFS(fsys fs.FS, dir string, requireLinks bool) error {
	migrations, err := loadFiles(fsys, dir)
	if err != nil {
		return err
	}
	names := make([]string, 0, len(migrations))
	for _, m := range migrations {
		names = append(names, m.Name)
	}
	if err := CheckChain(names); err != nil {
		return err
	}
	opts := []LoadOption{}
	if requireLinks {
		opts = append(opts, RequireParentLinks())
	}
	return VerifyChain(migrations, opts...)
}

// --- relink -----------------------------------------------------------------
//
// relink is an AUTHORING verb, and deliberately does NOT carry the guardrails
// the repair verbs do — no --reason, no audit row, no CI refusal. The repair
// verbs mutate a production LEDGER: state that is invisible, shared, and has no
// history of its own. relink mutates FILES in a working tree, and files already
// have an audit log — git. A --reason would duplicate the commit message, and an
// audit table cannot record a change that may never be committed. Refusing to
// run in CI would be actively wrong, because `relink --check` IS the CI gate.
//
// It will happily bless a tampered history, because recomputing is all it does.
// That is correct for an authoring tool, and is exactly why it is a separate
// verb from verification: you run it on purpose, and the diff goes through
// review like any other change.

// RelinkOptions configures Relink.
type RelinkOptions struct {
	// DryRun computes the changes without writing them.
	DryRun bool
	// From limits rewriting to migrations numbered at or above this prefix,
	// so a lane can rebase only its own new files.
	From string
}

// RelinkChange is one parent line Relink rewrote (or would rewrite).
type RelinkChange struct {
	File string
	From string // the previous header line, empty when there was none
	To   string // the header line it now carries
}

// Relink rewrites each migration's parent line to the digest of the file that
// actually precedes it, and stamps root on the lowest-numbered one. It reads
// and writes files and nothing else — no database, no context, no DSN.
func Relink(dir string, opts RelinkOptions) ([]RelinkChange, error) {
	migrations, err := loadFiles(os.DirFS(dir), ".")
	if err != nil {
		return nil, err
	}

	var from int64 = -1
	if strings.TrimSpace(opts.From) != "" {
		n, ok := numericPrefix(opts.From + "_x.up.sql")
		if !ok {
			return nil, fmt.Errorf("relink: --from %q is not a migration number", opts.From)
		}
		from = n
	}

	var changes []RelinkChange
	for i, m := range migrations {
		if from >= 0 {
			if n, ok := numericPrefix(m.Name); !ok || n < from {
				continue
			}
		}
		want := "-- parent: root"
		if i > 0 {
			prev := migrations[i-1]
			want = fmt.Sprintf("-- parent: %s sha256:%s", Prefix(prev.Name), ContentDigest(prev.Content))
		}

		have := ""
		if line, _, _, ok := firstContentLine(m.Content); ok && reParentHeader.MatchString(line) {
			have = line
		}
		if have == want {
			continue
		}
		changes = append(changes, RelinkChange{File: m.Name, From: have, To: want})
		if opts.DryRun {
			continue
		}
		if err := os.WriteFile(filepath.Join(dir, m.Name), []byte(want+"\n"+canonicalBody(m.Content)), 0o644); err != nil {
			return nil, fmt.Errorf("relink %s: %w", m.Name, err)
		}
	}
	sort.SliceStable(changes, func(i, j int) bool { return changes[i].File < changes[j].File })
	return changes, nil
}
