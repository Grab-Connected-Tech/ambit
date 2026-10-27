package migrate

import (
	"strings"
	"testing"

	"github.com/abhilashiig/ambit/model"
	"github.com/abhilashiig/ambit/policy"
)

func intp(i int) *int { return &i }

func field(name, col string, kind model.TypeKind, mods ...func(*model.Field)) model.Field {
	f := model.Field{Name: name, GoName: goName(name), Column: col, Type: model.Type{Kind: kind}}
	for _, m := range mods {
		m(&f)
	}
	return f
}

func goName(s string) string {
	out := []rune{}
	up := true
	for _, r := range s {
		if r == '_' {
			up = true
			continue
		}
		if up {
			out = append(out, []rune(strings.ToUpper(string(r)))[0])
			up = false
			continue
		}
		out = append(out, r)
	}
	return string(out)
}

func opt(f *model.Field) { f.Optional = true }

// app builds the billing reference model used by every test here.
func app() *model.App {
	invoice := model.Resource{
		Name: "Invoice", Table: "invoice", Tenant: true, Timestamps: true, Versioned: true,
		Fields: append(model.BuiltInFields(true),
			field("customer", "customer_id", model.KindRef, func(f *model.Field) {
				f.Ref = &model.Ref{Resource: "customers.Customer", OnDelete: "restrict"}
			}),
			(func() model.Field {
				f := field("amount", "amount_minor", model.KindMoney)
				f.Columns = []string{"amount_minor", "amount_currency"}
				f.Type.Currency = "USD"
				return f
			})(),
			(func() model.Field {
				f := field("status", "status", model.KindEnum)
				f.Type.Values = []string{"draft", "paid", "sent", "void"}
				return f
			})(),
			field("paid_at", "paid_at", model.KindTimestamp, opt),
			(func() model.Field {
				f := field("notes", "notes", model.KindText, opt)
				f.Sensitive = true
				f.Type.MaxLen = intp(2000)
				return f
			})(),
		),
		Invariants: []model.Invariant{{
			Name: "amount_positive", Enforcement: "check",
			Expr: policy.Normalize(policy.Cmp(policy.OpGt, policy.FieldRef("amount"), policy.Literal(policy.IntLit(0)))),
		}},
		Indexes: []model.Index{{Name: "invoice_customer_status", Fields: []string{"customer", "status"}}},
		Access: model.Access{
			Read:      policy.Role("member"),
			Delete:    policy.Never(),
			FieldRead: []model.FieldRule{{Field: "notes", Expr: policy.Role("admin")}},
		},
	}
	customer := model.Resource{
		Name: "Customer", Table: "customer", Tenant: true, Timestamps: true, Versioned: true,
		Fields: append(model.BuiltInFields(true),
			(func() model.Field {
				f := field("email", "email", model.KindEmail)
				f.Unique = true
				return f
			})()),
	}
	return &model.App{
		SchemaVersion: model.SchemaVersion,
		App:           model.AppInfo{Name: "billing", Tenant: "Org"},
		Roles:         []string{"admin", "member"},
		Tenant: &model.Tenant{Name: "Org", Table: "org",
			Fields: append(model.BuiltInFields(false), field("name", "name", model.KindText,
				func(f *model.Field) { f.Type.MaxLen = intp(200) }))},
		Membership: &model.Tenant{Name: "Membership", Table: "am_membership"},
		Account:    &model.Tenant{Name: "User", Table: "user_account"},
		Modules: []model.Module{
			{Name: "billing", Resources: []model.Resource{invoice}},
			{Name: "customers", Resources: []model.Resource{customer}},
		},
	}
}

func joined(t *testing.T, a *model.App) string {
	t.Helper()
	sql := strings.Join(DDL(a), "\n")
	if len(sql) < 200 {
		t.Fatalf("DDL looks empty: %d bytes", len(sql))
	}
	return sql
}

