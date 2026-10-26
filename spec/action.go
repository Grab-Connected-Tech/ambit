package spec

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"keystone.dev/keystone/model"
)

// ActionDecl is a declared action. Its pointer is what other specs reference for
// s.Calls and s.Runs.
type ActionDecl struct {
	module, name string
	line         int
	kind         model.ActionKind

	requires Rule
	public   bool
	input    []*Field
	returns  []returnSpec
	reads    []*ResourceDecl
	writes   []*ResourceDecl
	calls    []*ActionDecl
	effects  []*Effect
	errors   []errSpec
	limits   model.ActionLimits
	retry    *model.Retry
	conc     *model.Concurrency
	runAs    string
	cron     string
	runs     *ActionDecl
	cache    *model.Cache

	counts       bool
	tz           string
	crossTenant  bool
	idempotent   bool
	serializable bool
	replicaOK    bool
	allowGet     bool
	deprecated   string

	// problems are declaration errors surfaced when the model is built.
	problems []string
}

// Name returns the declared action name.
func (d *ActionDecl) Name() string { return d.name }

// Ref returns the qualified action name once the module is known.
func (d *ActionDecl) Ref() string { return model.Qual(d.module, d.name) }

// Path is the stable RPC route for the action (§12.1).
func (d *ActionDecl) Path() string { return "/api/" + d.module + "/" + d.name }

type returnSpec struct {
	resource *ResourceDecl
	page     *ResourceDecl
	fields   []*Field
}

type errSpec struct {
	code    string
	message string
}

// aOpt is the function form of an action option.
type aOpt func(*ActionDecl)

func (f aOpt) applyAction(d *ActionDecl) { f(d) }

// SessionTTL is an alias kept so a session TTL reads the same as a cache TTL.
var _ = Expiry{}

func aOptionFunc(fn func(*ActionDecl)) AOption { return aOpt(fn) }

// Query declares an action that reads only.
func Query(name string, opts ...AOption) *ActionDecl {
	return declareAction(name, model.KindQuery, opts)
}

// Mutation declares an action that writes.
func Mutation(name string, opts ...AOption) *ActionDecl {
	return declareAction(name, model.KindMutation, opts)
}

// Job declares background work that runs from the queue.
func Job(name string, opts ...AOption) *ActionDecl { return declareAction(name, model.KindJob, opts) }

// Schedule runs a job on a cron (§5.7).
func Schedule(name string, opts ...AOption) *ActionDecl {
	return declareAction(name, model.KindSchedule, opts)
}

func declareAction(name string, kind model.ActionKind, opts []AOption) *ActionDecl {
	if !isTypeName(name) {
		specErr("spec: action name %q must be UpperCamelCase", name)
	}
	d := &ActionDecl{name: name, kind: kind}
	d.register()
	for _, o := range opts {
		o.applyAction(d)
	}
	return d
}

// Input declares typed input fields, using the same builders as resources (§5.7).
func Input(fields ...*Field) AOption {
	return aOptionFunc(func(d *ActionDecl) { d.input = append(d.input, fields...) })
}

// Returns declares the output: a resource, s.Page(resource), or plain fields.
func Returns(targets ...any) AOption {
	return aOptionFunc(func(d *ActionDecl) {
		rs := returnSpec{}
		for _, t := range targets {
			switch v := t.(type) {
			case *ResourceDecl:
				rs.resource = v
			case pageSpec:
				rs.page = v.resource
			case *Field:
				rs.fields = append(rs.fields, v)
			case []*Field:
				rs.fields = append(rs.fields, v...)
			default:
				d.problems = append(d.problems, fmt.Sprintf("Returns: unsupported target %T", t))
			}
		}
		d.returns = append(d.returns, rs)
	})
}

type pageSpec struct{ resource *ResourceDecl }

// Page marks a query's output as a keyset-paginated list of a resource (§8.1).
func Page(res *ResourceDecl) any { return pageSpec{resource: res} }

// Requires names the rule that lets a caller invoke the action. Either Requires or
// Public is mandatory (KS-SPEC-004).
func Requires(rule Rule) AOption {
	return aOptionFunc(func(d *ActionDecl) {
		if d.public {
			d.problems = append(d.problems, "an action declares both Requires and Public")
			return
		}
		d.requires = rule
	})
}

// Public opts out of authorization for this action. Public mutations must also
// declare a rate limit (KS-SEC-005).
func Public() AOption {
	return aOptionFunc(func(d *ActionDecl) {
		if d.requires != nil {
			d.problems = append(d.problems, "an action declares both Requires and Public")
			return
		}
		d.public = true
	})
}

// Reads lists the resources an action may query (§4.1).
func Reads(res ...*ResourceDecl) AOption {
	return aOptionFunc(func(d *ActionDecl) { d.reads = append(d.reads, res...) })
}

// Writes lists the resources an action may insert, update or delete.
func Writes(res ...*ResourceDecl) AOption {
	return aOptionFunc(func(d *ActionDecl) { d.writes = append(d.writes, res...) })
}

