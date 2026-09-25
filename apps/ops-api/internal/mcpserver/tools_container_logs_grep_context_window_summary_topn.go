package mcpserver

import (
	"context"
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/synthiq-ai/synthiqbots/apps/ops-api/internal/dockerlog"
)

// Sort keys + clamp constants for the top-N variant. Kept as named consts so
// the input-schema description, the handler clamp, and the tests stay in
// lockstep — adding a new sort key only touches grepCtxTopNSortKeys.
const (
	grepCtxTopNSortMatchCount = "matchCount"
	grepCtxTopNSortLineCount  = "lineCount"
	grepCtxTopNSortFirstTs    = "firstTs"

	grepCtxTopNDefaultTopN = 5
	grepCtxTopNMaxTopN     = 200
)

// grepCtxTopNSortKeys is the canonical allowlist of `sortBy` values surfaced
// to operators. Order is significant: the handler echoes this list in the
// error message on a bad arg so a typo is self-correcting.
var grepCtxTopNSortKeys = []string{
	grepCtxTopNSortMatchCount,
	grepCtxTopNSortLineCount,
	grepCtxTopNSortFirstTs,
}

// RegisterContainerLogsGrepContextWindowSummaryTopNTool registers
// `container_logs_grep_context_window_summary_topn` — the pre-sorted top-N
// companion to `container_logs_grep_context_window_summary`. Runs the same
// window/regex/context-coalesce pipeline, then sorts the coalesced blocks by
// the operator's chosen field and slices to the top N before formatting.
//
// Why a separate tool, not a `sortBy`/`topN` arg pair on the base summary
// tool: same reasoning as the PR #154 / PR #165 split — the cheap unsorted
// path stays cheap (callers asking "every block in the window" don't pay for
// an extra in-memory sort), and agent tool-selection latches on intent
// ("top N" is a different question shape than "all"). Pairs with the base
// summary tool (sweep → identify hot windows when N is large) and the
// transcript tool (replay worst block via matchIndex).
//
// Output envelope mirrors the base tool: `blocks` carries the sliced view;
// `blockCount` reports the total pre-slice so the operator can tell whether
// more blocks were dropped beyond topN.
func RegisterContainerLogsGrepContextWindowSummaryTopNTool(reg *Registry, deps ContainerDeps) {
	if deps.DefaultContainer == "" {
		deps.DefaultContainer = "ac-worldserver"
	}

	reg.Register(Tool{
		Name: "container_logs_grep_context_window_summary_topn",
		Description: "Pre-sorted top-N hottest blocks from " +
			"`container_logs_grep_context_window_summary`. Runs the same " +
			"window/regex/context-coalesce pipeline and returns the same " +
			"`{firstTs, lastTs, matchCount, lineCount, matchIndex}` block shape, but " +
			"sorts by `sortBy` (`matchCount` default — also `lineCount`, `firstTs`) " +
			"and slices to `topN` (default 5, max 200) before formatting. Use this " +
			"when the question is \"what were the worst 5 incidents in the last 7d?\" " +
			"and you don't want to client-side sort the full unsorted block list. " +
			"`blockCount` reports the total pre-slice count so you can tell whether " +
			"more blocks were dropped beyond topN. Pair with " +
			"`container_logs_grep_context_window_summary` (cheap unsorted sweep when " +
			"every block matters) and `container_logs_grep_context` (replay the worst " +
			"one via the matchIndex returned here). Distinct from `container_logs_grep` " +
			"(per-match list, no block grouping). Same arg shape as the base summary " +
			"tool, default container ac-worldserver; default window 1h, max 7d. `regex:true` flips substring→RE2 regex; " +
			"`caseInsensitive:true` folds case. `contextBefore` / `contextAfter` " +
			"default 0 (max 10 each) and control the coalescing window. Bounded by " +
			"`maxMatches` (default 200, max 2000) and `maxScanLines` (default 200000, " +
			"max 1000000). Read-only.",
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
			"stream":{"type":"string","description":"Restrict to \"stdout\" or \"stderr\" (default both)"},
			"sortBy":{"type":"string","description":"Block sort key: matchCount (default), lineCount, or firstTs"},
			"topN":{"type":"integer","description":"Return the top N blocks after sort (default 5, max 200)"}
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
				SortBy          string `json:"sortBy"`
				TopN            int    `json:"topN"`
			}
			_ = json.Unmarshal(raw, &a)

			name := pickContainer(a.Name, deps.DefaultContainer)

			if strings.TrimSpace(a.Pattern) == "" {
				return map[string]any{"error": "pattern required", "name": name}
			}

			sortKey, err := normalizeGrepCtxTopNSortBy(a.SortBy)
			if err != nil {
				return map[string]any{"error": err.Error(), "name": name}
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
			topN := clampGrepIntCtx(a.TopN, grepCtxTopNDefaultTopN, 1, grepCtxTopNMaxTopN)

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

			totalBlocks := len(blocks)
			sortGrepCtxBlockSummariesTopN(blocks, sortKey)
			if topN < len(blocks) {
				blocks = blocks[:topN]
			}

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
				"sortBy":          sortKey,
				"topN":            topN,
				"scanned":         stats.scanned,
				"matchCount":      stats.matchCount,
				"blockCount":      totalBlocks,
				"lineCount":       stats.lineCount,
				"matchTruncated":  stats.matchTruncated,
				"scanTruncated":   stats.scanTruncated,
				"blocks":          out,
			}
		},
	})
}

