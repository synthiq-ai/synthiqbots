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
	ahTopSellersTimeout     = 10 * time.Second
	ahTopSellersScanCap     = 200000 // hard ceiling on auctionhouse rows folded into one snapshot
	ahTopSellersDefaultTopN = 25
	ahTopSellersMaxTopN     = 200
	ahTopSellersDefaultMin  = 1        // surface only sellers with at least one listing
	ahTopSellersBotPrefix   = "RNDBOT" // mod-playerbots' default RandomBotAccountPrefix
)

// ahTopSellersScanBase is the per-listing scan over acore_characters.auctionhouse.
// Only the columns this tool folds per seller — houseid (for the optional
// faction filter), itemowner (seller characters.guid), buyoutprice (copper, 0 =
// auction-only), lastbid (current high bid), deposit, buyguid (current high
// bidder, 0 = none). Narrower than ah_market_summary's scan (no startbid / time)
// because this tool ranks sellers, not the expiring-soon window. An optional
// `WHERE houseid = ?` and the trailing `LIMIT ?` (bound to scanCap+1 so
// truncation is detectable without a separate COUNT) are appended by the builder.
const ahTopSellersScanBase = "SELECT houseid, itemowner, buyoutprice, lastbid, deposit, buyguid FROM `auctionhouse`"

// ahSeller is one ranked seller. The itemOwnerGuid is the auctionhouse.itemowner
// (a characters.guid); characterName / accountId / accountUsername / isBot are
// folded in from a batch lookup against acore_characters.characters +
// acore_auth.account (empty name + accountId 0 when the owning character row is
// gone — e.g. a deleted character whose listings outlive it). The copper sums
// carry human gold strings alongside because a seller leaderboard is meant to be
// eyeballed (unlike ah_market_summary's lean machine-readable byHouse facet).
type ahSeller struct {
	ItemOwnerGuid         int64  `json:"itemOwnerGuid"`
	CharacterName         string `json:"characterName"`
	AccountID             int64  `json:"accountId"`
	AccountUsername       string `json:"accountUsername"`
	IsBot                 bool   `json:"isBot"`
	Listings              int    `json:"listings"`
	WithBuyout            int    `json:"withBuyout"`
	WithActiveBid         int    `json:"withActiveBid"`
	TotalBuyoutCopper     int64  `json:"totalBuyoutCopper"`
	TotalBuyoutGold       string `json:"totalBuyoutGold"`
	TotalCurrentBidCopper int64  `json:"totalCurrentBidCopper"`
	TotalCurrentBidGold   string `json:"totalCurrentBidGold"`
	TotalDepositCopper    int64  `json:"totalDepositCopper"`
	TotalDepositGold      string `json:"totalDepositGold"`
	MinBuyoutCopper       int64  `json:"minBuyoutCopper"`
	MinBuyoutGold         string `json:"minBuyoutGold"`

	// hasBuyout tracks whether any buyout>0 listing was folded so the floor
	// (MinBuyoutCopper) starts from the first real buyout rather than 0.
	// Unexported -> not serialized.
	hasBuyout bool
}

// fold adds one auctionhouse row to the seller's running totals.
func (s *ahSeller) fold(buyout, lastbid, deposit, buyguid int64) {
	s.Listings++
	s.TotalBuyoutCopper += buyout
	s.TotalCurrentBidCopper += lastbid
	s.TotalDepositCopper += deposit
	if buyout > 0 {
		s.WithBuyout++
		// Floor = cheapest active buyout this seller is posting. Auction-only
		// (buyout 0) rows are excluded so the floor reflects buy-now price.
		if !s.hasBuyout || buyout < s.MinBuyoutCopper {
			s.MinBuyoutCopper = buyout
		}
		s.hasBuyout = true
	}
	if buyguid != 0 {
		s.WithActiveBid++
	}
}

