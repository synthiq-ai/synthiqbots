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
	ahArbitrageTimeout          = 10 * time.Second
	ahArbitrageDefaultTop       = 15
	ahArbitrageMaxTop           = 100
	ahArbitrageMaxQuality       = 7 // AzerothCore item Quality enum tops out at 7 (Heirloom)
	ahArbitrageDefaultMinHouses = 2 // an item on only 1 house has no cross-house floor to compare
	ahArbitrageMaxHouses        = 3 // Alliance/Horde/Neutral — there are only 3 AuctionHouseIds
)

// ahArbitrageHouseIDs is the fixed AzerothCore AuctionHouseId set (Alliance=2,
// Horde=6, Neutral=7), walked in this order wherever a deterministic
// cheapest/priciest tiebreak is needed (a tie always resolves toward the
// lower-ordered house here, not map-iteration order).
var ahArbitrageHouseIDs = []int{2, 6, 7}

// ahArbitrageSortKeys is the Go-side sortBy allowlist. Unlike a flat per-item
// leaderboard, this tool's headline metric (spreadPct) is a comparison ACROSS
// per-house floors folded in Go, not a single SQL aggregate — so there is no
// clean ORDER BY expression to interpolate (the #349/#369/#374 fold-in-Go idiom
// applies here even more directly than to those siblings). Both keys are
// validated before any query runs.
var ahArbitrageSortKeys = map[string]bool{
	"spread":        true, // default — biggest % gap between cheapest and priciest house floor
	"cheapestFloor": true, // ascending — lowest absolute buy-in first (smallest-capital arbitrage entry)
}

// ahArbitrageQuery folds the per-(house, item) floor in ONE round trip rather
// than the three separate per-house queries the backlog note sketched:
// GROUP BY ah.houseid, it.entry is bounded by (item variety * 3 houses), the
// same "bounded by item variety, no scanCap needed" reasoning ah_floor_saturation
// established for its single-house floor map, so a single grouped query is
// simpler and cheaper than three round trips producing the same fold target.
// item_template is joined in this query (not a separate lookup) since every
// folded row needs it for the minQuality filter AND the name/quality/class
// display fields.
const ahArbitrageQuery = "SELECT ah.houseid, it.entry, it.name, it.Quality, it.class, MIN(ah.buyoutprice) AS floor " +
	"FROM `auctionhouse` ah " +
	"JOIN `item_instance` ii ON ah.itemguid = ii.guid " +
	"JOIN `acore_world`.`item_template` it ON ii.itemEntry = it.entry " +
	"WHERE ah.buyoutprice > 0"

// ahArbitrageTailQuery closes the query after any minQuality filter is appended.
const ahArbitrageTailQuery = " GROUP BY ah.houseid, it.entry, it.name, it.Quality, it.class"

// ahArbitrageAccum is the per-item fold target while scanning the grouped rows —
// one row per house the item is listed on, keyed by AuctionHouseId.
type ahArbitrageAccum struct {
	ItemName string
	Quality  int
	Class    int
	Floors   map[int]int64
}

// ahArbitrageItem is one item's cross-house floor comparison. Per-house floor
// fields are nullable (omitempty) — not every item lists on every house.
// SpreadPct reuses the ratio-of-a-delta helper ahCategoryValuePct(max-min, min)
// the same way ah_item_price_dispersion established for its own spreadPct, so
// the arbitrage % and that sibling's spread % share one formula and one
// div-by-zero guard.
type ahArbitrageItem struct {
	ItemEntry           int64   `json:"itemEntry"`
	ItemName            string  `json:"itemName"`
	Quality             int     `json:"quality"`
	QualityName         string  `json:"qualityName"`
	Class               int     `json:"class"`
	ClassName           string  `json:"className"`
	AllianceFloorCopper *int64  `json:"allianceFloorCopper,omitempty"`
	HordeFloorCopper    *int64  `json:"hordeFloorCopper,omitempty"`
	NeutralFloorCopper  *int64  `json:"neutralFloorCopper,omitempty"`
	HousesListed        int     `json:"housesListed"`
	CheapestHouseID     int     `json:"cheapestHouseId"`
	CheapestFaction     string  `json:"cheapestFaction"`
	CheapestFloorCopper int64   `json:"cheapestFloorCopper"`
	PriciestHouseID     int     `json:"priciestHouseId"`
	PriciestFaction     string  `json:"priciestFaction"`
	PriciestFloorCopper int64   `json:"priciestFloorCopper"`
	SpreadPct           float64 `json:"spreadPct"`
}

