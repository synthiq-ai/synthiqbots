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
	ahFootprintTimeout          = 10 * time.Second
	ahFootprintScanCap          = 200000 // hard ceiling on auctionhouse rows folded into one snapshot
	ahFootprintDefaultTopN      = 15
	ahFootprintMaxTopN          = 100
	ahFootprintDefaultMinHouses = 1        // 1 keeps the single-house whales visible; 2 is the cross-house-only filter
	ahFootprintMaxMinHouses     = 3        // Alliance/Horde/Neutral — there are only 3 AuctionHouseIds
	ahFootprintBotPrefix        = "RNDBOT" // mod-playerbots' default RandomBotAccountPrefix
)

// ahFootprintSortKeys is the sortBy allowlist. Every key ranks a value folded in
// Go over the scanned listings (not a SQL aggregate), so there is no ORDER BY
// expression to interpolate and no aggregate-alias footgun — the fold-in-Go idiom
// ah_undercutters (#369) / ah_cross_house_arbitrage_depth (#384) established. An
// unknown key is rejected before any query runs, so a typo cannot silently
// reorder the ranking.
//
// NOTE the direction: every key sorts DESCENDING except `spread`, which sorts
// concentrationPct ASCENDING — the most evenly spread inventory first, which is
// the whole point of asking for a spread ranking.
var ahFootprintSortKeys = map[string]bool{
	"housesUsed": true, // default: how many distinct auction houses the seller lists on
	"listings":   true, // the seller's total active listings across all houses
	"value":      true, // summed buyout value of those listings, in copper
	"spread":     true, // concentrationPct ASC — most evenly distributed inventory first
}

// ahFootprintScanQuery is the single per-listing fold source. Deliberately NOT
// filtered on buyoutprice > 0: this is a FOOTPRINT (where does a seller keep
// inventory), so an auction-only listing with no buy-now price is still inventory
// parked on that house. Such a row counts in listings/quantity/distinctItems and
// contributes 0 to the buyout value, which is why the value figures are named
// buyoutValue* rather than "market value". item_template is deliberately NOT
// joined — this is an ACTOR-grain tool that displays no item names, so
// distinctItems folds from ii.itemEntry alone and the whole query stays inside
// acore_characters with zero acore_world reach (the #369 precedent). LIMIT is
// bound to scanCap+1 so truncation is detectable without a separate COUNT.
const ahFootprintScanQuery = "SELECT ah.itemowner, ah.houseid, ii.itemEntry, ii.count, ah.buyoutprice " +
	"FROM `auctionhouse` ah JOIN `item_instance` ii ON ah.itemguid = ii.guid LIMIT ?"

// ahFootprintHouse is one seller's presence on ONE auction house. ListingsPct is
// that house's share of the seller's own listings (not of the market), so the
// per-house pcts of one seller sum to ~100.
type ahFootprintHouse struct {
	HouseID           int     `json:"houseId"`
	Faction           string  `json:"faction"`
	Listings          int     `json:"listings"`
	Quantity          int64   `json:"quantity"`
	DistinctItems     int     `json:"distinctItems"`
	BuyoutValueCopper int64   `json:"buyoutValueCopper"`
	BuyoutValueMoney  string  `json:"buyoutValueMoney"`
	ListingsPct       float64 `json:"listingsPct"`

	// items tracks the distinct itemEntries this seller lists on this house;
	// unexported -> not serialized, collapsed into DistinctItems by finalize.
	items map[int64]struct{}
}

