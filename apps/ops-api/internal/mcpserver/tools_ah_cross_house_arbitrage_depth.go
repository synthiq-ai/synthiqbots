package mcpserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"time"
)

const (
	ahDepthTimeout            = 10 * time.Second
	ahDepthScanCap            = 200000 // hard ceiling on auctionhouse listing rows folded into one snapshot
	ahDepthDefaultTop         = 15
	ahDepthMaxTop             = 100
	ahDepthMaxQuality         = 7 // AzerothCore item Quality enum tops out at 7 (Heirloom)
	ahDepthDefaultMinHouses   = 2 // an item on only 1 house has no cross-house floor to compare
	ahDepthMaxHouses          = 3 // Alliance/Horde/Neutral — there are only 3 AuctionHouseIds
	ahDepthDefaultMinFloorQty = 1 // a no-op default: every matched floor holds at least 1 unit

	// ahDepthUnitScale expresses a per-UNIT price as an integer in ten-thousandths
	// of a copper, and it is load-bearing in two places that must agree BYTE FOR
	// BYTE: the SQL floor map's MIN(FLOOR(buyoutprice * scale / count)) and the Go
	// fold's buyout * scale / count. Both floor at the same scale on positive
	// operands, so a listing matches its own house's floor exactly.
	//
	// The scale is what makes the floor comparison safe on stacks. A bare
	// buyoutprice/count in integer arithmetic truncates, so a 3-copper 2-unit stack
	// would collapse to a unit price of 1 and stop matching itself; at this scale it
	// is 15000 and matches exactly. Two listings whose true unit prices differ by
	// less than 1/10000 of a copper are treated as the same floor, which is far
	// below the granularity of the atomic currency unit.
	//
	// Overflow: buyoutprice is bounded by MAX_MONEY_AMOUNT (~2^31), so the scaled
	// product tops out around 2.1e13 — three orders of magnitude inside int64 and
	// inside MySQL's BIGINT.
	ahDepthUnitScale = 10000
)

// ahDepthSortKeys is the Go-side sortBy allowlist. Every key ranks a value folded
// in Go (the floor-match quantity sum, the cross-house cheapest/priciest pick),
// not a single SQL aggregate, so there is no ORDER BY expression to interpolate —
// the fold-in-Go idiom ah_floor_saturation / ah_undercutters / #377 established.
// Validated before any query runs.
var ahDepthSortKeys = map[string]bool{
	"gain":          true, // potentialGainCopper desc (depth-weighted GROSS, ignores the cut)
	"netGain":       true, // default — netGainCopper desc, the gross MINUS the resale house's cut
	"spread":        true, // spreadPct desc — #377's headline metric, on UNIT prices here
	"depth":         true, // cheapestFloorQuantity desc — the most units buyable at the cheap floor
	"cheapestFloor": true, // cheapestFloorUnitCopper asc — lowest per-unit buy-in first
}

// ahDepthFloorMapQuery computes each (house, item) pair's buyout floor as a
// per-UNIT price, scaled by ahDepthUnitScale. This is the AUTHORITATIVE floor: a
// GROUP BY bounded by (item variety * 3 houses) with NO LIMIT, so unlike the
// capped listing scan below it can never be truncated out from under the floor
// comparison — the same split ah_floor_saturation uses, widened from its
// single-house floor map to the per-house grain #377 folds.
//
// UNIT price, not listing price, and that distinction is the whole point.
// `auctionhouse.buyoutprice` is the price of the WHOLE listing whatever its stack
// size — AzerothCore reads it off CMSG_AUCTION_SELL_ITEM as one scalar
// (`AH->buyout = buyout`) and a buyout pays it once
// (`ModifyMoney(-int32(auction->buyout))`) regardless of `auction->itemCount`.
// So MIN(buyoutprice) answers "the cheapest listing", NOT "the cheapest units",
// and on a market where the floor is a stack of 20 those are different rows at a
// 20x different price. Comparing a stack listing on one house against a
// single-unit listing on another is not an arbitrage signal at all.
//
// minQuality is deliberately NOT applied here: quality is a static per-item
// attribute that cannot change which rows share an itemEntry, so it only needs
// to gate which items the listing scan folds.
//
// `ii.count > 0` guards the division. A non-positive stack size has no unit price
// and would make the quotient NULL; the listing scan keeps such rows visible and
// counts them (listingsSkippedNonPositiveCount) rather than dropping them
// silently.
const ahDepthFloorMapQuery = "SELECT ah.houseid, ii.itemEntry, " +
	"MIN(FLOOR(ah.buyoutprice * 10000 / ii.count)) AS unitFloorScaled " +
	"FROM `auctionhouse` ah JOIN `item_instance` ii ON ah.itemguid = ii.guid " +
	"WHERE ah.buyoutprice > 0 AND ii.count > 0 GROUP BY ah.houseid, ii.itemEntry"

// ahDepthScanQuery is the per-listing fold source — the stage that carries the
// STOCK DEPTH #377 could not see. #377 folds one grouped MIN(buyoutprice) row
// per (house, item) and therefore knows the floor PRICE but nothing about how
// many units sit on it; summing ii.count per listing is only possible at listing
// grain. item_template is joined for the display name/quality/class and the
// minQuality gate. LIMIT is bound to scanCap+1 so truncation is detectable
// without a separate COUNT.
const ahDepthScanQuery = "SELECT ah.houseid, ii.itemEntry, ah.itemowner, ah.buyoutprice, ii.count, " +
	"it.name, it.Quality, it.class " +
	"FROM `auctionhouse` ah JOIN `item_instance` ii ON ah.itemguid = ii.guid " +
	"JOIN `acore_world`.`item_template` it ON ii.itemEntry = it.entry " +
	"WHERE ah.buyoutprice > 0"

