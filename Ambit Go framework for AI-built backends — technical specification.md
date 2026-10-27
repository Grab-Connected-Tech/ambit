# Ambit: Go framework for AI-built backends — technical specification

Oct 27, 2026 · @Abhilash

## 1. Overview and goals

Ambit is an open-source Go framework where AI agents write the backend code, humans review a short spec and a machine-computed behavior diff, and the framework guarantees security, data integrity and performance underneath. Ambit is a working name.

The core bet: as AI writes most code, the bottleneck moves from writing to trusting. Ambit makes the common classes of backend bugs impossible by construction, so review effort goes only to what genuinely changed.

### 1.1 Goals and targets

Every target below is a design goal to validate with benchmarks, not a measured result.

| Area | Goal | Target |
| --- | --- | --- |
| Security | No data access outside the access rules | Zero cross-tenant reads possible without an explicit `CrossTenant()` or `unsafe` declaration |
| Security | Every action authorized | 100% of actions declare `Requires(...)` or `Public()`; the build fails otherwise |
| Integrity | Invariants hold regardless of code | Every invariant enforced in Postgres where expressible, at runtime otherwise |
| Integrity | No silent data loss | Destructive migrations blocked without an explicit plan |
| Performance | Minimal framework overhead | p99 overhead under 1 ms per request, excluding database and effect time |
| Performance | No reflection on the hot path | All routing, decoding, encoding and scanning is generated code |
| Performance | Small footprint | Idle memory under 50 MB; cold start under 1 s |
| Agent velocity | Fast feedback | Incremental `am check` under 2 s on a 100k-line app |
| Agent velocity | Context that fits | `am context <module>` output under 8k tokens for a typical module |
| Review | Small review surface | Every high-risk change appears in the behavior diff, computed without an LLM |

### 1.2 Non-goals

- Not a frontend framework. Ambit generates a typed TypeScript client; teams use any UI stack.
- Not multi-database. Postgres only, version 15 or later.
- Not microservices. One deployable binary with strong internal module boundaries.
- Not an agent runtime or LLM orchestration library. Apps may call LLMs through declared effects like any other API.
- Not GraphQL. Actions are typed RPC with generated OpenAPI.

### 1.3 Terminology

| Term | Meaning |
| --- | --- |
| Spec | Declarative Go files describing resources, access rules, invariants and actions. Owned by humans. |
| Resource | A persisted entity, backed by one Postgres table. |
| Action | A named unit of behavior: a query, mutation, job or scheduled task. |
| Effect | Any side effect outside the database: email, HTTP call, file storage, queue publish. |
| Capability | A permission an action holds, expressed as a field on its generated context struct. |
| App model | The canonical, machine-readable representation of the whole app, compiled from the spec. |
| Behavior diff | A risk-ranked summary of what changed between two app models and their code. |
| Actor | The identity an action runs as: user, service account, system or anonymous. |
| Tenant | The top-level isolation boundary, usually an organization. |
| Unsafe block | An explicitly marked escape hatch that weakens a guarantee and is always surfaced in review. |

## 2. Design principles

Ten principles govern every design decision; when two conflict, the lower number wins.

1. **Guarantees over guidelines.** If a rule matters, the compiler or the database enforces it. Documentation is never the enforcement mechanism.
2. **Deny by default.** No action runs, no row is readable and no effect is callable unless the spec grants it.
3. **Humans review intent, machines verify implementation.** The spec and the behavior diff are the human interface. Action code is checked, not trusted.
4. **Explicit over implicit.** No callbacks, hooks, signals, struct tags with behavior, or init-time registration in app code. The code that runs is the code you see.
5. **One way to do each thing.** A small, stable API surface reduces agent hallucination and keeps codebases uniform.
6. **Locality.** Everything about a feature lives in one module folder. An agent loading that folder has the full picture.
7. **Generated, not reflected.** Anything derivable from the spec is generated as plain Go: types, queries, routing, encoding, validation, clients.
8. **Fast, precise feedback.** Every error names the rule, the location and the fix, in human and JSON form.
9. **Visible escape hatches.** Weakening a guarantee is allowed only through `unsafe` blocks with a stated reason, and always appears in review.
10. **Boring infrastructure.** One binary, one Postgres database. No Redis, no message broker, no sidecars required.

## 3. Architecture

Ambit has two halves: a build-time toolchain that compiles the spec into an app model and derives everything from it, and a runtime library that enforces the spec's guarantees in production.

&#91;embedded content: architecture · build time and runtime\]

Humans own the spec, agents own the action code, and everything else is derived from the app model or enforced by the runtime.

### 3.1 Components

| Component | Package or command | Responsibility |
| --- | --- | --- |
| Spec library | `github.com/abhilashiig/ambit/spec` | Builder functions used in spec files. Pure declarations, no runtime behavior. |
| Spec compiler | `am gen` | Executes spec packages, validates them, emits `app.model.json`. |
| Code generator | `am gen` | Generates Go types, scoped DB access, routing, codecs, validators, contexts and clients from the app model. |
| Checker | `am check` | Runs the Go compiler plus Ambit analyzers over all code; emits structured diagnostics. |
| Migration engine | `am migrate` | Diffs app models against the live schema, classifies changes, plans and applies migrations. |
| Behavior differ | `am diff` | Compares app models and code between two git revisions; produces a risk-ranked report. |
| Runtime | `github.com/abhilashiig/ambit/runtime` | HTTP server, auth, request pipeline, transactions, policy enforcement, outbox, jobs, telemetry. |
| Test harness | `am test` | Ephemeral Postgres, fixtures, effect fakes, generated policy and invariant tests. |
| Dev server | `am dev` | Hot reload, introspection API for agents, trace viewer, seed data. |
| Agent tooling | `am context`, `am explain` | Compact module context, rule explanations, generated `AGENTS.md`. |

### 3.2 Build pipeline

1. `am gen` builds a temporary program that imports every spec package and serializes the declared app to `app.model.json`.
2. The compiler validates the model: references resolve, rules compile to SQL, roles exist, no cycles in module calls.
3. The generator writes `gen/` packages per module and the root `cmd/` entrypoint. Output is deterministic and byte-stable.
4. `am check` runs `go build`, `go vet` and Ambit analyzers. Results return as one diagnostic list.
5. `am migrate plan` diffs the model against the database and writes a migration plan when the schema changed.
6. `am diff` compares the committed model and code against a base revision for review.

### 3.3 Request lifecycle

1. **Accept.** The HTTP server enforces body size, header limits and per-route timeouts, then applies load shedding if the concurrency limit is reached.
2. **Route.** A generated static route table maps the path to an action. No reflection or regex routing.
3. **Authenticate.** The session cookie or bearer token resolves to an actor. Unknown or expired credentials yield an anonymous actor.
4. **Authorize the call.** The action's `Requires` rule is evaluated against the actor. Failure returns `unauthenticated` or `forbidden`.
5. **Rate limit.** Declared limits are checked against a Postgres-backed token bucket with an in-process cache.
6. **Decode and validate.** Generated decoders parse input; generated validators enforce field constraints from the spec.
7. **Build context.** The runtime constructs the action's generated context struct: a scoped DB bound to the actor, declared effects, logger and clock.
8. **Execute.** The action runs inside one database transaction with a statement timeout and a query budget.
9. **Commit.** Invariants checked at runtime are verified, the transaction commits, and outbox rows become visible.
10. **Respond.** Output passes through field-level read rules, then a generated encoder writes the response.
11. **Dispatch effects.** The outbox dispatcher, woken by `LISTEN/NOTIFY`, executes deferred effects after commit.

