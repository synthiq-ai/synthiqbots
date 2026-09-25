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

// gmtScanCols is the guild_eventlog column set the tool scans, in order.
func gmtScanCols() []string {
	return []string{"guildid", "EventType", "PlayerGuid2", "NewRank", "TimeStamp"}
}

// expectEmptyTargetScan queues the USE + guild_eventlog scan with no rows. Used
// by the arg-clamp tests where the fold result doesn't matter (empty -> no
// stage-2/3 queries fire, so only these two expectations are needed).
func expectEmptyTargetScan(mock sqlmock.Sqlmock) {
	mock.ExpectExec(regexp.QuoteMeta("USE `acore_characters`")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta("FROM `guild_eventlog` WHERE EventType IN (1,3,4,5)")).
		WillReturnRows(sqlmock.NewRows(gmtScanCols()))
}

// TestWowGuildMembershipTargets_DescriptionMentionsContext guards the description
// keywords — they shape which tool the agent reaches for when an operator asks
// "who's being rank-yo-yo'd?" / "who's getting mass-invited or kicked?". Drop the
// target/rank-churn framing and the agent falls back to a hand-written
// guild_eventlog join or the actor-axis tool (which answers a different
// question — who is DOING it, not who it's done TO).
func TestWowGuildMembershipTargets_DescriptionMentionsContext(t *testing.T) {
	reg := NewRegistry()
	RegisterWowGuildMembershipTargetsTool(reg, DBDeps{})
	tool, ok := reg.Get("wow_guild_membership_targets")
	if !ok {
		t.Fatal("wow_guild_membership_targets not registered")
	}
	for _, kw := range []string{
		"guild_eventlog", "acore_characters", "acore_auth.account", "ops_ro",
		"PlayerGuid2", "RNDBOT", "isBot", "Three-stage", "topN", "sortBy",
		"guildId", "sinceHours", "minEvents", "unresolved", "rankChurn",
		"promoted", "uninvited", "invited", "hasRankEvent", "latestNewRank",
		"JOIN_GUILD", "LEAVE_GUILD", "Read-only",
	} {
		if !strings.Contains(tool.Description, kw) {
			t.Errorf("description missing %q", kw)
		}
	}
}

// TestWowGuildMembershipTargets_ReadOnlyAnnotation pins readOnlyHint=true so a
// future copy-paste of a destructive sibling can't silently flip the gate.
func TestWowGuildMembershipTargets_ReadOnlyAnnotation(t *testing.T) {
	reg := NewRegistry()
	RegisterWowGuildMembershipTargetsTool(reg, DBDeps{})
	tool, _ := reg.Get("wow_guild_membership_targets")
	if !strings.Contains(string(tool.Annotations), `"readOnlyHint":true`) {
		t.Errorf("annotations missing readOnlyHint=true: %s", tool.Annotations)
	}
}

func TestWowGuildMembershipTargets_NoPoolReturnsError(t *testing.T) {
	reg := NewRegistry()
	RegisterWowGuildMembershipTargetsTool(reg, DBDeps{}) // QueryDB nil
	tool, _ := reg.Get("wow_guild_membership_targets")
	resp := tool.Handler(context.Background(), json.RawMessage(`{}`), "")
	got, _ := resp.(map[string]any)
	if msg, _ := got["error"].(string); !strings.Contains(msg, "ops_ro pool not configured") {
		t.Errorf("expected pool-not-configured error, got %v", got)
	}
}

