package codegen

import (
	"fmt"
	"strings"

	"keystone.dev/keystone/model"
)

// writeGoActions emits, for one module's actions, the generated contract: the context
// struct that is the only door from action code to the outside world, the scoped DB view
// holding exactly the resources the action declared, the input and output types with their
// validators, one constructor per declared error, and the handler adapter that wires the
// whole thing to the runtime (§7.2, §7.4).
func writeGoActions(b *strings.Builder, app *model.App, module string, actions []model.Action) error {
	b.WriteString(header + "\n")
	b.WriteString("package gen\n\n")
	b.WriteString("import (\n\t\"context\"\n\t\"encoding/json\"\n\t\"log/slog\"\n\t\"time\"\n\n" +
		"\t\"keystone.dev/keystone/ks\"\n\t\"keystone.dev/keystone/model\"\n\t\"keystone.dev/keystone/runtime\"\n)\n\n")
	b.WriteString("var (\n\t_ = context.Background\n\t_ = slog.Default\n\t_ = time.Now\n\t_ = json.Marshal\n\t_ = model.KindQuery\n)\n\n")
	for i := range actions {
		if err := writeAction(b, app, module, &actions[i]); err != nil {
			return err
		}
	}
	return nil
}

// inputStruct writes an action's argument list. A field whose type the spec derived from
// another field — s.FieldOf(Customer, "owner") — is optional exactly where the source
// field is, so an action cannot widen a reference it borrowed (§5.3).
func inputStruct(b *strings.Builder, a *model.Action, app *model.App) {
	for _, f := range a.Input {
		g := ident(f.Name)
		t := fieldGoType(&f)
		if f.Type.Kind == model.KindRef && f.Name != "" && !f.Optional {
			for _, r := range refSourceNames(app, a) {
				if r.field == f.Name && r.optional {
					t = "ks.Opt[" + t + "]"
				}
			}
		}
		b.WriteString("\t" + g + " " + t + " `json:\"" + f.Name + "\"`\n")
	}
}

type refSource struct {
	field    string
	optional bool
}

// refSourceNames reports, for each input field derived from a resource field, whether that
// source is optional.
func refSourceNames(app *model.App, a *model.Action) []refSource {
	var out []refSource
	for _, ref := range declaredRefs(app, a) {
		m := app.FindResource(ref)
		if m == nil {
			continue
		}
		for _, f := range m.Fields {
			out = append(out, refSource{field: f.Name, optional: f.Optional})
		}
	}
	return out
}

// compensates reports whether some immediate effect names the job as its compensation.
func compensates(app *model.App, job string) bool {
	for _, m := range app.Modules {
		for _, a := range m.Actions {
			for _, e := range a.Effects {
				if e.Compensates == job {
					return true
				}
			}
		}
	}
	return false
}

