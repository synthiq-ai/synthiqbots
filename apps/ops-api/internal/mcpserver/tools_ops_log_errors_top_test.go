package mcpserver

import (
	"strings"
	"testing"
	"time"

	"github.com/synthiq-ai/synthiqbots/apps/ops-api/internal/dockerlog"
)

// feedErrorLines mirrors the sibling tools' `feedLines` helper. We rename to
// avoid colliding with feedLines defined in tools_container_logs_grep_context_test.go
// (same package — single shared declaration would force ordering coupling
// between unrelated test files).
func feedErrorLines(lines []dockerlog.Line) <-chan dockerlog.Line {
	ch := make(chan dockerlog.Line, len(lines)+1)
	for _, l := range lines {
		ch <- l
	}
	close(ch)
	return ch
}

func errLine(stream, msg string) dockerlog.Line {
	return dockerlog.Line{Stream: stream, Msg: msg}
}

func errLineAt(stream, msg string, ts time.Time) dockerlog.Line {
	return dockerlog.Line{Stream: stream, Msg: msg, TS: ts}
}

// Normalizer baseline: two lines differing only in digit values must collapse
// to the same bucket. This is the load-bearing property of the tool — without
// it, every "failed to load 1234" / "failed to load 5678" is its own row and
// the top-N is dominated by counter-only variation.
func TestNormalizeLogLine_CollapsesDigitRuns(t *testing.T) {
	a := normalizeLogLine("ERROR: failed to load 1234 from creatures")
	b := normalizeLogLine("ERROR: failed to load 5678 from creatures")
	if a != b {
		t.Errorf("digit collapse failed: a=%q b=%q", a, b)
	}
	if !strings.Contains(a, "#") {
		t.Errorf("expected `#` placeholder in normalized form, got %q", a)
	}
}

func TestNormalizeLogLine_StripsTimestamps(t *testing.T) {
	in := "2026-05-29 12:34:56 ERROR: boom"
	out := normalizeLogLine(in)
	if !strings.Contains(out, "<ts>") {
		t.Errorf("expected <ts> placeholder, got %q", out)
	}
	if strings.Contains(out, "2026") || strings.Contains(out, "12:34:56") {
		t.Errorf("raw timestamp leaked into normalized form: %q", out)
	}
}

func TestNormalizeLogLine_ReplacesIPv4WithPort(t *testing.T) {
	out := normalizeLogLine("Client connect from 192.168.100.18:54321")
	if !strings.Contains(out, "<ip>") {
		t.Errorf("expected <ip> placeholder, got %q", out)
	}
	if strings.Contains(out, "192") || strings.Contains(out, "54321") {
		t.Errorf("raw IP/port leaked: %q", out)
	}
}

func TestNormalizeLogLine_ReplacesUUID(t *testing.T) {
	out := normalizeLogLine("session=550e8400-e29b-41d4-a716-446655440000 closed")
	if !strings.Contains(out, "<uuid>") {
		t.Errorf("expected <uuid> placeholder, got %q", out)
	}
}

func TestNormalizeLogLine_ReplacesHexLiteralAndBareHex(t *testing.T) {
	out := normalizeLogLine("frame=0x7f8c12345678 ret=0xdeadbeef ptr=a1b2c3d4e5f6")
	hexHits := strings.Count(out, "<hex>")
	if hexHits != 3 {
		t.Errorf("want 3 <hex> placeholders, got %d in %q", hexHits, out)
	}
}

// Two lines whose only difference is whitespace+timestamp must collapse, but
// lines with semantically different prose must NOT collapse — guards against
// an over-eager normalizer that strips alpha tokens.
func TestNormalizeLogLine_PreservesSemanticTokens(t *testing.T) {
	a := normalizeLogLine("ERROR LoadPlayerFromDB failed for guid 12345")
	b := normalizeLogLine("ERROR LoadPlayerFromDB failed for guid 99999")
	c := normalizeLogLine("ERROR LoadCreatureFromDB failed for guid 12345")
	if a != b {
		t.Errorf("equivalent semantic forms must collapse: a=%q b=%q", a, b)
	}
	if a == c {
		t.Errorf("distinct semantic forms must NOT collapse: a=%q c=%q", a, c)
	}
}

