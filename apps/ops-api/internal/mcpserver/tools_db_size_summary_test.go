package mcpserver

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// TestDBSizeSummary_DescriptionMentionsContext anchors the description to the
// keywords the agent uses to pick this tool ("disk", "size", "TABLES",
// "topN"). Same shape guard as the other db_* tools.
func TestDBSizeSummary_DescriptionMentionsContext(t *testing.T) {
	reg := NewRegistry()
	RegisterDBSizeSummaryTool(reg, DBDeps{})
	tool, ok := reg.Get("db_size_summary")
	if !ok {
		t.Fatal("db_size_summary not registered")
	}
	for _, kw := range []string{"information_schema.TABLES", "topN", "perDatabase", "topTables", "TABLE_ROWS", "acore_playerbots"} {
		if !strings.Contains(tool.Description, kw) {
			t.Errorf("description missing %q", kw)
		}
	}
}

func TestDBSizeSummary_ReadOnlyAnnotation(t *testing.T) {
	reg := NewRegistry()
	RegisterDBSizeSummaryTool(reg, DBDeps{})
	tool, _ := reg.Get("db_size_summary")
	if !strings.Contains(string(tool.Annotations), `"readOnlyHint":true`) {
		t.Errorf("expected readOnlyHint:true, got %s", tool.Annotations)
	}
	if tool.Destructive {
		t.Errorf("destructive should be false")
	}
}

func TestDBSizeSummary_NoPoolReturnsError(t *testing.T) {
	reg := NewRegistry()
	RegisterDBSizeSummaryTool(reg, DBDeps{})
	tool, _ := reg.Get("db_size_summary")
	resp := tool.Handler(context.Background(), json.RawMessage(`{}`), "")
	got, _ := resp.(map[string]any)
	if msg, _ := got["error"].(string); !strings.Contains(msg, "ops_ro pool not configured") {
		t.Errorf("expected pool-not-configured error, got %v", got)
	}
}

func TestDBSizeSummary_InvalidDatabaseRejected(t *testing.T) {
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	reg := NewRegistry()
	RegisterDBSizeSummaryTool(reg, DBDeps{QueryDB: db})
	tool, _ := reg.Get("db_size_summary")
	// acore_playerbots is intentionally NOT in the allowlist — would hit a
	// silent zero-byte row because ops_ro lacks the grant.
	resp := tool.Handler(context.Background(), json.RawMessage(`{"database":"acore_playerbots"}`), "")
	got, _ := resp.(map[string]any)
	if msg, _ := got["error"].(string); !strings.Contains(msg, "acore_world/acore_characters/acore_auth") {
		t.Errorf("expected allowlist error, got %v", got)
	}
}

