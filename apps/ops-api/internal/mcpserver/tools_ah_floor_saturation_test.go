package mcpserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

// ahFloorSaturationFloorMapCols is the Stage-1 floor-map row column set.
func ahFloorSaturationFloorMapCols() []string {
	return []string{"itemEntry", "floor"}
}

// ahFloorSaturationScanCols is the Stage-2 per-listing scan row column set.
func ahFloorSaturationScanCols() []string {
	return []string{"itemEntry", "itemowner", "buyoutprice", "name", "Quality", "class"}
}

// expectAhFloorSaturationEmpty stages the USE + an empty floor-map + an empty
// listing scan so tests that only care about arg validation still satisfy the
// always-first two queries. The listing-scan LIMIT always binds scanCap+1 —
// topN only slices the Go-side leaderboard after the fold, it never bounds the
// SQL scan — so there is no topN parameter here.
func expectAhFloorSaturationEmpty(mock sqlmock.Sqlmock) {
	mock.ExpectExec(regexp.QuoteMeta("USE `acore_characters`")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta("MIN(ah.buyoutprice) AS floor")).
		WillReturnRows(sqlmock.NewRows(ahFloorSaturationFloorMapCols()))
	mock.ExpectQuery(regexp.QuoteMeta("LIMIT ?")).
		WithArgs(int64(ahFloorSaturationScanCap + 1)).
		WillReturnRows(sqlmock.NewRows(ahFloorSaturationScanCols()))
}

// TestAhFloorSaturation_DescriptionMentionsContext guards the keywords that
// steer the agent here vs ah_undercutters (seller grain) / ah_item_price_dispersion
// (spread) / a hand-written db_query — the floor-crowding framing is load-bearing.
func TestAhFloorSaturation_DescriptionMentionsContext(t *testing.T) {
	reg := NewRegistry()
	RegisterAhFloorSaturationTool(reg, DBDeps{})
	tool, ok := reg.Get("ah_floor_saturation")
	if !ok {
		t.Fatal("ah_floor_saturation not registered")
	}
	for _, kw := range []string{
		"acore_characters.auctionhouse", "item_instance", "acore_world.item_template",
		"ah_undercutters", "ah_item_price_dispersion", "ops_ro",
		"floorSellers", "floorListings", "saturationPct", "price war",
		"qualityName", "Heirloom", "className", "topN", "sortBy", "minSellers",
		"minQuality", "houseId", "Alliance", "Horde", "Neutral", "Read-only",
	} {
		if !strings.Contains(tool.Description, kw) {
			t.Errorf("description missing %q: %s", kw, tool.Description)
		}
	}
}

// TestAhFloorSaturation_ReadOnlyAnnotation pins readOnlyHint=true so a future
// copy-paste of a destructive sibling can't silently flip the gate.
func TestAhFloorSaturation_ReadOnlyAnnotation(t *testing.T) {
	reg := NewRegistry()
	RegisterAhFloorSaturationTool(reg, DBDeps{})
	tool, _ := reg.Get("ah_floor_saturation")
	if !strings.Contains(string(tool.Annotations), `"readOnlyHint":true`) {
		t.Errorf("annotations missing readOnlyHint=true: %s", tool.Annotations)
	}
}

func TestAhFloorSaturation_NoPoolReturnsError(t *testing.T) {
	reg := NewRegistry()
	RegisterAhFloorSaturationTool(reg, DBDeps{}) // QueryDB nil
	tool, _ := reg.Get("ah_floor_saturation")
	resp := tool.Handler(context.Background(), json.RawMessage(`{}`), "")
	got, _ := resp.(map[string]any)
	if msg, _ := got["error"].(string); !strings.Contains(msg, "ops_ro pool not configured") {
		t.Errorf("expected pool-not-configured error, got %v", got)
	}
}