## 4. Project layout and module system

An app is a set of feature modules, each a self-contained folder holding its spec, its hand-written actions, its tests and its generated code.

```
myapp/
  am.yaml                    app configuration
  go.mod
  app.model.json             compiled app model (generated, committed)
  AGENTS.md                  agent instructions (generated)
  app/
    app.go                   app-level spec: roles, identity, tenancy
    billing/
      spec.go                resources and actions (human-reviewed)
      actions/
        send_invoice.go      action implementations (AI-written)
        send_invoice_test.go
      gen/                   generated, never edited
    customers/
      spec.go
      actions/
      gen/
  migrations/
    0001_init.sql            generated SQL
    0001_init.plan.json      classification and safety metadata
  cmd/myapp/main.go          generated entrypoint
  client/ts/                 generated TypeScript client
```

### 4.1 Module rules

- A module owns its resources. Only that module's actions may write them.
- Other modules may reference a resource in their spec with `Ref`, and read it only through a declared `Reads(...)`.
- Cross-module behavior goes through declared calls: `Calls(customers.GetCustomer)` adds `c.Calls.GetCustomer` to the context. Calls run in the same transaction, as the same actor, with the callee's access rules applied.
- Go imports between modules are limited to the other module's `gen` package types. Importing another module's `actions` package fails the check (AM-CODE-002).
- Module call graphs must be acyclic. The compiler reports cycles with the full path.

### 4.2 App-level spec

`app/app.go` declares what is shared across modules: actor types, roles, the tenant resource, membership, identity providers and global defaults such as query budgets and request limits. It is the only spec file outside a module.

## 5. Spec language

The spec is restricted, declarative Go written with builder functions from `github.com/abhilashiig/ambit/spec`, conventionally imported as `s`. Spec files contain only package-level `var` declarations whose values are spec builder calls, literals and references to other spec values. The checker rejects anything else (AM-SPEC-001).

### 5.1 App-level declarations

```go
package app

import s "github.com/abhilashiig/ambit/spec"

var Org = s.TenantResource("Org",
	s.Fields(s.Text("name").MaxLen(200)),
)

var (
	Admin      = s.Role("admin")
	Accountant = s.Role("accountant")
	Member     = s.Role("member")
)

var Identity = s.Identity(
	s.Sessions(s.TTL(30 * s.Day), s.IdleTimeout(7 * s.Day)),
	s.APITokens(s.Scoped()),
	s.OIDC("google"),
)
```

A role value is also a rule: `Admin` means the actor holds the admin role in the current tenant. Roles are granted through a generated `Membership` resource linking actors to tenants.

### 5.2 Resources and fields

Every resource gets `id` (UUIDv7), `created_at`, `updated_at` and `version` (for optimistic concurrency) automatically. A resource with `s.Tenant()` also gets `tenant_id`, which every query filters on.

| Builder | Go type | Postgres type | Options |
| --- | --- | --- | --- |
| `Text` | `string` | `text` | `MaxLen`, `MinLen`, `Pattern`, `Trim` |
| `Int` | `int64` | `bigint` | `Min`, `Max` |
| `Decimal` | `am.Decimal` | `numeric(p, s)` | `Precision`, `Scale`, `Min`, `Max` |
| `Money` | `am.Money` | `bigint` minor units + `text` currency | `Min`, `Max`, `Currency` (fixed) |
| `Bool` | `bool` | `boolean` | `Default` |
| `Timestamp` | `time.Time` | `timestamptz` | `DefaultNow` |
| `Date` | `am.Date` | `date` | — |
| `Enum` | generated string type | `text` + `CHECK` | `Default` |
| `Email` | `am.Email` | `text`, lowercased | — |
| `URL` | `am.URL` | `text`, validated | `Schemes` |
| `JSON` | a declared Go struct | `jsonb` | `Schema` (must be a generated type) |
| `Bytes` | `[]byte` | `bytea` | `MaxSize` |
| `Encrypted` | wraps another field | `bytea`, envelope-encrypted | key from config |
| `Ref` | generated ID type | `uuid` + foreign key | `OnDelete(Restrict, Cascade, SetNull)` |

Modifiers available on every field: `Optional()`, `Default(v)`, `Unique()`, `Immutable()` (cannot change after create) and `Sensitive()` (redacted from logs, traces and audit diffs). Enums use `text` with a `CHECK` constraint rather than Postgres enum types, so adding values never needs a type migration.

### 5.3 Relations and indexes

- `Ref` creates a belongs-to relation with a foreign key; the default `OnDelete` is `Restrict`.
- Has-many relations are derived automatically from refs and loaded with `.With(...)`, never lazily.
- Many-to-many relations require an explicit join resource, so the join's access rules are declared.
- `Index(fields...)`, `UniqueIndex(fields...)` and `.Where(rule)` for partial indexes. Every `Ref` gets an index automatically.

### 5.4 Expressions

Invariants and access rules share one expression language. Every expression must compile to SQL; the compiler rejects ones that cannot.

| Category | Forms |
| --- | --- |
| Operands | `F("field")`, `F("ref.field")` (path depth at most 2), literals, `Actor.ID()`, `Actor.Tenant()`, `Now()` |
| Comparison | `Eq`, `Ne`, `Gt`, `Gte`, `Lt`, `Lte`, `In`, `IsSet`, `IsNull`, `HasPrefix` |
| Logic | `And`, `Or`, `Not`, `Implies` |
| Shortcuts | role values, `Owner("field")` (field equals actor ID), `Always()`, `Never()` |

### 5.5 Invariants

`Invariants(Check(name, expr)...)` declares rules that must hold for every row. Rules using only the row's own fields and immutable values compile to Postgres `CHECK` constraints. Rules that traverse a `Ref` run as deferred checks inside the transaction before commit. Cross-row aggregate invariants, such as a balance summed over rows, are out of scope for v1.

### 5.6 Access policies

Access is deny-by-default. A resource with no `Access` block is readable and writable by no actor.

| Rule | Evaluated against | Effect on failure |
| --- | --- | --- |
| `Read(expr)` | each row, compiled into every query's `WHERE` | the row is invisible; lookups return `not_found` |
| `Create(expr)` | the proposed row | `forbidden` |
| `Update(expr)` | the existing row | `forbidden` |
| `UpdateAfter(expr)` | the row after the change | `forbidden` |
| `Delete(expr)` | the existing row | `forbidden` |
| `FieldRead(field, expr)` | each row, per field | the field is omitted from output and not selectable |
| `FieldWrite(field, expr)` | the existing row, per changed field | `forbidden` |

Tenant isolation is implicit for every `s.Tenant()` resource and cannot be expressed away inside a rule. Reading across tenants requires `CrossTenant()` on the action, which the behavior diff always ranks high-risk.

### 5.7 Actions

Four action kinds exist: `Query` (no writes), `Mutation`, `Job` (runs from the queue) and `Schedule` (runs a job on a cron).

