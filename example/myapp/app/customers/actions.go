// This file holds the customers module's action implementations. See the billing module's
// file for why they arrive through a generated context (§7.2).
package customers

import (
	"github.com/abhilashiig/ambit/example/myapp/gen"
	"github.com/abhilashiig/ambit/runtime"
)

var Actions = struct {
	CreateCustomer func(*gen.CreateCustomerCtx, gen.CreateCustomerInput) (*gen.Customer, error)
	GetCustomer    func(*gen.GetCustomerCtx, gen.GetCustomerInput) (*gen.Customer, error)
	ListCustomers  func(*gen.ListCustomersCtx, gen.ListCustomersInput) (*runtime.PageOf[gen.Customer], error)
}{
	CreateCustomer: createCustomer,
	GetCustomer:    getCustomer,
	ListCustomers:  listCustomers,
}

// createCustomer is a write path, so the spec declares it idempotent: a retried POST with
// the same key returns the first result instead of inserting a second customer (§5.7).
func createCustomer(c *gen.CreateCustomerCtx, in gen.CreateCustomerInput) (*gen.Customer, error) {
	sets := []runtime.Assignment{
		runtime.SetColumn("name", in.Name),
		runtime.SetColumn("email", in.Email),
	}
	// The owner is required on the row but optional in the input: it defaults to the caller.
	sets = append(sets, runtime.SetColumn("owner_id", in.Owner.Or(c.Actor.ID).Bytes()))
	return c.DB.Customers.Insert(c, sets)
}

// getCustomer is the read other modules call. It runs in the caller's transaction as the
// caller's actor, so this resource's read rule applies even through a call (§4.1).
func getCustomer(c *gen.GetCustomerCtx, in gen.GetCustomerInput) (*gen.Customer, error) {
	return c.DB.Customers.Get(c, in.CustomerID)
}

// listCustomers filters by the owner the caller may see. The email is never selected: the
// resource's field-read rule removes it from the projection, not from the response (§5.6).
func listCustomers(c *gen.ListCustomersCtx, in gen.ListCustomersInput) (*runtime.PageOf[gen.Customer], error) {
	q := c.DB.Customers.Q().OrderBy(gen.Customers.CreatedAt.Desc())
	if in.Owner.Present() {
		v, _ := in.Owner.Get()
		q = q.Where(gen.Customers.OwnerID.Eq(v))
	}
	if in.Q.Present() {
		v, _ := in.Q.Get()
		q = q.Where(gen.Customers.Name.Contains(v))
	}
	return q.Page(c, in.Cursor, in.Limit)
}
