// Package migrate turns app models into Postgres DDL and into classified migration
// plans (§13). Every change the spec can cause in the schema is named, and the
// classification decides whether it may run at all.
package migrate

import (
	"crypto/sha256"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/Grab-Connected-Tech/ambit/model"
	"github.com/Grab-Connected-Tech/ambit/policy"
)

// DDL renders the full schema for an app: framework tables first, then every
// resource, with constraints, indexes and row-level security (§8.5).
func DDL(a *model.App) []string {
	var out []string
	// Order matters: the tenant table first, because the framework's own tables
	// reference it, then the framework, then module tables.
	out = append(out, helpersDDL()...)
	if a.Tenant != nil {
		out = append(out, tenantTableDDL(a, *a.Tenant)...)
	}
	if a.Account != nil {
		out = append(out, accountTableDDL(a, *a.Account)...)
	}
	out = append(out, FrameworkDDL(a)...)
	for _, ref := range orderedResources(a) {
		res := a.FindResource(ref)
		if res == nil || frameworkOwned(a, res) {
			continue // the tenant, account and am_* tables are written above
		}
		out = append(out, resourceDDL(a, ref, *res)...)
	}
	return out
}

// frameworkOwned reports a table the framework writes itself.
func frameworkOwned(a *model.App, r *model.Resource) bool {
	if strings.HasPrefix(r.Table, "am_") {
		return true
	}
	if a.Tenant != nil && (r.Name == a.Tenant.Name || r.Table == a.Tenant.Table) {
		return true
	}
	if a.Account != nil && (r.Name == a.Account.Name || r.Table == a.Account.Table) {
		return true
	}
	return false
}

// orderedResources sorts resources so a table is created after the ones it points at.
// Cycles fall back to declaration order, which the compiler would have rejected anyway.
func orderedResources(a *model.App) []string {
	pending := make([]string, 0, len(a.AllResources()))
	for _, r := range a.AllResources() {
		pending = append(pending, r.Ref)
	}
	sort.Strings(pending)
	var done []string
	have := map[string]bool{}
	for _, fw := range a.Framework() {
		have[fw.Name] = true
		have[fw.Table] = true
	}
	for len(pending) > 0 {
		progressed := false
		var next []string
		for _, ref := range pending {
			res := a.FindResource(ref)
			if res == nil {
				continue
			}
			ready := true
			for _, f := range res.Fields {
				if f.Ref == nil || f.Ref.Resource == ref {
					continue
				}
				// By table: an app-level target is spelled "app.User", "User" or its table.
				if t := a.FindResource(f.Ref.Resource); t == nil || !have[t.Table] {
					ready = false
				}
			}
			if ready {
				done = append(done, ref)
				have[ref] = true
				if r := a.FindResource(ref); r != nil {
					have[r.Table] = true
				}
				progressed = true
				continue
			}
			next = append(next, ref)
		}
		if !progressed {
			done = append(done, next...)
			break
		}
		pending = next
	}
	return done
}

