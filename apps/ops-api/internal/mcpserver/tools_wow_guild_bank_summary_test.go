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

// guildBankSummaryListCols is the column set the list query scans, in order.
func guildBankSummaryListCols() []string {
	return []string{"guildid", "name", "leaderguid", "BankMoney", "createdate", "tabCount", "tabNames"}
}

// expectGuildBankPreamble stages the fixed head-of-collect queries every fire runs: the
// USE, the realm-wide aggregate, and the total-tabs count. The list (and the optional
// matched-count) are staged per-test after this.
func expectGuildBankPreamble(mock sqlmock.Sqlmock, totalGuilds, guildsWithBank, totalBankCopper, totalTabs int64) {
	mock.ExpectExec(regexp.QuoteMeta("USE `acore_characters`")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT COUNT(*), COALESCE(SUM(BankMoney > 0), 0), COALESCE(SUM(BankMoney), 0) FROM `guild`")).
		WillReturnRows(sqlmock.NewRows([]string{"cnt", "withBank", "total"}).
			AddRow(totalGuilds, guildsWithBank, totalBankCopper))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT COUNT(*) FROM `guild_bank_tab`")).
		WillReturnRows(sqlmock.NewRows([]string{"cnt"}).AddRow(totalTabs))
}

// TestWowGuildBankSummary_DescriptionMentionsContext guards the description keywords —
// they steer the agent to this tool when an operator asks "which guilds are hoarding gold
// / have the most bank tabs?". Drop the guild-table framing or the sibling cross-reference
// and the agent falls back to a hand-written db_query.
func TestWowGuildBankSummary_DescriptionMentionsContext(t *testing.T) {
	reg := NewRegistry()
	RegisterWowGuildBankSummaryTool(reg, DBDeps{})
	tool, ok := reg.Get("wow_guild_bank_summary")
	if !ok {
		t.Fatal("wow_guild_bank_summary not registered")
	}
	for _, kw := range []string{
		"acore_characters", "guild_bank_tab", "BankMoney", "ops_ro", "wow_guild_roster",
		"guildId", "leaderGuid", "bankMoneyCopper", "bankMoney", "tabCount", "tabNames",
		"createDateISO", "totalGuilds", "guildsWithBank", "totalBankMoney", "totalTabs",
		"matchedGuilds", "minBankGold", "sortBy", "topN", "gold/silver/copper", "Read-only",
	} {
		if !strings.Contains(tool.Description, kw) {
			t.Errorf("description missing %q: %s", kw, tool.Description)
		}
	}
}

// TestWowGuildBankSummary_ReadOnlyAnnotation pins readOnlyHint=true so a future copy-paste
// of a destructive sibling can't silently flip the gate.
func TestWowGuildBankSummary_ReadOnlyAnnotation(t *testing.T) {
	reg := NewRegistry()
	RegisterWowGuildBankSummaryTool(reg, DBDeps{})
	tool, _ := reg.Get("wow_guild_bank_summary")
	if !strings.Contains(string(tool.Annotations), `"readOnlyHint":true`) {
		t.Errorf("annotations missing readOnlyHint=true: %s", tool.Annotations)
	}
}

func TestWowGuildBankSummary_NoPoolReturnsError(t *testing.T) {
	reg := NewRegistry()
	RegisterWowGuildBankSummaryTool(reg, DBDeps{}) // QueryDB nil
	tool, _ := reg.Get("wow_guild_bank_summary")
	resp := tool.Handler(context.Background(), json.RawMessage(`{}`), "")
	got, _ := resp.(map[string]any)
	if msg, _ := got["error"].(string); !strings.Contains(msg, "ops_ro pool not configured") {
		t.Errorf("expected pool-not-configured error, got %v", got)
	}
}

// TestWowGuildBankSummary_InvalidSortBy — an unknown sortBy is rejected BEFORE any query
// (a typo, not a silent default), and only allowlisted values can reach the ORDER BY. No
// SQL round trip on a bad sort key.
func TestWowGuildBankSummary_InvalidSortBy(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// No ExpectExec / ExpectQuery — the handler must bail before touching the DB.
	reg := NewRegistry()
	RegisterWowGuildBankSummaryTool(reg, DBDeps{QueryDB: db})
	tool, _ := reg.Get("wow_guild_bank_summary")
	resp := tool.Handler(context.Background(), json.RawMessage(`{"sortBy":"; DROP TABLE guild"}`), "")
	got, _ := resp.(map[string]any)
	if msg, _ := got["error"].(string); !strings.Contains(msg, "invalid sortBy") {
		t.Errorf("expected invalid-sortBy error, got %v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("a query was issued on a bad sortBy: %v", err)
	}
}

// TestWowGuildBankSummary_TopNClamps asserts the post-clamp topN echo (unset->default,
// zero/negative->default, oversized->max, in-range passthrough) and that the LIMIT bind
// matches the clamped value. minBankGold=0 so there is no matched-count round trip and the
// list binds only (minCopper=0, LIMIT). Default sortBy resolves to the bankMoney clause.
func TestWowGuildBankSummary_TopNClamps(t *testing.T) {
	cases := []struct {
		name     string
		args     string
		wantTopN int
	}{
		{"unset uses default", `{}`, wowGuildBankSummaryDefaultTop},
		{"zero falls back to default", `{"topN":0}`, wowGuildBankSummaryDefaultTop},
		{"negative falls back to default", `{"topN":-7}`, wowGuildBankSummaryDefaultTop},
		{"oversized clamps to max", `{"topN":99999}`, wowGuildBankSummaryMaxTop},
		{"in-range passes through", `{"topN":42}`, 42},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			expectGuildBankPreamble(mock, 0, 0, 0, 0)
			mock.ExpectQuery(regexp.QuoteMeta("ORDER BY g.BankMoney DESC, g.guildid ASC LIMIT ?")).
				WithArgs(int64(0), int64(c.wantTopN)).
				WillReturnRows(sqlmock.NewRows(guildBankSummaryListCols()))

			reg := NewRegistry()
			RegisterWowGuildBankSummaryTool(reg, DBDeps{QueryDB: db})
			tool, _ := reg.Get("wow_guild_bank_summary")
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

// TestWowGuildBankSummary_SortByMapsToClause proves each allowlisted sortBy resolves to
// its ORDER BY clause verbatim (the clause is interpolated, so a wrong mapping is a silent
// injection/behaviour bug). Drives the handler once per key and matches the emitted SQL.
func TestWowGuildBankSummary_SortByMapsToClause(t *testing.T) {
	cases := []struct {
		sortBy string
		clause string
	}{
		{"bankMoney", "ORDER BY g.BankMoney DESC, g.guildid ASC LIMIT ?"},
		{"tabs", "ORDER BY tabCount DESC, g.BankMoney DESC, g.guildid ASC LIMIT ?"},
		{"name", "ORDER BY g.name ASC, g.guildid ASC LIMIT ?"},
		{"guildId", "ORDER BY g.guildid ASC LIMIT ?"},
		{"createDate", "ORDER BY g.createdate ASC, g.guildid ASC LIMIT ?"},
	}
	for _, c := range cases {
		t.Run(c.sortBy, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			expectGuildBankPreamble(mock, 0, 0, 0, 0)
			mock.ExpectQuery(regexp.QuoteMeta(c.clause)).
				WithArgs(int64(0), int64(wowGuildBankSummaryDefaultTop)).
				WillReturnRows(sqlmock.NewRows(guildBankSummaryListCols()))

			reg := NewRegistry()
			RegisterWowGuildBankSummaryTool(reg, DBDeps{QueryDB: db})
			tool, _ := reg.Get("wow_guild_bank_summary")
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

// TestCollectWowGuildBankSummary_Golden drives the full per-row fold: ORDER-BY-trust (rows
// echoed in DB order, no Go re-sort), the copper->"Xg Ys Zc" money render, the tabNames
// 0x1f split back to tabCount labels, the NULL-tabNames -> nil (bank-less guild) case,
// createDateISO set only for a nonzero createdate, the honest realm-wide totals, and
// totalGuilds (3) exceeding the returned list (2) -> truncated true.
func TestCollectWowGuildBankSummary_Golden(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	now := time.Unix(1_700_000_000, 0)
	// 1_234_567c = 123g 45s 67c — the honest realm total.
	expectGuildBankPreamble(mock, 3, 2, 1_234_567, 5)
	mock.ExpectQuery(regexp.QuoteMeta("ORDER BY g.BankMoney DESC, g.guildid ASC LIMIT ?")).
		WithArgs(int64(0), int64(2)).
		WillReturnRows(sqlmock.NewRows(guildBankSummaryListCols()).
			// richest: 1_000_000c = 100g, 3 named tabs, real createdate -> ISO set.
			AddRow(int64(10), "Rich Legion", int64(500), int64(1_000_000), int64(1_600_000_000), 3, "Bank\x1fConsumables\x1fEpics").
			// bank-less: 234_567c = 23g 45s 67c, NO tabs (tabNames NULL), createdate 0 -> no ISO.
			AddRow(int64(20), "Poor Crew", int64(0), int64(234_567), int64(0), 0, nil))

	out, err := collectWowGuildBankSummary(context.Background(), db, now, 2, "bankMoney", "g.BankMoney DESC, g.guildid ASC", 0)
	if err != nil {
		t.Fatalf("collectWowGuildBankSummary: %v", err)
	}

	if tg, _ := out["totalGuilds"].(int64); tg != 3 {
		t.Errorf("totalGuilds: %v want 3", out["totalGuilds"])
	}
	if gwb, _ := out["guildsWithBank"].(int64); gwb != 2 {
		t.Errorf("guildsWithBank: %v want 2", out["guildsWithBank"])
	}
	if tc, _ := out["totalBankMoneyCopper"].(int64); tc != 1_234_567 {
		t.Errorf("totalBankMoneyCopper: %v want 1234567", out["totalBankMoneyCopper"])
	}
	if tbm, _ := out["totalBankMoney"].(string); tbm != "123g 45s 67c" {
		t.Errorf("totalBankMoney: %q want 123g 45s 67c", out["totalBankMoney"])
	}
	if tt, _ := out["totalTabs"].(int64); tt != 5 {
		t.Errorf("totalTabs: %v want 5", out["totalTabs"])
	}
	// no filter -> matchedGuilds == totalGuilds.
	if mg, _ := out["matchedGuilds"].(int64); mg != 3 {
		t.Errorf("matchedGuilds: %v want 3", out["matchedGuilds"])
	}
	if r, _ := out["returned"].(int); r != 2 {
		t.Errorf("returned: %v want 2", out["returned"])
	}
	if tr, _ := out["truncated"].(bool); !tr {
		t.Errorf("truncated: %v want true (totalGuilds 3 > returned 2)", out["truncated"])
	}

	guilds, ok := out["guilds"].([]*guildBankSummaryRow)
	if !ok {
		t.Fatalf("guilds type: %T want []*guildBankSummaryRow", out["guilds"])
	}
	if len(guilds) != 2 {
		t.Fatalf("guilds len: %d want 2", len(guilds))
	}

	// guilds[0]: richest, 3 named tabs, ISO set.
	g0 := guilds[0]
	if g0.GuildID != 10 || g0.Name != "Rich Legion" || g0.LeaderGuid != 500 {
		t.Errorf("g0: %+v want id=10 Rich Legion leader=500", g0)
	}
	if g0.BankMoneyCopper != 1_000_000 || g0.BankMoney != "100g 0s 0c" {
		t.Errorf("g0 money: %d / %q want 1000000 / 100g 0s 0c", g0.BankMoneyCopper, g0.BankMoney)
	}
	if g0.TabCount != 3 || len(g0.TabNames) != 3 {
		t.Errorf("g0 tabs: count=%d names=%v want 3 / 3 labels", g0.TabCount, g0.TabNames)
	}
	if len(g0.TabNames) == 3 && (g0.TabNames[0] != "Bank" || g0.TabNames[2] != "Epics") {
		t.Errorf("g0 tabNames: %v want [Bank Consumables Epics]", g0.TabNames)
	}
	if g0.CreateDate != 1_600_000_000 || g0.CreateDateISO == "" {
		t.Errorf("g0 createDate: %d / %q want set + ISO", g0.CreateDate, g0.CreateDateISO)
	}

	// guilds[1]: bank-less, no tabs (tabNames nil), createdate 0 -> no ISO.
	g1 := guilds[1]
	if g1.GuildID != 20 || g1.BankMoney != "23g 45s 67c" {
		t.Errorf("g1: %+v want id=20 money=23g 45s 67c", g1)
	}
	if g1.TabCount != 0 || g1.TabNames != nil {
		t.Errorf("g1 tabs: count=%d names=%v want 0 / nil (NULL GROUP_CONCAT)", g1.TabCount, g1.TabNames)
	}
	if g1.CreateDate != 0 || g1.CreateDateISO != "" {
		t.Errorf("g1 createDate: %d / %q want 0 / '' (createdate 0)", g1.CreateDate, g1.CreateDateISO)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestCollectWowGuildBankSummary_MinBankGoldFilter pins the gold->copper threshold math:
// minBankGold=50 binds 500000 copper into BOTH the matched-count and the list WHERE, the
// matched-count round trip fires only when filtered, matchedGuilds reflects the filter (1)
// while the realm totals stay honest, and returned==matched -> truncated false.
func TestCollectWowGuildBankSummary_MinBankGoldFilter(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	now := time.Unix(1_700_000_000, 0)
	minCopper := int64(50) * copperPerGold // 500000

	expectGuildBankPreamble(mock, 3, 2, 1_234_567, 5)
	mock.ExpectQuery(regexp.QuoteMeta("SELECT COUNT(*) FROM `guild` WHERE BankMoney >= ?")).
		WithArgs(minCopper).
		WillReturnRows(sqlmock.NewRows([]string{"cnt"}).AddRow(int64(1)))
	mock.ExpectQuery(regexp.QuoteMeta("WHERE g.BankMoney >= ? GROUP BY g.guildid, g.name, g.leaderguid, g.BankMoney, g.createdate ORDER BY g.BankMoney DESC, g.guildid ASC LIMIT ?")).
		WithArgs(minCopper, int64(wowGuildBankSummaryDefaultTop)).
		WillReturnRows(sqlmock.NewRows(guildBankSummaryListCols()).
			AddRow(int64(10), "Rich Legion", int64(500), int64(1_000_000), int64(1_600_000_000), 2, "Bank\x1fVault"))

	out, err := collectWowGuildBankSummary(context.Background(), db, now, wowGuildBankSummaryDefaultTop, "bankMoney", "g.BankMoney DESC, g.guildid ASC", 50)
	if err != nil {
		t.Fatalf("collectWowGuildBankSummary: %v", err)
	}
	if mbg, _ := out["minBankGold"].(int64); mbg != 50 {
		t.Errorf("minBankGold echo: %v want 50", out["minBankGold"])
	}
	// realm totals stay honest (unfiltered) even under a filter.
	if tg, _ := out["totalGuilds"].(int64); tg != 3 {
		t.Errorf("totalGuilds: %v want 3 (unfiltered)", out["totalGuilds"])
	}
	if mg, _ := out["matchedGuilds"].(int64); mg != 1 {
		t.Errorf("matchedGuilds: %v want 1 (filtered)", out["matchedGuilds"])
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

// TestCollectWowGuildBankSummary_Empty — no guilds on the realm. guilds must be a non-nil
// empty slice (callers expect arrays), totals 0, totalBankMoney "0c", returned 0,
// matchedGuilds 0, truncated false. No matched-count round trip (minBankGold=0).
func TestCollectWowGuildBankSummary_Empty(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	now := time.Unix(1_700_000_000, 0)
	expectGuildBankPreamble(mock, 0, 0, 0, 0)
	mock.ExpectQuery(regexp.QuoteMeta("ORDER BY g.BankMoney DESC, g.guildid ASC LIMIT ?")).
		WithArgs(int64(0), int64(wowGuildBankSummaryDefaultTop)).
		WillReturnRows(sqlmock.NewRows(guildBankSummaryListCols()))

	out, err := collectWowGuildBankSummary(context.Background(), db, now, wowGuildBankSummaryDefaultTop, "bankMoney", "g.BankMoney DESC, g.guildid ASC", 0)
	if err != nil {
		t.Fatalf("collectWowGuildBankSummary: %v", err)
	}
	guilds, ok := out["guilds"].([]*guildBankSummaryRow)
	if !ok {
		t.Fatalf("guilds type: %T want []*guildBankSummaryRow", out["guilds"])
	}
	if len(guilds) != 0 {
		t.Errorf("guilds len: %d want 0", len(guilds))
	}
	if tbm, _ := out["totalBankMoney"].(string); tbm != "0c" {
		t.Errorf("totalBankMoney: %q want 0c", out["totalBankMoney"])
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
