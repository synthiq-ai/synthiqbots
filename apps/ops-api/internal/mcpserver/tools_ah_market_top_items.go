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
	ahTopItemsTimeout    = 10 * time.Second
	ahTopItemsDefaultTop = 15
	ahTopItemsMaxTop     = 100
	ahTopItemsMaxQuality = 7 // AzerothCore item Quality enum tops out at 7 (Heirloom)
)

// ahTopItemsBaseQuery is the cross-DB join the ah_market_summary single-table
// scan deliberately left out. acore_characters.auctionhouse.itemguid references
// acore_characters.item_instance.guid; item_instance.itemEntry references
// acore_world.item_template.entry (ops_ro holds SELECT on both characters and
// world). Columns anchored on the canonical AzerothCore schema:
//   - item_instance (data/sql/base/db_characters/item_instance.sql): guid (PK,
//     the auctioned item's instance GUID), itemEntry (the template), count
//     (stack size).
//   - item_template (data/sql/base/db_world/item_template.sql): entry (PK), name,
//     Quality (tinyint, the rarity 0-7; capitalized in the schema).
//
// Aggregation runs SQL-side (GROUP BY) rather than the Go-side fold
// ah_market_summary uses: the join repeats the item name (varchar(255)) on every
// listing row, so folding in Go would transfer the name once per listing across
// the whole market — the GROUP BY collapses it to one row per item before
// transfer. GROUP BY lists all three non-aggregate columns so the query is valid
// under ONLY_FULL_GROUP_BY on both MySQL 8 and MariaDB (entry is the PK, so name
// and Quality are functionally dependent, but spelling the group out stays
// portable across servers that don't detect the dependency through the join
// alias). MIN(NULLIF(buyoutprice,0)) is the floor price ignoring auction-only
// (buyoutprice 0) rows; it is NULL when every listing of the item is bid-only.
const ahTopItemsBaseQuery = "SELECT it.entry, it.name, it.Quality, " +
	"COUNT(*) AS listings, " +
	"SUM(CASE WHEN ah.buyoutprice > 0 THEN 1 ELSE 0 END) AS withBuyout, " +
	"SUM(ii.count) AS totalQuantity, " +
	"SUM(ah.buyoutprice) AS totalBuyout, " +
	"MIN(NULLIF(ah.buyoutprice, 0)) AS minBuyout " +
	"FROM `auctionhouse` ah " +
	"JOIN `item_instance` ii ON ah.itemguid = ii.guid " +
	"JOIN `acore_world`.`item_template` it ON ii.itemEntry = it.entry"

// ahTopItemsTailQuery closes the query after any WHERE filters are appended.
const ahTopItemsTailQuery = " GROUP BY it.entry, it.name, it.Quality ORDER BY listings DESC, it.entry ASC LIMIT ?"

// itemQualityName maps the item_template.Quality tinyint to the WotLK rarity
// name. Anchored on the ItemQualities enum (0 Poor .. 7 Heirloom). An
// unrecognized value surfaces as "quality-<q>" rather than dropping the row —
// the same never-vanish philosophy ahHouseFaction uses for an unknown house id.
func itemQualityName(q int) string {
	switch q {
	case 0:
		return "Poor"
	case 1:
		return "Common"
	case 2:
		return "Uncommon"
	case 3:
		return "Rare"
	case 4:
		return "Epic"
	case 5:
		return "Legendary"
	case 6:
		return "Artifact"
	case 7:
		return "Heirloom"
	default:
		return fmt.Sprintf("quality-%d", q)
	}
}

// ahTopItem is one grouped row of the top-items list. Copper sums are the
// machine-readable int64 facet; the *Gold strings render the same values as WoW
// money for at-a-glance reading (formatCopper, shared with ah_market_summary).
type ahTopItem struct {
	ItemEntry         int64  `json:"itemEntry"`
	ItemName          string `json:"itemName"`
	Quality           int    `json:"quality"`
	QualityName       string `json:"qualityName"`
	Listings          int    `json:"listings"`
	WithBuyout        int    `json:"withBuyout"`
	TotalQuantity     int64  `json:"totalQuantity"`
	TotalBuyoutCopper int64  `json:"totalBuyoutCopper"`
	TotalBuyoutGold   string `json:"totalBuyoutGold"`
	AvgBuyoutCopper   int64  `json:"avgBuyoutCopper"`
	AvgBuyoutGold     string `json:"avgBuyoutGold"`
	MinBuyoutCopper   int64  `json:"minBuyoutCopper"`
	MinBuyoutGold     string `json:"minBuyoutGold"`
}

