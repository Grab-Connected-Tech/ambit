// Package spec contains the declarative builders used in spec files (§5). It is
// pure declaration: importing it runs no server, opens no connection and reads no
// configuration. ks gen executes these declarations once at build time and writes
// the app model they describe.
//
// The two option interfaces are the point: a resource option cannot appear in an
// action declaration, and vice versa, because the Go type system refuses it. That
// is "guarantees over guidelines" applied to the spec language itself (§2.1).
package spec

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"runtime"
	"sort"
	"strings"
	"time"

	"keystone.dev/keystone/model"
	"keystone.dev/keystone/policy"
)

// Durations are named so specs read as text: s.TTL(30 * s.Day).
const (
	Second = time.Second
	Minute = time.Minute
	Hour   = time.Hour
	Day    = 24 * time.Hour
)

// ROption configures a resource.
type ROption interface{ applyResource(*ResourceDecl) }

// AOption configures an action.
type AOption interface{ applyAction(*ActionDecl) }

// registryT holds every declaration, keyed by pointer identity so cross-package
// references resolve without strings or registration calls.
type registryT struct {
	resources  []*ResourceDecl
	byRes      map[*ResourceDecl]string
	actions    []*ActionDecl
	byAction   map[*ActionDecl]string
	roles      map[string]*RoleDecl
	modules    map[string]*moduleDecl
	identity   *model.Identity
	tenant     *ResourceDecl
	accountRes *ResourceDecl
	appName    string
	defaults   *model.Defaults
	errs       []string
}

var registry = registryT{
	byRes:    map[*ResourceDecl]string{},
	byAction: map[*ActionDecl]string{},
	roles:    map[string]*RoleDecl{},
	modules:  map[string]*moduleDecl{},
}

type moduleDecl struct {
	name      string
	resources []*ResourceDecl
	actions   []*ActionDecl
}

// callerModule derives a module name from the declaring package, which is how a
// spec file needs no registration call of its own (§2.4: no init-time registration).
func callerModule() string {
	for depth := 2; depth < 12; depth++ {
		pc, _, _, ok := runtime.Caller(depth)
		if !ok {
			break
		}
		fn := runtime.FuncForPC(pc)
		if fn == nil {
			continue
		}
		name := fn.Name()
		dot := strings.LastIndex(name, ".")
		if dot <= 0 {
			continue
		}
		pkg := name[:dot]
		if pkg == "keystone.dev/keystone/spec" {
			continue
		}
		if i := strings.LastIndex(pkg, "/"); i >= 0 {
			return pkg[i+1:]
		}
		return pkg
	}
	return "app"
}

func callerLine() int {
	for depth := 2; depth < 10; depth++ {
		_, file, line, ok := runtime.Caller(depth)
		if !ok {
			break
		}
		if strings.Contains(file, "/keystone/spec/") {
			continue
		}
		return line
	}
	return 0
}

func (d *ResourceDecl) register() {
	d.module = callerModule()
	d.line = callerLine()
	registry.resources = append(registry.resources, d)
	registry.byRes[d] = model.Qual(d.module, d.name)
	registry.module(d.module).resources = append(registry.module(d.module).resources, d)
}

func (d *ActionDecl) register() {
	d.module = callerModule()
	d.line = callerLine()
	registry.actions = append(registry.actions, d)
	registry.byAction[d] = model.Qual(d.module, d.name)
	registry.module(d.module).actions = append(registry.module(d.module).actions, d)
}

func (r *registryT) module(name string) *moduleDecl {
	m := r.modules[name]
	if m == nil {
		m = &moduleDecl{name: name}
		r.modules[name] = m
	}
	return m
}

func specErr(format string, args ...any) {
	registry.errs = append(registry.errs, fmt.Sprintf(format, args...))
}

// ---- building the model ----

