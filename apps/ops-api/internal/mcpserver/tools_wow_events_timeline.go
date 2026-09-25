package mcpserver

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
)

const (
	// wowEventsTimelineDefaultWindow is the lookback applied when the caller
	// passes no `since`. Matches container_events — operators investigating a
	// stack outage first ask "what happened in the last day".
	wowEventsTimelineDefaultWindow = "24h"

	// wowEventsTimelineMaxWindowSec caps the since→until span at 30d, same as
	// container_events. The Docker daemon's in-memory event log retains far
	// less than that in practice, but the cap stops an accidental "1y" from
	// forcing the daemon into three long allocation-heavy walks (one per
	// container in the triad).
	wowEventsTimelineMaxWindowSec = int64(30 * 24 * 3600)

	// wowEventsTimelineMaxRows caps BOTH the per-container parse and the merged
	// output. Used as the per-container cap so no single crash-looping container
	// (ac-worldserver during a tight restart loop emits hundreds of die/start
	// pairs a day) can starve the others out of the merged view, and reused as
	// the final merged cap so the response stays under the 25 KB MCP envelope.
	// Truncation keeps the OLDEST rows (matching container_events' scan-order
	// truncation) and sets `truncated:true`; the operator narrows `since` to see
	// a more recent slice. See mergeContainerEventTimeline.
	wowEventsTimelineMaxRows = 500
)

// wowEventsTimelineContainers is the AzerothCore stack triad the timeline
// merges by default. Ordered database → worldserver → authserver to read as the
// boot-dependency chain, though the merged output is sorted by timestamp, not
// by this slice. Overridable via the `containers` arg for ad-hoc multi-container
// correlation (e.g. adding wow-ops-api).
var wowEventsTimelineContainers = []string{"ac-database", "ac-worldserver", "ac-authserver"}