// ahDepthHouse is one item's presence on ONE auction house: the floor price plus
// the depth sitting on it. FloorKnown is false only when the floor map had no row
// for this (house, item) pair — possible when a listing is INSERTed between the
// two queries — and such a house is folded into the totals but excluded from
// housesListed and the cheapest/priciest walk, since a floor comparison against
// an unknown floor is meaningless. FloorSellers counts DISTINCT itemowner guids
// at the floor: a floor held by one seller is a fragile, one-reprice-away
// opportunity even when its quantity is large.
type ahDepthHouse struct {
	HouseID    int    `json:"houseId"`
	Faction    string `json:"faction"`
	FloorKnown bool   `json:"floorKnown"`
	// FloorUnitCopper is the floor per-UNIT price and is the basis every price
	// comparison and money figure in this tool now uses. Fractional by nature: a
	// 3-copper 2-unit stack floors at 1.5 copper per unit.
	FloorUnitCopper float64 `json:"floorUnitCopper"`
	// FloorCopper is the cheapest single LISTING price among the listings sitting
	// at FloorUnitCopper — what one buyout ticket costs, not what one unit costs.
	// It is 0 when no scanned listing matched (see FloorQuantity).
	FloorCopper int64 `json:"floorCopper"`
	// FloorCostCopper is the EXACT capital needed to buy out every listing at this
	// house's floor unit price: the sum of their buyoutprice values. Summed, never
	// derived as quantity * price — those agree only when every floor listing is a
	// single unit.
	FloorCostCopper int64 `json:"floorCostCopper"`
	FloorQuantity   int64 `json:"floorQuantity"`
	FloorListings   int   `json:"floorListings"`
	FloorSellers    int   `json:"floorSellers"`
	TotalListings   int   `json:"totalListings"`
	TotalQuantity   int64 `json:"totalQuantity"`

	// floorUnitScaled is the floor unit price in ahDepthUnitScale units, straight
	// from the floor map — the exact integer the fold matches against, kept
	// unexported so the serialized surface carries only the copper form.
	floorUnitScaled int64
	// floorOwners tracks distinct itemowner guids seen at this house's floor;
	// unexported -> not serialized, collapsed into FloorSellers by finalize().
	floorOwners map[int64]bool
}

// fold adds one scanned listing row to this house's running totals.
//
// The floor test is on the listing's UNIT price, computed with the identical
// expression the floor map used, so a listing that IS its house's floor always
// matches itself whatever its stack size. Callers guarantee count > 0.
func (h *ahDepthHouse) fold(itemOwner, buyout, count int64) {
	h.TotalListings++
	h.TotalQuantity += count
	if !h.FloorKnown || buyout*ahDepthUnitScale/count != h.floorUnitScaled {
		return
	}
	h.FloorListings++
	h.FloorQuantity += count
	// Exact capital: what this listing actually costs to buy out.
	h.FloorCostCopper += buyout
	if h.FloorCopper == 0 || buyout < h.FloorCopper {
		h.FloorCopper = buyout
	}
	if h.floorOwners == nil {
		h.floorOwners = map[int64]bool{}
	}
	h.floorOwners[itemOwner] = true
}

// ahDepthItem is one item TYPE's cross-house arbitrage row, carrying the stock
// depth behind each floor. SpreadPct reuses ahCategoryValuePct(max-min, min) so
// the depth view and #377 report the identical spread figure from one formula
// with one div-by-zero guard. SweepCostCopper is the capital needed to clear the
// cheap floor outright and PotentialGainCopper the notional profit of reselling
// that whole stock at the priciest house's floor — the quantity-weighted numbers
// a floor-price-only view cannot produce.
type ahDepthItem struct {
	ItemEntry             int64           `json:"itemEntry"`
	ItemName              string          `json:"itemName"`
	Quality               int             `json:"quality"`
	QualityName           string          `json:"qualityName"`
	Class                 int             `json:"class"`
	ClassName             string          `json:"className"`
	HousesListed          int             `json:"housesListed"`
	Houses                []*ahDepthHouse `json:"houses"`
	CheapestHouseID       int             `json:"cheapestHouseId"`
	CheapestFaction       string          `json:"cheapestFaction"`
	CheapestFloorCopper   int64           `json:"cheapestFloorCopper"`
	CheapestFloorMoney    string          `json:"cheapestFloorMoney"`
	CheapestFloorQuantity int64           `json:"cheapestFloorQuantity"`
	CheapestFloorListings int             `json:"cheapestFloorListings"`
	CheapestFloorSellers  int             `json:"cheapestFloorSellers"`
	PriciestHouseID       int             `json:"priciestHouseId"`
	PriciestFaction       string          `json:"priciestFaction"`
	PriciestFloorCopper   int64           `json:"priciestFloorCopper"`
	// The per-UNIT floors the cheapest/priciest pick, spreadPct and every money
	// figure below are computed from. cheapestFloorCopper / priciestFloorCopper
	// remain LISTING prices and are not comparable across houses when the two
	// floors carry different stack sizes — which is exactly why these exist.
	CheapestFloorUnitCopper float64 `json:"cheapestFloorUnitCopper"`
	PriciestFloorUnitCopper float64 `json:"priciestFloorUnitCopper"`
	// StackedFloor is true when the cheap floor is not made up entirely of
	// single-unit listings, i.e. when sweepCostCopper and the pre-unit-basis
	// reading (cheapestFloorQuantity * cheapestFloorCopper) disagree. It marks the
	// rows whose money figures a listing-price reading would have got wrong.
	StackedFloor        bool    `json:"stackedFloor"`
	SpreadPct           float64 `json:"spreadPct"`
	SweepCostCopper     int64   `json:"sweepCostCopper"`
	SweepCostMoney      string  `json:"sweepCostMoney"`
	PotentialGainCopper int64   `json:"potentialGainCopper"`
	PotentialGainMoney  string  `json:"potentialGainMoney"`

	// NET-OF-CUT FIELDS. potentialGainCopper above is the GROSS notional: it
	// subtracts nothing for the consignment cut the auction house takes when the
	// resale actually sells, so a spread narrower than the sell house's cut is
	// reported as a profit when it is a LOSS. That is not a rounding quibble —
	// the neutral house's cut is materially larger than a faction house's, and
	// cross-house rows disproportionately have the neutral house on one leg.
	//
	// NetGainKnown is false when the resale house's rate could not be resolved
	// (no auctionhouse_dbc rows). In that case NetGainCopper falls back to the
	// GROSS figure rather than to zero: with no rate table the best available
	// estimate of net IS the gross, and defaulting it to 0 would rank every row
	// identically while looking like a computed answer. The flag, not the value,
	// is what tells a consumer the cut is unaccounted for.
	ResaleCutPercent float64 `json:"resaleCutPercent"`
	ResaleCutCopper  int64   `json:"resaleCutCopper"`
	ResaleCutMoney   string  `json:"resaleCutMoney"`
	NetGainCopper    int64   `json:"netGainCopper"`
	NetGainMoney     string  `json:"netGainMoney"`
	NetGainKnown     bool    `json:"netGainKnown"`
	// NetMarginPct is the net gain as a percentage of the capital sunk to buy the
	// cheap floor (sweepCostCopper) — the figure that ranks trades by return on
	// capital rather than by absolute size. Negative when the cut eats the spread.
	NetMarginPct float64 `json:"netMarginPct"`
	// Profitable is the headline this whole extension exists to expose: whether
	// the trade still makes money AFTER the cut. Always false when the net is
	// unknown, so a consumer that filters on it can never be misled by a row
	// whose cut was never computed.
	Profitable bool `json:"profitable"`

	// houses is the per-house fold target, keyed by AuctionHouseId; collapsed
	// into the ordered Houses slice by finalize().
	houses map[int]*ahDepthHouse
}

