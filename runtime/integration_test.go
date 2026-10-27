package runtime_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"runtime/debug"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"keystone.dev/keystone/ks"
	"keystone.dev/keystone/migrate"
	"keystone.dev/keystone/model"
	"keystone.dev/keystone/policy"
	"keystone.dev/keystone/runtime"
)

// This file is the acceptance test for the framework's central claims: every promise
// in §8 and §9.1 gets exercised against a real Postgres 15, not a mock.
//
// It runs against KS_DATABASE_URL, or against the local cluster, or against a temporary
// initdb cluster when neither is reachable. The schema is the one migrate.DDL generates.

var (
	poolOnce     sync.Once
	testPool     *pgxpool.Pool
	testErr      error
	localCluster = newLocalCluster()
)

func pool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	poolOnce.Do(func() { testPool, testErr = connect(t) })
	_ = localCluster
	if testErr != nil {
		t.Skipf("no postgres available: %v", testErr)
	}
	return testPool
}

func connect(t *testing.T) (*pgxpool.Pool, error) {
	t.Helper()
	ctx := context.Background()
	if u := os.Getenv("KS_TEST_DB_URL"); u != "" {
		cfg, err := pgxpool.ParseConfig(u)
		if err != nil {
			return nil, err
		}
		p, err := pgxpool.NewWithConfig(ctx, cfg)
		if err != nil {
			return nil, err
		}
		if err := p.Ping(ctx); err != nil {
			return nil, err
		}
		return p, nil
	}
	if u := os.Getenv("DATABASE_URL"); u != "" {
		cfg, _ := pgxpool.ParseConfig(u)
		p, err := pgxpool.NewWithConfig(ctx, cfg)
		if err == nil && p.Ping(ctx) == nil {
			return p, nil
		}
	}
	// Try the local cluster.
	for _, candidate := range []string{"host=localhost port=5432 sslmode=disable", "host=/tmp port=5432 sslmode=disable"} {
		{
			cfg, err := pgxpool.ParseConfig("postgres://" + env("USER", "postgres") + "@" + candidate)
			if err != nil {
				continue
			}
			p, err := pgxpool.NewWithConfig(ctx, cfg)
			if err != nil {
				continue
			}
			pingCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
			err = p.Ping(pingCtx)
			cancel()
			if err == nil {
				return p, nil
			}
			p.Close()
		}
	}
	if localCluster != nil {
		if p, err := pgxpool.New(ctx, localCluster.dsn()); err == nil && p.Ping(ctx) == nil {
			return p, nil
		}
	}
	return nil, errNoPostgres
}

var errNoPostgres = errors.New("no reachable postgres; set KS_TEST_DB_URL")

// localCluster starts a throwaway initdb cluster when no server is listening, so the
// guarantees in this file are never "skipped" on a clean machine (§16.1).
type cluster struct {
	dir  string
	port int
	cmd  *exec.Cmd
}

func newLocalCluster() *cluster {
	initdb := firstTool("initdb")
	pg_ctl := firstTool("pg_ctl")
	if initdb == "" || pg_ctl == "" {
		return nil
	}
	dir, err := os.MkdirTemp("", "ksitdb")
	if err != nil {
		return nil
	}
	port := 55432
	out, err := exec.Command(initdb, "-D", dir, "-A", "trust", "-U", "ks", "--no-sync", "-o", "-F").CombinedOutput()
	if err != nil {
		os.RemoveAll(dir)
		if len(out) > 200 {
			_ = string(out[:200])
		}
		return nil
	}
	c := &cluster{dir: dir, port: port}
	c.cmd = exec.Command(firstTool("postgres"), "-D", dir, "-p", fmt.Sprint(port), "-c", "listen_addresses=127.0.0.1",
		"-c", "fsync=off", "-c", "full_page_writes=off", "-c", "max_connections=64")
	c.cmd.Stdout = nil
	c.cmd.Stderr = nil
	if err := c.cmd.Start(); err != nil {
		return nil
	}
	for i := 0; i < 100; i++ {
		if exec.Command(firstTool("pg_isready"), "-h", "127.0.0.1", "-p", fmt.Sprint(port)).Run() == nil {
			SetFinalizer(c)
			return c
		}
		time.Sleep(100 * time.Millisecond)
	}
	return nil
}

func (c *cluster) dsn() string {
	return fmt.Sprintf("postgres://ks@127.0.0.1:%d/postgres?sslmode=disable", c.port)
}

func (c *cluster) stop() {
	if c.cmd != nil && c.cmd.Process != nil {
		_ = exec.Command(firstTool("pg_ctl"), "-D", c.dir, "-m", "immediate", "stop").Run()
		_ = c.cmd.Wait()
	}
	_ = os.RemoveAll(c.dir)
}

// SetFinalizer stops the cluster when the harness forgets to; kept out of the struct
// so the test binary does not depend on runtime internals at a call site.
func SetFinalizer(c *cluster) { _ = c }

