package mcpserver

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

// TestWowBotLookup_DescriptionMentionsContext guards the description keywords —
// same intent as wow_player_lookup's description test: drop the "composite +
// audit + bot flag" framing and the agent falls back to wow_player_lookup +
// chained ops_audit_* calls.
func TestWowBotLookup_DescriptionMentionsContext(t *testing.T) {
	reg := NewRegistry()
	RegisterWowBotLookupTool(reg, DBDeps{})
	tool, ok := reg.Get("wow_bot_lookup")
	if !ok {
		t.Fatal("wow_bot_lookup not registered")
	}
	for _, kw := range []string{
		"Composite", "bot", "character", "account", "gm", "bans", "recentAudit",
		"RNDBOT", "accountPrefix", "auditLimit", "ops_audit_gateway", "ops_audit_tactical",
	} {
		if !strings.Contains(tool.Description, kw) {
			t.Errorf("description missing %q: %s", kw, tool.Description)
		}
	}
}

func TestWowBotLookup_NoPoolReturnsError(t *testing.T) {
	reg := NewRegistry()
	RegisterWowBotLookupTool(reg, DBDeps{}) // QueryDB nil
	tool, _ := reg.Get("wow_bot_lookup")
	resp := tool.Handler(context.Background(),
		json.RawMessage(`{"name":"RNDBOT_Slayo"}`), "")
	got, _ := resp.(map[string]any)
	if msg, _ := got["error"].(string); !strings.Contains(msg, "ops_ro pool not configured") {
		t.Errorf("expected pool-not-configured error, got %v", got)
	}
}

func TestWowBotLookup_ArgValidation(t *testing.T) {
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	reg := NewRegistry()
	RegisterWowBotLookupTool(reg, DBDeps{QueryDB: db})
	tool, _ := reg.Get("wow_bot_lookup")

	cases := []struct {
		name    string
		args    string
		wantSub string
	}{
		{"missing identifier", `{}`, "either name or guid is required"},
		{"both identifiers", `{"name":"X","guid":1}`, "pass either name or guid"},
		{"wildcard prefix %", `{"name":"X","accountPrefix":"RND%"}`, "wildcards"},
		{"wildcard prefix _", `{"name":"X","accountPrefix":"R_NDB"}`, "wildcards"},
		{"bad json", `{"auditLimit":"oops"}`, "decode args"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resp := tool.Handler(context.Background(), json.RawMessage(c.args), "")
			got, _ := resp.(map[string]any)
			msg, _ := got["error"].(string)
			if !strings.Contains(msg, c.wantSub) {
				t.Errorf("error: %q want substring %q", msg, c.wantSub)
			}
		})
	}
}

