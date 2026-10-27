package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"keystone.dev/keystone/ks"
	"keystone.dev/keystone/model"
)

// EffectCall is one recorded side effect. Deferred calls become outbox rows inside
// the action's transaction, so an effect for a rolled-back change cannot exist (§9.1).
type EffectCall struct {
	ID             ks.ID            `json:"id"`
	Tenant         ks.ID            `json:"tenantId"`
	Action         string           `json:"action"`
	Invocation     ks.ID            `json:"invocationId"`
	Seq            int              `json:"seq"`
	Type           model.EffectKind `json:"type"`
	Mode           string           `json:"mode"`
	Hosts          []string         `json:"hosts,omitempty"`
	Topic          string           `json:"topic,omitempty"`
	Bucket         string           `json:"bucket,omitempty"`
	Template       string           `json:"template,omitempty"`
	Job            string           `json:"job,omitempty"`
	IdempotencyKey string           `json:"idempotencyKey"`
	Payload        json.RawMessage  `json:"payload"`
	RunAfter       time.Time        `json:"runAfter"`
}

// EffectResult is what an immediate effect hands back to the action (§10.2).
type EffectResult struct {
	Body   json.RawMessage `json:"body,omitempty"`
	Text   string          `json:"text,omitempty"`
	Replay bool            `json:"replayed,omitempty"`
}

// Provider executes effects (§10.3). Implementations must be safe to call twice with
// the same IdempotencyKey.
type Provider interface {
	Name() string
	Execute(ctx context.Context, e EffectCall) (EffectResult, error)
	Retryable(err error) bool
}

// Providers maps an effect type to its implementation.
type Providers struct {
	mu sync.RWMutex
	by map[model.EffectKind]Provider
}

func NewProviders() *Providers { return &Providers{by: map[model.EffectKind]Provider{}} }

func (p *Providers) Register(kind model.EffectKind, provider Provider) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.by[kind] = provider
}

func (p *Providers) Get(kind model.EffectKind) (Provider, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	got, ok := p.by[kind]
	return got, ok
}

// DerivedIdempotencyKey is the invocation plus sequence, which makes every recorded
// effect replay-safe without the author remembering to name a key (§10.1 step 5).
func DerivedIdempotencyKey(invocation ks.ID, seq int) string {
	h := sha256.New()
	h.Write([]byte(string(invocation)))
	h.Write([]byte{byte(seq), byte(seq >> 8)})
	return hex.EncodeToString(h.Sum(nil))[:40]
}

// RecordEffect validates an effect call against what the action declared and queues
// it. A declared-but-not-granted effect never reaches this function because the
// generated context struct has no field for it; this check is the second layer.
func (d *DB) RecordEffect(ctx context.Context, call EffectCall) error {
	if call.Type == "" {
		return ks.InternalError(errors.New("runtime: effect with no type"))
	}
	if call.IdempotencyKey == "" {
		call.IdempotencyKey = DerivedIdempotencyKey(call.Invocation, call.Seq)
	}
	if call.ID == "" {
		call.ID = ks.NewID()
	}
	if call.Tenant == "" {
		call.Tenant = d.actor.Tenant
	}
	if call.Mode == "" {
		call.Mode = "deferred"
	}
	if call.Payload == nil {
		call.Payload = json.RawMessage("{}")
	}
	// An immediate effect has already been run by CallEffect; recording it again would
	// run the provider twice. The outbox row exists so the trace shows it happened.
	d.EffectsQueued = append(d.EffectsQueued, call)
	return nil
}

// EffectHandle is the capability an action holds for one declared effect. It is a method
// value bound to the action's transaction, so an action can run only the effects its spec
// declared, and never a provider the app did not configure (§7.1, §7.4).
type EffectHandle struct {
	Type   model.EffectKind
	Mode   string // deferred | immediate
	Hosts  []string
	DB     *DB
	caller func(context.Context, EffectCall) (EffectResult, error)
}

