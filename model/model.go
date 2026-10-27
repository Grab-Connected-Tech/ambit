// Package model holds the canonical app model: the single source of truth the spec
// compiles into and that codegen, migrations, clients and the behavior diff derive
// from (§6).
package model

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/abhilashiig/ambit/policy"
)

// SchemaVersion is the format version of the model document.
const SchemaVersion = 1

// App is a whole application.
type App struct {
	SchemaVersion int      `json:"schemaVersion"`
	App           AppInfo  `json:"app"`
	Defaults      Defaults `json:"defaults"`
	Roles         []string `json:"roles"`
	Tenant        *Tenant  `json:"tenant,omitempty"`
	Membership    *Tenant  `json:"membership,omitempty"`
	Account       *Tenant  `json:"account,omitempty"`
	Identity      Identity `json:"identity"`
	Modules       []Module `json:"modules"`
	Limits        Limits   `json:"limits"`
	// Notes carries the app-level declarations that do not change behavior but
	// must survive a round trip for the diff to explain itself.
	Notes []string `json:"notes,omitempty"`
}

// AppInfo names the app and its tenant resource.
type AppInfo struct {
	Name   string `json:"name"`
	Tenant string `json:"tenant,omitempty"`
}

// Defaults are the per-action limits applied when an action declares none (§18.2).
type Defaults struct {
	QueryBudget    int    `json:"queryBudget"`
	RowBudget      int    `json:"rowBudget"`
	ActionTimeout  string `json:"actionTimeout"`
	StmtTimeout    string `json:"statementTimeout"`
	MaxPageSize    int    `json:"maxPageSize"`
	BodyLimitBytes int64  `json:"requestBodyBytes"`
	MaxInFlight    int    `json:"maxInFlight"`
	Compatibility  string `json:"compatibility,omitempty"`
}

// Limits are app-wide request protections (§9.3).
type Limits struct {
	CORS      []string `json:"cors,omitempty"`
	ShedAfter int      `json:"shedAfterQueueMs,omitempty"`
	Retries   int      `json:"serializationRetries,omitempty"`
}

// Tenant is a generated framework resource (the tenant table and memberships).
type Tenant struct {
	Name   string  `json:"name"`
	Table  string  `json:"table"`
	Fields []Field `json:"fields"`
	// Access is what the spec declared for an app-level resource (the tenant and the
	// account); the framework's own Membership has none.
	Access *Access `json:"access,omitempty"`
}

// AccessOrNone returns the declared access, or no rules (every operation denied).
func (t Tenant) AccessOrNone() Access {
	if t.Access == nil {
		return Access{}
	}
	return *t.Access
}

// Module is one folder of spec plus actions (§4).
type Module struct {
	Name      string     `json:"name"`
	Resources []Resource `json:"resources"`
	Actions   []Action   `json:"actions"`
}

// ResourceKind separates the tenant resource from ordinary ones.
type Resource struct {
	Name       string      `json:"name"`
	Table      string      `json:"table"`
	Tenant     bool        `json:"tenant,omitempty"`
	Account    bool        `json:"account,omitempty"`
	Fields     []Field     `json:"fields"`
	Indexes    []Index     `json:"indexes,omitempty"`
	Invariants []Invariant `json:"invariants,omitempty"`
	Access     Access      `json:"access"`
	Timestamps bool        `json:"timestamps"`
	Versioned  bool        `json:"versioned"`
}

// IDField is the name of the primary key on every resource.
const IDField = "id"

// BuiltInFields are added to every resource automatically (§5.2).
func BuiltInFields(tenant bool) []Field {
	out := []Field{
		{Name: "id", GoName: "ID", Column: "id", Type: Type{Kind: KindID}},
		{Name: "created_at", GoName: "CreatedAt", Column: "created_at", Type: Type{Kind: KindTimestamp}, Immutable: true, Default: &Value{Kind: "now"}},
		{Name: "updated_at", GoName: "UpdatedAt", Column: "updated_at", Type: Type{Kind: KindTimestamp}, Default: &Value{Kind: "now"}},
		{Name: "version", GoName: "Version", Column: "version", Type: Type{Kind: KindInt}, Default: &Value{Kind: "int", Text: "1"}},
	}
	if tenant {
		out = append([]Field{{Name: "tenant_id", GoName: "TenantID", Column: "tenant_id", Type: Type{Kind: KindID}, Immutable: true}}, out...)
	}
	return out
}

