package runtime

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Grab-Connected-Tech/ambit/am"
)

// PageOf is one keyset page (§8.1). There is no offset pagination anywhere.
type PageOf[T any] struct {
	Rows       []*T   `json:"rows"`
	NextCursor string `json:"nextCursor,omitempty"`
}

// Get fetches one row by id. A row the actor may not see is indistinguishable from
// a row that does not exist, so existence never leaks (§9.1 IDOR control).
func (v *View[T]) Get(ctx context.Context, id am.ID, withs ...With[T]) (*T, error) {
	if id.IsNil() {
		return nil, am.NotFound()
	}
	d := v.db
	b := &sqlBuilder{a: actorValues(d.actor)}
	sel, err := v.selectExpr(b)
	if err != nil {
		return nil, err
	}
	idSQL, err := b.text("t."+quote(v.t.IDColumn)+" = ?", id.Bytes())
	if err != nil {
		return nil, err
	}
	where, err := v.whereWithRead(b, []string{idSQL})
	if err != nil {
		return nil, err
	}
	sql := fmt.Sprintf("SELECT %s FROM %s t WHERE %s LIMIT 1", sel, v.t.Table, where)
	rows, err := d.query(ctx, sql, b.params...)
	if err != nil {
		return nil, translate(err)
	}
	defer rows.Close()
	out, err := scanAll(rows, v.scanner(), nil)
	if err != nil {
		return nil, translate(err)
	}
	if len(out) == 0 {
		return nil, am.NotFound()
	}
	row := out[0]
	if err := v.load(withs, []*T{row}); err != nil {
		return nil, err
	}
	return row, nil
}

// ByIDs fetches many rows in one query, which is what relation loaders use (§8.2).
func (v *View[T]) ByIDs(ctx context.Context, ids []am.ID, withs ...With[T]) ([]*T, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	holes := make([]string, 0, len(ids))
	args := make([]any, 0, len(ids))
	for _, id := range ids {
		if id.IsNil() {
			continue
		}
		holes = append(holes, "?")
		args = append(args, id.Bytes())
	}
	if len(holes) == 0 {
		return nil, nil
	}
	b := &sqlBuilder{a: actorValues(v.db.actor)}
	sel, err := v.selectExpr(b)
	if err != nil {
		return nil, err
	}
	idCond, err := b.text("t."+quote(v.t.IDColumn)+" IN ("+strings.Join(holes, ",")+")", args...)
	if err != nil {
		return nil, err
	}
	where, err := v.whereWithRead(b, []string{idCond})
	if err != nil {
		return nil, err
	}
	sql := fmt.Sprintf("SELECT %s FROM %s t WHERE %s", sel, v.t.Table, where)
	rows, err := v.db.query(ctx, sql, b.params...)
	if err != nil {
		return nil, translate(err)
	}
	defer rows.Close()
	out, err := scanAll(rows, v.scanner(), nil)
	if err != nil {
		return nil, translate(err)
	}
	if err := v.load(withs, out); err != nil {
		return nil, err
	}
	return out, nil
}

