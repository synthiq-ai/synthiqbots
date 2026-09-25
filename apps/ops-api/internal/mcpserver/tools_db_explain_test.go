package mcpserver

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// TestBareSelectRegex pins the read-only validation surface — the handler
// prepends EXPLAIN to whatever the caller passes, so the regex MUST reject
// anything that isn't a top-level SELECT, especially "EXPLAIN ..." (which
// would produce "EXPLAIN EXPLAIN ..." downstream).
func TestBareSelectRegex(t *testing.T) {
	good := []string{
		"SELECT 1",
		"select * from characters",
		"  SELECT name FROM characters WHERE guid = ?",
		"\n\tSELECT 1\n",
	}
	for _, s := range good {
		if !reBareSelect.MatchString(s) {
			t.Errorf("bare SELECT rejected: %q", s)
		}
	}
	bad := []string{
		"",
		"EXPLAIN SELECT 1",
		"explain select 1",
		"SHOW TABLES",
		"DESCRIBE characters",
		"WITH x AS (SELECT 1) SELECT * FROM x",
		"INSERT INTO x VALUES (1)",
		"UPDATE x SET y = 1",
		"DELETE FROM x WHERE id = 1",
		"(SELECT 1)",
	}
	for _, s := range bad {
		if reBareSelect.MatchString(s) {
			t.Errorf("non-bare-SELECT accepted: %q", s)
		}
	}
}

func TestDBExplain_DescriptionMentionsContext(t *testing.T) {
	reg := NewRegistry()
	RegisterDBExplainTool(reg, DBDeps{})
	tool, ok := reg.Get("db_explain")
	if !ok {
		t.Fatal("db_explain not registered")
	}
	for _, kw := range []string{"EXPLAIN", "FORMAT=JSON", "warnings", "filesort", "ops_ro", "do NOT prepend"} {
		if !strings.Contains(tool.Description, kw) {
			t.Errorf("description missing %q: %s", kw, tool.Description)
		}
	}
}

func TestDBExplain_NoPoolReturnsError(t *testing.T) {
	reg := NewRegistry()
	RegisterDBExplainTool(reg, DBDeps{}) // QueryDB nil
	tool, _ := reg.Get("db_explain")
	resp := tool.Handler(context.Background(),
		json.RawMessage(`{"database":"acore_world","sql":"SELECT 1"}`), "")
	got, _ := resp.(map[string]any)
	if msg, _ := got["error"].(string); !strings.Contains(msg, "ops_ro pool not configured") {
		t.Errorf("expected pool-not-configured error, got %v", got)
	}
}

func TestDBExplain_RejectsBadDatabase(t *testing.T) {
	reg := NewRegistry()
	db, _, _ := sqlmock.New()
	defer db.Close()
	RegisterDBExplainTool(reg, DBDeps{QueryDB: db})
	tool, _ := reg.Get("db_explain")
	resp := tool.Handler(context.Background(),
		json.RawMessage(`{"database":"mysql","sql":"SELECT 1"}`), "")
	got, _ := resp.(map[string]any)
	if msg, _ := got["error"].(string); !strings.Contains(msg, "database must be one of") {
		t.Errorf("expected database allowlist error, got %v", got)
	}
}

func TestDBExplain_RejectsExplainPrefix(t *testing.T) {
	reg := NewRegistry()
	db, _, _ := sqlmock.New()
	defer db.Close()
	RegisterDBExplainTool(reg, DBDeps{QueryDB: db})
	tool, _ := reg.Get("db_explain")
	resp := tool.Handler(context.Background(),
		json.RawMessage(`{"database":"acore_world","sql":"EXPLAIN SELECT 1"}`), "")
	got, _ := resp.(map[string]any)
	if msg, _ := got["error"].(string); !strings.Contains(msg, "do not prepend EXPLAIN") {
		t.Errorf("expected do-not-prepend error, got %v", got)
	}
}

