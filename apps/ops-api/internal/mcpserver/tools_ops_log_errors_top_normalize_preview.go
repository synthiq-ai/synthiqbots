package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

const (
	// opsLogErrorsTopNormalizePreviewMaxLines caps the input array so the
	// "preview the normalizer on a sample" use case can't be repurposed as
	// a bulk-normalize endpoint. 50 is enough to walk a representative
	// slice of an operator's worldserver tail; anything bigger should go
	// through the streaming ops_log_errors_top pipeline.
	opsLogErrorsTopNormalizePreviewMaxLines = 50
)

// RegisterOpsLogErrorsTopNormalizePreviewTool registers
// `ops_log_errors_top_normalize_preview` — the pure-function preview
// companion to `ops_log_errors_top`.
//
// Operator question shape: "if I tune `pattern` to X, will the normalizer
// collapse my inputs the way I expect?" The full ops_log_errors_top sweeps
// a real window (docker SDK + scan-cap) before showing collapsed output,
// which is expensive feedback for tuning. This tool takes raw line(s) +
// the same severity-filter regex and returns just (matched, normalized,
// rules-fired) per line — no window plumbing, no docker stream.
//
// Reuses `normalizeLogLine` and `buildErrorsMatcher` from the main tool
// so any future normalizer-rule edit applies to both surfaces in lockstep.
// Trace is produced by a parallel `normalizeLogLineWithTrace` that mirrors
// the same regex sequence — sanity-checked by `TestNormalizeLogLineWithTrace_MatchesUntracedOutput`
// so the two implementations can't drift silently.
func RegisterOpsLogErrorsTopNormalizePreviewTool(reg *Registry) {
	reg.Register(Tool{
		Name: "ops_log_errors_top_normalize_preview",
		Description: "Preview the `ops_log_errors_top` normalizer on a small batch of raw " +
			"log lines without running a window sweep. Each input line is run through " +
			"the same severity-filter regex (default `(?i)(error|fatal|panic|assert)`) " +
			"and, if matched, through the same `normalizeLogLine` pipeline used by the " +
			"main tool — returning (matched, normalized, rules) per line plus a " +
			"bucket count over the matched set. Use this to tune a custom `pattern` " +
			"or to debug \"normalizer is collapsing distinct errors\" / \"normalizer " +
			"isn't collapsing this counter\" feedback before paying the cost of a " +
			"real container scan. Companion to ops_log_errors_top (preview shape) and " +
			"a quieter alternative to container_logs_grep for ad-hoc string checks. " +
			"Pure-function — no docker SDK, no DB. `lines` array required (1..50). " +
			"Read-only.",
		InputSchema: json.RawMessage(`{"type":"object","required":["lines"],"properties":{
			"lines":{"type":"array","minItems":1,"maxItems":50,"items":{"type":"string"},"description":"Raw log lines to preview (1..50)"},
			"pattern":{"type":"string","description":"RE2 severity filter (default \"(?i)(error|fatal|panic|assert)\"). Lines that don't match are returned with matched=false and no normalized form."},
			"caseInsensitive":{"type":"boolean","description":"Fold case before matching (default false; the default pattern already includes (?i))"}
		}}`),
		Annotations: AnnRead(),
		Handler: func(_ context.Context, raw json.RawMessage, _ string) any {
			var a struct {
				Lines           []string `json:"lines"`
				Pattern         string   `json:"pattern"`
				CaseInsensitive bool     `json:"caseInsensitive"`
			}
			_ = json.Unmarshal(raw, &a)

			if len(a.Lines) == 0 {
				return map[string]any{"error": "lines must be a non-empty array"}
			}
			if len(a.Lines) > opsLogErrorsTopNormalizePreviewMaxLines {
				return map[string]any{
					"error": fmt.Sprintf("lines too long: %d > %d (use ops_log_errors_top for bulk windows)",
						len(a.Lines), opsLogErrorsTopNormalizePreviewMaxLines),
				}
			}

			pattern := strings.TrimSpace(a.Pattern)
			if pattern == "" {
				pattern = opsLogErrorsTopDefaultPattern
			}
			matcher, err := buildErrorsMatcher(pattern, a.CaseInsensitive)
			if err != nil {
				return map[string]any{"error": err.Error()}
			}

			results := make([]map[string]any, 0, len(a.Lines))
			buckets := make(map[string]struct{})
			matchedCount := 0
			for _, line := range a.Lines {
				row := map[string]any{
					"raw":     clipBytes(line, opsLogErrorsTopMaxNormalizedMsg),
					"matched": false,
				}
				if !matcher(line) {
					results = append(results, row)
					continue
				}
				matchedCount++
				normalized, trace := normalizeLogLineWithTrace(line)
				buckets[normalized] = struct{}{}
				row["matched"] = true
				row["normalized"] = clipBytes(normalized, opsLogErrorsTopMaxNormalizedMsg)
				row["rules"] = trace
				results = append(results, row)
			}

			return map[string]any{
				"pattern":         pattern,
				"caseInsensitive": a.CaseInsensitive,
				"lineCount":       len(a.Lines),
				"matchedCount":    matchedCount,
				"bucketCount":     len(buckets),
				"availableRules":  availableNormalizerRules(),
				"results":         results,
			}
		},
	})
}

