package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/synthiq-ai/synthiqbots/apps/ops-api/internal/dockerlog"
)

const (
	// opsLogErrorsTopDefaultSince mirrors container_logs_grep's 1h default. The
	// typical operator question shape ("what errors are happening?") collapses
	// to "right now"; multi-hour sweeps come with an explicit `since`.
	opsLogErrorsTopDefaultSince = "1h"

	// opsLogErrorsTopMaxWindow matches the json-file driver retention reality
	// on a busy ac-worldserver (~5d on disk, 7d ceiling above which the daemon
	// hits rotation boundaries anyway). Same cap as the sibling grep tools so
	// callers can chain them with consistent window semantics.
	opsLogErrorsTopMaxWindow = 7 * 24 * time.Hour

	// opsLogErrorsTopDefaultMaxScan keeps a 12h crash-loop sweep under ~10s
	// wall clock. Same as the sibling grep tools.
	opsLogErrorsTopDefaultMaxScan = 200_000
	opsLogErrorsTopHardMaxScan    = 1_000_000

	// opsLogErrorsTopDefaultTopN is the typical "top errors" cardinality an
	// operator scans visually. Hard cap 50 because top-100 is almost always
	// either noise or the operator wanting a different question shape
	// (per-pattern drill-down via container_logs_grep).
	opsLogErrorsTopDefaultTopN = 10
	opsLogErrorsTopMaxTopN     = 50

	// opsLogErrorsTopDefaultSampleLimit balances "show me the operator one
	// representative example" against MCP envelope weight. 3 samples covers
	// the cases where normalization collapses two genuinely different lines
	// into one bucket (rare but possible when the variable parts differ
	// only in non-normalized punctuation).
	opsLogErrorsTopDefaultSampleLimit = 3
	opsLogErrorsTopMaxSampleLimit     = 10

	// opsLogErrorsTopDefaultPattern is the severity filter applied when the
	// caller passes no `pattern`. Matches the muscle-memory shape of
	// `grep -iE 'error|fatal|panic|assert'` over ac-worldserver logs — the
	// four severity tokens worldserver actually emits. `assert` covers both
	// "ASSERT" macro output and "Assertion failed" lines from ACE/Boost.
	opsLogErrorsTopDefaultPattern = `(?i)(error|fatal|panic|assert)`

	// opsLogErrorsTopMaxNormalizedMsg caps each emitted normalized/sample
	// string at this many bytes. Worldserver SQL-error lines can be 2-4 KB
	// long (full statement echoed); a 200-byte clip is enough to recognize
	// the pattern without bloating the topN payload.
	opsLogErrorsTopMaxNormalizedMsg = 200
)