// RegisterAhMarketTopItemsTool registers `ah_market_top_items` — a read-only,
// item-level breakdown of the auction house ranked by active-listing count.
//
// This is the item-breakdown companion to ah_market_summary (which folds the
// whole market into faction-level totals from a single auctionhouse scan).
// ah_market_summary deliberately stayed single-table; this tool does the
// cross-DB join its doc comment reserved — auctionhouse -> item_instance ->
// acore_world.item_template — so an operator can answer "which item TYPES
// dominate the AH / where is the gold concentrated / what's the floor price on
// the most-listed items" without hand-writing a three-table GROUP BY.
func RegisterAhMarketTopItemsTool(reg *Registry, deps DBDeps) {
	reg.Register(Tool{
		Name: "ah_market_top_items",
		Description: "Top-N auction-house items ranked by active-listing count, joining " +
			"acore_characters.auctionhouse -> item_instance -> acore_world.item_template over the ops_ro pool " +
			"(the cross-DB join ah_market_summary deliberately skips). Each item carries itemEntry, itemName, " +
			"quality + qualityName (0 Poor .. 7 Heirloom — an unknown value surfaces as quality-<q>), listings, " +
			"withBuyout (listings carrying a buyout price), totalQuantity (summed stack sizes), and the " +
			"total/avg/min buyout in copper plus human gold strings (avg is over withBuyout listings; min is the " +
			"floor price ignoring auction-only rows). Args: topN (default 15, max 100), houseId (optional — " +
			"narrow to one auction house: 2=Alliance, 6=Horde, 7=Neutral, the AzerothCore AuctionHouseId; omit " +
			"for the whole market), minQuality (optional 0-7 — only items at or above this rarity; out-of-range " +
			"is rejected, not clamped). Sorted listings desc, itemEntry asc tiebreak. The item-breakdown " +
			"companion to ah_market_summary (market-wide totals). Read-only, 10 s timeout.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{
			"topN":{"type":"integer","description":"Max items returned, ranked by listing count (default 15, max 100)"},
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
				topN = ahTopItemsDefaultTop
			}
			if topN > ahTopItemsMaxTop {
				topN = ahTopItemsMaxTop
			}
			// minQuality is rejected (not silently clamped) when outside the 0-7
			// enum range — a typo'd 99 would otherwise return an empty list that
			// reads as "no items" rather than "bad filter".
			if a.MinQuality != nil && (*a.MinQuality < 0 || *a.MinQuality > ahTopItemsMaxQuality) {
				return map[string]any{"error": fmt.Sprintf("minQuality out of range: %d (valid 0-%d)", *a.MinQuality, ahTopItemsMaxQuality)}
			}
			c, cancel := context.WithTimeout(ctx, ahTopItemsTimeout)
			defer cancel()
			out, err := collectAhTopItems(c, deps.QueryDB, time.Now(), topN, a.HouseID, a.MinQuality)
			if err != nil {
				return map[string]any{"error": err.Error()}
			}
			return out
		},
	})
}

// collectAhTopItems builds the (optionally filtered) join query and folds the
// grouped rows into the items slice. Split out so tests can drive it with
// sqlmock and a fixed `now`. The SQL ORDER BY is the source of truth for row
// order — the handler does NOT re-sort (matching wow_failed_logins_top).
func collectAhTopItems(ctx context.Context, db *sql.DB, now time.Time, topN int, houseID, minQuality *int) (map[string]any, error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire conn: %w", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "USE `acore_characters`"); err != nil {
		return nil, fmt.Errorf("USE acore_characters: %w", err)
	}

	query := ahTopItemsBaseQuery
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
	query += ahTopItemsTailQuery
	args = append(args, topN)

	rows, err := conn.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("top-items query: %w", err)
	}
	defer rows.Close()

	items := make([]*ahTopItem, 0, topN)
	for rows.Next() {
		var (
			entry, totalQty, totalBuyout  int64
			listings, withBuyout, quality int
			name                          string
			minBuyout                     sql.NullInt64
		)
		if err := rows.Scan(&entry, &name, &quality, &listings, &withBuyout, &totalQty, &totalBuyout, &minBuyout); err != nil {
			return nil, fmt.Errorf("top-items scan: %w", err)
		}
		// avg is over the listings that actually carry a buyout; auction-only
		// rows (buyoutprice 0) are excluded so the average isn't dragged down.
		var avg int64
		if withBuyout > 0 {
			avg = totalBuyout / int64(withBuyout)
		}
		var minB int64
		if minBuyout.Valid {
			minB = minBuyout.Int64
		}
		items = append(items, &ahTopItem{
			ItemEntry:         entry,
			ItemName:          name,
			Quality:           quality,
			QualityName:       itemQualityName(quality),
			Listings:          listings,
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
		return nil, fmt.Errorf("top-items iter: %w", err)
	}

	out := map[string]any{
		"asOf":          now.UTC().Format(time.RFC3339),
		"topN":          topN,
		"distinctItems": len(items),
		"items":         items,
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
