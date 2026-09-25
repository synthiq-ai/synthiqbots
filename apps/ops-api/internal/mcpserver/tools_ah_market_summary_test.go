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

// ahRowCols is the column set ah_market_summary scans, in order.
func ahRowCols() []string {
	return []string{"houseid", "itemowner", "buyoutprice", "lastbid", "startbid", "deposit", "time", "buyguid"}
}

// TestAhMarketSummary_DescriptionMentionsContext guards the description keywords
// — they shape which tool the agent reaches for when an operator asks "what's
// the auction house look like?" / "how much gold is locked in the AH?". Drop the
// market/faction framing and the agent falls back to a hand-written db_query or
// (wrongly) to the bot-facing bot_ah_search.
func TestAhMarketSummary_DescriptionMentionsContext(t *testing.T) {
	reg := NewRegistry()
	RegisterAhMarketSummaryTool(reg, DBDeps{})
	tool, ok := reg.Get("ah_market_summary")
	if !ok {
		t.Fatal("ah_market_summary not registered")
	}
	for _, kw := range []string{
		"acore_characters.auctionhouse", "ops_ro", "byHouse", "Alliance", "Horde", "Neutral",
		"houseid", "buyout", "deposit", "expiringWindowHours", "topSellers", "wow_player_lookup",
		"mod-ah-bot", "bot_ah_search", "Read-only", "CONFIG_ALLOW_TWO_SIDE_INTERACTION_AUCTION", "truncated",
	} {
		if !strings.Contains(tool.Description, kw) {
			t.Errorf("description missing %q: %s", kw, tool.Description)
		}
	}
}

// TestAhMarketSummary_ReadOnlyAnnotation pins readOnlyHint=true so a future
// copy-paste of a destructive sibling can't silently flip the gate.
func TestAhMarketSummary_ReadOnlyAnnotation(t *testing.T) {
	reg := NewRegistry()
	RegisterAhMarketSummaryTool(reg, DBDeps{})
	tool, _ := reg.Get("ah_market_summary")
	if !strings.Contains(string(tool.Annotations), `"readOnlyHint":true`) {
		t.Errorf("annotations missing readOnlyHint=true: %s", tool.Annotations)
	}
}

func TestAhMarketSummary_NoPoolReturnsError(t *testing.T) {
	reg := NewRegistry()
	RegisterAhMarketSummaryTool(reg, DBDeps{}) // QueryDB nil
	tool, _ := reg.Get("ah_market_summary")
	resp := tool.Handler(context.Background(), json.RawMessage(`{}`), "")
	got, _ := resp.(map[string]any)
	if msg, _ := got["error"].(string); !strings.Contains(msg, "ops_ro pool not configured") {
		t.Errorf("expected pool-not-configured error, got %v", got)
	}
}

// TestAhMarketSummary_ExpiringWindowClamps drives the handler and asserts the
// post-clamp expiringWindowHours echo (unset->default, zero->default,
// oversized->cap, in-range passthrough) and that the LIMIT bind is always
// scanCap+1 regardless of args.
func TestAhMarketSummary_ExpiringWindowClamps(t *testing.T) {
	cases := []struct {
		name     string
		args     string
		wantHour int
	}{
		{"unset uses default", `{}`, ahMarketSummaryDefaultExpireH},
		{"zero falls back to default", `{"expiringWindowHours":0}`, ahMarketSummaryDefaultExpireH},
		{"negative falls back to default", `{"expiringWindowHours":-9}`, ahMarketSummaryDefaultExpireH},
		{"oversized clamps to max", `{"expiringWindowHours":99999}`, ahMarketSummaryMaxExpireH},
		{"in-range passes through", `{"expiringWindowHours":48}`, 48},
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
			mock.ExpectQuery(regexp.QuoteMeta("FROM `auctionhouse` LIMIT ?")).
				WithArgs(int64(ahMarketSummaryScanCap + 1)).
				WillReturnRows(sqlmock.NewRows(ahRowCols()))

			reg := NewRegistry()
			RegisterAhMarketSummaryTool(reg, DBDeps{QueryDB: db})
			tool, _ := reg.Get("ah_market_summary")
			resp := tool.Handler(context.Background(), json.RawMessage(c.args), "")
			got, _ := resp.(map[string]any)
			if got["error"] != nil {
				t.Fatalf("unexpected error: %v", got)
			}
			if h, _ := got["expiringWindowHours"].(int); h != c.wantHour {
				t.Errorf("expiringWindowHours: %v want %d", got["expiringWindowHours"], c.wantHour)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Errorf("unmet mock expectations: %v", err)
			}
		})
	}
}

