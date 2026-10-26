package spec

import (
	"fmt"
	"strings"
	"time"

	"keystone.dev/keystone/model"
)

// ResourceDecl is a declared resource. Specs pass the pointer to other builders
// (s.Ref, s.Writes, s.Returns), so a reference is a Go reference: there is no
// string to typo, and moving a resource is a compile error (KS-SPEC-002).
type ResourceDecl struct {
	module, name string
	line         int
	tenant       bool
	account      bool
	fields       []*Field
	indexes      []indexDecl
	invariants   []invariantDecl
	access       []accessDecl
}

// Name returns the declared resource name.
func (d *ResourceDecl) Name() string { return d.name }

// Ref returns the qualified name once the declaring module is known.
func (d *ResourceDecl) Ref() string { return model.Qual(d.module, d.name) }

// Field declares one field of a resource, or one input or output field of an
// action. The same builders serve both, which is what §5.7 promises.
type Field struct {
	name      string
	typ       model.Type
	optional  bool
	def       *model.Value
	unique    bool
	immutable bool
	sensitive bool
	trim      bool
	refTo     *ResourceDecl
	onDelete  string
	copyFrom  *Field
	err       string
}

// Name returns the field name.
func (f *Field) Name() string { return f.name }

// modifier applies a chainable option to a field and returns it for chaining.
func (f *Field) modifier(fn func(*Field)) *Field {
	if f == nil {
		return nil
	}
	fn(f)
	return f
}

// Optional allows the field to be absent.
func (f *Field) Optional() *Field { return f.modifier(func(x *Field) { x.optional = true }) }

// Default sets the stored default.
func (f *Field) Default(v any) *Field {
	return f.modifier(func(x *Field) {
		val, err := valueOf(v)
		if err != nil {
			x.err = fmt.Sprintf("field %q: bad default: %v", x.name, err)
			return
		}
		x.def = val
	})
}

// DefaultNow sets a timestamptz default of now().
func (f *Field) DefaultNow() *Field {
	return f.modifier(func(x *Field) { x.def = &model.Value{Kind: "now"} })
}

// Unique adds a uniqueness constraint.
func (f *Field) Unique() *Field { return f.modifier(func(x *Field) { x.unique = true }) }

// Immutable forbids changes after create (§5.2).
func (f *Field) Immutable() *Field { return f.modifier(func(x *Field) { x.immutable = true }) }

// Sensitive redacts the value from logs, traces and audit diffs (§9.1).
func (f *Field) Sensitive() *Field { return f.modifier(func(x *Field) { x.sensitive = true }) }

// Trim removes surrounding whitespace before validation.
func (f *Field) Trim() *Field { return f.modifier(func(x *Field) { x.trim = true }) }

// MaxLen caps a text-like field.
func (f *Field) MaxLen(n int) *Field {
	return f.modifier(func(x *Field) {
		if n <= 0 {
			x.err = fmt.Sprintf("field %q: MaxLen must be positive", x.name)
			return
		}
		x.typ.MaxLen = &n
	})
}

// MinLen floors a text-like field.
func (f *Field) MinLen(n int) *Field {
	return f.modifier(func(x *Field) {
		if n < 0 {
			x.err = fmt.Sprintf("field %q: MinLen must not be negative", x.name)
			return
		}
		x.typ.MinLen = &n
	})
}

// Pattern constrains a text field to a regular expression.
func (f *Field) Pattern(re string) *Field {
	return f.modifier(func(x *Field) { x.typ.Pattern = re })
}

// Min sets a numeric lower bound.
func (f *Field) Min(v any) *Field {
	return f.modifier(func(x *Field) {
		s, err := numText(v)
		if err != nil {
			x.err = fmt.Sprintf("field %q: Min: %v", x.name, err)
			return
		}
		x.typ.Min = s
	})
}

