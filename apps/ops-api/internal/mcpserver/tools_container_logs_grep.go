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
	// containerLogsGrepDefaultSince is the lookback applied when the caller
	// passes no `since`. One hour mirrors the typical "did something just
	// break?" follow-up to a worldserver alert; multi-hour windows usually
	// come with an explicit since.
	containerLogsGrepDefaultSince = "1h"

	// containerLogsGrepMaxWindow caps lookback. Seven days because the
	// daemon's json-file driver rotates aggressively on a busy ac-worldserver
	// and scanning further than that almost always hits the rotation
	// boundary anyway — there's no value in pretending we can search a 30d
	// window if the daemon only retains 5d of bytes.
	containerLogsGrepMaxWindow = 7 * 24 * time.Hour

	// containerLogsGrepDefaultMaxMatches caps returned matches when the
	// caller doesn't override. 200 fits comfortably under the 25 KB MCP
	// envelope at typical worldserver log-line widths (~200-400 B).
	containerLogsGrepDefaultMaxMatches = 200
	containerLogsGrepMaxMaxMatches     = 2000

	// containerLogsGrepDefaultMaxScan bails out of a runaway scan after this
	// many lines. Worldserver during a crash-loop can emit 30k+ lines/hour;
	// the default keeps a "12h" search under 10s wall clock in the happy path.
	containerLogsGrepDefaultMaxScan = 200_000
	containerLogsGrepHardMaxScan    = 1_000_000
)

// RegisterContainerLogsGrepTool registers `container_logs_grep` — a streaming
// pattern search over a container's stdout/stderr across a multi-hour window.
//
// The crucial difference vs. `ops_logs_tail`: that tool caps at 5000 lines
// TOTAL before the substring filter runs, so a match buried at line 30000 of
// a busy ac-worldserver window is invisible. This tool streams the full
// since→now window through the matcher and caps only the MATCHES it returns.
// Use it for "was there an OOM hint in the last 12h?", "every LoadFromDB
// error today", or any retro-search that ops_logs_tail can't reach.
func RegisterContainerLogsGrepTool(reg *Registry, deps ContainerDeps) {
	if deps.DefaultContainer == "" {
		deps.DefaultContainer = "ac-worldserver"
	}

	reg.Register(Tool{
		Name: "container_logs_grep",
		Description: "Pattern-search a container's stdout/stderr across a multi-hour " +
			"window. Streams the full since→now window through the matcher and caps " +
			"only the MATCHES it returns. Distinct from `ops_logs_tail`, which caps " +
			"at 5000 LINES TOTAL before filtering — a match buried at line 30000 of " +
			"a busy ac-worldserver window is invisible there. Use this for \"was there " +
			"an OOM-related hint in the last 12h?\" or \"find every LoadFromDB error " +
			"in the last 24h\" investigations. Pair with `container_events` (lifecycle " +
			"die/oom/start) for full incident timelines. Default window 1h, max 7d. " +
			"`regex:true` flips substring→RE2 regex; `caseInsensitive:true` folds case " +
			"before matching. Bounded by `maxMatches` (default 200, max 2000) and a " +
			"safety `maxScanLines` ceiling (default 200000, max 1000000). Read-only.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{
			"name":{"type":"string","description":"Container name or id (default ac-worldserver)"},
			"pattern":{"type":"string","description":"Substring (default) or RE2 regex (set regex:true)"},
			"regex":{"type":"boolean","description":"Treat pattern as RE2 regex (default false)"},
			"caseInsensitive":{"type":"boolean","description":"Case-insensitive match (default false)"},
			"since":{"type":"string","description":"Lookback (\"12h\", \"7d\", \"30m\") or unix seconds (default 1h, max 7d)"},
			"maxMatches":{"type":"integer","description":"Cap on returned matches (default 200, max 2000)"},
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
				MaxMatches      int    `json:"maxMatches"`
				MaxScanLines    int    `json:"maxScanLines"`
				Stream          string `json:"stream"`
			}
			_ = json.Unmarshal(raw, &a)

			name := pickContainer(a.Name, deps.DefaultContainer)

			if strings.TrimSpace(a.Pattern) == "" {
				return map[string]any{"error": "pattern required", "name": name}
			}

			matcher, err := buildGrepMatcher(a.Pattern, a.Regex, a.CaseInsensitive)
			if err != nil {
				return map[string]any{"error": err.Error(), "name": name}
			}

			sinceUnix, err := resolveGrepSince(a.Since)
			if err != nil {
				return map[string]any{"error": "since: " + err.Error(), "name": name}
			}

			streamFilter, err := normalizeStreamFilter(a.Stream)
			if err != nil {
				return map[string]any{"error": err.Error(), "name": name}
			}

			maxMatches := clampGrepInt(a.MaxMatches, containerLogsGrepDefaultMaxMatches, 1, containerLogsGrepMaxMaxMatches)
			maxScan := clampGrepInt(a.MaxScanLines, containerLogsGrepDefaultMaxScan, 1, containerLogsGrepHardMaxScan)

			// Cancel the docker stream as soon as we hit a cap so the
			// producer goroutine exits instead of sitting wedged on a full
			// channel after we stop reading.
			scanCtx, cancel := context.WithCancel(ctx)
			defer cancel()

			ch, _, err := deps.Docker.Stream(scanCtx, name, dockerlog.LogOpts{
				Tail:  "all",
				Since: strconv.FormatInt(sinceUnix, 10),
			})
			if err != nil {
				return map[string]any{"error": err.Error(), "name": name}
			}

			matches := make([]map[string]any, 0, maxMatches)
			scanned := 0
			scanTruncated := false
			matchTruncated := false

			for ln := range ch {
				scanned++
				if scanned > maxScan {
					scanTruncated = true
					cancel()
					break
				}
				if streamFilter != "" && ln.Stream != streamFilter {
					continue
				}
				if !matcher(ln.Msg) {
					continue
				}
				if len(matches) >= maxMatches {
					matchTruncated = true
					cancel()
					break
				}
				row := map[string]any{
					"stream": ln.Stream,
					"msg":    ln.Msg,
				}
				if !ln.TS.IsZero() {
					row["ts"] = ln.TS.UTC().Format(time.RFC3339Nano)
				}
				matches = append(matches, row)
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
				"scanned":         scanned,
				"matchCount":      len(matches),
				"matchTruncated":  matchTruncated,
				"scanTruncated":   scanTruncated,
				"matches":         matches,
			}
		},
	})
}

