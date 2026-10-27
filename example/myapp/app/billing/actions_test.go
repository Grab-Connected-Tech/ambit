package billing

import (
	"context"
	"strings"
	"testing"
	"time"

	"keystone.dev/keystone/example/myapp/app"
	"keystone.dev/keystone/example/myapp/gen"
	"keystone.dev/keystone/ks"
	"keystone.dev/keystone/kstest"
	"keystone.dev/keystone/runtime"
)

// world is one tenant with an accountant, a plain member and a draft invoice.
type world struct {
	env        *kstest.Env
	org        ks.ID
	accountant ks.Actor
	member     ks.Actor
	invoice    *gen.Invoice
}

func setup(t *testing.T) world {
	env := kstest.New(t, gen.Model)
	org := env.Tenant()
	acct := env.Actor(org, app.Accountant, app.Member)
	cust := kstest.Create(env, gen.CustomerTable, gen.Customer{TenantID: org, Name: "Ada", Email: "ada@example.test", OwnerID: acct.ID})
	inv := kstest.Create(env, gen.InvoiceTable, gen.Invoice{
		TenantID: org, CustomerID: cust.ID, Amount: ks.MoneyOf(5000, "USD"),
		Status: gen.InvoiceStatusDraft, Notes: ks.Set("net 30"),
	})
	return world{env: env, org: org, accountant: acct, member: env.Actor(org, app.Member), invoice: inv}
}

func TestSendInvoice(t *testing.T) {
	w := setup(t)
	out, err := kstest.Call(w.env, w.accountant, gen.SendInvoiceHandler, Actions.SendInvoice, gen.SendInvoiceInput{InvoiceID: w.invoice.ID})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != gen.InvoiceStatusSent {
		t.Fatalf("status %s, want sent", out.Status)
	}
	if at, _ := out.SentAt.Get(); !at.Equal(kstest.Epoch) {
		t.Fatalf("sent_at comes from the clock: got %v", at)
	}
	if n := len(w.env.Emails()); n != 1 {
		t.Fatalf("one email is queued with the commit, got %d", n)
	}
	w.env.AssertQueriesAtMost(4)

	// Sending twice is a declared failure, and its effect is rolled back with it.
	_, err = kstest.Call(w.env, w.accountant, gen.SendInvoiceHandler, Actions.SendInvoice, gen.SendInvoiceInput{InvoiceID: w.invoice.ID})
	if ks.AsError(err).Code != "not_draft" {
		t.Fatalf("got %v, want not_draft", err)
	}
	if n := len(w.env.Emails()); n != 1 {
		t.Fatalf("a failed send must not queue an email, got %d", n)
	}
}

func TestSendInvoiceNeedsARole(t *testing.T) {
	w := setup(t)
	_, err := kstest.Call(w.env, w.member, gen.SendInvoiceHandler, Actions.SendInvoice, gen.SendInvoiceInput{InvoiceID: w.invoice.ID})
	if ks.AsError(err).Code != ks.CodeForbidden {
		t.Fatalf("a member may not send: got %v", err)
	}
}

func TestAnotherTenantCannotSeeTheInvoice(t *testing.T) {
	w := setup(t)
	other := w.env.Actor(w.env.Tenant(), app.Admin, app.Accountant, app.Member)
	_, err := kstest.Call(w.env, other, gen.SendInvoiceHandler, Actions.SendInvoice, gen.SendInvoiceInput{InvoiceID: w.invoice.ID})
	if ks.AsError(err).Code != ks.CodeNotFound {
		t.Fatalf("another tenant's row is invisible, not forbidden: got %v", err)
	}
	page, err := kstest.Call(w.env, other, gen.ListInvoicesHandler, Actions.ListInvoices, gen.ListInvoicesInput{})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Rows) != 0 {
		t.Fatalf("another tenant lists %d invoices", len(page.Rows))
	}
}