// Aggregator: two semantically-identical lines must produce one bucket with
// count=2 and both samples (sampleLimit=2). The matcher and stream filter
// are at default — both lines match `error`.
func TestAggregateErrorPatterns_BucketsAndSamples(t *testing.T) {
	matcher, err := buildErrorsMatcher(opsLogErrorsTopDefaultPattern, false)
	if err != nil {
		t.Fatalf("matcher: %v", err)
	}
	lines := []dockerlog.Line{
		errLine("stdout", "ERROR LoadPlayerFromDB failed for guid 1"),
		errLine("stdout", "ERROR LoadPlayerFromDB failed for guid 2"),
		errLine("stdout", "INFO heartbeat"),
	}
	patterns, stats := aggregateErrorPatterns(feedErrorLines(lines), matcher, "", 2, 1000, func() {})
	if stats.scanned != 3 {
		t.Errorf("scanned: want 3 got %d", stats.scanned)
	}
	if stats.matchCount != 2 {
		t.Errorf("matchCount: want 2 got %d", stats.matchCount)
	}
	if len(patterns) != 1 {
		t.Fatalf("buckets: want 1 (collapsed by digit-run) got %d", len(patterns))
	}
	for _, p := range patterns {
		if p.count != 2 {
			t.Errorf("count: want 2 got %d", p.count)
		}
		if len(p.samples) != 2 {
			t.Errorf("samples: want 2 got %d (sampleLimit=2)", len(p.samples))
		}
	}
}

// Sample cap: third hit must NOT extend the samples slice. This guards the
// O(matchCount) → O(sampleLimit) cap that keeps the response envelope bounded
// when one error pattern dominates a 200000-line scan.
func TestAggregateErrorPatterns_SampleLimitCaps(t *testing.T) {
	matcher, _ := buildErrorsMatcher(opsLogErrorsTopDefaultPattern, false)
	lines := []dockerlog.Line{
		errLine("stdout", "ERROR failed for 1"),
		errLine("stdout", "ERROR failed for 2"),
		errLine("stdout", "ERROR failed for 3"),
		errLine("stdout", "ERROR failed for 4"),
	}
	patterns, _ := aggregateErrorPatterns(feedErrorLines(lines), matcher, "", 2, 1000, func() {})
	if len(patterns) != 1 {
		t.Fatalf("buckets: want 1 got %d", len(patterns))
	}
	for _, p := range patterns {
		if p.count != 4 {
			t.Errorf("count: want 4 got %d", p.count)
		}
		if len(p.samples) != 2 {
			t.Errorf("samples cap broken: want 2 got %d", len(p.samples))
		}
	}
}

// Stream filter narrows the input — stderr-only filter must drop stdout hits.
func TestAggregateErrorPatterns_StreamFilterNarrowsInput(t *testing.T) {
	matcher, _ := buildErrorsMatcher(opsLogErrorsTopDefaultPattern, false)
	lines := []dockerlog.Line{
		errLine("stdout", "ERROR stdout-side"),
		errLine("stderr", "ERROR stderr-side"),
	}
	patterns, stats := aggregateErrorPatterns(feedErrorLines(lines), matcher, "stderr", 3, 1000, func() {})
	if stats.matchCount != 1 {
		t.Errorf("matchCount: want 1 (stderr-only) got %d", stats.matchCount)
	}
	if len(patterns) != 1 {
		t.Fatalf("buckets: want 1 got %d", len(patterns))
	}
	for _, p := range patterns {
		if len(p.samples) == 0 || !strings.Contains(p.samples[0], "stderr-side") {
			t.Errorf("sample should be stderr-side: %v", p.samples)
		}
	}
}

// Scan-cap: aggregator must bail at maxScan and flag scanTruncated, without
// emitting more buckets than the cap allowed.
func TestAggregateErrorPatterns_ScanCapBails(t *testing.T) {
	matcher, _ := buildErrorsMatcher(opsLogErrorsTopDefaultPattern, false)
	lines := make([]dockerlog.Line, 50)
	for i := range lines {
		lines[i] = errLine("stdout", "ERROR boom")
	}
	cancelCalled := false
	cancel := func() { cancelCalled = true }
	patterns, stats := aggregateErrorPatterns(feedErrorLines(lines), matcher, "", 3, 10, cancel)
	if !stats.scanTruncated {
		t.Error("expected scanTruncated=true after maxScan exceeded")
	}
	if !cancelCalled {
		t.Error("expected cancel() to be invoked on scan-cap hit")
	}
	if stats.scanned <= 10 {
		t.Errorf("scanned: want >10 (one over the cap) got %d", stats.scanned)
	}
	if len(patterns) == 0 {
		t.Error("expected at least one bucket from the lines processed before cap")
	}
}

