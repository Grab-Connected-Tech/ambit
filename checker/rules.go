package checker

// Rule is one catalog entry (§14.3). The text lives in Go, not in a data file, so
// a rule and its explanation can never drift apart.
type Rule struct {
	ID       string
	Severity string
	Title    string
	Section  string
	Why      string
	Bad      string
	Good     string
	Where    string // which files the check runs on
}

// Catalog is every rule am check can report.
var Catalog = []Rule{
	{"AM-SPEC-001", "error", "spec file contains non-declarative code", "5",
		`A spec is the human review surface. If a spec can branch, loop or call a
function, then reviewing it means reading program logic, which is exactly what
the split between spec and code exists to avoid.`,
		`func init() {
    if os.Getenv("STAGING") != "" {
        s.Role("admin")
    }
}`,
		`var Admin = s.Role("admin")

var Invoice = s.Resource("Invoice",
    s.Tenant(),
    s.Fields(s.Text("memo").MaxLen(200)),
)`, "spec files"},
	{"AM-SPEC-002", "error", "spec references an unknown role, resource, field or action", "5",
		`References in a spec are Go values, so a typo is normally a compile error. This
rule fires when a reference resolves to nil or names something the model does not
contain, which happens when two packages declare the same name or a file was moved.`,
		`s.Reads(customers.Customers) // no such resource`,
		`s.Reads(customers.Customer)`, "everywhere"},
	{"AM-SPEC-003", "error", "rule or invariant cannot be compiled to SQL", "5.4",
		`Access rules and invariants are enforced by the database, not by the action. An
expression that cannot become SQL would be silently unchecked in production, so the
build refuses it instead.`,
		`s.Read(s.Not(s.F("tags").Eq("x"))) // tags is not a column`,
		`s.Read(s.F("status").In("draft", "sent"))`, "resources and actions"},
	{"AM-SPEC-004", "error", "action declares neither Requires nor Public", "5.7",
		`An unguarded endpoint is the single most common AI-written backend bug. Denying by
default means the build must fail when the spec says nothing about callers.`,
		`var GetInvoice = s.Query("GetInvoice",
    s.Input(s.ID("invoice_id", Invoice)),
    s.Reads(Invoice),
)`,
		`var GetInvoice = s.Query("GetInvoice",
    s.Input(s.ID("invoice_id", Invoice)),
    s.Requires(app.Member),
    s.Reads(Invoice),
)`, "actions"},
	{"AM-SPEC-005", "error", "an action's options contradict its kind or ownership", "4.1",
		`Queries cannot write, jobs cannot return values, a module cannot write another
module's resources, and a schedule must run a job. Each contradiction would otherwise
show up as a runtime surprise.`,
		`var ListInvoices = s.Query("ListInvoices",
    s.Writes(Invoice), // a query does not write
)`,
		`var ListInvoices = s.Query("ListInvoices",
    s.Reads(Invoice),
)`, "actions"},
	{"AM-SPEC-006", "error", "the same resource is listed in Reads and Writes", "4.1",
		`A module may always read what it owns, so the extra entry adds nothing and hides
what the action really touches when a reviewer reads the diff.`,
		`s.Reads(Invoice), s.Writes(Invoice)`,
		`s.Writes(Invoice)`, "actions"},
	{"AM-SEC-001", "error", "action package imports a banned package", "7.4",
		`Anything that reaches outside the process bypasses the framework's guarantees. If
an action could open a socket, the access rules would be advisory.`,
		`import (
    "net/http"
    "os"
)`,
		`import (
    "strings"
    "github.com/abhilashiig/ambit/am"
    "myapp/app/billing/gen"
)`, "actions"},
	{"AM-SEC-002", "error", "environment or secret read outside generated config", "9.4",
		`Secrets must be auditable in one place. An action reading os.Getenv can ship a
production key into a staging deploy, and nothing in the diff would show it.`,
		`key := os.Getenv("STRIPE_KEY")`,
		`// declared in am.yaml under secrets:, delivered through the action context`, "actions"},
	{"AM-SEC-003", "error", "unsafe block has no reason", "8.6",
		`An escape hatch is allowed, but it must be legible in review. A reasonless unsafe
block is indistinguishable from a mistake.`,
		`am.Unsafe(c, "", func(tx am.RawTx) error { ... })`,
		`am.Unsafe(c, "bulk import of 40k legacy rows; single statement is 30x faster", func(tx am.RawTx) error { ... })`,
		"actions"},
	{"AM-SEC-004", "error", "Sensitive field value reaches a log or error message", "9.1",
		`A field marked Sensitive is promised never to appear in logs, traces or audit
diffs. Logging the value directly breaks that promise in the one place an attacker
reads.`,
		`c.Log.Info("paid", "card", inv.CardNumber)`,
		`c.Log.Info("paid", "invoice_id", inv.ID)`, "actions"},
	{"AM-SEC-005", "error", "public mutation has no rate limit", "9.1",
		`A public write is an enumeration and abuse surface. The framework cannot guess a
sensible number, so it requires one.`,
		`var Signup = s.Mutation("Signup", s.Public(), s.Writes(User))`,
		`var Signup = s.Mutation("Signup", s.Public(), s.Writes(User),
    s.RateLimit(s.PerIP(5, s.Hour), s.PerActor(3, s.Hour)))`, "actions"},
	{"AM-SEC-006", "warning", "HTTP effect URL built from input", "9.1",
		`The host allowlist is checked after DNS resolution, so a request to an attacker
supplied name cannot reach a private address. Building the URL from input means the
check happens at request time rather than at build time.`,
		`c.HTTP.Fetch(ctx, in.CallbackURL)`,
		`c.HTTP.FetchHost(ctx, "api.example.com", in.Path)`, "actions"},
	{"AM-SEC-007", "warning", "Sensitive field has no field-level rule", "5.6",
		`Sensitive() redacts a value from logs. Only FieldRead keeps it out of responses, so
a Sensitive field with no rule is visible to anyone who can read the row.`,
		`s.Fields(s.Text("tax_id").Sensitive())`,
		`s.Fields(s.Text("tax_id").Sensitive()),
s.Access(s.FieldRead("tax_id", app.Admin))`, "resources"},
	{"AM-SEC-008", "error", "input declares a runtime-owned field", "9.1",
		`tenant_id, version and created_at are set by the framework from the actor. Accepting
them from a request body is the mass-assignment bug Ambit removes by construction.`,
		`s.Input(s.ID("tenant_id", app.Org), s.Text("name"))`,
		`s.Input(s.Text("name")) // the tenant comes from the actor`, "actions"},
	{"AM-SEC-009", "warning", "immediate effect without an idempotency key", "10.2",
		`An immediate effect runs before commit. If the client retries, the effect runs again
unless the mutation carries an idempotency key.`,
		`s.Effects(s.HTTP(s.Hosts("api.example.com")).Immediate())`,
		`s.Idempotent(), s.Effects(s.HTTP(s.Hosts("api.example.com")).Immediate())`, "actions"},
	{"AM-DATA-001", "error", "query has no terminal bound", "8.1",
		`An unbounded query is an out-of-memory error with a route attached. Every query must
end in Page, First, Get, Count or Each.`,
		`rows, err := c.DB.Invoices.Query().All(c)`,
		`page, err := c.DB.Invoices.Query().Page(c, in.Cursor, 50)`, "actions"},
	{"AM-DATA-002", "error", "scoped DB call inside a loop", "8.2",
		`One query per iteration is the N+1 pattern. Relations load with .With, which sends a
single batched query no matter how many rows came back.`,
		`for _, inv := range list {
    cust, err := c.DB.Customers.Get(c, inv.CustomerID, nil)
}`,
		`page, err := c.DB.Invoices.Query().With(gen.InvoiceWithCustomer).Page(c, in.Cursor, 50)
for _, inv := range page.Rows {
    email := inv.Customer.Email
}`, "actions"},
	{"AM-DATA-003", "error", "error from a DB or effect call is ignored", "7.3",
		`A dropped error means a write that never happened looks like a success, and the
transaction may still commit.`,
		`c.DB.Invoices.Update(c, id, ver, patch)`,
		`if _, err := c.DB.Invoices.Update(c, id, ver, patch); err != nil {
    return nil, err
}`, "actions"},
	{"AM-DATA-004", "warning", "text field has no MaxLen", "5.2",
		`An unbounded string turns one row into an arbitrarily large response and makes the
p99 latency of a list endpoint a function of user input.`,
		`s.Text("notes")`,
		`s.Text("notes").MaxLen(2000)`, "resources"},
	{"AM-CODE-001", "error", "generated file edited, or committed model differs from spec", "6.1",
		`Generated output is derived, not authored. Editing it means the next am gen erases
the change, so the check fails before that surprise reaches another engineer.`,
		`// editing gen/invoice.gen.go by hand`,
		`// change the spec, run am gen`, "gen and app.model.json"},
	{"AM-CODE-002", "error", "import of another module's internals", "4.1",
		`Modules talk through declared calls, not through each other's packages. Importing a
sibling's actions package silently skips the callee's access rules.`,
		`import "myapp/app/customers/actions"`,
		`import "myapp/app/customers/gen"`, "actions"},
	{"AM-CODE-003", "warning", "switch over a generated enum is not exhaustive", "5.2",
		`Adding an enum value must feel like a schema change, not a silent fall-through. An
exhaustive switch makes the compiler point at every place that needs a decision.`,
		`switch inv.Status {
case gen.InvoiceStatusDraft:
    ...
}`,
		`switch inv.Status {
case gen.InvoiceStatusDraft:
    ...
case gen.InvoiceStatusSent, gen.InvoiceStatusPaid, gen.InvoiceStatusVoid:
    ...
default:
    return nil, am.Invalid("status", "unhandled status")
}`, "actions"},
	{"AM-CODE-004", "error", "wall-clock time used instead of the context clock", "7.4",
		`A test must control time or time-dependent logic cannot be tested deterministically.
The context clock is a fake in tests and the system clock in production.`,
		`if time.Since(inv.SentAt) > 24*time.Hour { ... }`,
		`if c.Clock.Since(inv.SentAt) > 24*time.Hour { ... }`, "actions"},
	{"AM-CODE-005", "error", "declared action has no implementation", "7.2",
		`Every declared action is one exported function with a fixed signature. A missing
function is a broken promise to the client that was generated for it.`,
		`// spec declares SendInvoice, actions/ has no SendInvoice`,
		`func SendInvoice(c *gen.SendInvoiceCtx, in gen.SendInvoiceInput) (*gen.Invoice, error) { ... }`,
		"actions"},
	{"AM-API-001", "error", "breaking API change without a deprecation release", "12.4",
		`A client generated from the last release still calls the old shape. Marking it
Deprecated() for one release gives every client a version that warns before it breaks;
an internal-only API can opt out with api.compatibility: none.`,
		`// release N removes billing.SendInvoice outright`,
		`// release N: s.Deprecated("use SendInvoiceV2"); release N+1: remove it`, "diff"},
	{"AM-PERF-001", "error", "query or row budget exceeded in tests", "8.4",
		`Budgets are the framework's promise about per-request work. Exceeding one in a test is
the same failure with a smaller dataset.`,
		`// action issues 24 queries with a budget of 20`,
		`// declare s.QueryBudget(30) in the spec if the work is real, or batch the queries`, "tests"},
	{"AM-PERF-002", "warning", "captured query plans a sequential scan on a large table", "11.4",
		`A seq scan on a table above the size threshold is a missing index, and the spec is
where indexes are declared.`,
		`// am perf reports: seq scan on invoice (4.1M rows)`,
		`s.Index("customer", "status")`, "am perf"},
	{"AM-PERF-003", "error", "go statement in action code", "7.4",
		`A goroutine outlives the request and the transaction, so its work can commit after a
rollback or never run at all after a crash. Enqueue is the version that survives.`,
		`go sendEmail(inv)`,
		`c.Enqueue(billing.SendInvoiceEmail, gen.SendInvoiceEmailArgs{InvoiceID: inv.ID})`, "actions"},
	{"AM-MIG-001", "error", "destructive migration step without approval", "13.2",
		`Data loss must be impossible by accident. A destructive step needs an explicit flag,
which means a human typed it.`,
		`am migrate apply            // drops a column`,
		`am migrate apply --allow-destructive`, "migrations"},
	{"AM-MIG-002", "error", "applied migration file was modified", "13.1",
		`Applied migrations are history. Rewriting one means the next environment converges to
a different schema than production did.`,
		`// editing migrations/0001_init.sql`,
		`// write a new migration that changes what you meant`, "migrations"},
	{"AM-MIG-003", "warning", "immutable required field has no default", "5.2",
		`A field that is required, immutable and defaultless cannot appear in an INSERT, so
the resource can never be created.`,
		`s.Text("source").Immutable()`,
		`s.Text("source").Immutable().Default("manual")`, "resources"},
	{"AM-TEST-001", "warning", "action has no test", "16.3",
		`Generated tests cover the rules; only a written test covers the intent. An untested
action is behavior nobody described.`,
		`// actions/send_invoice.go with no send_invoice_test.go`,
		`func TestSendInvoice(t *testing.T) { env := amtest.New(t); ... }`, "actions"},
	{"AM-DEP-001", "error", "known vulnerability reachable from app code", "9.6",
		`A dependency you never call cannot hurt you; one you do can. govulncheck output is
folded into am check so the build, not a scheduled scan, is the gate.`,
		`// CVE-2026-0001 in a called function of an imported module`,
		`// upgrade the module, or drop the import`, "am check"},
}

