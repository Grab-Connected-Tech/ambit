// Package app holds the app-level spec: actors, roles, tenancy and identity.
// It is the only spec file outside a module (§4.2). Humans own this file.
package app

import (
	s "keystone.dev/keystone/spec"
)

// Org is the tenant: the top-level isolation boundary.
var Org = s.TenantResource("Org",
	s.Fields(
		s.Text("name").MaxLen(200),
		s.Enum("plan", "free", "pro", "enterprise").Default("free"),
	),
	s.Access(
		s.Read(Member),
		s.Update(Admin),
	),
)

// User is the account an actor logs in as. Its id is the actor id, so
// s.Owner("user") on any resource means "this account created it".
var User = s.Account("User",
	s.Fields(
		s.Email("email").Unique(),
		s.Text("name").MaxLen(200).Optional(),
		s.Timestamp("last_login").Optional(),
		s.Text("passwordHash").Optional().MaxLen(200).Sensitive(),
	),
	// Sign-up creates exactly one account per call, so Create is open and the
	// signup action itself carries a mandatory rate limit (KS-SEC-005).
	s.Access(
		s.Create(s.Always()),
		s.Read(s.Owner("id").Or(Admin)),
		s.Update(s.Owner("id").Or(Admin)),
		s.Delete(Admin),
		s.FieldRead("passwordHash", Admin),
	),
)

// Roles are granted per tenant through the generated Membership resource (§5.1).
var (
	Admin      = s.Role("admin")
	Accountant = s.Role("accountant")
	Member     = s.Role("member")
)

// Identity declares how callers become actors. Sessions rotate on login and on
// privilege change (§9.2).
var Identity = s.Identity(
	s.Sessions(s.TTL(30*s.Day), s.IdleTimeout(7*s.Day)),
	s.APITokens(s.Scoped()),
	s.OIDC("google"),
)