// TestAhMarketSummary_TopSellersPresence verifies the *int unset-vs-explicit-0
// distinction: unset (and any positive value) yields the topSellers list;
// explicit 0 or a negative (clamped to 0) omits the key entirely.
func TestAhMarketSummary_TopSellersPresence(t *testing.T) {
	cases := []struct {
		name    string
		args    string
		present bool
	}{
		{"unset -> default 10 -> present", `{}`, true},
		{"positive -> present", `{"topSellers":5}`, true},
		{"explicit zero -> omitted", `{"topSellers":0}`, false},
		{"negative clamps to zero -> omitted", `{"topSellers":-3}`, false},
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
			mock.ExpectQuery(regexp.QuoteMeta("FROM `auctionhouse` LIMIT ?")).
				WithArgs(int64(ahMarketSummaryScanCap + 1)).
				WillReturnRows(sqlmock.NewRows(ahRowCols()))

			reg := NewRegistry()
			RegisterAhMarketSummaryTool(reg, DBDeps{QueryDB: db})
			tool, _ := reg.Get("ah_market_summary")
			resp := tool.Handler(context.Background(), json.RawMessage(c.args), "")
			got, _ := resp.(map[string]any)
			if got["error"] != nil {
				t.Fatalf("unexpected error: %v", got)
			}
			if _, ok := got["topSellers"]; ok != c.present {
				t.Errorf("topSellers present=%v want %v", ok, c.present)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Errorf("unmet mock expectations: %v", err)
			}
		})
	}
}

