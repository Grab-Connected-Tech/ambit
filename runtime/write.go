package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Grab-Connected-Tech/ambit/am"
)

// Assignment is one column write produced from a generated Patch type. A patch field
// that is absent writes nothing, which is what keeps an update from guessing (§5.2).
type Assignment struct {
	Field  string // spec name: "status"
	Column string // SQL column: "status"
	SQL    string // "t.status = ?" with the column already qualified
	Args   []any
	// Default marks a value the framework supplied, not the action.
	Default bool
}

// Insert writes a new row. The Create rule is part of the statement, so a row that is
// not allowed never exists, and the tenant id comes from the actor, never from input
// (§9.1 mass assignment).
func (v *View[T]) Insert(ctx context.Context, sets []Assignment, withs ...With[T]) (*T, error) {
	d := v.db
	if v.t.Create.Denies() {
		return nil, am.Forbidden("the resource declares no Create rule, so nothing may be inserted")
	}
	cols, vals, args := v.insertValues(sets)
	sb := &sqlBuilder{params: args, a: actorValues(d.actor)}
	where, err := v.whereRule(sb, nil, v.t.Create)
	if err != nil {
		return nil, err
	}
	// The candidate row is presented as a one-row table named t, so the same compiled
	// rule text serves reads and writes, and the framework-supplied columns are never
	// taken from input (§9.1 mass assignment).
	sql := fmt.Sprintf("INSERT INTO %s (%s) SELECT t.* FROM (VALUES %s) AS t(%s) WHERE %s RETURNING %s",
		quote(v.t.Table), strings.Join(cols, ","), "("+strings.Join(vals, ",")+")", strings.Join(stripAll(cols), ","),
		where, unqualified(v.selectList(nil)))
	rows, err := d.query(ctx, sql, sb.params...)
	if err != nil {
		return nil, translate(err)
	}
	defer rows.Close()
	out, err := scanAll(rows, v.scanner(), nil)
	if err != nil {
		return nil, translate(err)
	}
	if len(out) == 0 {
		return nil, am.Forbidden("the Create rule refused the row")
	}
	created := out[0]
	if err := v.checkDeferred(ctx, created); err != nil {
		return nil, err
	}
	d.recordAudit("insert", v.t.Ref, v.idOf(created), nil, created)
	if err := v.load(withs, []*T{created}); err != nil {
		return nil, err
	}
	return created, nil
}

// Update applies a patch under optimistic concurrency (§8.1). A stale version is a
// 409, an invisible row a 404 and a refused rule a 403, in that order of discovery.
func (v *View[T]) Update(ctx context.Context, id am.ID, version am.Version, sets []Assignment, withs ...With[T]) (*T, error) {
	if len(sets) == 0 {
		return v.Get(ctx, id, withs...)
	}
	d := v.db
	before, err := v.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	sb := &sqlBuilder{a: actorValues(d.actor)}
	var set []string
	changed := make([]string, 0, len(sets))
	for _, s := range sets {
		if s.Column == "version" || s.Column == "id" || s.Column == "tenant_id" {
			continue
		}
		p, err := sb.text("?", v.seal(s.Column, id, s.arg()))
		if err != nil {
			return nil, err
		}
		set = append(set, quote(s.Column)+" = "+p+v.castFor(s.Column))
		changed = append(changed, s.Field)
	}
	if len(set) == 0 {
		return before, nil
	}
	// id and version conditions come after the row-level rule in the statement, and
	// field-level write rules come last, so numbering follows text order.
	idSQL, err := sb.text("t."+quote(v.t.IDColumn)+" = ?", id.Bytes())
	if err != nil {
		return nil, err
	}
	verSQL, err := sb.text("t.version = ?", int64(version))
	if err != nil {
		return nil, err
	}
	extra := []string{idSQL, verSQL}
	var fw []Frag
	for _, s := range sets {
		if f := v.fieldWriteFrag(s.Column); f != nil {
			fw = append(fw, *f)
		}
	}
	where, err := v.whereRule(sb, extra, v.t.Update)
	if err != nil {
		return nil, err
	}
	for _, f := range fw {
		sql, err := sb.frag(f)
		if err != nil {
			return nil, err
		}
		where += " AND (" + sql + ")"
	}
	sql := fmt.Sprintf("UPDATE %s t SET %s, version = t.version + 1, updated_at = now() WHERE %s RETURNING %s",
		quote(v.t.Table), strings.Join(set, ", "), where, unqualified(v.selectList(nil)))
	rows, err := d.query(ctx, sql, sb.params...)
	if err != nil {
		return nil, translate(err)
	}
	defer rows.Close()
	out, err := scanAll(rows, v.scanner(), nil)
	if err != nil {
		return nil, translate(err)
	}
	if len(out) == 0 {
		if v.versionOf(before) != int64(version) {
			return nil, am.Conflict()
		}
		return nil, am.Forbidden("a write rule refused the change")
	}
	after := out[0]
	if err := v.checkDeferred(ctx, after); err != nil {
		return nil, err
	}
	d.recordAudit("update", v.t.Ref, id, before, after)
	d.markChanged(changed)
	if err := v.load(withs, []*T{after}); err != nil {
		return nil, err
	}
	return after, nil
}

