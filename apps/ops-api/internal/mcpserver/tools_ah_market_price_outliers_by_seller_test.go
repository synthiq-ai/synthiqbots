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

// expectEmptyOutlierBySellerScan queues the USE + buyout scan with no rows. Used
// by the arg-clamp / reject tests where the fold is empty (no outliers -> no
// stage-2/3 identity queries fire, so only these two expectations are needed).
func expectEmptyOutlierBySellerScan(mock sqlmock.Sqlmock) {
	mock.ExpectExec(regexp.QuoteMeta("USE `acore_characters`")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta("ORDER BY it.entry ASC LIMIT ?")).
		WithArgs(int64(ahOutlierScanCap + 1)).
		WillReturnRows(sqlmock.NewRows(ahOutlierRowCols()))
}

// ahOutlierBySellerGoldenRows is the self-contained fixture. Rows arrive
// entry-ascending (matching the query's ORDER BY it.entry):
//   - 4306 "Silk Cloth" (q1, Neutral house 7): five 100c baselines (seller 9000)
//   - 600c (seller 1001) + 5000c (seller 1001) + 400c (seller 3003). Median 100,
//     so 600c=6x (medium), 5000c=50x (high), 400c=4x (medium) are outliers.
//   - 22445 "Arcane Dust" (q2, Alliance house 2): five 1000c baselines (seller
//     9000) + 8000c (seller 1001) + 12000c (seller 2002). Median 1000, so 8000c=8x
//     (medium) and 12000c=12x (high) are outliers.
//
// Net per seller: 1001 is the repeat offender (3 outliers across 2 items, one
// high); 2002 owns one 12x high; 3003 owns one 4x medium. Seller 9000 posts only
// baselines -> never an outlier, never surfaces.
func ahOutlierBySellerGoldenRows() *sqlmock.Rows {
	return sqlmock.NewRows(ahOutlierRowCols()).
		AddRow(int64(1), int64(4306), "Silk Cloth", 1, int64(7), int64(9000), int64(100)).
		AddRow(int64(2), int64(4306), "Silk Cloth", 1, int64(7), int64(9000), int64(100)).
		AddRow(int64(3), int64(4306), "Silk Cloth", 1, int64(7), int64(9000), int64(100)).
		AddRow(int64(4), int64(4306), "Silk Cloth", 1, int64(7), int64(9000), int64(100)).
		AddRow(int64(5), int64(4306), "Silk Cloth", 1, int64(7), int64(9000), int64(100)).
		AddRow(int64(6), int64(4306), "Silk Cloth", 1, int64(7), int64(1001), int64(600)).
		AddRow(int64(7), int64(4306), "Silk Cloth", 1, int64(7), int64(1001), int64(5000)).
		AddRow(int64(8), int64(4306), "Silk Cloth", 1, int64(7), int64(3003), int64(400)).
		AddRow(int64(10), int64(22445), "Arcane Dust", 2, int64(2), int64(9000), int64(1000)).
		AddRow(int64(11), int64(22445), "Arcane Dust", 2, int64(2), int64(9000), int64(1000)).
		AddRow(int64(12), int64(22445), "Arcane Dust", 2, int64(2), int64(9000), int64(1000)).
		AddRow(int64(13), int64(22445), "Arcane Dust", 2, int64(2), int64(9000), int64(1000)).
		AddRow(int64(14), int64(22445), "Arcane Dust", 2, int64(2), int64(9000), int64(1000)).
		AddRow(int64(15), int64(22445), "Arcane Dust", 2, int64(2), int64(1001), int64(8000)).
		AddRow(int64(16), int64(22445), "Arcane Dust", 2, int64(2), int64(2002), int64(12000))
}

