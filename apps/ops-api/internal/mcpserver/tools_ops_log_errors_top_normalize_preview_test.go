package mcpserver

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// invokePreview is a tiny driver that registers the preview tool, marshals
// the args, and returns the handler's response as map[string]any. Mirrors
// the call shape used elsewhere in the package.
func invokePreview(t *testing.T, args map[string]any) map[string]any {
	t.Helper()
	reg := NewRegistry()
	RegisterOpsLogErrorsTopNormalizePreviewTool(reg)
	tool, ok := reg.Get("ops_log_errors_top_normalize_preview")
	if !ok {
		t.Fatal("ops_log_errors_top_normalize_preview not registered")
	}
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	out := tool.Handler(context.Background(), raw, "")
	m, ok := out.(map[string]any)
	if !ok {
		t.Fatalf("handler returned %T not map[string]any", out)
	}
	return m
}

// Load-bearing invariant: the trace function must return the same final
// string as the untraced normalizer. If either drifts the preview lies
// to operators about what the main tool will see — this test fails first
// and forces the other implementation to follow.
func TestNormalizeLogLineWithTrace_MatchesUntracedOutput(t *testing.T) {
	fixtures := []string{
		"2026-05-29 12:34:56 ERROR: failed to load 1234 from creatures",
		"frame=0x7f8c12345678 ret=0xdeadbeef ptr=a1b2c3d4e5f6",
		"session=550e8400-e29b-41d4-a716-446655440000 closed",
		"Client connect from 192.168.100.18:54321",
		"plain line with no variables at all",
		"",
		"   leading and trailing whitespace   ",
		"INFO not an error line but exercise the path anyway",
	}
	for _, in := range fixtures {
		trun := normalizeLogLine(in)
		ttrace, _ := normalizeLogLineWithTrace(in)
		if trun != ttrace {
			t.Errorf("drift on %q: normalizeLogLine=%q normalizeLogLineWithTrace=%q", in, trun, ttrace)
		}
	}
}

// Rule names + placeholders must match what callers see in the
// availableRules catalog. If the catalog diverges from what the trace
// emits, operators get a useless reference.
func TestNormalizeLogLineWithTrace_RuleNamesMatchCatalog(t *testing.T) {
	// Synthetic line that fires every rule at least once.
	line := "2026-05-29T12:34:56 2025-01-01 11:22:33 198.51.100.4:9999 " +
		"550e8400-e29b-41d4-a716-446655440000 0xdeadbeef a1b2c3d4e5f6 42"
	_, trace := normalizeLogLineWithTrace(line)
	catalog := availableNormalizerRules()

	catalogRules := map[string]string{}
	for _, c := range catalog {
		catalogRules[c["rule"]] = c["placeholder"]
	}
	for _, t2 := range trace {
		want, ok := catalogRules[t2.Rule]
		if !ok {
			t.Errorf("trace emitted rule %q absent from availableRules catalog", t2.Rule)
		}
		if want != t2.Placeholder {
			t.Errorf("rule %q: placeholder %q in catalog but %q in trace", t2.Rule, want, t2.Placeholder)
		}
	}
}

// Counts: trace count == number of matches BEFORE replacement. A line with
// three digit runs must report digits.count=3 (not 1 from a single
// post-replacement scan).
func TestNormalizeLogLineWithTrace_CountsMatchesNotReplacements(t *testing.T) {
	_, trace := normalizeLogLineWithTrace("err 1 then 2 then 3")
	digits := traceCount(trace, "digits")
	if digits != 3 {
		t.Errorf("digit run count: want 3 got %d (trace=%+v)", digits, trace)
	}
}

// Trace order mirrors the sequence in normalizeLogLine — ts before digits.
// Operators read the trace as "first this collapsed, then that"; an
// out-of-order trace describes a normalization that didn't happen.
func TestNormalizeLogLineWithTrace_PreservesRuleOrder(t *testing.T) {
	_, trace := normalizeLogLineWithTrace("2026-05-29 12:34:56 lost 42 packets")
	// trace must contain iso_timestamp BEFORE digits (digits would otherwise
	// eat the timestamp's components).
	tsIdx, digitsIdx := -1, -1
	for i, r := range trace {
		if r.Rule == "iso_timestamp" {
			tsIdx = i
		}
		if r.Rule == "digits" {
			digitsIdx = i
		}
	}
	if tsIdx < 0 || digitsIdx < 0 {
		t.Fatalf("trace missing expected rules: %+v", trace)
	}
	if tsIdx >= digitsIdx {
		t.Errorf("trace order broken: iso_timestamp at %d, digits at %d", tsIdx, digitsIdx)
	}
}