| Option | Applies to | Meaning |
| --- | --- | --- |
| `Input(fields...)` | all | Typed input; fields use the same builders as resources |
| `Returns(resource or fields)` | query, mutation | Typed output; `Page(resource)` for paginated lists |
| `Requires(rule)` or `Public()` | query, mutation | Who may call it; one is mandatory |
| `Reads(resources...)` | all | Resources the action may query |
| `Writes(resources...)` | mutation, job | Resources the action may insert, update or delete |
| `Calls(actions...)` | all | Other modules' actions it may invoke |
| `Effects(effects...)` | mutation, job | Side effects it may perform |
| `Errors(codes...)` | all | Action-specific error codes, typed in the client |
| `RateLimit(PerActor(n, period))`, `PerIP` | query, mutation | Request limits |
| `Idempotent()` | mutation | Requires an idempotency key; repeats return the stored result |
| `Timeout(d)`, `QueryBudget(n)` | all | Override the app defaults |
| `CrossTenant()` | all | Permits reads across tenants; high-risk |
| `RunAs(System or Enqueuer)` | job | Actor the job runs as |
| `Retry(...)`, `Concurrency(...)` | job | Retry policy and per-key concurrency limit |
| `Deprecated(reason)` | all | Marks it for removal; shown in clients and diff |

The `System` actor holds every role within one tenant but never crosses the tenant boundary. A job iterating all tenants must declare `CrossTenant()`.

### 5.8 Effects

| Effect | Declared as | Notes |
| --- | --- | --- |
| Email | `Email()` | Via a configured provider; templates are declared files |
| HTTP | `HTTP(Hosts("api.example.com"))` | Host allowlist enforced at runtime; private IP ranges blocked |
| Webhook | `Webhook(Topic("invoice.sent"))` | Signed outbound webhooks to subscriber URLs |
| Storage | `Storage(Bucket("invoices"))` | Object storage through a configured provider |
| Enqueue | `Enqueue(job)` | Starts a job after commit |
| SMS | `SMS()` | Via a configured provider |

Effects are deferred by default: recorded in the outbox and run after commit. `Immediate()` runs an effect during the action, for cases that need its result before commit. Immediate effects must be idempotent and always rank medium-risk or higher in the diff.

### 5.9 Complete module example

```go
package billing

import (
	s "github.com/abhilashiig/ambit/spec"
	"myapp/app"
	"myapp/app/customers"
)

var Invoice = s.Resource("Invoice",
	s.Tenant(),
	s.Fields(
		s.Ref("customer", customers.Customer),
		s.Money("amount"),
		s.Enum("status", "draft", "sent", "paid", "void").Default("draft"),
		s.Timestamp("sent_at").Optional(),
		s.Timestamp("paid_at").Optional(),
		s.Text("notes").Optional().MaxLen(2000).Sensitive(),
	),
	s.Index("customer", "status"),
	s.Invariants(
		s.Check("amount_positive", s.F("amount").Gt(0)),
		s.Check("paid_has_date", s.F("status").Eq("paid").Implies(s.F("paid_at").IsSet())),
	),
	s.Access(
		s.Read(app.Member),
		s.Create(app.Admin.Or(app.Accountant)),
		s.Update(app.Admin.Or(app.Accountant).And(s.F("status").In("draft", "sent"))),
		s.Delete(s.Never()),
		s.FieldRead("notes", app.Admin.Or(app.Accountant)),
	),
)

var SendInvoice = s.Mutation("SendInvoice",
	s.Input(s.ID("invoice_id", Invoice)),
	s.Returns(Invoice),
	s.Requires(app.Admin.Or(app.Accountant)),
	s.Reads(customers.Customer),
	s.Writes(Invoice),
	s.Effects(s.Email()),
	s.Errors("not_draft"),
	s.RateLimit(s.PerActor(60, s.Minute)),
)

var ListInvoices = s.Query("ListInvoices",
	s.Input(s.FieldOf(Invoice, "status").Optional()),
	s.Returns(s.Page(Invoice)),
	s.Requires(app.Member),
	s.Reads(Invoice),
)

var SendReminders = s.Job("SendReminders",
	s.RunAs(s.System),
	s.Reads(Invoice, customers.Customer),
	s.Effects(s.Email()),
	s.Retry(s.MaxAttempts(5), s.Exponential(s.Second, 10*s.Minute)),
)

var DailyReminders = s.Schedule("DailyReminders",
	s.Cron("0 9 * * *"),
	s.Runs(SendReminders),
)
```

## 6. App model

The app model is the single source of truth: a canonical JSON document describing every module, resource, field, rule, action and effect, from which all generated code, migrations, clients and diffs are derived.

### 6.1 Properties

- **Canonical.** Keys are sorted, arrays have a defined order, and whitespace is fixed, so equal apps produce byte-identical files.
- **Committed.** `app.model.json` is checked into git. The behavior diff compares two committed models without building old code.
- **Verified.** `am check` rebuilds the model from the spec and fails if it differs from the committed file (AM-CODE-001).
- **Versioned.** A top-level `schemaVersion` allows the format to evolve with automated upgrades.
- **Language-neutral.** Nothing in the model is Go-specific, so other backends or clients can be generated from it later.

### 6.2 Structure

Rules are stored as expression trees, not strings, so they can be compiled to SQL, evaluated in Go and compared structurally.

```json
{
  "schemaVersion": 1,
  "app": { "name": "myapp", "tenant": "Org" },
  "roles": ["accountant", "admin", "member"],
  "modules": [
    {
      "name": "billing",
      "resources": [
        {
          "name": "Invoice",
          "tenant": true,
          "fields": [
            { "name": "amount", "type": "money" },
            { "name": "status", "type": "enum", "values": ["draft", "paid", "sent", "void"], "default": "draft" }
          ],
          "access": {
            "read": { "op": "role", "roles": ["member"] },
            "delete": { "op": "never" }
          }
        }
      ],
      "actions": [
        {
          "name": "SendInvoice",
          "kind": "mutation",
          "requires": { "op": "role", "roles": ["accountant", "admin"] },
          "reads": ["customers.Customer"],
          "writes": ["billing.Invoice"],
          "effects": [{ "type": "email", "mode": "deferred" }]
        }
      ]
    }
  ]
}
```

The excerpt omits fields for brevity; the real model also carries indexes, invariants, inputs, outputs, errors, limits and the declared `unsafe` blocks found in code.

### 6.3 Code facts

Alongside the spec, the compiler records facts extracted from action code by static analysis: each `unsafe` block's location and reason, imported Go modules, and the action-to-query map captured during tests. These facts live in a separate `app.facts.json` so the diff can report code-level risk without re-analyzing old revisions.

## 7. Code generation and the action contract

The generator turns the app model into plain, readable Go, and each action's generated context struct is the only door from action code to the outside world.

### 7.1 What is generated

| Output | Per | Contents |
| --- | --- | --- |
| Resource types | resource | Struct, typed ID, enum types and constants, `Patch` type with `am.Opt[T]` fields |
| Column descriptors | resource | Typed predicates and orderings for queries, such as `gen.Invoices.Status.Eq(...)` |
| Scoped DB views | action | Only the declared resources, only the operations the spec permits |
| Context struct | action | Embedded `context.Context`, actor, DB view, declared effects, declared calls, logger, clock |
| Input and output types | action | Structs with generated validators |
| Error constructors | action | One function per declared error code |
| Codecs | type | Allocation-light JSON encoders and decoders, no reflection |
| Row scanners | resource | Direct pgx scanning into structs, no reflection |
| Route table | app | Static map from path to handler adapter |
| Entrypoint | app | `cmd/<app>/main.go` wiring config, runtime and every action |
| SQL | resource | Table DDL, constraints, indexes, row-level security policies |
| Clients | app | TypeScript client and OpenAPI 3.1 document |

