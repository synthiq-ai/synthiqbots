package mcpserver

import (
	"context"
	"database/sql/driver"
	"encoding/json"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

// ahTopItemsRowCols is the column set ah_market_top_items scans, in order.
func ahTopItemsRowCols() []string {
	return []string{"entry", "name", "Quality", "listings", "withBuyout", "totalQuantity", "totalBuyout", "minBuyout"}
}

// TestAhMarketTopItems_DescriptionMentionsContext guards the keywords that steer
// the agent here vs ah_market_summary (totals) or a hand-written db_query — the
// cross-DB join framing + item/rarity vocabulary is load-bearing for selection.
func TestAhMarketTopItems_DescriptionMentionsContext(t *testing.T) {
	reg := NewRegistry()
	RegisterAhMarketTopItemsTool(reg, DBDeps{})
	tool, ok := reg.Get("ah_market_top_items")
	if !ok {
		t.Fatal("ah_market_top_items not registered")
	}
	for _, kw := range []string{
		"acore_characters.auctionhouse", "item_instance", "acore_world.item_template", "ah_market_summary",
		"ops_ro", "qualityName", "Heirloom", "houseId", "minQuality", "topN", "listings",
		"floor price", "Alliance", "Horde", "Neutral", "Read-only",
	} {
		if !strings.Contains(tool.Description, kw) {
			t.Errorf("description missing %q: %s", kw, tool.Description)
		}
	}
}

// TestAhMarketTopItems_ReadOnlyAnnotation pins readOnlyHint=true so a future
// copy-paste of a destructive sibling can't silently flip the gate.
func TestAhMarketTopItems_ReadOnlyAnnotation(t *testing.T) {
	reg := NewRegistry()
	RegisterAhMarketTopItemsTool(reg, DBDeps{})
	tool, _ := reg.Get("ah_market_top_items")
	if !strings.Contains(string(tool.Annotations), `"readOnlyHint":true`) {
		t.Errorf("annotations missing readOnlyHint=true: %s", tool.Annotations)
	}
}

func TestAhMarketTopItems_NoPoolReturnsError(t *testing.T) {
	reg := NewRegistry()
	RegisterAhMarketTopItemsTool(reg, DBDeps{}) // QueryDB nil
	tool, _ := reg.Get("ah_market_top_items")
	resp := tool.Handler(context.Background(), json.RawMessage(`{}`), "")
	got, _ := resp.(map[string]any)
	if msg, _ := got["error"].(string); !strings.Contains(msg, "ops_ro pool not configured") {
		t.Errorf("expected pool-not-configured error, got %v", got)
	}
}

// TestAhMarketTopItems_TopNClamps drives the handler and asserts the post-clamp
// topN echo (unset->default, zero/negative->default, oversized->max, in-range
// passthrough) and that the LIMIT bind matches the clamped value.
func TestAhMarketTopItems_TopNClamps(t *testing.T) {
	cases := []struct {
		name     string
		args     string
		wantTopN int
	}{
		{"unset uses default", `{}`, ahTopItemsDefaultTop},
		{"zero falls back to default", `{"topN":0}`, ahTopItemsDefaultTop},
		{"negative falls back to default", `{"topN":-7}`, ahTopItemsDefaultTop},
		{"oversized clamps to max", `{"topN":99999}`, ahTopItemsMaxTop},
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
			mock.ExpectQuery(regexp.QuoteMeta("ORDER BY listings DESC, it.entry ASC LIMIT ?")).
				WithArgs(int64(c.wantTopN)).
				WillReturnRows(sqlmock.NewRows(ahTopItemsRowCols()))

			reg := NewRegistry()
			RegisterAhMarketTopItemsTool(reg, DBDeps{QueryDB: db})
			tool, _ := reg.Get("ah_market_top_items")
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

// TestAhMarketTopItems_MinQualityRejected — out-of-range minQuality is rejected
// (no DB call) so a typo can't masquerade as an empty market. QueryDB is non-nil
// to prove the reject fires AFTER the pool check but before any query.
func TestAhMarketTopItems_MinQualityRejected(t *testing.T) {
	for _, bad := range []string{`{"minQuality":-1}`, `{"minQuality":8}`, `{"minQuality":99}`} {
		db, mock, err := sqlmock.New()
		if err != nil {
			t.Fatal(err)
		}
		reg := NewRegistry()
		RegisterAhMarketTopItemsTool(reg, DBDeps{QueryDB: db})
		tool, _ := reg.Get("ah_market_top_items")
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

// TestAhMarketTopItems_Filters drives the handler with each filter combination
// and asserts the WHERE clause shape, the bound args (filter values then LIMIT),
// and the echoed houseId/faction/minQuality keys.
func TestAhMarketTopItems_Filters(t *testing.T) {
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
			wantArgs:  []driverArg{i(7), i(ahTopItemsDefaultTop)},
			wantHouse: 7, wantFaction: "Neutral", wantMinQ: -1,
		},
		{
			name:      "quality only",
			args:      `{"minQuality":3}`,
			whereSub:  "WHERE it.Quality >= ? GROUP BY",
			wantArgs:  []driverArg{i(3), i(ahTopItemsDefaultTop)},
			wantHouse: -1, wantMinQ: 3,
		},
		{
			name:      "both filters",
			args:      `{"houseId":2,"minQuality":4,"topN":5}`,
			whereSub:  "WHERE ah.houseid = ? AND it.Quality >= ? GROUP BY",
			wantArgs:  []driverArg{i(2), i(4), i(5)},
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
				WillReturnRows(sqlmock.NewRows(ahTopItemsRowCols()))

			reg := NewRegistry()
			RegisterAhMarketTopItemsTool(reg, DBDeps{QueryDB: db})
			tool, _ := reg.Get("ah_market_top_items")
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

// driverArg + helpers keep the WithArgs lists readable: bound ints arrive at the
// driver as int64, so wrap each expected value once.
type driverArg struct{ v int64 }

func i(n int) driverArg { return driverArg{int64(n)} }

func toValues(a []driverArg) []driver.Value {
	out := make([]driver.Value, len(a))
	for k, x := range a {
		out[k] = x.v
	}
	return out
}

// TestCollectAhTopItems_Golden folds three grouped rows and verifies qualityName
// mapping, the avg-over-withBuyout division, the floor (min) buyout, the human
// gold strings, the withBuyout==0 div-guard, a NULL minBuyout, and that the
// handler trusts the SQL ORDER BY (no Go-side re-sort).
func TestCollectAhTopItems_Golden(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	now := time.Unix(1_700_000_000, 0)
	mock.ExpectExec(regexp.QuoteMeta("USE `acore_characters`")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta("ORDER BY listings DESC, it.entry ASC LIMIT ?")).
		WithArgs(int64(15)).
		WillReturnRows(sqlmock.NewRows(ahTopItemsRowCols()).
			// Common: 10 listings, 8 with buyout, avg 80000/8=10000=1g, floor 50s.
			AddRow(int64(4306), "Silk Cloth", 1, 10, 8, int64(200), int64(80000), int64(5000)).
			// Uncommon: avg 309001/6=51500=5g15s, total 30g90s1c, floor 1g20s.
			AddRow(int64(22445), "Arcane Dust", 2, 6, 6, int64(60), int64(309001), int64(12000)).
			// Epic, all auction-only: withBuyout 0 -> avg guard 0, NULL floor -> 0.
			AddRow(int64(49908), "Primordial Saronite", 4, 3, 0, int64(3), int64(0), nil))

	out, err := collectAhTopItems(context.Background(), db, now, 15, nil, nil)
	if err != nil {
		t.Fatalf("collectAhTopItems: %v", err)
	}
	if di, _ := out["distinctItems"].(int); di != 3 {
		t.Errorf("distinctItems: %v want 3", out["distinctItems"])
	}
	if _, ok := out["houseId"]; ok {
		t.Errorf("houseId should be absent with no filter, got %v", out["houseId"])
	}
	items, ok := out["items"].([]*ahTopItem)
	if !ok || len(items) != 3 {
		t.Fatalf("items: %T len %d want []*ahTopItem len 3", out["items"], len(items))
	}

	it0 := items[0]
	if it0.ItemEntry != 4306 || it0.ItemName != "Silk Cloth" || it0.Quality != 1 || it0.QualityName != "Common" {
		t.Errorf("items[0] identity: %+v", it0)
	}
	if it0.Listings != 10 || it0.WithBuyout != 8 || it0.TotalQuantity != 200 {
		t.Errorf("items[0] counts: %+v", it0)
	}
	if it0.TotalBuyoutCopper != 80000 || it0.TotalBuyoutGold != "8g" {
		t.Errorf("items[0] total: %+v", it0)
	}
	if it0.AvgBuyoutCopper != 10000 || it0.AvgBuyoutGold != "1g" {
		t.Errorf("items[0] avg: %+v", it0)
	}
	if it0.MinBuyoutCopper != 5000 || it0.MinBuyoutGold != "50s" {
		t.Errorf("items[0] min: %+v", it0)
	}

	it1 := items[1]
	if it1.QualityName != "Uncommon" || it1.AvgBuyoutCopper != 51500 || it1.AvgBuyoutGold != "5g15s" {
		t.Errorf("items[1] avg: %+v", it1)
	}
	if it1.TotalBuyoutGold != "30g90s1c" || it1.MinBuyoutGold != "1g20s" {
		t.Errorf("items[1] gold: %+v", it1)
	}

	it2 := items[2]
	if it2.QualityName != "Epic" || it2.WithBuyout != 0 {
		t.Errorf("items[2] identity: %+v", it2)
	}
	if it2.AvgBuyoutCopper != 0 || it2.AvgBuyoutGold != "0c" {
		t.Errorf("items[2] avg div-guard: %+v", it2)
	}
	if it2.MinBuyoutCopper != 0 || it2.MinBuyoutGold != "0c" {
		t.Errorf("items[2] NULL floor: %+v", it2)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestCollectAhTopItems_Empty — an empty market yields a non-nil empty items
// slice (callers expect an array) and distinctItems 0.
func TestCollectAhTopItems_Empty(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.ExpectExec(regexp.QuoteMeta("USE `acore_characters`")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta("ORDER BY listings DESC, it.entry ASC LIMIT ?")).
		WithArgs(int64(15)).
		WillReturnRows(sqlmock.NewRows(ahTopItemsRowCols()))

	out, err := collectAhTopItems(context.Background(), db, time.Unix(1_700_000_000, 0), 15, nil, nil)
	if err != nil {
		t.Fatalf("collectAhTopItems: %v", err)
	}
	items, ok := out["items"].([]*ahTopItem)
	if !ok {
		t.Fatalf("items type: %T want []*ahTopItem", out["items"])
	}
	if items == nil || len(items) != 0 {
		t.Errorf("items: %v want non-nil empty slice", items)
	}
	if di, _ := out["distinctItems"].(int); di != 0 {
		t.Errorf("distinctItems: %v want 0", out["distinctItems"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

func TestItemQualityName(t *testing.T) {
	cases := map[int]string{
		0: "Poor", 1: "Common", 2: "Uncommon", 3: "Rare", 4: "Epic",
		5: "Legendary", 6: "Artifact", 7: "Heirloom", 9: "quality-9", -1: "quality--1",
	}
	for in, want := range cases {
		if got := itemQualityName(in); got != want {
			t.Errorf("itemQualityName(%d) = %q want %q", in, got, want)
		}
	}
}
