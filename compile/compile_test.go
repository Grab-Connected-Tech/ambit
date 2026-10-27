package compile

import (
	"strings"
	"testing"

	"github.com/Grab-Connected-Tech/ambit/model"
	"github.com/Grab-Connected-Tech/ambit/policy"
)

// fixture builds the smallest app that passes, so each test can break one promise.
func fixture() *model.App {
	read := policy.Role("member")
	a := &model.App{
		SchemaVersion: model.SchemaVersion,
		App:           model.AppInfo{Name: "fixture", Tenant: "Org"},
		Roles:         []string{"admin", "member"},
		Tenant:        &model.Tenant{Name: "Org", Table: "org", Fields: model.BuiltInFields(false)},
		Membership:    &model.Tenant{Name: "Membership", Table: "am_membership", Fields: membershipFields()},
		Account:       &model.Tenant{Name: "User", Table: "user_account", Fields: append(model.BuiltInFields(true), model.Field{Name: "email", GoName: "Email", Column: "email", Type: model.Type{Kind: model.KindEmail}})},
		Defaults:      model.Defaults{QueryBudget: 20, RowBudget: 10000, ActionTimeout: "10s", StmtTimeout: "5s", MaxPageSize: 100, BodyLimitBytes: 1 << 20},
		Modules: []model.Module{{
			Name: "alpha",
			Resources: []model.Resource{{
				Name: "Thing", Table: "thing", Tenant: true, Timestamps: true, Versioned: true,
				Fields: append(model.BuiltInFields(true),
					model.Field{Name: "title", GoName: "Title", Column: "title", Type: model.Type{Kind: model.KindText, MaxLen: intp(80)}}),
				Access: model.Access{Read: read, Create: policy.Role("admin"), Update: policy.Role("admin"), Delete: policy.Never()},
			}},
			Actions: []model.Action{{
				Name: "ListThings", Kind: model.KindQuery, Requires: read,
				Reads:  []string{"alpha.Thing"},
				Output: model.Output{Page: "alpha.Thing"},
			}, {
				Name: "CreateThing", Kind: model.KindMutation, Requires: policy.Role("admin"),
				Writes: []string{"alpha.Thing"},
				Input:  []model.Field{{Name: "title", GoName: "Title", Column: "title", Type: model.Type{Kind: model.KindText, MaxLen: intp(80)}}},
				Output: model.Output{Resource: "alpha.Thing"},
			}},
		}, {
			Name: "beta",
			Resources: []model.Resource{{
				Name: "Note", Table: "note", Tenant: true, Timestamps: true, Versioned: true,
				Fields: append(model.BuiltInFields(true),
					model.Field{Name: "body", GoName: "Body", Column: "body", Type: model.Type{Kind: model.KindText, MaxLen: intp(200)}}),
				Access: model.Access{Read: read, Update: policy.Role("admin")},
			}},
			Actions: []model.Action{{
				Name: "Annotate", Kind: model.KindMutation, Requires: policy.Role("admin"),
				Writes: []string{"beta.Note"},
				Calls:  []string{"alpha.CreateThing"},
			}},
		}},
	}
	return a
}

func membershipFields() []model.Field {
	return []model.Field{
		{Name: "id", GoName: "ID", Column: "id", Type: model.Type{Kind: model.KindID}},
		{Name: "tenant_id", GoName: "TenantID", Column: "tenant_id", Type: model.Type{Kind: model.KindID}},
		{Name: "actor_id", GoName: "ActorID", Column: "actor_id", Type: model.Type{Kind: model.KindID}},
		{Name: "role", GoName: "Role", Column: "role", Type: model.Type{Kind: model.KindText, MaxLen: intp(64)}},
	}
}

func intp(i int) *int { return &i }

func mustHave(t *testing.T, d Diags, rule, want string) {
	t.Helper()
	for _, x := range d {
		if x.Rule == rule && strings.Contains(x.Message, want) {
			return
		}
	}
	t.Fatalf("no %s diagnostic mentioning %q in:\n%s", rule, want, dump(d))
}

