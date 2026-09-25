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

// ahContestCensusCols is the single-row census column set (honest realm totals).
func ahContestCensusCols() []string {
	return []string{"distinctItems", "totalListings", "totalLeadingAuctions"}
}

// ahContestDisplayCols is the per-item leaderboard column set, in scan order.
func ahContestDisplayCols() []string {
	return []string{"entry", "name", "Quality", "class", "listings", "leadingAuctions", "distinctBidders"}
}

// ahContestCensusRow builds a one-row census result (aggregate-with-no-GROUP-BY
// always returns exactly one row on real MySQL).
func ahContestCensusRow(distinctItems, totalListings, totalLeading int) *sqlmock.Rows {
	return sqlmock.NewRows(ahContestCensusCols()).AddRow(distinctItems, totalListings, totalLeading)
}

// censusQ / displayQ are the query-distinctive fragments used to pin each of the
// two ordered ExpectQuery calls to the right query.
const (
	ahContestCensusQ  = "COUNT(DISTINCT it.entry) AS distinctItems"
	ahContestDisplayQ = "HAVING listings >= ?"
)

// TestAhItemContestRatio_DescriptionMentionsContext guards the vocabulary that
// steers the agent here vs ah_market_top_items (pure supply) or ah_item_demand
// (pure demand) — the ratio / supply-vs-demand framing is load-bearing for
// selection.
func TestAhItemContestRatio_DescriptionMentionsContext(t *testing.T) {
	reg := NewRegistry()
	RegisterAhItemContestRatioTool(reg, DBDeps{})
	tool, ok := reg.Get("ah_item_contest_ratio")
	if !ok {
		t.Fatal("ah_item_contest_ratio not registered")
	}
	for _, kw := range []string{
		"acore_characters.auctionhouse", "item_instance", "acore_world.item_template", "ops_ro",
		"ah_market_top_items", "ah_item_demand", "contestRatioPct", "leadingAuctions", "listings",
		"distinctBidders", "supply", "demand", "contested", "qualityName", "className",
		"topN", "sortBy", "minItems", "minQuality", "houseId", "Alliance", "Horde", "Neutral", "Read-only",
	} {
		if !strings.Contains(tool.Description, kw) {
			t.Errorf("description missing %q: %s", kw, tool.Description)
		}
	}
}

// TestAhItemContestRatio_ReadOnlyAnnotation pins readOnlyHint=true so a future
// copy-paste of a destructive sibling can't silently flip the gate.
func TestAhItemContestRatio_ReadOnlyAnnotation(t *testing.T) {
	reg := NewRegistry()
	RegisterAhItemContestRatioTool(reg, DBDeps{})
	tool, _ := reg.Get("ah_item_contest_ratio")
	if !strings.Contains(string(tool.Annotations), `"readOnlyHint":true`) {
		t.Errorf("annotations missing readOnlyHint=true: %s", tool.Annotations)
	}
}

func TestAhItemContestRatio_NoPoolReturnsError(t *testing.T) {
	reg := NewRegistry()
	RegisterAhItemContestRatioTool(reg, DBDeps{}) // QueryDB nil
	tool, _ := reg.Get("ah_item_contest_ratio")
	resp := tool.Handler(context.Background(), json.RawMessage(`{}`), "")
	got, _ := resp.(map[string]any)
	if msg, _ := got["error"].(string); !strings.Contains(msg, "ops_ro pool not configured") {
		t.Errorf("expected pool-not-configured error, got %v", got)
	}
}