func TestFreshDDL(t *testing.T) {
	sql := joined(t, app())
	wants := []string{
		"CREATE TABLE \"invoice\"",
		"\"tenant_id\" uuid NOT NULL REFERENCES \"org\"(id) ON DELETE CASCADE",
		"\"id\" uuid NOT NULL",
		"\"created_at\" timestamptz NOT NULL DEFAULT now()",
		"\"version\" bigint NOT NULL DEFAULT 1",
		"\"customer_id\" uuid NOT NULL REFERENCES \"customer\"(id) ON DELETE RESTRICT",
		"\"amount_minor\" bigint NOT NULL",
		"\"amount_currency\" text NOT NULL DEFAULT 'USD'",
		"\"status\" text NOT NULL",
		"CHECK (\"status\" IN ('draft','paid','sent','void'))",
		"\"notes\" text",
		"CHECK (length(\"notes\") <= 2000)",
		"CONSTRAINT \"invoice_amount_positive\" CHECK (invoice.amount_minor > 0)",
		"CONSTRAINT \"invoice_pk\" PRIMARY KEY (id)",
		// Tenant-led, and the (customer_id) index the reference implies is covered by it.
		"CREATE INDEX \"invoice_customer_status\" ON \"invoice\" (\"tenant_id\", \"customer_id\", \"status\");",
		"ALTER TABLE \"invoice\" FORCE ROW LEVEL SECURITY;",
		"CREATE POLICY am_tenant_isolation ON \"invoice\"",
		"USING ((\"invoice\".tenant_id = coalesce(nullif(current_setting('am.tenant_id', true), ''), '00000000-0000-0000-0000-000000000000')::uuid OR",
		"WITH CHECK ((\"invoice\".tenant_id = coalesce",
		"CREATE TABLE \"am_membership\"", // framework tables are in the same stream
		"CREATE TABLE \"am_outbox\"",
		"CREATE TABLE \"am_jobs\"",
		"CREATE TABLE \"am_audit\"",
		"am_guard_immutable",
	}
	for _, w := range wants {
		if !strings.Contains(sql, w) {
			t.Errorf("DDL missing:\n  %s", w)
		}
	}
	if strings.Contains(sql, "invoice_customer_id_idx") {
		t.Error("an index whose columns lead another index is redundant")
	}
	if strings.Contains(sql, "CREATE TABLE \"am_membership\" (\n  id") && !strings.Contains(sql, "am_membership_unique") {
		t.Error("membership must be unique per actor, tenant and role")
	}
}

func TestDDLIsDeterministic(t *testing.T) {
	first := joined(t, app())
	for i := 0; i < 5; i++ {
		if got := joined(t, app()); got != first {
			t.Fatalf("DDL changed on run %d", i)
		}
	}
}

