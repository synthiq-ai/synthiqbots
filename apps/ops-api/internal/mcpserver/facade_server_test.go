package mcpserver

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// newFacadeServer registers two db_* tools (one read, one destructive) so the
// db facade exists, then folds. Mirrors newTestServer's shape.
func newFacadeServer(t *testing.T, allowActions bool) (*Server, *Registry) {
	t.Helper()
	reg := NewRegistry()
	reg.Register(Tool{
		Name:        "db_query",
		Description: "run a read query",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"sql":{"type":"string"}}}`),
		Annotations: AnnRead(),
		Handler: func(_ context.Context, raw json.RawMessage, _ string) any {
			return map[string]any{"tool": "db_query", "args": json.RawMessage(raw)}
		},
	})
	reg.Register(Tool{
		Name:        "db_exec",
		Description: "run a writing statement",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"sql":{"type":"string"}}}`),
		Annotations: AnnAction(false),
		Destructive: true,
		Handler: func(_ context.Context, raw json.RawMessage, _ string) any {
			return map[string]any{"tool": "db_exec", "args": json.RawMessage(raw)}
		},
	})
	InstallFacades(reg)
	srv := NewServer(ServerOptions{
		Registry:         reg,
		BearerToken:      "secret",
		AllowActions:     allowActions,
		DefaultRateLimit: 100,
	})
	return srv, reg
}

func TestToolsListAdvertisesFacadesOnly(t *testing.T) {
	srv, _ := newFacadeServer(t, false)
	_, out := post(t, mcpRoute(srv), `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, "secret")

	tools := out["result"].(map[string]any)["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("advertised %d tools, want 1 facade", len(tools))
	}
	if name := tools[0].(map[string]any)["name"]; name != "db" {
		t.Fatalf("advertised %v, want db", name)
	}
	// The action enum must be present — it is the only routing signal the
	// model gets once per-action schemas are gone.
	schema, _ := json.Marshal(tools[0].(map[string]any)["inputSchema"])
	for _, want := range []string{`"query"`, `"exec"`} {
		if !strings.Contains(string(schema), want) {
			t.Errorf("facade schema missing action %s: %s", want, schema)
		}
	}
}

func TestLegacyFlatNameStillDispatches(t *testing.T) {
	srv, _ := newFacadeServer(t, false)
	_, out := post(t, mcpRoute(srv),
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"db_query","arguments":{"sql":"SELECT 1"}}}`, "secret")

	res := out["result"].(map[string]any)
	if res["isError"] == true {
		t.Fatalf("legacy direct call failed: %v", res)
	}
	if !strings.Contains(textOf(res), `"db_query"`) {
		t.Fatalf("legacy call did not reach db_query: %v", res)
	}
}

func TestFacadeCallReachesTarget(t *testing.T) {
	srv, _ := newFacadeServer(t, false)
	_, out := post(t, mcpRoute(srv),
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"db","arguments":{"action":"query","params":{"sql":"SELECT 1"}}}}`, "secret")

	res := out["result"].(map[string]any)
	if res["isError"] == true {
		t.Fatalf("facade call failed: %v", res)
	}
	body := textOf(res)
	if !strings.Contains(body, `"db_query"`) || !strings.Contains(body, "SELECT 1") {
		t.Fatalf("facade did not pass params through to db_query: %s", body)
	}
}

// The security-critical one: a destructive action reached through a facade
// must still hit the AllowActions gate. A facade that laundered db_exec past
// the gate would be a privilege escalation.
func TestFacadeDoesNotBypassActionGate(t *testing.T) {
	srv, _ := newFacadeServer(t, false) // allowActions = false
	_, out := post(t, mcpRoute(srv),
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"db","arguments":{"action":"exec","params":{"sql":"DELETE FROM x"}}}}`, "secret")

	res := out["result"].(map[string]any)
	if res["isError"] != true {
		t.Fatalf("destructive action through facade was NOT gated: %v", res)
	}
	if !strings.Contains(textOf(res), "action tools disabled") {
		t.Fatalf("wrong gate error: %s", textOf(res))
	}
}

func TestFacadeAllowsActionWhenEnabled(t *testing.T) {
	srv, _ := newFacadeServer(t, true)
	_, out := post(t, mcpRoute(srv),
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"db","arguments":{"action":"exec","params":{"sql":"DELETE FROM x"}}}}`, "secret")

	res := out["result"].(map[string]any)
	if res["isError"] == true {
		t.Fatalf("action should be allowed: %v", res)
	}
	if !strings.Contains(textOf(res), `"db_exec"`) {
		t.Fatalf("did not reach db_exec: %s", textOf(res))
	}
}

func TestFacadeDescribeReturnsSchemaWithoutExecuting(t *testing.T) {
	srv, _ := newFacadeServer(t, false)
	_, out := post(t, mcpRoute(srv),
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"db","arguments":{"describe":"exec"}}}`, "secret")

	res := out["result"].(map[string]any)
	if res["isError"] == true {
		t.Fatalf("describe failed: %v", res)
	}
	body := textOf(res)
	// Describe on a destructive action is allowed (it executes nothing) and
	// must say so rather than silently implying the call would succeed.
	for _, want := range []string{`"db_exec"`, `"destructive":true`, `"sql"`} {
		if !strings.Contains(body, want) {
			t.Errorf("describe body missing %s: %s", want, body)
		}
	}
	if strings.Contains(body, "DELETE") {
		t.Errorf("describe appears to have executed: %s", body)
	}
}

func TestFacadeUnknownActionIsInvalidParams(t *testing.T) {
	srv, _ := newFacadeServer(t, false)
	_, out := post(t, mcpRoute(srv),
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"db","arguments":{"action":"drop_everything"}}}`, "secret")

	e, ok := out["error"].(map[string]any)
	if !ok {
		t.Fatalf("expected JSON-RPC error, got %v", out)
	}
	if !strings.Contains(e["message"].(string), "unknown action") {
		t.Fatalf("unhelpful error: %v", e)
	}
}

func textOf(res map[string]any) string {
	c, ok := res["content"].([]any)
	if !ok || len(c) == 0 {
		return ""
	}
	s, _ := c[0].(map[string]any)["text"].(string)
	return s
}