Generated files start with `// Code generated by am. DO NOT EDIT.` and carry a content hash. Editing one fails the check (AM-CODE-001). Output is gofmt-formatted and deterministic, so regeneration produces no diff unless the model changed.

### 7.2 The action contract

Each action is one exported function in its module's `actions` package with a fixed signature. The generated entrypoint asserts the signature at compile time, so a missing or mistyped action fails `go build`.

```go
// gen/send_invoice.gen.go — generated
type SendInvoiceCtx struct {
	context.Context
	Actor am.Actor
	DB    SendInvoiceDB
	Email am.EmailEffect // present because Effects(s.Email()) was declared
	Log   *slog.Logger
	Clock am.Clock
}

type SendInvoiceDB struct {
	Invoices  InvoiceWriter             // Get, Query, Insert, Update; no Delete because Delete(Never())
	Customers customers.CustomerReader // Get, Query only; declared in Reads
}
```

```go
// actions/send_invoice.go — written by the agent
func SendInvoice(c *gen.SendInvoiceCtx, in gen.SendInvoiceInput) (*gen.Invoice, error) {
	inv, err := c.DB.Invoices.Get(c, in.InvoiceID, gen.InvoiceWithCustomer)
	if err != nil {
		return nil, err // not found, including invoices the actor cannot see
	}
	if inv.Status != gen.InvoiceStatusDraft {
		return nil, gen.ErrNotDraft()
	}
	updated, err := c.DB.Invoices.Update(c, inv.ID, inv.Version, gen.InvoicePatch{
		Status: am.Set(gen.InvoiceStatusSent),
		SentAt: am.Set(c.Clock.Now()),
	})
	if err != nil {
		return nil, err // forbidden, conflict on stale version, or invariant violation
	}
	c.Email.Send(am.Email{To: inv.Customer.Email, Template: "invoice_sent", Data: updated})
	return updated, nil
}
```

### 7.3 Errors

| Error | Constructor | HTTP status |
| --- | --- | --- |
| Declared action error | `gen.ErrNotDraft()` | 422 |
| Not found or not visible | `am.NotFound()` | 404 |
| Validation | `am.Invalid(field, message)` | 400 |
| Forbidden | returned by the runtime | 403 |
| Stale version | returned by the runtime | 409 |
| Invariant violation | returned by the runtime | 422 |
| Any other error | — | 500, message replaced, details logged with the trace ID |

### 7.4 Import rules for action code

Action packages may import their own module's `gen`, other modules' `gen` types, `github.com/abhilashiig/ambit/am`, a pure standard-library allowlist (`strings`, `strconv`, `errors`, `fmt`, `slices`, `maps`, `sort`, `math`, `unicode`, `regexp`, `time` types) and third-party packages listed under `allowedImports` in `am.yaml`. Everything that reaches the outside world is banned: `database/sql`, pgx, `net`, `net/http`, `os`, `os/exec`, `syscall`, `unsafe`, `reflect` and `plugin` (AM-SEC-001). Calling `time.Now` is banned in favor of `c.Clock` so tests control time (AM-CODE-004), and `go` statements are banned in favor of `Enqueue` (AM-PERF-003).

## 8. Data layer

All data access goes through generated, actor-scoped query APIs backed by pgx on Postgres 15 or later, so access rules, tenancy and query limits apply to every query without the action code doing anything.

### 8.1 Scoped queries

Every query the scoped DB builds gets three predicates appended before execution: the tenant filter, the compiled `Read` rule for the current actor, and any filter the action wrote. Field-level read rules become column projections, so hidden fields are never fetched.

```go
page, err := c.DB.Invoices.Query().
	Where(gen.Invoices.Status.Eq(gen.InvoiceStatusSent)).
	OrderBy(gen.Invoices.CreatedAt.Desc()).
	With(gen.InvoiceWithCustomer).
	Page(c, in.Cursor, 50)
```

| Operation | Rule applied | Notes |
| --- | --- | --- |
| `Get(id)` | tenant + `Read` | Invisible rows return `not_found`, never `forbidden`, so existence never leaks |
| `Query().Page(...)` | tenant + `Read` | Keyset pagination only; page size capped by config |
| `Query().First(...)` | tenant + `Read` | Implicit limit 1 |
| `Query().Count(...)` | tenant + `Read` | Allowed only when declared with `Counts()` on the action |
| `Insert(row)` | `Create` + invariants | Tenant ID set from the actor, never from input |
| `Update(id, version, patch)` | `Update`, `UpdateAfter`, `FieldWrite` + invariants | Optimistic concurrency via `version` |
| `Delete(id, version)` | `Delete` | Generated only when the rule is not `Never()` |

There is no unbounded `All()`. A query must end in `Page`, `First`, `Get` or a declared `Each` batch iterator for jobs (AM-DATA-001).

### 8.2 Loading related data

Relations load only through `.With(...)`, which issues one batched query per relation regardless of row count. Lazy loading does not exist, which removes N+1 queries as a category. The checker also flags any scoped DB call inside a loop (AM-DATA-002).

### 8.3 Transactions

- Every query and mutation runs in exactly one transaction, opened before the action and committed after it returns without error.
- Isolation is `READ COMMITTED` by default; an action may declare `Serializable()`, and serialization failures retry automatically up to three times.
- Queries run on a read replica when the action declares `ReplicaOK()` and a replica is configured.
- Each action has a statement timeout (default 5 s) and a total timeout (default 10 s).

### 8.4 Query budget

Each action has a budget for the number of queries (default 20) and rows read (default 10,000). Exceeding it fails the request in development and tests, and logs a warning with a metric in production. Budgets are declared per action with `QueryBudget(...)`, and changes appear in the behavior diff.

### 8.5 Defense in depth with row-level security

The generator also emits Postgres row-level security policies for tenant isolation. The runtime sets `app.tenant_id` and `app.actor_id` per transaction with `SET LOCAL`. If a bug in the query layer ever omitted the tenant filter, the database would still refuse the rows. Role rules stay in the query layer, because duplicating them in RLS adds complexity with little extra protection.

### 8.6 Raw SQL

Raw SQL is available only inside `am.Unsafe(c, "reason", func(tx am.RawTx) error {...})`. Unsafe transactions still carry the tenant settings for RLS, still count against the query budget and always appear in the behavior diff (AM-SEC-003).

## 9. Security model

Ambit's security promise is that the vulnerabilities AI-written backends most often contain cannot be expressed in Ambit action code without an `unsafe` block that reviewers see.

### 9.1 Threats and controls

| Threat | Typical AI-written bug | Ambit control | Enforced by |
| --- | --- | --- | --- |
| Broken object-level access (IDOR) | Fetching a record by ID without an ownership check | `Read` rules compiled into every query; invisible rows return `not_found` | Query layer + RLS |
| Missing function-level authorization | An endpoint with no permission check | `Requires` or `Public` mandatory on every action | Compiler |
| Cross-tenant leaks | A query missing the tenant filter | Implicit tenant filter, RLS backstop, `CrossTenant()` required to opt out | Query layer + RLS + diff |
| Mass assignment | Binding request JSON directly onto a model | Inputs are separate generated types; tenant and owner fields set by the runtime | Codegen |
| SQL injection | String-built queries | Only parameterized generated queries; raw SQL only in `unsafe` | Compiler + checker |
| Server-side request forgery | Fetching a user-supplied URL | `HTTP` effect host allowlist; private and link-local ranges blocked after DNS resolution | Runtime |
| Secrets in code or logs | Hard-coded keys, logging full objects | Secrets only through generated config; `Sensitive()` fields redacted everywhere | Checker + runtime |
| Excessive data exposure | Returning whole records with private fields | Field-level read rules applied at projection | Query layer |
| Abuse and enumeration | No rate limits on public endpoints | Public mutations must declare `RateLimit` | Checker (AM-SEC-005) |
| Race conditions | Read-modify-write without locking | Optimistic concurrency on every update; `Serializable()` available | Runtime |
| Unsafe side effects | Sending email for a rolled-back change | Deferred effects via the transactional outbox | Runtime |

