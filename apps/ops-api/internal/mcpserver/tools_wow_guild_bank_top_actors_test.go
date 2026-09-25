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

// gbActorScanCols is the guild_bank_eventlog column set the tool scans, in order.
func gbActorScanCols() []string {
	return []string{"guildid", "EventType", "PlayerGuid", "ItemOrMoney", "TimeStamp"}
}

// expectEmptyActorScan queues the USE + guild_bank_eventlog scan with no rows.
// Used by the arg-clamp tests where the fold result doesn't matter (empty -> no
// stage-2/3 queries fire, so only these two expectations are needed).
func expectEmptyActorScan(mock sqlmock.Sqlmock) {
	mock.ExpectExec(regexp.QuoteMeta("USE `acore_characters`")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta("FROM `guild_bank_eventlog`")).
		WillReturnRows(sqlmock.NewRows(gbActorScanCols()))
}

// TestWowGuildBankTopActors_DescriptionMentionsContext guards the description
// keywords — they shape which tool the agent reaches for when an operator asks
// "who moves the most gold in the guild bank?" / "are bots churning guild banks?".
// Drop the actor/bot/money framing and the agent falls back to
// wow_guild_bank_summary (guild grain, no actor) or a hand-written eventlog join.
func TestWowGuildBankTopActors_DescriptionMentionsContext(t *testing.T) {
	reg := NewRegistry()
	RegisterWowGuildBankTopActorsTool(reg, DBDeps{})
	tool, ok := reg.Get("wow_guild_bank_top_actors")
	if !ok {
		t.Fatal("wow_guild_bank_top_actors not registered")
	}
	for _, kw := range []string{
		"guild_bank_eventlog", "acore_characters", "acore_auth.account", "ops_ro",
		"PlayerGuid", "RNDBOT", "isBot", "wow_guild_bank_summary", "wow_bot_lookup",
		"Three-stage", "topN", "sortBy", "guildId", "sinceHours", "minEvents",
		"unresolved", "moneyIn", "moneyOut", "netMoney", "recency", "itemDeposits",
		"Read-only",
	} {
		if !strings.Contains(tool.Description, kw) {
			t.Errorf("description missing %q", kw)
		}
	}
}

// TestWowGuildBankTopActors_ReadOnlyAnnotation pins readOnlyHint=true so a future
// copy-paste of a destructive sibling can't silently flip the gate.
func TestWowGuildBankTopActors_ReadOnlyAnnotation(t *testing.T) {
	reg := NewRegistry()
	RegisterWowGuildBankTopActorsTool(reg, DBDeps{})
	tool, _ := reg.Get("wow_guild_bank_top_actors")
	if !strings.Contains(string(tool.Annotations), `"readOnlyHint":true`) {
		t.Errorf("annotations missing readOnlyHint=true: %s", tool.Annotations)
	}
}

func TestWowGuildBankTopActors_NoPoolReturnsError(t *testing.T) {
	reg := NewRegistry()
	RegisterWowGuildBankTopActorsTool(reg, DBDeps{}) // QueryDB nil
	tool, _ := reg.Get("wow_guild_bank_top_actors")
	resp := tool.Handler(context.Background(), json.RawMessage(`{}`), "")
	got, _ := resp.(map[string]any)
	if msg, _ := got["error"].(string); !strings.Contains(msg, "ops_ro pool not configured") {
		t.Errorf("expected pool-not-configured error, got %v", got)
	}
}

