package mcpserver

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// columnTypeDriftCols mirrors the SELECT column order in collectColumnTypeDrift.
var columnTypeDriftCols = []string{
	"TABLE_SCHEMA", "TABLE_NAME", "COLUMN_NAME", "DATA_TYPE", "COLUMN_TYPE",
}

// TestDBColumnTypeDrift_DescriptionMentionsContext anchors the description to the
// keywords the agent uses to pick this tool. Same guard shape as the other db_*
// tools — keywords must live in the registered Description string.
func TestDBColumnTypeDrift_DescriptionMentionsContext(t *testing.T) {
	reg := NewRegistry()
	RegisterDBColumnTypeDriftTool(reg, DBDeps{})
	tool, ok := reg.Get("db_column_type_drift")
	if !ok {
		t.Fatal("db_column_type_drift not registered")
	}
	for _, kw := range []string{
		"information_schema.COLUMNS", "information_schema.TABLES",
		"DATA_TYPE", "COLUMN_TYPE", "signedness", "guid",
		"int(10) unsigned", "bigint(20)", "SIGNED", "UNSIGNED",
		"implicit", "index", "JOIN", "display width", "varchar(64)",
		"distinctSignatures", "driftCount",
		"db_charset_collation_audit", "db_table_info",
		"SHOW CREATE TABLE", "ALTER TABLE",
		"acore_playerbots", "ops_ro", "minSeverity", "Read-only",
	} {
		if !strings.Contains(tool.Description, kw) {
			t.Errorf("description missing %q", kw)
		}
	}
}

func TestDBColumnTypeDrift_ReadOnlyAnnotation(t *testing.T) {
	reg := NewRegistry()
	RegisterDBColumnTypeDriftTool(reg, DBDeps{})
	tool, _ := reg.Get("db_column_type_drift")
	if !strings.Contains(string(tool.Annotations), `"readOnlyHint":true`) {
		t.Errorf("expected readOnlyHint:true, got %s", tool.Annotations)
	}
	if tool.Destructive {
		t.Errorf("destructive should be false")
	}
}

func TestDBColumnTypeDrift_NoPoolReturnsError(t *testing.T) {
	reg := NewRegistry()
	RegisterDBColumnTypeDriftTool(reg, DBDeps{})
	tool, _ := reg.Get("db_column_type_drift")
	resp := tool.Handler(context.Background(), json.RawMessage(`{}`), "")
	got, _ := resp.(map[string]any)
	if msg, _ := got["error"].(string); !strings.Contains(msg, "ops_ro pool not configured") {
		t.Errorf("expected pool-not-configured error, got %v", got)
	}
}

