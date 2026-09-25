package mcpserver

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// TestWowBotLatencyProfile_DescriptionMentionsContext guards the keywords
// that shape tool selection. Drop "Composite" or "percentile" and the agent
// will keep reaching for ops_audit_gateway + manual aggregation instead.
func TestWowBotLatencyProfile_DescriptionMentionsContext(t *testing.T) {
	reg := NewRegistry()
	RegisterWowBotLatencyProfileTool(reg, DBDeps{})
	tool, ok := reg.Get("wow_bot_latency_profile")
	if !ok {
		t.Fatal("wow_bot_latency_profile not registered")
	}
	for _, kw := range []string{"Composite", "mod_ollama_chat_gateway_audit",
		"p50LatencyMs", "p95LatencyMs", "botGuid", "errorsOnly", "topN",
		"window", "168h"} {
		if !strings.Contains(tool.Description, kw) {
			t.Errorf("description missing %q", kw)
		}
	}
}

func TestWowBotLatencyProfile_NoPoolReturnsError(t *testing.T) {
	reg := NewRegistry()
	RegisterWowBotLatencyProfileTool(reg, DBDeps{}) // QueryDB nil
	tool, _ := reg.Get("wow_bot_latency_profile")
	resp := tool.Handler(context.Background(), json.RawMessage(`{}`), "")
	got, _ := resp.(map[string]any)
	if msg, _ := got["error"].(string); !strings.Contains(msg, "ops_ro pool not configured") {
		t.Errorf("expected pool-not-configured error, got %v", got)
	}
}

