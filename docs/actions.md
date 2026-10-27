# <img src="assets/logo/svg/ambit-mark.svg" alt="" width="28" height="28"> Writing actions

An action is a function with a signature fixed by its spec. Implementations live in the
module's `actions.go`, in an `Actions` table that the generated entrypoint wires up. If a
signature drifts from the spec, the build fails rather than a request.

```go
package billing

import (
	"myapp/gen"
	"github.com/abhilashiig/ambit/am"
	"github.com/abhilashiig/ambit/runtime"
)

var Actions = struct {
	SendInvoice  func(*gen.SendInvoiceCtx, gen.SendInvoiceInput) (*gen.Invoice, error)
	ListInvoices func(*gen.ListInvoicesCtx, gen.ListInvoicesInput) (*runtime.PageOf[gen.Invoice], error)
}{
	SendInvoice:  sendInvoice,
	ListInvoices: listInvoices,
}
```

`am gen` writes this file with `am.NotImplemented()` stubs the first time. After that the file
is yours: regeneration never touches it. `am new action` adds an entry for you.

## The context

`gen.<Action>Ctx` holds everything the action may reach:

| Field | What it is |
| --- | --- |
| `c` itself | A `context.Context`, so pass `c` wherever a context is expected |
| `c.Actor` | The caller: `ID`, `Tenant`, `Roles`, `Kind` (`user`, `service`, `system`) |
| `c.DB` | One view per resource the spec lists in `Reads`/`Writes`, e.g. `c.DB.Invoices` |
| `c.Email`, `c.HTTP`, ... | One handle per declared effect |
| `c.Clock` | The time. Use `c.Clock.Now()`, never `time.Now()` (AM-CODE-004), so tests control it |
| `c.Log` | A `slog.Logger` already tagged with the action and trace |

Action code may not import `net/http`, `database/sql`, `os` and similar packages (AM-SEC-001).
Everything reaches the world through the context. `am check` lists the rules, and
`am explain <RULE>` shows good and bad examples for each.

## Reading

Every read is limited to the caller's tenant and the resource's `Read` rule. You don't add
those filters; you can't remove them either.

```go
inv, err := c.DB.Invoices.Get(c, in.InvoiceID)        // not_found if missing *or* not readable

page, err := c.DB.Invoices.Q().
	Where(gen.Invoices.Status.Eq(gen.InvoiceStatusSent)).
	OrderBy(gen.Invoices.CreatedAt.Desc()).
	With(gen.WithInvoiceCustomer).                       // loads Customer for all rows in one query
	Page(c, in.Cursor, in.Limit)                          // keyset pagination, size capped by config

first, err := c.DB.Invoices.Q().Where(...).First(c)
n, err := c.DB.Invoices.Q().Where(...).Count(c)          // needs s.Counts() on the action
err = c.DB.Invoices.Q().Where(...).Each(c, 200, func(inv *gen.Invoice) error { ... }) // jobs
```

A query must end in `Get`, `First`, `Page`, `Count` or `Each`. There is no unbounded
"all rows" (AM-DATA-001). Relations load only through `.With(...)`, and a scoped call inside a
loop is flagged (AM-DATA-002).

Column descriptors live in `gen.<Plural>` (`gen.Invoices.Status`, `gen.Customers.Name`):

| Method | Available on |
| --- | --- |
| `.Eq`, `.Ne`, `.Gt`, `.Gte`, `.Lt`, `.Lte`, `.In`, `.NotIn`, `.IsNull`, `.IsNotNull` | most columns |
| `.Contains(s)`, `.HasPrefix(s)` | text columns |
| `.Asc()`, `.Desc()` | any column, for `OrderBy` |

## Writing

```go
// Insert: tenant_id comes from the actor, never from input; the Create rule is applied.
cust, err := c.DB.Customers.Insert(c, []runtime.Assignment{
	runtime.SetColumn("name", in.Name),
	runtime.SetColumn("email", in.Email),
	runtime.SetColumn("owner_id", in.Owner.Or(c.Actor.ID).Bytes()),
})

// Update: pass the version you read. A concurrent change returns a conflict (409).
updated, err := c.DB.Invoices.Update(c, inv.ID, inv.Version, gen.InvoicePatch{
	Status: am.Set(gen.InvoiceStatusSent),
	SentAt: am.Set(c.Clock.Now()),
}.Assignments())

err = c.DB.Invoices.Delete(c, inv.ID, inv.Version)   // only if the spec allows deletes
```

Every write records an audit row in the same transaction: who, which action, which fields
changed, and a redacted before/after. Invariants are checked before commit. A refused write
changes no row.

## Optional values

`am.Opt[T]` tells "absent" apart from "present but empty":

```go
if v, ok := in.Notes.Get(); ok { ... }
name := in.Nickname.Or("friend")
patch := gen.CustomerPatch{Name: am.Set("Ada")}    // fields left unset are not written
```

## Errors

Return the generated constructor for a declared error, or a framework error:

```go
return nil, gen.ErrSendInvoiceNotDraft()            // 422 {"code": "not_draft"}
return nil, am.Invalid("amount", "must be positive") // 400
return nil, am.NotFound()                            // 404
return nil, am.Forbidden("only the owner may")       // 403
```

Any other error becomes a 500 with the details kept out of the response. The client's types
list exactly the codes each action can return.

## Effects

```go
// Deferred (the default): recorded now, sent only if the transaction commits.
_, err := c.Email.Send(c, []byte(`{"to":["ada@example.com"],"data":{"number":"INV-1"}}`))

// Immediate: runs now and returns the provider's response. Its host must be declared.
res, err := c.HTTP.Send(c, []byte(`{"capture":"ch_1"}`), "api.psp.example")
```

The email payload is `{"to": [...], "subject": "...", "text": "..."}`, or `{"to": [...], "data":
{...}}` when the effect names a `Template`. Templates are files in `templates/<name>.txt`
whose first line is `Subject: ...`, rendered with Go's `text/template`. A storage payload is
`{"key": "...", "contentType": "...", "data": "<base64>"}`.

## Using another module's data

Declare it in `Reads` (`s.Reads(customers.Customer)`), and `c.DB` gets a view of that resource
under its own access rules. A module may read another module's resources but never write
them (`am check`, AM-SPEC-005).

`s.Calls(...)` is accepted and validated (it must point at a declared action, and call cycles
are refused), but **no call handle is generated yet**. An action cannot invoke another
module's action directly today.

## Raw SQL

Raw SQL is available only inside `am.Unsafe(c, "reason", func(tx am.RawTx) error { ... })`.
It still carries the tenant settings for row-level security, counts against the query
budget, and shows up in every behavior diff.

---

<sub>Ambit is built and maintained by [Grab Connected Tech](https://www.grabians.com/). Need a team to build or scale a product on Ambit? Write to [hello@grabians.com](mailto:hello@grabians.com).</sub>
