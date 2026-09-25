package mcpserver

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// TestDBOverIndexedTables_DescriptionMentionsContext anchors the description to
// the keywords the agent uses to pick this tool. Same shape guard as the other
// db_* tools.
func TestDBOverIndexedTables_DescriptionMentionsContext(t *testing.T) {
	reg := NewRegistry()
	RegisterDBOverIndexedTablesTool(reg, DBDeps{})
	tool, ok := reg.Get("db_over_indexed_tables")
	if !ok {
		t.Fatal("db_over_indexed_tables not registered")
	}
	for _, kw := range []string{
		"secondary", "write amplification", "PRIMARY KEY",
		"information_schema.STATISTICS", "information_schema.TABLES",
		"candidates", "perDatabase", "severity", "secondaryIndexCount",
		"indexToDataPercent", "minIndexes",
		"db_unindexed_tables", "db_redundant_indexes", "db_low_cardinality_indexes",
		"acore_playerbots",
	} {
		if !strings.Contains(tool.Description, kw) {
			t.Errorf("description missing %q", kw)
		}
	}
}

func TestDBOverIndexedTables_ReadOnlyAnnotation(t *testing.T) {
	reg := NewRegistry()
	RegisterDBOverIndexedTablesTool(reg, DBDeps{})
	tool, _ := reg.Get("db_over_indexed_tables")
	if !strings.Contains(string(tool.Annotations), `"readOnlyHint":true`) {
		t.Errorf("expected readOnlyHint:true, got %s", tool.Annotations)
	}
	if tool.Destructive {
		t.Errorf("destructive should be false")
	}
}

func TestDBOverIndexedTables_NoPoolReturnsError(t *testing.T) {
	reg := NewRegistry()
	RegisterDBOverIndexedTablesTool(reg, DBDeps{})
	tool, _ := reg.Get("db_over_indexed_tables")
	resp := tool.Handler(context.Background(), json.RawMessage(`{}`), "")
	got, _ := resp.(map[string]any)
	if msg, _ := got["error"].(string); !strings.Contains(msg, "ops_ro pool not configured") {
		t.Errorf("expected pool-not-configured error, got %v", got)
	}
}

func TestDBOverIndexedTables_InvalidDatabaseRejected(t *testing.T) {
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	reg := NewRegistry()
	RegisterDBOverIndexedTablesTool(reg, DBDeps{QueryDB: db})
	tool, _ := reg.Get("db_over_indexed_tables")
	// acore_playerbots is intentionally NOT in the allowlist — ops_ro lacks the
	// grant, so it would scan nothing yet look like a clean result.
	resp := tool.Handler(context.Background(), json.RawMessage(`{"database":"acore_playerbots"}`), "")
	got, _ := resp.(map[string]any)
	if msg, _ := got["error"].(string); !strings.Contains(msg, "acore_world/acore_characters/acore_auth") {
		t.Errorf("expected allowlist error, got %v", got)
	}
}

// overIndexedRowCols is the column set the join SELECT returns, in order.
var overIndexedRowCols = []string{"TABLE_SCHEMA", "TABLE_NAME", "ENGINE", "TABLE_ROWS", "INDEX_LENGTH", "DATA_LENGTH", "INDEX_COUNT", "SECONDARY_COUNT"}

