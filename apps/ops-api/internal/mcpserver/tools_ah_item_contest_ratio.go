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
	ahContestTimeout         = 10 * time.Second
	ahContestDefaultTop      = 15
	ahContestMaxTop          = 100
	ahContestMaxQuality      = 7 // AzerothCore item Quality enum tops out at 7 (Heirloom)
	ahContestDefaultMinItems = 2 // <2 listings makes contestRatioPct trivially 0/100; the default guard hides that noise
)

// ahContestSortColumns is the sortBy allowlist. The VALUE is spliced into the
// display query's ORDER BY, so it is a fixed server-owned string (never caller
// text) — invalid sortBy is rejected before any query runs, so an operator typo
// can't reach the SQL. The "contested" ratio orders by the aggregate EXPRESSION
// directly (leading/listings*100) rather than an alias: MySQL/MariaDB both accept
// an aggregate expression in ORDER BY of a GROUP BY query without spelling it in
// the SELECT, so there is no alias-in-ORDER-BY footgun (the aggregate is not
// scanned back — the Go side recomputes contestRatioPct from leadingAuctions +
// listings so the displayed number and the sort key share one formula).
// "demand"/"supply" order by the leadingAuctions/listings aliases, which ARE in
// the SELECT.
var ahContestSortColumns = map[string]string{
	"contested": "(SUM(CASE WHEN ah.buyguid <> 0 THEN 1 ELSE 0 END) * 100.0 / COUNT(*))",
	"demand":    "leadingAuctions",
	"supply":    "listings",
}

// ahContestJoin is the cross-DB item join shared with ah_market_top_items /
// ah_market_category_breakdown: acore_characters.auctionhouse.itemguid ->
// item_instance.guid, item_instance.itemEntry -> acore_world.item_template.entry
// (ops_ro holds SELECT on both characters and world). INNER JOINs drop dangling
// item pointers so only extant listings count. NO `WHERE buyguid <> 0` scope
// here (unlike a pure-demand tool): this ratio needs EVERY listing as the
// denominator, and the live-bid subset is a SUM(CASE) inside each group.
const ahContestJoin = "FROM `auctionhouse` ah " +
	"JOIN `item_instance` ii ON ah.itemguid = ii.guid " +
	"JOIN `acore_world`.`item_template` it ON ii.itemEntry = it.entry"

// ahContestCensusSelect is the honest realm-wide census: single-row totals over
// the SAME filtered join with NO GROUP BY and NO LIMIT, so distinctItems /
// totalListings / totalLeadingAuctions never truncate at topN (the ah_item_demand
// separate-census idiom — per-item GROUP BY cardinality is unbounded, so a
// fold-all-then-slice scanCap could under-count these denominators). COALESCE the
// SUM so an empty market scans to 0, not a NULL that fails Scan into int.
const ahContestCensusSelect = "SELECT COUNT(DISTINCT it.entry) AS distinctItems, " +
	"COUNT(*) AS totalListings, " +
	"COALESCE(SUM(CASE WHEN ah.buyguid <> 0 THEN 1 ELSE 0 END), 0) AS totalLeadingAuctions " +
	ahContestJoin

// ahContestDisplaySelect is the per-item leaderboard. listings = all rows;
// leadingAuctions = the live-bid subset (auctionhouse.buyguid is a bidder GUID, 0
// when no bid); distinctBidders counts distinct bidders over that subset (the
// CASE yields NULL for no-bid rows and COUNT(DISTINCT) skips NULLs). GROUP BY
// spells all four non-aggregate columns so the query is valid under
// ONLY_FULL_GROUP_BY on both MySQL 8 and MariaDB.
const ahContestDisplaySelect = "SELECT it.entry, it.name, it.Quality, it.class, " +
	"COUNT(*) AS listings, " +
	"SUM(CASE WHEN ah.buyguid <> 0 THEN 1 ELSE 0 END) AS leadingAuctions, " +
	"COUNT(DISTINCT CASE WHEN ah.buyguid <> 0 THEN ah.buyguid END) AS distinctBidders " +
	ahContestJoin

// ahContestItem is one item's supply-vs-demand tension. ContestRatioPct =
// leadingAuctions / listings * 100 (Go-computed via ahCategoryValuePct so the
// displayed value matches the SQL sort expression). A high ratio on a low listing
// count is a supply squeeze (everything posted is under bid); a low ratio on a
// high listing count is a glut.
type ahContestItem struct {
	ItemEntry       int64   `json:"itemEntry"`
	ItemName        string  `json:"itemName"`
	Quality         int     `json:"quality"`
	QualityName     string  `json:"qualityName"`
	Class           int     `json:"class"`
	ClassName       string  `json:"className"`
	Listings        int     `json:"listings"`
	LeadingAuctions int     `json:"leadingAuctions"`
	DistinctBidders int     `json:"distinctBidders"`
	ContestRatioPct float64 `json:"contestRatioPct"`
}

