package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"keystone.dev/keystone/ks"
	"keystone.dev/keystone/model"
	"keystone.dev/keystone/policy"
)

// App is one process: the model it was generated from, its pool, its routes and its
// effect providers. A Keystone app is a single binary, so this is the whole world
// (§2.10).
type App struct {
	Model    *model.App
	Cfg      *Config
	Pool     *pgxpool.Pool
	Log      *slog.Logger
	Handlers map[string]*Handler
	// ByPath is the route table's other half: a handler registered under a Ref that
	// differs from the spec's — a test standing in for a real action, typically — is still
	// reachable at the path its spec declares.
	ByPath    map[string]*Handler
	Routes    map[string]string // path -> "module.Action"
	Providers *Providers
	Registry  *Metrics
	// MetricSink receives every counter the framework increments. Set it with
	// SetMetricSink after construction; a plain field write races with the dispatchers
	// already running.
	MetricSink func(name string, delta float64)
	sink       atomic.Pointer[func(name string, delta float64)]
	Auth       *Auth
	Buckets    *Buckets
	Caches     *Cache
	Clock      ks.Clock
	// Keys seals and opens Encrypted() fields; nil when the app declares none (§9.4).
	Keys *Keyring
	// OnCall, when set, receives a Trace after every handler execution. Set it before
	// serving; it is read without a lock.
	OnCall func(Trace)
	// FailOnBudget makes a budget overrun an error, which is how ks test enforces it.
	FailOnBudget bool
	// StartWorkers and StartWebOnly select the process role (§10.6).
	StartWorkers bool
	StartWebOnly bool

	wake     chan struct{}
	inFlight atomic.Int64
	closed   atomic.Bool

	// SkipSchemaCheck lets ks migrate and ks test open a pool before a schema exists.
	SkipSchemaCheck bool

	mu     sync.Mutex
	roles  map[string]bool
	secret []byte
}

// Handler is one action wired into the pipeline.
type Handler struct {
	Ref     string // "billing.SendInvoice"
	Module  string
	Name    string
	Kind    model.ActionKind
	Path    string
	Methods []string
	// Handle runs the action's generated adapter. Call carries everything the adapter
	// needs, so an action's Go signature mentions no framework types other than its own
	// generated context (§7.2).
	Handle func(*Call) (any, error)
	// Schema is the input JSON schema, used by the introspection API and OpenAPI.
	Schema map[string]any
	Spec   model.Action
}

// DecodeInput fills the action's generated input struct from the request: JSON for a
// mutation, and the scalar query parameters for a list, because a GET should be
// constructible from a URL alone (§3.3).
func DecodeInput(c *Call, dst any) error {
	if len(c.Body) > 0 {
		if !utf8.Valid(c.Body) {
			return ks.Invalid("body", "the request body is not valid UTF-8")
		}
		if err := strictJSON(c.Body, dst); err != nil {
			return ks.Invalid("body", "the request body does not fit the input: "+err.Error())
		}
		return nil
	}
	if len(c.Query) == 0 {
		return nil
	}
	return decodeQuery(dst, c.Query)
}

// strictJSON decodes exactly one JSON value and refuses fields the input does not
// declare, so a typo in a client is an error rather than a silently ignored value (§9.3).
func strictJSON(raw []byte, dst any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	if dec.More() {
		return errors.New("trailing data after the JSON value")
	}
	return nil
}

// decodeQuery reads query parameters as strings first; a parameter that fills a number,
// boolean or object field (limit=10) is then read as the JSON literal it spells.
func decodeQuery(dst any, q map[string][]string) error {
	raw, err := json.Marshal(flatQuery(q))
	if err != nil {
		return ks.Invalid("query", err.Error())
	}
	if err := strictJSON(raw, dst); err == nil {
		return nil
	}
	// Decide per parameter, probing a fresh value of the input's type: "12" stays a string
	// for a text field and becomes 12 for a number field.
	typed := map[string]any{}
	for k, v := range flatQuery(q) {
		typed[k] = v
		s, ok := v.(string)
		if !ok || !json.Valid([]byte(s)) {
			continue
		}
		probe := reflect.New(reflect.TypeOf(dst).Elem()).Interface()
		if one, _ := json.Marshal(map[string]any{k: s}); strictJSON(one, probe) != nil {
			typed[k] = json.RawMessage(s)
		}
	}
	if raw, err = json.Marshal(typed); err == nil {
		err = strictJSON(raw, dst)
	}
	if err != nil {
		return ks.Invalid("query", "a query parameter does not fit: "+err.Error())
	}
	return nil
}

