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

// guildBankItemRowCols is the column set wow_guild_bank_item_breakdown scans, in order.
func guildBankItemRowCols() []string {
	return []string{"entry", "name", "Quality", "class", "stacks", "distinctGuilds", "distinctTabs", "totalQuantity"}
}

// TestWowGuildBankItemBreakdown_DescriptionMentionsContext guards the keywords that steer
// the agent here vs wow_guild_bank_summary (money/tab census) or a hand-written db_query —
// the item-join framing + guild-bank-contents vocabulary is load-bearing for tool selection.
func TestWowGuildBankItemBreakdown_DescriptionMentionsContext(t *testing.T) {
	reg := NewRegistry()
	RegisterWowGuildBankItemBreakdownTool(reg, DBDeps{})
	tool, ok := reg.Get("wow_guild_bank_item_breakdown")
	if !ok {
		t.Fatal("wow_guild_bank_item_breakdown not registered")
	}
	for _, kw := range []string{
		"acore_characters.guild_bank_item", "item_instance", "acore_world.item_template",
		"wow_guild_bank_summary", "wow_mail_item_breakdown", "ops_ro", "qualityName", "Heirloom",
		"className", "Glyph", "stacks", "distinctGuilds", "distinctTabs", "totalQuantity",
		"minQuality", "guildId", "sortBy", "topN", "Read-only",
	} {
		if !strings.Contains(tool.Description, kw) {
			t.Errorf("description missing %q: %s", kw, tool.Description)
		}
	}
}

// TestWowGuildBankItemBreakdown_ReadOnlyAnnotation pins readOnlyHint=true so a future
// copy-paste of a destructive sibling can't silently flip the gate.
func TestWowGuildBankItemBreakdown_ReadOnlyAnnotation(t *testing.T) {
	reg := NewRegistry()
	RegisterWowGuildBankItemBreakdownTool(reg, DBDeps{})
	tool, _ := reg.Get("wow_guild_bank_item_breakdown")
	if !strings.Contains(string(tool.Annotations), `"readOnlyHint":true`) {
		t.Errorf("annotations missing readOnlyHint=true: %s", tool.Annotations)
	}
}

func TestWowGuildBankItemBreakdown_NoPoolReturnsError(t *testing.T) {
	reg := NewRegistry()
	RegisterWowGuildBankItemBreakdownTool(reg, DBDeps{}) // QueryDB nil
	tool, _ := reg.Get("wow_guild_bank_item_breakdown")
	resp := tool.Handler(context.Background(), json.RawMessage(`{}`), "")
	got, _ := resp.(map[string]any)
	if msg, _ := got["error"].(string); !strings.Contains(msg, "ops_ro pool not configured") {
		t.Errorf("expected pool-not-configured error, got %v", got)
	}
}

