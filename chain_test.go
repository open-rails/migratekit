package migratekit

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
)

func rawSHA(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// chainFS builds a migration directory from name->body pairs.
func chainFS(files map[string]string) fstest.MapFS {
	out := fstest.MapFS{}
	for name, body := range files {
		out[name] = &fstest.MapFile{Data: []byte(body)}
	}
	return out
}

func headed(parent, digest, sql string) string {
	if parent == "root" {
		return "-- parent: root\n" + sql
	}
	return "-- parent: " + parent + " sha256:" + digest + "\n" + sql
}

// --- the digest contract ----------------------------------------------------

// The parent line is excluded from the digest, so adding one to an
// already-applied migration does not change what the ledger recorded. This is
// what makes adoption silent — no content-drift warning on any database.
func TestContentDigest_ExcludesTheParentLine(t *testing.T) {
	body := "CREATE TABLE t (id BIGINT);\n"
	bare := ContentDigest(body)
	if bare != rawSHA(body) {
		t.Fatalf("headerless digest changed from v1.5.0: %s vs %s", bare, rawSHA(body))
	}
	withRoot := ContentDigest("-- parent: root\n" + body)
	withLink := ContentDigest("-- parent: 4 sha256:" + strings.Repeat("a", 64) + "\n" + body)
	if withRoot != bare || withLink != bare {
		t.Fatalf("parent line leaked into the digest: bare=%s root=%s link=%s", bare, withRoot, withLink)
	}
}

// Only the FIRST non-blank line is a header. A `-- parent:` further down is
// ordinary prose and stays in the body.
func TestContentDigest_OnlyStripsTheHeaderLine(t *testing.T) {
	body := "CREATE TABLE t (id BIGINT);\n-- parent: 9 sha256:x\n"
	if ContentDigest("-- parent: root\n"+body) != ContentDigest(body) {
		t.Fatal("header strip is not idempotent on a prose parent line")
	}
	if ContentDigest(body) == ContentDigest("CREATE TABLE t (id BIGINT);\n") {
		t.Fatal("a non-header parent line must stay in the body")
	}
}

// --- parsing ----------------------------------------------------------------

func TestParseParentLink(t *testing.T) {
	d := strings.Repeat("ab", 32)
	for _, tc := range []struct {
		name, body string
		wantErr    string
		present    bool
		root       bool
		parent     string
	}{
		{name: "root", body: "-- parent: root\nSELECT 1;", present: true, root: true},
		{name: "link", body: "-- parent: 4 sha256:" + d + "\nSELECT 1;", present: true, parent: "4"},
		{name: "leading blank lines", body: "\n\n-- parent: root\nSELECT 1;", present: true, root: true},
		{name: "padded number normalizes", body: "-- parent: 0004 sha256:" + d + "\nSELECT 1;", present: true, parent: "4"},
		{name: "absent", body: "SELECT 1;"},
		{name: "no digest", body: "-- parent: 4\nSELECT 1;", wantErr: "sha256:"},
		{name: "short digest", body: "-- parent: 4 sha256:abcd\nSELECT 1;", wantErr: "sha256:"},
		{name: "non-hex digest", body: "-- parent: 4 sha256:" + strings.Repeat("z", 64) + "\nSELECT 1;", wantErr: "sha256:"},
		{name: "non-numeric parent", body: "-- parent: banana sha256:" + d + "\nSELECT 1;", wantErr: "sha256:"},
		{name: "root with a digest", body: "-- parent: root sha256:" + d + "\nSELECT 1;", wantErr: "sha256:"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			link, err := parseParentLink(tc.body)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("expected an error mentioning %q", tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error should teach the form (%q): %v", tc.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if link.Present != tc.present || link.IsRoot != tc.root || link.Parent != tc.parent {
				t.Fatalf("got %+v, want present=%v root=%v parent=%q", link, tc.present, tc.root, tc.parent)
			}
		})
	}
}

// --- chain verification -----------------------------------------------------

