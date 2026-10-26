// Package policy holds the one expression language shared by access rules and
// invariants (§5.4), plus the three ways it is used: compiled to SQL, evaluated in
// Go, and compared structurally for the behavior diff.
package policy

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Operation names. Kept as plain strings so the app model stays language neutral.
const (
	OpAlways      = "always"
	OpNever       = "never"
	OpRole        = "role"
	OpOwner       = "owner"
	OpField       = "field"
	OpActorID     = "actor.id"
	OpActorTenant = "actor.tenant"
	OpNow         = "now"
	OpLit         = "lit"
	OpEq          = "eq"
	OpNe          = "ne"
	OpGt          = "gt"
	OpGte         = "gte"
	OpLt          = "lt"
	OpLte         = "lte"
	OpIn          = "in"
	OpIsSet       = "isset"
	OpIsNull      = "isnull"
	OpPrefix      = "prefix"
	OpAnd         = "and"
	OpOr          = "or"
	OpNot         = "not"
	OpImplies     = "implies"
)

// Node is one expression-tree node. Nodes are immutable once built.
type Node struct {
	Op     string   `json:"op"`
	Path   []string `json:"path,omitempty"`
	Roles  []string `json:"roles,omitempty"`
	Values []Lit    `json:"values,omitempty"`
	Kids   []*Node  `json:"kids,omitempty"`
	Name   string   `json:"name,omitempty"` // invariant name, when this is a check
}

// Lit is a literal operand. Text holds the canonical rendering so JSON round
// trips never reformat a number or timestamp.
type Lit struct {
	Kind string `json:"kind"`           // null|bool|int|decimal|string|time|list
	Text string `json:"text,omitempty"` // canonical text form
	Elem string `json:"elem,omitempty"` // list element kind
	List []Lit  `json:"list,omitempty"`
}

// Table is the slice of schema a policy compiles against.
type Table struct {
	Module   string
	Name     string
	Table    string
	Tenant   bool
	IDColumn string
	Fields   []Field
}

// Field is a column or relation reachable by name from a table.
type Field struct {
	Name    string
	Column  string   // SQL column; empty for ref fields
	Kind    string   // text|int|decimal|money|bool|time|date|enum|email|url|json|bytes|ref|id
	Columns []string // money expands to {x_minor, x_currency}
	RefTo   *Table   // set for ref fields
}

// IsRef reports a belongs-to relation.
func (f Field) IsRef() bool { return f.RefTo != nil }

// ColumnFor returns the comparison column for a predicate kind.
func (f Field) ColumnFor(litKind string) string {
	if len(f.Columns) == 2 && (litKind == "int" || litKind == "decimal" || litKind == "" || litKind == "money") {
		return f.Columns[0]
	}
	return f.Column
}

// Lookup finds a field by name.
func (t *Table) Lookup(name string) (Field, bool) {
	if t == nil {
		return Field{}, false
	}
	for _, f := range t.Fields {
		if f.Name == name {
			return f, true
		}
	}
	return Field{}, false
}

// ByColumn maps a SQL column to its field.
func (t *Table) ByColumn(col string) (Field, bool) {
	for _, f := range t.Fields {
		if f.Column == col {
			return f, true
		}
		for _, c := range f.Columns {
			if c == col {
				return f, true
			}
		}
	}
	return Field{}, false
}

// ---- constructors used by the spec builders and the compiler ----

// Always matches every row.
func Always() *Node { return &Node{Op: OpAlways} }

// Never matches no row.
func Never() *Node { return &Node{Op: OpNever} }

// Role succeeds when the actor holds any of the named roles in the current tenant.
func Role(names ...string) *Node {
	r := append([]string(nil), names...)
	sort.Strings(r)
	return &Node{Op: OpRole, Roles: r}
}

// Owner succeeds when the named field equals the actor id (§5.4).
func Owner(field string) *Node { return &Node{Op: OpOwner, Path: []string{field}} }

// FieldRef names a field, possibly through one relation ("customer.plan").
func FieldRef(path ...string) *Node { return &Node{Op: OpField, Path: path} }

// ActorID is the acting user's id.
func ActorID() *Node { return &Node{Op: OpActorID} }

