// Package codegen turns an app model into the generated half of an application: the `gen`
// package, the SQL migrations, the OpenAPI document, the TypeScript client and AGENTS.md
// (§7). It is a text generator on purpose — the output has to be readable by the agent
// that is iterating on the app, and by the human reviewing the diff.
//
// Generation is deterministic: the same model produces byte-identical files, so a change
// in generated code always means a change in the model (§7.1).
package codegen

import (
	"bytes"
	"encoding/json"
	"fmt"
	"go/format"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Grab-Connected-Tech/ambit/migrate"
	"github.com/Grab-Connected-Tech/ambit/model"
)

// Options says where the app lives and what to write.
type Options struct {
	// Dir is the application's module root.
	Dir string
	// AppPackage is the import path of the package holding the spec files, e.g.
	// "example/myapp/app". Leave empty to use Model directly.
	AppPackage string
	// SpecDir is the directory of AppPackage, used when loading the model by running Go.
	SpecDir string
	// Module is the target module path; read from go.mod when empty.
	Module string
	// ModulePackages maps a spec module name to its import path, for an app that does
	// not follow the <appdir>/<module> layout.
	ModulePackages map[string]string
	// GenPackage is the import path of the generated package; derived from Module and
	// GoDir when empty.
	GenPackage string
	// Model may be supplied in-process, which is how the framework's own tests generate
	// without spawning a compiler.
	Model *model.App
	// GoDir is where the generated Go package lands, relative to Dir (default "gen").
	GoDir string
	// CmdDir is where the generated entrypoint lands (default "cmd/server").
	CmdDir string
	// SQLDir is where migrations land (default "migrations").
	SQLDir string
	// ClientDir is where the TypeScript client lands (default "client/ts").
	ClientDir string
}

// Result lists what was written, so a CLI can report and a test can assert.
type Result struct {
	Files []string
	Model *model.App
}

