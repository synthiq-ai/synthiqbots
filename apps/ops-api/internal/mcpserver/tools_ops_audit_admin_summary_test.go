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

// TestOpsAuditAdminSummary_DescriptionMentionsContext pins the keywords the
// operator-agent uses to pick this tool over a raw db_query SELECT or over
// ops_audit_admin (paged rows). Drop the framing ("admin-MCP audit",
// "errorRatePct", "ops_audit_summary" cross-reference, "ops_ro") and the
// agent will fall back to the lower-leverage tools, which is exactly the
// friction this tool removes.
func TestOpsAuditAdminSummary_DescriptionMentionsContext(t *testing.T) {
	reg := NewRegistry()
	RegisterOpsAuditAdminSummaryTool(reg, OpsDeps{})
	tool, ok := reg.Get("ops_audit_admin_summary")
	if !ok {
		t.Fatal("ops_audit_admin_summary not registered")
	}
	for _, kw := range []string{
		"mod_ollama_chat_admin_audit",
		"window",
		"errorRatePct",
		"avgDurationMs",
		"ops_audit_summary",
		"ops_ro",
	} {
		if !strings.Contains(tool.Description, kw) {
			t.Errorf("description missing %q: %s", kw, tool.Description)
		}
	}
	if tool.Destructive {
		t.Error("ops_audit_admin_summary must not be destructive")
	}
	if !strings.Contains(string(tool.Annotations), `"readOnlyHint":true`) {
		t.Errorf("annotations should mark read-only: %s", tool.Annotations)
	}
}

// TestOpsAuditAdminSummary_NoDBReturnsError covers the fail-closed branch when
// ac-database is down (routine on game-host — OOM-kill of ac-worldserver takes
// the DB with it). The tool surfaces a clean error rather than panicking on
// a nil *sql.DB.
func TestOpsAuditAdminSummary_NoDBReturnsError(t *testing.T) {
	reg := NewRegistry()
	srv := handlers.NewServer(&handlers.Deps{Cfg: &config.Config{RequestTimeout: 5 * time.Second}})
	RegisterOpsAuditAdminSummaryTool(reg, OpsDeps{Server: srv})
	tool, _ := reg.Get("ops_audit_admin_summary")
	resp := tool.Handler(context.Background(), json.RawMessage(`{}`), "")
	got, _ := resp.(map[string]any)
	if msg, _ := got["error"].(string); !strings.Contains(msg, "database not configured") {
		t.Errorf("expected database-not-configured error, got %v", got)
	}
}

// TestOpsAuditAdminSummary_BadJSONReturnsDecodeError pins the standard
// arg-decode branch so a malformed MCP arguments object surfaces a tool-level
// error rather than panicking in json.Unmarshal.
func TestOpsAuditAdminSummary_BadJSONReturnsDecodeError(t *testing.T) {
	srv, _ := makeAdminAuditServer(t)
	reg := NewRegistry()
	RegisterOpsAuditAdminSummaryTool(reg, OpsDeps{Server: srv})
	tool, _ := reg.Get("ops_audit_admin_summary")
	resp := tool.Handler(context.Background(), json.RawMessage(`not-json`), "")
	got, _ := resp.(map[string]any)
	if msg, _ := got["error"].(string); !strings.Contains(msg, "decode args") {
		t.Errorf("expected decode-args error, got %v", got)
	}
}

// TestOpsAuditAdminSummary_BadWindowReturnsError pins the duration-validation
// branch shared with ops_audit_summary. Negative / zero / >720h windows are
// rejected so a careless `"window":"-1h"` doesn't silently widen the
// time-range.
func TestOpsAuditAdminSummary_BadWindowReturnsError(t *testing.T) {
	srv, _ := makeAdminAuditServer(t)
	reg := NewRegistry()
	RegisterOpsAuditAdminSummaryTool(reg, OpsDeps{Server: srv})
	tool, _ := reg.Get("ops_audit_admin_summary")
	for _, bad := range []string{"-1h", "0s", "wat", "1000h"} {
		args, _ := json.Marshal(map[string]string{"window": bad})
		resp := tool.Handler(context.Background(), args, "")
		got, _ := resp.(map[string]any)
		if msg, _ := got["error"].(string); !strings.Contains(msg, "window must be a positive duration") {
			t.Errorf("window=%q: expected window-validation error, got %v", bad, got)
		}
	}
}

