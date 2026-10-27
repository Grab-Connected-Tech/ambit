package runtime

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Grab-Connected-Tech/ambit/am"
)

// The OTLP trace exporter of §18.1, on the standard library: spans are encoded as
// OTLP/HTTP JSON and posted to <telemetry.otlpEndpoint>/v1/traces in batches. Each
// action is one span with a child per query and per effect, carrying the action, actor
// kind and tenant id; statement arguments and payloads are never exported, so a
// Sensitive() value cannot leave in a trace.
//
// ponytail: export is best effort: a full buffer drops spans (counted in
// am_otlp_dropped) rather than slowing requests, and a failed post is not retried.

const otlpBatch, otlpBuffer = 256, 4096

type otlpExporter struct {
	a        *App
	endpoint string
	client   *http.Client
	spans    chan otlpSpan
	flush    time.Duration
}

type otlpSpan struct {
	TraceID      string     `json:"traceId"`
	SpanID       string     `json:"spanId"`
	ParentSpanID string     `json:"parentSpanId,omitempty"`
	Name         string     `json:"name"`
	Kind         int        `json:"kind"` // 1 internal, 2 server, 3 client
	Start        string     `json:"startTimeUnixNano"`
	End          string     `json:"endTimeUnixNano"`
	Attributes   []otlpAttr `json:"attributes,omitempty"`
	Status       otlpStatus `json:"status"`
}

type otlpAttr struct {
	Key   string         `json:"key"`
	Value map[string]any `json:"value"`
}

type otlpStatus struct {
	Code    int    `json:"code"` // 0 unset, 1 ok, 2 error
	Message string `json:"message,omitempty"`
}

func str(k, v string) otlpAttr { return otlpAttr{k, map[string]any{"stringValue": v}} }
func num(k string, v int64) otlpAttr {
	return otlpAttr{k, map[string]any{"intValue": strconv.FormatInt(v, 10)}}
}

func nanos(t time.Time) string { return strconv.FormatInt(t.UnixNano(), 10) }

func spanID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// startOTLP chains an exporter onto OnCall when an endpoint is configured; Close stops it
// after a final flush.
func (a *App) startOTLP() {
	if a.Cfg.OTLPEndpoint == "" {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	a.stopOTLP = func() { cancel(); <-done }
	x := &otlpExporter{a: a, endpoint: strings.TrimRight(a.Cfg.OTLPEndpoint, "/") + "/v1/traces",
		client: &http.Client{Timeout: 5 * time.Second}, spans: make(chan otlpSpan, otlpBuffer), flush: 2 * time.Second}
	prev := a.OnCall
	a.OnCall = func(t Trace) {
		if prev != nil {
			prev(t)
		}
		x.record(t)
	}
	go func() { x.run(ctx); close(done) }()
}

// spansOf turns one action execution into its span tree.
func spansOf(t Trace) []otlpSpan {
	trace := t.ID
	if len(trace) != 32 {
		var b [16]byte
		_, _ = rand.Read(b[:])
		trace = hex.EncodeToString(b[:])
	}
	end := t.Start.Add(time.Duration(t.Millis * float64(time.Millisecond)))
	root := otlpSpan{TraceID: trace, SpanID: spanID(), Name: t.Action, Kind: 2, Start: nanos(t.Start), End: nanos(end),
		Attributes: []otlpAttr{str("am.action", t.Action), str("am.actor.kind", string(t.Actor.Kind)),
			str("am.tenant_id", string(t.Actor.Tenant)), num("am.queries", int64(len(t.Stmts)))},
		Status: otlpStatus{Code: 1}}
	if t.Err != nil {
		e := am.AsError(t.Err)
		root.Status = otlpStatus{Code: 2, Message: e.Code}
		root.Attributes = append(root.Attributes, str("am.error.code", e.Code))
		if e.Code == "forbidden" {
			root.Attributes = append(root.Attributes, str("am.policy.decision", "deny"))
		}
	}
	out := []otlpSpan{root}
	for _, s := range t.Stmts {
		at := s.At
		if at.IsZero() {
			at = t.Start
		}
		out = append(out, otlpSpan{TraceID: trace, SpanID: spanID(), ParentSpanID: root.SpanID, Name: "db " + firstWord(s.SQL),
			Kind: 3, Start: nanos(at), End: nanos(at.Add(time.Duration(s.Millis * float64(time.Millisecond)))),
			Attributes: []otlpAttr{str("db.system", "postgresql"), str("db.statement", s.SQL), num("db.rows", s.Rows)}})
	}
	for _, e := range t.Effects {
		out = append(out, otlpSpan{TraceID: trace, SpanID: spanID(), ParentSpanID: root.SpanID, Name: "effect " + string(e.Type),
			Kind: 3, Start: nanos(t.Start), End: nanos(t.Start),
			Attributes: []otlpAttr{str("am.effect.type", string(e.Type)), str("am.effect.mode", e.Mode)}})
	}
	return out
}

func firstWord(sql string) string {
	f := strings.Fields(sql)
	if len(f) == 0 {
		return "statement"
	}
	return strings.ToUpper(f[0])
}

func (x *otlpExporter) record(t Trace) {
	for _, s := range spansOf(t) {
		select {
		case x.spans <- s:
		default:
			x.a.metric("am_otlp_dropped", 1)
		}
	}
}

func (x *otlpExporter) run(ctx context.Context) {
	tick := time.NewTicker(x.flush)
	defer tick.Stop()
	var batch []otlpSpan
	send := func() {
		if len(batch) > 0 {
			x.post(batch)
			batch = batch[:0]
		}
	}
	for {
		select {
		case <-ctx.Done():
			// Drain what is already buffered: a select may see Done before the spans
			// recorded just ahead of it, and those are the last of the process.
			for {
				select {
				case s := <-x.spans:
					if batch = append(batch, s); len(batch) >= otlpBatch {
						send()
					}
					continue
				default:
				}
				break
			}
			send()
			return
		case s := <-x.spans:
			if batch = append(batch, s); len(batch) >= otlpBatch {
				send()
			}
		case <-tick.C:
			send()
		}
	}
}

func (x *otlpExporter) post(spans []otlpSpan) {
	body, err := json.Marshal(map[string]any{"resourceSpans": []any{map[string]any{
		"resource":   map[string]any{"attributes": []otlpAttr{str("service.name", x.a.Cfg.App)}},
		"scopeSpans": []any{map[string]any{"scope": map[string]any{"name": "ambit"}, "spans": spans}},
	}}})
	if err != nil {
		return
	}
	res, err := x.client.Post(x.endpoint, "application/json", bytes.NewReader(body))
	if err != nil {
		x.a.metric("am_otlp_failed", 1)
		return
	}
	res.Body.Close()
	if res.StatusCode >= 300 {
		x.a.metric("am_otlp_failed", 1)
	}
}
