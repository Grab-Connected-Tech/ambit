// Package compile turns a declared spec into a verified app model. It is where the
// promises the spec language makes are actually checked, so nothing below it has to
// defend against a malformed model (§3.2 step 2).
package compile

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/abhilashiig/ambit/model"
	"github.com/abhilashiig/ambit/policy"
)

// Severity names how a diagnostic fails a build (§14.1).
type Severity string

const (
	SevError   Severity = "error"
	SevWarning Severity = "warning"
)

// Diag is one finding, in the shape §14.2 prescribes. Decl names the model element
// so the CLI can resolve it to a file and line through the checker's index.
type Diag struct {
	Rule     string   `json:"rule"`
	Severity Severity `json:"severity"`
	Decl     string   `json:"decl,omitempty"`
	File     string   `json:"file,omitempty"`
	Line     int      `json:"line,omitempty"`
	Column   int      `json:"column,omitempty"`
	Message  string   `json:"message"`
	Fix      string   `json:"fix,omitempty"`
	Docs     string   `json:"docs"`
}

// Diags is a sortable diagnostic list; order is deterministic so CI output diffs.
type Diags []Diag

func (d Diags) Len() int { return len(d) }
func (d Diags) Less(i, j int) bool {
	if d[i].Rule != d[j].Rule {
		return d[i].Rule < d[j].Rule
	}
	if d[i].Decl != d[j].Decl {
		return d[i].Decl < d[j].Decl
	}
	return d[i].Message < d[j].Message
}
func (d Diags) Swap(i, j int) { d[i], d[j] = d[j], d[i] }

// Errors reports whether any diagnostic is an error.
func (d Diags) Errors() int {
	n := 0
	for _, x := range d {
		if x.Severity == SevError {
			n++
		}
	}
	return n
}

// JSON renders the diagnostic list for --json output.
func (d Diags) JSON() string {
	out := append(Diags(nil), d...)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].File != out[j].File {
			return out[i].File < out[j].File
		}
		if out[i].Line != out[j].Line {
			return out[i].Line < out[j].Line
		}
		return out[i].Rule < out[j].Rule
	})
	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return "[]"
	}
	return string(b)
}

func bad(rule, decl, msg, fix string) Diag {
	return Diag{Rule: rule, Severity: SevError, Decl: decl, Message: msg, Fix: fix, Docs: "am explain " + rule}
}

func soft(rule, decl, msg, fix string) Diag {
	return Diag{Rule: rule, Severity: SevWarning, Decl: decl, Message: msg, Fix: fix, Docs: "am explain " + rule}
}

// Parse reads a raw model document produced by the spec program.
func Parse(raw []byte) (*model.App, Diags) {
	a, err := model.Load(raw)
	if err != nil {
		return nil, Diags{bad("AM-CODE-001", "", err.Error(),
			"run am gen to rebuild app.model.json from the spec")}
	}
	return a, Validate(a)
}

// Validate checks every promise the runtime relies on.
func Validate(a *model.App) Diags {
	var d Diags
	if a == nil {
		return Diags{bad("AM-CODE-001", "", "no app model", "run am gen")}
	}
	d = append(d, checkShape(a)...)
	d = append(d, checkRoles(a)...)
	d = append(d, checkOwnership(a)...)
	d = append(d, checkActions(a)...)
	d = append(d, checkCycles(a)...)
	d = append(d, checkSQL(a)...)
	d = append(d, checkFields(a)...)
	sort.SliceStable(d, func(i, j int) bool {
		if d[i].Severity != d[j].Severity {
			return d[i].Severity == SevError
		}
		return d[i].Rule+d[i].Decl < d[j].Rule+d[j].Decl
	})
	return dedup(d)
}

func dedup(in Diags) Diags {
	seen := map[string]bool{}
	var out Diags
	for _, x := range in {
		k := x.Rule + "|" + x.Decl + "|" + x.Message
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, x)
	}
	return out
}

