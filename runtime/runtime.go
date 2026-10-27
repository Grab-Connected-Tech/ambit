// Package runtime enforces the spec at request time: the actor-scoped query layer,
// the transaction, budgets, invariants, optimistic concurrency, the audit record, the
// outbox, jobs and the HTTP pipeline (§3.3). Generated code calls into it; app code
// never reaches it directly except through the context structs the generator writes.
package runtime

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Grab-Connected-Tech/ambit/am"
	"github.com/Grab-Connected-Tech/ambit/model"
	"github.com/Grab-Connected-Tech/ambit/policy"
)

// Conn is satisfied by both a pool and a transaction, so the same scoped code runs
// before and after the action's transaction opens.
type Conn interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Row is the positional view a generated scanner reads. Values arrive already
// decoded by pgx, so scanning is a type assertion, not reflection (§11.1).
type Row interface {
	Null(i int) bool
	Str(i int) string
	ID(i int) am.ID
	Int(i int) int64
	Bool(i int) bool
	Time(i int) time.Time
	Decimal(i int) am.Decimal
	Blob(i int) []byte
	Raw(i int) any
	Err() error
}

type row struct {
	v   []any
	err error
}

func (r *row) Null(i int) bool {
	if i >= len(r.v) || r.v[i] == nil {
		return true
	}
	if t, ok := r.v[i].(time.Time); ok {
		return t.IsZero()
	}
	return false
}
func (r *row) Str(i int) string {
	x := r.at(i)
	switch t := x.(type) {
	case nil:
		return ""
	case string:
		return t
	case []byte:
		return string(t)
	}
	return fmt.Sprint(x)
}
func (r *row) ID(i int) am.ID {
	id, err := am.ScanID(r.at(i))
	if err != nil {
		r.err = err
	}
	return id
}
func (r *row) Int(i int) int64 {
	switch t := r.at(i).(type) {
	case int64:
		return t
	case int32:
		return int64(t)
	case string:
		n, _ := strconv.ParseInt(t, 10, 64)
		return n
	case []byte:
		n, _ := strconv.ParseInt(string(t), 10, 64)
		return n
	}
	return 0
}
func (r *row) Bool(i int) bool { b, _ := r.at(i).(bool); return b }
func (r *row) Time(i int) time.Time {
	switch t := r.at(i).(type) {
	case time.Time:
		return t.UTC()
	case string: // an Encrypted() timestamp, opened to its text form
		p, _ := time.Parse(time.RFC3339Nano, t)
		return p.UTC()
	}
	return time.Time{}
}
func (r *row) Decimal(i int) am.Decimal {
	switch t := r.at(i).(type) {
	case string:
		d, _ := am.ParseDecimal(t)
		return d
	case int64:
		return am.DecimalInt(t)
	}
	return am.DecimalInt(0)
}
func (r *row) Blob(i int) []byte {
	switch t := r.at(i).(type) {
	case []byte:
		return t
	case string:
		return []byte(t)
	}
	return nil
}
func (r *row) Raw(i int) any { return r.at(i) }
func (r *row) Err() error    { return r.err }
func (r *row) at(i int) any {
	if i >= len(r.v) {
		return nil
	}
	return r.v[i]
}

// Column describes one selectable column of a resource.
type Column struct {
	Name      string // spec name: "notes"
	GoName    string // field name: "Notes"
	Column    string // bare column: "notes"
	SQL       string // qualified: "t.notes"
	SQLType   string // postgres type, for casts
	Kind      string // model.TypeKind
	Sensitive bool
	Encrypted bool // sealed with the app's keyring at rest (§9.4)
}

// Frag is a compiled policy fragment: SQL with ? placeholders for actor values only.
type Frag struct {
	SQL   string
	Binds []policy.Bind
}

// Empty reports a fragment that imposes no restriction.
func (f Frag) Empty() bool { return f.SQL == "" || f.SQL == "true" || f.SQL == "1=1" }

// Denies reports a rule that admits nothing: Never(), or no rule at all. Empty is the
// opposite question (a rule that adds no condition), so Always() must not be read as
// "no rule" by a write guard.
func (f Frag) Denies() bool { return f.SQL == "" || f.SQL == "false" }

// Check is a deferred invariant: a predicate over one row that must hold (§5.5).
type Check struct {
	Name string
	Frag Frag
	// Forbidden marks an UpdateAfter rule, whose failure is a 403 not a 422 (§5.6).
	Forbidden bool
}

