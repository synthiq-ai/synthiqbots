package mcpserver

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// TestWowBotTokenUsage_DescriptionMentionsContext guards the keywords that
// shape tool selection. Drop "token" or the column names and the agent will
// keep reaching for wow_bot_lookup's raw-row dump + manual summation instead.
func TestWowBotTokenUsage_DescriptionMentionsContext(t *testing.T) {
	reg := NewRegistry()
	RegisterWowBotTokenUsageTool(reg, DBDeps{})
	tool, ok := reg.Get("wow_bot_token_usage")
	if !ok {
		t.Fatal("wow_bot_token_usage not registered")
	}
	for _, kw := range []string{"Composite", "mod_ollama_chat_gateway_audit",
		"promptTokens", "completionTokens", "totalTokens", "avgTotalTokensPerCall",
		"requestChars", "responseChars", "wow_bot_latency_profile",
		"wow_bot_source_channel_breakdown", "proact_", "botGuid", "errorsOnly",
		"topN", "window", "168h"} {
		if !strings.Contains(tool.Description, kw) {
			t.Errorf("description missing %q", kw)
		}
	}
}

func TestWowBotTokenUsage_NoPoolReturnsError(t *testing.T) {
	reg := NewRegistry()
	RegisterWowBotTokenUsageTool(reg, DBDeps{}) // QueryDB nil
	tool, _ := reg.Get("wow_bot_token_usage")
	resp := tool.Handler(context.Background(), json.RawMessage(`{}`), "")
	got, _ := resp.(map[string]any)
	if msg, _ := got["error"].(string); !strings.Contains(msg, "ops_ro pool not configured") {
		t.Errorf("expected pool-not-configured error, got %v", got)
	}
}

