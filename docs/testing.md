# Testing

`ks test` runs two kinds of tests against a real Postgres: those Keystone generates from the
spec, and those you write with `kstest`. Effects are captured instead of sent.

```sh
ks test            # uses KS_TEST_DB_URL, or starts a throwaway Postgres for the run
ks test --json     # go test -json output, for agents and CI
```

With no `KS_TEST_DB_URL`, `ks test` initializes a temporary cluster from local Postgres server
binaries (falling back to a Docker container) and deletes it afterwards.

## How a test runs

Each test gets its own database, cloned from a template that already holds the schema (built
once per schema and reused across runs), so tests run in parallel without interfering. The
app connects as a role that does *not* own the tables, so row-level security is genuinely in
force. Each test also gets:

- a fake clock starting at `kstest.Epoch` (2030-01-01 09:00 UTC);
- recording providers for every effect. An HTTP effect fails unless you stub its host;
- a fresh encryption key, so `Encrypted()` fields work.

## Writing a test

Put tests beside the actions, in the module package (`app/billing/actions_test.go`):

```go
func TestSendInvoice(t *testing.T) {
	env := kstest.New(t, gen.Model)
	org := env.Tenant()
	acct := env.Actor(org, app.Accountant, app.Member)
	cust := kstest.Create(env, gen.CustomerTable, gen.Customer{
		TenantID: org, Name: "Ada", Email: "ada@example.test", OwnerID: acct.ID})
	inv := kstest.Create(env, gen.InvoiceTable, gen.Invoice{
		TenantID: org, CustomerID: cust.ID, Amount: ks.MoneyOf(5000, "USD"), Status: gen.InvoiceStatusDraft})

	out, err := kstest.Call(env, acct, gen.SendInvoiceHandler, Actions.SendInvoice,
		gen.SendInvoiceInput{InvoiceID: inv.ID})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != gen.InvoiceStatusSent {
		t.Fatalf("status %s", out.Status)
	}
	if len(env.Emails()) != 1 {
		t.Fatal("one email is queued with the commit")
	}
	env.AssertQueriesAtMost(4)
}
```

| Helper | What it does |
| --- | --- |
| `kstest.New(t, gen.Model)` | A fresh app and database. Skips the test when `KS_TEST_DB_URL` is unset (`ks test` sets it) |
| `env.Tenant()` | A new tenant row. Returns its id |
| `env.Actor(tenant, roles...)` | A new account in the tenant, holding those roles (spec roles or strings) |
| `kstest.Create(env, gen.XTable, gen.X{...})` | Inserts a fixture row as-is, bypassing access rules |
| `env.Fixture("billing.Invoice", tenant)` | A row with placeholder values, creating whatever it references |
| `kstest.Call(env, actor, gen.XHandler, Actions.X, input)` | Runs the action through the full pipeline: auth, validation, transaction, rules, invariants |
| `env.Emails()`, `env.Effects()` | Effects that committed (a rolled-back action's are not there) |
| `env.StubHTTP(host, body)` | The response an HTTP effect to that host receives |
| `env.Clock.Advance(d)` | Moves time forward |
| `env.AssertQueriesAtMost(n)` | Fails if the last action ran more statements |
| `env.As(actor, fn)` | A scoped `runtime.DB` as that actor, for checking the data layer directly |
| `env.QueryRow(sql, args, dest...)` | Reads the database as its owner, to check what is actually stored |

Errors come back as `*ks.Error`; check the code with `ks.AsError(err).Code`. Budgets are
enforced in tests, so going over an action's query budget fails it (KS-PERF-001).

`ks check` warns about any action that no test calls (KS-TEST-001). A test counts when it
mentions the action's adapter, `gen.<Action>Handler`.

## Generated tests

`ks gen` writes `gen/rules_test.go`, which proves for every resource and action:

- **Tenant isolation.** An actor in another tenant, holding every role, cannot read or delete
  the row, and an unfiltered `SELECT` under its scope sees nothing.
- **The access matrix.** For `Read`/`Create`/`Update`/`Delete` rules over roles, an actor with
  an allowed role succeeds and one without is refused.
- **Field rules.** Only readers holding the role see a `FieldRead`-protected field.
- **Invariants.** A row breaking a `CHECK` invariant is rejected by the database.
- **Authorization.** A caller without the required roles gets `forbidden`.
- **Input validation.** Over-long text and malformed emails, ids and enum values are rejected
  as `invalid`.

Rules that depend on row fields or the actor's id are skipped, with the rule printed in the
test log, so you can see what the generated tests do not cover.

## Property tests

`kstest.Fuzz` runs an action many times with random valid inputs, as random actors in two
tenants, drawing ids from both tenants' rows. It fails if the action ever returns an internal
error, or returns a row from a tenant other than the caller's.

```go
func TestFuzzBilling(t *testing.T) {
	env := kstest.New(t, gen.Model)
	env.StubHTTP("api.psp.example", []byte(`{"captured":true}`))
	kstest.Fuzz(env, gen.MarkPaidHandler, Actions.MarkPaid, 40)
}
```

A failure prints its seed. Rerun it with `KS_FUZZ_SEED=<seed> ks test`.

## Performance

`ks test` records every query shape the actions ran in `.ks/queries.jsonl`. Then:

```sh
ks perf --min-rows 1000      # EXPLAIN them against KS_DATABASE_URL (a seeded database)
ks bench billing.ListInvoices --max-p95 5
```

`ks perf` flags sequential scans on large tables (KS-PERF-002) and names the columns to
index. `ks bench` runs an action in process through the full pipeline and reports
p50/p95/p99 latency, the error rate, and allocations per call. `--max-p95` fails the run
over a budget, for CI.