// App assembles the declared model. ks gen runs this inside a throwaway program.
func App() (*model.App, error) {
	a := &model.App{
		SchemaVersion: model.SchemaVersion,
		App:           model.AppInfo{Name: appName()},
		Defaults: model.Defaults{
			QueryBudget: 20, RowBudget: 10000, ActionTimeout: "10s", StmtTimeout: "5s",
			MaxPageSize: 100, BodyLimitBytes: 1 << 20, MaxInFlight: 256,
		},
		Limits: model.Limits{Retries: 3},
	}
	for _, r := range sortedRoles() {
		a.Roles = append(a.Roles, r.name)
	}
	a.Identity = *identityOrEmpty()
	if registry.defaults != nil {
		d := *registry.defaults
		if d.QueryBudget != 0 {
			a.Defaults.QueryBudget = d.QueryBudget
		}
		if d.MaxPageSize != 0 {
			a.Defaults.MaxPageSize = d.MaxPageSize
		}
	}

	if registry.accountRes != nil {
		a.Account = &model.Tenant{Name: registry.accountRes.name, Table: accountTable(registry.accountRes), Fields: resourceFields(registry.accountRes)}
	}
	if registry.tenant != nil {
		a.App.Tenant = registry.tenant.name
		a.Tenant = &model.Tenant{
			Name:   registry.tenant.name,
			Table:  snake(registry.tenant.name),
			Fields: resourceFields(registry.tenant),
		}
	}
	a.Membership = &model.Tenant{Name: "Membership", Table: "ks_membership", Fields: membershipFields()}

	names := make([]string, 0, len(registry.modules))
	for n := range registry.modules {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if n == "app" {
			continue // app-level declarations are not a module (§4.2)
		}
		m := registry.modules[n]
		mm := model.Module{Name: n}
		for _, d := range m.resources {
			res, err := buildResource(d)
			if err != nil {
				return nil, err
			}
			mm.Resources = append(mm.Resources, *res)
		}
		for _, d := range m.actions {
			act, err := buildAction(d)
			if err != nil {
				return nil, err
			}
			mm.Actions = append(mm.Actions, *act)
		}
		a.Modules = append(a.Modules, mm)
	}
	if len(registry.errs) > 0 {
		return nil, fmt.Errorf("spec: %s", strings.Join(dedupe(registry.errs), "; "))
	}
	return a, nil
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

func appName() string {
	if registry.appName != "" {
		return registry.appName
	}
	if n := os.Getenv("KS_APP_NAME"); n != "" {
		return n
	}
	return "app"
}

func identityOrEmpty() *model.Identity {
	if registry.identity == nil {
		// Deny by default still needs a way to be anybody, so an undeclared app
		// gets rotating cookie sessions with a two-week TTL.
		return &model.Identity{Sessions: &model.Sessions{TTL: "336h0m0s", Rotate: true}}
	}
	return registry.identity
}

func sortedRoles() []*RoleDecl {
	out := make([]*RoleDecl, 0, len(registry.roles))
	for _, r := range registry.roles {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}

func membershipFields() []model.Field {
	return []model.Field{
		{Name: "id", GoName: "ID", Column: "id", Type: model.Type{Kind: model.KindID}},
		{Name: "tenant_id", GoName: "TenantID", Column: "tenant_id", Type: model.Type{Kind: model.KindID}, Immutable: true},
		{Name: "actor_id", GoName: "ActorID", Column: "actor_id", Type: model.Type{Kind: model.KindID}, Immutable: true},
		{Name: "role", GoName: "Role", Column: "role", Type: model.Type{Kind: model.KindText, MaxLen: intPtr(64)}},
	}
}

func intPtr(i int) *int { return &i }

// resourceFields returns the model fields for the tenant table.
func resourceFields(d *ResourceDecl) []model.Field {
	res, err := buildResource(d)
	if err != nil {
		specErr("%v", err)
		return nil
	}
	return res.Fields
}

// Dump writes the declared app as JSON; this is what the generated dump program calls.
func Dump(w io.Writer) error {
	a, err := App()
	if err != nil {
		return err
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(a)
}

// DumpFile writes the model with deterministic bytes.
func DumpFile(path string) error {
	a, err := App()
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := f.Write(a.Bytes()); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

var _ = policy.Always