// TestCollectAhMarketSummary_Empty — the empty-market case. byHouse and
// topSellers must be non-nil empty slices (callers expect arrays), totals all
// zero, pcts guarded to 0.
func TestCollectAhMarketSummary_Empty(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.ExpectExec(regexp.QuoteMeta("USE `acore_characters`")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta("FROM `auctionhouse` LIMIT ?")).
		WithArgs(int64(ahMarketSummaryScanCap + 1)).
		WillReturnRows(sqlmock.NewRows(ahRowCols()))

	out, err := collectAhMarketSummary(context.Background(), db, time.Unix(1_700_000_000, 0), 2*time.Hour, 10)
	if err != nil {
		t.Fatalf("collectAhMarketSummary: %v", err)
	}
	bh, ok := out["byHouse"].([]*ahHouseBucket)
	if !ok {
		t.Fatalf("byHouse type: %T want []*ahHouseBucket", out["byHouse"])
	}
	if len(bh) != 0 {
		t.Errorf("byHouse len: %d want 0", len(bh))
	}
	ts, ok := out["topSellers"].([]*ahTopSeller)
	if !ok {
		t.Fatalf("topSellers type: %T want []*ahTopSeller", out["topSellers"])
	}
	if len(ts) != 0 {
		t.Errorf("topSellers len: %d want 0", len(ts))
	}
	totals, _ := out["totals"].(map[string]any)
	if l, _ := totals["listings"].(int); l != 0 {
		t.Errorf("totals.listings: %v want 0", totals["listings"])
	}
	if p, _ := totals["withBuyoutPct"].(float64); p != 0 {
		t.Errorf("totals.withBuyoutPct: %v want 0 (div-by-zero guard)", totals["withBuyoutPct"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestCollectAhMarketSummary_TopSellersOmittedWhenZero — topSellers=0 must omit
// the key (and skip building the seller map).
func TestCollectAhMarketSummary_TopSellersOmittedWhenZero(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.ExpectExec(regexp.QuoteMeta("USE `acore_characters`")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta("FROM `auctionhouse` LIMIT ?")).
		WithArgs(int64(ahMarketSummaryScanCap + 1)).
		WillReturnRows(sqlmock.NewRows(ahRowCols()).
			AddRow(7, int64(100), int64(9000), int64(0), int64(8000), int64(100), int64(1_700_000_500), int64(0)))

	out, err := collectAhMarketSummary(context.Background(), db, time.Unix(1_700_000_000, 0), 2*time.Hour, 0)
	if err != nil {
		t.Fatalf("collectAhMarketSummary: %v", err)
	}
	if _, ok := out["topSellers"]; ok {
		t.Errorf("topSellers should be omitted when topSellers=0, got %v", out["topSellers"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestCollectAhMarketSummary_Golden drives the full fold across all three known
// houses plus an unknown house, exercising: byHouse houseId-asc order, per-house
// counts, the totals fold, expiringSoon window math (incl. the boundary == cutoff
// counts, just-past doesn't, time==0 guarded), withBuyout / withActiveBid flags,
// topSellers count-desc + guid-asc tiebreaker, and the human gold strings.
func TestCollectAhMarketSummary_Golden(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	now := time.Unix(1_700_000_000, 0)
	cutoff := now.Unix() + int64((2 * time.Hour).Seconds()) // 1_700_007_200

	mock.ExpectExec(regexp.QuoteMeta("USE `acore_characters`")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta("FROM `auctionhouse` LIMIT ?")).
		WithArgs(int64(ahMarketSummaryScanCap + 1)).
		WillReturnRows(sqlmock.NewRows(ahRowCols()).
			// Alliance (2): owner 100, has buyout, expires soon (now+3600).
			AddRow(2, int64(100), int64(50000), int64(0), int64(40000), int64(500), now.Unix()+3600, int64(0)).
			// Alliance (2): owner 100, auction-only (buyout 0) with an active bid, NOT soon (now+10000).
			AddRow(2, int64(100), int64(0), int64(12000), int64(10000), int64(300), now.Unix()+10000, int64(999)).
			// Horde (6): owner 200, big buyout, expires in 1s (soon).
			AddRow(6, int64(200), int64(250000), int64(0), int64(200000), int64(2000), now.Unix()+1, int64(0)).
			// Neutral (7): owner 100, buyout, expires EXACTLY at cutoff (== counts).
			AddRow(7, int64(100), int64(9000), int64(0), int64(8000), int64(100), cutoff, int64(0)).
			// Neutral (7): owner 300, malformed time==0 -> expiringSoon guard skips it.
			AddRow(7, int64(300), int64(0), int64(0), int64(5000), int64(50), int64(0), int64(0)).
			// Unknown house (99): owner 200, already-expired (now-100) still counts (>0 && <=cutoff).
			AddRow(99, int64(200), int64(1), int64(0), int64(1), int64(1), now.Unix()-100, int64(0)))

	out, err := collectAhMarketSummary(context.Background(), db, now, 2*time.Hour, 10)
	if err != nil {
		t.Fatalf("collectAhMarketSummary: %v", err)
	}

	if s, _ := out["scanned"].(int); s != 6 {
		t.Errorf("scanned: %v want 6", out["scanned"])
	}
	if tr, _ := out["truncated"].(bool); tr {
		t.Errorf("truncated: %v want false", out["truncated"])
	}

	// byHouse order: 2, 6, 7, 99.
	bh, _ := out["byHouse"].([]*ahHouseBucket)
	if len(bh) != 4 {
		t.Fatalf("byHouse len: %d want 4", len(bh))
	}
	if bh[0].HouseID != 2 || bh[0].Faction != "Alliance" {
		t.Errorf("bh[0]: %+v want Alliance/2", bh[0])
	}
	if bh[1].HouseID != 6 || bh[1].Faction != "Horde" {
		t.Errorf("bh[1]: %+v want Horde/6", bh[1])
	}
	if bh[2].HouseID != 7 || bh[2].Faction != "Neutral" {
		t.Errorf("bh[2]: %+v want Neutral/7", bh[2])
	}
	if bh[3].HouseID != 99 || bh[3].Faction != "house-99" {
		t.Errorf("bh[3]: %+v want house-99/99", bh[3])
	}

	// Alliance: 2 listings, 1 buyout, 1 active bid, 1 expiring soon.
	a := bh[0]
	if a.Listings != 2 || a.WithBuyout != 1 || a.WithActiveBid != 1 || a.ExpiringSoon != 1 {
		t.Errorf("Alliance bucket: %+v", a)
	}
	if a.TotalBuyoutCopper != 50000 || a.TotalCurrentBidCopper != 12000 || a.TotalStartBidCopper != 50000 || a.TotalDepositCopper != 800 {
		t.Errorf("Alliance sums: %+v", a)
	}
	// Neutral: 2 listings, malformed-time row excluded from expiringSoon (1 soon).
	n := bh[2]
	if n.Listings != 2 || n.ExpiringSoon != 1 {
		t.Errorf("Neutral bucket: %+v (expiringSoon should exclude time==0 row)", n)
	}

	totals, _ := out["totals"].(map[string]any)
	if l, _ := totals["listings"].(int); l != 6 {
		t.Errorf("totals.listings: %v want 6", totals["listings"])
	}
	if wb, _ := totals["withBuyout"].(int); wb != 4 {
		t.Errorf("totals.withBuyout: %v want 4", totals["withBuyout"])
	}
	if ab, _ := totals["withActiveBid"].(int); ab != 1 {
		t.Errorf("totals.withActiveBid: %v want 1", totals["withActiveBid"])
	}
	if es, _ := totals["expiringSoon"].(int); es != 4 {
		t.Errorf("totals.expiringSoon: %v want 4 (Alliance1 + Horde + Neutral-cutoff + house99-expired)", totals["expiringSoon"])
	}
	if bc, _ := totals["totalBuyoutCopper"].(int64); bc != 309001 {
		t.Errorf("totals.totalBuyoutCopper: %v want 309001", totals["totalBuyoutCopper"])
	}
	if dc, _ := totals["totalDepositCopper"].(int64); dc != 2951 {
		t.Errorf("totals.totalDepositCopper: %v want 2951", totals["totalDepositCopper"])
	}
	if g, _ := totals["totalBuyoutGold"].(string); g != "30g90s1c" {
		t.Errorf("totals.totalBuyoutGold: %q want 30g90s1c", g)
	}
	if g, _ := totals["totalCurrentBidGold"].(string); g != "1g20s" {
		t.Errorf("totals.totalCurrentBidGold: %q want 1g20s", g)
	}
	if g, _ := totals["totalDepositGold"].(string); g != "29s51c" {
		t.Errorf("totals.totalDepositGold: %q want 29s51c", g)
	}
	if p, _ := totals["withBuyoutPct"].(float64); p != 66.7 {
		t.Errorf("totals.withBuyoutPct: %v want 66.7", p)
	}
	if p, _ := totals["withActiveBidPct"].(float64); p != 16.7 {
		t.Errorf("totals.withActiveBidPct: %v want 16.7", p)
	}

	// topSellers: owner 100 (3 listings) > owner 200 (2) > owner 300 (1).
	ts, _ := out["topSellers"].([]*ahTopSeller)
	if len(ts) != 3 {
		t.Fatalf("topSellers len: %d want 3", len(ts))
	}
	if ts[0].ItemOwnerGuid != 100 || ts[0].Listings != 3 || ts[0].TotalBuyoutCopper != 59000 {
		t.Errorf("topSellers[0]: %+v want owner=100 listings=3 buyout=59000", ts[0])
	}
	if ts[1].ItemOwnerGuid != 200 || ts[1].Listings != 2 || ts[1].TotalBuyoutCopper != 250001 {
		t.Errorf("topSellers[1]: %+v want owner=200 listings=2 buyout=250001", ts[1])
	}
	if ts[2].ItemOwnerGuid != 300 || ts[2].Listings != 1 {
		t.Errorf("topSellers[2]: %+v want owner=300 listings=1", ts[2])
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

func TestAhHouseFaction(t *testing.T) {
	cases := map[int]string{2: "Alliance", 6: "Horde", 7: "Neutral", 0: "house-0", 99: "house-99"}
	for in, want := range cases {
		if got := ahHouseFaction(in); got != want {
			t.Errorf("ahHouseFaction(%d) = %q want %q", in, got, want)
		}
	}
}

func TestFormatCopper(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{0, "0c"},
		{-5, "0c"},
		{50, "50c"},
		{100, "1s"},
		{10000, "1g"},
		{10050, "1g50c"},
		{12000, "1g20s"},
		{309001, "30g90s1c"},
		{2951, "29s51c"},
	}
	for _, c := range cases {
		if got := formatCopper(c.in); got != c.want {
			t.Errorf("formatCopper(%d) = %q want %q", c.in, got, c.want)
		}
	}
}

func TestAhPct(t *testing.T) {
	cases := []struct {
		n, total int
		want     float64
	}{
		{4, 6, 66.7},
		{1, 6, 16.7},
		{1, 3, 33.3},
		{1, 2, 50},
		{0, 0, 0}, // div-by-zero guard
		{5, 0, 0}, // div-by-zero guard
		{3, 3, 100},
	}
	for _, c := range cases {
		if got := ahPct(c.n, c.total); got != c.want {
			t.Errorf("ahPct(%d,%d) = %v want %v", c.n, c.total, got, c.want)
		}
	}
}

// TestTopNAhSellers_TiebreakerAndSlice — count-desc with guid-asc tiebreaker on
// equal counts, then slice to n. Guards against Go map-iteration flake.
func TestTopNAhSellers_TiebreakerAndSlice(t *testing.T) {
	m := map[int64]*ahTopSeller{
		30: {ItemOwnerGuid: 30, Listings: 5},
		10: {ItemOwnerGuid: 10, Listings: 5}, // tied with 30 -> 10 sorts first (guid asc)
		20: {ItemOwnerGuid: 20, Listings: 9}, // highest count -> first overall
		40: {ItemOwnerGuid: 40, Listings: 1},
	}
	out := topNAhSellers(m, 2)
	if len(out) != 2 {
		t.Fatalf("len: %d want 2 (sliced to n)", len(out))
	}
	if out[0].ItemOwnerGuid != 20 {
		t.Errorf("out[0]: %+v want owner=20 (count 9)", out[0])
	}
	if out[1].ItemOwnerGuid != 10 {
		t.Errorf("out[1]: %+v want owner=10 (count 5, guid-asc tiebreak over 30)", out[1])
	}
}
