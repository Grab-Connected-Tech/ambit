package runtime

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"

	"github.com/jackc/pgx/v5"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"

	"keystone.dev/keystone/ks"
	"keystone.dev/keystone/model"
	"keystone.dev/keystone/policy"
)

// Server is the HTTP surface: one static route table, one pipeline, no reflection on
// the hot path (§11.1).
type Server struct {
	A        *App
	addr     string
	srv      *http.Server
	admin    *http.Server
	sem      chan struct{}
	log      *slog.Logger
	inflight atomicCount
	dev      http.Handler // /_ks/* introspection, in development only
}

type atomicCount struct {
	mu sync.Mutex
	n  int64
}

func (c *atomicCount) add(d int64) int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.n += d
	return c.n
}

// NewServer wires routes and the backpressure limit.
func (a *App) NewServer(addr string) *Server {
	s := &Server{A: a, addr: addr, sem: make(chan struct{}, max1(a.Cfg.MaxInFlight))}
	if DevMode() {
		s.dev = a.enableDev()
	}
	s.log = a.Log
	s.srv = &http.Server{Addr: addr, Handler: s.recover(s.limit(http.HandlerFunc(s.router)))}
	s.admin = &http.Server{Addr: ":" + strconv.Itoa(a.Cfg.AdminPort), Handler: s.adminHandler()}
	return s
}

func max1(n int) int {
	if n <= 0 {
		return 64
	}
	return n
}

// Serve runs web, worker and schedules according to the process role (§10.6).
func (s *Server) Serve(ctx context.Context) error {
	switch strings.ToLower(s.A.Cfg.Role) {
	case "web":
		s.A.StartWebOnly = true
	case "worker":
		s.A.StartWorkers = true
	case "all":
		s.A.StartWorkers = true
	}
	return s.run(ctx)
}

func (s *Server) run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	drain, stop := signalContext(ctx)
	defer stop()

	if s.A.StartWorkers {
		outbox := s.A.NewOutbox()
		worker := s.A.NewWorker()
		sched := s.A.NewSchedules()
		go outbox.Run(drain)
		go worker.RunLoop(drain)
		go sched.RunLoop(drain)
		go s.A.Listen(drain)
	}
	ln, err := net.Listen("tcp", s.addr)
	if err != nil {
		return err
	}
	errs := make(chan error, 2)
	go func() {
		s.log.Info("listening", "addr", s.addr, "role", s.A.Cfg.Role)
		if err := s.srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errs <- err
		}
	}()
	if s.admin != nil && s.A.Cfg.AdminPort > 0 {
		go func() {
			if err := s.admin.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				s.log.Debug("admin server", "err", err)
			}
		}()
	}
	select {
	case <-drain.Done():
		s.shutdown()
		return nil
	case err := <-errs:
		return err
	}
}

func (s *Server) shutdown() {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	s.A.closed.Store(true)
	_ = s.srv.Shutdown(ctx)
	if s.admin != nil {
		_ = s.admin.Shutdown(ctx)
	}
}

// adminHandler serves metrics, health and profiling on a separate, non-public port (§18.3).
func (s *Server) adminHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		w.Write(s.A.Registry.Bytes())
	})
	mux.HandleFunc("/health/live", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "ok")
	})
	mux.HandleFunc("/health/ready", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := s.A.Pool.Ping(ctx); err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprintln(w, "database unreachable")
			return
		}
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "ready")
	})
	mux.HandleFunc("/debug/pprof/", pprofIndex)
	return mux
}