### 9.2 Authentication

- **Sessions.** Server-side session records in Postgres, referenced by an opaque random token in an `HttpOnly`, `Secure`, `SameSite=Lax` cookie. Sessions rotate on login and privilege change.
- **API tokens.** Random tokens shown once, stored as SHA-256 hashes, optionally scoped to a subset of actions.
- **OIDC.** Login through any OpenID Connect provider, using authorization code flow with PKCE.
- **Passwords.** Optional module using Argon2id, breached-password checks and login throttling. Off by default.
- **Service accounts.** Non-human actors with API tokens, bound to one tenant and a fixed role set.

### 9.3 Request protections

Defaults that apply to every route: a 1 MB body limit, strict JSON decoding that rejects unknown fields, UTF-8 validation, CSRF protection for cookie sessions through `Origin` checks, a deny-by-default CORS policy and security headers including `Strict-Transport-Security`, `X-Content-Type-Options` and a restrictive `Content-Security-Policy` for any served HTML.

### 9.4 Secrets and encryption

Secrets are declared in `am.yaml` by name and resolved from environment variables or a configured secret manager at startup. Action code never sees raw secrets; effect providers receive them through their configuration. `Encrypted()` fields use envelope encryption with a key-encryption key from config, and support key rotation through `am keys rotate`.

### 9.5 Audit log

Every mutation writes an audit record in the same transaction: actor, tenant, action, resource IDs, changed field names and a redacted before-and-after diff. Audit records are append-only, and the audit table's Postgres grants deny update and delete to the app role.

### 9.6 Supply chain

New Go module dependencies appear in the behavior diff as high-risk. `am check` runs `govulncheck` and fails on known vulnerabilities reachable from app code. The framework itself keeps its dependency set small: pgx, OpenTelemetry and the Go standard library.

## 10. Effects, outbox and jobs

Side effects and background work run on Postgres alone, with at-least-once delivery, idempotency keys and retries built in, so there is no Redis or message broker to operate.

### 10.1 Deferred effects and the outbox

1. An action calls an effect such as `c.Email.Send(...)`. The call validates its arguments and inserts an outbox row in the action's transaction. Nothing leaves the process yet.
2. If the action fails or the transaction rolls back, the outbox row disappears with it.
3. On commit, the runtime issues `NOTIFY am_outbox`. Dispatchers in every instance listen, and also poll every second as a fallback.
4. A dispatcher claims rows with `SELECT ... FOR UPDATE SKIP LOCKED` and executes them through the effect's provider.
5. Each execution carries an idempotency key derived from the action invocation ID and the effect's sequence number. Providers pass it to the external API where supported.
6. Failures retry with exponential backoff and jitter, by default up to 8 attempts over about 24 hours. Rows that exhaust retries move to a dead-letter state.

`am outbox list`, `am outbox inspect <id>` and `am outbox replay <id>` operate on pending and dead-lettered effects. Outbox lag, attempts and dead-letter counts are exported as metrics.

### 10.2 Immediate effects

An effect declared `Immediate()` executes during the action and returns its result, for example creating a payment intent whose ID the response needs. Immediate effects must pass an idempotency key, are traced as child spans, and may declare a compensating effect that the runtime enqueues if the transaction later rolls back.

### 10.3 Effect providers

Providers implement a small interface and are configured in `am.yaml`. Ambit ships providers for SMTP, a generic HTTP client, S3-compatible storage and signed webhooks; community providers cover specific email, SMS and payment vendors.

```go
type Provider interface {
	Name() string
	Execute(ctx context.Context, e am.EffectCall) (am.EffectResult, error)
	Retryable(err error) bool
}
```

### 10.4 Jobs

- Jobs are rows in a `am_jobs` table, claimed with `FOR UPDATE SKIP LOCKED`, so any number of worker instances can run safely.
- A job runs in its own transaction with its declared `Reads`, `Writes` and `Effects`, exactly like a mutation.
- `RunAs(Enqueuer)` runs the job as a snapshot of the actor who enqueued it; `RunAs(System)` runs it as the tenant's system actor.
- `Concurrency(Key("customer_id"), 1)` serializes jobs sharing a key value.
- Priorities run from 0 to 9, and a job can be delayed with `Enqueue(job).After(d)`.
- Retries follow the job's `Retry` policy; exhausted jobs move to dead-letter and can be replayed with `am jobs replay`.

### 10.5 Schedules

Schedules use standard five-field cron expressions evaluated in UTC unless a time zone is declared. A leader elected through a Postgres advisory lock enqueues scheduled runs, and each run gets a deterministic key so a schedule never fires twice for the same slot, even across restarts.

### 10.6 Process roles

The same binary runs as `--role=web`, `--role=worker` or `--role=all` (the default). Small apps run one process; larger ones scale web and worker instances independently.

## 11. Performance design

Ambit gets its speed from moving work to build time: everything the spec determines is generated as straight-line Go, so the hot path has no reflection, no dynamic routing and no runtime query planning in the framework.

### 11.1 Hot-path choices

| Concern | Choice | Why |
| --- | --- | --- |
| HTTP server | Go standard `net/http` with HTTP/2 | Mature, fast, no dependency risk |
| Routing | Generated static lookup table | Constant-time dispatch, no regex or tree walking |
| JSON | Generated append-based encoders and decoders | No reflection; pooled buffers keep allocations low |
| Database driver | pgx v5 native interface with `pgxpool` | Binary protocol, automatic prepared statement cache |
| Row scanning | Generated scanners into concrete structs | No reflection or map allocation per row |
| Related data | `.With(...)` sent as one `pgx.Batch` | One network round trip for all relations |
| Validation | Generated per input type | Branch-only checks, no tag parsing |
| Logging | `log/slog` with a JSON handler; fields built lazily | Near-zero cost when a level is disabled |

### 11.2 Resource management

- **Connection pool.** Sized by default to twice the CPU count per instance, with a pool-wait timeout that returns 503 rather than queueing indefinitely.
- **Backpressure.** A maximum number of in-flight requests per instance; excess requests get 503 with `Retry-After` instead of degrading every request.
- **Memory.** The runtime sets `GOMEMLIMIT` from the container memory limit when one is detected.
- **Compression.** Responses over 1 KB are compressed when the client accepts gzip or zstd.
- **Graceful shutdown.** On SIGTERM the server stops accepting, drains in-flight requests and jobs up to a deadline, then exits.

### 11.3 Caching

Query actions may declare `Cache(TTL(d), InvalidatedBy(resources...))`. The cache key includes the tenant, the actor's role set and the input, so cached responses never cross access boundaries. Writes to an invalidating resource bump a per-tenant generation counter, which invalidates entries without scanning. The cache is in-process by default; a shared Postgres-backed tier is an option for multi-instance consistency.

### 11.4 Performance feedback for agents