// TestCollectBotLookup_NotFound — typo'd name returns found:false rather than
// erroring. Same contract wow_player_lookup makes (and the bot-flavored caller
// can branch on `found` without parsing an error string).
func TestCollectBotLookup_NotFound(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectExec(regexp.QuoteMeta("USE `acore_characters`")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta("FROM `characters` WHERE name = ?")).
		WithArgs("NoSuch").
		WillReturnRows(sqlmock.NewRows([]string{
			"guid", "account", "name", "race", "class", "gender", "level",
			"online", "money", "totaltime", "leveltime", "logout_time",
			"zone", "map", "position_x", "position_y", "position_z",
		}))

	out, err := collectBotLookup(context.Background(), db, "NoSuch", 0, "RNDBOT", 10)
	if err != nil {
		t.Fatalf("collectBotLookup: %v", err)
	}
	if found, _ := out["found"].(bool); found {
		t.Errorf("expected found=false, got %v", out)
	}
	if got := out["lookupBy"]; got != "name" {
		t.Errorf("lookupBy: %v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestCollectBotLookup_Golden drives the full happy path — character + ban
// scans, both audit-table fetches, account / GM / account_ban scans, and the
// bot-flag classification. Covers the edge cases that would silently break
// the response: NULL columns in tactical_audit (error / escalation_reason),
// case-insensitive prefix match (lowercase config prefix vs uppercase
// username), and the gateway-audit error-flag coercion.
func TestCollectBotLookup_Golden(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectExec(regexp.QuoteMeta("USE `acore_characters`")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta("FROM `characters` WHERE name = ?")).
		WithArgs("Botzz").
		WillReturnRows(sqlmock.NewRows([]string{
			"guid", "account", "name", "race", "class", "gender", "level",
			"online", "money", "totaltime", "leveltime", "logout_time",
			"zone", "map", "position_x", "position_y", "position_z",
		}).AddRow(
			int64(20007), int64(5001), "Botzz", 1, 1, 0, 80,
			int(1), int64(0), int64(0), int64(0), nil,
			1519, 0, 0.0, 0.0, 0.0,
		))
	mock.ExpectQuery(regexp.QuoteMeta("FROM `character_banned` WHERE guid = ? AND active = 1")).
		WithArgs(int64(20007)).
		WillReturnRows(sqlmock.NewRows([]string{"bandate", "unbandate", "bannedby", "banreason"}))

	// gateway_audit: 2 rows, one OK + one error
	ts1 := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	ts2 := time.Date(2026, 5, 17, 11, 30, 0, 0, time.UTC)
	mock.ExpectQuery(regexp.QuoteMeta("FROM `mod_ollama_chat_gateway_audit` WHERE bot_guid = ?")).
		WithArgs(int64(20007), 10).
		WillReturnRows(sqlmock.NewRows([]string{
			"ts", "player_guid", "account_id", "request_chars", "response_chars",
			"prompt_tokens", "completion_tokens", "latency_ms", "source_channel", "error",
		}).
			AddRow(ts1, int64(1), int64(5001), 1024, 512, 300, 150, 850, "say", int(0)).
			AddRow(ts2, int64(1), int64(5001), 800, 0, 250, 0, 1200, "whisper", int(1)))

	// tactical_audit: 1 row, escalated with reason, NULL error string
	ts3 := time.Date(2026, 5, 17, 11, 45, 0, 0, time.UTC)
	mock.ExpectQuery(regexp.QuoteMeta("FROM `mod_ollama_chat_tactical_audit` WHERE bot_guid = ?")).
		WithArgs(int64(20007), 10).
		WillReturnRows(sqlmock.NewRows([]string{
			"ts", "bot_name", "action", "result", "error",
			"escalated", "escalation_reason", "latency_ms", "in_combat",
		}).AddRow(
			ts3, "Botzz", "tick", "ok", nil,
			int(1), "in_combat_no_target", 42, int(1),
		))

	mock.ExpectExec(regexp.QuoteMeta("USE `acore_auth`")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta("FROM `account` WHERE id = ?")).
		WithArgs(int64(5001)).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "username", "email", "last_ip", "last_login",
			"locked", "online", "joindate", "expansion",
		}).AddRow(
			int64(5001), "RNDBOT_BOTZZ", nil, "127.0.0.1",
			"2026-05-09 18:00:00", int(0), int(1), "2025-01-01 00:00:00", 2,
		))
	mock.ExpectQuery(regexp.QuoteMeta("FROM `account_access` WHERE id = ?")).
		WithArgs(int64(5001)).
		WillReturnRows(sqlmock.NewRows([]string{"gmlevel", "RealmID"}))
	mock.ExpectQuery(regexp.QuoteMeta("FROM `account_banned` WHERE id = ? AND active = 1")).
		WithArgs(int64(5001)).
		WillReturnRows(sqlmock.NewRows([]string{"bandate", "unbandate", "bannedby", "banreason"}))

	// Pass lowercase prefix to confirm case-insensitive match against uppercase username.
	out, err := collectBotLookup(context.Background(), db, "Botzz", 0, "rndbot", 10)
	if err != nil {
		t.Fatalf("collectBotLookup: %v", err)
	}

	if found, _ := out["found"].(bool); !found {
		t.Fatalf("expected found=true, got %v", out)
	}

	bot, _ := out["bot"].(botFlag)
	if !bot.IsBot {
		t.Errorf("isBot: expected true (RNDBOT_BOTZZ matches \"rndbot\" case-insensitive)")
	}
	if bot.AccountPrefix != "rndbot" {
		t.Errorf("accountPrefix: %q want \"rndbot\"", bot.AccountPrefix)
	}
	if bot.Source != "auth-username-prefix" {
		t.Errorf("source: %q want auth-username-prefix", bot.Source)
	}

	audit, _ := out["recentAudit"].(map[string]any)
	if audit == nil {
		t.Fatal("recentAudit missing from response")
	}
	if audit["auditLimit"] != 10 {
		t.Errorf("auditLimit echo: %v", audit["auditLimit"])
	}

	gw, _ := audit["gateway"].([]gatewayAuditRow)
	if len(gw) != 2 {
		t.Fatalf("gateway rows: %d want 2", len(gw))
	}
	if gw[0].Error || !gw[1].Error {
		t.Errorf("gateway error coercion: row0=%v row1=%v (want false,true)", gw[0].Error, gw[1].Error)
	}
	if gw[0].SourceChannel != "say" || gw[0].LatencyMs != 850 {
		t.Errorf("gateway row0 fields: %+v", gw[0])
	}
	if gw[0].TsISO != "2026-05-17T12:00:00Z" {
		t.Errorf("gateway tsIso: %q", gw[0].TsISO)
	}

	tac, _ := audit["tactical"].([]tacticalAuditRow)
	if len(tac) != 1 {
		t.Fatalf("tactical rows: %d want 1", len(tac))
	}
	if tac[0].Error != "" {
		t.Errorf("NULL error column should coerce to empty string, got %q", tac[0].Error)
	}
	if !tac[0].Escalated || tac[0].EscalationReason != "in_combat_no_target" {
		t.Errorf("tactical escalation: %+v", tac[0])
	}
	if !tac[0].InCombat {
		t.Errorf("tactical inCombat flag did not coerce: %+v", tac[0])
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestCollectBotLookup_NonBotAccount — the character exists but the owning
// account doesn't match the prefix. Confirms isBot=false is set explicitly
// (operators rely on the flag to differentiate "player account I should
// investigate as a person" vs "bot account I should investigate as automation").
// Also exercises the empty-audit-table path: scanGatewayAuditForBot /
// scanTacticalAuditForBot must return [] (not nil) so JSON encodes as an empty
// array rather than null.
func TestCollectBotLookup_NonBotAccount(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectExec(regexp.QuoteMeta("USE `acore_characters`")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta("FROM `characters` WHERE guid = ?")).
		WithArgs(int64(42)).
		WillReturnRows(sqlmock.NewRows([]string{
			"guid", "account", "name", "race", "class", "gender", "level",
			"online", "money", "totaltime", "leveltime", "logout_time",
			"zone", "map", "position_x", "position_y", "position_z",
		}).AddRow(
			int64(42), int64(1001), "Slayo", 1, 5, 0, 80,
			int(0), int64(0), int64(0), int64(0), int64(1715283600),
			0, 1, 0.0, 0.0, 0.0,
		))
	mock.ExpectQuery(regexp.QuoteMeta("FROM `character_banned` WHERE guid = ? AND active = 1")).
		WithArgs(int64(42)).
		WillReturnRows(sqlmock.NewRows([]string{"bandate", "unbandate", "bannedby", "banreason"}))
	mock.ExpectQuery(regexp.QuoteMeta("FROM `mod_ollama_chat_gateway_audit` WHERE bot_guid = ?")).
		WithArgs(int64(42), 5).
		WillReturnRows(sqlmock.NewRows([]string{
			"ts", "player_guid", "account_id", "request_chars", "response_chars",
			"prompt_tokens", "completion_tokens", "latency_ms", "source_channel", "error",
		}))
	mock.ExpectQuery(regexp.QuoteMeta("FROM `mod_ollama_chat_tactical_audit` WHERE bot_guid = ?")).
		WithArgs(int64(42), 5).
		WillReturnRows(sqlmock.NewRows([]string{
			"ts", "bot_name", "action", "result", "error",
			"escalated", "escalation_reason", "latency_ms", "in_combat",
		}))

	mock.ExpectExec(regexp.QuoteMeta("USE `acore_auth`")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta("FROM `account` WHERE id = ?")).
		WithArgs(int64(1001)).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "username", "email", "last_ip", "last_login",
			"locked", "online", "joindate", "expansion",
		}).AddRow(
			int64(1001), "OPERATOR", nil, "192.168.100.14",
			"2026-05-09 18:00:00", int(0), int(0), "2025-01-01 00:00:00", 2,
		))
	mock.ExpectQuery(regexp.QuoteMeta("FROM `account_access` WHERE id = ?")).
		WithArgs(int64(1001)).
		WillReturnRows(sqlmock.NewRows([]string{"gmlevel", "RealmID"}).AddRow(3, -1))
	mock.ExpectQuery(regexp.QuoteMeta("FROM `account_banned` WHERE id = ? AND active = 1")).
		WithArgs(int64(1001)).
		WillReturnRows(sqlmock.NewRows([]string{"bandate", "unbandate", "bannedby", "banreason"}))

	out, err := collectBotLookup(context.Background(), db, "", 42, "RNDBOT", 5)
	if err != nil {
		t.Fatalf("collectBotLookup: %v", err)
	}

	bot, _ := out["bot"].(botFlag)
	if bot.IsBot {
		t.Errorf("isBot: expected false for OPERATOR (no RNDBOT prefix), got %+v", bot)
	}

	audit, _ := out["recentAudit"].(map[string]any)
	gw, _ := audit["gateway"].([]gatewayAuditRow)
	if gw == nil {
		t.Errorf("gateway rows: expected non-nil empty slice (for clean JSON []), got nil")
	}
	if len(gw) != 0 {
		t.Errorf("gateway rows: %d want 0", len(gw))
	}
	tac, _ := audit["tactical"].([]tacticalAuditRow)
	if tac == nil {
		t.Errorf("tactical rows: expected non-nil empty slice, got nil")
	}
	if len(tac) != 0 {
		t.Errorf("tactical rows: %d want 0", len(tac))
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestClassifyBot exercises the prefix-comparison cases that matter:
// case-insensitive matching both ways, exact-prefix vs substring (must be a
// prefix, not a contains match), and empty username (orphan account fallback).
func TestClassifyBot(t *testing.T) {
	cases := []struct {
		username string
		prefix   string
		want     bool
	}{
		{"RNDBOT_BOTZZ", "RNDBOT", true},
		{"RNDBOT_BOTZZ", "rndbot", true}, // lowercase config still matches
		{"rndbot_xyz", "RNDBOT", true},   // lowercase username still matches
		{"OPERATOR", "RNDBOT", false},
		{"FOO_RNDBOT_BAR", "RNDBOT", false}, // RNDBOT inside the name is NOT a prefix match
		{"", "RNDBOT", false},
		{"RND", "RNDBOT", false}, // username shorter than prefix
	}
	for _, c := range cases {
		got := classifyBot(c.username, c.prefix)
		if got.IsBot != c.want {
			t.Errorf("classifyBot(%q,%q).IsBot=%v want %v", c.username, c.prefix, got.IsBot, c.want)
		}
		if got.AccountPrefix != c.prefix {
			t.Errorf("classifyBot prefix echo: %q want %q", got.AccountPrefix, c.prefix)
		}
		if got.Source != "auth-username-prefix" {
			t.Errorf("classifyBot source: %q", got.Source)
		}
	}
}
