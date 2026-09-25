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

// ahItemDemandCensusCols is the single census row's column set, in scan order.
func ahItemDemandCensusCols() []string {
	return []string{"distinctItems", "totalLeadingAuctions", "totalCommittedBid", "totalBuyoutValue"}
}

// ahItemDemandDisplayCols is the per-item display row column set, in scan order.
func ahItemDemandDisplayCols() []string {
	return []string{"entry", "name", "Quality", "class", "leadingAuctions", "distinctBidders", "totalCommittedBid", "totalBuyoutValue"}
}

// expectAhItemDemandCensus stages the USE + an unfiltered (zero-arg) one-row
// census reply so tests that only care about the display query still satisfy the
// always-first census call.
func expectAhItemDemandCensus(mock sqlmock.Sqlmock) {
	mock.ExpectExec(regexp.QuoteMeta("USE `acore_characters`")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta("COUNT(DISTINCT it.entry) AS distinctItems")).
		WithArgs().
		WillReturnRows(sqlmock.NewRows(ahItemDemandCensusCols()).
			AddRow(0, 0, int64(0), int64(0)))
}

// TestAhItemDemand_DescriptionMentionsContext guards the keywords that steer the
// agent here vs ah_market_top_items (supply) / ah_bidder_activity (bidder grain)
// / a hand-written db_query — the demand framing + bid vocabulary is load-bearing.
func TestAhItemDemand_DescriptionMentionsContext(t *testing.T) {
	reg := NewRegistry()
	RegisterAhItemDemandTool(reg, DBDeps{})
	tool, ok := reg.Get("ah_item_demand")
	if !ok {
		t.Fatal("ah_item_demand not registered")
	}
	for _, kw := range []string{
		"acore_characters.auctionhouse", "item_instance", "acore_world.item_template",
		"ah_market_top_items", "ah_bidder_activity", "ops_ro", "buyguid", "DEMAND",
		"qualityName", "Heirloom", "className", "leadingAuctions", "distinctBidders",
		"committed", "buyoutValue", "topN", "sortBy", "houseId", "minQuality",
		"Alliance", "Horde", "Neutral", "mod-ah-bot", "Read-only",
	} {
		if !strings.Contains(tool.Description, kw) {
			t.Errorf("description missing %q: %s", kw, tool.Description)
		}
	}
}

// TestAhItemDemand_ReadOnlyAnnotation pins readOnlyHint=true so a future
// copy-paste of a destructive sibling can't silently flip the gate.
func TestAhItemDemand_ReadOnlyAnnotation(t *testing.T) {
	reg := NewRegistry()
	RegisterAhItemDemandTool(reg, DBDeps{})
	tool, _ := reg.Get("ah_item_demand")
	if !strings.Contains(string(tool.Annotations), `"readOnlyHint":true`) {
		t.Errorf("annotations missing readOnlyHint=true: %s", tool.Annotations)
	}
}

func TestAhItemDemand_NoPoolReturnsError(t *testing.T) {
	reg := NewRegistry()
	RegisterAhItemDemandTool(reg, DBDeps{}) // QueryDB nil
	tool, _ := reg.Get("ah_item_demand")
	resp := tool.Handler(context.Background(), json.RawMessage(`{}`), "")
	got, _ := resp.(map[string]any)
	if msg, _ := got["error"].(string); !strings.Contains(msg, "ops_ro pool not configured") {
		t.Errorf("expected pool-not-configured error, got %v", got)
	}
}