// normalizerRuleTrace records one rule's contribution to a normalization.
// Emitted in the order the rules fire (mirrors the sequence in
// normalizeLogLine) so an operator can read the trace as "first ts collapsed,
// then ipv4_port, then digits".
type normalizerRuleTrace struct {
	Placeholder string `json:"placeholder"`
	Rule        string `json:"rule"`
	Count       int    `json:"count"`
}

// normalizerRuleSpec pairs a regex with the placeholder it emits and the
// stable rule name surfaced in the trace. Order is load-bearing — it must
// match the sequence in normalizeLogLine exactly, otherwise the trace
// describes a normalization the main tool doesn't perform.
type normalizerRuleSpec struct {
	re          *regexp.Regexp
	placeholder string
	rule        string
}

// normalizerRules is the single source of truth for what the trace surfaces
// AND the order the preview applies them. Declared as a package-level var
// (not a function-scope literal) so the parallel test that asserts
// preview output == normalizeLogLine output can verify the sequence
// without re-listing it.
var normalizerRules = []normalizerRuleSpec{
	{reErrIsoTimestamp, "<ts>", "iso_timestamp"},
	{reErrDate, "<date>", "date"},
	{reErrTime, "<time>", "time"},
	{reErrIPv4, "<ip>", "ipv4_port"},
	{reErrUUID, "<uuid>", "uuid"},
	{reErrHexLiteral, "<hex>", "hex_literal"},
	{reErrBareHex, "<hex>", "bare_hex"},
	{reErrDigits, "#", "digits"},
}

// normalizeLogLineWithTrace runs the same regex sequence as normalizeLogLine
// but records how many times each rule fired. It counts matches BEFORE the
// replacement (via FindAllStringIndex) so a rule whose pattern would have
// re-matched the replacement placeholder doesn't get double-counted.
//
// The returned `normalized` string MUST equal `normalizeLogLine(msg)` —
// verified by TestNormalizeLogLineWithTrace_MatchesUntracedOutput. If
// either function is edited the test fails and forces the other to follow.
func normalizeLogLineWithTrace(msg string) (string, []normalizerRuleTrace) {
	s := msg
	trace := make([]normalizerRuleTrace, 0, len(normalizerRules))
	for _, rule := range normalizerRules {
		matches := rule.re.FindAllStringIndex(s, -1)
		if len(matches) > 0 {
			trace = append(trace, normalizerRuleTrace{
				Placeholder: rule.placeholder,
				Rule:        rule.rule,
				Count:       len(matches),
			})
		}
		s = rule.re.ReplaceAllString(s, rule.placeholder)
	}
	s = reErrWhitespace.ReplaceAllString(s, " ")
	return strings.TrimSpace(s), trace
}

// availableNormalizerRules surfaces the rule catalog in tool responses so
// operators can see what placeholders the normalizer can emit without
// trial-and-error.
func availableNormalizerRules() []map[string]string {
	out := make([]map[string]string, 0, len(normalizerRules))
	for _, rule := range normalizerRules {
		out = append(out, map[string]string{
			"placeholder": rule.placeholder,
			"rule":        rule.rule,
		})
	}
	return out
}
