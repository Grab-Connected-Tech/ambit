package kstest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"keystone.dev/keystone/ks"
	"keystone.dev/keystone/model"
	"keystone.dev/keystone/policy"
	"keystone.dev/keystone/runtime"
)

// The tests ks gen writes into gen/ call TableRules once per resource and ActionRules
// once with every action's adapter (§16.2). The cases are derived from the model when the
// test runs, so they cannot drift from the rules they test.
//
// ponytail: the access matrix covers rules that test roles only (Role, an Or of roles,
// Always, Never); a rule over row fields or the actor's id is skipped with a log line.
// CHECK-enforced invariants are covered when they compare one field with an integer or
// are an implication "a = v implies b is (not) set". Widening either means generating
// satisfying and violating rows from any rule tree.

// Fixture creates a row of the resource in the tenant with placeholder values, creating
// whatever it references first, and returns its id.
func (e *Env) Fixture(ref string, tenant ks.ID) ks.ID {
	e.T.Helper()
	_, name := model.SplitRef(ref)
	if e.Model.Account != nil && name == e.Model.Account.Name {
		return e.Actor(tenant).ID
	}
	if e.Model.Tenant != nil && name == e.Model.Tenant.Name {
		return tenant
	}
	res := e.Model.FindResource(ref)
	if res == nil {
		e.T.Fatalf("kstest: no resource %s", ref)
	}
	id := ks.NewID()
	set := map[string]any{}
	if res.Tenant {
		set["tenant_id"] = tenant
	}
	e.insertIn(res.Table, res.Fields, id, set, tenant)
	e.invalidate(ref, tenant)
	return id
}

// assignments are the column values of a new row, the way an action would pass them.
func (e *Env) assignments(res *model.Resource, tenant ks.ID) []runtime.Assignment {
	var out []runtime.Assignment
	for _, f := range res.Fields {
		if framework[f.Column] || f.Optional || f.Default != nil {
			continue
		}
		switch {
		case f.Type.Kind == model.KindMoney && len(f.Columns) == 2:
			out = append(out, runtime.SetColumn(f.Columns[0], int64(1)), runtime.SetColumn(f.Columns[1], currency(f)))
		case f.Ref != nil:
			out = append(out, runtime.SetColumn(f.Column, e.Fixture(f.Ref.Resource, tenant)))
		default:
			if v, ok := placeholder(f, e.Clock.Now()); ok {
				out = append(out, runtime.SetColumn(f.Column, v))
			}
		}
	}
	return out
}

var framework = map[string]bool{"id": true, "tenant_id": true, "created_at": true, "updated_at": true, "version": true}

func currency(f model.Field) string {
	if f.Type.Currency != "" {
		return f.Type.Currency
	}
	return "USD"
}

// As runs fn with a scoped DB for the actor, the way the pipeline opens one, and rolls
// back: the generated tests only look at what each operation returns.
func (e *Env) As(actor ks.Actor, fn func(ctx context.Context, d *runtime.DB) error) error {
	ctx := context.Background()
	tx, err := e.App.BeginTx(ctx, runtime.Scope{Tenant: actor.Tenant, Actor: actor.ID, IsSystem: actor.Kind == ks.ActorSystem}, 5*time.Second, false)
	if err != nil {
		return err
	}
	defer e.App.Rollback(ctx, tx)
	d := e.App.NewDB(tx, actor, runtime.WithApp(e.App), runtime.WithAction("kstest"), runtime.WithClock(e.Clock), runtime.WithBudget(100, 100000, false))
	return fn(ctx, d)
}

// rolesOnly reports the roles that satisfy a rule that tests nothing but roles. all is
// true for Always: any actor at all.
func rolesOnly(n *policy.Node) (roles []string, all, ok bool) {
	if n == nil {
		return nil, false, true // no rule denies
	}
	n = policy.Normalize(n)
	switch n.Op {
	case policy.OpAlways:
		return nil, true, true
	case policy.OpNever:
		return nil, false, true
	case policy.OpRole:
		return n.Roles, false, true
	}
	return nil, false, false
}