// router maps a path to one action with a static lookup, no regex (§3.3 step 2).
func (s *Server) router(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	traceID := traceFromRequest(r)
	if r.URL.Path == "/_ks/health/live" || r.URL.Path == "/healthz" {
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "ok")
		return
	}
	if r.URL.Path == "/_ks/health/ready" {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := s.A.Pool.Ping(ctx); err != nil {
			writeError(w, r, ks.Unavailable("database unreachable"), traceID)
			return
		}
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "ready")
		return
	}
	if s.dev != nil && strings.HasPrefix(r.URL.Path, "/_ks/") {
		s.dev.ServeHTTP(w, r)
		return
	}
	h, ok := s.A.ByPath[r.URL.Path]
	if !ok {
		writeError(w, r, ks.NotFound(), traceID)
		return
	}
	if !methodAllowed(h, r.Method) {
		w.Header().Set("Allow", strings.Join(allowedMethods(h), ", "))
		writeError(w, r, ks.NewError("invalid_request", http.StatusMethodNotAllowed, "method not allowed"), traceID)
		return
	}
	if h.Kind == model.KindJob || h.Kind == model.KindSchedule {
		writeError(w, r, ks.Forbidden("jobs are not reachable over HTTP"), traceID)
		return
	}
	body, err := readBody(r, s.A.Cfg.BodyLimitBytes)
	if err != nil {
		writeError(w, r, err, traceID)
		return
	}
	actor, err := s.A.Authenticate(r)
	if err != nil {
		writeError(w, r, err, traceID)
		return
	}
	in := &Call{
		Actor:   actor,
		Body:    body,
		Query:   r.URL.Query(),
		Headers: headers(r),
		IP:      clientIP(r),
		Key:     r.Header.Get("Idempotency-Key"),
		Trace:   traceID,
	}
	out, err := s.A.Run(r.Context(), h, actor, in, s.log.With("traceId", traceID, "action", h.Ref))
	meta := &CallMeta{Trace: traceID}
	if err == nil && out.Meta != nil {
		meta = out.Meta
	}
	if err == nil && meta.Replayed {
		s.A.metric("ks_replayed_requests", 1)
	}
	if err != nil {
		s.A.observe(h.Ref, start, err)
		writeError(w, r, err, traceID)
		return
	}
	s.A.observe(h.Ref, start, nil)
	writeResult(w, r, out, traceID)
}

func methodAllowed(h *Handler, method string) bool {
	for _, m := range allowedMethods(h) {
		if m == method {
			return true
		}
	}
	return false
}

func allowedMethods(h *Handler) []string {
	out := []string{http.MethodPost}
	if h.Spec.AllowGet {
		out = append(out, http.MethodGet)
	}
	return out
}

func readBody(r *http.Request, limit int64) ([]byte, error) {
	if r.Body == nil || r.ContentLength == 0 {
		return []byte("{}"), nil
	}
	if limit <= 0 {
		limit = 1 << 20
	}
	lr := io.LimitReader(r.Body, limit+1)
	body, err := io.ReadAll(lr)
	if err != nil {
		return nil, ks.NewError("invalid_request", http.StatusBadRequest, "the request body could not be read")
	}
	if int64(len(body)) > limit {
		return nil, ks.NewError("payload_too_large", http.StatusRequestEntityTooLarge,
			fmt.Sprintf("the body exceeds the %d byte limit", limit))
	}
	return body, nil
}

func headers(r *http.Request) map[string]string {
	out := map[string]string{}
	for _, k := range []string{"Origin", "User-Agent", "X-Forwarded-For", "Content-Type", "Referer"} {
		if v := r.Header.Get(k); v != "" {
			out[strings.ToLower(k)] = v
		}
	}
	return out
}

