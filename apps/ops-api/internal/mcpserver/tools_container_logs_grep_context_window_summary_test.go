package mcpserver

import (
	"strings"
	"testing"
	"time"

	"github.com/synthiq-ai/synthiqbots/apps/ops-api/internal/dockerlog"
)

// mkLineTs is the timestamped twin of mkLine (test helper from the sibling
// test file). Summary mode is meaningful only when the per-block firstTs /
// lastTs bounds carry real data — bare mkLine yields zero-valued TS that
// would mask off-by-one bugs in the TS-walk logic.
func mkLineTs(stream, msg string, ts time.Time) dockerlog.Line {
	return dockerlog.Line{Stream: stream, Msg: msg, TS: ts}
}

// Single match with -B 2 -A 1: one block, matchCount=1, lineCount=4
// (two before + match + one after), firstTs from oldest before line,
// lastTs from the trailing after line. Validates the TS-walk hits the
// outermost bounds, not the match line.
func TestGrepContextSummary_SingleMatchBeforeAfter(t *testing.T) {
	base := time.Date(2026, 5, 24, 10, 0, 0, 0, time.UTC)
	lines := []dockerlog.Line{
		mkLineTs("stdout", "line1", base),
		mkLineTs("stdout", "line2", base.Add(1*time.Second)),
		mkLineTs("stdout", "line3", base.Add(2*time.Second)),
		mkLineTs("stdout", "ERROR boom", base.Add(3*time.Second)),
		mkLineTs("stdout", "line5", base.Add(4*time.Second)),
		mkLineTs("stdout", "line6", base.Add(5*time.Second)),
	}
	matcher, _ := buildGrepMatcherCtx("ERROR", false, false)
	blocks, stats := grepContextSummary(feedLines(lines), matcher, "", 2, 1, 100, 1000, func() {})

	if len(blocks) != 1 {
		t.Fatalf("blocks: want 1 got %d", len(blocks))
	}
	if stats.matchCount != 1 {
		t.Errorf("matchCount: want 1 got %d", stats.matchCount)
	}
	b := blocks[0]
	if b.matchCount != 1 {
		t.Errorf("block.matchCount: want 1 got %d", b.matchCount)
	}
	if b.lineCount != 4 {
		t.Errorf("block.lineCount: want 4 (2 before + match + 1 after) got %d", b.lineCount)
	}
	if !b.firstTs.Equal(base.Add(1 * time.Second)) {
		t.Errorf("firstTs: want %v (oldest of 2 before lines) got %v", base.Add(1*time.Second), b.firstTs)
	}
	if !b.lastTs.Equal(base.Add(4 * time.Second)) {
		t.Errorf("lastTs: want %v (after line) got %v", base.Add(4*time.Second), b.lastTs)
	}
	if b.matchIndex != 0 {
		t.Errorf("matchIndex: want 0 got %d", b.matchIndex)
	}
}

// Coalesce: same load-bearing property as the transcript tool — two matches
// inside the after-window of the first must collapse into ONE block, no
// double-counting of shared context lines. lineCount must equal the count
// of physically unique lines that would have been emitted (1 before + match
// + 1 inter + match + 2 after = 6), NOT the naive sum (1+1+2 + 1+1+2 = 8).
func TestGrepContextSummary_AdjacentMatchesCoalesce(t *testing.T) {
	base := time.Date(2026, 5, 24, 10, 0, 0, 0, time.UTC)
	lines := []dockerlog.Line{
		mkLineTs("stdout", "line1", base),
		mkLineTs("stdout", "line2", base.Add(1*time.Second)),
		mkLineTs("stdout", "ERROR first", base.Add(2*time.Second)),
		mkLineTs("stdout", "line4", base.Add(3*time.Second)),
		mkLineTs("stdout", "ERROR second", base.Add(4*time.Second)),
		mkLineTs("stdout", "line6", base.Add(5*time.Second)),
		mkLineTs("stdout", "line7", base.Add(6*time.Second)),
	}
	matcher, _ := buildGrepMatcherCtx("ERROR", false, false)
	blocks, stats := grepContextSummary(feedLines(lines), matcher, "", 1, 2, 100, 1000, func() {})

	if len(blocks) != 1 {
		t.Fatalf("blocks: want 1 (coalesced) got %d — algorithm counted overlapping context twice", len(blocks))
	}
	if stats.matchCount != 2 {
		t.Errorf("matchCount: want 2 got %d", stats.matchCount)
	}
	b := blocks[0]
	if b.matchCount != 2 {
		t.Errorf("block.matchCount: want 2 got %d", b.matchCount)
	}
	if b.lineCount != 6 {
		t.Errorf("block.lineCount: want 6 (before+match+inter+match+after+after) got %d", b.lineCount)
	}
	if b.matchIndex != 1 {
		t.Errorf("matchIndex: want 1 (latest folded match) got %d", b.matchIndex)
	}
	if !b.firstTs.Equal(base.Add(1 * time.Second)) {
		t.Errorf("firstTs: want %v got %v", base.Add(1*time.Second), b.firstTs)
	}
	if !b.lastTs.Equal(base.Add(6 * time.Second)) {
		t.Errorf("lastTs: want %v got %v", base.Add(6*time.Second), b.lastTs)
	}
}