- **Query capture.** During `am test`, the runtime records every query shape each action issues.
- **Plan analysis.** `am perf` runs `EXPLAIN` on captured shapes against a seeded database and flags sequential scans on tables above a size threshold, with the missing index as a suggested spec change (AM-PERF-002).
- **Budgets.** Actions exceeding their query or row budget fail tests (AM-PERF-001).
- **Benchmarks.** `am bench` load-tests declared actions with generated inputs and reports latency percentiles and allocations per request; CI can fail on regressions beyond a threshold.
- **Profiling.** `pprof` endpoints are available on the admin port, behind admin authentication.

## 12. API layer and generated clients

Every query and mutation is exposed automatically as a typed JSON RPC endpoint, described by a generated OpenAPI 3.1 document and consumed through a generated TypeScript client.

### 12.1 Endpoints

| Kind | Method and path | Body |
| --- | --- | --- |
| Query | `POST /api/<module>/<Action>`, or `GET` with query parameters when declared `Get()` | Input JSON |
| Mutation | `POST /api/<module>/<Action>` | Input JSON; `Idempotency-Key` header when `Idempotent()` |
| Health | `GET /_am/health/live`, `GET /_am/health/ready` | — |
| Metrics | `GET /metrics` on the admin port | — |

Paths are stable identifiers derived from module and action names. Renaming an action is a breaking change and the behavior diff reports it as one.

### 12.2 Error format

```json
{
  "error": {
    "code": "not_draft",
    "message": "Invoice is not in draft status.",
    "fields": { "invoice_id": "must be a draft invoice" },
    "traceId": "4bf92f3577b34da6a3ce929d0e0e4736"
  }
}
```

`code` is always one of the framework codes or a declared action error, so clients can switch on it exhaustively. `fields` appears only for validation errors.

### 12.3 Generated TypeScript client

```ts
import { createClient } from "./client";

const api = createClient({ baseUrl: "/api" });

const result = await api.billing.sendInvoice({ invoiceId });
if (!result.ok && result.error.code === "not_draft") {
  // typed: the compiler knows every possible error code
}
```

The client has no runtime dependencies, uses `fetch`, returns discriminated result types rather than throwing, and includes types for every enum, input, output and error code.

### 12.4 Compatibility

The behavior diff classifies API changes as compatible (new optional input, new output field, new action) or breaking (removed or renamed action, new required input, removed output field, narrowed enum). Breaking changes require `Deprecated(...)` on the old shape for at least one release, unless the app sets `api.compatibility: none` for internal-only APIs.

## 13. Migrations

Migrations are generated from the difference between app models, classified by risk, and applied with lock and statement timeouts; destructive changes are impossible without a two-step plan.

### 13.1 Workflow

1. The agent changes the spec and runs `am gen`.
2. `am migrate plan` diffs the new model against the last applied model and writes a numbered SQL file plus a plan file with each step's classification.
3. `am migrate verify` applies the plan to a fresh database and, when configured, to a snapshot of production data, then runs invariant checks.
4. `am migrate apply` runs at deploy time with `lock_timeout` (default 2 s) and `statement_timeout` set, retrying lock failures with backoff.
5. Applied migration files are immutable. Editing one fails the check (AM-MIG-002).

### 13.2 Change classification

| Change | Class | Generated strategy |
| --- | --- | --- |
| Add resource | Safe | `CREATE TABLE` with constraints and RLS policies |
| Add optional field | Safe | `ADD COLUMN` nullable |
| Add field with default | Safe | `ADD COLUMN ... DEFAULT`, metadata-only in Postgres 11+ |
| Add required field without default | Needs backfill | Add nullable, backfill in batches, then `SET NOT NULL` via a validated `CHECK` |
| Add index | Safe | `CREATE INDEX CONCURRENTLY`, outside the transaction |
| Add invariant | Needs validation | `ADD CONSTRAINT ... NOT VALID`, then `VALIDATE CONSTRAINT` |
| Add enum value | Safe | Replace the `CHECK` constraint with a widened one |
| Remove enum value | Destructive | Requires a backfill mapping for existing rows |
| Rename field | Needs plan | Declared with `RenamedFrom("old")`; otherwise treated as remove plus add |
| Change field type | Needs plan | Expand and contract: new column, dual write, backfill, switch, remove |
| Remove field or resource | Destructive | Two phases: `Deprecated()` first, removal in a later release with `--allow-destructive` |

Needs-backfill and needs-plan changes rank medium-risk in the behavior diff; destructive changes rank high. Applying a destructive step without an explicit flag fails (AM-MIG-001).

### 13.3 Backfills

Backfills run as resumable jobs in batches of 1,000 rows by default, keyed by primary key, with progress recorded in `am_migrations`. A deploy can proceed while a backfill runs, because the generated code reads both old and new shapes during the expand phase.

## 14. Checker and rule catalog

`am check` is the agent's single feedback command: it runs the Go compiler, `go vet`, `govulncheck` and Ambit's analyzers, and returns one list of diagnostics, each with a rule ID, a location and a concrete fix.

### 14.1 Behavior

- Analyzers are built on Go's standard `golang.org/x/tools/go/analysis` framework and run in one pass with the compiler's type information.
- Checks are incremental: only packages whose inputs changed are re-analyzed, keyed by content hash.
- Errors fail the check. Warnings are reported but pass, unless `--strict` is set, which CI should use.
- A rule can be suppressed on one line with `//am:allow AM-XXX-000 reason`. Suppressions require a reason and appear in the behavior diff.

### 14.2 Diagnostic format

```json
{
  "rule": "AM-DATA-002",
  "severity": "error",
  "file": "app/billing/actions/send_reminders.go",
  "line": 18,
  "column": 3,
  "message": "Scoped DB call inside a loop issues one query per iteration.",
  "fix": "Load customers with .With(gen.InvoiceWithCustomer) on the outer query, then read inv.Customer inside the loop.",
  "docs": "am explain AM-DATA-002"
}
```

### 14.3 Rule catalog

| ID | Severity | Rule |
| --- | --- | --- |
| AM-SPEC-001 | error | Spec file contains non-declarative code: functions, control flow or non-spec imports |
| AM-SPEC-002 | error | Spec references an unknown role, resource, field or action |
| AM-SPEC-003 | error | Rule or invariant cannot be compiled to SQL |
| AM-SPEC-004 | error | Query or mutation declares neither `Requires` nor `Public` |
| AM-SEC-001 | error | Action package imports a banned package |
| AM-SEC-002 | error | Environment variables or secrets accessed outside generated config |
| AM-SEC-003 | error | `unsafe` block has no reason |
| AM-SEC-004 | error | A `Sensitive()` field is passed to a logger or error message |
| AM-SEC-005 | error | Public mutation has no rate limit |
| AM-SEC-006 | warning | HTTP effect URL built from input; host checked only at runtime |
| AM-DATA-001 | error | Query has no terminal bound: `Page`, `First`, `Get` or `Each` |
| AM-DATA-002 | error | Scoped DB call inside a loop |
| AM-DATA-003 | error | Error returned by a DB or effect call is ignored |
| AM-CODE-001 | error | Generated file edited, or committed app model differs from the spec |
| AM-CODE-002 | error | Import of another module's internal packages |
| AM-CODE-003 | warning | `switch` over a generated enum is not exhaustive |
| AM-CODE-004 | error | Wall-clock time used instead of `c.Clock` |
| AM-PERF-001 | error | Query or row budget exceeded during tests |
| AM-PERF-002 | warning | Captured query plans a sequential scan on a large table |
| AM-PERF-003 | error | `go` statement in action code |
| AM-MIG-001 | error | Destructive migration step without explicit approval |
| AM-MIG-002 | error | Applied migration file was modified |
| AM-TEST-001 | warning | Action has no test |
| AM-DEP-001 | error | Known vulnerability reachable from app code |

