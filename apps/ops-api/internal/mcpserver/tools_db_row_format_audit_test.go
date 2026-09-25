package mcpserver

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// rowFormatTablesCols mirrors the SELECT column order in collectRowFormatAudit
// (ROW_FORMAT / TABLE_ROWS / DATA_LENGTH / INDEX_LENGTH already IFNULL'd by the
// query).
var rowFormatTablesCols = []string{
	"TABLE_SCHEMA", "TABLE_NAME", "ROW_FORMAT", "TABLE_ROWS", "DATA_LENGTH", "INDEX_LENGTH",
}

// TestDBRowFormatAudit_DescriptionMentionsContext anchors the description to the
// keywords the agent uses to pick this tool. Same guard shape as the other db_*
// tools — keywords must live in the registered Description string.
func TestDBRowFormatAudit_DescriptionMentionsContext(t *testing.T) {
	reg := NewRegistry()
	RegisterDBRowFormatAuditTool(reg, DBDeps{})
	tool, ok := reg.Get("db_row_format_audit")
	if !ok {
		t.Fatal("db_row_format_audit not registered")
	}
	for _, kw := range []string{
		"information_schema.TABLES", "ROW_FORMAT", "InnoDB",
		"Antelope", "Barracuda", "Compact", "Redundant", "Dynamic", "Compressed",
		"767", "index key prefix", "ALTER TABLE", "ROW_FORMAT=DYNAMIC",
		"SHOW TABLE STATUS", "SHOW CREATE TABLE",
		"db_engine_distribution", "db_table_info", "acore_playerbots",
		"ops_ro", "minSeverity", "Read-only",
	} {
		if !strings.Contains(tool.Description, kw) {
			t.Errorf("description missing %q", kw)
		}
	}
}

func TestDBRowFormatAudit_ReadOnlyAnnotation(t *testing.T) {
	reg := NewRegistry()
	RegisterDBRowFormatAuditTool(reg, DBDeps{})
	tool, _ := reg.Get("db_row_format_audit")
	if !strings.Contains(string(tool.Annotations), `"readOnlyHint":true`) {
		t.Errorf("expected readOnlyHint:true, got %s", tool.Annotations)
	}
	if tool.Destructive {
		t.Errorf("destructive should be false")
	}
}

func TestDBRowFormatAudit_NoPoolReturnsError(t *testing.T) {
	reg := NewRegistry()
	RegisterDBRowFormatAuditTool(reg, DBDeps{})
	tool, _ := reg.Get("db_row_format_audit")
	resp := tool.Handler(context.Background(), json.RawMessage(`{}`), "")
	got, _ := resp.(map[string]any)
	if msg, _ := got["error"].(string); !strings.Contains(msg, "ops_ro pool not configured") {
		t.Errorf("expected pool-not-configured error, got %v", got)
	}
}

