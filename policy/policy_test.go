package policy

import (
	"strings"
	"testing"
	"time"
)

var userTable = &Table{
	Module: "users", Name: "User", Table: "user_account", Tenant: true, IDColumn: "id",
	Fields: []Field{{Name: "id", Column: "id", Kind: "id"}},
}

var invoiceTable = &Table{
	Module: "billing", Name: "Invoice", Table: "invoice", Tenant: true, IDColumn: "id",
	Fields: []Field{
		{Name: "id", Column: "id", Kind: "id"},
		{Name: "status", Column: "status", Kind: "enum"},
		{Name: "amount", Column: "amount_minor", Kind: "money", Columns: []string{"amount_minor", "amount_currency"}},
		{Name: "paid_at", Column: "paid_at", Kind: "time"},
		{Name: "notes", Column: "notes", Kind: "text"},
		{Name: "customer", Column: "customer_id", Kind: "ref", RefTo: &Table{
			Module: "customers", Name: "Customer", Table: "customer", Tenant: true, IDColumn: "id",
			Fields: []Field{
				{Name: "id", Column: "id", Kind: "id"},
				{Name: "plan", Column: "plan", Kind: "enum"},
				{Name: "owner", Column: "owner_id", Kind: "ref", RefTo: userTable},
			},
		}},
	},
}

func render(t *testing.T, n *Node, tbl *Table, target Target, alias string) (string, []Bind) {
	t.Helper()
	s, err := TableSQL(n, tbl, target, alias)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	return s.Text, s.Binds
}

func TestRoleCompilesToMembershipLookup(t *testing.T) {
	rule := Or(Role("admin"), Role("accountant"))
	text, binds := render(t, rule, invoiceTable, TargetQuery, "ks_t")
	if !strings.Contains(text, "FROM ks_membership m") {
		t.Fatalf("want a membership lookup, got %s", text)
	}
	if !strings.Contains(text, "m.role IN (?)") {
		t.Fatalf("want role params, got %s", text)
	}
	if strings.Contains(text, "current_setting") {
		t.Fatalf("query target must bind params, not GUCs: %s", text)
	}
	// Two branches, each with system shortcut + tenant + actor + one role literal.
	if len(binds) != 8 {
		t.Fatalf("want 8 binds, got %d: %+v", len(binds), binds)
	}
	if binds[0].Kind != BindActorSystem {
		t.Fatalf("first bind should be the system shortcut, got %v", binds[0].Kind)
	}
	if got := strings.Count(text, "?"); got != 8 {
		t.Fatalf("placeholder count %d must equal bind count %d", got, len(binds))
	}
}

func TestTenantFilterAndReadRuleCompose(t *testing.T) {
	f := TenantFilter(TargetQuery, false)
	r := TenantFilter(TargetQuery, true)
	tf := f.WithRoot("t").Text
	rf := r.WithRoot("t").Text
	if tf != "t.tenant_id = ?" {
		t.Fatalf("tenant filter = %q", tf)
	}
	if rf != "true" {
		t.Fatalf("cross-tenant filter should collapse to true, got %q", rf)
	}
	text, binds := render(t, Role("member"), invoiceTable, TargetQuery, "t")
	if len(binds) != 4 {
		t.Fatalf("want 4 binds, got %d: %+v", len(binds), binds)
	}
	sql, args, err := Place(&SQL{Text: "SELECT 1 FROM invoice t WHERE " + text, Binds: binds}, 1,
		ActorValues{ID: make([]byte, 16), Tenant: make([]byte, 16), IsSystem: false})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sql, "$2") || len(args) != 4 {
		t.Fatalf("renumbering wrong: %s / %v", sql, args)
	}
}

func TestInvariantToCheckConstraint(t *testing.T) {
	cases := []struct {
		name string
		node *Node
		want string
	}{
		{"positive", Cmp(OpGt, FieldRef("amount"), Literal(IntLit(0))), "invoice.amount_minor > 0"},
		{
			"implies",
			Implies(Cmp(OpEq, FieldRef("status"), Literal(StringLit("paid"))),
				&Node{Op: OpIsSet, Kids: []*Node{FieldRef("paid_at")}}),
			"(NOT (invoice.status = 'paid') OR (invoice.paid_at IS NOT NULL))",
		},
		{
			"in",
			Cmp(OpIn, FieldRef("status"), Literal(ListLit("string", []Lit{StringLit("draft"), StringLit("sent")}))),
			"invoice.status = ANY(ARRAY['draft','sent']::text[])",
		},
		{
			"ne is distinct",
			Cmp(OpNe, FieldRef("notes"), Literal(StringLit("x"))),
			"(invoice.notes IS DISTINCT FROM 'x')",
		},
	}
	for _, tc := range cases {
		s, err := TableSQL(tc.node, invoiceTable, TargetDDL, "invoice")
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if s.Text != tc.want {
			t.Errorf("%s:\n got %s\nwant %s", tc.name, s.Text, tc.want)
		}
		if len(s.Binds) != 0 {
			t.Errorf("%s: DDL must inline literals, got %d binds", tc.name, len(s.Binds))
		}
	}
}