// Field is one column, or one relation.
type Field struct {
	Name      string   `json:"name"`
	GoName    string   `json:"goName"`
	Column    string   `json:"column"`
	Columns   []string `json:"columns,omitempty"` // money splits into minor and currency
	Type      Type     `json:"type"`
	Optional  bool     `json:"optional,omitempty"`
	Default   *Value   `json:"default,omitempty"`
	Unique    bool     `json:"unique,omitempty"`
	Immutable bool     `json:"immutable,omitempty"`
	Sensitive bool     `json:"sensitive,omitempty"`
	Ref       *Ref     `json:"ref,omitempty"`
	// RenamedFrom marks a rename so the diff and migrations see one change, not two (§13.2).
	RenamedFrom string `json:"renamedFrom,omitempty"`
}

// Ref points a belongs-to field at another resource.
type Ref struct {
	Resource string `json:"resource"` // "customers.Customer"
	OnDelete string `json:"onDelete"` // restrict|cascade|set_null
}

// Value is a typed constant stored as canonical text.
type Value struct {
	Kind string `json:"kind"`
	Text string `json:"text"`
}

// TypeKind enumerates the field builders in the spec language (§5.2).
type TypeKind string

const (
	KindText      TypeKind = "text"
	KindInt       TypeKind = "int"
	KindDecimal   TypeKind = "decimal"
	KindMoney     TypeKind = "money"
	KindBool      TypeKind = "bool"
	KindTimestamp TypeKind = "timestamp"
	KindDate      TypeKind = "date"
	KindEnum      TypeKind = "enum"
	KindEmail     TypeKind = "email"
	KindURL       TypeKind = "url"
	KindJSON      TypeKind = "json"
	KindBytes     TypeKind = "bytes"
	KindID        TypeKind = "id"
	KindRef       TypeKind = "ref"
)

// Type describes a field's storage and validation parameters.
type Type struct {
	Kind      TypeKind `json:"kind"`
	MinLen    *int     `json:"minLen,omitempty"`
	MaxLen    *int     `json:"maxLen,omitempty"`
	Pattern   string   `json:"pattern,omitempty"`
	Min       string   `json:"min,omitempty"`
	Max       string   `json:"max,omitempty"`
	Precision int      `json:"precision,omitempty"`
	Scale     int      `json:"scale,omitempty"`
	Values    []string `json:"values,omitempty"`
	Currency  string   `json:"currency,omitempty"`
	Schemes   []string `json:"schemes,omitempty"`
	GoType    string   `json:"goType,omitempty"`
	Schema    string   `json:"schema,omitempty"`
	MaxSize   int      `json:"maxSize,omitempty"`
	Encrypted bool     `json:"encrypted,omitempty"`
	Trim      bool     `json:"trim,omitempty"`
	// Inner describes the wrapped field of an Encrypted field (§5.2).
	Inner *Type `json:"inner,omitempty"`
}

// Index is a database index declaration (§5.3).
type Index struct {
	Name   string       `json:"name"`
	Fields []string     `json:"fields"`
	Unique bool         `json:"unique,omitempty"`
	Where  *policy.Node `json:"where,omitempty"`
}

// Invariant is a named rule that must hold for every row (§5.5).
type Invariant struct {
	Name string       `json:"name"`
	Expr *policy.Node `json:"expr"`
	// Enforcement is derived: "check" for row-local rules, "deferred" for rules
	// that traverse a relation.
	Enforcement string `json:"enforcement"`
}

// Access holds the deny-by-default policy for a resource (§5.6).
type Access struct {
	Read        *policy.Node `json:"read,omitempty"`
	Create      *policy.Node `json:"create,omitempty"`
	Update      *policy.Node `json:"update,omitempty"`
	UpdateAfter *policy.Node `json:"updateAfter,omitempty"`
	Delete      *policy.Node `json:"delete,omitempty"`
	FieldRead   []FieldRule  `json:"fieldRead,omitempty"`
	FieldWrite  []FieldRule  `json:"fieldWrite,omitempty"`
}

// FieldRule is one field-level read or write rule.
type FieldRule struct {
	Field string       `json:"field"`
	Expr  *policy.Node `json:"expr"`
}

// ActionKind is one of the four kinds of behavior (§5.7).
type ActionKind string

