package runtime_test

import (
	"context"
	"testing"

	"github.com/Grab-Connected-Tech/ambit/am"
	"github.com/Grab-Connected-Tech/ambit/runtime"
)

// TestAggregatesReadTheFilteredView: sums and groups see the tenant, the Read rule and
// field-read rules exactly as Page does, never mix currencies, and stay bounded.
func TestAggregatesReadTheFilteredView(t *testing.T) {
	h := setup(t)
	acme := h.newCustomer(h.member, "AcmeCo")
	beta := h.newCustomer(h.member, "BetaCo")
	h.newInvoice(h.admin, acme, 1000, "draft")
	h.newInvoice(h.admin, acme, 2500, "draft")
	h.newInvoice(h.admin, beta, 4000, "sent")
	globex := h.newCustomer(h.other, "GlobexCo")
	h.newInvoice(h.other, globex, 99999, "draft") // another tenant: never counted
	hidden := h.newInvoice(h.admin, beta, 500, "sent")
	if err := h.call(h.admin, func(ctx context.Context, d *runtime.DB) error {
		_, err := runtime.Scoped(d, invTable).Update(ctx, hidden.ID, hidden.Version, []runtime.Assignment{
			{Field: "notes", Column: "notes", Args: []any{"secret"}},
		})
		return err
	}); err != nil {
		t.Fatal(detail(err))
	}

	amount := runtime.NewMoneyCol("t.amount_minor", "t.amount_currency")
	type invStatus string // an enum key scans into its generated named type
	status := runtime.NewCol[invStatus]("t.status")
	customer := runtime.NewIDCol("t.customer_id", "customers.Customer")
	notes := runtime.NewOptCol[string]("t.notes")

	err := h.call(h.member, func(ctx context.Context, d *runtime.DB) error {
		v := runtime.Scoped(d, invTable)
		q := v.Q()
		usd, err := q.SumMoney(ctx, amount, "usd")
		if err != nil {
			return err
		}
		if usd != am.MoneyOf(8000, "USD") {
			t.Errorf("USD total: %v", usd)
		}
		if eur, err := q.SumMoney(ctx, amount, "EUR"); err != nil || eur != am.MoneyOf(0, "EUR") {
			t.Errorf("no EUR rows sum to zero EUR: %v %v", eur, err)
		}
		// The currency filter of a sum must not stick to the caller's query.
		if n, err := q.Count(ctx); err != nil || n != 4 {
			t.Errorf("count after SumMoney: %d %v", n, err)
		}
		if n, err := v.Q().Where(status.Eq("sent")).Sum(ctx, amount.Minor); err != nil || n != 4500 {
			t.Errorf("sum of sent: %d %v", n, err)
		}

		byStatus, err := runtime.GroupBy(v.Q(), status).SumMoney(ctx, amount, "USD")
		if err != nil {
			return err
		}
		want := []runtime.Group[invStatus, am.Money]{
			{Key: "draft", Count: 2, Sum: am.MoneyOf(3500, "USD")},
			{Key: "sent", Count: 2, Sum: am.MoneyOf(4500, "USD")},
		}
		if len(byStatus) != len(want) || byStatus[0] != want[0] || byStatus[1] != want[1] {
			t.Errorf("by status: %+v", byStatus)
		}

		byCustomer, err := runtime.GroupBy(v.Q(), customer).Count(ctx)
		if err != nil {
			return err
		}
		counts := map[am.ID]int64{}
		for _, g := range byCustomer {
			counts[g.Key] = g.Count
		}
		if len(counts) != 2 || counts[acme.ID] != 2 || counts[beta.ID] != 2 {
			t.Errorf("by customer: %+v", byCustomer)
		}

		// A member may not read notes, so every row falls into the NULL group.
		byNotes, err := runtime.GroupBy(v.Q(), notes.Col).Count(ctx)
		if err != nil {
			return err
		}
		if len(byNotes) != 1 || byNotes[0].Key != nil || byNotes[0].Count != 4 {
			t.Errorf("a hidden key must not leak: %+v", byNotes)
		}
		return nil
	})
	if err != nil {
		t.Fatal(detail(err))
	}

	// An admin may read notes, so the same group shows the value.
	if err := h.call(h.admin, func(ctx context.Context, d *runtime.DB) error {
		byNotes, err := runtime.GroupBy(runtime.Scoped(d, invTable).Q(), notes.Col).Count(ctx)
		if err != nil {
			return err
		}
		if len(byNotes) != 2 || byNotes[0].Key == nil || *byNotes[0].Key != "secret" {
			t.Errorf("admin groups by notes: %+v", byNotes)
		}
		return nil
	}); err != nil {
		t.Fatal(detail(err))
	}

	// More groups than the page cap (50 in the fixture) is an error, not a short list.
	for i := 0; i < 47; i++ {
		h.newInvoice(h.admin, acme, 100, "draft")
	}
	err = h.call(h.member, func(ctx context.Context, d *runtime.DB) error {
		_, err := runtime.GroupBy(runtime.Scoped(d, invTable).Q(), runtime.NewIDCol("t.id", "")).Count(ctx)
		return err
	})
	if code(err) != "too_many_groups" {
		t.Fatalf("51 groups must be refused: %v", detail(err))
	}
}
