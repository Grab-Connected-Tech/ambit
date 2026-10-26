// Package diff computes the behavior diff: a risk-ranked summary of what the app
// can now do that it could not before (§15). It is static comparison only, never an
// LLM, because the review surface has to be reproducible.
package diff

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"keystone.dev/keystone/model"
	"keystone.dev/keystone/policy"
)

// Risk ranks a change.
type Risk int

const (
	Low Risk = iota
	Medium
	High
)

func (r Risk) String() string {
	switch r {
	case High:
		return "HIGH"
	case Medium:
		return "MED"
	}
	return "LOW"
}

// Label is the lowercase form used in JSON output.
func (r Risk) Label() string {
	switch r {
	case High:
		return "high"
	case Medium:
		return "medium"
	}
	return "low"
}

// Category names the kind of change, matching the columns of §15.1.
const (
	CatAccess = "access"
	CatAction = "action"
	CatEffect = "effect"
	CatData   = "data"
	CatCode   = "code"
	CatDep    = "dependency"
	CatAPI    = "api"
	CatLimit  = "limits"
	CatIdent  = "identity"
)

// Change is one line of the report.
type Change struct {
	Risk     Risk   `json:"risk"`
	Category string `json:"category"`
	Subject  string `json:"subject"`
	Detail   string `json:"detail"`
	Why      string `json:"why,omitempty"`
}

// Report is a whole diff.
type Report struct {
	Base    string   `json:"base"`
	Head    string   `json:"head"`
	Changes []Change `json:"changes"`
	Unsafe  int      `json:"unsafeBlocks"`
	NewDeps int      `json:"newDependencies"`
	Allow   int      `json:"suppressions"`
}

// Count tallies a risk level.
func (r Report) Count(x Risk) int {
	n := 0
	for _, c := range r.Changes {
		if c.Risk == x {
			n++
		}
	}
	return n
}

// Empty reports no changes at all.
func (r Report) Empty() bool { return len(r.Changes) == 0 && r.Unsafe == 0 && r.NewDeps == 0 }

// Between compares two models and their code facts.
func Between(base, head *model.App, bf, hf model.Facts) Report {
	if base == nil {
		// No base model means a first release: everything that writes is new.
		base = &model.App{}
	}
	if head == nil {
		head = &model.App{}
	}
	var out []Change
	out = append(out, accessChanges(base, head)...)
	out = append(out, actionChanges(base, head)...)
	out = append(out, effectChanges(base, head)...)
	out = append(out, dataChanges(base, head)...)
	out = append(out, apiChanges(base, head)...)
	out = append(out, limitChanges(base, head)...)
	out = append(out, identityChanges(base, head)...)
	out = append(out, codeChanges(bf, hf)...)
	out = append(out, depChanges(bf.Deps, hf.Deps)...)
	r := Report{
		Base:    short(base),
		Head:    short(head),
		Changes: order(out),
		Unsafe:  countNew(bf.Unsafe, hf.Unsafe, func(x, y model.UnsafeFact) bool { return x.Key() == y.Key() }),
		NewDeps: countNewDeps(bf.Deps, hf.Deps),
		Allow:   countNew(bf.Allows, hf.Allows, func(x, y model.AllowFact) bool { return x.Key() == y.Key() }),
	}
	if r.Unsafe > 0 {
		r.Changes = append([]Change{{Risk: High, Category: CatCode, Subject: "unsafe blocks",
			Detail: fmt.Sprintf("%d new raw-SQL or escape-hatch block(s)", r.Unsafe)}}, r.Changes...)
	}
	if r.Allow > 0 {
		r.Changes = append([]Change{{Risk: High, Category: CatCode, Subject: "suppressions",
			Detail: fmt.Sprintf("%d new //ks:allow suppression(s)", r.Allow)}}, r.Changes...)
	}
	sort.SliceStable(r.Changes, func(i, j int) bool {
		if r.Changes[i].Risk != r.Changes[j].Risk {
			return r.Changes[i].Risk > r.Changes[j].Risk
		}
		if r.Changes[i].Category != r.Changes[j].Category {
			return r.Changes[i].Category < r.Changes[j].Category
		}
		return r.Changes[i].Subject < r.Changes[j].Subject
	})
	return r
}

func short(a *model.App) string {
	if a == nil {
		return "none"
	}
	return a.Hash()[:12]
}

func dedupe(in []Change) []Change {
	seen := map[string]bool{}
	var out []Change
	for _, c := range in {
		k := c.Category + "|" + c.Subject + "|" + c.Detail
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, c)
	}
	return out
}

