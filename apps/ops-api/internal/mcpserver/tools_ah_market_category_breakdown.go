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
	ahCategoryTimeout    = 10 * time.Second
	ahCategoryDefaultTop = 20 // >= the 17 ItemClass values, so the default shows the whole composition
	ahCategoryMaxTop     = 50
	ahCategoryMaxQuality = 7    // AzerothCore item Quality enum tops out at 7 (Heirloom)
	ahCategoryScanCap    = 1000 // GROUP BY it.class is bounded by class cardinality (~17); generous cap + truncation detect
)

// ahCategoryBaseQuery is the same cross-DB join ah_market_top_items uses
// (auctionhouse -> item_instance -> acore_world.item_template), but grouped one
// level COARSER — by item CATEGORY (item_template.class) rather than per item
// (it.entry). It answers the composition question the per-item tool doesn't:
// "what KINDS of things dominate the AH — trade goods vs gear vs consumables —
// and where is the gold concentrated by category?".
//
// Columns anchored on the canonical AzerothCore item_template loader
// (ObjectMgr.cpp: "SELECT entry, class, subclass, ... name, ... Quality, ... FROM
// item_template"): class (tinyint, the ItemClass enum 0 Consumable .. 16 Glyph),
// entry (PK), Quality (rarity 0-7). The GROUP BY is on the single non-aggregate
// column it.class, so the query is valid under ONLY_FULL_GROUP_BY on both MySQL 8
// and MariaDB without spelling out a functional-dependency list (unlike
// ah_market_top_items, whose name+Quality columns forced the explicit group).
// COUNT(DISTINCT it.entry) is the count of distinct item TYPES in the category
// (an item entry belongs to exactly one class, so the per-class distinct counts
// never overlap and sum to the market-wide distinct total). MIN(NULLIF(buyoutprice,0))
// is the category floor ignoring auction-only (buyoutprice 0) rows; NULL when
// every listing in the category is bid-only.
const ahCategoryBaseQuery = "SELECT it.class, " +
	"COUNT(*) AS listings, " +
	"COUNT(DISTINCT it.entry) AS distinctItems, " +
	"SUM(CASE WHEN ah.buyoutprice > 0 THEN 1 ELSE 0 END) AS withBuyout, " +
	"SUM(ii.count) AS totalQuantity, " +
	"SUM(ah.buyoutprice) AS totalBuyout, " +
	"MIN(NULLIF(ah.buyoutprice, 0)) AS minBuyout " +
	"FROM `auctionhouse` ah " +
	"JOIN `item_instance` ii ON ah.itemguid = ii.guid " +
	"JOIN `acore_world`.`item_template` it ON ii.itemEntry = it.entry"

// ahCategoryTailQuery closes the query after any WHERE filters are appended. The
// LIMIT is bound to scanCap+1 (NOT topN) so EVERY category is folded before the
// market-wide pct denominators are computed — topN only trims the DISPLAYED list
// afterward, in Go, the way ah_market_summary slices topSellers post-scan.
const ahCategoryTailQuery = " GROUP BY it.class ORDER BY listings DESC, it.class ASC LIMIT ?"

// itemClassName maps the item_template.class tinyint to the WotLK ItemClass enum
// name. Anchored on AzerothCore's enum ItemClass (ItemTemplate.h: 0 Consumable ..
// 16 Glyph, MAX_ITEM_CLASS 17). An unrecognized value surfaces as "class-<c>"
// rather than dropping the row — the same never-vanish philosophy itemQualityName
// uses for an unknown quality and ahHouseFaction for an unknown house id.
func itemClassName(c int) string {
	switch c {
	case 0:
		return "Consumable"
	case 1:
		return "Container"
	case 2:
		return "Weapon"
	case 3:
		return "Gem"
	case 4:
		return "Armor"
	case 5:
		return "Reagent"
	case 6:
		return "Projectile"
	case 7:
		return "Trade Goods"
	case 8:
		return "Generic"
	case 9:
		return "Recipe"
	case 10:
		return "Money"
	case 11:
		return "Quiver"
	case 12:
		return "Quest"
	case 13:
		return "Key"
	case 14:
		return "Permanent"
	case 15:
		return "Miscellaneous"
	case 16:
		return "Glyph"
	default:
		return fmt.Sprintf("class-%d", c)
	}
}

// ahCategoryValuePct returns n as a percentage of total rounded to one decimal,
// with a total<=0 div-by-zero guard. The int64 sibling of ah_market_summary's
// ahPct (which is int-typed for listing COUNTS): copper value sums are int64 and
// can exceed an int on a 32-bit build, so converting to ahPct's int would be
// lossy. A future consolidation could fold both into one generic numeric helper
// once a third caller appears.
func ahCategoryValuePct(n, total int64) float64 {
	if total <= 0 {
		return 0
	}
	return math.Round(float64(n)/float64(total)*1000) / 10
}

