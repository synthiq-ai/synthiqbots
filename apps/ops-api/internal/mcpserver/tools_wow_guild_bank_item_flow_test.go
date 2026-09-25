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

// guildBankItemFlowCols is the column set wow_guild_bank_item_flow scans, in order.
func guildBankItemFlowCols() []string {
	return []string{"entry", "name", "Quality", "class", "deposits", "withdrawals", "moves", "totalEvents", "distinctActors", "quantityMoved"}
}

// guildBankItemFlowFixture returns three grouped rows in entry-asc order (the SQL
// FETCH order) chosen so the three sortBy keys each produce a DISTINCT ranking:
//
//	quantity: B(300), A(50),  C(6)   -> B, A, C
//	events:   A(9),   B(8),   C(6)   -> A, B, C
//	actors:   C(6),   A(4),   B(2)   -> C, A, B
//
// which proves the Go-side comparator actually reads the selected aggregate.
func guildBankItemFlowFixture() *sqlmock.Rows {
	return sqlmock.NewRows(guildBankItemFlowCols()).
		// A: Netherweave Cloth — Common (1) Trade Goods (7); 5 dep / 3 wd / 1 move.
		AddRow(int64(21877), "Netherweave Cloth", 1, 7, 5, 3, 1, 9, 4, int64(50)).
		// B: Badge of Justice — Epic (4) Quest (12); 2 dep / 6 wd / 0 move.
		AddRow(int64(29434), "Badge of Justice", 4, 12, 2, 6, 0, 8, 2, int64(300)).
		// C: Emblem of Heroism — Rare (3) Miscellaneous (15); 1 dep / 1 wd / 4 move.
		AddRow(int64(40752), "Emblem of Heroism", 3, 15, 1, 1, 4, 6, 6, int64(6))
}

// TestWowGuildBankItemFlow_DescriptionMentionsContext guards the keywords that
// steer the agent here vs wow_guild_bank_summary (money census) or a hand-written
// db_query — the eventlog item-flow framing + the EventType scoping vocabulary is
// load-bearing for tool selection.
func TestWowGuildBankItemFlow_DescriptionMentionsContext(t *testing.T) {
	reg := NewRegistry()
	RegisterWowGuildBankItemFlowTool(reg, DBDeps{})
	tool, ok := reg.Get("wow_guild_bank_item_flow")
	if !ok {
		t.Fatal("wow_guild_bank_item_flow not registered")
	}
	for _, kw := range []string{
		"acore_characters.guild_bank_eventlog", "acore_world.item_template", "EventType", "ItemStackCount",
		"wow_guild_bank_summary", "wow_mail_item_breakdown", "ah_market_top_items", "ops_ro",
		"qualityName", "className", "Heirloom", "deposits", "withdrawals", "quantityMoved",
		"totalEvents", "totalItemEvents", "distinctActors", "topN", "sortBy", "minQuality",
		"guildId", "sinceHours", "Read-only",
	} {
		if !strings.Contains(tool.Description, kw) {
			t.Errorf("description missing %q: %s", kw, tool.Description)
		}
	}
}

// TestWowGuildBankItemFlow_ReadOnlyAnnotation pins readOnlyHint=true so a future
// copy-paste of a destructive sibling can't silently flip the gate.
func TestWowGuildBankItemFlow_ReadOnlyAnnotation(t *testing.T) {
	reg := NewRegistry()
	RegisterWowGuildBankItemFlowTool(reg, DBDeps{})
	tool, _ := reg.Get("wow_guild_bank_item_flow")
	if !strings.Contains(string(tool.Annotations), `"readOnlyHint":true`) {
		t.Errorf("annotations missing readOnlyHint=true: %s", tool.Annotations)
	}
}

func TestWowGuildBankItemFlow_NoPoolReturnsError(t *testing.T) {
	reg := NewRegistry()
	RegisterWowGuildBankItemFlowTool(reg, DBDeps{}) // QueryDB nil
	tool, _ := reg.Get("wow_guild_bank_item_flow")
	resp := tool.Handler(context.Background(), json.RawMessage(`{}`), "")
	got, _ := resp.(map[string]any)
	if msg, _ := got["error"].(string); !strings.Contains(msg, "ops_ro pool not configured") {
		t.Errorf("expected pool-not-configured error, got %v", got)
	}
}

