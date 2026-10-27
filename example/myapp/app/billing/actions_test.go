package billing

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/abhilashiig/ambit/am"
	"github.com/abhilashiig/ambit/amtest"
	"github.com/abhilashiig/ambit/example/myapp/app"
	"github.com/abhilashiig/ambit/example/myapp/gen"
	"github.com/abhilashiig/ambit/runtime"
)

// world is one tenant with an accountant, a plain member and a draft invoice.
type world struct {
	env        *amtest.Env
	org        am.ID
	accountant am.Actor
	member     am.Actor
	invoice    *gen.Invoice
}

func setup(t *testing.T) world {
	env := amtest.New(t, gen.Model)
	org := env.Tenant()
	acct := env.Actor(org, app.Accountant, app.Member)
	cust := amtest.Create(env, gen.CustomerTable, gen.Customer{TenantID: org, Name: "Ada", Email: "ada@example.test", OwnerID: acct.ID})
	inv := amtest.Create(env, gen.InvoiceTable, gen.Invoice{
		TenantID: org, CustomerID: cust.ID, Amount: am.MoneyOf(5000, "USD"),
		Status: gen.InvoiceStatusDraft, Notes: am.Set("net 30"),
	})
	return world{env: env, org: org, accountant: acct, member: env.Actor(org, app.Member), invoice: inv}
}

func TestSendInvoice(t *testing.T) {
	w := setup(t)
	out, err := amtest.Call(w.env, w.accountant, gen.SendInvoiceHandler, Actions.SendInvoice, gen.SendInvoiceInput{InvoiceID: w.invoice.ID})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != gen.InvoiceStatusSent {
		t.Fatalf("status %s, want sent", out.Status)
	}
	if at, _ := out.SentAt.Get(); !at.Equal(amtest.Epoch) {
		t.Fatalf("sent_at comes from the clock: got %v", at)
	}
	if n := len(w.env.Emails()); n != 1 {
		t.Fatalf("one email is queued with the commit, got %d", n)
	}
	w.env.AssertQueriesAtMost(4)

	// Sending twice is a declared failure, and its effect is rolled back with it.
	_, err = amtest.Call(w.env, w.accountant, gen.SendInvoiceHandler, Actions.SendInvoice, gen.SendInvoiceInput{InvoiceID: w.invoice.ID})
	if am.AsError(err).Code != "not_draft" {
		t.Fatalf("got %v, want not_draft", err)
	}
	if n := len(w.env.Emails()); n != 1 {
		t.Fatalf("a failed send must not queue an email, got %d", n)
	}
}

func TestSendInvoiceNeedsARole(t *testing.T) {
	w := setup(t)
	_, err := amtest.Call(w.env, w.member, gen.SendInvoiceHandler, Actions.SendInvoice, gen.SendInvoiceInput{InvoiceID: w.invoice.ID})
	if am.AsError(err).Code != am.CodeForbidden {
		t.Fatalf("a member may not send: got %v", err)
	}
}

func TestAnotherTenantCannotSeeTheInvoice(t *testing.T) {
	w := setup(t)
	other := w.env.Actor(w.env.Tenant(), app.Admin, app.Accountant, app.Member)
	_, err := amtest.Call(w.env, other, gen.SendInvoiceHandler, Actions.SendInvoice, gen.SendInvoiceInput{InvoiceID: w.invoice.ID})
	if am.AsError(err).Code != am.CodeNotFound {
		t.Fatalf("another tenant's row is invisible, not forbidden: got %v", err)
	}
	page, err := amtest.Call(w.env, other, gen.ListInvoicesHandler, Actions.ListInvoices, gen.ListInvoicesInput{})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Rows) != 0 {
		t.Fatalf("another tenant lists %d invoices", len(page.Rows))
	}
}