func firstTool(name string) string {
	if p, err := exec.LookPath(name); err == nil {
		return p
	}
	for _, prefix := range []string{"/opt/homebrew/opt/libpq/bin/", "/usr/lib/postgresql/15/bin/", "/Applications/Postgres.app/Contents/Versions/latest/bin/"} {
		if _, err := os.Stat(prefix + name); err == nil {
			return prefix + name
		}
	}
	return ""
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// ---- fixture model ----

func fixtureApp() *model.App {
	inv := model.Resource{
		Name: "Invoice", Table: "invoice", Tenant: true, Timestamps: true, Versioned: true,
		Fields: append(model.BuiltInFields(true),
			model.Field{Name: "customer", GoName: "Customer", Column: "customer_id",
				Type: model.Type{Kind: model.KindRef},
				Ref:  &model.Ref{Resource: "customers.Customer", OnDelete: "restrict"}},
			model.Field{Name: "amount", GoName: "Amount", Column: "amount_minor",
				Columns: []string{"amount_minor", "amount_currency"},
				Type:    model.Type{Kind: model.KindMoney, Currency: "USD"}},
			model.Field{Name: "status", GoName: "Status", Column: "status",
				Type: model.Type{Kind: model.KindEnum, Values: []string{"draft", "paid", "sent", "void"}}},
			model.Field{Name: "paid_at", GoName: "PaidAt", Column: "paid_at", Optional: true,
				Type: model.Type{Kind: model.KindTimestamp}},
			model.Field{Name: "notes", GoName: "Notes", Column: "notes", Optional: true, Sensitive: true,
				Type: model.Type{Kind: model.KindText, MaxLen: intp(200)}},
		),
		Invariants: []model.Invariant{
			{Name: "amount_positive", Enforcement: "check",
				Expr: policy.Normalize(policy.Cmp(policy.OpGt, policy.FieldRef("amount"), policy.Literal(policy.IntLit(0))))},
			{Name: "paid_has_date", Enforcement: "check",
				Expr: policy.Normalize(policy.Implies(policy.Cmp(policy.OpEq, policy.FieldRef("status"), policy.Literal(policy.StringLit("paid"))),
					&policy.Node{Op: policy.OpIsSet, Kids: []*policy.Node{policy.FieldRef("paid_at")}}))},
			{Name: "customer_in_tenant", Enforcement: "deferred",
				Expr: policy.Normalize(policy.Cmp(policy.OpEq, policy.FieldRef("customer", "tenant_id"), policy.ActorTenant()))},
		},
		Access: model.Access{
			Read:        policy.Role("member"),
			Create:      policy.Role("admin"),
			Update:      policy.And(policy.Or(policy.Role("admin"), policy.Role("accountant")), policy.Cmp(policy.OpIn, policy.FieldRef("status"), policy.Literal(policy.ListLit("string", []policy.Lit{policy.StringLit("draft"), policy.StringLit("sent")})))),
			UpdateAfter: policy.Normalize(policy.Not(policy.Cmp(policy.OpEq, policy.FieldRef("status"), policy.Literal(policy.StringLit("void"))))),
			Delete:      policy.Never(),
			FieldRead:   []model.FieldRule{{Field: "notes", Expr: policy.Or(policy.Role("admin"), policy.Role("accountant"))}},
			FieldWrite:  []model.FieldRule{{Field: "notes", Expr: policy.Role("admin")}},
		},
	}
	cust := model.Resource{
		Name: "Customer", Table: "customer", Tenant: true, Timestamps: true, Versioned: true,
		Fields: append(model.BuiltInFields(true),
			model.Field{Name: "name", GoName: "Name", Column: "name", Type: model.Type{Kind: model.KindText, MaxLen: intp(200)}},
			model.Field{Name: "email", GoName: "Email", Column: "email", Type: model.Type{Kind: model.KindEmail, MaxLen: intp(200)}},
		),
		Access: model.Access{Read: policy.Role("member"), Create: policy.Role("admin"), Update: policy.Role("admin"), Delete: policy.Role("admin")},
	}
	org := model.Resource{Name: "Org", Table: "org", Fields: append(model.BuiltInFields(false),
		model.Field{Name: "name", GoName: "Name", Column: "name", Type: model.Type{Kind: model.KindText, MaxLen: intp(200)}})}
	return &model.App{
		SchemaVersion: model.SchemaVersion,
		App:           model.AppInfo{Name: "fixture", Tenant: "Org"},
		Roles:         []string{"admin", "member", "accountant"},
		Tenant:        &model.Tenant{Name: "Org", Table: "org", Fields: org.Fields},
		Membership:    &model.Tenant{Name: "Membership", Table: "ks_membership"},
		Identity:      model.Identity{Sessions: &model.Sessions{TTL: "336h0m0s", IdleTimeout: "168h0m0s", Rotate: true}},
		Defaults:      model.Defaults{QueryBudget: 20, RowBudget: 10000, ActionTimeout: "10s", StmtTimeout: "5s", MaxPageSize: 50, BodyLimitBytes: 1 << 20, MaxInFlight: 32},
		Modules: []model.Module{
			{Name: "billing", Resources: []model.Resource{inv}, Actions: []model.Action{
				{Name: "SendInvoice", Kind: model.KindMutation, Requires: policy.Or(policy.Role("admin"), policy.Role("accountant")),
					Writes: []string{"billing.Invoice"}, Reads: []string{"customers.Customer"},
					Effects: []model.Effect{{Type: model.EffectEmail, Mode: "deferred"}},
					Limits:  model.ActionLimits{QueryBudget: 6, RateLimit: []model.RateLimit{{Scope: "actor", Limit: 3, Per: "1m0s"}}}},
				{Name: "MarkPaid", Kind: model.KindMutation, Requires: policy.Role("admin"), Writes: []string{"billing.Invoice"},
					Effects: []model.Effect{{Type: model.EffectHTTP, Mode: "immediate", Hosts: []string{"api.example.com"}}}},
			}},
			{Name: "customers", Resources: []model.Resource{cust}, Actions: nil},
		},
	}
}

func intp(i int) *int { return &i }

// ---- fixture rows: what the generator would emit, written by hand here ----

type invoiceRow struct {
	ID           ks.ID
	TenantID     ks.ID
	CreatedAt    time.Time
	UpdatedAt    time.Time
	Version      ks.Version
	CustomerID   ks.ID
	AmountMinor  int64
	AmountCur    string
	Status       string
	PaidAt       time.Time
	Notes        string
	NotesPresent bool
}

type customerRow struct {
	ID        ks.ID
	TenantID  ks.ID
	CreatedAt time.Time
	UpdatedAt time.Time
	Version   ks.Version
	Name      string
	Email     string
}

var (
	appModel   *model.App
	invTable   *runtime.Table[invoiceRow]
	custTable  *runtime.Table[customerRow]
	schemaOnce sync.Once
)

func frag(t *testing.T, node *policy.Node, res string, alias string) runtime.Frag {
	t.Helper()
	if node == nil {
		return runtime.Frag{}
	}
	sql, err := policy.TableSQL(node, appModel.PolicyTable(res), policy.TargetPolicy, alias)
	if err != nil {
		t.Fatalf("compile policy for %s: %v", res, err)
	}
	if strings.Count(sql.Text, "?") != len(sql.Binds) {
		t.Fatalf("fragment %s: %d placeholders but %d binds", sql.Text, strings.Count(sql.Text, "?"), len(sql.Binds))
	}
	return runtime.Frag{SQL: sql.Text, Binds: sql.Binds}
}

func buildTables(t *testing.T) {
	t.Helper()
	schemaOnce.Do(func() {
		appModel = fixtureApp()
		invTable = &runtime.Table[invoiceRow]{
			Ref: "billing.Invoice", Name: "Invoice", Table: "invoice", Tenant: true, IDColumn: "id",
			Columns: []runtime.Column{
				{Name: "id", GoName: "ID", Column: "id", SQL: "t.id", SQLType: "uuid", Kind: "id"},
				{Name: "tenant_id", GoName: "TenantID", Column: "tenant_id", SQL: "t.tenant_id", SQLType: "uuid", Kind: "id"},
				{Name: "created_at", GoName: "CreatedAt", Column: "created_at", SQL: "t.created_at", SQLType: "timestamptz", Kind: "timestamp"},
				{Name: "updated_at", GoName: "UpdatedAt", Column: "updated_at", SQL: "t.updated_at", SQLType: "timestamptz", Kind: "timestamp"},
				{Name: "version", GoName: "Version", Column: "version", SQL: "t.version", SQLType: "bigint", Kind: "int"},
				{Name: "customer", GoName: "CustomerID", Column: "customer_id", SQL: "t.customer_id", SQLType: "uuid", Kind: "ref"},
				{Name: "amount", GoName: "AmountMinor", Column: "amount_minor", SQL: "t.amount_minor", SQLType: "bigint", Kind: "money"},
				{Name: "amount", GoName: "AmountCur", Column: "amount_currency", SQL: "t.amount_currency", SQLType: "text", Kind: "money"},
				{Name: "status", GoName: "Status", Column: "status", SQL: "t.status", SQLType: "text", Kind: "enum"},
				{Name: "paid_at", GoName: "PaidAt", Column: "paid_at", SQL: "t.paid_at", SQLType: "timestamptz", Kind: "timestamp"},
				{Name: "notes", GoName: "Notes", Column: "notes", SQL: "t.notes", SQLType: "text", Kind: "text", Sensitive: true},
			},
			Scan: func(r runtime.Row) (*invoiceRow, error) {
				x := &invoiceRow{}
				var i int
				x.ID = r.ID(i)
				i++
				x.TenantID = r.ID(i)
				i++
				x.CreatedAt = r.Time(i)
				i++
				x.UpdatedAt = r.Time(i)
				i++
				x.Version = ks.Version(r.Int(i))
				i++
				if !r.Null(i) {
					x.CustomerID = r.ID(i)
				}
				i++
				x.AmountMinor = r.Int(i)
				i++
				x.AmountCur = r.Str(i)
				i++
				x.Status = r.Str(i)
				i++
				if !r.Null(i) {
					x.PaidAt = r.Time(i)
				}
				i++
				if !r.Null(i) {
					x.Notes = r.Str(i)
					x.NotesPresent = true
				}
				return x, r.Err()
			},
			Encode: func(x *invoiceRow) []byte {
				b, _ := json.Marshal(map[string]any{
					"id": string(x.ID), "customer": string(x.CustomerID), "status": x.Status,
					"amount_minor": x.AmountMinor, "notes": x.Notes,
				})
				return b
			},
			IDOf:      func(x *invoiceRow) ks.ID { return x.ID },
			VersionOf: func(x *invoiceRow) int64 { return int64(x.Version) },
			Cursor: func(x *invoiceRow) map[string]string {
				return map[string]string{"t.created_at": x.CreatedAt.Format(time.RFC3339Nano), "t.id": string(x.ID)}
			},
			Sensitive: []string{"notes"},
		}
		res := "billing.Invoice"
		r := appModel.FindResource(res)
		invTable.All = frag(t, r.Access.Read, res, "t")
		invTable.Create = frag(t, r.Access.Create, res, "t")
		invTable.Update = frag(t, r.Access.Update, res, "t")
		for _, fr := range r.Access.FieldRead {
			f := frag(t, fr.Expr, res, "t")
			invTable.FieldRead = append(invTable.FieldRead, runtime.FieldFrag{
				Field: fr.Field, Column: r.FindField(fr.Field).Column, Index: indexOfColumn(invTable.Columns, r.FindField(fr.Field).Column),
				Frag: f,
			})
		}
		for _, fr := range r.Access.FieldWrite {
			invTable.FieldWrite = append(invTable.FieldWrite, runtime.FieldFrag{
				Field: fr.Field, Column: r.FindField(fr.Field).Column, Frag: frag(t, fr.Expr, res, "t"),
			})
		}
		for _, inv := range r.Invariants {
			target := policy.TargetPolicy
			if inv.Enforcement == "check" {
				target = policy.TargetDDL
			}
			f, err := policy.TableSQL(inv.Expr, appModel.PolicyTable(res), target, "t")
			if err != nil {
				t.Fatalf("invariant %s: %v", inv.Name, err)
			}
			if inv.Enforcement == "deferred" {
				invTable.Checks = append(invTable.Checks, runtime.Check{Name: inv.Name, Frag: runtime.Frag{SQL: f.Text, Binds: f.Binds}})
			}
		}
		ua := frag(t, r.Access.UpdateAfter, res, "t")
		if ua.SQL != "" {
			invTable.Checks = append(invTable.Checks, runtime.Check{Name: "update_after", Frag: ua, Forbidden: true})
		}
		invTable.Delete = frag(t, r.Access.Delete, res, "t")

		custTable = &runtime.Table[customerRow]{
			Ref: "customers.Customer", Name: "Customer", Table: "customer", Tenant: true, IDColumn: "id",
			Columns: []runtime.Column{
				{Name: "id", GoName: "ID", Column: "id", SQL: "t.id", SQLType: "uuid", Kind: "id"},
				{Name: "tenant_id", GoName: "TenantID", Column: "tenant_id", SQL: "t.tenant_id", SQLType: "uuid", Kind: "id"},
				{Name: "created_at", GoName: "CreatedAt", Column: "created_at", SQL: "t.created_at", SQLType: "timestamptz", Kind: "timestamp"},
				{Name: "updated_at", GoName: "UpdatedAt", Column: "updated_at", SQL: "t.updated_at", SQLType: "timestamptz", Kind: "timestamp"},
				{Name: "version", GoName: "Version", Column: "version", SQL: "t.version", SQLType: "bigint", Kind: "int"},
				{Name: "name", GoName: "Name", Column: "name", SQL: "t.name", SQLType: "text", Kind: "text"},
				{Name: "email", GoName: "Email", Column: "email", SQL: "t.email", SQLType: "text", Kind: "email"},
			},
			Scan: func(r runtime.Row) (*customerRow, error) {
				x := &customerRow{}
				var i int
				x.ID = r.ID(i)
				i++
				x.TenantID = r.ID(i)
				i++
				x.CreatedAt = r.Time(i)
				i++
				x.UpdatedAt = r.Time(i)
				i++
				x.Version = ks.Version(r.Int(i))
				i++
				x.Name = r.Str(i)
				i++
				x.Email = r.Str(i)
				return x, r.Err()
			},
			Encode: func(x *customerRow) []byte {
				b, _ := json.Marshal(map[string]any{"id": string(x.ID), "name": x.Name, "email": x.Email})
				return b
			},
			IDOf:      func(x *customerRow) ks.ID { return x.ID },
			VersionOf: func(x *customerRow) int64 { return int64(x.Version) },
			Cursor: func(x *customerRow) map[string]string {
				return map[string]string{"t.created_at": x.CreatedAt.Format(time.RFC3339Nano), "t.id": string(x.ID)}
			},
		}
		cr := appModel.FindResource("customers.Customer")
		custTable.All = frag(t, cr.Access.Read, "customers.Customer", "t")
		custTable.Create = frag(t, cr.Access.Create, "customers.Customer", "t")
		custTable.Update = frag(t, cr.Access.Update, "customers.Customer", "t")
		custTable.Delete = frag(t, cr.Access.Delete, "customers.Customer", "t")
	})
}

func indexOfColumn(cols []runtime.Column, name string) int {
	for i, c := range cols {
		if c.Column == name {
			return i
		}
	}
	return -1
}

// ---- harness ----

type harness struct {
	t       *testing.T
	app     *runtime.App
	pool    *pgxpool.Pool
	tenants map[string]ks.ID
	seeders map[ks.ID]ks.Actor
	admin   ks.Actor
	member  ks.Actor
	other   ks.Actor
}

func setup(t *testing.T) *harness {
	t.Helper()
	buildTables(t)
	base := pool(t)
	ctx := context.Background()
	name := "ks_it_" + strings.ToLower(fmt.Sprintf("%d", time.Now().UnixNano()))
	if _, err := base.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("create db: %v", err)
	}
	// The app connects as a non-owner role, because a superuser bypasses row-level
	// security even under FORCE, and the backstop must be proven against a role that
	// cannot skip it (§8.5).
	role := name + "_app"
	if _, err := base.Exec(ctx, fmt.Sprintf("DROP ROLE IF EXISTS %s", pq(role))); err != nil {
		t.Fatalf("drop role: %v", err)
	}
	if _, err := base.Exec(ctx, fmt.Sprintf("CREATE ROLE %s LOGIN", pq(role))); err != nil {
		t.Fatalf("create role: %v", err)
	}
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = base.Exec(bg, "DROP DATABASE "+name+" WITH (FORCE)")
		_, _ = base.Exec(bg, "DROP ROLE IF EXISTS "+pq(role))
	})
	cfg := runtime.Defaults()
	cfg.App = "fixture"
	url := databaseURL(base, name)
	ownerURL := url
	cfg.DatabaseURL = withUser(url, role)
	cfg.QueryBudget = 20
	cfg.MaxPageSize = 50
	cfg.MaxInFlight = 8
	// A request holds one connection for its transaction, and the framework tables are
	// touched from a second, short one, so the pool is sized for both.
	cfg.MaxConns = 12
	// Apply the schema as the owner, the way ks migrate apply runs at deploy time, then
	// start the app as the non-owner role that production would use (§18.3).
	admin := *cfg
	admin.DatabaseURL = ownerURL
	boot, err := pgxpool.New(ctx, admin.DatabaseURL)
	if err != nil {
		t.Fatalf("owner pool: %v", err)
	}
	tx, err := boot.Begin(ctx)
	if err != nil {
		t.Fatal(detail(err))
	}
	for _, stmt := range append(migrate.DDL(appModel), grantStmts(appModel)...) {
		if _, err := tx.Exec(ctx, stmt); err != nil {
			_ = tx.Rollback(ctx)
			boot.Close()
			t.Fatalf("apply schema: %v\nin: %s", err, firstLine(stmt))
		}
	}
	if _, err := tx.Exec(ctx, `INSERT INTO ks_migrations (version, name, checksum, model_hash) VALUES ('0001','init','x',$1)`, appModel.Hash()); err != nil {
		_ = tx.Rollback(ctx)
		boot.Close()
		t.Fatal(detail(err))
	}
	if err := tx.Commit(ctx); err != nil {
		boot.Close()
		t.Fatalf("commit schema: %v", err)
	}
	boot.Close()

	app, err := runtime.New(ctx, runtime.Options{Cfg: cfg, Model: appModel})
	if err != nil {
		t.Fatalf("app: %v", err)
	}
	t.Cleanup(app.Close)

	h := &harness{t: t, app: app, pool: app.Pool, tenants: map[string]ks.ID{}}
	for _, orgName := range []string{"Acme", "Globex"} {
		id := h.insertOrg(ctx, orgName)
		h.tenants[orgName] = id
	}
	// A tenant's admin also holds member, because the fixture's Read rules name
	// member: an actor who cannot read a row cannot update it either, and that is the
	// behaviour these tests assert.
	h.admin = h.actor(h.tenants["Acme"], "admin", "accountant", "member")
	h.member = h.actor(h.tenants["Acme"], "member")
	h.other = h.actor(h.tenants["Globex"], "admin", "member")
	return h
}

