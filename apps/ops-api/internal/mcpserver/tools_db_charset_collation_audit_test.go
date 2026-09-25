package mcpserver

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// charsetColumnsCols mirrors the SELECT column order in
// collectCharsetCollationAudit (TABLE_COLLATION + ENGINE already IFNULL'd to ”
// by the query).
var charsetColumnsCols = []string{
	"TABLE_SCHEMA", "TABLE_NAME", "COLUMN_NAME", "DATA_TYPE", "COLUMN_TYPE",
	"CHARACTER_SET_NAME", "COLLATION_NAME", "TABLE_COLLATION", "ENGINE",
}

// TestDBCharsetCollationAudit_DescriptionMentionsContext anchors the description
// to the keywords the agent uses to pick this tool. Same guard shape as the
// other db_* tools — keywords must live in the registered Description string.
func TestDBCharsetCollationAudit_DescriptionMentionsContext(t *testing.T) {
	reg := NewRegistry()
	RegisterDBCharsetCollationAuditTool(reg, DBDeps{})
	tool, ok := reg.Get("db_charset_collation_audit")
	if !ok {
		t.Fatal("db_charset_collation_audit not registered")
	}
	for _, kw := range []string{
		"information_schema.COLUMNS", "information_schema.TABLES",
		"CHARACTER_SET_NAME", "COLLATION_NAME", "Illegal mix of collations",
		"implicit", "JOIN", "dominant", "utf8mb3", "utf8mb4",
		"db_size_summary", "db_table_info", "SHOW CREATE TABLE",
		"CONVERT TO CHARACTER SET", "acore_playerbots", "ops_ro",
	} {
		if !strings.Contains(tool.Description, kw) {
			t.Errorf("description missing %q", kw)
		}
	}
}

func TestDBCharsetCollationAudit_ReadOnlyAnnotation(t *testing.T) {
	reg := NewRegistry()
	RegisterDBCharsetCollationAuditTool(reg, DBDeps{})
	tool, _ := reg.Get("db_charset_collation_audit")
	if !strings.Contains(string(tool.Annotations), `"readOnlyHint":true`) {
		t.Errorf("expected readOnlyHint:true, got %s", tool.Annotations)
	}
	if tool.Destructive {
		t.Errorf("destructive should be false")
	}
}

func TestDBCharsetCollationAudit_NoPoolReturnsError(t *testing.T) {
	reg := NewRegistry()
	RegisterDBCharsetCollationAuditTool(reg, DBDeps{})
	tool, _ := reg.Get("db_charset_collation_audit")
	resp := tool.Handler(context.Background(), json.RawMessage(`{}`), "")
	got, _ := resp.(map[string]any)
	if msg, _ := got["error"].(string); !strings.Contains(msg, "ops_ro pool not configured") {
		t.Errorf("expected pool-not-configured error, got %v", got)
	}
}