// Generate writes every artifact derived from the model.
func Generate(o Options) (*Result, error) {
	app, err := o.resolve()
	if err != nil {
		return nil, err
	}
	goDir := orDefault(o.GoDir, "gen")
	sqlDir := orDefault(o.SQLDir, "migrations")
	clientDir := orDefault(o.ClientDir, "client/ts")

	res := &Result{Model: app}
	write := func(rel string, b []byte, gofmt bool) error {
		if gofmt {
			formatted, err := format.Source(b)
			if err != nil {
				if v := os.Getenv("AM_GEN_DEBUG"); v != "" {
					_ = os.WriteFile(filepath.Join(v, strings.ReplaceAll(rel, "/", "_")), b, 0o644)
				}
				return fmt.Errorf("codegen: generated %s does not parse: %w", rel, err)
			}
			b = formatted
		}
		path := filepath.Join(o.Dir, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		// A file whose content did not change is not rewritten, so regeneration is
		// invisible to git (§7.1). Note the write still happens when the old file cannot be
		// compiled: a two-pass regeneration must be able to replace it.
		if old, err := os.ReadFile(path); err == nil && bytes.Equal(old, b) && gofmt && compiles(path) {
			res.Files = append(res.Files, rel)
			return nil
		}
		if err := os.WriteFile(path, b, 0o644); err != nil {
			return err
		}
		res.Files = append(res.Files, rel)
		return nil
	}

	// The committed model is the input to am diff, which compares revisions without
	// building old code (§6.1).
	raw, err := json.MarshalIndent(app, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := write("app.model.json", append(raw, '\n'), false); err != nil {
		return nil, err
	}

	// The app-level resources (the tenant and the account) are generated too: a module's
	// reference field may point at one of them, and both sides of a reference need a type.
	appResources := appLevelResources(app)
	var ab strings.Builder
	if err := writeGoApp(&ab, app, o); err != nil {
		return nil, err
	}
	ab.WriteString(amMustIDHelper)
	if err := write(filepath.Join(goDir, "app.gen.go"), []byte(ab.String()), true); err != nil {
		return nil, err
	}

	if len(appResources) > 0 {
		var rb strings.Builder
		if err := writeGoResources(&rb, app, "", appResources); err != nil {
			return nil, err
		}
		if err := write(filepath.Join(goDir, "app.resources.gen.go"), []byte(rb.String()), true); err != nil {
			return nil, err
		}
	}

	// Every resource needs the same idiom: a positionally-read uuid becomes an am.ID.
	helper := "\n// amMustID maps a uuid column onto an id. Generated scanners use it because the driver\n" +
		"// hands over bytes, and an id is a string type in Go.\n" +
		"func amMustID(v any) am.ID {\n" +
		"\tid, err := am.ScanID(v)\n" +
		"\tif err != nil {\n" +
		"\t\tpanic(\"gen: a uuid column did not scan: \" + err.Error())\n" +
		"\t}\n" +
		"\treturn id\n}\n\n"
	_ = helper
	for _, m := range app.Modules {
		resPath := filepath.Join(goDir, strings.ToLower(m.Name)+".resources.gen.go")
		if len(m.Resources) == 0 {
			// A module of actions only has no resources file; drop one left from before.
			_ = os.Remove(filepath.Join(o.Dir, resPath))
		} else {
			var rb strings.Builder
			if err := writeGoResources(&rb, app, m.Name, m.Resources); err != nil {
				return nil, err
			}
			if err := write(resPath, []byte(rb.String()), true); err != nil {
				return nil, err
			}
		}
		if len(m.Actions) == 0 {
			continue
		}
		var gb strings.Builder
		if err := writeGoActions(&gb, app, m.Name, m.Actions); err != nil {
			return nil, err
		}
		if err := write(filepath.Join(goDir, strings.ToLower(m.Name)+".actions.gen.go"), []byte(gb.String()), true); err != nil {
			return nil, err
		}
	}

	// The entrypoint is generated too: it is the file that ties an action function to
	// its contract, and there is nothing app-specific for a human to maintain (§7.2).
	if o.Module == "" {
		o.Module = readModulePath(o.Dir)
	}
	if o.Module == "" && o.AppPackage != "" {
		// An app that lives inside another module (this repository's example, for
		// instance) has no go.mod of its own: take the module from the import path.
		o.Module = strings.TrimSuffix(o.AppPackage, "/"+filepath.Base(o.AppPackage))
		if o.Module == o.AppPackage {
			o.Module = ""
		}
		// Prefer the longest prefix that resolves to a directory on disk.
		for i := len(o.AppPackage); i > 0; i-- {
			if o.AppPackage[i-1] != '/' {
				continue
			}
			candidate := o.AppPackage[:i-1]
			if _, err := os.Stat(filepath.Join(candidate)); err == nil {
				o.Module = candidate
				break
			}
		}
	}

	var mb strings.Builder
	if err := writeGoMain(&mb, app, o); err != nil {
		return nil, err
	}
	if err := write(filepath.Join(orDefault(o.CmdDir, "cmd/server"), "main.go"), []byte(mb.String()), true); err != nil {
		return nil, err
	}

	if err := write(filepath.Join(goDir, "rules_test.go"), rulesTest(app), true); err != nil {
		return nil, err
	}
	if err := write(filepath.Join(goDir, "bench_test.go"), benchTest(app, o), true); err != nil {
		return nil, err
	}

	// The migrations directory is a package that embeds its plan files, so the app
	// binary can apply them (§18.3).
	if err := write(filepath.Join(sqlDir, "migrations.go"), []byte(header+"\n// Package migrations embeds the planned migrations of this app.\npackage migrations\n\nimport \"embed\"\n\n// FS holds the plan files am migrate plan wrote; the binary applies them.\n//\n//go:embed *\nvar FS embed.FS\n"), true); err != nil {
		return nil, err
	}
	if err := write("Dockerfile", dockerfile(o), false); err != nil {
		return nil, err
	}

	stubs, err := writeActionStubs(app, o)
	if err != nil {
		return nil, err
	}
	res.Files = append(res.Files, stubs...)

	ddl := migrate.DDL(app)
	// One file per generated schema pass: the DDL is the model's own answer to "what does
	// the database look like", and am migrate applies it inside a single transaction.
	var sb strings.Builder
	sb.WriteString("-- generated from the app model by am gen; do not edit\n\n")
	for _, stmt := range ddl {
		sb.WriteString(strings.TrimSuffix(strings.TrimSpace(stmt), ";") + ";\n\n")
	}
	if err := write(filepath.Join(sqlDir, "schema.gen.sql"), []byte(sb.String()), false); err != nil {
		return nil, err
	}

	spec, err := OpenAPI(app)
	if err != nil {
		return nil, err
	}
	if err := write(filepath.Join(goDir, "openapi.json"), spec, false); err != nil {
		return nil, err
	}
	if err := write(filepath.Join(clientDir, "index.ts"), TypeScriptClient(app), false); err != nil {
		return nil, err
	}
	preserved, _ := teamNotes(filepath.Join(o.Dir, "AGENTS.md"))
	if err := write("AGENTS.md", AgentsMarkdown(app, preserved), false); err != nil {
		return nil, err
	}
	sort.Strings(res.Files)
	return res, nil
}

const header = "// Code generated by am. DO NOT EDIT.\n"

// genImport is the import path the entrypoint uses for the generated package.
func (o Options) genImport() string {
	if o.GenPackage != "" {
		return o.GenPackage
	}
	mod := o.Module
	if mod == "" {
		mod = readModulePath(o.Dir)
	}
	if mod == "" {
		// Without a module path the import cannot be resolved; say so instead of
		// emitting an empty import, which is a file that will not parse.
		return ""
	}
	return mod + "/" + strings.TrimSuffix(orDefault(o.GoDir, "gen"), "/")
}

// readModulePath returns the module line of a go.mod, which is how generation finds the
// import paths of the packages it is writing into.
func readModulePath(dir string) string {
	raw, err := os.ReadFile(filepath.Join(dir, "go.mod"))
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if f := strings.Fields(line); len(f) == 2 && f[0] == "module" {
			return f[1]
		}
	}
	return ""
}

// LoadModel reads the app model from its spec without writing anything.
func LoadModel(o Options) (*model.App, error) { return o.resolve() }

func (o Options) resolve() (*model.App, error) {
	if o.Model != nil {
		return o.Model, nil
	}
	if o.Dir == "" {
		return nil, fmt.Errorf("codegen: either Model or Dir must be set")
	}
	return loadModelByRunning(o, o.Dir)
}

// loadModelByRunning executes the app's declarations to obtain the model. It has to be a
// real program run: a spec is Go, and there is no way to interpret Go without compiling it
// (§4.1). Every package in the module that imports the spec DSL is blank-imported, so a
// module directory nobody remembered to reference still contributes its resources.
func loadModelByRunning(o Options, dir string) (*model.App, error) {
	// -e: a package that does not compile yet (the generated entrypoint, before the
	// action functions exist) still reports its path, and is filtered out below.
	cmd := exec.Command("go", "list", "-e", "-deps", "-f", "{{.ImportPath}}", "./...")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("codegen: go list ./...: %w\n%s", err, out)
	}
	if o.Module == "" {
		o.Module = readModulePath(dir)
	}
	tmp, err := os.MkdirTemp("", "ammodel")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)
	paths := strings.Fields(string(out))
	var spec []string
	// A file that imports the generated package would ask a half-written file to compile,
	// but the spec beside it in the same package is still needed: the model is read with
	// those files replaced by an empty file of the same package (go build -overlay).
	overlay := map[string]string{}
	for _, p := range paths {
		if err := stubGenFiles(dir, p, tmp, overlay); err != nil {
			return nil, err
		}
		if !specImports(dir, p) {
			continue
		}
		spec = append(spec, p)
	}
	if len(spec) == 0 {
		return nil, fmt.Errorf("codegen: no package under %s imports the spec DSL, so there is nothing to generate", dir)
	}
	sort.Strings(spec)
	// The generated package is being rewritten, so its current contents may still name
	// the previous runtime's API. Build the model from the spec packages only: importing
	// gen here would ask a half-regenerated file to compile.
	// The generated package's import path, computed the same way a file's path is: the
	// module plus the output directory.
	genPath := filepath.ToSlash(filepath.Join(o.Module, orDefault(o.GoDir, "gen")))
	var load []string
	for _, p := range spec {
		if p == genPath {
			continue
		}
		load = append(load, p)
	}
	spec = load
	var b strings.Builder
	b.WriteString(header)
	b.WriteString("\npackage main\n\nimport (\n\t\"fmt\"\n\t\"os\"\n\n")
	for _, p := range spec {
		b.WriteString("\t_ \"" + p + "\"\n")
	}
	b.WriteString("\n\t\"github.com/Grab-Connected-Tech/ambit/spec\"\n)\n\nfunc main() {\n" +
		"\tapp, err := spec.App()\n" +
		"\tif err != nil {\n\t\tfmt.Fprintln(os.Stderr, \"spec:\", err)\n\t\tos.Exit(1)\n\t}\n" +
		"\traw := app.Bytes()\n" +
		"\tos.Stdout.Write(raw)\n}\n")
	path := filepath.Join(tmp, "main.go")
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		return nil, err
	}
	rawOverlay, err := json.Marshal(map[string]any{"Replace": overlay})
	if err != nil {
		return nil, err
	}
	overlayPath := filepath.Join(tmp, "overlay.json")
	if err := os.WriteFile(overlayPath, rawOverlay, 0o644); err != nil {
		return nil, err
	}
	run := exec.Command("go", "run", "-overlay", overlayPath, path)
	run.Dir = dir
	var stdout, stderr bytes.Buffer
	run.Stdout = &stdout
	run.Stderr = &stderr
	cmd = run
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("codegen: reading the app model failed: %w\n%s", err, stderr.String())
	}
	app := &model.App{}
	if err := json.Unmarshal(stdout.Bytes(), app); err != nil {
		return nil, fmt.Errorf("codegen: the spec printed something that is not an app model: %w", err)
	}
	return app, nil
}

