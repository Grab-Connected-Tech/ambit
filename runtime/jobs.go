package runtime

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"keystone.dev/keystone/ks"
	"keystone.dev/keystone/model"
)

// JobCall is one queued unit of background work (§10.4).
type JobCall struct {
	ID          ks.ID         `json:"id"`
	Tenant      ks.ID         `json:"tenantId"`
	Action      string        `json:"action"`
	Args        []byte        `json:"args"`
	Priority    int           `json:"priority"`
	RunAfter    time.Time     `json:"runAfter"`
	State       string        `json:"state"`
	Attempts    int           `json:"attempts"`
	MaxAttempts int           `json:"maxAttempts"`
	Base        time.Duration `json:"base"`
	Cap         time.Duration `json:"cap"`
	Backoff     string        `json:"backoff"`
	Concurrency string        `json:"concurrencyKey,omitempty"`
	Error       string        `json:"lastError,omitempty"`
	Trace       string        `json:"traceId,omitempty"`
	Invocation  ks.ID         `json:"invocationId"`
	PayloadSpec []byte        `json:"policy,omitempty"`
}

// Enqueue adds a job to the queue inside the current transaction, which is what makes
// "start work only if the change committed" the default rather than a pattern (§10.1).
func (d *DB) Enqueue(ctx context.Context, call JobCall) error {
	if call.Action == "" {
		return ks.InternalError(errors.New("runtime: Enqueue needs a job action"))
	}
	if call.ID == "" {
		call.ID = ks.NewID()
	}
	if call.Tenant == "" {
		call.Tenant = d.actor.Tenant
	}
	if call.RunAfter.IsZero() {
		call.RunAfter = d.clock.Now()
	}
	if call.MaxAttempts == 0 {
		call.MaxAttempts = 5
	}
	if call.Base == 0 {
		call.Base = time.Second
	}
	if call.Cap == 0 {
		call.Cap = 10 * time.Minute
	}
	if call.Args == nil {
		call.Args = []byte("{}")
	}
	spec := ks.NewW(64 + len(call.Args))
	spec.ObjStart()
	spec.Key("maxAttempts")
	spec.Num(int64(call.MaxAttempts))
	spec.Key("backoff")
	spec.Str(call.Backoff)
	spec.Key("base")
	spec.Str(call.Base.String())
	spec.Key("cap")
	spec.Str(call.Cap.String())
	spec.ObjEnd()
	call.PayloadSpec = spec.Bytes()

	_, err := d.exec(ctx,
		`INSERT INTO ks_jobs (id, tenant_id, action, args, priority, state, run_after, attempts, max_attempts, backoff, base_delay, cap_delay, concurrency_key, invocation_id, policy, created_at)
         VALUES ($1,$2,$3,$4,$5,'queued',$6,0,$7,$8,$9::interval,$10::interval,$11,$12,$13,now())`,
		call.ID.Bytes(), call.Tenant.OrNil(), call.Action, call.Args, call.Priority, call.RunAfter,
		call.MaxAttempts, call.Backoff, seconds(call.Base), seconds(call.Cap), nullString(call.Concurrency),
		call.Invocation.OrNil(), call.PayloadSpec)
	if err != nil {
		return translate(err)
	}
	d.JobsQueued = append(d.JobsQueued, call)
	return nil
}

func seconds(d time.Duration) string {
	return strconv.FormatFloat(d.Seconds(), 'f', -1, 64) + " seconds"
}

// Worker claims and runs jobs. Any number of instances may run it: claims use
// FOR UPDATE SKIP LOCKED (§10.4).
type Worker struct {
	A        *App
	Instance string
	Batch    int
	Poll     time.Duration

	mu     sync.Mutex
	closed bool
}

// NewWorker names this instance in claims and heartbeats.
func (a *App) NewWorker() *Worker {
	return &Worker{A: a, Instance: instanceID(), Batch: 8, Poll: time.Second}
}

