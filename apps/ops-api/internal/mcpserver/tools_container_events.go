package mcpserver

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

const (
	// containerEventsDefaultWindow is the lookback applied when the caller
	// passes no `since`. Matches what an operator typically asks for first
	// when investigating "what happened overnight" on a crashed container.
	containerEventsDefaultWindow = "24h"

	// containerEventsMaxWindowSec caps the since→until span. The Docker
	// daemon's in-memory event log generally retains much less than 30 days,
	// but capping prevents an accidental "1y" from forcing the daemon into a
	// long, allocation-heavy walk of older events.
	containerEventsMaxWindowSec = int64(30 * 24 * 3600)

	// containerEventsMaxRows caps the rows we surface in the response. Busy
	// containers (e.g. ac-worldserver during a tight crash-restart loop) can
	// emit hundreds of die/start pairs in a single day; the cap keeps the
	// tool result under the 25 KB MCP envelope. `truncated:true` tells the
	// operator to narrow the window or filter.
	containerEventsMaxRows = 500
)

// containerEventDefaults is the lifecycle subset the tool surfaces when the
// caller doesn't override `eventTypes`. Curated for the OOM-forensics use
// case: `oom` is emitted by the daemon BEFORE the `die` event when the kernel
// reaper steps in, and that ordering is what distinguishes a real OOM kill
// from a manual SIGKILL.
var containerEventDefaults = []string{
	"die", "oom", "kill", "start", "restart", "stop", "health_status",
}