func dump(d Diags) string {
	var b strings.Builder
	for _, x := range d {
		b.WriteString("  " + string(x.Severity) + " " + x.Rule + " " + x.Decl + ": " + x.Message + "\n")
	}
	return b.String()
}

func mustNotHave(t *testing.T, d Diags, rule string) {
	t.Helper()
	for _, x := range d {
		if x.Rule == rule {
			t.Fatalf("unexpected %s: %s\n%s", rule, x.Message, dump(d))
		}
	}
}

func TestFixturePasses(t *testing.T) {
	a := fixture()
	d := Validate(a)
	if d.Errors() != 0 {
		t.Fatalf("a well-formed app must pass, got:\n%s", dump(d))
	}
}

func TestEveryActionMustSayWhoMayCallIt(t *testing.T) {
	a := fixture()
	a.Modules[0].Actions[0].Requires = nil
	d := Validate(a)
	mustHave(t, d, "AM-SPEC-004", "neither Requires nor Public")
	if d.Errors() == 0 {
		t.Fatal("an unguarded endpoint must fail the build")
	}
}

func TestPublicMutationNeedsARateLimit(t *testing.T) {
	a := fixture()
	a.Modules[0].Actions[1].Public = true
	a.Modules[0].Actions[1].Requires = nil
	d := Validate(a)
	mustHave(t, d, "AM-SEC-005", "no rate limit")
	a.Modules[0].Actions[1].Limits.RateLimit = []model.RateLimit{{Scope: "ip", Limit: 5, Per: "1m0s"}}
	if got := Validate(a); got.Errors() != 0 {
		t.Fatalf("with a rate limit it must pass:\n%s", dump(got))
	}
}

func TestModulesMayNotWriteEachOthersResources(t *testing.T) {
	a := fixture()
	a.Modules[1].Actions[0].Writes = []string{"alpha.Thing"}
	d := Validate(a)
	mustHave(t, d, "AM-SPEC-005", "owned by module")
}

func TestUnknownRoleReference(t *testing.T) {
	a := fixture()
	a.Modules[0].Actions[0].Requires = policy.Role("superuser")
	d := Validate(a)
	mustHave(t, d, "AM-SPEC-002", `undeclared role "superuser"`)
}

func TestUnknownFieldInRuleFailsToCompile(t *testing.T) {
	a := fixture()
	a.Modules[0].Resources[0].Access.Read = policy.Cmp(policy.OpEq, policy.FieldRef("secret"), policy.Literal(policy.BoolLit(true)))
	d := Validate(a)
	mustHave(t, d, "AM-SPEC-003", "cannot be compiled to SQL")
}

func TestActorRefsCannotBecomeCheckConstraints(t *testing.T) {
	a := fixture()
	a.Modules[0].Resources[0].Invariants = []model.Invariant{
		{Name: "owner_is_admin", Expr: policy.Normalize(policy.Role("admin")), Enforcement: "check"},
	}
	d := Validate(a)
	mustHave(t, d, "AM-SPEC-003", "cannot be compiled")
}

func TestRowLocalInvariantCompiles(t *testing.T) {
	a := fixture()
	a.Modules[0].Resources[0].Invariants = []model.Invariant{
		{Name: "title_set", Expr: policy.Normalize(&policy.Node{Op: policy.OpIsSet, Kids: []*policy.Node{policy.FieldRef("title")}}), Enforcement: "check"},
	}
	if d := Validate(a); d.Errors() != 0 {
		t.Fatalf("a row-local invariant must pass:\n%s", dump(d))
	}
}