// without lists the app's roles other than the given ones.
func without(all, these []string) []any {
	var out []any
	for _, r := range all {
		keep := true
		for _, x := range these {
			if r == x {
				keep = false
			}
		}
		if keep {
			out = append(out, r)
		}
	}
	return out
}

func toAny(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

// TableRules proves a resource's tenant isolation, access matrix and invariants.
func TableRules[T any](t *testing.T, app *model.App, table *runtime.Table[T]) {
	res := app.FindResource(table.Ref)
	if res == nil {
		t.Fatalf("kstest: %s is not in the model", table.Ref)
	}
	if res.Tenant {
		t.Run("tenant isolation", func(t *testing.T) { tenantIsolation(t, app, table, res) })
	}
	rules := []struct {
		op   string
		node *policy.Node
	}{{"read", res.Access.Read}, {"create", res.Access.Create}, {"update", res.Access.Update}, {"delete", res.Access.Delete}}
	for _, r := range rules {
		r := r
		t.Run("access "+r.op, func(t *testing.T) {
			roles, all, ok := rolesOnly(r.node)
			if !ok {
				t.Skipf("%s rule is not role-only: %s", r.op, policy.Canonical(r.node))
			}
			e := New(t, app)
			org := e.Tenant()
			// A row an actor cannot read, it cannot change either, so a writer also
			// holds a role the read rule accepts; only the rule under test differs.
			var reader []any
			if readRoles, readAll, ok := rolesOnly(res.Access.Read); ok && !readAll && len(readRoles) > 0 && r.op != "read" && r.op != "create" {
				reader = append(reader, readRoles[0])
			}
			var allowed, denied []ks.Actor
			switch {
			case all:
				allowed = append(allowed, e.Actor(org, reader...))
			case len(roles) > 0:
				for _, role := range roles {
					allowed = append(allowed, e.Actor(org, append([]any{role}, reader...)...))
				}
			}
			if !all {
				denied = append(denied, e.Actor(org, without(app.Roles, roles)...))
			}
			for _, a := range allowed {
				if err := operate(e, table, res, r.op, a, org); err != nil {
					t.Errorf("%s with roles %v is allowed, got %v", r.op, a.Roles, err)
				}
			}
			for _, a := range denied {
				if err := operate(e, table, res, r.op, a, org); err == nil {
					t.Errorf("%s with roles %v must be refused", r.op, a.Roles)
				}
			}
		})
	}
	for _, fr := range res.Access.FieldRead {
		fr := fr
		t.Run("field read "+fr.Field, func(t *testing.T) { fieldRead(t, app, table, res, fr) })
	}
	for _, inv := range res.Invariants {
		inv := inv
		t.Run("invariant "+inv.Name, func(t *testing.T) { invariant(t, app, res, inv) })
	}
}

// operate performs one operation on a fresh row as the actor.
func operate[T any](e *Env, table *runtime.Table[T], res *model.Resource, op string, actor ks.Actor, org ks.ID) error {
	var id ks.ID
	if op != "create" {
		id = e.Fixture(table.Ref, org)
	}
	sets := e.assignments(res, org)
	return e.As(actor, func(ctx context.Context, d *runtime.DB) error {
		v := runtime.Scoped(d, table)
		switch op {
		case "read":
			_, err := v.Get(ctx, id)
			return err
		case "create":
			_, err := v.Insert(ctx, sets)
			return err
		case "update":
			if len(sets) == 0 {
				return nil
			}
			_, err := v.Update(ctx, id, 1, sets[:1])
			return err
		default:
			return v.Delete(ctx, id, 1)
		}
	})
}

func tenantIsolation[T any](t *testing.T, app *model.App, table *runtime.Table[T], res *model.Resource) {
	e := New(t, app)
	mine, theirs := e.Tenant(), e.Tenant()
	id := e.Fixture(table.Ref, mine)
	outsider := e.Actor(theirs, toAny(app.Roles)...)
	err := e.As(outsider, func(ctx context.Context, d *runtime.DB) error {
		v := runtime.Scoped(d, table)
		if _, err := v.Get(ctx, id); code(err) != ks.CodeNotFound {
			t.Errorf("another tenant's row must be not found, got %v", err)
		}
		if err := v.Delete(ctx, id, 1); err == nil {
			t.Error("another tenant's row must not be deletable")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// The RLS backstop: even a statement that forgets the tenant filter sees nothing.
	ctx := context.Background()
	tx, err := e.App.BeginTx(ctx, runtime.Scope{Tenant: outsider.Tenant, Actor: outsider.ID}, 5*time.Second, false)
	if err != nil {
		t.Fatal(err)
	}
	defer e.App.Rollback(ctx, tx)
	var n int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM "`+res.Table+`" WHERE tenant_id = $1`, mine).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("an unfiltered query from another tenant sees %d rows", n)
	}
}

func fieldRead[T any](t *testing.T, app *model.App, table *runtime.Table[T], res *model.Resource, fr model.FieldRule) {
	roles, all, ok := rolesOnly(fr.Expr)
	if !ok || all {
		t.Skipf("field rule is not a role restriction: %s", policy.Canonical(fr.Expr))
	}
	f := res.FindField(fr.Field)
	if f == nil {
		t.Fatalf("no field %s", fr.Field)
	}
	readRoles, readAll, readOK := rolesOnly(res.Access.Read)
	if !readOK {
		t.Skip("the row's read rule is not role-only")
	}
	e := New(t, app)
	org := e.Tenant()
	value, ok := placeholder(*f, e.Clock.Now())
	if !ok {
		t.Skipf("no placeholder for a %s field", f.Type.Kind)
	}
	id := ks.NewID()
	e.insertIn(res.Table, res.Fields, id, map[string]any{"tenant_id": org, f.Column: value}, org)
	// Each reader also holds a role that may read the row, so only the field rule differs.
	extra := []any{}
	if !readAll && len(readRoles) > 0 {
		extra = append(extra, readRoles[0])
	}
	see := func(a ks.Actor) bool {
		var visible bool
		err := e.As(a, func(ctx context.Context, d *runtime.DB) error {
			row, err := runtime.Scoped(d, table).Get(ctx, id)
			if err != nil {
				return err
			}
			fv := reflect.ValueOf(row).Elem().FieldByName(f.GoName)
			visible = fv.IsValid() && !fv.IsZero()
			return nil
		})
		if err != nil {
			t.Fatalf("read as %v: %v", a.Roles, err)
		}
		return visible
	}
	for _, r := range roles {
		if a := e.Actor(org, append([]any{r}, extra...)...); !see(a) {
			t.Errorf("%s may read %s", r, fr.Field)
		}
	}
	others := without(app.Roles, roles)
	var deniedRoles []any
	for _, r := range others {
		if !contains(roles, r.(string)) {
			deniedRoles = append(deniedRoles, r)
		}
	}
	// The denied reader holds every role the field rule does not name.
	if a := e.Actor(org, append(deniedRoles, extra...)...); !containsAny(roles, a.Roles) && see(a) {
		t.Errorf("roles %v must not read %s", a.Roles, fr.Field)
	}
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

func containsAny(ss, xs []string) bool {
	for _, x := range xs {
		if contains(ss, x) {
			return true
		}
	}
	return false
}

// invariant inserts a row that breaks a CHECK-enforced comparison and expects the
// database to refuse it.
func invariant(t *testing.T, app *model.App, res *model.Resource, inv model.Invariant) {
	if inv.Enforcement != "check" {
		t.Skipf("enforced %s, not by CHECK", inv.Enforcement)
	}
	e := New(t, app)
	set, ok := violation(e, res, inv.Expr)
	if !ok {
		t.Skipf("no violating row can be derived from %s", policy.Canonical(inv.Expr))
	}
	err := e.tryInsert(res, e.Tenant(), set)
	var pg *pgconn.PgError
	if !errors.As(err, &pg) || pg.Code != "23514" {
		t.Fatalf("a row with %v must violate %s, got %v", set, inv.Name, err)
	}
}

// violation derives column values that break a rule of one of two shapes: a field
// compared with an integer, or an implication "a = v implies b is set / is not set"
// (which normalizes to !(a = v) | [!] b IS SET).
func violation(e *Env, res *model.Resource, n *policy.Node) (map[string]any, bool) {
	field := func(k *policy.Node) *model.Field {
		if k == nil || k.Op != policy.OpField || len(k.Path) != 1 {
			return nil
		}
		return res.FindField(k.Path[0])
	}
	column := func(f *model.Field) string {
		if len(f.Columns) == 2 {
			return f.Columns[0]
		}
		return f.Column
	}
	if n == nil || len(n.Kids) != 2 {
		return nil, false
	}
	if f := field(n.Kids[0]); f != nil && n.Kids[1].Op == policy.OpLit && len(n.Kids[1].Values) == 1 {
		lit, err := strconv.ParseInt(n.Kids[1].Values[0].Text, 10, 64)
		if err != nil {
			return nil, false
		}
		bad := map[string]int64{policy.OpGt: lit, policy.OpGte: lit - 1, policy.OpLt: lit, policy.OpLte: lit + 1, policy.OpNe: lit, policy.OpEq: lit + 1}
		v, ok := bad[n.Op]
		return map[string]any{column(f): v}, ok
	}
	if n.Op != policy.OpOr {
		return nil, false
	}
	// !(a = v): make the premise true.
	premise := n.Kids[0]
	if premise.Op != policy.OpNot || len(premise.Kids) != 1 || premise.Kids[0].Op != policy.OpEq || len(premise.Kids[0].Kids) != 2 {
		return nil, false
	}
	a, lit := field(premise.Kids[0].Kids[0]), premise.Kids[0].Kids[1]
	if a == nil || lit.Op != policy.OpLit || len(lit.Values) != 1 {
		return nil, false
	}
	set := map[string]any{column(a): lit.Values[0].Text}
	// [!] b IS SET: make the conclusion false.
	concl, negated := n.Kids[1], false
	if concl.Op == policy.OpNot && len(concl.Kids) == 1 {
		concl, negated = concl.Kids[0], true
	}
	if concl.Op != policy.OpIsSet || len(concl.Kids) != 1 {
		return nil, false
	}
	b := field(concl.Kids[0])
	if b == nil || !b.Optional {
		return nil, false
	}
	if negated {
		v, ok := placeholder(*b, e.Clock.Now())
		if !ok {
			return nil, false
		}
		set[column(b)] = v
	}
	return set, true
}

// tryInsert is insertIn that returns the database error instead of failing the test.
func (e *Env) tryInsert(res *model.Resource, tenant ks.ID, set map[string]any) error {
	cols, args := e.rowValues(res.Table, res.Fields, ks.NewID(), merge([]map[string]any{{"tenant_id": tenant}, set}), tenant)
	sql, _ := insertSQL(res.Table, cols)
	return pgx.BeginFunc(context.Background(), e.owner, func(tx pgx.Tx) error {
		if _, err := tx.Exec(context.Background(), "SELECT set_config('ks.cross_tenant','on',true)"); err != nil {
			return err
		}
		_, err := tx.Exec(context.Background(), sql, args...)
		return err
	})
}

// ActionRules proves every action's Requires and input validation, through the
// generated adapters wrapped around a stub (§16.2).
func ActionRules(t *testing.T, app *model.App, handlers []*runtime.Handler) {
	for _, h := range handlers {
		h := h
		if h.Kind == model.KindJob || h.Kind == model.KindSchedule || h.Spec.Public {
			continue
		}
		t.Run(h.Ref, func(t *testing.T) {
			roles, all, ok := rolesOnly(h.Spec.Requires)
			e := New(t, app)
			org := e.Tenant()
			run := func(a ks.Actor, body map[string]any) error {
				raw, _ := json.Marshal(body)
				_, err := e.App.Run(context.Background(), h, a, &runtime.Call{Actor: a, Body: raw, Key: string(ks.NewID())}, nil)
				return err
			}
			if ok && !all {
				denied := e.Actor(org, without(app.Roles, roles)...)
				if code := code(run(denied, map[string]any{})); code != ks.CodeForbidden {
					t.Errorf("roles %v lack %v: got %q, want forbidden", denied.Roles, roles, code)
				}
			} else if !ok {
				t.Logf("Requires is not role-only: %s", policy.Canonical(h.Spec.Requires))
			}
			// Input validation runs before the action, as an actor who may call it.
			caller := e.Actor(org, toAny(app.Roles)...)
			for _, f := range h.Spec.Input {
				for _, bad := range badInputs(f) {
					if code := code(run(caller, map[string]any{f.Name: bad})); code != ks.CodeInvalid {
						t.Errorf("%s = %v: got %q, want invalid", f.Name, short(bad), code)
					}
				}
			}
		})
	}
}

// badInputs are values just outside what a field accepts.
func badInputs(f model.Field) []any {
	var out []any
	if f.Type.MaxLen != nil {
		out = append(out, strings.Repeat("x", *f.Type.MaxLen+1))
	}
	switch f.Type.Kind {
	case model.KindEmail:
		out = append(out, "not-an-email")
	case model.KindID, model.KindRef:
		out = append(out, "not-a-uuid")
	case model.KindEnum:
		out = append(out, "not-a-member")
	}
	return out
}

// code is an error's wire code, or "" for success.
func code(err error) string {
	if err == nil {
		return ""
	}
	return ks.AsError(err).Code
}

func short(v any) any {
	if s, ok := v.(string); ok && len(s) > 12 {
		return s[:12] + "…"
	}
	return v
}

// Fuzz runs an action repeatedly with random valid inputs as random actors in two tenants,
// and asserts after every run that it did not fail internally and that no response holds
// a row of a tenant other than the caller's (§16.4). Ids in the input are drawn from rows
// of both tenants, so a missing scope shows up as a leak. KS_FUZZ_SEED replays a run.
//
// ponytail: the property is tenant isolation plus "no 500"; invariants rely on the
// database's CHECKs and the pre-commit check rather than being re-evaluated here.
func Fuzz[C, In, Out any](e *Env, wrap func(func(*C, In) (Out, error)) *runtime.Handler, fn func(*C, In) (Out, error), runs int) {
	e.T.Helper()
	seed := time.Now().UnixNano()
	if v, err := strconv.ParseInt(os.Getenv("KS_FUZZ_SEED"), 10, 64); err == nil {
		seed = v
	}
	rng := rand.New(rand.NewSource(seed))
	h := wrap(fn)
	tenants := []ks.ID{e.Tenant(), e.Tenant()}
	ids := map[string][]ks.ID{} // referenced resource -> rows across both tenants
	for _, f := range h.Spec.Input {
		if f.Ref == nil {
			continue
		}
		for _, t := range tenants {
			for i := 0; i < 3; i++ {
				ids[f.Ref.Resource] = append(ids[f.Ref.Resource], e.Fixture(f.Ref.Resource, t))
			}
		}
	}
	for run := 0; run < runs; run++ {
		tenant := tenants[rng.Intn(len(tenants))]
		var roles []any
		for _, r := range e.Model.Roles {
			if rng.Intn(2) == 0 {
				roles = append(roles, r)
			}
		}
		actor := e.Actor(tenant, roles...)
		body := map[string]any{}
		for _, f := range h.Spec.Input {
			if f.Optional && rng.Intn(2) == 0 {
				continue
			}
			if v, ok := randomValue(rng, f, ids, e.Clock.Now()); ok {
				body[f.Name] = v
			}
		}
		raw, _ := json.Marshal(body)
		out, err := e.App.Run(context.Background(), h, actor, &runtime.Call{Actor: actor, Body: raw, Key: string(ks.NewID())}, nil)
		if code(err) == ks.CodeInternal {
			e.T.Fatalf("seed %d run %d: %s as %v with %s failed internally: %v", seed, run, h.Ref, actor.Roles, raw, errors.Unwrap(err))
		}
		if err != nil || len(out.Body) == 0 {
			continue
		}
		var tree any
		if json.Unmarshal(out.Body, &tree) == nil {
			if leaked := foreignTenant(tree, string(tenant)); leaked != "" {
				e.T.Fatalf("seed %d run %d: %s as tenant %s returned a row of tenant %s", seed, run, h.Ref, tenant, leaked)
			}
		}
	}
}

// foreignTenant finds a TenantID in a response that is not the caller's.
func foreignTenant(v any, mine string) string {
	switch v := v.(type) {
	case map[string]any:
		for k, x := range v {
			if s, ok := x.(string); ok && (k == "TenantID" || k == "tenant_id") && s != "" && s != mine {
				return s
			}
			if leaked := foreignTenant(x, mine); leaked != "" {
				return leaked
			}
		}
	case []any:
		for _, x := range v {
			if leaked := foreignTenant(x, mine); leaked != "" {
				return leaked
			}
		}
	}
	return ""
}

// randomValue draws a value the field's declaration accepts.
func randomValue(rng *rand.Rand, f model.Field, ids map[string][]ks.ID, now time.Time) (any, bool) {
	if f.Ref != nil {
		pool := ids[f.Ref.Resource]
		if len(pool) == 0 {
			return nil, false
		}
		return pool[rng.Intn(len(pool))], true
	}
	switch f.Type.Kind {
	case model.KindText, model.KindURL:
		n := 1 + rng.Intn(16)
		if f.Type.MaxLen != nil && *f.Type.MaxLen < n {
			n = *f.Type.MaxLen
		}
		letters := make([]byte, n)
		for i := range letters {
			letters[i] = byte('a' + rng.Intn(26))
		}
		if f.Type.Kind == model.KindURL {
			return "https://example.test/" + string(letters), true
		}
		return string(letters), true
	case model.KindEmail:
		return fmt.Sprintf("u%d@example.test", rng.Intn(1_000_000)), true
	case model.KindInt:
		return rng.Intn(1000), true
	case model.KindBool:
		return rng.Intn(2) == 0, true
	case model.KindEnum:
		if len(f.Type.Values) > 0 {
			return f.Type.Values[rng.Intn(len(f.Type.Values))], true
		}
	case model.KindMoney:
		return ks.MoneyOf(int64(1+rng.Intn(100000)), currency(f)), true
	case model.KindTimestamp:
		return now.Add(time.Duration(rng.Intn(1000)) * time.Hour), true
	case model.KindID:
		return ks.NewID(), true
	}
	return nil, false
}

// Bench measures an action through the full pipeline (§11.4): one actor holding every
// role, inputs drawn as Fuzz draws them but only from the actor's own tenant, so most
// calls succeed. It reports latency percentiles and the share of calls that failed;
// go test -benchmem adds allocations per call. Rate limits are lifted: they would
// measure the limiter, not the action.
func Bench[C, In, Out any](b *testing.B, app *model.App, wrap func(func(*C, In) (Out, error)) *runtime.Handler, fn func(*C, In) (Out, error)) {
	e := New(b, app)
	h := wrap(fn)
	h.Spec.Limits.RateLimit = nil
	tenant := e.Tenant()
	actor := e.Actor(tenant, toAny(app.Roles)...)
	ids := map[string][]ks.ID{}
	for _, f := range h.Spec.Input {
		if f.Ref != nil {
			for i := 0; i < 5; i++ {
				ids[f.Ref.Resource] = append(ids[f.Ref.Resource], e.Fixture(f.Ref.Resource, tenant))
			}
		}
	}
	e.App.OnCall = nil // traces are not what is measured
	rng := rand.New(rand.NewSource(1))
	bodies := make([][]byte, 64)
	for i := range bodies {
		body := map[string]any{}
		for _, f := range h.Spec.Input {
			if v, ok := randomValue(rng, f, ids, e.Clock.Now()); ok && (!f.Optional || rng.Intn(2) == 0) {
				body[f.Name] = v
			}
		}
		bodies[i], _ = json.Marshal(body)
	}
	lat := make([]time.Duration, 0, b.N)
	failed := 0
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		start := time.Now()
		_, err := e.App.Run(context.Background(), h, actor, &runtime.Call{Actor: actor, Body: bodies[i%len(bodies)], Key: string(ks.NewID())}, nil)
		lat = append(lat, time.Since(start))
		if err != nil {
			failed++
		}
	}
	b.StopTimer()
	sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
	at := func(p float64) float64 { return float64(lat[int(p*float64(len(lat)-1))].Microseconds()) / 1000 }
	b.ReportMetric(at(0.50), "p50-ms")
	b.ReportMetric(at(0.95), "p95-ms")
	b.ReportMetric(at(0.99), "p99-ms")
	b.ReportMetric(float64(failed)/float64(b.N), "err/op")
}