// Max sets a numeric upper bound.
func (f *Field) Max(v any) *Field {
	return f.modifier(func(x *Field) {
		s, err := numText(v)
		if err != nil {
			x.err = fmt.Sprintf("field %q: Max: %v", x.name, err)
			return
		}
		x.typ.Max = s
	})
}

// Precision sets numeric precision for Decimal.
func (f *Field) Precision(p int) *Field {
	return f.modifier(func(x *Field) { x.typ.Precision = p })
}

// Scale sets numeric scale for Decimal.
func (f *Field) Scale(s int) *Field { return f.modifier(func(x *Field) { x.typ.Scale = s }) }

// Currency fixes a Money field to one currency.
func (f *Field) Currency(code string) *Field {
	return f.modifier(func(x *Field) { x.typ.Currency = strings.ToUpper(code) })
}

// Schemes restricts the URL schemes a field accepts.
func (f *Field) Schemes(s ...string) *Field {
	return f.modifier(func(x *Field) { x.typ.Schemes = append(x.typ.Schemes, s...) })
}

// MaxSize caps a Bytes field in bytes.
func (f *Field) MaxSize(n int) *Field { return f.modifier(func(x *Field) { x.typ.MaxSize = n }) }

// OnDelete sets referential behaviour for Ref (§5.3). The default is restrict.
func (f *Field) OnDelete(action string) *Field {
	return f.modifier(func(x *Field) {
		switch action {
		case "restrict", "cascade", "set_null":
			x.onDelete = action
		default:
			x.err = fmt.Sprintf("field %q: OnDelete must be Restrict, Cascade or SetNull", x.name)
		}
	})
}

// RenameFrom declares that this field was renamed, so the diff sees one change
// instead of a removal plus an addition (§13.2).
func (f *Field) RenameFrom(old string) *Field {
	return f.modifier(func(x *Field) { x.copyFrom = &Field{name: old} })
}

// ---- field builders ----

func newField(name string, kind model.TypeKind) *Field {
	return &Field{name: name, typ: model.Type{Kind: kind}}
}

// Text declares a string field.
func Text(name string) *Field { return newField(name, model.KindText) }

// Int declares a bigint field.
func Int(name string) *Field { return newField(name, model.KindInt) }

// Decimal declares a numeric(p,s) field.
func Decimal(name string) *Field { return newField(name, model.KindDecimal) }

// Money declares an amount in minor units plus a currency code (§5.2).
func Money(name string) *Field {
	f := newField(name, model.KindMoney)
	f.typ.Currency = "USD"
	return f
}

// Bool declares a boolean field.
func Bool(name string) *Field { return newField(name, model.KindBool) }

// Timestamp declares a timestamptz field.
func Timestamp(name string) *Field { return newField(name, model.KindTimestamp) }

// Date declares a calendar date field.
func Date(name string) *Field { return newField(name, model.KindDate) }

// Enum declares a string field with a fixed set of values. Postgres stores text
// plus a CHECK, so adding a value never needs a type migration (§5.2).
func Enum(name string, values ...string) *Field {
	f := newField(name, model.KindEnum)
	if len(values) == 0 {
		f.err = fmt.Sprintf("field %q: an enum needs at least one value", name)
	}
	f.typ.Values = append(f.typ.Values, values...)
	return f
}

// Email declares a lowercased email address.
func Email(name string) *Field { return newField(name, model.KindEmail) }

// URL declares a validated absolute URL.
func URL(name string) *Field { return newField(name, model.KindURL) }

// JSON declares a structured field backed by a generated Go type. The type must
// be declared, never map[string]any, so the client stays typed.
func JSON[T any](name string) *Field {
	f := newField(name, model.KindJSON)
	f.typ.GoType = goTypeName[T]()
	return f
}

// Bytes declares a bytea field.
func Bytes(name string) *Field { return newField(name, model.KindBytes) }