const (
	KindQuery    ActionKind = "query"
	KindMutation ActionKind = "mutation"
	KindJob      ActionKind = "job"
	KindSchedule ActionKind = "schedule"
)

// RunAs values for jobs (§5.7).
const (
	RunAsSystem   = "system"
	RunAsEnqueuer = "enqueuer"
)

// Action is a named unit of behavior.
type Action struct {
	Name         string        `json:"name"`
	Kind         ActionKind    `json:"kind"`
	Requires     *policy.Node  `json:"requires,omitempty"`
	Public       bool          `json:"public,omitempty"`
	Input        []Field       `json:"input,omitempty"`
	Output       Output        `json:"output"`
	Reads        []string      `json:"reads,omitempty"`
	Writes       []string      `json:"writes,omitempty"`
	Calls        []string      `json:"calls,omitempty"`
	Effects      []Effect      `json:"effects,omitempty"`
	Errors       []ActionError `json:"errors,omitempty"`
	Limits       ActionLimits  `json:"limits"`
	Retry        *Retry        `json:"retry,omitempty"`
	Concurrency  *Concurrency  `json:"concurrency,omitempty"`
	RunAs        string        `json:"runAs,omitempty"`
	Cron         string        `json:"cron,omitempty"`
	Runs         string        `json:"runs,omitempty"`
	CrossTenant  bool          `json:"crossTenant,omitempty"`
	Idempotent   bool          `json:"idempotent,omitempty"`
	Counts       bool          `json:"counts,omitempty"`
	TimeZone     string        `json:"timezone,omitempty"`
	Serializable bool          `json:"serializable,omitempty"`
	ReplicaOK    bool          `json:"replicaOK,omitempty"`
	AllowGet     bool          `json:"get,omitempty"`
	Deprecated   string        `json:"deprecated,omitempty"`
	Cache        *Cache        `json:"cache,omitempty"`
	// Implementation is the expected Go signature, checked at build time (§7.2).
	Signature string `json:"signature,omitempty"`
}

// ActionError is a declared, typed failure of one action.
type ActionError struct {
	Code    string `json:"code"`
	Message string `json:"message,omitempty"`
}

// Output names the result shape: a resource, fields, or a page of a resource.
type Output struct {
	Resource string  `json:"resource,omitempty"`
	Page     string  `json:"page,omitempty"`
	Fields   []Field `json:"fields,omitempty"`
	None     bool    `json:"none,omitempty"`
}

// ActionLimits are the per-request and per-query ceilings (§8.4, §18.2).
type ActionLimits struct {
	RateLimit   []RateLimit `json:"rateLimit,omitempty"`
	Timeout     string      `json:"timeout,omitempty"`
	StmtTimeout string      `json:"statementTimeout,omitempty"`
	QueryBudget int         `json:"queryBudget,omitempty"`
	RowBudget   int         `json:"rowBudget,omitempty"`
}

// RateLimit is one declared token bucket (§5.7).
type RateLimit struct {
	Scope string `json:"scope"` // actor|ip|tenant
	Limit int    `json:"limit"`
	Per   string `json:"per"` // canonical duration, e.g. "1m0s"
}

// Retry is a job retry policy (§5.7).
type Retry struct {
	MaxAttempts int    `json:"maxAttempts"`
	Backoff     string `json:"backoff"` // fixed|exponential
	Base        string `json:"base"`
	Cap         string `json:"cap"`
}

// Concurrency serializes jobs by key (§10.4).
type Concurrency struct {
	Key   string `json:"key,omitempty"`
	Limit int    `json:"limit"`
}

// Cache declares a cached query with its invalidators (§11.3).
type Cache struct {
	TTL           string   `json:"ttl"`
	InvalidatedBy []string `json:"invalidatedBy,omitempty"`
}

// EffectKind enumerates the side effects the framework can run (§5.8).
type EffectKind string

const (
	EffectEmail   EffectKind = "email"
	EffectSMS     EffectKind = "sms"
	EffectHTTP    EffectKind = "http"
	EffectWebhook EffectKind = "webhook"
	EffectStorage EffectKind = "storage"
	EffectEnqueue EffectKind = "enqueue"
)

