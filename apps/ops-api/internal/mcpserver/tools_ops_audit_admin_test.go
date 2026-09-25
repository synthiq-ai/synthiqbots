package mcpserver

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"

	"github.com/synthiq-ai/synthiqbots/apps/ops-api/internal/config"
	"github.com/synthiq-ai/synthiqbots/apps/ops-api/internal/handlers"
)

// makeAdminAuditServer builds a minimal *handlers.Server backed by sqlmock so
// the tool handler exercises the real AuditAdminQuery path. Mirrors the helper
// in handlers/feedback_test.go but with the read-pool DB wired (admin audit
// reads use the ops_ro pool, not ops_rw).
func makeAdminAuditServer(t *testing.T) (*handlers.Server, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return handlers.NewServer(&handlers.Deps{
		Cfg: &config.Config{RequestTimeout: 5 * time.Second},
		DB:  db,
	}), mock
}

// TestOpsAuditAdmin_DescriptionMentionsContext pins the description keywords
// the operator-agent uses to pick this tool over a raw db_query SELECT. Drop
// the "mod_ollama_chat_admin_audit" framing or the per-filter list and the
// agent will fall back to db_query — which is the exact friction this tool
// removes.
func TestOpsAuditAdmin_DescriptionMentionsContext(t *testing.T) {
	reg := NewRegistry()
	RegisterOpsAuditAdminTool(reg, OpsDeps{})
	tool, ok := reg.Get("ops_audit_admin")
	if !ok {
		t.Fatal("ops_audit_admin not registered")
	}
	for _, kw := range []string{
		"mod_ollama_chat_admin_audit",
		"tool",
		"clientIp",
		"errors",
		"durationMs",
		"ops_ro",
	} {
		if !strings.Contains(tool.Description, kw) {
			t.Errorf("description missing %q: %s", kw, tool.Description)
		}
	}
	// Read-only annotation — must NOT carry destructive flag.
	if tool.Destructive {
		t.Error("ops_audit_admin must not be destructive")
	}
	if !strings.Contains(string(tool.Annotations), `"readOnlyHint":true`) {
		t.Errorf("annotations should mark read-only: %s", tool.Annotations)
	}
}

// TestOpsAuditAdmin_NoDBReturnsError covers the same fail-closed branch as the
// other audit tools: when ac-database is down (frequent OOM-kill scenario on
// game-host), the query method returns "database not configured" and the tool
// surfaces it cleanly instead of panicking.
func TestOpsAuditAdmin_NoDBReturnsError(t *testing.T) {
	reg := NewRegistry()
	srv := handlers.NewServer(&handlers.Deps{Cfg: &config.Config{RequestTimeout: 5 * time.Second}})
	RegisterOpsAuditAdminTool(reg, OpsDeps{Server: srv})
	tool, _ := reg.Get("ops_audit_admin")
	resp := tool.Handler(context.Background(), json.RawMessage(`{}`), "")
	got, _ := resp.(map[string]any)
	if msg, _ := got["error"].(string); !strings.Contains(msg, "database not configured") {
		t.Errorf("expected database-not-configured error, got %v", got)
	}
}

// TestOpsAuditAdmin_BadJSONReturnsDecodeError pins the standard arg-decode
// branch so a malformed MCP arguments object surfaces a tool-level error
// rather than panicking in json.Unmarshal.
func TestOpsAuditAdmin_BadJSONReturnsDecodeError(t *testing.T) {
	srv, _ := makeAdminAuditServer(t)
	reg := NewRegistry()
	RegisterOpsAuditAdminTool(reg, OpsDeps{Server: srv})
	tool, _ := reg.Get("ops_audit_admin")
	resp := tool.Handler(context.Background(), json.RawMessage(`not-json`), "")
	got, _ := resp.(map[string]any)
	if msg, _ := got["error"].(string); !strings.Contains(msg, "decode args") {
		t.Errorf("expected decode-args error, got %v", got)
	}
}