// TestWowGuildBankTopActors_TopNClamps drives the handler and asserts the
// post-clamp topN echo (unset->default, zero->default, oversized->cap, in-range
// passthrough) and that the LIMIT bind is always scanCap+1 regardless of args.
func TestWowGuildBankTopActors_TopNClamps(t *testing.T) {
	cases := []struct {
		name     string
		args     string
		wantTopN int
	}{
		{"unset uses default", `{}`, gbTopActorsDefaultTopN},
		{"zero falls back to default", `{"topN":0}`, gbTopActorsDefaultTopN},
		{"negative falls back to default", `{"topN":-4}`, gbTopActorsDefaultTopN},
		{"oversized clamps to max", `{"topN":99999}`, gbTopActorsMaxTopN},
		{"in-range passes through", `{"topN":50}`, 50},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			mock.ExpectExec(regexp.QuoteMeta("USE `acore_characters`")).
				WillReturnResult(sqlmock.NewResult(0, 0))
			// No guildId / sinceHours -> no WHERE; LIMIT bind is scanCap+1.
			mock.ExpectQuery(regexp.QuoteMeta("FROM `guild_bank_eventlog` LIMIT ?")).
				WithArgs(int64(gbTopActorsScanCap + 1)).
				WillReturnRows(sqlmock.NewRows(gbActorScanCols()))

			reg := NewRegistry()
			RegisterWowGuildBankTopActorsTool(reg, DBDeps{QueryDB: db})
			tool, _ := reg.Get("wow_guild_bank_top_actors")
			resp := tool.Handler(context.Background(), json.RawMessage(c.args), "")
			got, _ := resp.(map[string]any)
			if got["error"] != nil {
				t.Fatalf("unexpected error: %v", got)
			}
			if n, _ := got["topN"].(int); n != c.wantTopN {
				t.Errorf("topN: %v want %d", got["topN"], c.wantTopN)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Errorf("unmet mock expectations: %v", err)
			}
		})
	}
}

// TestWowGuildBankTopActors_MinEventsClamps verifies the minEvents floor of 1
// (unset/zero/negative all clamp to 1) is echoed in the response.
func TestWowGuildBankTopActors_MinEventsClamps(t *testing.T) {
	cases := []struct {
		args    string
		wantMin int
	}{
		{`{}`, 1},
		{`{"minEvents":0}`, 1},
		{`{"minEvents":-5}`, 1},
		{`{"minEvents":3}`, 3},
	}
	for _, c := range cases {
		t.Run(c.args, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			expectEmptyActorScan(mock)

			reg := NewRegistry()
			RegisterWowGuildBankTopActorsTool(reg, DBDeps{QueryDB: db})
			tool, _ := reg.Get("wow_guild_bank_top_actors")
			resp := tool.Handler(context.Background(), json.RawMessage(c.args), "")
			got, _ := resp.(map[string]any)
			if got["error"] != nil {
				t.Fatalf("unexpected error: %v", got)
			}
			if m, _ := got["minEvents"].(int); m != c.wantMin {
				t.Errorf("minEvents: %v want %d", got["minEvents"], c.wantMin)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Errorf("unmet mock expectations: %v", err)
			}
		})
	}
}

// TestWowGuildBankTopActors_InvalidSortByRejected — an unknown sortBy is a clean
// argument error and NO query fires (validated before the pool is touched).
func TestWowGuildBankTopActors_InvalidSortByRejected(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	reg := NewRegistry()
	RegisterWowGuildBankTopActorsTool(reg, DBDeps{QueryDB: db})
	tool, _ := reg.Get("wow_guild_bank_top_actors")
	resp := tool.Handler(context.Background(), json.RawMessage(`{"sortBy":"bogus"}`), "")
	got, _ := resp.(map[string]any)
	if msg, _ := got["error"].(string); !strings.Contains(msg, "sortBy must be one of") {
		t.Errorf("expected sortBy error, got %v", got)
	}
	// No ExpectQuery queued -> ExpectationsWereMet passes only if none fired.
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations (no query should fire): %v", err)
	}
}

// TestWowGuildBankTopActors_NegativeGuildIdRejected — a non-positive guildId is a
// clean argument error and NO query fires.
func TestWowGuildBankTopActors_NegativeGuildIdRejected(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	reg := NewRegistry()
	RegisterWowGuildBankTopActorsTool(reg, DBDeps{QueryDB: db})
	tool, _ := reg.Get("wow_guild_bank_top_actors")
	resp := tool.Handler(context.Background(), json.RawMessage(`{"guildId":-1}`), "")
	got, _ := resp.(map[string]any)
	if msg, _ := got["error"].(string); !strings.Contains(msg, "guildId must be a positive integer") {
		t.Errorf("expected guildId error, got %v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations (no query should fire): %v", err)
	}
}

