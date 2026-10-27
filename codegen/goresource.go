package codegen

import (
	"regexp"
	"fmt"
	"strings"

	"keystone.dev/keystone/model"
)

// This file writes the resource half of the gen package: the row struct, its enum types,
// the patch type, the storage table, the column descriptors and the relation loaders
// (§7.1). Everything is derived from the app model, so the generator has no opinion about
// the schema that could disagree with the migration.

// col is one generated column descriptor plus what a scanner must do with it.
type col struct {
	// fields are the struct members this column produces: the value, plus a loaded
	// relation for a reference.
	fields   string
	descName string // descriptor field name: "CustomerID", "Amount"
	descType string // descriptor Go type
	expr     string // descriptor initialiser
	// scan fills the struct from a runtime.Row; index is the SQL column it reads, and
	// width is how many it consumes (money takes two).
	scan  func(i int) string
	index int
	width int
	// The model's own description of the column, which decides how a patch writes it.
	field    string
	gotype   string
	column   string
	nullable bool
	ref      bool
	money    bool
}

func writeGoResources(b *strings.Builder, app *model.App, module string, resources []model.Resource) error {
	b.WriteString(header + "\n")
	b.WriteString("package gen\n\n")
	b.WriteString("import (\n\t\"encoding/json\"\n\t\"time\"\n\n\t\"keystone.dev/keystone/ks\"\n\t\"keystone.dev/keystone/runtime\"\n)\n\n")
	b.WriteString("var (\n\t_ = json.RawMessage{}\n\t_ = time.Now\n\t_ = runtime.Always\n)\n\n")
	for i := range resources {
		if err := writeResource(b, app, module, &resources[i]); err != nil {
			return err
		}
	}
	return nil
}

func writeResource(b *strings.Builder, app *model.App, module string, res *model.Resource) error {
	name := res.Name
	table := plural(name)
	cols := resourceColumns(app, module, res)

	b.WriteString("// " + name + " is one row of " + model.Qual(module, name) + " (table " + res.Table + ").\n")
	b.WriteString("type " + name + " struct {\n")
	var body strings.Builder
	for _, c := range cols {
		body.WriteString(c.fields + "\n")
		b.WriteString("\t" + strings.ReplaceAll(c.fields, "\n", "\n\t") + "\n")
	}
	b.WriteString("}\n\n")
	writeEncoder(b, name, body.String())

	for _, f := range res.Fields {
		if f.Type.Kind != model.KindEnum {
			continue
		}
		et := enumType(res, &f)
		b.WriteString("// " + et + " is the domain of " + name + "." + f.Name + ".\n")
		b.WriteString("type " + et + " string\n\nconst (\n")
		for _, v := range f.Type.Values {
			b.WriteString("\t" + et + title(v) + " " + et + " = \"" + v + "\"\n")
		}
		b.WriteString(")\n\n")
	}

	writePatch(b, name, res, cols)

	b.WriteString("// " + table + " carries the typed predicates and orderings for " +
		model.Qual(module, name) + ". A query cannot name a column the resource does not have (§7.1).\n")
	b.WriteString("var " + table + " = struct {\n")
	for _, c := range cols {
		if c.descName == "" {
			continue
		}
		b.WriteString("\t" + c.descName + " " + c.descType + "\n")
	}
	b.WriteString("}{\n")
	for _, c := range cols {
		if c.descName == "" {
			continue
		}
		b.WriteString("\t" + c.descName + ": " + c.expr + ",\n")
	}
	b.WriteString("}\n\n")

	b.WriteString("// " + name + "Table is the storage description, built from the app model at startup so\n")
	b.WriteString("// the compiled rules live in one place rather than in emitted text (§6, §7.1).\n")
	b.WriteString("var " + name + "Table = runtime.BuildTable[" + name + "](Model, \"" + module + "\", \"" + name + "\", &runtime.Table[" + name + "]{\n")
	b.WriteString("\tIDOf:      func(r *" + name + ") ks.ID { return r.ID },\n")
	b.WriteString("\tVersionOf: func(r *" + name + ") int64 { return int64(r.Version) },\n")
	b.WriteString("\tScan: func(row runtime.Row) (*" + name + ", error) {\n")
	b.WriteString("\t\tif row.Err() != nil {\n\t\t\treturn nil, row.Err()\n\t\t}\n")
	b.WriteString("\t\tout := &" + name + "{}\n")
	for _, c := range cols {
		b.WriteString("\t\t" + c.scan(c.index) + "\n")
	}
	b.WriteString("\t\treturn out, nil\n\t},\n})\n\n")

	b.WriteString("// " + name + "View is a scoped handle: the tenant filter, the read rule and the hidden\n")
	b.WriteString("// columns are applied by the type, not by the caller (§8.5, §5.6).\n")
	b.WriteString("type " + name + "View = runtime.View[" + name + "]\n\n")
	b.WriteString("func Scoped" + table + "(d *runtime.DB) *" + name + "View {\n\treturn runtime.Scoped(d, " + name + "Table)\n}\n\n")

	for i := range res.Fields {
		f := &res.Fields[i]
		if f.Type.Kind != model.KindRef || f.Ref == nil {
			continue
		}
		writeWithLoader(b, name, f)
	}
	return nil
}

