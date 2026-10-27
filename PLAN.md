# Keystone — implementation plan (working document)

Source of truth: `Keystone Go framework for AI-built backends — technical specification.md`
(spec is 1069 lines; grep it for details instead of re-reading it whole).

## Context strategy (how this build is driven)
1. Read the spec once, then never re-read it wholesale. Later lookups are `grep` for a section
   heading / rule ID / table only.
2. This file is the durable memory: contracts, decisions and a status table live here so the
   spec doc itself never has to be reloaded into context.
3. Build bottom-up in layers; the compiler is the oracle. Verify with `go build ./...` /
   `go vet ./...` / `go test ./...` instead of re-reading source files.
4. Keep files small and single-purpose; use `edit` for changes, never rewrite a file just to
   look at it (`read` with offset/limit for the part needed).
5. Anything deferred is written down here rather than remembered.

## Environment facts
- Go 1.24.2 (darwin/arm64), module proxy reachable.
- Postgres 15.17 running on localhost:5432, user = $USER (superuser). Integration tests may use
  it; they must skip cleanly when `KS_TEST_DB_URL` is unset/unreachable.
- Deps allowed: `pgx/v5`, `google/uuid` (+ stdlib only). Deviation: checker analyzers are built
  on `go/ast` + `go/types` (stdlib) instead of `x/tools/go/analysis`, to keep the dependency set
  minimal (§2.10 "boring infrastructure"). Same pass structure, same diagnostics.

## Repo layout (single module `keystone.dev/keystone`)
```
ks/         public scalars, errors, Actor, Clock, Opt, ID, Money, Decimal, Date, Email, URL, Unsafe
spec/       declarative builders + init-time registry + raw model dump (no runtime behavior)
model/      app model structs, canonical JSON (sorted keys), hash, load/save
compile/    raw dump -> validated canonical model (references, cycles, rules->SQL)
policy/     expression tree -> SQL (WHERE, CHECK, RLS), -> Go evaluator, structural compare
codegen/    model -> Go (types, scoped DB, contexts, routes, entrypoint), SQL, TS client,
            openapi, AGENTS.md ; text/template + go/format, byte-deterministic
migrate/    model DDL, live-schema diff, classification, plan files, apply
diff/       two models + facts -> risk-ranked report (text | markdown | json)
checker/    KS-* rule catalog + ast/type based analyzers + `explain` text
runtime/    HTTP server, pipeline, scoped queries, tx, RLS SET LOCAL, outbox, jobs, schedules,
            auth(sessions/tokens/OIDC-shape), audit, ratelimit, config, metrics, health
kstest/     harness: template DB + per-test clone, recording effect fakes, fake clock, env.Call
cmd/ks/     CLI: init gen check test diff context explain migrate dev client build
example/myapp/  reference app (app.go + customers + billing) with committed gen/ output
```

## Core contracts (pin these so files stop drifting)
- `model.App` = {SchemaVersion, App{Name,Tenant,Defaults}, Roles[], Modules[], Identity, Unsafe[]}
  - `model.Resource` = {Module,Name,Table,Tenant bool,Fields[],Indexes[],Invariants[],Access}
  - `model.Field` = {Name,Col,GoName,Type *Type,Optional,Default,Unique,Immutable,Sensitive,RefTo}
  - `model.Action` = {Module,Name,Kind,Requires *policy.Node,Pub bool,Input[],Output,Reads[],Writes[],
    Calls[],Effects[],Errors[],Limits{Rate,Timeout,QueryBudget,RowBudget},CrossTenant,Idempotent,
    RunAs,Retry,Concurrency,Cache,Deprecated}
  - everything in the model is sorted (modules by name, fields by name, roles by name) so
    canonical JSON is byte-stable; `model.Hash(app)` = sha256 of canonical bytes.
- `policy.Node` = {Op, Path[], Roles[], Values[], Kids[]} — the one expression IR for access
  rules, invariants and column predicates.
  - `policy.CompileSQL(n, ctx)` -> (sql, args) with tokens `$KS_ACTOR_ID`, `$KS_ACTOR_TENANT`,
    `$KS_ACTOR_ROLES`, `$KS_NOW` that the query layer turns into positional params.
  - `policy.EvalGo(n, row, actor)` — used by behavior diff and field-level output filtering.
  - `policy.Compare(old,new)` -> {Same|Narrowed|Widened|Unknown} (§15.2: unknown => widened).
