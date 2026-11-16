package runtime

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/Grab-Connected-Tech/ambit/am"
)

// Aggregates read the same filtered view as Page: tenant, Read rule, the query's
// conditions, and field-read rules on every column they touch. They exist only for
// actions that declared Counts() (AM-DATA-005).

// Count totals the visible rows.
func (b *Query[T]) Count(ctx context.Context) (int64, error) {
	return b.scalar(ctx, "count(*)", "")
}

// Sum totals an integer column over the visible rows; no rows sum to 0.
func (b *Query[T]) Sum(ctx context.Context, col Col[int64]) (int64, error) {
	return b.scalar(ctx, "", col.SQL)
}

// SumMoney totals a money column over the visible rows in one currency. Rows in other
// currencies are filtered out, so a sum never mixes currencies.
func (b *Query[T]) SumMoney(ctx context.Context, col MoneyCol, currency string) (am.Money, error) {
	currency = strings.ToUpper(currency)
	n, err := b.and(col.Currency.Eq(currency)).scalar(ctx, "", col.Minor.SQL)
	return am.MoneyOf(n, currency), err
}

// scalar runs one aggregate: expr verbatim, or the sum of column when expr is empty.
func (b *Query[T]) scalar(ctx context.Context, expr, column string) (int64, error) {
	v := b.v
	sb := &sqlBuilder{a: actorValues(v.db.actor)}
	if expr == "" {
		col, err := v.colExpr(sb, column)
		if err != nil {
			return 0, err
		}
		expr = "coalesce(sum(" + col + "), 0)::bigint"
	}
	where, err := b.filter(sb)
	if err != nil {
		return 0, err
	}
	sql := fmt.Sprintf("SELECT %s FROM %s t WHERE %s", expr, v.t.Table, where)
	var n int64
	if err := v.db.usable(); err != nil {
		return 0, err
	}
	start := time.Now()
	if err := v.db.conn.QueryRow(ctx, sql, sb.params...).Scan(&n); err != nil {
		return 0, translate(err)
	}
	v.db.record(sql, sb.params, 1, start)
	return n, translate(v.db.budgetErr())
}

// and is Where on a copy, so a sum's own filter never sticks to the caller's query.
func (b *Query[T]) and(c Cond) *Query[T] {
	n := *b
	n.conds = append(b.conds[:len(b.conds):len(b.conds)], c)
	return &n
}

// filter renders the Read-filtered WHERE for the query's conditions.
func (b *Query[T]) filter(sb *sqlBuilder) (string, error) {
	var extra []string
	for _, c := range b.conds {
		sql, err := sb.text(c.SQL, c.Args...)
		if err != nil {
			return "", err
		}
		if sql != "" {
			extra = append(extra, sql)
		}
	}
	return b.v.whereWithRead(sb, extra)
}

// colExpr is one column as the actor may see it: a value a FieldRead rule hides reads
// as NULL, so it drops out of a sum and falls into the NULL group instead of leaking.
// An encrypted column is ciphertext in the database and cannot be aggregated.
func (v *View[T]) colExpr(b *sqlBuilder, sql string) (string, error) {
	for i, c := range v.t.Columns {
		if c.SQL != sql {
			continue
		}
		if c.Encrypted {
			return "", fmt.Errorf("runtime: %s.%s is encrypted and cannot be summed or grouped", v.t.Table, c.Column)
		}
		rule := -1
		for j, fr := range v.t.FieldRead {
			if fr.Index == i {
				rule = j
			}
		}
		if rule < 0 {
			return sql, nil
		}
		cond, err := b.frag(v.t.FieldRead[rule].Frag)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("CASE WHEN (%s) THEN %s ELSE NULL END", cond, sql), nil
	}
	return "", fmt.Errorf("runtime: %s is not a column of %s", sql, v.t.Table)
}

// Group is one row of a grouped read: the key, how many visible rows share it, and the
// sum when one was asked for.
type Group[K, V any] struct {
	Key   K
	Count int64
	Sum   V
}

