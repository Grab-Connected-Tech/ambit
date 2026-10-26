// Package migrate turns app models into Postgres DDL and into classified migration
// plans (§13). Every change the spec can cause in the schema is named, and the
// classification decides whether it may run at all.
package migrate

import (
	"fmt"
	"sort"
	"strings"

	"keystone.dev/keystone/model"
	"keystone.dev/keystone/policy"
)

// DDL renders the full schema for an app: framework tables first, then every
// resource, with constraints, indexes and row-level security (§8.5).
func DDL(a *model.App) []string {
	var out []string
	out = append(out, FrameworkDDL(a)...)
	for _, m := range a.Modules {
		for _, r := range m.Resources {
			out = append(out, resourceDDL(a, model.Qual(m.Name, r.Name), r)...)
		}
	}
	for _, fw := range a.Framework() {
		if fw.Name == "Membership" || fw.Name == a.App.Tenant || (a.Account != nil && fw.Name == a.Account.Name) {
			continue
		}
		out = append(out, tenantTableDDL(a, fw)...)
	}
	if a.Tenant != nil {
		out = append(out, tenantTableDDL(a, *a.Tenant)...)
	}
	if a.Account != nil {
		out = append(out, accountTableDDL(a, *a.Account)...)
	}
	return out
}

func resourceDDL(a *model.App, ref string, r model.Resource) []string {
	var b strings.Builder
	cols := []string{}
	for _, f := range r.Fields {
		cols = append(cols, columnDDL(a, r, f))
	}
	fmt.Fprintf(&b, "CREATE TABLE %s (\n", quoteIdent(r.Table))
	lines := make([]string, 0, len(cols)+len(r.Invariants)+1)
	lines = append(lines, cols...)
	lines = append(lines, "  CONSTRAINT "+quoteIdent(r.Table+"_pk")+" PRIMARY KEY (id)")
	for _, inv := range r.Invariants {
		if inv.Enforcement != "check" {
			continue
		}
		sql, err := policy.TableSQL(inv.Expr, a.PolicyTable(ref), policy.TargetDDL, r.Table)
		if err != nil {
			continue
		}
		lines = append(lines, fmt.Sprintf("  CONSTRAINT %s CHECK (%s)", quoteIdent(r.Table+"_"+inv.Name), sql.Text))
	}
	b.WriteString(strings.Join(lines, ",\n"))
	b.WriteString("\n);")
	stmts := []string{b.String()}
	stmts = append(stmts, commentDDL(r)...)
	stmts = append(stmts, indexDDL(a, ref, r)...)
	stmts = append(stmts, rlsof(r)...)
	stmts = append(stmts, immutableTrigger(a, ref, r)...)
	return stmts
}

func commentDDL(r model.Resource) []string {
	return []string{fmt.Sprintf("COMMENT ON TABLE %s IS 'resource %s; managed by ks, do not edit'", quoteIdent(r.Table), r.Name)}
}

