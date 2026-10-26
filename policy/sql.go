package policy

import (
	"fmt"
	"strings"
	"time"
)

// Target decides how operands are materialized.
type Target int

const (
	// TargetQuery embeds the fragment in a SELECT/UPDATE: actor values and
	// literals become ? placeholders that the caller renumbers.
	TargetQuery Target = iota
	// TargetDDL inlines literals for CHECK constraints; actor references are a
	// compile error because a CHECK cannot see the caller.
	TargetDDL
	// TargetRLS inlines literals and reads actor values from the transaction's
	// SET LOCAL GUCs, which is the database-side backstop (§8.5).
	TargetRLS
)

// SQL is a compiled fragment: text with ? placeholders plus ordered bindings.
type SQL struct {
	Text  string
	Binds []Bind
}

// BindKind names the thing a placeholder stands for.
type BindKind string

const (
	BindLit         BindKind = "lit"
	BindActorID     BindKind = "actor.id"
	BindActorTenant BindKind = "actor.tenant"
	BindActorSystem BindKind = "actor.system"
)

// Bind is one placeholder in a compiled fragment.
type Bind struct {
	Kind BindKind
	Lit  Lit
	Cast string
}

// rootMarker is replaced by the table prefix at render time. It is only ever
// written by this package, never by caller data.
const rootMarker = "__KS_ROOT__"

const (
	gucActorID  = "coalesce(nullif(current_setting('app.actor_id', true), ''), '00000000-0000-0000-0000-000000000000')::uuid"
	gucTenantID = "coalesce(nullif(current_setting('app.tenant_id', true), ''), '00000000-0000-0000-0000-000000000000')::uuid"
	gucIsSystem = "coalesce(nullif(current_setting('app.actor_system', true), ''), 'off')::bool"
	gucCross    = "coalesce(nullif(current_setting('app.cross_tenant', true), ''), 'off')::bool"
)

// membershipTable is the generated ks_membership table that grants roles.
const membershipTable = "ks_membership"

// compiler state.
type comp struct {
	t      *Table
	binds  []Bind
	err    error
	target Target
}

// Compile translates a rule into SQL against a table.
func Compile(n *Node, t *Table, target Target) (*SQL, error) {
	if n == nil {
		return nil, fmt.Errorf("policy: compile of nil rule")
	}
	if err := n.Valid(); err != nil {
		return nil, err
	}
	c := &comp{t: t, target: target}
	text := c.expr(n)
	if c.err != nil {
		return nil, c.err
	}
	return &SQL{Text: text, Binds: c.binds}, nil
}

func (c *comp) fail(format string, args ...any) {
	if c.err == nil {
		c.err = fmt.Errorf("policy: "+format, args...)
	}
}

func (c *comp) add(b Bind) string {
	if c.target == TargetQuery {
		c.binds = append(c.binds, b)
		return "?"
	}
	lit, err := inline(b, c.target)
	if err != nil {
		c.fail("%v", err)
		return "null"
	}
	return lit
}

func inline(b Bind, target Target) (string, error) {
	switch b.Kind {
	case BindActorID:
		if target == TargetDDL {
			return "", fmt.Errorf("actor.id used in a database constraint")
		}
		return gucActorID, nil
	case BindActorTenant:
		if target == TargetDDL {
			return "", fmt.Errorf("actor.tenant used in a database constraint")
		}
		return gucTenantID, nil
	case BindActorSystem:
		if target == TargetDDL {
			return "", fmt.Errorf("a role rule used in a database constraint")
		}
		return gucIsSystem, nil
	}
	return litSQL(b.Lit, b.Cast)
}

