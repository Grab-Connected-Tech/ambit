// Package kstest is the test harness ks test runs app tests with (§16). Each test gets
// its own database cloned from a template that holds the current schema, a fake clock,
// recording effect providers, and actions run through the full pipeline.
//
//	env := kstest.New(t, gen.Model)
//	org := env.Tenant()
//	acct := env.Actor(org, app.Accountant)
//	inv := kstest.Create(env, gen.InvoiceTable, gen.Invoice{TenantID: org, Status: gen.InvoiceStatusDraft})
//	out, err := kstest.Call(env, acct, gen.SendInvoiceHandler, billing.Actions.SendInvoice, gen.SendInvoiceInput{InvoiceID: inv.ID})
//	env.AssertQueriesAtMost(3)
package kstest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"keystone.dev/keystone/ks"
	"keystone.dev/keystone/migrate"
	"keystone.dev/keystone/model"
	"keystone.dev/keystone/runtime"
)

// Epoch is where every test's clock starts, so time-dependent logic is deterministic.
var Epoch = time.Date(2030, 1, 1, 9, 0, 0, 0, time.UTC)

// appRole is the non-owner role tests connect as: a superuser bypasses row-level
// security even under FORCE, and the backstop must hold against the app (§8.5).
const appRole, appPassword = "ks_test_app", "ks_test_app"

// Env is one test's isolated app.
type Env struct {
	T     testing.TB
	App   *runtime.App
	Clock *ks.FakeClock
	Model *model.App

	mu     sync.Mutex
	traces []runtime.Trace
	sent   []runtime.EffectCall
	stubs  map[string]runtime.EffectResult
	owner  *pgxpool.Pool // the schema owner, for fixtures that bypass rules
}

// New clones a fresh database for the test and starts the app on it. Without
// KS_TEST_DB_URL the test is skipped, so a plain go test still passes offline.
func New(t testing.TB, app *model.App) *Env {
	t.Helper()
	base := os.Getenv("KS_TEST_DB_URL")
	if base == "" {
		t.Skip("KS_TEST_DB_URL is not set; ks test sets it")
	}
	ctx := context.Background()
	tmpl, err := template(ctx, base, app)
	if err != nil {
		t.Fatalf("kstest: template database: %v", err)
	}
	name := "ks_t_" + strings.ReplaceAll(string(ks.NewID()), "-", "")
	admin, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatalf("kstest: %v", err)
	}
	defer admin.Close(ctx)
	if _, err := admin.Exec(ctx, fmt.Sprintf("CREATE DATABASE %s TEMPLATE %s", name, tmpl)); err != nil {
		t.Fatalf("kstest: clone template: %v", err)
	}
	env := &Env{T: t, Clock: ks.NewFakeClock(Epoch), Model: app, stubs: map[string]runtime.EffectResult{}}
	ownerURL := withDB(base, name)
	t.Cleanup(func() {
		bg := context.Background()
		if c, err := pgx.Connect(bg, base); err == nil {
			_, _ = c.Exec(bg, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
			c.Close(bg)
		}
	})
	if env.owner, err = pgxpool.New(ctx, ownerURL); err != nil {
		t.Fatalf("kstest: %v", err)
	}
	t.Cleanup(env.owner.Close)

	cfg := runtime.Defaults()
	cfg.App = app.Name()
	cfg.DatabaseURL = withUser(ownerURL, appRole, appPassword)
	cfg.MaxConns = 8
	// Encrypted() fields need a key; tests get one per process.
	if cfg.Secrets == nil {
		cfg.Secrets = map[string]string{}
	}
	if cfg.DeclaredKeys == nil {
		cfg.DeclaredKeys = map[string]bool{}
	}
	cfg.Secrets["encryptionKey"], cfg.DeclaredKeys["secrets.encryptionKey"] = testKey, true
	if app.Defaults.QueryBudget > 0 {
		cfg.QueryBudget = app.Defaults.QueryBudget
	}
	providers := map[model.EffectKind]runtime.Provider{}
	for _, k := range []model.EffectKind{model.EffectEmail, model.EffectSMS, model.EffectHTTP,
		model.EffectWebhook, model.EffectStorage, model.EffectEnqueue} {
		providers[k] = recorder{env: env, kind: k}
	}
	env.App, err = runtime.New(ctx, runtime.Options{Cfg: cfg, Model: app, Providers: providers})
	if err != nil {
		t.Fatalf("kstest: start app: %v", err)
	}
	t.Cleanup(env.App.Close)
	env.App.Clock = env.Clock
	env.App.FailOnBudget = true
	env.App.OnCall = func(tr runtime.Trace) {
		env.mu.Lock()
		env.traces = append(env.traces, tr)
		env.mu.Unlock()
	}
	if path := os.Getenv("KS_CAPTURE_QUERIES"); path != "" {
		t.Cleanup(func() { env.capture(path) })
	}
	return env
}