// specImports reports whether a package imports the spec DSL, directly or through another
// package in the same module. A module's app.go does; a hand-written action file that only
// imports gen does not need to.
func specImports(dir, importPath string) bool {
	cmd := exec.Command("go", "list", "-e", "-f", "{{.Name}} {{join .Deps \" \"}}", importPath)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return false
	}
	line := strings.TrimSpace(string(out))
	// A command is not an importable package, and the app's generated entrypoint is a
	// program whose dependency closure includes the spec anyway.
	name, _, _ := strings.Cut(line, " ")
	if name == "main" {
		return false
	}
	return strings.Contains(line, "github.com/Grab-Connected-Tech/ambit/spec")
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// appLevelResources are the resources declared outside a module: the tenant table and the
// account table.
func appLevelResources(app *model.App) []model.Resource {
	var out []model.Resource
	add := func(name string, t *model.Tenant) {
		if t == nil || t.Table == "" {
			return
		}
		// Only the fields the spec declares: BuildTable adds the framework columns, and a
		// generated struct must not name one of them twice.
		var fields []model.Field
		for _, f := range t.Fields {
			switch f.Name {
			case "id", "tenant_id", "created_at", "updated_at", "version":
				continue
			}
			fields = append(fields, f)
		}
		out = append(out, model.Resource{
			Name: name, Table: t.Table, Tenant: true,
			Fields: fields, Timestamps: true, Versioned: true,
			Access: t.AccessOrNone(),
		})
	}
	add(app.App.Tenant, app.Tenant)
	if app.Account != nil {
		add(app.Account.Name, app.Account)
	}
	return out
}