// litSQL renders a literal as a SQL constant. Values come from the spec, not from
// request data, and are quoted here, so injection is not reachable (§9.1).
func litSQL(l Lit, cast string) (string, error) {
	var out string
	switch l.Kind {
	case "null":
		out = "null"
	case "bool":
		out = l.Text
	case "int", "decimal":
		if !numRe(l.Text) {
			return "", fmt.Errorf("unsafe numeric literal %q", l.Text)
		}
		out = l.Text
	case "time":
		out = quote(l.Text) + "::timestamptz"
	case "money":
		minor, cur, ok := splitMoney(l.Text)
		if !ok {
			return "", fmt.Errorf("malformed money literal %q", l.Text)
		}
		out = fmt.Sprintf("(%d,%s)::record", minor, quote(cur))
	case "string":
		out = quote(l.Text)
	case "list":
		if len(l.List) == 0 {
			return "", fmt.Errorf("empty value list")
		}
		parts := make([]string, len(l.List))
		for i, v := range l.List {
			s, err := litSQL(v, "")
			if err != nil {
				return "", err
			}
			parts[i] = s
		}
		out = "ARRAY[" + strings.Join(parts, ",") + "]" + elemCast(l.Elem)
	default:
		return "", fmt.Errorf("unknown literal kind %q", l.Kind)
	}
	if cast != "" && l.Kind != "null" {
		out = "(" + out + ")::" + cast
	}
	return out, nil
}

func elemCast(kind string) string {
	switch kind {
	case "int":
		return "::bigint[]"
	case "string", "enum":
		return "::text[]"
	case "time":
		return "::timestamptz[]"
	}
	return ""
}

func quote(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

func numRe(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		switch {
		case r >= '0' && r <= '9':
		case (r == '-' || r == '+') && i == 0:
		case r == '.':
		default:
			return false
		}
	}
	return true
}

func splitMoney(text string) (int64, string, bool) {
	i := strings.IndexByte(text, ' ')
	if i < 0 {
		return 0, "", false
	}
	var minor int64
	for _, r := range text[:i] {
		if r == '-' {
			continue
		}
		if r < '0' || r > '9' {
			return 0, "", false
		}
		minor = minor*10 + int64(r-'0')
	}
	if strings.HasPrefix(text[:i], "-") {
		minor = -minor
	}
	return minor, text[i+1:], true
}

func (c *comp) col(f Field, lit Lit) string {
	name := f.Column
	switch lit.Kind {
	case "int", "decimal":
		if len(f.Columns) == 2 {
			name = f.Columns[0] // money compares on minor units
		}
	case "money":
		if len(f.Columns) == 2 {
			return "(" + f.Columns[0] + "," + f.Columns[1] + ")"
		}
	}
	return rootMarker + "." + name
}

// operand renders one operand position.
func (c *comp) operand(n *Node) string {
	switch n.Op {
	case OpLit:
		return c.add(Bind{Kind: BindLit, Lit: n.Values[0]})
	case OpActorID:
		if !c.allowsActorRefs() {
			return "false"
		}
		return c.add(Bind{Kind: BindActorID})
	case OpActorTenant:
		if !c.allowsActorRefs() {
			return "false"
		}
		return c.add(Bind{Kind: BindActorTenant})
	case OpNow:
		return "now()"
	case OpField:
		return c.path(n.Path)
	}
	// A nested boolean used as a value: compare it to true.
	return "(" + c.expr(n) + ")"
}

// path resolves a field path to SQL, crossing at most one relation (§5.4).
func (c *comp) path(p []string) string {
	if c.t == nil {
		c.fail("no table to resolve %v against", p)
		return "null"
	}
	if len(p) == 1 {
		f, ok := c.t.Lookup(p[0])
		if !ok {
			c.fail("unknown field %q on %s", p[0], c.t.Name)
			return "null"
		}
		if f.IsRef() {
			return rootMarker + "." + f.Column
		}
		return rootMarker + "." + f.Column
	}
	f, ok := c.t.Lookup(p[0])
	if !ok || !f.IsRef() {
		c.fail("%q is not a relation on %s", p[0], c.t.Name)
		return "null"
	}
	g, ok := f.RefTo.Lookup(p[1])
	if !ok {
		c.fail("unknown field %q on %s", p[1], f.RefTo.Name)
		return "null"
	}
	if p[1] == "id" {
		// The foreign key column is the target id, no subquery needed.
		return rootMarker + "." + f.Column
	}
	sub := "ks_sub"
	return fmt.Sprintf("(SELECT %s.%s FROM %s %s WHERE %s.%s = %s.%s)",
		sub, g.ColumnFor(""), f.RefTo.Table, sub, sub, f.RefTo.IDColumn, rootMarker, f.Column)
}