func TestMarkPaid(t *testing.T) {
	w := setup(t)
	w.env.StubHTTP("api.psp.example", []byte(`{"captured":true}`))
	_, err := amtest.Call(w.env, w.accountant, gen.MarkPaidHandler, Actions.MarkPaid,
		gen.MarkPaidInput{InvoiceID: w.invoice.ID, Amount: am.MoneyOf(4000, "USD"), PspChargeID: "ch_1"})
	if am.AsError(err).Code != "amount_mismatch" {
		t.Fatalf("got %v, want amount_mismatch", err)
	}
	w.env.Clock.Advance(time.Hour)
	out, err := amtest.Call(w.env, w.accountant, gen.MarkPaidHandler, Actions.MarkPaid,
		gen.MarkPaidInput{InvoiceID: w.invoice.ID, Amount: am.MoneyOf(5000, "USD"), PspChargeID: "ch_1"})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != gen.InvoiceStatusPaid || !out.PaidAt.Present() {
		t.Fatalf("paid invoice: %+v", out)
	}
	// A paid invoice is outside the Update rule (draft or sent), so a second payment
	// cannot even see it as writable.
	_, err = amtest.Call(w.env, w.accountant, gen.MarkPaidHandler, Actions.MarkPaid,
		gen.MarkPaidInput{InvoiceID: w.invoice.ID, Amount: am.MoneyOf(5000, "USD"), PspChargeID: "ch_2"})
	if code := am.AsError(err).Code; code != "already_paid" {
		t.Fatalf("got %v, want already_paid", err)
	}
}

func TestListInvoicesHidesNotesFromMembers(t *testing.T) {
	w := setup(t)
	page, err := amtest.Call(w.env, w.member, gen.ListInvoicesHandler, Actions.ListInvoices, gen.ListInvoicesInput{})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Rows) != 1 {
		t.Fatalf("a member sees the tenant's invoice: got %d", len(page.Rows))
	}
	if page.Rows[0].Notes.Present() {
		t.Fatal("notes are readable by admins and accountants only")
	}
	page, err = amtest.Call(w.env, w.accountant, gen.ListInvoicesHandler, Actions.ListInvoices, gen.ListInvoicesInput{})
	if err != nil {
		t.Fatal(err)
	}
	if notes, _ := page.Rows[0].Notes.Get(); notes != "net 30" {
		t.Fatalf("an accountant reads notes: got %q", notes)
	}
}

func TestCountDrafts(t *testing.T) {
	w := setup(t)
	out, err := amtest.Call(w.env, w.member, gen.CountDraftsHandler, Actions.CountDrafts, gen.CountDraftsInput{})
	if err != nil {
		t.Fatal(err)
	}
	if out.Total != 1 {
		t.Fatalf("one draft, counted %d", out.Total)
	}
	w.env.AssertQueriesAtMost(1)
}

func TestSendRemindersRunsAsSystem(t *testing.T) {
	w := setup(t)
	system := am.Actor{Kind: am.ActorSystem, ID: w.org, Tenant: w.org, Authed: true}
	if _, err := amtest.Call(w.env, system, gen.SendRemindersHandler, Actions.SendReminders, gen.SendRemindersInput{}); err != nil {
		t.Fatal(err)
	}
	if n := len(w.env.Emails()); n != 1 {
		t.Fatalf("one open invoice, %d reminders", n)
	}
}

func TestFuzzBillingActions(t *testing.T) {
	env := amtest.New(t, gen.Model)
	amtest.Fuzz(env, gen.SendInvoiceHandler, Actions.SendInvoice, 40)
	env.StubHTTP("api.psp.example", []byte(`{"captured":true}`))
	amtest.Fuzz(env, gen.MarkPaidHandler, Actions.MarkPaid, 40)
	amtest.Fuzz(env, gen.ListInvoicesHandler, Actions.ListInvoices, 40)
}

func TestTaxIDIsEncryptedAtRest(t *testing.T) {
	w := setup(t)
	admin := w.env.Actor(w.org, app.Admin, app.Member)
	inv := amtest.Create(w.env, gen.InvoiceTable, gen.Invoice{
		TenantID: w.org, CustomerID: w.invoice.CustomerID, Amount: am.MoneyOf(100, "USD"),
		Status: gen.InvoiceStatusDraft, TaxId: am.Set("DE123456789"),
	})
	var raw []byte
	w.env.QueryRow(`SELECT tax_id FROM invoice WHERE id = $1`, []any{inv.ID}, &raw)
	if len(raw) == 0 || strings.Contains(string(raw), "DE123456789") {
		t.Fatalf("tax_id is stored in the clear: %q", raw)
	}
	page, err := amtest.Call(w.env, admin, gen.ListInvoicesHandler, Actions.ListInvoices, gen.ListInvoicesInput{})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range page.Rows {
		if r.ID == inv.ID {
			if v, _ := r.TaxId.Get(); v != "DE123456789" {
				t.Fatalf("an admin reads the plaintext: got %q", v)
			}
			return
		}
	}
	t.Fatal("the invoice is not listed")
}

