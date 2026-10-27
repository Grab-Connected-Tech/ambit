package runtime

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/smtp"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Grab-Connected-Tech/ambit/model"
)

// The get-vanilla case of the AWS SigV4 test suite.
func TestSignV4MatchesTheReferenceVector(t *testing.T) {
	req, _ := http.NewRequest(http.MethodGet, "https://example.amazonaws.com/", nil)
	at, _ := time.Parse("20060102T150405Z", "20150830T123600Z")
	SignV4(req, nil, "AKIDEXAMPLE", "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY", "us-east-1", "service", at)
	want := "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20150830/us-east-1/service/aws4_request, SignedHeaders=host;x-amz-date, Signature=5fa00fa31553b73ebf1942676e86291e8372ff2a2260956d9b8aae1d763fbf31"
	if got := req.Header.Get("Authorization"); got != want {
		t.Fatalf("authorization\n got %s\nwant %s", got, want)
	}
}

func TestWebhookIsSignedAndDelivered(t *testing.T) {
	var got *http.Request
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, body = r, must(io.ReadAll(r.Body))
		w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()
	at := time.Unix(1700000000, 0)
	p := &WebhookProvider{Endpoint: srv.URL, Secret: []byte("s3cret"), allowPrivate: true, now: func() time.Time { return at }}
	res, err := p.Execute(context.Background(), EffectCall{Topic: "invoice.paid", IdempotencyKey: "k1", Payload: json.RawMessage(`{"id":"i1"}`)})
	if err != nil {
		t.Fatal(err)
	}
	if string(res.Body) != `{"ok":true}` {
		t.Fatalf("result %s", res.Body)
	}
	// What a receiver does: recompute the signature over the timestamp and the raw body.
	if sig := got.Header.Get("Ambit-Signature"); sig != p.Sign(at, body) || !strings.HasPrefix(sig, "t=1700000000,v1=") {
		t.Fatalf("signature %q", sig)
	}
	if got.Header.Get("Idempotency-Key") != "k1" || got.Header.Get("Ambit-Topic") != "invoice.paid" {
		t.Fatalf("headers %v", got.Header)
	}
	// Without the test switch the SSRF guard refuses a loopback endpoint.
	p.allowPrivate = false
	if _, err := p.Execute(context.Background(), EffectCall{}); err == nil {
		t.Fatal("a non-https or private endpoint must be refused")
	}
}

func TestSMTPRendersTemplatesAndRefusesHeaderInjection(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "invoice_sent.txt"), []byte("Subject: Invoice {{.number}}\nHello {{.name}},\nyour invoice is attached.\n"), 0o644)
	var sent []byte
	var rcpt []string
	p := &SMTPProvider{Host: "smtp.example.com", Port: 587, From: "app@example.com", Templates: dir,
		send: func(addr string, a smtp.Auth, from string, to []string, msg []byte) error {
			sent, rcpt = msg, to
			return nil
		}}
	_, err := p.Execute(context.Background(), EffectCall{Template: "invoice_sent", IdempotencyKey: "k7",
		Payload: json.RawMessage(`{"to":["ada@example.com"],"data":{"number":"INV-1","name":"Ada"}}`)})
	if err != nil {
		t.Fatal(err)
	}
	msg := string(sent)
	for _, want := range []string{"Subject: Invoice INV-1\r\n", "To: ada@example.com\r\n", "Message-ID: <k7@ambit>", "Hello Ada,\r\n"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("message lacks %q:\n%s", want, msg)
		}
	}
	if len(rcpt) != 1 || rcpt[0] != "ada@example.com" {
		t.Fatalf("recipients %v", rcpt)
	}
	_, err = p.Execute(context.Background(), EffectCall{Payload: json.RawMessage(`{"to":["a@b.c\r\nBcc: x@y.z"],"subject":"hi"}`)})
	if err == nil || p.Retryable(err) {
		t.Fatalf("a header injection is a permanent failure, got %v", err)
	}
}

func TestS3PutIsPathStyleAndSigned(t *testing.T) {
	var got *http.Request
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { got, body = r, must(io.ReadAll(r.Body)) }))
	defer srv.Close()
	p := &S3Provider{Endpoint: srv.URL, Region: "eu-west-1", AccessKey: "AK", SecretKey: "SK"}
	payload, _ := json.Marshal(Object{Key: "invoices/2030/i 1.pdf", ContentType: "application/pdf", Data: []byte("%PDF")})
	if _, err := p.Execute(context.Background(), EffectCall{Bucket: "invoices", Payload: payload}); err != nil {
		t.Fatal(err)
	}
	if got.Method != http.MethodPut || got.URL.EscapedPath() != "/invoices/invoices/2030/i%201.pdf" || string(body) != "%PDF" {
		t.Fatalf("%s %s %q", got.Method, got.URL.EscapedPath(), body)
	}
	if a := got.Header.Get("Authorization"); !strings.Contains(a, "/eu-west-1/s3/aws4_request") || got.Header.Get("X-Amz-Content-Sha256") == "" {
		t.Fatalf("unsigned: %v", got.Header)
	}
}

func TestProvidersFromConfig(t *testing.T) {
	c := &Config{Effects: map[string]map[string]string{"email": {"provider": "smtp", "host": "smtp.example.com", "from": "a@b.c"}}}
	ps, err := providersFromConfig(c, nil)
	if err != nil {
		t.Fatal(err)
	}
	if ps[model.EffectEmail].Name() != "smtp" || ps[model.EffectWebhook].Name() != "log" || ps[model.EffectHTTP].Name() != "http" {
		t.Fatalf("providers %v %v %v", ps[model.EffectEmail].Name(), ps[model.EffectWebhook].Name(), ps[model.EffectHTTP].Name())
	}
	for _, bad := range []map[string]string{{"provider": "smtp"}, {"provider": "sendgrid"}} {
		if _, err := providersFromConfig(&Config{Effects: map[string]map[string]string{"email": bad}}, nil); err == nil {
			t.Fatalf("%v must be refused at startup", bad)
		}
	}
}

func must[T any](v T, _ error) T { return v }

func TestCgroupLimit(t *testing.T) {
	for in, want := range map[string]int64{"max\n": 0, "536870912\n": 536870912, "9223372036854771712": 0, "": 0} {
		if got := cgroupLimit(in); got != want {
			t.Errorf("%q: got %d want %d", in, got, want)
		}
	}
}

func TestPprofNeedsTheAdminToken(t *testing.T) {
	s := &Server{A: &App{Cfg: Defaults(), Registry: NewMetrics()}}
	h := s.adminHandler()
	get := func(token string) int {
		req := httptest.NewRequest(http.MethodGet, "/debug/pprof/", nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	if get("") != http.StatusUnauthorized {
		t.Fatal("with no token configured, profiles stay off")
	}
	t.Setenv("AM_ADMIN_TOKEN", "t0ken")
	if get("wrong") != http.StatusUnauthorized || get("t0ken") != http.StatusOK {
		t.Fatal("only the admin token opens the profiles")
	}
}
