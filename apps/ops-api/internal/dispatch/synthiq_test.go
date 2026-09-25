package dispatch

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeSynthiq mimics an OpenAI-compatible chat-completions endpoint and
// records what the client sent for assertions.
type capturedReq struct {
	bearer  string
	model   string
	system  string
	user    []map[string]any
}

func newFakeSynthiq(t *testing.T, status int, content string, cap *capturedReq) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cap.bearer = r.Header.Get("Authorization")
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Model    string `json:"model"`
			Messages []struct {
				Role    string `json:"role"`
				Content any    `json:"content"`
			} `json:"messages"`
		}
		_ = json.Unmarshal(body, &req)
		cap.model = req.Model
		for _, m := range req.Messages {
			if m.Role == "system" {
				cap.system, _ = m.Content.(string)
			}
			if m.Role == "user" {
				if arr, ok := m.Content.([]any); ok {
					for _, b := range arr {
						if mp, ok := b.(map[string]any); ok {
							cap.user = append(cap.user, mp)
						}
					}
				}
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":` + jsonString(content) + `}}]}`))
	}))
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func TestCallParsesAgentJSON(t *testing.T) {
	cap := &capturedReq{}
	agent := `{"action":"config_tweak","summary":"raise tactical heartbeat","reasoning":"too many wakeups","config_changes":[{"file":"mod_ollama_chat.conf","key":"OllamaChat.Tactical.HeartbeatMs","old":"10000","new":"15000"}]}`
	srv := newFakeSynthiq(t, http.StatusOK, agent, cap)
	defer srv.Close()

	c := New(srv.URL, "secret", "claude-sonnet-4-6")
	resp, err := c.Call(context.Background(), []map[string]any{
		{"type": "text", "text": "hello"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if cap.bearer != "Bearer secret" {
		t.Errorf("bearer: %q", cap.bearer)
	}
	if cap.model != "claude-sonnet-4-6" {
		t.Errorf("model: %q", cap.model)
	}
	if cap.system == "" || !strings.Contains(cap.system, "code-improvement agent") {
		t.Errorf("system: %q", cap.system)
	}
	if resp.Action != "config_tweak" {
		t.Errorf("action: %q", resp.Action)
	}
	if len(resp.ConfigChanges) != 1 || resp.ConfigChanges[0].New != "15000" {
		t.Errorf("config_changes: %+v", resp.ConfigChanges)
	}
}

func TestCallStripsCodeFences(t *testing.T) {
	cap := &capturedReq{}
	wrapped := "```json\n" + `{"action":"no_action","summary":"all good"}` + "\n```"
	srv := newFakeSynthiq(t, http.StatusOK, wrapped, cap)
	defer srv.Close()

	c := New(srv.URL, "x", "m")
	resp, err := c.Call(context.Background(), []map[string]any{{"type": "text", "text": "x"}})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Action != "no_action" {
		t.Errorf("action: %q", resp.Action)
	}
}

func TestCallSoftFailsOnNonJSON(t *testing.T) {
	cap := &capturedReq{}
	srv := newFakeSynthiq(t, http.StatusOK, "I'm sorry, I can't do that.", cap)
	defer srv.Close()

	c := New(srv.URL, "x", "m")
	resp, err := c.Call(context.Background(), []map[string]any{{"type": "text", "text": "x"}})
	if err != nil {
		t.Fatalf("non-JSON should soft-fail, not return an error: %v", err)
	}
	if resp.Action != "no_action" {
		t.Errorf("action: %q (expected fallback)", resp.Action)
	}
	if !strings.Contains(resp.RawContent, "I'm sorry") {
		t.Errorf("raw_content: %q", resp.RawContent)
	}
}

func TestCallBubblesUpstream5xx(t *testing.T) {
	cap := &capturedReq{}
	srv := newFakeSynthiq(t, http.StatusInternalServerError, "", cap)
	defer srv.Close()

	c := New(srv.URL, "x", "m")
	_, err := c.Call(context.Background(), []map[string]any{{"type": "text", "text": "x"}})
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("err: %v", err)
	}
}

func TestCallRefusesUnconfigured(t *testing.T) {
	c := New("", "", "")
	_, err := c.Call(context.Background(), []map[string]any{{"type": "text", "text": "x"}})
	if err == nil {
		t.Fatal("expected error from unconfigured client")
	}
}
