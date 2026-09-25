package mcpserver

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"
)

// MCP protocol version we advertise on `initialize`. Pinned so a future
// Anthropic SDK upgrade doesn't silently break us. Mirrors the value in
// src/mod-ollama-chat_mcpserver.cpp:15.
const ProtocolVersion = "2025-11-25"

// JSON-RPC 2.0 error codes.
const (
	rpcParseError     = -32700
	rpcInvalidRequest = -32600
	rpcMethodNotFound = -32601
	rpcInvalidParams  = -32602
)

// Server bundles the registry, rate-limiter, audit DB and config gates.
type Server struct {
	Registry      *Registry
	BearerToken   string
	AllowActions  bool
	AdminAuditDB  *sql.DB     // ops_rw connection used for inserting audit rows
	rl            *rateLimiter
}

type ServerOptions struct {
	Registry          *Registry
	BearerToken       string
	AllowActions      bool
	AdminAuditDB      *sql.DB
	RateLimits        map[string]int // tool → calls/min/IP
	DefaultRateLimit  int            // applied when tool not in RateLimits
}

func NewServer(opt ServerOptions) *Server {
	return &Server{
		Registry:     opt.Registry,
		BearerToken:  opt.BearerToken,
		AllowActions: opt.AllowActions,
		AdminAuditDB: opt.AdminAuditDB,
		rl:           newRateLimiter(opt.RateLimits, opt.DefaultRateLimit),
	}
}

// HealthHandler returns a tiny GET /mcp/health handler. No auth — external
// monitors must be able to verify liveness.
func (s *Server) HealthHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status":       "ok",
			"tools":        s.Registry.Len(),
			"allowActions": s.AllowActions,
		})
	}
}

// MCPHandler returns a chi-compatible POST /mcp handler. Bearer auth + JSON-RPC 2.0.
func (s *Server) MCPHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Per-request CORS — bearer is the actual gate.
		w.Header().Set("Access-Control-Allow-Origin", "*")

		// Bearer auth: required on every request, constant-time compared.
		var presented string
		if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
			presented = strings.TrimPrefix(h, "Bearer ")
		}
		if subtle.ConstantTimeCompare([]byte(presented), []byte(s.BearerToken)) != 1 {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}

		body, err := io.ReadAll(r.Body)
		if err != nil {
			writeRPCError(w, nil, rpcParseError, "read body: "+err.Error())
			return
		}
		var req struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      json.RawMessage `json:"id"`
			Method  string          `json:"method"`
			Params  json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			writeRPCError(w, nil, rpcParseError, "parse error")
			return
		}
		if req.Method == "" {
			writeRPCError(w, req.ID, rpcInvalidRequest, "missing method")
			return
		}

		switch {
		case req.Method == "initialize":
			s.handleInitialize(w, req.ID)
		case strings.HasPrefix(req.Method, "notifications/"):
			// Fire-and-forget. The SDK sends `notifications/initialized` after
			// the handshake; ack with no content.
			w.WriteHeader(http.StatusNoContent)
		case req.Method == "tools/list":
			s.handleToolsList(w, req.ID)
		case req.Method == "tools/call":
			s.handleToolsCall(r.Context(), w, req.ID, req.Params, clientIP(r))
		default:
			writeRPCError(w, req.ID, rpcMethodNotFound, "unknown method: "+req.Method)
		}
	}
}

// OptionsHandler answers CORS preflight on /mcp.
func (s *Server) OptionsHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
		w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
		w.WriteHeader(http.StatusNoContent)
	}
}

func (s *Server) handleInitialize(w http.ResponseWriter, id json.RawMessage) {
	writeRPCResult(w, id, map[string]any{
		"protocolVersion": ProtocolVersion,
		"capabilities":    map[string]any{"tools": map[string]any{}},
		"serverInfo": map[string]any{
			"name":    "wow-admin",
			"version": "0.1.0",
		},
	})
}