func checkShape(a *model.App) Diags {
	var d Diags
	if a.App.Name == "" {
		d = append(d, bad("AM-SPEC-002", "app", "the app has no name", "declare s.AppName or set app: in am.yaml"))
	}
	if len(a.Roles) == 0 {
		d = append(d, soft("AM-SPEC-002", "app", "no roles are declared, so every Requires must be Public or a field rule",
			"declare at least one role in app/app.go"))
	}
	if a.App.Tenant == "" && len(a.Modules) > 0 {
		d = append(d, bad("AM-SPEC-002", "app", "no tenant resource is declared but modules exist",
			"declare s.TenantResource in app/app.go, or drop s.Tenant from every resource"))
	}
	seenTable := map[string]string{}
	for _, r := range a.AllResources() {
		if prev, dup := seenTable[r.Resource.Table]; dup && prev != r.Ref {
			d = append(d, bad("AM-SPEC-002", r.Ref,
				fmt.Sprintf("tables %q are shared by %s and %s", r.Resource.Table, prev, r.Ref),
				"resource names must differ beyond case; rename one resource"))
		}
		seenTable[r.Resource.Table] = r.Ref
		if len(r.Resource.Fields) == 0 {
			d = append(d, bad("AM-SPEC-002", r.Ref, "the resource declares no fields", "add s.Fields(...)"))
		}
		if r.Resource.FindField("id") == nil {
			d = append(d, bad("AM-SPEC-002", r.Ref, "the resource has no id column", "this is a generator bug: rerun am gen"))
		}
	}
	names := map[string]bool{}
	for _, m := range a.Modules {
		if m.Name == "" {
			d = append(d, bad("AM-SPEC-002", "app", "a module has no name", "name the package that declares the spec"))
		}
		if names[m.Name] {
			d = append(d, bad("AM-SPEC-002", m.Name, "two modules share a name", "rename one module folder"))
		}
		names[m.Name] = true
	}
	return d
}

func checkRoles(a *model.App) Diags {
	var d Diags
	known := map[string]bool{}
	for _, r := range a.Roles {
		known[r] = true
	}
	check := func(decl string, n *policy.Node) {
		if n == nil {
			return
		}
		n.Walk(func(x *policy.Node) bool {
			for _, role := range x.Roles {
				if !known[role] {
					d = append(d, bad("AM-SPEC-002", decl,
						fmt.Sprintf("rule references undeclared role %q", role),
						"declare it with s.Role in app/app.go"))
				}
			}
			return true
		})
	}
	for _, m := range a.Modules {
		for ri := range m.Resources {
			r := &m.Resources[ri]
			decl := model.Qual(m.Name, r.Name)
			for _, n := range []*policy.Node{r.Access.Read, r.Access.Create, r.Access.Update, r.Access.UpdateAfter, r.Access.Delete} {
				check(decl, n)
			}
			for _, fr := range append(append([]model.FieldRule{}, r.Access.FieldRead...), r.Access.FieldWrite...) {
				check(decl+"."+fr.Field, fr.Expr)
			}
		}
		for _, act := range m.Actions {
			check(model.Qual(m.Name, act.Name), act.Requires)
		}
	}
	return d
}

