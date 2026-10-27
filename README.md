<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="docs/assets/logo/svg/ambit-mark-dark.svg">
    <img src="docs/assets/logo/svg/ambit-mark.svg" alt="Ambit logo: an indigo ring with an amber dot inside it" width="96" height="96">
  </picture>
</p>

<h1 align="center">Ambit</h1>

<p align="center">
  <strong>The Go backend framework for AI-written code that people can safely review.</strong><br>
  Declare what exists and who may do what. Ambit generates the API, the Postgres schema with
  row-level security, migrations, tests and a TypeScript client, and it keeps agent-written
  code inside those bounds.
</p>

<p align="center">
  <a href="docs/getting-started.md">Getting started</a> ·
  <a href="docs/spec.md">Spec language</a> ·
  <a href="docs/cli.md">CLI</a> ·
  <a href="example/myapp">Example app</a> ·
  <a href="#faq">FAQ</a>
</p>

<p align="center">
  <img alt="Go 1.25+" src="https://img.shields.io/badge/Go-1.25%2B-00ADD8?logo=go&logoColor=white">
  <img alt="Postgres 15+" src="https://img.shields.io/badge/Postgres-15%2B-4169E1?logo=postgresql&logoColor=white">
  <img alt="Row-level security" src="https://img.shields.io/badge/multi--tenant-row--level%20security-23206B">
  <a href="LICENSE"><img alt="MIT License" src="https://img.shields.io/badge/license-MIT-F2A516"></a>
</p>

---

**Ambit is an open-source Go framework for building secure, multi-tenant backends with AI
coding agents** such as Claude Code, Cursor and Copilot. You write a small declarative spec.
Ambit turns it into a typed Go API, a Postgres schema with row-level security, an OpenAPI
document, a TypeScript client, migrations and tests. The action code an agent writes can only
reach the data and side effects the spec grants it.

Every change ends in a **behavior diff**: a risk-ranked list of what the app can now do that
it could not before. That diff, not the generated code, is what a person reviews.

```go
var Invoice = s.Resource("Invoice",
	s.Tenant(),
	s.Fields(
		s.Ref("customer", customers.Customer),
		s.Money("amount"),
		s.Enum("status", "draft", "sent", "paid").Default("draft"),
	),
	s.Invariants(s.Check("amount_positive", s.F("amount").Gt(0))),
	s.Access(
		s.Read(app.Member),
		s.Update(app.Admin.Or(app.Accountant).And(s.F("status").In("draft", "sent"))),
		s.Delete(s.Never()),
	),
)
```

From that declaration, every query an action makes is limited to the caller's tenant and
filtered by the `Read` rule. Writes are checked against `Update`. The invariant becomes a
database `CHECK`. And no action can delete an invoice.

## Why Ambit

AI agents write backend code quickly, but reviewing that code line by line doesn't scale, and
the expensive mistakes are rarely syntax errors. They're a missing tenant filter, an
authorization check that's too broad, an email sent from a transaction that later rolled
back, or a migration that drops a column. Ambit makes those mistakes either impossible to
write or visible in review:

- **Access rules enforced twice.** Rules compile into every query, and a Postgres
  row-level-security policy isolates tenants even if a query forgets.
- **Actions with a fixed contract.** Each action gets a generated context holding exactly the
  data, effects and calls its spec declared, and nothing else.
- **Effects that cannot outlive a rollback.** Email, webhooks, HTTP calls and storage writes go
  through a transactional outbox. Immediate effects can name a compensation.
- **Safe migrations.** Every schema change is classified (safe, needs backfill, destructive),
  destructive steps need an explicit flag, and backfills run resumably.
- **Generated tests.** Tenant isolation, the access matrix, invariants, authorization and input
  validation are tested from the spec, against a real Postgres.
- **A reviewable behavior diff.** New capabilities, widened access rules, new effects and risky
  migrations are ranked by risk, so review time goes where the risk is.
- **One binary.** It carries its migrations, plus metrics, OTLP traces, health checks and a
  generated Dockerfile.
- **Built for agents.** `am check --json`, `am test --json`, `am context`, `am docs --llm` and
  an MCP server (`am dev --mcp`) give agents machine-readable feedback in a tight loop.

## Quickstart

You need Go 1.25+ and Postgres 15+.

```sh
# Build the CLI from this repository.
go install ./cmd/am

# Create an app that builds against this checkout.
am init myapp --framework "$(pwd)"
cd myapp

am test            # starts a throwaway Postgres if none is configured
am check           # compiler, vet, model checks and the Ambit analyzers
```

`am init` writes an app with one module (`app/tasks`), runs `am gen`, and stubs every action
with `am.NotImplemented()`. Next, implement `app/tasks/actions.go`, then run it:

```sh
export AM_DATABASE_URL=postgres://localhost:5432/myapp?sslmode=disable
am migrate plan && am migrate apply
am dev             # http://localhost:8080, with /_am/actions to explore
```

