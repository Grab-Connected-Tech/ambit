# HANDOVER — Keystone framework build, continuing after codegen landed

Repo: `/Users/abhilashreddy/Projects/keystone` · module `keystone.dev/keystone`
Read `PLAN.md` first: it holds the context strategy, environment facts, core contracts,
layer gates and the list of deliberate spec deviations. This file only adds what is true
*right now*.

## Where things stand

| Layer | State |
| --- | --- |
| `ks`, `spec`, `model`, `policy` | done, tested |
| `compile`, `migrate`, `diff`, `checker` | done, tested |
| `runtime` | done, integration-tested against Postgres |
| `codegen` | done; `codegen_test.go` regenerates the example, asserts byte-stability, builds it and applies `schema.gen.sql` to a fresh DB |
| `cmd/ks` CLI | **not started** (`cmd/ks` is an empty dir) |
| `kstest` harness | not started (empty dir) |

`go build ./...`, `go vet ./...` and `go test ./...` (with `KS_TEST_DB_URL` set) are all green.

```sh
export KS_TEST_DB_URL="postgres://abhilashreddy@localhost:5432/postgres?sslmode=disable"
```

## Open questions found along the way (not fixed, need a decision)

- The tenant table (`org`) gets `ks_public` RLS (`USING (true)`), so the database backstop
  does not isolate orgs; only the query layer's tenant filter does. Tightening it means
  org creation (signup) must run cross-tenant or as system. `migrate.tenantTableDDL`.
- `compile/compile_test.go` is not gofmt-clean (pre-existing).

## codegen: what it does and how to run it

```sh
cat > /tmp/ksmain.go <<'EOF'
package main

import (
	"fmt"
	"os"

	"keystone.dev/keystone/codegen"
)

func main() {
	res, err := codegen.Generate(codegen.Options{
		Dir:        "example/myapp",
		AppPackage: "keystone.dev/keystone/example/myapp/app",
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "gen:", err)
		os.Exit(1)
	}
	for _, f := range res.Files {
		fmt.Println(f)
	}
}
EOF
go run /tmp/ksmain.go            # writes example/myapp/{gen,migrations,client,AGENTS.md,app.model.json,cmd/server/main.go}
KS_GEN_DEBUG=/tmp/genfail go run /tmp/ksmain.go   # dumps the unformatted buffer of a file that will not parse
go build ./example/...
```

Outputs: `app.model.json`, `gen/app.gen.go` (embedded model + Config/New/Serve + helpers
`mustAction`, `validEmail`, `matchPattern`, `oneOf`, `dedupe`, `ksMustID`),
`gen/<module>.resources.gen.go` (struct, enums, `XPatch` with `Assignments()`, column
descriptor set `var Invoices = struct{...}{...}`, `XTable = runtime.BuildTable[...]`,
`XView`, `ScopedX`, `WithXRelation` loaders), `gen/<module>.actions.gen.go` (`XCtx`,
`XDB`, `XInput` (+`Cursor`/`Limit` for queries), `XOut`, `Err<Action><Code>()`,
`XHandler(fn)`), `migrations/schema.gen.sql`, `gen/openapi.json`, `client/ts/index.ts`,
`AGENTS.md` (preserves the `<!-- ks:team-notes -->` block).

Hard rule to preserve: **the projection order in `codegen.resourceColumns` must equal
`runtime.ColumnsOf`**, because a generated scanner indexes by position. `runtime`'s
`buildTables` and its two table tests are the drift check.

Facts learned the slow way (do not re-derive):
- `model.App.Bytes()` returns **one** value. `FindResource` takes `module.Name`; a
  resource's own `Name` is the bare name, so build refs, not names.
- `model.Tenant{Name, Table, Fields}` is *not* a `model.Resource`; app-level (Org/User)
  resources must be projected, with the framework fields stripped (they are re-added).
  Both Org and User are tenant-scoped (PLAN.md deviation 4) in model, codegen and DDL.
- The model spells framework fields in **snake case** (`created_at`), so
  `frameworkField`/`seeded` must compare `f.Column` too — otherwise generated structs get
  duplicate fields.
- Money: `spec/build.go` stores the model column as `amount_minor`; `policy` derives its
  pair from the *base* name, so `m.Columns` carries the real pair. Use `f.Columns` when
  present or you get `amount_minor_minor`.
- Loading the model compiles the app's spec packages with every file that imports
  `.../gen` replaced by an empty stub (`go run -overlay`, `stubGenFiles`): spec and actions
  share a package, so excluding whole packages silently drops modules from the model.
- Schedules: `gen` currently emits a handler per action *including* schedules, but
  `cmd/server/main.go` skips schedule entries. `example/.../billing/actions.go` therefore
  does not declare `DailyReminders`. Decide: alias schedule → job in `gen`, or skip in
  both places.
- `example/myapp/app/billing/spec.go` declares actions as `var SendInvoice = s.Mutation(...)`,
  so implementations must live in an **`Actions` struct** in `actions.go` (a same-named
  package-level func collides). That collision broke model loading for a while.

## Not yet done, in priority order

1. `cmd/ks` CLI: `gen`, `check` (build+vet+`checker.Run`+model-hash compare, KS-CODE-001),
   `migrate plan|apply|status` (use `migrate.DDL`/plan + `KS_DATABASE_URL`), `diff --base`,
   `explain <RULE-ID>` (`checker.Explain`), `test`. It should shell out to `codegen` the
   way `/tmp/ksmain.go` does — reuse that as the entrypoint body.
2. `kstest`: ephemeral DB + `env.Create`/`env.Call` per §14 of the spec (sample in the
   spec doc around line 874).
3. Generated policy/invariant tests (§14 "generated tests"), `ks dev` introspection routes,
   govulncheck in `ks check`.

## Commit conventions here

- Author must stay `Abhilash <abhi@grabians.com>` (repo-local git config; already set).
- Do **not** add `Co-Authored-By` trailers. Nobody asked for one, and this session is not
  running the vendor such a line would name.
- `git commit -F <file>` rather than `-m "$(cat <<EOF…)"`: the local bash is 3.2 and
  mishandles apostrophes inside command-substituted heredocs.
