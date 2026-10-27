# <img src="assets/logo/svg/ambit-mark.svg" alt="" width="28" height="28"> Getting started

This guide takes a new app from `am init` to a running server, and walks through the loop
you (or an agent) repeat for every change.

## Install

Ambit needs Go 1.25+ and Postgres 15+. Build the CLI from a checkout of this repository:

```sh
go install ./cmd/am
am explain            # lists the checker rules; a quick way to confirm it works
```

## Create an app

```sh
am init myapp --framework /path/to/ambit
cd myapp
```

`--framework` adds a `replace` directive so the app builds against your checkout. Leave it
out once Ambit is published as a module.

## Project layout

```
myapp/
  app/
    app.go              # app-level spec: the tenant, the account, roles, identity
    tasks/
      spec.go           # the module's resources and actions (people review this)
      actions.go        # the action implementations (agents write this)
      actions_test.go   # tests you write
  gen/                  # generated Go: types, scoped views, adapters, rule tests (never edit)
  cmd/server/main.go    # generated entrypoint (never edit)
  migrations/           # schema.gen.sql, and numbered migration plans
  client/ts/index.ts    # generated TypeScript client
  app.model.json        # the compiled model; am diff compares it across revisions
  app.facts.json        # code facts from am check (dependencies, unsafe blocks)
  AGENTS.md             # generated instructions for coding agents
  am.yaml               # configuration
  seed/                 # SQL that am dev loads into the dev database
```

Files under `gen/` and `cmd/server/` carry a `Code generated ... DO NOT EDIT` header, and
`am check` flags edits to them. Change the spec and run `am gen` instead.

## The change loop

Every feature follows the same seven steps. `AGENTS.md` describes them to agents too.

1. **Read the context.** `am context tasks` prints the module's spec and the generated
   signatures its actions are written against. Add `--action tasks.CreateTask` for one action.
2. **Change the spec.** Edit `app/tasks/spec.go`. `am new action tasks.Archive` scaffolds a
   declaration and its stub. New actions start as `Requires(s.Never())`, so nobody can call
   them until you say who may.
3. **Generate.** `am gen` regenerates `gen/`, the client, the schema and `AGENTS.md`. It writes
   `actions.go` with stubs if the module has none yet.
4. **Implement** the action bodies in `actions.go`. See [Writing actions](actions.md).
5. **Check.** `am check --json` until it reports no errors. `--strict` also fails on warnings.
6. **Test.** `am test --json` until generated and written tests pass. See [Testing](testing.md).
7. **Review.** `am diff --base main` prints what the app can now do that it could not before.

## Run it

```sh
export AM_DATABASE_URL=postgres://localhost:5432/myapp?sslmode=disable
am migrate plan        # writes migrations/0001_initial.*
am migrate apply
am dev                 # serves on :8080, rebuilds and restarts on every change
```

`am dev` loads `seed/*.sql` into the database and serves a development-only API:

```sh
curl localhost:8080/_am/actions                      # every action, its input and roles
curl -X POST localhost:8080/_am/call -d '{
  "action": "tasks.ListTasks",
  "actor":  {"ID": "<user id>", "Tenant": "<org id>", "Roles": ["member"]},
  "input":  {}
}'                                                    # returns output and a traceId
curl localhost:8080/_am/traces/<traceId>              # its queries, timings and effects
```

`am dev --mcp` exposes the same API as Model Context Protocol tools, so an agent can call
them directly.

## Calling the API

Every query and mutation is `POST /api/<module>/<Action>` with a JSON body. Queries declared
with `s.Get()` also answer `GET` with query parameters. Mutations declared `Idempotent()`
need an `Idempotency-Key` header. Errors always have this shape:

```json
{"error": {"code": "not_draft", "message": "...", "fields": {"amount": "..."}, "traceId": "..."}}
```

The generated TypeScript client wraps this:

```ts
import { createClient } from "./client/ts"

const api = createClient({ baseUrl: "/api" })
const res = await api.billing.sendInvoice({ invoice_id: id })
if (!res.ok && res.error.code === "not_draft") {
  // the compiler knows every code this action can return
}
```

Callers authenticate with a session cookie or an `Authorization: Bearer <token>` API token.
See [Operations](operations.md#authentication) for how those are issued.

---

<sub>Ambit is built and maintained by [Grab Connected Tech](https://www.grabians.com/). Need a team to build or scale a product on Ambit? Write to [hello@grabians.com](mailto:hello@grabians.com).</sub>