// Encrypted wraps another field in envelope encryption (§9.4). The stored column
// is bytea; the Go type is the inner field's.
func Encrypted(inner *Field) *Field {
	if inner == nil {
		return &Field{name: "encrypted", err: "Encrypted() needs a field to wrap"}
	}
	f := &Field{name: inner.name, typ: model.Type{Kind: inner.typ.Kind, Encrypted: true, Inner: &inner.typ, Values: inner.typ.Values}}
	f.optional, f.def, f.unique, f.immutable, f.sensitive, f.err = inner.optional, inner.def, inner.unique, inner.immutable, true, inner.err
	f.refTo, f.onDelete, f.copyFrom = inner.refTo, inner.onDelete, inner.copyFrom
	return f
}

// Ref declares a belongs-to relation with a foreign key (§5.3).
func Ref(name string, target *ResourceDecl) *Field {
	f := newField(name, model.KindRef)
	if target == nil {
		f.err = fmt.Sprintf("field %q: Ref target is nil (KS-SPEC-002)", name)
		return f
	}
	f.refTo = target
	f.onDelete = "restrict"
	return f
}

// ID declares an identifier input field bound to a resource.
func ID(name string, target *ResourceDecl) *Field {
	f := Ref(name, target)
	f.typ.Kind = model.KindID
	f.refTo = target
	return f
}

// FieldOf reuses a resource field's type in an action input or output (§5.9).
func FieldOf(res *ResourceDecl, field string) *Field {
	if res == nil {
		return &Field{name: field, err: "FieldOf: unknown source resource"}
	}
	for _, f := range res.fields {
		if f.name == field {
			clone := *f
			clone.copyFrom = f
			return &clone
		}
	}
	return &Field{name: field, err: fmt.Sprintf("FieldOf: %s has no field %q", res.name, field)}
}

// Fields lists a resource's fields (§5.2).
func Fields(f ...*Field) ROption {
	return rOptionFunc(func(d *ResourceDecl) { d.fields = append(d.fields, f...) })
}

// Tenant marks a resource as tenant scoped: every query filters on tenant_id.
func Tenant() ROption { return rOptionFunc(func(d *ResourceDecl) { d.tenant = true }) }

// Index declares a composite index (§5.3).
func Index(fields ...string) ROption {
	return rOptionFunc(func(d *ResourceDecl) {
		d.indexes = append(d.indexes, indexDecl{fields: fields})
	})
}

// UniqueIndex declares a unique index.
func UniqueIndex(fields ...string) ROption {
	return rOptionFunc(func(d *ResourceDecl) {
		d.indexes = append(d.indexes, indexDecl{fields: fields, unique: true})
	})
}

// Where narrows the previous index declaration to a partial index (§5.3).
func Where(rule Rule) ROption {
	return rOptionFunc(func(d *ResourceDecl) {
		if len(d.indexes) == 0 {
			specErr("spec: s.Where must follow s.Index or s.UniqueIndex")
			return
		}
		d.indexes[len(d.indexes)-1].where = rule
	})
}

// Invariants groups the rules that must hold for every row (§5.5).
func Invariants(in ...*Invariant) ROption {
	return rOptionFunc(func(d *ResourceDecl) {
		for _, v := range in {
			if v == nil {
				specErr("spec: s.Invariants received a nil Check")
				continue
			}
			d.invariants = append(d.invariants, invariantDecl{name: v.name, rule: v.rule})
		}
	})
}

// Check names one invariant expression.
func Check(name string, rule Rule) *Invariant { return &Invariant{name: name, rule: rule} }

// Invariant is a named row rule.
type Invariant struct {
	name string
	rule Rule
}

// Access declares a resource's read and write policy (§5.6). Omitting it denies
// everything, to every actor.
func Access(rules ...AccessRule) ROption {
	return rOptionFunc(func(d *ResourceDecl) {
		for _, r := range rules {
			d.access = append(d.access, accessDecl{kind: r.kind, field: r.field, rule: r.rule})
		}
	})
}