// house returns (creating on first sight) this item's fold target for one
// auction house, seeding the authoritative per-unit floor from the floor map.
// unitFloorScaled is in ahDepthUnitScale units; FloorUnitCopper is its copper
// rendering, the only form that leaves this package.
func (it *ahDepthItem) house(houseID int, unitFloorScaled int64, floorKnown bool) *ahDepthHouse {
	h := it.houses[houseID]
	if h == nil {
		h = &ahDepthHouse{
			HouseID:         houseID,
			Faction:         ahHouseFaction(houseID),
			FloorKnown:      floorKnown,
			floorUnitScaled: unitFloorScaled,
		}
		if floorKnown {
			h.FloorUnitCopper = float64(unitFloorScaled) / ahDepthUnitScale
		}
		it.houses[houseID] = h
	}
	return h
}

// ahDepthProceeds values `qty` units sold at a floor of `unitScaled` per unit,
// in whole copper.
//
// Split into whole-copper and fractional parts rather than computing
// qty*unitScaled/scale directly: the naive product overflows int64 for a large
// stock at a high unit price (a 2^31 copper unit price is already 2.1e13 once
// scaled), while each half here stays many orders of magnitude inside the range.
// The remainder term keeps sub-copper unit prices from vanishing, so a floor of
// 0.5 copper per unit over 100 units is 50 copper, not 0.
func ahDepthProceeds(qty, unitScaled int64) int64 {
	return qty*(unitScaled/ahDepthUnitScale) + qty*(unitScaled%ahDepthUnitScale)/ahDepthUnitScale
}