func clientIP(r *http.Request) string {
	if v := r.Header.Get("X-Forwarded-For"); v != "" {
		return strings.TrimSpace(strings.Split(v, ",")[0])
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func traceFromRequest(r *http.Request) string {
	if v := r.Header.Get("X-Trace-Id"); len(v) == 32 {
		return v
	}
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// writeResult applies the security headers and compression, then the body (§9.3, §11.2).
func writeResult(w http.ResponseWriter, r *http.Request, out HandlerOutput, traceID string) {
	setSecurityHeaders(w, r)
	status := out.Status
	if status == 0 {
		status = http.StatusOK
	}
	w.Header().Set("X-Trace-Id", traceID)
	if out.ContentType != "" {
		w.Header().Set("Content-Type", out.ContentType)
	} else {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
	}
	body := out.Body
	if len(body) >= 1024 && acceptsGzip(r) {
		var buf bytes.Buffer
		gz := gzip.NewWriter(&buf)
		_, _ = gz.Write(body)
		_ = gz.Close()
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("Vary", "Accept-Encoding")
		body = buf.Bytes()
	}
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func acceptsGzip(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept-Encoding"), "gzip")
}

// setSecurityHeaders is the deny-by-default baseline from §9.3.
func setSecurityHeaders(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Strict-Transport-Security", "max-age=63072000; includeSubDomains")
	h.Set("Referrer-Policy", "same-origin")
	h.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
	h.Set("Cache-Control", "no-store")
	if origin := r.Header.Get("Origin"); origin != "" && !originAllowed(origin) {
		h.Set("X- CSRF-Rejected", "true")
	}
}

func originAllowed(origin string) bool {
	return strings.HasPrefix(origin, "https://") || strings.HasPrefix(origin, "http://localhost") ||
		strings.HasPrefix(origin, "http://127.0.0.1")
}

// limit enforces the in-flight ceiling; excess requests get 503 with Retry-After rather
// than slowing every other request down (§11.2).
func (s *Server) limit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.A.closed.Load() {
			w.Header().Set("Retry-After", "1")
			http.Error(w, "shutting down", http.StatusServiceUnavailable)
			return
		}
		select {
		case s.sem <- struct{}{}:
			defer func() { <-s.sem }()
		default:
			w.Header().Set("Retry-After", "1")
			writeError(w, r, ks.Unavailable("the server is at its concurrency limit"), "")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) recover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				s.log.Error("panic", "value", rec, "stack", string(debug.Stack()))
				writeError(w, r, ks.InternalError(fmt.Errorf("panic: %v", rec)), "")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// writeError renders the shared error shape (§12.2). A caller-supplied message is
// never echoed for internal errors.
func writeError(w http.ResponseWriter, r *http.Request, err error, traceID string) {
	e := ks.AsError(err)
	status := e.Status
	if status == 0 {
		status = http.StatusInternalServerError
	}
	setSecurityHeaders(w, r)
	body := map[string]any{
		"error": map[string]any{
			"code":    e.Code,
			"message": e.Message,
			"traceId": traceID,
		},
	}
	if len(e.Fields) > 0 {
		body["error"].(map[string]any)["fields"] = e.Fields
	}
	if status >= 500 {
		body["error"].(map[string]any)["message"] = "Internal error."
	}
	raw, merr := json.Marshal(body)
	if merr != nil {
		raw = []byte(`{"error":{"code":"internal","message":"Internal error."}}`)
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Trace-Id", traceID)
	if e.RetryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(e.RetryAfter))
	}
	w.WriteHeader(status)
	_, _ = w.Write(raw)
	if e.Internal != nil {
		slog.Default().Error("request failed", "code", e.Code, "err", e.Internal, "traceId", traceID)
	}
}

// CallMeta travels with a result so the pipeline can report replays and traces.
type CallMeta struct {
	Trace    string
	Replayed bool
	Queries  int
}

func (o *HandlerOutput) setMeta(m *CallMeta) { o.meta = m }

// runOptions tunes one invocation.
type runOptions struct {
	NoCommit bool
}

// Run is the pipeline for one action invocation: authorize, rate limit, transaction,
// decode, execute, commit, encode (§3.3).
func (a *App) Run(ctx context.Context, h *Handler, actor ks.Actor, in *Call, log *slog.Logger) (HandlerOutput, error) {
	return a.runWith(ctx, h, actor, in, log, runOptions{})
}

func (a *App) run(ctx context.Context, h *Handler, actor ks.Actor, in *Call, log *slog.Logger) error {
	_, err := a.runWith(ctx, h, actor, in, log, runOptions{})
	return err
}

func (a *App) runWith(ctx context.Context, h *Handler, actor ks.Actor, in *Call, log *slog.Logger, opts runOptions) (HandlerOutput, error) {
	var out HandlerOutput
	if log == nil {
		log = a.Log
	}
	// A handler needs one of the two adapter shapes; the pipeline itself is unchanged by
	// which one an app was generated against (§7.2).
	if h == nil || h.Handle == nil {
		return out, ks.NotFound()
	}
	// Step 4: authorize the call. Requires is evaluated in Go against the actor the
	// session resolved, so this costs no query (§3.3 step 4).
	if err := authorize(h, actor); err != nil {
		return out, err
	}
	// Step 5: rate limit.
	if err := a.checkRateLimit(ctx, h, actor, in.IP); err != nil {
		return out, err
	}
	timeout := a.Cfg.ActionTimeout
	if d, err := time.ParseDuration(h.Spec.Limits.Timeout); err == nil && d > 0 {
		timeout = d
	}
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	invocation := ks.NewID()
	if in.Job != nil && !in.Job.Invocation.IsNil() {
		invocation = in.Job.Invocation
	}
	if err := a.recordInvocation(callCtx, invocation, h, actor); err != nil {
		return out, err
	}
	if h.Spec.Idempotent {
		stored, replay, ierr := a.idempotentResponse(callCtx, h, actor, in)
		if ierr != nil {
			return out, ierr
		}
		if replay {
			out.Body = stored
			out.Meta = &CallMeta{Replayed: true, Trace: traceIDFrom(in)}
			return out, nil
		}
	}

	stmtTimeout := a.Cfg.StmtTimeout
	if d, err := time.ParseDuration(h.Spec.Limits.StmtTimeout); err == nil && d > 0 {
		stmtTimeout = d
	}
	attempts := 1
	if h.Spec.Serializable {
		attempts = 3
	}
	for attempt := 1; ; attempt++ {
		var aerr error
		out, aerr = a.attempt(callCtx, h, actor, in, log, invocation, stmtTimeout, opts)
		if aerr == nil || !isSerialization(aerr) || attempt >= attempts {
			return out, aerr
		}
		select {
		case <-callCtx.Done():
			return out, ks.Timeout("action")
		case <-time.After(time.Duration(attempt*attempt) * 20 * time.Millisecond):
		}
	}
}

func traceIDFrom(in *Call) string { return in.Trace }

func isSerialization(err error) bool {
	var e *ks.Error
	if errors.As(err, &e) {
		return e.Code == "serialization_failure"
	}
	return false
}

func (a *App) attempt(ctx context.Context, h *Handler, actor ks.Actor, in *Call, log *slog.Logger, invocation ks.ID, stmtTimeout time.Duration, opts runOptions) (HandlerOutput, error) {
	var out HandlerOutput
	scope := Scope{Tenant: actor.Tenant, Actor: actor.ID, IsSystem: actor.Kind == ks.ActorSystem, CrossTenant: h.Spec.CrossTenant}
	tx, err := a.BeginTx(ctx, scope, stmtTimeout, h.Spec.Serializable)
	if err != nil {
		return out, err
	}
	d := a.NewDB(tx, actor, WithApp(a), WithLogger(log), WithAction(h.Ref), WithClock(a.Clock), WithTrace(in.Trace),
		WithCrossTenant(h.Spec.CrossTenant),
		WithBudget(orInt(h.Spec.Limits.QueryBudget, a.Cfg.QueryBudget), int64(orInt(h.Spec.Limits.RowBudget, int(a.Cfg.RowBudget))), a.FailOnBudget),
	)
	d.invocation = invocation
	d.conn = tx
	if a.Clock == nil {
		d.clock = SystemClock{}
	}
	call := &Call{Context: ctx, Actor: actor, DB: d, Log: log, Clock: a.Clock, Body: in.Body, Query: in.Query,
		Headers: in.Headers, IP: in.IP, Key: in.Key, Invocation: invocation, Trace: in.Trace, App: a}
	start := time.Now()
	result, callErr := h.Handle(call)
	if a.OnCall != nil {
		a.OnCall(Trace{ID: in.Trace, Invocation: invocation, Action: h.Ref, Actor: actor, Start: start,
			Millis: float64(time.Since(start).Microseconds()) / 1000, Err: callErr,
			Stmts: d.Stmts(), Effects: d.EffectsQueued})
	}
	out = HandlerOutput{}
	if callErr == nil {
		encoded, encErr := Encode(result)
		if encErr != nil {
			return out, encErr
		}
		out.Body = encoded
	}
	if callErr != nil {
		a.Rollback(ctx, tx)
		return out, callErr
	}
	if opts.NoCommit {
		a.Rollback(ctx, tx)
		return out, nil
	}
	if err := a.Commit(ctx, d, tx); err != nil {
		return out, err
	}
	if h.Spec.Idempotent && in.Key != "" {
		if err := a.storeIdempotent(ctx, h, actor, in, out); err != nil {
			a.Log.Warn("idempotency store", "err", err)
		}
	}
	if d.JobsQueued != nil {
		if err := a.Notify(ctx, a.Pool); err != nil {
			a.Log.Debug("job notify", "err", err)
		}
	}
	return out, nil
}

// authorize evaluates the action's Requires against the actor, in Go (§3.3 step 4).
func authorize(h *Handler, actor ks.Actor) error {
	if h.Spec.Public {
		return nil
	}
	// A job has no caller: the action that enqueued it carried the rule, and the job
	// runs as its declared actor (§5.7).
	if h.Kind == model.KindJob || h.Kind == model.KindSchedule {
		return nil
	}
	if h.Spec.Requires == nil {
		return ks.Unauthenticated()
	}
	env := &policy.Env{A: policy.ActorInfo{
		ID:       string(actor.ID),
		Tenant:   string(actor.Tenant),
		Roles:    actor.Roles,
		IsSystem: actor.Kind == ks.ActorSystem,
		Now:      time.Now().UTC(),
	}}
	ok, err := policy.Eval(h.Spec.Requires, env)
	if err != nil {
		return ks.InternalError(err)
	}
	if ok {
		return nil
	}
	if actor.IsAnonymous() {
		return ks.Unauthenticated()
	}
	return ks.Forbidden("the action's Requires rule refused this caller")
}

// Coalesce keeps an empty role list as an empty array, not NULL, because the column
// is NOT NULL and an anonymous caller still gets an invocation record.
func Coalesce(roles []string) []string {
	if roles == nil {
		return []string{}
	}
	return roles
}

func orInt(v, def int) int {
	if v != 0 {
		return v
	}
	return def
}

// recordInvocation stores who ran what, so a RunAs(Enqueuer) job can rebuild the
// actor and so a trace id can be replayed later (§9.5, §10.4).
func (a *App) recordInvocation(ctx context.Context, id ks.ID, h *Handler, actor ks.Actor) error {
	if id.IsNil() {
		return nil
	}
	err := a.inSystemScope(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO ks_invocations (id, tenant_id, actor_id, actor_kind, actor_roles, action, created_at)
	         VALUES ($1,$2,$3,$4,$5,$6,now()) ON CONFLICT (id) DO NOTHING`,
			id, actor.Tenant.OrNil(), actor.ID.OrNil(), string(actor.Kind),
			Coalesce(actor.SortedRoles()), h.Ref)
		return err
	})
	return translate(err)
}

// checkRateLimit enforces declared token buckets with a Postgres store and an
// in-process cache, so a burst costs one statement (§3.3 step 5).
func (a *App) checkRateLimit(ctx context.Context, h *Handler, actor ks.Actor, ip string) error {
	for _, l := range h.Spec.Limits.RateLimit {
		key := bucketKey(l.Scope, actor, ip, h.Ref)
		if _, answered := a.Buckets.allow(key, l.Limit, l.Per); answered {
			// A refusal the cache still stands behind; no statement is spent on it.
			return ks.RateLimited(int(a.Buckets.until(key).Seconds()) + 1)
		}
		rows, err := a.Pool.Query(ctx,
			`WITH upsert AS (
			   INSERT INTO ks_rate_bucket (id, action, scope, bucket_key, tokens, max_tokens, refill_seconds, updated_at)
			     VALUES ($6, $1, $2, $3, $4 - 1, $4, $5, now())
			     ON CONFLICT (action, scope, bucket_key)
			     DO UPDATE SET tokens = LEAST($4, LEAST($4, ks_rate_bucket.tokens + (EXTRACT(EPOCH FROM (now() - ks_rate_bucket.updated_at)) / $5))) - 1,
			                   updated_at = now()
			     RETURNING tokens)
			 SELECT tokens FROM upsert`,
			h.Ref, l.Scope, key, float64(l.Limit), periodSeconds(l.Per), ks.NewID())
		if err != nil {
			// A broken limit fails open rather than refusing all traffic over an
			// unrelated outage, but it must not be silent: the code that should shed
			// is not consulted, so the only defence left is the query budget.
			a.Log.Warn("rate limit unavailable, allowing", "err", err, "cause", errors.Unwrap(err), "action", h.Ref)
			a.metric("ks_rate_limit_error", 1)
			continue
		}
		var remaining float64 = -1
		for rows.Next() {
			_ = rows.Scan(&remaining)
		}
		rows.Close()
		if remaining < 0 {
			wait := a.Buckets.denied(key, l.Limit, l.Per)
			return ks.RateLimited(int(wait.Seconds()) + 1)
		}
		a.Buckets.allowed(key)
	}
	return nil
}

func periodSeconds(per string) float64 {
	d, err := time.ParseDuration(per)
	if err != nil || d <= 0 {
		return 60
	}
	return d.Seconds()
}

func bucketKey(scope string, actor ks.Actor, ip, action string) string {
	switch scope {
	case "actor":
		if actor.ID.IsNil() {
			return action + "|ip|" + ip
		}
		return action + "|actor|" + string(actor.ID)
	case "tenant":
		return action + "|tenant|" + string(actor.Tenant)
	default:
		return action + "|ip|" + ip
	}
}

// idempotentResponse returns a stored result when the same key and body were seen
// before, and refuses a different body under the same key (§5.7).
func (a *App) idempotentResponse(ctx context.Context, h *Handler, actor ks.Actor, in *Call) ([]byte, bool, error) {
	if in.Key == "" {
		return nil, false, ks.Invalid("Idempotency-Key", "the action requires an idempotency key header")
	}
	hash := hashBytes(append([]byte(in.Key), in.Body...))
	var stored []byte
	var seenHash []byte
	err := a.Pool.QueryRow(ctx,
		`SELECT response, request_hash FROM ks_idempotency WHERE action=$1 AND tenant_id=$2 AND key=$3`,
		h.Ref, actor.Tenant.OrNil(), in.Key).Scan(&stored, &seenHash)
	switch {
	case errors.Is(err, pgNoRows()):
		return nil, false, nil
	case err != nil:
		return nil, false, translate(err)
	}
	if !bytes.Equal(seenHash, hash) {
		return nil, false, ks.Conflict()
	}
	return stored, true, nil
}

func (a *App) storeIdempotent(ctx context.Context, h *Handler, actor ks.Actor, in *Call, out HandlerOutput) error {
	err := a.inSystemScope(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO ks_idempotency (id, action, tenant_id, key, request_hash, response, created_at, expires_at)
	         VALUES (ks_new_id(),$1,$2,$3,$4,$5,now(),now() + interval '1 day') ON CONFLICT (action, tenant_id, key) DO NOTHING`,
			h.Ref, actor.Tenant.OrNil(), in.Key, hashBytes(append([]byte(in.Key), in.Body...)), out.Body)
		return err
	})
	return translate(err)
}

func hashBytes(b []byte) []byte {
	sum := sha256.Sum256(b)
	return sum[:]
}

// Cache is the in-process response cache; the shared tier is a Postgres table (§11.3).
type Cache struct {
	mu    sync.Mutex
	items map[string]cacheItem
	Gen   map[string]int64
}

type cacheItem struct {
	body []byte
	exp  time.Time
	gen  int64
}

func newCache() *Cache {
	return &Cache{items: map[string]cacheItem{}, Gen: map[string]int64{}}
}

// cacheKey includes the tenant and the actor's role set, so an entry can never cross
// an access boundary (§11.3).
func cacheKey(h *Handler, actor ks.Actor, body []byte) string {
	roles := strings.Join(actor.SortedRoles(), ",")
	sum := sha256.Sum256(append([]byte(h.Ref+"|"+string(actor.Tenant)+"|"+roles+"|"), body...))
	return h.Ref + "|" + string(actor.Tenant) + "|" + base64.RawURLEncoding.EncodeToString(sum[:8])
}

func (c *Cache) get(key string, ttl time.Duration, gen int64) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	it, ok := c.items[key]
	if !ok || time.Now().After(it.exp) || it.gen != gen {
		return nil, false
	}
	return it.body, true
}