// ActorTenant is the acting user's tenant id.
func ActorTenant() *Node { return &Node{Op: OpActorTenant} }

// Now is the transaction timestamp.
func Now() *Node { return &Node{Op: OpNow} }

// Literal wraps a literal value as an operand.
func Literal(l Lit) *Node { return &Node{Op: OpLit, Values: []Lit{l}} }

// Cmp builds a binary comparison.
func Cmp(op string, kids ...*Node) *Node { return &Node{Op: op, Kids: kids} }

// Logic builds and/or/not/implies.
func Logic(op string, kids ...*Node) *Node { return &Node{Op: op, Kids: kids} }

// And conjoins rules.
func And(kids ...*Node) *Node { return Logic(OpAnd, kids...) }

// Or disjoins rules.
func Or(kids ...*Node) *Node { return Logic(OpOr, kids...) }

// Not negates a rule.
func Not(kid *Node) *Node { return Logic(OpNot, kid) }

// Implies builds a -> b.
func Implies(a, b *Node) *Node { return Logic(OpImplies, a, b) }

// Clone deep-copies a tree.
func (n *Node) Clone() *Node {
	if n == nil {
		return nil
	}
	out := &Node{Op: n.Op, Name: n.Name}
	out.Path = append(out.Path, n.Path...)
	out.Roles = append(out.Roles, n.Roles...)
	out.Values = append(out.Values, n.Values...)
	for _, k := range n.Kids {
		out.Kids = append(out.Kids, k.Clone())
	}
	return out
}

// ---- traversal helpers ----

// Walk visits every node in deterministic order.
func (n *Node) Walk(fn func(*Node) bool) {
	if n == nil {
		return
	}
	if !fn(n) {
		return
	}
	for _, k := range n.Kids {
		k.Walk(fn)
	}
}

// UsesActor reports whether the expression depends on the caller, which decides
// whether it can become a database CHECK constraint (§5.5).
func (n *Node) UsesActor() bool {
	found := false
	n.Walk(func(x *Node) bool {
		switch x.Op {
		case OpActorID, OpActorTenant, OpRole, OpOwner:
			found = true
		}
		return true
	})
	return found
}

// CrossesRef reports whether the expression traverses a relation, which moves it
// from a CHECK constraint to a deferred in-transaction check (§5.5).
func (n *Node) CrossesRef() bool {
	found := false
	n.Walk(func(x *Node) bool {
		if x.Op == OpField && len(x.Path) > 1 {
			found = true
		}
		return true
	})
	return found
}

