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

// ahArbitrageCols is the grouped floor-query column set, in scan order.
func ahArbitrageCols() []string {
	return []string{"houseid", "entry", "name", "Quality", "class", "floor"}
}

// ahArbitrageQ is the query-distinctive fragment used to pin the single
// ExpectQuery call.
const ahArbitrageQ = "GROUP BY ah.houseid, it.entry, it.name, it.Quality, it.class"

// TestAhCrossHouseArbitrage_DescriptionMentionsContext guards the vocabulary
// that steers the agent here vs ah_floor_saturation (per-house crowding, one
// house per call) or ah_item_price_dispersion (per-item spread, house-agnostic)
// — the cross-house framing is load-bearing for tool selection.
func TestAhCrossHouseArbitrage_DescriptionMentionsContext(t *testing.T) {
	reg := NewRegistry()
	RegisterAhCrossHouseArbitrageTool(reg, DBDeps{})
	tool, ok := reg.Get("ah_cross_house_arbitrage")
	if !ok {
		t.Fatal("ah_cross_house_arbitrage not registered")
	}
	for _, kw := range []string{
		"acore_characters.auctionhouse", "item_instance", "acore_world.item_template", "ops_ro",
		"ah_floor_saturation", "ah_item_price_dispersion", "spreadPct", "allianceFloorCopper",
		"hordeFloorCopper", "neutralFloorCopper", "cheapestFaction", "priciestFaction", "housesListed",
		"qualityName", "className", "topN", "sortBy", "minHouses", "minQuality", "Alliance", "Horde",
		"Neutral", "Read-only",
	} {
		if !strings.Contains(tool.Description, kw) {
			t.Errorf("description missing %q: %s", kw, tool.Description)
		}
	}
}

// TestAhCrossHouseArbitrage_ReadOnlyAnnotation pins readOnlyHint=true so a
// future copy-paste of a destructive sibling can't silently flip the gate.
func TestAhCrossHouseArbitrage_ReadOnlyAnnotation(t *testing.T) {
	reg := NewRegistry()
	RegisterAhCrossHouseArbitrageTool(reg, DBDeps{})
	tool, _ := reg.Get("ah_cross_house_arbitrage")
	if !strings.Contains(string(tool.Annotations), `"readOnlyHint":true`) {
		t.Errorf("annotations missing readOnlyHint=true: %s", tool.Annotations)
	}
}

func TestAhCrossHouseArbitrage_NoPoolReturnsError(t *testing.T) {
	reg := NewRegistry()
	RegisterAhCrossHouseArbitrageTool(reg, DBDeps{}) // QueryDB nil
	tool, _ := reg.Get("ah_cross_house_arbitrage")
	resp := tool.Handler(context.Background(), json.RawMessage(`{}`), "")
	got, _ := resp.(map[string]any)
	if msg, _ := got["error"].(string); !strings.Contains(msg, "ops_ro pool not configured") {
		t.Errorf("expected pool-not-configured error, got %v", got)
	}
}

