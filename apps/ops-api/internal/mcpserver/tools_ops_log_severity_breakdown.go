package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/synthiq-ai/synthiqbots/apps/ops-api/internal/dockerlog"
)

const (
	// opsLogSeverityBreakdownDefaultSampleLimit keeps a couple of representative
	// raw lines per severity level — enough to recognize what a level is mostly
	// carrying without bloating the envelope (5 levels * 2 ≈ 10 samples max).
	opsLogSeverityBreakdownDefaultSampleLimit = 2
	opsLogSeverityBreakdownMaxSampleLimit     = 10
)

// severityLevel pairs a level label with the RE2 matcher that recognizes it.
type severityLevel struct {
	name string
	re   *regexp.Regexp
}

// opsLogSeverityLadder is the severity classification ladder, ordered MOST
// SEVERE first (index 0 = highest priority). A log line is attributed to the
// FIRST (most-severe) level whose matcher fires, so "WARN: error loading X"
// classifies as `error` (more severe than `warn`) and "FATAL: panic in Y" as
// `panic`. The tokens mirror `ops_log_errors_top`'s default severity filter
// `(?i)(error|fatal|panic|assert)` exactly (case-insensitive substring, same
// precision profile — "errors"/"warning"/"asserting" all match their token) so
// the matched set composes cleanly: with `minLevel:"error"` (warn excluded) this
// tool's matchCount equals `ops_log_errors_top`'s matchCount over the same
// window. `assert` sits at fatal-class (between fatal and error) because an
// assertion failure aborts the worldserver, not a recoverable error. `warn` is
// the lowest rung and is INCLUDED by default — the severity composition is most
// useful when the warn floor is visible ("is it spewing FATALs or just WARNs?").
var opsLogSeverityLadder = []severityLevel{
	{"panic", regexp.MustCompile(`(?i)panic`)},
	{"fatal", regexp.MustCompile(`(?i)fatal`)},
	{"assert", regexp.MustCompile(`(?i)assert`)},
	{"error", regexp.MustCompile(`(?i)error`)},
	{"warn", regexp.MustCompile(`(?i)warn`)},
}