// checkOwnership enforces §4.1: a module owns its resources, and only its actions
// write them. Reads across modules are allowed because the rules still apply.
func checkOwnership(a *model.App) Diags {
	var d Diags
	owner := map[string]string{}
	for _, m := range a.Modules {
		for ri := range m.Resources {
			owner[model.Qual(m.Name, m.Resources[ri].Name)] = m.Name
		}
	}
	for _, m := range a.Modules {
		for i := range m.Actions {
			act := &m.Actions[i]
			decl := model.Qual(m.Name, act.Name)
			for _, w := range act.Writes {
				if owner[w] == "" {
					d = append(d, bad("AM-SPEC-002", decl,
						fmt.Sprintf("the action writes %s, which is not declared", w),
						"fix the resource reference"))
					continue
				}
				if owner[w] != m.Name {
					d = append(d, bad("AM-SPEC-005", decl,
						fmt.Sprintf("the action writes %s, owned by module %q", w, owner[w]),
						"declare an action in "+owner[w]+" and call it with s.Calls instead"))
				}
			}
			for _, r := range act.Reads {
				if a.FindResource(r) == nil {
					d = append(d, bad("AM-SPEC-002", decl,
						fmt.Sprintf("the action reads %s, which is not declared", r),
						"fix the resource reference"))
					continue
				}
				// A cross-module read needs a rule that lets this actor see it; the
				// check that matters is that reading your own resource needs no entry,
				// so an unnecessary one is only a warning.
				if owner[r] == m.Name && len(act.Writes) > 0 && contains(act.Writes, r) && redundantRead(act) {
					d = append(d, soft("AM-SPEC-006", decl,
						fmt.Sprintf("%s is in Reads and Writes of the same module", r),
						"drop it from s.Reads; a module may always read what it owns"))
				}
			}
			for _, ref := range fieldResourceRefs(a, act) {
				if a.FindResource(ref) == nil {
					d = append(d, bad("AM-SPEC-002", decl,
						fmt.Sprintf("the output or input references %s, which is not declared", ref),
						"fix the resource reference"))
				}
			}
			for _, e := range act.Effects {
				if e.Type == model.EffectEnqueue {
					if m2, j := a.FindAction(e.Job); m2 == nil {
						d = append(d, bad("AM-SPEC-002", decl,
							fmt.Sprintf("the action enqueues %s, which is not a declared job", e.Job),
							"declare the job or fix the reference"))
					} else if j.Kind != model.KindJob {
						d = append(d, bad("AM-SPEC-005", decl,
							fmt.Sprintf("s.Enqueue target %s is a %s, not a job", e.Job, j.Kind),
							"Enqueue takes a s.Job"))
					}
				}
				if e.Compensates != "" {
					if e.Mode != "immediate" {
						d = append(d, bad("AM-SPEC-005", decl, "only an immediate effect can need compensating; a deferred one never runs before commit",
							"drop Compensate, or make the effect Immediate()"))
					} else if m2, j := a.FindAction(e.Compensates); m2 == nil || j.Kind != model.KindJob {
						d = append(d, bad("AM-SPEC-002", decl,
							fmt.Sprintf("the compensation %s is not a declared job", e.Compensates),
							"name a s.Job as module.Job; it receives the effect call and its result"))
					}
				}
			}
		}
	}
	return d
}

func redundantRead(act *model.Action) bool { return len(act.Reads) == 1 }

func contains(in []string, v string) bool {
	for _, x := range in {
		if x == v {
			return true
		}
	}
	return false
}

// fieldResourceRefs walks the resources named by an action's output.
func fieldResourceRefs(a *model.App, act *model.Action) []string {
	var out []string
	if act.Output.Resource != "" {
		out = append(out, act.Output.Resource)
	}
	if act.Output.Page != "" {
		out = append(out, act.Output.Page)
	}
	for _, f := range act.Input {
		if f.Ref != nil {
			out = append(out, f.Ref.Resource)
		}
	}
	return out
}

