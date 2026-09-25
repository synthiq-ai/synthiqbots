package mcpserver

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// indexKeyLenStatsCols mirrors the SELECT column order in
// collectIndexKeyLengthAudit (SUB_PART / ROW_FORMAT / CHARACTER_*_LENGTH already
// IFNULL'd by the query). One row per index column.
var indexKeyLenStatsCols = []string{
	"TABLE_SCHEMA", "TABLE_NAME", "INDEX_NAME", "SEQ_IN_INDEX", "COLUMN_NAME",
	"SUB_PART", "NON_UNIQUE", "INDEX_TYPE", "ROW_FORMAT",
	"DATA_TYPE", "CHARACTER_MAXIMUM_LENGTH", "CHARACTER_OCTET_LENGTH",
}

// TestDBIndexKeyLengthAudit_DescriptionMentionsContext anchors the description to
// the keywords the agent uses to pick this tool. Same guard shape as the other
// db_* tools — keywords must live in the registered Description string.
func TestDBIndexKeyLengthAudit_DescriptionMentionsContext(t *testing.T) {
	reg := NewRegistry()
	RegisterDBIndexKeyLengthAuditTool(reg, DBDeps{})
	tool, ok := reg.Get("db_index_key_length_audit")
	if !ok {
		t.Fatal("db_index_key_length_audit not registered")
	}
	for _, kw := range []string{
		"information_schema.STATISTICS", "information_schema.COLUMNS",
		"CHARACTER_OCTET_LENGTH", "SUB_PART", "ROW_FORMAT",
		"InnoDB", "Antelope", "Barracuda", "DYNAMIC", "COMPACT",
		"767", "3072", "innodb_large_prefix", "utf8mb4", "VARCHAR(192)",
		"prefix index", "col(191)", "db_row_format_audit", "db_low_cardinality_indexes",
		"SHOW CREATE TABLE", "acore_playerbots", "ops_ro", "minSeverity", "Read-only",
	} {
		if !strings.Contains(tool.Description, kw) {
			t.Errorf("description missing %q", kw)
		}
	}
}

func TestDBIndexKeyLengthAudit_ReadOnlyAnnotation(t *testing.T) {
	reg := NewRegistry()
	RegisterDBIndexKeyLengthAuditTool(reg, DBDeps{})
	tool, _ := reg.Get("db_index_key_length_audit")
	if !strings.Contains(string(tool.Annotations), `"readOnlyHint":true`) {
		t.Errorf("expected readOnlyHint:true, got %s", tool.Annotations)
	}
	if tool.Destructive {
		t.Errorf("destructive should be false")
	}
}

func TestDBIndexKeyLengthAudit_NoPoolReturnsError(t *testing.T) {
	reg := NewRegistry()
	RegisterDBIndexKeyLengthAuditTool(reg, DBDeps{})
	tool, _ := reg.Get("db_index_key_length_audit")
	resp := tool.Handler(context.Background(), json.RawMessage(`{}`), "")
	got, _ := resp.(map[string]any)
	if msg, _ := got["error"].(string); !strings.Contains(msg, "ops_ro pool not configured") {
		t.Errorf("expected pool-not-configured error, got %v", got)
	}
}

