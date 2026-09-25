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
	ahOutlierTimeout            = 10 * time.Second
	ahOutlierDefaultTopN        = 50
	ahOutlierMaxTopN            = 500
	ahOutlierMaxQuality         = 7      // AzerothCore item Quality enum tops out at 7 (Heirloom)
	ahOutlierDefaultMinRatio    = 3.0    // flag listings priced >= 3x the item median by default
	ahOutlierHighRatio          = 10.0   // absolute danger line: >=10x median is almost certainly a typo/scam
	ahOutlierDefaultMinListings = 5      // an item needs this many buyout listings for its median to be a trustworthy baseline
	ahOutlierMinListingsFloor   = 2      // hard floor: a baseline + at least one candidate listing
	ahOutlierScanCap            = 200000 // hard ceiling on auctionhouse rows folded into one snapshot (matches ah_market_price_percentiles)
)

// ahOutlierScanBase pulls one per-listing row per buyout auction so each item's
// median can be computed in Go and every individual listing compared against it.
// Same cross-DB join as ah_market_price_percentiles / ah_market_top_items
// (acore_characters.auctionhouse -> item_instance -> acore_world.item_template,
// columns anchored on the canonical AzerothCore schema), but it additionally
// carries ah.id (the auction id, so an operator can act on the exact listing)
// and ah.itemowner (the seller guid, resolvable via wow_player_lookup /
// ah_market_top_sellers). Deliberately NOT GROUP BY'd: an outlier is a single
// listing relative to its item's median, so every individual buyoutprice must
// be transferred.
//
// Only buyout listings (buyoutprice > 0) are scanned: auction-only rows carry
// buyoutprice 0 and have no fixed sale price to rank, so including them would
// drag every median toward zero and manufacture phantom outliers. ORDER BY
// it.entry keeps each item's rows contiguous and makes the scan-cap truncation
// deterministic (high-entry items drop first rather than an arbitrary subset).
const ahOutlierScanBase = "SELECT ah.id, it.entry, it.name, it.Quality, ah.houseid, ah.itemowner, ah.buyoutprice " +
	"FROM `auctionhouse` ah " +
	"JOIN `item_instance` ii ON ah.itemguid = ii.guid " +
	"JOIN `acore_world`.`item_template` it ON ii.itemEntry = it.entry " +
	"WHERE ah.buyoutprice > 0"

// ahOutlierTailQuery closes the query after any AND-ed filters are appended.
const ahOutlierTailQuery = " ORDER BY it.entry ASC LIMIT ?"

// ahOutlierListing is one raw buyout listing collected during the scan, retained
// so pass two can compare it against its item's median once the median is known.
type ahOutlierListing struct {
	auctionID  int64
	houseID    int64
	sellerGuid int64
	price      uint64
}

// ahOutlierAgg accumulates one item's buyout listings during the scan. The
// median is derived from the listing prices at fold time.
type ahOutlierAgg struct {
	name     string
	quality  int
	listings []ahOutlierListing
}

// ahPriceOutlier is one flagged listing: a single buyout priced far above its
// item's median. Copper fields are the machine-readable facet; the *Gold strings
// render the same values as WoW money (formatCopper, shared with the ah_market_*
// family). Ratio is listingBuyout / medianBuyout. The decision to flag uses the
// raw ratio; Ratio here is rounded for display only.
type ahPriceOutlier struct {
	AuctionID           int64   `json:"auctionId"`
	ItemEntry           int64   `json:"itemEntry"`
	ItemName            string  `json:"itemName"`
	Quality             int     `json:"quality"`
	QualityName         string  `json:"qualityName"`
	HouseID             int64   `json:"houseId"`
	Faction             string  `json:"faction"`
	SellerGuid          int64   `json:"sellerGuid"`
	ListingBuyoutCopper int64   `json:"listingBuyoutCopper"`
	ListingBuyoutGold   string  `json:"listingBuyoutGold"`
	MedianBuyoutCopper  int64   `json:"medianBuyoutCopper"`
	MedianBuyoutGold    string  `json:"medianBuyoutGold"`
	Ratio               float64 `json:"ratio"`
	ListingsForItem     int     `json:"listingsForItem"`
	Severity            string  `json:"severity"`
	Reason              string  `json:"reason"`
}