// Delete removes one row under optimistic concurrency. The method only exists on
// resources whose Delete rule is not Never(), because the generator omits it (§8.1).
func (v *View[T]) Delete(ctx context.Context, id am.ID, version am.Version) error {
	d := v.db
	before, err := v.Get(ctx, id)
	if err != nil {
		return err
	}
	if v.t.Delete.Denies() {
		return am.Forbidden("the resource declares Delete(Never())")
	}
	sb := &sqlBuilder{a: actorValues(d.actor)}
	idSQL, err := sb.text("t."+quote(v.t.IDColumn)+" = ?", id.Bytes())
	if err != nil {
		return err
	}
	verSQL, err := sb.text("t.version = ?", int64(version))
	if err != nil {
		return err
	}
	where, err := v.whereRule(sb, []string{idSQL, verSQL}, v.t.Delete)
	if err != nil {
		return err
	}
	sql := fmt.Sprintf("DELETE FROM %s t WHERE %s", quote(v.t.Table), where)
	tag, err := d.exec(ctx, sql, sb.params...)
	if err != nil {
		return translate(err)
	}
	if tag.RowsAffected() == 0 {
		if v.versionOf(before) != int64(version) {
			return am.Conflict()
		}
		return am.Forbidden("a delete rule refused the row")
	}
	d.recordAudit("delete", v.t.Ref, id, before, nil)
	return nil
}

// whereWith composes the tenant filter, one row-level rule and extra predicates.
func (v *View[T]) whereWith(extra []string, args []any, rule Frag) (string, []any, error) {
	d := v.db
	a := actorValues(d.actor)
	var parts []string
	if v.t.Tenant {
		f := tenantPolicy(v.db.cross, "t")
		sql, next, err := bind(f, args, a, 0)
		if err != nil {
			return "", args, err
		}
		args = next
		parts = append(parts, sql)
	}
	if !rule.Empty() {
		sql, next, err := bind(rule, args, a, 0)
		if err != nil {
			return "", args, err
		}
		args = next
		parts = append(parts, "("+sql+")")
	}
	for _, x := range extra {
		if x != "" && x != "true" {
			parts = append(parts, "("+x+")")
		}
	}
	if len(parts) == 0 {
		return "true", args, nil
	}
	return strings.Join(parts, " AND "), args, nil
}