func (s *Server) handleToolsList(w http.ResponseWriter, id json.RawMessage) {
	names := s.Registry.AdvertisedSorted()
	tools := make([]map[string]any, 0, len(names))
	for _, n := range names {
		t, ok := s.Registry.Get(n)
		if !ok {
			continue
		}
		desc := map[string]any{
			"name":        t.Name,
			"description": t.Description,
			"inputSchema": t.InputSchema,
		}
		if len(t.Annotations) > 0 {
			desc["annotations"] = t.Annotations
		}
		tools = append(tools, desc)
	}
	writeRPCResult(w, id, map[string]any{"tools": tools})
}

func (s *Server) handleToolsCall(ctx context.Context, w http.ResponseWriter, id json.RawMessage, params json.RawMessage, ip string) {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		writeRPCError(w, id, rpcInvalidParams, "params: "+err.Error())
		return
	}
	t, ok := s.Registry.Get(p.Name)
	if !ok {
		writeRPCError(w, id, rpcMethodNotFound, "unknown tool: "+p.Name)
		return
	}
	// Facade dispatch. Resolve to the real tool BEFORE the destructive gate,
	// the rate limiter and the audit write, so all three see the real tool
	// name and the real arguments — a facade must never launder a destructive
	// action past the gate or hide it from the audit trail.
	if t.IsFacade {
		target, targetName, targetArgs, describeOnly, schema, err := s.Registry.ResolveFacade(t, p.Arguments)
		if err != nil {
			writeRPCError(w, id, rpcInvalidParams, err.Error())
			return
		}
		if describeOnly {
			// Schema lookup executes nothing, so it skips the gate. It is
			// still rate limited under the facade name to keep it from being
			// used as an unmetered probe.
			if !s.rl.allow(p.Name, ip) {
				errMsg := "rate limit exceeded for " + p.Name
				s.respondToolResult(w, id, p.Name, p.Arguments, ip, time.Now(),
					map[string]any{"error": errMsg}, true, errMsg)
				return
			}
			writeRPCResult(w, id, map[string]any{
				"content": []map[string]any{{"type": "text", "text": string(mustDescribeJSON(targetName, target, schema))}},
				"isError": false,
			})
			return
		}
		t, p.Name, p.Arguments = target, targetName, targetArgs
	}
	// Action gate.
	if t.Destructive && !s.AllowActions {
		errMsg := "action tools disabled (set OPS_ADMIN_ALLOW_ACTIONS=1)"
		s.respondToolResult(w, id, p.Name, p.Arguments, ip, time.Now(),
			map[string]any{"error": errMsg}, true, errMsg)
		return
	}
	// Per-tool rate limit.
	if !s.rl.allow(p.Name, ip) {
		errMsg := "rate limit exceeded for " + p.Name
		s.respondToolResult(w, id, p.Name, p.Arguments, ip, time.Now(),
			map[string]any{"error": errMsg}, true, errMsg)
		return
	}

	t0 := time.Now()
	var result any
	func() {
		defer func() {
			if rec := recover(); rec != nil {
				result = map[string]any{"error": fmt.Sprintf("dispatch panic: %v", rec)}
			}
		}()
		result = t.Handler(ctx, p.Arguments, ip)
	}()
	isError, errStr := classifyToolResult(result)
	s.respondToolResult(w, id, p.Name, p.Arguments, ip, t0, result, isError, errStr)
}

// classifyToolResult inspects a handler return value and decides whether it
// represents an error case. Handlers may return either:
//
//   - map[string]any with an "error" string key (the original convention),
//   - a typed struct that JSON-marshals to an object with a non-empty
//     "error" field — used by gitRepoResult, sqlImportFileResult, and any
//     future per-item result types,
//   - a struct/map with `ok:false` and no explicit `error` (volume restore
//     restart-failure path) — also flagged as an error.
//
// Without this round-trip check, typed-result errors would silently audit
// as success and the MCP isError flag would lie to the caller.
func classifyToolResult(result any) (bool, string) {
	if result == nil {
		return false, ""
	}
	if m, ok := result.(map[string]any); ok {
		return classifyMap(m)
	}
	// Typed struct: marshal+unmarshal to read its JSON shape. We're going to
	// marshal again in respondToolResult anyway, so this is one extra trip
	// per call — acceptable for the handful of struct-returning tools.
	b, err := json.Marshal(result)
	if err != nil {
		return false, ""
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return false, ""
	}
	return classifyMap(m)
}

