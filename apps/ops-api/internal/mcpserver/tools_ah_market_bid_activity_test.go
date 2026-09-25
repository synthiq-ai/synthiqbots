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

// ahBidAggCols is the column set the aggregate census scans, in order.
func ahBidAggCols() []string {
	return []string{"auctionsWithBid", "totalCurrentBid", "totalStartBid", "withBuyout"}
}

// ahBidListCols is the column set each top-N list query scans, in order.
func ahBidListCols() []string {
	return []string{"id", "houseid", "entry", "name", "Quality", "startbid", "lastbid", "buyoutprice"}
}

// ahBidContestedOrderBy / ahBidClosestOrderBy are the distinctive ORDER BY tails
// that let the mock disambiguate the two list queries.
const (
	ahBidAggSelect        = "SELECT COUNT(*), COALESCE(SUM(ah.lastbid), 0)"
	ahBidContestedOrderBy = " ORDER BY ah.lastbid DESC, ah.id ASC LIMIT ?"
	ahBidClosestOrderBy   = " ORDER BY (ah.lastbid / ah.buyoutprice) DESC, ah.id ASC LIMIT ?"
)

// TestAhMarketBidActivity_DescriptionMentionsContext guards the keywords that
// steer the agent here vs ah_market_summary (a withActiveBid COUNT only) or a
// price/expiry lens — the bid-competition vocabulary is load-bearing for selection.
func TestAhMarketBidActivity_DescriptionMentionsContext(t *testing.T) {
	reg := NewRegistry()
	RegisterAhMarketBidActivityTool(reg, DBDeps{})
	tool, ok := reg.Get("ah_market_bid_activity")
	if !ok {
		t.Fatal("ah_market_bid_activity not registered")
	}
	for _, kw := range []string{
		"acore_characters.auctionhouse", "item_instance", "acore_world.item_template", "ah_market_summary",
		"ops_ro", "buyguid", "mostContested", "closestToBuyout", "bidOverStartPct", "bidToBuyoutPct",
		"qualityName", "Heirloom", "houseId", "minBid", "topN", "Alliance", "Horde", "Neutral", "Read-only",
	} {
		if !strings.Contains(tool.Description, kw) {
			t.Errorf("description missing %q: %s", kw, tool.Description)
		}
	}
}

// TestAhMarketBidActivity_ReadOnlyAnnotation pins readOnlyHint=true so a future
// copy-paste of a destructive sibling can't silently flip the gate.
func TestAhMarketBidActivity_ReadOnlyAnnotation(t *testing.T) {
	reg := NewRegistry()
	RegisterAhMarketBidActivityTool(reg, DBDeps{})
	tool, _ := reg.Get("ah_market_bid_activity")
	if !strings.Contains(string(tool.Annotations), `"readOnlyHint":true`) {
		t.Errorf("annotations missing readOnlyHint=true: %s", tool.Annotations)
	}
}

func TestAhMarketBidActivity_NoPoolReturnsError(t *testing.T) {
	reg := NewRegistry()
	RegisterAhMarketBidActivityTool(reg, DBDeps{}) // QueryDB nil
	tool, _ := reg.Get("ah_market_bid_activity")
	resp := tool.Handler(context.Background(), json.RawMessage(`{}`), "")
	got, _ := resp.(map[string]any)
	if msg, _ := got["error"].(string); !strings.Contains(msg, "ops_ro pool not configured") {
		t.Errorf("expected pool-not-configured error, got %v", got)
	}
}

