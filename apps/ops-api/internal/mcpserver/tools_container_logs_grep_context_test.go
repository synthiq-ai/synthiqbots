package mcpserver

import (
	"strings"
	"testing"
	"time"

	"github.com/synthiq-ai/synthiqbots/apps/ops-api/internal/dockerlog"
)

// feedLines spins up a buffered channel populated with the given lines and
// closes it. Buffered + pre-closed lets grepWithContext drain deterministically
// without needing real cancellation plumbing in unit tests.
func feedLines(lines []dockerlog.Line) <-chan dockerlog.Line {
	ch := make(chan dockerlog.Line, len(lines)+1)
	for _, l := range lines {
		ch <- l
	}
	close(ch)
	return ch
}

func mkLine(stream, msg string) dockerlog.Line {
	return dockerlog.Line{Stream: stream, Msg: msg}
}

func lineKinds(block map[string]any) []string {
	rows, _ := block["lines"].([]map[string]any)
	kinds := make([]string, len(rows))
	for i, r := range rows {
		kinds[i], _ = r["kind"].(string)
	}
	return kinds
}

func lineMsgs(block map[string]any) []string {
	rows, _ := block["lines"].([]map[string]any)
	msgs := make([]string, len(rows))
	for i, r := range rows {
		msgs[i], _ = r["msg"].(string)
	}
	return msgs
}

// Baseline: a single match with -B 2 -A 1 returns 2 before + match + 1 after,
// in scan order. Anything else is the algorithm losing track of the ring or
// the post-match countdown.
func TestGrepWithContext_SingleMatchBeforeAfter(t *testing.T) {
	lines := []dockerlog.Line{
		mkLine("stdout", "line1"),
		mkLine("stdout", "line2"),
		mkLine("stdout", "line3"),
		mkLine("stdout", "ERROR boom"),
		mkLine("stdout", "line5"),
		mkLine("stdout", "line6"),
	}
	matcher, _ := buildGrepMatcherCtx("ERROR", false, false)
	blocks, stats := grepWithContext(feedLines(lines), matcher, "", 2, 1, 100, 1000, func() {})

	if len(blocks) != 1 {
		t.Fatalf("blocks: want 1 got %d", len(blocks))
	}
	if stats.matchCount != 1 {
		t.Errorf("matchCount: want 1 got %d", stats.matchCount)
	}
	wantKinds := []string{"before", "before", "match", "after"}
	if got := lineKinds(blocks[0]); !strSlicesEqual(got, wantKinds) {
		t.Errorf("kinds: want %v got %v", wantKinds, got)
	}
	wantMsgs := []string{"line2", "line3", "ERROR boom", "line5"}
	if got := lineMsgs(blocks[0]); !strSlicesEqual(got, wantMsgs) {
		t.Errorf("msgs: want %v got %v", wantMsgs, got)
	}
}

// Coalesce: two matches within the after-window of the first must produce ONE
// block with both match lines, no duplicated context lines. This is the
// load-bearing property that justifies the algorithm over "emit B before +
// match + A after per match" — naive emission would repeat lines 2-3 below.
func TestGrepWithContext_AdjacentMatchesCoalesce(t *testing.T) {
	lines := []dockerlog.Line{
		mkLine("stdout", "line1"),
		mkLine("stdout", "line2"),
		mkLine("stdout", "ERROR first"),
		mkLine("stdout", "line4"),
		mkLine("stdout", "ERROR second"),
		mkLine("stdout", "line6"),
		mkLine("stdout", "line7"),
	}
	matcher, _ := buildGrepMatcherCtx("ERROR", false, false)
	blocks, stats := grepWithContext(feedLines(lines), matcher, "", 1, 2, 100, 1000, func() {})

	if len(blocks) != 1 {
		t.Fatalf("blocks: want 1 (coalesced) got %d — algorithm emitted overlapping context twice", len(blocks))
	}
	if stats.matchCount != 2 {
		t.Errorf("matchCount: want 2 got %d", stats.matchCount)
	}
	wantMsgs := []string{"line2", "ERROR first", "line4", "ERROR second", "line6", "line7"}
	if got := lineMsgs(blocks[0]); !strSlicesEqual(got, wantMsgs) {
		t.Errorf("msgs: want %v got %v", wantMsgs, got)
	}
	wantKinds := []string{"before", "match", "after", "match", "after", "after"}
	if got := lineKinds(blocks[0]); !strSlicesEqual(got, wantKinds) {
		t.Errorf("kinds: want %v got %v", wantKinds, got)
	}
}

