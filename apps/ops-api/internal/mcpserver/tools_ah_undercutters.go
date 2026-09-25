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
	ahUndercutTimeout          = 10 * time.Second
	ahUndercutScanCap          = 200000 // hard ceiling on auctionhouse rows folded into one snapshot
	ahUndercutDefaultTopN      = 15
	ahUndercutMaxTopN          = 100
	ahUndercutDefaultMinFloors = 1        // a seller holding <1 floor is not an undercutter at all
	ahUndercutBotPrefix        = "RNDBOT" // mod-playerbots' default RandomBotAccountPrefix
)

// ahUndercutSortKeys is the sortBy allowlist. Sorting is done in Go over the
// folded slice (not an interpolated SQL ORDER BY), so these are plain validation
// keys — an unknown key is rejected before any query runs, so a typo can't reach
// the fold. Go-side sort sidesteps the ORDER-BY-alias footgun the derived-table
// aggregate would otherwise invite.
var ahUndercutSortKeys = map[string]bool{
	"floorHeld":     true, // default: how many of the seller's listings sit AT the item floor
	"distinctItems": true, // how many distinct item TYPES the seller holds a floor on
	"listings":      true, // the seller's total priced listings (context for the floor-hold rate)
}

// ahUndercutFloorQuery computes the per-item price floor — the cheapest active
// buy-now price for each item TYPE — across the whole market (or one house). One
// row per distinct itemEntry, so it is bounded by item VARIETY (not listing
// count) and needs no scan cap. buyoutprice>0 excludes auction-only rows (no
// buy-now price = no floor). The optional `AND ah.houseid = ?` + trailing
// `GROUP BY ii.itemEntry` are appended by the builder; the houseid scope matters
// because undercutting is per-house (the Alliance floor and the Neutral floor for
// the same item differ).
const ahUndercutFloorQuery = "SELECT ii.itemEntry, MIN(ah.buyoutprice) " +
	"FROM `auctionhouse` ah JOIN `item_instance` ii ON ah.itemguid = ii.guid " +
	"WHERE ah.buyoutprice > 0"

// ahUndercutScanBase is the per-listing scan folded per owner: itemowner (seller
// characters.guid), itemEntry (to look up the item's floor from the map above),
// buyoutprice (compared against that floor). Same buyoutprice>0 + optional houseid
// scope as the floor query so the fold and the floor agree on the population. An
// optional `AND ah.houseid = ?` and the trailing `LIMIT ?` (bound to scanCap+1 so
// truncation is detectable without a separate COUNT) are appended by the builder.
const ahUndercutScanBase = "SELECT ah.itemowner, ii.itemEntry, ah.buyoutprice " +
	"FROM `auctionhouse` ah JOIN `item_instance` ii ON ah.itemguid = ii.guid " +
	"WHERE ah.buyoutprice > 0"

// ahUndercutter is one ranked seller. floorHeld = how many of this seller's
// listings sit at their item's floor (the cheapest buy-now for that item type);
// distinctItemsUndercut = how many distinct item types they hold a floor on;
// listings = their total priced listings; floorHoldRatePct = floorHeld/listings
// as a percent (how aggressively the seller camps the cheap end). characterName /
// accountId / accountUsername / isBot are folded in from a batch lookup against
// acore_characters.characters + acore_auth.account (empty name + accountId 0 when
// the owning character row is gone — a deleted character whose listings outlive
// it). A tie at the same floor across multiple sellers is CORRECT (a saturated
// undercut floor): every seller posting at that floor is a floor-holder.
type ahUndercutter struct {
	ItemOwnerGuid         int64   `json:"itemOwnerGuid"`
	CharacterName         string  `json:"characterName"`
	AccountID             int64   `json:"accountId"`
	AccountUsername       string  `json:"accountUsername"`
	IsBot                 bool    `json:"isBot"`
	FloorHeld             int     `json:"floorHeld"`
	Listings              int     `json:"listings"`
	DistinctItemsUndercut int     `json:"distinctItemsUndercut"`
	FloorHoldRatePct      float64 `json:"floorHoldRatePct"`

	// floorItems is the set of distinct itemEntries this seller floors; its len
	// becomes DistinctItemsUndercut at slice time. Unexported -> not serialized.
	floorItems map[int64]struct{}
}

// foldListing adds one priced listing to the seller's running totals, marking it
// a floor listing when its buyout equals the item's floor from floorByItem.
func (u *ahUndercutter) foldListing(itemEntry, buyout int64, floorByItem map[int64]int64) {
	u.Listings++
	if floor, ok := floorByItem[itemEntry]; ok && buyout == floor {
		u.FloorHeld++
		if u.floorItems == nil {
			u.floorItems = map[int64]struct{}{}
		}
		u.floorItems[itemEntry] = struct{}{}
	}
}

