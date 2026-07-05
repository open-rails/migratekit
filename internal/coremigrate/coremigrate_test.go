package coremigrate

import (
	"strings"
	"testing"
)

func TestSubstituteTemplates(t *testing.T) {
	t.Setenv("MKIT_SET_VAR", "value1")
	t.Setenv("MKIT_EMPTY_VAR", "")

	// Set vars substitute (both styles); explicitly-empty vars are allowed.
	got, err := SubstituteTemplates("a ${MKIT_SET_VAR} b {{MKIT_SET_VAR}} c {{MKIT_EMPTY_VAR}} d")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "a value1 b value1 c  d" {
		t.Fatalf("got %q", got)
	}

	// Unset vars are an error, not a silent empty substitution.
	if _, err := SubstituteTemplates("PASSWORD '{{MKIT_DEFINITELY_UNSET_VAR}}'"); err == nil {
		t.Fatal("expected error for unset template variable")
	} else if !strings.Contains(err.Error(), "MKIT_DEFINITELY_UNSET_VAR") {
		t.Fatalf("error should name the variable: %v", err)
	}

	// ON_CLUSTER placeholders pass through untouched; empty templates are skipped.
	got, err = SubstituteTemplates("CREATE TABLE t {{ON_CLUSTER}} (${ON_CLUSTER}) ${} {{}}")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "CREATE TABLE t {{ON_CLUSTER}} (${ON_CLUSTER}) ${} {{}}" {
		t.Fatalf("got %q", got)
	}
}

func TestContains(t *testing.T) {
	if !Contains([]string{"a", "b"}, "b") {
		t.Fatal("expected true")
	}
	if Contains([]string{"a", "b"}, "c") {
		t.Fatal("expected false")
	}
	if Contains(nil, "a") {
		t.Fatal("expected false for nil slice")
	}
}