// Table is a generated resource's storage description: the only way in.
type Table[T any] struct {
	Ref         string
	Name        string
	Table       string
	Tenant      bool
	IDColumn    string
	Columns     []Column
	Select      string // projection without the t. prefix resolved at build time
	All         Frag   // compiled Read rule for the whole row
	Create      Frag
	Update      Frag
	UpdateAfter Frag
	Delete      Frag
	FieldRead   []FieldFrag
	Checks      []Check
	// FieldWrite carries per-column write predicates (§5.6).
	FieldWrite []FieldFrag
	// IDOf reads a row's primary key without reflection.
	IDOf func(*T) am.ID
	// VersionOf reads a row's optimistic-concurrency counter.
	VersionOf func(*T) int64
	Scan      func(Row) (*T, error)
	Encode    func(*T) []byte
	// Cursor renders the ordering values of a row, keyed by the SQL expression the
	// page cursor will ask for. It is what keeps pagination keyset-only (§8.1).
	Cursor func(*T) map[string]string
	// Sensitive columns are dropped from audit diffs even if a rule forgets them.
	Sensitive []string
}

// FieldFrag pairs a column with a read or write rule (§5.6 FieldRead).
type FieldFrag struct {
	Field  string
	Column string
	Frag       // the rule, with its actor parameters
	Index  int // position in the projection, for reads
}

// fragFromPolicy adapts a compiled fragment to the runtime representation.
func fragFromPolicy(s *policy.SQL) Frag {
	if s == nil {
		return Frag{}
	}
	return Frag{SQL: s.Text, Binds: s.Binds}
}

// tenantPolicy is the isolation predicate used by every scoped statement, qualified
// with the statement's table alias.
func tenantPolicy(cross bool, alias string) Frag {
	return fragFromPolicy(policy.TenantPolicy(cross).WithRoot(alias))
}

// Column returns the projection expression for a named column.
func (t *Table[T]) Column(name string) (Column, bool) {
	for _, c := range t.Columns {
		if c.Name == name {
			return c, true
		}
	}
	return Column{}, false
}

// DB is the actor-scoped database view handed to one action (§8.1). Every query built
// through it carries the tenant filter, the compiled Read rule and the budget.
type DB struct {
	conn          Conn
	tx            pgx.Tx
	actor         am.Actor
	app           *model.App
	cfg           *Config
	log           *slog.Logger
	clock         am.Clock
	budget        *Budget
	cross         bool
	replica       bool
	mu            *sync.Mutex
	stmts         []Stmt
	Audit         []AuditRecord
	EffectsQueued []EffectCall
	JobsQueued    []JobCall
	// Compensations are queued only if the transaction does not commit.
	Compensations []JobCall
	traceID       string
	action        string
	invocation    am.ID
	start         time.Time
	providers     *Providers
	keys          *Keyring
}

// Stmt is one executed statement, kept for traces and for am perf capture.
type Stmt struct {
	SQL    string
	At     time.Time
	Args   []any `json:"-"` // may hold Sensitive() values; never leaves the process
	Rows   int64
	Millis float64
	Table  string
}

// AuditRecord is one row of the audit log (§9.5).
type AuditRecord struct {
	Tenant   am.ID
	Actor    am.Actor
	Action   string
	Resource string
	RowID    am.ID
	Changed  []string
	Before   json.RawMessage
	After    json.RawMessage
	Trace    string
	At       time.Time
}

// Budget counts queries and rows for one action (§8.4).
type Budget struct {
	mu      sync.Mutex
	Queries int
	Rows    int64
	MaxQ    int
	MaxR    int64
	err     error
	// FailHard makes an overrun an error, which is how tests enforce budgets (§14.3
	// AM-PERF-001); production logs a metric instead.
	FailHard bool
}

func (b *Budget) addQuery(rows int64) error {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.Queries++
	b.Rows += rows
	switch {
	case b.MaxQ > 0 && b.Queries > b.MaxQ:
		if b.FailHard && b.err == nil {
			b.err = am.BudgetExceeded("query", b.MaxQ)
		}
	case b.MaxR > 0 && b.Rows > b.MaxR:
		if b.FailHard && b.err == nil {
			b.err = am.BudgetExceeded("row", int(b.MaxR))
		}
	}
	return nil
}