// finalize collapses the per-house fold into the ordered Houses slice, resolves
// the cheapest/priciest houses, and derives the spread + depth-weighted money
// figures. Called for EVERY folded item, not just the topN slice, so the honest
// census reflects the full fold before the minHouses/minFloorQuantity/topN cuts.
func (it *ahDepthItem) finalize(rates *ahAuctionRates) {
	// Walk the fixed house order so both the emitted slice and any cheapest /
	// priciest tie resolve deterministically, never by Go's randomized map
	// iteration order.
	it.Houses = make([]*ahDepthHouse, 0, len(it.houses))
	cheapest, priciest := -1, -1
	for _, id := range ahArbitrageHouseIDs {
		h := it.houses[id]
		if h == nil {
			continue
		}
		h.FloorSellers = len(h.floorOwners)
		it.Houses = append(it.Houses, h)
		if !h.FloorKnown {
			continue
		}
		it.HousesListed++
		// Compare UNIT floors. On listing prices a 20-unit stack looks dearer than
		// a single unit of the same goods, so the cheapest/priciest pick itself —
		// not just the money derived from it — was decided on the wrong basis.
		if cheapest == -1 || h.floorUnitScaled < it.houses[cheapest].floorUnitScaled {
			cheapest = id
		}
		if priciest == -1 || h.floorUnitScaled > it.houses[priciest].floorUnitScaled {
			priciest = id
		}
	}
	if cheapest == -1 || priciest == -1 {
		return
	}
	cheap, dear := it.houses[cheapest], it.houses[priciest]
	it.CheapestHouseID = cheap.HouseID
	it.CheapestFaction = cheap.Faction
	it.CheapestFloorCopper = cheap.FloorCopper
	it.CheapestFloorMoney = formatCopper(cheap.FloorCopper)
	it.CheapestFloorQuantity = cheap.FloorQuantity
	it.CheapestFloorListings = cheap.FloorListings
	it.CheapestFloorSellers = cheap.FloorSellers
	it.PriciestHouseID = dear.HouseID
	it.PriciestFaction = dear.Faction
	it.PriciestFloorCopper = dear.FloorCopper
	it.CheapestFloorUnitCopper = cheap.FloorUnitCopper
	it.PriciestFloorUnitCopper = dear.FloorUnitCopper
	// Spread on UNIT floors, in the scaled integer form, so the ratio is exact and
	// reuses the one div-guarded percentage helper rather than a second formula.
	it.SpreadPct = ahCategoryValuePct(dear.floorUnitScaled-cheap.floorUnitScaled, cheap.floorUnitScaled)
	// Capital to clear the cheap floor: the SUM of what those listings cost. The
	// previous form, quantity * floor price, multiplied a whole-listing price by a
	// unit count and so overstated a stacked floor by its stack size.
	it.SweepCostCopper = cheap.FloorCostCopper
	it.SweepCostMoney = formatCopper(it.SweepCostCopper)
	it.StackedFloor = cheap.FloorCostCopper != cheap.FloorQuantity*cheap.FloorCopper
	// Notional resale of that same stock at the dear house's unit floor, minus what
	// it cost to acquire. Both legs are now per-unit prices against a unit count.
	it.PotentialGainCopper = ahDepthProceeds(cheap.FloorQuantity, dear.floorUnitScaled) - it.SweepCostCopper
	it.PotentialGainMoney = formatMoney(it.PotentialGainCopper)

	// Net the consignment cut out of the resale leg. The cut is charged on the
	// SELL side only (the buy leg just pays the asking price), and it is charged
	// by the house the item is SOLD on — the priciest house here — which is
	// precisely why a per-house rate table matters rather than one global rate.
	//
	// The cut is charged on the SALE PROCEEDS, so proceeds must be the same figure
	// the gross gain was derived from — the cheap floor's units valued at the dear
	// house's UNIT floor. Deriving it from dear.FloorCopper instead would charge
	// the cut on one listing's ticket price against a whole stock's worth of
	// units, reintroducing on the net leg exactly the conflation the gross leg
	// just lost. netGain == potentialGain - resaleCut stays exact.
	proceeds := ahDepthProceeds(cheap.FloorQuantity, dear.floorUnitScaled)
	cut, known := rates.cutCopper(dear.HouseID, proceeds)
	it.NetGainKnown = known
	if !known {
		// No rate for the sell house: report the gross as the net estimate and
		// let netGainKnown:false carry the caveat. Profitable stays false — an
		// unverified profit is not a profit.
		it.NetGainCopper = it.PotentialGainCopper
		it.NetGainMoney = formatMoney(it.NetGainCopper)
		return
	}
	it.ResaleCutCopper = cut
	it.ResaleCutMoney = formatCopper(cut)
	it.ResaleCutPercent, _ = rates.cutPercentFor(dear.HouseID)
	it.NetGainCopper = it.PotentialGainCopper - cut
	// formatMoney, NOT formatCopper, and this is load-bearing: netGainCopper is
	// the one signed money figure in this tool, and formatCopper clamps anything
	// <=0 to "0c" as a defensive guard. Rendering a 5-gold LOSS as "0c" would
	// hide precisely the case this whole extension exists to surface, so the
	// signed helper (which emits a leading "-") is the only correct one here.
	// resaleCutCopper is non-negative by construction and keeps formatCopper,
	// matching every other money string in this file.
	it.NetGainMoney = formatMoney(it.NetGainCopper)
	it.NetMarginPct = ahCategoryValuePct(it.NetGainCopper, it.SweepCostCopper)
	it.Profitable = it.NetGainCopper > 0
}

