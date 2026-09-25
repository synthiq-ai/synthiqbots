package mcpserver

import (
	"strings"
	"testing"
	"time"

	"github.com/synthiq-ai/synthiqbots/apps/ops-api/internal/dockerlog"
)

// resolveSeverityFloor: empty defaults to the lowest rung (warn = include all),
// each level name resolves to its ladder index (case-insensitive, trimmed), and
// an unknown value is rejected rather than silently defaulted.
func TestResolveSeverityFloor(t *testing.T) {
	warnIdx := len(opsLogSeverityLadder) - 1
	cases := []struct {
		in   string
		want int
		err  bool
	}{
		{"", warnIdx, false}, // default = warn (everything)
		{"panic", 0, false},  // most severe
		{"fatal", 1, false},  //
		{"assert", 2, false}, //
		{"error", 3, false},  //
		{"warn", warnIdx, false},
		{"ERROR", 3, false},         // case-insensitive
		{"  warn ", warnIdx, false}, // trimmed
		{"info", 0, true},           // not on the ladder → rejected
		{"garbage", 0, true},        // rejected, NOT defaulted
	}
	for _, c := range cases {
		got, err := resolveSeverityFloor(c.in)
		if c.err {
			if err == nil {
				t.Errorf("resolveSeverityFloor(%q): expected error", c.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("resolveSeverityFloor(%q): unexpected err %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("resolveSeverityFloor(%q): got %d want %d", c.in, got, c.want)
		}
	}
}

// classifySeverity attributes a line to its MOST-SEVERE matching level; a line
// carrying no severity token returns -1.
func TestClassifySeverity_MostSevereWins(t *testing.T) {
	cases := []struct {
		msg  string
		want int // ladder index, -1 = unclassified
	}{
		{"PANIC: world melted", 0},
		{"FATAL: db gone", 1},
		{"ASSERT failed at Foo.cpp:42", 2},
		{"ERROR: failed to load creature", 3},
		{"WARN: slow query", 4},
		{"WARN: error while loading X", 3}, // both warn+error → error (more severe)
		{"FATAL panic unwound", 0},         // both fatal+panic → panic (most severe)
		{"INFO: server up", -1},            // no severity token
		{"plain text", -1},                 // no severity token
	}
	for _, c := range cases {
		if got := classifySeverity(c.msg, opsLogSeverityLadder); got != c.want {
			t.Errorf("classifySeverity(%q): got %d want %d", c.msg, got, c.want)
		}
	}
}

// aggregateSeverityBreakdown: golden composition path — a mixed batch tallies
// into the right level buckets, ignores unclassified lines, caps samples, and
// tracks first/last timestamps per level.
func TestAggregateSeverityBreakdown_GoldenComposition(t *testing.T) {
	since := time.Date(2026, 6, 8, 10, 0, 0, 0, time.UTC)
	lines := []dockerlog.Line{
		errLineAt("stdout", "ERROR a", since.Add(1*time.Minute)),
		errLineAt("stdout", "ERROR b", since.Add(2*time.Minute)),
		errLineAt("stdout", "ERROR c", since.Add(3*time.Minute)),
		errLineAt("stderr", "FATAL boom", since.Add(4*time.Minute)),
		errLineAt("stdout", "WARN slow", since.Add(5*time.Minute)),
		errLineAt("stdout", "WARN: error in cache", since.Add(6*time.Minute)), // → error, not warn
		errLineAt("stdout", "INFO healthy", since.Add(7*time.Minute)),         // unclassified
	}
	warnIdx := len(opsLogSeverityLadder) - 1
	stats, st := aggregateSeverityBreakdown(
		feedErrorLines(lines), opsLogSeverityLadder, warnIdx, "", 2, 1000, func() {})

	if st.scanned != 7 {
		t.Errorf("scanned: want 7 got %d", st.scanned)
	}
	if st.matchCount != 6 { // INFO line excluded
		t.Errorf("matchCount: want 6 got %d", st.matchCount)
	}
	// panic=0, fatal=1, assert=2, error=3, warn=4
	wantCounts := map[int]int{0: 0, 1: 1, 2: 0, 3: 4, 4: 1}
	for idx, w := range wantCounts {
		if stats[idx].count != w {
			t.Errorf("level idx %d (%s): want count %d got %d",
				idx, opsLogSeverityLadder[idx].name, w, stats[idx].count)
		}
	}
	// samples capped at 2 for the error level (4 hits).
	if len(stats[3].samples) != 2 {
		t.Errorf("error samples: want 2 (capped) got %d", len(stats[3].samples))
	}
	// first/last TS on the error level span the first and last error-classified line.
	if !stats[3].firstTs.Equal(since.Add(1 * time.Minute)) {
		t.Errorf("error firstTs: want 10:01 got %v", stats[3].firstTs)
	}
	if !stats[3].lastTs.Equal(since.Add(6 * time.Minute)) {
		t.Errorf("error lastTs: want 10:06 got %v", stats[3].lastTs)
	}
}

// aggregateSeverityBreakdown: the minLevel floor drops levels below it, but a
// line whose MOST-SEVERE level is in range still counts there even when it also
// carries a below-floor token ("WARN: error X" counts as error under
// minLevel=error, not dropped as warn).
func TestAggregateSeverityBreakdown_MinLevelFloorDropsBelow(t *testing.T) {
	since := time.Date(2026, 6, 8, 10, 0, 0, 0, time.UTC)
	lines := []dockerlog.Line{
		errLineAt("stdout", "ERROR x", since.Add(1*time.Minute)),
		errLineAt("stdout", "WARN plain", since.Add(2*time.Minute)),    // below floor → dropped
		errLineAt("stdout", "WARN: error y", since.Add(3*time.Minute)), // → error, kept
		errLineAt("stdout", "FATAL z", since.Add(4*time.Minute)),
	}
	errorIdx := 3
	stats, st := aggregateSeverityBreakdown(
		feedErrorLines(lines), opsLogSeverityLadder, errorIdx, "", 5, 1000, func() {})

	if st.matchCount != 3 { // plain WARN excluded
		t.Errorf("matchCount: want 3 (plain warn dropped) got %d", st.matchCount)
	}
	if stats[3].count != 2 { // both ERROR x and "WARN: error y"
		t.Errorf("error count: want 2 got %d", stats[3].count)
	}
	if stats[1].count != 1 { // FATAL
		t.Errorf("fatal count: want 1 got %d", stats[1].count)
	}
	if stats[4].count != 0 { // warn never tallied (below floor)
		t.Errorf("warn count: want 0 (below floor) got %d", stats[4].count)
	}
}

// aggregateSeverityBreakdown: stream filter narrows the input.
func TestAggregateSeverityBreakdown_StreamFilter(t *testing.T) {
	since := time.Date(2026, 6, 8, 10, 0, 0, 0, time.UTC)
	lines := []dockerlog.Line{
		errLineAt("stdout", "ERROR out", since.Add(1*time.Minute)),
		errLineAt("stderr", "ERROR err", since.Add(1*time.Minute)),
	}
	warnIdx := len(opsLogSeverityLadder) - 1
	_, st := aggregateSeverityBreakdown(
		feedErrorLines(lines), opsLogSeverityLadder, warnIdx, "stderr", 5, 1000, func() {})
	if st.matchCount != 1 {
		t.Errorf("matchCount: want 1 (stderr-only) got %d", st.matchCount)
	}
}

// aggregateSeverityBreakdown: unclassified lines never count toward matchCount.
func TestAggregateSeverityBreakdown_UnclassifiedNotCounted(t *testing.T) {
	since := time.Date(2026, 6, 8, 10, 0, 0, 0, time.UTC)
	lines := []dockerlog.Line{
		errLineAt("stdout", "INFO ok", since.Add(1*time.Minute)),
		errLineAt("stdout", "DEBUG trace", since.Add(2*time.Minute)),
		errLineAt("stdout", "connection established", since.Add(3*time.Minute)),
	}
	warnIdx := len(opsLogSeverityLadder) - 1
	stats, st := aggregateSeverityBreakdown(
		feedErrorLines(lines), opsLogSeverityLadder, warnIdx, "", 5, 1000, func() {})
	if st.matchCount != 0 {
		t.Errorf("matchCount: want 0 (all unclassified) got %d", st.matchCount)
	}
	for i := range stats {
		if stats[i].count != 0 {
			t.Errorf("level %s: want 0 got %d", opsLogSeverityLadder[i].name, stats[i].count)
		}
	}
}

// aggregateSeverityBreakdown: scan-cap bails and cancels the producer.
func TestAggregateSeverityBreakdown_ScanCapBailsAndCancels(t *testing.T) {
	since := time.Date(2026, 6, 8, 10, 0, 0, 0, time.UTC)
	lines := make([]dockerlog.Line, 50)
	for i := range lines {
		lines[i] = errLineAt("stdout", "ERROR boom", since.Add(time.Duration(i)*time.Second))
	}
	cancelCalled := false
	cancel := func() { cancelCalled = true }
	warnIdx := len(opsLogSeverityLadder) - 1
	_, st := aggregateSeverityBreakdown(
		feedErrorLines(lines), opsLogSeverityLadder, warnIdx, "", 5, 10, cancel)
	if !st.scanTruncated {
		t.Error("expected scanTruncated=true after maxScan exceeded")
	}
	if !cancelCalled {
		t.Error("expected cancel() to be invoked on scan-cap hit")
	}
	if st.scanned <= 10 {
		t.Errorf("scanned: want >10 (one over the cap) got %d", st.scanned)
	}
}

// severityLevelRows: every in-range level is emitted including zero-count ones,
// ordered most-severe first; a zero-count level has count 0, pct 0, an empty
// (non-nil) samples slice, and NO firstTs/lastTs keys.
func TestSeverityLevelRows_EmitsAllInRangeInclZeroCount(t *testing.T) {
	warnIdx := len(opsLogSeverityLadder) - 1
	stats := make([]severityLevelStat, len(opsLogSeverityLadder))
	ts := time.Date(2026, 6, 8, 10, 0, 0, 0, time.UTC)
	stats[3] = severityLevelStat{count: 3, samples: []string{"ERROR a"}, firstTs: ts, lastTs: ts.Add(time.Minute)}
	stats[4] = severityLevelStat{count: 1, samples: []string{"WARN b"}, firstTs: ts, lastTs: ts}
	matchCount := 4

	rows := severityLevelRows(stats, opsLogSeverityLadder, warnIdx, matchCount)
	if len(rows) != 5 {
		t.Fatalf("want 5 level rows (all in-range incl zero-count) got %d", len(rows))
	}
	// Ordering is most-severe first.
	if rows[0]["level"] != "panic" || rows[4]["level"] != "warn" {
		t.Errorf("ordering wrong: first=%v last=%v", rows[0]["level"], rows[4]["level"])
	}
	// panic row is zero-count: count 0, pct 0, empty non-nil samples, no TS keys.
	if rows[0]["count"].(int) != 0 {
		t.Errorf("panic count: want 0 got %v", rows[0]["count"])
	}
	if rows[0]["pct"].(float64) != 0 {
		t.Errorf("panic pct: want 0 got %v", rows[0]["pct"])
	}
	s, ok := rows[0]["samples"].([]string)
	if !ok || s == nil {
		t.Errorf("panic samples must be a non-nil slice, got %#v", rows[0]["samples"])
	}
	if len(s) != 0 {
		t.Errorf("panic samples: want empty got %v", s)
	}
	if _, present := rows[0]["firstTs"]; present {
		t.Error("zero-count level must omit firstTs")
	}
	// error row: 3/4 = 75.0 pct, TS keys present.
	if rows[3]["pct"].(float64) != 75.0 {
		t.Errorf("error pct: want 75.0 got %v", rows[3]["pct"])
	}
	if _, present := rows[3]["firstTs"]; !present {
		t.Error("error level should carry firstTs")
	}
}

// severityLevelRows: a minLevel floor truncates the ladder — minLevel=error
// emits panic..error (4 rows), no warn row.
func TestSeverityLevelRows_MinLevelTruncatesLadder(t *testing.T) {
	errorIdx := 3
	stats := make([]severityLevelStat, len(opsLogSeverityLadder))
	stats[3] = severityLevelStat{count: 2}
	rows := severityLevelRows(stats, opsLogSeverityLadder, errorIdx, 2)
	if len(rows) != 4 {
		t.Fatalf("want 4 rows (panic..error) got %d", len(rows))
	}
	for _, r := range rows {
		if r["level"] == "warn" {
			t.Error("warn row must not appear under minLevel=error")
		}
	}
}

// dominantSeverityLevel: the highest-count level wins.
func TestDominantSeverityLevel_HighestCountWins(t *testing.T) {
	warnIdx := len(opsLogSeverityLadder) - 1
	stats := make([]severityLevelStat, len(opsLogSeverityLadder))
	stats[1] = severityLevelStat{count: 7}   // fatal
	stats[3] = severityLevelStat{count: 142} // error
	stats[4] = severityLevelStat{count: 50}  // warn
	dom := dominantSeverityLevel(stats, opsLogSeverityLadder, warnIdx, 199)
	if dom == nil {
		t.Fatal("expected a dominant level")
	}
	if dom["level"] != "error" {
		t.Errorf("dominant level: want error got %v", dom["level"])
	}
	if dom["count"].(int) != 142 {
		t.Errorf("dominant count: want 142 got %v", dom["count"])
	}
	if dom["pct"].(float64) != 71.4 { // 142/199 = 71.356 → 71.4
		t.Errorf("dominant pct: want 71.4 got %v", dom["pct"])
	}
}

// dominantSeverityLevel: a count tie is broken by the MORE-SEVERE level (lower
// ladder index) — error beats warn at equal counts.
func TestDominantSeverityLevel_TieMostSevereWins(t *testing.T) {
	warnIdx := len(opsLogSeverityLadder) - 1
	stats := make([]severityLevelStat, len(opsLogSeverityLadder))
	stats[3] = severityLevelStat{count: 50} // error
	stats[4] = severityLevelStat{count: 50} // warn
	dom := dominantSeverityLevel(stats, opsLogSeverityLadder, warnIdx, 100)
	if dom == nil {
		t.Fatal("expected a dominant level")
	}
	if dom["level"] != "error" {
		t.Errorf("tie should favor the more-severe level: want error got %v", dom["level"])
	}
}

// dominantSeverityLevel: nothing classified → nil (handler omits the key).
func TestDominantSeverityLevel_EmptyReturnsNil(t *testing.T) {
	warnIdx := len(opsLogSeverityLadder) - 1
	stats := make([]severityLevelStat, len(opsLogSeverityLadder))
	if dom := dominantSeverityLevel(stats, opsLogSeverityLadder, warnIdx, 0); dom != nil {
		t.Errorf("want nil dominant for empty scan, got %v", dom)
	}
}

// pctOf rounds to one decimal place and guards a zero total.
func TestPctOf(t *testing.T) {
	cases := []struct {
		n, total int
		want     float64
	}{
		{142, 199, 71.4},
		{1, 3, 33.3},
		{2, 3, 66.7},
		{1, 1, 100.0},
		{0, 0, 0}, // div-by-zero guard
		{0, 10, 0},
		{5, 10, 50.0},
	}
	for _, c := range cases {
		if got := pctOf(c.n, c.total); got != c.want {
			t.Errorf("pctOf(%d,%d): got %v want %v", c.n, c.total, got, c.want)
		}
	}
}

// Description-keyword regression: agent tool-selection latches on these to
// distinguish the breakdown (WHICH severity / composition) from
// ops_log_errors_top (WHAT pattern) and ops_log_errors_histogram (WHEN).
func TestOpsLogSeverityBreakdownTool_DescriptionMentionsKeyDistinctions(t *testing.T) {
	reg := NewRegistry()
	RegisterOpsLogSeverityBreakdownTool(reg, ContainerDeps{DefaultContainer: "ac-worldserver"})
	tool, ok := reg.Get("ops_log_severity_breakdown")
	if !ok {
		t.Fatal("ops_log_severity_breakdown not registered")
	}
	for _, kw := range []string{
		"ops_log_errors_top",
		"ops_log_errors_histogram",
		"severity",
		"dominantLevel",
		"minLevel",
		"panic",
		"fatal",
		"warn",
		"ac-worldserver",
		"Read-only",
	} {
		if !strings.Contains(tool.Description, kw) {
			t.Errorf("description missing %q: %s", kw, tool.Description)
		}
	}
}

func TestOpsLogSeverityBreakdownTool_IsReadOnly(t *testing.T) {
	reg := NewRegistry()
	RegisterOpsLogSeverityBreakdownTool(reg, ContainerDeps{DefaultContainer: "ac-worldserver"})
	tool, ok := reg.Get("ops_log_severity_breakdown")
	if !ok {
		t.Fatal("ops_log_severity_breakdown not registered")
	}
	if tool.Destructive {
		t.Error("Destructive: got true want false (read-only tool)")
	}
	if !strings.Contains(string(tool.Annotations), `"readOnlyHint":true`) {
		t.Errorf("annotations missing readOnlyHint:true: %s", tool.Annotations)
	}
}

func TestOpsLogSeverityBreakdownTool_DefaultsContainerWhenDepsEmpty(t *testing.T) {
	reg := NewRegistry()
	RegisterOpsLogSeverityBreakdownTool(reg, ContainerDeps{})
	if _, ok := reg.Get("ops_log_severity_breakdown"); !ok {
		t.Fatal("ops_log_severity_breakdown not registered when DefaultContainer empty")
	}
}
