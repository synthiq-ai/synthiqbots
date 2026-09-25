package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newTestServer(t *testing.T, allowActions bool) (*Server, *Registry) {
	t.Helper()
	reg := NewRegistry()
	reg.Register(Tool{
		Name:        "echo_read",
		Description: "echo args back (read tool, never gated)",
		InputSchema: json.RawMessage(`{"type":"object"}`),
		Annotations: AnnRead(),
		Handler: func(_ context.Context, raw json.RawMessage, _ string) any {
			return map[string]any{"args": json.RawMessage(raw)}
		},
	})
	reg.Register(Tool{
		Name:        "echo_action",
		Description: "echo args back (action tool, gated by AllowActions)",
		InputSchema: json.RawMessage(`{"type":"object"}`),
		Annotations: AnnAction(true),
		Destructive: true,
		Handler: func(_ context.Context, raw json.RawMessage, _ string) any {
			return map[string]any{"args": json.RawMessage(raw)}
		},
	})
	srv := NewServer(ServerOptions{
		Registry:         reg,
		BearerToken:      "secret",
		AllowActions:     allowActions,
		DefaultRateLimit: 100,
	})
	return srv, reg
}

func mcpRoute(srv *Server) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/mcp", srv.MCPHandler())
	mux.HandleFunc("/mcp/health", srv.HealthHandler())
	return mux
}

func post(t *testing.T, h http.Handler, body, bearer string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	req := httptest.NewRequest("POST", "/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK && rec.Code != http.StatusUnauthorized && rec.Code != http.StatusNoContent {
		t.Fatalf("unexpected status: %d body=%s", rec.Code, rec.Body.String())
	}
	if rec.Code == http.StatusUnauthorized {
		return rec, nil
	}
	if rec.Code == http.StatusNoContent {
		return rec, nil
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode response: %v body=%s", err, rec.Body.String())
	}
	return rec, out
}

func TestBearerReject(t *testing.T) {
	srv, _ := newTestServer(t, false)
	h := mcpRoute(srv)
	rec, _ := post(t, h, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, "wrong-token")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 with wrong bearer, got %d", rec.Code)
	}
}

func TestInitialize(t *testing.T) {
	srv, _ := newTestServer(t, false)
	h := mcpRoute(srv)
	_, body := post(t, h, `{"jsonrpc":"2.0","id":1,"method":"initialize"}`, "secret")
	res := body["result"].(map[string]any)
	if res["protocolVersion"] != ProtocolVersion {
		t.Errorf("wrong protocolVersion: %v", res["protocolVersion"])
	}
}

func TestToolsList(t *testing.T) {
	srv, _ := newTestServer(t, false)
	h := mcpRoute(srv)
	_, body := post(t, h, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, "secret")
	tools := body["result"].(map[string]any)["tools"].([]any)
	if len(tools) != 2 {
		t.Fatalf("expected 2 tools, got %d", len(tools))
	}
	// Sorted alphabetically: echo_action first, echo_read second.
	if tools[0].(map[string]any)["name"] != "echo_action" {
		t.Errorf("expected echo_action first, got %v", tools[0])
	}
	if tools[1].(map[string]any)["name"] != "echo_read" {
		t.Errorf("expected echo_read second, got %v", tools[1])
	}
}

func TestToolsCallReadAlways(t *testing.T) {
	srv, _ := newTestServer(t, false) // allowActions=false
	h := mcpRoute(srv)
	_, body := post(t, h,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo_read","arguments":{"x":1}}}`,
		"secret")
	res := body["result"].(map[string]any)
	if res["isError"].(bool) {
		t.Fatalf("read tool should succeed when AllowActions=false: %v", res)
	}
}

func TestToolsCallActionGate(t *testing.T) {
	srv, _ := newTestServer(t, false) // gate closed
	h := mcpRoute(srv)
	_, body := post(t, h,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo_action","arguments":{"x":1}}}`,
		"secret")
	res := body["result"].(map[string]any)
	if !res["isError"].(bool) {
		t.Fatal("action tool should be gated")
	}
	text := res["content"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, "action tools disabled") {
		t.Errorf("expected gate message, got: %s", text)
	}
}

func TestToolsCallActionAllowed(t *testing.T) {
	srv, _ := newTestServer(t, true) // gate open
	h := mcpRoute(srv)
	_, body := post(t, h,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo_action","arguments":{"x":1}}}`,
		"secret")
	res := body["result"].(map[string]any)
	if res["isError"].(bool) {
		t.Errorf("action should succeed with AllowActions=true: %v", res)
	}
}

func TestUnknownMethod(t *testing.T) {
	srv, _ := newTestServer(t, false)
	h := mcpRoute(srv)
	_, body := post(t, h, `{"jsonrpc":"2.0","id":1,"method":"foo/bar"}`, "secret")
	if body["error"] == nil {
		t.Errorf("expected error for unknown method, got: %v", body)
	}
}

func TestNotificationsAck(t *testing.T) {
	srv, _ := newTestServer(t, false)
	h := mcpRoute(srv)
	rec, _ := post(t, h, `{"jsonrpc":"2.0","method":"notifications/initialized"}`, "secret")
	if rec.Code != http.StatusNoContent {
		t.Errorf("expected 204 for notifications/*, got %d", rec.Code)
	}
}

func TestHealthOpen(t *testing.T) {
	srv, _ := newTestServer(t, true)
	h := mcpRoute(srv)
	req := httptest.NewRequest("GET", "/mcp/health", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("health should be open, got %d", rec.Code)
	}
	body, _ := io.ReadAll(rec.Body)
	if !bytes.Contains(body, []byte(`"tools":2`)) {
		t.Errorf("expected tool count in health body, got %s", body)
	}
}
