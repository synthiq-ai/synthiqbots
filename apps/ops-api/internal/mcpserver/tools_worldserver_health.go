package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/synthiq-ai/synthiqbots/apps/ops-api/internal/dockerlog"
	"github.com/synthiq-ai/synthiqbots/apps/ops-api/internal/handlers"
)

const (
	worldserverHealthTimeout              = 15 * time.Second
	worldserverHealthErrorLookbackDefault = "30m"
	worldserverHealthErrorLookbackMax     = 24 * time.Hour
	// worldserverHealthLogLineCap upper-bounds the docker logs Tail argument
	// when scanning for the most recent error. Higher than ops_logs_tail's
	// 5000-line cap because we filter aggressively before keeping anything.
	worldserverHealthLogLineCap = 20000
	// memPercentWarnThreshold is the rolled-up health-check threshold for
	// memory_under_threshold. ac-worldserver gets OOM-killed (exit 137) when
	// the cgroup limit is hit; flagging at 90% gives the operator a heads-up
	// before the kernel reaper steps in.
	memPercentWarnThreshold = 90.0
)

// reAnsiEscape strips terminal color/cursor escape sequences from log
// messages. ac-worldserver wraps every error in red ANSI codes
// (\x1b[31;1m...\x1b[0m) — without stripping, the operator sees noise.
var reAnsiEscape = regexp.MustCompile(`\x1b\[[0-9;]*[A-Za-z]`)

// reHealthError matches log lines indicating an error or worse. The regex is
// curated for ac-worldserver output: it matches the red ANSI prefix used by
// AzerothCore for ERROR/FATAL severity (`\x1b[31`), plus common keywords that
// show up in stack traces and integration failures (HTTP request failed,
// connection refused, etc.). Keep this list deliberately small — false
// positives drown the signal.
var reHealthError = regexp.MustCompile(
	`(?i)\x1b\[31|\bfatal\b|\bpanic\b|\babort\b|\bcritical\b|\bexception\b|` +
		`\berror\b|\bfailed?\b|cannot\s+(open|find|connect|bind|read|write)|` +
		`connection\s+refused|no\s+response|\bdenied\b|\bcorrupt\b|\bcrash\b|` +
		`segfault|stack\s+trace`)

