package mcpserver

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// TestDBProcessList_DescriptionMentionsContext keeps the description anchored
// to the keywords the agent uses to pick this tool when an operator says
// "what's running on the DB?" / "show me long queries". Same shape as the
// db_table_info / db_explain description guards.
func TestDBProcessList_DescriptionMentionsContext(t *testing.T) {
	reg := NewRegistry()
	RegisterDBProcessListTool(reg, DBDeps{})
	tool, ok := reg.Get("db_processlist")
	if !ok {
		t.Fatal("db_processlist not registered")
	}
	for _, kw := range []string{"PROCESSLIST", "Sleep", "minTime", "byCommand", "PROCESS privilege"} {
		if !strings.Contains(tool.Description, kw) {
			t.Errorf("description missing %q: %s", kw, tool.Description)
		}
	}
}

func TestDBProcessList_NoPoolReturnsError(t *testing.T) {
	reg := NewRegistry()
	RegisterDBProcessListTool(reg, DBDeps{}) // QueryDB nil
	tool, _ := reg.Get("db_processlist")
	resp := tool.Handler(context.Background(), json.RawMessage(`{}`), "")
	got, _ := resp.(map[string]any)
	if msg, _ := got["error"].(string); !strings.Contains(msg, "ops_ro pool not configured") {
		t.Errorf("expected pool-not-configured error, got %v", got)
	}
}

