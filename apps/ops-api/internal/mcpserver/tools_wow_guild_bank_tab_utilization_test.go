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

// tabUtilListCols is the column set the per-tab list query scans, in order.
func tabUtilListCols() []string {
	return []string{"guildid", "name", "TabId", "TabName", "filled"}
}

// tabUtilAggregateSQL is the distinctive head of the honest realm-wide occupancy aggregate
// (the derived-table SUM over every purchased tab). Matched as a substring by sqlmock.
const tabUtilAggregateSQL = "SELECT COUNT(*), COALESCE(SUM(filled), 0), COALESCE(SUM(filled = 0), 0), " +
	"COALESCE(SUM(filled >= 98), 0), COUNT(DISTINCT guildid) FROM ("

// expectTabUtilPreamble stages the fixed head-of-collect queries every fire runs: the USE
// and the realm-wide occupancy aggregate. The optional matched-count and the list are
// staged per-test after this.
func expectTabUtilPreamble(mock sqlmock.Sqlmock, totalTabs, totalFilled, emptyTabs, fullTabs, guildsWithTabs int64) {
	mock.ExpectExec(regexp.QuoteMeta("USE `acore_characters`")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta(tabUtilAggregateSQL)).
		WillReturnRows(sqlmock.NewRows([]string{"tabs", "filled", "empty", "full", "guilds"}).
			AddRow(totalTabs, totalFilled, emptyTabs, fullTabs, guildsWithTabs))
}

// TestWowGuildBankTabUtilization_DescriptionMentionsContext guards the description keywords
// — they steer the agent here when an operator asks "which guild-bank tabs are near-full /
// sitting empty?". Drop the table framing or the sibling cross-reference and the agent
// falls back to a hand-written db_query.
func TestWowGuildBankTabUtilization_DescriptionMentionsContext(t *testing.T) {
	reg := NewRegistry()
	RegisterWowGuildBankTabUtilizationTool(reg, DBDeps{})
	tool, ok := reg.Get("wow_guild_bank_tab_utilization")
	if !ok {
		t.Fatal("wow_guild_bank_tab_utilization not registered")
	}
	for _, kw := range []string{
		"acore_characters", "guild_bank_tab", "guild_bank_item", "ops_ro", "wow_guild_bank_summary",
		"GUILD_BANK_MAX_SLOTS", "guildId", "guildName", "tabId", "tabName", "filled", "capacity",
		"freeSlots", "fillPercent", "empty", "full", "totalTabs", "guildsWithTabs", "totalFilledSlots",
		"overallFillPercent", "emptyTabs", "fullTabs", "matchedTabs", "minFillPercent", "sortBy",
		"topN", "Read-only",
	} {
		if !strings.Contains(tool.Description, kw) {
			t.Errorf("description missing %q: %s", kw, tool.Description)
		}
	}
}

// TestWowGuildBankTabUtilization_ReadOnlyAnnotation pins readOnlyHint=true so a future
// copy-paste of a destructive sibling can't silently flip the gate.
func TestWowGuildBankTabUtilization_ReadOnlyAnnotation(t *testing.T) {
	reg := NewRegistry()
	RegisterWowGuildBankTabUtilizationTool(reg, DBDeps{})
	tool, _ := reg.Get("wow_guild_bank_tab_utilization")
	if !strings.Contains(string(tool.Annotations), `"readOnlyHint":true`) {
		t.Errorf("annotations missing readOnlyHint=true: %s", tool.Annotations)
	}
}

func TestWowGuildBankTabUtilization_NoPoolReturnsError(t *testing.T) {
	reg := NewRegistry()
	RegisterWowGuildBankTabUtilizationTool(reg, DBDeps{}) // QueryDB nil
	tool, _ := reg.Get("wow_guild_bank_tab_utilization")
	resp := tool.Handler(context.Background(), json.RawMessage(`{}`), "")
	got, _ := resp.(map[string]any)
	if msg, _ := got["error"].(string); !strings.Contains(msg, "ops_ro pool not configured") {
		t.Errorf("expected pool-not-configured error, got %v", got)
	}
}

