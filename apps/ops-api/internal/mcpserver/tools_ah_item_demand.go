package mcpserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

const (
	ahItemDemandTimeout    = 10 * time.Second
	ahItemDemandDefaultTop = 15
	ahItemDemandMaxTop     = 100
	ahItemDemandMaxQuality = 7 // AzerothCore item Quality enum tops out at 7 (Heirloom)
)

// ahItemDemandSortColumns is the sortBy allowlist. The map VALUE is the SELECT
// alias interpolated into ORDER BY — so a caller can never inject arbitrary SQL
// (only these four keys resolve; anything else is rejected before a query runs).
// EVERY value is an aggregate alias declared with `AS <alias>` in the display
// SELECT, so real MySQL resolves the ORDER BY (an unaliased aggregate in the
// ORDER BY errors "Unknown column in 'order clause'" — sqlmock only string-matches
// so it would pass CI yet die at runtime; the AS aliases avoid that trap).
var ahItemDemandSortColumns = map[string]string{
	"bids":        "leadingAuctions",   // COUNT(*) — most-contested item types
	"bidders":     "distinctBidders",   // COUNT(DISTINCT buyguid) — broadest interest
	"committed":   "totalCommittedBid", // SUM(lastbid) — most bid capital committed
	"buyoutValue": "totalBuyoutValue",  // SUM(buyoutprice) — highest-value contested
}

// ahItemDemandFrom is the cross-DB join ah_market_top_items established VERBATIM:
// acore_characters.auctionhouse.itemguid -> item_instance.guid, then
// item_instance.itemEntry -> acore_world.item_template.entry (ops_ro holds SELECT
// on both characters and world). INNER JOINs drop dangling item pointers (an
// auction row whose item_instance/template was deleted). The demand scope
// (WHERE ah.buyguid <> 0) is appended by collectAhItemDemand so both the census
// and the display query share one join + one filter string.
const ahItemDemandFrom = " FROM `auctionhouse` ah " +
	"JOIN `item_instance` ii ON ah.itemguid = ii.guid " +
	"JOIN `acore_world`.`item_template` it ON ii.itemEntry = it.entry"

// ahItemDemandCensusHead is the honest realm-wide census — a single-row COUNT/SUM
// over the SAME filtered join as the display query but with NO GROUP BY and NO
// LIMIT, so the totals can never be truncated by topN (the ah_market_bid_activity
// honest-census idiom). COALESCE guards SUM returning NULL on an empty market.
const ahItemDemandCensusHead = "SELECT COUNT(DISTINCT it.entry) AS distinctItems, " +
	"COUNT(*) AS totalLeadingAuctions, " +
	"COALESCE(SUM(ah.lastbid), 0) AS totalCommittedBid, " +
	"COALESCE(SUM(ah.buyoutprice), 0) AS totalBuyoutValue"

// ahItemDemandDisplayHead is the per-item grouped SELECT. leadingAuctions counts
// the live-bid rows for the item; distinctBidders (COUNT DISTINCT buyguid) answers
// "one whale or a crowd fighting?"; totalCommittedBid sums the current bids; and
// totalBuyoutValue sums the buyout of those contested listings. Every aggregate is
// aliased so the interpolated ORDER BY resolves on real MySQL.
const ahItemDemandDisplayHead = "SELECT it.entry, it.name, it.Quality, it.class, " +
	"COUNT(*) AS leadingAuctions, " +
	"COUNT(DISTINCT ah.buyguid) AS distinctBidders, " +
	"COALESCE(SUM(ah.lastbid), 0) AS totalCommittedBid, " +
	"COALESCE(SUM(ah.buyoutprice), 0) AS totalBuyoutValue"

// ahItemDemandGroupBy spells out every non-aggregate column so the query is valid
// under ONLY_FULL_GROUP_BY on both MySQL 8 and MariaDB (entry is the PK so name /
// Quality / class are functionally dependent, but spelling the group out stays
// portable across servers that don't detect the dependency through the join alias
// — the same explicit-group choice ah_market_top_items makes).
const ahItemDemandGroupBy = " GROUP BY it.entry, it.name, it.Quality, it.class"

// ahItemDemandItem is one grouped row — a single item type under active bid.
// Copper sums are the machine-readable int64 facet; the *Gold strings render the
// same values as WoW money for at-a-glance reading (formatCopper, shared with the
// ah_market_* family).
type ahItemDemandItem struct {
	ItemEntry          int64  `json:"itemEntry"`
	ItemName           string `json:"itemName"`
	Quality            int    `json:"quality"`
	QualityName        string `json:"qualityName"`
	Class              int    `json:"class"`
	ClassName          string `json:"className"`
	LeadingAuctions    int    `json:"leadingAuctions"`
	DistinctBidders    int    `json:"distinctBidders"`
	CommittedBidCopper int64  `json:"committedBidCopper"`
	CommittedBidGold   string `json:"committedBidGold"`
	BuyoutValueCopper  int64  `json:"buyoutValueCopper"`
	BuyoutValueGold    string `json:"buyoutValueGold"`
}