// RegisterAhCrossHouseArbitrageDepthTool registers
// `ah_cross_house_arbitrage_depth` — the STOCK-DEPTH view of the cross-house
// arbitrage signal.
//
// ah_cross_house_arbitrage reports the cheapest house's floor PRICE but not how
// much QUANTITY sits on it: a floor held by a single 1-count listing is a
// fragile, one-shot trade that evaporates the moment anyone reprices, while a
// floor holding 50 units across several sellers is a repeatable opportunity.
// Neither that tool (cross-house floor price, no quantity) nor
// ah_floor_saturation (per-house floor SELLER crowding, one house per call)
// answers how much an operator can actually move at the cheap floor before the
// price changes.
func RegisterAhCrossHouseArbitrageDepthTool(reg *Registry, deps DBDeps) {
	reg.Register(Tool{
		Name: "ah_cross_house_arbitrage_depth",
		Description: "Cross-house auction-house arbitrage with STOCK DEPTH, joining acore_characters.auctionhouse " +
			"-> item_instance -> acore_world.item_template over the ops_ro pool. For each item TYPE listed on 2 or " +
			"more of the 3 AzerothCore auction houses (2=Alliance, 6=Horde, 7=Neutral) it folds, per house, the " +
			"cheapest per-UNIT buyout (floorUnitCopper) AND the depth sitting on it. FLOORS ARE PER UNIT, and that " +
			"is the basis every comparison here rests on: auctionhouse.buyoutprice is the price of a WHOLE listing " +
			"whatever its stack size (core reads it off CMSG_AUCTION_SELL_ITEM as one scalar and a buyout pays it " +
			"once, regardless of itemCount) while item_instance.count is that stack size, so MIN(buyoutprice) finds " +
			"the cheapest LISTING, not the cheapest units — on a market whose floor is a stack of 20 those differ by " +
			"20x, and comparing a stack on one house against a single unit on another is not an arbitrage signal at " +
			"all. Per house: floorUnitCopper, floorCopper (the cheapest single LISTING ticket sitting at that unit " +
			"floor — a whole-listing price, NOT comparable across houses with different stack sizes), " +
			"floorCostCopper (the exact capital to buy out that floor = the SUM of those listings' buyout prices, " +
			"never quantity*price), floorQuantity (summed item_instance.count " +
			"across listings at that house's unit floor), floorListings, floorSellers (distinct itemowner " +
			"— a floor held by ONE seller is fragile however deep it looks), totalListings and totalQuantity. Each " +
			"row then reports housesListed, the per-house houses[] array, cheapestHouseId+cheapestFaction+" +
			"cheapestFloorCopper+cheapestFloorMoney with its cheapestFloorQuantity/cheapestFloorListings/" +
			"cheapestFloorSellers, priciestHouseId+priciestFaction+priciestFloorCopper, " +
			"cheapestFloorUnitCopper+priciestFloorUnitCopper (the UNIT floors the pick and all the money below are " +
			"computed from), stackedFloor (true when the cheap floor is not all single-unit listings, i.e. the rows " +
			"a per-listing reading would have mis-priced), spreadPct " +
			"((priciestUnit-cheapestUnit)/cheapestUnit*100 — note this is a UNIT spread, so it can differ from the " +
			"per-listing figure ah_cross_house_arbitrage reports whenever stack sizes differ across houses), " +
			"sweepCostCopper (capital to buy out the whole cheap floor — the exact SUM of those listings' buyout " +
			"prices, not cheapestFloorQuantity*cheapestFloorCopper, which overstates a stacked floor by its stack " +
			"size) and potentialGainCopper (cheapestFloorQuantity valued at the priciest house's UNIT floor, minus " +
			"sweepCostCopper — the depth-weighted notional profit, GROSS; signed, since sub-copper unit prices are " +
			"held to 1/10000 copper and a zero-spread row can land a copper either side of nil), both also rendered " +
			"as WoW money. NET OF THE AUCTION-HOUSE CUT: potentialGainCopper subtracts " +
			"nothing for the consignment cut the house takes when the resale sells, so a spread narrower than that " +
			"cut is a LOSS reported as a profit — and the neutral house's cut is materially bigger than a faction " +
			"house's, exactly the leg cross-house rows tend to involve. Each row therefore also reports " +
			"resaleCutPercent/resaleCutCopper (the cut charged by the PRICIEST house, the one the resale sells on), " +
			"netGainCopper = potentialGainCopper - resaleCutCopper, netGainMoney (signed — a loss renders with a " +
			"leading minus), netMarginPct (net over sweepCostCopper, i.e. return on the capital sunk) and profitable " +
			"(netGainCopper > 0, always false when the net is unknown). Rates are read live from " +
			"acore_world.auctionhouse_dbc (ConsignmentRate=cutPercent, DepositRate=depositPercent), NOT hardcoded, " +
			"mirroring AuctionEntry::GetAuctionCut's CalculatePct(bid, cutPercent)*Rate.Auction.Cut including its two " +
			"truncations; the rates block echoes the table, the rateAuctionCut multiplier assumed and whether it " +
			"resolved. If auctionhouse_dbc is empty (client-data import not run) netGainCopper falls back to the " +
			"GROSS and netGainKnown is false on every row. The listing DEPOSIT is deliberately NOT netted: core " +
			"refunds it in full on a successful sale (profit = bid + deposit - cut) and forfeits it only when the " +
			"auction expires unsold, so depositPercent is context only. totals.matchedItemsNetNegative counts matched " +
			"rows with a positive gross but a non-positive net — the rows a gross-only reading would have sold as " +
			"opportunities. Assumes Rate.Auction.Cut is unmodified unless overridden, and that " +
			"AllowTwoSideInteraction.Auction is OFF (with it ON the worldserver charges every house the neutral " +
			"rate). This is the quantity-risk axis ah_cross_house_arbitrage (floor price " +
			"only, no stock) and ah_floor_saturation (per-house crowding, one house per call, no cross-house " +
			"comparison) cannot show: a 900% spread on a single 1-unit listing outranks nothing. Also returns " +
			"itemEntry, itemName, quality + qualityName (0 Poor .. 7 Heirloom, unknown as quality-<q>) and class + " +
			"className (unknown as class-<c>). Floors come from an unLIMITed GROUP BY so they are never truncated; " +
			"a house whose floor row is missing (a listing inserted between the two queries) is folded into the " +
			"totals with floorKnown:false and excluded from housesListed and the cheapest/priciest pick. Honest " +
			"realm-wide totals (distinctItemsListed, distinctItemsOnMultipleHouses, distinctItemsOnAllHouses, " +
			"totalListingsScanned, totalQuantityScanned, housesMissingFloor, matchedItemsStackedFloor — how many " +
			"matched rows carry a stacked cheap floor, i.e. how much of this market the unit basis actually moves; " +
			"0 means a per-listing reading would have agreed everywhere here — and " +
			"listingsSkippedNonPositiveCount, listings with a non-positive stack size that have no unit price and " +
			"are excluded from the floors rather than dropped in silence) are folded across every scanned item " +
			"independent of topN. A basis block states the floor grain and unit scale. " +
			"Args: topN (default 15, max 100), rateAuctionCut (assumed Rate.Auction.Cut " +
			"multiplier, default 1, rejected outside 0-100), sortBy (netGain [default, netGainCopper desc] | gain " +
			"[potentialGainCopper " +
			"desc] | spread [spreadPct desc] | depth [cheapestFloorQuantity desc] | cheapestFloor " +
			"[cheapestFloorUnitCopper asc, lowest per-unit buy-in first]; an unknown key is rejected), minHouses (default 2, " +
			"clamped to the 2-3 range), minFloorQuantity (default 1, clamped to >=1 — raise it to drop fragile " +
			"one-unit floors), minQuality (optional 0-7 — only items at or above this rarity; out-of-range is " +
			"rejected, not clamped). Scan capped at 200000 listing rows; truncated:true flags an incomplete fold. " +
			"itemEntry-asc tiebreak within a sort key. Read-only, 10 s timeout.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{
			"topN":{"type":"integer","description":"Max items returned (default 15, max 100)"},
			"sortBy":{"type":"string","enum":["netGain","gain","spread","depth","cheapestFloor"],"description":"netGain=netGainCopper desc (default, net of the resale house's cut), gain=potentialGainCopper desc (GROSS), spread=spreadPct desc (UNIT-price spread), depth=cheapestFloorQuantity desc, cheapestFloor=cheapestFloorUnitCopper asc (lowest per-unit buy-in)"},
			"rateAuctionCut":{"type":"number","description":"Assumed worldserver Rate.Auction.Cut multiplier applied to the DBC cut percentage (default 1; rejected outside 0-100). ops-api cannot read the live worldserver config, so this is echoed back in the rates block"},
			"minHouses":{"type":"integer","description":"Only items listed on at least this many of the 3 houses (default 2, clamped to 2-3)"},
			"minFloorQuantity":{"type":"integer","description":"Only items with at least this many units at the cheapest house's floor (default 1, clamped to >=1) — raise it to drop fragile one-unit floors"},
			"minQuality":{"type":"integer","description":"Only items at or above this rarity (0=Poor .. 7=Heirloom); omit for all qualities"}
		}}`),
		Annotations: AnnRead(),
		Handler: func(ctx context.Context, raw json.RawMessage, _ string) any {
			var a struct {
				TopN             int      `json:"topN"`
				SortBy           string   `json:"sortBy"`
				MinHouses        *int     `json:"minHouses"`
				MinFloorQuantity *int64   `json:"minFloorQuantity"`
				MinQuality       *int     `json:"minQuality"`
				RateAuctionCut   *float64 `json:"rateAuctionCut"`
			}
			if len(raw) > 0 && string(raw) != "null" {
				if err := json.Unmarshal(raw, &a); err != nil {
					return map[string]any{"error": "decode args: " + err.Error()}
				}
			}
			if deps.QueryDB == nil {
				return map[string]any{"error": "ops_ro pool not configured (DB_DSN empty?)"}
			}
			topN := a.TopN
			if topN <= 0 {
				topN = ahDepthDefaultTop
			}
			if topN > ahDepthMaxTop {
				topN = ahDepthMaxTop
			}
			// sortBy resolves to an allowlist key; empty -> the "gain" default. An
			// unknown key is rejected (no query) so a typo can't silently reorder
			// the leaderboard rather than erroring.
			sortKey := a.SortBy
			if sortKey == "" {
				sortKey = "netGain"
			}
			if !ahDepthSortKeys[sortKey] {
				return map[string]any{"error": fmt.Sprintf("invalid sortBy: %q (valid: netGain, gain, spread, depth, cheapestFloor)", a.SortBy)}
			}
			minHouses := ahDepthDefaultMinHouses
			if a.MinHouses != nil {
				minHouses = *a.MinHouses
				if minHouses < ahDepthDefaultMinHouses {
					minHouses = ahDepthDefaultMinHouses
				}
				if minHouses > ahDepthMaxHouses {
					minHouses = ahDepthMaxHouses
				}
			}
			// minFloorQuantity is a soft floor, not an enum: default 1, clamp <1 up
			// to 1 (a matched floor always holds at least one unit).
			minFloorQty := int64(ahDepthDefaultMinFloorQty)
			if a.MinFloorQuantity != nil {
				minFloorQty = *a.MinFloorQuantity
				if minFloorQty < ahDepthDefaultMinFloorQty {
					minFloorQty = ahDepthDefaultMinFloorQty
				}
			}
			// minQuality is rejected (not clamped) when outside 0-7 so a typo'd 99
			// reads as "bad filter", not an empty market.
			if a.MinQuality != nil && (*a.MinQuality < 0 || *a.MinQuality > ahDepthMaxQuality) {
				return map[string]any{"error": fmt.Sprintf("minQuality out of range: %d (valid 0-%d)", *a.MinQuality, ahDepthMaxQuality)}
			}
			// Rate.Auction.Cut is a worldserver config ops-api cannot read, so it
			// is an explicit assumption: default 1 (worldserver.conf.dist),
			// overridable, clamped to a sane band, and always echoed back in the
			// response so a net figure is never reported without the multiplier
			// that produced it. Negative is rejected rather than clamped — it
			// would invert the arithmetic, which is a bad filter, not a taste.
			rateCut := ahRateAuctionCutDefault
			if a.RateAuctionCut != nil {
				rateCut = *a.RateAuctionCut
				if rateCut < 0 || rateCut > ahRateAuctionCutMax {
					return map[string]any{"error": fmt.Sprintf("rateAuctionCut out of range: %v (valid 0-%v)", *a.RateAuctionCut, ahRateAuctionCutMax)}
				}
			}
			c, cancel := context.WithTimeout(ctx, ahDepthTimeout)
			defer cancel()
			// A rate lookup failure is NON-FATAL: the arbitrage fold is still
			// worth returning, so we degrade to gross-only with rates.resolved
			// false rather than failing a working tool over an optional enricher.
			rates, ratesErr := resolveAhAuctionRates(c, deps.QueryDB, rateCut)
			out, err := collectAhCrossHouseArbitrageDepth(c, deps.QueryDB, time.Now(), topN, minHouses, sortKey, minFloorQty, a.MinQuality, rates)
			if err != nil {
				return map[string]any{"error": err.Error()}
			}
			if ratesErr != nil {
				// Surface the reason instead of letting an access-denied on
				// acore_world masquerade as an unpopulated DBC table.
				if rb, ok := out["rates"].(map[string]any); ok {
					rb["error"] = ratesErr.Error()
					rb["detail"] = "rate lookup failed — netGainCopper falls back to the GROSS potentialGainCopper and every netGainKnown is false"
				}
			}
			return out
		},
	})
}

// collectAhCrossHouseArbitrageDepth runs the per-(house, item) floor map, then
// the per-listing scan + fold, on one shared connection. Split out so tests can
// drive it with sqlmock and a fixed `now`.
// rates may be nil, which forces every net figure to the unknown path and the
// pre-existing gross-only behaviour — the degradation existing goldens exercise
// when they pass nil, and the same nil-means-no-history shape db_global_status
// uses for its snapshot ring.
func collectAhCrossHouseArbitrageDepth(ctx context.Context, db *sql.DB, now time.Time, topN, minHouses int, sortKey string, minFloorQty int64, minQuality *int, rates *ahAuctionRates) (map[string]any, error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire conn: %w", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "USE `acore_characters`"); err != nil {
		return nil, fmt.Errorf("USE acore_characters: %w", err)
	}

	// Stage 1: authoritative per-(house, item) floor map (see query doc).
	floorRows, err := conn.QueryContext(ctx, ahDepthFloorMapQuery)
	if err != nil {
		return nil, fmt.Errorf("depth floor-map query: %w", err)
	}
	floors := map[int64]map[int]int64{}
	for floorRows.Next() {
		var (
			houseID      int
			entry, floor int64
		)
		if err := floorRows.Scan(&houseID, &entry, &floor); err != nil {
			floorRows.Close()
			return nil, fmt.Errorf("depth floor-map scan: %w", err)
		}
		byHouse := floors[entry]
		if byHouse == nil {
			byHouse = map[int]int64{}
			floors[entry] = byHouse
		}
		byHouse[houseID] = floor
	}
	if err := floorRows.Err(); err != nil {
		floorRows.Close()
		return nil, fmt.Errorf("depth floor-map iter: %w", err)
	}
	// Explicit Close (not just defer) so the shared conn is free before Stage 2.
	floorRows.Close()

	// Stage 2: per-listing scan + fold. minQuality only applies here since it
	// cannot affect the floor computation.
	scanQuery := ahDepthScanQuery
	var scanArgs []any
	if minQuality != nil {
		scanQuery += " AND it.Quality >= ?"
		scanArgs = append(scanArgs, *minQuality)
	}
	scanQuery += " LIMIT ?"
	scanArgs = append(scanArgs, ahDepthScanCap+1)

	rows, err := conn.QueryContext(ctx, scanQuery, scanArgs...)
	if err != nil {
		return nil, fmt.Errorf("depth listing-scan query: %w", err)
	}
	items := map[int64]*ahDepthItem{}
	scanned := 0
	listingsSkippedNonPositiveCount := 0
	truncated := false
	for rows.Next() {
		if scanned >= ahDepthScanCap {
			truncated = true
			break
		}
		var (
			houseID                       int
			entry, itemOwner, buyout, qty int64
			name                          string
			quality, class                int
		)
		if err := rows.Scan(&houseID, &entry, &itemOwner, &buyout, &qty, &name, &quality, &class); err != nil {
			rows.Close()
			return nil, fmt.Errorf("depth listing-scan scan: %w", err)
		}
		scanned++
		// A non-positive stack size has no unit price, so it cannot be placed
		// against a unit floor and the floor map excluded it too. Counted rather
		// than dropped in silence: a row that vanished and a row that was measured
		// and found ordinary must not look the same.
		if qty <= 0 {
			listingsSkippedNonPositiveCount++
			continue
		}
		it := items[entry]
		if it == nil {
			it = &ahDepthItem{
				ItemEntry:   entry,
				ItemName:    name,
				Quality:     quality,
				QualityName: itemQualityName(quality),
				Class:       class,
				ClassName:   itemClassName(class),
				houses:      map[int]*ahDepthHouse{},
			}
			items[entry] = it
		}
		unitFloorScaled, floorKnown := floors[entry][houseID]
		it.house(houseID, unitFloorScaled, floorKnown).fold(itemOwner, buyout, qty)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("depth listing-scan iter: %w", err)
	}
	rows.Close()

	// Honest census: finalize + fold EVERY scanned item, independent of the
	// minHouses/minFloorQuantity/topN cuts applied next.
	var (
		distinctItemsOnMultipleHouses int
		distinctItemsOnAllHouses      int
		totalListingsScanned          int
		housesMissingFloor            int
		totalQuantityScanned          int64
		// matchedNetNegative counts rows that clear every filter and show a
		// POSITIVE gross gain yet a non-positive net once the resale house's cut
		// is taken — i.e. exactly the rows a gross-only reading would have sold
		// as opportunities. Folded over the matched set (not the whole scan) so
		// it is directly comparable to matchedItems, and reported only when the
		// rates actually resolved.
		matchedNetNegative int
		// matchedStackedFloor counts matched rows whose cheap floor is not made up
		// entirely of single-unit listings — precisely the rows where the exact
		// sweep capital and the pre-unit-basis quantity*listing-price reading
		// disagree. It is how an operator sizes the correction on THIS market
		// rather than taking the change on faith: 0 means the two readings would
		// have agreed everywhere here.
		matchedStackedFloor int
	)
	matched := make([]*ahDepthItem, 0, len(items))
	for _, it := range items {
		it.finalize(rates)
		for _, h := range it.Houses {
			totalListingsScanned += h.TotalListings
			totalQuantityScanned += h.TotalQuantity
			if !h.FloorKnown {
				housesMissingFloor++
			}
		}
		if it.HousesListed >= ahDepthDefaultMinHouses {
			distinctItemsOnMultipleHouses++
		}
		if it.HousesListed >= ahDepthMaxHouses {
			distinctItemsOnAllHouses++
		}
		if it.HousesListed >= minHouses && it.CheapestFloorQuantity >= minFloorQty {
			matched = append(matched, it)
			if it.NetGainKnown && it.PotentialGainCopper > 0 && it.NetGainCopper <= 0 {
				matchedNetNegative++
			}
			if it.StackedFloor {
				matchedStackedFloor++
			}
		}
	}

	sort.Slice(matched, func(i, j int) bool {
		switch sortKey {
		case "spread":
			if matched[i].SpreadPct != matched[j].SpreadPct {
				return matched[i].SpreadPct > matched[j].SpreadPct
			}
		case "depth":
			if matched[i].CheapestFloorQuantity != matched[j].CheapestFloorQuantity {
				return matched[i].CheapestFloorQuantity > matched[j].CheapestFloorQuantity
			}
		case "cheapestFloor":
			// Ranks by the cheapest UNIT price, not the cheapest listing ticket: a
			// bulk stack is the lower buy-in per unit even when its listing price is
			// the larger number.
			if matched[i].CheapestFloorUnitCopper != matched[j].CheapestFloorUnitCopper {
				return matched[i].CheapestFloorUnitCopper < matched[j].CheapestFloorUnitCopper
			}
		case "gain":
			if matched[i].PotentialGainCopper != matched[j].PotentialGainCopper {
				return matched[i].PotentialGainCopper > matched[j].PotentialGainCopper
			}
		default: // "netGain" — the default: rank by what the trade actually keeps
			if matched[i].NetGainCopper != matched[j].NetGainCopper {
				return matched[i].NetGainCopper > matched[j].NetGainCopper
			}
			if matched[i].PotentialGainCopper != matched[j].PotentialGainCopper {
				return matched[i].PotentialGainCopper > matched[j].PotentialGainCopper
			}
		}
		return matched[i].ItemEntry < matched[j].ItemEntry
	})

	matchedItems := len(matched)
	if len(matched) > topN {
		matched = matched[:topN]
	}

	out := map[string]any{
		"asOf":             now.UTC().Format(time.RFC3339),
		"topN":             topN,
		"sortBy":           sortKey,
		"minHouses":        minHouses,
		"minFloorQuantity": minFloorQty,
		"scanned":          scanned,
		"scanCap":          ahDepthScanCap,
		"truncated":        truncated,
		"matchedItems":     matchedItems,
		"totals": map[string]any{
			"distinctItemsListed":             len(items),
			"distinctItemsOnMultipleHouses":   distinctItemsOnMultipleHouses,
			"distinctItemsOnAllHouses":        distinctItemsOnAllHouses,
			"totalListingsScanned":            totalListingsScanned,
			"totalQuantityScanned":            totalQuantityScanned,
			"housesMissingFloor":              housesMissingFloor,
			"matchedItemsStackedFloor":        matchedStackedFloor,
			"listingsSkippedNonPositiveCount": listingsSkippedNonPositiveCount,
		},
		// The basis block states what a "floor" means here, so a net figure is never
		// read against the wrong grain — the same shape db_global_status and
		// db_table_bloat use to publish the basis of a number rather than leaving a
		// consumer to infer it.
		"basis": map[string]any{
			"floorGrain": "unitPrice",
			"unitScale":  ahDepthUnitScale,
			"detail": "auctionhouse.buyoutprice is the price of a WHOLE listing and item_instance.count is that " +
				"listing's stack size, so floors are compared per UNIT (buyoutprice/count) and not per listing. " +
				"floorCopper/cheapestFloorCopper/priciestFloorCopper remain LISTING prices and are not comparable " +
				"across houses whose floors carry different stack sizes; floorUnitCopper, spreadPct, sweepCostCopper " +
				"and every gain figure use the unit basis. sweepCostCopper is the exact SUM of the floor listings' " +
				"buyout prices. Unit prices are held to 1/" + fmt.Sprint(ahDepthUnitScale) + " copper, so a resale " +
				"valuation can differ from an exact rational by well under a copper.",
		},
		"items": matched,
	}
	if minQuality != nil {
		out["minQuality"] = *minQuality
	}
	// The rates block states the economics actually applied, so a reader never
	// has to guess whether a net figure is real. When rates are unresolved the
	// block says so explicitly instead of being omitted — a missing key reads as
	// "the tool forgot", a false flag reads as "the tool checked and could not".
	if rates == nil {
		out["rates"] = map[string]any{
			"resolved": false,
			"source":   ahAuctionRatesSource,
			"detail": "no rows in acore_world.auctionhouse_dbc (client-data import not run, or the table is empty) — " +
				"netGainCopper falls back to the GROSS potentialGainCopper and every netGainKnown is false",
		}
	} else {
		out["rates"] = map[string]any{
			"resolved":       true,
			"source":         rates.Source,
			"rateAuctionCut": rates.RateAuctionCut,
			"houses":         rates.HouseList,
			"detail": "cutPercent is the consignment cut charged on a SALE (AuctionEntry::GetAuctionCut); " +
				"depositPercent is reported for context only and is NOT netted — the listing deposit is refunded " +
				"in full when an auction sells and is forfeited only when it expires unsold",
		}
		out["totals"].(map[string]any)["matchedItemsNetNegative"] = matchedNetNegative
	}
	return out, nil
}
