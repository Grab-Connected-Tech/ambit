// Command ks is the Keystone CLI (§19). Every command accepts --json and exits non-zero
// on failure. It runs in the app's root directory: the one holding app/, gen/ and
// app.model.json.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"keystone.dev/keystone/checker"
	"keystone.dev/keystone/codegen"
	"keystone.dev/keystone/compile"
	"keystone.dev/keystone/diff"
	"keystone.dev/keystone/model"
)

const usage = `usage: ks <command> [flags]

  init <name>                       create a new app
  gen                               compile the spec and regenerate code
  check [--strict]                  compile, vet and analyze
  test                              run generated and written tests
  diff --base <rev> [--format f]    risk-ranked behavior diff (text|markdown|json)
  dev [--addr :8080]                run with reload and introspection
  context <module> [--action m.A]   print a compact context bundle
  explain [<rule>]                  explain a checker rule
  migrate plan|verify|apply|status  plan and apply migrations
  client ts --out <dir>             write the TypeScript client elsewhere
  build [--out bin/app]             produce the production binary
  keys new|rotate                   manage the keys of Encrypted() fields

Every command accepts --json.
`

// exitError carries an exit status without printing anything more: the command already
// reported what went wrong.
type exitError struct{ code int }

func (e exitError) Error() string { return fmt.Sprintf("exit %d", e.code) }

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	cmds := map[string]func([]string) error{
		"init": cmdInit, "gen": cmdGen, "check": cmdCheck, "test": cmdTest, "diff": cmdDiff,
		"dev": cmdDev, "context": cmdContext, "explain": cmdExplain, "migrate": cmdMigrate,
		"client": cmdClient, "build": cmdBuild, "keys": cmdKeys,
	}
	run, ok := cmds[os.Args[1]]
	if !ok {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err := run(os.Args[2:]); err != nil {
		var ee exitError
		if errors.As(err, &ee) {
			os.Exit(ee.code)
		}
		fmt.Fprintln(os.Stderr, "ks:", err)
		os.Exit(1)
	}
}

// flags parses a command's flags; positional arguments may come before or after them.
func flags(name string, args []string, define func(*flag.FlagSet)) (*flag.FlagSet, bool, error) {
	fs := flag.NewFlagSet("ks "+name, flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "machine-readable output")
	if define != nil {
		define(fs)
	}
	var pos []string
	for len(args) > 0 {
		if err := fs.Parse(args); err != nil {
			return nil, false, err
		}
		args = fs.Args()
		if len(args) > 0 {
			pos = append(pos, args[0])
			args = args[1:]
		}
	}
	_ = fs.Parse(pos) // leaves pos as fs.Args()
	return fs, *asJSON, nil
}

func printJSON(v any) {
	raw, _ := json.MarshalIndent(v, "", "  ")
	fmt.Println(string(raw))
}

// appOptions describes the app in the working directory to codegen.
func appOptions() (codegen.Options, error) {
	path, err := importPath(".")
	if err != nil {
		return codegen.Options{}, err
	}
	return codegen.Options{Dir: ".", AppPackage: path + "/app", Module: path}, nil
}