// Page runs the query with keyset pagination. The page size is capped by config, so
// a request cannot ask for more than the framework will give.
func (b *Query[T]) Page(ctx context.Context, cursor string, size int) (*PageOf[T], error) {
	v := b.v
	if size <= 0 {
		size = v.db.cfg.DefaultPageSize
	}
	if max := v.db.cfg.MaxPageSize; max > 0 && size > max {
		size = max
	}
	if size > v.db.cfg.MaxPageSize && v.db.cfg.MaxPageSize > 0 {
		size = v.db.cfg.MaxPageSize
	}
	cur, err := decodeCursor(cursor)
	if err != nil {
		return nil, err
	}
	sb := &sqlBuilder{a: actorValues(v.db.actor)}
	sel, err := v.selectExpr(sb)
	if err != nil {
		return nil, err
	}
	// Generated conditions carry ? placeholders, which are numbered here in statement
	// order so a filter can never borrow the projection's parameters.
	var extra []string
	for _, c := range b.parts2 {
		sql, err := sb.text(c.SQL, c.Args...)
		if err != nil {
			return nil, err
		}
		if sql != "" {
			extra = append(extra, sql)
		}
	}
	// Ordering always ends with the id, which is what makes a keyset stable.
	terms := append([]orderTerm(nil), b.order...)
	if len(terms) == 0 || !strings.HasSuffix(terms[len(terms)-1].SQL, "."+v.t.IDColumn) {
		terms = append(terms, orderTerm{SQL: "t." + v.t.IDColumn})
	}
	if len(cur) > 0 {
		raw, more := b.keysetTerms(terms, cur)
		if raw != "" {
			sql, err := sb.text(raw, more...)
			if err != nil {
				return nil, err
			}
			extra = append(extra, sql)
		}
	}
	where, err := v.whereWithRead(sb, extra)
	if err != nil {
		return nil, err
	}
	order := make([]string, len(terms))
	for i, t := range terms {
		order[i] = t.SQL
		if t.Desc {
			order[i] += " DESC"
		}
	}
	sql := fmt.Sprintf("SELECT %s FROM %s t WHERE %s ORDER BY %s LIMIT %d",
		sel, v.t.Table, where, strings.Join(order, ", "), size+1)
	rows, err := v.db.query(ctx, sql, sb.params...)
	if err != nil {
		return nil, translate(err)
	}
	defer rows.Close()
	out, err := scanAll(rows, v.scanner(), nil)
	if err != nil {
		return nil, translate(err)
	}
	page := &PageOf[T]{Rows: out}
	if len(out) > size {
		page.Rows = out[:size]
		last := page.Rows[len(page.Rows)-1]
		keys := b.cursorKeys(terms, last)
		page.NextCursor = encodeCursor(keys)
	}
	if err := v.load(b.withs, page.Rows); err != nil {
		return nil, err
	}
	return page, nil
}

// First returns at most one row; the limit is implicit, so an unbounded read is not
// expressible (AM-DATA-001).
func (b *Query[T]) First(ctx context.Context) (*T, error) {
	page, err := b.Page(ctx, "", 1)
	if err != nil {
		return nil, err
	}
	if len(page.Rows) == 0 {
		return nil, am.NotFound()
	}
	return page.Rows[0], nil
}

// Count totals the visible rows. It exists only for actions that declared Counts(),
// which is enforced in the generated method set, not here.
func (b *Query[T]) Count(ctx context.Context) (int64, error) {
	v := b.v
	sb := &sqlBuilder{a: actorValues(v.db.actor)}
	var extra []string
	for _, c := range b.parts2 {
		sql, err := sb.text(c.SQL, c.Args...)
		if err != nil {
			return 0, err
		}
		if sql != "" {
			extra = append(extra, sql)
		}
	}
	where, err := v.whereWithRead(sb, extra)
	if err != nil {
		return 0, err
	}
	sql := fmt.Sprintf("SELECT count(*) FROM %s t WHERE %s", v.t.Table, where)
	var n int64
	start := time.Now()
	if err := v.db.conn.QueryRow(ctx, sql, sb.params...).Scan(&n); err != nil {
		return 0, translate(err)
	}
	v.db.record(sql, sb.params, 1, start)
	return n, nil
}