func flatQuery(q map[string][]string) map[string]any {
	out := map[string]any{}
	for k, v := range q {
		if len(v) == 1 {
			out[k] = v[0]
			continue
		}
		parts := make([]any, len(v))
		for i, s := range v {
			parts[i] = s
		}
		out[k] = parts
	}
	return out
}

// HandlerOutput is what an adapter returns before encoding.
type HandlerOutput struct {
	Status      int
	Body        []byte
	ContentType string
	Meta        *CallMeta
	meta        *CallMeta
}

// Options configures startup.
type Options struct {
	Cfg       *Config
	Model     *model.App
	Log       *slog.Logger
	Handlers  []*Handler
	Providers map[model.EffectKind]Provider
	// SkipSchemaCheck starts the app against an empty database, for ks migrate and the
	// test harness (§16.1).
	SkipSchemaCheck bool
}

// New builds the app, opens the pool and checks the schema version. Instances refuse
// to start against a schema older than their model (§18.3).
func New(ctx context.Context, o Options) (*App, error) {
	if o.Cfg == nil {
		o.Cfg = Defaults()
	}
	if o.Cfg.App == "" && o.Model != nil {
		o.Cfg.App = o.Model.App.Name
	}
	log := o.Log
	if log == nil {
		log = slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	}
	a := &App{
		Model: o.Model, Cfg: o.Cfg, Log: log,
		Handlers:  map[string]*Handler{},
		ByPath:    map[string]*Handler{},
		Routes:    map[string]string{},
		Providers: NewProviders(),
		Registry:  NewMetrics(),
		wake:      make(chan struct{}, 1),
		roles:     map[string]bool{},
	}
	keys, err := KeyringFromConfig(o.Cfg)
	if err != nil {
		return nil, err
	}
	if keys == nil && hasEncrypted(o.Model) {
		return nil, errors.New("runtime: the app declares Encrypted() fields but no key is configured; set KS_ENCRYPTION_KEY (ks keys new prints one)")
	}
	a.Keys = keys
	for _, h := range o.Handlers {
		a.Handlers[h.Ref] = h
		a.ByPath[h.Name] = h
		for _, p := range h.PathList() {
			a.Routes[p] = h.Name
			a.ByPath[p] = h
		}
	}
	// Configured providers first; ones passed in code (tests, community providers) win.
	configured, err := providersFromConfig(o.Cfg, log)
	if err != nil {
		return nil, err
	}
	for kind, p := range configured {
		a.Providers.Register(kind, p)
	}
	for kind, p := range o.Providers {
		a.Providers.Register(kind, p)
	}
	a.Buckets = newBuckets()
	a.Caches = newCache()
	a.Auth = a.newAuth()
	if a.Clock == nil {
		a.Clock = SystemClock{}
	}
	for _, r := range o.Model.Roles {
		a.roles[r] = true
	}
	if o.Cfg.DatabaseURL == "" {
		return nil, errors.New("runtime: no database url configured")
	}
	cfg, err := pgxpool.ParseConfig(o.Cfg.DatabaseURL)
	if err != nil {
		return nil, fmt.Errorf("runtime: database url: %w", err)
	}
	if o.Cfg.MaxConns > 0 {
		cfg.MaxConns = int32(o.Cfg.MaxConns)
	}
	cfg.MaxConnLifetime = time.Hour
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	pingCtx, cancel := context.WithTimeout(ctx, o.Cfg.PoolWaitTimeout+2*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("runtime: cannot reach the database: %w", err)
	}
	a.Pool = pool
	if o.SkipSchemaCheck {
		a.SkipSchemaCheck = true
	} else if err := a.checkSchema(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return a, nil
}

// Close drains and releases the pool.
func (a *App) Close() {
	if a.closed.Swap(true) {
		return
	}
	if a.Pool != nil {
		a.Pool.Close()
	}
}

// checkSchema compares the newest applied migration with the model's expectation.
func (a *App) checkSchema(ctx context.Context) error {
	var got string
	err := a.Pool.QueryRow(ctx, `SELECT coalesce(max(version),'') FROM ks_migrations`).Scan(&got)
	if err != nil {
		// An empty database is a legitimate first boot; a missing table is not.
		if strings.Contains(err.Error(), "does not exist") {
			return errors.New("runtime: schema is not managed by ks; run ks migrate apply")
		}
		return nil
	}
	if got == "" && !a.SkipSchemaCheck {
		return errors.New("runtime: no migrations applied; run ks migrate apply")
	}
	return nil
}

// HandlerFor looks up an action by its qualified name.
func (a *App) HandlerFor(ref string) (*Handler, bool) {
	h, ok := a.Handlers[ref]
	return h, ok
}

// PathList is the route set for one handler (§12.1).
func (h *Handler) PathList() []string {
	if h.Path == "" {
		return nil
	}
	return []string{h.Path}
}

// Scope is the per-transaction identity the database sees (§8.5). Role rules live in
// the query layer; the tenant boundary is also a policy, so it needs GUCs.
type Scope struct {
	Tenant      ks.ID
	Actor       ks.ID
	IsSystem    bool
	CrossTenant bool
}

func systemScope() Scope {
	return Scope{IsSystem: true, CrossTenant: true}
}

// beginScope applies the session variables for one transaction. SET LOCAL makes them
// revert at commit or rollback, so a pooled connection never leaks an identity.
func beginScope(ctx context.Context, tx pgx.Tx, s Scope) error {
	// SET does not accept parameters, so the identity travels through set_config,
	// which is transaction-local with the third argument true (§8.5).
	values := []struct {
		key string
		val string
	}{
		{"ks.tenant_id", uuidText(s.Tenant)},
		{"ks.actor_id", uuidText(s.Actor)},
		{"ks.actor_system", onoff(s.IsSystem)},
		{"ks.cross_tenant", onoff(s.CrossTenant)},
	}
	for _, v := range values {
		if _, err := tx.Exec(ctx, "SELECT set_config($1, $2, true)", v.key, v.val); err != nil {
			return translate(err)
		}
	}
	return nil
}

func uuidText(id ks.ID) string {
	if id.IsNil() {
		return ""
	}
	return string(id)
}

func onoff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

// BeginTx opens the action's transaction with its identity and timeouts (§8.3).
func (a *App) BeginTx(ctx context.Context, s Scope, stmtTimeout time.Duration, serializable bool) (pgx.Tx, error) {
	if os.Getenv("KS_DEBUG_SQL") != "" {
		fmt.Fprintf(os.Stderr, "\n# BEGIN acquired=%d\n", a.Pool.Stat().AcquiredConns())
	}
	tx, err := a.Pool.BeginTx(ctx, pgx.TxOptions{
		AccessMode: pgx.ReadWrite,
		IsoLevel:   level(serializable),
	})
	if err != nil {
		return nil, translate(err)
	}
	if err := beginScope(ctx, tx, s); err != nil {
		_ = tx.Rollback(ctx)
		return nil, err
	}
	if stmtTimeout > 0 {
		if _, err := tx.Exec(ctx, "SELECT set_config('statement_timeout', $1, true)", fmt.Sprintf("%dms", stmtTimeout.Milliseconds())); err != nil {
			_ = tx.Rollback(ctx)
			return nil, translate(err)
		}
	}
	if _, err := tx.Exec(ctx, "SELECT set_config('lock_timeout', $1, true)", "2s"); err != nil {
		_ = tx.Rollback(ctx)
		return nil, translate(err)
	}
	return tx, nil
}

func level(serializable bool) pgx.TxIsoLevel {
	if serializable {
		return pgx.Serializable
	}
	return pgx.ReadCommitted
}

// inSystemScope runs a framework write with no tenant attached: the identity tables,
// idempotency records and schedule slots are framework-owned and written outside any
// request's tenant scope (§10.5, §12.1).
func (a *App) inSystemScope(ctx context.Context, fn func(tx pgx.Tx) error) error {
	tx, err := a.BeginTx(ctx, Scope{IsSystem: true, CrossTenant: true}, a.Cfg.StmtTimeout, false)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		a.Rollback(ctx, tx)
		return err
	}
	return tx.Commit(ctx)
}