// Match near start of stream: only 1 line of before available even though
// before=3. Ring buffer must not emit phantom empty lines.
func TestGrepWithContext_MatchNearStart(t *testing.T) {
	lines := []dockerlog.Line{
		mkLine("stdout", "first"),
		mkLine("stdout", "ERROR early"),
		mkLine("stdout", "next"),
	}
	matcher, _ := buildGrepMatcherCtx("ERROR", false, false)
	blocks, _ := grepWithContext(feedLines(lines), matcher, "", 3, 1, 100, 1000, func() {})

	if len(blocks) != 1 {
		t.Fatalf("blocks: want 1 got %d", len(blocks))
	}
	wantKinds := []string{"before", "match", "after"}
	if got := lineKinds(blocks[0]); !strSlicesEqual(got, wantKinds) {
		t.Errorf("kinds: want %v got %v", wantKinds, got)
	}
}

// Match at end with no remaining lines: after-window can't fill, but the
// block MUST still close on channel close — otherwise flushOpen would drop
// the match.
func TestGrepWithContext_MatchAtEndFlushes(t *testing.T) {
	lines := []dockerlog.Line{
		mkLine("stdout", "line1"),
		mkLine("stdout", "ERROR last"),
	}
	matcher, _ := buildGrepMatcherCtx("ERROR", false, false)
	blocks, stats := grepWithContext(feedLines(lines), matcher, "", 1, 5, 100, 1000, func() {})

	if len(blocks) != 1 {
		t.Fatalf("blocks: want 1 got %d", len(blocks))
	}
	if stats.matchCount != 1 {
		t.Errorf("matchCount: want 1 got %d", stats.matchCount)
	}
	if got := lineMsgs(blocks[0]); !strSlicesEqual(got, []string{"line1", "ERROR last"}) {
		t.Errorf("msgs: %v", got)
	}
}

// before=0 + after=0: the cheap match-only shape. Returns each match as a
// single-line block. Validates the tool degrades gracefully when callers
// don't ask for context.
func TestGrepWithContext_ZeroContext(t *testing.T) {
	lines := []dockerlog.Line{
		mkLine("stdout", "a"),
		mkLine("stdout", "ERROR x"),
		mkLine("stdout", "b"),
		mkLine("stdout", "ERROR y"),
	}
	matcher, _ := buildGrepMatcherCtx("ERROR", false, false)
	blocks, stats := grepWithContext(feedLines(lines), matcher, "", 0, 0, 100, 1000, func() {})

	if len(blocks) != 2 {
		t.Fatalf("blocks: want 2 got %d", len(blocks))
	}
	if stats.matchCount != 2 {
		t.Errorf("matchCount: want 2 got %d", stats.matchCount)
	}
	for i, b := range blocks {
		if got := lineKinds(b); !strSlicesEqual(got, []string{"match"}) {
			t.Errorf("block %d kinds: %v", i, got)
		}
	}
}

// Stream filter narrows scanning: stderr-only must skip stdout lines for both
// match and context. A stdout "ERROR" must NOT pull a preceding stderr line
// into context because that's not the channel the operator filtered to.
func TestGrepWithContext_StreamFilter(t *testing.T) {
	lines := []dockerlog.Line{
		mkLine("stderr", "stderr-a"),
		mkLine("stdout", "stdout-ERROR ignored"),
		mkLine("stderr", "stderr-b"),
		mkLine("stderr", "stderr-ERROR keep"),
		mkLine("stderr", "stderr-c"),
	}
	matcher, _ := buildGrepMatcherCtx("ERROR", false, false)
	blocks, stats := grepWithContext(feedLines(lines), matcher, "stderr", 2, 1, 100, 1000, func() {})

	if len(blocks) != 1 {
		t.Fatalf("blocks: want 1 got %d", len(blocks))
	}
	if stats.matchCount != 1 {
		t.Errorf("matchCount: want 1 got %d (stdout ERROR must be ignored)", stats.matchCount)
	}
	wantMsgs := []string{"stderr-a", "stderr-b", "stderr-ERROR keep", "stderr-c"}
	if got := lineMsgs(blocks[0]); !strSlicesEqual(got, wantMsgs) {
		t.Errorf("msgs: want %v got %v", wantMsgs, got)
	}
}

// maxMatches cap: 3 matches in stream, cap=2. The third is NOT emitted, but
// the second's after-window still drains so the operator gets clean context
// for what they did get.
func TestGrepWithContext_MatchCap(t *testing.T) {
	lines := []dockerlog.Line{
		mkLine("stdout", "ERROR one"),
		mkLine("stdout", "ERROR two"),
		mkLine("stdout", "ERROR three"),
		mkLine("stdout", "tail"),
	}
	matcher, _ := buildGrepMatcherCtx("ERROR", false, false)
	blocks, stats := grepWithContext(feedLines(lines), matcher, "", 0, 0, 2, 1000, func() {})

	if stats.matchCount != 2 {
		t.Errorf("matchCount: want 2 got %d", stats.matchCount)
	}
	if !stats.matchTruncated {
		t.Error("matchTruncated: want true")
	}
	if len(blocks) != 2 {
		t.Errorf("blocks: want 2 got %d", len(blocks))
	}
}

