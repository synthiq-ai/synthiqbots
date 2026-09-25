package mcpserver

import (
	"context"
	"encoding/json"

	"github.com/synthiq-ai/synthiqbots/apps/ops-api/internal/files"
	"github.com/synthiq-ai/synthiqbots/apps/ops-api/internal/handlers"
)

// OpsDeps is the subset of handler deps the migrated read-only tools need.
// Pulled into a separate struct so the registration call site stays readable
// and so tests can stub each piece independently.
type OpsDeps struct {
	Server *handlers.Server // gives us StatusJSON, LogsTail, AuditQuery*
	Files  *files.Resolver
}

// RegisterOpsTools registers the 8 migrated ops_* tools onto reg. Each is a
// thin Go-function adapter to the same StatusJSON/LogsTail/etc. functions the
// REST handlers use — no HTTP self-loop.
func RegisterOpsTools(reg *Registry, deps OpsDeps) {
	reg.Register(Tool{
		Name: "ops_status",
		Description: "Return a full system status snapshot: worldserver container state, MCP server " +
			"reachability, DB ping, git SHA, and the embedded ops_module_status. Use this as the " +
			"first call when diagnosing module health.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
		Annotations: AnnRead(),
		Handler: func(ctx context.Context, _ json.RawMessage, _ string) any {
			return deps.Server.StatusJSON(ctx)
		},
	})

	reg.Register(Tool{
		Name: "ops_logs_tail",
		Description: "Tail recent container log lines. Defaults to ac-worldserver. Returns " +
			"{lines:[{ts,stream,msg}], truncated:bool}. Use `grep` (substring) or `regex=true` " +
			"to filter; `since` accepts a duration (\"15m\", \"1h\") or a unix-seconds epoch.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{
			"container":{"type":"string","description":"Container name (default ac-worldserver)"},
			"lines":{"type":"integer","description":"Max lines (default 200, max 5000)"},
			"since":{"type":"string","description":"Lookback window (e.g. \"15m\") or unix seconds"},
			"grep":{"type":"string","description":"Substring filter (or regex if regex=true)"},
			"regex":{"type":"boolean","description":"Treat grep as regex (default false)"}
		}}`),
		Annotations: AnnRead(),
		Handler: func(ctx context.Context, raw json.RawMessage, _ string) any {
			var a struct {
				Container string `json:"container"`
				Lines     any    `json:"lines"` // int or string from MCP
				Since     string `json:"since"`
				Grep      string `json:"grep"`
				Regex     bool   `json:"regex"`
			}
			_ = json.Unmarshal(raw, &a)
			tail := numToString(a.Lines)
			res, err := deps.Server.LogsTail(ctx, a.Container, tail, a.Since, a.Grep, a.Regex)
			if err != nil {
				return map[string]any{"error": err.Error()}
			}
			return res
		},
	})

	reg.Register(Tool{
		Name: "ops_files_list",
		Description: "List files under an allowlisted root. `root` is one of the names exported " +
			"by ops-api's OPS_FILE_ROOTS (typically \"repo\" for the module source tree and " +
			"\"configs\" for AzerothCore .conf files). Optional `path` narrows to a subdir; " +
			"`glob` filters names (e.g. \"*.cpp\").",
		InputSchema: json.RawMessage(`{"type":"object","properties":{
			"root":{"type":"string","description":"Allowlisted root name"},
			"path":{"type":"string","description":"Subpath under root"},
			"glob":{"type":"string","description":"Filename glob (default *)"}
		},"required":["root"]}`),
		Annotations: AnnRead(),
		Handler: func(_ context.Context, raw json.RawMessage, _ string) any {
			var a struct {
				Root, Path, Glob string
			}
			_ = json.Unmarshal(raw, &a)
			entries, err := deps.Files.List(a.Root, a.Path, a.Glob)
			if err != nil {
				return map[string]any{"error": err.Error()}
			}
			return map[string]any{"root": a.Root, "path": a.Path, "entries": entries}
		},
	})

	reg.Register(Tool{
		Name: "ops_files_read",
		Description: "Read a file from an allowlisted root. Returns {path, content, totalLines, " +
			"start, end, truncated}. Use `start`/`end` (1-based, inclusive) to slice; `maxBytes` " +
			"caps the read (default 1 MiB, hard ceiling 5 MiB). The resolver rejects '..' literal, " +
			"resolves symlinks, and any path that escapes the allowlist root.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{
			"root":{"type":"string"},
			"path":{"type":"string"},
			"start":{"type":"integer"},
			"end":{"type":"integer"},
			"maxBytes":{"type":"integer"}
		},"required":["root","path"]}`),
		Annotations: AnnRead(),
		Handler: func(_ context.Context, raw json.RawMessage, _ string) any {
			var a struct {
				Root, Path string
				Start, End int
				MaxBytes   int
			}
			_ = json.Unmarshal(raw, &a)
			if a.MaxBytes <= 0 {
				a.MaxBytes = files.DefaultMaxRead
			}
			res, err := deps.Files.Read(a.Root, a.Path, a.Start, a.End, a.MaxBytes)
			if err != nil {
				return map[string]any{"error": err.Error()}
			}
			return res
		},
	})

	reg.Register(Tool{
		Name: "ops_files_search",
		Description: "Substring or regex search across an allowlisted root. Skips .git and binary " +
			"files (NUL-byte sniff on first 512 B), caps at `max` hits and 10 s wall clock.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{
			"root":{"type":"string"},
			"q":{"type":"string","description":"Search string (or regex if regex=true)"},
			"path":{"type":"string","description":"Optional subpath under root"},
			"regex":{"type":"boolean","description":"Treat q as regex (default false)"},
			"max":{"type":"integer","description":"Max hits (default 200)"}
		},"required":["root","q"]}`),
		Annotations: AnnRead(),
		Handler: func(ctx context.Context, raw json.RawMessage, _ string) any {
			var a struct {
				Root, Q, Path string
				Regex         bool
				Max           int
			}
			_ = json.Unmarshal(raw, &a)
			hits, timedOut, err := deps.Files.Search(ctx, a.Root, a.Path, a.Q, a.Regex, a.Max)
			if err != nil {
				return map[string]any{"error": err.Error()}
			}
			return map[string]any{"hits": hits, "timedOut": timedOut}
		},
	})

	reg.Register(Tool{
		Name: "ops_audit_gateway",
		Description: "Query the gateway audit table (mod_ollama_chat_gateway_audit). Filter by " +
			"botGuid, accountId, time window (`from`/`to` as RFC3339), and errors-only. " +
			"Returns {rows, limit, offset}.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{
			"botGuid":{"type":"integer"},
			"accountId":{"type":"integer"},
			"from":{"type":"string"},
			"to":{"type":"string"},
			"errors":{"type":"boolean"},
			"limit":{"type":"integer","description":"Default 200, max 1000"},
			"offset":{"type":"integer"}
		}}`),
		Annotations: AnnRead(),
		Handler: func(ctx context.Context, raw json.RawMessage, _ string) any {
			var a struct {
				BotGuid   any    `json:"botGuid"`
				AccountID any    `json:"accountId"`
				From      string `json:"from"`
				To        string `json:"to"`
				Errors    bool   `json:"errors"`
				Limit     int    `json:"limit"`
				Offset    int    `json:"offset"`
			}
			_ = json.Unmarshal(raw, &a)
			rows, err := deps.Server.AuditGatewayQuery(ctx, handlers.AuditFilters{
				BotGuid: numToString(a.BotGuid), AccountID: numToString(a.AccountID),
				From: a.From, To: a.To, ErrorsOnly: a.Errors,
				Limit: a.Limit, Offset: a.Offset,
			})
			if err != nil {
				return map[string]any{"error": err.Error()}
			}
			return map[string]any{"rows": rows, "limit": a.Limit, "offset": a.Offset}
		},
	})

	reg.Register(Tool{
		Name: "ops_audit_tactical",
		Description: "Query the tactical-tick audit table (mod_ollama_chat_tactical_audit). Filter " +
			"by botGuid, action, result, escalated-only, and time window.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{
			"botGuid":{"type":"integer"},
			"action":{"type":"string"},
			"result":{"type":"string"},
			"escalated":{"type":"boolean"},
			"from":{"type":"string"},
			"to":{"type":"string"},
			"limit":{"type":"integer","description":"Default 200, max 1000"},
			"offset":{"type":"integer"}
		}}`),
		Annotations: AnnRead(),
		Handler: func(ctx context.Context, raw json.RawMessage, _ string) any {
			var a struct {
				BotGuid   any    `json:"botGuid"`
				Action    string `json:"action"`
				Result    string `json:"result"`
				Escalated bool   `json:"escalated"`
				From      string `json:"from"`
				To        string `json:"to"`
				Limit     int    `json:"limit"`
				Offset    int    `json:"offset"`
			}
			_ = json.Unmarshal(raw, &a)
			rows, err := deps.Server.AuditTacticalQuery(ctx, handlers.AuditFilters{
				BotGuid: numToString(a.BotGuid), Action: a.Action, Result: a.Result,
				Escalated: a.Escalated, From: a.From, To: a.To,
				Limit: a.Limit, Offset: a.Offset,
			})
			if err != nil {
				return map[string]any{"error": err.Error()}
			}
			return map[string]any{"rows": rows, "limit": a.Limit, "offset": a.Offset}
		},
	})

	reg.Register(Tool{
		Name: "ops_audit_summary",
		Description: "Aggregate audit-row counts grouped by bot and action over a window (default " +
			"24h). Useful for a quick \"who's misbehaving\" overview.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{
			"window":{"type":"string","description":"Lookback window (e.g. \"1h\", \"24h\", \"7d\"; default 24h)"}
		}}`),
		Annotations: AnnRead(),
		Handler: func(ctx context.Context, raw json.RawMessage, _ string) any {
			var a struct {
				Window string `json:"window"`
			}
			_ = json.Unmarshal(raw, &a)
			res, err := deps.Server.AuditSummaryQuery(ctx, a.Window)
			if err != nil {
				return map[string]any{"error": err.Error()}
			}
			return res
		},
	})
}

// numToString converts a JSON-decoded number-or-string into a decimal string.
// MCP clients freely send `"botGuid": 20007` or `"botGuid": "20007"` —
// accepting both keeps tools forgiving.
func numToString(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case float64:
		// JSON numbers always decode as float64. Preserve integer formatting.
		if x == float64(int64(x)) {
			return formatInt64(int64(x))
		}
		return formatFloat(x)
	case int:
		return formatInt64(int64(x))
	case int64:
		return formatInt64(x)
	default:
		return ""
	}
}

func formatInt64(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

func formatFloat(f float64) string {
	// Bare-bones; the only callers should be integers anyway. Avoid pulling fmt
	// just for this hot path.
	return formatInt64(int64(f))
}