// RegisterWorldserverHealthTool registers `worldserver_health` — a single-call
// composite that gathers the diagnostic snapshot operators need when the
// AzerothCore worldserver gets flaky. Replaces the ops_status + container_stats
// + ops_logs_tail (grep="error") + container_inspect 4-call sequence operators
// were running per worldserver incident.
func RegisterWorldserverHealthTool(reg *Registry, deps OpsDeps) {
	reg.Register(Tool{
		Name: "worldserver_health",
		Description: "Composite ac-worldserver health snapshot in ONE call. Combines container " +
			"inspect (state, running, uptimeHuman, restartCount, exitCode), live resource usage " +
			"(cpuPercent, memPercent, memUsageBytes, memLimitBytes — same shape as container_stats), " +
			"DB ping (ops_ro pool), MCP ping (gameplay MCP at 18790), and the most recent " +
			"ERROR-severity log line within the lookback window (ANSI-stripped, default 30m, max " +
			"24h). Output includes a `checks[]` array rolling each piece into ok/fail with " +
			"`allOk` boolean — operators can branch on that single field instead of stitching " +
			"the sub-results. Replaces the ops_status + container_stats + ops_logs_tail " +
			"(grep=\"error\") + container_inspect 4-call sequence operators were running per " +
			"worldserver incident; the lastError piece in particular is the gap (no existing " +
			"tool surfaces severity-filtered log lines with ANSI stripping). Use this as the " +
			"FIRST call when investigating ac-worldserver outages, OOM kills (exit 137 = " +
			"memPercent >90 pre-crash), or DB connectivity issues. Read-only, ~1.5 s wall clock.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{
			"container":{"type":"string","description":"Container name (default ac-worldserver)"},
			"errorLookback":{"type":"string","description":"Window to scan for last ERROR (e.g. \"15m\", \"1h\"; default 30m, max 24h)"}
		}}`),
		Annotations: AnnRead(),
		Handler: func(ctx context.Context, raw json.RawMessage, _ string) any {
			var a struct {
				Container     string `json:"container"`
				ErrorLookback string `json:"errorLookback"`
			}
			_ = json.Unmarshal(raw, &a)
			container := a.Container
			if container == "" && deps.Server != nil && deps.Server.Cfg != nil {
				container = deps.Server.Cfg.WorldserverContainer
			}
			if container == "" {
				container = "ac-worldserver"
			}
			lookback := strings.TrimSpace(a.ErrorLookback)
			if lookback == "" {
				lookback = worldserverHealthErrorLookbackDefault
			}
			d, err := time.ParseDuration(lookback)
			if err != nil {
				return map[string]any{"error": "errorLookback must be a duration like \"30m\" or \"1h\""}
			}
			if d <= 0 {
				return map[string]any{"error": "errorLookback must be positive"}
			}
			if d > worldserverHealthErrorLookbackMax {
				return map[string]any{"error": "errorLookback exceeds 24h max"}
			}
			if deps.Server == nil {
				return map[string]any{"error": "handlers.Server not configured"}
			}

			c, cancel := context.WithTimeout(ctx, worldserverHealthTimeout)
			defer cancel()

			return collectWorldserverHealth(c, deps.Server, container, lookback)
		},
	})
}

// collectWorldserverHealth gathers each sub-snapshot and packs them into the
// response shape. Each sub-call is independent — a failure in one does NOT
// kill the others. Per-piece errors are surfaced both as inline `*Error` keys
// and as failing entries in `checks[]`, so the agent gets a partial-but-useful
// answer instead of a hard error.
func collectWorldserverHealth(ctx context.Context, srv *handlers.Server, container, lookback string) map[string]any {
	out := map[string]any{
		"container":     container,
		"errorLookback": lookback,
		"generatedAt":   time.Now().UTC(),
	}
	checks := make([]map[string]any, 0, 6)

	// 1. Inspect: container state + uptime.
	var insp *dockerlog.Inspect
	if i, err := srv.Docker.Inspect(ctx, container); err == nil {
		insp = i
		out["state"] = i.State.Status
		out["running"] = i.State.Running
		out["restartCount"] = i.RestartCount
		out["image"] = i.Image
		out["startedAt"] = i.State.StartedAt
		out["exitCode"] = i.State.ExitCode
		uptimeSecs, uptimeHuman := computeUptime(i.State.StartedAt, i.State.Running)
		out["uptimeSeconds"] = uptimeSecs
		out["uptimeHuman"] = uptimeHuman
		checks = append(checks, map[string]any{
			"name":   "container_running",
			"ok":     i.State.Running,
			"detail": i.State.Status,
		})
	} else {
		out["inspectError"] = err.Error()
		checks = append(checks, map[string]any{
			"name":   "container_running",
			"ok":     false,
			"detail": "inspect: " + err.Error(),
		})
	}

	// 2. Stats: only if container is running. Stopped containers have no live
	// stats — calling /stats on them returns immediately with zeroed fields,
	// which would just clutter the response.
	if insp != nil && insp.State.Running {
		if body, err := srv.Docker.StatsRaw(ctx, container); err == nil {
			if stats, err := summarizeStats(body); err == nil {
				out["cpuPercent"] = stats["cpuPercent"]
				out["memUsageBytes"] = stats["memUsageBytes"]
				out["memUsageHuman"] = stats["memUsageHuman"]
				out["memLimitBytes"] = stats["memLimitBytes"]
				out["memLimitHuman"] = stats["memLimitHuman"]
				out["memPercent"] = stats["memPercent"]
				memPct, _ := stats["memPercent"].(float64)
				checks = append(checks, map[string]any{
					"name":   "memory_under_threshold",
					"ok":     memPct < memPercentWarnThreshold,
					"detail": fmt.Sprintf("%.1f%% of limit (threshold %.0f%%)", memPct, memPercentWarnThreshold),
				})
			} else {
				out["statsError"] = "decode: " + err.Error()
			}
		} else {
			out["statsError"] = err.Error()
		}
	}

	// 3. DB ping (ops_ro pool). Operators care about the AzerothCore DB
	// (acore_*) being reachable — when ac-worldserver gets OOM-killed it
	// takes ac-database with it (exit 137 cascade), so DB ping doubles as
	// "the whole stack is up" indicator.
	if srv.DB != nil {
		if err := srv.DB.PingContext(ctx); err == nil {
			out["dbOk"] = true
			checks = append(checks, map[string]any{"name": "db_ping", "ok": true})
		} else {
			out["dbOk"] = false
			out["dbError"] = err.Error()
			checks = append(checks, map[string]any{
				"name":   "db_ping",
				"ok":     false,
				"detail": err.Error(),
			})
		}
	} else {
		out["dbOk"] = false
		out["dbError"] = "DB_DSN not configured"
		checks = append(checks, map[string]any{
			"name":   "db_ping",
			"ok":     false,
			"detail": "DB_DSN not configured",
		})
	}

	// 4. MCP ping. Even when the worldserver container is "running", the
	// in-process MCP can be wedged (mutex held, listener thread crashed).
	// Pinging the loopback bridge catches that case.
	if srv.MCP != nil {
		if err := srv.MCP.Ping(ctx); err == nil {
			out["mcpOk"] = true
			checks = append(checks, map[string]any{"name": "mcp_ping", "ok": true})
		} else {
			out["mcpOk"] = false
			out["mcpError"] = err.Error()
			checks = append(checks, map[string]any{
				"name":   "mcp_ping",
				"ok":     false,
				"detail": err.Error(),
			})
		}
	}

	// 5. Last error from logs. Skip when the container is stopped — the
	// stream-then-grep is wasted work and the docker logs endpoint can hang
	// briefly on stopped TTY containers.
	if insp != nil && insp.State.Running {
		last, count := scanLastError(ctx, srv.Docker, container, lookback)
		if last != nil {
			out["lastError"] = last
			out["errorCount"] = count
			checks = append(checks, map[string]any{
				"name":   "no_recent_errors",
				"ok":     false,
				"detail": fmt.Sprintf("matched %d error line(s) in %s", count, lookback),
			})
		} else {
			out["lastError"] = nil
			out["errorCount"] = 0
			checks = append(checks, map[string]any{
				"name": "no_recent_errors",
				"ok":   true,
			})
		}
	}

	out["checks"] = checks
	out["allOk"] = checksAllOK(checks)
	return out
}

// computeUptime parses Docker's RFC3339Nano StartedAt and returns seconds
// uptime + a humanized string ("2d3h", "5m"). Returns 0/empty if the
// container isn't running or the timestamp is unparseable.
func computeUptime(startedAt string, running bool) (int64, string) {
	if !running || startedAt == "" {
		return 0, ""
	}
	t, err := time.Parse(time.RFC3339Nano, startedAt)
	if err != nil {
		// Older daemons emit non-Nano. Fall back.
		t, err = time.Parse(time.RFC3339, startedAt)
		if err != nil {
			return 0, ""
		}
	}
	delta := time.Since(t)
	if delta < 0 {
		return 0, ""
	}
	return int64(delta.Seconds()), humanDuration(delta)
}

// humanDuration formats a duration as a compact uptime string. Mirrors
// `docker ps`'s "Up 2d" column rather than time.Duration.String() so the
// operator gets "2d3h" instead of "51h0m0s".
func humanDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	if d < 24*time.Hour {
		h := int(d.Hours())
		m := int(d.Minutes()) - h*60
		if m == 0 {
			return fmt.Sprintf("%dh", h)
		}
		return fmt.Sprintf("%dh%dm", h, m)
	}
	days := int(d.Hours()) / 24
	hours := int(d.Hours()) % 24
	if hours == 0 {
		return fmt.Sprintf("%dd", days)
	}
	return fmt.Sprintf("%dd%dh", days, hours)
}

// scanLastError streams up to lookback's worth of logs and returns the most
// recent line that matches reHealthError. Returns (nil, 0) when no matches
// fall in the window or the docker stream errors out — the caller surfaces
// the empty result as "no recent errors" rather than a hard failure.
func scanLastError(ctx context.Context, c *dockerlog.Client, container, lookback string) (map[string]any, int) {
	if c == nil {
		return nil, 0
	}
	ch, _, err := c.Stream(ctx, container, dockerlog.LogOpts{
		Tail:  fmt.Sprintf("%d", worldserverHealthLogLineCap),
		Since: lookback,
	})
	if err != nil {
		return nil, 0
	}
	lines := make([]dockerlog.Line, 0, 64)
	for ln := range ch {
		if reHealthError.MatchString(ln.Msg) {
			lines = append(lines, ln)
		}
	}
	return pickLatestError(lines)
}

// pickLatestError returns the most recent matching line in the slice plus the
// total count. "Most recent" prefers max TS when timestamps are present;
// falls back to last-by-iteration when stream timestamps are zero (some
// older docker daemons emit them empty for TTY containers).
//
// Split out from scanLastError so unit tests can drive the selection logic
// without standing up a docker socket.
func pickLatestError(lines []dockerlog.Line) (map[string]any, int) {
	if len(lines) == 0 {
		return nil, 0
	}
	best := lines[0]
	for _, ln := range lines[1:] {
		if best.TS.IsZero() && ln.TS.IsZero() {
			// Stream order; later wins.
			best = ln
			continue
		}
		if ln.TS.After(best.TS) {
			best = ln
		}
	}
	return map[string]any{
		"ts":            best.TS,
		"stream":        best.Stream,
		"msg":           best.Msg,
		"msgNormalized": strings.TrimSpace(reAnsiEscape.ReplaceAllString(best.Msg, "")),
	}, len(lines)
}

// checksAllOK returns true iff every entry in checks has ok:true.
func checksAllOK(checks []map[string]any) bool {
	for _, c := range checks {
		if ok, _ := c["ok"].(bool); !ok {
			return false
		}
	}
	return true
}