func checkActions(a *model.App) Diags {
	var d Diags
	framework := map[string]bool{}
	for _, c := range []string{"not_found", "invalid", "forbidden", "conflict", "internal", "unauthenticated", "rate_limited", "timeout", "invariant_violation", "not_implemented", "query_budget_exceeded", "unavailable"} {
		framework[c] = true
	}
	for _, m := range a.Modules {
		seen := map[string]bool{}
		for i := range m.Actions {
			act := &m.Actions[i]
			decl := model.Qual(m.Name, act.Name)
			if seen[act.Name] {
				d = append(d, bad("AM-SPEC-002", decl, "two actions share a name", "rename one"))
			}
			seen[act.Name] = true
			owned := map[string]bool{}
			for _, r := range m.Resources {
				owned[model.Qual(m.Name, r.Name)] = true
			}

			// AM-SPEC-004: no action runs without a decision about who may call it.
			if (act.Kind == model.KindQuery || act.Kind == model.KindMutation) && !act.Public && act.Requires == nil {
				d = append(d, bad("AM-SPEC-004", decl,
					"a "+string(act.Kind)+" declares neither Requires nor Public",
					"add s.Requires(...) naming a rule, or s.Public() to allow anonymous callers"))
			}
			// AM-SEC-005: public mutations are the enumeration surface.
			if act.Public && act.Kind == model.KindMutation && len(act.Limits.RateLimit) == 0 {
				d = append(d, bad("AM-SEC-005", decl,
					"a public mutation has no rate limit",
					"add s.RateLimit(s.PerActor(n, s.Minute), s.PerIP(n, s.Minute))"))
			}
			if act.Public && act.Kind == model.KindQuery && len(act.Limits.RateLimit) == 0 {
				d = append(d, soft("AM-SEC-005", decl, "a public query has no rate limit",
					"add s.RateLimit(s.PerIP(n, s.Minute))"))
			}
			if act.Requires != nil && act.Public {
				d = append(d, bad("AM-SPEC-004", decl, "the action declares both Requires and Public",
					"keep one; Public is the weaker statement"))
			}
			if act.Kind == model.KindQuery {
				if len(act.Writes) > 0 {
					d = append(d, bad("AM-SPEC-005", decl, "a query declares Writes",
						"make it a s.Mutation, or drop s.Writes"))
				}
				if len(act.Effects) > 0 {
					d = append(d, bad("AM-SPEC-005", decl, "a query declares Effects",
						"make it a s.Mutation; queries must not change the world"))
				}
				if act.Idempotent {
					d = append(d, bad("AM-SPEC-005", decl, "a query declares Idempotent",
						"idempotency keys only apply to mutations"))
				}
			}
			if act.Kind == model.KindMutation {
				if len(act.Writes) == 0 && len(act.Calls) == 0 {
					d = append(d, soft("AM-SPEC-005", decl, "a mutation writes nothing",
						"declare it as a s.Query, or add s.Writes"))
				}
			}
			if act.Kind == model.KindJob {
				if !act.Public && act.Requires != nil {
					d = append(d, soft("AM-SPEC-004", decl, "a job declares Requires",
						"jobs run as RunAs; the enqueueing action carries the rule"))
				}
				if act.Output.Resource != "" || act.Output.Page != "" || len(act.Output.Fields) > 0 {
					d = append(d, bad("AM-SPEC-005", decl, "a job declares Returns",
						"jobs have no caller to return to; record results on resources"))
				}
				if len(act.Writes) == 0 && len(act.Effects) == 0 && len(act.Calls) == 0 {
					d = append(d, soft("AM-SPEC-005", decl, "a job writes and does nothing",
						"a job with no effect is dead code"))
				}
			}
			if act.Kind == model.KindSchedule {
				if act.Cron == "" {
					d = append(d, bad("AM-SPEC-005", decl, "a schedule has no cron expression",
						"add s.Cron(\"0 9 * * *\")"))
				}
				if act.Runs == "" {
					d = append(d, bad("AM-SPEC-005", decl, "a schedule runs nothing",
						"add s.Runs(JobName)"))
				} else {
					if _, j := a.FindAction(act.Runs); j == nil {
						d = append(d, bad("AM-SPEC-002", decl,
							fmt.Sprintf("the schedule runs %s, which is not declared", act.Runs),
							"fix the job reference"))
					} else if j.Kind != model.KindJob {
						d = append(d, bad("AM-SPEC-005", decl,
							fmt.Sprintf("s.Runs target %s is a %s, not a job", act.Runs, j.Kind),
							"schedules may only run jobs"))
					} else if runMod, _ := a.FindAction(act.Runs); runMod != nil && runMod.Name != m.Name {
						d = append(d, bad("AM-SPEC-005", decl,
							fmt.Sprintf("the schedule runs a job in another module (%s)", act.Runs),
							"declare the schedule next to the job it runs"))
					}
				}
			}
			if (act.Kind == model.KindJob || act.Kind == model.KindSchedule) && len(act.Limits.RateLimit) > 0 {
				d = append(d, soft("AM-SEC-005", decl, "rate limits on a background action do nothing",
					"limits are request-shaped; use s.Concurrency instead"))
			}
			if act.CrossTenant && act.Kind == model.KindQuery && act.Cache != nil {
				d = append(d, soft("AM-SEC-007", decl,
					"a cross-tenant query is cached; the cache key includes the tenant so nothing leaks, but the entry churns",
					"drop s.Cache or drop s.CrossTenant"))
			}
			// Input and output shape.
			seenField := map[string]bool{}
			for _, f := range act.Input {
				if seenField[f.Name] {
					d = append(d, bad("AM-SPEC-002", decl, "input declares "+f.Name+" twice", "keep one"))
				}
				seenField[f.Name] = true
				if isRuntimeOwnedField(f.Name) {
					d = append(d, bad("AM-SEC-008", decl,
						"input declares "+f.Name+", which the runtime sets from the actor",
						"mass assignment is blocked by design: remove the field and let the framework fill it"))
				}
				if err := fieldRules(f); err != "" {
					d = append(d, bad("AM-SPEC-002", decl, "input "+f.Name+": "+err, "fix the field declaration"))
				}
			}
			for _, f := range act.Output.Fields {
				if err := fieldRules(f); err != "" {
					d = append(d, bad("AM-SPEC-002", decl, "output "+f.Name+": "+err, "fix the field declaration"))
				}
			}
			if act.Output.Resource != "" && act.Output.Page != "" {
				d = append(d, bad("AM-SPEC-005", decl, "the action returns both a resource and a page",
					"choose one shape"))
			}
			for _, ref := range []string{act.Output.Resource, act.Output.Page} {
				if ref == "" {
					continue
				}
				if a.FindResource(ref) == nil {
					d = append(d, bad("AM-SPEC-002", decl,
						fmt.Sprintf("the action returns %s, which is not declared", ref),
						"fix the resource reference"))
				}
			}
			seenCode := map[string]bool{}
			for _, e := range act.Errors {
				if framework[e.Code] {
					d = append(d, bad("AM-SPEC-002", decl,
						fmt.Sprintf("error code %q is reserved by the framework", e.Code),
						"choose a domain-specific code"))
				}
				if seenCode[e.Code] {
					d = append(d, bad("AM-SPEC-002", decl, "error code "+e.Code+" declared twice", "keep one"))
				}
				seenCode[e.Code] = true
			}
			// Calls must exist and must not be local.
			for _, c := range act.Calls {
				cm, ca := a.FindAction(c)
				if cm == nil {
					d = append(d, bad("AM-SPEC-002", decl,
						fmt.Sprintf("the action calls %s, which is not declared", c),
						"fix the reference; s.Calls takes another module's action"))
					continue
				}
				if cm.Name == m.Name {
					d = append(d, soft("AM-CODE-002", decl,
						fmt.Sprintf("s.Calls(%s) points inside its own module", c),
						"same-module actions are ordinary function calls"))
				}
				if act.Kind == model.KindQuery && (ca.Kind == model.KindMutation || ca.Kind == model.KindJob) {
					d = append(d, bad("AM-SPEC-005", decl,
						fmt.Sprintf("a query calls %s, a %s", c, ca.Kind),
						"queries must not write; make the caller a mutation"))
				}
			}
			if act.Cache != nil {
				for _, r := range act.Cache.InvalidatedBy {
					if a.FindResource(r) == nil {
						d = append(d, bad("AM-SPEC-002", decl,
							fmt.Sprintf("the cache is invalidated by %s, which is not declared", r),
							"fix the resource reference"))
					}
				}
				if act.Kind != model.KindQuery {
					d = append(d, bad("AM-SPEC-005", decl, "only queries may be cached", "drop s.Cache"))
				}
			}
			for _, e := range act.Effects {
				if e.Type == model.EffectHTTP && len(e.Hosts) == 0 {
					d = append(d, bad("AM-SEC-006", decl, "an HTTP effect has no host allowlist",
						"add s.HTTP(s.Hosts(\"api.example.com\")) so SSRF is refused at runtime"))
				}
				if e.Mode == "immediate" && !act.Idempotent && e.Type != model.EffectEnqueue {
					d = append(d, soft("AM-SEC-009", decl,
						"an immediate "+string(e.Type)+" effect has no idempotency key",
						"immediate effects must be replay-safe (§10.2); make the mutation Idempotent"))
				}
			}
		}
	}
	return d
}

