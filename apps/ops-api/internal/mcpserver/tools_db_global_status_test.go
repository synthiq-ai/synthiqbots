package mcpserver

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

// TestDBGlobalStatus_DescriptionMentionsContext keeps the description anchored
// to the keywords the agent uses to pick this tool when an operator says
// "is the DB OK?" / "what's MySQL doing?" / "buffer pool hit ratio". Same
// shape as the db_table_info / db_explain / db_processlist guards.
func TestDBGlobalStatus_DescriptionMentionsContext(t *testing.T) {
	reg := NewRegistry()
	RegisterDBGlobalStatusTool(reg, DBDeps{})
	tool, ok := reg.Get("db_global_status")
	if !ok {
		t.Fatal("db_global_status not registered")
	}
	for _, kw := range []string{
		"SHOW GLOBAL STATUS",
		"SHOW GLOBAL VARIABLES",
		"connectionUsagePct",
		"innodbBufferPoolHitRatioPct",
		"queriesPerSecond",
		"checks",
		"allOk",
	} {
		if !strings.Contains(tool.Description, kw) {
			t.Errorf("description missing %q: %s", kw, tool.Description)
		}
	}
}

func TestDBGlobalStatus_NoPoolReturnsError(t *testing.T) {
	reg := NewRegistry()
	RegisterDBGlobalStatusTool(reg, DBDeps{}) // QueryDB nil
	tool, _ := reg.Get("db_global_status")
	resp := tool.Handler(context.Background(), json.RawMessage(`{}`), "")
	got, _ := resp.(map[string]any)
	if msg, _ := got["error"].(string); !strings.Contains(msg, "ops_ro pool not configured") {
		t.Errorf("expected pool-not-configured error, got %v", got)
	}
}

