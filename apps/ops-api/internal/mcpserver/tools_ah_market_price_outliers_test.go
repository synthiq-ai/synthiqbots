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

// ahOutlierRowCols is the per-listing column set ah_market_price_outliers scans,
// in order.
func ahOutlierRowCols() []string {
	return []string{"id", "entry", "name", "Quality", "houseid", "itemowner", "buyoutprice"}
}

// TestAhMarketPriceOutliers_DescriptionMentionsContext guards the keywords that
// steer the agent here vs ah_market_price_percentiles (distribution) or a
// hand-written db_query — the outlier/typo/scam + median-baseline vocabulary is
// load-bearing for selection.
func TestAhMarketPriceOutliers_DescriptionMentionsContext(t *testing.T) {
	reg := NewRegistry()
	RegisterAhMarketPriceOutliersTool(reg, DBDeps{})
	tool, ok := reg.Get("ah_market_price_outliers")
	if !ok {
		t.Fatal("ah_market_price_outliers not registered")
	}
	for _, kw := range []string{
		"acore_characters.auctionhouse", "item_instance", "acore_world.item_template",
		"ah_market_price_percentiles", "ah_market_top_sellers", "wow_player_lookup", "ops_ro",
		"median", "p50", "outlier", "typo", "scam", "ratio", "minRatio", "buyoutprice",
		"auctionId", "sellerGuid", "qualityName", "Heirloom", "houseId", "minQuality",
		"itemEntry", "minListings", "topN", "Alliance", "Horde", "Neutral", "Read-only",
	} {
		if !strings.Contains(tool.Description, kw) {
			t.Errorf("description missing %q: %s", kw, tool.Description)
		}
	}
}

// TestAhMarketPriceOutliers_ReadOnlyAnnotation pins readOnlyHint=true so a future
// copy-paste of a destructive sibling can't silently flip the gate.
func TestAhMarketPriceOutliers_ReadOnlyAnnotation(t *testing.T) {
	reg := NewRegistry()
	RegisterAhMarketPriceOutliersTool(reg, DBDeps{})
	tool, _ := reg.Get("ah_market_price_outliers")
	if !strings.Contains(string(tool.Annotations), `"readOnlyHint":true`) {
		t.Errorf("annotations missing readOnlyHint=true: %s", tool.Annotations)
	}
}

func TestAhMarketPriceOutliers_NoPoolReturnsError(t *testing.T) {
	reg := NewRegistry()
	RegisterAhMarketPriceOutliersTool(reg, DBDeps{}) // QueryDB nil
	tool, _ := reg.Get("ah_market_price_outliers")
	resp := tool.Handler(context.Background(), json.RawMessage(`{}`), "")
	got, _ := resp.(map[string]any)
	if msg, _ := got["error"].(string); !strings.Contains(msg, "ops_ro pool not configured") {
		t.Errorf("expected pool-not-configured error, got %v", got)
	}
}

// TestAhMarketPriceOutliers_TopNClamps drives the handler and asserts the
// post-clamp topN echo. topN trims the Go-side result list, NOT the SQL LIMIT
// (which always binds the scan cap), so the bound arg is scanCap+1 in every case.
func TestAhMarketPriceOutliers_TopNClamps(t *testing.T) {
	cases := []struct {
		name     string
		args     string
		wantTopN int
	}{
		{"unset uses default", `{}`, ahOutlierDefaultTopN},
		{"zero falls back to default", `{"topN":0}`, ahOutlierDefaultTopN},
		{"negative falls back to default", `{"topN":-7}`, ahOutlierDefaultTopN},
		{"oversized clamps to max", `{"topN":99999}`, ahOutlierMaxTopN},
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
				WithArgs(int64(ahOutlierScanCap + 1)).
				WillReturnRows(sqlmock.NewRows(ahOutlierRowCols()))

			reg := NewRegistry()
			RegisterAhMarketPriceOutliersTool(reg, DBDeps{QueryDB: db})
			tool, _ := reg.Get("ah_market_price_outliers")
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

// TestAhMarketPriceOutliers_MinListingsClamps drives the handler and asserts the
// post-clamp minListings echo: unset/zero -> default 5, an explicit value below
// the floor -> 2, in-range passes through.
func TestAhMarketPriceOutliers_MinListingsClamps(t *testing.T) {
	cases := []struct {
		name    string
		args    string
		wantMin int
	}{
		{"unset uses default", `{}`, ahOutlierDefaultMinListings},
		{"zero falls back to default", `{"minListings":0}`, ahOutlierDefaultMinListings},
		{"one clamps to floor", `{"minListings":1}`, ahOutlierMinListingsFloor},
		{"in-range passes through", `{"minListings":8}`, 8},
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
				WithArgs(int64(ahOutlierScanCap + 1)).
				WillReturnRows(sqlmock.NewRows(ahOutlierRowCols()))

			reg := NewRegistry()
			RegisterAhMarketPriceOutliersTool(reg, DBDeps{QueryDB: db})
			tool, _ := reg.Get("ah_market_price_outliers")
			resp := tool.Handler(context.Background(), json.RawMessage(c.args), "")
			got, _ := resp.(map[string]any)
			if got["error"] != nil {
				t.Fatalf("unexpected error: %v", got)
			}
			if n, _ := got["minListings"].(int); n != c.wantMin {
				t.Errorf("minListings: %v want %d", got["minListings"], c.wantMin)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Errorf("unmet mock expectations: %v", err)
			}
		})
	}
}

