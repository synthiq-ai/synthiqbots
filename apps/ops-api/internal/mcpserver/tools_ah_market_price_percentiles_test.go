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

// ahPricePctRowCols is the per-listing column set ah_market_price_percentiles
// scans, in order.
func ahPricePctRowCols() []string {
	return []string{"entry", "name", "Quality", "buyoutprice"}
}

// TestAhMarketPricePercentiles_DescriptionMentionsContext guards the keywords
// that steer the agent here vs ah_market_top_items (ranking/avg) or a
// hand-written db_query — the distribution/percentile vocabulary + cross-DB join
// framing is load-bearing for selection.
func TestAhMarketPricePercentiles_DescriptionMentionsContext(t *testing.T) {
	reg := NewRegistry()
	RegisterAhMarketPricePercentilesTool(reg, DBDeps{})
	tool, ok := reg.Get("ah_market_price_percentiles")
	if !ok {
		t.Fatal("ah_market_price_percentiles not registered")
	}
	for _, kw := range []string{
		"acore_characters.auctionhouse", "item_instance", "acore_world.item_template",
		"ah_market_top_items", "ops_ro", "nearest-rank", "median", "p50", "p90", "p95",
		"distribution", "buyoutprice", "qualityName", "Heirloom", "houseId", "minQuality",
		"itemEntry", "minListings", "topN", "Alliance", "Horde", "Neutral", "Read-only",
	} {
		if !strings.Contains(tool.Description, kw) {
			t.Errorf("description missing %q: %s", kw, tool.Description)
		}
	}
}

// TestAhMarketPricePercentiles_ReadOnlyAnnotation pins readOnlyHint=true so a
// future copy-paste of a destructive sibling can't silently flip the gate.
func TestAhMarketPricePercentiles_ReadOnlyAnnotation(t *testing.T) {
	reg := NewRegistry()
	RegisterAhMarketPricePercentilesTool(reg, DBDeps{})
	tool, _ := reg.Get("ah_market_price_percentiles")
	if !strings.Contains(string(tool.Annotations), `"readOnlyHint":true`) {
		t.Errorf("annotations missing readOnlyHint=true: %s", tool.Annotations)
	}
}

func TestAhMarketPricePercentiles_NoPoolReturnsError(t *testing.T) {
	reg := NewRegistry()
	RegisterAhMarketPricePercentilesTool(reg, DBDeps{}) // QueryDB nil
	tool, _ := reg.Get("ah_market_price_percentiles")
	resp := tool.Handler(context.Background(), json.RawMessage(`{}`), "")
	got, _ := resp.(map[string]any)
	if msg, _ := got["error"].(string); !strings.Contains(msg, "ops_ro pool not configured") {
		t.Errorf("expected pool-not-configured error, got %v", got)
	}
}