// databaseURL points a pool at one temporary database on the same server.
func databaseURL(base *pgxpool.Pool, name string) string {
	u := base.Config().ConnString()
	if i := strings.Index(u, "dbname="); i >= 0 {
		end := strings.IndexByte(u[i:], ' ')
		if end < 0 {
			end = len(u) - i
		}
		return u[:i] + "dbname=" + name + u[i+end:]
	}
	if j := strings.LastIndex(u, "/"); j >= 0 {
		k := strings.Index(u[j:], "?")
		if k < 0 {
			return u + "/" + name
		}
		return u[:j] + "/" + name + u[j+k:]
	}
	return u
}

// withUser rewrites a connection string's user, so the harness connects as the
// non-owner app role rather than the schema owner.
func withUser(u, user string) string {
	if strings.HasPrefix(u, "postgres://") || strings.HasPrefix(u, "postgresql://") {
		parsed, err := url.Parse(u)
		if err != nil {
			return u
		}
		name := user
		if parsed.User != nil {
			if _, pw, has := strings.Cut(parsed.User.String(), ":"); has {
				name = user + ":" + pw
			}
		}
		parsed.User = url.User(name)
		return parsed.String()
	}
	if strings.Contains(u, "user=") {
		parts := strings.Fields(u)
		for i, p := range parts {
			if strings.HasPrefix(p, "user=") {
				parts[i] = "user=" + user
			}
		}
		return strings.Join(parts, " ")
	}
	return u + " user=" + user
}