var testKey = runtime.NewKey()

var templates sync.Map // schema hash -> template name, per process

// template returns a database holding the app's schema, creating it once per schema.
// It is keyed by the DDL itself, so a framework change rebuilds it too.
func template(ctx context.Context, base string, app *model.App) (string, error) {
	ddl := migrate.DDL(app)
	sum := sha256.Sum256([]byte(strings.Join(ddl, "\n")))
	name := "ks_tmpl_" + hex.EncodeToString(sum[:8])
	if _, ok := templates.Load(name); ok {
		return name, nil
	}
	admin, err := pgx.Connect(ctx, base)
	if err != nil {
		return "", err
	}
	defer admin.Close(ctx)
	// Test binaries for different packages run at once; one of them builds the template.
	if _, err := admin.Exec(ctx, "SELECT pg_advisory_lock(hashtext('ks_test_template'))"); err != nil {
		return "", err
	}
	defer admin.Exec(ctx, "SELECT pg_advisory_unlock(hashtext('ks_test_template'))")
	var exists bool
	if err := admin.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname=$1)", name).Scan(&exists); err != nil {
		return "", err
	}
	if !exists {
		if err := buildTemplate(ctx, admin, base, name, app, ddl); err != nil {
			_, _ = admin.Exec(ctx, "DROP DATABASE IF EXISTS "+name)
			return "", err
		}
	}
	templates.Store(name, true)
	return name, nil
}

func buildTemplate(ctx context.Context, admin *pgx.Conn, base, name string, app *model.App, ddl []string) error {
	var hasRole bool
	if err := admin.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname=$1)", appRole).Scan(&hasRole); err != nil {
		return err
	}
	if !hasRole {
		if _, err := admin.Exec(ctx, fmt.Sprintf("CREATE ROLE %s LOGIN PASSWORD '%s'", appRole, appPassword)); err != nil {
			return err
		}
	}
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		return err
	}
	c, err := pgx.Connect(ctx, withDB(base, name))
	if err != nil {
		return err
	}
	defer c.Close(ctx)
	// The schema is owned by the migration role and the app gets DML only, which is
	// what a deploy looks like (§18.3).
	stmts := append(ddl,
		"GRANT USAGE ON SCHEMA public TO "+appRole,
		"GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO "+appRole,
		"GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO "+appRole,
		"REVOKE UPDATE, DELETE, TRUNCATE ON ks_audit FROM "+appRole,
	)
	for _, stmt := range stmts {
		if _, err := c.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("%w\n  in: %s", err, strings.SplitN(strings.TrimSpace(stmt), "\n", 2)[0])
		}
	}
	_, err = c.Exec(ctx, `INSERT INTO ks_migrations (version, name, checksum, model_hash) VALUES ('0000', 'kstest', '', $1)`, app.Hash())
	return err
}

func withDB(u, name string) string {
	parsed, err := url.Parse(u)
	if err != nil {
		return u
	}
	parsed.Path = "/" + name
	return parsed.String()
}

func withUser(u, user, password string) string {
	parsed, err := url.Parse(u)
	if err != nil {
		return u
	}
	parsed.User = url.UserPassword(user, password)
	return parsed.String()
}

// system runs fixture SQL as the schema owner, across tenants.
func (e *Env) system(fn func(tx pgx.Tx) error) {
	e.T.Helper()
	ctx := context.Background()
	err := pgx.BeginFunc(ctx, e.owner, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SELECT set_config('ks.cross_tenant','on',true), set_config('ks.actor_system','on',true)"); err != nil {
			return err
		}
		return fn(tx)
	})
	if err != nil {
		e.T.Fatalf("kstest: fixture: %v", err)
	}
}