`am explain <ID>` prints the rule's rationale, a failing example, the corrected version and links to the relevant spec section. The rule set is versioned with the framework, and new rules arrive as warnings for one minor release before becoming errors.

## 15. Behavior diff

`am diff` is the human review interface: it compares the app model and code facts between two git revisions and reports, ranked by risk, what the app can now do that it could not before. It is computed by static comparison, never by an LLM.

### 15.1 What is compared

| Category | High risk | Medium risk | Low risk |
| --- | --- | --- | --- |
| Access | Any rule widened; `CrossTenant()` added; new `Public()` action | `FieldRead` widened | Rule narrowed |
| Actions | New action that writes data | Inputs or outputs changed | New query action |
| Effects | New effect or new HTTP host; effect made `Immediate()` | Retry policy changed | Effect removed |
| Data model | Destructive migration | Needs-backfill or needs-plan migration; invariant removed | Field or resource added; invariant added |
| Code | New `unsafe` block; new `//am:allow` suppression | — | — |
| Dependencies | New Go module or major version | Minor version bump | Patch bump |
| API | Breaking change | Deprecation | Compatible addition |
| Limits | Rate limit removed or raised more than 10x; query budget raised | Timeout raised | Limit tightened |

### 15.2 How widening is decided

Access rules are compared as expression trees. A rule counts as widened when the new rule accepts some actor and row the old one rejected: adding a role to an `Or`, removing a clause from an `And`, or replacing a rule with `Always()`. When structural comparison cannot prove a change is a narrowing, it is reported as widened. False alarms are acceptable; missed widenings are not.

### 15.3 Output

```
am diff --base main

HIGH  access    billing.Invoice.update  widened: accountant may now update invoices with status "sent" or "paid" (was "draft", "sent")
HIGH  effect    billing.MarkPaid        new HTTP host: api.payments.example.com
MED   migration billing.Invoice.paid_at needs backfill: field becomes required
LOW   action    billing.ListOverdue     new query action, requires member

4 changes · 2 high · 1 medium · 1 low · 0 unsafe blocks · 0 new dependencies
```

Formats are terminal text, Markdown for pull request comments and JSON for tooling. A provided GitHub Action posts the Markdown report on each pull request and can require an approving review from a code owner whenever any high-risk change is present.

## 16. Testing

`am test` combines tests the framework generates from the spec with tests the agent writes, all against a real Postgres database, with effects captured by fakes instead of sent.

### 16.1 Test environment

- `am test` starts a disposable Postgres (an embedded binary or a container, configurable) and creates a template database with the current schema once per run.
- Each test gets its own database cloned from the template, so tests run in parallel without interfering.
- Effects are replaced with recording fakes; the HTTP effect fails any request without a declared stub.
- `c.Clock` is a controllable fake clock, so time-dependent logic is deterministic.

### 16.2 Generated tests

| Generated test | Derived from | What it proves |
| --- | --- | --- |
| Access matrix | Every `Read`, `Create`, `Update`, `Delete` and field rule | For each rule, an actor that satisfies it succeeds and one that does not is refused |
| Tenant isolation | Every tenant resource and action | An actor in tenant A can never read or write tenant B's rows through any action |
| Invariant enforcement | Every invariant | Rows violating it are rejected by the database or the pre-commit check |
| Input validation | Every input type | Boundary values pass, out-of-range and malformed values return `invalid` |
| Authorization | Every action's `Requires` | Callers without the required role get `forbidden` |

Generated tests live in `gen/` and regenerate with the spec, so they can never drift from the rules they test.

### 16.3 Writing tests

```go
func TestSendInvoice(t *testing.T) {
	env := amtest.New(t)
	org := env.Tenant()
	acct := env.Actor(org, app.Accountant)
	inv := env.Create(gen.Invoice{Tenant: org, Status: gen.InvoiceStatusDraft, Amount: am.MoneyOf(5000, "USD")})

	out, err := env.Call(acct, actions.SendInvoice, gen.SendInvoiceInput{InvoiceID: inv.ID})

	require.NoError(t, err)
	require.Equal(t, gen.InvoiceStatusSent, out.Status)
	require.Len(t, env.Emails(), 1)
	env.AssertQueriesAtMost(3)
}
```

`env.Call` runs the action through the full pipeline, including authorization, validation, transaction, invariants and output filtering, so tests exercise the same path as production.

### 16.4 Property tests

`amtest.Fuzz(actions.X)` generates random valid inputs and random actors, runs the action repeatedly, and asserts after every run that all invariants hold and that no actor observed a row its `Read` rule excludes.

## 17. Agent interface and developer tooling

Ambit treats AI agents as its primary users: every command has machine-readable output, context comes in compact bundles sized for a model's window, and one fixed loop takes a feature from spec to reviewed change.

### 17.1 The standard loop

1. **Read context.** `am context billing` returns the module's spec, generated signatures, declared errors and relevant conventions.
2. **Change the spec.** Add or edit resources, rules and actions in `spec.go`.
3. **Generate.** `am gen` regenerates code and stubs any new action with a function returning `am.NotImplemented()`.
4. **Implement.** Write the action bodies in `actions/`.
5. **Check.** `am check --json` until it reports no errors.
6. **Test.** `am test --json` until generated and written tests pass.
7. **Review.** `am diff --base main` produces the report the human approves.

Steps 4 to 6 repeat until clean. The generated `AGENTS.md` describes this loop, so agents follow it without custom prompting.

### 17.2 Context bundles

`am context <module>` prints a compact, deterministic bundle: the module's spec, the generated context, input and output types for each action, the names and signatures of referenced types from other modules, and the conventions that apply. `am context --action billing.SendInvoice` narrows it to one action. Bundles never include generated implementation code, only signatures, to keep them small.

### 17.3 Generated AGENTS.md

Regenerated by `am gen`, it contains the standard loop, the project's modules with one-line descriptions, the commands, the import rules, the rule catalog summary and links to `am explain`. Teams add project-specific notes in a marked section that regeneration preserves.

### 17.4 Dev server

`am dev` runs the app with hot reload on file change, seeds data from `seed/` files, and serves an introspection API agents can call to verify behavior at runtime:

| Endpoint | Returns |
| --- | --- |
| `GET /_am/actions` | Every action with its input schema and required roles |
| `POST /_am/call` | Runs an action as a chosen seeded actor and returns output plus trace ID |
| `GET /_am/traces/<id>` | The full trace: queries with plans, effects, timings, policy decisions |
| `GET /_am/logs?since=` | Structured logs since a cursor |
| `POST /_am/reset` | Resets the database to the seed state |

The same capabilities are exposed as a Model Context Protocol server through `am dev --mcp`, so agent tools can call them as native tools. The introspection API exists only in development builds.

### 17.5 Docs for models

`am docs --llm` writes the complete framework reference as one plain-text file under 40k tokens, versioned with the installed framework. Agents load it when the context bundle is not enough, which reduces invented APIs from older training data.

## 18. Observability and operations

A Ambit app ships as one statically linked binary with built-in OpenTelemetry tracing, structured logs, Prometheus metrics, health checks and embedded migrations.

### 18.1 Telemetry

