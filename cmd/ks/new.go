package main

import (
	"bytes"
	"flag"
	"fmt"
	"go/format"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var (
	moduleNameRe = regexp.MustCompile(`^[a-z][a-z0-9]*$`)
	actionNameRe = regexp.MustCompile(`^[A-Z][A-Za-z0-9]*$`)
)

// cmdNew scaffolds spec declarations (§19):
//
//	ks new module <name>                      app/<name>/spec.go with an empty spec
//	ks new action <module>.<Action> [--kind]  a declaration that nobody may call yet, and
//	                                          its implementation stub
//
// A new action requires Never(): deny by default, until the author says who may call it.
func cmdNew(args []string) error {
	var kind *string
	fs, _, err := flags("new", args, func(fs *flag.FlagSet) {
		kind = fs.String("kind", "mutation", "action kind: query, mutation or job")
	})
	if err != nil {
		return err
	}
	switch fs.Arg(0) {
	case "module":
		return newModule(fs.Arg(1))
	case "action":
		return newAction(fs.Arg(1), *kind)
	}
	return fmt.Errorf("usage: ks new module <name> | ks new action <module>.<Action> [--kind query|mutation|job]")
}

func newModule(name string) error {
	if !moduleNameRe.MatchString(name) {
		return fmt.Errorf("a module name is lower case letters and digits: %q", name)
	}
	path := filepath.Join("app", name, "spec.go")
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("%s already exists", path)
	}
	appPkg, err := importPath("app")
	if err != nil {
		return err
	}
	src := fmt.Sprintf(`// Package %[1]s is a module. Humans own this spec; actions.go is AI-written (§4).
package %[1]s

import (
	s "keystone.dev/keystone/spec"

	"%[2]s"
)

// Declare the module's resources and actions here, then run ks gen.
var _ = s.Never
var _ = app.Admin
`, name, appPkg)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		return err
	}
	fmt.Println(path)
	return nil
}

func newAction(ref, kind string) error {
	module, name, ok := strings.Cut(ref, ".")
	if !ok || !moduleNameRe.MatchString(module) || !actionNameRe.MatchString(name) {
		return fmt.Errorf("want <module>.<Action>, e.g. billing.VoidInvoice: %q", ref)
	}
	builder := map[string]string{"query": "Query", "mutation": "Mutation", "job": "Job"}[kind]
	if builder == "" {
		return fmt.Errorf("--kind is query, mutation or job, not %q", kind)
	}
	specPath := filepath.Join("app", module, "spec.go")
	spec, err := os.ReadFile(specPath)
	if err != nil {
		return fmt.Errorf("no module %s (ks new module %s first): %w", module, module, err)
	}
	if regexp.MustCompile(`(?m)^var ` + name + `\b`).Match(spec) {
		return fmt.Errorf("%s already declares %s", specPath, name)
	}
	decl := fmt.Sprintf("\n// %[1]s is new: say what it does, what it reads and writes, and who may call it.\nvar %[1]s = s.%[2]s(%[1]q,\n", name, builder)
	if kind != "job" {
		decl += "\t// Nobody may call it until this says who may (deny by default).\n\ts.Requires(s.Never()),\n"
	} else {
		decl += "\ts.RunAs(s.System),\n"
	}
	decl += ")\n"
	if err := os.WriteFile(specPath, append(spec, decl...), 0o644); err != nil {
		return err
	}
	fmt.Println(specPath)
	// With no actions.go yet, ks gen writes one with every stub; otherwise splice this
	// action into the Actions table the entrypoint names.
	actPath := filepath.Join("app", module, "actions.go")
	src, err := os.ReadFile(actPath)
	if err != nil {
		fmt.Println("run ks gen: it writes", actPath)
		return nil
	}
	out, err := spliceAction(src, name)
	if err != nil {
		return fmt.Errorf("%s: %w; add %s to the Actions table by hand", actPath, err, name)
	}
	if err := os.WriteFile(actPath, out, 0o644); err != nil {
		return err
	}
	fmt.Println(actPath)
	fmt.Println("now run ks gen")
	return nil
}

// spliceAction adds an action returning nothing to an Actions table: a field in the
// struct type, its value in the literal, and a stub returning ks.NotImplemented().
func spliceAction(src []byte, name string) ([]byte, error) {
	s := string(src)
	start := strings.Index(s, "var Actions = struct {")
	if start < 0 {
		return nil, fmt.Errorf("no Actions table")
	}
	mid := strings.Index(s[start:], "\n}{")
	if mid < 0 {
		return nil, fmt.Errorf("the Actions table has an unexpected shape")
	}
	mid += start
	end := strings.Index(s[mid+3:], "\n}")
	if end < 0 {
		return nil, fmt.Errorf("the Actions table has an unexpected shape")
	}
	end += mid + 3
	lower := strings.ToLower(name[:1]) + name[1:]
	field := fmt.Sprintf("\n\t%s func(*gen.%sCtx, gen.%sInput) (struct{}, error)", name, name, name)
	value := fmt.Sprintf("\n\t%s: %s,", name, lower)
	stub := fmt.Sprintf("\n// %s is not written yet.\nfunc %s(c *gen.%sCtx, in gen.%sInput) (struct{}, error) {\n\treturn struct{}{}, ks.NotImplemented()\n}\n", lower, lower, name, name)
	s = s[:end] + value + s[end:]
	s = s[:mid] + field + s[mid:]
	s += stub
	if !strings.Contains(s, `"keystone.dev/keystone/ks"`) {
		s = strings.Replace(s, "import (\n", "import (\n\t\"keystone.dev/keystone/ks\"\n", 1)
	}
	formatted, err := format.Source([]byte(s))
	if err != nil {
		return nil, err
	}
	return bytes.TrimLeft(formatted, "\n"), nil
}
