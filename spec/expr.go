package spec

import (
	"reflect"
	"sort"
	"strings"
	"time"

	"keystone.dev/keystone/policy"
)

// Rule is anything usable as a predicate: an expression or a role value (§5.4).
type Rule interface {
	Node() *policy.Node
}

// Expr is an expression under construction. Every method returns a new node, so
// a shared sub-expression can never be mutated by a second use.
type Expr struct {
	n *policy.Node
}

// Node implements Rule.
func (e *Expr) Node() *policy.Node {
	if e == nil {
		return policy.Never()
	}
	return e.n
}

// F names a field, optionally through one relation: F("customer.plan").
func F(path string) *Expr {
	parts := strings.Split(path, ".")
	if len(parts) > 2 {
		return &Expr{n: policy.Never()}
	}
	return &Expr{n: policy.FieldRef(parts...)}
}

// Actor exposes the caller-side operands (§5.4).
var Actor = actorNS{}

type actorNS struct{}

func (actorNS) ID() *Expr                 { return &Expr{n: policy.ActorID()} }
func (actorNS) Tenant() *Expr             { return &Expr{n: policy.ActorTenant()} }
func (actorNS) HasRole(r *RoleDecl) *Expr { return &Expr{n: policy.Role(r.name)} }

// Owner succeeds when the field equals the actor's id.
func Owner(field string) *Expr { return &Expr{n: policy.Owner(field)} }

// Always and Never are the two constants the diff reads most often.
func Always() *Expr { return &Expr{n: policy.Always()} }
func Never() *Expr  { return &Expr{n: policy.Never()} }

// Now is the transaction timestamp.
func Now() *Expr { return &Expr{n: policy.Now()} }

// Lit wraps a Go scalar as an operand.
func Lit(v any) *Expr {
	l, err := policyLiteral(v)
	if err != nil {
		return &Expr{n: policy.Never()}
	}
	return &Expr{n: policy.Literal(l)}
}

func (e *Expr) bin(op string, other any) *Expr {
	return &Expr{n: policy.Cmp(op, e.Node(), toNode(other))}
}

// Eq, Ne and the ordering comparisons mirror the SQL they compile to.
func (e *Expr) Eq(v any) *Expr        { return e.bin(policy.OpEq, v) }
func (e *Expr) Ne(v any) *Expr        { return e.bin(policy.OpNe, v) }
func (e *Expr) Gt(v any) *Expr        { return e.bin(policy.OpGt, v) }
func (e *Expr) Gte(v any) *Expr       { return e.bin(policy.OpGte, v) }
func (e *Expr) Lt(v any) *Expr        { return e.bin(policy.OpLt, v) }
func (e *Expr) Lte(v any) *Expr       { return e.bin(policy.OpLte, v) }
func (e *Expr) HasPrefix(v any) *Expr { return e.bin(policy.OpPrefix, v) }

// In tests membership of a value list.
func (e *Expr) In(vs ...any) *Expr {
	kinds := map[string]bool{}
	out := make([]policy.Lit, 0, len(vs))
	for _, v := range vs {
		l, err := policyLiteral(v)
		if err != nil {
			return &Expr{n: policy.Never()}
		}
		kinds[l.Kind] = true
		out = append(out, l)
	}
	if len(kinds) > 1 {
		return &Expr{n: policy.Never()}
	}
	kind := "string"
	for k := range kinds {
		kind = k
	}
	list := policy.Lit{Kind: "list", Elem: kind, List: out}
	return &Expr{n: policy.Cmp(policy.OpIn, e.Node(), policy.Literal(list))}
}

// IsSet reports a non-null value.
func (e *Expr) IsSet() *Expr {
	return &Expr{n: &policy.Node{Op: policy.OpIsSet, Kids: []*policy.Node{e.Node()}}}
}

// IsNull reports a null value.
func (e *Expr) IsNull() *Expr {
	return &Expr{n: &policy.Node{Op: policy.OpIsNull, Kids: []*policy.Node{e.Node()}}}
}

// And, Or, Not and Implies combine rules (§5.4).
func (e *Expr) And(rs ...Rule) *Expr {
	kids := append([]Rule{e}, rs...)
	return &Expr{n: policy.Logic(policy.OpAnd, nodes(kids)...)}
}

func (e *Expr) Or(rs ...Rule) *Expr {
	kids := append([]Rule{e}, rs...)
	return &Expr{n: policy.Logic(policy.OpOr, nodes(kids)...)}
}