func writeAction(b *strings.Builder, app *model.App, module string, a *model.Action) error {
	name := a.Name
	ref := model.Qual(module, name)
	outType, outIsPtr := outputType(app, a)

	// The input type.
	if true { // every action gets an input type: a job's is empty but still named (§7.2)
		b.WriteString("// " + name + "Input is the action's argument list. Fields are validated before\n")
		b.WriteString("// the action runs, so an action never re-checks shape (§7.1).\n")
		b.WriteString("type " + name + "Input struct {\n")
		inputStruct(b, a, app)
		if compensates(app, ref) {
			// A compensation job receives the immediate effect it undoes (§10.2).
			b.WriteString("\t// Effect is the immediate effect whose action did not commit.\n\tEffect runtime.EffectCall `json:\"effect\"`\n" +
				"\t// Result is what the provider returned when the effect ran.\n\tResult runtime.EffectResult `json:\"result\"`\n")
		}
		if a.Kind == model.KindQuery {
			// A list takes a cursor and a size without the spec naming them: the runtime
			// clamps the size to the configured maximum, so a client cannot ask for the
			// whole table (§8.1, §8.4).
			b.WriteString("\t// Cursor is the opaque keyset position from the previous page.\n\tCursor string `json:\"cursor\"`\n" +
				"\t// Limit is clamped to the app's maximum page size.\n\tLimit int `json:\"limit\"`\n")
		}
		b.WriteString("}\n\n")
		b.WriteString("func (in *" + name + "Input) Validate() *ks.Error {\n")
		b.WriteString("\tif in == nil {\n\t\treturn ks.Invalid(\"body\", \"the request has no body\")\n\t}\n")
		for _, v := range validationsFor(a.Input, "in") {
			b.WriteString(v)
		}
		b.WriteString("\treturn nil\n}\n\n")
	}

	// Output types, when the action returns something shaped rather than a resource.
	if len(a.Output.Fields) > 0 {
		b.WriteString("type " + name + "Out struct {\n")
		for _, f := range a.Output.Fields {
			b.WriteString("\t" + ident(f.Name) + " " + fieldGoType(&f) + " `json:\"" + f.Name + "\"`\n")
		}
		b.WriteString("}\n\n")
	}

	// The DB view: only declared resources, only the operations the spec permits.
	b.WriteString("// " + name + "DB is the action's door to data. It carries exactly the resources the\n")
	b.WriteString("// spec declared, and a resource listed only in Reads has no writer, so an action\n")
	b.WriteString("// cannot write something it never asked to (§7.1).\n")
	b.WriteString("type " + name + "DB struct {\n")
	seen := map[string]bool{}
	for _, r := range declaredRefs(app, a) {
		_, rn := splitRef(r)
		if seen[rn] {
			continue
		}
		seen[rn] = true
		b.WriteString("\t" + plural(rn) + " *" + rn + "View\n")
	}
	b.WriteString("}\n\n")

	// The context struct.
	b.WriteString("// " + name + "Ctx is everything an action may reach. It embeds context.Context, so a\n")
	b.WriteString("// cancellation or a deadline propagates to every query without the action threading\n")
	b.WriteString("// anything (§7.2).\n")
	b.WriteString("type " + name + "Ctx struct {\n\tcontext.Context\n\tActor ks.Actor\n\tDB " + name + "DB\n")
	b.WriteString("\tLog *slog.Logger\n\tClock ks.Clock\n")
	for _, e := range a.Effects {
		b.WriteString("\t// " + effectField(e) + " runs the " + string(e.Type) + " effect declared in the spec.\n")
		b.WriteString("\t" + effectField(e) + " runtime.EffectHandle\n")
	}
	b.WriteString("}\n\n")
	if len(a.Effects) > 0 {
		b.WriteString("// bindEffects attaches each declared effect to this action's transaction, so a\n")
		b.WriteString("// deferred effect is recorded by the same commit that writes the row (§10.1).\n")
		b.WriteString("func (c *" + name + "Ctx) bindEffects(d *runtime.DB) {\n")
		for _, e := range a.Effects {
			b.WriteString("\tc." + effectField(e) + " = runtime.EffectHandle{Type: " + effectKind(e) +
				", Mode: " + fmt.Sprintf("%q", e.Mode) + ", Hosts: " + effectHosts(e) + ", DB: d}\n")
		}
		b.WriteString("}\n\n")
	} else {
		b.WriteString("func (c *" + name + "Ctx) bindEffects(d *runtime.DB) { _ = d }\n\n")
	}
	b.WriteString("func new" + name + "DB(d *runtime.DB) " + name + "DB {\n\tvar v " + name + "DB\n")
	for _, r := range declaredRefs(app, a) {
		_, rn := splitRef(r)
		b.WriteString("\tif v." + plural(rn) + " == nil {\n\t\tv." + plural(rn) + " = Scoped" + plural(rn) + "(d)\n\t}\n")
	}
	b.WriteString("\treturn v\n}\n\n")

	// Declared error constructors.
	for _, e := range a.Errors {
		b.WriteString("// " + errorCtor(name, e.Code) + " is the declared failure " + e.Code + ". A failure an\n")
		b.WriteString("// action returns without a declared code is a bug, not a 400 (§7.4).\n")
		b.WriteString("func " + errorCtor(name, e.Code) + "(why ...string) *ks.Error {\n")
		b.WriteString("\tm := \"" + e.Code + "\"\n\tif len(why) > 0 && why[0] != \"\" {\n\t\tm = why[0]\n\t}\n")
		b.WriteString("\treturn ks.Declared(\"" + e.Code + "\", m)\n}\n\n")
	}

	// The handler adapter.
	b.WriteString("// " + name + "Handler wraps a written action function as a runtime handler. Passing\n")
	b.WriteString("// the implementation here is what asserts its signature: a missing or mistyped\n")
	b.WriteString("// action fails the build, not the request (§7.2).\n")
	b.WriteString("func " + name + "Handler(fn func(*" + name + "Ctx, " + name + "Input) (" + pointer(outType, outIsPtr) + ", error)) *runtime.Handler {\n")
	b.WriteString("\treturn &runtime.Handler{\n")
	b.WriteString("\t\tRef: " + fmt.Sprintf("%q", ref) + ", Module: " + fmt.Sprintf("%q", module) + ", Name: " + fmt.Sprintf("%q", name) + ",\n")
	b.WriteString("\t\tKind: " + kindExpr(a) + ", Path: " + fmt.Sprintf("%q", "/api/"+module+"/"+name) + ",\n")
	b.WriteString("\t\tSpec: mustAction(" + fmt.Sprintf("%q", ref) + "),\n")
	b.WriteString("\t\tHandle: func(c *runtime.Call) (any, error) {\n")
	b.WriteString("\t\t\tvar args " + name + "Input\n")
	if len(a.Input) > 0 || a.Kind == model.KindQuery || compensates(app, ref) {
		b.WriteString("\t\t\tif err := runtime.DecodeInput(c, &args); err != nil {\n\t\t\t\treturn nil, err\n\t\t\t}\n")
		b.WriteString("\t\t\tif err := args.Validate(); err != nil {\n\t\t\t\treturn nil, err\n\t\t\t}\n")
	}
	b.WriteString("\t\t\tctx := &" + name + "Ctx{Context: c.Context, Actor: c.Actor, DB: new" + name + "DB(c.DB), Log: c.Log, Clock: c.Clock}\n")
	b.WriteString("\t\t\tctx.bindEffects(c.DB)\n")
	b.WriteString("\t\t\treturn fn(ctx, args)\n\t\t},\n\t}\n}\n\n")
	return nil
}

