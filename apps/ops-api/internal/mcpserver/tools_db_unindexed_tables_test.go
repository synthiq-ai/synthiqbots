package mcpserver

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// TestDBUnindexedTables_DescriptionMentionsContext anchors the description to
// the keywords the agent uses to pick this tool. Same shape guard as the other
// db_* tools.
func TestDBUnindexedTables_DescriptionMentionsContext(t *testing.T) {
	reg := NewRegistry()
	RegisterDBUnindexedTablesTool(reg, DBDeps{})
	tool, ok := reg.Get("db_unindexed_tables")
	if !ok {
		t.Fatal("db_unindexed_tables not registered")
	}
	for _, kw := range []string{
		"PRIMARY KEY", "UNIQUE", "GEN_CLUST_INDEX", "information_schema.STATISTICS",
		"information_schema.TABLES", "candidates", "perDatabase", "severity",
		"db_redundant_indexes", "db_size_summary", "acore_playerbots", "minRows", "rowsEstimate",
	} {
		if !strings.Contains(tool.Description, kw) {
			t.Errorf("description missing %q", kw)
		}
	}
}

func TestDBUnindexedTables_ReadOnlyAnnotation(t *testing.T) {
	reg := NewRegistry()
	RegisterDBUnindexedTablesTool(reg, DBDeps{})
	tool, _ := reg.Get("db_unindexed_tables")
	if !strings.Contains(string(tool.Annotations), `"readOnlyHint":true`) {
		t.Errorf("expected readOnlyHint:true, got %s", tool.Annotations)
	}
	if tool.Destructive {
		t.Errorf("destructive should be false")
	}
}

func TestDBUnindexedTables_NoPoolReturnsError(t *testing.T) {
	reg := NewRegistry()
	RegisterDBUnindexedTablesTool(reg, DBDeps{})
	tool, _ := reg.Get("db_unindexed_tables")
	resp := tool.Handler(context.Background(), json.RawMessage(`{}`), "")
	got, _ := resp.(map[string]any)
	if msg, _ := got["error"].(string); !strings.Contains(msg, "ops_ro pool not configured") {
		t.Errorf("expected pool-not-configured error, got %v", got)
	}
}

func TestDBUnindexedTables_InvalidDatabaseRejected(t *testing.T) {
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	reg := NewRegistry()
	RegisterDBUnindexedTablesTool(reg, DBDeps{QueryDB: db})
	tool, _ := reg.Get("db_unindexed_tables")
	// acore_playerbots is intentionally NOT in the allowlist — ops_ro lacks the
	// grant, so it would scan nothing yet look like a clean result.
	resp := tool.Handler(context.Background(), json.RawMessage(`{"database":"acore_playerbots"}`), "")
	got, _ := resp.(map[string]any)
	if msg, _ := got["error"].(string); !strings.Contains(msg, "acore_world/acore_characters/acore_auth") {
		t.Errorf("expected allowlist error, got %v", got)
	}
}

// unindexedRowCols is the column set the join SELECT returns, in order.
var unindexedRowCols = []string{"TABLE_SCHEMA", "TABLE_NAME", "ENGINE", "TABLE_ROWS", "TOTAL_BYTES", "HAS_PRIMARY", "HAS_UNIQUE"}