// TestDBIndexKeyLengthAudit_InvalidDatabaseRejected — an out-of-allowlist database
// is rejected with NO query run (a typo reads as "bad filter", not "empty schema").
func TestDBIndexKeyLengthAudit_InvalidDatabaseRejected(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	reg := NewRegistry()
	RegisterDBIndexKeyLengthAuditTool(reg, DBDeps{QueryDB: db})
	tool, _ := reg.Get("db_index_key_length_audit")
	resp := tool.Handler(context.Background(), json.RawMessage(`{"database":"acore_playerbots"}`), "")
	got, _ := resp.(map[string]any)
	if msg, _ := got["error"].(string); !strings.Contains(msg, "acore_world/acore_characters/acore_auth") {
		t.Errorf("expected allowlist error, got %v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("a query was issued despite the reject: %v", err)
	}
}

// TestDBIndexKeyLengthAudit_InvalidMinSeverityRejected — an out-of-range
// minSeverity is rejected with NO query run (reject-not-clamp).
func TestDBIndexKeyLengthAudit_InvalidMinSeverityRejected(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	reg := NewRegistry()
	RegisterDBIndexKeyLengthAuditTool(reg, DBDeps{QueryDB: db})
	tool, _ := reg.Get("db_index_key_length_audit")
	resp := tool.Handler(context.Background(), json.RawMessage(`{"minSeverity":"low"}`), "")
	got, _ := resp.(map[string]any)
	if msg, _ := got["error"].(string); !strings.Contains(msg, "minSeverity must be one of medium/high") {
		t.Errorf("expected minSeverity reject error, got %v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("a query was issued despite the reject: %v", err)
	}
}

// goldenIndexKeyLenRows is the shared fixture for the default-path + filter tests.
// Column-level rows, ordered by schema/table/index/seq exactly as the query emits.
// Byte arithmetic per index (bytes-per-char from octet/max):
//
//	acore_auth.account PRIMARY (id int)                  -> 4    OK
//	acore_auth.account idx_username (username vc32 mb4)  -> 128  OK
//	acore_characters.charlog idx_legacy (note vc255 mb4, Compact) -> 1020 > 767 effLimit -> HIGH
//	acore_characters.composite idx_multi (cname vc190 mb4=760 + acctid bigint=8) Dynamic -> 768 -> MEDIUM
//	acore_characters.decimal_tbl idx_amt (amount DECIMAL[unsized] + note vc255 mb4=1020) Dynamic -> 1020 lowerBound MEDIUM
//	acore_characters.item_text idx_note_full (note vc255 mb4) Dynamic -> 1020 -> MEDIUM
//	acore_characters.name_tbl idx_name_prefix (name vc255 mb4, SUB_PART 20) Dynamic -> 80 OK (prefix sized)
//	acore_world.spell idx_name_latin1 (name vc500 latin1) Dynamic -> 500 OK (bytes-per-char 1)
func goldenIndexKeyLenRows() *sqlmock.Rows {
	return sqlmock.NewRows(indexKeyLenStatsCols).
		// acore_auth
		AddRow("acore_auth", "account", "PRIMARY", 1, "id", 0, 0, "BTREE", "Dynamic", "int", -1, -1).
		AddRow("acore_auth", "account", "idx_username", 1, "username", 0, 1, "BTREE", "Dynamic", "varchar", 32, 128).
		// acore_characters — charlog/idx_legacy: full utf8mb4 vc255 on a COMPACT table -> over the 767 effLimit -> HIGH.
		AddRow("acore_characters", "charlog", "idx_legacy", 1, "note", 0, 1, "BTREE", "Compact", "varchar", 255, 1020).
		// composite/idx_multi: vc190 mb4 (760) + bigint (8) = 768 -> MEDIUM (numeric column tips it past 767).
		AddRow("acore_characters", "composite", "idx_multi", 1, "cname", 0, 1, "BTREE", "Dynamic", "varchar", 190, 760).
		AddRow("acore_characters", "composite", "idx_multi", 2, "acctid", 0, 1, "BTREE", "Dynamic", "bigint", -1, -1).
		// decimal_tbl/idx_amt: DECIMAL (unsized -> lower bound) + vc255 mb4 (1020) = 1020 -> MEDIUM, unsizedColumns=[amount].
		AddRow("acore_characters", "decimal_tbl", "idx_amt", 1, "amount", 0, 1, "BTREE", "Dynamic", "decimal", -1, -1).
		AddRow("acore_characters", "decimal_tbl", "idx_amt", 2, "note", 0, 1, "BTREE", "Dynamic", "varchar", 255, 1020).
		// item_text/idx_note_full: full utf8mb4 vc255 (1020) on Dynamic -> MEDIUM (Barracuda-dependent).
		AddRow("acore_characters", "item_text", "idx_note_full", 1, "note", 0, 1, "BTREE", "Dynamic", "varchar", 255, 1020).
		// name_tbl/idx_name_prefix: vc255 mb4 but SUB_PART 20 -> 20*4 = 80 -> OK (prefix indexes are sized at the prefix).
		AddRow("acore_characters", "name_tbl", "idx_name_prefix", 1, "name", 20, 1, "BTREE", "Dynamic", "varchar", 255, 1020).
		// acore_world — latin1 vc500 (bytes-per-char 1) -> 500 -> OK.
		AddRow("acore_world", "spell", "idx_name_latin1", 1, "name", 0, 1, "BTREE", "Dynamic", "varchar", 500, 500)
}

// TestDBIndexKeyLengthAudit_GoldenAggregation drives the default path and verifies
// the full fold: per-column byte sizing (utf8mb4 / latin1 / numeric / prefix),
// ROW_FORMAT-aware severity (HIGH over current limit vs MEDIUM Barracuda-dependent),
// the unsized DECIMAL lower-bound mechanism, worst-first sort, perDatabase + totals.
func TestDBIndexKeyLengthAudit_GoldenAggregation(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectQuery(regexp.QuoteMeta("FROM information_schema.STATISTICS")).
		WithArgs("acore_world", "acore_characters", "acore_auth").
		WillReturnRows(goldenIndexKeyLenRows())

	reg := NewRegistry()
	RegisterDBIndexKeyLengthAuditTool(reg, DBDeps{QueryDB: db})
	tool, _ := reg.Get("db_index_key_length_audit")
	resp := tool.Handler(context.Background(), json.RawMessage(`{}`), "")
	got, ok := resp.(map[string]any)
	if !ok {
		t.Fatalf("unexpected response shape: %T", resp)
	}
	if msg, hasErr := got["error"]; hasErr {
		t.Fatalf("unexpected error: %v", msg)
	}

	if got["antelopeLimitBytes"].(int64) != 767 || got["barracudaLimitBytes"].(int64) != 3072 {
		t.Errorf("limit constants wrong: %v / %v", got["antelopeLimitBytes"], got["barracudaLimitBytes"])
	}

	cands := got["candidates"].([]indexKeyLenRow)
	if len(cands) != 4 {
		t.Fatalf("expected 4 flagged indexes, got %d: %+v", len(cands), cands)
	}
	// cands[0]: HIGH wins regardless of bytes — charlog/idx_legacy on a Compact table.
	if cands[0].Database != "acore_characters" || cands[0].Table != "charlog" || cands[0].Index != "idx_legacy" ||
		cands[0].Severity != "high" || cands[0].KeyBytes != 1020 || cands[0].EffectiveLimitBytes != 767 ||
		cands[0].RowFormat != "Compact" {
		t.Errorf("cands[0] should be charlog/idx_legacy HIGH 1020 (Compact, effLimit 767): %+v", cands[0])
	}
	if cands[0].OverAntelopeBytes != 1020-767 {
		t.Errorf("cands[0] overAntelopeBytes want %d, got %d", 1020-767, cands[0].OverAntelopeBytes)
	}
	if !strings.Contains(cands[0].Reason, "EXCEEDS the table's current InnoDB limit") {
		t.Errorf("cands[0] high reason should name the current-limit breach: %q", cands[0].Reason)
	}
	// MEDIUMs, keyBytes desc then table asc: decimal_tbl(1020) < item_text(1020) by table, then composite(768).
	if cands[1].Table != "decimal_tbl" || cands[1].Severity != "medium" || cands[1].KeyBytes != 1020 {
		t.Errorf("cands[1] should be decimal_tbl MEDIUM 1020: %+v", cands[1])
	}
	if !cands[1].KeyBytesLowerBound || len(cands[1].UnsizedColumns) != 1 || cands[1].UnsizedColumns[0] != "amount" {
		t.Errorf("cands[1] should carry the DECIMAL lower-bound flag + unsizedColumns=[amount]: %+v", cands[1])
	}
	if !strings.Contains(cands[1].Reason, "LOWER bound") {
		t.Errorf("cands[1] reason should note the lower bound: %q", cands[1].Reason)
	}
	if cands[2].Table != "item_text" || cands[2].Severity != "medium" || cands[2].KeyBytes != 1020 ||
		cands[2].EffectiveLimitBytes != 3072 {
		t.Errorf("cands[2] should be item_text MEDIUM 1020 (Dynamic, effLimit 3072): %+v", cands[2])
	}
	if cands[2].KeyBytesLowerBound {
		t.Errorf("cands[2] should NOT be a lower bound (all columns sized): %+v", cands[2])
	}
	if !strings.Contains(cands[2].Reason, "Do not ROW_FORMAT=COMPACT") {
		t.Errorf("cands[2] medium reason should warn against downgrading the row format: %q", cands[2].Reason)
	}
	if cands[3].Table != "composite" || cands[3].Severity != "medium" || cands[3].KeyBytes != 768 ||
		len(cands[3].Columns) != 2 || cands[3].Columns[0] != "cname" || cands[3].Columns[1] != "acctid" {
		t.Errorf("cands[3] should be composite MEDIUM 768 with [cname, acctid] (bigint tips it past 767): %+v", cands[3])
	}

	totals := got["totals"].(map[string]any)
	if totals["indexesScanned"].(int) != 8 {
		t.Errorf("totals.indexesScanned want 8, got %v", totals["indexesScanned"])
	}
	if totals["overLimitCount"].(int) != 4 {
		t.Errorf("totals.overLimitCount want 4, got %v", totals["overLimitCount"])
	}
	if _, present := got["minSeverity"]; present {
		t.Errorf("minSeverity should be omitted when not set, got %v", got["minSeverity"])
	}

	perDB := got["perDatabase"].([]indexKeyLenPerDatabase)
	if len(perDB) != 3 {
		t.Fatalf("expected 3 perDatabase rows, got %d", len(perDB))
	}
	// Sorted overLimitCount desc, schema asc: characters(4), then auth(0)/world(0) db-asc.
	if perDB[0].Database != "acore_characters" || perDB[0].OverLimitCount != 4 || perDB[0].IndexesScanned != 5 {
		t.Errorf("perDB[0] acore_characters want scanned 5 / overLimit 4: %+v", perDB[0])
	}
	if perDB[1].Database != "acore_auth" || perDB[1].OverLimitCount != 0 || perDB[1].IndexesScanned != 2 {
		t.Errorf("perDB[1] acore_auth want scanned 2 / overLimit 0: %+v", perDB[1])
	}
	if perDB[2].Database != "acore_world" || perDB[2].OverLimitCount != 0 || perDB[2].IndexesScanned != 1 {
		t.Errorf("perDB[2] acore_world want scanned 1 / overLimit 0: %+v", perDB[2])
	}

	if got["truncated"].(bool) {
		t.Errorf("truncated should be false for 4 candidates")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestDBIndexKeyLengthAudit_DatabaseFilterNarrowsScan verifies `database` narrows
// the IN clause to a single bind.
func TestDBIndexKeyLengthAudit_DatabaseFilterNarrowsScan(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectQuery(regexp.QuoteMeta("FROM information_schema.STATISTICS")).
		WithArgs("acore_characters"). // single bind — IN list narrowed to one schema
		WillReturnRows(sqlmock.NewRows(indexKeyLenStatsCols).
			AddRow("acore_characters", "charlog", "idx_legacy", 1, "note", 0, 1, "BTREE", "Compact", "varchar", 255, 1020).
			AddRow("acore_characters", "item_text", "idx_note_full", 1, "note", 0, 1, "BTREE", "Dynamic", "varchar", 255, 1020))

	reg := NewRegistry()
	RegisterDBIndexKeyLengthAuditTool(reg, DBDeps{QueryDB: db})
	tool, _ := reg.Get("db_index_key_length_audit")
	resp := tool.Handler(context.Background(), json.RawMessage(`{"database":"acore_characters"}`), "")
	got := resp.(map[string]any)
	if msg, hasErr := got["error"]; hasErr {
		t.Fatalf("unexpected error: %v", msg)
	}
	scanned := got["scannedSchemas"].([]string)
	if len(scanned) != 1 || scanned[0] != "acore_characters" {
		t.Errorf("scannedSchemas want [acore_characters], got %v", scanned)
	}
	cands := got["candidates"].([]indexKeyLenRow)
	// Worst-first: HIGH (charlog, Compact) before MEDIUM (item_text, Dynamic).
	if len(cands) != 2 || cands[0].Table != "charlog" || cands[0].Severity != "high" ||
		cands[1].Table != "item_text" || cands[1].Severity != "medium" {
		t.Errorf("expected [charlog HIGH, item_text MEDIUM], got %+v", cands)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestDBIndexKeyLengthAudit_MinSeverityFilter — minSeverity=high trims the list to
// the over-current-limit index only, while perDatabase / totals stay honest.
func TestDBIndexKeyLengthAudit_MinSeverityFilter(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectQuery(regexp.QuoteMeta("FROM information_schema.STATISTICS")).
		WithArgs("acore_world", "acore_characters", "acore_auth").
		WillReturnRows(goldenIndexKeyLenRows())

	reg := NewRegistry()
	RegisterDBIndexKeyLengthAuditTool(reg, DBDeps{QueryDB: db})
	tool, _ := reg.Get("db_index_key_length_audit")
	resp := tool.Handler(context.Background(), json.RawMessage(`{"minSeverity":"high"}`), "")
	got := resp.(map[string]any)
	if msg, hasErr := got["error"]; hasErr {
		t.Fatalf("unexpected error: %v", msg)
	}

	cands := got["candidates"].([]indexKeyLenRow)
	if len(cands) != 1 || cands[0].Table != "charlog" || cands[0].Severity != "high" {
		t.Errorf("minSeverity=high should list only the over-current-limit index, got %+v", cands)
	}
	// totals stays honest: 4 over-limit total (1 high + 3 medium).
	totals := got["totals"].(map[string]any)
	if totals["overLimitCount"].(int) != 4 {
		t.Errorf("totals.overLimitCount should be honest 4, got %v", totals["overLimitCount"])
	}
	// perDatabase overLimitCount also honest: acore_characters keeps its 4.
	perDB := got["perDatabase"].([]indexKeyLenPerDatabase)
	if perDB[0].Database != "acore_characters" || perDB[0].OverLimitCount != 4 {
		t.Errorf("perDB[0] acore_characters should keep its honest 4 over-limit indexes, got %+v", perDB[0])
	}
	if got["minSeverity"].(string) != "high" {
		t.Errorf("applied minSeverity should be echoed as high, got %v", got["minSeverity"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestDBIndexKeyLengthAudit_EmptyResultNonNilSlice — when every index is within the
// 767-byte ceiling, `candidates` is a non-nil empty slice (renders as []), and
// every scanned schema still surfaces in perDatabase.
func TestDBIndexKeyLengthAudit_EmptyResultNonNilSlice(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectQuery(regexp.QuoteMeta("FROM information_schema.STATISTICS")).
		WithArgs("acore_world", "acore_characters", "acore_auth").
		WillReturnRows(sqlmock.NewRows(indexKeyLenStatsCols).
			AddRow("acore_characters", "characters", "PRIMARY", 1, "guid", 0, 0, "BTREE", "Dynamic", "int", -1, -1).
			// vc191 utf8mb4 = 764 <= 767 — the classic safe length, NOT flagged.
			AddRow("acore_characters", "characters", "idx_name", 1, "name", 0, 1, "BTREE", "Dynamic", "varchar", 191, 764))

	reg := NewRegistry()
	RegisterDBIndexKeyLengthAuditTool(reg, DBDeps{QueryDB: db})
	tool, _ := reg.Get("db_index_key_length_audit")
	resp := tool.Handler(context.Background(), json.RawMessage(`{}`), "")
	got := resp.(map[string]any)

	cands := got["candidates"].([]indexKeyLenRow)
	if cands == nil {
		t.Errorf("candidates should be non-nil empty slice")
	}
	if len(cands) != 0 {
		t.Errorf("expected 0 candidates (vc191 mb4 = 764 <= 767), got %d: %+v", len(cands), cands)
	}
	b, _ := json.Marshal(got)
	if !strings.Contains(string(b), `"candidates":[]`) {
		t.Errorf("candidates should marshal to [], got %s", b)
	}
	perDB := got["perDatabase"].([]indexKeyLenPerDatabase)
	if len(perDB) != 3 {
		t.Errorf("all 3 scanned schemas should surface, got %d", len(perDB))
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestDBIndexKeyLengthAudit_Truncation — more than the 500 cap flagged → candidates
// sliced to 500 + truncated:true, but totals stays the honest pre-cap count.
func TestDBIndexKeyLengthAudit_Truncation(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	rs := sqlmock.NewRows(indexKeyLenStatsCols)
	const over = dbIndexKeyLenAuditMaxCandidates + 100 // 600
	// All vc255 utf8mb4 (1020 bytes) on Dynamic tables → all MEDIUM → all flagged.
	for i := 0; i < over; i++ {
		rs.AddRow("acore_characters", "t_"+itoaSmall(i), "idx_note", 1, "note", 0, 1, "BTREE", "Dynamic", "varchar", 255, 1020)
	}
	mock.ExpectQuery(regexp.QuoteMeta("FROM information_schema.STATISTICS")).
		WithArgs("acore_world", "acore_characters", "acore_auth").
		WillReturnRows(rs)

	reg := NewRegistry()
	RegisterDBIndexKeyLengthAuditTool(reg, DBDeps{QueryDB: db})
	tool, _ := reg.Get("db_index_key_length_audit")
	resp := tool.Handler(context.Background(), json.RawMessage(`{}`), "")
	got := resp.(map[string]any)

	cands := got["candidates"].([]indexKeyLenRow)
	if len(cands) != dbIndexKeyLenAuditMaxCandidates {
		t.Errorf("candidates should be capped at %d, got %d", dbIndexKeyLenAuditMaxCandidates, len(cands))
	}
	if !got["truncated"].(bool) {
		t.Errorf("truncated should be true")
	}
	totals := got["totals"].(map[string]any)
	if totals["overLimitCount"].(int) != over {
		t.Errorf("totals.overLimitCount should be honest pre-cap %d, got %v", over, totals["overLimitCount"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestDBIndexKeyLengthAudit_PureHelpers covers the unit-testable helpers in
// isolation: per-column byte sizing (utf8mb4 full / prefix / latin1 / binary /
// numeric / unsized), the ROW_FORMAT→limit map, severity classification, and the
// severity ranking.
func TestDBIndexKeyLengthAudit_PureHelpers(t *testing.T) {
	// indexColumnKeyBytes — string full column: octet length verbatim.
	if b, ok := indexColumnKeyBytes("varchar", 255, 1020, 0); !ok || b != 1020 {
		t.Errorf("vc255 mb4 full want 1020,true got %d,%v", b, ok)
	}
	// latin1 (bytes-per-char 1).
	if b, ok := indexColumnKeyBytes("varchar", 500, 500, 0); !ok || b != 500 {
		t.Errorf("vc500 latin1 want 500,true got %d,%v", b, ok)
	}
	// prefix index: SUB_PART 20 chars * 4 bytes-per-char = 80.
	if b, ok := indexColumnKeyBytes("varchar", 255, 1020, 20); !ok || b != 80 {
		t.Errorf("vc255 mb4 prefix(20) want 80,true got %d,%v", b, ok)
	}
	// prefix can't exceed the full column octet length.
	if b, ok := indexColumnKeyBytes("varchar", 10, 10, 50); !ok || b != 10 {
		t.Errorf("prefix beyond full column should cap at 10, got %d,%v", b, ok)
	}
	// binary/blob (octet length present, bytes-per-char 1).
	if b, ok := indexColumnKeyBytes("varbinary", 16, 16, 0); !ok || b != 16 {
		t.Errorf("varbinary(16) want 16,true got %d,%v", b, ok)
	}
	// fixed numeric/temporal types.
	for _, c := range []struct {
		dt   string
		want int64
	}{{"int", 4}, {"bigint", 8}, {"tinyint", 1}, {"smallint", 2}, {"mediumint", 3}, {"datetime", 8}, {"timestamp", 4}, {"date", 3}} {
		if b, ok := indexColumnKeyBytes(c.dt, -1, -1, 0); !ok || b != c.want {
			t.Errorf("%s want %d,true got %d,%v", c.dt, c.want, b, ok)
		}
	}
	// unsized types (DECIMAL/BIT/unknown) → (0,false).
	for _, dt := range []string{"decimal", "bit", "geometry", ""} {
		if b, ok := indexColumnKeyBytes(dt, -1, -1, 0); ok || b != 0 {
			t.Errorf("%q should be unsized (0,false), got %d,%v", dt, b, ok)
		}
	}

	// innodbKeyPrefixLimit: Barracuda 3072, Antelope/unknown 767 (case-insensitive).
	for _, rf := range []string{"Dynamic", "DYNAMIC", "Compressed"} {
		if got := innodbKeyPrefixLimit(rf); got != 3072 {
			t.Errorf("%s limit want 3072, got %d", rf, got)
		}
	}
	for _, rf := range []string{"Compact", "Redundant", "", "Paged"} {
		if got := innodbKeyPrefixLimit(rf); got != 767 {
			t.Errorf("%q limit want 767 (Antelope/conservative), got %d", rf, got)
		}
	}

	// classifyIndexKeyLength: over effLimit → high; over 767 but within effLimit →
	// medium; within 767 → not flagged.
	if sev, f := classifyIndexKeyLength(1020, 767); !f || sev != "high" { // Antelope table over its own limit
		t.Errorf("1020 vs effLimit 767 want high,true got %q,%v", sev, f)
	}
	if sev, f := classifyIndexKeyLength(1020, 3072); !f || sev != "medium" { // Barracuda-dependent
		t.Errorf("1020 vs effLimit 3072 want medium,true got %q,%v", sev, f)
	}
	if sev, f := classifyIndexKeyLength(767, 3072); f || sev != "" { // exactly the limit, safe everywhere
		t.Errorf("767 (== Antelope limit) should not be flagged, got %q,%v", sev, f)
	}
	if sev, f := classifyIndexKeyLength(3100, 3072); !f || sev != "high" { // over the Barracuda limit
		t.Errorf("3100 vs effLimit 3072 want high,true got %q,%v", sev, f)
	}

	// indexKeyLenSeverityRank ordering: high < medium < other.
	if !(indexKeyLenSeverityRank("high") < indexKeyLenSeverityRank("medium") &&
		indexKeyLenSeverityRank("medium") < indexKeyLenSeverityRank("low")) {
		t.Errorf("severity rank ordering wrong")
	}
}