// TestWowGuildBankItemFlow_TopNClamps asserts the post-clamp topN echo. There is
// no LIMIT bind (rows are folded whole then sliced Go-side), so the query is the
// unfiltered shape and only the echo is checked.
func TestWowGuildBankItemFlow_TopNClamps(t *testing.T) {
	cases := []struct {
		name     string
		args     string
		wantTopN int
	}{
		{"unset uses default", `{}`, wowGuildBankItemFlowDefaultTop},
		{"zero falls back to default", `{"topN":0}`, wowGuildBankItemFlowDefaultTop},
		{"negative falls back to default", `{"topN":-7}`, wowGuildBankItemFlowDefaultTop},
		{"oversized clamps to max", `{"topN":99999}`, wowGuildBankItemFlowMaxTop},
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
			mock.ExpectQuery(regexp.QuoteMeta("ORDER BY it.entry ASC")).
				WillReturnRows(sqlmock.NewRows(guildBankItemFlowCols()))

			reg := NewRegistry()
			RegisterWowGuildBankItemFlowTool(reg, DBDeps{QueryDB: db})
			tool, _ := reg.Get("wow_guild_bank_item_flow")
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

// TestWowGuildBankItemFlow_InvalidSortByRejected — an unknown sortBy is rejected
// (no DB call). Includes the wrong-case "QUANTITY" (keys are case-sensitive) and
// "moves" (the item-move BUCKET is a field, NOT a sort key) so a caller expecting
// the backlog's original naming gets a clear error rather than a silent default.
func TestWowGuildBankItemFlow_InvalidSortByRejected(t *testing.T) {
	for _, bad := range []string{`{"sortBy":"bogus"}`, `{"sortBy":"QUANTITY"}`, `{"sortBy":"moves"}`} {
		db, mock, err := sqlmock.New()
		if err != nil {
			t.Fatal(err)
		}
		reg := NewRegistry()
		RegisterWowGuildBankItemFlowTool(reg, DBDeps{QueryDB: db})
		tool, _ := reg.Get("wow_guild_bank_item_flow")
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

// TestWowGuildBankItemFlow_MinQualityRejected — out-of-range minQuality is rejected
// (no DB call) so a typo can't masquerade as an empty bank. QueryDB is non-nil to
// prove the reject fires AFTER the pool check but before any query.
func TestWowGuildBankItemFlow_MinQualityRejected(t *testing.T) {
	for _, bad := range []string{`{"minQuality":-1}`, `{"minQuality":8}`, `{"minQuality":99}`} {
		db, mock, err := sqlmock.New()
		if err != nil {
			t.Fatal(err)
		}
		reg := NewRegistry()
		RegisterWowGuildBankItemFlowTool(reg, DBDeps{QueryDB: db})
		tool, _ := reg.Get("wow_guild_bank_item_flow")
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

// TestWowGuildBankItemFlow_GuildIdRejectsBadId — a non-numeric / negative guildId
// is rejected BEFORE any query is issued (no SQL round trip on a bad filter).
func TestWowGuildBankItemFlow_GuildIdRejectsBadId(t *testing.T) {
	for _, args := range []string{`{"guildId":"abc"}`, `{"guildId":-5}`} {
		db, mock, err := sqlmock.New()
		if err != nil {
			t.Fatal(err)
		}
		// No ExpectExec / ExpectQuery — the handler must bail before touching the DB.
		reg := NewRegistry()
		RegisterWowGuildBankItemFlowTool(reg, DBDeps{QueryDB: db})
		tool, _ := reg.Get("wow_guild_bank_item_flow")
		resp := tool.Handler(context.Background(), json.RawMessage(args), "")
		got, _ := resp.(map[string]any)
		if msg, _ := got["error"].(string); !strings.Contains(msg, "guildId must be a positive integer") {
			t.Errorf("args %s: expected guildId reject, got %v", args, got)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("args %s: a query was issued on a bad guildId: %v", args, err)
		}
		db.Close()
	}
}

// TestWowGuildBankItemFlow_Filters drives the handler with each filter combination
// and asserts the WHERE clause shape (filters AND-appended after the base EventType
// IN guard), the bound args (guildId, then the sinceHours cutoff, then minQuality),
// and the echoed guildId/minQuality/sinceHours keys. The now-relative sinceHours
// cutoff bind is matched with AnyArg (the handler uses the real clock); its exact
// value is pinned in TestCollectWowGuildBankItemFlow_SinceCutoff.
func TestWowGuildBankItemFlow_Filters(t *testing.T) {
	cases := []struct {
		name       string
		args       string
		whereSub   string
		wantArgs   []driver.Value
		wantGuild  int64 // -1 = key absent
		wantMinQ   int   // -1 = key absent
		wantSinceH int   // -1 = key absent
	}{
		{
			name:      "guildId only",
			args:      `{"guildId":42}`,
			whereSub:  "IN (1, 2, 3, 7) AND e.guildid = ? GROUP BY",
			wantArgs:  []driver.Value{uint64(42)},
			wantGuild: 42, wantMinQ: -1, wantSinceH: -1,
		},
		{
			name:       "sinceHours only",
			args:       `{"sinceHours":24}`,
			whereSub:   "IN (1, 2, 3, 7) AND e.TimeStamp >= ? GROUP BY",
			wantArgs:   []driver.Value{sqlmock.AnyArg()},
			wantGuild:  -1,
			wantMinQ:   -1,
			wantSinceH: 24,
		},
		{
			name:      "minQuality only",
			args:      `{"minQuality":3}`,
			whereSub:  "IN (1, 2, 3, 7) AND it.Quality >= ? GROUP BY",
			wantArgs:  []driver.Value{int64(3)},
			wantGuild: -1, wantMinQ: 3, wantSinceH: -1,
		},
		{
			name:       "all three filters",
			args:       `{"guildId":7,"sinceHours":12,"minQuality":2,"topN":5}`,
			whereSub:   "IN (1, 2, 3, 7) AND e.guildid = ? AND e.TimeStamp >= ? AND it.Quality >= ? GROUP BY",
			wantArgs:   []driver.Value{uint64(7), sqlmock.AnyArg(), int64(2)},
			wantGuild:  7,
			wantMinQ:   2,
			wantSinceH: 12,
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
				WithArgs(c.wantArgs...).
				WillReturnRows(sqlmock.NewRows(guildBankItemFlowCols()))

			reg := NewRegistry()
			RegisterWowGuildBankItemFlowTool(reg, DBDeps{QueryDB: db})
			tool, _ := reg.Get("wow_guild_bank_item_flow")
			resp := tool.Handler(context.Background(), json.RawMessage(c.args), "")
			got, _ := resp.(map[string]any)
			if got["error"] != nil {
				t.Fatalf("unexpected error: %v", got)
			}
			if c.wantGuild >= 0 {
				if g, _ := got["guildId"].(uint64); g != uint64(c.wantGuild) {
					t.Errorf("guildId echo: %v want %d", got["guildId"], c.wantGuild)
				}
			} else if _, ok := got["guildId"]; ok {
				t.Errorf("guildId should be absent, got %v", got["guildId"])
			}
			if c.wantMinQ >= 0 {
				if q, _ := got["minQuality"].(int); q != c.wantMinQ {
					t.Errorf("minQuality: %v want %d", got["minQuality"], c.wantMinQ)
				}
			} else if _, ok := got["minQuality"]; ok {
				t.Errorf("minQuality should be absent, got %v", got["minQuality"])
			}
			if c.wantSinceH >= 0 {
				if s, _ := got["sinceHours"].(int); s != c.wantSinceH {
					t.Errorf("sinceHours: %v want %d", got["sinceHours"], c.wantSinceH)
				}
				if _, ok := got["sinceCutoff"].(string); !ok {
					t.Errorf("sinceCutoff should be present when sinceHours set, got %v", got["sinceCutoff"])
				}
			} else if _, ok := got["sinceHours"]; ok {
				t.Errorf("sinceHours should be absent, got %v", got["sinceHours"])
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Errorf("unmet mock expectations: %v", err)
			}
		})
	}
}

// TestCollectWowGuildBankItemFlow_Golden folds the three-row fixture (sortBy
// quantity), verifying the quality/class enum maps, the scan field mapping
// (deposit/withdraw/move split + totalEvents + distinctActors + summed quantity),
// the honest realm totals (over ALL rows, not just the display slice), and that
// the Go-side sort reorders the entry-asc fetch into quantity-desc order (B, A, C).
func TestCollectWowGuildBankItemFlow_Golden(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	now := time.Unix(1_700_000_000, 0)
	mock.ExpectExec(regexp.QuoteMeta("USE `acore_characters`")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta("ORDER BY it.entry ASC")).
		WillReturnRows(guildBankItemFlowFixture())

	out, err := collectWowGuildBankItemFlow(context.Background(), db, now, 15, "quantity", "", nil, 0)
	if err != nil {
		t.Fatalf("collectWowGuildBankItemFlow: %v", err)
	}
	if di, _ := out["distinctItems"].(int); di != 3 {
		t.Errorf("distinctItems: %v want 3", out["distinctItems"])
	}
	if te, _ := out["totalItemEvents"].(int64); te != 23 {
		t.Errorf("totalItemEvents: %v want 23", out["totalItemEvents"])
	}
	if tq, _ := out["totalQuantityMoved"].(int64); tq != 356 {
		t.Errorf("totalQuantityMoved: %v want 356", out["totalQuantityMoved"])
	}
	if sb, _ := out["sortBy"].(string); sb != "quantity" {
		t.Errorf("sortBy echo: %v want quantity", out["sortBy"])
	}
	for _, k := range []string{"guildId", "minQuality", "sinceHours", "sinceCutoff"} {
		if _, ok := out[k]; ok {
			t.Errorf("%s should be absent with no filter, got %v", k, out[k])
		}
	}
	items, ok := out["items"].([]*guildBankItemFlowRow)
	if !ok || len(items) != 3 {
		t.Fatalf("items: %T len %d want []*guildBankItemFlowRow len 3", out["items"], len(items))
	}

	// quantity desc: B(300), A(50), C(6).
	if items[0].ItemEntry != 29434 || items[1].ItemEntry != 21877 || items[2].ItemEntry != 40752 {
		t.Errorf("quantity order: got %d,%d,%d want 29434,21877,40752", items[0].ItemEntry, items[1].ItemEntry, items[2].ItemEntry)
	}

	// items[0] is Badge of Justice — Epic / Quest, folds + quantity.
	b := items[0]
	if b.ItemName != "Badge of Justice" || b.Quality != 4 || b.QualityName != "Epic" || b.Class != 12 || b.ClassName != "Quest" {
		t.Errorf("items[0] identity: %+v", b)
	}
	if b.Deposits != 2 || b.Withdrawals != 6 || b.Moves != 0 || b.TotalEvents != 8 || b.DistinctActors != 2 || b.QuantityMoved != 300 {
		t.Errorf("items[0] folds: %+v", b)
	}

	// items[1] is Netherweave Cloth — Common / Trade Goods.
	a := items[1]
	if a.QualityName != "Common" || a.ClassName != "Trade Goods" || a.Deposits != 5 || a.Withdrawals != 3 || a.Moves != 1 || a.QuantityMoved != 50 {
		t.Errorf("items[1]: %+v want Common/Trade Goods/5 dep/3 wd/1 move/50 qty", a)
	}

	// items[2] is Emblem of Heroism — Rare / Miscellaneous.
	c := items[2]
	if c.QualityName != "Rare" || c.ClassName != "Miscellaneous" || c.DistinctActors != 6 || c.Moves != 4 {
		t.Errorf("items[2]: %+v want Rare/Miscellaneous/6 actors/4 moves", c)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestCollectWowGuildBankItemFlow_SortKeys re-runs the same fixture under each
// sortBy key and asserts the Go-side comparator reads the matching aggregate —
// the three keys yield three DISTINCT orderings (see guildBankItemFlowFixture).
func TestCollectWowGuildBankItemFlow_SortKeys(t *testing.T) {
	cases := []struct {
		sortBy    string
		wantOrder []int64
	}{
		{"quantity", []int64{29434, 21877, 40752}}, // B, A, C
		{"events", []int64{21877, 29434, 40752}},   // A, B, C
		{"actors", []int64{40752, 21877, 29434}},   // C, A, B
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
			mock.ExpectQuery(regexp.QuoteMeta("ORDER BY it.entry ASC")).
				WillReturnRows(guildBankItemFlowFixture())

			out, err := collectWowGuildBankItemFlow(context.Background(), db, time.Unix(1_700_000_000, 0), 15, c.sortBy, "", nil, 0)
			if err != nil {
				t.Fatalf("collectWowGuildBankItemFlow: %v", err)
			}
			items, _ := out["items"].([]*guildBankItemFlowRow)
			if len(items) != 3 {
				t.Fatalf("items len %d want 3", len(items))
			}
			for i, want := range c.wantOrder {
				if items[i].ItemEntry != want {
					t.Errorf("sortBy=%s pos %d: got %d want %d", c.sortBy, i, items[i].ItemEntry, want)
				}
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Errorf("unmet mock expectations: %v", err)
			}
		})
	}
}

// TestCollectWowGuildBankItemFlow_TopNSlice — topN cuts the DISPLAY slice while the
// honest totals stay computed over the full folded set (the reason there is no SQL
// LIMIT). topN=2, quantity desc keeps B, A and drops C, but totals still see all 3.
func TestCollectWowGuildBankItemFlow_TopNSlice(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.ExpectExec(regexp.QuoteMeta("USE `acore_characters`")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta("ORDER BY it.entry ASC")).
		WillReturnRows(guildBankItemFlowFixture())

	out, err := collectWowGuildBankItemFlow(context.Background(), db, time.Unix(1_700_000_000, 0), 2, "quantity", "", nil, 0)
	if err != nil {
		t.Fatalf("collectWowGuildBankItemFlow: %v", err)
	}
	items, _ := out["items"].([]*guildBankItemFlowRow)
	if len(items) != 2 {
		t.Fatalf("items len %d want 2 (sliced to topN)", len(items))
	}
	if items[0].ItemEntry != 29434 || items[1].ItemEntry != 21877 {
		t.Errorf("topN slice: got %d,%d want 29434,21877", items[0].ItemEntry, items[1].ItemEntry)
	}
	if di, _ := out["distinctItems"].(int); di != 3 {
		t.Errorf("distinctItems: %v want 3 (full set, not sliced)", out["distinctItems"])
	}
	if tq, _ := out["totalQuantityMoved"].(int64); tq != 356 {
		t.Errorf("totalQuantityMoved: %v want 356 (full set)", out["totalQuantityMoved"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestCollectWowGuildBankItemFlow_SinceCutoff pins the exact now-relative cutoff:
// the TimeStamp column is uint32 unix seconds, so the bind is cutoff.Unix() (int64),
// NOT a time.Time. With now=1_700_000_000 and sinceHours=6 the cutoff is
// 1_700_000_000 - 6*3600 = 1_699_978_400, and sinceCutoff echoes its RFC3339 form.
func TestCollectWowGuildBankItemFlow_SinceCutoff(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	now := time.Unix(1_700_000_000, 0)
	wantCutoff := int64(1_699_978_400)
	mock.ExpectExec(regexp.QuoteMeta("USE `acore_characters`")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta("IN (1, 2, 3, 7) AND e.TimeStamp >= ? GROUP BY")).
		WithArgs(wantCutoff).
		WillReturnRows(sqlmock.NewRows(guildBankItemFlowCols()))

	out, err := collectWowGuildBankItemFlow(context.Background(), db, now, 15, "quantity", "", nil, 6)
	if err != nil {
		t.Fatalf("collectWowGuildBankItemFlow: %v", err)
	}
	if s, _ := out["sinceHours"].(int); s != 6 {
		t.Errorf("sinceHours echo: %v want 6", out["sinceHours"])
	}
	wantISO := time.Unix(wantCutoff, 0).UTC().Format(time.RFC3339)
	if iso, _ := out["sinceCutoff"].(string); iso != wantISO {
		t.Errorf("sinceCutoff echo: %v want %s", out["sinceCutoff"], wantISO)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestCollectWowGuildBankItemFlow_Empty — an empty eventlog window yields a non-nil
// empty items slice (callers expect an array) and zeroed honest totals.
func TestCollectWowGuildBankItemFlow_Empty(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.ExpectExec(regexp.QuoteMeta("USE `acore_characters`")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta("ORDER BY it.entry ASC")).
		WillReturnRows(sqlmock.NewRows(guildBankItemFlowCols()))

	out, err := collectWowGuildBankItemFlow(context.Background(), db, time.Unix(1_700_000_000, 0), 15, "quantity", "", nil, 0)
	if err != nil {
		t.Fatalf("collectWowGuildBankItemFlow: %v", err)
	}
	items, ok := out["items"].([]*guildBankItemFlowRow)
	if !ok {
		t.Fatalf("items type: %T want []*guildBankItemFlowRow", out["items"])
	}
	if items == nil || len(items) != 0 {
		t.Errorf("items: %v want non-nil empty slice", items)
	}
	if di, _ := out["distinctItems"].(int); di != 0 {
		t.Errorf("distinctItems: %v want 0", out["distinctItems"])
	}
	if te, _ := out["totalItemEvents"].(int64); te != 0 {
		t.Errorf("totalItemEvents: %v want 0", out["totalItemEvents"])
	}
	if tq, _ := out["totalQuantityMoved"].(int64); tq != 0 {
		t.Errorf("totalQuantityMoved: %v want 0", out["totalQuantityMoved"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}