// checkDeferred runs the row rules that cannot be CHECK constraints: those crossing a
// relation, and UpdateAfter rules, which need the row as it now is (§5.5, §5.6).
func (v *View[T]) checkDeferred(ctx context.Context, row *T) error {
	id := v.idOf(row)
	if id.IsNil() {
		return nil
	}
	for _, c := range v.t.Checks {
		sb := &sqlBuilder{a: actorValues(v.db.actor)}
		idSQL, err := sb.text("t."+quote(v.t.IDColumn)+" = ?", id.Bytes())
		if err != nil {
			return err
		}
		frag := Frag{SQL: "NOT (" + c.Frag.SQL + ")", Binds: c.Frag.Binds}
		where, err := v.whereRule(sb, []string{idSQL}, frag)
		if err != nil {
			return err
		}
		sql := fmt.Sprintf("SELECT 1 FROM %s t WHERE %s LIMIT 1", quote(v.t.Table), where)
		rows, err := v.db.query(ctx, sql, sb.params...)
		if err != nil {
			return translate(err)
		}
		found := rows.Next()
		rows.Close()
		if err := rows.Err(); err != nil {
			return translate(err)
		}
		if found {
			if c.Forbidden {
				return am.Forbidden("the value after the change is not allowed")
			}
			return am.Invariant(c.Name)
		}
	}
	return nil
}

// fieldWriteFrag returns the write rule attached to a column, if any (§5.6).
func (v *View[T]) fieldWriteFrag(column string) *Frag {
	for i, f := range v.t.FieldWrite {
		if f.Column == column {
			return &v.t.FieldWrite[i].Frag
		}
	}
	return nil
}

// idOf reads a row's primary key through the generated accessor.
func (v *View[T]) idOf(row *T) am.ID {
	if v.t.IDOf == nil || row == nil {
		return am.NilID
	}
	return v.t.IDOf(row)
}

// versionOf reads a row's concurrency counter.
func (v *View[T]) versionOf(row *T) int64 {
	if v.t.VersionOf == nil || row == nil {
		return 0
	}
	return v.t.VersionOf(row)
}

// arg returns the value an assignment carries, collapsing the one-argument case that
// generated code produces for every scalar field.
func (a Assignment) arg() any {
	if len(a.Args) == 1 {
		return a.Args[0]
	}
	return a.Args
}

// Args0 returns the single value an assignment carries.
func (a Assignment) Args0() any {
	if len(a.Args) == 1 {
		return a.Args[0]
	}
	if len(a.Args) == 0 {
		return nil
	}
	return a.Args
}

// insertValues lists the columns, value expressions and parameters of a new row. The
// framework owns the bookkeeping columns: id is a fresh UUIDv7, tenant_id comes from
// the actor and never from input, and version starts at 1 (§5.2, §9.1).
func (v *View[T]) insertValues(sets []Assignment) ([]string, []string, []any) {
	var cols, exprs []string
	var args []any
	add := func(col, cast string, val any) {
		args = append(args, val)
		cols = append(cols, quote(col))
		exprs = append(exprs, fmt.Sprintf("$%d%s", len(args), cast))
	}
	addLit := func(col, expr string) {
		cols = append(cols, quote(col))
		exprs = append(exprs, expr)
	}
	id := am.NewID()
	add("id", "::uuid", id.Bytes())
	for _, s := range sets {
		switch s.Column {
		case "id", "version", "created_at", "updated_at", "tenant_id":
			continue
		}
		add(s.Column, v.columnCast(s.Column), v.seal(s.Column, id, s.arg()))
	}
	if v.t.Tenant {
		add("tenant_id", "::uuid", v.db.actor.Tenant.Bytes())
	}
	addLit("created_at", "now()")
	addLit("updated_at", "now()")
	addLit("version", "1")
	return cols, exprs, args
}

// columnCast names the type a placeholder needs, because a bare NULL in a VALUES list
// has no type of its own.
func (v *View[T]) columnCast(column string) string {
	t := v.columnType(column)
	if t == "" || t == "text" {
		return ""
	}
	return "::" + t
}

// hasCol reports whether an assignment already names a column, so the framework never
// writes id or tenant_id twice.
func hasCol(cols []string, want string) bool {
	for _, c := range cols {
		if strings.Trim(c, `"`) == want {
			return true
		}
	}
	return false
}

func quote(s string) string {
	if s == "" || strings.ContainsAny(s, `"; `) {
		return `""`
	}
	return `"` + s + `"`
}

func stripAll(in []string) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = strings.Trim(s, `"`)
	}
	return out
}

// columnType names a column's Postgres type for the casts a VALUES row constructor
// needs, since an untyped NULL there has no type.
func (v *View[T]) columnType(column string) string {
	for _, c := range v.t.Columns {
		if c.Column == column {
			if c.SQLType != "" {
				return c.SQLType
			}
			return "text"
		}
	}
	return "text"
}

