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

// guildRosterListCols is the column set wow_guild_roster's list query scans, in order.
func guildRosterListCols() []string {
	return []string{"guid", "name", "race", "class", "gender", "level", "online", "logout_time", "zone", "account", "rank", "rname", "pnote", "offnote"}
}

// TestWowGuildRoster_DescriptionMentionsContext guards the description keywords — they
// shape which tool the agent reaches for when an operator asks "who is in guild X /
// which members are dormant?". Drop the guild-table framing or the sibling
// cross-references and the agent falls back to a hand-written db_query.
func TestWowGuildRoster_DescriptionMentionsContext(t *testing.T) {
	reg := NewRegistry()
	RegisterWowGuildRosterTool(reg, DBDeps{})
	tool, ok := reg.Get("wow_guild_roster")
	if !ok {
		t.Fatal("wow_guild_roster not registered")
	}
	for _, kw := range []string{
		"acore_characters", "guild_member", "guild_rank", "ops_ro", "wow_player_lookup",
		"guildId", "guildName", "found:false", "raceName", "className", "rankName",
		"rank-<id>", "isGuildMaster", "leaderguid", "logout_time", "logoutTimeISO",
		"inactiveDays", "inactiveHuman", "zoneId", "accountId", "publicNote", "officerNote",
		"Guild Master", "totalMembers", "onlineMembers", "matchedMembers", "minInactiveDays",
		"wow_mail_unread", "topN", "Read-only",
	} {
		if !strings.Contains(tool.Description, kw) {
			t.Errorf("description missing %q: %s", kw, tool.Description)
		}
	}
}

// TestWowGuildRoster_ReadOnlyAnnotation pins readOnlyHint=true so a future copy-paste of
// a destructive sibling can't silently flip the gate.
func TestWowGuildRoster_ReadOnlyAnnotation(t *testing.T) {
	reg := NewRegistry()
	RegisterWowGuildRosterTool(reg, DBDeps{})
	tool, _ := reg.Get("wow_guild_roster")
	if !strings.Contains(string(tool.Annotations), `"readOnlyHint":true`) {
		t.Errorf("annotations missing readOnlyHint=true: %s", tool.Annotations)
	}
}

func TestWowGuildRoster_NoPoolReturnsError(t *testing.T) {
	reg := NewRegistry()
	RegisterWowGuildRosterTool(reg, DBDeps{}) // QueryDB nil
	tool, _ := reg.Get("wow_guild_roster")
	resp := tool.Handler(context.Background(), json.RawMessage(`{"guildId":1}`), "")
	got, _ := resp.(map[string]any)
	if msg, _ := got["error"].(string); !strings.Contains(msg, "ops_ro pool not configured") {
		t.Errorf("expected pool-not-configured error, got %v", got)
	}
}

// TestWowGuildRoster_SelectorValidation — the guild selector is rejected BEFORE any
// query when absent, doubly-specified, or negative (a bad guild is a typo, not "all
// guilds"). No SQL round trip on a bad selector.
func TestWowGuildRoster_SelectorValidation(t *testing.T) {
	cases := []struct {
		name    string
		args    string
		wantMsg string
	}{
		{"neither", `{}`, "either guildId or guildName is required"},
		{"both", `{"guildId":1,"guildName":"X"}`, "pass either guildId or guildName, not both"},
		{"negative id", `{"guildId":-3}`, "guildId must be a positive integer"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			// No ExpectExec / ExpectQuery — the handler must bail before touching the DB.
			reg := NewRegistry()
			RegisterWowGuildRosterTool(reg, DBDeps{QueryDB: db})
			tool, _ := reg.Get("wow_guild_roster")
			resp := tool.Handler(context.Background(), json.RawMessage(c.args), "")
			got, _ := resp.(map[string]any)
			if msg, _ := got["error"].(string); !strings.Contains(msg, c.wantMsg) {
				t.Errorf("args %s: expected %q, got %v", c.args, c.wantMsg, got)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Errorf("args %s: a query was issued on a bad selector: %v", c.args, err)
			}
		})
	}
}