// TestWowBotLatencyProfile_WindowValidation exercises the parse + cap checks.
// Bad windows must fail fast at the handler so the SQL never runs.
func TestWowBotLatencyProfile_WindowValidation(t *testing.T) {
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	reg := NewRegistry()
	RegisterWowBotLatencyProfileTool(reg, DBDeps{QueryDB: db})
	tool, _ := reg.Get("wow_bot_latency_profile")
	cases := []struct {
		name string
		args string
		want string
	}{
		{"unparseable", `{"window":"notaduration"}`, "positive duration"},
		{"zero", `{"window":"0s"}`, "positive duration"},
		{"negative", `{"window":"-1h"}`, "positive duration"},
		{"over cap", `{"window":"169h"}`, "168h"},
		{"bad botGuid", `{"botGuid":"abc"}`, "botGuid must be a positive integer"},
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

// TestWowBotLatencyProfile_FiltersAndRowCap verifies the SQL takes shape with
// the bot/error filters AND that the row-cap placeholder lands at the LIMIT
// slot. Without these guards we'd risk an unbounded scan of the audit table.
func TestWowBotLatencyProfile_FiltersAndRowCap(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.ExpectExec(regexp.QuoteMeta("USE `acore_characters`")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	// Three conds: ts, bot_guid, error = 1; then LIMIT.
	mock.ExpectQuery(regexp.QuoteMeta(
		"FROM `mod_ollama_chat_gateway_audit` WHERE ts >= ? AND bot_guid = ? AND error = 1 ORDER BY bot_guid LIMIT ?")).
		WithArgs(sqlmock.AnyArg(), uint64(20007), int64(wowBotLatencyProfileRowCap)).
		WillReturnRows(sqlmock.NewRows([]string{"bot_guid", "latency_ms", "error"}))

	reg := NewRegistry()
	RegisterWowBotLatencyProfileTool(reg, DBDeps{QueryDB: db})
	tool, _ := reg.Get("wow_bot_latency_profile")
	resp := tool.Handler(context.Background(),
		json.RawMessage(`{"botGuid":20007,"errorsOnly":true,"window":"1h"}`), "")
	if got, _ := resp.(map[string]any); got != nil && got["error"] != nil {
		t.Fatalf("unexpected error: %v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestCollectBotLatencyProfile_Golden drives the full happy path. Two bots,
// one with a wide latency spread (so p50/p95 differ) and one orderly bot
// with all-equal latencies. Validates: percentile math, error-rate %, avg,
// min/max, and call-desc sort.
func TestCollectBotLatencyProfile_Golden(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.ExpectExec(regexp.QuoteMeta("USE `acore_characters`")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	// 20 rows for bot 100 with a spread, 5 rows for bot 200 with constants.
	rows := sqlmock.NewRows([]string{"bot_guid", "latency_ms", "error"})
	// Bot 100: 1..20 ms. Errors at 15, 18, 20 → 3/20 = 15.0%.
	for i := uint64(1); i <= 20; i++ {
		errFlag := uint8(0)
		if i == 15 || i == 18 || i == 20 {
			errFlag = 1
		}
		rows.AddRow(uint64(100), i, errFlag)
	}
	// Bot 200: 5 rows of 50 ms, no errors.
	for i := 0; i < 5; i++ {
		rows.AddRow(uint64(200), uint64(50), uint8(0))
	}
	mock.ExpectQuery(regexp.QuoteMeta("FROM `mod_ollama_chat_gateway_audit`")).
		WillReturnRows(rows)

	out, err := collectBotLatencyProfile(context.Background(), db, 0, "", 25, false)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if out.ScannedRows != 25 {
		t.Errorf("scanned: %d want 25", out.ScannedRows)
	}
	if len(out.Bots) != 2 {
		t.Fatalf("bots: %d want 2", len(out.Bots))
	}
	// Sort: bot 100 has more calls, comes first.
	b1, b2 := out.Bots[0], out.Bots[1]
	if b1.BotGuid != 100 {
		t.Errorf("b1 guid: %d want 100", b1.BotGuid)
	}
	if b1.Calls != 20 || b1.Errors != 3 {
		t.Errorf("b1 calls/errors: %d/%d want 20/3", b1.Calls, b1.Errors)
	}
	if b1.ErrorRatePct != 15.0 {
		t.Errorf("b1 errorRatePct: %v want 15.0", b1.ErrorRatePct)
	}
	if b1.MinLatencyMs != 1 || b1.MaxLatencyMs != 20 {
		t.Errorf("b1 min/max: %d/%d want 1/20", b1.MinLatencyMs, b1.MaxLatencyMs)
	}
	// Avg = (1+2+...+20) / 20 = 210 / 20 = 10 (integer divide).
	if b1.AvgLatencyMs != 10 {
		t.Errorf("b1 avg: %d want 10", b1.AvgLatencyMs)
	}
	// p50 nearest-rank: ceil(0.5*20) = 10 → sorted[9] = 10.
	if b1.P50LatencyMs != 10 {
		t.Errorf("b1 p50: %d want 10", b1.P50LatencyMs)
	}
	// p95 nearest-rank: ceil(0.95*20) = 19 → sorted[18] = 19.
	if b1.P95LatencyMs != 19 {
		t.Errorf("b1 p95: %d want 19", b1.P95LatencyMs)
	}

	if b2.BotGuid != 200 {
		t.Errorf("b2 guid: %d want 200", b2.BotGuid)
	}
	if b2.Calls != 5 || b2.Errors != 0 || b2.ErrorRatePct != 0 {
		t.Errorf("b2 calls/errors/pct: %+v", b2)
	}
	if b2.P50LatencyMs != 50 || b2.P95LatencyMs != 50 {
		t.Errorf("b2 percentiles on constant series: %+v", b2)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestCollectBotLatencyProfile_TopNTrims verifies topN slices the response
// after sorting. A drilldown view shouldn't return 1000 bots when the
// operator asked for 2.
func TestCollectBotLatencyProfile_TopNTrims(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.ExpectExec(regexp.QuoteMeta("USE `acore_characters`")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	rows := sqlmock.NewRows([]string{"bot_guid", "latency_ms", "error"})
	// Three bots with different call counts: 5, 3, 1.
	for i := 0; i < 5; i++ {
		rows.AddRow(uint64(1), uint64(10), uint8(0))
	}
	for i := 0; i < 3; i++ {
		rows.AddRow(uint64(2), uint64(20), uint8(0))
	}
	rows.AddRow(uint64(3), uint64(30), uint8(0))
	mock.ExpectQuery(regexp.QuoteMeta("FROM `mod_ollama_chat_gateway_audit`")).
		WillReturnRows(rows)

	out, err := collectBotLatencyProfile(context.Background(), db, 0, "", 2, false)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if len(out.Bots) != 2 {
		t.Fatalf("bots: %d want 2", len(out.Bots))
	}
	if out.Bots[0].BotGuid != 1 || out.Bots[1].BotGuid != 2 {
		t.Errorf("top-2 sort: %+v", out.Bots)
	}
	if out.ScannedRows != 9 {
		t.Errorf("scannedRows: %d want 9", out.ScannedRows)
	}
}

// TestCollectBotLatencyProfile_Empty — no data path. The shape must still be
// valid JSON with bots:[] (not null), or callers expecting an array break.
func TestCollectBotLatencyProfile_Empty(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.ExpectExec(regexp.QuoteMeta("USE `acore_characters`")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta("FROM `mod_ollama_chat_gateway_audit`")).
		WillReturnRows(sqlmock.NewRows([]string{"bot_guid", "latency_ms", "error"}))

	out, err := collectBotLatencyProfile(context.Background(), db, 0, "", 25, false)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if out.ScannedRows != 0 {
		t.Errorf("scanned: %d want 0", out.ScannedRows)
	}
	if out.Bots == nil || len(out.Bots) != 0 {
		t.Errorf("bots want non-nil empty slice: %+v", out.Bots)
	}
}

func TestNearestRankPercentile(t *testing.T) {
	cases := []struct {
		name   string
		series []uint64
		p      float64
		want   uint64
	}{
		{"empty", nil, 0.5, 0},
		{"single", []uint64{42}, 0.5, 42},
		{"single p95", []uint64{42}, 0.95, 42},
		{"two p50", []uint64{1, 2}, 0.5, 1},          // ceil(0.5*2) = 1 → sorted[0] = 1
		{"two p95", []uint64{1, 2}, 0.95, 2},         // ceil(0.95*2) = 2 → sorted[1] = 2
		{"ten p50", []uint64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}, 0.5, 5},  // ceil(0.5*10) = 5
		{"ten p95", []uint64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}, 0.95, 10}, // ceil(0.95*10) = 10
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := nearestRankPercentile(c.series, c.p)
			if got != c.want {
				t.Errorf("nearestRankPercentile(%v, %v) = %d, want %d", c.series, c.p, got, c.want)
			}
		})
	}
}