// writePatch emits the patch type and, next to it, the assignment list generated code uses
// for inserts and updates: the same rule for both, so a column cannot be written one way on
// a create and another way on an update (§9.1).
// writePatch emits the patch type and the assignment list both inserts and updates use:
// one rule for how a field becomes a column write, so a field cannot be written one way on
// a create and another way on an update (§9.1).
func writePatch(b *strings.Builder, name string, res *model.Resource, cols []col) {
	writable := func(c col) bool {
		// The framework's columns are never patchable: a version belongs to concurrency
		// control and a tenant_id belongs to isolation, so neither is application input
		// (§9.1). They are recognised by position as well as by name, because the model may
		// spell a built-in field either way.
		if c.index < frameworkColumnCount(res) {
			return false
		}
		switch c.field {
		case "id", "tenant_id", "version", "created_at", "updated_at", "createdAt", "updatedAt", "tenant":
			return false
		}
		return c.descName != "" && c.gotype != ""
	}
	b.WriteString("// " + name + "Patch is the set of columns an update may change. A field that is unset\n")
	b.WriteString("// is not written at all, which is what makes PATCH mean PATCH (§7.1).\n")
	b.WriteString("type " + name + "Patch struct {\n")
	for _, c := range cols {
		if !writable(c) {
			continue
		}
		b.WriteString("\t" + c.patchName() + " " + c.patchType() + "\n")
	}
	b.WriteString("}\n\n")
	b.WriteString("// Assignments is the one place a write's column list is built. The framework's own\n")
	b.WriteString("// columns never appear, so no caller can set a version or move a row between tenants.\n")
	b.WriteString("func (p " + name + "Patch) Assignments() []runtime.Assignment {\n")
	b.WriteString("\tvar out []runtime.Assignment\n")
	for _, c := range cols {
		if !writable(c) {
			continue
		}
		f := c.patchName()
		b.WriteString("\tif p." + f + ".Present() {\n")
		switch c.patchKind() {
		case "money":
			b.WriteString("\t\tv, _ := p." + f + ".Get()\n")
			b.WriteString("\t\tout = append(out,\n\t\t\truntime.SetColumn(\"" + c.column + "_minor\", v.Minor),\n\t\t\truntime.SetColumn(\"" + c.column + "_currency\", v.Currency))\n")
		case "id":
			b.WriteString("\t\tv, _ := p." + f + ".Get()\n")
			b.WriteString("\t\tout = append(out, runtime.SetColumn(\"" + c.column + "\", v.Bytes()))\n")
		case "nullable":
			b.WriteString("\t\tout = append(out, runtime.SetColumn(\"" + c.column + "\", p." + f + ".Ptr()))\n")
		default:
			b.WriteString("\t\tv, _ := p." + f + ".Get()\n")
			b.WriteString("\t\tout = append(out, runtime.SetColumn(\"" + c.column + "\", v))\n")
		}
		b.WriteString("\t}\n")
	}
	b.WriteString("\treturn out\n}\n\n")
}