func columnDDL(a *model.App, r model.Resource, f model.Field) string {
	switch f.Type.Kind {
	case model.KindMoney:
		null := " NOT NULL"
		if f.Optional {
			null = ""
		}
		return fmt.Sprintf("  %s bigint%s,\n  %s text%s DEFAULT %s CHECK (%s ~ '^[A-Z]{3}$')",
			quoteIdent(f.Columns[0]), null,
			quoteIdent(f.Columns[1]), null, quoteSQL(f.Type.Currency), quoteIdent(f.Columns[1]))
	case model.KindRef, model.KindID:
		if f.Name == "tenant_id" {
			tenant := "org"
			if a.Tenant != nil {
				tenant = a.Tenant.Table
			}
			return fmt.Sprintf("  %s uuid NOT NULL REFERENCES %s(id) ON DELETE CASCADE", quoteIdent(f.Column), quoteIdent(tenant))
		}
		null := " NOT NULL"
		onDelete := "RESTRICT"
		if f.Optional {
			null = ""
		}
		if f.Ref != nil {
			switch f.Ref.OnDelete {
			case "cascade":
				onDelete = "CASCADE"
			case "set_null":
				onDelete = "SET NULL"
			}
		}
		target := ""
		if f.Ref != nil {
			target = a.TableName(f.Ref.Resource)
		}
		if target == "" && f.Name != "id" && strings.HasSuffix(f.Column, "_id") {
			// A bare id with no declared relation stays a plain uuid: no FK, so the
			// database cannot silently block a delete the spec never mentioned.
			target = ""
		}
		if target == "" {
			if f.Name == "id" {
				return fmt.Sprintf("  %s uuid%s", quoteIdent(f.Column), null)
			}
			return fmt.Sprintf("  %s uuid%s", quoteIdent(f.Column), null)
		}
		return fmt.Sprintf("  %s uuid%s REFERENCES %s(id) ON DELETE %s", quoteIdent(f.Column), null, quoteIdent(target), onDelete)
	case model.KindTimestamp:
		def := ""
		if f.Default != nil && f.Default.Kind == "now" {
			def = " DEFAULT now()"
		}
		null := " NOT NULL"
		if f.Optional {
			null = ""
			if def == "" {
				null = ""
			}
		}
		return fmt.Sprintf("  %s timestamptz%s%s", quoteIdent(f.Column), null, def)
	case model.KindEnum:
		null := " NOT NULL"
		if f.Optional {
			null = ""
		}
		def := ""
		if f.Default != nil {
			def = " DEFAULT " + quoteSQL(f.Default.Text)
		}
		vals := make([]string, len(f.Type.Values))
		for i, v := range f.Type.Values {
			vals[i] = quoteSQL(v)
		}
		check := ""
		if len(vals) > 0 {
			check = fmt.Sprintf(" CHECK (%s IN (%s))", quoteIdent(f.Column), strings.Join(vals, ","))
		}
		return fmt.Sprintf("  %s text%s%s%s", quoteIdent(f.Column), null, def, check)
	case model.KindDecimal:
		null := " NOT NULL"
		if f.Optional {
			null = ""
		}
		return fmt.Sprintf("  %s %s%s%s", quoteIdent(f.Column), f.SQLType(), null, defaultSQL(f))
	case model.KindBytes:
		null := " NOT NULL"
		if f.Optional {
			null = ""
		}
		return fmt.Sprintf("  %s bytea%s", quoteIdent(f.Column), null)
	case model.KindDate:
		null := " NOT NULL"
		if f.Optional {
			null = ""
		}
		return fmt.Sprintf("  %s date%s%s", quoteIdent(f.Column), null, defaultSQL(f))
	case model.KindJSON:
		null := " NOT NULL"
		if f.Optional {
			null = ""
		}
		return fmt.Sprintf("  %s jsonb%s DEFAULT '{}'::jsonb", quoteIdent(f.Column), null)
	case model.KindBool:
		null := " NOT NULL"
		if f.Optional {
			null = ""
		}
		def := ""
		if f.Default != nil {
			def = " DEFAULT " + f.Default.Text
		}
		return fmt.Sprintf("  %s boolean%s%s", quoteIdent(f.Column), null, def)
	default:
		// text, email, url and encrypted values
		typ := f.SQLType()
		if f.Type.Encrypted {
			typ = "bytea"
		}
		null := " NOT NULL"
		if f.Optional {
			null = ""
		}
		def := defaultSQL(f)
		extra := ""
		switch {
		case f.Type.MaxLen != nil && !f.Type.Encrypted:
			extra = fmt.Sprintf(" CHECK (length(%s) <= %d)", quoteIdent(f.Column), *f.Type.MaxLen)
		case f.Type.Pattern != "" && !f.Type.Encrypted:
			extra = fmt.Sprintf(" CHECK (%s ~ %s)", quoteIdent(f.Column), quoteSQL(f.Type.Pattern))
		}
		ck := ""
		if f.Type.Kind == model.KindEmail && !f.Type.Encrypted {
			ck = fmt.Sprintf(" CHECK (%s ~ %s)", quoteIdent(f.Column), quoteSQL(emailPattern))
		}
		return fmt.Sprintf("  %s %s%s%s%s%s", quoteIdent(f.Column), typ, null, def, extra, ck)
	}
}

const emailPattern = `^[^@[:space:]]+@[^@[:space:]]+\.[^@[:space:]]+$`

func defaultSQL(f model.Field) string {
	if f.Default == nil {
		return ""
	}
	switch f.Default.Kind {
	case "now":
		return " DEFAULT now()"
	case "null":
		return ""
	case "int", "bool", "decimal":
		return " DEFAULT " + f.Default.Text
	default:
		return " DEFAULT " + quoteSQL(f.Default.Text)
	}
}