// RegisterAhCrossHouseArbitrageTool registers `ah_cross_house_arbitrage` — a
// read-only cross-house price-arbitrage view of the auction house.
//
// Every existing ah_* tool scopes to ONE house at a time (an optional houseId
// filter) or ignores house entirely. None answers whether the SAME item type
// floats a meaningfully DIFFERENT floor price across Alliance/Horde/Neutral
// right now — a market-inefficiency signal ah_floor_saturation's per-house
// crowding view and ah_item_price_dispersion's single-query spread can't show,
// since neither compares floors ACROSS houses in one call.
func RegisterAhCrossHouseArbitrageTool(reg *Registry, deps DBDeps) {
	reg.Register(Tool{
		Name: "ah_cross_house_arbitrage",
		Description: "Cross-house auction-house price arbitrage, joining acore_characters.auctionhouse -> " +
			"item_instance -> acore_world.item_template over the ops_ro pool. For each item TYPE listed on 2 or " +
			"more of the 3 AzerothCore auction houses (2=Alliance, 6=Horde, 7=Neutral), folds each house's cheapest " +
			"buyout (MIN(buyoutprice) per house) into one row: allianceFloorCopper/hordeFloorCopper/neutralFloorCopper " +
			"(each omitted when the item isn't listed on that house), housesListed, cheapestHouseId+cheapestFaction+" +
			"cheapestFloorCopper, priciestHouseId+priciestFaction+priciestFloorCopper, and spreadPct " +
			"((priciest-cheapest)/cheapest*100). Surfaces same-item price disagreement ACROSS houses — buy on the " +
			"cheap house, notionally resell on the expensive one — which neither ah_floor_saturation (per-house " +
			"floor crowding, one house per call) nor ah_item_price_dispersion (per-item seller spread, house-agnostic) " +
			"shows. Also returns itemEntry, itemName, quality + qualityName (0 Poor .. 7 Heirloom, unknown as " +
			"quality-<q>) and class + className (unknown as class-<c>). Honest realm-wide totals " +
			"(distinctItemsListed, distinctItemsOnMultipleHouses, distinctItemsOnAllHouses, matchedItems) are a " +
			"separate fold independent of topN. Args: topN (default 15, max 100), sortBy (spread [default, by " +
			"spreadPct desc] | cheapestFloor [by cheapestFloorCopper ASC — the lowest-capital entry first]; an " +
			"unknown key is rejected), minHouses (default 2 — only items listed on at least this many houses; " +
			"clamped to the 2-3 range, since 3 is the whole market), minQuality (optional 0-7 — only items at or " +
			"above this rarity; out-of-range is rejected, not clamped). itemEntry-asc tiebreak within a sort key. " +
			"Read-only, 10 s timeout.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{
			"topN":{"type":"integer","description":"Max items returned (default 15, max 100)"},
			"sortBy":{"type":"string","enum":["spread","cheapestFloor"],"description":"spread=spreadPct desc (default), cheapestFloor=cheapestFloorCopper asc (cheapest entry first)"},
			"minHouses":{"type":"integer","description":"Only items listed on at least this many of the 3 houses (default 2, clamped to 2-3)"},
			"minQuality":{"type":"integer","description":"Only items at or above this rarity (0=Poor .. 7=Heirloom); omit for all qualities"}
		}}`),
		Annotations: AnnRead(),
		Handler: func(ctx context.Context, raw json.RawMessage, _ string) any {
			var a struct {
				TopN       int    `json:"topN"`
				SortBy     string `json:"sortBy"`
				MinHouses  *int   `json:"minHouses"`
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
				topN = ahArbitrageDefaultTop
			}
			if topN > ahArbitrageMaxTop {
				topN = ahArbitrageMaxTop
			}
			sortKey := a.SortBy
			if sortKey == "" {
				sortKey = "spread"
			}
			if !ahArbitrageSortKeys[sortKey] {
				return map[string]any{"error": fmt.Sprintf("invalid sortBy: %q (valid: spread, cheapestFloor)", a.SortBy)}
			}
			minHouses := ahArbitrageDefaultMinHouses
			if a.MinHouses != nil {
				minHouses = *a.MinHouses
				if minHouses < ahArbitrageDefaultMinHouses {
					minHouses = ahArbitrageDefaultMinHouses
				}
				if minHouses > ahArbitrageMaxHouses {
					minHouses = ahArbitrageMaxHouses
				}
			}
			if a.MinQuality != nil && (*a.MinQuality < 0 || *a.MinQuality > ahArbitrageMaxQuality) {
				return map[string]any{"error": fmt.Sprintf("minQuality out of range: %d (valid 0-%d)", *a.MinQuality, ahArbitrageMaxQuality)}
			}
			c, cancel := context.WithTimeout(ctx, ahArbitrageTimeout)
			defer cancel()
			out, err := collectAhCrossHouseArbitrage(c, deps.QueryDB, time.Now(), topN, minHouses, sortKey, a.MinQuality)
			if err != nil {
				return map[string]any{"error": err.Error()}
			}
			return out
		},
	})
}

// collectAhCrossHouseArbitrage runs the single grouped floor query, folds it
// per item, and Go-sorts the matched subset. Split out so tests can drive it
// with sqlmock and a fixed `now`.
func collectAhCrossHouseArbitrage(ctx context.Context, db *sql.DB, now time.Time, topN, minHouses int, sortKey string, minQuality *int) (map[string]any, error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire conn: %w", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "USE `acore_characters`"); err != nil {
		return nil, fmt.Errorf("USE acore_characters: %w", err)
	}

	query := ahArbitrageQuery
	var args []any
	if minQuality != nil {
		query += " AND it.Quality >= ?"
		args = append(args, *minQuality)
	}
	query += ahArbitrageTailQuery

	rows, err := conn.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("arbitrage floor query: %w", err)
	}
	defer rows.Close()

	accums := map[int64]*ahArbitrageAccum{}
	for rows.Next() {
		var (
			houseID        int
			entry          int64
			name           string
			quality, class int
			floor          int64
		)
		if err := rows.Scan(&houseID, &entry, &name, &quality, &class, &floor); err != nil {
			return nil, fmt.Errorf("arbitrage scan: %w", err)
		}
		acc, ok := accums[entry]
		if !ok {
			acc = &ahArbitrageAccum{ItemName: name, Quality: quality, Class: class, Floors: map[int]int64{}}
			accums[entry] = acc
		}
		acc.Floors[houseID] = floor
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("arbitrage iter: %w", err)
	}

	// Honest realm-wide totals, folded over EVERY scanned item independent of
	// minHouses/topN — the floor query has no LIMIT so nothing is truncated
	// out from under these counts.
	distinctItemsListed := len(accums)
	distinctItemsOnMultipleHouses := 0
	distinctItemsOnAllHouses := 0
	for _, acc := range accums {
		if len(acc.Floors) >= 2 {
			distinctItemsOnMultipleHouses++
		}
		if len(acc.Floors) >= ahArbitrageMaxHouses {
			distinctItemsOnAllHouses++
		}
	}

	items := make([]*ahArbitrageItem, 0, len(accums))
	for entry, acc := range accums {
		if len(acc.Floors) < minHouses {
			continue
		}
		item := &ahArbitrageItem{
			ItemEntry:    entry,
			ItemName:     acc.ItemName,
			Quality:      acc.Quality,
			QualityName:  itemQualityName(acc.Quality),
			Class:        acc.Class,
			ClassName:    itemClassName(acc.Class),
			HousesListed: len(acc.Floors),
		}
		if v, ok := acc.Floors[2]; ok {
			item.AllianceFloorCopper = &v
		}
		if v, ok := acc.Floors[6]; ok {
			item.HordeFloorCopper = &v
		}
		if v, ok := acc.Floors[7]; ok {
			item.NeutralFloorCopper = &v
		}
		// Deterministic cheapest/priciest pick: walk the fixed house order so a
		// tie always resolves the same way regardless of Go's randomized map
		// iteration order over acc.Floors.
		cheapestHouse, priciestHouse := -1, -1
		var cheapest, priciest int64
		for _, h := range ahArbitrageHouseIDs {
			v, ok := acc.Floors[h]
			if !ok {
				continue
			}
			if cheapestHouse == -1 || v < cheapest {
				cheapest, cheapestHouse = v, h
			}
			if priciestHouse == -1 || v > priciest {
				priciest, priciestHouse = v, h
			}
		}
		item.CheapestHouseID = cheapestHouse
		item.CheapestFaction = ahHouseFaction(cheapestHouse)
		item.CheapestFloorCopper = cheapest
		item.PriciestHouseID = priciestHouse
		item.PriciestFaction = ahHouseFaction(priciestHouse)
		item.PriciestFloorCopper = priciest
		item.SpreadPct = ahCategoryValuePct(priciest-cheapest, cheapest)
		items = append(items, item)
	}

	switch sortKey {
	case "cheapestFloor":
		sort.Slice(items, func(i, j int) bool {
			if items[i].CheapestFloorCopper != items[j].CheapestFloorCopper {
				return items[i].CheapestFloorCopper < items[j].CheapestFloorCopper
			}
			return items[i].ItemEntry < items[j].ItemEntry
		})
	default: // "spread"
		sort.Slice(items, func(i, j int) bool {
			if items[i].SpreadPct != items[j].SpreadPct {
				return items[i].SpreadPct > items[j].SpreadPct
			}
			return items[i].ItemEntry < items[j].ItemEntry
		})
	}

	matchedItems := len(items)
	if len(items) > topN {
		items = items[:topN]
	}

	out := map[string]any{
		"asOf":         now.UTC().Format(time.RFC3339),
		"topN":         topN,
		"minHouses":    minHouses,
		"sortBy":       sortKey,
		"matchedItems": matchedItems,
		"totals": map[string]any{
			"distinctItemsListed":           distinctItemsListed,
			"distinctItemsOnMultipleHouses": distinctItemsOnMultipleHouses,
			"distinctItemsOnAllHouses":      distinctItemsOnAllHouses,
		},
		"items": items,
	}
	if minQuality != nil {
		out["minQuality"] = *minQuality
	}
	return out, nil
}
