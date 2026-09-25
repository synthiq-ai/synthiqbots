package mcpserver

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// engineTablesCols mirrors the SELECT column order in collectEngineDistribution
// (ENGINE / TABLE_ROWS / DATA_LENGTH / INDEX_LENGTH already IFNULL'd by the query).
var engineTablesCols = []string{
	"TABLE_SCHEMA", "TABLE_NAME", "ENGINE", "TABLE_ROWS", "DATA_LENGTH", "INDEX_LENGTH",
}

// TestDBEngineDistribution_DescriptionMentionsContext anchors the description to
// the keywords the agent uses to pick this tool. Same guard shape as the other
// db_* tools — keywords must live in the registered Description string.
func TestDBEngineDistribution_DescriptionMentionsContext(t *testing.T) {
	reg := NewRegistry()
	RegisterDBEngineDistributionTool(reg, DBDeps{})
	tool, ok := reg.Get("db_engine_distribution")
	if !ok {
		t.Fatal("db_engine_distribution not registered")
	}
	for _, kw := range []string{
		"information_schema.TABLES", "ENGINE", "InnoDB", "MyISAM",
		"crash recovery", "table-level locking", "transaction", "dominant",
		"ALTER TABLE", "ENGINE=InnoDB", "SHOW CREATE TABLE",
		"db_size_summary", "db_table_info", "acore_world",
		"acore_playerbots", "ops_ro", "minSeverity", "Read-only",
	} {
		if !strings.Contains(tool.Description, kw) {
			t.Errorf("description missing %q", kw)
		}
	}
}

func TestDBEngineDistribution_ReadOnlyAnnotation(t *testing.T) {
	reg := NewRegistry()
	RegisterDBEngineDistributionTool(reg, DBDeps{})
	tool, _ := reg.Get("db_engine_distribution")
	if !strings.Contains(string(tool.Annotations), `"readOnlyHint":true`) {
		t.Errorf("expected readOnlyHint:true, got %s", tool.Annotations)
	}
	if tool.Destructive {
		t.Errorf("destructive should be false")
	}
}

func TestDBEngineDistribution_NoPoolReturnsError(t *testing.T) {
	reg := NewRegistry()
	RegisterDBEngineDistributionTool(reg, DBDeps{})
	tool, _ := reg.Get("db_engine_distribution")
	resp := tool.Handler(context.Background(), json.RawMessage(`{}`), "")
	got, _ := resp.(map[string]any)
	if msg, _ := got["error"].(string); !strings.Contains(msg, "ops_ro pool not configured") {
		t.Errorf("expected pool-not-configured error, got %v", got)
	}
}