// View is one resource's scoped handle inside one action's database.
type View[T any] struct {
	t   *Table[T]
	db  *DB
	ctx context.Context
}

// DB returns the scope a view was built from, which relation loaders need.
func (v *View[T]) DB() *DB { return v.db }

// Context returns the context the loader runs under.
func (v *View[T]) Context() context.Context {
	if v.ctx != nil {
		return v.ctx
	}
	return context.Background()
}

// Scoped binds a generated table description to one action's scope. It is a function
// rather than a method because Go methods cannot take type parameters.
func Scoped[T any](d *DB, t *Table[T]) *View[T] {
	return &View[T]{t: t, db: d}
}

// Actor returns the identity the scope was built with.
func (d *DB) Actor() am.Actor { return d.actor }

// CrossTenant reports whether the action declared CrossTenant().
func (d *DB) CrossTenant() bool { return d.cross }

// Log returns the action's logger, pre-scoped to the action (§18.1).
func (d *DB) Log() *slog.Logger { return d.log }

// Clock returns the action's clock.
func (d *DB) Clock() am.Clock { return d.clock }

// Stmts returns the statements run so far, for traces and am perf capture.
func (d *DB) Stmts() []Stmt { return d.stmts }

// exec runs one statement under the budget.
func (d *DB) exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	start := time.Now()
	tag, err := d.conn.Exec(ctx, sql, args...)
	d.record(sql, args, tag.RowsAffected(), start)
	if err != nil {
		return tag, err
	}
	return tag, d.budgetErr()
}

func (d *DB) query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	start := time.Now()
	rows, err := d.conn.Query(ctx, sql, args...)
	d.record(sql, args, 0, start)
	if err != nil {
		return rows, err
	}
	return rows, d.budgetErr()
}

// budgetErr reports the first budget overrun, so a query can fail after it ran and
// before its results reach the action (§8.4).
func (d *DB) budgetErr() error {
	if d.budget == nil {
		return nil
	}
	return d.budget.err
}

func (d *DB) record(sql string, args []any, rows int64, start time.Time) {
	table := ""
	if i := strings.Index(sql, " FROM "); i > 0 {
		rest := sql[i+6:]
		table = strings.Fields(rest)[0]
	}
	d.stmts = append(d.stmts, Stmt{SQL: sql, Args: args, Rows: rows, Table: table, At: start,
		Millis: float64(time.Since(start).Microseconds()) / 1000})
	if os.Getenv("AM_DEBUG_SQL") != "" && !strings.HasPrefix(sql, "SELECT set_config") {
		fmt.Fprintf(os.Stderr, "\n# %s   --args %v\n", sql, args)
	}
	_ = d.budget.addQuery(rows)
}

// sqlBuilder numbers placeholders in statement order, which is the one mistake that
// would silently mix parameters between the projection and the filter.
type sqlBuilder struct {
	params []any
	a      policy.ActorValues
}

// text replaces each ? in a fragment with the next $n, appending its value.
func (b *sqlBuilder) text(sql string, args ...any) (string, error) {
	var out strings.Builder
	n := 0
	for i := 0; i < len(sql); i++ {
		if sql[i] != '?' {
			out.WriteByte(sql[i])
			continue
		}
		if n >= len(args) {
			return "", fmt.Errorf("runtime: placeholder %d has no value in %q", n+1, sql)
		}
		b.params = append(b.params, args[n])
		fmt.Fprintf(&out, "$%d", len(b.params))
		n++
	}
	return out.String(), nil
}

// frag renders a compiled policy fragment, binding actor values as parameters.
func (b *sqlBuilder) frag(f Frag) (string, error) {
	if f.Empty() {
		return "true", nil
	}
	sql := f.SQL
	marker := 0
	var out strings.Builder
	for i := 0; i < len(sql); i++ {
		if sql[i] != '?' {
			out.WriteByte(sql[i])
			continue
		}
		if marker >= len(f.Binds) {
			return "", fmt.Errorf("runtime: fragment %q has more placeholders than bindings", sql)
		}
		bd := f.Binds[marker]
		marker++
		v, err := bindValue(bd, b.a)
		if err != nil {
			return "", fmt.Errorf("%w (fragment %q, bind %d of %d)", err, sql, marker, len(f.Binds))
		}
		b.params = append(b.params, v)
		fmt.Fprintf(&out, "$%d", len(b.params))
	}
	if marker != len(f.Binds) {
		return "", fmt.Errorf("runtime: fragment %q has %d bindings but %d placeholders", sql, len(f.Binds), marker)
	}
	return out.String(), nil
}

