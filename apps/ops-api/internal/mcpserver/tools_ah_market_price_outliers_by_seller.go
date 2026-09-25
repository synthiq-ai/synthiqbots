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
	ahOutlierBySellerTimeout = 10 * time.Second

	// ahOutlierBySellerDefaultTopN / MaxTopN bound the SELLER list (not the
	// listing list — that scan is capped separately by ahOutlierScanCap). Seller
	// counts are far smaller than listing counts, so the same 25/200 envelope as
	// ah_market_top_sellers fits (vs ah_market_price_outliers' 50/500, which sizes
	// a per-LISTING result).
	ahOutlierBySellerDefaultTopN = 25
	ahOutlierBySellerMaxTopN     = 200

	// ahOutlierBySellerDefaultMinOutliers — a seller surfaces once they own at
	// least this many flagged (outlier) listings. Default 1 = every seller with
	// any outlier; raise it to isolate REPEAT offenders (the distinguishing knob
	// of this tool vs the per-listing ah_market_price_outliers). Clamps to >=1.
	ahOutlierBySellerDefaultMinOutliers = 1

	// ahOutlierBySellerUnboundedListings is passed as flagAhPriceOutliers' topN so
	// its `shown` trim is a no-op: this tool regroups the FULL pre-trim outlier set
	// (result.all) by seller, then applies its own seller-level topN afterward.
	ahOutlierBySellerUnboundedListings = 1 << 30
)

// ahOutlierSeller is one ranked seller, aggregated over the subset of THEIR
// listings that ah_market_price_outliers flagged as priced far above the
// per-item median. The identity block (characterName / accountId /
// accountUsername / isBot) is folded in from the same characters + account batch
// resolution ah_market_top_sellers uses (empty name + account 0 when the owning
// character row is gone — a deleted character whose listings outlive it). Copper
// sums carry human gold strings alongside (formatCopper) because a repeat-
// offender leaderboard is meant to be eyeballed.
type ahOutlierSeller struct {
	SellerGuid      int64  `json:"sellerGuid"`
	CharacterName   string `json:"characterName"`
	AccountID       int64  `json:"accountId"`
	AccountUsername string `json:"accountUsername"`
	IsBot           bool   `json:"isBot"`

	OutlierListings   int     `json:"outlierListings"`   // how many of this seller's listings are outliers
	DistinctItems     int     `json:"distinctItems"`     // across how many distinct item_template entries
	HighSeverityCount int     `json:"highSeverityCount"` // how many of those are `high` (>=10x median)
	TotalMarkupCopper int64   `json:"totalMarkupCopper"` // sum of (listing - itemMedian) across the seller's outliers
	TotalMarkupGold   string  `json:"totalMarkupGold"`
	MaxRatio          float64 `json:"maxRatio"` // the seller's single worst listing/median ratio

	WorstAuctionID     int64  `json:"worstAuctionId"` // the listing carrying MaxRatio (act on this one first)
	WorstItemEntry     int64  `json:"worstItemEntry"`
	WorstItemName      string `json:"worstItemName"`
	WorstListingCopper int64  `json:"worstListingCopper"`
	WorstListingGold   string `json:"worstListingGold"`

	Severity string `json:"severity"` // `high` if the seller owns any high-severity outlier, else `medium`
	Reason   string `json:"reason"`

	// items tracks the distinct item entries this seller overprices so
	// DistinctItems can be set at finalize time. Unexported -> not serialized.
	items map[int64]bool
}

// fold attributes one flagged outlier listing to the seller, accumulating the
// counts, distinct-item set, total over-median markup, high-severity tally, and
// the running worst-ratio listing. Order-independent (sums + a max), so the
// result is deterministic regardless of map-iteration order upstream.
func (s *ahOutlierSeller) fold(o *ahPriceOutlier) {
	s.OutlierListings++
	if s.items == nil {
		s.items = map[int64]bool{}
	}
	s.items[o.ItemEntry] = true
	// listing > minRatio*median > median, so markup is always positive; the
	// guard is purely defensive against a future caller passing minRatio <= 1.
	markup := o.ListingBuyoutCopper - o.MedianBuyoutCopper
	if markup > 0 {
		s.TotalMarkupCopper += markup
	}
	if o.Severity == "high" {
		s.HighSeverityCount++
	}
	if o.Ratio > s.MaxRatio {
		s.MaxRatio = o.Ratio
		s.WorstAuctionID = o.AuctionID
		s.WorstItemEntry = o.ItemEntry
		s.WorstItemName = o.ItemName
		s.WorstListingCopper = o.ListingBuyoutCopper
	}
}