// TestAhItemDemand_TopNClamps drives the handler and asserts the post-clamp topN
// echo (unset->default, zero/negative->default, oversized->max, in-range
// passthrough) and that the display LIMIT bind matches the clamped value.
func TestAhItemDemand_TopNClamps(t *testing.T) {
	cases := []struct {
		name     string
		args     string
		wantTopN int
	}{
		{"unset uses default", `{}`, ahItemDemandDefaultTop},
		{"zero falls back to default", `{"topN":0}`, ahItemDemandDefaultTop},
		{"negative falls back to default", `{"topN":-7}`, ahItemDemandDefaultTop},
		{"oversized clamps to max", `{"topN":99999}`, ahItemDemandMaxTop},
		{"in-range passes through", `{"topN":42}`, 42},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			expectAhItemDemandCensus(mock)
			mock.ExpectQuery(regexp.QuoteMeta("ORDER BY leadingAuctions DESC, it.entry ASC LIMIT ?")).
				WithArgs(int64(c.wantTopN)).
				WillReturnRows(sqlmock.NewRows(ahItemDemandDisplayCols()))

			reg := NewRegistry()
			RegisterAhItemDemandTool(reg, DBDeps{QueryDB: db})
			tool, _ := reg.Get("ah_item_demand")
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

// TestAhItemDemand_InvalidSortByRejected — an out-of-allowlist sortBy is rejected
// (no DB call) so a typo can't quietly change the ranking axis or, worse, reach
// the interpolated ORDER BY. QueryDB is non-nil to prove the reject fires AFTER
// the pool check but before any query.
func TestAhItemDemand_InvalidSortByRejected(t *testing.T) {
	for _, bad := range []string{`{"sortBy":"frobnicate"}`, `{"sortBy":"BIDS"}`, `{"sortBy":"price; DROP"}`} {
		db, mock, err := sqlmock.New()
		if err != nil {
			t.Fatal(err)
		}
		reg := NewRegistry()
		RegisterAhItemDemandTool(reg, DBDeps{QueryDB: db})
		tool, _ := reg.Get("ah_item_demand")
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

// TestAhItemDemand_MinQualityRejected — out-of-range minQuality is rejected (no DB
// call) so a typo can't masquerade as an empty market.
func TestAhItemDemand_MinQualityRejected(t *testing.T) {
	for _, bad := range []string{`{"minQuality":-1}`, `{"minQuality":8}`, `{"minQuality":99}`} {
		db, mock, err := sqlmock.New()
		if err != nil {
			t.Fatal(err)
		}
		reg := NewRegistry()
		RegisterAhItemDemandTool(reg, DBDeps{QueryDB: db})
		tool, _ := reg.Get("ah_item_demand")
		resp := tool.Handler(context.Background(), json.RawMessage(bad), "")
		got, _ := resp.(map[string]any)
		if msg, _ := got["error"].(string); !strings.Contains(msg, "minQuality out of range") {
			t.Errorf("args %s: expected out-of-range error, got %v", bad, got)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("args %s: no query should run on reject: %v", bad, err)
		}
		db.Close()
	}
}

// TestAhItemDemand_SortByClause asserts each allowlisted sortBy interpolates its
// aliased aggregate into the display ORDER BY (and that the response echoes the
// resolved key). The aliases MUST be declared in the SELECT or real MySQL rejects
// the ORDER BY — sqlmock only string-matches, so this pins the alias contract.
func TestAhItemDemand_SortByClause(t *testing.T) {
	cases := []struct {
		args       string
		wantSortBy string
		orderBy    string
	}{
		{`{}`, "bids", "ORDER BY leadingAuctions DESC, it.entry ASC LIMIT ?"},
		{`{"sortBy":"bids"}`, "bids", "ORDER BY leadingAuctions DESC, it.entry ASC LIMIT ?"},
		{`{"sortBy":"bidders"}`, "bidders", "ORDER BY distinctBidders DESC, it.entry ASC LIMIT ?"},
		{`{"sortBy":"committed"}`, "committed", "ORDER BY totalCommittedBid DESC, it.entry ASC LIMIT ?"},
		{`{"sortBy":"buyoutValue"}`, "buyoutValue", "ORDER BY totalBuyoutValue DESC, it.entry ASC LIMIT ?"},
	}
	for _, c := range cases {
		t.Run(c.wantSortBy, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			expectAhItemDemandCensus(mock)
			mock.ExpectQuery(regexp.QuoteMeta(c.orderBy)).
				WithArgs(int64(ahItemDemandDefaultTop)).
				WillReturnRows(sqlmock.NewRows(ahItemDemandDisplayCols()))

			reg := NewRegistry()
			RegisterAhItemDemandTool(reg, DBDeps{QueryDB: db})
			tool, _ := reg.Get("ah_item_demand")
			resp := tool.Handler(context.Background(), json.RawMessage(c.args), "")
			got, _ := resp.(map[string]any)
			if got["error"] != nil {
				t.Fatalf("unexpected error: %v", got)
			}
			if s, _ := got["sortBy"].(string); s != c.wantSortBy {
				t.Errorf("sortBy echo: %v want %q", got["sortBy"], c.wantSortBy)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Errorf("unmet mock expectations: %v", err)
			}
		})
	}
}

// TestAhItemDemand_Filters drives each filter combination and asserts BOTH the
// census and the display query carry the live-bid scope + the WHERE filters, with
// the right bound args (filters for the census; filters then LIMIT for display),
// and the echoed houseId/faction/minQuality keys.
func TestAhItemDemand_Filters(t *testing.T) {
	cases := []struct {
		name          string
		args          string
		censusWhere   string
		displayWhere  string
		wantCensusArg []driverArg
		wantDispArg   []driverArg
		wantHouse     int // -1 = key absent
		wantFaction   string
		wantMinQ      int // -1 = key absent
	}{
		{
			name:          "house only",
			args:          `{"houseId":7}`,
			censusWhere:   "WHERE ah.buyguid <> 0 AND ah.houseid = ?",
			displayWhere:  "WHERE ah.buyguid <> 0 AND ah.houseid = ? GROUP BY",
			wantCensusArg: []driverArg{i(7)},
			wantDispArg:   []driverArg{i(7), i(ahItemDemandDefaultTop)},
			wantHouse:     7, wantFaction: "Neutral", wantMinQ: -1,
		},
		{
			name:          "quality only",
			args:          `{"minQuality":3}`,
			censusWhere:   "WHERE ah.buyguid <> 0 AND it.Quality >= ?",
			displayWhere:  "WHERE ah.buyguid <> 0 AND it.Quality >= ? GROUP BY",
			wantCensusArg: []driverArg{i(3)},
			wantDispArg:   []driverArg{i(3), i(ahItemDemandDefaultTop)},
			wantHouse:     -1, wantMinQ: 3,
		},
		{
			name:          "both filters",
			args:          `{"houseId":2,"minQuality":4,"topN":5}`,
			censusWhere:   "WHERE ah.buyguid <> 0 AND ah.houseid = ? AND it.Quality >= ?",
			displayWhere:  "WHERE ah.buyguid <> 0 AND ah.houseid = ? AND it.Quality >= ? GROUP BY",
			wantCensusArg: []driverArg{i(2), i(4)},
			wantDispArg:   []driverArg{i(2), i(4), i(5)},
			wantHouse:     2, wantFaction: "Alliance", wantMinQ: 4,
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
			mock.ExpectQuery(regexp.QuoteMeta(c.censusWhere)).
				WithArgs(toValues(c.wantCensusArg)...).
				WillReturnRows(sqlmock.NewRows(ahItemDemandCensusCols()).AddRow(0, 0, int64(0), int64(0)))
			mock.ExpectQuery(regexp.QuoteMeta(c.displayWhere)).
				WithArgs(toValues(c.wantDispArg)...).
				WillReturnRows(sqlmock.NewRows(ahItemDemandDisplayCols()))

			reg := NewRegistry()
			RegisterAhItemDemandTool(reg, DBDeps{QueryDB: db})
			tool, _ := reg.Get("ah_item_demand")
			resp := tool.Handler(context.Background(), json.RawMessage(c.args), "")
			got, _ := resp.(map[string]any)
			if got["error"] != nil {
				t.Fatalf("unexpected error: %v", got)
			}
			if c.wantHouse >= 0 {
				if h, _ := got["houseId"].(int); h != c.wantHouse {
					t.Errorf("houseId: %v want %d", got["houseId"], c.wantHouse)
				}
				if f, _ := got["faction"].(string); f != c.wantFaction {
					t.Errorf("faction: %v want %q", got["faction"], c.wantFaction)
				}
			} else if _, ok := got["houseId"]; ok {
				t.Errorf("houseId should be absent, got %v", got["houseId"])
			}
			if c.wantMinQ >= 0 {
				if q, _ := got["minQuality"].(int); q != c.wantMinQ {
					t.Errorf("minQuality: %v want %d", got["minQuality"], c.wantMinQ)
				}
			} else if _, ok := got["minQuality"]; ok {
				t.Errorf("minQuality should be absent, got %v", got["minQuality"])
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Errorf("unmet mock expectations: %v", err)
			}
		})
	}
}

// TestCollectAhItemDemand_Golden folds the census single-row totals plus three
// grouped display rows and verifies qualityName/className mapping, the human gold
// strings (incl. a multi-denomination value and a zero -> "0c"), the honest realm
// totals sub-map, and that the handler trusts the SQL ORDER BY (no Go re-sort).
func TestCollectAhItemDemand_Golden(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	now := time.Unix(1_700_000_000, 0)
	mock.ExpectExec(regexp.QuoteMeta("USE `acore_characters`")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	// Census: 4 distinct items realm-wide (one beyond the 3 displayed), 28 leading
	// auctions, 560g committed, 830g buyout — independent of the display rows.
	mock.ExpectQuery(regexp.QuoteMeta("COUNT(DISTINCT it.entry) AS distinctItems")).
		WithArgs().
		WillReturnRows(sqlmock.NewRows(ahItemDemandCensusCols()).
			AddRow(4, 28, int64(5_600_000), int64(8_300_000)))
	mock.ExpectQuery(regexp.QuoteMeta("ORDER BY leadingAuctions DESC, it.entry ASC LIMIT ?")).
		WithArgs(int64(15)).
		WillReturnRows(sqlmock.NewRows(ahItemDemandDisplayCols()).
			// Epic weapon: 12 leading / 5 bidders, 500g committed, 800g buyout.
			AddRow(int64(49623), "Gladiator's Greatsword", 4, 2, 12, 5, int64(5_000_000), int64(8_000_000)).
			// Common trade good: 8 leading / 3 bidders, 15g committed, 25g buyout.
			AddRow(int64(33470), "Frostweave Cloth", 1, 7, 8, 3, int64(150_000), int64(250_000)).
			// Uncommon trade good: multi-denom committed 30g55s1c, zero buyout -> "0c".
			AddRow(int64(43102), "Frost Lotus", 2, 7, 5, 4, int64(305_501), int64(0)))

	out, err := collectAhItemDemand(context.Background(), db, now, 15, "bids", "leadingAuctions", nil, nil)
	if err != nil {
		t.Fatalf("collectAhItemDemand: %v", err)
	}
	if s, _ := out["sortBy"].(string); s != "bids" {
		t.Errorf("sortBy: %v want bids", out["sortBy"])
	}
	if _, ok := out["houseId"]; ok {
		t.Errorf("houseId should be absent with no filter, got %v", out["houseId"])
	}

	totals, ok := out["totals"].(map[string]any)
	if !ok {
		t.Fatalf("totals type: %T want map[string]any", out["totals"])
	}
	if di, _ := totals["distinctItems"].(int); di != 4 {
		t.Errorf("totals.distinctItems: %v want 4", totals["distinctItems"])
	}
	if la, _ := totals["totalLeadingAuctions"].(int); la != 28 {
		t.Errorf("totals.totalLeadingAuctions: %v want 28", totals["totalLeadingAuctions"])
	}
	if cb, _ := totals["totalCommittedBidCopper"].(int64); cb != 5_600_000 {
		t.Errorf("totals.totalCommittedBidCopper: %v want 5600000", totals["totalCommittedBidCopper"])
	}
	if g, _ := totals["totalCommittedBidGold"].(string); g != "560g" {
		t.Errorf("totals.totalCommittedBidGold: %v want 560g", totals["totalCommittedBidGold"])
	}
	if bv, _ := totals["totalBuyoutValueCopper"].(int64); bv != 8_300_000 {
		t.Errorf("totals.totalBuyoutValueCopper: %v want 8300000", totals["totalBuyoutValueCopper"])
	}
	if g, _ := totals["totalBuyoutValueGold"].(string); g != "830g" {
		t.Errorf("totals.totalBuyoutValueGold: %v want 830g", totals["totalBuyoutValueGold"])
	}

	items, ok := out["items"].([]*ahItemDemandItem)
	if !ok || len(items) != 3 {
		t.Fatalf("items: %T len %d want []*ahItemDemandItem len 3", out["items"], len(items))
	}

	it0 := items[0]
	if it0.ItemEntry != 49623 || it0.ItemName != "Gladiator's Greatsword" {
		t.Errorf("items[0] identity: %+v", it0)
	}
	if it0.Quality != 4 || it0.QualityName != "Epic" || it0.Class != 2 || it0.ClassName != "Weapon" {
		t.Errorf("items[0] enums: %+v", it0)
	}
	if it0.LeadingAuctions != 12 || it0.DistinctBidders != 5 {
		t.Errorf("items[0] counts: %+v", it0)
	}
	if it0.CommittedBidCopper != 5_000_000 || it0.CommittedBidGold != "500g" {
		t.Errorf("items[0] committed: %+v", it0)
	}
	if it0.BuyoutValueCopper != 8_000_000 || it0.BuyoutValueGold != "800g" {
		t.Errorf("items[0] buyout: %+v", it0)
	}

	it1 := items[1]
	if it1.QualityName != "Common" || it1.ClassName != "Trade Goods" {
		t.Errorf("items[1] enums: %+v", it1)
	}
	if it1.CommittedBidGold != "15g" || it1.BuyoutValueGold != "25g" {
		t.Errorf("items[1] gold: %+v", it1)
	}

	it2 := items[2]
	if it2.QualityName != "Uncommon" || it2.DistinctBidders != 4 {
		t.Errorf("items[2] identity: %+v", it2)
	}
	if it2.CommittedBidGold != "30g55s1c" {
		t.Errorf("items[2] multi-denom gold: %+v", it2)
	}
	if it2.BuyoutValueCopper != 0 || it2.BuyoutValueGold != "0c" {
		t.Errorf("items[2] zero buyout: %+v", it2)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestCollectAhItemDemand_Empty — an empty (no live-bid) market yields a non-nil
// empty items slice (callers expect an array) and zeroed honest totals with "0c"
// gold strings from the COALESCE guards.
func TestCollectAhItemDemand_Empty(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.ExpectExec(regexp.QuoteMeta("USE `acore_characters`")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta("COUNT(DISTINCT it.entry) AS distinctItems")).
		WithArgs().
		WillReturnRows(sqlmock.NewRows(ahItemDemandCensusCols()).AddRow(0, 0, int64(0), int64(0)))
	mock.ExpectQuery(regexp.QuoteMeta("ORDER BY leadingAuctions DESC, it.entry ASC LIMIT ?")).
		WithArgs(int64(15)).
		WillReturnRows(sqlmock.NewRows(ahItemDemandDisplayCols()))

	out, err := collectAhItemDemand(context.Background(), db, time.Unix(1_700_000_000, 0), 15, "bids", "leadingAuctions", nil, nil)
	if err != nil {
		t.Fatalf("collectAhItemDemand: %v", err)
	}
	items, ok := out["items"].([]*ahItemDemandItem)
	if !ok {
		t.Fatalf("items type: %T want []*ahItemDemandItem", out["items"])
	}
	if items == nil || len(items) != 0 {
		t.Errorf("items: %v want non-nil empty slice", items)
	}
	totals, _ := out["totals"].(map[string]any)
	if di, _ := totals["distinctItems"].(int); di != 0 {
		t.Errorf("totals.distinctItems: %v want 0", totals["distinctItems"])
	}
	if g, _ := totals["totalCommittedBidGold"].(string); g != "0c" {
		t.Errorf("totals.totalCommittedBidGold: %v want 0c", totals["totalCommittedBidGold"])
	}
	if g, _ := totals["totalBuyoutValueGold"].(string); g != "0c" {
		t.Errorf("totals.totalBuyoutValueGold: %v want 0c", totals["totalBuyoutValueGold"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}