// TestCollectGlobalStatus_Golden drives the full collectGlobalStatus path with
// a mock connection. Exercises (1) curated subset extraction, (2) derived
// metric computation (qps, conn %, buffer hit ratio), (3) checks[] roll-up.
//
// Numbers chosen to make every threshold predictable:
//   - Threads_connected=40, max_connections=200 → 20% usage (under 80% threshold → ok)
//   - bufReads=100, bufReadRequests=10_000 → 99% hit ratio (above 95% floor → ok)
//   - Slow_queries=0 → ok
//   - Aborted_connects=10, Uptime=3600 → 10/hour (under 30/hour threshold → ok)
//   - read_only=OFF → ok
//     → allOk=true
func TestCollectGlobalStatus_Golden(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectQuery(regexp.QuoteMeta("SHOW GLOBAL STATUS")).
		WillReturnRows(sqlmock.NewRows([]string{"Variable_name", "Value"}).
			AddRow("Uptime", "3600").
			AddRow("Queries", "72000"). // 20 qps
			AddRow("Questions", "70000").
			AddRow("Threads_connected", "40").
			AddRow("Threads_running", "2").
			AddRow("Threads_created", "50").
			AddRow("Threads_cached", "10").
			AddRow("Connections", "100").
			AddRow("Aborted_clients", "5").
			AddRow("Aborted_connects", "10"). // 10/hour
			AddRow("Slow_queries", "0").
			AddRow("Com_select", "60000").
			AddRow("Com_insert", "5000").
			AddRow("Com_update", "4000").
			AddRow("Com_delete", "3000").
			AddRow("Com_commit", "11000").
			AddRow("Com_rollback", "100").
			AddRow("Innodb_buffer_pool_reads", "100").
			AddRow("Innodb_buffer_pool_read_requests", "10000"). // 99% hit
			AddRow("Innodb_buffer_pool_pages_total", "8192").
			AddRow("Innodb_buffer_pool_pages_free", "1000").
			AddRow("Innodb_buffer_pool_pages_dirty", "50").
			AddRow("Innodb_rows_read", "500000").
			AddRow("Innodb_rows_inserted", "5000").
			AddRow("Innodb_rows_updated", "4000").
			AddRow("Innodb_rows_deleted", "3000").
			AddRow("Table_locks_immediate", "9000").
			AddRow("Table_locks_waited", "100").
			AddRow("Open_tables", "200").
			AddRow("Opened_tables", "300").
			AddRow("Created_tmp_tables", "1000").
			AddRow("Created_tmp_disk_tables", "10"))

	mock.ExpectQuery(regexp.QuoteMeta("SHOW GLOBAL VARIABLES")).
		WillReturnRows(sqlmock.NewRows([]string{"Variable_name", "Value"}).
			AddRow("version", "8.0.36").
			AddRow("version_comment", "MySQL Community Server - GPL").
			AddRow("max_connections", "200").
			AddRow("innodb_buffer_pool_size", "134217728"). // 128 MiB
			AddRow("slow_query_log", "ON").
			AddRow("long_query_time", "1.000000").
			AddRow("log_slow_admin_statements", "OFF").
			AddRow("log_queries_not_using_indexes", "OFF").
			AddRow("read_only", "OFF").
			AddRow("super_read_only", "OFF"))

	// nil ring = no snapshot history, which pins these cases to the lifetime
	// basis — the behaviour these goldens were written against.
	out, err := collectGlobalStatus(context.Background(), db, nil, time.Now())
	if err != nil {
		t.Fatalf("collectGlobalStatus: %v", err)
	}

	if got := out["version"].(string); got != "8.0.36" {
		t.Errorf("version: %q", got)
	}
	if got := out["uptimeSeconds"].(int64); got != 3600 {
		t.Errorf("uptimeSeconds: %d", got)
	}
	if got := out["uptimeHuman"].(string); got != "1h" {
		t.Errorf("uptimeHuman: %q", got)
	}
	if got := out["threadsConnected"].(int64); got != 40 {
		t.Errorf("threadsConnected: %d", got)
	}
	if got := out["maxConnections"].(int64); got != 200 {
		t.Errorf("maxConnections: %d", got)
	}
	if got := out["connectionUsagePct"].(float64); got != 20.0 {
		t.Errorf("connectionUsagePct: %v (want 20.0)", got)
	}
	if got := out["queriesPerSecond"].(float64); got != 20.0 {
		t.Errorf("queriesPerSecond: %v (want 20.0)", got)
	}
	if got := out["innodbBufferPoolHitRatioPct"].(float64); got != 99.0 {
		t.Errorf("innodbBufferPoolHitRatioPct: %v (want 99.0)", got)
	}
	if got := out["innodbBufferPoolSize"].(int64); got != 134217728 {
		t.Errorf("innodbBufferPoolSize: %d", got)
	}
	if got := out["innodbBufferPoolSizeHuman"].(string); got != "128.00 MiB" {
		t.Errorf("innodbBufferPoolSizeHuman: %q", got)
	}
	if got := out["slowQueries"].(int64); got != 0 {
		t.Errorf("slowQueries: %d", got)
	}
	if got := out["slowQueryLog"].(bool); !got {
		t.Errorf("slowQueryLog should be true (ON)")
	}
	if got := out["longQueryTime"].(float64); got != 1.0 {
		t.Errorf("longQueryTime: %v", got)
	}
	if got := out["readOnly"].(bool); got {
		t.Errorf("readOnly should be false")
	}
	if got := out["abortedConnectsPerHour"].(float64); got != 10.0 {
		t.Errorf("abortedConnectsPerHour: %v (want 10.0)", got)
	}

	// All 5 thresholds should be met → allOk=true.
	checks := out["checks"].([]map[string]any)
	if len(checks) != 5 {
		t.Fatalf("expected 5 checks, got %d", len(checks))
	}
	if !out["allOk"].(bool) {
		t.Errorf("allOk should be true; checks=%v", checks)
	}
	for _, c := range checks {
		if !c["ok"].(bool) {
			t.Errorf("check %q should be ok: %v", c["name"], c)
		}
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestCollectGlobalStatus_FailingChecks flips every threshold simultaneously
// and verifies each `checks[]` entry reports ok=false. Catches the case where
// the threshold constants drift but the comparison directions don't update.
func TestCollectGlobalStatus_FailingChecks(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectQuery(regexp.QuoteMeta("SHOW GLOBAL STATUS")).
		WillReturnRows(sqlmock.NewRows([]string{"Variable_name", "Value"}).
			AddRow("Uptime", "3600").
			AddRow("Threads_connected", "180"). // 90% of 200 → over 80% threshold
			AddRow("Slow_queries", "42").       // non-zero
			AddRow("Aborted_connects", "500").  // 500/hour → over 30/hour threshold
			AddRow("Innodb_buffer_pool_reads", "5000").
			AddRow("Innodb_buffer_pool_read_requests", "10000")) // 50% hit ratio → under 95% floor

	mock.ExpectQuery(regexp.QuoteMeta("SHOW GLOBAL VARIABLES")).
		WillReturnRows(sqlmock.NewRows([]string{"Variable_name", "Value"}).
			AddRow("max_connections", "200").
			AddRow("read_only", "ON")) // writes disabled

	// nil ring = no snapshot history, which pins these cases to the lifetime
	// basis — the behaviour these goldens were written against.
	out, err := collectGlobalStatus(context.Background(), db, nil, time.Now())
	if err != nil {
		t.Fatalf("collectGlobalStatus: %v", err)
	}

	if out["allOk"].(bool) {
		t.Errorf("allOk should be false")
	}
	checks := out["checks"].([]map[string]any)
	if len(checks) != 5 {
		t.Fatalf("expected 5 checks, got %d", len(checks))
	}
	failed := map[string]bool{}
	for _, c := range checks {
		if !c["ok"].(bool) {
			failed[c["name"].(string)] = true
		}
	}
	wantFailed := []string{
		"connection_usage_under_threshold",
		"innodb_buffer_pool_hit_ratio_ok",
		"no_slow_queries",
		"aborted_connects_rate_under_threshold",
		"writes_enabled",
	}
	for _, name := range wantFailed {
		if !failed[name] {
			t.Errorf("expected check %q to fail; checks=%v", name, checks)
		}
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestCollectGlobalStatus_NoDerivedOnZeroUptime verifies that derived fields
// (qps, abortedConnectsPerHour) are NOT emitted when Uptime=0 — a freshly
// restarted server reports Uptime=0 transiently, and dividing by zero would
// produce NaN/Inf that JSON.Marshal refuses to serialize.
func TestCollectGlobalStatus_NoDerivedOnZeroUptime(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectQuery(regexp.QuoteMeta("SHOW GLOBAL STATUS")).
		WillReturnRows(sqlmock.NewRows([]string{"Variable_name", "Value"}).
			AddRow("Uptime", "0").
			AddRow("Queries", "0").
			AddRow("Threads_connected", "1").
			AddRow("Aborted_connects", "0").
			AddRow("Innodb_buffer_pool_reads", "0").
			AddRow("Innodb_buffer_pool_read_requests", "0"))
	mock.ExpectQuery(regexp.QuoteMeta("SHOW GLOBAL VARIABLES")).
		WillReturnRows(sqlmock.NewRows([]string{"Variable_name", "Value"}).
			AddRow("max_connections", "200").
			AddRow("read_only", "OFF"))

	// nil ring = no snapshot history, which pins these cases to the lifetime
	// basis — the behaviour these goldens were written against.
	out, err := collectGlobalStatus(context.Background(), db, nil, time.Now())
	if err != nil {
		t.Fatalf("collectGlobalStatus: %v", err)
	}
	if _, ok := out["queriesPerSecond"]; ok {
		t.Errorf("queriesPerSecond should be absent on Uptime=0")
	}
	if _, ok := out["abortedConnectsPerHour"]; ok {
		t.Errorf("abortedConnectsPerHour should be absent on Uptime=0")
	}
	// Buffer pool hit ratio also gated by bufReadRequests > 0.
	if _, ok := out["innodbBufferPoolHitRatioPct"]; ok {
		t.Errorf("innodbBufferPoolHitRatioPct should be absent on read_requests=0")
	}
	// allOk must still be computable — the checks that ARE present should pass.
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestIsOn pins the boolean-variable normalizer. MySQL/MariaDB emit different
// truthy shapes for the same flag across versions; isOn collapses them.
func TestIsOn(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"ON", true},
		{"on", true},
		{"1", true},
		{"TRUE", true},
		{"true", true},
		{"OFF", false},
		{"off", false},
		{"0", false},
		{"", false},
		{"  ON  ", false}, // strict — driver gives clean values
		{"YES", false},    // we don't claim YES handling
	}
	for _, c := range cases {
		if got := isOn(c.in); got != c.want {
			t.Errorf("isOn(%q) = %v want %v", c.in, got, c.want)
		}
	}
}

// TestUint64Of guards the negative-floor behavior. humanBytes takes uint64,
// and a negative status counter (driver quirk) would wrap to ~18 EiB without
// the floor.
func TestUint64Of(t *testing.T) {
	cases := []struct {
		in   int64
		want uint64
	}{
		{0, 0},
		{1, 1},
		{134217728, 134217728},
		{-1, 0},
		{-1 << 62, 0},
	}
	for _, c := range cases {
		if got := uint64Of(c.in); got != c.want {
			t.Errorf("uint64Of(%d) = %d want %d", c.in, got, c.want)
		}
	}
}