// TestDBRowFormatAudit_InvalidDatabaseRejected — an out-of-allowlist database is
// rejected with NO query run (a typo reads as "bad filter", not "empty schema").
func TestDBRowFormatAudit_InvalidDatabaseRejected(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	reg := NewRegistry()
	RegisterDBRowFormatAuditTool(reg, DBDeps{QueryDB: db})
	tool, _ := reg.Get("db_row_format_audit")
	resp := tool.Handler(context.Background(), json.RawMessage(`{"database":"acore_playerbots"}`), "")
	got, _ := resp.(map[string]any)
	if msg, _ := got["error"].(string); !strings.Contains(msg, "acore_world/acore_characters/acore_auth") {
		t.Errorf("expected allowlist error, got %v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("a query was issued despite the reject: %v", err)
	}
}

// TestDBRowFormatAudit_InvalidMinSeverityRejected — an out-of-range minSeverity is
// rejected with NO query run (reject-not-clamp, like db_engine_distribution).
func TestDBRowFormatAudit_InvalidMinSeverityRejected(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	reg := NewRegistry()
	RegisterDBRowFormatAuditTool(reg, DBDeps{QueryDB: db})
	tool, _ := reg.Get("db_row_format_audit")
	resp := tool.Handler(context.Background(), json.RawMessage(`{"minSeverity":"critical"}`), "")
	got, _ := resp.(map[string]any)
	if msg, _ := got["error"].(string); !strings.Contains(msg, "minSeverity must be one of low/medium/high") {
		t.Errorf("expected minSeverity reject error, got %v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("a query was issued despite the reject: %v", err)
	}
}

// TestDBRowFormatAudit_GoldenAggregation drives the default path (all three
// schemas) and verifies the full fold: absolute Antelope classification (Redundant
// high / Compact medium), Barracuda formats (Dynamic / Compressed) NOT flagged,
// rowFormatBreakdown ordering, worst-first candidate sort (severity then bytes),
// perDatabase counts, totals.
func TestDBRowFormatAudit_GoldenAggregation(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	rs := sqlmock.NewRows(rowFormatTablesCols).
		// acore_characters — dominant Dynamic (2), one Compact (MEDIUM), one Redundant (HIGH).
		AddRow("acore_characters", "characters", "Dynamic", 100, 1000, 200). // healthy
		AddRow("acore_characters", "guild_member", "Dynamic", 80, 800, 150). // healthy
		AddRow("acore_characters", "guild", "Compact", 50, 5000, 1000).      // MEDIUM (total 6000)
		AddRow("acore_characters", "logs", "Redundant", 9999, 8000, 2000).   // HIGH (total 10000)
		// acore_auth — Dynamic + Compressed (both Barracuda, healthy) + one Compact (MEDIUM).
		AddRow("acore_auth", "account", "Dynamic", 10, 300, 50).         // healthy
		AddRow("acore_auth", "account_banned", "Compressed", 5, 100, 0). // healthy (Barracuda)
		AddRow("acore_auth", "account_access", "Compact", 4, 400, 100).  // MEDIUM (total 500)
		// acore_world — its InnoDB tables only; one Redundant (HIGH) + one Dynamic (healthy).
		AddRow("acore_world", "playercreateinfo", "Redundant", 30, 2000, 500). // HIGH (total 2500)
		AddRow("acore_world", "spell_dbc_cache", "Dynamic", 200, 600, 0)       // healthy

	mock.ExpectQuery(regexp.QuoteMeta("FROM information_schema.TABLES")).
		WithArgs("acore_world", "acore_characters", "acore_auth").
		WillReturnRows(rs)

	reg := NewRegistry()
	RegisterDBRowFormatAuditTool(reg, DBDeps{QueryDB: db})
	tool, _ := reg.Get("db_row_format_audit")
	resp := tool.Handler(context.Background(), json.RawMessage(`{}`), "")
	got, ok := resp.(map[string]any)
	if !ok {
		t.Fatalf("unexpected response shape: %T", resp)
	}
	if msg, hasErr := got["error"]; hasErr {
		t.Fatalf("unexpected error: %v", msg)
	}

	cands := got["candidates"].([]rowFormatLegacyRow)
	if len(cands) != 4 {
		t.Fatalf("expected 4 flagged tables, got %d: %+v", len(cands), cands)
	}
	// Worst-first: HIGH (Redundant) by bytes desc, then MEDIUM (Compact) by bytes desc.
	if cands[0].Database != "acore_characters" || cands[0].Table != "logs" ||
		cands[0].Severity != "high" || cands[0].RowFormat != "Redundant" || cands[0].TotalBytes != 10000 {
		t.Errorf("cands[0] should be acore_characters/logs Redundant HIGH total 10000: %+v", cands[0])
	}
	if !strings.Contains(cands[0].Reason, "ROW_FORMAT=DYNAMIC") || !strings.Contains(cands[0].Reason, "oldest") {
		t.Errorf("cands[0] high reason should name the migration + 'oldest': %q", cands[0].Reason)
	}
	if cands[1].Database != "acore_world" || cands[1].Table != "playercreateinfo" ||
		cands[1].Severity != "high" || cands[1].TotalBytes != 2500 {
		t.Errorf("cands[1] should be acore_world/playercreateinfo Redundant HIGH total 2500: %+v", cands[1])
	}
	if cands[2].Database != "acore_characters" || cands[2].Table != "guild" ||
		cands[2].Severity != "medium" || cands[2].RowFormat != "Compact" || cands[2].TotalBytes != 6000 {
		t.Errorf("cands[2] should be acore_characters/guild Compact MEDIUM total 6000: %+v", cands[2])
	}
	if cands[3].Database != "acore_auth" || cands[3].Table != "account_access" ||
		cands[3].Severity != "medium" || cands[3].TotalBytes != 500 {
		t.Errorf("cands[3] should be acore_auth/account_access Compact MEDIUM total 500: %+v", cands[3])
	}
	if strings.Contains(cands[3].Reason, "oldest") {
		t.Errorf("cands[3] medium reason should not claim 'oldest' (that's the high/Redundant copy): %q", cands[3].Reason)
	}

	totals := got["totals"].(map[string]any)
	if totals["tablesScanned"].(int) != 9 {
		t.Errorf("totals.tablesScanned want 9, got %v", totals["tablesScanned"])
	}
	if totals["legacyCount"].(int) != 4 {
		t.Errorf("totals.legacyCount want 4, got %v", totals["legacyCount"])
	}

	// No minSeverity applied → the key is omitted.
	if _, present := got["minSeverity"]; present {
		t.Errorf("minSeverity should be omitted when not set, got %v", got["minSeverity"])
	}

	perDB := got["perDatabase"].([]rowFormatPerDatabase)
	if len(perDB) != 3 {
		t.Fatalf("expected 3 perDatabase rows, got %d", len(perDB))
	}
	// Sorted legacyCount desc, schema asc: characters(2), then auth(1)/world(1) db-asc.
	if perDB[0].Database != "acore_characters" || perDB[0].LegacyCount != 2 ||
		perDB[0].TablesScanned != 4 || perDB[0].DistinctRowFormats != 3 {
		t.Errorf("perDB[0] acore_characters rollup wrong: %+v", perDB[0])
	}
	// rowFormatBreakdown sorted tableCount desc: Dynamic(2), then Compact/Redundant (1 each) rowFormat-asc.
	if len(perDB[0].RowFormatBreakdown) != 3 ||
		perDB[0].RowFormatBreakdown[0].RowFormat != "Dynamic" || perDB[0].RowFormatBreakdown[0].TableCount != 2 ||
		perDB[0].RowFormatBreakdown[1].RowFormat != "Compact" || perDB[0].RowFormatBreakdown[2].RowFormat != "Redundant" {
		t.Errorf("perDB[0] rowFormatBreakdown order wrong: %+v", perDB[0].RowFormatBreakdown)
	}
	// Dynamic bytes summed across the 2 healthy tables: (1000+200)+(800+150)=2150.
	if perDB[0].RowFormatBreakdown[0].TotalBytes != 2150 {
		t.Errorf("perDB[0] Dynamic totalBytes want 2150, got %d", perDB[0].RowFormatBreakdown[0].TotalBytes)
	}
	if perDB[1].Database != "acore_auth" || perDB[1].LegacyCount != 1 || perDB[1].DistinctRowFormats != 3 {
		t.Errorf("perDB[1] want acore_auth legacy 1 / 3 distinct formats: %+v", perDB[1])
	}
	if perDB[2].Database != "acore_world" || perDB[2].LegacyCount != 1 || perDB[2].TablesScanned != 2 {
		t.Errorf("perDB[2] want acore_world legacy 1 / 2 scanned: %+v", perDB[2])
	}

	if got["truncated"].(bool) {
		t.Errorf("truncated should be false for 4 candidates")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestDBRowFormatAudit_DatabaseFilterNarrowsScan verifies `database` narrows the
// IN clause to a single bind.
func TestDBRowFormatAudit_DatabaseFilterNarrowsScan(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectQuery(regexp.QuoteMeta("FROM information_schema.TABLES")).
		WithArgs("acore_characters"). // single bind — IN list narrowed to one schema
		WillReturnRows(sqlmock.NewRows(rowFormatTablesCols).
			AddRow("acore_characters", "characters", "Dynamic", 100, 1000, 200). // healthy
			AddRow("acore_characters", "guild", "Compact", 50, 5000, 1000).      // MEDIUM
			AddRow("acore_characters", "logs", "Redundant", 9999, 8000, 2000))   // HIGH

	reg := NewRegistry()
	RegisterDBRowFormatAuditTool(reg, DBDeps{QueryDB: db})
	tool, _ := reg.Get("db_row_format_audit")
	resp := tool.Handler(context.Background(), json.RawMessage(`{"database":"acore_characters"}`), "")
	got := resp.(map[string]any)
	if msg, hasErr := got["error"]; hasErr {
		t.Fatalf("unexpected error: %v", msg)
	}
	scanned := got["scannedSchemas"].([]string)
	if len(scanned) != 1 || scanned[0] != "acore_characters" {
		t.Errorf("scannedSchemas want [acore_characters], got %v", scanned)
	}
	perDB := got["perDatabase"].([]rowFormatPerDatabase)
	if len(perDB) != 1 || perDB[0].Database != "acore_characters" {
		t.Errorf("perDatabase want one acore_characters row, got %+v", perDB)
	}
	cands := got["candidates"].([]rowFormatLegacyRow)
	// Worst-first: Redundant HIGH (logs) before Compact MEDIUM (guild).
	if len(cands) != 2 || cands[0].Table != "logs" || cands[0].Severity != "high" ||
		cands[1].Table != "guild" || cands[1].Severity != "medium" {
		t.Errorf("expected [logs HIGH, guild MEDIUM], got %+v", cands)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestDBRowFormatAudit_MinSeverityFilter — minSeverity=high trims the candidate
// list to the Redundant tables only, while perDatabase / totals stay the honest
// full legacy counts.
func TestDBRowFormatAudit_MinSeverityFilter(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectQuery(regexp.QuoteMeta("FROM information_schema.TABLES")).
		WithArgs("acore_world", "acore_characters", "acore_auth").
		WillReturnRows(sqlmock.NewRows(rowFormatTablesCols).
			AddRow("acore_characters", "characters", "Dynamic", 100, 1000, 200). // healthy
			AddRow("acore_characters", "guild", "Compact", 50, 5000, 1000).      // MEDIUM
			AddRow("acore_characters", "logs", "Redundant", 9999, 8000, 2000))   // HIGH

	reg := NewRegistry()
	RegisterDBRowFormatAuditTool(reg, DBDeps{QueryDB: db})
	tool, _ := reg.Get("db_row_format_audit")
	resp := tool.Handler(context.Background(), json.RawMessage(`{"minSeverity":"high"}`), "")
	got := resp.(map[string]any)
	if msg, hasErr := got["error"]; hasErr {
		t.Fatalf("unexpected error: %v", msg)
	}

	cands := got["candidates"].([]rowFormatLegacyRow)
	if len(cands) != 1 || cands[0].Table != "logs" || cands[0].Severity != "high" {
		t.Errorf("minSeverity=high should list only the Redundant table, got %+v", cands)
	}
	// totals stays honest: 2 legacy total (1 high Redundant + 1 medium Compact).
	totals := got["totals"].(map[string]any)
	if totals["legacyCount"].(int) != 2 {
		t.Errorf("totals.legacyCount should be honest 2, got %v", totals["legacyCount"])
	}
	// perDatabase legacyCount also honest: acore_characters keeps its 2 legacy tables.
	perDB := got["perDatabase"].([]rowFormatPerDatabase)
	if perDB[0].Database != "acore_characters" || perDB[0].LegacyCount != 2 {
		t.Errorf("perDB[0] acore_characters should keep its honest 2 legacy tables, got %+v", perDB[0])
	}
	if got["minSeverity"].(string) != "high" {
		t.Errorf("applied minSeverity should be echoed as high, got %v", got["minSeverity"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestDBRowFormatAudit_EmptyResultNonNilSlice — when every InnoDB table is already
// on a modern Barracuda format, `candidates` is a non-nil empty slice (renders as
// []), and every scanned schema still surfaces in perDatabase.
func TestDBRowFormatAudit_EmptyResultNonNilSlice(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectQuery(regexp.QuoteMeta("FROM information_schema.TABLES")).
		WithArgs("acore_world", "acore_characters", "acore_auth").
		WillReturnRows(sqlmock.NewRows(rowFormatTablesCols).
			AddRow("acore_characters", "characters", "Dynamic", 100, 1000, 200).
			AddRow("acore_characters", "guild", "Compressed", 50, 500, 100))

	reg := NewRegistry()
	RegisterDBRowFormatAuditTool(reg, DBDeps{QueryDB: db})
	tool, _ := reg.Get("db_row_format_audit")
	resp := tool.Handler(context.Background(), json.RawMessage(`{}`), "")
	got := resp.(map[string]any)

	cands := got["candidates"].([]rowFormatLegacyRow)
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
	perDB := got["perDatabase"].([]rowFormatPerDatabase)
	if len(perDB) != 3 {
		t.Errorf("all 3 scanned schemas should surface, got %d", len(perDB))
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestDBRowFormatAudit_Truncation — more than the 500 cap flagged → candidates
// sliced to 500 + truncated:true, but totals stays the honest pre-cap count.
func TestDBRowFormatAudit_Truncation(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	rs := sqlmock.NewRows(rowFormatTablesCols)
	const legacy = dbRowFormatAuditMaxCandidates + 100 // 600
	// All Compact → all MEDIUM → all flagged (absolute classification, no dominant).
	for i := 0; i < legacy; i++ {
		rs.AddRow("acore_characters", "t_"+itoaSmall(i), "Compact", 10, 100, 10)
	}
	mock.ExpectQuery(regexp.QuoteMeta("FROM information_schema.TABLES")).
		WithArgs("acore_world", "acore_characters", "acore_auth").
		WillReturnRows(rs)

	reg := NewRegistry()
	RegisterDBRowFormatAuditTool(reg, DBDeps{QueryDB: db})
	tool, _ := reg.Get("db_row_format_audit")
	resp := tool.Handler(context.Background(), json.RawMessage(`{}`), "")
	got := resp.(map[string]any)

	cands := got["candidates"].([]rowFormatLegacyRow)
	if len(cands) != dbRowFormatAuditMaxCandidates {
		t.Errorf("candidates should be capped at %d, got %d", dbRowFormatAuditMaxCandidates, len(cands))
	}
	if !got["truncated"].(bool) {
		t.Errorf("truncated should be true")
	}
	totals := got["totals"].(map[string]any)
	if totals["legacyCount"].(int) != legacy {
		t.Errorf("totals.legacyCount should be honest pre-cap %d, got %v", legacy, totals["legacyCount"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestDBRowFormatAudit_PureHelpers covers the unit-testable helpers in isolation:
// row-format classification (all formats + case-insensitivity), severity ranking,
// and the shared severityName inverse this tool reuses.
func TestDBRowFormatAudit_PureHelpers(t *testing.T) {
	// classifyRowFormat: Antelope formats flagged, Barracuda + unknown not.
	if sev, leg := classifyRowFormat("Redundant"); !leg || sev != "high" {
		t.Errorf("Redundant want high,true got %q,%v", sev, leg)
	}
	if sev, leg := classifyRowFormat("redundant"); !leg || sev != "high" {
		t.Errorf("classifyRowFormat should be case-insensitive (redundant), got %q,%v", sev, leg)
	}
	if sev, leg := classifyRowFormat("Compact"); !leg || sev != "medium" {
		t.Errorf("Compact want medium,true got %q,%v", sev, leg)
	}
	for _, modern := range []string{"Dynamic", "DYNAMIC", "Compressed", "", "Paged"} {
		if sev, leg := classifyRowFormat(modern); leg || sev != "" {
			t.Errorf("%q should not be flagged, got %q,%v", modern, sev, leg)
		}
	}

	// rowFormatSeverityRank ordering: high < medium < low < other.
	if !(rowFormatSeverityRank("high") < rowFormatSeverityRank("medium") &&
		rowFormatSeverityRank("medium") < rowFormatSeverityRank("low") &&
		rowFormatSeverityRank("low") < rowFormatSeverityRank("other")) {
		t.Errorf("severity rank ordering wrong")
	}

	// severityName (shared helper) inverse — the ranks this tool emits map back.
	if severityName(0) != "high" || severityName(1) != "medium" || severityName(2) != "low" {
		t.Errorf("severityName mapping wrong")
	}
}
