package mcpserver

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"strings"

	"github.com/synthiq-ai/synthiqbots/apps/ops-api/internal/dockerlog"
)

type ContainerDeps struct {
	Docker           *dockerlog.Client
	DefaultContainer string   // ac-worldserver by default
	DeniedKeyGlobs   []string // applied to env keys + label keys in container_inspect
}

// redactDocker walks a parsed inspect document and redacts environment variable
// values whose key matches any of `deniedKeyGlobs`. Mirrors the deny-pattern
// behavior that gates `file_set_key`. Without this the inspect output leaks
// every secret the operator passed to the container (bearer tokens, DB
// passwords, API keys, etc.).
//
// Matching is case-insensitive. The operator's deny patterns are typically
// TitleCase (matching .conf keys like OllamaChat.Mcp.BearerToken), but Docker
// env vars are SCREAMING_SNAKE (OPS_BEARER_TOKEN) — without case-folding,
// every prod secret would slip through.
func redactDocker(doc map[string]any, deniedKeyGlobs []string) {
	if doc == nil {
		return
	}
	cfg, _ := doc["Config"].(map[string]any)
	if cfg == nil {
		return
	}
	lowerGlobs := make([]string, len(deniedKeyGlobs))
	for i, g := range deniedKeyGlobs {
		lowerGlobs[i] = strings.ToLower(g)
	}
	if env, ok := cfg["Env"].([]any); ok {
		for i, e := range env {
			s, ok := e.(string)
			if !ok {
				continue
			}
			eq := strings.IndexByte(s, '=')
			if eq < 0 {
				continue
			}
			key := s[:eq]
			if MatchesAny(strings.ToLower(key), lowerGlobs) {
				env[i] = key + "=***REDACTED***"
			}
		}
	}
	// Labels are a map[string]string; redact same way for symmetry. Traefik
	// labels with bearer tokens occasionally end up here too.
	if labels, ok := cfg["Labels"].(map[string]any); ok {
		for k := range labels {
			if MatchesAny(strings.ToLower(k), lowerGlobs) {
				labels[k] = "***REDACTED***"
			}
		}
	}
}

