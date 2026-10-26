package diff

import (
	"strings"
	"testing"

	"keystone.dev/keystone/model"
	"keystone.dev/keystone/policy"
)

func base() *model.App {
	invoice := model.Resource{
		Name: "Invoice", Table: "invoice", Tenant: true, Timestamps: true, Versioned: true,
		Fields: append(model.BuiltInFields(true),
			model.Field{Name: "status", GoName: "Status", Column: "status", Type: model.Type{Kind: model.KindEnum, Values: []string{"draft", "paid", "sent", "void"}}},
			model.Field{Name: "notes", GoName: "Notes", Column: "notes", Optional: true, Type: model.Type{Kind: model.KindText, MaxLen: intp(2000)}}),
		Invariants: []model.Invariant{{Name: "status_known", Enforcement: "check",
			Expr: policy.Normalize(policy.Cmp(policy.OpIn, policy.FieldRef("status"),
				policy.Literal(policy.ListLit("string", []policy.Lit{policy.StringLit("draft"), policy.StringLit("sent"), policy.StringLit("paid"), policy.StringLit("void")}))))}},
		Access: model.Access{
			Read:      policy.Role("member"),
			Update:    policy.And(policy.Or(policy.Role("admin"), policy.Role("accountant")), policy.Cmp(policy.OpIn, policy.FieldRef("status"), policy.Literal(policy.ListLit("string", []policy.Lit{policy.StringLit("draft"), policy.StringLit("sent")})))),
			Delete:    policy.Never(),
			FieldRead: []model.FieldRule{{Field: "notes", Expr: policy.And(policy.Role("admin"), policy.Role("accountant"))}},
		},
	}
	return &model.App{
		SchemaVersion: model.SchemaVersion,
		App:           model.AppInfo{Name: "myapp", Tenant: "Org"},
		Roles:         []string{"accountant", "admin", "member"},
		Tenant:        &model.Tenant{Name: "Org", Table: "org", Fields: model.BuiltInFields(false)},
		Membership:    &model.Tenant{Name: "Membership", Table: "ks_membership"},
		Identity:      model.Identity{Sessions: &model.Sessions{TTL: "720h0m0s", Rotate: true}, APITokens: &model.APITokens{Scoped: true}},
		Defaults:      model.Defaults{QueryBudget: 20, RowBudget: 10000, ActionTimeout: "10s", Compatibility: ""},
		Modules: []model.Module{{
			Name:      "billing",
			Resources: []model.Resource{invoice},
			Actions: []model.Action{
				{Name: "SendInvoice", Kind: model.KindMutation, Requires: policy.Role("admin"),
					Writes:  []string{"billing.Invoice"},
					Effects: []model.Effect{{Type: model.EffectEmail, Mode: "deferred"}},
					Limits:  model.ActionLimits{RateLimit: []model.RateLimit{{Scope: "actor", Limit: 60, Per: "1m0s"}}},
					Output:  model.Output{Resource: "billing.Invoice"}},
				{Name: "ListInvoices", Kind: model.KindQuery, Requires: policy.Role("member"),
					Reads: []string{"billing.Invoice"}, Output: model.Output{Page: "billing.Invoice"}},
			},
		}},
	}
}

func intp(i int) *int { return &i }

func find(t *testing.T, r Report, want Change) *Change {
	t.Helper()
	for i := range r.Changes {
		c := r.Changes[i]
		if c.Category == want.Category && c.Subject == want.Subject && strings.Contains(c.Detail, want.Detail) {
			if want.Risk != 0 && c.Risk != want.Risk {
				t.Fatalf("%s %s: risk %s want %s", c.Category, c.Subject, c.Risk, want.Risk)
			}
			return &r.Changes[i]
		}
	}
	var all []string
	for _, c := range r.Changes {
		all = append(all, c.Risk.String()+" "+c.Category+" "+c.Subject+" "+c.Detail)
	}
	t.Fatalf("missing change %s/%s/%s, got:\n  %s", want.Category, want.Subject, want.Detail, strings.Join(all, "\n  "))
	return nil
}