// TestAhMarketBidActivity_TopNClamps drives the handler and asserts the post-clamp
// topN echo (unset->default, zero/negative->default, oversized->max, in-range
// passthrough) and that BOTH list queries' LIMIT bind matches the clamped value.
func TestAhMarketBidActivity_TopNClamps(t *testing.T) {
	cases := []struct {
		name     string
		args     string
		wantTopN int
	}{
		{"unset uses default", `{}`, ahBidActivityDefaultTop},
		{"zero falls back to default", `{"topN":0}`, ahBidActivityDefaultTop},
		{"negative falls back to default", `{"topN":-7}`, ahBidActivityDefaultTop},
		{"oversized clamps to max", `{"topN":99999}`, ahBidActivityMaxTop},
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
			mock.ExpectQuery(regexp.QuoteMeta(ahBidAggSelect)).
				WillReturnRows(sqlmock.NewRows(ahBidAggCols()).AddRow(int64(0), int64(0), int64(0), int64(0)))
			mock.ExpectQuery(regexp.QuoteMeta(ahBidContestedOrderBy)).
				WithArgs(int64(c.wantTopN)).
				WillReturnRows(sqlmock.NewRows(ahBidListCols()))
			mock.ExpectQuery(regexp.QuoteMeta(ahBidClosestOrderBy)).
				WithArgs(int64(c.wantTopN)).
				WillReturnRows(sqlmock.NewRows(ahBidListCols()))

			reg := NewRegistry()
			RegisterAhMarketBidActivityTool(reg, DBDeps{QueryDB: db})
			tool, _ := reg.Get("ah_market_bid_activity")
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

// TestAhMarketBidActivity_HouseIdRejected — a houseId outside {2,6,7} is rejected
// (no DB call) so a typo can't masquerade as an empty market. QueryDB is non-nil
// to prove the reject fires AFTER the pool check but before any query.
func TestAhMarketBidActivity_HouseIdRejected(t *testing.T) {
	for _, bad := range []string{`{"houseId":0}`, `{"houseId":1}`, `{"houseId":3}`, `{"houseId":99}`} {
		db, mock, err := sqlmock.New()
		if err != nil {
			t.Fatal(err)
		}
		reg := NewRegistry()
		RegisterAhMarketBidActivityTool(reg, DBDeps{QueryDB: db})
		tool, _ := reg.Get("ah_market_bid_activity")
		resp := tool.Handler(context.Background(), json.RawMessage(bad), "")
		got, _ := resp.(map[string]any)
		if msg, _ := got["error"].(string); !strings.Contains(msg, "houseId invalid") {
			t.Errorf("args %s: expected houseId-invalid error, got %v", bad, got)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("args %s: no query should run on reject: %v", bad, err)
		}
		db.Close()
	}
}

// TestAhMarketBidActivity_MinBidRejected — a negative minBid floor is rejected (no
// DB call): lastbid is unsigned so >= negative would match every row, silently
// disabling the filter.
func TestAhMarketBidActivity_MinBidRejected(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	reg := NewRegistry()
	RegisterAhMarketBidActivityTool(reg, DBDeps{QueryDB: db})
	tool, _ := reg.Get("ah_market_bid_activity")
	resp := tool.Handler(context.Background(), json.RawMessage(`{"minBid":-5}`), "")
	got, _ := resp.(map[string]any)
	if msg, _ := got["error"].(string); !strings.Contains(msg, "minBid negative") {
		t.Errorf("expected minBid-negative error, got %v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("no query should run on reject: %v", err)
	}
}

// TestAhMarketBidActivity_Filters drives the handler with each filter combination
// and asserts the WHERE clause shape + bound args on all three queries (aggregate,
// most-contested, closest-to-buyout) plus the echoed houseId/faction/minBidCopper.
// The closest-to-buyout query carries the extra buyoutprice>0 guard with NO bind,
// so its arg order stays baseArgs+LIMIT.
func TestAhMarketBidActivity_Filters(t *testing.T) {
	cases := []struct {
		name         string
		args         string
		aggArgs      []driverArg
		contPattern  string
		contArgs     []driverArg
		closePattern string
		closeArgs    []driverArg
		wantHouse    int // -1 = key absent
		wantFaction  string
		wantMinBid   int64
		wantMinBidOK bool
	}{
		{
			name:         "house only",
			args:         `{"houseId":7}`,
			aggArgs:      []driverArg{i(7)},
			contPattern:  "WHERE ah.buyguid <> 0 AND ah.houseid = ? ORDER BY ah.lastbid DESC, ah.id ASC LIMIT ?",
			contArgs:     []driverArg{i(7), i(ahBidActivityDefaultTop)},
			closePattern: "WHERE ah.buyguid <> 0 AND ah.houseid = ? AND ah.buyoutprice > 0 ORDER BY (ah.lastbid / ah.buyoutprice) DESC, ah.id ASC LIMIT ?",
			closeArgs:    []driverArg{i(7), i(ahBidActivityDefaultTop)},
			wantHouse:    7, wantFaction: "Neutral", wantMinBidOK: false,
		},
		{
			name:         "minBid only",
			args:         `{"minBid":5000}`,
			aggArgs:      []driverArg{i(5000)},
			contPattern:  "WHERE ah.buyguid <> 0 AND ah.lastbid >= ? ORDER BY ah.lastbid DESC, ah.id ASC LIMIT ?",
			contArgs:     []driverArg{i(5000), i(ahBidActivityDefaultTop)},
			closePattern: "WHERE ah.buyguid <> 0 AND ah.lastbid >= ? AND ah.buyoutprice > 0 ORDER BY (ah.lastbid / ah.buyoutprice) DESC, ah.id ASC LIMIT ?",
			closeArgs:    []driverArg{i(5000), i(ahBidActivityDefaultTop)},
			wantHouse:    -1, wantMinBid: 5000, wantMinBidOK: true,
		},
		{
			name:         "both filters",
			args:         `{"houseId":2,"minBid":1000,"topN":5}`,
			aggArgs:      []driverArg{i(2), i(1000)},
			contPattern:  "WHERE ah.buyguid <> 0 AND ah.houseid = ? AND ah.lastbid >= ? ORDER BY ah.lastbid DESC, ah.id ASC LIMIT ?",
			contArgs:     []driverArg{i(2), i(1000), i(5)},
			closePattern: "WHERE ah.buyguid <> 0 AND ah.houseid = ? AND ah.lastbid >= ? AND ah.buyoutprice > 0 ORDER BY (ah.lastbid / ah.buyoutprice) DESC, ah.id ASC LIMIT ?",
			closeArgs:    []driverArg{i(2), i(1000), i(5)},
			wantHouse:    2, wantFaction: "Alliance", wantMinBid: 1000, wantMinBidOK: true,
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
			mock.ExpectQuery(regexp.QuoteMeta(ahBidAggSelect)).
				WithArgs(toValues(c.aggArgs)...).
				WillReturnRows(sqlmock.NewRows(ahBidAggCols()).AddRow(int64(0), int64(0), int64(0), int64(0)))
			mock.ExpectQuery(regexp.QuoteMeta(c.contPattern)).
				WithArgs(toValues(c.contArgs)...).
				WillReturnRows(sqlmock.NewRows(ahBidListCols()))
			mock.ExpectQuery(regexp.QuoteMeta(c.closePattern)).
				WithArgs(toValues(c.closeArgs)...).
				WillReturnRows(sqlmock.NewRows(ahBidListCols()))

			reg := NewRegistry()
			RegisterAhMarketBidActivityTool(reg, DBDeps{QueryDB: db})
			tool, _ := reg.Get("ah_market_bid_activity")
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
			if c.wantMinBidOK {
				if m, _ := got["minBidCopper"].(int64); m != c.wantMinBid {
					t.Errorf("minBidCopper: %v want %d", got["minBidCopper"], c.wantMinBid)
				}
			} else if _, ok := got["minBidCopper"]; ok {
				t.Errorf("minBidCopper should be absent, got %v", got["minBidCopper"])
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Errorf("unmet mock expectations: %v", err)
			}
		})
	}
}

// TestCollectAhBidActivity_Golden folds the aggregate census plus both top-N lists
// and verifies the gold strings, the avg-over-count division, the aggregate
// bidOverStartPct, per-row bidOverStartPct / bidToBuyoutPct, HasBuyout, the
// bidToBuyoutPct omitempty (absent for an auction-only listing), and that the
// collector trusts the SQL ORDER BY (no Go-side re-sort).
func TestCollectAhBidActivity_Golden(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	now := time.Unix(1_700_000_000, 0)
	mock.ExpectExec(regexp.QuoteMeta("USE `acore_characters`")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	// Aggregate: 4 live-bid listings, 3 with buyout; current sum 16g, opening sum
	// 10g -> avg 4g, bidOverStartPct (160000-100000)/100000 = 60.0.
	mock.ExpectQuery(regexp.QuoteMeta(ahBidAggSelect)).
		WillReturnRows(sqlmock.NewRows(ahBidAggCols()).
			AddRow(int64(4), int64(160000), int64(100000), int64(3)))
	// most-contested (SQL order = lastbid desc: A 9g, B 6g, C 15s). A is
	// auction-only (buyout 0) -> HasBuyout false, bidToBuyoutPct omitted.
	mock.ExpectQuery(regexp.QuoteMeta(ahBidContestedOrderBy)).
		WithArgs(int64(15)).
		WillReturnRows(sqlmock.NewRows(ahBidListCols()).
			AddRow(int64(101), 2, int64(49623), "Shadowmourne", 5, int64(50000), int64(90000), int64(0)).
			AddRow(int64(102), 6, int64(22445), "Arcane Dust", 2, int64(10000), int64(60000), int64(120000)).
			AddRow(int64(103), 7, int64(4306), "Silk Cloth", 1, int64(1000), int64(1500), int64(2000)))
	// closest-to-buyout (SQL order = ratio desc: C 75%, B 50%; A excluded, no buyout).
	mock.ExpectQuery(regexp.QuoteMeta(ahBidClosestOrderBy)).
		WithArgs(int64(15)).
		WillReturnRows(sqlmock.NewRows(ahBidListCols()).
			AddRow(int64(103), 7, int64(4306), "Silk Cloth", 1, int64(1000), int64(1500), int64(2000)).
			AddRow(int64(102), 6, int64(22445), "Arcane Dust", 2, int64(10000), int64(60000), int64(120000)))

	out, err := collectAhBidActivity(context.Background(), db, now, 15, nil, nil)
	if err != nil {
		t.Fatalf("collectAhBidActivity: %v", err)
	}
	if _, ok := out["houseId"]; ok {
		t.Errorf("houseId should be absent with no filter, got %v", out["houseId"])
	}
	if _, ok := out["minBidCopper"]; ok {
		t.Errorf("minBidCopper should be absent with no filter, got %v", out["minBidCopper"])
	}

	totals, ok := out["totals"].(map[string]any)
	if !ok {
		t.Fatalf("totals: %T want map[string]any", out["totals"])
	}
	if n, _ := totals["auctionsWithBid"].(int); n != 4 {
		t.Errorf("auctionsWithBid: %v want 4", totals["auctionsWithBid"])
	}
	if n, _ := totals["withBuyout"].(int); n != 3 {
		t.Errorf("withBuyout: %v want 3", totals["withBuyout"])
	}
	if v, _ := totals["totalCurrentBidCopper"].(int64); v != 160000 {
		t.Errorf("totalCurrentBidCopper: %v want 160000", totals["totalCurrentBidCopper"])
	}
	if s, _ := totals["totalCurrentBidGold"].(string); s != "16g" {
		t.Errorf("totalCurrentBidGold: %v want 16g", totals["totalCurrentBidGold"])
	}
	if s, _ := totals["totalStartBidGold"].(string); s != "10g" {
		t.Errorf("totalStartBidGold: %v want 10g", totals["totalStartBidGold"])
	}
	if v, _ := totals["avgCurrentBidCopper"].(int64); v != 40000 {
		t.Errorf("avgCurrentBidCopper: %v want 40000", totals["avgCurrentBidCopper"])
	}
	if s, _ := totals["avgCurrentBidGold"].(string); s != "4g" {
		t.Errorf("avgCurrentBidGold: %v want 4g", totals["avgCurrentBidGold"])
	}
	if p, _ := totals["bidOverStartPct"].(float64); p != 60.0 {
		t.Errorf("bidOverStartPct: %v want 60.0", totals["bidOverStartPct"])
	}

	mc, ok := out["mostContested"].([]*ahBidAuction)
	if !ok || len(mc) != 3 {
		t.Fatalf("mostContested: %T len %d want []*ahBidAuction len 3", out["mostContested"], len(mc))
	}
	a := mc[0]
	if a.AuctionID != 101 || a.HouseID != 2 || a.Faction != "Alliance" || a.ItemName != "Shadowmourne" || a.QualityName != "Legendary" {
		t.Errorf("mostContested[0] identity: %+v", a)
	}
	if a.CurrentBidCopper != 90000 || a.CurrentBidGold != "9g" || a.StartBidGold != "5g" {
		t.Errorf("mostContested[0] bid: %+v", a)
	}
	if a.BuyoutCopper != 0 || a.BuyoutGold != "0c" || a.HasBuyout {
		t.Errorf("mostContested[0] buyout: %+v", a)
	}
	if a.BidOverStartPct != 80.0 || a.BidToBuyoutPct != 0 {
		t.Errorf("mostContested[0] pct: %+v", a)
	}
	if mc[1].BidOverStartPct != 500.0 || !mc[1].HasBuyout || mc[1].BidToBuyoutPct != 50.0 {
		t.Errorf("mostContested[1] pct: %+v", mc[1])
	}
	if mc[2].BidOverStartPct != 50.0 || mc[2].BidToBuyoutPct != 75.0 || mc[2].CurrentBidGold != "15s" {
		t.Errorf("mostContested[2] pct: %+v", mc[2])
	}

	cb, ok := out["closestToBuyout"].([]*ahBidAuction)
	if !ok || len(cb) != 2 {
		t.Fatalf("closestToBuyout: %T len %d want []*ahBidAuction len 2", out["closestToBuyout"], len(cb))
	}
	if cb[0].AuctionID != 103 || cb[0].BidToBuyoutPct != 75.0 {
		t.Errorf("closestToBuyout[0]: %+v", cb[0])
	}
	if cb[1].AuctionID != 102 || cb[1].BidToBuyoutPct != 50.0 {
		t.Errorf("closestToBuyout[1]: %+v", cb[1])
	}

	// omitempty: the auction-only listing (mc[0]) drops bidToBuyoutPct from JSON;
	// the buyout-bearing one (mc[1]) keeps it.
	if j, _ := json.Marshal(mc[0]); strings.Contains(string(j), "bidToBuyoutPct") {
		t.Errorf("mostContested[0] JSON should omit bidToBuyoutPct: %s", j)
	}
	if j, _ := json.Marshal(mc[1]); !strings.Contains(string(j), "bidToBuyoutPct") {
		t.Errorf("mostContested[1] JSON should carry bidToBuyoutPct: %s", j)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestCollectAhBidActivity_Empty — an AH with no live bids folds cleanly: the
// COALESCE'd aggregate yields zeros (avg + bidOverStartPct div-guarded to 0) and
// both lists are non-nil empty slices (callers expect arrays).
func TestCollectAhBidActivity_Empty(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.ExpectExec(regexp.QuoteMeta("USE `acore_characters`")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta(ahBidAggSelect)).
		WillReturnRows(sqlmock.NewRows(ahBidAggCols()).AddRow(int64(0), int64(0), int64(0), int64(0)))
	mock.ExpectQuery(regexp.QuoteMeta(ahBidContestedOrderBy)).
		WithArgs(int64(15)).
		WillReturnRows(sqlmock.NewRows(ahBidListCols()))
	mock.ExpectQuery(regexp.QuoteMeta(ahBidClosestOrderBy)).
		WithArgs(int64(15)).
		WillReturnRows(sqlmock.NewRows(ahBidListCols()))

	out, err := collectAhBidActivity(context.Background(), db, time.Unix(1_700_000_000, 0), 15, nil, nil)
	if err != nil {
		t.Fatalf("collectAhBidActivity: %v", err)
	}
	totals, _ := out["totals"].(map[string]any)
	if n, _ := totals["auctionsWithBid"].(int); n != 0 {
		t.Errorf("auctionsWithBid: %v want 0", totals["auctionsWithBid"])
	}
	if v, _ := totals["avgCurrentBidCopper"].(int64); v != 0 {
		t.Errorf("avgCurrentBidCopper div-guard: %v want 0", totals["avgCurrentBidCopper"])
	}
	if p, _ := totals["bidOverStartPct"].(float64); p != 0 {
		t.Errorf("bidOverStartPct div-guard: %v want 0", totals["bidOverStartPct"])
	}
	mc, ok := out["mostContested"].([]*ahBidAuction)
	if !ok || mc == nil || len(mc) != 0 {
		t.Errorf("mostContested: %v want non-nil empty slice", out["mostContested"])
	}
	cb, ok := out["closestToBuyout"].([]*ahBidAuction)
	if !ok || cb == nil || len(cb) != 0 {
		t.Errorf("closestToBuyout: %v want non-nil empty slice", out["closestToBuyout"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestAhBidPct exercises the percent helper: one-decimal rounding and the
// total<=0 div-guard.
func TestAhBidPct(t *testing.T) {
	cases := []struct {
		n, total int64
		want     float64
	}{
		{15000, 150000, 10.0},
		{50000, 10000, 500.0},
		{1500, 2000, 75.0},
		{1, 3, 33.3},
		{5, 0, 0},  // div-guard
		{5, -1, 0}, // div-guard (negative total)
		{0, 1000, 0},
	}
	for _, c := range cases {
		if got := ahBidPct(c.n, c.total); got != c.want {
			t.Errorf("ahBidPct(%d, %d) = %v want %v", c.n, c.total, got, c.want)
		}
	}
}
