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
	ahBidderTimeout           = 10 * time.Second
	ahBidderScanCap           = 200000 // hard ceiling on live-bid auctionhouse rows folded into one snapshot
	ahBidderDefaultTopN       = 10
	ahBidderMaxTopN           = 50
	ahBidderDefaultMinLeading = 1        // surface only bidders currently leading at least one auction
	ahBidderBotPrefix         = "RNDBOT" // mod-playerbots' default RandomBotAccountPrefix
)

// ahBidderScanBase is the per-listing scan over acore_characters.auctionhouse,
// PRE-FILTERED to rows with an active leading bid (`buyguid <> 0`). buyguid is the
// characters.guid of the current high bidder (0 when nobody has bid), so this is
// the DEMAND side of the house — every returned row is an auction someone is
// currently winning. Only the columns folded per bidder: houseid (for the
// optional faction filter), buyguid (the bidder = group key), lastbid (that
// bidder's current committed high bid, copper), buyoutprice (the listing's
// buy-now price, copper, 0 = auction-only). An optional `AND houseid = ?` and the
// trailing `LIMIT ?` (bound to scanCap+1 so truncation is detectable without a
// separate COUNT) are appended by the builder.
const ahBidderScanBase = "SELECT houseid, buyguid, lastbid, buyoutprice FROM `auctionhouse` WHERE buyguid <> 0"

// ahBidder is one ranked bidder. The bidderGuid is the auctionhouse.buyguid (a
// characters.guid); characterName / accountId / accountUsername / isBot are folded
// in from a batch lookup against acore_characters.characters + acore_auth.account
// (empty name + accountId 0 when the bidding character row is gone — e.g. a
// deleted character whose leading bids outlive it). The copper sums carry human
// gold strings alongside because a bidder leaderboard is meant to be eyeballed.
type ahBidder struct {
	BidderGuid              int64  `json:"bidderGuid"`
	CharacterName           string `json:"characterName"`
	AccountID               int64  `json:"accountId"`
	AccountUsername         string `json:"accountUsername"`
	IsBot                   bool   `json:"isBot"`
	LeadingAuctions         int    `json:"leadingAuctions"`
	WithBuyout              int    `json:"withBuyout"`
	TotalCommittedBidCopper int64  `json:"totalCommittedBidCopper"`
	TotalCommittedBidGold   string `json:"totalCommittedBidGold"`
	TotalBuyoutValueCopper  int64  `json:"totalBuyoutValueCopper"`
	TotalBuyoutValueGold    string `json:"totalBuyoutValueGold"`
	MaxBidCopper            int64  `json:"maxBidCopper"`
	MaxBidGold              string `json:"maxBidGold"`
}

// fold adds one live-bid auctionhouse row to the bidder's running totals.
func (b *ahBidder) fold(lastbid, buyout int64) {
	b.LeadingAuctions++
	b.TotalCommittedBidCopper += lastbid
	b.TotalBuyoutValueCopper += buyout
	if buyout > 0 {
		b.WithBuyout++
	}
	if lastbid > b.MaxBidCopper {
		b.MaxBidCopper = lastbid
	}
}

// finalizeGold renders the human gold strings once, after the top-N slice, so we
// don't format copper for bidders that never make the cut.
func (b *ahBidder) finalizeGold() {
	b.TotalCommittedBidGold = formatCopper(b.TotalCommittedBidCopper)
	b.TotalBuyoutValueGold = formatCopper(b.TotalBuyoutValueCopper)
	b.MaxBidGold = formatCopper(b.MaxBidCopper)
}