// TestSpecExampleRows walks the table in §15.1 and checks each named change lands in
// the risk column the spec assigns it.
func TestSpecExampleRows(t *testing.T) {
	cases := []struct {
		name string
		mut  func(a *model.App)
		want Change
	}{
		{"access widened", func(a *model.App) {
			a.Modules[0].Resources[0].Access.Update = policy.Role("member")
		}, Change{Risk: High, Category: CatAccess, Subject: "billing.Invoice.update", Detail: "widened"}},
		{"cross tenant", func(a *model.App) {
			a.Modules[0].Actions[0].CrossTenant = true
		}, Change{Risk: High, Category: CatAccess, Subject: "billing.SendInvoice", Detail: "CrossTenant() added"}},
		{"public action", func(a *model.App) {
			a.Modules[0].Actions[1].Public = true
			a.Modules[0].Actions[1].Requires = nil
		}, Change{Risk: High, Category: CatAccess, Subject: "billing.ListInvoices", Detail: "became public"}},
		{"field read widened", func(a *model.App) {
			a.Modules[0].Resources[0].Access.FieldRead[0].Expr = policy.Role("member")
		}, Change{Risk: Medium, Category: CatAccess, Subject: "billing.Invoice.notes.read", Detail: "widened"}},
		{"rule narrowed", func(a *model.App) {
			a.Modules[0].Resources[0].Access.Read = policy.Role("admin")
		}, Change{Risk: High, Category: CatAccess, Subject: "billing.Invoice.read", Detail: "widened"}},
		{"new writing action", func(a *model.App) {
			a.Modules[0].Actions = append(a.Modules[0].Actions, model.Action{
				Name: "VoidInvoice", Kind: model.KindMutation, Requires: policy.Role("admin"), Writes: []string{"billing.Invoice"}})
		}, Change{Risk: High, Category: CatAction, Subject: "billing.VoidInvoice", Detail: "new action that writes"}},
		{"new query action", func(a *model.App) {
			a.Modules[0].Actions = append(a.Modules[0].Actions, model.Action{
				Name: "ListOverdue", Kind: model.KindQuery, Requires: policy.Role("member"), Reads: []string{"billing.Invoice"}})
		}, Change{Risk: Low, Category: CatAction, Subject: "billing.ListOverdue", Detail: "new query action"}},
		{"new http host", func(a *model.App) {
			a.Modules[0].Actions[0].Effects = append(a.Modules[0].Actions[0].Effects,
				model.Effect{Type: model.EffectHTTP, Mode: "deferred", Hosts: []string{"api.payments.example.com"}})
		}, Change{Risk: High, Category: CatEffect, Subject: "billing.SendInvoice", Detail: "new HTTP host"}},
		{"effect made immediate", func(a *model.App) {
			a.Modules[0].Actions[0].Effects[0].Mode = "immediate"
		}, Change{Risk: High, Category: CatEffect, Subject: "billing.SendInvoice", Detail: "effect made immediate"}},
		{"effect removed", func(a *model.App) {
			a.Modules[0].Actions[0].Effects = nil
		}, Change{Risk: Low, Category: CatEffect, Subject: "billing.SendInvoice", Detail: "effect removed"}},
		{"field removed", func(a *model.App) {
			res := &a.Modules[0].Resources[0]
			var keep []model.Field
			for _, f := range res.Fields {
				if f.Name != "notes" {
					keep = append(keep, f)
				}
			}
			res.Fields = keep
		}, Change{Risk: High, Category: CatData, Subject: "billing.Invoice.notes", Detail: "field removed"}},
		{"field added", func(a *model.App) {
			a.Modules[0].Resources[0].Fields = append(a.Modules[0].Resources[0].Fields,
				model.Field{Name: "due_at", GoName: "DueAt", Column: "due_at", Optional: true, Type: model.Type{Kind: model.KindTimestamp}})
		}, Change{Risk: Low, Category: CatData, Subject: "billing.Invoice.due_at", Detail: "field added"}},
		{"invariant removed", func(a *model.App) {
			a.Modules[0].Resources[0].Invariants = nil
		}, Change{Risk: Medium, Category: CatData, Subject: "billing.Invoice.status_known", Detail: "invariant removed"}},
		{"rate limit removed", func(a *model.App) {
			a.Modules[0].Actions[0].Limits.RateLimit = nil
		}, Change{Risk: High, Category: CatLimit, Subject: "billing.SendInvoice", Detail: "rate limit removed"}},
		{"rate limit raised hard", func(a *model.App) {
			a.Modules[0].Actions[0].Limits.RateLimit[0].Limit = 2000
		}, Change{Risk: High, Category: CatLimit, Subject: "billing.SendInvoice", Detail: "raised 60 -> 2000"}},
		{"query budget raised", func(a *model.App) {
			a.Modules[0].Actions[0].Limits.QueryBudget = 200
		}, Change{Risk: High, Category: CatLimit, Subject: "billing.SendInvoice", Detail: "query budget 20 -> 200"}},
		{"timeout raised", func(a *model.App) {
			a.Modules[0].Actions[0].Limits.Timeout = "30s"
		}, Change{Risk: Medium, Category: CatLimit, Subject: "billing.SendInvoice", Detail: "timeout"}},
		{"session ttl lengthened", func(a *model.App) {
			a.Identity.Sessions.TTL = "7200h0m0s"
		}, Change{Risk: High, Category: CatIdent, Subject: "identity.sessions", Detail: "session settings changed"}},
		{"role removed", func(a *model.App) {
			a.Roles = []string{"accountant", "admin"}
		}, Change{Risk: High, Category: CatIdent, Subject: "role member", Detail: "role removed"}},
		{"action removed is breaking api", func(a *model.App) {
			a.Modules[0].Actions = a.Modules[0].Actions[:1]
		}, Change{Risk: High, Category: CatAPI, Subject: "billing.ListInvoices", Detail: "breaking: action removed"}},
		{"new required input is breaking", func(a *model.App) {
			a.Modules[0].Actions[0].Input = append(a.Modules[0].Actions[0].Input,
				model.Field{Name: "reason", GoName: "Reason", Column: "reason", Type: model.Type{Kind: model.KindText}})
		}, Change{Risk: High, Category: CatAPI, Subject: "billing.SendInvoice", Detail: "new required input"}},
		{"new optional input is compatible", func(a *model.App) {
			a.Modules[0].Actions[0].Input = append(a.Modules[0].Actions[0].Input,
				model.Field{Name: "memo", GoName: "Memo", Column: "memo", Optional: true, Type: model.Type{Kind: model.KindText}})
		}, Change{Risk: Low, Category: CatAPI, Subject: "billing.SendInvoice", Detail: "compatible addition"}},
	}
	for _, tc := range cases {
		b := base()
		u := base()
		tc.mut(u)
		r := Between(b, u, model.Facts{}, model.Facts{})
		find(t, r, tc.want)
	}
}

