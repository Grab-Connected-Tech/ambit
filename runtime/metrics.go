package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
)

// Metrics is a small Prometheus registry: counters and latency histograms per action
// (§18.1). A hand-rolled registry keeps the dependency list boring and the exposition
// format exactly what Prometheus expects.
type Metrics struct {
	mu        sync.Mutex
	counters  map[string]*counterVec
	histogram map[string]*hist
}

type counterVec struct {
	mu sync.Mutex
	v  map[string]float64
}

type hist struct {
	mu  sync.Mutex
	obs map[string]*histSeries
}

type histSeries struct {
	count   int
	sum     float64
	buckets map[float64]int
}

var histBuckets = []float64{0.5, 1, 2.5, 5, 10, 25, 50, 100, 250, 500, 1000, 2500, 5000, 10000}

// NewMetrics builds an empty registry.
func NewMetrics() *Metrics {
	return &Metrics{
		counters:  map[string]*counterVec{},
		histogram: map[string]*hist{},
	}
}

func (m *Metrics) inc(name string, labels map[string]string) {
	m.metric(name, 1, labels)
}

// metric adds a delta to a counter.
func (m *Metrics) metric(name string, delta float64, labels ...map[string]string) {
	m.mu.Lock()
	c := m.counters[name]
	if c == nil {
		c = &counterVec{v: map[string]float64{}}
		m.counters[name] = c
	}
	m.mu.Unlock()
	key := labelKey(labels)
	c.mu.Lock()
	c.v[key] += delta
	c.mu.Unlock()
}

func labelKey(labels []map[string]string) string {
	if len(labels) == 0 || labels[0] == nil {
		return ""
	}
	keys := make([]string, 0, len(labels[0]))
	for k := range labels[0] {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		parts = append(parts, k+"="+labels[0][k])
	}
	return strings.Join(parts, ",")
}

// observe records one request latency in milliseconds.
func (m *Metrics) observe(ref string, ms float64) {
	m.mu.Lock()
	h := m.histogram["ks_request_duration_ms"]
	if h == nil {
		h = &hist{obs: map[string]*histSeries{}}
		m.histogram["ks_request_duration_ms"] = h
	}
	m.mu.Unlock()
	s := h.obs[ref]
	if s == nil {
		s = &histSeries{buckets: map[float64]int{}}
		h.obs[ref] = s
	}
	s.count++
	s.sum += ms
	for _, b := range histBuckets {
		if ms <= b {
			s.buckets[b]++
		}
	}
}

// Bytes renders the Prometheus text exposition.
func (m *Metrics) Bytes() []byte {
	var b strings.Builder
	m.mu.Lock()
	names := make([]string, 0, len(m.counters))
	for n := range m.counters {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		c := m.counters[n]
		c.mu.Lock()
		keys := make([]string, 0, len(c.v))
		for k := range c.v {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		fmt.Fprintf(&b, "# TYPE %s counter\n", n)
		for _, k := range keys {
			if k == "" {
				fmt.Fprintf(&b, "%s %s\n", n, strconv.FormatFloat(c.v[k], 'f', -1, 64))
				continue
			}
			fmt.Fprintf(&b, "%s{%s} %s\n", n, k, strconv.FormatFloat(c.v[k], 'f', -1, 64))
		}
		c.mu.Unlock()
	}
	for _, n := range []string{"ks_request_duration_ms"} {
		h := m.histogram[n]
		if h == nil {
			continue
		}
		fmt.Fprintf(&b, "# TYPE %s histogram\n", n)
		refs := make([]string, 0, len(h.obs))
		for r := range h.obs {
			refs = append(refs, r)
		}
		sort.Strings(refs)
		for _, r := range refs {
			s := h.obs[r]
			for _, bkt := range histBuckets {
				fmt.Fprintf(&b, "%s_bucket{action=%q,le=%q} %d\n", n, r, strconv.FormatFloat(bkt, 'f', -1, 64), s.buckets[bkt])
			}
			fmt.Fprintf(&b, "%s_bucket{action=%q,le=\"+Inf\"} %d\n", n, r, s.count)
			fmt.Fprintf(&b, "%s_count{action=%q} %d\n", n, r, s.count)
			fmt.Fprintf(&b, "%s_sum{action=%q} %s\n", n, r, strconv.FormatFloat(s.sum, 'f', -1, 64))
		}
	}
	m.mu.Unlock()
	return []byte(b.String())
}

// Buckets is the in-process cache behind the Postgres rate-limit store (§3.3 step 5).
//
// It caches denials only. A cached *allowance* would let one instance hand out requests
// the shared bucket never granted, and a limit of four-per-minute would quietly become
// four-per-minute-per-cache-cycle. Refusing needs no shared state to be correct, so that
// is the answer worth remembering: a flood is shed with a map lookup, and the store is
// consulted only by requests that might be allowed.
type Buckets struct {
	mu    sync.Mutex
	empty map[string]time.Time
	Clock func() time.Time
}

func newBuckets() *Buckets { return &Buckets{empty: map[string]time.Time{}} }

func (b *Buckets) now() time.Time {
	if b.Clock != nil {
		return b.Clock()
	}
	return time.Now()
}

// allow asks the cache. answered is false when the store has to be consulted, which is
// the common case for a key that is not currently refusing.
func (b *Buckets) allow(key string, limit int, per string) (bool, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	until, ok := b.empty[key]
	if !ok {
		return true, false
	}
	if b.now().Before(until) {
		return false, true
	}
	delete(b.empty, key)
	return true, false
}

// denied records a refusal and returns how long it stands: the bucket refills at
// max tokens per period, so one token costs period/max.
func (b *Buckets) denied(key string, limit int, per string) time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	wait := time.Duration(periodSeconds(per) / math.Max(float64(limit), 1) * float64(time.Second))
	if wait <= 0 {
		wait = time.Second
	}
	b.empty[key] = b.now().Add(wait)
	return wait
}

// allowed clears a refusal the store has since forgiven.
func (b *Buckets) allowed(key string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.empty, key)
}

// until reports how long a cached refusal still stands, for a Retry-After value.
func (b *Buckets) until(key string) time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	until, ok := b.empty[key]
	if !ok || !until.After(b.now()) {
		return 0
	}
	return until.Sub(b.now())
}

// Reset forgets every cached refusal; tests use it to isolate one another.
func (b *Buckets) Reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.empty = map[string]time.Time{}
}

// signalContext cancels on SIGTERM and SIGINT, which is the drain signal (§11.2).
func signalContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
}

func pgNoRows() error { return pgx.ErrNoRows }

func jsonDecoder(r io.Reader) *json.Decoder { return json.NewDecoder(r) }

func jsonUnmarshal(b []byte, out any) error { return json.Unmarshal(b, out) }

var _ = sort.Strings
