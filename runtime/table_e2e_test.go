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
func TestGeneratedStyleTableRoundTrip(t *testing.T) {
	h := setup(t)
	type row struct {
		id      ks.ID
		version int64
		cust    ks.ID
	}
	tbl := runtime.BuildTable[row](h.app.Model, "billing", "Invoice", &runtime.Table[row]{
		IDOf:      func(r *row) ks.ID { return r.id },
		VersionOf: func(r *row) int64 { return r.version },
		Scan: func(rw runtime.Row) (*row, error) {
			out := &row{}
			out.version = rw.Int(4)
			out.id = rw.ID(0)
			out.cust = rw.ID(5)
			// The remaining positions are read to prove the generated shape of a scan
			// works for nullable and money columns too.
			_ = rw.Str(7)
			_ = rw.Str(8)
			if !rw.Null(9) {
				_ = rw.Time(9)
			}
			if rw.Err() != nil {
				return nil, rw.Err()
			}
			return out, nil
		},
	})

	crow := h.newCustomer(h.member, "GenCo")
	cust := crow.ID
	var created *row
	if err := h.call(h.admin, func(ctx context.Context, d *runtime.DB) error {
		v := runtime.Scoped(d, tbl)
		one, err := v.Q().Where(runtime.NewIDCol("t.customer_id", "customers.Customer").Eq(cust)).First(ctx)
		if err != nil {
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
		t.Fatalf("model-driven write path: %v", err)
	}
	if created == nil || created.cust != cust || created.version != 1 {
		t.Fatalf("row: %+v", created)
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
		t.Fatalf("model-driven read path: %v", err)
	}

	// And an update through a patch-shaped assignment list, with concurrency control.
	if err := h.call(h.admin, func(ctx context.Context, d *runtime.DB) error {
		_, err := runtime.Scoped(d, tbl).Update(ctx, created.id, ks.Version(created.version),
			runtime.AssignmentsOf(
				runtime.SetOpt("status", ks.Set("sent"))...,
			))
		return err
	}); err != nil {
		t.Fatalf("model-driven update path: %v", err)
	}
}
