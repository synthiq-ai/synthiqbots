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

// ahBidderScanCols is the auctionhouse column set ah_bidder_activity scans, in
// order.
func ahBidderScanCols() []string {
	return []string{"houseid", "buyguid", "lastbid", "buyoutprice"}
}

// expectEmptyBidderScan queues the USE + auctionhouse scan with no rows. Used by
// the arg-clamp tests where the fold result doesn't matter (empty -> no
// stage-2/3 queries fire, so only these two expectations are needed).
func expectEmptyBidderScan(mock sqlmock.Sqlmock) {
	mock.ExpectExec(regexp.QuoteMeta("USE `acore_characters`")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta("FROM `auctionhouse`")).
		WillReturnRows(sqlmock.NewRows(ahBidderScanCols()))
}

// TestAhBidderActivity_DescriptionMentionsContext guards the description
// keywords — they shape which tool the agent reaches for when an operator asks
// "who are the biggest AH bidders?" / "are bots cornering the market via bids?".
// Drop the bidder/demand framing and the agent falls back to ah_market_summary or
// a hand-written buyguid join.
func TestAhBidderActivity_DescriptionMentionsContext(t *testing.T) {
	reg := NewRegistry()
	RegisterAhBidderActivityTool(reg, DBDeps{})
	tool, ok := reg.Get("ah_bidder_activity")
	if !ok {
		t.Fatal("ah_bidder_activity not registered")
	}
	for _, kw := range []string{
		"acore_characters.auctionhouse", "acore_characters.characters", "acore_auth.account",
		"ops_ro", "buyguid", "RNDBOT", "isBot", "AHBot", "mod-ah-bot",
		"ah_market_top_sellers", "wow_player_lookup", "Three-stage", "topN", "houseId",
		"minLeading", "unresolved", "demand", "leading", "committed", "Read-only",
	} {
		if !strings.Contains(tool.Description, kw) {
			t.Errorf("description missing %q", kw)
		}
	}
}

// TestAhBidderActivity_ReadOnlyAnnotation pins readOnlyHint=true so a future
// copy-paste of a destructive sibling can't silently flip the gate.
func TestAhBidderActivity_ReadOnlyAnnotation(t *testing.T) {
	reg := NewRegistry()
	RegisterAhBidderActivityTool(reg, DBDeps{})
	tool, _ := reg.Get("ah_bidder_activity")
	if !strings.Contains(string(tool.Annotations), `"readOnlyHint":true`) {
		t.Errorf("annotations missing readOnlyHint=true: %s", tool.Annotations)
	}
}

func TestAhBidderActivity_NoPoolReturnsError(t *testing.T) {
	reg := NewRegistry()
	RegisterAhBidderActivityTool(reg, DBDeps{}) // QueryDB nil
	tool, _ := reg.Get("ah_bidder_activity")
	resp := tool.Handler(context.Background(), json.RawMessage(`{}`), "")
	got, _ := resp.(map[string]any)
	if msg, _ := got["error"].(string); !strings.Contains(msg, "ops_ro pool not configured") {
		t.Errorf("expected pool-not-configured error, got %v", got)
	}
}