// maxScan cap: terminates the loop with scanTruncated=true. Match count
// reflects what was actually found before the bail-out.
func TestGrepWithContext_ScanCap(t *testing.T) {
	lines := make([]dockerlog.Line, 50)
	for i := range lines {
		lines[i] = mkLine("stdout", "noise")
	}
	matcher, _ := buildGrepMatcherCtx("ERROR", false, false)
	_, stats := grepWithContext(feedLines(lines), matcher, "", 0, 0, 100, 10, func() {})

	if !stats.scanTruncated {
		t.Error("scanTruncated: want true")
	}
	if stats.scanned <= 10 {
		// We bail "scanned > maxScan", so scanned reaches 11 before the break.
		t.Errorf("scanned: want >10 got %d", stats.scanned)
	}
}

// matchIndex on the emitted block reflects the LAST match folded into the
// block at flush time (zero-indexed across all matches seen). For a coalesced
// 2-match block this is 1. The convention is "what was the latest match
// anchor when this block closed" — useful for cross-referencing with a
// non-context grep's match list.
//
// Coalescing requires the second match to arrive INSIDE the first's
// after-window. With contextAfter=2, the "tail" line consumes one after slot
// without closing the block, then "ERROR b" lands while pendingAfter still > 0
// and shares the same block.
func TestGrepWithContext_MatchIndexOnCoalescedBlock(t *testing.T) {
	lines := []dockerlog.Line{
		mkLine("stdout", "ERROR a"),
		mkLine("stdout", "tail"),
		mkLine("stdout", "ERROR b"),
		mkLine("stdout", "tail2"),
	}
	matcher, _ := buildGrepMatcherCtx("ERROR", false, false)
	blocks, _ := grepWithContext(feedLines(lines), matcher, "", 0, 2, 100, 1000, func() {})

	if len(blocks) != 1 {
		t.Fatalf("blocks: want 1 (coalesced) got %d", len(blocks))
	}
	mi, _ := blocks[0]["matchIndex"].(int)
	if mi != 1 {
		t.Errorf("matchIndex on coalesced block: want 1 got %d", mi)
	}
}

func TestBuildGrepMatcherCtx_SubstringDefault(t *testing.T) {
	m, err := buildGrepMatcherCtx("OOM", false, false)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if !m("kernel: OOM killer invoked") {
		t.Error("expected substring hit")
	}
	if m("kernel: oom killer invoked") {
		t.Error("case-sensitive default must not match lowercase")
	}
}

func TestBuildGrepMatcherCtx_CaseInsensitiveSubstring(t *testing.T) {
	m, _ := buildGrepMatcherCtx("OOM", false, true)
	if !m("kernel: oom killer invoked") {
		t.Error("ci substring must match lowercase")
	}
}

// Same (?i)-inline contract as the sibling tool: regex case insensitivity
// must NOT be implemented via ToLower on the line, or [A-Z]+ stops matching
// uppercase tokens.
func TestBuildGrepMatcherCtx_RegexCaseInsensitiveUsesInlineFlag(t *testing.T) {
	m, err := buildGrepMatcherCtx(`exitCode\s*=\s*\d+`, true, true)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if !m("ExitCode = 137") {
		t.Error("expected ci regex hit")
	}
}

func TestBuildGrepMatcherCtx_BadRegex(t *testing.T) {
	if _, err := buildGrepMatcherCtx("[unclosed", true, false); err == nil {
		t.Error("expected error for malformed regex")
	}
}

func TestResolveGrepSinceCtx_DurationFormats(t *testing.T) {
	now := time.Now().Unix()
	cases := []struct {
		in        string
		wantDelta int64
	}{
		{"", 3600},
		{"30m", 1800},
		{"12h", 12 * 3600},
		{"7d", 7 * 86400},
	}
	for _, c := range cases {
		got, err := resolveGrepSinceCtx(c.in)
		if err != nil {
			t.Errorf("resolveGrepSinceCtx(%q): %v", c.in, err)
			continue
		}
		want := now - c.wantDelta
		if got < want-2 || got > want+2 {
			t.Errorf("resolveGrepSinceCtx(%q): got %d want ~%d", c.in, got, want)
		}
	}
}

