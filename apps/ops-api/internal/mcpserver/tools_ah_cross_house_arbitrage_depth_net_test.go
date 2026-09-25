package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

// ahNetRates is the fixture rate table. House 7 (Neutral) carries the bigger
// consignment cut than the faction houses, which is the asymmetry the whole
// net-of-cut extension exists to surface: cross-house rows disproportionately
// have the neutral house on one leg.
func ahNetRates(rateCut float64) *ahAuctionRates {
	h2 := &ahHouseRate{HouseID: 2, Faction: ahHouseFaction(2), CutPercent: 5, DepositPercent: 15}
	h6 := &ahHouseRate{HouseID: 6, Faction: ahHouseFaction(6), CutPercent: 5, DepositPercent: 15}
	h7 := &ahHouseRate{HouseID: 7, Faction: ahHouseFaction(7), CutPercent: 15, DepositPercent: 25}
	return &ahAuctionRates{
		Source:         ahAuctionRatesSource,
		RateAuctionCut: rateCut,
		Houses:         map[int]*ahHouseRate{2: h2, 6: h6, 7: h7},
		HouseList:      []*ahHouseRate{h2, h6, h7},
	}
}

// ahNetFixture builds three items whose GROSS ranking and NET ranking disagree —
// the property that makes the default sort change meaningful rather than
// cosmetic:
//
//	item 100  buy 1000 x100 on Alliance, sell 1200 on NEUTRAL (15% cut)
//	          gross 20000, cut 18000  -> net   2000
//	item 200  buy 1000 x100 on Alliance, sell 1150 on Horde   ( 5% cut)
//	          gross 15000, cut  5750  -> net   9250
//	item 300  buy 1000 x10  on Alliance, sell 1100 on NEUTRAL (15% cut)
//	          gross  1000, cut  1650  -> net   -650   <- a POSITIVE gross that is a LOSS
//
// item 300 is the backlog item's exact scenario: a 10% spread against a 15% cut,
// reported as a profit by the gross-only view.
//
// Every "buy 1000 xN" above is a UNIT price, which is what the gross figures in
// the table were computed from (100 units at a 200c unit spread = 20000). The
// buy-side rows therefore carry a listing price of 1000*N — a 100-unit stack at
// 1000c per unit is a 100000c ticket, not a 1000c one. The rows used to say 1000
// flat, which under a per-listing reading was indistinguishable from the unit
// price and is now a 10c/unit floor. Stating the ticket explicitly leaves every
// gross, cut and net assertion in this file byte-identical. The sell-side rows
// are single-unit, so their ticket and unit prices coincide and are unchanged.
func ahNetFixture() (*sqlmock.Rows, *sqlmock.Rows) {
	floors := sqlmock.NewRows(ahDepthFloorCols()).
		AddRow(2, 100, ahDepthUnitFloor(100000, 100)).AddRow(7, 100, ahDepthUnitFloor(1200, 1)).
		AddRow(2, 200, ahDepthUnitFloor(100000, 100)).AddRow(6, 200, ahDepthUnitFloor(1150, 1)).
		AddRow(2, 300, ahDepthUnitFloor(10000, 10)).AddRow(7, 300, ahDepthUnitFloor(1100, 1))
	scan := sqlmock.NewRows(ahDepthScanCols()).
		AddRow(2, 100, 11, 100000, 100, "Item100", 2, 7).
		AddRow(7, 100, 12, 1200, 1, "Item100", 2, 7).
		AddRow(2, 200, 21, 100000, 100, "Item200", 2, 7).
		AddRow(6, 200, 22, 1150, 1, "Item200", 2, 7).
		AddRow(2, 300, 31, 10000, 10, "Item300", 2, 7).
		AddRow(7, 300, 32, 1100, 1, "Item300", 2, 7)
	return floors, scan
}