// RegisterAhUndercuttersTool registers `ah_undercutters` — a read-only ranking of
// the sellers who hold the most item price floors realm-wide (the aggressive
// undercutters / AHBot floor-campers), with character-name + bot-vs-player
// resolution folded in.
//
// The ACTOR view of the price war. ah_item_price_dispersion (PR #362) answers
// WHICH item TYPES have the widest seller price disagreement (MIN/MAX spread) but
// names no seller; ah_market_top_sellers ranks sellers by raw listing count/value
// but not by floor-presence. This is the lens neither shows: rank sellers by how
// often their listing IS the item floor, flagging who dominates the cheap end of
// the market and whether they are bots. On a mod-ah-bot server the floor is often
// held by AHBot characters, so "who is undercutting everyone, and are they bots?"
// is the recurring operator question this answers without a hand-written join.
func RegisterAhUndercuttersTool(reg *Registry, deps DBDeps) {
	reg.Register(Tool{
		Name: "ah_undercutters",
		Description: "Top-N auction-house sellers ranked by how many item price FLOORS they hold — the aggressive " +
			"undercutters / AHBot floor-campers — aggregated from acore_characters.auctionhouse joined to " +
			"item_instance over the ops_ro pool, with character-name + bot-vs-player resolution. A per-item floor " +
			"(the cheapest active buy-now price per item TYPE) is computed first, then every priced listing is folded " +
			"per itemowner: floorHeld (listings sitting AT the item floor), listings (the seller's total priced " +
			"listings), distinctItemsUndercut (distinct item types the seller floors), and floorHoldRatePct " +
			"(floorHeld/listings*100 — how aggressively the seller camps the cheap end). The top itemowner guids are " +
			"then batch-resolved to characters.name + account and to acore_auth.account.username, flagging isBot when " +
			"the username starts with \"RNDBOT\" (mod-playerbots' RandomBotAccountPrefix). The ACTOR view of the price " +
			"war: ah_item_price_dispersion reports per-item spread but names no seller, and ah_market_top_sellers " +
			"ranks by raw listing count, not floor-presence — this surfaces who dominates the cheap end and whether " +
			"they are AHBot floor-campers (on a mod-ah-bot server the floor is usually held by AHBot characters). " +
			"Multiple sellers tied at the same floor are ALL counted as floor-holders " +
			"(a saturated undercut floor is correct, not a bug). A seller whose owning character row is gone (deleted " +
			"character with surviving listings) surfaces with an empty characterName + accountId 0 and is counted in " +
			"`unresolved`. Honest realm-wide totals (distinctPricedSellers, distinctFloorHolders, matchedFloorHolders, " +
			"totalFloorListings) are folded independent of topN. Args: topN (default 15, max 100), sortBy (floorHeld " +
			"[default] | distinctItems | listings; an unknown key is rejected), minFloors (default 1, clamps to >=1 — " +
			"only sellers holding at least this many floors), houseId (optional 2=Alliance, 6=Horde, 7=Neutral, the " +
			"AzerothCore AuctionHouseId; another value is rejected; omit for the whole market). Sorted by the chosen " +
			"key desc, itemOwnerGuid asc tiebreak. Scan capped at 200000 rows; truncated:true flags an incomplete " +
			"fold. Read-only, 10 s timeout.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{
			"topN":{"type":"integer","description":"Max sellers to rank (default 15, max 100)"},
			"sortBy":{"type":"string","enum":["floorHeld","distinctItems","listings"],"description":"floorHeld=floors held (default), distinctItems=distinct item types floored, listings=total priced listings"},
			"minFloors":{"type":"integer","description":"Only rank sellers holding at least this many floors (default 1, clamps to >=1)"},
			"houseId":{"type":"integer","description":"Narrow to one auction house (2=Alliance, 6=Horde, 7=Neutral); omit for the whole market"}
		}}`),
		Annotations: AnnRead(),
		Handler: func(ctx context.Context, raw json.RawMessage, _ string) any {
			var a struct {
				TopN      int    `json:"topN"`
				SortBy    string `json:"sortBy"`
				MinFloors *int   `json:"minFloors"`
				HouseID   *int   `json:"houseId"`
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
				topN = ahUndercutDefaultTopN
			}
			if topN > ahUndercutMaxTopN {
				topN = ahUndercutMaxTopN
			}
			// sortBy resolves to an allowlist key; empty -> "floorHeld". An unknown
			// key is rejected (no query) so a typo can't silently reorder the ranking.
			sortKey := a.SortBy
			if sortKey == "" {
				sortKey = "floorHeld"
			}
			if !ahUndercutSortKeys[sortKey] {
				return map[string]any{"error": fmt.Sprintf("invalid sortBy: %q (valid: floorHeld, distinctItems, listings)", a.SortBy)}
			}
			// minFloors is a soft floor, not an enum: default 1, clamp <1 up to 1 (a
			// seller holding zero floors is not an undercutter).
			minFloors := ahUndercutDefaultMinFloors
			if a.MinFloors != nil {
				minFloors = *a.MinFloors
				if minFloors < 1 {
					minFloors = 1
				}
			}
			// houseId is rejected (not clamped) when it isn't a real AuctionHouseId so
			// a bad id can't masquerade as an empty house (matches the ah_* family).
			if a.HouseID != nil && *a.HouseID != 2 && *a.HouseID != 6 && *a.HouseID != 7 {
				return map[string]any{"error": fmt.Sprintf("invalid houseId: %d (valid 2=Alliance, 6=Horde, 7=Neutral)", *a.HouseID)}
			}
			c, cancel := context.WithTimeout(ctx, ahUndercutTimeout)
			defer cancel()
			out, err := collectAhUndercutters(c, deps.QueryDB, time.Now(), a.HouseID, minFloors, topN, sortKey)
			if err != nil {
				return map[string]any{"error": err.Error()}
			}
			return out
		},
	})
}

// collectAhUndercutters runs the composite on one conn (so a single USE applies):
// (1) load the per-item floor map, (2) scan + fold every priced listing per owner,
// (3) filter/sort/slice to topN in Go, (4) batch-resolve the winners' names +
// accounts. Split out so tests can drive it with sqlmock and a fixed `now`.
func collectAhUndercutters(ctx context.Context, db *sql.DB, now time.Time, houseID *int, minFloors, topN int, sortKey string) (map[string]any, error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire conn: %w", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "USE `acore_characters`"); err != nil {
		return nil, fmt.Errorf("USE acore_characters: %w", err)
	}

	// Stage 1a: per-item floor map (fully drained + closed before the next query on
	// this conn).
	floorQ := ahUndercutFloorQuery
	var floorArgs []any
	if houseID != nil {
		floorQ += " AND ah.houseid = ?"
		floorArgs = append(floorArgs, *houseID)
	}
	floorQ += " GROUP BY ii.itemEntry"
	floorRows, err := conn.QueryContext(ctx, floorQ, floorArgs...)
	if err != nil {
		return nil, fmt.Errorf("floor query: %w", err)
	}
	floorByItem := map[int64]int64{}
	for floorRows.Next() {
		var entry, floor int64
		if err := floorRows.Scan(&entry, &floor); err != nil {
			floorRows.Close()
			return nil, fmt.Errorf("floor scan: %w", err)
		}
		floorByItem[entry] = floor
	}
	if err := floorRows.Err(); err != nil {
		floorRows.Close()
		return nil, fmt.Errorf("floor iter: %w", err)
	}
	floorRows.Close()

	// Stage 1b: scan every priced listing + fold per owner.
	scanQ := ahUndercutScanBase
	scanArgs := make([]any, 0, 2)
	if houseID != nil {
		scanQ += " AND ah.houseid = ?"
		scanArgs = append(scanArgs, *houseID)
	}
	scanQ += " LIMIT ?"
	scanArgs = append(scanArgs, ahUndercutScanCap+1)

	rows, err := conn.QueryContext(ctx, scanQ, scanArgs...)
	if err != nil {
		return nil, fmt.Errorf("auctionhouse scan query: %w", err)
	}
	owners := map[int64]*ahUndercutter{}
	scanned := 0
	truncated := false
	for rows.Next() {
		if scanned >= ahUndercutScanCap {
			truncated = true
			break
		}
		var itemOwner, itemEntry, buyout int64
		if err := rows.Scan(&itemOwner, &itemEntry, &buyout); err != nil {
			rows.Close()
			return nil, fmt.Errorf("auctionhouse scan: %w", err)
		}
		scanned++
		u := owners[itemOwner]
		if u == nil {
			u = &ahUndercutter{ItemOwnerGuid: itemOwner}
			owners[itemOwner] = u
		}
		u.foldListing(itemEntry, buyout, floorByItem)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("auctionhouse iter: %w", err)
	}
	// Close explicitly (not just via defer) so the conn is free for the stage-3
	// annotate queries even on an early truncation break.
	rows.Close()

	// Stage 2: minFloors filter, sort by the chosen key, slice to topN — plus the
	// honest realm-wide census folded over ALL owners (independent of topN).
	top, distinctFloorHolders, matchedFloorHolders, totalFloorListings := topNAhUndercutters(owners, minFloors, topN, sortKey)

	// Stage 3: resolve names (still in acore_characters) then accounts (acore_auth),
	// and finalize the per-seller floor-hold rate for the winners only.
	unresolved := 0
	if len(top) > 0 {
		if err := annotateUndercutterCharacters(ctx, conn, top); err != nil {
			return nil, err
		}
		if _, err := conn.ExecContext(ctx, "USE `acore_auth`"); err != nil {
			return nil, fmt.Errorf("USE acore_auth: %w", err)
		}
		if err := annotateUndercutterAccounts(ctx, conn, top); err != nil {
			return nil, err
		}
		for _, u := range top {
			u.FloorHoldRatePct = ahCategoryValuePct(int64(u.FloorHeld), int64(u.Listings))
			if u.CharacterName == "" {
				unresolved++
			}
		}
	}

	out := map[string]any{
		"asOf":             now.UTC().Format(time.RFC3339),
		"topN":             topN,
		"minFloors":        minFloors,
		"sortBy":           sortKey,
		"scanned":          scanned,
		"scanCap":          ahUndercutScanCap,
		"truncated":        truncated,
		"displayedSellers": len(top),
		"unresolved":       unresolved,
		"totals": map[string]any{
			"distinctPricedSellers": len(owners),
			"distinctFloorHolders":  distinctFloorHolders,
			"matchedFloorHolders":   matchedFloorHolders,
			"totalFloorListings":    totalFloorListings,
		},
		"sellers": top,
	}
	if houseID != nil {
		out["houseId"] = *houseID
		out["faction"] = ahHouseFaction(*houseID)
	}
	return out, nil
}

// topNAhUndercutters filters the folded owners to those holding >= minFloors
// floors, sets DistinctItemsUndercut from the tracked floor-item set, sorts by the
// chosen key desc with itemOwnerGuid-asc as a deterministic tiebreaker (without it
// Go's randomized map iteration would flake tests on ties), and slices to n. It
// also returns the pre-slice census: distinctFloorHolders (owners holding >=1
// floor), matchedFloorHolders (owners passing minFloors), and totalFloorListings
// (sum of every owner's floorHeld) — all independent of topN so the honest totals
// never shrink when the display list is trimmed. Always a non-nil slice.
func topNAhUndercutters(m map[int64]*ahUndercutter, minFloors, n int, sortKey string) (top []*ahUndercutter, distinctFloorHolders, matchedFloorHolders, totalFloorListings int) {
	out := make([]*ahUndercutter, 0, len(m))
	for _, u := range m {
		if u.FloorHeld >= 1 {
			distinctFloorHolders++
			totalFloorListings += u.FloorHeld
		}
		if u.FloorHeld >= minFloors {
			u.DistinctItemsUndercut = len(u.floorItems)
			out = append(out, u)
		}
	}
	matchedFloorHolders = len(out)
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		switch sortKey {
		case "listings":
			if a.Listings != b.Listings {
				return a.Listings > b.Listings
			}
		case "distinctItems":
			if a.DistinctItemsUndercut != b.DistinctItemsUndercut {
				return a.DistinctItemsUndercut > b.DistinctItemsUndercut
			}
		default: // floorHeld
			if a.FloorHeld != b.FloorHeld {
				return a.FloorHeld > b.FloorHeld
			}
		}
		return a.ItemOwnerGuid < b.ItemOwnerGuid
	})
	if len(out) > n {
		out = out[:n]
	}
	return out, distinctFloorHolders, matchedFloorHolders, totalFloorListings
}

// annotateUndercutterCharacters batch-resolves the top sellers' itemowner guids to
// characters.name + account in one IN(...) query. A guid with no row (a deleted
// character whose listings outlive it) is left with an empty name + account 0 —
// surfaced via the top-level `unresolved` count, not dropped. Own-named (bound to
// *ahUndercutter) so it does not collide with the ah_market_top_sellers helper.
func annotateUndercutterCharacters(ctx context.Context, conn *sql.Conn, sellers []*ahUndercutter) error {
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

// annotateUndercutterAccounts batch-resolves the resolved sellers' account ids to
// acore_auth.account.username in one IN(...) query and flags isBot when the
// username carries the RNDBOT prefix (case-insensitive). Sellers whose character
// row was missing (account 0) are skipped. Distinct account ids are deduped so two
// seller-characters on the same account bind the id once.
func annotateUndercutterAccounts(ctx context.Context, conn *sql.Conn, sellers []*ahUndercutter) error {
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
			s.IsBot = strings.HasPrefix(strings.ToUpper(u), ahUndercutBotPrefix)
		}
	}
	return nil
}
