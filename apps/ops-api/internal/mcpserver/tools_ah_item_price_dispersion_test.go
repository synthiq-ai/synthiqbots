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

// ahDispersionCensusCols is the single-row census column set (honest realm totals).
func ahDispersionCensusCols() []string {
	return []string{"distinctItems", "totalPricedListings", "marketMin", "marketMax"}
}

// ahDispersionDisplayCols is the per-item leaderboard column set, in scan order.
func ahDispersionDisplayCols() []string {
	return []string{"entry", "name", "Quality", "class", "listings", "distinctSellers", "minBuyout", "maxBuyout", "totalBuyout"}
}

// ahDispersionCensusRow builds a one-row census result (aggregate-with-no-GROUP-BY
// always returns exactly one row on real MySQL).
func ahDispersionCensusRow(distinctItems, totalListings int, marketMin, marketMax int64) *sqlmock.Rows {
	return sqlmock.NewRows(ahDispersionCensusCols()).AddRow(distinctItems, totalListings, marketMin, marketMax)
}

// censusQ / displayQ are the query-distinctive fragments used to pin each of the
// two ordered ExpectQuery calls to the right query.
const (
	ahDispersionCensusQ  = "COALESCE(MIN(ah.buyoutprice), 0) AS marketMin"
	ahDispersionDisplayQ = "HAVING listings >= ?"
)

// TestAhItemPriceDispersion_DescriptionMentionsContext guards the vocabulary that
// steers the agent here vs ah_market_price_percentiles (whole-market) or
// ah_market_top_items (floor+avg) — the per-item spread / seller-disagreement
// framing is load-bearing for selection.
func TestAhItemPriceDispersion_DescriptionMentionsContext(t *testing.T) {
	reg := NewRegistry()
	RegisterAhItemPriceDispersionTool(reg, DBDeps{})
	tool, ok := reg.Get("ah_item_price_dispersion")
	if !ok {
		t.Fatal("ah_item_price_dispersion not registered")
	}
	for _, kw := range []string{
		"acore_characters.auctionhouse", "item_instance", "acore_world.item_template", "ops_ro",
		"ah_market_top_items", "ah_market_price_percentiles", "spreadPct", "minBuyout", "maxBuyout",
		"avgBuyout", "distinctSellers", "listings", "dispersion", "qualityName", "className",
		"topN", "sortBy", "minListings", "minQuality", "houseId", "Alliance", "Horde", "Neutral", "Read-only",
	} {
		if !strings.Contains(tool.Description, kw) {
			t.Errorf("description missing %q: %s", kw, tool.Description)
		}
	}
}

// TestAhItemPriceDispersion_ReadOnlyAnnotation pins readOnlyHint=true so a future
// copy-paste of a destructive sibling can't silently flip the gate.
func TestAhItemPriceDispersion_ReadOnlyAnnotation(t *testing.T) {
	reg := NewRegistry()
	RegisterAhItemPriceDispersionTool(reg, DBDeps{})
	tool, _ := reg.Get("ah_item_price_dispersion")
	if !strings.Contains(string(tool.Annotations), `"readOnlyHint":true`) {
		t.Errorf("annotations missing readOnlyHint=true: %s", tool.Annotations)
	}
}

func TestAhItemPriceDispersion_NoPoolReturnsError(t *testing.T) {
	reg := NewRegistry()
	RegisterAhItemPriceDispersionTool(reg, DBDeps{}) // QueryDB nil
	tool, _ := reg.Get("ah_item_price_dispersion")
	resp := tool.Handler(context.Background(), json.RawMessage(`{}`), "")
	got, _ := resp.(map[string]any)
	if msg, _ := got["error"].(string); !strings.Contains(msg, "ops_ro pool not configured") {
		t.Errorf("expected pool-not-configured error, got %v", got)
	}
}