// ahFootprintSeller is one seller's cross-house inventory footprint. HousesUsed is
// the headline: 1 = a single-house whale whose whole position evaporates if that
// faction's market moves, >=2 = an established cross-house trader (the competition
// for anything ah_cross_house_arbitrage_depth surfaces). ConcentrationPct is the
// biggest house's share of the seller's listings — 100 for a single-house seller,
// ~50 for a seller split evenly across two — so a LOW concentration with a HIGH
// housesUsed is a genuinely diversified operator. characterName / accountId /
// accountUsername / isBot are folded in from a batch lookup against
// acore_characters.characters + acore_auth.account (empty name + accountId 0 when
// the owning character row is gone — a deleted character whose listings outlive
// it).
type ahFootprintSeller struct {
	ItemOwnerGuid     int64               `json:"itemOwnerGuid"`
	CharacterName     string              `json:"characterName"`
	AccountID         int64               `json:"accountId"`
	AccountUsername   string              `json:"accountUsername"`
	IsBot             bool                `json:"isBot"`
	HousesUsed        int                 `json:"housesUsed"`
	Listings          int                 `json:"listings"`
	Quantity          int64               `json:"quantity"`
	DistinctItems     int                 `json:"distinctItems"`
	BuyoutValueCopper int64               `json:"buyoutValueCopper"`
	BuyoutValueMoney  string              `json:"buyoutValueMoney"`
	PrimaryHouseID    int                 `json:"primaryHouseId"`
	PrimaryFaction    string              `json:"primaryFaction"`
	ConcentrationPct  float64             `json:"concentrationPct"`
	Houses            []*ahFootprintHouse `json:"houses"`

	// houses / items are the fold-time accumulators; unexported -> not
	// serialized, collapsed by finalize into Houses + DistinctItems.
	houses map[int]*ahFootprintHouse
	items  map[int64]struct{}
}

// fold adds one scanned listing to the seller's running totals and to the
// per-house bucket it belongs to.
func (s *ahFootprintSeller) fold(houseID int, itemEntry, count, buyout int64) {
	s.Listings++
	s.Quantity += count
	s.BuyoutValueCopper += buyout
	if s.items == nil {
		s.items = map[int64]struct{}{}
	}
	s.items[itemEntry] = struct{}{}

	if s.houses == nil {
		s.houses = map[int]*ahFootprintHouse{}
	}
	h := s.houses[houseID]
	if h == nil {
		// ahHouseFaction labels an unrecognised houseid "house-N" rather than
		// dropping the row, so a realm with a non-standard AuctionHouseId still
		// folds honestly instead of silently under-counting the footprint.
		h = &ahFootprintHouse{HouseID: houseID, Faction: ahHouseFaction(houseID), items: map[int64]struct{}{}}
		s.houses[houseID] = h
	}
	h.Listings++
	h.Quantity += count
	h.BuyoutValueCopper += buyout
	h.items[itemEntry] = struct{}{}
}

// finalize collapses the unexported sets into the serialized counts and derives
// the concentration figures. Houses is ordered by houseId asc so the output is
// deterministic (Go map iteration is randomized). PrimaryHouseID is the house
// carrying the MOST of the seller's listings, ties resolved to the LOWEST houseId
// because the walk runs over that already-sorted slice with a strict `>`.
func (s *ahFootprintSeller) finalize() {
	s.DistinctItems = len(s.items)
	s.HousesUsed = len(s.houses)
	s.BuyoutValueMoney = formatCopper(s.BuyoutValueCopper)

	s.Houses = make([]*ahFootprintHouse, 0, len(s.houses))
	for _, h := range s.houses {
		h.DistinctItems = len(h.items)
		h.BuyoutValueMoney = formatCopper(h.BuyoutValueCopper)
		h.ListingsPct = ahCategoryValuePct(int64(h.Listings), int64(s.Listings))
		s.Houses = append(s.Houses, h)
	}
	sort.Slice(s.Houses, func(i, j int) bool { return s.Houses[i].HouseID < s.Houses[j].HouseID })

	maxListings := 0
	for _, h := range s.Houses {
		if h.Listings > maxListings {
			maxListings = h.Listings
			s.PrimaryHouseID = h.HouseID
			s.PrimaryFaction = h.Faction
		}
	}
	// Same helper (and therefore the same div-by-zero guard + one-decimal
	// rounding) the family already uses for every ratio-of-a-part.
	s.ConcentrationPct = ahCategoryValuePct(int64(maxListings), int64(s.Listings))
}