// Match near start of stream: only 1 line of before available even though
// before=3. lineCount must reflect the actual ring contents (1 before + match
// + 1 after = 3), NOT the requested before count.
func TestGrepContextSummary_MatchNearStart(t *testing.T) {
	base := time.Date(2026, 5, 24, 10, 0, 0, 0, time.UTC)
	lines := []dockerlog.Line{
		mkLineTs("stdout", "first", base),
		mkLineTs("stdout", "ERROR early", base.Add(1*time.Second)),
		mkLineTs("stdout", "next", base.Add(2*time.Second)),
	}
	matcher, _ := buildGrepMatcherCtx("ERROR", false, false)
	blocks, _ := grepContextSummary(feedLines(lines), matcher, "", 3, 1, 100, 1000, func() {})

	if len(blocks) != 1 {
		t.Fatalf("blocks: want 1 got %d", len(blocks))
	}
	b := blocks[0]
	if b.lineCount != 3 {
		t.Errorf("block.lineCount: want 3 (1 available before + match + after) got %d", b.lineCount)
	}
	if !b.firstTs.Equal(base) {
		t.Errorf("firstTs: want %v got %v", base, b.firstTs)
	}
}

// Match at end with no remaining lines: after-window can't fill, but the
// block MUST still appear in the summary — flushOpen on channel close is
// load-bearing.
func TestGrepContextSummary_MatchAtEndFlushes(t *testing.T) {
	base := time.Date(2026, 5, 24, 10, 0, 0, 0, time.UTC)
	lines := []dockerlog.Line{
		mkLineTs("stdout", "line1", base),
		mkLineTs("stdout", "ERROR last", base.Add(1*time.Second)),
	}
	matcher, _ := buildGrepMatcherCtx("ERROR", false, false)
	blocks, stats := grepContextSummary(feedLines(lines), matcher, "", 1, 5, 100, 1000, func() {})

	if len(blocks) != 1 {
		t.Fatalf("blocks: want 1 got %d (flushOpen missed end-of-stream match)", len(blocks))
	}
	if stats.matchCount != 1 {
		t.Errorf("matchCount: want 1 got %d", stats.matchCount)
	}
	b := blocks[0]
	if b.lineCount != 2 {
		t.Errorf("block.lineCount: want 2 (before + match, after never arrived) got %d", b.lineCount)
	}
	if !b.lastTs.Equal(base.Add(1 * time.Second)) {
		t.Errorf("lastTs: want match TS %v got %v", base.Add(1*time.Second), b.lastTs)
	}
}

// before=0 + after=0: cheap match-only summary path. Each match flushes
// immediately as its own block with matchCount=1, lineCount=1, firstTs=lastTs.
func TestGrepContextSummary_ZeroContext(t *testing.T) {
	base := time.Date(2026, 5, 24, 10, 0, 0, 0, time.UTC)
	lines := []dockerlog.Line{
		mkLineTs("stdout", "a", base),
		mkLineTs("stdout", "ERROR x", base.Add(1*time.Second)),
		mkLineTs("stdout", "b", base.Add(2*time.Second)),
		mkLineTs("stdout", "ERROR y", base.Add(3*time.Second)),
	}
	matcher, _ := buildGrepMatcherCtx("ERROR", false, false)
	blocks, stats := grepContextSummary(feedLines(lines), matcher, "", 0, 0, 100, 1000, func() {})

	if len(blocks) != 2 {
		t.Fatalf("blocks: want 2 got %d", len(blocks))
	}
	if stats.matchCount != 2 {
		t.Errorf("matchCount: want 2 got %d", stats.matchCount)
	}
	for i, b := range blocks {
		if b.matchCount != 1 || b.lineCount != 1 {
			t.Errorf("block %d: want {matchCount:1,lineCount:1} got {%d,%d}", i, b.matchCount, b.lineCount)
		}
		if !b.firstTs.Equal(b.lastTs) {
			t.Errorf("block %d: zero-context block must have firstTs == lastTs, got %v vs %v", i, b.firstTs, b.lastTs)
		}
	}
	if !blocks[0].firstTs.Equal(base.Add(1 * time.Second)) {
		t.Errorf("block 0 firstTs: want %v got %v", base.Add(1*time.Second), blocks[0].firstTs)
	}
	if !blocks[1].firstTs.Equal(base.Add(3 * time.Second)) {
		t.Errorf("block 1 firstTs: want %v got %v", base.Add(3*time.Second), blocks[1].firstTs)
	}
}