// RegisterContainerTools registers the 5 container-control tools.
//
// Tool descriptions explicitly steer the agent toward WoW containers
// (ac-worldserver / ac-database / ac-authserver / wow-ops-api) by default.
// There is no hardcoded allowlist — operators can ad-hoc target other
// containers when explicitly directed.
func RegisterContainerTools(reg *Registry, deps ContainerDeps) {
	if deps.DefaultContainer == "" {
		deps.DefaultContainer = "ac-worldserver"
	}

	reg.Register(Tool{
		Name: "container_list",
		Description: "List Docker containers (running and stopped). Returns one row per " +
			"container with id/names/image/state/status/created. By default Labels are " +
			"omitted to keep the response small — pass `verbose:true` to include them. " +
			"WoW-related containers are typically named ac-worldserver, ac-database, " +
			"ac-authserver, and wow-ops-api.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{
			"verbose":{"type":"boolean","description":"Include Labels in the response (default false; large Traefik label blocks blow the tool-result cap)"}
		}}`),
		Annotations: AnnRead(),
		Handler: func(ctx context.Context, raw json.RawMessage, _ string) any {
			var a struct{ Verbose bool }
			_ = json.Unmarshal(raw, &a)
			items, err := deps.Docker.List(ctx)
			if err != nil {
				return map[string]any{"error": err.Error()}
			}
			if !a.Verbose {
				// Drop Labels in-place. The agent rarely needs them, and a single
				// 40-container call with full Traefik labels exceeds the 25 KB
				// tool-result cap.
				for i := range items {
					items[i].Labels = nil
				}
			}
			return map[string]any{"containers": items}
		},
	})

	reg.Register(Tool{
		Name: "container_inspect",
		Description: "Return the full /containers/{name}/json document from the Docker Engine. " +
			"Defaults to ac-worldserver. Env values whose key matches OPS_ADMIN_DENIED_KEYS " +
			"(Token/Password/Secret/BearerToken/ApiKey/DatabaseInfo by default) are redacted " +
			"to '***REDACTED***' before the response is built — the deny pattern that " +
			"protects file_set_key keys also protects inspect output.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{
			"name":{"type":"string","description":"Container name or id (default ac-worldserver)"}
		}}`),
		Annotations: AnnRead(),
		Handler: func(ctx context.Context, raw json.RawMessage, _ string) any {
			var a struct{ Name string }
			_ = json.Unmarshal(raw, &a)
			name := pickContainer(a.Name, deps.DefaultContainer)
			body, err := deps.Docker.InspectRaw(ctx, name)
			if err != nil {
				return map[string]any{"error": err.Error(), "name": name}
			}
			// Decode into a typed map so we can redact .Config.Env before re-emit.
			var inspect map[string]any
			if err := json.Unmarshal(body, &inspect); err != nil {
				return map[string]any{"error": "decode: " + err.Error(), "name": name}
			}
			redactDocker(inspect, deps.DeniedKeyGlobs)
			return map[string]any{"name": name, "inspect": inspect}
		},
	})

	reg.Register(Tool{
		Name: "container_restart",
		Description: "Restart a container (graceful SIGTERM with `t` seconds, then SIGKILL). " +
			"Defaults to ac-worldserver. WARNING: restarting ac-worldserver severs the " +
			"gameplay-MCP connection at :18790 for ~2 minutes; poll ops_status before issuing " +
			"further bot tool calls. Restarting wow-ops-api will terminate this admin MCP " +
			"session — only do so when explicitly asked.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{
			"name":{"type":"string","description":"Container name (default ac-worldserver)"},
			"t":{"type":"integer","description":"SIGTERM grace period in seconds (default 10)"}
		}}`),
		Annotations: AnnAction(true),
		Destructive: true,
		Handler: func(ctx context.Context, raw json.RawMessage, _ string) any {
			var a struct {
				Name string
				T    int
			}
			_ = json.Unmarshal(raw, &a)
			name := pickContainer(a.Name, deps.DefaultContainer)
			t := a.T
			if t <= 0 {
				t = 10
			}
			if name == hostname() {
				slog.Warn("self-restart requested via MCP", "container", name)
			}
			if err := deps.Docker.Restart(ctx, name, t); err != nil {
				return map[string]any{"error": err.Error(), "name": name}
			}
			res := map[string]any{"ok": true, "name": name, "action": "restart"}
			if name == "ac-worldserver" {
				res["hint"] = "Worldserver MCP unreachable for ~2 min; poll ops_status before next gameplay tool call."
			}
			return res
		},
	})

	reg.Register(Tool{
		Name: "container_stop",
		Description: "Stop a container (graceful SIGTERM with `t` seconds, then SIGKILL). Defaults " +
			"to ac-worldserver. 304 (already stopped) is treated as success.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{
			"name":{"type":"string"},
			"t":{"type":"integer","description":"SIGTERM grace period (default 10)"}
		}}`),
		Annotations: AnnAction(true),
		Destructive: true,
		Handler: func(ctx context.Context, raw json.RawMessage, _ string) any {
			var a struct {
				Name string
				T    int
			}
			_ = json.Unmarshal(raw, &a)
			name := pickContainer(a.Name, deps.DefaultContainer)
			t := a.T
			if t <= 0 {
				t = 10
			}
			if err := deps.Docker.Stop(ctx, name, t); err != nil {
				return map[string]any{"error": err.Error(), "name": name}
			}
			return map[string]any{"ok": true, "name": name, "action": "stop"}
		},
	})

	reg.Register(Tool{
		Name: "container_start",
		Description: "Start a stopped container. Defaults to ac-worldserver. 304 (already running) " +
			"is treated as success.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{
			"name":{"type":"string"}
		}}`),
		Annotations: AnnAction(true),
		Destructive: true,
		Handler: func(ctx context.Context, raw json.RawMessage, _ string) any {
			var a struct{ Name string }
			_ = json.Unmarshal(raw, &a)
			name := pickContainer(a.Name, deps.DefaultContainer)
			if err := deps.Docker.Start(ctx, name); err != nil {
				return map[string]any{"error": err.Error(), "name": name}
			}
			return map[string]any{"ok": true, "name": name, "action": "start"}
		},
	})
}

func pickContainer(arg, def string) string {
	if arg == "" {
		return def
	}
	return arg
}

func hostname() string {
	if h, err := os.Hostname(); err == nil {
		return h
	}
	return ""
}
