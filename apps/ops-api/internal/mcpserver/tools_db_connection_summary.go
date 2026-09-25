package mcpserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

const (
	dbConnSummaryTimeout    = 5 * time.Second
	dbConnSummaryScanCap    = 5000
	dbConnSummaryDefaultTop = 10
	dbConnSummaryHardMaxTop = 50
)

// dbConnSummaryEntry is one row of a faceted top-N list. Slice form is used
// instead of a map so the response is pre-sorted by count descending — the
// operator question "which user is hogging the pool?" should not require a
// client-side sort.
type dbConnSummaryEntry struct {
	Key   string `json:"key"`
	Count int    `json:"count"`
}

// RegisterDBConnectionSummaryTool registers `db_connection_summary` — an
// aggregation companion to `db_processlist`. Where db_processlist returns
// per-row triage, this tool collapses PROCESSLIST into five faceted top-N
// lists (byUser / byHost / byDb / byCommand / byState) in one round trip.
//
// Operator question: "where are the connections coming from / who's hogging
// the pool / what state are they stuck in?" — answered without paging
// through hundreds of individual rows. Pairs with db_processlist (drill into
// the noisy bucket) and db_global_status (rate vs. raw counts).
func RegisterDBConnectionSummaryTool(reg *Registry, deps DBDeps) {
	reg.Register(Tool{
		Name: "db_connection_summary",
		Description: "Aggregated MySQL connection snapshot from " +
			"information_schema.PROCESSLIST. Returns `total` (count of " +
			"connections after filters) plus five faceted top-N lists: " +
			"`byUser` / `byHost` / `byDb` / `byCommand` / `byState`. Each " +
			"list is pre-sorted by count descending (then key asc as " +
			"tiebreaker) and capped at `topN` (default 10, hard cap 50). " +
			"Host is normalized — the `:port` suffix MySQL stores on the " +
			"HOST column is stripped so multiple connections from the same " +
			"source ip don't fragment across high-cardinality buckets. " +
			"Defaults exclude 'Sleep' connections (same bias as " +
			"db_processlist — Sleep is 90%+ of PROCESSLIST during normal " +
			"operation and drowns out the diagnostic signal). Set " +
			"`includeSleep:true` to include them; `minTime` (seconds) " +
			"narrows to long-running only. Scan is capped at 5000 rows; " +
			"`truncated:true` flags when the cap was hit and the aggregation " +
			"is incomplete. Read-only, uses the ops_ro pool — sees all " +
			"connections only if ops_ro has the PROCESS privilege; without " +
			"it MySQL returns only ops_ro's own connections (still useful " +
			"for verifying ops-api's own activity). Pairs with " +
			"db_processlist (per-row drill-down on a hot bucket) and " +
			"db_global_status (rate-derived view).",
		InputSchema: json.RawMessage(`{"type":"object","properties":{
			"includeSleep":{"type":"boolean","description":"Include idle Sleep connections (default false)"},
			"minTime":{"type":"integer","description":"Minimum connection age in seconds (default 0)"},
			"topN":{"type":"integer","description":"Max entries per facet list (default 10, hard cap 50)"}
		}}`),
		Annotations: AnnRead(),
		Handler: func(ctx context.Context, raw json.RawMessage, _ string) any {
			var a struct {
				IncludeSleep bool  `json:"includeSleep"`
				MinTime      int64 `json:"minTime"`
				TopN         int   `json:"topN"`
			}
			if len(raw) > 0 && string(raw) != "null" {
				if err := json.Unmarshal(raw, &a); err != nil {
					return map[string]any{"error": "decode args: " + err.Error()}
				}
			}
			if deps.QueryDB == nil {
				return map[string]any{"error": "ops_ro pool not configured (DB_DSN empty?)"}
			}
			if a.TopN <= 0 {
				a.TopN = dbConnSummaryDefaultTop
			}
			if a.TopN > dbConnSummaryHardMaxTop {
				a.TopN = dbConnSummaryHardMaxTop
			}
			if a.MinTime < 0 {
				a.MinTime = 0
			}
			c, cancel := context.WithTimeout(ctx, dbConnSummaryTimeout)
			defer cancel()
			out, err := collectConnectionSummary(c, deps.QueryDB, a.IncludeSleep, a.MinTime, a.TopN)
			if err != nil {
				return map[string]any{"error": err.Error()}
			}
			return out
		},
	})
}

// dbConnFacetNames is the canonical facet order. Shared with
// db_connection_summary_growth so both tools name (and iterate) the five
// facets identically — a facet added here shows up in both responses.
var dbConnFacetNames = []string{"byUser", "byHost", "byDb", "byCommand", "byState"}

// dbConnFacetSnapshot is one PROCESSLIST scan aggregated into raw,
// UNTRUNCATED facet maps. db_connection_summary slices each map to topN for
// display; db_connection_summary_growth stores the whole thing, because a
// bucket outside today's topN can still be the one that grew (or vanished) by
// the next call — truncating before the diff would hide exactly the movement
// the growth lens exists to find.
type dbConnFacetSnapshot struct {
	Total     int
	Truncated bool
	ByUser    map[string]int
	ByHost    map[string]int
	ByDb      map[string]int
	ByCommand map[string]int
	ByState   map[string]int
}

// facet returns the map for a dbConnFacetNames entry, or nil for an unknown
// name. Lets callers walk the facets generically without reflection.
func (s *dbConnFacetSnapshot) facet(name string) map[string]int {
	switch name {
	case "byUser":
		return s.ByUser
	case "byHost":
		return s.ByHost
	case "byDb":
		return s.ByDb
	case "byCommand":
		return s.ByCommand
	case "byState":
		return s.ByState
	}
	return nil
}

