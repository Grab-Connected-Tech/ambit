package runtime

import (
	"fmt"
	"strings"

	"keystone.dev/keystone/ks"
	"keystone.dev/keystone/model"
	"keystone.dev/keystone/policy"
)

// BuildTable derives the storage description of a resource from the app model: the
// columns, their types, and every access rule compiled to SQL. Generated code calls it
// once per resource at package init, which keeps the compiled rules in one place — the
// model — instead of scattered through emitted files (§7.1).
func BuildTable[T any](app *model.App, module, resource string, t *Table[T]) *Table[T] {
	ref := module + "." + resource
	res := app.FindResource(ref)
	if res == nil {
		panic("keystone: no resource " + module + "." + resource + " in the app model")
	}
	if t.Ref == "" {
		t.Ref = ref
	}
	if t.Name == "" {
		t.Name = resource
	}
	if t.Table == "" {
		t.Table = res.Table
	}
	if t.IDColumn == "" {
		t.IDColumn = "id"
	}
	t.Tenant = res.Tenant
	if len(t.Columns) == 0 {
		t.Columns = ColumnsOf(res)
	}
	if len(t.Sensitive) == 0 {
		for _, c := range t.Columns {
			if c.Sensitive {
				t.Sensitive = append(t.Sensitive, c.Column)
			}
		}
	}
	alias := "t"
	if t.All.SQL == "" {
		t.All = mustFrag(policy.TableSQL(res.Access.Read, app.PolicyTable(ref), policy.TargetPolicy, alias))
	}
	if t.Create.SQL == "" {
		t.Create = mustFrag(policy.TableSQL(res.Access.Create, app.PolicyTable(ref), policy.TargetPolicy, alias))
	}
	if t.Update.SQL == "" {
		t.Update = mustFrag(policy.TableSQL(res.Access.Update, app.PolicyTable(ref), policy.TargetPolicy, alias))
	}
	if t.Delete.SQL == "" {
		t.Delete = mustFrag(policy.TableSQL(res.Access.Delete, app.PolicyTable(ref), policy.TargetPolicy, alias))
	}
	// UpdateAfter is a rule over the row after the write, which is what makes it a
	// forbidden transition rather than an invariant breach (§5.6).
	if t.UpdateAfter.SQL == "" && res.Access.UpdateAfter != nil {
		t.UpdateAfter = mustFrag(policy.TableSQL(res.Access.UpdateAfter, app.PolicyTable(ref), policy.TargetPolicy, alias))
	}
	if len(t.FieldRead) == 0 {
		for _, fr := range res.Access.FieldRead {
			col, idx := indexOfColumn(t.Columns, fr.Field)
			if col == "" {
				continue
			}
			t.FieldRead = append(t.FieldRead, FieldFrag{
				Field: fr.Field, Column: col, Index: idx,
				Frag: mustFrag(policy.TableSQL(fr.Expr, app.PolicyTable(ref), policy.TargetPolicy, alias)),
			})
		}
	}
	if len(t.FieldWrite) == 0 {
		for _, fw := range res.Access.FieldWrite {
			col := columnNamed(t.Columns, fw.Field)
			if col == "" {
				continue
			}
			t.FieldWrite = append(t.FieldWrite, FieldFrag{
				Field: fw.Field, Column: col,
				Frag: mustFrag(policy.TableSQL(fw.Expr, app.PolicyTable(ref), policy.TargetPolicy, alias)),
			})
		}
	}
	if len(t.Checks) == 0 {
		for _, inv := range res.Invariants {
			if inv.Enforcement != "deferred" {
				// A CHECK lives in the DDL; the runtime does not repeat it.
				continue
			}
			t.Checks = append(t.Checks, Check{
				Name: inv.Name,
				Frag: mustFrag(policy.TableSQL(inv.Expr, app.PolicyTable(ref), policy.TargetPolicy, alias)),
			})
		}
	}
	// UpdateAfter is a rule over the row the write would produce: a violation is a
	// forbidden transition rather than an invariant breach, which is the only difference
	// in the status code it returns (§5.6).
	if t.UpdateAfter.SQL != "" {
		t.Checks = append(t.Checks, Check{
			Name:      "update_after",
			Frag:      mustFrag(policy.TableSQL(res.Access.UpdateAfter, app.PolicyTable(ref), policy.TargetPolicy, alias)),
			Forbidden: true,
		})
	}
	return t
}

// mustFrag turns a compile failure into a startup failure: it can only mean the model
// references something the schema does not have, which is a bug in the app's spec.
func mustFrag(s *policy.SQL, err error) Frag {
	if err != nil {
		panic("keystone: compiling an access rule: " + err.Error())
	}
	return fragFromPolicy(s)
}