// RegisterAhItemDemandTool registers `ah_item_demand` — a read-only, item-level
// census of what the auction house is actively BIDDING on.
//
// This is the DEMAND-side mirror of ah_market_top_items (which ranks item types by
// listing count — the SUPPLY side, what's being posted). By scoping to live-bid
// rows (auctionhouse.buyguid <> 0) and grouping by item_template, it answers "which
// item TYPES are the market actually fighting over" — the item-level companion to
// ah_bidder_activity's per-bidder grain. On a mod-ah-bot server it surfaces which
// item types AHBot bidding concentrates on. Reuses ah_market_top_items' merged
// cross-DB join + the itemQualityName / itemClassName / formatCopper / ahHouseFaction
// helpers verbatim.
func RegisterAhItemDemandTool(reg *Registry, deps DBDeps) {
	reg.Register(Tool{
		Name: "ah_item_demand",
		Description: "Top-N auction-house item TYPES ranked by ACTIVE-BID demand, joining " +
			"acore_characters.auctionhouse -> item_instance -> acore_world.item_template over the ops_ro pool and " +
			"scoping to live-bid rows (buyguid <> 0). The DEMAND-side mirror of ah_market_top_items (which ranks the " +
			"SUPPLY side by listing count) and the item-level companion to ah_bidder_activity's per-bidder grain. Each " +
			"item carries itemEntry, itemName, quality + qualityName (0 Poor .. 7 Heirloom — an unknown value surfaces " +
			"as quality-<q>), class + className (the AzerothCore ItemClass enum; unknown surfaces as class-<c>), " +
			"leadingAuctions (listings with a live bid), distinctBidders (COUNT DISTINCT buyguid — one whale or a crowd " +
			"fighting?), and the committed-bid + buyout value in copper plus human gold strings. Honest realm totals " +
			"(distinctItems, totalLeadingAuctions, totalCommittedBid, totalBuyoutValue) come from a separate census so " +
			"they never truncate at topN. Answers 'which epics/consumables are hotly contested' and, on a mod-ah-bot " +
			"server, where AHBot bidding concentrates. Args: topN (default 15, max 100), sortBy (bids [default] | " +
			"bidders | committed | buyoutValue), houseId (optional — 2=Alliance, 6=Horde, 7=Neutral, the AzerothCore " +
			"AuctionHouseId; omit for the whole market), minQuality (optional 0-7 — only items at or above this rarity; " +
			"out-of-range is rejected, not clamped). Sorted by the chosen key desc, itemEntry asc tiebreak. Read-only, " +
			"10 s timeout.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{
			"topN":{"type":"integer","description":"Max item types returned, ranked by the sortBy key (default 15, max 100)"},
			"sortBy":{"type":"string","enum":["bids","bidders","committed","buyoutValue"],"description":"Ranking key: bids (leading-auction count, default), bidders (distinct bidders), committed (summed current bid), buyoutValue (summed buyout)"},
			"houseId":{"type":"integer","description":"Narrow to one auction house (2=Alliance, 6=Horde, 7=Neutral); omit for the whole market"},
			"minQuality":{"type":"integer","description":"Only items at or above this rarity (0=Poor .. 7=Heirloom); omit for all qualities"}
		}}`),
		Annotations: AnnRead(),
		Handler: func(ctx context.Context, raw json.RawMessage, _ string) any {
			var a struct {
				TopN       int    `json:"topN"`
				SortBy     string `json:"sortBy"`
				HouseID    *int   `json:"houseId"`
				MinQuality *int   `json:"minQuality"`
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
				topN = ahItemDemandDefaultTop
			}
			if topN > ahItemDemandMaxTop {
				topN = ahItemDemandMaxTop
			}
			// sortBy defaults to "bids"; anything outside the allowlist is rejected
			// (not silently coerced) so a typo can't quietly change the ranking axis.
			sortBy := a.SortBy
			if sortBy == "" {
				sortBy = "bids"
			}
			sortCol, ok := ahItemDemandSortColumns[sortBy]
			if !ok {
				return map[string]any{"error": fmt.Sprintf("invalid sortBy: %q (valid: bids, bidders, committed, buyoutValue)", sortBy)}
			}
			// minQuality is rejected (not silently clamped) when outside the 0-7 enum
			// range — a typo'd 99 would otherwise return an empty list that reads as
			// "no items" rather than "bad filter" (mirrors ah_market_top_items).
			if a.MinQuality != nil && (*a.MinQuality < 0 || *a.MinQuality > ahItemDemandMaxQuality) {
				return map[string]any{"error": fmt.Sprintf("minQuality out of range: %d (valid 0-%d)", *a.MinQuality, ahItemDemandMaxQuality)}
			}
			c, cancel := context.WithTimeout(ctx, ahItemDemandTimeout)
			defer cancel()
			out, err := collectAhItemDemand(c, deps.QueryDB, time.Now(), topN, sortBy, sortCol, a.HouseID, a.MinQuality)
			if err != nil {
				return map[string]any{"error": err.Error()}
			}
			return out
		},
	})
}

// collectAhItemDemand runs the honest census first, then the top-N per-item
// display query, sharing one connection and one WHERE fragment. Split out so tests
// can drive it with sqlmock and a fixed `now`. The SQL ORDER BY (chosen key desc,
// itemEntry asc) is the source of truth for row order — the handler does NOT
// re-sort (matching ah_market_top_items). sortCol is an allowlist VALUE, never
// caller text, so the interpolation is injection-safe.
func collectAhItemDemand(ctx context.Context, db *sql.DB, now time.Time, topN int, sortBy, sortCol string, houseID, minQuality *int) (map[string]any, error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire conn: %w", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "USE `acore_characters`"); err != nil {
		return nil, fmt.Errorf("USE acore_characters: %w", err)
	}

	// One WHERE fragment shared by both queries: the live-bid scope is always
	// present, filters append after it. filterArgs are bound in the same order
	// (houseId then minQuality) for census and display; display also binds topN.
	conds := []string{"ah.buyguid <> 0"}
	var filterArgs []any
	if houseID != nil {
		conds = append(conds, "ah.houseid = ?")
		filterArgs = append(filterArgs, *houseID)
	}
	if minQuality != nil {
		conds = append(conds, "it.Quality >= ?")
		filterArgs = append(filterArgs, *minQuality)
	}
	where := " WHERE " + strings.Join(conds, " AND ")

	// Census: single-row honest totals, never LIMIT-truncated.
	censusQuery := ahItemDemandCensusHead + ahItemDemandFrom + where
	var distinctItems, totalLeadingAuctions int
	var totalCommittedBid, totalBuyoutValue int64
	if err := conn.QueryRowContext(ctx, censusQuery, filterArgs...).
		Scan(&distinctItems, &totalLeadingAuctions, &totalCommittedBid, &totalBuyoutValue); err != nil {
		return nil, fmt.Errorf("item-demand census: %w", err)
	}

	// Display: top-N per-item grouped rows, ranked by the chosen key.
	displayQuery := ahItemDemandDisplayHead + ahItemDemandFrom + where + ahItemDemandGroupBy +
		" ORDER BY " + sortCol + " DESC, it.entry ASC LIMIT ?"
	displayArgs := append(append([]any{}, filterArgs...), topN)

	rows, err := conn.QueryContext(ctx, displayQuery, displayArgs...)
	if err != nil {
		return nil, fmt.Errorf("item-demand query: %w", err)
	}
	defer rows.Close()

	items := make([]*ahItemDemandItem, 0, topN)
	for rows.Next() {
		var (
			entry                            int64
			name                             string
			quality, class                   int
			leadingAuctions, distinctBidders int
			committedBid, buyoutValue        int64
		)
		if err := rows.Scan(&entry, &name, &quality, &class, &leadingAuctions, &distinctBidders, &committedBid, &buyoutValue); err != nil {
			return nil, fmt.Errorf("item-demand scan: %w", err)
		}
		items = append(items, &ahItemDemandItem{
			ItemEntry:          entry,
			ItemName:           name,
			Quality:            quality,
			QualityName:        itemQualityName(quality),
			Class:              class,
			ClassName:          itemClassName(class),
			LeadingAuctions:    leadingAuctions,
			DistinctBidders:    distinctBidders,
			CommittedBidCopper: committedBid,
			CommittedBidGold:   formatCopper(committedBid),
			BuyoutValueCopper:  buyoutValue,
			BuyoutValueGold:    formatCopper(buyoutValue),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("item-demand iter: %w", err)
	}

	out := map[string]any{
		"asOf":   now.UTC().Format(time.RFC3339),
		"topN":   topN,
		"sortBy": sortBy,
		"totals": map[string]any{
			"distinctItems":           distinctItems,
			"totalLeadingAuctions":    totalLeadingAuctions,
			"totalCommittedBidCopper": totalCommittedBid,
			"totalCommittedBidGold":   formatCopper(totalCommittedBid),
			"totalBuyoutValueCopper":  totalBuyoutValue,
			"totalBuyoutValueGold":    formatCopper(totalBuyoutValue),
		},
		"items": items,
	}
	// houseId/faction and minQuality are echoed only when the filter was set, so
	// the response reflects exactly what was applied (an absent key == no filter).
	if houseID != nil {
		out["houseId"] = *houseID
		out["faction"] = ahHouseFaction(*houseID)
	}
	if minQuality != nil {
		out["minQuality"] = *minQuality
	}
	return out, nil
}