// collectConnectionSummary materializes a facet scan as five top-N slices —
// the db_connection_summary response shape.
func collectConnectionSummary(ctx context.Context, db *sql.DB, includeSleep bool, minTime int64, topN int) (map[string]any, error) {
	s, err := collectConnectionFacets(ctx, db, includeSleep, minTime)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"total":        s.Total,
		"truncated":    s.Truncated,
		"scanCap":      dbConnSummaryScanCap,
		"byUser":       topNEntries(s.ByUser, topN),
		"byHost":       topNEntries(s.ByHost, topN),
		"byDb":         topNEntries(s.ByDb, topN),
		"byCommand":    topNEntries(s.ByCommand, topN),
		"byState":      topNEntries(s.ByState, topN),
		"includeSleep": includeSleep,
		"minTime":      minTime,
		"topN":         topN,
	}, nil
}

// collectConnectionFacets streams PROCESSLIST rows and aggregates them into
// five facet maps in a single pass. Aggregation is in Go (not SQL GROUP BY) so
// we get all five facets from one round trip without an UNION ALL or per-facet
// query. Scan is bounded by dbConnSummaryScanCap to keep memory pressure
// constant under a runaway PROCESSLIST.
func collectConnectionFacets(ctx context.Context, db *sql.DB, includeSleep bool, minTime int64) (*dbConnFacetSnapshot, error) {
	var q strings.Builder
	q.WriteString(`SELECT USER, HOST, IFNULL(DB,''), COMMAND, IFNULL(STATE,'')
		FROM information_schema.PROCESSLIST
		WHERE 1=1`)
	args := []any{}
	if !includeSleep {
		q.WriteString(" AND COMMAND != ?")
		args = append(args, "Sleep")
	}
	if minTime > 0 {
		q.WriteString(" AND TIME >= ?")
		args = append(args, minTime)
	}
	// scanCap+1 so we can detect the truncation without an extra COUNT(*).
	q.WriteString(" LIMIT ?")
	args = append(args, dbConnSummaryScanCap+1)

	rows, err := db.QueryContext(ctx, q.String(), args...)
	if err != nil {
		return nil, fmt.Errorf("query processlist: %w", err)
	}
	defer rows.Close()

	byUser := map[string]int{}
	byHost := map[string]int{}
	byDb := map[string]int{}
	byCommand := map[string]int{}
	byState := map[string]int{}
	total := 0
	truncated := false
	for rows.Next() {
		if total >= dbConnSummaryScanCap {
			truncated = true
			break
		}
		var user, host, dbName, command, state string
		if err := rows.Scan(&user, &host, &dbName, &command, &state); err != nil {
			return nil, fmt.Errorf("scan processlist: %w", err)
		}
		byUser[user]++
		byHost[normalizeHostKey(host)]++
		// Empty DB is a legitimate state (connections that haven't selected
		// one yet, replication threads, etc.) — bucket as "(none)" so it
		// surfaces in the facet instead of disappearing silently.
		byDb[orNone(dbName)]++
		byCommand[command]++
		byState[orNone(state)]++
		total++
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate processlist: %w", err)
	}
	return &dbConnFacetSnapshot{
		Total:     total,
		Truncated: truncated,
		ByUser:    byUser,
		ByHost:    byHost,
		ByDb:      byDb,
		ByCommand: byCommand,
		ByState:   byState,
	}, nil
}

// normalizeHostKey strips the `:port` suffix MySQL stores on PROCESSLIST.HOST
// so multiple connections from the same source aren't fragmented across
// per-connection port keys. Handles ipv4 (`172.17.0.5:55001` -> `172.17.0.5`)
// and bracketed ipv6 (`[::1]:55001` -> `[::1]`). Connections with no host
// (e.g. localhost socket connections returning "localhost") pass through
// unchanged.
func normalizeHostKey(host string) string {
	if host == "" {
		return "(none)"
	}
	// Bracketed ipv6: `[addr]:port` — keep through the closing bracket.
	if strings.HasPrefix(host, "[") {
		if idx := strings.Index(host, "]"); idx >= 0 {
			return host[:idx+1]
		}
		return host
	}
	// ipv4 or hostname: strip from the LAST colon. Only safe to strip when
	// there's EXACTLY one colon — multiple colons mean unbracketed ipv6
	// (`::1`, `fe80::abcd`) where stripping at the last colon would mangle
	// the address. MySQL normally brackets ipv6 in PROCESSLIST.HOST so the
	// multi-colon path is defensive, not load-bearing.
	if strings.Count(host, ":") == 1 {
		idx := strings.IndexByte(host, ':')
		port := host[idx+1:]
		// Defense: only strip if the suffix looks like a port (all digits).
		// A hostname like `host:something` shouldn't be truncated.
		if isAllDigits(port) {
			return host[:idx]
		}
	}
	return host
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}

// topNEntries sorts a facet map by count desc (key asc as tiebreaker) and
// returns the first `n` entries. Tiebreaker on key ensures the response is
// deterministic for tests; without it Go's map iteration randomization would
// make assertions flaky on ties.
func topNEntries(m map[string]int, n int) []dbConnSummaryEntry {
	out := make([]dbConnSummaryEntry, 0, len(m))
	for k, v := range m {
		out = append(out, dbConnSummaryEntry{Key: k, Count: v})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Key < out[j].Key
	})
	if len(out) > n {
		out = out[:n]
	}
	return out
}