// moduleOfImport finds the module path that owns an import path, by walking the
// filesystem: <dir>/go.mod declares the module whose packages live under <dir>. The
// caller passes the directory that holds the app, so the search is one stat.
func moduleOfImport(importPath string) string {
	// The longest prefix that is a module path is the one whose remaining part exists as
	// a directory relative to the module root; without a filesystem walk of GOPATH-style
	// layouts, the rule that works is: the module path is the import path minus the
	// package directories, discovered from the deepest directory that has a go.mod.
	for i := len(importPath) - 1; i > 0; i-- {
		if importPath[i] != '/' {
			continue
		}
		root, rest := importPath[:i], importPath[i+1:]
		if hasGoMod(root) {
			return root
		}
		_ = rest
	}
	return ""
}

// hasGoMod reports whether a module root directory of that import path exists in this
// checkout: github.com/Grab-Connected-Tech/ambit/example/myapp -> ./example/myapp.
func hasGoMod(modulePath string) bool {
	if !strings.HasPrefix(modulePath, "github.com/Grab-Connected-Tech/ambit") {
		return false
	}
	rel := strings.TrimPrefix(modulePath, "github.com/Grab-Connected-Tech/ambit")
	rel = strings.TrimPrefix(rel, "/")
	if rel == "" {
		_, err := os.Stat("go.mod")
		return err == nil
	}
	_, err := os.Stat(filepath.Join(rel, "go.mod"))
	return err == nil
}

