# <img src="assets/logo/svg/ambit-mark.svg" alt="" width="28" height="28"> CLI reference

Run `am` from the app's root directory: the one holding `app/`, `gen/` and `app.model.json`.
Every command exits non-zero on failure. Commands that produce findings or data print JSON
with `--json`: `check`, `gen`, `diff`, `explain`, `context`, `migrate`, `keys rotate`, `test`,
`outbox` and `jobs`.

## Building an app

| Command | What it does |
| --- | --- |
| `am init <name> [--module path] [--framework dir]` | Create an app: `go.mod`, `app/app.go`, a `tasks` module, `am.yaml`, a seed file and a CI workflow. Then run `gen` |
| `am new module <name>` | Add `app/<name>/spec.go` |
| `am new action <module>.<Action> [--kind query\|mutation\|job]` | Append a declaration nobody may call yet, and a `NotImplemented` stub in `actions.go` |
| `am gen` | Compile the spec (refusing an invalid one) and regenerate `gen/`, `cmd/server/`, `migrations/schema.gen.sql`, `migrations/migrations.go`, the OpenAPI document, the TS client, `AGENTS.md`, the `Dockerfile` and `app.model.json` |
| `am context <module> [--action m.A]` | The module's spec plus the generated signatures, without implementation code. Sized for a model's context |
| `am explain [<RULE>]` | Every checker rule, or one rule with why it exists and bad and good examples |
| `am docs --llm [--out file]` | The single-file framework reference for models (under 40k tokens) |

## Checking and testing

| Command | What it does |
| --- | --- |
| `am check [--strict] [--json]` | `go build`, `go vet`, model validation, the spec-vs-generated check (AM-CODE-001), the analyzers, and `govulncheck` if installed. Writes `app.facts.json`. `--strict` fails on warnings |
| `am test [--json]` | `go test ./...` against `AM_TEST_DB_URL`, or a disposable Postgres. Records query shapes for `perf` |
| `am diff --base <rev> [--format text\|markdown\|json]` | The behavior diff between the committed model at `<rev>` and the live spec and source. Refuses to run (AM-CODE-001) when `app.model.json` is stale; exits 1 on a blocked change (AM-API-001) |
| `am perf [--min-rows n]` | Explain the recorded query shapes against `AM_DATABASE_URL` and flag sequential scans on large tables (AM-PERF-002) |
| `am bench [<module.Action>] [--time 2s\|300x] [--max-p95 ms]` | In-process load test: p50/p95/p99, error rate, allocations per call |

## Running

| Command | What it does |
| --- | --- |
| `am dev [--addr :8080] [--mcp]` | Regenerate, migrate, build and run with `AM_DEV=1`, seed from `seed/*.sql`, and restart on every source change. `--mcp` serves the dev API as MCP tools on stdio |
| `am build [--out bin/server]` | A static, trimmed production binary |
| `am client ts [--out dir]` | Write the TypeScript client somewhere else, e.g. into a frontend repository |

The development API, served only with `AM_DEV=1`:

| Endpoint | Returns |
| --- | --- |
| `GET /_am/actions` | Every action with its kind, input fields, required roles and errors |
| `POST /_am/call` | `{"action","actor","input"}` runs an action as that actor; returns the output (or error) and a trace id |
| `GET /_am/traces/<id>` | The queries (with timings), effects and error of one call |
| `GET /_am/logs?since=<n>` | Log lines after a cursor, and the next cursor |
| `POST /_am/reset` | Empty the database and reload the seed files |

## Database and operations

| Command | What it does |
| --- | --- |
| `am migrate plan [--name n]` | Write the next numbered migration ([Migrations](migrations.md)) |
| `am migrate verify` | Apply every migration to a fresh database and compare it with the model's schema |
| `am migrate apply [--plan] [--allow-destructive]` | Apply pending migrations to `AM_DATABASE_URL` |
| `am migrate status` | Each migration: applied, pending or modified |
| `am keys new` | Print a key for `AM_ENCRYPTION_KEY` |
| `am keys rotate` | Re-seal every encrypted value under the current key |
| `am outbox list [--state s] [--limit n]`, `inspect <id>`, `replay <id>` | Pending and dead-lettered effects |
| `am jobs list`, `inspect <id>`, `replay <id>` | Jobs |
| `am upgrade [version]` | Update the framework (default `latest`), regenerate, and plan a framework-upgrade migration |

The app binary itself answers `myapp migrate apply [--plan] [--allow-destructive]` and
`myapp migrate status`, from the migrations embedded in it.

---

<sub>Ambit is built and maintained by [Grab Connected Tech](https://www.grabians.com/). Need a team to build or scale a product on Ambit? Write to [hello@grabians.com](mailto:hello@grabians.com).</sub>
