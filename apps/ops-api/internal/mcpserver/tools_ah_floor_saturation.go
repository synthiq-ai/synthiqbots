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
	ahFloorSaturationTimeout           = 10 * time.Second
	ahFloorSaturationScanCap           = 200000 // hard ceiling on auctionhouse listing rows folded into one snapshot
	ahFloorSaturationDefaultTop        = 15
	ahFloorSaturationMaxTop            = 100
	ahFloorSaturationMaxQuality        = 7 // AzerothCore item Quality enum tops out at 7 (Heirloom)
	ahFloorSaturationDefaultMinSellers = 2 // a floor held by ONE seller is not a price war
)

// ahFloorSaturationSortKeys is the Go-side sortBy allowlist. Sorting happens in
// topNAhFloorSaturation (not an interpolated SQL ORDER BY) because the floor
// comparison already lives in Go (the derived-table equality join would make a
// SQL-side aggregate sort awkward) — the same choice ah_market_top_sellers /
// ah_undercutters make for their fold-in-Go leaderboards.
var ahFloorSaturationSortKeys = map[string]bool{
	"floorSellers":  true,
	"floorListings": true,
	"saturation":    true,
}

// ahFloorSaturationFloorMapQuery computes each item's buyout floor: the
// MIN(buyoutprice) across all buyout-priced listings for that item, joined
// through item_instance (auctionhouse carries no itemEntry directly). Bounded by
// item VARIETY via GROUP BY (no LIMIT needed) — item variety is orders of
// magnitude smaller than raw listing rows, unlike the listing scan below.
// houseId (when set) scopes the floor per-house, since the same item can float a
// different cheapest buyout on each auction house; minQuality is deliberately
// NOT applied here — quality is a static per-item attribute that can't change
// which rows share an itemEntry, so it only needs to gate which items the
// listing scan folds.
const ahFloorSaturationFloorMapQuery = "SELECT ii.itemEntry, MIN(ah.buyoutprice) AS floor " +
	"FROM `auctionhouse` ah JOIN `item_instance` ii ON ah.itemguid = ii.guid " +
	"WHERE ah.buyoutprice > 0"

// ahFloorSaturationScanQuery is the per-listing fold source: every buyout-priced
// row joined through item_instance -> acore_world.item_template for display
// name/quality/class — an item-grain leaderboard needs names, unlike the
// seller-grain ah_undercutters which drops this join entirely. minQuality (when
// set) narrows which items are folded at all; houseId scopes both this and the
// floor map so the floor comparison is always apples-to-apples per house. LIMIT
// is bound to scanCap+1 so truncation is detectable without a separate COUNT.
const ahFloorSaturationScanQuery = "SELECT ii.itemEntry, ah.itemowner, ah.buyoutprice, it.name, it.Quality, it.class " +
	"FROM `auctionhouse` ah JOIN `item_instance` ii ON ah.itemguid = ii.guid " +
	"JOIN `acore_world`.`item_template` it ON ii.itemEntry = it.entry " +
	"WHERE ah.buyoutprice > 0"

// ahFloorSaturationItem is one item TYPE's floor-crowding snapshot. FloorSellers
// counts DISTINCT itemowner guids whose buyout sits exactly at the item's floor
// — multiple sellers tied at one floor is the genuine "price war" signal, not a
// bug, mirroring the multi-seller tie ah_undercutters proved correct on the
// seller axis. SaturationPct is the share of the item's total listings sitting
// at the floor (floorListings/totalListings*100) — the crowding intensity,
// independent of the item's raw listing volume.
type ahFloorSaturationItem struct {
	ItemEntry     int64   `json:"itemEntry"`
	ItemName      string  `json:"itemName"`
	Quality       int     `json:"quality"`
	QualityName   string  `json:"qualityName"`
	Class         int     `json:"class"`
	ClassName     string  `json:"className"`
	FloorSellers  int     `json:"floorSellers"`
	FloorListings int     `json:"floorListings"`
	TotalListings int     `json:"totalListings"`
	SaturationPct float64 `json:"saturationPct"`

	// floorOwners tracks distinct itemowner guids seen at this item's floor;
	// unexported -> not serialized, collapsed into FloorSellers by finalize().
	floorOwners map[int64]bool
}