func bindValue(b policy.Bind, a policy.ActorValues) (any, error) {
	switch b.Kind {
	case policy.BindActorID:
		return nilIfEmpty(a.ID), nil
	case policy.BindActorTenant:
		return nilIfEmpty(a.Tenant), nil
	case policy.BindActorSystem:
		return a.IsSystem, nil
	}
	return nil, fmt.Errorf("runtime: unexpected literal binding %q in a generated fragment", b.Lit.Text)
}

func nilIfEmpty(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return b
}

// bind turns a policy fragment into SQL with $n placeholders appended to params.
func bind(f Frag, params []any, a policy.ActorValues, offset int) (string, []any, error) {
	if f.Empty() {
		return "true", params, nil
	}
	sql := f.SQL
	if !strings.Contains(sql, "?") && len(f.Binds) == 0 {
		return sql, params, nil
	}
	s := &policy.SQL{Text: sql, Binds: f.Binds}
	out, args, err := policy.Place(s, offset+len(params), a)
	if err != nil {
		return "", params, fmt.Errorf("%w\nfragment: %s\nbinds: %d params: %d", err, sql, len(f.Binds), len(params))
	}
	if err != nil {
		return "", params, err
	}
	return out, append(params, args...), nil
}

// actorValues renders the caller for parameter binding.
func actorValues(a am.Actor) policy.ActorValues {
	return policy.ActorValues{ID: a.ID.Bytes(), Tenant: a.Tenant.Bytes(), IsSystem: a.Kind == am.ActorSystem}
}

// Query assembles a scoped SELECT (§8.1): tenant, then the Read rule, then the action's
// own filter. It is what generated code hands to an action, so the only way to reach the
// database through a resource is a typed one.
type Query[T any] struct {
	v      *View[T]
	parts  []string
	parts2 []Cond
	args   []any
	order  []orderTerm
	withs  []With[T]
	limit  int
	cursor string
	count  bool
}

type orderTerm struct {
	SQL  string
	Desc bool
	// Value and ID come from the cursor.
	Value string
}

// With is a relation loader: one batched query per relation, never per row (§8.2).
type With[T any] struct {
	Name string
	Load func(v *View[T], rows []*T) error
}

// Q starts a scoped query on the resource.
func (v *View[T]) Q() *Query[T] { return &Query[T]{v: v} }

// Where adds a generated condition.
func (b *Query[T]) Where(c ...Cond) *Query[T] {
	for _, x := range c {
		if x.SQL == "" {
			continue
		}
		b.parts = append(b.parts, x.SQL)
		b.args = append(b.args, x.Args...)
	}
	return b
}

// Cond is one generated predicate.
type Cond struct {
	SQL  string
	Args []any
}

// AndOf joins conditions with AND, which is what generated column helpers use for
// multi-column predicates such as money.
func AndOf(in ...Cond) Cond {
	var sqls []string
	var args []any
	for _, c := range in {
		if c.SQL == "" {
			continue
		}
		sqls = append(sqls, "("+c.SQL+")")
		args = append(args, c.Args...)
	}
	if len(sqls) == 0 {
		return Cond{}
	}
	return Cond{SQL: strings.Join(sqls, " AND "), Args: args}
}

// OrOf joins conditions with OR.
func OrOf(in ...Cond) Cond {
	var sqls []string
	var args []any
	for _, c := range in {
		if c.SQL == "" {
			continue
		}
		sqls = append(sqls, "("+c.SQL+")")
		args = append(args, c.Args...)
	}
	if len(sqls) == 0 {
		return Cond{}
	}
	return Cond{SQL: strings.Join(sqls, " OR "), Args: args}
}

// Always is a condition that matches; generated helpers use it for IsSet on a
// non-nullable column.
func Always() Cond { return Cond{SQL: "true"} }

// Never is a condition that matches nothing, which is what an invisible field gets.
func Never() Cond { return Cond{SQL: "false"} }

// OrderBy appends ordering terms.
func (b *Query[T]) OrderBy(terms ...orderTerm) *Query[T] {
	b.order = append(b.order, terms...)
	return b
}

