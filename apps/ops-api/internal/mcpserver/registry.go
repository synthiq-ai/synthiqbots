// Package mcpserver implements a JSON-RPC 2.0 MCP server with a typed
// tool registry. Mirrors the C++ GatewayMcpServer at
// src/mod-ollama-chat_mcpserver.cpp:184-373 and the registry shape at
// src/mod-ollama-chat_tools.h.
package mcpserver

import (
	"context"
	"encoding/json"
	"sort"
	"sync"
)

// ToolHandler executes a tool. Args is the raw JSON-RPC `arguments` object.
// clientIP is the requester's address (used for rate limiting / audit).
type ToolHandler func(ctx context.Context, args json.RawMessage, clientIP string) any

// Tool is a registry entry — name, description, JSON Schema, MCP annotations,
// destructive flag (gated by AdminAllowActions), and the handler.
type Tool struct {
	Name        string
	Description string
	InputSchema json.RawMessage
	Annotations json.RawMessage
	Destructive bool // true => requires AdminAllowActions=1
	Handler     ToolHandler

	// Hidden suppresses this tool from tools/list. It stays dispatchable by
	// name via tools/call. Set on tools folded into a facade — see facade.go.
	Hidden bool
	// IsFacade marks a synthetic dispatcher tool. Facades carry no Handler;
	// server.handleToolsCall resolves Actions[action] to a real tool first.
	IsFacade bool
	// Actions maps a facade's action name to the target tool name. Empty on
	// non-facade tools.
	Actions map[string]string
}

// Registry is a name-keyed tool table protected by a mutex (registration is
// startup-only; reads are concurrent under the read side of the mutex).
type Registry struct {
	mu    sync.RWMutex
	tools map[string]Tool
}

func NewRegistry() *Registry {
	return &Registry{tools: map[string]Tool{}}
}

func (r *Registry) Register(t Tool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tools[t.Name] = t
}

func (r *Registry) Get(name string) (Tool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.tools[name]
	return t, ok
}

// NamesSorted returns tool names alphabetically. Stable ordering lets MCP
// clients cache the tool list and improves upstream prompt-cache hit rates
// (mirrors GetGatewayToolNamesSorted in the C++ side).
func (r *Registry) NamesSorted() []string {
	r.mu.RLock()
	out := make([]string, 0, len(r.tools))
	for n := range r.tools {
		out = append(out, n)
	}
	r.mu.RUnlock()
	sort.Strings(out)
	return out
}

// AdvertisedSorted returns the names tools/list should expose: NamesSorted
// minus anything folded into a facade. Kept separate from NamesSorted so
// dispatch, health counts and InstallFacades still see the full table.
func (r *Registry) AdvertisedSorted() []string {
	all := r.NamesSorted()
	out := make([]string, 0, len(all))
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, n := range all {
		if t, ok := r.tools[n]; ok && !t.Hidden {
			out = append(out, n)
		}
	}
	return out
}

func (r *Registry) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.tools)
}

// AnnRead returns the standard MCP annotations object for a read-only tool.
func AnnRead() json.RawMessage {
	return json.RawMessage(`{"readOnlyHint":true,"idempotentHint":true,"destructiveHint":false,"openWorldHint":false}`)
}

// AnnAction returns the standard MCP annotations object for a state-changing tool.
// idempotent==true marks operations like "stop" or "set_value" where reapplying
// has no further effect.
func AnnAction(idempotent bool) json.RawMessage {
	if idempotent {
		return json.RawMessage(`{"readOnlyHint":false,"idempotentHint":true,"destructiveHint":true,"openWorldHint":false}`)
	}
	return json.RawMessage(`{"readOnlyHint":false,"idempotentHint":false,"destructiveHint":true,"openWorldHint":false}`)
}