func isRuntimeOwnedField(name string) bool {
	switch name {
	case "tenant_id", "version", "created_at", "updated_at", "actor_id", "id":
		return true
	}
	return false
}

func fieldRules(f model.Field) string {
	if f.Type.Kind == model.KindEnum && len(f.Type.Values) == 0 {
		return "an enum needs values"
	}
	if f.Default != nil && f.Default.Kind == "string" && len(f.Type.Values) > 0 {
		ok := false
		for _, v := range f.Type.Values {
			if v == f.Default.Text {
				ok = true
			}
		}
		if !ok {
			return fmt.Sprintf("default %q is not one of %v", f.Default.Text, f.Type.Values)
		}
	}
	if f.Type.Pattern != "" {
		if _, err := regexp.Compile(f.Type.Pattern); err != nil {
			return fmt.Sprintf("invalid Pattern: %v", err)
		}
	}
	if f.Type.Min != "" && f.Type.Max != "" {
		lo, lerr := toFloat(f.Type.Min)
		hi, herr := toFloat(f.Type.Max)
		if lerr != nil || herr != nil {
			return "Min and Max must be numbers"
		}
		if lo > hi {
			return fmt.Sprintf("Min %v is above Max %v", lo, hi)
		}
	}
	if f.Type.MaxLen != nil && *f.Type.MaxLen <= 0 {
		return "MaxLen must be positive"
	}
	return ""
}