- Scoped query contract (§8.1): every SELECT gets `tenant_id = $KS_ACTOR_TENANT` AND
  `ReadPolicy` AND action filter; hidden fields drop out of the column projection.
- Action signature (§7.2): `func Name(c *gen.NameCtx, in gen.NameInput) (*gen.NameOut, error)`;
  the generated entrypoint asserts it with a `var _ = Name` type assert so build fails on drift.

## Layers and gates
- **L1 ks, spec, model, policy** — gate: `go test ./ks ./spec ./model ./policy` green, incl.
  SQL-compile table tests and rule widening tests.
- **L2 compile, migrate, diff, checker** — gate: tests green; fixture model -> DDL + plan + diff
  output matches the §13.2 / §15.3 examples.
- **L3 runtime** — gate: integration test on real Postgres proving: IDOR invisible, tenant
  isolation (also with a deliberately unfiltered query, blocked by RLS), invariant CHECK,
  optimistic-concurrency 409, deferred outbox rolled back on error, job claim + retry, rate limit,
  audit row written.
- **L4 codegen + example app** — gate: `go build ./example/...`, `ks check` clean,
  `ks test` green on the reference app, `ks diff` non-empty vs. base.

## Status
- **L1 done** `ks`, `policy`, `model`, `spec` — DSL builds a canonical model from the
  reference app specs; policy compiles to SQL / Go eval / widening analysis. Tests green.
- **L2 done** `compile` (KS-SPEC/SEC/DATA checks w/ diagnostics), `migrate` (DDL + RLS +
  §13.2 classification), `diff` (risk-ranked report, text/md/json), `checker` (catalog +
  AST analyzers + facts). Tests green.
- **L3 done** `runtime` on pgx: pipeline (§3.3), scoped reads/writes, sessions/API tokens,
  jobs, outbox, schedules w/ advisory lock, budgets, rate limits, audit, metrics, drain.
  `go test ./runtime` runs a real-Postgres integration suite (temp DB per test, non-owner
  role so RLS is actually tested) and skips cleanly without `KS_TEST_DB_URL`. Green.
- **L4 done** `codegen` (types, scoped views, contexts, adapters, entrypoint, SQL, OpenAPI,
  TS client, AGENTS.md, action stubs, `gen/rules_test.go`), `cmd/ks` (init gen check test
  diff dev context explain migrate plan|verify|apply|status client build), `kstest`
  (per-test DB cloned from a schema template, non-owner role, fake clock, recording
  effects, generated §16.2 rule tests, seeded fuzz). Gate met on `example/myapp`:
  `go build ./example/...`, `ks gen` byte-stable, `ks check --strict` clean, `ks test`
  green, `ks diff` non-empty vs. base. Run the suite with
  `KS_TEST_DB_URL=postgres://$USER@localhost:5432/postgres?sslmode=disable go test ./...`.

## Deviations from the spec text (all deliberate, all recorded)
1. Checker analyzers use stdlib `go/ast` + `go/parser`, not `x/tools/go/analysis` (§2.10).
2. `s.Email(name)` is the field builder, so the email effect is `s.EmailEffect()`; SMS is
   `s.SMEffect`→`s.SMSEffect`. Same reason: one package holds both (§5.2 vs §5.8).
3. `s.TTL(d)` returns one `Expiry` value usable in both `s.Sessions` and `s.Cache`.
4. App-level actors need a table, so `s.Account("User", ...)` exists (§4.2 "actor types"):
   a tenant-scoped resource whose id is the actor id.
5. Extra rule IDs beyond §14.3: KS-SPEC-005/006, KS-SEC-007/008/009, KS-DATA-004,
   KS-CODE-005, KS-MIG-003. They are in `checker/rules.go` and `ks explain`.
6. Role checks compile to an EXISTS over `ks_membership` (immediate revocation) rather
   than a role array on the session; `RunAs(System)` uses the `app.actor_system` GUC.
7. Diff category for schema rows is `migration` (matches the §15.3 sample output).