// Effect is one declared side effect with its constraints.
type Effect struct {
	Type        EffectKind `json:"type"`
	Mode        string     `json:"mode"` // deferred|immediate
	Hosts       []string   `json:"hosts,omitempty"`
	Topic       string     `json:"topic,omitempty"`
	Bucket      string     `json:"bucket,omitempty"`
	Template    string     `json:"template,omitempty"`
	Job         string     `json:"job,omitempty"`
	After       string     `json:"after,omitempty"`
	Compensates string     `json:"compensates,omitempty"`
}

// Identity is the app's authentication surface (§9.2).
type Identity struct {
	Sessions  *Sessions  `json:"sessions,omitempty"`
	APITokens *APITokens `json:"apiTokens,omitempty"`
	OIDC      []OIDC     `json:"oidc,omitempty"`
	Passwords *Passwords `json:"passwords,omitempty"`
}

// Sessions configures cookie sessions.
type Sessions struct {
	TTL         string `json:"ttl"`
	IdleTimeout string `json:"idleTimeout,omitempty"`
	Rotate      bool   `json:"rotate"`
}

// APITokens configures hashed bearer tokens with optional scopes.
type APITokens struct {
	Scoped bool `json:"scoped"`
}

// OIDC names one login provider.
type OIDC struct {
	Provider string `json:"provider"`
}

// Passwords configures the optional password module.
type Passwords struct {
	Algorithm    string `json:"algorithm"`
	BreachChecks bool   `json:"breachChecks"`
}

// ---- lookup helpers ----

// Name returns the app name.
func (a *App) Name() string {
	if a == nil {
		return ""
	}
	return a.App.Name
}

// Qual formats a qualified name such as "billing.Invoice".
func Qual(module, name string) string { return module + "." + name }

// SplitRef divides "billing.Invoice" into its parts.
func SplitRef(ref string) (module, name string) {
	for i := 0; i < len(ref); i++ {
		if ref[i] == '.' {
			return ref[:i], ref[i+1:]
		}
	}
	return "", ref
}

// FindResource locates a resource by qualified name. App-level resources are
// addressed as "app.Org" from specs and by bare name elsewhere (§4.2).
func (a *App) FindResource(ref string) *Resource {
	for m := range a.Modules {
		for r := range a.Modules[m].Resources {
			if Qual(a.Modules[m].Name, a.Modules[m].Resources[r].Name) == ref {
				return &a.Modules[m].Resources[r]
			}
		}
	}
	_, name := SplitRef(ref)
	isTenant := func(n string) bool { return a.App.Tenant != "" && n == a.App.Tenant }
	isAccount := func(n string) bool { return a.Account != nil && n == a.Account.Name }
	for _, fw := range a.Framework() {
		if fw.Name == ref || fw.Name == name || fw.Table == ref {
			return &Resource{
				Name: fw.Name, Table: fw.Table, Fields: fw.Fields,
				Tenant:  isTenant(fw.Name) || isAccount(fw.Name),
				Account: isAccount(fw.Name),
				Access:  fw.AccessOrNone(),
			}
		}
	}
	return nil
}

// Framework lists the app-level resources that exist outside modules: the tenant
// table, the account table and memberships (§4.2).
func (a *App) Framework() []Tenant {
	var out []Tenant
	if a.Tenant != nil {
		out = append(out, *a.Tenant)
	}
	if a.Account != nil {
		out = append(out, *a.Account)
	}
	if a.Membership != nil {
		out = append(out, *a.Membership)
	}
	return out
}

// ResourceModule returns the module that owns a resource.
func (a *App) ResourceModule(ref string) string {
	m, _ := SplitRef(ref)
	return m
}

// FindAction locates an action by qualified name.
func (a *App) FindAction(ref string) (*Module, *Action) {
	mo, na := SplitRef(ref)
	for m := range a.Modules {
		if a.Modules[m].Name != mo {
			continue
		}
		for i := range a.Modules[m].Actions {
			if a.Modules[m].Actions[i].Name == na {
				return &a.Modules[m], &a.Modules[m].Actions[i]
			}
		}
	}
	return nil, nil
}

// FindField locates a field on a resource.
func (r *Resource) FindField(name string) *Field {
	for i := range r.Fields {
		if r.Fields[i].Name == name {
			return &r.Fields[i]
		}
	}
	return nil
}