// TestWowGuildMembershipTargets_TopNClamps drives the handler and asserts the
// post-clamp topN echo (unset->default, zero->default, oversized->cap, in-range
// passthrough) and that the LIMIT bind is always scanCap+1 regardless of args.
func TestWowGuildMembershipTargets_TopNClamps(t *testing.T) {
	cases := []struct {
		name     string
		args     string
		wantTopN int
	}{
		{"unset uses default", `{}`, gmtDefaultTopN},
		{"zero falls back to default", `{"topN":0}`, gmtDefaultTopN},
		{"negative falls back to default", `{"topN":-4}`, gmtDefaultTopN},
		{"oversized clamps to max", `{"topN":99999}`, gmtMaxTopN},
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
			// No guildId / sinceHours -> no extra AND; LIMIT bind is scanCap+1.
			mock.ExpectQuery(regexp.QuoteMeta("FROM `guild_eventlog` WHERE EventType IN (1,3,4,5) LIMIT ?")).
				WithArgs(int64(gmtScanCap + 1)).
				WillReturnRows(sqlmock.NewRows(gmtScanCols()))

			reg := NewRegistry()
			RegisterWowGuildMembershipTargetsTool(reg, DBDeps{QueryDB: db})
			tool, _ := reg.Get("wow_guild_membership_targets")
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

// TestWowGuildMembershipTargets_MinEventsClamps verifies the minEvents floor of 1
// (unset/zero/negative all clamp to 1) is echoed in the response.
func TestWowGuildMembershipTargets_MinEventsClamps(t *testing.T) {
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
			expectEmptyTargetScan(mock)

			reg := NewRegistry()
			RegisterWowGuildMembershipTargetsTool(reg, DBDeps{QueryDB: db})
			tool, _ := reg.Get("wow_guild_membership_targets")
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

// TestWowGuildMembershipTargets_InvalidSortByRejected — an unknown sortBy is a
// clean argument error and NO query fires.
func TestWowGuildMembershipTargets_InvalidSortByRejected(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	reg := NewRegistry()
	RegisterWowGuildMembershipTargetsTool(reg, DBDeps{QueryDB: db})
	tool, _ := reg.Get("wow_guild_membership_targets")
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

// TestWowGuildMembershipTargets_NegativeGuildIdRejected — a non-positive guildId
// is a clean argument error and NO query fires.
func TestWowGuildMembershipTargets_NegativeGuildIdRejected(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	reg := NewRegistry()
	RegisterWowGuildMembershipTargetsTool(reg, DBDeps{QueryDB: db})
	tool, _ := reg.Get("wow_guild_membership_targets")
	resp := tool.Handler(context.Background(), json.RawMessage(`{"guildId":-1}`), "")
	got, _ := resp.(map[string]any)
	if msg, _ := got["error"].(string); !strings.Contains(msg, "guildId must be a positive integer") {
		t.Errorf("expected guildId error, got %v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations (no query should fire): %v", err)
	}
}

// TestWowGuildMembershipTargets_GuildIdFilter verifies the optional guildId binds
// an `AND guildid = ?` (after the base officer-event WHERE, before the LIMIT) and
// is echoed.
func TestWowGuildMembershipTargets_GuildIdFilter(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.ExpectExec(regexp.QuoteMeta("USE `acore_characters`")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta("AND guildid = ? LIMIT ?")).
		WithArgs(5, int64(gmtScanCap+1)).
		WillReturnRows(sqlmock.NewRows(gmtScanCols()))

	reg := NewRegistry()
	RegisterWowGuildMembershipTargetsTool(reg, DBDeps{QueryDB: db})
	tool, _ := reg.Get("wow_guild_membership_targets")
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

// TestWowGuildMembershipTargets_SinceHoursWindow drives collect with a fixed
// `now` and asserts the sinceHours cutoff bind math (now - N*3600) + the echoed
// sinceCutoff.
func TestWowGuildMembershipTargets_SinceHoursWindow(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	now := time.Unix(1_700_000_000, 0)
	cutoff := int64(1_700_000_000 - 24*3600) // 1_699_913_600

	mock.ExpectExec(regexp.QuoteMeta("USE `acore_characters`")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta("AND TimeStamp >= ? LIMIT ?")).
		WithArgs(cutoff, int64(gmtScanCap+1)).
		WillReturnRows(sqlmock.NewRows(gmtScanCols()))

	out, err := collectWowGuildMembershipTargets(context.Background(), db, now, nil, "rankChurn", 24, 1, 25)
	if err != nil {
		t.Fatalf("collectWowGuildMembershipTargets: %v", err)
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

// TestCollectWowGuildMembershipTargets_Empty — empty log: targets must be a
// non-nil empty slice, distinctTargets 0, unresolved 0, and NO stage-2/3 queries
// fire.
func TestCollectWowGuildMembershipTargets_Empty(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	expectEmptyTargetScan(mock)

	out, err := collectWowGuildMembershipTargets(context.Background(), db, time.Unix(1_700_000_000, 0), nil, "rankChurn", 0, 1, 25)
	if err != nil {
		t.Fatalf("collectWowGuildMembershipTargets: %v", err)
	}
	sl, ok := out["targets"].([]*gmTarget)
	if !ok {
		t.Fatalf("targets type: %T want []*gmTarget", out["targets"])
	}
	if len(sl) != 0 {
		t.Errorf("targets len: %d want 0", len(sl))
	}
	if d, _ := out["distinctTargets"].(int); d != 0 {
		t.Errorf("distinctTargets: %v want 0", out["distinctTargets"])
	}
	if u, _ := out["unresolved"].(int); u != 0 {
		t.Errorf("unresolved: %v want 0", out["unresolved"])
	}
	// sinceHours 0 -> no sinceCutoff key.
	if _, ok := out["sinceCutoff"]; ok {
		t.Errorf("sinceCutoff should be absent when sinceHours=0, got %v", out["sinceCutoff"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestCollectWowGuildMembershipTargets_Golden drives the full three-stage fold:
// the officer-event-type split (invite/promote/demote/uninvite), the rankChurn
// computation + latestNewRank tracked from the most-recent promote/demote row
// (not invite/uninvite), the minEvents filter (drops a 1-event target), the
// rankChurn-desc + guid-asc ordering (with a tie), the characters batch
// resolution (incl. a deleted-character miss -> unresolved), the account batch
// resolution + RNDBOT bot flag, distinct guilds, recency, and honest realm
// totals.
func TestCollectWowGuildMembershipTargets_Golden(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	now := time.Unix(1_700_000_000, 0)

	mock.ExpectExec(regexp.QuoteMeta("USE `acore_characters`")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta("FROM `guild_eventlog` WHERE EventType IN (1,3,4,5) LIMIT ?")).
		WithArgs(int64(gmtScanCap + 1)).
		WillReturnRows(sqlmock.NewRows(gmtScanCols()).
			// Target 500 (bot): invite, promote(rank5), demote(rank6, LATER ts so
			// latestNewRank tracks the demote not the promote), uninvite. 4 events,
			// rankChurn=2, all in guild 1.
			AddRow(int64(1), gmtInvite, int64(500), 0, int64(1_699_980_000)).
			AddRow(int64(1), gmtPromote, int64(500), 5, int64(1_699_990_000)).
			AddRow(int64(1), gmtDemote, int64(500), 6, int64(1_699_995_000)).
			AddRow(int64(1), gmtUninvite, int64(500), 0, int64(1_699_998_000)).
			// Target 600 (player): 2 invites across TWO guilds (distinctGuilds 2),
			// no rank events -> hasRankEvent stays false.
			AddRow(int64(1), gmtInvite, int64(600), 0, int64(1_699_997_000)).
			AddRow(int64(2), gmtInvite, int64(600), 0, int64(1_699_999_000)).
			// Target 700 (deleted character -> no characters row): uninvite + invite.
			AddRow(int64(1), gmtUninvite, int64(700), 0, int64(1_699_999_500)).
			AddRow(int64(1), gmtInvite, int64(700), 0, int64(1_699_999_600)).
			// Target 800: a single invite -> 1 event, dropped by minEvents=2 and
			// never resolved.
			AddRow(int64(1), gmtInvite, int64(800), 0, int64(1_699_990_500)))

	// Stage 2: characters lookup over the ranked guids (500,600,700 after
	// minEvents=2 drops 800). 700 deliberately absent (deleted-character miss).
	mock.ExpectQuery(regexp.QuoteMeta("FROM `characters` WHERE guid IN (?,?,?)")).
		WithArgs(int64(500), int64(600), int64(700)).
		WillReturnRows(sqlmock.NewRows([]string{"guid", "name", "account"}).
			AddRow(int64(500), "Ahbotzug", int64(11)).
			AddRow(int64(600), "Slayo", int64(22)))

	// Stage 3: account lookup over the two resolved accounts. 11 is RNDBOT* (bot),
	// 22 is a human.
	mock.ExpectExec(regexp.QuoteMeta("USE `acore_auth`")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta("FROM `account` WHERE id IN (?,?)")).
		WithArgs(int64(11), int64(22)).
		WillReturnRows(sqlmock.NewRows([]string{"id", "username"}).
			AddRow(int64(11), "RNDBOT0007").
			AddRow(int64(22), "OPERATOR"))

	// minEvents=2 drops target 800 (1 event); topN=10 keeps the rest.
	out, err := collectWowGuildMembershipTargets(context.Background(), db, now, nil, "rankChurn", 0, 2, 10)
	if err != nil {
		t.Fatalf("collectWowGuildMembershipTargets: %v", err)
	}

	if s, _ := out["scanned"].(int); s != 9 {
		t.Errorf("scanned: %v want 9", out["scanned"])
	}
	if d, _ := out["distinctTargets"].(int); d != 4 {
		t.Errorf("distinctTargets: %v want 4 (500,600,700,800 pre-minEvents)", out["distinctTargets"])
	}
	if m, _ := out["matchedTargets"].(int); m != 3 {
		t.Errorf("matchedTargets: %v want 3 (minEvents=2 drops 800)", out["matchedTargets"])
	}
	if u, _ := out["unresolved"].(int); u != 1 {
		t.Errorf("unresolved: %v want 1 (target 700 deleted char)", out["unresolved"])
	}
	if ti, _ := out["totalInvited"].(int); ti != 5 {
		t.Errorf("totalInvited: %v want 5", out["totalInvited"])
	}
	if tp, _ := out["totalPromoted"].(int); tp != 1 {
		t.Errorf("totalPromoted: %v want 1", out["totalPromoted"])
	}
	if td, _ := out["totalDemoted"].(int); td != 1 {
		t.Errorf("totalDemoted: %v want 1", out["totalDemoted"])
	}
	if tu, _ := out["totalUninvited"].(int); tu != 2 {
		t.Errorf("totalUninvited: %v want 2", out["totalUninvited"])
	}

	targets, _ := out["targets"].([]*gmTarget)
	if len(targets) != 3 {
		t.Fatalf("targets len: %d want 3", len(targets))
	}

	// Order: 500 (rankChurn 2) > 600 (rankChurn 0) == 700 (rankChurn 0), guid-asc
	// tiebreak keeps 600 before 700.
	tg0 := targets[0]
	if tg0.PlayerGuid != 500 || tg0.TotalOfficerEvents != 4 {
		t.Errorf("targets[0]: %+v want guid=500 events=4", tg0)
	}
	if tg0.Invited != 1 || tg0.Promoted != 1 || tg0.Demoted != 1 || tg0.Uninvited != 1 {
		t.Errorf("targets[0] counts: inv=%d prom=%d dem=%d uninv=%d want 1/1/1/1",
			tg0.Invited, tg0.Promoted, tg0.Demoted, tg0.Uninvited)
	}
	if tg0.RankChurn != 2 {
		t.Errorf("targets[0] rankChurn: %d want 2", tg0.RankChurn)
	}
	if !tg0.HasRankEvent || tg0.LatestNewRank != 6 {
		t.Errorf("targets[0] rank: hasRankEvent=%v latestNewRank=%d want true/6 (demote is the LATER event, not the promote)",
			tg0.HasRankEvent, tg0.LatestNewRank)
	}
	if tg0.CharacterName != "Ahbotzug" || tg0.AccountID != 11 || !tg0.IsBot {
		t.Errorf("targets[0] identity: name=%q acct=%d isBot=%v want Ahbotzug/11/true", tg0.CharacterName, tg0.AccountID, tg0.IsBot)
	}
	if tg0.DistinctGuilds != 1 {
		t.Errorf("targets[0] distinctGuilds: %d want 1", tg0.DistinctGuilds)
	}
	// lastTS 1_699_998_000 -> now-2000s = 33m.
	if tg0.RecencyHuman != "33m" {
		t.Errorf("targets[0] recency: %q want 33m", tg0.RecencyHuman)
	}
	if tg0.FirstActivityISO != formatUnixISO(1_699_980_000) || tg0.LastActivityISO != formatUnixISO(1_699_998_000) {
		t.Errorf("targets[0] activity: first=%q last=%q", tg0.FirstActivityISO, tg0.LastActivityISO)
	}

	// Target 600: two invites across two guilds, no rank events.
	tg1 := targets[1]
	if tg1.PlayerGuid != 600 || tg1.TotalOfficerEvents != 2 || tg1.Invited != 2 {
		t.Errorf("targets[1]: %+v want guid=600 events=2 invited=2", tg1)
	}
	if tg1.HasRankEvent {
		t.Errorf("targets[1] hasRankEvent: %v want false (never promoted/demoted)", tg1.HasRankEvent)
	}
	if tg1.DistinctGuilds != 2 {
		t.Errorf("targets[1] distinctGuilds: %d want 2", tg1.DistinctGuilds)
	}
	if tg1.CharacterName != "Slayo" || tg1.AccountID != 22 || tg1.IsBot {
		t.Errorf("targets[1] identity: name=%q acct=%d isBot=%v want Slayo/22/false", tg1.CharacterName, tg1.AccountID, tg1.IsBot)
	}

	// Target 700: deleted character -> empty name, account 0, not a bot.
	tg2 := targets[2]
	if tg2.PlayerGuid != 700 || tg2.CharacterName != "" || tg2.AccountID != 0 || tg2.IsBot {
		t.Errorf("targets[2] (deleted char): %+v want guid=700 empty-name acct0 not-bot", tg2)
	}
	if tg2.Uninvited != 1 || tg2.Invited != 1 {
		t.Errorf("targets[2] counts: uninv=%d inv=%d want 1/1", tg2.Uninvited, tg2.Invited)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestTopNGuildMembershipTargets_SortSlice exercises the pure helper: the
// minEvents filter, each sort key (rankChurn/uninvited/invited/events, all desc)
// with guid-asc tiebreaker, slice to n, and the pre-slice matched count. Guards
// against Go map-iteration flake on ties.
func TestTopNGuildMembershipTargets_SortSlice(t *testing.T) {
	// Base fixture rebuilt per sub-test so a prior sort doesn't leak ordering.
	build := func() map[int64]*gmTarget {
		return map[int64]*gmTarget{
			30: {PlayerGuid: 30, TotalOfficerEvents: 5, Promoted: 3, Demoted: 2, Uninvited: 1, Invited: 1},
			10: {PlayerGuid: 10, TotalOfficerEvents: 5, Promoted: 3, Demoted: 2, Uninvited: 9, Invited: 2}, // ties rankChurn+promoted with 30
			20: {PlayerGuid: 20, TotalOfficerEvents: 9, Promoted: 1, Demoted: 0, Uninvited: 0, Invited: 8},
			40: {PlayerGuid: 40, TotalOfficerEvents: 1, Promoted: 0, Demoted: 0, Uninvited: 0, Invited: 1}, // below minEvents=2
		}
	}

	t.Run("rankChurn desc + guid-asc tiebreak + minEvents filter + slice", func(t *testing.T) {
		out, matched := topNGuildMembershipTargets(build(), "rankChurn", 2, 2)
		if matched != 3 {
			t.Errorf("matched: %d want 3 (40 filtered by minEvents=2)", matched)
		}
		if len(out) != 2 {
			t.Fatalf("len: %d want 2 (sliced to n)", len(out))
		}
		if out[0].PlayerGuid != 10 {
			t.Errorf("out[0]: %+v want guid=10 (rankChurn 5, guid-asc tiebreak over 30)", out[0])
		}
		if out[1].PlayerGuid != 30 {
			t.Errorf("out[1]: %+v want guid=30", out[1])
		}
	})

	t.Run("uninvited desc", func(t *testing.T) {
		out, matched := topNGuildMembershipTargets(build(), "uninvited", 1, 4)
		if matched != 4 {
			t.Errorf("matched: %d want 4 (minEvents=1)", matched)
		}
		if out[0].PlayerGuid != 10 || out[1].PlayerGuid != 30 {
			t.Errorf("uninvited order: [%d,%d,...] want [10,30,...]", out[0].PlayerGuid, out[1].PlayerGuid)
		}
	})

	t.Run("promoted desc", func(t *testing.T) {
		out, _ := topNGuildMembershipTargets(build(), "promoted", 1, 4)
		if out[0].PlayerGuid != 10 || out[1].PlayerGuid != 30 {
			t.Errorf("promoted order: [%d,%d,...] want [10,30,...] (tie -> guid-asc)", out[0].PlayerGuid, out[1].PlayerGuid)
		}
	})

	t.Run("invited desc", func(t *testing.T) {
		out, _ := topNGuildMembershipTargets(build(), "invited", 1, 4)
		if out[0].PlayerGuid != 20 || out[1].PlayerGuid != 10 {
			t.Errorf("invited order: [%d,%d,...] want [20,10,...]", out[0].PlayerGuid, out[1].PlayerGuid)
		}
	})

	t.Run("events desc", func(t *testing.T) {
		out, _ := topNGuildMembershipTargets(build(), "events", 1, 4)
		if out[0].PlayerGuid != 20 || out[1].PlayerGuid != 10 {
			t.Errorf("events order: [%d,%d,...] want [20,10,...] (tie -> guid-asc)", out[0].PlayerGuid, out[1].PlayerGuid)
		}
	})
}

// TestTopNGuildMembershipTargets_EmptyNonNil — an all-filtered map still returns a
// non-nil slice (callers JSON-encode it as []).
func TestTopNGuildMembershipTargets_EmptyNonNil(t *testing.T) {
	m := map[int64]*gmTarget{1: {PlayerGuid: 1, TotalOfficerEvents: 1}}
	out, matched := topNGuildMembershipTargets(m, "rankChurn", 5, 10)
	if out == nil {
		t.Fatal("out is nil, want non-nil empty slice")
	}
	if len(out) != 0 || matched != 0 {
		t.Errorf("len=%d matched=%d want 0/0", len(out), matched)
	}
}

// TestAnnotateTargetAccounts_DedupesAccountIds — two target-characters on the
// same account bind the id ONCE in the IN list, and both rows get the username +
// bot flag. Targets with account 0 (unresolved character) are skipped entirely.
func TestAnnotateTargetAccounts_DedupesAccountIds(t *testing.T) {
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

	targets := []*gmTarget{
		{PlayerGuid: 500, AccountID: 11},
		{PlayerGuid: 501, AccountID: 11}, // same account -> deduped in IN list
		{PlayerGuid: 502, AccountID: 0},  // unresolved char -> skipped
	}
	mock.ExpectQuery(regexp.QuoteMeta("FROM `account` WHERE id IN (?)")).
		WithArgs(int64(11)).
		WillReturnRows(sqlmock.NewRows([]string{"id", "username"}).
			AddRow(int64(11), "RNDBOTAAA"))

	if err := annotateTargetAccounts(context.Background(), conn, targets); err != nil {
		t.Fatalf("annotateTargetAccounts: %v", err)
	}
	if !targets[0].IsBot || targets[0].AccountUsername != "RNDBOTAAA" {
		t.Errorf("targets[0]: %+v want RNDBOTAAA/bot", targets[0])
	}
	if !targets[1].IsBot || targets[1].AccountUsername != "RNDBOTAAA" {
		t.Errorf("targets[1] (same account): %+v want RNDBOTAAA/bot", targets[1])
	}
	if targets[2].AccountUsername != "" || targets[2].IsBot {
		t.Errorf("targets[2] (acct 0): %+v want empty/not-bot", targets[2])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestAnnotateTargetAccounts_NoResolvedAccounts — when every target has account 0
// (all characters unresolved), NO account query fires.
func TestAnnotateTargetAccounts_NoResolvedAccounts(t *testing.T) {
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

	targets := []*gmTarget{{PlayerGuid: 500, AccountID: 0}}
	if err := annotateTargetAccounts(context.Background(), conn, targets); err != nil {
		t.Fatalf("annotateTargetAccounts: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations (account query should not fire): %v", err)
	}
}

// TestGmTargetFold_Sums — the officer-event-type split, the rankChurn/rank-event
// tracking (only promote/demote rows set latestNewRank, and only the LATER of the
// two by TimeStamp wins), first/last timestamp tracking, distinct-guild count,
// and the finalize-time recency.
func TestGmTargetFold_Sums(t *testing.T) {
	tg := &gmTarget{PlayerGuid: 7}
	tg.fold(gmtInvite, 1, 0, 100)   // invited, guild 1, earliest ts
	tg.fold(gmtPromote, 1, 5, 200)  // promoted, rank 5
	tg.fold(gmtDemote, 1, 6, 250)   // demoted, rank 6 — LATER than the promote, so this wins
	tg.fold(gmtUninvite, 2, 0, 300) // uninvited, NEW guild 2, latest ts

	if tg.TotalOfficerEvents != 4 {
		t.Errorf("totalOfficerEvents: %d want 4", tg.TotalOfficerEvents)
	}
	if tg.Invited != 1 || tg.Promoted != 1 || tg.Demoted != 1 || tg.Uninvited != 1 {
		t.Errorf("counts: inv=%d prom=%d dem=%d uninv=%d want 1/1/1/1", tg.Invited, tg.Promoted, tg.Demoted, tg.Uninvited)
	}
	if !tg.HasRankEvent || tg.LatestNewRank != 6 {
		t.Errorf("rank: hasRankEvent=%v latestNewRank=%d want true/6", tg.HasRankEvent, tg.LatestNewRank)
	}
	if tg.firstTS != 100 || tg.lastTS != 300 {
		t.Errorf("ts: first=%d last=%d want 100/300", tg.firstTS, tg.lastTS)
	}
	if len(tg.guilds) != 2 {
		t.Errorf("distinct guilds: %d want 2", len(tg.guilds))
	}

	tg.finalize(time.Unix(1000, 0))
	if tg.RankChurn != 2 {
		t.Errorf("rankChurn: %d want 2", tg.RankChurn)
	}
	if tg.DistinctGuilds != 2 {
		t.Errorf("distinctGuilds: %d want 2", tg.DistinctGuilds)
	}
	if tg.RecencyHuman != "11m" { // now 1000 - lastTS 300 = 700s -> 11m
		t.Errorf("recency: %q want 11m", tg.RecencyHuman)
	}
}
