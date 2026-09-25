package mcpserver

import (
	"context"
	"database/sql/driver"
	"encoding/json"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

// guildMembershipChurnCols is the column set wow_guild_membership_churn scans, in order.
func guildMembershipChurnCols() []string {
	return []string{"guildid", "name", "joins", "leaves", "promotes", "demotes", "invites", "uninvites", "totalEvents", "lastActivity"}
}

// guildMembershipChurnFixture returns three grouped guild rows in guildid-asc order
// (the SQL FETCH order) chosen so each of the four sortBy keys produces a DISTINCT
// ranking, which proves the Go-side comparator actually reads the selected aggregate:
//
//	netMembers: A(8),  C(3),  B(-2)  -> A, C, B  (10, 30, 20)
//	joins:      A(9),  B(8),  C(5)   -> A, B, C  (10, 20, 30)
//	leaves:     B(10), C(2),  A(1)   -> B, C, A  (20, 30, 10)
//	events:     B(30), A(25), C(18)  -> B, A, C  (20, 10, 30)
//
// Every guild's six facets sum exactly to its totalEvents so the fixture is also
// internally consistent (joins+leaves+promotes+demotes+invites+uninvites == events).
func guildMembershipChurnFixture() *sqlmock.Rows {
	return sqlmock.NewRows(guildMembershipChurnCols()).
		// A: <Slayo's Vanguard> id 10; 9 join / 1 leave -> net 8; 25 events.
		AddRow(int64(10), "Slayo's Vanguard", 9, 1, 2, 1, 10, 2, 25, int64(1_699_000_000)).
		// B: <Bleeding Edge> id 20; 8 join / 10 leave -> net -2; 30 events (bleeding).
		AddRow(int64(20), "Bleeding Edge", 8, 10, 1, 3, 6, 2, 30, int64(1_699_500_000)).
		// C: <Steady State> id 30; 5 join / 2 leave -> net 3; 18 events.
		AddRow(int64(30), "Steady State", 5, 2, 4, 0, 6, 1, 18, int64(1_698_000_000))
}

// TestWowGuildMembershipChurn_DescriptionMentionsContext guards the keywords that
// steer the agent here vs wow_guild_summary (headcount census), the guild-BANK
// family (guild_bank_eventlog), or a hand-written db_query — the guild_eventlog
// MEMBERSHIP framing + the EventType-split vocabulary is load-bearing for tool
// selection, and the guild_eventlog-vs-guild_bank_eventlog distinction is the exact
// trap this tool must not be confused with.
func TestWowGuildMembershipChurn_DescriptionMentionsContext(t *testing.T) {
	reg := NewRegistry()
	RegisterWowGuildMembershipChurnTool(reg, DBDeps{})
	tool, ok := reg.Get("wow_guild_membership_churn")
	if !ok {
		t.Fatal("wow_guild_membership_churn not registered")
	}
	for _, kw := range []string{
		"acore_characters.guild_eventlog", "guild_bank_eventlog", "EventType", "GuildEventLogTypes",
		"get_guild_event_log", "wow_guild_summary", "wow_guild_bank_item_flow", "ops_ro",
		"joins", "leaves", "promotes", "demotes", "netMembers", "totalEvents",
		"totalJoins", "distinctGuilds", "topN", "sortBy", "guildId", "sinceHours", "Read-only",
	} {
		if !strings.Contains(tool.Description, kw) {
			t.Errorf("description missing %q: %s", kw, tool.Description)
		}
	}
}

// TestWowGuildMembershipChurn_ReadOnlyAnnotation pins readOnlyHint=true so a future
// copy-paste of a destructive sibling can't silently flip the gate.
func TestWowGuildMembershipChurn_ReadOnlyAnnotation(t *testing.T) {
	reg := NewRegistry()
	RegisterWowGuildMembershipChurnTool(reg, DBDeps{})
	tool, _ := reg.Get("wow_guild_membership_churn")
	if !strings.Contains(string(tool.Annotations), `"readOnlyHint":true`) {
		t.Errorf("annotations missing readOnlyHint=true: %s", tool.Annotations)
	}
}

func TestWowGuildMembershipChurn_NoPoolReturnsError(t *testing.T) {
	reg := NewRegistry()
	RegisterWowGuildMembershipChurnTool(reg, DBDeps{}) // QueryDB nil
	tool, _ := reg.Get("wow_guild_membership_churn")
	resp := tool.Handler(context.Background(), json.RawMessage(`{}`), "")
	got, _ := resp.(map[string]any)
	if msg, _ := got["error"].(string); !strings.Contains(msg, "ops_ro pool not configured") {
		t.Errorf("expected pool-not-configured error, got %v", got)
	}
}

// TestWowGuildMembershipChurn_TopNClamps asserts the post-clamp topN echo. There is
// no LIMIT bind (rows are folded whole then sliced Go-side), so the query is the
// unfiltered shape and only the echo is checked.
func TestWowGuildMembershipChurn_TopNClamps(t *testing.T) {
	cases := []struct {
		name     string
		args     string
		wantTopN int
	}{
		{"unset uses default", `{}`, wowGuildMembershipChurnDefaultTop},
		{"zero falls back to default", `{"topN":0}`, wowGuildMembershipChurnDefaultTop},
		{"negative falls back to default", `{"topN":-7}`, wowGuildMembershipChurnDefaultTop},
		{"oversized clamps to max", `{"topN":99999}`, wowGuildMembershipChurnMaxTop},
		{"in-range passes through", `{"topN":42}`, 42},
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
			mock.ExpectQuery(regexp.QuoteMeta("GROUP BY g.guildid, g.name ORDER BY g.guildid ASC")).
				WillReturnRows(sqlmock.NewRows(guildMembershipChurnCols()))

			reg := NewRegistry()
			RegisterWowGuildMembershipChurnTool(reg, DBDeps{QueryDB: db})
			tool, _ := reg.Get("wow_guild_membership_churn")
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

// TestWowGuildMembershipChurn_InvalidSortByRejected — an unknown sortBy is rejected
// (no DB call). Includes the wrong-case "NETMEMBERS" (keys are case-sensitive) and
// "promotes" (a facet FIELD, not a sort key) so a caller expecting the row's field
// names as sort keys gets a clear error rather than a silent default.
func TestWowGuildMembershipChurn_InvalidSortByRejected(t *testing.T) {
	for _, bad := range []string{`{"sortBy":"bogus"}`, `{"sortBy":"NETMEMBERS"}`, `{"sortBy":"promotes"}`} {
		db, mock, err := sqlmock.New()
		if err != nil {
			t.Fatal(err)
		}
		reg := NewRegistry()
		RegisterWowGuildMembershipChurnTool(reg, DBDeps{QueryDB: db})
		tool, _ := reg.Get("wow_guild_membership_churn")
		resp := tool.Handler(context.Background(), json.RawMessage(bad), "")
		got, _ := resp.(map[string]any)
		if msg, _ := got["error"].(string); !strings.Contains(msg, "invalid sortBy") {
			t.Errorf("args %s: expected invalid-sortBy error, got %v", bad, got)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("args %s: no query should run on reject: %v", bad, err)
		}
		db.Close()
	}
}

// TestWowGuildMembershipChurn_GuildIdRejectsBadId — a non-numeric / negative guildId
// is rejected BEFORE any query is issued (no SQL round trip on a bad filter).
func TestWowGuildMembershipChurn_GuildIdRejectsBadId(t *testing.T) {
	for _, args := range []string{`{"guildId":"abc"}`, `{"guildId":-5}`} {
		db, mock, err := sqlmock.New()
		if err != nil {
			t.Fatal(err)
		}
		// No ExpectExec / ExpectQuery — the handler must bail before touching the DB.
		reg := NewRegistry()
		RegisterWowGuildMembershipChurnTool(reg, DBDeps{QueryDB: db})
		tool, _ := reg.Get("wow_guild_membership_churn")
		resp := tool.Handler(context.Background(), json.RawMessage(args), "")
		got, _ := resp.(map[string]any)
		if msg, _ := got["error"].(string); !strings.Contains(msg, "guildId must be a positive integer") {
			t.Errorf("args %s: expected guildId reject, got %v", args, got)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("args %s: a query was issued on a bad guildId: %v", args, err)
		}
		db.Close()
	}
}

// TestWowGuildMembershipChurn_Filters drives the handler with each filter
// combination and asserts the WHERE clause shape (filters AND-appended, no base
// WHERE since every guild_eventlog row is a membership event), the bound args
// (guildId, then the sinceHours cutoff), and the echoed guildId/sinceHours keys. The
// now-relative sinceHours cutoff bind is matched with AnyArg (the handler uses the
// real clock); its exact value is pinned in TestCollectWowGuildMembershipChurn_SinceCutoff.
func TestWowGuildMembershipChurn_Filters(t *testing.T) {
	cases := []struct {
		name       string
		args       string
		whereSub   string
		wantArgs   []driver.Value
		wantGuild  int64 // -1 = key absent
		wantSinceH int   // -1 = key absent
	}{
		{
			name:      "guildId only",
			args:      `{"guildId":42}`,
			whereSub:  "e.guildid = g.guildid WHERE e.guildid = ? GROUP BY",
			wantArgs:  []driver.Value{uint64(42)},
			wantGuild: 42, wantSinceH: -1,
		},
		{
			name:       "sinceHours only",
			args:       `{"sinceHours":24}`,
			whereSub:   "e.guildid = g.guildid WHERE e.TimeStamp >= ? GROUP BY",
			wantArgs:   []driver.Value{sqlmock.AnyArg()},
			wantGuild:  -1,
			wantSinceH: 24,
		},
		{
			name:       "both filters",
			args:       `{"guildId":7,"sinceHours":12,"topN":5}`,
			whereSub:   "WHERE e.guildid = ? AND e.TimeStamp >= ? GROUP BY",
			wantArgs:   []driver.Value{uint64(7), sqlmock.AnyArg()},
			wantGuild:  7,
			wantSinceH: 12,
		},
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
			mock.ExpectQuery(regexp.QuoteMeta(c.whereSub)).
				WithArgs(c.wantArgs...).
				WillReturnRows(sqlmock.NewRows(guildMembershipChurnCols()))

			reg := NewRegistry()
			RegisterWowGuildMembershipChurnTool(reg, DBDeps{QueryDB: db})
			tool, _ := reg.Get("wow_guild_membership_churn")
			resp := tool.Handler(context.Background(), json.RawMessage(c.args), "")
			got, _ := resp.(map[string]any)
			if got["error"] != nil {
				t.Fatalf("unexpected error: %v", got)
			}
			if c.wantGuild >= 0 {
				if g, _ := got["guildId"].(uint64); g != uint64(c.wantGuild) {
					t.Errorf("guildId echo: %v want %d", got["guildId"], c.wantGuild)
				}
			} else if _, ok := got["guildId"]; ok {
				t.Errorf("guildId should be absent, got %v", got["guildId"])
			}
			if c.wantSinceH >= 0 {
				if s, _ := got["sinceHours"].(int); s != c.wantSinceH {
					t.Errorf("sinceHours: %v want %d", got["sinceHours"], c.wantSinceH)
				}
				if _, ok := got["sinceCutoff"].(string); !ok {
					t.Errorf("sinceCutoff should be present when sinceHours set, got %v", got["sinceCutoff"])
				}
			} else if _, ok := got["sinceHours"]; ok {
				t.Errorf("sinceHours should be absent, got %v", got["sinceHours"])
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Errorf("unmet mock expectations: %v", err)
			}
		})
	}
}

// TestCollectWowGuildMembershipChurn_Golden folds the three-row fixture (default
// sortBy netMembers), verifying the scan field mapping (six facet splits +
// totalEvents + lastActivity), the derived netMembers (joins − leaves, negative for
// the bleeding guild), the lastActivityISO stamp, the honest realm totals (over ALL
// rows, not just the display slice), and that the Go-side sort reorders the
// guildid-asc fetch into netMembers-desc order (A, C, B).
func TestCollectWowGuildMembershipChurn_Golden(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	now := time.Unix(1_700_000_000, 0)
	mock.ExpectExec(regexp.QuoteMeta("USE `acore_characters`")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta("GROUP BY g.guildid, g.name ORDER BY g.guildid ASC")).
		WillReturnRows(guildMembershipChurnFixture())

	out, err := collectWowGuildMembershipChurn(context.Background(), db, now, 25, "netMembers", "", 0)
	if err != nil {
		t.Fatalf("collectWowGuildMembershipChurn: %v", err)
	}
	if dg, _ := out["distinctGuilds"].(int); dg != 3 {
		t.Errorf("distinctGuilds: %v want 3", out["distinctGuilds"])
	}
	if tj, _ := out["totalJoins"].(int64); tj != 22 {
		t.Errorf("totalJoins: %v want 22", out["totalJoins"])
	}
	if tl, _ := out["totalLeaves"].(int64); tl != 13 {
		t.Errorf("totalLeaves: %v want 13", out["totalLeaves"])
	}
	if te, _ := out["totalEvents"].(int64); te != 73 {
		t.Errorf("totalEvents: %v want 73", out["totalEvents"])
	}
	if nm, _ := out["netMembers"].(int64); nm != 9 {
		t.Errorf("netMembers (realm): %v want 9", out["netMembers"])
	}
	if sb, _ := out["sortBy"].(string); sb != "netMembers" {
		t.Errorf("sortBy echo: %v want netMembers", out["sortBy"])
	}
	for _, k := range []string{"guildId", "sinceHours", "sinceCutoff"} {
		if _, ok := out[k]; ok {
			t.Errorf("%s should be absent with no filter, got %v", k, out[k])
		}
	}
	guilds, ok := out["guilds"].([]*guildMembershipChurnRow)
	if !ok || len(guilds) != 3 {
		t.Fatalf("guilds: %T len %d want []*guildMembershipChurnRow len 3", out["guilds"], len(guilds))
	}

	// netMembers desc: A(8), C(3), B(-2).
	if guilds[0].GuildID != 10 || guilds[1].GuildID != 30 || guilds[2].GuildID != 20 {
		t.Errorf("netMembers order: got %d,%d,%d want 10,30,20", guilds[0].GuildID, guilds[1].GuildID, guilds[2].GuildID)
	}

	// guilds[0] is <Slayo's Vanguard> — facet splits, derived net, ISO stamp.
	a := guilds[0]
	if a.Name != "Slayo's Vanguard" || a.Joins != 9 || a.Leaves != 1 || a.Promotes != 2 || a.Demotes != 1 || a.Invites != 10 || a.Uninvites != 2 || a.TotalEvents != 25 {
		t.Errorf("guilds[0] folds: %+v", a)
	}
	if a.NetMembers != 8 {
		t.Errorf("guilds[0] netMembers: %d want 8 (9 joins − 1 leave)", a.NetMembers)
	}
	if a.LastActivity != 1_699_000_000 || a.LastActivityISO != time.Unix(1_699_000_000, 0).UTC().Format(time.RFC3339) {
		t.Errorf("guilds[0] lastActivity: %d / %q", a.LastActivity, a.LastActivityISO)
	}

	// guilds[2] is <Bleeding Edge> — the negative-net bleeding guild.
	b := guilds[2]
	if b.Name != "Bleeding Edge" || b.NetMembers != -2 {
		t.Errorf("guilds[2]: %+v want Bleeding Edge / netMembers -2", b)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestCollectWowGuildMembershipChurn_SortKeys re-runs the same fixture under each
// sortBy key and asserts the Go-side comparator reads the matching aggregate — the
// four keys yield four DISTINCT orderings (see guildMembershipChurnFixture).
func TestCollectWowGuildMembershipChurn_SortKeys(t *testing.T) {
	cases := []struct {
		sortBy    string
		wantOrder []int64
	}{
		{"netMembers", []int64{10, 30, 20}}, // A, C, B
		{"joins", []int64{10, 20, 30}},      // A, B, C
		{"leaves", []int64{20, 30, 10}},     // B, C, A
		{"events", []int64{20, 10, 30}},     // B, A, C
	}
	for _, c := range cases {
		t.Run(c.sortBy, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			mock.ExpectExec(regexp.QuoteMeta("USE `acore_characters`")).
				WillReturnResult(sqlmock.NewResult(0, 0))
			mock.ExpectQuery(regexp.QuoteMeta("GROUP BY g.guildid, g.name ORDER BY g.guildid ASC")).
				WillReturnRows(guildMembershipChurnFixture())

			out, err := collectWowGuildMembershipChurn(context.Background(), db, time.Unix(1_700_000_000, 0), 25, c.sortBy, "", 0)
			if err != nil {
				t.Fatalf("collectWowGuildMembershipChurn: %v", err)
			}
			guilds, _ := out["guilds"].([]*guildMembershipChurnRow)
			if len(guilds) != 3 {
				t.Fatalf("guilds len %d want 3", len(guilds))
			}
			for i, want := range c.wantOrder {
				if guilds[i].GuildID != want {
					t.Errorf("sortBy=%s pos %d: got %d want %d", c.sortBy, i, guilds[i].GuildID, want)
				}
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Errorf("unmet mock expectations: %v", err)
			}
		})
	}
}

// TestCollectWowGuildMembershipChurn_TopNSlice — topN cuts the DISPLAY slice while
// the honest totals stay computed over the full folded set (the reason there is no
// SQL LIMIT). topN=2, netMembers desc keeps A, C and drops B, but totals still see
// all 3 (including the bleeding guild B's leaves).
func TestCollectWowGuildMembershipChurn_TopNSlice(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.ExpectExec(regexp.QuoteMeta("USE `acore_characters`")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta("GROUP BY g.guildid, g.name ORDER BY g.guildid ASC")).
		WillReturnRows(guildMembershipChurnFixture())

	out, err := collectWowGuildMembershipChurn(context.Background(), db, time.Unix(1_700_000_000, 0), 2, "netMembers", "", 0)
	if err != nil {
		t.Fatalf("collectWowGuildMembershipChurn: %v", err)
	}
	guilds, _ := out["guilds"].([]*guildMembershipChurnRow)
	if len(guilds) != 2 {
		t.Fatalf("guilds len %d want 2 (sliced to topN)", len(guilds))
	}
	if guilds[0].GuildID != 10 || guilds[1].GuildID != 30 {
		t.Errorf("topN slice: got %d,%d want 10,30", guilds[0].GuildID, guilds[1].GuildID)
	}
	if dg, _ := out["distinctGuilds"].(int); dg != 3 {
		t.Errorf("distinctGuilds: %v want 3 (full set, not sliced)", out["distinctGuilds"])
	}
	if tl, _ := out["totalLeaves"].(int64); tl != 13 {
		t.Errorf("totalLeaves: %v want 13 (full set includes dropped B)", out["totalLeaves"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestCollectWowGuildMembershipChurn_SinceCutoff pins the exact now-relative cutoff:
// the TimeStamp column is uint32 unix seconds, so the bind is cutoff.Unix() (int64),
// NOT a time.Time. With now=1_700_000_000 and sinceHours=6 the cutoff is
// 1_700_000_000 - 6*3600 = 1_699_978_400, and sinceCutoff echoes its RFC3339 form.
func TestCollectWowGuildMembershipChurn_SinceCutoff(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	now := time.Unix(1_700_000_000, 0)
	wantCutoff := int64(1_699_978_400)
	mock.ExpectExec(regexp.QuoteMeta("USE `acore_characters`")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta("WHERE e.TimeStamp >= ? GROUP BY")).
		WithArgs(wantCutoff).
		WillReturnRows(sqlmock.NewRows(guildMembershipChurnCols()))

	out, err := collectWowGuildMembershipChurn(context.Background(), db, now, 25, "netMembers", "", 6)
	if err != nil {
		t.Fatalf("collectWowGuildMembershipChurn: %v", err)
	}
	if s, _ := out["sinceHours"].(int); s != 6 {
		t.Errorf("sinceHours echo: %v want 6", out["sinceHours"])
	}
	wantISO := time.Unix(wantCutoff, 0).UTC().Format(time.RFC3339)
	if iso, _ := out["sinceCutoff"].(string); iso != wantISO {
		t.Errorf("sinceCutoff echo: %v want %s", out["sinceCutoff"], wantISO)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestCollectWowGuildMembershipChurn_Empty — an empty eventlog window yields a
// non-nil empty guilds slice (callers expect an array) and zeroed honest totals
// (including netMembers, div-free since it is a plain subtraction).
func TestCollectWowGuildMembershipChurn_Empty(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.ExpectExec(regexp.QuoteMeta("USE `acore_characters`")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta("GROUP BY g.guildid, g.name ORDER BY g.guildid ASC")).
		WillReturnRows(sqlmock.NewRows(guildMembershipChurnCols()))

	out, err := collectWowGuildMembershipChurn(context.Background(), db, time.Unix(1_700_000_000, 0), 25, "netMembers", "", 0)
	if err != nil {
		t.Fatalf("collectWowGuildMembershipChurn: %v", err)
	}
	guilds, ok := out["guilds"].([]*guildMembershipChurnRow)
	if !ok {
		t.Fatalf("guilds type: %T want []*guildMembershipChurnRow", out["guilds"])
	}
	if guilds == nil || len(guilds) != 0 {
		t.Errorf("guilds: %v want non-nil empty slice", guilds)
	}
	if dg, _ := out["distinctGuilds"].(int); dg != 0 {
		t.Errorf("distinctGuilds: %v want 0", out["distinctGuilds"])
	}
	if te, _ := out["totalEvents"].(int64); te != 0 {
		t.Errorf("totalEvents: %v want 0", out["totalEvents"])
	}
	if nm, _ := out["netMembers"].(int64); nm != 0 {
		t.Errorf("netMembers: %v want 0", out["netMembers"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}