// TestAhMarketPriceOutliersBySeller_DescriptionMentionsContext guards the
// keywords that steer the agent here (the per-seller / repeat-offender framing)
// vs ah_market_price_outliers (per listing) or ah_market_top_sellers (by volume).
func TestAhMarketPriceOutliersBySeller_DescriptionMentionsContext(t *testing.T) {
	reg := NewRegistry()
	RegisterAhMarketPriceOutliersBySellerTool(reg, DBDeps{})
	tool, ok := reg.Get("ah_market_price_outliers_by_seller")
	if !ok {
		t.Fatal("ah_market_price_outliers_by_seller not registered")
	}
	for _, kw := range []string{
		"acore_characters.auctionhouse", "item_instance", "acore_world.item_template",
		"acore_auth.account", "ah_market_price_outliers", "ah_market_top_sellers",
		"wow_player_lookup", "ops_ro", "median", "p50", "outlier", "repeat-offender",
		"typo", "scam", "minRatio", "minListings", "minOutlierListings", "sellerGuid",
		"outlierListings", "distinctItems", "totalMarkup", "maxRatio", "worstAuctionId",
		"RNDBOT", "isBot", "AHBot", "mod-ah-bot", "houseId", "minQuality", "topN",
		"unresolved", "Alliance", "Horde", "Neutral", "Read-only",
	} {
		if !strings.Contains(tool.Description, kw) {
			t.Errorf("description missing %q", kw)
		}
	}
}

// TestAhMarketPriceOutliersBySeller_ReadOnlyAnnotation pins readOnlyHint=true.
func TestAhMarketPriceOutliersBySeller_ReadOnlyAnnotation(t *testing.T) {
	reg := NewRegistry()
	RegisterAhMarketPriceOutliersBySellerTool(reg, DBDeps{})
	tool, _ := reg.Get("ah_market_price_outliers_by_seller")
	if !strings.Contains(string(tool.Annotations), `"readOnlyHint":true`) {
		t.Errorf("annotations missing readOnlyHint=true: %s", tool.Annotations)
	}
}

func TestAhMarketPriceOutliersBySeller_NoPoolReturnsError(t *testing.T) {
	reg := NewRegistry()
	RegisterAhMarketPriceOutliersBySellerTool(reg, DBDeps{}) // QueryDB nil
	tool, _ := reg.Get("ah_market_price_outliers_by_seller")
	resp := tool.Handler(context.Background(), json.RawMessage(`{}`), "")
	got, _ := resp.(map[string]any)
	if msg, _ := got["error"].(string); !strings.Contains(msg, "ops_ro pool not configured") {
		t.Errorf("expected pool-not-configured error, got %v", got)
	}
}

// TestAhMarketPriceOutliersBySeller_TopNClamps asserts the post-clamp topN echo.
// topN trims the Go-side seller list, NOT the SQL LIMIT (always scanCap+1).
func TestAhMarketPriceOutliersBySeller_TopNClamps(t *testing.T) {
	cases := []struct {
		name     string
		args     string
		wantTopN int
	}{
		{"unset uses default", `{}`, ahOutlierBySellerDefaultTopN},
		{"zero falls back to default", `{"topN":0}`, ahOutlierBySellerDefaultTopN},
		{"negative falls back to default", `{"topN":-9}`, ahOutlierBySellerDefaultTopN},
		{"oversized clamps to max", `{"topN":99999}`, ahOutlierBySellerMaxTopN},
		{"in-range passes through", `{"topN":40}`, 40},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			expectEmptyOutlierBySellerScan(mock)

			reg := NewRegistry()
			RegisterAhMarketPriceOutliersBySellerTool(reg, DBDeps{QueryDB: db})
			tool, _ := reg.Get("ah_market_price_outliers_by_seller")
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

// TestAhMarketPriceOutliersBySeller_MinListingsClamps mirrors the
// ah_market_price_outliers floor: unset/zero -> default 5, below floor -> 2.
func TestAhMarketPriceOutliersBySeller_MinListingsClamps(t *testing.T) {
	cases := []struct {
		args    string
		wantMin int
	}{
		{`{}`, ahOutlierDefaultMinListings},
		{`{"minListings":0}`, ahOutlierDefaultMinListings},
		{`{"minListings":1}`, ahOutlierMinListingsFloor},
		{`{"minListings":9}`, 9},
	}
	for _, c := range cases {
		t.Run(c.args, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			expectEmptyOutlierBySellerScan(mock)

			reg := NewRegistry()
			RegisterAhMarketPriceOutliersBySellerTool(reg, DBDeps{QueryDB: db})
			tool, _ := reg.Get("ah_market_price_outliers_by_seller")
			resp := tool.Handler(context.Background(), json.RawMessage(c.args), "")
			got, _ := resp.(map[string]any)
			if got["error"] != nil {
				t.Fatalf("unexpected error: %v", got)
			}
			if m, _ := got["minListings"].(int); m != c.wantMin {
				t.Errorf("minListings: %v want %d", got["minListings"], c.wantMin)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Errorf("unmet mock expectations: %v", err)
			}
		})
	}
}

// TestAhMarketPriceOutliersBySeller_MinOutlierListingsClamps — the repeat-offender
// threshold clamps to >=1 (unset/zero/negative -> 1) and is echoed.
func TestAhMarketPriceOutliersBySeller_MinOutlierListingsClamps(t *testing.T) {
	cases := []struct {
		args string
		want int
	}{
		{`{}`, 1},
		{`{"minOutlierListings":0}`, 1},
		{`{"minOutlierListings":-3}`, 1},
		{`{"minOutlierListings":4}`, 4},
	}
	for _, c := range cases {
		t.Run(c.args, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			expectEmptyOutlierBySellerScan(mock)

			reg := NewRegistry()
			RegisterAhMarketPriceOutliersBySellerTool(reg, DBDeps{QueryDB: db})
			tool, _ := reg.Get("ah_market_price_outliers_by_seller")
			resp := tool.Handler(context.Background(), json.RawMessage(c.args), "")
			got, _ := resp.(map[string]any)
			if got["error"] != nil {
				t.Fatalf("unexpected error: %v", got)
			}
			if m, _ := got["minOutlierListings"].(int); m != c.want {
				t.Errorf("minOutlierListings: %v want %d", got["minOutlierListings"], c.want)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Errorf("unmet mock expectations: %v", err)
			}
		})
	}
}

