package spec_test

import (
	"strings"
	"testing"

	"github.com/Grab-Connected-Tech/ambit/model"
	"github.com/Grab-Connected-Tech/ambit/policy"
	s "github.com/Grab-Connected-Tech/ambit/spec"

	// The reference app's spec packages are the fixtures: importing them is
	// exactly what the am gen dump program does (§3.2).
	"github.com/Grab-Connected-Tech/ambit/example/myapp/app"
	"github.com/Grab-Connected-Tech/ambit/example/myapp/app/billing"
	"github.com/Grab-Connected-Tech/ambit/example/myapp/app/customers"
)

func build(t *testing.T) *model.App {
	t.Helper()
	_ = app.Identity
	_ = billing.Invoice
	_ = customers.Customer
	a, err := s.App()
	if err != nil {
		t.Fatalf("spec.App: %v", err)
	}
	return a
}

func TestReferenceAppModelShape(t *testing.T) {
	a := build(t)
	if a.App.Name == "" || a.App.Name != "app" {
		t.Logf("app name from environment: %q", a.App.Name)
	}
	if a.App.Tenant != "Org" {
		t.Fatalf("tenant resource = %q, want Org", a.App.Tenant)
	}
	if len(a.Roles) != 3 || strings.Join(a.Roles, ",") != "accountant,admin,member" {
		t.Fatalf("roles must be sorted and complete: %v", a.Roles)
	}
	if a.Membership == nil || a.Membership.Table != "am_membership" {
		t.Fatal("the membership resource must be generated from role declarations (§5.1)")
	}
	if a.Account == nil || a.Account.Name != "User" {
		t.Fatalf("account resource: %+v", a.Account)
	}
	if a.Identity.Sessions == nil || a.Identity.Sessions.TTL != "720h0m0s" {
		t.Fatalf("sessions: %+v", a.Identity.Sessions)
	}
	if a.Identity.APITokens == nil || !a.Identity.APITokens.Scoped {
		t.Fatalf("api tokens: %+v", a.Identity.APITokens)
	}
	if len(a.Identity.OIDC) != 1 || a.Identity.OIDC[0].Provider != "google" {
		t.Fatalf("oidc: %+v", a.Identity.OIDC)
	}
	if len(a.Modules) != 2 || a.Modules[0].Name != "billing" || a.Modules[1].Name != "customers" {
		t.Fatalf("modules: %+v", a.Modules)
	}
	for _, m := range a.Modules {
		if len(m.Resources) != 1 {
			t.Fatalf("%s resources: %d", m.Name, len(m.Resources))
		}
	}
}

func TestFieldsCarryStorageAndValidation(t *testing.T) {
	a := build(t)
	inv := a.FindResource("billing.Invoice")
	if inv == nil {
		t.Fatalf("missing billing.Invoice, have %v", resourceNames(a))
	}
	if !inv.Tenant {
		t.Fatal("Invoice is tenant scoped")
	}
	// The five automatic columns are there without being declared (§5.2).
	for _, want := range []string{"id", "created_at", "updated_at", "version", "tenant_id"} {
		if inv.FindField(want) == nil {
			t.Errorf("missing built-in field %q", want)
		}
	}
	amount := inv.FindField("amount")
	if amount == nil || len(amount.Columns) != 2 || amount.Column != "amount_minor" {
		t.Fatalf("money must split into minor and currency columns: %+v", amount)
	}
	status := inv.FindField("status")
	if status == nil || strings.Join(status.Type.Values, ",") != "draft,paid,sent,void" {
		t.Fatalf("enum values must be sorted and stored: %+v", status)
	}
	if status.Default == nil || status.Default.Text != "draft" {
		t.Fatalf("enum default lost: %+v", status.Default)
	}
	notes := inv.FindField("notes")
	if notes == nil || !notes.Sensitive {
		t.Fatal("Sensitive() must survive into the model (§9.1)")
	}
	tax := inv.FindField("tax_id")
	if tax == nil || !tax.Type.Encrypted || !tax.Sensitive {
		t.Fatalf("Encrypted() must wrap the inner type and force Sensitive: %+v", tax)
	}
	cust := inv.FindField("customer")
	if cust == nil || cust.Column != "customer_id" || cust.Ref == nil || cust.Ref.Resource != "customers.Customer" {
		t.Fatalf("ref field: %+v", cust)
	}
	if cust.Ref.OnDelete != "restrict" {
		t.Fatalf("default OnDelete must be restrict: %q", cust.Ref.OnDelete)
	}
	if inv.FindField("sent_at") == nil || !inv.FindField("sent_at").Optional {
		t.Fatal("Optional() lost")
	}
}