// TestAhFloorSaturation_TopNClamps drives the handler and asserts the post-clamp
// topN echo (unset->default, zero/negative->default, oversized->max, in-range
// passthrough) and that the listing-scan LIMIT bind is unaffected (topN only
// slices the Go-side leaderboard, it never bounds the SQL scan).
func TestAhFloorSaturation_TopNClamps(t *testing.T) {
	cases := []struct {
		name     string
		args     string
		wantTopN int
	}{
		{"unset uses default", `{}`, ahFloorSaturationDefaultTop},
		{"zero falls back to default", `{"topN":0}`, ahFloorSaturationDefaultTop},
		{"negative falls back to default", `{"topN":-7}`, ahFloorSaturationDefaultTop},
		{"oversized clamps to max", `{"topN":99999}`, ahFloorSaturationMaxTop},
		{"in-range passes through", `{"topN":42}`, 42},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			expectAhFloorSaturationEmpty(mock)

			reg := NewRegistry()
			RegisterAhFloorSaturationTool(reg, DBDeps{QueryDB: db})
			tool, _ := reg.Get("ah_floor_saturation")
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

// TestAhFloorSaturation_MinSellersClamps mirrors the topN clamp test for the
// minSellers soft floor: default 2, clamp <1 up to 1, in-range passthrough.
func TestAhFloorSaturation_MinSellersClamps(t *testing.T) {
	cases := []struct {
		name           string
		args           string
		wantMinSellers int
	}{
		{"unset uses default", `{}`, ahFloorSaturationDefaultMinSellers},
		{"zero clamps to 1", `{"minSellers":0}`, 1},
		{"negative clamps to 1", `{"minSellers":-3}`, 1},
		{"in-range passes through", `{"minSellers":5}`, 5},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			expectAhFloorSaturationEmpty(mock)

			reg := NewRegistry()
			RegisterAhFloorSaturationTool(reg, DBDeps{QueryDB: db})
			tool, _ := reg.Get("ah_floor_saturation")
			resp := tool.Handler(context.Background(), json.RawMessage(c.args), "")
			got, _ := resp.(map[string]any)
			if got["error"] != nil {
				t.Fatalf("unexpected error: %v", got)
			}
			if n, _ := got["minSellers"].(int); n != c.wantMinSellers {
				t.Errorf("minSellers: %v want %d", got["minSellers"], c.wantMinSellers)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Errorf("unmet mock expectations: %v", err)
			}
		})
	}
}

// TestAhFloorSaturation_InvalidSortByRejected — an out-of-allowlist sortBy is
// rejected (no DB call at all) so a typo can't quietly change the ranking axis.
func TestAhFloorSaturation_InvalidSortByRejected(t *testing.T) {
	for _, bad := range []string{`{"sortBy":"frobnicate"}`, `{"sortBy":"FLOORSELLERS"}`, `{"sortBy":"price; DROP"}`} {
		db, mock, err := sqlmock.New()
		if err != nil {
			t.Fatal(err)
		}
		reg := NewRegistry()
		RegisterAhFloorSaturationTool(reg, DBDeps{QueryDB: db})
		tool, _ := reg.Get("ah_floor_saturation")
		resp := tool.Handler(context.Background(), json.RawMessage(bad), "")
		got, _ := resp.(map[string]any)
		if msg, _ := got["error"].(string); !strings.Contains(msg, "invalid sortBy") {
			t.Errorf("args %s: expected invalid-sortBy error, got %v", bad, got)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("args %s: no query should run on reject: %v", bad, err)
		}
		db.Close()
	}
}