// TestWowGuildBankTabUtilization_InvalidSortBy — an unknown sortBy is rejected BEFORE any
// query (a typo, not a silent default), and only allowlisted values can reach the ORDER BY.
func TestWowGuildBankTabUtilization_InvalidSortBy(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// No ExpectExec / ExpectQuery — the handler must bail before touching the DB.
	reg := NewRegistry()
	RegisterWowGuildBankTabUtilizationTool(reg, DBDeps{QueryDB: db})
	tool, _ := reg.Get("wow_guild_bank_tab_utilization")
	resp := tool.Handler(context.Background(), json.RawMessage(`{"sortBy":"; DROP TABLE guild_bank_tab"}`), "")
	got, _ := resp.(map[string]any)
	if msg, _ := got["error"].(string); !strings.Contains(msg, "invalid sortBy") {
		t.Errorf("expected invalid-sortBy error, got %v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("a query was issued on a bad sortBy: %v", err)
	}
}

// TestWowGuildBankTabUtilization_TopNClamps asserts the post-clamp topN echo (unset->default,
// zero/negative->default, oversized->max, in-range passthrough) and that the LIMIT bind
// matches the clamped value. No filter (guildId 0, minFillPercent 0) so there is no
// matched-count round trip and the list binds (0, 0, minFilled 0, LIMIT). Default sortBy
// resolves to the fillPercent (== filled) clause.
func TestWowGuildBankTabUtilization_TopNClamps(t *testing.T) {
	cases := []struct {
		name     string
		args     string
		wantTopN int
	}{
		{"unset uses default", `{}`, wowGuildBankTabUtilDefaultTop},
		{"zero falls back to default", `{"topN":0}`, wowGuildBankTabUtilDefaultTop},
		{"negative falls back to default", `{"topN":-7}`, wowGuildBankTabUtilDefaultTop},
		{"oversized clamps to max", `{"topN":99999}`, wowGuildBankTabUtilMaxTop},
		{"in-range passes through", `{"topN":42}`, 42},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			expectTabUtilPreamble(mock, 0, 0, 0, 0, 0)
			mock.ExpectQuery(regexp.QuoteMeta("ORDER BY filled DESC, gbt.guildid ASC, gbt.TabId ASC LIMIT ?")).
				WithArgs(int64(0), int64(0), int64(0), int64(c.wantTopN)).
				WillReturnRows(sqlmock.NewRows(tabUtilListCols()))

			reg := NewRegistry()
			RegisterWowGuildBankTabUtilizationTool(reg, DBDeps{QueryDB: db})
			tool, _ := reg.Get("wow_guild_bank_tab_utilization")
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

// TestWowGuildBankTabUtilization_SortByMapsToClause proves each allowlisted sortBy resolves
// to its ORDER BY clause verbatim (the clause is interpolated, so a wrong mapping is a
// silent injection/behaviour bug). fillPercent and filled intentionally share a clause.
func TestWowGuildBankTabUtilization_SortByMapsToClause(t *testing.T) {
	cases := []struct {
		sortBy string
		clause string
	}{
		{"fillPercent", "ORDER BY filled DESC, gbt.guildid ASC, gbt.TabId ASC LIMIT ?"},
		{"filled", "ORDER BY filled DESC, gbt.guildid ASC, gbt.TabId ASC LIMIT ?"},
		{"guild", "ORDER BY g.name ASC, gbt.guildid ASC, gbt.TabId ASC LIMIT ?"},
	}
	for _, c := range cases {
		t.Run(c.sortBy, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			expectTabUtilPreamble(mock, 0, 0, 0, 0, 0)
			mock.ExpectQuery(regexp.QuoteMeta(c.clause)).
				WithArgs(int64(0), int64(0), int64(0), int64(wowGuildBankTabUtilDefaultTop)).
				WillReturnRows(sqlmock.NewRows(tabUtilListCols()))

			reg := NewRegistry()
			RegisterWowGuildBankTabUtilizationTool(reg, DBDeps{QueryDB: db})
			tool, _ := reg.Get("wow_guild_bank_tab_utilization")
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

// TestCollectWowGuildBankTabUtilization_Golden drives the full per-row fold: ORDER-BY-trust
// (rows echoed in DB order, no Go re-sort), the filled/capacity/freeSlots/fillPercent math,
// the empty (filled 0) and full (filled 98) flags, the orphan-tab guildName "" (LEFT JOIN
// NULL name), the empty tabName -> omitted case, the honest realm-wide totals, and
// totalTabs (5, pre-limit) exceeding the returned list (3) -> truncated true.
func TestCollectWowGuildBankTabUtilization_Golden(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	now := time.Unix(1_700_000_000, 0)
	// realm: 5 tabs, 240 filled slots, 1 empty tab, 1 full tab, 2 guilds with tabs.
	expectTabUtilPreamble(mock, 5, 240, 1, 1, 2)
	mock.ExpectQuery(regexp.QuoteMeta("ORDER BY filled DESC, gbt.guildid ASC, gbt.TabId ASC LIMIT ?")).
		WithArgs(int64(0), int64(0), int64(0), int64(3)).
		WillReturnRows(sqlmock.NewRows(tabUtilListCols()).
			// full: 98/98, named, real guild.
			AddRow(int64(10), "Rich Legion", 0, "Bank", 98).
			// half: 49/98 -> 50.0%, named, real guild.
			AddRow(int64(10), "Rich Legion", 1, "Herbs", 49).
			// empty orphan: 0/98, guild row missing (name NULL -> ""), tab name blank -> omitted.
			AddRow(int64(30), nil, 0, "", 0))

	out, err := collectWowGuildBankTabUtilization(context.Background(), db, now, 3, "fillPercent", "filled DESC, gbt.guildid ASC, gbt.TabId ASC", 0, 0)
	if err != nil {
		t.Fatalf("collectWowGuildBankTabUtilization: %v", err)
	}

	// ---- honest realm-wide totals ----
	if tt, _ := out["totalTabs"].(int64); tt != 5 {
		t.Errorf("totalTabs: %v want 5", out["totalTabs"])
	}
	if gwt, _ := out["guildsWithTabs"].(int64); gwt != 2 {
		t.Errorf("guildsWithTabs: %v want 2", out["guildsWithTabs"])
	}
	if tf, _ := out["totalFilledSlots"].(int64); tf != 240 {
		t.Errorf("totalFilledSlots: %v want 240", out["totalFilledSlots"])
	}
	if tc, _ := out["totalCapacity"].(int64); tc != 490 { // 5 * 98
		t.Errorf("totalCapacity: %v want 490", out["totalCapacity"])
	}
	if tfr, _ := out["totalFreeSlots"].(int64); tfr != 250 { // 490 - 240
		t.Errorf("totalFreeSlots: %v want 250", out["totalFreeSlots"])
	}
	if ofp, _ := out["overallFillPercent"].(float64); math.Abs(ofp-49.0) > 0.05 { // 240/490 -> 49.0
		t.Errorf("overallFillPercent: %v want ~49.0", out["overallFillPercent"])
	}
	if et, _ := out["emptyTabs"].(int64); et != 1 {
		t.Errorf("emptyTabs: %v want 1", out["emptyTabs"])
	}
	if ft, _ := out["fullTabs"].(int64); ft != 1 {
		t.Errorf("fullTabs: %v want 1", out["fullTabs"])
	}
	if sp, _ := out["slotsPerTab"].(int); sp != 98 {
		t.Errorf("slotsPerTab: %v want 98", out["slotsPerTab"])
	}
	// no filter -> matchedTabs == totalTabs, and returned (3) < matched (5) -> truncated.
	if mt, _ := out["matchedTabs"].(int64); mt != 5 {
		t.Errorf("matchedTabs: %v want 5", out["matchedTabs"])
	}
	if r, _ := out["returned"].(int); r != 3 {
		t.Errorf("returned: %v want 3", out["returned"])
	}
	if tr, _ := out["truncated"].(bool); !tr {
		t.Errorf("truncated: %v want true (matchedTabs 5 > returned 3)", out["truncated"])
	}

	tabs, ok := out["tabs"].([]*guildBankTabUtilRow)
	if !ok {
		t.Fatalf("tabs type: %T want []*guildBankTabUtilRow", out["tabs"])
	}
	if len(tabs) != 3 {
		t.Fatalf("tabs len: %d want 3", len(tabs))
	}

	// tabs[0]: full tab 98/98.
	t0 := tabs[0]
	if t0.GuildID != 10 || t0.GuildName != "Rich Legion" || t0.TabID != 0 || t0.TabName != "Bank" {
		t.Errorf("t0 ids: %+v want guild 10 Rich Legion tab 0 Bank", t0)
	}
	if t0.Filled != 98 || t0.Capacity != 98 || t0.FreeSlots != 0 || math.Abs(t0.FillPercent-100.0) > 0.05 {
		t.Errorf("t0 occupancy: filled=%d cap=%d free=%d pct=%v want 98/98/0/100.0", t0.Filled, t0.Capacity, t0.FreeSlots, t0.FillPercent)
	}
	if t0.Empty || !t0.Full {
		t.Errorf("t0 flags: empty=%v full=%v want false/true", t0.Empty, t0.Full)
	}

	// tabs[1]: half-full 49/98 -> 50.0%.
	t1 := tabs[1]
	if t1.Filled != 49 || t1.FreeSlots != 49 || math.Abs(t1.FillPercent-50.0) > 0.05 {
		t.Errorf("t1 occupancy: filled=%d free=%d pct=%v want 49/49/50.0", t1.Filled, t1.FreeSlots, t1.FillPercent)
	}
	if t1.Empty || t1.Full {
		t.Errorf("t1 flags: empty=%v full=%v want false/false", t1.Empty, t1.Full)
	}

	// tabs[2]: empty orphan 0/98, guildName "" (NULL name), tabName "" (blank).
	t2 := tabs[2]
	if t2.GuildID != 30 || t2.GuildName != "" || t2.TabName != "" {
		t.Errorf("t2 ids: %+v want guild 30 name '' tabName ''", t2)
	}
	if t2.Filled != 0 || t2.FreeSlots != 98 || math.Abs(t2.FillPercent-0.0) > 0.05 {
		t.Errorf("t2 occupancy: filled=%d free=%d pct=%v want 0/98/0.0", t2.Filled, t2.FreeSlots, t2.FillPercent)
	}
	if !t2.Empty || t2.Full {
		t.Errorf("t2 flags: empty=%v full=%v want true/false", t2.Empty, t2.Full)
	}

	// guildName "" must serialize away (omitempty) for the orphan row.
	blob, _ := json.Marshal(t2)
	if strings.Contains(string(blob), "guildName") || strings.Contains(string(blob), "tabName") {
		t.Errorf("orphan row JSON should omit empty guildName/tabName: %s", blob)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestCollectWowGuildBankTabUtilization_Filter pins the guildId drilldown + minFillPercent
// floor: minFillPercent 50 becomes the integer slot floor minFilled 49 (ceil(50% of 98)),
// which binds into BOTH the matched-count and the list; the matched-count round trip fires
// only when filtered; the guildid sentinel binds twice; matchedTabs reflects the filter (1)
// while the realm totals stay honest (5); and returned == matched -> truncated false.
func TestCollectWowGuildBankTabUtilization_Filter(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	now := time.Unix(1_700_000_000, 0)
	expectTabUtilPreamble(mock, 5, 240, 1, 1, 2)
	// matched-count: guildId 10 bound twice, minFilled 49.
	mock.ExpectQuery(regexp.QuoteMeta("SELECT COUNT(*) FROM (SELECT gbt.guildid AS guildid, COUNT(gbi.SlotId) AS filled FROM `guild_bank_tab` gbt LEFT JOIN `guild_bank_item`")).
		WithArgs(int64(10), int64(10), int64(49)).
		WillReturnRows(sqlmock.NewRows([]string{"cnt"}).AddRow(int64(1)))
	// list: same binds + LIMIT.
	mock.ExpectQuery(regexp.QuoteMeta("HAVING COUNT(gbi.SlotId) >= ? ORDER BY filled DESC, gbt.guildid ASC, gbt.TabId ASC LIMIT ?")).
		WithArgs(int64(10), int64(10), int64(49), int64(wowGuildBankTabUtilDefaultTop)).
		WillReturnRows(sqlmock.NewRows(tabUtilListCols()).
			AddRow(int64(10), "Rich Legion", 0, "Bank", 98))

	out, err := collectWowGuildBankTabUtilization(context.Background(), db, now, wowGuildBankTabUtilDefaultTop, "fillPercent", "filled DESC, gbt.guildid ASC, gbt.TabId ASC", 10, 50)
	if err != nil {
		t.Fatalf("collectWowGuildBankTabUtilization: %v", err)
	}
	if gid, _ := out["guildId"].(int64); gid != 10 {
		t.Errorf("guildId echo: %v want 10", out["guildId"])
	}
	if mfp, _ := out["minFillPercent"].(int); mfp != 50 {
		t.Errorf("minFillPercent echo: %v want 50", out["minFillPercent"])
	}
	// realm totals stay honest (unfiltered) even under a filter.
	if tt, _ := out["totalTabs"].(int64); tt != 5 {
		t.Errorf("totalTabs: %v want 5 (unfiltered)", out["totalTabs"])
	}
	if mt, _ := out["matchedTabs"].(int64); mt != 1 {
		t.Errorf("matchedTabs: %v want 1 (filtered)", out["matchedTabs"])
	}
	if r, _ := out["returned"].(int); r != 1 {
		t.Errorf("returned: %v want 1", out["returned"])
	}
	if tr, _ := out["truncated"].(bool); tr {
		t.Errorf("truncated: %v want false (matchedTabs 1 == returned 1)", out["truncated"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestWowGuildBankTabUtilization_MinFillPercentClamps proves the handler clamps the percent
// floor to [0,100] and that the clamp drives BOTH the echo and the filtered/unfiltered
// path: a negative floor clamps to 0 (unfiltered -> no matched-count round trip, minFilled
// 0), and an over-100 floor clamps to 100 (filtered -> matched-count round trip, minFilled
// 98 = ceil(100% of 98)).
func TestWowGuildBankTabUtilization_MinFillPercentClamps(t *testing.T) {
	t.Run("negative clamps to 0 (unfiltered)", func(t *testing.T) {
		db, mock, err := sqlmock.New()
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		expectTabUtilPreamble(mock, 2, 0, 2, 0, 1)
		// No matched-count round trip; list binds minFilled 0.
		mock.ExpectQuery(regexp.QuoteMeta("ORDER BY filled DESC, gbt.guildid ASC, gbt.TabId ASC LIMIT ?")).
			WithArgs(int64(0), int64(0), int64(0), int64(wowGuildBankTabUtilDefaultTop)).
			WillReturnRows(sqlmock.NewRows(tabUtilListCols()))

		reg := NewRegistry()
		RegisterWowGuildBankTabUtilizationTool(reg, DBDeps{QueryDB: db})
		tool, _ := reg.Get("wow_guild_bank_tab_utilization")
		resp := tool.Handler(context.Background(), json.RawMessage(`{"minFillPercent":-5}`), "")
		got, _ := resp.(map[string]any)
		if got["error"] != nil {
			t.Fatalf("unexpected error: %v", got)
		}
		if mfp, _ := got["minFillPercent"].(int); mfp != 0 {
			t.Errorf("minFillPercent echo: %v want 0", got["minFillPercent"])
		}
		if mt, _ := got["matchedTabs"].(int64); mt != 2 { // == totalTabs, no round trip
			t.Errorf("matchedTabs: %v want 2 (unfiltered)", got["matchedTabs"])
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("unmet mock expectations: %v", err)
		}
	})

	t.Run("over-100 clamps to 100 (filtered, minFilled 98)", func(t *testing.T) {
		db, mock, err := sqlmock.New()
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		expectTabUtilPreamble(mock, 5, 240, 1, 1, 2)
		mock.ExpectQuery(regexp.QuoteMeta("SELECT COUNT(*) FROM (SELECT gbt.guildid AS guildid, COUNT(gbi.SlotId) AS filled FROM `guild_bank_tab` gbt LEFT JOIN `guild_bank_item`")).
			WithArgs(int64(0), int64(0), int64(98)).
			WillReturnRows(sqlmock.NewRows([]string{"cnt"}).AddRow(int64(1)))
		mock.ExpectQuery(regexp.QuoteMeta("HAVING COUNT(gbi.SlotId) >= ? ORDER BY filled DESC, gbt.guildid ASC, gbt.TabId ASC LIMIT ?")).
			WithArgs(int64(0), int64(0), int64(98), int64(wowGuildBankTabUtilDefaultTop)).
			WillReturnRows(sqlmock.NewRows(tabUtilListCols()).
				AddRow(int64(10), "Rich Legion", 0, "Bank", 98))

		reg := NewRegistry()
		RegisterWowGuildBankTabUtilizationTool(reg, DBDeps{QueryDB: db})
		tool, _ := reg.Get("wow_guild_bank_tab_utilization")
		resp := tool.Handler(context.Background(), json.RawMessage(`{"minFillPercent":150}`), "")
		got, _ := resp.(map[string]any)
		if got["error"] != nil {
			t.Fatalf("unexpected error: %v", got)
		}
		if mfp, _ := got["minFillPercent"].(int); mfp != 100 {
			t.Errorf("minFillPercent echo: %v want 100", got["minFillPercent"])
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("unmet mock expectations: %v", err)
		}
	})
}

// TestCollectWowGuildBankTabUtilization_Empty — no tabs on the realm. tabs must be a non-nil
// empty slice (callers expect arrays), totals 0, overallFillPercent 0 (no div-by-zero on
// totalCapacity 0), returned 0, matchedTabs 0, truncated false. No matched-count round trip
// (unfiltered).
func TestCollectWowGuildBankTabUtilization_Empty(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	now := time.Unix(1_700_000_000, 0)
	expectTabUtilPreamble(mock, 0, 0, 0, 0, 0)
	mock.ExpectQuery(regexp.QuoteMeta("ORDER BY filled DESC, gbt.guildid ASC, gbt.TabId ASC LIMIT ?")).
		WithArgs(int64(0), int64(0), int64(0), int64(wowGuildBankTabUtilDefaultTop)).
		WillReturnRows(sqlmock.NewRows(tabUtilListCols()))

	out, err := collectWowGuildBankTabUtilization(context.Background(), db, now, wowGuildBankTabUtilDefaultTop, "fillPercent", "filled DESC, gbt.guildid ASC, gbt.TabId ASC", 0, 0)
	if err != nil {
		t.Fatalf("collectWowGuildBankTabUtilization: %v", err)
	}
	tabs, ok := out["tabs"].([]*guildBankTabUtilRow)
	if !ok {
		t.Fatalf("tabs type: %T want []*guildBankTabUtilRow", out["tabs"])
	}
	if len(tabs) != 0 {
		t.Errorf("tabs len: %d want 0", len(tabs))
	}
	if tc, _ := out["totalCapacity"].(int64); tc != 0 {
		t.Errorf("totalCapacity: %v want 0", out["totalCapacity"])
	}
	if ofp, _ := out["overallFillPercent"].(float64); ofp != 0 {
		t.Errorf("overallFillPercent: %v want 0 (no div-by-zero)", out["overallFillPercent"])
	}
	if r, _ := out["returned"].(int); r != 0 {
		t.Errorf("returned: %v want 0", out["returned"])
	}
	if mt, _ := out["matchedTabs"].(int64); mt != 0 {
		t.Errorf("matchedTabs: %v want 0", out["matchedTabs"])
	}
	if tr, _ := out["truncated"].(bool); tr {
		t.Errorf("truncated: %v want false", out["truncated"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}
