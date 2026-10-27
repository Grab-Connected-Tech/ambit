package spec

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/Grab-Connected-Tech/ambit/model"
	"github.com/Grab-Connected-Tech/ambit/policy"
)

// buildResource turns one declaration into model form, reporting every reference
// that does not resolve. The compiler adds the deeper checks later.
func buildResource(d *ResourceDecl) (*model.Resource, error) {
	if d == nil {
		return nil, fmt.Errorf("spec: nil resource declaration")
	}
	var probs []string
	res := &model.Resource{
		Name:       d.name,
		Table:      accountTable(d),
		Tenant:     d.tenant,
		Account:    d.account,
		Timestamps: true,
		Versioned:  true,
	}
	seen := map[string]bool{"id": true, "created_at": true, "updated_at": true, "version": true, "tenant_id": true}
	res.Fields = model.BuiltInFields(d.tenant)
	for _, f := range d.fields {
		if f == nil {
			probs = append(probs, d.name+": a field declaration is nil")
			continue
		}
		if f.err != "" {
			probs = append(probs, d.name+"."+f.name+": "+f.err)
			continue
		}
		if seen[f.name] {
			probs = append(probs, fmt.Sprintf("%s.%s: declared twice or reserved", d.name, f.name))
			continue
		}
		seen[f.name] = true
		mf, err := toModelField(f)
		if err != nil {
			probs = append(probs, fmt.Sprintf("%s.%s: %v", d.name, f.name, err))
			continue
		}
		res.Fields = append(res.Fields, *mf)
	}

	for _, ix := range d.indexes {
		name := ix.name
		if name == "" {
			name = snake(d.name) + "_" + strings.Join(ix.fields, "_")
			if ix.unique {
				name += "_unique"
			}
		}
		for _, f := range ix.fields {
			if !seen[f] && !knownField(res, f) {
				probs = append(probs, fmt.Sprintf("%s: index names unknown field %q", d.name, f))
			}
		}
		mi := model.Index{Name: name, Fields: ix.fields, Unique: ix.unique}
		if ix.where != nil {
			mi.Where = ix.where.Node()
		}
		res.Indexes = append(res.Indexes, mi)
	}

	// Every ref gets an index automatically (§5.3).
	for _, f := range res.Fields {
		if f.Ref == nil || f.Type.Kind == model.KindID {
			continue
		}
		name := f.Column + "_index"
		if hasIndex(res, name) {
			continue
		}
		res.Indexes = append(res.Indexes, model.Index{Name: name, Fields: []string{f.Name}})
	}
	for _, f := range res.Fields {
		if f.Unique && f.Name != "id" {
			res.Indexes = append(res.Indexes, model.Index{
				Name: snake(d.name) + "_" + f.Column + "_unique", Fields: []string{f.Name}, Unique: true,
			})
		}
	}

	for _, inv := range d.invariants {
		if inv.rule == nil {
			probs = append(probs, fmt.Sprintf("%s: invariant %q has no expression", d.name, inv.name))
			continue
		}
		if inv.name == "" || !isSnakeName(inv.name) {
			probs = append(probs, fmt.Sprintf("%s: invariant names must be snake_case, got %q", d.name, inv.name))
			continue
		}
		node := policy.Normalize(inv.rule.Node())
		if node.UsesActor() {
			probs = append(probs, fmt.Sprintf("%s: invariant %q may not reference the actor or a role; that belongs in Access", d.name, inv.name))
			continue
		}
		enforcement := "check"
		if node.CrossesRef() {
			enforcement = "deferred"
		}
		// Check that every field path resolves before it reaches the SQL compiler.
		if err := verifyPaths(node, res); err != nil {
			probs = append(probs, fmt.Sprintf("%s: invariant %q: %v", d.name, inv.name, err))
			continue
		}
		res.Invariants = append(res.Invariants, model.Invariant{Name: inv.name, Expr: node, Enforcement: enforcement})
	}

	for _, a := range d.access {
		if a.rule == nil {
			probs = append(probs, fmt.Sprintf("%s: access rule %s has no expression", d.name, a.kind))
			continue
		}
		node := policy.Normalize(a.rule.Node())
		if a.field != "" {
			if !knownField(res, a.field) {
				probs = append(probs, fmt.Sprintf("%s: %s names unknown field %q", d.name, a.kind, a.field))
				continue
			}
			rule := model.FieldRule{Field: a.field, Expr: node}
			if a.kind == "field_read" {
				res.Access.FieldRead = append(res.Access.FieldRead, rule)
			} else {
				res.Access.FieldWrite = append(res.Access.FieldWrite, rule)
			}
			continue
		}
		switch a.kind {
		case "read":
			mergeRule(&res.Access.Read, node)
		case "create":
			mergeRule(&res.Access.Create, node)
		case "update":
			mergeRule(&res.Access.Update, node)
		case "update_after":
			mergeRule(&res.Access.UpdateAfter, node)
		case "delete":
			mergeRule(&res.Access.Delete, node)
		default:
			probs = append(probs, fmt.Sprintf("%s: unknown access rule %q", d.name, a.kind))
		}
	}
	if res.Access.Delete == nil {
		// Deny by default applies to deletion too: a resource that never says
		// otherwise cannot be deleted, and no Delete method is generated (§7.1).
		res.Access.Delete = policy.Never()
	}
	for _, p := range probs {
		specErr("spec: %s", p)
	}
	if len(probs) > 0 {
		return nil, fmt.Errorf("spec: %s: %s", d.name, strings.Join(probs, "; "))
	}
	return res, nil
}

