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

// guildBankActivityListCols is the column set the list query scans, in order.
func guildBankActivityListCols() []string {
	return []string{
		"guildid", "name", "totalEvents", "distinctActors",
		"itemDeposits", "itemWithdrawals", "itemMoves",
		"moneyInCopper", "moneyWithdrawnCopper", "repairMoneyCopper",
		"firstActivity", "lastActivity",
	}
}

// expectGuildBankActivityPreamble stages the fixed head-of-collect queries every fire runs: the
// USE and the realm-wide window-scoped aggregate (bound on cutoff). The list (and the optional
// matched-count) are staged per-test after this.
func expectGuildBankActivityPreamble(mock sqlmock.Sqlmock, cutoff, totalEvents, distinctGuilds, distinctActors, moneyIn, moneyOut int64) {
	mock.ExpectExec(regexp.QuoteMeta("USE `acore_characters`")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta(
		"SELECT COUNT(*), COUNT(DISTINCT guildid), COUNT(DISTINCT PlayerGuid), " +
			"COALESCE(SUM(CASE WHEN EventType = 4 THEN ItemOrMoney ELSE 0 END), 0), " +
			"COALESCE(SUM(CASE WHEN EventType IN (5, 6) THEN ItemOrMoney ELSE 0 END), 0) " +
			"FROM `guild_bank_eventlog` WHERE TimeStamp >= ?")).
		WithArgs(cutoff).
		WillReturnRows(sqlmock.NewRows([]string{"c", "dg", "da", "in", "out"}).
			AddRow(totalEvents, distinctGuilds, distinctActors, moneyIn, moneyOut))
}

// TestWowGuildBankActivity_DescriptionMentionsContext guards the description keywords — they
// steer the agent to this tool when an operator asks "which guild banks are actively used / who's
// moving the gold?". Drop the eventlog framing or the sibling cross-reference and the agent falls
// back to a hand-written db_query over an OVERLOADED column (the ItemOrMoney money/item trap).
func TestWowGuildBankActivity_DescriptionMentionsContext(t *testing.T) {
	reg := NewRegistry()
	RegisterWowGuildBankActivityTool(reg, DBDeps{})
	tool, ok := reg.Get("wow_guild_bank_activity")
	if !ok {
		t.Fatal("wow_guild_bank_activity not registered")
	}
	for _, kw := range []string{
		"acore_characters", "guild_bank_eventlog", "EventType", "PlayerGuid", "ItemOrMoney",
		"TimeStamp", "ops_ro", "wow_guild_bank_summary", "guildId", "sinceHours", "sortBy", "topN",
		"totalEvents", "distinctActors", "distinctGuilds", "itemDeposits", "itemWithdrawals",
		"moneyIn", "moneyOut", "repairMoney", "netMoney", "recencyHuman", "gold/silver/copper",
		"matchedGuilds", "Read-only",
	} {
		if !strings.Contains(tool.Description, kw) {
			t.Errorf("description missing %q: %s", kw, tool.Description)
		}
	}
}

// TestWowGuildBankActivity_ReadOnlyAnnotation pins readOnlyHint=true so a future copy-paste of a
// destructive sibling can't silently flip the gate.
func TestWowGuildBankActivity_ReadOnlyAnnotation(t *testing.T) {
	reg := NewRegistry()
	RegisterWowGuildBankActivityTool(reg, DBDeps{})
	tool, _ := reg.Get("wow_guild_bank_activity")
	if !strings.Contains(string(tool.Annotations), `"readOnlyHint":true`) {
		t.Errorf("annotations missing readOnlyHint=true: %s", tool.Annotations)
	}
}

func TestWowGuildBankActivity_NoPoolReturnsError(t *testing.T) {
	reg := NewRegistry()
	RegisterWowGuildBankActivityTool(reg, DBDeps{}) // QueryDB nil
	tool, _ := reg.Get("wow_guild_bank_activity")
	resp := tool.Handler(context.Background(), json.RawMessage(`{}`), "")
	got, _ := resp.(map[string]any)
	if msg, _ := got["error"].(string); !strings.Contains(msg, "ops_ro pool not configured") {
		t.Errorf("expected pool-not-configured error, got %v", got)
	}
}