// ahCategory is one grouped row of the breakdown — a single item category. The
// pct fields are the category's share of the WHOLE market (computed against the
// pre-slice grand totals), so a category still sliced out of the topN display
// list is counted in the denominators. Copper sums are the machine-readable
// int64 facet; the *Gold strings render the same values as WoW money for
// at-a-glance reading (formatCopper, shared with ah_market_summary).
type ahCategory struct {
	Class             int     `json:"class"`
	ClassName         string  `json:"className"`
	Listings          int     `json:"listings"`
	ListingsPct       float64 `json:"listingsPct"`
	DistinctItems     int     `json:"distinctItems"`
	WithBuyout        int     `json:"withBuyout"`
	TotalQuantity     int64   `json:"totalQuantity"`
	TotalBuyoutCopper int64   `json:"totalBuyoutCopper"`
	TotalBuyoutGold   string  `json:"totalBuyoutGold"`
	TotalBuyoutPct    float64 `json:"totalBuyoutPct"`
	AvgBuyoutCopper   int64   `json:"avgBuyoutCopper"`
	AvgBuyoutGold     string  `json:"avgBuyoutGold"`
	MinBuyoutCopper   int64   `json:"minBuyoutCopper"`
	MinBuyoutGold     string  `json:"minBuyoutGold"`
}