func TestIndexesAndInvariants(t *testing.T) {
	a := build(t)
	inv := a.FindResource("billing.Invoice")
	var composite, fk bool
	for _, ix := range inv.Indexes {
		if strings.Join(ix.Fields, ",") == "customer,status" {
			composite = true
		}
		if ix.Name == "customer_id_index" {
			fk = true
		}
	}
	if !composite {
		t.Fatalf("declared index missing: %+v", inv.Indexes)
	}
	if !fk {
		t.Fatalf("every ref must get an index automatically: %+v", inv.Indexes)
	}
	if len(inv.Invariants) != 4 {
		t.Fatalf("invariants: %d", len(inv.Invariants))
	}
	byName := map[string]model.Invariant{}
	for _, v := range inv.Invariants {
		byName[v.Name] = v
	}
	if got := byName["amount_positive"]; got.Enforcement != "check" {
		t.Fatalf("a row-local rule must become a CHECK: %+v", got)
	}
	if got := byName["paid_has_date"]; got.Enforcement != "check" {
		t.Fatalf("an implies over row fields is still a CHECK: %s", policy.Canonical(got.Expr))
	}
	cust := a.FindResource("customers.Customer")
	if cust.FindField("email").Unique == false {
		t.Fatal("Unique() must produce a unique index")
	}
	var unique bool
	for _, ix := range cust.Indexes {
		if ix.Unique && strings.Join(ix.Fields, ",") == "email" {
			unique = true
		}
	}
	if !unique {
		t.Fatalf("missing unique index on email: %+v", cust.Indexes)
	}
}

func TestAccessRulesAreDenyByDefaultAndNormalized(t *testing.T) {
	a := build(t)
	inv := a.FindResource("billing.Invoice")
	if inv.Access.Read == nil || policy.Canonical(inv.Access.Read) != "role[member]" {
		t.Fatalf("read rule: %+v", inv.Access.Read)
	}
	if inv.Access.Delete == nil || policy.Canonical(inv.Access.Delete) != "false" {
		t.Fatalf("Delete(Never()) must be recorded: %v", policy.Canonical(inv.Access.Delete))
	}
	want := `(F(status) IN ["draft","sent"] & role[accountant,admin])`
	if got := policy.Canonical(inv.Access.Update); got != want {
		t.Fatalf("update rule normalization:\n got %s\nwant %s", got, want)
	}
	if len(inv.Access.FieldRead) != 2 {
		t.Fatalf("field read rules must be sorted by field: %+v", inv.Access.FieldRead)
	}
	if inv.Access.FieldRead[0].Field != "notes" || inv.Access.FieldRead[1].Field != "tax_id" {
		t.Fatalf("field rules out of order: %s %s", inv.Access.FieldRead[0].Field, inv.Access.FieldRead[1].Field)
	}
	// App-level resources resolve by qualified name but are not module resources.
	org := a.FindResource("app.Org")
	if org == nil || !org.Tenant {
		t.Fatalf("tenant resource: %+v", org)
	}
	for _, m := range a.Modules {
		for _, r := range m.Resources {
			if r.Name == "Org" || r.Name == "User" {
				t.Fatalf("app-level resource leaked into module %s", m.Name)
			}
		}
	}
}