// helpersDDL writes the SQL functions the framework's own statements and triggers use.
func helpersDDL() []string {
	return []string{
		`CREATE OR REPLACE FUNCTION am_new_id() RETURNS uuid AS $$
  SELECT gen_random_uuid()
$$ LANGUAGE sql VOLATILE;`,
	}
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
	return []string{fmt.Sprintf("COMMENT ON TABLE %s IS 'resource %s; managed by am, do not edit'", quoteIdent(r.Table), r.Name)}
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
	type index struct {
		name   string
		cols   []string
		unique bool
		where  string
	}
	var all []index
	add := func(name string, cols []string, unique bool, where string) {
		if len(cols) == 0 {
			return
		}
		// Every scoped query filters on the tenant first, and uniqueness holds within a
		// tenant: a global unique index would let one tenant learn, from a conflict,
		// that another tenant holds a value (§1.1, §8.1).
		if r.Tenant && cols[0] != quoteIdent("tenant_id") {
			cols = append([]string{quoteIdent("tenant_id")}, cols...)
		}
		// Index names share one namespace per schema, so they carry the table.
		if !strings.HasPrefix(name, r.Table+"_") {
			name = r.Table + "_" + name
		}
		all = append(all, index{name, cols, unique, where})
	}
	for _, ix := range r.Indexes {
		var cols []string
		for _, fname := range ix.Fields {
			if f := r.FindField(fname); f != nil {
				for _, c := range f.StorageColumns() {
					cols = append(cols, quoteIdent(c))
				}
			}
		}
		where := ""
		if ix.Where != nil {
			if sql, err := policy.TableSQL(ix.Where, a.PolicyTable(ref), policy.TargetDDL, r.Table); err == nil {
				where = sql.Text
			}
		}
		add(ix.Name, cols, ix.Unique, where)
	}
	for _, f := range r.Fields {
		if f.Type.Kind == model.KindRef && f.Ref != nil {
			add(f.Column+"_idx", []string{quoteIdent(f.Column)}, false, "")
		}
		if f.Unique && f.Name != "id" && len(f.Columns) <= 1 { // a plain field has no Columns list
			add(f.Column+"_uniq", []string{quoteIdent(f.Column)}, true, "")
		}
	}
	// The tenant table's tenant_id is its own primary key, which is already indexed.
	if r.Tenant && (a.Tenant == nil || a.Tenant.Table != r.Table) {
		add("tenant", []string{quoteIdent("tenant_id")}, false, "")
	}
	// An index is redundant when another one has the same definition, or (if it is not
	// unique) when its columns lead another index with the same predicate: Postgres uses
	// the longer one for both, and every extra index costs every write.
	covered := func(x index) bool {
		for _, y := range all {
			if y.name == x.name || y.where != x.where || len(y.cols) < len(x.cols) {
				continue
			}
			same := strings.Join(y.cols[:len(x.cols)], ",") == strings.Join(x.cols, ",")
			if !same {
				continue
			}
			if len(y.cols) > len(x.cols) && !x.unique {
				return true
			}
			if len(y.cols) == len(x.cols) && y.unique == x.unique && y.name < x.name {
				return true // keep one of two identical indexes
			}
			if len(y.cols) == len(x.cols) && y.unique && !x.unique {
				return true // a unique index already serves lookups
			}
		}
		return false
	}
	var out []string
	for _, x := range all {
		if covered(x) {
			continue
		}
		kw := "INDEX"
		if x.unique {
			kw = "UNIQUE INDEX"
		}
		sql := fmt.Sprintf("CREATE %s %s ON %s (%s)", kw, quoteIdent(x.name), quoteIdent(r.Table), strings.Join(x.cols, ", "))
		if x.where != "" {
			sql += " WHERE " + x.where
		}
		out = append(out, sql+";")
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
			fmt.Sprintf("DROP POLICY IF EXISTS am_public ON %s;", quoteIdent(r.Table)),
			fmt.Sprintf("CREATE POLICY am_public ON %s AS PERMISSIVE FOR ALL USING (true) WITH CHECK (true);", quoteIdent(r.Table)),
		}
	}
	pred := strings.ReplaceAll(policy.RLSPredicate(), "__KS_ROOT__.", quoteIdent(r.Table)+".")
	return []string{
		fmt.Sprintf("ALTER TABLE %s ENABLE ROW LEVEL SECURITY;", quoteIdent(r.Table)),
		fmt.Sprintf("ALTER TABLE %s FORCE ROW LEVEL SECURITY;", quoteIdent(r.Table)),
		fmt.Sprintf("DROP POLICY IF EXISTS am_tenant_isolation ON %s;", quoteIdent(r.Table)),
		fmt.Sprintf("CREATE POLICY am_tenant_isolation ON %s AS PERMISSIVE FOR ALL USING (%s) WITH CHECK (%s);",
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
		"CREATE TRIGGER %s BEFORE UPDATE ON %s FOR EACH ROW EXECUTE FUNCTION am_guard_immutable(%s);",
		quoteIdent(r.Table+"_immutable"), quoteIdent(r.Table), strings.Join(cols, ", "))}
}