// TestWowGuildBankItemBreakdown_InvalidSortBy — an unknown sortBy is rejected (no DB call)
// so a typo can't silently default, and only allowlisted clauses ever reach the ORDER BY.
func TestWowGuildBankItemBreakdown_InvalidSortBy(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// No ExpectExec / ExpectQuery — the handler must bail before touching the DB.
	reg := NewRegistry()
	RegisterWowGuildBankItemBreakdownTool(reg, DBDeps{QueryDB: db})
	tool, _ := reg.Get("wow_guild_bank_item_breakdown")
	resp := tool.Handler(context.Background(), json.RawMessage(`{"sortBy":"; DROP TABLE guild_bank_item"}`), "")
	got, _ := resp.(map[string]any)
	if msg, _ := got["error"].(string); !strings.Contains(msg, "invalid sortBy") {
		t.Errorf("expected invalid-sortBy error, got %v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("a query was issued on a bad sortBy: %v", err)
	}
}

// TestWowGuildBankItemBreakdown_TopNClamps asserts the post-clamp topN echo (unset->default,
// zero/negative->default, oversized->max, in-range passthrough) and that the LIMIT bind
// matches the clamped value. Default sortBy resolves to the stacks clause; no filters so the
// only bind is LIMIT.
func TestWowGuildBankItemBreakdown_TopNClamps(t *testing.T) {
	cases := []struct {
		name     string
		args     string
		wantTopN int
	}{
		{"unset uses default", `{}`, wowGuildBankItemsDefaultTop},
		{"zero falls back to default", `{"topN":0}`, wowGuildBankItemsDefaultTop},
		{"negative falls back to default", `{"topN":-7}`, wowGuildBankItemsDefaultTop},
		{"oversized clamps to max", `{"topN":99999}`, wowGuildBankItemsMaxTop},
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
			mock.ExpectQuery(regexp.QuoteMeta("ORDER BY stacks DESC, it.entry ASC LIMIT ?")).
				WithArgs(int64(c.wantTopN)).
				WillReturnRows(sqlmock.NewRows(guildBankItemRowCols()))

			reg := NewRegistry()
			RegisterWowGuildBankItemBreakdownTool(reg, DBDeps{QueryDB: db})
			tool, _ := reg.Get("wow_guild_bank_item_breakdown")
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

// TestWowGuildBankItemBreakdown_SortByMapsToClause proves each allowlisted sortBy resolves
// to its ORDER BY clause verbatim (the clause is interpolated, so a wrong mapping is a
// silent injection/behaviour bug). Drives the handler once per key and matches the SQL.
func TestWowGuildBankItemBreakdown_SortByMapsToClause(t *testing.T) {
	cases := []struct {
		sortBy string
		clause string
	}{
		{"stacks", "ORDER BY stacks DESC, it.entry ASC LIMIT ?"},
		{"quantity", "ORDER BY totalQuantity DESC, it.entry ASC LIMIT ?"},
		{"guilds", "ORDER BY distinctGuilds DESC, stacks DESC, it.entry ASC LIMIT ?"},
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
			mock.ExpectQuery(regexp.QuoteMeta(c.clause)).
				WithArgs(int64(wowGuildBankItemsDefaultTop)).
				WillReturnRows(sqlmock.NewRows(guildBankItemRowCols()))

			reg := NewRegistry()
			RegisterWowGuildBankItemBreakdownTool(reg, DBDeps{QueryDB: db})
			tool, _ := reg.Get("wow_guild_bank_item_breakdown")
			resp := tool.Handler(context.Background(), json.RawMessage(`{"sortBy":"`+c.sortBy+`"}`), "")
			got, _ := resp.(map[string]any)
			if got["error"] != nil {
				t.Fatalf("unexpected error: %v", got)
			}
			if sb, _ := got["sortBy"].(string); sb != c.sortBy {
				t.Errorf("sortBy echo: %v want %s", got["sortBy"], c.sortBy)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Errorf("unmet mock expectations: %v", err)
			}
		})
	}
}

// TestWowGuildBankItemBreakdown_MinQualityRejected — out-of-range minQuality is rejected (no
// DB call) so a typo can't masquerade as an empty guild bank. QueryDB is non-nil to prove
// the reject fires AFTER the pool check but before any query.
func TestWowGuildBankItemBreakdown_MinQualityRejected(t *testing.T) {
	for _, bad := range []string{`{"minQuality":-1}`, `{"minQuality":8}`, `{"minQuality":99}`} {
		db, mock, err := sqlmock.New()
		if err != nil {
			t.Fatal(err)
		}
		reg := NewRegistry()
		RegisterWowGuildBankItemBreakdownTool(reg, DBDeps{QueryDB: db})
		tool, _ := reg.Get("wow_guild_bank_item_breakdown")
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

// TestWowGuildBankItemBreakdown_GuildIdRejectsBad — a non-numeric / negative guildId is
// rejected BEFORE any query is issued (no SQL round trip on a bad filter).
func TestWowGuildBankItemBreakdown_GuildIdRejectsBad(t *testing.T) {
	for _, args := range []string{`{"guildId":"abc"}`, `{"guildId":-5}`} {
		db, mock, err := sqlmock.New()
		if err != nil {
			t.Fatal(err)
		}
		// No ExpectExec / ExpectQuery — the handler must bail before touching the DB.
		reg := NewRegistry()
		RegisterWowGuildBankItemBreakdownTool(reg, DBDeps{QueryDB: db})
		tool, _ := reg.Get("wow_guild_bank_item_breakdown")
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

// TestWowGuildBankItemBreakdown_Filters drives the handler with each filter combination and
// asserts the WHERE clause shape, the bound args (filters in text order, then LIMIT), and
// the echoed guildId/minQuality keys.
func TestWowGuildBankItemBreakdown_Filters(t *testing.T) {
	cases := []struct {
		name      string
		args      string
		whereSub  string
		wantArgs  []driver.Value
		wantGuild int64 // -1 = key absent
		wantMinQ  int   // -1 = key absent
	}{
		{
			name:     "guildId only",
			args:     `{"guildId":42}`,
			whereSub: "WHERE gbi.guildid = ? GROUP BY",
			// guildId bind, LIMIT bind.
			wantArgs:  []driver.Value{uint64(42), int64(wowGuildBankItemsDefaultTop)},
			wantGuild: 42, wantMinQ: -1,
		},
		{
			name:      "minQuality only",
			args:      `{"minQuality":4}`,
			whereSub:  "WHERE it.Quality >= ? GROUP BY",
			wantArgs:  []driver.Value{int64(4), int64(wowGuildBankItemsDefaultTop)},
			wantGuild: -1, wantMinQ: 4,
		},
		{
			name:     "both filters",
			args:     `{"guildId":7,"minQuality":3,"topN":5}`,
			whereSub: "WHERE gbi.guildid = ? AND it.Quality >= ? GROUP BY",
			// guildId, minQuality, LIMIT.
			wantArgs:  []driver.Value{uint64(7), int64(3), int64(5)},
			wantGuild: 7, wantMinQ: 3,
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
				WillReturnRows(sqlmock.NewRows(guildBankItemRowCols()))

			reg := NewRegistry()
			RegisterWowGuildBankItemBreakdownTool(reg, DBDeps{QueryDB: db})
			tool, _ := reg.Get("wow_guild_bank_item_breakdown")
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
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Errorf("unmet mock expectations: %v", err)
			}
		})
	}
}

// TestCollectWowGuildBankItemBreakdown_Golden folds four grouped rows and verifies the
// qualityName + className mappings (including the never-vanish quality-<q>/class-<c>
// fallback for out-of-enum values), the scan field mapping (stacks + distinct folds +
// summed quantity), distinctItems, the LIMIT bind, and that the handler trusts the SQL
// ORDER BY (no Go-side re-sort).
func TestCollectWowGuildBankItemBreakdown_Golden(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	now := time.Unix(1_700_000_000, 0)
	mock.ExpectExec(regexp.QuoteMeta("USE `acore_characters`")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta("ORDER BY stacks DESC, it.entry ASC LIMIT ?")).
		WithArgs(int64(15)).
		WillReturnRows(sqlmock.NewRows(guildBankItemRowCols()).
			// Netherweave Cloth: 12 slots across 3 guilds / 5 tabs, 2400 total (Trade Goods).
			AddRow(int64(21877), "Netherweave Cloth", 1, 7, 12, 3, 5, int64(2400)).
			// Frostweave Cloth: 5 slots across 2 guilds / 3 tabs, 1000 total (Trade Goods).
			AddRow(int64(33470), "Frostweave Cloth", 1, 7, 5, 2, 3, int64(1000)).
			// Thunderfury: a single legendary weapon in one guild's vault.
			AddRow(int64(19019), "Thunderfury", 5, 2, 1, 1, 1, int64(1)).
			// Out-of-enum quality/class exercise the never-vanish fallback (row not dropped).
			AddRow(int64(99999), "Anomalous Trinket", 9, 99, 1, 1, 1, int64(1)))

	out, err := collectWowGuildBankItemBreakdown(context.Background(), db, now, 15, "stacks", "stacks DESC, it.entry ASC", "", nil)
	if err != nil {
		t.Fatalf("collectWowGuildBankItemBreakdown: %v", err)
	}
	if di, _ := out["distinctItems"].(int); di != 4 {
		t.Errorf("distinctItems: %v want 4", out["distinctItems"])
	}
	if sb, _ := out["sortBy"].(string); sb != "stacks" {
		t.Errorf("sortBy echo: %v want stacks", out["sortBy"])
	}
	for _, k := range []string{"guildId", "minQuality"} {
		if _, ok := out[k]; ok {
			t.Errorf("%s should be absent with no filter, got %v", k, out[k])
		}
	}
	items, ok := out["items"].([]*guildBankItemRow)
	if !ok || len(items) != 4 {
		t.Fatalf("items: %T len %d want []*guildBankItemRow len 4", out["items"], len(items))
	}

	it0 := items[0]
	if it0.ItemEntry != 21877 || it0.ItemName != "Netherweave Cloth" || it0.Quality != 1 || it0.QualityName != "Common" ||
		it0.Class != 7 || it0.ClassName != "Trade Goods" {
		t.Errorf("items[0] identity: %+v", it0)
	}
	if it0.Stacks != 12 || it0.DistinctGuilds != 3 || it0.DistinctTabs != 5 || it0.TotalQuantity != 2400 {
		t.Errorf("items[0] folds: %+v", it0)
	}

	it1 := items[1]
	if it1.ItemName != "Frostweave Cloth" || it1.Stacks != 5 || it1.DistinctGuilds != 2 || it1.TotalQuantity != 1000 {
		t.Errorf("items[1]: %+v want Frostweave/5 slots/2 guilds/1000 total", it1)
	}

	it2 := items[2]
	if it2.QualityName != "Legendary" || it2.ClassName != "Weapon" || it2.Stacks != 1 || it2.TotalQuantity != 1 {
		t.Errorf("items[2]: %+v want Legendary/Weapon/1 slot/1 total", it2)
	}

	it3 := items[3]
	if it3.QualityName != "quality-9" || it3.ClassName != "class-99" {
		t.Errorf("items[3] fallback: %+v want quality-9/class-99", it3)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestCollectWowGuildBankItemBreakdown_Empty — an empty (or fully-filtered) guild bank
// yields a non-nil empty items slice (callers expect an array) and distinctItems 0.
func TestCollectWowGuildBankItemBreakdown_Empty(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.ExpectExec(regexp.QuoteMeta("USE `acore_characters`")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta("ORDER BY stacks DESC, it.entry ASC LIMIT ?")).
		WithArgs(int64(15)).
		WillReturnRows(sqlmock.NewRows(guildBankItemRowCols()))

	out, err := collectWowGuildBankItemBreakdown(context.Background(), db, time.Unix(1_700_000_000, 0), 15, "stacks", "stacks DESC, it.entry ASC", "", nil)
	if err != nil {
		t.Fatalf("collectWowGuildBankItemBreakdown: %v", err)
	}
	items, ok := out["items"].([]*guildBankItemRow)
	if !ok {
		t.Fatalf("items type: %T want []*guildBankItemRow", out["items"])
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
