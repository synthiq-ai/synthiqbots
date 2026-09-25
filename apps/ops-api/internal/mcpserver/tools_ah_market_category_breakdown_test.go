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

// ahCategoryRowCols is the column set ah_market_category_breakdown scans, in order.
func ahCategoryRowCols() []string {
	return []string{"class", "listings", "distinctItems", "withBuyout", "totalQuantity", "totalBuyout", "minBuyout"}
}

// ahCategoryTailRe matches the closing GROUP BY / ORDER BY / LIMIT shared by every
// query the tool issues — used so a mock can pin the bound args without repeating
// the full SELECT.
const ahCategoryTailRe = "GROUP BY it.class ORDER BY listings DESC, it.class ASC LIMIT ?"

// TestAhMarketCategoryBreakdown_DescriptionMentionsContext guards the keywords
// that steer the agent here vs ah_market_summary (totals) or ah_market_top_items
// (per item) — the by-category composition vocabulary is load-bearing for tool
// selection.
func TestAhMarketCategoryBreakdown_DescriptionMentionsContext(t *testing.T) {
	reg := NewRegistry()
	RegisterAhMarketCategoryBreakdownTool(reg, DBDeps{})
	tool, ok := reg.Get("ah_market_category_breakdown")
	if !ok {
		t.Fatal("ah_market_category_breakdown not registered")
	}
	for _, kw := range []string{
		"acore_characters.auctionhouse", "item_instance", "acore_world.item_template",
		"ah_market_summary", "ah_market_top_items", "ops_ro", "composition", "className",
		"Trade Goods", "Armor", "Glyph", "listingsPct", "houseId", "minQuality", "topN",
		"Alliance", "Horde", "Neutral", "Read-only",
	} {
		if !strings.Contains(tool.Description, kw) {
			t.Errorf("description missing %q: %s", kw, tool.Description)
		}
	}
}

// TestAhMarketCategoryBreakdown_ReadOnlyAnnotation pins readOnlyHint=true so a
// future copy-paste of a destructive sibling can't silently flip the gate.
func TestAhMarketCategoryBreakdown_ReadOnlyAnnotation(t *testing.T) {
	reg := NewRegistry()
	RegisterAhMarketCategoryBreakdownTool(reg, DBDeps{})
	tool, _ := reg.Get("ah_market_category_breakdown")
	if !strings.Contains(string(tool.Annotations), `"readOnlyHint":true`) {
		t.Errorf("annotations missing readOnlyHint=true: %s", tool.Annotations)
	}
}

func TestAhMarketCategoryBreakdown_NoPoolReturnsError(t *testing.T) {
	reg := NewRegistry()
	RegisterAhMarketCategoryBreakdownTool(reg, DBDeps{}) // QueryDB nil
	tool, _ := reg.Get("ah_market_category_breakdown")
	resp := tool.Handler(context.Background(), json.RawMessage(`{}`), "")
	got, _ := resp.(map[string]any)
	if msg, _ := got["error"].(string); !strings.Contains(msg, "ops_ro pool not configured") {
		t.Errorf("expected pool-not-configured error, got %v", got)
	}
}