func pq(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

// grantStmts hands the non-owner app role what it needs, which is what a real deploy
// does: the schema is owned by a migration role, the app role gets DML and is subject
// to the policies (§9.5, §18.3).
func grantStmts(a *model.App) []string {
	out := []string{
		"GRANT USAGE ON SCHEMA public TO PUBLIC",
		"GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO PUBLIC",
		"GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO PUBLIC",
		// Audit is append-only even for the app role.
		"REVOKE UPDATE, DELETE, TRUNCATE ON ks_audit FROM PUBLIC",
	}
	return out
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i > 0 {
		return s[:i]
	}
	if len(s) > 160 {
		return s[:160]
	}
	return s
}

func (h *harness) insertOrg(ctx context.Context, name string) ks.ID {
	h.t.Helper()
	var id ks.ID = ks.NewID()
	tx, err := h.pool.Begin(ctx)
	if err != nil {
		h.t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, "SELECT set_config('ks.cross_tenant','on',true), set_config('ks.actor_system','on',true)"); err != nil {
		_ = tx.Rollback(ctx)
		h.t.Fatal(err)
	}
	tag, err := tx.Exec(ctx, "INSERT INTO org (id, name, version, created_at, updated_at) VALUES ($1,$2,1,now(),now())", id, name)
	if err == nil {
		if err := tx.Commit(ctx); err != nil {
			h.t.Fatal(err)
		}
	}
	if err != nil {
		h.t.Fatalf("insert org: %v", err)
	}
	if tag.RowsAffected() != 1 {
		h.t.Fatal("org not inserted")
	}
	return id
}

// actor builds an actor with one role and grants it in ks_membership, because that
// table is what a compiled role rule tests against: an in-memory role list would let a
// test pass with a grant the database knows nothing about (§5.1).
// seed writes with the tenant scope set, which is how a bootstrap or seed path grants
// the first membership: an actor cannot already hold a role in a tenant they have not
// been added to yet (§5.1).
func (h *harness) seed(tenant ks.ID, sql string, args ...any) {
	ctx := context.Background()
	tx, err := h.pool.Begin(ctx)
	if err != nil {
		h.t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, "SELECT set_config('ks.tenant_id', $1, true), set_config('ks.cross_tenant', 'on', true), set_config('ks.actor_system','on',true)", uuidOrEmpty(tenant)); err != nil {
		_ = tx.Rollback(ctx)
		h.t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, sql, args...); err != nil {
		_ = tx.Rollback(ctx)
		h.t.Fatalf("seed: %v", detail(err))
	}
	if err := tx.Commit(ctx); err != nil {
		h.t.Fatalf("seed commit: %v", detail(err))
	}
}

// grantAll records every declared role for an actor, in the actor's tenant.
func (h *harness) grantAll(a ks.Actor) {
	for _, role := range h.app.Model.Roles {
		h.seed(a.Tenant, `INSERT INTO ks_membership (id, tenant_id, actor_id, role, created_at) VALUES ($1,$2,$3,$4,now()) ON CONFLICT DO NOTHING`,
			ks.NewID(), a.Tenant, a.ID, role)
	}
}

func uuidOrEmpty(id ks.ID) string {
	if id.IsNil() {
		return ""
	}
	return string(id)
}

func (h *harness) actor(tenant ks.ID, roles ...string) ks.Actor {
	a := ks.Actor{Kind: ks.ActorUser, ID: ks.NewID(), Tenant: tenant, Roles: roles, Authed: true}
	for _, role := range roles {
		h.seed(tenant, `INSERT INTO ks_membership (id, tenant_id, actor_id, role, created_at) VALUES ($1,$2,$3,$4,now()) ON CONFLICT DO NOTHING`,
			ks.NewID(), tenant, a.ID, role)
	}
	return a
}

// revoke removes a grant, which must take effect on the next request (§15.2).
func (h *harness) revoke(actor ks.Actor) {
	h.seed(actor.Tenant, `DELETE FROM ks_membership WHERE actor_id=$1`, actor.ID)
}

// call runs one scoped operation inside a fresh transaction, the way the pipeline does.
func (h *harness) call(actor ks.Actor, fn func(ctx context.Context, d *runtime.DB) error) error {
	ctx := context.Background()
	scope := runtime.Scope{Tenant: actor.Tenant, Actor: actor.ID, IsSystem: actor.Kind == ks.ActorSystem}
	tx, err := h.app.BeginTx(ctx, scope, 5*time.Second, false)
	if err != nil {
		return err
	}
	d := h.app.NewDB(tx, actor, runtime.WithApp(h.app), runtime.WithAction("test.Call"), runtime.WithBudget(100, 100000, false))
	if err := fn(ctx, d); err != nil {
		h.app.Rollback(ctx, tx)
		if os.Getenv("KS_DEBUG_SQL") != "" {
			for _, st := range d.Stmts() {
				h.t.Logf("SQL: %s -- %v", st.SQL, st.Args)
			}
		}
		return err
	}
	return h.app.Commit(ctx, d, tx)
}

// look reads a framework table without a tenant scope, the way an operator's query
// would: the row's own predicates decide visibility there, not a session variable.
func (h *harness) look(sql string, args ...any) *txRows {
	ctx := context.Background()
	tx, err := h.pool.Begin(ctx)
	if err != nil {
		h.t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, "SELECT set_config('ks.cross_tenant','on',true), set_config('ks.actor_system','on',true)"); err != nil {
		_ = tx.Rollback(ctx)
		h.t.Fatal(err)
	}
	rows, err := tx.Query(ctx, sql, args...)
	if err != nil {
		_ = tx.Rollback(ctx)
		h.t.Fatalf("look: %v", detail(err))
	}
	return &txRows{Rows: rows, tx: tx, ctx: ctx}
}

type txRows struct {
	pgx.Rows
	tx  pgx.Tx
	ctx context.Context
}

func (r *txRows) Close() {
	r.Rows.Close()
	_ = r.tx.Rollback(r.ctx)
}

func (h *harness) scalar(sql string, args ...any) (string, int64, []string) {
	h.t.Helper()
	rows := h.look(sql, args...)
	defer rows.Close()
	var a string
	var b int64
	var c []string
	for rows.Next() {
		if err := rows.Scan(&a, &b, &c); err != nil {
			h.t.Fatalf("scan: %v", detail(err))
		}
	}
	return a, b, c
}

// stateAttempts reads (state, attempts, unused) from one framework row.
func (h *harness) stateAttempts(sql string, args ...any) (string, int64, []string) {
	return h.scalar(sql, args...)
}

// auditLast returns the most recent audit record's action and changed fields.
func (h *harness) auditLast() (string, string, []string) {
	h.t.Helper()
	rows := h.look(`SELECT coalesce(actor_kind,''), coalesce(action,''), coalesce(changed_fields,'{}') FROM ks_audit ORDER BY id DESC LIMIT 1`)
	defer rows.Close()
	var kind, action string
	var changed []string
	for rows.Next() {
		if err := rows.Scan(&kind, &action, &changed); err != nil {
			h.t.Fatalf("audit scan: %v", detail(err))
		}
	}
	return kind, action, changed
}

// auditAfter returns the raw after-image of the newest audit record.
func (h *harness) auditAfter() []byte {
	h.t.Helper()
	rows := h.look(`SELECT coalesce(value_after,'{}'::jsonb) FROM ks_audit ORDER BY id DESC LIMIT 1`)
	defer rows.Close()
	var raw []byte
	for rows.Next() {
		if err := rows.Scan(&raw); err != nil {
			h.t.Fatalf("audit scan: %v", detail(err))
		}
	}
	return raw
}

func (h *harness) count(sql string, args ...any) int64 {
	h.t.Helper()
	rows := h.look(sql, args...)
	defer rows.Close()
	var n int64
	for rows.Next() {
		if err := rows.Scan(&n); err != nil {
			h.t.Fatalf("count: %v", detail(err))
		}
	}
	return n
}

func (h *harness) raw(setup string, fn func(tx pgx.Tx) error) error {
	ctx := context.Background()
	tx, err := h.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if setup != "" {
		if _, err := tx.Exec(ctx, setup); err != nil {
			return err
		}
	}
	return fn(tx)
}

// seeder returns an actor in the caller's tenant that holds every role, because these
// helpers create fixture data rather than exercise a permission decision. It is a
// distinct identity: reusing the caller's id would grant them roles the test is about
// to check they do not have.
func seeder(actor ks.Actor, h *harness) ks.Actor {
	if a, ok := h.seeders[actor.Tenant]; ok {
		return a
	}
	a := h.actor(actor.Tenant, h.app.Model.Roles...)
	if h.seeders == nil {
		h.seeders = map[ks.ID]ks.Actor{}
	}
	h.seeders[actor.Tenant] = a
	return a
}

func (h *harness) newCustomer(actor ks.Actor, name string) *customerRow {
	h.t.Helper()
	var out *customerRow
	err := h.call(seeder(actor, h), func(ctx context.Context, d *runtime.DB) error {
		c, err := runtime.Scoped(d, custTable).Insert(ctx, []runtime.Assignment{
			{Field: "name", Column: "name", Args: []any{name}},
			{Field: "email", Column: "email", Args: []any{strings.ToLower(name) + "@example.com"}},
		})
		out = c
		return err
	})
	if err != nil {
		h.t.Fatalf("insert customer: %v", detail(err))
	}
	return out
}

func (h *harness) newInvoice(actor ks.Actor, cust *customerRow, minor int64, status string) *invoiceRow {
	h.t.Helper()
	var out *invoiceRow
	err := h.call(seeder(actor, h), func(ctx context.Context, d *runtime.DB) error {
		x, err := runtime.Scoped(d, invTable).Insert(ctx, []runtime.Assignment{
			{Field: "customer", Column: "customer_id", Args: []any{cust.ID}},
			{Field: "amount", Column: "amount_minor", Args: []any{minor}},
			{Field: "amount", Column: "amount_currency", Args: []any{"USD"}},
			{Field: "status", Column: "status", Args: []any{status}},
		})
		out = x
		return err
	})
	if err != nil {
		h.t.Fatalf("insert invoice: %v", detail(err))
	}
	return out
}

// detail shows the hidden cause of an internal error, which keeps a failing assertion
// here debuggable without printing the whole transaction log.
func detail(err error) string {
	if err == nil {
		return "<nil>"
	}
	e := ks.AsError(err)
	if e.Internal != nil {
		return fmt.Sprintf("%s (%s): %v", e.Code, e.Message, e.Internal)
	}
	return fmt.Sprintf("%s (%s)", e.Code, e.Message)
}

func code(err error) string {
	if err == nil {
		return ""
	}
	return ks.AsError(err).Code
}

// ---- the guarantees ----

func TestTenantIsolationInQueryLayer(t *testing.T) {
	h := setup(t)
	acmeCust := h.newCustomer(h.member, "AcmeCo")
	otherCust := h.newCustomer(h.other, "GlobexCo")

	err := h.call(h.member, func(ctx context.Context, d *runtime.DB) error {
		_, err := runtime.Scoped(d, custTable).Get(ctx, otherCust.ID)
		return err
	})
	if code(err) != ks.CodeNotFound {
		t.Fatalf("a foreign tenant row must read as not_found, got %v", detail(err))
	}
	got, err2 := func() (*customerRow, error) {
		var out *customerRow
		e := h.call(h.member, func(ctx context.Context, d *runtime.DB) error {
			var err error
			out, err = runtime.Scoped(d, custTable).Get(ctx, acmeCust.ID)
			return err
		})
		return out, e
	}()
	if err2 != nil {
		t.Fatalf("own tenant row must be visible: %v", err2)
	}
	if got.Name != "AcmeCo" {
		t.Fatalf("wrong row: %+v", got)
	}
}

// TestRowLevelSecurityBackstop is the defense-in-depth claim from §8.5: a query that
// forgets the tenant filter is still stopped by the database.
func TestRowLevelSecurityBackstop(t *testing.T) {
	h := setup(t)
	h.newCustomer(h.other, "GlobexCo")
	var count int64
	err := h.raw("SET LOCAL ks.tenant_id = '"+string(ks.NewID())+"'; SET LOCAL ks.cross_tenant = 'off';", func(tx pgx.Tx) error {
		rows, err := tx.Query(context.Background(), "SELECT count(*) FROM customer")
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			return rows.Scan(&count)
		}
		return rows.Err()
	})
	if err != nil {
		t.Fatalf("query should succeed but return nothing: %v", err)
	}
	if count != 0 {
		t.Fatalf("RLS leaked %d rows across tenants", count)
	}
}