func classifyMap(m map[string]any) (bool, string) {
	if e, ok := m["error"].(string); ok && e != "" {
		return true, e
	}
	if ok, present := m["ok"].(bool); present && !ok {
		// Some handlers fall through with ok=false but no `error` string
		// (e.g. volume restore that succeeds-then-fails-to-restart). Surface
		// it as an error so the audit row records "error" not "ok".
		return true, "ok=false (see result for details)"
	}
	return false, ""
}

func (s *Server) respondToolResult(w http.ResponseWriter, id json.RawMessage, name string,
	args json.RawMessage, ip string, started time.Time, result any, isError bool, errStr string) {

	bodyJSON, err := json.Marshal(result)
	if err != nil {
		writeRPCError(w, id, rpcInvalidRequest, "marshal result: "+err.Error())
		return
	}
	dur := int(time.Since(started).Milliseconds())

	// Audit (best-effort, async-friendly via short timeout).
	res := "ok"
	if isError {
		res = "error"
	}
	go WriteAdminAudit(context.Background(), s.AdminAuditDB, AuditRow{
		ClientIP:   ip,
		Tool:       name,
		Args:       redactArgsForAudit(name, args),
		Result:     res,
		Error:      errStr,
		DurationMs: dur,
	})

	writeRPCResult(w, id, map[string]any{
		"content": []map[string]any{
			{"type": "text", "text": string(bodyJSON)},
		},
		"isError": isError,
	})
	if isError {
		slog.Info("mcp tools/call error", "tool", name, "ip", ip, "dur_ms", dur, "err", errStr)
	}
}

func writeRPCResult(w http.ResponseWriter, id json.RawMessage, result any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"jsonrpc": "2.0",
		"id":      rawOrNull(id),
		"result":  result,
	})
}

func writeRPCError(w http.ResponseWriter, id json.RawMessage, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"jsonrpc": "2.0",
		"id":      rawOrNull(id),
		"error":   map[string]any{"code": code, "message": msg},
	})
}

func rawOrNull(id json.RawMessage) any {
	if len(id) == 0 {
		return nil
	}
	return id
}

// clientIP extracts the originating client address from a request.
//
// Order: X-Forwarded-For → X-Real-IP → Cf-Connecting-IP → RemoteAddr.
// Behind Traefik (which is what fronts /mcp publicly) the real client lives
// in X-Forwarded-For. RemoteAddr alone is meaningless — it'd be Traefik's
// container IP, or the literal string "client" in some httptest-derived
// paths — so we treat any non-IP-looking RemoteAddr as junk and fall back
// to "unknown".
func clientIP(r *http.Request) string {
	if v := firstIP(r.Header.Get("X-Forwarded-For")); v != "" {
		return v
	}
	if v := firstIP(r.Header.Get("X-Real-IP")); v != "" {
		return v
	}
	if v := firstIP(r.Header.Get("Cf-Connecting-IP")); v != "" {
		return v
	}
	host := r.RemoteAddr
	if i := strings.LastIndex(host, ":"); i >= 0 {
		host = host[:i]
	}
	host = strings.Trim(host, "[]")
	if looksLikeIP(host) {
		return host
	}
	return "unknown"
}

// firstIP trims and returns the first non-empty entry in a comma-separated
// header value, validating that it parses as an IP. Empty if nothing matches.
func firstIP(h string) string {
	if h == "" {
		return ""
	}
	for _, part := range strings.Split(h, ",") {
		s := strings.TrimSpace(part)
		s = strings.Trim(s, "[]")
		if looksLikeIP(s) {
			return s
		}
	}
	return ""
}

func looksLikeIP(s string) bool {
	if s == "" {
		return false
	}
	return net.ParseIP(s) != nil
}