// Columns returns every SQL column a resource maps to, in a stable order.
func (r *Resource) Columns() []string {
	var out []string
	for _, f := range r.Fields {
		if len(f.Columns) > 0 {
			out = append(out, f.Columns...)
			continue
		}
		if f.Type.Kind == KindRef {
			continue // stored in Column
		}
		out = append(out, f.Column)
	}
	return out
}

// TableName returns the SQL table for a qualified resource name.
func (a *App) TableName(ref string) string {
	if r := a.FindResource(ref); r != nil {
		return r.Table
	}
	return ""
}

// FieldRead returns the read rule for one field, or nil.
func (ac Access) FieldReadFor(field string) *policy.Node {
	for _, fr := range ac.FieldRead {
		if fr.Field == field {
			return fr.Expr
		}
	}
	return nil
}

// FieldWriteFor returns the write rule for one field, or nil.
func (ac Access) FieldWriteFor(field string) *policy.Node {
	for _, fr := range ac.FieldWrite {
		if fr.Field == field {
			return fr.Expr
		}
	}
	return nil
}

// AllActions walks every action with its module.
func (a *App) AllActions() []struct {
	Module string
	Action *Action
} {
	var out []struct {
		Module string
		Action *Action
	}
	for m := range a.Modules {
		for i := range a.Modules[m].Actions {
			out = append(out, struct {
				Module string
				Action *Action
			}{a.Modules[m].Name, &a.Modules[m].Actions[i]})
		}
	}
	return out
}

// AllResources walks every resource with its qualified name.
func (a *App) AllResources() []struct {
	Ref      string
	Resource *Resource
} {
	var out []struct {
		Ref      string
		Resource *Resource
	}
	for m := range a.Modules {
		for i := range a.Modules[m].Resources {
			out = append(out, struct {
				Ref      string
				Resource *Resource
			}{Qual(a.Modules[m].Name, a.Modules[m].Resources[i].Name), &a.Modules[m].Resources[i]})
		}
	}
	for _, fw := range a.Framework() {
		out = append(out, struct {
			Ref      string
			Resource *Resource
		}{fw.Name, &Resource{Name: fw.Name, Table: fw.Table, Fields: fw.Fields, Access: fw.AccessOrNone()}})
	}
	return out
}

// ---- canonical form ----

// Canonical sorts every collection so two equal apps produce identical bytes.
func (a *App) Canonical() {
	sort.Strings(a.Roles)
	sort.Strings(a.Notes)
	sort.Slice(a.Modules, func(i, j int) bool { return a.Modules[i].Name < a.Modules[j].Name })
	for m := range a.Modules {
		mo := &a.Modules[m]
		sort.Slice(mo.Resources, func(i, j int) bool { return mo.Resources[i].Name < mo.Resources[j].Name })
		sort.Slice(mo.Actions, func(i, j int) bool { return mo.Actions[i].Name < mo.Actions[j].Name })
		for r := range mo.Resources {
			res := &mo.Resources[r]
			sort.Slice(res.Fields, func(i, j int) bool { return res.Fields[i].Name < res.Fields[j].Name })
			for f := range res.Fields {
				sort.Strings(res.Fields[f].Type.Values)
				sort.Strings(res.Fields[f].Type.Schemes)
				// money columns keep their (minor, currency) order: it is the
				// storage layout, not a set.
			}
			sort.Slice(res.Indexes, func(i, j int) bool { return res.Indexes[i].Name < res.Indexes[j].Name })
			sort.Slice(res.Invariants, func(i, j int) bool { return res.Invariants[i].Name < res.Invariants[j].Name })
			sort.Slice(res.Access.FieldRead, func(i, j int) bool { return res.Access.FieldRead[i].Field < res.Access.FieldRead[j].Field })
			sort.Slice(res.Access.FieldWrite, func(i, j int) bool { return res.Access.FieldWrite[i].Field < res.Access.FieldWrite[j].Field })
			res.Access.FieldRead = dedupFieldRules(res.Access.FieldRead)
			res.Access.FieldWrite = dedupFieldRules(res.Access.FieldWrite)
		}
		for i := range mo.Actions {
			act := &mo.Actions[i]
			sort.Strings(act.Reads)
			sort.Strings(act.Writes)
			sort.Strings(act.Calls)
			sort.Slice(act.Effects, func(i, j int) bool {
				if act.Effects[i].Type != act.Effects[j].Type {
					return act.Effects[i].Type < act.Effects[j].Type
				}
				return act.Effects[i].String() < act.Effects[j].String()
			})
			act.Effects = dedupEffects(act.Effects)
			sort.Slice(act.Errors, func(i, j int) bool { return act.Errors[i].Code < act.Errors[j].Code })
			sort.Slice(act.Limits.RateLimit, func(i, j int) bool { return act.Limits.RateLimit[i].Scope < act.Limits.RateLimit[j].Scope })
			for f := range act.Input {
				sort.Strings(act.Input[f].Type.Values)
				sort.Strings(act.Input[f].Type.Schemes)
			}
			sort.Slice(act.Output.Fields, func(i, j int) bool { return act.Output.Fields[i].Name < act.Output.Fields[j].Name })
			sort.Strings(act.Cache.invalidatable())
			for e := range act.Errors {
				if act.Errors[e].Code == "" {
					continue
				}
			}
		}
	}
}

