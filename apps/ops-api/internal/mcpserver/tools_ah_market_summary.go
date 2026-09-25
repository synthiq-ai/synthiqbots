package mcpserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"time"
)

const (
	ahMarketSummaryTimeout        = 10 * time.Second
	ahMarketSummaryScanCap        = 200000 // hard ceiling on rows folded into one snapshot
	ahMarketSummaryDefaultExpireH = 2      // expiring-soon window default (hours)
	ahMarketSummaryMaxExpireH     = 168    // 7d cap on the expiring-soon window
	ahMarketSummaryDefaultTopSell = 10
	ahMarketSummaryMaxTopSell     = 50

	copperPerSilver = 100
	copperPerGold   = 10000
)

// ahMarketSummaryQuery is the single full-table scan. acore_characters.auctionhouse
// columns are anchored on the canonical AzerothCore schema
// (data/sql/base/db_characters/auctionhouse.sql): houseid (tinyint, the
// AuctionHouseId), itemowner (seller characters.guid), buyoutprice (copper, 0 =
// auction-only), lastbid (current high bid copper, 0 = none), startbid, deposit,
// time (expire epoch seconds), buyguid (current high bidder, 0 = none). `time`
// is backticked because it collides with the reserved word. LIMIT is bound to
// scanCap+1 so truncation is detectable without a separate COUNT(*).
const ahMarketSummaryQuery = "SELECT houseid, itemowner, buyoutprice, lastbid, startbid, deposit, `time`, buyguid FROM `auctionhouse` LIMIT ?"

// ahHouseFaction maps the persisted auctionhouse.houseid (tinyint) to a faction
// label. Anchored on AzerothCore's AuctionHouseId enum {Alliance=2, Horde=6,
// Neutral=7}; the column DEFAULTs to 7 and when
// CONFIG_ALLOW_TWO_SIDE_INTERACTION_AUCTION is enabled every listing collapses
// to the neutral house. An unrecognized id is surfaced as "house-<id>" rather
// than dropped — a config/expansion that introduces a new house won't vanish
// from the snapshot silently.
func ahHouseFaction(houseID int) string {
	switch houseID {
	case 2:
		return "Alliance"
	case 6:
		return "Horde"
	case 7:
		return "Neutral"
	default:
		return fmt.Sprintf("house-%d", houseID)
	}
}

// formatCopper renders a copper amount as a WoW money string "Ng Ms Kc"
// (1 gold = 100 silver = 10000 copper), omitting zero-valued denominations.
// Zero (or negative — a defensive guard) renders "0c". Mirrors the client money
// display so an operator can sanity-check totals against in-game values.
func formatCopper(c int64) string {
	if c <= 0 {
		return "0c"
	}
	gold := c / copperPerGold
	silver := (c % copperPerGold) / copperPerSilver
	copper := c % copperPerSilver
	out := ""
	if gold > 0 {
		out += fmt.Sprintf("%dg", gold)
	}
	if silver > 0 {
		out += fmt.Sprintf("%ds", silver)
	}
	if copper > 0 {
		out += fmt.Sprintf("%dc", copper)
	}
	return out
}

// ahPct returns n as a percentage of total, rounded to one decimal. Guards
// total<=0 -> 0 so an empty market doesn't divide by zero.
func ahPct(n, total int) float64 {
	if total <= 0 {
		return 0
	}
	return math.Round(float64(n)/float64(total)*1000) / 10
}

// ahHouseBucket is the per-faction slice of the snapshot. Copper sums are
// machine-readable int64; the top-level `totals` block carries the human gold
// strings so per-house rows stay lean (the snapshot is meant to be parsed, not
// just eyeballed, and byHouse is the faction-split facet).
type ahHouseBucket struct {
	HouseID               int    `json:"houseId"`
	Faction               string `json:"faction"`
	Listings              int    `json:"listings"`
	WithBuyout            int    `json:"withBuyout"`
	WithActiveBid         int    `json:"withActiveBid"`
	ExpiringSoon          int    `json:"expiringSoon"`
	TotalBuyoutCopper     int64  `json:"totalBuyoutCopper"`
	TotalCurrentBidCopper int64  `json:"totalCurrentBidCopper"`
	TotalStartBidCopper   int64  `json:"totalStartBidCopper"`
	TotalDepositCopper    int64  `json:"totalDepositCopper"`
}

// ahTopSeller is one entry of the top-N sellers-by-listing-count list. The
// itemOwnerGuid is a characters.guid — resolve a name with wow_player_lookup.
// On a mod-ah-bot server the heaviest sellers are typically AHBot characters,
// so surfacing guid + count makes "the market is dominated by bot listings from
// guid X" visible without a separate query.
type ahTopSeller struct {
	ItemOwnerGuid     int64 `json:"itemOwnerGuid"`
	Listings          int   `json:"listings"`
	TotalBuyoutCopper int64 `json:"totalBuyoutCopper"`
}