// RegisterContainerEventsTool registers `container_events` — a bounded query
// over the Docker daemon's event log for a single container. Replaces the
// shell-level `docker events --since=X --until=Y --filter=container=NAME`
// invocation operators run when investigating ac-worldserver OOM kills or
// multi-day ac-database outages.
//
// `container_inspect` only surfaces the LAST start/stop pair; for incidents
// where the container crash-looped overnight, the operator needs the FULL
// sequence to spot whether the OOM kill was isolated or part of a pattern.
func RegisterContainerEventsTool(reg *Registry, deps ContainerDeps) {
	if deps.DefaultContainer == "" {
		deps.DefaultContainer = "ac-worldserver"
	}

	reg.Register(Tool{
		Name: "container_events",
		Description: "Query Docker daemon lifecycle events for a container over a bounded " +
			"window. Returns oldest→newest die/oom/kill/start/restart/stop/health_status " +
			"events with timestamp, action, exit code, and signal. Default window 24h, " +
			"max 30d (capped further by the daemon's in-memory event-log retention). " +
			"Use this for OOM forensics on ac-worldserver (exit code 137 = OOM, and the " +
			"daemon emits a separate `oom` event before the `die`) or multi-day " +
			"ac-database outages — `container_inspect` only shows the last start/stop, " +
			"not the history. Pass `eventTypes:[]` (empty array) to bypass the lifecycle " +
			"filter and see every event type. Read-only.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{
			"name":{"type":"string","description":"Container name or id (default ac-worldserver)"},
			"since":{"type":"string","description":"Lookback (e.g. \"24h\", \"7d\", \"30m\") or unix seconds (default 24h, max 30d)"},
			"until":{"type":"string","description":"End of window (duration or unix seconds, default now)"},
			"eventTypes":{"type":"array","items":{"type":"string"},"description":"Override default lifecycle filter. Empty array = all container events."}
		}}`),
		Annotations: AnnRead(),
		Handler: func(ctx context.Context, raw json.RawMessage, _ string) any {
			var a struct {
				Name       string   `json:"name"`
				Since      string   `json:"since"`
				Until      string   `json:"until"`
				EventTypes []string `json:"eventTypes"`
			}
			_ = json.Unmarshal(raw, &a)

			name := pickContainer(a.Name, deps.DefaultContainer)

			untilUnix, err := resolveEventTimestamp(a.Until, "now", false)
			if err != nil {
				return map[string]any{"error": "until: " + err.Error(), "name": name}
			}
			sinceUnix, err := resolveEventTimestamp(a.Since, containerEventsDefaultWindow, true)
			if err != nil {
				return map[string]any{"error": "since: " + err.Error(), "name": name}
			}
			if sinceUnix >= untilUnix {
				return map[string]any{"error": "since must be < until", "name": name, "since": sinceUnix, "until": untilUnix}
			}
			if untilUnix-sinceUnix > containerEventsMaxWindowSec {
				sinceUnix = untilUnix - containerEventsMaxWindowSec
			}

			// Distinguish "use defaults" (nil) from "no filter" (empty slice).
			// The Anthropic MCP SDK serializes both forms, and operators reach
			// for the empty-array form when they want to see less-common events
			// like `exec_die` or `rename` without listing every variant.
			var eventTypes []string
			useDefaults := a.EventTypes == nil
			if useDefaults {
				eventTypes = containerEventDefaults
			} else {
				eventTypes = a.EventTypes
			}

			body, err := deps.Docker.EventsRaw(ctx, name, sinceUnix, untilUnix, eventTypes)
			if err != nil {
				return map[string]any{"error": err.Error(), "name": name}
			}

			events, truncated := parseContainerEvents(body, containerEventsMaxRows)

			return map[string]any{
				"name":       name,
				"since":      sinceUnix,
				"until":      untilUnix,
				"eventTypes": eventTypes,
				"events":     events,
				"count":      len(events),
				"truncated":  truncated,
			}
		},
	})
}

// resolveEventTimestamp converts a user-supplied string into a unix-seconds
// timestamp. Accepts:
//   - "now" / "" → wall clock (the empty case applies `def` first)
//   - unix seconds (10-digit integer)
//   - Go duration with our "d" extension ("7d" = 7*24h)
//
// `isPast` controls relative-duration arithmetic: `since` subtracts from now;
// `until` would add (rare — operators don't usually ask for future events).
func resolveEventTimestamp(arg, def string, isPast bool) (int64, error) {
	now := time.Now().Unix()
	in := strings.TrimSpace(arg)
	if in == "" {
		in = def
	}
	if in == "now" || in == "" {
		return now, nil
	}
	if u, err := strconv.ParseInt(in, 10, 64); err == nil {
		return u, nil
	}
	d, err := parseDurationDays(in)
	if err != nil {
		return 0, err
	}
	if isPast {
		return now - int64(d.Seconds()), nil
	}
	return now + int64(d.Seconds()), nil
}

// parseDurationDays wraps time.ParseDuration with a "Nd" extension. Operators
// reach for "7d" before "168h" — Go's parser rejects "d", so we strip and
// multiply locally. Defers to the stdlib for everything else (h, m, s, ms).
func parseDurationDays(s string) (time.Duration, error) {
	if strings.HasSuffix(s, "d") {
		n, err := strconv.Atoi(strings.TrimSuffix(s, "d"))
		if err != nil {
			return 0, fmt.Errorf("bad days suffix %q", s)
		}
		if n < 0 {
			return 0, fmt.Errorf("negative days %q", s)
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	return time.ParseDuration(s)
}

// parseContainerEvents decodes the NDJSON event stream that Docker's /events
// endpoint returns (one JSON object per line). Truncates at maxRows; malformed
// or blank lines are skipped rather than failing the whole call — daemons
// occasionally emit a trailing blank line or a partial record on connection
// close.
//
// Line-by-line scanning (vs. json.Decoder over the full stream) is the
// load-bearing choice: json.Decoder gets stuck on a malformed token because
// the decoder doesn't reliably advance past arbitrary garbage, even between
// well-formed records. bufio.Scanner with the default ScanLines splitter
// gives us deterministic per-record framing.
func parseContainerEvents(body []byte, maxRows int) (events []map[string]any, truncated bool) {
	events = make([]map[string]any, 0)
	sc := bufio.NewScanner(bytes.NewReader(body))
	// Default scanner buffer is 64 KB. Docker event records are small (well
	// under 4 KB), but a single misformatted log line with a huge embedded
	// label could blow the default — bump the cap so we don't truncate at
	// the framing layer.
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var ev dockerEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			continue
		}
		if len(events) >= maxRows {
			truncated = true
			continue
		}
		row := map[string]any{
			"ts":      ev.Time,
			"tsHuman": time.Unix(ev.Time, 0).UTC().Format(time.RFC3339),
			"action":  ev.Action,
		}
		if ev.Actor.Attributes != nil {
			if v, ok := ev.Actor.Attributes["exitCode"]; ok && v != "" {
				row["exitCode"] = v
			}
			if v, ok := ev.Actor.Attributes["signal"]; ok && v != "" {
				row["signal"] = v
			}
			// Curated attribute subset — the full map can include arbitrary
			// labels (Traefik blocks etc.) that blow the tool-result cap.
			attrs := map[string]string{}
			for _, k := range []string{"name", "image", "execID", "execDuration"} {
				if v, ok := ev.Actor.Attributes[k]; ok && v != "" {
					attrs[k] = v
				}
			}
			if len(attrs) > 0 {
				row["attributes"] = attrs
			}
		}
		events = append(events, row)
	}
	// We don't surface sc.Err() to the caller. The parser is best-effort
	// recovery, not strict NDJSON validation — the truncated flag captures
	// the "you may be missing rows" signal that operators act on.
	return events, truncated
}

// dockerEvent is the subset of a Docker /events record we surface. Field tags
// match the daemon's JSON exactly (capitalized Type/Action/Actor, lowercase
// time/timeNano) — the spec inverted this for unclear reasons.
type dockerEvent struct {
	Type   string `json:"Type"`
	Action string `json:"Action"`
	Time   int64  `json:"time"`
	Actor  struct {
		ID         string            `json:"ID"`
		Attributes map[string]string `json:"Attributes"`
	} `json:"Actor"`
}
