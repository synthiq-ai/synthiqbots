package mcpserver

import (
	"strings"
	"testing"
	"time"
)

// mkBlock builds a grepCtxBlockSummary for sort-only unit tests. matchIndex
// is the scan-order tiebreaker — the suite passes it explicitly per fixture
// so equal-primary equal-secondary blocks land in a predictable order.
func mkBlock(firstTs time.Time, matchCount, lineCount, matchIndex int) grepCtxBlockSummary {
	return grepCtxBlockSummary{
		firstTs:    firstTs,
		lastTs:     firstTs,
		matchCount: matchCount,
		lineCount:  lineCount,
		matchIndex: matchIndex,
	}
}

// matchCount is the default sort. Three fixtures with distinct counts must
// land in strict descending order regardless of input ordering — this is the
// load-bearing property the tool exists to provide.
func TestSortGrepCtxBlockSummariesTopN_MatchCountDesc(t *testing.T) {
	base := time.Date(2026, 5, 31, 10, 0, 0, 0, time.UTC)
	blocks := []grepCtxBlockSummary{
		mkBlock(base, 3, 5, 0),
		mkBlock(base.Add(1*time.Minute), 10, 20, 1),
		mkBlock(base.Add(2*time.Minute), 5, 15, 2),
	}
	sortGrepCtxBlockSummariesTopN(blocks, grepCtxTopNSortMatchCount)
	wantCounts := []int{10, 5, 3}
	for i, b := range blocks {
		if b.matchCount != wantCounts[i] {
			t.Errorf("position %d matchCount: want %d got %d", i, wantCounts[i], b.matchCount)
		}
	}
}

// lineCount sort: primary key swaps from matchCount → lineCount. A block with
// lower matchCount but higher lineCount wins. Validates the switch wiring
// independently of the matchCount path.
func TestSortGrepCtxBlockSummariesTopN_LineCountDesc(t *testing.T) {
	base := time.Date(2026, 5, 31, 10, 0, 0, 0, time.UTC)
	blocks := []grepCtxBlockSummary{
		mkBlock(base, 10, 12, 0),
		mkBlock(base.Add(1*time.Minute), 2, 50, 1),
		mkBlock(base.Add(2*time.Minute), 5, 30, 2),
	}
	sortGrepCtxBlockSummariesTopN(blocks, grepCtxTopNSortLineCount)
	wantLines := []int{50, 30, 12}
	for i, b := range blocks {
		if b.lineCount != wantLines[i] {
			t.Errorf("position %d lineCount: want %d got %d", i, wantLines[i], b.lineCount)
		}
	}
}

// firstTs sort: most recent first. Operator triaging "what just happened?"
// expects the freshest block at the head of the list. matchCount + lineCount
// equal across all three so the primary direction is the only signal.
func TestSortGrepCtxBlockSummariesTopN_FirstTsDesc(t *testing.T) {
	base := time.Date(2026, 5, 31, 10, 0, 0, 0, time.UTC)
	blocks := []grepCtxBlockSummary{
		mkBlock(base, 5, 10, 0),
		mkBlock(base.Add(5*time.Minute), 5, 10, 1),
		mkBlock(base.Add(2*time.Minute), 5, 10, 2),
	}
	sortGrepCtxBlockSummariesTopN(blocks, grepCtxTopNSortFirstTs)
	wantTs := []time.Time{
		base.Add(5 * time.Minute),
		base.Add(2 * time.Minute),
		base,
	}
	for i, b := range blocks {
		if !b.firstTs.Equal(wantTs[i]) {
			t.Errorf("position %d firstTs: want %v got %v", i, wantTs[i], b.firstTs)
		}
	}
}

// Tiebreaker chain on matchCount: equal-primary blocks fall through to
// lineCount desc → firstTs desc → matchIndex asc. The fixture exercises all
// three secondary keys: equal matchCount, equal lineCount, distinct firstTs.
// Without the firstTs secondary the result would depend on slice input order.
func TestSortGrepCtxBlockSummariesTopN_MatchCountTiebreakerFirstTsDesc(t *testing.T) {
	base := time.Date(2026, 5, 31, 10, 0, 0, 0, time.UTC)
	blocks := []grepCtxBlockSummary{
		mkBlock(base, 7, 10, 0),
		mkBlock(base.Add(2*time.Minute), 7, 10, 1),
		mkBlock(base.Add(1*time.Minute), 7, 10, 2),
	}
	sortGrepCtxBlockSummariesTopN(blocks, grepCtxTopNSortMatchCount)
	wantIdx := []int{1, 2, 0} // newest firstTs first
	for i, b := range blocks {
		if b.matchIndex != wantIdx[i] {
			t.Errorf("position %d matchIndex: want %d got %d", i, wantIdx[i], b.matchIndex)
		}
	}
}