func writeWithLoader(b *strings.Builder, name string, f *model.Field) {
	target := f.Ref.Resource
	tModule, tName := splitRef(target)
	if tName == "" {
		return
	}
	pluralTarget := plural(tName)
	b.WriteString("// With" + name + f.GoName + " loads " + f.GoName + " for every row in one query, never one per\n")
	b.WriteString("// row (§8.2).\n")
	b.WriteString("var With" + name + f.GoName + " = runtime.With[" + name + "]{\n\tName: \"" + f.Name + "\",\n")
	b.WriteString("\tLoad: func(v *runtime.View[" + name + "], rows []*" + name + ") error {\n")
	b.WriteString("\t\tids := make([]ks.ID, 0, len(rows))\n\t\tat := map[ks.ID][]int{}\n\t\tfor i, r := range rows {\n")
	b.WriteString("\t\t\tif r." + f.GoName + "ID.IsNil() {\n\t\t\t\tcontinue\n\t\t\t}\n")
	b.WriteString("\t\t\tids = append(ids, r." + f.GoName + "ID)\n\t\t\tat[r." + f.GoName + "ID] = append(at[r." + f.GoName + "ID], i)\n\t\t}\n")
	b.WriteString("\t\tif len(ids) == 0 {\n\t\t\treturn nil\n\t\t}\n")
	b.WriteString("\t\tgot, err := Scoped" + pluralTarget + "(v.DB()).ByIDs(v.Context(), dedupe(ids))\n")
	b.WriteString("\t\tif err != nil {\n\t\t\treturn err\n\t\t}\n")
	b.WriteString("\t\tfor _, g := range got {\n\t\t\tfor _, i := range at[g.ID] {\n\t\t\t\trows[i]." + f.GoName + " = g\n\t\t\t}\n\t\t}\n")
	b.WriteString("\t\treturn nil\n\t},\n}\n\n")
	// The other side of the reference needs its own scoped helper in scope.
	b.WriteString("var _ = Scoped" + pluralTarget + "\n\n")
	_ = tModule
}

// resourceColumns lists the columns in projection order: the framework's, then the
// declared ones. It must agree with runtime.ColumnsOf to the position — the scanner indexes
// by position — so both read the same rule about which fields are the framework's and how
// many columns a type occupies.
// resourceColumns projects the model's field list into generated columns in the same
// order as runtime.ColumnsOf: the framework's five first, then every declared field. A
// generated scanner indexes by position, so the two must agree exactly (§6, §7.1).
func resourceColumns(app *model.App, module string, res *model.Resource) []col {
	out := []col{
		{fields: "ID ks.ID `json:\"id\"`", descName: "ID", descType: "runtime.IDCol",
			expr: fmt.Sprintf("runtime.NewIDCol(\"t.id\", %q)", model.Qual(module, res.Name)),
			scan: func(k int) string { return fmt.Sprintf("out.ID = row.ID(%d)", k) }},
	}
	if res.Tenant {
		out = append(out, col{index: 1, fields: "TenantID ks.ID `json:\"tenant_id\"`", descName: "TenantID", descType: "runtime.IDCol",
			expr: "runtime.NewIDCol(\"t.tenant_id\", \"\")",
			scan: func(k int) string { return fmt.Sprintf("out.TenantID = row.ID(%d)", k) }})
	}
	for _, c := range []col{
		{fields: "CreatedAt time.Time `json:\"created_at\"`", descName: "CreatedAt", descType: "runtime.Col[time.Time]",
			expr: "runtime.NewCol[time.Time](\"t.created_at\")",
			scan: func(k int) string { return fmt.Sprintf("out.CreatedAt = row.Time(%d)", k) }},
		{fields: "UpdatedAt time.Time `json:\"updated_at\"`", descName: "UpdatedAt", descType: "runtime.Col[time.Time]",
			expr: "runtime.NewCol[time.Time](\"t.updated_at\")",
			scan: func(k int) string { return fmt.Sprintf("out.UpdatedAt = row.Time(%d)", k) }},
		{fields: "Version ks.Version `json:\"version\"`", descName: "Version", descType: "runtime.Col[ks.Version]",
			expr: "runtime.NewCol[ks.Version](\"t.version\")",
			scan: func(k int) string { return fmt.Sprintf("out.Version = ks.Version(row.Int(%d))", k) }},
	} {
		c.index = len(runtimeCols(out))
		out = append(out, c)
	}
	// The projection starts with the framework's own columns, at these positions; a field
	// the model lists with one of these column names is that same column, not another one.
	seeded := map[string]bool{
		"id": true, "tenant_id": true, "created_at": true, "updated_at": true, "version": true,
	}
	at := 5
	if !res.Tenant {
		seeded["tenant"] = false
		at = 4
	}
	for i := range res.Fields {
		f := &res.Fields[i]
		if seeded[f.Name] || seeded[f.Column] {
			continue
		}
		c := declaredColumn(res, f)
		c.index = at
		out = append(out, c)
		at += widthOf(f)
	}
	return out
}