// Tenant creates a tenant row with placeholder values for its required fields.
func (e *Env) Tenant(fields ...map[string]any) ks.ID {
	e.T.Helper()
	if e.Model.Tenant == nil {
		e.T.Fatal("kstest: the app declares no tenant")
	}
	id := ks.NewID()
	set := merge(fields)
	if columnOf(e.Model.Tenant.Fields, "tenant_id") {
		set["tenant_id"] = id // the tenant row is its own tenant
	}
	e.insertIn(e.Model.Tenant.Table, e.Model.Tenant.Fields, id, set, id)
	return id
}

// Actor creates an account in the tenant holding the given roles: role names, or the
// spec's role declarations (anything with a Name() string method).
func (e *Env) Actor(tenant ks.ID, roles ...any) ks.Actor {
	e.T.Helper()
	a := ks.Actor{Kind: ks.ActorUser, ID: ks.NewID(), Tenant: tenant, Authed: true}
	for _, r := range roles {
		var name string
		switch r := r.(type) {
		case string:
			name = r
		case interface{ Name() string }:
			name = r.Name()
		default:
			e.T.Fatalf("kstest: a role is a string or a spec role, not %T", r)
		}
		if !contains(a.Roles, name) {
			a.Roles = append(a.Roles, name)
		}
	}
	if acc := e.Model.Account; acc != nil {
		set := map[string]any{}
		if columnOf(acc.Fields, "tenant_id") {
			set["tenant_id"] = tenant
		}
		e.insertIn(acc.Table, acc.Fields, a.ID, set, tenant)
	}
	e.system(func(tx pgx.Tx) error {
		for _, role := range a.Roles {
			if _, err := tx.Exec(context.Background(),
				`INSERT INTO ks_membership (id, tenant_id, actor_id, role) VALUES ($1, $2, $3, $4)`,
				ks.NewID(), tenant, a.ID, role); err != nil {
				return err
			}
		}
		return nil
	})
	return a
}

// insertIn writes one row: the given values, the framework columns, a fixture for every
// required reference (in the same tenant), and a placeholder for every other required
// field without a default.
func (e *Env) insertIn(table string, fields []model.Field, id ks.ID, set map[string]any, tenant ks.ID) {
	e.T.Helper()
	cols, args := e.rowValues(table, fields, id, set, tenant)
	e.exec(table, cols, args)
}

func (e *Env) rowValues(table string, fields []model.Field, id ks.ID, set map[string]any, tenant ks.ID) ([]string, []any) {
	e.T.Helper()
	cols := []string{"id"}
	args := []any{id}
	for _, f := range fields {
		switch f.Column {
		case "id", "created_at", "updated_at", "version":
			continue
		}
		if f.Column == "tenant_id" && !columnSet(set, "tenant_id") {
			continue
		}
		if f.Type.Kind == model.KindMoney && len(f.Columns) == 2 {
			minor, has := set[f.Columns[0]]
			if !has {
				if f.Optional || f.Default != nil {
					continue
				}
				minor = int64(1) // positive: money invariants usually ask for it
			}
			cols, args = append(cols, f.Columns[0], f.Columns[1]), append(args, minor, currency(f))
			continue
		}
		if v, ok := set[f.Column]; ok {
			cols, args = append(cols, f.Column), append(args, e.sealFixture(f.Type.Encrypted, table, f.Column, id, v))
			continue
		}
		if f.Optional || f.Default != nil {
			continue
		}
		if f.Ref != nil {
			cols, args = append(cols, f.Column), append(args, e.Fixture(f.Ref.Resource, tenant))
			continue
		}
		v, ok := placeholder(f, e.Clock.Now())
		if !ok {
			e.T.Fatalf("kstest: %s is required and has no placeholder; pass it explicitly", f.Column)
		}
		cols, args = append(cols, f.Column), append(args, e.sealFixture(f.Type.Encrypted, table, f.Column, id, v))
	}
	return cols, args
}