// RegisterAhBidderActivityTool registers `ah_bidder_activity` — a read-only
// ranking of the heaviest auction-house BIDDERS by the number of auctions each is
// currently leading, with character-name + bot-vs-player resolution folded in.
//
// The DEMAND view of the AH, the mirror of ah_market_top_sellers. top_sellers
// ranks itemowner (the SUPPLY side — who is posting listings); this ranks buyguid
// (the DEMAND side — who is currently winning/leading auctions via bids). Answers
// "which players/bots are cornering the market through bids": GROUP BY buyguid
// over live-bid rows (buyguid<>0), COUNT of auctions each currently leads, their
// committed bid capital (summed lastbid), the buyout value of the listings they
// lead (summed buyoutprice), and the largest single leading bid — joined to
// characters for the bidder name + acore_auth.account for the isBot flag. On a
// mod-ah-bot server this exposes whether AHBot characters are self-bidding to
// churn the market.
func RegisterAhBidderActivityTool(reg *Registry, deps DBDeps) {
	reg.Register(Tool{
		Name: "ah_bidder_activity",
		Description: "Top-N auction-house BIDDERS by the number of auctions each is currently leading, " +
			"aggregated from acore_characters.auctionhouse (rows where buyguid<>0) over the ops_ro pool, with " +
			"character-name + bot-vs-player resolution. The DEMAND-side mirror of ah_market_top_sellers: " +
			"top_sellers ranks itemowner (who POSTS listings — supply); this ranks buyguid (who currently LEADS " +
			"auctions via bids — demand). Three-stage composite: (1) fold every live-bid listing per buyguid in Go " +
			"from one full-table scan, (2) batch-resolve the top-N bidder guids to characters.name + account via an " +
			"IN(...) query on acore_characters.characters, (3) batch-resolve those accounts to " +
			"acore_auth.account.username in one IN(...) query and flag isBot when the username starts with " +
			"\"RNDBOT\" (mod-playerbots' RandomBotAccountPrefix). Per bidder: leadingAuctions, withBuyout (how many " +
			"of those listings offer a buyout), totalCommittedBidCopper (copper committed across their leading " +
			"bids), " +
			"totalBuyoutValueCopper (buy-now cost of everything they lead), and maxBidCopper (largest single " +
			"leading bid) — each copper sum paired with a human gold string. On a mod-ah-bot server the heaviest " +
			"bidders are usually AHBot characters self-bidding to churn the market — isBot makes that visible " +
			"without a separate wow_player_lookup. A bidder whose character row is gone (deleted character with " +
			"surviving leading bids) surfaces with an empty characterName + accountId 0 and is counted in " +
			"`unresolved`. Honest realm totals (independent of topN): distinctBidders, totalLeadingAuctions, " +
			"totalCommittedBidCopper. Args: topN (default 10, max 50), houseId (optional — restrict to one auction " +
			"house; 2=Alliance, 6=Horde, 7=Neutral; any other value is rejected, not silently emptied, echoed with " +
			"its faction), minLeading (default 1, clamps to >=1 — only bidders currently leading at least this many " +
			"auctions). Scan capped at 200000 rows; truncated:true flags an incomplete fold. Read-only, 10 s timeout.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{
			"topN":{"type":"integer","description":"Max bidders to rank (default 10, max 50)"},
			"houseId":{"type":"integer","description":"Restrict to one auction house by houseid (2=Alliance, 6=Horde, 7=Neutral); omit for all houses. Any other value is rejected."},
			"minLeading":{"type":"integer","description":"Only rank bidders currently leading at least this many auctions (default 1, clamps to >=1)"}
		}}`),
		Annotations: AnnRead(),
		Handler: func(ctx context.Context, raw json.RawMessage, _ string) any {
			var a struct {
				TopN       int  `json:"topN"`
				HouseID    *int `json:"houseId"`
				MinLeading int  `json:"minLeading"`
			}
			if len(raw) > 0 && string(raw) != "null" {
				if err := json.Unmarshal(raw, &a); err != nil {
					return map[string]any{"error": "decode args: " + err.Error()}
				}
			}
			if deps.QueryDB == nil {
				return map[string]any{"error": "ops_ro pool not configured (DB_DSN empty?)"}
			}
			// houseId is rejected (not silently treated as an empty market) when
			// present and outside the AuctionHouseId enum {2,6,7} — a typo can't
			// masquerade as "no bidders". Checked before any query.
			if a.HouseID != nil && !isAhBidderHouse(*a.HouseID) {
				return map[string]any{"error": fmt.Sprintf("invalid houseId %d (expected 2=Alliance, 6=Horde, or 7=Neutral)", *a.HouseID)}
			}
			topN := a.TopN
			if topN <= 0 {
				topN = ahBidderDefaultTopN
			}
			if topN > ahBidderMaxTopN {
				topN = ahBidderMaxTopN
			}
			minLeading := a.MinLeading
			if minLeading < ahBidderDefaultMinLeading {
				minLeading = ahBidderDefaultMinLeading
			}
			c, cancel := context.WithTimeout(ctx, ahBidderTimeout)
			defer cancel()
			out, err := collectAhBidderActivity(c, deps.QueryDB, time.Now(), a.HouseID, minLeading, topN)
			if err != nil {
				return map[string]any{"error": err.Error()}
			}
			return out
		},
	})
}

// isAhBidderHouse reports whether id is a recognized AuctionHouseId
// {Alliance=2, Horde=6, Neutral=7}. Kept local so this tool's houseId gate is
// independent of the unmerged ah_market_bid_activity/expiry_timeline filter
// helpers (which land in sibling PRs).
func isAhBidderHouse(id int) bool {
	return id == 2 || id == 6 || id == 7
}

// collectAhBidderActivity runs the three-stage composite. Split out so tests can
// drive it with sqlmock and a fixed `now`. houseID is *int so "unset" (all houses)
// is distinguishable from an explicit house filter (already validated by the
// handler).
func collectAhBidderActivity(ctx context.Context, db *sql.DB, now time.Time, houseID *int, minLeading, topN int) (map[string]any, error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire conn: %w", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "USE `acore_characters`"); err != nil {
		return nil, fmt.Errorf("USE acore_characters: %w", err)
	}

	// Stage 1: scan live-bid rows + fold per bidder, accumulating honest realm
	// totals (over every scanned row, before the minLeading/topN cut).
	q := ahBidderScanBase
	args := make([]any, 0, 2)
	if houseID != nil {
		q += " AND houseid = ?"
		args = append(args, *houseID)
	}
	q += " LIMIT ?"
	args = append(args, ahBidderScanCap+1)

	rows, err := conn.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("auctionhouse query: %w", err)
	}
	defer rows.Close()

	bidders := map[int64]*ahBidder{}
	scanned := 0
	truncated := false
	var realmBidCopper int64
	for rows.Next() {
		if scanned >= ahBidderScanCap {
			truncated = true
			break
		}
		var houseIDRow int
		var buyguid, lastbid, buyout int64
		if err := rows.Scan(&houseIDRow, &buyguid, &lastbid, &buyout); err != nil {
			return nil, fmt.Errorf("auctionhouse scan: %w", err)
		}
		scanned++
		realmBidCopper += lastbid
		b := bidders[buyguid]
		if b == nil {
			b = &ahBidder{BidderGuid: buyguid}
			bidders[buyguid] = b
		}
		b.fold(lastbid, buyout)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("auctionhouse iter: %w", err)
	}

	// Stage 2: minLeading filter, sort leadingAuctions-desc, slice to topN.
	top, distinct := topNAhBidders(bidders, minLeading, topN)

	// Stage 3: resolve names (still in acore_characters) then accounts (acore_auth).
	unresolved := 0
	if len(top) > 0 {
		if err := annotateBidderCharacters(ctx, conn, top); err != nil {
			return nil, err
		}
		if _, err := conn.ExecContext(ctx, "USE `acore_auth`"); err != nil {
			return nil, fmt.Errorf("USE acore_auth: %w", err)
		}
		if err := annotateBidderAccounts(ctx, conn, top); err != nil {
			return nil, err
		}
		for _, b := range top {
			b.finalizeGold()
			if b.CharacterName == "" {
				unresolved++
			}
		}
	}

	out := map[string]any{
		"asOf":                    now.UTC().Format(time.RFC3339),
		"topN":                    topN,
		"minLeading":              minLeading,
		"distinctBidders":         distinct,
		"scanned":                 scanned,
		"scanCap":                 ahBidderScanCap,
		"truncated":               truncated,
		"unresolved":              unresolved,
		"totalLeadingAuctions":    scanned,
		"totalCommittedBidCopper": realmBidCopper,
		"totalCommittedBidGold":   formatCopper(realmBidCopper),
		"bidders":                 top,
	}
	if houseID != nil {
		out["houseId"] = *houseID
		out["faction"] = ahHouseFaction(*houseID)
	}
	return out, nil
}

// topNAhBidders filters the folded bidders to those leading >= minLeading
// auctions, sorts leadingAuctions-desc with bidderGuid-asc as a deterministic
// tiebreaker (without it Go's randomized map iteration would flake tests on tied
// counts), slices to n, and returns the slice plus the pre-slice distinct count
// (so the caller can report "ranking the top 10 of 120 bidders"). Always a
// non-nil slice.
func topNAhBidders(m map[int64]*ahBidder, minLeading, n int) ([]*ahBidder, int) {
	out := make([]*ahBidder, 0, len(m))
	for _, b := range m {
		if b.LeadingAuctions >= minLeading {
			out = append(out, b)
		}
	}
	distinct := len(out)
	sort.Slice(out, func(i, j int) bool {
		if out[i].LeadingAuctions != out[j].LeadingAuctions {
			return out[i].LeadingAuctions > out[j].LeadingAuctions
		}
		return out[i].BidderGuid < out[j].BidderGuid
	})
	if len(out) > n {
		out = out[:n]
	}
	return out, distinct
}

// annotateBidderCharacters batch-resolves the top bidders' buyguid guids to
// characters.name + account in one IN(...) query. Mirrors the wow_* GM-annotation
// batch-IN idiom (wow_failed_logins_top / wow_recent_logins) and the seller-side
// twin: one round trip, a guid->row map, then a second pass to set the fields. A
// guid with no row (a deleted character whose leading bids outlive it) is left
// with an empty name + account 0 — surfaced via the top-level `unresolved` count,
// not dropped.
func annotateBidderCharacters(ctx context.Context, conn *sql.Conn, bidders []*ahBidder) error {
	if len(bidders) == 0 {
		return nil
	}
	var b strings.Builder
	b.Grow(64 + len(bidders)*2)
	b.WriteString("SELECT guid, name, account FROM `characters` WHERE guid IN (")
	args := make([]any, 0, len(bidders))
	for i, bd := range bidders {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteByte('?')
		args = append(args, bd.BidderGuid)
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
	byGuid := make(map[int64]charRow, len(bidders))
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
	for _, bd := range bidders {
		if c, ok := byGuid[bd.BidderGuid]; ok {
			bd.CharacterName = c.name
			bd.AccountID = c.account
		}
	}
	return nil
}

// annotateBidderAccounts batch-resolves the resolved bidders' account ids to
// acore_auth.account.username in one IN(...) query and flags isBot when the
// username carries the RNDBOT prefix (case-insensitive). Bidders whose character
// row was missing (account 0) are skipped — they carry no username and isBot stays
// false. Distinct account ids are deduped so two bidder-characters on the same
// account bind the id once.
func annotateBidderAccounts(ctx context.Context, conn *sql.Conn, bidders []*ahBidder) error {
	ids := make([]int64, 0, len(bidders))
	seen := make(map[int64]bool, len(bidders))
	for _, b := range bidders {
		if b.AccountID != 0 && !seen[b.AccountID] {
			seen[b.AccountID] = true
			ids = append(ids, b.AccountID)
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
	for _, b := range bidders {
		if u, ok := byID[b.AccountID]; ok {
			b.AccountUsername = u
			b.IsBot = strings.HasPrefix(strings.ToUpper(u), ahBidderBotPrefix)
		}
	}
	return nil
}