func (v *View[T]) castFor(column string) string {
	t := v.columnType(column)
	if t == "" || t == "text" {
		return ""
	}
	return "::" + t
}

// recordAudit appends the change to the in-transaction audit buffer (§9.5). It is
// written in the same transaction, so an audit row cannot disagree with the data.
func (d *DB) recordAudit(op, resource string, id am.ID, before, after any) {
	rec := AuditRecord{
		Tenant: d.actor.Tenant, Actor: d.actor, Action: d.action, Resource: resource,
		RowID: id, Trace: d.traceID, At: d.clock.Now(),
	}
	if before != nil {
		rec.Before = redact(d, encodeRow(before))
	}
	if after != nil {
		rec.After = redact(d, encodeRow(after))
	}
	rec.Changed = diffKeys(rec.Before, rec.After)
	d.Audit = append(d.Audit, rec)
}

func (d *DB) markChanged(names []string) {
	if len(d.Audit) == 0 {
		return
	}
	d.Audit[len(d.Audit)-1].Changed = append(d.Audit[len(d.Audit)-1].Changed, names...)
}

func encodeRow(v any) json.RawMessage {
	switch t := v.(type) {
	case json.RawMessage:
		return t
	}
	if enc, ok := v.(interface{ JSONBytes() []byte }); ok {
		return enc.JSONBytes()
	}
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage("{}")
	}
	return b
}

// redact removes Sensitive fields from an audit diff (§9.4).
func redact(d *DB, raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	body, err := am.ParseObject(raw)
	if err != nil {
		return raw
	}
	w := am.NewW(len(raw))
	w.ObjStart()
	for _, k := range body.Keys() {
		if isSensitiveKey(d, k) {
			continue
		}
		w.Key(k)
		w.Val(body.Get(k))
	}
	w.ObjEnd()
	return w.Bytes()
}

func isSensitiveKey(d *DB, key string) bool {
	if d.app == nil {
		return false
	}
	for _, r := range d.app.AllResources() {
		for _, f := range r.Resource.Fields {
			if f.Sensitive && (f.Name == key || f.GoName == key) {
				return true
			}
		}
	}
	return false
}

func diffKeys(before, after json.RawMessage) []string {
	b, err1 := am.ParseObject(before)
	a, err2 := am.ParseObject(after)
	if err1 != nil || err2 != nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, k := range a.Keys() {
		seen[k] = true
		x, y := b.Get(k), a.Get(k)
		if !sameVal(x, y) {
			out = append(out, k)
		}
	}
	for _, k := range b.Keys() {
		if !seen[k] {
			out = append(out, k)
		}
	}
	return out
}

func sameVal(a, b *am.Val) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return valText(a) == valText(b)
}

func valText(v *am.Val) string {
	w := am.NewW(16)
	w.Val(v)
	return string(w.Bytes())
}

// flushAudit writes buffered audit rows. Grants deny update and delete on the table,
// which is what makes the log append-only (§9.5).
func (a *App) flushAudit(ctx context.Context, d *DB) error {
	for _, rec := range d.Audit {
		before := []byte(nil)
		after := []byte(nil)
		if len(rec.Before) > 0 {
			before = rec.Before
		}
		if len(rec.After) > 0 {
			after = rec.After
		}
		if _, err := d.exec(ctx,
			`INSERT INTO am_audit (tenant_id, actor_id, actor_kind, action, resource, resource_id, changed_fields, value_before, value_after, trace_id, created_at)
	         VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`,
			rec.Tenant.OrNil(), rec.Actor.ID.OrNil(), string(rec.Actor.Kind), rec.Action, rec.Resource,
			rec.RowID.OrNil(), rec.Changed, nullJSON(before), nullJSON(after), rec.Trace, rec.At); err != nil {
			return translate(err)
		}
	}
	d.Audit = nil
	return nil
}

func nullJSON(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return b
}

var errNotImplemented = errors.New("runtime: not implemented")

var _ = time.Second