// TestClassificationTable walks the rows of §13.2 and asserts each change gets the
// class the specification assigns it.
func TestClassificationTable(t *testing.T) {
	cases := []struct {
		name string
		pre  func(a *model.App)
		mut  func(a *model.App)
		want Class
		sub  string
	}{
		{"add resource", nil, func(a *model.App) {
			a.Modules = append(a.Modules, model.Module{Name: "ledger", Resources: []model.Resource{{
				Name: "Entry", Table: "ledger_entry", Tenant: true, Timestamps: true, Versioned: true,
				Fields: append(model.BuiltInFields(true), field("memo", "memo", model.KindText, opt)),
			}}})
		}, Safe, "add resource"},
		{"add optional field", nil, func(a *model.App) {
			res := &a.Modules[0].Resources[0]
			res.Fields = append(res.Fields, field("due_at", "due_at", model.KindTimestamp, opt))
		}, Safe, "add optional field"},
		{"add field with default", nil, func(a *model.App) {
			f := field("channel", "channel", model.KindText)
			f.Default = &model.Value{Kind: "string", Text: "email"}
			f.Type.MaxLen = intp(20)
			a.Modules[0].Resources[0].Fields = append(a.Modules[0].Resources[0].Fields, f)
		}, Safe, "add field with default"},
		{"add required field without default", nil, func(a *model.App) {
			a.Modules[0].Resources[0].Fields = append(a.Modules[0].Resources[0].Fields,
				field("reference", "reference", model.KindText))
		}, NeedsBackfill, "add required field"},
		{"add index", nil, func(a *model.App) {
			res := &a.Modules[0].Resources[0]
			res.Indexes = append(res.Indexes, model.Index{Name: "invoice_status_idx", Fields: []string{"status"}})
		}, Safe, "add index"},
		{"add invariant", nil, func(a *model.App) {
			res := &a.Modules[0].Resources[0]
			res.Invariants = append(res.Invariants, model.Invariant{Name: "notes_short", Enforcement: "check",
				Expr: policy.Normalize(policy.Cmp(policy.OpLte, policy.FieldRef("notes"), policy.Literal(policy.IntLit(2000))))})
		}, NeedsValidation, "add invariant"},
		{"add enum value", nil, func(a *model.App) {
			f := a.Modules[0].Resources[0].FindField("status")
			f.Type.Values = append(f.Type.Values, "refunded")
		}, Safe, "add enum value"},
		{"remove enum value", nil, func(a *model.App) {
			f := a.Modules[0].Resources[0].FindField("status")
			f.Type.Values = []string{"draft", "paid", "void"}
		}, Destructive, "remove enum value"},
		{"rename field", nil, func(a *model.App) {
			res := &a.Modules[0].Resources[0]
			var keep []model.Field
			for _, f := range res.Fields {
				if f.Name == "notes" {
					f.Name, f.Column, f.GoName = "comment", "comment", "Comment"
					f.RenamedFrom = "notes"
				}
				keep = append(keep, f)
			}
			res.Fields = keep
		}, NeedsPlan, "rename field"},
		{"change field type", nil, func(a *model.App) {
			f := a.Modules[0].Resources[0].FindField("notes")
			f.Type = model.Type{Kind: model.KindJSON, GoType: "gen.Note"}
		}, NeedsPlan, "change type"},
		{"remove field", nil, func(a *model.App) {
			res := &a.Modules[0].Resources[0]
			var keep []model.Field
			for _, f := range res.Fields {
				if f.Name != "notes" {
					keep = append(keep, f)
				}
			}
			res.Fields = keep
		}, Destructive, "remove field"},
		{"remove resource", nil, func(a *model.App) {
			a.Modules = []model.Module{a.Modules[0]}
		}, Destructive, "remove resource"},
		{"becomes required", nil, func(a *model.App) {
			a.Modules[0].Resources[0].FindField("paid_at").Optional = false
		}, NeedsBackfill, "field becomes required"},
		{"becomes optional", nil, func(a *model.App) {
			a.Modules[0].Resources[0].FindField("customer").Optional = true
		}, Safe, "field becomes optional"},
		{"add uniqueness", nil, func(a *model.App) {
			a.Modules[1].Resources[0].FindField("email").Unique = false
			a.Modules[0].Resources[0].FindField("notes").Unique = true
			a.Modules[0].Resources[0].FindField("notes").Optional = true
		}, NeedsValidation, "add uniqueness"},
		{"remove index", nil, func(a *model.App) {
			a.Modules[0].Resources[0].Indexes = nil
		}, Safe, "remove index"},
		{"tenant scoping added", func(a *model.App) {
			res := &a.Modules[1].Resources[0]
			res.Tenant = false
			var keep []model.Field
			for _, f := range res.Fields {
				if f.Name != "tenant_id" {
					keep = append(keep, f)
				}
			}
			res.Fields = keep
		}, func(a *model.App) {
			res := &a.Modules[1].Resources[0]
			res.Tenant = true
			res.Fields = append(res.Fields, model.Field{Name: "tenant_id", GoName: "TenantID", Column: "tenant_id",
				Type: model.Type{Kind: model.KindID}, Immutable: true})
		}, NeedsBackfill, "becomes tenant scoped"},
		{"tenant scoping removed", nil, func(a *model.App) {
			res := &a.Modules[1].Resources[0]
			res.Tenant = false
			var keep []model.Field
			for _, f := range res.Fields {
				if f.Name != "tenant_id" {
					keep = append(keep, f)
				}
			}
			res.Fields = keep
		}, Destructive, "leaves tenant scoping"},
		{"invariant removed", nil, func(a *model.App) {
			a.Modules[0].Resources[0].Invariants = nil
		}, Safe, "remove invariant"},
		{"encryption turned on", nil, func(a *model.App) {
			f := a.Modules[0].Resources[0].FindField("notes")
			f.Type.Encrypted = true
			f.Type.Kind = model.KindText
		}, NeedsPlan, "encryption"},
	}
	for _, tc := range cases {
		base := app()
		if tc.pre != nil {
			tc.pre(base)
		}
		updated := base.Clone()
		if tc.mut != nil {
			tc.mut(updated)
		}
		p := Diff(base, updated)
		var got *Step
		for i := range p.Steps {
			if strings.Contains(p.Steps[i].Detail, tc.sub) || strings.Contains(p.Steps[i].Subject, tc.sub) {
				got = &p.Steps[i]
				break
			}
		}
		if got == nil {
			var all []string
			for _, s := range p.Steps {
				all = append(all, string(s.Class)+" "+s.Subject+" "+s.Detail)
			}
			t.Errorf("%s: no step matched %q, got: %s", tc.name, tc.sub, strings.Join(all, " | "))
			continue
		}
		if got.Class != tc.want {
			t.Errorf("%s: class %s want %s (%s)", tc.name, got.Class, tc.want, got.Detail)
		}
		if got.Class == Destructive && got.NeedsFlag != "--allow-destructive" {
			t.Errorf("%s: destructive steps must demand a flag", tc.name)
		}
		if got.Class == NeedsBackfill && got.Backfill == nil && len(got.Phases) == 0 {
			t.Errorf("%s: a needs-backfill step must describe the backfill", tc.name)
		}
	}
}

