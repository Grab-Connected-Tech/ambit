package migrate

import (
	"crypto/sha256"
	"fmt"
	"sort"
	"strings"

	"keystone.dev/keystone/model"
	"keystone.dev/keystone/policy"
)

// Class is how §13.2 names a schema change. It decides what the tool may do
// automatically and what a human must approve.
type Class string

const (
	Safe            Class = "safe"
	NeedsValidation Class = "needs_validation"
	NeedsBackfill   Class = "needs_backfill"
	NeedsPlan       Class = "needs_plan"
	Destructive     Class = "destructive"
)

// Risk ranks a class for the behavior diff (§13.2).
func (c Class) Risk() string {
	switch c {
	case Destructive:
		return "high"
	case NeedsBackfill, NeedsPlan:
		return "medium"
	case NeedsValidation:
		return "low"
	}
	return "low"
}

// Step is one classified change with the SQL that performs it.
type Step struct {
	Class     Class     `json:"class"`
	Subject   string    `json:"subject"`
	Detail    string    `json:"detail"`
	InTx      []string  `json:"inTx,omitempty"`
	OutsideTx []string  `json:"outsideTx,omitempty"`
	Phases    []string  `json:"phases,omitempty"`
	Backfill  *Backfill `json:"backfill,omitempty"`
	NeedsFlag string    `json:"needsFlag,omitempty"`
}

// Backfill describes a resumable data step (§13.3).
type Backfill struct {
	Table  string `json:"table"`
	Column string `json:"column"`
	From   string `json:"from,omitempty"`
	// Set is the SQL expression each null row receives. The plan fills it when the model
	// knows the value (a declared default, a source column); otherwise the reviewer writes
	// it into the plan file before applying, and apply refuses an empty one (§13.3).
	Set   string `json:"set"`
	Batch int    `json:"batch"`
	Job   string `json:"job"`
}

// Plan is the ordered set of steps between two models.
type Plan struct {
	Name      string `json:"name"`
	BaseHash  string `json:"baseHash"`
	ModelHash string `json:"modelHash"`
	// DDLHash is the generated schema this plan leads to; a later plan that finds it
	// changed under the same model knows the framework was upgraded.
	DDLHash string `json:"ddlHash,omitempty"`
	Steps   []Step `json:"steps"`
}

// Empty reports a plan with nothing to do.
func (p Plan) Empty() bool { return len(p.Steps) == 0 }

// MaxRisk names the highest risk in the plan, which is what the diff reports.
func (p Plan) MaxRisk() string {
	out := "low"
	for _, s := range p.Steps {
		if rank(s.Class.Risk()) > rank(out) {
			out = s.Class.Risk()
		}
	}
	return out
}

func rank(s string) int {
	switch s {
	case "high":
		return 2
	case "medium":
		return 1
	}
	return 0
}

// Has returns whether any step carries a class.
func (p Plan) Has(c Class) bool {
	for _, s := range p.Steps {
		if s.Class == c {
			return true
		}
	}
	return false
}

// Diff compares two models and produces the migration plan between them. It works
// on models alone, so it runs without a database (§6.1).
func Diff(old, updated *model.App) Plan {
	p := Plan{BaseHash: old.Hash(), ModelHash: updated.Hash()}
	// The framework's own tables only appear in a plan when the framework needs a
	// new one, so a spec-only change stays free of unrelated steps.
	if frameworkHash(old) != frameworkHash(updated) {
		p.Steps = append(p.Steps, ensureFrameworkSteps(updated)...)
	}

	oldRes := indexResources(old)
	newRes := indexResources(updated)

	refs := []string{}
	for ref := range newRes {
		refs = append(refs, ref)
	}
	for ref := range oldRes {
		if _, ok := newRes[ref]; !ok {
			refs = append(refs, ref)
		}
	}
	sort.Strings(refs)

	for _, ref := range refs {
		o, had := oldRes[ref]
		n, has := newRes[ref]
		switch {
		case has && !had:
			p.Steps = append(p.Steps, Step{
				Class: Safe, Subject: ref, Detail: "add resource",
				InTx: DDLFor(updated, ref),
			})
		case had && !has:
			p.Steps = append(p.Steps, Step{
				Class: Destructive, Subject: ref, Detail: "remove resource",
				NeedsFlag: "--allow-destructive",
				Phases: []string{
					"phase 1: mark the resource Deprecated() and ship it, so no action reads it",
					"phase 2: drop the table with ks migrate apply --allow-destructive",
				},
				InTx: []string{fmt.Sprintf("DROP TABLE IF EXISTS %s CASCADE;", quoteIdent(o.Table))},
			})
		default:
			p.Steps = append(p.Steps, diffResource(o, n, ref, updated)...)
		}
	}
	return p
}

