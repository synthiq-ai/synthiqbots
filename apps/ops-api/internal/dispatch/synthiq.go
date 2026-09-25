package dispatch

// Synthiq HTTP client — OpenAI-compatible chat/completions wrapper with
// vision support (content-blocks in the user message). Kept narrow: just
// the call shape the dispatcher needs, no per-bot routing or session keys
// from the in-game gateway path.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Client wraps the Synthiq endpoint config.
type Client struct {
	URL    string
	Bearer string
	Model  string
	HTTP   *http.Client
}

// New builds a Client with sensible HTTP timeouts for vision payloads.
// Image base64 inflation + multimodal model latency mean the timeout has
// to be generous; 5 minutes is the practical upper bound observed against
// Sonnet-4.6 in vision mode.
func New(url, bearer, model string) *Client {
	return &Client{
		URL:    strings.TrimRight(url, "/"),
		Bearer: bearer,
		Model:  model,
		HTTP:   &http.Client{Timeout: 5 * time.Minute},
	}
}

// AgentResponse is the parsed JSON we expect back from the model. We
// keep the schema permissive — fields the agent didn't fill come through
// as zero-value, which is fine for downstream.
type AgentResponse struct {
	Action            string         `json:"action"`
	Summary           string         `json:"summary"`
	Reasoning         string         `json:"reasoning"`
	ConfigChanges     []ConfigChange `json:"config_changes"`
	PRURL             string         `json:"pr_url"`
	FollowUpQuestion  string         `json:"follow_up_question"`

	// RawContent is the model's raw assistant content string, kept around
	// for audit even when it didn't parse as the expected JSON.
	RawContent string `json:"-"`
}

type ConfigChange struct {
	File string `json:"file"`
	Key  string `json:"key"`
	Old  string `json:"old"`
	New  string `json:"new"`
}

// Call makes one chat-completions request. Returns the parsed response
// or a transport / decode error. The caller decides whether the agent's
// "action" warrants a retry, an error mark, or a resolved-with-no-action.
func (c *Client) Call(ctx context.Context, userContent []map[string]any) (*AgentResponse, error) {
	if c.URL == "" || c.Bearer == "" || c.Model == "" {
		return nil, errors.New("dispatch: synthiq client not configured (url/bearer/model)")
	}

	req := map[string]any{
		"model": c.Model,
		"messages": []map[string]any{
			{"role": "system", "content": SystemPrompt},
			{"role": "user", "content": userContent},
		},
		"temperature": 0.3, // crisp decisions, not creative writing
		// Vision payloads can be big; let the upstream cap completion
		// length itself (most providers default to ~4096).
	}

	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.URL+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("new request: %w", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+c.Bearer)
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.HTTP.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("http: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)

	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("synthiq %d: %s", resp.StatusCode, snippet(respBody))
	}

	var envelope struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(respBody, &envelope); err != nil {
		return nil, fmt.Errorf("decode envelope: %w (body: %s)", err, snippet(respBody))
	}
	if len(envelope.Choices) == 0 {
		return nil, fmt.Errorf("synthiq returned no choices: %s", snippet(respBody))
	}

	raw := envelope.Choices[0].Message.Content
	out := &AgentResponse{RawContent: raw}

	// Models often wrap JSON in ```json fences; strip those before parsing.
	cleaned := stripCodeFences(raw)
	if err := json.Unmarshal([]byte(cleaned), out); err != nil {
		// Soft-fail: the call succeeded, the model just didn't follow the
		// JSON contract. Caller still gets a usable RawContent for audit.
		out.Action = "no_action"
		out.Summary = "(model returned non-JSON; see raw_content)"
	}
	return out, nil
}

func stripCodeFences(s string) string {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "```") {
		return s
	}
	// Drop the opening fence (```json\n or ```\n).
	if i := strings.Index(s, "\n"); i >= 0 {
		s = s[i+1:]
	}
	// Drop the closing fence.
	if j := strings.LastIndex(s, "```"); j >= 0 {
		s = s[:j]
	}
	return strings.TrimSpace(s)
}

func snippet(b []byte) string {
	const max = 256
	s := string(b)
	if len(s) > max {
		s = s[:max] + "..."
	}
	return s
}