// fold adds one scanned listing row to the item's running totals. atFloor is
// true when this listing's buyout equals the item's floor (computed up front
// from the floor map).
func (it *ahFloorSaturationItem) fold(itemOwner int64, atFloor bool) {
	it.TotalListings++
	if atFloor {
		it.FloorListings++
		if it.floorOwners == nil {
			it.floorOwners = map[int64]bool{}
		}
		it.floorOwners[itemOwner] = true
	}
}

// finalize computes FloorSellers + SaturationPct once folding is complete. It is
// called for EVERY scanned item, not just the topN slice, so the honest census
// (distinctItems / totalFlooredListings) reflects the full fold before the
// minSellers/topN cuts are applied.
func (it *ahFloorSaturationItem) finalize() {
	it.FloorSellers = len(it.floorOwners)
	it.SaturationPct = ahCategoryValuePct(int64(it.FloorListings), int64(it.TotalListings))
}

// RegisterAhFloorSaturationTool registers `ah_floor_saturation` — a read-only,
// per-ITEM view of how crowded each item's cheapest buyout (its "floor") is.
//
// ah_undercutters ranks SELLERS by how often their listing holds an item floor
// (the actor-identity axis). ah_item_price_dispersion reports each item's
// MIN/MAX buyout SPREAD but not how many sellers cluster at that minimum — a
// tight spread with 20 sellers all sitting at the floor is a fierce undercut war
// that a spread metric alone scores as "settled". This tool pivots the exact
// floor-tie ah_undercutters proved correct onto the item axis: which items have
// the most SELLERS simultaneously holding the cheapest buyout right now, i.e.
// the hottest undercut battlegrounds.
func RegisterAhFloorSaturationTool(reg *Registry, deps DBDeps) {
	reg.Register(Tool{
		Name: "ah_floor_saturation",
		Description: "Per-item auction-house floor-crowding census, joining acore_characters.auctionhouse -> " +
			"item_instance -> acore_world.item_template over the ops_ro pool. For each item TYPE: floorSellers " +
			"(COUNT DISTINCT itemowner among listings whose buyout equals the item's cheapest buyout — multiple " +
			"sellers tied at one floor all count, a genuine price war not a bug), floorListings (listing count at " +
			"that floor), totalListings, saturationPct (floorListings/totalListings*100 — the crowding intensity), " +
			"plus itemEntry, itemName, quality + qualityName (0 Poor .. 7 Heirloom, unknown surfaces as quality-<q>) " +
			"and class + className (the ItemClass enum, unknown as class-<c>). The item-grain companion to " +
			"ah_undercutters' seller-grain ranking and the crowding signal ah_item_price_dispersion's MIN/MAX spread " +
			"can't show: a tight spread with many sellers at the floor is a fierce undercut war a spread metric alone " +
			"scores as settled. Honest realm-wide totals (distinctItems, totalFlooredListings, totalListingsScanned) " +
			"are folded across every scanned item independent of topN/minSellers. Args: topN (default 15, max 100), " +
			"sortBy (floorSellers [default] | floorListings | saturation; an unknown key is rejected), minSellers " +
			"(default 2, clamped to >=1 — a floor held by one seller is not a war), minQuality (optional 0-7 — only " +
			"items at or above this rarity; out-of-range is rejected, not clamped), houseId (optional 2=Alliance, " +
			"6=Horde, 7=Neutral, the AzerothCore AuctionHouseId; another value is rejected; scopes both the floor " +
			"computation and the listing scan so the floor is compared per-house). Scan capped at 200000 listing " +
			"rows; truncated:true flags an incomplete fold. Sorted by the chosen key desc, itemEntry asc tiebreak. " +
			"Read-only, 10 s timeout.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{
			"topN":{"type":"integer","description":"Max items returned (default 15, max 100)"},
			"sortBy":{"type":"string","enum":["floorSellers","floorListings","saturation"],"description":"floorSellers=distinct sellers at the floor (default), floorListings=listing count at the floor, saturation=saturationPct"},
			"minSellers":{"type":"integer","description":"Only items with at least this many distinct sellers at the floor (default 2, clamped to >=1) — a floor held by one seller is not a war"},
			"minQuality":{"type":"integer","description":"Only items at or above this rarity (0=Poor .. 7=Heirloom); omit for all qualities"},
			"houseId":{"type":"integer","description":"Narrow to one auction house (2=Alliance, 6=Horde, 7=Neutral); scopes the floor computation itself, not just the display; omit for the whole market"}
		}}`),
		Annotations: AnnRead(),
		Handler: func(ctx context.Context, raw json.RawMessage, _ string) any {
			var a struct {
				TopN       int    `json:"topN"`
				SortBy     string `json:"sortBy"`
				MinSellers *int   `json:"minSellers"`
				MinQuality *int   `json:"minQuality"`
				HouseID    *int   `json:"houseId"`
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
				topN = ahFloorSaturationDefaultTop
			}
			if topN > ahFloorSaturationMaxTop {
				topN = ahFloorSaturationMaxTop
			}
			// sortBy resolves to an allowlist key; empty -> the "floorSellers"
			// default. An unknown key is rejected (no query) so a typo can't
			// silently reorder the leaderboard rather than erroring.
			sortKey := a.SortBy
			if sortKey == "" {
				sortKey = "floorSellers"
			}
			if !ahFloorSaturationSortKeys[sortKey] {
				return map[string]any{"error": fmt.Sprintf("invalid sortBy: %q (valid: floorSellers, floorListings, saturation)", a.SortBy)}
			}
			// minSellers is a soft floor, not an enum: default 2, clamp <1 up to 1.
			minSellers := ahFloorSaturationDefaultMinSellers
			if a.MinSellers != nil {
				minSellers = *a.MinSellers
				if minSellers < 1 {
					minSellers = 1
				}
			}
			// minQuality is rejected (not clamped) when outside 0-7 so a typo'd 99
			// reads as "bad filter", not an empty market.
			if a.MinQuality != nil && (*a.MinQuality < 0 || *a.MinQuality > ahFloorSaturationMaxQuality) {
				return map[string]any{"error": fmt.Sprintf("minQuality out of range: %d (valid 0-%d)", *a.MinQuality, ahFloorSaturationMaxQuality)}
			}
			// houseId is rejected (not clamped) when it isn't a real AuctionHouseId
			// so a bad id can't masquerade as an empty house.
			if a.HouseID != nil && *a.HouseID != 2 && *a.HouseID != 6 && *a.HouseID != 7 {
				return map[string]any{"error": fmt.Sprintf("invalid houseId: %d (valid 2=Alliance, 6=Horde, 7=Neutral)", *a.HouseID)}
			}
			c, cancel := context.WithTimeout(ctx, ahFloorSaturationTimeout)
			defer cancel()
			out, err := collectAhFloorSaturation(c, deps.QueryDB, time.Now(), topN, sortKey, minSellers, a.HouseID, a.MinQuality)
			if err != nil {
				return map[string]any{"error": err.Error()}
			}
			return out
		},
	})
}

// collectAhFloorSaturation runs the floor-map query, then the listing scan +
// fold, on one shared connection. Split out so tests can drive it with sqlmock
// and a fixed `now`.
func collectAhFloorSaturation(ctx context.Context, db *sql.DB, now time.Time, topN int, sortKey string, minSellers int, houseID, minQuality *int) (map[string]any, error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire conn: %w", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "USE `acore_characters`"); err != nil {
		return nil, fmt.Errorf("USE acore_characters: %w", err)
	}

	// Stage 1: floor map, scoped by houseId only (see ahFloorSaturationFloorMapQuery doc).
	floorQuery := ahFloorSaturationFloorMapQuery
	var floorArgs []any
	if houseID != nil {
		floorQuery += " AND ah.houseid = ?"
		floorArgs = append(floorArgs, *houseID)
	}
	floorQuery += " GROUP BY ii.itemEntry"

	floorRows, err := conn.QueryContext(ctx, floorQuery, floorArgs...)
	if err != nil {
		return nil, fmt.Errorf("floor-map query: %w", err)
	}
	floorMap := map[int64]int64{}
	for floorRows.Next() {
		var entry, floor int64
		if err := floorRows.Scan(&entry, &floor); err != nil {
			floorRows.Close()
			return nil, fmt.Errorf("floor-map scan: %w", err)
		}
		floorMap[entry] = floor
	}
	if err := floorRows.Err(); err != nil {
		floorRows.Close()
		return nil, fmt.Errorf("floor-map iter: %w", err)
	}
	// Explicit Close (not just defer) so the shared conn is free before Stage 2
	// runs, even though sql.Rows would normally be released by the connection
	// pool automatically once fully drained.
	floorRows.Close()

	// Stage 2: per-listing scan + fold. Shares the houseId clause+bind with
	// Stage 1 (the SQL text, not just the arg, must repeat here); minQuality
	// only applies here since it doesn't affect the floor computation.
	scanQuery := ahFloorSaturationScanQuery
	scanArgs := append([]any{}, floorArgs...)
	if houseID != nil {
		scanQuery += " AND ah.houseid = ?"
	}
	if minQuality != nil {
		scanQuery += " AND it.Quality >= ?"
		scanArgs = append(scanArgs, *minQuality)
	}
	scanQuery += " LIMIT ?"
	scanArgs = append(scanArgs, ahFloorSaturationScanCap+1)

	rows, err := conn.QueryContext(ctx, scanQuery, scanArgs...)
	if err != nil {
		return nil, fmt.Errorf("listing-scan query: %w", err)
	}
	items := map[int64]*ahFloorSaturationItem{}
	scanned := 0
	truncated := false
	for rows.Next() {
		if scanned >= ahFloorSaturationScanCap {
			truncated = true
			break
		}
		var entry, itemOwner, buyout int64
		var name string
		var quality, class int
		if err := rows.Scan(&entry, &itemOwner, &buyout, &name, &quality, &class); err != nil {
			rows.Close()
			return nil, fmt.Errorf("listing-scan scan: %w", err)
		}
		scanned++
		it := items[entry]
		if it == nil {
			it = &ahFloorSaturationItem{
				ItemEntry:   entry,
				ItemName:    name,
				Quality:     quality,
				QualityName: itemQualityName(quality),
				Class:       class,
				ClassName:   itemClassName(class),
			}
			items[entry] = it
		}
		it.fold(itemOwner, buyout == floorMap[entry])
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("listing-scan iter: %w", err)
	}
	rows.Close()

	// Honest census: finalize + fold EVERY scanned item, independent of the
	// minSellers/topN cuts applied next.
	var totalFlooredListings, totalListingsScanned int
	for _, it := range items {
		it.finalize()
		totalFlooredListings += it.FloorListings
		totalListingsScanned += it.TotalListings
	}

	top, matched := topNAhFloorSaturation(items, minSellers, sortKey, topN)

	out := map[string]any{
		"asOf":       now.UTC().Format(time.RFC3339),
		"topN":       topN,
		"sortBy":     sortKey,
		"minSellers": minSellers,
		"scanned":    scanned,
		"scanCap":    ahFloorSaturationScanCap,
		"truncated":  truncated,
		"matched":    matched,
		"totals": map[string]any{
			"distinctItems":        len(items),
			"totalFlooredListings": totalFlooredListings,
			"totalListingsScanned": totalListingsScanned,
		},
		"items": top,
	}
	if houseID != nil {
		out["houseId"] = *houseID
		out["faction"] = ahHouseFaction(*houseID)
	}
	if minQuality != nil {
		out["minQuality"] = *minQuality
	}
	return out, nil
}

// topNAhFloorSaturation filters the folded items to those with >= minSellers
// distinct floor-holders, sorts by the chosen key desc with itemEntry-asc as a
// deterministic tiebreaker (Go's randomized map iteration would otherwise flake
// tests on tied values), slices to n, and returns the slice plus the pre-slice
// matched count. Always a non-nil slice.
func topNAhFloorSaturation(m map[int64]*ahFloorSaturationItem, minSellers int, sortKey string, n int) ([]*ahFloorSaturationItem, int) {
	out := make([]*ahFloorSaturationItem, 0, len(m))
	for _, it := range m {
		if it.FloorSellers >= minSellers {
			out = append(out, it)
		}
	}
	matched := len(out)
	sort.Slice(out, func(i, j int) bool {
		switch sortKey {
		case "floorListings":
			if out[i].FloorListings != out[j].FloorListings {
				return out[i].FloorListings > out[j].FloorListings
			}
		case "saturation":
			if out[i].SaturationPct != out[j].SaturationPct {
				return out[i].SaturationPct > out[j].SaturationPct
			}
		default: // "floorSellers"
			if out[i].FloorSellers != out[j].FloorSellers {
				return out[i].FloorSellers > out[j].FloorSellers
			}
		}
		return out[i].ItemEntry < out[j].ItemEntry
	})
	if len(out) > n {
		out = out[:n]
	}
	return out, matched
}