// classifyPriceOutlier grades a flagged listing. high = ratio at or above the
// absolute danger line (10x by default — an extra digit / scam), else medium.
// The cutoff scales up with minRatio so that when an operator deliberately sets
// minRatio above the danger line everything flagged reads as high (a medium band
// only exists below the danger line).
func classifyPriceOutlier(ratio, minRatio float64) string {
	high := ahOutlierHighRatio
	if minRatio > high {
		high = minRatio
	}
	if ratio >= high {
		return "high"
	}
	return "medium"
}

// roundOutlierRatio rounds a positive ratio to two decimals for stable display.
// Ratios are always > 0 (price and median are both > 0), so the +0.5 rounding
// needs no negative guard.
func roundOutlierRatio(r float64) float64 {
	return float64(int64(r*100+0.5)) / 100
}

// priceOutlierReason renders the human explanation surfaced on each flagged
// listing, including the verify-before-acting guidance (the rowsEstimate-honesty
// pattern: tell the operator to confirm the specific listing, not act blindly on
// the heuristic).
func priceOutlierReason(severity string, ratio float64, listingCopper, medianCopper int64, listings int) string {
	tail := "above the typical spread — review the listing before repricing"
	if severity == "high" {
		tail = "almost certainly a typo or scam — verify the listing before cancelling/repricing"
	}
	return fmt.Sprintf("buyout %s = %gx the %d-listing median %s; %s",
		formatCopper(listingCopper), ratio, listings, formatCopper(medianCopper), tail)
}