// finalize sets the distinct-item count, the seller-level severity (high if any
// of the seller's outliers crossed the absolute danger line), the human gold
// strings, and the operator-facing reason. Called only on the topN slice so we
// don't format copper for sellers that never make the cut.
func (s *ahOutlierSeller) finalize() {
	s.DistinctItems = len(s.items)
	s.Severity = "medium"
	if s.HighSeverityCount > 0 {
		s.Severity = "high"
	}
	s.TotalMarkupGold = formatCopper(s.TotalMarkupCopper)
	s.WorstListingGold = formatCopper(s.WorstListingCopper)
	s.Reason = s.buildReason()
}

// buildReason renders the human explanation, leading with the bot-vs-player call
// (the recurring "is the gouger a misconfigured AHBot or a real scammer?"
// question on a mod-ah-bot realm) and closing with the verify-before-acting
// guidance shared with ah_market_price_outliers.
func (s *ahOutlierSeller) buildReason() string {
	who := "seller"
	if s.IsBot {
		who = "AHBot character"
	}
	scam := ""
	if s.HighSeverityCount > 0 {
		scam = fmt.Sprintf(" (%d at >=10x — almost certainly typos/scams)", s.HighSeverityCount)
	}
	return fmt.Sprintf(
		"this %s owns %d outlier listings across %d items%s; worst %gx the item median on '%s' (auction %d, %s), total markup %s over the per-item medians. On a mod-ah-bot realm a repeat over-pricer is usually a misconfigured AHBot — verify the listings (resolve the guid via wow_player_lookup) before mass-cancelling/repricing.",
		who, s.OutlierListings, s.DistinctItems, scam, s.MaxRatio, s.WorstItemName, s.WorstAuctionID, s.WorstListingGold, s.TotalMarkupGold)
}

// ahOutlierSellerSeverityRank orders the seller-level severities worst-first for
// the candidate sort (high before medium). classifyPriceOutlier only ever yields
// high/medium, so two levels suffice.
func ahOutlierSellerSeverityRank(sev string) int {
	if sev == "high" {
		return 0
	}
	return 1
}

