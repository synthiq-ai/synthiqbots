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
	ahExpiryTimeout    = 10 * time.Second
	ahExpiryDefaultTop = 15
	ahExpiryMaxTop     = 100
	// ahExpiryScanCap bounds the buckets fold the same way ah_market_summary
	// caps its full-table scan — the LIMIT is bound to scanCap+1 so a market
	// larger than the cap is detectable (truncated=true) without a COUNT(*).
	ahExpiryScanCap = 200000

	ahExpirySecPerHour = 3600
)

// ahExpiry window boundaries (seconds until expiry). A listing lands in the
// first window whose upper bound it does not exceed; expired (secondsUntil<=0)
// is its own terminal bucket. Anchored on the same now-relative comparison
// ah_market_summary uses (expireT vs now.Unix()).
const (
	ahExpiryWindow1h  = 1 * ahExpirySecPerHour
	ahExpiryWindow6h  = 6 * ahExpirySecPerHour
	ahExpiryWindow24h = 24 * ahExpirySecPerHour
)

// ahExpiryWindowOrder is the fixed, pre-seeded bucket order — every window is
// emitted even when empty so a quiet market still surfaces the full timeline
// shape (mirrors the perDatabase pre-seed convention in the db_* family).
var ahExpiryWindowOrder = []string{"expired", "under1h", "under6h", "under24h", "over24h"}

// ahExpiryBucketsQuery is the single-table census scan folded in Go. It counts
// EVERY active auction (no item join) so dangling item_instance pointers don't
// drop a listing from the timeline — the census must total the whole market.
// `time` (expire epoch seconds) is backticked because it collides with the
// reserved word. buyoutprice is copper (0 = auction-only / bid-only).
const ahExpiryBucketsQuery = "SELECT `time`, buyoutprice FROM `auctionhouse`"

// ahExpirySoonestBaseQuery is the actionable top-N list. Unlike the census it
// INNER JOINs item_instance -> acore_world.item_template for item names (a
// dangling guid drops from this list but is still counted in the buckets),
// mirroring the cross-DB hop ah_market_top_items established (ops_ro holds
// SELECT on acore_characters + acore_world).
const ahExpirySoonestBaseQuery = "SELECT ah.itemguid, it.entry, it.name, it.Quality, ah.buyoutprice, ah.houseid, ah.`time`, ah.buyguid " +
	"FROM `auctionhouse` ah " +
	"JOIN `item_instance` ii ON ah.itemguid = ii.guid " +
	"JOIN `acore_world`.`item_template` it ON ii.itemEntry = it.entry"

// classifyExpiryWindow maps seconds-until-expiry to a timeline bucket. Only
// called for rows with a valid (>0) expire epoch — a malformed time<=0 row is
// segregated by the caller so it never inflates the "expired" bucket (the same
// expireT>0 guard ah_market_summary applies to its expiringSoon count).
func classifyExpiryWindow(secondsUntil int64) string {
	switch {
	case secondsUntil <= 0:
		return "expired"
	case secondsUntil <= ahExpiryWindow1h:
		return "under1h"
	case secondsUntil <= ahExpiryWindow6h:
		return "under6h"
	case secondsUntil <= ahExpiryWindow24h:
		return "under24h"
	default:
		return "over24h"
	}
}

// ahExpiryBucket is one expiry window with its count and summed buyout value.
type ahExpiryBucket struct {
	Window            string `json:"window"`
	Count             int    `json:"count"`
	TotalBuyoutCopper int64  `json:"totalBuyoutCopper"`
	TotalBuyoutGold   string `json:"totalBuyoutGold"`
}

