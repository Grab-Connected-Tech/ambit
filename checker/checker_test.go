package checker

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Grab-Connected-Tech/ambit/compile"
	"github.com/Grab-Connected-Tech/ambit/model"
	"github.com/Grab-Connected-Tech/ambit/policy"
)

// app is the model the analyzers reason about: one module, one resource with a
// sensitive field and an enum, and two declared actions.
func testApp() *model.App {
	return &model.App{
		SchemaVersion: model.SchemaVersion,
		App:           model.AppInfo{Name: "myapp", Tenant: "Org"},
		Roles:         []string{"admin", "member"},
		Tenant:        &model.Tenant{Name: "Org", Table: "org", Fields: model.BuiltInFields(false)},
		Membership:    &model.Tenant{Name: "Membership", Table: "am_membership"},
		Modules: []model.Module{{
			Name: "billing",
			Resources: []model.Resource{{
				Name:   "Invoice",
				Table:  "invoice",
				Tenant: true,
				Fields: append(model.BuiltInFields(true),
					model.Field{Name: "notes", GoName: "Notes", Column: "notes", Sensitive: true, Type: model.Type{Kind: model.KindText}},
					model.Field{Name: "status", GoName: "Status", Column: "status", Type: model.Type{Kind: model.KindEnum, Values: []string{"draft", "paid", "sent", "void"}}}),
				Access: model.Access{Read: policy.Role("member")},
			}},
			Actions: []model.Action{
				{Name: "SendInvoice", Kind: model.KindMutation, Requires: policy.Role("admin"), Writes: []string{"billing.Invoice"}},
				{Name: "ListInvoices", Kind: model.KindQuery, Requires: policy.Role("member"), Reads: []string{"billing.Invoice"}},
			},
		}},
	}
}

