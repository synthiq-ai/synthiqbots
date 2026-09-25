package mcpserver

import (
	"strings"
	"testing"
	"time"

	"github.com/synthiq-ai/synthiqbots/apps/ops-api/internal/dockerlog"
)

// resolveHistogramBucket — duration formats, the 1m floor, the "Nd" extension,
// default substitution, and the reject-don't-default contract for garbage.
func TestResolveHistogramBucket(t *testing.T) {
	cases := []struct {
		in   string
		want int64
		err  bool
	}{
		{"", 300, false},    // default 5m
		{"5m", 300, false},  // explicit 5m
		{"1h", 3600, false}, // hours
		{"30s", 60, false},  // sub-minute floored to 1m
		{"1m", 60, false},   // exactly the floor
		{"1d", 86400, false},
		{"0s", 0, true},           // non-positive
		{"-5m", 0, true},          // negative duration string
		{"not-a-bucket", 0, true}, // garbage rejected, NOT defaulted
	}
	for _, c := range cases {
		got, err := resolveHistogramBucket(c.in)
		if c.err {
			if err == nil {
				t.Errorf("resolveHistogramBucket(%q): expected error", c.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("resolveHistogramBucket(%q): unexpected err %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("resolveHistogramBucket(%q): got %d want %d", c.in, got, c.want)
		}
	}
}

// planHistogramBuckets: the common path — a 1h window with a 5m bucket yields 12
// buckets at the requested width (no coarsening).
func TestPlanHistogramBuckets_CommonPathNoCoarsening(t *testing.T) {
	since := int64(0)
	until := int64(3600) // 1h
	eff, req, n, err := planHistogramBuckets(since, until, "5m")
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if eff != 300 || req != 300 {
		t.Errorf("widths: eff=%d req=%d want both 300", eff, req)
	}
	if n != 12 {
		t.Errorf("numBuckets: want 12 got %d", n)
	}
}

// planHistogramBuckets: a non-divisible window rounds the bucket count UP so the
// tail interval is still covered (90m / 60m = 2 buckets, not 1).
func TestPlanHistogramBuckets_CeilOnNonDivisibleWindow(t *testing.T) {
	eff, _, n, err := planHistogramBuckets(0, 5400, "1h") // 90 min window
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if eff != 3600 {
		t.Errorf("eff width: want 3600 got %d", eff)
	}
	if n != 2 {
		t.Errorf("numBuckets: want 2 (ceil 90m/60m) got %d", n)
	}
}

// planHistogramBuckets: a bucket wider than the window collapses to one
// full-window bucket and flags the width as adjusted (eff != req).
func TestPlanHistogramBuckets_BucketWiderThanWindowCollapses(t *testing.T) {
	eff, req, n, err := planHistogramBuckets(0, 600, "1h") // 10m window, 1h bucket
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if n != 1 {
		t.Errorf("numBuckets: want 1 got %d", n)
	}
	if eff != 600 {
		t.Errorf("eff width: want 600 (clamped to window) got %d", eff)
	}
	if eff == req {
		t.Errorf("expected eff(%d) != req(%d) so bucketWidthAdjusted fires", eff, req)
	}
}

// planHistogramBuckets: a fine bucket over a long window is COARSENED up to keep
// the bucket count <= the cap, and the coarsening is visible (eff != req).
func TestPlanHistogramBuckets_CoarsensToMaxBuckets(t *testing.T) {
	// 7d window in 1m buckets would be 10080 buckets — must coarsen.
	window := int64(7 * 24 * 3600)
	eff, req, n, err := planHistogramBuckets(0, window, "1m")
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if n > opsLogErrorsHistogramMaxBuckets {
		t.Errorf("numBuckets %d exceeds cap %d", n, opsLogErrorsHistogramMaxBuckets)
	}
	if eff <= req {
		t.Errorf("expected coarsened eff(%d) > req(%d)", eff, req)
	}
	// Effective width must actually be wide enough that n stays under the cap.
	if int64(n)*eff < window {
		t.Errorf("buckets don't cover the window: n=%d eff=%d window=%d", n, eff, window)
	}
}

// planHistogramBuckets: degenerate window (since == until) → one bucket, no
// division fault.
func TestPlanHistogramBuckets_DegenerateWindow(t *testing.T) {
	eff, _, n, err := planHistogramBuckets(1000, 1000, "5m")
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if n != 1 {
		t.Errorf("numBuckets: want 1 got %d", n)
	}
	if eff != 300 {
		t.Errorf("eff width: want 300 got %d", eff)
	}
}

// planHistogramBuckets propagates the bucket parse error.
func TestPlanHistogramBuckets_BadBucketErrors(t *testing.T) {
	if _, _, _, err := planHistogramBuckets(0, 3600, "garbage"); err == nil {
		t.Error("expected error for unparseable bucket")
	}
}

// aggregateErrorHistogram: matching lines land in the correct time bucket; a
// non-matching line is ignored; counts are per-bucket.
func TestAggregateErrorHistogram_BucketsByTime(t *testing.T) {
	matcher, err := buildErrorsMatcher(opsLogErrorsTopDefaultPattern, false)
	if err != nil {
		t.Fatalf("matcher: %v", err)
	}
	since := time.Date(2026, 6, 7, 10, 0, 0, 0, time.UTC)
	width := int64(600) // 10m buckets
	// bucket 0: [10:00,10:10), bucket 1: [10:10,10:20), bucket 2: [10:20,10:30)
	lines := []dockerlog.Line{
		errLineAt("stdout", "ERROR a", since.Add(1*time.Minute)),  // bucket 0
		errLineAt("stdout", "ERROR b", since.Add(2*time.Minute)),  // bucket 0
		errLineAt("stdout", "INFO ok", since.Add(3*time.Minute)),  // no match
		errLineAt("stdout", "FATAL c", since.Add(12*time.Minute)), // bucket 1
		errLineAt("stdout", "ERROR d", since.Add(25*time.Minute)), // bucket 2
	}
	counts, undated, stats := aggregateErrorHistogram(
		feedErrorLines(lines), matcher, "", since.Unix(), width, 3, 1000, func() {})
	if stats.scanned != 5 {
		t.Errorf("scanned: want 5 got %d", stats.scanned)
	}
	if stats.matchCount != 4 {
		t.Errorf("matchCount: want 4 got %d", stats.matchCount)
	}
	if undated != 0 {
		t.Errorf("undated: want 0 got %d", undated)
	}
	want := []int{2, 1, 1}
	for i, w := range want {
		if counts[i] != w {
			t.Errorf("bucket %d: want %d got %d (counts=%v)", i, w, counts[i], counts)
		}
	}
}

// aggregateErrorHistogram: a matched line with NO timestamp is counted in
// `undated`, NOT placed into a fabricated bucket.
func TestAggregateErrorHistogram_UndatedNotBucketed(t *testing.T) {
	matcher, _ := buildErrorsMatcher(opsLogErrorsTopDefaultPattern, false)
	since := time.Date(2026, 6, 7, 10, 0, 0, 0, time.UTC)
	lines := []dockerlog.Line{
		errLineAt("stdout", "ERROR dated", since.Add(1*time.Minute)),
		errLine("stdout", "ERROR undated"), // zero TS
	}
	counts, undated, stats := aggregateErrorHistogram(
		feedErrorLines(lines), matcher, "", since.Unix(), 600, 2, 1000, func() {})
	if stats.matchCount != 2 {
		t.Errorf("matchCount: want 2 got %d", stats.matchCount)
	}
	if undated != 1 {
		t.Errorf("undated: want 1 got %d", undated)
	}
	total := 0
	for _, c := range counts {
		total += c
	}
	if total != 1 {
		t.Errorf("placed total: want 1 (only the dated line) got %d", total)
	}
}

// aggregateErrorHistogram: timestamps outside [since, until] clamp into the edge
// buckets rather than indexing out of range (daemon Since filter is best-effort).
func TestAggregateErrorHistogram_ClampsOutOfRangeTimestamps(t *testing.T) {
	matcher, _ := buildErrorsMatcher(opsLogErrorsTopDefaultPattern, false)
	since := time.Date(2026, 6, 7, 10, 0, 0, 0, time.UTC)
	width := int64(600)
	n := 3 // window [10:00, 10:30)
	lines := []dockerlog.Line{
		errLineAt("stdout", "ERROR early", since.Add(-5*time.Minute)), // before since → bucket 0
		errLineAt("stdout", "ERROR late", since.Add(120*time.Minute)), // way past → last bucket
	}
	counts, _, _ := aggregateErrorHistogram(
		feedErrorLines(lines), matcher, "", since.Unix(), width, n, 1000, func() {})
	if counts[0] != 1 {
		t.Errorf("early line should clamp to bucket 0: counts=%v", counts)
	}
	if counts[n-1] != 1 {
		t.Errorf("late line should clamp to last bucket: counts=%v", counts)
	}
}

// aggregateErrorHistogram: stream filter narrows the input.
func TestAggregateErrorHistogram_StreamFilter(t *testing.T) {
	matcher, _ := buildErrorsMatcher(opsLogErrorsTopDefaultPattern, false)
	since := time.Date(2026, 6, 7, 10, 0, 0, 0, time.UTC)
	lines := []dockerlog.Line{
		errLineAt("stdout", "ERROR out", since.Add(1*time.Minute)),
		errLineAt("stderr", "ERROR err", since.Add(1*time.Minute)),
	}
	_, _, stats := aggregateErrorHistogram(
		feedErrorLines(lines), matcher, "stderr", since.Unix(), 600, 1, 1000, func() {})
	if stats.matchCount != 1 {
		t.Errorf("matchCount: want 1 (stderr-only) got %d", stats.matchCount)
	}
}

// aggregateErrorHistogram: scan-cap bails and cancels the producer.
func TestAggregateErrorHistogram_ScanCapBailsAndCancels(t *testing.T) {
	matcher, _ := buildErrorsMatcher(opsLogErrorsTopDefaultPattern, false)
	since := time.Date(2026, 6, 7, 10, 0, 0, 0, time.UTC)
	lines := make([]dockerlog.Line, 50)
	for i := range lines {
		lines[i] = errLineAt("stdout", "ERROR boom", since.Add(time.Duration(i)*time.Second))
	}
	cancelCalled := false
	cancel := func() { cancelCalled = true }
	_, _, stats := aggregateErrorHistogram(
		feedErrorLines(lines), matcher, "", since.Unix(), 600, 1, 10, cancel)
	if !stats.scanTruncated {
		t.Error("expected scanTruncated=true after maxScan exceeded")
	}
	if !cancelCalled {
		t.Error("expected cancel() to be invoked on scan-cap hit")
	}
	if stats.scanned <= 10 {
		t.Errorf("scanned: want >10 (one over the cap) got %d", stats.scanned)
	}
}

// histogramBuckets: every bucket is emitted (including zero-count ones) so the
// curve is contiguous, and the final bucket's endTs is clamped to `until`.
func TestHistogramBuckets_EmitsAllBucketsAndClampsTail(t *testing.T) {
	since := time.Date(2026, 6, 7, 10, 0, 0, 0, time.UTC).Unix()
	width := int64(600)
	until := since + 1500 // 25m — last (3rd) bucket only half-full
	counts := []int{2, 0, 5}
	out := histogramBuckets(counts, since, width, until, 3)
	if len(out) != 3 {
		t.Fatalf("want 3 buckets (incl zero-count) got %d", len(out))
	}
	if out[1]["count"].(int) != 0 {
		t.Errorf("zero-count bucket must still be emitted: %v", out[1])
	}
	// First bucket: 10:00 → 10:10.
	if out[0]["startTs"] != "2026-06-07T10:00:00Z" || out[0]["endTs"] != "2026-06-07T10:10:00Z" {
		t.Errorf("bucket 0 boundaries wrong: %v", out[0])
	}
	// Last bucket end clamps to until (10:25), not 10:30.
	if out[2]["endTs"] != "2026-06-07T10:25:00Z" {
		t.Errorf("last bucket endTs should clamp to until 10:25, got %v", out[2]["endTs"])
	}
}

// histogramBuckets never returns nil (callers iterate without a nil check).
func TestHistogramBuckets_NonNil(t *testing.T) {
	out := histogramBuckets([]int{0}, 0, 600, 600, 1)
	if out == nil {
		t.Error("want non-nil slice")
	}
}

// peakHistogramBucket: highest-count bucket wins, earliest index on ties.
func TestPeakHistogramBucket_HottestWithEarliestTiebreak(t *testing.T) {
	since := time.Date(2026, 6, 7, 10, 0, 0, 0, time.UTC).Unix()
	width := int64(600)
	until := since + 1800
	// Buckets 1 and 3 tie at 5; earliest (index 1) must win.
	counts := []int{2, 5, 1, 5}
	peak := peakHistogramBucket(counts, since, width, until)
	if peak == nil {
		t.Fatal("expected a peak bucket")
	}
	if peak["index"].(int) != 1 {
		t.Errorf("peak index: want 1 (earliest of tied max) got %v", peak["index"])
	}
	if peak["count"].(int) != 5 {
		t.Errorf("peak count: want 5 got %v", peak["count"])
	}
	if peak["startTs"] != "2026-06-07T10:10:00Z" {
		t.Errorf("peak startTs: want 10:10 got %v", peak["startTs"])
	}
}

// peakHistogramBucket: all-zero counts → nil (no placed lines), so the handler
// omits the key rather than reporting a misleading count-0 peak.
func TestPeakHistogramBucket_AllZeroReturnsNil(t *testing.T) {
	if peak := peakHistogramBucket([]int{0, 0, 0}, 0, 600, 1800); peak != nil {
		t.Errorf("want nil peak for all-zero counts, got %v", peak)
	}
}

// Description-keyword regression: agent tool-selection latches on these to
// distinguish the histogram (WHEN) from ops_log_errors_top (WHAT) and the grep
// family. Their presence is load-bearing.
func TestOpsLogErrorsHistogramTool_DescriptionMentionsKeyDistinctions(t *testing.T) {
	reg := NewRegistry()
	RegisterOpsLogErrorsHistogramTool(reg, ContainerDeps{DefaultContainer: "ac-worldserver"})
	tool, ok := reg.Get("ops_log_errors_histogram")
	if !ok {
		t.Fatal("ops_log_errors_histogram not registered")
	}
	for _, kw := range []string{
		"ops_log_errors_top",
		"container_logs_grep",
		"histogram",
		"peakBucket",
		"ac-worldserver",
		"bucket",
		"undated",
		"bucketWidthAdjusted",
		"Read-only",
	} {
		if !strings.Contains(tool.Description, kw) {
			t.Errorf("description missing %q: %s", kw, tool.Description)
		}
	}
}

func TestOpsLogErrorsHistogramTool_IsReadOnly(t *testing.T) {
	reg := NewRegistry()
	RegisterOpsLogErrorsHistogramTool(reg, ContainerDeps{DefaultContainer: "ac-worldserver"})
	tool, ok := reg.Get("ops_log_errors_histogram")
	if !ok {
		t.Fatal("ops_log_errors_histogram not registered")
	}
	if tool.Destructive {
		t.Error("Destructive: got true want false (read-only tool)")
	}
	if !strings.Contains(string(tool.Annotations), `"readOnlyHint":true`) {
		t.Errorf("annotations missing readOnlyHint:true: %s", tool.Annotations)
	}
}

func TestOpsLogErrorsHistogramTool_DefaultsContainerWhenDepsEmpty(t *testing.T) {
	reg := NewRegistry()
	RegisterOpsLogErrorsHistogramTool(reg, ContainerDeps{})
	if _, ok := reg.Get("ops_log_errors_histogram"); !ok {
		t.Fatal("ops_log_errors_histogram not registered when DefaultContainer empty")
	}
}