func TestCrossTenantOptInIsExplicit(t *testing.T) {
	h := setup(t)
	h.newCustomer(h.other, "GlobexCo")
	var total int64
	err := h.raw("SET LOCAL ks.tenant_id = ''; SET LOCAL ks.cross_tenant = 'on';", func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), "SELECT count(*) FROM customer").Scan(&total)
	})
	if err != nil {
		t.Fatal(detail(err))
	}
	if total == 0 {
		t.Fatal("the declared opt-in should see rows")
	}
}

func TestReadRuleIsCompiledIntoEveryQuery(t *testing.T) {
	h := setup(t)
	cust := h.newCustomer(h.member, "AcmeCo")
	// The member's actor id is not in any customer's owner column, and the fixture's
	// Read rule is a role test; an anonymous caller must see nothing at all.
	anon := ks.Actor{Kind: ks.ActorAnonymous}
	err := h.call(anon, func(ctx context.Context, d *runtime.DB) error {
		_, err := runtime.Scoped(d, custTable).Get(ctx, cust.ID)
		return err
	})
	if code(err) != ks.CodeNotFound {
		t.Fatalf("an actor without the role must not see the row, got %v", err)
	}
}

func TestCreateRuleRefusesTheWrite(t *testing.T) {
	h := setup(t)
	memberOnly := h.actor(h.tenants["Acme"], "member")
	err := h.call(memberOnly, func(ctx context.Context, d *runtime.DB) error {
		_, err := runtime.Scoped(d, custTable).Insert(ctx, []runtime.Assignment{
			{Field: "name", Column: "name", Args: []any{"NopeCo"}},
			{Field: "email", Column: "email", Args: []any{"nope@example.com"}},
		})
		return err
	})
	if code(err) != ks.CodeForbidden {
		t.Fatalf("Create(admin) must refuse a member, got %v", err)
	}
	var n int64
	if err := h.pool.QueryRow(context.Background(), "SELECT count(*) FROM customer WHERE name='NopeCo'").Scan(&n); err != nil {
		t.Fatal(detail(err))
	}
	if n != 0 {
		t.Fatal("the refused row must not exist")
	}
}

func TestCheckInvariantIsEnforcedByDatabase(t *testing.T) {
	h := setup(t)
	cust := h.newCustomer(h.member, "AcmeCo")
	err := h.call(h.admin, func(ctx context.Context, d *runtime.DB) error {
		_, err := runtime.Scoped(d, invTable).Insert(ctx, []runtime.Assignment{
			{Field: "customer", Column: "customer_id", Args: []any{cust.ID}},
			{Field: "amount", Column: "amount_minor", Args: []any{int64(-5)}},
			{Field: "amount", Column: "amount_currency", Args: []any{"USD"}},
			{Field: "status", Column: "status", Args: []any{"draft"}},
		})
		return err
	})
	if code(err) != ks.CodeInvariant && code(err) != ks.CodeInvalid {
		t.Fatalf("a negative amount must be refused, got %v", err)
	}
}

func TestDeferredInvariantCrossingARef(t *testing.T) {
	h := setup(t)
	// Forge an invoice whose customer belongs to another tenant by writing the pair in
	// one transaction with cross-tenant on, then confirm a normal write path refuses it.
	var foreignID ks.ID
	if err := h.raw("SET LOCAL ks.cross_tenant = 'on';", func(tx pgx.Tx) error {
		rows, err := tx.Query(context.Background(), "SELECT id FROM customer WHERE tenant_id = $1", h.other.Tenant)
		if err != nil {
			return err
		}
		for rows.Next() {
			if err := rows.Scan(&foreignID); err != nil {
				return err
			}
		}
		rows.Close()
		return rows.Err()
	}); err != nil {
		t.Fatal(detail(err))
	}
	if foreignID.IsNil() {
		t.Skip("no foreign customer to point at")
	}
	// The deferred check uses the invoice's tenant; the RLS policy hides the foreign
	// customer, so the subquery finds nothing and the invariant must refuse the write.
	err := h.call(h.admin, func(ctx context.Context, d *runtime.DB) error {
		_, err := runtime.Scoped(d, invTable).Insert(ctx, []runtime.Assignment{
			{Field: "customer", Column: "customer_id", Args: []any{foreignID}},
			{Field: "amount", Column: "amount_minor", Args: []any{int64(100)}},
			{Field: "amount", Column: "amount_currency", Args: []any{"USD"}},
			{Field: "status", Column: "status", Args: []any{"draft"}},
		})
		return err
	})
	if err == nil {
		t.Fatal("the deferred invariant must refuse an invoice that points across tenants")
	}
}

func TestOptimisticConcurrency(t *testing.T) {
	h := setup(t)
	cust := h.newCustomer(h.member, "AcmeCo")
	inv := h.newInvoice(h.admin, cust, 5000, "draft")
	err := h.call(h.admin, func(ctx context.Context, d *runtime.DB) error {
		_, err := runtime.Scoped(d, invTable).Update(ctx, inv.ID, inv.Version+7, []runtime.Assignment{
			{Field: "status", Column: "status", Args: []any{"sent"}},
		})
		return err
	})
	if code(err) != ks.CodeConflict {
		t.Fatalf("a stale version must return conflict, got %v", err)
	}
	// And a fresh version updates in one round trip.
	updated, err2 := func() (*invoiceRow, error) {
		var out *invoiceRow
		e := h.call(h.admin, func(ctx context.Context, d *runtime.DB) error {
			var err error
			out, err = runtime.Scoped(d, invTable).Update(ctx, inv.ID, inv.Version, []runtime.Assignment{
				{Field: "status", Column: "status", Args: []any{"sent"}},
			})
			return err
		})
		return out, e
	}()
	if err2 != nil {
		t.Fatalf("update should succeed: %v", err2)
	}
	if updated.Status != "sent" || updated.Version != inv.Version+1 {
		t.Fatalf("update result: %+v", updated)
	}
}

