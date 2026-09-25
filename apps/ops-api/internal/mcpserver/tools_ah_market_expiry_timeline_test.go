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

// ahExpiryBucketCols is the column set the buckets census scans, in order.
func ahExpiryBucketCols() []string { return []string{"time", "buyoutprice"} }

// ahExpirySoonestCols is the column set the soonest join scans, in order.
func ahExpirySoonestCols() []string {
	return []string{"itemguid", "entry", "name", "Quality", "buyoutprice", "houseid", "time", "buyguid"}
}

// The literal SQL fragments the tests pin. Kept as consts so a query reword
// fails the test loudly rather than silently mismatching a mock.
const (
	ahExpiryBucketsNoFilterSQL = "SELECT `time`, buyoutprice FROM `auctionhouse` LIMIT ?"
	ahExpirySoonestOrderSQL    = "ORDER BY ah.`time` ASC, ah.itemguid ASC LIMIT ?"
)

// TestAhMarketExpiryTimeline_DescriptionMentionsContext guards the vocabulary
// that steers the agent here (the TIME axis) vs the price/count/composition ah_*
// tools — the buckets/soonest + expiry framing is load-bearing for selection.
func TestAhMarketExpiryTimeline_DescriptionMentionsContext(t *testing.T) {
	reg := NewRegistry()
	RegisterAhMarketExpiryTimelineTool(reg, DBDeps{})
	tool, ok := reg.Get("ah_market_expiry_timeline")
	if !ok {
		t.Fatal("ah_market_expiry_timeline not registered")
	}
	for _, kw := range []string{
		"acore_characters.auctionhouse", "item_instance", "acore_world.item_template", "ops_ro",
		"buckets", "soonest", "expired", "under1h", "over24h", "hasBidder", "qualityName", "Heirloom",
		"Alliance", "Horde", "Neutral", "houseId", "minBuyout", "includeExpired", "topN", "Read-only",
	} {
		if !strings.Contains(tool.Description, kw) {
			t.Errorf("description missing %q: %s", kw, tool.Description)
		}
	}
}

// TestAhMarketExpiryTimeline_ReadOnlyAnnotation pins readOnlyHint=true so a
// future copy-paste of a destructive sibling can't silently flip the gate.
func TestAhMarketExpiryTimeline_ReadOnlyAnnotation(t *testing.T) {
	reg := NewRegistry()
	RegisterAhMarketExpiryTimelineTool(reg, DBDeps{})
	tool, _ := reg.Get("ah_market_expiry_timeline")
	if !strings.Contains(string(tool.Annotations), `"readOnlyHint":true`) {
		t.Errorf("annotations missing readOnlyHint=true: %s", tool.Annotations)
	}
}

func TestAhMarketExpiryTimeline_NoPoolReturnsError(t *testing.T) {
	reg := NewRegistry()
	RegisterAhMarketExpiryTimelineTool(reg, DBDeps{}) // QueryDB nil
	tool, _ := reg.Get("ah_market_expiry_timeline")
	resp := tool.Handler(context.Background(), json.RawMessage(`{}`), "")
	got, _ := resp.(map[string]any)
	if msg, _ := got["error"].(string); !strings.Contains(msg, "ops_ro pool not configured") {
		t.Errorf("expected pool-not-configured error, got %v", got)
	}
}

