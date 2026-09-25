package mcpserver

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// TestDBWidestCompositeIndexes_DescriptionMentionsContext anchors the
// description to the keywords the agent uses to pick this tool. Same shape guard
// as the other db_* tools; the keywords must live in the registered Description
// string (not the Go doc-comment).
func TestDBWidestCompositeIndexes_DescriptionMentionsContext(t *testing.T) {
	reg := NewRegistry()
	RegisterDBWidestCompositeIndexesTool(reg, DBDeps{})
	tool, ok := reg.Get("db_widest_composite_indexes")
	if !ok {
		t.Fatal("db_widest_composite_indexes not registered")
	}
	for _, kw := range []string{
		"widest", "composite", "column", "columnCount", "columns",
		"information_schema.STATISTICS", "SEQ_IN_INDEX", "GROUP_CONCAT",
		"isPrimary", "severity", "minColumns", "includePrimary", "perDatabase",
		"db_over_indexed_tables", "db_index_key_length_audit", "db_index_data_ratio",
		"db_redundant_indexes", "db_low_cardinality_indexes",
		"acore_playerbots",
	} {
		if !strings.Contains(tool.Description, kw) {
			t.Errorf("description missing %q", kw)
		}
	}
}

func TestDBWidestCompositeIndexes_ReadOnlyAnnotation(t *testing.T) {
	reg := NewRegistry()
	RegisterDBWidestCompositeIndexesTool(reg, DBDeps{})
	tool, _ := reg.Get("db_widest_composite_indexes")
	if !strings.Contains(string(tool.Annotations), `"readOnlyHint":true`) {
		t.Errorf("expected readOnlyHint:true, got %s", tool.Annotations)
	}
	if tool.Destructive {
		t.Errorf("destructive should be false")
	}
}

func TestDBWidestCompositeIndexes_NoPoolReturnsError(t *testing.T) {
	reg := NewRegistry()
	RegisterDBWidestCompositeIndexesTool(reg, DBDeps{})
	tool, _ := reg.Get("db_widest_composite_indexes")
	resp := tool.Handler(context.Background(), json.RawMessage(`{}`), "")
	got, _ := resp.(map[string]any)
	if msg, _ := got["error"].(string); !strings.Contains(msg, "ops_ro pool not configured") {
		t.Errorf("expected pool-not-configured error, got %v", got)
	}
}

func TestDBWidestCompositeIndexes_InvalidDatabaseRejected(t *testing.T) {
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	reg := NewRegistry()
	RegisterDBWidestCompositeIndexesTool(reg, DBDeps{QueryDB: db})
	tool, _ := reg.Get("db_widest_composite_indexes")
	// acore_playerbots is intentionally NOT in the allowlist — ops_ro lacks the
	// grant, so it would scan nothing yet look like a clean result.
	resp := tool.Handler(context.Background(), json.RawMessage(`{"database":"acore_playerbots"}`), "")
	got, _ := resp.(map[string]any)
	if msg, _ := got["error"].(string); !strings.Contains(msg, "acore_world/acore_characters/acore_auth") {
		t.Errorf("expected allowlist error, got %v", got)
	}
}

// wideIndexRowCols is the column set the aggregate SELECT returns, in order.
var wideIndexRowCols = []string{"TABLE_SCHEMA", "TABLE_NAME", "INDEX_NAME", "NON_UNIQUE", "INDEX_TYPE", "COLUMN_COUNT", "COLUMNS"}

