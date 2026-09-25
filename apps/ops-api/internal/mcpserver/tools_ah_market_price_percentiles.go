package mcpserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

const (
	ahPricePctTimeout     = 10 * time.Second
	ahPricePctDefaultTopN = 15
	ahPricePctMaxTopN     = 100
	ahPricePctMaxQuality  = 7      // AzerothCore item Quality enum tops out at 7 (Heirloom)
	ahPricePctDefaultMin  = 1      // sample-size floor: an item needs >= this many buyout listings to be reported
	ahPricePctScanCap     = 200000 // hard ceiling on auctionhouse rows folded into one snapshot (matches ah_market_top_sellers)
)

// ahPricePctScanBase pulls one per-listing row per buyout auction so the price
// DISTRIBUTION can be computed in Go. Same cross-DB join as ah_market_top_items
// (acore_characters.auctionhouse -> item_instance -> acore_world.item_template,
// columns anchored on the canonical AzerothCore schema), but deliberately NOT
// GROUP BY'd: nearest-rank percentiles need every individual price, which MySQL
// 5.7 / MariaDB cannot compute portably server-side (no PERCENTILE_CONT). The
// fold + percentile math therefore runs in Go — the wow_bot_latency_profile
// single-scan-then-fold idiom applied to copper buyout prices instead of
// latency_ms (its nearestRankPercentile helper is reused verbatim).
//
// Only buyout listings (buyoutprice > 0) are scanned: auction-only rows carry
// buyoutprice 0 and have no fixed sale price to rank, so including them would
// poison every percentile with a phantom zero. ORDER BY it.entry keeps each
// item's rows contiguous and makes the scan-cap truncation deterministic
// (high-entry items drop first rather than an arbitrary subset).
const ahPricePctScanBase = "SELECT it.entry, it.name, it.Quality, ah.buyoutprice " +
	"FROM `auctionhouse` ah " +
	"JOIN `item_instance` ii ON ah.itemguid = ii.guid " +
	"JOIN `acore_world`.`item_template` it ON ii.itemEntry = it.entry " +
	"WHERE ah.buyoutprice > 0"

// ahPricePctTailQuery closes the query after any AND-ed filters are appended.
const ahPricePctTailQuery = " ORDER BY it.entry ASC LIMIT ?"

// ahPricePctItem is one item's buyout-price distribution. Copper fields are the
// machine-readable facet; the *Gold strings render the same values as WoW money
// (formatCopper, shared with the ah_market_* family) for at-a-glance reading.
// Listings is the sample size the percentiles are drawn from (buyout listings
// only).
type ahPricePctItem struct {
	ItemEntry       int64  `json:"itemEntry"`
	ItemName        string `json:"itemName"`
	Quality         int    `json:"quality"`
	QualityName     string `json:"qualityName"`
	Listings        int    `json:"listings"`
	MinBuyoutCopper int64  `json:"minBuyoutCopper"`
	MinBuyoutGold   string `json:"minBuyoutGold"`
	P50BuyoutCopper int64  `json:"p50BuyoutCopper"`
	P50BuyoutGold   string `json:"p50BuyoutGold"`
	P90BuyoutCopper int64  `json:"p90BuyoutCopper"`
	P90BuyoutGold   string `json:"p90BuyoutGold"`
	P95BuyoutCopper int64  `json:"p95BuyoutCopper"`
	P95BuyoutGold   string `json:"p95BuyoutGold"`
	MaxBuyoutCopper int64  `json:"maxBuyoutCopper"`
	MaxBuyoutGold   string `json:"maxBuyoutGold"`
	AvgBuyoutCopper int64  `json:"avgBuyoutCopper"`
	AvgBuyoutGold   string `json:"avgBuyoutGold"`
}

// ahPricePctAgg accumulates one item's buyout prices during the scan. min/max
// are derived from the sorted slice at fold time, so only the raw prices + their
// running sum (for the mean) need tracking here.
type ahPricePctAgg struct {
	name    string
	quality int
	prices  []uint64
	sum     uint64
}

