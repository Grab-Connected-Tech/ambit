# The spec language

A spec is Go code that only declares things. Each module's `spec.go` holds package-level
`var` declarations built from the `spec` package, imported as `s`. Spec files may not contain
functions or control flow (`ks check` enforces this, rule KS-SPEC-001). `ks gen` runs the
declarations once at build time and compiles them into `app.model.json`.

```go
package billing

import (
	s "keystone.dev/keystone/spec"

	"myapp/app"
	"myapp/app/customers"
)
```

## App-level spec (`app/app.go`)

`app/app.go` declares the tenant, the account people log in as, the roles, and how callers
authenticate.

```go
// The tenant: the isolation boundary. Every tenant resource belongs to one.
var Org = s.TenantResource("Org",
	s.Fields(s.Text("name").MaxLen(200)),
	s.Access(s.Read(Member), s.Update(Admin)),
)

// The account: its id is the actor id, so s.Owner("owner") means "the caller created it".
var User = s.Account("User",
	s.Fields(s.Email("email").Unique(), s.Text("name").MaxLen(200).Optional()),
	s.Access(
		s.Read(s.Owner("id").Or(Admin)),
		s.Update(s.Owner("id").Or(Admin)),
	),
)

var (
	Admin  = s.Role("admin")
	Member = s.Role("member")
)

var Identity = s.Identity(
	s.Sessions(s.TTL(30*s.Day), s.IdleTimeout(7*s.Day)),
	s.APITokens(s.Scoped()),
	s.OIDC("google"),
)
```

Roles are granted per tenant (an actor can be an admin in one org and a member in another).
Revoking a role takes effect on the next request.

## Resources

```go
var Customer = s.Resource("Customer",
	s.Tenant(),                                   // rows belong to a tenant (almost always)
	s.Fields(
		s.Text("name").MaxLen(200),
		s.Email("email").Unique(),                // unique within the tenant
		s.Text("external_id").Optional().MaxLen(64),
		s.Ref("owner", app.User),                 // stored as owner_id
	),
	s.Index("owner"),
	s.Access(/* see below */),
)
```

Every resource gets `id` (a UUIDv7), `tenant_id`, `created_at`, `updated_at` and `version`
automatically. The table name is the snake-cased resource name.

### Field types

| Builder | Go type in `gen` | Notes |
| --- | --- | --- |
| `s.Text(name)` | `string` | Add `.MaxLen(n)`: a text field without one is a warning (KS-DATA-004) |
| `s.Int(name)` | `int64` | `.Min(n)`, `.Max(n)` |
| `s.Decimal(name)` | `ks.Decimal` | `.Precision(p)`, `.Scale(s)`; travels as a string in JSON |
| `s.Money(name)` | `ks.Money` | Minor units plus currency (`.Currency("EUR")`); JSON `{"minor":1250,"currency":"EUR"}` |
| `s.Bool(name)` | `bool` | |
| `s.Timestamp(name)`, `s.Date(name)` | `time.Time` | `.DefaultNow()` |
| `s.Enum(name, values...)` | a generated named string type | e.g. `gen.InvoiceStatusDraft` |
| `s.Email(name)`, `s.URL(name)` | `string` | Validated on input; `.Schemes(...)` for URLs |
| `s.JSON(name)`, `s.Bytes(name)` | raw JSON, `[]byte` | `.MaxSize(n)` |
| `s.ID(name, resource)` | `ks.ID` | An id input that refers to a resource |
| `s.Ref(name, resource)` | `ks.ID` (`<Name>ID`) | A foreign key; `.OnDelete("restrict"\|"cascade"\|"set_null")` |

Modifiers that apply to any field:

| Modifier | Meaning |
| --- | --- |
| `.Optional()` | May be absent. The Go type becomes `ks.Opt[T]` |
| `.Default(v)` | Value when absent on insert |
| `.Unique()` | Unique (within the tenant, for tenant resources) |
| `.Immutable()` | Cannot change after insert (enforced by a trigger) |
| `.Sensitive()` | Redacted from logs, traces and audit diffs. Pair with a `FieldRead` rule (KS-SEC-007) |
| `.Pattern(re)`, `.MinLen(n)`, `.Trim()` | Input validation |
| `.RenameFrom("old")` | Lets a migration rename a column instead of dropping and re-adding it |
| `s.Encrypted(field)` | Encrypted at rest with the app's key; see [Operations](operations.md#encryption-at-rest) |

### Indexes and invariants

```go
s.Index("customer", "status"),          // indexes lead with tenant_id automatically
s.UniqueIndex("customer", "number"),

s.Invariants(
	s.Check("amount_positive", s.F("amount").Gt(0)),
	s.Check("paid_has_date", s.F("status").Eq("paid").Implies(s.F("paid_at").IsSet())),
),
```

An invariant over a single row becomes a database `CHECK` constraint, so no write path can
break it.

## Expressions

Access rules and invariants share one small expression language:

| Expression | Meaning |
| --- | --- |
| `s.F("status")` | A field of the row. `.Eq(v)`, `.Ne`, `.Gt`, `.Gte`, `.Lt`, `.Lte`, `.In(v...)`, `.IsSet()`, `.IsNull()`, `.HasPrefix(s)` |
| `app.Admin` | The caller holds the role in the row's tenant |
| `s.Owner("owner")` | The field equals the caller's actor id |
| `a.And(b)`, `a.Or(b)`, `a.Not()`, `a.Implies(b)` | Combinators; `s.Not(a)` also works |
| `s.Always()`, `s.Never()` | Constants |