// TestDBOverIndexedTables_GoldenAggregation drives the default path (all three
// schemas, default minIndexes=5). Fixture mixes below-threshold tables (skipped
// but counted in the denominator), one high (8 secondary), and two medium (6
// and 5 secondary). Verifies: below-threshold filtered out, severity banding,
// worst-first sort (high before medium, then secondaryIndexCount desc within a
// band), indexToDataPercent (incl. the -1 no-data case), human bytes,
// perDatabase counts incl. denominator, totals.
func TestDBOverIndexedTables_GoldenAggregation(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	const (
		KiB = 1024
		MiB = 1024 * 1024
	)
	mock.ExpectQuery(regexp.QuoteMeta("COUNT(DISTINCT CASE WHEN s.INDEX_NAME")).
		WithArgs("acore_world", "acore_characters", "acore_auth").
		WillReturnRows(sqlmock.NewRows(overIndexedRowCols).
			// 2 secondary indexes — below default threshold, skipped but counted.
			AddRow("acore_world", "creature_template", "InnoDB", int64(50000), int64(4*MiB), int64(8*MiB), int64(3), int64(2)).
			// 8 secondary → HIGH. indexToDataPercent = 6MiB*100/3MiB = 200.
			AddRow("acore_characters", "characters", "InnoDB", int64(500), int64(6*MiB), int64(3*MiB), int64(9), int64(8)).
			// 6 secondary → MEDIUM. pct = 1MiB*100/4MiB = 25.
			AddRow("acore_characters", "item_instance", "InnoDB", int64(2000), int64(1*MiB), int64(4*MiB), int64(7), int64(6)).
			// 5 secondary → MEDIUM, DATA_LENGTH 0 → pct = -1.
			AddRow("acore_characters", "mail", "InnoDB", int64(100), int64(512*KiB), int64(0), int64(6), int64(5)).
			// 3 secondary — below threshold, skipped but counted.
			AddRow("acore_auth", "account", "InnoDB", int64(10), int64(8*KiB), int64(16*KiB), int64(4), int64(3)))

	reg := NewRegistry()
	RegisterDBOverIndexedTablesTool(reg, DBDeps{QueryDB: db})
	tool, _ := reg.Get("db_over_indexed_tables")
	resp := tool.Handler(context.Background(), json.RawMessage(`{}`), "")
	got, ok := resp.(map[string]any)
	if !ok {
		t.Fatalf("unexpected response shape: %T", resp)
	}
	if msg, hasErr := got["error"]; hasErr {
		t.Fatalf("unexpected error: %v", msg)
	}

	cands := got["candidates"].([]overIndexedTableRow)
	if len(cands) != 3 {
		t.Fatalf("candidates len: %d (%+v)", len(cands), cands)
	}
	// Worst-first: HIGH first, then the two MEDIUMs by secondaryIndexCount desc.
	if cands[0].Table != "characters" || cands[0].Severity != "high" || cands[0].SecondaryIndexCount != 8 || cands[0].IndexCount != 9 {
		t.Errorf("candidates[0] should be the high characters (8 secondary): %+v", cands[0])
	}
	if cands[0].IndexToDataPercent != 200 {
		t.Errorf("characters indexToDataPercent should be 200, got %d", cands[0].IndexToDataPercent)
	}
	if cands[0].IndexSizeHuman != "6.00 MiB" || cands[0].DataSizeHuman != "3.00 MiB" {
		t.Errorf("human bytes wrong: idx=%q data=%q", cands[0].IndexSizeHuman, cands[0].DataSizeHuman)
	}
	if cands[1].Table != "item_instance" || cands[1].Severity != "medium" || cands[1].SecondaryIndexCount != 6 {
		t.Errorf("candidates[1] should be the medium item_instance (6 secondary): %+v", cands[1])
	}
	if cands[1].IndexToDataPercent != 25 {
		t.Errorf("item_instance indexToDataPercent should be 25, got %d", cands[1].IndexToDataPercent)
	}
	if cands[2].Table != "mail" || cands[2].Severity != "medium" || cands[2].SecondaryIndexCount != 5 {
		t.Errorf("candidates[2] should be the medium mail (5 secondary): %+v", cands[2])
	}
	if cands[2].IndexToDataPercent != -1 {
		t.Errorf("mail (no data) indexToDataPercent should be -1, got %d", cands[2].IndexToDataPercent)
	}
	if !strings.Contains(cands[0].Reason, "write amplification") {
		t.Errorf("high reason should mention write amplification: %q", cands[0].Reason)
	}

	perDB := got["perDatabase"].([]overIndexedPerDatabase)
	if len(perDB) != 3 {
		t.Fatalf("perDatabase len: %d", len(perDB))
	}
	// Sorted overIndexedCount desc, schema-asc tiebreak: characters(3) then
	// auth(0) before world(0).
	if perDB[0].Database != "acore_characters" || perDB[0].OverIndexedCount != 3 || perDB[0].TablesScanned != 3 {
		t.Errorf("perDB[0]: %+v", perDB[0])
	}
	if perDB[1].Database != "acore_auth" || perDB[1].OverIndexedCount != 0 || perDB[1].TablesScanned != 1 {
		t.Errorf("perDB[1] (acore_auth, clean denominator-only): %+v", perDB[1])
	}
	if perDB[2].Database != "acore_world" || perDB[2].OverIndexedCount != 0 || perDB[2].TablesScanned != 1 {
		t.Errorf("perDB[2] (acore_world, clean denominator-only): %+v", perDB[2])
	}

	totals := got["totals"].(map[string]any)
	if totals["tablesScanned"].(int) != 5 {
		t.Errorf("totals.tablesScanned: %v", totals["tablesScanned"])
	}
	if totals["overIndexedCount"].(int) != 3 {
		t.Errorf("totals.overIndexedCount: %v", totals["overIndexedCount"])
	}
	if got["truncated"].(bool) {
		t.Errorf("should not be truncated for 3 candidates")
	}
	if got["minIndexes"].(int) != 5 {
		t.Errorf("minIndexes should default to 5, got %v", got["minIndexes"])
	}
	if got["asOf"].(string) == "" {
		t.Errorf("asOf should be populated")
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestDBOverIndexedTables_DatabaseFilterNarrowsScan verifies passing `database`
// narrows the IN clause to a single bind (asserted via WithArgs).
func TestDBOverIndexedTables_DatabaseFilterNarrowsScan(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectQuery(regexp.QuoteMeta("COUNT(DISTINCT CASE WHEN s.INDEX_NAME")).
		WithArgs("acore_characters"). // single bind — IN list narrowed to one schema
		WillReturnRows(sqlmock.NewRows(overIndexedRowCols).
			AddRow("acore_characters", "characters", "InnoDB", int64(42), int64(8192), int64(4096), int64(10), int64(9)))

	reg := NewRegistry()
	RegisterDBOverIndexedTablesTool(reg, DBDeps{QueryDB: db})
	tool, _ := reg.Get("db_over_indexed_tables")
	resp := tool.Handler(context.Background(), json.RawMessage(`{"database":"acore_characters"}`), "")
	got := resp.(map[string]any)
	scanned := got["scannedSchemas"].([]string)
	if len(scanned) != 1 || scanned[0] != "acore_characters" {
		t.Errorf("scannedSchemas: %v", scanned)
	}
	cands := got["candidates"].([]overIndexedTableRow)
	if len(cands) != 1 || cands[0].Table != "characters" || cands[0].Severity != "high" || cands[0].SecondaryIndexCount != 9 {
		t.Errorf("candidates: %+v", cands)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestDBOverIndexedTables_MinIndexesLowersFloor verifies lowering minIndexes
// surfaces the `low` band (< 5 secondary) that the default suppresses, echoes
// the threshold, and that a below-floor table is still excluded.
func TestDBOverIndexedTables_MinIndexesLowersFloor(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectQuery(regexp.QuoteMeta("COUNT(DISTINCT CASE WHEN s.INDEX_NAME")).
		WithArgs("acore_world").
		WillReturnRows(sqlmock.NewRows(overIndexedRowCols).
			// 6 secondary → medium (reported at any minIndexes <= 6).
			AddRow("acore_world", "gameobject", "InnoDB", int64(9000), int64(2048), int64(4096), int64(7), int64(6)).
			// 4 secondary → low band, surfaced only because minIndexes lowered to 3.
			AddRow("acore_world", "quest_template", "InnoDB", int64(8000), int64(1024), int64(2048), int64(5), int64(4)).
			// 2 secondary → still below the lowered floor of 3, excluded.
			AddRow("acore_world", "spell_dbc", "InnoDB", int64(1000), int64(512), int64(1024), int64(3), int64(2)))

	reg := NewRegistry()
	RegisterDBOverIndexedTablesTool(reg, DBDeps{QueryDB: db})
	tool, _ := reg.Get("db_over_indexed_tables")
	resp := tool.Handler(context.Background(), json.RawMessage(`{"database":"acore_world","minIndexes":3}`), "")
	got := resp.(map[string]any)
	if got["minIndexes"].(int) != 3 {
		t.Errorf("minIndexes should echo 3, got %v", got["minIndexes"])
	}
	cands := got["candidates"].([]overIndexedTableRow)
	if len(cands) != 2 {
		t.Fatalf("expected 2 candidates (6 and 4 secondary), got %+v", cands)
	}
	if cands[0].Table != "gameobject" || cands[0].Severity != "medium" {
		t.Errorf("candidates[0] should be the medium gameobject: %+v", cands[0])
	}
	if cands[1].Table != "quest_template" || cands[1].Severity != "low" || cands[1].SecondaryIndexCount != 4 {
		t.Errorf("candidates[1] should be the low-band quest_template (4 secondary): %+v", cands[1])
	}
	if !strings.Contains(cands[1].Reason, "minIndexes was lowered") {
		t.Errorf("low reason should explain the lowered threshold: %q", cands[1].Reason)
	}
	if got["totals"].(map[string]any)["tablesScanned"].(int) != 3 {
		t.Errorf("all three rows count toward the denominator: %v", got["totals"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestDBOverIndexedTables_MinIndexesResetsOnNonPositive verifies a <1 minIndexes
// (which would otherwise flag every table) resets to the default of 5.
func TestDBOverIndexedTables_MinIndexesResetsOnNonPositive(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectQuery(regexp.QuoteMeta("COUNT(DISTINCT CASE WHEN s.INDEX_NAME")).
		WithArgs("acore_world").
		WillReturnRows(sqlmock.NewRows(overIndexedRowCols).
			AddRow("acore_world", "few_idx", "InnoDB", int64(1), int64(0), int64(0), int64(3), int64(2)))

	reg := NewRegistry()
	RegisterDBOverIndexedTablesTool(reg, DBDeps{QueryDB: db})
	tool, _ := reg.Get("db_over_indexed_tables")
	resp := tool.Handler(context.Background(), json.RawMessage(`{"database":"acore_world","minIndexes":0}`), "")
	got := resp.(map[string]any)
	if got["minIndexes"].(int) != 5 {
		t.Errorf("minIndexes <1 should reset to the default 5, got %v", got["minIndexes"])
	}
	cands := got["candidates"].([]overIndexedTableRow)
	if len(cands) != 0 {
		t.Errorf("2-secondary table should not be flagged under the reset default: %+v", cands)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestDBOverIndexedTables_AllUnderThresholdEmptyResult — every table is below
// the threshold, so candidates is an empty (but non-nil) slice and every
// scanned schema still surfaces in perDatabase with a clean rollup.
func TestDBOverIndexedTables_AllUnderThresholdEmptyResult(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectQuery(regexp.QuoteMeta("COUNT(DISTINCT CASE WHEN s.INDEX_NAME")).
		WithArgs("acore_world", "acore_characters", "acore_auth").
		WillReturnRows(sqlmock.NewRows(overIndexedRowCols).
			AddRow("acore_world", "creature_template", "InnoDB", int64(50000), int64(2048), int64(4096), int64(3), int64(2)).
			AddRow("acore_auth", "account", "InnoDB", int64(10), int64(1024), int64(2048), int64(2), int64(1)))

	reg := NewRegistry()
	RegisterDBOverIndexedTablesTool(reg, DBDeps{QueryDB: db})
	tool, _ := reg.Get("db_over_indexed_tables")
	resp := tool.Handler(context.Background(), json.RawMessage(`{}`), "")
	got := resp.(map[string]any)
	cands := got["candidates"].([]overIndexedTableRow)
	if cands == nil {
		t.Fatal("candidates should be a non-nil empty slice, not nil")
	}
	if len(cands) != 0 {
		t.Fatalf("expected no candidates, got %+v", cands)
	}
	perDB := got["perDatabase"].([]overIndexedPerDatabase)
	if len(perDB) != 3 {
		t.Fatalf("expected all 3 schemas surfaced, got %d", len(perDB))
	}
	if got["totals"].(map[string]any)["overIndexedCount"].(int) != 0 {
		t.Errorf("totals.overIndexedCount should be 0: %v", got["totals"])
	}
	if got["totals"].(map[string]any)["tablesScanned"].(int) != 2 {
		t.Errorf("totals.tablesScanned: %v", got["totals"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestDBOverIndexedTables_TruncationCapsCandidates — more than the hard cap of
// over-indexed tables: candidates is sliced to the cap with truncated=true, but
// totals.overIndexedCount stays honest (the full pre-truncation count).
func TestDBOverIndexedTables_TruncationCapsCandidates(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	rs := sqlmock.NewRows(overIndexedRowCols)
	const n = dbOverIndexedTablesMaxCandidates + 1
	for i := 0; i < n; i++ {
		// 5 secondary each → all medium, all above the default floor.
		rs.AddRow("acore_world", "t_"+itoaSmall(i), "InnoDB", int64(i), int64(4096), int64(4096), int64(6), int64(5))
	}
	mock.ExpectQuery(regexp.QuoteMeta("COUNT(DISTINCT CASE WHEN s.INDEX_NAME")).
		WithArgs("acore_world").
		WillReturnRows(rs)

	reg := NewRegistry()
	RegisterDBOverIndexedTablesTool(reg, DBDeps{QueryDB: db})
	tool, _ := reg.Get("db_over_indexed_tables")
	resp := tool.Handler(context.Background(), json.RawMessage(`{"database":"acore_world"}`), "")
	got := resp.(map[string]any)
	cands := got["candidates"].([]overIndexedTableRow)
	if len(cands) != dbOverIndexedTablesMaxCandidates {
		t.Errorf("candidates should be capped at %d, got %d", dbOverIndexedTablesMaxCandidates, len(cands))
	}
	if !got["truncated"].(bool) {
		t.Errorf("truncated flag should be set")
	}
	if got["totals"].(map[string]any)["overIndexedCount"].(int) != n {
		t.Errorf("totals.overIndexedCount should be the honest pre-cap count %d, got %v", n, got["totals"].(map[string]any)["overIndexedCount"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestClassifyOverIndexed unit-tests the pure severity/reason banding at and
// around the boundaries (8 → high, 5-7 → medium, <5 → low).
func TestClassifyOverIndexed(t *testing.T) {
	cases := []struct {
		secondary int
		want      string
	}{
		{12, "high"},
		{8, "high"},
		{7, "medium"},
		{5, "medium"},
		{4, "low"},
		{0, "low"},
	}
	for _, c := range cases {
		sev, reason := classifyOverIndexed(c.secondary)
		if sev != c.want {
			t.Errorf("classifyOverIndexed(%d) = %q, want %q", c.secondary, sev, c.want)
		}
		if reason == "" {
			t.Errorf("classifyOverIndexed(%d) empty reason", c.secondary)
		}
	}
}

// TestOverIndexedSeverityRank guards the sort ordering: high < medium < low <
// unknown.
func TestOverIndexedSeverityRank(t *testing.T) {
	if overIndexedSeverityRank("high") >= overIndexedSeverityRank("medium") {
		t.Error("high should rank before medium")
	}
	if overIndexedSeverityRank("medium") >= overIndexedSeverityRank("low") {
		t.Error("medium should rank before low")
	}
	if overIndexedSeverityRank("low") >= overIndexedSeverityRank("other") {
		t.Error("low should rank before unknown severities")
	}
}