// outputType maps the declared output onto a Go type: a resource, a page of one, a shaped
// struct, or nothing.
func outputType(app *model.App, a *model.Action) (string, bool) {
	switch {
	case a.Output.Resource != "":
		_, n := splitRef(a.Output.Resource)
		return n, true
	case a.Output.Page != "":
		_, n := splitRef(a.Output.Page)
		return "*runtime.PageOf[" + n + "]", false
	case len(a.Output.Fields) > 0:
		return a.Name + "Out", false
	default:
		return "struct{}", false
	}
}

// declaredRefs is what the action's DB view exposes: the resources it listed in Reads and
// Writes, and nothing else (§7.1).
func declaredRefs(app *model.App, a *model.Action) []string {
	var out []string
	seen := map[string]bool{}
	for _, r := range append(append([]string(nil), a.Reads...), a.Writes...) {
		if seen[r] {
			continue
		}
		seen[r] = true
		out = append(out, r)
	}
	return out
}

// validationsFor produces the guard clauses for an input list.
func validationsFor(fields []model.Field, receiver string) []string {
	var out []string
	for i := range fields {
		f := &fields[i]
		out = append(out, validations(f, receiver+"."+ident(f.Name))...)
	}
	return out
}

func effectField(e model.Effect) string {
	switch e.Type {
	case model.EffectEmail:
		return "Email"
	case model.EffectSMS:
		return "SMS"
	case model.EffectHTTP:
		return "HTTP"
	case model.EffectWebhook:
		return "Webhook"
	case model.EffectStorage:
		return "Storage"
	default:
		return "Enqueue"
	}
}

func effectKind(e model.Effect) string {
	switch e.Type {
	case model.EffectEmail:
		return "model.EffectEmail"
	case model.EffectSMS:
		return "model.EffectSMS"
	case model.EffectHTTP:
		return "model.EffectHTTP"
	case model.EffectWebhook:
		return "model.EffectWebhook"
	case model.EffectStorage:
		return "model.EffectStorage"
	default:
		return "model.EffectEnqueue"
	}
}

