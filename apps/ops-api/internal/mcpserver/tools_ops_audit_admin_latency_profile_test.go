package mcpserver

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// TestOpsAuditAdminLatencyProfile_DescriptionMentionsContext guards the
// keywords that shape tool selection. Drop "tail-latency" or "p95DurationMs"
// and the agent will keep reaching for ops_audit_admin_summary (avg/min/max
// only) — the same regression PR #137 caught for the gateway-audit side.
func TestOpsAuditAdminLatencyProfile_DescriptionMentionsContext(t *testing.T) {
	reg := NewRegistry()
	RegisterOpsAuditAdminLatencyProfileTool(reg, DBDeps{})
	tool, ok := reg.Get("ops_audit_admin_latency_profile")
	if !ok {
		t.Fatal("ops_audit_admin_latency_profile not registered")
	}
	for _, kw := range []string{
		"Composite", "mod_ollama_chat_admin_audit",
		"ops_audit_admin_summary", "ops_audit_admin",
		"Tail-latency",
		"p50DurationMs", "p95DurationMs",
		"tool", "errorsOnly", "topN", "window", "168h",
		"ops_ro", "nearest-rank", "Read-only",
	} {
		if !strings.Contains(tool.Description, kw) {
			t.Errorf("description missing %q", kw)
		}
	}
}

// TestOpsAuditAdminLatencyProfile_ReadOnlyAnnotation guards the annotation
// shape — read-only, idempotent, non-destructive. Operators (and the agent
// orchestrator) rely on this hint to skip the destructive-flag confirmation.
func TestOpsAuditAdminLatencyProfile_ReadOnlyAnnotation(t *testing.T) {
	reg := NewRegistry()
	RegisterOpsAuditAdminLatencyProfileTool(reg, DBDeps{})
	tool, _ := reg.Get("ops_audit_admin_latency_profile")
	if tool.Annotations == nil {
		t.Fatal("annotations missing")
	}
	var ann map[string]any
	if err := json.Unmarshal(tool.Annotations, &ann); err != nil {
		t.Fatalf("annotations decode: %v", err)
	}
	if ann["readOnlyHint"] != true {
		t.Errorf("readOnlyHint: %v want true", ann["readOnlyHint"])
	}
	if ann["destructiveHint"] != false {
		t.Errorf("destructiveHint: %v want false", ann["destructiveHint"])
	}
}

func TestOpsAuditAdminLatencyProfile_NoPoolReturnsError(t *testing.T) {
	reg := NewRegistry()
	RegisterOpsAuditAdminLatencyProfileTool(reg, DBDeps{}) // QueryDB nil
	tool, _ := reg.Get("ops_audit_admin_latency_profile")
	resp := tool.Handler(context.Background(), json.RawMessage(`{}`), "")
	got, _ := resp.(map[string]any)
	if msg, _ := got["error"].(string); !strings.Contains(msg, "ops_ro pool not configured") {
		t.Errorf("expected pool-not-configured error, got %v", got)
	}
}

// TestOpsAuditAdminLatencyProfile_WindowValidation exercises the parse + cap
// checks. Bad windows must fail fast at the handler so the SQL never runs.
func TestOpsAuditAdminLatencyProfile_WindowValidation(t *testing.T) {
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	reg := NewRegistry()
	RegisterOpsAuditAdminLatencyProfileTool(reg, DBDeps{QueryDB: db})
	tool, _ := reg.Get("ops_audit_admin_latency_profile")
	cases := []struct {
		name string
		args string
		want string
	}{
		{"unparseable", `{"window":"notaduration"}`, "positive duration"},
		{"zero", `{"window":"0s"}`, "positive duration"},
		{"negative", `{"window":"-1h"}`, "positive duration"},
		{"over cap", `{"window":"169h"}`, "168h"},
		{"bad json", `{`, "decode args"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resp := tool.Handler(context.Background(), json.RawMessage(c.args), "")
			got, _ := resp.(map[string]any)
			msg, _ := got["error"].(string)
			if !strings.Contains(msg, c.want) {
				t.Errorf("error %q does not contain %q", msg, c.want)
			}
		})
	}
}

