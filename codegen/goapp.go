package codegen

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"keystone.dev/keystone/model"
)

// writeGoApp emits gen/app.gen.go: the model itself, the small helpers generated code
// calls, and the bootstrap that turns configuration into a running app (§7.1, §16.1).
func writeGoApp(b *strings.Builder, app *model.App, o Options) error {
	raw, err := json.Marshal(app)
	if err != nil {
		return err
	}
	b.WriteString(header + "\n")
	b.WriteString("package gen\n\n")
	b.WriteString("import (\n" +
		"\t\"context\"\n\t\"encoding/json\"\n\t\"fmt\"\n\t\"os\"\n\t\"regexp\"\n\t\"strconv\"\n\t\"sync\"\n\t\"time\"\n\n" +
		"\t\"keystone.dev/keystone/ks\"\n\t\"keystone.dev/keystone/model\"\n\t\"keystone.dev/keystone/runtime\"\n)\n\n")
	fmt.Fprintf(b, "// AppName is the value declared in the app spec.\nconst AppName = %q\n\n", app.App.Name)
	fmt.Fprintf(b, "// ModelHash is the canonical identity of this app's model; ks check recomputes it (§6.1).\nconst ModelHash = %q\n\n", app.Hash())
	b.WriteString("// modelJSON is the app model, compiled from the spec when ks gen ran. Everything here\n")
	b.WriteString("// is derived from it, which is why regenerating a file twice produces the same bytes (§6).\n")
	b.WriteString("const modelJSON = `" + string(raw) + "`\n\n")
	b.WriteString("// Model is the parsed app model.\nvar Model = mustModel(modelJSON)\n\n")
	b.WriteString(`func mustModel(raw string) *model.App {
	app := &model.App{}
	if err := json.Unmarshal([]byte(raw), app); err != nil {
		panic("gen: the embedded app model does not parse: " + err.Error())
	}
	return app
}

// mustAction looks an action up in the model. A generated handler that names an action the
// spec does not declare is a generator bug, and it says so at startup rather than at
// request time.
func mustAction(ref string) model.Action {
	_, a := Model.FindAction(ref)
	if a == nil {
		panic("gen: " + ref + " is not a declared action")
	}
	return *a
}

var emailPattern = regexp.MustCompile("^[^@\\s]+@[^@\\s]+\\.[^@\\s]+$")

func validEmail(s string) bool { return emailPattern.MatchString(s) }

var patterns patternCache

// syncMap caches compiled regexes; a pattern in a validation is written once and read on
// every request.
type patternCache struct{ m sync.Map }

func matchPattern(s, expr string) bool {
	if v, ok := patterns.m.Load(expr); ok {
		return v.(*regexp.Regexp).MatchString(s)
	}
	re, err := regexp.Compile(expr)
	if err != nil {
		// A pattern the spec could not compile is a build-time bug; failing open would
		// silently accept anything.
		panic("gen: bad validation pattern " + expr + ": " + err.Error())
	}
	patterns.m.Store(expr, re)
	return re.MatchString(s)
}

func dedupe(ids []ks.ID) []ks.ID {
	seen := make(map[ks.ID]bool, len(ids))
	out := ids[:0]
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

// HandlersWithConfig builds the runtime handlers for every declared action. The
// implementations arrive as arguments, which is where the compile-time signature check
// happens (§7.2).
func HandlersWithConfig(handlers ...*runtime.Handler) []*runtime.Handler {
	return handlers
}

// oneOf is the membership test a generated enum validator uses; it is written once here so
// a validation stays a single line.
func oneOf(v string, options []string) bool {
	for _, o := range options {
		if o == v {
			return true
		}
	}
	return false
}

// Config is the environment the app runs in: ks.yaml where it exists, the documented
// KS_* variables over it, and the framework's own numbers under both, so a laptop can run
// the binary with only a database URL (§16.1).
func Config() runtime.Config {
	c := *runtime.Defaults()
	c.App = AppName
	if path := os.Getenv("KS_CONFIG"); path != "" {
		loaded, err := runtime.LoadConfig(path)
		if err != nil {
			panic("gen: " + path + ": " + err.Error())
		}
		c = *loaded
		c.App = AppName
	}
	envStr(&c.DatabaseURL, "KS_DATABASE_URL")
	envStr(&c.ReplicaURL, "KS_REPLICA_URL")
	envStr(&c.Role, "KS_ROLE")
	envDur(&c.ActionTimeout, "KS_ACTION_TIMEOUT")
	envDur(&c.StmtTimeout, "KS_STATEMENT_TIMEOUT")
	envInt(&c.QueryBudget, "KS_QUERY_BUDGET")
	envInt(&c.MaxConns, "KS_MAX_CONNS")
	envInt(&c.AdminPort, "KS_ADMIN_PORT")
	return c
}

func envStr(dst *string, key string) {
	if v := os.Getenv(key); v != "" {
		*dst = v
	}
}

func envInt(dst *int, key string) {
	if v := os.Getenv(key); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			panic("gen: " + key + ": " + err.Error())
		}
		*dst = n
	}
}

func envDur(dst *time.Duration, key string) {
	if v := os.Getenv(key); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			panic("gen: " + key + ": " + err.Error())
		}
		*dst = d
	}
}

// New builds the app: connection pool, schema check, and every handler (§16.1).
func New(ctx context.Context, cfg *runtime.Config, handlers []*runtime.Handler) (*runtime.App, error) {
	merged := Config()
	if cfg != nil {
		c := *cfg
		if c.DatabaseURL != "" {
			merged.DatabaseURL = c.DatabaseURL
		}
		if c.Role != "" {
			merged.Role = c.Role
		}
	}
	// The model is passed by value semantics here: the runtime reads it to build routes,
	// budgets and the schema check, and an app never edits it at runtime (§6).
	return runtime.New(ctx, runtime.Options{Cfg: &merged, Model: Model, Handlers: handlers})
}

// Serve is what main calls: build the app, then run web, workers and schedules according
// to the process role (§10.6).
func Serve(ctx context.Context, handlers []*runtime.Handler) error {
	app, err := New(ctx, nil, handlers)
	if err != nil {
		return err
	}
	defer app.Close()
	addr := os.Getenv("KS_ADDR")
	if addr == "" {
		addr = ":8080"
	}
	return app.NewServer(addr).Serve(ctx)
}

`)
	// The generated helpers need a couple of stdlib imports that are not always used by
	// the emitters above; keep the list honest per model instead of guessing.
	b.WriteString("var _ = json.Marshal\nvar _ = fmt.Sprint\nvar _ = os.Getenv\nvar _ = regexp.Match\nvar _ = strconv.Itoa\nvar _ = patternCache{}\nvar _ = time.Now\nvar _ = context.Background\nvar _ json.RawMessage\n\n")

	_ = o
	return nil
}