## Documentation

| Guide | Read it for |
| --- | --- |
| [Getting started](docs/getting-started.md) | The project layout and the change loop, step by step |
| [The spec language](docs/spec.md) | Resources, fields, access rules, invariants, actions and effects |
| [Writing actions](docs/actions.md) | The action context, queries, writes, errors and effects |
| [Testing](docs/testing.md) | `amtest`, generated tests, fuzzing and benchmarks |
| [Migrations](docs/migrations.md) | Planning, applying, backfills and framework upgrades |
| [Operations](docs/operations.md) | Configuration, secrets, encryption, auth, deployment and telemetry |
| [CLI reference](docs/cli.md) | Every `am` command |
| [Example app](example/myapp) | A billing app with two modules, tests and migrations |

Agents can load a single-file reference with `am docs --llm`, generated from the installed
framework source. A short machine-readable summary of the project is in [llms.txt](llms.txt).
The design rationale lives in the
[technical specification](Ambit%20Go%20framework%20for%20AI-built%20backends%20—%20technical%20specification.md).

## FAQ

### What is Ambit?
Ambit is a Go framework for multi-tenant SaaS backends whose application code is mostly written
by AI coding agents and reviewed by people. A declarative spec defines resources, access rules,
invariants and actions. Ambit generates the API, database schema, migrations, tests and
clients from it, and it confines hand-written (or agent-written) code to what the spec allows.

### How does Ambit keep AI-generated code safe?
Agent-written action code receives a generated context that holds only the tables, effects and
calls its spec declared. Every query is scoped to the caller's tenant and filtered by the
resource's access rules, and Postgres row-level security enforces tenant isolation a second
time. Static analyzers (`am check`) ban raw database access, network calls and other escape
hatches outside an audited `am.Unsafe` block.

### What does a reviewer actually review?
The behavior diff (`am diff`). It lists what the app can do after a change that it couldn't do
before: new actions, widened access rules, new side effects, destructive migrations. Each item
is ranked by risk.

### How is Ambit different from an ORM or a code generator like sqlc or Ent?
Those tools generate data access code. Ambit also owns authorization, tenant isolation, side
effects, migrations, tests and the review artifact, and it restricts what the remaining
hand-written code can import and touch. The goal is a backend whose safety doesn't depend on
every line being reviewed.

### Does Ambit work with Claude Code, Cursor or GitHub Copilot?
Yes. Ambit is agent-agnostic. Every app gets a generated `AGENTS.md`, and the CLI emits JSON
diagnostics. `am dev --mcp` exposes the dev loop over the Model Context Protocol.

### What database does Ambit use?
PostgreSQL 15 or later. Ambit relies on row-level security, transactional DDL and advisory
locks.

### Is Ambit production-ready?
Ambit is young. Its core (scoped data access, row-level security, outbox, migrations, generated
tests) is covered by tests against a real Postgres. Read the
[technical specification](Ambit%20Go%20framework%20for%20AI-built%20backends%20—%20technical%20specification.md)
for what is deliberately out of scope.

### Can I get help building a product on Ambit?
Yes. Ambit is built and maintained by [Grab Connected Tech](https://www.grabians.com/), a software services
company. See [Built by Grab Connected Tech](#built-by-grab-connected-tech) below.

## Repository layout

| Path | What it is |
| --- | --- |
| `spec/` | The declarative builders spec files use (`s.Resource`, `s.Mutation`, ...) |
| `am/` | Scalars, errors and actors that action code uses (`am.ID`, `am.Money`, `am.Opt`) |
| `runtime/` | The HTTP server, request pipeline, scoped data layer, outbox, jobs and auth |
| `codegen/` | Spec model to Go, SQL, OpenAPI, TypeScript and AGENTS.md |
| `migrate/` | DDL, change classification and migration plans (`migrate/run` applies them) |
| `compile/`, `checker/`, `policy/`, `diff/` | Model validation, static analysis, rule compilation and the behavior diff |
| `amtest/` | The test harness |
| `cmd/am/` | The CLI |
| `docs/assets/logo/` | The Ambit logo, favicons and usage notes |

## Built by Grab Connected Tech

Ambit is designed, built and maintained by **[Grab Connected Tech](https://www.grabians.com/)**, a
software services company that builds and maintains custom web and mobile applications, DevOps
and cloud infrastructure for startups, growing companies and enterprises.

The approach behind Ambit is the one Grab Connected Tech uses with clients: deliver quickly, keep the code
reviewable, and treat long-term maintenance as part of the job. Grab Connected Tech works as a long-term
engineering partner, with clear communication and competitive, personalized pricing.

If you want to build a product on Ambit, bring AI-assisted development into an existing team
safely, or get help with a Go, Rails, Node or React codebase, write to
**[hello@grabians.com](mailto:hello@grabians.com)**.
