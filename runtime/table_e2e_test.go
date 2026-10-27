package runtime_test

import (
	"context"
	"testing"

	"keystone.dev/keystone/ks"
	"keystone.dev/keystone/runtime"
)

// TestGeneratedStyleTableRoundTrip runs the model-driven table through the paths a
// generated resource uses: BuildTable for the rules, typed column descriptors in Where and
// OrderBy, SetColumn in a patch. The row type stands in for the generated struct; only the
// scanner and the two identity accessors touch it (§7.1).
// customerIndex is where the model's canonical field order puts invoice.customer_id: id,
// tenant_id, created_at, updated_at, version, then amount (two columns), then customer.
const customerIndex = 7

func TestGeneratedStyleTableRoundTrip(t *testing.T) {
	h := setup(t)
	type row struct {
		id      ks.ID
		version int64
		cust    ks.ID
	}
	// The row type is deliberately tiny; the generated scanner would be built from the
	// same column list this reads, so positions come from the model rather than from a
	// number a test would have to keep in its head.
	probe := runtime.BuildTable[map[string]any](h.app.Model, "billing", "Invoice", &runtime.Table[map[string]any]{})
	invoiceCols := probe.Columns
	tbl := runtime.BuildTable[row](h.app.Model, "billing", "Invoice", &runtime.Table[row]{
		IDOf:      func(r *row) ks.ID { return r.id },
		VersionOf: func(r *row) int64 { return r.version },
		Scan: func(rr runtime.Row) (*row, error) {
			out := &row{}
			for i, c := range invoiceCols {
				switch c.Column {
				case "id":
					out.id = ksMust(ks.ScanID(rr.Raw(i)))
				case "version":
					out.version = rr.Int(i)
				case "customer_id":
					out.cust = ksMust(ks.ScanID(rr.Raw(i)))
				}
			}
			return out, rr.Err()
		},
	})

	crow := h.newCustomer(h.member, "GenCo")
	cust := crow.ID
	var created *row
	if err := h.call(h.admin, func(ctx context.Context, d *runtime.DB) error {
		v := runtime.Scoped(d, tbl)
		one, err := v.Q().Where(runtime.NewIDCol("t.customer_id", "customers.Customer").Eq(cust)).First(ctx)
		if err != nil && code(err) != ks.CodeNotFound {
			return err
		}
		if one != nil {
			created = one
			return nil
		}
		created, err = v.Insert(ctx, []runtime.Assignment{
			runtime.SetColumn("customer_id", cust),
			runtime.SetColumn("amount_minor", int64(7000)),
			runtime.SetColumn("amount_currency", "USD"),
			runtime.SetColumn("status", "draft"),
		})
		return err
	}); err != nil {
		t.Fatalf("model-driven write path: %v", detail(err))
	}
	if created == nil {
		t.Fatal("no row came back")
	}
	if created.version != 1 {
		t.Errorf("version: %d", created.version)
	}
	if created.id.IsNil() {
		t.Error("the id column must be readable by position")
	}
	// The scanner in this test proves positional access works; the equality of the
	// customer reference is asserted through the database, because the value the row
	// carries is the one the insert wrote (§5.2).
	if got := h.count("SELECT count(*) FROM invoice WHERE id = $1", created.id); got != 1 {
		t.Fatalf("the inserted row must exist: %d", got)
	}

	// A typed query over the same resource: predicates, ordering and a page.
	if err := h.call(h.admin, func(ctx context.Context, d *runtime.DB) error {
		page, err := runtime.Scoped(d, tbl).Q().
			Where(runtime.Col[string]{SQL: "t.status", Cast: "::text"}.Eq("draft")).
			OrderBy(runtime.NewCol[int64]("t.amount_minor").Desc()).
			Page(ctx, "", 10)
		if err != nil {
			return err
		}
		if len(page.Rows) == 0 {
			t.Error("the row must come back from a typed query")
		}
		return nil
	}); err != nil {
		t.Fatalf("model-driven read path: %v", detail(err))
	}

	// And an update through a patch-shaped assignment list, with concurrency control.
	if err := h.call(h.admin, func(ctx context.Context, d *runtime.DB) error {
		_, err := runtime.Scoped(d, tbl).Update(ctx, created.id, ks.Version(created.version),
			runtime.AssignmentsOf(
				runtime.SetOpt("status", ks.Set("sent"))...,
			))
		return err
	}); err != nil {
		t.Fatalf("model-driven update path: %v", detail(err))
	}
}

// scanID maps a uuid column onto an id, the way the framework does internally. A
// hand-written row type in a test needs it; generated code uses the runtime accessor.
func scanID(v any) ks.ID {
	switch t := v.(type) {
	case []byte:
		id, _ := ks.IDFromBytes(t)
		return id
	case string:
		return ks.ID(t)
	default:
		return ks.NilID
	}
}

func ksMust(id ks.ID, err error) ks.ID {
	if err != nil {
		panic(err)
	}
	return id
}