func frameworkHash(a *model.App) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(strings.Join(FrameworkDDL(a), "\n"))))
}

func indexResources(a *model.App) map[string]model.Resource {
	out := map[string]model.Resource{}
	for _, r := range a.AllResources() {
		out[r.Ref] = *r.Resource
	}
	return out
}

// DDLFor renders the CREATE statements for one resource.
func DDLFor(a *model.App, ref string) []string {
	res := a.FindResource(ref)
	if res == nil {
		return nil
	}
	return resourceDDL(a, ref, *res)
}

func diffResource(o, n model.Resource, ref string, a *model.App) []Step {
	var steps []Step
	if !o.Tenant && n.Tenant {
		steps = append(steps, Step{
			Class: NeedsBackfill, Subject: ref, Detail: "resource becomes tenant scoped",
			InTx: []string{
				fmt.Sprintf("ALTER TABLE %s ADD COLUMN tenant_id uuid;", quoteIdent(n.Table)),
				fmt.Sprintf("UPDATE %s SET tenant_id = ks_backfill_tenant();", quoteIdent(n.Table)),
				fmt.Sprintf("ALTER TABLE %s ALTER COLUMN tenant_id SET NOT NULL;", quoteIdent(n.Table)),
			},
			Backfill:  &Backfill{Table: n.Table, Column: "tenant_id", Batch: 1000, Job: "ks_backfill_tenant_id"},
			NeedsFlag: "--plan",
		})
		steps = append(steps, Step{Class: Safe, Subject: ref, Detail: "enable row-level security",
			InTx: rlsof(n)})
	}
	if o.Tenant && !n.Tenant {
		steps = append(steps, Step{
			Class: Destructive, Subject: ref, Detail: "resource leaves tenant scoping",
			NeedsFlag: "--allow-destructive",
			Phases: []string{
				"1. prove every row belongs to one tenant and copy tenant_id somewhere auditable",
				"2. drop the isolation policy and the column with --allow-destructive",
			},
			InTx: []string{
				fmt.Sprintf("DROP POLICY IF EXISTS ks_tenant_isolation ON %s;", quoteIdent(n.Table)),
				fmt.Sprintf("ALTER TABLE %s DROP COLUMN IF EXISTS tenant_id CASCADE;", quoteIdent(n.Table)),
			},
		})
	}
	if o.Versioned != n.Versioned {
		steps = append(steps, columnChangeStep(n.Table, "version", o.FindField("version"), n.FindField("version")))
	}

	byName := func(fs []model.Field) map[string]model.Field {
		out := map[string]model.Field{}
		for _, f := range fs {
			out[f.Name] = f
		}
		return out
	}
	of, nf := byName(o.Fields), byName(n.Fields)

	names := []string{}
	for k := range nf {
		names = append(names, k)
	}
	for k := range of {
		if _, ok := nf[k]; !ok {
			names = append(names, k)
		}
	}
	sort.Strings(names)

	for _, name := range names {
		// A rename is declared, never guessed (§13.2).
		if of[name].Name != "" && nf[name].Name == "" {
			for nn, f := range nf {
				if f.RenamedFrom == name {
					steps = append(steps, renameStep(n.Table, name, nn))
					delete(of, name)
					delete(nf, nn)
					break
				}
			}
		}
	}
	names = names[:0]
	for k := range nf {
		names = append(names, k)
	}
	for k := range of {
		if _, ok := nf[k]; !ok {
			names = append(names, k)
		}
	}
	sort.Strings(names)

	for _, name := range names {
		ov, had := of[name]
		nv, has := nf[name]
		if has && !had {
			steps = append(steps, addColumnStep(n, nv, ref, a))
			continue
		}
		if had && !has {
			if name == "tenant_id" {
				continue
			}
			steps = append(steps, Step{
				Class: Destructive, Subject: ref + "." + name, Detail: "remove field",
				NeedsFlag: "--allow-destructive",
				Phases: []string{
					"phase 1: mark the field Deprecated() so generated code stops reading it",
					"phase 2: drop the column with ks migrate apply --allow-destructive",
				},
				InTx: dropColumnSQL(n, ov),
			})
			continue
		}
		steps = append(steps, alterColumnSteps(n, ov, nv, ref, a)...)
	}

	steps = append(steps, diffIndexes(o, n, ref, a)...)
	steps = append(steps, diffInvariants(o, n, ref, a)...)
	return keepNonEmpty(steps)
}

