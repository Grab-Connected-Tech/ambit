// Package customers owns the Customer resource. Humans own this file; the module's
// actions folder is AI-written (§4).
package customers

import (
	s "github.com/Grab-Connected-Tech/ambit/spec"

	"github.com/Grab-Connected-Tech/ambit/example/myapp/app"
)

// Customer is who an invoice is billed to.
var Customer = s.Resource("Customer",
	s.Tenant(),
	s.Fields(
		s.Text("name").MaxLen(200),
		s.Email("email").Unique(),
		s.Text("external_id").Optional().MaxLen(64),
		s.Ref("owner", app.User),
	),
	s.Index("owner"),
	s.Access(
		s.Read(app.Member),
		s.Create(app.Admin.Or(app.Accountant)),
		s.Update(app.Admin.Or(app.Accountant)),
		s.Delete(app.Admin),
	),
)

// GetCustomer is the read other modules call. Calls run in the same transaction, as
// the same actor, with the callee's rules applied (§4.1).
var GetCustomer = s.Query("GetCustomer",
	s.Input(s.ID("customer_id", Customer)),
	s.Returns(Customer),
	s.Requires(app.Member),
	s.Reads(Customer),
)

// ListCustomers is a paginated list with a filter and a cache (§11.3).
var ListCustomers = s.Query("ListCustomers",
	s.Input(
		s.FieldOf(Customer, "owner").Optional(),
		s.Text("q").Optional().MaxLen(100),
	),
	s.Returns(s.Page(Customer)),
	s.Requires(app.Member),
	s.Reads(Customer),
	s.Cache(s.TTL(5*s.Minute), s.InvalidatedBy(Customer)),
)

// CreateCustomer is a write path, so it carries an idempotency key (§5.7).
var CreateCustomer = s.Mutation("CreateCustomer",
	s.Input(
		s.Text("name").MaxLen(200),
		s.Email("email"),
		s.Ref("owner", app.User).Optional(),
	),
	s.Returns(Customer),
	s.Requires(app.Admin.Or(app.Accountant)),
	s.Writes(Customer),
	s.RateLimit(s.PerActor(120, s.Minute)),
	s.Idempotent(),
	s.Errors("email_taken"),
)