// TestAhItemPriceDispersion_TopNClamps drives the handler and asserts the post-clamp
// topN echo and that the display LIMIT bind matches the clamped value. Both the
// census and display queries are mocked (the tool runs two queries on one conn).
func TestAhItemPriceDispersion_TopNClamps(t *testing.T) {
	cases := []struct {
		name     string
		args     string
		wantTopN int
	}{
		{"unset uses default", `{}`, ahDispersionDefaultTop},
		{"zero falls back to default", `{"topN":0}`, ahDispersionDefaultTop},
		{"negative falls back to default", `{"topN":-7}`, ahDispersionDefaultTop},
		{"oversized clamps to max", `{"topN":99999}`, ahDispersionMaxTop},
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
			mock.ExpectQuery(regexp.QuoteMeta(ahDispersionCensusQ)).
				WillReturnRows(ahDispersionCensusRow(0, 0, 0, 0))
			mock.ExpectQuery(regexp.QuoteMeta(ahDispersionDisplayQ)).
				WithArgs(int64(ahDispersionDefaultMinItems), int64(c.wantTopN)).
				WillReturnRows(sqlmock.NewRows(ahDispersionDisplayCols()))

			reg := NewRegistry()
			RegisterAhItemPriceDispersionTool(reg, DBDeps{QueryDB: db})
			tool, _ := reg.Get("ah_item_price_dispersion")
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

// TestAhItemPriceDispersion_MinListingsClamp — minListings defaults to 2 and clamps
// up to 1 (a floor below one listing is meaningless); the HAVING bind and the echoed
// minListings both reflect the clamped value.
func TestAhItemPriceDispersion_MinListingsClamp(t *testing.T) {
	cases := []struct {
		name            string
		args            string
		wantMinListings int
	}{
		{"unset uses default 2", `{}`, ahDispersionDefaultMinItems},
		{"zero clamps to 1", `{"minListings":0}`, 1},
		{"negative clamps to 1", `{"minListings":-4}`, 1},
		{"one passes through", `{"minListings":1}`, 1},
		{"in-range passes through", `{"minListings":5}`, 5},
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
			mock.ExpectQuery(regexp.QuoteMeta(ahDispersionCensusQ)).
				WillReturnRows(ahDispersionCensusRow(0, 0, 0, 0))
			mock.ExpectQuery(regexp.QuoteMeta(ahDispersionDisplayQ)).
				WithArgs(int64(c.wantMinListings), int64(ahDispersionDefaultTop)).
				WillReturnRows(sqlmock.NewRows(ahDispersionDisplayCols()))

			reg := NewRegistry()
			RegisterAhItemPriceDispersionTool(reg, DBDeps{QueryDB: db})
			tool, _ := reg.Get("ah_item_price_dispersion")
			resp := tool.Handler(context.Background(), json.RawMessage(c.args), "")
			got, _ := resp.(map[string]any)
			if got["error"] != nil {
				t.Fatalf("unexpected error: %v", got)
			}
			if n, _ := got["minListings"].(int); n != c.wantMinListings {
				t.Errorf("minListings: %v want %d", got["minListings"], c.wantMinListings)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Errorf("unmet mock expectations: %v", err)
			}
		})
	}
}

