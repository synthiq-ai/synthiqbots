package mcpserver

import (
	"context"
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

// ahDepthFloorCols is the Stage-1 floor-map column set, in scan order. The third
// column is a per-UNIT floor scaled by ahDepthUnitScale, not a listing price, so
// every fixture below states floors as buyoutprice*10000/count.
func ahDepthFloorCols() []string {
	return []string{"houseid", "itemEntry", "unitFloorScaled"}
}

// ahDepthUnitFloor is the fixture-side helper for a scaled unit floor. It is
// deliberately the SAME expression the SQL floor map and the Go fold use, so a
// fixture can never disagree with the code about what a floor is.
func ahDepthUnitFloor(buyout, count int64) int64 {
	return buyout * ahDepthUnitScale / count
}

// ahDepthScanCols is the Stage-2 per-listing column set, in scan order.
func ahDepthScanCols() []string {
	return []string{"houseid", "itemEntry", "itemowner", "buyoutprice", "count", "name", "Quality", "class"}
}

const (
	// ahDepthFloorQ / ahDepthScanQ are the query-distinctive fragments pinning
	// each of the two ExpectQuery calls.
	ahDepthFloorQ = "GROUP BY ah.houseid, ii.itemEntry"
	ahDepthScanQ  = "ah.itemowner, ah.buyoutprice, ii.count"
)

// ahDepthExpectBothQueries wires the USE + both stage queries with the given
// row sets, in the order collectAhCrossHouseArbitrageDepth issues them.
func ahDepthExpectBothQueries(mock sqlmock.Sqlmock, floorRows, scanRows *sqlmock.Rows) {
	mock.ExpectExec(regexp.QuoteMeta("USE `acore_characters`")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta(ahDepthFloorQ)).WillReturnRows(floorRows)
	mock.ExpectQuery(regexp.QuoteMeta(ahDepthScanQ)).
		WithArgs(int64(ahDepthScanCap + 1)).
		WillReturnRows(scanRows)
}

// ahDepthExpectRatesQuery wires the acore_world.auctionhouse_dbc rate lookup
// that the HANDLER issues before the fold. collectAhCrossHouseArbitrageDepth
// takes an already-resolved *ahAuctionRates and never queries for it, so this
// belongs to handler-level tests only — which is also why it is not folded into
// ahDepthExpectBothQueries (that helper is shared with the collect-level tests,
// and an extra expectation there would go unmet).
//
// Passing NO rows is deliberate for the pre-existing tests: an empty
// auctionhouse_dbc resolves to nil rates, which forces the gross-only path those
// tests were written against, so every one of their assertions still asserts
// exactly what it always did.
func ahDepthExpectRatesQuery(mock sqlmock.Sqlmock, rows *sqlmock.Rows) {
	mock.ExpectQuery(regexp.QuoteMeta(ahAuctionRatesQuery)).WillReturnRows(rows)
}

// ahDepthRateCols are the auctionhouse_dbc columns the rate lookup scans.
func ahDepthRateCols() []string {
	return []string{"ID", "ConsignmentRate", "DepositRate"}
}

// ahDepthSortFixture is the shared two-item fixture for the sort-key tests. It
// is deliberately built so every sort key produces a DIFFERENT order, which is
// also the tool's whole thesis: item 1 shows a spectacular 900% spread on a
// single 1-unit floor listing (a fragile trap), item 2 a modest 20% spread on a
// 10-unit floor (the genuinely tradeable depth).
//
//	item 1: Alliance 100c x1 unit (100/unit), Neutral 1000c x1 (1000/unit)
//	        -> spread 900.0, gain 900, depth 1, cheapest unit 100
//	item 2: Alliance 5000c x10 units (500/unit), Neutral 600c x1 (600/unit)
//	        -> spread  20.0, gain 1000, depth 10, cheapest unit 500
//
// Item 2's Alliance listing is priced 5000c for its 10-unit stack, NOT 500c. The
// fixture used to say 500c while its own comment called that "the floor", and
// under a per-listing reading those were the same claim; per unit they are not,
// and 500c-for-10 would be a 50c unit floor rather than the 500c one every
// expected order below was written against. The rows now state what the comment
// always meant, and all four expected orders are unchanged as a result.
func ahDepthSortFixture() (*sqlmock.Rows, *sqlmock.Rows) {
	floors := sqlmock.NewRows(ahDepthFloorCols()).
		AddRow(2, int64(1), ahDepthUnitFloor(100, 1)).
		AddRow(7, int64(1), ahDepthUnitFloor(1000, 1)).
		AddRow(2, int64(2), ahDepthUnitFloor(5000, 10)).
		AddRow(7, int64(2), ahDepthUnitFloor(600, 1))
	scan := sqlmock.NewRows(ahDepthScanCols()).
		AddRow(2, int64(1), int64(10), int64(100), int64(1), "Thin Trap", 3, 2).
		AddRow(7, int64(1), int64(11), int64(1000), int64(1), "Thin Trap", 3, 2).
		AddRow(2, int64(2), int64(20), int64(5000), int64(10), "Deep Stock", 3, 2).
		AddRow(7, int64(2), int64(21), int64(600), int64(1), "Deep Stock", 3, 2)
	return floors, scan
}

// TestAhCrossHouseArbitrageDepth_DescriptionMentionsContext guards the
// vocabulary that steers the agent here vs ah_cross_house_arbitrage (floor price
// only, no stock) or ah_floor_saturation (per-house crowding, one house per
// call) — the depth/quantity framing is load-bearing for tool selection.
func TestAhCrossHouseArbitrageDepth_DescriptionMentionsContext(t *testing.T) {
	reg := NewRegistry()
	RegisterAhCrossHouseArbitrageDepthTool(reg, DBDeps{})
	tool, ok := reg.Get("ah_cross_house_arbitrage_depth")
	if !ok {
		t.Fatal("ah_cross_house_arbitrage_depth not registered")
	}
	for _, kw := range []string{
		"acore_characters.auctionhouse", "item_instance", "acore_world.item_template", "ops_ro",
		"ah_cross_house_arbitrage", "ah_floor_saturation", "floorQuantity", "floorSellers",
		"cheapestFloorQuantity", "sweepCostCopper", "potentialGainCopper", "spreadPct",
		"housesListed", "floorKnown", "qualityName", "className", "topN", "sortBy", "minHouses",
		"minFloorQuantity", "minQuality", "truncated", "Alliance", "Horde", "Neutral", "Read-only",
		// The unit-price basis is the thing an agent most needs stated outright,
		// since every money figure here is only meaningful against it.
		"floorUnitCopper", "floorCostCopper", "stackedFloor", "matchedItemsStackedFloor",
		"listingsSkippedNonPositiveCount", "cheapestFloorUnitCopper", "WHOLE listing",
	} {
		if !strings.Contains(tool.Description, kw) {
			t.Errorf("description missing %q: %s", kw, tool.Description)
		}
	}
}

// TestAhCrossHouseArbitrageDepth_ReadOnlyAnnotation pins readOnlyHint=true so a
// future copy-paste of a destructive sibling can't silently flip the gate.
func TestAhCrossHouseArbitrageDepth_ReadOnlyAnnotation(t *testing.T) {
	reg := NewRegistry()
	RegisterAhCrossHouseArbitrageDepthTool(reg, DBDeps{})
	tool, _ := reg.Get("ah_cross_house_arbitrage_depth")
	if !strings.Contains(string(tool.Annotations), `"readOnlyHint":true`) {
		t.Errorf("annotations missing readOnlyHint=true: %s", tool.Annotations)
	}
}

func TestAhCrossHouseArbitrageDepth_NoPoolReturnsError(t *testing.T) {
	reg := NewRegistry()
	RegisterAhCrossHouseArbitrageDepthTool(reg, DBDeps{}) // QueryDB nil
	tool, _ := reg.Get("ah_cross_house_arbitrage_depth")
	resp := tool.Handler(context.Background(), json.RawMessage(`{}`), "")
	got, _ := resp.(map[string]any)
	if msg, _ := got["error"].(string); !strings.Contains(msg, "ops_ro pool not configured") {
		t.Errorf("expected pool-not-configured error, got %v", got)
	}
}

// TestAhCrossHouseArbitrageDepth_TopNClamps drives the handler and asserts the
// post-clamp topN echo. topN only slices the post-fold display list — it never
// binds into either query.
func TestAhCrossHouseArbitrageDepth_TopNClamps(t *testing.T) {
	cases := []struct {
		name     string
		args     string
		wantTopN int
	}{
		{"unset uses default", `{}`, ahDepthDefaultTop},
		{"zero falls back to default", `{"topN":0}`, ahDepthDefaultTop},
		{"negative falls back to default", `{"topN":-7}`, ahDepthDefaultTop},
		{"oversized clamps to max", `{"topN":99999}`, ahDepthMaxTop},
		{"in-range passes through", `{"topN":42}`, 42},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			ahDepthExpectRatesQuery(mock, sqlmock.NewRows(ahDepthRateCols()))
			ahDepthExpectBothQueries(mock,
				sqlmock.NewRows(ahDepthFloorCols()),
				sqlmock.NewRows(ahDepthScanCols()))

			reg := NewRegistry()
			RegisterAhCrossHouseArbitrageDepthTool(reg, DBDeps{QueryDB: db})
			tool, _ := reg.Get("ah_cross_house_arbitrage_depth")
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

// TestAhCrossHouseArbitrageDepth_MinHousesClamp — minHouses defaults to 2 and
// clamps to the 2-3 range (there are only 3 houses total).
func TestAhCrossHouseArbitrageDepth_MinHousesClamp(t *testing.T) {
	cases := []struct {
		name          string
		args          string
		wantMinHouses int
	}{
		{"unset uses default 2", `{}`, 2},
		{"zero clamps to 2", `{"minHouses":0}`, 2},
		{"one clamps to 2", `{"minHouses":1}`, 2},
		{"negative clamps to 2", `{"minHouses":-4}`, 2},
		{"three passes through", `{"minHouses":3}`, 3},
		{"oversized clamps to 3", `{"minHouses":99}`, 3},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			ahDepthExpectRatesQuery(mock, sqlmock.NewRows(ahDepthRateCols()))
			ahDepthExpectBothQueries(mock,
				sqlmock.NewRows(ahDepthFloorCols()),
				sqlmock.NewRows(ahDepthScanCols()))

			reg := NewRegistry()
			RegisterAhCrossHouseArbitrageDepthTool(reg, DBDeps{QueryDB: db})
			tool, _ := reg.Get("ah_cross_house_arbitrage_depth")
			resp := tool.Handler(context.Background(), json.RawMessage(c.args), "")
			got, _ := resp.(map[string]any)
			if got["error"] != nil {
				t.Fatalf("unexpected error: %v", got)
			}
			if n, _ := got["minHouses"].(int); n != c.wantMinHouses {
				t.Errorf("minHouses: %v want %d", got["minHouses"], c.wantMinHouses)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Errorf("unmet mock expectations: %v", err)
			}
		})
	}
}

// TestAhCrossHouseArbitrageDepth_MinFloorQuantityClamp — minFloorQuantity is a
// soft floor: default 1, anything below 1 clamps up (a matched floor always
// holds at least one unit), larger values pass through.
func TestAhCrossHouseArbitrageDepth_MinFloorQuantityClamp(t *testing.T) {
	cases := []struct {
		name    string
		args    string
		wantQty int64
	}{
		{"unset uses default 1", `{}`, 1},
		{"zero clamps to 1", `{"minFloorQuantity":0}`, 1},
		{"negative clamps to 1", `{"minFloorQuantity":-9}`, 1},
		{"in-range passes through", `{"minFloorQuantity":25}`, 25},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			ahDepthExpectRatesQuery(mock, sqlmock.NewRows(ahDepthRateCols()))
			ahDepthExpectBothQueries(mock,
				sqlmock.NewRows(ahDepthFloorCols()),
				sqlmock.NewRows(ahDepthScanCols()))

			reg := NewRegistry()
			RegisterAhCrossHouseArbitrageDepthTool(reg, DBDeps{QueryDB: db})
			tool, _ := reg.Get("ah_cross_house_arbitrage_depth")
			resp := tool.Handler(context.Background(), json.RawMessage(c.args), "")
			got, _ := resp.(map[string]any)
			if got["error"] != nil {
				t.Fatalf("unexpected error: %v", got)
			}
			if n, _ := got["minFloorQuantity"].(int64); n != c.wantQty {
				t.Errorf("minFloorQuantity: %v want %d", got["minFloorQuantity"], c.wantQty)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Errorf("unmet mock expectations: %v", err)
			}
		})
	}
}

