package migratekit

import (
	"errors"
	"strings"
	"testing"
)

func TestLockTableErrorNamesTheSetting(t *testing.T) {
	bare := errors.New("fingerprint billing (function): ERROR: out of shared memory (SQLSTATE 53200)")
	err := lockTableError(`reading the fingerprint of schema "billing"`, bare)
	for _, want := range []string{`reading the fingerprint of schema "billing"`, "max_locks_per_transaction", "out of shared memory"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("explanation lacks %q: %v", want, err)
		}
	}
	if !errors.Is(err, bare) {
		t.Fatal("the server's error must stay in the chain")
	}
	if again := lockTableError("rebuilding 3 migrations", err); again != err {
		t.Fatalf("explained twice: %v", again)
	}
	other := errors.New("ERROR: syntax error at or near \"x\" (SQLSTATE 42601)")
	if lockTableError("reading", other) != other || lockTableError("reading", nil) != nil {
		t.Fatal("only a full lock table is explained")
	}
}