func ahNetCollect(t *testing.T, sortKey string, rates *ahAuctionRates) map[string]any {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	floors, scan := ahNetFixture()
	ahDepthExpectBothQueries(mock, floors, scan)
	out, err := collectAhCrossHouseArbitrageDepth(context.Background(), db,
		time.Unix(1_700_000_000, 0), 15, 2, sortKey, ahDepthDefaultMinFloorQty, nil, rates)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
	return out
}

func ahNetItems(t *testing.T, out map[string]any) []*ahDepthItem {
	t.Helper()
	items, ok := out["items"].([]*ahDepthItem)
	if !ok {
		t.Fatalf("items has type %T, want []*ahDepthItem", out["items"])
	}
	return items
}

// TestAhDepthNet_GrossPositiveButNetNegative is the headline regression: a 10%
// spread resold on the 15%-cut neutral house is a LOSS, and the tool must say so
// while still reporting the positive gross it is derived from.
func TestAhDepthNet_GrossPositiveButNetNegative(t *testing.T) {
	out := ahNetCollect(t, "netGain", ahNetRates(1))
	for _, it := range ahNetItems(t, out) {
		if it.ItemEntry != 300 {
			continue
		}
		if it.PotentialGainCopper != 1000 {
			t.Errorf("gross = %d want 1000", it.PotentialGainCopper)
		}
		if it.ResaleCutCopper != 1650 {
			t.Errorf("cut = %d want 1650 (15%% of 10*1100)", it.ResaleCutCopper)
		}
		if it.NetGainCopper != -650 {
			t.Errorf("net = %d want -650", it.NetGainCopper)
		}
		if !it.NetGainKnown {
			t.Error("netGainKnown = false, want true (rates resolved)")
		}
		if it.Profitable {
			t.Error("profitable = true for a net LOSS")
		}
		if it.PriciestHouseID != 7 {
			t.Errorf("priciest house = %d want 7 (neutral)", it.PriciestHouseID)
		}
		if it.ResaleCutPercent != 15 {
			t.Errorf("resaleCutPercent = %v want 15", it.ResaleCutPercent)
		}
		// net/sweep = -650/10000 -> -6.5%
		if it.NetMarginPct != -6.5 {
			t.Errorf("netMarginPct = %v want -6.5", it.NetMarginPct)
		}
		return
	}
	t.Fatal("item 300 missing from results")
}

// TestAhDepthNet_LossRendersWithMinusSign pins the money-helper choice. This is
// the one signed money field in the tool, and formatCopper — used by every other
// money string here — clamps anything <=0 to "0c". Rendering a loss as "0c"
// would hide exactly what this change exposes, so the assertion is on the
// STRING, not just the integer.
func TestAhDepthNet_LossRendersWithMinusSign(t *testing.T) {
	out := ahNetCollect(t, "netGain", ahNetRates(1))
	for _, it := range ahNetItems(t, out) {
		if it.ItemEntry != 300 {
			continue
		}
		if want := "-6s 50c"; it.NetGainMoney != want {
			t.Errorf("netGainMoney = %q want %q", it.NetGainMoney, want)
		}
		if it.NetGainMoney == "0c" {
			t.Error("a net LOSS rendered as 0c — formatCopper's clamp leaked in")
		}
		// Guard the helper choice directly so a future refactor that swaps
		// formatMoney back to formatCopper fails here with a clear reason.
		if formatCopper(it.NetGainCopper) != "0c" {
			t.Fatal("formatCopper no longer clamps negatives; revisit the helper choice comment")
		}
		return
	}
	t.Fatal("item 300 missing from results")
}