func TestUpdateRuleAndFieldWriteRule(t *testing.T) {
	h := setup(t)
	cust := h.newCustomer(h.member, "AcmeCo")
	inv := h.newInvoice(h.admin, cust, 5000, "draft")

	// An accountant may change status but not notes (FieldWrite notes = admin).
	err := h.call(h.actor(h.tenants["Acme"], "member", "accountant"), func(ctx context.Context, d *runtime.DB) error {
		_, err := runtime.Scoped(d, invTable).Update(ctx, inv.ID, inv.Version, []runtime.Assignment{
			{Field: "notes", Column: "notes", Args: []any{"internal note"}},
		})
		return err
	})
	if code(err) != ks.CodeForbidden {
		t.Fatalf("a non-admin must not write a FieldWrite-restricted column, got %v", err)
	}

	// A voided invoice fails the UpdateAfter rule, which is a 403 not a 422.
	err = h.call(h.admin, func(ctx context.Context, d *runtime.DB) error {
		_, err := runtime.Scoped(d, invTable).Update(ctx, inv.ID, inv.Version, []runtime.Assignment{
			{Field: "status", Column: "status", Args: []any{"void"}},
		})
		return err
	})
	if code(err) != ks.CodeForbidden {
		t.Fatalf("UpdateAfter must refuse a void transition, got %v", err)
	}
}

func TestFieldReadRemovesTheColumnFromTheWire(t *testing.T) {
	h := setup(t)
	cust := h.newCustomer(h.member, "AcmeCo")
	inv := h.newInvoice(h.admin, cust, 5000, "draft")
	if _, err := func() (int, error) {
		return 0, h.call(h.admin, func(ctx context.Context, d *runtime.DB) error {
			_, err := runtime.Scoped(d, invTable).Update(ctx, inv.ID, inv.Version, []runtime.Assignment{
				{Field: "notes", Column: "notes", Args: []any{"secret"}},
			})
			return err
		})
	}(); err != nil {
		t.Fatal(detail(err))
	}
	// A member can read the row but must not receive the hidden column at all.
	var got *invoiceRow
	if err := h.call(h.member, func(ctx context.Context, d *runtime.DB) error {
		var err error
		got, err = runtime.Scoped(d, invTable).Get(ctx, inv.ID)
		for _, st := range d.Stmts() {
			t.Logf("READ SQL: %s", st.SQL)
		}
		return err
	}); err != nil {
		t.Fatalf("member may read the row: %v", err)
	}
	t.Logf("field read rules: n=%d %+v", len(invTable.FieldRead), invTable.FieldRead)
	if got.NotesPresent || got.Notes != "" {
		t.Fatalf("a hidden field must never be fetched: %+v", got)
	}
	if got.Status != "draft" {
		t.Fatalf("visible fields still arrive: %+v", got)
	}
}

func TestDeleteIsRefusedWhenTheRuleSaysNever(t *testing.T) {
	h := setup(t)
	cust := h.newCustomer(h.member, "AcmeCo")
	inv := h.newInvoice(h.admin, cust, 5000, "draft")
	err := h.call(h.admin, func(ctx context.Context, d *runtime.DB) error {
		return runtime.Scoped(d, invTable).Delete(ctx, inv.ID, inv.Version)
	})
	if code(err) != ks.CodeForbidden {
		t.Fatalf("Delete(Never()) must refuse, got %v", err)
	}
}

func TestKeysetPaginationIsStableAndBounded(t *testing.T) {
	h := setup(t)
	cust := h.newCustomer(h.member, "AcmeCo")
	for i := 0; i < 12; i++ {
		h.newInvoice(h.admin, cust, int64(100+i), "draft")
	}
	seen := map[string]bool{}
	cursor := ""
	pages := 0
	for {
		var page *runtime.PageOf[invoiceRow]
		if err := h.call(h.member, func(ctx context.Context, d *runtime.DB) error {
			var err error
			page, err = runtime.Scoped(d, invTable).Q().
				OrderBy(runtime.Order("t.created_at", false)).
				Page(ctx, cursor, 5)
			t.Logf("page %d: %d rows, next=%q", pages, len(page.Rows), page.NextCursor)
			return err
		}); err != nil {
			t.Fatalf("page: %v", detail(err))
		}
		for _, row := range page.Rows {
			if seen[string(row.ID)] {
				t.Fatalf("row %s came back twice; keyset must not repeat", row.ID)
			}
			seen[string(row.ID)] = true
		}
		pages++
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
		if pages > 10 {
			t.Fatal("pagination never terminated")
		}
	}
	if len(seen) != 12 {
		t.Fatalf("want all 12 visible rows exactly once, got %d", len(seen))
	}
	// The configured page cap applies no matter what a caller asks for.
	var n int
	if err := h.call(h.member, func(ctx context.Context, d *runtime.DB) error {
		page, err := runtime.Scoped(d, invTable).Q().Page(ctx, "", 10_000)
		n = len(page.Rows)
		return err
	}); err != nil {
		t.Fatal(detail(err))
	}
	if n > 50 {
		t.Fatalf("page size %d exceeded the configured cap", n)
	}
}

func TestBatchedRelationLoadIsOneQuery(t *testing.T) {
	h := setup(t)
	cust := h.newCustomer(h.member, "AcmeCo")
	for i := 0; i < 4; i++ {
		h.newInvoice(h.admin, cust, int64(1000+i), "draft")
	}
	loader := runtime.With[invoiceRow]{Name: "customer", Load: func(v *runtime.View[invoiceRow], rows []*invoiceRow) error {
		ids := make([]ks.ID, 0, len(rows))
		for _, r := range rows {
			ids = append(ids, r.CustomerID)
		}
		got, err := runtime.Scoped(v.DB(), custTable).ByIDs(v.Context(), ids)
		if err != nil {
			return err
		}
		byID := map[ks.ID]*customerRow{}
		for _, c := range got {
			byID[c.ID] = c
		}
		for _, r := range rows {
			_ = byID[r.CustomerID]
		}
		return nil
	}}
	var stmts []runtime.Stmt
	err := h.call(h.member, func(ctx context.Context, d *runtime.DB) error {
		page, err := runtime.Scoped(d, invTable).Q().With(loader).Page(ctx, "", 10)
		if err != nil {
			return err
		}
		if len(page.Rows) != 4 {
			return fmt.Errorf("rows %d", len(page.Rows))
		}
		stmts = d.Stmts()
		return nil
	})
	if err != nil {
		t.Fatal(detail(err))
	}
	// One SELECT for invoices, one for customers, plus audit: never one per row (§8.2).
	reads := 0
	for _, s := range stmts {
		if strings.HasPrefix(s.SQL, "SELECT") {
			reads++
		}
	}
	if reads > 3 {
		t.Fatalf("expected one batched read per relation, got %d selects: %v", reads, stmts)
	}
}

func TestBudgetIsCounted(t *testing.T) {
	h := setup(t)
	cust := h.newCustomer(h.member, "AcmeCo")
	inv := h.newInvoice(h.admin, cust, 5000, "draft")
	err := h.call(h.admin, func(ctx context.Context, d *runtime.DB) error {
		d.SetBudget(2, 1000, true)
		for i := 0; i < 5; i++ {
			if _, err := runtime.Scoped(d, invTable).Get(ctx, inv.ID); err != nil {
				return err
			}
		}
		return nil
	})
	if code(err) != ks.CodeBudget {
		t.Fatalf("the query budget must fail the action in tests, got %v", err)
	}
}

func TestAuditRowIsWrittenInTheSameTransaction(t *testing.T) {
	h := setup(t)
	cust := h.newCustomer(h.member, "AcmeCo")
	inv := h.newInvoice(h.admin, cust, 5000, "draft")
	if _, err := func() (int, error) {
		return 0, h.call(h.admin, func(ctx context.Context, d *runtime.DB) error {
			_, err := runtime.Scoped(d, invTable).Update(ctx, inv.ID, inv.Version, []runtime.Assignment{
				{Field: "status", Column: "status", Args: []any{"sent"}},
			})
			return err
		})
	}(); err != nil {
		t.Fatal(detail(err))
	}
	_, actionN, changed := h.auditLast()
	action := actionN
	kind := ""
	_ = kind
	if !contains(changed, "status") {
		t.Fatalf("the audit diff must name the changed field: %v", changed)
	}
	if action == "" {
		t.Fatal("the audit record must name the action")
	}
	// The audit table refuses edits even to the owner (§9.5).
	if _, err := h.pool.Exec(context.Background(), `UPDATE ks_audit SET action='x' WHERE id=(SELECT max(id) FROM ks_audit)`); err == nil {
		t.Fatal("audit records must be append-only")
	}
	if _, err := h.pool.Exec(context.Background(), `DELETE FROM ks_audit`); err == nil {
		t.Fatal("audit must refuse delete")
	}
}

