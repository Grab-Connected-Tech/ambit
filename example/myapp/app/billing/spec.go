// Package billing owns invoices. This file is the human review surface: resources,
// rules and actions, with no implementation (§5).
package billing

import (
	s "github.com/Grab-Connected-Tech/ambit/spec"

	"github.com/Grab-Connected-Tech/ambit/example/myapp/app"
	"github.com/Grab-Connected-Tech/ambit/example/myapp/app/customers"
)

// Invoice is the module's central resource.
var Invoice = s.Resource("Invoice",
	s.Tenant(),
	s.Fields(
		s.Ref("customer", customers.Customer),
		s.Money("amount"),
		s.Enum("status", "draft", "sent", "paid", "void").Default("draft"),
		s.Timestamp("sent_at").Optional(),
		s.Timestamp("paid_at").Optional(),
		s.Text("notes").Optional().MaxLen(2000).Sensitive(),
		s.Encrypted(s.Text("tax_id").Optional().MaxLen(40)),
	),
	s.Index("customer", "status"),
	s.Invariants(
		s.Check("amount_positive", s.F("amount").Gt(0)),
		s.Check("paid_has_date", s.F("status").Eq("paid").Implies(s.F("paid_at").IsSet())),
		s.Check("sent_has_date", s.F("status").Eq("sent").Implies(s.F("sent_at").IsSet())),
		s.Check("void_not_paid", s.F("status").Eq("void").Implies(s.Not(s.F("paid_at").IsSet()))),
	),
	s.Access(
		s.Read(app.Member),
		s.Create(app.Admin.Or(app.Accountant)),
		s.Update(app.Admin.Or(app.Accountant).And(s.F("status").In("draft", "sent"))),
		s.Delete(s.Never()),
		s.FieldRead("notes", app.Admin.Or(app.Accountant)),
		s.FieldRead("tax_id", app.Admin),
		s.FieldWrite("tax_id", app.Admin),
	),
)

// SendInvoice emails a draft invoice and marks it sent.
var SendInvoice = s.Mutation("SendInvoice",
	s.Input(s.ID("invoice_id", Invoice)),
	s.Returns(Invoice),
	s.Requires(app.Admin.Or(app.Accountant)),
	s.Reads(customers.Customer),
	s.Writes(Invoice),
	s.Effects(s.EmailEffect()),
	s.Errors("not_draft"),
	s.RateLimit(s.PerActor(60, s.Minute)),
)

// MarkPaid records a payment through an external PSP. The HTTP effect is immediate
// because the response carries the charge id, so it names a compensating effect.
var MarkPaid = s.Mutation("MarkPaid",
	s.Input(
		s.ID("invoice_id", Invoice),
		s.Money("amount"),
		s.Text("psp_charge_id").MaxLen(200),
	),
	s.Returns(Invoice),
	s.Requires(app.Admin.Or(app.Accountant)),
	s.Writes(Invoice),
	s.Effects(
		s.Webhook(s.Topic("invoice.paid")),
		// Confirming the capture is immediate: if the payment write then fails, the
		// runtime queues RefundCharge, because the PSP call cannot be rolled back.
		s.HTTP(s.Hosts("api.psp.example")).Immediate().Compensate("billing.RefundCharge"),
	),
	s.Errors("already_paid", "amount_mismatch"),
	s.QueryBudget(8),
	s.Idempotent(),
)

// ListInvoices is the module's list endpoint.
var ListInvoices = s.Query("ListInvoices",
	s.Input(s.FieldOf(Invoice, "status").Optional()),
	s.Returns(s.Page(Invoice)),
	s.Requires(app.Member),
	s.Reads(Invoice),
)

// CountDrafts asks for a total, so it declares Counts() (§8.1).
var CountDrafts = s.Query("CountDrafts",
	s.Returns(s.Int("total")),
	s.Requires(app.Member),
	s.Reads(Invoice),
	s.Counts(),
)

// SendReminders walks overdue invoices. A job has no HTTP surface, so it declares
// its own reads, effects and retry policy.
var SendReminders = s.Job("SendReminders",
	s.RunAs(s.System),
	s.Reads(Invoice, customers.Customer),
	s.Effects(s.EmailEffect()),
	s.Retry(s.MaxAttempts(5), s.Exponential(s.Second, 10*s.Minute)),
	s.Concurrency(s.Key("customer"), 1),
)

// RefundCharge undoes a confirmed capture whose MarkPaid did not commit. It receives the
// effect call and the PSP's response.
var RefundCharge = s.Job("RefundCharge",
	s.RunAs(s.System),
	s.Effects(s.HTTP(s.Hosts("api.psp.example"))),
	s.Retry(s.MaxAttempts(8), s.Exponential(s.Second, 10*s.Minute)),
)

// DailyReminders is the schedule that starts the job (§10.5).
var DailyReminders = s.Schedule("DailyReminders",
	s.Cron("0 9 * * *"),
	s.Runs(SendReminders),
)
