package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/synthiq-ai/synthiqbots/apps/ops-api/internal/dockerlog"
)

const (
	// containerLogsGrepCtxDefaultSince mirrors container_logs_grep's 1h default:
	// operators reach for this tool after seeing a hit elsewhere ("re-run with
	// context"), so the typical window is the same shape.
	containerLogsGrepCtxDefaultSince = "1h"

	// containerLogsGrepCtxMaxWindow matches the json-file driver retention
	// reality on a busy ac-worldserver (~5d on disk, 7d ceiling above which
	// the daemon hits rotation boundaries anyway).
	containerLogsGrepCtxMaxWindow = 7 * 24 * time.Hour

	// containerLogsGrepCtxDefaultMaxMatches caps returned match-anchored
	// blocks (not lines). With contextBefore+contextAfter <= 20 a block tops
	// out around 21 lines, so 200 blocks fits comfortably under the 25 KB
	// MCP envelope at typical worldserver line widths.
	containerLogsGrepCtxDefaultMaxMatches = 200
	containerLogsGrepCtxMaxMaxMatches     = 2000

	// containerLogsGrepCtxDefaultMaxScan keeps a 12h "noisy worldserver" pass
	// under ~10s wall clock; matches the sibling tool's safety ceiling.
	containerLogsGrepCtxDefaultMaxScan = 200_000
	containerLogsGrepCtxHardMaxScan    = 1_000_000

	// containerLogsGrepCtxMaxContext caps `contextBefore` / `contextAfter`.
	// 10 lines mirrors `grep -B 10 -A 10` muscle memory; larger values
	// blow the per-block size budget and rarely add diagnostic signal
	// (a stack trace fits in <10 lines for ac-worldserver crashes).
	containerLogsGrepCtxMaxContext = 10
)

// RegisterContainerLogsGrepContextTool registers `container_logs_grep_context` —
// the `-B`/`-A` shape of `container_logs_grep`. Returns each match flanked by
// up to N lines of preceding and following context, grouped into blocks.
//
// Why a separate tool, not new args on `container_logs_grep`: the cheap
// match-only path is what most callers want (it's small, fast, and the
// agent's tool-selection heuristics latch onto "search → grep"). Bolting
// `contextBefore`/`contextAfter` onto the existing tool would force every
// caller through the ring-buffer code path even when context=0. Keeping the
// shape distinct lets operators reach for the context variant only when
// triaging stack traces or pre-/post-crash sequences.
func RegisterContainerLogsGrepContextTool(reg *Registry, deps ContainerDeps) {
	if deps.DefaultContainer == "" {
		deps.DefaultContainer = "ac-worldserver"
	}

	reg.Register(Tool{
		Name: "container_logs_grep_context",
		Description: "Pattern-search a container's stdout/stderr across a multi-hour " +
			"window, returning each match with `contextBefore` / `contextAfter` lines " +
			"of surrounding context (the `-B`/`-A` shape of grep). Matches are grouped " +
			"into blocks; adjacent matches whose context windows overlap coalesce into " +
			"one block (no duplicate lines). Use this when `container_logs_grep` finds " +
			"a hit and you want the calling lines around it — e.g. an ac-worldserver " +
			"stack trace where the panic line is meaningless without the 5 preceding " +
			"frames. Distinct from `container_logs_grep` (cheap match-only path) and " +
			"`ops_logs_tail` (5000-line cap before filter). Default window 1h, max 7d. " +
			"`regex:true` flips substring→RE2 regex; `caseInsensitive:true` folds case. " +
			"`contextBefore` / `contextAfter` default 0 (max 10 each). Bounded by " +
			"`maxMatches` (default 200, max 2000) and `maxScanLines` (default 200000, " +
			"max 1000000). Read-only.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{
			"name":{"type":"string","description":"Container name or id (default ac-worldserver)"},
			"pattern":{"type":"string","description":"Substring (default) or RE2 regex (set regex:true)"},
			"regex":{"type":"boolean","description":"Treat pattern as RE2 regex (default false)"},
			"caseInsensitive":{"type":"boolean","description":"Case-insensitive match (default false)"},
			"since":{"type":"string","description":"Lookback (\"12h\", \"7d\", \"30m\") or unix seconds (default 1h, max 7d)"},
			"contextBefore":{"type":"integer","description":"Lines before each match (default 0, max 10)"},
			"contextAfter":{"type":"integer","description":"Lines after each match (default 0, max 10)"},
			"maxMatches":{"type":"integer","description":"Cap on returned match-anchored blocks (default 200, max 2000)"},
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

			blocks, stats := grepWithContext(ch, matcher, streamFilter, before, after, maxMatches, maxScan, cancel)

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
				"blocks":          blocks,
			}
		},
	})
}

// grepCtxStats is the post-scan accounting returned alongside the blocks.
type grepCtxStats struct {
	scanned        int
	matchCount     int
	lineCount      int
	matchTruncated bool
	scanTruncated  bool
}