// TestWowGuildBankTopActors_GuildIdFilter verifies the optional guildId binds a
// `WHERE guildid = ?` (before the LIMIT) and is echoed.
func TestWowGuildBankTopActors_GuildIdFilter(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.ExpectExec(regexp.QuoteMeta("USE `acore_characters`")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta("WHERE guildid = ? LIMIT ?")).
		WithArgs(5, int64(gbTopActorsScanCap+1)).
		WillReturnRows(sqlmock.NewRows(gbActorScanCols()))

	reg := NewRegistry()
	RegisterWowGuildBankTopActorsTool(reg, DBDeps{QueryDB: db})
	tool, _ := reg.Get("wow_guild_bank_top_actors")
	resp := tool.Handler(context.Background(), json.RawMessage(`{"guildId":5}`), "")
	got, _ := resp.(map[string]any)
	if got["error"] != nil {
		t.Fatalf("unexpected error: %v", got)
	}
	if g, _ := got["guildId"].(int); g != 5 {
		t.Errorf("guildId echo: %v want 5", got["guildId"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestWowGuildBankTopActors_SinceHoursWindow drives collect with a fixed `now` and
// asserts the sinceHours cutoff bind math (now - N*3600) + the echoed sinceCutoff.
func TestWowGuildBankTopActors_SinceHoursWindow(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	now := time.Unix(1_700_000_000, 0)
	cutoff := int64(1_700_000_000 - 24*3600) // 1_699_913_600

	mock.ExpectExec(regexp.QuoteMeta("USE `acore_characters`")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta("WHERE TimeStamp >= ? LIMIT ?")).
		WithArgs(cutoff, int64(gbTopActorsScanCap+1)).
		WillReturnRows(sqlmock.NewRows(gbActorScanCols()))

	out, err := collectWowGuildBankTopActors(context.Background(), db, now, nil, "events", 24, 1, 25)
	if err != nil {
		t.Fatalf("collectWowGuildBankTopActors: %v", err)
	}
	if sh, _ := out["sinceHours"].(int); sh != 24 {
		t.Errorf("sinceHours echo: %v want 24", out["sinceHours"])
	}
	if sc, _ := out["sinceCutoff"].(string); sc != formatUnixISO(cutoff) {
		t.Errorf("sinceCutoff: %q want %q", sc, formatUnixISO(cutoff))
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestCollectWowGuildBankTopActors_Empty — empty log: actors must be a non-nil
// empty slice, distinctActors 0, unresolved 0, and NO stage-2/3 queries fire.
func TestCollectWowGuildBankTopActors_Empty(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	expectEmptyActorScan(mock)

	out, err := collectWowGuildBankTopActors(context.Background(), db, time.Unix(1_700_000_000, 0), nil, "events", 0, 1, 25)
	if err != nil {
		t.Fatalf("collectWowGuildBankTopActors: %v", err)
	}
	sl, ok := out["actors"].([]*gbActor)
	if !ok {
		t.Fatalf("actors type: %T want []*gbActor", out["actors"])
	}
	if len(sl) != 0 {
		t.Errorf("actors len: %d want 0", len(sl))
	}
	if d, _ := out["distinctActors"].(int); d != 0 {
		t.Errorf("distinctActors: %v want 0", out["distinctActors"])
	}
	if u, _ := out["unresolved"].(int); u != 0 {
		t.Errorf("unresolved: %v want 0", out["unresolved"])
	}
	if mm, _ := out["totalMoneyMovedGold"].(string); mm != "0c" {
		t.Errorf("totalMoneyMovedGold: %q want 0c", mm)
	}
	// sinceHours 0 -> no sinceCutoff key.
	if _, ok := out["sinceCutoff"]; ok {
		t.Errorf("sinceCutoff should be absent when sinceHours=0, got %v", out["sinceCutoff"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestCollectWowGuildBankTopActors_Golden drives the full three-stage fold: the
// money-event split (deposit=in, withdraw+repair=out, incl. a NEGATIVE net), the
// item-event counts, the minEvents filter (drops a 1-event actor), events-desc +
// guid-asc ordering, the characters batch resolution (incl. a deleted-character
// miss -> unresolved), the account batch resolution + RNDBOT bot flag, distinct
// guilds, recency, honest realm totals, and the (spaced) formatMoney gold strings.
func TestCollectWowGuildBankTopActors_Golden(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	now := time.Unix(1_700_000_000, 0)

	mock.ExpectExec(regexp.QuoteMeta("USE `acore_characters`")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta("FROM `guild_bank_eventlog` LIMIT ?")).
		WithArgs(int64(gbTopActorsScanCap + 1)).
		WillReturnRows(sqlmock.NewRows(gbActorScanCols()).
			// Actor 100 (AHBot): 5 events — money in 50000, out 20000+3000 (incl
			// 3000 repair), 1 item deposit + 1 item withdraw. net +27000.
			AddRow(int64(1), 4, int64(100), int64(50000), int64(1_699_990_000)).
			AddRow(int64(1), 5, int64(100), int64(20000), int64(1_699_995_000)).
			AddRow(int64(1), 6, int64(100), int64(3000), int64(1_699_999_000)).
			AddRow(int64(1), 1, int64(100), int64(12345), int64(1_699_980_000)).
			AddRow(int64(1), 2, int64(100), int64(12345), int64(1_699_985_000)).
			// Actor 200 (player): 3 events — one 80000 withdraw, two item moves
			// across TWO guilds (distinctGuilds 2). net -80000 (only withdrew).
			AddRow(int64(1), 5, int64(200), int64(80000), int64(1_699_998_000)).
			AddRow(int64(1), 3, int64(200), int64(999), int64(1_699_997_000)).
			AddRow(int64(2), 7, int64(200), int64(888), int64(1_699_996_000)).
			// Actor 300 (deleted character -> no characters row): 2 money events.
			AddRow(int64(1), 4, int64(300), int64(10000), int64(1_699_999_500)).
			AddRow(int64(1), 5, int64(300), int64(4000), int64(1_699_999_600)).
			// Actor 400: a single BUY_SLOT (9, neither money nor item) -> 1 event,
			// dropped by minEvents=2 and never resolved.
			AddRow(int64(1), 9, int64(400), int64(0), int64(1_699_990_500)))

	// Stage 2: characters lookup over the ranked guids (100,200,300 after minEvents=2
	// drops 400). 300 deliberately absent (deleted-character miss).
	mock.ExpectQuery(regexp.QuoteMeta("FROM `characters` WHERE guid IN (?,?,?)")).
		WithArgs(int64(100), int64(200), int64(300)).
		WillReturnRows(sqlmock.NewRows([]string{"guid", "name", "account"}).
			AddRow(int64(100), "Ahbotzug", int64(11)).
			AddRow(int64(200), "Slayo", int64(22)))

	// Stage 3: account lookup over the two resolved accounts. 11 is RNDBOT* (bot),
	// 22 is a human.
	mock.ExpectExec(regexp.QuoteMeta("USE `acore_auth`")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta("FROM `account` WHERE id IN (?,?)")).
		WithArgs(int64(11), int64(22)).
		WillReturnRows(sqlmock.NewRows([]string{"id", "username"}).
			AddRow(int64(11), "RNDBOT0007").
			AddRow(int64(22), "OPERATOR"))

	// minEvents=2 drops actor 400 (1 event); topN=10 keeps the rest.
	out, err := collectWowGuildBankTopActors(context.Background(), db, now, nil, "events", 0, 2, 10)
	if err != nil {
		t.Fatalf("collectWowGuildBankTopActors: %v", err)
	}

	if s, _ := out["scanned"].(int); s != 11 {
		t.Errorf("scanned: %v want 11", out["scanned"])
	}
	if d, _ := out["distinctActors"].(int); d != 4 {
		t.Errorf("distinctActors: %v want 4 (100,200,300,400 pre-minEvents)", out["distinctActors"])
	}
	if m, _ := out["matchedActors"].(int); m != 3 {
		t.Errorf("matchedActors: %v want 3 (minEvents=2 drops 400)", out["matchedActors"])
	}
	if u, _ := out["unresolved"].(int); u != 1 {
		t.Errorf("unresolved: %v want 1 (actor 300 deleted char)", out["unresolved"])
	}
	if ie, _ := out["totalItemEvents"].(int); ie != 4 {
		t.Errorf("totalItemEvents: %v want 4", out["totalItemEvents"])
	}
	if me, _ := out["totalMoneyEvents"].(int); me != 6 {
		t.Errorf("totalMoneyEvents: %v want 6", out["totalMoneyEvents"])
	}
	if mmc, _ := out["totalMoneyMovedCopper"].(int64); mmc != 167000 {
		t.Errorf("totalMoneyMovedCopper: %v want 167000", out["totalMoneyMovedCopper"])
	}
	if mmg, _ := out["totalMoneyMovedGold"].(string); mmg != "16g 70s 0c" {
		t.Errorf("totalMoneyMovedGold: %q want '16g 70s 0c'", mmg)
	}

	actors, _ := out["actors"].([]*gbActor)
	if len(actors) != 3 {
		t.Fatalf("actors len: %d want 3", len(actors))
	}

	// Order: 100 (5 events) > 200 (3) > 300 (2).
	a0 := actors[0]
	if a0.PlayerGuid != 100 || a0.TotalEvents != 5 {
		t.Errorf("actors[0]: %+v want guid=100 events=5", a0)
	}
	if a0.ItemDeposits != 1 || a0.ItemWithdrawals != 1 || a0.ItemMoves != 0 {
		t.Errorf("actors[0] items: dep=%d wd=%d move=%d want 1/1/0", a0.ItemDeposits, a0.ItemWithdrawals, a0.ItemMoves)
	}
	if a0.MoneyInCopper != 50000 || a0.MoneyOutCopper != 23000 || a0.RepairMoneyCopper != 3000 || a0.NetMoneyCopper != 27000 {
		t.Errorf("actors[0] money: in=%d out=%d repair=%d net=%d want 50000/23000/3000/27000",
			a0.MoneyInCopper, a0.MoneyOutCopper, a0.RepairMoneyCopper, a0.NetMoneyCopper)
	}
	if a0.MoneyInGold != "5g 0s 0c" || a0.MoneyOutGold != "2g 30s 0c" || a0.NetMoneyGold != "2g 70s 0c" {
		t.Errorf("actors[0] gold: in=%q out=%q net=%q want '5g 0s 0c'/'2g 30s 0c'/'2g 70s 0c'",
			a0.MoneyInGold, a0.MoneyOutGold, a0.NetMoneyGold)
	}
	if a0.CharacterName != "Ahbotzug" || a0.AccountID != 11 || !a0.IsBot {
		t.Errorf("actors[0] identity: name=%q acct=%d isBot=%v want Ahbotzug/11/true", a0.CharacterName, a0.AccountID, a0.IsBot)
	}
	if a0.DistinctGuilds != 1 {
		t.Errorf("actors[0] distinctGuilds: %d want 1", a0.DistinctGuilds)
	}
	// lastTS 1_699_999_000 -> now-1000s = 16m; first activity is the item deposit.
	if a0.RecencyHuman != "16m" {
		t.Errorf("actors[0] recency: %q want 16m", a0.RecencyHuman)
	}
	if a0.FirstActivityISO != formatUnixISO(1_699_980_000) || a0.LastActivityISO != formatUnixISO(1_699_999_000) {
		t.Errorf("actors[0] activity: first=%q last=%q", a0.FirstActivityISO, a0.LastActivityISO)
	}

	// Actor 200: negative net (only withdrew), two distinct guilds.
	a1 := actors[1]
	if a1.PlayerGuid != 200 || a1.TotalEvents != 3 || a1.ItemMoves != 2 {
		t.Errorf("actors[1]: %+v want guid=200 events=3 itemMoves=2", a1)
	}
	if a1.NetMoneyCopper != -80000 || a1.NetMoneyGold != "-8g 0s 0c" {
		t.Errorf("actors[1] net: copper=%d gold=%q want -80000/'-8g 0s 0c'", a1.NetMoneyCopper, a1.NetMoneyGold)
	}
	if a1.MoneyInGold != "0c" {
		t.Errorf("actors[1] moneyInGold: %q want 0c", a1.MoneyInGold)
	}
	if a1.DistinctGuilds != 2 {
		t.Errorf("actors[1] distinctGuilds: %d want 2", a1.DistinctGuilds)
	}
	if a1.CharacterName != "Slayo" || a1.AccountID != 22 || a1.IsBot {
		t.Errorf("actors[1] identity: name=%q acct=%d isBot=%v want Slayo/22/false", a1.CharacterName, a1.AccountID, a1.IsBot)
	}

	// Actor 300: deleted character -> empty name, account 0, not a bot; net +6000.
	a2 := actors[2]
	if a2.PlayerGuid != 300 || a2.CharacterName != "" || a2.AccountID != 0 || a2.IsBot {
		t.Errorf("actors[2] (deleted char): %+v want guid=300 empty-name acct0 not-bot", a2)
	}
	if a2.NetMoneyCopper != 6000 || a2.NetMoneyGold != "60s 0c" {
		t.Errorf("actors[2] net: copper=%d gold=%q want 6000/'60s 0c'", a2.NetMoneyCopper, a2.NetMoneyGold)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestTopNGuildBankActors_SortSlice exercises the pure helper: the minEvents
// filter, each sort key (events / moneyOut / recency, all desc) with guid-asc
// tiebreaker, slice to n, and the pre-slice matched count. Guards against Go
// map-iteration flake on ties.
func TestTopNGuildBankActors_SortSlice(t *testing.T) {
	// Base fixture rebuilt per sub-test so a prior sort doesn't leak ordering.
	build := func() map[int64]*gbActor {
		return map[int64]*gbActor{
			30: {PlayerGuid: 30, TotalEvents: 5, MoneyOutCopper: 100, MoneyInCopper: 10, lastTS: 10},
			10: {PlayerGuid: 10, TotalEvents: 5, MoneyOutCopper: 300, MoneyInCopper: 50, lastTS: 30}, // ties events with 30
			20: {PlayerGuid: 20, TotalEvents: 9, MoneyOutCopper: 200, MoneyInCopper: 20, lastTS: 20},
			40: {PlayerGuid: 40, TotalEvents: 1, MoneyOutCopper: 999, MoneyInCopper: 99, lastTS: 40}, // below minEvents=2
		}
	}

	t.Run("events desc + guid-asc tiebreak + minEvents filter + slice", func(t *testing.T) {
		out, matched := topNGuildBankActors(build(), "events", 2, 2)
		if matched != 3 {
			t.Errorf("matched: %d want 3 (40 filtered by minEvents=2)", matched)
		}
		if len(out) != 2 {
			t.Fatalf("len: %d want 2 (sliced to n)", len(out))
		}
		if out[0].PlayerGuid != 20 {
			t.Errorf("out[0]: %+v want guid=20 (events 9)", out[0])
		}
		if out[1].PlayerGuid != 10 {
			t.Errorf("out[1]: %+v want guid=10 (events 5, guid-asc tiebreak over 30)", out[1])
		}
	})

	t.Run("moneyOut desc", func(t *testing.T) {
		out, matched := topNGuildBankActors(build(), "moneyOut", 1, 4)
		if matched != 4 {
			t.Errorf("matched: %d want 4 (minEvents=1)", matched)
		}
		if out[0].PlayerGuid != 40 || out[1].PlayerGuid != 10 {
			t.Errorf("moneyOut order: [%d,%d,...] want [40,10,...]", out[0].PlayerGuid, out[1].PlayerGuid)
		}
	})

	t.Run("recency desc", func(t *testing.T) {
		out, _ := topNGuildBankActors(build(), "recency", 1, 4)
		if out[0].PlayerGuid != 40 || out[1].PlayerGuid != 10 {
			t.Errorf("recency order: [%d,%d,...] want [40,10,...]", out[0].PlayerGuid, out[1].PlayerGuid)
		}
	})
}

// TestTopNGuildBankActors_EmptyNonNil — an all-filtered map still returns a
// non-nil slice (callers JSON-encode it as []).
func TestTopNGuildBankActors_EmptyNonNil(t *testing.T) {
	m := map[int64]*gbActor{1: {PlayerGuid: 1, TotalEvents: 1}}
	out, matched := topNGuildBankActors(m, "events", 5, 10)
	if out == nil {
		t.Fatal("out is nil, want non-nil empty slice")
	}
	if len(out) != 0 || matched != 0 {
		t.Errorf("len=%d matched=%d want 0/0", len(out), matched)
	}
}

// TestAnnotateActorAccounts_DedupesAccountIds — two actor-characters on the same
// account bind the id ONCE in the IN list, and both rows get the username + bot
// flag. Actors with account 0 (unresolved character) are skipped entirely.
func TestAnnotateActorAccounts_DedupesAccountIds(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	actors := []*gbActor{
		{PlayerGuid: 100, AccountID: 11},
		{PlayerGuid: 101, AccountID: 11}, // same account -> deduped in IN list
		{PlayerGuid: 102, AccountID: 0},  // unresolved char -> skipped
	}
	mock.ExpectQuery(regexp.QuoteMeta("FROM `account` WHERE id IN (?)")).
		WithArgs(int64(11)).
		WillReturnRows(sqlmock.NewRows([]string{"id", "username"}).
			AddRow(int64(11), "RNDBOTAAA"))

	if err := annotateActorAccounts(context.Background(), conn, actors); err != nil {
		t.Fatalf("annotateActorAccounts: %v", err)
	}
	if !actors[0].IsBot || actors[0].AccountUsername != "RNDBOTAAA" {
		t.Errorf("actors[0]: %+v want RNDBOTAAA/bot", actors[0])
	}
	if !actors[1].IsBot || actors[1].AccountUsername != "RNDBOTAAA" {
		t.Errorf("actors[1] (same account): %+v want RNDBOTAAA/bot", actors[1])
	}
	if actors[2].AccountUsername != "" || actors[2].IsBot {
		t.Errorf("actors[2] (acct 0): %+v want empty/not-bot", actors[2])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestAnnotateActorAccounts_NoResolvedAccounts — when every actor has account 0
// (all characters unresolved), NO account query fires.
func TestAnnotateActorAccounts_NoResolvedAccounts(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	actors := []*gbActor{{PlayerGuid: 100, AccountID: 0}}
	if err := annotateActorAccounts(context.Background(), conn, actors); err != nil {
		t.Fatalf("annotateActorAccounts: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations (account query should not fire): %v", err)
	}
}

// TestGbActorFold_Sums — the money-vs-item split, repair break-out, first/last
// timestamp tracking, distinct-guild count, and the finalize-time net + gold.
func TestGbActorFold_Sums(t *testing.T) {
	a := &gbActor{PlayerGuid: 7}
	a.fold(gbtaDepositMoney, 1, 50000, 100)  // moneyIn 50000
	a.fold(gbtaWithdrawMoney, 1, 20000, 200) // moneyOut 20000
	a.fold(gbtaRepairMoney, 1, 3000, 150)    // moneyOut +3000, repair 3000
	a.fold(gbtaDepositItem, 2, 12345, 50)    // itemDeposits, new guild, earliest ts
	a.fold(gbtaMoveItem2, 2, 888, 300)       // itemMoves, latest ts

	if a.TotalEvents != 5 {
		t.Errorf("totalEvents: %d want 5", a.TotalEvents)
	}
	if a.MoneyInCopper != 50000 || a.MoneyOutCopper != 23000 || a.RepairMoneyCopper != 3000 {
		t.Errorf("money: in=%d out=%d repair=%d want 50000/23000/3000", a.MoneyInCopper, a.MoneyOutCopper, a.RepairMoneyCopper)
	}
	if a.ItemDeposits != 1 || a.ItemWithdrawals != 0 || a.ItemMoves != 1 {
		t.Errorf("items: dep=%d wd=%d move=%d want 1/0/1", a.ItemDeposits, a.ItemWithdrawals, a.ItemMoves)
	}
	if a.firstTS != 50 || a.lastTS != 300 {
		t.Errorf("ts: first=%d last=%d want 50/300", a.firstTS, a.lastTS)
	}
	if len(a.guilds) != 2 {
		t.Errorf("distinct guilds: %d want 2", len(a.guilds))
	}

	a.finalize(time.Unix(1000, 0))
	if a.NetMoneyCopper != 27000 || a.NetMoneyGold != "2g 70s 0c" {
		t.Errorf("net: copper=%d gold=%q want 27000/'2g 70s 0c'", a.NetMoneyCopper, a.NetMoneyGold)
	}
	if a.DistinctGuilds != 2 {
		t.Errorf("distinctGuilds: %d want 2", a.DistinctGuilds)
	}
	if a.RecencyHuman != "11m" { // now 1000 - lastTS 300 = 700s -> 11m
		t.Errorf("recency: %q want 11m", a.RecencyHuman)
	}
}
