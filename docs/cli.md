# CLI reference

Run `ks` from the app's root directory: the one holding `app/`, `gen/` and `app.model.json`.
Every command exits non-zero on failure. Commands that produce findings or data print JSON
with `--json`: `check`, `gen`, `diff`, `explain`, `context`, `migrate`, `keys rotate`, `test`,
`outbox` and `jobs`.

## Building an app

| Command | What it does |
| --- | --- |
| `ks init <name> [--module path] [--framework dir]` | Create an app: `go.mod`, `app/app.go`, a `tasks` module, `ks.yaml`, a seed file and a CI workflow. Then run `gen` |
| `ks new module <name>` | Add `app/<name>/spec.go` |
| `ks new action <module>.<Action> [--kind query\|mutation\|job]` | Append a declaration nobody may call yet, and a `NotImplemented` stub in `actions.go` |
| `ks gen` | Compile the spec (refusing an invalid one) and regenerate `gen/`, `cmd/server/`, `migrations/schema.gen.sql`, `migrations/migrations.go`, the OpenAPI document, the TS client, `AGENTS.md`, the `Dockerfile` and `app.model.json` |
| `ks context <module> [--action m.A]` | The module's spec plus the generated signatures, without implementation code. Sized for a model's context |
| `ks explain [<RULE>]` | Every checker rule, or one rule with why it exists and bad and good examples |
| `ks docs --llm [--out file]` | The single-file framework reference for models (under 40k tokens) |

## Checking and testing

| Command | What it does |
| --- | --- |
| `ks check [--strict] [--json]` | `go build`, `go vet`, model validation, the spec-vs-generated check (KS-CODE-001), the analyzers, and `govulncheck` if installed. Writes `app.facts.json`. `--strict` fails on warnings |
| `ks test [--json]` | `go test ./...` against `KS_TEST_DB_URL`, or a disposable Postgres. Records query shapes for `perf` |
| `ks diff --base <rev> [--format text\|markdown\|json]` | The behavior diff between the committed model at `<rev>` and now. Exits 1 on a blocked change (KS-API-001) |
| `ks perf [--min-rows n]` | Explain the recorded query shapes against `KS_DATABASE_URL` and flag sequential scans on large tables (KS-PERF-002) |
| `ks bench [<module.Action>] [--time 2s\|300x] [--max-p95 ms]` | In-process load test: p50/p95/p99, error rate, allocations per call |

## Running

| Command | What it does |
| --- | --- |
| `ks dev [--addr :8080] [--mcp]` | Regenerate, migrate, build and run with `KS_DEV=1`, seed from `seed/*.sql`, and restart on every source change. `--mcp` serves the dev API as MCP tools on stdio |
| `ks build [--out bin/server]` | A static, trimmed production binary |
| `ks client ts [--out dir]` | Write the TypeScript client somewhere else, e.g. into a frontend repository |

The development API, served only with `KS_DEV=1`:

| Endpoint | Returns |
| --- | --- |
| `GET /_ks/actions` | Every action with its kind, input fields, required roles and errors |
| `POST /_ks/call` | `{"action","actor","input"}` runs an action as that actor; returns the output (or error) and a trace id |
| `GET /_ks/traces/<id>` | The queries (with timings), effects and error of one call |
| `GET /_ks/logs?since=<n>` | Log lines after a cursor, and the next cursor |
| `POST /_ks/reset` | Empty the database and reload the seed files |

## Database and operations

| Command | What it does |
| --- | --- |
| `ks migrate plan [--name n]` | Write the next numbered migration ([Migrations](migrations.md)) |
| `ks migrate verify` | Apply every migration to a fresh database and compare it with the model's schema |
| `ks migrate apply [--plan] [--allow-destructive]` | Apply pending migrations to `KS_DATABASE_URL` |
| `ks migrate status` | Each migration: applied, pending or modified |
| `ks keys new` | Print a key for `KS_ENCRYPTION_KEY` |
| `ks keys rotate` | Re-seal every encrypted value under the current key |
| `ks outbox list [--state s] [--limit n]`, `inspect <id>`, `replay <id>` | Pending and dead-lettered effects |
| `ks jobs list`, `inspect <id>`, `replay <id>` | Jobs |
| `ks upgrade [version]` | Update the framework (default `latest`), regenerate, and plan a framework-upgrade migration |

The app binary itself answers `myapp migrate apply [--plan] [--allow-destructive]` and
`myapp migrate status`, from the migrations embedded in it.