// TestAhDepthNet_NetRankingDiffersFromGross proves the net figure is not a
// cosmetic extra field: ranking by it reorders the leaderboard, so the tool's
// top recommendation genuinely changes.
func TestAhDepthNet_NetRankingDiffersFromGross(t *testing.T) {
	gross := ahNetItems(t, ahNetCollect(t, "gain", ahNetRates(1)))
	net := ahNetItems(t, ahNetCollect(t, "netGain", ahNetRates(1)))

	gotGross := []int64{gross[0].ItemEntry, gross[1].ItemEntry, gross[2].ItemEntry}
	gotNet := []int64{net[0].ItemEntry, net[1].ItemEntry, net[2].ItemEntry}
	wantGross := []int64{100, 200, 300} // 20000, 15000, 1000
	wantNet := []int64{200, 100, 300}   // 9250, 2000, -650

	for i := range wantGross {
		if gotGross[i] != wantGross[i] {
			t.Errorf("gross order = %v want %v", gotGross, wantGross)
			break
		}
	}
	for i := range wantNet {
		if gotNet[i] != wantNet[i] {
			t.Errorf("net order = %v want %v", gotNet, wantNet)
			break
		}
	}
	if gotGross[0] == gotNet[0] {
		t.Errorf("gross and net picked the same top row (%d) — fixture no longer proves the reorder", gotGross[0])
	}
}

// TestAhDepthNet_CensusCountsNetNegativeRows — the honest census must count the
// rows a gross-only reading would have sold as opportunities.
func TestAhDepthNet_CensusCountsNetNegativeRows(t *testing.T) {
	out := ahNetCollect(t, "netGain", ahNetRates(1))
	totals, _ := out["totals"].(map[string]any)
	got, ok := totals["matchedItemsNetNegative"].(int)
	if !ok {
		t.Fatalf("matchedItemsNetNegative missing or not int: %T", totals["matchedItemsNetNegative"])
	}
	if got != 1 {
		t.Errorf("matchedItemsNetNegative = %d want 1 (item 300)", got)
	}
}

// TestAhDepthNet_UnknownRatesFallBackToGross — with no rate table the tool must
// report the gross as its net ESTIMATE and flag it, never default the net to
// zero (which would rank every row identically while looking computed) and never
// claim a row is profitable on an uncomputed cut.
func TestAhDepthNet_UnknownRatesFallBackToGross(t *testing.T) {
	out := ahNetCollect(t, "netGain", nil)
	for _, it := range ahNetItems(t, out) {
		if it.NetGainKnown {
			t.Errorf("item %d: netGainKnown = true with nil rates", it.ItemEntry)
		}
		if it.NetGainCopper != it.PotentialGainCopper {
			t.Errorf("item %d: net %d != gross %d — unknown net must fall back to gross",
				it.ItemEntry, it.NetGainCopper, it.PotentialGainCopper)
		}
		if it.Profitable {
			t.Errorf("item %d: profitable = true on an uncomputed cut", it.ItemEntry)
		}
		if it.ResaleCutCopper != 0 {
			t.Errorf("item %d: resaleCutCopper = %d want 0", it.ItemEntry, it.ResaleCutCopper)
		}
	}
	rates, _ := out["rates"].(map[string]any)
	if resolved, _ := rates["resolved"].(bool); resolved {
		t.Error("rates.resolved = true with nil rates")
	}
	if src, _ := rates["source"].(string); src != ahAuctionRatesSource {
		t.Errorf("rates.source = %q, want the table named even when unresolved", src)
	}
	// The net-negative census is meaningless without rates and must be absent
	// rather than a misleading 0.
	totals, _ := out["totals"].(map[string]any)
	if _, present := totals["matchedItemsNetNegative"]; present {
		t.Error("matchedItemsNetNegative reported despite unresolved rates")
	}
}

// TestAhDepthNet_RatesBlockReportsAssumptions — a net figure must never be
// reported without the multiplier and table that produced it.
func TestAhDepthNet_RatesBlockReportsAssumptions(t *testing.T) {
	out := ahNetCollect(t, "netGain", ahNetRates(1))
	rates, _ := out["rates"].(map[string]any)
	if resolved, _ := rates["resolved"].(bool); !resolved {
		t.Error("rates.resolved = false")
	}
	if got, _ := rates["rateAuctionCut"].(float64); got != 1 {
		t.Errorf("rateAuctionCut = %v want 1", got)
	}
	houses, ok := rates["houses"].([]*ahHouseRate)
	if !ok || len(houses) != 3 {
		t.Fatalf("rates.houses = %T len %d, want 3 entries", rates["houses"], len(houses))
	}
	detail, _ := rates["detail"].(string)
	if !strings.Contains(detail, "refunded") {
		t.Errorf("rates.detail must explain that the deposit is refunded on a sale, got %q", detail)
	}
}