// TestDBColumnTypeDrift_InvalidDatabaseRejected — an out-of-allowlist database is
// rejected with NO query run (a typo reads as "bad filter", not "empty schema").
func TestDBColumnTypeDrift_InvalidDatabaseRejected(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	reg := NewRegistry()
	RegisterDBColumnTypeDriftTool(reg, DBDeps{QueryDB: db})
	tool, _ := reg.Get("db_column_type_drift")
	resp := tool.Handler(context.Background(), json.RawMessage(`{"database":"acore_playerbots"}`), "")
	got, _ := resp.(map[string]any)
	if msg, _ := got["error"].(string); !strings.Contains(msg, "acore_world/acore_characters/acore_auth") {
		t.Errorf("expected allowlist error, got %v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("a query was issued despite the reject: %v", err)
	}
}

// TestDBColumnTypeDrift_InvalidMinSeverityRejected — an out-of-range minSeverity
// is rejected with NO query run (reject-not-clamp, like db_engine_distribution).
func TestDBColumnTypeDrift_InvalidMinSeverityRejected(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	reg := NewRegistry()
	RegisterDBColumnTypeDriftTool(reg, DBDeps{QueryDB: db})
	tool, _ := reg.Get("db_column_type_drift")
	resp := tool.Handler(context.Background(), json.RawMessage(`{"minSeverity":"critical"}`), "")
	got, _ := resp.(map[string]any)
	if msg, _ := got["error"].(string); !strings.Contains(msg, "minSeverity must be one of low/medium/high") {
		t.Errorf("expected minSeverity reject error, got %v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("a query was issued despite the reject: %v", err)
	}
}

// TestDBColumnTypeDrift_GoldenAggregation drives the default path (all three
// schemas) and verifies the full fold: HIGH drift (base type differs), MEDIUM
// drift (signedness differs), display-width-only + varchar-length-only NOT
// flagged, variant grouping/sorting + table sort, worst-first candidate sort,
// perDatabase counts (empty schema surfaces), totals.
func TestDBColumnTypeDrift_GoldenAggregation(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	rs := sqlmock.NewRows(columnTypeDriftCols).
		// acore_characters
		// guid: int unsigned (2 tables) + bigint unsigned (1) -> HIGH (base differs), tc 3.
		AddRow("acore_characters", "characters", "guid", "int", "int(10) unsigned").
		AddRow("acore_characters", "character_inventory", "guid", "int", "int(10) unsigned").
		AddRow("acore_characters", "item_instance", "guid", "bigint", "bigint(20) unsigned").
		// account: int unsigned + int signed -> MEDIUM (signedness differs), tc 2.
		AddRow("acore_characters", "characters", "account", "int", "int(10) unsigned").
		AddRow("acore_characters", "guild", "account", "int", "int(11)").
		// flags: int(10) unsigned vs int(11) unsigned -> display width only, NOT flagged.
		AddRow("acore_characters", "characters", "flags", "int", "int(10) unsigned").
		AddRow("acore_characters", "guild", "flags", "int", "int(11) unsigned").
		// name: varchar(12) vs varchar(24) -> length only, NOT flagged.
		AddRow("acore_characters", "characters", "name", "varchar", "varchar(12)").
		AddRow("acore_characters", "guild", "name", "varchar", "varchar(24)").
		// level: uniform -> NOT flagged.
		AddRow("acore_characters", "characters", "level", "tinyint", "tinyint(3) unsigned").
		AddRow("acore_characters", "guild", "level", "tinyint", "tinyint(3) unsigned").
		// acore_world
		// entry: int unsigned + mediumint unsigned -> HIGH (base differs), tc 2.
		AddRow("acore_world", "creature", "entry", "int", "int(10) unsigned").
		AddRow("acore_world", "creature_template", "entry", "mediumint", "mediumint(8) unsigned").
		// acore_auth — no drift (each name in a single table).
		AddRow("acore_auth", "account", "id", "int", "int(10) unsigned").
		AddRow("acore_auth", "account", "username", "varchar", "varchar(32)")

	mock.ExpectQuery(regexp.QuoteMeta("FROM information_schema.COLUMNS")).
		WithArgs("acore_world", "acore_characters", "acore_auth").
		WillReturnRows(rs)

	reg := NewRegistry()
	RegisterDBColumnTypeDriftTool(reg, DBDeps{QueryDB: db})
	tool, _ := reg.Get("db_column_type_drift")
	resp := tool.Handler(context.Background(), json.RawMessage(`{}`), "")
	got, ok := resp.(map[string]any)
	if !ok {
		t.Fatalf("unexpected response shape: %T", resp)
	}
	if msg, hasErr := got["error"]; hasErr {
		t.Fatalf("unexpected error: %v", msg)
	}

	cands := got["candidates"].([]columnTypeDriftRow)
	if len(cands) != 3 {
		t.Fatalf("expected 3 flagged columns, got %d: %+v", len(cands), cands)
	}
	// Worst-first: HIGH by blast radius desc (guid tc3 > entry tc2), then MEDIUM (account).
	if cands[0].Database != "acore_characters" || cands[0].Column != "guid" ||
		cands[0].Severity != "high" || cands[0].TableCount != 3 || cands[0].DistinctSignatures != 2 {
		t.Errorf("cands[0] should be acore_characters/guid HIGH tc3 sig2: %+v", cands[0])
	}
	if !strings.Contains(cands[0].Reason, "different base types") || !strings.Contains(cands[0].Reason, "ALTER TABLE ... MODIFY") {
		t.Errorf("cands[0] high reason should name base-type drift + the fix: %q", cands[0].Reason)
	}
	// guid variants sorted dataType asc: bigint(20) unsigned then int(10) unsigned.
	if len(cands[0].Variants) != 2 {
		t.Fatalf("guid should have 2 variants, got %+v", cands[0].Variants)
	}
	if cands[0].Variants[0].DataType != "bigint" || cands[0].Variants[0].ColumnType != "bigint(20) unsigned" ||
		!cands[0].Variants[0].Unsigned || cands[0].Variants[0].TableCount != 1 ||
		len(cands[0].Variants[0].Tables) != 1 || cands[0].Variants[0].Tables[0] != "item_instance" {
		t.Errorf("guid variants[0] wrong: %+v", cands[0].Variants[0])
	}
	if cands[0].Variants[1].DataType != "int" || cands[0].Variants[1].ColumnType != "int(10) unsigned" ||
		cands[0].Variants[1].TableCount != 2 ||
		len(cands[0].Variants[1].Tables) != 2 ||
		cands[0].Variants[1].Tables[0] != "character_inventory" || cands[0].Variants[1].Tables[1] != "characters" {
		t.Errorf("guid variants[1] wrong (tables should be sorted asc): %+v", cands[0].Variants[1])
	}
	if cands[1].Database != "acore_world" || cands[1].Column != "entry" ||
		cands[1].Severity != "high" || cands[1].TableCount != 2 {
		t.Errorf("cands[1] should be acore_world/entry HIGH tc2: %+v", cands[1])
	}
	if cands[2].Database != "acore_characters" || cands[2].Column != "account" ||
		cands[2].Severity != "medium" || cands[2].TableCount != 2 {
		t.Errorf("cands[2] should be acore_characters/account MEDIUM tc2: %+v", cands[2])
	}
	if !strings.Contains(cands[2].Reason, "SIGNED and UNSIGNED") || strings.Contains(cands[2].Reason, "different base types") {
		t.Errorf("cands[2] medium reason should name signedness (not base-type) drift: %q", cands[2].Reason)
	}

	totals := got["totals"].(map[string]any)
	if totals["columnsScanned"].(int) != 15 {
		t.Errorf("totals.columnsScanned want 15, got %v", totals["columnsScanned"])
	}
	if totals["driftCount"].(int) != 3 {
		t.Errorf("totals.driftCount want 3, got %v", totals["driftCount"])
	}

	// No minSeverity applied → the key is omitted.
	if _, present := got["minSeverity"]; present {
		t.Errorf("minSeverity should be omitted when not set, got %v", got["minSeverity"])
	}

	perDB := got["perDatabase"].([]columnTypeDriftPerDatabase)
	if len(perDB) != 3 {
		t.Fatalf("expected 3 perDatabase rows, got %d", len(perDB))
	}
	// Sorted driftCount desc, schema asc: characters(2), world(1), auth(0).
	if perDB[0].Database != "acore_characters" || perDB[0].DriftCount != 2 ||
		perDB[0].ColumnsScanned != 11 || perDB[0].DistinctColumnNames != 5 {
		t.Errorf("perDB[0] acore_characters rollup wrong: %+v", perDB[0])
	}
	if perDB[1].Database != "acore_world" || perDB[1].DriftCount != 1 ||
		perDB[1].ColumnsScanned != 2 || perDB[1].DistinctColumnNames != 1 {
		t.Errorf("perDB[1] acore_world rollup wrong: %+v", perDB[1])
	}
	if perDB[2].Database != "acore_auth" || perDB[2].DriftCount != 0 ||
		perDB[2].ColumnsScanned != 2 || perDB[2].DistinctColumnNames != 2 {
		t.Errorf("perDB[2] acore_auth rollup wrong (empty schema still surfaces): %+v", perDB[2])
	}

	if got["truncated"].(bool) {
		t.Errorf("truncated should be false for 3 candidates")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestDBColumnTypeDrift_DatabaseFilterNarrowsScan verifies `database` narrows the
// IN clause to a single bind and severity is the primary sort key.
func TestDBColumnTypeDrift_DatabaseFilterNarrowsScan(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectQuery(regexp.QuoteMeta("FROM information_schema.COLUMNS")).
		WithArgs("acore_characters"). // single bind — IN list narrowed to one schema
		WillReturnRows(sqlmock.NewRows(columnTypeDriftCols).
			AddRow("acore_characters", "characters", "account", "int", "int(10) unsigned").
			AddRow("acore_characters", "guild", "account", "int", "int(11)"). // MEDIUM
			AddRow("acore_characters", "characters", "guid", "int", "int(10) unsigned").
			AddRow("acore_characters", "item_instance", "guid", "bigint", "bigint(20)")) // HIGH

	reg := NewRegistry()
	RegisterDBColumnTypeDriftTool(reg, DBDeps{QueryDB: db})
	tool, _ := reg.Get("db_column_type_drift")
	resp := tool.Handler(context.Background(), json.RawMessage(`{"database":"acore_characters"}`), "")
	got := resp.(map[string]any)
	if msg, hasErr := got["error"]; hasErr {
		t.Fatalf("unexpected error: %v", msg)
	}
	scanned := got["scannedSchemas"].([]string)
	if len(scanned) != 1 || scanned[0] != "acore_characters" {
		t.Errorf("scannedSchemas want [acore_characters], got %v", scanned)
	}
	perDB := got["perDatabase"].([]columnTypeDriftPerDatabase)
	if len(perDB) != 1 || perDB[0].Database != "acore_characters" {
		t.Errorf("perDatabase want one acore_characters row, got %+v", perDB)
	}
	cands := got["candidates"].([]columnTypeDriftRow)
	// Worst-first: HIGH (guid) before MEDIUM (account) even though "account" < "guid".
	if len(cands) != 2 || cands[0].Column != "guid" || cands[0].Severity != "high" ||
		cands[1].Column != "account" || cands[1].Severity != "medium" {
		t.Errorf("expected [guid HIGH, account MEDIUM], got %+v", cands)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestDBColumnTypeDrift_MinSeverityFilter — minSeverity=high trims the candidate
// list to the base-type drift only, while perDatabase / totals stay the honest
// full drift counts.
func TestDBColumnTypeDrift_MinSeverityFilter(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectQuery(regexp.QuoteMeta("FROM information_schema.COLUMNS")).
		WithArgs("acore_world", "acore_characters", "acore_auth").
		WillReturnRows(sqlmock.NewRows(columnTypeDriftCols).
			AddRow("acore_characters", "characters", "guid", "int", "int(10) unsigned").
			AddRow("acore_characters", "item_instance", "guid", "bigint", "bigint(20)"). // HIGH
			AddRow("acore_characters", "characters", "account", "int", "int(10) unsigned").
			AddRow("acore_characters", "guild", "account", "int", "int(11)")) // MEDIUM

	reg := NewRegistry()
	RegisterDBColumnTypeDriftTool(reg, DBDeps{QueryDB: db})
	tool, _ := reg.Get("db_column_type_drift")
	resp := tool.Handler(context.Background(), json.RawMessage(`{"minSeverity":"high"}`), "")
	got := resp.(map[string]any)
	if msg, hasErr := got["error"]; hasErr {
		t.Fatalf("unexpected error: %v", msg)
	}

	cands := got["candidates"].([]columnTypeDriftRow)
	if len(cands) != 1 || cands[0].Column != "guid" || cands[0].Severity != "high" {
		t.Errorf("minSeverity=high should list only the base-type drift, got %+v", cands)
	}
	// totals stays honest: 2 drifts total (1 high guid + 1 medium account).
	totals := got["totals"].(map[string]any)
	if totals["driftCount"].(int) != 2 {
		t.Errorf("totals.driftCount should be honest 2, got %v", totals["driftCount"])
	}
	// perDatabase driftCount also honest: acore_characters keeps its 2 drifts.
	perDB := got["perDatabase"].([]columnTypeDriftPerDatabase)
	if perDB[0].Database != "acore_characters" || perDB[0].DriftCount != 2 {
		t.Errorf("perDB[0] acore_characters should keep its honest 2 drifts, got %+v", perDB[0])
	}
	if got["minSeverity"].(string) != "high" {
		t.Errorf("applied minSeverity should be echoed as high, got %v", got["minSeverity"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestDBColumnTypeDrift_EmptyResultNonNilSlice — when every same-named column is
// uniformly typed (incl. display-width-only + length-only variation that the
// signature deliberately ignores), `candidates` is a non-nil empty slice (renders
// as []), and every scanned schema still surfaces in perDatabase.
func TestDBColumnTypeDrift_EmptyResultNonNilSlice(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectQuery(regexp.QuoteMeta("FROM information_schema.COLUMNS")).
		WithArgs("acore_world", "acore_characters", "acore_auth").
		WillReturnRows(sqlmock.NewRows(columnTypeDriftCols).
			// guid: same signature across tables despite display-width difference.
			AddRow("acore_characters", "characters", "guid", "int", "int(10) unsigned").
			AddRow("acore_characters", "item_instance", "guid", "int", "int(11) unsigned").
			// name: varchar length differs but same signature -> not flagged.
			AddRow("acore_characters", "characters", "name", "varchar", "varchar(12)").
			AddRow("acore_characters", "guild", "name", "varchar", "varchar(24)"))

	reg := NewRegistry()
	RegisterDBColumnTypeDriftTool(reg, DBDeps{QueryDB: db})
	tool, _ := reg.Get("db_column_type_drift")
	resp := tool.Handler(context.Background(), json.RawMessage(`{}`), "")
	got := resp.(map[string]any)

	cands := got["candidates"].([]columnTypeDriftRow)
	if cands == nil {
		t.Errorf("candidates should be non-nil empty slice")
	}
	if len(cands) != 0 {
		t.Errorf("expected 0 candidates, got %d", len(cands))
	}
	b, _ := json.Marshal(got)
	if !strings.Contains(string(b), `"candidates":[]`) {
		t.Errorf("candidates should marshal to [], got %s", b)
	}
	perDB := got["perDatabase"].([]columnTypeDriftPerDatabase)
	if len(perDB) != 3 {
		t.Errorf("all 3 scanned schemas should surface, got %d", len(perDB))
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestDBColumnTypeDrift_Truncation — more than the 500 cap drifted → candidates
// sliced to 500 + truncated:true, but totals stays the honest pre-cap count.
func TestDBColumnTypeDrift_Truncation(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	rs := sqlmock.NewRows(columnTypeDriftCols)
	const drifted = dbColumnTypeDriftMaxCandidates + 100 // 600
	// Each column name c_<i> is int in wide_a and bigint in wide_b -> HIGH drift.
	for i := 0; i < drifted; i++ {
		col := "c_" + itoaSmall(i)
		rs.AddRow("acore_characters", "wide_a", col, "int", "int(10) unsigned")
		rs.AddRow("acore_characters", "wide_b", col, "bigint", "bigint(20) unsigned")
	}
	mock.ExpectQuery(regexp.QuoteMeta("FROM information_schema.COLUMNS")).
		WithArgs("acore_world", "acore_characters", "acore_auth").
		WillReturnRows(rs)

	reg := NewRegistry()
	RegisterDBColumnTypeDriftTool(reg, DBDeps{QueryDB: db})
	tool, _ := reg.Get("db_column_type_drift")
	resp := tool.Handler(context.Background(), json.RawMessage(`{}`), "")
	got := resp.(map[string]any)

	cands := got["candidates"].([]columnTypeDriftRow)
	if len(cands) != dbColumnTypeDriftMaxCandidates {
		t.Errorf("candidates should be capped at %d, got %d", dbColumnTypeDriftMaxCandidates, len(cands))
	}
	if !got["truncated"].(bool) {
		t.Errorf("truncated should be true")
	}
	totals := got["totals"].(map[string]any)
	if totals["driftCount"].(int) != drifted {
		t.Errorf("totals.driftCount should be honest pre-cap %d, got %v", drifted, totals["driftCount"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestDBColumnTypeDrift_PureHelpers covers the unit-testable helpers in isolation:
// signedness detection, signature construction, severity classification/ranking,
// the shared severityName inverse, and variant finalization (sort + table clip).
func TestDBColumnTypeDrift_PureHelpers(t *testing.T) {
	// columnTypeUnsigned: only numeric "unsigned" columns, case-insensitive.
	if !columnTypeUnsigned("int(10) unsigned") || !columnTypeUnsigned("INT(10) UNSIGNED") {
		t.Errorf("unsigned int columns should report true")
	}
	if columnTypeUnsigned("int(11)") || columnTypeUnsigned("varchar(32)") {
		t.Errorf("signed / string columns should report false")
	}

	// columnTypeSignature: base type lowercased + signedness tag; width ignored.
	if columnTypeSignature("INT", true) != "int|u" || columnTypeSignature("int", false) != "int|s" {
		t.Errorf("signature construction wrong")
	}
	if columnTypeSignature("BigInt", true) != "bigint|u" {
		t.Errorf("signature should lowercase the base type")
	}

	// classifyColumnTypeDrift: >=2 base types -> high, single base type -> medium.
	if classifyColumnTypeDrift(2) != "high" || classifyColumnTypeDrift(3) != "high" {
		t.Errorf("multi-base-type drift should be high")
	}
	if classifyColumnTypeDrift(1) != "medium" {
		t.Errorf("single-base-type (signedness) drift should be medium")
	}

	// columnTypeDriftSeverityRank ordering: high < medium < low < other.
	if !(columnTypeDriftSeverityRank("high") < columnTypeDriftSeverityRank("medium") &&
		columnTypeDriftSeverityRank("medium") < columnTypeDriftSeverityRank("low") &&
		columnTypeDriftSeverityRank("low") < columnTypeDriftSeverityRank("other")) {
		t.Errorf("severity rank ordering wrong")
	}

	// severityName (shared helper) inverse — the ranks this tool emits map back.
	if severityName(0) != "high" || severityName(1) != "medium" || severityName(2) != "low" {
		t.Errorf("severityName mapping wrong")
	}

	// finalizeColumnTypeVariants: variants sorted dataType asc; a variant's example
	// tables sorted asc + clipped to the per-variant cap, TableCount stays honest.
	manyTables := make([]string, 0, dbColumnTypeDriftMaxTablesPerVariant+5)
	for i := 0; i < dbColumnTypeDriftMaxTablesPerVariant+5; i++ {
		manyTables = append(manyTables, "t"+itoaSmall(i))
	}
	variants := map[string]*columnTypeVariant{
		"int(10) unsigned": {DataType: "int", ColumnType: "int(10) unsigned", Unsigned: true,
			TableCount: len(manyTables), Tables: append([]string(nil), manyTables...)},
		"bigint(20)": {DataType: "bigint", ColumnType: "bigint(20)", Unsigned: false,
			TableCount: 1, Tables: []string{"z_only"}},
	}
	out := finalizeColumnTypeVariants(variants)
	if len(out) != 2 || out[0].DataType != "bigint" || out[1].DataType != "int" {
		t.Fatalf("variants should sort dataType asc: %+v", out)
	}
	if len(out[1].Tables) != dbColumnTypeDriftMaxTablesPerVariant {
		t.Errorf("int variant tables should be clipped to %d, got %d", dbColumnTypeDriftMaxTablesPerVariant, len(out[1].Tables))
	}
	if out[1].TableCount != dbColumnTypeDriftMaxTablesPerVariant+5 {
		t.Errorf("int variant TableCount should stay the honest %d, got %d", dbColumnTypeDriftMaxTablesPerVariant+5, out[1].TableCount)
	}
	if out[1].Tables[0] != "t0" {
		t.Errorf("tables should be sorted asc, got first %q", out[1].Tables[0])
	}
}