func TestDBExplain_RejectsWriteStatement(t *testing.T) {
	reg := NewRegistry()
	db, _, _ := sqlmock.New()
	defer db.Close()
	RegisterDBExplainTool(reg, DBDeps{QueryDB: db})
	tool, _ := reg.Get("db_explain")
	resp := tool.Handler(context.Background(),
		json.RawMessage(`{"database":"acore_world","sql":"UPDATE characters SET name='x' WHERE guid=1"}`), "")
	got, _ := resp.(map[string]any)
	if msg, _ := got["error"].(string); !strings.Contains(msg, "bare SELECT") {
		t.Errorf("expected bare-SELECT error, got %v", got)
	}
}

func TestDBExplain_RejectsForbiddenSchema(t *testing.T) {
	reg := NewRegistry()
	db, _, _ := sqlmock.New()
	defer db.Close()
	RegisterDBExplainTool(reg, DBDeps{QueryDB: db})
	tool, _ := reg.Get("db_explain")
	resp := tool.Handler(context.Background(),
		json.RawMessage(`{"database":"acore_world","sql":"SELECT * FROM mysql.user"}`), "")
	got, _ := resp.(map[string]any)
	if msg, _ := got["error"].(string); !strings.Contains(msg, "mysql/information_schema") {
		t.Errorf("expected forbidden-schema error, got %v", got)
	}
}

func TestDBExplain_RejectsMultiStatement(t *testing.T) {
	reg := NewRegistry()
	db, _, _ := sqlmock.New()
	defer db.Close()
	RegisterDBExplainTool(reg, DBDeps{QueryDB: db})
	tool, _ := reg.Get("db_explain")
	resp := tool.Handler(context.Background(),
		json.RawMessage(`{"database":"acore_world","sql":"SELECT 1; SELECT 2"}`), "")
	got, _ := resp.(map[string]any)
	if msg, _ := got["error"].(string); !strings.Contains(msg, "multiple statements") {
		t.Errorf("expected multi-statement error, got %v", got)
	}
}