func TestCodeFactsAreHighRisk(t *testing.T) {
	b, u := base(), base()
	bf := model.Facts{}
	hf := model.Facts{
		Unsafe: []model.UnsafeFact{{File: "app/billing/actions/send_invoice.go", Line: 22, Module: "billing", Action: "SendInvoice", Kind: "raw_sql", Reason: "legacy bulk insert"}},
		Allows: []model.AllowFact{{Rule: "KS-DATA-002", File: "app/billing/actions/send_reminders.go", Line: 18, Reason: "batch API is faster"}},
		Deps:   []model.Dep{{Path: "github.com/stripe/stripe-go/v78", Version: "v78.2.0"}},
	}
	r := Between(b, u, bf, hf)
	if r.Unsafe != 1 || r.Allow != 1 || r.NewDeps != 1 {
		t.Fatalf("counts: %+v", r)
	}
	find(t, r, Change{Risk: High, Category: CatCode, Subject: "unsafe blocks", Detail: "1 new raw-SQL"})
	find(t, r, Change{Risk: High, Category: CatCode, Subject: "suppressions", Detail: "1 new //ks:allow"})
	find(t, r, Change{Risk: High, Category: CatDep, Subject: "github.com/stripe/stripe-go/v78", Detail: "new dependency"})
	if r.Count(High) < 3 {
		t.Fatalf("expected at least three high rows, got %d", r.Count(High))
	}
}

func TestMinorAndPatchBumps(t *testing.T) {
	b, u := base(), base()
	bf := model.Facts{Deps: []model.Dep{{Path: "github.com/jackc/pgx/v5", Version: "v5.7.0"}}}
	h1 := model.Facts{Deps: []model.Dep{{Path: "github.com/jackc/pgx/v5", Version: "v5.8.0"}}}
	h2 := model.Facts{Deps: []model.Dep{{Path: "github.com/jackc/pgx/v5", Version: "v6.0.0"}}}
	h3 := model.Facts{Deps: []model.Dep{{Path: "github.com/jackc/pgx/v5", Version: "v5.7.1"}}}
	r1 := Between(b, u, bf, h1)
	if got := r1.Changes[0].Risk; got != Medium {
		t.Fatalf("minor bump = %s", got)
	}
	r2 := Between(b, u, bf, h2)
	if got := r2.Changes[0].Risk; got != High {
		t.Fatalf("major bump = %s", got)
	}
	r3 := Between(b, u, bf, h3)
	if got := r3.Changes[0].Risk; got != Low {
		t.Fatalf("patch bump = %s", got)
	}
}

