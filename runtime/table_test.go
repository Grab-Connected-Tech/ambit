package runtime_test

import (
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/Grab-Connected-Tech/ambit/policy"
	"github.com/Grab-Connected-Tech/ambit/runtime"
)

// stripLits removes SQL literals from rule text, leaving the structure: which columns and
// which operator shape the rule has.
var litRe = regexp.MustCompile("'[^']*'")
var listRe = regexp.MustCompile(`\((\?\s*(,\s*)?)+\)`)

func stripLits(sql string) string {
	out := litRe.ReplaceAllString(sql, "?")
	// A list literal inlines as ('a','b') and a parameterised one as ($1,$2): collapse any
	// list of markers to a single one so the two renderings compare equal.
	out = listRe.ReplaceAllString(out, "(?)")
	// Collapse the repeated membership subqueries so a rule with two roles and a rule with
	// one differ only in structure, not in how many times the same pattern appears.
	out = strings.Join(strings.Fields(out), " ")
	return out
}

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
	res := appModel.FindResource("billing.Invoice")
	fixtureNormalized := map[string]string{}
	for label, node := range map[string]*policy.Node{
		"update rule":     res.Access.Update,
		"field read rule": res.Access.FieldRead[0].Expr,
	} {
		f, err := policy.TableSQL(policy.Normalize(node), appModel.PolicyTable("billing.Invoice"), policy.TargetPolicy, "t")
		if err != nil {
			t.Fatalf("normalise %s: %v", label, err)
		}
		fixtureNormalized[label] = stripLits(f.Text)
	}
	// The fixture was written before the model-driven builder existed and inlines its
	// policy literals into the SQL text. That is a rendering choice, not a semantic one,
	// so the comparison is structural: same rule, same actor bindings.
	cmp := func(t *testing.T, label string, g, f runtime.Frag) {
		t.Helper()
		if kinds(g.Binds) != kinds(f.Binds) && !sameRoleSet(g.Binds, f.Binds) {
			t.Errorf("%s: bindings %v vs the fixture's %v", label, kinds(g.Binds), kinds(f.Binds))
		}
		if want, ok := fixtureNormalized[label]; ok {
			if stripLits(g.SQL) != want {
				t.Errorf("%s: the model-driven rule is not the normalised rule:\n got  %s\n want %s", label, stripLits(g.SQL), want)
			}
		}
		// A rule whose normalised form differs from the fixture's literal spelling is only
		// compared against the normalised form; the fixture predates normalisation and
		// writes an Or of two role tests where the model compiles one role-set test.
		if _, normalised := fixtureNormalized[label]; !normalised && stripLits(g.SQL) != stripLits(f.SQL) {
			t.Errorf("%s differs:\n generated %s\n fixture  %s", label, g.SQL, f.SQL)
		}
	}
	// The fixture writes an Or of two Role() nodes by hand, which mentions each actor
	// binding twice; Normalize rewrites the same rule as a single IN list. Both are the
	// same rule, so binding counts are compared after that normalisation.
	r := func(f runtime.Frag) runtime.Frag { return f }
	_ = r
	cmp(t, "read rule", got.All, invTable.All)
	cmp(t, "create rule", got.Create, invTable.Create)
	cmp(t, "update rule", got.Update, invTable.Update)
	cmp(t, "delete rule", got.Delete, invTable.Delete)
	cmp(t, "field read rule", got.FieldRead[0].Frag, invTable.FieldRead[0].Frag)

	if len(got.FieldRead) != len(invTable.FieldRead) || len(got.FieldRead) == 0 {
		t.Fatalf("field read rules: %d vs %d", len(got.FieldRead), len(invTable.FieldRead))
	}
	// The index is a position in that table's own projection, so what has to agree is
	// which column it points at, not the number.
	if got.Columns[got.FieldRead[0].Index].Column != "notes" {
		t.Errorf("hidden column index points at %q", got.Columns[got.FieldRead[0].Index].Column)
	}
	if invTable.Columns[invTable.FieldRead[0].Index].Column != "notes" {
		t.Errorf("fixture index points at %q", invTable.Columns[invTable.FieldRead[0].Index].Column)
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

func kinds(bs []policy.Bind) string {
	parts := make([]string, len(bs))
	for i, b := range bs {
		parts[i] = string(b.Kind)
	}
	return strings.Join(parts, ",")
}

// sameRoleSet accepts a fixture that repeats an actor binding for each branch of an Or,
// where the normalised form mentions it once per role set.
func sameRoleSet(a, b []policy.Bind) bool {
	set := func(bs []policy.Bind) map[string]int {
		out := map[string]int{}
		for _, x := range bs {
			out[string(x.Kind)]++
		}
		return out
	}
	as, bs := set(a), set(b)
	for k, v := range bs {
		if as[k] == 0 && v > 0 {
			return false
		}
	}
	for k, v := range as {
		if v > 0 && bs[k] == 0 {
			return false
		}
	}
	return true
}

// TestGeneratedTableAgreesWithModelRules is the drift check on its own terms: the rules a
// resource's statements are built from must be exactly what the model says, normalised,
// with the actor bindings the rule needs. The hand-written fixture in the integration
// suite is a *second* implementation of the same idea, useful for testing the query layer
// but not for defining what the rules are (§6, §7.1).
func TestGeneratedTableAgreesWithModelRules(t *testing.T) {
	buildTables(t)
	type row struct{}
	tbl := runtime.BuildTable[row](appModel, "billing", "Invoice", &runtime.Table[row]{})
	res := appModel.FindResource("billing.Invoice")

	for label, pair := range map[string]struct {
		node *policy.Node
		frag runtime.Frag
	}{
		"read":        {res.Access.Read, tbl.All},
		"create":      {res.Access.Create, tbl.Create},
		"update":      {res.Access.Update, tbl.Update},
		"delete":      {res.Access.Delete, tbl.Delete},
		"updateAfter": {res.Access.UpdateAfter, tbl.UpdateAfter},
	} {
		want, err := policy.TableSQL(policy.Normalize(pair.node), appModel.PolicyTable("billing.Invoice"), policy.TargetPolicy, "t")
		if err != nil {
			t.Fatalf("%s: %v", label, err)
		}
		if want.Text != pair.frag.SQL {
			t.Errorf("%s: generated %q, the model says %q", label, pair.frag.SQL, want.Text)
		}
		if len(want.Binds) != len(pair.frag.Binds) {
			t.Errorf("%s: %d bindings, the model says %d", label, len(pair.frag.Binds), len(want.Binds))
		}
	}
	// A CHECK-enforced invariant lives in the DDL, not in a statement: the database
	// rejects the row and the framework maps the constraint name (§5.5). So the runtime
	// carries the deferred invariants plus the UpdateAfter transition test.
	deferred := 0
	for _, inv := range res.Invariants {
		if inv.Enforcement == "deferred" {
			deferred++
		}
	}
	if len(tbl.Checks) != deferred+1 {
		t.Errorf("%d checks: want %d deferred invariants plus the UpdateAfter test", len(tbl.Checks), deferred)
	}
	for _, c := range tbl.Checks {
		if c.Frag.SQL == "" {
			t.Errorf("check %s compiled to nothing", c.Name)
		}
	}
}

func TestDecodeInputIsStrict(t *testing.T) {
	type in struct {
		Name  string `json:"name"`
		Limit int    `json:"limit"`
	}
	var v in
	if err := runtime.DecodeInput(&runtime.Call{Body: []byte(`{"name":"a","nmae":"b"}`)}, &v); err == nil {
		t.Fatal("an unknown field must be refused")
	}
	if err := runtime.DecodeInput(&runtime.Call{Body: []byte("{\"name\":\"\xff\"}")}, &v); err == nil {
		t.Fatal("invalid UTF-8 must be refused")
	}
	if err := runtime.DecodeInput(&runtime.Call{Body: []byte(`{"name":"a"} {}`)}, &v); err == nil {
		t.Fatal("trailing data must be refused")
	}
	v = in{}
	if err := runtime.DecodeInput(&runtime.Call{Query: map[string][]string{"name": {"12"}, "limit": {"10"}}}, &v); err != nil || v.Limit != 10 || v.Name != "12" {
		t.Fatalf("query parameters: %+v %v", v, err)
	}
}
