package migratekit

import (
	"strings"
	"testing"
	"testing/fstest"
)

// findings is the whole subject of these tests: what the lint complains about,
// for one in-memory migration.
func findings(t *testing.T, body string) []string {
	t.Helper()
	return RepairTotalityFindings("0002_x.up.sql", body)
}

func wantClean(t *testing.T, body string) {
	t.Helper()
	if got := findings(t, body); len(got) != 0 {
		t.Fatalf("expected no findings, got:\n%s", strings.Join(got, "\n"))
	}
}

func wantRefused(t *testing.T, body, mustContain string) {
	t.Helper()
	got := findings(t, body)
	if len(got) == 0 {
		t.Fatalf("expected a finding, got none")
	}
	joined := strings.Join(got, "\n")
	if !strings.Contains(joined, mustContain) {
		t.Fatalf("finding does not mention %q:\n%s", mustContain, joined)
	}
}

func TestRepairTotality_ConstraintWithNoRepairIsRefused(t *testing.T) {
	wantRefused(t, `
ALTER TABLE app.users ALTER COLUMN email SET NOT NULL;
`, "app.users")
}

func TestRepairTotality_RepairBeforeConstraintPasses(t *testing.T) {
	wantClean(t, `
DELETE FROM app.users WHERE email IS NULL;
ALTER TABLE app.users ALTER COLUMN email SET NOT NULL;
`)
}

// Position is the whole point: a repair below the constraint runs after the
// constraint has already refused the rows.
func TestRepairTotality_RepairAfterConstraintIsRefused(t *testing.T) {
	wantRefused(t, `
ALTER TABLE app.users ALTER COLUMN email SET NOT NULL;
DELETE FROM app.users WHERE email IS NULL;
`, "app.users")
}

func TestRepairTotality_WaiverPasses(t *testing.T) {
	wantClean(t, `
-- Repair: none-needed the column has been written NOT NULL by every code path
-- since th#100, and the backfill ran in 0001.
ALTER TABLE app.users ALTER COLUMN email SET NOT NULL;
`)
}

func TestRepairTotality_WaiverMustPrecedeTheConstraint(t *testing.T) {
	wantRefused(t, `
ALTER TABLE app.users ALTER COLUMN email SET NOT NULL;
-- Repair: none-needed too late to matter
`, "app.users")
}

// Prose that promises a repair which is not there is exactly what the gate is
// for: a bare `-- Repair:` with no DML and no `none-needed` does not satisfy it.
func TestRepairTotality_BareRepairProseIsNotAWaiver(t *testing.T) {
	wantRefused(t, `
-- Repair: the violating rows are deleted below.
ALTER TABLE app.users ALTER COLUMN email SET NOT NULL;
`, "app.users")
}

func TestRepairTotality_SameFileTableIsExemptWithoutAWaiver(t *testing.T) {
	wantClean(t, `
CREATE TABLE app.things (id BIGINT PRIMARY KEY, slug TEXT);
ALTER TABLE app.things ALTER COLUMN slug SET NOT NULL;
ALTER TABLE app.things ADD CONSTRAINT things_slug_key UNIQUE (slug);
CREATE UNIQUE INDEX things_slug_idx ON app.things (slug);
`)
}

func TestRepairTotality_DeleteInsideACommentIsNotARepair(t *testing.T) {
	wantRefused(t, `
-- DELETE FROM app.users WHERE email IS NULL;
/* DELETE FROM app.users WHERE email IS NULL; */
ALTER TABLE app.users ALTER COLUMN email SET NOT NULL;
`, "app.users")
}

// A statement whose table the lexical scan cannot resolve must be reported,
// never silently passed. The waiver is the only way through.
func TestRepairTotality_UnresolvedTableIsUnknownAndRefused(t *testing.T) {
	wantRefused(t, `
VALIDATE CONSTRAINT users_email_check;
`, "UNKNOWN")
}

func TestRepairTotality_UnresolvedTableAcceptsAWaiver(t *testing.T) {
	wantClean(t, `
-- Repair: none-needed this VALIDATE is a no-op re-run; the constraint was
-- validated in 0003 and the table has been append-only since.
VALIDATE CONSTRAINT users_email_check;
`)
}

func TestRepairTotality_NotValidAssertsNothing(t *testing.T) {
	wantClean(t, `
ALTER TABLE app.users ADD CONSTRAINT users_email_ck CHECK (email <> '') NOT VALID;
`)
}

func TestRepairTotality_ValidateNeedsARepair(t *testing.T) {
	wantRefused(t, `
ALTER TABLE app.users ADD CONSTRAINT users_email_ck CHECK (email <> '') NOT VALID;
ALTER TABLE app.users VALIDATE CONSTRAINT users_email_ck;
`, "validate-constraint")
}