// runtimeCols is the framework prefix of a projection: id, tenant, created_at, updated_at
// and version are always the first five columns, or four for a resource outside a tenant.
func runtimeCols(out []col) []col { return out }

// widthOf is how many SQL columns a model field occupies. Money is stored as a pair
// (§5.2), which is the only type that takes two.
func widthOf(f *model.Field) int {
	if f.Type.Kind == model.KindMoney {
		return 2
	}
	return 1
}

func declaredColumn(res *model.Resource, f *model.Field) col {
	c := col{
		field: f.Name, column: f.Column, nullable: f.Optional,
		money: f.Type.Kind == model.KindMoney,
		ref:   f.Type.Kind == model.KindRef,
	}
	g := f.GoName
	if c.ref {
		g += "ID"
	}
	c.descName = g
	c.gotype = baseScalarOf(f)
	if c.gotype == "enum" {
		c.gotype = enumType(res, f)
	}
	// Wire names are the declared field names; a reference carries its id under the
	// column name, so the loaded row can sit under the field name (§12).
	c.fields = g + " " + optOr(f, c.gotype) + " `json:\"" + f.Name + "\"`"
	if c.ref {
		c.fields = g + " ks.ID `json:\"" + f.Column + "\"`\n// " + strings.TrimSuffix(g, "ID") + " is loaded by With" + res.Name +
			strings.TrimSuffix(g, "ID") + ", and is nil until a query asks for it (§8.2).\n" +
			strings.TrimSuffix(g, "ID") + " *" + refName(f) + " `json:\"" + f.Name + ",omitempty\"`"
	}
	c.scan = scanFor(f, g, c.gotype)
	sql := "t." + f.Column
	switch {
	case c.money:
		// The model spells the pair in f.Columns (amount_minor, amount_currency); the
		// base name is only a fallback for a model that predates it.
		minor, currency := sql+"_minor", sql+"_currency"
		if len(f.Columns) == 2 {
			minor, currency = "t."+f.Columns[0], "t."+f.Columns[1]
		}
		c.descType, c.expr, c.width = "runtime.MoneyCol", `runtime.NewMoneyCol("`+minor+`", "`+currency+`")`, 2
	case c.ref || f.Type.Kind == model.KindID:
		c.descType = "runtime.IDCol"
		c.expr = fmt.Sprintf("runtime.NewIDCol(%q, %q)", sql, targetOf(f))
	case f.Type.Kind == model.KindEnum:
		c.descType = "runtime.Col[" + c.gotype + "]"
		c.expr = `runtime.ColOf[` + c.gotype + `]("` + sql + `", "::text")`
	case f.Type.Kind == model.KindText || f.Type.Kind == model.KindEmail || f.Type.Kind == model.KindURL:
		if f.Optional {
			c.descType, c.expr = "runtime.Col[string]", `runtime.NewCol[string]("`+sql+`")`
		} else {
			c.descType, c.expr = "runtime.TextCol", `runtime.NewTextCol("`+sql+`")`
		}
	case f.Optional:
		c.descType = "runtime.OptCol[" + c.gotype + "]"
		c.expr = "runtime.NewOptCol[" + c.gotype + "](\"" + sql + "\")"
	default:
		c.descType = "runtime.Col[" + c.gotype + "]"
		c.expr = "runtime.NewCol[" + c.gotype + "](\"" + sql + "\")"
	}
	return c
}