// RegisterOpsLogErrorsTopTool registers `ops_log_errors_top` — aggregated
// top-N error patterns from a container's stdout/stderr.
//
// The operator question this answers: "what are the top error patterns in the
// last N hours?" — the discovery-mode complement to `container_logs_grep`
// (which needs a known pattern up front). Lines passing the severity filter
// are run through a normalizer that collapses variable parts (timestamps,
// hex addresses, UUIDs, IPs, digit runs) into placeholders, then the
// normalized forms are counted and the top-N returned with per-pattern
// counts, sample messages, and first/last timestamps.
//
// Distinct from `container_logs_grep` (cheap match-only on a known pattern,
// no aggregation) and from `ops_logs_tail` (5000-line cap before filter,
// no normalization).
func RegisterOpsLogErrorsTopTool(reg *Registry, deps ContainerDeps) {
	if deps.DefaultContainer == "" {
		deps.DefaultContainer = "ac-worldserver"
	}

	reg.Register(Tool{
		Name: "ops_log_errors_top",
		Description: "Aggregate top-N error patterns from a container's stdout/stderr across a " +
			"multi-hour window. Lines passing the severity filter (default RE2 " +
			"`(?i)(error|fatal|panic|assert)`) are run through a normalizer that " +
			"collapses variable parts (timestamps, hex addresses, UUIDs, IPv4+port, " +
			"digit runs) into placeholders, then the normalized forms are counted " +
			"and the top-N returned with per-pattern counts, sample messages, and " +
			"first/last timestamps. Discovery-mode complement to `container_logs_grep` " +
			"(which needs a known pattern up front) and `ops_logs_tail` (5000-line " +
			"cap before filter, no normalization). Use this for \"what's broken on " +
			"ac-worldserver right now?\" investigations where you don't yet know what " +
			"pattern to search for. Override `pattern` with a custom RE2 to scope to a " +
			"non-default severity (e.g. `(?i)warn` for warnings). Default window 1h, " +
			"max 7d. `topN` default 10 (max 50), `sampleLimit` default 3 (max 10). " +
			"Bounded by `maxScanLines` (default 200000, max 1000000). Read-only.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{
			"name":{"type":"string","description":"Container name or id (default ac-worldserver)"},
			"pattern":{"type":"string","description":"RE2 severity filter (default \"(?i)(error|fatal|panic|assert)\")"},
			"caseInsensitive":{"type":"boolean","description":"Fold case before matching (default false; the default pattern already includes (?i))"},
			"since":{"type":"string","description":"Lookback (\"12h\", \"7d\", \"30m\") or unix seconds (default 1h, max 7d)"},
			"topN":{"type":"integer","description":"Number of distinct patterns to return (default 10, max 50)"},
			"sampleLimit":{"type":"integer","description":"Sample messages kept per pattern (default 3, max 10)"},
			"maxScanLines":{"type":"integer","description":"Cap on lines SCANNED before bailing (default 200000, max 1000000)"},
			"stream":{"type":"string","description":"Restrict to \"stdout\" or \"stderr\" (default both)"}
		}}`),
		Annotations: AnnRead(),
		Handler: func(ctx context.Context, raw json.RawMessage, _ string) any {
			var a struct {
				Name            string `json:"name"`
				Pattern         string `json:"pattern"`
				CaseInsensitive bool   `json:"caseInsensitive"`
				Since           string `json:"since"`
				TopN            int    `json:"topN"`
				SampleLimit     int    `json:"sampleLimit"`
				MaxScanLines    int    `json:"maxScanLines"`
				Stream          string `json:"stream"`
			}
			_ = json.Unmarshal(raw, &a)

			name := pickContainer(a.Name, deps.DefaultContainer)

			pattern := strings.TrimSpace(a.Pattern)
			if pattern == "" {
				pattern = opsLogErrorsTopDefaultPattern
			}
			matcher, err := buildErrorsMatcher(pattern, a.CaseInsensitive)
			if err != nil {
				return map[string]any{"error": err.Error(), "name": name}
			}

			sinceUnix, err := resolveErrorsSince(a.Since)
			if err != nil {
				return map[string]any{"error": "since: " + err.Error(), "name": name}
			}

			streamFilter, err := normalizeErrorsStreamFilter(a.Stream)
			if err != nil {
				return map[string]any{"error": err.Error(), "name": name}
			}

			topN := clampErrorsInt(a.TopN, opsLogErrorsTopDefaultTopN, 1, opsLogErrorsTopMaxTopN)
			sampleLimit := clampErrorsInt(a.SampleLimit, opsLogErrorsTopDefaultSampleLimit, 1, opsLogErrorsTopMaxSampleLimit)
			maxScan := clampErrorsInt(a.MaxScanLines, opsLogErrorsTopDefaultMaxScan, 1, opsLogErrorsTopHardMaxScan)

			scanCtx, cancel := context.WithCancel(ctx)
			defer cancel()

			ch, _, err := deps.Docker.Stream(scanCtx, name, dockerlog.LogOpts{
				Tail:  "all",
				Since: strconv.FormatInt(sinceUnix, 10),
			})
			if err != nil {
				return map[string]any{"error": err.Error(), "name": name}
			}

			patterns, stats := aggregateErrorPatterns(ch, matcher, streamFilter, sampleLimit, maxScan, cancel)
			result := topErrorPatterns(patterns, topN)

			streamReported := streamFilter
			if streamReported == "" {
				streamReported = "both"
			}

			return map[string]any{
				"name":             name,
				"since":            sinceUnix,
				"until":            time.Now().Unix(),
				"pattern":          pattern,
				"caseInsensitive":  a.CaseInsensitive,
				"stream":           streamReported,
				"scanned":          stats.scanned,
				"matchCount":       stats.matchCount,
				"normalizedCount":  len(patterns),
				"scanTruncated":    stats.scanTruncated,
				"topN":             topN,
				"sampleLimit":      sampleLimit,
				"patterns":         result,
			}
		},
	})
}

// errorPatternStat holds the per-normalized-form aggregates during a scan.
// Kept as a struct (not map[string]any) so the aggregation loop avoids
// repeated map allocation per hit on hot worldserver windows.
type errorPatternStat struct {
	normalized string
	count      int
	samples    []string
	firstTs    time.Time
	lastTs     time.Time
}

// errorTopStats accounts for what the aggregator saw vs. what it returned.
type errorTopStats struct {
	scanned       int
	matchCount    int
	scanTruncated bool
}

// aggregateErrorPatterns drains the line channel, normalizes each line that
// passes the matcher + stream filter, and tallies per-normalized counts.
// `cancel` is invoked on scan-cap so the producer goroutine exits instead
// of wedging on a full channel after we stop draining.
func aggregateErrorPatterns(
	lines <-chan dockerlog.Line,
	matcher func(string) bool,
	streamFilter string,
	sampleLimit, maxScan int,
	cancel context.CancelFunc,
) (map[string]*errorPatternStat, errorTopStats) {
	patterns := make(map[string]*errorPatternStat)
	stats := errorTopStats{}

	for ln := range lines {
		stats.scanned++
		if stats.scanned > maxScan {
			stats.scanTruncated = true
			cancel()
			break
		}
		if streamFilter != "" && ln.Stream != streamFilter {
			continue
		}
		if !matcher(ln.Msg) {
			continue
		}
		stats.matchCount++

		norm := normalizeLogLine(ln.Msg)
		entry, ok := patterns[norm]
		if !ok {
			entry = &errorPatternStat{normalized: norm}
			patterns[norm] = entry
		}
		entry.count++
		if len(entry.samples) < sampleLimit {
			entry.samples = append(entry.samples, clipBytes(ln.Msg, opsLogErrorsTopMaxNormalizedMsg))
		}
		if !ln.TS.IsZero() {
			if entry.firstTs.IsZero() || ln.TS.Before(entry.firstTs) {
				entry.firstTs = ln.TS
			}
			if ln.TS.After(entry.lastTs) {
				entry.lastTs = ln.TS
			}
		}
	}

	return patterns, stats
}

// topErrorPatterns sorts the aggregated patterns by count desc with
// normalized-asc as the deterministic tiebreaker (without it Go map
// iteration randomization would make tests flaky on count ties), then
// truncates to topN and projects to the response shape.
func topErrorPatterns(patterns map[string]*errorPatternStat, topN int) []map[string]any {
	if len(patterns) == 0 {
		return []map[string]any{}
	}
	keys := make([]string, 0, len(patterns))
	for k := range patterns {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		ki, kj := keys[i], keys[j]
		if patterns[ki].count != patterns[kj].count {
			return patterns[ki].count > patterns[kj].count
		}
		return ki < kj
	})
	if len(keys) > topN {
		keys = keys[:topN]
	}
	out := make([]map[string]any, 0, len(keys))
	for _, k := range keys {
		p := patterns[k]
		row := map[string]any{
			"normalized": clipBytes(p.normalized, opsLogErrorsTopMaxNormalizedMsg),
			"count":      p.count,
			"samples":    p.samples,
		}
		if !p.firstTs.IsZero() {
			row["firstTs"] = p.firstTs.UTC().Format(time.RFC3339Nano)
		}
		if !p.lastTs.IsZero() {
			row["lastTs"] = p.lastTs.UTC().Format(time.RFC3339Nano)
		}
		out = append(out, row)
	}
	return out
}

// Normalizer regexes are package-level so they compile once at init rather
// than per-call (the hot worldserver path tallies thousands of lines).
var (
	// ISO date+time pair, with or without 'T' separator and optional fractional
	// seconds — covers worldserver bracket-prefixes like "2026-05-29 12:34:56".
	reErrIsoTimestamp = regexp.MustCompile(`\b\d{4}-\d{2}-\d{2}[T ]\d{2}:\d{2}:\d{2}(\.\d+)?(Z|[+-]\d{2}:?\d{2})?\b`)
	// Standalone ISO date.
	reErrDate = regexp.MustCompile(`\b\d{4}-\d{2}-\d{2}\b`)
	// Standalone HH:MM:SS time.
	reErrTime = regexp.MustCompile(`\b\d{2}:\d{2}:\d{2}(\.\d+)?\b`)
	// IPv4 with optional :port suffix.
	reErrIPv4 = regexp.MustCompile(`\b\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}(:\d{1,5})?\b`)
	// Canonical UUID / GUID (8-4-4-4-12 hex). Worldserver GUIDs aren't this
	// shape but extension modules + monitoring tooling emit canonical UUIDs.
	reErrUUID = regexp.MustCompile(`\b[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}\b`)
	// Hex literals (0x-prefixed). Worldserver stack frames + AC pointer dumps.
	reErrHexLiteral = regexp.MustCompile(`\b0[xX][0-9a-fA-F]+\b`)
	// Long bare hex tokens (8+ chars, all hex) without the 0x prefix —
	// catches GUID-style identifiers worldserver emits inline. Must run
	// AFTER digit-runs would, but BEFORE — handled by ordering in the
	// normalize function. Eight chars is the threshold below which we'd
	// false-positive on decimal counters (a 4-digit number is hex too).
	reErrBareHex = regexp.MustCompile(`\b[0-9a-fA-F]{8,}\b`)
	// Digit runs (1+ chars). Replace with `#` to collapse counters, GUIDs,
	// line numbers. Must run last among value-collapsing patterns so we
	// don't eat digits inside the longer-form regexes above.
	reErrDigits = regexp.MustCompile(`\d+`)
	// Whitespace runs (final pass, to coalesce consecutive replacements).
	reErrWhitespace = regexp.MustCompile(`\s+`)
)