func TestCallCyclesAreReportedWithThePath(t *testing.T) {
	a := fixture()
	a.Modules[0].Actions[1].Calls = []string{"beta.Annotate"}
	d := Validate(a)
	var found bool
	for _, x := range d {
		if x.Rule == "AM-SPEC-002" && strings.Contains(x.Message, "cyclic") {
			found = true
			if !strings.Contains(x.Message, "alpha.CreateThing -> beta.Annotate -> alpha.CreateThing") {
				t.Fatalf("cycle path must be shown in full: %s", x.Message)
			}
		}
	}
	if !found {
		t.Fatalf("want a cycle diagnostic:\n%s", dump(d))
	}
}

func TestInputMayNotDeclareARuntimeOwnedField(t *testing.T) {
	a := fixture()
	a.Modules[0].Actions[1].Input = append(a.Modules[0].Actions[1].Input,
		model.Field{Name: "tenant_id", GoName: "TenantID", Column: "tenant_id", Type: model.Type{Kind: model.KindID}})
	d := Validate(a)
	mustHave(t, d, "AM-SEC-008", "the runtime sets")
}

func TestHTTPEffectNeedsAHostAllowlist(t *testing.T) {
	a := fixture()
	a.Modules[0].Actions[1].Effects = []model.Effect{{Type: model.EffectHTTP, Mode: "deferred"}}
	d := Validate(a)
	mustHave(t, d, "AM-SEC-006", "no host allowlist")
}

func TestSensitiveFieldWithoutAFieldRuleWarns(t *testing.T) {
	a := fixture()
	res := &a.Modules[0].Resources[0]
	res.Fields = append(res.Fields, model.Field{Name: "pin", GoName: "Pin", Column: "pin", Sensitive: true,
		Type: model.Type{Kind: model.KindText, MaxLen: intp(8)}})
	d := Validate(a)
	mustHave(t, d, "AM-SEC-007", "Sensitive field has no FieldRead rule")
	res.Access.FieldRead = []model.FieldRule{{Field: "pin", Expr: policy.Role("admin")}}
	if d2 := Validate(a); hasRule(d2, "AM-SEC-007") {
		t.Fatalf("the warning must disappear once a rule exists:\n%s", dump(d2))
	}
}

func hasRule(d Diags, rule string) bool {
	for _, x := range d {
		if x.Rule == rule {
			return true
		}
	}
	return false
}

func TestKindMisuseIsAnError(t *testing.T) {
	cases := []struct {
		name string
		mut  func(a *model.App)
		rule string
		want string
	}{
		{"query writes", func(a *model.App) { a.Modules[0].Actions[0].Writes = []string{"alpha.Thing"} }, "AM-SPEC-005", "a query declares Writes"},
		{"query effects", func(a *model.App) {
			a.Modules[0].Actions[0].Effects = []model.Effect{{Type: model.EffectEmail, Mode: "deferred"}}
		}, "AM-SPEC-005", "a query declares Effects"},
		{"job returns", func(a *model.App) {
			a.Modules[1].Actions = append(a.Modules[1].Actions, model.Action{Name: "Sweep", Kind: model.KindJob, Output: model.Output{Resource: "beta.Note"}})
		}, "AM-SPEC-005", "a job declares Returns"},
		{"schedule runs a mutation", func(a *model.App) {
			a.Modules[1].Actions = append(a.Modules[1].Actions, model.Action{Name: "Nightly", Kind: model.KindSchedule, Cron: "0 3 * * *", Runs: "beta.Annotate"})
		}, "AM-SPEC-005", "not a job"},
		{"schedule missing cron", func(a *model.App) {
			a.Modules[1].Actions = append(a.Modules[1].Actions, model.Action{Name: "Nightly", Kind: model.KindSchedule, Runs: "alpha.ListThings"})
		}, "AM-SPEC-005", "no cron"},
		{"query calls a mutation", func(a *model.App) { a.Modules[0].Actions[0].Calls = []string{"alpha.CreateThing"} }, "AM-SPEC-005", "a query calls"},
		{"cache on a mutation", func(a *model.App) { a.Modules[0].Actions[1].Cache = &model.Cache{TTL: "1m0s"} }, "AM-SPEC-005", "only queries may be cached"},
		{"Requires with a row condition", func(a *model.App) {
			a.Modules[0].Actions[0].Requires = policy.And(policy.Role("member"), policy.Cmp(policy.OpGt, policy.FieldRef("title"), policy.Literal(policy.IntLit(0))))
		}, "AM-SPEC-003", "only reference roles"},
		{"enum default outside values", func(a *model.App) {
			a.Modules[0].Resources[0].Fields = append(a.Modules[0].Resources[0].Fields, model.Field{
				Name: "kind", GoName: "Kind", Column: "kind",
				Type:    model.Type{Kind: model.KindEnum, Values: []string{"a", "b"}},
				Default: &model.Value{Kind: "string", Text: "z"}})
		}, "AM-SPEC-002", "is not one of"},
	}
	for _, tc := range cases {
		a := fixture()
		tc.mut(a)
		d := Validate(a)
		mustHave(t, d, tc.rule, tc.want)
	}
}