// First/last TS tracking: spread of timestamps across matching lines is
// captured per-bucket. Validates the IsZero guard for lines without TS.
func TestAggregateErrorPatterns_FirstLastTimestamps(t *testing.T) {
	matcher, _ := buildErrorsMatcher(opsLogErrorsTopDefaultPattern, false)
	t1 := time.Date(2026, 5, 29, 10, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 5, 29, 11, 0, 0, 0, time.UTC)
	t3 := time.Date(2026, 5, 29, 12, 0, 0, 0, time.UTC)
	lines := []dockerlog.Line{
		errLineAt("stdout", "ERROR boom 1", t2),
		errLineAt("stdout", "ERROR boom 2", t1), // earliest
		errLineAt("stdout", "ERROR boom 3", t3), // latest
		errLine("stdout", "ERROR boom 4"),       // zero TS — must NOT clobber tracked first/last
	}
	patterns, _ := aggregateErrorPatterns(feedErrorLines(lines), matcher, "", 3, 1000, func() {})
	if len(patterns) != 1 {
		t.Fatalf("buckets: want 1 got %d", len(patterns))
	}
	for _, p := range patterns {
		if !p.firstTs.Equal(t1) {
			t.Errorf("firstTs: want %v got %v", t1, p.firstTs)
		}
		if !p.lastTs.Equal(t3) {
			t.Errorf("lastTs: want %v got %v", t3, p.lastTs)
		}
	}
}

// topErrorPatterns sort + truncate: counts desc with normalized-asc tiebreaker.
// Without the deterministic tiebreaker, map iteration randomization would make
// tied-count rows appear in unstable order across runs.
func TestTopErrorPatterns_SortDescWithTiebreaker(t *testing.T) {
	patterns := map[string]*errorPatternStat{
		"a-pattern": {normalized: "a-pattern", count: 3},
		"b-pattern": {normalized: "b-pattern", count: 5},
		"c-pattern": {normalized: "c-pattern", count: 5}, // tied with b
		"d-pattern": {normalized: "d-pattern", count: 1},
	}
	got := topErrorPatterns(patterns, 10)
	if len(got) != 4 {
		t.Fatalf("rows: want 4 got %d", len(got))
	}
	wantOrder := []string{"b-pattern", "c-pattern", "a-pattern", "d-pattern"}
	for i, want := range wantOrder {
		if got[i]["normalized"] != want {
			t.Errorf("row %d: want %q got %v", i, want, got[i]["normalized"])
		}
	}
}

// topErrorPatterns must clip to topN.
func TestTopErrorPatterns_TruncatesToTopN(t *testing.T) {
	patterns := map[string]*errorPatternStat{
		"x1": {normalized: "x1", count: 10},
		"x2": {normalized: "x2", count: 9},
		"x3": {normalized: "x3", count: 8},
	}
	got := topErrorPatterns(patterns, 2)
	if len(got) != 2 {
		t.Errorf("topN truncation: want 2 got %d", len(got))
	}
	if got[0]["normalized"] != "x1" || got[1]["normalized"] != "x2" {
		t.Errorf("wrong top-2: %v %v", got[0]["normalized"], got[1]["normalized"])
	}
}

// Empty input → empty (not nil) slice. Nil would force callers to nil-check
// before iterating; an empty slice is safer for JSON consumers downstream.
func TestTopErrorPatterns_EmptyInputReturnsEmptySlice(t *testing.T) {
	got := topErrorPatterns(map[string]*errorPatternStat{}, 10)
	if got == nil {
		t.Error("want non-nil empty slice, got nil")
	}
	if len(got) != 0 {
		t.Errorf("want length 0, got %d", len(got))
	}
}