// RegisterAhItemContestRatioTool registers `ah_item_contest_ratio` — a read-only,
// per-item supply-vs-demand tension view of the auction house.
//
// ah_market_top_items ranks item TYPES by pure SUPPLY (total listing count);
// ah_item_demand ranks pure DEMAND (listings under a live bid). NEITHER exposes
// the RATIO between them. An item with 3 listings all under bid (100% contested)
// is a supply squeeze; an item with 300 listings and 5 under bid (1.7%) is a
// glut — this tool surfaces exactly that market-tension lens neither sibling
// gives alone, joining auctionhouse -> item_instance -> acore_world.item_template
// and folding the live-bid subset per item.
func RegisterAhItemContestRatioTool(reg *Registry, deps DBDeps) {
	reg.Register(Tool{
		Name: "ah_item_contest_ratio",
		Description: "Per-item auction-house supply-vs-demand tension, joining " +
			"acore_characters.auctionhouse -> item_instance -> acore_world.item_template over the ops_ro pool. " +
			"For each item TYPE: listings (all active listings = supply), leadingAuctions (listings carrying a live " +
			"bid = demand, auctionhouse.buyguid<>0), contestRatioPct (leadingAuctions/listings*100 — the market " +
			"tension), distinctBidders, plus itemEntry, itemName, quality + qualityName (0 Poor .. 7 Heirloom, an " +
			"unknown value surfaces as quality-<q>) and class + className (the ItemClass enum, unknown as class-<c>). " +
			"Surfaces supply-squeeze items (few listings, all contested) that neither ah_market_top_items (pure " +
			"supply) nor ah_item_demand (pure demand) shows alone. Honest realm-wide totals (distinctItems, " +
			"totalListings, totalLeadingAuctions, marketContestRatioPct) are a separate census independent of topN. " +
			"Args: topN (default 15, max 100), sortBy (contested [default, by contestRatioPct] | demand [by " +
			"leadingAuctions] | supply [by listings]; an unknown key is rejected), minItems (default 2 — only items " +
			"with at least this many listings, filtering trivially-100%/0% single-listing noise; clamped to >=1), " +
			"minQuality (optional 0-7 — only items at or above this rarity; out-of-range is rejected, not clamped), " +
			"houseId (optional 2=Alliance, 6=Horde, 7=Neutral, the AzerothCore AuctionHouseId; another value is " +
			"rejected; omit for the whole market). Sorted by the chosen key desc, itemEntry asc tiebreak. Read-only, " +
			"10 s timeout.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{
			"topN":{"type":"integer","description":"Max items returned (default 15, max 100)"},
			"sortBy":{"type":"string","enum":["contested","demand","supply"],"description":"contested=contestRatioPct (default), demand=leadingAuctions, supply=listings"},
			"minItems":{"type":"integer","description":"Only items with at least this many listings (default 2, clamped to >=1) — filters trivially-100%/0% single-listing noise"},
			"minQuality":{"type":"integer","description":"Only items at or above this rarity (0=Poor .. 7=Heirloom); omit for all qualities"},
			"houseId":{"type":"integer","description":"Narrow to one auction house (2=Alliance, 6=Horde, 7=Neutral); omit for the whole market"}
		}}`),
		Annotations: AnnRead(),
		Handler: func(ctx context.Context, raw json.RawMessage, _ string) any {
			var a struct {
				TopN       int    `json:"topN"`
				SortBy     string `json:"sortBy"`
				MinItems   *int   `json:"minItems"`
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
				topN = ahContestDefaultTop
			}
			if topN > ahContestMaxTop {
				topN = ahContestMaxTop
			}
			// sortBy resolves to an allowlist key; empty -> the "contested" default.
			// An unknown key is rejected (no query) so a typo can't silently reorder
			// the market rather than erroring.
			sortKey := a.SortBy
			if sortKey == "" {
				sortKey = "contested"
			}
			if _, ok := ahContestSortColumns[sortKey]; !ok {
				return map[string]any{"error": fmt.Sprintf("invalid sortBy: %q (valid: contested, demand, supply)", a.SortBy)}
			}
			// minItems is a soft floor, not an enum: default 2, clamp <1 up to 1 (a
			// floor below one listing is meaningless).
			minItems := ahContestDefaultMinItems
			if a.MinItems != nil {
				minItems = *a.MinItems
				if minItems < 1 {
					minItems = 1
				}
			}
			// minQuality is rejected (not clamped) when outside 0-7 so a typo'd 99
			// reads as "bad filter", not an empty market (mirrors ah_market_top_items).
			if a.MinQuality != nil && (*a.MinQuality < 0 || *a.MinQuality > ahContestMaxQuality) {
				return map[string]any{"error": fmt.Sprintf("minQuality out of range: %d (valid 0-%d)", *a.MinQuality, ahContestMaxQuality)}
			}
			// houseId is rejected (not clamped) when it isn't a real AuctionHouseId so
			// a bad id can't masquerade as an empty house (matches the ah_* family).
			if a.HouseID != nil && *a.HouseID != 2 && *a.HouseID != 6 && *a.HouseID != 7 {
				return map[string]any{"error": fmt.Sprintf("invalid houseId: %d (valid 2=Alliance, 6=Horde, 7=Neutral)", *a.HouseID)}
			}
			c, cancel := context.WithTimeout(ctx, ahContestTimeout)
			defer cancel()
			out, err := collectAhItemContestRatio(c, deps.QueryDB, time.Now(), topN, minItems, sortKey, a.HouseID, a.MinQuality)
			if err != nil {
				return map[string]any{"error": err.Error()}
			}
			return out
		},
	})
}

// collectAhItemContestRatio runs the census + display queries on one conn (so the
// single USE applies to both) and folds the per-item leaderboard. Split out so
// tests can drive it with sqlmock and a fixed `now`. The SQL ORDER BY is the
// source of truth for row order — the handler does NOT re-sort.
func collectAhItemContestRatio(ctx context.Context, db *sql.DB, now time.Time, topN, minItems int, sortKey string, houseID, minQuality *int) (map[string]any, error) {
	sortCol := ahContestSortColumns[sortKey] // validated in the handler; always present

	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire conn: %w", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "USE `acore_characters`"); err != nil {
		return nil, fmt.Errorf("USE acore_characters: %w", err)
	}

	// Shared WHERE (houseId / minQuality) — applied to BOTH the census and the
	// display query so the honest totals scope to exactly what the leaderboard shows.
	var conds []string
	var baseArgs []any
	if houseID != nil {
		conds = append(conds, "ah.houseid = ?")
		baseArgs = append(baseArgs, *houseID)
	}
	if minQuality != nil {
		conds = append(conds, "it.Quality >= ?")
		baseArgs = append(baseArgs, *minQuality)
	}
	where := ""
	if len(conds) > 0 {
		where = " WHERE " + strings.Join(conds, " AND ")
	}

	// Census: one row of realm-wide denominators, no GROUP BY / no LIMIT / no minItems.
	var distinctItems, totalListings, totalLeadingAuctions int
	if err := conn.QueryRowContext(ctx, ahContestCensusSelect+where, baseArgs...).
		Scan(&distinctItems, &totalListings, &totalLeadingAuctions); err != nil {
		return nil, fmt.Errorf("contest census query: %w", err)
	}

	// Display: per-item leaderboard. minItems is a HAVING floor on the listing
	// count; topN caps the returned rows. Bind order: WHERE args, then minItems,
	// then topN.
	displayQuery := ahContestDisplaySelect + where +
		" GROUP BY it.entry, it.name, it.Quality, it.class" +
		" HAVING listings >= ?" +
		" ORDER BY " + sortCol + " DESC, it.entry ASC LIMIT ?"
	displayArgs := append(append([]any{}, baseArgs...), minItems, topN)

	rows, err := conn.QueryContext(ctx, displayQuery, displayArgs...)
	if err != nil {
		return nil, fmt.Errorf("contest display query: %w", err)
	}
	defer rows.Close()

	items := make([]*ahContestItem, 0, topN)
	for rows.Next() {
		var (
			entry                                      int64
			name                                       string
			quality, class, listings, leading, bidders int
		)
		if err := rows.Scan(&entry, &name, &quality, &class, &listings, &leading, &bidders); err != nil {
			return nil, fmt.Errorf("contest scan: %w", err)
		}
		items = append(items, &ahContestItem{
			ItemEntry:       entry,
			ItemName:        name,
			Quality:         quality,
			QualityName:     itemQualityName(quality),
			Class:           class,
			ClassName:       itemClassName(class),
			Listings:        listings,
			LeadingAuctions: leading,
			DistinctBidders: bidders,
			ContestRatioPct: ahCategoryValuePct(int64(leading), int64(listings)),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("contest iter: %w", err)
	}

	out := map[string]any{
		"asOf":           now.UTC().Format(time.RFC3339),
		"topN":           topN,
		"minItems":       minItems,
		"sortBy":         sortKey,
		"displayedItems": len(items),
		"totals": map[string]any{
			"distinctItems":         distinctItems,
			"totalListings":         totalListings,
			"totalLeadingAuctions":  totalLeadingAuctions,
			"marketContestRatioPct": ahCategoryValuePct(int64(totalLeadingAuctions), int64(totalListings)),
		},
		"items": items,
	}
	// houseId/faction and minQuality are echoed only when set, so an absent key
	// means no filter was applied.
	if houseID != nil {
		out["houseId"] = *houseID
		out["faction"] = ahHouseFaction(*houseID)
	}
	if minQuality != nil {
		out["minQuality"] = *minQuality
	}
	return out, nil
}
