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

// gmActorScanCols is the guild_eventlog column set the tool scans, in order.
func gmActorScanCols() []string {
	return []string{"guildid", "EventType", "PlayerGuid1", "TimeStamp"}
}

// expectEmptyMembershipScan queues the USE + guild_eventlog scan with no rows.
// Used by the arg-clamp tests where the fold result doesn't matter (empty -> no
// stage-2/3 queries fire, so only these two expectations are needed).
func expectEmptyMembershipScan(mock sqlmock.Sqlmock) {
	mock.ExpectExec(regexp.QuoteMeta("USE `acore_characters`")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta("FROM `guild_eventlog`")).
		WillReturnRows(sqlmock.NewRows(gmActorScanCols()))
}

// TestWowGuildMembershipActors_DescriptionMentionsContext guards the description
// keywords — they shape which tool the agent reaches for when an operator asks
// "who is driving guild churn?" / "is a bot mass-inviting?". Drop the actor/officer/
// bot framing and the agent falls back to wow_guild_membership_churn (guild grain,
// no actor) or a hand-written eventlog join.
func TestWowGuildMembershipActors_DescriptionMentionsContext(t *testing.T) {
	reg := NewRegistry()
	RegisterWowGuildMembershipActorsTool(reg, DBDeps{})
	tool, ok := reg.Get("wow_guild_membership_actors")
	if !ok {
		t.Fatal("wow_guild_membership_actors not registered")
	}
	for _, kw := range []string{
		"guild_eventlog", "acore_characters", "acore_auth.account", "ops_ro",
		"PlayerGuid1", "RNDBOT", "isBot", "wow_guild_membership_churn", "wow_bot_lookup",
		"wow_guild_bank_top_actors", "Three-stage", "topN", "sortBy", "guildId",
		"sinceHours", "minEvents", "unresolved", "officerEvents", "selfEvents",
		"invites", "promotes", "demotes", "uninvites", "joins", "leaves", "recency",
		"Read-only",
	} {
		if !strings.Contains(tool.Description, kw) {
			t.Errorf("description missing %q", kw)
		}
	}
}

// TestWowGuildMembershipActors_ReadOnlyAnnotation pins readOnlyHint=true so a future
// copy-paste of a destructive sibling can't silently flip the gate.
func TestWowGuildMembershipActors_ReadOnlyAnnotation(t *testing.T) {
	reg := NewRegistry()
	RegisterWowGuildMembershipActorsTool(reg, DBDeps{})
	tool, _ := reg.Get("wow_guild_membership_actors")
	if !strings.Contains(string(tool.Annotations), `"readOnlyHint":true`) {
		t.Errorf("annotations missing readOnlyHint=true: %s", tool.Annotations)
	}
}

func TestWowGuildMembershipActors_NoPoolReturnsError(t *testing.T) {
	reg := NewRegistry()
	RegisterWowGuildMembershipActorsTool(reg, DBDeps{}) // QueryDB nil
	tool, _ := reg.Get("wow_guild_membership_actors")
	resp := tool.Handler(context.Background(), json.RawMessage(`{}`), "")
	got, _ := resp.(map[string]any)
	if msg, _ := got["error"].(string); !strings.Contains(msg, "ops_ro pool not configured") {
		t.Errorf("expected pool-not-configured error, got %v", got)
	}
}