// TestDBCharsetCollationAudit_InvalidDatabaseRejected — an out-of-allowlist
// database is rejected with NO query run (a typo reads as "bad filter", not
// "empty schema").
func TestDBCharsetCollationAudit_InvalidDatabaseRejected(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	reg := NewRegistry()
	RegisterDBCharsetCollationAuditTool(reg, DBDeps{QueryDB: db})
	tool, _ := reg.Get("db_charset_collation_audit")
	resp := tool.Handler(context.Background(), json.RawMessage(`{"database":"acore_playerbots"}`), "")
	got, _ := resp.(map[string]any)
	if msg, _ := got["error"].(string); !strings.Contains(msg, "acore_world/acore_characters/acore_auth") {
		t.Errorf("expected allowlist error, got %v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("a query was issued despite the reject: %v", err)
	}
}

// TestDBCharsetCollationAudit_GoldenAggregation drives the default path (all
// three schemas) and verifies the full fold: dominant-pairing computation,
// high (cross-charset) vs medium (within-charset collation) classification,
// healthy columns skipped-but-counted, columnOverridesTable, worst-first sort,
// perDatabase counts + dominant pairing, totals.
func TestDBCharsetCollationAudit_GoldenAggregation(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	rs := sqlmock.NewRows(charsetColumnsCols).
		// acore_characters.characters — dominant utf8mb4/utf8mb4_general_ci (3 healthy),
		// one latin1 (HIGH cross-charset), one utf8mb4_unicode_ci (MEDIUM within-charset).
		AddRow("acore_characters", "characters", "name", "varchar", "varchar(12)", "utf8mb4", "utf8mb4_general_ci", "utf8mb4_general_ci", "InnoDB").
		AddRow("acore_characters", "characters", "subname", "varchar", "varchar(64)", "utf8mb4", "utf8mb4_general_ci", "utf8mb4_general_ci", "InnoDB").
		AddRow("acore_characters", "characters", "comment", "varchar", "varchar(32)", "utf8mb4", "utf8mb4_general_ci", "utf8mb4_general_ci", "InnoDB").
		AddRow("acore_characters", "characters", "legacy_name", "varchar", "varchar(12)", "latin1", "latin1_swedish_ci", "utf8mb4_general_ci", "InnoDB").  // HIGH
		AddRow("acore_characters", "characters", "search_key", "varchar", "varchar(32)", "utf8mb4", "utf8mb4_unicode_ci", "utf8mb4_general_ci", "InnoDB"). // MEDIUM
		// acore_world.creature_template — uniform utf8mb3 → nothing flagged.
		AddRow("acore_world", "creature_template", "name", "varchar", "varchar(100)", "utf8mb3", "utf8mb3_general_ci", "utf8mb3_general_ci", "MyISAM").
		AddRow("acore_world", "creature_template", "subname", "varchar", "varchar(100)", "utf8mb3", "utf8mb3_general_ci", "utf8mb3_general_ci", "MyISAM").
		// acore_auth.account — dominant utf8mb4_general_ci (2), one utf8mb4_bin (MEDIUM).
		AddRow("acore_auth", "account", "username", "varchar", "varchar(32)", "utf8mb4", "utf8mb4_general_ci", "utf8mb4_general_ci", "InnoDB").
		AddRow("acore_auth", "account", "email", "varchar", "varchar(255)", "utf8mb4", "utf8mb4_general_ci", "utf8mb4_general_ci", "InnoDB").
		AddRow("acore_auth", "account", "token", "varchar", "varchar(100)", "utf8mb4", "utf8mb4_bin", "utf8mb4_general_ci", "InnoDB") // MEDIUM

	mock.ExpectQuery(regexp.QuoteMeta("FROM information_schema.COLUMNS")).
		WithArgs("acore_world", "acore_characters", "acore_auth").
		WillReturnRows(rs)

	reg := NewRegistry()
	RegisterDBCharsetCollationAuditTool(reg, DBDeps{QueryDB: db})
	tool, _ := reg.Get("db_charset_collation_audit")
	resp := tool.Handler(context.Background(), json.RawMessage(`{}`), "")
	got, ok := resp.(map[string]any)
	if !ok {
		t.Fatalf("unexpected response shape: %T", resp)
	}
	if msg, hasErr := got["error"]; hasErr {
		t.Fatalf("unexpected error: %v", msg)
	}

	cands := got["candidates"].([]charsetMismatchRow)
	if len(cands) != 3 {
		t.Fatalf("expected 3 flagged columns, got %d: %+v", len(cands), cands)
	}
	// Worst-first: HIGH first; then MEDIUMs by database asc (acore_auth < acore_characters).
	c0 := cands[0]
	if c0.Database != "acore_characters" || c0.Table != "characters" || c0.Column != "legacy_name" || c0.Severity != "high" {
		t.Errorf("cands[0] should be acore_characters/characters/legacy_name/high: %+v", c0)
	}
	if c0.Charset != "latin1" || c0.DominantCharset != "utf8mb4" || !c0.ColumnOverridesTable {
		t.Errorf("cands[0] charset/dominant/override wrong: %+v", c0)
	}
	if !strings.Contains(c0.Reason, "Illegal mix of collations") {
		t.Errorf("cands[0] reason should mention the high-severity conversion risk: %q", c0.Reason)
	}
	c1 := cands[1]
	if c1.Database != "acore_auth" || c1.Column != "token" || c1.Severity != "medium" {
		t.Errorf("cands[1] should be acore_auth/account/token/medium: %+v", c1)
	}
	if c1.Collation != "utf8mb4_bin" || c1.DominantCollation != "utf8mb4_general_ci" {
		t.Errorf("cands[1] collation/dominant wrong: %+v", c1)
	}
	c2 := cands[2]
	if c2.Database != "acore_characters" || c2.Column != "search_key" || c2.Severity != "medium" {
		t.Errorf("cands[2] should be acore_characters/characters/search_key/medium: %+v", c2)
	}
	if c2.Collation != "utf8mb4_unicode_ci" || c2.Charset != "utf8mb4" {
		t.Errorf("cands[2] collation/charset wrong: %+v", c2)
	}

	totals := got["totals"].(map[string]any)
	if totals["stringColumnsScanned"].(int) != 10 {
		t.Errorf("totals.stringColumnsScanned want 10, got %v", totals["stringColumnsScanned"])
	}
	if totals["mismatchCount"].(int) != 3 {
		t.Errorf("totals.mismatchCount want 3, got %v", totals["mismatchCount"])
	}

	perDB := got["perDatabase"].([]charsetPerDatabase)
	if len(perDB) != 3 {
		t.Fatalf("expected 3 perDatabase rows, got %d", len(perDB))
	}
	// Sorted by mismatchCount desc, schema asc: characters(2), auth(1), world(0).
	if perDB[0].Database != "acore_characters" || perDB[0].MismatchCount != 2 ||
		perDB[0].StringColumnsScanned != 5 || perDB[0].DistinctCharsets != 2 ||
		perDB[0].DistinctCollations != 3 || perDB[0].DominantCharset != "utf8mb4" ||
		perDB[0].DominantCollation != "utf8mb4_general_ci" {
		t.Errorf("perDB[0] acore_characters rollup wrong: %+v", perDB[0])
	}
	if perDB[1].Database != "acore_auth" || perDB[1].MismatchCount != 1 || perDB[1].StringColumnsScanned != 3 {
		t.Errorf("perDB[1] want acore_auth 1/3: %+v", perDB[1])
	}
	if perDB[2].Database != "acore_world" || perDB[2].MismatchCount != 0 ||
		perDB[2].StringColumnsScanned != 2 || perDB[2].DominantCharset != "utf8mb3" {
		t.Errorf("perDB[2] want acore_world 0/2 dominant utf8mb3: %+v", perDB[2])
	}

	if got["truncated"].(bool) {
		t.Errorf("truncated should be false for 3 candidates")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestDBCharsetCollationAudit_DatabaseFilterNarrowsScan verifies `database`
// narrows the IN clause to a single bind.
func TestDBCharsetCollationAudit_DatabaseFilterNarrowsScan(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectQuery(regexp.QuoteMeta("FROM information_schema.COLUMNS")).
		WithArgs("acore_world"). // single bind — IN list narrowed to one schema
		WillReturnRows(sqlmock.NewRows(charsetColumnsCols).
			AddRow("acore_world", "item_template", "name", "varchar", "varchar(255)", "utf8mb3", "utf8mb3_general_ci", "utf8mb3_general_ci", "MyISAM").
			AddRow("acore_world", "item_template", "description", "varchar", "varchar(255)", "utf8mb4", "utf8mb4_general_ci", "utf8mb3_general_ci", "MyISAM")) // HIGH (latin... utf8mb4 minority)

	reg := NewRegistry()
	RegisterDBCharsetCollationAuditTool(reg, DBDeps{QueryDB: db})
	tool, _ := reg.Get("db_charset_collation_audit")
	resp := tool.Handler(context.Background(), json.RawMessage(`{"database":"acore_world"}`), "")
	got := resp.(map[string]any)
	if msg, hasErr := got["error"]; hasErr {
		t.Fatalf("unexpected error: %v", msg)
	}
	scanned := got["scannedSchemas"].([]string)
	if len(scanned) != 1 || scanned[0] != "acore_world" {
		t.Errorf("scannedSchemas want [acore_world], got %v", scanned)
	}
	perDB := got["perDatabase"].([]charsetPerDatabase)
	if len(perDB) != 1 || perDB[0].Database != "acore_world" {
		t.Errorf("perDatabase want one acore_world row, got %+v", perDB)
	}
	// utf8mb3 is dominant (it appears first → tie? no, both count 1 → tie broken
	// by lexicographically smaller: "utf8mb3" < "utf8mb4", so utf8mb3 dominant,
	// the utf8mb4 column flags HIGH).
	cands := got["candidates"].([]charsetMismatchRow)
	if len(cands) != 1 || cands[0].Column != "description" || cands[0].Severity != "high" {
		t.Errorf("expected the utf8mb4 column flagged high, got %+v", cands)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestDBCharsetCollationAudit_EmptyResultNonNilSlice — when every column matches
// its schema's dominant pairing, `candidates` is a non-nil empty slice (renders
// as []), and every scanned schema still surfaces in perDatabase.
func TestDBCharsetCollationAudit_EmptyResultNonNilSlice(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectQuery(regexp.QuoteMeta("FROM information_schema.COLUMNS")).
		WithArgs("acore_world", "acore_characters", "acore_auth").
		WillReturnRows(sqlmock.NewRows(charsetColumnsCols).
			AddRow("acore_characters", "characters", "name", "varchar", "varchar(12)", "utf8mb4", "utf8mb4_general_ci", "utf8mb4_general_ci", "InnoDB").
			AddRow("acore_characters", "characters", "subname", "varchar", "varchar(64)", "utf8mb4", "utf8mb4_general_ci", "utf8mb4_general_ci", "InnoDB"))

	reg := NewRegistry()
	RegisterDBCharsetCollationAuditTool(reg, DBDeps{QueryDB: db})
	tool, _ := reg.Get("db_charset_collation_audit")
	resp := tool.Handler(context.Background(), json.RawMessage(`{}`), "")
	got := resp.(map[string]any)

	cands := got["candidates"].([]charsetMismatchRow)
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
	perDB := got["perDatabase"].([]charsetPerDatabase)
	if len(perDB) != 3 {
		t.Errorf("all 3 scanned schemas should surface, got %d", len(perDB))
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestDBCharsetCollationAudit_Truncation — more than the 500 cap flagged →
// candidates sliced to 500 + truncated:true, but totals stays the honest
// pre-cap count.
func TestDBCharsetCollationAudit_Truncation(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	rs := sqlmock.NewRows(charsetColumnsCols)
	const deviating = dbCharsetCollationAuditMaxCandidates + 100 // 600
	// dominant population must strictly outnumber the deviating one so the
	// dominant pairing stays utf8mb4_general_ci (601 > 600).
	for i := 0; i <= deviating; i++ {
		rs.AddRow("acore_world", "t_big", "dom_"+itoaSmall(i), "varchar", "varchar(32)", "utf8mb4", "utf8mb4_general_ci", "utf8mb4_general_ci", "InnoDB")
	}
	for i := 0; i < deviating; i++ {
		rs.AddRow("acore_world", "t_big", "dev_"+itoaSmall(i), "varchar", "varchar(32)", "utf8mb4", "utf8mb4_unicode_ci", "utf8mb4_general_ci", "InnoDB")
	}
	mock.ExpectQuery(regexp.QuoteMeta("FROM information_schema.COLUMNS")).
		WithArgs("acore_world", "acore_characters", "acore_auth").
		WillReturnRows(rs)

	reg := NewRegistry()
	RegisterDBCharsetCollationAuditTool(reg, DBDeps{QueryDB: db})
	tool, _ := reg.Get("db_charset_collation_audit")
	resp := tool.Handler(context.Background(), json.RawMessage(`{}`), "")
	got := resp.(map[string]any)

	cands := got["candidates"].([]charsetMismatchRow)
	if len(cands) != dbCharsetCollationAuditMaxCandidates {
		t.Errorf("candidates should be capped at %d, got %d", dbCharsetCollationAuditMaxCandidates, len(cands))
	}
	if !got["truncated"].(bool) {
		t.Errorf("truncated should be true")
	}
	totals := got["totals"].(map[string]any)
	if totals["mismatchCount"].(int) != deviating {
		t.Errorf("totals.mismatchCount should be honest pre-cap %d, got %v", deviating, totals["mismatchCount"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestDBCharsetCollationAudit_PureHelpers covers the unit-testable helpers in
// isolation: dominant-string selection + tiebreak, classification, severity
// ranking.
func TestDBCharsetCollationAudit_PureHelpers(t *testing.T) {
	// dominantString: clear winner.
	if got := dominantString(map[string]int{"utf8mb4": 5, "latin1": 2}); got != "utf8mb4" {
		t.Errorf("dominantString clear winner want utf8mb4, got %q", got)
	}
	// Tie → lexicographically smallest key.
	if got := dominantString(map[string]int{"utf8mb4": 3, "latin1": 3}); got != "latin1" {
		t.Errorf("dominantString tie want latin1 (lexicographically smaller), got %q", got)
	}
	// Empty map → "".
	if got := dominantString(map[string]int{}); got != "" {
		t.Errorf("dominantString empty want \"\", got %q", got)
	}

	// classifyCharsetCollation: cross-charset = high.
	if sev, m := classifyCharsetCollation("latin1", "latin1_swedish_ci", "utf8mb4", "utf8mb4_general_ci"); !m || sev != "high" {
		t.Errorf("cross-charset want high,true got %q,%v", sev, m)
	}
	// Same charset, different collation = medium.
	if sev, m := classifyCharsetCollation("utf8mb4", "utf8mb4_bin", "utf8mb4", "utf8mb4_general_ci"); !m || sev != "medium" {
		t.Errorf("within-charset collation diff want medium,true got %q,%v", sev, m)
	}
	// Exact match = healthy, not flagged.
	if sev, m := classifyCharsetCollation("utf8mb4", "utf8mb4_general_ci", "utf8mb4", "utf8mb4_general_ci"); m || sev != "" {
		t.Errorf("match want \"\",false got %q,%v", sev, m)
	}

	if charsetSeverityRank("high") >= charsetSeverityRank("medium") {
		t.Errorf("high should rank before medium")
	}
	if charsetSeverityRank("medium") >= charsetSeverityRank("other") {
		t.Errorf("medium should rank before unknown")
	}
}
