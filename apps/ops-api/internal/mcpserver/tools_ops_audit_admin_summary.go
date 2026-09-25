package mcpserver

import (
	"context"
	"encoding/json"
)

// RegisterOpsAuditAdminSummaryTool registers `ops_audit_admin_summary` — a
// per-tool aggregate of mod_ollama_chat_admin_audit over a time window. Closes
// the gap left by `ops_audit_summary`, which only rolls up the gateway and
// tactical tables. Operators (and the wow-ops-api-improve cron itself) need
// the same shape for the admin-audit table to spot:
//
//   - top tools by call count (which surface drives the most operator work)
//   - error rate per tool (which surface fails most)
//   - duration min/avg/max per tool (which surface is slow)
//
// Without this, the cron's "look at audit logs to spot operator-agent
// patterns" step has to call ops_audit_admin (1000-row hard cap) and fold
// rows in-memory, which can't cover a busy week. db_query with a custom
// GROUP BY works but yields raw [][]any rows instead of a typed bucket list.
func RegisterOpsAuditAdminSummaryTool(reg *Registry, deps OpsDeps) {
	reg.Register(Tool{
		Name: "ops_audit_admin_summary",
		Description: "Aggregate the admin-MCP audit table (mod_ollama_chat_admin_audit) into per-tool " +
			"buckets over a time window (default 24h, max 720h). Returns " +
			"{window, since, totalCalls, totalErrors, tools:[{tool, calls, errors, errorRatePct, " +
			"minDurationMs, avgDurationMs, maxDurationMs}]} sorted by call count DESC. " +
			"Use this — instead of ops_audit_admin — when you want \"which tools are hot, " +
			"which error, which are slow?\" without paging up to 1000 raw rows. Companion to " +
			"ops_audit_summary (which covers gateway + tactical, not admin). Read-only via " +
			"ops_ro pool — no destructive flag.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{
			"window":{"type":"string","description":"Lookback window (e.g. \"1h\", \"24h\", \"7d\"; default 24h, max 720h)"}
		}}`),
		Annotations: AnnRead(),
		Handler: func(ctx context.Context, raw json.RawMessage, _ string) any {
			var a struct {
				Window string `json:"window"`
			}
			if err := json.Unmarshal(raw, &a); err != nil {
				return map[string]any{"error": "decode args: " + err.Error()}
			}
			res, err := deps.Server.AuditAdminSummaryQuery(ctx, a.Window)
			if err != nil {
				return map[string]any{"error": err.Error()}
			}
			return res
		},
	})
}