func TestActorRefsRejectedInConstraints(t *testing.T) {
	if _, err := TableSQL(Role("admin"), invoiceTable, TargetDDL, "invoice"); err == nil {
		t.Fatal("a role rule must not compile to a CHECK constraint")
	}
	if _, err := TableSQL(Cmp(OpEq, FieldRef("customer.owner"), ActorID()), invoiceTable, TargetDDL, "invoice"); err == nil {
		t.Fatal("actor.id must not compile to a CHECK constraint")
	}
}

func TestRefPathCompilesToCorrelatedSubquery(t *testing.T) {
	s, err := TableSQL(Cmp(OpEq, FieldRef("customer", "plan"), Literal(StringLit("pro"))), invoiceTable, TargetQuery, "t")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(s.Text, "(SELECT ks_sub.plan FROM customer ks_sub WHERE ks_sub.id = t.customer_id)") {
		t.Fatalf("subselect wrong: %s", s.Text)
	}
	// .id through a ref uses the foreign key directly, no subquery.
	s2, err := TableSQL(Cmp(OpEq, FieldRef("customer", "id"), ActorID()), invoiceTable, TargetQuery, "t")
	if err != nil {
		t.Fatal(err)
	}
	if s2.Text != "t.customer_id = ?" {
		t.Fatalf("want fk shortcut, got %s", s2.Text)
	}
}

func TestUnknownFieldIsACompileError(t *testing.T) {
	if _, err := TableSQL(FieldRef("nope"), invoiceTable, TargetQuery, "t"); err == nil {
		t.Fatal("want an error for an unknown field (KS-SPEC-002)")
	}
	if _, err := TableSQL(Cmp(OpEq, FieldRef("notes", "notes"), Literal(StringLit("a"))), invoiceTable, TargetQuery, "t"); err == nil {
		t.Fatal("want an error for a non-relation path")
	}
}

func TestPathDepthLimitedToTwo(t *testing.T) {
	if err := (&Node{Op: OpField, Path: []string{"a", "b", "c"}}).Valid(); err == nil {
		t.Fatal("path depth must be capped at 2 (§5.4)")
	}
}

func TestEvalSemantics(t *testing.T) {
	env := &Env{
		Row:  Row{"status": "sent", "amount": int64(5000), "paid_at": nil, "notes": nil},
		Refs: map[string]Row{"customer": {"plan": "pro", "id": "c1"}},
		A:    ActorInfo{ID: "a1", Tenant: "t1", Roles: []string{"accountant"}},
	}
	yes := []struct {
		name string
		n    *Node
	}{
		{"role", Role("admin", "accountant")},
		{"or", Or(Role("nobody"), Cmp(OpEq, FieldRef("status"), Literal(StringLit("sent"))))},
		{"money gt", Cmp(OpGt, FieldRef("amount"), Literal(IntLit(0)))},
		{"ref path", Cmp(OpEq, FieldRef("customer", "plan"), Literal(StringLit("pro")))},
		{"implies true branch", Implies(Cmp(OpEq, FieldRef("status"), Literal(StringLit("sent"))), Cmp(OpGt, FieldRef("amount"), Literal(IntLit(1000))))},
		{"implies vacuous", Implies(Cmp(OpEq, FieldRef("status"), Literal(StringLit("paid"))), &Node{Op: OpIsSet, Kids: []*Node{FieldRef("paid_at")}})},
		{"in", Cmp(OpIn, FieldRef("status"), Literal(ListLit("string", []Lit{StringLit("draft"), StringLit("sent")})))},
		{"null compare", Cmp(OpNe, FieldRef("notes"), Literal(StringLit("x")))},
		{"prefix", Cmp(OpPrefix, FieldRef("status"), Literal(StringLit("se")))},
	}
	for _, tc := range yes {
		got, err := Eval(tc.n, env)
		if err != nil || !got {
			t.Errorf("%s: got %v, %v want true", tc.name, got, err)
		}
	}
	no := []struct {
		name string
		n    *Node
	}{
		{"role missing", Role("admin")},
		{"null lt", Cmp(OpLt, FieldRef("paid_at"), Literal(TimeLit("2026-01-01T00:00:00Z")))},
		{"implies fails", Implies(Cmp(OpEq, FieldRef("status"), Literal(StringLit("sent"))), &Node{Op: OpIsSet, Kids: []*Node{FieldRef("paid_at")}})},
		{"never", Never()},
	}
	for _, tc := range no {
		got, err := Eval(tc.n, env)
		if err != nil || got {
			t.Errorf("%s: got %v, %v want false", tc.name, got, err)
		}
	}
}