func (c *comp) expr(n *Node) string {
	switch n.Op {
	case OpAlways:
		return "true"
	case OpNever:
		return "false"
	case OpAnd:
		parts := make([]string, len(n.Kids))
		for i, k := range n.Kids {
			parts[i] = "(" + c.expr(k) + ")"
		}
		return strings.Join(parts, " AND ")
	case OpOr:
		parts := make([]string, len(n.Kids))
		for i, k := range n.Kids {
			parts[i] = "(" + c.expr(k) + ")"
		}
		return strings.Join(parts, " OR ")
	case OpNot:
		return "NOT (" + c.expr(n.Kids[0]) + ")"
	case OpImplies:
		return "(NOT (" + c.expr(n.Kids[0]) + ") OR (" + c.expr(n.Kids[1]) + "))"
	case OpIsSet, OpIsNull:
		inner := c.setOperand(n.Kids[0])
		if n.Op == OpIsSet {
			return inner + " IS NOT NULL"
		}
		return inner + " IS NULL"
	case OpRole:
		return c.role(n.Roles)
	case OpOwner:
		return ownerSQL(c, n.Path[0])
	case OpIn:
		left := c.setOperand(n.Kids[0])
		list := n.Kids[1].Values[0]
		if list.Kind != "list" || len(list.List) == 0 {
			c.fail("in: needs a non-empty list")
			return "false"
		}
		if c.target == TargetQuery {
			holes := make([]string, len(list.List))
			for i, v := range list.List {
				c.binds = append(c.binds, Bind{Kind: BindLit, Lit: v})
				holes[i] = "?"
			}
			return left + " IN (" + strings.Join(holes, ",") + ")"
		}
		s, err := litSQL(list, "")
		if err != nil {
			c.fail("%v", err)
			return "false"
		}
		return left + " = ANY(" + s + ")"
	case OpEq, OpNe, OpGt, OpGte, OpLt, OpLte, OpPrefix:
		return c.cmp(n)
	}
	c.fail("cannot compile op %q to SQL", n.Op)
	return "false"
}

// setOperand renders the left-hand side of a predicate: a column, never a bind.
func (c *comp) setOperand(n *Node) string {
	switch n.Op {
	case OpField:
		return c.path(n.Path)
	case OpActorID:
		if !c.allowsActorRefs() {
			return "false"
		}
		return c.add(Bind{Kind: BindActorID})
	case OpActorTenant:
		if !c.allowsActorRefs() {
			return "false"
		}
		return c.add(Bind{Kind: BindActorTenant})
	case OpNow:
		return "now()"
	case OpLit:
		return c.add(Bind{Kind: BindLit, Lit: n.Values[0]})
	}
	return "(" + c.expr(n) + ")"
}

func (c *comp) cmp(n *Node) string {
	if len(n.Kids) != 2 {
		c.fail("%s needs two operands", n.Op)
		return "false"
	}
	a, b := n.Kids[0], n.Kids[1]
	// Put the column on the left when exactly one side is a field.
	if a.Op != OpField && b.Op == OpField {
		a, b = b, a
	}
	lhs := c.setOperand(a)
	if n.Op == OpPrefix {
		rhs := c.add(Bind{Kind: BindLit, Lit: litOf(b), Cast: "text"})
		return fmt.Sprintf("(%s LIKE (%s || '%%'))", lhs, rhs)
	}
	rhsLit := litOf(b)
	rhs := c.add(Bind{Kind: BindLit, Lit: rhsLit})
	// A single-segment field selects its comparison column by literal kind (money
	// compares on minor units). Multi-segment paths already rendered a subselect.
	if a.Op == OpField && len(a.Path) == 1 {
		if f, ok := c.t.Lookup(a.Path[0]); ok {
			lhs = rootMarker + "." + f.ColumnFor(rhsLit.Kind)
		}
	}
	op := map[string]string{
		OpEq: "=", OpNe: "<>", OpGt: ">", OpGte: ">=", OpLt: "<", OpLte: "<=",
	}[n.Op]
	if n.Op == OpNe {
		return fmt.Sprintf("(%s IS DISTINCT FROM %s)", lhs, rhs)
	}
	return fmt.Sprintf("%s %s %s", lhs, op, rhs)
}

