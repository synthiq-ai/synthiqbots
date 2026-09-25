package mcpserver

import (
	"context"
	"encoding/json"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

// ahFootprintScanCols is the per-listing scan column set, in scan order.
func ahFootprintScanCols() []string {
	return []string{"itemowner", "houseid", "itemEntry", "count", "buyoutprice"}
}

// ahFootprintScanQ pins the single stage-1 query: it is the only one selecting
// itemowner + houseid together.
const ahFootprintScanQ = "SELECT ah.itemowner, ah.houseid, ii.itemEntry, ii.count, ah.buyoutprice"

// expectEmptyFootprint queues USE + an empty listing scan. With no sellers folded,
// no stage-4 (characters/account) queries fire, so these two expectations are all
// the arg-clamp tests need.
func expectEmptyFootprint(mock sqlmock.Sqlmock) {
	mock.ExpectExec(regexp.QuoteMeta("USE `acore_characters`")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta(ahFootprintScanQ)).
		WithArgs(int64(ahFootprintScanCap + 1)).
		WillReturnRows(sqlmock.NewRows(ahFootprintScanCols()))
}

// mkFootprint folds a seller from a per-house listing count map and finalizes it,
// so the sort/census helpers can be driven without sqlmock. Each listing gets a
// unique itemEntry (so distinctItems == listings) and quantity 1.
func mkFootprint(guid int64, listingsPerHouse map[int]int, buyoutEach int64) *ahFootprintSeller {
	s := &ahFootprintSeller{ItemOwnerGuid: guid}
	houses := make([]int, 0, len(listingsPerHouse))
	for h := range listingsPerHouse {
		houses = append(houses, h)
	}
	sort.Ints(houses) // keeps the synthesized itemEntries stable across runs
	entry := int64(0)
	for _, h := range houses {
		for k := 0; k < listingsPerHouse[h]; k++ {
			entry++
			s.fold(h, entry, 1, buyoutEach)
		}
	}
	s.finalize()
	return s
}

// TestAhSellerHouseFootprint_DescriptionMentionsContext guards the vocabulary that
// steers the agent here rather than to ah_market_top_sellers / ah_undercutters
// (house scoped or folded away) or ah_cross_house_arbitrage_depth (item axis, no
// seller). The cross-house ACTOR framing is load-bearing for tool selection.
func TestAhSellerHouseFootprint_DescriptionMentionsContext(t *testing.T) {
	reg := NewRegistry()
	RegisterAhSellerHouseFootprintTool(reg, DBDeps{})
	tool, ok := reg.Get("ah_seller_house_footprint")
	if !ok {
		t.Fatal("ah_seller_house_footprint not registered")
	}
	for _, kw := range []string{
		"acore_characters.auctionhouse", "item_instance", "acore_auth.account.username", "ops_ro",
		"itemowner", "RNDBOT", "isBot",
		"housesUsed", "concentrationPct", "primaryHouseId", "listingsPct", "distinctItems",
		"buyoutValueCopper", "houseTotals", "crossHouseSellers", "unresolved", "truncated",
		"ah_cross_house_arbitrage_depth", "ah_market_top_sellers", "ah_undercutters",
		"topN", "sortBy", "minHouses", "spread", "auction-only",
		"Alliance", "Horde", "Neutral", "Read-only",
	} {
		if !strings.Contains(tool.Description, kw) {
			t.Errorf("description missing %q", kw)
		}
	}
}

// TestAhSellerHouseFootprint_ReadOnlyAnnotation pins readOnlyHint=true so a future
// copy-paste of a destructive sibling can't silently flip the gate.
func TestAhSellerHouseFootprint_ReadOnlyAnnotation(t *testing.T) {
	reg := NewRegistry()
	RegisterAhSellerHouseFootprintTool(reg, DBDeps{})
	tool, _ := reg.Get("ah_seller_house_footprint")
	if !strings.Contains(string(tool.Annotations), `"readOnlyHint":true`) {
		t.Errorf("annotations missing readOnlyHint=true: %s", tool.Annotations)
	}
}

// TestAhSellerHouseFootprint_NoHouseIdArg is the axis guard: every other
// actor-grain ah_* tool takes an optional houseId, and copying one in here would
// collapse the exact dimension this tool exists to expose. Asserted against the
// INPUT SCHEMA (the arg surface), not the Description — which mentions houseId
// only to say the argument is deliberately absent.
func TestAhSellerHouseFootprint_NoHouseIdArg(t *testing.T) {
	reg := NewRegistry()
	RegisterAhSellerHouseFootprintTool(reg, DBDeps{})
	tool, _ := reg.Get("ah_seller_house_footprint")
	if strings.Contains(string(tool.InputSchema), "houseId") {
		t.Errorf("InputSchema must not accept houseId (it would collapse the cross-house axis): %s", tool.InputSchema)
	}
}

func TestAhSellerHouseFootprint_NoPoolReturnsError(t *testing.T) {
	reg := NewRegistry()
	RegisterAhSellerHouseFootprintTool(reg, DBDeps{}) // QueryDB nil
	tool, _ := reg.Get("ah_seller_house_footprint")
	resp := tool.Handler(context.Background(), json.RawMessage(`{}`), "")
	got, _ := resp.(map[string]any)
	if msg, _ := got["error"].(string); !strings.Contains(msg, "ops_ro pool not configured") {
		t.Errorf("expected pool-not-configured error, got %v", got)
	}
}

// TestAhSellerHouseFootprint_TopNClamps drives the handler and asserts the
// post-clamp topN echo (topN is a Go-side slice, so it never touches the SQL —
// the fold is empty).
func TestAhSellerHouseFootprint_TopNClamps(t *testing.T) {
	cases := []struct {
		name     string
		args     string
		wantTopN int
	}{
		{"unset uses default", `{}`, ahFootprintDefaultTopN},
		{"zero falls back to default", `{"topN":0}`, ahFootprintDefaultTopN},
		{"negative falls back to default", `{"topN":-7}`, ahFootprintDefaultTopN},
		{"oversized clamps to max", `{"topN":99999}`, ahFootprintMaxTopN},
		{"in-range passes through", `{"topN":42}`, 42},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			expectEmptyFootprint(mock)

			reg := NewRegistry()
			RegisterAhSellerHouseFootprintTool(reg, DBDeps{QueryDB: db})
			tool, _ := reg.Get("ah_seller_house_footprint")
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

// TestAhSellerHouseFootprint_MinHousesClamp — minHouses clamps at BOTH ends
// (1..3). The upper clamp is the one that matters: there are only three
// AuctionHouseIds, so an unclamped minHouses=9 would return an empty ranking that
// looks like "no cross-house sellers exist" rather than a bad argument.
func TestAhSellerHouseFootprint_MinHousesClamp(t *testing.T) {
	cases := []struct {
		args          string
		wantMinHouses int
	}{
		{`{}`, ahFootprintDefaultMinHouses},
		{`{"minHouses":0}`, 1},
		{`{"minHouses":-5}`, 1},
		{`{"minHouses":1}`, 1},
		{`{"minHouses":2}`, 2},
		{`{"minHouses":3}`, 3},
		{`{"minHouses":9}`, ahFootprintMaxMinHouses},
	}
	for _, c := range cases {
		t.Run(c.args, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			expectEmptyFootprint(mock)

			reg := NewRegistry()
			RegisterAhSellerHouseFootprintTool(reg, DBDeps{QueryDB: db})
			tool, _ := reg.Get("ah_seller_house_footprint")
			resp := tool.Handler(context.Background(), json.RawMessage(c.args), "")
			got, _ := resp.(map[string]any)
			if got["error"] != nil {
				t.Fatalf("unexpected error: %v", got)
			}
			if m, _ := got["minHouses"].(int); m != c.wantMinHouses {
				t.Errorf("minHouses: %v want %d", got["minHouses"], c.wantMinHouses)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Errorf("unmet mock expectations: %v", err)
			}
		})
	}
}

// TestAhSellerHouseFootprint_InvalidSortByRejected — an unknown sortBy is rejected
// before any query. QueryDB is non-nil to prove the reject fires after the pool
// check but before the scan.
func TestAhSellerHouseFootprint_InvalidSortByRejected(t *testing.T) {
	for _, bad := range []string{
		`{"sortBy":"houses"}`,
		`{"sortBy":"HOUSESUSED"}`,
		`{"sortBy":"listings; DROP TABLE auctionhouse"}`,
	} {
		db, mock, err := sqlmock.New()
		if err != nil {
			t.Fatal(err)
		}
		reg := NewRegistry()
		RegisterAhSellerHouseFootprintTool(reg, DBDeps{QueryDB: db})
		tool, _ := reg.Get("ah_seller_house_footprint")
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

// TestCollectAhSellerHouseFootprint_Empty — an empty market: sellers is a non-nil
// empty slice, houseTotals a non-nil empty slice, zeroed totals, and NO stage-4
// queries fire.
func TestCollectAhSellerHouseFootprint_Empty(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	expectEmptyFootprint(mock)

	out, err := collectAhSellerHouseFootprint(context.Background(), db, time.Unix(1_700_000_000, 0), 1, 15, "housesUsed")
	if err != nil {
		t.Fatalf("collectAhSellerHouseFootprint: %v", err)
	}
	sl, ok := out["sellers"].([]*ahFootprintSeller)
	if !ok {
		t.Fatalf("sellers type: %T want []*ahFootprintSeller", out["sellers"])
	}
	if sl == nil || len(sl) != 0 {
		t.Errorf("sellers: %v want non-nil empty slice", sl)
	}
	ht, ok := out["houseTotals"].([]*ahFootprintHouseTotal)
	if !ok {
		t.Fatalf("houseTotals type: %T want []*ahFootprintHouseTotal", out["houseTotals"])
	}
	if ht == nil || len(ht) != 0 {
		t.Errorf("houseTotals: %v want non-nil empty slice", ht)
	}
	if d, _ := out["displayedSellers"].(int); d != 0 {
		t.Errorf("displayedSellers: %v want 0", out["displayedSellers"])
	}
	if u, _ := out["unresolved"].(int); u != 0 {
		t.Errorf("unresolved: %v want 0", out["unresolved"])
	}
	totals, _ := out["totals"].(map[string]any)
	for _, k := range []string{"distinctSellers", "crossHouseSellers", "matchedSellers", "totalListings", "housesSeen"} {
		if v, _ := totals[k].(int); v != 0 {
			t.Errorf("totals.%s: %v want 0", k, totals[k])
		}
	}
	for _, k := range []string{"totalQuantity", "totalBuyoutValueCopper"} {
		if v, _ := totals[k].(int64); v != 0 {
			t.Errorf("totals.%s: %v want 0", k, totals[k])
		}
	}
	if m, _ := totals["totalBuyoutValueMoney"].(string); m != "0c" {
		t.Errorf("totals.totalBuyoutValueMoney: %v want 0c", totals["totalBuyoutValueMoney"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestCollectAhSellerHouseFootprint_Golden drives the full composite over a market
// with one three-house trader, one single-house whale (incl. an auction-only
// buyoutprice=0 listing), one two-house seller on a NON-STANDARD houseid, and one
// deleted character. It verifies the per-(owner,house) fold, the per-house arrays
// ordered by houseId, concentrationPct + primaryHouse selection, the realm-wide
// census + houseTotals (both independent of topN), the housesUsed-desc /
// listings-desc / guid-asc ordering, the characters batch resolution with a
// deleted-character miss, and the account batch resolution with dedup (two
// characters on ONE account both carry the RNDBOT flag).
func TestCollectAhSellerHouseFootprint_Golden(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	now := time.Unix(1_700_000_000, 0)

	mock.ExpectExec(regexp.QuoteMeta("USE `acore_characters`")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta(ahFootprintScanQ)).
		WithArgs(int64(ahFootprintScanCap + 1)).
		WillReturnRows(sqlmock.NewRows(ahFootprintScanCols()).
			// Seller 10 — cross-house trader on all three houses (4 listings).
			AddRow(int64(10), int64(2), int64(100), int64(1), int64(1000)).
			AddRow(int64(10), int64(2), int64(101), int64(2), int64(2000)).
			AddRow(int64(10), int64(6), int64(100), int64(3), int64(3000)).
			AddRow(int64(10), int64(7), int64(102), int64(4), int64(4000)).
			// Seller 20 — single-house whale (3 listings), one of them auction-only
			// (buyoutprice 0: counted in listings/quantity/distinctItems, 0 value).
			AddRow(int64(20), int64(2), int64(100), int64(5), int64(5000)).
			AddRow(int64(20), int64(2), int64(103), int64(1), int64(0)).
			AddRow(int64(20), int64(2), int64(104), int64(2), int64(500)).
			// Seller 30 — two houses, one of them a non-standard houseid (99).
			AddRow(int64(30), int64(6), int64(100), int64(1), int64(100)).
			AddRow(int64(30), int64(99), int64(100), int64(1), int64(200)).
			// Seller 40 — deleted character, listings outlive it.
			AddRow(int64(40), int64(7), int64(105), int64(1), int64(700)))

	// Stage 4a: characters lookup in display order (housesUsed desc, then listings
	// desc): 10, 30, 20, 40. Guid 40 deliberately absent (deleted-char miss).
	mock.ExpectQuery(regexp.QuoteMeta("FROM `characters` WHERE guid IN (?,?,?,?)")).
		WithArgs(int64(10), int64(30), int64(20), int64(40)).
		WillReturnRows(sqlmock.NewRows([]string{"guid", "name", "account"}).
			AddRow(int64(10), "Traderone", int64(501)).
			AddRow(int64(30), "Splitter", int64(502)).
			AddRow(int64(20), "Whaleman", int64(501)))

	// Stage 4b: account lookup — 501 appears TWICE across sellers 10 and 20 and must
	// be bound once; 40 has account 0 and is skipped.
	mock.ExpectExec(regexp.QuoteMeta("USE `acore_auth`")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta("FROM `account` WHERE id IN (?,?)")).
		WithArgs(int64(501), int64(502)).
		WillReturnRows(sqlmock.NewRows([]string{"id", "username"}).
			AddRow(int64(501), "RNDBOT_ALPHA").
			AddRow(int64(502), "Playerjoe"))

	out, err := collectAhSellerHouseFootprint(context.Background(), db, now, 1, 10, "housesUsed")
	if err != nil {
		t.Fatalf("collectAhSellerHouseFootprint: %v", err)
	}

	if s, _ := out["scanned"].(int); s != 10 {
		t.Errorf("scanned: %v want 10", out["scanned"])
	}
	if tr, _ := out["truncated"].(bool); tr {
		t.Errorf("truncated: %v want false", out["truncated"])
	}
	if d, _ := out["displayedSellers"].(int); d != 4 {
		t.Errorf("displayedSellers: %v want 4", out["displayedSellers"])
	}
	if u, _ := out["unresolved"].(int); u != 1 {
		t.Errorf("unresolved: %v want 1 (seller 40 deleted char)", out["unresolved"])
	}

	totals, _ := out["totals"].(map[string]any)
	if v, _ := totals["distinctSellers"].(int); v != 4 {
		t.Errorf("totals.distinctSellers: %v want 4", totals["distinctSellers"])
	}
	if v, _ := totals["crossHouseSellers"].(int); v != 2 {
		t.Errorf("totals.crossHouseSellers: %v want 2 (sellers 10 and 30)", totals["crossHouseSellers"])
	}
	if v, _ := totals["matchedSellers"].(int); v != 4 {
		t.Errorf("totals.matchedSellers: %v want 4 (minHouses=1 matches everyone)", totals["matchedSellers"])
	}
	if v, _ := totals["totalListings"].(int); v != 10 {
		t.Errorf("totals.totalListings: %v want 10 (4+3+2+1)", totals["totalListings"])
	}
	if v, _ := totals["totalQuantity"].(int64); v != 21 {
		t.Errorf("totals.totalQuantity: %v want 21 (10+8+2+1)", totals["totalQuantity"])
	}
	if v, _ := totals["totalBuyoutValueCopper"].(int64); v != 16500 {
		t.Errorf("totals.totalBuyoutValueCopper: %v want 16500", totals["totalBuyoutValueCopper"])
	}
	if v, _ := totals["totalBuyoutValueMoney"].(string); v != "1g65s" {
		t.Errorf("totals.totalBuyoutValueMoney: %v want 1g65s", totals["totalBuyoutValueMoney"])
	}
	if v, _ := totals["housesSeen"].(int); v != 4 {
		t.Errorf("totals.housesSeen: %v want 4 (2,6,7,99)", totals["housesSeen"])
	}

	// houseTotals: ordered by houseId asc, seller counts overlap (they sum to 7
	// across 4 distinct sellers — that excess IS the cross-house overlap).
	ht, _ := out["houseTotals"].([]*ahFootprintHouseTotal)
	if len(ht) != 4 {
		t.Fatalf("houseTotals len: %d want 4", len(ht))
	}
	wantHT := []struct {
		house    int
		faction  string
		sellers  int
		listings int
		quantity int64
		copper   int64
		money    string
	}{
		{2, "Alliance", 2, 5, 11, 8500, "85s"},
		{6, "Horde", 2, 2, 4, 3100, "31s"},
		{7, "Neutral", 2, 2, 5, 4700, "47s"},
		{99, "house-99", 1, 1, 1, 200, "2s"},
	}
	for k, w := range wantHT {
		g := ht[k]
		if g.HouseID != w.house || g.Faction != w.faction || g.Sellers != w.sellers ||
			g.Listings != w.listings || g.Quantity != w.quantity ||
			g.BuyoutValueCopper != w.copper || g.BuyoutValueMoney != w.money {
			t.Errorf("houseTotals[%d]: %+v want %+v", k, g, w)
		}
	}

	sellers, _ := out["sellers"].([]*ahFootprintSeller)
	if len(sellers) != 4 {
		t.Fatalf("sellers len: %d want 4", len(sellers))
	}
	gotOrder := []int64{sellers[0].ItemOwnerGuid, sellers[1].ItemOwnerGuid, sellers[2].ItemOwnerGuid, sellers[3].ItemOwnerGuid}
	wantOrder := []int64{10, 30, 20, 40}
	for k := range wantOrder {
		if gotOrder[k] != wantOrder[k] {
			t.Fatalf("order: %v want %v (housesUsed desc, listings desc, guid asc)", gotOrder, wantOrder)
		}
	}

	// Seller 10 — the three-house trader.
	s0 := sellers[0]
	if s0.HousesUsed != 3 || s0.Listings != 4 || s0.Quantity != 10 || s0.DistinctItems != 3 {
		t.Errorf("sellers[0]: housesUsed=%d listings=%d quantity=%d distinctItems=%d want 3/4/10/3",
			s0.HousesUsed, s0.Listings, s0.Quantity, s0.DistinctItems)
	}
	if s0.BuyoutValueCopper != 10000 || s0.BuyoutValueMoney != "1g" {
		t.Errorf("sellers[0] value: %d %q want 10000 \"1g\"", s0.BuyoutValueCopper, s0.BuyoutValueMoney)
	}
	if s0.PrimaryHouseID != 2 || s0.PrimaryFaction != "Alliance" || s0.ConcentrationPct != 50.0 {
		t.Errorf("sellers[0] primary: house=%d faction=%q concentration=%v want 2/Alliance/50",
			s0.PrimaryHouseID, s0.PrimaryFaction, s0.ConcentrationPct)
	}
	if s0.CharacterName != "Traderone" || s0.AccountID != 501 || !s0.IsBot {
		t.Errorf("sellers[0] identity: %q/%d/%v want Traderone/501/true", s0.CharacterName, s0.AccountID, s0.IsBot)
	}
	if len(s0.Houses) != 3 {
		t.Fatalf("sellers[0] houses len: %d want 3", len(s0.Houses))
	}
	wantHouses := []struct {
		house    int
		faction  string
		listings int
		quantity int64
		items    int
		copper   int64
		pct      float64
	}{
		{2, "Alliance", 2, 3, 2, 3000, 50.0},
		{6, "Horde", 1, 3, 1, 3000, 25.0},
		{7, "Neutral", 1, 4, 1, 4000, 25.0},
	}
	for k, w := range wantHouses {
		h := s0.Houses[k]
		if h.HouseID != w.house || h.Faction != w.faction || h.Listings != w.listings ||
			h.Quantity != w.quantity || h.DistinctItems != w.items ||
			h.BuyoutValueCopper != w.copper || h.ListingsPct != w.pct {
			t.Errorf("sellers[0].Houses[%d]: %+v want %+v", k, h, w)
		}
	}

	// Seller 30 — two houses, the second a non-standard houseid labelled house-99.
	// Both houses hold 1 listing, so the primary tie resolves to the LOWER houseId.
	s1 := sellers[1]
	if s1.HousesUsed != 2 || s1.Listings != 2 || s1.DistinctItems != 1 {
		t.Errorf("sellers[1]: housesUsed=%d listings=%d distinctItems=%d want 2/2/1",
			s1.HousesUsed, s1.Listings, s1.DistinctItems)
	}
	if s1.PrimaryHouseID != 6 || s1.ConcentrationPct != 50.0 {
		t.Errorf("sellers[1] primary: house=%d concentration=%v want 6/50 (tie -> lowest houseId)",
			s1.PrimaryHouseID, s1.ConcentrationPct)
	}
	if len(s1.Houses) != 2 || s1.Houses[1].HouseID != 99 || s1.Houses[1].Faction != "house-99" {
		t.Errorf("sellers[1] houses: %+v want the second to be houseId 99 labelled house-99", s1.Houses)
	}
	if s1.CharacterName != "Splitter" || s1.AccountID != 502 || s1.IsBot {
		t.Errorf("sellers[1] identity: %q/%d/%v want Splitter/502/false", s1.CharacterName, s1.AccountID, s1.IsBot)
	}

	// Seller 20 — single-house whale. The auction-only listing counts toward
	// listings/quantity/distinctItems but adds 0 copper.
	s2 := sellers[2]
	if s2.HousesUsed != 1 || s2.Listings != 3 || s2.Quantity != 8 || s2.DistinctItems != 3 {
		t.Errorf("sellers[2]: housesUsed=%d listings=%d quantity=%d distinctItems=%d want 1/3/8/3",
			s2.HousesUsed, s2.Listings, s2.Quantity, s2.DistinctItems)
	}
	if s2.BuyoutValueCopper != 5500 || s2.BuyoutValueMoney != "55s" {
		t.Errorf("sellers[2] value: %d %q want 5500 \"55s\" (auction-only row adds 0)", s2.BuyoutValueCopper, s2.BuyoutValueMoney)
	}
	if s2.ConcentrationPct != 100.0 || s2.PrimaryHouseID != 2 {
		t.Errorf("sellers[2] concentration: %v house=%d want 100/2", s2.ConcentrationPct, s2.PrimaryHouseID)
	}
	// Same account as seller 10 -> deduped in the bind list, still flagged a bot.
	if s2.AccountID != 501 || s2.AccountUsername != "RNDBOT_ALPHA" || !s2.IsBot {
		t.Errorf("sellers[2] identity: %d/%q/%v want 501/RNDBOT_ALPHA/true", s2.AccountID, s2.AccountUsername, s2.IsBot)
	}

	// Seller 40 — deleted character: empty name, account 0, not a bot, still folded.
	s3 := sellers[3]
	if s3.ItemOwnerGuid != 40 || s3.CharacterName != "" || s3.AccountID != 0 || s3.IsBot {
		t.Errorf("sellers[3] (deleted char): guid=%d name=%q acct=%d isBot=%v want 40/empty/0/false",
			s3.ItemOwnerGuid, s3.CharacterName, s3.AccountID, s3.IsBot)
	}
	if s3.HousesUsed != 1 || s3.Listings != 1 || s3.ConcentrationPct != 100.0 {
		t.Errorf("sellers[3]: housesUsed=%d listings=%d concentration=%v want 1/1/100",
			s3.HousesUsed, s3.Listings, s3.ConcentrationPct)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestCollectAhSellerHouseFootprint_MinHousesFilter — minHouses=2 displays only
// the cross-house sellers, while the realm census (distinctSellers, totalListings,
// houseTotals) stays whole. This is the honest-totals property: trimming the
// display list must not shrink the denominators.
func TestCollectAhSellerHouseFootprint_MinHousesFilter(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectExec(regexp.QuoteMeta("USE `acore_characters`")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta(ahFootprintScanQ)).
		WithArgs(int64(ahFootprintScanCap + 1)).
		WillReturnRows(sqlmock.NewRows(ahFootprintScanCols()).
			AddRow(int64(10), int64(2), int64(100), int64(1), int64(1000)).
			AddRow(int64(10), int64(6), int64(100), int64(1), int64(1000)).
			AddRow(int64(20), int64(2), int64(101), int64(1), int64(2000)).
			AddRow(int64(30), int64(7), int64(102), int64(1), int64(3000)))

	// Only seller 10 survives minHouses=2, so only its guid is bound.
	mock.ExpectQuery(regexp.QuoteMeta("FROM `characters` WHERE guid IN (?)")).
		WithArgs(int64(10)).
		WillReturnRows(sqlmock.NewRows([]string{"guid", "name", "account"}).
			AddRow(int64(10), "Traderone", int64(501)))
	mock.ExpectExec(regexp.QuoteMeta("USE `acore_auth`")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta("FROM `account` WHERE id IN (?)")).
		WithArgs(int64(501)).
		WillReturnRows(sqlmock.NewRows([]string{"id", "username"}).
			AddRow(int64(501), "Playerjoe"))

	out, err := collectAhSellerHouseFootprint(context.Background(), db, time.Unix(1_700_000_000, 0), 2, 10, "housesUsed")
	if err != nil {
		t.Fatalf("collectAhSellerHouseFootprint: %v", err)
	}
	sellers, _ := out["sellers"].([]*ahFootprintSeller)
	if len(sellers) != 1 || sellers[0].ItemOwnerGuid != 10 {
		t.Fatalf("sellers: %+v want only guid 10", sellers)
	}
	if m, _ := out["minHouses"].(int); m != 2 {
		t.Errorf("minHouses echo: %v want 2", out["minHouses"])
	}
	totals, _ := out["totals"].(map[string]any)
	if v, _ := totals["distinctSellers"].(int); v != 3 {
		t.Errorf("totals.distinctSellers: %v want 3 (census ignores minHouses)", totals["distinctSellers"])
	}
	if v, _ := totals["matchedSellers"].(int); v != 1 {
		t.Errorf("totals.matchedSellers: %v want 1", totals["matchedSellers"])
	}
	if v, _ := totals["crossHouseSellers"].(int); v != 1 {
		t.Errorf("totals.crossHouseSellers: %v want 1", totals["crossHouseSellers"])
	}
	if v, _ := totals["totalListings"].(int); v != 4 {
		t.Errorf("totals.totalListings: %v want 4 (census ignores minHouses)", totals["totalListings"])
	}
	if ht, _ := out["houseTotals"].([]*ahFootprintHouseTotal); len(ht) != 3 {
		t.Errorf("houseTotals len: %d want 3 (census ignores minHouses)", len(ht))
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestCollectAhSellerHouseFootprint_ScanBindsCapPlusOne — the LIMIT is bound to
// scanCap+1, which is what makes truncation detectable without a second COUNT
// query: the fold breaks at scanCap, so seeing the extra row is the signal. A
// refactor binding the bare cap would silently report truncated:false on an
// exactly-full market. scanCap+1 is also the ONLY bound arg (the scan takes no
// filters), so an added bind shows up here too.
func TestCollectAhSellerHouseFootprint_ScanBindsCapPlusOne(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.ExpectExec(regexp.QuoteMeta("USE `acore_characters`")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta(ahFootprintScanQ)).
		WithArgs(toValues([]driverArg{i(ahFootprintScanCap + 1)})...).
		WillReturnRows(sqlmock.NewRows(ahFootprintScanCols()))

	out, err := collectAhSellerHouseFootprint(context.Background(), db, time.Unix(1_700_000_000, 0), 1, 15, "housesUsed")
	if err != nil {
		t.Fatalf("collectAhSellerHouseFootprint: %v", err)
	}
	if c, _ := out["scanCap"].(int); c != ahFootprintScanCap {
		t.Errorf("scanCap echo: %v want %d", out["scanCap"], ahFootprintScanCap)
	}
	if tr, _ := out["truncated"].(bool); tr {
		t.Errorf("truncated: %v want false on an empty scan", out["truncated"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestTopNAhFootprintSellers_SortKeys — each sortBy key ranks a DIFFERENT seller
// top, proving the comparator reads the right field: housesUsed -> A (3 houses),
// listings -> B (20), value -> C (10000 copper), spread -> C (lowest
// concentration, 50 vs A's 80 and B's 100).
func TestTopNAhFootprintSellers_SortKeys(t *testing.T) {
	build := func() map[int64]*ahFootprintSeller {
		return map[int64]*ahFootprintSeller{
			10: mkFootprint(10, map[int]int{2: 8, 6: 1, 7: 1}, 100), // widest footprint
			20: mkFootprint(20, map[int]int{2: 20}, 100),            // most listings
			30: mkFootprint(30, map[int]int{2: 1, 6: 1}, 5000),      // most value, least concentrated
		}
	}
	cases := []struct {
		sortKey string
		wantTop int64
	}{
		{"housesUsed", 10},
		{"listings", 20},
		{"value", 30},
		{"spread", 30},
	}
	for _, c := range cases {
		t.Run(c.sortKey, func(t *testing.T) {
			top, _, _ := topNAhFootprintSellers(build(), 1, 10, c.sortKey)
			if len(top) != 3 {
				t.Fatalf("top len: %d want 3", len(top))
			}
			if top[0].ItemOwnerGuid != c.wantTop {
				t.Errorf("sortBy=%s top: guid %d want %d (%+v)", c.sortKey, top[0].ItemOwnerGuid, c.wantTop, top[0])
			}
		})
	}
}

// TestTopNAhFootprintSellers_SpreadIsAscending pins the one inverted key. Every
// other sortBy ranks descending; `spread` ranks concentrationPct ASCENDING, so a
// future "consistency" refactor that flips it to desc fails loudly here rather
// than silently returning the most concentrated sellers under a spread label.
func TestTopNAhFootprintSellers_SpreadIsAscending(t *testing.T) {
	m := map[int64]*ahFootprintSeller{
		10: mkFootprint(10, map[int]int{2: 3}, 100),       // concentration 100
		20: mkFootprint(20, map[int]int{2: 3, 6: 1}, 100), // concentration 75
		30: mkFootprint(30, map[int]int{2: 1, 6: 1}, 100), // concentration 50
	}
	top, _, _ := topNAhFootprintSellers(m, 1, 10, "spread")
	wantOrder := []int64{30, 20, 10}
	wantPct := []float64{50.0, 75.0, 100.0}
	for k := range wantOrder {
		if top[k].ItemOwnerGuid != wantOrder[k] || top[k].ConcentrationPct != wantPct[k] {
			t.Fatalf("spread order: got guid=%d pct=%v at %d, want guid=%d pct=%v",
				top[k].ItemOwnerGuid, top[k].ConcentrationPct, k, wantOrder[k], wantPct[k])
		}
	}
}

// TestTopNAhFootprintSellers_TieBreakIsGuidAsc — Go map iteration is randomized,
// so without the guid tiebreak the ranking would flake. Run the identical fixture
// repeatedly and assert a stable order.
func TestTopNAhFootprintSellers_TieBreakIsGuidAsc(t *testing.T) {
	for run := 0; run < 20; run++ {
		m := map[int64]*ahFootprintSeller{
			30: mkFootprint(30, map[int]int{2: 1, 6: 1}, 100),
			10: mkFootprint(10, map[int]int{2: 1, 6: 1}, 100),
			20: mkFootprint(20, map[int]int{2: 1, 6: 1}, 100),
		}
		top, _, _ := topNAhFootprintSellers(m, 1, 10, "housesUsed")
		if top[0].ItemOwnerGuid != 10 || top[1].ItemOwnerGuid != 20 || top[2].ItemOwnerGuid != 30 {
			t.Fatalf("run %d: unstable tie order %d,%d,%d want 10,20,30",
				run, top[0].ItemOwnerGuid, top[1].ItemOwnerGuid, top[2].ItemOwnerGuid)
		}
	}
}

// TestTopNAhFootprintSellers_TopNSlice — the slice trims the display list while
// crossHouseSellers / matchedSellers stay pre-slice.
func TestTopNAhFootprintSellers_TopNSlice(t *testing.T) {
	m := map[int64]*ahFootprintSeller{
		10: mkFootprint(10, map[int]int{2: 1, 6: 1, 7: 1}, 100),
		20: mkFootprint(20, map[int]int{2: 1, 6: 1}, 100),
		30: mkFootprint(30, map[int]int{2: 1}, 100),
	}
	top, crossHouse, matched := topNAhFootprintSellers(m, 1, 1, "housesUsed")
	if len(top) != 1 || top[0].ItemOwnerGuid != 10 {
		t.Errorf("top: %+v want just guid 10", top)
	}
	if crossHouse != 2 {
		t.Errorf("crossHouseSellers: %d want 2 (pre-slice)", crossHouse)
	}
	if matched != 3 {
		t.Errorf("matchedSellers: %d want 3 (pre-slice)", matched)
	}
}

// TestAhFootprintSeller_FinalizeSingleHouse — the degenerate case the whole tool
// contrasts against: one house means concentration 100 and listingsPct 100.
func TestAhFootprintSeller_FinalizeSingleHouse(t *testing.T) {
	s := mkFootprint(7, map[int]int{7: 4}, 250)
	if s.HousesUsed != 1 || s.ConcentrationPct != 100.0 {
		t.Errorf("housesUsed=%d concentration=%v want 1/100", s.HousesUsed, s.ConcentrationPct)
	}
	if s.PrimaryHouseID != 7 || s.PrimaryFaction != "Neutral" {
		t.Errorf("primary: %d/%q want 7/Neutral", s.PrimaryHouseID, s.PrimaryFaction)
	}
	if len(s.Houses) != 1 || s.Houses[0].ListingsPct != 100.0 {
		t.Errorf("houses: %+v want a single house at 100%%", s.Houses)
	}
	if s.BuyoutValueCopper != 1000 || s.BuyoutValueMoney != "10s" {
		t.Errorf("value: %d %q want 1000 \"10s\"", s.BuyoutValueCopper, s.BuyoutValueMoney)
	}
}

// TestAhFootprintSeller_FoldCountsAuctionOnly — a listing with buyoutprice 0 is
// inventory parked on a house: it counts in listings/quantity/distinctItems and
// adds 0 copper. Filtering it out (as the priced-only siblings do) would
// under-report the footprint this tool exists to measure.
func TestAhFootprintSeller_FoldCountsAuctionOnly(t *testing.T) {
	s := &ahFootprintSeller{ItemOwnerGuid: 1}
	s.fold(2, 100, 3, 0)
	s.fold(2, 101, 1, 500)
	s.finalize()
	if s.Listings != 2 || s.Quantity != 4 || s.DistinctItems != 2 {
		t.Errorf("listings=%d quantity=%d distinctItems=%d want 2/4/2", s.Listings, s.Quantity, s.DistinctItems)
	}
	if s.BuyoutValueCopper != 500 {
		t.Errorf("buyoutValueCopper: %d want 500 (the auction-only row adds 0)", s.BuyoutValueCopper)
	}
}

// TestAhFootprintHouseTotals_SellersOverlap — a seller present on N houses is
// counted once PER HOUSE, so summing houseTotals[].sellers exceeds
// distinctSellers by exactly the cross-house overlap. Also pins the houseId-asc
// ordering and the grand totals.
func TestAhFootprintHouseTotals_SellersOverlap(t *testing.T) {
	m := map[int64]*ahFootprintSeller{
		10: mkFootprint(10, map[int]int{2: 1, 6: 1, 7: 1}, 100),
		20: mkFootprint(20, map[int]int{2: 2}, 100),
	}
	totals, listings, quantity, value := ahFootprintHouseTotals(m)
	if len(totals) != 3 {
		t.Fatalf("houseTotals len: %d want 3", len(totals))
	}
	if totals[0].HouseID != 2 || totals[1].HouseID != 6 || totals[2].HouseID != 7 {
		t.Errorf("houseTotals order: %d,%d,%d want 2,6,7", totals[0].HouseID, totals[1].HouseID, totals[2].HouseID)
	}
	sellerSum := 0
	for _, tot := range totals {
		sellerSum += tot.Sellers
	}
	if sellerSum != 4 {
		t.Errorf("sum(houseTotals.sellers): %d want 4 (2 distinct sellers, 4 house-presences)", sellerSum)
	}
	if totals[0].Sellers != 2 || totals[0].Listings != 3 {
		t.Errorf("house 2: sellers=%d listings=%d want 2/3", totals[0].Sellers, totals[0].Listings)
	}
	if listings != 5 || quantity != 5 || value != 500 {
		t.Errorf("grand totals: listings=%d quantity=%d value=%d want 5/5/500", listings, quantity, value)
	}
}
