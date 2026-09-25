package mcpserver

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// TestSafeIdentifierRegex pins the table-name allowlist. The handler interpolates
// `table` into SHOW CREATE TABLE / SHOW INDEX FROM (placeholders aren't supported
// for SHOW DDL targets), so anything outside this regex would be a SQL injection.
func TestSafeIdentifierRegex(t *testing.T) {
	good := []string{"characters", "mod_ollama_chat_gateway_audit", "abc123", "T", "_x"}
	for _, s := range good {
		if !reSafeIdentifier.MatchString(s) {
			t.Errorf("safe identifier rejected: %q", s)
		}
	}
	bad := []string{
		"",
		"characters; DROP TABLE x",
		"foo`bar",
		"foo bar",
		"foo'bar",
		"foo--",
		"foo/*",
		"foo.bar", // schema-qualified is not supported here — caller passes `database` separately
	}
	for _, s := range bad {
		if reSafeIdentifier.MatchString(s) {
			t.Errorf("unsafe identifier accepted: %q", s)
		}
	}
}

// TestDBTableInfo_DescriptionMentionsContext is the same kind of guard as the
// container_stats description test — the description shapes which tool the
// agent picks when an operator says "what's the schema of X table?". Drop the
// composite-vs-three-calls framing and the agent will fall back to db_query.
func TestDBTableInfo_DescriptionMentionsContext(t *testing.T) {
	reg := NewRegistry()
	RegisterDBTableInfoTool(reg, DBDeps{})
	tool, ok := reg.Get("db_table_info")
	if !ok {
		t.Fatal("db_table_info not registered")
	}
	for _, kw := range []string{"createTable", "indexes", "primaryKey", "SHOW CREATE TABLE", "SHOW INDEX"} {
		if !strings.Contains(tool.Description, kw) {
			t.Errorf("description missing %q: %s", kw, tool.Description)
		}
	}
}

func TestDBTableInfo_NoPoolReturnsError(t *testing.T) {
	reg := NewRegistry()
	RegisterDBTableInfoTool(reg, DBDeps{}) // QueryDB nil
	tool, _ := reg.Get("db_table_info")
	resp := tool.Handler(context.Background(),
		json.RawMessage(`{"database":"acore_world","table":"item_template"}`), "")
	got, _ := resp.(map[string]any)
	if msg, _ := got["error"].(string); !strings.Contains(msg, "ops_ro pool not configured") {
		t.Errorf("expected pool-not-configured error, got %v", got)
	}
}

func TestDBTableInfo_RejectsBadDatabase(t *testing.T) {
	reg := NewRegistry()
	// Pass a non-nil pool so we don't short-circuit on the pool-not-configured
	// branch before the database allowlist check runs.
	db, _, _ := sqlmock.New()
	defer db.Close()
	RegisterDBTableInfoTool(reg, DBDeps{QueryDB: db})
	tool, _ := reg.Get("db_table_info")
	resp := tool.Handler(context.Background(),
		json.RawMessage(`{"database":"mysql","table":"user"}`), "")
	got, _ := resp.(map[string]any)
	if msg, _ := got["error"].(string); !strings.Contains(msg, "database must be one of") {
		t.Errorf("expected database allowlist error, got %v", got)
	}
}

func TestDBTableInfo_RejectsBadTableName(t *testing.T) {
	reg := NewRegistry()
	db, _, _ := sqlmock.New()
	defer db.Close()
	RegisterDBTableInfoTool(reg, DBDeps{QueryDB: db})
	tool, _ := reg.Get("db_table_info")
	resp := tool.Handler(context.Background(),
		json.RawMessage(`{"database":"acore_world","table":"item; DROP TABLE x"}`), "")
	got, _ := resp.(map[string]any)
	if msg, _ := got["error"].(string); !strings.Contains(msg, "table must match") {
		t.Errorf("expected table-name validation error, got %v", got)
	}
}

