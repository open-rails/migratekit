package migratekit

import (
	"errors"
	"testing"
)

func mig(name, content string) Migration { return Migration{Name: name, Content: content} }

func appliedRow(m Migration) AppliedRecord {
	return AppliedRecord{
		Key: Prefix(m.Name), Filename: m.Name,
		Digest: ContentDigest(m.Content), SemanticDigest: SemanticContentDigest(m.Content),
		Status: StatusApplied,
	}
}

func ledger(records ...AppliedRecord) map[string]AppliedRecord {
	out := map[string]AppliedRecord{}
	for _, r := range records {
		out[r.Key] = r
	}
	return out
}

func TestPlanBaselineDecisions(t *testing.T) {
	shipped := mig("1000_v1_schema.up.sql", "CREATE TABLE users();")
	retired := AppliedRecord{Key: "15", Filename: "0015_drop_invites.up.sql", Digest: "old", SemanticDigest: "old", Status: StatusApplied}

	for name, tc := range map[string]struct {
		applied                 map[string]AppliedRecord
		record, retire, untouch int
	}{
		"an empty ledger records the whole tree": {
			applied: ledger(), record: 1,
		},
		"a retired predecessor is removed and the tree recorded": {
			applied: ledger(retired), record: 1, retire: 1,
		},
		"a ledger that already matches changes nothing": {
			applied: ledger(appliedRow(shipped)), untouch: 1,
		},
	} {
		t.Run(name, func(t *testing.T) {
			plan, err := PlanBaseline(tc.applied, []Migration{shipped})
			if err != nil {
				t.Fatalf("PlanBaseline: %v", err)
			}
			if len(plan.Record) != tc.record || len(plan.Retire) != tc.retire || len(plan.Untouched) != tc.untouch {
				t.Fatalf("record=%d retire=%d untouched=%d, want %d/%d/%d",
					len(plan.Record), len(plan.Retire), len(plan.Untouched), tc.record, tc.retire, tc.untouch)
			}
			if plan.Empty() != (tc.record+tc.retire == 0) {
				t.Fatalf("Empty() = %v for %+v", plan.Empty(), plan)
			}
		})
	}
}

// Each of these is a case where guessing would lose DDL or hide a half-applied
// migration, so the verb has to refuse rather than decide.
func TestPlanBaselineRefusals(t *testing.T) {
	shipped := mig("1000_v1_schema.up.sql", "CREATE TABLE users();")

	dirty := appliedRow(shipped)
	dirty.Status = "failed"
	dirty.Error = "relation already exists"

	ahead := AppliedRecord{Key: "1001", Filename: "1001_later.up.sql", Digest: "d", SemanticDigest: "s", Status: StatusApplied}
	unorderable := AppliedRecord{Key: "legacy", Filename: "legacy_thing.up.sql", Digest: "d", SemanticDigest: "s", Status: StatusApplied}

	drifted := appliedRow(shipped)
	drifted.Digest = "somethingelse"

	driftedSemantic := appliedRow(shipped)
	driftedSemantic.SemanticDigest = "somethingelse"

	renamed := appliedRow(shipped)
	renamed.Filename = "1000_renamed.up.sql"

	for name, tc := range map[string]struct {
		applied map[string]AppliedRecord
		tree    []Migration
		want    string
	}{
		"a half-applied migration is not erased":        {ledger(dirty), []Migration{shipped}, "half done"},
		"a row above the tree is not retired":           {ledger(ahead), []Migration{shipped}, "sorts above"},
		"a row that cannot be ordered is not retired":   {ledger(unorderable), []Migration{shipped}, "cannot be ordered"},
		"a drifted content digest belongs to adopt":     {ledger(drifted), []Migration{shipped}, "repair adopt"},
		"a drifted semantic digest belongs to adopt":    {ledger(driftedSemantic), []Migration{shipped}, "repair adopt"},
		"a renamed file belongs to adopt":               {ledger(renamed), []Migration{shipped}, "repair adopt"},
		"an empty tree has nothing to baseline against": {ledger(), nil, "no migrations"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := PlanBaseline(tc.applied, tc.tree)
			if err == nil {
				t.Fatal("expected a refusal")
			}
			if !errors.Is(err, ErrBaselineUnsafe) {
				t.Fatalf("err = %v, want it to wrap ErrBaselineUnsafe", err)
			}
			if !contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to explain %q", err, tc.want)
			}
		})
	}
}

// A dirty row refuses even when its key is one the tree has retired: the
// half-applied work matters more than the fact the file is gone.
func TestPlanBaselineRefusesDirtyRetiredRow(t *testing.T) {
	shipped := mig("1000_v1_schema.up.sql", "CREATE TABLE users();")
	dirty := AppliedRecord{Key: "15", Filename: "0015_drop_invites.up.sql", Status: "running"}
	if _, err := PlanBaseline(ledger(dirty), []Migration{shipped}); !errors.Is(err, ErrBaselineUnsafe) {
		t.Fatalf("err = %v, want a refusal", err)
	}
}
