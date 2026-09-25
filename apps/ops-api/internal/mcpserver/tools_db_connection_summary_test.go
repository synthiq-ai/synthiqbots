package mcpserver

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// TestDBConnSummary_DescriptionMentionsContext keeps the description anchored
// to the keywords the agent latches on to when an operator asks "where are
// the connections coming from?" / "who's hogging the pool?". Same shape as
// the db_processlist / db_innodb_status description guards.
func TestDBConnSummary_DescriptionMentionsContext(t *testing.T) {
	reg := NewRegistry()
	RegisterDBConnectionSummaryTool(reg, DBDeps{})
	tool, ok := reg.Get("db_connection_summary")
	if !ok {
		t.Fatal("db_connection_summary not registered")
	}
	for _, kw := range []string{"PROCESSLIST", "byUser", "byHost", "byDb", "byCommand", "byState", "topN", "PROCESS privilege"} {
		if !strings.Contains(tool.Description, kw) {
			t.Errorf("description missing %q: %s", kw, tool.Description)
		}
	}
}

// TestDBConnSummary_ReadOnlyAnnotation — ensures the tool is annotated
// read-only. Catches an accidental Destructive:true that would gate it
// behind OPS_ADMIN_ALLOW_ACTIONS.
func TestDBConnSummary_ReadOnlyAnnotation(t *testing.T) {
	reg := NewRegistry()
	RegisterDBConnectionSummaryTool(reg, DBDeps{})
	tool, _ := reg.Get("db_connection_summary")
	if tool.Destructive {
		t.Errorf("db_connection_summary marked destructive — should be read-only")
	}
	if !strings.Contains(string(tool.Annotations), "readOnlyHint") {
		t.Errorf("missing readOnlyHint annotation: %s", string(tool.Annotations))
	}
}

func TestDBConnSummary_NoPoolReturnsError(t *testing.T) {
	reg := NewRegistry()
	RegisterDBConnectionSummaryTool(reg, DBDeps{}) // QueryDB nil
	tool, _ := reg.Get("db_connection_summary")
	resp := tool.Handler(context.Background(), json.RawMessage(`{}`), "")
	got, _ := resp.(map[string]any)
	if msg, _ := got["error"].(string); !strings.Contains(msg, "ops_ro pool not configured") {
		t.Errorf("expected pool-not-configured error, got %v", got)
	}
}