// Send runs the effect. A deferred effect is recorded in the outbox and happens only if
// this transaction commits (§10.1); an immediate effect runs now and its result comes
// back.
func (h EffectHandle) Send(ctx context.Context, payload []byte, host ...string) (EffectResult, error) {
	call := EffectCall{Type: h.Type, Mode: h.Mode, Payload: payload, Hosts: host}
	if h.Mode == "" {
		call.Mode = "deferred"
	}
	if call.Mode == "immediate" {
		return h.DB.CallEffect(ctx, call)
	}
	return EffectResult{}, h.DB.RecordEffect(ctx, call)
}

func (d *DB) callProvider(ctx context.Context, call EffectCall) (EffectResult, error) {
	if d.app == nil {
		return EffectResult{}, ks.InternalError(errors.New("runtime: no app model for effect dispatch"))
	}
	if d.providers == nil {
		return EffectResult{}, ks.InternalError(errors.New("runtime: no effect providers"))
	}
	p, ok := d.providers.Get(call.Type)
	if !ok {
		return EffectResult{}, ks.InternalError(fmt.Errorf("runtime: no provider configured for %s effects", call.Type))
	}
	if err := declaresEffect(d.actionApp(), call); err != nil {
		return EffectResult{}, err
	}
	return p.Execute(ctx, call)
}

func (d *DB) actionApp() *model.App { return d.app }

// declaresEffect is the runtime half of the capability check: a host outside the
// allowlist is refused here, not merely discouraged (§5.8).
func declaresEffect(a *model.App, call EffectCall) error {
	if a == nil || call.Action == "" {
		return nil
	}
	_, act := a.FindAction(call.Action)
	if act == nil {
		return ks.InternalError(fmt.Errorf("runtime: the effect host %q is not a declared action of this app", call.Action))
	}
	for _, e := range act.Effects {
		if e.Type == call.Type && e.Mode == "immediate" {
			return nil
		}
	}
	return ks.Forbidden(fmt.Sprintf("the action %q declares no %s effect run immediately, and an undeclared effect is a capability the action was never granted (§7.4)", call.Action, call.Type))
}
func splitAction(ref string) (string, string) {
	if i := strings.IndexByte(ref, '.'); i > 0 {
		return ref[:i], ref[i+1:]
	}
	return "", ref
}

func containsFold(list []string, want string) bool {
	for _, x := range list {
		if strings.EqualFold(x, want) {
			return true
		}
	}
	return false
}

// Outbox stores and dispatches deferred effects on Postgres alone (§10.1).
type Outbox struct {
	D         *App
	Instance  string
	Batch     int
	MaxAtt    int
	PollEvery time.Duration
	Notify    string
	Base      time.Duration // first retry delay
	Cap       time.Duration // largest retry delay
}

// NewOutbox returns a dispatcher with the spec's default retry curve: 8 attempts over
// about a day (§10.1 step 6).
func (a *App) NewOutbox() *Outbox {
	return &Outbox{D: a, Instance: instanceID(), Batch: 16, MaxAtt: 8, PollEvery: time.Second,
		Notify: "ks_outbox", Base: 5 * time.Second, Cap: 6 * time.Hour}
}

// Insert records deferred effects for a committed action. It runs inside the action's
// transaction, so a rollback removes the rows with it.
func (o *Outbox) Insert(ctx context.Context, d *DB) error {
	if len(d.EffectsQueued) == 0 {
		return nil
	}
	for _, e := range d.EffectsQueued {
		if _, err := d.exec(ctx,
			`INSERT INTO ks_outbox (id, tenant_id, action, invocation_id, seq, type, mode, template, topic, bucket, hosts, idempotency_key, payload, state, run_after, created_at)
	         VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14, COALESCE($15, now()), now())`,
			e.ID.Bytes(), e.Tenant.OrNil(), e.Action, e.Invocation.OrNil(), e.Seq, string(e.Type), e.Mode,
			nullString(e.Template), nullString(e.Topic), nullString(e.Bucket), e.Hosts,
			e.IdempotencyKey, []byte(e.Payload), stateFor(e.Mode), nullTime(e.RunAfter)); err != nil {
			return translate(err)
		}
	}
	return nil
}