func TestCompareClassifiesWidening(t *testing.T) {
	old := Role("admin")
	cases := []struct {
		name string
		a, b *Node
		want Relation
	}{
		{"same", old, Role("admin"), Equal},
		{"role added", old, Or(Role("admin"), Role("member")), Widened},
		{"role list widened", old, Role("accountant", "admin"), Widened},
		{"role removed", Role("admin", "member"), Role("admin"), Narrowed},
		{"and clause added", Role("admin"), And(Role("admin"), Owner("customer")), Narrowed},
		{"and clause dropped", And(Role("admin"), Cmp(OpEq, FieldRef("status"), Literal(StringLit("draft")))), Role("admin"), Widened},
		{"replaced by always", old, Always(), Widened},
		{"replaced by never", Always(), old, Narrowed},
		{"bound loosened", Cmp(OpGt, FieldRef("amount"), Literal(IntLit(100))), Cmp(OpGt, FieldRef("amount"), Literal(IntLit(0))), Widened},
		{"in narrowed", Cmp(OpIn, FieldRef("status"), Literal(ListLit("string", []Lit{StringLit("draft"), StringLit("sent"), StringLit("paid")}))),
			Cmp(OpIn, FieldRef("status"), Literal(ListLit("string", []Lit{StringLit("draft")}))), Narrowed},
		{"unrelated shapes", Cmp(OpEq, FieldRef("status"), Literal(StringLit("sent"))), Role("member"), Unknown},
		{"negation flip is unprovable", Not(Role("admin")), Role("admin"), Unknown},
	}
	for _, tc := range cases {
		got, note := Compare(tc.a, tc.b)
		if got != tc.want {
			t.Errorf("%s: got %s (%s) want %s", tc.name, got, note, tc.want)
		}
	}
}

func TestCanonicalizationMakesImpliesComparable(t *testing.T) {
	a := Implies(Cmp(OpEq, FieldRef("status"), Literal(StringLit("paid"))), &Node{Op: OpIsSet, Kids: []*Node{FieldRef("paid_at")}})
	b := Or(Not(Cmp(OpEq, FieldRef("status"), Literal(StringLit("paid")))), &Node{Op: OpIsSet, Kids: []*Node{FieldRef("paid_at")}})
	if !a.Equal(b) {
		t.Fatalf("a->b must equal !a|b:\n %s\n %s", Canonical(a), Canonical(b))
	}
	rel, _ := Compare(a, b)
	if rel != Equal {
		t.Fatalf("want equal, got %s", rel)
	}
}

func TestUsesActorAndCrossesRefDecisions(t *testing.T) {
	rowOnly := Cmp(OpGt, FieldRef("amount"), Literal(IntLit(0)))
	if rowOnly.UsesActor() || rowOnly.CrossesRef() {
		t.Fatal("row-only rule should become a CHECK constraint")
	}
	traverses := Cmp(OpEq, FieldRef("customer", "plan"), Literal(StringLit("pro")))
	if !traverses.CrossesRef() || traverses.UsesActor() {
		t.Fatal("ref-crossing rule must become a deferred check")
	}
	if !Role("admin").UsesActor() {
		t.Fatal("role rules reference the actor")
	}
}

func TestLiteralRoundTripIsStable(t *testing.T) {
	n := And(Role("b", "a"), Cmp(OpIn, FieldRef("status"), Literal(ListLit("string", []Lit{StringLit("z"), StringLit("y")}))), Cmp(OpLte, FieldRef("paid_at"), Literal(TimeLit("2026-01-01T00:00:00Z"))))
	c1 := Canonical(n)
	again := Normalize(Normalize(n))
	if Canonical(again) != c1 {
		t.Fatalf("normalization is not idempotent:\n%s\n%s", c1, Canonical(again))
	}
	if !strings.Contains(c1, "2026-01-01T00:00:00Z") {
		t.Fatalf("time literal lost: %s", c1)
	}
	e, err := time.Parse(time.RFC3339Nano, "2026-01-01T00:00:00Z")
	if err != nil || e.IsZero() {
		t.Fatal("fixture time invalid")
	}
}