// grepWithContext consumes a dockerlog.Line channel and produces match-anchored
// blocks with surrounding before/after context. Adjacent matches whose
// after-windows overlap coalesce into a single block so the operator never
// sees the same line twice in one response.
//
// Coalescing model: while we're still emitting `after` lines for the previous
// match and a NEW match arrives, the new match's "before" window is already
// inside the open block (those would-be-after lines ARE the would-be-before
// lines), so we just append the new match line and reset the after-countdown.
// Same physical line, single emission — the operator gets one continuous
// transcript instead of two blocks with a half-shared duplicate prefix.
//
// `cancel` is invoked on cap-hit so the producer goroutine exits instead of
// wedging on a full channel after we stop draining.
func grepWithContext(
	lines <-chan dockerlog.Line,
	matcher func(string) bool,
	streamFilter string,
	before, after, maxMatches, maxScan int,
	cancel context.CancelFunc,
) ([]map[string]any, grepCtxStats) {
	blocks := make([]map[string]any, 0)
	var ring []dockerlog.Line // last `before` non-match lines that passed stream filter
	if before > 0 {
		ring = make([]dockerlog.Line, 0, before)
	}

	var openBlockLines []map[string]any
	pendingAfter := 0
	stats := grepCtxStats{}

	flushOpen := func() {
		if openBlockLines != nil {
			blocks = append(blocks, map[string]any{
				"lines":      openBlockLines,
				"matchIndex": stats.matchCount - 1,
			})
			openBlockLines = nil
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

			if openBlockLines == nil {
				openBlockLines = make([]map[string]any, 0, len(ring)+1+after)
				for _, r := range ring {
					openBlockLines = append(openBlockLines, formatGrepCtxLine(r, "before"))
					stats.lineCount++
				}
			}
			openBlockLines = append(openBlockLines, formatGrepCtxLine(ln, "match"))
			stats.lineCount++
			ring = ring[:0]
			pendingAfter = after
			if pendingAfter == 0 {
				flushOpen()
			}
			continue
		}

		if openBlockLines != nil && pendingAfter > 0 {
			openBlockLines = append(openBlockLines, formatGrepCtxLine(ln, "after"))
			stats.lineCount++
			pendingAfter--
			if pendingAfter == 0 {
				flushOpen()
			}
			continue
		}

		// Outside any open block: this line is a candidate "before" for some
		// future match. Drop into the ring (drop oldest if full).
		if before > 0 {
			if len(ring) == before {
				copy(ring, ring[1:])
				ring = ring[:before-1]
			}
			ring = append(ring, ln)
		}
	}

	// Channel closed while a block was still open (after-window didn't fill).
	flushOpen()

	return blocks, stats
}

func formatGrepCtxLine(ln dockerlog.Line, kind string) map[string]any {
	row := map[string]any{
		"stream": ln.Stream,
		"msg":    ln.Msg,
		"kind":   kind,
	}
	if !ln.TS.IsZero() {
		row["ts"] = ln.TS.UTC().Format(time.RFC3339Nano)
	}
	return row
}

// buildGrepMatcherCtx mirrors the matcher in container_logs_grep but lives
// here as a private duplicate so this file compiles standalone (the sibling
// tool may not have landed yet, or may have evolved independently). Same
// (?i)-folded RE2 contract: case insensitivity goes into the pattern at
// compile time, not via per-line ToLower, so caller flags like (?m) survive
// untouched.
func buildGrepMatcherCtx(pattern string, useRegex, caseInsensitive bool) (func(string) bool, error) {
	if useRegex {
		expr := pattern
		if caseInsensitive {
			expr = "(?i)" + expr
		}
		re, err := regexp.Compile(expr)
		if err != nil {
			return nil, fmt.Errorf("invalid regex: %w", err)
		}
		return re.MatchString, nil
	}
	if caseInsensitive {
		needle := strings.ToLower(pattern)
		return func(s string) bool { return strings.Contains(strings.ToLower(s), needle) }, nil
	}
	return func(s string) bool { return strings.Contains(s, pattern) }, nil
}

// resolveGrepSinceCtx parses the user-supplied lookback. Same parser as
// container_logs_grep — duration, Nd days, or unix-seconds epoch — with the
// same 7d cap.
func resolveGrepSinceCtx(arg string) (int64, error) {
	in := strings.TrimSpace(arg)
	if in == "" {
		in = containerLogsGrepCtxDefaultSince
	}
	now := time.Now()
	maxLookback := now.Add(-containerLogsGrepCtxMaxWindow).Unix()

	if d, err := time.ParseDuration(in); err == nil {
		if d < 0 {
			return 0, fmt.Errorf("negative duration %q", arg)
		}
		if d > containerLogsGrepCtxMaxWindow {
			d = containerLogsGrepCtxMaxWindow
		}
		return now.Add(-d).Unix(), nil
	}
	if d, err := parseDurationDays(in); err == nil {
		if d < 0 {
			return 0, fmt.Errorf("negative duration %q", arg)
		}
		if d > containerLogsGrepCtxMaxWindow {
			d = containerLogsGrepCtxMaxWindow
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

func normalizeStreamFilterCtx(arg string) (string, error) {
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

func clampGrepIntCtx(v, def, lo, hi int) int {
	if v < 0 {
		return def
	}
	// Distinguish "caller passed 0 explicitly" (valid for context args)
	// from "field absent" — we can't, since absent and zero both decode to 0.
	// The default for context args IS 0, so this is moot for them. For
	// caps where 0 means "use default", the def comparison still wins.
	if v == 0 {
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
