package runtime

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"keystone.dev/keystone/ks"
	"keystone.dev/keystone/model"
)

// DevMode reports whether the introspection API is on. Only the environment can turn it
// on, never a config file, so a production deploy cannot expose it by accident (§17.4).
func DevMode() bool { return os.Getenv("KS_DEV") == "1" }

const devKeep = 500 // traces and log lines kept in memory

// devAPI serves /_ks/* in development: actions, call-as-actor, traces, logs and reset.
type devAPI struct {
	a      *App
	mu     sync.Mutex
	traces map[string]Trace
	order  []string
	logs   []devLog
	seq    int64
}

type devLog struct {
	Seq   int64          `json:"seq"`
	Time  string         `json:"time"`
	Level string         `json:"level"`
	Msg   string         `json:"msg"`
	Attrs map[string]any `json:"attrs,omitempty"`
}

// enableDev installs the trace hook and a log tee, and returns the handler.
func (a *App) enableDev() *devAPI {
	d := &devAPI{a: a, traces: map[string]Trace{}}
	prev := a.OnCall
	a.OnCall = func(t Trace) {
		if prev != nil {
			prev(t)
		}
		d.addTrace(t)
	}
	a.Log = slog.New(devTee{Handler: a.Log.Handler(), d: d})
	return d
}

func (d *devAPI) addTrace(t Trace) {
	if t.ID == "" {
		t.ID = string(t.Invocation)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.traces[t.ID]; !ok {
		d.order = append(d.order, t.ID)
	}
	d.traces[t.ID] = t
	if len(d.order) > devKeep {
		delete(d.traces, d.order[0])
		d.order = d.order[1:]
	}
}

// devTee copies every record into the dev log buffer and passes it on.
type devTee struct {
	slog.Handler
	d     *devAPI
	attrs []slog.Attr
}

func (t devTee) Handle(ctx context.Context, r slog.Record) error {
	entry := devLog{Time: r.Time.Format("2006-01-02T15:04:05.000Z07:00"), Level: r.Level.String(), Msg: r.Message, Attrs: map[string]any{}}
	for _, a := range t.attrs {
		entry.Attrs[a.Key] = a.Value.Any()
	}
	r.Attrs(func(a slog.Attr) bool {
		entry.Attrs[a.Key] = a.Value.Any()
		return true
	})
	t.d.mu.Lock()
	t.d.seq++
	entry.Seq = t.d.seq
	t.d.logs = append(t.d.logs, entry)
	if len(t.d.logs) > devKeep {
		t.d.logs = t.d.logs[1:]
	}
	t.d.mu.Unlock()
	return t.Handler.Handle(ctx, r)
}

func (t devTee) WithAttrs(as []slog.Attr) slog.Handler {
	return devTee{Handler: t.Handler.WithAttrs(as), d: t.d, attrs: append(append([]slog.Attr{}, t.attrs...), as...)}
}

func (t devTee) WithGroup(name string) slog.Handler {
	return devTee{Handler: t.Handler.WithGroup(name), d: t.d, attrs: t.attrs}
}

func (d *devAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/_ks")
	switch {
	case path == "/actions" && r.Method == http.MethodGet:
		d.actions(w)
	case path == "/call" && r.Method == http.MethodPost:
		d.call(w, r)
	case strings.HasPrefix(path, "/traces/") && r.Method == http.MethodGet:
		d.mu.Lock()
		t, ok := d.traces[strings.TrimPrefix(path, "/traces/")]
		d.mu.Unlock()
		if !ok {
			writeError(w, r, ks.NotFound(), "")
			return
		}
		devJSON(w, http.StatusOK, devTrace(t))
	case path == "/logs" && r.Method == http.MethodGet:
		since, _ := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
		d.mu.Lock()
		out := []devLog{}
		for _, l := range d.logs {
			if l.Seq > since {
				out = append(out, l)
			}
		}
		next := d.seq
		d.mu.Unlock()
		devJSON(w, http.StatusOK, map[string]any{"logs": out, "next": next})
	case path == "/reset" && r.Method == http.MethodPost:
		if err := d.a.ResetToSeed(r.Context()); err != nil {
			writeError(w, r, ks.InternalError(err), "")
			return
		}
		devJSON(w, http.StatusOK, map[string]any{"reset": true})
	default:
		writeError(w, r, ks.NotFound(), "")
	}
}

func devTrace(t Trace) map[string]any {
	out := map[string]any{"trace": t}
	if t.Err != nil {
		out["error"] = ks.AsError(t.Err)
	}
	return out
}

func devJSON(w http.ResponseWriter, status int, v any) {
	raw, _ := json.MarshalIndent(v, "", "  ")
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(raw)
}

func (d *devAPI) actions(w http.ResponseWriter) {
	type entry struct {
		Action   string        `json:"action"`
		Kind     string        `json:"kind"`
		Path     string        `json:"path,omitempty"`
		Public   bool          `json:"public,omitempty"`
		Requires any           `json:"requires,omitempty"`
		Input    []model.Field `json:"input"`
		Errors   any           `json:"errors,omitempty"`
	}
	var out []entry
	for ref, h := range d.a.Handlers {
		out = append(out, entry{Action: ref, Kind: string(h.Kind), Path: h.Path, Public: h.Spec.Public,
			Requires: h.Spec.Requires, Input: h.Spec.Input, Errors: h.Spec.Errors})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Action < out[j].Action })
	devJSON(w, http.StatusOK, out)
}