// With loads relations in one batched query each.
func (b *Query[T]) With(w ...With[T]) *Query[T] {
	b.withs = append(b.withs, w...)
	return b
}

// Order builds an ordering term from a generated column descriptor.
func Order(c string, desc bool) orderTerm { return orderTerm{SQL: c, Desc: desc} }

// selectList returns the visible projection: hidden columns are never fetched, so a
// field-level read rule cannot be forgotten (§5.6, §9.1 excessive data exposure).
func (v *View[T]) selectList(hide map[int]bool) string {
	parts := make([]string, 0, len(v.t.Columns))
	for i, c := range v.t.Columns {
		if hide[i] {
			continue
		}
		parts = append(parts, c.SQL)
	}
	if len(parts) == 0 {
		parts = append(parts, "t.id")
	}
	return strings.Join(parts, ", ")
}

// fieldRules evaluates the FieldRead predicates, returning the positions to hide.
func (v *View[T]) fieldRules(params []any) (map[int]bool, []any, error) {
	hide := map[int]bool{}
	if len(v.t.FieldRead) == 0 {
		return hide, params, nil
	}
	a := actorValues(v.db.actor)
	for _, fr := range v.t.FieldRead {
		sql, next, err := bind(Frag{SQL: fr.SQL, Binds: fr.Binds}, params, a, 0)
		if err != nil {
			return nil, params, err
		}
		// A field rule that cannot be evaluated in the projection becomes a CASE
		// expression, so a hidden value never travels over the wire.
		params = append(params, next[len(params):]...)
		_ = sql
		hide[fr.Index] = true
	}
	return hide, params, nil
}

// whereAssembles builds the full WHERE clause for a scoped read.
func (v *View[T]) where(extra []string, params []any) (string, []any, error) {
	a := actorValues(v.db.actor)
	var parts []string
	if v.t.Tenant {
		sql, next, err := bind(tenantPolicy(v.db.cross, "t"), params, a, 0)
		if err != nil {
			return "", params, err
		}
		parts = append(parts, sql)
		params = next
	}
	sql, next, err := bind(v.t.All, params, a, 0)
	if err != nil {
		return "", params, err
	}
	params = next
	if !strings.EqualFold(sql, "true") {
		parts = append(parts, "("+sql+")")
	}
	parts = append(parts, extra...)
	if len(parts) == 0 {
		return "true", params, nil
	}
	return strings.Join(parts, " AND "), params, nil
}

// cursorValue is one ordering key encoded in a page cursor.
type cursorValue struct {
	Val string
	ID  am.ID
}

func encodeCursor(c []cursorValue) string {
	b, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodeCursor(s string) ([]cursorValue, error) {
	if s == "" {
		return nil, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return nil, am.Invalid("cursor", "malformed cursor")
	}
	var out []cursorValue
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, am.Invalid("cursor", "malformed cursor")
	}
	return out, nil
}

var errNoRows = errors.New("runtime: no rows")

// SetBudget overrides the action's budgets, which is how am test enforces them
// (AM-PERF-001).
func (d *DB) SetBudget(queries int, rows int64, failHard bool) {
	d.budget = &Budget{MaxQ: queries, MaxR: rows, FailHard: failHard}
}

// CallEffect runs an immediate effect and returns its result (§10.2).
func (d *DB) CallEffect(ctx context.Context, call EffectCall) (EffectResult, error) {
	if call.Mode == "" {
		call.Mode = "immediate"
	}
	if call.Invocation.IsNil() {
		call.Invocation = d.invocation
	}
	if call.Action == "" {
		call.Action = d.action
	}
	res, err := d.callProvider(ctx, call)
	if err != nil {
		return res, err
	}
	call.Payload = firstJSON(call.Payload)
	// The effect happened and cannot be unsent: if this transaction later rolls back,
	// the declared compensation runs instead (§10.2).
	if job := compensationFor(d.app, call); job != "" {
		args, _ := json.Marshal(map[string]any{"effect": call, "result": res})
		d.Compensations = append(d.Compensations, JobCall{Action: job, Tenant: d.actor.Tenant, Args: args, Invocation: d.invocation})
	}
	return res, d.RecordEffect(ctx, call)
}

func firstJSON(b []byte) []byte {
	if len(b) == 0 {
		return []byte("{}")
	}
	return b
}