// TestAhMarketCategoryBreakdown_TopNClamps drives the handler and asserts the
// post-clamp topN echo (unset->default, zero/negative->default, oversized->max,
// in-range passthrough). Unlike ah_market_top_items, the LIMIT bind is always
// scanCap+1 (topN is applied in Go after the grand totals are folded), so the
// query expectation pins scanCap+1 in every case.
func TestAhMarketCategoryBreakdown_TopNClamps(t *testing.T) {
	cases := []struct {
		name     string
		args     string
		wantTopN int
	}{
		{"unset uses default", `{}`, ahCategoryDefaultTop},
		{"zero falls back to default", `{"topN":0}`, ahCategoryDefaultTop},
		{"negative falls back to default", `{"topN":-7}`, ahCategoryDefaultTop},
		{"oversized clamps to max", `{"topN":99999}`, ahCategoryMaxTop},
		{"in-range passes through", `{"topN":12}`, 12},
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
			mock.ExpectQuery(regexp.QuoteMeta(ahCategoryTailRe)).
				WithArgs(int64(ahCategoryScanCap + 1)).
				WillReturnRows(sqlmock.NewRows(ahCategoryRowCols()))

			reg := NewRegistry()
			RegisterAhMarketCategoryBreakdownTool(reg, DBDeps{QueryDB: db})
			tool, _ := reg.Get("ah_market_category_breakdown")
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

// TestAhMarketCategoryBreakdown_MinQualityRejected — out-of-range minQuality is
// rejected (no DB call) so a typo can't masquerade as an empty market. QueryDB is
// non-nil to prove the reject fires AFTER the pool check but before any query.
func TestAhMarketCategoryBreakdown_MinQualityRejected(t *testing.T) {
	for _, bad := range []string{`{"minQuality":-1}`, `{"minQuality":8}`, `{"minQuality":99}`} {
		db, mock, err := sqlmock.New()
		if err != nil {
			t.Fatal(err)
		}
		reg := NewRegistry()
		RegisterAhMarketCategoryBreakdownTool(reg, DBDeps{QueryDB: db})
		tool, _ := reg.Get("ah_market_category_breakdown")
		resp := tool.Handler(context.Background(), json.RawMessage(bad), "")
		got, _ := resp.(map[string]any)
		if msg, _ := got["error"].(string); !strings.Contains(msg, "minQuality out of range") {
			t.Errorf("args %s: expected out-of-range error, got %v", bad, got)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("args %s: no query should run on reject: %v", bad, err)
		}
		db.Close()
	}
}

// TestAhMarketCategoryBreakdown_Filters drives the handler with each filter
// combination and asserts the WHERE clause shape, the bound args (filter values
// then the scanCap+1 LIMIT), and the echoed houseId/faction/minQuality keys.
func TestAhMarketCategoryBreakdown_Filters(t *testing.T) {
	cap1 := i(ahCategoryScanCap + 1)
	cases := []struct {
		name        string
		args        string
		whereSub    string
		wantArgs    []driverArg
		wantHouse   int // -1 = key absent
		wantFaction string
		wantMinQ    int // -1 = key absent
	}{
		{
			name:      "house only",
			args:      `{"houseId":7}`,
			whereSub:  "WHERE ah.houseid = ? GROUP BY",
			wantArgs:  []driverArg{i(7), cap1},
			wantHouse: 7, wantFaction: "Neutral", wantMinQ: -1,
		},
		{
			name:      "quality only",
			args:      `{"minQuality":3}`,
			whereSub:  "WHERE it.Quality >= ? GROUP BY",
			wantArgs:  []driverArg{i(3), cap1},
			wantHouse: -1, wantMinQ: 3,
		},
		{
			name:      "both filters",
			args:      `{"houseId":2,"minQuality":4,"topN":5}`,
			whereSub:  "WHERE ah.houseid = ? AND it.Quality >= ? GROUP BY",
			wantArgs:  []driverArg{i(2), i(4), cap1},
			wantHouse: 2, wantFaction: "Alliance", wantMinQ: 4,
		},
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
			mock.ExpectQuery(regexp.QuoteMeta(c.whereSub)).
				WithArgs(toValues(c.wantArgs)...).
				WillReturnRows(sqlmock.NewRows(ahCategoryRowCols()))

			reg := NewRegistry()
			RegisterAhMarketCategoryBreakdownTool(reg, DBDeps{QueryDB: db})
			tool, _ := reg.Get("ah_market_category_breakdown")
			resp := tool.Handler(context.Background(), json.RawMessage(c.args), "")
			got, _ := resp.(map[string]any)
			if got["error"] != nil {
				t.Fatalf("unexpected error: %v", got)
			}
			if c.wantHouse >= 0 {
				if h, _ := got["houseId"].(int); h != c.wantHouse {
					t.Errorf("houseId: %v want %d", got["houseId"], c.wantHouse)
				}
				if f, _ := got["faction"].(string); f != c.wantFaction {
					t.Errorf("faction: %v want %q", got["faction"], c.wantFaction)
				}
			} else if _, ok := got["houseId"]; ok {
				t.Errorf("houseId should be absent, got %v", got["houseId"])
			}
			if c.wantMinQ >= 0 {
				if q, _ := got["minQuality"].(int); q != c.wantMinQ {
					t.Errorf("minQuality: %v want %d", got["minQuality"], c.wantMinQ)
				}
			} else if _, ok := got["minQuality"]; ok {
				t.Errorf("minQuality should be absent, got %v", got["minQuality"])
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Errorf("unmet mock expectations: %v", err)
			}
		})
	}
}

