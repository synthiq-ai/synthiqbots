package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/synthiq-ai/synthiqbots/apps/ops-api/internal/dockerlog"
)

const (
	// opsLogErrorsHistogramDefaultBucket gives a 12-bucket curve over the
	// default 1h window — fine enough to see a spike, coarse enough that the
	// envelope stays tiny. Wider windows pass a wider `bucket` (or accept the
	// auto-coarsening below).
	opsLogErrorsHistogramDefaultBucket = "5m"

	// opsLogErrorsHistogramMinBucket floors the bucket width. Sub-minute
	// buckets over any meaningful window blow past the bucket-count cap anyway
	// and rarely add signal for log-rate triage.
	opsLogErrorsHistogramMinBucket = time.Minute

	// opsLogErrorsHistogramMaxBuckets caps the emitted bucket array so the
	// response envelope stays bounded (~200 rows * ~75 bytes ≈ 15 KB) regardless
	// of window/bucket combination. When a request would exceed this, the bucket
	// width is COARSENED up to fit (reported via bucketWidthAdjusted) rather than
	// erroring — matches the clamp-don't-error philosophy of the sibling tools.
	opsLogErrorsHistogramMaxBuckets = 200
)

// RegisterOpsLogErrorsHistogramTool registers `ops_log_errors_histogram` —
// the time-bucketed companion to `ops_log_errors_top`.
//
// `ops_log_errors_top` answers "WHAT are the top error patterns?" (bucketed by
// normalized pattern). This answers "WHEN did errors spike?" (bucketed by time):
// it streams the same severity-filtered window, then tallies each matching line
// into a fixed-width time bucket so the operator sees the error-rate curve and
// the single hottest interval (`peakBucket`). The "collapse N sequential calls
// into one" / "error rate over time" wedge — the operator's typical follow-up
// during a recurring outage is "the worldserver died; when did it START
// erroring?", which a top-N-by-pattern view structurally can't show.
//
// Docker-SDK based (reads the daemon log stream), so it works against a STOPPED
// container — `docker logs` serves historical lines for exited containers, which
// is exactly the post-mortem case ("what was the error rate leading up to the
// crash?").
func RegisterOpsLogErrorsHistogramTool(reg *Registry, deps ContainerDeps) {
	if deps.DefaultContainer == "" {
		deps.DefaultContainer = "ac-worldserver"
	}

	reg.Register(Tool{
		Name: "ops_log_errors_histogram",
		Description: "Bucket error-log lines into fixed-width TIME intervals to show the error " +
			"RATE over a window — the time-bucketed companion to `ops_log_errors_top`. " +
			"Where `ops_log_errors_top` answers \"WHAT are the top error patterns?\" " +
			"(grouped by normalized pattern), this answers \"WHEN did errors spike?\": " +
			"lines passing the severity filter (default RE2 `(?i)(error|fatal|panic|assert)`) " +
			"are counted per time bucket and returned as a histogram plus `peakBucket` " +
			"(the single hottest interval). Use this for \"the ac-worldserver died — when " +
			"did it START erroring?\" post-mortems (Docker-SDK based, so it reads historical " +
			"lines from a STOPPED container), then run `ops_log_errors_top` scoped to the " +
			"spike window to see what the errors were, or `container_logs_grep` to read the " +
			"raw lines. Default window 1h (max 7d), default bucket 5m (min 1m); the bucket " +
			"width is auto-coarsened so the histogram never exceeds 200 buckets " +
			"(bucketWidthAdjusted flags it). `undated` counts matched lines the daemon emitted " +
			"with no timestamp (can't be placed in a bucket). Bounded by `maxScanLines` " +
			"(default 200000, max 1000000). Read-only.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{
			"name":{"type":"string","description":"Container name or id (default ac-worldserver)"},
			"pattern":{"type":"string","description":"RE2 severity filter (default \"(?i)(error|fatal|panic|assert)\")"},
			"caseInsensitive":{"type":"boolean","description":"Fold case before matching (default false; the default pattern already includes (?i))"},
			"since":{"type":"string","description":"Lookback (\"12h\", \"7d\", \"30m\") or unix seconds (default 1h, max 7d)"},
			"bucket":{"type":"string","description":"Time-bucket width (\"5m\", \"1h\", \"30s\"→clamped to 1m, \"1d\"); default 5m, auto-coarsened to keep <=200 buckets"},
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
				Bucket          string `json:"bucket"`
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

			maxScan := clampErrorsInt(a.MaxScanLines, opsLogErrorsTopDefaultMaxScan, 1, opsLogErrorsTopHardMaxScan)

			until := time.Now().Unix()
			effWidth, reqWidth, numBuckets, err := planHistogramBuckets(sinceUnix, until, a.Bucket)
			if err != nil {
				return map[string]any{"error": err.Error(), "name": name}
			}

			scanCtx, cancel := context.WithCancel(ctx)
			defer cancel()

			ch, _, err := deps.Docker.Stream(scanCtx, name, dockerlog.LogOpts{
				Tail:  "all",
				Since: strconv.FormatInt(sinceUnix, 10),
			})
			if err != nil {
				return map[string]any{"error": err.Error(), "name": name}
			}

			counts, undated, stats := aggregateErrorHistogram(ch, matcher, streamFilter, sinceUnix, effWidth, numBuckets, maxScan, cancel)

			streamReported := streamFilter
			if streamReported == "" {
				streamReported = "both"
			}

			resp := map[string]any{
				"name":                name,
				"since":               sinceUnix,
				"until":               until,
				"pattern":             pattern,
				"caseInsensitive":     a.CaseInsensitive,
				"stream":              streamReported,
				"bucketSeconds":       effWidth,
				"bucketCount":         numBuckets,
				"bucketWidthAdjusted": effWidth != reqWidth,
				"scanned":             stats.scanned,
				"matchCount":          stats.matchCount,
				"placed":              stats.matchCount - undated,
				"undated":             undated,
				"scanTruncated":       stats.scanTruncated,
				"buckets":             histogramBuckets(counts, sinceUnix, effWidth, until, numBuckets),
			}
			if peak := peakHistogramBucket(counts, sinceUnix, effWidth, until); peak != nil {
				resp["peakBucket"] = peak
			}
			return resp
		},
	})
}

