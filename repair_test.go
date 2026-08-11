package migratekit

import (
	"context"
	"errors"
	"testing"
)

// clearCI makes the process look like an operator's shell. The repair verbs
// refuse under CI, and the test suite itself runs under CI, so every test that
// exercises a repair has to say so explicitly.
func clearCI(t *testing.T) {
	t.Helper()
	for _, name := range ciEnvVars {
		t.Setenv(name, "")
	}
}

func TestRepairRefusesInCI(t *testing.T) {
	clearCI(t)
	t.Setenv("GITHUB_ACTIONS", "true")

	m := Migration{Name: "0001_init.up.sql", Content: "SELECT 1"}
	req := RepairRequest{Reason: "the pipeline said so"}

	// db is nil on purpose: the guard must fire before anything touches it.
	p := NewPostgres(nil, "app")

	if _, err := p.RepairAdopt(context.Background(), m, req); !errors.Is(err, ErrRepairInCI) {
		t.Fatalf("repair adopt must refuse in CI, got: %v", err)
	}
	if _, err := p.RepairAcceptContent(context.Background(), m, req); !errors.Is(err, ErrRepairInCI) {
		t.Fatalf("repair accept-content must refuse in CI, got: %v", err)
	}
	if _, err := p.RepairAdoptAllUnmatched(context.Background(), []Migration{m}, req); !errors.Is(err, ErrRepairInCI) {
		t.Fatalf("repair adopt --all-unmatched must refuse in CI, got: %v", err)
	}
	err := p.ApplyWithOrderingException(context.Background(), []Migration{m}, []string{"1"}, req)
	if !errors.Is(err, ErrRepairInCI) {
		t.Fatalf("apply --allow-below-applied must refuse in CI, got: %v", err)
	}
	// The refusal has to say WHY a pipeline is the wrong place for it.
	if !contains(err.Error(), "fix it in the repository") {
		t.Errorf("the CI refusal must point at the real fix; got: %v", err)
	}
}

func TestDetectCI(t *testing.T) {
	clearCI(t)
	if name, in := DetectCI(); in {
		t.Fatalf("a cleared environment must not look like CI, got %s", name)
	}
	// Some shells export CI="" or CI=false; neither is CI.
	t.Setenv("CI", "false")
	if name, in := DetectCI(); in {
		t.Fatalf(`CI="false" must not count, got %s`, name)
	}
	t.Setenv("CI", "1")
	if _, in := DetectCI(); !in {
		t.Fatal("CI=1 must count")
	}
}

func TestRepairRequiresAReason(t *testing.T) {
	clearCI(t)
	m := Migration{Name: "0001_init.up.sql", Content: "SELECT 1"}
	p := NewPostgres(nil, "app")

	for _, reason := range []string{"", "   "} {
		if _, err := p.RepairAdopt(context.Background(), m, RepairRequest{Reason: reason}); err == nil {
			t.Fatalf("a repair with reason %q must be refused", reason)
		} else if !contains(err.Error(), "--reason") {
			t.Fatalf("the refusal must name the missing flag; got: %v", err)
		}
	}
}

func TestApplyWithOrderingExceptionNeedsATarget(t *testing.T) {
	clearCI(t)
	err := NewPostgres(nil, "app").ApplyWithOrderingException(
		context.Background(), nil, nil, RepairRequest{Reason: "x"})
	if err == nil || !contains(err.Error(), "at least one migration") {
		t.Fatalf("an exception with nothing to exempt is a plain apply; got: %v", err)
	}
}