func TestActionDeclarations(t *testing.T) {
	a := build(t)
	m, send := a.FindAction("billing.SendInvoice")
	if m == nil || send == nil {
		t.Fatalf("actions: %+v", a.Modules)
	}
	if send.Kind != model.KindMutation {
		t.Fatalf("kind %q", send.Kind)
	}
	if send.Requires == nil || policy.Canonical(send.Requires) != "role[accountant,admin]" {
		t.Fatalf("requires: %v", policy.Canonical(send.Requires))
	}
	if len(send.Reads) != 1 || send.Reads[0] != "customers.Customer" {
		t.Fatalf("reads: %v", send.Reads)
	}
	if len(send.Writes) != 1 || send.Writes[0] != "billing.Invoice" {
		t.Fatalf("writes: %v", send.Writes)
	}
	if len(send.Effects) != 1 || send.Effects[0].Type != model.EffectEmail || send.Effects[0].Mode != "deferred" {
		t.Fatalf("effects: %+v", send.Effects)
	}
	if len(send.Errors) != 1 || send.Errors[0].Code != "not_draft" {
		t.Fatalf("errors: %+v", send.Errors)
	}
	if len(send.Limits.RateLimit) != 1 || send.Limits.RateLimit[0].Scope != "actor" || send.Limits.RateLimit[0].Limit != 60 {
		t.Fatalf("rate limit: %+v", send.Limits.RateLimit)
	}
	if send.Input[0].Name != "invoice_id" || send.Input[0].Ref == nil {
		t.Fatalf("input: %+v", send.Input[0])
	}
	if send.Output.Resource != "billing.Invoice" {
		t.Fatalf("output: %+v", send.Output)
	}

	_, mark := a.FindAction("billing.MarkPaid")
	if mark == nil || len(mark.Effects) != 2 || mark.Effects[0].Type != model.EffectWebhook ||
		mark.Effects[1].Mode != "immediate" || mark.Effects[1].Compensates != "billing.RefundCharge" {
		t.Fatalf("markpaid effects: %+v", mark.Effects)
	}
	if !mark.Idempotent || mark.Limits.QueryBudget != 8 {
		t.Fatalf("markpaid limits: %+v", mark.Limits)
	}

	_, list := a.FindAction("billing.ListInvoices")
	if list.Output.Page != "billing.Invoice" {
		t.Fatalf("page output: %+v", list.Output)
	}
	if list.Requires == nil {
		t.Fatal("a query with neither Requires nor Public must be refused by the compiler")
	}

	_, job := a.FindAction("billing.SendReminders")
	if job.Kind != model.KindJob || job.RunAs != s.System {
		t.Fatalf("job: %+v", job)
	}
	if job.Retry == nil || job.Retry.MaxAttempts != 5 || job.Retry.Backoff != "exponential" {
		t.Fatalf("retry: %+v", job.Retry)
	}
	if job.Concurrency == nil || job.Concurrency.Key != "customer" || job.Concurrency.Limit != 1 {
		t.Fatalf("concurrency: %+v", job.Concurrency)
	}
	_, sched := a.FindAction("billing.DailyReminders")
	if sched.Kind != model.KindSchedule || sched.Cron != "0 9 * * *" || sched.Runs != "billing.SendReminders" {
		t.Fatalf("schedule: %+v", sched)
	}
	_, cached := a.FindAction("customers.ListCustomers")
	if cached.Cache == nil || cached.Cache.TTL != "5m0s" || len(cached.Cache.InvalidatedBy) != 1 {
		t.Fatalf("cache: %+v", cached.Cache)
	}
	if len(cached.Input) != 2 {
		t.Fatalf("input fields: %+v", cached.Input)
	}
	if cached.Input[0].Type.Kind != model.KindRef {
		t.Fatalf("FieldOf must inherit the field type: %+v", cached.Input[0].Type)
	}
}

func TestModelIsCanonicalAndStable(t *testing.T) {
	a := build(t)
	first := string(a.Bytes())
	second := string(a.Clone().Bytes())
	if first != second {
		t.Fatal("the same app must produce byte-identical models (§6.1)")
	}
	if a.Hash() != a.Clone().Hash() {
		t.Fatal("hash must be stable")
	}
	// Field order must not matter: reversing a module's field order gives one byte.
	shuffled := a.Clone()
	m := &shuffled.Modules[0]
	if len(m.Resources[0].Fields) > 2 {
		f := m.Resources[0].Fields
		f[0], f[1] = f[1], f[0]
	}
	if shuffled.Hash() != a.Hash() {
		t.Fatal("canonical sorting must make declaration order irrelevant")
	}
}

func resourceNames(a *model.App) []string {
	var out []string
	for _, r := range a.AllResources() {
		out = append(out, r.Ref)
	}
	return out
}
