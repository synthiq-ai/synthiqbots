package mcpserver

import (
	"context"
	"database/sql"
	"fmt"
	"math"
)

// AUCTION-HOUSE ECONOMICS, SOURCE-ANCHORED
//
// Every constant and formula below is read out of AzerothCore core source, not
// from wiki memory or a WotLK-era blog — the backlog item that motivated this
// file said explicitly DO NOT GUESS THE RATES, and the numbers a player "knows"
// (5% / 15%) are client-era folklore that a realm's DBC can and does override.
//
//	AuctionEntry::GetAuctionCut()               src/server/game/AuctionHouse/AuctionHouseMgr.cpp:568
//	    int32 cut = int32(CalculatePct(bid, auctionHouseEntry->cutPercent)
//	                      * sWorld->getRate(RATE_AUCTION_CUT));
//	    return std::max(cut, 0);
//	CalculatePct(base, pct)                     src/common/Utilities/Util.h:52
//	    return T(base * static_cast<float>(pct) / 100.0f);
//	struct AuctionHouseEntry                    src/server/shared/DataStores/DBCStructure.h:573
//	    uint32 houseId;        // 0
//	    uint32 faction;        // 1
//	    uint32 depositPercent; // 2   "1/3 from real"
//	    uint32 cutPercent;     // 3
//
// The DBC is surfaced in the world DB as `acore_world.auctionhouse_dbc`, whose
// column order matches the struct field order: ID, FactionID, DepositRate
// (= depositPercent), ConsignmentRate (= cutPercent). Reading the rates from
// that table rather than baking in constants is the whole point: it is the same
// data the worldserver itself loads, so a realm that customised its DBC gets
// arithmetic that matches what its players are actually charged.
//
// THE CUT IS THE ONLY COST OF A COMPLETED TRADE. The listing deposit is NOT —
// it is refunded in full when the auction sells:
//
//	AuctionHouseMgr::SendAuctionSuccessfulMail   AuctionHouseMgr.cpp:194
//	    uint32 profit = auction->bid + auction->deposit - auction->GetAuctionCut();
//	    ... MailDraft(...).AddMoney(profit)
//
// and forfeited only when the auction EXPIRES unsold, where the expiry mail
// carries `.AddItem(pItem)` and no `.AddMoney(...)` at all (AuctionHouseMgr.cpp:264).
// So the deposit is working capital at risk on an unsold relist, not a cost of a
// sale, and netting it out of a completed trade's gain would understate that gain.
// depositPercent is therefore resolved and reported here as informational
// context, deliberately NOT subtracted from any net figure.

const (
	// ahRateAuctionCutDefault mirrors worldserver.conf.dist's `Rate.Auction.Cut = 1`
	// (src/server/apps/worldserver/worldserver.conf.dist:4702). ops-api has no
	// route to a running worldserver's live config, so the multiplier is an
	// explicit, overridable ASSUMPTION echoed back in the response rather than a
	// silently baked-in 1.0.
	ahRateAuctionCutDefault = 1.0

	// ahRateAuctionCutMax bounds the override. The clamp exists to keep a typo'd
	// 1000 from reporting every trade as a catastrophic loss; it is a sanity rail,
	// not a game rule.
	ahRateAuctionCutMax = 100.0

	// ahAuctionRatesSource names the rate table in both the resolved and the
	// unresolved response block, so a consumer reading "resolved:false" still
	// learns WHERE the rates were looked for.
	ahAuctionRatesSource = "acore_world.auctionhouse_dbc"
)

// ahAuctionRatesQuery reads the per-house economics the worldserver loads from
// AuctionHouse.dbc. Fully schema-qualified so it runs on the shared connection
// the caller has already pointed at acore_characters, and ordered for a
// deterministic response block.
//
// The table ships EMPTY in AzerothCore's base SQL
// (data/sql/base/db_world/auctionhouse_dbc.sql has a LOCK/UNLOCK pair and no
// rows) — it is populated by the client-data import step. A realm that never ran
// that step has no rates, which is why an empty result is a first-class
// "unresolved" outcome here rather than an error.
const ahAuctionRatesQuery = "SELECT ID, ConsignmentRate, DepositRate " +
	"FROM `acore_world`.`auctionhouse_dbc` ORDER BY ID"

// ahHouseRate is one auction house's economics as the worldserver sees them.
// CutPercent is the consignment cut charged on a SALE; DepositPercent is
// reported for context only (see the deposit note above) and is the raw DBC
// value, which core scales by 1/3 when it computes an actual deposit
// (AuctionHouseMgr.cpp:99).
type ahHouseRate struct {
	HouseID        int     `json:"houseId"`
	Faction        string  `json:"faction"`
	CutPercent     float64 `json:"cutPercent"`
	DepositPercent float64 `json:"depositPercent"`
}