func TestMarkPaid(t *testing.T) {
	w := setup(t)
	_, err := kstest.Call(w.env, w.accountant, gen.MarkPaidHandler, Actions.MarkPaid,
		gen.MarkPaidInput{InvoiceID: w.invoice.ID, Amount: ks.MoneyOf(4000, "USD"), PspChargeID: "ch_1"})
	if ks.AsError(err).Code != "amount_mismatch" {
		t.Fatalf("got %v, want amount_mismatch", err)
	}
	w.env.Clock.Advance(time.Hour)
	out, err := kstest.Call(w.env, w.accountant, gen.MarkPaidHandler, Actions.MarkPaid,
		gen.MarkPaidInput{InvoiceID: w.invoice.ID, Amount: ks.MoneyOf(5000, "USD"), PspChargeID: "ch_1"})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != gen.InvoiceStatusPaid || !out.PaidAt.Present() {
		t.Fatalf("paid invoice: %+v", out)
	}
	// A paid invoice is outside the Update rule (draft or sent), so a second payment
	// cannot even see it as writable.
	_, err = kstest.Call(w.env, w.accountant, gen.MarkPaidHandler, Actions.MarkPaid,
		gen.MarkPaidInput{InvoiceID: w.invoice.ID, Amount: ks.MoneyOf(5000, "USD"), PspChargeID: "ch_2"})
	if code := ks.AsError(err).Code; code != "already_paid" {
		t.Fatalf("got %v, want already_paid", err)
	}
}

func TestListInvoicesHidesNotesFromMembers(t *testing.T) {
	w := setup(t)
	page, err := kstest.Call(w.env, w.member, gen.ListInvoicesHandler, Actions.ListInvoices, gen.ListInvoicesInput{})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Rows) != 1 {
		t.Fatalf("a member sees the tenant's invoice: got %d", len(page.Rows))
	}
	if page.Rows[0].Notes.Present() {
		t.Fatal("notes are readable by admins and accountants only")
	}
	page, err = kstest.Call(w.env, w.accountant, gen.ListInvoicesHandler, Actions.ListInvoices, gen.ListInvoicesInput{})
	if err != nil {
		t.Fatal(err)
	}
	if notes, _ := page.Rows[0].Notes.Get(); notes != "net 30" {
		t.Fatalf("an accountant reads notes: got %q", notes)
	}
}

func TestCountDrafts(t *testing.T) {
	w := setup(t)
	out, err := kstest.Call(w.env, w.member, gen.CountDraftsHandler, Actions.CountDrafts, gen.CountDraftsInput{})
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
	system := ks.Actor{Kind: ks.ActorSystem, ID: w.org, Tenant: w.org, Authed: true}
	if _, err := kstest.Call(w.env, system, gen.SendRemindersHandler, Actions.SendReminders, gen.SendRemindersInput{}); err != nil {
		t.Fatal(err)
	}
	if n := len(w.env.Emails()); n != 1 {
		t.Fatalf("one open invoice, %d reminders", n)
	}
}

func TestFuzzBillingActions(t *testing.T) {
	env := kstest.New(t, gen.Model)
	kstest.Fuzz(env, gen.SendInvoiceHandler, Actions.SendInvoice, 40)
	kstest.Fuzz(env, gen.MarkPaidHandler, Actions.MarkPaid, 40)
	kstest.Fuzz(env, gen.ListInvoicesHandler, Actions.ListInvoices, 40)
}

func TestTaxIDIsEncryptedAtRest(t *testing.T) {
	w := setup(t)
	admin := w.env.Actor(w.org, app.Admin, app.Member)
	inv := kstest.Create(w.env, gen.InvoiceTable, gen.Invoice{
		TenantID: w.org, CustomerID: w.invoice.CustomerID, Amount: ks.MoneyOf(100, "USD"),
		Status: gen.InvoiceStatusDraft, TaxId: ks.Set("DE123456789"),
	})
	var raw []byte
	w.env.QueryRow(`SELECT tax_id FROM invoice WHERE id = $1`, []any{inv.ID}, &raw)
	if len(raw) == 0 || strings.Contains(string(raw), "DE123456789") {
		t.Fatalf("tax_id is stored in the clear: %q", raw)
	}
	page, err := kstest.Call(w.env, admin, gen.ListInvoicesHandler, Actions.ListInvoices, gen.ListInvoicesInput{})
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