func (c *Cache) put(key string, body []byte, ttl time.Duration, gen int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.items[key] = cacheItem{body: body, exp: time.Now().Add(ttl), gen: gen}
}

func (c *Cache) bump(resource string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.Gen[resource]++
}

func (c *Cache) generation(resource string) int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.Gen[resource]
}

// observe records request metrics.
func (a *App) observe(ref string, start time.Time, err error) {
	d := time.Since(start)
	a.Registry.observe(ref, float64(d.Microseconds())/1000.0)
	if err != nil {
		a.Registry.inc("ks_requests_error", map[string]string{"action": ref, "code": ks.AsError(err).Code})
	}
	a.Registry.inc("ks_requests", map[string]string{"action": ref})
}

// Clock is the app-wide clock; tests replace it (§7.4).
type Clock = ks.Clock

// SystemClock reads the wall clock.
type SystemClock = ks.SystemClock

func pprofIndex(w http.ResponseWriter, r *http.Request) {
	// The framework exposes the standard profiles only when the binary links
	// net/http/pprof; ks builds do, on the admin port alone (§11.4).
	http.NotFound(w, r)
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// Call is one run of one action: the transaction, the actor and the request's own data.
// The generated adapter takes nothing else, which is what keeps an action's signature free
// of framework types (§7.2).
// HandlerInput is what the pipeline hands a generated adapter: identity, the request's
// data, and the transaction the action runs inside.
type HandlerInput struct {
	Actor      ks.Actor
	Body       []byte
	Query      map[string][]string
	Headers    map[string]string
	IP         string
	Key        string
	DB         *DB
	Job        *JobCall
	Invocation ks.ID
	Trace      string
}

type Call struct {
	Context    context.Context
	Actor      ks.Actor
	DB         *DB
	Log        *slog.Logger
	Clock      ks.Clock
	Body       []byte
	Query      map[string][]string
	Headers    map[string]string
	IP         string
	Key        string // Idempotency-Key
	Job        *JobCall
	Invocation ks.ID
	Trace      string
	App        *App
}

// Trace is what one handler execution did: the statements it ran and the effects it
// queued. App.OnCall receives one per attempt, which is how ks test counts queries and
// captures effects, and how ks dev serves /_ks/traces (§16.1, §17.4).
type Trace struct {
	ID         string       `json:"traceId"`
	Invocation ks.ID        `json:"invocationId"`
	Action     string       `json:"action"`
	Actor      ks.Actor     `json:"actor"`
	Start      time.Time    `json:"start"`
	Millis     float64      `json:"millis"`
	Err        error        `json:"-"`
	Stmts      []Stmt       `json:"queries"`
	Effects    []EffectCall `json:"effects"`
}

// Encode turns what an action returned into the response body. A nil result is a 204 with
// no body, which is what a delete should look like.
func Encode(v any) ([]byte, error) {
	if v == nil {
		return nil, nil
	}
	return json.Marshal(v)
}