// AccessRule is one line of an access policy.
type AccessRule struct {
	kind  string
	field string
	rule  Rule
}

func Read(rule Rule) AccessRule        { return AccessRule{kind: "read", rule: rule} }
func Create(rule Rule) AccessRule      { return AccessRule{kind: "create", rule: rule} }
func Update(rule Rule) AccessRule      { return AccessRule{kind: "update", rule: rule} }
func UpdateAfter(rule Rule) AccessRule { return AccessRule{kind: "update_after", rule: rule} }
func Delete(rule Rule) AccessRule      { return AccessRule{kind: "delete", rule: rule} }

// FieldRead omits a column from output and from the projection when the rule fails.
func FieldRead(field string, rule Rule) AccessRule {
	return AccessRule{kind: "field_read", field: field, rule: rule}
}

// FieldWrite refuses a change to one column when the rule fails.
func FieldWrite(field string, rule Rule) AccessRule {
	return AccessRule{kind: "field_write", field: field, rule: rule}
}

type (
	indexDecl struct {
		name   string
		fields []string
		unique bool
		where  Rule
	}
	invariantDecl struct {
		name string
		rule Rule
	}
	accessDecl struct {
		kind  string
		field string
		rule  Rule
	}
)

type rOpt func(*ResourceDecl)

func (f rOpt) applyResource(d *ResourceDecl) { f(d) }

func rOptionFunc(fn func(*ResourceDecl)) ROption { return rOpt(fn) }

// Resource declares a persisted entity backed by one Postgres table.
func Resource(name string, opts ...ROption) *ResourceDecl {
	if !isTypeName(name) {
		specErr("spec: resource name %q must be UpperCamelCase", name)
	}
	d := &ResourceDecl{name: name}
	d.register()
	for _, o := range opts {
		o.applyResource(d)
	}
	return d
}

// Account declares the resource that identities log in as: a tenant-scoped row
// whose id is the actor id used by sessions, memberships and Owner() rules (§4.2).
func Account(name string, opts ...ROption) *ResourceDecl {
	d := Resource(name, opts...)
	d.account = true
	d.tenant = true
	if registry.accountRes != nil {
		specErr("spec: more than one account resource")
	}
	registry.accountRes = d
	return d
}

// TenantResource declares the isolation boundary (§5.1). Exactly one exists.
func TenantResource(name string, opts ...ROption) *ResourceDecl {
	if registry.tenant != nil {
		specErr("spec: more than one tenant resource (%s and %s)", registry.tenant.name, name)
	}
	d := Resource(name, opts...)
	d.tenant = true
	registry.tenant = d
	return d
}

func isTypeName(s string) bool {
	if s == "" || s[0] < 'A' || s[0] > 'Z' {
		return false
	}
	return !strings.ContainsAny(s, " .-")
}

// snake converts a type name into a table name: LineItem -> line_item.
func snake(s string) string {
	var b strings.Builder
	for i, r := range s {
		if r >= 'A' && r <= 'Z' {
			if i > 0 {
				b.WriteByte('_')
			}
			b.WriteRune(r - 'A' + 'a')
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func numText(v any) (string, error) {
	switch t := v.(type) {
	case int:
		return fmt.Sprintf("%d", t), nil
	case int64:
		return fmt.Sprintf("%d", t), nil
	case string:
		return t, nil
	case time.Duration:
		return t.String(), nil
	}
	return "", fmt.Errorf("unsupported bound type %T", v)
}

func valueOf(v any) (*model.Value, error) {
	switch t := v.(type) {
	case nil:
		return nil, nil
	case time.Time:
		return &model.Value{Kind: "time", Text: t.UTC().Format(time.RFC3339Nano)}, nil
	case time.Duration:
		return &model.Value{Kind: "duration", Text: t.String()}, nil
	}
	l, err := policyLiteral(v)
	if err != nil {
		return nil, err
	}
	return &model.Value{Kind: l.Kind, Text: l.Text}, nil
}