// TestAhItemPriceDispersion_InvalidSortByRejected — an unknown sortBy is rejected
// before any query (a typo can't silently reorder the market). Case-sensitivity
// and an injection attempt are both covered. QueryDB is non-nil to prove the reject
// fires after the pool check but before any query.
func TestAhItemPriceDispersion_InvalidSortByRejected(t *testing.T) {
	for _, bad := range []string{`{"sortBy":"price"}`, `{"sortBy":"SPREAD"}`, `{"sortBy":"listings; DROP TABLE auctionhouse"}`} {
		db, mock, err := sqlmock.New()
		if err != nil {
			t.Fatal(err)
		}
		reg := NewRegistry()
		RegisterAhItemPriceDispersionTool(reg, DBDeps{QueryDB: db})
		tool, _ := reg.Get("ah_item_price_dispersion")
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

// TestAhItemPriceDispersion_MinQualityRejected — out-of-range minQuality is rejected
// (no query) so a typo can't masquerade as an empty market.
func TestAhItemPriceDispersion_MinQualityRejected(t *testing.T) {
	for _, bad := range []string{`{"minQuality":-1}`, `{"minQuality":8}`, `{"minQuality":99}`} {
		db, mock, err := sqlmock.New()
		if err != nil {
			t.Fatal(err)
		}
		reg := NewRegistry()
		RegisterAhItemPriceDispersionTool(reg, DBDeps{QueryDB: db})
		tool, _ := reg.Get("ah_item_price_dispersion")
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

// TestAhItemPriceDispersion_InvalidHouseIdRejected — a houseId that isn't a real
// AuctionHouseId (2/6/7) is rejected (no query), not silently treated as an empty
// house.
func TestAhItemPriceDispersion_InvalidHouseIdRejected(t *testing.T) {
	for _, bad := range []string{`{"houseId":0}`, `{"houseId":3}`, `{"houseId":99}`} {
		db, mock, err := sqlmock.New()
		if err != nil {
			t.Fatal(err)
		}
		reg := NewRegistry()
		RegisterAhItemPriceDispersionTool(reg, DBDeps{QueryDB: db})
		tool, _ := reg.Get("ah_item_price_dispersion")
		resp := tool.Handler(context.Background(), json.RawMessage(bad), "")
		got, _ := resp.(map[string]any)
		if msg, _ := got["error"].(string); !strings.Contains(msg, "invalid houseId") {
			t.Errorf("args %s: expected invalid-houseId error, got %v", bad, got)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("args %s: no query should run on reject: %v", bad, err)
		}
		db.Close()
	}
}

// TestAhItemPriceDispersion_Filters drives each filter combination and asserts the
// display WHERE-clause shape (buyoutprice>0 base + optional AND filters), the bound
// args on BOTH the census (filter values) and display (filter values + minListings +
// topN) queries, and the echoed houseId/faction/minQuality keys.
func TestAhItemPriceDispersion_Filters(t *testing.T) {
	cases := []struct {
		name         string
		args         string
		displayWhere string
		censusArgs   []driverArg
		displayArgs  []driverArg
		wantHouse    int // -1 = key absent
		wantFaction  string
		wantMinQ     int // -1 = key absent
	}{
		{
			name:         "no filters",
			args:         `{}`,
			displayWhere: "WHERE ah.buyoutprice > 0 GROUP BY it.entry",
			censusArgs:   nil,
			displayArgs:  []driverArg{i(ahDispersionDefaultMinItems), i(ahDispersionDefaultTop)},
			wantHouse:    -1, wantMinQ: -1,
		},
		{
			name:         "house only",
			args:         `{"houseId":7}`,
			displayWhere: "WHERE ah.buyoutprice > 0 AND ah.houseid = ? GROUP BY it.entry",
			censusArgs:   []driverArg{i(7)},
			displayArgs:  []driverArg{i(7), i(ahDispersionDefaultMinItems), i(ahDispersionDefaultTop)},
			wantHouse:    7, wantFaction: "Neutral", wantMinQ: -1,
		},
		{
			name:         "quality only",
			args:         `{"minQuality":3}`,
			displayWhere: "WHERE ah.buyoutprice > 0 AND it.Quality >= ? GROUP BY it.entry",
			censusArgs:   []driverArg{i(3)},
			displayArgs:  []driverArg{i(3), i(ahDispersionDefaultMinItems), i(ahDispersionDefaultTop)},
			wantHouse:    -1, wantMinQ: 3,
		},
		{
			name:         "both filters + topN + minListings",
			args:         `{"houseId":2,"minQuality":4,"topN":5,"minListings":3}`,
			displayWhere: "WHERE ah.buyoutprice > 0 AND ah.houseid = ? AND it.Quality >= ? GROUP BY it.entry",
			censusArgs:   []driverArg{i(2), i(4)},
			displayArgs:  []driverArg{i(2), i(4), i(3), i(5)},
			wantHouse:    2, wantFaction: "Alliance", wantMinQ: 4,
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
			censusExpect := mock.ExpectQuery(regexp.QuoteMeta(ahDispersionCensusQ))
			if len(c.censusArgs) > 0 {
				censusExpect = censusExpect.WithArgs(toValues(c.censusArgs)...)
			}
			censusExpect.WillReturnRows(ahDispersionCensusRow(0, 0, 0, 0))
			mock.ExpectQuery(regexp.QuoteMeta(c.displayWhere)).
				WithArgs(toValues(c.displayArgs)...).
				WillReturnRows(sqlmock.NewRows(ahDispersionDisplayCols()))

			reg := NewRegistry()
			RegisterAhItemPriceDispersionTool(reg, DBDeps{QueryDB: db})
			tool, _ := reg.Get("ah_item_price_dispersion")
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

// TestAhItemPriceDispersion_SortByClause — each sortBy key produces its distinct
// ORDER BY clause in the display query. "spread"/"avgPrice" order by the aggregate
// expression directly; "listings" orders by the SELECT alias.
func TestAhItemPriceDispersion_SortByClause(t *testing.T) {
	cases := []struct {
		sortBy     string
		orderBySub string
	}{
		{"spread", "ORDER BY ((MAX(ah.buyoutprice) - MIN(ah.buyoutprice)) * 100.0 / MIN(ah.buyoutprice)) DESC"},
		{"listings", "ORDER BY listings DESC"},
		{"avgPrice", "ORDER BY (SUM(ah.buyoutprice) / COUNT(*)) DESC"},
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
			mock.ExpectQuery(regexp.QuoteMeta(ahDispersionCensusQ)).
				WillReturnRows(ahDispersionCensusRow(0, 0, 0, 0))
			mock.ExpectQuery(regexp.QuoteMeta(c.orderBySub)).
				WithArgs(int64(ahDispersionDefaultMinItems), int64(ahDispersionDefaultTop)).
				WillReturnRows(sqlmock.NewRows(ahDispersionDisplayCols()))

			out, err := collectAhItemPriceDispersion(context.Background(), db, time.Unix(1_700_000_000, 0),
				ahDispersionDefaultTop, ahDispersionDefaultMinItems, c.sortBy, nil, nil)
			if err != nil {
				t.Fatalf("collectAhItemPriceDispersion: %v", err)
			}
			if sb, _ := out["sortBy"].(string); sb != c.sortBy {
				t.Errorf("sortBy echo: %v want %q", out["sortBy"], c.sortBy)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Errorf("unmet mock expectations: %v", err)
			}
		})
	}
}

// TestCollectAhItemPriceDispersion_Golden folds three grouped rows plus a separate
// census, verifying the per-item spreadPct (incl. one-decimal rounding), the
// Go-folded avg (total/listings), the min/max/avg gold strings, the quality/class
// enum maps, the honest realm totals + marketSpreadPct (independent of the displayed
// rows), and that the handler trusts the SQL ORDER BY (no Go-side re-sort).
func TestCollectAhItemPriceDispersion_Golden(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	now := time.Unix(1_700_000_000, 0)
	mock.ExpectExec(regexp.QuoteMeta("USE `acore_characters`")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	// Census: realm-wide extremes independent of the 3 displayed rows —
	// marketSpreadPct = (1000000-10000)/10000*100 = 9900.0.
	mock.ExpectQuery(regexp.QuoteMeta(ahDispersionCensusQ)).
		WillReturnRows(ahDispersionCensusRow(8, 420, 10000, 1000000))
	// Display, already in spread-desc order as the SQL ORDER BY returns it.
	mock.ExpectQuery(regexp.QuoteMeta(ahDispersionDisplayQ)).
		WithArgs(int64(ahDispersionDefaultMinItems), int64(15)).
		WillReturnRows(sqlmock.NewRows(ahDispersionDisplayCols()).
			// widest: min 1g, max 10g -> spread 900.0; avg 200000/5 = 40000 = "4g"
			AddRow(int64(41163), "Titansteel Bar", 4, 7, 5, 3, int64(10000), int64(100000), int64(200000)).
			// mid: min 3g, max 4g -> (10000/30000)*100 = 33.3 (one-decimal rounding); avg 140000/4 = 35000 = "3g50s"
			AddRow(int64(3577), "Gold Bar", 1, 7, 4, 2, int64(30000), int64(40000), int64(140000)).
			// tight: min 2g, max 2g50s -> 5000/20000*100 = 25.0; avg 66000/3 = 22000 = "2g20s"
			AddRow(int64(23445), "Ethereum Prison Key", 3, 13, 3, 3, int64(20000), int64(25000), int64(66000)))

	out, err := collectAhItemPriceDispersion(context.Background(), db, now, 15, ahDispersionDefaultMinItems, "spread", nil, nil)
	if err != nil {
		t.Fatalf("collectAhItemPriceDispersion: %v", err)
	}
	if di, _ := out["displayedItems"].(int); di != 3 {
		t.Errorf("displayedItems: %v want 3", out["displayedItems"])
	}
	if _, ok := out["houseId"]; ok {
		t.Errorf("houseId should be absent with no filter, got %v", out["houseId"])
	}

	totals, _ := out["totals"].(map[string]any)
	if totals == nil {
		t.Fatalf("totals missing: %v", out)
	}
	if v, _ := totals["distinctItems"].(int); v != 8 {
		t.Errorf("totals.distinctItems: %v want 8", totals["distinctItems"])
	}
	if v, _ := totals["totalPricedListings"].(int); v != 420 {
		t.Errorf("totals.totalPricedListings: %v want 420", totals["totalPricedListings"])
	}
	if v, _ := totals["marketMinCopper"].(int64); v != 10000 {
		t.Errorf("totals.marketMinCopper: %v want 10000", totals["marketMinCopper"])
	}
	if v, _ := totals["marketMaxCopper"].(int64); v != 1000000 {
		t.Errorf("totals.marketMaxCopper: %v want 1000000", totals["marketMaxCopper"])
	}
	if v, _ := totals["marketMinGold"].(string); v != "1g" {
		t.Errorf("totals.marketMinGold: %v want 1g", totals["marketMinGold"])
	}
	if v, _ := totals["marketMaxGold"].(string); v != "100g" {
		t.Errorf("totals.marketMaxGold: %v want 100g", totals["marketMaxGold"])
	}
	if v, _ := totals["marketSpreadPct"].(float64); v != 9900.0 {
		t.Errorf("totals.marketSpreadPct: %v want 9900.0", totals["marketSpreadPct"])
	}

	items, ok := out["items"].([]*ahDispersionItem)
	if !ok || len(items) != 3 {
		t.Fatalf("items: %T len %d want []*ahDispersionItem len 3", out["items"], len(items))
	}

	it0 := items[0]
	if it0.ItemEntry != 41163 || it0.ItemName != "Titansteel Bar" || it0.Quality != 4 || it0.QualityName != "Epic" {
		t.Errorf("items[0] identity: %+v", it0)
	}
	if it0.Class != 7 || it0.ClassName != "Trade Goods" {
		t.Errorf("items[0] class: %+v", it0)
	}
	if it0.Listings != 5 || it0.DistinctSellers != 3 || it0.SpreadPct != 900.0 {
		t.Errorf("items[0] dispersion (want spread 900.0): %+v", it0)
	}
	if it0.MinBuyoutCopper != 10000 || it0.MaxBuyoutCopper != 100000 || it0.AvgBuyoutCopper != 40000 {
		t.Errorf("items[0] copper (want min 10000 max 100000 avg 40000): %+v", it0)
	}
	if it0.MinBuyoutGold != "1g" || it0.MaxBuyoutGold != "10g" || it0.AvgBuyoutGold != "4g" {
		t.Errorf("items[0] gold (want 1g/10g/4g): %+v", it0)
	}

	it1 := items[1]
	if it1.ItemName != "Gold Bar" || it1.QualityName != "Common" || it1.ClassName != "Trade Goods" || it1.SpreadPct != 33.3 {
		t.Errorf("items[1] dispersion (want spread 33.3): %+v", it1)
	}
	if it1.AvgBuyoutCopper != 35000 || it1.AvgBuyoutGold != "3g50s" {
		t.Errorf("items[1] avg (want 35000 / 3g50s): %+v", it1)
	}

	it2 := items[2]
	if it2.ItemName != "Ethereum Prison Key" || it2.QualityName != "Rare" || it2.ClassName != "Key" || it2.SpreadPct != 25.0 {
		t.Errorf("items[2] dispersion (want spread 25.0): %+v", it2)
	}
	if it2.MaxBuyoutGold != "2g50s" || it2.AvgBuyoutGold != "2g20s" {
		t.Errorf("items[2] gold (want max 2g50s / avg 2g20s): %+v", it2)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestCollectAhItemPriceDispersion_Empty — an empty market yields a non-nil empty
// items slice, displayedItems 0, zeroed totals, and a div-guarded marketSpreadPct
// of 0 (not NaN — the COALESCE'd marketMin is 0).
func TestCollectAhItemPriceDispersion_Empty(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.ExpectExec(regexp.QuoteMeta("USE `acore_characters`")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta(ahDispersionCensusQ)).
		WillReturnRows(ahDispersionCensusRow(0, 0, 0, 0))
	mock.ExpectQuery(regexp.QuoteMeta(ahDispersionDisplayQ)).
		WithArgs(int64(ahDispersionDefaultMinItems), int64(15)).
		WillReturnRows(sqlmock.NewRows(ahDispersionDisplayCols()))

	out, err := collectAhItemPriceDispersion(context.Background(), db, time.Unix(1_700_000_000, 0), 15, ahDispersionDefaultMinItems, "spread", nil, nil)
	if err != nil {
		t.Fatalf("collectAhItemPriceDispersion: %v", err)
	}
	items, ok := out["items"].([]*ahDispersionItem)
	if !ok {
		t.Fatalf("items type: %T want []*ahDispersionItem", out["items"])
	}
	if items == nil || len(items) != 0 {
		t.Errorf("items: %v want non-nil empty slice", items)
	}
	if di, _ := out["displayedItems"].(int); di != 0 {
		t.Errorf("displayedItems: %v want 0", out["displayedItems"])
	}
	totals, _ := out["totals"].(map[string]any)
	if v, _ := totals["marketSpreadPct"].(float64); v != 0 {
		t.Errorf("totals.marketSpreadPct: %v want 0 (div-guard)", totals["marketSpreadPct"])
	}
	if v, _ := totals["marketMinGold"].(string); v != "0c" {
		t.Errorf("totals.marketMinGold: %v want 0c", totals["marketMinGold"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}
