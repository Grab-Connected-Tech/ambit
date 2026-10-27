# Keystone

Keystone is a Go framework for backends that are mostly written by AI agents and reviewed
by people. You declare *what exists and who may do what* in a small spec. Keystone turns
that spec into a typed Go API, a Postgres schema with row-level security, an OpenAPI
document, a TypeScript client, migrations and tests. The action code an agent writes can
only reach the data and side effects the spec grants it.

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

## What you get

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
- **One binary.** It carries its migrations, plus metrics, OTLP traces, health checks and a
  generated Dockerfile.

## Quickstart

You need Go 1.25+ and Postgres 15+.

```sh
# Build the CLI from this repository.
go install ./cmd/ks

# Create an app that builds against this checkout.
ks init myapp --framework "$(pwd)"
cd myapp

ks test            # starts a throwaway Postgres if none is configured
ks check           # compiler, vet, model checks and the Keystone analyzers
```

`ks init` writes an app with one module (`app/tasks`), runs `ks gen`, and stubs every action
with `ks.NotImplemented()`. Next, implement `app/tasks/actions.go`, then run it:

```sh
export KS_DATABASE_URL=postgres://localhost:5432/myapp?sslmode=disable
ks migrate plan && ks migrate apply
ks dev             # http://localhost:8080, with /_ks/actions to explore
```

## Documentation

| Guide | Read it for |
| --- | --- |
| [Getting started](docs/getting-started.md) | The project layout and the change loop, step by step |
| [The spec language](docs/spec.md) | Resources, fields, access rules, invariants, actions and effects |
| [Writing actions](docs/actions.md) | The action context, queries, writes, errors and effects |
| [Testing](docs/testing.md) | `kstest`, generated tests, fuzzing and benchmarks |
| [Migrations](docs/migrations.md) | Planning, applying, backfills and framework upgrades |
| [Operations](docs/operations.md) | Configuration, secrets, encryption, auth, deployment and telemetry |
| [CLI reference](docs/cli.md) | Every `ks` command |
| [Example app](example/myapp) | A billing app with two modules, tests and migrations |

Agents can load a single-file reference with `ks docs --llm`, generated from the installed
framework source. The design rationale lives in the
[technical specification](Keystone%20Go%20framework%20for%20AI-built%20backends%20—%20technical%20specification.md).

## Repository layout

| Path | What it is |
| --- | --- |
| `spec/` | The declarative builders spec files use (`s.Resource`, `s.Mutation`, ...) |
| `ks/` | Scalars, errors and actors that action code uses (`ks.ID`, `ks.Money`, `ks.Opt`) |
| `runtime/` | The HTTP server, request pipeline, scoped data layer, outbox, jobs and auth |
| `codegen/` | Spec model to Go, SQL, OpenAPI, TypeScript and AGENTS.md |
| `migrate/` | DDL, change classification and migration plans (`migrate/run` applies them) |
| `compile/`, `checker/`, `policy/`, `diff/` | Model validation, static analysis, rule compilation and the behavior diff |
| `kstest/` | The test harness |
| `cmd/ks/` | The CLI |