// GroupKey is any column a read can group by: plain, text, enum, optional (nil key for
// NULL) and id columns.
type GroupKey[K any] interface {
	groupKey(dst *K) (sql string, scan any)
}

func (c Col[T]) groupKey(dst *T) (string, any) { return c.SQL, dst }

func (c IDCol) groupKey(dst *am.ID) (string, any) { return c.SQL, idScanner{dst} }

type idScanner struct{ dst *am.ID }

func (s idScanner) Scan(src any) (err error) {
	*s.dst, err = am.ScanID(src)
	return err
}

// Grouped is a query split by one key. It is a function rather than a method because
// a Go method cannot introduce the key's type.
type Grouped[T, K any] struct {
	q   *Query[T]
	key GroupKey[K]
}

// GroupBy groups the query's visible rows by one column. The result is bounded: more
// groups than the configured maximum page size is an error, never a truncated list.
func GroupBy[T, K any](q *Query[T], key GroupKey[K]) *Grouped[T, K] {
	return &Grouped[T, K]{q: q, key: key}
}

// Count is the number of visible rows per key, ordered by key.
func (g *Grouped[T, K]) Count(ctx context.Context) ([]Group[K, int64], error) {
	return g.run(ctx, g.q, "")
}

// Sum totals an integer column per key.
func (g *Grouped[T, K]) Sum(ctx context.Context, col Col[int64]) ([]Group[K, int64], error) {
	return g.run(ctx, g.q, col.SQL)
}

// SumMoney totals a money column per key, in one currency only.
func (g *Grouped[T, K]) SumMoney(ctx context.Context, col MoneyCol, currency string) ([]Group[K, am.Money], error) {
	currency = strings.ToUpper(currency)
	rows, err := g.run(ctx, g.q.and(col.Currency.Eq(currency)), col.Minor.SQL)
	if err != nil {
		return nil, err
	}
	out := make([]Group[K, am.Money], len(rows))
	for i, r := range rows {
		out[i] = Group[K, am.Money]{Key: r.Key, Count: r.Count, Sum: am.MoneyOf(r.Sum, currency)}
	}
	return out, nil
}

func (g *Grouped[T, K]) run(ctx context.Context, q *Query[T], sumColumn string) ([]Group[K, int64], error) {
	v := q.v
	sb := &sqlBuilder{a: actorValues(v.db.actor)}
	var probe K
	keySQL, _ := g.key.groupKey(&probe)
	key, err := v.colExpr(sb, keySQL)
	if err != nil {
		return nil, err
	}
	sum := "0::bigint"
	if sumColumn != "" {
		col, err := v.colExpr(sb, sumColumn)
		if err != nil {
			return nil, err
		}
		sum = "coalesce(sum(" + col + "), 0)::bigint"
	}
	where, err := q.filter(sb)
	if err != nil {
		return nil, err
	}
	limit := v.db.cfg.MaxPageSize
	// Group by position: the key may be a CASE carrying parameters, and Postgres does
	// not match a repeated expression whose placeholders differ.
	sql := fmt.Sprintf("SELECT %s, count(*), %s FROM %s t WHERE %s GROUP BY 1 ORDER BY 1 LIMIT %d",
		key, sum, v.t.Table, where, limit+1)
	rows, err := v.db.query(ctx, sql, sb.params...)
	if err != nil {
		return nil, translate(err)
	}
	defer rows.Close()
	var out []Group[K, int64]
	for rows.Next() {
		var r Group[K, int64]
		_, dst := g.key.groupKey(&r.Key)
		if err := rows.Scan(dst, &r.Count, &r.Sum); err != nil {
			return nil, translate(err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, translate(err)
	}
	if len(out) > limit {
		return nil, am.NewError("too_many_groups", 422,
			fmt.Sprintf("More than %d groups; narrow the query with Where.", limit))
	}
	return out, nil
}