// TestAhBidderActivity_TopNClamps drives the handler and asserts the post-clamp
// topN echo (unset->default, zero->default, oversized->cap, in-range passthrough)
// and that the LIMIT bind is always scanCap+1 regardless of args.
func TestAhBidderActivity_TopNClamps(t *testing.T) {
	cases := []struct {
		name     string
		args     string
		wantTopN int
	}{
		{"unset uses default", `{}`, ahBidderDefaultTopN},
		{"zero falls back to default", `{"topN":0}`, ahBidderDefaultTopN},
		{"negative falls back to default", `{"topN":-4}`, ahBidderDefaultTopN},
		{"oversized clamps to max", `{"topN":99999}`, ahBidderMaxTopN},
		{"in-range passes through", `{"topN":25}`, 25},
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
			// No houseId -> WHERE buyguid<>0 with no AND; LIMIT bind is scanCap+1.
			mock.ExpectQuery(regexp.QuoteMeta("WHERE buyguid <> 0 LIMIT ?")).
				WithArgs(int64(ahBidderScanCap + 1)).
				WillReturnRows(sqlmock.NewRows(ahBidderScanCols()))

			reg := NewRegistry()
			RegisterAhBidderActivityTool(reg, DBDeps{QueryDB: db})
			tool, _ := reg.Get("ah_bidder_activity")
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

// TestAhBidderActivity_MinLeadingClamps verifies the minLeading floor of 1
// (unset/zero/negative all clamp to 1) is echoed in the response.
func TestAhBidderActivity_MinLeadingClamps(t *testing.T) {
	cases := []struct {
		args    string
		wantMin int
	}{
		{`{}`, 1},
		{`{"minLeading":0}`, 1},
		{`{"minLeading":-5}`, 1},
		{`{"minLeading":3}`, 3},
	}
	for _, c := range cases {
		t.Run(c.args, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			expectEmptyBidderScan(mock)

			reg := NewRegistry()
			RegisterAhBidderActivityTool(reg, DBDeps{QueryDB: db})
			tool, _ := reg.Get("ah_bidder_activity")
			resp := tool.Handler(context.Background(), json.RawMessage(c.args), "")
			got, _ := resp.(map[string]any)
			if got["error"] != nil {
				t.Fatalf("unexpected error: %v", got)
			}
			if m, _ := got["minLeading"].(int); m != c.wantMin {
				t.Errorf("minLeading: %v want %d", got["minLeading"], c.wantMin)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Errorf("unmet mock expectations: %v", err)
			}
		})
	}
}

// TestAhBidderActivity_HouseIdFilter verifies the optional houseId binds an
// `AND houseid = ?` (after the buyguid<>0 predicate, before the LIMIT) and is
// echoed with its faction.
func TestAhBidderActivity_HouseIdFilter(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.ExpectExec(regexp.QuoteMeta("USE `acore_characters`")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta("WHERE buyguid <> 0 AND houseid = ? LIMIT ?")).
		WithArgs(6, int64(ahBidderScanCap+1)).
		WillReturnRows(sqlmock.NewRows(ahBidderScanCols()))

	reg := NewRegistry()
	RegisterAhBidderActivityTool(reg, DBDeps{QueryDB: db})
	tool, _ := reg.Get("ah_bidder_activity")
	resp := tool.Handler(context.Background(), json.RawMessage(`{"houseId":6}`), "")
	got, _ := resp.(map[string]any)
	if got["error"] != nil {
		t.Fatalf("unexpected error: %v", got)
	}
	if h, _ := got["houseId"].(int); h != 6 {
		t.Errorf("houseId echo: %v want 6", got["houseId"])
	}
	if f, _ := got["faction"].(string); f != "Horde" {
		t.Errorf("faction echo: %v want Horde", got["faction"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestAhBidderActivity_HouseIdRejected — an out-of-enum houseId is rejected
// (no DB call) so a typo can't masquerade as an empty market. QueryDB is non-nil
// to prove the reject fires AFTER the pool check but before any query.
func TestAhBidderActivity_HouseIdRejected(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	reg := NewRegistry()
	RegisterAhBidderActivityTool(reg, DBDeps{QueryDB: db})
	tool, _ := reg.Get("ah_bidder_activity")
	for _, bad := range []string{`{"houseId":0}`, `{"houseId":3}`, `{"houseId":8}`, `{"houseId":-1}`} {
		resp := tool.Handler(context.Background(), json.RawMessage(bad), "")
		got, _ := resp.(map[string]any)
		if msg, _ := got["error"].(string); !strings.Contains(msg, "invalid houseId") {
			t.Errorf("args %s: expected invalid-houseId error, got %v", bad, got)
		}
	}
	// No ExpectQuery/ExpectExec queued -> ExpectationsWereMet passes only if none fired.
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("no query should run on reject: %v", err)
	}
}

// TestAhBidderActivity_HouseIdAbsentWhenUnset confirms the houseId/faction keys
// are omitted entirely when no filter is passed (absent != zero-value).
func TestAhBidderActivity_HouseIdAbsentWhenUnset(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	expectEmptyBidderScan(mock)

	out, err := collectAhBidderActivity(context.Background(), db, time.Unix(1_700_000_000, 0), nil, 1, 10)
	if err != nil {
		t.Fatalf("collectAhBidderActivity: %v", err)
	}
	if _, ok := out["houseId"]; ok {
		t.Errorf("houseId should be absent when unset, got %v", out["houseId"])
	}
	if _, ok := out["faction"]; ok {
		t.Errorf("faction should be absent when unset, got %v", out["faction"])
	}
}

// TestCollectAhBidderActivity_Empty — no live bids: bidders must be a non-nil
// empty slice, distinctBidders 0, unresolved 0, realm totals 0, and NO stage-2/3
// queries fire.
func TestCollectAhBidderActivity_Empty(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	expectEmptyBidderScan(mock)

	out, err := collectAhBidderActivity(context.Background(), db, time.Unix(1_700_000_000, 0), nil, 1, 10)
	if err != nil {
		t.Fatalf("collectAhBidderActivity: %v", err)
	}
	bl, ok := out["bidders"].([]*ahBidder)
	if !ok {
		t.Fatalf("bidders type: %T want []*ahBidder", out["bidders"])
	}
	if len(bl) != 0 {
		t.Errorf("bidders len: %d want 0", len(bl))
	}
	if d, _ := out["distinctBidders"].(int); d != 0 {
		t.Errorf("distinctBidders: %v want 0", out["distinctBidders"])
	}
	if u, _ := out["unresolved"].(int); u != 0 {
		t.Errorf("unresolved: %v want 0", out["unresolved"])
	}
	if la, _ := out["totalLeadingAuctions"].(int); la != 0 {
		t.Errorf("totalLeadingAuctions: %v want 0", out["totalLeadingAuctions"])
	}
	if bc, _ := out["totalCommittedBidCopper"].(int64); bc != 0 {
		t.Errorf("totalCommittedBidCopper: %v want 0", out["totalCommittedBidCopper"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestCollectAhBidderActivity_Golden drives the full three-stage fold: per-bidder
// leadingAuctions/withBuyout/committed-bid/buyout-value sums + the max single bid,
// the minLeading filter, leadingAuctions-desc + guid-asc ordering, the characters
// batch resolution (incl. a deleted-character miss -> unresolved), the account
// batch resolution + RNDBOT bot flag, the honest realm totals, and the gold
// strings.
func TestCollectAhBidderActivity_Golden(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	now := time.Unix(1_700_000_000, 0)

	mock.ExpectExec(regexp.QuoteMeta("USE `acore_characters`")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta("WHERE buyguid <> 0 LIMIT ?")).
		WithArgs(int64(ahBidderScanCap + 1)).
		WillReturnRows(sqlmock.NewRows(ahBidderScanCols()).
			// Bidder 100 (AHBot): leads 3 auctions; one auction-only (buyout 0);
			// max single bid 12000.
			AddRow(2, int64(100), int64(5000), int64(50000)).
			AddRow(2, int64(100), int64(12000), int64(0)).
			AddRow(6, int64(100), int64(3000), int64(8000)).
			// Bidder 200 (player): leads 2 auctions, both with buyout.
			AddRow(7, int64(200), int64(9000), int64(20000)).
			AddRow(7, int64(200), int64(1000), int64(2000)).
			// Bidder 300 (deleted character -> no characters row): leads 2 auctions,
			// one auction-only -> withBuyout 1, name unresolved.
			AddRow(7, int64(300), int64(7000), int64(0)).
			AddRow(7, int64(300), int64(500), int64(3000)).
			// Bidder 400: only 1 leading auction -> filtered out by minLeading=2.
			AddRow(7, int64(400), int64(100), int64(1)))

	// Stage 2: characters lookup. 100 -> Botbidderx (acct 11), 200 -> Slayo
	// (acct 22). 300 deliberately absent (deleted-char miss); 400 filtered before
	// resolution so it must NOT be in the IN list.
	mock.ExpectQuery(regexp.QuoteMeta("FROM `characters` WHERE guid IN (?,?,?)")).
		WithArgs(int64(100), int64(200), int64(300)).
		WillReturnRows(sqlmock.NewRows([]string{"guid", "name", "account"}).
			AddRow(int64(100), "Botbidderx", int64(11)).
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

	// minLeading=2 drops bidder 400 (1 leading); topN=10 keeps the rest.
	out, err := collectAhBidderActivity(context.Background(), db, now, nil, 2, 10)
	if err != nil {
		t.Fatalf("collectAhBidderActivity: %v", err)
	}

	if s, _ := out["scanned"].(int); s != 8 {
		t.Errorf("scanned: %v want 8", out["scanned"])
	}
	if d, _ := out["distinctBidders"].(int); d != 3 {
		t.Errorf("distinctBidders: %v want 3 (100,200,300 after minLeading=2 drops 400)", out["distinctBidders"])
	}
	if u, _ := out["unresolved"].(int); u != 1 {
		t.Errorf("unresolved: %v want 1 (bidder 300 deleted char)", out["unresolved"])
	}
	// Honest realm totals span ALL scanned rows (incl. filtered bidder 400).
	if la, _ := out["totalLeadingAuctions"].(int); la != 8 {
		t.Errorf("totalLeadingAuctions: %v want 8", out["totalLeadingAuctions"])
	}
	if bc, _ := out["totalCommittedBidCopper"].(int64); bc != 37600 {
		t.Errorf("totalCommittedBidCopper: %v want 37600", out["totalCommittedBidCopper"])
	}
	if bg, _ := out["totalCommittedBidGold"].(string); bg != "3g76s" {
		t.Errorf("totalCommittedBidGold: %v want 3g76s", out["totalCommittedBidGold"])
	}

	bidders, _ := out["bidders"].([]*ahBidder)
	if len(bidders) != 3 {
		t.Fatalf("bidders len: %d want 3", len(bidders))
	}

	// Order: 100 (3 leading) > 200 (2) == 300 (2) -> guid-asc breaks the tie -> 200 before 300.
	b0 := bidders[0]
	if b0.BidderGuid != 100 || b0.LeadingAuctions != 3 {
		t.Errorf("bidders[0]: %+v want guid=100 leading=3", b0)
	}
	if b0.WithBuyout != 2 {
		t.Errorf("bidders[0] withBuyout: %d want 2", b0.WithBuyout)
	}
	if b0.TotalCommittedBidCopper != 20000 || b0.TotalBuyoutValueCopper != 58000 || b0.MaxBidCopper != 12000 {
		t.Errorf("bidders[0] sums: committed=%d buyoutValue=%d max=%d want 20000/58000/12000",
			b0.TotalCommittedBidCopper, b0.TotalBuyoutValueCopper, b0.MaxBidCopper)
	}
	if b0.CharacterName != "Botbidderx" || b0.AccountID != 11 || !b0.IsBot {
		t.Errorf("bidders[0] identity: name=%q acct=%d isBot=%v want Botbidderx/11/true", b0.CharacterName, b0.AccountID, b0.IsBot)
	}
	if b0.TotalCommittedBidGold != "2g" || b0.MaxBidGold != "1g20s" || b0.TotalBuyoutValueGold != "5g80s" {
		t.Errorf("bidders[0] gold: committed=%q max=%q buyoutValue=%q want 2g/1g20s/5g80s",
			b0.TotalCommittedBidGold, b0.MaxBidGold, b0.TotalBuyoutValueGold)
	}

	b1 := bidders[1]
	if b1.BidderGuid != 200 || b1.LeadingAuctions != 2 || b1.MaxBidCopper != 9000 {
		t.Errorf("bidders[1]: %+v want guid=200 leading=2 max=9000", b1)
	}
	if b1.CharacterName != "Slayo" || b1.AccountID != 22 || b1.IsBot {
		t.Errorf("bidders[1] identity: name=%q acct=%d isBot=%v want Slayo/22/false", b1.CharacterName, b1.AccountID, b1.IsBot)
	}

	// Bidder 300: deleted character -> empty name, account 0, not a bot; one
	// auction-only listing -> withBuyout 1.
	b2 := bidders[2]
	if b2.BidderGuid != 300 || b2.CharacterName != "" || b2.AccountID != 0 || b2.IsBot {
		t.Errorf("bidders[2] (deleted char): %+v want guid=300 empty-name acct0 not-bot", b2)
	}
	if b2.WithBuyout != 1 || b2.LeadingAuctions != 2 || b2.MaxBidCopper != 7000 {
		t.Errorf("bidders[2]: withBuyout=%d leading=%d max=%d want 1/2/7000", b2.WithBuyout, b2.LeadingAuctions, b2.MaxBidCopper)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestTopNAhBidders_FilterSortSlice exercises the pure helper: minLeading filter,
// leadingAuctions-desc with guid-asc tiebreaker on ties, slice to n, and the
// pre-slice distinct count. Guards against Go map-iteration flake on ties.
func TestTopNAhBidders_FilterSortSlice(t *testing.T) {
	m := map[int64]*ahBidder{
		30: {BidderGuid: 30, LeadingAuctions: 5},
		10: {BidderGuid: 10, LeadingAuctions: 5}, // tied with 30 -> 10 sorts first (guid asc)
		20: {BidderGuid: 20, LeadingAuctions: 9}, // highest -> first overall
		40: {BidderGuid: 40, LeadingAuctions: 1}, // below minLeading=2 -> filtered
	}
	out, distinct := topNAhBidders(m, 2, 2)
	if distinct != 3 {
		t.Errorf("distinct: %d want 3 (40 filtered by minLeading)", distinct)
	}
	if len(out) != 2 {
		t.Fatalf("len: %d want 2 (sliced to n)", len(out))
	}
	if out[0].BidderGuid != 20 {
		t.Errorf("out[0]: %+v want guid=20 (count 9)", out[0])
	}
	if out[1].BidderGuid != 10 {
		t.Errorf("out[1]: %+v want guid=10 (count 5, guid-asc tiebreak over 30)", out[1])
	}
}

// TestTopNAhBidders_EmptyNonNil — an all-filtered map still returns a non-nil
// slice (callers JSON-encode it as []).
func TestTopNAhBidders_EmptyNonNil(t *testing.T) {
	m := map[int64]*ahBidder{
		1: {BidderGuid: 1, LeadingAuctions: 1},
	}
	out, distinct := topNAhBidders(m, 5, 10)
	if out == nil {
		t.Fatal("out is nil, want non-nil empty slice")
	}
	if len(out) != 0 || distinct != 0 {
		t.Errorf("len=%d distinct=%d want 0/0", len(out), distinct)
	}
}

// TestAnnotateBidderAccounts_DedupesAccountIds — two bidder-characters on the same
// account bind the id ONCE in the IN list, and both rows get the username + bot
// flag. Bidders with account 0 (unresolved character) are skipped entirely.
func TestAnnotateBidderAccounts_DedupesAccountIds(t *testing.T) {
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

	bidders := []*ahBidder{
		{BidderGuid: 100, AccountID: 11},
		{BidderGuid: 101, AccountID: 11}, // same account -> deduped in IN list
		{BidderGuid: 102, AccountID: 0},  // unresolved char -> skipped
	}
	mock.ExpectQuery(regexp.QuoteMeta("FROM `account` WHERE id IN (?)")).
		WithArgs(int64(11)).
		WillReturnRows(sqlmock.NewRows([]string{"id", "username"}).
			AddRow(int64(11), "RNDBOTAAA"))

	if err := annotateBidderAccounts(context.Background(), conn, bidders); err != nil {
		t.Fatalf("annotateBidderAccounts: %v", err)
	}
	if !bidders[0].IsBot || bidders[0].AccountUsername != "RNDBOTAAA" {
		t.Errorf("bidders[0]: %+v want RNDBOTAAA/bot", bidders[0])
	}
	if !bidders[1].IsBot || bidders[1].AccountUsername != "RNDBOTAAA" {
		t.Errorf("bidders[1] (same account): %+v want RNDBOTAAA/bot", bidders[1])
	}
	if bidders[2].AccountUsername != "" || bidders[2].IsBot {
		t.Errorf("bidders[2] (acct 0): %+v want empty/not-bot", bidders[2])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestAnnotateBidderAccounts_NoResolvedAccounts — when every bidder has account 0
// (all characters unresolved), NO account query fires.
func TestAnnotateBidderAccounts_NoResolvedAccounts(t *testing.T) {
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

	bidders := []*ahBidder{{BidderGuid: 100, AccountID: 0}}
	if err := annotateBidderAccounts(context.Background(), conn, bidders); err != nil {
		t.Fatalf("annotateBidderAccounts: %v", err)
	}
	// No ExpectQuery queued -> ExpectationsWereMet passes only if none fired.
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations (account query should not fire): %v", err)
	}
}

// TestAhBidderFold_Sums — the fold accumulates leadingAuctions, withBuyout (only
// buyout>0 rows), the committed-bid + buyout-value sums, and tracks the max single
// leading bid.
func TestAhBidderFold_Sums(t *testing.T) {
	b := &ahBidder{}
	b.fold(5000, 50000) // has buyout; max so far 5000
	b.fold(12000, 0)    // auction-only (no buyout); new max 12000
	b.fold(3000, 8000)  // has buyout; below max
	if b.LeadingAuctions != 3 {
		t.Errorf("leadingAuctions: %d want 3", b.LeadingAuctions)
	}
	if b.WithBuyout != 2 {
		t.Errorf("withBuyout: %d want 2 (auction-only row excluded)", b.WithBuyout)
	}
	if b.TotalCommittedBidCopper != 20000 {
		t.Errorf("totalCommittedBid: %d want 20000", b.TotalCommittedBidCopper)
	}
	if b.TotalBuyoutValueCopper != 58000 {
		t.Errorf("totalBuyoutValue: %d want 58000", b.TotalBuyoutValueCopper)
	}
	if b.MaxBidCopper != 12000 {
		t.Errorf("maxBid: %d want 12000", b.MaxBidCopper)
	}
}