// RegisterAhMarketSummaryTool registers `ah_market_summary` — a read-only,
// market-wide snapshot of the auction house aggregated from
// acore_characters.auctionhouse over the ops_ro pool.
//
// This is the DB-side companion to the bot-facing `bot_ah_search` (which walks
// the worldserver's in-memory AuctionEntryMap for live price discovery on a
// single item). The ops-api sidecar is a separate Go process with no access to
// that in-memory map, so it reads the persisted table directly and folds the
// whole market into one snapshot in a single scan — answering the operator's
// first market questions (how big is the AH, how is it split by faction, how
// much value is locked up, how many listings carry bids/buyouts, what's expiring
// soon, who are the biggest sellers) without a hand-written db_query + GROUP BY.
//
// Single-table by design: item-name / item-type breakdowns need a cross-DB join
// to item_instance + acore_world.item_template and are left to a follow-up
// (`ah_market_top_items`) so this stays a clean, schema-confident snapshot.
func RegisterAhMarketSummaryTool(reg *Registry, deps DBDeps) {
	reg.Register(Tool{
		Name: "ah_market_summary",
		Description: "Auction-house market snapshot aggregated from acore_characters.auctionhouse over the " +
			"ops_ro pool — every active listing folded in Go from one full-table scan, no joins. Top-level " +
			"`totals`: listings, withBuyout (+pct), withActiveBid (+pct), expiringSoon, and copper sums for " +
			"buyout / currentBid / startBid / deposit (with human gold strings on the headline three). " +
			"`byHouse` splits the same metrics per auction house — houseid 2=Alliance, 6=Horde, 7=Neutral " +
			"(the AzerothCore AuctionHouseId enum; an unknown id surfaces as house-<id> rather than " +
			"vanishing). When CONFIG_ALLOW_TWO_SIDE_INTERACTION_AUCTION is on every listing collapses to the " +
			"neutral house. `topSellers` lists the top-N itemowner character guids by listing count (resolve " +
			"names via wow_player_lookup — on a mod-ah-bot server the heaviest sellers are usually AHBot " +
			"characters). The DB-side overview companion to the bot-facing bot_ah_search (live per-item price " +
			"discovery). Args: expiringWindowHours (default 2, max 168 — counts listings whose expire time " +
			"falls within now+window), topSellers (default 10, max 50, set 0 to omit). Scan capped at 200000 " +
			"rows; truncated:true flags an incomplete fold. Read-only, 10 s timeout.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{
			"expiringWindowHours":{"type":"integer","description":"Window in hours for the expiringSoon bucket — counts listings whose expire time falls within now+window (default 2, max 168)"},
			"topSellers":{"type":"integer","description":"Max entries in the topSellers (by listing count) list (default 10, max 50; set 0 to omit the list)"}
		}}`),
		Annotations: AnnRead(),
		Handler: func(ctx context.Context, raw json.RawMessage, _ string) any {
			var a struct {
				ExpiringWindowHours int  `json:"expiringWindowHours"`
				TopSellers          *int `json:"topSellers"`
			}
			if len(raw) > 0 && string(raw) != "null" {
				if err := json.Unmarshal(raw, &a); err != nil {
					return map[string]any{"error": "decode args: " + err.Error()}
				}
			}
			if deps.QueryDB == nil {
				return map[string]any{"error": "ops_ro pool not configured (DB_DSN empty?)"}
			}
			expireH := a.ExpiringWindowHours
			if expireH <= 0 {
				expireH = ahMarketSummaryDefaultExpireH
			}
			if expireH > ahMarketSummaryMaxExpireH {
				expireH = ahMarketSummaryMaxExpireH
			}
			// *int so an explicit 0 ("omit the list") is distinguishable from
			// unset ("use the default 10").
			topSellers := ahMarketSummaryDefaultTopSell
			if a.TopSellers != nil {
				topSellers = *a.TopSellers
			}
			if topSellers < 0 {
				topSellers = 0
			}
			if topSellers > ahMarketSummaryMaxTopSell {
				topSellers = ahMarketSummaryMaxTopSell
			}
			c, cancel := context.WithTimeout(ctx, ahMarketSummaryTimeout)
			defer cancel()
			out, err := collectAhMarketSummary(c, deps.QueryDB, time.Now(), time.Duration(expireH)*time.Hour, topSellers)
			if err != nil {
				return map[string]any{"error": err.Error()}
			}
			return out
		},
	})
}

// collectAhMarketSummary runs the single full-table scan and folds it into the
// per-house buckets, totals, and top-N sellers. Split out so tests can drive it
// with sqlmock and a fixed `now` (the expiringSoon window is now-relative).
//
// Aggregation is in Go (not a SQL GROUP BY) so the byHouse split, the totals
// fold, the expiringSoon count, and the top-N sellers all come from ONE round
// trip — the same single-scan-fold pattern db_connection_summary uses.
func collectAhMarketSummary(ctx context.Context, db *sql.DB, now time.Time, expireWindow time.Duration, topSellers int) (map[string]any, error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire conn: %w", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "USE `acore_characters`"); err != nil {
		return nil, fmt.Errorf("USE acore_characters: %w", err)
	}

	rows, err := conn.QueryContext(ctx, ahMarketSummaryQuery, ahMarketSummaryScanCap+1)
	if err != nil {
		return nil, fmt.Errorf("auctionhouse query: %w", err)
	}
	defer rows.Close()

	expireCutoff := now.Unix() + int64(expireWindow.Seconds())
	houses := map[int]*ahHouseBucket{}
	sellers := map[int64]*ahTopSeller{}
	scanned := 0
	truncated := false
	for rows.Next() {
		if scanned >= ahMarketSummaryScanCap {
			truncated = true
			break
		}
		var houseID int
		var itemOwner, buyout, lastbid, startbid, deposit, expireT, buyguid int64
		if err := rows.Scan(&houseID, &itemOwner, &buyout, &lastbid, &startbid, &deposit, &expireT, &buyguid); err != nil {
			return nil, fmt.Errorf("auctionhouse scan: %w", err)
		}
		scanned++

		h := houses[houseID]
		if h == nil {
			h = &ahHouseBucket{HouseID: houseID, Faction: ahHouseFaction(houseID)}
			houses[houseID] = h
		}
		h.Listings++
		h.TotalBuyoutCopper += buyout
		h.TotalCurrentBidCopper += lastbid
		h.TotalStartBidCopper += startbid
		h.TotalDepositCopper += deposit
		if buyout > 0 {
			h.WithBuyout++
		}
		if buyguid != 0 {
			h.WithActiveBid++
		}
		// expiringSoon: listing expires within [now, now+window]. The expireT>0
		// guard keeps a malformed zero-time row from inflating the count (0 would
		// otherwise always be <= cutoff).
		if expireT > 0 && expireT <= expireCutoff {
			h.ExpiringSoon++
		}

		if topSellers > 0 {
			s := sellers[itemOwner]
			if s == nil {
				s = &ahTopSeller{ItemOwnerGuid: itemOwner}
				sellers[itemOwner] = s
			}
			s.Listings++
			s.TotalBuyoutCopper += buyout
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("auctionhouse iter: %w", err)
	}

	// byHouse: deterministic order by houseId asc (small fixed set).
	byHouse := make([]*ahHouseBucket, 0, len(houses))
	for _, h := range houses {
		byHouse = append(byHouse, h)
	}
	sort.Slice(byHouse, func(i, j int) bool { return byHouse[i].HouseID < byHouse[j].HouseID })

	// totals: fold across the buckets so the headline numbers can't drift from
	// byHouse (a single source of truth for both).
	tot := ahHouseBucket{}
	for _, h := range byHouse {
		tot.Listings += h.Listings
		tot.WithBuyout += h.WithBuyout
		tot.WithActiveBid += h.WithActiveBid
		tot.ExpiringSoon += h.ExpiringSoon
		tot.TotalBuyoutCopper += h.TotalBuyoutCopper
		tot.TotalCurrentBidCopper += h.TotalCurrentBidCopper
		tot.TotalStartBidCopper += h.TotalStartBidCopper
		tot.TotalDepositCopper += h.TotalDepositCopper
	}

	out := map[string]any{
		"asOf":                now.UTC().Format(time.RFC3339),
		"expiringWindowHours": int(expireWindow.Hours()),
		"scanned":             scanned,
		"scanCap":             ahMarketSummaryScanCap,
		"truncated":           truncated,
		"byHouse":             byHouse,
		"totals": map[string]any{
			"listings":              tot.Listings,
			"withBuyout":            tot.WithBuyout,
			"withBuyoutPct":         ahPct(tot.WithBuyout, tot.Listings),
			"withActiveBid":         tot.WithActiveBid,
			"withActiveBidPct":      ahPct(tot.WithActiveBid, tot.Listings),
			"expiringSoon":          tot.ExpiringSoon,
			"totalBuyoutCopper":     tot.TotalBuyoutCopper,
			"totalBuyoutGold":       formatCopper(tot.TotalBuyoutCopper),
			"totalCurrentBidCopper": tot.TotalCurrentBidCopper,
			"totalCurrentBidGold":   formatCopper(tot.TotalCurrentBidCopper),
			"totalStartBidCopper":   tot.TotalStartBidCopper,
			"totalDepositCopper":    tot.TotalDepositCopper,
			"totalDepositGold":      formatCopper(tot.TotalDepositCopper),
		},
	}
	if topSellers > 0 {
		out["topSellers"] = topNAhSellers(sellers, topSellers)
	}
	return out, nil
}

// topNAhSellers materializes the seller map as a slice sorted by listing count
// desc, with itemOwnerGuid asc as a deterministic tiebreaker (without it Go's
// randomized map iteration would flake tests on tied counts), then slices to n.
func topNAhSellers(m map[int64]*ahTopSeller, n int) []*ahTopSeller {
	out := make([]*ahTopSeller, 0, len(m))
	for _, s := range m {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Listings != out[j].Listings {
			return out[i].Listings > out[j].Listings
		}
		return out[i].ItemOwnerGuid < out[j].ItemOwnerGuid
	})
	if len(out) > n {
		out = out[:n]
	}
	return out
}