// firstTs/lastTs presence: a row with a tracked timestamp must emit the keys;
// a row with zero timestamps must omit them entirely (downstream checks key
// presence, not zero-value).
func TestTopErrorPatterns_EmitsTimestampsWhenTracked(t *testing.T) {
	ts := time.Date(2026, 5, 29, 10, 0, 0, 0, time.UTC)
	patterns := map[string]*errorPatternStat{
		"with-ts": {normalized: "with-ts", count: 1, firstTs: ts, lastTs: ts},
		"no-ts":   {normalized: "no-ts", count: 1},
	}
	got := topErrorPatterns(patterns, 10)
	var withTs, noTs map[string]any
	for _, r := range got {
		if r["normalized"] == "with-ts" {
			withTs = r
		}
		if r["normalized"] == "no-ts" {
			noTs = r
		}
	}
	if withTs == nil || noTs == nil {
		t.Fatalf("missing expected rows: with=%v no=%v", withTs, noTs)
	}
	if _, ok := withTs["firstTs"]; !ok {
		t.Error("with-ts row missing firstTs")
	}
	if _, ok := noTs["firstTs"]; ok {
		t.Error("no-ts row must omit firstTs entirely")
	}
}

// buildErrorsMatcher: default pattern matches the four severity tokens.
func TestBuildErrorsMatcher_DefaultPatternMatchesSeverityTokens(t *testing.T) {
	m, err := buildErrorsMatcher(opsLogErrorsTopDefaultPattern, false)
	if err != nil {
		t.Fatalf("matcher: %v", err)
	}
	cases := []struct {
		in   string
		want bool
	}{
		{"ERROR boom", true},
		{"error boom", true},
		{"FATAL boom", true},
		{"panic: nil deref", true},
		{"Assertion failed", true},
		{"INFO heartbeat", false},
		{"DEBUG trace", false},
	}
	for _, c := range cases {
		if got := m(c.in); got != c.want {
			t.Errorf("matcher(%q): want %v got %v", c.in, c.want, got)
		}
	}
}

// CaseInsensitive flag adds (?i) only if not already present — avoids the
// "(?i)(?i)..." double-prefix when the default pattern is in play.
func TestBuildErrorsMatcher_CaseInsensitiveIsAdditive(t *testing.T) {
	m, err := buildErrorsMatcher("WARN", true)
	if err != nil {
		t.Fatalf("matcher: %v", err)
	}
	if !m("warn") || !m("WARN") {
		t.Error("caseInsensitive=true must match both cases")
	}
}

// BadRegex must error rather than silently falling back to substring.
func TestBuildErrorsMatcher_BadRegex(t *testing.T) {
	if _, err := buildErrorsMatcher("[unclosed", false); err == nil {
		t.Error("expected error for malformed regex")
	}
}

// resolveErrorsSince — duration formats, 7d clamp, unix passthrough.
func TestResolveErrorsSince_DurationAndClamp(t *testing.T) {
	now := time.Now().Unix()
	got, err := resolveErrorsSince("")
	if err != nil {
		t.Fatalf("default: %v", err)
	}
	if got < now-3601 || got > now-3599 {
		t.Errorf("default 1h: got %d want ~%d", got, now-3600)
	}
	got, err = resolveErrorsSince("30d")
	if err != nil {
		t.Fatalf("30d: %v", err)
	}
	want := now - int64(opsLogErrorsTopMaxWindow.Seconds())
	if got < want-2 || got > want+2 {
		t.Errorf("30d clamp: got %d want ~%d", got, want)
	}
}

func TestResolveErrorsSince_RejectsFutureAndGarbage(t *testing.T) {
	future := time.Now().Add(1 * time.Hour).Unix()
	if _, err := resolveErrorsSince(strconvFormatInt(future)); err == nil {
		t.Error("expected error for future unix timestamp")
	}
	if _, err := resolveErrorsSince("not-a-thing"); err == nil {
		t.Error("expected error for garbage input")
	}
}

// strconvFormatInt is a small inline helper to avoid a strconv import here
// (the other helpers in this file don't need it). Inlining keeps the test
// file self-contained without colliding with formatInt64 used elsewhere in
// the package.
func strconvFormatInt(v int64) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var b [20]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