// sealFixture encrypts a fixture value for an Encrypted() column exactly as the runtime
// would, so actions read it back.
func (e *Env) sealFixture(encrypted bool, table, column string, id ks.ID, v any) any {
	if !encrypted || v == nil {
		return v
	}
	s := fmt.Sprint(v)
	if t, ok := v.(time.Time); ok {
		s = t.UTC().Format(time.RFC3339Nano)
	}
	sealed, err := e.App.Keys.Seal([]byte(s), table, column, string(id))
	if err != nil {
		e.T.Fatalf("kstest: seal %s.%s: %v", table, column, err)
	}
	return sealed
}

func columnSet(set map[string]any, col string) bool {
	_, ok := set[col]
	return ok
}

func insertSQL(table string, cols []string) (string, int) {
	params := make([]string, len(cols))
	quoted := make([]string, len(cols))
	for i := range cols {
		params[i] = fmt.Sprintf("$%d", i+1)
		quoted[i] = `"` + cols[i] + `"`
	}
	return fmt.Sprintf(`INSERT INTO "%s" (%s) VALUES (%s)`, table, strings.Join(quoted, ", "), strings.Join(params, ", ")), len(cols)
}

func (e *Env) exec(table string, cols []string, args []any) {
	e.T.Helper()
	sql, _ := insertSQL(table, cols)
	e.system(func(tx pgx.Tx) error {
		_, err := tx.Exec(context.Background(), sql, args...)
		return err
	})
}

func placeholder(f model.Field, now time.Time) (any, bool) {
	switch f.Type.Kind {
	case model.KindText:
		return "test " + f.Name, true
	case model.KindEmail:
		return strings.ToLower(string(ks.NewID()))[24:] + "@example.test", true
	case model.KindURL:
		return "https://example.test/", true
	case model.KindInt, model.KindDecimal:
		return 0, true
	case model.KindBool:
		return false, true
	case model.KindEnum:
		if len(f.Type.Values) > 0 {
			return f.Type.Values[0], true
		}
	case model.KindTimestamp:
		return now, true
	case model.KindDate:
		return now.Format("2006-01-02"), true
	case model.KindJSON:
		return "{}", true
	}
	return nil, false
}

func columnOf(fields []model.Field, col string) bool {
	for _, f := range fields {
		if f.Column == col {
			return true
		}
	}
	return false
}

func merge(ms []map[string]any) map[string]any {
	out := map[string]any{}
	for _, m := range ms {
		for k, v := range m {
			out[k] = v
		}
	}
	return out
}

// Create inserts a row as a fixture, bypassing access rules: the test states the world
// it starts from. Unset framework fields are filled in (a fresh id, version 1, the
// clock's time) and the row is returned as stored.
func Create[T any](e *Env, table *runtime.Table[T], row T) *T {
	e.T.Helper()
	v := reflect.ValueOf(&row).Elem()
	if id := v.FieldByName("ID"); id.IsValid() && id.String() == "" {
		id.SetString(string(ks.NewID()))
	}
	if ver := v.FieldByName("Version"); ver.IsValid() && ver.Int() == 0 {
		ver.SetInt(1)
	}
	for _, name := range []string{"CreatedAt", "UpdatedAt"} {
		if f := v.FieldByName(name); f.IsValid() && f.Interface().(time.Time).IsZero() {
			f.Set(reflect.ValueOf(e.Clock.Now()))
		}
	}
	if table.Tenant {
		if t := v.FieldByName("TenantID"); t.IsValid() && t.String() == "" {
			e.T.Fatalf("kstest: %s is tenant scoped; set TenantID", table.Name)
		}
	}
	var cols []string
	var args []any
	for i := 0; i < len(table.Columns); i++ {
		c := table.Columns[i]
		f := v.FieldByName(c.GoName)
		if !f.IsValid() || f.Kind() == reflect.Pointer {
			f = v.FieldByName(c.GoName + "ID") // a reference column: Owner -> OwnerID
		}
		if !f.IsValid() {
			continue
		}
		val, set := columnValue(f)
		if m, ok := val.(ks.Money); ok {
			// A money field is two adjacent columns: minor units, then currency.
			if i+1 < len(table.Columns) && table.Columns[i+1].GoName == c.GoName {
				cols, args = append(cols, c.Column, table.Columns[i+1].Column), append(args, m.Minor, m.Currency)
				i++
			}
			continue
		}
		if !set {
			continue // absent: the column's default applies
		}
		cols, args = append(cols, c.Column), append(args, e.sealFixture(c.Encrypted, table.Table, c.Column, ks.ID(v.FieldByName("ID").String()), val))
	}
	e.exec(table.Table, cols, args)
	e.invalidate(table.Ref, ks.ID(v.FieldByName("TenantID").String()))
	return &row
}

