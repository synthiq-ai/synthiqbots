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
	ahDispersionTimeout         = 10 * time.Second
	ahDispersionDefaultTop      = 15
	ahDispersionMaxTop          = 100
	ahDispersionMaxQuality      = 7 // AzerothCore item Quality enum tops out at 7 (Heirloom)
	ahDispersionDefaultMinItems = 2 // a 1-listing item has zero spread (a single price point); the default floor hides that noise
)

// ahDispersionSortColumns is the sortBy allowlist. The VALUE is spliced into the
// display query's ORDER BY, so it is a fixed server-owned string (never caller
// text) — an unknown sortBy is rejected before any query runs, so an operator
// typo can't reach the SQL. "spread" orders by the aggregate EXPRESSION directly
// ((max-min)*100/min) rather than an alias: MySQL/MariaDB both accept an aggregate
// expression in ORDER BY of a GROUP BY query without spelling it in the SELECT, so
// there is no alias-in-ORDER-BY footgun (the expression is not scanned back — the
// Go side recomputes spreadPct from minBuyout + maxBuyout via ahCategoryValuePct so
// the displayed number and the sort key share one formula). Division by MIN is safe
// because the base WHERE scopes to buyoutprice>0, so every group's MIN is >=1.
// "avgPrice" likewise orders by the SUM/COUNT average expression (Go recomputes the
// displayed avg the same way); "listings" orders by the COUNT(*) alias, which IS in
// the SELECT.
var ahDispersionSortColumns = map[string]string{
	"spread":   "((MAX(ah.buyoutprice) - MIN(ah.buyoutprice)) * 100.0 / MIN(ah.buyoutprice))",
	"listings": "listings",
	"avgPrice": "(SUM(ah.buyoutprice) / COUNT(*))",
}

// ahDispersionJoin is the cross-DB item join shared with ah_market_top_items /
// ah_item_contest_ratio: acore_characters.auctionhouse.itemguid ->
// item_instance.guid, item_instance.itemEntry -> acore_world.item_template.entry
// (ops_ro holds SELECT on both characters and world). INNER JOINs drop dangling
// item pointers so only extant listings count.
const ahDispersionJoin = "FROM `auctionhouse` ah " +
	"JOIN `item_instance` ii ON ah.itemguid = ii.guid " +
	"JOIN `acore_world`.`item_template` it ON ii.itemEntry = it.entry"

// ahDispersionCensusSelect is the honest realm-wide census: single-row totals over
// the SAME filtered join with NO GROUP BY and NO LIMIT, so distinctItems /
// totalPricedListings / the market-wide min+max never truncate at topN (the
// ah_item_contest_ratio separate-census idiom — per-item GROUP BY cardinality is
// unbounded, so a fold-all-then-slice scanCap could under-count these). COALESCE
// the MIN/MAX so an empty market scans to 0, not a NULL that fails Scan into int64.
const ahDispersionCensusSelect = "SELECT COUNT(DISTINCT it.entry) AS distinctItems, " +
	"COUNT(*) AS totalPricedListings, " +
	"COALESCE(MIN(ah.buyoutprice), 0) AS marketMin, " +
	"COALESCE(MAX(ah.buyoutprice), 0) AS marketMax " +
	ahDispersionJoin

// ahDispersionDisplaySelect is the per-item leaderboard. listings = COUNT(*) of
// priced listings; distinctSellers = COUNT(DISTINCT itemowner) (do a handful of
// sellers or a whole crowd disagree on price?); min/max/total buyout drive the
// spread + avg (avg is folded in Go from total/listings, matching ah_market_top_items
// so nothing scans a DECIMAL). GROUP BY spells all four non-aggregate columns so
// the query is valid under ONLY_FULL_GROUP_BY on both MySQL 8 and MariaDB.
const ahDispersionDisplaySelect = "SELECT it.entry, it.name, it.Quality, it.class, " +
	"COUNT(*) AS listings, " +
	"COUNT(DISTINCT ah.itemowner) AS distinctSellers, " +
	"MIN(ah.buyoutprice) AS minBuyout, " +
	"MAX(ah.buyoutprice) AS maxBuyout, " +
	"SUM(ah.buyoutprice) AS totalBuyout " +
	ahDispersionJoin