// TestAhItemContestRatio_TopNClamps drives the handler and asserts the post-clamp
// topN echo and that the display LIMIT bind matches the clamped value. Both the
// census and display queries are mocked (the tool runs two queries on one conn).
func TestAhItemContestRatio_TopNClamps(t *testing.T) {
	cases := []struct {
		name     string
		args     string
		wantTopN int
	}{
		{"unset uses default", `{}`, ahContestDefaultTop},
		{"zero falls back to default", `{"topN":0}`, ahContestDefaultTop},
		{"negative falls back to default", `{"topN":-7}`, ahContestDefaultTop},
		{"oversized clamps to max", `{"topN":99999}`, ahContestMaxTop},
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
			mock.ExpectQuery(regexp.QuoteMeta(ahContestCensusQ)).
				WillReturnRows(ahContestCensusRow(0, 0, 0))
			mock.ExpectQuery(regexp.QuoteMeta(ahContestDisplayQ)).
				WithArgs(int64(ahContestDefaultMinItems), int64(c.wantTopN)).
				WillReturnRows(sqlmock.NewRows(ahContestDisplayCols()))

			reg := NewRegistry()
			RegisterAhItemContestRatioTool(reg, DBDeps{QueryDB: db})
			tool, _ := reg.Get("ah_item_contest_ratio")
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

// TestAhItemContestRatio_MinItemsClamp — minItems defaults to 2 and clamps up to
// 1 (a floor below one listing is meaningless); the HAVING bind and the echoed
// minItems both reflect the clamped value.
func TestAhItemContestRatio_MinItemsClamp(t *testing.T) {
	cases := []struct {
		name         string
		args         string
		wantMinItems int
	}{
		{"unset uses default 2", `{}`, ahContestDefaultMinItems},
		{"zero clamps to 1", `{"minItems":0}`, 1},
		{"negative clamps to 1", `{"minItems":-4}`, 1},
		{"one passes through", `{"minItems":1}`, 1},
		{"in-range passes through", `{"minItems":5}`, 5},
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
			mock.ExpectQuery(regexp.QuoteMeta(ahContestCensusQ)).
				WillReturnRows(ahContestCensusRow(0, 0, 0))
			mock.ExpectQuery(regexp.QuoteMeta(ahContestDisplayQ)).
				WithArgs(int64(c.wantMinItems), int64(ahContestDefaultTop)).
				WillReturnRows(sqlmock.NewRows(ahContestDisplayCols()))

			reg := NewRegistry()
			RegisterAhItemContestRatioTool(reg, DBDeps{QueryDB: db})
			tool, _ := reg.Get("ah_item_contest_ratio")
			resp := tool.Handler(context.Background(), json.RawMessage(c.args), "")
			got, _ := resp.(map[string]any)
			if got["error"] != nil {
				t.Fatalf("unexpected error: %v", got)
			}
			if n, _ := got["minItems"].(int); n != c.wantMinItems {
				t.Errorf("minItems: %v want %d", got["minItems"], c.wantMinItems)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Errorf("unmet mock expectations: %v", err)
			}
		})
	}
}