// normalizeLogLine collapses variable parts of a log line so that
// "ERROR: failed to load 1234 from creatures" and
// "ERROR: failed to load 5678 from creatures" hash to the same bucket.
//
// Order matters: composite multi-field patterns (timestamps, IPs) run before
// single-field patterns (bare hex, digit runs) so we don't eat the inner
// parts of a composite. The result is space-collapsed and trimmed so
// rare-whitespace-only variation doesn't fragment buckets.
func normalizeLogLine(msg string) string {
	s := msg
	s = reErrIsoTimestamp.ReplaceAllString(s, "<ts>")
	s = reErrDate.ReplaceAllString(s, "<date>")
	s = reErrTime.ReplaceAllString(s, "<time>")
	s = reErrIPv4.ReplaceAllString(s, "<ip>")
	s = reErrUUID.ReplaceAllString(s, "<uuid>")
	s = reErrHexLiteral.ReplaceAllString(s, "<hex>")
	s = reErrBareHex.ReplaceAllString(s, "<hex>")
	s = reErrDigits.ReplaceAllString(s, "#")
	s = reErrWhitespace.ReplaceAllString(s, " ")
	return strings.TrimSpace(s)
}

// buildErrorsMatcher compiles the severity-filter regex. The default pattern
// already carries `(?i)`, but the explicit `caseInsensitive` arg is folded
// in additively so an operator-supplied case-sensitive regex can still be
// flipped via a single flag.
func buildErrorsMatcher(pattern string, caseInsensitive bool) (func(string) bool, error) {
	expr := pattern
	if caseInsensitive && !strings.HasPrefix(expr, "(?i)") {
		expr = "(?i)" + expr
	}
	re, err := regexp.Compile(expr)
	if err != nil {
		return nil, fmt.Errorf("invalid pattern: %w", err)
	}
	return re.MatchString, nil
}