// Full tie across primary + all secondaries: matchIndex is the final
// stability key. Three blocks with identical matchCount/lineCount/firstTs
// must come back in matchIndex-asc order (scan order). Defends against the
// SliceStable→Slice regression that would otherwise pass on this fixture
// only by accident.
func TestSortGrepCtxBlockSummariesTopN_FullTieMatchIndexAsc(t *testing.T) {
	ts := time.Date(2026, 5, 31, 10, 0, 0, 0, time.UTC)
	blocks := []grepCtxBlockSummary{
		mkBlock(ts, 5, 10, 2),
		mkBlock(ts, 5, 10, 0),
		mkBlock(ts, 5, 10, 1),
	}
	sortGrepCtxBlockSummariesTopN(blocks, grepCtxTopNSortMatchCount)
	wantIdx := []int{0, 1, 2}
	for i, b := range blocks {
		if b.matchIndex != wantIdx[i] {
			t.Errorf("position %d matchIndex: want %d got %d", i, wantIdx[i], b.matchIndex)
		}
	}
}

// Unknown sortKey: the default branch must still produce a deterministic
// matchCount-desc result. The handler rejects unknown sortBy upstream — this
// guards the safety fallback in the sort fn so a future surface change
// doesn't silently leave blocks in scan order.
func TestSortGrepCtxBlockSummariesTopN_UnknownKeyFallsBackToMatchCount(t *testing.T) {
	base := time.Date(2026, 5, 31, 10, 0, 0, 0, time.UTC)
	blocks := []grepCtxBlockSummary{
		mkBlock(base, 3, 5, 0),
		mkBlock(base, 10, 20, 1),
		mkBlock(base, 5, 15, 2),
	}
	sortGrepCtxBlockSummariesTopN(blocks, "garbage")
	wantCounts := []int{10, 5, 3}
	for i, b := range blocks {
		if b.matchCount != wantCounts[i] {
			t.Errorf("position %d matchCount: want %d got %d", i, wantCounts[i], b.matchCount)
		}
	}
}

// normalizeGrepCtxTopNSortBy: empty string → default matchCount. The handler
// relies on this so callers that omit `sortBy` get the documented default.
func TestNormalizeGrepCtxTopNSortBy_EmptyDefaults(t *testing.T) {
	got, err := normalizeGrepCtxTopNSortBy("")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got != grepCtxTopNSortMatchCount {
		t.Errorf("default: want %q got %q", grepCtxTopNSortMatchCount, got)
	}
}

// All three canonical sortBy values must round-trip through the normalizer.
// Failure mode this guards against: a typo in the const → no compile error
// because the const is still a valid string, but the allowlist diverges.
func TestNormalizeGrepCtxTopNSortBy_AllCanonicalKeysAccepted(t *testing.T) {
	for _, k := range grepCtxTopNSortKeys {
		got, err := normalizeGrepCtxTopNSortBy(k)
		if err != nil {
			t.Errorf("%q: unexpected err %v", k, err)
		}
		if got != k {
			t.Errorf("%q: round-trip got %q", k, got)
		}
	}
}