// TestAhMarketPricePercentiles_TopNClamps drives the handler and asserts the
// post-clamp topN echo (unset->default, zero/negative->default, oversized->max,
// in-range passthrough). topN trims the Go-side result list, NOT the SQL LIMIT
// (which always binds the scan cap), so the bound arg is scanCap+1 in every case.
func TestAhMarketPricePercentiles_TopNClamps(t *testing.T) {
	cases := []struct {
		name     string
		args     string
		wantTopN int
	}{
		{"unset uses default", `{}`, ahPricePctDefaultTopN},
		{"zero falls back to default", `{"topN":0}`, ahPricePctDefaultTopN},
		{"negative falls back to default", `{"topN":-7}`, ahPricePctDefaultTopN},
		{"oversized clamps to max", `{"topN":99999}`, ahPricePctMaxTopN},
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
			mock.ExpectQuery(regexp.QuoteMeta("ORDER BY it.entry ASC LIMIT ?")).
				WithArgs(int64(ahPricePctScanCap + 1)).
				WillReturnRows(sqlmock.NewRows(ahPricePctRowCols()))

			reg := NewRegistry()
			RegisterAhMarketPricePercentilesTool(reg, DBDeps{QueryDB: db})
			tool, _ := reg.Get("ah_market_price_percentiles")
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

// TestAhMarketPricePercentiles_MinQualityRejected — out-of-range minQuality is
// rejected (no DB call) so a typo can't masquerade as an empty market. QueryDB
// is non-nil to prove the reject fires AFTER the pool check but before any query.
func TestAhMarketPricePercentiles_MinQualityRejected(t *testing.T) {
	for _, bad := range []string{`{"minQuality":-1}`, `{"minQuality":8}`, `{"minQuality":99}`} {
		db, mock, err := sqlmock.New()
		if err != nil {
			t.Fatal(err)
		}
		reg := NewRegistry()
		RegisterAhMarketPricePercentilesTool(reg, DBDeps{QueryDB: db})
		tool, _ := reg.Get("ah_market_price_percentiles")
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

// TestAhMarketPricePercentiles_Filters drives the handler with each filter
// combination and asserts the WHERE-clause shape (extra filters AND-ed onto the
// base buyoutprice>0 predicate), the bound args (filter values then the scan-cap
// LIMIT), and the echoed houseId/faction/minQuality/itemEntry keys.
func TestAhMarketPricePercentiles_Filters(t *testing.T) {
	cap1 := i(ahPricePctScanCap + 1)
	cases := []struct {
		name        string
		args        string
		whereSub    string
		wantArgs    []driverArg
		wantHouse   int // -1 = key absent
		wantFaction string
		wantMinQ    int // -1 = key absent
		wantEntry   int // -1 = key absent
	}{
		{
			name:      "house only",
			args:      `{"houseId":7}`,
			whereSub:  "WHERE ah.buyoutprice > 0 AND ah.houseid = ? ORDER BY",
			wantArgs:  []driverArg{i(7), cap1},
			wantHouse: 7, wantFaction: "Neutral", wantMinQ: -1, wantEntry: -1,
		},
		{
			name:      "quality only",
			args:      `{"minQuality":3}`,
			whereSub:  "WHERE ah.buyoutprice > 0 AND it.Quality >= ? ORDER BY",
			wantArgs:  []driverArg{i(3), cap1},
			wantHouse: -1, wantMinQ: 3, wantEntry: -1,
		},
		{
			name:      "itemEntry only",
			args:      `{"itemEntry":4306}`,
			whereSub:  "WHERE ah.buyoutprice > 0 AND it.entry = ? ORDER BY",
			wantArgs:  []driverArg{i(4306), cap1},
			wantHouse: -1, wantMinQ: -1, wantEntry: 4306,
		},
		{
			name:      "all filters",
			args:      `{"houseId":2,"minQuality":4,"itemEntry":777,"topN":5,"minListings":2}`,
			whereSub:  "WHERE ah.buyoutprice > 0 AND ah.houseid = ? AND it.Quality >= ? AND it.entry = ? ORDER BY",
			wantArgs:  []driverArg{i(2), i(4), i(777), cap1},
			wantHouse: 2, wantFaction: "Alliance", wantMinQ: 4, wantEntry: 777,
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
				WithArgs(toValues(c.wantArgs)...).
				WillReturnRows(sqlmock.NewRows(ahPricePctRowCols()))

			reg := NewRegistry()
			RegisterAhMarketPricePercentilesTool(reg, DBDeps{QueryDB: db})
			tool, _ := reg.Get("ah_market_price_percentiles")
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
			if c.wantEntry >= 0 {
				if e, _ := got["itemEntry"].(int); e != c.wantEntry {
					t.Errorf("itemEntry: %v want %d", got["itemEntry"], c.wantEntry)
				}
			} else if _, ok := got["itemEntry"]; ok {
				t.Errorf("itemEntry should be absent, got %v", got["itemEntry"])
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Errorf("unmet mock expectations: %v", err)
			}
		})
	}
}

// ahPricePctGoldenRows is the shared fixture: item 4306 (5 buyout listings,
// added out of order to prove the Go-side ascending sort), item 22445 (2), item
// 49908 (1). Rows arrive entry-ascending, matching the query's ORDER BY it.entry.
func ahPricePctGoldenRows() *sqlmock.Rows {
	return sqlmock.NewRows(ahPricePctRowCols()).
		AddRow(int64(4306), "Silk Cloth", 1, int64(300)).
		AddRow(int64(4306), "Silk Cloth", 1, int64(100)).
		AddRow(int64(4306), "Silk Cloth", 1, int64(500)).
		AddRow(int64(4306), "Silk Cloth", 1, int64(200)).
		AddRow(int64(4306), "Silk Cloth", 1, int64(400)).
		AddRow(int64(22445), "Arcane Dust", 2, int64(20000)).
		AddRow(int64(22445), "Arcane Dust", 2, int64(10000)).
		AddRow(int64(49908), "Primordial Saronite", 4, int64(50000))
}

// TestCollectAhPricePercentiles_Golden folds the shared fixture and verifies the
// nearest-rank percentile math (p50/p90/p95), min/max from the sorted slice, the
// avg-over-count division, qualityName mapping, the human gold strings, the
// listings-desc sort, scannedListings, distinctItems, and truncated=false.
func TestCollectAhPricePercentiles_Golden(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	now := time.Unix(1_700_000_000, 0)
	mock.ExpectExec(regexp.QuoteMeta("USE `acore_characters`")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta("ORDER BY it.entry ASC LIMIT ?")).
		WithArgs(int64(ahPricePctScanCap + 1)).
		WillReturnRows(ahPricePctGoldenRows())

	out, err := collectAhPricePercentiles(context.Background(), db, now, 15, 1, nil, nil, nil)
	if err != nil {
		t.Fatalf("collectAhPricePercentiles: %v", err)
	}
	if sl, _ := out["scannedListings"].(int); sl != 8 {
		t.Errorf("scannedListings: %v want 8", out["scannedListings"])
	}
	if di, _ := out["distinctItems"].(int); di != 3 {
		t.Errorf("distinctItems: %v want 3", out["distinctItems"])
	}
	if tr, _ := out["truncated"].(bool); tr {
		t.Errorf("truncated: %v want false", out["truncated"])
	}
	if _, ok := out["houseId"]; ok {
		t.Errorf("houseId should be absent with no filter, got %v", out["houseId"])
	}
	items, ok := out["items"].([]*ahPricePctItem)
	if !ok || len(items) != 3 {
		t.Fatalf("items: %T len %d want []*ahPricePctItem len 3", out["items"], len(items))
	}

	// Sorted listings-desc: 4306 (5) > 22445 (2) > 49908 (1).
	a := items[0]
	if a.ItemEntry != 4306 || a.ItemName != "Silk Cloth" || a.Quality != 1 || a.QualityName != "Common" {
		t.Errorf("items[0] identity: %+v", a)
	}
	if a.Listings != 5 {
		t.Errorf("items[0] listings: %d want 5", a.Listings)
	}
	if a.MinBuyoutCopper != 100 || a.MinBuyoutGold != "1s" {
		t.Errorf("items[0] min: %+v", a)
	}
	if a.P50BuyoutCopper != 300 || a.P50BuyoutGold != "3s" {
		t.Errorf("items[0] p50: %+v", a)
	}
	if a.P90BuyoutCopper != 500 || a.P90BuyoutGold != "5s" {
		t.Errorf("items[0] p90: %+v", a)
	}
	if a.P95BuyoutCopper != 500 || a.P95BuyoutGold != "5s" {
		t.Errorf("items[0] p95: %+v", a)
	}
	if a.MaxBuyoutCopper != 500 || a.MaxBuyoutGold != "5s" {
		t.Errorf("items[0] max: %+v", a)
	}
	if a.AvgBuyoutCopper != 300 || a.AvgBuyoutGold != "3s" {
		t.Errorf("items[0] avg: %+v", a)
	}

	b := items[1]
	if b.ItemEntry != 22445 || b.QualityName != "Uncommon" || b.Listings != 2 {
		t.Errorf("items[1] identity: %+v", b)
	}
	if b.MinBuyoutCopper != 10000 || b.MinBuyoutGold != "1g" {
		t.Errorf("items[1] min: %+v", b)
	}
	// n=2: p50 -> rank 1 (10000); p90/p95 -> rank 2 (20000).
	if b.P50BuyoutCopper != 10000 || b.P50BuyoutGold != "1g" {
		t.Errorf("items[1] p50: %+v", b)
	}
	if b.P90BuyoutCopper != 20000 || b.P90BuyoutGold != "2g" {
		t.Errorf("items[1] p90: %+v", b)
	}
	if b.P95BuyoutCopper != 20000 || b.P95BuyoutGold != "2g" {
		t.Errorf("items[1] p95: %+v", b)
	}
	if b.MaxBuyoutCopper != 20000 || b.AvgBuyoutCopper != 15000 || b.AvgBuyoutGold != "1g50s" {
		t.Errorf("items[1] max/avg: %+v", b)
	}

	c := items[2]
	if c.ItemEntry != 49908 || c.QualityName != "Epic" || c.Listings != 1 {
		t.Errorf("items[2] identity: %+v", c)
	}
	// Single sample: every statistic collapses to the lone price.
	if c.MinBuyoutCopper != 50000 || c.P50BuyoutCopper != 50000 || c.P90BuyoutCopper != 50000 ||
		c.P95BuyoutCopper != 50000 || c.MaxBuyoutCopper != 50000 || c.AvgBuyoutCopper != 50000 {
		t.Errorf("items[2] single-sample stats: %+v", c)
	}
	if c.P50BuyoutGold != "5g" {
		t.Errorf("items[2] gold: %+v", c)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestCollectAhPricePercentiles_MinListings — a minListings floor suppresses
// thin-sample items (and drops them from distinctItems) so the reported
// distributions are statistically meaningful. With minListings=3 only the
// 5-listing item survives the 5/2/1 fixture.
func TestCollectAhPricePercentiles_MinListings(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.ExpectExec(regexp.QuoteMeta("USE `acore_characters`")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta("ORDER BY it.entry ASC LIMIT ?")).
		WithArgs(int64(ahPricePctScanCap + 1)).
		WillReturnRows(ahPricePctGoldenRows())

	out, err := collectAhPricePercentiles(context.Background(), db, time.Unix(1_700_000_000, 0), 15, 3, nil, nil, nil)
	if err != nil {
		t.Fatalf("collectAhPricePercentiles: %v", err)
	}
	if ml, _ := out["minListings"].(int); ml != 3 {
		t.Errorf("minListings echo: %v want 3", out["minListings"])
	}
	if di, _ := out["distinctItems"].(int); di != 1 {
		t.Errorf("distinctItems: %v want 1 (only the 5-listing item clears minListings=3)", out["distinctItems"])
	}
	// scannedListings counts every folded row regardless of the minListings filter.
	if sl, _ := out["scannedListings"].(int); sl != 8 {
		t.Errorf("scannedListings: %v want 8", out["scannedListings"])
	}
	items, _ := out["items"].([]*ahPricePctItem)
	if len(items) != 1 || items[0].ItemEntry != 4306 {
		t.Fatalf("items: want exactly entry 4306, got %+v", items)
	}
}

// TestCollectAhPricePercentiles_Empty — an empty market yields a non-nil empty
// items slice (callers expect an array), distinctItems 0, and truncated false.
func TestCollectAhPricePercentiles_Empty(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.ExpectExec(regexp.QuoteMeta("USE `acore_characters`")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta("ORDER BY it.entry ASC LIMIT ?")).
		WithArgs(int64(ahPricePctScanCap + 1)).
		WillReturnRows(sqlmock.NewRows(ahPricePctRowCols()))

	out, err := collectAhPricePercentiles(context.Background(), db, time.Unix(1_700_000_000, 0), 15, 1, nil, nil, nil)
	if err != nil {
		t.Fatalf("collectAhPricePercentiles: %v", err)
	}
	items, ok := out["items"].([]*ahPricePctItem)
	if !ok {
		t.Fatalf("items type: %T want []*ahPricePctItem", out["items"])
	}
	if items == nil || len(items) != 0 {
		t.Errorf("items: %v want non-nil empty slice", items)
	}
	if di, _ := out["distinctItems"].(int); di != 0 {
		t.Errorf("distinctItems: %v want 0", out["distinctItems"])
	}
	if tr, _ := out["truncated"].(bool); tr {
		t.Errorf("truncated: %v want false", out["truncated"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}
