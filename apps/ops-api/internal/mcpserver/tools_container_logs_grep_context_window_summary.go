package mcpserver

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/synthiq-ai/synthiqbots/apps/ops-api/internal/dockerlog"
)

// RegisterContainerLogsGrepContextWindowSummaryTool registers
// `container_logs_grep_context_window_summary` — the block-count companion to
// `container_logs_grep_context`. Runs the same window/regex/context-coalesce
// pipeline but returns one summary row per block (firstTs/lastTs/matchCount/
// lineCount/matchIndex) instead of the transcript itself.
//
// Why a separate tool, not a mode arg on container_logs_grep_context: the
// transcript path materializes a map[string]any per line, which dominates
// allocation cost on noisy windows. The summary path skips that entirely —
// allocations are O(blocks) not O(lines), so a 7d "count every OOM" sweep
// completes without dragging the transcript tool's per-line cost into a use
// case that doesn't need it. Operators reach for this when triaging
// "how many incidents across N days?" and then drill into the worst block via
// the transcript tool using the matchIndex returned here as a pointer.
func RegisterContainerLogsGrepContextWindowSummaryTool(reg *Registry, deps ContainerDeps) {
	if deps.DefaultContainer == "" {
		deps.DefaultContainer = "ac-worldserver"
	}

	reg.Register(Tool{
		Name: "container_logs_grep_context_window_summary",
		Description: "Block-count summary of `container_logs_grep_context` matches across " +
			"a multi-hour window, without the line bodies. Each block represents one " +
			"contiguous group of matches (adjacent matches whose context windows " +
			"overlap coalesce, same shape as the transcript tool) and is reported as " +
			"`{firstTs, lastTs, matchCount, lineCount, matchIndex}`. Use this when the " +
			"question is \"how many incidents hit ac-worldserver in the last 7d?\" " +
			"rather than \"show me the lines\" — output is ~50 bytes per block vs. " +
			"~4 KB transcript blocks, so 7d windows fit comfortably under the MCP " +
			"envelope. Pair with `container_logs_grep_context`: scan with this to find " +
			"hot windows, then replay the worst one in the transcript tool (matchIndex " +
			"returned here is the same anchor index used there). Distinct from " +
			"`container_logs_grep` (per-match list, no block grouping). Same arg shape " +
			"as the transcript tool: default window 1h, max 7d. `regex:true` flips " +
			"substring→RE2 regex; `caseInsensitive:true` folds case. `contextBefore` / " +
			"`contextAfter` default 0 (max 10 each) and control the coalescing window. " +
			"Bounded by `maxMatches` (default 200, max 2000) and `maxScanLines` " +
			"(default 200000, max 1000000). Read-only.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{
			"name":{"type":"string","description":"Container name or id (default ac-worldserver)"},
			"pattern":{"type":"string","description":"Substring (default) or RE2 regex (set regex:true)"},
			"regex":{"type":"boolean","description":"Treat pattern as RE2 regex (default false)"},
			"caseInsensitive":{"type":"boolean","description":"Case-insensitive match (default false)"},
			"since":{"type":"string","description":"Lookback (\"12h\", \"7d\", \"30m\") or unix seconds (default 1h, max 7d)"},
			"contextBefore":{"type":"integer","description":"Lines before each match (default 0, max 10) — controls block coalescing"},
			"contextAfter":{"type":"integer","description":"Lines after each match (default 0, max 10) — controls block coalescing"},
			"maxMatches":{"type":"integer","description":"Cap on counted matches (default 200, max 2000)"},
			"maxScanLines":{"type":"integer","description":"Cap on lines SCANNED before bailing (default 200000, max 1000000)"},
			"stream":{"type":"string","description":"Restrict to \"stdout\" or \"stderr\" (default both)"}
		},"required":["pattern"]}`),
		Annotations: AnnRead(),
		Handler: func(ctx context.Context, raw json.RawMessage, _ string) any {
			var a struct {
				Name            string `json:"name"`
				Pattern         string `json:"pattern"`
				Regex           bool   `json:"regex"`
				CaseInsensitive bool   `json:"caseInsensitive"`
				Since           string `json:"since"`
				ContextBefore   int    `json:"contextBefore"`
				ContextAfter    int    `json:"contextAfter"`
				MaxMatches      int    `json:"maxMatches"`
				MaxScanLines    int    `json:"maxScanLines"`
				Stream          string `json:"stream"`
			}
			_ = json.Unmarshal(raw, &a)

			name := pickContainer(a.Name, deps.DefaultContainer)

			if strings.TrimSpace(a.Pattern) == "" {
				return map[string]any{"error": "pattern required", "name": name}
			}

			matcher, err := buildGrepMatcherCtx(a.Pattern, a.Regex, a.CaseInsensitive)
			if err != nil {
				return map[string]any{"error": err.Error(), "name": name}
			}

			sinceUnix, err := resolveGrepSinceCtx(a.Since)
			if err != nil {
				return map[string]any{"error": "since: " + err.Error(), "name": name}
			}

			streamFilter, err := normalizeStreamFilterCtx(a.Stream)
			if err != nil {
				return map[string]any{"error": err.Error(), "name": name}
			}

			before := clampGrepIntCtx(a.ContextBefore, 0, 0, containerLogsGrepCtxMaxContext)
			after := clampGrepIntCtx(a.ContextAfter, 0, 0, containerLogsGrepCtxMaxContext)
			maxMatches := clampGrepIntCtx(a.MaxMatches, containerLogsGrepCtxDefaultMaxMatches, 1, containerLogsGrepCtxMaxMaxMatches)
			maxScan := clampGrepIntCtx(a.MaxScanLines, containerLogsGrepCtxDefaultMaxScan, 1, containerLogsGrepCtxHardMaxScan)

			scanCtx, cancel := context.WithCancel(ctx)
			defer cancel()

			ch, _, err := deps.Docker.Stream(scanCtx, name, dockerlog.LogOpts{
				Tail:  "all",
				Since: strconv.FormatInt(sinceUnix, 10),
			})
			if err != nil {
				return map[string]any{"error": err.Error(), "name": name}
			}

			blocks, stats := grepContextSummary(ch, matcher, streamFilter, before, after, maxMatches, maxScan, cancel)

			out := make([]map[string]any, len(blocks))
			for i, b := range blocks {
				out[i] = formatGrepCtxBlockSummary(b)
			}

			streamReported := streamFilter
			if streamReported == "" {
				streamReported = "both"
			}

			return map[string]any{
				"name":            name,
				"since":           sinceUnix,
				"until":           time.Now().Unix(),
				"pattern":         a.Pattern,
				"regex":           a.Regex,
				"caseInsensitive": a.CaseInsensitive,
				"stream":          streamReported,
				"contextBefore":   before,
				"contextAfter":    after,
				"scanned":         stats.scanned,
				"matchCount":      stats.matchCount,
				"blockCount":      len(blocks),
				"lineCount":       stats.lineCount,
				"matchTruncated":  stats.matchTruncated,
				"scanTruncated":   stats.scanTruncated,
				"blocks":          out,
			}
		},
	})
}

// grepCtxBlockSummary collapses one transcript block into the counts the
// summary tool returns. matchIndex tracks the LAST match folded into the
// block (mirrors the transcript tool's contract so the summary can serve as
// a pointer back into a follow-up transcript scan).
type grepCtxBlockSummary struct {
	firstTs    time.Time
	lastTs     time.Time
	matchCount int
	lineCount  int
	matchIndex int
}

// grepContextSummary runs the same window/coalesce shape as grepWithContext
// but tracks per-block counts instead of materializing line bodies. The
// coalesce model is identical: while pendingAfter > 0, a new match folds into
// the open block (its would-be-before window is already covered by the open
// block's after slots), so adjacent matches collapse into one summary row.
//
// `cancel` is invoked on cap-hit so the producer goroutine exits instead of
// wedging on a full channel after we stop draining.
func grepContextSummary(
	lines <-chan dockerlog.Line,
	matcher func(string) bool,
	streamFilter string,
	before, after, maxMatches, maxScan int,
	cancel context.CancelFunc,
) ([]grepCtxBlockSummary, grepCtxStats) {
	blocks := make([]grepCtxBlockSummary, 0)
	var ring []dockerlog.Line
	if before > 0 {
		ring = make([]dockerlog.Line, 0, before)
	}

	var open *grepCtxBlockSummary
	pendingAfter := 0
	stats := grepCtxStats{}

	flushOpen := func() {
		if open != nil {
			blocks = append(blocks, *open)
			open = nil
		}
	}

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

		if matcher(ln.Msg) {
			if stats.matchCount >= maxMatches {
				stats.matchTruncated = true
				cancel()
				break
			}
			stats.matchCount++

			if open == nil {
				open = &grepCtxBlockSummary{}
				// Pull before-context counts from the ring into the new block.
				// We don't carry the line bodies, but we still account for
				// their TS bounds (firstTs picks the first non-zero ring TS;
				// lastTs walks forward).
				for _, r := range ring {
					open.lineCount++
					stats.lineCount++
					if !r.TS.IsZero() {
						if open.firstTs.IsZero() {
							open.firstTs = r.TS
						}
						open.lastTs = r.TS
					}
				}
			}
			open.matchCount++
			open.lineCount++
			stats.lineCount++
			if !ln.TS.IsZero() {
				if open.firstTs.IsZero() {
					open.firstTs = ln.TS
				}
				open.lastTs = ln.TS
			}
			open.matchIndex = stats.matchCount - 1
			ring = ring[:0]
			pendingAfter = after
			if pendingAfter == 0 {
				flushOpen()
			}
			continue
		}

		if open != nil && pendingAfter > 0 {
			open.lineCount++
			stats.lineCount++
			if !ln.TS.IsZero() {
				if open.firstTs.IsZero() {
					open.firstTs = ln.TS
				}
				open.lastTs = ln.TS
			}
			pendingAfter--
			if pendingAfter == 0 {
				flushOpen()
			}
			continue
		}

		if before > 0 {
			if len(ring) == before {
				copy(ring, ring[1:])
				ring = ring[:before-1]
			}
			ring = append(ring, ln)
		}
	}

	flushOpen()
	return blocks, stats
}

func formatGrepCtxBlockSummary(b grepCtxBlockSummary) map[string]any {
	row := map[string]any{
		"matchCount": b.matchCount,
		"lineCount":  b.lineCount,
		"matchIndex": b.matchIndex,
	}
	if !b.firstTs.IsZero() {
		row["firstTs"] = b.firstTs.UTC().Format(time.RFC3339Nano)
	}
	if !b.lastTs.IsZero() {
		row["lastTs"] = b.lastTs.UTC().Format(time.RFC3339Nano)
	}
	return row
}