// RegisterAhMarketCategoryBreakdownTool registers `ah_market_category_breakdown`
// — a read-only composition view of the auction house grouped by item category
// (item_template.class).
//
// This is the third lens on the AH, distinct from its two siblings:
// ah_market_summary folds the market into faction-level TOTALS (how big / the
// split / value locked); ah_market_top_items ranks individual item TYPES; this
// rolls every listing up to its CATEGORY (Trade Goods / Armor / Weapon /
// Consumable / Gem / ...) so an operator can answer "is this a
// crafting-materials economy or a gear economy — what share of listings and of
// gold sits in each category?" without a hand-written GROUP BY it.class. Reuses
// ah_market_top_items' merged cross-DB join + ah_market_summary's formatCopper /
// ahHouseFaction / ahPct verbatim.
func RegisterAhMarketCategoryBreakdownTool(reg *Registry, deps DBDeps) {
	reg.Register(Tool{
		Name: "ah_market_category_breakdown",
		Description: "Auction-house composition grouped by item CATEGORY (item_template.class), joining " +
			"acore_characters.auctionhouse -> item_instance -> acore_world.item_template over the ops_ro pool " +
			"(the same cross-DB join as ah_market_top_items, rolled up one level coarser — per category, not per " +
			"item). Each category carries class + className (0 Consumable, 2 Weapon, 4 Armor, 7 Trade Goods, " +
			"16 Glyph, ... the AzerothCore ItemClass enum; an unknown value surfaces as class-<c>), listings + " +
			"listingsPct (share of all market listings), distinctItems (distinct item types in the category), " +
			"withBuyout, totalQuantity (summed stack sizes), and the total/avg/min buyout in copper plus human " +
			"gold strings with totalBuyoutPct (share of all market buyout value; avg is over withBuyout listings, " +
			"min is the floor ignoring auction-only rows). All pcts are shares of the WHOLE market even when topN " +
			"trims the list. Answers 'is this a crafting-materials or a gear economy — where do listings and gold " +
			"concentrate by category?'. Args: topN (default 20, max 50 — there are ~17 classes so the default " +
			"shows the full composition), houseId (optional — narrow to one auction house: 2=Alliance, 6=Horde, " +
			"7=Neutral, the AzerothCore AuctionHouseId; omit for the whole market), minQuality (optional 0-7 — " +
			"only items at or above this rarity; out-of-range is rejected, not clamped). Sorted listings desc, " +
			"class asc tiebreak. The category-composition companion to ah_market_summary (market-wide totals) and " +
			"ah_market_top_items (per-item ranking). Read-only, 10 s timeout.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{
			"topN":{"type":"integer","description":"Max categories returned, ranked by listing count (default 20, max 50; ~17 item classes exist so the default shows the whole composition)"},
			"houseId":{"type":"integer","description":"Narrow to one auction house (2=Alliance, 6=Horde, 7=Neutral); omit for the whole market"},
			"minQuality":{"type":"integer","description":"Only items at or above this rarity (0=Poor .. 7=Heirloom); omit for all qualities"}
		}}`),
		Annotations: AnnRead(),
		Handler: func(ctx context.Context, raw json.RawMessage, _ string) any {
			var a struct {
				TopN       int  `json:"topN"`
				HouseID    *int `json:"houseId"`
				MinQuality *int `json:"minQuality"`
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
				topN = ahCategoryDefaultTop
			}
			if topN > ahCategoryMaxTop {
				topN = ahCategoryMaxTop
			}
			// minQuality is rejected (not silently clamped) when outside the 0-7
			// enum range — a typo'd 99 would otherwise return an empty list that
			// reads as "no items" rather than "bad filter" (mirrors ah_market_top_items).
			if a.MinQuality != nil && (*a.MinQuality < 0 || *a.MinQuality > ahCategoryMaxQuality) {
				return map[string]any{"error": fmt.Sprintf("minQuality out of range: %d (valid 0-%d)", *a.MinQuality, ahCategoryMaxQuality)}
			}
			c, cancel := context.WithTimeout(ctx, ahCategoryTimeout)
			defer cancel()
			out, err := collectAhCategoryBreakdown(c, deps.QueryDB, time.Now(), topN, a.HouseID, a.MinQuality)
			if err != nil {
				return map[string]any{"error": err.Error()}
			}
			return out
		},
	})
}

// collectAhCategoryBreakdown builds the (optionally filtered) join query, folds
// EVERY grouped category to compute the market-wide grand totals, fills each
// category's market-relative pct, then slices the display list to topN. Split
// out so tests can drive it with sqlmock and a fixed `now`. The SQL ORDER BY is
// the source of truth for row order — the handler does NOT re-sort (matching
// ah_market_top_items); the SQL "listings DESC, it.class ASC" is already a total
// order, so the topN slice keeps the biggest categories deterministically.
func collectAhCategoryBreakdown(ctx context.Context, db *sql.DB, now time.Time, topN int, houseID, minQuality *int) (map[string]any, error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire conn: %w", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "USE `acore_characters`"); err != nil {
		return nil, fmt.Errorf("USE acore_characters: %w", err)
	}

	query := ahCategoryBaseQuery
	var args []any
	var conds []string
	if houseID != nil {
		conds = append(conds, "ah.houseid = ?")
		args = append(args, *houseID)
	}
	if minQuality != nil {
		conds = append(conds, "it.Quality >= ?")
		args = append(args, *minQuality)
	}
	if len(conds) > 0 {
		query += " WHERE " + strings.Join(conds, " AND ")
	}
	query += ahCategoryTailQuery
	args = append(args, ahCategoryScanCap+1)

	rows, err := conn.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("category query: %w", err)
	}
	defer rows.Close()

	cats := make([]*ahCategory, 0, topN)
	var grandListings, grandWithBuyout, grandDistinct int
	var grandQty, grandBuyout int64
	scanned := 0
	truncated := false
	for rows.Next() {
		if scanned >= ahCategoryScanCap {
			truncated = true
			break
		}
		var (
			class, listings, distinctItems, withBuyout int
			totalQty, totalBuyout                      int64
			minBuyout                                  sql.NullInt64
		)
		if err := rows.Scan(&class, &listings, &distinctItems, &withBuyout, &totalQty, &totalBuyout, &minBuyout); err != nil {
			return nil, fmt.Errorf("category scan: %w", err)
		}
		scanned++

		// grand totals fold across ALL categories (incl. any past the topN slice)
		// so the per-category pcts are shares of the whole market.
		grandListings += listings
		grandDistinct += distinctItems
		grandWithBuyout += withBuyout
		grandQty += totalQty
		grandBuyout += totalBuyout

		// avg is over the listings that actually carry a buyout; auction-only rows
		// (buyoutprice 0) are excluded so the average isn't dragged down.
		var avg int64
		if withBuyout > 0 {
			avg = totalBuyout / int64(withBuyout)
		}
		var minB int64
		if minBuyout.Valid {
			minB = minBuyout.Int64
		}
		cats = append(cats, &ahCategory{
			Class:             class,
			ClassName:         itemClassName(class),
			Listings:          listings,
			DistinctItems:     distinctItems,
			WithBuyout:        withBuyout,
			TotalQuantity:     totalQty,
			TotalBuyoutCopper: totalBuyout,
			TotalBuyoutGold:   formatCopper(totalBuyout),
			AvgBuyoutCopper:   avg,
			AvgBuyoutGold:     formatCopper(avg),
			MinBuyoutCopper:   minB,
			MinBuyoutGold:     formatCopper(minB),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("category iter: %w", err)
	}

	// Second pass: fill the market-relative pcts now that the grand totals are
	// known (the denominator needs every category, including any beyond topN).
	for _, cat := range cats {
		cat.ListingsPct = ahPct(cat.Listings, grandListings)
		cat.TotalBuyoutPct = ahCategoryValuePct(cat.TotalBuyoutCopper, grandBuyout)
	}

	distinctCategories := len(cats)
	if len(cats) > topN {
		cats = cats[:topN]
	}

	out := map[string]any{
		"asOf":               now.UTC().Format(time.RFC3339),
		"topN":               topN,
		"distinctCategories": distinctCategories,
		"scanned":            scanned,
		"scanCap":            ahCategoryScanCap,
		"truncated":          truncated,
		"totals": map[string]any{
			"listings":          grandListings,
			"distinctItems":     grandDistinct,
			"withBuyout":        grandWithBuyout,
			"totalQuantity":     grandQty,
			"totalBuyoutCopper": grandBuyout,
			"totalBuyoutGold":   formatCopper(grandBuyout),
		},
		"categories": cats,
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