// importPath is the Go import path of a directory, which need not hold Go files itself.
func importPath(dir string) (string, error) {
	out, err := exec.Command("go", "list", "-m", "-f", "{{.Path}}\n{{.Dir}}").Output()
	if err != nil {
		return "", fmt.Errorf("not inside a Go module: %w", err)
	}
	lines := strings.SplitN(strings.TrimSpace(string(out)), "\n", 2)
	if len(lines) != 2 {
		return "", fmt.Errorf("go list -m: unexpected output %q", out)
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	modDir, _ := filepath.EvalSymlinks(lines[1])
	abs, _ = filepath.EvalSymlinks(abs)
	rel, err := filepath.Rel(modDir, abs)
	if err != nil || strings.HasPrefix(rel, "..") {
		return "", fmt.Errorf("%s is outside module %s", abs, lines[0])
	}
	if rel == "." {
		return lines[0], nil
	}
	return lines[0] + "/" + filepath.ToSlash(rel), nil
}

// loadModel compiles the spec into a validated model.
func loadModel() (*model.App, compile.Diags, error) {
	o, err := appOptions()
	if err != nil {
		return nil, nil, err
	}
	app, err := codegen.LoadModel(o)
	if err != nil {
		return nil, nil, err
	}
	return app, compile.Validate(app), nil
}

// report prints diagnostics and turns errors (or warnings under --strict) into a failure.
func report(d compile.Diags, asJSON, strict bool) error {
	sort.Stable(d)
	if asJSON {
		if d == nil {
			d = compile.Diags{}
		}
		printJSON(d)
	} else {
		for _, x := range d {
			loc := x.File
			if loc == "" {
				loc = x.Decl
			}
			if x.Line > 0 {
				loc = fmt.Sprintf("%s:%d:%d", loc, x.Line, x.Column)
			}
			fmt.Printf("%s: %s %s: %s\n", loc, x.Severity, x.Rule, x.Message)
			if x.Fix != "" {
				fmt.Printf("    fix: %s\n", x.Fix)
			}
		}
	}
	for _, x := range d {
		if x.Severity == compile.SevError || strict {
			return exitError{1}
		}
	}
	return nil
}

func hasErrors(d compile.Diags) bool {
	for _, x := range d {
		if x.Severity == compile.SevError {
			return true
		}
	}
	return false
}

func cmdGen(args []string) error {
	_, asJSON, err := flags("gen", args, nil)
	if err != nil {
		return err
	}
	app, diags, err := loadModel()
	if err != nil {
		return err
	}
	// An invalid model is never generated: the runtime relies on what Validate promises.
	if hasErrors(diags) {
		return report(diags, asJSON, false)
	}
	o, _ := appOptions()
	o.Model = app
	res, err := codegen.Generate(o)
	if err != nil {
		return err
	}
	if asJSON {
		printJSON(map[string]any{"files": res.Files, "modelHash": app.Hash(), "diagnostics": diags})
		return nil
	}
	for _, f := range res.Files {
		fmt.Println(f)
	}
	return report(diags, false, false)
}

func cmdCheck(args []string) error {
	var strict *bool
	_, asJSON, err := flags("check", args, func(fs *flag.FlagSet) {
		strict = fs.Bool("strict", false, "treat warnings as errors")
	})
	if err != nil {
		return err
	}
	var d compile.Diags
	// The compiler first: nothing else is meaningful about code that does not build.
	if out, err := exec.Command("go", "build", "./...").CombinedOutput(); err != nil {
		d = append(d, toolDiags("GO-BUILD", out)...)
		return report(d, asJSON, *strict)
	}
	if out, err := exec.Command("go", "vet", "./...").CombinedOutput(); err != nil {
		d = append(d, toolDiags("GO-VET", out)...)
	}
	app, md, err := loadModel()
	if err != nil {
		return err
	}
	d = append(d, md...)
	// Generated code must come from the current spec (KS-CODE-001).
	if raw, err := os.ReadFile("app.model.json"); err != nil {
		d = append(d, stale("app.model.json is missing"))
	} else if committed, err := model.Load(raw); err != nil || committed.Hash() != app.Hash() {
		d = append(d, stale("the spec changed since generated code was written"))
	}
	path, _ := importPath(".")
	gomod, _ := os.ReadFile(goModPath())
	res, err := checker.Run(checker.Config{Root: ".", ModulePath: path, GoMod: string(gomod), App: app, Strict: *strict})
	if err != nil {
		return err
	}
	d = append(d, res.Diags...)
	// Facts sit beside the model so ks diff can compare code risk across revisions (§6.3).
	res.Facts.ModelHash = app.Hash()
	res.Facts.Sorted()
	facts, _ := json.MarshalIndent(res.Facts, "", "  ")
	if err := writeIfChanged("app.facts.json", append(facts, '\n')); err != nil {
		return err
	}
	if _, err := exec.LookPath("govulncheck"); err == nil {
		if out, err := exec.Command("govulncheck", "./...").CombinedOutput(); err != nil {
			d = append(d, toolDiags("GO-VULN", out)...)
		}
	}
	return report(d, asJSON, *strict)
}

func stale(why string) compile.Diag {
	return compile.Diag{Rule: "KS-CODE-001", Severity: compile.SevError, File: "app.model.json",
		Message: why, Fix: "run ks gen", Docs: "ks explain KS-CODE-001"}
}

// toolDiags turns a Go tool's "file:line:col: message" output into diagnostics.
func toolDiags(rule string, out []byte) compile.Diags {
	var d compile.Diags
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		x := compile.Diag{Rule: rule, Severity: compile.SevError, Message: line}
		parts := strings.SplitN(line, ":", 4)
		if len(parts) == 4 {
			var l, c int
			if _, err := fmt.Sscan(parts[1], &l); err == nil {
				fmt.Sscan(parts[2], &c)
				x.File, x.Line, x.Column, x.Message = strings.TrimPrefix(parts[0], "./"), l, c, strings.TrimSpace(parts[3])
			}
		}
		d = append(d, x)
	}
	return d
}

func goModPath() string {
	out, err := exec.Command("go", "env", "GOMOD").Output()
	if err != nil {
		return "go.mod"
	}
	return strings.TrimSpace(string(out))
}

func writeIfChanged(path string, b []byte) error {
	if old, err := os.ReadFile(path); err == nil && bytes.Equal(old, b) {
		return nil
	}
	return os.WriteFile(path, b, 0o644)
}

func cmdExplain(args []string) error {
	fs, asJSON, err := flags("explain", args, nil)
	if err != nil {
		return err
	}
	if fs.NArg() == 0 {
		fmt.Print(checker.RuleIndex())
		return nil
	}
	id := strings.ToUpper(fs.Arg(0))
	rule, ok := checker.RuleByID(id)
	if !ok {
		return fmt.Errorf("no rule %s; run ks explain for the list", id)
	}
	if asJSON {
		printJSON(rule)
		return nil
	}
	fmt.Print(checker.Explain(id))
	return nil
}

func cmdDiff(args []string) error {
	var base, format *string
	_, asJSON, err := flags("diff", args, func(fs *flag.FlagSet) {
		base = fs.String("base", "main", "git revision to compare against")
		format = fs.String("format", "text", "text, markdown or json")
	})
	if err != nil {
		return err
	}
	if asJSON {
		*format = "json"
	}
	f := diff.Format(*format)
	switch f {
	case diff.Text, diff.Markdown, diff.JSON:
	default:
		return fmt.Errorf("unknown format %q", *format)
	}
	// Both sides are the committed model and facts: no old code is built (§6.1).
	headApp, headFacts, err := readSide(os.ReadFile)
	if err != nil {
		return err
	}
	prefix, err := gitPrefix()
	if err != nil {
		return err
	}
	baseApp, baseFacts, err := readSide(func(name string) ([]byte, error) {
		return exec.Command("git", "show", *base+":"+prefix+name).Output()
	})
	if err != nil {
		// A revision without a model is the first release: everything is new.
		baseApp, baseFacts = nil, model.Facts{}
	}
	fmt.Print(diff.Render(diff.Between(baseApp, headApp, baseFacts, headFacts), f))
	return nil
}

func readSide(read func(string) ([]byte, error)) (*model.App, model.Facts, error) {
	var facts model.Facts
	raw, err := read("app.model.json")
	if err != nil {
		return nil, facts, fmt.Errorf("app.model.json: %w (run ks gen)", err)
	}
	app, err := model.Load(raw)
	if err != nil {
		return nil, facts, err
	}
	if raw, err := read("app.facts.json"); err == nil {
		_ = json.Unmarshal(raw, &facts)
	}
	return app, facts, nil
}

// gitPrefix is the working directory relative to the repository root, for git show.
func gitPrefix() (string, error) {
	out, err := exec.Command("git", "rev-parse", "--show-prefix").Output()
	if err != nil {
		return "", fmt.Errorf("ks diff needs a git repository: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

func cmdClient(args []string) error {
	var out *string
	fs, _, err := flags("client", args, func(fs *flag.FlagSet) {
		out = fs.String("out", "client/ts", "output directory")
	})
	if err != nil {
		return err
	}
	if fs.Arg(0) != "ts" {
		return fmt.Errorf("usage: ks client ts --out <dir>")
	}
	app, diags, err := loadModel()
	if err != nil {
		return err
	}
	if hasErrors(diags) {
		return report(diags, false, false)
	}
	if err := os.MkdirAll(*out, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(*out, "index.ts"), codegen.TypeScriptClient(app), 0o644)
}

func cmdBuild(args []string) error {
	var out *string
	_, _, err := flags("build", args, func(fs *flag.FlagSet) {
		out = fs.String("out", filepath.Join("bin", "server"), "binary path")
	})
	if err != nil {
		return err
	}
	cmd := exec.Command("go", "build", "-trimpath", "-ldflags=-s -w", "-o", *out, "./cmd/server")
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run()
}

func cmdTest(args []string) error {
	_, asJSON, err := flags("test", args, nil)
	if err != nil {
		return err
	}
	goArgs := []string{"test", "-count=1"}
	if asJSON {
		goArgs = append(goArgs, "-json")
	}
	cmd := exec.Command("go", append(goArgs, "./...")...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	// Tests need a Postgres to clone per-test databases from (§16.1).
	cmd.Env = os.Environ()
	if os.Getenv("KS_TEST_DB_URL") == "" && os.Getenv("KS_DATABASE_URL") != "" {
		cmd.Env = append(cmd.Env, "KS_TEST_DB_URL="+os.Getenv("KS_DATABASE_URL"))
	}
	if err := cmd.Run(); err != nil {
		return exitError{1}
	}
	return nil
}
