package model

import (
	"strings"

	"github.com/Grab-Connected-Tech/ambit/policy"
)

// PolicyTable projects a resource into the small schema the SQL compiler needs.
// It is the only bridge between the model and policy, which keeps policy free of
// model types and lets both be tested alone.
func (a *App) PolicyTable(ref string) *policy.Table {
	res := a.FindResource(ref)
	if res == nil {
		if m, _ := SplitRef(ref); m == "" && ref == a.App.Tenant {
			res = a.FindResource(ref)
		}
	}
	if res == nil {
		return nil
	}
	t := &policy.Table{
		Name: res.Name, Table: res.Table, Tenant: res.Tenant, IDColumn: "id",
	}
	if m, _ := SplitRef(ref); m != "" {
		t.Module = m
	}
	for _, f := range res.Fields {
		pf := policy.Field{
			Name: f.Name, Column: f.Column, Columns: declaredColumns(&f), Kind: string(f.Type.Kind),
		}
		if f.Ref != nil && f.Type.Kind != KindID {
			pf.Kind = string(KindRef)
			pf.RefTo = a.PolicyTable(f.Ref.Resource)
		}
		t.Fields = append(t.Fields, pf)
	}
	return t
}

// PolicyTables projects every resource, including the app-level ones.
func (a *App) PolicyTables() map[string]*policy.Table {
	out := map[string]*policy.Table{}
	for _, r := range a.AllResources() {
		if t := a.PolicyTable(r.Ref); t != nil {
			out[r.Ref] = t
		}
	}
	for _, fw := range a.Framework() {
		if t := out[fw.Name]; t == nil {
			if res := a.FindResource(fw.Name); res != nil {
				t = &policy.Table{Name: res.Name, Table: res.Table, IDColumn: "id"}
				for _, f := range res.Fields {
					t.Fields = append(t.Fields, policy.Field{Name: f.Name, Column: f.Column, Kind: string(f.Type.Kind)})
				}
				out[fw.Name] = t
			}
		}
	}
	return out
}

// Encrypted reports whether the value is stored as ciphertext.
func (f Field) Encrypted() bool { return f.Type.Encrypted }

// Columns returns the storage columns for a field, expanding money into its pair.
func (f Field) StorageColumns() []string {
	if len(f.Columns) > 0 {
		return f.Columns
	}
	return []string{f.Column}
}

// SQLType returns the Postgres type for a field.
func (f Field) SQLType() string {
	t := f.Type
	switch t.Kind {
	case KindID:
		return "uuid"
	case KindText, KindEnum, KindEmail, KindURL:
		return "text"
	case KindInt:
		return "bigint"
	case KindDecimal:
		p, s := t.Precision, t.Scale
		if p == 0 {
			p, s = 18, 4
		}
		return "numeric(" + itoa(p) + "," + itoa(s) + ")"
	case KindMoney:
		return "bigint"
	case KindBool:
		return "boolean"
	case KindTimestamp:
		return "timestamptz"
	case KindDate:
		return "date"
	case KindJSON:
		return "jsonb"
	case KindBytes:
		return "bytea"
	case KindRef:
		return "uuid"
	}
	return "text"
}

// SQLTypes lists the columns with their types, in declaration order.
func (f Field) SQLTypes() []Column {
	if f.Type.Kind == KindMoney {
		return []Column{
			{Name: f.Columns[0], SQL: "bigint", Field: f},
			{Name: f.Columns[1], SQL: "text", Field: f},
		}
	}
	if f.Type.Encrypted {
		return []Column{{Name: f.Column, SQL: "bytea", Field: f}}
	}
	if f.Type.Kind == KindRef || f.Type.Kind == KindID {
		return []Column{{Name: f.Column, SQL: "uuid", Field: f}}
	}
	return []Column{{Name: f.Column, SQL: f.SQLType(), Field: f}}
}

// Column is one SQL column paired with the field it came from.
type Column struct {
	Name  string
	SQL   string
	Field Field
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// declaredColumns names the physical columns a model field occupies. A money field is
// stored as a minor-units column and a currency column; the spec may spell the field's
// column either way, and both the projection and the compiled rules must derive the same
// pair from it (§5.2).
func declaredColumns(f *Field) []string {
	switch {
	case len(f.Columns) > 0:
		return f.Columns
	case f.Type.Kind == KindMoney:
		base := strings.TrimSuffix(f.Column, "_minor")
		return []string{base + "_minor", base + "_currency"}
	default:
		return nil
	}
}
