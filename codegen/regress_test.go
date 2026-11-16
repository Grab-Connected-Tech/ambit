package codegen

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Grab-Connected-Tech/ambit/model"
)

// Regressions from an agent building an app from scratch: an optional reference that
// generated code which did not compile, cursor and limit on a query with no page, and
// generated files left behind after their module was deleted.
func TestGenerateRegressions(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "example", "myapp", "app.model.json"))
	if err != nil {
		t.Fatal(err)
	}
	var app model.App
	if err := json.Unmarshal(raw, &app); err != nil {
		t.Fatal(err)
	}
	var invoice *model.Resource
	for i := range app.Modules {
		for j := range app.Modules[i].Resources {
			if app.Modules[i].Resources[j].Name == "Invoice" {
				invoice = &app.Modules[i].Resources[j]
			}
		}
	}
	if invoice == nil {
		t.Fatal("example model has no Invoice")
	}
	for _, f := range invoice.Fields {
		if f.Name == "customer" {
			f.Name, f.GoName, f.Column, f.Optional = "approver", "Approver", "approver_id", true
			invoice.Fields = append(invoice.Fields, f)
			break
		}
	}

	dir := t.TempDir()
	stale := filepath.Join(dir, "gen", "ghost.actions.gen.go")
	mine := filepath.Join(dir, "gen", "mine.gen.go") // no generated header: not ours to delete
	if err := os.MkdirAll(filepath.Dir(stale), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stale, []byte(header+"\npackage gen\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(mine, []byte("package gen\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := Generate(Options{Dir: dir, Model: &app, Module: "example.test/app"}); err != nil {
		t.Fatal(err)
	}
	res, err := os.ReadFile(filepath.Join(dir, "gen", "billing.resources.gen.go"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(res), "out.ApproverID = am.Unset") || strings.Contains(string(res), "out.ApproverID = am.Set") {
		t.Error("an optional reference is a plain am.ID; it must not be assigned an am.Opt")
	}
	acts, err := os.ReadFile(filepath.Join(dir, "gen", "billing.actions.gen.go"))
	if err != nil {
		t.Fatal(err)
	}
	countInput := between(string(acts), "type CountDraftsInput struct {", "}")
	if strings.Contains(countInput, "Cursor") {
		t.Errorf("a query with no page must not take a cursor:\n%s", countInput)
	}
	if !strings.Contains(between(string(acts), "type ListInvoicesInput struct {", "}"), "Cursor") {
		t.Error("a paged query must still take a cursor")
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Error("a generated file no module writes any more must be removed")
	}
	if _, err := os.Stat(mine); err != nil {
		t.Error("a file without the generated header must be left alone")
	}
}

func between(s, from, to string) string {
	i := strings.Index(s, from)
	if i < 0 {
		return ""
	}
	s = s[i+len(from):]
	if j := strings.Index(s, to); j >= 0 {
		return s[:j]
	}
	return s
}