// invalidate bumps a resource's cache generation for a fixture written beside the
// runtime, as a write through it would (§11.3).
func (e *Env) invalidate(ref string, tenant ks.ID) {
	if tenant.IsNil() {
		return
	}
	if strings.HasPrefix(ref, ".") {
		ref = "app" + ref
	}
	e.system(func(tx pgx.Tx) error {
		_, err := tx.Exec(context.Background(), `INSERT INTO ks_cache_generation (id, tenant_id, resource, generation) VALUES ($1, $2, $3, 1)
			ON CONFLICT (tenant_id, resource) DO UPDATE SET generation = ks_cache_generation.generation + 1`, ks.NewID(), tenant, ref)
		return err
	})
}

// columnValue reads a struct field as a parameter; set is false for an absent ks.Opt.
func columnValue(f reflect.Value) (any, bool) {
	if p := f.MethodByName("Present"); p.IsValid() && f.MethodByName("Ptr").IsValid() {
		if !p.Call(nil)[0].Bool() {
			return nil, false
		}
		return columnValue(f.MethodByName("Ptr").Call(nil)[0].Elem())
	}
	switch f.Kind() {
	case reflect.String:
		if f.String() == "" && f.Type() == reflect.TypeOf(ks.ID("")) {
			return nil, false
		}
		return f.String(), true // ids, enums and text all travel as text
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return f.Int(), true
	}
	return f.Interface(), true
}

// Call runs an action through the full pipeline (authorization, validation,
// transaction, invariants, output filtering) as the actor, exactly as a request would.
// wrap is the generated adapter (gen.XHandler) and fn the implementation.
func Call[C, In, Out any](e *Env, actor ks.Actor, wrap func(func(*C, In) (Out, error)) *runtime.Handler, fn func(*C, In) (Out, error), in In) (Out, error) {
	e.T.Helper()
	var out Out
	body, err := json.Marshal(in)
	if err != nil {
		e.T.Fatalf("kstest: encode input: %v", err)
	}
	res, err := e.App.Run(context.Background(), wrap(fn), actor, &runtime.Call{Actor: actor, Body: body, Key: string(ks.NewID())}, nil)
	if err != nil {
		return out, err
	}
	if len(res.Body) > 0 {
		if err := json.Unmarshal(res.Body, &out); err != nil {
			e.T.Fatalf("kstest: decode output: %v", err)
		}
	}
	return out, nil
}

// QueryRow reads the database directly as the schema owner, across tenants: for
// asserting what is stored, below the access rules.
func (e *Env) QueryRow(sql string, args []any, dest ...any) {
	e.T.Helper()
	e.system(func(tx pgx.Tx) error { return tx.QueryRow(context.Background(), sql, args...).Scan(dest...) })
}

// Traces lists every action execution so far, oldest first.
func (e *Env) Traces() []runtime.Trace {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]runtime.Trace(nil), e.traces...)
}

// Shape is one captured statement, which ks perf explains against a seeded database
// (§11.4). Arguments are rendered as text so they replay as parameters.
type Shape struct {
	Action string   `json:"action"`
	SQL    string   `json:"sql"`
	Args   []string `json:"args"`
}

// capture appends this test's statement shapes, one JSON line per statement, so test
// binaries running at once can share the file.
func (e *Env) capture(path string) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	for _, tr := range e.Traces() {
		for _, st := range tr.Stmts {
			if strings.HasPrefix(st.SQL, "SELECT set_config") {
				continue
			}
			sh := Shape{Action: tr.Action, SQL: st.SQL}
			for _, a := range st.Args {
				sh.Args = append(sh.Args, argText(a))
			}
			line, _ := json.Marshal(sh)
			_, _ = f.Write(append(line, '\n'))
		}
	}
}