func TestRepairTotality_AddNotNullColumnWithDefaultIsFine(t *testing.T) {
	wantClean(t, `
ALTER TABLE app.users ADD COLUMN tier TEXT NOT NULL DEFAULT 'free';
`)
}

func TestRepairTotality_AddNotNullColumnWithoutDefaultNeedsARepair(t *testing.T) {
	wantRefused(t, `
ALTER TABLE app.users ADD COLUMN tier TEXT NOT NULL;
`, "add-not-null-column")
}

func TestRepairTotality_ForeignKeyNeedsARepair(t *testing.T) {
	wantRefused(t, `
ALTER TABLE app.orders ADD CONSTRAINT orders_user_fk FOREIGN KEY (user_id) REFERENCES app.users (id);
`, "add-foreign-key")
}

func TestRepairTotality_UpdateCountsAsARepair(t *testing.T) {
	wantClean(t, `
UPDATE app.users SET email = '' WHERE email IS NULL;
ALTER TABLE app.users ALTER COLUMN email SET NOT NULL;
`)
}

// The repair must target the table being constrained, not just any table.
func TestRepairTotality_RepairOnADifferentTableDoesNotCount(t *testing.T) {
	wantRefused(t, `
DELETE FROM app.sessions WHERE email IS NULL;
ALTER TABLE app.users ALTER COLUMN email SET NOT NULL;
`, "app.users")
}

// tensorhub's 0005 is the worked example: three DELETEs, then the constraints
// they make satisfiable.
func TestRepairTotality_WorkedExample(t *testing.T) {
	wantClean(t, `
-- Repair: the violating rows are DELETED below, not migrated.
DELETE FROM th.upload_session_files WHERE blob_digest !~ '^sha256:[0-9a-f]{64}$';
DELETE FROM th.blob_reverse_lookup  WHERE blob_digest !~ '^sha256:[0-9a-f]{64}$';
DELETE FROM th.blobs                WHERE blob_digest !~ '^sha256:[0-9a-f]{64}$';

ALTER TABLE th.blobs DROP CONSTRAINT blobs_blob_digest_tagged;
ALTER TABLE th.blobs
    ADD CONSTRAINT blobs_blob_digest_sha256
    CHECK (blob_digest ~ '^sha256:[0-9a-f]{64}$') NOT VALID;
ALTER TABLE th.blobs VALIDATE CONSTRAINT blobs_blob_digest_sha256;

ALTER TABLE th.blob_reverse_lookup DROP CONSTRAINT blob_reverse_lookup_blob_digest_tagged;
ALTER TABLE th.blob_reverse_lookup
    ADD CONSTRAINT blob_reverse_lookup_blob_digest_sha256
    CHECK (blob_digest ~ '^sha256:[0-9a-f]{64}$') NOT VALID;
ALTER TABLE th.blob_reverse_lookup VALIDATE CONSTRAINT blob_reverse_lookup_blob_digest_sha256;

ALTER TABLE th.upload_session_files DROP CONSTRAINT upload_session_files_blob_digest_tagged;
ALTER TABLE th.upload_session_files
    ADD CONSTRAINT upload_session_files_blob_digest_sha256
    CHECK (blob_digest ~ '^sha256:[0-9a-f]{64}$') NOT VALID;
ALTER TABLE th.upload_session_files VALIDATE CONSTRAINT upload_session_files_blob_digest_sha256;
`)
}

func TestCheckRepairTotality_OverAFilesystem(t *testing.T) {
	fsys := fstest.MapFS{
		"m/0001_init.up.sql":  {Data: []byte("CREATE TABLE app.users (id BIGINT PRIMARY KEY, email TEXT);\n")},
		"m/0002_tight.up.sql": {Data: []byte("ALTER TABLE app.users ALTER COLUMN email SET NOT NULL;\n")},
		"m/notes.txt":         {Data: []byte("ALTER TABLE app.users ALTER COLUMN email SET NOT NULL;\n")},
	}
	err := CheckRepairTotality(fsys, "m")
	if err == nil {
		t.Fatal("expected 0002 to be refused")
	}
	if !strings.Contains(err.Error(), "0002_tight.up.sql") {
		t.Fatalf("error should name the offending file: %v", err)
	}
	// 0001 creates the table it constrains — it must not be reported, and a
	// non-migration file must not be scanned at all.
	if strings.Contains(err.Error(), "0001_init") || strings.Contains(err.Error(), "notes.txt") {
		t.Fatalf("unexpected file in findings: %v", err)
	}
}
