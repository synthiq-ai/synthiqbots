package mcpserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"time"
)

// AuditRow is a single mod_ollama_chat_admin_audit insertion.
type AuditRow struct {
	ClientIP   string
	Tool       string
	Args       json.RawMessage // serialized args object
	Result     string          // "ok" or "error"
	Error      string          // truncated to 512 chars
	DurationMs int
}

// WriteAdminAudit inserts one audit row. Errors are logged and swallowed —
// auditing failure must not block the actual tool call.
//
// We grab a connection and explicitly `USE acore_characters` before the
// INSERT. The previous version relied on the default schema in DB_EXEC_DSN
// and silently dropped every row when the operator's DSN omitted the path
// segment ("Error 1046 (3D000): No database selected"). Since the audit
// table only ever lives in acore_characters, we hardcode the USE here
// rather than threading the schema name through deps.
func WriteAdminAudit(ctx context.Context, db *sql.DB, row AuditRow) {
	if db == nil {
		return
	}
	if len(row.Error) > 512 {
		row.Error = row.Error[:509] + "…"
	}
	args := string(row.Args)
	if len(args) > 4096 {
		args = args[:4093] + "…"
	}
	c, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	conn, err := db.Conn(c)
	if err != nil {
		slog.Warn("admin audit acquire conn failed", "tool", row.Tool, "err", err)
		return
	}
	defer conn.Close()
	if _, err := conn.ExecContext(c, "USE `acore_characters`"); err != nil {
		slog.Warn("admin audit USE acore_characters failed", "tool", row.Tool, "err", err)
		return
	}
	if _, err := conn.ExecContext(c,
		"INSERT INTO mod_ollama_chat_admin_audit "+
			"(client_ip, tool, args_json, result, error, duration_ms) VALUES (?,?,?,?,?,?)",
		row.ClientIP, row.Tool, args, row.Result, row.Error, row.DurationMs); err != nil {
		slog.Warn("admin audit insert failed", "tool", row.Tool, "err", err)
	}
}