// TestDBSizeSummary_AllSchemasDefaultAggregation drives the default code path
// (no database filter, default topN=10) with three rows across two schemas.
// Verifies: per-DB sums, totals, top-table ordering, human-formatted twins.
func TestDBSizeSummary_AllSchemasDefaultAggregation(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// 2 MiB and 4 MiB tables in acore_world, 6 MiB table in acore_characters.
	// Use values that exercise the humanBytes MiB branch so we catch a
	// regression in the rollup vs. the formatter.
	const MiB = 1024 * 1024
	mock.ExpectQuery(regexp.QuoteMeta("FROM information_schema.TABLES")).
		WithArgs("acore_world", "acore_characters", "acore_auth").
		WillReturnRows(sqlmock.NewRows([]string{"TABLE_SCHEMA", "TABLE_NAME", "ENGINE", "TABLE_ROWS", "DATA_LENGTH", "INDEX_LENGTH"}).
			AddRow("acore_world", "creature_template", "InnoDB", int64(50000), int64(2*MiB), int64(0)).
			AddRow("acore_world", "item_template", "InnoDB", int64(30000), int64(3*MiB), int64(1*MiB)).
			AddRow("acore_characters", "characters", "InnoDB", int64(100), int64(5*MiB), int64(1*MiB)))

	reg := NewRegistry()
	RegisterDBSizeSummaryTool(reg, DBDeps{QueryDB: db})
	tool, _ := reg.Get("db_size_summary")
	resp := tool.Handler(context.Background(), json.RawMessage(`{}`), "")
	got, ok := resp.(map[string]any)
	if !ok {
		t.Fatalf("unexpected response shape: %T", resp)
	}
	if msg, hasErr := got["error"]; hasErr {
		t.Fatalf("unexpected error: %v", msg)
	}

	perDB := got["perDatabase"].([]dbSizePerDatabase)
	if len(perDB) != 3 {
		t.Fatalf("perDatabase len: %d", len(perDB))
	}
	// Sorted by totalSizeBytes desc: characters(6) > world(6) — tied, so schema asc.
	// Both characters and world total to 6 MiB; tiebreaker is schema asc:
	// acore_characters < acore_world < acore_auth (empty).
	if perDB[0].Database != "acore_characters" || perDB[0].TotalSizeBytes != 6*MiB {
		t.Errorf("perDB[0]: %+v", perDB[0])
	}
	if perDB[1].Database != "acore_world" || perDB[1].TotalSizeBytes != 6*MiB {
		t.Errorf("perDB[1]: %+v", perDB[1])
	}
	if perDB[2].Database != "acore_auth" || perDB[2].TableCount != 0 {
		t.Errorf("perDB[2] (acore_auth) should be empty rollup: %+v", perDB[2])
	}
	if perDB[0].TotalSizeHuman != "6.00 MiB" {
		t.Errorf("human bytes wrong: %q", perDB[0].TotalSizeHuman)
	}

	topTables := got["topTables"].([]tableSizeRow)
	// 3 rows, default topN=10 → all 3 surface, sorted by total desc.
	if len(topTables) != 3 {
		t.Fatalf("topTables len: %d", len(topTables))
	}
	// Tied: characters (6 MiB) and item_template (4 MiB) and creature_template (2 MiB).
	// First slot should be the 6 MiB characters table.
	if topTables[0].TotalSizeBytes != 6*MiB || topTables[0].Table != "characters" {
		t.Errorf("topTables[0]: %+v", topTables[0])
	}
	if topTables[1].Table != "item_template" || topTables[1].TotalSizeBytes != 4*MiB {
		t.Errorf("topTables[1]: %+v", topTables[1])
	}

	totals := got["totals"].(map[string]any)
	if totals["tableCount"].(int64) != 3 {
		t.Errorf("totals.tableCount: %v", totals["tableCount"])
	}
	if totals["totalSizeBytes"].(int64) != int64(12*MiB) {
		t.Errorf("totals.totalSizeBytes: %v", totals["totalSizeBytes"])
	}
	if totals["totalSizeHuman"].(string) != "12.00 MiB" {
		t.Errorf("totals.totalSizeHuman: %v", totals["totalSizeHuman"])
	}
	if got["topN"].(int) != dbSizeSummaryDefaultN {
		t.Errorf("topN default not applied: %v", got["topN"])
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestDBSizeSummary_DatabaseFilterNarrowsScan verifies passing `database`
// narrows the IN clause to one schema. We assert via WithArgs — a mismatch
// in the bound args fails the test.
func TestDBSizeSummary_DatabaseFilterNarrowsScan(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectQuery(regexp.QuoteMeta("FROM information_schema.TABLES")).
		WithArgs("acore_world"). // single bind — IN list narrowed to one schema
		WillReturnRows(sqlmock.NewRows([]string{"TABLE_SCHEMA", "TABLE_NAME", "ENGINE", "TABLE_ROWS", "DATA_LENGTH", "INDEX_LENGTH"}).
			AddRow("acore_world", "spell_dbc", "InnoDB", int64(99), int64(1024), int64(512)))

	reg := NewRegistry()
	RegisterDBSizeSummaryTool(reg, DBDeps{QueryDB: db})
	tool, _ := reg.Get("db_size_summary")
	resp := tool.Handler(context.Background(), json.RawMessage(`{"database":"acore_world"}`), "")
	got := resp.(map[string]any)
	perDB := got["perDatabase"].([]dbSizePerDatabase)
	if len(perDB) != 1 {
		t.Fatalf("expected one schema in perDatabase, got %d", len(perDB))
	}
	if perDB[0].Database != "acore_world" {
		t.Errorf("expected acore_world, got %s", perDB[0].Database)
	}
	scanned := got["scannedSchemas"].([]string)
	if len(scanned) != 1 || scanned[0] != "acore_world" {
		t.Errorf("scannedSchemas: %v", scanned)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestDBSizeSummary_TopNClamping — handler clamps topN<=0 to 10 and topN>50
// to 50. The clamping path is in the registered handler, not
// collectDBSizeSummary, so we exercise via tool.Handler.
func TestDBSizeSummary_TopNClamping(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// Return 60 tiny rows so we can verify the slice is capped at 50.
	rs := sqlmock.NewRows([]string{"TABLE_SCHEMA", "TABLE_NAME", "ENGINE", "TABLE_ROWS", "DATA_LENGTH", "INDEX_LENGTH"})
	for i := 0; i < 60; i++ {
		rs.AddRow("acore_world", "t_"+itoaSmall(i), "InnoDB", int64(i), int64(i), int64(0))
	}
	mock.ExpectQuery(regexp.QuoteMeta("FROM information_schema.TABLES")).
		WithArgs("acore_world", "acore_characters", "acore_auth").
		WillReturnRows(rs)

	reg := NewRegistry()
	RegisterDBSizeSummaryTool(reg, DBDeps{QueryDB: db})
	tool, _ := reg.Get("db_size_summary")
	resp := tool.Handler(context.Background(), json.RawMessage(`{"topN":99999}`), "")
	got := resp.(map[string]any)
	if msg, hasErr := got["error"]; hasErr {
		t.Fatalf("unexpected error: %v", msg)
	}
	if got["topN"].(int) != dbSizeSummaryMaxN {
		t.Errorf("topN not clamped to hard max: %v", got["topN"])
	}
	if len(got["topTables"].([]tableSizeRow)) != dbSizeSummaryMaxN {
		t.Errorf("topTables not sliced to hard max: %d", len(got["topTables"].([]tableSizeRow)))
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestDBSizeSummary_EmptySchemasStillSurface — when a schema has zero base
// tables (or ops_ro can't see them), we still emit a zero-rollup entry so the
// operator can tell the schema was scanned. Catches the regression where a
// silent absence in perDatabase looked like a tool bug.
func TestDBSizeSummary_EmptySchemasStillSurface(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// Only acore_auth returns rows; the other two schemas have no base tables
	// in this fixture — they should still appear in perDatabase with zeroes.
	mock.ExpectQuery(regexp.QuoteMeta("FROM information_schema.TABLES")).
		WithArgs("acore_world", "acore_characters", "acore_auth").
		WillReturnRows(sqlmock.NewRows([]string{"TABLE_SCHEMA", "TABLE_NAME", "ENGINE", "TABLE_ROWS", "DATA_LENGTH", "INDEX_LENGTH"}).
			AddRow("acore_auth", "account", "InnoDB", int64(10), int64(2048), int64(512)))

	reg := NewRegistry()
	RegisterDBSizeSummaryTool(reg, DBDeps{QueryDB: db})
	tool, _ := reg.Get("db_size_summary")
	resp := tool.Handler(context.Background(), json.RawMessage(`{}`), "")
	got := resp.(map[string]any)
	perDB := got["perDatabase"].([]dbSizePerDatabase)
	if len(perDB) != 3 {
		t.Fatalf("expected 3 entries (one per scanned schema), got %d", len(perDB))
	}
	// The non-zero one sorts first.
	if perDB[0].Database != "acore_auth" || perDB[0].TableCount != 1 {
		t.Errorf("perDB[0]: %+v", perDB[0])
	}
	// Tied at zero — schema-asc tiebreaker puts acore_characters before acore_world.
	if perDB[1].Database != "acore_characters" || perDB[1].TableCount != 0 {
		t.Errorf("perDB[1]: %+v", perDB[1])
	}
	if perDB[2].Database != "acore_world" || perDB[2].TableCount != 0 {
		t.Errorf("perDB[2]: %+v", perDB[2])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestDBSizeSummary_IsKnownSizeSchemaLookup is the unit test for the
// allowlist check helper. Cheap but guards against a future refactor that
// drops acore_auth from dbSizeKnownSchemas.
func TestDBSizeSummary_IsKnownSizeSchemaLookup(t *testing.T) {
	for _, s := range []string{"acore_world", "acore_characters", "acore_auth"} {
		if !isKnownSizeSchema(s) {
			t.Errorf("%s should be in the allowlist", s)
		}
	}
	for _, s := range []string{"acore_playerbots", "mysql", "information_schema", "", "ACORE_WORLD"} {
		if isKnownSizeSchema(s) {
			t.Errorf("%s should NOT be in the allowlist", s)
		}
	}
}

// itoaSmall — local tiny integer → ascii for the topN-clamp fixture. Avoids
// pulling strconv just for table-name suffixes in test data.
func itoaSmall(i int) string {
	if i == 0 {
		return "0"
	}
	var buf [4]byte
	n := 0
	for i > 0 {
		buf[n] = byte('0' + i%10)
		i /= 10
		n++
	}
	for l, r := 0, n-1; l < r; l, r = l+1, r-1 {
		buf[l], buf[r] = buf[r], buf[l]
	}
	return string(buf[:n])
}