// scanFor is the statement that fills one field from the row at a position. A nullable
// column keeps the difference between an empty value and no value at all, which is the whole
// reason optional fields are ks.Opt (§5.2).
func scanFor(f *model.Field, g, gotype string) func(i int) string {
	return func(i int) string {
		var read string
		switch gotype {
		case "string":
			read = fmt.Sprintf("row.Str(%d)", i)
		case "int64":
			read = fmt.Sprintf("row.Int(%d)", i)
		case "bool":
			read = fmt.Sprintf("row.Bool(%d)", i)
		case "time.Time":
			read = fmt.Sprintf("row.Time(%d)", i)
		case "ks.Date":
			read = fmt.Sprintf("ks.DateFromTime(row.Time(%d))", i)
		case "ks.Decimal":
			read = fmt.Sprintf("row.Decimal(%d)", i)
		case "[]byte", "json.RawMessage":
			read = fmt.Sprintf("row.Blob(%d)", i)
		case "ks.ID":
			read = fmt.Sprintf("ksMustID(row.Raw(%d))", i)
		case "ks.Money":
			read = fmt.Sprintf("ks.MoneyOf(row.Int(%d), row.Str(%d))", i, i+1)
		default: // an enum: a named string type
			read = fmt.Sprintf("%s(row.Str(%d))", gotype, i)
		}
		if f.Optional {
			return fmt.Sprintf("if row.Null(%d) { out.%s = ks.Unset[%s]() } else { out.%s = ks.Set(%s) }", i, g, gotype, g, read)
		}
		return fmt.Sprintf("out.%s = %s", g, read)
	}
}

func baseScalarOf(f *model.Field) string {
	switch f.Type.Kind {
	case model.KindID, model.KindRef:
		return "ks.ID"
	case model.KindInt:
		return "int64"
	case model.KindMoney:
		return "ks.Money"
	case model.KindBool:
		return "bool"
	case model.KindTimestamp:
		return "time.Time"
	case model.KindDate:
		return "ks.Date"
	case model.KindDecimal:
		return "ks.Decimal"
	case model.KindBytes:
		return "[]byte"
	case model.KindJSON:
		return "json.RawMessage"
	case model.KindEnum:
		return "enum"
	default:
		return "string"
	}
}

func optOr(f *model.Field, base string) string {
	if f.Optional {
		return "ks.Opt[" + base + "]"
	}
	return base
}

func optType(res *model.Resource, f *model.Field) string {
	switch {
	case f.Type.Kind == model.KindMoney:
		return "ks.Opt[ks.Money]"
	case f.Type.Kind == model.KindRef:
		return "ks.Opt[ks.ID]"
	case f.Type.Kind == model.KindEnum:
		return "ks.Opt[" + enumType(res, f) + "]"
	default:
		return "ks.Opt[" + baseScalar(f) + "]"
	}
}

func baseScalar(f *model.Field) string {
	switch f.Type.Kind {
	case model.KindInt:
		return "int64"
	case model.KindBool:
		return "bool"
	case model.KindTimestamp:
		return "time.Time"
	case model.KindDecimal:
		return "ks.Decimal"
	case model.KindBytes:
		return "[]byte"
	case model.KindJSON:
		return "json.RawMessage"
	case model.KindID, model.KindRef:
		return "ks.ID"
	default:
		return "string"
	}
}

func enumType(res *model.Resource, f *model.Field) string { return res.Name + f.GoName }

// optExpr is the descriptor constructor for a column whose Go type may be optional: the
// descriptor type and its constructor have to agree, or the generated struct will not
// compile.
func optExpr(f *model.Field, gotype, sql string) string {
	if f.Optional {
		return "runtime.NewOptCol[" + gotype + "](\"" + sql + "\")"
	}
	return "runtime.NewCol[" + gotype + "](\"" + sql + "\")"
}

func refName(f *model.Field) string {
	_, n := splitRef(f.Ref.Resource)
	return n
}

func targetOf(f *model.Field) string {
	if f.Ref != nil {
		return f.Ref.Resource
	}
	return ""
}

// The descriptor and scan helpers differ only by whether the column is nullable.
func strDescType(f *model.Field) string {
	if f.Optional {
		return "runtime.OptCol[string]"
	}
	return "runtime.TextCol"
}

func intDescType(f *model.Field) string {
	if f.Optional {
		return "runtime.OptCol[int64]"
	}
	return "runtime.Col[int64]"
}

func boolDescType(f *model.Field) string {
	if f.Optional {
		return "runtime.OptCol[bool]"
	}
	return "runtime.Col[bool]"
}

func timeDescType(f *model.Field) string {
	if f.Optional {
		return "runtime.OptCol[time.Time]"
	}
	return "runtime.Col[time.Time]"
}

func decDescType(f *model.Field) string {
	if f.Optional {
		return "runtime.OptCol[ks.Decimal]"
	}
	return "runtime.Col[ks.Decimal]"
}

