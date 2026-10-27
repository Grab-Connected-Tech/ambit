// This file holds the module's action implementations. The spec in spec.go says what may
// happen; here is how. Everything an action can reach — which rows, which effect, which
// actor — arrives through the generated context, so a capability the spec did not grant is
// not expressible in this file (§7.2).
package billing

import (
	"keystone.dev/keystone/example/myapp/gen"
	"keystone.dev/keystone/ks"
	"keystone.dev/keystone/runtime"
)

// Actions is the implementation table the generated entrypoint names. Writing it as a
// struct of functions means a signature drift is a compile error rather than a runtime one.
var Actions = struct {
	SendInvoice   func(*gen.SendInvoiceCtx, gen.SendInvoiceInput) (*gen.Invoice, error)
	MarkPaid      func(*gen.MarkPaidCtx, gen.MarkPaidInput) (*gen.Invoice, error)
	ListInvoices  func(*gen.ListInvoicesCtx, gen.ListInvoicesInput) (*runtime.PageOf[gen.Invoice], error)
	CountDrafts   func(*gen.CountDraftsCtx, gen.CountDraftsInput) (gen.CountDraftsOut, error)
	SendReminders func(*gen.SendRemindersCtx, gen.SendRemindersInput) (struct{}, error)
	RefundCharge  func(*gen.RefundChargeCtx, gen.RefundChargeInput) (struct{}, error)
}{
	SendInvoice:   sendInvoice,
	MarkPaid:      markPaid,
	ListInvoices:  listInvoices,
	CountDrafts:   countDrafts,
	SendReminders: sendReminders,
	RefundCharge:  refundCharge,
}

// sendInvoice moves a draft to sent and queues the email. The status test happens before
// the write, and the write is itself guarded by the spec's Update rule, so a caller who may
// not change status never reaches this code at all (§5.6).
func sendInvoice(c *gen.SendInvoiceCtx, in gen.SendInvoiceInput) (*gen.Invoice, error) {
	inv, err := c.DB.Invoices.Get(c, in.InvoiceID)
	if err != nil {
		return nil, err
	}
	if inv.Status != gen.InvoiceStatusDraft {
		return nil, gen.ErrSendInvoiceNotDraft()
	}
	updated, err := c.DB.Invoices.Update(c, inv.ID, inv.Version, gen.InvoicePatch{
		Status: ks.Set(gen.InvoiceStatusSent),
		SentAt: ks.Set(c.Clock.Now()),
	}.Assignments())
	if err != nil {
		return nil, err
	}
	// The effect is recorded inside this transaction: it happens if and only if the commit
	// happens, so a rollback cannot leave a sent email behind (§10.1).
	if _, err := c.Email.Send(c, []byte("{}")); err != nil {
		return nil, err
	}
	return updated, nil
}

// markPaid records a payment the PSP already took. A mismatched amount is a declared failure,
// not a validation error, because only this integration can produce it (§7.4).
func markPaid(c *gen.MarkPaidCtx, in gen.MarkPaidInput) (*gen.Invoice, error) {
	inv, err := c.DB.Invoices.Get(c, in.InvoiceID)
	if err != nil {
		return nil, err
	}
	if inv.Status == gen.InvoiceStatusPaid {
		return nil, gen.ErrMarkPaidAlreadyPaid()
	}
	if inv.Amount != in.Amount {
		return nil, gen.ErrMarkPaidAmountMismatch()
	}
	// Confirm the capture with the PSP before recording it. The call happens now and cannot
	// be rolled back, so the spec names RefundCharge to undo it if the write below fails.
	if _, err := c.HTTP.Send(c, []byte(`{"capture":"`+in.PspChargeID+`"}`), "api.psp.example"); err != nil {
		return nil, err
	}
	return c.DB.Invoices.Update(c, inv.ID, inv.Version, gen.InvoicePatch{
		Status: ks.Set(gen.InvoiceStatusPaid),
		PaidAt: ks.Set(c.Clock.Now()),
	}.Assignments())
}

// listInvoices is a page. Ordering always ends with the id, so a cursor cannot skip or
// repeat a row while the table changes underneath it (§8.1).
func listInvoices(c *gen.ListInvoicesCtx, in gen.ListInvoicesInput) (*runtime.PageOf[gen.Invoice], error) {
	q := c.DB.Invoices.Q().OrderBy(gen.Invoices.CreatedAt.Desc())
	if in.Status.Present() {
		v, _ := in.Status.Get()
		q = q.Where(gen.Invoices.Status.Eq(gen.InvoiceStatus(v)))
	}
	return q.Page(c, in.Cursor, in.Limit)
}

// countDrafts is the aggregate a dashboard asks for. It is one query, and the read rule is
// part of it, so the number is the count *this actor* can see (§8.5).
func countDrafts(c *gen.CountDraftsCtx, in gen.CountDraftsInput) (gen.CountDraftsOut, error) {
	total, err := c.DB.Invoices.Q().Where(gen.Invoices.Status.Eq(gen.InvoiceStatusDraft)).Count(c)
	return gen.CountDraftsOut{Total: total}, err
}

// sendReminders runs as a scheduled job. It pages instead of loading every open invoice,
// because a job's memory should not grow with how well the business is doing (§8.1).
func sendReminders(c *gen.SendRemindersCtx, in gen.SendRemindersInput) (struct{}, error) {
	err := c.DB.Invoices.Q().
		Where(gen.Invoices.Status.In(gen.InvoiceStatusDraft, gen.InvoiceStatusSent)).
		OrderBy(gen.Invoices.CreatedAt.Asc()).
		Each(c, 200, func(inv *gen.Invoice) error {
			_, err := c.Email.Send(c, []byte("{}"))
			return err
		})
	return struct{}{}, err
}

// refundCharge runs when a confirmed capture's MarkPaid did not commit: the money moved,
// the invoice did not, so the capture is refunded (§10.2).
func refundCharge(c *gen.RefundChargeCtx, in gen.RefundChargeInput) (struct{}, error) {
	_, err := c.HTTP.Send(c, append([]byte(`{"refund":`), append(in.Effect.Payload, '}')...), "api.psp.example")
	return struct{}{}, err
}