func TestNarrowingThatCanBeProved(t *testing.T) {
	b, u := base(), base()
	b.Modules[0].Resources[0].Access.Read = policy.Or(policy.Role("admin"), policy.Role("member"))
	u.Modules[0].Resources[0].Access.Read = policy.Role("admin")
	r := Between(b, u, model.Facts{}, model.Facts{})
	find(t, r, Change{Risk: Medium, Category: CatAccess, Subject: "billing.Invoice.read", Detail: "narrowed"})
}

func TestNoChangeIsSilent(t *testing.T) {
	b, u := base(), base()
	r := Between(b, u, model.Facts{}, model.Facts{})
	if !r.Empty() {
		t.Fatalf("an unchanged app must report nothing:\n%s", r.Text())
	}
	if got := r.Text(); !strings.Contains(got, "no behavior change") {
		t.Fatalf("text output: %q", got)
	}
}

func TestTextMatchesTheSpecExample(t *testing.T) {
	b, u := base(), base()
	u.Modules[0].Resources[0].Access.Update = policy.Or(policy.Role("admin"), policy.Role("accountant"))
	u.Modules[0].Actions[0].Effects = append(u.Modules[0].Actions[0].Effects,
		model.Effect{Type: model.EffectHTTP, Mode: "deferred", Hosts: []string{"api.payments.example.com"}})
	u.Modules[0].Resources[0].Fields = append(u.Modules[0].Resources[0].Fields,
		model.Field{Name: "chased_at", GoName: "ChasedAt", Column: "chased_at", Type: model.Type{Kind: model.KindTimestamp}})
	u.Modules[0].Actions = append(u.Modules[0].Actions, model.Action{
		Name: "ListOverdue", Kind: model.KindQuery, Requires: policy.Role("member"), Reads: []string{"billing.Invoice"}})
	r := Between(b, u, model.Facts{}, model.Facts{})
	text := r.Text()
	for _, want := range []string{
		"HIGH  access      ",
		"MED   data        ",
		"LOW   action      ",
		"changes · ",
		"high · ",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("text output missing %q:\n%s", want, text)
		}
	}
	if !strings.HasPrefix(text, "HIGH") {
		t.Fatalf("the highest risk must come first:\n%s", text)
	}
	if strings.Contains(text, "0 changes") {
		t.Fatalf("summary is wrong:\n%s", text)
	}
}

func TestMarkdownAndJSON(t *testing.T) {
	b, u := base(), base()
	u.Modules[0].Resources[0].Access.Read = policy.AnyNode()
	r := Between(b, u, model.Facts{}, model.Facts{})
	md := Render(r, Markdown)
	if !strings.Contains(md, "| HIGH |") || !strings.Contains(md, "code-owner approval") {
		t.Fatalf("markdown:\n%s", md)
	}
	j := Render(r, JSON)
	if !strings.Contains(j, `"risk": "high"`) || !strings.Contains(j, `"category": "access"`) {
		t.Fatalf("json:\n%s", j)
	}
}

func TestReportsAreDeterministic(t *testing.T) {
	b, u := base(), base()
	u.Modules[0].Resources[0].Access.Read = policy.Role("admin")
	u.Modules[0].Actions[0].Effects[0].Mode = "immediate"
	first := Between(b, u, model.Facts{}, model.Facts{}).JSON()
	for i := 0; i < 25; i++ {
		if got := Between(b, u, model.Facts{}, model.Facts{}).JSON(); got != first {
			t.Fatalf("report changed on run %d", i)
		}
	}
}

func TestUnknownBaseIsAllHighRisk(t *testing.T) {
	// A first release has no base model: everything new that writes must be visible.
	u := base()
	r := Between(nil, u, model.Facts{}, model.Facts{})
	find(t, r, Change{Risk: High, Category: CatAction, Subject: "billing.SendInvoice", Detail: "new action that writes"})
	find(t, r, Change{Risk: Low, Category: CatAction, Subject: "billing.ListInvoices", Detail: "new query action"})
}