// ahDispersionItem is one item TYPE's buyout-price dispersion. SpreadPct =
// (max-min)/min*100 (Go-computed via ahCategoryValuePct so the displayed value
// matches the SQL sort expression). A wide spread means the sellers of the same
// item wildly disagree on price — a mispricing / arbitrage window or an active
// undercut war; a tight spread means a settled price.
type ahDispersionItem struct {
	ItemEntry       int64   `json:"itemEntry"`
	ItemName        string  `json:"itemName"`
	Quality         int     `json:"quality"`
	QualityName     string  `json:"qualityName"`
	Class           int     `json:"class"`
	ClassName       string  `json:"className"`
	Listings        int     `json:"listings"`
	DistinctSellers int     `json:"distinctSellers"`
	MinBuyoutCopper int64   `json:"minBuyoutCopper"`
	MinBuyoutGold   string  `json:"minBuyoutGold"`
	MaxBuyoutCopper int64   `json:"maxBuyoutCopper"`
	MaxBuyoutGold   string  `json:"maxBuyoutGold"`
	AvgBuyoutCopper int64   `json:"avgBuyoutCopper"`
	AvgBuyoutGold   string  `json:"avgBuyoutGold"`
	SpreadPct       float64 `json:"spreadPct"`
}

// RegisterAhItemPriceDispersionTool registers `ah_item_price_dispersion` — a
// read-only, per-item view of how much the sellers of the SAME item disagree on
// price.
//
// The ah_* PRICE family is market-wide or per-listing: ah_market_price_percentiles
// gives the whole-market price distribution; ah_market_price_outliers[_by_seller]
// flag individual over/under-priced LISTINGS; ah_market_top_items reports only the
// floor + avg buyout per item. NONE reports per-item-TYPE price DISPERSION. An item
// whose buyouts range 5g-500g (spread 9900%) is either a mispricing/arbitrage
// window or an active undercut war; a tight spread means a settled price. This tool
// joins auctionhouse -> item_instance -> acore_world.item_template, scopes to rows
// carrying a buyout, and folds MIN/MAX/AVG(buyoutprice) per item entry.
func RegisterAhItemPriceDispersionTool(reg *Registry, deps DBDeps) {
	reg.Register(Tool{
		Name: "ah_item_price_dispersion",
		Description: "Per-item auction-house buyout-price dispersion (how much the sellers of the SAME item TYPE " +
			"disagree on price), joining acore_characters.auctionhouse -> item_instance -> acore_world.item_template " +
			"over the ops_ro pool and scoping to listings that carry a buyout (buyoutprice>0). For each item TYPE: " +
			"minBuyout / maxBuyout / avgBuyout (copper + human gold), spreadPct ((max-min)/min*100 — a wide spread is " +
			"a mispricing/arbitrage or undercut-war signal, a tight one a settled price), listings, distinctSellers " +
			"(how many sellers set the price), plus itemEntry, itemName, quality + qualityName (0 Poor .. 7 Heirloom, " +
			"an unknown value surfaces as quality-<q>) and class + className (the ItemClass enum, unknown as class-<c>). " +
			"Surfaces the seller price-disagreement lens neither ah_market_price_percentiles (whole-market) nor " +
			"ah_market_top_items (floor+avg only) shows per item TYPE. Honest realm-wide totals (distinctItems, " +
			"totalPricedListings, marketMin/Max, marketSpreadPct) are a separate census independent of topN. Args: " +
			"topN (default 15, max 100), sortBy (spread [default, by spreadPct] | listings | avgPrice; an unknown key " +
			"is rejected), minListings (default 2 — only items with at least this many priced listings, filtering the " +
			"zero-spread single-listing noise; clamped to >=1), minQuality (optional 0-7 — only items at or above this " +
			"rarity; out-of-range is rejected, not clamped), houseId (optional 2=Alliance, 6=Horde, 7=Neutral, the " +
			"AzerothCore AuctionHouseId; another value is rejected; omit for the whole market). Sorted by the chosen " +
			"key desc, itemEntry asc tiebreak. Read-only, 10 s timeout.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{
			"topN":{"type":"integer","description":"Max items returned (default 15, max 100)"},
			"sortBy":{"type":"string","enum":["spread","listings","avgPrice"],"description":"spread=spreadPct (default), listings=listing count, avgPrice=average buyout"},
			"minListings":{"type":"integer","description":"Only items with at least this many priced listings (default 2, clamped to >=1) — filters the zero-spread single-listing noise"},
			"minQuality":{"type":"integer","description":"Only items at or above this rarity (0=Poor .. 7=Heirloom); omit for all qualities"},
			"houseId":{"type":"integer","description":"Narrow to one auction house (2=Alliance, 6=Horde, 7=Neutral); omit for the whole market"}
		}}`),
		Annotations: AnnRead(),
		Handler: func(ctx context.Context, raw json.RawMessage, _ string) any {
			var a struct {
				TopN        int    `json:"topN"`
				SortBy      string `json:"sortBy"`
				MinListings *int   `json:"minListings"`
				HouseID     *int   `json:"houseId"`
				MinQuality  *int   `json:"minQuality"`
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
				topN = ahDispersionDefaultTop
			}
			if topN > ahDispersionMaxTop {
				topN = ahDispersionMaxTop
			}
			// sortBy resolves to an allowlist key; empty -> the "spread" default.
			// An unknown key is rejected (no query) so a typo can't silently
			// reorder the market rather than erroring.
			sortKey := a.SortBy
			if sortKey == "" {
				sortKey = "spread"
			}
			if _, ok := ahDispersionSortColumns[sortKey]; !ok {
				return map[string]any{"error": fmt.Sprintf("invalid sortBy: %q (valid: spread, listings, avgPrice)", a.SortBy)}
			}
			// minListings is a soft floor, not an enum: default 2, clamp <1 up to 1
			// (a floor below one listing is meaningless).
			minListings := ahDispersionDefaultMinItems
			if a.MinListings != nil {
				minListings = *a.MinListings
				if minListings < 1 {
					minListings = 1
				}
			}
			// minQuality is rejected (not clamped) when outside 0-7 so a typo'd 99
			// reads as "bad filter", not an empty market (mirrors ah_market_top_items).
			if a.MinQuality != nil && (*a.MinQuality < 0 || *a.MinQuality > ahDispersionMaxQuality) {
				return map[string]any{"error": fmt.Sprintf("minQuality out of range: %d (valid 0-%d)", *a.MinQuality, ahDispersionMaxQuality)}
			}
			// houseId is rejected (not clamped) when it isn't a real AuctionHouseId
			// so a bad id can't masquerade as an empty house (matches the ah_* family).
			if a.HouseID != nil && *a.HouseID != 2 && *a.HouseID != 6 && *a.HouseID != 7 {
				return map[string]any{"error": fmt.Sprintf("invalid houseId: %d (valid 2=Alliance, 6=Horde, 7=Neutral)", *a.HouseID)}
			}
			c, cancel := context.WithTimeout(ctx, ahDispersionTimeout)
			defer cancel()
			out, err := collectAhItemPriceDispersion(c, deps.QueryDB, time.Now(), topN, minListings, sortKey, a.HouseID, a.MinQuality)
			if err != nil {
				return map[string]any{"error": err.Error()}
			}
			return out
		},
	})
}

// collectAhItemPriceDispersion runs the census + display queries on one conn (so
// the single USE applies to both) and folds the per-item leaderboard. Split out so
// tests can drive it with sqlmock and a fixed `now`. The SQL ORDER BY is the source
// of truth for row order — the handler does NOT re-sort.
func collectAhItemPriceDispersion(ctx context.Context, db *sql.DB, now time.Time, topN, minListings int, sortKey string, houseID, minQuality *int) (map[string]any, error) {
	sortCol := ahDispersionSortColumns[sortKey] // validated in the handler; always present

	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire conn: %w", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "USE `acore_characters`"); err != nil {
		return nil, fmt.Errorf("USE acore_characters: %w", err)
	}

	// Base WHERE is ALWAYS buyoutprice>0 (the LOAD-BEARING scope: auction-only rows
	// have no buyout, so they would poison MIN as 0 and make spreadPct infinite/
	// garbage — and the division by MIN in the spread sort would blow up). houseId /
	// minQuality are appended with AND, applied to BOTH the census and the display
	// query so the honest totals scope to exactly what the leaderboard shows.
	conds := []string{"ah.buyoutprice > 0"}
	var baseArgs []any
	if houseID != nil {
		conds = append(conds, "ah.houseid = ?")
		baseArgs = append(baseArgs, *houseID)
	}
	if minQuality != nil {
		conds = append(conds, "it.Quality >= ?")
		baseArgs = append(baseArgs, *minQuality)
	}
	where := " WHERE " + strings.Join(conds, " AND ")

	// Census: one row of realm-wide denominators, no GROUP BY / no LIMIT / no minListings.
	var distinctItems, totalPricedListings int
	var marketMin, marketMax int64
	if err := conn.QueryRowContext(ctx, ahDispersionCensusSelect+where, baseArgs...).
		Scan(&distinctItems, &totalPricedListings, &marketMin, &marketMax); err != nil {
		return nil, fmt.Errorf("dispersion census query: %w", err)
	}

	// Display: per-item leaderboard. minListings is a HAVING floor on the listing
	// count; topN caps the returned rows. Bind order: WHERE args, then minListings,
	// then topN.
	displayQuery := ahDispersionDisplaySelect + where +
		" GROUP BY it.entry, it.name, it.Quality, it.class" +
		" HAVING listings >= ?" +
		" ORDER BY " + sortCol + " DESC, it.entry ASC LIMIT ?"
	displayArgs := append(append([]any{}, baseArgs...), minListings, topN)

	rows, err := conn.QueryContext(ctx, displayQuery, displayArgs...)
	if err != nil {
		return nil, fmt.Errorf("dispersion display query: %w", err)
	}
	defer rows.Close()

	items := make([]*ahDispersionItem, 0, topN)
	for rows.Next() {
		var (
			entry                    int64
			name                     string
			quality, class, listings int
			sellers                  int
			minB, maxB, totalB       int64
		)
		if err := rows.Scan(&entry, &name, &quality, &class, &listings, &sellers, &minB, &maxB, &totalB); err != nil {
			return nil, fmt.Errorf("dispersion scan: %w", err)
		}
		// avg is folded in Go from the summed buyout over the listing count (all
		// rows carry a buyout under the WHERE, and listings >= minListings >= 1, so
		// no divide-by-zero) — matches ah_market_top_items rather than scanning a
		// DECIMAL AVG().
		var avg int64
		if listings > 0 {
			avg = totalB / int64(listings)
		}
		items = append(items, &ahDispersionItem{
			ItemEntry:       entry,
			ItemName:        name,
			Quality:         quality,
			QualityName:     itemQualityName(quality),
			Class:           class,
			ClassName:       itemClassName(class),
			Listings:        listings,
			DistinctSellers: sellers,
			MinBuyoutCopper: minB,
			MinBuyoutGold:   formatCopper(minB),
			MaxBuyoutCopper: maxB,
			MaxBuyoutGold:   formatCopper(maxB),
			AvgBuyoutCopper: avg,
			AvgBuyoutGold:   formatCopper(avg),
			// spreadPct = (max-min)/min*100 rounded to one decimal; ahCategoryValuePct
			// guards min<=0 -> 0 (never reached here since buyoutprice>0, but keeps the
			// helper's contract).
			SpreadPct: ahCategoryValuePct(maxB-minB, minB),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("dispersion iter: %w", err)
	}

	out := map[string]any{
		"asOf":           now.UTC().Format(time.RFC3339),
		"topN":           topN,
		"minListings":    minListings,
		"sortBy":         sortKey,
		"displayedItems": len(items),
		"totals": map[string]any{
			"distinctItems":       distinctItems,
			"totalPricedListings": totalPricedListings,
			"marketMinCopper":     marketMin,
			"marketMinGold":       formatCopper(marketMin),
			"marketMaxCopper":     marketMax,
			"marketMaxGold":       formatCopper(marketMax),
			// marketSpreadPct is the realm-wide extremes' spread (marketMax vs
			// marketMin); div-guarded to 0 for an empty market (marketMin 0).
			"marketSpreadPct": ahCategoryValuePct(marketMax-marketMin, marketMin),
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