// RegisterAhMarketPricePercentilesTool registers `ah_market_price_percentiles` —
// a read-only, per-item buyout-price DISTRIBUTION over the auction house.
//
// The first distribution lens for the ah_market_* family: ah_market_summary
// folds the market into faction totals, ah_market_top_items / top_sellers rank
// by listing count, and ah_market_category_breakdown sums by item class — none
// surface a price SPREAD. A single whale auction skews the average, so "what
// should I pay for X?" needs the median (p50) and the upper tail (p90/p95), not
// the mean. This tool joins the same auctionhouse -> item_instance ->
// item_template chain ah_market_top_items uses, but transfers per-listing buyout
// prices and computes nearest-rank percentiles per item in Go.
func RegisterAhMarketPricePercentilesTool(reg *Registry, deps DBDeps) {
	reg.Register(Tool{
		Name: "ah_market_price_percentiles",
		Description: "Per-item auction-house buyout-price distribution (min, p50/median, p90, p95, max, avg) " +
			"joining acore_characters.auctionhouse -> item_instance -> acore_world.item_template over the " +
			"ops_ro pool. The distribution companion to ah_market_top_items (which ranks by listing count " +
			"and reports only avg/min): percentiles are computed nearest-rank in Go from every buyoutprice, " +
			"because a single overpriced listing skews the mean — use this to answer \"what's a fair price " +
			"for X?\" or \"how wide is the price spread?\". Only buyout listings (buyoutprice > 0) are " +
			"sampled; auction-only rows are excluded so they can't poison the percentiles. Each item carries " +
			"itemEntry, itemName, quality + qualityName (0 Poor .. 7 Heirloom — an unknown value surfaces as " +
			"quality-<q>), listings (the sample size), and the min/p50/p90/p95/max/avg buyout in copper plus " +
			"human gold strings. Returns {asOf, topN, minListings, scannedListings, distinctItems, scanCap, " +
			"truncated, items:[...]} sorted by listings desc (itemEntry asc tiebreak). Args: topN (default 15, " +
			"max 100), houseId (optional — 2=Alliance, 6=Horde, 7=Neutral; omit for the whole market), " +
			"minQuality (optional 0-7 — only items at or above this rarity; out-of-range is rejected, not " +
			"clamped), itemEntry (optional — drill into a single item's spread), minListings (default 1, " +
			"clamps to >=1 — suppress items with too few buyout listings to form a meaningful distribution). " +
			"Scan capped at 200000 rows; truncated:true flags an incomplete fold (narrow by houseId/minQuality/" +
			"itemEntry). Read-only, 10 s timeout.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{
			"topN":{"type":"integer","description":"Max items returned, ranked by buyout-listing count (default 15, max 100)"},
			"houseId":{"type":"integer","description":"Narrow to one auction house (2=Alliance, 6=Horde, 7=Neutral); omit for the whole market"},
			"minQuality":{"type":"integer","description":"Only items at or above this rarity (0=Poor .. 7=Heirloom); omit for all qualities"},
			"itemEntry":{"type":"integer","description":"Drill into a single item_template entry's price spread"},
			"minListings":{"type":"integer","description":"Only items with at least this many buyout listings (default 1, clamps to >=1)"}
		}}`),
		Annotations: AnnRead(),
		Handler: func(ctx context.Context, raw json.RawMessage, _ string) any {
			var a struct {
				TopN        int  `json:"topN"`
				HouseID     *int `json:"houseId"`
				MinQuality  *int `json:"minQuality"`
				ItemEntry   *int `json:"itemEntry"`
				MinListings int  `json:"minListings"`
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
				topN = ahPricePctDefaultTopN
			}
			if topN > ahPricePctMaxTopN {
				topN = ahPricePctMaxTopN
			}
			// minQuality is rejected (not silently clamped) when outside the 0-7
			// enum range — a typo'd 99 would otherwise return an empty list that
			// reads as "no items" rather than "bad filter" (the ah_market_top_items
			// precedent).
			if a.MinQuality != nil && (*a.MinQuality < 0 || *a.MinQuality > ahPricePctMaxQuality) {
				return map[string]any{"error": fmt.Sprintf("minQuality out of range: %d (valid 0-%d)", *a.MinQuality, ahPricePctMaxQuality)}
			}
			minListings := a.MinListings
			if minListings < ahPricePctDefaultMin {
				minListings = ahPricePctDefaultMin
			}
			c, cancel := context.WithTimeout(ctx, ahPricePctTimeout)
			defer cancel()
			out, err := collectAhPricePercentiles(c, deps.QueryDB, time.Now(), topN, minListings, a.HouseID, a.MinQuality, a.ItemEntry)
			if err != nil {
				return map[string]any{"error": err.Error()}
			}
			return out
		},
	})
}

// collectAhPricePercentiles scans the (optionally filtered) buyout listings,
// folds them per item_template entry, and computes a nearest-rank percentile
// distribution per item. Split out so tests can drive it with sqlmock and a
// fixed `now`.
func collectAhPricePercentiles(ctx context.Context, db *sql.DB, now time.Time, topN, minListings int, houseID, minQuality, itemEntry *int) (map[string]any, error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire conn: %w", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "USE `acore_characters`"); err != nil {
		return nil, fmt.Errorf("USE acore_characters: %w", err)
	}

	query := ahPricePctScanBase
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
	if itemEntry != nil {
		conds = append(conds, "it.entry = ?")
		args = append(args, *itemEntry)
	}
	if len(conds) > 0 {
		// ahPricePctScanBase already opens the WHERE (buyoutprice > 0), so extra
		// filters are AND-ed on.
		query += " AND " + strings.Join(conds, " AND ")
	}
	query += ahPricePctTailQuery
	args = append(args, ahPricePctScanCap+1)

	rows, err := conn.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("price-percentiles query: %w", err)
	}
	defer rows.Close()

	byItem := map[int64]*ahPricePctAgg{}
	scanned := 0
	truncated := false
	for rows.Next() {
		if scanned >= ahPricePctScanCap {
			truncated = true
			break
		}
		var entry int64
		var name string
		var quality int
		var price uint64
		if err := rows.Scan(&entry, &name, &quality, &price); err != nil {
			return nil, fmt.Errorf("price-percentiles scan: %w", err)
		}
		scanned++
		a := byItem[entry]
		if a == nil {
			a = &ahPricePctAgg{name: name, quality: quality}
			byItem[entry] = a
		}
		a.prices = append(a.prices, price)
		a.sum += price
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("price-percentiles iter: %w", err)
	}

	items, distinct := topNAhPricePctItems(byItem, minListings, topN)

	out := map[string]any{
		"asOf":            now.UTC().Format(time.RFC3339),
		"topN":            topN,
		"minListings":     minListings,
		"scannedListings": scanned,
		"distinctItems":   distinct,
		"scanCap":         ahPricePctScanCap,
		"truncated":       truncated,
		"items":           items,
	}
	// houseId/faction, minQuality, itemEntry are echoed only when the filter was
	// set, so the response reflects exactly what was applied (an absent key == no
	// filter).
	if houseID != nil {
		out["houseId"] = *houseID
		out["faction"] = ahHouseFaction(*houseID)
	}
	if minQuality != nil {
		out["minQuality"] = *minQuality
	}
	if itemEntry != nil {
		out["itemEntry"] = *itemEntry
	}
	return out, nil
}

// topNAhPricePctItems filters the folded items to those with >= minListings
// buyout listings, computes each item's nearest-rank percentile distribution
// (min/p50/p90/p95/max from the ascending-sorted price slice, avg from the
// running sum), sorts listings-desc with itemEntry-asc as a deterministic
// tiebreaker (Go's randomized map iteration would otherwise flake tests on tied
// counts), slices to n, and returns the slice plus the pre-slice distinct count
// (so the caller can report "the top 15 of 240 priced items"). Always a non-nil
// slice.
func topNAhPricePctItems(m map[int64]*ahPricePctAgg, minListings, n int) ([]*ahPricePctItem, int) {
	out := make([]*ahPricePctItem, 0, len(m))
	for entry, a := range m {
		if len(a.prices) < minListings {
			continue
		}
		sort.Slice(a.prices, func(i, j int) bool { return a.prices[i] < a.prices[j] })
		count := len(a.prices)
		minP := int64(a.prices[0])
		maxP := int64(a.prices[count-1])
		avg := int64(a.sum / uint64(count)) // count > 0: the bucket exists because a row landed in it
		p50 := int64(nearestRankPercentile(a.prices, 0.50))
		p90 := int64(nearestRankPercentile(a.prices, 0.90))
		p95 := int64(nearestRankPercentile(a.prices, 0.95))
		out = append(out, &ahPricePctItem{
			ItemEntry:       entry,
			ItemName:        a.name,
			Quality:         a.quality,
			QualityName:     itemQualityName(a.quality),
			Listings:        count,
			MinBuyoutCopper: minP,
			MinBuyoutGold:   formatCopper(minP),
			P50BuyoutCopper: p50,
			P50BuyoutGold:   formatCopper(p50),
			P90BuyoutCopper: p90,
			P90BuyoutGold:   formatCopper(p90),
			P95BuyoutCopper: p95,
			P95BuyoutGold:   formatCopper(p95),
			MaxBuyoutCopper: maxP,
			MaxBuyoutGold:   formatCopper(maxP),
			AvgBuyoutCopper: avg,
			AvgBuyoutGold:   formatCopper(avg),
		})
	}
	distinct := len(out)
	sort.Slice(out, func(i, j int) bool {
		if out[i].Listings != out[j].Listings {
			return out[i].Listings > out[j].Listings
		}
		return out[i].ItemEntry < out[j].ItemEntry
	})
	if len(out) > n {
		out = out[:n]
	}
	return out, distinct
}