// ahExpiryListing is one soonest-expiring auction. secondsUntil is negative for
// an already-expired listing (only present when includeExpired=true).
type ahExpiryListing struct {
	ItemEntry    int64  `json:"itemEntry"`
	ItemName     string `json:"itemName"`
	Quality      int    `json:"quality"`
	QualityName  string `json:"qualityName"`
	BuyoutCopper int64  `json:"buyoutCopper"`
	BuyoutGold   string `json:"buyoutGold"`
	HouseID      int    `json:"houseId"`
	Faction      string `json:"faction"`
	ExpireEpoch  int64  `json:"expireEpoch"`
	SecondsUntil int64  `json:"secondsUntil"`
	Expired      bool   `json:"expired"`
	HasBidder    bool   `json:"hasBidder"`
}

// RegisterAhMarketExpiryTimelineTool registers `ah_market_expiry_timeline` — a
// read-only TIME axis over the auction house that the ah_* family lacked. Every
// other ah_* tool answers a PRICE, COUNT, or COMPOSITION question (summary,
// top_items, top_sellers, category_breakdown, price_percentiles, the two
// price_outliers). None answers "what is about to leave the market": how many
// listings expire in the next hour / 6h / 24h / beyond, how much buyout value
// sits in each window, and which high-value listings expire soonest.
//
// Two facets from one connection: `buckets` — a count + summed buyout per expiry
// window folded in Go from `auctionhouse.time` (a single-table census over the
// whole market); and `soonest` — the top-N earliest-expiring listings with item
// name/quality/faction/hasBidder via the item_instance -> item_template join.
func RegisterAhMarketExpiryTimelineTool(reg *Registry, deps DBDeps) {
	reg.Register(Tool{
		Name: "ah_market_expiry_timeline",
		Description: "Expiry TIME axis over the auction house (the temporal lens the ah_* family lacked — every other " +
			"ah_* tool is a price/count/composition view). Reads acore_characters.auctionhouse over the ops_ro pool " +
			"and returns two facets: `buckets` — a market-wide census counting every active listing into expiry " +
			"windows (expired, under1h, under6h, under24h, over24h) computed from auctionhouse.time (the expire epoch), " +
			"each window carrying count + summed buyout in copper plus a human gold string; and `soonest` — the top-N " +
			"earliest-expiring listings joining auctionhouse -> item_instance -> acore_world.item_template for itemName, " +
			"quality + qualityName (0 Poor .. 7 Heirloom), buyout, houseId + faction, expireEpoch, secondsUntil, expired, " +
			"and hasBidder (buyguid != 0). Answers 'what is about to leave the market / expire unsold' — distinct from " +
			"ah_market_price_outliers (overpriced) or ah_market_top_items (abundant). The census counts ALL auctions " +
			"(dangling item rows included); the soonest list inner-joins so only listings with a resolvable item appear. " +
			"Args: topN (default 15, max 100 — size of the soonest list), houseId (optional — narrow to one auction " +
			"house: 2=Alliance, 6=Horde, 7=Neutral; any other value is rejected, not silently emptied), minBuyout " +
			"(optional copper floor — only listings with buyoutprice at or above it; negative is rejected), " +
			"includeExpired (default false — when true the soonest list also includes already-expired listings; the " +
			"buckets always show the expired window regardless). Read-only, 10 s timeout.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{
			"topN":{"type":"integer","description":"Size of the soonest-expiring list (default 15, max 100)"},
			"houseId":{"type":"integer","description":"Narrow to one auction house (2=Alliance, 6=Horde, 7=Neutral); any other value is rejected"},
			"minBuyout":{"type":"integer","description":"Copper floor — only listings with buyoutprice at or above this value; negative rejected"},
			"includeExpired":{"type":"boolean","description":"When true the soonest list also includes already-expired listings (default false); buckets always show the expired window"}
		}}`),
		Annotations: AnnRead(),
		Handler: func(ctx context.Context, raw json.RawMessage, _ string) any {
			var a struct {
				TopN           int    `json:"topN"`
				HouseID        *int   `json:"houseId"`
				MinBuyout      *int64 `json:"minBuyout"`
				IncludeExpired bool   `json:"includeExpired"`
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
				topN = ahExpiryDefaultTop
			}
			if topN > ahExpiryMaxTop {
				topN = ahExpiryMaxTop
			}
			// houseId is rejected (not clamped/passed-through) when outside the
			// AuctionHouseId enum — an unknown house would return an empty
			// timeline that reads as "no auctions" rather than "bad filter".
			if a.HouseID != nil && *a.HouseID != 2 && *a.HouseID != 6 && *a.HouseID != 7 {
				return map[string]any{"error": fmt.Sprintf("houseId invalid: %d (valid 2=Alliance, 6=Horde, 7=Neutral)", *a.HouseID)}
			}
			// A negative floor is a typo, not a filter — reject rather than let it
			// match every row (buyoutprice is unsigned, so >= negative is a no-op).
			if a.MinBuyout != nil && *a.MinBuyout < 0 {
				return map[string]any{"error": fmt.Sprintf("minBuyout negative: %d (copper floor must be >= 0)", *a.MinBuyout)}
			}
			c, cancel := context.WithTimeout(ctx, ahExpiryTimeout)
			defer cancel()
			out, err := collectAhExpiryTimeline(c, deps.QueryDB, time.Now(), topN, a.HouseID, a.MinBuyout, a.IncludeExpired)
			if err != nil {
				return map[string]any{"error": err.Error()}
			}
			return out
		},
	})
}

// ahExpiryFilters builds the shared houseId/minBuyout WHERE fragment. colPrefix
// is "" for the un-aliased census scan and "ah." for the aliased soonest join,
// so the two facets apply the identical filter over the same rows.
func ahExpiryFilters(colPrefix string, houseID *int, minBuyout *int64) ([]string, []any) {
	var conds []string
	var args []any
	if houseID != nil {
		conds = append(conds, colPrefix+"houseid = ?")
		args = append(args, *houseID)
	}
	if minBuyout != nil {
		conds = append(conds, colPrefix+"buyoutprice >= ?")
		args = append(args, *minBuyout)
	}
	return conds, args
}

// collectAhExpiryTimeline runs the census fold then the soonest list on one
// connection. Split out so tests can drive it with sqlmock and a fixed `now`
// (every window classification and secondsUntil is now-relative). The soonest
// SQL ORDER BY is the source of truth for that list's order — the handler does
// not re-sort (matching ah_market_top_items / wow_failed_logins_top).
func collectAhExpiryTimeline(ctx context.Context, db *sql.DB, now time.Time, topN int, houseID *int, minBuyout *int64, includeExpired bool) (map[string]any, error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire conn: %w", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "USE `acore_characters`"); err != nil {
		return nil, fmt.Errorf("USE acore_characters: %w", err)
	}
	nowEpoch := now.Unix()

	// --- Facet 1: buckets census (single-table, Go-side fold). ---
	bConds, bArgs := ahExpiryFilters("", houseID, minBuyout)
	bucketsQuery := ahExpiryBucketsQuery
	if len(bConds) > 0 {
		bucketsQuery += " WHERE " + strings.Join(bConds, " AND ")
	}
	bucketsQuery += " LIMIT ?"
	bArgs = append(bArgs, ahExpiryScanCap+1)

	bRows, err := conn.QueryContext(ctx, bucketsQuery, bArgs...)
	if err != nil {
		return nil, fmt.Errorf("buckets query: %w", err)
	}
	defer bRows.Close()

	counts := make(map[string]int, len(ahExpiryWindowOrder))
	sums := make(map[string]int64, len(ahExpiryWindowOrder))
	var scanned, malformed int
	var truncated bool
	for bRows.Next() {
		if scanned >= ahExpiryScanCap {
			truncated = true
			break
		}
		var expireT, buyout int64
		if err := bRows.Scan(&expireT, &buyout); err != nil {
			return nil, fmt.Errorf("buckets scan: %w", err)
		}
		scanned++
		// A malformed zero (or negative) expire epoch is not a genuinely-expired
		// auction — segregate it so the expired bucket stays trustworthy.
		if expireT <= 0 {
			malformed++
			continue
		}
		w := classifyExpiryWindow(expireT - nowEpoch)
		counts[w]++
		sums[w] += buyout
	}
	if err := bRows.Err(); err != nil {
		return nil, fmt.Errorf("buckets iter: %w", err)
	}
	bRows.Close()

	buckets := make([]*ahExpiryBucket, 0, len(ahExpiryWindowOrder))
	var bucketed int
	for _, w := range ahExpiryWindowOrder {
		buckets = append(buckets, &ahExpiryBucket{
			Window:            w,
			Count:             counts[w],
			TotalBuyoutCopper: sums[w],
			TotalBuyoutGold:   formatCopper(sums[w]),
		})
		bucketed += counts[w]
	}

	// --- Facet 2: soonest-expiring list (item join, SQL ORDER BY). ---
	sConds, sArgs := ahExpiryFilters("ah.", houseID, minBuyout)
	// A time lower bound is always applied: exclude expired (time > now) by
	// default, else exclude only malformed time<=0 rows so the includeExpired
	// list can't be led by a bogus epoch-0 listing.
	if includeExpired {
		sConds = append(sConds, "ah.`time` > ?")
		sArgs = append(sArgs, int64(0))
	} else {
		sConds = append(sConds, "ah.`time` > ?")
		sArgs = append(sArgs, nowEpoch)
	}
	soonestQuery := ahExpirySoonestBaseQuery + " WHERE " + strings.Join(sConds, " AND ") +
		" ORDER BY ah.`time` ASC, ah.itemguid ASC LIMIT ?"
	sArgs = append(sArgs, topN)

	sRows, err := conn.QueryContext(ctx, soonestQuery, sArgs...)
	if err != nil {
		return nil, fmt.Errorf("soonest query: %w", err)
	}
	defer sRows.Close()

	soonest := make([]*ahExpiryListing, 0, topN)
	for sRows.Next() {
		var (
			itemguid, entry, buyout, expireT, buyguid int64
			houseIDv, quality                         int
			name                                      string
		)
		if err := sRows.Scan(&itemguid, &entry, &name, &quality, &buyout, &houseIDv, &expireT, &buyguid); err != nil {
			return nil, fmt.Errorf("soonest scan: %w", err)
		}
		secs := expireT - nowEpoch
		soonest = append(soonest, &ahExpiryListing{
			ItemEntry:    entry,
			ItemName:     name,
			Quality:      quality,
			QualityName:  itemQualityName(quality),
			BuyoutCopper: buyout,
			BuyoutGold:   formatCopper(buyout),
			HouseID:      houseIDv,
			Faction:      ahHouseFaction(houseIDv),
			ExpireEpoch:  expireT,
			SecondsUntil: secs,
			Expired:      secs <= 0,
			HasBidder:    buyguid != 0,
		})
	}
	if err := sRows.Err(); err != nil {
		return nil, fmt.Errorf("soonest iter: %w", err)
	}

	out := map[string]any{
		"asOf":              now.UTC().Format(time.RFC3339),
		"nowEpoch":          nowEpoch,
		"topN":              topN,
		"includeExpired":    includeExpired,
		"totalAuctions":     bucketed,
		"malformedTimeRows": malformed,
		"scanCap":           ahExpiryScanCap,
		"truncated":         truncated,
		"buckets":           buckets,
		"soonest":           soonest,
		"distinctSoonest":   len(soonest),
	}
	// Filters are echoed only when applied, so an absent key == no filter.
	if houseID != nil {
		out["houseId"] = *houseID
		out["faction"] = ahHouseFaction(*houseID)
	}
	if minBuyout != nil {
		out["minBuyout"] = *minBuyout
	}
	return out, nil
}