// TestAhAuctionRates_CutMirrorsCoreTruncations pins the arithmetic against
// AuctionEntry::GetAuctionCut, which truncates TWICE: CalculatePct instantiates
// T as the uint32 bid, so the percentage lands on a whole copper BEFORE the
// Rate.Auction.Cut multiplier, and the product is truncated again by the int32
// cast. The sale=13 / 15% / rate=3 case is chosen because the two-step result
// (3) and the collapsed single-multiply result (5) DIFFER — so this test fails
// if anyone "simplifies" the expression.
func TestAhAuctionRates_CutMirrorsCoreTruncations(t *testing.T) {
	cases := []struct {
		name    string
		sale    int64
		rateCut float64
		want    int64
	}{
		{"two truncations differ from one multiply", 13, 3, 3}, // collapsed would be 5
		{"exact percentage", 11000, 1, 1650},
		{"fractional truncates down", 999, 1, 149},
		{"zero sale", 0, 1, 0},
		{"zero rate", 11000, 0, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, known := ahNetRates(c.rateCut).cutCopper(7, c.sale) // house 7 = 15%
			if !known {
				t.Fatal("known = false for a house present in the table")
			}
			if got != c.want {
				t.Errorf("cutCopper(%d) = %d want %d", c.sale, got, c.want)
			}
		})
	}
	// Collapsing the two truncations is the specific regression this guards, so
	// assert the fixture still tells the two forms apart. The operands go through
	// variables because Go folds a constant expression at compile time and then
	// refuses the truncating int64 conversion outright.
	sale, pct, rate := int64(13), 15.0, 3.0
	if collapsed := int64(float64(sale) * pct / 100 * rate); collapsed == 3 {
		t.Fatal("fixture no longer distinguishes the two-step form; pick another sale value")
	}
}

// TestAhAuctionRates_CutClampedToSale — a customised DBC plus a large multiplier
// can exceed the proceeds; the cut is capped so the net lands at exactly the
// buy cost rather than inventing a loss bigger than the trade.
func TestAhAuctionRates_CutClampedToSale(t *testing.T) {
	got, known := ahNetRates(100).cutCopper(7, 10)
	if !known {
		t.Fatal("known = false")
	}
	if got != 10 {
		t.Errorf("cutCopper = %d want 10 (clamped to the sale)", got)
	}
}

// TestAhAuctionRates_UnknownHouseIsNotZero — a house absent from the rate table
// must report unknown, not a free 0% cut.
func TestAhAuctionRates_UnknownHouseIsNotZero(t *testing.T) {
	got, known := ahNetRates(1).cutCopper(99, 10000)
	if known {
		t.Error("known = true for a house absent from the table")
	}
	if got != 0 {
		t.Errorf("cutCopper = %d want 0 alongside known=false", got)
	}
	var nilRates *ahAuctionRates
	if _, known := nilRates.cutCopper(7, 10000); known {
		t.Error("nil rates reported known = true")
	}
}