func (e *Expr) Not() *Expr { return &Expr{n: policy.Logic(policy.OpNot, e.Node())} }

func (e *Expr) Implies(r Rule) *Expr {
	return &Expr{n: policy.Logic(policy.OpImplies, e.Node(), r.Node())}
}

// And conjoins rules.
func And(rs ...Rule) *Expr { return &Expr{n: policy.Logic(policy.OpAnd, nodes(rs)...)} }

// Or disjoins rules.
func Or(rs ...Rule) *Expr { return &Expr{n: policy.Logic(policy.OpOr, nodes(rs)...)} }

// Not negates a rule.
func Not(r Rule) *Expr { return &Expr{n: policy.Logic(policy.OpNot, r.Node())} }

// Implies builds a -> b.
func Implies(a, b Rule) *Expr { return &Expr{n: policy.Logic(policy.OpImplies, a.Node(), b.Node())} }

func nodes(rs []Rule) []*policy.Node {
	out := make([]*policy.Node, 0, len(rs))
	for _, r := range rs {
		if r == nil {
			continue
		}
		out = append(out, r.Node())
	}
	return out
}

// toNode accepts a Rule, a Go scalar or a *policy.Node in an operand position.
func toNode(v any) *policy.Node {
	switch t := v.(type) {
	case nil:
		return policy.Literal(policy.NullLit())
	case *policy.Node:
		return t
	case Rule:
		return t.Node()
	case time.Time:
		return policy.Literal(policy.TimeLit(t.UTC().Format(time.RFC3339Nano)))
	case []string:
		return policy.Literal(policy.ListLit("string", stringsLits(t)))
	}
	l, err := policyLiteral(v)
	if err != nil {
		return policy.Never()
	}
	return policy.Literal(l)
}

func stringsLits(in []string) []policy.Lit {
	out := make([]policy.Lit, len(in))
	for i, s := range in {
		out[i] = policy.StringLit(s)
	}
	return out
}

func policyLiteral(v any) (policy.Lit, error) { return policy.LitFromGo(v) }

// RoleDecl is both a declaration and a rule: Admin means "the actor holds admin in
// the current tenant" (§5.1).
type RoleDecl struct {
	name  string
	line  int
	filep string
}

// Role declares a role value usable anywhere a rule is accepted.
func Role(name string) *RoleDecl {
	if !isRoleName(name) {
		specErr("spec: role name %q must be lowercase", name)
	}
	if prev, ok := registry.roles[name]; ok {
		specErr("spec: role %q already declared (line %d)", name, prev.line)
		return prev
	}
	r := &RoleDecl{name: name, line: callerLine()}
	registry.roles[name] = r
	return r
}

func isRoleName(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_' || r == '-') {
			return false
		}
	}
	return true
}

// Name returns the role's identifier.
func (r *RoleDecl) Name() string { return r.name }

// Node implements Rule.
func (r *RoleDecl) Node() *policy.Node { return policy.Role(r.name) }

// Or combines a role with other rules, the commonest shape in real policies.
func (r *RoleDecl) Or(rs ...Rule) *Expr {
	kids := append([]Rule{r}, rs...)
	return &Expr{n: policy.Logic(policy.OpOr, nodes(kids)...)}
}

// And combines a role with other rules.
func (r *RoleDecl) And(rs ...Rule) *Expr {
	kids := append([]Rule{r}, rs...)
	return &Expr{n: policy.Logic(policy.OpAnd, nodes(kids)...)}
}

// Not negates a role test.
func (r *RoleDecl) Not() *Expr { return &Expr{n: policy.Logic(policy.OpNot, r.Node())} }

// goTypeName resolves the Go type name for s.JSON[T] without touching the hot path.
func goTypeName[T any]() string {
	t := reflect.TypeOf((*T)(nil)).Elem()
	if t == nil {
		return "any"
	}
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.PkgPath() == "" {
		return t.Name()
	}
	pkg := t.PkgPath()
	if i := strings.LastIndex(pkg, "/"); i >= 0 {
		pkg = pkg[i+1:]
	}
	return pkg + "." + t.Name()
}

// dedupSorted keeps model collections canonical.
func dedupSorted(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	j := 0
	for i, v := range out {
		if i > 0 && out[i] == out[j-1] {
			continue
		}
		out[j] = v
		j++
	}
	return out[:j]
}