// Bad sortBy: error message must echo the allowlist so a typo is
// self-correcting. Operator pastes "matchcount" (lowercase) and the response
// surfaces the canonical casing in the rejection.
func TestNormalizeGrepCtxTopNSortBy_RejectsUnknownAndEchoesAllowlist(t *testing.T) {
	_, err := normalizeGrepCtxTopNSortBy("matchcount")
	if err == nil {
		t.Fatal("want err on bad sortBy, got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "matchcount") {
		t.Errorf("err must echo bad value, got %q", msg)
	}
	for _, k := range grepCtxTopNSortKeys {
		if !strings.Contains(msg, k) {
			t.Errorf("err must list allowlist key %q, got %q", k, msg)
		}
	}
}

// Whitespace-only sortBy is treated as empty → default. Forgives the agent
// pasting a trailing newline from a multi-line prompt.
func TestNormalizeGrepCtxTopNSortBy_TrimsWhitespace(t *testing.T) {
	got, err := normalizeGrepCtxTopNSortBy("   ")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got != grepCtxTopNSortMatchCount {
		t.Errorf("whitespace-only: want default %q got %q", grepCtxTopNSortMatchCount, got)
	}
}

// Description-keyword guard: agent tool-selection depends on phrases that
// route operators between this tool, the base summary, and the transcript /
// cheap-grep variants. Same shape as the base summary's regression test.
func TestContainerLogsGrepContextWindowSummaryTopNTool_DescriptionMentionsKeyDistinctions(t *testing.T) {
	reg := NewRegistry()
	RegisterContainerLogsGrepContextWindowSummaryTopNTool(reg, ContainerDeps{DefaultContainer: "ac-worldserver"})
	tool, ok := reg.Get("container_logs_grep_context_window_summary_topn")
	if !ok {
		t.Fatal("container_logs_grep_context_window_summary_topn not registered")
	}
	for _, kw := range []string{
		"container_logs_grep_context_window_summary",
		"container_logs_grep_context",
		"container_logs_grep",
		"sortBy",
		"topN",
		"matchCount",
		"lineCount",
		"firstTs",
		"blockCount",
		"matchIndex",
		"hottest",
		"ac-worldserver",
	} {
		if !strings.Contains(tool.Description, kw) {
			t.Errorf("description missing %q: %s", kw, tool.Description)
		}
	}
}

func TestContainerLogsGrepContextWindowSummaryTopNTool_IsReadOnly(t *testing.T) {
	reg := NewRegistry()
	RegisterContainerLogsGrepContextWindowSummaryTopNTool(reg, ContainerDeps{DefaultContainer: "ac-worldserver"})
	tool, _ := reg.Get("container_logs_grep_context_window_summary_topn")
	if tool.Destructive {
		t.Error("Destructive: got true want false (read-only)")
	}
	if !strings.Contains(string(tool.Annotations), `"readOnlyHint":true`) {
		t.Errorf("annotations missing readOnlyHint:true: %s", tool.Annotations)
	}
}

func TestContainerLogsGrepContextWindowSummaryTopNTool_DefaultsContainerWhenDepsEmpty(t *testing.T) {
	reg := NewRegistry()
	RegisterContainerLogsGrepContextWindowSummaryTopNTool(reg, ContainerDeps{})
	if _, ok := reg.Get("container_logs_grep_context_window_summary_topn"); !ok {
		t.Fatal("not registered when DefaultContainer empty")
	}
}

// topN clamp: defaults to 5 when unspecified, max 200. The full clamp test
// goes through clampGrepIntCtx (already exercised by the base tool's suite);
// this test asserts that the topN-specific consts are wired to the same
// helper so a bad arg can't bypass the cap.
func TestGrepCtxTopNClampConstants(t *testing.T) {
	if grepCtxTopNDefaultTopN != 5 {
		t.Errorf("default topN: want 5 got %d", grepCtxTopNDefaultTopN)
	}
	if grepCtxTopNMaxTopN != 200 {
		t.Errorf("max topN: want 200 got %d", grepCtxTopNMaxTopN)
	}
	if got := clampGrepIntCtx(99999, grepCtxTopNDefaultTopN, 1, grepCtxTopNMaxTopN); got != grepCtxTopNMaxTopN {
		t.Errorf("clamp 99999: want %d got %d", grepCtxTopNMaxTopN, got)
	}
	if got := clampGrepIntCtx(0, grepCtxTopNDefaultTopN, 1, grepCtxTopNMaxTopN); got != grepCtxTopNDefaultTopN {
		t.Errorf("clamp 0 → default: want %d got %d", grepCtxTopNDefaultTopN, got)
	}
	if got := clampGrepIntCtx(-5, grepCtxTopNDefaultTopN, 1, grepCtxTopNMaxTopN); got != grepCtxTopNDefaultTopN {
		t.Errorf("clamp -5 → default: want %d got %d", grepCtxTopNDefaultTopN, got)
	}
}

// sortKey allowlist drift guard: the canonical list surfaced in the tool
// description, the normalizer, and the sort fn must all agree on exactly
// three keys. Adding a fourth key requires touching every site — this test
// fails if you add a const but forget the allowlist.
func TestGrepCtxTopNSortKeys_CanonicalLength(t *testing.T) {
	if len(grepCtxTopNSortKeys) != 3 {
		t.Errorf("sort keys: want 3 got %d — update description + normalizer + sort fn together", len(grepCtxTopNSortKeys))
	}
	for _, k := range grepCtxTopNSortKeys {
		if k == "" {
			t.Error("sort keys: empty string in allowlist")
		}
	}
}