// TestWowBotTokenUsage_WindowValidation exercises the parse + cap checks. Bad
// windows (and a non-numeric botGuid) must fail fast at the handler so the SQL
// never runs.
func TestWowBotTokenUsage_WindowValidation(t *testing.T) {
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	reg := NewRegistry()
	RegisterWowBotTokenUsageTool(reg, DBDeps{QueryDB: db})
	tool, _ := reg.Get("wow_bot_token_usage")
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

// TestWowBotTokenUsage_FiltersAndRowCap verifies the SQL takes shape with the
// bot/error filters AND that the row-cap placeholder lands at the LIMIT slot.
// Without these guards we'd risk an unbounded scan of the audit table.
func TestWowBotTokenUsage_FiltersAndRowCap(t *testing.T) {
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
		WithArgs(sqlmock.AnyArg(), uint64(20007), int64(wowBotTokenUsageRowCap)).
		WillReturnRows(sqlmock.NewRows([]string{"bot_guid", "prompt_tokens", "completion_tokens", "request_chars", "response_chars", "error"}))

	reg := NewRegistry()
	RegisterWowBotTokenUsageTool(reg, DBDeps{QueryDB: db})
	tool, _ := reg.Get("wow_bot_token_usage")
	resp := tool.Handler(context.Background(),
		json.RawMessage(`{"botGuid":20007,"errorsOnly":true,"window":"1h"}`), "")
	if got, _ := resp.(map[string]any); got != nil && got["error"] != nil {
		t.Fatalf("unexpected error: %v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestCollectBotTokenUsage_Golden drives the full happy path. Bot 100 has FEWER
// calls than bot 200 but far MORE total tokens — so the totalTokens-desc sort
// (not call-count) must put bot 100 first. Validates per-bot sums, totalTokens,
// avgTotalTokensPerCall integer-divide, errorRatePct, char sums, and that
// fleet totals fold across every row.
func TestCollectBotTokenUsage_Golden(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.ExpectExec(regexp.QuoteMeta("USE `acore_characters`")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	cols := []string{"bot_guid", "prompt_tokens", "completion_tokens", "request_chars", "response_chars", "error"}
	rows := sqlmock.NewRows(cols)
	// Bot 100: 2 heavy calls, one of them errored.
	rows.AddRow(uint64(100), uint64(100), uint64(50), uint64(200), uint64(80), uint8(0))
	rows.AddRow(uint64(100), uint64(300), uint64(150), uint64(400), uint64(160), uint8(1))
	// Bot 200: 5 light calls, no errors.
	for i := 0; i < 5; i++ {
		rows.AddRow(uint64(200), uint64(10), uint64(5), uint64(20), uint64(8), uint8(0))
	}
	mock.ExpectQuery(regexp.QuoteMeta("FROM `mod_ollama_chat_gateway_audit`")).
		WillReturnRows(rows)

	out, err := collectBotTokenUsage(context.Background(), db, 0, "", 25, false)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if out.ScannedRows != 7 {
		t.Errorf("scanned: %d want 7", out.ScannedRows)
	}
	// Fleet totals fold across every scanned row.
	wantTotals := botTokenUsageTotals{
		Calls: 7, PromptTokens: 450, CompletionTokens: 225, TotalTokens: 675,
		RequestChars: 700, ResponseChars: 280,
	}
	if out.Totals != wantTotals {
		t.Errorf("totals: %+v want %+v", out.Totals, wantTotals)
	}
	if len(out.Bots) != 2 {
		t.Fatalf("bots: %d want 2", len(out.Bots))
	}
	// Sort by totalTokens desc: bot 100 (600) before bot 200 (75), despite 100
	// having fewer calls (2 < 5) — the load-bearing divergence from the
	// latency-profile call-count sort.
	b1, b2 := out.Bots[0], out.Bots[1]
	if b1.BotGuid != 100 {
		t.Errorf("b1 guid: %d want 100 (token sort, not call-count)", b1.BotGuid)
	}
	if b1.Calls != 2 || b1.Errors != 1 || b1.ErrorRatePct != 50.0 {
		t.Errorf("b1 calls/errors/pct: %d/%d/%v want 2/1/50.0", b1.Calls, b1.Errors, b1.ErrorRatePct)
	}
	if b1.PromptTokens != 400 || b1.CompletionTokens != 200 || b1.TotalTokens != 600 {
		t.Errorf("b1 tokens: %d/%d/%d want 400/200/600", b1.PromptTokens, b1.CompletionTokens, b1.TotalTokens)
	}
	// avg = 600 / 2 = 300 (integer divide).
	if b1.AvgTotalTokensPerCall != 300 {
		t.Errorf("b1 avgTotalTokensPerCall: %d want 300", b1.AvgTotalTokensPerCall)
	}
	if b1.RequestChars != 600 || b1.ResponseChars != 240 {
		t.Errorf("b1 chars: %d/%d want 600/240", b1.RequestChars, b1.ResponseChars)
	}
	if b2.BotGuid != 200 || b2.Calls != 5 || b2.TotalTokens != 75 || b2.AvgTotalTokensPerCall != 15 {
		t.Errorf("b2: %+v want guid 200 / calls 5 / totalTokens 75 / avg 15", b2)
	}
	if b2.Errors != 0 || b2.ErrorRatePct != 0 {
		t.Errorf("b2 errors/pct: %d/%v want 0/0", b2.Errors, b2.ErrorRatePct)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestCollectBotTokenUsage_TopNHonestTotals — topN trims the displayed bots but
// the fleet totals must stay computed over the FULL scan, so a caller can still
// derive "bot X = N% of fleet token spend" honestly.
func TestCollectBotTokenUsage_TopNHonestTotals(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.ExpectExec(regexp.QuoteMeta("USE `acore_characters`")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	cols := []string{"bot_guid", "prompt_tokens", "completion_tokens", "request_chars", "response_chars", "error"}
	rows := sqlmock.NewRows(cols)
	rows.AddRow(uint64(100), uint64(100), uint64(50), uint64(200), uint64(80), uint8(0))
	rows.AddRow(uint64(100), uint64(300), uint64(150), uint64(400), uint64(160), uint8(1))
	for i := 0; i < 5; i++ {
		rows.AddRow(uint64(200), uint64(10), uint64(5), uint64(20), uint64(8), uint8(0))
	}
	mock.ExpectQuery(regexp.QuoteMeta("FROM `mod_ollama_chat_gateway_audit`")).
		WillReturnRows(rows)

	out, err := collectBotTokenUsage(context.Background(), db, 0, "", 1, false)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if len(out.Bots) != 1 || out.Bots[0].BotGuid != 100 {
		t.Fatalf("topN=1 should keep only bot 100: %+v", out.Bots)
	}
	// Totals + scannedRows stay whole-fleet despite the 1-bot display.
	if out.ScannedRows != 7 {
		t.Errorf("scannedRows: %d want 7", out.ScannedRows)
	}
	if out.Totals.Calls != 7 || out.Totals.TotalTokens != 675 {
		t.Errorf("totals trimmed with topN: calls=%d totalTokens=%d want 7/675",
			out.Totals.Calls, out.Totals.TotalTokens)
	}
}

// TestCollectBotTokenUsage_Empty — no data path. The shape must still be valid
// JSON with bots:[] (not null) and zeroed totals, or callers expecting an array
// break.
func TestCollectBotTokenUsage_Empty(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.ExpectExec(regexp.QuoteMeta("USE `acore_characters`")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta("FROM `mod_ollama_chat_gateway_audit`")).
		WillReturnRows(sqlmock.NewRows([]string{"bot_guid", "prompt_tokens", "completion_tokens", "request_chars", "response_chars", "error"}))

	out, err := collectBotTokenUsage(context.Background(), db, 0, "", 25, false)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if out.ScannedRows != 0 {
		t.Errorf("scanned: %d want 0", out.ScannedRows)
	}
	if out.Bots == nil || len(out.Bots) != 0 {
		t.Errorf("bots want non-nil empty slice: %+v", out.Bots)
	}
	if (out.Totals != botTokenUsageTotals{}) {
		t.Errorf("totals want zero-value: %+v", out.Totals)
	}
	// Non-nil empty slice marshals to [] not null.
	blob, _ := json.Marshal(out)
	if !strings.Contains(string(blob), `"bots":[]`) {
		t.Errorf("empty bots should marshal to []: %s", blob)
	}
}