// Stream filter narrows scanning: stderr-only must skip stdout lines for both
// match and context counting. The stdout ERROR must NOT increment matchCount,
// and a preceding stdout line must NOT contribute to lineCount.
func TestGrepContextSummary_StreamFilter(t *testing.T) {
	base := time.Date(2026, 5, 24, 10, 0, 0, 0, time.UTC)
	lines := []dockerlog.Line{
		mkLineTs("stderr", "stderr-a", base),
		mkLineTs("stdout", "stdout-ERROR ignored", base.Add(1*time.Second)),
		mkLineTs("stderr", "stderr-b", base.Add(2*time.Second)),
		mkLineTs("stderr", "stderr-ERROR keep", base.Add(3*time.Second)),
		mkLineTs("stderr", "stderr-c", base.Add(4*time.Second)),
	}
	matcher, _ := buildGrepMatcherCtx("ERROR", false, false)
	blocks, stats := grepContextSummary(feedLines(lines), matcher, "stderr", 2, 1, 100, 1000, func() {})

	if len(blocks) != 1 {
		t.Fatalf("blocks: want 1 got %d", len(blocks))
	}
	if stats.matchCount != 1 {
		t.Errorf("matchCount: want 1 (stdout ERROR ignored) got %d", stats.matchCount)
	}
	b := blocks[0]
	if b.lineCount != 4 {
		t.Errorf("block.lineCount: want 4 (2 stderr before + match + 1 stderr after) got %d", b.lineCount)
	}
}

