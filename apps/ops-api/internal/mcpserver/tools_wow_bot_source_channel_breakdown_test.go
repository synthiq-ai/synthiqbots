package mcpserver

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// TestWowBotSourceChannelBreakdown_DescriptionMentionsContext guards the
// keywords that shape tool selection. Drop "Composite" or "source_channel"
// and the agent will keep reaching for ops_audit_gateway + manual aggregation
// instead.
func TestWowBotSourceChannelBreakdown_DescriptionMentionsContext(t *testing.T) {
	reg := NewRegistry()
	RegisterWowBotSourceChannelBreakdownTool(reg, DBDeps{})
	tool, ok := reg.Get("wow_bot_source_channel_breakdown")
	if !ok {
		t.Fatal("wow_bot_source_channel_breakdown not registered")
	}
	for _, kw := range []string{"Composite", "mod_ollama_chat_gateway_audit",
		"source_channel", "botGuid", "errorsOnly", "topN", "window", "168h",
		"avgLatencyMs", "pct"} {
		if !strings.Contains(tool.Description, kw) {
			t.Errorf("description missing %q", kw)
		}
	}
}

func TestWowBotSourceChannelBreakdown_NoPoolReturnsError(t *testing.T) {
	reg := NewRegistry()
	RegisterWowBotSourceChannelBreakdownTool(reg, DBDeps{}) // QueryDB nil
	tool, _ := reg.Get("wow_bot_source_channel_breakdown")
	resp := tool.Handler(context.Background(), json.RawMessage(`{}`), "")
	got, _ := resp.(map[string]any)
	if msg, _ := got["error"].(string); !strings.Contains(msg, "ops_ro pool not configured") {
		t.Errorf("expected pool-not-configured error, got %v", got)
	}
}

