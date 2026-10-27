package runtime

import (
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/abhilashiig/ambit/am"
)

// Config is the validated view of am.yaml (§18.2). Action code never sees a secret:
// providers receive theirs from here, and the only secrets an app can read are the
// ones it declared.
type Config struct {
	App        string
	AppDir     string
	ModulePath string

	DatabaseURL     string
	ReplicaURL      string
	MaxConns        int
	PoolWaitTimeout time.Duration

	BodyLimitBytes  int64
	ActionTimeout   time.Duration
	StmtTimeout     time.Duration
	QueryBudget     int
	RowBudget       int64
	MaxPageSize     int
	DefaultPageSize int
	MaxInFlight     int

	Role string // web | worker | all (§10.6)

	Secrets map[string]string
	Effects map[string]map[string]string

	AllowedImports []string
	OTLPEndpoint   string
	AdminPort      int

	// CacheShared adds the Postgres entry tier to the in-process query cache (§11.3).
	CacheShared bool

	// Compatibility: "none" lets an app break its API on purpose (§12.4).
	Compatibility string

	// DeclaredKeys holds every path present in the file, so a lookup for an
	// undeclared secret returns nothing instead of reaching the environment.
	DeclaredKeys map[string]bool

	unknown []string
}

// Defaults returns the framework's own numbers, before configuration.
func Defaults() *Config {
	return &Config{
		MaxConns:        16,
		PoolWaitTimeout: 2 * time.Second,
		BodyLimitBytes:  1 << 20,
		ActionTimeout:   10 * time.Second,
		StmtTimeout:     5 * time.Second,
		QueryBudget:     20,
		RowBudget:       10000,
		MaxPageSize:     100,
		DefaultPageSize: 25,
		MaxInFlight:     256,
		Role:            "all",
		AdminPort:       9091,
		Secrets:         map[string]string{},
		Effects:         map[string]map[string]string{},
		DeclaredKeys:    map[string]bool{},
	}
}

// LoadConfig reads am.yaml. The format is the restricted subset §18.2 shows: nested
// maps, scalar values, lists of scalars and ${ENV} expansion. A parser small enough
// to audit is one dependency avoided (§2.10).
func LoadConfig(path string) (*Config, error) {
	c := Defaults()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return c, c.applyEnv()
		}
		return nil, err
	}
	var lines []string
	for _, l := range strings.Split(string(data), "\n") {
		l = stripComment(l)
		if strings.TrimSpace(l) != "" {
			lines = append(lines, strings.TrimRight(l, " \t"))
		}
	}
	if len(lines) == 0 {
		return c, c.applyEnv()
	}
	i := 0
	doc, err := parseBlock(lines, &i, indentOf(lines[0]))
	if err != nil {
		return nil, err
	}
	m, ok := doc.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("am.yaml: expected a mapping at the top level")
	}
	c.DeclaredKeys = map[string]bool{}
	flatten(m, nil, c.DeclaredKeys)
	if err := c.apply(m); err != nil {
		return nil, err
	}
	return c, c.applyEnv()
}

func indentOf(s string) int {
	return len(s) - len(strings.TrimLeft(s, " "))
}

// parseBlock reads one indentation level: a mapping, or a list of scalars.
func parseBlock(lines []string, i *int, indent int) (any, error) {
	if *i >= len(lines) {
		return map[string]any{}, nil
	}
	if isItem(lines[*i]) {
		var out []any
		for *i < len(lines) && indentOf(lines[*i]) == indent && isItem(lines[*i]) {
			out = append(out, scalar(strings.TrimSpace(lines[*i])[2:]))
			*i++
		}
		return out, nil
	}
	m := map[string]any{}
	for *i < len(lines) {
		line := lines[*i]
		switch {
		case indentOf(line) < indent:
			return m, nil
		case indentOf(line) > indent:
			return nil, fmt.Errorf("am.yaml:%d: unexpected indentation in %q", *i+1, line)
		}
		if isItem(line) {
			return m, nil
		}
		key, rest, ok := strings.Cut(strings.TrimSpace(line), ":")
		if !ok {
			return nil, fmt.Errorf("am.yaml:%d: expected key: value, got %q", *i+1, line)
		}
		*i++
		rest = strings.TrimSpace(rest)
		if rest != "" {
			m[key] = scalar(rest)
			continue
		}
		if *i < len(lines) && (indentOf(lines[*i]) > indent ||
			(indentOf(lines[*i]) == indent && isItem(lines[*i]))) {
			child, err := parseBlock(lines, i, indentOf(lines[*i]))
			if err != nil {
				return nil, err
			}
			m[key] = child
			continue
		}
		m[key] = map[string]any{}
	}
	return m, nil
}

func isItem(line string) bool { return strings.HasPrefix(strings.TrimSpace(line), "- ") }

func stripComment(s string) string {
	if strings.HasPrefix(strings.TrimSpace(s), "#") {
		return ""
	}
	if i := strings.Index(s, " #"); i >= 0 {
		return s[:i]
	}
	return s
}

