package mcpserver

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

// ahSellerScanCols is the auctionhouse column set ah_market_top_sellers scans,
// in order.
func ahSellerScanCols() []string {
	return []string{"houseid", "itemowner", "buyoutprice", "lastbid", "deposit", "buyguid"}
}

// expectSellerScan queues the USE + auctionhouse scan with no rows. Used by the
// arg-clamp tests where the fold result doesn't matter (empty -> no stage-2/3
// queries fire, so only these two expectations are needed).
func expectEmptySellerScan(mock sqlmock.Sqlmock) {
	mock.ExpectExec(regexp.QuoteMeta("USE `acore_characters`")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta("FROM `auctionhouse`")).
		WillReturnRows(sqlmock.NewRows(ahSellerScanCols()))
}

// TestAhMarketTopSellers_DescriptionMentionsContext guards the description
// keywords — they shape which tool the agent reaches for when an operator asks
// "who are the biggest AH sellers?" / "are the top sellers bots?". Drop the
// seller/bot framing and the agent falls back to ah_market_summary (whose
// topSellers facet is guid-only, no name/bot flag) or a hand-written join.
func TestAhMarketTopSellers_DescriptionMentionsContext(t *testing.T) {
	reg := NewRegistry()
	RegisterAhMarketTopSellersTool(reg, DBDeps{})
	tool, ok := reg.Get("ah_market_top_sellers")
	if !ok {
		t.Fatal("ah_market_top_sellers not registered")
	}
	for _, kw := range []string{
		"acore_characters.auctionhouse", "acore_characters.characters", "acore_auth.account",
		"ops_ro", "itemowner", "RNDBOT", "isBot", "AHBot", "mod-ah-bot",
		"ah_market_summary", "wow_player_lookup", "Three-stage", "topN", "houseId",
		"minListings", "unresolved", "floor", "Read-only",
	} {
		if !strings.Contains(tool.Description, kw) {
			t.Errorf("description missing %q", kw)
		}
	}
}

// TestAhMarketTopSellers_ReadOnlyAnnotation pins readOnlyHint=true so a future
// copy-paste of a destructive sibling can't silently flip the gate.
func TestAhMarketTopSellers_ReadOnlyAnnotation(t *testing.T) {
	reg := NewRegistry()
	RegisterAhMarketTopSellersTool(reg, DBDeps{})
	tool, _ := reg.Get("ah_market_top_sellers")
	if !strings.Contains(string(tool.Annotations), `"readOnlyHint":true`) {
		t.Errorf("annotations missing readOnlyHint=true: %s", tool.Annotations)
	}
}

func TestAhMarketTopSellers_NoPoolReturnsError(t *testing.T) {
	reg := NewRegistry()
	RegisterAhMarketTopSellersTool(reg, DBDeps{}) // QueryDB nil
	tool, _ := reg.Get("ah_market_top_sellers")
	resp := tool.Handler(context.Background(), json.RawMessage(`{}`), "")
	got, _ := resp.(map[string]any)
	if msg, _ := got["error"].(string); !strings.Contains(msg, "ops_ro pool not configured") {
		t.Errorf("expected pool-not-configured error, got %v", got)
	}
}

// TestAhMarketTopSellers_TopNClamps drives the handler and asserts the post-clamp
// topN echo (unset->default, zero->default, oversized->cap, in-range passthrough)
// and that the LIMIT bind is always scanCap+1 regardless of args.
func TestAhMarketTopSellers_TopNClamps(t *testing.T) {
	cases := []struct {
		name     string
		args     string
		wantTopN int
	}{
		{"unset uses default", `{}`, ahTopSellersDefaultTopN},
		{"zero falls back to default", `{"topN":0}`, ahTopSellersDefaultTopN},
		{"negative falls back to default", `{"topN":-4}`, ahTopSellersDefaultTopN},
		{"oversized clamps to max", `{"topN":99999}`, ahTopSellersMaxTopN},
		{"in-range passes through", `{"topN":50}`, 50},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			mock.ExpectExec(regexp.QuoteMeta("USE `acore_characters`")).
				WillReturnResult(sqlmock.NewResult(0, 0))
			// No houseId -> no WHERE; LIMIT bind is scanCap+1.
			mock.ExpectQuery(regexp.QuoteMeta("FROM `auctionhouse` LIMIT ?")).
				WithArgs(int64(ahTopSellersScanCap + 1)).
				WillReturnRows(sqlmock.NewRows(ahSellerScanCols()))

			reg := NewRegistry()
			RegisterAhMarketTopSellersTool(reg, DBDeps{QueryDB: db})
			tool, _ := reg.Get("ah_market_top_sellers")
			resp := tool.Handler(context.Background(), json.RawMessage(c.args), "")
			got, _ := resp.(map[string]any)
			if got["error"] != nil {
				t.Fatalf("unexpected error: %v", got)
			}
			if n, _ := got["topN"].(int); n != c.wantTopN {
				t.Errorf("topN: %v want %d", got["topN"], c.wantTopN)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Errorf("unmet mock expectations: %v", err)
			}
		})
	}
}

