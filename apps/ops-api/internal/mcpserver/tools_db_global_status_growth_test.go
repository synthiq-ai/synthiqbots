package mcpserver

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

// gsKV is one SHOW GLOBAL STATUS row. A slice of these (rather than a map)
// keeps the fixture order stable so a scan-order-dependent bug would be
// reproducible rather than flaky.
type gsKV struct {
	name  string
	value string
}

func gsDefaultOpts() dbGSGrowthOpts {
	return dbGSGrowthOpts{TopN: dbGSGrowthDefaultTop, SortKey: "rate"}
}

// gsCall drives one full collectGlobalStatusGrowth pass against a fresh sqlmock
// connection, using the caller's own ring so the process-wide dbGSGrowthRing is
// never touched.
func gsCall(t *testing.T, ring *dbGSSnapshotRing, now time.Time, opts dbGSGrowthOpts, fixture []gsKV) map[string]any {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	rows := sqlmock.NewRows([]string{"Variable_name", "Value"})
	for _, kv := range fixture {
		rows.AddRow(kv.name, kv.value)
	}
	mock.ExpectQuery(regexp.QuoteMeta("SHOW GLOBAL STATUS")).WillReturnRows(rows)

	out, err := collectGlobalStatusGrowth(context.Background(), db, ring, now, opts)
	if err != nil {
		t.Fatalf("collectGlobalStatusGrowth: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
	return out
}

func gsEntries(t *testing.T, out map[string]any) []dbGSGrowthEntry {
	t.Helper()
	list, ok := out["variables"].([]dbGSGrowthEntry)
	if !ok {
		t.Fatalf("variables is not []dbGSGrowthEntry: %T", out["variables"])
	}
	return list
}

func gsByName(t *testing.T, out map[string]any) map[string]dbGSGrowthEntry {
	t.Helper()
	m := map[string]dbGSGrowthEntry{}
	for _, e := range gsEntries(t, out) {
		m[e.Variable] = e
	}
	return m
}

func gsNames(t *testing.T, out map[string]any) []string {
	t.Helper()
	var names []string
	for _, e := range gsEntries(t, out) {
		names = append(names, e.Variable)
	}
	return names
}

func wantInt64(t *testing.T, label string, got *int64, want int64) {
	t.Helper()
	if got == nil {
		t.Errorf("%s = nil, want %d", label, want)
		return
	}
	if *got != want {
		t.Errorf("%s = %d, want %d", label, *got, want)
	}
}

func wantNilInt64(t *testing.T, label string, got *int64) {
	t.Helper()
	if got != nil {
		t.Errorf("%s = %d, want nil", label, *got)
	}
}

func wantFloat(t *testing.T, label string, got *float64, want float64) {
	t.Helper()
	if got == nil {
		t.Errorf("%s = nil, want %v", label, want)
		return
	}
	if *got != want {
		t.Errorf("%s = %v, want %v", label, *got, want)
	}
}

// gsGoldenBaseline / gsGoldenCurrent are the shared fixture pair for the golden
// and sort tests. Baseline Uptime 3600 -> current 3660 gives a 60-second
// uptime delta, so every rate below is a clean literal rather than a
// hand-computed irrational quotient.
func gsGoldenBaseline() []gsKV {
	return []gsKV{
		{"Uptime", "3600"},
		{"Questions", "100000"},
		{"Com_select", "50000"},
		{"Aborted_connects", "100"},
		{"Threads_connected", "40"},
		{"Threads_created", "500"},
		{"Innodb_buffer_pool_pages_free", "1000"},
		{"Innodb_buffer_pool_pages_flushed", "7000"},
		{"Slow_queries", "3"},
		{"Qcache_hits", "5000"},
	}
}

func gsGoldenCurrent() []gsKV {
	return []gsKV{
		{"Uptime", "3660"},
		{"Questions", "106000"},
		{"Com_select", "50600"},
		{"Aborted_connects", "101"},
		{"Threads_connected", "8"},
		{"Threads_created", "512"},
		{"Innodb_buffer_pool_pages_free", "900"},
		{"Innodb_buffer_pool_pages_flushed", "7300"},
		{"Slow_queries", "3"},
		// New since the baseline — no prior reading, so no delta.
		{"Innodb_rows_read", "900000"},
		// Non-numeric: a real server returns a sentence here.
		{"Innodb_buffer_pool_dump_status", "not started"},
		// Qcache_hits is absent: it VANISHED between readings.
	}
}

// TestDBGSGrowth_DescriptionMentionsContext anchors the description to the
// keywords an agent latches on to for "what is the DB actually doing right
// now / what is the rate", plus the cross-references that route it to the
// point-in-time and per-connection tools instead. Asserts the registered
// Description STRING, not the Go doc-comment.
func TestDBGSGrowth_DescriptionMentionsContext(t *testing.T) {
	reg := NewRegistry()
	RegisterDBGlobalStatusGrowthTool(reg, DBDeps{})
	tool, ok := reg.Get("db_global_status_growth")
	if !ok {
		t.Fatal("db_global_status_growth not registered")
	}
	for _, kw := range []string{
		"SHOW GLOBAL STATUS", "db_global_status", "db_connection_summary_growth", "db_processlist",
		"counter", "gauge", "perSecond", "perHour", "delta", "prevValue",
		"snapshotId", "storedSnapshots", "firstSnapshot", "serverRestarted",
		"rateBasis", "Uptime", "Threads_created", "Innodb_buffer_pool_pages_flushed",
		"topN", "sortBy", "prefix", "curatedOnly", "includeUnchanged",
		"variablesScanned", "variablesMoved", "variablesVanished", "variablesSkippedNonNumeric",
		"ops_ro", "Read-only", "PROCESS",
	} {
		if !strings.Contains(tool.Description, kw) {
			t.Errorf("description missing %q", kw)
		}
	}
}

// TestDBGSGrowth_ReadOnlyAnnotation — recording a snapshot is in-process
// bookkeeping, not a server mutation; the tool must stay read-only or it would
// be gated behind OPS_ADMIN_ALLOW_ACTIONS.
func TestDBGSGrowth_ReadOnlyAnnotation(t *testing.T) {
	reg := NewRegistry()
	RegisterDBGlobalStatusGrowthTool(reg, DBDeps{})
	tool, _ := reg.Get("db_global_status_growth")
	if tool.Destructive {
		t.Errorf("db_global_status_growth marked destructive — should be read-only")
	}
	if !strings.Contains(string(tool.Annotations), "readOnlyHint") {
		t.Errorf("missing readOnlyHint annotation: %s", string(tool.Annotations))
	}
}

func TestDBGSGrowth_NoPoolReturnsError(t *testing.T) {
	reg := NewRegistry()
	RegisterDBGlobalStatusGrowthTool(reg, DBDeps{}) // QueryDB nil
	tool, _ := reg.Get("db_global_status_growth")
	resp := tool.Handler(context.Background(), json.RawMessage(`{}`), "")
	got, _ := resp.(map[string]any)
	if msg, _ := got["error"].(string); !strings.Contains(msg, "ops_ro pool not configured") {
		t.Errorf("expected pool-not-configured error, got %v", got)
	}
}

// TestDBGSGrowth_InvalidSortByRejected — an unknown sortBy must error BEFORE
// the read (no ExpectQuery is queued, so a stray query would fail the mock).
// Case matters: "Rate" is not "rate".
func TestDBGSGrowth_InvalidSortByRejected(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	reg := NewRegistry()
	RegisterDBGlobalStatusGrowthTool(reg, DBDeps{QueryDB: db})
	tool, _ := reg.Get("db_global_status_growth")

	for _, bad := range []string{
		`{"sortBy":"Rate"}`,
		`{"sortBy":"growth"}`,
		`{"sortBy":"name; DROP TABLE x"}`,
	} {
		resp := tool.Handler(context.Background(), json.RawMessage(bad), "")
		got, _ := resp.(map[string]any)
		if msg, _ := got["error"].(string); !strings.Contains(msg, "invalid sortBy") {
			t.Errorf("args %s: expected invalid sortBy error, got %v", bad, got)
		}
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestDBGSGrowth_TopNClamping — topN<=0 falls back to 15, topN>100 clamps to
// 100, and the post-clamp value is echoed so the agent can see the cut.
func TestDBGSGrowth_TopNClamping(t *testing.T) {
	cases := []struct {
		args string
		want int
	}{
		{`{}`, dbGSGrowthDefaultTop},
		{`{"topN":0}`, dbGSGrowthDefaultTop},
		{`{"topN":-5}`, dbGSGrowthDefaultTop},
		{`{"topN":7}`, 7},
		{`{"topN":99999}`, dbGSGrowthHardMaxTop},
	}
	for _, c := range cases {
		db, mock, err := sqlmock.New()
		if err != nil {
			t.Fatal(err)
		}
		reg := NewRegistry()
		RegisterDBGlobalStatusGrowthTool(reg, DBDeps{QueryDB: db})
		tool, _ := reg.Get("db_global_status_growth")
		mock.ExpectQuery(regexp.QuoteMeta("SHOW GLOBAL STATUS")).
			WillReturnRows(sqlmock.NewRows([]string{"Variable_name", "Value"}).AddRow("Uptime", "10"))

		resp := tool.Handler(context.Background(), json.RawMessage(c.args), "")
		got, _ := resp.(map[string]any)
		if msg, ok := got["error"]; ok {
			t.Fatalf("args %s: unexpected error: %v", c.args, msg)
		}
		if got["topN"].(int) != c.want {
			t.Errorf("args %s: topN = %v, want %d", c.args, got["topN"], c.want)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("args %s: unmet mock expectations: %v", c.args, err)
		}
		db.Close()
	}
}

// TestDBGSGrowth_FirstSnapshotHasNoBaseline — the very first call has nothing
// to diff against. Unlike db_connection_summary_growth (whose facets are
// point-in-time counts, where delta==count is at least coherent), a GLOBAL
// STATUS counter is cumulative since server start: reporting delta==value would
// claim an entire lifetime of accrual happened in this interval, which is the
// exact misreading the tool exists to prevent. So every delta is NULL and the
// response says so.
func TestDBGSGrowth_FirstSnapshotHasNoBaseline(t *testing.T) {
	ring := newDBGSSnapshotRing(5)
	out := gsCall(t, ring, time.Unix(1000, 0), gsDefaultOpts(), gsGoldenBaseline())

	if first, _ := out["firstSnapshot"].(bool); !first {
		t.Errorf("firstSnapshot = %v, want true", out["firstSnapshot"])
	}
	if out["comparedTo"] != nil {
		t.Errorf("comparedTo = %v, want nil", out["comparedTo"])
	}
	if out["rateBasis"] != nil {
		t.Errorf("rateBasis = %v, want nil", out["rateBasis"])
	}
	if out["snapshotId"] != "gs-1" {
		t.Errorf("snapshotId = %v, want gs-1", out["snapshotId"])
	}
	if out["variablesMoved"].(int) != 0 {
		t.Errorf("variablesMoved = %v, want 0", out["variablesMoved"])
	}
	for _, e := range gsEntries(t, out) {
		wantNilInt64(t, e.Variable+".delta", e.Delta)
		wantNilInt64(t, e.Variable+".prevValue", e.PrevValue)
		if e.PerSecond != nil {
			t.Errorf("%s.perSecond = %v, want nil on first snapshot", e.Variable, *e.PerSecond)
		}
		if e.Value == nil {
			t.Errorf("%s.value = nil, want the current reading", e.Variable)
		}
	}
	// Every sort scalar is nil, so the ordering must degrade cleanly to
	// alphabetical instead of shuffling with Go's map iteration.
	got := gsNames(t, out)
	if len(got) != 10 || got[0] != "Aborted_connects" || got[len(got)-1] != "Uptime" {
		t.Errorf("first-call order not alphabetical: %v", got)
	}
	notes, _ := out["notes"].([]string)
	if len(notes) == 0 || !strings.Contains(strings.Join(notes, " "), "no prior snapshot") {
		t.Errorf("expected a no-prior-snapshot note, got %v", notes)
	}
}

// TestDBGSGrowth_GoldenRateAgainstPriorSnapshot is the whole thesis in one
// fixture: cumulative counters become rates, gauges get a delta but no rate,
// an unchanged variable is hidden, a new one has no delta, a vanished one has
// no VALUE (not a zero), and the honest census counts the full union.
func TestDBGSGrowth_GoldenRateAgainstPriorSnapshot(t *testing.T) {
	ring := newDBGSSnapshotRing(5)
	t0 := time.Unix(1_700_000_000, 0)
	_ = gsCall(t, ring, t0, gsDefaultOpts(), gsGoldenBaseline())
	out := gsCall(t, ring, t0.Add(60*time.Second), gsDefaultOpts(), gsGoldenCurrent())

	if first, _ := out["firstSnapshot"].(bool); first {
		t.Errorf("firstSnapshot = true on the second call")
	}
	if out["comparedTo"] != "gs-1" {
		t.Errorf("comparedTo = %v, want gs-1", out["comparedTo"])
	}
	if restarted, _ := out["serverRestarted"].(bool); restarted {
		t.Errorf("serverRestarted = true; the fixture only VANISHES a counter, it never resets one")
	}
	if out["rateBasis"] != "uptime" {
		t.Errorf("rateBasis = %v, want uptime", out["rateBasis"])
	}
	if out["rateIntervalSeconds"].(int64) != 60 {
		t.Errorf("rateIntervalSeconds = %v, want 60", out["rateIntervalSeconds"])
	}

	// Honest census over the full 11-key union (10 current numeric + the
	// vanished Qcache_hits), independent of the display cut.
	for _, c := range []struct {
		key  string
		want int
	}{
		{"variablesScanned", 10},
		{"variablesSkippedNonNumeric", 1},
		{"gaugesTracked", 3},   // Uptime, Threads_connected, Innodb_buffer_pool_pages_free
		{"countersTracked", 8}, // the remaining union members
		{"variablesNew", 1},
		{"variablesVanished", 1},
		{"variablesMoved", 8},    // everything with a known non-zero delta
		{"variablesMatched", 10}, // 11 union minus the unchanged Slow_queries
	} {
		if got := out[c.key].(int); got != c.want {
			t.Errorf("%s = %d, want %d", c.key, got, c.want)
		}
	}

	byName := gsByName(t, out)

	// Counters: delta plus a rate on both scales.
	q := byName["Questions"]
	if q.Kind != "counter" {
		t.Errorf("Questions kind = %q, want counter", q.Kind)
	}
	wantInt64(t, "Questions.value", q.Value, 106000)
	wantInt64(t, "Questions.prevValue", q.PrevValue, 100000)
	wantInt64(t, "Questions.delta", q.Delta, 6000)
	wantFloat(t, "Questions.perSecond", q.PerSecond, 100.0)
	wantFloat(t, "Questions.perHour", q.PerHour, 360000.0)

	// A slow counter is exactly where the raw lifetime value misleads most:
	// 101 aborted connects since boot vs 60/hour right now.
	ac := byName["Aborted_connects"]
	wantInt64(t, "Aborted_connects.delta", ac.Delta, 1)
	wantFloat(t, "Aborted_connects.perSecond", ac.PerSecond, 0.017)
	wantFloat(t, "Aborted_connects.perHour", ac.PerHour, 60.0)

	// Gauges: a delta, but never a rate — "Threads_connected per second" is
	// nonsense, and a negative one doubly so.
	tc := byName["Threads_connected"]
	if tc.Kind != "gauge" {
		t.Errorf("Threads_connected kind = %q, want gauge", tc.Kind)
	}
	wantInt64(t, "Threads_connected.delta", tc.Delta, -32)
	if tc.PerSecond != nil || tc.PerHour != nil {
		t.Errorf("Threads_connected got a rate: %v/%v", tc.PerSecond, tc.PerHour)
	}

	// New variable: present now, absent from the baseline. Its lifetime total
	// did NOT accrue during this interval, so prevValue/delta stay null.
	nv := byName["Innodb_rows_read"]
	wantInt64(t, "Innodb_rows_read.value", nv.Value, 900000)
	wantNilInt64(t, "Innodb_rows_read.prevValue", nv.PrevValue)
	wantNilInt64(t, "Innodb_rows_read.delta", nv.Delta)

	// Vanished variable: value is NULL, not 0 — we have no current reading, and
	// claiming zero would invent a 5000-count collapse.
	vv := byName["Qcache_hits"]
	if _, ok := byName["Qcache_hits"]; !ok {
		t.Fatal("vanished Qcache_hits dropped from the union")
	}
	wantNilInt64(t, "Qcache_hits.value", vv.Value)
	wantInt64(t, "Qcache_hits.prevValue", vv.PrevValue, 5000)
	wantNilInt64(t, "Qcache_hits.delta", vv.Delta)

	// Unchanged variable hidden by default.
	if _, ok := byName["Slow_queries"]; ok {
		t.Errorf("Slow_queries (delta 0) should be hidden unless includeUnchanged")
	}
	// Non-numeric row never becomes an entry.
	if _, ok := byName["Innodb_buffer_pool_dump_status"]; ok {
		t.Errorf("non-numeric Innodb_buffer_pool_dump_status leaked into the entries")
	}

	// Default sort is by rate, so the busiest counter leads and the rateless
	// entries fall to the back ordered by movement magnitude.
	want := []string{
		"Questions", "Com_select", "Innodb_buffer_pool_pages_flushed", "Threads_created",
		"Aborted_connects", "Innodb_buffer_pool_pages_free", "Uptime", "Threads_connected",
		"Innodb_rows_read", "Qcache_hits",
	}
	got := gsNames(t, out)
	if len(got) != len(want) {
		t.Fatalf("entry count = %d, want %d (%v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("rate order[%d] = %s, want %s (full: %v)", i, got[i], want[i], got)
		}
	}
}

// TestDBGSGrowth_GaugeAllowlistIsExactNotPrefix is the regression that stops a
// future "simplification" into prefix matching. Two prefix families are mixed:
// Threads_created is a cumulative counter among gauge siblings, and
// Innodb_buffer_pool_pages_flushed is a counter among gauge siblings. A prefix
// rule would strip both of their rates.
func TestDBGSGrowth_GaugeAllowlistIsExactNotPrefix(t *testing.T) {
	cases := []struct {
		name string
		want string
	}{
		{"Threads_connected", "gauge"},
		{"Threads_running", "gauge"},
		{"Threads_cached", "gauge"},
		{"Threads_created", "counter"},
		{"Innodb_buffer_pool_pages_free", "gauge"},
		{"Innodb_buffer_pool_pages_total", "gauge"},
		{"Innodb_buffer_pool_pages_flushed", "counter"},
		{"Qcache_free_blocks", "gauge"},
		{"Qcache_hits", "counter"},
		{"Slave_open_temp_tables", "gauge"},
		{"Slave_retried_transactions", "counter"},
		{"Open_tables", "gauge"},
		{"Opened_tables", "counter"},
		{"Uptime", "gauge"},
		{"Questions", "counter"},
		{"Some_plugin_counter", "counter"}, // unknown defaults to counter
	}
	for _, c := range cases {
		if got := dbGSKind(c.name); got != c.want {
			t.Errorf("dbGSKind(%q) = %q, want %q", c.name, got, c.want)
		}
	}

	// And end-to-end: the counter siblings must actually receive a rate while
	// the gauge siblings must not.
	ring := newDBGSSnapshotRing(5)
	t0 := time.Unix(2000, 0)
	base := []gsKV{
		{"Uptime", "100"}, {"Threads_created", "10"}, {"Threads_connected", "10"},
		{"Innodb_buffer_pool_pages_flushed", "10"}, {"Innodb_buffer_pool_pages_free", "10"},
	}
	cur := []gsKV{
		{"Uptime", "110"}, {"Threads_created", "30"}, {"Threads_connected", "30"},
		{"Innodb_buffer_pool_pages_flushed", "30"}, {"Innodb_buffer_pool_pages_free", "30"},
	}
	_ = gsCall(t, ring, t0, gsDefaultOpts(), base)
	byName := gsByName(t, gsCall(t, ring, t0.Add(10*time.Second), gsDefaultOpts(), cur))

	for _, n := range []string{"Threads_created", "Innodb_buffer_pool_pages_flushed"} {
		wantFloat(t, n+".perSecond", byName[n].PerSecond, 2.0)
	}
	for _, n := range []string{"Threads_connected", "Innodb_buffer_pool_pages_free", "Uptime"} {
		if byName[n].PerSecond != nil {
			t.Errorf("%s got a rate %v, want nil (gauge)", n, *byName[n].PerSecond)
		}
		wantInt64(t, n+".delta", byName[n].Delta, map[string]int64{
			"Threads_connected": 20, "Innodb_buffer_pool_pages_free": 20, "Uptime": 10,
		}[n])
	}
}

// TestDBGSGrowth_UptimeBackwardsDetectsRestart — the primary restart signal.
// Counter deltas and every rate are suppressed; gauge deltas survive because a
// gauge is an independent point-in-time level, not an accumulation.
func TestDBGSGrowth_UptimeBackwardsDetectsRestart(t *testing.T) {
	ring := newDBGSSnapshotRing(5)
	t0 := time.Unix(3000, 0)
	_ = gsCall(t, ring, t0, gsDefaultOpts(), []gsKV{
		{"Uptime", "86400"}, {"Questions", "5000000"}, {"Threads_connected", "40"},
	})
	out := gsCall(t, ring, t0.Add(300*time.Second), gsDefaultOpts(), []gsKV{
		{"Uptime", "30"}, {"Questions", "1200"}, {"Threads_connected", "6"},
	})

	if restarted, _ := out["serverRestarted"].(bool); !restarted {
		t.Fatalf("serverRestarted = false after Uptime went 86400 -> 30")
	}
	if out["rateBasis"] != nil {
		t.Errorf("rateBasis = %v, want nil across a restart", out["rateBasis"])
	}
	byName := gsByName(t, out)
	// The counter: had we subtracted, we'd have published -4998800.
	wantNilInt64(t, "Questions.delta", byName["Questions"].Delta)
	if byName["Questions"].PerSecond != nil {
		t.Errorf("Questions kept a rate across a restart")
	}
	wantInt64(t, "Questions.value", byName["Questions"].Value, 1200)
	wantInt64(t, "Questions.prevValue", byName["Questions"].PrevValue, 5000000)
	// The gauge is still comparable.
	wantInt64(t, "Threads_connected.delta", byName["Threads_connected"].Delta, -34)

	notes, _ := out["notes"].([]string)
	if !strings.Contains(strings.Join(notes, " "), "server restarted") {
		t.Errorf("expected a restart note, got %v", notes)
	}
}

// TestDBGSGrowth_CounterDecreaseDetectsRestart — the secondary signal, for the
// case where Uptime looks plausible (a fast restart between two distant calls)
// but a monotonic counter has clearly been reset.
func TestDBGSGrowth_CounterDecreaseDetectsRestart(t *testing.T) {
	ring := newDBGSSnapshotRing(5)
	t0 := time.Unix(4000, 0)
	_ = gsCall(t, ring, t0, gsDefaultOpts(), []gsKV{
		{"Uptime", "100"}, {"Questions", "999999"},
	})
	out := gsCall(t, ring, t0.Add(3600*time.Second), gsDefaultOpts(), []gsKV{
		{"Uptime", "200"}, {"Questions", "42"}, // uptime advanced, counter did not
	})
	if restarted, _ := out["serverRestarted"].(bool); !restarted {
		t.Errorf("serverRestarted = false though Questions fell 999999 -> 42")
	}
}

// TestDBGSGrowth_VanishedVariableDoesNotTripRestart — the subtle one. A
// variable present in the baseline and gone now is NOT a counter decrease; if
// the restart heuristic walked the union blindly it would read the absence as a
// fall to zero and suppress every rate on a perfectly healthy server.
func TestDBGSGrowth_VanishedVariableDoesNotTripRestart(t *testing.T) {
	ring := newDBGSSnapshotRing(5)
	t0 := time.Unix(5000, 0)
	_ = gsCall(t, ring, t0, gsDefaultOpts(), []gsKV{
		{"Uptime", "1000"}, {"Questions", "100"}, {"Qcache_hits", "999999"},
	})
	out := gsCall(t, ring, t0.Add(10*time.Second), gsDefaultOpts(), []gsKV{
		{"Uptime", "1010"}, {"Questions", "200"},
	})
	if restarted, _ := out["serverRestarted"].(bool); restarted {
		t.Fatalf("serverRestarted = true because Qcache_hits vanished — a vanished variable is not a reset counter")
	}
	byName := gsByName(t, out)
	wantFloat(t, "Questions.perSecond", byName["Questions"].PerSecond, 10.0)
	if out["variablesVanished"].(int) != 1 {
		t.Errorf("variablesVanished = %v, want 1", out["variablesVanished"])
	}
}

// TestDBGSGrowth_RateBasisPrefersUptimeOverWallClock — the server's own clock
// is the honest denominator; a skewed or paused client clock must not distort
// the rate. The wall-clock gap here is 600 s while the server only advanced
// 60 s, and the rate must follow the server.
func TestDBGSGrowth_RateBasisPrefersUptimeOverWallClock(t *testing.T) {
	ring := newDBGSSnapshotRing(5)
	t0 := time.Unix(6000, 0)
	_ = gsCall(t, ring, t0, gsDefaultOpts(), []gsKV{{"Uptime", "1000"}, {"Questions", "0"}})
	out := gsCall(t, ring, t0.Add(600*time.Second), gsDefaultOpts(), []gsKV{{"Uptime", "1060"}, {"Questions", "600"}})

	if out["rateBasis"] != "uptime" {
		t.Errorf("rateBasis = %v, want uptime", out["rateBasis"])
	}
	if out["intervalSeconds"].(int) != 600 {
		t.Errorf("intervalSeconds = %v, want 600 (the wall clock is still reported)", out["intervalSeconds"])
	}
	if out["rateIntervalSeconds"].(int64) != 60 {
		t.Errorf("rateIntervalSeconds = %v, want 60", out["rateIntervalSeconds"])
	}
	byName := gsByName(t, out)
	// 600/60 = 10/s, NOT 600/600 = 1/s.
	wantFloat(t, "Questions.perSecond", byName["Questions"].PerSecond, 10.0)
}

// TestDBGSGrowth_WallClockFallbackWhenUptimeFlat — a sub-second-resolution
// Uptime can read identically across two quick calls; rather than dropping
// rates entirely, fall back to the wall clock and say so.
func TestDBGSGrowth_WallClockFallbackWhenUptimeFlat(t *testing.T) {
	ring := newDBGSSnapshotRing(5)
	t0 := time.Unix(7000, 0)
	_ = gsCall(t, ring, t0, gsDefaultOpts(), []gsKV{{"Uptime", "1000"}, {"Questions", "0"}})
	out := gsCall(t, ring, t0.Add(20*time.Second), gsDefaultOpts(), []gsKV{{"Uptime", "1000"}, {"Questions", "100"}})

	if out["rateBasis"] != "wallClock" {
		t.Fatalf("rateBasis = %v, want wallClock", out["rateBasis"])
	}
	byName := gsByName(t, out)
	wantFloat(t, "Questions.perSecond", byName["Questions"].PerSecond, 5.0)
	notes, _ := out["notes"].([]string)
	if !strings.Contains(strings.Join(notes, " "), "wall-clock") {
		t.Errorf("expected a wall-clock fallback note, got %v", notes)
	}
}

// TestDBGSGrowth_ZeroIntervalSuppressesRates — two calls inside the same second
// have no usable denominator. Deltas are still real; rates are not.
func TestDBGSGrowth_ZeroIntervalSuppressesRates(t *testing.T) {
	ring := newDBGSSnapshotRing(5)
	t0 := time.Unix(8000, 0)
	_ = gsCall(t, ring, t0, gsDefaultOpts(), []gsKV{{"Uptime", "1000"}, {"Questions", "0"}})
	out := gsCall(t, ring, t0, gsDefaultOpts(), []gsKV{{"Uptime", "1000"}, {"Questions", "50"}})

	if out["rateBasis"] != nil {
		t.Errorf("rateBasis = %v, want nil for a zero interval", out["rateBasis"])
	}
	byName := gsByName(t, out)
	wantInt64(t, "Questions.delta", byName["Questions"].Delta, 50)
	if byName["Questions"].PerSecond != nil {
		t.Errorf("Questions.perSecond = %v, want nil (no interval to divide by)", *byName["Questions"].PerSecond)
	}
	notes, _ := out["notes"].([]string)
	if !strings.Contains(strings.Join(notes, " "), "less than a second apart") {
		t.Errorf("expected a zero-interval note, got %v", notes)
	}
}

// TestDBGSGrowth_SortOrders drives all four keys over ONE fixture where every
// key yields a different leader, so a broken comparator cannot coincidentally
// pass.
func TestDBGSGrowth_SortOrders(t *testing.T) {
	cases := []struct {
		sortKey string
		wantTop string
	}{
		{"rate", "Questions"},                     // busiest counter
		{"delta", "Questions"},                    // biggest absolute rise
		{"drop", "Innodb_buffer_pool_pages_free"}, // biggest decrease
		{"name", "Aborted_connects"},              // alphabetical
	}
	for _, c := range cases {
		ring := newDBGSSnapshotRing(5)
		t0 := time.Unix(9000, 0)
		opts := gsDefaultOpts()
		opts.SortKey = c.sortKey
		_ = gsCall(t, ring, t0, opts, gsGoldenBaseline())
		out := gsCall(t, ring, t0.Add(60*time.Second), opts, gsGoldenCurrent())

		got := gsNames(t, out)
		if len(got) == 0 {
			t.Fatalf("sortBy=%s returned no entries", c.sortKey)
		}
		if got[0] != c.wantTop {
			t.Errorf("sortBy=%s top = %s, want %s (full: %v)", c.sortKey, got[0], c.wantTop, got)
		}
	}
}

// TestDBGSGrowth_SortNilsAlwaysLast — an entry with no sort scalar (new or
// vanished variable) must never outrank one that has a real number, on any key.
func TestDBGSGrowth_SortNilsAlwaysLast(t *testing.T) {
	for _, key := range []string{"rate", "delta", "drop"} {
		ring := newDBGSSnapshotRing(5)
		t0 := time.Unix(9500, 0)
		opts := gsDefaultOpts()
		opts.SortKey = key
		_ = gsCall(t, ring, t0, opts, gsGoldenBaseline())
		out := gsCall(t, ring, t0.Add(60*time.Second), opts, gsGoldenCurrent())

		names := gsNames(t, out)
		byName := gsByName(t, out)
		seenNil := false
		for _, n := range names {
			isNil := byName[n].Delta == nil
			if seenNil && !isNil {
				t.Errorf("sortBy=%s: %s (with a delta) sorted after a nil-delta entry: %v", key, n, names)
				break
			}
			seenNil = seenNil || isNil
		}
	}
}

// TestDBGSGrowth_PrefixFilter — the prefix is a Go-side string filter, never
// interpolated into SQL, and it is case-insensitive so "com_" finds "Com_*".
func TestDBGSGrowth_PrefixFilter(t *testing.T) {
	ring := newDBGSSnapshotRing(5)
	t0 := time.Unix(10000, 0)
	opts := gsDefaultOpts()
	opts.Prefix = "innodb_"
	_ = gsCall(t, ring, t0, opts, gsGoldenBaseline())
	out := gsCall(t, ring, t0.Add(60*time.Second), opts, gsGoldenCurrent())

	names := gsNames(t, out)
	if len(names) != 3 {
		t.Fatalf("prefix innodb_ matched %v, want the 3 Innodb_* entries", names)
	}
	for _, n := range names {
		if !strings.HasPrefix(n, "Innodb_") {
			t.Errorf("prefix filter leaked %s", n)
		}
	}
	if out["prefix"] != "innodb_" {
		t.Errorf("prefix not echoed: %v", out["prefix"])
	}
	// The census still counts the whole union — the filter is a display cut.
	if out["variablesMatched"].(int) != 3 || out["countersTracked"].(int) != 8 {
		t.Errorf("filter leaked into the census: matched=%v counters=%v",
			out["variablesMatched"], out["countersTracked"])
	}
	// A prefix nobody matches is empty, not an error.
	ring2 := newDBGSSnapshotRing(5)
	opts.Prefix = "Wsrep_"
	_ = gsCall(t, ring2, t0, opts, gsGoldenBaseline())
	out2 := gsCall(t, ring2, t0.Add(60*time.Second), opts, gsGoldenCurrent())
	if n := len(gsEntries(t, out2)); n != 0 {
		t.Errorf("prefix Wsrep_ matched %d entries, want 0", n)
	}
}

// TestDBGSGrowth_CuratedOnlyFilter — curatedOnly reuses db_global_status's own
// curatedStatusKeys, so the two tools cannot drift apart.
func TestDBGSGrowth_CuratedOnlyFilter(t *testing.T) {
	ring := newDBGSSnapshotRing(5)
	t0 := time.Unix(11000, 0)
	opts := gsDefaultOpts()
	opts.CuratedOnly = true
	_ = gsCall(t, ring, t0, opts, gsGoldenBaseline())
	out := gsCall(t, ring, t0.Add(60*time.Second), opts, gsGoldenCurrent())

	curated := dbGSCurated()
	for _, e := range gsEntries(t, out) {
		if !curated[e.Variable] {
			t.Errorf("curatedOnly leaked non-curated %s", e.Variable)
		}
	}
	byName := gsByName(t, out)
	// Qcache_hits is NOT in db_global_status's curated list, so it must be cut.
	if _, ok := byName["Qcache_hits"]; ok {
		t.Errorf("curatedOnly kept Qcache_hits")
	}
	// ...while Questions is.
	if _, ok := byName["Questions"]; !ok {
		t.Errorf("curatedOnly dropped Questions, which db_global_status curates")
	}
	if !curated["Threads_connected"] || curated["Some_plugin_counter"] {
		t.Errorf("curated set does not mirror curatedStatusKeys")
	}
}

// TestDBGSGrowth_IncludeUnchanged — a KNOWN zero delta is hidden by default and
// shown on request, but an UNKNOWN delta is always kept: unknown is not
// unchanged, and hiding it would make the first call look like an idle server.
func TestDBGSGrowth_IncludeUnchanged(t *testing.T) {
	ring := newDBGSSnapshotRing(5)
	t0 := time.Unix(12000, 0)
	opts := gsDefaultOpts()
	opts.IncludeUnchanged = true
	_ = gsCall(t, ring, t0, opts, gsGoldenBaseline())
	out := gsCall(t, ring, t0.Add(60*time.Second), opts, gsGoldenCurrent())

	byName := gsByName(t, out)
	sq, ok := byName["Slow_queries"]
	if !ok {
		t.Fatal("includeUnchanged did not surface the delta-0 Slow_queries")
	}
	wantInt64(t, "Slow_queries.delta", sq.Delta, 0)
	if out["variablesMatched"].(int) != 11 {
		t.Errorf("variablesMatched = %v, want the full 11-key union", out["variablesMatched"])
	}
	// Unknown-delta entries survive the DEFAULT filter too.
	ring2 := newDBGSSnapshotRing(5)
	out2 := gsCall(t, ring2, t0, gsDefaultOpts(), gsGoldenBaseline())
	if n := len(gsEntries(t, out2)); n != 10 {
		t.Errorf("first call returned %d entries; unknown deltas must not be filtered as unchanged", n)
	}
}

// TestDBGSGrowth_TopNSlicesAfterHonestTotals — topN is a display cut applied
// last, so the census still describes the whole union.
func TestDBGSGrowth_TopNSlicesAfterHonestTotals(t *testing.T) {
	ring := newDBGSSnapshotRing(5)
	t0 := time.Unix(13000, 0)
	opts := gsDefaultOpts()
	opts.TopN = 2
	_ = gsCall(t, ring, t0, opts, gsGoldenBaseline())
	out := gsCall(t, ring, t0.Add(60*time.Second), opts, gsGoldenCurrent())

	if n := len(gsEntries(t, out)); n != 2 {
		t.Errorf("returned %d entries, want 2", n)
	}
	if out["variablesMatched"].(int) != 10 {
		t.Errorf("variablesMatched = %v, want 10 (pre-cut)", out["variablesMatched"])
	}
	if out["variablesMoved"].(int) != 8 {
		t.Errorf("variablesMoved = %v, want 8 (pre-cut)", out["variablesMoved"])
	}
	// The two survivors are the top of the rate ranking, not an arbitrary pair.
	if got := gsNames(t, out); got[0] != "Questions" || got[1] != "Com_select" {
		t.Errorf("topN kept %v, want the two fastest counters", got)
	}
}

// TestDBGSGrowth_ExplicitSnapshotIdBaseline — pinning an older snapshot lets an
// operator measure across a longer window than the last call.
func TestDBGSGrowth_ExplicitSnapshotIdBaseline(t *testing.T) {
	ring := newDBGSSnapshotRing(5)
	t0 := time.Unix(14000, 0)
	_ = gsCall(t, ring, t0, gsDefaultOpts(), []gsKV{{"Uptime", "1000"}, {"Questions", "0"}})
	_ = gsCall(t, ring, t0.Add(60*time.Second), gsDefaultOpts(), []gsKV{{"Uptime", "1060"}, {"Questions", "600"}})

	opts := gsDefaultOpts()
	opts.WantID = "gs-1"
	out := gsCall(t, ring, t0.Add(120*time.Second), opts, []gsKV{{"Uptime", "1120"}, {"Questions", "1200"}})

	if out["comparedTo"] != "gs-1" {
		t.Fatalf("comparedTo = %v, want the pinned gs-1", out["comparedTo"])
	}
	if out["rateIntervalSeconds"].(int64) != 120 {
		t.Errorf("rateIntervalSeconds = %v, want 120 across the pinned window", out["rateIntervalSeconds"])
	}
	byName := gsByName(t, out)
	wantInt64(t, "Questions.delta", byName["Questions"].Delta, 1200)
	wantFloat(t, "Questions.perSecond", byName["Questions"].PerSecond, 10.0)

	// storedSnapshots is newest-first so the operator can pick the next id.
	stored, _ := out["storedSnapshots"].([]map[string]any)
	if len(stored) != 3 || stored[0]["id"] != "gs-3" {
		t.Errorf("storedSnapshots = %v, want 3 entries newest-first", stored)
	}
}

// TestDBGSGrowth_UnknownSnapshotIdErrors — an evicted or invented id must be an
// error, and the message has to say the snapshot was NOT recorded so the
// operator does not retry against an id that will never appear.
func TestDBGSGrowth_UnknownSnapshotIdErrors(t *testing.T) {
	ring := newDBGSSnapshotRing(5)
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.ExpectQuery(regexp.QuoteMeta("SHOW GLOBAL STATUS")).
		WillReturnRows(sqlmock.NewRows([]string{"Variable_name", "Value"}).AddRow("Uptime", "10"))

	opts := gsDefaultOpts()
	opts.WantID = "gs-999"
	_, err = collectGlobalStatusGrowth(context.Background(), db, ring, time.Unix(15000, 0), opts)
	if err == nil {
		t.Fatal("expected an error for an unknown snapshotId")
	}
	if !strings.Contains(err.Error(), "snapshot not found") || !strings.Contains(err.Error(), "seed a baseline") {
		t.Errorf("error = %v, want a not-found message that explains the recovery", err)
	}
	if len(ring.list()) != 0 {
		t.Errorf("a failed lookup must not record the snapshot: %v", ring.list())
	}
}

// TestDBGSGrowth_RingEvictsOldest — the ring is bounded, and an evicted id
// stops resolving.
func TestDBGSGrowth_RingEvictsOldest(t *testing.T) {
	ring := newDBGSSnapshotRing(2)
	t0 := time.Unix(16000, 0)
	for i := 0; i < 3; i++ {
		_ = gsCall(t, ring, t0.Add(time.Duration(i)*time.Minute), gsDefaultOpts(),
			[]gsKV{{"Uptime", "1000"}, {"Questions", "0"}})
	}
	stored := ring.list()
	if len(stored) != 2 {
		t.Fatalf("ring holds %d snapshots, want 2", len(stored))
	}
	if stored[0]["id"] != "gs-3" || stored[1]["id"] != "gs-2" {
		t.Errorf("ring = %v, want gs-3 then gs-2 (newest first)", stored)
	}
	if stored[0]["variables"].(int) != 2 {
		t.Errorf("snapshot summary variables = %v, want 2", stored[0]["variables"])
	}
}

// TestDBGSGrowth_NonNumericRowsSkipped — a real server returns sentences and
// PEM blobs among the counters. They are dropped from the diff but counted, so
// "where did Ssl_cipher_list go?" has an answer.
func TestDBGSGrowth_NonNumericRowsSkipped(t *testing.T) {
	ring := newDBGSSnapshotRing(5)
	out := gsCall(t, ring, time.Unix(17000, 0), gsDefaultOpts(), []gsKV{
		{"Uptime", "1000"},
		{"Questions", "42"},
		{"Innodb_buffer_pool_dump_status", "not started"},
		{"Ssl_cipher_list", "TLS_AES_256_GCM_SHA384:TLS_CHACHA20"},
		{"Compression", "OFF"},
		{"Last_query_cost", "0.000000"}, // a float is not an integer counter
	})
	if out["variablesScanned"].(int) != 2 {
		t.Errorf("variablesScanned = %v, want 2", out["variablesScanned"])
	}
	if out["variablesSkippedNonNumeric"].(int) != 4 {
		t.Errorf("variablesSkippedNonNumeric = %v, want 4", out["variablesSkippedNonNumeric"])
	}
	if truncated, _ := out["truncated"].(bool); truncated {
		t.Errorf("truncated = true on a 6-row reading")
	}
}

// TestDBGSGrowth_EmptyStatusIsNotAnError — an ops_ro user without the global
// privilege can get back very little; that is a thin response, not a crash.
func TestDBGSGrowth_EmptyStatusIsNotAnError(t *testing.T) {
	ring := newDBGSSnapshotRing(5)
	out := gsCall(t, ring, time.Unix(18000, 0), gsDefaultOpts(), nil)
	if out["variablesScanned"].(int) != 0 {
		t.Errorf("variablesScanned = %v, want 0", out["variablesScanned"])
	}
	if entries := gsEntries(t, out); len(entries) != 0 {
		t.Errorf("entries = %v, want none", entries)
	}
	if out["uptimeSeconds"].(int64) != 0 {
		t.Errorf("uptimeSeconds = %v, want 0", out["uptimeSeconds"])
	}
}

// TestDBGSGrowth_RingRecordIsAtomicUnderConcurrency — the MCP server handles
// concurrent requests against ONE process-wide ring, so record() has to resolve
// the baseline and append the new snapshot inside a SINGLE critical section. If
// those were two separate locks a caller could be handed the snapshot it is
// itself recording as its own baseline (every delta silently zero), or two
// callers could be issued the same sequence number. Run under -race.
func TestDBGSGrowth_RingRecordIsAtomicUnderConcurrency(t *testing.T) {
	ring := newDBGSSnapshotRing(64)
	const workers = 32

	var wg sync.WaitGroup
	var mu sync.Mutex
	ids := map[string]bool{}
	selfBaseline := 0

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			snap := &dbGSStoredSnapshot{
				TakenAt: time.Unix(int64(20000+i), 0),
				Uptime:  int64(1000 + i),
				Vars:    map[string]int64{"Questions": int64(i)},
			}
			base, err := ring.record(snap, "")
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				t.Errorf("record: %v", err)
				return
			}
			if ids[snap.ID] {
				t.Errorf("duplicate snapshot id %s", snap.ID)
			}
			ids[snap.ID] = true
			if base == snap {
				selfBaseline++
			}
		}(i)
	}
	wg.Wait()

	if len(ids) != workers {
		t.Errorf("got %d distinct ids, want %d", len(ids), workers)
	}
	if selfBaseline != 0 {
		t.Errorf("%d snapshots resolved themselves as their own baseline", selfBaseline)
	}
	if n := len(ring.list()); n != workers {
		t.Errorf("ring holds %d snapshots, want %d", n, workers)
	}
}

// TestDBGSGrowth_RegisteredInGlobalRing — the registered handler must record
// into the process-wide ring, or a second call would never find a baseline.
func TestDBGSGrowth_RegisteredInGlobalRing(t *testing.T) {
	for i := 0; i < 2; i++ {
		db, mock, err := sqlmock.New()
		if err != nil {
			t.Fatal(err)
		}
		reg := NewRegistry()
		RegisterDBGlobalStatusGrowthTool(reg, DBDeps{QueryDB: db})
		tool, _ := reg.Get("db_global_status_growth")
		mock.ExpectQuery(regexp.QuoteMeta("SHOW GLOBAL STATUS")).
			WillReturnRows(sqlmock.NewRows([]string{"Variable_name", "Value"}).
				AddRow("Uptime", "1000").AddRow("Questions", "10"))

		resp := tool.Handler(context.Background(), json.RawMessage(`{}`), "")
		got, _ := resp.(map[string]any)
		if msg, ok := got["error"]; ok {
			t.Fatalf("call %d: unexpected error: %v", i, msg)
		}
		if i == 1 && got["comparedTo"] == nil {
			t.Errorf("second call found no baseline — the handler is not using the global ring")
		}
		db.Close()
	}
}
