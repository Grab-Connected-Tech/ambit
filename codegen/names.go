package codegen

import (
	"strings"

	"keystone.dev/keystone/model"
)

// splitRef divides "billing.Invoice" into its module and name.
func splitRef(ref string) (string, string) {
	for i := 0; i < len(ref); i++ {
		if ref[i] == '.' {
			return ref[:i], ref[i+1:]
		}
	}
	return "", ref
}

// resourceOf finds a resource by its qualified reference.
func resourceOf(app *model.App, ref string) (*model.Module, *model.Resource) {
	module, name := splitRef(ref)
	for m := range app.Modules {
		if app.Modules[m].Name != module {
			continue
		}
		for r := range app.Modules[m].Resources {
			if app.Modules[m].Resources[r].Name == name {
				return &app.Modules[m], &app.Modules[m].Resources[r]
			}
		}
	}
	return nil, nil
}

// viewName is the generated scoped-handle type for a resource: InvoiceView.
func viewName(app *model.App, ref string) string {
	_, name := splitRef(ref)
	return name + "View"
}

// tableVar is the generated storage description: InvoiceTable.
func tableVar(ref string) string {
	_, name := splitRef(ref)
	return name + "Table"
}

// pluralVar is the generated column-descriptor set: Invoices.
func pluralVar(ref string) string {
	_, name := splitRef(ref)
	return plural(name)
}

// fieldGoType is the Go type of an action input or output field.
func fieldGoType(f *model.Field) string {
	base := scalarGoType(f)
	if f.Optional {
		return "ks.Opt[" + base + "]"
	}
	return base
}

func scalarGoType(f *model.Field) string {
	switch f.Type.Kind {
	case model.KindID, model.KindRef:
		return "ks.ID"
	case model.KindInt:
		return "int64"
	case model.KindMoney:
		return "ks.Money"
	case model.KindDecimal:
		return "ks.Decimal"
	case model.KindBool:
		return "bool"
	case model.KindTimestamp:
		return "time.Time"
	case model.KindDate:
		return "ks.Date"
	case model.KindBytes:
		return "[]byte"
	case model.KindJSON:
		return "json.RawMessage"
	default:
		return "string"
	}
}

// bind converts an input value for a SQL parameter.
func bind(expr string, f *model.Field) string {
	switch f.Type.Kind {
	case model.KindID, model.KindRef:
		return expr + ".Bytes()"
	default:
		return expr
	}
}

// ident turns a spec name into an exported Go identifier: "invoice_id" -> "InvoiceID".
func ident(s string) string {
	parts := strings.FieldsFunc(s, func(r rune) bool {
		return r == '_' || r == '-' || r == '.' || r == ' '
	})
	out := ""
	for _, p := range parts {
		if strings.ToLower(p) == "id" {
			out += "ID"
			continue
		}
		out += title(p)
	}
	if out == "" {
		out = "Value"
	}
	return out
}

func lines(b *strings.Builder, indent int, ss ...string) {
	pad := strings.Repeat("\t", indent)
	for _, s := range ss {
		if s == "" {
			b.WriteString("\n")
			continue
		}
		b.WriteString(pad + s + "\n")
	}
}

func pkgName(importPath string) string {
	parts := strings.Split(importPath, "/")
	last := parts[len(parts)-1]
	switch last {
	case "app", "cmd", "internal":
		return last + "pkg"
	default:
		return last
	}
}

// errorCtor names the generated constructor for a declared action error code (§7.4).
func errorCtor(action, code string) string {
	return "Err" + action + ident(code)
}

// wireName is a resource field's JSON key in responses: the declared name, except that a
// reference carries its id under the column (customer_id) and the loaded row under the
// name. Generated structs, OpenAPI and the TypeScript client all use it (§12).
func wireName(f *model.Field) string {
	if f.Type.Kind == model.KindRef {
		return f.Column
	}
	return f.Name
}