// TestCollectAhCategoryBreakdown_Golden folds three category rows and verifies
// className mapping, listings/value pct against the market grand totals, the
// avg-over-withBuyout division, the floor (min) buyout, the human gold strings,
// the withBuyout==0 div-guard, a NULL minBuyout, the totals block, and that the
// handler trusts the SQL ORDER BY (no Go-side re-sort).
func TestCollectAhCategoryBreakdown_Golden(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	now := time.Unix(1_700_000_000, 0)
	mock.ExpectExec(regexp.QuoteMeta("USE `acore_characters`")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta(ahCategoryTailRe)).
		WithArgs(int64(ahCategoryScanCap + 1)).
		WillReturnRows(sqlmock.NewRows(ahCategoryRowCols()).
			// Trade Goods: 60 listings, all with buyout, avg 600000/60=1g, floor 10s, 60g total.
			AddRow(7, 60, 12, 60, int64(600), int64(600000), int64(1000)).
			// Armor: 30 listings, 20 with buyout, avg 300000/20=1g50s, floor 50s, 30g total.
			AddRow(4, 30, 20, 20, int64(30), int64(300000), int64(5000)).
			// Weapon: all auction-only -> withBuyout 0 -> avg guard 0, NULL floor -> 0.
			AddRow(2, 10, 8, 0, int64(10), int64(0), nil))

	out, err := collectAhCategoryBreakdown(context.Background(), db, now, 20, nil, nil)
	if err != nil {
		t.Fatalf("collectAhCategoryBreakdown: %v", err)
	}
	if dc, _ := out["distinctCategories"].(int); dc != 3 {
		t.Errorf("distinctCategories: %v want 3", out["distinctCategories"])
	}
	if sc, _ := out["scanned"].(int); sc != 3 {
		t.Errorf("scanned: %v want 3", out["scanned"])
	}
	if tr, _ := out["truncated"].(bool); tr {
		t.Errorf("truncated should be false, got %v", out["truncated"])
	}
	if _, ok := out["houseId"]; ok {
		t.Errorf("houseId should be absent with no filter, got %v", out["houseId"])
	}

	totals, _ := out["totals"].(map[string]any)
	if l, _ := totals["listings"].(int); l != 100 {
		t.Errorf("totals.listings: %v want 100", totals["listings"])
	}
	if d, _ := totals["distinctItems"].(int); d != 40 {
		t.Errorf("totals.distinctItems: %v want 40", totals["distinctItems"])
	}
	if wb, _ := totals["withBuyout"].(int); wb != 80 {
		t.Errorf("totals.withBuyout: %v want 80", totals["withBuyout"])
	}
	if q, _ := totals["totalQuantity"].(int64); q != 640 {
		t.Errorf("totals.totalQuantity: %v want 640", totals["totalQuantity"])
	}
	if g, _ := totals["totalBuyoutGold"].(string); g != "90g" {
		t.Errorf("totals.totalBuyoutGold: %v want 90g", totals["totalBuyoutGold"])
	}

	cats, ok := out["categories"].([]*ahCategory)
	if !ok || len(cats) != 3 {
		t.Fatalf("categories: %T len %d want []*ahCategory len 3", out["categories"], len(cats))
	}

	tg := cats[0]
	if tg.Class != 7 || tg.ClassName != "Trade Goods" {
		t.Errorf("categories[0] identity: %+v", tg)
	}
	if tg.Listings != 60 || tg.ListingsPct != 60.0 {
		t.Errorf("categories[0] listings/pct: %+v", tg)
	}
	if tg.DistinctItems != 12 || tg.WithBuyout != 60 || tg.TotalQuantity != 600 {
		t.Errorf("categories[0] counts: %+v", tg)
	}
	if tg.TotalBuyoutCopper != 600000 || tg.TotalBuyoutGold != "60g" || tg.TotalBuyoutPct != 66.7 {
		t.Errorf("categories[0] total/pct: %+v", tg)
	}
	if tg.AvgBuyoutCopper != 10000 || tg.AvgBuyoutGold != "1g" {
		t.Errorf("categories[0] avg: %+v", tg)
	}
	if tg.MinBuyoutCopper != 1000 || tg.MinBuyoutGold != "10s" {
		t.Errorf("categories[0] floor: %+v", tg)
	}

	ar := cats[1]
	if ar.ClassName != "Armor" || ar.ListingsPct != 30.0 || ar.TotalBuyoutPct != 33.3 {
		t.Errorf("categories[1] pct: %+v", ar)
	}
	if ar.AvgBuyoutGold != "1g50s" || ar.MinBuyoutGold != "50s" {
		t.Errorf("categories[1] gold: %+v", ar)
	}

	wp := cats[2]
	if wp.ClassName != "Weapon" || wp.WithBuyout != 0 {
		t.Errorf("categories[2] identity: %+v", wp)
	}
	if wp.AvgBuyoutCopper != 0 || wp.AvgBuyoutGold != "0c" {
		t.Errorf("categories[2] avg div-guard: %+v", wp)
	}
	if wp.MinBuyoutCopper != 0 || wp.MinBuyoutGold != "0c" || wp.TotalBuyoutPct != 0.0 {
		t.Errorf("categories[2] NULL floor / zero-value pct: %+v", wp)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestCollectAhCategoryBreakdown_TopNSlice — with more categories than topN, the
// display list is sliced but distinctCategories carries the pre-slice count AND
// the pcts are still computed against the WHOLE market (the sliced-out category's
// listings remain in the denominator). This is the load-bearing correctness
// property of folding grand totals before slicing.
func TestCollectAhCategoryBreakdown_TopNSlice(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectExec(regexp.QuoteMeta("USE `acore_characters`")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta(ahCategoryTailRe)).
		WithArgs(int64(ahCategoryScanCap + 1)).
		WillReturnRows(sqlmock.NewRows(ahCategoryRowCols()).
			AddRow(7, 60, 12, 60, int64(600), int64(600000), int64(1000)).
			AddRow(4, 30, 20, 20, int64(30), int64(300000), int64(5000)).
			AddRow(2, 10, 8, 0, int64(10), int64(0), nil))

	out, err := collectAhCategoryBreakdown(context.Background(), db, time.Unix(1_700_000_000, 0), 2, nil, nil)
	if err != nil {
		t.Fatalf("collectAhCategoryBreakdown: %v", err)
	}
	if dc, _ := out["distinctCategories"].(int); dc != 3 {
		t.Errorf("distinctCategories: %v want 3 (pre-slice)", out["distinctCategories"])
	}
	cats, _ := out["categories"].([]*ahCategory)
	if len(cats) != 2 {
		t.Fatalf("categories len %d want 2 (sliced to topN)", len(cats))
	}
	// Trade Goods pct is 60/100 = 60.0, NOT 60/90 — the sliced-out Weapon's 10
	// listings still count toward the market total.
	if cats[0].ListingsPct != 60.0 {
		t.Errorf("categories[0].ListingsPct: %v want 60.0 (denominator = full market)", cats[0].ListingsPct)
	}
	totals, _ := out["totals"].(map[string]any)
	if l, _ := totals["listings"].(int); l != 100 {
		t.Errorf("totals.listings: %v want 100 (includes sliced-out category)", totals["listings"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestCollectAhCategoryBreakdown_Empty — an empty market yields a non-nil empty
// categories slice (callers expect an array), distinctCategories 0, zeroed
// totals, and no pct div-by-zero.
func TestCollectAhCategoryBreakdown_Empty(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.ExpectExec(regexp.QuoteMeta("USE `acore_characters`")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta(ahCategoryTailRe)).
		WithArgs(int64(ahCategoryScanCap + 1)).
		WillReturnRows(sqlmock.NewRows(ahCategoryRowCols()))

	out, err := collectAhCategoryBreakdown(context.Background(), db, time.Unix(1_700_000_000, 0), 20, nil, nil)
	if err != nil {
		t.Fatalf("collectAhCategoryBreakdown: %v", err)
	}
	cats, ok := out["categories"].([]*ahCategory)
	if !ok {
		t.Fatalf("categories type: %T want []*ahCategory", out["categories"])
	}
	if cats == nil || len(cats) != 0 {
		t.Errorf("categories: %v want non-nil empty slice", cats)
	}
	if dc, _ := out["distinctCategories"].(int); dc != 0 {
		t.Errorf("distinctCategories: %v want 0", out["distinctCategories"])
	}
	totals, _ := out["totals"].(map[string]any)
	if l, _ := totals["listings"].(int); l != 0 {
		t.Errorf("totals.listings: %v want 0", totals["listings"])
	}
	if g, _ := totals["totalBuyoutGold"].(string); g != "0c" {
		t.Errorf("totals.totalBuyoutGold: %v want 0c", totals["totalBuyoutGold"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

func TestItemClassName(t *testing.T) {
	cases := map[int]string{
		0: "Consumable", 1: "Container", 2: "Weapon", 3: "Gem", 4: "Armor",
		5: "Reagent", 6: "Projectile", 7: "Trade Goods", 8: "Generic", 9: "Recipe",
		10: "Money", 11: "Quiver", 12: "Quest", 13: "Key", 14: "Permanent",
		15: "Miscellaneous", 16: "Glyph", 17: "class-17", -1: "class--1",
	}
	for in, want := range cases {
		if got := itemClassName(in); got != want {
			t.Errorf("itemClassName(%d) = %q want %q", in, got, want)
		}
	}
}

func TestAhCategoryValuePct(t *testing.T) {
	cases := []struct {
		n, total int64
		want     float64
	}{
		{0, 0, 0},              // div-by-zero guard
		{5, 0, 0},              // div-by-zero guard (nonzero numerator)
		{600000, 900000, 66.7}, // rounds to one decimal
		{300000, 900000, 33.3},
		{900000, 900000, 100.0},
		{1, 3, 33.3},
	}
	for _, c := range cases {
		if got := ahCategoryValuePct(c.n, c.total); got != c.want {
			t.Errorf("ahCategoryValuePct(%d,%d) = %v want %v", c.n, c.total, got, c.want)
		}
	}
}