- **Traces.** One span per action, with child spans for each query, effect, call and job. Spans carry the action name, actor type, tenant ID and policy decision, never `Sensitive()` values. Export uses OTLP.
- **Logs.** JSON through `log/slog`, every line carrying the trace ID, action and tenant. `c.Log` is pre-scoped to the action.
- **Metrics.** Per-action request count, error count by code and latency histogram; DB pool usage; query counts per action; outbox lag and dead letters; job queue depth and age.

### 18.2 Configuration

```yaml
app: myapp
database:
  url: ${DATABASE_URL}
  replicaUrl: ${DATABASE_REPLICA_URL}
  maxConns: 32
secrets:
  smtpPassword: ${SMTP_PASSWORD}
  encryptionKey: ${AM_ENCRYPTION_KEY}
effects:
  email:
    provider: smtp
    host: smtp.example.com
    password: secret:smtpPassword
limits:
  requestBodyBytes: 1048576
  actionTimeout: 10s
  queryBudget: 20
allowedImports:
  - github.com/shopspring/decimal
telemetry:
  otlpEndpoint: ${OTEL_EXPORTER_OTLP_ENDPOINT}
```

Configuration is validated at startup against a generated schema; a missing secret or unknown key stops the process with a clear message rather than failing later.

### 18.3 Deployment

- **Build.** `am build` produces one binary with migrations, email templates and static assets embedded.
- **Migrate.** `myapp migrate apply` runs as a release step before new instances start. Instances refuse to start against a schema older than their model.
- **Run.** Instances are stateless and scale horizontally. Leader-only work such as schedules uses Postgres advisory locks.
- **Health.** `/_am/health/live` reports the process is up; `/_am/health/ready` also checks the database and the migration version.
- **Admin port.** Metrics, `pprof` and the admin API listen on a separate port that should not be public.

A generated `Dockerfile` builds a minimal distroless image.

## 19. CLI reference

Every command accepts `--json` for machine-readable output and exits non-zero on failure.

| Command | Purpose |
| --- | --- |
| `am init <name>` | Create a new app with an example module, config and CI workflow |
| `am new module <name>` | Add a module folder with an empty spec |
| `am new action <module>.<Action>` | Add an action declaration and an implementation stub |
| `am gen` | Compile the spec, write `app.model.json`, regenerate code, clients and `AGENTS.md` |
| `am check` | Compile and analyze; `--strict` treats warnings as errors |
| `am test` | Run generated and written tests against disposable Postgres |
| `am diff --base <rev>` | Risk-ranked behavior diff; `--format text, markdown or json` |
| `am dev` | Run with hot reload, seed data and introspection; `--mcp` for agent tools |
| `am context <module>` | Print a compact context bundle; `--action` narrows it |
| `am explain <rule>` | Explain a checker rule with examples |
| `am migrate plan, verify, apply, status` | Plan, verify, apply and inspect migrations |
| `am perf` | Analyze captured query plans for missing indexes |
| `am bench <action>` | Load-test an action and report latency and allocations |
| `am outbox list, inspect, replay` | Operate on pending and dead-lettered effects |
| `am jobs list, inspect, replay` | Operate on jobs |
| `am keys rotate` | Rotate encryption keys for `Encrypted()` fields |
| `am client ts --out <dir>` | Write the TypeScript client to another location |
| `am docs --llm` | Write the single-file framework reference for models |
| `am build` | Produce the production binary |
| `am upgrade` | Upgrade the framework version and apply automated code changes |

## 20. Prior art and differentiation

Each piece of Ambit exists somewhere; what is new is combining them around AI as the primary author and a machine-computed behavior diff as the review surface.

| Project | Language | What it shares with Ambit | What Ambit adds |
| --- | --- | --- | --- |
| ent | Go | Schema as Go code, code generation, privacy policies | Policies compiled into SQL with an RLS backstop, effects and capabilities, behavior diff, migration safety classes |
| sqlc | Go | Type-safe generated query code | Actor-scoped queries, access rules, a full application runtime |
| Encore | Go | Declarative backend, generated clients, built-in infrastructure | Data-level access rules, invariants, capability-scoped action code, review diff |
| Ash | Elixir | Declarative resources with policies | Go performance and ecosystem, AI-author tooling, behavior diff |
| Supabase | Postgres platform | Row-level security as the access model | Rules declared once and enforced in both app and database, plus actions, effects and jobs |
| Rails, Django | Ruby, Python | Conventions, migrations, batteries included | Guarantees by construction instead of conventions; no implicit behavior |

This comparison reflects general knowledge rather than a fresh review of each project; verify current features before using it publicly.

## 21. Roadmap and MVP scope

Ambit ships in three releases, each released only when its gate is met; v0.1 is the MVP that proves the core guarantees work end to end.

&#91;embedded content: roadmap · 3 releases, 3 gates\]

Phases are sequenced by gates, not dates. The launch benchmark from section 22 runs at the second gate, because the security comparison is the main announcement.

## 22. Launch benchmark plan

Ambit launches with an open, reproducible benchmark showing how AI-built backends compare across frameworks on security, correctness, speed of building and runtime performance.

### 22.1 Method

1. Write three reference app specifications in plain English, each with acceptance tests: a multi-tenant invoicing SaaS, a two-sided marketplace with payments, and an internal approvals tool.
2. Pick comparison stacks: Rails, Django, NestJS, Go with chi and sqlc, and Ambit.
3. For each app and stack, have the same coding agent build the app from the specification 10 times, with identical prompts apart from stack-specific setup.
4. Run each result through the measurements below.
5. Publish the harness, prompts, generated code and raw results so anyone can rerun or challenge them.

### 22.2 Measurements

| Measure | How |
| --- | --- |
| Acceptance | Share of acceptance tests passing |
| Time to green | Agent wall-clock time and tokens until acceptance tests pass |
| Security findings | Automated scanning plus a fixed manual audit checklist: IDOR, missing authorization, cross-tenant leaks, mass assignment, injection, SSRF |
| Data safety | Destructive or locking migrations produced during feature changes |
| Review surface | Lines a reviewer must read versus lines in the behavior diff |
| Throughput and latency | Requests per second and p50, p99 latency under a fixed load profile on identical hardware |
| Footprint | Memory at idle and under load |

### 22.3 Fairness rules

Each stack gets an idiomatic setup prepared by someone experienced in it, the same agent and model version, the same time budget and the same database. Results are reported per run, not only as averages, and any run excluded is listed with the reason.

## 23. Open questions

- [ ] **Name.** "Ambit" is a placeholder; check trademark, domain and GitHub availability before announcing.
- [ ] **Spec readability.** Is Go builder syntax readable enough for human review, or should spec diffs always be shown through `am diff` only?
- [ ] **Cross-row invariants.** Should v1 support aggregate rules such as "sum of payments never exceeds invoice amount", and at what cost to write performance?
- [ ] **File uploads.** Direct-to-storage signed uploads as a built-in, or left to the `Storage` effect?
- [ ] **Real-time.** Server-sent event subscriptions to query results, and how they interact with access rules and caching.
- [ ] **Multi-region.** Out of scope for v1, but which design choices would block it later?
- [ ] **Licensing.** Apache 2.0 or MIT for the framework; whether any hosted tooling is commercial.
- [ ] **Template data typing.** Email and webhook payloads currently accept any value; should templates declare typed data in the spec?
- [ ] **Escape-hatch policy.** Should teams be able to forbid `unsafe` entirely in some modules through `am.yaml`?
