package mcpserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"
)

const (
	ahBidActivityTimeout    = 10 * time.Second
	ahBidActivityDefaultTop = 15
	ahBidActivityMaxTop     = 100
)

// ahBidAggQuery is the honest census of the live-bid subset of the auction house.
// It reads acore_characters.auctionhouse ALONE (no join) so COUNT/SUM are always
// complete regardless of how many listings carry bids — the headline numbers can
// never be truncated by a LIMIT the way the top-N lists below are. Columns
// anchored on the canonical AzerothCore schema
// (data/sql/base/db_characters/auctionhouse.sql): buyguid (current high bidder,
// 0 = no live bid), lastbid (current high bid copper), startbid (opening bid
// copper), buyoutprice (0 = auction-only, no buyout to reach). The COALESCEs turn
// SUM-over-empty (NULL) into 0 so an AH with zero live bids folds cleanly. The
// WHERE (buyguid <> 0 plus any filters) is appended by the collector so it stays
// identical to the two list queries.
const ahBidAggQuery = "SELECT COUNT(*), " +
	"COALESCE(SUM(ah.lastbid), 0), " +
	"COALESCE(SUM(ah.startbid), 0), " +
	"COALESCE(SUM(CASE WHEN ah.buyoutprice > 0 THEN 1 ELSE 0 END), 0) " +
	"FROM `auctionhouse` ah"

// ahBidListBaseQuery is the per-auction cross-DB join that resolves item names
// for the two top-N lists. It reuses the ah_market_top_items join shape:
// acore_characters.auctionhouse.itemguid -> item_instance.guid;
// item_instance.itemEntry -> acore_world.item_template.entry (ops_ro holds SELECT
// on both characters and world). The INNER JOINs drop a listing whose item
// instance no longer exists (a dangling itemguid) — a live bid on a vanished item
// is not something an operator can act on. ah.id is the auctionhouse PK, used both
// as the stable tiebreaker and as the returned auctionId. The ORDER BY + LIMIT are
// appended by the collector (the two lists sort differently).
const ahBidListBaseQuery = "SELECT ah.id, ah.houseid, it.entry, it.name, it.Quality, " +
	"ah.startbid, ah.lastbid, ah.buyoutprice " +
	"FROM `auctionhouse` ah " +
	"JOIN `item_instance` ii ON ah.itemguid = ii.guid " +
	"JOIN `acore_world`.`item_template` it ON ii.itemEntry = it.entry"

// ahBidPct returns n as a percentage of total, rounded to one decimal, guarding
// total<=0 -> 0 so an empty live-bid set (or a malformed zero startbid/buyout row)
// never divides by zero. Kept local (int64) rather than reusing ah_market_summary's
// int ahPct so the two bid tools don't couple through a shared helper — the copper
// sums are int64 and an int cast would overflow a large market total.
func ahBidPct(n, total int64) float64 {
	if total <= 0 {
		return 0
	}
	return math.Round(float64(n)/float64(total)*1000) / 10
}

// ahBidAuction is one live-bid listing surfaced in either top-N list. Copper
// amounts are the machine-readable int64 facet; the *Gold strings render the same
// values as WoW money (formatCopper, shared with ah_market_summary). BidOverStartPct
// is how far the current bid has climbed above the opening bid; BidToBuyoutPct is
// how close the current bid is to the buyout (omitted for auction-only listings
// that have no buyout to approach — HasBuyout disambiguates a genuine 0).
type ahBidAuction struct {
	AuctionID        int64   `json:"auctionId"`
	HouseID          int     `json:"houseId"`
	Faction          string  `json:"faction"`
	ItemEntry        int64   `json:"itemEntry"`
	ItemName         string  `json:"itemName"`
	Quality          int     `json:"quality"`
	QualityName      string  `json:"qualityName"`
	StartBidCopper   int64   `json:"startBidCopper"`
	StartBidGold     string  `json:"startBidGold"`
	CurrentBidCopper int64   `json:"currentBidCopper"`
	CurrentBidGold   string  `json:"currentBidGold"`
	BuyoutCopper     int64   `json:"buyoutCopper"`
	BuyoutGold       string  `json:"buyoutGold"`
	HasBuyout        bool    `json:"hasBuyout"`
	BidOverStartPct  float64 `json:"bidOverStartPct"`
	BidToBuyoutPct   float64 `json:"bidToBuyoutPct,omitempty"`
}