func toFloat(s string) (float64, error) {
	var f float64
	_, err := fmt.Sscanf(s, "%g", &f)
	return f, err
}

// checkCycles walks the call graph; §4.1 requires it to be acyclic and reports the
// full path when it is not.
func checkCycles(a *model.App) Diags {
	var d Diags
	const (
		white = 0
		gray  = 1
		black = 2
	)
	state := map[string]int{}
	var stack []string
	var walk func(string)
	walk = func(ref string) {
		switch state[ref] {
		case gray:
			path := append([]string(nil), stack...)
			start := 0
			for i, s := range path {
				if s == ref {
					start = i
					break
				}
			}
			d = append(d, bad("AM-SPEC-002", ref,
				"cyclic module calls: "+strings.Join(append(path[start:], ref), " -> "),
				"extract the shared work into one action and call it from both"))
			return
		case black:
			return
		}
		state[ref] = gray
		stack = append(stack, ref)
		if _, act := a.FindAction(ref); act != nil {
			for _, c := range act.Calls {
				walk(c)
			}
		}
		stack = stack[:len(stack)-1]
		state[ref] = black
	}
	for _, m := range a.Modules {
		for _, act := range m.Actions {
			walk(model.Qual(m.Name, act.Name))
		}
	}
	return d
}

// checkSQL compiles every rule into SQL, which is how the build proves that a rule
// can actually be enforced by Postgres (§5.4: expressions that cannot compile are
// rejected here, not discovered at query time).
func checkSQL(a *model.App) Diags {
	var d Diags
	tables := a.PolicyTables()
	for _, m := range a.Modules {
		for _, r := range m.Resources {
			decl := model.Qual(m.Name, r.Name)
			t := tables[decl]
			if t == nil {
				d = append(d, bad("AM-SPEC-002", decl, "no table was built for the resource", "rerun am gen"))
				continue
			}
			for kind, node := range map[string]*policy.Node{
				"Read": r.Access.Read, "Create": r.Access.Create, "Update": r.Access.Update,
				"UpdateAfter": r.Access.UpdateAfter, "Delete": r.Access.Delete,
			} {
				if node == nil {
					continue
				}
				if _, err := policy.TableSQL(node, t, policy.TargetQuery, "am_t"); err != nil {
					d = append(d, bad("AM-SPEC-003", decl,
						fmt.Sprintf("the %s rule cannot be compiled to SQL: %v", kind, err),
						"simplify the rule: only field paths up to one relation deep, roles, actor ids and literals are supported"))
				}
			}
			for _, fr := range append(append([]model.FieldRule{}, r.Access.FieldRead...), r.Access.FieldWrite...) {
				if _, err := policy.TableSQL(fr.Expr, t, policy.TargetQuery, "am_t"); err != nil {
					d = append(d, bad("AM-SPEC-003", decl+"."+fr.Field,
						"the field rule cannot be compiled to SQL: "+err.Error(),
						"simplify the rule"))
				}
			}
			for _, ix := range r.Indexes {
				if ix.Where == nil {
					continue
				}
				if _, err := policy.TableSQL(ix.Where, t, policy.TargetDDL, r.Table); err != nil {
					d = append(d, bad("AM-SPEC-003", decl,
						"the partial index condition cannot be compiled: "+err.Error(),
						"a partial index may only use row-local fields"))
				}
			}
			for _, inv := range r.Invariants {
				target := policy.TargetDDL
				if inv.Enforcement == "deferred" {
					target = policy.TargetQuery
				}
				if _, err := policy.TableSQL(inv.Expr, t, target, r.Table); err != nil {
					d = append(d, bad("AM-SPEC-003", decl+"."+inv.Name,
						"the invariant cannot be compiled to SQL: "+err.Error(),
						"express it over row fields, or over one relation"))
					continue
				}
				if inv.Enforcement == "check" {
					sql, err := policy.TableSQL(inv.Expr, t, policy.TargetDDL, r.Table)
					if err == nil && strings.Contains(sql.Text, "SELECT") {
						d = append(d, bad("AM-SPEC-003", decl+"."+inv.Name,
							"the invariant needs a subquery, so it cannot be a CHECK constraint",
							"traverse a Ref field so it runs as a deferred check instead"))
					}
				}
			}
		}
		for _, act := range m.Actions {
			decl := model.Qual(m.Name, act.Name)
			if act.Requires != nil {
				// Requires is evaluated against the actor alone; it may not read rows,
				// otherwise an authorization check becomes a query.
				if nodeTouchesRows(act.Requires) {
					d = append(d, bad("AM-SPEC-003", decl,
						"Requires may only reference roles, the actor and literals",
						"move row conditions into the resource's Access rules"))
				}
			}
		}
	}
	return d
}