// TestDBEngineDistribution_InvalidDatabaseRejected — an out-of-allowlist database
// is rejected with NO query run (a typo reads as "bad filter", not "empty schema").
func TestDBEngineDistribution_InvalidDatabaseRejected(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	reg := NewRegistry()
	RegisterDBEngineDistributionTool(reg, DBDeps{QueryDB: db})
	tool, _ := reg.Get("db_engine_distribution")
	resp := tool.Handler(context.Background(), json.RawMessage(`{"database":"acore_playerbots"}`), "")
	got, _ := resp.(map[string]any)
	if msg, _ := got["error"].(string); !strings.Contains(msg, "acore_world/acore_characters/acore_auth") {
		t.Errorf("expected allowlist error, got %v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("a query was issued despite the reject: %v", err)
	}
}

// TestDBEngineDistribution_InvalidMinSeverityRejected — an out-of-range
// minSeverity is rejected with NO query run (reject-not-clamp, like the
// minQuality/maxSelectivity precedent).
func TestDBEngineDistribution_InvalidMinSeverityRejected(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	reg := NewRegistry()
	RegisterDBEngineDistributionTool(reg, DBDeps{QueryDB: db})
	tool, _ := reg.Get("db_engine_distribution")
	resp := tool.Handler(context.Background(), json.RawMessage(`{"minSeverity":"critical"}`), "")
	got, _ := resp.(map[string]any)
	if msg, _ := got["error"].(string); !strings.Contains(msg, "minSeverity must be one of low/medium/high") {
		t.Errorf("expected minSeverity reject error, got %v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("a query was issued despite the reject: %v", err)
	}
}

// TestDBEngineDistribution_GoldenAggregation drives the default path (all three
// schemas) and verifies the full fold: per-schema dominant-engine computation,
// high (non-crash-safe in InnoDB schema) / medium (non-safe deviation in a
// MyISAM schema) / low (crash-safe in a MyISAM schema) classification,
// engineBreakdown ordering, worst-first candidate sort (severity then bytes),
// perDatabase counts, totals.
func TestDBEngineDistribution_GoldenAggregation(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	rs := sqlmock.NewRows(engineTablesCols).
		// acore_characters — dominant InnoDB (2), one MyISAM straggler (HIGH).
		AddRow("acore_characters", "characters", "InnoDB", 100, 1000, 200).
		AddRow("acore_characters", "guild", "InnoDB", 50, 500, 100).
		AddRow("acore_characters", "logs", "MyISAM", 9999, 8000, 1000). // HIGH (total 9000)
		// acore_auth — dominant InnoDB (2), one MEMORY straggler (HIGH).
		AddRow("acore_auth", "account", "InnoDB", 10, 300, 50).
		AddRow("acore_auth", "account_access", "InnoDB", 4, 80, 10).
		AddRow("acore_auth", "account_banned", "MEMORY", 5, 100, 0). // HIGH (total 100)
		// acore_world — dominant MyISAM (3, by design); InnoDB (LOW) + MEMORY (MEDIUM).
		AddRow("acore_world", "creature_template", "MyISAM", 1000, 50000, 5000).
		AddRow("acore_world", "item_template", "MyISAM", 2000, 80000, 8000).
		AddRow("acore_world", "quest_template", "MyISAM", 1500, 60000, 6000).
		AddRow("acore_world", "playercreateinfo", "InnoDB", 30, 400, 40). // LOW (total 440)
		AddRow("acore_world", "spell_dbc_cache", "MEMORY", 200, 600, 0)   // MEDIUM (total 600)

	mock.ExpectQuery(regexp.QuoteMeta("FROM information_schema.TABLES")).
		WithArgs("acore_world", "acore_characters", "acore_auth").
		WillReturnRows(rs)

	reg := NewRegistry()
	RegisterDBEngineDistributionTool(reg, DBDeps{QueryDB: db})
	tool, _ := reg.Get("db_engine_distribution")
	resp := tool.Handler(context.Background(), json.RawMessage(`{}`), "")
	got, ok := resp.(map[string]any)
	if !ok {
		t.Fatalf("unexpected response shape: %T", resp)
	}
	if msg, hasErr := got["error"]; hasErr {
		t.Fatalf("unexpected error: %v", msg)
	}

	cands := got["candidates"].([]engineDeviationRow)
	if len(cands) != 4 {
		t.Fatalf("expected 4 flagged tables, got %d: %+v", len(cands), cands)
	}
	// Worst-first: HIGH (by bytes desc), then MEDIUM, then LOW.
	if cands[0].Database != "acore_characters" || cands[0].Table != "logs" ||
		cands[0].Severity != "high" || cands[0].Engine != "MyISAM" ||
		cands[0].DominantEngine != "InnoDB" || cands[0].TotalBytes != 9000 {
		t.Errorf("cands[0] should be acore_characters/logs MyISAM HIGH total 9000: %+v", cands[0])
	}
	if !strings.Contains(cands[0].Reason, "ENGINE=InnoDB") {
		t.Errorf("cands[0] reason should suggest the migration: %q", cands[0].Reason)
	}
	if cands[1].Database != "acore_auth" || cands[1].Table != "account_banned" ||
		cands[1].Severity != "high" || cands[1].Engine != "MEMORY" {
		t.Errorf("cands[1] should be acore_auth/account_banned MEMORY HIGH: %+v", cands[1])
	}
	if cands[2].Database != "acore_world" || cands[2].Table != "spell_dbc_cache" ||
		cands[2].Severity != "medium" || cands[2].DominantEngine != "MyISAM" {
		t.Errorf("cands[2] should be acore_world/spell_dbc_cache MEDIUM (dominant MyISAM): %+v", cands[2])
	}
	if cands[3].Database != "acore_world" || cands[3].Table != "playercreateinfo" ||
		cands[3].Severity != "low" || cands[3].Engine != "InnoDB" {
		t.Errorf("cands[3] should be acore_world/playercreateinfo InnoDB LOW: %+v", cands[3])
	}
	if !strings.Contains(cands[3].Reason, "SAFER than the schema norm") {
		t.Errorf("cands[3] low reason should be reassuring/informational: %q", cands[3].Reason)
	}

	totals := got["totals"].(map[string]any)
	if totals["tablesScanned"].(int) != 11 {
		t.Errorf("totals.tablesScanned want 11, got %v", totals["tablesScanned"])
	}
	if totals["deviatingCount"].(int) != 4 {
		t.Errorf("totals.deviatingCount want 4, got %v", totals["deviatingCount"])
	}

	// No minSeverity applied → the key is omitted.
	if _, present := got["minSeverity"]; present {
		t.Errorf("minSeverity should be omitted when not set, got %v", got["minSeverity"])
	}

	perDB := got["perDatabase"].([]enginePerDatabase)
	if len(perDB) != 3 {
		t.Fatalf("expected 3 perDatabase rows, got %d", len(perDB))
	}
	// Sorted deviatingCount desc, schema asc: world(2), auth(1), characters(1).
	if perDB[0].Database != "acore_world" || perDB[0].DeviatingCount != 2 ||
		perDB[0].TablesScanned != 5 || perDB[0].DominantEngine != "MyISAM" ||
		perDB[0].DistinctEngines != 3 {
		t.Errorf("perDB[0] acore_world rollup wrong: %+v", perDB[0])
	}
	// engineBreakdown sorted tableCount desc: MyISAM(3), then InnoDB/MEMORY engine-asc.
	if len(perDB[0].EngineBreakdown) != 3 ||
		perDB[0].EngineBreakdown[0].Engine != "MyISAM" || perDB[0].EngineBreakdown[0].TableCount != 3 ||
		perDB[0].EngineBreakdown[1].Engine != "InnoDB" || perDB[0].EngineBreakdown[2].Engine != "MEMORY" {
		t.Errorf("perDB[0] engineBreakdown order wrong: %+v", perDB[0].EngineBreakdown)
	}
	// MyISAM bytes summed across 3 world tables: (50000+5000)+(80000+8000)+(60000+6000)=209000.
	if perDB[0].EngineBreakdown[0].TotalBytes != 209000 {
		t.Errorf("perDB[0] MyISAM totalBytes want 209000, got %d", perDB[0].EngineBreakdown[0].TotalBytes)
	}
	if perDB[1].Database != "acore_auth" || perDB[1].DeviatingCount != 1 ||
		perDB[1].DominantEngine != "InnoDB" {
		t.Errorf("perDB[1] want acore_auth 1 dominant InnoDB: %+v", perDB[1])
	}
	if perDB[2].Database != "acore_characters" || perDB[2].DeviatingCount != 1 ||
		perDB[2].TablesScanned != 3 || perDB[2].DominantEngine != "InnoDB" {
		t.Errorf("perDB[2] want acore_characters 1/3 dominant InnoDB: %+v", perDB[2])
	}

	if got["truncated"].(bool) {
		t.Errorf("truncated should be false for 4 candidates")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestDBEngineDistribution_DatabaseFilterNarrowsScan verifies `database` narrows
// the IN clause to a single bind.
func TestDBEngineDistribution_DatabaseFilterNarrowsScan(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectQuery(regexp.QuoteMeta("FROM information_schema.TABLES")).
		WithArgs("acore_characters"). // single bind — IN list narrowed to one schema
		WillReturnRows(sqlmock.NewRows(engineTablesCols).
			AddRow("acore_characters", "characters", "InnoDB", 100, 1000, 200).
			AddRow("acore_characters", "guild", "InnoDB", 50, 500, 100).
			AddRow("acore_characters", "logs", "MyISAM", 9999, 8000, 1000)) // HIGH

	reg := NewRegistry()
	RegisterDBEngineDistributionTool(reg, DBDeps{QueryDB: db})
	tool, _ := reg.Get("db_engine_distribution")
	resp := tool.Handler(context.Background(), json.RawMessage(`{"database":"acore_characters"}`), "")
	got := resp.(map[string]any)
	if msg, hasErr := got["error"]; hasErr {
		t.Fatalf("unexpected error: %v", msg)
	}
	scanned := got["scannedSchemas"].([]string)
	if len(scanned) != 1 || scanned[0] != "acore_characters" {
		t.Errorf("scannedSchemas want [acore_characters], got %v", scanned)
	}
	perDB := got["perDatabase"].([]enginePerDatabase)
	if len(perDB) != 1 || perDB[0].Database != "acore_characters" {
		t.Errorf("perDatabase want one acore_characters row, got %+v", perDB)
	}
	cands := got["candidates"].([]engineDeviationRow)
	if len(cands) != 1 || cands[0].Table != "logs" || cands[0].Severity != "high" {
		t.Errorf("expected the MyISAM table flagged high, got %+v", cands)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestDBEngineDistribution_MinSeverityFilter — minSeverity=high trims the
// candidate list to the high-severity stragglers only, while perDatabase /
// totals stay the honest full deviation counts.
func TestDBEngineDistribution_MinSeverityFilter(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectQuery(regexp.QuoteMeta("FROM information_schema.TABLES")).
		WithArgs("acore_world", "acore_characters", "acore_auth").
		WillReturnRows(sqlmock.NewRows(engineTablesCols).
			AddRow("acore_characters", "characters", "InnoDB", 100, 1000, 200).
			AddRow("acore_characters", "logs", "MyISAM", 9999, 8000, 1000). // HIGH
			// acore_world MyISAM-dominant: one LOW (InnoDB) + one MEDIUM (MEMORY).
			AddRow("acore_world", "creature_template", "MyISAM", 1000, 50000, 5000).
			AddRow("acore_world", "item_template", "MyISAM", 2000, 80000, 8000).
			AddRow("acore_world", "playercreateinfo", "InnoDB", 30, 400, 40). // LOW
			AddRow("acore_world", "spell_dbc_cache", "MEMORY", 200, 600, 0))  // MEDIUM

	reg := NewRegistry()
	RegisterDBEngineDistributionTool(reg, DBDeps{QueryDB: db})
	tool, _ := reg.Get("db_engine_distribution")
	resp := tool.Handler(context.Background(), json.RawMessage(`{"minSeverity":"high"}`), "")
	got := resp.(map[string]any)
	if msg, hasErr := got["error"]; hasErr {
		t.Fatalf("unexpected error: %v", msg)
	}

	cands := got["candidates"].([]engineDeviationRow)
	if len(cands) != 1 || cands[0].Table != "logs" || cands[0].Severity != "high" {
		t.Errorf("minSeverity=high should list only the high straggler, got %+v", cands)
	}
	// totals stays honest: 3 deviations total (1 high + 1 low + 1 medium).
	totals := got["totals"].(map[string]any)
	if totals["deviatingCount"].(int) != 3 {
		t.Errorf("totals.deviatingCount should be honest 3, got %v", totals["deviatingCount"])
	}
	// perDatabase deviatingCount also honest: acore_world keeps its 2 deviations.
	perDB := got["perDatabase"].([]enginePerDatabase)
	if perDB[0].Database != "acore_world" || perDB[0].DeviatingCount != 2 {
		t.Errorf("perDB[0] acore_world should keep its honest 2 deviations, got %+v", perDB[0])
	}
	if got["minSeverity"].(string) != "high" {
		t.Errorf("applied minSeverity should be echoed as high, got %v", got["minSeverity"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestDBEngineDistribution_EmptyResultNonNilSlice — when every table matches its
// schema's dominant engine, `candidates` is a non-nil empty slice (renders as
// []), and every scanned schema still surfaces in perDatabase.
func TestDBEngineDistribution_EmptyResultNonNilSlice(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectQuery(regexp.QuoteMeta("FROM information_schema.TABLES")).
		WithArgs("acore_world", "acore_characters", "acore_auth").
		WillReturnRows(sqlmock.NewRows(engineTablesCols).
			AddRow("acore_characters", "characters", "InnoDB", 100, 1000, 200).
			AddRow("acore_characters", "guild", "InnoDB", 50, 500, 100))

	reg := NewRegistry()
	RegisterDBEngineDistributionTool(reg, DBDeps{QueryDB: db})
	tool, _ := reg.Get("db_engine_distribution")
	resp := tool.Handler(context.Background(), json.RawMessage(`{}`), "")
	got := resp.(map[string]any)

	cands := got["candidates"].([]engineDeviationRow)
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
	perDB := got["perDatabase"].([]enginePerDatabase)
	if len(perDB) != 3 {
		t.Errorf("all 3 scanned schemas should surface, got %d", len(perDB))
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestDBEngineDistribution_Truncation — more than the 500 cap flagged →
// candidates sliced to 500 + truncated:true, but totals stays the honest pre-cap
// count.
func TestDBEngineDistribution_Truncation(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	rs := sqlmock.NewRows(engineTablesCols)
	const deviating = dbEngineDistributionMaxCandidates + 100 // 600
	// dominant population (InnoDB) must strictly outnumber the deviating MyISAM
	// tables so InnoDB stays dominant (601 > 600).
	for i := 0; i <= deviating; i++ {
		rs.AddRow("acore_characters", "dom_"+itoaSmall(i), "InnoDB", 10, 100, 10)
	}
	for i := 0; i < deviating; i++ {
		rs.AddRow("acore_characters", "dev_"+itoaSmall(i), "MyISAM", 10, 100, 10)
	}
	mock.ExpectQuery(regexp.QuoteMeta("FROM information_schema.TABLES")).
		WithArgs("acore_world", "acore_characters", "acore_auth").
		WillReturnRows(rs)

	reg := NewRegistry()
	RegisterDBEngineDistributionTool(reg, DBDeps{QueryDB: db})
	tool, _ := reg.Get("db_engine_distribution")
	resp := tool.Handler(context.Background(), json.RawMessage(`{}`), "")
	got := resp.(map[string]any)

	cands := got["candidates"].([]engineDeviationRow)
	if len(cands) != dbEngineDistributionMaxCandidates {
		t.Errorf("candidates should be capped at %d, got %d", dbEngineDistributionMaxCandidates, len(cands))
	}
	if !got["truncated"].(bool) {
		t.Errorf("truncated should be true")
	}
	totals := got["totals"].(map[string]any)
	if totals["deviatingCount"].(int) != deviating {
		t.Errorf("totals.deviatingCount should be honest pre-cap %d, got %v", deviating, totals["deviatingCount"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestDBEngineDistribution_PureHelpers covers the unit-testable helpers in
// isolation: crash-safe detection, deviation classification (all directions),
// severity ranking, severity-name inverse.
func TestDBEngineDistribution_PureHelpers(t *testing.T) {
	// isCrashSafeEngine: only InnoDB, case-insensitive.
	if !isCrashSafeEngine("InnoDB") || !isCrashSafeEngine("innodb") {
		t.Errorf("InnoDB should be crash-safe (case-insensitive)")
	}
	if isCrashSafeEngine("MyISAM") || isCrashSafeEngine("MEMORY") || isCrashSafeEngine("") {
		t.Errorf("non-InnoDB engines should not be crash-safe")
	}

	// classifyEngineDeviation: match = not flagged.
	if sev, dev := classifyEngineDeviation("InnoDB", "InnoDB"); dev || sev != "" {
		t.Errorf("matching engine want \"\",false got %q,%v", sev, dev)
	}
	// non-crash-safe in InnoDB-dominant schema = high.
	if sev, dev := classifyEngineDeviation("MyISAM", "InnoDB"); !dev || sev != "high" {
		t.Errorf("MyISAM vs InnoDB-dominant want high,true got %q,%v", sev, dev)
	}
	// crash-safe in MyISAM-dominant schema = low (safer than norm).
	if sev, dev := classifyEngineDeviation("InnoDB", "MyISAM"); !dev || sev != "low" {
		t.Errorf("InnoDB vs MyISAM-dominant want low,true got %q,%v", sev, dev)
	}
	// non-safe-vs-non-safe deviation = medium.
	if sev, dev := classifyEngineDeviation("MEMORY", "MyISAM"); !dev || sev != "medium" {
		t.Errorf("MEMORY vs MyISAM-dominant want medium,true got %q,%v", sev, dev)
	}

	// engineSeverityRank ordering: high < medium < low < other.
	if !(engineSeverityRank("high") < engineSeverityRank("medium") &&
		engineSeverityRank("medium") < engineSeverityRank("low") &&
		engineSeverityRank("low") < engineSeverityRank("other")) {
		t.Errorf("severity rank ordering wrong")
	}

	// severityName inverse.
	if severityName(0) != "high" || severityName(1) != "medium" || severityName(2) != "low" {
		t.Errorf("severityName mapping wrong")
	}
}