// TestWowGuildMembershipActors_TopNClamps drives the handler and asserts the
// post-clamp topN echo (unset->default, zero->default, oversized->cap, in-range
// passthrough) and that the LIMIT bind is always scanCap+1 regardless of args.
func TestWowGuildMembershipActors_TopNClamps(t *testing.T) {
	cases := []struct {
		name     string
		args     string
		wantTopN int
	}{
		{"unset uses default", `{}`, gmActorsDefaultTopN},
		{"zero falls back to default", `{"topN":0}`, gmActorsDefaultTopN},
		{"negative falls back to default", `{"topN":-4}`, gmActorsDefaultTopN},
		{"oversized clamps to max", `{"topN":99999}`, gmActorsMaxTopN},
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
			mock.ExpectQuery(regexp.QuoteMeta("FROM `guild_eventlog` LIMIT ?")).
				WithArgs(int64(gmActorsScanCap + 1)).
				WillReturnRows(sqlmock.NewRows(gmActorScanCols()))

			reg := NewRegistry()
			RegisterWowGuildMembershipActorsTool(reg, DBDeps{QueryDB: db})
			tool, _ := reg.Get("wow_guild_membership_actors")
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

// TestWowGuildMembershipActors_MinEventsClamps verifies the minEvents floor of 1
// (unset/zero/negative all clamp to 1) is echoed in the response.
func TestWowGuildMembershipActors_MinEventsClamps(t *testing.T) {
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
			expectEmptyMembershipScan(mock)

			reg := NewRegistry()
			RegisterWowGuildMembershipActorsTool(reg, DBDeps{QueryDB: db})
			tool, _ := reg.Get("wow_guild_membership_actors")
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

// TestWowGuildMembershipActors_InvalidSortByRejected — an unknown sortBy is a clean
// argument error and NO query fires (validated before the pool is touched). Covers
// a case-variant ('OfficerEvents') and an injection string.
func TestWowGuildMembershipActors_InvalidSortByRejected(t *testing.T) {
	for _, bad := range []string{"bogus", "OfficerEvents", "1; DROP TABLE guild_eventlog"} {
		t.Run(bad, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()

			reg := NewRegistry()
			RegisterWowGuildMembershipActorsTool(reg, DBDeps{QueryDB: db})
			tool, _ := reg.Get("wow_guild_membership_actors")
			resp := tool.Handler(context.Background(), json.RawMessage(`{"sortBy":"`+bad+`"}`), "")
			got, _ := resp.(map[string]any)
			if msg, _ := got["error"].(string); !strings.Contains(msg, "sortBy must be one of") {
				t.Errorf("expected sortBy error, got %v", got)
			}
			// No ExpectQuery queued -> ExpectationsWereMet passes only if none fired.
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Errorf("unmet mock expectations (no query should fire): %v", err)
			}
		})
	}
}

// TestWowGuildMembershipActors_NegativeGuildIdRejected — a non-positive guildId is a
// clean argument error and NO query fires.
func TestWowGuildMembershipActors_NegativeGuildIdRejected(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	reg := NewRegistry()
	RegisterWowGuildMembershipActorsTool(reg, DBDeps{QueryDB: db})
	tool, _ := reg.Get("wow_guild_membership_actors")
	resp := tool.Handler(context.Background(), json.RawMessage(`{"guildId":-1}`), "")
	got, _ := resp.(map[string]any)
	if msg, _ := got["error"].(string); !strings.Contains(msg, "guildId must be a positive integer") {
		t.Errorf("expected guildId error, got %v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations (no query should fire): %v", err)
	}
}

// TestWowGuildMembershipActors_GuildIdFilter verifies the optional guildId binds a
// `WHERE guildid = ?` (before the LIMIT) and is echoed.
func TestWowGuildMembershipActors_GuildIdFilter(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.ExpectExec(regexp.QuoteMeta("USE `acore_characters`")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta("WHERE guildid = ? LIMIT ?")).
		WithArgs(5, int64(gmActorsScanCap+1)).
		WillReturnRows(sqlmock.NewRows(gmActorScanCols()))

	reg := NewRegistry()
	RegisterWowGuildMembershipActorsTool(reg, DBDeps{QueryDB: db})
	tool, _ := reg.Get("wow_guild_membership_actors")
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

// TestWowGuildMembershipActors_SinceHoursWindow drives collect with a fixed `now`
// and asserts the sinceHours cutoff bind math (now - N*3600, an int64 second count
// for the uint32 TimeStamp column) + the echoed sinceCutoff.
func TestWowGuildMembershipActors_SinceHoursWindow(t *testing.T) {
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
		WithArgs(cutoff, int64(gmActorsScanCap+1)).
		WillReturnRows(sqlmock.NewRows(gmActorScanCols()))

	out, err := collectWowGuildMembershipActors(context.Background(), db, now, nil, "officerEvents", 24, 1, 25)
	if err != nil {
		t.Fatalf("collectWowGuildMembershipActors: %v", err)
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

// TestCollectWowGuildMembershipActors_Empty — empty log: actors must be a non-nil
// empty slice, distinctActors 0, unresolved 0, and NO stage-2/3 queries fire.
func TestCollectWowGuildMembershipActors_Empty(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	expectEmptyMembershipScan(mock)

	out, err := collectWowGuildMembershipActors(context.Background(), db, time.Unix(1_700_000_000, 0), nil, "officerEvents", 0, 1, 25)
	if err != nil {
		t.Fatalf("collectWowGuildMembershipActors: %v", err)
	}
	sl, ok := out["actors"].([]*gmActor)
	if !ok {
		t.Fatalf("actors type: %T want []*gmActor", out["actors"])
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
	if oe, _ := out["totalOfficerEvents"].(int); oe != 0 {
		t.Errorf("totalOfficerEvents: %v want 0", out["totalOfficerEvents"])
	}
	// sinceHours 0 -> no sinceCutoff key.
	if _, ok := out["sinceCutoff"]; ok {
		t.Errorf("sinceCutoff should be absent when sinceHours=0, got %v", out["sinceCutoff"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestCollectWowGuildMembershipActors_Golden drives the full three-stage fold: the
// officer-vs-self event split (invite/promote/demote/uninvite vs join/leave), the
// minEvents filter (drops a 1-event actor), officerEvents-desc + guid-asc ordering,
// the characters batch resolution (incl. a deleted-character miss -> unresolved),
// the account batch resolution + RNDBOT bot flag, distinct guilds, recency, and the
// honest realm totals.
func TestCollectWowGuildMembershipActors_Golden(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	now := time.Unix(1_700_000_000, 0)

	mock.ExpectExec(regexp.QuoteMeta("USE `acore_characters`")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta("FROM `guild_eventlog` LIMIT ?")).
		WithArgs(int64(gmActorsScanCap + 1)).
		WillReturnRows(sqlmock.NewRows(gmActorScanCols()).
			// Actor 100 (RNDBOT mass-inviter): 3 invites + 1 promote (officerEvents 4)
			// + 1 self join (selfEvents 1) = 5 events, all in guild 1. last ts drives
			// recency; the earliest invite is firstActivity.
			AddRow(int64(1), gmaInvitePlayer, int64(100), int64(1_699_980_000)).
			AddRow(int64(1), gmaInvitePlayer, int64(100), int64(1_699_981_000)).
			AddRow(int64(1), gmaInvitePlayer, int64(100), int64(1_699_982_000)).
			AddRow(int64(1), gmaPromotePlayer, int64(100), int64(1_699_990_000)).
			AddRow(int64(1), gmaJoinGuild, int64(100), int64(1_699_999_000)).
			// Actor 200 (human officer across TWO guilds): 2 uninvites (guilds 1+2) +
			// 1 demote = officerEvents 3, selfEvents 0, distinctGuilds 2.
			AddRow(int64(1), gmaUninvitePlayer, int64(200), int64(1_699_997_000)).
			AddRow(int64(2), gmaUninvitePlayer, int64(200), int64(1_699_998_000)).
			AddRow(int64(1), gmaDemotePlayer, int64(200), int64(1_699_996_000)).
			// Actor 300 (deleted character -> no characters row): 1 join + 1 leave =
			// selfEvents 2, officerEvents 0.
			AddRow(int64(1), gmaJoinGuild, int64(300), int64(1_699_999_500)).
			AddRow(int64(1), gmaLeaveGuild, int64(300), int64(1_699_999_600)).
			// Actor 400: a single invite -> 1 event, dropped by minEvents=2, never
			// resolved.
			AddRow(int64(1), gmaInvitePlayer, int64(400), int64(1_699_990_500)))

	// Stage 2: characters lookup over the ranked guids (100,200,300 after minEvents=2
	// drops 400). 300 deliberately absent (deleted-character miss).
	mock.ExpectQuery(regexp.QuoteMeta("FROM `characters` WHERE guid IN (?,?,?)")).
		WithArgs(int64(100), int64(200), int64(300)).
		WillReturnRows(sqlmock.NewRows([]string{"guid", "name", "account"}).
			AddRow(int64(100), "Invitezug", int64(11)).
			AddRow(int64(200), "Officer", int64(22)))

	// Stage 3: account lookup over the two resolved accounts. 11 is RNDBOT* (bot),
	// 22 is a human.
	mock.ExpectExec(regexp.QuoteMeta("USE `acore_auth`")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta("FROM `account` WHERE id IN (?,?)")).
		WithArgs(int64(11), int64(22)).
		WillReturnRows(sqlmock.NewRows([]string{"id", "username"}).
			AddRow(int64(11), "RNDBOT0007").
			AddRow(int64(22), "OPERATOR"))

	// minEvents=2 drops actor 400 (1 event); topN=10 keeps the rest. Default sort
	// officerEvents desc: 100 (4) > 200 (3) > 300 (0).
	out, err := collectWowGuildMembershipActors(context.Background(), db, now, nil, "officerEvents", 0, 2, 10)
	if err != nil {
		t.Fatalf("collectWowGuildMembershipActors: %v", err)
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
	// officer events across ALL scanned rows: 100(3 inv+1 promo=4) + 200(2 uninv+1
	// demo=3) + 400(1 inv) = 8. self: 100(1 join) + 300(1 join+1 leave=2) = 3.
	if oe, _ := out["totalOfficerEvents"].(int); oe != 8 {
		t.Errorf("totalOfficerEvents: %v want 8", out["totalOfficerEvents"])
	}
	if se, _ := out["totalSelfEvents"].(int); se != 3 {
		t.Errorf("totalSelfEvents: %v want 3", out["totalSelfEvents"])
	}
	if te, _ := out["totalEvents"].(int); te != 11 {
		t.Errorf("totalEvents: %v want 11", out["totalEvents"])
	}

	actors, _ := out["actors"].([]*gmActor)
	if len(actors) != 3 {
		t.Fatalf("actors len: %d want 3", len(actors))
	}

	// Order: 100 (officerEvents 4) > 200 (3) > 300 (0).
	a0 := actors[0]
	if a0.PlayerGuid != 100 || a0.TotalEvents != 5 {
		t.Errorf("actors[0]: %+v want guid=100 events=5", a0)
	}
	if a0.Invites != 3 || a0.Promotes != 1 || a0.Demotes != 0 || a0.Uninvites != 0 {
		t.Errorf("actors[0] officer split: inv=%d promo=%d demo=%d uninv=%d want 3/1/0/0",
			a0.Invites, a0.Promotes, a0.Demotes, a0.Uninvites)
	}
	if a0.OfficerEvents != 4 || a0.SelfEvents != 1 || a0.Joins != 1 || a0.Leaves != 0 {
		t.Errorf("actors[0] rollup: officer=%d self=%d joins=%d leaves=%d want 4/1/1/0",
			a0.OfficerEvents, a0.SelfEvents, a0.Joins, a0.Leaves)
	}
	if a0.CharacterName != "Invitezug" || a0.AccountID != 11 || !a0.IsBot {
		t.Errorf("actors[0] identity: name=%q acct=%d isBot=%v want Invitezug/11/true", a0.CharacterName, a0.AccountID, a0.IsBot)
	}
	if a0.DistinctGuilds != 1 {
		t.Errorf("actors[0] distinctGuilds: %d want 1", a0.DistinctGuilds)
	}
	// lastTS 1_699_999_000 -> now-1000s = 16m; first activity is the earliest invite.
	if a0.RecencyHuman != "16m" {
		t.Errorf("actors[0] recency: %q want 16m", a0.RecencyHuman)
	}
	if a0.FirstActivityISO != formatUnixISO(1_699_980_000) || a0.LastActivityISO != formatUnixISO(1_699_999_000) {
		t.Errorf("actors[0] activity: first=%q last=%q", a0.FirstActivityISO, a0.LastActivityISO)
	}

	// Actor 200: officer across two distinct guilds, no self events.
	a1 := actors[1]
	if a1.PlayerGuid != 200 || a1.OfficerEvents != 3 || a1.Uninvites != 2 || a1.Demotes != 1 {
		t.Errorf("actors[1]: %+v want guid=200 officer=3 uninv=2 demo=1", a1)
	}
	if a1.SelfEvents != 0 || a1.DistinctGuilds != 2 {
		t.Errorf("actors[1]: self=%d distinctGuilds=%d want 0/2", a1.SelfEvents, a1.DistinctGuilds)
	}
	if a1.CharacterName != "Officer" || a1.AccountID != 22 || a1.IsBot {
		t.Errorf("actors[1] identity: name=%q acct=%d isBot=%v want Officer/22/false", a1.CharacterName, a1.AccountID, a1.IsBot)
	}

	// Actor 300: deleted character -> empty name, account 0, not a bot; self churner.
	a2 := actors[2]
	if a2.PlayerGuid != 300 || a2.CharacterName != "" || a2.AccountID != 0 || a2.IsBot {
		t.Errorf("actors[2] (deleted char): %+v want guid=300 empty-name acct0 not-bot", a2)
	}
	if a2.Joins != 1 || a2.Leaves != 1 || a2.SelfEvents != 2 || a2.OfficerEvents != 0 {
		t.Errorf("actors[2] rollup: joins=%d leaves=%d self=%d officer=%d want 1/1/2/0",
			a2.Joins, a2.Leaves, a2.SelfEvents, a2.OfficerEvents)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestTopNGuildMembershipActors_SortSlice exercises the pure helper: the minEvents
// filter and each sort key (officerEvents / invites / promotes / uninvites / events,
// all desc) with guid-asc tiebreaker, slice to n, and the pre-slice matched count.
// Each key yields a DISTINCT ordering so the comparator is proven to read the right
// field. Guards against Go map-iteration flake on ties.
func TestTopNGuildMembershipActors_SortSlice(t *testing.T) {
	// Base fixture rebuilt per sub-test so a prior sort doesn't leak ordering.
	build := func() map[int64]*gmActor {
		return map[int64]*gmActor{
			30: {PlayerGuid: 30, OfficerEvents: 5, Invites: 1, Promotes: 4, Uninvites: 0, TotalEvents: 6},
			10: {PlayerGuid: 10, OfficerEvents: 5, Invites: 5, Promotes: 0, Uninvites: 5, TotalEvents: 10}, // ties officerEvents with 30
			20: {PlayerGuid: 20, OfficerEvents: 9, Invites: 2, Promotes: 2, Uninvites: 1, TotalEvents: 9},
			40: {PlayerGuid: 40, OfficerEvents: 1, Invites: 0, Promotes: 9, Uninvites: 9, TotalEvents: 1}, // below minEvents=2 by TotalEvents
		}
	}

	t.Run("officerEvents desc + guid-asc tiebreak + minEvents filter + slice", func(t *testing.T) {
		out, matched := topNGuildMembershipActors(build(), "officerEvents", 2, 2)
		if matched != 3 {
			t.Errorf("matched: %d want 3 (40 filtered by minEvents=2)", matched)
		}
		if len(out) != 2 {
			t.Fatalf("len: %d want 2 (sliced to n)", len(out))
		}
		if out[0].PlayerGuid != 20 {
			t.Errorf("out[0]: %+v want guid=20 (officerEvents 9)", out[0])
		}
		if out[1].PlayerGuid != 10 {
			t.Errorf("out[1]: %+v want guid=10 (officerEvents 5, guid-asc tiebreak over 30)", out[1])
		}
	})

	t.Run("invites desc", func(t *testing.T) {
		out, _ := topNGuildMembershipActors(build(), "invites", 1, 4)
		if out[0].PlayerGuid != 10 || out[1].PlayerGuid != 20 {
			t.Errorf("invites order: [%d,%d,...] want [10,20,...]", out[0].PlayerGuid, out[1].PlayerGuid)
		}
	})

	t.Run("promotes desc", func(t *testing.T) {
		out, _ := topNGuildMembershipActors(build(), "promotes", 1, 4)
		if out[0].PlayerGuid != 40 || out[1].PlayerGuid != 30 {
			t.Errorf("promotes order: [%d,%d,...] want [40,30,...]", out[0].PlayerGuid, out[1].PlayerGuid)
		}
	})

	t.Run("uninvites desc", func(t *testing.T) {
		out, _ := topNGuildMembershipActors(build(), "uninvites", 1, 4)
		if out[0].PlayerGuid != 40 || out[1].PlayerGuid != 10 {
			t.Errorf("uninvites order: [%d,%d,...] want [40,10,...]", out[0].PlayerGuid, out[1].PlayerGuid)
		}
	})

	t.Run("events desc", func(t *testing.T) {
		out, _ := topNGuildMembershipActors(build(), "events", 1, 4)
		if out[0].PlayerGuid != 10 || out[1].PlayerGuid != 20 {
			t.Errorf("events order: [%d,%d,...] want [10,20,...]", out[0].PlayerGuid, out[1].PlayerGuid)
		}
	})
}

// TestTopNGuildMembershipActors_EmptyNonNil — an all-filtered map still returns a
// non-nil slice (callers JSON-encode it as []).
func TestTopNGuildMembershipActors_EmptyNonNil(t *testing.T) {
	m := map[int64]*gmActor{1: {PlayerGuid: 1, TotalEvents: 1}}
	out, matched := topNGuildMembershipActors(m, "officerEvents", 5, 10)
	if out == nil {
		t.Fatal("out is nil, want non-nil empty slice")
	}
	if len(out) != 0 || matched != 0 {
		t.Errorf("len=%d matched=%d want 0/0", len(out), matched)
	}
}

// TestAnnotateMembershipActorAccounts_DedupesAccountIds — two actor-characters on
// the same account bind the id ONCE in the IN list, and both rows get the username +
// bot flag. Actors with account 0 (unresolved character) are skipped entirely.
func TestAnnotateMembershipActorAccounts_DedupesAccountIds(t *testing.T) {
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

	actors := []*gmActor{
		{PlayerGuid: 100, AccountID: 11},
		{PlayerGuid: 101, AccountID: 11}, // same account -> deduped in IN list
		{PlayerGuid: 102, AccountID: 0},  // unresolved char -> skipped
	}
	mock.ExpectQuery(regexp.QuoteMeta("FROM `account` WHERE id IN (?)")).
		WithArgs(int64(11)).
		WillReturnRows(sqlmock.NewRows([]string{"id", "username"}).
			AddRow(int64(11), "RNDBOTAAA"))

	if err := annotateMembershipActorAccounts(context.Background(), conn, actors); err != nil {
		t.Fatalf("annotateMembershipActorAccounts: %v", err)
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

// TestAnnotateMembershipActorAccounts_NoResolvedAccounts — when every actor has
// account 0 (all characters unresolved), NO account query fires.
func TestAnnotateMembershipActorAccounts_NoResolvedAccounts(t *testing.T) {
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

	actors := []*gmActor{{PlayerGuid: 100, AccountID: 0}}
	if err := annotateMembershipActorAccounts(context.Background(), conn, actors); err != nil {
		t.Fatalf("annotateMembershipActorAccounts: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations (account query should not fire): %v", err)
	}
}

// TestGmActorFold_Sums — the officer-vs-self split, per-type counts, first/last
// timestamp tracking, distinct-guild count, and the finalize-time recency.
func TestGmActorFold_Sums(t *testing.T) {
	a := &gmActor{PlayerGuid: 7}
	a.fold(gmaInvitePlayer, 1, 100)   // invite (officer), new guild 1, earliest ts
	a.fold(gmaPromotePlayer, 1, 200)  // promote (officer)
	a.fold(gmaUninvitePlayer, 2, 150) // uninvite (officer), new guild 2
	a.fold(gmaJoinGuild, 1, 50)       // join (self), earliest ts
	a.fold(gmaLeaveGuild, 2, 300)     // leave (self), latest ts

	if a.TotalEvents != 5 {
		t.Errorf("totalEvents: %d want 5", a.TotalEvents)
	}
	if a.Invites != 1 || a.Promotes != 1 || a.Uninvites != 1 || a.Demotes != 0 {
		t.Errorf("officer split: inv=%d promo=%d uninv=%d demo=%d want 1/1/1/0", a.Invites, a.Promotes, a.Uninvites, a.Demotes)
	}
	if a.Joins != 1 || a.Leaves != 1 {
		t.Errorf("self split: joins=%d leaves=%d want 1/1", a.Joins, a.Leaves)
	}
	if a.OfficerEvents != 3 || a.SelfEvents != 2 {
		t.Errorf("rollup: officer=%d self=%d want 3/2", a.OfficerEvents, a.SelfEvents)
	}
	if a.firstTS != 50 || a.lastTS != 300 {
		t.Errorf("ts: first=%d last=%d want 50/300", a.firstTS, a.lastTS)
	}
	if len(a.guilds) != 2 {
		t.Errorf("distinct guilds: %d want 2", len(a.guilds))
	}

	a.finalize(time.Unix(1000, 0))
	if a.DistinctGuilds != 2 {
		t.Errorf("distinctGuilds: %d want 2", a.DistinctGuilds)
	}
	if a.RecencyHuman != "11m" { // now 1000 - lastTS 300 = 700s -> 11m
		t.Errorf("recency: %q want 11m", a.RecencyHuman)
	}
}