func TestNoOpDiffIsEmpty(t *testing.T) {
	a, b := app(), app()
	p := Diff(a, b)
	if !p.Empty() {
		for _, s := range p.Steps {
			t.Errorf("unexpected step %s %s %s", s.Class, s.Subject, s.Detail)
		}
	}
	if a.Hash() != b.Hash() {
		t.Fatal("the fixture model is not stable")
	}
}

func TestDestructiveStepsCarryTwoPhases(t *testing.T) {
	base := app()
	updated := base.Clone()
	updated.Modules = []model.Module{base.Modules[0]}
	p := Diff(base, updated)
	for _, s := range p.Steps {
		if s.Class == Destructive && len(s.Phases) < 2 {
			t.Fatalf("destructive step %s must document the two-phase plan", s.Subject)
		}
	}
	if p.MaxRisk() != "high" {
		t.Fatalf("max risk = %s", p.MaxRisk())
	}
}

func TestIndexChangesRunOutsideTheTransaction(t *testing.T) {
	base := app()
	updated := base.Clone()
	updated.Modules[0].Resources[0].Indexes = append(updated.Modules[0].Resources[0].Indexes,
		model.Index{Name: "invoice_paid_at_idx", Fields: []string{"paid_at"}})
	p := Diff(base, updated)
	var found bool
	for _, s := range p.Steps {
		for _, x := range s.OutsideTx {
			if strings.Contains(x, "CREATE INDEX CONCURRENTLY") && strings.Contains(x, "invoice_paid_at_idx") {
				found = true
			}
		}
		for _, x := range s.InTx {
			if strings.Contains(x, "CONCURRENTLY") {
				t.Fatalf("CONCURRENTLY inside a transaction fails in Postgres: %s", x)
			}
		}
	}
	if !found {
		t.Fatal("a new index must be built concurrently, outside the transaction")
	}
}

func TestSensitiveToggleHasNoSchemaChange(t *testing.T) {
	base := app()
	updated := base.Clone()
	updated.Modules[0].Resources[0].FindField("notes").Sensitive = false
	p := Diff(base, updated)
	if len(p.Steps) != 1 {
		t.Fatalf("want exactly one step, got %d", len(p.Steps))
	}
	if len(p.Steps[0].InTx) != 0 {
		t.Fatalf("a sensitivity toggle must not touch the schema: %v", p.Steps[0].InTx)
	}
}