// frameworkField names the columns the framework owns. It is a predicate on the *declared*
// field list, not on what the framework itself adds: those columns are emitted separately,
// and re-emitting a declared field with the same name would put it in the projection twice.
func frameworkField(f *model.Field) bool {
	switch f.Name {
	case "id", "tenant_id", "created_at", "updated_at", "version":
		return true
	}
	return false
}

func plural(name string) string {
	switch {
	case strings.HasSuffix(name, "s"), strings.HasSuffix(name, "x"),
		strings.HasSuffix(name, "ch"), strings.HasSuffix(name, "sh"):
		return name + "es"
	case strings.HasSuffix(name, "y") && len(name) > 1 && !isVowel(name[len(name)-2]):
		return name[:len(name)-1] + "ies"
	default:
		return name + "s"
	}
}

func isVowel(b byte) bool { return strings.IndexByte("aeiou", b) >= 0 }

func title(s string) string {
	if s == "" {
		return s
	}
	parts := strings.FieldsFunc(s, func(r rune) bool { return r == '_' || r == '-' || r == ' ' })
	for i, p := range parts {
		parts[i] = strings.ToUpper(p[:1]) + p[1:]
	}
	return strings.Join(parts, "")
}

// patchName is the patch struct's field for a column. A reference's column is
// customer_id, and its patch field is Customer, which is what the row's own field is called.
func (c col) patchName() string {
	if c.ref {
		return strings.TrimSuffix(c.descName, "ID")
	}
	return c.descName
}

func (c col) patchType() string {
	t := c.gotype
	if c.ref {
		t = "ks.ID"
	}
	if c.money {
		t = "ks.Money"
	}
	return "ks.Opt[" + t + "]"
}

func (c col) patchKind() string {
	switch {
	case c.money:
		return "money"
	case c.ref:
		return "id"
	case c.nullable:
		return "nullable"
	default:
		return "plain"
	}
}

func (c col) sql() string { return c.column }

// frameworkColumnCount is how many leading projection columns the framework owns.
func frameworkColumnCount(res *model.Resource) int {
	if res.Tenant {
		return 5
	}
	return 4
}

var structField = regexp.MustCompile(`(?m)^(\w+) (\S+) .*json:"([^",]+)(,omitempty)?"`)

// writeEncoder emits MarshalJSON for a row: an append-based writer instead of encoding/json
// walking the struct by reflection (§11.1). The keys are read back from the struct's own
// tags, so the two cannot disagree, and each field is written by a typed call, so nothing
// is boxed.
func writeEncoder(b *strings.Builder, name, fields string) {
	b.WriteString("// MarshalJSON writes the row without reflection (§11.1).\n")
	b.WriteString("func (x " + name + ") MarshalJSON() ([]byte, error) {\n\tw := ks.NewW(256)\n\tw.ObjStart()\n")
	for _, m := range structField.FindAllStringSubmatch(fields, -1) {
		field, typ, key := m[1], m[2], m[3]
		if m[4] != "" { // a loaded relation: present only when a query asked for it
			fmt.Fprintf(b, "\tif x.%s != nil {\n\t\tw.Key(%q)\n\t\tw.Any(x.%s)\n\t}\n", field, key, field)
			continue
		}
		fmt.Fprintf(b, "\tw.Key(%q)\n\t%s\n", key, encodeCall(typ, "x."+field))
	}
	b.WriteString("\tw.ObjEnd()\n\treturn w.Bytes(), w.Err()\n}\n\n")
}

func encodeCall(typ, v string) string {
	switch {
	case typ == "ks.ID":
		return "w.ID(" + v + ")"
	case typ == "time.Time":
		return "w.Time(" + v + ")"
	case typ == "ks.Version", typ == "int64", typ == "int":
		return "w.Num(int64(" + v + "))"
	case typ == "ks.Money":
		return "w.Money(" + v + ")"
	case typ == "bool":
		return "w.Bool(" + v + ")"
	case typ == "string":
		return "w.Str(" + v + ")"
	case strings.HasPrefix(typ, "ks.Opt["):
		return v + ".AppendTo(w)"
	case !strings.ContainsAny(typ, ".[*"): // a generated enum: a named string type
		return "w.Str(string(" + v + "))"
	}
	return "w.Any(" + v + ")"
}