// RegisterAhMarketPriceOutliersBySellerTool registers
// `ah_market_price_outliers_by_seller` — the repeat-offender rollup of
// ah_market_price_outliers. Where that tool names individual mispriced LISTINGS,
// this one aggregates those same outliers per SELLER ("who keeps over-pricing?"),
// folding in the characters + account batch resolution from ah_market_top_sellers
// so the isBot flag answers the recurring "is the gouger a misconfigured AHBot or
// a real scammer?" question. The WHO-is-mispricing view: ah_market_top_sellers
// ranks sellers by VOLUME, ah_market_price_outliers lists per-listing outliers,
// and this sits at their intersection — outlier-ness aggregated BY seller.
func RegisterAhMarketPriceOutliersBySellerTool(reg *Registry, deps DBDeps) {
	reg.Register(Tool{
		Name: "ah_market_price_outliers_by_seller",
		Description: "Rank auction-house sellers by how many of their listings are priced far above the item median " +
			"— the repeat-offender rollup of ah_market_price_outliers, joining acore_characters.auctionhouse -> " +
			"item_instance -> acore_world.item_template over the ops_ro pool. ah_market_price_outliers names the " +
			"individual mispriced listings; ah_market_top_sellers ranks sellers by VOLUME; this tool sits at their " +
			"intersection — it groups the SAME median-baseline outliers BY seller to answer \"which sellers keep " +
			"over-pricing?\". For every item with enough buyout listings the median (p50) is computed nearest-rank " +
			"in Go (the median, not the mean, so one overpriced listing can't skew the baseline toward itself and " +
			"mask the outlier), each listing priced >= minRatio x that median is an outlier, and outliers are then " +
			"folded per itemowner. Each seller carries sellerGuid plus characterName + accountId + accountUsername " +
			"+ isBot (the same characters + acore_auth.account batch resolution as ah_market_top_sellers, flagging " +
			"the RNDBOT AHBot prefix), outlierListings, distinctItems, highSeverityCount, totalMarkup (copper + " +
			"human gold), maxRatio, the worst listing (worstAuctionId / worstItemEntry / worstItemName / " +
			"worstListing), and a severity (high = the seller owns any listing >= 10x the median, almost certainly " +
			"a typo/scam; medium otherwise). On a mod-ah-bot server a repeat over-pricer is usually a misconfigured " +
			"AHBot, so isBot makes that visible without a separate wow_player_lookup. Returns {asOf, topN, minRatio, " +
			"minListings, minOutlierListings, scannedListings, evaluatedItems, evaluatedListings, " +
			"totalOutlierListings, distinctOutlierSellers, scanCap, truncated, unresolved, sellers:[...]} sorted " +
			"worst-first (severity, then outlierListings desc, then totalMarkup desc, sellerGuid asc tiebreak); " +
			"distinctOutlierSellers is the honest pre-topN count. Args: topN (default 25, max 200 — trims the seller " +
			"list, not the counts), minRatio (default 3.0 — must be > 1.0, rejected not clamped), minListings " +
			"(default 5, floor 2 — an item needs this many buyout listings for its median to be a trustworthy " +
			"baseline; thin-sample items are scanned but not evaluated), minOutlierListings (default 1, clamps " +
			">=1 — only sellers with at least this many flagged listings, the repeat-offender threshold), houseId " +
			"(optional — 2=Alliance, 6=Horde, 7=Neutral; omit for the whole market), minQuality (optional 0-7, " +
			"rejected not clamped). A seller whose owning character row is gone surfaces with an empty " +
			"characterName + accountId 0 and is counted in `unresolved`. Scan capped at 200000 rows; truncated:true " +
			"flags an incomplete fold (narrow by houseId/minQuality). Read-only, 10 s timeout.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{
			"topN":{"type":"integer","description":"Max sellers returned, worst-first (default 25, max 200); trims the list, not the counts"},
			"minRatio":{"type":"number","description":"A listing is an outlier at this multiple of the item median or above (default 3.0, must be > 1.0)"},
			"minListings":{"type":"integer","description":"An item needs at least this many buyout listings to be evaluated (default 5, floor 2)"},
			"minOutlierListings":{"type":"integer","description":"Only rank sellers with at least this many outlier listings (default 1, clamps to >=1) — raise to isolate repeat offenders"},
			"houseId":{"type":"integer","description":"Narrow to one auction house (2=Alliance, 6=Horde, 7=Neutral); omit for the whole market"},
			"minQuality":{"type":"integer","description":"Only items at or above this rarity (0=Poor .. 7=Heirloom); omit for all qualities"}
		}}`),
		Annotations: AnnRead(),
		Handler: func(ctx context.Context, raw json.RawMessage, _ string) any {
			var a struct {
				TopN               int      `json:"topN"`
				MinRatio           *float64 `json:"minRatio"`
				MinListings        int      `json:"minListings"`
				MinOutlierListings int      `json:"minOutlierListings"`
				HouseID            *int     `json:"houseId"`
				MinQuality         *int     `json:"minQuality"`
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
				topN = ahOutlierBySellerDefaultTopN
			}
			if topN > ahOutlierBySellerMaxTopN {
				topN = ahOutlierBySellerMaxTopN
			}
			// minRatio is rejected (not clamped) when <= 1.0 — a multiple at or
			// below 1x would flag the median itself and everything above it (the
			// ah_market_price_outliers precedent).
			minRatio := ahOutlierDefaultMinRatio
			if a.MinRatio != nil {
				if *a.MinRatio <= 1.0 {
					return map[string]any{"error": fmt.Sprintf("minRatio must be greater than 1.0 (got %g)", *a.MinRatio)}
				}
				minRatio = *a.MinRatio
			}
			// minQuality is rejected (not silently clamped) when outside 0-7 — a
			// typo'd 99 would otherwise read as "no outliers" rather than "bad
			// filter".
			if a.MinQuality != nil && (*a.MinQuality < 0 || *a.MinQuality > ahOutlierMaxQuality) {
				return map[string]any{"error": fmt.Sprintf("minQuality out of range: %d (valid 0-%d)", *a.MinQuality, ahOutlierMaxQuality)}
			}
			// minListings: unset/zero -> default 5; below the hard floor of 2 (a
			// median needs a baseline plus at least one candidate) -> floor.
			minListings := a.MinListings
			if minListings <= 0 {
				minListings = ahOutlierDefaultMinListings
			} else if minListings < ahOutlierMinListingsFloor {
				minListings = ahOutlierMinListingsFloor
			}
			// minOutlierListings clamps to >=1 (the ah_market_top_sellers
			// minListings floor pattern): an explicit 0/negative is meaningless
			// (every outlier seller owns at least one outlier).
			minOutlierListings := a.MinOutlierListings
			if minOutlierListings < ahOutlierBySellerDefaultMinOutliers {
				minOutlierListings = ahOutlierBySellerDefaultMinOutliers
			}
			c, cancel := context.WithTimeout(ctx, ahOutlierBySellerTimeout)
			defer cancel()
			out, err := collectAhPriceOutliersBySeller(c, deps.QueryDB, time.Now(), topN, minListings, minOutlierListings, minRatio, a.HouseID, a.MinQuality)
			if err != nil {
				return map[string]any{"error": err.Error()}
			}
			return out
		},
	})
}

// collectAhPriceOutliersBySeller scans the (optionally filtered) buyout listings
// exactly as ah_market_price_outliers (reusing its query + per-item median +
// outlier-flagging logic so an outlier here is identical to one there), regroups
// the flagged listings per seller, ranks the repeat offenders, and resolves the
// top-N seller guids to character + account identity (the ah_market_top_sellers
// batch-IN composite, reached through throwaway *ahSeller proxies so the merged
// resolution helpers are reused verbatim rather than re-implemented). Split out so
// tests can drive it with sqlmock and a fixed `now`.
func collectAhPriceOutliersBySeller(ctx context.Context, db *sql.DB, now time.Time, topN, minListings, minOutlierListings int, minRatio float64, houseID, minQuality *int) (map[string]any, error) {
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
	if len(conds) > 0 {
		// ahOutlierScanBase already opens the WHERE (buyoutprice > 0).
		for _, c := range conds {
			query += " AND " + c
		}
	}
	query += ahOutlierTailQuery
	args = append(args, ahOutlierScanCap+1)

	rows, err := conn.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("price-outliers-by-seller query: %w", err)
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
			return nil, fmt.Errorf("price-outliers-by-seller scan: %w", err)
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
		return nil, fmt.Errorf("price-outliers-by-seller iter: %w", err)
	}

	// Reuse the exact #284 flagging (pass an unbounded listing-topN so its `shown`
	// trim is a no-op; we regroup result.all — the honest full outlier set).
	result, evaluatedItems, evaluatedListings := flagAhPriceOutliers(byItem, minListings, minRatio, ahOutlierBySellerUnboundedListings)
	sellers, distinctSellers := rollupAhOutlierSellers(result.all, minOutlierListings, topN)

	// Stage 3: resolve the top-N seller guids to characters + accounts, reusing
	// ah_market_top_sellers' merged batch helpers through proxy *ahSeller rows.
	unresolved := 0
	if len(sellers) > 0 {
		proxies := make([]*ahSeller, len(sellers))
		for i, s := range sellers {
			proxies[i] = &ahSeller{ItemOwnerGuid: s.SellerGuid}
		}
		if err := annotateSellerCharacters(ctx, conn, proxies); err != nil {
			return nil, err
		}
		if _, err := conn.ExecContext(ctx, "USE `acore_auth`"); err != nil {
			return nil, fmt.Errorf("USE acore_auth: %w", err)
		}
		if err := annotateSellerAccounts(ctx, conn, proxies); err != nil {
			return nil, err
		}
		for i, s := range sellers {
			s.CharacterName = proxies[i].CharacterName
			s.AccountID = proxies[i].AccountID
			s.AccountUsername = proxies[i].AccountUsername
			s.IsBot = proxies[i].IsBot
			s.finalize()
			if s.CharacterName == "" {
				unresolved++
			}
		}
	}

	out := map[string]any{
		"asOf":                   now.UTC().Format(time.RFC3339),
		"topN":                   topN,
		"minRatio":               minRatio,
		"minListings":            minListings,
		"minOutlierListings":     minOutlierListings,
		"scannedListings":        scanned,
		"evaluatedItems":         evaluatedItems,
		"evaluatedListings":      evaluatedListings,
		"totalOutlierListings":   len(result.all),
		"distinctOutlierSellers": distinctSellers,
		"scanCap":                ahOutlierScanCap,
		"truncated":              truncated,
		"unresolved":             unresolved,
		"sellers":                sellers,
	}
	// houseId/faction + minQuality echoed only when the filter was applied (an
	// absent key == no filter).
	if houseID != nil {
		out["houseId"] = *houseID
		out["faction"] = ahHouseFaction(*houseID)
	}
	if minQuality != nil {
		out["minQuality"] = *minQuality
	}
	return out, nil
}

// rollupAhOutlierSellers folds the flagged outlier listings per seller, keeps
// sellers with at least minOutlierListings outliers, ranks them worst-first
// (severity, then outlierListings desc, then totalMarkup desc, then maxRatio
// desc, then sellerGuid asc as a deterministic tiebreaker against Go's
// randomized map iteration), and trims to topN while reporting the pre-trim
// distinct seller count. The returned slice is always non-nil. finalize() is left
// to the caller (after identity resolution) so gold strings + reason are rendered
// only for the sellers that survive the topN cut.
func rollupAhOutlierSellers(outliers []*ahPriceOutlier, minOutlierListings, topN int) ([]*ahOutlierSeller, int) {
	bySeller := map[int64]*ahOutlierSeller{}
	for _, o := range outliers {
		s := bySeller[o.SellerGuid]
		if s == nil {
			s = &ahOutlierSeller{SellerGuid: o.SellerGuid}
			bySeller[o.SellerGuid] = s
		}
		s.fold(o)
	}
	out := make([]*ahOutlierSeller, 0, len(bySeller))
	for _, s := range bySeller {
		if s.OutlierListings < minOutlierListings {
			continue
		}
		// Resolve the seller-level severity now so the sort can key on it; the
		// rest of finalize() (gold strings, reason) waits until after the topN cut.
		if s.HighSeverityCount > 0 {
			s.Severity = "high"
		} else {
			s.Severity = "medium"
		}
		out = append(out, s)
	}
	distinct := len(out)
	sort.Slice(out, func(i, j int) bool {
		ri, rj := ahOutlierSellerSeverityRank(out[i].Severity), ahOutlierSellerSeverityRank(out[j].Severity)
		if ri != rj {
			return ri < rj
		}
		if out[i].OutlierListings != out[j].OutlierListings {
			return out[i].OutlierListings > out[j].OutlierListings
		}
		if out[i].TotalMarkupCopper != out[j].TotalMarkupCopper {
			return out[i].TotalMarkupCopper > out[j].TotalMarkupCopper
		}
		if out[i].MaxRatio != out[j].MaxRatio {
			return out[i].MaxRatio > out[j].MaxRatio
		}
		return out[i].SellerGuid < out[j].SellerGuid
	})
	if len(out) > topN {
		out = out[:topN]
	}
	return out, distinct
}
