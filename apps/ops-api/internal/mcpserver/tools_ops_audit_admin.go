package mcpserver

import (
	"context"
	"encoding/json"

	"github.com/synthiq-ai/synthiqbots/apps/ops-api/internal/handlers"
)

// RegisterOpsAuditAdminTool registers `ops_audit_admin` — a read-only query
// against mod_ollama_chat_admin_audit, the per-tools/call audit row that
// ops-api writes for itself via WriteAdminAudit. Mirrors ops_audit_gateway /
// ops_audit_tactical so the audit-tool family is consistent across all three
// audit tables (gateway / tactical / admin).
//
// Closes a self-introspection gap: until this tool, the operator agent could
// not answer "which ops-api tools have I been calling, how often, and which
// ones errored?" without dropping into raw db_query SQL. The wow-ops-api-improve
// cron's "look at audit logs to spot operator-agent patterns" gap-finding
// step relied on exactly this table — it now has a first-class tool to call.
func RegisterOpsAuditAdminTool(reg *Registry, deps OpsDeps) {
	reg.Register(Tool{
		Name: "ops_audit_admin",
		Description: "Query the admin-MCP audit table (mod_ollama_chat_admin_audit) — one row per " +
			"tools/call dispatch on this ops-api. Filter by `tool` (exact match, e.g. \"db_query\"), " +
			"`clientIp` (exact match), `errors` (result='error' only), and time window (`from`/`to` " +
			"as RFC3339 or 'YYYY-MM-DD HH:MM:SS'). Returns {rows:[{id,ts,clientIp,tool,argsJson," +
			"result,error,durationMs}], limit, offset}. Use this to spot operator-agent patterns: " +
			"top tools, error rates per tool, slow tools (sort by durationMs in argsJson). " +
			"Companion to ops_audit_gateway (chat traffic) and ops_audit_tactical (bot decisions). " +
			"Read-only via ops_ro pool — no destructive flag.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{
			"tool":{"type":"string","description":"Exact tool name to filter by (e.g. \"db_query\", \"container_inspect\")"},
			"clientIp":{"type":"string","description":"Exact client IP to filter by"},
			"errors":{"type":"boolean","description":"Only return result='error' rows"},
			"from":{"type":"string","description":"Start of time window (RFC3339 or MySQL DATETIME)"},
			"to":{"type":"string","description":"End of time window (RFC3339 or MySQL DATETIME)"},
			"limit":{"type":"integer","description":"Default 200, max 1000"},
			"offset":{"type":"integer"}
		}}`),
		Annotations: AnnRead(),
		Handler: func(ctx context.Context, raw json.RawMessage, _ string) any {
			var a struct {
				Tool     string `json:"tool"`
				ClientIP string `json:"clientIp"`
				Errors   bool   `json:"errors"`
				From     string `json:"from"`
				To       string `json:"to"`
				Limit    int    `json:"limit"`
				Offset   int    `json:"offset"`
			}
			if err := json.Unmarshal(raw, &a); err != nil {
				return map[string]any{"error": "decode args: " + err.Error()}
			}
			rows, err := deps.Server.AuditAdminQuery(ctx, handlers.AuditFilters{
				Tool: a.Tool, ClientIP: a.ClientIP,
				From: a.From, To: a.To, ErrorsOnly: a.Errors,
				Limit: a.Limit, Offset: a.Offset,
			})
			if err != nil {
				return map[string]any{"error": err.Error()}
			}
			return map[string]any{"rows": rows, "limit": a.Limit, "offset": a.Offset}
		},
	})
}