// TestDBProcessList_DefaultExcludesSleep verifies that the default query
// excludes 'Sleep' connections — this is the most important shaping decision
// of the tool. On a healthy server PROCESSLIST is overwhelmingly Sleep rows
// (mod-ollama-chat keeps a pool of idle connections per ac-worldserver
// thread); if we returned them by default the slow-query that triggered the
// diagnosis would be lost in the noise.
func TestDBProcessList_DefaultExcludesSleep(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectQuery(regexp.QuoteMeta("FROM information_schema.PROCESSLIST")).
		WithArgs("Sleep", 200).
		WillReturnRows(sqlmock.NewRows([]string{"ID", "USER", "HOST", "DB", "COMMAND", "TIME", "STATE", "INFO"}).
			AddRow(int64(42), "ops_ro", "172.17.0.5:55001", "acore_characters", "Query", int64(7), "Sending data", "SELECT * FROM characters WHERE name='Slayo'").
			AddRow(int64(43), "acore", "172.17.0.6:55102", "acore_world", "Query", int64(3), "executing", "SELECT * FROM item_template WHERE entry=12345"))

	out, err := collectProcessList(context.Background(), db, false, 0, "", "", 200)
	if err != nil {
		t.Fatalf("collectProcessList: %v", err)
	}
	rows := out["rows"].([]processListRow)
	if len(rows) != 2 {
		t.Fatalf("rowCount: %d", len(rows))
	}
	if rows[0].ID != 42 || rows[0].Time != 7 {
		t.Errorf("row[0] wrong: %+v", rows[0])
	}
	by := out["byCommand"].(map[string]int)
	if by["Query"] != 2 {
		t.Errorf("byCommand[Query]: %d", by["Query"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestDBProcessList_IncludeSleepDropsFilter — when includeSleep=true the
// COMMAND != 'Sleep' predicate must NOT be in the query. WithArgs() will fail
// the test if the binding count or order differs from what we expect, so this
// catches accidental filter retention.
func TestDBProcessList_IncludeSleepDropsFilter(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectQuery(regexp.QuoteMeta("FROM information_schema.PROCESSLIST")).
		WithArgs(200). // only the LIMIT placeholder — no Sleep, no minTime, no user, no db
		WillReturnRows(sqlmock.NewRows([]string{"ID", "USER", "HOST", "DB", "COMMAND", "TIME", "STATE", "INFO"}).
			AddRow(int64(1), "ops_ro", "h", "", "Sleep", int64(100), "", ""))

	out, err := collectProcessList(context.Background(), db, true, 0, "", "", 200)
	if err != nil {
		t.Fatalf("collectProcessList: %v", err)
	}
	if got := out["rowCount"].(int); got != 1 {
		t.Errorf("rowCount: %d", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestDBProcessList_MinTimeFilter verifies the minTime filter is wired into
// the bound args in the right position.
func TestDBProcessList_MinTimeFilter(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectQuery(regexp.QuoteMeta("AND TIME >= ?")).
		WithArgs("Sleep", int64(60), 200).
		WillReturnRows(sqlmock.NewRows([]string{"ID", "USER", "HOST", "DB", "COMMAND", "TIME", "STATE", "INFO"}))

	if _, err := collectProcessList(context.Background(), db, false, 60, "", "", 200); err != nil {
		t.Fatalf("collectProcessList: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestDBProcessList_UserAndDBFilters verifies both name filters bind their
// placeholders in the documented order: includeSleep filter, then minTime,
// then user, then db, then limit.
func TestDBProcessList_UserAndDBFilters(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectQuery(regexp.QuoteMeta("FROM information_schema.PROCESSLIST")).
		WithArgs("Sleep", "ops_rw", "acore_world", 50).
		WillReturnRows(sqlmock.NewRows([]string{"ID", "USER", "HOST", "DB", "COMMAND", "TIME", "STATE", "INFO"}))

	if _, err := collectProcessList(context.Background(), db, false, 0, "ops_rw", "acore_world", 50); err != nil {
		t.Fatalf("collectProcessList: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestDBProcessList_InfoTruncation verifies INFO longer than the cap is
// elided and infoTruncated:true is surfaced. This is the runaway-payload
// guard — a single multi-MB INSERT INTO ... VALUES (...) would otherwise blow
// up the MCP response shape.
func TestDBProcessList_InfoTruncation(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	bigSQL := strings.Repeat("x", dbProcessListInfoMaxBytes+200)
	mock.ExpectQuery(regexp.QuoteMeta("FROM information_schema.PROCESSLIST")).
		WithArgs("Sleep", 200).
		WillReturnRows(sqlmock.NewRows([]string{"ID", "USER", "HOST", "DB", "COMMAND", "TIME", "STATE", "INFO"}).
			AddRow(int64(99), "u", "h", "d", "Query", int64(0), "", bigSQL))

	out, err := collectProcessList(context.Background(), db, false, 0, "", "", 200)
	if err != nil {
		t.Fatalf("collectProcessList: %v", err)
	}
	rows := out["rows"].([]processListRow)
	if len(rows) != 1 {
		t.Fatalf("rowCount: %d", len(rows))
	}
	if !rows[0].InfoTruncated {
		t.Errorf("infoTruncated not set")
	}
	// len + 3 because the elision sentinel "…" is one rune (3 UTF-8 bytes)
	// appended after the truncation point.
	if len(rows[0].Info) != dbProcessListInfoMaxBytes+len("…") {
		t.Errorf("truncated info wrong length: %d", len(rows[0].Info))
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestDBProcessList_LimitClamping — the handler clamps limit<=0 to 200 and
// limit>500 to 500. Exercised via the handler path (not collectProcessList)
// so we cover the clamping logic.
func TestDBProcessList_LimitClamping(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	reg := NewRegistry()
	RegisterDBProcessListTool(reg, DBDeps{QueryDB: db})
	tool, _ := reg.Get("db_processlist")

	mock.ExpectQuery(regexp.QuoteMeta("FROM information_schema.PROCESSLIST")).
		WithArgs("Sleep", dbProcessListHardMax).
		WillReturnRows(sqlmock.NewRows([]string{"ID", "USER", "HOST", "DB", "COMMAND", "TIME", "STATE", "INFO"}))

	resp := tool.Handler(context.Background(), json.RawMessage(`{"limit":99999}`), "")
	got, _ := resp.(map[string]any)
	if msg, ok := got["error"]; ok {
		t.Fatalf("unexpected error: %v", msg)
	}
	if got["limit"].(int) != dbProcessListHardMax {
		t.Errorf("limit not clamped to hard max: %v", got["limit"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}