func TestNormalizeErrorsStreamFilter(t *testing.T) {
	cases := []struct {
		in   string
		want string
		err  bool
	}{
		{"", "", false},
		{"both", "", false},
		{"STDOUT", "stdout", false},
		{"stderr", "stderr", false},
		{"junk", "", true},
	}
	for _, c := range cases {
		got, err := normalizeErrorsStreamFilter(c.in)
		if c.err {
			if err == nil {
				t.Errorf("(%q): expected err", c.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("(%q): %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("(%q): got %q want %q", c.in, got, c.want)
		}
	}
}

func TestClampErrorsInt(t *testing.T) {
	cases := []struct {
		v, def, lo, hi, want int
	}{
		{0, 10, 1, 50, 10},
		{-5, 10, 1, 50, 10},
		{25, 10, 1, 50, 25},
		{100, 10, 1, 50, 50},
		{1, 10, 5, 50, 5},
	}
	for _, c := range cases {
		got := clampErrorsInt(c.v, c.def, c.lo, c.hi)
		if got != c.want {
			t.Errorf("clampErrorsInt(%d,%d,%d,%d): got %d want %d", c.v, c.def, c.lo, c.hi, got, c.want)
		}
	}
}

// clipBytes: ASCII below cap = passthrough; ASCII over cap = truncated with ~.
func TestClipBytes_ASCIIPassthroughAndTruncate(t *testing.T) {
	if got := clipBytes("short", 10); got != "short" {
		t.Errorf("passthrough: got %q want %q", got, "short")
	}
	got := clipBytes("0123456789ABCD", 10)
	if !strings.HasSuffix(got, "~") {
		t.Errorf("truncated form must end with ~ marker: %q", got)
	}
	if len(got) != 11 {
		t.Errorf("truncated len: want 11 got %d", len(got))
	}
}

// clipBytes UTF-8 safety: cut must back up to a valid rune boundary so we
// never emit a half-decoded sequence. Without the back-up, slicing mid-rune
// would produce invalid UTF-8 that downstream JSON encoders would replace
// with U+FFFD or refuse.
func TestClipBytes_BacksUpToRuneBoundary(t *testing.T) {
	// "é" is 0xC3 0xA9 (2 bytes). With cap=2, slicing at 2 lands at the rune
	// END (boundary) — fine. With cap=1, slicing would land mid-rune; the
	// back-up should drop the lead byte too.
	got := clipBytes("é-extra", 1)
	if got == "é"[:1]+"~" {
		t.Errorf("cap=1 must back up off the multibyte start, got %q (invalid UTF-8)", got)
	}
}

// Description-keyword regression: agent tool-selection latches on these
// keywords to distinguish from sibling tools. Their presence is load-bearing.
func TestOpsLogErrorsTopTool_DescriptionMentionsKeyDistinctions(t *testing.T) {
	reg := NewRegistry()
	RegisterOpsLogErrorsTopTool(reg, ContainerDeps{DefaultContainer: "ac-worldserver"})
	tool, ok := reg.Get("ops_log_errors_top")
	if !ok {
		t.Fatal("ops_log_errors_top not registered")
	}
	for _, kw := range []string{
		"container_logs_grep",
		"ops_logs_tail",
		"normalizer",
		"top-N",
		"topN",
		"sampleLimit",
		"ac-worldserver",
		"Discovery-mode",
	} {
		if !strings.Contains(tool.Description, kw) {
			t.Errorf("description missing %q: %s", kw, tool.Description)
		}
	}
}

func TestOpsLogErrorsTopTool_IsReadOnly(t *testing.T) {
	reg := NewRegistry()
	RegisterOpsLogErrorsTopTool(reg, ContainerDeps{DefaultContainer: "ac-worldserver"})
	tool, ok := reg.Get("ops_log_errors_top")
	if !ok {
		t.Fatal("ops_log_errors_top not registered")
	}
	if tool.Destructive {
		t.Error("Destructive: got true want false (read-only tool)")
	}
	if !strings.Contains(string(tool.Annotations), `"readOnlyHint":true`) {
		t.Errorf("annotations missing readOnlyHint:true: %s", tool.Annotations)
	}
}

func TestOpsLogErrorsTopTool_DefaultsContainerWhenDepsEmpty(t *testing.T) {
	reg := NewRegistry()
	RegisterOpsLogErrorsTopTool(reg, ContainerDeps{})
	if _, ok := reg.Get("ops_log_errors_top"); !ok {
		t.Fatal("ops_log_errors_top not registered when DefaultContainer empty")
	}
}