// TestOpsAuditAdminLatencyProfile_FiltersAndRowCap verifies the SQL takes shape
// with the tool/error filters AND that the row-cap placeholder lands at the
// LIMIT slot. Without these guards we'd risk an unbounded scan of the audit
// table.
func TestOpsAuditAdminLatencyProfile_FiltersAndRowCap(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.ExpectExec(regexp.QuoteMeta("USE `acore_characters`")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	// Three conds: ts, tool, result='error'; then LIMIT.
	mock.ExpectQuery(regexp.QuoteMeta(
		"FROM `mod_ollama_chat_admin_audit` WHERE ts >= ? AND tool = ? AND result = 'error' ORDER BY tool LIMIT ?")).
		WithArgs(sqlmock.AnyArg(), "db_query", int64(opsAuditAdminLatencyProfileRowCap)).
		WillReturnRows(sqlmock.NewRows([]string{"tool", "duration_ms", "err"}))

	reg := NewRegistry()
	RegisterOpsAuditAdminLatencyProfileTool(reg, DBDeps{QueryDB: db})
	tool, _ := reg.Get("ops_audit_admin_latency_profile")
	resp := tool.Handler(context.Background(),
		json.RawMessage(`{"tool":"db_query","errorsOnly":true,"window":"1h"}`), "")
	if got, _ := resp.(map[string]any); got != nil && got["error"] != nil {
		t.Fatalf("unexpected error: %v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestCollectOpsAuditAdminLatencyProfile_Golden drives the full happy path.
// Two tools: one with a wide spread (so p50/p95 differ) and one with constant
// latencies. Validates: percentile math, error-rate %, avg, min/max, and
// call-desc sort.
func TestCollectOpsAuditAdminLatencyProfile_Golden(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.ExpectExec(regexp.QuoteMeta("USE `acore_characters`")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	rows := sqlmock.NewRows([]string{"tool", "duration_ms", "err"})
	// Tool "db_query": 1..20 ms. Errors at 15, 18, 20 → 3/20 = 15.0%.
	for i := uint64(1); i <= 20; i++ {
		errFlag := uint8(0)
		if i == 15 || i == 18 || i == 20 {
			errFlag = 1
		}
		rows.AddRow("db_query", i, errFlag)
	}
	// Tool "ops_status": 5 rows of 50 ms, no errors.
	for i := 0; i < 5; i++ {
		rows.AddRow("ops_status", uint64(50), uint8(0))
	}
	mock.ExpectQuery(regexp.QuoteMeta("FROM `mod_ollama_chat_admin_audit`")).
		WillReturnRows(rows)

	out, err := collectOpsAuditAdminLatencyProfile(context.Background(), db, 0, "", 25, false)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if out.ScannedRows != 25 {
		t.Errorf("scanned: %d want 25", out.ScannedRows)
	}
	if len(out.Tools) != 2 {
		t.Fatalf("tools: %d want 2", len(out.Tools))
	}
	t1, t2 := out.Tools[0], out.Tools[1]
	if t1.Tool != "db_query" {
		t.Errorf("t1 tool: %q want db_query", t1.Tool)
	}
	if t1.Calls != 20 || t1.Errors != 3 {
		t.Errorf("t1 calls/errors: %d/%d want 20/3", t1.Calls, t1.Errors)
	}
	if t1.ErrorRatePct != 15.0 {
		t.Errorf("t1 errorRatePct: %v want 15.0", t1.ErrorRatePct)
	}
	if t1.MinDurationMs != 1 || t1.MaxDurationMs != 20 {
		t.Errorf("t1 min/max: %d/%d want 1/20", t1.MinDurationMs, t1.MaxDurationMs)
	}
	if t1.AvgDurationMs != 10 {
		t.Errorf("t1 avg: %d want 10", t1.AvgDurationMs)
	}
	// p50 nearest-rank: ceil(0.5*20) = 10 → sorted[9] = 10.
	if t1.P50DurationMs != 10 {
		t.Errorf("t1 p50: %d want 10", t1.P50DurationMs)
	}
	// p95 nearest-rank: ceil(0.95*20) = 19 → sorted[18] = 19.
	if t1.P95DurationMs != 19 {
		t.Errorf("t1 p95: %d want 19", t1.P95DurationMs)
	}

	if t2.Tool != "ops_status" {
		t.Errorf("t2 tool: %q want ops_status", t2.Tool)
	}
	if t2.Calls != 5 || t2.Errors != 0 || t2.ErrorRatePct != 0 {
		t.Errorf("t2 calls/errors/pct: %+v", t2)
	}
	// Constant series: p50 == p95 == max == min == 50. This is the
	// load-bearing edge — a percentile impl that interpolates would
	// drift a constant series; nearest-rank doesn't.
	if t2.P50DurationMs != 50 || t2.P95DurationMs != 50 {
		t.Errorf("t2 percentiles on constant series: %+v", t2)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestCollectOpsAuditAdminLatencyProfile_TopNTrims verifies topN slices the
// response after sorting. A drilldown view shouldn't return 1000 tools when
// the operator asked for 2.
func TestCollectOpsAuditAdminLatencyProfile_TopNTrims(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.ExpectExec(regexp.QuoteMeta("USE `acore_characters`")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	rows := sqlmock.NewRows([]string{"tool", "duration_ms", "err"})
	for i := 0; i < 5; i++ {
		rows.AddRow("alpha", uint64(10), uint8(0))
	}
	for i := 0; i < 3; i++ {
		rows.AddRow("beta", uint64(20), uint8(0))
	}
	rows.AddRow("gamma", uint64(30), uint8(0))
	mock.ExpectQuery(regexp.QuoteMeta("FROM `mod_ollama_chat_admin_audit`")).
		WillReturnRows(rows)

	out, err := collectOpsAuditAdminLatencyProfile(context.Background(), db, 0, "", 2, false)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if len(out.Tools) != 2 {
		t.Fatalf("tools: %d want 2", len(out.Tools))
	}
	if out.Tools[0].Tool != "alpha" || out.Tools[1].Tool != "beta" {
		t.Errorf("top-2 sort: %+v", out.Tools)
	}
	if out.ScannedRows != 9 {
		t.Errorf("scannedRows: %d want 9", out.ScannedRows)
	}
}

// TestCollectOpsAuditAdminLatencyProfile_TiebreakerIsToolAsc — when call
// counts tie, the tool-name ascending tiebreaker keeps the sort stable
// against Go's randomized map-iteration order. Without it test runs flake on
// CI.
func TestCollectOpsAuditAdminLatencyProfile_TiebreakerIsToolAsc(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.ExpectExec(regexp.QuoteMeta("USE `acore_characters`")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	rows := sqlmock.NewRows([]string{"tool", "duration_ms", "err"})
	// Three tools with identical call count = 3, intentionally added in
	// non-sorted order so map-iter randomization is exercised.
	for _, name := range []string{"charlie", "alpha", "bravo"} {
		for i := 0; i < 3; i++ {
			rows.AddRow(name, uint64(10), uint8(0))
		}
	}
	mock.ExpectQuery(regexp.QuoteMeta("FROM `mod_ollama_chat_admin_audit`")).
		WillReturnRows(rows)

	out, err := collectOpsAuditAdminLatencyProfile(context.Background(), db, 0, "", 25, false)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if len(out.Tools) != 3 {
		t.Fatalf("tools: %d want 3", len(out.Tools))
	}
	want := []string{"alpha", "bravo", "charlie"}
	for i, w := range want {
		if out.Tools[i].Tool != w {
			t.Errorf("tiebreaker[%d]: %q want %q", i, out.Tools[i].Tool, w)
		}
	}
}

// TestCollectOpsAuditAdminLatencyProfile_Empty — no data path. The shape must
// still be valid JSON with tools:[] (not null), or callers expecting an array
// break.
func TestCollectOpsAuditAdminLatencyProfile_Empty(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.ExpectExec(regexp.QuoteMeta("USE `acore_characters`")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta("FROM `mod_ollama_chat_admin_audit`")).
		WillReturnRows(sqlmock.NewRows([]string{"tool", "duration_ms", "err"}))

	out, err := collectOpsAuditAdminLatencyProfile(context.Background(), db, 0, "", 25, false)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if out.ScannedRows != 0 {
		t.Errorf("scanned: %d want 0", out.ScannedRows)
	}
	if out.Tools == nil || len(out.Tools) != 0 {
		t.Errorf("tools want non-nil empty slice: %+v", out.Tools)
	}
}

// TestOpsAuditAdminLatencyProfile_TopNClamp — oversized topN gets clamped to
// the max (200), not silently passed through. Without the clamp a hostile
// caller could ask for 10000 tools and force the server to fold a huge
// response.
func TestOpsAuditAdminLatencyProfile_TopNClamp(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.ExpectExec(regexp.QuoteMeta("USE `acore_characters`")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta("FROM `mod_ollama_chat_admin_audit`")).
		WillReturnRows(sqlmock.NewRows([]string{"tool", "duration_ms", "err"}))

	reg := NewRegistry()
	RegisterOpsAuditAdminLatencyProfileTool(reg, DBDeps{QueryDB: db})
	tool, _ := reg.Get("ops_audit_admin_latency_profile")
	// topN=99999 — handler must clamp to 200 without erroring.
	resp := tool.Handler(context.Background(),
		json.RawMessage(`{"topN":99999,"window":"1h"}`), "")
	if got, _ := resp.(map[string]any); got != nil && got["error"] != nil {
		t.Fatalf("unexpected error: %v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}