// TestAhMarketExpiryTimeline_TopNClamps drives the handler and asserts the
// post-clamp topN echo and that the soonest LIMIT bind matches the clamped
// value. The buckets census runs first (empty), then the soonest list.
func TestAhMarketExpiryTimeline_TopNClamps(t *testing.T) {
	cases := []struct {
		name     string
		args     string
		wantTopN int
	}{
		{"unset uses default", `{}`, ahExpiryDefaultTop},
		{"zero falls back to default", `{"topN":0}`, ahExpiryDefaultTop},
		{"negative falls back to default", `{"topN":-3}`, ahExpiryDefaultTop},
		{"oversized clamps to max", `{"topN":99999}`, ahExpiryMaxTop},
		{"in-range passes through", `{"topN":42}`, 42},
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
			mock.ExpectQuery(regexp.QuoteMeta(ahExpiryBucketsNoFilterSQL)).
				WithArgs(int64(ahExpiryScanCap + 1)).
				WillReturnRows(sqlmock.NewRows(ahExpiryBucketCols()))
			mock.ExpectQuery(regexp.QuoteMeta(ahExpirySoonestOrderSQL)).
				WithArgs(sqlmock.AnyArg(), int64(c.wantTopN)).
				WillReturnRows(sqlmock.NewRows(ahExpirySoonestCols()))

			reg := NewRegistry()
			RegisterAhMarketExpiryTimelineTool(reg, DBDeps{QueryDB: db})
			tool, _ := reg.Get("ah_market_expiry_timeline")
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

// TestAhMarketExpiryTimeline_HouseIdRejected — a houseId outside {2,6,7} is
// rejected before any DB call so a typo can't masquerade as an empty market.
func TestAhMarketExpiryTimeline_HouseIdRejected(t *testing.T) {
	for _, bad := range []string{`{"houseId":0}`, `{"houseId":1}`, `{"houseId":3}`, `{"houseId":99}`} {
		db, mock, err := sqlmock.New()
		if err != nil {
			t.Fatal(err)
		}
		reg := NewRegistry()
		RegisterAhMarketExpiryTimelineTool(reg, DBDeps{QueryDB: db})
		tool, _ := reg.Get("ah_market_expiry_timeline")
		resp := tool.Handler(context.Background(), json.RawMessage(bad), "")
		got, _ := resp.(map[string]any)
		if msg, _ := got["error"].(string); !strings.Contains(msg, "houseId invalid") {
			t.Errorf("args %s: expected houseId-invalid error, got %v", bad, got)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("args %s: no query should run on reject: %v", bad, err)
		}
		db.Close()
	}
}

// TestAhMarketExpiryTimeline_MinBuyoutRejected — a negative floor is rejected
// before any DB call (buyoutprice is unsigned, so >= negative would match all).
func TestAhMarketExpiryTimeline_MinBuyoutRejected(t *testing.T) {
	for _, bad := range []string{`{"minBuyout":-1}`, `{"minBuyout":-99999}`} {
		db, mock, err := sqlmock.New()
		if err != nil {
			t.Fatal(err)
		}
		reg := NewRegistry()
		RegisterAhMarketExpiryTimelineTool(reg, DBDeps{QueryDB: db})
		tool, _ := reg.Get("ah_market_expiry_timeline")
		resp := tool.Handler(context.Background(), json.RawMessage(bad), "")
		got, _ := resp.(map[string]any)
		if msg, _ := got["error"].(string); !strings.Contains(msg, "minBuyout negative") {
			t.Errorf("args %s: expected minBuyout-negative error, got %v", bad, got)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("args %s: no query should run on reject: %v", bad, err)
		}
		db.Close()
	}
}

// TestAhMarketExpiryTimeline_Filters drives the handler with houseId+minBuyout
// and asserts the WHERE shape + bind order in BOTH the census and soonest
// queries, and the echoed houseId/faction/minBuyout keys.
func TestAhMarketExpiryTimeline_Filters(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.ExpectExec(regexp.QuoteMeta("USE `acore_characters`")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	// Census: filters then the scanCap+1 LIMIT.
	mock.ExpectQuery(regexp.QuoteMeta("FROM `auctionhouse` WHERE houseid = ? AND buyoutprice >= ? LIMIT ?")).
		WithArgs(int64(2), int64(5000), int64(ahExpiryScanCap+1)).
		WillReturnRows(sqlmock.NewRows(ahExpiryBucketCols()))
	// Soonest: same filters, then the default (exclude-expired) time bound, then LIMIT.
	mock.ExpectQuery(regexp.QuoteMeta("WHERE ah.houseid = ? AND ah.buyoutprice >= ? AND ah.`time` > ? ORDER BY")).
		WithArgs(int64(2), int64(5000), sqlmock.AnyArg(), int64(5)).
		WillReturnRows(sqlmock.NewRows(ahExpirySoonestCols()))

	reg := NewRegistry()
	RegisterAhMarketExpiryTimelineTool(reg, DBDeps{QueryDB: db})
	tool, _ := reg.Get("ah_market_expiry_timeline")
	resp := tool.Handler(context.Background(), json.RawMessage(`{"houseId":2,"minBuyout":5000,"topN":5}`), "")
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
	if mb, _ := got["minBuyout"].(int64); mb != 5000 {
		t.Errorf("minBuyout echo: %v want 5000", got["minBuyout"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestCollectAhExpiryTimeline_Golden folds a fixture across every window (incl. a
// malformed time<=0 row segregated from the expired bucket, an empty pre-seeded
// under24h window, and boundary-exact 1h) and verifies the soonest fields
// (faction, qualityName, secondsUntil, expired, hasBidder, gold rendering).
func TestCollectAhExpiryTimeline_Golden(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	now := time.Unix(1_700_000_000, 0)
	ne := now.Unix()
	mock.ExpectExec(regexp.QuoteMeta("USE `acore_characters`")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta(ahExpiryBucketsNoFilterSQL)).
		WithArgs(int64(ahExpiryScanCap + 1)).
		WillReturnRows(sqlmock.NewRows(ahExpiryBucketCols()).
			AddRow(ne-100, int64(5000)).     // expired (valid, <=now): 50s
			AddRow(ne+1800, int64(10000)).   // under1h
			AddRow(ne+3600, int64(1000)).    // under1h (boundary <=3600) -> count 2, sum 11000 = 1g10s
			AddRow(ne+10800, int64(20000)).  // under6h: 2g
			AddRow(ne+172800, int64(40000)). // over24h: 4g
			AddRow(int64(0), int64(99999)))  // malformed time=0 -> segregated, NOT expired
	// Soonest (exclude-expired default): time bound = now, LIMIT 15.
	mock.ExpectQuery(regexp.QuoteMeta(ahExpirySoonestOrderSQL)).
		WithArgs(ne, int64(15)).
		WillReturnRows(sqlmock.NewRows(ahExpirySoonestCols()).
			AddRow(int64(501), int64(4306), "Silk Cloth", 1, int64(10000), 2, ne+1800, int64(0)).
			AddRow(int64(502), int64(22445), "Arcane Dust", 2, int64(20000), 6, ne+10800, int64(777)).
			AddRow(int64(503), int64(49908), "Primordial Saronite", 4, int64(0), 7, ne+172800, int64(0)))

	out, err := collectAhExpiryTimeline(context.Background(), db, now, 15, nil, nil, false)
	if err != nil {
		t.Fatalf("collectAhExpiryTimeline: %v", err)
	}

	if ta, _ := out["totalAuctions"].(int); ta != 5 {
		t.Errorf("totalAuctions: %v want 5", out["totalAuctions"])
	}
	if mf, _ := out["malformedTimeRows"].(int); mf != 1 {
		t.Errorf("malformedTimeRows: %v want 1", out["malformedTimeRows"])
	}
	if tr, _ := out["truncated"].(bool); tr {
		t.Errorf("truncated: want false, got true")
	}
	if _, ok := out["houseId"]; ok {
		t.Errorf("houseId should be absent with no filter, got %v", out["houseId"])
	}

	buckets, ok := out["buckets"].([]*ahExpiryBucket)
	if !ok || len(buckets) != 5 {
		t.Fatalf("buckets: %T len %d want []*ahExpiryBucket len 5", out["buckets"], len(buckets))
	}
	want := []struct {
		window string
		count  int
		copper int64
		gold   string
	}{
		{"expired", 1, 5000, "50s"},
		{"under1h", 2, 11000, "1g10s"},
		{"under6h", 1, 20000, "2g"},
		{"under24h", 0, 0, "0c"}, // pre-seeded empty window still surfaces
		{"over24h", 1, 40000, "4g"},
	}
	for k, w := range want {
		b := buckets[k]
		if b.Window != w.window || b.Count != w.count || b.TotalBuyoutCopper != w.copper || b.TotalBuyoutGold != w.gold {
			t.Errorf("buckets[%d] = %+v want %+v", k, b, w)
		}
	}

	soonest, ok := out["soonest"].([]*ahExpiryListing)
	if !ok || len(soonest) != 3 {
		t.Fatalf("soonest: %T len %d want []*ahExpiryListing len 3", out["soonest"], len(soonest))
	}
	if ds, _ := out["distinctSoonest"].(int); ds != 3 {
		t.Errorf("distinctSoonest: %v want 3", out["distinctSoonest"])
	}

	s0 := soonest[0]
	if s0.ItemEntry != 4306 || s0.ItemName != "Silk Cloth" || s0.QualityName != "Common" || s0.Faction != "Alliance" {
		t.Errorf("soonest[0] identity: %+v", s0)
	}
	if s0.SecondsUntil != 1800 || s0.Expired || s0.HasBidder || s0.BuyoutGold != "1g" {
		t.Errorf("soonest[0] facets: %+v", s0)
	}
	s1 := soonest[1]
	if s1.QualityName != "Uncommon" || s1.Faction != "Horde" || !s1.HasBidder || s1.SecondsUntil != 10800 {
		t.Errorf("soonest[1] facets: %+v", s1)
	}
	s2 := soonest[2]
	if s2.QualityName != "Epic" || s2.Faction != "Neutral" || s2.BuyoutCopper != 0 || s2.BuyoutGold != "0c" {
		t.Errorf("soonest[2] facets: %+v", s2)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestCollectAhExpiryTimeline_IncludeExpired — includeExpired=true switches the
// soonest time bound from `> now` to `> 0` (so already-expired listings can
// appear) and marks a past-expiry listing expired with a negative secondsUntil.
func TestCollectAhExpiryTimeline_IncludeExpired(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	now := time.Unix(1_700_000_000, 0)
	ne := now.Unix()
	mock.ExpectExec(regexp.QuoteMeta("USE `acore_characters`")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta(ahExpiryBucketsNoFilterSQL)).
		WithArgs(int64(ahExpiryScanCap + 1)).
		WillReturnRows(sqlmock.NewRows(ahExpiryBucketCols()).AddRow(ne-500, int64(300)))
	// includeExpired=true -> the soonest time bound is 0, not now.
	mock.ExpectQuery(regexp.QuoteMeta(ahExpirySoonestOrderSQL)).
		WithArgs(int64(0), int64(15)).
		WillReturnRows(sqlmock.NewRows(ahExpirySoonestCols()).
			AddRow(int64(9), int64(4306), "Silk Cloth", 1, int64(300), 7, ne-500, int64(0)))

	out, err := collectAhExpiryTimeline(context.Background(), db, now, 15, nil, nil, true)
	if err != nil {
		t.Fatalf("collectAhExpiryTimeline: %v", err)
	}
	if ie, _ := out["includeExpired"].(bool); !ie {
		t.Errorf("includeExpired echo: want true")
	}
	soonest := out["soonest"].([]*ahExpiryListing)
	if len(soonest) != 1 {
		t.Fatalf("soonest len %d want 1", len(soonest))
	}
	if !soonest[0].Expired || soonest[0].SecondsUntil != -500 {
		t.Errorf("soonest[0] expired facets: %+v", soonest[0])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestCollectAhExpiryTimeline_Empty — an empty market still yields all five
// pre-seeded (zeroed) buckets and a non-nil empty soonest slice.
func TestCollectAhExpiryTimeline_Empty(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Unix(1_700_000_000, 0)
	mock.ExpectExec(regexp.QuoteMeta("USE `acore_characters`")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta(ahExpiryBucketsNoFilterSQL)).
		WithArgs(int64(ahExpiryScanCap + 1)).
		WillReturnRows(sqlmock.NewRows(ahExpiryBucketCols()))
	mock.ExpectQuery(regexp.QuoteMeta(ahExpirySoonestOrderSQL)).
		WithArgs(now.Unix(), int64(15)).
		WillReturnRows(sqlmock.NewRows(ahExpirySoonestCols()))

	out, err := collectAhExpiryTimeline(context.Background(), db, now, 15, nil, nil, false)
	if err != nil {
		t.Fatalf("collectAhExpiryTimeline: %v", err)
	}
	buckets, ok := out["buckets"].([]*ahExpiryBucket)
	if !ok || len(buckets) != 5 {
		t.Fatalf("buckets: %T len %d want 5", out["buckets"], len(buckets))
	}
	for _, b := range buckets {
		if b.Count != 0 || b.TotalBuyoutCopper != 0 || b.TotalBuyoutGold != "0c" {
			t.Errorf("empty bucket not zeroed: %+v", b)
		}
	}
	soonest, ok := out["soonest"].([]*ahExpiryListing)
	if !ok || soonest == nil || len(soonest) != 0 {
		t.Errorf("soonest: %v want non-nil empty slice", out["soonest"])
	}
	if ta, _ := out["totalAuctions"].(int); ta != 0 {
		t.Errorf("totalAuctions: %v want 0", out["totalAuctions"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestClassifyExpiryWindow pins the pure banding + boundary conditions (each
// window's inclusive upper edge and the just-over transition).
func TestClassifyExpiryWindow(t *testing.T) {
	cases := []struct {
		secs int64
		want string
	}{
		{-100, "expired"},
		{0, "expired"},
		{1, "under1h"},
		{3599, "under1h"},
		{3600, "under1h"},   // inclusive upper edge
		{3601, "under6h"},   // just over 1h
		{21600, "under6h"},  // inclusive upper edge (6h)
		{21601, "under24h"}, // just over 6h
		{86400, "under24h"}, // inclusive upper edge (24h)
		{86401, "over24h"},  // just over 24h
	}
	for _, c := range cases {
		if got := classifyExpiryWindow(c.secs); got != c.want {
			t.Errorf("classifyExpiryWindow(%d) = %q want %q", c.secs, got, c.want)
		}
	}
}