// TestDBConnSummary_DefaultExcludesSleep mirrors db_processlist's same-name
// test. Sleep being excluded by default is the most important shaping
// decision of the tool — on a healthy server PROCESSLIST is mostly Sleep
// rows from the worldserver's pool, which would otherwise dominate the
// faceted counts.
func TestDBConnSummary_DefaultExcludesSleep(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectQuery(regexp.QuoteMeta("FROM information_schema.PROCESSLIST")).
		WithArgs("Sleep", dbConnSummaryScanCap+1).
		WillReturnRows(sqlmock.NewRows([]string{"USER", "HOST", "DB", "COMMAND", "STATE"}).
			AddRow("ops_ro", "172.17.0.5:55001", "acore_characters", "Query", "Sending data").
			AddRow("ops_ro", "172.17.0.5:55002", "acore_characters", "Query", "Sending data").
			AddRow("acore", "172.17.0.6:55102", "acore_world", "Query", "executing"))

	out, err := collectConnectionSummary(context.Background(), db, false, 0, 10)
	if err != nil {
		t.Fatalf("collectConnectionSummary: %v", err)
	}
	if got := out["total"].(int); got != 3 {
		t.Errorf("total: %d", got)
	}
	byUser := out["byUser"].([]dbConnSummaryEntry)
	if len(byUser) != 2 || byUser[0].Key != "ops_ro" || byUser[0].Count != 2 || byUser[1].Key != "acore" {
		t.Errorf("byUser unexpected: %+v", byUser)
	}
	byHost := out["byHost"].([]dbConnSummaryEntry)
	// port should be stripped → 172.17.0.5 collapses two rows, 172.17.0.6 stays at one
	if len(byHost) != 2 || byHost[0].Key != "172.17.0.5" || byHost[0].Count != 2 {
		t.Errorf("byHost host normalization broken: %+v", byHost)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestDBConnSummary_IncludeSleepDropsFilter — same shape as
// db_processlist's IncludeSleepDropsFilter test. WithArgs() will fail the
// test if the binding count or order differs from what we expect, so this
// catches accidental retention of the Sleep predicate.
func TestDBConnSummary_IncludeSleepDropsFilter(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectQuery(regexp.QuoteMeta("FROM information_schema.PROCESSLIST")).
		WithArgs(dbConnSummaryScanCap + 1). // only the LIMIT placeholder
		WillReturnRows(sqlmock.NewRows([]string{"USER", "HOST", "DB", "COMMAND", "STATE"}).
			AddRow("ops_ro", "h", "", "Sleep", ""))

	out, err := collectConnectionSummary(context.Background(), db, true, 0, 10)
	if err != nil {
		t.Fatalf("collectConnectionSummary: %v", err)
	}
	if got := out["total"].(int); got != 1 {
		t.Errorf("total: %d", got)
	}
	byCommand := out["byCommand"].([]dbConnSummaryEntry)
	if len(byCommand) != 1 || byCommand[0].Key != "Sleep" {
		t.Errorf("byCommand: %+v", byCommand)
	}
	byDb := out["byDb"].([]dbConnSummaryEntry)
	if len(byDb) != 1 || byDb[0].Key != "(none)" {
		t.Errorf("empty DB should bucket as (none): %+v", byDb)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestDBConnSummary_MinTimeFilter verifies the minTime predicate binds in
// the right position. Mirrors db_processlist's TestDBProcessList_MinTimeFilter.
func TestDBConnSummary_MinTimeFilter(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectQuery(regexp.QuoteMeta("AND TIME >= ?")).
		WithArgs("Sleep", int64(60), dbConnSummaryScanCap+1).
		WillReturnRows(sqlmock.NewRows([]string{"USER", "HOST", "DB", "COMMAND", "STATE"}))

	if _, err := collectConnectionSummary(context.Background(), db, false, 60, 10); err != nil {
		t.Fatalf("collectConnectionSummary: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestDBConnSummary_TopNClamping — the handler clamps topN<=0 to 10 and
// topN>50 to 50. Driven through the handler path so the clamping arithmetic
// is exercised.
func TestDBConnSummary_TopNClamping(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	reg := NewRegistry()
	RegisterDBConnectionSummaryTool(reg, DBDeps{QueryDB: db})
	tool, _ := reg.Get("db_connection_summary")

	mock.ExpectQuery(regexp.QuoteMeta("FROM information_schema.PROCESSLIST")).
		WithArgs("Sleep", dbConnSummaryScanCap+1).
		WillReturnRows(sqlmock.NewRows([]string{"USER", "HOST", "DB", "COMMAND", "STATE"}))

	resp := tool.Handler(context.Background(), json.RawMessage(`{"topN":99999}`), "")
	got, _ := resp.(map[string]any)
	if msg, ok := got["error"]; ok {
		t.Fatalf("unexpected error: %v", msg)
	}
	if got["topN"].(int) != dbConnSummaryHardMaxTop {
		t.Errorf("topN not clamped to hard max: %v", got["topN"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestDBConnSummary_TopNSlicing — facets bigger than topN get sliced. Also
// verifies the descending count ordering. 11 distinct users with descending
// counts → topN=3 must return the top 3 in count-desc order.
func TestDBConnSummary_TopNSlicing(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	rows := sqlmock.NewRows([]string{"USER", "HOST", "DB", "COMMAND", "STATE"})
	// user_i contributes (11-i) connections so user_0 wins with 11.
	for i := 0; i < 11; i++ {
		user := "user_" + string(rune('a'+i))
		for j := 0; j < (11 - i); j++ {
			rows.AddRow(user, "h", "d", "Query", "executing")
		}
	}
	mock.ExpectQuery(regexp.QuoteMeta("FROM information_schema.PROCESSLIST")).
		WithArgs("Sleep", dbConnSummaryScanCap+1).
		WillReturnRows(rows)

	out, err := collectConnectionSummary(context.Background(), db, false, 0, 3)
	if err != nil {
		t.Fatalf("collectConnectionSummary: %v", err)
	}
	byUser := out["byUser"].([]dbConnSummaryEntry)
	if len(byUser) != 3 {
		t.Fatalf("byUser len: %d", len(byUser))
	}
	if byUser[0].Key != "user_a" || byUser[0].Count != 11 {
		t.Errorf("top user wrong: %+v", byUser[0])
	}
	if byUser[1].Count != 10 || byUser[2].Count != 9 {
		t.Errorf("descending count broken: %+v", byUser)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestDBConnSummary_TopNTiebreakerKeyAsc — when two facet entries tie on
// count, ordering must fall back to key ascending so the response is
// deterministic. Without the tiebreaker Go's map iteration randomization
// would make this assertion flaky.
func TestDBConnSummary_TopNTiebreakerKeyAsc(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectQuery(regexp.QuoteMeta("FROM information_schema.PROCESSLIST")).
		WithArgs("Sleep", dbConnSummaryScanCap+1).
		WillReturnRows(sqlmock.NewRows([]string{"USER", "HOST", "DB", "COMMAND", "STATE"}).
			AddRow("zeta", "h", "d", "Query", "").
			AddRow("alpha", "h", "d", "Query", "").
			AddRow("middle", "h", "d", "Query", ""))

	out, err := collectConnectionSummary(context.Background(), db, false, 0, 10)
	if err != nil {
		t.Fatalf("collectConnectionSummary: %v", err)
	}
	byUser := out["byUser"].([]dbConnSummaryEntry)
	// All three tied at count=1 → must come back in key-asc order.
	if len(byUser) != 3 || byUser[0].Key != "alpha" || byUser[1].Key != "middle" || byUser[2].Key != "zeta" {
		t.Errorf("key tiebreaker broken: %+v", byUser)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestDBConnSummary_HostNormalization — host:port should collapse to host;
// bracketed ipv6 should keep the brackets; bare hostnames pass through.
// Load-bearing because per-port bucketing would shatter the facet — the
// operator wants "all connections from worker-1", not "23 single-count
// entries for worker-1:55001 through worker-1:55023".
func TestDBConnSummary_HostNormalization(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"172.17.0.5:55001", "172.17.0.5"},
		{"172.17.0.5", "172.17.0.5"},
		{"[::1]:55001", "[::1]"},
		{"[::1]", "[::1]"},
		{"localhost", "localhost"},
		{"", "(none)"},
		// Unbracketed bare ipv6 — last colon would be inside the address.
		// Defense: only strip if the suffix is all digits. `::1` is not
		// all-digits so leave it alone.
		{"::1", "::1"},
		// Hostname with a colon-suffix that isn't a port — must NOT strip.
		{"weird-host:notaport", "weird-host:notaport"},
	}
	for _, c := range cases {
		if got := normalizeHostKey(c.in); got != c.want {
			t.Errorf("normalizeHostKey(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestDBConnSummary_ScanCapTruncation — a flood of rows beyond
// dbConnSummaryScanCap must be capped, and `truncated:true` must be
// surfaced so the agent knows the aggregation is incomplete. The cap+1
// LIMIT lets us detect this in one round trip (no extra COUNT).
func TestDBConnSummary_ScanCapTruncation(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	rows := sqlmock.NewRows([]string{"USER", "HOST", "DB", "COMMAND", "STATE"})
	for i := 0; i < dbConnSummaryScanCap+50; i++ {
		rows.AddRow("u", "h", "d", "Query", "")
	}
	mock.ExpectQuery(regexp.QuoteMeta("FROM information_schema.PROCESSLIST")).
		WithArgs("Sleep", dbConnSummaryScanCap+1).
		WillReturnRows(rows)

	out, err := collectConnectionSummary(context.Background(), db, false, 0, 10)
	if err != nil {
		t.Fatalf("collectConnectionSummary: %v", err)
	}
	if !out["truncated"].(bool) {
		t.Errorf("truncated should be true when scan cap is hit")
	}
	if got := out["total"].(int); got != dbConnSummaryScanCap {
		t.Errorf("total should equal scanCap when truncated: got %d, want %d", got, dbConnSummaryScanCap)
	}
}