// tenantTableDDL writes the tenant table. Its tenant_id is its own id, so the same
// isolation policy as every tenant resource applies: one tenant cannot read another's
// row even through a query that forgets the filter. Creating a tenant (signup) is
// therefore a cross-tenant or system operation (§8.5).
func tenantTableDDL(a *model.App, fw model.Tenant) []string {
	r := model.Resource{Name: fw.Name, Table: fw.Table, Fields: fw.Fields, Tenant: hasColumn(fw.Fields, "tenant_id"), Timestamps: true}
	return resourceDDL(a, fw.Name, r)
}

func hasColumn(fields []model.Field, col string) bool {
	for _, f := range fields {
		if f.Column == col {
			return true
		}
	}
	return false
}

// UpgradeDDL is what a framework upgrade re-applies when the DDL it generates changed
// but the model did not. In the transaction: every table's row-level security, and the
// removal of indexes on app tables that the framework no longer generates (indexes
// behind constraints are kept). Outside it: every generated index rebuilt concurrently,
// so a changed definition under an unchanged name takes effect.
//
// ponytail: a rebuilt index is missing between its drop and its concurrent create, which
// slows queries that need it during the release; a framework upgrade is rare enough.
// Building under a temporary name and renaming would close that gap.
func UpgradeDDL(a *model.App) (inTx, outside []string) {
	var keep, tables []string
	for _, stmt := range DDL(a) {
		s := strings.TrimSpace(stmt)
		switch {
		case strings.Contains(s, "ROW LEVEL SECURITY") || strings.HasPrefix(s, "DROP POLICY") || strings.HasPrefix(s, "CREATE POLICY"):
			inTx = append(inTx, stmt)
		case strings.HasPrefix(s, "CREATE INDEX \"") || strings.HasPrefix(s, "CREATE UNIQUE INDEX \""):
			name := strings.SplitN(strings.SplitN(s, "\"", 3)[1], "\"", 2)[0]
			keep = append(keep, quoteSQL(name))
			outside = append(outside, "DROP INDEX CONCURRENTLY IF EXISTS "+quoteIdent(name)+";",
				strings.Replace(s, "INDEX ", "INDEX CONCURRENTLY ", 1))
		}
	}
	for _, r := range a.AllResources() {
		tables = append(tables, quoteSQL(r.Resource.Table))
	}
	if a.Tenant != nil {
		tables = append(tables, quoteSQL(a.Tenant.Table))
	}
	if a.Account != nil {
		tables = append(tables, quoteSQL(a.Account.Table))
	}
	if len(keep) == 0 {
		keep = append(keep, "''")
	}
	inTx = append(inTx, fmt.Sprintf(`DO $$ DECLARE r record; BEGIN
  FOR r IN SELECT i.indexname FROM pg_indexes i
    WHERE i.schemaname = 'public' AND i.tablename IN (%s) AND i.indexname NOT IN (%s)
      AND NOT EXISTS (SELECT 1 FROM pg_constraint c WHERE c.conindid = format('%%I', i.indexname)::regclass)
  LOOP EXECUTE format('DROP INDEX %%I', r.indexname); END LOOP;
END $$;`, strings.Join(tables, ", "), strings.Join(keep, ", ")))
	return inTx, outside
}

// DDLHash identifies the schema the framework generates for a model.
func DDLHash(a *model.App) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(strings.Join(DDL(a), "\n"))))
}

func accountTableDDL(a *model.App, fw model.Tenant) []string {
	// The account is tenant-scoped (its RLS filters on tenant_id). The model usually
	// spells the built-in columns already; add only the ones it does not.
	var fields []model.Field
	for _, b := range model.BuiltInFields(true) {
		if !slices.ContainsFunc(fw.Fields, func(f model.Field) bool { return f.Column == b.Column }) {
			fields = append(fields, b)
		}
	}
	r := model.Resource{Name: fw.Name, Table: fw.Table, Tenant: true, Fields: append(fields, fw.Fields...), Timestamps: true}
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