var envRe = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)(:-[^}]*)?\}`)

// scalar parses one value, resolving ${VAR} and ${VAR:-default}.
func scalar(v string) any {
	v = strings.TrimSpace(v)
	if len(v) >= 2 {
		if (v[0] == '\'' && v[len(v)-1] == '\'') || (v[0] == '"' && v[len(v)-1] == '"') {
			return v[1 : len(v)-1]
		}
	}
	if v == "" {
		return ""
	}
	if !envRe.MatchString(v) {
		return v
	}
	return os.Expand(v, func(key string) string {
		name, def, _ := strings.Cut(key, ":-")
		if got, ok := os.LookupEnv(name); ok {
			return got
		}
		return def
	})
}

func text(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	}
	return fmt.Sprint(v)
}

func asMap(v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return map[string]any{}
}

func asList(v any) []any {
	if l, ok := v.([]any); ok {
		return l
	}
	return nil
}

func flatten(m map[string]any, prefix []string, out map[string]bool) {
	for k, v := range m {
		path := append(append([]string(nil), prefix...), k)
		out[strings.Join(path, ".")] = true
		if sub, ok := v.(map[string]any); ok {
			flatten(sub, path, out)
		}
	}
}

// apply maps the parsed document onto the typed config and records unknown keys.
func (c *Config) apply(doc map[string]any) error {
	// Secrets first: effect settings name them ("secret:smtpPassword"), and map order
	// would otherwise decide whether the reference resolves.
	for name, val := range asMap(doc["secrets"]) {
		c.Secrets[name] = text(val)
	}
	for k, v := range doc {
		switch k {
		case "app":
			c.App = text(v)
		case "database":
			sub := asMap(v)
			c.DatabaseURL = text(sub["url"])
			c.ReplicaURL = text(sub["replicaUrl"])
			if n, err := strconv.Atoi(text(sub["maxConns"])); err == nil && text(sub["maxConns"]) != "" {
				c.MaxConns = n
			}
		case "limits":
			sub := asMap(v)
			if n, err := strconv.ParseInt(text(sub["requestBodyBytes"]), 10, 64); err == nil {
				c.BodyLimitBytes = n
			}
			c.ActionTimeout = parseDur(text(sub["actionTimeout"]), c.ActionTimeout)
			c.StmtTimeout = parseDur(text(sub["statementTimeout"]), c.StmtTimeout)
			if n, err := strconv.Atoi(text(sub["queryBudget"])); err == nil && text(sub["queryBudget"]) != "" {
				c.QueryBudget = n
			}
			if n, err := strconv.Atoi(text(sub["maxPageSize"])); err == nil && text(sub["maxPageSize"]) != "" {
				c.MaxPageSize = n
			}
			if n, err := strconv.Atoi(text(sub["maxInFlight"])); err == nil && text(sub["maxInFlight"]) != "" {
				c.MaxInFlight = n
			}
		case "secrets":
		case "cache":
			c.CacheShared = text(asMap(v)["shared"]) == "true"
		case "effects":
			for name, cfg := range asMap(v) {
				m := map[string]string{}
				for kk, vv := range asMap(cfg) {
					t := text(vv)
					if strings.HasPrefix(t, "secret:") {
						t = c.Secrets[strings.TrimPrefix(t, "secret:")]
					}
					m[kk] = t
				}
				c.Effects[name] = m
			}
		case "allowedImports":
			for _, x := range asList(v) {
				c.AllowedImports = append(c.AllowedImports, text(x))
			}
		case "telemetry":
			sub := asMap(v)
			c.OTLPEndpoint = text(sub["otlpEndpoint"])
			if n, err := strconv.Atoi(text(sub["adminPort"])); err == nil {
				c.AdminPort = n
			}
		case "api":
			c.Compatibility = text(asMap(v)["compatibility"])
		default:
			c.unknown = append(c.unknown, k)
		}
	}
	return nil
}

func parseDur(s string, def time.Duration) time.Duration {
	if s == "" {
		return def
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return def
	}
	return d
}

// applyEnv fills in what a config file may leave to the environment.
func (c *Config) applyEnv() error {
	if v := os.Getenv("AM_DATABASE_URL"); v != "" {
		c.DatabaseURL = v
	}
	if v := os.Getenv("AM_DATABASE_REPLICA_URL"); v != "" {
		c.ReplicaURL = v
	}
	if v := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"); v != "" {
		c.OTLPEndpoint = v
	}
	if v := os.Getenv("AM_ROLE"); v != "" {
		c.Role = v
	}
	if v := os.Getenv("AM_MAX_CONNS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.MaxConns = n
		}
	}
	for name, v := range c.Secrets {
		if v == "" {
			c.Secrets[name] = os.Getenv(name)
		}
	}
	return nil
}

// UnknownKeys lists configuration keys the schema does not define (§18.2 asks for a
// startup failure rather than a later surprise).
func (c *Config) UnknownKeys() []string { return c.unknown }

// Validate reports a config that cannot start a process.
func (c *Config) Validate() error {
	var problems []string
	if c.DatabaseURL == "" {
		problems = append(problems, "database.url is empty; set it in am.yaml or AM_DATABASE_URL")
	}
	if len(c.unknown) > 0 {
		problems = append(problems, "unknown config keys: "+strings.Join(c.unknown, ", "))
	}
	for name, v := range c.Secrets {
		if v == "" {
			problems = append(problems, "secret "+name+" is not set")
		}
	}
	if c.MaxPageSize <= 0 {
		problems = append(problems, "limits.maxPageSize must be positive")
	}
	if len(problems) > 0 {
		return am.InvalidMulti(map[string]string{"config": strings.Join(problems, "; ")})
	}
	return nil
}

// EffectSettings returns one effect's provider configuration.
func (c *Config) EffectSettings(name string) map[string]string { return c.Effects[name] }

// Secret returns a declared secret by name. An undeclared name returns nothing, so a
// typo can never reach a provider or a log line.
func (c *Config) Secret(name string) string {
	if !c.DeclaredKeys["secrets."+name] {
		return ""
	}
	return c.Secrets[name]
}
