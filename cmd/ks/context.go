package main

import (
	"bytes"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"keystone.dev/keystone/model"
)

const conventions = `// Conventions (see AGENTS.md):
// - implement actions in the module's Actions struct; signatures are fixed by gen
// - reach data only through c.DB, effects only through the context, time only through c.Clock
// - return declared failures with the generated Err<Action><Code>() constructors
// - change behaviour in spec.go, then run ks gen, ks check --json and ks test --json
`

// cmdContext prints a compact, deterministic bundle for one module or action (§17.2):
// the module's spec, then the generated declarations it is written against, as
// signatures only. Generated implementation code is never included.
func cmdContext(args []string) error {
	var action *string
	fs, asJSON, err := flags("context", args, func(fs *flag.FlagSet) {
		action = fs.String("action", "", "narrow to one action, e.g. billing.SendInvoice")
	})
	if err != nil {
		return err
	}
	module, only := fs.Arg(0), ""
	if *action != "" {
		module, only = model.SplitRef(*action)
		if only == "" {
			return fmt.Errorf("--action takes module.Action")
		}
	}
	if module == "" {
		return fmt.Errorf("usage: ks context <module> [--action module.Action]")
	}
	spec, err := os.ReadFile(filepath.Join("app", module, "spec.go"))
	if err != nil {
		return fmt.Errorf("no module %s: %w", module, err)
	}
	var sigs bytes.Buffer
	for _, kind := range []string{"actions", "resources"} {
		path := filepath.Join("gen", module+"."+kind+".gen.go")
		if _, err := os.Stat(path); err != nil {
			continue
		}
		if err := signatures(&sigs, path, only); err != nil {
			return err
		}
	}
	if asJSON {
		printJSON(map[string]string{"module": module, "action": only, "spec": string(spec),
			"generated": sigs.String(), "conventions": conventions})
		return nil
	}
	fmt.Printf("// ===== app/%s/spec.go\n%s\n// ===== generated signatures (gen package)\n%s\n%s", module, spec, sigs.String(), conventions)
	return nil
}

// signatures writes a file's exported types and function signatures, without bodies.
// With only set, it keeps the declarations named for that action.
func signatures(w *bytes.Buffer, path, only string) error {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
	if err != nil {
		return err
	}
	keep := func(name string) bool {
		if !ast.IsExported(name) {
			return false
		}
		return only == "" || strings.HasPrefix(name, only) || strings.HasPrefix(name, "Err"+only)
	}
	type decl struct {
		name string
		node ast.Node
	}
	var out []decl
	for _, d := range f.Decls {
		switch d := d.(type) {
		case *ast.FuncDecl:
			name := d.Name.Name
			if d.Recv != nil && len(d.Recv.List) == 1 {
				// A method belongs to its receiver type: keep it with that type.
				t := d.Recv.List[0].Type
				if s, ok := t.(*ast.StarExpr); ok {
					t = s.X
				}
				if id, ok := t.(*ast.Ident); ok {
					name = id.Name
				}
				if !ast.IsExported(d.Name.Name) {
					continue
				}
			}
			if !keep(name) {
				continue
			}
			d.Body = nil
			out = append(out, decl{name, d})
		case *ast.GenDecl:
			if d.Tok != token.TYPE {
				continue
			}
			for _, s := range d.Specs {
				ts := s.(*ast.TypeSpec)
				if keep(ts.Name.Name) {
					out = append(out, decl{ts.Name.Name, &ast.GenDecl{Tok: token.TYPE, Specs: []ast.Spec{ts}}})
				}
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].name < out[j].name })
	cfg := printer.Config{Mode: printer.UseSpaces | printer.TabIndent, Tabwidth: 8}
	for _, d := range out {
		if err := cfg.Fprint(w, fset, d.node); err != nil {
			return err
		}
		w.WriteString("\n\n")
	}
	return nil
}