// resolveHistogramBucket parses the user-supplied bucket width into whole
// seconds. Accepts a Go duration ("5m", "1h", "30s") or the "Nd" days extension
// shared with container_events. Sub-minute widths are floored at 1m; a
// non-positive or unparseable value is an error (silently substituting the
// default would mask a typo'd bucket arg and silently change the resolution).
func resolveHistogramBucket(arg string) (int64, error) {
	in := strings.TrimSpace(arg)
	if in == "" {
		in = opsLogErrorsHistogramDefaultBucket
	}
	var d time.Duration
	if dd, err := time.ParseDuration(in); err == nil {
		d = dd
	} else if dd, err := parseDurationDays(in); err == nil {
		d = dd
	} else {
		return 0, fmt.Errorf("invalid bucket %q (use \"5m\", \"1h\", \"30s\", \"1d\")", arg)
	}
	if d <= 0 {
		return 0, fmt.Errorf("bucket must be positive, got %q", arg)
	}
	if d < opsLogErrorsHistogramMinBucket {
		d = opsLogErrorsHistogramMinBucket
	}
	return int64(d.Seconds()), nil
}

// planHistogramBuckets resolves the effective bucket width + bucket count for a
// [since, until] window. It returns the EFFECTIVE width (after the min-bucket
// floor, the >window clamp, and the maxBuckets coarsening) alongside the
// REQUESTED width so the caller can flag whether coarsening happened. The two
// adjustments:
//   - a bucket wider than the whole window collapses to a single full-window
//     bucket (effWidth = windowSec);
//   - a bucket fine enough to produce >maxBuckets buckets is coarsened up to the
//     smallest width that fits maxBuckets.
//
// widthSec is guaranteed >= 1 and numBuckets >= 1 so the aggregator's integer
// division never faults.
func planHistogramBuckets(since, until int64, bucketArg string) (effWidth, reqWidth int64, numBuckets int, err error) {
	reqWidth, err = resolveHistogramBucket(bucketArg)
	if err != nil {
		return 0, 0, 0, err
	}
	effWidth = reqWidth

	windowSec := until - since
	if windowSec <= 0 {
		// Degenerate window (since == until, or a clock skew) — one bucket.
		return effWidth, reqWidth, 1, nil
	}
	if effWidth > windowSec {
		effWidth = windowSec
	}
	numBuckets = int(ceilDivInt64(windowSec, effWidth))
	if numBuckets > opsLogErrorsHistogramMaxBuckets {
		effWidth = ceilDivInt64(windowSec, int64(opsLogErrorsHistogramMaxBuckets))
		numBuckets = int(ceilDivInt64(windowSec, effWidth))
		if numBuckets > opsLogErrorsHistogramMaxBuckets {
			numBuckets = opsLogErrorsHistogramMaxBuckets // guard integer-rounding overrun
		}
	}
	if numBuckets < 1 {
		numBuckets = 1
	}
	return effWidth, reqWidth, numBuckets, nil
}