// RuleByID looks up a rule for am explain.
func RuleByID(id string) (Rule, bool) {
	for _, r := range Catalog {
		if r.ID == id {
			return r, true
		}
	}
	return Rule{}, false
}

// Explain renders the am explain output for one rule.
func Explain(id string) string {
	r, ok := RuleByID(id)
	if !ok {
		return "no rule " + id + "\n\n" + RuleIndex()
	}
	out := r.ID + " (" + r.Severity + ") - " + r.Title + "\n"
	out += "spec section " + r.Section + ", runs on " + r.Where + "\n\n"
	out += "why\n  " + indent(r.Why) + "\n\n"
	out += "fails\n  " + indent(r.Bad) + "\n\n"
	out += "passes\n  " + indent(r.Good) + "\n"
	return out
}

// RuleIndex is a one-line-per-rule summary, used by am explain with no argument and
// by the generated AGENTS.md.
func RuleIndex() string {
	var out string
	for _, r := range Catalog {
		out += r.ID + "  " + r.Severity + "  " + r.Title + "\n"
	}
	return out
}

func indent(s string) string {
	var b []byte
	atLineStart := true
	for i := 0; i < len(s); i++ {
		c := s[i]
		if atLineStart && c != '\n' {
			b = append(b, "  "...)
			atLineStart = false
		}
		b = append(b, c)
		if c == '\n' {
			atLineStart = true
		}
	}
	return string(b)
}