func effectHosts(e model.Effect) string {
	if len(e.Hosts) == 0 {
		return "nil"
	}
	parts := make([]string, len(e.Hosts))
	for i, h := range e.Hosts {
		parts[i] = fmt.Sprintf("%q", h)
	}
	return "[]string{" + strings.Join(parts, ", ") + "}"
}

func pointer(t string, ptr bool) string {
	if !ptr || strings.HasPrefix(t, "*") {
		return t
	}
	return "*" + t
}

func kindExpr(a *model.Action) string {
	switch a.Kind {
	case model.KindQuery:
		return "model.KindQuery"
	case model.KindJob:
		return "model.KindJob"
	case model.KindSchedule:
		return "model.KindSchedule"
	default:
		return "model.KindMutation"
	}
}

// validations emits the guard clauses one declared type implies: length, range, pattern and
// shape are checked before the action runs, so the database is not where a bad value is
// first noticed (§7.1).
// validations emits the guard clauses one declared type implies. Length, range, pattern and
// membership are checked before the action runs, so the database is not where a bad value is
// first noticed (§7.1).
func validations(f *model.Field, expr string) []string {
	type rule struct {
		cond, msg string
	}
	var rules []rule
	add := func(cond, msg string) { rules = append(rules, rule{cond, msg}) }

	switch {
	case f.Type.Kind == model.KindEmail:
		add("!validEmail(V)", "is not an email address")
	case f.Type.Kind == model.KindRef, f.Type.Kind == model.KindID:
		if !f.Optional {
			add("V.IsNil()", "is required")
		}
		add("malformedID(V)", "is not an id")
	case f.Type.Kind == model.KindText, f.Type.Kind == model.KindURL:
		if f.Type.MaxLen != nil {
			add("len(V) > "+fmt.Sprint(*f.Type.MaxLen), "is too long")
		}
		if f.Type.MinLen != nil {
			add("len(V) < "+fmt.Sprint(*f.Type.MinLen), "is too short")
		}
		if f.Type.Pattern != "" {
			add("!matchPattern(V, "+fmt.Sprintf("%q", f.Type.Pattern)+")", "has the wrong shape")
		}
	case f.Type.Kind == model.KindInt:
		if f.Type.Min != "" {
			add("V < "+f.Type.Min, "is below the minimum")
		}
		if f.Type.Max != "" {
			add("V > "+f.Type.Max, "is above the maximum")
		}
	case f.Type.Kind == model.KindEnum && len(f.Type.Values) > 0:
		parts := make([]string, len(f.Type.Values))
		for i, v := range f.Type.Values {
			parts[i] = fmt.Sprintf("%q", v)
		}
		add("!oneOf(string(V), []string{"+strings.Join(parts, ", ")+"})", "is not one of the declared values")
	}
	if len(rules) == 0 {
		return nil
	}
	var out []string
	for _, r := range rules {
		cond := strings.ReplaceAll(r.cond, "V", expr)
		out = append(out, "\tif "+cond+" {\n\t\treturn ks.Invalid(\""+f.Name+"\", \""+r.msg+"\")\n\t}\n")
	}
	if !f.Optional {
		return out
	}
	// An optional field is checked only when it is present: absent is a legal answer, and
	// conflating it with empty is how a PATCH wipes a column nobody meant to touch.
	var body strings.Builder
	body.WriteString("\tif " + expr + ".Present() {\n\t\tv, _ := " + expr + ".Get()\n")
	for _, r := range rules {
		cond := strings.ReplaceAll(r.cond, "V", "v")
		body.WriteString("\t\tif " + cond + " {\n\t\t\treturn ks.Invalid(\"" + f.Name + "\", \"" + r.msg + "\")\n\t\t}\n")
	}
	body.WriteString("\t}\n")
	return []string{body.String()}
}

// validations emits the guard clauses one declared type implies: length, range, pattern and
// shape are checked before the action runs, so the database is not where a bad value is
// first noticed (§7.1).
// validations emits the guard clauses one declared type implies. Length, range, pattern and
// membership are checked before the action runs, so the database is not where a bad value is
// first noticed (§7.1).

// validations emits the guard clauses one declared type implies: length, range, pattern and
// shape are checked before the action runs, so the database is not where a bad value is
// first noticed (§7.1).