// TestWowBotSourceChannelBreakdown_WindowValidation exercises the parse + cap
// checks. Bad windows must fail fast at the handler so the SQL never runs.
func TestWowBotSourceChannelBreakdown_WindowValidation(t *testing.T) {
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	reg := NewRegistry()
	RegisterWowBotSourceChannelBreakdownTool(reg, DBDeps{QueryDB: db})
	tool, _ := reg.Get("wow_bot_source_channel_breakdown")
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

// TestWowBotSourceChannelBreakdown_FiltersAndRowCap verifies the SQL takes
// shape with the bot/error filters AND that the row-cap placeholder lands at
// the LIMIT slot. Without these guards we'd risk an unbounded scan of the
// audit table.
func TestWowBotSourceChannelBreakdown_FiltersAndRowCap(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.ExpectExec(regexp.QuoteMeta("USE `acore_characters`")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta(
		"FROM `mod_ollama_chat_gateway_audit` WHERE ts >= ? AND bot_guid = ? AND error = 1 ORDER BY bot_guid LIMIT ?")).
		WithArgs(sqlmock.AnyArg(), uint64(20007), int64(wowBotSrcChanBreakdownRowCap)).
		WillReturnRows(sqlmock.NewRows([]string{"bot_guid", "source_channel", "latency_ms", "error"}))

	reg := NewRegistry()
	RegisterWowBotSourceChannelBreakdownTool(reg, DBDeps{QueryDB: db})
	tool, _ := reg.Get("wow_bot_source_channel_breakdown")
	resp := tool.Handler(context.Background(),
		json.RawMessage(`{"botGuid":20007,"errorsOnly":true,"window":"1h"}`), "")
	if got, _ := resp.(map[string]any); got != nil && got["error"] != nil {
		t.Fatalf("unexpected error: %v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestCollectBotSourceChannelBreakdown_Golden drives the full happy path. Two
// bots; bot 100 spans three channels with varying latencies, bot 200 is
// single-channel. Validates: per-channel calls/errors/avg, percentage math,
// channels-desc sort within a bot, and bots-desc sort.
func TestCollectBotSourceChannelBreakdown_Golden(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.ExpectExec(regexp.QuoteMeta("USE `acore_characters`")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	rows := sqlmock.NewRows([]string{"bot_guid", "source_channel", "latency_ms", "error"})
	// Bot 100: 5 SAY (200ms, 1 error), 3 YELL (400ms, no errors), 2 WHISPER (100ms, no errors).
	for i := 0; i < 5; i++ {
		errFlag := uint8(0)
		if i == 0 {
			errFlag = 1
		}
		rows.AddRow(uint64(100), "SAY", uint64(200), errFlag)
	}
	for i := 0; i < 3; i++ {
		rows.AddRow(uint64(100), "YELL", uint64(400), uint8(0))
	}
	for i := 0; i < 2; i++ {
		rows.AddRow(uint64(100), "WHISPER", uint64(100), uint8(0))
	}
	// Bot 200: 4 SAY (50ms each).
	for i := 0; i < 4; i++ {
		rows.AddRow(uint64(200), "SAY", uint64(50), uint8(0))
	}
	mock.ExpectQuery(regexp.QuoteMeta("FROM `mod_ollama_chat_gateway_audit`")).
		WillReturnRows(rows)

	out, err := collectBotSourceChannelBreakdown(context.Background(), db, 0, "", 25, false)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if out.ScannedRows != 14 {
		t.Errorf("scanned: %d want 14", out.ScannedRows)
	}
	if len(out.Bots) != 2 {
		t.Fatalf("bots: %d want 2", len(out.Bots))
	}
	b1, b2 := out.Bots[0], out.Bots[1]
	// Bot 100 (10 calls) outranks bot 200 (4 calls).
	if b1.BotGuid != 100 || b1.Calls != 10 {
		t.Errorf("b1 guid/calls: %d/%d want 100/10", b1.BotGuid, b1.Calls)
	}
	if len(b1.Channels) != 3 {
		t.Fatalf("b1 channels: %d want 3", len(b1.Channels))
	}
	// Channels sorted by calls desc: SAY(5) > YELL(3) > WHISPER(2).
	if b1.Channels[0].Channel != "SAY" || b1.Channels[0].Calls != 5 {
		t.Errorf("b1 ch0: %+v", b1.Channels[0])
	}
	if b1.Channels[0].Pct != 50.0 {
		t.Errorf("b1 SAY pct: %v want 50.0", b1.Channels[0].Pct)
	}
	if b1.Channels[0].Errors != 1 {
		t.Errorf("b1 SAY errors: %d want 1", b1.Channels[0].Errors)
	}
	if b1.Channels[0].AvgLatencyMs != 200 {
		t.Errorf("b1 SAY avg: %d want 200", b1.Channels[0].AvgLatencyMs)
	}
	if b1.Channels[1].Channel != "YELL" || b1.Channels[1].Calls != 3 {
		t.Errorf("b1 ch1: %+v", b1.Channels[1])
	}
	if b1.Channels[1].Pct != 30.0 {
		t.Errorf("b1 YELL pct: %v want 30.0", b1.Channels[1].Pct)
	}
	if b1.Channels[1].AvgLatencyMs != 400 {
		t.Errorf("b1 YELL avg: %d want 400", b1.Channels[1].AvgLatencyMs)
	}
	if b1.Channels[2].Channel != "WHISPER" || b1.Channels[2].Calls != 2 {
		t.Errorf("b1 ch2: %+v", b1.Channels[2])
	}
	if b1.Channels[2].Pct != 20.0 {
		t.Errorf("b1 WHISPER pct: %v want 20.0", b1.Channels[2].Pct)
	}

	if b2.BotGuid != 200 || b2.Calls != 4 {
		t.Errorf("b2 guid/calls: %d/%d want 200/4", b2.BotGuid, b2.Calls)
	}
	if len(b2.Channels) != 1 {
		t.Fatalf("b2 channels: %d want 1", len(b2.Channels))
	}
	if b2.Channels[0].Channel != "SAY" || b2.Channels[0].Pct != 100.0 {
		t.Errorf("b2 single channel: %+v", b2.Channels[0])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestCollectBotSourceChannelBreakdown_TopNTrims verifies topN slices the
// bots list after sorting. A drilldown view shouldn't return 1000 bots when
// the operator asked for 2.
func TestCollectBotSourceChannelBreakdown_TopNTrims(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.ExpectExec(regexp.QuoteMeta("USE `acore_characters`")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	rows := sqlmock.NewRows([]string{"bot_guid", "source_channel", "latency_ms", "error"})
	// Three bots with different call counts: 5, 3, 1.
	for i := 0; i < 5; i++ {
		rows.AddRow(uint64(1), "SAY", uint64(10), uint8(0))
	}
	for i := 0; i < 3; i++ {
		rows.AddRow(uint64(2), "SAY", uint64(20), uint8(0))
	}
	rows.AddRow(uint64(3), "SAY", uint64(30), uint8(0))
	mock.ExpectQuery(regexp.QuoteMeta("FROM `mod_ollama_chat_gateway_audit`")).
		WillReturnRows(rows)

	out, err := collectBotSourceChannelBreakdown(context.Background(), db, 0, "", 2, false)
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

// TestCollectBotSourceChannelBreakdown_Empty — no data path. The shape must
// still be valid JSON with bots:[] (not null), or callers expecting an array
// break.
func TestCollectBotSourceChannelBreakdown_Empty(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.ExpectExec(regexp.QuoteMeta("USE `acore_characters`")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta("FROM `mod_ollama_chat_gateway_audit`")).
		WillReturnRows(sqlmock.NewRows([]string{"bot_guid", "source_channel", "latency_ms", "error"}))

	out, err := collectBotSourceChannelBreakdown(context.Background(), db, 0, "", 25, false)
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