// writeGoMain emits cmd/server/main.go: the entrypoint that names each action's
// implementation. Passing the function to its generated handler adapter is where the
// action contract is asserted by the compiler (§7.2).
func writeGoMain(b *strings.Builder, app *model.App, o Options) error {
	var mods []*model.Module
	for i := range app.Modules {
		mods = append(mods, &app.Modules[i])
	}
	sort.Slice(mods, func(i, j int) bool { return mods[i].Name < mods[j].Name })
	b.WriteString(header + "\n")
	b.WriteString("package main\n\nimport (\n\t\"context\"\n\t\"fmt\"\n\t\"os\"\n\t\"os/signal\"\n\t\"syscall\"\n\n")
	for _, m := range mods {
		if len(m.Actions) == 0 {
			continue
		}
		imp := o.moduleImport(m.Name)
		b.WriteString("\t" + pkgName(imp) + " \"" + imp + "\"\n")
	}
	b.WriteString("\n\t\"" + o.genImport() + "\"\n\t\"keystone.dev/keystone/runtime\"\n)\n\n")
	b.WriteString("func main() {\n\tctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)\n\tdefer stop()\n\n")
	b.WriteString("\thandlers := []*runtime.Handler{\n")
	for _, m := range mods {
		if len(m.Actions) == 0 {
			continue
		}
		alias := pkgName(o.moduleImport(m.Name))
		list := append([]model.Action(nil), m.Actions...)
		sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
		for _, a := range list {
			if a.Kind == model.KindSchedule {
				// A schedule enqueues the job it names; it is not an entrypoint of its own.
				continue
			}
			b.WriteString("\t\tgen." + a.Name + "Handler(" + alias + ".Actions." + a.Name + "),\n")
		}
	}
	b.WriteString("\t}\n\tif err := gen.Serve(ctx, handlers); err != nil {\n\t\tfmt.Fprintln(os.Stderr, err)\n\t\tos.Exit(1)\n\t}\n}\n")
	return nil
}

func sortedKeys(m map[string]*model.Module) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func (o Options) moduleImport(module string) string {
	if p, ok := o.ModulePackages[module]; ok && p != "" {
		return p
	}
	base := o.AppPackage
	if base == "" {
		base = o.Dir
	}
	return strings.TrimSuffix(base, "/") + "/" + module
}
