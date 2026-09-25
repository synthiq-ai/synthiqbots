package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync/atomic"
	"time"
)

type Client struct {
	baseURL string
	token   string
	http    *http.Client
	idCtr   atomic.Uint64
}

func New(baseURL, token string) *Client {
	return &Client{
		baseURL: baseURL,
		token:   token,
		http:    &http.Client{Timeout: 10 * time.Second},
	}
}

type rpcRequest struct {
	JSONRPC string         `json:"jsonrpc"`
	ID      uint64         `json:"id"`
	Method  string         `json:"method"`
	Params  map[string]any `json:"params"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      uint64          `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *rpcError       `json:"error"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *rpcError) Error() string { return fmt.Sprintf("mcp rpc error %d: %s", e.Code, e.Message) }

// CallTool dispatches an MCP `tools/call` and returns the parsed JSON of the
// first text content block (the registry returns its tool result as one
// text/json block, so this is sufficient for our read tools).
func (c *Client) CallTool(ctx context.Context, name string, args map[string]any) (json.RawMessage, error) {
	req := rpcRequest{
		JSONRPC: "2.0",
		ID:      c.idCtr.Add(1),
		Method:  "tools/call",
		Params: map[string]any{
			"name":      name,
			"arguments": args,
		},
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/mcp", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if c.token != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("mcp http %d: %s", resp.StatusCode, truncate(raw, 200))
	}
	var rr rpcResponse
	if err := json.Unmarshal(raw, &rr); err != nil {
		return nil, fmt.Errorf("decode mcp response: %w (body: %s)", err, truncate(raw, 200))
	}
	if rr.Error != nil {
		return nil, rr.Error
	}
	// MCP `tools/call` result shape: { content: [{type:"text", text:"<json>"}], isError: bool }
	var inner struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(rr.Result, &inner); err != nil {
		// Some MCP servers return the tool result raw — pass through.
		return rr.Result, nil
	}
	if len(inner.Content) == 0 {
		return rr.Result, nil
	}
	return json.RawMessage(inner.Content[0].Text), nil
}

// Ping does an unauthenticated GET /mcp/health to test reachability.
func (c *Client) Ping(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/mcp/health", nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("mcp health http %d", resp.StatusCode)
	}
	return nil
}

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "…"
}