// maxMatches cap: 3 matches in stream, cap=2. matchTruncated set, but the
// emitted summary reflects only the matches actually counted. After-window
// drain for the second match still completes via flushOpen so the operator
// sees clean bounds.
func TestGrepContextSummary_MatchCap(t *testing.T) {
	base := time.Date(2026, 5, 24, 10, 0, 0, 0, time.UTC)
	lines := []dockerlog.Line{
		mkLineTs("stdout", "ERROR one", base),
		mkLineTs("stdout", "ERROR two", base.Add(1*time.Second)),
		mkLineTs("stdout", "ERROR three", base.Add(2*time.Second)),
		mkLineTs("stdout", "tail", base.Add(3*time.Second)),
	}
	matcher, _ := buildGrepMatcherCtx("ERROR", false, false)
	blocks, stats := grepContextSummary(feedLines(lines), matcher, "", 0, 0, 2, 1000, func() {})

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
// reflects what was actually found before the bail-out — block summary
// must still flush any open block (here there's no open block).
func TestGrepContextSummary_ScanCap(t *testing.T) {
	lines := make([]dockerlog.Line, 50)
	for i := range lines {
		lines[i] = mkLineTs("stdout", "noise", time.Now())
	}
	matcher, _ := buildGrepMatcherCtx("ERROR", false, false)
	_, stats := grepContextSummary(feedLines(lines), matcher, "", 0, 0, 100, 10, func() {})

	if !stats.scanTruncated {
		t.Error("scanTruncated: want true")
	}
	if stats.scanned <= 10 {
		t.Errorf("scanned: want >10 got %d", stats.scanned)
	}
}

// matchIndex on a coalesced block tracks the LATEST match anchor at flush
// time, so the summary can serve as a pointer back into a follow-up
// transcript scan. Same coalescing shape as the transcript-tool test.
func TestGrepContextSummary_MatchIndexOnCoalescedBlock(t *testing.T) {
	base := time.Date(2026, 5, 24, 10, 0, 0, 0, time.UTC)
	lines := []dockerlog.Line{
		mkLineTs("stdout", "ERROR a", base),
		mkLineTs("stdout", "tail", base.Add(1*time.Second)),
		mkLineTs("stdout", "ERROR b", base.Add(2*time.Second)),
		mkLineTs("stdout", "tail2", base.Add(3*time.Second)),
	}
	matcher, _ := buildGrepMatcherCtx("ERROR", false, false)
	blocks, _ := grepContextSummary(feedLines(lines), matcher, "", 0, 2, 100, 1000, func() {})

	if len(blocks) != 1 {
		t.Fatalf("blocks: want 1 (coalesced) got %d", len(blocks))
	}
	if blocks[0].matchIndex != 1 {
		t.Errorf("matchIndex on coalesced block: want 1 got %d", blocks[0].matchIndex)
	}
}

// Lines with zero TS (frame had no timestamp) must not poison the bounds.
// firstTs/lastTs should fall through to the first/last line that DID carry a
// TS, and the formatter must omit the field entirely when nothing usable was
// seen.
func TestGrepContextSummary_ZeroTimestampLinesSkipped(t *testing.T) {
	withTs := time.Date(2026, 5, 24, 10, 0, 0, 0, time.UTC)
	lines := []dockerlog.Line{
		mkLineTs("stdout", "before-no-ts", time.Time{}),
		mkLineTs("stdout", "ERROR match", withTs),
		mkLineTs("stdout", "after-no-ts", time.Time{}),
	}
	matcher, _ := buildGrepMatcherCtx("ERROR", false, false)
	blocks, _ := grepContextSummary(feedLines(lines), matcher, "", 1, 1, 100, 1000, func() {})

	if len(blocks) != 1 {
		t.Fatalf("blocks: want 1 got %d", len(blocks))
	}
	b := blocks[0]
	if !b.firstTs.Equal(withTs) {
		t.Errorf("firstTs: want %v (skip zero-TS before line) got %v", withTs, b.firstTs)
	}
	if !b.lastTs.Equal(withTs) {
		t.Errorf("lastTs: want %v (skip zero-TS after line) got %v", withTs, b.lastTs)
	}
}

// formatGrepCtxBlockSummary: zero-TS bounds must be OMITTED from the output
// map entirely, not serialized as the zero string. Operators downstream check
// for field presence, not zero-value.
func TestFormatGrepCtxBlockSummary_OmitsZeroTs(t *testing.T) {
	row := formatGrepCtxBlockSummary(grepCtxBlockSummary{matchCount: 1, lineCount: 1, matchIndex: 0})
	if _, ok := row["firstTs"]; ok {
		t.Error("firstTs: must be omitted when zero, got presence")
	}
	if _, ok := row["lastTs"]; ok {
		t.Error("lastTs: must be omitted when zero, got presence")
	}
}

func TestFormatGrepCtxBlockSummary_EmitsRfc3339Nano(t *testing.T) {
	ts := time.Date(2026, 5, 24, 10, 0, 0, 123456789, time.UTC)
	row := formatGrepCtxBlockSummary(grepCtxBlockSummary{firstTs: ts, lastTs: ts, matchCount: 1, lineCount: 1})
	got, _ := row["firstTs"].(string)
	if got != ts.Format(time.RFC3339Nano) {
		t.Errorf("firstTs format: want %q got %q", ts.Format(time.RFC3339Nano), got)
	}
}

// Description-keyword guard: agent tool-selection depends on the description.
// The differentiation phrases that route operators between summary vs.
// transcript vs. cheap-grep must survive editorial drift.
func TestContainerLogsGrepContextWindowSummaryTool_DescriptionMentionsKeyDistinctions(t *testing.T) {
	reg := NewRegistry()
	RegisterContainerLogsGrepContextWindowSummaryTool(reg, ContainerDeps{DefaultContainer: "ac-worldserver"})
	tool, ok := reg.Get("container_logs_grep_context_window_summary")
	if !ok {
		t.Fatal("container_logs_grep_context_window_summary not registered")
	}
	for _, kw := range []string{
		"container_logs_grep_context",
		"container_logs_grep",
		"without the line bodies",
		"coalesce",
		"matchIndex",
		"firstTs",
		"lastTs",
		"contextBefore",
		"contextAfter",
		"ac-worldserver",
	} {
		if !strings.Contains(tool.Description, kw) {
			t.Errorf("description missing %q: %s", kw, tool.Description)
		}
	}
}

func TestContainerLogsGrepContextWindowSummaryTool_IsReadOnly(t *testing.T) {
	reg := NewRegistry()
	RegisterContainerLogsGrepContextWindowSummaryTool(reg, ContainerDeps{DefaultContainer: "ac-worldserver"})
	tool, _ := reg.Get("container_logs_grep_context_window_summary")
	if tool.Destructive {
		t.Error("Destructive: got true want false (read-only)")
	}
	if !strings.Contains(string(tool.Annotations), `"readOnlyHint":true`) {
		t.Errorf("annotations missing readOnlyHint:true: %s", tool.Annotations)
	}
}

func TestContainerLogsGrepContextWindowSummaryTool_DefaultsContainerWhenDepsEmpty(t *testing.T) {
	reg := NewRegistry()
	RegisterContainerLogsGrepContextWindowSummaryTool(reg, ContainerDeps{})
	if _, ok := reg.Get("container_logs_grep_context_window_summary"); !ok {
		t.Fatal("not registered when DefaultContainer empty")
	}
}