// Rules with zero matches are omitted from the trace — keeps the response
// shape lean and matches operator intuition (an empty rule list reads
// the same as "didn't apply").
func TestNormalizeLogLineWithTrace_OmitsZeroCountRules(t *testing.T) {
	_, trace := normalizeLogLineWithTrace("plain text with no variables")
	if len(trace) != 0 {
		t.Errorf("trace must be empty when no rules fired, got %+v", trace)
	}
}

// End-to-end through the tool handler: matched line surfaces normalized
// + rules; non-matched line surfaces matched=false with no normalized key.
func TestPreviewTool_MatchedAndUnmatchedShape(t *testing.T) {
	out := invokePreview(t, map[string]any{
		"lines": []string{
			"ERROR: failed to load 1234 from creatures",
			"INFO: nothing wrong here",
		},
	})
	if errStr, ok := out["error"]; ok {
		t.Fatalf("unexpected error: %v", errStr)
	}
	results, ok := out["results"].([]map[string]any)
	if !ok || len(results) != 2 {
		t.Fatalf("results shape: want 2-elem []map[string]any, got %T %v", out["results"], out["results"])
	}
	row0 := results[0]
	if row0["matched"] != true {
		t.Errorf("row0 matched: want true got %v", row0["matched"])
	}
	if _, ok := row0["normalized"]; !ok {
		t.Errorf("row0 missing normalized key: %+v", row0)
	}
	row1 := results[1]
	if row1["matched"] != false {
		t.Errorf("row1 matched: want false got %v", row1["matched"])
	}
	if _, ok := row1["normalized"]; ok {
		t.Errorf("row1 must NOT carry normalized key when matched=false: %+v", row1)
	}
	if out["matchedCount"] != 1 {
		t.Errorf("matchedCount: want 1 got %v", out["matchedCount"])
	}
}

// Bucketing: two lines differing only in digit values must collapse to one
// normalized bucket. bucketCount surfaces the operator's primary question
// ("does this set collapse to one row or ten?").
func TestPreviewTool_BucketCollapse(t *testing.T) {
	out := invokePreview(t, map[string]any{
		"lines": []string{
			"ERROR: load 1234 from creatures",
			"ERROR: load 5678 from creatures",
			"ERROR: load 9999 from creatures",
		},
	})
	if out["bucketCount"] != 1 {
		t.Errorf("bucketCount: want 1 got %v (results=%v)", out["bucketCount"], out["results"])
	}
	if out["matchedCount"] != 3 {
		t.Errorf("matchedCount: want 3 got %v", out["matchedCount"])
	}
}

// Custom pattern is honored end-to-end and reported back in the response.
func TestPreviewTool_CustomPattern(t *testing.T) {
	out := invokePreview(t, map[string]any{
		"lines":   []string{"WARN: slow query 500ms", "ERROR: real error"},
		"pattern": "(?i)warn",
	})
	if out["pattern"] != "(?i)warn" {
		t.Errorf("pattern echoed: want (?i)warn got %v", out["pattern"])
	}
	results := out["results"].([]map[string]any)
	if results[0]["matched"] != true {
		t.Errorf("WARN line should match (?i)warn pattern")
	}
	if results[1]["matched"] != false {
		t.Errorf("ERROR line should NOT match (?i)warn pattern")
	}
}

// caseInsensitive flag is additive — adding (?i) when the regex doesn't
// already have it, leaving the regex alone when it does. Matches the
// behavior of ops_log_errors_top via the shared buildErrorsMatcher.
func TestPreviewTool_CaseInsensitiveAdditive(t *testing.T) {
	out := invokePreview(t, map[string]any{
		"lines":           []string{"WARN slow"},
		"pattern":         "warn",
		"caseInsensitive": true,
	})
	results := out["results"].([]map[string]any)
	if results[0]["matched"] != true {
		t.Errorf("caseInsensitive should let WARN match `warn`")
	}
}

// Empty lines array rejected — same shape as oversized rejection so the
// operator can correct in one round trip.
func TestPreviewTool_RejectsEmptyLines(t *testing.T) {
	out := invokePreview(t, map[string]any{"lines": []string{}})
	errStr, ok := out["error"].(string)
	if !ok || !strings.Contains(errStr, "non-empty") {
		t.Errorf("want non-empty rejection, got %v", out["error"])
	}
}