// RegisterAhMarketPriceOutliersTool registers `ah_market_price_outliers` — a
// read-only scan that flags individual auction listings priced far above their
// item's median buyout.
//
// The action sibling of ah_market_price_percentiles: where that tool reports the
// per-item price DISTRIBUTION, this one names the specific listings (auction id +
// seller guid) that sit at the top tail, i.e. "which auctions are mispriced /
// typo'd / scams?". The baseline is the median (p50), not the mean, precisely
// because a scam listing inflates the mean toward itself and would mask the
// outlier — the median is robust to it (the whole rationale of #274). Reuses the
// nearestRankPercentile helper (from wow_bot_latency_profile, also used by
// ah_market_price_percentiles) and the same cross-DB auctionhouse ->
// item_instance -> item_template join.
func RegisterAhMarketPriceOutliersTool(reg *Registry, deps DBDeps) {
	reg.Register(Tool{
		Name: "ah_market_price_outliers",
		Description: "Flag individual auction-house listings priced far above their item's median buyout — the " +
			"\"is this auction a typo or scam?\" view, joining acore_characters.auctionhouse -> item_instance -> " +
			"acore_world.item_template over the ops_ro pool. The action sibling of ah_market_price_percentiles: " +
			"that tool reports the per-item price distribution, this one names the specific listings at the top " +
			"tail. For every item with enough buyout listings the median (p50) is computed nearest-rank in Go " +
			"(the median is used, not the mean, because one overpriced listing skews the mean toward itself and " +
			"masks the outlier), then each listing whose buyoutprice is >= minRatio x that median is reported. " +
			"Each outlier carries auctionId (the exact listing), sellerGuid (the auctionhouse.itemowner — resolve " +
			"via wow_player_lookup or ah_market_top_sellers), itemEntry/itemName, quality + qualityName (0 Poor .. " +
			"7 Heirloom), houseId + faction, the listing + median buyout in copper plus human gold, the ratio, the " +
			"item's listing count, and a severity (high = >= 10x median, almost certainly a typo/scam; medium = " +
			"above minRatio but below 10x). Returns {asOf, topN, minRatio, minListings, scannedListings, " +
			"evaluatedItems, evaluatedListings, outlierCount, scanCap, truncated, outliers:[...]} sorted ratio " +
			"desc (auctionId asc tiebreak); outlierCount is the honest pre-topN total. Args: topN (default 50, " +
			"max 500 — trims the returned list, not the count), minRatio (default 3.0 — must be > 1.0, rejected " +
			"not clamped), minListings (default 5, floor 2 — an item needs this many buyout listings for its " +
			"median to be a trustworthy baseline; thin-sample items are scanned but not evaluated), houseId " +
			"(optional — 2=Alliance, 6=Horde, 7=Neutral; omit for the whole market), minQuality (optional 0-7, " +
			"rejected not clamped), itemEntry (optional — restrict to one item). Scan capped at 200000 rows; " +
			"truncated:true flags an incomplete fold (narrow by houseId/minQuality/itemEntry). Read-only, 10 s " +
			"timeout.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{
			"topN":{"type":"integer","description":"Max outliers returned, worst-first by ratio (default 50, max 500); trims the list, not the count"},
			"minRatio":{"type":"number","description":"Flag listings priced at least this multiple of the item median (default 3.0, must be > 1.0)"},
			"minListings":{"type":"integer","description":"An item needs at least this many buyout listings to be evaluated (default 5, floor 2)"},
			"houseId":{"type":"integer","description":"Narrow to one auction house (2=Alliance, 6=Horde, 7=Neutral); omit for the whole market"},
			"minQuality":{"type":"integer","description":"Only items at or above this rarity (0=Poor .. 7=Heirloom); omit for all qualities"},
			"itemEntry":{"type":"integer","description":"Restrict to a single item_template entry"}
		}}`),
		Annotations: AnnRead(),
		Handler: func(ctx context.Context, raw json.RawMessage, _ string) any {
			var a struct {
				TopN        int      `json:"topN"`
				MinRatio    *float64 `json:"minRatio"`
				MinListings int      `json:"minListings"`
				HouseID     *int     `json:"houseId"`
				MinQuality  *int     `json:"minQuality"`
				ItemEntry   *int     `json:"itemEntry"`
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
				topN = ahOutlierDefaultTopN
			}
			if topN > ahOutlierMaxTopN {
				topN = ahOutlierMaxTopN
			}
			// minRatio is rejected (not clamped) when <= 1.0 — a multiple at or
			// below 1x would flag the median itself and everything above it,
			// reading as "everything is an outlier" rather than a real threshold.
			minRatio := ahOutlierDefaultMinRatio
			if a.MinRatio != nil {
				if *a.MinRatio <= 1.0 {
					return map[string]any{"error": fmt.Sprintf("minRatio must be greater than 1.0 (got %g)", *a.MinRatio)}
				}
				minRatio = *a.MinRatio
			}
			// minQuality is rejected (not silently clamped) when outside the 0-7
			// enum range — a typo'd 99 would otherwise return an empty list that
			// reads as "no outliers" rather than "bad filter" (the
			// ah_market_price_percentiles precedent).
			if a.MinQuality != nil && (*a.MinQuality < 0 || *a.MinQuality > ahOutlierMaxQuality) {
				return map[string]any{"error": fmt.Sprintf("minQuality out of range: %d (valid 0-%d)", *a.MinQuality, ahOutlierMaxQuality)}
			}
			// minListings: unset/zero -> default 5; an explicit value below the
			// hard floor of 2 (a median needs a baseline plus at least one
			// candidate) is clamped up to the floor.
			minListings := a.MinListings
			if minListings <= 0 {
				minListings = ahOutlierDefaultMinListings
			} else if minListings < ahOutlierMinListingsFloor {
				minListings = ahOutlierMinListingsFloor
			}
			c, cancel := context.WithTimeout(ctx, ahOutlierTimeout)
			defer cancel()
			out, err := collectAhPriceOutliers(c, deps.QueryDB, time.Now(), topN, minListings, minRatio, a.HouseID, a.MinQuality, a.ItemEntry)
			if err != nil {
				return map[string]any{"error": err.Error()}
			}
			return out
		},
	})
}

// collectAhPriceOutliers scans the (optionally filtered) buyout listings, folds
// them per item_template entry, computes each item's nearest-rank median, and
// flags every listing priced >= minRatio x that median. Split out so tests can
// drive it with sqlmock and a fixed `now`.
func collectAhPriceOutliers(ctx context.Context, db *sql.DB, now time.Time, topN, minListings int, minRatio float64, houseID, minQuality, itemEntry *int) (map[string]any, error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire conn: %w", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "USE `acore_characters`"); err != nil {
		return nil, fmt.Errorf("USE acore_characters: %w", err)
	}

	query := ahOutlierScanBase
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
		// ahOutlierScanBase already opens the WHERE (buyoutprice > 0), so extra
		// filters are AND-ed on.
		query += " AND " + strings.Join(conds, " AND ")
	}
	query += ahOutlierTailQuery
	args = append(args, ahOutlierScanCap+1)

	rows, err := conn.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("price-outliers query: %w", err)
	}
	defer rows.Close()

	byItem := map[int64]*ahOutlierAgg{}
	scanned := 0
	truncated := false
	for rows.Next() {
		if scanned >= ahOutlierScanCap {
			truncated = true
			break
		}
		var auctionID, entry, houseid, owner int64
		var name string
		var quality int
		var price uint64
		if err := rows.Scan(&auctionID, &entry, &name, &quality, &houseid, &owner, &price); err != nil {
			return nil, fmt.Errorf("price-outliers scan: %w", err)
		}
		scanned++
		ag := byItem[entry]
		if ag == nil {
			ag = &ahOutlierAgg{name: name, quality: quality}
			byItem[entry] = ag
		}
		ag.listings = append(ag.listings, ahOutlierListing{auctionID: auctionID, houseID: houseid, sellerGuid: owner, price: price})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("price-outliers iter: %w", err)
	}

	outliers, evaluatedItems, evaluatedListings := flagAhPriceOutliers(byItem, minListings, minRatio, topN)

	out := map[string]any{
		"asOf":              now.UTC().Format(time.RFC3339),
		"topN":              topN,
		"minRatio":          minRatio,
		"minListings":       minListings,
		"scannedListings":   scanned,
		"evaluatedItems":    evaluatedItems,
		"evaluatedListings": evaluatedListings,
		"outlierCount":      len(outliers.all),
		"scanCap":           ahOutlierScanCap,
		"truncated":         truncated,
		"outliers":          outliers.shown,
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

// ahOutlierResult carries both the honest pre-topN count (all) and the trimmed
// list shown to the caller (shown), so outlierCount stays truthful while the
// returned array respects topN.
type ahOutlierResult struct {
	all   []*ahPriceOutlier
	shown []*ahPriceOutlier
}

// flagAhPriceOutliers walks the folded items, evaluates only those with at least
// minListings buyout listings (so the median is a trustworthy baseline), flags
// every listing priced >= minRatio x the item median, sorts the result worst-
// first (ratio desc, auctionId asc as a deterministic tiebreaker against Go's
// randomized map iteration), and trims to topN while reporting the pre-trim
// count. Also returns the number of evaluated items and the listing population
// across them (the honest denominator for "what fraction of listings are
// outliers"). The shown slice is always non-nil.
func flagAhPriceOutliers(m map[int64]*ahOutlierAgg, minListings int, minRatio float64, topN int) (ahOutlierResult, int, int) {
	all := make([]*ahPriceOutlier, 0)
	evaluatedItems := 0
	evaluatedListings := 0
	for entry, ag := range m {
		if len(ag.listings) < minListings {
			continue
		}
		evaluatedItems++
		evaluatedListings += len(ag.listings)
		prices := make([]uint64, len(ag.listings))
		for i, l := range ag.listings {
			prices[i] = l.price
		}
		sort.Slice(prices, func(i, j int) bool { return prices[i] < prices[j] })
		median := nearestRankPercentile(prices, 0.50)
		if median == 0 {
			continue // defensive: buyoutprice > 0 is enforced in SQL, so median > 0
		}
		for _, l := range ag.listings {
			ratio := float64(l.price) / float64(median)
			if ratio < minRatio {
				continue
			}
			rr := roundOutlierRatio(ratio)
			sev := classifyPriceOutlier(ratio, minRatio)
			listingCopper := int64(l.price)
			medianCopper := int64(median)
			all = append(all, &ahPriceOutlier{
				AuctionID:           l.auctionID,
				ItemEntry:           entry,
				ItemName:            ag.name,
				Quality:             ag.quality,
				QualityName:         itemQualityName(ag.quality),
				HouseID:             l.houseID,
				Faction:             ahHouseFaction(int(l.houseID)),
				SellerGuid:          l.sellerGuid,
				ListingBuyoutCopper: listingCopper,
				ListingBuyoutGold:   formatCopper(listingCopper),
				MedianBuyoutCopper:  medianCopper,
				MedianBuyoutGold:    formatCopper(medianCopper),
				Ratio:               rr,
				ListingsForItem:     len(ag.listings),
				Severity:            sev,
				Reason:              priceOutlierReason(sev, rr, listingCopper, medianCopper, len(ag.listings)),
			})
		}
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].Ratio != all[j].Ratio {
			return all[i].Ratio > all[j].Ratio
		}
		return all[i].AuctionID < all[j].AuctionID
	})
	shown := all
	if len(shown) > topN {
		shown = shown[:topN]
	}
	return ahOutlierResult{all: all, shown: shown}, evaluatedItems, evaluatedListings
}