func (c *comp) resolveField(p []string) (Field, bool) {
	if c.t == nil {
		return Field{}, false
	}
	if len(p) == 1 {
		return c.t.Lookup(p[0])
	}
	f, ok := c.t.Lookup(p[0])
	if !ok || !f.IsRef() {
		return Field{}, false
	}
	return f.RefTo.Lookup(p[1])
}

func litOf(n *Node) Lit {
	switch n.Op {
	case OpLit:
		return n.Values[0]
	case OpRole:
		out := make([]Lit, len(n.Roles))
		for i, r := range n.Roles {
			out[i] = StringLit(r)
		}
		return ListLit("string", out)
	}
	return NullLit()
}

// role compiles a role test as a membership lookup plus the system shortcut.
// Bind order must match placeholder order, so the shortcut is added first.
func (c *comp) role(names []string) string {
	if c.target == TargetQuery {
		sys := c.add(Bind{Kind: BindActorSystem})
		ten := c.add(Bind{Kind: BindActorTenant})
		act := c.add(Bind{Kind: BindActorID})
		holes := make([]string, len(names))
		for i, n := range names {
			c.binds = append(c.binds, Bind{Kind: BindLit, Lit: StringLit(n)})
			holes[i] = "?"
		}
		return "((" + sys + ") OR EXISTS (SELECT 1 FROM " + membershipTable +
			" m WHERE m.tenant_id = " + ten + " AND m.actor_id = " + act +
			" AND m.role IN (" + strings.Join(holes, ",") + ")))"
	}
	if c.err != nil {
		return "false"
	}
	if !c.allowsActorRefs() {
		return "false"
	}
	arr, err := litSQL(ListLit("string", strLits(names)), "")
	if err != nil {
		c.fail("%v", err)
		return "false"
	}
	return "((" + gucIsSystem + ") OR EXISTS (SELECT 1 FROM " + membershipTable +
		" m WHERE m.tenant_id = " + gucTenantID + " AND m.actor_id = " + gucActorID +
		" AND m.role = ANY(" + arr + ")))"
}

func strLits(names []string) []Lit {
	out := make([]Lit, len(names))
	for i, n := range names {
		out[i] = StringLit(n)
	}
	return out
}

// allowsActorRefs reports whether actor-dependent SQL may be emitted at all.
func (c *comp) allowsActorRefs() bool {
	if c.target == TargetDDL {
		c.fail("a role or actor rule cannot become a database CHECK constraint")
		return false
	}
	return true
}

// OwnerSQL renders owner(<field>).
func ownerSQL(c *comp, field string) string {
	f, ok := c.t.Lookup(field)
	if !ok {
		c.fail("owner(): unknown field %q", field)
		return "false"
	}
	if c.target == TargetDDL {
		c.fail("owner() cannot appear in a CHECK constraint")
		return "false"
	}
	return rootMarker + "." + f.Column + " = " + c.add(Bind{Kind: BindActorID})
}

// Place rewrites ? placeholders into $n parameters starting at offset, appending
// the runtime values for each bind.
func Place(s *SQL, offset int, av ActorValues) (string, []any, error) {
	binds := append([]Bind(nil), s.Binds...)
	out := strings.Builder{}
	args := make([]any, 0, len(binds))
	i, n := 0, offset
	for i < len(s.Text) {
		c := s.Text[i]
		if c != '?' {
			out.WriteByte(c)
			i++
			continue
		}
		if len(binds) == 0 {
			return "", nil, fmt.Errorf("policy: placeholder without a binding")
		}
		b := binds[0]
		binds = binds[1:]
		v, err := b.value(av)
		if err != nil {
			return "", nil, err
		}
		n++
		fmt.Fprintf(&out, "$%d", n)
		args = append(args, v)
		i++
	}
	if len(binds) != 0 {
		return "", nil, fmt.Errorf("policy: %d bindings with no placeholder", len(binds))
	}
	return out.String(), args, nil
}