// TestAhMarketTopSellers_MinListingsClamps verifies the minListings floor of 1
// (unset/zero/negative all clamp to 1) is echoed in the response.
func TestAhMarketTopSellers_MinListingsClamps(t *testing.T) {
	cases := []struct {
		args    string
		wantMin int
	}{
		{`{}`, 1},
		{`{"minListings":0}`, 1},
		{`{"minListings":-5}`, 1},
		{`{"minListings":3}`, 3},
	}
	for _, c := range cases {
		t.Run(c.args, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			expectEmptySellerScan(mock)

			reg := NewRegistry()
			RegisterAhMarketTopSellersTool(reg, DBDeps{QueryDB: db})
			tool, _ := reg.Get("ah_market_top_sellers")
			resp := tool.Handler(context.Background(), json.RawMessage(c.args), "")
			got, _ := resp.(map[string]any)
			if got["error"] != nil {
				t.Fatalf("unexpected error: %v", got)
			}
			if m, _ := got["minListings"].(int); m != c.wantMin {
				t.Errorf("minListings: %v want %d", got["minListings"], c.wantMin)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Errorf("unmet mock expectations: %v", err)
			}
		})
	}
}

// TestAhMarketTopSellers_HouseIdFilter verifies the optional houseId binds a
// `WHERE houseid = ?` (before the LIMIT) and is echoed with its faction.
func TestAhMarketTopSellers_HouseIdFilter(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.ExpectExec(regexp.QuoteMeta("USE `acore_characters`")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta("WHERE houseid = ? LIMIT ?")).
		WithArgs(2, int64(ahTopSellersScanCap+1)).
		WillReturnRows(sqlmock.NewRows(ahSellerScanCols()))

	reg := NewRegistry()
	RegisterAhMarketTopSellersTool(reg, DBDeps{QueryDB: db})
	tool, _ := reg.Get("ah_market_top_sellers")
	resp := tool.Handler(context.Background(), json.RawMessage(`{"houseId":2}`), "")
	got, _ := resp.(map[string]any)
	if got["error"] != nil {
		t.Fatalf("unexpected error: %v", got)
	}
	if h, _ := got["houseId"].(int); h != 2 {
		t.Errorf("houseId echo: %v want 2", got["houseId"])
	}
	if f, _ := got["faction"].(string); f != "Alliance" {
		t.Errorf("faction echo: %v want Alliance", got["faction"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestAhMarketTopSellers_HouseIdAbsentWhenUnset confirms the houseId/faction keys
// are omitted entirely when no filter is passed (absent != zero-value).
func TestAhMarketTopSellers_HouseIdAbsentWhenUnset(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	expectEmptySellerScan(mock)

	out, err := collectAhTopSellers(context.Background(), db, time.Unix(1_700_000_000, 0), nil, 1, 25)
	if err != nil {
		t.Fatalf("collectAhTopSellers: %v", err)
	}
	if _, ok := out["houseId"]; ok {
		t.Errorf("houseId should be absent when unset, got %v", out["houseId"])
	}
	if _, ok := out["faction"]; ok {
		t.Errorf("faction should be absent when unset, got %v", out["faction"])
	}
}

// TestCollectAhTopSellers_Empty — empty market: sellers must be a non-nil empty
// slice, distinctSellers 0, unresolved 0, and NO stage-2/3 queries fire.
func TestCollectAhTopSellers_Empty(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	expectEmptySellerScan(mock)

	out, err := collectAhTopSellers(context.Background(), db, time.Unix(1_700_000_000, 0), nil, 1, 25)
	if err != nil {
		t.Fatalf("collectAhTopSellers: %v", err)
	}
	sl, ok := out["sellers"].([]*ahSeller)
	if !ok {
		t.Fatalf("sellers type: %T want []*ahSeller", out["sellers"])
	}
	if len(sl) != 0 {
		t.Errorf("sellers len: %d want 0", len(sl))
	}
	if d, _ := out["distinctSellers"].(int); d != 0 {
		t.Errorf("distinctSellers: %v want 0", out["distinctSellers"])
	}
	if u, _ := out["unresolved"].(int); u != 0 {
		t.Errorf("unresolved: %v want 0", out["unresolved"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestCollectAhTopSellers_Golden drives the full three-stage fold: per-seller
// listing/buyout/bid/deposit sums, the floor (min active buyout, auction-only
// rows excluded), the minListings filter, listings-desc + guid-asc ordering, the
// characters batch resolution (incl. a deleted-character miss -> unresolved), the
// account batch resolution + RNDBOT bot flag, and the gold strings.
func TestCollectAhTopSellers_Golden(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	now := time.Unix(1_700_000_000, 0)

	mock.ExpectExec(regexp.QuoteMeta("USE `acore_characters`")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta("FROM `auctionhouse` LIMIT ?")).
		WithArgs(int64(ahTopSellersScanCap + 1)).
		WillReturnRows(sqlmock.NewRows(ahSellerScanCols()).
			// Seller 100 (AHBot): 3 listings. Floor = 5000 (cheapest buyout);
			// one auction-only (buyout 0) row with an active bid.
			AddRow(2, int64(100), int64(50000), int64(0), int64(500), int64(0)).
			AddRow(2, int64(100), int64(5000), int64(0), int64(100), int64(0)).
			AddRow(6, int64(100), int64(0), int64(12000), int64(300), int64(999)).
			// Seller 200 (player): 2 listings, both with buyout, floor 9000.
			AddRow(7, int64(200), int64(20000), int64(0), int64(200), int64(0)).
			AddRow(7, int64(200), int64(9000), int64(0), int64(90), int64(0)).
			// Seller 300 (deleted character -> no characters row): 2 auction-only
			// listings -> survives minListings=2, floor 0, name unresolved.
			AddRow(7, int64(300), int64(0), int64(7000), int64(50), int64(123)).
			AddRow(7, int64(300), int64(0), int64(3000), int64(60), int64(0)).
			// Seller 400: only 1 listing -> filtered out by minListings=2.
			AddRow(7, int64(400), int64(1), int64(0), int64(1), int64(0)))

	// Stage 2: characters lookup. Seller 100 -> AHBOTXYZ (acct 11), 200 ->
	// Slayo (acct 22). 300 and 400 deliberately absent (300 = deleted char miss;
	// 400 was filtered before resolution so it must NOT be in the IN list).
	mock.ExpectQuery(regexp.QuoteMeta("FROM `characters` WHERE guid IN (?,?,?)")).
		WithArgs(int64(100), int64(200), int64(300)).
		WillReturnRows(sqlmock.NewRows([]string{"guid", "name", "account"}).
			AddRow(int64(100), "Ahbotzug", int64(11)).
			AddRow(int64(200), "Slayo", int64(22)))

	// Stage 3: account lookup over the two resolved accounts. 11 is RNDBOT* (bot),
	// 22 is a human.
	mock.ExpectExec(regexp.QuoteMeta("USE `acore_auth`")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta("FROM `account` WHERE id IN (?,?)")).
		WithArgs(int64(11), int64(22)).
		WillReturnRows(sqlmock.NewRows([]string{"id", "username"}).
			AddRow(int64(11), "RNDBOT0007").
			AddRow(int64(22), "OPERATOR"))

	// minListings=2 drops seller 400 (1 listing); topN=10 keeps the rest.
	out, err := collectAhTopSellers(context.Background(), db, now, nil, 2, 10)
	if err != nil {
		t.Fatalf("collectAhTopSellers: %v", err)
	}

	if s, _ := out["scanned"].(int); s != 8 {
		t.Errorf("scanned: %v want 8", out["scanned"])
	}
	if d, _ := out["distinctSellers"].(int); d != 3 {
		t.Errorf("distinctSellers: %v want 3 (100,200,300 after minListings=2 drops 400)", out["distinctSellers"])
	}
	if u, _ := out["unresolved"].(int); u != 1 {
		t.Errorf("unresolved: %v want 1 (seller 300 deleted char)", out["unresolved"])
	}

	sellers, _ := out["sellers"].([]*ahSeller)
	if len(sellers) != 3 {
		t.Fatalf("sellers len: %d want 3", len(sellers))
	}

	// Order: 100 (3 listings) > 200 (2) > 300 (1).
	s0 := sellers[0]
	if s0.ItemOwnerGuid != 100 || s0.Listings != 3 {
		t.Errorf("sellers[0]: %+v want guid=100 listings=3", s0)
	}
	if s0.WithBuyout != 2 || s0.WithActiveBid != 1 {
		t.Errorf("sellers[0] flags: withBuyout=%d withActiveBid=%d want 2/1", s0.WithBuyout, s0.WithActiveBid)
	}
	if s0.TotalBuyoutCopper != 55000 || s0.MinBuyoutCopper != 5000 {
		t.Errorf("sellers[0] buyout: total=%d min=%d want 55000/5000", s0.TotalBuyoutCopper, s0.MinBuyoutCopper)
	}
	if s0.TotalCurrentBidCopper != 12000 || s0.TotalDepositCopper != 900 {
		t.Errorf("sellers[0] bid/deposit: bid=%d deposit=%d want 12000/900", s0.TotalCurrentBidCopper, s0.TotalDepositCopper)
	}
	if s0.CharacterName != "Ahbotzug" || s0.AccountID != 11 || !s0.IsBot {
		t.Errorf("sellers[0] identity: name=%q acct=%d isBot=%v want Ahbotzug/11/true", s0.CharacterName, s0.AccountID, s0.IsBot)
	}
	if s0.TotalBuyoutGold != "5g50s" || s0.MinBuyoutGold != "50s" {
		t.Errorf("sellers[0] gold: total=%q min=%q want 5g50s/50s", s0.TotalBuyoutGold, s0.MinBuyoutGold)
	}

	s1 := sellers[1]
	if s1.ItemOwnerGuid != 200 || s1.Listings != 2 || s1.MinBuyoutCopper != 9000 {
		t.Errorf("sellers[1]: %+v want guid=200 listings=2 min=9000", s1)
	}
	if s1.CharacterName != "Slayo" || s1.AccountID != 22 || s1.IsBot {
		t.Errorf("sellers[1] identity: name=%q acct=%d isBot=%v want Slayo/22/false", s1.CharacterName, s1.AccountID, s1.IsBot)
	}

	// Seller 300: deleted character -> empty name, account 0, not a bot, floor 0
	// (only an auction-only listing).
	s2 := sellers[2]
	if s2.ItemOwnerGuid != 300 || s2.CharacterName != "" || s2.AccountID != 0 || s2.IsBot {
		t.Errorf("sellers[2] (deleted char): %+v want guid=300 empty-name acct0 not-bot", s2)
	}
	if s2.WithBuyout != 0 || s2.MinBuyoutCopper != 0 || s2.MinBuyoutGold != "0c" {
		t.Errorf("sellers[2] floor: withBuyout=%d min=%d gold=%q want 0/0/0c", s2.WithBuyout, s2.MinBuyoutCopper, s2.MinBuyoutGold)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestTopNAhMarketSellers_FilterSortSlice exercises the pure helper: minListings
// filter, listings-desc with guid-asc tiebreaker on ties, slice to n, and the
// pre-slice distinct count. Guards against Go map-iteration flake on ties.
func TestTopNAhMarketSellers_FilterSortSlice(t *testing.T) {
	m := map[int64]*ahSeller{
		30: {ItemOwnerGuid: 30, Listings: 5},
		10: {ItemOwnerGuid: 10, Listings: 5}, // tied with 30 -> 10 sorts first (guid asc)
		20: {ItemOwnerGuid: 20, Listings: 9}, // highest -> first overall
		40: {ItemOwnerGuid: 40, Listings: 1}, // below minListings=2 -> filtered
	}
	out, distinct := topNAhMarketSellers(m, 2, 2)
	if distinct != 3 {
		t.Errorf("distinct: %d want 3 (40 filtered by minListings)", distinct)
	}
	if len(out) != 2 {
		t.Fatalf("len: %d want 2 (sliced to n)", len(out))
	}
	if out[0].ItemOwnerGuid != 20 {
		t.Errorf("out[0]: %+v want guid=20 (count 9)", out[0])
	}
	if out[1].ItemOwnerGuid != 10 {
		t.Errorf("out[1]: %+v want guid=10 (count 5, guid-asc tiebreak over 30)", out[1])
	}
}

// TestTopNAhMarketSellers_EmptyNonNil — an all-filtered map still returns a
// non-nil slice (callers JSON-encode it as []).
func TestTopNAhMarketSellers_EmptyNonNil(t *testing.T) {
	m := map[int64]*ahSeller{
		1: {ItemOwnerGuid: 1, Listings: 1},
	}
	out, distinct := topNAhMarketSellers(m, 5, 10)
	if out == nil {
		t.Fatal("out is nil, want non-nil empty slice")
	}
	if len(out) != 0 || distinct != 0 {
		t.Errorf("len=%d distinct=%d want 0/0", len(out), distinct)
	}
}

// TestAnnotateSellerAccounts_DedupesAccountIds — two seller-characters on the
// same account bind the id ONCE in the IN list, and both rows get the username +
// bot flag. Sellers with account 0 (unresolved character) are skipped entirely.
func TestAnnotateSellerAccounts_DedupesAccountIds(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	sellers := []*ahSeller{
		{ItemOwnerGuid: 100, AccountID: 11},
		{ItemOwnerGuid: 101, AccountID: 11}, // same account -> deduped in IN list
		{ItemOwnerGuid: 102, AccountID: 0},  // unresolved char -> skipped
	}
	mock.ExpectQuery(regexp.QuoteMeta("FROM `account` WHERE id IN (?)")).
		WithArgs(int64(11)).
		WillReturnRows(sqlmock.NewRows([]string{"id", "username"}).
			AddRow(int64(11), "RNDBOTAAA"))

	if err := annotateSellerAccounts(context.Background(), conn, sellers); err != nil {
		t.Fatalf("annotateSellerAccounts: %v", err)
	}
	if !sellers[0].IsBot || sellers[0].AccountUsername != "RNDBOTAAA" {
		t.Errorf("sellers[0]: %+v want RNDBOTAAA/bot", sellers[0])
	}
	if !sellers[1].IsBot || sellers[1].AccountUsername != "RNDBOTAAA" {
		t.Errorf("sellers[1] (same account): %+v want RNDBOTAAA/bot", sellers[1])
	}
	if sellers[2].AccountUsername != "" || sellers[2].IsBot {
		t.Errorf("sellers[2] (acct 0): %+v want empty/not-bot", sellers[2])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestAnnotateSellerAccounts_NoResolvedAccounts — when every seller has account 0
// (all characters unresolved), NO account query fires.
func TestAnnotateSellerAccounts_NoResolvedAccounts(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	sellers := []*ahSeller{{ItemOwnerGuid: 100, AccountID: 0}}
	if err := annotateSellerAccounts(context.Background(), conn, sellers); err != nil {
		t.Fatalf("annotateSellerAccounts: %v", err)
	}
	// No ExpectQuery queued -> ExpectationsWereMet passes only if none fired.
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations (account query should not fire): %v", err)
	}
}

// TestAhSellerFold_FloorIgnoresAuctionOnly — the floor tracks the cheapest
// buyout>0 listing and ignores auction-only (buyout 0) rows entirely; a seller
// with only auction-only listings has floor 0.
func TestAhSellerFold_FloorIgnoresAuctionOnly(t *testing.T) {
	s := &ahSeller{}
	s.fold(0, 1000, 10, 5) // auction-only: no buyout, has bid
	s.fold(8000, 0, 20, 0) // buyout 8000
	s.fold(3000, 0, 30, 0) // buyout 3000 -> new floor
	s.fold(0, 0, 40, 0)    // auction-only: ignored for floor
	if s.Listings != 4 {
		t.Errorf("listings: %d want 4", s.Listings)
	}
	if s.WithBuyout != 2 {
		t.Errorf("withBuyout: %d want 2", s.WithBuyout)
	}
	if s.WithActiveBid != 1 {
		t.Errorf("withActiveBid: %d want 1", s.WithActiveBid)
	}
	if s.MinBuyoutCopper != 3000 {
		t.Errorf("min buyout (floor): %d want 3000", s.MinBuyoutCopper)
	}
	if s.TotalBuyoutCopper != 11000 || s.TotalCurrentBidCopper != 1000 || s.TotalDepositCopper != 100 {
		t.Errorf("sums: buyout=%d bid=%d deposit=%d want 11000/1000/100",
			s.TotalBuyoutCopper, s.TotalCurrentBidCopper, s.TotalDepositCopper)
	}

	empty := &ahSeller{}
	empty.fold(0, 500, 5, 0) // only auction-only
	if empty.MinBuyoutCopper != 0 || empty.hasBuyout {
		t.Errorf("auction-only seller floor: min=%d hasBuyout=%v want 0/false", empty.MinBuyoutCopper, empty.hasBuyout)
	}
}