// tree writes an app folder from a map of relative path to contents.
func tree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for path, content := range files {
		full := filepath.Join(root, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func check(t *testing.T, files map[string]string) []compile.Diag {
	t.Helper()
	root := tree(t, files)
	res, err := Run(Config{Root: root, ModulePath: "myapp", App: testApp(), AllowedImports: []string{"github.com/shopspring/decimal"}})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	return res.Diags
}

func has(t *testing.T, d []compile.Diag, rule string) *compile.Diag {
	t.Helper()
	for i := range d {
		if d[i].Rule == rule {
			return &d[i]
		}
	}
	t.Fatalf("no %s in:\n%s", rule, show(d))
	return nil
}

func lacks(t *testing.T, d []compile.Diag, rule string) {
	t.Helper()
	for _, x := range d {
		if x.Rule == rule {
			t.Fatalf("unexpected %s: %s (%s)", rule, x.Message, x.File)
		}
	}
}

func show(d []compile.Diag) string {
	var b strings.Builder
	for _, x := range d {
		fmt.Fprintf(&b, "  %s %s %s:%d %s\n", x.Severity, x.Rule, x.File, x.Line, x.Message)
	}
	return b.String()
}

const goodAction = `package actions

import (
	"github.com/Grab-Connected-Tech/ambit/am"
	"myapp/app/billing/gen"
)

func SendInvoice(c *gen.SendInvoiceCtx, in gen.SendInvoiceInput) (*gen.Invoice, error) {
	inv, err := c.DB.Invoices.Get(c, in.InvoiceID, gen.InvoiceWithCustomer)
	if err != nil {
		return nil, err
	}
	if inv.Status != gen.InvoiceStatusDraft {
		return nil, gen.ErrNotDraft()
	}
	out, err := c.DB.Invoices.Update(c, inv.ID, inv.Version, gen.InvoicePatch{Status: am.Set(gen.InvoiceStatusSent)})
	if err != nil {
		return nil, err
	}
	c.Email.Send(gen.EmailArgs{To: inv.Customer.Email})
	return out, nil
}

func ListInvoices(c *gen.ListInvoicesCtx, in gen.ListInvoicesInput) (*gen.InvoicePage, error) {
	return c.DB.Invoices.Query().Where(gen.Invoices.Status.Eq(gen.InvoiceStatusSent)).Page(c, in.Cursor, 50)
}
`

const goodTest = `package actions_test

import (
	"testing"

	"github.com/Grab-Connected-Tech/ambit/amtest"
	"myapp/app/billing/actions"
	"myapp/app/billing/gen"
)

func TestSendInvoice(t *testing.T) {
	env := amtest.New(t)
	out, err := env.Call(env.Admin(), actions.SendInvoice, gen.SendInvoiceInput{})
	if err != nil {
		t.Fatal(err)
	}
	_ = out
}

func TestListInvoices(t *testing.T) {
	env := amtest.New(t)
	_, _ = env.Call(env.Member(), actions.ListInvoices, gen.ListInvoicesInput{})
}
`

func specFiles(extra map[string]string) map[string]string {
	base := map[string]string{
		"app/app.go": `package app

import s "github.com/Grab-Connected-Tech/ambit/spec"

var Org = s.TenantResource("Org", s.Fields(s.Text("name").MaxLen(200)))
var Admin = s.Role("admin")
var Member = s.Role("member")
`,
		"app/billing/spec.go": `package billing

import (
	s "github.com/Grab-Connected-Tech/ambit/spec"
	"myapp/app"
)

var Invoice = s.Resource("Invoice",
	s.Tenant(),
	s.Fields(s.Text("notes").Optional().Sensitive(), s.Enum("status", "draft", "sent", "paid", "void")),
	s.Access(s.Read(app.Member)),
)

var SendInvoice = s.Mutation("SendInvoice",
	s.Requires(app.Admin),
	s.Writes(Invoice),
)

var ListInvoices = s.Query("ListInvoices",
	s.Requires(app.Member),
	s.Reads(Invoice),
)
`,
		"app/billing/actions/send_invoice.go":      goodAction,
		"app/billing/actions/send_invoice_test.go": goodTest,
	}
	for k, v := range extra {
		if v == "" {
			delete(base, k)
			continue
		}
		base[k] = v
	}
	return base
}

func TestCleanAppReportsNothing(t *testing.T) {
	d := check(t, specFiles(nil))
	if len(d) != 0 {
		t.Fatalf("a well-formed app must be silent:\n%s", show(d))
	}
}

func TestBannedImports(t *testing.T) {
	files := specFiles(map[string]string{
		"app/billing/actions/send_invoice.go": `package actions

import (
	"net/http"
	"os"
	"database/sql"
	"reflect"
	_ "unsafe"
	_ "os/exec"
	_ "github.com/jackc/pgx/v5"
)

var _ = http.Get
var _ = os.Getenv
var _ = sql.Open
var _ = reflect.TypeOf
`,
	})
	d := check(t, files)
	for _, r := range []string{"AM-SEC-001"} {
		has(t, d, r)
	}
	n := 0
	for _, x := range d {
		if x.Rule == "AM-SEC-001" {
			n++
		}
	}
	if n < 6 {
		t.Fatalf("want a diagnostic per banned import, got %d:\n%s", n, show(d))
	}
}

func TestUnknownThirdPartyImportIsNamed(t *testing.T) {
	files := specFiles(map[string]string{
		"app/billing/actions/send_invoice.go": `package actions

import (
	_ "github.com/rusty/hazard"
	_ "github.com/shopspring/decimal"
)
`,
	})
	d := check(t, files)
	got := has(t, d, "AM-SEC-001")
	if !strings.Contains(got.Message, "rusty/hazard") {
		t.Fatalf("the message must name the import: %s", got.Message)
	}
	for _, x := range d {
		if strings.Contains(x.Message, "shopspring") {
			t.Fatal("an allowedImports entry must pass")
		}
	}
}

func TestCrossModuleImportIsRefused(t *testing.T) {
	files := specFiles(map[string]string{
		"app/billing/actions/send_invoice.go": `package actions

import _ "myapp/app/customers/actions"
`,
	})
	has(t, check(t, files), "AM-CODE-002")
}

func TestEnvReadOutsideConfig(t *testing.T) {
	files := specFiles(map[string]string{
		"app/billing/actions/send_invoice.go": `package actions

import "os"

func SendInvoice(c *gen.SendInvoiceCtx, in gen.SendInvoiceInput) (*gen.Invoice, error) {
	key := os.Getenv("STRIPE_KEY")
	_ = key
	return nil, nil
}

func ListInvoices(c *gen.ListInvoicesCtx, in gen.ListInvoicesInput) (*gen.InvoicePage, error) {
	return c.DB.Invoices.Query().First(c)
}
`,
	})
	d := check(t, files)
	has(t, d, "AM-SEC-002")
}

func TestUnsafeNeedsAReason(t *testing.T) {
	files := specFiles(map[string]string{
		"app/billing/actions/blank.go": `package actions

func Blank(c *gen.SendInvoiceCtx) error {
	err := am.Unsafe(c, "", func(tx am.RawTx) error { return nil })
	return err
}
`,
	})
	has(t, check(t, files), "AM-SEC-003")

	files = specFiles(map[string]string{
		"app/billing/actions/blank.go": `package actions

func Blank(c *gen.SendInvoiceCtx, why string) error {
	return am.Unsafe(c, why, func(tx am.RawTx) error { return nil })
}
`,
	})
	d := check(t, files)
	got := has(t, d, "AM-SEC-003")
	if !strings.Contains(got.Message, "constant string") {
		t.Fatalf("a computed reason must be called out: %s", got.Message)
	}

	// With a reason it passes, and the block becomes a code fact for the diff.
	files = specFiles(map[string]string{
		"app/billing/actions/blank.go": `package actions

func Blank(c *gen.SendInvoiceCtx) error {
	return am.Unsafe(c, "bulk import of legacy rows", func(tx am.RawTx) error { return nil })
}
`,
	})
	root := tree(t, files)
	res, err := Run(Config{Root: root, ModulePath: "myapp", App: testApp()})
	if err != nil {
		t.Fatal(err)
	}
	lacks(t, res.Diags, "AM-SEC-003")
	if len(res.Facts.Unsafe) != 1 || !strings.Contains(res.Facts.Unsafe[0].Reason, "legacy") {
		t.Fatalf("the unsafe block must be recorded as a fact: %+v", res.Facts.Unsafe)
	}
}

func TestSensitiveValueInALog(t *testing.T) {
	files := specFiles(map[string]string{
		"app/billing/actions/send_invoice.go": `package actions

func SendInvoice(c *gen.SendInvoiceCtx, in gen.SendInvoiceInput) (*gen.Invoice, error) {
	inv, err := c.DB.Invoices.Get(c, in.InvoiceID, nil)
	if err != nil {
		return nil, err
	}
	c.Log.Info("invoice", "notes", inv.Notes)
	return inv, nil
}

func ListInvoices(c *gen.ListInvoicesCtx, in gen.ListInvoicesInput) (*gen.InvoicePage, error) {
	return c.DB.Invoices.Query().First(c)
}
`,
	})
	d := check(t, files)
	got := has(t, d, "AM-SEC-004")
	if !strings.Contains(got.Message, "billing.Invoice.notes") {
		t.Fatalf("the message must name the field: %s", got.Message)
	}
}

func TestQueryNeedsATerminalBound(t *testing.T) {
	files := specFiles(map[string]string{
		"app/billing/actions/list_invoices.go": `package actions

func ListInvoices(c *gen.ListInvoicesCtx, in gen.ListInvoicesInput) (*gen.InvoicePage, error) {
	rows, err := c.DB.Invoices.Query().Where(gen.Invoices.Status.Eq(gen.InvoiceStatusSent)).All(c)
	return rows, err
}
`,
	})
	has(t, check(t, files), "AM-DATA-001")
}

func TestScopedCallInALoop(t *testing.T) {
	inLoop := `package actions

func ListInvoices(c *gen.ListInvoicesCtx, in gen.ListInvoicesInput) (*gen.InvoicePage, error) {
	page, err := c.DB.Invoices.Query().Page(c, in.Cursor, 50)
	if err != nil {
		return nil, err
	}
	for _, inv := range page.Rows {
		cust, err := c.DB.Customers.Get(c, inv.CustomerID, nil)
		if err != nil {
			return nil, err
		}
		_ = cust
	}
	return page, nil
}
`
	d := check(t, specFiles(map[string]string{"app/billing/actions/list_invoices.go": inLoop}))
	got := has(t, d, "AM-DATA-002")
	if !strings.Contains(got.Fix, "With(") {
		t.Fatalf("the fix must name the batched alternative: %s", got.Fix)
	}

	// The .With form must be clean.
	fixed := strings.Replace(inLoop, `	page, err := c.DB.Invoices.Query().Page(c, in.Cursor, 50)`,
		`	page, err := c.DB.Invoices.Query().With(gen.InvoiceWithCustomer).Page(c, in.Cursor, 50)`, 1)
	fixed = strings.Replace(fixed, `		cust, err := c.DB.Customers.Get(c, inv.CustomerID, nil)
		if err != nil {
			return nil, err
		}
		_ = cust`, `		cust := inv.Customer`, 1)
	d2 := check(t, specFiles(map[string]string{"app/billing/actions/list_invoices.go": fixed}))
	lacks(t, d2, "AM-DATA-002")
}

func TestIgnoredError(t *testing.T) {
	files := specFiles(map[string]string{
		"app/billing/actions/send_invoice.go": `package actions

func SendInvoice(c *gen.SendInvoiceCtx, in gen.SendInvoiceInput) (*gen.Invoice, error) {
	c.DB.Invoices.Delete(c, in.InvoiceID, 1)
	return nil, nil
}

func ListInvoices(c *gen.ListInvoicesCtx, in gen.ListInvoicesInput) (*gen.InvoicePage, error) {
	return c.DB.Invoices.Query().First(c)
}
`,
	})
	has(t, check(t, files), "AM-DATA-003")
}

func TestWallClockTime(t *testing.T) {
	files := specFiles(map[string]string{
		"app/billing/actions/send_invoice.go": `package actions

func SendInvoice(c *gen.SendInvoiceCtx, in gen.SendInvoiceInput) (*gen.Invoice, error) {
	if time.Since(in.When) > 24*time.Hour {
		return nil, nil
	}
	now := time.Now()
	_ = now
	return nil, nil
}

func ListInvoices(c *gen.ListInvoicesCtx, in gen.ListInvoicesInput) (*gen.InvoicePage, error) {
	return c.DB.Invoices.Query().First(c)
}
`,
		"app/billing/actions/imports.go": "package actions\n\nimport \"time\"\n\nvar _ = time.Now\n",
	})
	n := 0
	for _, x := range check(t, files) {
		if x.Rule == "AM-CODE-004" {
			n++
		}
	}
	if n < 2 {
		t.Fatalf("want both time.Now and time.Since flagged, got %d", n)
	}
}

func TestGoStatement(t *testing.T) {
	files := specFiles(map[string]string{
		"app/billing/actions/send_invoice.go": `package actions

func SendInvoice(c *gen.SendInvoiceCtx, in gen.SendInvoiceInput) (*gen.Invoice, error) {
	go notify(in.InvoiceID)
	return nil, nil
}
func notify(x any) {}

func ListInvoices(c *gen.ListInvoicesCtx, in gen.ListInvoicesInput) (*gen.InvoicePage, error) {
	return c.DB.Invoices.Query().First(c)
}
`,
	})
	got := has(t, check(t, files), "AM-PERF-003")
	if !strings.Contains(got.Fix, "Enqueue") {
		t.Fatalf("the fix must point at Enqueue: %s", got.Fix)
	}
}

func TestSpecFileMustStayDeclarative(t *testing.T) {
	files := specFiles(map[string]string{
		"app/billing/spec.go": `package billing

import (
	"os"

	s "github.com/Grab-Connected-Tech/ambit/spec"
	"myapp/app"
)

func init() {
	if os.Getenv("STAGING") != "" {
		Admin = s.Role("admin")
	}
}

var Admin = s.Role("admin")

var Invoice = s.Resource("Invoice",
	s.Tenant(),
	s.Fields(func() []*s.Field { return nil }()),
)

var SendInvoice = s.Mutation("SendInvoice", s.Requires(app.Admin))
`,
	})
	d := check(t, files)
	has(t, d, "AM-SPEC-001")
}

func TestMissingImplementation(t *testing.T) {
	d := check(t, specFiles(map[string]string{"app/billing/actions/send_invoice.go": ""}))
	got := has(t, d, "AM-CODE-005")
	if !strings.Contains(got.Message, "SendInvoice") {
		t.Fatalf("the message must name the action: %s", got.Message)
	}
}

func TestActionWithoutTestWarns(t *testing.T) {
	d := check(t, specFiles(map[string]string{"app/billing/actions/send_invoice_test.go": ""}))
	got := has(t, d, "AM-TEST-001")
	if got.Severity != compile.SevWarning {
		t.Fatalf("a missing test is a warning: %s", got.Severity)
	}
}

func TestNonExhaustiveEnumSwitch(t *testing.T) {
	files := specFiles(map[string]string{
		"app/billing/actions/send_invoice.go": `package actions

func SendInvoice(inv *gen.Invoice) string {
	switch inv.Status {
	case gen.InvoiceStatusDraft:
		return "draft"
	}
	return ""
}

func ListInvoices(c *gen.ListInvoicesCtx, in gen.ListInvoicesInput) (*gen.InvoicePage, error) {
	return c.DB.Invoices.Query().First(c)
}
`,
	})
	got := has(t, check(t, files), "AM-CODE-003")
	if !strings.Contains(got.Message, "1 of 4") {
		t.Fatalf("the message must count the values: %s", got.Message)
	}
}

func TestGeneratedFileEditIsCaught(t *testing.T) {
	// Build a generated file, hash it the way the generator would, then edit it.
	body := "package gen\n\nfunc Hello() string { return \"hi\" }\n"
	marker := "// Code generated by am. DO NOT EDIT.\n// am:hash " + contentHash(body) + "\n\n"
	clean := marker + body
	files := specFiles(map[string]string{"app/billing/gen/invoice.gen.go": clean})
	lacks(t, check(t, files), "AM-CODE-001")

	edited := strings.Replace(clean, `"hi"`, `"hello"`, 1)
	files = specFiles(map[string]string{"app/billing/gen/invoice.gen.go": edited})
	got := has(t, check(t, files), "AM-CODE-001")
	if !strings.Contains(got.Message, "edited") {
		t.Fatalf("message: %s", got.Message)
	}
}

// contentHash mirrors the generator's marker hash.
func contentHash(body string) string {
	sum := sha256.Sum256([]byte(normalizeForHash(body)))
	return hex.EncodeToString(sum[:])[:16]
}

func TestSuppressionNeedsAReasonAndIsRecorded(t *testing.T) {
	bare := specFiles(map[string]string{
		"app/billing/actions/send_invoice.go": `package actions

func SendInvoice(c *gen.SendInvoiceCtx, in gen.SendInvoiceInput) (*gen.Invoice, error) {
	//am:allow AM-DATA-002
	for _, id := range in.IDs {
		_, err := c.DB.Invoices.Get(c, id, nil)
		if err != nil {
			return nil, err
		}
	}
	return nil, nil
}

func ListInvoices(c *gen.ListInvoicesCtx, in gen.ListInvoicesInput) (*gen.InvoicePage, error) {
	return c.DB.Invoices.Query().First(c)
}
`,
	})
	has(t, check(t, bare), "AM-SEC-003")

	explained := strings.Replace(bare["app/billing/actions/send_invoice.go"],
		"//am:allow AM-DATA-002", "//am:allow AM-DATA-002 the ids list is capped at 10 by input validation", 1)
	files := specFiles(map[string]string{"app/billing/actions/send_invoice.go": explained})
	root := tree(t, files)
	res, err := Run(Config{Root: root, ModulePath: "myapp", App: testApp()})
	if err != nil {
		t.Fatal(err)
	}
	lacks(t, res.Diags, "AM-DATA-002")
	lacks(t, res.Diags, "AM-SEC-003")
	if len(res.Facts.Allows) != 1 || res.Facts.Allows[0].Reason == "" {
		t.Fatalf("a suppression must be recorded as a fact: %+v", res.Facts.Allows)
	}
}

func TestFactsCollectImportsAndDeps(t *testing.T) {
	root := tree(t, specFiles(nil))
	res, err := Run(Config{Root: root, ModulePath: "myapp", App: testApp(),
		GoMod: "module myapp\n\ngo 1.24\n\nrequire (\n\tgithub.com/jackc/pgx/v5 v5.11.0\n\tgithub.com/shopspring/decimal v1.4.0\n)\n"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Facts.Deps) != 2 {
		t.Fatalf("deps: %+v", res.Facts.Deps)
	}
	if res.Facts.ModelHash != testApp().Hash() {
		t.Fatal("facts must pin the model they were extracted from")
	}
	found := false
	for _, imp := range res.Facts.Imports {
		if strings.Contains(imp, "gen") {
			found = true
		}
	}
	if !found {
		t.Fatalf("imports: %v", res.Facts.Imports)
	}
}

func TestModuleOfHandlesEveryLayout(t *testing.T) {
	cases := map[string]string{
		"app/billing/actions/x.go":            "billing",
		"app/billing/gen/x.gen.go":            "billing",
		"app/billing/spec.go":                 "billing",
		"app/app.go":                          "app",
		"example/myapp/app/customers/spec.go": "customers",
		"cmd/myapp/main.go":                   "myapp",
	}
	for path, want := range cases {
		if got := moduleOf(path); got != want {
			t.Errorf("moduleOf(%s) = %s want %s", path, got, want)
		}
	}
}

func TestExplainCoversEveryReportedRule(t *testing.T) {
	reported := map[string]bool{}
	for _, r := range Catalog {
		reported[r.ID] = true
	}
	for _, rule := range []string{
		"AM-SPEC-001", "AM-SPEC-002", "AM-SPEC-003", "AM-SPEC-004", "AM-SEC-001", "AM-SEC-002",
		"AM-SEC-003", "AM-SEC-004", "AM-SEC-005", "AM-DATA-001", "AM-DATA-002", "AM-DATA-003",
		"AM-CODE-001", "AM-CODE-002", "AM-CODE-003", "AM-CODE-004", "AM-PERF-003", "AM-MIG-001",
		"AM-TEST-001",
	} {
		if !reported[rule] {
			t.Errorf("rule %s is in §14.3 but not in the catalog", rule)
		}
	}
	if out := Explain("AM-DATA-002"); !strings.Contains(out, "With(") {
		t.Fatalf("explain must show the corrected version:\n%s", out)
	}
	if out := Explain("AM-NOPE-999"); !strings.Contains(out, "no rule") {
		t.Fatal("an unknown rule id must say so and show the index")
	}
}
