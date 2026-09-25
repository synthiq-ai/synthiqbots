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

// ahUndercutFloorCols is the per-item floor query column set (itemEntry -> floor).
func ahUndercutFloorCols() []string { return []string{"itemEntry", "floor"} }

// ahUndercutScanCols is the per-listing scan column set, in scan order.
func ahUndercutScanCols() []string { return []string{"itemowner", "itemEntry", "buyoutprice"} }

// Query-distinctive fragments used to pin the two ordered stage-1 ExpectQuery
// calls: the floor query is the only one with MIN(...); the scan is the only one
// selecting itemowner.
const (
	ahUndercutFloorQ = "MIN(ah.buyoutprice)"
	ahUndercutScanQ  = "SELECT ah.itemowner, ii.itemEntry, ah.buyoutprice"
)

// expectEmptyUndercut queues USE + an empty floor query + an empty scan. With no
// owners folded, no stage-2/3 (characters/account) queries fire, so these three
// expectations are all the arg-clamp tests need.
func expectEmptyUndercut(mock sqlmock.Sqlmock) {
	mock.ExpectExec(regexp.QuoteMeta("USE `acore_characters`")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta(ahUndercutFloorQ)).
		WillReturnRows(sqlmock.NewRows(ahUndercutFloorCols()))
	mock.ExpectQuery(regexp.QuoteMeta(ahUndercutScanQ)).
		WillReturnRows(sqlmock.NewRows(ahUndercutScanCols()))
}

// TestAhUndercutters_DescriptionMentionsContext guards the vocabulary that steers
// the agent here vs ah_item_price_dispersion (per-item spread, no seller) or
// ah_market_top_sellers (raw listing count, not floor-presence) — the
// floor-holder / undercutter framing is load-bearing for selection.
func TestAhUndercutters_DescriptionMentionsContext(t *testing.T) {
	reg := NewRegistry()
	RegisterAhUndercuttersTool(reg, DBDeps{})
	tool, ok := reg.Get("ah_undercutters")
	if !ok {
		t.Fatal("ah_undercutters not registered")
	}
	for _, kw := range []string{
		"acore_characters.auctionhouse", "item_instance", "acore_auth.account.username", "ops_ro",
		"itemowner", "RNDBOT", "isBot", "AHBot", "mod-ah-bot", "floor",
		"floorHeld", "listings", "distinctItemsUndercut", "floorHoldRatePct", "unresolved",
		"ah_item_price_dispersion", "ah_market_top_sellers",
		"topN", "sortBy", "minFloors", "houseId", "Alliance", "Horde", "Neutral", "Read-only",
	} {
		if !strings.Contains(tool.Description, kw) {
			t.Errorf("description missing %q", kw)
		}
	}
}

// TestAhUndercutters_ReadOnlyAnnotation pins readOnlyHint=true so a future
// copy-paste of a destructive sibling can't silently flip the gate.
func TestAhUndercutters_ReadOnlyAnnotation(t *testing.T) {
	reg := NewRegistry()
	RegisterAhUndercuttersTool(reg, DBDeps{})
	tool, _ := reg.Get("ah_undercutters")
	if !strings.Contains(string(tool.Annotations), `"readOnlyHint":true`) {
		t.Errorf("annotations missing readOnlyHint=true: %s", tool.Annotations)
	}
}

func TestAhUndercutters_NoPoolReturnsError(t *testing.T) {
	reg := NewRegistry()
	RegisterAhUndercuttersTool(reg, DBDeps{}) // QueryDB nil
	tool, _ := reg.Get("ah_undercutters")
	resp := tool.Handler(context.Background(), json.RawMessage(`{}`), "")
	got, _ := resp.(map[string]any)
	if msg, _ := got["error"].(string); !strings.Contains(msg, "ops_ro pool not configured") {
		t.Errorf("expected pool-not-configured error, got %v", got)
	}
}