## Access rules

```go
s.Access(
	s.Read(app.Member),
	s.Create(app.Admin.Or(app.Accountant)),
	s.Update(app.Admin.Or(app.Accountant).And(s.F("status").In("draft", "sent"))),
	s.UpdateAfter(s.F("status").Ne("void")),     // checked on the row a write would produce
	s.Delete(s.Never()),
	s.FieldRead("notes", app.Admin.Or(app.Accountant)),
	s.FieldWrite("tax_id", app.Admin),
),
```

- **Deny by default.** An operation with no rule is refused.
- **Invisible rows.** A row the caller may not read returns `not_found`, never `forbidden`, so
  its existence does not leak.
- **Hidden fields.** A field the caller may not read is never fetched. It comes back absent
  (or zero, for a required field).
- **Tenant isolation is implicit.** No rule can express it away. Reading across tenants needs
  `s.CrossTenant()` on the action, and `ks diff` always ranks that high-risk.

## Actions

There are four kinds of action: `s.Query` (no writes), `s.Mutation`, `s.Job` (runs from the
queue) and `s.Schedule` (starts a job on a cron).

```go
var SendInvoice = s.Mutation("SendInvoice",
	s.Input(s.ID("invoice_id", Invoice)),
	s.Returns(Invoice),
	s.Requires(app.Admin.Or(app.Accountant)),     // or s.Public(); one is mandatory
	s.Reads(customers.Customer),
	s.Writes(Invoice),
	s.Effects(s.EmailEffect()),
	s.Errors("not_draft"),
	s.RateLimit(s.PerActor(60, s.Minute)),
)

var ListInvoices = s.Query("ListInvoices",
	s.Input(s.FieldOf(Invoice, "status").Optional()),
	s.Returns(s.Page(Invoice)),                   // keyset-paginated; input gains cursor and limit
	s.Requires(app.Member),
	s.Reads(Invoice),
	s.Cache(s.TTL(5*s.Minute), s.InvalidatedBy(Invoice)),
)

var SendReminders = s.Job("SendReminders",
	s.RunAs(s.System),                            // or s.Enqueuer: a snapshot of whoever queued it
	s.Reads(Invoice),
	s.Effects(s.EmailEffect()),
	s.Retry(s.MaxAttempts(5), s.Exponential(s.Second, 10*s.Minute)),
	s.Concurrency(s.Key("customer"), 1),
)

var DailyReminders = s.Schedule("DailyReminders", s.Cron("0 9 * * *"), s.Runs(SendReminders))
```

| Option | Meaning |
| --- | --- |
| `Input(fields...)` | Typed, validated input. Uses the same builders as fields; `s.FieldOf(res, "f")` reuses one |
| `Returns(res)`, `Returns(s.Page(res))`, `Returns(fields...)` | The output |
| `Requires(rule)` / `Public()` | Who may call it. Public mutations need a rate limit (KS-SEC-005) |
| `Reads`, `Writes`, `Calls`, `Effects` | The only data, other actions and side effects the code can reach |
| `Errors(codes...)` | Declared failure codes, each with a typed constructor and a client type |
| `RateLimit(s.PerActor/PerIP/PerTenant(n, period))` | Request limits |
| `Idempotent()` | Requires an `Idempotency-Key`; a repeat returns the stored result |
| `Timeout(d)`, `QueryBudget(n)`, `RowsBudget(n)` | Override the app defaults |
| `Counts()` | Allows `Count()` queries |
| `Get()` | Also answer `GET` with query parameters |
| `ReplicaOK()`, `Serializable()` | Read from the replica; serializable isolation with automatic retries |
| `CrossTenant()` | May read across tenants (high-risk in the diff) |
| `Deprecated(reason)` | Shown in the client. Breaking API changes need a release of this first (KS-API-001) |

## Effects

| Effect | Declared as | The action calls |
| --- | --- | --- |
| Email | `s.EmailEffect(s.Template("invoice_sent"))` | `c.Email.Send(c, payload)` |
| SMS | `s.SMSEffect()` | `c.SMS.Send(c, payload)` |
| HTTP | `s.HTTP(s.Hosts("api.psp.example"))` | `c.HTTP.Send(c, payload, host)` |
| Webhook | `s.Webhook(s.Topic("invoice.paid"))` | `c.Webhook.Send(c, payload)` |
| Storage | `s.Storage(s.Bucket("invoices"))` | `c.Storage.Send(c, payload)` |
| Enqueue | `s.Enqueue(SendReminders)` | starts the job after commit |

Effects are **deferred** by default. They are written to an outbox inside the action's
transaction and run only if it commits. `.Immediate()` runs an effect during the action when
you need its result. Name a job with `.Compensate("billing.RefundCharge")`, and that job is
queued if the action's transaction later rolls back:

```go
s.HTTP(s.Hosts("api.psp.example")).Immediate().Compensate("billing.RefundCharge"),
```

The compensating job receives the original effect call and the provider's response as its
input (`in.Effect`, `in.Result`).