// TestDBWidestCompositeIndexes_GoldenAggregation drives the default path (all
// three schemas, default minColumns=4, includePrimary=true). Fixture mixes
// below-floor indexes (skipped but counted in the denominator), one high (6
// columns), and two medium (4 columns each — one secondary, one PRIMARY).
// Verifies: below-floor filtered out, severity banding, widest-first sort (high
// then the two mediums with secondary before PRIMARY at equal width), the
// PRIMARY-specific reason note, the ordered column split, perDatabase counts
// incl. denominator, and honest totals.
func TestDBWidestCompositeIndexes_GoldenAggregation(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectQuery(regexp.QuoteMeta("GROUP_CONCAT(s.COLUMN_NAME ORDER BY s.SEQ_IN_INDEX")).
		WithArgs("acore_world", "acore_characters", "acore_auth").
		WillReturnRows(sqlmock.NewRows(wideIndexRowCols).
			// 2-column secondary — below default floor, skipped but counted.
			AddRow("acore_world", "creature_template", "idx_narrow", int64(1), "BTREE", int64(2), "entry,map").
			// 2-column PRIMARY — below floor, skipped but counted.
			AddRow("acore_characters", "character_spell", "PRIMARY", int64(0), "BTREE", int64(2), "guid,spell").
			// 6-column secondary → HIGH.
			AddRow("acore_characters", "item_instance", "idx_owner_bag", int64(1), "BTREE", int64(6), "a,b,c,d,e,f").
			// 4-column PRIMARY → MEDIUM, isPrimary (unavoidable-PK note).
			AddRow("acore_characters", "mail_items", "PRIMARY", int64(0), "BTREE", int64(4), "mailid,item_guid,slot,receiver").
			// 4-column secondary → MEDIUM (sorts before the 4-col PRIMARY).
			AddRow("acore_characters", "characters", "idx_online", int64(1), "BTREE", int64(4), "online,account,level,race").
			// 3-column secondary — below floor, skipped but counted.
			AddRow("acore_auth", "account", "idx_username", int64(1), "BTREE", int64(3), "username,email,joindate"))

	reg := NewRegistry()
	RegisterDBWidestCompositeIndexesTool(reg, DBDeps{QueryDB: db})
	tool, _ := reg.Get("db_widest_composite_indexes")
	resp := tool.Handler(context.Background(), json.RawMessage(`{}`), "")
	got, ok := resp.(map[string]any)
	if !ok {
		t.Fatalf("unexpected response shape: %T", resp)
	}
	if msg, hasErr := got["error"]; hasErr {
		t.Fatalf("unexpected error: %v", msg)
	}

	rows := got["rows"].([]wideIndexRow)
	if len(rows) != 3 {
		t.Fatalf("rows len: %d (%+v)", len(rows), rows)
	}
	// Widest-first: HIGH (6-col) first.
	if rows[0].Table != "item_instance" || rows[0].Severity != "high" || rows[0].ColumnCount != 6 || rows[0].IsPrimary {
		t.Errorf("rows[0] should be the high 6-col item_instance: %+v", rows[0])
	}
	if len(rows[0].Columns) != 6 || rows[0].Columns[0] != "a" || rows[0].Columns[5] != "f" {
		t.Errorf("rows[0] columns should be the ordered 6-col split: %v", rows[0].Columns)
	}
	if !strings.Contains(rows[0].Reason, "very wide") {
		t.Errorf("high reason should mention very wide: %q", rows[0].Reason)
	}
	// Then the two 4-col mediums, secondary before PRIMARY.
	if rows[1].Table != "characters" || rows[1].IndexName != "idx_online" || rows[1].Severity != "medium" || rows[1].ColumnCount != 4 || rows[1].IsPrimary || rows[1].Unique {
		t.Errorf("rows[1] should be the medium secondary characters.idx_online: %+v", rows[1])
	}
	if rows[2].Table != "mail_items" || rows[2].IndexName != "PRIMARY" || rows[2].Severity != "medium" || rows[2].ColumnCount != 4 || !rows[2].IsPrimary || !rows[2].Unique {
		t.Errorf("rows[2] should be the medium 4-col PRIMARY mail_items: %+v", rows[2])
	}
	if !strings.Contains(rows[2].Reason, "PRIMARY KEY") {
		t.Errorf("PRIMARY reason should carry the unavoidable-PK note: %q", rows[2].Reason)
	}

	perDB := got["perDatabase"].([]wideIndexPerDatabase)
	if len(perDB) != 3 {
		t.Fatalf("perDatabase len: %d", len(perDB))
	}
	// Sorted wideCount desc, schema-asc tiebreak: characters(3) then auth(0) then world(0).
	if perDB[0].Database != "acore_characters" || perDB[0].WideCount != 3 || perDB[0].IndexesScanned != 4 {
		t.Errorf("perDB[0]: %+v", perDB[0])
	}
	if perDB[1].Database != "acore_auth" || perDB[1].WideCount != 0 || perDB[1].IndexesScanned != 1 {
		t.Errorf("perDB[1] (acore_auth, clean denominator-only): %+v", perDB[1])
	}
	if perDB[2].Database != "acore_world" || perDB[2].WideCount != 0 || perDB[2].IndexesScanned != 1 {
		t.Errorf("perDB[2] (acore_world, clean denominator-only): %+v", perDB[2])
	}

	totals := got["totals"].(map[string]any)
	if totals["indexesScanned"].(int) != 6 {
		t.Errorf("totals.indexesScanned: %v", totals["indexesScanned"])
	}
	if totals["wideCount"].(int) != 3 {
		t.Errorf("totals.wideCount: %v", totals["wideCount"])
	}
	if got["truncated"].(bool) {
		t.Errorf("should not be truncated for 3 rows")
	}
	if got["minColumns"].(int) != 4 {
		t.Errorf("minColumns should default to 4, got %v", got["minColumns"])
	}
	if got["includePrimary"].(bool) != true {
		t.Errorf("includePrimary should default to true, got %v", got["includePrimary"])
	}
	if got["topN"].(int) != 10 {
		t.Errorf("topN should default to 10, got %v", got["topN"])
	}
	if got["asOf"].(string) == "" {
		t.Errorf("asOf should be populated")
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestDBWidestCompositeIndexes_DatabaseFilterNarrowsScan verifies passing
// `database` narrows the IN clause to a single bind (asserted via WithArgs).
func TestDBWidestCompositeIndexes_DatabaseFilterNarrowsScan(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectQuery(regexp.QuoteMeta("GROUP_CONCAT(s.COLUMN_NAME ORDER BY s.SEQ_IN_INDEX")).
		WithArgs("acore_characters"). // single bind — IN list narrowed to one schema
		WillReturnRows(sqlmock.NewRows(wideIndexRowCols).
			AddRow("acore_characters", "item_instance", "idx_wide", int64(1), "BTREE", int64(5), "a,b,c,d,e"))

	reg := NewRegistry()
	RegisterDBWidestCompositeIndexesTool(reg, DBDeps{QueryDB: db})
	tool, _ := reg.Get("db_widest_composite_indexes")
	resp := tool.Handler(context.Background(), json.RawMessage(`{"database":"acore_characters"}`), "")
	got := resp.(map[string]any)
	scanned := got["scannedSchemas"].([]string)
	if len(scanned) != 1 || scanned[0] != "acore_characters" {
		t.Errorf("scannedSchemas: %v", scanned)
	}
	rows := got["rows"].([]wideIndexRow)
	if len(rows) != 1 || rows[0].Table != "item_instance" || rows[0].Severity != "medium" || rows[0].ColumnCount != 5 {
		t.Errorf("rows: %+v", rows)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestDBWidestCompositeIndexes_IncludePrimaryFalseHidesPK verifies
// includePrimary:false drops PRIMARY rows from the candidate list while still
// counting them in the indexesScanned denominator.
func TestDBWidestCompositeIndexes_IncludePrimaryFalseHidesPK(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectQuery(regexp.QuoteMeta("GROUP_CONCAT(s.COLUMN_NAME ORDER BY s.SEQ_IN_INDEX")).
		WithArgs("acore_world").
		WillReturnRows(sqlmock.NewRows(wideIndexRowCols).
			// Wide PRIMARY — hidden by includePrimary:false but still scanned.
			AddRow("acore_world", "creature_loot_template", "PRIMARY", int64(0), "BTREE", int64(5), "entry,item,a,b,c").
			// Wide secondary — the only survivor.
			AddRow("acore_world", "gameobject", "idx_wide", int64(1), "BTREE", int64(4), "map,zone,area,phase"))

	reg := NewRegistry()
	RegisterDBWidestCompositeIndexesTool(reg, DBDeps{QueryDB: db})
	tool, _ := reg.Get("db_widest_composite_indexes")
	resp := tool.Handler(context.Background(), json.RawMessage(`{"database":"acore_world","includePrimary":false}`), "")
	got := resp.(map[string]any)
	if got["includePrimary"].(bool) != false {
		t.Errorf("includePrimary should echo false, got %v", got["includePrimary"])
	}
	rows := got["rows"].([]wideIndexRow)
	if len(rows) != 1 || rows[0].Table != "gameobject" || rows[0].IsPrimary {
		t.Fatalf("expected only the secondary gameobject index, got %+v", rows)
	}
	// Denominator counts BOTH indexes; wideCount counts only the surviving secondary.
	totals := got["totals"].(map[string]any)
	if totals["indexesScanned"].(int) != 2 {
		t.Errorf("indexesScanned should count the hidden PRIMARY too: %v", totals["indexesScanned"])
	}
	if totals["wideCount"].(int) != 1 {
		t.Errorf("wideCount should be 1 (PRIMARY excluded): %v", totals["wideCount"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestDBWidestCompositeIndexes_MinColumnsLowersFloor verifies lowering
// minColumns surfaces the `low` band (< 4 columns) the default suppresses,
// echoes the threshold, and excludes indexes below the lowered floor.
func TestDBWidestCompositeIndexes_MinColumnsLowersFloor(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectQuery(regexp.QuoteMeta("GROUP_CONCAT(s.COLUMN_NAME ORDER BY s.SEQ_IN_INDEX")).
		WithArgs("acore_world").
		WillReturnRows(sqlmock.NewRows(wideIndexRowCols).
			// 4-col → medium (reported at any minColumns <= 4).
			AddRow("acore_world", "gameobject", "idx_a", int64(1), "BTREE", int64(4), "a,b,c,d").
			// 3-col → low band, surfaced only because minColumns lowered to 3.
			AddRow("acore_world", "creature", "idx_b", int64(1), "BTREE", int64(3), "a,b,c").
			// 2-col → still below the lowered floor of 3, excluded.
			AddRow("acore_world", "spell_dbc", "idx_c", int64(1), "BTREE", int64(2), "a,b"))

	reg := NewRegistry()
	RegisterDBWidestCompositeIndexesTool(reg, DBDeps{QueryDB: db})
	tool, _ := reg.Get("db_widest_composite_indexes")
	resp := tool.Handler(context.Background(), json.RawMessage(`{"database":"acore_world","minColumns":3}`), "")
	got := resp.(map[string]any)
	if got["minColumns"].(int) != 3 {
		t.Errorf("minColumns should echo 3, got %v", got["minColumns"])
	}
	rows := got["rows"].([]wideIndexRow)
	if len(rows) != 2 {
		t.Fatalf("expected 2 rows (4-col and 3-col), got %+v", rows)
	}
	if rows[0].Table != "gameobject" || rows[0].Severity != "medium" {
		t.Errorf("rows[0] should be the medium 4-col gameobject: %+v", rows[0])
	}
	if rows[1].Table != "creature" || rows[1].Severity != "low" || rows[1].ColumnCount != 3 {
		t.Errorf("rows[1] should be the low-band 3-col creature: %+v", rows[1])
	}
	if !strings.Contains(rows[1].Reason, "minColumns was lowered") {
		t.Errorf("low reason should explain the lowered threshold: %q", rows[1].Reason)
	}
	if got["totals"].(map[string]any)["indexesScanned"].(int) != 3 {
		t.Errorf("all three rows count toward the denominator: %v", got["totals"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestDBWidestCompositeIndexes_MinColumnsResetsBelowMin verifies a minColumns
// below the allowed minimum (2) — which would flag single-column indexes —
// resets to the default of 4.
func TestDBWidestCompositeIndexes_MinColumnsResetsBelowMin(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectQuery(regexp.QuoteMeta("GROUP_CONCAT(s.COLUMN_NAME ORDER BY s.SEQ_IN_INDEX")).
		WithArgs("acore_auth").
		WillReturnRows(sqlmock.NewRows(wideIndexRowCols).
			// 3-col — would surface at floor 1, but the floor resets to 4 so it's excluded.
			AddRow("acore_auth", "account", "idx_x", int64(1), "BTREE", int64(3), "a,b,c"))

	reg := NewRegistry()
	RegisterDBWidestCompositeIndexesTool(reg, DBDeps{QueryDB: db})
	tool, _ := reg.Get("db_widest_composite_indexes")
	resp := tool.Handler(context.Background(), json.RawMessage(`{"database":"acore_auth","minColumns":1}`), "")
	got := resp.(map[string]any)
	if got["minColumns"].(int) != 4 {
		t.Errorf("minColumns <2 should reset to default 4, got %v", got["minColumns"])
	}
	rows := got["rows"].([]wideIndexRow)
	if len(rows) != 0 {
		t.Errorf("3-col index should be excluded at reset floor 4: %+v", rows)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestDBWidestCompositeIndexes_TopNClampAndTruncate verifies topN caps the
// returned rows (truncated=true) while totals.wideCount stays honest, and that
// a topN above the hard cap clamps to 50.
func TestDBWidestCompositeIndexes_TopNClampAndTruncate(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectQuery(regexp.QuoteMeta("GROUP_CONCAT(s.COLUMN_NAME ORDER BY s.SEQ_IN_INDEX")).
		WithArgs("acore_world").
		WillReturnRows(sqlmock.NewRows(wideIndexRowCols).
			AddRow("acore_world", "t1", "idx_a", int64(1), "BTREE", int64(6), "a,b,c,d,e,f").
			AddRow("acore_world", "t2", "idx_b", int64(1), "BTREE", int64(5), "a,b,c,d,e").
			AddRow("acore_world", "t3", "idx_c", int64(1), "BTREE", int64(4), "a,b,c,d"))

	reg := NewRegistry()
	RegisterDBWidestCompositeIndexesTool(reg, DBDeps{QueryDB: db})
	tool, _ := reg.Get("db_widest_composite_indexes")
	resp := tool.Handler(context.Background(), json.RawMessage(`{"database":"acore_world","topN":2}`), "")
	got := resp.(map[string]any)
	if got["topN"].(int) != 2 {
		t.Errorf("topN should echo 2, got %v", got["topN"])
	}
	rows := got["rows"].([]wideIndexRow)
	if len(rows) != 2 {
		t.Fatalf("expected 2 rows after topN cap, got %d", len(rows))
	}
	if !got["truncated"].(bool) {
		t.Errorf("should be truncated (3 candidates, topN 2)")
	}
	// Honest total survives truncation.
	if got["totals"].(map[string]any)["wideCount"].(int) != 3 {
		t.Errorf("wideCount should stay 3 despite truncation: %v", got["totals"])
	}
}

// TestDBWidestCompositeIndexes_TopNClampsToMax verifies a topN above the hard
// cap of 50 clamps to 50.
func TestDBWidestCompositeIndexes_TopNClampsToMax(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectQuery(regexp.QuoteMeta("GROUP_CONCAT(s.COLUMN_NAME ORDER BY s.SEQ_IN_INDEX")).
		WithArgs("acore_world", "acore_characters", "acore_auth").
		WillReturnRows(sqlmock.NewRows(wideIndexRowCols))

	reg := NewRegistry()
	RegisterDBWidestCompositeIndexesTool(reg, DBDeps{QueryDB: db})
	tool, _ := reg.Get("db_widest_composite_indexes")
	resp := tool.Handler(context.Background(), json.RawMessage(`{"topN":99999}`), "")
	got := resp.(map[string]any)
	if got["topN"].(int) != 50 {
		t.Errorf("topN should clamp to 50, got %v", got["topN"])
	}
}

// TestDBWidestCompositeIndexes_EmptyResultNonNil verifies an all-below-floor
// scan returns a non-nil empty rows slice (JSON []) and a pre-seeded
// perDatabase with zero counts, so the caller can distinguish "nothing wide"
// from "schema filter matched nothing".
func TestDBWidestCompositeIndexes_EmptyResultNonNil(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectQuery(regexp.QuoteMeta("GROUP_CONCAT(s.COLUMN_NAME ORDER BY s.SEQ_IN_INDEX")).
		WithArgs("acore_world", "acore_characters", "acore_auth").
		WillReturnRows(sqlmock.NewRows(wideIndexRowCols).
			AddRow("acore_world", "creature", "idx_a", int64(1), "BTREE", int64(2), "a,b"))

	reg := NewRegistry()
	RegisterDBWidestCompositeIndexesTool(reg, DBDeps{QueryDB: db})
	tool, _ := reg.Get("db_widest_composite_indexes")
	resp := tool.Handler(context.Background(), json.RawMessage(`{}`), "")
	got := resp.(map[string]any)
	rows, ok := got["rows"].([]wideIndexRow)
	if !ok {
		t.Fatalf("rows should be a []wideIndexRow, got %T", got["rows"])
	}
	if rows == nil {
		t.Errorf("rows should be non-nil empty slice for JSON []")
	}
	if len(rows) != 0 {
		t.Errorf("rows should be empty, got %+v", rows)
	}
	perDB := got["perDatabase"].([]wideIndexPerDatabase)
	if len(perDB) != 3 {
		t.Errorf("perDatabase should be pre-seeded for all three schemas, got %d", len(perDB))
	}
}

// TestClassifyIndexWidth pins the pure severity banding: high >= 6, medium >= 4,
// low below 4. Asserted in isolation so a banding regression is caught without a
// DB round trip.
func TestClassifyIndexWidth(t *testing.T) {
	cases := []struct {
		columns int
		want    string
	}{
		{7, "high"},
		{6, "high"},
		{5, "medium"},
		{4, "medium"},
		{3, "low"},
		{2, "low"},
	}
	for _, c := range cases {
		sev, reason := classifyIndexWidth(c.columns)
		if sev != c.want {
			t.Errorf("classifyIndexWidth(%d) severity = %q, want %q", c.columns, sev, c.want)
		}
		if reason == "" {
			t.Errorf("classifyIndexWidth(%d) reason should be non-empty", c.columns)
		}
	}
	// Severity rank orders the widest-first sort.
	if wideIndexSeverityRank("high") >= wideIndexSeverityRank("medium") ||
		wideIndexSeverityRank("medium") >= wideIndexSeverityRank("low") ||
		wideIndexSeverityRank("low") >= wideIndexSeverityRank("bogus") {
		t.Errorf("severity rank should order high<medium<low<unknown")
	}
}