func order(in []Change) []Change {
	out := dedupe(in)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Risk != out[j].Risk {
			return out[i].Risk > out[j].Risk
		}
		if out[i].Category != out[j].Category {
			return out[i].Category < out[j].Category
		}
		return out[i].Subject < out[j].Subject
	})
	return out
}

// ---- access ----

func accessChanges(base, head *model.App) []Change {
	var out []Change
	cmp := func(subject, kind string, old, new *policy.Node, widenRisk Risk) {
		rel, note := policy.Compare(old, new)
		switch rel {
		case policy.Equal:
			return
		case policy.Narrowed:
			out = append(out, Change{Risk: Medium, Category: CatAccess, Subject: subject + "." + kind,
				Detail: "narrowed: " + note, Why: "an actor or row that could act before may not now"})
		case policy.Widened:
			out = append(out, Change{Risk: orHigh(widenRisk), Category: CatAccess, Subject: subject + "." + kind,
				Detail: "widened: " + note, Why: "the app can now do something it could not before"})
		default:
			// §15.2: an unproven change is reported as widened.
			out = append(out, Change{Risk: orHigh(widenRisk), Category: CatAccess, Subject: subject + "." + kind,
				Detail: "widened (comparison cannot prove it is narrower): " + note,
				Why:    "false alarms are acceptable, missed widenings are not"})
		}
	}
	for _, ref := range union(aRes(base), aRes(head)) {
		o, n := base.FindResource(ref), head.FindResource(ref)
		if o == nil {
			out = append(out, Change{Risk: Low, Category: CatAccess, Subject: ref, Detail: "resource added"})
			continue
		}
		if n == nil {
			continue // reported under data
		}
		for kind, pick := range map[string]func(m *model.Resource) *policy.Node{
			"read":        func(m *model.Resource) *policy.Node { return m.Access.Read },
			"create":      func(m *model.Resource) *policy.Node { return m.Access.Create },
			"update":      func(m *model.Resource) *policy.Node { return m.Access.Update },
			"updateAfter": func(m *model.Resource) *policy.Node { return m.Access.UpdateAfter },
			"delete":      func(m *model.Resource) *policy.Node { return m.Access.Delete },
		} {
			cmp(ref, kind, pick(o), pick(n), 0)
		}
		for _, f := range unionFieldRules(o.Access.FieldRead, n.Access.FieldRead, "read") {
			cmp(ref+"."+f, "read", fieldRuleOf(o.Access.FieldRead, f), fieldRuleOf(n.Access.FieldRead, f), Medium)
		}
		for _, f := range unionFieldRules(o.Access.FieldWrite, n.Access.FieldWrite, "write") {
			cmp(ref+"."+f, "write", fieldRuleOf(o.Access.FieldWrite, f), fieldRuleOf(n.Access.FieldWrite, f), Medium)
		}
	}
	for _, ref := range union(aActs(base), aActs(head)) {
		o, had := findAction(base, ref)
		n, has := findAction(head, ref)
		if !had || !has {
			continue
		}
		if !o.Public() && n.Public() {
			out = append(out, Change{Risk: High, Category: CatAccess, Subject: ref,
				Detail: "action became public: no authorization is required to call it",
				Why:    "the endpoint is reachable by anonymous traffic"})
		}
		if o.Public() && !n.Public() {
			out = append(out, Change{Risk: Medium, Category: CatAccess, Subject: ref, Detail: "action is no longer public"})
		}
		if !o.CrossTenant() && n.CrossTenant() {
			out = append(out, Change{Risk: High, Category: CatAccess, Subject: ref,
				Detail: "CrossTenant() added: reads may leave the actor's tenant",
				Why:    "tenant isolation is the guarantee the framework promises"})
		}
		cmp(ref, "requires", o.Requires(), n.Requires(), 0)
	}
	return out
}

// pair bundles an action with its module name. The methods keep the diff code
// reading like the spec table it implements.
type pair struct {
	module string
	action *model.Action
}

func (p pair) Public() bool           { return p.action.Public }
func (p pair) CrossTenant() bool      { return p.action.CrossTenant }
func (p pair) Requires() *policy.Node { return p.action.Requires }
func (p pair) Kind() model.ActionKind { return p.action.Kind }

func (p pair) effects() []model.Effect     { return p.action.Effects }
func (p pair) Writes() []string            { return p.action.Writes }
func (p pair) Reads() []string             { return p.action.Reads }
func (p pair) Effects() []model.Effect     { return p.action.Effects }
func (p pair) Input() []model.Field        { return p.action.Input }
func (p pair) Output() model.Output        { return p.action.Output }
func (p pair) Limits() model.ActionLimits  { return p.action.Limits }
func (p pair) Retry() *model.Retry         { return p.action.Retry }
func (p pair) Deprecated() string          { return p.action.Deprecated }
func (p pair) Errors() []model.ActionError { return p.action.Errors }
func (p pair) Calls() []string             { return p.action.Calls }