// TestDBUnindexedTables_GoldenAggregation drives the default path (all three
// schemas, minRows=0). Fixture mixes PK'd tables (skipped), no-PK+no-UNIQUE
// (high) and no-PK+UNIQUE (medium). Verifies: PK tables filtered out, severity
// classification, worst-first sort (high before medium, rowsEstimate desc
// within a severity), perDatabase counts incl. denominator, totals, human bytes.
func TestDBUnindexedTables_GoldenAggregation(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	const (
		KiB = 1024
		MiB = 1024 * 1024
	)
	mock.ExpectQuery(regexp.QuoteMeta("LEFT JOIN information_schema.STATISTICS")).
		WithArgs("acore_world", "acore_characters", "acore_auth").
		WillReturnRows(sqlmock.NewRows(unindexedRowCols).
			// has PRIMARY — skipped, but counted in TablesScanned.
			AddRow("acore_world", "creature_template", "InnoDB", int64(50000), int64(2*MiB), int64(1), int64(0)).
			// no PK, no UNIQUE → HIGH (small).
			AddRow("acore_world", "custom_loot_staging", "InnoDB", int64(1000), int64(64*KiB), int64(0), int64(0)).
			// no PK, no UNIQUE → HIGH (large — sorts above the small high one).
			AddRow("acore_characters", "log_blob", "InnoDB", int64(500000), int64(8*MiB), int64(0), int64(0)).
			// no PK, UNIQUE exists → MEDIUM.
			AddRow("acore_characters", "item_alias", "InnoDB", int64(200), int64(16*KiB), int64(0), int64(1)).
			// has PRIMARY — skipped.
			AddRow("acore_auth", "account", "InnoDB", int64(10), int64(4*KiB), int64(1), int64(0)))

	reg := NewRegistry()
	RegisterDBUnindexedTablesTool(reg, DBDeps{QueryDB: db})
	tool, _ := reg.Get("db_unindexed_tables")
	resp := tool.Handler(context.Background(), json.RawMessage(`{}`), "")
	got, ok := resp.(map[string]any)
	if !ok {
		t.Fatalf("unexpected response shape: %T", resp)
	}
	if msg, hasErr := got["error"]; hasErr {
		t.Fatalf("unexpected error: %v", msg)
	}

	cands := got["candidates"].([]unindexedTableRow)
	if len(cands) != 3 {
		t.Fatalf("candidates len: %d (%+v)", len(cands), cands)
	}
	// Worst-first: both HIGH before the MEDIUM; within HIGH, rowsEstimate desc.
	if cands[0].Table != "log_blob" || cands[0].Severity != "high" || cands[0].HasUniqueKey {
		t.Errorf("candidates[0] should be the big high log_blob: %+v", cands[0])
	}
	if cands[0].TotalSizeHuman != "8.00 MiB" {
		t.Errorf("human bytes wrong: %q", cands[0].TotalSizeHuman)
	}
	if cands[1].Table != "custom_loot_staging" || cands[1].Severity != "high" {
		t.Errorf("candidates[1] should be the small high staging table: %+v", cands[1])
	}
	if cands[2].Table != "item_alias" || cands[2].Severity != "medium" || !cands[2].HasUniqueKey {
		t.Errorf("candidates[2] should be the medium item_alias: %+v", cands[2])
	}
	if !strings.Contains(cands[2].Reason, "UNIQUE index exists") {
		t.Errorf("medium reason should mention the UNIQUE-index promotion: %q", cands[2].Reason)
	}

	perDB := got["perDatabase"].([]unindexedPerDatabase)
	if len(perDB) != 3 {
		t.Fatalf("perDatabase len: %d", len(perDB))
	}
	// Sorted unindexedCount desc: characters(2) > world(1) > auth(0).
	if perDB[0].Database != "acore_characters" || perDB[0].UnindexedCount != 2 || perDB[0].TablesScanned != 2 {
		t.Errorf("perDB[0]: %+v", perDB[0])
	}
	if perDB[1].Database != "acore_world" || perDB[1].UnindexedCount != 1 || perDB[1].TablesScanned != 2 {
		t.Errorf("perDB[1]: %+v", perDB[1])
	}
	if perDB[2].Database != "acore_auth" || perDB[2].UnindexedCount != 0 || perDB[2].TablesScanned != 1 {
		t.Errorf("perDB[2] (acore_auth) should be a clean denominator-only entry: %+v", perDB[2])
	}

	totals := got["totals"].(map[string]any)
	if totals["tablesScanned"].(int) != 5 {
		t.Errorf("totals.tablesScanned: %v", totals["tablesScanned"])
	}
	if totals["unindexedCount"].(int) != 3 {
		t.Errorf("totals.unindexedCount: %v", totals["unindexedCount"])
	}
	if got["truncated"].(bool) {
		t.Errorf("should not be truncated for 3 candidates")
	}
	if got["asOf"].(string) == "" {
		t.Errorf("asOf should be populated")
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestDBUnindexedTables_DatabaseFilterNarrowsScan verifies passing `database`
// narrows the IN clause to a single bind (asserted via WithArgs).
func TestDBUnindexedTables_DatabaseFilterNarrowsScan(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectQuery(regexp.QuoteMeta("LEFT JOIN information_schema.STATISTICS")).
		WithArgs("acore_world"). // single bind — IN list narrowed to one schema
		WillReturnRows(sqlmock.NewRows(unindexedRowCols).
			AddRow("acore_world", "orphan_tmp", "InnoDB", int64(42), int64(8192), int64(0), int64(0)))

	reg := NewRegistry()
	RegisterDBUnindexedTablesTool(reg, DBDeps{QueryDB: db})
	tool, _ := reg.Get("db_unindexed_tables")
	resp := tool.Handler(context.Background(), json.RawMessage(`{"database":"acore_world"}`), "")
	got := resp.(map[string]any)
	scanned := got["scannedSchemas"].([]string)
	if len(scanned) != 1 || scanned[0] != "acore_world" {
		t.Errorf("scannedSchemas: %v", scanned)
	}
	cands := got["candidates"].([]unindexedTableRow)
	if len(cands) != 1 || cands[0].Table != "orphan_tmp" || cands[0].Severity != "high" {
		t.Errorf("candidates: %+v", cands)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestDBUnindexedTables_MinRowsSuppresses verifies a no-PK table below the
// minRows threshold is dropped from candidates AND from unindexedCount, while
// still counting toward tablesScanned (the denominator is the full set).
func TestDBUnindexedTables_MinRowsSuppresses(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectQuery(regexp.QuoteMeta("LEFT JOIN information_schema.STATISTICS")).
		WithArgs("acore_world").
		WillReturnRows(sqlmock.NewRows(unindexedRowCols).
			AddRow("acore_world", "big_unindexed", "InnoDB", int64(100000), int64(4096), int64(0), int64(0)).
			AddRow("acore_world", "tiny_unindexed", "InnoDB", int64(5), int64(4096), int64(0), int64(0)))

	reg := NewRegistry()
	RegisterDBUnindexedTablesTool(reg, DBDeps{QueryDB: db})
	tool, _ := reg.Get("db_unindexed_tables")
	resp := tool.Handler(context.Background(), json.RawMessage(`{"database":"acore_world","minRows":1000}`), "")
	got := resp.(map[string]any)
	cands := got["candidates"].([]unindexedTableRow)
	if len(cands) != 1 || cands[0].Table != "big_unindexed" {
		t.Fatalf("minRows should suppress tiny_unindexed: %+v", cands)
	}
	if got["minRows"].(int64) != 1000 {
		t.Errorf("minRows not echoed: %v", got["minRows"])
	}
	perDB := got["perDatabase"].([]unindexedPerDatabase)
	if perDB[0].TablesScanned != 2 {
		t.Errorf("tablesScanned should count both (denominator is unfiltered): %+v", perDB[0])
	}
	if perDB[0].UnindexedCount != 1 {
		t.Errorf("unindexedCount should reflect the post-filter candidate: %+v", perDB[0])
	}
	if got["totals"].(map[string]any)["unindexedCount"].(int) != 1 {
		t.Errorf("totals.unindexedCount: %v", got["totals"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestDBUnindexedTables_AllIndexedEmptyResult — every table has a PK, so
// candidates is an empty (but non-nil) slice and every scanned schema still
// surfaces in perDatabase with a clean rollup.
func TestDBUnindexedTables_AllIndexedEmptyResult(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectQuery(regexp.QuoteMeta("LEFT JOIN information_schema.STATISTICS")).
		WithArgs("acore_world", "acore_characters", "acore_auth").
		WillReturnRows(sqlmock.NewRows(unindexedRowCols).
			AddRow("acore_world", "creature_template", "InnoDB", int64(50000), int64(2048), int64(1), int64(0)).
			AddRow("acore_auth", "account", "InnoDB", int64(10), int64(1024), int64(1), int64(0)))

	reg := NewRegistry()
	RegisterDBUnindexedTablesTool(reg, DBDeps{QueryDB: db})
	tool, _ := reg.Get("db_unindexed_tables")
	resp := tool.Handler(context.Background(), json.RawMessage(`{}`), "")
	got := resp.(map[string]any)
	cands := got["candidates"].([]unindexedTableRow)
	if cands == nil {
		t.Fatal("candidates should be a non-nil empty slice, not nil")
	}
	if len(cands) != 0 {
		t.Fatalf("expected no candidates, got %+v", cands)
	}
	perDB := got["perDatabase"].([]unindexedPerDatabase)
	if len(perDB) != 3 {
		t.Fatalf("expected all 3 schemas surfaced, got %d", len(perDB))
	}
	if got["totals"].(map[string]any)["unindexedCount"].(int) != 0 {
		t.Errorf("totals.unindexedCount should be 0: %v", got["totals"])
	}
	if got["totals"].(map[string]any)["tablesScanned"].(int) != 2 {
		t.Errorf("totals.tablesScanned: %v", got["totals"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestDBUnindexedTables_TruncationCapsCandidates — more than the hard cap of
// no-PK tables: candidates is sliced to the cap with truncated=true, but
// totals.unindexedCount stays honest (the full pre-truncation count).
func TestDBUnindexedTables_TruncationCapsCandidates(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	rs := sqlmock.NewRows(unindexedRowCols)
	const n = dbUnindexedTablesMaxCandidates + 1
	for i := 0; i < n; i++ {
		rs.AddRow("acore_world", "t_"+itoaSmall(i), "InnoDB", int64(i), int64(4096), int64(0), int64(0))
	}
	mock.ExpectQuery(regexp.QuoteMeta("LEFT JOIN information_schema.STATISTICS")).
		WithArgs("acore_world").
		WillReturnRows(rs)

	reg := NewRegistry()
	RegisterDBUnindexedTablesTool(reg, DBDeps{QueryDB: db})
	tool, _ := reg.Get("db_unindexed_tables")
	resp := tool.Handler(context.Background(), json.RawMessage(`{"database":"acore_world"}`), "")
	got := resp.(map[string]any)
	cands := got["candidates"].([]unindexedTableRow)
	if len(cands) != dbUnindexedTablesMaxCandidates {
		t.Errorf("candidates should be capped at %d, got %d", dbUnindexedTablesMaxCandidates, len(cands))
	}
	if !got["truncated"].(bool) {
		t.Errorf("truncated flag should be set")
	}
	if got["totals"].(map[string]any)["unindexedCount"].(int) != n {
		t.Errorf("totals.unindexedCount should be the honest pre-cap count %d, got %v", n, got["totals"].(map[string]any)["unindexedCount"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestClassifyUnindexed unit-tests the pure severity/reason helper.
func TestClassifyUnindexed(t *testing.T) {
	sev, reason := classifyUnindexed(false)
	if sev != "high" {
		t.Errorf("no-unique table should be high, got %q", sev)
	}
	if !strings.Contains(reason, "GEN_CLUST_INDEX") {
		t.Errorf("high reason should mention GEN_CLUST_INDEX: %q", reason)
	}
	sev, reason = classifyUnindexed(true)
	if sev != "medium" {
		t.Errorf("has-unique table should be medium, got %q", sev)
	}
	if !strings.Contains(reason, "UNIQUE index exists") {
		t.Errorf("medium reason should mention the UNIQUE index: %q", reason)
	}
}

// TestUnindexedSeverityRank guards the sort ordering: high < medium < unknown.
func TestUnindexedSeverityRank(t *testing.T) {
	if unindexedSeverityRank("high") >= unindexedSeverityRank("medium") {
		t.Error("high should rank before medium")
	}
	if unindexedSeverityRank("medium") >= unindexedSeverityRank("other") {
		t.Error("medium should rank before unknown severities")
	}
}