## Decisions to keep
- `policy` SQL text uses the `__KS_ROOT__.` marker + `?` binds; `Place` renumbers to `$n`.
- Framework tables are `ks_*` and created by the same generated migration stream.
- Actor GUCs: `ks.tenant_id`, `ks.actor_id`, `ks.actor_system`, `ks.cross_tenant`.
- `policy.TableSQL(node, table, target, alias)` + `SQL.WithRoot(alias)` produce fragments;
  `TargetPolicy` inlines literals and leaves actor values as binds (`policy.Place`).
- Runtime statement building goes through one `sqlBuilder` so every `?` becomes `$n` in
  the order the parameters appear — projection first, then WHERE, then RETURNING.
- Field-read rules are projected as `CASE WHEN … THEN col ELSE NULL END`; field-write
  rules are ANDed into the UPDATE's WHERE, so a refused write changes no row.
- `Buckets` caches rate-limit *denials* only. A cached allowance is wrong across
  instances, and refilling a cached bucket stretches the declared limit.
- **Projection order:** `codegen.resourceColumns` must equal `runtime.ColumnsOf` to the
  position (a generated scanner indexes by position); `codegen_test.go` and the runtime
  table tests are the drift check. The model spells framework fields in snake case, and a
  money field's real column pair is `f.Columns` (`amount_minor`, `amount_currency`).
- Org and User are app-level (`model.Tenant`) but carry their declared `Access`, are
  tenant-scoped (deviation 4), and a missing rule denies (`BuildTable`, `policy.Compare`).
- Model loading compiles the spec packages with every file importing `.../gen` stubbed
  out (`go run -overlay`): spec and actions share a package, so dropping whole packages
  silently loses modules.
- Actions may live in `app/<module>/actions.go` as an `Actions` struct (the spec's
  `var X = s.Mutation(...)` would collide with a same-named func); the checker treats
  `app/<module>/*.go` as action code and counts `kstest.Call(env, a, gen.XHandler, ...)`
  as a test of X. Schedules get a generated adapter but no entrypoint: they enqueue a job.
- `/_ks/*` introspection is on only with `KS_DEV=1` in the environment, never by config.
- `kstest` keeps one `ks_tmpl_<ddl hash>` database per schema on the test server (reused
  across runs; safe to drop) and connects as the `ks_test_app` role it creates.
- Migrations are `migrations/NNNN_name.{plan.json,sql,model.json}`; apply executes the
  plan file, checksums it into `ks_migrations` (edited ⇒ KS-MIG-002), and runs backfills
  between expand statements and constraints using each step's `backfill.set`.
- Framework (`ks_*`) tables the runtime writes outside a request use the permissive RLS
  policy; their own predicates decide visibility. `ks_membership`/`ks_audit` stay
  tenant-scoped, so tests read them through `harness.look` (a system+cross tx).

## Deliberate scope calls (recorded so it looks unfinished rather than forgotten)
In: spec DSL, model+canonical hash, policy->SQL/Go/compare, codegen (types/db/ctx/routes/main/
migrations/TS client/openapi/AGENTS.md), pipeline, scoped queries + RLS, invariants (CHECK +
deferred), optimistic concurrency, audit, outbox, jobs, schedules w/ advisory-lock leader,
sessions/API tokens/OIDC-shape, rate limits, budgets, checker (KS-* catalog), diff, migrate
plan/verify/apply/status, kstest harness, CLI.
Thin: OIDC = code+PKCE flow against any discovery doc, no vendor presets. `ks dev` = run+watch
+introspection endpoints, no MCP. Telemetry = slog JSON + Prometheus-text metrics + trace IDs
(no OTLP exporter). Property tests = one seeded fuzz (tenant isolation + no 500s). Backfills =
plan + resumable batch job. Generated access tests cover role-only rules and two invariant
shapes; other rules are skipped with the rule printed. `ks test` uses `KS_TEST_DB_URL` rather
than starting its own Postgres. Not built: `ks new`, `perf`, `bench`, `outbox|jobs` ops, `keys
rotate`, `docs --llm`, `upgrade` (§19 rows outside the v0.1 list above).
Out (v1 per spec): cross-row aggregate invariants, GraphQL, multi-DB, real-time, multi-region.