func nodeTouchesRows(n *policy.Node) bool {
	found := false
	n.Walk(func(x *policy.Node) bool {
		if x.Op == policy.OpField || x.Op == policy.OpOwner || x.Op == policy.OpNow {
			found = true
		}
		return true
	})
	return found
}

// checkFields reports storage-level problems the generators would hit later.
func checkFields(a *model.App) Diags {
	var d Diags
	for _, r := range a.AllResources() {
		decl := r.Ref
		for _, f := range r.Resource.Fields {
			if msg := fieldRules(f); msg != "" {
				d = append(d, bad("AM-SPEC-002", decl+"."+f.Name, msg, "fix the field declaration"))
			}
			if f.Type.Kind == model.KindText && f.Type.MaxLen == nil {
				d = append(d, soft("AM-DATA-004", decl+"."+f.Name,
					"a text field with no MaxLen can carry an unbounded value into every response",
					"add .MaxLen(n), or make it Bytes and store it in the Storage effect"))
			}
			if f.Sensitive && r.Resource.Access.FieldReadFor(f.Name) == nil {
				d = append(d, soft("AM-SEC-007", decl+"."+f.Name,
					"a Sensitive field has no FieldRead rule, so any actor that can read the row sees it",
					"add s.FieldRead(\""+f.Name+"\", ...) to narrow who may see the value"))
			}
			if f.Ref != nil {
				tgt := a.FindResource(f.Ref.Resource)
				if tgt == nil {
					d = append(d, bad("AM-SPEC-002", decl+"."+f.Name,
						"the relation points at "+f.Ref.Resource+", which is not declared",
						"fix the resource reference"))
					continue
				}
				if tgt.Tenant && !r.Resource.Tenant {
					d = append(d, bad("AM-SPEC-005", decl+"."+f.Name,
						"a non-tenant resource points at the tenant-scoped "+f.Ref.Resource,
						"add s.Tenant() to "+decl+" so the two cannot straddle a tenant boundary"))
				}
			}
			if f.Immutable && f.Default == nil && !f.Optional && !isRuntimeOwnedField(f.Name) {
				d = append(d, soft("AM-MIG-003", decl+"."+f.Name,
					"an immutable required field with no default can never gain a new row",
					"add .Default(v) or make it .Optional()"))
			}
		}
		for _, ix := range r.Resource.Indexes {
			for _, f := range ix.Fields {
				if r.Resource.FindField(f) == nil {
					d = append(d, bad("AM-SPEC-002", decl, "index "+ix.Name+" names unknown field "+f,
						"fix the field name"))
				}
			}
		}
	}
	return d
}