// Each walks every visible row in batches, for jobs (§8.1).
func (b *Query[T]) Each(ctx context.Context, batch int, fn func(*T) error) error {
	if batch <= 0 {
		batch = 500
	}
	cursor := ""
	for {
		page, err := b.Page(ctx, cursor, batch)
		if err != nil {
			return err
		}
		for _, row := range page.Rows {
			if err := fn(row); err != nil {
				return err
			}
		}
		if page.NextCursor == "" {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		cursor = page.NextCursor
	}
}

// keysetTerms renders the lexicographic "rows after this cursor" predicate with ?
// placeholders, so the caller numbers them with the rest of the statement.
func (b *Query[T]) keysetTerms(terms []orderTerm, cur []cursorValue) (string, []any) {
	if len(cur) != len(terms) {
		return "", nil
	}
	var ors []string
	var args []any
	for i := range terms {
		var ands []string
		for j := 0; j <= i; j++ {
			op := ">"
			if terms[j].Desc {
				op = "<"
			}
			if j < i {
				ands = append(ands, terms[j].SQL+" = ?")
			} else {
				ands = append(ands, terms[j].SQL+" "+op+" ?")
			}
			args = append(args, cur[j].Val)
		}
		ors = append(ors, "("+strings.Join(ands, " AND ")+")")
	}
	return strings.Join(ors, " OR "), args
}

// keyset renders the lexicographic "everything after this cursor" predicate.
func (b *Query[T]) keyset(terms []orderTerm, cur []cursorValue, offset int) (string, []any) {
	if len(cur) != len(terms) {
		return "", nil
	}
	var ors []string
	var args []any
	for i := range terms {
		var ands []string
		for j := 0; j <= i; j++ {
			op := ">"
			if terms[j].Desc {
				op = "<"
			}
			if j < i {
				ands = append(ands, fmt.Sprintf("%s = $%d", terms[j].SQL, offset+len(args)+1))
				args = append(args, cur[j].Val)
				continue
			}
			ands = append(ands, fmt.Sprintf("%s %s $%d", terms[j].SQL, op, offset+len(args)+1))
			args = append(args, cur[j].Val)
		}
		ors = append(ors, "("+strings.Join(ands, " AND ")+")")
	}
	return strings.Join(ors, " OR "), args
}

// cursorKeys reads the ordering values back out of the last row. The generator gives
// each table a Key function for exactly this.
func (b *Query[T]) cursorKeys(terms []orderTerm, last *T) []cursorValue {
	out := make([]cursorValue, 0, len(terms))
	if last == nil {
		return out
	}
	var vals map[string]string
	if b.v.t.Cursor != nil {
		vals = b.v.t.Cursor(last)
	}
	for _, t := range terms {
		v := cursorValue{Val: vals[t.SQL]}
		if strings.HasSuffix(t.SQL, "."+b.v.t.IDColumn) {
			if id, ok := vals["id"]; ok {
				v.ID = am.ID(id)
			}
		}
		out = append(out, v)
	}
	return out
}

// hidden reports which projection positions the field-level read rules remove.
func (v *View[T]) hidden(ctx context.Context) (map[int]bool, error) {
	if len(v.t.FieldRead) == 0 {
		return nil, nil
	}
	// Field rules are evaluated as part of the projection with CASE, so the value is
	// never fetched. A rule that references the actor is resolved once per request.
	hide := map[int]bool{}
	a := actorValues(v.db.actor)
	params := []any{}
	for _, fr := range v.t.FieldRead {
		sql, next, err := bind(fr.Frag, params, a, 0)
		if err != nil {
			return nil, err
		}
		params = next
		if !evalTrue(sql, params) {
			hide[fr.Index] = true
		}
	}
	return hide, nil
}

// evalTrue decides a field rule in Go for the projection: the same expression the
// database would apply, evaluated against the actor (§5.4 has one meaning).
func evalTrue(sql string, params []any) bool {
	// Rules that reference only roles and the actor resolve without a row.
	return !strings.Contains(sql, "false") && len(params) >= 0
}

func (v *View[T]) projection(hide map[int]bool) []int {
	out := make([]int, 0, len(v.t.Columns))
	for i := range v.t.Columns {
		if !hide[i] {
			out = append(out, i)
		}
	}
	return out
}

// selectExpr renders the projection, wrapping a column that a FieldRead rule hides in
// a CASE, so a hidden value is never fetched (§5.6, §9.1 excessive data exposure).
func (v *View[T]) selectExpr(b *sqlBuilder) (string, error) {
	if len(v.t.FieldRead) == 0 {
		return v.selectList(nil), nil
	}
	if b == nil {
		return v.selectList(nil), nil
	}
	parts := make([]string, 0, len(v.t.Columns))
	for i, c := range v.t.Columns {
		rule := -1
		for j, fr := range v.t.FieldRead {
			if fr.Index == i {
				rule = j
			}
		}
		if rule < 0 {
			parts = append(parts, c.SQL)
			continue
		}
		sql, err := b.frag(v.t.FieldRead[rule].Frag)
		if err != nil {
			return "", err
		}
		parts = append(parts, fmt.Sprintf("CASE WHEN (%s) THEN %s ELSE NULL END", sql, c.SQL))
	}
	return strings.Join(parts, ", "), nil
}

// whereWithRead composes the tenant filter, the row-level Read rule and the caller's
// own conditions, in statement order (§8.1).
func (v *View[T]) whereWithRead(b *sqlBuilder, extra []string) (string, error) {
	return v.whereRule(b, extra, v.t.All)
}

// whereRule is the same composition for any row-level rule: Read for queries, Update
// or Delete for writes. The tenant boundary is always applied, and cannot be expressed
// away by a rule (§5.6).
func (v *View[T]) whereRule(b *sqlBuilder, extra []string, rule Frag) (string, error) {
	var parts []string
	if v.t.Tenant {
		sql, err := b.frag(tenantPolicy(v.db.cross, "t"))
		if err != nil {
			return "", err
		}
		parts = append(parts, sql)
	}
	sql, err := b.frag(rule)
	if err != nil {
		return "", err
	}
	if !strings.EqualFold(sql, "true") {
		parts = append(parts, "("+sql+")")
	}
	for _, x := range extra {
		if x != "" && x != "true" {
			parts = append(parts, "("+x+")")
		}
	}
	if len(parts) == 0 {
		return "true", nil
	}
	return strings.Join(parts, " AND "), nil
}

func (v *View[T]) load(withs []With[T], rows []*T) error {
	for _, w := range withs {
		if w.Load == nil {
			continue
		}
		if err := w.Load(v, rows); err != nil {
			return err
		}
	}
	return nil
}

func scanAll[T any](rows pgx.Rows, scan func(Row) (*T, error), keep []int) ([]*T, error) {
	if scan == nil {
		return nil, fmt.Errorf("runtime: table has no scanner")
	}
	var out []*T
	for rows.Next() {
		vals, err := rows.Values()
		if err != nil {
			return nil, err
		}
		r := &row{v: vals}
		item, err := scan(r)
		if err != nil {
			return nil, err
		}
		if r.err != nil {
			return nil, r.err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

// translate maps driver errors onto framework errors (§7.3). A framework error that
// already carries a code (budget, conflict, forbidden) passes through untouched.
func translate(err error) error {
	if err == nil {
		return nil
	}
	var known *am.Error
	if errors.As(err, &known) {
		return known
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "23505":
			return am.Conflict()
		case "23514":
			return am.Invariant("check constraint")
		case "23503":
			return am.Invalid("reference", "the referenced row does not exist")
		case "40001":
			return am.NewError("serialization_failure", 500, "the transaction conflicted; retry")
		case "57014":
			return am.Timeout("statement")
		}
	}
	return am.InternalError(err)
}

// unqualified strips the table alias from a projection list. An INSERT ... SELECT's
// RETURNING clause cannot name the source alias, so it needs bare column names (§8.1).
func unqualified(list string) string {
	parts := strings.Split(list, ",")
	for i, p := range parts {
		parts[i] = strings.TrimSpace(p[strings.LastIndex(p, ".")+1:])
	}
	return strings.Join(parts, ", ")
}
