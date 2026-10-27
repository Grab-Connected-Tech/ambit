package main

import (
	"bytes"
	"flag"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestFlagsAcceptPositionalsAnywhere(t *testing.T) {
	var strict *bool
	fs, asJSON, err := flags("x", []string{"billing", "--json", "--strict", "more"}, func(fs *flag.FlagSet) {
		strict = fs.Bool("strict", false, "")
	})
	if err != nil {
		t.Fatal(err)
	}
	if !asJSON || !*strict || fs.NArg() != 2 || fs.Arg(0) != "billing" || fs.Arg(1) != "more" {
		t.Fatalf("json=%v strict=%v args=%v", asJSON, *strict, fs.Args())
	}
}

func TestToolDiagsReadCompilerPositions(t *testing.T) {
	d := toolDiags("GO-BUILD", []byte("# github.com/abhilashiig/ambit/x\n./app/billing/actions.go:12:3: undefined: foo\nplain line\n"))
	if len(d) != 2 {
		t.Fatalf("got %d diagnostics: %+v", len(d), d)
	}
	if d[0].File != "app/billing/actions.go" || d[0].Line != 12 || d[0].Column != 3 || d[0].Message != "undefined: foo" {
		t.Fatalf("position not parsed: %+v", d[0])
	}
	if d[1].File != "" || d[1].Message != "plain line" {
		t.Fatalf("a line without a position keeps its text: %+v", d[1])
	}
}

func TestMCPForwardsToolsToTheDevServer(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		gotPath = r.Method + " " + r.URL.RequestURI()
		w.Write([]byte(`{"traceId":"t1","output":{"total":3}}`))
	}))
	defer srv.Close()
	in := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26"}}
{"jsonrpc":"2.0","method":"notifications/initialized"}
{"jsonrpc":"2.0","id":2,"method":"tools/list"}
{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"am_call","arguments":{"action":"billing.CountDrafts","actor":{"Roles":["member"]}}}}
{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"am_logs","arguments":{"since":7}}}
{"jsonrpc":"2.0","id":5,"method":"nope"}
`)
	var out bytes.Buffer
	if err := mcpServe(in, &out, srv.URL); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 5 {
		t.Fatalf("one reply per request with an id, none for the notification: %d\n%s", len(lines), out.String())
	}
	if !strings.Contains(lines[0], `"protocolVersion":"2025-03-26"`) || !strings.Contains(lines[1], `"am_trace"`) {
		t.Fatalf("handshake or tool list: %s / %s", lines[0], lines[1])
	}
	if !strings.Contains(lines[2], `\"total\":3`) || !strings.Contains(lines[2], `"isError":false`) {
		t.Fatalf("tool result: %s", lines[2])
	}
	if gotPath != "GET /_am/logs?since=7" || !strings.Contains(lines[4], `-32601`) {
		t.Fatalf("logs path %q, unknown method %s", gotPath, lines[4])
	}
}