// resolveErrorsSince parses the user-supplied lookback. Same parser as
// container_logs_grep — Go duration, the Nd days extension shared with
// container_events, or a unix-seconds epoch — with the same 7d cap.
func resolveErrorsSince(arg string) (int64, error) {
	in := strings.TrimSpace(arg)
	if in == "" {
		in = opsLogErrorsTopDefaultSince
	}
	now := time.Now()
	maxLookback := now.Add(-opsLogErrorsTopMaxWindow).Unix()

	if d, err := time.ParseDuration(in); err == nil {
		if d < 0 {
			return 0, fmt.Errorf("negative duration %q", arg)
		}
		if d > opsLogErrorsTopMaxWindow {
			d = opsLogErrorsTopMaxWindow
		}
		return now.Add(-d).Unix(), nil
	}
	if d, err := parseDurationDays(in); err == nil {
		if d < 0 {
			return 0, fmt.Errorf("negative duration %q", arg)
		}
		if d > opsLogErrorsTopMaxWindow {
			d = opsLogErrorsTopMaxWindow
		}
		return now.Add(-d).Unix(), nil
	}
	if v, err := strconv.ParseInt(in, 10, 64); err == nil {
		if v > now.Unix() {
			return 0, fmt.Errorf("future timestamp %d", v)
		}
		if v < maxLookback {
			v = maxLookback
		}
		return v, nil
	}
	return 0, fmt.Errorf("invalid since %q", arg)
}

func normalizeErrorsStreamFilter(arg string) (string, error) {
	v := strings.ToLower(strings.TrimSpace(arg))
	switch v {
	case "", "both":
		return "", nil
	case "stdout", "stderr":
		return v, nil
	default:
		return "", fmt.Errorf("stream must be stdout|stderr|both")
	}
}

func clampErrorsInt(v, def, lo, hi int) int {
	if v <= 0 {
		return def
	}
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// clipBytes trims a string to at most n bytes, appending a single-byte ellipsis
// indicator if truncation occurred. Single-byte (ASCII '~') keeps the byte
// budget honest even when the underlying string is multi-byte UTF-8 — important
// because the cap exists to bound MCP envelope size, not character count.
// We slice on a byte boundary; if it lands mid-rune the trailing bytes are
// dropped along with the rune (safe — the result is still valid since we
// don't preserve a partial rune).
func clipBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	// Find the highest valid-rune boundary <= n to avoid emitting a
	// half-decoded sequence; back up until we find a non-continuation byte.
	cut := n
	for cut > 0 && (s[cut]&0xC0) == 0x80 {
		cut--
	}
	return s[:cut] + "~"
}