// amMustIDHelper is appended to each resource file: one definition per file keeps a
// generated file readable on its own, and gofmt does not mind a repeated helper.
const amMustIDHelper = `
// amMustID maps a uuid column onto an id: the driver hands over bytes, and am.ID is a
// string type in Go.
func amMustID(v any) am.ID {
	id, err := am.ScanID(v)
	if err != nil {
		panic("gen: a uuid column did not scan: " + err.Error())
	}
	return id
}
`

// selfPackage is the generated package's import path, which the model loader must not
// import: it is the thing being regenerated.
func (o Options) selfPackage(dir string) string {
	if o.GenPackage != "" {
		return o.GenPackage
	}
	return strings.TrimSuffix(o.genImport(), "/")
}

func (o Options) noGen() {}

// stubGenFiles records, in overlay, an empty stand-in for every file of a package that
// imports a generated package, so the rest of the package compiles without it.
func stubGenFiles(dir, importPath, tmp string, overlay map[string]string) error {
	cmd := exec.Command("go", "list", "-e", "-f", "{{.Dir}}\n{{.Name}}\n{{join .GoFiles \"\\n\"}}", importPath)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return nil // a package go list cannot describe is not a spec package either
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) < 3 {
		return nil
	}
	pkgDir, name := lines[0], lines[1]
	for _, f := range lines[2:] {
		path := filepath.Join(pkgDir, f)
		parsed, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			continue
		}
		for _, imp := range parsed.Imports {
			if strings.HasSuffix(strings.Trim(imp.Path.Value, `"`), "/gen") {
				stub := filepath.Join(tmp, fmt.Sprintf("stub%d.go", len(overlay)))
				if err := os.WriteFile(stub, []byte("package "+name+"\n"), 0o644); err != nil {
					return err
				}
				overlay[path] = stub
				break
			}
		}
	}
	return nil
}

// compiles reports whether a written file parses. Generation is two passes over the same
// package — first the app model, then the files that must build against the new runtime —
// so a stale file that no longer compiles is rewritten rather than skipped.
func compiles(path string) bool {
	raw, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	return goSplits(raw)
}

func goSplits(raw []byte) bool {
	_, err := format.Source(raw)
	return err == nil
}