// RegisterOpsLogSeverityBreakdownTool registers `ops_log_severity_breakdown` —
// the severity-COMPOSITION companion to `ops_log_errors_top` (WHAT pattern) and
// `ops_log_errors_histogram` (WHEN). It streams the same container log window
// and classifies each severity line into a fixed ladder (panic > fatal > assert
// > error > warn), returning per-level counts + percentages + a couple of sample
// lines + `dominantLevel` (the level carrying the bulk of the volume).
//
// This is the missing leg of the log-triage trio. `ops_log_errors_top` groups by
// normalized MESSAGE (so the ERROR count is fragmented across many patterns) and
// `ops_log_errors_histogram` groups by TIME (so the severity is collapsed into a
// single matched-count). Neither answers the first triage question — "of the
// matched lines, how many were FATAL vs ERROR vs WARN?" — which is exactly what
// an operator wants in the lead-up to a crash ("did the worldserver escalate
// from WARNs to FATALs before the exit-137 kill, or was it steady-state?").
//
// Docker-SDK based (reads the daemon log stream), so it works against a STOPPED
// container — `docker logs` serves historical lines for exited containers, the
// post-mortem case.
func RegisterOpsLogSeverityBreakdownTool(reg *Registry, deps ContainerDeps) {
	if deps.DefaultContainer == "" {
		deps.DefaultContainer = "ac-worldserver"
	}

	reg.Register(Tool{
		Name: "ops_log_severity_breakdown",
		Description: "Classify a container's log lines into a fixed severity ladder " +
			"(panic > fatal > assert > error > warn) and return the COMPOSITION — " +
			"per-level counts, percentages, a couple of sample lines each, and " +
			"`dominantLevel` (the level carrying the bulk of the volume). The " +
			"severity-split companion to `ops_log_errors_top` (which answers WHAT the " +
			"top error patterns are, grouped by normalized message) and " +
			"`ops_log_errors_histogram` (WHEN errors spiked, grouped by time): neither " +
			"shows the severity mix, so this answers the first triage question — \"of " +
			"the matched lines, how many were FATAL vs ERROR vs WARN?\". Each line is " +
			"attributed to its MOST-SEVERE matching level (\"WARN: error X\" counts as " +
			"error). Use it for \"the ac-worldserver died — was it escalating to FATALs " +
			"or just steady WARNs before the kill?\" post-mortems (Docker-SDK based, so " +
			"it reads historical lines from a STOPPED container), then run " +
			"`ops_log_errors_top` to see the actual messages. `minLevel` floors the " +
			"ladder (default `warn` = everything; `error` excludes warnings and makes " +
			"the matched set identical to `ops_log_errors_top`'s default). Default " +
			"window 1h (max 7d). Bounded by `maxScanLines` (default 200000, max " +
			"1000000). Read-only.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{
			"name":{"type":"string","description":"Container name or id (default ac-worldserver)"},
			"since":{"type":"string","description":"Lookback (\"12h\", \"7d\", \"30m\") or unix seconds (default 1h, max 7d)"},
			"minLevel":{"type":"string","description":"Lowest severity to include: panic|fatal|assert|error|warn (default warn = include all; error excludes warnings)"},
			"sampleLimit":{"type":"integer","description":"Sample lines kept per level (default 2, max 10)"},
			"maxScanLines":{"type":"integer","description":"Cap on lines SCANNED before bailing (default 200000, max 1000000)"},
			"stream":{"type":"string","description":"Restrict to \"stdout\" or \"stderr\" (default both)"}
		}}`),
		Annotations: AnnRead(),
		Handler: func(ctx context.Context, raw json.RawMessage, _ string) any {
			var a struct {
				Name         string `json:"name"`
				Since        string `json:"since"`
				MinLevel     string `json:"minLevel"`
				SampleLimit  int    `json:"sampleLimit"`
				MaxScanLines int    `json:"maxScanLines"`
				Stream       string `json:"stream"`
			}
			_ = json.Unmarshal(raw, &a)

			name := pickContainer(a.Name, deps.DefaultContainer)

			minIdx, err := resolveSeverityFloor(a.MinLevel)
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

			sampleLimit := clampErrorsInt(a.SampleLimit, opsLogSeverityBreakdownDefaultSampleLimit, 1, opsLogSeverityBreakdownMaxSampleLimit)
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

			stats, st := aggregateSeverityBreakdown(ch, opsLogSeverityLadder, minIdx, streamFilter, sampleLimit, maxScan, cancel)

			streamReported := streamFilter
			if streamReported == "" {
				streamReported = "both"
			}

			resp := map[string]any{
				"name":          name,
				"since":         sinceUnix,
				"until":         time.Now().Unix(),
				"stream":        streamReported,
				"minLevel":      opsLogSeverityLadder[minIdx].name,
				"scanned":       st.scanned,
				"matchCount":    st.matchCount,
				"scanTruncated": st.scanTruncated,
				"levels":        severityLevelRows(stats, opsLogSeverityLadder, minIdx, st.matchCount),
			}
			if dom := dominantSeverityLevel(stats, opsLogSeverityLadder, minIdx, st.matchCount); dom != nil {
				resp["dominantLevel"] = dom
			}
			return resp
		},
	})
}

// resolveSeverityFloor maps the `minLevel` arg to its ladder index. Empty →
// the lowest rung (warn) so the default includes every severity. An unknown
// value is an error (echoing the allowlist) rather than a silent default — a
// typo'd `minLevel` would otherwise silently widen the matched set.
func resolveSeverityFloor(arg string) (int, error) {
	v := strings.ToLower(strings.TrimSpace(arg))
	if v == "" {
		return len(opsLogSeverityLadder) - 1, nil
	}
	for i, lv := range opsLogSeverityLadder {
		if lv.name == v {
			return i, nil
		}
	}
	return 0, fmt.Errorf("minLevel must be one of panic|fatal|assert|error|warn")
}

// classifySeverity returns the index of the MOST-SEVERE ladder level whose
// matcher fires on msg, or -1 if the line carries no severity token. The ladder
// is ordered most-severe-first, so the first match is the highest severity.
func classifySeverity(msg string, ladder []severityLevel) int {
	for i, lv := range ladder {
		if lv.re.MatchString(msg) {
			return i
		}
	}
	return -1
}

// severityLevelStat holds the per-level aggregates during a scan. Kept as a
// struct (not map[string]any) so the hot loop avoids per-hit map allocation.
type severityLevelStat struct {
	count   int
	samples []string
	firstTs time.Time
	lastTs  time.Time
}

// aggregateSeverityBreakdown drains the line channel, classifies each line to
// its most-severe level, and tallies per-level counts/samples/timestamps. Lines
// that carry no severity token (classifySeverity == -1) and lines whose level is
// below the `minLevelIdx` floor are skipped (not counted in matchCount). The
// returned stats slice is sized to the FULL ladder; only indices [0, minLevelIdx]
// are ever populated. `cancel` is invoked on scan-cap so the producer goroutine
// exits instead of wedging on a full channel — the same streaming contract as
// `ops_log_errors_top`.
func aggregateSeverityBreakdown(
	lines <-chan dockerlog.Line,
	ladder []severityLevel,
	minLevelIdx int,
	streamFilter string,
	sampleLimit, maxScan int,
	cancel context.CancelFunc,
) ([]severityLevelStat, errorTopStats) {
	stats := make([]severityLevelStat, len(ladder))
	var s errorTopStats

	for ln := range lines {
		s.scanned++
		if s.scanned > maxScan {
			s.scanTruncated = true
			cancel()
			break
		}
		if streamFilter != "" && ln.Stream != streamFilter {
			continue
		}
		idx := classifySeverity(ln.Msg, ladder)
		if idx < 0 || idx > minLevelIdx {
			continue
		}
		s.matchCount++

		st := &stats[idx]
		st.count++
		if len(st.samples) < sampleLimit {
			st.samples = append(st.samples, clipBytes(ln.Msg, opsLogErrorsTopMaxNormalizedMsg))
		}
		if !ln.TS.IsZero() {
			if st.firstTs.IsZero() || ln.TS.Before(st.firstTs) {
				st.firstTs = ln.TS
			}
			if ln.TS.After(st.lastTs) {
				st.lastTs = ln.TS
			}
		}
	}

	return stats, s
}

// severityLevelRows projects the in-range ladder (indices [0, minLevelIdx]) into
// the response shape, ordered MOST SEVERE first. EVERY in-range level is emitted
// including zero-count ones — a zero FATAL count is itself signal ("no fatals,
// good"), the same all-buckets-emitted philosophy as the histogram. `samples` is
// always a non-nil slice; firstTs/lastTs keys are omitted on a level that never
// matched (presence, not zero-value, is the downstream check).
func severityLevelRows(stats []severityLevelStat, ladder []severityLevel, minLevelIdx, matchCount int) []map[string]any {
	out := make([]map[string]any, 0, minLevelIdx+1)
	for i := 0; i <= minLevelIdx; i++ {
		st := stats[i]
		samples := st.samples
		if samples == nil {
			samples = []string{}
		}
		row := map[string]any{
			"level":   ladder[i].name,
			"count":   st.count,
			"pct":     pctOf(st.count, matchCount),
			"samples": samples,
		}
		if !st.firstTs.IsZero() {
			row["firstTs"] = st.firstTs.UTC().Format(time.RFC3339Nano)
		}
		if !st.lastTs.IsZero() {
			row["lastTs"] = st.lastTs.UTC().Format(time.RFC3339Nano)
		}
		out = append(out, row)
	}
	return out
}

// dominantSeverityLevel returns the in-range level carrying the most lines (the
// bulk of the volume), with the MORE-SEVERE level winning ties — iterating the
// severity-ordered ladder ascending with a strict `>` keeps the earliest (most
// severe) index on a tie. Returns nil when nothing was classified so the handler
// omits the key rather than emitting a misleading count-0 dominant level.
func dominantSeverityLevel(stats []severityLevelStat, ladder []severityLevel, minLevelIdx, matchCount int) map[string]any {
	if matchCount <= 0 {
		return nil
	}
	best := -1
	for i := 0; i <= minLevelIdx; i++ {
		if best < 0 || stats[i].count > stats[best].count {
			best = i
		}
	}
	if best < 0 || stats[best].count == 0 {
		return nil
	}
	return map[string]any{
		"level": ladder[best].name,
		"count": stats[best].count,
		"pct":   pctOf(stats[best].count, matchCount),
	}
}

// pctOf is n/total as a percentage rounded to one decimal place. Guards
// total <= 0 (an empty scan) so the division never faults and every pct reads 0.
func pctOf(n, total int) float64 {
	if total <= 0 {
		return 0
	}
	return math.Round(float64(n)/float64(total)*1000) / 10
}