func TestEncryptedFieldsRoundTripThroughTheRuntime(t *testing.T) {
	w := setup(t)
	admin := w.env.Actor(w.org, app.Admin, app.Member)
	err := w.env.As(admin, func(ctx context.Context, d *runtime.DB) error {
		v := runtime.Scoped(d, gen.InvoiceTable)
		inv, err := v.Insert(ctx, []runtime.Assignment{
			runtime.SetColumn("customer_id", w.invoice.CustomerID.Bytes()),
			runtime.SetColumn("amount_minor", int64(100)), runtime.SetColumn("amount_currency", "USD"),
			runtime.SetColumn("tax_id", "FR999"),
		})
		if err != nil {
			return err
		}
		if got, _ := inv.TaxId.Get(); got != "FR999" {
			t.Fatalf("insert returns the plaintext, got %q", got)
		}
		again, err := v.Get(ctx, inv.ID)
		if err != nil {
			return err
		}
		if got, _ := again.TaxId.Get(); got != "FR999" {
			t.Fatalf("a read opens what an insert sealed, got %q", got)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestAFailedPaymentWriteRefundsTheCapture(t *testing.T) {
	w := setup(t)
	w.env.StubHTTP("api.psp.example", []byte(`{"captured":true}`))
	refunds := func() int {
		var n int
		w.env.QueryRow(`SELECT count(*) FROM am_jobs WHERE action = 'billing.RefundCharge'`, nil, &n)
		return n
	}
	// The capture is confirmed, then the action fails: the transaction rolls back, the
	// capture cannot, so its compensation is queued.
	failing := func(c *gen.MarkPaidCtx, in gen.MarkPaidInput) (*gen.Invoice, error) {
		if _, err := c.HTTP.Send(c, []byte(`{"capture":"ch_9"}`), "api.psp.example"); err != nil {
			return nil, err
		}
		return nil, gen.ErrMarkPaidAmountMismatch()
	}
	in := gen.MarkPaidInput{InvoiceID: w.invoice.ID, Amount: am.MoneyOf(5000, "USD"), PspChargeID: "ch_9"}
	if _, err := amtest.Call(w.env, w.accountant, gen.MarkPaidHandler, failing, in); err == nil {
		t.Fatal("the action fails")
	}
	if n := refunds(); n != 1 {
		t.Fatalf("one refund is queued after a rolled-back capture, got %d", n)
	}
	// The worker runs the compensation as the tenant's system actor; its refund is an
	// effect like any other.
	worker := w.env.App.NewWorker()
	jobs, err := worker.Claim(context.Background(), 10)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("claim: %d jobs, %v", len(jobs), err)
	}
	h := gen.RefundChargeHandler(Actions.RefundCharge)
	w.env.App.Handlers[h.Ref] = h
	if err := worker.Run(context.Background(), jobs[0]); err != nil {
		t.Fatalf("refund job: %v cause=%v", err, errors.Unwrap(err))
	}
	var refund bool
	for _, e := range w.env.Effects() {
		if strings.Contains(string(e.Payload), `"refund"`) && strings.Contains(string(e.Payload), "ch_9") {
			refund = true
		}
	}
	if !refund {
		t.Fatalf("the refund effect names the capture: %+v", w.env.Effects())
	}
	// A payment that commits queues nothing.
	if _, err := amtest.Call(w.env, w.accountant, gen.MarkPaidHandler, Actions.MarkPaid, in); err != nil {
		t.Fatal(err)
	}
	if n := refunds(); n != 1 {
		t.Fatalf("a committed capture is not refunded, got %d refund jobs", n)
	}
}