// ActorValues carries the caller identity into a compiled fragment.
type ActorValues struct {
	ID       []byte
	Tenant   []byte
	IsSystem bool
}

func (b Bind) value(av ActorValues) (any, error) {
	switch b.Kind {
	case BindActorID:
		return b.orNil(av.ID), nil
	case BindActorTenant:
		return b.orNil(av.Tenant), nil
	case BindActorSystem:
		return av.IsSystem, nil
	}
	return litGo(b.Lit)
}

func (b Bind) orNil(v []byte) any {
	if len(v) == 0 {
		return nil
	}
	return v
}

// litGo converts a literal into a Go value suitable as a query parameter.
func litGo(l Lit) (any, error) {
	switch l.Kind {
	case "null":
		return nil, nil
	case "bool":
		return l.Text == "true", nil
	case "int":
		var n int64
		for _, r := range l.Text {
			if r == '-' {
				continue
			}
			if r < '0' || r > '9' {
				return nil, fmt.Errorf("policy: bad int %q", l.Text)
			}
			n = n*10 + int64(r-'0')
		}
		if strings.HasPrefix(l.Text, "-") {
			n = -n
		}
		return n, nil
	case "decimal":
		return l.Text, nil
	case "string":
		return l.Text, nil
	case "time":
		t, err := time.Parse(time.RFC3339Nano, l.Text)
		if err != nil {
			return nil, fmt.Errorf("policy: bad time literal %q", l.Text)
		}
		return t, nil
	case "money":
		minor, cur, ok := splitMoney(l.Text)
		if !ok {
			return nil, fmt.Errorf("policy: bad money %q", l.Text)
		}
		return []any{minor, cur}, nil
	case "list":
		out := make([]any, 0, len(l.List))
		for _, v := range l.List {
			g, err := litGo(v)
			if err != nil {
				return nil, err
			}
			out = append(out, g)
		}
		return out, nil
	}
	return nil, fmt.Errorf("policy: literal kind %q unsupported", l.Kind)
}

// Render produces SQL for DDL or RLS targets: no placeholders, so no arguments.
func (s *SQL) Render() string { return s.Text }

// TenantFilter returns the isolation predicate for a tenant resource (§8.1). It is
// appended to every query and cannot be expressed away inside a rule.
func TenantFilter(target Target, crossTenant bool) *SQL {
	if crossTenant {
		return &SQL{Text: "true"}
	}
	if target == TargetRLS {
		return &SQL{Text: rootMarker + ".tenant_id = " + gucTenantID}
	}
	return &SQL{
		Text:  rootMarker + ".tenant_id = ?",
		Binds: []Bind{{Kind: BindActorTenant}},
	}
}

// RLSPredicate is the row-level security expression used by generated policies:
// the tenant boundary, plus the explicit cross-tenant opt-in for declared actions.
func RLSPredicate() string {
	return "(" + rootMarker + ".tenant_id = " + gucTenantID + " OR " + gucCross + ")"
}

// WithRoot replaces the root marker with a table alias, so the same compiled rule
// serves the main table and correlated subqueries. An empty alias drops the prefix.
func (s *SQL) WithRoot(alias string) *SQL {
	prefix := alias + "."
	if alias == "" {
		prefix = ""
	}
	text := strings.ReplaceAll(s.Text, rootMarker+".", prefix)
	return &SQL{Text: text, Binds: append([]Bind(nil), s.Binds...)}
}

// TableSQL compiles a rule against a table and immediately binds its root alias.
func TableSQL(n *Node, t *Table, target Target, alias string) (*SQL, error) {
	s, err := Compile(n, t, target)
	if err != nil {
		return nil, err
	}
	return s.WithRoot(alias), nil
}