// mergeRule combines repeated declarations of the same access kind with OR, which
// is the only reading that never silently narrows what a human wrote.
func mergeRule(dst **policy.Node, add *policy.Node) {
	if *dst == nil {
		*dst = add
		return
	}
	*dst = policy.Normalize(policy.Or(*dst, add))
}

// accountTable keeps user tables out of the reserved am_ prefix.
func accountTable(d *ResourceDecl) string {
	if d.account && snake(d.name) == "user" {
		return "user_account"
	}
	return snake(d.name)
}

func hasIndex(res *model.Resource, name string) bool {
	for _, ix := range res.Indexes {
		if ix.Name == name {
			return true
		}
	}
	return false
}

func knownField(res *model.Resource, name string) bool { return res.FindField(name) != nil }

// verifyPaths walks a rule and confirms each field reference resolves, so an
// unresolvable reference fails at am gen rather than at query time (AM-SPEC-002).
func verifyPaths(n *policy.Node, res *model.Resource) error {
	var err error
	n.Walk(func(x *policy.Node) bool {
		if err != nil || x.Op != policy.OpField {
			return true
		}
		f := res.FindField(x.Path[0])
		if f == nil {
			err = fmt.Errorf("unknown field %q", x.Path[0])
			return false
		}
		if len(x.Path) == 1 {
			return true
		}
		if f.Ref == nil {
			err = fmt.Errorf("%q is not a relation, so %q cannot be read", x.Path[0], strings.Join(x.Path, "."))
			return false
		}
		tgt := f.Ref.Resource
		r := findResourceByRef(tgt)
		if r == nil {
			err = fmt.Errorf("relation %q points at a resource that is not declared", x.Path[0])
			return false
		}
		if r.FindField(x.Path[1]) == nil {
			err = fmt.Errorf("%s has no field %q", tgt, x.Path[1])
			return false
		}
		return true
	})
	return err
}

// findResourceByRef looks a resource up by its qualified name.
func findResourceByRef(ref string) *model.Resource {
	for _, d := range registry.resources {
		if d.Ref() == ref {
			res, err := buildResource(d)
			if err != nil {
				return nil
			}
			return res
		}
	}
	return nil
}

// toModelField converts one declared field.
func toModelField(f *Field) (*model.Field, error) {
	mf := &model.Field{
		Name: f.name, GoName: goName(f.name), Column: snake(f.name),
		Type: f.typ, Optional: f.optional, Default: f.def, Unique: f.unique,
		Immutable: f.immutable, Sensitive: f.sensitive,
	}
	if f.copyFrom != nil && f.copyFrom.name != f.name {
		mf.RenamedFrom = f.copyFrom.name
	}
	if f.err != "" {
		return nil, errors.New(f.err)
	}
	switch mf.Type.Kind {
	case model.KindText, model.KindEmail, model.KindURL, model.KindEnum:
		if mf.Type.Trim || f.trim {
			mf.Type.Trim = true
		}
	case model.KindMoney:
		mf.Column = snake(f.name) + "_minor"
		mf.Columns = []string{mf.Column, snake(f.name) + "_currency"}
		if mf.Type.Currency == "" {
			mf.Type.Currency = "USD"
		}
	case model.KindDecimal:
		if mf.Type.Precision == 0 {
			mf.Type.Precision = 18
		}
		if mf.Type.Scale == 0 {
			mf.Type.Scale = 4
		}
	case model.KindRef, model.KindID:
		if f.refTo == nil {
			return nil, fmt.Errorf("a ref field needs a target resource")
		}
		if f.name == "tenant" {
			mf.Name, mf.GoName, mf.Column = "tenant_id", "TenantID", "tenant_id"
		} else if mf.Type.Kind == model.KindRef {
			mf.Column = snake(f.name) + "_id"
		}
		onDelete := f.onDelete
		if onDelete == "" {
			onDelete = "restrict"
		}
		mf.Ref = &model.Ref{Resource: f.refTo.Ref(), OnDelete: onDelete}
		if mf.Type.Kind == model.KindRef {
			mf.Type.GoType = idTypeName(f.refTo)
		}
	case model.KindTimestamp, model.KindDate:
		// no extra parameters
	}
	if mf.Type.Kind == model.KindID && f.refTo != nil {
		mf.Ref = &model.Ref{Resource: f.refTo.Ref(), OnDelete: "restrict"}
		mf.Column = snake(f.name) + "_id"
	}
	if len(mf.Type.Values) > 1 {
		sorted := dedupSorted(mf.Type.Values)
		mf.Type.Values = sorted
	}
	return mf, nil
}