func TestResolveGrepSinceCtx_ClampsBeyondMaxWindow(t *testing.T) {
	now := time.Now().Unix()
	got, err := resolveGrepSinceCtx("30d")
	if err != nil {
		t.Fatalf("resolveGrepSinceCtx(30d): %v", err)
	}
	want := now - int64(containerLogsGrepCtxMaxWindow.Seconds())
	if got < want-2 || got > want+2 {
		t.Errorf("30d clamp: got %d want ~%d", got, want)
	}
}

func TestResolveGrepSinceCtx_RejectsFuture(t *testing.T) {
	future := time.Now().Add(1 * time.Hour).Unix()
	if _, err := resolveGrepSinceCtx(formatInt64(future)); err == nil {
		t.Error("expected error for future unix timestamp")
	}
}

func TestResolveGrepSinceCtx_RejectsGarbage(t *testing.T) {
	if _, err := resolveGrepSinceCtx("nonsense"); err == nil {
		t.Error("expected error for garbage input")
	}
}

func TestResolveGrepSinceCtx_RejectsNegative(t *testing.T) {
	if _, err := resolveGrepSinceCtx("-1h"); err == nil {
		t.Error("expected error for negative duration")
	}
}

func TestNormalizeStreamFilterCtx(t *testing.T) {
	cases := []struct {
		in   string
		want string
		err  bool
	}{
		{"", "", false},
		{"both", "", false},
		{"stdout", "stdout", false},
		{"stderr", "stderr", false},
		{"STDOUT", "stdout", false},
		{"junk", "", true},
	}
	for _, c := range cases {
		got, err := normalizeStreamFilterCtx(c.in)
		if c.err {
			if err == nil {
				t.Errorf("normalizeStreamFilterCtx(%q): expected err", c.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("normalizeStreamFilterCtx(%q): %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("normalizeStreamFilterCtx(%q): got %q want %q", c.in, got, c.want)
		}
	}
}

// Description-keyword guard: agent tool-selection depends on the description,
// so the keywords that differentiate this tool from container_logs_grep
// (cheap match-only path) and ops_logs_tail (5000-line cap) need a regression
// test. Operator-facing args also pinned (contextBefore/contextAfter).
func TestContainerLogsGrepContextTool_DescriptionMentionsKeyDistinctions(t *testing.T) {
	reg := NewRegistry()
	RegisterContainerLogsGrepContextTool(reg, ContainerDeps{DefaultContainer: "ac-worldserver"})
	tool, ok := reg.Get("container_logs_grep_context")
	if !ok {
		t.Fatal("container_logs_grep_context not registered")
	}
	for _, kw := range []string{
		"container_logs_grep",
		"ops_logs_tail",
		"contextBefore",
		"contextAfter",
		"coalesce",
		"ac-worldserver",
	} {
		if !strings.Contains(tool.Description, kw) {
			t.Errorf("description missing %q: %s", kw, tool.Description)
		}
	}
}

func TestContainerLogsGrepContextTool_IsReadOnly(t *testing.T) {
	reg := NewRegistry()
	RegisterContainerLogsGrepContextTool(reg, ContainerDeps{DefaultContainer: "ac-worldserver"})
	tool, _ := reg.Get("container_logs_grep_context")
	if tool.Destructive {
		t.Error("Destructive: got true want false (read-only)")
	}
	if !strings.Contains(string(tool.Annotations), `"readOnlyHint":true`) {
		t.Errorf("annotations missing readOnlyHint:true: %s", tool.Annotations)
	}
}

func TestContainerLogsGrepContextTool_DefaultsContainerWhenDepsEmpty(t *testing.T) {
	reg := NewRegistry()
	RegisterContainerLogsGrepContextTool(reg, ContainerDeps{})
	if _, ok := reg.Get("container_logs_grep_context"); !ok {
		t.Fatal("not registered when DefaultContainer empty")
	}
}

func TestClampGrepIntCtx_NegativeReturnsDefault(t *testing.T) {
	if got := clampGrepIntCtx(-1, 5, 0, 10); got != 5 {
		t.Errorf("got %d want 5", got)
	}
}

func TestClampGrepIntCtx_OverMaxClamps(t *testing.T) {
	if got := clampGrepIntCtx(100, 5, 0, 10); got != 10 {
		t.Errorf("got %d want 10", got)
	}
}

func TestClampGrepIntCtx_ZeroReturnsDefault(t *testing.T) {
	if got := clampGrepIntCtx(0, 7, 0, 10); got != 7 {
		t.Errorf("got %d want 7", got)
	}
}

func strSlicesEqual(a, b []string) bool {
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