// TestCollectExplain_GoldenFullScan exercises the happy path AND warning
// detection together: a SELECT that the optimizer plans as a full table scan
// with a filesort. Both warnings should fire, and the JSON plan should be
// surfaced parsed (not raw).
func TestCollectExplain_GoldenFullScan(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectExec(regexp.QuoteMeta("USE `acore_characters`")).
		WillReturnResult(sqlmock.NewResult(0, 0))

	// EXPLAIN: one row, type=ALL, Extra contains "Using filesort"
	mock.ExpectQuery(regexp.QuoteMeta("EXPLAIN SELECT * FROM characters ORDER BY name")).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "select_type", "table", "partitions", "type",
			"possible_keys", "key", "key_len", "ref", "rows", "filtered", "Extra",
		}).
			AddRow(int64(1), "SIMPLE", "characters", nil, "ALL",
				nil, nil, nil, nil, int64(50000), "100.00", "Using filesort"))

	// EXPLAIN FORMAT=JSON: the canonical MySQL 8 shape — one row, one column.
	planJSON := `{"query_block":{"select_id":1,"cost_info":{"query_cost":"5042.00"},"table":{"table_name":"characters","access_type":"ALL","rows_examined_per_scan":50000,"rows_produced_per_join":50000,"filtered":"100.00","cost_info":{"read_cost":"42.00","eval_cost":"5000.00","prefix_cost":"5042.00","data_read_per_join":"7M"}}}}`
	mock.ExpectQuery(regexp.QuoteMeta("EXPLAIN FORMAT=JSON SELECT * FROM characters ORDER BY name")).
		WillReturnRows(sqlmock.NewRows([]string{"EXPLAIN"}).AddRow(planJSON))

	out, err := collectExplain(context.Background(), db, "acore_characters",
		"SELECT * FROM characters ORDER BY name", nil)
	if err != nil {
		t.Fatalf("collectExplain: %v", err)
	}

	if got := out["rowCount"].(int); got != 1 {
		t.Errorf("rowCount: %d", got)
	}
	if got := out["estimatedRows"].(int64); got != 50000 {
		t.Errorf("estimatedRows: %d", got)
	}

	rows, _ := out["rows"].([]explainRow)
	if len(rows) != 1 {
		t.Fatalf("rows length: %d", len(rows))
	}
	r := rows[0]
	if r.Table != "characters" || r.Type != "ALL" || r.Rows != 50000 || r.Filtered != 100.0 {
		t.Errorf("row decoded wrong: %+v", r)
	}
	if !strings.Contains(r.Extra, "filesort") {
		t.Errorf("extra missing filesort: %q", r.Extra)
	}

	// Warnings: full scan + filesort.
	warns, _ := out["warnings"].([]string)
	if len(warns) != 2 {
		t.Fatalf("warnings: %v (want 2)", warns)
	}
	gotWarn := strings.Join(warns, "|")
	if !strings.Contains(gotWarn, "full table scan on characters") {
		t.Errorf("missing full-scan warning: %v", warns)
	}
	if !strings.Contains(gotWarn, "filesort on characters") {
		t.Errorf("missing filesort warning: %v", warns)
	}

	// JSON plan parsed into a map (not the raw string).
	plan, ok := out["json"].(map[string]any)
	if !ok {
		t.Fatalf("json plan not parsed into map, got %T", out["json"])
	}
	qb, ok := plan["query_block"].(map[string]any)
	if !ok {
		t.Fatalf("query_block missing from parsed plan")
	}
	if got := qb["select_id"]; got != float64(1) {
		t.Errorf("select_id: %v", got)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestCollectExplain_IndexHitNoWarnings covers the "good" plan: a primary-key
// lookup. type=const + no Extra hints + JSON plan parses → warnings array
// empty.
func TestCollectExplain_IndexHitNoWarnings(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectExec(regexp.QuoteMeta("USE `acore_characters`")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta("EXPLAIN SELECT name FROM characters WHERE guid = ?")).
		WithArgs(int64(42)).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "select_type", "table", "partitions", "type",
			"possible_keys", "key", "key_len", "ref", "rows", "filtered", "Extra",
		}).
			AddRow(int64(1), "SIMPLE", "characters", nil, "const",
				"PRIMARY", "PRIMARY", "4", "const", int64(1), "100.00", ""))
	mock.ExpectQuery(regexp.QuoteMeta("EXPLAIN FORMAT=JSON SELECT name FROM characters WHERE guid = ?")).
		WithArgs(int64(42)).
		WillReturnRows(sqlmock.NewRows([]string{"EXPLAIN"}).
			AddRow(`{"query_block":{"select_id":1,"table":{"table_name":"characters","access_type":"const"}}}`))

	out, err := collectExplain(context.Background(), db, "acore_characters",
		"SELECT name FROM characters WHERE guid = ?", []interface{}{int64(42)})
	if err != nil {
		t.Fatalf("collectExplain: %v", err)
	}

	if warns, _ := out["warnings"].([]string); len(warns) != 0 {
		t.Errorf("expected no warnings on indexed lookup, got %v", warns)
	}
	if got := out["estimatedRows"].(int64); got != 1 {
		t.Errorf("estimatedRows: %d", got)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestCollectExplain_TempTableWarning covers a join with GROUP BY — type=ALL
// produces both "full scan" and "temporary table" warnings.
func TestCollectExplain_TempTableWarning(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectExec(regexp.QuoteMeta("USE `acore_characters`")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta("EXPLAIN SELECT race, COUNT(*) FROM characters GROUP BY race")).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "select_type", "table", "partitions", "type",
			"possible_keys", "key", "key_len", "ref", "rows", "filtered", "Extra",
		}).
			AddRow(int64(1), "SIMPLE", "characters", nil, "ALL",
				nil, nil, nil, nil, int64(50000), "100.00", "Using temporary; Using filesort"))
	mock.ExpectQuery(regexp.QuoteMeta("EXPLAIN FORMAT=JSON SELECT race, COUNT(*) FROM characters GROUP BY race")).
		WillReturnRows(sqlmock.NewRows([]string{"EXPLAIN"}).
			AddRow(`{"query_block":{"select_id":1}}`))

	out, err := collectExplain(context.Background(), db, "acore_characters",
		"SELECT race, COUNT(*) FROM characters GROUP BY race", nil)
	if err != nil {
		t.Fatalf("collectExplain: %v", err)
	}

	warns, _ := out["warnings"].([]string)
	if len(warns) != 3 {
		t.Errorf("expected 3 warnings (full scan + temporary + filesort), got %d: %v", len(warns), warns)
	}
	joined := strings.Join(warns, "|")
	for _, kw := range []string{"full table scan", "temporary table", "filesort"} {
		if !strings.Contains(joined, kw) {
			t.Errorf("missing warning %q: %v", kw, warns)
		}
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestCollectExplain_JSONFallback covers the case where MySQL accepts EXPLAIN
// but EXPLAIN FORMAT=JSON returns an unparseable string — the tool should
// surface raw + parseError under `json` and still return the tabular plan.
func TestCollectExplain_JSONFallback(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectExec(regexp.QuoteMeta("USE `acore_world`")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta("EXPLAIN SELECT 1")).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "select_type", "table", "partitions", "type",
			"possible_keys", "key", "key_len", "ref", "rows", "filtered", "Extra",
		}).
			AddRow(int64(1), "SIMPLE", nil, nil, nil,
				nil, nil, nil, nil, int64(0), "100.00", "No tables used"))
	mock.ExpectQuery(regexp.QuoteMeta("EXPLAIN FORMAT=JSON SELECT 1")).
		WillReturnRows(sqlmock.NewRows([]string{"EXPLAIN"}).AddRow("not-valid-json"))

	out, err := collectExplain(context.Background(), db, "acore_world", "SELECT 1", nil)
	if err != nil {
		t.Fatalf("collectExplain: %v", err)
	}
	plan, ok := out["json"].(map[string]any)
	if !ok {
		t.Fatalf("expected fallback map, got %T", out["json"])
	}
	if plan["raw"] != "not-valid-json" {
		t.Errorf("raw passthrough wrong: %v", plan["raw"])
	}
	if _, ok := plan["parseError"]; !ok {
		t.Errorf("parseError missing: %v", plan)
	}
}

