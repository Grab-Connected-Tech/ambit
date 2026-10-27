package customers

import (
	"testing"

	"github.com/abhilashiig/ambit/am"
	"github.com/abhilashiig/ambit/amtest"
	"github.com/abhilashiig/ambit/example/myapp/app"
	"github.com/abhilashiig/ambit/example/myapp/gen"
)

func TestCreateGetAndListCustomers(t *testing.T) {
	env := amtest.New(t, gen.Model)
	org := env.Tenant()
	acct := env.Actor(org, app.Accountant, app.Member)
	member := env.Actor(org, app.Member)

	created, err := amtest.Call(env, acct, gen.CreateCustomerHandler, Actions.CreateCustomer,
		gen.CreateCustomerInput{Name: "Ada", Email: "ada@example.test", Owner: am.Set(acct.ID)})
	if err != nil {
		t.Fatal(err)
	}
	if created.TenantID != org || created.OwnerID != acct.ID {
		t.Fatalf("tenant and owner come from the actor and the input: %+v", created)
	}

	got, err := amtest.Call(env, member, gen.GetCustomerHandler, Actions.GetCustomer, gen.GetCustomerInput{CustomerID: created.ID})
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "Ada" {
		t.Fatalf("got %+v", got)
	}

	page, err := amtest.Call(env, member, gen.ListCustomersHandler, Actions.ListCustomers, gen.ListCustomersInput{Q: am.Set("Ad")})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Rows) != 1 {
		t.Fatalf("the name filter finds one customer, got %d", len(page.Rows))
	}
	env.AssertQueriesAtMost(2)
}

func TestMembersCannotCreateCustomers(t *testing.T) {
	env := amtest.New(t, gen.Model)
	org := env.Tenant()
	member := env.Actor(org, app.Member)
	_, err := amtest.Call(env, member, gen.CreateCustomerHandler, Actions.CreateCustomer,
		gen.CreateCustomerInput{Name: "Eve", Email: "eve@example.test"})
	if am.AsError(err).Code != am.CodeForbidden {
		t.Fatalf("got %v, want forbidden", err)
	}
}

func TestInputIsValidatedBeforeTheAction(t *testing.T) {
	env := amtest.New(t, gen.Model)
	acct := env.Actor(env.Tenant(), app.Accountant)
	_, err := amtest.Call(env, acct, gen.CreateCustomerHandler, Actions.CreateCustomer,
		gen.CreateCustomerInput{Name: "Bob", Email: "not-an-email"})
	if am.AsError(err).Code != am.CodeInvalid {
		t.Fatalf("got %v, want invalid", err)
	}
}

func TestFuzzCustomerActions(t *testing.T) {
	env := amtest.New(t, gen.Model)
	amtest.Fuzz(env, gen.GetCustomerHandler, Actions.GetCustomer, 40)
	amtest.Fuzz(env, gen.CreateCustomerHandler, Actions.CreateCustomer, 40)
	amtest.Fuzz(env, gen.ListCustomersHandler, Actions.ListCustomers, 40)
}

func TestListCustomersIsCachedAndInvalidatedByWrites(t *testing.T) {
	env := amtest.New(t, gen.Model)
	org := env.Tenant()
	acct := env.Actor(org, app.Accountant, app.Member)
	list := func(a am.Actor) int {
		page, err := amtest.Call(env, a, gen.ListCustomersHandler, Actions.ListCustomers, gen.ListCustomersInput{})
		if err != nil {
			t.Fatal(err)
		}
		return len(page.Rows)
	}
	if n := list(acct); n != 0 {
		t.Fatalf("empty tenant lists %d", n)
	}
	executions := len(env.Traces())
	list(acct)
	if len(env.Traces()) != executions {
		t.Fatal("the second identical call is answered from the cache, without running the action")
	}
	if _, err := amtest.Call(env, acct, gen.CreateCustomerHandler, Actions.CreateCustomer,
		gen.CreateCustomerInput{Name: "Ada", Email: "ada@example.test"}); err != nil {
		t.Fatal(err)
	}
	if n := list(acct); n != 1 {
		t.Fatalf("a write to Customer invalidates the cached list: got %d", n)
	}
	// Another tenant never sees this tenant's cached page.
	other := env.Actor(env.Tenant(), app.Member)
	if n := list(other); n != 0 {
		t.Fatalf("another tenant lists %d", n)
	}
}