// TestWowGuildRoster_TopNClamps asserts the post-clamp topN echo (unset->default,
// zero/negative->default, oversized->max, in-range passthrough) and that the LIMIT bind
// matches the clamped value. minInactiveDays=0 so there is no matched-count round trip
// and no cutoff bind — the list binds only (guildid, LIMIT).
func TestWowGuildRoster_TopNClamps(t *testing.T) {
	cases := []struct {
		name     string
		args     string
		wantTopN int
	}{
		{"unset uses default", `{"guildId":1}`, wowGuildRosterDefaultTop},
		{"zero falls back to default", `{"guildId":1,"topN":0}`, wowGuildRosterDefaultTop},
		{"negative falls back to default", `{"guildId":1,"topN":-7}`, wowGuildRosterDefaultTop},
		{"oversized clamps to max", `{"guildId":1,"topN":99999}`, wowGuildRosterMaxTop},
		{"in-range passes through", `{"guildId":1,"topN":42}`, 42},
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
			mock.ExpectQuery(regexp.QuoteMeta("FROM `guild` WHERE guildid = ?")).
				WithArgs(int64(1)).
				WillReturnRows(sqlmock.NewRows([]string{"guildid", "name", "leaderguid", "createdate"}).
					AddRow(int64(1), "G", int64(0), int64(0)))
			mock.ExpectQuery(regexp.QuoteMeta("SELECT COUNT(*), COALESCE(SUM(c.online = 1), 0)")).
				WithArgs(int64(1)).
				WillReturnRows(sqlmock.NewRows([]string{"cnt", "online"}).AddRow(int64(0), int64(0)))
			mock.ExpectQuery(regexp.QuoteMeta("ORDER BY gm.rank ASC, c.level DESC, gm.guid ASC LIMIT ?")).
				WithArgs(int64(1), int64(c.wantTopN)).
				WillReturnRows(sqlmock.NewRows(guildRosterListCols()))

			reg := NewRegistry()
			RegisterWowGuildRosterTool(reg, DBDeps{QueryDB: db})
			tool, _ := reg.Get("wow_guild_roster")
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

// TestWowGuildRoster_NotFound — an unresolved guild returns a structured found:false
// (with lookupBy/lookupValue) and issues NO aggregate/list queries.
func TestWowGuildRoster_NotFound(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.ExpectExec(regexp.QuoteMeta("USE `acore_characters`")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta("FROM `guild` WHERE guildid = ?")).
		WithArgs(int64(42)).
		WillReturnRows(sqlmock.NewRows([]string{"guildid", "name", "leaderguid", "createdate"})) // no rows

	reg := NewRegistry()
	RegisterWowGuildRosterTool(reg, DBDeps{QueryDB: db})
	tool, _ := reg.Get("wow_guild_roster")
	resp := tool.Handler(context.Background(), json.RawMessage(`{"guildId":42}`), "")
	got, _ := resp.(map[string]any)
	if got["error"] != nil {
		t.Fatalf("unexpected error: %v", got)
	}
	if f, _ := got["found"].(bool); f {
		t.Errorf("found: %v want false", got["found"])
	}
	if lb, _ := got["lookupBy"].(string); lb != "guildId" {
		t.Errorf("lookupBy: %v want guildId", got["lookupBy"])
	}
	if lv, _ := got["lookupValue"].(int64); lv != 42 {
		t.Errorf("lookupValue: %v want 42", got["lookupValue"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestCollectWowGuildRoster_MinInactiveDaysFilter pins the load-bearing cutoff math: with
// a fixed now the cutoff bind is exactly now - minInactiveDays*86400, shared by the
// matched-count and the list. It also fixes the bind order (guildid, cutoff, then LIMIT
// for the list) and proves matchedMembers reflects the filter (2) while totalMembers stays
// the honest full-guild count (10) -> truncated true against the single returned row.
func TestCollectWowGuildRoster_MinInactiveDaysFilter(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	now := time.Unix(1_700_000_000, 0)
	cutoff := now.Unix() - 30*wowGuildRosterDaySeconds // minInactiveDays=30

	mock.ExpectExec(regexp.QuoteMeta("USE `acore_characters`")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta("FROM `guild` WHERE guildid = ?")).
		WithArgs(int64(3)).
		WillReturnRows(sqlmock.NewRows([]string{"guildid", "name", "leaderguid", "createdate"}).
			AddRow(int64(3), "Dormant Legion", int64(99), int64(0)))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT COUNT(*), COALESCE(SUM(c.online = 1), 0)")).
		WithArgs(int64(3)).
		WillReturnRows(sqlmock.NewRows([]string{"cnt", "online"}).AddRow(int64(10), int64(4)))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT COUNT(*) FROM `guild_member` gm JOIN `characters` c ON c.guid = gm.guid WHERE gm.guildid = ? AND c.online = 0 AND c.logout_time > 0 AND c.logout_time <= ?")).
		WithArgs(int64(3), cutoff).
		WillReturnRows(sqlmock.NewRows([]string{"cnt"}).AddRow(int64(2)))
	mock.ExpectQuery(regexp.QuoteMeta("AND c.online = 0 AND c.logout_time > 0 AND c.logout_time <= ? ORDER BY gm.rank ASC, c.level DESC, gm.guid ASC LIMIT ?")).
		WithArgs(int64(3), cutoff, int64(15)).
		WillReturnRows(sqlmock.NewRows(guildRosterListCols()).
			AddRow(int64(50), "Dorm", 1, 1, 0, 70, 0, cutoff-1000, 1, int64(7), 3, "Member", "", ""))

	out, err := collectWowGuildRoster(context.Background(), db, now, 3, "", 15, 30)
	if err != nil {
		t.Fatalf("collectWowGuildRoster: %v", err)
	}
	if md, _ := out["minInactiveDays"].(int); md != 30 {
		t.Errorf("minInactiveDays echo: %v want 30", out["minInactiveDays"])
	}
	if tm, _ := out["totalMembers"].(int64); tm != 10 {
		t.Errorf("totalMembers: %v want 10", out["totalMembers"])
	}
	if om, _ := out["onlineMembers"].(int64); om != 4 {
		t.Errorf("onlineMembers: %v want 4", out["onlineMembers"])
	}
	if mm, _ := out["matchedMembers"].(int64); mm != 2 {
		t.Errorf("matchedMembers: %v want 2", out["matchedMembers"])
	}
	if r, _ := out["returned"].(int); r != 1 {
		t.Errorf("returned: %v want 1", out["returned"])
	}
	if tr, _ := out["truncated"].(bool); !tr {
		t.Errorf("truncated: %v want true (matchedMembers 2 > returned 1)", out["truncated"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestCollectWowGuildRoster_Empty — the empty-roster case. members must be a non-nil empty
// slice (callers expect arrays), returned 0, matchedMembers 0, truncated false. No
// matched-count round trip since minInactiveDays=0.
func TestCollectWowGuildRoster_Empty(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	now := time.Unix(1_700_000_000, 0)
	mock.ExpectExec(regexp.QuoteMeta("USE `acore_characters`")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta("FROM `guild` WHERE guildid = ?")).
		WithArgs(int64(8)).
		WillReturnRows(sqlmock.NewRows([]string{"guildid", "name", "leaderguid", "createdate"}).
			AddRow(int64(8), "Empty", int64(0), int64(0)))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT COUNT(*), COALESCE(SUM(c.online = 1), 0)")).
		WithArgs(int64(8)).
		WillReturnRows(sqlmock.NewRows([]string{"cnt", "online"}).AddRow(int64(0), int64(0)))
	mock.ExpectQuery(regexp.QuoteMeta("ORDER BY gm.rank ASC, c.level DESC, gm.guid ASC LIMIT ?")).
		WithArgs(int64(8), int64(wowGuildRosterDefaultTop)).
		WillReturnRows(sqlmock.NewRows(guildRosterListCols()))

	out, err := collectWowGuildRoster(context.Background(), db, now, 8, "", wowGuildRosterDefaultTop, 0)
	if err != nil {
		t.Fatalf("collectWowGuildRoster: %v", err)
	}
	members, ok := out["members"].([]*guildRosterMember)
	if !ok {
		t.Fatalf("members type: %T want []*guildRosterMember", out["members"])
	}
	if len(members) != 0 {
		t.Errorf("members len: %d want 0", len(members))
	}
	if r, _ := out["returned"].(int); r != 0 {
		t.Errorf("returned: %v want 0", out["returned"])
	}
	if mm, _ := out["matchedMembers"].(int64); mm != 0 {
		t.Errorf("matchedMembers: %v want 0", out["matchedMembers"])
	}
	if tr, _ := out["truncated"].(bool); tr {
		t.Errorf("truncated: %v want false", out["truncated"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestCollectWowGuildRoster_Golden drives the full per-row fold: ORDER-BY-trust (rows
// echoed in DB order, no Go re-sort), race/class name mapping, isGuildMaster (guid ==
// leaderguid), the rank-<id> LEFT-JOIN fallback when guild_rank has no row, the three
// inactiveDays cases (online -> 0/"", offline-with-logout -> whole days + human,
// logout_time=0 -> 0/"" unknown), NULL note -> "", the guild header + createDateISO, and
// the honest totalMembers (4) exceeding the returned list (3) -> truncated true.
func TestCollectWowGuildRoster_Golden(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	now := time.Unix(1_700_000_000, 0)

	mock.ExpectExec(regexp.QuoteMeta("USE `acore_characters`")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta("FROM `guild` WHERE guildid = ?")).
		WithArgs(int64(7)).
		WillReturnRows(sqlmock.NewRows([]string{"guildid", "name", "leaderguid", "createdate"}).
			AddRow(int64(7), "Knights of Cenarius", int64(1001), int64(1_600_000_000)))
	// Honest total 4 > the 3 rows the list returns -> truncated.
	mock.ExpectQuery(regexp.QuoteMeta("SELECT COUNT(*), COALESCE(SUM(c.online = 1), 0)")).
		WithArgs(int64(7)).
		WillReturnRows(sqlmock.NewRows([]string{"cnt", "online"}).AddRow(int64(4), int64(1)))
	mock.ExpectQuery(regexp.QuoteMeta("ORDER BY gm.rank ASC, c.level DESC, gm.guid ASC LIMIT ?")).
		WithArgs(int64(7), int64(3)).
		WillReturnRows(sqlmock.NewRows(guildRosterListCols()).
			// GM: guid==leaderguid, online (inactiveDays 0 despite an old logout_time),
			// rank 0 "Guild Master", notes present.
			AddRow(int64(1001), "Slayo", 1, 2, 0, 80, 1, now.Unix()-100000, 1519, int64(5000), 0, "Guild Master", "founder", "do not kick").
			// officer: offline 3 days, rank 1 "Officer", offnote NULL -> "".
			AddRow(int64(1002), "Officerbot", 5, 8, 1, 80, 0, now.Unix()-3*wowGuildRosterDaySeconds, 85, int64(5001), 1, "Officer", "", nil).
			// fresh recruit: rank row MISSING (rname NULL -> rank-4), logout_time=0
			// (never logged out -> inactiveDays 0/"" unknown), notes NULL.
			AddRow(int64(1003), "Fresh", 7, 4, 0, 1, 0, int64(0), 0, int64(5002), 4, nil, nil, nil))

	out, err := collectWowGuildRoster(context.Background(), db, now, 7, "", 3, 0)
	if err != nil {
		t.Fatalf("collectWowGuildRoster: %v", err)
	}

	if f, _ := out["found"].(bool); !f {
		t.Errorf("found: %v want true", out["found"])
	}
	if r, _ := out["returned"].(int); r != 3 {
		t.Errorf("returned: %v want 3", out["returned"])
	}
	if tm, _ := out["totalMembers"].(int64); tm != 4 {
		t.Errorf("totalMembers: %v want 4", out["totalMembers"])
	}
	if om, _ := out["onlineMembers"].(int64); om != 1 {
		t.Errorf("onlineMembers: %v want 1", out["onlineMembers"])
	}
	// matchedMembers == totalMembers when no filter is applied.
	if mm, _ := out["matchedMembers"].(int64); mm != 4 {
		t.Errorf("matchedMembers: %v want 4", out["matchedMembers"])
	}
	if tr, _ := out["truncated"].(bool); !tr {
		t.Errorf("truncated: %v want true (totalMembers 4 > returned 3)", out["truncated"])
	}

	hdr, ok := out["guild"].(*guildRosterHeader)
	if !ok {
		t.Fatalf("guild type: %T want *guildRosterHeader", out["guild"])
	}
	if hdr.GuildID != 7 || hdr.Name != "Knights of Cenarius" || hdr.LeaderGuid != 1001 {
		t.Errorf("guild header: %+v want id=7 Knights leader=1001", hdr)
	}
	if hdr.CreateDate != 1_600_000_000 || hdr.CreateDateISO == "" {
		t.Errorf("guild createDate: %d / %q want set + ISO", hdr.CreateDate, hdr.CreateDateISO)
	}

	members, _ := out["members"].([]*guildRosterMember)
	if len(members) != 3 {
		t.Fatalf("members len: %d want 3", len(members))
	}

	// members[0]: the GM — online, isGuildMaster, inactiveDays 0 despite an old logout.
	m0 := members[0]
	if m0.Guid != 1001 || !m0.IsGuildMaster || !m0.Online {
		t.Errorf("m0: %+v want guid=1001 GM online", m0)
	}
	if m0.RaceName != "Human" || m0.ClassName != "Paladin" || m0.RankName != "Guild Master" {
		t.Errorf("m0 names: race=%q class=%q rank=%q want Human/Paladin/Guild Master", m0.RaceName, m0.ClassName, m0.RankName)
	}
	if m0.InactiveDays != 0 || m0.InactiveHuman != "" {
		t.Errorf("m0 inactivity: %d / %q want 0 / '' (online)", m0.InactiveDays, m0.InactiveHuman)
	}
	if m0.LogoutTime != now.Unix()-100000 || m0.LogoutTimeISO == "" {
		t.Errorf("m0 logout: %d / %q want set (online members still carry logout_time)", m0.LogoutTime, m0.LogoutTimeISO)
	}
	if m0.PublicNote != "founder" || m0.OfficerNote != "do not kick" {
		t.Errorf("m0 notes: %q / %q want founder / do not kick", m0.PublicNote, m0.OfficerNote)
	}

	// members[1]: officer, offline 3 days -> inactiveDays 3 + "72h 0m", offnote NULL -> "".
	m1 := members[1]
	if m1.Guid != 1002 || m1.IsGuildMaster || m1.Online {
		t.Errorf("m1: %+v want guid=1002 non-GM offline", m1)
	}
	if m1.RankName != "Officer" || m1.RaceName != "Undead" || m1.ClassName != "Mage" {
		t.Errorf("m1 names: rank=%q race=%q class=%q want Officer/Undead/Mage", m1.RankName, m1.RaceName, m1.ClassName)
	}
	if m1.InactiveDays != 3 || m1.InactiveHuman != "72h 0m" {
		t.Errorf("m1 inactivity: %d / %q want 3 / 72h 0m", m1.InactiveDays, m1.InactiveHuman)
	}
	if m1.PublicNote != "" || m1.OfficerNote != "" {
		t.Errorf("m1 notes: %q / %q want '' / '' (offnote NULL)", m1.PublicNote, m1.OfficerNote)
	}

	// members[2]: recruit, missing rank row -> rank-4, logout_time=0 -> unknown inactivity.
	m2 := members[2]
	if m2.Guid != 1003 || m2.RankName != "rank-4" {
		t.Errorf("m2: %+v want guid=1003 rank-4 (LEFT JOIN NULL fallback)", m2)
	}
	if m2.RaceName != "Gnome" || m2.ClassName != "Rogue" {
		t.Errorf("m2 names: race=%q class=%q want Gnome/Rogue", m2.RaceName, m2.ClassName)
	}
	if m2.InactiveDays != 0 || m2.InactiveHuman != "" || m2.LogoutTimeISO != "" {
		t.Errorf("m2 inactivity: %d / %q / iso=%q want 0 / '' / '' (logout_time=0)", m2.InactiveDays, m2.InactiveHuman, m2.LogoutTimeISO)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}