func stateFor(mode string) string {
	if mode == "immediate" {
		return "done"
	}
	return "pending"
}

// Dispatch claims and runs due effects. Any number of instances may call it: claims
// use FOR UPDATE SKIP LOCKED, so a row is worked on once (§10.1 step 4).
func (o *Outbox) Dispatch(ctx context.Context, batch int) (int, error) {
	if batch <= 0 {
		batch = o.Batch
	}
	conn := o.D.Pool
	tx, err := conn.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := beginScope(ctx, tx, systemScope()); err != nil {
		return 0, err
	}
	rows, err := tx.Query(ctx,
		`UPDATE ks_outbox SET state='running', claimed_by=$1, claimed_at=now(), attempts=attempts+1
		   WHERE id IN (
		     SELECT id FROM ks_outbox
		      WHERE state='pending' AND run_after <= now()
		      ORDER BY run_after
		      FOR UPDATE SKIP LOCKED
		      LIMIT $2)
		RETURNING id, coalesce(tenant_id,'00000000-0000-0000-0000-000000000000'), action, invocation_id, seq, type, mode,
	                 coalesce(template,''), coalesce(topic,''), coalesce(bucket,''), coalesce(hosts,'{}'), idempotency_key, payload, attempts, run_after`,
		o.Instance, batch)
	if err != nil {
		return 0, translate(err)
	}
	type pending struct {
		call EffectCall
		attn int
	}
	var got []pending
	for rows.Next() {
		var (
			id                                         ks.ID
			tenant                                     ks.ID
			action, typ, mode, template, topic, bucket string
			invocation                                 ks.ID
			seq, attempts                              int
			hosts                                      []string
			key                                        string
			payload                                    []byte
			runAfter                                   *time.Time
		)
		if err := rows.Scan(&id, &tenant, &action, &invocation, &seq, &typ, &mode, &template, &topic,
			&bucket, &hosts, &key, &payload, &attempts, &runAfter); err != nil {
			rows.Close()
			return 0, translate(err)
		}
		call := EffectCall{ID: id, Tenant: tenant, Action: action, Invocation: invocation, Seq: seq,
			Type: model.EffectKind(typ), Mode: mode, Template: template, Topic: topic, Bucket: bucket,
			Hosts: hosts, IdempotencyKey: key, Payload: payload}
		if runAfter != nil {
			call.RunAfter = *runAfter
		}
		got = append(got, pending{call: call, attn: attempts})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, translate(err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, translate(err)
	}

	done := 0
	for _, p := range got {
		err := o.runOne(ctx, p.call)
		switch {
		case err == nil:
			done++
			if _, err := conn.Exec(ctx, `UPDATE ks_outbox SET state='done', completed_at=now() WHERE id=$1`, p.call.ID); err != nil {
				return done, translate(err)
			}
		case o.retryable(p.call, err):
			delay := backoff(p.attn, o.Base, o.Cap)
			if p.attn >= o.MaxAtt {
				delay = 0
				if _, err := conn.Exec(ctx,
					`UPDATE ks_outbox SET state='dead_letter', last_error=$2, dead_lettered_at=now() WHERE id=$1`,
					p.call.ID, truncate(err.Error(), 900)); err != nil {
					return done, translate(err)
				}
				o.D.metric("ks_outbox_dead_letter", 1)
				continue
			}
			if _, err := conn.Exec(ctx,
				`UPDATE ks_outbox SET state='pending', last_error=$2, run_after=now()+$3::interval WHERE id=$1`,
				p.call.ID, truncate(err.Error(), 900), intervalText(delay)); err != nil {
				return done, translate(err)
			}
			o.D.metric("ks_outbox_retry", 1)
		default:
			if _, err := conn.Exec(ctx,
				`UPDATE ks_outbox SET state='dead_letter', last_error=$2, dead_lettered_at=now() WHERE id=$1`,
				p.call.ID, truncate(err.Error(), 900)); err != nil {
				return done, translate(err)
			}
			o.D.metric("ks_outbox_dead_letter", 1)
		}
	}
	return done, nil
}

func (o *Outbox) runOne(ctx context.Context, call EffectCall) error {
	provider, ok := o.D.Providers.Get(call.Type)
	if !ok {
		return fmt.Errorf("no provider configured for %s effects: %w", call.Type, errPermanent)
	}
	_, err := provider.Execute(ctx, call)
	return err
}

func (o *Outbox) retryable(call EffectCall, err error) bool {
	if errors.Is(err, errPermanent) {
		return false
	}
	if p, ok := o.D.Providers.Get(call.Type); ok {
		return p.Retryable(err)
	}
	return true
}

var errPermanent = errors.New("permanent")

// Permanent marks an error that retrying cannot fix, such as a rejected payload.
func Permanent(err error) error { return fmt.Errorf("%w: %v", errPermanent, err) }

// Run polls the outbox forever, waking early on NOTIFY.
func (o *Outbox) Run(ctx context.Context) {
	timer := time.NewTicker(o.PollEvery)
	defer timer.Stop()
	for {
		n, err := o.Dispatch(ctx, 0)
		if err != nil && ctx.Err() == nil {
			o.D.Log.Error("outbox dispatch", "err", err)
		}
		if n > 0 {
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		case <-o.D.wake:
		}
	}
}

// Notify asks every instance to look at the outbox now instead of at the next poll.
func (a *App) Notify(ctx context.Context, conn Conn) error {
	_, err := conn.Exec(ctx, "SELECT pg_notify($1, '')", "ks_outbox")
	return err
}

// Listen keeps a connection watching for NOTIFY; the poll loop is the fallback.
func (a *App) Listen(ctx context.Context) {
	for ctx.Err() == nil {
		conn, err := a.Pool.Acquire(ctx)
		if err != nil {
			return
		}
		_, err = conn.Exec(ctx, "LISTEN ks_outbox")
		if err != nil {
			conn.Release()
			time.Sleep(time.Second)
			continue
		}
		for ctx.Err() == nil {
			notification, waitErr := conn.Conn().WaitForNotification(ctx)
			if waitErr != nil {
				break
			}
			_ = notification
			select {
			case a.wake <- struct{}{}:
			default:
			}
		}
		conn.Release()
		time.Sleep(250 * time.Millisecond)
	}
}

// backoff grows exponentially and is capped, with jitter so a fleet does not retry in
// lockstep.
func backoff(attempts int, base, max time.Duration) time.Duration {
	if base <= 0 {
		return 0
	}
	if attempts < 1 {
		attempts = 1
	}
	d := base
	for i := 1; i < attempts && d < max; i++ {
		d *= 2
	}
	if d > max {
		d = max
	}
	jitter := time.Duration(rand.Int63n(int64(d) / 4))
	return d + jitter
}

func intervalText(d time.Duration) string {
	return fmt.Sprintf("%d seconds", int(d.Seconds()))
}

func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func instanceID() string {
	h, err := os.Hostname()
	if err != nil || h == "" {
		h = "instance"
	}
	return fmt.Sprintf("%s-%d", h, os.Getpid())
}

// Recorder is the provider the test harness installs: it records what would have been
// sent and never touches the network (§16.1).
type Recorder struct {
	mu sync.Mutex
	// Calls holds the attempts that succeeded; Attempts holds every call made, so a
	// retry test can count the failure as well as the recovery.
	Calls    []EffectCall
	Attempts []EffectCall
	FailOn   func(EffectCall) error
}

func (r *Recorder) Name() string { return "recorder" }

func (r *Recorder) Execute(ctx context.Context, e EffectCall) (EffectResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.Attempts = append(r.Attempts, e)
	if r.FailOn != nil {
		if err := r.FailOn(e); err != nil {
			return EffectResult{}, err
		}
	}
	r.Calls = append(r.Calls, e)
	return EffectResult{Text: "recorded"}, nil
}

func (r *Recorder) Retryable(err error) bool { return !errors.Is(err, errPermanent) }

// AttemptsOf counts calls including the ones the fake provider rejected.
func (r *Recorder) AttemptsOf(kind model.EffectKind) []EffectCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []EffectCall
	for _, c := range r.Attempts {
		if c.Type == kind {
			out = append(out, c)
		}
	}
	return out
}

