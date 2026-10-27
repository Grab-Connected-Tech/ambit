package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// The Model Context Protocol face of ks dev (§17.4): the introspection API as tools an
// agent calls natively, over newline-delimited JSON-RPC 2.0 on stdio.

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type mcpTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
	method      string
	path        func(args map[string]any) string
	body        func(args map[string]any) any
}

func object(props map[string]any, required ...string) map[string]any {
	s := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

var mcpTools = []mcpTool{
	{Name: "ks_actions", Description: "Every action of the running app with its kind, input fields and required roles.",
		InputSchema: object(map[string]any{}), method: http.MethodGet,
		path: func(map[string]any) string { return "/_ks/actions" }},
	{Name: "ks_call", Description: "Run an action through the full pipeline as an actor; returns its output or error and a trace id.",
		InputSchema: object(map[string]any{
			"action": map[string]any{"type": "string", "description": "module.Action"},
			"actor": object(map[string]any{
				"ID": map[string]any{"type": "string"}, "Tenant": map[string]any{"type": "string"},
				"Roles": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
				"Kind":  map[string]any{"type": "string", "enum": []string{"user", "service", "system", "anonymous"}},
			}),
			"input": map[string]any{"type": "object"},
		}, "action", "actor"),
		method: http.MethodPost, path: func(map[string]any) string { return "/_ks/call" },
		body: func(a map[string]any) any { return a }},
	{Name: "ks_trace", Description: "What one call did: its queries with timings, its effects, its error.",
		InputSchema: object(map[string]any{"id": map[string]any{"type": "string"}}, "id"), method: http.MethodGet,
		path: func(a map[string]any) string { return "/_ks/traces/" + url.PathEscape(fmt.Sprint(a["id"])) }},
	{Name: "ks_logs", Description: "Structured log lines after a cursor; pass the returned next to continue.",
		InputSchema: object(map[string]any{"since": map[string]any{"type": "integer"}}), method: http.MethodGet,
		path: func(a map[string]any) string {
			since := 0.0
			if v, ok := a["since"].(float64); ok {
				since = v
			}
			return fmt.Sprintf("/_ks/logs?since=%.0f", since)
		}},
	{Name: "ks_reset", Description: "Empty the database and reload the seed files.",
		InputSchema: object(map[string]any{}), method: http.MethodPost,
		path: func(map[string]any) string { return "/_ks/reset" }},
}

// mcpServe answers MCP requests from in on out until in closes, forwarding tool calls to
// the dev server at base.
func mcpServe(in io.Reader, out io.Writer, base string) error {
	client := &http.Client{Timeout: 30 * time.Second}
	enc := json.NewEncoder(out)
	reply := func(id json.RawMessage, result any, rpcErr map[string]any) {
		msg := map[string]any{"jsonrpc": "2.0", "id": id}
		if rpcErr != nil {
			msg["error"] = rpcErr
		} else {
			msg["result"] = result
		}
		_ = enc.Encode(msg)
	}
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 0, 64<<10), 8<<20)
	for sc.Scan() {
		var req rpcRequest
		if err := json.Unmarshal(sc.Bytes(), &req); err != nil {
			reply(json.RawMessage("null"), nil, map[string]any{"code": -32700, "message": "parse error"})
			continue
		}
		if len(req.ID) == 0 {
			continue // a notification, such as notifications/initialized
		}
		switch req.Method {
		case "initialize":
			var p struct {
				ProtocolVersion string `json:"protocolVersion"`
			}
			_ = json.Unmarshal(req.Params, &p)
			if p.ProtocolVersion == "" {
				p.ProtocolVersion = "2024-11-05"
			}
			reply(req.ID, map[string]any{"protocolVersion": p.ProtocolVersion,
				"capabilities": map[string]any{"tools": map[string]any{}},
				"serverInfo":   map[string]any{"name": "keystone-dev", "version": "0.1"}}, nil)
		case "ping":
			reply(req.ID, map[string]any{}, nil)
		case "tools/list":
			reply(req.ID, map[string]any{"tools": mcpTools}, nil)
		case "tools/call":
			var p struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			}
			_ = json.Unmarshal(req.Params, &p)
			var tool *mcpTool
			for i := range mcpTools {
				if mcpTools[i].Name == p.Name {
					tool = &mcpTools[i]
				}
			}
			if tool == nil {
				reply(req.ID, nil, map[string]any{"code": -32602, "message": "unknown tool " + p.Name})
				continue
			}
			text, failed := mcpForward(client, base, tool, p.Arguments)
			reply(req.ID, map[string]any{"content": []any{map[string]any{"type": "text", "text": text}}, "isError": failed}, nil)
		default:
			reply(req.ID, nil, map[string]any{"code": -32601, "message": "method not found: " + req.Method})
		}
	}
	return sc.Err()
}

func mcpForward(c *http.Client, base string, t *mcpTool, args map[string]any) (string, bool) {
	if args == nil {
		args = map[string]any{}
	}
	var body io.Reader
	if t.body != nil {
		raw, _ := json.Marshal(t.body(args))
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(t.method, base+t.path(args), body)
	if err != nil {
		return err.Error(), true
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := c.Do(req)
	if err != nil {
		return "the dev server is not reachable (is it rebuilding?): " + err.Error(), true
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 4<<20))
	return string(raw), res.StatusCode >= 400
}