func TestReservedAndDuplicateErrorCodes(t *testing.T) {
	a := fixture()
	a.Modules[0].Actions[1].Errors = []model.ActionError{{Code: "not_found"}, {Code: "x"}, {Code: "x"}}
	d := Validate(a)
	mustHave(t, d, "AM-SPEC-002", "reserved by the framework")
	mustHave(t, d, "AM-SPEC-002", "declared twice")
}

func TestCrossModuleReadReferencesResolve(t *testing.T) {
	a := fixture()
	a.Modules[0].Actions[0].Reads = []string{"gamma.Whatever"}
	d := Validate(a)
	mustHave(t, d, "AM-SPEC-002", "not declared")
}

func TestTwoResourcesCannotShareATable(t *testing.T) {
	a := fixture()
	a.Modules[1].Resources[0].Table = "thing"
	d := Validate(a)
	mustHave(t, d, "AM-SPEC-002", "shared by")
}

func TestTenantResourceRefConsistency(t *testing.T) {
	a := fixture()
	a.Modules[1].Resources[0].Tenant = false
	a.Modules[1].Resources[0].Fields = a.Modules[1].Resources[0].Fields[1:] // drop tenant_id
	a.Modules[1].Resources[0].Fields = append(a.Modules[1].Resources[0].Fields,
		model.Field{Name: "thing", GoName: "Thing", Column: "thing_id", Type: model.Type{Kind: model.KindRef},
			Ref: &model.Ref{Resource: "alpha.Thing", OnDelete: "restrict"}})
	d := Validate(a)
	mustHave(t, d, "AM-SPEC-005", "non-tenant resource points at the tenant-scoped")
}

func TestDiagnosticsAreDeterministic(t *testing.T) {
	a := fixture()
	a.Modules[0].Actions[0].Requires = nil
	a.Modules[1].Actions[0].Writes = []string{"alpha.Thing"}
	first := Validate(a).JSON()
	for i := 0; i < 20; i++ {
		if got := Validate(a).JSON(); got != first {
			t.Fatalf("diagnostic output changed on run %d", i)
		}
	}
	if !strings.Contains(first, `"docs": "am explain AM-SPEC-004"`) {
		t.Fatalf("diagnostics must point at am explain: %s", first)
	}
}

func TestParseRejectsForeignSchemaVersion(t *testing.T) {
	a := fixture()
	raw := strings.Replace(string(a.Bytes()), `"schemaVersion": 1`, `"schemaVersion": 7`, 1)
	_, d := Parse([]byte(raw))
	mustHave(t, d, "AM-CODE-001", "schemaVersion 7")
}

func TestParseRoundTripsTheFixture(t *testing.T) {
	a := fixture()
	got, d := Parse(a.Bytes())
	if d.Errors() != 0 {
		t.Fatalf("the fixture model must parse and validate:\n%s", dump(d))
	}
	if got.Hash() != a.Hash() {
		t.Fatal("a model must survive a JSON round trip unchanged")
	}
}