// TestWowGuildBankActivity_InvalidSortBy — an unknown sortBy is rejected BEFORE any query (a
// typo, not a silent default), and only allowlisted values can reach the interpolated ORDER BY.
func TestWowGuildBankActivity_InvalidSortBy(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// No ExpectExec / ExpectQuery — the handler must bail before touching the DB.
	reg := NewRegistry()
	RegisterWowGuildBankActivityTool(reg, DBDeps{QueryDB: db})
	tool, _ := reg.Get("wow_guild_bank_activity")
	resp := tool.Handler(context.Background(), json.RawMessage(`{"sortBy":"; DROP TABLE guild_bank_eventlog"}`), "")
	got, _ := resp.(map[string]any)
	if msg, _ := got["error"].(string); !strings.Contains(msg, "invalid sortBy") {
		t.Errorf("expected invalid-sortBy error, got %v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("a query was issued on a bad sortBy: %v", err)
	}
}

// TestWowGuildBankActivity_NegativeGuildIdRejected — guildId is a drilldown selector, so an
// explicit negative is a typo bounced before any query (0/absent still means "all guilds").
func TestWowGuildBankActivity_NegativeGuildIdRejected(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	reg := NewRegistry()
	RegisterWowGuildBankActivityTool(reg, DBDeps{QueryDB: db})
	tool, _ := reg.Get("wow_guild_bank_activity")
	resp := tool.Handler(context.Background(), json.RawMessage(`{"guildId":-5}`), "")
	got, _ := resp.(map[string]any)
	if msg, _ := got["error"].(string); !strings.Contains(msg, "guildId must be a positive integer") {
		t.Errorf("expected guildId error, got %v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("a query was issued on a bad guildId: %v", err)
	}
}

// TestWowGuildBankActivity_TopNClamps asserts the post-clamp topN echo (unset->default,
// zero/negative->default, oversized->max, in-range passthrough) and that the LIMIT bind matches
// the clamped value. sinceHours=0 -> cutoff 0, no guildId -> no matched-count round trip; the
// list binds (cutoff=0, LIMIT). Default sortBy resolves to the events clause.
func TestWowGuildBankActivity_TopNClamps(t *testing.T) {
	cases := []struct {
		name     string
		args     string
		wantTopN int
	}{
		{"unset uses default", `{}`, wowGuildBankActivityDefaultTop},
		{"zero falls back to default", `{"topN":0}`, wowGuildBankActivityDefaultTop},
		{"negative falls back to default", `{"topN":-7}`, wowGuildBankActivityDefaultTop},
		{"oversized clamps to max", `{"topN":99999}`, wowGuildBankActivityMaxTop},
		{"in-range passes through", `{"topN":42}`, 42},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			expectGuildBankActivityPreamble(mock, 0, 0, 0, 0, 0, 0)
			mock.ExpectQuery(regexp.QuoteMeta("ORDER BY totalEvents DESC, lastActivity DESC, e.guildid ASC LIMIT ?")).
				WithArgs(int64(0), int64(c.wantTopN)).
				WillReturnRows(sqlmock.NewRows(guildBankActivityListCols()))

			reg := NewRegistry()
			RegisterWowGuildBankActivityTool(reg, DBDeps{QueryDB: db})
			tool, _ := reg.Get("wow_guild_bank_activity")
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

// TestWowGuildBankActivity_SortByMapsToClause proves each allowlisted sortBy resolves to its
// ORDER BY clause verbatim (the clause is interpolated, so a wrong mapping is a silent
// injection/behaviour bug). Drives the handler once per key and matches the emitted SQL.
func TestWowGuildBankActivity_SortByMapsToClause(t *testing.T) {
	cases := []struct {
		sortBy string
		clause string
	}{
		{"events", "ORDER BY totalEvents DESC, lastActivity DESC, e.guildid ASC LIMIT ?"},
		{"recency", "ORDER BY lastActivity DESC, totalEvents DESC, e.guildid ASC LIMIT ?"},
		{"guild", "ORDER BY e.guildid ASC LIMIT ?"},
	}
	for _, c := range cases {
		t.Run(c.sortBy, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			expectGuildBankActivityPreamble(mock, 0, 0, 0, 0, 0, 0)
			mock.ExpectQuery(regexp.QuoteMeta(c.clause)).
				WithArgs(int64(0), int64(wowGuildBankActivityDefaultTop)).
				WillReturnRows(sqlmock.NewRows(guildBankActivityListCols()))

			reg := NewRegistry()
			RegisterWowGuildBankActivityTool(reg, DBDeps{QueryDB: db})
			tool, _ := reg.Get("wow_guild_bank_activity")
			resp := tool.Handler(context.Background(), json.RawMessage(`{"sortBy":"`+c.sortBy+`"}`), "")
			got, _ := resp.(map[string]any)
			if got["error"] != nil {
				t.Fatalf("unexpected error: %v", got)
			}
			if sb, _ := got["sortBy"].(string); sb != c.sortBy {
				t.Errorf("sortBy echo: %v want %s", got["sortBy"], c.sortBy)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Errorf("unmet mock expectations: %v", err)
			}
		})
	}
}

// TestCollectWowGuildBankActivity_Golden drives the full per-row fold: ORDER-BY-trust (rows
// echoed in DB order, no Go re-sort), the money split (moneyOut = withdrawals + repairs; net =
// in - out incl. a NEGATIVE-net guild rendered with a leading minus), the raw item-event counts
// (never summed as entry ids), the NULL-name -> guild-<id> fallback, repairMoney omitted when
// zero, recencyHuman from lastActivity, and the honest realm-wide totals with distinctGuilds (3)
// exceeding the returned list (2) -> truncated true.
func TestCollectWowGuildBankActivity_Golden(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	now := time.Unix(1_700_000_000, 0)
	// realm totals: 40 events across 3 guilds, 12 actors; 5_000_000c in / 3_000_000c out ->
	// net 2_000_000c = 200g.
	expectGuildBankActivityPreamble(mock, 0, 40, 3, 12, 5_000_000, 3_000_000)
	mock.ExpectQuery(regexp.QuoteMeta("ORDER BY totalEvents DESC, lastActivity DESC, e.guildid ASC LIMIT ?")).
		WithArgs(int64(0), int64(2)).
		WillReturnRows(sqlmock.NewRows(guildBankActivityListCols()).
			// Rich Legion: 400g in, 100g withdrawn + 50g repair = 150g out, net +250g. recency 1000s = 16m.
			AddRow(int64(10), "Rich Legion", int64(25), int64(8), int64(5), int64(3), int64(2),
				int64(4_000_000), int64(1_000_000), int64(500_000), int64(1_600_000_000), int64(1_699_999_000)).
			// NULL name -> guild-20; 10g in, 60g withdrawn + 0 repair = 60g out, net -50g; repairMoney omitted.
			AddRow(int64(20), nil, int64(15), int64(4), int64(0), int64(1), int64(0),
				int64(100_000), int64(600_000), int64(0), int64(1_650_000_000), int64(1_650_000_500)))

	out, err := collectWowGuildBankActivity(context.Background(), db, now, 2, "events",
		"totalEvents DESC, lastActivity DESC, e.guildid ASC", 0, 0)
	if err != nil {
		t.Fatalf("collectWowGuildBankActivity: %v", err)
	}

	// ---- honest realm-wide totals ----
	if te, _ := out["totalEvents"].(int64); te != 40 {
		t.Errorf("totalEvents: %v want 40", out["totalEvents"])
	}
	if dg, _ := out["distinctGuilds"].(int64); dg != 3 {
		t.Errorf("distinctGuilds: %v want 3", out["distinctGuilds"])
	}
	if da, _ := out["distinctActors"].(int64); da != 12 {
		t.Errorf("distinctActors: %v want 12", out["distinctActors"])
	}
	if v, _ := out["totalMoneyInCopper"].(int64); v != 5_000_000 {
		t.Errorf("totalMoneyInCopper: %v want 5000000", out["totalMoneyInCopper"])
	}
	if v, _ := out["totalMoneyIn"].(string); v != "500g 0s 0c" {
		t.Errorf("totalMoneyIn: %q want 500g 0s 0c", out["totalMoneyIn"])
	}
	if v, _ := out["netMoneyCopper"].(int64); v != 2_000_000 {
		t.Errorf("netMoneyCopper: %v want 2000000", out["netMoneyCopper"])
	}
	if v, _ := out["netMoney"].(string); v != "200g 0s 0c" {
		t.Errorf("netMoney: %q want 200g 0s 0c", out["netMoney"])
	}
	if wh, _ := out["windowHours"].(int); wh != 0 {
		t.Errorf("windowHours: %v want 0", out["windowHours"])
	}
	// no drilldown -> matchedGuilds == distinctGuilds, guildId absent from the response.
	if mg, _ := out["matchedGuilds"].(int64); mg != 3 {
		t.Errorf("matchedGuilds: %v want 3", out["matchedGuilds"])
	}
	if _, present := out["guildId"]; present {
		t.Errorf("guildId should be absent without a drilldown, got %v", out["guildId"])
	}
	if r, _ := out["returned"].(int); r != 2 {
		t.Errorf("returned: %v want 2", out["returned"])
	}
	if tr, _ := out["truncated"].(bool); !tr {
		t.Errorf("truncated: %v want true (distinctGuilds 3 > returned 2)", out["truncated"])
	}

	guilds, ok := out["guilds"].([]*guildBankActivityRow)
	if !ok {
		t.Fatalf("guilds type: %T want []*guildBankActivityRow", out["guilds"])
	}
	if len(guilds) != 2 {
		t.Fatalf("guilds len: %d want 2", len(guilds))
	}

	// guilds[0]: money split + item counts + recency.
	g0 := guilds[0]
	if g0.GuildID != 10 || g0.Name != "Rich Legion" {
		t.Errorf("g0 identity: %+v want id=10 Rich Legion", g0)
	}
	if g0.TotalEvents != 25 || g0.DistinctActors != 8 {
		t.Errorf("g0 counts: events=%d actors=%d want 25/8", g0.TotalEvents, g0.DistinctActors)
	}
	if g0.ItemDeposits != 5 || g0.ItemWithdrawals != 3 || g0.ItemMoves != 2 {
		t.Errorf("g0 item counts: %d/%d/%d want 5/3/2", g0.ItemDeposits, g0.ItemWithdrawals, g0.ItemMoves)
	}
	if g0.MoneyInCopper != 4_000_000 || g0.MoneyIn != "400g 0s 0c" {
		t.Errorf("g0 in: %d / %q want 4000000 / 400g 0s 0c", g0.MoneyInCopper, g0.MoneyIn)
	}
	if g0.MoneyOutCopper != 1_500_000 || g0.MoneyOut != "150g 0s 0c" {
		t.Errorf("g0 out: %d / %q want 1500000 / 150g 0s 0c (withdraw 100g + repair 50g)", g0.MoneyOutCopper, g0.MoneyOut)
	}
	if g0.RepairMoneyCopper != 500_000 || g0.RepairMoney != "50g 0s 0c" {
		t.Errorf("g0 repair: %d / %q want 500000 / 50g 0s 0c", g0.RepairMoneyCopper, g0.RepairMoney)
	}
	if g0.NetMoneyCopper != 2_500_000 || g0.NetMoney != "250g 0s 0c" {
		t.Errorf("g0 net: %d / %q want 2500000 / 250g 0s 0c", g0.NetMoneyCopper, g0.NetMoney)
	}
	if g0.FirstActivity != 1_600_000_000 || g0.FirstActivityISO == "" {
		t.Errorf("g0 first: %d / %q want set + ISO", g0.FirstActivity, g0.FirstActivityISO)
	}
	if g0.LastActivity != 1_699_999_000 || g0.LastActivityISO == "" {
		t.Errorf("g0 last: %d / %q want set + ISO", g0.LastActivity, g0.LastActivityISO)
	}
	if g0.RecencyHuman != "16m" {
		t.Errorf("g0 recency: %q want 16m (now - lastActivity = 1000s)", g0.RecencyHuman)
	}

	// guilds[1]: NULL-name fallback, NEGATIVE net, repairMoney omitted.
	g1 := guilds[1]
	if g1.GuildID != 20 || g1.Name != "guild-20" {
		t.Errorf("g1 identity: %+v want id=20 name=guild-20 (NULL-name fallback)", g1)
	}
	if g1.MoneyOutCopper != 600_000 || g1.MoneyOut != "60g 0s 0c" {
		t.Errorf("g1 out: %d / %q want 600000 / 60g 0s 0c", g1.MoneyOutCopper, g1.MoneyOut)
	}
	if g1.NetMoneyCopper != -500_000 || g1.NetMoney != "-50g 0s 0c" {
		t.Errorf("g1 net: %d / %q want -500000 / -50g 0s 0c (out > in)", g1.NetMoneyCopper, g1.NetMoney)
	}
	if g1.RepairMoneyCopper != 0 || g1.RepairMoney != "" {
		t.Errorf("g1 repair: %d / %q want 0 / '' (omitempty when no repair events)", g1.RepairMoneyCopper, g1.RepairMoney)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestCollectWowGuildBankActivity_GuildIdDrilldown pins the drilldown path: guildId>0 binds the
// extra guildid predicate into BOTH the matched-count and the list WHERE, the matched-count round
// trip fires only under a drilldown, matchedGuilds reflects it (1) while the realm totals stay
// honest, guildId is echoed, and returned==matched -> truncated false.
func TestCollectWowGuildBankActivity_GuildIdDrilldown(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	now := time.Unix(1_700_000_000, 0)
	expectGuildBankActivityPreamble(mock, 0, 40, 3, 12, 5_000_000, 3_000_000)
	mock.ExpectQuery(regexp.QuoteMeta("SELECT COUNT(DISTINCT guildid) FROM `guild_bank_eventlog` WHERE TimeStamp >= ? AND guildid = ?")).
		WithArgs(int64(0), int64(10)).
		WillReturnRows(sqlmock.NewRows([]string{"cnt"}).AddRow(int64(1)))
	mock.ExpectQuery(regexp.QuoteMeta("WHERE e.TimeStamp >= ? AND e.guildid = ? GROUP BY e.guildid, g.name ORDER BY totalEvents DESC, lastActivity DESC, e.guildid ASC LIMIT ?")).
		WithArgs(int64(0), int64(10), int64(wowGuildBankActivityDefaultTop)).
		WillReturnRows(sqlmock.NewRows(guildBankActivityListCols()).
			AddRow(int64(10), "Rich Legion", int64(25), int64(8), int64(5), int64(3), int64(2),
				int64(4_000_000), int64(1_000_000), int64(500_000), int64(1_600_000_000), int64(1_699_999_000)))

	out, err := collectWowGuildBankActivity(context.Background(), db, now, wowGuildBankActivityDefaultTop, "events",
		"totalEvents DESC, lastActivity DESC, e.guildid ASC", 10, 0)
	if err != nil {
		t.Fatalf("collectWowGuildBankActivity: %v", err)
	}
	if gid, _ := out["guildId"].(int64); gid != 10 {
		t.Errorf("guildId echo: %v want 10", out["guildId"])
	}
	// realm totals stay honest (realm-wide) even under a drilldown.
	if dg, _ := out["distinctGuilds"].(int64); dg != 3 {
		t.Errorf("distinctGuilds: %v want 3 (realm-wide)", out["distinctGuilds"])
	}
	if mg, _ := out["matchedGuilds"].(int64); mg != 1 {
		t.Errorf("matchedGuilds: %v want 1 (drilldown)", out["matchedGuilds"])
	}
	if r, _ := out["returned"].(int); r != 1 {
		t.Errorf("returned: %v want 1", out["returned"])
	}
	if tr, _ := out["truncated"].(bool); tr {
		t.Errorf("truncated: %v want false (matchedGuilds 1 == returned 1)", out["truncated"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestCollectWowGuildBankActivity_SinceHoursWindow pins the window math: sinceHours=24 binds
// cutoff = now - 24h into the aggregate AND the list (both scoped to the window), and windowHours
// is echoed. No guildId -> no matched-count round trip.
func TestCollectWowGuildBankActivity_SinceHoursWindow(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	now := time.Unix(1_700_000_000, 0)
	cutoff := now.Unix() - int64(24)*wowGuildBankActivityHourSecs // 1_699_913_600

	expectGuildBankActivityPreamble(mock, cutoff, 10, 2, 5, 1_000_000, 250_000)
	mock.ExpectQuery(regexp.QuoteMeta("ORDER BY totalEvents DESC, lastActivity DESC, e.guildid ASC LIMIT ?")).
		WithArgs(cutoff, int64(wowGuildBankActivityDefaultTop)).
		WillReturnRows(sqlmock.NewRows(guildBankActivityListCols()).
			AddRow(int64(7), "Recent Crew", int64(10), int64(5), int64(2), int64(1), int64(0),
				int64(1_000_000), int64(250_000), int64(0), int64(1_699_950_000), int64(1_699_990_000)))

	out, err := collectWowGuildBankActivity(context.Background(), db, now, wowGuildBankActivityDefaultTop, "events",
		"totalEvents DESC, lastActivity DESC, e.guildid ASC", 0, 24)
	if err != nil {
		t.Fatalf("collectWowGuildBankActivity: %v", err)
	}
	if wh, _ := out["windowHours"].(int); wh != 24 {
		t.Errorf("windowHours echo: %v want 24", out["windowHours"])
	}
	if te, _ := out["totalEvents"].(int64); te != 10 {
		t.Errorf("totalEvents: %v want 10", out["totalEvents"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestCollectWowGuildBankActivity_Empty — no guild-bank activity on the realm. guilds must be a
// non-nil empty slice (callers expect arrays), totals 0, the money strings "0c", returned 0,
// matchedGuilds 0, truncated false. No matched-count round trip (no guildId).
func TestCollectWowGuildBankActivity_Empty(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	now := time.Unix(1_700_000_000, 0)
	expectGuildBankActivityPreamble(mock, 0, 0, 0, 0, 0, 0)
	mock.ExpectQuery(regexp.QuoteMeta("ORDER BY totalEvents DESC, lastActivity DESC, e.guildid ASC LIMIT ?")).
		WithArgs(int64(0), int64(wowGuildBankActivityDefaultTop)).
		WillReturnRows(sqlmock.NewRows(guildBankActivityListCols()))

	out, err := collectWowGuildBankActivity(context.Background(), db, now, wowGuildBankActivityDefaultTop, "events",
		"totalEvents DESC, lastActivity DESC, e.guildid ASC", 0, 0)
	if err != nil {
		t.Fatalf("collectWowGuildBankActivity: %v", err)
	}
	guilds, ok := out["guilds"].([]*guildBankActivityRow)
	if !ok {
		t.Fatalf("guilds type: %T want []*guildBankActivityRow", out["guilds"])
	}
	if len(guilds) != 0 {
		t.Errorf("guilds len: %d want 0", len(guilds))
	}
	if v, _ := out["totalMoneyIn"].(string); v != "0c" {
		t.Errorf("totalMoneyIn: %q want 0c", out["totalMoneyIn"])
	}
	if v, _ := out["netMoney"].(string); v != "0c" {
		t.Errorf("netMoney: %q want 0c", out["netMoney"])
	}
	if r, _ := out["returned"].(int); r != 0 {
		t.Errorf("returned: %v want 0", out["returned"])
	}
	if mg, _ := out["matchedGuilds"].(int64); mg != 0 {
		t.Errorf("matchedGuilds: %v want 0", out["matchedGuilds"])
	}
	if tr, _ := out["truncated"].(bool); tr {
		t.Errorf("truncated: %v want false", out["truncated"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}