// TestAhCrossHouseArbitrageDepth_InvalidSortByRejected — an unknown sortBy is
// rejected before any query (a typo can't silently reorder the market).
func TestAhCrossHouseArbitrageDepth_InvalidSortByRejected(t *testing.T) {
	for _, bad := range []string{`{"sortBy":"quantity"}`, `{"sortBy":"GAIN"}`, `{"sortBy":"gain; DROP TABLE auctionhouse"}`} {
		db, mock, err := sqlmock.New()
		if err != nil {
			t.Fatal(err)
		}
		reg := NewRegistry()
		RegisterAhCrossHouseArbitrageDepthTool(reg, DBDeps{QueryDB: db})
		tool, _ := reg.Get("ah_cross_house_arbitrage_depth")
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

// TestAhCrossHouseArbitrageDepth_MinQualityRejected — out-of-range minQuality is
// rejected (no query) so a typo can't masquerade as an empty market.
func TestAhCrossHouseArbitrageDepth_MinQualityRejected(t *testing.T) {
	for _, bad := range []string{`{"minQuality":-1}`, `{"minQuality":8}`, `{"minQuality":99}`} {
		db, mock, err := sqlmock.New()
		if err != nil {
			t.Fatal(err)
		}
		reg := NewRegistry()
		RegisterAhCrossHouseArbitrageDepthTool(reg, DBDeps{QueryDB: db})
		tool, _ := reg.Get("ah_cross_house_arbitrage_depth")
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

// TestAhCrossHouseArbitrageDepth_MinQualityBindsScanOnly — minQuality binds an
// extra arg onto the SCAN query only. The floor map deliberately stays
// unfiltered: quality can't change which rows share an itemEntry, and filtering
// it there would only risk a floor drifting away from the listings it gates.
func TestAhCrossHouseArbitrageDepth_MinQualityBindsScanOnly(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ahDepthExpectRatesQuery(mock, sqlmock.NewRows(ahDepthRateCols()))
	mock.ExpectExec(regexp.QuoteMeta("USE `acore_characters`")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta("WHERE ah.buyoutprice > 0 AND ii.count > 0 GROUP BY ah.houseid, ii.itemEntry")).
		WithArgs().
		WillReturnRows(sqlmock.NewRows(ahDepthFloorCols()))
	mock.ExpectQuery(regexp.QuoteMeta("AND it.Quality >= ? LIMIT ?")).
		WithArgs(int64(4), int64(ahDepthScanCap+1)).
		WillReturnRows(sqlmock.NewRows(ahDepthScanCols()))

	reg := NewRegistry()
	RegisterAhCrossHouseArbitrageDepthTool(reg, DBDeps{QueryDB: db})
	tool, _ := reg.Get("ah_cross_house_arbitrage_depth")
	resp := tool.Handler(context.Background(), json.RawMessage(`{"minQuality":4}`), "")
	got, _ := resp.(map[string]any)
	if got["error"] != nil {
		t.Fatalf("unexpected error: %v", got)
	}
	if q, _ := got["minQuality"].(int); q != 4 {
		t.Errorf("minQuality: %v want 4", got["minQuality"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

func TestAhCrossHouseArbitrageDepth_MinQualityAbsentWhenUnset(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ahDepthExpectRatesQuery(mock, sqlmock.NewRows(ahDepthRateCols()))
	ahDepthExpectBothQueries(mock,
		sqlmock.NewRows(ahDepthFloorCols()),
		sqlmock.NewRows(ahDepthScanCols()))

	reg := NewRegistry()
	RegisterAhCrossHouseArbitrageDepthTool(reg, DBDeps{QueryDB: db})
	tool, _ := reg.Get("ah_cross_house_arbitrage_depth")
	resp := tool.Handler(context.Background(), json.RawMessage(`{}`), "")
	got, _ := resp.(map[string]any)
	if _, ok := got["minQuality"]; ok {
		t.Errorf("minQuality should be absent, got %v", got["minQuality"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestCollectAhCrossHouseArbitrageDepth_Golden folds a market covering: a
// 3-house item whose Alliance floor is shared by TWO sellers across two listings
// (the multi-seller floor tie ah_undercutters / ah_floor_saturation proved
// correct, here summing UNITS not listings), a listing priced above that floor
// which must count toward totals but not depth, a 2-house item, and a 1-house
// item that can't be an arbitrage row at all. Verifies the per-house depth fold,
// deterministic cheapest/priciest picks, spreadPct, the sweep/gain money
// derivations, and the honest totals independent of the default sort/topN.
func TestCollectAhCrossHouseArbitrageDepth_Golden(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	now := time.Unix(1_700_000_000, 0)
	ahDepthExpectBothQueries(mock,
		sqlmock.NewRows(ahDepthFloorCols()).
			// item 100 on all 3 houses; item 200 on 2; item 300 on 1.
			AddRow(2, int64(100), ahDepthUnitFloor(100, 2)).
			AddRow(6, int64(100), ahDepthUnitFloor(150, 1)).
			AddRow(7, int64(100), ahDepthUnitFloor(1200, 4)).
			AddRow(6, int64(200), ahDepthUnitFloor(1000, 1)).
			AddRow(7, int64(200), ahDepthUnitFloor(1100, 2)).
			AddRow(2, int64(300), ahDepthUnitFloor(50, 7)),
		sqlmock.NewRows(ahDepthScanCols()).
			// item 100 Alliance floor 50/unit: two sellers tie on the UNIT price
			// from different listing prices (100c for 2, 150c for 3) — 5 units at
			// the floor across 2 listings — plus one 5-unit listing at 400c
			// (80/unit) that is ABOVE the floor (totals only). The tie is what a
			// per-listing reading cannot see: those two listings agree on what a
			// unit costs and disagree on what a ticket costs.
			AddRow(2, int64(100), int64(10), int64(100), int64(2), "Primal Might", 4, 7).
			AddRow(2, int64(100), int64(11), int64(150), int64(3), "Primal Might", 4, 7).
			AddRow(2, int64(100), int64(12), int64(400), int64(5), "Primal Might", 4, 7).
			AddRow(6, int64(100), int64(20), int64(150), int64(1), "Primal Might", 4, 7).
			AddRow(7, int64(100), int64(30), int64(1200), int64(4), "Primal Might", 4, 7).
			// item 200: UNTOUCHED from the pre-fix fixture, and the sharpest
			// demonstration in this file. 1000c for 1 unit on Horde vs 1100c for 2
			// on Neutral: per listing Horde is "cheapest" at 1000 < 1100, per unit
			// Neutral is cheapest at 550 < 1000. Same rows, opposite trade.
			AddRow(6, int64(200), int64(40), int64(1000), int64(1), "Frozen Rune", 3, 2).
			AddRow(7, int64(200), int64(50), int64(1100), int64(2), "Frozen Rune", 3, 2).
			// item 300: single house -> dropped from matchedItems.
			AddRow(2, int64(300), int64(60), int64(50), int64(7), "Rusty Dagger", 0, 2))

	out, err := collectAhCrossHouseArbitrageDepth(context.Background(), db, now, 15,
		ahDepthDefaultMinHouses, "gain", ahDepthDefaultMinFloorQty, nil, nil)
	if err != nil {
		t.Fatalf("collectAhCrossHouseArbitrageDepth: %v", err)
	}

	if _, ok := out["minQuality"]; ok {
		t.Errorf("minQuality should be absent with no filter, got %v", out["minQuality"])
	}
	if v, _ := out["scanned"].(int); v != 8 {
		t.Errorf("scanned: %v want 8", out["scanned"])
	}
	if v, _ := out["truncated"].(bool); v {
		t.Errorf("truncated: %v want false", out["truncated"])
	}
	if v, _ := out["scanCap"].(int); v != ahDepthScanCap {
		t.Errorf("scanCap: %v want %d", out["scanCap"], ahDepthScanCap)
	}
	if v, _ := out["matchedItems"].(int); v != 2 {
		t.Errorf("matchedItems: %v want 2", out["matchedItems"])
	}

	totals, _ := out["totals"].(map[string]any)
	if totals == nil {
		t.Fatalf("totals missing: %v", out)
	}
	for _, c := range []struct {
		key  string
		want int
	}{
		{"distinctItemsListed", 3},
		{"distinctItemsOnMultipleHouses", 2},
		{"distinctItemsOnAllHouses", 1},
		{"totalListingsScanned", 8},
		{"housesMissingFloor", 0},
		// Both matched rows carry a stacked cheap floor, so both are rows a
		// per-listing reading would have mis-priced.
		{"matchedItemsStackedFloor", 2},
		{"listingsSkippedNonPositiveCount", 0},
	} {
		if v, _ := totals[c.key].(int); v != c.want {
			t.Errorf("totals.%s: %v want %d", c.key, totals[c.key], c.want)
		}
	}
	// 3+2+5+1+4 (item 100) + 1+2 (item 200) + 7 (item 300) = 25.
	if v, _ := totals["totalQuantityScanned"].(int64); v != 25 {
		t.Errorf("totals.totalQuantityScanned: %v want 25", totals["totalQuantityScanned"])
	}

	items, ok := out["items"].([]*ahDepthItem)
	if !ok || len(items) != 2 {
		t.Fatalf("items: %T len %d want []*ahDepthItem len 2", out["items"], len(items))
	}

	// Default sort is "gain" desc -> item 100 (5 units * 200 spread = 1000)
	// ahead of item 200 (1 unit * 100 = 100).
	it0 := items[0]
	if it0.ItemEntry != 100 || it0.ItemName != "Primal Might" || it0.QualityName != "Epic" || it0.ClassName != "Trade Goods" {
		t.Errorf("items[0] identity: %+v", it0)
	}
	if it0.HousesListed != 3 || len(it0.Houses) != 3 {
		t.Errorf("items[0] houses: HousesListed=%d len(Houses)=%d want 3/3", it0.HousesListed, len(it0.Houses))
	}
	// Houses are emitted in the fixed Alliance/Horde/Neutral walk order.
	alliance, horde, neutral := it0.Houses[0], it0.Houses[1], it0.Houses[2]
	if alliance.HouseID != 2 || alliance.Faction != "Alliance" || horde.HouseID != 6 || neutral.HouseID != 7 {
		t.Errorf("items[0] house order: %d/%d/%d want 2/6/7", alliance.HouseID, horde.HouseID, neutral.HouseID)
	}
	if !alliance.FloorKnown || alliance.FloorCopper != 100 {
		t.Errorf("items[0] Alliance floor: known=%v copper=%d want true/100", alliance.FloorKnown, alliance.FloorCopper)
	}
	if alliance.FloorQuantity != 5 || alliance.FloorListings != 2 || alliance.FloorSellers != 2 {
		t.Errorf("items[0] Alliance depth: qty=%d listings=%d sellers=%d want 5/2/2",
			alliance.FloorQuantity, alliance.FloorListings, alliance.FloorSellers)
	}
	// The 400-copper listing counts toward totals but never toward floor depth.
	if alliance.TotalListings != 3 || alliance.TotalQuantity != 10 {
		t.Errorf("items[0] Alliance totals: listings=%d qty=%d want 3/10",
			alliance.TotalListings, alliance.TotalQuantity)
	}
	if neutral.FloorQuantity != 4 || neutral.FloorSellers != 1 {
		t.Errorf("items[0] Neutral depth: qty=%d sellers=%d want 4/1", neutral.FloorQuantity, neutral.FloorSellers)
	}
	if it0.CheapestHouseID != 2 || it0.CheapestFaction != "Alliance" || it0.CheapestFloorCopper != 100 {
		t.Errorf("items[0] cheapest: %+v", it0)
	}
	if it0.CheapestFloorQuantity != 5 || it0.CheapestFloorListings != 2 || it0.CheapestFloorSellers != 2 {
		t.Errorf("items[0] cheapest depth: qty=%d listings=%d sellers=%d want 5/2/2",
			it0.CheapestFloorQuantity, it0.CheapestFloorListings, it0.CheapestFloorSellers)
	}
	if it0.PriciestHouseID != 7 || it0.PriciestFaction != "Neutral" || it0.PriciestFloorCopper != 1200 {
		t.Errorf("items[0] priciest: %+v", it0)
	}
	// Unit floors: Alliance 100c/2 = 50, Horde 150c/1 = 150, Neutral 1200c/4 = 300.
	if it0.CheapestFloorUnitCopper != 50.0 || it0.PriciestFloorUnitCopper != 300.0 {
		t.Errorf("items[0] unit floors: cheap=%v dear=%v want 50/300",
			it0.CheapestFloorUnitCopper, it0.PriciestFloorUnitCopper)
	}
	// Spread on UNIT floors: (300-50)/50 = 500%.
	if it0.SpreadPct != 500.0 {
		t.Errorf("items[0].SpreadPct: %v want 500.0", it0.SpreadPct)
	}
	// The exact capital for the cheap floor is what its two listings COST —
	// 100c + 150c = 250c — not cheapestFloorQuantity*cheapestFloorCopper (5*100 =
	// 500c), which is the reading this fixture is here to rule out.
	if it0.SweepCostCopper != 250 || it0.SweepCostMoney != "2s50c" {
		t.Errorf("items[0] sweep: %d %q want 250 \"2s50c\"", it0.SweepCostCopper, it0.SweepCostMoney)
	}
	if it0.CheapestFloorQuantity*it0.CheapestFloorCopper == it0.SweepCostCopper {
		t.Errorf("items[0] sweep cost coincides with the quantity*listing-price reading (%d) — "+
			"this fixture must distinguish them", it0.SweepCostCopper)
	}
	if !it0.StackedFloor {
		t.Errorf("items[0].StackedFloor: false, want true (the cheap floor is two multi-unit listings)")
	}
	// 5 units resold at the Neutral unit floor of 300c = 1500c, less the 250c
	// acquisition = 1250c.
	// formatMoney (signed), not formatCopper: the gross gain is no longer
	// non-negative by construction, since the cheap and dear legs are now a summed
	// exact cost against a scale-truncated unit price. formatMoney omits a zero
	// gold place, hence "12s 50c".
	if it0.PotentialGainCopper != 1250 || it0.PotentialGainMoney != "12s 50c" {
		t.Errorf("items[0] gain: %d %q want 1250 \"12s 50c\"", it0.PotentialGainCopper, it0.PotentialGainMoney)
	}
	if it0.CheapestFloorMoney != "1s" {
		t.Errorf("items[0].CheapestFloorMoney: %q want \"1s\"", it0.CheapestFloorMoney)
	}

	it1 := items[1]
	if it1.ItemEntry != 200 || it1.HousesListed != 2 || len(it1.Houses) != 2 {
		t.Errorf("items[1] identity: %+v", it1)
	}
	// THE FLIP, on rows unchanged from the pre-fix fixture. Horde lists 1000c for
	// ONE unit, Neutral 1100c for TWO. Comparing listing prices makes Horde the
	// cheap leg (1000 < 1100) and reports a 10% spread on a 1-unit floor; per unit
	// Neutral is the cheap leg at 550 against Horde's 1000, an 81.8% spread on 2
	// units. The pre-fix answer named the wrong house to buy from.
	if it1.CheapestHouseID != 7 || it1.CheapestFaction != "Neutral" || it1.CheapestFloorCopper != 1100 {
		t.Errorf("items[1] cheapest: house=%d %q copper=%d want 7/Neutral/1100 "+
			"(per unit 550 beats Horde's 1000; per listing it would be house 6)",
			it1.CheapestHouseID, it1.CheapestFaction, it1.CheapestFloorCopper)
	}
	if it1.PriciestHouseID != 6 || it1.PriciestFloorCopper != 1000 {
		t.Errorf("items[1] priciest: house=%d copper=%d want 6/1000", it1.PriciestHouseID, it1.PriciestFloorCopper)
	}
	if it1.CheapestFloorUnitCopper != 550.0 || it1.PriciestFloorUnitCopper != 1000.0 {
		t.Errorf("items[1] unit floors: cheap=%v dear=%v want 550/1000",
			it1.CheapestFloorUnitCopper, it1.PriciestFloorUnitCopper)
	}
	if it1.CheapestFloorQuantity != 2 {
		t.Errorf("items[1].CheapestFloorQuantity: %d want 2", it1.CheapestFloorQuantity)
	}
	if it1.SpreadPct != 81.8 {
		t.Errorf("items[1].SpreadPct: %v want 81.8", it1.SpreadPct)
	}
	// 2 units at the Horde unit floor of 1000c = 2000c, less the 1100c the Neutral
	// stack actually costs.
	if it1.SweepCostCopper != 1100 || it1.PotentialGainCopper != 900 {
		t.Errorf("items[1] sweep/gain: %d/%d want 1100/900", it1.SweepCostCopper, it1.PotentialGainCopper)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestCollectAhCrossHouseArbitrageDepth_SortKeys — all four keys over ONE
// fixture, each producing a different order. The gain-vs-spread divergence is
// the tool's reason to exist: the 900%-spread item is a 1-unit trap that the
// depth-weighted default correctly ranks BELOW the modest 20% spread carrying
// 10 units.
func TestCollectAhCrossHouseArbitrageDepth_SortKeys(t *testing.T) {
	cases := []struct {
		sortKey string
		want    []int64
	}{
		{"gain", []int64{2, 1}},          // 1000 vs 900
		{"spread", []int64{1, 2}},        // 900.0% vs 20.0%
		{"depth", []int64{2, 1}},         // 10 units vs 1
		{"cheapestFloor", []int64{1, 2}}, // 100c vs 500c, ascending
	}
	for _, c := range cases {
		t.Run(c.sortKey, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			floors, scan := ahDepthSortFixture()
			ahDepthExpectBothQueries(mock, floors, scan)

			out, err := collectAhCrossHouseArbitrageDepth(context.Background(), db,
				time.Unix(1_700_000_000, 0), 15, 2, c.sortKey, ahDepthDefaultMinFloorQty, nil, nil)
			if err != nil {
				t.Fatalf("collectAhCrossHouseArbitrageDepth: %v", err)
			}
			items, _ := out["items"].([]*ahDepthItem)
			if len(items) != len(c.want) {
				t.Fatalf("items len %d want %d", len(items), len(c.want))
			}
			for i, want := range c.want {
				if items[i].ItemEntry != want {
					t.Errorf("sortBy=%s items[%d].ItemEntry = %d want %d (order %v)",
						c.sortKey, i, items[i].ItemEntry, want, c.want)
				}
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Errorf("unmet mock expectations: %v", err)
			}
		})
	}
}

// TestCollectAhCrossHouseArbitrageDepth_MinFloorQuantityFilters — raising
// minFloorQuantity drops the fragile 1-unit floor, leaving only the item with
// real stock behind its cheap floor. matchedItems reflects the post-filter count
// while the totals stay honest over the full fold.
func TestCollectAhCrossHouseArbitrageDepth_MinFloorQuantityFilters(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	floors, scan := ahDepthSortFixture()
	ahDepthExpectBothQueries(mock, floors, scan)

	out, err := collectAhCrossHouseArbitrageDepth(context.Background(), db,
		time.Unix(1_700_000_000, 0), 15, 2, "gain", 5, nil, nil)
	if err != nil {
		t.Fatalf("collectAhCrossHouseArbitrageDepth: %v", err)
	}
	if v, _ := out["minFloorQuantity"].(int64); v != 5 {
		t.Errorf("minFloorQuantity echo: %v want 5", out["minFloorQuantity"])
	}
	if v, _ := out["matchedItems"].(int); v != 1 {
		t.Errorf("matchedItems: %v want 1 (the 1-unit floor is filtered out)", out["matchedItems"])
	}
	items, _ := out["items"].([]*ahDepthItem)
	if len(items) != 1 || items[0].ItemEntry != 2 {
		t.Fatalf("items: %+v want only itemEntry 2", items)
	}
	totals, _ := out["totals"].(map[string]any)
	if v, _ := totals["distinctItemsListed"].(int); v != 2 {
		t.Errorf("totals.distinctItemsListed: %v want 2 (honest, pre-filter)", totals["distinctItemsListed"])
	}
	if v, _ := totals["distinctItemsOnMultipleHouses"].(int); v != 2 {
		t.Errorf("totals.distinctItemsOnMultipleHouses: %v want 2 (honest, pre-filter)", totals["distinctItemsOnMultipleHouses"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestCollectAhCrossHouseArbitrageDepth_MinHousesThree — raising minHouses to 3
// drops an item listed on only 2 houses.
func TestCollectAhCrossHouseArbitrageDepth_MinHousesThree(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	floors, scan := ahDepthSortFixture()
	ahDepthExpectBothQueries(mock, floors, scan)

	out, err := collectAhCrossHouseArbitrageDepth(context.Background(), db,
		time.Unix(1_700_000_000, 0), 15, 3, "gain", ahDepthDefaultMinFloorQty, nil, nil)
	if err != nil {
		t.Fatalf("collectAhCrossHouseArbitrageDepth: %v", err)
	}
	if v, _ := out["matchedItems"].(int); v != 0 {
		t.Errorf("matchedItems: %v want 0 (both fixture items sit on 2 houses)", out["matchedItems"])
	}
	items, ok := out["items"].([]*ahDepthItem)
	if !ok || items == nil || len(items) != 0 {
		t.Errorf("items: %v want non-nil empty slice", items)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestCollectAhCrossHouseArbitrageDepth_MissingFloorHouseExcluded — a house whose
// floor row is absent from the (unLIMITed) floor map, i.e. a listing INSERTed
// between the two queries, is still folded into the totals but must not count
// toward housesListed nor win the cheapest pick with a phantom 0-copper floor.
func TestCollectAhCrossHouseArbitrageDepth_MissingFloorHouseExcluded(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ahDepthExpectBothQueries(mock,
		// Neutral (7) is deliberately MISSING from the floor map for item 700.
		sqlmock.NewRows(ahDepthFloorCols()).
			AddRow(2, int64(700), ahDepthUnitFloor(200, 4)).
			AddRow(6, int64(700), ahDepthUnitFloor(500, 1)),
		sqlmock.NewRows(ahDepthScanCols()).
			AddRow(2, int64(700), int64(10), int64(200), int64(4), "Late Lister", 2, 4).
			AddRow(6, int64(700), int64(11), int64(500), int64(1), "Late Lister", 2, 4).
			AddRow(7, int64(700), int64(12), int64(9), int64(6), "Late Lister", 2, 4))

	out, err := collectAhCrossHouseArbitrageDepth(context.Background(), db,
		time.Unix(1_700_000_000, 0), 15, 2, "gain", ahDepthDefaultMinFloorQty, nil, nil)
	if err != nil {
		t.Fatalf("collectAhCrossHouseArbitrageDepth: %v", err)
	}
	totals, _ := out["totals"].(map[string]any)
	if v, _ := totals["housesMissingFloor"].(int); v != 1 {
		t.Errorf("totals.housesMissingFloor: %v want 1", totals["housesMissingFloor"])
	}
	// The unknown-floor house still contributes its listing + units to the census.
	if v, _ := totals["totalListingsScanned"].(int); v != 3 {
		t.Errorf("totals.totalListingsScanned: %v want 3", totals["totalListingsScanned"])
	}
	if v, _ := totals["totalQuantityScanned"].(int64); v != 11 {
		t.Errorf("totals.totalQuantityScanned: %v want 11", totals["totalQuantityScanned"])
	}
	items, _ := out["items"].([]*ahDepthItem)
	if len(items) != 1 {
		t.Fatalf("items len %d want 1", len(items))
	}
	it := items[0]
	if it.HousesListed != 2 || len(it.Houses) != 3 {
		t.Errorf("HousesListed=%d len(Houses)=%d want 2/3 (unknown-floor house emitted, not counted)",
			it.HousesListed, len(it.Houses))
	}
	unknown := it.Houses[2]
	if unknown.HouseID != 7 || unknown.FloorKnown || unknown.FloorQuantity != 0 || unknown.FloorListings != 0 {
		t.Errorf("unknown-floor house: %+v want houseId 7, floorKnown false, zero depth", unknown)
	}
	if unknown.TotalListings != 1 || unknown.TotalQuantity != 6 {
		t.Errorf("unknown-floor house totals: listings=%d qty=%d want 1/6",
			unknown.TotalListings, unknown.TotalQuantity)
	}
	// The 9-copper phantom must NOT become the cheapest floor.
	if it.CheapestHouseID != 2 || it.CheapestFloorCopper != 200 {
		t.Errorf("cheapest: house=%d copper=%d want 2/200 (the 9c unknown-floor listing must not win)",
			it.CheapestHouseID, it.CheapestFloorCopper)
	}
	if it.PriciestHouseID != 6 || it.PriciestFloorCopper != 500 {
		t.Errorf("priciest: house=%d copper=%d want 6/500", it.PriciestHouseID, it.PriciestFloorCopper)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestCollectAhCrossHouseArbitrageDepth_TieBreakItemEntryAsc — two items tied on
// the sort metric fall back to itemEntry ascending, not scan/map order.
func TestCollectAhCrossHouseArbitrageDepth_TieBreakItemEntryAsc(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// Both items: floors 100/200 with 1 unit at each floor -> identical gain 100.
	ahDepthExpectBothQueries(mock,
		sqlmock.NewRows(ahDepthFloorCols()).
			AddRow(2, int64(500), ahDepthUnitFloor(100, 1)).
			AddRow(7, int64(500), ahDepthUnitFloor(200, 1)).
			AddRow(2, int64(400), ahDepthUnitFloor(100, 1)).
			AddRow(7, int64(400), ahDepthUnitFloor(200, 1)),
		sqlmock.NewRows(ahDepthScanCols()).
			AddRow(7, int64(500), int64(10), int64(200), int64(1), "Tied B", 1, 1).
			AddRow(2, int64(500), int64(11), int64(100), int64(1), "Tied B", 1, 1).
			AddRow(2, int64(400), int64(12), int64(100), int64(1), "Tied A", 1, 1).
			AddRow(7, int64(400), int64(13), int64(200), int64(1), "Tied A", 1, 1))

	out, err := collectAhCrossHouseArbitrageDepth(context.Background(), db,
		time.Unix(1_700_000_000, 0), 15, 2, "gain", ahDepthDefaultMinFloorQty, nil, nil)
	if err != nil {
		t.Fatalf("collectAhCrossHouseArbitrageDepth: %v", err)
	}
	items, _ := out["items"].([]*ahDepthItem)
	if len(items) != 2 || items[0].ItemEntry != 400 || items[1].ItemEntry != 500 {
		t.Errorf("items tiebreak order: %+v want [400, 500] (itemEntry asc on tied gain)", items)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestCollectAhCrossHouseArbitrageDepth_TopNSlicesAfterFold — topN trims the
// displayed list but matchedItems + totals still reflect the FULL fold.
func TestCollectAhCrossHouseArbitrageDepth_TopNSlicesAfterFold(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	floors, scan := ahDepthSortFixture()
	ahDepthExpectBothQueries(mock, floors, scan)

	out, err := collectAhCrossHouseArbitrageDepth(context.Background(), db,
		time.Unix(1_700_000_000, 0), 1, 2, "gain", ahDepthDefaultMinFloorQty, nil, nil)
	if err != nil {
		t.Fatalf("collectAhCrossHouseArbitrageDepth: %v", err)
	}
	items, _ := out["items"].([]*ahDepthItem)
	if len(items) != 1 {
		t.Fatalf("items len: %d want 1 (topN=1)", len(items))
	}
	if v, _ := out["matchedItems"].(int); v != 2 {
		t.Errorf("matchedItems: %v want 2 (full match count, independent of topN)", out["matchedItems"])
	}
	totals, _ := out["totals"].(map[string]any)
	if v, _ := totals["distinctItemsListed"].(int); v != 2 {
		t.Errorf("totals.distinctItemsListed: %v want 2", totals["distinctItemsListed"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestCollectAhCrossHouseArbitrageDepth_Empty — an empty market yields a non-nil
// empty items slice, matchedItems 0, and zeroed totals.
func TestCollectAhCrossHouseArbitrageDepth_Empty(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ahDepthExpectBothQueries(mock,
		sqlmock.NewRows(ahDepthFloorCols()),
		sqlmock.NewRows(ahDepthScanCols()))

	out, err := collectAhCrossHouseArbitrageDepth(context.Background(), db,
		time.Unix(1_700_000_000, 0), 15, ahDepthDefaultMinHouses, "gain", ahDepthDefaultMinFloorQty, nil, nil)
	if err != nil {
		t.Fatalf("collectAhCrossHouseArbitrageDepth: %v", err)
	}
	items, ok := out["items"].([]*ahDepthItem)
	if !ok {
		t.Fatalf("items type: %T want []*ahDepthItem", out["items"])
	}
	if items == nil || len(items) != 0 {
		t.Errorf("items: %v want non-nil empty slice", items)
	}
	if v, _ := out["matchedItems"].(int); v != 0 {
		t.Errorf("matchedItems: %v want 0", out["matchedItems"])
	}
	totals, _ := out["totals"].(map[string]any)
	if v, _ := totals["distinctItemsListed"].(int); v != 0 {
		t.Errorf("totals.distinctItemsListed: %v want 0", totals["distinctItemsListed"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestAhDepthUnitFloor_MatchesTheSQLExpression pins the one invariant the whole
// two-stage split rests on: the Go fold and the SQL floor map must compute a
// listing's unit price with the SAME expression, or a listing stops matching the
// floor it defines and every floorQuantity silently collapses to 0 — an empty
// market that looks like a quiet one rather than a bug.
func TestAhDepthUnitFloor_MatchesTheSQLExpression(t *testing.T) {
	if !strings.Contains(ahDepthFloorMapQuery, "FLOOR(ah.buyoutprice * 10000 / ii.count)") {
		t.Fatalf("floor map no longer floors a scaled unit price: %s", ahDepthFloorMapQuery)
	}
	// The literal in the SQL and the Go constant are two statements of one number.
	if !strings.Contains(ahDepthFloorMapQuery, strconv.Itoa(ahDepthUnitScale)) {
		t.Errorf("floor map scale literal disagrees with ahDepthUnitScale=%d: %s",
			ahDepthUnitScale, ahDepthFloorMapQuery)
	}
	if !strings.Contains(ahDepthFloorMapQuery, "ii.count > 0") {
		t.Errorf("floor map must exclude non-positive stacks (division guard): %s", ahDepthFloorMapQuery)
	}
}

// TestAhDepthHouseFold_StackedFloorMatchesItself is the regression for the
// truncation trap. A bare buyoutprice/count in integer arithmetic collapses a
// 3-copper 2-unit stack to a unit price of 1, which no longer equals the floor it
// defines, so the listing fails to match ITSELF. At ahDepthUnitScale it is 15000
// and matches exactly. Asserted across stacks that do not divide evenly, which is
// where an unscaled form fails.
func TestAhDepthHouseFold_StackedFloorMatchesItself(t *testing.T) {
	for _, c := range []struct {
		name          string
		buyout, count int64
	}{
		{"non-dividing 3c over 2", 3, 2},
		{"non-dividing 1c over 3", 1, 3},
		{"non-dividing 100c over 7", 100, 7},
		{"sub-copper 1c over 20", 1, 20},
		{"exact 100c over 4", 100, 4},
		{"single unit", 250, 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			floor := ahDepthUnitFloor(c.buyout, c.count)
			h := &ahDepthHouse{FloorKnown: true, floorUnitScaled: floor}
			h.fold(1, c.buyout, c.count)
			if h.FloorQuantity != c.count || h.FloorListings != 1 {
				t.Fatalf("a listing failed to match its own floor: qty=%d listings=%d want %d/1 "+
					"(buyout=%d count=%d scaled floor=%d)",
					h.FloorQuantity, h.FloorListings, c.count, c.buyout, c.count, floor)
			}
			if h.FloorCostCopper != c.buyout {
				t.Errorf("FloorCostCopper=%d want %d (the listing's own price)", h.FloorCostCopper, c.buyout)
			}
			// The unscaled form is what this scale exists to avoid; prove it would
			// have broken this case rather than asserting that it would.
			if c.buyout/c.count != c.buyout*ahDepthUnitScale/c.count/ahDepthUnitScale {
				t.Logf("unscaled unit price %d truncates away from the scaled %d/%d — the case this guards",
					c.buyout/c.count, floor, ahDepthUnitScale)
			}
		})
	}
}

// TestAhDepthHouseFold_DistinctUnitPricesDoNotShareAFloor — two listings at the
// same LISTING price but different stack sizes are NOT at the same floor, which
// is the whole correction. 100c-for-2 (50/unit) is the floor; 100c-for-4
// (25/unit) would be cheaper still and 100c-for-1 (100/unit) is dearer.
func TestAhDepthHouseFold_DistinctUnitPricesDoNotShareAFloor(t *testing.T) {
	h := &ahDepthHouse{FloorKnown: true, floorUnitScaled: ahDepthUnitFloor(100, 2)}
	h.fold(1, 100, 2) // at the floor
	h.fold(2, 100, 1) // same ticket price, dearer per unit -> totals only
	h.fold(3, 200, 4) // different ticket price, SAME unit price -> at the floor
	if h.FloorQuantity != 6 || h.FloorListings != 2 || len(h.floorOwners) != 2 {
		t.Errorf("floor set: qty=%d listings=%d sellers=%d want 6/2/2",
			h.FloorQuantity, h.FloorListings, len(h.floorOwners))
	}
	if h.FloorCostCopper != 300 {
		t.Errorf("FloorCostCopper=%d want 300 (100 + 200)", h.FloorCostCopper)
	}
	if h.TotalListings != 3 || h.TotalQuantity != 7 {
		t.Errorf("totals: listings=%d qty=%d want 3/7", h.TotalListings, h.TotalQuantity)
	}
	// FloorCopper is the cheapest TICKET among the floor listings, not the unit
	// price and not the cheapest ticket overall.
	if h.FloorCopper != 100 {
		t.Errorf("FloorCopper=%d want 100", h.FloorCopper)
	}
}

// TestAhDepthProceeds_NoOverflowAndNoSubCopperCollapse guards the two halves of
// the split multiply. The naive qty*unitScaled overflows int64 at realistic
// worst-case money, and a naive qty*(unitScaled/scale) throws away every
// sub-copper unit price — a 0.5c/unit floor over 100 units must be 50c, not 0.
func TestAhDepthProceeds_NoOverflowAndNoSubCopperCollapse(t *testing.T) {
	if got := ahDepthProceeds(100, ahDepthUnitFloor(1, 2)); got != 50 {
		t.Errorf("sub-copper floor: 100 units at 0.5c = %d want 50", got)
	}
	if got := ahDepthProceeds(5, ahDepthUnitFloor(1200, 4)); got != 1500 {
		t.Errorf("whole-copper floor: 5 units at 300c = %d want 1500", got)
	}
	// MAX_MONEY_AMOUNT-scale unit price against a large stock: the naive product
	// would wrap negative, this must stay positive and sane.
	const bigUnit = int64(2_000_000_000)
	got := ahDepthProceeds(1_000_000, bigUnit*ahDepthUnitScale)
	if got != bigUnit*1_000_000 {
		t.Errorf("large-value proceeds = %d want %d (overflow?)", got, bigUnit*1_000_000)
	}
	if got <= 0 {
		t.Fatalf("proceeds overflowed to %d", got)
	}
}

// TestCollectAhCrossHouseArbitrageDepth_NonPositiveCountCounted — a listing with
// a non-positive stack size has no unit price and cannot be placed against a unit
// floor, so it is excluded from the fold. It must be COUNTED, not dropped in
// silence: a row that vanished and a row that was measured and found ordinary
// must not look the same (the qualifier discipline #410/#414/#418 established).
func TestCollectAhCrossHouseArbitrageDepth_NonPositiveCountCounted(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ahDepthExpectBothQueries(mock,
		sqlmock.NewRows(ahDepthFloorCols()).
			AddRow(2, int64(900), ahDepthUnitFloor(100, 1)).
			AddRow(7, int64(900), ahDepthUnitFloor(400, 1)),
		sqlmock.NewRows(ahDepthScanCols()).
			AddRow(2, int64(900), int64(10), int64(100), int64(1), "Odd Stack", 2, 4).
			AddRow(7, int64(900), int64(11), int64(400), int64(1), "Odd Stack", 2, 4).
			// Corrupt row: no unit price is derivable from a zero stack.
			AddRow(2, int64(900), int64(12), int64(70), int64(0), "Odd Stack", 2, 4))

	out, err := collectAhCrossHouseArbitrageDepth(context.Background(), db,
		time.Unix(1_700_000_000, 0), 15, 2, "gain", ahDepthDefaultMinFloorQty, nil, nil)
	if err != nil {
		t.Fatalf("collectAhCrossHouseArbitrageDepth: %v", err)
	}
	totals, _ := out["totals"].(map[string]any)
	if v, _ := totals["listingsSkippedNonPositiveCount"].(int); v != 1 {
		t.Errorf("totals.listingsSkippedNonPositiveCount: %v want 1", totals["listingsSkippedNonPositiveCount"])
	}
	// It is excluded from the fold, so it must not inflate the depth census.
	if v, _ := totals["totalListingsScanned"].(int); v != 2 {
		t.Errorf("totals.totalListingsScanned: %v want 2 (the zero-stack row is not folded)", totals["totalListingsScanned"])
	}
	// scanned counts rows READ, which is what truncation is judged against.
	if v, _ := out["scanned"].(int); v != 3 {
		t.Errorf("scanned: %v want 3 (all rows were read)", out["scanned"])
	}
	items, _ := out["items"].([]*ahDepthItem)
	if len(items) != 1 || items[0].SweepCostCopper != 100 {
		t.Fatalf("items: %+v want one row with sweepCost 100", items)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestCollectAhCrossHouseArbitrageDepth_SingleUnitMarketUnchanged is the blast-radius
// bound. On a market where every listing is one unit, a unit price and a listing
// price are the same number, so the corrected arithmetic must return EXACTLY what
// the per-listing form returned: sweep = qty*floor, gain = qty*(dear-cheap), and
// nothing flagged as stacked. This is what makes the change safe for the common
// case and localises it to the markets that actually carry stacks.
func TestCollectAhCrossHouseArbitrageDepth_SingleUnitMarketUnchanged(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ahDepthExpectBothQueries(mock,
		sqlmock.NewRows(ahDepthFloorCols()).
			AddRow(2, int64(800), ahDepthUnitFloor(100, 1)).
			AddRow(7, int64(800), ahDepthUnitFloor(300, 1)),
		sqlmock.NewRows(ahDepthScanCols()).
			AddRow(2, int64(800), int64(10), int64(100), int64(1), "Plain Goods", 2, 4).
			AddRow(2, int64(800), int64(11), int64(100), int64(1), "Plain Goods", 2, 4).
			AddRow(7, int64(800), int64(12), int64(300), int64(1), "Plain Goods", 2, 4))

	out, err := collectAhCrossHouseArbitrageDepth(context.Background(), db,
		time.Unix(1_700_000_000, 0), 15, 2, "gain", ahDepthDefaultMinFloorQty, nil, nil)
	if err != nil {
		t.Fatalf("collectAhCrossHouseArbitrageDepth: %v", err)
	}
	items, _ := out["items"].([]*ahDepthItem)
	if len(items) != 1 {
		t.Fatalf("items len %d want 1", len(items))
	}
	it := items[0]
	// The pre-fix formulae, computed here and required to agree.
	wantSweep := it.CheapestFloorQuantity * it.CheapestFloorCopper
	wantGain := it.CheapestFloorQuantity * (it.PriciestFloorCopper - it.CheapestFloorCopper)
	if it.SweepCostCopper != wantSweep || it.SweepCostCopper != 200 {
		t.Errorf("sweep: %d want %d (=200, the per-listing reading)", it.SweepCostCopper, wantSweep)
	}
	if it.PotentialGainCopper != wantGain || it.PotentialGainCopper != 400 {
		t.Errorf("gain: %d want %d (=400, the per-listing reading)", it.PotentialGainCopper, wantGain)
	}
	if it.SpreadPct != 200.0 {
		t.Errorf("spreadPct: %v want 200.0 (unit and listing prices coincide here)", it.SpreadPct)
	}
	if it.StackedFloor {
		t.Errorf("StackedFloor: true, want false on an all-single-unit floor")
	}
	totals, _ := out["totals"].(map[string]any)
	if v, _ := totals["matchedItemsStackedFloor"].(int); v != 0 {
		t.Errorf("totals.matchedItemsStackedFloor: %v want 0 — a single-unit market is where the "+
			"two readings agree, so nothing should be flagged", totals["matchedItemsStackedFloor"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestCollectAhCrossHouseArbitrageDepth_BasisBlock — the response states the grain
// its floors are measured at, so a consumer never has to infer whether a figure is
// per unit or per listing (the basis-block shape db_global_status / db_table_bloat use).
func TestCollectAhCrossHouseArbitrageDepth_BasisBlock(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ahDepthExpectBothQueries(mock,
		sqlmock.NewRows(ahDepthFloorCols()),
		sqlmock.NewRows(ahDepthScanCols()))

	out, err := collectAhCrossHouseArbitrageDepth(context.Background(), db,
		time.Unix(1_700_000_000, 0), 15, 2, "gain", ahDepthDefaultMinFloorQty, nil, nil)
	if err != nil {
		t.Fatalf("collectAhCrossHouseArbitrageDepth: %v", err)
	}
	basis, ok := out["basis"].(map[string]any)
	if !ok {
		t.Fatalf("basis block missing: %v", out)
	}
	if g, _ := basis["floorGrain"].(string); g != "unitPrice" {
		t.Errorf("basis.floorGrain: %v want unitPrice", basis["floorGrain"])
	}
	if s, _ := basis["unitScale"].(int); s != ahDepthUnitScale {
		t.Errorf("basis.unitScale: %v want %d", basis["unitScale"], ahDepthUnitScale)
	}
	if d, _ := basis["detail"].(string); !strings.Contains(d, "WHOLE listing") {
		t.Errorf("basis.detail must state why the grain matters: %q", d)
	}
	// The empty path must carry the same key set as a populated one — a guard-clause
	// return that drifts from the main return makes a consumer branch on shape.
	totals, _ := out["totals"].(map[string]any)
	for _, k := range []string{"matchedItemsStackedFloor", "listingsSkippedNonPositiveCount"} {
		if _, ok := totals[k]; !ok {
			t.Errorf("totals.%s absent on the empty path", k)
		}
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestAhDepthHouseFold_NearbyUnitPricesDoNotCollapse is the OTHER half of the
// truncation trap, and the one that bites silently. Losing the scale does not
// merely make a listing miss its own floor — it MERGES floors that are not the
// same: 3c-for-2 (1.5/unit) and 1c-for-1 (1.0/unit) both truncate to an integer
// unit price of 1, so an unscaled matcher folds a genuinely cheaper listing into
// a dearer floor, inflating floorQuantity and floorCostCopper with stock that is
// not at that price at all. At ahDepthUnitScale they are 15000 and 10000 and stay
// distinct.
func TestAhDepthHouseFold_NearbyUnitPricesDoNotCollapse(t *testing.T) {
	floor := ahDepthUnitFloor(3, 2) // 1.5 copper per unit
	h := &ahDepthHouse{FloorKnown: true, floorUnitScaled: floor}
	h.fold(1, 3, 2) // AT the floor
	h.fold(2, 1, 1) // 1.0/unit — cheaper, a DIFFERENT floor, must not be folded in
	h.fold(3, 2, 1) // 2.0/unit — dearer, must not be folded in
	if h.FloorQuantity != 2 || h.FloorListings != 1 {
		t.Errorf("floor set: qty=%d listings=%d want 2/1 — unit prices of 1.0, 1.5 and 2.0 "+
			"collapsed into one floor (the unscaled-truncation bug)", h.FloorQuantity, h.FloorListings)
	}
	if h.FloorCostCopper != 3 {
		t.Errorf("FloorCostCopper=%d want 3 — only the 3c listing sits at this floor", h.FloorCostCopper)
	}
	// Prove the premise rather than asserting it: without the scale all three
	// listings share an integer unit price, so the guard above is load-bearing.
	if 3/2 != 1/1 {
		t.Fatal("fixture no longer exercises the collapse; pick unit prices that truncate together")
	}
	if h.TotalListings != 3 {
		t.Errorf("TotalListings=%d want 3 — every listing still counts toward the census", h.TotalListings)
	}
}