// TestOpsAuditAdminSummary_HappyPath drives the full tool -> AuditAdminSummaryQuery
// -> sqlmock path. Verifies (a) the SQL shape (CASE WHEN error count, MIN/AVG/MAX
// duration, GROUP BY tool ORDER BY count DESC), (b) the per-bucket error-rate
// computation rounds to 1 decimal, and (c) totalCalls/totalErrors are summed
// across buckets.
func TestOpsAuditAdminSummary_HappyPath(t *testing.T) {
	srv, mock := makeAdminAuditServer(t)
	reg := NewRegistry()
	RegisterOpsAuditAdminSummaryTool(reg, OpsDeps{Server: srv})
	tool, _ := reg.Get("ops_audit_admin_summary")

	mock.ExpectQuery(regexp.QuoteMeta(
		"FROM mod_ollama_chat_admin_audit WHERE ts >= ? GROUP BY tool ORDER BY COUNT(*) DESC LIMIT 200",
	)).
		WillReturnRows(sqlmock.NewRows([]string{
			"tool", "calls", "errors", "min", "avg", "max",
		}).
			AddRow("db_query", uint64(100), uint64(5), uint64(2), uint64(15), uint64(120)).
			AddRow("container_inspect", uint64(40), uint64(0), uint64(8), uint64(22), uint64(85)).
			AddRow("ops_status", uint64(3), uint64(1), uint64(50), uint64(60), uint64(70)))

	resp := tool.Handler(context.Background(), json.RawMessage(`{"window":"24h"}`), "")
	got, ok := resp.(*handlers.AdminSummaryResult)
	if !ok {
		t.Fatalf("response wrong type: %T (%v)", resp, resp)
	}
	if got.Window != "24h" {
		t.Errorf("window: %q", got.Window)
	}
	if got.TotalCalls != 143 {
		t.Errorf("totalCalls: %d", got.TotalCalls)
	}
	if got.TotalErrors != 6 {
		t.Errorf("totalErrors: %d", got.TotalErrors)
	}
	if len(got.Tools) != 3 {
		t.Fatalf("tools len: %d", len(got.Tools))
	}
	if got.Tools[0].Tool != "db_query" || got.Tools[0].ErrorRatePct != 5.0 {
		t.Errorf("bucket[0]: %+v", got.Tools[0])
	}
	if got.Tools[1].Tool != "container_inspect" || got.Tools[1].ErrorRatePct != 0.0 {
		t.Errorf("bucket[1]: %+v", got.Tools[1])
	}
	// 1/3 = 33.333...% → rounded to 33.3.
	if got.Tools[2].Tool != "ops_status" || got.Tools[2].ErrorRatePct != 33.3 {
		t.Errorf("bucket[2]: %+v", got.Tools[2])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// TestOpsAuditAdminSummary_ScansBytesNumericForAvg pins the real-driver shape
// of the AVG column. Without `CAST(AVG(...) AS UNSIGNED)` in the query MySQL
// returns AVG as DECIMAL, which the Go driver delivers as []byte ("7.0000")
// — direct rows.Scan into uint64 then panics with
// "converting driver.Value type []uint8 ... invalid syntax". The CAST forces
// MySQL to return BIGINT UNSIGNED, which the driver delivers as numeric []byte
// ("7") that database/sql converts to uint64 cleanly. This test simulates the
// post-CAST shape and would have caught the bug if it had existed before
// b736468 shipped (the original happy-path test fed uint64 directly via
// sqlmock, masking the production scan path).
func TestOpsAuditAdminSummary_ScansBytesNumericForAvg(t *testing.T) {
	srv, mock := makeAdminAuditServer(t)
	reg := NewRegistry()
	RegisterOpsAuditAdminSummaryTool(reg, OpsDeps{Server: srv})
	tool, _ := reg.Get("ops_audit_admin_summary")

	mock.ExpectQuery(regexp.QuoteMeta(
		"FROM mod_ollama_chat_admin_audit WHERE ts >= ? GROUP BY tool ORDER BY COUNT(*) DESC LIMIT 200",
	)).
		WillReturnRows(sqlmock.NewRows([]string{"tool", "calls", "errors", "min", "avg", "max"}).
			AddRow("db_query", []byte("100"), []byte("5"), []byte("2"), []byte("15"), []byte("120")))

	resp := tool.Handler(context.Background(), json.RawMessage(`{"window":"24h"}`), "")
	got, ok := resp.(*handlers.AdminSummaryResult)
	if !ok {
		t.Fatalf("response wrong type: %T (%v)", resp, resp)
	}
	if len(got.Tools) != 1 {
		t.Fatalf("tools len: %d", len(got.Tools))
	}
	if got.Tools[0].AvgDurationMs != 15 {
		t.Errorf("avgDurationMs: got %d want 15", got.Tools[0].AvgDurationMs)
	}
}

// TestOpsAuditAdminSummary_DefaultWindow24h confirms the empty/missing
// `window` arg falls back to 24h and the SQL still receives a `since`
// timestamp that's roughly 24h ago. We don't pin the exact value (depends on
// time.Now) — just that it's within the last 25h.
func TestOpsAuditAdminSummary_DefaultWindow24h(t *testing.T) {
	srv, mock := makeAdminAuditServer(t)
	reg := NewRegistry()
	RegisterOpsAuditAdminSummaryTool(reg, OpsDeps{Server: srv})
	tool, _ := reg.Get("ops_audit_admin_summary")

	mock.ExpectQuery(regexp.QuoteMeta("FROM mod_ollama_chat_admin_audit")).
		WithArgs(sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"tool", "calls", "errors", "min", "avg", "max"}))

	resp := tool.Handler(context.Background(), json.RawMessage(`{}`), "")
	got, ok := resp.(*handlers.AdminSummaryResult)
	if !ok {
		t.Fatalf("response wrong type: %T", resp)
	}
	if got.Window != "24h" {
		t.Errorf("default window: %q", got.Window)
	}
	if since := time.Since(got.Since); since < 23*time.Hour || since > 25*time.Hour {
		t.Errorf("since not ~24h ago: %v", since)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}