func aRes(a *model.App) []string {
	var out []string
	if a == nil {
		return out
	}
	for _, r := range a.AllResources() {
		out = append(out, r.Ref)
	}
	sort.Strings(out)
	return out
}

func aActs(a *model.App) []string {
	var out []string
	if a == nil {
		return out
	}
	for _, m := range a.Modules {
		for i := range m.Actions {
			out = append(out, model.Qual(m.Name, m.Actions[i].Name))
		}
	}
	sort.Strings(out)
	return out
}

// findAction returns a wrapped action, so callers can read fields uniformly.
func findAction(a *model.App, ref string) (pair, bool) {
	if a == nil {
		return pair{}, false
	}
	m, act := a.FindAction(ref)
	if act == nil {
		return pair{}, false
	}
	return pair{module: m.Name, action: act}, true
}

func union(a, b []string) []string {
	set := map[string]bool{}
	for _, x := range append(append([]string(nil), a...), b...) {
		set[x] = true
	}
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func unionFieldRules(a, b []model.FieldRule, _ string) []string {
	set := map[string]bool{}
	for _, x := range append(append([]model.FieldRule(nil), a...), b...) {
		set[x.Field] = true
	}
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func fieldRuleOf(rules []model.FieldRule, field string) *policy.Node {
	for _, r := range rules {
		if r.Field == field {
			return r.Expr
		}
	}
	return nil
}

// ---- actions ----

func actionChanges(base, head *model.App) []Change {
	var out []Change
	for _, ref := range union(aActs(base), aActs(head)) {
		o, had := findAction(base, ref)
		n, has := findAction(head, ref)
		switch {
		case has && !had:
			risk := Low
			detail := "new query action"
			if len(n.Writes()) > 0 || n.Kind() == model.KindJob || n.Kind() == model.KindMutation {
				risk = High
				detail = "new action that writes data"
			} else if n.Public() {
				risk = High
				detail = "new public action"
			}
			if n.Requires() != nil {
				detail += ", requires " + policy.Canonical(n.Requires())
			} else if n.Public() {
				detail += ", no authorization"
			}
			out = append(out, Change{Risk: risk, Category: CatAction, Subject: ref, Detail: detail})
		case had && !has:
			out = append(out, Change{Risk: High, Category: CatAction, Subject: ref,
				Detail: "action removed", Why: "callers of the old shape break"})
		default:
			if !sameFields(o.Input(), n.Input()) {
				out = append(out, Change{Risk: Medium, Category: CatAction, Subject: ref,
					Detail: "input changed: " + fieldDelta(o.Input(), n.Input())})
			}
			if !sameOutput(o.Output(), n.Output()) {
				out = append(out, Change{Risk: Medium, Category: CatAction, Subject: ref,
					Detail: "output changed: " + outputText(o.Output()) + " -> " + outputText(n.Output())})
			}
			if !sameStrings(o.Calls(), n.Calls()) {
				out = append(out, Change{Risk: Medium, Category: CatAction, Subject: ref,
					Detail: "calls changed: " + strings.Join(union(n.Calls(), o.Calls()), ","),
					Why:    "a call runs the callee's rules, so behavior moves with it"})
			}
			if o.Deprecated() != n.Deprecated() && n.Deprecated() != "" {
				out = append(out, Change{Risk: Medium, Category: CatAPI, Subject: ref,
					Detail: "marked deprecated: " + n.Deprecated()})
			}
			if !sameErrors(o.Errors(), n.Errors()) {
				out = append(out, Change{Risk: Medium, Category: CatAPI, Subject: ref,
					Detail: "declared errors changed: " + strings.Join(errorDelta(o.Errors(), n.Errors()), ",")})
			}
		}
	}
	if len(out) > 0 {
		// A rename shows up as a removal plus an addition; say so once.
		for i := range out {
			if out[i].Category == CatAction && strings.Contains(out[i].Detail, "removed") {
				out[i].Why = "renames read as a removal plus an addition"
			}
		}
	}
	return out
}

func sameErrors(a, b []model.ActionError) bool { return errorDelta(a, b) == nil }

func errorDelta(a, b []model.ActionError) []string {
	set := map[string]bool{}
	for _, x := range a {
		set[x.Code] = true
	}
	var out []string
	for _, x := range b {
		if !set[x.Code] {
			out = append(out, "+"+x.Code)
		}
	}
	for _, x := range a {
		found := false
		for _, y := range b {
			if y.Code == x.Code {
				found = true
			}
		}
		if !found {
			out = append(out, "-"+x.Code)
		}
	}
	sort.Strings(out)
	return out
}

// ---- effects ----

func effectChanges(base, head *model.App) []Change {
	var out []Change
	for _, ref := range union(aActs(base), aActs(head)) {
		o, had := findAction(base, ref)
		n, has := findAction(head, ref)
		if !had || !has {
			continue
		}
		om := effectMap(o.effects())
		nm := effectMap(n.effects())
		for _, k := range union(keysOf(om), keysOf(nm)) {
			oe, hasO := om[k]
			ne, hasN := nm[k]
			switch {
			case hasN && !hasO:
				if len(ne.Hosts) > 0 {
					// One row per host is what a reviewer acts on; the effect itself is
					// already visible in the action's own row.
					for _, h := range ne.Hosts {
						out = append(out, Change{Risk: High, Category: CatEffect, Subject: ref,
							Detail: "new HTTP host: " + h})
					}
				} else {
					out = append(out, Change{Risk: High, Category: CatEffect, Subject: ref,
						Detail: "new effect: " + describeEffect(ne)})
				}
			case hasO && !hasN:
				out = append(out, Change{Risk: Low, Category: CatEffect, Subject: ref,
					Detail: "effect removed: " + describeEffect(oe)})
			default:
				if oe.Mode != ne.Mode && ne.Mode == "immediate" {
					out = append(out, Change{Risk: High, Category: CatEffect, Subject: ref,
						Detail: "effect made immediate: " + string(ne.Type) + " now runs before commit",
						Why:    "a rolled-back transaction cannot undo an effect that already ran"})
				}
				for _, h := range missing(ne.Hosts, oe.Hosts) {
					out = append(out, Change{Risk: High, Category: CatEffect, Subject: ref,
						Detail: "new HTTP host: " + h})
				}
			}
		}
		if !sameRetry(o.Retry(), n.Retry()) {
			out = append(out, Change{Risk: Medium, Category: CatEffect, Subject: ref,
				Detail: "retry policy changed: " + retryText(o.Retry()) + " -> " + retryText(n.Retry())})
		}
	}
	return out
}

func effectMap(in []model.Effect) map[string]model.Effect {
	out := map[string]model.Effect{}
	for _, e := range in {
		out[string(e.Type)+":"+e.Topic+":"+e.Bucket+":"+e.Job] = e
	}
	return out
}

func keysOf(m map[string]model.Effect) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func describeEffect(e model.Effect) string {
	s := string(e.Type)
	if e.Mode == "immediate" {
		s += " (immediate)"
	} else {
		s += " (deferred)"
	}
	if len(e.Hosts) > 0 {
		s += " hosts=" + strings.Join(e.Hosts, ",")
	}
	if e.Topic != "" {
		s += " topic=" + e.Topic
	}
	if e.Bucket != "" {
		s += " bucket=" + e.Bucket
	}
	return s
}

func missing(a, b []string) []string {
	set := map[string]bool{}
	for _, x := range b {
		set[x] = true
	}
	var out []string
	for _, x := range a {
		if !set[x] {
			out = append(out, x)
		}
	}
	sort.Strings(out)
	return out
}

func sameRetry(a, b *model.Retry) bool {
	x, y := "", ""
	if a != nil {
		x = fmt.Sprint(*a)
	}
	if b != nil {
		y = fmt.Sprint(*b)
	}
	return x == y
}

func retryText(r *model.Retry) string {
	if r == nil {
		return "default"
	}
	return fmt.Sprintf("%d attempts, %s(%s..%s)", r.MaxAttempts, r.Backoff, r.Base, r.Cap)
}

// ---- data model ----

func dataChanges(base, head *model.App) []Change {
	var out []Change
	for _, ref := range union(aRes(base), aRes(head)) {
		o, n := base.FindResource(ref), head.FindResource(ref)
		if o == nil || n == nil {
			continue
		}
		for _, f := range union(fieldNames(o), fieldNames(n)) {
			of, nf := o.FindField(f), n.FindField(f)
			switch {
			case of == nil:
				risk, detail := Low, "field added"
				if !nf.Optional && nf.Default == nil {
					risk, detail = Medium, "needs backfill: field becomes required"
				}
				out = append(out, Change{Risk: risk, Category: CatData, Subject: ref + "." + f, Detail: detail})
				continue
			case nf == nil:
				out = append(out, Change{Risk: High, Category: CatData, Subject: ref + "." + f,
					Detail: "field removed", Why: "destructive: needs a two-phase plan"})
				continue
			case of.Type.Kind != nf.Type.Kind:
				out = append(out, Change{Risk: Medium, Category: CatData, Subject: ref + "." + f,
					Detail: fmt.Sprintf("type changed %s -> %s", of.Type.Kind, nf.Type.Kind), Why: "needs a plan"})
			case of.Optional && !nf.Optional:
				out = append(out, Change{Risk: Medium, Category: CatData, Subject: ref + "." + f,
					Detail: "field becomes required", Why: "needs a backfill"})
			case !of.Optional && nf.Optional:
				out = append(out, Change{Risk: Low, Category: CatData, Subject: ref + "." + f, Detail: "field becomes optional"})
			}
			if len(of.Type.Values) > 0 || len(nf.Type.Values) > 0 {
				for _, v := range missing(of.Type.Values, nf.Type.Values) {
					out = append(out, Change{Risk: Low, Category: CatData, Subject: ref + "." + f, Detail: "enum value added: " + v})
				}
				for _, v := range missing(nf.Type.Values, of.Type.Values) {
					out = append(out, Change{Risk: High, Category: CatData, Subject: ref + "." + f,
						Detail: "enum value removed: " + v, Why: "existing rows must be mapped"})
				}
			}
			if of.Sensitive != nf.Sensitive {
				out = append(out, Change{Risk: boolRisk(nf.Sensitive && !of.Sensitive), Category: CatData, Subject: ref + "." + f,
					Detail: fmt.Sprintf("Sensitive = %v", nf.Sensitive),
					Why:    whySensitive(nf.Sensitive)})
			}
			if of.Type.Encrypted != nf.Type.Encrypted {
				out = append(out, Change{Risk: High, Category: CatData, Subject: ref + "." + f,
					Detail: fmt.Sprintf("encrypted = %v", nf.Type.Encrypted)})
			}
			if of.Immutable != nf.Immutable {
				out = append(out, Change{Risk: Medium, Category: CatData, Subject: ref + "." + f,
					Detail: fmt.Sprintf("Immutable = %v", nf.Immutable)})
			}
		}
		for _, v := range unionInvariants(o, n) {
			ov, nv := invariantOf(o, v), invariantOf(n, v)
			switch {
			case ov == nil:
				out = append(out, Change{Risk: Low, Category: CatData, Subject: ref + "." + v, Detail: "invariant added"})
			case nv == nil:
				out = append(out, Change{Risk: Medium, Category: CatData, Subject: ref + "." + v,
					Detail: "invariant removed", Why: "a guarantee the framework no longer enforces"})
			case policy.Canonical(ov) != policy.Canonical(nv):
				rel, note := policy.Compare(ov, nv)
				out = append(out, Change{Risk: Medium, Category: CatData, Subject: ref + "." + v,
					Detail: "invariant changed (" + rel.String() + "): " + note})
			}
		}
	}
	return out
}

func whySensitive(on bool) string {
	if on {
		return "values stop appearing in logs and audit diffs"
	}
	return "values become visible in logs and audit diffs"
}

func fieldNames(r *model.Resource) []string {
	var out []string
	for _, f := range r.Fields {
		out = append(out, f.Name)
	}
	sort.Strings(out)
	return out
}

func unionInvariants(o, n *model.Resource) []string {
	set := map[string]bool{}
	for _, v := range o.Invariants {
		set[v.Name] = true
	}
	for _, v := range n.Invariants {
		set[v.Name] = true
	}
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func invariantOf(r *model.Resource, name string) *policy.Node {
	for _, v := range r.Invariants {
		if v.Name == name {
			return v.Expr
		}
	}
	return nil
}

// ---- api ----

func apiChanges(base, head *model.App) []Change {
	var out []Change
	compat := head.Defaults.Compatibility
	for _, ref := range union(aActs(base), aActs(head)) {
		o, had := findAction(base, ref)
		n, has := findAction(head, ref)
		switch {
		case has && !had:
			if compat != "none" {
				out = append(out, Change{Risk: Low, Category: CatAPI, Subject: ref, Detail: "compatible addition: new action"})
			}
		case had && !has:
			out = append(out, Change{Risk: High, Category: CatAPI, Subject: ref, Detail: "breaking: action removed or renamed"})
		default:
			breaking := false
			for _, f := range addedRequired(o.Input(), n.Input()) {
				breaking = true
				out = append(out, Change{Risk: High, Category: CatAPI, Subject: ref,
					Detail: "breaking: new required input " + f})
			}
			for _, f := range removedOutputFields(o.Output(), n.Output()) {
				breaking = true
				out = append(out, Change{Risk: High, Category: CatAPI, Subject: ref,
					Detail: "breaking: removed output field " + f})
			}
			for _, f := range narrowedEnums(o.Input(), n.Input()) {
				breaking = true
				out = append(out, Change{Risk: High, Category: CatAPI, Subject: ref,
					Detail: "breaking: narrowed enum " + f})
			}
			if n.Deprecated() != "" && o.Deprecated() == "" {
				out = append(out, Change{Risk: Medium, Category: CatAPI, Subject: ref,
					Detail: "deprecated: " + n.Deprecated()})
			}
			for _, f := range addedOptional(o.Input(), n.Input()) {
				out = append(out, Change{Risk: Low, Category: CatAPI, Subject: ref,
					Detail: "compatible addition: new optional input " + f})
			}
			if !breaking {
				for _, f := range addedOutput(o.Output(), n.Output()) {
					out = append(out, Change{Risk: Low, Category: CatAPI, Subject: ref,
						Detail: "compatible addition: new output field " + f})
				}
			}
		}
	}
	return out
}

func sameFields(a, b []model.Field) bool { return fieldDelta(a, b) == "" }

func fieldDelta(a, b []model.Field) string {
	var out []string
	am := map[string]model.Field{}
	bm := map[string]model.Field{}
	for _, f := range a {
		am[f.Name] = f
	}
	for _, f := range b {
		bm[f.Name] = f
	}
	for _, n := range union(keysOfFields(am), keysOfFields(bm)) {
		of, had := am[n]
		nf, has := bm[n]
		switch {
		case has && !had:
			if nf.Optional {
				out = append(out, "+"+n+"?")
			} else {
				out = append(out, "+"+n)
			}
		case had && !has:
			out = append(out, "-"+n)
		case of.SQLType() != nf.SQLType() || of.Optional != nf.Optional:
			out = append(out, "~"+n)
		}
	}
	sort.Strings(out)
	return strings.Join(out, " ")
}

func keysOfFields(m map[string]model.Field) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func addedRequired(a, b []model.Field) []string {
	am := map[string]model.Field{}
	for _, f := range a {
		am[f.Name] = f
	}
	var out []string
	for _, f := range b {
		prev, had := am[f.Name]
		if !had && !f.Optional {
			out = append(out, f.Name)
		} else if had && prev.Optional && !f.Optional {
			out = append(out, f.Name)
		}
	}
	sort.Strings(out)
	return out
}

func addedOptional(a, b []model.Field) []string {
	am := map[string]model.Field{}
	for _, f := range a {
		am[f.Name] = f
	}
	var out []string
	for _, f := range b {
		if _, had := am[f.Name]; !had && f.Optional {
			out = append(out, f.Name)
		}
	}
	sort.Strings(out)
	return out
}

func narrowedEnums(a, b []model.Field) []string {
	am := map[string]model.Field{}
	for _, f := range a {
		am[f.Name] = f
	}
	var out []string
	for _, f := range b {
		prev, had := am[f.Name]
		if !had || len(prev.Type.Values) == 0 {
			continue
		}
		if len(missing(prev.Type.Values, f.Type.Values)) > 0 {
			out = append(out, f.Name)
		}
	}
	sort.Strings(out)
	return out
}

func outputRef(o model.Output) string {
	if o.Page != "" {
		return o.Page
	}
	return o.Resource
}

func sameOutput(a, b model.Output) bool {
	x, y := outputText(a), outputText(b)
	return x == y
}

func outputText(o model.Output) string {
	if o.Page != "" {
		return "page:" + o.Page
	}
	if o.Resource != "" {
		return "resource:" + o.Resource
	}
	if o.None {
		return "none"
	}
	var names []string
	for _, f := range o.Fields {
		names = append(names, f.Name+":"+f.SQLType())
	}
	sort.Strings(names)
	return "fields:" + strings.Join(names, ",")
}

func removedOutputFields(a, b model.Output) []string {
	// A changed output resource is compared through the resource's own fields; here
	// only explicit field lists can lose a name.
	am := map[string]bool{}
	for _, f := range a.Fields {
		am[f.Name] = true
	}
	var out []string
	for name := range am {
		found := false
		for _, f := range b.Fields {
			if f.Name == name {
				found = true
			}
		}
		if !found {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	if outputRef(a) != outputRef(b) && outputRef(b) == "" {
		out = append(out, outputRef(a))
	}
	return out
}

func addedOutput(a, b model.Output) []string {
	am := map[string]bool{}
	for _, f := range a.Fields {
		am[f.Name] = true
	}
	var out []string
	for _, f := range b.Fields {
		if !am[f.Name] {
			out = append(out, f.Name)
		}
	}
	sort.Strings(out)
	if outputRef(b) != "" && outputRef(a) != outputRef(b) {
		out = append(out, outputRef(b))
	}
	return out
}

// ---- limits ----

func limitChanges(base, head *model.App) []Change {
	var out []Change
	for _, ref := range union(aActs(base), aActs(head)) {
		o, had := findAction(base, ref)
		n, has := findAction(head, ref)
		if !had || !has {
			continue
		}
		ol, nl := limitMap(o.Limits()), limitMap(n.Limits())
		for _, k := range union(keysOfLimits(ol), keysOfLimits(nl)) {
			ov, hasO := ol[k]
			nv, hasN := nl[k]
			switch {
			case hasO && !hasN:
				out = append(out, Change{Risk: High, Category: CatLimit, Subject: ref,
					Detail: "rate limit removed: " + k})
			case !hasO && hasN:
				out = append(out, Change{Risk: Low, Category: CatLimit, Subject: ref,
					Detail: "rate limit added: " + k})
			default:
				switch {
				case nv > ov*10:
					out = append(out, Change{Risk: High, Category: CatLimit, Subject: ref,
						Detail: fmt.Sprintf("rate limit %s raised %d -> %d", k, ov, nv)})
				case nv > ov:
					out = append(out, Change{Risk: Low, Category: CatLimit, Subject: ref,
						Detail: fmt.Sprintf("rate limit %s raised %d -> %d", k, ov, nv)})
				case nv < ov:
					out = append(out, Change{Risk: Low, Category: CatLimit, Subject: ref,
						Detail: fmt.Sprintf("rate limit %s tightened %d -> %d", k, ov, nv)})
				}
			}
		}
		out = append(out, budgetChange(ref, "query budget", o.Limits().QueryBudget, n.Limits().QueryBudget, head.Defaults.QueryBudget)...)
		out = append(out, budgetChange(ref, "row budget", o.Limits().RowBudget, n.Limits().RowBudget, head.Defaults.RowBudget)...)
		ot, nt := durText(o.Limits().Timeout), durText(n.Limits().Timeout)
		if ot != nt {
			risk := Low
			if nt > ot {
				risk = Medium
			}
			out = append(out, Change{Risk: risk, Category: CatLimit, Subject: ref,
				Detail: fmt.Sprintf("timeout %s -> %s", o.Limits().Timeout, n.Limits().Timeout)})
		}
		if o.Idempotent() != n.Idempotent() {
			out = append(out, Change{Risk: boolRisk(n.Idempotent()), Category: CatLimit, Subject: ref,
				Detail: fmt.Sprintf("idempotency key required = %v", n.Idempotent())})
		}
		if !sameStrings(o.Reads(), n.Reads()) {
			out = append(out, Change{Risk: Medium, Category: CatAccess, Subject: ref,
				Detail: "declared reads changed: " + strings.Join(union(n.Reads(), o.Reads()), ","),
				Why:    "an action can now query a resource it could not before"})
		}
	}
	return out
}

// pair accessors used above.
func (p pair) Idempotent() bool { return p.action.Idempotent }

func boolRisk(v bool) Risk {
	if v {
		return Low
	}
	return High
}

// durText normalizes a duration string for comparison, defaulting to the
// framework's 10s action timeout.
func durText(s string) string {
	if s == "" {
		return "10s"
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return s
	}
	return d.String()
}

func sameStrings(a, b []string) bool {
	return strings.Join(a, "|") == strings.Join(b, "|")
}

func limitMap(l model.ActionLimits) map[string]int {
	out := map[string]int{}
	for _, r := range l.RateLimit {
		out[r.Scope] = r.Limit
	}
	return out
}

func keysOfLimits(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func budgetChange(ref, what string, oldv, newv, def int) []Change {
	if oldv == 0 {
		oldv = def
	}
	if newv == 0 {
		newv = def
	}
	if oldv == newv {
		return nil
	}
	risk := Low
	switch {
	case newv > oldv && oldv > 0 && newv > oldv*2:
		risk = High
	case newv > oldv:
		risk = Medium
	}
	return []Change{{Risk: risk, Category: CatLimit, Subject: ref,
		Detail: fmt.Sprintf("%s %d -> %d", what, oldv, newv)}}
}

// ---- identity ----

func identityChanges(base, head *model.App) []Change {
	var out []Change
	b, h := base.Identity, head.Identity
	if sessionsEqual(b.Sessions, h.Sessions) == "" {
	} else {
		out = append(out, Change{Risk: High, Category: CatIdent, Subject: "identity.sessions",
			Detail: sessionsEqual(b.Sessions, h.Sessions)})
	}
	if changed := oidcDelta(b.OIDC, h.OIDC); changed != "" {
		out = append(out, Change{Risk: High, Category: CatIdent, Subject: "identity.oidc", Detail: changed})
	}
	if (b.APITokens == nil) != (h.APITokens == nil) || (b.APITokens != nil && h.APITokens != nil && b.APITokens.Scoped != h.APITokens.Scoped) {
		out = append(out, Change{Risk: High, Category: CatIdent, Subject: "identity.apiTokens",
			Detail: "API token surface changed"})
	}
	if (b.Passwords == nil) != (h.Passwords == nil) {
		out = append(out, Change{Risk: High, Category: CatIdent, Subject: "identity.passwords",
			Detail: fmt.Sprintf("password module enabled = %v", h.Passwords != nil)})
	} else if b.Passwords != nil && h.Passwords != nil && b.Passwords.BreachChecks != h.Passwords.BreachChecks {
		out = append(out, Change{Risk: Medium, Category: CatIdent, Subject: "identity.passwords",
			Detail: fmt.Sprintf("breached-password checks = %v", h.Passwords.BreachChecks)})
	}
	if !sameStrings(base.Roles, head.Roles) {
		for _, r := range missing(head.Roles, base.Roles) {
			out = append(out, Change{Risk: Medium, Category: CatIdent, Subject: "role " + r, Detail: "role added"})
		}
		for _, r := range missing(base.Roles, head.Roles) {
			out = append(out, Change{Risk: High, Category: CatIdent, Subject: "role " + r,
				Detail: "role removed", Why: "every rule naming it changes meaning"})
		}
	}
	return out
}

func sessionsEqual(a, b *model.Sessions) string {
	x, y := "-", "-"
	if a != nil {
		x = a.TTL + "/" + a.IdleTimeout + fmt.Sprintf("/rotate=%v", a.Rotate)
	}
	if b != nil {
		y = b.TTL + "/" + b.IdleTimeout + fmt.Sprintf("/rotate=%v", b.Rotate)
	}
	if x == y {
		return ""
	}
	return "session settings changed: " + x + " -> " + y
}

func oidcDelta(a, b []model.OIDC) string {
	am := map[string]bool{}
	for _, x := range a {
		am[x.Provider] = true
	}
	bm := map[string]bool{}
	for _, x := range b {
		bm[x.Provider] = true
	}
	var out []string
	for k := range bm {
		if !am[k] {
			out = append(out, "provider added: "+k)
		}
	}
	for k := range am {
		if !bm[k] {
			out = append(out, "provider removed: "+k)
		}
	}
	sort.Strings(out)
	return strings.Join(out, ", ")
}

// ---- code and dependencies ----

func codeChanges(bf, hf model.Facts) []Change {
	var out []Change
	seen := map[string]bool{}
	for _, u := range bf.Unsafe {
		seen[u.Key()] = true
	}
	for _, u := range hf.Unsafe {
		if seen[u.Key()] {
			continue
		}
		out = append(out, Change{Risk: High, Category: CatCode,
			Subject: fmt.Sprintf("%s:%d", u.File, u.Line),
			Detail:  "new unsafe block: " + u.Reason,
			Why:     "raw SQL or a banned call weakens a framework guarantee"})
	}
	return out
}

func depChanges(bd, hd []model.Dep) []Change {
	var out []Change
	before := map[string]model.Dep{}
	for _, d := range bd {
		before[d.Path] = d
	}
	for _, d := range hd {
		prev, had := before[d.Path]
		if !had {
			out = append(out, Change{Risk: High, Category: CatDep, Subject: d.Path,
				Detail: "new dependency at " + d.Version,
				Why:    "a new module dependency is a new supply chain"})
			continue
		}
		if prev.Version == d.Version {
			continue
		}
		risk := Low
		what := "patch bump"
		switch {
		case major(prev.Version) != major(d.Version):
			risk, what = High, "major version bump"
		case minor(prev.Version) != minor(d.Version):
			risk, what = Medium, "minor version bump"
		}
		out = append(out, Change{Risk: risk, Category: CatDep, Subject: d.Path,
			Detail: what + ": " + prev.Version + " -> " + d.Version})
	}
	return out
}

func major(v string) string { return part(v, 0) }
func minor(v string) string { return part(v, 1) }

func part(v string, idx int) string {
	s := strings.TrimPrefix(v, "v")
	parts := strings.Split(s, ".")
	if len(parts) <= idx {
		return "0"
	}
	return parts[idx]
}

// countNew counts entries present in the head list but not in the base list.
func countNew[T any](base, head []T, key func(a, b T) bool) int {
	n := 0
	for _, h := range head {
		found := false
		for _, b := range base {
			if key(b, h) {
				found = true
				break
			}
		}
		if !found {
			n++
		}
	}
	return n
}

func countNewDeps(base, head []model.Dep) int {
	set := map[string]bool{}
	for _, d := range base {
		set[d.Path] = true
	}
	n := 0
	for _, d := range head {
		if !set[d.Path] {
			n++
		}
	}
	return n
}

// orHigh defaults an unset risk to High, the safe reading for an access change.
func orHigh(r Risk) Risk {
	if r == 0 {
		return High
	}
	return r
}