// framework runs one statement against a framework table outside any request. These
// tables are readable under the permissive policy, but the statement still executes
// inside a transaction so set_config has somewhere to live, and the query carries its
// own predicate — there is no actor to inherit one from (§5.1).
func (a *App) framework(ctx context.Context, fn func(tx pgx.Tx) error) error {
	return a.inSystemScope(ctx, fn)
}

// NewDB builds the scoped database handle for one action run (§3.3 step 7).
func (a *App) NewDB(tx pgx.Tx, actor ks.Actor, opts ...DBOption) *DB {
	d := &DB{
		conn:   tx,
		tx:     tx,
		actor:  actor,
		app:    a.Model,
		cfg:    a.Cfg,
		log:    a.Log.With("action", actor.Kind),
		clock:  SystemClock{},
		budget: &Budget{MaxQ: a.Cfg.QueryBudget, MaxR: int64(a.Cfg.RowBudget)},
		Audit:  nil,
	}
	for _, o := range opts {
		o(d)
	}
	return d
}

// DBOption adjusts one action's scope.
type DBOption func(*DB)

// WithClock installs the action's clock.
func WithClock(c ks.Clock) DBOption { return func(d *DB) { d.clock = c } }

// WithLogger scopes the logger to the action.
func WithLogger(l *slog.Logger) DBOption { return func(d *DB) { d.log = l } }