// Calls declares cross-module actions; each adds a capability to the context (§4.1).
func Calls(acts ...*ActionDecl) AOption {
	return aOptionFunc(func(d *ActionDecl) { d.calls = append(d.calls, acts...) })
}

// Effects declares the side effects an action may perform.
func Effects(effs ...*Effect) AOption {
	return aOptionFunc(func(d *ActionDecl) { d.effects = append(d.effects, effs...) })
}

// Errors declares action-specific error codes, typed in the client.
func Errors(codes ...any) AOption {
	return aOptionFunc(func(d *ActionDecl) {
		for _, c := range codes {
			switch v := c.(type) {
			case string:
				d.errors = append(d.errors, errSpec{code: v})
			case *ErrorSpec:
				d.errors = append(d.errors, errSpec{code: v.code, message: v.message})
			default:
				d.problems = append(d.problems, fmt.Sprintf("Errors: expected a code string, got %T", c))
			}
		}
	})
}

// ErrorSpec pairs a code with a message shown to callers.
type ErrorSpec struct {
	code, message string
}

// NamedError declares an error code with its human message.
func NamedError(code, message string) *ErrorSpec { return &ErrorSpec{code: code, message: message} }

// RateLimit declares one or more token buckets (§5.7).
func RateLimit(limits ...model.RateLimit) AOption {
	return aOptionFunc(func(d *ActionDecl) {
		d.limits.RateLimit = append(d.limits.RateLimit, limits...)
	})
}

// PerActor limits calls per authenticated actor.
func PerActor(n int, per time.Duration) model.RateLimit {
	return model.RateLimit{Scope: "actor", Limit: n, Per: per.String()}
}

// PerIP limits calls per client address, which is what public endpoints need.
func PerIP(n int, per time.Duration) model.RateLimit {
	return model.RateLimit{Scope: "ip", Limit: n, Per: per.String()}
}

// PerTenant limits calls per tenant.
func PerTenant(n int, per time.Duration) model.RateLimit {
	return model.RateLimit{Scope: "tenant", Limit: n, Per: per.String()}
}

// Timeout overrides the action's total timeout (§8.3).
func Timeout(d time.Duration) AOption {
	return aOptionFunc(func(x *ActionDecl) { x.limits.Timeout = d.String() })
}

// StatementTimeout overrides the per-statement deadline.
func StatementTimeout(d time.Duration) AOption {
	return aOptionFunc(func(x *ActionDecl) { x.limits.StmtTimeout = d.String() })
}

// QueryBudget caps the number of queries an action may issue (§8.4).
func QueryBudget(n int) AOption {
	return aOptionFunc(func(d *ActionDecl) {
		if n <= 0 {
			d.problems = append(d.problems, "QueryBudget must be positive")
			return
		}
		d.limits.QueryBudget = n
	})
}

// RowsBudget caps the number of rows an action may read (§8.4).
func RowsBudget(n int) AOption {
	return aOptionFunc(func(d *ActionDecl) { d.limits.RowBudget = n })
}

// Counts permits a Query().Count() call, which is otherwise unbounded (§8.1).
func Counts() AOption {
	return aOptionFunc(func(d *ActionDecl) { d.limits.QueryBudget |= 0; d.counts = true })
}

// CrossTenant permits reads outside the actor's tenant. The diff always ranks it
// high risk, and no rule can express it away (§5.6).
func CrossTenant() AOption {
	return aOptionFunc(func(d *ActionDecl) { d.crossTenant = true })
}

// Idempotent requires an Idempotency-Key and replays the stored result (§5.7).
func Idempotent() AOption {
	return aOptionFunc(func(d *ActionDecl) { d.idempotent = true })
}

// Serializable runs the action at SERIALIZABLE with automatic retries (§8.3).
func Serializable() AOption {
	return aOptionFunc(func(d *ActionDecl) { d.serializable = true })
}

// ReplicaOK routes the action's reads to a configured replica (§8.3).
func ReplicaOK() AOption {
	return aOptionFunc(func(d *ActionDecl) { d.replicaOK = true })
}

// Get exposes a query over GET with query parameters as well as POST (§12.1).
func Get() AOption {
	return aOptionFunc(func(d *ActionDecl) { d.allowGet = true })
}

// Deprecated marks a shape for removal; it is what makes a breaking change legal
// for one release (§12.4).
func Deprecated(reason string) AOption {
	return aOptionFunc(func(d *ActionDecl) {
		if strings.TrimSpace(reason) == "" {
			d.problems = append(d.problems, "Deprecated needs a reason")
			return
		}
		d.deprecated = reason
	})
}

// RunAs names the actor a job runs as (§5.7).
func RunAs(as string) AOption {
	return aOptionFunc(func(d *ActionDecl) {
		switch as {
		case System, Enqueuer:
			d.runAs = as
		default:
			d.problems = append(d.problems, "RunAs must be s.System or s.Enqueuer")
		}
	})
}