// ColumnsOf maps model fields onto the runtime column list, in projection order: the
// framework columns first, then the declared ones.
func ColumnsOf(res *model.Resource) []Column {
	framework := map[string]bool{}
	out := []Column{
		{Name: "id", GoName: "ID", Column: "id", SQL: "t.id", SQLType: "uuid", Kind: string(model.KindID)},
		{Name: "tenant", GoName: "TenantID", Column: "tenant_id", SQL: "t.tenant_id", SQLType: "uuid", Kind: string(model.KindID)},
		{Name: "createdAt", GoName: "CreatedAt", Column: "created_at", SQL: "t.created_at", SQLType: "timestamptz", Kind: string(model.KindTimestamp)},
		{Name: "updatedAt", GoName: "UpdatedAt", Column: "updated_at", SQL: "t.updated_at", SQLType: "timestamptz", Kind: string(model.KindTimestamp)},
		{Name: "version", GoName: "Version", Column: "version", SQL: "t.version", SQLType: "bigint", Kind: string(model.KindInt)},
	}
	if !res.Tenant {
		out = append(out[:1], out[2:]...)
	}
	for _, c := range out {
		framework[c.Column] = true
	}
	// The model carries the framework columns alongside the declared ones; the
	// projection takes the declared list, so the built-in names are skipped here rather
	// than appearing twice with different positions.
	for i := 0; i < len(res.Fields); i++ {
		f := &res.Fields[i]
		if framework[f.Column] {
			continue
		}
		out = append(out, splitField(f)...)
	}
	return out
}

// splitField expands one declared field into the columns it occupies: a money value is
// stored as a minor-units column and a currency column (§5.2).
func splitField(f *model.Field) []Column {
	base := Column{
		Name: f.Name, GoName: f.GoName, Column: f.Column, SQL: "t." + f.Column,
		SQLType: sqlTypeOf(f.Type), Kind: string(f.Type.Kind), Sensitive: f.Sensitive,
	}
	if f.Type.Kind == model.KindMoney {
		minor := Column{Name: f.Name, GoName: f.GoName, Column: f.Column + "_minor", SQL: "t." + f.Column + "_minor", SQLType: "bigint", Kind: string(model.KindMoney)}
		cur := Column{Name: f.Name, GoName: f.GoName, Column: f.Column + "_currency", SQL: "t." + f.Column + "_currency", SQLType: "text", Kind: string(model.KindMoney)}
		return []Column{minor, cur}
	}
	return []Column{base}
}

func sqlTypeOf(t model.Type) string {
	switch t.Kind {
	case model.KindInt, model.KindMoney:
		return "bigint"
	case model.KindDecimal:
		return fmt.Sprintf("numeric(%d,%d)", max(t.Precision, 1), t.Scale)
	case model.KindBool:
		return "boolean"
	case model.KindTimestamp:
		return "timestamptz"
	case model.KindDate:
		return "date"
	case model.KindJSON:
		return "jsonb"
	case model.KindBytes:
		return "bytea"
	case model.KindID, model.KindRef:
		return "uuid"
	default:
		return "text"
	}
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func indexOfColumn(cols []Column, name string) (string, int) {
	for i, c := range cols {
		if c.Name == name {
			return c.Column, i
		}
	}
	return "", -1
}

func columnNamed(cols []Column, name string) string {
	c, _ := indexOfColumn(cols, name)
	return c
}

// SetColumn is one field of an insert or a patch: the column and the value.
func SetColumn(column string, val any) Assignment {
	return Assignment{Column: column, SQL: "t." + quote(column) + " = ?", Args: []any{val}}
}

// SetOpt adds a column only when the caller actually supplied it. Absent and empty are
// different things, which is why optional fields are ks.Opt[T] (§5.2).
func SetOpt[T any](column string, v ks.Opt[T]) []Assignment {
	if !v.Present() {
		return nil
	}
	return []Assignment{SetColumn(column, v.Ptr())}
}

// SetExpr writes a column from SQL rather than a value, for the cases where the database
// is the better arithmetic (a counter bump, now()).
func SetExpr(column, expr string, args ...any) Assignment {
	return Assignment{Column: column, SQL: "t." + quote(column) + " = " + expr, Args: args}
}

// AssignmentsOf is the patch-to-columns bridge generated code calls: it never assigns a
// framework column from application input (§9.1).
func AssignmentsOf(pairs ...Assignment) []Assignment {
	out := make([]Assignment, 0, len(pairs))
	for _, a := range pairs {
		if a.Column == "" {
			continue
		}
		if strings.HasPrefix(a.SQL, "t.") || a.SQL == "" {
			a.SQL = "t." + quote(a.Column) + " = ?"
			if len(a.Args) == 0 {
				continue
			}
		}
		out = append(out, a)
	}
	return out
}

var _ = strings.HasPrefix