// TableNames lists root tables touched, for cache invalidation and diff hints.
func (n *Node) TableNames() []string {
	seen := map[string]bool{}
	n.Walk(func(x *Node) bool {
		if x.Op == OpField && len(x.Path) > 1 {
			seen[x.Path[0]] = true
		}
		return true
	})
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Valid rejects nodes the other stages cannot handle.
func (n *Node) Valid() error {
	if n == nil {
		return fmt.Errorf("policy: nil node")
	}
	switch n.Op {
	case OpAlways, OpNever, OpActorID, OpActorTenant, OpNow, OpLit:
	case OpRole:
		if len(n.Roles) == 0 {
			return fmt.Errorf("policy: role node has no roles")
		}
	case OpOwner, OpField:
		if len(n.Path) == 0 || len(n.Path) > 2 {
			return fmt.Errorf("policy: path %v must have depth 1 or 2", n.Path)
		}
	case OpEq, OpNe, OpGt, OpGte, OpLt, OpLte, OpPrefix, OpImplies, OpNot, OpIsSet, OpIsNull:
		want := map[string]int{OpImplies: 2, OpNot: 1, OpIsSet: 1, OpIsNull: 1, OpPrefix: 2}
		if w, ok := want[n.Op]; ok {
			if len(n.Kids) != w {
				return fmt.Errorf("policy: %s expects %d operands", n.Op, w)
			}
		} else if len(n.Kids) != 2 {
			return fmt.Errorf("policy: %s expects 2 operands", n.Op)
		}
	case OpIn:
		if len(n.Kids) != 2 || len(n.Kids[1].Values) == 0 {
			return fmt.Errorf("policy: in expects a field and a value list")
		}
	case OpAnd, OpOr:
		if len(n.Kids) == 0 {
			return fmt.Errorf("policy: %s needs operands", n.Op)
		}
	default:
		return fmt.Errorf("policy: unknown op %q", n.Op)
	}
	for _, k := range n.Kids {
		if err := k.Valid(); err != nil {
			return err
		}
	}
	return nil
}

// ---- literals ----

// Interned literal kinds.
func IntLit(v int64) Lit      { return Lit{Kind: "int", Text: strconv.FormatInt(v, 10)} }
func DecimalLit(v string) Lit { return Lit{Kind: "decimal", Text: v} }
func BoolLit(v bool) Lit      { return Lit{Kind: "bool", Text: strconv.FormatBool(v)} }
func StringLit(v string) Lit  { return Lit{Kind: "string", Text: v} }
func TimeLit(v string) Lit    { return Lit{Kind: "time", Text: v} }
func MoneyLit(v string) Lit   { return Lit{Kind: "money", Text: v} }
func NullLit() Lit            { return Lit{Kind: "null"} }
func ListLit(kind string, vs []Lit) Lit {
	return Lit{Kind: "list", Elem: kind, List: vs}
}

// LitFromGo converts a Go scalar into a literal (used by the spec builders).
func LitFromGo(v any) (Lit, error) {
	switch t := v.(type) {
	case nil:
		return NullLit(), nil
	case bool:
		return BoolLit(t), nil
	case int:
		return IntLit(int64(t)), nil
	case int32:
		return IntLit(int64(t)), nil
	case int64:
		return IntLit(t), nil
	case float64:
		return DecimalLit(strconv.FormatFloat(t, 'f', -1, 64)), nil
	case string:
		return StringLit(t), nil
	case []string:
		vs := make([]Lit, len(t))
		for i, s := range t {
			vs[i] = StringLit(s)
		}
		return ListLit("string", vs), nil
	case []int64:
		vs := make([]Lit, len(t))
		for i, s := range t {
			vs[i] = IntLit(s)
		}
		return ListLit("int", vs), nil
	case Lit:
		return t, nil
	case LitPtrAlias:
		return *t, nil
	}
	return Lit{}, fmt.Errorf("policy: literal of unsupported type %T", v)
}

// LitPtrAlias lets the builders pass *Lit for convenience.
type LitPtrAlias = *Lit

// SortList normalizes an In list so structural equality ignores declaration order.
func (l Lit) SortList() Lit {
	if l.Kind != "list" {
		return l
	}
	out := append([]Lit(nil), l.List...)
	sort.Slice(out, func(i, j int) bool { return out[i].Text < out[j].Text })
	l.List = out
	return l
}

// ---- canonical text: the identity used by the diff and the model hash ----

// String renders the tree in a canonical infix form. Two rules that mean the same
// thing always render the same, because the compiler normalizes before storing.
func (n *Node) String() string {
	if n == nil {
		return "false"
	}
	switch n.Op {
	case OpAlways:
		return "true"
	case OpNever:
		return "false"
	case OpActorID:
		return "actor.id"
	case OpActorTenant:
		return "actor.tenant"
	case OpNow:
		return "now()"
	case OpLit:
		return n.Values[0].String()
	case OpField:
		return "F(" + strings.Join(n.Path, ".") + ")"
	case OpOwner:
		return "owner(" + strings.Join(n.Path, ".") + ")"
	case OpRole:
		return "role[" + strings.Join(n.Roles, ",") + "]"
	case OpNot:
		return "!" + n.Kids[0].String()
	case OpIsSet:
		return n.Kids[0].String() + " IS SET"
	case OpIsNull:
		return n.Kids[0].String() + " IS NULL"
	case OpAnd, OpOr:
		parts := make([]string, len(n.Kids))
		for i, k := range n.Kids {
			parts[i] = k.String()
		}
		joiner := " & "
		if n.Op == OpOr {
			joiner = " | "
		}
		return "(" + strings.Join(parts, joiner) + ")"
	case OpImplies:
		return "(" + n.Kids[0].String() + " -> " + n.Kids[1].String() + ")"
	case OpIn:
		return n.Kids[0].String() + " IN " + n.Kids[1].String()
	case OpPrefix:
		return "prefix(" + n.Kids[0].String() + ", " + n.Kids[1].String() + ")"
	default:
		return "(" + n.Kids[0].String() + " " + n.Op + " " + n.Kids[1].String() + ")"
	}
}

func (l Lit) String() string {
	switch l.Kind {
	case "null":
		return "null"
	case "list":
		parts := make([]string, len(l.List))
		for i, v := range l.List {
			parts[i] = v.String()
		}
		return "[" + strings.Join(parts, ",") + "]"
	case "string":
		return strconv.Quote(l.Text)
	}
	return l.Text
}

// Equal compares trees structurally.
func (a *Node) Equal(b *Node) bool { return a.canon() == b.canon() }

func (n *Node) canon() string {
	if n == nil {
		return ""
	}
	clone := n.Clone()
	canonicalize(clone)
	return clone.String()
}

// canonicalize sorts commutative operands and normalizes implications so that
// semantically identical rules compare equal.
func canonicalize(n *Node) {
	if n == nil {
		return
	}
	for _, k := range n.Kids {
		canonicalize(k)
	}
	for i, v := range n.Values {
		n.Values[i] = v.SortList()
	}
	switch n.Op {
	case OpAnd, OpOr:
		var kids []*Node
		seen := map[string]bool{}
		for _, k := range n.Kids {
			// AND with a true clause, or OR with a false clause, is the other clause.
			if (n.Op == OpAnd && k.Op == OpAlways) || (n.Op == OpOr && k.Op == OpNever) {
				continue
			}
			// AND with a false clause, or OR with a true clause, collapses.
			if (n.Op == OpAnd && k.Op == OpNever) || (n.Op == OpOr && k.Op == OpAlways) {
				n.Op, n.Kids = k.Op, nil
				return
			}
			c := k.String()
			if seen[c] {
				continue
			}
			seen[c] = true
			kids = append(kids, k)
		}
		if len(kids) == 1 {
			*n = *kids[0]
			return
		}
		n.Kids = kids
		sort.SliceStable(n.Kids, func(i, j int) bool { return n.Kids[i].String() < n.Kids[j].String() })
	case OpImplies:
		// a -> b  ==  !a | b ; store in the canonical disjunctive form
		a, b := n.Kids[0], n.Kids[1]
		neg := &Node{Op: OpNot, Kids: []*Node{a}}
		canonicalize(neg)
		n.Kids = []*Node{neg, b}
		n.Op = OpOr
		sort.SliceStable(n.Kids, func(i, j int) bool { return n.Kids[i].String() < n.Kids[j].String() })
	case OpRole:
		sort.Strings(n.Roles)
	case OpEq, OpIn:
		// keep field on the left for a stable rendering
		if n.Kids[0].Op == OpLit && n.Kids[1].Op != OpLit {
			n.Kids[0], n.Kids[1] = n.Kids[1], n.Kids[0]
		}
	}
}

// Canonical renders a normalized, stable string; it is what the app model stores
// alongside the tree for hashing and diffing.
func Canonical(n *Node) string { return n.canon() }

// Normalize returns a fresh, canonicalized tree, ready to enter the model.
func Normalize(n *Node) *Node {
	c := n.Clone()
	canonicalize(c)
	return c
}

// MarshalJSON keeps nodes byte-stable.
func (n *Node) MarshalJSON() ([]byte, error) {
	type wire struct {
		Op     string   `json:"op"`
		Path   []string `json:"path,omitempty"`
		Roles  []string `json:"roles,omitempty"`
		Values []Lit    `json:"values,omitempty"`
		Kids   []*Node  `json:"kids,omitempty"`
		Name   string   `json:"name,omitempty"`
	}
	w := wire{Op: n.Op, Path: n.Path, Roles: n.Roles, Values: n.Values, Kids: n.Kids, Name: n.Name}
	if len(w.Path) == 0 {
		w.Path = nil
	}
	return json.Marshal(w)
}