// WithAction names the action, for logs, audit and trace spans.
func WithAction(ref string) DBOption {
	return func(d *DB) {
		d.action = ref
		if d.log != nil {
			d.log = d.log.With("action", ref)
		}
	}
}

// WithTrace carries the request's trace id into every record.
func WithTrace(id string) DBOption { return func(d *DB) { d.traceID = id } }

// WithCrossTenant opts out of tenant isolation for one action, which the diff flags.
func WithCrossTenant(on bool) DBOption { return func(d *DB) { d.cross = on } }

// WithBudget overrides the action's budgets.
func WithBudget(queries int, rows int64, failHard bool) DBOption {
	return func(d *DB) {
		d.budget = &Budget{MaxQ: queries, MaxR: rows, FailHard: failHard}
	}
}

// WithProviders gives the DB access to immediate effects.
func WithApp(a *App) DBOption {
	return func(d *DB) {
		d.app = a.Model
		d.providers = a.Providers
		d.keys = a.Keys
	}
}

// Commit finishes the transaction and makes outbox rows visible (§3.3 step 9).
func (a *App) Commit(ctx context.Context, d *DB, tx pgx.Tx) error {
	// Anything that fails before the COMMIT must end the transaction, or the
	// connection goes back to the pool with an open one inside it.
	fail := func(err error) error {
		a.Rollback(ctx, tx)
		return err
	}
	if err := a.flushAudit(ctx, d); err != nil {
		return fail(err)
	}
	if err := a.NewOutbox().Insert(ctx, d); err != nil {
		return fail(err)
	}
	if err := tx.Commit(ctx); err != nil {
		return translate(err)
	}
	if len(d.EffectsQueued) > 0 {
		if err := a.Notify(ctx, a.Pool); err != nil {
			a.Log.Warn("outbox notify", "err", err)
		}
	}
	return nil
}

// Rollback abandons the transaction; queued effects disappear with it.
func (a *App) Rollback(ctx context.Context, tx pgx.Tx) {
	if tx != nil {
		err := tx.Rollback(ctx)
		if os.Getenv("KS_DEBUG_SQL") != "" {
			fmt.Fprintf(os.Stderr, "\n# ROLLBACK err=%v acquired=%d\n", err, a.Pool.Stat().AcquiredConns())
		}
	}
}

var _ = policy.Always

// metric records a counter, or forwards to an installed sink.
// SetMetricSink installs a metrics sink while the app is running.
func (a *App) SetMetricSink(fn func(name string, delta float64)) {
	if fn == nil {
		return
	}
	a.sink.Store(&fn)
}

func (a *App) metric(name string, delta float64) {
	if p := a.sink.Load(); p != nil {
		(*p)(name, delta)
		return
	}
	if a.MetricSink != nil {
		a.MetricSink(name, delta)
		return
	}
	if a.Registry != nil {
		a.Registry.metric(name, delta)
	}
}