// invalidatable keeps cache targets sorted.
func (c *Cache) invalidatable() []string {
	if c == nil {
		return nil
	}
	sort.Strings(c.InvalidatedBy)
	return c.InvalidatedBy
}

func dedupFieldRules(in []FieldRule) []FieldRule {
	seen := map[string]bool{}
	var out []FieldRule
	for _, r := range in {
		if seen[r.Field] {
			continue
		}
		seen[r.Field] = true
		out = append(out, r)
	}
	return out
}

func dedupEffects(in []Effect) []Effect {
	seen := map[string]bool{}
	var out []Effect
	for _, e := range in {
		k := e.String()
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, e)
	}
	return out
}

// String renders an effect as a stable key for dedup and diffing.
func (e Effect) String() string {
	b, _ := json.Marshal(e)
	return string(b)
}

// Bytes returns the canonical JSON document.
func (a *App) Bytes() []byte {
	a.Canonical()
	a.SchemaVersion = SchemaVersion
	normalizeNodes(a)
	buf, err := json.MarshalIndent(a, "", "  ")
	if err != nil {
		panic("model: canonical encoding failed: " + err.Error())
	}
	return append(buf, '\n')
}

// normalizeNodes replaces every rule with its canonical tree, so an equivalent
// rule written two different ways hashes the same.
func normalizeNodes(a *App) {
	fix := func(n **policy.Node) {
		if *n != nil {
			*n = policy.Normalize(*n)
		}
	}
	access := func(ac *Access) {
		fix(&ac.Read)
		fix(&ac.Create)
		fix(&ac.Update)
		fix(&ac.UpdateAfter)
		fix(&ac.Delete)
		for i := range ac.FieldRead {
			fix(&ac.FieldRead[i].Expr)
		}
		for i := range ac.FieldWrite {
			fix(&ac.FieldWrite[i].Expr)
		}
	}
	for _, t := range []*Tenant{a.Tenant, a.Account} {
		if t != nil && t.Access != nil {
			access(t.Access)
		}
	}
	for m := range a.Modules {
		for r := range a.Modules[m].Resources {
			res := &a.Modules[m].Resources[r]
			access(&res.Access)
			for i := range res.Indexes {
				if res.Indexes[i].Where != nil {
					res.Indexes[i].Where = policy.Normalize(res.Indexes[i].Where)
				}
			}
			for i := range res.Invariants {
				res.Invariants[i].Expr = policy.Normalize(res.Invariants[i].Expr)
			}
		}
		for i := range a.Modules[m].Actions {
			fix(&a.Modules[m].Actions[i].Requires)
		}
	}
}

// Hash is the identity used by am check (AM-CODE-001) and am diff.
func (a *App) Hash() string {
	sum := sha256.Sum256(a.Bytes())
	return hex.EncodeToString(sum[:])
}

// Load parses a model document.
func Load(b []byte) (*App, error) {
	var a App
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&a); err != nil {
		return nil, fmt.Errorf("model: parse: %w", err)
	}
	if a.SchemaVersion != SchemaVersion {
		return nil, fmt.Errorf("model: schemaVersion %d, this build understands %d", a.SchemaVersion, SchemaVersion)
	}
	return &a, nil
}

// Clone deep-copies a model through its canonical encoding.
func (a *App) Clone() *App {
	out, err := Load(a.Bytes())
	if err != nil {
		panic("model: clone failed: " + err.Error())
	}
	return out
}
