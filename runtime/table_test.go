package runtime_test

import (
	"slices"
	"testing"

	"keystone.dev/keystone/runtime"
)

// TestBuildTableMatchesTheFixture says the model-driven table and the hand-written
// fixture agree, which is what lets generated code call BuildTable instead of emitting
// compiled rules into Go source (§7.1). If this drifts, generation is wrong somewhere.
func TestBuildTableMatchesTheFixture(t *testing.T) {
	// The fixture tables are built from pure Go: the model and the compiled rules, with
	// no database involved.
	buildTables(t)
	type row struct{}
	got := runtime.BuildTable[row](appModel, "billing", "Invoice", &runtime.Table[row]{})
	if got.Table != "invoice" {
		t.Fatalf("table name: %q", got.Table)
	}
	for name, pair := range map[string][2]runtime.Frag{
		"read":   {got.All, invTable.All},
		"create": {got.Create, invTable.Create},
		"update": {got.Update, invTable.Update},
		"delete": {got.Delete, invTable.Delete},
	} {
		if pair[0].SQL != pair[1].SQL {
			t.Errorf("%s rule differs:\n generated %s\n fixture  %s", name, pair[0].SQL, pair[1].SQL)
		}
		if len(pair[0].Binds) != len(pair[1].Binds) {
			t.Errorf("%s rule bind count: %d vs %d", name, len(pair[0].Binds), len(pair[1].Binds))
		}
	}
	if len(got.FieldRead) != len(invTable.FieldRead) || len(got.FieldRead) == 0 {
		t.Fatalf("field read rules: %d vs %d", len(got.FieldRead), len(invTable.FieldRead))
	}
	if got.FieldRead[0].SQL != invTable.FieldRead[0].SQL {
		t.Errorf("field read rule differs:\n %s\n %s", got.FieldRead[0].SQL, invTable.FieldRead[0].SQL)
	}
	if got.FieldRead[0].Index != invTable.FieldRead[0].Index {
		t.Errorf("hidden column index: %d vs %d", got.FieldRead[0].Index, invTable.FieldRead[0].Index)
	}
	if len(got.Checks) != len(invTable.Checks) {
		t.Fatalf("deferred invariants: %d vs %d", len(got.Checks), len(invTable.Checks))
	}
	for i := range got.Checks {
		if got.Checks[i].Name != invTable.Checks[i].Name || got.Checks[i].Frag.SQL != invTable.Checks[i].Frag.SQL {
			t.Errorf("invariant %d differs: %+v vs %+v", i, got.Checks[i], invTable.Checks[i])
		}
	}
	if !slices.Contains(got.Sensitive, "notes") {
		t.Errorf("a sensitive column must be listed for the audit layer: %v", got.Sensitive)
	}
}