// normalizeGrepCtxTopNSortBy validates the `sortBy` arg. Empty string falls
// back to the default sort (matchCount). Unknown values produce an error
// that echoes the allowlist so a typo is self-correcting.
func normalizeGrepCtxTopNSortBy(in string) (string, error) {
	s := strings.TrimSpace(in)
	if s == "" {
		return grepCtxTopNSortMatchCount, nil
	}
	for _, k := range grepCtxTopNSortKeys {
		if s == k {
			return s, nil
		}
	}
	return "", &grepCtxTopNSortByErr{got: in}
}

type grepCtxTopNSortByErr struct{ got string }

func (e *grepCtxTopNSortByErr) Error() string {
	return "sortBy: unknown value " + strconv.Quote(e.got) + " — expected one of " + strings.Join(grepCtxTopNSortKeys, ", ")
}

// sortGrepCtxBlockSummariesTopN sorts blocks in-place by the chosen field
// with deterministic tiebreakers. matchIndex is the final tiebreaker — it's
// monotonically assigned per scan and unique across all blocks in the same
// invocation, so equal-primary equal-secondary blocks land in stable scan
// order (oldest match first) without any random-iteration churn.
//
// All three primary sort directions surface "what was the most/biggest" with
// the operator's chosen lens:
//   - matchCount: desc — most matches first (the typical "worst incident" use case)
//   - lineCount:  desc — biggest context-coalesced block first
//   - firstTs:    desc — most recent block first (operator triaging "what just happened?")
//
// Tiebreakers chain through the other two fields for stability:
//   - matchCount primary → lineCount desc → firstTs desc → matchIndex asc
//   - lineCount  primary → matchCount desc → firstTs desc → matchIndex asc
//   - firstTs    primary → matchCount desc → lineCount desc → matchIndex asc
func sortGrepCtxBlockSummariesTopN(blocks []grepCtxBlockSummary, sortKey string) {
	sort.SliceStable(blocks, func(i, j int) bool {
		a, b := blocks[i], blocks[j]
		switch sortKey {
		case grepCtxTopNSortLineCount:
			if a.lineCount != b.lineCount {
				return a.lineCount > b.lineCount
			}
			if a.matchCount != b.matchCount {
				return a.matchCount > b.matchCount
			}
			if !a.firstTs.Equal(b.firstTs) {
				return a.firstTs.After(b.firstTs)
			}
		case grepCtxTopNSortFirstTs:
			if !a.firstTs.Equal(b.firstTs) {
				return a.firstTs.After(b.firstTs)
			}
			if a.matchCount != b.matchCount {
				return a.matchCount > b.matchCount
			}
			if a.lineCount != b.lineCount {
				return a.lineCount > b.lineCount
			}
		default: // matchCount + unknown (already rejected upstream, safety fallback)
			if a.matchCount != b.matchCount {
				return a.matchCount > b.matchCount
			}
			if a.lineCount != b.lineCount {
				return a.lineCount > b.lineCount
			}
			if !a.firstTs.Equal(b.firstTs) {
				return a.firstTs.After(b.firstTs)
			}
		}
		return a.matchIndex < b.matchIndex
	})
}