// TestOpsAuditAdmin_HappyPath drives the full handler -> AuditAdminQuery ->
// sqlmock path. Verifies (a) the SQL shape matches what the schema expects,
// (b) row scanning packs columns into AdminAuditRow correctly, and (c) the
// tool wraps rows with limit/offset metadata for paging clients.
func TestOpsAuditAdmin_HappyPath(t *testing.T) {
	srv, mock := makeAdminAuditServer(t)
	reg := NewRegistry()
	RegisterOpsAuditAdminTool(reg, OpsDeps{Server: srv})
	tool, _ := reg.Get("ops_audit_admin")

	now := time.Date(2026, 5, 3, 12, 0, 0, 0, time.UTC)
	mock.ExpectQuery(regexp.QuoteMeta("FROM mod_ollama_chat_admin_audit")).
		WithArgs(200, 0).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "ts", "client_ip", "tool", "args_json", "result", "error", "duration_ms",
		}).
			AddRow(uint64(2), now, "172.17.0.5", "container_inspect", `{"name":"ac-worldserver"}`, "ok", "", uint32(42)).
			AddRow(uint64(1), now.Add(-time.Minute), "172.17.0.5", "db_query", `{"sql":"SELECT 1"}`, "error", "boom", uint32(11)))

	resp := tool.Handler(context.Background(), json.RawMessage(`{}`), "")
	got, _ := resp.(map[string]any)
	if msg, _ := got["error"].(string); msg != "" {
		t.Fatalf("unexpected error: %v", got)
	}
	rows, ok := got["rows"].([]handlers.AdminAuditRow)
	if !ok {
		t.Fatalf("rows wrong type: %T", got["rows"])
	}
	if len(rows) != 2 {
		t.Fatalf("rows: %d", len(rows))
	}
	if rows[0].Tool != "container_inspect" || rows[0].Result != "ok" || rows[0].DurationMs != 42 {
		t.Errorf("row[0] mismatch: %+v", rows[0])
	}
	if rows[1].Tool != "db_query" || rows[1].Result != "error" || rows[1].Error != "boom" {
		t.Errorf("row[1] mismatch: %+v", rows[1])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// TestOpsAuditAdmin_FilterApplication verifies each optional filter shows up as
// a WHERE clause and a positional arg in the prepared query. The bug shape we
// guard against: a future refactor of AuditFilters that drops a clause and
// silently widens the result set (operator queries "show me errors" and gets
// non-error rows back).
func TestOpsAuditAdmin_FilterApplication(t *testing.T) {
	srv, mock := makeAdminAuditServer(t)
	reg := NewRegistry()
	RegisterOpsAuditAdminTool(reg, OpsDeps{Server: srv})
	tool, _ := reg.Get("ops_audit_admin")

	mock.ExpectQuery(regexp.QuoteMeta(
		"FROM mod_ollama_chat_admin_audit WHERE tool = ? AND client_ip = ? AND ts >= ? AND ts <= ? AND result = 'error'",
	)).
		WithArgs("db_query", "172.17.0.1", "2026-05-01 00:00:00", "2026-05-03 00:00:00", 50, 0).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "ts", "client_ip", "tool", "args_json", "result", "error", "duration_ms",
		}))

	args := json.RawMessage(`{
		"tool":"db_query",
		"clientIp":"172.17.0.1",
		"from":"2026-05-01 00:00:00",
		"to":"2026-05-03 00:00:00",
		"errors":true,
		"limit":50
	}`)
	resp := tool.Handler(context.Background(), args, "")
	got, _ := resp.(map[string]any)
	if msg, _ := got["error"].(string); msg != "" {
		t.Fatalf("unexpected error: %v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// TestOpsAuditAdmin_LimitClamping ensures an over-1000 limit is clamped to the
// hard cap. Operators occasionally pass huge limits hoping to grab "everything"
// — the cap matches AuditGateway/Tactical's clamp so this tool can't be used
// to evade the existing 1000-row ceiling.
func TestOpsAuditAdmin_LimitClamping(t *testing.T) {
	srv, mock := makeAdminAuditServer(t)
	reg := NewRegistry()
	RegisterOpsAuditAdminTool(reg, OpsDeps{Server: srv})
	tool, _ := reg.Get("ops_audit_admin")

	// limit=99999 should land as 1000 in the SQL args; offset=0 default.
	mock.ExpectQuery(regexp.QuoteMeta("FROM mod_ollama_chat_admin_audit")).
		WithArgs(1000, 0).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "ts", "client_ip", "tool", "args_json", "result", "error", "duration_ms",
		}))

	resp := tool.Handler(context.Background(), json.RawMessage(`{"limit":99999}`), "")
	got, _ := resp.(map[string]any)
	if msg, _ := got["error"].(string); msg != "" {
		t.Fatalf("unexpected error: %v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}