// TestAhMarketPriceOutliersBySeller_MinRatioRejected — minRatio <= 1.0 is rejected
// before any query (QueryDB non-nil to prove the reject fires after the pool check).
func TestAhMarketPriceOutliersBySeller_MinRatioRejected(t *testing.T) {
	for _, bad := range []string{`{"minRatio":1}`, `{"minRatio":1.0}`, `{"minRatio":0.5}`, `{"minRatio":-2}`} {
		db, mock, err := sqlmock.New()
		if err != nil {
			t.Fatal(err)
		}
		reg := NewRegistry()
		RegisterAhMarketPriceOutliersBySellerTool(reg, DBDeps{QueryDB: db})
		tool, _ := reg.Get("ah_market_price_outliers_by_seller")
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

// TestAhMarketPriceOutliersBySeller_MinQualityRejected — out-of-range minQuality
// is rejected before any query.
func TestAhMarketPriceOutliersBySeller_MinQualityRejected(t *testing.T) {
	for _, bad := range []string{`{"minQuality":-1}`, `{"minQuality":8}`, `{"minQuality":99}`} {
		db, mock, err := sqlmock.New()
		if err != nil {
			t.Fatal(err)
		}
		reg := NewRegistry()
		RegisterAhMarketPriceOutliersBySellerTool(reg, DBDeps{QueryDB: db})
		tool, _ := reg.Get("ah_market_price_outliers_by_seller")
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

// TestAhMarketPriceOutliersBySeller_HouseFilter verifies houseId AND-s onto the
// base buyoutprice>0 predicate and is echoed with its faction.
func TestAhMarketPriceOutliersBySeller_HouseFilter(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.ExpectExec(regexp.QuoteMeta("USE `acore_characters`")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta("WHERE ah.buyoutprice > 0 AND ah.houseid = ? ORDER BY it.entry ASC LIMIT ?")).
		WithArgs(6, int64(ahOutlierScanCap+1)).
		WillReturnRows(sqlmock.NewRows(ahOutlierRowCols()))

	reg := NewRegistry()
	RegisterAhMarketPriceOutliersBySellerTool(reg, DBDeps{QueryDB: db})
	tool, _ := reg.Get("ah_market_price_outliers_by_seller")
	resp := tool.Handler(context.Background(), json.RawMessage(`{"houseId":6}`), "")
	got, _ := resp.(map[string]any)
	if got["error"] != nil {
		t.Fatalf("unexpected error: %v", got)
	}
	if h, _ := got["houseId"].(int); h != 6 {
		t.Errorf("houseId echo: %v want 6", got["houseId"])
	}
	if f, _ := got["faction"].(string); f != "Horde" {
		t.Errorf("faction echo: %v want Horde", got["faction"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestCollectAhPriceOutliersBySeller_Golden folds the fixture at the default
// thresholds (minRatio 3, minListings 5, minOutlierListings 1) and verifies the
// per-seller rollup, the worst-first sort (severity then outlier count), the
// total-markup + worst-listing fields, the bot/player identity resolution, and a
// deleted-character miss surfacing as unresolved.
func TestCollectAhPriceOutliersBySeller_Golden(t *testing.T) {
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
		WillReturnRows(ahOutlierBySellerGoldenRows())
	// Stage 2: characters for the ranked top sellers [1001, 2002, 3003]. 3003 is
	// deliberately absent (deleted character -> unresolved).
	mock.ExpectQuery(regexp.QuoteMeta("FROM `characters` WHERE guid IN (?,?,?)")).
		WithArgs(int64(1001), int64(2002), int64(3003)).
		WillReturnRows(sqlmock.NewRows([]string{"guid", "name", "account"}).
			AddRow(int64(1001), "Goldgouger", int64(11)).
			AddRow(int64(2002), "Scalper", int64(22)))
	// Stage 3: accounts for the two resolved sellers. 11 is RNDBOT* (bot), 22 human.
	mock.ExpectExec(regexp.QuoteMeta("USE `acore_auth`")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta("FROM `account` WHERE id IN (?,?)")).
		WithArgs(int64(11), int64(22)).
		WillReturnRows(sqlmock.NewRows([]string{"id", "username"}).
			AddRow(int64(11), "RNDBOT0001").
			AddRow(int64(22), "Realdude"))

	out, err := collectAhPriceOutliersBySeller(context.Background(), db, now, 25, 5, 1, 3.0, nil, nil)
	if err != nil {
		t.Fatalf("collectAhPriceOutliersBySeller: %v", err)
	}

	if sl, _ := out["scannedListings"].(int); sl != 15 {
		t.Errorf("scannedListings: %v want 15", out["scannedListings"])
	}
	if ei, _ := out["evaluatedItems"].(int); ei != 2 {
		t.Errorf("evaluatedItems: %v want 2", out["evaluatedItems"])
	}
	if el, _ := out["evaluatedListings"].(int); el != 15 {
		t.Errorf("evaluatedListings: %v want 15", out["evaluatedListings"])
	}
	if to, _ := out["totalOutlierListings"].(int); to != 5 {
		t.Errorf("totalOutlierListings: %v want 5", out["totalOutlierListings"])
	}
	if d, _ := out["distinctOutlierSellers"].(int); d != 3 {
		t.Errorf("distinctOutlierSellers: %v want 3", out["distinctOutlierSellers"])
	}
	if u, _ := out["unresolved"].(int); u != 1 {
		t.Errorf("unresolved: %v want 1 (seller 3003 deleted char)", out["unresolved"])
	}

	sellers, ok := out["sellers"].([]*ahOutlierSeller)
	if !ok || len(sellers) != 3 {
		t.Fatalf("sellers: %T len %d want []*ahOutlierSeller len 3", out["sellers"], len(sellers))
	}

	// [0] = seller 1001, the repeat offender (high severity, 3 outliers).
	s0 := sellers[0]
	if s0.SellerGuid != 1001 || s0.Severity != "high" || s0.OutlierListings != 3 || s0.DistinctItems != 2 {
		t.Errorf("sellers[0]: %+v want guid=1001 high outliers=3 items=2", s0)
	}
	if s0.HighSeverityCount != 1 || s0.MaxRatio != 50 {
		t.Errorf("sellers[0] sev/ratio: highCount=%d maxRatio=%g want 1/50", s0.HighSeverityCount, s0.MaxRatio)
	}
	if s0.TotalMarkupCopper != 12400 || s0.TotalMarkupGold != "1g24s" {
		t.Errorf("sellers[0] markup: %d %q want 12400 / 1g24s", s0.TotalMarkupCopper, s0.TotalMarkupGold)
	}
	if s0.WorstAuctionID != 7 || s0.WorstItemEntry != 4306 || s0.WorstItemName != "Silk Cloth" || s0.WorstListingCopper != 5000 || s0.WorstListingGold != "50s" {
		t.Errorf("sellers[0] worst: %+v want auction=7 entry=4306 'Silk Cloth' 5000/50s", s0)
	}
	if s0.CharacterName != "Goldgouger" || s0.AccountID != 11 || !s0.IsBot {
		t.Errorf("sellers[0] identity: name=%q acct=%d isBot=%v want Goldgouger/11/true", s0.CharacterName, s0.AccountID, s0.IsBot)
	}
	if !strings.Contains(s0.Reason, "AHBot character") || !strings.Contains(s0.Reason, ">=10x") {
		t.Errorf("sellers[0] reason: %q", s0.Reason)
	}

	// [1] = seller 2002, single 12x high outlier.
	s1 := sellers[1]
	if s1.SellerGuid != 2002 || s1.Severity != "high" || s1.OutlierListings != 1 || s1.MaxRatio != 12 {
		t.Errorf("sellers[1]: %+v want guid=2002 high outliers=1 maxRatio=12", s1)
	}
	if s1.TotalMarkupCopper != 11000 || s1.WorstAuctionID != 16 {
		t.Errorf("sellers[1] markup/worst: %d auction=%d want 11000/16", s1.TotalMarkupCopper, s1.WorstAuctionID)
	}
	if s1.CharacterName != "Scalper" || s1.AccountID != 22 || s1.IsBot {
		t.Errorf("sellers[1] identity: name=%q acct=%d isBot=%v want Scalper/22/false", s1.CharacterName, s1.AccountID, s1.IsBot)
	}

	// [2] = seller 3003, single 4x medium, deleted character (unresolved).
	s2 := sellers[2]
	if s2.SellerGuid != 3003 || s2.Severity != "medium" || s2.OutlierListings != 1 || s2.MaxRatio != 4 {
		t.Errorf("sellers[2]: %+v want guid=3003 medium outliers=1 maxRatio=4", s2)
	}
	if s2.TotalMarkupCopper != 300 || s2.CharacterName != "" || s2.AccountID != 0 || s2.IsBot {
		t.Errorf("sellers[2] (deleted char): %+v want markup=300 empty-name acct0 not-bot", s2)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestCollectAhPriceOutliersBySeller_MinOutlierListings raises the repeat-offender
// threshold to 2 so only seller 1001 (3 outliers) survives; 2002 and 3003 (1 each)
// drop out, and the IN-query binds only the surviving seller.
func TestCollectAhPriceOutliersBySeller_MinOutlierListings(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectExec(regexp.QuoteMeta("USE `acore_characters`")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta("ORDER BY it.entry ASC LIMIT ?")).
		WithArgs(int64(ahOutlierScanCap + 1)).
		WillReturnRows(ahOutlierBySellerGoldenRows())
	mock.ExpectQuery(regexp.QuoteMeta("FROM `characters` WHERE guid IN (?)")).
		WithArgs(int64(1001)).
		WillReturnRows(sqlmock.NewRows([]string{"guid", "name", "account"}).
			AddRow(int64(1001), "Goldgouger", int64(11)))
	mock.ExpectExec(regexp.QuoteMeta("USE `acore_auth`")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta("FROM `account` WHERE id IN (?)")).
		WithArgs(int64(11)).
		WillReturnRows(sqlmock.NewRows([]string{"id", "username"}).
			AddRow(int64(11), "RNDBOT0001"))

	out, err := collectAhPriceOutliersBySeller(context.Background(), db, time.Unix(1_700_000_000, 0), 25, 5, 2, 3.0, nil, nil)
	if err != nil {
		t.Fatalf("collectAhPriceOutliersBySeller: %v", err)
	}
	// totalOutlierListings stays the honest full count (5); only the seller list
	// and its distinct count reflect the minOutlierListings=2 filter.
	if to, _ := out["totalOutlierListings"].(int); to != 5 {
		t.Errorf("totalOutlierListings: %v want 5 (honest, pre-filter)", out["totalOutlierListings"])
	}
	if d, _ := out["distinctOutlierSellers"].(int); d != 1 {
		t.Errorf("distinctOutlierSellers: %v want 1 (only 1001 has >=2 outliers)", out["distinctOutlierSellers"])
	}
	sellers, _ := out["sellers"].([]*ahOutlierSeller)
	if len(sellers) != 1 || sellers[0].SellerGuid != 1001 {
		t.Fatalf("sellers: want only guid 1001, got %+v", sellers)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestCollectAhPriceOutliersBySeller_Empty — an empty market yields a non-nil
// empty sellers slice, zeroed counters, and NO stage-2/3 identity queries.
func TestCollectAhPriceOutliersBySeller_Empty(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	expectEmptyOutlierBySellerScan(mock)

	out, err := collectAhPriceOutliersBySeller(context.Background(), db, time.Unix(1_700_000_000, 0), 25, 5, 1, 3.0, nil, nil)
	if err != nil {
		t.Fatalf("collectAhPriceOutliersBySeller: %v", err)
	}
	sellers, ok := out["sellers"].([]*ahOutlierSeller)
	if !ok {
		t.Fatalf("sellers type: %T want []*ahOutlierSeller", out["sellers"])
	}
	if sellers == nil || len(sellers) != 0 {
		t.Errorf("sellers: %v want non-nil empty slice", sellers)
	}
	if to, _ := out["totalOutlierListings"].(int); to != 0 {
		t.Errorf("totalOutlierListings: %v want 0", out["totalOutlierListings"])
	}
	if u, _ := out["unresolved"].(int); u != 0 {
		t.Errorf("unresolved: %v want 0", out["unresolved"])
	}
	if _, ok := out["houseId"]; ok {
		t.Errorf("houseId should be absent with no filter, got %v", out["houseId"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestRollupAhOutlierSellers_SortAndFilter exercises the pure rollup: per-seller
// fold, the minOutlierListings filter, the worst-first sort (severity, then
// outlierListings desc, then totalMarkup desc, then sellerGuid asc), the distinct
// count, and the non-nil empty return. Built from hand-made outliers so it is
// independent of the SQL scan.
func TestRollupAhOutlierSellers_SortAndFilter(t *testing.T) {
	o := func(seller, auction, entry int64, listing, median int64, ratio float64, sev string) *ahPriceOutlier {
		return &ahPriceOutlier{
			SellerGuid: seller, AuctionID: auction, ItemEntry: entry, ItemName: "X",
			ListingBuyoutCopper: listing, MedianBuyoutCopper: median, Ratio: ratio, Severity: sev,
		}
	}
	outliers := []*ahPriceOutlier{
		// seller 10: two medium outliers (no high) -> medium, count 2
		o(10, 1, 100, 500, 100, 5, "medium"),
		o(10, 2, 200, 400, 100, 4, "medium"),
		// seller 20: two high outliers -> high, count 2 (ranks above 10 on severity)
		o(20, 3, 100, 2000, 100, 20, "high"),
		o(20, 5, 300, 1500, 100, 15, "high"),
		// seller 30: one medium outlier -> filtered by minOutlierListings=2
		o(30, 4, 100, 300, 100, 3, "medium"),
	}
	got, distinct := rollupAhOutlierSellers(outliers, 2, 10)
	if distinct != 2 {
		t.Errorf("distinct: %d want 2 (seller 30 filtered by minOutlierListings=2)", distinct)
	}
	if len(got) != 2 {
		t.Fatalf("len: %d want 2", len(got))
	}
	// severity-first: seller 20 (high, 2 outliers) ranks above seller 10 (medium,
	// 2 outliers) on the leading severity key even though their counts tie.
	if got[0].SellerGuid != 20 || got[0].Severity != "high" {
		t.Errorf("got[0]: %+v want guid=20 high", got[0])
	}
	if got[1].SellerGuid != 10 || got[1].Severity != "medium" || got[1].OutlierListings != 2 {
		t.Errorf("got[1]: %+v want guid=10 medium outliers=2", got[1])
	}

	empty, d := rollupAhOutlierSellers(nil, 1, 10)
	if empty == nil || len(empty) != 0 || d != 0 {
		t.Errorf("nil outliers: want non-nil empty slice + distinct 0, got %v / %d", empty, d)
	}
}