// System and Enqueuer are the two job actor choices.
const (
	System   = "system"
	Enqueuer = "enqueuer"
)

// Retry sets a job's retry policy (§5.7).
func Retry(opts ...RetryOption) AOption {
	return aOptionFunc(func(d *ActionDecl) {
		r := &model.Retry{MaxAttempts: 5, Backoff: "exponential", Base: time.Second.String(), Cap: (10 * time.Minute).String()}
		for _, o := range opts {
			o.applyRetry(r)
		}
		d.retry = r
	})
}

// RetryOption mutates a retry policy.
type RetryOption interface{ applyRetry(*model.Retry) }

type retryOpt func(*model.Retry)

func (f retryOpt) applyRetry(r *model.Retry) { f(r) }

// MaxAttempts caps the number of tries before dead-lettering.
func MaxAttempts(n int) RetryOption {
	return retryOpt(func(r *model.Retry) { r.MaxAttempts = n })
}

// Exponential sets exponential backoff with jitter between base and cap.
func Exponential(base, cap time.Duration) RetryOption {
	return retryOpt(func(r *model.Retry) {
		r.Backoff = "exponential"
		r.Base = base.String()
		r.Cap = cap.String()
	})
}

// Fixed sets a constant delay between attempts.
func Fixed(d time.Duration) RetryOption {
	return retryOpt(func(r *model.Retry) { r.Backoff = "fixed"; r.Base = d.String(); r.Cap = d.String() })
}

// Concurrency serializes jobs that share a key value (§10.4).
func Concurrency(key string, limit int) AOption {
	return aOptionFunc(func(d *ActionDecl) { d.conc = &model.Concurrency{Key: key, Limit: limit} })
}

// Key names the field used for job concurrency grouping.
func Key(field string) string { return field }

// Cron sets a schedule's five-field cron expression (§10.5).
func Cron(expr string) AOption {
	return aOptionFunc(func(d *ActionDecl) {
		if err := validCron(expr); err != nil {
			d.problems = append(d.problems, err.Error())
			return
		}
		d.cron = expr
	})
}

// TimeZone names the zone a schedule evaluates in; UTC unless declared.
func TimeZone(zone string) AOption {
	return aOptionFunc(func(d *ActionDecl) { d.tz = zone })
}

// Runs names the job a schedule enqueues.
func Runs(job *ActionDecl) AOption {
	return aOptionFunc(func(d *ActionDecl) {
		if job == nil {
			d.problems = append(d.problems, "Runs: nil job")
			return
		}
		d.runs = job
	})
}

// Cache declares a cached query with its invalidators (§11.3).
func Cache(opts ...CacheOption) AOption {
	return aOptionFunc(func(d *ActionDecl) {
		c := &model.Cache{TTL: time.Minute.String()}
		for _, o := range opts {
			o.applyCache(c)
		}
		d.cache = c
	})
}

// CacheOption mutates a cache declaration.
type CacheOption interface{ applyCache(*model.Cache) }

type cacheOpt func(*model.Cache)

func (f cacheOpt) applyCache(c *model.Cache) { f(c) }

// InvalidatedBy names the writes that drop the cache entry.
func InvalidatedBy(res ...*ResourceDecl) CacheOption {
	return cacheOpt(func(c *model.Cache) {
		for _, r := range res {
			if r != nil {
				c.InvalidatedBy = append(c.InvalidatedBy, r.Ref())
			}
		}
		sort.Strings(c.InvalidatedBy)
	})
}

// validCron accepts the five-field subset the spec needs (§10.5).
func validCron(expr string) error {
	fields := strings.Fields(expr)
	if len(fields) != 5 {
		return fmt.Errorf("cron %q must have five fields", expr)
	}
	bounds := [][2]int{{0, 59}, {0, 23}, {1, 12}, {1, 31}, {0, 7}}
	for i, f := range fields {
		for _, part := range strings.Split(f, ",") {
			step := ""
			if s := strings.Index(part, "/"); s >= 0 {
				step, part = part[s+1:], part[:s]
			}
			if step != "" && !isNum(step) {
				return fmt.Errorf("cron field %q has a bad step", f)
			}
			if part == "*" {
				continue
			}
			if strings.Contains(part, "-") {
				lo, hi, ok := strings.Cut(part, "-")
				if !ok || !inRange(lo, bounds[i]) || !inRange(hi, bounds[i]) {
					return fmt.Errorf("cron range %q out of bounds", part)
				}
				continue
			}
			if !inRange(part, bounds[i]) {
				return fmt.Errorf("cron value %q out of bounds", part)
			}
		}
	}
	return nil
}

func isNum(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func inRange(s string, b [2]int) bool {
	if !isNum(s) {
		return false
	}
	n := 0
	for _, r := range s {
		n = n*10 + int(r-'0')
	}
	return n >= b[0] && n <= b[1]
}