// finalizeGold renders the human gold strings once, after the top-N slice, so we
// don't format copper for sellers that never make the cut.
func (s *ahSeller) finalizeGold() {
	s.TotalBuyoutGold = formatCopper(s.TotalBuyoutCopper)
	s.TotalCurrentBidGold = formatCopper(s.TotalCurrentBidCopper)
	s.TotalDepositGold = formatCopper(s.TotalDepositCopper)
	s.MinBuyoutGold = formatCopper(s.MinBuyoutCopper)
}

// RegisterAhMarketTopSellersTool registers `ah_market_top_sellers` — a read-only
// ranking of the heaviest auction-house sellers by active-listing count, with
// character-name + bot-vs-player resolution folded in.
//
// The WHO view of the AH. ah_market_summary (PR #229) folds the market into
// faction-level TOTALS (how big / the split / value locked up) and carries a lean
// topSellers facet that is itemowner GUIDS ONLY — no name, no bot flag.
// ah_market_top_items (the item-side sibling) ranks item TYPES. This promotes the
// seller facet into a dedicated, richer tool: per-seller listings / withBuyout /
// withActiveBid / total buyout|bid|deposit / floor buyout, plus a
// characters.name + acore_auth.account.username lookup that flags whether each
// top seller is an AHBot (RNDBOT prefix) or a real player. On a mod-ah-bot server
// the heaviest sellers are usually AHBot characters, so "is the top of the market
// bots or players, and which guids?" is the recurring operator question this
// answers without a hand-written cross-DB join.
func RegisterAhMarketTopSellersTool(reg *Registry, deps DBDeps) {
	reg.Register(Tool{
		Name: "ah_market_top_sellers",
		Description: "Top-N auction-house sellers by active-listing count, aggregated from " +
			"acore_characters.auctionhouse over the ops_ro pool, with character-name + bot-vs-player " +
			"resolution. Three-stage composite: (1) fold every active listing per itemowner in Go from one " +
			"full-table scan, (2) batch-resolve the top-N itemowner guids to characters.name + account via an " +
			"IN(...) query on acore_characters.characters, (3) batch-resolve those accounts to " +
			"acore_auth.account.username in one IN(...) query and flag isBot when the username starts with " +
			"\"RNDBOT\" (mod-playerbots' RandomBotAccountPrefix). The WHO view of the AH: ah_market_summary " +
			"folds the market into faction TOTALS and carries only a lean itemowner-guid topSellers facet (no " +
			"name, no bot flag); this promotes that into a ranked leaderboard with per-seller listings, " +
			"withBuyout, withActiveBid, total buyout/currentBid/deposit (copper + human gold strings), and the " +
			"floor (cheapest active buyout). On a mod-ah-bot server the heaviest sellers are usually AHBot " +
			"characters — isBot makes that visible without a separate wow_player_lookup. A seller whose owning " +
			"character row is gone (deleted character with surviving listings) surfaces with an empty " +
			"characterName + accountId 0 and is counted in `unresolved`. Args: topN (default 25, max 200), " +
			"houseId (optional — restrict to one auction house; 2=Alliance, 6=Horde, 7=Neutral, echoed with " +
			"its faction), minListings (default 1, clamps to >=1 — only sellers with at least this many active " +
			"listings). Scan capped at 200000 rows; truncated:true flags an incomplete fold. Read-only, " +
			"10 s timeout.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{
			"topN":{"type":"integer","description":"Max sellers to rank (default 25, max 200)"},
			"houseId":{"type":"integer","description":"Restrict to one auction house by houseid (2=Alliance, 6=Horde, 7=Neutral); omit for all houses"},
			"minListings":{"type":"integer","description":"Only rank sellers with at least this many active listings (default 1, clamps to >=1)"}
		}}`),
		Annotations: AnnRead(),
		Handler: func(ctx context.Context, raw json.RawMessage, _ string) any {
			var a struct {
				TopN        int  `json:"topN"`
				HouseID     *int `json:"houseId"`
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
				topN = ahTopSellersDefaultTopN
			}
			if topN > ahTopSellersMaxTopN {
				topN = ahTopSellersMaxTopN
			}
			minListings := a.MinListings
			if minListings < ahTopSellersDefaultMin {
				minListings = ahTopSellersDefaultMin
			}
			c, cancel := context.WithTimeout(ctx, ahTopSellersTimeout)
			defer cancel()
			out, err := collectAhTopSellers(c, deps.QueryDB, time.Now(), a.HouseID, minListings, topN)
			if err != nil {
				return map[string]any{"error": err.Error()}
			}
			return out
		},
	})
}

// collectAhTopSellers runs the three-stage composite. Split out so tests can
// drive it with sqlmock and a fixed `now`. houseID is *int so "unset" (all
// houses) is distinguishable from an explicit house filter.
func collectAhTopSellers(ctx context.Context, db *sql.DB, now time.Time, houseID *int, minListings, topN int) (map[string]any, error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire conn: %w", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "USE `acore_characters`"); err != nil {
		return nil, fmt.Errorf("USE acore_characters: %w", err)
	}

	// Stage 1: scan + fold per seller.
	q := ahTopSellersScanBase
	args := make([]any, 0, 2)
	if houseID != nil {
		q += " WHERE houseid = ?"
		args = append(args, *houseID)
	}
	q += " LIMIT ?"
	args = append(args, ahTopSellersScanCap+1)

	rows, err := conn.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("auctionhouse query: %w", err)
	}
	defer rows.Close()

	sellers := map[int64]*ahSeller{}
	scanned := 0
	truncated := false
	for rows.Next() {
		if scanned >= ahTopSellersScanCap {
			truncated = true
			break
		}
		var houseIDRow int
		var itemOwner, buyout, lastbid, deposit, buyguid int64
		if err := rows.Scan(&houseIDRow, &itemOwner, &buyout, &lastbid, &deposit, &buyguid); err != nil {
			return nil, fmt.Errorf("auctionhouse scan: %w", err)
		}
		scanned++
		s := sellers[itemOwner]
		if s == nil {
			s = &ahSeller{ItemOwnerGuid: itemOwner}
			sellers[itemOwner] = s
		}
		s.fold(buyout, lastbid, deposit, buyguid)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("auctionhouse iter: %w", err)
	}

	// Stage 2: minListings filter, sort listings-desc, slice to topN.
	top, distinct := topNAhMarketSellers(sellers, minListings, topN)

	// Stage 3: resolve names (still in acore_characters) then accounts (acore_auth).
	unresolved := 0
	if len(top) > 0 {
		if err := annotateSellerCharacters(ctx, conn, top); err != nil {
			return nil, err
		}
		if _, err := conn.ExecContext(ctx, "USE `acore_auth`"); err != nil {
			return nil, fmt.Errorf("USE acore_auth: %w", err)
		}
		if err := annotateSellerAccounts(ctx, conn, top); err != nil {
			return nil, err
		}
		for _, s := range top {
			s.finalizeGold()
			if s.CharacterName == "" {
				unresolved++
			}
		}
	}

	out := map[string]any{
		"asOf":            now.UTC().Format(time.RFC3339),
		"topN":            topN,
		"minListings":     minListings,
		"distinctSellers": distinct,
		"scanned":         scanned,
		"scanCap":         ahTopSellersScanCap,
		"truncated":       truncated,
		"unresolved":      unresolved,
		"sellers":         top,
	}
	if houseID != nil {
		out["houseId"] = *houseID
		out["faction"] = ahHouseFaction(*houseID)
	}
	return out, nil
}

// topNAhMarketSellers filters the folded sellers to those with >= minListings,
// sorts listings-desc with itemOwnerGuid-asc as a deterministic tiebreaker
// (without it Go's randomized map iteration would flake tests on tied counts),
// slices to n, and returns the slice plus the pre-slice distinct count (so the
// caller can report "ranking the top 25 of 540 sellers"). Always a non-nil slice.
func topNAhMarketSellers(m map[int64]*ahSeller, minListings, n int) ([]*ahSeller, int) {
	out := make([]*ahSeller, 0, len(m))
	for _, s := range m {
		if s.Listings >= minListings {
			out = append(out, s)
		}
	}
	distinct := len(out)
	sort.Slice(out, func(i, j int) bool {
		if out[i].Listings != out[j].Listings {
			return out[i].Listings > out[j].Listings
		}
		return out[i].ItemOwnerGuid < out[j].ItemOwnerGuid
	})
	if len(out) > n {
		out = out[:n]
	}
	return out, distinct
}

// annotateSellerCharacters batch-resolves the top sellers' itemowner guids to
// characters.name + account in one IN(...) query. Mirrors the wow_* GM-annotation
// batch-IN idiom (wow_failed_logins_top / wow_recent_logins): one round trip, a
// guid->row map, then a second pass to set the fields. A guid with no row (a
// deleted character whose listings outlive it) is left with an empty name +
// account 0 — surfaced via the top-level `unresolved` count, not dropped.
func annotateSellerCharacters(ctx context.Context, conn *sql.Conn, sellers []*ahSeller) error {
	if len(sellers) == 0 {
		return nil
	}
	var b strings.Builder
	b.Grow(64 + len(sellers)*2)
	b.WriteString("SELECT guid, name, account FROM `characters` WHERE guid IN (")
	args := make([]any, 0, len(sellers))
	for i, s := range sellers {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteByte('?')
		args = append(args, s.ItemOwnerGuid)
	}
	b.WriteByte(')')
	rs, err := conn.QueryContext(ctx, b.String(), args...)
	if err != nil {
		return fmt.Errorf("characters query: %w", err)
	}
	defer rs.Close()
	type charRow struct {
		name    string
		account int64
	}
	byGuid := make(map[int64]charRow, len(sellers))
	for rs.Next() {
		var guid, account int64
		var name string
		if err := rs.Scan(&guid, &name, &account); err != nil {
			return fmt.Errorf("characters scan: %w", err)
		}
		byGuid[guid] = charRow{name: name, account: account}
	}
	if err := rs.Err(); err != nil {
		return fmt.Errorf("characters iter: %w", err)
	}
	for _, s := range sellers {
		if c, ok := byGuid[s.ItemOwnerGuid]; ok {
			s.CharacterName = c.name
			s.AccountID = c.account
		}
	}
	return nil
}

// annotateSellerAccounts batch-resolves the resolved sellers' account ids to
// acore_auth.account.username in one IN(...) query and flags isBot when the
// username carries the RNDBOT prefix (case-insensitive). Sellers whose character
// row was missing (account 0) are skipped — they carry no username and isBot
// stays false. Distinct account ids are deduped so two seller-characters on the
// same account bind the id once.
func annotateSellerAccounts(ctx context.Context, conn *sql.Conn, sellers []*ahSeller) error {
	ids := make([]int64, 0, len(sellers))
	seen := make(map[int64]bool, len(sellers))
	for _, s := range sellers {
		if s.AccountID != 0 && !seen[s.AccountID] {
			seen[s.AccountID] = true
			ids = append(ids, s.AccountID)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	var b strings.Builder
	b.Grow(48 + len(ids)*2)
	b.WriteString("SELECT id, username FROM `account` WHERE id IN (")
	args := make([]any, 0, len(ids))
	for i, id := range ids {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteByte('?')
		args = append(args, id)
	}
	b.WriteByte(')')
	rs, err := conn.QueryContext(ctx, b.String(), args...)
	if err != nil {
		return fmt.Errorf("account query: %w", err)
	}
	defer rs.Close()
	byID := make(map[int64]string, len(ids))
	for rs.Next() {
		var id int64
		var username string
		if err := rs.Scan(&id, &username); err != nil {
			return fmt.Errorf("account scan: %w", err)
		}
		byID[id] = username
	}
	if err := rs.Err(); err != nil {
		return fmt.Errorf("account iter: %w", err)
	}
	for _, s := range sellers {
		if u, ok := byID[s.AccountID]; ok {
			s.AccountUsername = u
			s.IsBot = strings.HasPrefix(strings.ToUpper(u), ahTopSellersBotPrefix)
		}
	}
	return nil
}