// TestAhFloorSaturation_MinQualityRejected — out-of-range minQuality is rejected
// (no DB call) so a typo can't masquerade as an empty market.
func TestAhFloorSaturation_MinQualityRejected(t *testing.T) {
	for _, bad := range []string{`{"minQuality":-1}`, `{"minQuality":8}`, `{"minQuality":99}`} {
		db, mock, err := sqlmock.New()
		if err != nil {
			t.Fatal(err)
		}
		reg := NewRegistry()
		RegisterAhFloorSaturationTool(reg, DBDeps{QueryDB: db})
		tool, _ := reg.Get("ah_floor_saturation")
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

// TestAhFloorSaturation_InvalidHouseIdRejected — a houseId outside {2,6,7} is
// rejected (not clamped) so a bad id can't masquerade as an empty house.
func TestAhFloorSaturation_InvalidHouseIdRejected(t *testing.T) {
	for _, bad := range []string{`{"houseId":1}`, `{"houseId":0}`, `{"houseId":99}`} {
		db, mock, err := sqlmock.New()
		if err != nil {
			t.Fatal(err)
		}
		reg := NewRegistry()
		RegisterAhFloorSaturationTool(reg, DBDeps{QueryDB: db})
		tool, _ := reg.Get("ah_floor_saturation")
		resp := tool.Handler(context.Background(), json.RawMessage(bad), "")
		got, _ := resp.(map[string]any)
		if msg, _ := got["error"].(string); !strings.Contains(msg, "invalid houseId") {
			t.Errorf("args %s: expected invalid-houseId error, got %v", bad, got)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("args %s: no query should run on reject: %v", bad, err)
		}
		db.Close()
	}
}

// TestAhFloorSaturation_HouseIdFilter asserts houseId binds on BOTH the floor-map
// query and the listing-scan query (so the floor is compared per-house), and that
// the response echoes houseId + the resolved faction.
func TestAhFloorSaturation_HouseIdFilter(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.ExpectExec(regexp.QuoteMeta("USE `acore_characters`")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta("WHERE ah.buyoutprice > 0 AND ah.houseid = ? GROUP BY ii.itemEntry")).
		WithArgs(int64(6)).
		WillReturnRows(sqlmock.NewRows(ahFloorSaturationFloorMapCols()))
	mock.ExpectQuery(regexp.QuoteMeta("WHERE ah.buyoutprice > 0 AND ah.houseid = ? LIMIT ?")).
		WithArgs(int64(6), int64(ahFloorSaturationScanCap+1)).
		WillReturnRows(sqlmock.NewRows(ahFloorSaturationScanCols()))

	reg := NewRegistry()
	RegisterAhFloorSaturationTool(reg, DBDeps{QueryDB: db})
	tool, _ := reg.Get("ah_floor_saturation")
	resp := tool.Handler(context.Background(), json.RawMessage(`{"houseId":6}`), "")
	got, _ := resp.(map[string]any)
	if got["error"] != nil {
		t.Fatalf("unexpected error: %v", got)
	}
	if h, _ := got["houseId"].(int); h != 6 {
		t.Errorf("houseId: %v want 6", got["houseId"])
	}
	if f, _ := got["faction"].(string); f != "Horde" {
		t.Errorf("faction: %v want Horde", got["faction"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestAhFloorSaturation_MinQualityFilter asserts minQuality binds ONLY on the
// listing scan (it is not part of the floor computation) and is echoed when set.
func TestAhFloorSaturation_MinQualityFilter(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.ExpectExec(regexp.QuoteMeta("USE `acore_characters`")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta("WHERE ah.buyoutprice > 0 GROUP BY ii.itemEntry")).
		WithArgs().
		WillReturnRows(sqlmock.NewRows(ahFloorSaturationFloorMapCols()))
	mock.ExpectQuery(regexp.QuoteMeta("WHERE ah.buyoutprice > 0 AND it.Quality >= ? LIMIT ?")).
		WithArgs(int64(4), int64(ahFloorSaturationScanCap+1)).
		WillReturnRows(sqlmock.NewRows(ahFloorSaturationScanCols()))

	reg := NewRegistry()
	RegisterAhFloorSaturationTool(reg, DBDeps{QueryDB: db})
	tool, _ := reg.Get("ah_floor_saturation")
	resp := tool.Handler(context.Background(), json.RawMessage(`{"minQuality":4}`), "")
	got, _ := resp.(map[string]any)
	if got["error"] != nil {
		t.Fatalf("unexpected error: %v", got)
	}
	if q, _ := got["minQuality"].(int); q != 4 {
		t.Errorf("minQuality: %v want 4", got["minQuality"])
	}
	if _, ok := got["houseId"]; ok {
		t.Errorf("houseId should be absent with no filter, got %v", got["houseId"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestCollectAhFloorSaturation_Golden folds a 3-item floor map against an
// 8-row listing scan proving: a cross-seller TIE at one item's floor (3 sellers),
// a single-seller floor dropped by the default minSellers=2 filter, a
// non-floor-priced listing counted in totalListings but not floorListings, and
// the honest census totals independent of the minSellers cut.
func TestCollectAhFloorSaturation_Golden(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	now := time.Unix(1_700_000_000, 0)
	mock.ExpectExec(regexp.QuoteMeta("USE `acore_characters`")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	// Floor map: item 100's floor is 5000, item 200's floor is 9000, item 300's
	// floor is 1000 (a single-seller floor, filtered out by the default minSellers=2).
	mock.ExpectQuery(regexp.QuoteMeta("MIN(ah.buyoutprice) AS floor")).
		WillReturnRows(sqlmock.NewRows(ahFloorSaturationFloorMapCols()).
			AddRow(int64(100), int64(5000)).
			AddRow(int64(200), int64(9000)).
			AddRow(int64(300), int64(1000)))
	mock.ExpectQuery(regexp.QuoteMeta("LIMIT ?")).
		WithArgs(int64(ahFloorSaturationScanCap + 1)).
		WillReturnRows(sqlmock.NewRows(ahFloorSaturationScanCols()).
			// Item 100 (Epic weapon): sellers 10/20/30 ALL tied at floor 5000 — the
			// genuine price-war tie — plus seller 40 posted above the floor at 6000.
			AddRow(int64(100), int64(10), int64(5000), "Gladiator's Greatsword", 4, 2).
			AddRow(int64(100), int64(20), int64(5000), "Gladiator's Greatsword", 4, 2).
			AddRow(int64(100), int64(30), int64(5000), "Gladiator's Greatsword", 4, 2).
			AddRow(int64(100), int64(40), int64(6000), "Gladiator's Greatsword", 4, 2).
			// Item 200 (Rare trade good): only seller 50 at the floor 9000, plus
			// seller 60 above it — a single-seller floor, dropped by minSellers=2.
			AddRow(int64(200), int64(50), int64(9000), "Frost Lotus", 3, 7).
			AddRow(int64(200), int64(60), int64(9500), "Frost Lotus", 3, 7).
			// Item 300 (Common trade good): seller 70 at the floor 1000.
			AddRow(int64(300), int64(70), int64(1000), "Frostweave Cloth", 1, 7))

	out, err := collectAhFloorSaturation(context.Background(), db, now, 15, "floorSellers", ahFloorSaturationDefaultMinSellers, nil, nil)
	if err != nil {
		t.Fatalf("collectAhFloorSaturation: %v", err)
	}
	if s, _ := out["sortBy"].(string); s != "floorSellers" {
		t.Errorf("sortBy: %v want floorSellers", out["sortBy"])
	}
	if sc, _ := out["scanned"].(int); sc != 7 {
		t.Errorf("scanned: %v want 7", out["scanned"])
	}
	if tr, _ := out["truncated"].(bool); tr {
		t.Errorf("truncated: %v want false", out["truncated"])
	}
	if mt, _ := out["matched"].(int); mt != 1 {
		t.Errorf("matched: %v want 1 (only item 100 clears minSellers=2)", out["matched"])
	}

	totals, ok := out["totals"].(map[string]any)
	if !ok {
		t.Fatalf("totals type: %T want map[string]any", out["totals"])
	}
	// Honest census: 3 distinct items scanned, 5 floored listings (3+1+1),
	// 7 total listings — independent of the minSellers=2 cut.
	if di, _ := totals["distinctItems"].(int); di != 3 {
		t.Errorf("totals.distinctItems: %v want 3", totals["distinctItems"])
	}
	if fl, _ := totals["totalFlooredListings"].(int); fl != 5 {
		t.Errorf("totals.totalFlooredListings: %v want 5", totals["totalFlooredListings"])
	}
	if tls, _ := totals["totalListingsScanned"].(int); tls != 7 {
		t.Errorf("totals.totalListingsScanned: %v want 7", totals["totalListingsScanned"])
	}

	items, ok := out["items"].([]*ahFloorSaturationItem)
	if !ok || len(items) != 1 {
		t.Fatalf("items: %T len %d want []*ahFloorSaturationItem len 1", out["items"], len(items))
	}
	it0 := items[0]
	if it0.ItemEntry != 100 || it0.ItemName != "Gladiator's Greatsword" {
		t.Errorf("items[0] identity: %+v", it0)
	}
	if it0.Quality != 4 || it0.QualityName != "Epic" || it0.Class != 2 || it0.ClassName != "Weapon" {
		t.Errorf("items[0] enums: %+v", it0)
	}
	if it0.FloorSellers != 3 {
		t.Errorf("items[0] floorSellers: %v want 3 (the cross-seller tie)", it0.FloorSellers)
	}
	if it0.FloorListings != 3 {
		t.Errorf("items[0] floorListings: %v want 3", it0.FloorListings)
	}
	if it0.TotalListings != 4 {
		t.Errorf("items[0] totalListings: %v want 4 (3 at floor + 1 above)", it0.TotalListings)
	}
	if it0.SaturationPct != 75.0 {
		t.Errorf("items[0] saturationPct: %v want 75.0", it0.SaturationPct)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestCollectAhFloorSaturation_SortKeys proves each sortBy key produces a
// distinct top-of-leaderboard ordering (the comparator, not the SQL, drives
// order here since this is a fold-in-Go leaderboard).
func TestCollectAhFloorSaturation_SortKeys(t *testing.T) {
	newFixture := func(t *testing.T) *sql.DB {
		t.Helper()
		db, mock, err := sqlmock.New()
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { db.Close() })
		mock.ExpectExec(regexp.QuoteMeta("USE `acore_characters`")).
			WillReturnResult(sqlmock.NewResult(0, 0))
		mock.ExpectQuery(regexp.QuoteMeta("MIN(ah.buyoutprice) AS floor")).
			WillReturnRows(sqlmock.NewRows(ahFloorSaturationFloorMapCols()).
				AddRow(int64(100), int64(1000)).
				AddRow(int64(200), int64(1000)))
		mock.ExpectQuery(regexp.QuoteMeta("LIMIT ?")).
			WithArgs(int64(ahFloorSaturationScanCap + 1)).
			WillReturnRows(sqlmock.NewRows(ahFloorSaturationScanCols()).
				// Item 100: 2 sellers at the floor, out of 2 total listings -> 100% saturation.
				AddRow(int64(100), int64(1), int64(1000), "Item A", 1, 1).
				AddRow(int64(100), int64(2), int64(1000), "Item A", 1, 1).
				// Item 200: 3 sellers at the floor, out of 10 total listings -> 30% saturation
				// (more floorSellers + floorListings, but a LOWER saturationPct than item 100).
				AddRow(int64(200), int64(10), int64(1000), "Item B", 1, 1).
				AddRow(int64(200), int64(20), int64(1000), "Item B", 1, 1).
				AddRow(int64(200), int64(30), int64(1000), "Item B", 1, 1).
				AddRow(int64(200), int64(40), int64(2000), "Item B", 1, 1).
				AddRow(int64(200), int64(40), int64(2000), "Item B", 1, 1).
				AddRow(int64(200), int64(40), int64(2000), "Item B", 1, 1).
				AddRow(int64(200), int64(40), int64(2000), "Item B", 1, 1).
				AddRow(int64(200), int64(40), int64(2000), "Item B", 1, 1))
		return db
	}

	cases := []struct {
		sortKey    string
		wantTopEnt int64
	}{
		{"floorSellers", 200}, // item 200 has 3 floor-sellers vs item 100's 2
		{"floorListings", 200},
		{"saturation", 100}, // item 100's 100% saturation beats item 200's 30%
	}
	for _, c := range cases {
		t.Run(c.sortKey, func(t *testing.T) {
			db := newFixture(t)
			out, err := collectAhFloorSaturation(context.Background(), db, time.Unix(1_700_000_000, 0), 15, c.sortKey, 1, nil, nil)
			if err != nil {
				t.Fatalf("collectAhFloorSaturation: %v", err)
			}
			items, _ := out["items"].([]*ahFloorSaturationItem)
			if len(items) == 0 {
				t.Fatal("items empty")
			}
			if items[0].ItemEntry != c.wantTopEnt {
				t.Errorf("top item for sortKey %s: %d want %d", c.sortKey, items[0].ItemEntry, c.wantTopEnt)
			}
		})
	}
}

// TestCollectAhFloorSaturation_Empty — an empty market yields a non-nil empty
// items slice and zeroed honest totals.
func TestCollectAhFloorSaturation_Empty(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	expectAhFloorSaturationEmpty(mock)

	out, err := collectAhFloorSaturation(context.Background(), db, time.Unix(1_700_000_000, 0), ahFloorSaturationDefaultTop, "floorSellers", ahFloorSaturationDefaultMinSellers, nil, nil)
	if err != nil {
		t.Fatalf("collectAhFloorSaturation: %v", err)
	}
	items, ok := out["items"].([]*ahFloorSaturationItem)
	if !ok {
		t.Fatalf("items type: %T want []*ahFloorSaturationItem", out["items"])
	}
	if items == nil || len(items) != 0 {
		t.Errorf("items: %v want non-nil empty slice", items)
	}
	totals, _ := out["totals"].(map[string]any)
	if di, _ := totals["distinctItems"].(int); di != 0 {
		t.Errorf("totals.distinctItems: %v want 0", totals["distinctItems"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestTopNAhFloorSaturation_MinSellersFilterAndTiebreak is a pure-unit test of
// the filter+sort+slice helper: two items tied on floorSellers=2 break the tie
// itemEntry-asc, and an item below minSellers is dropped from `matched` too.
func TestTopNAhFloorSaturation_MinSellersFilterAndTiebreak(t *testing.T) {
	m := map[int64]*ahFloorSaturationItem{
		300: {ItemEntry: 300, FloorSellers: 2},
		100: {ItemEntry: 100, FloorSellers: 2},
		200: {ItemEntry: 200, FloorSellers: 1}, // dropped by minSellers=2
	}
	out, matched := topNAhFloorSaturation(m, 2, "floorSellers", 15)
	if matched != 2 {
		t.Errorf("matched: %d want 2", matched)
	}
	if len(out) != 2 || out[0].ItemEntry != 100 || out[1].ItemEntry != 300 {
		t.Errorf("tiebreak order: %+v want [100, 300]", out)
	}
}

// TestAhFloorSaturationItem_FinalizeEmptyNonNil proves finalize() is safe on an
// item with zero floor-matched listings (no floorOwners map ever allocated).
func TestAhFloorSaturationItem_FinalizeEmptyNonNil(t *testing.T) {
	it := &ahFloorSaturationItem{ItemEntry: 1}
	it.fold(10, false)
	it.finalize()
	if it.FloorSellers != 0 {
		t.Errorf("FloorSellers: %d want 0", it.FloorSellers)
	}
	if it.SaturationPct != 0 {
		t.Errorf("SaturationPct: %v want 0", it.SaturationPct)
	}
	if it.TotalListings != 1 {
		t.Errorf("TotalListings: %d want 1", it.TotalListings)
	}
}