func TestChain_ValidChainLoads(t *testing.T) {
	one := "CREATE TABLE a (id BIGINT);\n"
	f1 := headed("root", "", one)
	two := "CREATE TABLE b (id BIGINT);\n"
	f2 := headed("1", ContentDigest(f1), two)
	three := "CREATE TABLE c (id BIGINT);\n"
	f3 := headed("2", ContentDigest(f2), three)

	fsys := chainFS(map[string]string{
		"0001_a.up.sql": f1, "0002_b.up.sql": f2, "0003_c.up.sql": f3,
	})
	got, err := Load(fsys, ".", RequireParentLinks())
	if err != nil {
		t.Fatalf("valid chain refused: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("want 3 migrations, got %d", len(got))
	}
}

func loadErr(t *testing.T, files map[string]string, opts ...LoadOption) string {
	t.Helper()
	_, err := Load(chainFS(files), ".", opts...)
	if err == nil {
		t.Fatal("expected the chain to be refused")
	}
	return err.Error()
}

func mustMention(t *testing.T, msg string, parts ...string) {
	t.Helper()
	for _, p := range parts {
		if !strings.Contains(msg, p) {
			t.Fatalf("error must name %q:\n%s", p, msg)
		}
	}
}

func TestChain_MissingParent(t *testing.T) {
	f1 := headed("root", "", "SELECT 1;\n")
	f3 := headed("2", strings.Repeat("aa", 32), "SELECT 3;\n")
	msg := loadErr(t, map[string]string{"0001_a.up.sql": f1, "0003_c.up.sql": f3})
	mustMention(t, msg, "0003_c.up.sql", "2")
}

// The property that makes the directory a hash chain: editing history breaks
// every later link.
func TestChain_HashMismatchNamesBothFiles(t *testing.T) {
	f1 := headed("root", "", "CREATE TABLE a (id BIGINT);\n")
	f2 := headed("1", ContentDigest(f1), "CREATE TABLE b (id BIGINT);\n")
	tampered := headed("root", "", "CREATE TABLE a (id BIGINT, evil TEXT);\n")

	msg := loadErr(t, map[string]string{"0001_a.up.sql": tampered, "0002_b.up.sql": f2})
	mustMention(t, msg, "0001_a.up.sql", "0002_b.up.sql")
}

// The lane that merges second: it renumbered but left its parent line pointing
// at the file that is no longer its predecessor.
func TestChain_StaleParentAfterRenumberIsRefused(t *testing.T) {
	f1 := headed("root", "", "SELECT 1;\n")
	laneA := headed("1", ContentDigest(f1), "SELECT 2;\n")
	// Lane B branched from 1 too, renumbered to 0003, forgot the parent line.
	laneB := headed("1", ContentDigest(f1), "SELECT 3;\n")

	msg := loadErr(t, map[string]string{
		"0001_a.up.sql": f1, "0002_a.up.sql": laneA, "0003_b.up.sql": laneB,
	})
	mustMention(t, msg, "0003_b.up.sql", "0002_a.up.sql")
}

func TestChain_ForwardLinkIsRefused(t *testing.T) {
	f1 := headed("root", "", "SELECT 1;\n")
	f2 := headed("3", strings.Repeat("bb", 32), "SELECT 2;\n")
	f3 := headed("2", strings.Repeat("cc", 32), "SELECT 3;\n")
	msg := loadErr(t, map[string]string{
		"0001_a.up.sql": f1, "0002_b.up.sql": f2, "0003_c.up.sql": f3,
	})
	mustMention(t, msg, "0002_b.up.sql")
}

func TestChain_TwoRootsAreRefused(t *testing.T) {
	msg := loadErr(t, map[string]string{
		"0001_a.up.sql": headed("root", "", "SELECT 1;\n"),
		"0002_b.up.sql": headed("root", "", "SELECT 2;\n"),
	})
	mustMention(t, msg, "0001_a.up.sql", "0002_b.up.sql", "root")
}

// A squash resets the chain root by DELETING what it replaces, so the squash
// file is the lowest-numbered file. A root anywhere else is a mistake.
func TestChain_MisplacedRootIsRefused(t *testing.T) {
	msg := loadErr(t, map[string]string{
		"0001_a.up.sql": "SELECT 1;\n", // headerless, tolerated

		"0002_b.up.sql": headed("root", "", "SELECT 2;\n"),
	})
	mustMention(t, msg, "0002_b.up.sql", "0001_a.up.sql")
}

// ...and the squash itself, where the replaced files are gone, is accepted.
func TestChain_SquashResetsTheRoot(t *testing.T) {
	squash := headed("root", "", "CREATE TABLE everything (id BIGINT);\n")
	next := headed("41", ContentDigest(squash), "SELECT 1;\n")
	if _, err := Load(chainFS(map[string]string{
		"0041_squash.up.sql": squash, "0042_next.up.sql": next,
	}), ".", RequireParentLinks()); err != nil {
		t.Fatalf("a squash root must be accepted: %v", err)
	}
}

// --- legacy tolerance -------------------------------------------------------

func TestChain_HeaderlessIsWarnedNotRefused(t *testing.T) {
	var warned []string
	_, err := Load(chainFS(map[string]string{
		"0001_a.up.sql": "SELECT 1;\n", "0002_b.up.sql": "SELECT 2;\n",
	}), ".", WithChainWarnFunc(func(s string) { warned = append(warned, s) }))
	if err != nil {
		t.Fatalf("headerless files must be tolerated: %v", err)
	}
	if len(warned) != 2 {
		t.Fatalf("want a warning per headerless file, got %d: %v", len(warned), warned)
	}
}

func TestChain_HeaderlessIsRefusedWhenRequired(t *testing.T) {
	msg := loadErr(t, map[string]string{"0001_a.up.sql": "SELECT 1;\n"}, RequireParentLinks())
	mustMention(t, msg, "0001_a.up.sql", "-- parent:")
}

// Incremental adoption: a repo adopts by heading its NEWEST file, whose parent
// is still headerless. Hashing a parent does not require the parent to have a
// header.
func TestChain_HeadedFileVerifiesAgainstAHeaderlessParent(t *testing.T) {
	f1 := "SELECT 1;\n"
	f2 := headed("1", ContentDigest(f1), "SELECT 2;\n")
	if _, err := Load(chainFS(map[string]string{
		"0001_a.up.sql": f1, "0002_b.up.sql": f2,
	}), "."); err != nil {
		t.Fatalf("mixed chain refused: %v", err)
	}
	// ...and the link still detects a tampered headerless parent.
	msg := loadErr(t, map[string]string{"0001_a.up.sql": "SELECT 999;\n", "0002_b.up.sql": f2})
	mustMention(t, msg, "0001_a.up.sql", "0002_b.up.sql")
}

// LoadFromFS keeps its v1.5.0 contract: tolerant, no options.
func TestLoadFromFS_StaysTolerant(t *testing.T) {
	if _, err := LoadFromFS(chainFS(map[string]string{"0001_a.up.sql": "SELECT 1;\n"})); err != nil {
		t.Fatalf("LoadFromFS must stay tolerant: %v", err)
	}
}

// --- the CI gate ------------------------------------------------------------

func TestCheckChainFS(t *testing.T) {
	f1 := headed("root", "", "SELECT 1;\n")
	f2 := headed("1", ContentDigest(f1), "SELECT 2;\n")
	good := chainFS(map[string]string{"0001_a.up.sql": f1, "0002_b.up.sql": f2})
	if err := CheckChainFS(good, ".", true); err != nil {
		t.Fatalf("clean chain refused: %v", err)
	}
	bad := chainFS(map[string]string{"0001_a.up.sql": f1, "0003_b.up.sql": f2})
	if err := CheckChainFS(bad, ".", true); err == nil {
		t.Fatal("a gap must still be refused by the numbering rule")
	}
}

// --- relink -----------------------------------------------------------------

func TestRelink_FixesAStaleParentLineAfterARenumber(t *testing.T) {
	dir := t.TempDir()
	f1 := headed("root", "", "SELECT 1;\n")
	laneA := headed("1", ContentDigest(f1), "SELECT 2;\n")
	staleB := headed("1", ContentDigest(f1), "SELECT 3;\n") // renumbered to 0003, line not updated
	write := func(n, b string) { mustWrite(t, filepath.Join(dir, n), b) }
	write("0001_a.up.sql", f1)
	write("0002_a.up.sql", laneA)
	write("0003_b.up.sql", staleB)

	if _, err := Load(os.DirFS(dir), "."); err == nil {
		t.Fatal("precondition: the stale chain must be refused")
	}

	changes, err := Relink(dir, RelinkOptions{DryRun: true})
	if err != nil {
		t.Fatalf("relink --dry-run: %v", err)
	}
	if len(changes) != 1 || changes[0].File != "0003_b.up.sql" {
		t.Fatalf("want one change to 0003_b.up.sql, got %+v", changes)
	}
	if _, err := Load(os.DirFS(dir), "."); err == nil {
		t.Fatal("--dry-run must not have written anything")
	}

	if _, err := Relink(dir, RelinkOptions{}); err != nil {
		t.Fatalf("relink: %v", err)
	}
	if _, err := Load(os.DirFS(dir), ".", RequireParentLinks()); err != nil {
		t.Fatalf("relink did not repair the chain: %v", err)
	}
	// Idempotent.
	again, err := Relink(dir, RelinkOptions{})
	if err != nil || len(again) != 0 {
		t.Fatalf("relink is not idempotent: %v %+v", err, again)
	}
}

func TestRelink_StampsRootAndAddsMissingHeaders(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "0001_a.up.sql"), "SELECT 1;\n")
	mustWrite(t, filepath.Join(dir, "0002_b.up.sql"), "SELECT 2;\n")

	if _, err := Relink(dir, RelinkOptions{}); err != nil {
		t.Fatalf("relink: %v", err)
	}
	if _, err := Load(os.DirFS(dir), ".", RequireParentLinks()); err != nil {
		t.Fatalf("relink must produce a chain that satisfies RequireParentLinks: %v", err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "0001_a.up.sql"))
	if !strings.HasPrefix(string(b), "-- parent: root\n") {
		t.Fatalf("lowest file must be stamped root, got:\n%s", b)
	}
	// The digest is body-only, so relinking did NOT change what the ledger
	// recorded for an already-applied migration.
	if ContentDigest(string(b)) != ContentDigest("SELECT 1;\n") {
		t.Fatal("relink changed an applied migration's ledger digest")
	}
}

func TestRelink_NeverTouchesADatabase(t *testing.T) {
	// Structural: Relink takes a directory and nothing else. A compile-time
	// assertion that the signature carries no DSN, no *sql.DB, no context.
	var _ func(string, RelinkOptions) ([]RelinkChange, error) = Relink
}

func mustWrite(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