// ahAuctionRates is the resolved rate table plus the assumptions applied to it.
// A nil *ahAuctionRates means "rates could not be resolved" and every net figure
// derived from it must be reported as unknown rather than defaulted — the same
// nil-is-not-zero discipline db_global_status_growth uses for an absent counter
// reading, and for the same reason: a fabricated 0% cut would make every gross
// figure look correct and silently defeat the point of the tool.
type ahAuctionRates struct {
	Source         string               `json:"source"`
	RateAuctionCut float64              `json:"rateAuctionCut"`
	Houses         map[int]*ahHouseRate `json:"-"`
	HouseList      []*ahHouseRate       `json:"houses"`
}

// cutCopper returns the consignment cut charged on a sale of `sale` copper at
// house `houseID`, and whether that house's rate is known.
//
// The arithmetic mirrors GetAuctionCut()'s TWO truncations rather than doing the
// multiply in one step: CalculatePct instantiates T as the uint32 `bid`, so the
// percentage is truncated to a whole copper BEFORE the RATE_AUCTION_CUT
// multiplier is applied, and the product is then truncated again by the int32
// cast. Collapsing them into a single float multiply is off by up to a copper
// against what the server actually charges, which is exactly the kind of quiet
// divergence this file exists to avoid.
func (r *ahAuctionRates) cutCopper(houseID int, sale int64) (int64, bool) {
	if r == nil || sale <= 0 {
		return 0, r != nil
	}
	h := r.Houses[houseID]
	if h == nil {
		return 0, false
	}
	inner := int64(float64(sale) * h.CutPercent / 100.0)
	cut := int64(float64(inner) * r.RateAuctionCut)
	if cut < 0 {
		cut = 0
	}
	if cut > sale {
		// Defensive: a customised DBC + a large Rate.Auction.Cut override can
		// exceed the sale price. Core clamps only at zero, but a cut larger than
		// the proceeds would make netGain diverge from "you keep sale-cut", so
		// cap it at the sale and let the net land at exactly -buyCost.
		cut = sale
	}
	return cut, true
}

// cutPercentFor returns the EFFECTIVE cut percentage for a house — the DBC
// percentage after the Rate.Auction.Cut multiplier — so a response can state the
// rate it actually applied rather than making the reader recompute it.
func (r *ahAuctionRates) cutPercentFor(houseID int) (float64, bool) {
	if r == nil {
		return 0, false
	}
	h := r.Houses[houseID]
	if h == nil {
		return 0, false
	}
	return math.Round(h.CutPercent*r.RateAuctionCut*100) / 100, true
}

// resolveAhAuctionRates loads the per-house rate table. It is deliberately
// separate from the collectors that consume it: a collector takes an already
// resolved *ahAuctionRates (or nil), which keeps the rate lookup out of every
// caller's sqlmock expectation sequence and lets tests drive the net arithmetic
// from a literal rate table instead of a fixture.
//
// A missing or empty auctionhouse_dbc is NOT an error — it returns (nil, nil),
// and the caller reports net figures as unknown.
func resolveAhAuctionRates(ctx context.Context, db *sql.DB, rateAuctionCut float64) (*ahAuctionRates, error) {
	if db == nil {
		return nil, nil
	}
	rows, err := db.QueryContext(ctx, ahAuctionRatesQuery)
	if err != nil {
		return nil, fmt.Errorf("auction rates query: %w", err)
	}
	defer rows.Close()

	rates := &ahAuctionRates{
		Source:         ahAuctionRatesSource,
		RateAuctionCut: rateAuctionCut,
		Houses:         map[int]*ahHouseRate{},
	}
	for rows.Next() {
		var (
			id           int
			cut, deposit float64
		)
		if err := rows.Scan(&id, &cut, &deposit); err != nil {
			return nil, fmt.Errorf("auction rates scan: %w", err)
		}
		h := &ahHouseRate{
			HouseID:        id,
			Faction:        ahHouseFaction(id),
			CutPercent:     cut,
			DepositPercent: deposit,
		}
		rates.Houses[id] = h
		rates.HouseList = append(rates.HouseList, h)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("auction rates iter: %w", err)
	}
	if len(rates.Houses) == 0 {
		// Table present but unpopulated (no client-data import) — indistinguishable
		// to a consumer from "no rates", so say so with nil rather than shipping an
		// empty table that would resolve every cut to unknown one house at a time.
		return nil, nil
	}
	return rates, nil
}