func TestParseFloat64(t *testing.T) {
	cases := []struct {
		in   string
		want float64
	}{
		{"0", 0},
		{"100.00", 100.0},
		{"42", 42.0},
		{"3.14", 3.14},
		{"-7.5", -7.5},
		{"", 0},
		{"NULL", 0},
		{"  50.50  ", 50.5},
		{"abc", 0}, // malformed → 0, no parse error surfaced
		{"1.2x", 0},
	}
	for _, c := range cases {
		got := parseFloat64(c.in)
		// Allow tiny epsilon for fractional cases — the manual digit loop has
		// the usual base-10 round-off (e.g. 3.14 ≈ 3.139999...).
		diff := got - c.want
		if diff < 0 {
			diff = -diff
		}
		if diff > 1e-6 {
			t.Errorf("parseFloat64(%q) = %v want %v", c.in, got, c.want)
		}
	}
}

func TestFloat64FromCoercion(t *testing.T) {
	if got := float64From([]byte("100.00")); got != 100.0 {
		t.Errorf("[]byte coerce: %v", got)
	}
	if got := float64From("42"); got != 42.0 {
		t.Errorf("string passthrough: %v", got)
	}
	if got := float64From(float64(3.14)); got != 3.14 {
		t.Errorf("float passthrough: %v", got)
	}
	if got := float64From(int64(5)); got != 5.0 {
		t.Errorf("int64 widened: %v", got)
	}
	if got := float64From(nil); got != 0 {
		t.Errorf("nil: %v", got)
	}
}
