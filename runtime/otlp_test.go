package runtime

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Grab-Connected-Tech/ambit/am"
)

func TestOTLPExportsActionAndQuerySpansWithoutArguments(t *testing.T) {
	var mu sync.Mutex
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/traces" {
			t.Errorf("path %s", r.URL.Path)
		}
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(b))
		mu.Unlock()
	}))
	defer srv.Close()
	cfg := Defaults()
	cfg.App, cfg.OTLPEndpoint = "myapp", srv.URL
	a := &App{Cfg: cfg, Registry: NewMetrics()}
	a.startOTLP()
	start := time.Now()
	a.OnCall(Trace{ID: strings.Repeat("ab", 16), Action: "billing.SendInvoice", Start: start, Millis: 3,
		Actor: am.Actor{Kind: am.ActorUser, Tenant: "t-1"},
		Stmts: []Stmt{{SQL: "SELECT t.id FROM invoice t WHERE t.tax_id = $1", Args: []any{"DE-SECRET"}, At: start, Millis: 1}},
		Err:   am.Forbidden("no")})
	a.stopOTLP() // flushes
	mu.Lock()
	defer mu.Unlock()
	all := strings.Join(bodies, "")
	if strings.Contains(all, "DE-SECRET") {
		t.Fatal("a statement argument was exported")
	}
	var doc struct {
		ResourceSpans []struct {
			ScopeSpans []struct {
				Spans []otlpSpan `json:"spans"`
			} `json:"scopeSpans"`
		} `json:"resourceSpans"`
	}
	if err := json.Unmarshal([]byte(bodies[0]), &doc); err != nil {
		t.Fatal(err)
	}
	spans := doc.ResourceSpans[0].ScopeSpans[0].Spans
	if len(spans) != 2 || spans[0].Name != "billing.SendInvoice" || spans[1].ParentSpanID != spans[0].SpanID ||
		spans[1].Name != "db SELECT" || spans[0].TraceID != strings.Repeat("ab", 16) || spans[0].Status.Code != 2 {
		t.Fatalf("spans %+v", spans)
	}
	if !strings.Contains(all, `"am.policy.decision"`) || !strings.Contains(all, `"service.name"`) {
		t.Fatalf("attributes missing: %s", all)
	}
}

func TestTraceparentContinuesTheCallersTrace(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	r.Header.Set("traceparent", "00-4BF92F3577B34DA6A3CE929D0E0E4736-00f067aa0ba902b7-01")
	if got := traceFromRequest(r); got != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Fatalf("trace id %s", got)
	}
}