// ahFootprintHouseTotal is one auction house's realm-wide census row — the
// denominator for the per-seller footprints. Sellers counts DISTINCT itemowner
// guids with at least one listing on that house, so summing `sellers` across
// houses exceeds distinctSellers exactly by the cross-house overlap.
type ahFootprintHouseTotal struct {
	HouseID           int    `json:"houseId"`
	Faction           string `json:"faction"`
	Sellers           int    `json:"sellers"`
	Listings          int    `json:"listings"`
	Quantity          int64  `json:"quantity"`
	BuyoutValueCopper int64  `json:"buyoutValueCopper"`
	BuyoutValueMoney  string `json:"buyoutValueMoney"`
}

// RegisterAhSellerHouseFootprintTool registers `ah_seller_house_footprint` — a
// read-only ranking of auction-house sellers by how many DISTINCT houses they
// keep inventory on, with the per-house split and a concentration figure.
//
// The actor-axis mirror of the cross-house item lenses. Every other actor-grain
// ah_* tool (ah_market_top_sellers, ah_undercutters, ah_bidder_activity,
// ah_market_price_outliers_by_seller) takes an optional houseId that SCOPES to one
// house and folds house away entirely when it is omitted — so a seller listing 40
// items on Alliance and 2 on Neutral is indistinguishable from one listing 42 on
// Alliance. The item axis has two cross-house lenses (ah_cross_house_arbitrage,
// ah_cross_house_arbitrage_depth); this is the first on the actor axis. It answers
// the two operator questions those cannot: who are the established cross-house
// traders (i.e. the competition already arbitraging what the depth tool surfaces),
// and which sellers are single-house whales whose whole position is exposed to one
// faction's market.
//
// Deliberately takes NO houseId argument: scoping this tool to one house would
// collapse the very axis it exists to expose. Use minHouses to filter instead.
func RegisterAhSellerHouseFootprintTool(reg *Registry, deps DBDeps) {
	reg.Register(Tool{
		Name: "ah_seller_house_footprint",
		Description: "Top-N auction-house sellers ranked by their CROSS-HOUSE inventory footprint — how many " +
			"distinct auction houses each keeps listings on — aggregated from acore_characters.auctionhouse joined " +
			"to item_instance over the ops_ro pool, with character-name + bot-vs-player resolution. Every listing is " +
			"folded per itemowner AND per houseid: housesUsed (distinct houses, the headline), listings, quantity " +
			"(summed ii.count), distinctItems (distinct itemEntry), buyoutValueCopper (+ buyoutValueMoney), " +
			"primaryHouseId/primaryFaction (the house holding most of that seller's listings, ties to the lowest " +
			"houseId), concentrationPct (the primary house's share of the seller's listings — 100 for a single-house " +
			"seller, ~50 for an even two-house split), and a per-house `houses` array of {houseId, faction, listings, " +
			"quantity, distinctItems, buyoutValueCopper, buyoutValueMoney, listingsPct} ordered by houseId asc. The " +
			"top itemowner guids are then batch-resolved to characters.name + account and to " +
			"acore_auth.account.username, flagging isBot when the username starts with \"RNDBOT\" (mod-playerbots' " +
			"RandomBotAccountPrefix). The ACTOR-axis mirror of the cross-house item lenses: " +
			"ah_cross_house_arbitrage and ah_cross_house_arbitrage_depth compare one ITEM's floors across houses but " +
			"name no seller, while ah_market_top_sellers and ah_undercutters rank sellers with house either scoped to " +
			"one value or folded away — so neither can distinguish a seller with 40 Alliance + 2 Neutral listings " +
			"from one with 42 Alliance listings. Answers who the established cross-house traders are (the competition " +
			"for any arbitrage the depth tool surfaces) and which sellers are single-house whales exposed to one " +
			"faction's market. Listings with buyoutprice=0 (auction-only, no buy-now) ARE counted in listings/" +
			"quantity/distinctItems and contribute 0 to the buyout value — this is a footprint, not a price census. " +
			"There is deliberately NO houseId argument (scoping to one house would collapse the axis this tool " +
			"exists to expose) and no acore_world/item_template join (actor grain, no item names displayed). A seller " +
			"whose owning character row is gone (deleted character with surviving listings) surfaces with an empty " +
			"characterName + accountId 0 and is counted in `unresolved`. Honest realm-wide totals " +
			"(distinctSellers, crossHouseSellers, matchedSellers, totalListings, totalQuantity, " +
			"totalBuyoutValueCopper) plus a per-house `houseTotals` census (sellers/listings/quantity/value per " +
			"houseId) are folded independent of topN. Args: topN (default 15, max 100), sortBy (housesUsed " +
			"[default] | listings | value | spread; an unknown key is rejected), minHouses (default 1, clamped to " +
			"1..3 — pass 2 for cross-house sellers ONLY). Every sortBy ranks DESCENDING except `spread`, which " +
			"ranks concentrationPct ASCENDING (most evenly distributed inventory first); itemOwnerGuid asc is the " +
			"tiebreak throughout. Houses are the AzerothCore AuctionHouseIds 2=Alliance, 6=Horde, 7=Neutral; any " +
			"other houseid still folds, labelled \"house-N\". Scan capped at 200000 rows; truncated:true flags an " +
			"incomplete fold (the totals are then a floor, not a census). Read-only, 10 s timeout.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{
			"topN":{"type":"integer","description":"Max sellers to rank (default 15, max 100)"},
			"sortBy":{"type":"string","enum":["housesUsed","listings","value","spread"],"description":"housesUsed=distinct houses desc (default), listings=total listings desc, value=summed buyout copper desc, spread=concentrationPct ASCENDING (most evenly distributed inventory first)"},
			"minHouses":{"type":"integer","description":"Only rank sellers listing on at least this many distinct houses (default 1, clamped to 1..3; pass 2 for cross-house sellers only)"}
		}}`),
		Annotations: AnnRead(),
		Handler: func(ctx context.Context, raw json.RawMessage, _ string) any {
			var a struct {
				TopN      int    `json:"topN"`
				SortBy    string `json:"sortBy"`
				MinHouses *int   `json:"minHouses"`
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
				topN = ahFootprintDefaultTopN
			}
			if topN > ahFootprintMaxTopN {
				topN = ahFootprintMaxTopN
			}
			// sortBy resolves to an allowlist key; empty -> "housesUsed". An unknown
			// key is rejected (no query runs) so a typo can't silently reorder.
			sortKey := a.SortBy
			if sortKey == "" {
				sortKey = "housesUsed"
			}
			if !ahFootprintSortKeys[sortKey] {
				return map[string]any{"error": fmt.Sprintf("invalid sortBy: %q (valid: housesUsed, listings, value, spread)", a.SortBy)}
			}
			// minHouses is a soft filter, clamped at BOTH ends rather than rejected:
			// <1 is meaningless and >3 could never match (there are only 3 houses), so
			// clamping keeps a fat-fingered value from silently returning an empty
			// ranking. The post-clamp value is echoed in the response.
			minHouses := ahFootprintDefaultMinHouses
			if a.MinHouses != nil {
				minHouses = *a.MinHouses
				if minHouses < 1 {
					minHouses = 1
				}
				if minHouses > ahFootprintMaxMinHouses {
					minHouses = ahFootprintMaxMinHouses
				}
			}
			c, cancel := context.WithTimeout(ctx, ahFootprintTimeout)
			defer cancel()
			out, err := collectAhSellerHouseFootprint(c, deps.QueryDB, time.Now(), minHouses, topN, sortKey)
			if err != nil {
				return map[string]any{"error": err.Error()}
			}
			return out
		},
	})
}

// collectAhSellerHouseFootprint runs the composite on one conn (so a single USE
// applies): (1) scan + fold every listing per owner and per house, (2) finalize
// the per-seller derivations and fold the realm-wide census, (3) filter/sort/slice
// to topN in Go, (4) batch-resolve the winners' names + accounts. Split out so
// tests can drive it with sqlmock and a fixed `now`.
func collectAhSellerHouseFootprint(ctx context.Context, db *sql.DB, now time.Time, minHouses, topN int, sortKey string) (map[string]any, error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire conn: %w", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "USE `acore_characters`"); err != nil {
		return nil, fmt.Errorf("USE acore_characters: %w", err)
	}

	// Stage 1: one listing scan, folded per (owner, house).
	rows, err := conn.QueryContext(ctx, ahFootprintScanQuery, ahFootprintScanCap+1)
	if err != nil {
		return nil, fmt.Errorf("auctionhouse scan query: %w", err)
	}
	owners := map[int64]*ahFootprintSeller{}
	scanned := 0
	truncated := false
	for rows.Next() {
		if scanned >= ahFootprintScanCap {
			truncated = true
			break
		}
		var itemOwner, itemEntry, count, buyout int64
		var houseID int
		if err := rows.Scan(&itemOwner, &houseID, &itemEntry, &count, &buyout); err != nil {
			rows.Close()
			return nil, fmt.Errorf("auctionhouse scan: %w", err)
		}
		scanned++
		s := owners[itemOwner]
		if s == nil {
			s = &ahFootprintSeller{ItemOwnerGuid: itemOwner}
			owners[itemOwner] = s
		}
		s.fold(houseID, itemEntry, count, buyout)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("auctionhouse iter: %w", err)
	}
	// Close explicitly (not just via defer) so the conn is free for the stage-4
	// annotate queries even on an early truncation break.
	rows.Close()

	// Stage 2: derive per-seller figures for EVERY seller (not just the displayed
	// ones) so the census below and the minHouses filter both see finalized data.
	for _, s := range owners {
		s.finalize()
	}
	houseTotals, totalListings, totalQuantity, totalValue := ahFootprintHouseTotals(owners)

	// Stage 3: filter, sort, slice.
	top, crossHouseSellers, matchedSellers := topNAhFootprintSellers(owners, minHouses, topN, sortKey)

	// Stage 4: resolve names (still in acore_characters) then accounts (acore_auth)
	// for the winners only.
	unresolved := 0
	if len(top) > 0 {
		if err := annotateFootprintCharacters(ctx, conn, top); err != nil {
			return nil, err
		}
		if _, err := conn.ExecContext(ctx, "USE `acore_auth`"); err != nil {
			return nil, fmt.Errorf("USE acore_auth: %w", err)
		}
		if err := annotateFootprintAccounts(ctx, conn, top); err != nil {
			return nil, err
		}
		for _, s := range top {
			if s.CharacterName == "" {
				unresolved++
			}
		}
	}

	return map[string]any{
		"asOf":             now.UTC().Format(time.RFC3339),
		"topN":             topN,
		"minHouses":        minHouses,
		"sortBy":           sortKey,
		"scanned":          scanned,
		"scanCap":          ahFootprintScanCap,
		"truncated":        truncated,
		"displayedSellers": len(top),
		"unresolved":       unresolved,
		"totals": map[string]any{
			"distinctSellers":        len(owners),
			"crossHouseSellers":      crossHouseSellers,
			"matchedSellers":         matchedSellers,
			"totalListings":          totalListings,
			"totalQuantity":          totalQuantity,
			"totalBuyoutValueCopper": totalValue,
			"totalBuyoutValueMoney":  formatCopper(totalValue),
			"housesSeen":             len(houseTotals),
		},
		"houseTotals": houseTotals,
		"sellers":     top,
	}, nil
}

// ahFootprintHouseTotals folds the finalized sellers into the per-house realm
// census plus the grand totals — all independent of topN, so the denominators
// never shrink when the display list is trimmed. Ordered by houseId asc.
func ahFootprintHouseTotals(m map[int64]*ahFootprintSeller) (totals []*ahFootprintHouseTotal, listings int, quantity, value int64) {
	byHouse := map[int]*ahFootprintHouseTotal{}
	for _, s := range m {
		listings += s.Listings
		quantity += s.Quantity
		value += s.BuyoutValueCopper
		for _, h := range s.Houses {
			t := byHouse[h.HouseID]
			if t == nil {
				t = &ahFootprintHouseTotal{HouseID: h.HouseID, Faction: h.Faction}
				byHouse[h.HouseID] = t
			}
			t.Sellers++ // one increment per seller present on this house
			t.Listings += h.Listings
			t.Quantity += h.Quantity
			t.BuyoutValueCopper += h.BuyoutValueCopper
		}
	}
	totals = make([]*ahFootprintHouseTotal, 0, len(byHouse))
	for _, t := range byHouse {
		t.BuyoutValueMoney = formatCopper(t.BuyoutValueCopper)
		totals = append(totals, t)
	}
	sort.Slice(totals, func(i, j int) bool { return totals[i].HouseID < totals[j].HouseID })
	return totals, listings, quantity, value
}

// topNAhFootprintSellers filters the folded sellers to those on >= minHouses
// distinct houses, sorts by the chosen key with itemOwnerGuid-asc as a
// deterministic tiebreaker (without it Go's randomized map iteration would flake
// tests on ties), and slices to n. It also returns the pre-slice census:
// crossHouseSellers (>=2 houses, independent of minHouses) and matchedSellers
// (passing minHouses) — both independent of topN. Always a non-nil slice.
// Assumes finalize() has already run on every seller.
func topNAhFootprintSellers(m map[int64]*ahFootprintSeller, minHouses, n int, sortKey string) (top []*ahFootprintSeller, crossHouseSellers, matchedSellers int) {
	out := make([]*ahFootprintSeller, 0, len(m))
	for _, s := range m {
		if s.HousesUsed >= 2 {
			crossHouseSellers++
		}
		if s.HousesUsed >= minHouses {
			out = append(out, s)
		}
	}
	matchedSellers = len(out)
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		switch sortKey {
		case "listings":
			if a.Listings != b.Listings {
				return a.Listings > b.Listings
			}
		case "value":
			if a.BuyoutValueCopper != b.BuyoutValueCopper {
				return a.BuyoutValueCopper > b.BuyoutValueCopper
			}
		case "spread":
			// The one ASCENDING key: least concentrated (most evenly spread across
			// houses) first, breaking ties toward the wider footprint.
			if a.ConcentrationPct != b.ConcentrationPct {
				return a.ConcentrationPct < b.ConcentrationPct
			}
			if a.HousesUsed != b.HousesUsed {
				return a.HousesUsed > b.HousesUsed
			}
		default: // housesUsed
			if a.HousesUsed != b.HousesUsed {
				return a.HousesUsed > b.HousesUsed
			}
			if a.Listings != b.Listings {
				return a.Listings > b.Listings
			}
		}
		return a.ItemOwnerGuid < b.ItemOwnerGuid
	})
	if len(out) > n {
		out = out[:n]
	}
	return out, crossHouseSellers, matchedSellers
}

// annotateFootprintCharacters batch-resolves the displayed sellers' itemowner
// guids to characters.name + account in one IN(...) query. A guid with no row (a
// deleted character whose listings outlive it) is left with an empty name +
// account 0 — surfaced via the top-level `unresolved` count, not dropped.
// Own-named (bound to *ahFootprintSeller) so it does not collide with the
// ah_undercutters / ah_market_top_sellers helpers.
func annotateFootprintCharacters(ctx context.Context, conn *sql.Conn, sellers []*ahFootprintSeller) error {
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

// annotateFootprintAccounts batch-resolves the resolved sellers' account ids to
// acore_auth.account.username in one IN(...) query and flags isBot when the
// username carries the RNDBOT prefix (case-insensitive). Sellers whose character
// row was missing (account 0) are skipped. Distinct account ids are deduped so two
// seller-characters on the same account bind the id once — which matters here more
// than in the single-house siblings, since one account running an Alliance and a
// Horde mule is exactly the pattern this tool surfaces.
func annotateFootprintAccounts(ctx context.Context, conn *sql.Conn, sellers []*ahFootprintSeller) error {
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
			s.IsBot = strings.HasPrefix(strings.ToUpper(u), ahFootprintBotPrefix)
		}
	}
	return nil
}