// Oversized array is rejected with a hint pointing at the streaming tool
// (operators who hit this cap are doing the wrong shape of query).
func TestPreviewTool_RejectsOversizedLines(t *testing.T) {
	lines := make([]string, opsLogErrorsTopNormalizePreviewMaxLines+1)
	for i := range lines {
		lines[i] = "x"
	}
	out := invokePreview(t, map[string]any{"lines": lines})
	errStr, ok := out["error"].(string)
	if !ok || !strings.Contains(errStr, "lines too long") {
		t.Errorf("want too-long rejection, got %v", out["error"])
	}
	if !strings.Contains(errStr, "ops_log_errors_top") {
		t.Errorf("error should point at ops_log_errors_top for bulk: %s", errStr)
	}
}

// Bad regex bubbles up — operators tuning a pattern must see why it failed
// before paying for a real container scan.
func TestPreviewTool_RejectsInvalidPattern(t *testing.T) {
	out := invokePreview(t, map[string]any{
		"lines":   []string{"anything"},
		"pattern": "[", // unterminated character class
	})
	errStr, ok := out["error"].(string)
	if !ok || !strings.Contains(errStr, "invalid pattern") {
		t.Errorf("want invalid-pattern rejection, got %v", out["error"])
	}
}

// availableRules: agent-facing catalog must be non-empty and cover the
// 8 rules normalizeLogLine actually applies. Regression guard against
// silent drift.
func TestPreviewTool_AvailableRulesCatalogShape(t *testing.T) {
	out := invokePreview(t, map[string]any{"lines": []string{"anything"}})
	cat, ok := out["availableRules"].([]map[string]string)
	if !ok || len(cat) != 8 {
		t.Fatalf("availableRules: want 8 rules got %T len=%d", out["availableRules"], len(cat))
	}
	want := map[string]bool{
		"iso_timestamp": true,
		"date":          true,
		"time":          true,
		"ipv4_port":     true,
		"uuid":          true,
		"hex_literal":   true,
		"bare_hex":      true,
		"digits":        true,
	}
	for _, c := range cat {
		delete(want, c["rule"])
	}
	if len(want) > 0 {
		t.Errorf("availableRules missing entries: %v", want)
	}
}

// Long input must clip to opsLogErrorsTopMaxNormalizedMsg bytes in BOTH
// `raw` and `normalized` — same cap the main tool uses, so envelope sizes
// stay consistent across the two surfaces.
func TestPreviewTool_ClipsLongInputs(t *testing.T) {
	long := "ERROR " + strings.Repeat("x", opsLogErrorsTopMaxNormalizedMsg*2)
	out := invokePreview(t, map[string]any{"lines": []string{long}})
	row := out["results"].([]map[string]any)[0]
	if got := row["raw"].(string); len(got) > opsLogErrorsTopMaxNormalizedMsg+1 { // +1 for '~' indicator
		t.Errorf("raw not clipped: len=%d cap=%d", len(got), opsLogErrorsTopMaxNormalizedMsg)
	}
	if got, ok := row["normalized"].(string); ok {
		if len(got) > opsLogErrorsTopMaxNormalizedMsg+1 {
			t.Errorf("normalized not clipped: len=%d cap=%d", len(got), opsLogErrorsTopMaxNormalizedMsg)
		}
	}
}

// Description-keyword regression: agent tool-selection latches on these
// keywords to distinguish from sibling tools. Their presence is load-bearing.
func TestPreviewTool_DescriptionMentionsKeyDistinctions(t *testing.T) {
	reg := NewRegistry()
	RegisterOpsLogErrorsTopNormalizePreviewTool(reg)
	tool, ok := reg.Get("ops_log_errors_top_normalize_preview")
	if !ok {
		t.Fatal("ops_log_errors_top_normalize_preview not registered")
	}
	for _, kw := range []string{
		"ops_log_errors_top",
		"normalizer",
		"normalizeLogLine",
		"rules",
		"pattern",
		"window sweep",
		"Pure-function",
		"container_logs_grep",
	} {
		if !strings.Contains(tool.Description, kw) {
			t.Errorf("description missing %q: %s", kw, tool.Description)
		}
	}
}

func TestPreviewTool_IsReadOnly(t *testing.T) {
	reg := NewRegistry()
	RegisterOpsLogErrorsTopNormalizePreviewTool(reg)
	tool, ok := reg.Get("ops_log_errors_top_normalize_preview")
	if !ok {
		t.Fatal("not registered")
	}
	if tool.Destructive {
		t.Error("Destructive: got true want false (pure-function preview)")
	}
	if !strings.Contains(string(tool.Annotations), `"readOnlyHint":true`) {
		t.Errorf("annotations missing readOnlyHint:true: %s", tool.Annotations)
	}
}

// traceCount sums Count for a given rule name across a trace slice.
func traceCount(trace []normalizerRuleTrace, rule string) int {
	n := 0
	for _, t := range trace {
		if t.Rule == rule {
			n += t.Count
		}
	}
	return n
}