// TestResolveAhAuctionRates_Golden — the rate lookup maps auctionhouse_dbc's
// ConsignmentRate to the CUT and DepositRate to the deposit, per the DBC struct
// field order (depositPercent index 2, cutPercent index 3).
func TestResolveAhAuctionRates_Golden(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.ExpectQuery(regexp.QuoteMeta(ahAuctionRatesQuery)).
		WillReturnRows(sqlmock.NewRows(ahDepthRateCols()).
			AddRow(2, 5, 15).
			AddRow(6, 5, 15).
			AddRow(7, 15, 25))

	rates, err := resolveAhAuctionRates(context.Background(), db, 1)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if rates == nil {
		t.Fatal("rates = nil for a populated table")
	}
	if len(rates.Houses) != 3 {
		t.Fatalf("houses = %d want 3", len(rates.Houses))
	}
	if got := rates.Houses[7].CutPercent; got != 15 {
		t.Errorf("house 7 cutPercent = %v want 15 (ConsignmentRate)", got)
	}
	if got := rates.Houses[7].DepositPercent; got != 25 {
		t.Errorf("house 7 depositPercent = %v want 25 (DepositRate)", got)
	}
	if got := rates.Houses[7].Faction; got != ahHouseFaction(7) {
		t.Errorf("house 7 faction = %q want %q", got, ahHouseFaction(7))
	}
	if rates.Source != ahAuctionRatesSource {
		t.Errorf("source = %q want %q", rates.Source, ahAuctionRatesSource)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// TestResolveAhAuctionRates_EmptyTableIsNilNotError — auctionhouse_dbc ships
// EMPTY in AzerothCore's base SQL and is filled by the client-data import, so an
// unpopulated table is an expected state, not a failure.
func TestResolveAhAuctionRates_EmptyTableIsNilNotError(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.ExpectQuery(regexp.QuoteMeta(ahAuctionRatesQuery)).
		WillReturnRows(sqlmock.NewRows(ahDepthRateCols()))

	rates, err := resolveAhAuctionRates(context.Background(), db, 1)
	if err != nil {
		t.Fatalf("empty table returned an error: %v", err)
	}
	if rates != nil {
		t.Errorf("rates = %+v want nil for an empty table", rates)
	}
}

// TestResolveAhAuctionRates_QueryErrorSurfaces — an access-denied on acore_world
// must NOT masquerade as an unpopulated table; the caller reports it separately.
func TestResolveAhAuctionRates_QueryErrorSurfaces(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.ExpectQuery(regexp.QuoteMeta(ahAuctionRatesQuery)).
		WillReturnError(errors.New("SELECT command denied to user 'ops_ro'"))

	rates, err := resolveAhAuctionRates(context.Background(), db, 1)
	if err == nil {
		t.Fatal("expected an error")
	}
	if rates != nil {
		t.Error("rates non-nil alongside an error")
	}
	if !strings.Contains(err.Error(), "denied") {
		t.Errorf("error lost its cause: %v", err)
	}
}

// TestAhDepthNet_RateAuctionCutRejectedOutOfRange — a negative multiplier would
// invert the arithmetic and a wild one would report every trade as a disaster;
// both are bad filters, so they are rejected rather than clamped (the same
// treatment minQuality gets).
func TestAhDepthNet_RateAuctionCutRejectedOutOfRange(t *testing.T) {
	for _, bad := range []string{`{"rateAuctionCut":-1}`, `{"rateAuctionCut":1000}`} {
		db, _, err := sqlmock.New()
		if err != nil {
			t.Fatal(err)
		}
		reg := NewRegistry()
		RegisterAhCrossHouseArbitrageDepthTool(reg, DBDeps{QueryDB: db})
		tool, _ := reg.Get("ah_cross_house_arbitrage_depth")
		resp := tool.Handler(context.Background(), json.RawMessage(bad), "")
		got, _ := resp.(map[string]any)
		msg, _ := got["error"].(string)
		if !strings.Contains(msg, "rateAuctionCut out of range") {
			t.Errorf("args %s: expected out-of-range error, got %v", bad, got)
		}
		db.Close()
	}
}

// TestAhDepthNet_HandlerResolvesRatesAndNets drives the REGISTERED handler end to
// end, which is the only path that actually issues the rate lookup — the
// collectors take an already-resolved table.
func TestAhDepthNet_HandlerResolvesRatesAndNets(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ahDepthExpectRatesQuery(mock, sqlmock.NewRows(ahDepthRateCols()).
		AddRow(2, 5, 15).AddRow(6, 5, 15).AddRow(7, 15, 25))
	floors, scan := ahNetFixture()
	ahDepthExpectBothQueries(mock, floors, scan)

	reg := NewRegistry()
	RegisterAhCrossHouseArbitrageDepthTool(reg, DBDeps{QueryDB: db})
	tool, _ := reg.Get("ah_cross_house_arbitrage_depth")
	resp := tool.Handler(context.Background(), json.RawMessage(`{}`), "")
	got, _ := resp.(map[string]any)
	if got["error"] != nil {
		t.Fatalf("unexpected error: %v", got)
	}
	// Default sortBy must now be the net ranking.
	if sortBy, _ := got["sortBy"].(string); sortBy != "netGain" {
		t.Errorf("default sortBy = %q want netGain", sortBy)
	}
	items := ahNetItems(t, got)
	if len(items) != 3 {
		t.Fatalf("items = %d want 3", len(items))
	}
	if items[0].ItemEntry != 200 {
		t.Errorf("top row = %d want 200 (best NET, not best gross)", items[0].ItemEntry)
	}
	if !items[0].NetGainKnown || items[0].NetGainCopper != 9250 {
		t.Errorf("top net = %d (known %v) want 9250", items[0].NetGainCopper, items[0].NetGainKnown)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestAhDepthNet_RateLookupFailureIsNonFatal — the arbitrage fold is still worth
// returning when the optional rate enricher fails, and the reason must be
// surfaced rather than presented as an empty DBC table.
func TestAhDepthNet_RateLookupFailureIsNonFatal(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.ExpectQuery(regexp.QuoteMeta(ahAuctionRatesQuery)).
		WillReturnError(errors.New("SELECT command denied to user 'ops_ro'"))
	floors, scan := ahNetFixture()
	ahDepthExpectBothQueries(mock, floors, scan)

	reg := NewRegistry()
	RegisterAhCrossHouseArbitrageDepthTool(reg, DBDeps{QueryDB: db})
	tool, _ := reg.Get("ah_cross_house_arbitrage_depth")
	resp := tool.Handler(context.Background(), json.RawMessage(`{}`), "")
	got, _ := resp.(map[string]any)
	if got["error"] != nil {
		t.Fatalf("rate failure took down the whole tool: %v", got["error"])
	}
	items := ahNetItems(t, got)
	if len(items) != 3 {
		t.Fatalf("items = %d want 3 — the fold must survive a rate failure", len(items))
	}
	rates, _ := got["rates"].(map[string]any)
	if resolved, _ := rates["resolved"].(bool); resolved {
		t.Error("rates.resolved = true after a lookup failure")
	}
	reason, _ := rates["error"].(string)
	if !strings.Contains(reason, "denied") {
		t.Errorf("rates.error must carry the cause, got %q", reason)
	}
}

// TestAhCrossHouseArbitrageDepth_NetDescriptionMentionsContext — the keyword test
// asserts the REGISTERED Description string (not the doc-comment), matching the
// house convention.
func TestAhCrossHouseArbitrageDepth_NetDescriptionMentionsContext(t *testing.T) {
	reg := NewRegistry()
	RegisterAhCrossHouseArbitrageDepthTool(reg, DBDeps{})
	tool, ok := reg.Get("ah_cross_house_arbitrage_depth")
	if !ok {
		t.Fatal("tool not registered")
	}
	for _, kw := range []string{
		"netGainCopper", "resaleCutCopper", "resaleCutPercent", "netMarginPct", "profitable",
		"netGainKnown", "auctionhouse_dbc", "ConsignmentRate", "Rate.Auction.Cut",
		"rateAuctionCut", "matchedItemsNetNegative", "refunds it in full",
		"AllowTwoSideInteraction.Auction",
	} {
		if !strings.Contains(tool.Description, kw) {
			t.Errorf("Description missing %q", kw)
		}
	}
}