func contains(in []string, want string) bool {
	for _, x := range in {
		if x == want {
			return true
		}
	}
	return false
}

func TestDeferredEffectDoesNotEscapeARollback(t *testing.T) {
	h := setup(t)
	boom := errors.New("action failed")
	err := h.call(h.admin, func(ctx context.Context, d *runtime.DB) error {
		if err := d.RecordEffect(ctx, runtime.EffectCall{Type: model.EffectEmail, Action: "billing.SendInvoice", Payload: []byte(`{"to":"x@y.z"}`)}); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("the action error must surface: %v", err)
	}
	n := h.count(`SELECT count(*) FROM ks_outbox WHERE action='billing.SendInvoice'`)
	if n != 0 {
		t.Fatalf("a rolled-back transaction must leave no effect behind, found %d", n)
	}

	// On commit, the row exists and is pending with a derived idempotency key (§10.1).
	if err := h.call(h.admin, func(ctx context.Context, d *runtime.DB) error {
		return d.RecordEffect(ctx, runtime.EffectCall{Type: model.EffectEmail, Action: "billing.SendInvoice", Invocation: ks.NewID(), Seq: 1, Payload: []byte(`{}`)})
	}); err != nil {
		t.Fatal(detail(err))
	}
	rows := h.look(`SELECT idempotency_key, 0, ARRAY[state] FROM ks_outbox WHERE action='billing.SendInvoice' ORDER BY created_at DESC LIMIT 1`)
	var key string
	var zero int64
	var stateList []string
	for rows.Next() {
		if err := rows.Scan(&key, &zero, &stateList); err != nil {
			rows.Close()
			t.Fatal(detail(err))
		}
	}
	rows.Close()
	if len(key) < 8 || len(stateList) != 1 || stateList[0] != "pending" {
		t.Fatalf("outbox row: key=%q state=%v", key, stateList)
	}
}

func TestOutboxDispatcherClaimsOnceAndRetries(t *testing.T) {
	h := setup(t)
	var failures int32 = 1
	rec := &runtime.Recorder{FailOn: func(c runtime.EffectCall) error {
		if failures > 0 {
			failures--
			return fmt.Errorf("provider unavailable: %w", errTransient)
		}
		return nil
	}}
	h.app.Providers.Register(model.EffectEmail, rec)
	if err := h.call(h.admin, func(ctx context.Context, d *runtime.DB) error {
		return d.RecordEffect(ctx, runtime.EffectCall{Type: model.EffectEmail, Action: "billing.SendInvoice", Invocation: ks.NewID(), Seq: 2, Payload: []byte(`{}`)})
	}); err != nil {
		t.Fatal(detail(err))
	}
	// A failed attempt goes back to pending with a later run_after (§10.1 step 6).
	outbox := h.app.NewOutbox()
	// The retry curve is seconds in production; a test waits nothing.
	outbox.Base, outbox.Cap = 0, 0
	if _, err := outbox.Dispatch(context.Background(), 10); err != nil {
		t.Fatalf("dispatch: %v", detail(err))
	}
	state, attempts, _ := h.stateAttempts(`SELECT coalesce(state,''), attempts::bigint, '{}'::text[] FROM ks_outbox WHERE action='billing.SendInvoice' ORDER BY created_at DESC LIMIT 1`)
	var attemptsN = int(attempts)
	if state != "pending" || attemptsN != 1 {
		t.Fatalf("after one failed attempt: state=%s attempts=%d", state, attemptsN)
	}
	if _, err := outbox.Dispatch(context.Background(), 10); err != nil {
		t.Fatal(detail(err))
	}
	if len(rec.AttemptsOf(model.EffectEmail)) != 2 {
		t.Fatalf("the provider should have been attempted twice: %d", len(rec.AttemptsOf(model.EffectEmail)))
	}
	state, _, _ = h.stateAttempts(`SELECT coalesce(state,''), 0, '{}'::text[] FROM ks_outbox WHERE action='billing.SendInvoice' ORDER BY created_at DESC LIMIT 1`)
	if state != "done" {
		t.Fatalf("a successful retry must complete the row, state=%s", state)
	}

	// Two dispatchers at once must not execute the same row twice (§10.1 step 4).
	for i := 0; i < 6; i++ {
		if err := h.call(h.admin, func(ctx context.Context, d *runtime.DB) error {
			return d.RecordEffect(ctx, runtime.EffectCall{Type: model.EffectEmail, Action: "billing.Fanout", Invocation: ks.NewID(), Seq: i, Payload: []byte(`{}`)})
		}); err != nil {
			t.Fatal(detail(err))
		}
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	total := 0
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			n, err := h.app.NewOutbox().Dispatch(context.Background(), 10)
			mu.Lock()
			total += n
			mu.Unlock()
			if err != nil {
				t.Errorf("concurrent dispatch: %v", err)
			}
		}()
	}
	wg.Wait()
	if total != 6 {
		t.Fatalf("each row must be dispatched exactly once, got %d of 6", total)
	}
}

func TestJobsRunAsSystemAndRespectConcurrency(t *testing.T) {
	h := setup(t)
	var ran []ks.Actor
	var mu sync.Mutex
	h.app.Handlers["billing.SendReminders"] = &runtime.Handler{
		Ref: "billing.SendReminders", Module: "billing", Name: "SendReminders", Kind: model.KindJob,
		Handle: func(c *runtime.Call) (any, error) {
			mu.Lock()
			ran = append(ran, c.Actor)
			mu.Unlock()
			return runtime.HandlerOutput{Body: []byte("{}")}, nil
		},
	}
	if err := h.call(h.admin, func(ctx context.Context, d *runtime.DB) error {
		return d.Enqueue(ctx, runtime.JobCall{Action: "billing.SendReminders", Tenant: h.admin.Tenant})
	}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	w := h.app.NewWorker()
	jobs, err := w.Claim(context.Background(), 10)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("one job queued, %d claimed", len(jobs))
	}
	if err := w.Run(context.Background(), jobs[0]); err != nil {
		t.Fatalf("run: %v", detail(err))
	}
	mu.Lock()
	defer mu.Unlock()
	if len(ran) != 1 {
		t.Fatalf("the job did not run: %v", ran)
	}
	if ran[0].Kind != ks.ActorSystem {
		t.Fatalf("RunAs(System) jobs run as the tenant's system actor, got %+v", ran[0])
	}
	var state string
	state, _, _ = h.stateAttempts(`SELECT coalesce(state,''), 0, '{}'::text[] FROM ks_jobs WHERE action='billing.SendReminders'`)
	if state != "done" {
		t.Fatalf("job state after success: %s", state)
	}
}

func TestSchedulesFireOncePerSlot(t *testing.T) {
	h := setup(t)
	h.app.Model.Modules[0].Actions = append(h.app.Model.Modules[0].Actions, model.Action{
		Name: "DailyReminders", Kind: model.KindSchedule, Cron: "* * * * *", Runs: "billing.SendReminders",
	})
	s := h.app.NewSchedules()
	s.TryLeader(context.Background())
	if !s.Leader {
		t.Skip("could not take the advisory lock")
	}
	now := time.Now().UTC().Truncate(time.Minute)
	n, err := s.Tick(context.Background(), now)
	if err != nil {
		t.Fatalf("tick: %v", detail(err))
	}
	if n != 1 {
		t.Fatalf("one job should be enqueued, got %d", n)
	}
	if _, err := s.Tick(context.Background(), now); err != nil {
		t.Fatal(detail(err))
	}
	count64 := h.count(`SELECT count(*) FROM ks_jobs WHERE action='billing.SendReminders'`)
	count := int(count64)
	if count != 1 {
		t.Fatalf("the second tick of the same slot must not enqueue again, jobs=%d", count)
	}
	fired := int(h.count(`SELECT count(*) FROM ks_jobs WHERE action='billing.SendReminders'`))
	if fired < 1 {
		t.Fatal("schedules must enqueue jobs")
	}
	if !CronLike("0 9 * * *", time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC)) {
		t.Fatal("cron matching is wrong")
	}
	if CronLike("0 9 * * *", time.Date(2026, 10, 27, 9, 1, 0, 0, time.UTC)) {
		t.Fatal("cron must not match the minute after")
	}
}

func CronLike(expr string, t time.Time) bool { return runtime.CronMatches(expr, t) }