// ceilDivInt64 is ceil(a/b) for positive ints. b is always >= 1 at call sites.
func ceilDivInt64(a, b int64) int64 {
	if b <= 0 {
		return a
	}
	return (a + b - 1) / b
}

// aggregateErrorHistogram drains the line channel, applies the same stream
// filter + severity matcher as ops_log_errors_top, and tallies each matching
// line into its time bucket. Lines whose timestamp falls outside [since, until]
// are clamped into the edge buckets (the daemon Since filter is best-effort, and
// TTY-mode containers don't honor it precisely). Lines with no timestamp can't
// be placed and are counted in `undated` instead of fabricating a bucket —
// graceful-degradation signal: undated ≈ matchCount means the log driver isn't
// emitting timestamps. `cancel` is invoked on scan-cap so the producer goroutine
// exits instead of wedging on a full channel.
func aggregateErrorHistogram(
	lines <-chan dockerlog.Line,
	matcher func(string) bool,
	streamFilter string,
	since, widthSec int64,
	numBuckets, maxScan int,
	cancel context.CancelFunc,
) (counts []int, undated int, stats errorTopStats) {
	counts = make([]int, numBuckets)

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

		if ln.TS.IsZero() {
			undated++
			continue
		}
		idx := int((ln.TS.Unix() - since) / widthSec)
		if idx < 0 {
			idx = 0
		}
		if idx >= numBuckets {
			idx = numBuckets - 1
		}
		counts[idx]++
	}

	return counts, undated, stats
}

// histogramBuckets projects the per-index counts into the response shape. Every
// bucket (including zero-count ones) is emitted so the rate curve is contiguous
// — a quiet interval between two spikes is itself signal. The final bucket's
// endTs is clamped to `until` so it doesn't advertise a window past "now".
// Always returns a non-nil slice (callers iterate without a nil check).
func histogramBuckets(counts []int, since, widthSec, until int64, numBuckets int) []map[string]any {
	out := make([]map[string]any, 0, numBuckets)
	for i := 0; i < numBuckets; i++ {
		start := since + int64(i)*widthSec
		end := start + widthSec
		if end > until {
			end = until
		}
		out = append(out, map[string]any{
			"startTs": time.Unix(start, 0).UTC().Format(time.RFC3339),
			"endTs":   time.Unix(end, 0).UTC().Format(time.RFC3339),
			"count":   counts[i],
		})
	}
	return out
}

// peakHistogramBucket returns the single hottest bucket (highest count), with
// the earliest index winning ties (strict `>` so the first max is kept) for
// determinism. Returns nil when nothing was placed (no matched+dated lines) so
// the handler omits the key rather than emitting a misleading count-0 peak.
func peakHistogramBucket(counts []int, since, widthSec, until int64) map[string]any {
	peakIdx, peakCount := -1, 0
	for i, c := range counts {
		if c > peakCount {
			peakCount, peakIdx = c, i
		}
	}
	if peakIdx < 0 {
		return nil
	}
	start := since + int64(peakIdx)*widthSec
	end := start + widthSec
	if end > until {
		end = until
	}
	return map[string]any{
		"index":   peakIdx,
		"startTs": time.Unix(start, 0).UTC().Format(time.RFC3339),
		"endTs":   time.Unix(end, 0).UTC().Format(time.RFC3339),
		"count":   peakCount,
	}
}