func argText(a any) string {
	switch v := a.(type) {
	case []byte:
		if len(v) == 16 {
			return fmt.Sprintf("%x-%x-%x-%x-%x", v[0:4], v[4:6], v[6:8], v[8:10], v[10:16])
		}
		return string(v)
	case time.Time:
		return v.UTC().Format(time.RFC3339Nano)
	case *string:
		if v != nil {
			return *v
		}
	case nil:
		return ""
	}
	return fmt.Sprint(a)
}

// LastTrace is what the most recent action execution did.
func (e *Env) LastTrace() runtime.Trace {
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.traces) == 0 {
		e.T.Fatal("kstest: no action has run")
	}
	return e.traces[len(e.traces)-1]
}

// AssertQueriesAtMost fails the test when the last action issued more statements.
func (e *Env) AssertQueriesAtMost(n int) {
	e.T.Helper()
	if t := e.LastTrace(); len(t.Stmts) > n {
		var sqls []string
		for _, s := range t.Stmts {
			sqls = append(sqls, s.SQL)
		}
		e.T.Fatalf("%s issued %d queries, at most %d expected:\n  %s", t.Action, len(t.Stmts), n, strings.Join(sqls, "\n  "))
	}
}

// Effects lists every effect committed so far: deferred ones from the outbox, in order,
// then immediate ones the recording providers received. A rolled-back action's
// deferred effects are not here, because they never reached the outbox (§10.1).
func (e *Env) Effects() []runtime.EffectCall {
	e.T.Helper()
	var out []runtime.EffectCall
	e.system(func(tx pgx.Tx) error {
		rows, err := tx.Query(context.Background(),
			`SELECT id::text, coalesce(tenant_id::text, ''), action, seq, type, mode, coalesce(template, ''),
			        coalesce(topic, ''), coalesce(bucket, ''), coalesce(hosts, '{}'), idempotency_key, payload
			   FROM ks_outbox ORDER BY created_at, invocation_id, seq`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var c runtime.EffectCall
			var kind string
			var payload []byte
			if err := rows.Scan(&c.ID, &c.Tenant, &c.Action, &c.Seq, &kind, &c.Mode, &c.Template,
				&c.Topic, &c.Bucket, &c.Hosts, &c.IdempotencyKey, &payload); err != nil {
				return err
			}
			c.Type, c.Payload = model.EffectKind(kind), payload
			out = append(out, c)
		}
		return rows.Err()
	})
	e.mu.Lock()
	out = append(out, e.sent...)
	e.mu.Unlock()
	return out
}

// Emails lists the committed email effects.
func (e *Env) Emails() []runtime.EffectCall { return e.ofKind(model.EffectEmail) }

func (e *Env) ofKind(k model.EffectKind) []runtime.EffectCall {
	var out []runtime.EffectCall
	for _, c := range e.Effects() {
		if c.Type == k {
			out = append(out, c)
		}
	}
	return out
}

// StubHTTP declares the response an immediate HTTP effect to host receives. A request
// to a host without a stub fails, so a test can never reach the network (§16.1).
func (e *Env) StubHTTP(host string, body json.RawMessage) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.stubs[host] = runtime.EffectResult{Body: body}
}

// recorder is the fake provider every effect kind gets in a test.
type recorder struct {
	env  *Env
	kind model.EffectKind
}

func (r recorder) Name() string         { return "kstest-" + string(r.kind) }
func (r recorder) Retryable(error) bool { return false }
func (r recorder) Execute(_ context.Context, c runtime.EffectCall) (runtime.EffectResult, error) {
	r.env.mu.Lock()
	defer r.env.mu.Unlock()
	if r.kind == model.EffectHTTP {
		for _, h := range c.Hosts {
			if res, ok := r.env.stubs[h]; ok {
				r.env.sent = append(r.env.sent, c)
				return res, nil
			}
		}
		return runtime.EffectResult{}, fmt.Errorf("kstest: HTTP effect to %v has no stub; declare one with env.StubHTTP", c.Hosts)
	}
	r.env.sent = append(r.env.sent, c)
	return runtime.EffectResult{}, nil
}