// indexDDL emits declared, ref and unique indexes. Every ref gets one (§5.3).
func indexDDL(a *model.App, ref string, r model.Resource) []string {
	var out []string
	seen := map[string]bool{}
	add := func(name, cols string, unique bool, where string) {
		if seen[name] {
			return
		}
		seen[name] = true
		kw := "INDEX"
		if unique {
			kw = "UNIQUE INDEX"
		}
		sql := fmt.Sprintf("CREATE %s %s ON %s (%s)", kw, quoteIdent(name), quoteIdent(r.Table), cols)
		if where != "" {
			sql += " WHERE " + where
		}
		out = append(out, sql+";")
	}
	for _, ix := range r.Indexes {
		cols := make([]string, 0, len(ix.Fields))
		for _, fname := range ix.Fields {
			f := r.FindField(fname)
			if f == nil {
				continue
			}
			for _, c := range f.StorageColumns() {
				if ix.Unique && c == f.Column && f.Type.Kind == model.KindMoney {
					cols = append(cols, quoteIdent(c))
					continue
				}
				cols = append(cols, quoteIdent(c))
			}
		}
		where := ""
		if ix.Where != nil {
			if sql, err := policy.TableSQL(ix.Where, a.PolicyTable(ref), policy.TargetDDL, r.Table); err == nil {
				where = sql.Text
			}
		}
		add(ix.Name, strings.Join(cols, ", "), ix.Unique, where)
	}
	for _, f := range r.Fields {
		if f.Type.Kind == model.KindRef && f.Ref != nil {
			add(f.Column+"_idx", quoteIdent(f.Column), false, "")
		}
		if f.Unique && f.Name != "id" && len(f.Columns) == 1 {
			add(r.Table+"_"+f.Column+"_uniq", quoteIdent(f.Column), true, "")
		}
	}
	sort.Strings(out)
	return out
}

// rlsof emits the tenant backstop: a policy the query layer cannot accidentally
// bypass, plus FORCE so the table owner is subject to it too (§8.5).
func rlsof(r model.Resource) []string {
	if !r.Tenant {
		// A non-tenant resource has no isolation boundary, but RLS is still enabled
		// so that every table in the app behaves the same way at the database.
		return []string{
			fmt.Sprintf("ALTER TABLE %s ENABLE ROW LEVEL SECURITY;", quoteIdent(r.Table)),
			fmt.Sprintf("ALTER TABLE %s FORCE ROW LEVEL SECURITY;", quoteIdent(r.Table)),
			fmt.Sprintf("DROP POLICY IF EXISTS ks_public ON %s;", quoteIdent(r.Table)),
			fmt.Sprintf("CREATE POLICY ks_public ON %s AS PERMISSIVE FOR ALL USING (true) WITH CHECK (true);", quoteIdent(r.Table)),
		}
	}
	pred := strings.ReplaceAll(policy.RLSPredicate(), "__KS_ROOT__.", quoteIdent(r.Table)+".")
	return []string{
		fmt.Sprintf("ALTER TABLE %s ENABLE ROW LEVEL SECURITY;", quoteIdent(r.Table)),
		fmt.Sprintf("ALTER TABLE %s FORCE ROW LEVEL SECURITY;", quoteIdent(r.Table)),
		fmt.Sprintf("DROP POLICY IF EXISTS ks_tenant_isolation ON %s;", quoteIdent(r.Table)),
		fmt.Sprintf("CREATE POLICY ks_tenant_isolation ON %s AS PERMISSIVE FOR ALL USING (%s) WITH CHECK (%s);",
			quoteIdent(r.Table), pred, pred),
	}
}

// immutableTrigger refuses changes to Immutable columns in the database, which is
// the only place a race cannot slip past (§5.2).
func immutableTrigger(a *model.App, ref string, r model.Resource) []string {
	var cols []string
	for _, f := range r.Fields {
		if f.Immutable && f.Name != "tenant_id" && f.Name != "id" && f.Name != "created_at" {
			cols = append(cols, "'"+f.Column+"'")
		}
	}
	if len(cols) == 0 {
		return nil
	}
	return []string{fmt.Sprintf(
		"CREATE TRIGGER %s BEFORE UPDATE ON %s FOR EACH ROW EXECUTE FUNCTION ks_guard_immutable(%s);",
		quoteIdent(r.Table+"_immutable"), quoteIdent(r.Table), strings.Join(cols, ", "))}
}

func tenantTableDDL(a *model.App, fw model.Tenant) []string {
	r := model.Resource{Name: fw.Name, Table: fw.Table, Fields: fw.Fields, Tenant: false, Timestamps: true}
	return resourceDDL(a, fw.Name, r)
}

func accountTableDDL(a *model.App, fw model.Tenant) []string {
	r := model.Resource{Name: fw.Name, Table: fw.Table, Tenant: true, Fields: append(model.BuiltInFields(true), fw.Fields...), Timestamps: true}
	// BuiltInFields already includes tenant_id; drop the duplicate from the spec.
	var keep []model.Field
	for _, f := range r.Fields {
		if f.Name == "tenant_id" {
			continue
		}
		keep = append(keep, f)
	}
	r.Fields = keep
	return resourceDDL(a, fw.Name, r)
}

// quoteIdent protects table and column names. They come from the model, but a
// resource named with a quote must fail loudly rather than rewrite the schema.
func quoteIdent(s string) string {
	if strings.ContainsAny(s, "\"; \t\n") || s == "" {
		return `""`
	}
	return `"` + s + `"`
}

// quoteSQL renders a SQL string constant.
func quoteSQL(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }
