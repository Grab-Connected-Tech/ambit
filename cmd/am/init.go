package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"strings"
)

// cmdInit creates a new app: go.mod, the app-level spec, one example module, config, a
// CI workflow, and everything am gen derives from them (§19).
func cmdInit(args []string) error {
	var module, framework *string
	fs, _, err := flags("init", args, func(fs *flag.FlagSet) {
		module = fs.String("module", "", "Go module path (default: the name)")
		framework = fs.String("framework", "", "local Ambit checkout to build against (adds a replace)")
	})
	if err != nil {
		return err
	}
	name := fs.Arg(0)
	if name == "" || strings.ContainsAny(name, `/\ `) {
		return fmt.Errorf("usage: am init <name>")
	}
	if _, err := os.Stat(name); err == nil {
		return fmt.Errorf("%s already exists", name)
	}
	if *module == "" {
		*module = name
	}
	version := "v0.1.0"
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		version = bi.Main.Version
	}
	gomod := fmt.Sprintf("module %s\n\ngo 1.24\n\nrequire github.com/Grab-Connected-Tech/ambit %s\n", *module, version)
	if *framework != "" {
		abs, err := filepath.Abs(*framework)
		if err != nil {
			return err
		}
		gomod += "\nreplace github.com/Grab-Connected-Tech/ambit => " + abs + "\n"
	}
	files := map[string]string{
		"go.mod":                   gomod,
		"app/app.go":               initApp,
		"app/tasks/spec.go":        strings.ReplaceAll(initTasks, "MODULE", *module),
		"am.yaml":                  "# Ambit configuration (§18.2). Secrets come from the environment.\nlimits:\n  queryBudget: 50\n",
		".github/workflows/am.yml": initCI,
		"seed/001_example.sql":     "-- Seed rows for am dev, loaded on start and on POST /_am/reset.\n",
		".gitignore":               "bin/\n.am/\n",
	}
	for path, body := range files {
		full := filepath.Join(name, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			return err
		}
	}
	if err := os.Chdir(name); err != nil {
		return err
	}
	tidy := exec.Command("go", "mod", "tidy")
	tidy.Stderr = os.Stderr
	if err := tidy.Run(); err != nil {
		return errors.New("go mod tidy failed; pass --framework <ambit checkout> if the framework is not published")
	}
	if err := cmdGen(nil); err != nil {
		return err
	}
	tidy = exec.Command("go", "mod", "tidy")
	tidy.Stderr = os.Stderr
	if err := tidy.Run(); err != nil {
		return err
	}
	fmt.Printf("created %s: edit app/tasks/spec.go, implement app/tasks/actions.go, then am check && am test\n", name)
	return nil
}

const initApp = `// Package app holds the app-level spec: actors, roles, tenancy and identity (§4.2).
package app

import (
	s "github.com/Grab-Connected-Tech/ambit/spec"
)

// Org is the tenant: the top-level isolation boundary.
var Org = s.TenantResource("Org",
	s.Fields(s.Text("name").MaxLen(200)),
	s.Access(s.Read(Member), s.Update(Admin)),
)

// User is the account an actor logs in as; its id is the actor id.
var User = s.Account("User",
	s.Fields(
		s.Email("email").Unique(),
		s.Text("name").MaxLen(200).Optional(),
	),
	s.Access(
		s.Read(s.Owner("id").Or(Admin)),
		s.Update(s.Owner("id").Or(Admin)),
		s.Delete(Admin),
	),
)

var (
	Admin  = s.Role("admin")
	Member = s.Role("member")
)

var Identity = s.Identity(
	s.Sessions(s.TTL(30*s.Day), s.IdleTimeout(7*s.Day)),
	s.APITokens(s.Scoped()),
)
`

const initTasks = `// Package tasks is the example module. Humans own this spec; actions.go is AI-written.
package tasks

import (
	s "github.com/Grab-Connected-Tech/ambit/spec"

	"MODULE/app"
)

var Task = s.Resource("Task",
	s.Tenant(),
	s.Fields(
		s.Text("title").MaxLen(200),
		s.Bool("done").Default(false),
		s.Ref("owner", app.User),
	),
	s.Access(
		s.Read(app.Member),
		s.Create(app.Member),
		s.Update(s.Owner("owner").Or(app.Admin)),
		s.Delete(app.Admin),
	),
)

var CreateTask = s.Mutation("CreateTask",
	s.Input(s.Text("title").MaxLen(200)),
	s.Returns(Task),
	s.Requires(app.Member),
	s.Writes(Task),
	s.Idempotent(),
)

var ListTasks = s.Query("ListTasks",
	s.Input(s.Bool("done").Optional()),
	s.Returns(s.Page(Task)),
	s.Requires(app.Member),
	s.Reads(Task),
)
`

const initCI = `name: am
on: [push, pull_request]
jobs:
  check:
    runs-on: ubuntu-latest
    services:
      postgres:
        image: postgres:16
        env:
          POSTGRES_PASSWORD: postgres
        ports: ["5432:5432"]
    env:
      AM_TEST_DB_URL: postgres://postgres:postgres@localhost:5432/postgres?sslmode=disable
    steps:
      - uses: actions/checkout@v4
        with:
          fetch-depth: 0
      - uses: actions/setup-go@v5
        with:
          go-version-file: go.mod
      - run: go install github.com/Grab-Connected-Tech/ambit/cmd/am@latest
      - run: am gen && git diff --exit-code
      - run: am check --strict
      - run: am test
      - run: am migrate verify
      - run: am diff --base origin/${{ github.base_ref || 'main' }} --format markdown
`