// buildGrepMatcher returns a string-matching predicate. RE2 case-insensitivity
// is folded into the pattern via the (?i) flag — applying it once at compile
// time is cheaper than ToLower-ing every scanned line, and it composes with
// caller-supplied flags like (?m).
func buildGrepMatcher(pattern string, useRegex, caseInsensitive bool) (func(string) bool, error) {
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

// resolveGrepSince converts a user-supplied since argument into an absolute
// unix-seconds timestamp. Accepts Go durations ("12h", "30m"), the "Nd" days
// extension shared with container_events ("7d"), or a unix-seconds epoch.
// Window is capped at containerLogsGrepMaxWindow.
func resolveGrepSince(arg string) (int64, error) {
	in := strings.TrimSpace(arg)
	if in == "" {
		in = containerLogsGrepDefaultSince
	}
	now := time.Now()
	maxLookback := now.Add(-containerLogsGrepMaxWindow).Unix()

	if d, err := time.ParseDuration(in); err == nil {
		if d < 0 {
			return 0, fmt.Errorf("negative duration %q", arg)
		}
		if d > containerLogsGrepMaxWindow {
			d = containerLogsGrepMaxWindow
		}
		return now.Add(-d).Unix(), nil
	}
	if d, err := parseDurationDays(in); err == nil {
		if d < 0 {
			return 0, fmt.Errorf("negative duration %q", arg)
		}
		if d > containerLogsGrepMaxWindow {
			d = containerLogsGrepMaxWindow
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

// normalizeStreamFilter validates the stream arg and returns the canonical
// docker stream name to match against ("stdout"/"stderr") or "" for both.
func normalizeStreamFilter(arg string) (string, error) {
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

func clampGrepInt(v, def, lo, hi int) int {
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