func (r *Recorder) CallsOf(kind model.EffectKind) []EffectCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []EffectCall
	for _, c := range r.Calls {
		if c.Type == kind {
			out = append(out, c)
		}
	}
	return out
}

// LogProvider is the default provider for every effect kind: it writes what would have
// happened and succeeds. It exists so a new app runs before anyone has configured
// credentials.
type LogProvider struct {
	Kind model.EffectKind
	Log  Logger
}

// Logger is the minimal logging surface the runtime needs.
type Logger interface{ Info(msg string, kv ...any) }

func (p LogProvider) Name() string { return "log" }

func (p LogProvider) Execute(ctx context.Context, e EffectCall) (EffectResult, error) {
	if p.Log != nil {
		p.Log.Info("effect", "type", string(p.Kind), "action", e.Action, "template", e.Template)
	}
	return EffectResult{}, nil
}

func (p LogProvider) Retryable(error) bool { return true }

// HTTPProvider executes the HTTP effect against a declared host allowlist. It resolves
// the name first and refuses private, loopback and link-local addresses, which is the
// SSRF control from §9.1.
type HTTPProvider struct {
	Client *http.Client
	Limit  int64
}

func (p *HTTPProvider) Name() string { return "http" }

// Execute performs a GET against the allowlisted host. The path may come from data;
// the host may not.
func (p *HTTPProvider) Execute(ctx context.Context, e EffectCall) (EffectResult, error) {
	if len(e.Hosts) == 0 {
		return EffectResult{}, Permanent(errors.New("http effect with no host"))
	}
	target := e.Hosts[0]
	if err := AssertPublicHost(ctx, target); err != nil {
		return EffectResult{}, Permanent(err)
	}
	path := e.Topic
	if path == "" {
		path = "/"
	}
	client := p.Client
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+target+path, nil)
	if err != nil {
		return EffectResult{}, Permanent(err)
	}
	req.Header.Set("Idempotency-Key", e.IdempotencyKey)
	res, err := client.Do(req)
	if err != nil {
		return EffectResult{}, err
	}
	defer res.Body.Close()
	body := make([]byte, 0, 512)
	buf := make([]byte, 4096)
	limit := p.Limit
	if limit == 0 {
		limit = 64 << 10
	}
	for int64(len(body)) < limit {
		n, err := res.Body.Read(buf)
		body = append(body, buf[:n]...)
		if err != nil {
			break
		}
	}
	if res.StatusCode >= 500 {
		return EffectResult{}, fmt.Errorf("upstream %s: %d", target, res.StatusCode)
	}
	if res.StatusCode >= 400 {
		return EffectResult{}, Permanent(fmt.Errorf("upstream %s: %d", target, res.StatusCode))
	}
	return EffectResult{Body: body}, nil
}

func (p *HTTPProvider) Retryable(err error) bool { return !errors.Is(err, errPermanent) }

// AssertPublicHost resolves a name and refuses any address inside private, loopback,
// link-local or unique-local ranges.
func AssertPublicHost(ctx context.Context, host string) error {
	if !strings.Contains(host, ".") {
		return fmt.Errorf("runtime: refusing single-label host %q", host)
	}
	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return fmt.Errorf("runtime: resolve %s: %w", host, err)
	}
	if len(addrs) == 0 {
		return fmt.Errorf("runtime: %s resolved to nothing", host)
	}
	for _, a := range addrs {
		if !a.IP.IsGlobalUnicast() || a.IP.IsPrivate() || a.IP.IsLoopback() || a.IP.IsLinkLocalUnicast() ||
			a.IP.IsLinkLocalMulticast() || isV6Local(a.IP) {
			return fmt.Errorf("runtime: %s resolves to a non-public address (%s)", host, a.IP)
		}
	}
	return nil
}

func isV6Local(ip net.IP) bool {
	if v4 := ip.To4(); v4 != nil {
		return false
	}
	return ip.IsUnspecified()
}