// call runs an action as an actor the caller names, through the full pipeline.
func (d *devAPI) call(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Action string          `json:"action"`
		Actor  ks.Actor        `json:"actor"`
		Input  json.RawMessage `json:"input"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, r, ks.Invalid("body", err.Error()), "")
		return
	}
	h, ok := d.a.HandlerFor(req.Action)
	if !ok {
		writeError(w, r, ks.NotFound(), "")
		return
	}
	actor := req.Actor
	if actor.Kind == "" {
		actor.Kind = ks.ActorUser
	}
	actor.Authed = actor.Kind != ks.ActorAnonymous
	trace := string(ks.NewID())
	if len(req.Input) == 0 {
		req.Input = json.RawMessage("{}")
	}
	out, err := d.a.Run(r.Context(), h, actor, &Call{Actor: actor, Body: req.Input, Trace: trace},
		d.a.Log.With("traceId", trace, "action", h.Ref))
	resp := map[string]any{"traceId": trace}
	if err != nil {
		resp["error"] = ks.AsError(err)
	} else if len(out.Body) > 0 {
		resp["output"] = json.RawMessage(out.Body)
	}
	devJSON(w, http.StatusOK, resp)
}

// ResetToSeed empties every application table and loads the seed/*.sql files in name
// order, as the system across tenants (§17.4).
func (a *App) ResetToSeed(ctx context.Context) error {
	var tables []string
	for _, fw := range a.Model.Framework() {
		tables = append(tables, quoteTable(fw.Table))
	}
	for _, r := range a.Model.AllResources() {
		tables = append(tables, quoteTable(r.Resource.Table))
	}
	for _, t := range []string{"ks_session", "ks_api_token", "ks_identity_link", "ks_outbox", "ks_jobs",
		"ks_scheduled_runs", "ks_invocations", "ks_audit", "ks_idempotency", "ks_rate_bucket", "ks_cache_entry"} {
		tables = append(tables, t)
	}
	seeds, _ := filepath.Glob(filepath.Join(a.Cfg.AppDir, "seed", "*.sql"))
	sort.Strings(seeds)
	tx, err := a.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "SET LOCAL ks.cross_tenant = 'on'; SET LOCAL ks.actor_system = 'on'"); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, "TRUNCATE "+strings.Join(tables, ", ")+" CASCADE"); err != nil {
		return err
	}
	for _, f := range seeds {
		raw, err := os.ReadFile(f)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, string(raw)); err != nil {
			return &seedError{file: f, err: err}
		}
	}
	return tx.Commit(ctx)
}

type seedError struct {
	file string
	err  error
}

func (e *seedError) Error() string { return "seed " + e.file + ": " + e.err.Error() }
func (e *seedError) Unwrap() error { return e.err }

func quoteTable(t string) string { return `"` + strings.ReplaceAll(t, `"`, `""`) + `"` }