func keepNonEmpty(in []Step) []Step {
	var out []Step
	for _, s := range in {
		if s.Class == "" {
			continue
		}
		out = append(out, s)
	}
	return out
}

func addColumnStep(n model.Resource, f model.Field, ref string, a *model.App) Step {
	subject := ref + "." + f.Name
	def := strings.TrimSpace(columnDDL(a, n, f))
	switch {
	case f.Optional:
		return Step{Class: Safe, Subject: subject, Detail: "add optional field",
			InTx: []string{fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s;", quoteIdent(n.Table), def)}}
	case f.Default != nil:
		return Step{Class: Safe, Subject: subject, Detail: "add field with default",
			InTx: []string{fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s;", quoteIdent(n.Table), def)}}
	default:
		// A required column on a populated table cannot be added in one statement:
		// Postgres would rewrite the table. Add nullable, backfill, then set not null
		// behind a validated check so the constraint never blocks writes.
		// The same column, without the NOT NULL that would rewrite the table.
		nullable := strings.Replace(def, " NOT NULL", "", 1)
		return Step{
			Class: NeedsBackfill, Subject: subject, Detail: "add required field without default",
			NeedsFlag: "--plan",
			Phases: []string{
				"1. add the column nullable",
				"2. backfill in batches of 1000 keyed by primary key",
				"3. SET NOT NULL via a validated CHECK",
			},
			InTx: []string{
				fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s;", quoteIdent(n.Table), nullable),
			},
			OutsideTx: []string{
				fmt.Sprintf("ALTER TABLE %s ADD CONSTRAINT %s CHECK (%s IS NOT NULL) NOT VALID;",
					quoteIdent(n.Table), quoteIdent(n.Table+"_"+f.Column+"_not_null"), quoteIdent(firstColumn(f))),
				fmt.Sprintf("ALTER TABLE %s VALIDATE CONSTRAINT %s;",
					quoteIdent(n.Table), quoteIdent(n.Table+"_"+f.Column+"_not_null")),
				// With the validated CHECK in place Postgres sets NOT NULL without a scan,
				// and the helper constraint has done its job.
				fmt.Sprintf("ALTER TABLE %s ALTER COLUMN %s SET NOT NULL;", quoteIdent(n.Table), quoteIdent(firstColumn(f))),
				fmt.Sprintf("ALTER TABLE %s DROP CONSTRAINT %s;", quoteIdent(n.Table), quoteIdent(n.Table+"_"+f.Column+"_not_null")),
			},
			Backfill: &Backfill{Table: n.Table, Column: firstColumn(f), Batch: 1000, Job: "ks_backfill_column"},
		}
	}
}

func firstColumn(f model.Field) string {
	if len(f.Columns) > 0 {
		return f.Columns[0]
	}
	return f.Column
}

func columnChangeStep(table string, name string, old, f *model.Field) Step {
	if f == nil {
		return Step{Class: Destructive, Subject: table + "." + name, Detail: "remove version column", NeedsFlag: "--allow-destructive"}
	}
	if old == nil {
		return Step{Class: Safe, Subject: table + "." + name, Detail: "add optimistic concurrency column",
			InTx: []string{fmt.Sprintf("ALTER TABLE %s ADD COLUMN version bigint NOT NULL DEFAULT 1;", quoteIdent(table))}}
	}
	return Step{}
}

func dropColumnSQL(n model.Resource, f model.Field) []string {
	cols := f.StorageColumns()
	out := make([]string, 0, len(cols))
	for _, c := range cols {
		out = append(out, fmt.Sprintf("ALTER TABLE %s DROP COLUMN IF EXISTS %s CASCADE;", quoteIdent(n.Table), quoteIdent(c)))
	}
	return out
}

func renameStep(table, from, to string) Step {
	return Step{
		Class: NeedsPlan, Subject: table + "." + from + " -> " + to, Detail: "rename field",
		NeedsFlag: "--plan",
		Phases: []string{
			"1. add the new column and dual write from generated code",
			"2. backfill the new column from the old one",
			"3. switch reads, then drop the old column with --allow-destructive",
		},
		InTx: []string{fmt.Sprintf("ALTER TABLE %s RENAME COLUMN %s TO %s;",
			quoteIdent(table), quoteIdent(from), quoteIdent(to))},
	}
}

func alterColumnSteps(n model.Resource, old, f model.Field, ref string, a *model.App) []Step {
	var steps []Step
	subject := ref + "." + f.Name
	t := a.PolicyTable(ref)
	_ = t
	if old.SQLType() != f.SQLType() || !sameMoney(old, f) {
		steps = append(steps, Step{
			Class: NeedsPlan, Subject: subject, Detail: fmt.Sprintf("change type %s -> %s", old.SQLType(), f.SQLType()),
			NeedsFlag: "--plan",
			Phases: []string{
				"1. add a new column with the target type",
				"2. dual write, backfill, then switch reads and indexes",
				"3. drop the old column with --allow-destructive",
			},
			InTx: []string{fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s;",
				quoteIdent(n.Table), quoteIdent(f.Column+"_ksnew"), f.SQLType())},
		})
	}
	if old.Optional && !f.Optional {
		steps = append(steps, Step{
			Class: NeedsBackfill, Subject: subject, Detail: "field becomes required",
			NeedsFlag: "--plan",
			Backfill:  &Backfill{Table: n.Table, Column: firstColumn(f), Set: strings.TrimPrefix(defaultSQL(f), " DEFAULT "), Batch: 1000, Job: "ks_backfill_set_default"},
			Phases: []string{
				"1. backfill nulls to the declared default in batches of 1000",
				"2. SET NOT NULL with a validated CHECK",
			},
			OutsideTx: []string{
				fmt.Sprintf("ALTER TABLE %s ALTER COLUMN %s SET NOT NULL;", quoteIdent(n.Table), quoteIdent(firstColumn(f))),
			},
		})
	}
	if !old.Optional && f.Optional {
		steps = append(steps, Step{Class: Safe, Subject: subject, Detail: "field becomes optional",
			InTx: []string{fmt.Sprintf("ALTER TABLE %s ALTER COLUMN %s DROP NOT NULL;", quoteIdent(n.Table), quoteIdent(firstColumn(f)))}})
	}
	if !sameDefault(old, f) {
		steps = append(steps, Step{Class: Safe, Subject: subject, Detail: "default changed",
			InTx: []string{fmt.Sprintf("ALTER TABLE %s ALTER COLUMN %s SET DEFAULT %s;",
				quoteIdent(n.Table), quoteIdent(firstColumn(f)), defaultExpr(f))}})
	}
	if enumLike(old) && enumLike(f) {
		added := missingValues(old.Type.Values, f.Type.Values)
		removed := missingValues(f.Type.Values, old.Type.Values)
		switch {
		case len(removed) > 0:
			steps = append(steps, Step{
				Class: Destructive, Subject: subject, Detail: "remove enum value " + strings.Join(removed, ","),
				NeedsFlag: "--allow-destructive",
				Phases: []string{
					"1. declare a mapping that rewrites existing rows to an allowed value",
					"2. replace the CHECK constraint once no row holds a removed value",
				},
				InTx: enumCheckSQL(n, f),
			})
		case len(added) > 0:
			steps = append(steps, Step{
				Class: Safe, Subject: subject, Detail: "add enum value " + strings.Join(added, ","),
				InTx: enumCheckSQL(n, f),
			})
		}
	} else if !sameConstraint(old, f) {
		steps = append(steps, Step{Class: NeedsValidation, Subject: subject, Detail: "tighten field constraint",
			InTx: constraintSQL(n, f, a, ref)})
	}
	if old.Immutable != f.Immutable {
		steps = append(steps, Step{Class: Safe, Subject: subject, Detail: fmt.Sprintf("immutable = %v", f.Immutable),
			InTx: immutableTrigger(a, ref, n)})
	}
	if old.Sensitive != f.Sensitive {
		steps = append(steps, Step{Class: NeedsPlan, Subject: subject, Detail: sensitiveNote(old, f),
			Phases: []string{"no schema change: the value is redacted in logs, traces and audit diffs from the next deploy"}})
	}
	if old.Encrypted() != f.Encrypted() {
		steps = append(steps, Step{Class: NeedsPlan, Subject: subject,
			Detail:    fmt.Sprintf("encryption %v -> %v", old.Encrypted(), f.Encrypted()),
			NeedsFlag: "--plan",
			Phases: []string{
				"1. add a bytea column and dual write plaintext plus the encrypted value",
				"2. backfill the encrypted column with the current key",
				"3. switch reads, then drop the plaintext column with --allow-destructive",
			}})
	}
	if !sameRef(old, f) {
		steps = append(steps, Step{Class: Safe, Subject: subject, Detail: "update foreign key",
			InTx: []string{
				fmt.Sprintf("ALTER TABLE %s DROP CONSTRAINT IF EXISTS %s;", quoteIdent(n.Table), quoteIdent(n.Table+"_"+old.Column+"_fkey")),
				fmt.Sprintf("ALTER TABLE %s ADD FOREIGN KEY (%s) REFERENCES %s;",
					quoteIdent(n.Table), quoteIdent(f.Column), refTarget(a, f)),
			}})
	}
	if old.Unique != f.Unique {
		if f.Unique {
			steps = append(steps, Step{Class: NeedsValidation, Subject: subject, Detail: "add uniqueness",
				OutsideTx: []string{fmt.Sprintf("CREATE UNIQUE INDEX CONCURRENTLY %s ON %s (%s);",
					quoteIdent(n.Table+"_"+firstColumn(f)+"_uniq"), quoteIdent(n.Table), quoteIdent(firstColumn(f)))}})
		} else {
			steps = append(steps, Step{Class: Safe, Subject: subject, Detail: "remove uniqueness",
				OutsideTx: []string{fmt.Sprintf("DROP INDEX CONCURRENTLY IF EXISTS %s;", quoteIdent(n.Table+"_"+firstColumn(f)+"_uniq"))}})
		}
	}
	return steps
}

func onDelete(f model.Field) string {
	if f.Ref == nil {
		return "restrict"
	}
	return f.Ref.OnDelete
}

func refTarget(a *model.App, f model.Field) string {
	if f.Ref == nil {
		return "NULL"
	}
	t := a.TableName(f.Ref.Resource)
	if t == "" {
		t = "ks_unknown"
	}
	return quoteIdent(t) + "(id) ON DELETE " + strings.ToUpper(onDelete(f))
}

func enumLike(f model.Field) bool { return f.Type.Kind == model.KindEnum }

func sameEnum(a, b model.Field) bool {
	return a.Type.Kind == model.KindEnum && b.Type.Kind == model.KindEnum &&
		strings.Join(a.Type.Values, ",") != "" && subsetOf(a.Type.Values, b.Type.Values)
}

func subsetOf(a, b []string) bool {
	set := map[string]bool{}
	for _, x := range b {
		set[x] = true
	}
	for _, x := range a {
		if !set[x] {
			return false
		}
	}
	return true
}

func missingValues(from, in []string) []string {
	set := map[string]bool{}
	for _, x := range from {
		set[x] = true
	}
	var out []string
	for _, x := range in {
		if !set[x] {
			out = append(out, x)
		}
	}
	sort.Strings(out)
	return out
}

func enumCheckSQL(n model.Resource, f model.Field) []string {
	vals := make([]string, len(f.Type.Values))
	for i, v := range f.Type.Values {
		vals[i] = quoteSQL(v)
	}
	name := quoteIdent(n.Table + "_" + f.Column + "_check")
	return []string{
		fmt.Sprintf("ALTER TABLE %s DROP CONSTRAINT IF EXISTS %s;", quoteIdent(n.Table), name),
		fmt.Sprintf("ALTER TABLE %s ADD CONSTRAINT %s CHECK (%s IN (%s)) NOT VALID;",
			quoteIdent(n.Table), name, quoteIdent(f.Column), strings.Join(vals, ",")),
		fmt.Sprintf("ALTER TABLE %s VALIDATE CONSTRAINT %s;", quoteIdent(n.Table), name),
	}
}

func constraintSQL(n model.Resource, f model.Field, a *model.App, ref string) []string {
	name := quoteIdent(n.Table + "_" + f.Column + "_check")
	def := columnDDL(a, n, f)
	def = strings.TrimSpace(def)
	// Only the constraint part matters for an existing column.
	i := strings.Index(def, "CHECK")
	if i < 0 {
		return nil
	}
	return []string{
		fmt.Sprintf("ALTER TABLE %s DROP CONSTRAINT IF EXISTS %s;", quoteIdent(n.Table), name),
		fmt.Sprintf("ALTER TABLE %s ADD CONSTRAINT %s %s NOT VALID;", quoteIdent(n.Table), name, def[i:]),
		fmt.Sprintf("ALTER TABLE %s VALIDATE CONSTRAINT %s;", quoteIdent(n.Table), name),
	}
}

func sameDefault(a, b model.Field) bool {
	x, y := "", ""
	if a.Default != nil {
		x = a.Default.Kind + a.Default.Text
	}
	if b.Default != nil {
		y = b.Default.Kind + b.Default.Text
	}
	return x == y
}

func sameMoney(a, b model.Field) bool {
	return a.Type.Kind != model.KindMoney || b.Type.Kind != model.KindMoney ||
		len(a.Columns) == len(b.Columns)
}

func maxLenOf(f model.Field) int {
	if f.Type.MaxLen == nil {
		return -1
	}
	return *f.Type.MaxLen
}

func sameConstraint(a, b model.Field) bool {
	if a.Type.Kind == model.KindEnum || b.Type.Kind == model.KindEnum {
		return strings.Join(a.Type.Values, "|") == strings.Join(b.Type.Values, "|")
	}
	return maxLenOf(a) == maxLenOf(b) && a.Type.Pattern == b.Type.Pattern &&
		a.Type.Min == b.Type.Min && a.Type.Max == b.Type.Max && a.Type.Encrypted == b.Type.Encrypted
}

func sameRef(a, b model.Field) bool {
	x, y := "", ""
	if a.Ref != nil {
		x = a.Ref.Resource + a.Ref.OnDelete
	}
	if b.Ref != nil {
		y = b.Ref.Resource + b.Ref.OnDelete
	}
	return x == y
}

func defaultExpr(f model.Field) string {
	if f.Default == nil {
		if f.Type.Kind == model.KindTimestamp {
			return "now()"
		}
		return "NULL"
	}
	switch f.Default.Kind {
	case "now":
		return "now()"
	case "int", "bool", "decimal":
		return f.Default.Text
	}
	return quoteSQL(f.Default.Text)
}

func sensitiveNote(old, f model.Field) string {
	if f.Sensitive {
		return "field marked Sensitive: values stop appearing in logs, traces and audit diffs"
	}
	return "field no longer Sensitive: values become visible in logs, traces and audit diffs"
}

func diffIndexes(o, n model.Resource, ref string, a *model.App) []Step {
	var steps []Step
	byName := map[string]model.Index{}
	for _, ix := range o.Indexes {
		byName[ix.Name] = ix
	}
	for _, ix := range n.Indexes {
		prev, had := byName[ix.Name]
		if !had {
			steps = append(steps, Step{
				Class: Safe, Subject: ref + ":index " + ix.Name, Detail: "add index",
				OutsideTx: concurrently(indexDDL(a, ref, n), ix.Name),
			})
			continue
		}
		if strings.Join(prev.Fields, ",") != strings.Join(ix.Fields, ",") || prev.Unique != ix.Unique {
			steps = append(steps, Step{
				Class: NeedsPlan, Subject: ref + ":index " + ix.Name, Detail: "index definition changed",
				NeedsFlag: "--plan",
				Phases: []string{
					"1. create the new index concurrently under a temporary name",
					"2. swap the names in one transaction, then drop the old index",
				},
				OutsideTx: concurrently(indexDDL(a, ref, n), ix.Name),
			})
		}
		if prev.Where != nil && ix.Where != nil && policy.Canonical(prev.Where) != policy.Canonical(ix.Where) {
			steps = append(steps, Step{Class: NeedsPlan, Subject: ref + ":index " + ix.Name,
				Detail: "partial index condition changed", NeedsFlag: "--plan"})
		}
	}
	newNames := map[string]bool{}
	for _, ix := range n.Indexes {
		newNames[ix.Name] = true
	}
	for _, ix := range o.Indexes {
		if !newNames[ix.Name] && ix.Name != o.Table+"_pk" {
			steps = append(steps, Step{Class: Safe, Subject: ref + ":index " + ix.Name, Detail: "remove index",
				OutsideTx: []string{fmt.Sprintf("DROP INDEX CONCURRENTLY IF EXISTS %s;", quoteIdent(ix.Name))}})
		}
	}
	return steps
}

// concurrently rewrites a plain CREATE INDEX into the non-blocking form, which is
// the only thing in a migration that cannot run inside a transaction (§13.2).
func concurrently(stmts []string, name string) []string {
	var out []string
	for _, s := range stmts {
		if !strings.Contains(s, quoteIdent(name)) {
			continue
		}
		if strings.HasPrefix(s, "CREATE UNIQUE INDEX") {
			out = append(out, strings.Replace(s, "CREATE UNIQUE INDEX", "CREATE UNIQUE INDEX CONCURRENTLY", 1))
			continue
		}
		if strings.HasPrefix(s, "CREATE INDEX") {
			out = append(out, strings.Replace(s, "CREATE INDEX", "CREATE INDEX CONCURRENTLY", 1))
		}
	}
	return out
}

func diffInvariants(o, n model.Resource, ref string, a *model.App) []Step {
	var steps []Step
	old := map[string]model.Invariant{}
	for _, v := range o.Invariants {
		old[v.Name] = v
	}
	fresh := map[string]model.Invariant{}
	for _, v := range n.Invariants {
		fresh[v.Name] = v
	}
	for _, v := range n.Invariants {
		prev, had := old[v.Name]
		sql := invariantSQL(a, ref, n, v)
		if !had {
			steps = append(steps, Step{Class: NeedsValidation, Subject: ref + "." + v.Name,
				Detail: "add invariant", InTx: sql})
			continue
		}
		if policy.Canonical(prev.Expr) != policy.Canonical(v.Expr) {
			steps = append(steps, Step{Class: NeedsValidation, Subject: ref + "." + v.Name,
				Detail: "invariant expression changed", InTx: sql})
		}
	}
	for _, v := range o.Invariants {
		if _, ok := fresh[v.Name]; !ok {
			steps = append(steps, Step{Class: Safe, Subject: ref + "." + v.Name, Detail: "remove invariant",
				InTx: []string{fmt.Sprintf("ALTER TABLE %s DROP CONSTRAINT IF EXISTS %s;",
					quoteIdent(n.Table), quoteIdent(n.Table+"_"+v.Name))}})
		}
	}
	return steps
}

func invariantSQL(a *model.App, ref string, n model.Resource, v model.Invariant) []string {
	if v.Enforcement != "check" {
		return nil
	}
	sql, err := constraintFor(a, ref, n, v)
	if err != nil || sql == "" {
		return nil
	}
	name := quoteIdent(n.Table + "_" + v.Name)
	return []string{
		fmt.Sprintf("ALTER TABLE %s DROP CONSTRAINT IF EXISTS %s;", quoteIdent(n.Table), name),
		fmt.Sprintf("ALTER TABLE %s ADD CONSTRAINT %s CHECK (%s) NOT VALID;", quoteIdent(n.Table), name, sql),
		fmt.Sprintf("ALTER TABLE %s VALIDATE CONSTRAINT %s;", quoteIdent(n.Table), name),
	}
}

func constraintFor(a *model.App, ref string, n model.Resource, v model.Invariant) (string, error) {
	sql, err := policy.TableSQL(v.Expr, a.PolicyTable(ref), policy.TargetDDL, n.Table)
	if err != nil {
		return "", err
	}
	return sql.Text, nil
}

// NeedsFlagged reports the steps a plain apply must refuse.
func (p Plan) NeedsFlagged() []Step {
	var out []Step
	for _, s := range p.Steps {
		if s.NeedsFlag != "" {
			out = append(out, s)
		}
	}
	return out
}