// TestAhCrossHouseArbitrage_TopNClamps drives the handler and asserts the
// post-clamp topN echo. topN only slices the post-fold display list — it does
// NOT bind into the floor query (which has no LIMIT at all), so only one query
// is mocked here.
func TestAhCrossHouseArbitrage_TopNClamps(t *testing.T) {
	cases := []struct {
		name     string
		args     string
		wantTopN int
	}{
		{"unset uses default", `{}`, ahArbitrageDefaultTop},
		{"zero falls back to default", `{"topN":0}`, ahArbitrageDefaultTop},
		{"negative falls back to default", `{"topN":-7}`, ahArbitrageDefaultTop},
		{"oversized clamps to max", `{"topN":99999}`, ahArbitrageMaxTop},
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
			mock.ExpectQuery(regexp.QuoteMeta(ahArbitrageQ)).
				WillReturnRows(sqlmock.NewRows(ahArbitrageCols()))

			reg := NewRegistry()
			RegisterAhCrossHouseArbitrageTool(reg, DBDeps{QueryDB: db})
			tool, _ := reg.Get("ah_cross_house_arbitrage")
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

// TestAhCrossHouseArbitrage_MinHousesClamp — minHouses defaults to 2 and
// clamps to the 2-3 range (there are only 3 houses total).
func TestAhCrossHouseArbitrage_MinHousesClamp(t *testing.T) {
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
			mock.ExpectExec(regexp.QuoteMeta("USE `acore_characters`")).
				WillReturnResult(sqlmock.NewResult(0, 0))
			mock.ExpectQuery(regexp.QuoteMeta(ahArbitrageQ)).
				WillReturnRows(sqlmock.NewRows(ahArbitrageCols()))

			reg := NewRegistry()
			RegisterAhCrossHouseArbitrageTool(reg, DBDeps{QueryDB: db})
			tool, _ := reg.Get("ah_cross_house_arbitrage")
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

// TestAhCrossHouseArbitrage_InvalidSortByRejected — an unknown sortBy is
// rejected before any query (a typo can't silently reorder the market).
func TestAhCrossHouseArbitrage_InvalidSortByRejected(t *testing.T) {
	for _, bad := range []string{`{"sortBy":"floor"}`, `{"sortBy":"SPREAD"}`, `{"sortBy":"spread; DROP TABLE auctionhouse"}`} {
		db, mock, err := sqlmock.New()
		if err != nil {
			t.Fatal(err)
		}
		reg := NewRegistry()
		RegisterAhCrossHouseArbitrageTool(reg, DBDeps{QueryDB: db})
		tool, _ := reg.Get("ah_cross_house_arbitrage")
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

// TestAhCrossHouseArbitrage_MinQualityRejected — out-of-range minQuality is
// rejected (no query) so a typo can't masquerade as an empty market.
func TestAhCrossHouseArbitrage_MinQualityRejected(t *testing.T) {
	for _, bad := range []string{`{"minQuality":-1}`, `{"minQuality":8}`, `{"minQuality":99}`} {
		db, mock, err := sqlmock.New()
		if err != nil {
			t.Fatal(err)
		}
		reg := NewRegistry()
		RegisterAhCrossHouseArbitrageTool(reg, DBDeps{QueryDB: db})
		tool, _ := reg.Get("ah_cross_house_arbitrage")
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

// TestAhCrossHouseArbitrage_MinQualityFilter — minQuality binds an extra arg
// onto the floor query and is echoed on the response; absent when unset.
func TestAhCrossHouseArbitrage_MinQualityFilter(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.ExpectExec(regexp.QuoteMeta("USE `acore_characters`")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta("AND it.Quality >= ?")).
		WithArgs(int64(4)).
		WillReturnRows(sqlmock.NewRows(ahArbitrageCols()))

	reg := NewRegistry()
	RegisterAhCrossHouseArbitrageTool(reg, DBDeps{QueryDB: db})
	tool, _ := reg.Get("ah_cross_house_arbitrage")
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

func TestAhCrossHouseArbitrage_MinQualityAbsentWhenUnset(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.ExpectExec(regexp.QuoteMeta("USE `acore_characters`")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta(ahArbitrageQ)).
		WillReturnRows(sqlmock.NewRows(ahArbitrageCols()))

	reg := NewRegistry()
	RegisterAhCrossHouseArbitrageTool(reg, DBDeps{QueryDB: db})
	tool, _ := reg.Get("ah_cross_house_arbitrage")
	resp := tool.Handler(context.Background(), json.RawMessage(`{}`), "")
	got, _ := resp.(map[string]any)
	if _, ok := got["minQuality"]; ok {
		t.Errorf("minQuality should be absent, got %v", got["minQuality"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestCollectAhCrossHouseArbitrage_Golden folds grouped (house, item) rows
// covering: a 3-house item with a clean spread, a 2-house item that just
// clears the default minHouses=2 floor, and a 1-house item that must be
// dropped from matchedItems entirely. Verifies per-house nullable floor
// fields, deterministic cheapest/priciest picks, spreadPct rounding, the
// enum maps, and the honest totals independent of the default sort/topN.
func TestCollectAhCrossHouseArbitrage_Golden(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	now := time.Unix(1_700_000_000, 0)
	mock.ExpectExec(regexp.QuoteMeta("USE `acore_characters`")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta(ahArbitrageQ)).
		WillReturnRows(sqlmock.NewRows(ahArbitrageCols()).
			// item 100: on all 3 houses, floors 100/150/300 -> cheapest Alliance 100,
			// priciest Neutral 300, spread (300-100)/100*100 = 200.0
			AddRow(2, int64(100), "Primal Might", 4, 7, int64(100)).
			AddRow(6, int64(100), "Primal Might", 4, 7, int64(150)).
			AddRow(7, int64(100), "Primal Might", 4, 7, int64(300)).
			// item 200: on 2 houses only (Horde/Neutral), floors 1000/1100 ->
			// cheapest Horde, priciest Neutral, spread (1100-1000)/1000*100 = 10.0
			AddRow(6, int64(200), "Frozen Rune", 3, 2, int64(1000)).
			AddRow(7, int64(200), "Frozen Rune", 3, 2, int64(1100)).
			// item 300: on 1 house only -> dropped from matchedItems (default minHouses=2)
			AddRow(2, int64(300), "Rusty Dagger", 0, 2, int64(50)))

	out, err := collectAhCrossHouseArbitrage(context.Background(), db, now, 15, ahArbitrageDefaultMinHouses, "spread", nil)
	if err != nil {
		t.Fatalf("collectAhCrossHouseArbitrage: %v", err)
	}

	if _, ok := out["minQuality"]; ok {
		t.Errorf("minQuality should be absent with no filter, got %v", out["minQuality"])
	}
	if v, _ := out["matchedItems"].(int); v != 2 {
		t.Errorf("matchedItems: %v want 2", out["matchedItems"])
	}

	totals, _ := out["totals"].(map[string]any)
	if totals == nil {
		t.Fatalf("totals missing: %v", out)
	}
	if v, _ := totals["distinctItemsListed"].(int); v != 3 {
		t.Errorf("totals.distinctItemsListed: %v want 3", totals["distinctItemsListed"])
	}
	if v, _ := totals["distinctItemsOnMultipleHouses"].(int); v != 2 {
		t.Errorf("totals.distinctItemsOnMultipleHouses: %v want 2", totals["distinctItemsOnMultipleHouses"])
	}
	if v, _ := totals["distinctItemsOnAllHouses"].(int); v != 1 {
		t.Errorf("totals.distinctItemsOnAllHouses: %v want 1", totals["distinctItemsOnAllHouses"])
	}

	items, ok := out["items"].([]*ahArbitrageItem)
	if !ok || len(items) != 2 {
		t.Fatalf("items: %T len %d want []*ahArbitrageItem len 2", out["items"], len(items))
	}

	// Default sort is "spread" desc -> item 100 (200.0) before item 200 (10.0).
	it0 := items[0]
	if it0.ItemEntry != 100 || it0.ItemName != "Primal Might" || it0.QualityName != "Epic" || it0.ClassName != "Trade Goods" {
		t.Errorf("items[0] identity: %+v", it0)
	}
	if it0.HousesListed != 3 {
		t.Errorf("items[0].HousesListed: %v want 3", it0.HousesListed)
	}
	if it0.AllianceFloorCopper == nil || *it0.AllianceFloorCopper != 100 {
		t.Errorf("items[0].AllianceFloorCopper: %+v want 100", it0.AllianceFloorCopper)
	}
	if it0.HordeFloorCopper == nil || *it0.HordeFloorCopper != 150 {
		t.Errorf("items[0].HordeFloorCopper: %+v want 150", it0.HordeFloorCopper)
	}
	if it0.NeutralFloorCopper == nil || *it0.NeutralFloorCopper != 300 {
		t.Errorf("items[0].NeutralFloorCopper: %+v want 300", it0.NeutralFloorCopper)
	}
	if it0.CheapestHouseID != 2 || it0.CheapestFaction != "Alliance" || it0.CheapestFloorCopper != 100 {
		t.Errorf("items[0] cheapest: %+v", it0)
	}
	if it0.PriciestHouseID != 7 || it0.PriciestFaction != "Neutral" || it0.PriciestFloorCopper != 300 {
		t.Errorf("items[0] priciest: %+v", it0)
	}
	if it0.SpreadPct != 200.0 {
		t.Errorf("items[0].SpreadPct: %v want 200.0", it0.SpreadPct)
	}

	it1 := items[1]
	if it1.ItemEntry != 200 || it1.HousesListed != 2 {
		t.Errorf("items[1] identity: %+v", it1)
	}
	if it1.AllianceFloorCopper != nil {
		t.Errorf("items[1].AllianceFloorCopper should be nil (not listed), got %v", *it1.AllianceFloorCopper)
	}
	if it1.CheapestHouseID != 6 || it1.CheapestFaction != "Horde" || it1.CheapestFloorCopper != 1000 {
		t.Errorf("items[1] cheapest: %+v", it1)
	}
	if it1.PriciestHouseID != 7 || it1.PriciestFaction != "Neutral" || it1.PriciestFloorCopper != 1100 {
		t.Errorf("items[1] priciest: %+v", it1)
	}
	if it1.SpreadPct != 10.0 {
		t.Errorf("items[1].SpreadPct: %v want 10.0", it1.SpreadPct)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestCollectAhCrossHouseArbitrage_MinHousesThree — raising minHouses to 3
// drops the 2-house item, leaving only the item listed on all 3 houses.
func TestCollectAhCrossHouseArbitrage_MinHousesThree(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.ExpectExec(regexp.QuoteMeta("USE `acore_characters`")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta(ahArbitrageQ)).
		WillReturnRows(sqlmock.NewRows(ahArbitrageCols()).
			AddRow(2, int64(100), "Primal Might", 4, 7, int64(100)).
			AddRow(6, int64(100), "Primal Might", 4, 7, int64(150)).
			AddRow(7, int64(100), "Primal Might", 4, 7, int64(300)).
			AddRow(6, int64(200), "Frozen Rune", 3, 2, int64(1000)).
			AddRow(7, int64(200), "Frozen Rune", 3, 2, int64(1100)))

	out, err := collectAhCrossHouseArbitrage(context.Background(), db, time.Unix(1_700_000_000, 0), 15, 3, "spread", nil)
	if err != nil {
		t.Fatalf("collectAhCrossHouseArbitrage: %v", err)
	}
	if v, _ := out["matchedItems"].(int); v != 1 {
		t.Errorf("matchedItems: %v want 1", out["matchedItems"])
	}
	items, _ := out["items"].([]*ahArbitrageItem)
	if len(items) != 1 || items[0].ItemEntry != 100 {
		t.Errorf("items: %+v want only itemEntry 100", items)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestCollectAhCrossHouseArbitrage_SortCheapestFloor — the cheapestFloor sort
// key orders ascending by CheapestFloorCopper (lowest-capital entry first),
// the inverse direction of the default spread-desc sort.
func TestCollectAhCrossHouseArbitrage_SortCheapestFloor(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.ExpectExec(regexp.QuoteMeta("USE `acore_characters`")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta(ahArbitrageQ)).
		WillReturnRows(sqlmock.NewRows(ahArbitrageCols()).
			AddRow(2, int64(100), "Primal Might", 4, 7, int64(100)).
			AddRow(6, int64(100), "Primal Might", 4, 7, int64(150)).
			AddRow(7, int64(100), "Primal Might", 4, 7, int64(300)).
			AddRow(6, int64(200), "Frozen Rune", 3, 2, int64(1000)).
			AddRow(7, int64(200), "Frozen Rune", 3, 2, int64(1100)))

	out, err := collectAhCrossHouseArbitrage(context.Background(), db, time.Unix(1_700_000_000, 0), 15, 2, "cheapestFloor", nil)
	if err != nil {
		t.Fatalf("collectAhCrossHouseArbitrage: %v", err)
	}
	items, _ := out["items"].([]*ahArbitrageItem)
	if len(items) != 2 || items[0].ItemEntry != 100 || items[1].ItemEntry != 200 {
		t.Errorf("items order: %+v want [100 (floor 100), 200 (floor 1000)]", items)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestCollectAhCrossHouseArbitrage_TieBreakItemEntryAsc — two items tied on
// the sort metric fall back to itemEntry ascending, not scan/map order.
func TestCollectAhCrossHouseArbitrage_TieBreakItemEntryAsc(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.ExpectExec(regexp.QuoteMeta("USE `acore_characters`")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	// Both items: floors 100/200 on 2 houses -> identical spreadPct = 100.0.
	mock.ExpectQuery(regexp.QuoteMeta(ahArbitrageQ)).
		WillReturnRows(sqlmock.NewRows(ahArbitrageCols()).
			AddRow(7, int64(500), "Tied B", 1, 1, int64(200)).
			AddRow(2, int64(500), "Tied B", 1, 1, int64(100)).
			AddRow(2, int64(400), "Tied A", 1, 1, int64(100)).
			AddRow(7, int64(400), "Tied A", 1, 1, int64(200)))

	out, err := collectAhCrossHouseArbitrage(context.Background(), db, time.Unix(1_700_000_000, 0), 15, 2, "spread", nil)
	if err != nil {
		t.Fatalf("collectAhCrossHouseArbitrage: %v", err)
	}
	items, _ := out["items"].([]*ahArbitrageItem)
	if len(items) != 2 || items[0].ItemEntry != 400 || items[1].ItemEntry != 500 {
		t.Errorf("items tiebreak order: %+v want [400, 500] (itemEntry asc on tied spreadPct)", items)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestCollectAhCrossHouseArbitrage_TopNSlicesAfterFold — topN trims the
// displayed list but matchedItems + totals still reflect the FULL fold.
func TestCollectAhCrossHouseArbitrage_TopNSlicesAfterFold(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.ExpectExec(regexp.QuoteMeta("USE `acore_characters`")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta(ahArbitrageQ)).
		WillReturnRows(sqlmock.NewRows(ahArbitrageCols()).
			AddRow(2, int64(1), "A", 1, 1, int64(100)).
			AddRow(6, int64(1), "A", 1, 1, int64(200)).
			AddRow(2, int64(2), "B", 1, 1, int64(100)).
			AddRow(6, int64(2), "B", 1, 1, int64(300)))

	out, err := collectAhCrossHouseArbitrage(context.Background(), db, time.Unix(1_700_000_000, 0), 1, 2, "spread", nil)
	if err != nil {
		t.Fatalf("collectAhCrossHouseArbitrage: %v", err)
	}
	items, _ := out["items"].([]*ahArbitrageItem)
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

// TestCollectAhCrossHouseArbitrage_Empty — an empty market yields a non-nil
// empty items slice, matchedItems 0, and zeroed totals.
func TestCollectAhCrossHouseArbitrage_Empty(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.ExpectExec(regexp.QuoteMeta("USE `acore_characters`")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta(ahArbitrageQ)).
		WillReturnRows(sqlmock.NewRows(ahArbitrageCols()))

	out, err := collectAhCrossHouseArbitrage(context.Background(), db, time.Unix(1_700_000_000, 0), 15, ahArbitrageDefaultMinHouses, "spread", nil)
	if err != nil {
		t.Fatalf("collectAhCrossHouseArbitrage: %v", err)
	}
	items, ok := out["items"].([]*ahArbitrageItem)
	if !ok {
		t.Fatalf("items type: %T want []*ahArbitrageItem", out["items"])
	}
	if items == nil || len(items) != 0 {
		t.Errorf("items: %v want non-nil empty slice", items)
	}
	if v, _ := out["matchedItems"].(int); v != 0 {
		t.Errorf("matchedItems: %v want 0", out["matchedItems"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}
