package codegen

import (
	"encoding/json"
	"fmt"
	"sort"

	"keystone.dev/keystone/model"
)

// OpenAPI renders the app's HTTP surface as an OpenAPI 3.1 document (§7.1). It comes from
// the model rather than from handler code, so the contract and the enforcement are the same
// declaration: an action that requires a role documents that requirement.
func OpenAPI(app *model.App) ([]byte, error) {
	paths := map[string]any{}
	schemas := map[string]any{}
	for _, e := range app.AllResources() {
		schemas[e.Ref] = resourceSchema(e.Resource)
	}
	for m := range app.Modules {
		for i := range app.Modules[m].Actions {
			a := &app.Modules[m].Actions[i]
			if a.Kind == model.KindJob || a.Kind == model.KindSchedule {
				continue
			}
			method := "post"
			if a.Kind == model.KindQuery {
				method = "get"
			}
			op := map[string]any{
				"operationId": app.Modules[m].Name + "." + a.Name,
				"summary":     summaryOf(a),
				"responses": map[string]any{
					"200": map[string]any{"description": "success",
						"content": map[string]any{"application/json": map[string]any{
							"schema": outputSchema(app, a)}}},
					"422": map[string]any{"description": "a declared failure"},
				},
			}
			if len(a.Input) > 0 {
				op["parameters"] = parametersOf(a.Input, method == "get")
			}
			if !a.Public && len(a.Errors) > 0 {
				op["description"] = "Declared failures: " + joinCodes(a.Errors)
			}
			if a.Idempotent {
				op["parameters"] = append(toParams(op["parameters"]), map[string]any{
					"name": "Idempotency-Key", "in": "header", "required": true,
					"schema": map[string]any{"type": "string"},
				})
			}
			paths["/api/"+app.Modules[m].Name+"/"+a.Name] = map[string]any{method: op}
		}
	}
	sortMapKeys(paths)
	doc := map[string]any{
		"openapi": "3.1.0",
		"info": map[string]any{
			"title":       app.App.Name,
			"description": fmt.Sprintf("%d resources, %d roles", len(app.AllResources()), len(app.Roles)),
		},
		"components": map[string]any{"schemas": schemas},
		"paths":      paths,
	}
	return json.MarshalIndent(doc, "", "  ")
}

func summaryOf(a *model.Action) string {
	if a.Kind == model.KindQuery {
		return "Reads " + a.Name
	}
	return "Mutates " + a.Name
}

func joinCodes(errs []model.ActionError) string {
	out := ""
	for i, e := range errs {
		if i > 0 {
			out += ", "
		}
		out += e.Code
	}
	return out
}

func toParams(v any) []any {
	if p, ok := v.([]any); ok {
		return p
	}
	return nil
}

func parametersOf(fields []model.Field, asQuery bool) []any {
	var out []any
	for _, f := range fields {
		in := "query"
		if !asQuery {
			in = "body"
		}
		out = append(out, map[string]any{
			"name": f.Name, "in": in, "required": !f.Optional,
			"schema": typeSchema(&f),
		})
	}
	return out
}

func outputSchema(app *model.App, a *model.Action) any {
	switch {
	case a.Output.Resource != "":
		_, n := splitRef(a.Output.Resource)
		return map[string]any{"$ref": "#/components/schemas/" + n}
	case a.Output.Page != "":
		_, n := splitRef(a.Output.Page)
		return map[string]any{"type": "object", "properties": map[string]any{
			"rows":       map[string]any{"type": "array", "items": map[string]any{"$ref": "#/components/schemas/" + n}},
			"nextCursor": map[string]any{"type": "string"},
		}}
	case len(a.Output.Fields) > 0:
		return map[string]any{"type": "object", "properties": fieldProps(a.Output.Fields)}
	default:
		return map[string]any{"type": "object"}
	}
}

func resourceSchema(res *model.Resource) any {
	props := map[string]any{}
	var required []string
	for _, f := range res.Fields {
		props[f.Name] = typeSchema(&f)
		if !f.Optional && f.Name != "id" && f.Name != "version" {
			required = append(required, f.Name)
		}
	}
	sort.Strings(required)
	out := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		out["required"] = required
	}
	return out
}

func fieldProps(fields []model.Field) map[string]any {
	out := map[string]any{}
	for _, f := range fields {
		out[f.Name] = typeSchema(&f)
	}
	return out
}

func typeSchema(f *model.Field) any {
	t := map[string]any{}
	switch f.Type.Kind {
	case model.KindInt:
		t["type"] = "integer"
	case model.KindBool:
		t["type"] = "boolean"
	case model.KindDecimal, model.KindMoney:
		t["type"] = "string"
		t["format"] = string(f.Type.Kind)
	case model.KindTimestamp:
		t["type"] = "string"
		t["format"] = "date-time"
	case model.KindDate:
		t["type"] = "string"
		t["format"] = "date"
	case model.KindJSON:
		t["type"] = "object"
	case model.KindBytes:
		t["type"] = "string"
		t["contentEncoding"] = "base64"
	case model.KindID, model.KindRef:
		t["type"] = "string"
		t["format"] = "uuid"
	case model.KindEmail:
		t["type"] = "string"
		t["format"] = "email"
	case model.KindURL:
		t["type"] = "string"
		t["format"] = "uri"
	case model.KindEnum:
		t["type"] = "string"
		vals := make([]any, len(f.Type.Values))
		for i, v := range f.Type.Values {
			vals[i] = v
		}
		t["enum"] = vals
	default:
		t["type"] = "string"
	}
	if f.Type.MaxLen != nil {
		t["maxLength"] = *f.Type.MaxLen
	}
	if f.Type.MinLen != nil {
		t["minLength"] = *f.Type.MinLen
	}
	if f.Type.Pattern != "" {
		t["pattern"] = f.Type.Pattern
	}
	if f.Sensitive {
		t["x-sensitive"] = true
	}
	return t
}

func sortMapKeys(m map[string]any) {
	// A map serialises in key order already; sorting the nested path objects keeps the
	// document byte-stable across generations, which is what makes regeneration diff-free.
	for _, v := range m {
		if inner, ok := v.(map[string]any); ok {
			sortMapKeys(inner)
		}
	}
}