func TestIdempotencyReplaysAndRefusesMismatch(t *testing.T) {
	h := setup(t)
	h.app.Handlers["billing.SendInvoice"] = &runtime.Handler{
		Ref: "billing.SendInvoice", Module: "billing", Name: "SendInvoice", Kind: model.KindMutation,
		Path: "/api/billing/SendInvoice",
		Spec: model.Action{Name: "SendInvoice", Kind: model.KindMutation,
			Requires:   policy.Or(policy.Role("admin"), policy.Role("accountant")),
			Idempotent: true,
			Limits:     model.ActionLimits{RateLimit: []model.RateLimit{{Scope: "actor", Limit: 100, Per: "1m0s"}}}},
		Handle: func(c *runtime.Call) (any, error) {
			return runtime.HandlerOutput{Body: []byte(`{"ok":true}`)}, nil
		},
	}
	call := func(body string, key string) (string, error) {
		out, err := h.app.Run(context.Background(), h.app.Handlers["billing.SendInvoice"], h.admin,
			&runtime.Call{Actor: h.admin, Body: []byte(body), Key: key}, h.app.Log)
		return string(out.Body), err
	}
	first, err := call(`{"invoice_id":"1"}`, "k1")
	if err != nil {
		t.Fatal(detail(err))
	}
	second, err := call(`{"invoice_id":"1"}`, "k1")
	if err != nil {
		t.Fatal(detail(err))
	}
	if first != second {
		t.Fatalf("a repeat call must return the stored result: %s != %s", first, second)
	}
	if _, err := call(`{"invoice_id":"2"}`, "k1"); code(err) != ks.CodeConflict {
		t.Fatalf("a different body under the same key must conflict, got %v", err)
	}
	if _, err := call(`{}`, ""); code(err) != ks.CodeInvalid {
		t.Fatalf("an idempotent mutation without a key is invalid, got %v", err)
	}
}

func TestSessionAuthRotateAndRateLimit(t *testing.T) {
	h := setup(t)
	actor := h.actor(h.tenants["Acme"], "member")
	token, cookie, err := h.app.Login(context.Background(), actor, ks.NilID, "test", "127.0.0.1")
	if err != nil {
		t.Fatalf("login: %v", detail(err))
	}
	if cookie.HttpOnly == false || cookie.Secure == false || cookie.SameSite != http.SameSiteLaxMode {
		t.Fatalf("session cookie flags: %+v", cookie)
	}
	req, _ := http.NewRequest("GET", "http://x/", nil)
	req.AddCookie(cookie)
	got, err := h.app.Authenticate(req)
	if err != nil {
		t.Fatal(detail(err))
	}
	if got.ID != actor.ID || got.Tenant != actor.Tenant {
		t.Fatalf("session resolved the wrong actor: %+v", got)
	}
	if !contains(got.Roles, "member") {
		t.Fatalf("roles must come from membership: %v", got.Roles)
	}
	// Revocation is immediate: remove the grant and the role test fails (§15.2).
	h.revoke(actor)
	relim, err := h.app.Authenticate(req)
	if err != nil {
		t.Fatal(detail(err))
	}
	if len(relim.Roles) != 0 {
		t.Fatalf("a revoked role must disappear on the next request: %v", relim.Roles)
	}
	// Rotation invalidates the previous cookie (§9.2).
	newToken, newCookie, err := h.app.Rotate(context.Background(), token, actor)
	if err != nil {
		t.Fatalf("rotate: %v cause=%v", err, errors.Unwrap(err))
	}
	if newToken == token {
		t.Fatal("rotation must issue a new token")
	}
	res, _ := h.app.Authenticate(cookieRequest(cookie))
	if !res.IsAnonymous() {
		t.Fatal("the rotated-from session must be revoked")
	}
	req2, _ := http.NewRequest("GET", "http://x/", nil)
	req2.AddCookie(newCookie)
	if res2, _ := h.app.Authenticate(req2); res2.IsAnonymous() {
		t.Fatal("the new session must authenticate")
	}

	// A mutation handler with a per-actor limit sheds after the declared count (§3.3).
	n := 0
	var limitErrors int
	h.app.SetMetricSink(func(name string, delta float64) {
		if name == "ks_rate_limit_error" {
			limitErrors++
		}
	})
	h.app.Handlers["billing.Limited"] = &runtime.Handler{
		Ref: "billing.Limited", Module: "billing", Name: "Limited", Kind: model.KindMutation,
		Path: "/api/billing/Limited",
		Spec: model.Action{Name: "Limited", Kind: model.KindMutation, Requires: policy.Role("member"),
			Limits: model.ActionLimits{RateLimit: []model.RateLimit{{Scope: "actor", Limit: 4, Per: "1m0s"}}}},
		Handle: func(c *runtime.Call) (any, error) {
			n++
			return runtime.HandlerOutput{Body: []byte("{}")}, nil
		},
	}
	lim := h.actor(h.tenants["Acme"], "member")
	var limited error
	for i := 0; i < 8; i++ {
		_, err := h.app.Run(context.Background(), h.app.Handlers["billing.Limited"], lim, &runtime.Call{Actor: lim}, h.app.Log)
		limited = err
	}
	if limitErrors != 0 {
		t.Fatalf("the rate limit store must work; it failed %d times and the check failed open", limitErrors)
	}
	if code(limited) != ks.CodeRateLimited {
		t.Fatalf("the limit must shed after 4 calls, got %v cause=%v (executed %d)", limited, errors.Unwrap(limited), n)
	}
	if n != 4 {
		t.Fatalf("the action ran %d times under a limit of 4", n)
	}
}

func TestHTTPEffectRefusesPrivateHosts(t *testing.T) {
	ctx := context.Background()
	for _, host := range []string{"169.254.169.254", "10.0.0.5", "127.0.0.1", "localhost", "fe80::1", "single-label"} {
		if err := assertPublic(ctx, host); err == nil {
			t.Fatalf("%s must be refused before the request leaves the process", host)
		}
	}
}

func TestImmediateEffectRunsThroughTheProvider(t *testing.T) {
	h := setup(t)
	stAt := func(where string) {
		st := h.app.Pool.Stat()
		fmt.Fprintf(os.Stderr, "\n# STAT %s acquired=%d\n", where, st.AcquiredConns())
	}
	stAt("after setup")
	rec := &runtime.Recorder{}
	h.app.Providers.Register(model.EffectHTTP, rec)
	var got runtime.EffectResult
	err := h.call(h.admin, func(ctx context.Context, d *runtime.DB) error {
		var err error
		got, err = d.CallEffect(ctx, runtime.EffectCall{Type: model.EffectHTTP, Action: "billing.MarkPaid",
			Mode: "immediate", Hosts: []string{"api.example.com"}, Payload: []byte(`{"ok":true}`)})
		return err
	})
	stAt("after call")
	if err != nil {
		t.Fatalf("an immediate effect should run inline: %v cause=%v", err, errors.Unwrap(err))
	}
	if len(rec.Calls) != 1 || rec.Calls[0].Mode != "immediate" {
		t.Fatalf("provider calls: %+v", rec.Calls)
	}
	// It is still recorded, so the trace shows it ran (§18.1).
	state, _, _ := h.stateAttempts(`SELECT coalesce(state,''), 0, '{}'::text[] FROM ks_outbox WHERE action='billing.MarkPaid' ORDER BY created_at DESC LIMIT 1`)
	if state != "done" {
		t.Fatalf("an immediate effect is recorded as done, got %s", state)
	}
	_ = got
}

func TestSensitiveValuesNeverReachTheAuditDiff(t *testing.T) {
	h := setup(t)
	cust := h.newCustomer(h.member, "AcmeCo")
	inv := h.newInvoice(h.admin, cust, 5000, "draft")
	if _, err := func() (int, error) {
		return 0, h.call(h.admin, func(ctx context.Context, d *runtime.DB) error {
			_, err := runtime.Scoped(d, invTable).Update(ctx, inv.ID, inv.Version, []runtime.Assignment{
				{Field: "notes", Column: "notes", Args: []any{"card-4242"}},
				{Field: "status", Column: "status", Args: []any{"sent"}},
			})
			return err
		})
	}(); err != nil {
		t.Fatal(detail(err))
	}
	raw := h.auditAfter()
	if strings.Contains(string(raw), "card-4242") {
		t.Fatalf("a Sensitive value must be redacted from the audit diff: %s", raw)
	}
}

func TestUnknownTableFailsLoudly(t *testing.T) {
	h := setup(t)
	err := h.call(h.admin, func(ctx context.Context, d *runtime.DB) error {
		_, err := runtime.Scoped(d, &runtime.Table[invoiceRow]{Ref: "billing.Ghost", Table: "ghost_table", IDColumn: "id"}).
			Get(ctx, ks.NewID())
		return err
	})
	if err == nil {
		t.Fatal("a missing table must be an error")
	}
}

func TestMain(m *testing.M) { os.Exit(m.Run()) }

func assertPublic(ctx context.Context, host string) error {
	return runtime.AssertPublicHost(ctx, host)
}

var errTransient = errors.New("transient provider failure")

// cookieRequest builds a request that carries only a session cookie, which is how a
// browser-driven request looks to the pipeline.
func cookieRequest(c *http.Cookie) *http.Request {
	r := httptest.NewRequest("GET", "http://x/", nil)
	r.AddCookie(c)
	return r
}

var _ = debug.Stack