// TestAhItemContestRatio_InvalidSortByRejected — an unknown sortBy is rejected
// before any query (a typo can't silently reorder the market). Case-sensitivity
// and an injection attempt are both covered. QueryDB is non-nil to prove the
// reject fires after the pool check but before any query.
func TestAhItemContestRatio_InvalidSortByRejected(t *testing.T) {
	for _, bad := range []string{`{"sortBy":"ratio"}`, `{"sortBy":"CONTESTED"}`, `{"sortBy":"listings; DROP TABLE auctionhouse"}`} {
		db, mock, err := sqlmock.New()
		if err != nil {
			t.Fatal(err)
		}
		reg := NewRegistry()
		RegisterAhItemContestRatioTool(reg, DBDeps{QueryDB: db})
		tool, _ := reg.Get("ah_item_contest_ratio")
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

// TestAhItemContestRatio_MinQualityRejected — out-of-range minQuality is rejected
// (no query) so a typo can't masquerade as an empty market.
func TestAhItemContestRatio_MinQualityRejected(t *testing.T) {
	for _, bad := range []string{`{"minQuality":-1}`, `{"minQuality":8}`, `{"minQuality":99}`} {
		db, mock, err := sqlmock.New()
		if err != nil {
			t.Fatal(err)
		}
		reg := NewRegistry()
		RegisterAhItemContestRatioTool(reg, DBDeps{QueryDB: db})
		tool, _ := reg.Get("ah_item_contest_ratio")
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

// TestAhItemContestRatio_InvalidHouseIdRejected — a houseId that isn't a real
// AuctionHouseId (2/6/7) is rejected (no query), not silently treated as an empty
// house.
func TestAhItemContestRatio_InvalidHouseIdRejected(t *testing.T) {
	for _, bad := range []string{`{"houseId":0}`, `{"houseId":3}`, `{"houseId":99}`} {
		db, mock, err := sqlmock.New()
		if err != nil {
			t.Fatal(err)
		}
		reg := NewRegistry()
		RegisterAhItemContestRatioTool(reg, DBDeps{QueryDB: db})
		tool, _ := reg.Get("ah_item_contest_ratio")
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

// TestAhItemContestRatio_Filters drives each filter combination and asserts the
// display WHERE-clause shape, the bound args on BOTH the census (filter values)
// and display (filter values + minItems + topN) queries, and the echoed
// houseId/faction/minQuality keys.
func TestAhItemContestRatio_Filters(t *testing.T) {
	cases := []struct {
		name         string
		args         string
		displayWhere string
		censusArgs   []driverArg
		displayArgs  []driverArg
		wantHouse    int // -1 = key absent
		wantFaction  string
		wantMinQ     int // -1 = key absent
	}{
		{
			name:         "house only",
			args:         `{"houseId":7}`,
			displayWhere: "WHERE ah.houseid = ? GROUP BY it.entry",
			censusArgs:   []driverArg{i(7)},
			displayArgs:  []driverArg{i(7), i(ahContestDefaultMinItems), i(ahContestDefaultTop)},
			wantHouse:    7, wantFaction: "Neutral", wantMinQ: -1,
		},
		{
			name:         "quality only",
			args:         `{"minQuality":3}`,
			displayWhere: "WHERE it.Quality >= ? GROUP BY it.entry",
			censusArgs:   []driverArg{i(3)},
			displayArgs:  []driverArg{i(3), i(ahContestDefaultMinItems), i(ahContestDefaultTop)},
			wantHouse:    -1, wantMinQ: 3,
		},
		{
			name:         "both filters + topN + minItems",
			args:         `{"houseId":2,"minQuality":4,"topN":5,"minItems":3}`,
			displayWhere: "WHERE ah.houseid = ? AND it.Quality >= ? GROUP BY it.entry",
			censusArgs:   []driverArg{i(2), i(4)},
			displayArgs:  []driverArg{i(2), i(4), i(3), i(5)},
			wantHouse:    2, wantFaction: "Alliance", wantMinQ: 4,
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
			mock.ExpectQuery(regexp.QuoteMeta(ahContestCensusQ)).
				WithArgs(toValues(c.censusArgs)...).
				WillReturnRows(ahContestCensusRow(0, 0, 0))
			mock.ExpectQuery(regexp.QuoteMeta(c.displayWhere)).
				WithArgs(toValues(c.displayArgs)...).
				WillReturnRows(sqlmock.NewRows(ahContestDisplayCols()))

			reg := NewRegistry()
			RegisterAhItemContestRatioTool(reg, DBDeps{QueryDB: db})
			tool, _ := reg.Get("ah_item_contest_ratio")
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

// TestAhItemContestRatio_SortByClause — each sortBy key produces its distinct
// ORDER BY clause in the display query. The ratio ("contested") orders by the
// aggregate expression directly; demand/supply order by the SELECT aliases.
func TestAhItemContestRatio_SortByClause(t *testing.T) {
	cases := []struct {
		sortBy     string
		orderBySub string
	}{
		{"contested", "ORDER BY (SUM(CASE WHEN ah.buyguid <> 0 THEN 1 ELSE 0 END) * 100.0 / COUNT(*)) DESC"},
		{"demand", "ORDER BY leadingAuctions DESC"},
		{"supply", "ORDER BY listings DESC"},
	}
	for _, c := range cases {
		t.Run(c.sortBy, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			mock.ExpectExec(regexp.QuoteMeta("USE `acore_characters`")).
				WillReturnResult(sqlmock.NewResult(0, 0))
			mock.ExpectQuery(regexp.QuoteMeta(ahContestCensusQ)).
				WillReturnRows(ahContestCensusRow(0, 0, 0))
			mock.ExpectQuery(regexp.QuoteMeta(c.orderBySub)).
				WithArgs(int64(ahContestDefaultMinItems), int64(ahContestDefaultTop)).
				WillReturnRows(sqlmock.NewRows(ahContestDisplayCols()))

			out, err := collectAhItemContestRatio(context.Background(), db, time.Unix(1_700_000_000, 0),
				ahContestDefaultTop, ahContestDefaultMinItems, c.sortBy, nil, nil)
			if err != nil {
				t.Fatalf("collectAhItemContestRatio: %v", err)
			}
			if sb, _ := out["sortBy"].(string); sb != c.sortBy {
				t.Errorf("sortBy echo: %v want %q", out["sortBy"], c.sortBy)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Errorf("unmet mock expectations: %v", err)
			}
		})
	}
}

// TestCollectAhItemContestRatio_Golden folds three grouped rows plus a separate
// census, verifying the per-item contestRatioPct (incl. one-decimal rounding),
// the quality/class enum maps, the honest realm totals + marketContestRatioPct
// (independent of the displayed rows), and that the handler trusts the SQL ORDER
// BY (no Go-side re-sort).
func TestCollectAhItemContestRatio_Golden(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	now := time.Unix(1_700_000_000, 0)
	mock.ExpectExec(regexp.QuoteMeta("USE `acore_characters`")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	// Census: realm-wide totals independent of the 3 displayed rows.
	mock.ExpectQuery(regexp.QuoteMeta(ahContestCensusQ)).
		WillReturnRows(ahContestCensusRow(4, 350, 28)) // market ratio 28/350 = 8.0%
	// Display, already in contested-desc order as the SQL ORDER BY returns it.
	mock.ExpectQuery(regexp.QuoteMeta(ahContestDisplayQ)).
		WithArgs(int64(ahContestDefaultMinItems), int64(15)).
		WillReturnRows(sqlmock.NewRows(ahContestDisplayCols()).
			// squeeze: 3 listings all under bid -> 100.0
			AddRow(int64(49908), "Primordial Saronite", 4, 7, 3, 3, 2).
			// mid: 10/30 -> 33.3 (one-decimal rounding)
			AddRow(int64(4306), "Silk Cloth", 1, 7, 30, 10, 6).
			// glut: 5/100 -> 5.0
			AddRow(int64(22445), "Arcane Dust", 2, 3, 100, 5, 3))

	out, err := collectAhItemContestRatio(context.Background(), db, now, 15, ahContestDefaultMinItems, "contested", nil, nil)
	if err != nil {
		t.Fatalf("collectAhItemContestRatio: %v", err)
	}
	if di, _ := out["displayedItems"].(int); di != 3 {
		t.Errorf("displayedItems: %v want 3", out["displayedItems"])
	}
	if _, ok := out["houseId"]; ok {
		t.Errorf("houseId should be absent with no filter, got %v", out["houseId"])
	}

	totals, _ := out["totals"].(map[string]any)
	if totals == nil {
		t.Fatalf("totals missing: %v", out)
	}
	if v, _ := totals["distinctItems"].(int); v != 4 {
		t.Errorf("totals.distinctItems: %v want 4", totals["distinctItems"])
	}
	if v, _ := totals["totalListings"].(int); v != 350 {
		t.Errorf("totals.totalListings: %v want 350", totals["totalListings"])
	}
	if v, _ := totals["totalLeadingAuctions"].(int); v != 28 {
		t.Errorf("totals.totalLeadingAuctions: %v want 28", totals["totalLeadingAuctions"])
	}
	if v, _ := totals["marketContestRatioPct"].(float64); v != 8.0 {
		t.Errorf("totals.marketContestRatioPct: %v want 8.0", totals["marketContestRatioPct"])
	}

	items, ok := out["items"].([]*ahContestItem)
	if !ok || len(items) != 3 {
		t.Fatalf("items: %T len %d want []*ahContestItem len 3", out["items"], len(items))
	}

	it0 := items[0]
	if it0.ItemEntry != 49908 || it0.ItemName != "Primordial Saronite" || it0.Quality != 4 || it0.QualityName != "Epic" {
		t.Errorf("items[0] identity: %+v", it0)
	}
	if it0.Class != 7 || it0.ClassName != "Trade Goods" {
		t.Errorf("items[0] class: %+v", it0)
	}
	if it0.Listings != 3 || it0.LeadingAuctions != 3 || it0.DistinctBidders != 2 || it0.ContestRatioPct != 100.0 {
		t.Errorf("items[0] contest: %+v", it0)
	}

	it1 := items[1]
	if it1.QualityName != "Common" || it1.ClassName != "Trade Goods" || it1.ContestRatioPct != 33.3 {
		t.Errorf("items[1] contest (want 33.3): %+v", it1)
	}

	it2 := items[2]
	if it2.QualityName != "Uncommon" || it2.ClassName != "Gem" || it2.ContestRatioPct != 5.0 {
		t.Errorf("items[2] contest (want 5.0): %+v", it2)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestCollectAhItemContestRatio_Empty — an empty market yields a non-nil empty
// items slice, displayedItems 0, zeroed totals, and a div-guarded
// marketContestRatioPct of 0 (not NaN).
func TestCollectAhItemContestRatio_Empty(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.ExpectExec(regexp.QuoteMeta("USE `acore_characters`")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta(ahContestCensusQ)).
		WillReturnRows(ahContestCensusRow(0, 0, 0))
	mock.ExpectQuery(regexp.QuoteMeta(ahContestDisplayQ)).
		WithArgs(int64(ahContestDefaultMinItems), int64(15)).
		WillReturnRows(sqlmock.NewRows(ahContestDisplayCols()))

	out, err := collectAhItemContestRatio(context.Background(), db, time.Unix(1_700_000_000, 0), 15, ahContestDefaultMinItems, "contested", nil, nil)
	if err != nil {
		t.Fatalf("collectAhItemContestRatio: %v", err)
	}
	items, ok := out["items"].([]*ahContestItem)
	if !ok {
		t.Fatalf("items type: %T want []*ahContestItem", out["items"])
	}
	if items == nil || len(items) != 0 {
		t.Errorf("items: %v want non-nil empty slice", items)
	}
	if di, _ := out["displayedItems"].(int); di != 0 {
		t.Errorf("displayedItems: %v want 0", out["displayedItems"])
	}
	totals, _ := out["totals"].(map[string]any)
	if v, _ := totals["marketContestRatioPct"].(float64); v != 0 {
		t.Errorf("totals.marketContestRatioPct: %v want 0 (div-guard)", totals["marketContestRatioPct"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}