// RegisterAhMarketBidActivityTool registers `ah_market_bid_activity` — a read-only
// census of the BID side of the auction house.
//
// Every other ah_* tool keys off buyoutprice (summary/top_items/top_sellers/
// category/percentiles/outliers x2) or expire time (expiry_timeline); none reports
// where the bidding action is. ah_market_summary gives only a withActiveBid COUNT —
// no value, no progression, no top-N. This tool answers "which listings are
// contested / about to sell via bid": the count + summed current/opening bid of
// listings carrying a live bid (buyguid != 0), how far bids have climbed above
// their opening (bidOverStartPct), and two top-N lists — most-contested (highest
// current bid) and closest-to-buyout (current bid nearest the buyout price, the
// listings about to sell via bid rather than expire).
func RegisterAhMarketBidActivityTool(reg *Registry, deps DBDeps) {
	reg.Register(Tool{
		Name: "ah_market_bid_activity",
		Description: "Bid-competition census of the auction house over the ops_ro pool — the BID side that " +
			"ah_market_summary (buyout/count totals) and the price/expiry lenses leave out. A live-bid listing is " +
			"one with buyguid != 0 (a current high bidder). `totals`: auctionsWithBid, withBuyout (of those, how " +
			"many also carry a buyout), totalCurrentBid / totalStartBid / avgCurrentBid in copper + human gold, and " +
			"bidOverStartPct (how far the summed current bids sit above their opening bids). Two top-N lists join " +
			"acore_characters.auctionhouse -> item_instance -> acore_world.item_template for item names: " +
			"`mostContested` (highest currentBid first — the hottest items) and `closestToBuyout` (currentBid " +
			"nearest the buyoutprice, i.e. about to sell via bid rather than expire; auction-only listings with no " +
			"buyout are excluded). Each entry: auctionId, itemEntry, itemName, quality + qualityName (0 Poor .. 7 " +
			"Heirloom), houseId + faction (2=Alliance, 6=Horde, 7=Neutral), start/current/buyout copper + gold, " +
			"hasBuyout, bidOverStartPct, and bidToBuyoutPct (current bid as a pct of buyout; omitted when no " +
			"buyout). Args: topN (default 15, max 100 — size of EACH list), houseId (optional — narrow to one " +
			"auction house 2/6/7; any other value is rejected, not silently emptied), minBid (optional copper floor " +
			"on lastbid — only listings whose current bid is at or above it; negative is rejected). The bid-demand " +
			"companion to ah_market_summary. Read-only, 10 s timeout.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{
			"topN":{"type":"integer","description":"Size of EACH top-N list (default 15, max 100)"},
			"houseId":{"type":"integer","description":"Narrow to one auction house (2=Alliance, 6=Horde, 7=Neutral); any other value is rejected"},
			"minBid":{"type":"integer","description":"Copper floor on the current bid (lastbid) — only listings at or above this value; negative rejected"}
		}}`),
		Annotations: AnnRead(),
		Handler: func(ctx context.Context, raw json.RawMessage, _ string) any {
			var a struct {
				TopN    int    `json:"topN"`
				HouseID *int   `json:"houseId"`
				MinBid  *int64 `json:"minBid"`
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
				topN = ahBidActivityDefaultTop
			}
			if topN > ahBidActivityMaxTop {
				topN = ahBidActivityMaxTop
			}
			// houseId is rejected (not clamped/passed-through) when outside the
			// AuctionHouseId enum — an unknown house would return empty lists that
			// read as "no bids" rather than "bad filter". Matches ah_market_expiry_timeline.
			if a.HouseID != nil && *a.HouseID != 2 && *a.HouseID != 6 && *a.HouseID != 7 {
				return map[string]any{"error": fmt.Sprintf("houseId invalid: %d (valid 2=Alliance, 6=Horde, 7=Neutral)", *a.HouseID)}
			}
			// A negative floor is a typo, not a filter — reject rather than let it
			// silently pass every row (lastbid unsigned, so >= negative is always true).
			if a.MinBid != nil && *a.MinBid < 0 {
				return map[string]any{"error": fmt.Sprintf("minBid negative: %d (copper floor must be >= 0)", *a.MinBid)}
			}
			c, cancel := context.WithTimeout(ctx, ahBidActivityTimeout)
			defer cancel()
			out, err := collectAhBidActivity(c, deps.QueryDB, time.Now(), topN, a.HouseID, a.MinBid)
			if err != nil {
				return map[string]any{"error": err.Error()}
			}
			return out
		},
	})
}

// ahBidFilters builds the WHERE conditions shared by all three queries. The
// live-bid gate (buyguid <> 0) is always present; houseId/minBid are appended when
// set. Returned as (conds, args) so the collector can splice the extra
// closest-to-buyout guard in without re-deriving the bind order.
func ahBidFilters(houseID *int, minBid *int64) ([]string, []any) {
	conds := []string{"ah.buyguid <> 0"}
	var args []any
	if houseID != nil {
		conds = append(conds, "ah.houseid = ?")
		args = append(args, *houseID)
	}
	if minBid != nil {
		conds = append(conds, "ah.lastbid >= ?")
		args = append(args, *minBid)
	}
	return conds, args
}

// collectAhBidActivity runs the aggregate census plus the two top-N list queries
// and assembles the snapshot. Split out so tests can drive it with sqlmock and a
// fixed `now`. Three round trips in a fixed order: aggregate (honest, unbounded
// COUNT/SUM), mostContested (ORDER BY lastbid DESC), closestToBuyout (ORDER BY the
// bid/buyout ratio DESC over buyout-bearing rows). The SQL ORDER BY is the source
// of truth for each list's order — the collector does not re-sort.
func collectAhBidActivity(ctx context.Context, db *sql.DB, now time.Time, topN int, houseID *int, minBid *int64) (map[string]any, error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire conn: %w", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "USE `acore_characters`"); err != nil {
		return nil, fmt.Errorf("USE acore_characters: %w", err)
	}

	conds, baseArgs := ahBidFilters(houseID, minBid)
	where := " WHERE " + strings.Join(conds, " AND ")

	// 1. Honest realm-wide (or filtered) census of the live-bid subset.
	var auctionsWithBid, withBuyout int
	var totalCurrentBid, totalStartBid int64
	aggRow := conn.QueryRowContext(ctx, ahBidAggQuery+where, baseArgs...)
	if err := aggRow.Scan(&auctionsWithBid, &totalCurrentBid, &totalStartBid, &withBuyout); err != nil {
		return nil, fmt.Errorf("bid-activity aggregate: %w", err)
	}

	// 2. Most-contested: the highest current bids across all live-bid listings.
	contestedQuery := ahBidListBaseQuery + where + " ORDER BY ah.lastbid DESC, ah.id ASC LIMIT ?"
	contestedArgs := append(append([]any{}, baseArgs...), topN)
	mostContested, err := queryAhBidAuctions(ctx, conn, contestedQuery, contestedArgs)
	if err != nil {
		return nil, fmt.Errorf("most-contested query: %w", err)
	}

	// 3. Closest-to-buyout: live-bid listings whose current bid is nearest the
	// buyout price. buyoutprice > 0 is guarded IN (auction-only rows have no buyout
	// to approach — dividing by 0 would be meaningless), so the extra cond carries
	// no bind param and the arg order stays baseArgs + LIMIT.
	closestConds := append(append([]string{}, conds...), "ah.buyoutprice > 0")
	closestWhere := " WHERE " + strings.Join(closestConds, " AND ")
	closestQuery := ahBidListBaseQuery + closestWhere + " ORDER BY (ah.lastbid / ah.buyoutprice) DESC, ah.id ASC LIMIT ?"
	closestArgs := append(append([]any{}, baseArgs...), topN)
	closestToBuyout, err := queryAhBidAuctions(ctx, conn, closestQuery, closestArgs)
	if err != nil {
		return nil, fmt.Errorf("closest-to-buyout query: %w", err)
	}

	var avgCurrentBid int64
	if auctionsWithBid > 0 {
		avgCurrentBid = totalCurrentBid / int64(auctionsWithBid)
	}

	out := map[string]any{
		"asOf": now.UTC().Format(time.RFC3339),
		"topN": topN,
		"totals": map[string]any{
			"auctionsWithBid":       auctionsWithBid,
			"withBuyout":            withBuyout,
			"totalCurrentBidCopper": totalCurrentBid,
			"totalCurrentBidGold":   formatCopper(totalCurrentBid),
			"totalStartBidCopper":   totalStartBid,
			"totalStartBidGold":     formatCopper(totalStartBid),
			"avgCurrentBidCopper":   avgCurrentBid,
			"avgCurrentBidGold":     formatCopper(avgCurrentBid),
			"bidOverStartPct":       ahBidPct(totalCurrentBid-totalStartBid, totalStartBid),
		},
		"mostContested":   mostContested,
		"closestToBuyout": closestToBuyout,
	}
	// Filters are echoed only when set, so an absent key == no filter applied.
	if houseID != nil {
		out["houseId"] = *houseID
		out["faction"] = ahHouseFaction(*houseID)
	}
	if minBid != nil {
		out["minBidCopper"] = *minBid
	}
	return out, nil
}

// queryAhBidAuctions runs one of the top-N list queries and folds each row into an
// ahBidAuction, computing the per-row gold strings and bid progression. Returns a
// non-nil (possibly empty) slice so the JSON always renders an array.
func queryAhBidAuctions(ctx context.Context, conn *sql.Conn, query string, args []any) ([]*ahBidAuction, error) {
	rows, err := conn.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]*ahBidAuction, 0, 16)
	for rows.Next() {
		var (
			id, entry, startbid, lastbid, buyout int64
			houseID, quality                     int
			name                                 string
		)
		if err := rows.Scan(&id, &houseID, &entry, &name, &quality, &startbid, &lastbid, &buyout); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		a := &ahBidAuction{
			AuctionID:        id,
			HouseID:          houseID,
			Faction:          ahHouseFaction(houseID),
			ItemEntry:        entry,
			ItemName:         name,
			Quality:          quality,
			QualityName:      itemQualityName(quality),
			StartBidCopper:   startbid,
			StartBidGold:     formatCopper(startbid),
			CurrentBidCopper: lastbid,
			CurrentBidGold:   formatCopper(lastbid),
			BuyoutCopper:     buyout,
			BuyoutGold:       formatCopper(buyout),
			HasBuyout:        buyout > 0,
			// How far the current bid climbed above the opening bid.
			BidOverStartPct: ahBidPct(lastbid-startbid, startbid),
		}
		// Only meaningful for listings that carry a buyout to approach.
		if buyout > 0 {
			a.BidToBuyoutPct = ahBidPct(lastbid, buyout)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iter: %w", err)
	}
	return out, nil
}
