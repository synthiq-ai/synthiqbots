package mcpserver

import (
	"context"
	"encoding/json"
	"math"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

// guildSummaryAggCols is the column set the aggregate query scans, in SELECT order.
func guildSummaryAggCols() []string {
	return []string{
		"guildid", "name", "leaderguid", "leaderName", "memberCount",
		"onlineMembers", "maxLevel", "avgLevel", "createdate", "lastLogout",
	}
}

const guildSummaryAggMatch = "GROUP BY g.guildid, g.name, g.leaderguid, g.createdate ORDER BY g.guildid ASC LIMIT ?"

// TestWowGuildSummary_DescriptionMentionsContext guards the description keywords — they
// route the agent here for "which guilds are biggest / most active / most abandoned?"
// instead of a hand-written db_query, and cross-reference the wow_guild_roster drilldown.
func TestWowGuildSummary_DescriptionMentionsContext(t *testing.T) {
	reg := NewRegistry()
	RegisterWowGuildSummaryTool(reg, DBDeps{})
	tool, ok := reg.Get("wow_guild_summary")
	if !ok {
		t.Fatal("wow_guild_summary not registered")
	}
	for _, kw := range []string{
		"acore_characters", "guild_member", "ops_ro", "wow_guild_roster", "discovery",
		"leaderName", "memberCount", "onlineMembers", "maxLevel", "avgLevel",
		"createDateISO", "lastActivity", "inactiveDays", "inactiveHuman", "dormant",
		"totalGuilds", "matchedGuilds", "sortBy", "minMembers", "minInactiveDays",
		"wow_mail_unread", "topN", "Read-only",
	} {
		if !strings.Contains(tool.Description, kw) {
			t.Errorf("description missing %q: %s", kw, tool.Description)
		}
	}
}

// TestWowGuildSummary_ReadOnlyAnnotation pins readOnlyHint=true so a copy-paste of a
// destructive sibling can't silently flip the gate.
func TestWowGuildSummary_ReadOnlyAnnotation(t *testing.T) {
	reg := NewRegistry()
	RegisterWowGuildSummaryTool(reg, DBDeps{})
	tool, _ := reg.Get("wow_guild_summary")
	if !strings.Contains(string(tool.Annotations), `"readOnlyHint":true`) {
		t.Errorf("annotations missing readOnlyHint=true: %s", tool.Annotations)
	}
}

func TestWowGuildSummary_NoPoolReturnsError(t *testing.T) {
	reg := NewRegistry()
	RegisterWowGuildSummaryTool(reg, DBDeps{}) // QueryDB nil
	tool, _ := reg.Get("wow_guild_summary")
	resp := tool.Handler(context.Background(), json.RawMessage(`{}`), "")
	got, _ := resp.(map[string]any)
	if msg, _ := got["error"].(string); !strings.Contains(msg, "ops_ro pool not configured") {
		t.Errorf("expected pool-not-configured error, got %v", got)
	}
}

// TestWowGuildSummary_BadSortByRejectedBeforeQuery — an unknown sortBy is rejected with a
// self-correcting allowlist error and issues NO SQL (the sort key is validated before the
// pool check runs a query). QueryDB is non-nil to prove the reject fires before any query.
func TestWowGuildSummary_BadSortByRejectedBeforeQuery(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// No ExpectQuery — the handler must bail before touching the DB.
	reg := NewRegistry()
	RegisterWowGuildSummaryTool(reg, DBDeps{QueryDB: db})
	tool, _ := reg.Get("wow_guild_summary")
	resp := tool.Handler(context.Background(), json.RawMessage(`{"sortBy":"bogus"}`), "")
	got, _ := resp.(map[string]any)
	if msg, _ := got["error"].(string); !strings.Contains(msg, "invalid sortBy") || !strings.Contains(msg, "members") {
		t.Errorf("expected invalid-sortBy error echoing the allowlist, got %v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("a query was issued on a bad sortBy: %v", err)
	}
}

// TestNormalizeWowGuildSummarySortBy pins the normalizer: empty/whitespace → the default
// members; case/whitespace-insensitive match for each canonical key; unknown → error.
func TestNormalizeWowGuildSummarySortBy(t *testing.T) {
	for _, c := range []struct {
		in   string
		want string
		err  bool
	}{
		{"", wowGuildSummarySortMembers, false},
		{"   ", wowGuildSummarySortMembers, false},
		{"members", wowGuildSummarySortMembers, false},
		{" ONLINE ", wowGuildSummarySortOnline, false},
		{"Inactive", wowGuildSummarySortInactive, false},
		{"maxlevel", wowGuildSummarySortMaxLevel, false},
		{"created", wowGuildSummarySortCreated, false},
		{"size", "", true},
		{"membercount", "", true},
	} {
		got, err := normalizeWowGuildSummarySortBy(c.in)
		if c.err {
			if err == nil {
				t.Errorf("normalize(%q): expected error, got %q", c.in, got)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("normalize(%q): got (%q,%v) want (%q,nil)", c.in, got, err, c.want)
		}
	}
}

// TestWowGuildSummarySortKeysDriftGuard fails if a key is added/removed without updating
// the sort fn + tests — the allowlist is the single source of truth for the sort surface.
func TestWowGuildSummarySortKeysDriftGuard(t *testing.T) {
	if len(wowGuildSummarySortKeys) != 5 {
		t.Fatalf("sort-key count changed to %d — update sortWowGuildSummary + its tests", len(wowGuildSummarySortKeys))
	}
	want := map[string]bool{"members": true, "online": true, "inactive": true, "maxlevel": true, "created": true}
	for _, k := range wowGuildSummarySortKeys {
		if !want[k] {
			t.Errorf("unexpected sort key %q — add a branch in sortWowGuildSummary + a case here", k)
		}
	}
}

// TestSortWowGuildSummary drives the sort fn in isolation for every key, including the
// tiebreaker chains ending in guildId asc — the load-bearing determinism guard against
// Go's randomized map-iter order flaking tied rows across runs.
func TestSortWowGuildSummary(t *testing.T) {
	// Fresh copy per case so a prior sort doesn't seed the next.
	rows := func() []*guildSummaryRow {
		return []*guildSummaryRow{
			{GuildID: 1, MemberCount: 10, OnlineMembers: 2, InactiveDays: 0, MaxLevel: 80, CreateDate: 300},
			{GuildID: 2, MemberCount: 30, OnlineMembers: 0, InactiveDays: 50, MaxLevel: 60, CreateDate: 100},
			{GuildID: 3, MemberCount: 30, OnlineMembers: 5, InactiveDays: 0, MaxLevel: 60, CreateDate: 200},
			{GuildID: 4, MemberCount: 10, OnlineMembers: 0, InactiveDays: 50, MaxLevel: 90, CreateDate: 100},
		}
	}
	ids := func(rs []*guildSummaryRow) []int64 {
		out := make([]int64, len(rs))
		for i, r := range rs {
			out[i] = r.GuildID
		}
		return out
	}
	eq := func(a, b []int64) bool {
		if len(a) != len(b) {
			return false
		}
		for i := range a {
			if a[i] != b[i] {
				return false
			}
		}
		return true
	}
	for _, c := range []struct {
		key  string
		want []int64
	}{
		{"members", []int64{2, 3, 1, 4}},  // 30,30,10,10 → tie broken by guildId asc
		{"online", []int64{3, 1, 2, 4}},   // 5,2, then 0s by members desc (30>10)
		{"inactive", []int64{2, 4, 3, 1}}, // 50,50 (members desc) then 0,0 (members desc)
		{"maxlevel", []int64{4, 1, 2, 3}}, // 90,80, then 60,60 by guildId asc
		{"created", []int64{2, 4, 3, 1}},  // 100,100 (guildId asc) then 200,300
	} {
		rs := rows()
		sortWowGuildSummary(rs, c.key)
		if got := ids(rs); !eq(got, c.want) {
			t.Errorf("sort %q: got %v want %v", c.key, got, c.want)
		}
	}
}

// TestCollectWowGuildSummary_Empty — no guilds on the realm. guilds must be a non-nil empty
// slice (callers expect arrays), totalGuilds/matchedGuilds 0, truncated + scanTruncated false.
func TestCollectWowGuildSummary_Empty(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	now := time.Unix(1_700_000_000, 0)
	mock.ExpectExec(regexp.QuoteMeta("USE `acore_characters`")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT COUNT(*) FROM `guild`")).
		WillReturnRows(sqlmock.NewRows([]string{"cnt"}).AddRow(int64(0)))
	mock.ExpectQuery(regexp.QuoteMeta(guildSummaryAggMatch)).
		WithArgs(int64(wowGuildSummaryScanCap + 1)).
		WillReturnRows(sqlmock.NewRows(guildSummaryAggCols()))

	out, err := collectWowGuildSummary(context.Background(), db, now, wowGuildSummarySortMembers, wowGuildSummaryDefaultTop, 0, 0)
	if err != nil {
		t.Fatalf("collectWowGuildSummary: %v", err)
	}
	guilds, ok := out["guilds"].([]*guildSummaryRow)
	if !ok {
		t.Fatalf("guilds type: %T want []*guildSummaryRow", out["guilds"])
	}
	if len(guilds) != 0 {
		t.Errorf("guilds len: %d want 0", len(guilds))
	}
	if tg, _ := out["totalGuilds"].(int64); tg != 0 {
		t.Errorf("totalGuilds: %v want 0", out["totalGuilds"])
	}
	if mg, _ := out["matchedGuilds"].(int); mg != 0 {
		t.Errorf("matchedGuilds: %v want 0", out["matchedGuilds"])
	}
	if tr, _ := out["truncated"].(bool); tr {
		t.Errorf("truncated: %v want false", out["truncated"])
	}
	if st, _ := out["scanTruncated"].(bool); st {
		t.Errorf("scanTruncated: %v want false", out["scanTruncated"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestCollectWowGuildSummary_Golden drives the full per-row fold: the aggregate LIMIT binds
// scanCap+1; rows are fed OUT of order to prove the Go sort (default members desc) drives the
// output; leaderName resolves (and NULL → "" for a deleted leader); avgLevel rounds to 1dp;
// and the three dormancy cases — online guild (lastActivity=now, inactiveDays 0), offline
// guild (most-recent logout → inactiveDays + human), never-logged-out guild (lastActivity 0,
// inactiveDays 0, ISO "").
func TestCollectWowGuildSummary_Golden(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	now := time.Unix(1_700_000_000, 0)
	n := now.Unix()

	mock.ExpectExec(regexp.QuoteMeta("USE `acore_characters`")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT COUNT(*) FROM `guild`")).
		WillReturnRows(sqlmock.NewRows([]string{"cnt"}).AddRow(int64(4)))
	mock.ExpectQuery(regexp.QuoteMeta(guildSummaryAggMatch)).
		WithArgs(int64(wowGuildSummaryScanCap + 1)).
		WillReturnRows(sqlmock.NewRows(guildSummaryAggCols()).
			// Fed scrambled (B, A, C, D) — the Go members-desc sort must reorder to A,D,B,C.
			// B: dormant 10d, leader deleted (NULL leaderName).
			AddRow(int64(2), "Dormant", int64(2001), nil, int64(10), int64(0), 70, float64(55.0), int64(1_610_000_000), n-10*wowGuildSummaryDaySeconds).
			// A: biggest, online → active now, avg rounds 72.34 → 72.3.
			AddRow(int64(1), "Big Online", int64(1001), "Slayo", int64(50), int64(5), 80, float64(72.34), int64(1_600_000_000), n-100000).
			// C: ghost — nobody ever logged out (lastLogout 0) → unknown activity.
			AddRow(int64(3), "Ghost", int64(3001), "Ghosty", int64(3), int64(0), 20, float64(12.0), int64(1_620_000_000), int64(0)).
			// D: medium, online → active now.
			AddRow(int64(4), "Medium", int64(4001), "Med", int64(25), int64(2), 80, float64(65.5), int64(1_615_000_000), n-1000))

	out, err := collectWowGuildSummary(context.Background(), db, now, wowGuildSummarySortMembers, wowGuildSummaryDefaultTop, 0, 0)
	if err != nil {
		t.Fatalf("collectWowGuildSummary: %v", err)
	}

	if tg, _ := out["totalGuilds"].(int64); tg != 4 {
		t.Errorf("totalGuilds: %v want 4", out["totalGuilds"])
	}
	if mg, _ := out["matchedGuilds"].(int); mg != 4 {
		t.Errorf("matchedGuilds: %v want 4", out["matchedGuilds"])
	}
	if r, _ := out["returned"].(int); r != 4 {
		t.Errorf("returned: %v want 4", out["returned"])
	}
	if tr, _ := out["truncated"].(bool); tr {
		t.Errorf("truncated: %v want false", out["truncated"])
	}

	guilds, _ := out["guilds"].([]*guildSummaryRow)
	if len(guilds) != 4 {
		t.Fatalf("guilds len: %d want 4", len(guilds))
	}
	if got := []int64{guilds[0].GuildID, guilds[1].GuildID, guilds[2].GuildID, guilds[3].GuildID}; got[0] != 1 || got[1] != 4 || got[2] != 2 || got[3] != 3 {
		t.Fatalf("members-desc order: %v want [1 4 2 3]", got)
	}

	// guilds[0] = A: biggest, online → active now, avg 72.3, leaderName resolved.
	a := guilds[0]
	if a.MemberCount != 50 || a.OnlineMembers != 5 || a.LeaderName != "Slayo" {
		t.Errorf("A: %+v want members=50 online=5 leader=Slayo", a)
	}
	if math.Abs(a.AvgLevel-72.3) > 1e-9 {
		t.Errorf("A avgLevel: %v want 72.3 (72.34 rounded 1dp)", a.AvgLevel)
	}
	if a.LastActivity != n || a.LastActivityISO == "" || a.InactiveDays != 0 || a.InactiveHuman != "" {
		t.Errorf("A activity: last=%d iso=%q days=%d human=%q want now / set / 0 / '' (online)", a.LastActivity, a.LastActivityISO, a.InactiveDays, a.InactiveHuman)
	}
	if a.CreateDateISO == "" {
		t.Errorf("A createDateISO empty")
	}

	// guilds[2] = B: dormant 10 days, leader deleted → leaderName "".
	b := guilds[2]
	if b.GuildID != 2 || b.LeaderName != "" {
		t.Errorf("B: %+v want id=2 leaderName='' (deleted leader)", b)
	}
	if b.LastActivity != n-10*wowGuildSummaryDaySeconds || b.InactiveDays != 10 || b.InactiveHuman != "240h 0m" {
		t.Errorf("B activity: last=%d days=%d human=%q want %d / 10 / 240h 0m", b.LastActivity, b.InactiveDays, b.InactiveHuman, n-10*wowGuildSummaryDaySeconds)
	}

	// guilds[3] = C: never-logged-out → unknown activity (lastActivity 0, ISO "", days 0).
	c := guilds[3]
	if c.GuildID != 3 || c.LastActivity != 0 || c.LastActivityISO != "" || c.InactiveDays != 0 || c.InactiveHuman != "" {
		t.Errorf("C activity: id=%d last=%d iso=%q days=%d human=%q want id=3 / 0 / '' / 0 / '' (unknown)", c.GuildID, c.LastActivity, c.LastActivityISO, c.InactiveDays, c.InactiveHuman)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// addGuildSummaryFilterFixture seeds four guilds used by the filter cases: an active guild
// (nobody dormant), two dormant guilds (40d / 5d), and a never-logged-out guild (unknown).
func addGuildSummaryFilterFixture(rows *sqlmock.Rows, now time.Time) *sqlmock.Rows {
	n := now.Unix()
	return rows.
		AddRow(int64(1), "Active", int64(11), "L1", int64(50), int64(3), 80, float64(70.0), int64(1_600_000_000), int64(0)). // online → active now
		AddRow(int64(2), "Dormant40", int64(21), "L2", int64(8), int64(0), 70, float64(55.0), int64(1_600_000_000), n-40*wowGuildSummaryDaySeconds).
		AddRow(int64(3), "Dormant5", int64(31), "L3", int64(2), int64(0), 30, float64(20.0), int64(1_600_000_000), n-5*wowGuildSummaryDaySeconds).
		AddRow(int64(4), "Unknown", int64(41), "L4", int64(20), int64(0), 60, float64(40.0), int64(1_600_000_000), int64(0)) // never logged out
}

// TestCollectWowGuildSummary_Filters pins the two Go-side filters against a fixed fixture:
// minMembers floors the roster size; minInactiveDays isolates dormant guilds (online and
// never-logged-out guilds are excluded); and topN truncation reports the honest pre-limit
// matchedGuilds with truncated=true.
func TestCollectWowGuildSummary_Filters(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	for _, c := range []struct {
		name            string
		sortKey         string
		topN            int
		minMembers      int
		minInactiveDays int
		wantMatched     int
		wantIDs         []int64
		wantTruncated   bool
	}{
		// minMembers >= 10 keeps Active(50) + Unknown(20); Dormant40(8)/Dormant5(2) drop.
		{"minMembers", "members", 50, 10, 0, 2, []int64{1, 4}, false},
		// minInactiveDays 30 keeps only Dormant40 — Active(online) + Unknown(no logout) + Dormant5(5d) all excluded.
		{"minInactiveDays", "inactive", 50, 0, 30, 1, []int64{2}, false},
		// no filter, topN 2 → matched 4 (honest) but only the 2 biggest returned → truncated.
		{"topNtruncation", "members", 2, 0, 0, 4, []int64{1, 4}, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			mock.ExpectExec(regexp.QuoteMeta("USE `acore_characters`")).
				WillReturnResult(sqlmock.NewResult(0, 0))
			mock.ExpectQuery(regexp.QuoteMeta("SELECT COUNT(*) FROM `guild`")).
				WillReturnRows(sqlmock.NewRows([]string{"cnt"}).AddRow(int64(4)))
			mock.ExpectQuery(regexp.QuoteMeta(guildSummaryAggMatch)).
				WithArgs(int64(wowGuildSummaryScanCap + 1)).
				WillReturnRows(addGuildSummaryFilterFixture(sqlmock.NewRows(guildSummaryAggCols()), now))

			out, err := collectWowGuildSummary(context.Background(), db, now, c.sortKey, c.topN, c.minMembers, c.minInactiveDays)
			if err != nil {
				t.Fatalf("collectWowGuildSummary: %v", err)
			}
			if mg, _ := out["matchedGuilds"].(int); mg != c.wantMatched {
				t.Errorf("matchedGuilds: %v want %d", out["matchedGuilds"], c.wantMatched)
			}
			if tr, _ := out["truncated"].(bool); tr != c.wantTruncated {
				t.Errorf("truncated: %v want %v", out["truncated"], c.wantTruncated)
			}
			guilds, _ := out["guilds"].([]*guildSummaryRow)
			if len(guilds) != len(c.wantIDs) {
				t.Fatalf("returned len: %d want %d", len(guilds), len(c.wantIDs))
			}
			for i, want := range c.wantIDs {
				if guilds[i].GuildID != want {
					t.Errorf("guilds[%d].GuildID: %d want %d", i, guilds[i].GuildID, want)
				}
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Errorf("unmet mock expectations: %v", err)
			}
		})
	}
}

// TestWowGuildSummary_TopNClamps asserts the post-clamp topN echo end-to-end through the
// handler (unset/zero/negative → default, oversized → max, in-range passthrough) with a
// single-guild fixture so the clamp is visible independent of matched count.
func TestWowGuildSummary_TopNClamps(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	for _, c := range []struct {
		name     string
		args     string
		wantTopN int
	}{
		{"unset uses default", `{}`, wowGuildSummaryDefaultTop},
		{"zero falls back to default", `{"topN":0}`, wowGuildSummaryDefaultTop},
		{"negative falls back to default", `{"topN":-7}`, wowGuildSummaryDefaultTop},
		{"oversized clamps to max", `{"topN":99999}`, wowGuildSummaryMaxTop},
		{"in-range passes through", `{"topN":42}`, 42},
	} {
		t.Run(c.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			mock.ExpectExec(regexp.QuoteMeta("USE `acore_characters`")).
				WillReturnResult(sqlmock.NewResult(0, 0))
			mock.ExpectQuery(regexp.QuoteMeta("SELECT COUNT(*) FROM `guild`")).
				WillReturnRows(sqlmock.NewRows([]string{"cnt"}).AddRow(int64(1)))
			mock.ExpectQuery(regexp.QuoteMeta(guildSummaryAggMatch)).
				WithArgs(int64(wowGuildSummaryScanCap + 1)).
				WillReturnRows(sqlmock.NewRows(guildSummaryAggCols()).
					AddRow(int64(1), "Solo", int64(11), "L1", int64(1), int64(0), 60, float64(60.0), int64(1_600_000_000), int64(0)))

			reg := NewRegistry()
			RegisterWowGuildSummaryTool(reg, DBDeps{QueryDB: db})
			tool, _ := reg.Get("wow_guild_summary")
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
			_ = now
		})
	}
}