func idTypeName(d *ResourceDecl) string { return d.name + "ID" }

// goName converts snake_case to UpperCamelCase.
func goName(s string) string {
	parts := strings.Split(s, "_")
	var b strings.Builder
	for _, p := range parts {
		if p == "" {
			continue
		}
		b.WriteString(strings.ToUpper(p[:1]) + p[1:])
	}
	return b.String()
}

func isSnakeName(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_') {
			return false
		}
	}
	return true
}

// buildAction converts one action declaration.
func buildAction(d *ActionDecl) (*model.Action, error) {
	if d == nil {
		return nil, fmt.Errorf("spec: nil action declaration")
	}
	var probs []string
	a := &model.Action{
		Name: d.name, Kind: d.kind, Public: d.public,
		Reads: refsOf(d.reads), Writes: refsOf(d.writes), Calls: actionRefsOf(d.calls),
		Limits: d.limits, Retry: d.retry, Concurrency: d.conc,
		RunAs: d.runAs, Cron: d.cron, CrossTenant: d.crossTenant, Idempotent: d.idempotent,
		Serializable: d.serializable, ReplicaOK: d.replicaOK, AllowGet: d.allowGet,
		Deprecated: d.deprecated, Cache: d.cache,
	}
	if d.requires != nil {
		a.Requires = policy.Normalize(d.requires.Node())
	}
	if d.counts {
		a.Counts = true
	}
	if d.tz != "" {
		a.TimeZone = d.tz
	}
	if d.runs != nil {
		a.Runs = d.runs.Ref()
	}
	for _, f := range d.input {
		if f == nil {
			probs = append(probs, d.name+": an input field is nil")
			continue
		}
		if f.err != "" {
			probs = append(probs, d.name+" input: "+f.err)
			continue
		}
		mf, err := toModelField(f)
		if err != nil {
			probs = append(probs, d.name+" input: "+err.Error())
			continue
		}
		a.Input = append(a.Input, *mf)
	}
	for _, r := range d.returns {
		if r.resource != nil {
			a.Output.Resource = r.resource.Ref()
		}
		if r.page != nil {
			a.Output.Page = r.page.Ref()
		}
		for _, f := range r.fields {
			mf, err := toModelField(f)
			if err != nil {
				probs = append(probs, d.name+" output: "+err.Error())
				continue
			}
			a.Output.Fields = append(a.Output.Fields, *mf)
		}
	}
	if len(a.Output.Fields) > 0 && a.Output.Resource == "" && a.Output.Page == "" {
		a.Output.None = false
	}
	for _, e := range d.effects {
		if e == nil {
			probs = append(probs, d.name+": an effect declaration is nil")
			continue
		}
		if e.missing {
			probs = append(probs, d.name+": an "+string(e.typ)+" effect needs an allowlist, topic, bucket or job")
			continue
		}
		eff := model.Effect{Type: e.typ, Mode: e.mode, Hosts: dedupSorted(e.hosts), Topic: e.topic,
			Bucket: e.bucket, Template: e.template, Compensates: e.comp}
		if e.job != nil {
			eff.Job = e.job.Ref()
		}
		if e.after != "" {
			eff.After = e.after
		}
		a.Effects = append(a.Effects, eff)
	}
	for _, e := range d.errors {
		if e.code == "" {
			continue
		}
		if !isSnakeName(e.code) {
			probs = append(probs, d.name+": error codes must be snake_case, got "+e.code)
			continue
		}
		a.Errors = append(a.Errors, model.ActionError{Code: e.code, Message: e.message})
	}
	for _, p := range probs {
		specErr("spec: %s", p)
	}
	if len(probs) > 0 {
		return nil, fmt.Errorf("spec: %s: %s", d.name, strings.Join(probs, "; "))
	}
	return a, nil
}

func refsOf(ds []*ResourceDecl) []string {
	out := make([]string, 0, len(ds))
	for _, d := range ds {
		if d == nil {
			continue
		}
		out = append(out, d.Ref())
	}
	sort.Strings(out)
	return out
}

func actionRefsOf(ds []*ActionDecl) []string {
	out := make([]string, 0, len(ds))
	for _, d := range ds {
		if d == nil {
			continue
		}
		out = append(out, d.Ref())
	}
	sort.Strings(out)
	return out
}