// TestAhUndercutters_TopNClamps drives the handler and asserts the post-clamp topN
// echo (topN is a Go-side slice, so it never touches the SQL — the fold is empty).
func TestAhUndercutters_TopNClamps(t *testing.T) {
	cases := []struct {
		name     string
		args     string
		wantTopN int
	}{
		{"unset uses default", `{}`, ahUndercutDefaultTopN},
		{"zero falls back to default", `{"topN":0}`, ahUndercutDefaultTopN},
		{"negative falls back to default", `{"topN":-7}`, ahUndercutDefaultTopN},
		{"oversized clamps to max", `{"topN":99999}`, ahUndercutMaxTopN},
		{"in-range passes through", `{"topN":42}`, 42},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			expectEmptyUndercut(mock)

			reg := NewRegistry()
			RegisterAhUndercuttersTool(reg, DBDeps{QueryDB: db})
			tool, _ := reg.Get("ah_undercutters")
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

// TestAhUndercutters_MinFloorsClamp — minFloors defaults to 1 and clamps up to 1
// (a seller holding zero floors is not an undercutter); the echoed minFloors
// reflects the clamped value.
func TestAhUndercutters_MinFloorsClamp(t *testing.T) {
	cases := []struct {
		args          string
		wantMinFloors int
	}{
		{`{}`, ahUndercutDefaultMinFloors},
		{`{"minFloors":0}`, 1},
		{`{"minFloors":-5}`, 1},
		{`{"minFloors":1}`, 1},
		{`{"minFloors":4}`, 4},
	}
	for _, c := range cases {
		t.Run(c.args, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			expectEmptyUndercut(mock)

			reg := NewRegistry()
			RegisterAhUndercuttersTool(reg, DBDeps{QueryDB: db})
			tool, _ := reg.Get("ah_undercutters")
			resp := tool.Handler(context.Background(), json.RawMessage(c.args), "")
			got, _ := resp.(map[string]any)
			if got["error"] != nil {
				t.Fatalf("unexpected error: %v", got)
			}
			if m, _ := got["minFloors"].(int); m != c.wantMinFloors {
				t.Errorf("minFloors: %v want %d", got["minFloors"], c.wantMinFloors)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Errorf("unmet mock expectations: %v", err)
			}
		})
	}
}

// TestAhUndercutters_InvalidSortByRejected — an unknown sortBy is rejected before
// any query (a typo can't silently reorder the ranking). Case-sensitivity and an
// injection attempt are both covered; QueryDB is non-nil to prove the reject fires
// after the pool check but before any query.
func TestAhUndercutters_InvalidSortByRejected(t *testing.T) {
	for _, bad := range []string{`{"sortBy":"floors"}`, `{"sortBy":"FLOORHELD"}`, `{"sortBy":"listings; DROP TABLE auctionhouse"}`} {
		db, mock, err := sqlmock.New()
		if err != nil {
			t.Fatal(err)
		}
		reg := NewRegistry()
		RegisterAhUndercuttersTool(reg, DBDeps{QueryDB: db})
		tool, _ := reg.Get("ah_undercutters")
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

// TestAhUndercutters_InvalidHouseIdRejected — a houseId that isn't a real
// AuctionHouseId (2/6/7) is rejected (no query), not silently treated as an empty
// house.
func TestAhUndercutters_InvalidHouseIdRejected(t *testing.T) {
	for _, bad := range []string{`{"houseId":0}`, `{"houseId":3}`, `{"houseId":99}`} {
		db, mock, err := sqlmock.New()
		if err != nil {
			t.Fatal(err)
		}
		reg := NewRegistry()
		RegisterAhUndercuttersTool(reg, DBDeps{QueryDB: db})
		tool, _ := reg.Get("ah_undercutters")
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

// TestAhUndercutters_HouseIdFilter verifies the optional houseId binds a
// `AND ah.houseid = ?` on BOTH the floor query and the scan query (the scan also
// binds scanCap+1), and is echoed with its faction.
func TestAhUndercutters_HouseIdFilter(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.ExpectExec(regexp.QuoteMeta("USE `acore_characters`")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta(ahUndercutFloorQ)).
		WithArgs(toValues([]driverArg{i(7)})...).
		WillReturnRows(sqlmock.NewRows(ahUndercutFloorCols()))
	mock.ExpectQuery(regexp.QuoteMeta(ahUndercutScanQ)).
		WithArgs(toValues([]driverArg{i(7), i(ahUndercutScanCap + 1)})...).
		WillReturnRows(sqlmock.NewRows(ahUndercutScanCols()))

	reg := NewRegistry()
	RegisterAhUndercuttersTool(reg, DBDeps{QueryDB: db})
	tool, _ := reg.Get("ah_undercutters")
	resp := tool.Handler(context.Background(), json.RawMessage(`{"houseId":7}`), "")
	got, _ := resp.(map[string]any)
	if got["error"] != nil {
		t.Fatalf("unexpected error: %v", got)
	}
	if h, _ := got["houseId"].(int); h != 7 {
		t.Errorf("houseId echo: %v want 7", got["houseId"])
	}
	if f, _ := got["faction"].(string); f != "Neutral" {
		t.Errorf("faction echo: %v want Neutral", got["faction"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestAhUndercutters_HouseIdAbsentWhenUnset confirms the houseId/faction keys are
// omitted entirely when no filter is passed (absent != zero-value).
func TestAhUndercutters_HouseIdAbsentWhenUnset(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	expectEmptyUndercut(mock)

	out, err := collectAhUndercutters(context.Background(), db, time.Unix(1_700_000_000, 0), nil, 1, 15, "floorHeld")
	if err != nil {
		t.Fatalf("collectAhUndercutters: %v", err)
	}
	if _, ok := out["houseId"]; ok {
		t.Errorf("houseId should be absent when unset, got %v", out["houseId"])
	}
	if _, ok := out["faction"]; ok {
		t.Errorf("faction should be absent when unset, got %v", out["faction"])
	}
}

// TestCollectAhUndercutters_Empty — an empty market: sellers is a non-nil empty
// slice, zeroed totals, unresolved 0, and NO stage-2/3 queries fire.
func TestCollectAhUndercutters_Empty(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	expectEmptyUndercut(mock)

	out, err := collectAhUndercutters(context.Background(), db, time.Unix(1_700_000_000, 0), nil, 1, 15, "floorHeld")
	if err != nil {
		t.Fatalf("collectAhUndercutters: %v", err)
	}
	sl, ok := out["sellers"].([]*ahUndercutter)
	if !ok {
		t.Fatalf("sellers type: %T want []*ahUndercutter", out["sellers"])
	}
	if sl == nil || len(sl) != 0 {
		t.Errorf("sellers: %v want non-nil empty slice", sl)
	}
	if d, _ := out["displayedSellers"].(int); d != 0 {
		t.Errorf("displayedSellers: %v want 0", out["displayedSellers"])
	}
	if u, _ := out["unresolved"].(int); u != 0 {
		t.Errorf("unresolved: %v want 0", out["unresolved"])
	}
	totals, _ := out["totals"].(map[string]any)
	for _, k := range []string{"distinctPricedSellers", "distinctFloorHolders", "matchedFloorHolders", "totalFloorListings"} {
		if v, _ := totals[k].(int); v != 0 {
			t.Errorf("totals.%s: %v want 0", k, totals[k])
		}
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestCollectAhUndercutters_Golden drives the full composite: the per-item floor
// map, the per-owner fold (incl. a floor TIE across sellers — both count), the
// minFloors filter, floorHeld-desc + guid-asc ordering, the honest realm census
// independent of topN, the characters batch resolution (incl. a deleted-character
// miss -> unresolved), the account batch resolution + RNDBOT bot flag, the
// distinctItemsUndercut set size, and the floorHoldRatePct (incl. one-decimal
// rounding).
func TestCollectAhUndercutters_Golden(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	now := time.Unix(1_700_000_000, 0)

	mock.ExpectExec(regexp.QuoteMeta("USE `acore_characters`")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	// Floor map: item 100 -> 5000, 200 -> 8000, 300 -> 2000.
	mock.ExpectQuery(regexp.QuoteMeta(ahUndercutFloorQ)).
		WillReturnRows(sqlmock.NewRows(ahUndercutFloorCols()).
			AddRow(int64(100), int64(5000)).
			AddRow(int64(200), int64(8000)).
			AddRow(int64(300), int64(2000)))
	// Listings scan.
	mock.ExpectQuery(regexp.QuoteMeta(ahUndercutScanQ)).
		WithArgs(int64(ahUndercutScanCap + 1)).
		WillReturnRows(sqlmock.NewRows(ahUndercutScanCols()).
			// Seller 10 (AHBot): floors item100 + item200, plus one over-floor 200.
			AddRow(int64(10), int64(100), int64(5000)).
			AddRow(int64(10), int64(200), int64(8000)).
			AddRow(int64(10), int64(200), int64(15000)).
			// Seller 20 (player): floors item100 (TIE with 10) + item300.
			AddRow(int64(20), int64(100), int64(5000)).
			AddRow(int64(20), int64(300), int64(2000)).
			// Seller 30 (deleted char -> no characters row): floors item100 + item300.
			AddRow(int64(30), int64(100), int64(5000)).
			AddRow(int64(30), int64(300), int64(2000)).
			// Seller 40: floors item300 only -> below minFloors=2, filtered from
			// display but counted in the census.
			AddRow(int64(40), int64(300), int64(2000)))

	// Stage 2: characters lookup over the 3 matched sellers (guid-asc: 10,20,30).
	// 30 deliberately absent (deleted-char miss).
	mock.ExpectQuery(regexp.QuoteMeta("FROM `characters` WHERE guid IN (?,?,?)")).
		WithArgs(int64(10), int64(20), int64(30)).
		WillReturnRows(sqlmock.NewRows([]string{"guid", "name", "account"}).
			AddRow(int64(10), "Ahbotzug", int64(11)).
			AddRow(int64(20), "Slayo", int64(22)))

	// Stage 3: account lookup over the two resolved accounts (30 has account 0 ->
	// skipped). 11 is RNDBOT* (bot), 22 is a human.
	mock.ExpectExec(regexp.QuoteMeta("USE `acore_auth`")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta("FROM `account` WHERE id IN (?,?)")).
		WithArgs(int64(11), int64(22)).
		WillReturnRows(sqlmock.NewRows([]string{"id", "username"}).
			AddRow(int64(11), "RNDBOT0007").
			AddRow(int64(22), "OPERATOR"))

	// minFloors=2 drops seller 40 (1 floor); topN=10 keeps the rest.
	out, err := collectAhUndercutters(context.Background(), db, now, nil, 2, 10, "floorHeld")
	if err != nil {
		t.Fatalf("collectAhUndercutters: %v", err)
	}

	if s, _ := out["scanned"].(int); s != 8 {
		t.Errorf("scanned: %v want 8", out["scanned"])
	}
	if d, _ := out["displayedSellers"].(int); d != 3 {
		t.Errorf("displayedSellers: %v want 3", out["displayedSellers"])
	}
	if u, _ := out["unresolved"].(int); u != 1 {
		t.Errorf("unresolved: %v want 1 (seller 30 deleted char)", out["unresolved"])
	}

	totals, _ := out["totals"].(map[string]any)
	if v, _ := totals["distinctPricedSellers"].(int); v != 4 {
		t.Errorf("totals.distinctPricedSellers: %v want 4", totals["distinctPricedSellers"])
	}
	if v, _ := totals["distinctFloorHolders"].(int); v != 4 {
		t.Errorf("totals.distinctFloorHolders: %v want 4 (10,20,30,40 all hold >=1 floor)", totals["distinctFloorHolders"])
	}
	if v, _ := totals["matchedFloorHolders"].(int); v != 3 {
		t.Errorf("totals.matchedFloorHolders: %v want 3 (40 has 1 floor < minFloors=2)", totals["matchedFloorHolders"])
	}
	if v, _ := totals["totalFloorListings"].(int); v != 7 {
		t.Errorf("totals.totalFloorListings: %v want 7 (2+2+2+1)", totals["totalFloorListings"])
	}

	sellers, _ := out["sellers"].([]*ahUndercutter)
	if len(sellers) != 3 {
		t.Fatalf("sellers len: %d want 3", len(sellers))
	}

	// Order: all three tie at floorHeld=2 -> guid asc: 10, 20, 30.
	s0 := sellers[0]
	if s0.ItemOwnerGuid != 10 || s0.FloorHeld != 2 || s0.Listings != 3 || s0.DistinctItemsUndercut != 2 {
		t.Errorf("sellers[0]: %+v want guid=10 floorHeld=2 listings=3 distinctItems=2", s0)
	}
	if s0.FloorHoldRatePct != 66.7 {
		t.Errorf("sellers[0] floorHoldRatePct: %v want 66.7 (2/3, one-decimal rounding)", s0.FloorHoldRatePct)
	}
	if s0.CharacterName != "Ahbotzug" || s0.AccountID != 11 || !s0.IsBot {
		t.Errorf("sellers[0] identity: name=%q acct=%d isBot=%v want Ahbotzug/11/true", s0.CharacterName, s0.AccountID, s0.IsBot)
	}

	s1 := sellers[1]
	if s1.ItemOwnerGuid != 20 || s1.FloorHeld != 2 || s1.Listings != 2 || s1.DistinctItemsUndercut != 2 {
		t.Errorf("sellers[1]: %+v want guid=20 floorHeld=2 listings=2 distinctItems=2", s1)
	}
	if s1.FloorHoldRatePct != 100.0 {
		t.Errorf("sellers[1] floorHoldRatePct: %v want 100.0 (2/2)", s1.FloorHoldRatePct)
	}
	if s1.CharacterName != "Slayo" || s1.AccountID != 22 || s1.IsBot {
		t.Errorf("sellers[1] identity: name=%q acct=%d isBot=%v want Slayo/22/false", s1.CharacterName, s1.AccountID, s1.IsBot)
	}

	// Seller 30: deleted character -> empty name, account 0, not a bot; still a
	// floor-holder with 2 floors on 2 distinct items.
	s2 := sellers[2]
	if s2.ItemOwnerGuid != 30 || s2.CharacterName != "" || s2.AccountID != 0 || s2.IsBot {
		t.Errorf("sellers[2] (deleted char): %+v want guid=30 empty-name acct0 not-bot", s2)
	}
	if s2.FloorHeld != 2 || s2.DistinctItemsUndercut != 2 {
		t.Errorf("sellers[2] floors: floorHeld=%d distinctItems=%d want 2/2", s2.FloorHeld, s2.DistinctItemsUndercut)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// mkUndercutter builds a folded seller with a floorItems set of the requested size
// (its len becomes DistinctItemsUndercut at slice time).
func mkUndercutter(guid int64, floorHeld, listings, distinctItems int) *ahUndercutter {
	fi := make(map[int64]struct{}, distinctItems)
	for k := 0; k < distinctItems; k++ {
		fi[int64(k)] = struct{}{}
	}
	return &ahUndercutter{ItemOwnerGuid: guid, FloorHeld: floorHeld, Listings: listings, floorItems: fi}
}

// TestTopNAhUndercutters_SortKeys — each sortBy key ranks a DIFFERENT seller top,
// proving the comparator reads the right field. floorHeld -> A (5), listings -> B
// (20), distinctItems -> C (4 distinct).
func TestTopNAhUndercutters_SortKeys(t *testing.T) {
	build := func() map[int64]*ahUndercutter {
		return map[int64]*ahUndercutter{
			10: mkUndercutter(10, 5, 6, 2),  // most floors
			20: mkUndercutter(20, 4, 20, 1), // most listings
			30: mkUndercutter(30, 3, 4, 4),  // most distinct items
		}
	}
	cases := []struct {
		sortKey string
		wantTop int64
	}{
		{"floorHeld", 10},
		{"listings", 20},
		{"distinctItems", 30},
	}
	for _, c := range cases {
		t.Run(c.sortKey, func(t *testing.T) {
			top, _, _, _ := topNAhUndercutters(build(), 1, 10, c.sortKey)
			if len(top) != 3 {
				t.Fatalf("len(top)=%d want 3", len(top))
			}
			if top[0].ItemOwnerGuid != c.wantTop {
				t.Errorf("top[0]=%d want %d", top[0].ItemOwnerGuid, c.wantTop)
			}
		})
	}
}

// TestTopNAhUndercutters_FilterCensusTiebreak exercises the pure helper: the
// minFloors filter, the floorHeld-desc + guid-asc tiebreak, the slice to n, and
// the pre-slice census (distinctFloorHolders counts ALL >=1-floor owners,
// matchedFloorHolders only those passing minFloors, totalFloorListings sums every
// owner's floorHeld — all independent of topN). Guards against Go map-iteration
// flake on ties.
func TestTopNAhUndercutters_FilterCensusTiebreak(t *testing.T) {
	m := map[int64]*ahUndercutter{
		30: mkUndercutter(30, 3, 3, 1),
		10: mkUndercutter(10, 3, 3, 1), // tied with 30 -> 10 sorts first (guid asc)
		20: mkUndercutter(20, 9, 9, 1), // highest -> first overall
		40: mkUndercutter(40, 1, 1, 1), // 1 floor < minFloors=2 -> filtered from display, counted in census
	}
	top, dfh, mfh, tfl := topNAhUndercutters(m, 2, 2, "floorHeld")
	if dfh != 4 {
		t.Errorf("distinctFloorHolders: %d want 4 (all hold >=1 floor)", dfh)
	}
	if mfh != 3 {
		t.Errorf("matchedFloorHolders: %d want 3 (40 filtered by minFloors=2)", mfh)
	}
	if tfl != 16 {
		t.Errorf("totalFloorListings: %d want 16 (3+3+9+1)", tfl)
	}
	if len(top) != 2 {
		t.Fatalf("len(top): %d want 2 (sliced to n)", len(top))
	}
	if top[0].ItemOwnerGuid != 20 {
		t.Errorf("top[0]: %+v want guid=20 (9 floors)", top[0])
	}
	if top[1].ItemOwnerGuid != 10 {
		t.Errorf("top[1]: %+v want guid=10 (3 floors, guid-asc tiebreak over 30)", top[1])
	}
}

// TestTopNAhUndercutters_EmptyNonNil — an all-filtered map still returns a non-nil
// slice (callers JSON-encode it as []).
func TestTopNAhUndercutters_EmptyNonNil(t *testing.T) {
	m := map[int64]*ahUndercutter{1: mkUndercutter(1, 1, 1, 1)}
	top, dfh, mfh, tfl := topNAhUndercutters(m, 5, 10, "floorHeld")
	if top == nil {
		t.Fatal("top is nil, want non-nil empty slice")
	}
	if len(top) != 0 {
		t.Errorf("len(top): %d want 0", len(top))
	}
	// The lone owner still holds 1 floor -> counted in the census even though it's
	// below minFloors=5.
	if dfh != 1 || mfh != 0 || tfl != 1 {
		t.Errorf("census: dfh=%d mfh=%d tfl=%d want 1/0/1", dfh, mfh, tfl)
	}
}

// TestAhUndercutterFold_FloorMatch — foldListing counts a listing as floor-held
// only when its buyout equals the item's floor; an item missing from the floor map
// (a defensive belt-and-suspenders case) never counts.
func TestAhUndercutterFold_FloorMatch(t *testing.T) {
	floorMap := map[int64]int64{100: 5000, 200: 8000}
	u := &ahUndercutter{}
	u.foldListing(100, 5000, floorMap) // floor match item100
	u.foldListing(100, 9000, floorMap) // above floor -> listing only
	u.foldListing(200, 8000, floorMap) // floor match item200
	u.foldListing(300, 1000, floorMap) // item300 not in map -> listing only
	if u.Listings != 4 {
		t.Errorf("listings: %d want 4", u.Listings)
	}
	if u.FloorHeld != 2 {
		t.Errorf("floorHeld: %d want 2", u.FloorHeld)
	}
	if len(u.floorItems) != 2 {
		t.Errorf("distinct floor items: %d want 2 (100,200)", len(u.floorItems))
	}
}

// TestAnnotateUndercutterAccounts_DedupesAccountIds — two seller-characters on the
// same account bind the id ONCE in the IN list, and both get the username + bot
// flag. Sellers with account 0 (unresolved character) are skipped entirely.
func TestAnnotateUndercutterAccounts_DedupesAccountIds(t *testing.T) {
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

	sellers := []*ahUndercutter{
		{ItemOwnerGuid: 100, AccountID: 11},
		{ItemOwnerGuid: 101, AccountID: 11}, // same account -> deduped in IN list
		{ItemOwnerGuid: 102, AccountID: 0},  // unresolved char -> skipped
	}
	mock.ExpectQuery(regexp.QuoteMeta("FROM `account` WHERE id IN (?)")).
		WithArgs(int64(11)).
		WillReturnRows(sqlmock.NewRows([]string{"id", "username"}).
			AddRow(int64(11), "RNDBOTAAA"))

	if err := annotateUndercutterAccounts(context.Background(), conn, sellers); err != nil {
		t.Fatalf("annotateUndercutterAccounts: %v", err)
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