// Claim takes the next runnable jobs and marks them running.
func (w *Worker) Claim(ctx context.Context, batch int) ([]JobCall, error) {
	if batch <= 0 {
		batch = w.Batch
	}
	tx, err := w.A.BeginTx(ctx, Scope{IsSystem: true, CrossTenant: true}, 5*time.Second, false)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	rows, err := tx.Query(ctx,
		`UPDATE ks_jobs SET state='running', claimed_by=$1, claimed_at=now(), heartbeat_at=now(), attempts=attempts+1
	    WHERE id IN (
	      SELECT j.id FROM ks_jobs j
	       WHERE j.state='queued' AND j.run_after <= now()
	         AND (j.concurrency_key IS NULL OR NOT EXISTS (
	               SELECT 1 FROM ks_jobs r
	                WHERE r.concurrency_key = j.concurrency_key AND r.state='running' AND r.id <> j.id))
	       ORDER BY j.priority, j.run_after
	       FOR UPDATE SKIP LOCKED
	       LIMIT $2)
	RETURNING id, tenant_id, action, args, priority, state, attempts, max_attempts, base_delay, cap_delay, backoff, coalesce(concurrency_key,''), coalesce(last_error,''), coalesce(invocation_id, '00000000-0000-0000-0000-000000000000'), coalesce(policy,'{}'::jsonb)`,
		w.Instance, batch)
	if err != nil {
		return nil, translate(err)
	}
	var out []JobCall
	for rows.Next() {
		var (
			c                           JobCall
			base, cap                   time.Duration
			policyBytes                 []byte
			tenant, invocation          ks.ID
			prio, attempts, maxAttempts int
		)
		if err := rows.Scan(&c.ID, &tenant, &c.Action, &c.Args, &prio, &c.State, &attempts, &maxAttempts,
			&base, &cap, &c.Backoff, &c.Concurrency, &c.Error, &invocation, &policyBytes); err != nil {
			rows.Close()
			return nil, translate(err)
		}
		c.Tenant, c.Invocation = tenant, invocation
		c.Priority, c.Attempts, c.MaxAttempts = prio, attempts, maxAttempts
		c.Base, c.Cap = base, cap
		c.PayloadSpec = policyBytes
		out = append(out, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, translate(err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, translate(err)
	}
	return out, nil
}

// Run executes one claimed job through the same pipeline a request uses, as the actor
// the job declared (§10.4).
func (w *Worker) Run(ctx context.Context, call JobCall) error {
	h, ok := w.A.HandlerFor(call.Action)
	if !ok {
		return w.finish(ctx, call, fmt.Errorf("no handler for %s", call.Action), true)
	}
	actor := w.actorFor(ctx, call, h)
	err := w.A.run(ctx, h, actor, &Call{
		Body: call.Args,
		Key:  string(call.ID),
		Job:  &call,
	}, w.A.Log.With("job", call.Action))
	if err != nil {
		return w.finish(ctx, call, err, true)
	}
	return w.finish(ctx, call, nil, false)
}

func (w *Worker) actorFor(ctx context.Context, call JobCall, h *Handler) ks.Actor {
	module, name := splitAction(call.Action)
	_, act := w.A.Model.FindAction(module + "." + name)
	a := ks.Actor{Kind: ks.ActorSystem, ID: call.Tenant, Tenant: call.Tenant, Authed: true}
	if act == nil {
		return a
	}
	switch act.RunAs {
	case model.RunAsEnqueuer:
		if snap, err := w.actorSnapshot(ctx, call); err == nil && snap.ID != "" {
			return snap
		}
	}
	roles, err := w.rolesFor(ctx, a.Tenant, a.ID)
	if err == nil {
		a.Roles = roles
	}
	return a
}

// actorSnapshot rebuilds the actor captured when a RunAs(Enqueuer) job was enqueued.
func (w *Worker) actorSnapshot(ctx context.Context, call JobCall) (ks.Actor, error) {
	var a ks.Actor
	var tenant []byte
	var roles []string
	err := w.A.Pool.QueryRow(ctx,
		`SELECT coalesce(actor_id,'00000000-0000-0000-0000-000000000000'), coalesce(tenant_id,'00000000-0000-0000-0000-000000000000'), coalesce(actor_roles,'{}')
		   FROM ks_invocations WHERE id=$1`, call.Invocation).
		Scan(&a.ID, &tenant, &roles)
	if err != nil {
		return ks.Actor{}, translate(err)
	}
	a.Tenant = ks.ID(hexUUID(tenant))
	a.Roles = roles
	a.Kind = ks.ActorUser
	a.Authed = true
	a.Snapshot = true
	return a, nil
}

func (w *Worker) rolesFor(ctx context.Context, tenant, actor ks.ID) ([]string, error) {
	// The system actor holds every role within one tenant (§5.7).
	if tenant.IsNil() {
		return w.A.Model.Roles, nil
	}
	rows, err := w.A.Pool.Query(ctx,
		`SELECT role FROM ks_membership WHERE tenant_id=$1 AND actor_id=$2`, tenant, actor)
	if err != nil {
		return nil, translate(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var r string
		if err := rows.Scan(&r); err != nil {
			return nil, translate(err)
		}
		out = append(out, r)
	}
	if len(out) == 0 {
		return w.A.Model.Roles, nil
	}
	return out, nil
}

func (w *Worker) finish(ctx context.Context, call JobCall, err error, mayRetry bool) error {
	state := "done"
	delay := time.Duration(0)
	message := ""
	if err != nil {
		message = truncate(err.Error(), 900)
		state = "dead_letter"
		if mayRetry && call.Attempts < call.MaxAttempts {
			state = "queued"
			delay = backoff(call.Attempts, maxd(call.Base, time.Second), maxd(call.Cap, time.Hour))
		}
		w.A.metric("ks_job_"+state, 1)
	}
	sql := `UPDATE ks_jobs SET state=$2, last_error=$3, finished_at=CASE WHEN $2 IN ('done','dead_letter') THEN now() END`
	args := []any{call.ID, state, message}
	if state == "queued" {
		sql += `, run_after = now() + $4::interval, claimed_by = NULL, claimed_at = NULL`
		args = append(args, seconds(delay))
	}
	sql += ` WHERE id=$1`
	_, qerr := w.A.Pool.Exec(ctx, sql, args...)
	if qerr != nil {
		return translate(qerr)
	}
	return err
}

func maxd(a, b time.Duration) time.Duration {
	if a > b {
		return a
	}
	return b
}

// Run polls the queue until the context ends.
func (w *Worker) RunLoop(ctx context.Context) {
	t := time.NewTicker(w.Poll)
	defer t.Stop()
	for ctx.Err() == nil {
		jobs, err := w.Claim(ctx, 0)
		if err != nil {
			w.A.Log.Error("job claim", "err", err)
		}
		for _, j := range jobs {
			if err := w.Run(ctx, j); err != nil && ctx.Err() == nil {
				w.A.Log.Warn("job failed", "action", j.Action, "attempt", j.Attempts, "err", err)
			}
		}
		if len(jobs) > 0 {
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-w.A.wake:
		}
	}
}

// Schedules enqueue jobs on cron expressions. A leader elected with an advisory lock
// owns the ticks, and each run gets a deterministic slot key so a restart cannot fire
// twice for the same minute (§10.5).
type Schedules struct {
	A      *App
	Zone   *time.Location
	Slots  map[string]bool
	Leader bool
}

const scheduleLockKey = 6182374

func (a *App) NewSchedules() *Schedules {
	return &Schedules{A: a, Zone: time.UTC, Slots: map[string]bool{}}
}

// TryLeader takes the advisory lock if it is free.
func (s *Schedules) TryLeader(ctx context.Context) {
	var got bool
	if err := s.A.Pool.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", scheduleLockKey).Scan(&got); err != nil {
		return
	}
	s.Leader = got
}

// Unlock releases the leader lock.
func (s *Schedules) Unlock(ctx context.Context) {
	if !s.Leader {
		return
	}
	_, _ = s.A.Pool.Exec(ctx, "SELECT pg_advisory_unlock($1)", scheduleLockKey)
	s.Leader = false
}

// Tick enqueues every schedule whose cron matches the current minute.
func (s *Schedules) Tick(ctx context.Context, now time.Time) (int, error) {
	if !s.Leader {
		s.TryLeader(ctx)
	}
	if !s.Leader {
		return 0, nil
	}
	now = now.In(s.Zone)
	slot := now.Format("2006-01-02T15:04")
	n := 0
	for _, m := range s.A.Model.Modules {
		for i := range m.Actions {
			act := &m.Actions[i]
			if act.Kind != model.KindSchedule || !CronMatches(act.Cron, now) {
				continue
			}
			key := act.Name + "@" + slot
			if s.Slots[key] {
				continue
			}
			s.Slots[key] = true
			var exists bool
			err := s.A.Pool.QueryRow(ctx,
				`SELECT EXISTS (SELECT 1 FROM ks_scheduled_runs WHERE schedule=$1 AND slot=$2 AND tenant_id IS NULL)`,
				model.Qual(m.Name, act.Name), slot).Scan(&exists)
			if err == nil && exists {
				continue
			}
			if _, err := s.A.Pool.Exec(ctx,
				`INSERT INTO ks_scheduled_runs (id, schedule, slot, created_at) VALUES ($1,$2,$3,now()) ON CONFLICT DO NOTHING`,
				ks.NewID(), model.Qual(m.Name, act.Name), slot); err != nil {
				return n, translate(err)
			}
			job := JobCall{Action: act.Runs, Tenant: ks.NilID, RunAfter: now, Invocation: ks.NewID(), ID: ks.NewID()}
			if err := s.enqueueJobNoTx(ctx, job); err != nil {
				return n, err
			}
			n++
		}
	}
	return n, nil
}

func (s *Schedules) enqueueJobNoTx(ctx context.Context, call JobCall) error {
	if call.Args == nil {
		call.Args = []byte("{}")
	}
	if call.MaxAttempts == 0 {
		call.MaxAttempts = 5
	}
	err := s.A.inSystemScope(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO ks_jobs (id, tenant_id, action, args, priority, state, run_after, attempts, max_attempts, backoff, base_delay, cap_delay, invocation_id, policy, created_at)
	         VALUES ($1,$2,$3,$4,5,'queued',now(),0,$5,'exponential','1 second','10 minutes',$6,'{}'::jsonb,now())`,
			call.ID, nil, call.Action, call.Args, call.MaxAttempts, call.Invocation)
		return err
	})
	return translate(err)
}

// RunLoop ticks once a minute.
func (s *Schedules) RunLoop(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for ctx.Err() == nil {
		if _, err := s.Tick(ctx, time.Now()); err != nil {
			s.A.Log.Error("schedule tick", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// CronMatches evaluates a five-field cron expression against a time in UTC (§10.5).
func CronMatches(expr string, t time.Time) bool {
	fields := strings.Fields(expr)
	if len(fields) != 5 {
		return false
	}
	values := []int{t.Minute(), t.Hour(), t.Day(), int(t.Month()), int(t.Weekday())}
	for i, f := range fields {
		if !fieldMatches(f, values[i], i == 4) {
			return false
		}
	}
	return true
}

func fieldMatches(f string, v int, weekday bool) bool {
	if weekday && v == 7 {
		v = 0
	}
	for _, part := range strings.Split(f, ",") {
		step := 1
		if s := strings.IndexByte(part, '/'); s >= 0 {
			n, err := strconv.Atoi(part[s+1:])
			if err != nil || n <= 0 {
				return false
			}
			step, part = n, part[:s]
		}
		if part == "*" {
			if step > 1 {
				return v%step == 0
			}
			return true
		}
		if lo, hi, ok := strings.Cut(part, "-"); ok {
			a, b := atoi(lo), atoi(hi)
			if v < a || v > b {
				continue
			}
			if step > 1 && (v-a)%step != 0 {
				continue
			}
			return true
		}
		if atoi(part) == v {
			return true
		}
	}
	return false
}

func atoi(s string) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		return -1
	}
	return n
}

func hexUUID(b []byte) string {
	if len(b) != 16 {
		return ""
	}
	const d = "0123456789abcdef"
	out := make([]byte, 36)
	pos := 0
	for i, v := range b {
		out[pos] = d[v>>4]
		out[pos+1] = d[v&0x0f]
		pos += 2
		if i == 3 || i == 5 || i == 7 || i == 9 {
			out[pos] = '-'
			pos++
		}
	}
	return string(out)
}

var _ = sha256.New
var _ = model.RunAsSystem