// RegisterWowEventsTimelineTool registers `wow_events_timeline` — a composite
// over the Docker daemon's event log that merges lifecycle events from the
// whole AzerothCore stack (ac-database + ac-worldserver + ac-authserver) into
// one timestamp-sorted, container-tagged sequence.
//
// `container_events` answers "what happened to THIS container"; diagnosing a
// stack outage means asking it three times and merging the results by hand
// (hold three responses in context, interleave by timestamp). This tool
// collapses that into one call and does the merge server-side, so the operator
// can read the causal ordering directly — e.g. ac-database `die` at T, then
// ac-worldserver `die` (exit 137) at T+2s is a DB-dependency cascade, not an
// independent worldserver OOM.
func RegisterWowEventsTimelineTool(reg *Registry, deps ContainerDeps) {
	reg.Register(Tool{
		Name: "wow_events_timeline",
		Description: "Merge Docker daemon lifecycle events across the whole AzerothCore stack " +
			"(ac-database + ac-worldserver + ac-authserver) into one timestamp-sorted, " +
			"container-tagged timeline. Each row carries ts/action/container plus exit code " +
			"and signal when present. Default window 24h, max 30d (capped further by the " +
			"daemon's event-log retention). Use this for cross-container outage forensics — " +
			"a ac-database `die` followed seconds later by an ac-worldserver `die` (exit code " +
			"137) is a DB-dependency cascade, which three separate `container_events` calls " +
			"only reveal after a manual client-side merge. Returns oldest→newest; when more " +
			"than 500 events match, the oldest are kept and `truncated:true` is set (narrow " +
			"`since` to see a recent slice). Pass `containers:[]` to override the default " +
			"triad, `eventTypes:[]` (empty array) to bypass the lifecycle filter. Read-only.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{
			"since":{"type":"string","description":"Lookback (e.g. \"24h\", \"7d\", \"30m\") or unix seconds (default 24h, max 30d)"},
			"until":{"type":"string","description":"End of window (duration or unix seconds, default now)"},
			"eventTypes":{"type":"array","items":{"type":"string"},"description":"Override default lifecycle filter (die/oom/kill/start/restart/stop/health_status). Empty array = all container events."},
			"containers":{"type":"array","items":{"type":"string"},"description":"Override the default AC triad (ac-database, ac-worldserver, ac-authserver). Empty array = default triad."}
		}}`),
		Annotations: AnnRead(),
		Handler: func(ctx context.Context, raw json.RawMessage, _ string) any {
			var a struct {
				Since      string   `json:"since"`
				Until      string   `json:"until"`
				EventTypes []string `json:"eventTypes"`
				Containers []string `json:"containers"`
			}
			_ = json.Unmarshal(raw, &a)

			untilUnix, err := resolveEventTimestamp(a.Until, "now", false)
			if err != nil {
				return map[string]any{"error": "until: " + err.Error()}
			}
			sinceUnix, err := resolveEventTimestamp(a.Since, wowEventsTimelineDefaultWindow, true)
			if err != nil {
				return map[string]any{"error": "since: " + err.Error()}
			}
			if sinceUnix >= untilUnix {
				return map[string]any{"error": "since must be < until", "since": sinceUnix, "until": untilUnix}
			}
			if untilUnix-sinceUnix > wowEventsTimelineMaxWindowSec {
				sinceUnix = untilUnix - wowEventsTimelineMaxWindowSec
			}

			// nil → defaults, empty/explicit → no filter. Same nil-vs-empty
			// distinction container_events relies on (the Anthropic SDK
			// serializes both forms and operators use the empty-array form to
			// see less-common events like exec_die).
			var eventTypes []string
			if a.EventTypes == nil {
				eventTypes = containerEventDefaults
			} else {
				eventTypes = a.EventTypes
			}

			containers := normalizeTimelineContainers(a.Containers)

			// Fetch + parse each container independently. A daemon-level error
			// on one container (rare — the /events endpoint serves history for
			// stopped containers too, so a down stack still returns rows) is
			// collected into fetchErrors and the merge proceeds with whatever
			// succeeded: a partial timeline beats no timeline during an outage.
			perContainer := make(map[string][]map[string]any, len(containers))
			var fetchErrors map[string]string
			for _, name := range containers {
				body, err := deps.Docker.EventsRaw(ctx, name, sinceUnix, untilUnix, eventTypes)
				if err != nil {
					if fetchErrors == nil {
						fetchErrors = map[string]string{}
					}
					fetchErrors[name] = err.Error()
					continue
				}
				evs, _ := parseContainerEvents(body, wowEventsTimelineMaxRows)
				perContainer[name] = evs
			}

			events, truncated, merged := mergeContainerEventTimeline(perContainer, wowEventsTimelineMaxRows)

			perContainerCounts := make(map[string]int, len(perContainer))
			for name, evs := range perContainer {
				perContainerCounts[name] = len(evs)
			}

			resp := map[string]any{
				"containers":         containers,
				"since":              sinceUnix,
				"until":              untilUnix,
				"eventTypes":         eventTypes,
				"events":             events,
				"count":              len(events),
				"merged":             merged,
				"truncated":          truncated,
				"perContainerCounts": perContainerCounts,
			}
			if fetchErrors != nil {
				resp["fetchErrors"] = fetchErrors
			}
			return resp
		},
	})
}

// normalizeTimelineContainers resolves the caller's `containers` arg to the set
// the timeline will query. nil/empty → the default AC triad. A supplied list is
// trimmed, de-duplicated (preserving first-seen order so the response echoes a
// stable set), and empty entries dropped; if cleaning leaves nothing, it falls
// back to the triad. De-duplication is load-bearing: a name listed twice would
// otherwise be fetched twice and double-count every one of its events in the
// merged timeline.
func normalizeTimelineContainers(in []string) []string {
	if len(in) == 0 {
		return wowEventsTimelineContainers
	}
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, c := range in {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		if _, dup := seen[c]; dup {
			continue
		}
		seen[c] = struct{}{}
		out = append(out, c)
	}
	if len(out) == 0 {
		return wowEventsTimelineContainers
	}
	return out
}

// mergeContainerEventTimeline interleaves the per-container event slices (as
// produced by parseContainerEvents) into one timestamp-sorted timeline, tagging
// each row with its source `container`. Returns the (possibly truncated) merged
// rows oldest→newest, the truncated flag, and `merged` — the total number of
// events that entered the merge (post per-container cap).
//
// Sort order is (ts asc, container asc, seq asc):
//   - ts asc gives the causal reading operators want.
//   - container asc groups same-second events from different containers
//     deterministically (without it Go's randomized map iteration would flake
//     tests on ties).
//   - seq asc is a monotonic per-input index that preserves each container's
//     daemon emission order for same-second same-container events (e.g. an `oom`
//     and a `die` that land in the same second stay oom-before-die).
//
// Truncation keeps the OLDEST maxRows and sets truncated=true, matching
// container_events' scan-order truncation so the two tools behave consistently;
// the operator narrows `since` to surface a more recent slice.
func mergeContainerEventTimeline(perContainer map[string][]map[string]any, maxRows int) (events []map[string]any, truncated bool, merged int) {
	names := make([]string, 0, len(perContainer))
	for n := range perContainer {
		names = append(names, n)
	}
	sort.Strings(names)

	type taggedEvent struct {
		row       map[string]any
		ts        int64
		container string
		seq       int
	}

	all := make([]taggedEvent, 0)
	seq := 0
	for _, name := range names {
		for _, row := range perContainer[name] {
			// parseContainerEvents returns fresh maps each call, so tagging in
			// place is safe (no aliasing across callers).
			row["container"] = name
			ts, _ := row["ts"].(int64)
			all = append(all, taggedEvent{row: row, ts: ts, container: name, seq: seq})
			seq++
		}
	}
	merged = len(all)

	sort.SliceStable(all, func(i, j int) bool {
		if all[i].ts != all[j].ts {
			return all[i].ts < all[j].ts
		}
		if all[i].container != all[j].container {
			return all[i].container < all[j].container
		}
		return all[i].seq < all[j].seq
	})

	events = make([]map[string]any, 0, len(all))
	for i := range all {
		if maxRows > 0 && len(events) >= maxRows {
			truncated = true
			break
		}
		events = append(events, all[i].row)
	}
	return events, truncated, merged
}