// TestAhMarketPriceOutliers_MinRatioRejected — minRatio at or below 1.0 is
// rejected (no DB call) so a degenerate threshold can't flag the median itself
// and everything above it. QueryDB is non-nil to prove the reject fires AFTER
// the pool check but before any query.
func TestAhMarketPriceOutliers_MinRatioRejected(t *testing.T) {
	for _, bad := range []string{`{"minRatio":1}`, `{"minRatio":1.0}`, `{"minRatio":0.5}`, `{"minRatio":-2}`} {
		db, mock, err := sqlmock.New()
		if err != nil {
			t.Fatal(err)
		}
		reg := NewRegistry()
		RegisterAhMarketPriceOutliersTool(reg, DBDeps{QueryDB: db})
		tool, _ := reg.Get("ah_market_price_outliers")
		resp := tool.Handler(context.Background(), json.RawMessage(bad), "")
		got, _ := resp.(map[string]any)
		if msg, _ := got["error"].(string); !strings.Contains(msg, "minRatio must be greater than 1.0") {
			t.Errorf("args %s: expected minRatio reject, got %v", bad, got)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("args %s: no query should run on reject: %v", bad, err)
		}
		db.Close()
	}
}

// TestAhMarketPriceOutliers_MinQualityRejected — out-of-range minQuality is
// rejected (no DB call) so a typo can't masquerade as an empty market.
func TestAhMarketPriceOutliers_MinQualityRejected(t *testing.T) {
	for _, bad := range []string{`{"minQuality":-1}`, `{"minQuality":8}`, `{"minQuality":99}`} {
		db, mock, err := sqlmock.New()
		if err != nil {
			t.Fatal(err)
		}
		reg := NewRegistry()
		RegisterAhMarketPriceOutliersTool(reg, DBDeps{QueryDB: db})
		tool, _ := reg.Get("ah_market_price_outliers")
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

// TestAhMarketPriceOutliers_Filters drives the handler with each filter
// combination and asserts the WHERE-clause shape (extra filters AND-ed onto the
// base buyoutprice>0 predicate), the bound args (SQL filters then the scan-cap
// LIMIT — minRatio/minListings are applied in Go and never hit SQL), and the
// echoed houseId/faction/minQuality/itemEntry keys.
func TestAhMarketPriceOutliers_Filters(t *testing.T) {
	cap1 := i(ahOutlierScanCap + 1)
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
			args:      `{"houseId":2,"minQuality":4,"itemEntry":777,"topN":5,"minRatio":4,"minListings":3}`,
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
				WillReturnRows(sqlmock.NewRows(ahOutlierRowCols()))

			reg := NewRegistry()
			RegisterAhMarketPriceOutliersTool(reg, DBDeps{QueryDB: db})
			tool, _ := reg.Get("ah_market_price_outliers")
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

// ahOutlierGoldenRows is the shared fixture. Rows arrive entry-ascending, matching
// the query's ORDER BY it.entry:
//   - 4306 "Silk Cloth" (q1, Neutral house 7): five 100c listings + one 400c + one
//     5000c -> median 100, so 400c=4x (medium) and 5000c=50x (high) are outliers.
//   - 22445 "Arcane Dust" (q2, Alliance house 2): five identical 10000c listings ->
//     median 10000, no outlier (evaluated-but-clean).
//   - 49908 "Primordial Saronite" (q4, Horde house 6): two listings 50000c/200000c
//     -> below the default minListings=5 floor (scanned but not evaluated); becomes
//     a 4x outlier only once minListings drops to 2.
func ahOutlierGoldenRows() *sqlmock.Rows {
	return sqlmock.NewRows(ahOutlierRowCols()).
		AddRow(int64(1), int64(4306), "Silk Cloth", 1, int64(7), int64(1001), int64(100)).
		AddRow(int64(2), int64(4306), "Silk Cloth", 1, int64(7), int64(1001), int64(100)).
		AddRow(int64(3), int64(4306), "Silk Cloth", 1, int64(7), int64(1001), int64(100)).
		AddRow(int64(4), int64(4306), "Silk Cloth", 1, int64(7), int64(1001), int64(100)).
		AddRow(int64(5), int64(4306), "Silk Cloth", 1, int64(7), int64(1001), int64(100)).
		AddRow(int64(6), int64(4306), "Silk Cloth", 1, int64(7), int64(2002), int64(400)).
		AddRow(int64(7), int64(4306), "Silk Cloth", 1, int64(7), int64(3003), int64(5000)).
		AddRow(int64(10), int64(22445), "Arcane Dust", 2, int64(2), int64(4004), int64(10000)).
		AddRow(int64(11), int64(22445), "Arcane Dust", 2, int64(2), int64(4004), int64(10000)).
		AddRow(int64(12), int64(22445), "Arcane Dust", 2, int64(2), int64(4004), int64(10000)).
		AddRow(int64(13), int64(22445), "Arcane Dust", 2, int64(2), int64(4004), int64(10000)).
		AddRow(int64(14), int64(22445), "Arcane Dust", 2, int64(2), int64(4004), int64(10000)).
		AddRow(int64(20), int64(49908), "Primordial Saronite", 4, int64(6), int64(5005), int64(50000)).
		AddRow(int64(21), int64(49908), "Primordial Saronite", 4, int64(6), int64(6006), int64(200000))
}

// TestCollectAhPriceOutliers_Golden folds the shared fixture at the default
// thresholds (minRatio 3, minListings 5) and verifies the median-baseline
// flagging, the worst-first sort, severity classification, the human gold
// strings, the per-item listing count, and the evaluated/scanned counters.
func TestCollectAhPriceOutliers_Golden(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	now := time.Unix(1_700_000_000, 0)
	mock.ExpectExec(regexp.QuoteMeta("USE `acore_characters`")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta("ORDER BY it.entry ASC LIMIT ?")).
		WithArgs(int64(ahOutlierScanCap + 1)).
		WillReturnRows(ahOutlierGoldenRows())

	out, err := collectAhPriceOutliers(context.Background(), db, now, 50, 5, 3.0, nil, nil, nil)
	if err != nil {
		t.Fatalf("collectAhPriceOutliers: %v", err)
	}
	if sl, _ := out["scannedListings"].(int); sl != 14 {
		t.Errorf("scannedListings: %v want 14", out["scannedListings"])
	}
	if ei, _ := out["evaluatedItems"].(int); ei != 2 {
		t.Errorf("evaluatedItems: %v want 2 (4306 + 22445; 49908 below floor)", out["evaluatedItems"])
	}
	if el, _ := out["evaluatedListings"].(int); el != 12 {
		t.Errorf("evaluatedListings: %v want 12 (7 + 5)", out["evaluatedListings"])
	}
	if oc, _ := out["outlierCount"].(int); oc != 2 {
		t.Errorf("outlierCount: %v want 2", out["outlierCount"])
	}
	if mr, _ := out["minRatio"].(float64); mr != 3.0 {
		t.Errorf("minRatio echo: %v want 3.0", out["minRatio"])
	}
	if tr, _ := out["truncated"].(bool); tr {
		t.Errorf("truncated: %v want false", out["truncated"])
	}
	if _, ok := out["houseId"]; ok {
		t.Errorf("houseId should be absent with no filter, got %v", out["houseId"])
	}

	outliers, ok := out["outliers"].([]*ahPriceOutlier)
	if !ok || len(outliers) != 2 {
		t.Fatalf("outliers: %T len %d want []*ahPriceOutlier len 2", out["outliers"], len(outliers))
	}

	// Sorted ratio desc: the 50x high outlier first, then the 4x medium.
	hi := outliers[0]
	if hi.AuctionID != 7 || hi.ItemEntry != 4306 || hi.ItemName != "Silk Cloth" || hi.Quality != 1 || hi.QualityName != "Common" {
		t.Errorf("outliers[0] identity: %+v", hi)
	}
	if hi.HouseID != 7 || hi.Faction != "Neutral" || hi.SellerGuid != 3003 {
		t.Errorf("outliers[0] house/seller: %+v", hi)
	}
	if hi.ListingBuyoutCopper != 5000 || hi.ListingBuyoutGold != "50s" {
		t.Errorf("outliers[0] listing: %+v", hi)
	}
	if hi.MedianBuyoutCopper != 100 || hi.MedianBuyoutGold != "1s" {
		t.Errorf("outliers[0] median: %+v", hi)
	}
	if hi.Ratio != 50.0 || hi.Severity != "high" || hi.ListingsForItem != 7 {
		t.Errorf("outliers[0] ratio/severity: %+v", hi)
	}
	if !strings.Contains(hi.Reason, "typo or scam") {
		t.Errorf("outliers[0] reason: %q", hi.Reason)
	}

	med := outliers[1]
	if med.AuctionID != 6 || med.SellerGuid != 2002 {
		t.Errorf("outliers[1] identity: %+v", med)
	}
	if med.ListingBuyoutCopper != 400 || med.ListingBuyoutGold != "4s" {
		t.Errorf("outliers[1] listing: %+v", med)
	}
	if med.MedianBuyoutCopper != 100 || med.Ratio != 4.0 || med.Severity != "medium" {
		t.Errorf("outliers[1] ratio/severity: %+v", med)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestCollectAhPriceOutliers_MinListingsFloor drops minListings to the floor (2)
// so the 2-listing item 49908 is now evaluated, adding a third 4x medium outlier;
// it also proves the ratio-desc sort with the auctionId-asc tiebreak between the
// two equal 4x outliers (auction 6 from 4306 before auction 21 from 49908).
func TestCollectAhPriceOutliers_MinListingsFloor(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.ExpectExec(regexp.QuoteMeta("USE `acore_characters`")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta("ORDER BY it.entry ASC LIMIT ?")).
		WithArgs(int64(ahOutlierScanCap + 1)).
		WillReturnRows(ahOutlierGoldenRows())

	out, err := collectAhPriceOutliers(context.Background(), db, time.Unix(1_700_000_000, 0), 50, 2, 3.0, nil, nil, nil)
	if err != nil {
		t.Fatalf("collectAhPriceOutliers: %v", err)
	}
	if ei, _ := out["evaluatedItems"].(int); ei != 3 {
		t.Errorf("evaluatedItems: %v want 3 (49908 now clears the floor)", out["evaluatedItems"])
	}
	if el, _ := out["evaluatedListings"].(int); el != 14 {
		t.Errorf("evaluatedListings: %v want 14", out["evaluatedListings"])
	}
	if oc, _ := out["outlierCount"].(int); oc != 3 {
		t.Errorf("outlierCount: %v want 3", out["outlierCount"])
	}
	outliers, _ := out["outliers"].([]*ahPriceOutlier)
	if len(outliers) != 3 {
		t.Fatalf("outliers len %d want 3", len(outliers))
	}
	// ratio desc: 50x (auction 7) first; then two 4x tied, auctionId asc -> 6 before 21.
	if outliers[0].AuctionID != 7 {
		t.Errorf("outliers[0] auctionId %d want 7", outliers[0].AuctionID)
	}
	if outliers[1].AuctionID != 6 || outliers[2].AuctionID != 21 {
		t.Errorf("tiebreak: got auctionIds %d,%d want 6,21", outliers[1].AuctionID, outliers[2].AuctionID)
	}
	if outliers[2].ItemEntry != 49908 || outliers[2].Ratio != 4.0 || outliers[2].Severity != "medium" {
		t.Errorf("outliers[2]: %+v", outliers[2])
	}
}

// TestCollectAhPriceOutliers_TopNHonest — topN trims the returned list while
// outlierCount stays the honest pre-trim total.
func TestCollectAhPriceOutliers_TopNHonest(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.ExpectExec(regexp.QuoteMeta("USE `acore_characters`")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta("ORDER BY it.entry ASC LIMIT ?")).
		WithArgs(int64(ahOutlierScanCap + 1)).
		WillReturnRows(ahOutlierGoldenRows())

	out, err := collectAhPriceOutliers(context.Background(), db, time.Unix(1_700_000_000, 0), 1, 5, 3.0, nil, nil, nil)
	if err != nil {
		t.Fatalf("collectAhPriceOutliers: %v", err)
	}
	if oc, _ := out["outlierCount"].(int); oc != 2 {
		t.Errorf("outlierCount: %v want 2 (honest pre-trim)", out["outlierCount"])
	}
	outliers, _ := out["outliers"].([]*ahPriceOutlier)
	if len(outliers) != 1 || outliers[0].AuctionID != 7 {
		t.Fatalf("topN=1 should keep only the worst (auction 7), got %+v", outliers)
	}
}

// TestCollectAhPriceOutliers_Empty — an empty market yields a non-nil empty
// outliers slice, zeroed counters, and truncated false.
func TestCollectAhPriceOutliers_Empty(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.ExpectExec(regexp.QuoteMeta("USE `acore_characters`")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta("ORDER BY it.entry ASC LIMIT ?")).
		WithArgs(int64(ahOutlierScanCap + 1)).
		WillReturnRows(sqlmock.NewRows(ahOutlierRowCols()))

	out, err := collectAhPriceOutliers(context.Background(), db, time.Unix(1_700_000_000, 0), 50, 5, 3.0, nil, nil, nil)
	if err != nil {
		t.Fatalf("collectAhPriceOutliers: %v", err)
	}
	outliers, ok := out["outliers"].([]*ahPriceOutlier)
	if !ok {
		t.Fatalf("outliers type: %T want []*ahPriceOutlier", out["outliers"])
	}
	if outliers == nil || len(outliers) != 0 {
		t.Errorf("outliers: %v want non-nil empty slice", outliers)
	}
	if oc, _ := out["outlierCount"].(int); oc != 0 {
		t.Errorf("outlierCount: %v want 0", out["outlierCount"])
	}
	if ei, _ := out["evaluatedItems"].(int); ei != 0 {
		t.Errorf("evaluatedItems: %v want 0", out["evaluatedItems"])
	}
	if sl, _ := out["scannedListings"].(int); sl != 0 {
		t.Errorf("scannedListings: %v want 0", out["scannedListings"])
	}
	if tr, _ := out["truncated"].(bool); tr {
		t.Errorf("truncated: %v want false", out["truncated"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestClassifyPriceOutlier covers the high/medium boundary and the minRatio-scaled
// cutoff (when minRatio exceeds the absolute danger line, everything flagged is high).
func TestClassifyPriceOutlier(t *testing.T) {
	cases := []struct {
		ratio    float64
		minRatio float64
		want     string
	}{
		{50, 3, "high"},
		{10, 3, "high"}, // boundary: >= 10x is high
		{9.99, 3, "medium"},
		{4, 3, "medium"},
		{25, 20, "high"},   // cutoff scales to minRatio=20
		{15, 20, "medium"}, // below the scaled cutoff
	}
	for _, c := range cases {
		if got := classifyPriceOutlier(c.ratio, c.minRatio); got != c.want {
			t.Errorf("classifyPriceOutlier(%g, %g) = %q want %q", c.ratio, c.minRatio, got, c.want)
		}
	}
}

// TestRoundOutlierRatio checks two-decimal rounding on exact and fractional inputs.
func TestRoundOutlierRatio(t *testing.T) {
	if r := roundOutlierRatio(50.0); r != 50.0 {
		t.Errorf("roundOutlierRatio(50.0) = %g want 50", r)
	}
	if r := roundOutlierRatio(4.0); r != 4.0 {
		t.Errorf("roundOutlierRatio(4.0) = %g want 4", r)
	}
	if r := roundOutlierRatio(3.126); r <= 3.12 || r >= 3.14 {
		t.Errorf("roundOutlierRatio(3.126) = %g want ~3.13", r)
	}
}