// TestCollectTableInfo_Golden drives the full collectTableInfo path with a
// mock connection — exercises the SHOW CREATE / SHOW INDEX / SHOW TABLE STATUS
// parsing in one place. Keeps the test compact: a single representative table
// with one PRIMARY and one composite secondary index, which is the shape that
// historically broke the parser (single-segment indexes work by accident).
func TestCollectTableInfo_Golden(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectExec(regexp.QuoteMeta("USE `acore_characters`")).
		WillReturnResult(sqlmock.NewResult(0, 0))

	mock.ExpectQuery(regexp.QuoteMeta("SHOW CREATE TABLE `characters`")).
		WillReturnRows(sqlmock.NewRows([]string{"Table", "Create Table"}).
			AddRow("characters", "CREATE TABLE `characters` (\n  `guid` int unsigned NOT NULL,\n  `name` varchar(12) NOT NULL,\n  PRIMARY KEY (`guid`),\n  KEY `idx_name_account` (`name`,`account`)\n) ENGINE=InnoDB"))

	// Two indexes: PRIMARY (single segment) and idx_name_account (two segments).
	// Cardinality reported on the leading segment per MySQL convention; the
	// trailing segment row has a higher per-prefix estimate.
	mock.ExpectQuery(regexp.QuoteMeta("SHOW INDEX FROM `characters`")).
		WillReturnRows(sqlmock.NewRows([]string{
			"Table", "Non_unique", "Key_name", "Seq_in_index", "Column_name",
			"Collation", "Cardinality", "Sub_part", "Packed", "Null", "Index_type", "Comment", "Index_comment",
		}).
			AddRow("characters", int64(0), "PRIMARY", int64(1), "guid", "A", int64(50000), nil, nil, "", "BTREE", "", "").
			AddRow("characters", int64(1), "idx_name_account", int64(1), "name", "A", int64(45000), nil, nil, "", "BTREE", "", "").
			AddRow("characters", int64(1), "idx_name_account", int64(2), "account", "A", int64(50000), nil, nil, "", "BTREE", "", ""))

	mock.ExpectQuery(regexp.QuoteMeta("SHOW TABLE STATUS LIKE 'characters'")).
		WillReturnRows(sqlmock.NewRows([]string{
			"Name", "Engine", "Version", "Row_format", "Rows", "Avg_row_length",
			"Data_length", "Max_data_length", "Index_length", "Data_free",
			"Auto_increment", "Create_time", "Update_time", "Check_time",
			"Collation", "Checksum", "Create_options", "Comment",
		}).
			AddRow("characters", "InnoDB", int64(10), "Dynamic", int64(50000), int64(256),
				uint64(12800000), uint64(0), uint64(2048000), uint64(0),
				int64(60000), "2026-01-01 00:00:00", "2026-04-01 00:00:00", nil,
				"utf8mb4_general_ci", nil, "", ""))

	out, err := collectTableInfo(context.Background(), db, "acore_characters", "characters")
	if err != nil {
		t.Fatalf("collectTableInfo: %v", err)
	}

	if got := out["createTable"].(string); !strings.HasPrefix(got, "CREATE TABLE `characters`") {
		t.Errorf("createTable: %q", got)
	}
	if got := out["engine"].(string); got != "InnoDB" {
		t.Errorf("engine: %q", got)
	}
	if got := out["rowsEstimate"].(int64); got != 50000 {
		t.Errorf("rowsEstimate: %d", got)
	}
	if got := out["dataLength"].(uint64); got != 12800000 {
		t.Errorf("dataLength: %d", got)
	}
	if got := out["indexLength"].(uint64); got != 2048000 {
		t.Errorf("indexLength: %d", got)
	}
	if got := out["totalSizeBytes"].(uint64); got != 14848000 {
		t.Errorf("totalSizeBytes: %d", got)
	}
	if got := out["totalSizeHuman"].(string); got != "14.16 MiB" {
		t.Errorf("totalSizeHuman: %q", got)
	}
	if got := out["autoIncrement"].(int64); got != 60000 {
		t.Errorf("autoIncrement: %d", got)
	}

	indexes, _ := out["indexes"].([]indexEntry)
	if len(indexes) != 2 {
		t.Fatalf("indexCount: %d", len(indexes))
	}
	if indexes[0].KeyName != "PRIMARY" || !indexes[0].Unique || len(indexes[0].Columns) != 1 || indexes[0].Columns[0] != "guid" {
		t.Errorf("PRIMARY index parsed wrong: %+v", indexes[0])
	}
	if indexes[1].KeyName != "idx_name_account" || indexes[1].Unique {
		t.Errorf("secondary index unique flag wrong: %+v", indexes[1])
	}
	// Multi-segment index: columns must come back in Seq_in_index order.
	if got, want := indexes[1].Columns, []string{"name", "account"}; !equalStrSlices(got, want) {
		t.Errorf("composite columns: got %v want %v", got, want)
	}
	if indexes[1].Cardinality != 45000 {
		t.Errorf("cardinality should be the leading-segment estimate, got %d", indexes[1].Cardinality)
	}

	primary, _ := out["primaryKey"].([]string)
	if !equalStrSlices(primary, []string{"guid"}) {
		t.Errorf("primaryKey: %v", primary)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestCollectTableInfo_TableMissing covers the SHOW TABLE STATUS LIKE returning
// zero rows — the most common failure once the table name passes regex but the
// caller asked about a table that doesn't exist in this database.
func TestCollectTableInfo_TableMissing(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectExec(regexp.QuoteMeta("USE `acore_world`")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta("SHOW CREATE TABLE `nope`")).
		WillReturnRows(sqlmock.NewRows([]string{"Table", "Create Table"}).
			AddRow("nope", "CREATE TABLE `nope` (id int)"))
	mock.ExpectQuery(regexp.QuoteMeta("SHOW INDEX FROM `nope`")).
		WillReturnRows(sqlmock.NewRows([]string{
			"Table", "Non_unique", "Key_name", "Seq_in_index", "Column_name",
			"Collation", "Cardinality", "Sub_part", "Packed", "Null", "Index_type", "Comment", "Index_comment",
		}))
	mock.ExpectQuery(regexp.QuoteMeta("SHOW TABLE STATUS LIKE 'nope'")).
		WillReturnRows(sqlmock.NewRows([]string{"Name"}))

	_, err = collectTableInfo(context.Background(), db, "acore_world", "nope")
	if err == nil {
		t.Fatal("expected not-found error, got nil")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("unexpected err: %v", err)
	}
}

// TestParseInt64 nails the forgiving-numeric helper. The MySQL driver returns
// most numerics as []byte under the hood, but a few build configs hand back
// signed/unsigned ints directly — make sure all four shapes parse the same.
func TestParseInt64(t *testing.T) {
	cases := []struct {
		in   string
		want int64
	}{
		{"0", 0},
		{"123", 123},
		{"-7", -7},
		{"", 0},
		{"NULL", 0},
		{"   42  ", 42},
		{"abc", 0},  // non-numeric → 0 (don't surface a parse error)
		{"12x", 0},  // mixed → 0
	}
	for _, c := range cases {
		if got := parseInt64(c.in); got != c.want {
			t.Errorf("parseInt64(%q) = %d want %d", c.in, got, c.want)
		}
	}
}

func TestStringFromCoercion(t *testing.T) {
	if got := stringFrom([]byte("foo")); got != "foo" {
		t.Errorf("[]byte coerce: %q", got)
	}
	if got := stringFrom("bar"); got != "bar" {
		t.Errorf("string passthrough: %q", got)
	}
	if got := stringFrom(nil); got != "" {
		t.Errorf("nil: %q", got)
	}
}

func equalStrSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
