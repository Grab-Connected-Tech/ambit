package runtime

import (
	"strings"

	"github.com/Grab-Connected-Tech/ambit/am"
)

// Col is a typed column descriptor: the generated `gen.Invoices.Status` in
// `gen.Invoices.Status.Eq(gen.InvoiceStatusSent)` (§7.1). It carries the SQL reference
// and nothing else, so a query can be built without touching the database.
type Col[T any] struct {
	SQL string
	// Cast is applied to the placeholder when the Go type and the Postgres type need a
	// nudge, e.g. ::text for an enum or ::uuid for an id.
	Cast string
}

// NewCol declares a descriptor for one column of the row under query.
func NewCol[T any](sql string) Col[T] { return Col[T]{SQL: sql} }

// ColOf is NewCol with a cast, used by the generator for enum and money columns.
func ColOf[T any](sql, cast string) Col[T] { return Col[T]{SQL: sql, Cast: cast} }

func (c Col[T]) ph() string {
	if c.Cast == "" {
		return "?"
	}
	return "?" + c.Cast
}

func (c Col[T]) pred(op string, v T) Cond {
	return Cond{SQL: c.SQL + " " + op + " " + c.ph(), Args: []any{v}}
}

// Eq is "="; for a NULL-able column comparing against the zero value is a bug rather
// than a match, so generated code uses IsNull for that case.
func (c Col[T]) Eq(v T) Cond  { return c.pred("=", v) }
func (c Col[T]) Ne(v T) Cond  { return c.pred("<>", v) }
func (c Col[T]) Gt(v T) Cond  { return c.pred(">", v) }
func (c Col[T]) Gte(v T) Cond { return c.pred(">=", v) }
func (c Col[T]) Lt(v T) Cond  { return c.pred("<", v) }
func (c Col[T]) Lte(v T) Cond { return c.pred("<=", v) }

// In is a set membership test; an empty set matches nothing, which is what a caller who
// filtered everything away means.
func (c Col[T]) In(vs ...T) Cond {
	if len(vs) == 0 {
		return Cond{SQL: "false"}
	}
	parts := make([]string, len(vs))
	args := make([]any, len(vs))
	for i, v := range vs {
		parts[i] = c.ph()
		args[i] = v
	}
	return Cond{SQL: c.SQL + " IN (" + strings.Join(parts, ",") + ")", Args: args}
}

func (c Col[T]) NotIn(vs ...T) Cond {
	if len(vs) == 0 {
		return Always()
	}
	in := c.In(vs...)
	return Cond{SQL: "NOT (" + in.SQL + ")", Args: in.Args}
}

// IsNull tests the column itself, so it takes no argument.
func (c Col[T]) IsNull() Cond { return Cond{SQL: c.SQL + " IS NULL"} }

func (c Col[T]) IsNotNull() Cond { return Cond{SQL: c.SQL + " IS NOT NULL"} }

// Asc and Desc are the ordering half of the descriptor; a page always ends with the id,
// which the query builder appends so a cursor cannot skip or repeat a row (§8.1).
func (c Col[T]) Asc() orderTerm  { return orderTerm{SQL: c.SQL} }
func (c Col[T]) Desc() orderTerm { return orderTerm{SQL: c.SQL, Desc: true} }

// OptCol describes a column whose Go type is am.Opt[T]; Set and Clear are the only ways
// to write it, which is what keeps "absent" different from "empty" (§5.2).
type OptCol[T any] struct{ Col[*T] }

func NewOptCol[T any](sql string) OptCol[T] { return OptCol[T]{Col[*T]{SQL: sql}} }

func (c OptCol[T]) Set(v T) Cond {
	return Cond{SQL: c.SQL + " IS NOT NULL AND " + c.SQL + " = " + c.ph(), Args: []any{&v}}
}
func (c OptCol[T]) Clear() Cond { return c.IsNull() }

// TextCol adds the string-only predicates a generated text column needs.
type TextCol struct{ Col[string] }

func NewTextCol(sql string) TextCol { return TextCol{Col[string]{SQL: sql}} }

// Contains is a case-sensitive substring test. It is a LIKE with the wildcards escaped,
// so a value containing % is searched for literally rather than matching everything.
func (c TextCol) Contains(v string) Cond {
	return Cond{SQL: c.SQL + " LIKE ? ESCAPE E'\\\\'", Args: []any{"%" + escapeLike(v) + "%"}}
}

// HasPrefix is index-friendly where a full-text scan would not be.
func (c TextCol) HasPrefix(v string) Cond {
	return Cond{SQL: c.SQL + " LIKE ? ESCAPE E'\\\\'", Args: []any{escapeLike(v) + "%"}}
}

func escapeLike(v string) string {
	v = strings.ReplaceAll(v, `\`, `\\`)
	v = strings.ReplaceAll(v, "%", `\%`)
	v = strings.ReplaceAll(v, "_", `\_`)
	return v
}

// MoneyCol spans the two columns a am.Money is stored in (§5.2), so an equality test
// compares the pair and cannot match a different currency by accident.
type MoneyCol struct {
	Minor    Col[int64]
	Currency TextCol
}

func NewMoneyCol(minor, currency string) MoneyCol {
	return MoneyCol{Minor: NewCol[int64](minor), Currency: NewTextCol(currency)}
}

func (m MoneyCol) Eq(v am.Money) Cond {
	return AndOf(m.Minor.Eq(v.Minor), m.Currency.Eq(string(v.Currency)))
}

func (m MoneyCol) Gte(v am.Money) Cond {
	return AndOf(m.Currency.Eq(string(v.Currency)), m.Minor.Gte(v.Minor))
}

func (m MoneyCol) Gt(v am.Money) Cond {
	return AndOf(m.Currency.Eq(string(v.Currency)), m.Minor.Gt(v.Minor))
}

func (m MoneyCol) Lt(v am.Money) Cond {
	return AndOf(m.Currency.Eq(string(v.Currency)), m.Minor.Lt(v.Minor))
}

func (m MoneyCol) Lte(v am.Money) Cond {
	return AndOf(m.Currency.Eq(string(v.Currency)), m.Minor.Lte(v.Minor))
}

// IDCol is a reference column: the same operators as a plain column, plus the resource
// it points at, which is what a generated relation loader reads.
type IDCol struct {
	SQL    string
	Target string
}

func NewIDCol(sql, target string) IDCol { return IDCol{SQL: sql, Target: target} }

func (c IDCol) pred(op string, v am.ID) Cond {
	return Cond{SQL: c.SQL + " " + op + " ?", Args: []any{v.Bytes()}}
}

func (c IDCol) Eq(v am.ID) Cond { return c.pred("=", v) }
func (c IDCol) Ne(v am.ID) Cond { return c.pred("<>", v) }
func (c IDCol) IsNull() Cond    { return Cond{SQL: c.SQL + " IS NULL"} }
func (c IDCol) IsNotNull() Cond { return Cond{SQL: c.SQL + " IS NOT NULL"} }
func (c IDCol) Asc() orderTerm  { return orderTerm{SQL: c.SQL} }
func (c IDCol) Desc() orderTerm { return orderTerm{SQL: c.SQL, Desc: true} }

func (c IDCol) In(vs ...am.ID) Cond {
	if len(vs) == 0 {
		return Cond{SQL: "false"}
	}
	parts := make([]string, len(vs))
	args := make([]any, len(vs))
	for i, v := range vs {
		parts[i] = "?"
		args[i] = v.Bytes()
	}
	return Cond{SQL: c.SQL + " IN (" + strings.Join(parts, ",") + ")", Args: args}
}
