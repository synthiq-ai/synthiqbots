package mcpserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
)

const (
	dbConnGrowthTimeout    = 5 * time.Second
	dbConnGrowthDefaultTop = 10
	dbConnGrowthHardMaxTop = 50
	// dbConnGrowthRingCap bounds the in-process snapshot history. Each stored
	// snapshot is five small facet maps (PROCESSLIST cardinality, not row
	// count), so 20 of them is negligible memory next to the scan itself.
	dbConnGrowthRingCap = 20
)

// dbConnGrowthSortKeys is the Go-side sortBy allowlist. Every facet list is
// sorted in Go over the diffed union, so there is no SQL ORDER BY to protect —
// the allowlist exists so a typo'd key errors instead of silently falling back
// to a default ordering the operator didn't ask for.
var dbConnGrowthSortKeys = map[string]bool{
	"growth": true,
	"shrink": true,
	"count":  true,
}

// errDBConnSnapshotNotFound is returned when an explicit snapshotId is not in
// the ring — either it was evicted or it never existed.
var errDBConnSnapshotNotFound = errors.New("snapshot not found")

// dbConnGrowthEntry is one facet bucket's before/after pair. PrevCount is
// carried alongside Delta so the operator can tell "8 -> 40" (a real leak)
// from "0 -> 32" (a pool that simply connected), which a bare delta can't.
// A key present in the baseline but gone now surfaces with Count 0 and a
// negative Delta rather than disappearing — a bucket that VANISHED is signal
// (worker died / pool drained), not absence of signal.
type dbConnGrowthEntry struct {
	Key       string `json:"key"`
	Count     int    `json:"count"`
	PrevCount int    `json:"prevCount"`
	Delta     int    `json:"delta"`
}

// dbConnStoredSnapshot is one recorded PROCESSLIST aggregation plus the filter
// parameters it was taken under. The filters are stored because a snapshot
// taken with includeSleep=true is not comparable to one taken without it —
// diffing across that boundary would report the entire Sleep population as
// "growth".
type dbConnStoredSnapshot struct {
	ID           string
	TakenAt      time.Time
	IncludeSleep bool
	MinTime      int64
	Facets       *dbConnFacetSnapshot
}

// filterKey is the comparability signature: two snapshots are apples-to-apples
// only when both were scanned under the same filters.
func (s *dbConnStoredSnapshot) filterKey() string {
	return fmt.Sprintf("sleep=%t;minTime=%d", s.IncludeSleep, s.MinTime)
}

// dbConnSnapshotRing is the in-process snapshot history: a bounded, oldest-first
// ring guarded by a mutex, in the same shape as the rateLimiter's per-process
// state. It is deliberately GLOBAL rather than per-client — read tools have no
// auth gate beyond the bearer token, so there is no durable client identity to
// key on, and an operator investigating a leak may well call from more than one
// session. Snapshots are process-local and vanish on restart; that is fine for
// a "call twice N minutes apart" workflow and keeps the tool free of storage.
type dbConnSnapshotRing struct {
	mu    sync.Mutex
	cap   int
	seq   int
	snaps []*dbConnStoredSnapshot // oldest first
}

func newDBConnSnapshotRing(capacity int) *dbConnSnapshotRing {
	if capacity <= 0 {
		capacity = dbConnGrowthRingCap
	}
	return &dbConnSnapshotRing{cap: capacity}
}

// dbConnGrowthRing is the process-wide history the registered tool records
// into. Tests drive the pure paths with their own ring so they never touch it.
var dbConnGrowthRing = newDBConnSnapshotRing(dbConnGrowthRingCap)

// record stores a freshly scanned snapshot and resolves its baseline in ONE
// critical section, so the baseline is always the ring state as it was BEFORE
// this snapshot landed (a concurrent caller can never make a snapshot its own
// baseline). When wantID is empty the baseline is the most recent stored
// snapshot with a matching filter signature; otherwise it is that exact id,
// whose filters may differ — the caller flags the mismatch rather than
// refusing, since comparing across filters is occasionally what an operator
// actually wants.
func (r *dbConnSnapshotRing) record(snap *dbConnStoredSnapshot, wantID string) (*dbConnStoredSnapshot, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	var baseline *dbConnStoredSnapshot
	if wantID != "" {
		for _, s := range r.snaps {
			if s.ID == wantID {
				baseline = s
				break
			}
		}
		if baseline == nil {
			return nil, fmt.Errorf("%w: %q", errDBConnSnapshotNotFound, wantID)
		}
	} else {
		key := snap.filterKey()
		// Walk newest-first: the most recent comparable snapshot wins.
		for i := len(r.snaps) - 1; i >= 0; i-- {
			if r.snaps[i].filterKey() == key {
				baseline = r.snaps[i]
				break
			}
		}
	}

	r.seq++
	snap.ID = fmt.Sprintf("snap-%d", r.seq)
	r.snaps = append(r.snaps, snap)
	if len(r.snaps) > r.cap {
		// Drop the oldest. Copy into a fresh slice rather than resliceing so
		// the evicted pointer isn't kept alive by the backing array.
		trimmed := make([]*dbConnStoredSnapshot, r.cap)
		copy(trimmed, r.snaps[len(r.snaps)-r.cap:])
		r.snaps = trimmed
	}
	return baseline, nil
}

// list summarizes the stored snapshots newest-first so the operator can pick an
// explicit snapshotId for the next call without holding ids from prior
// responses in context.
func (r *dbConnSnapshotRing) list() []map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]map[string]any, 0, len(r.snaps))
	for i := len(r.snaps) - 1; i >= 0; i-- {
		s := r.snaps[i]
		out = append(out, map[string]any{
			"id":           s.ID,
			"takenAt":      s.TakenAt.UTC().Format(time.RFC3339),
			"total":        s.Facets.Total,
			"includeSleep": s.IncludeSleep,
			"minTime":      s.MinTime,
		})
	}
	return out
}

// RegisterDBConnectionSummaryGrowthTool registers
// `db_connection_summary_growth` — the delta lens over db_connection_summary.
//
// db_connection_summary answers "who is connected right now". Capacity and
// leak investigations ask the harder question "what is GROWING", which today
// costs two calls plus a client-side diff: the agent must hold the first
// response in context (tokens) and subtract by hand across five facets, which
// is exactly the kind of arithmetic it quietly gets wrong. This tool records
// each scan in a small in-process ring and returns the movement against the
// previous comparable scan directly — call it, wait, call it again.
func RegisterDBConnectionSummaryGrowthTool(reg *Registry, deps DBDeps) {
	reg.Register(Tool{
		Name: "db_connection_summary_growth",
		Description: "Connection GROWTH lens: takes a fresh information_schema.PROCESSLIST snapshot and diffs it " +
			"against a previously recorded one, so \"what is growing?\" is one call instead of two calls plus a " +
			"client-side subtraction. Same five facets as db_connection_summary (byUser / byHost / byDb / " +
			"byCommand / byState), but each entry carries count, prevCount and delta. A bucket present in the " +
			"baseline and gone now reports count 0 with a negative delta rather than vanishing — a drained pool " +
			"is signal too. Snapshots live in a process-local ring (last 20, ids snap-N, lost on restart); every " +
			"call records one and returns `snapshotId` plus `storedSnapshots` so the next call can pin an explicit " +
			"baseline. By default the baseline is the most recent snapshot taken with the SAME includeSleep/minTime " +
			"filters — a snapshot taken under different filters is not comparable (the whole Sleep population would " +
			"read as growth); passing `snapshotId` overrides that and sets baselineFilterMismatch:true if the " +
			"filters differ. The FIRST call has no baseline: firstSnapshot:true, deltas equal the raw counts, and " +
			"the operator should call again after an interval. Honest per-facet key totals (facetKeysTotal) count " +
			"the full diffed union before the topN cut. Args: topN (default 10, hard cap 50), sortBy (growth " +
			"[default, delta desc] | shrink [delta asc, biggest drains first] | count [current count desc]; an " +
			"unknown key is rejected), snapshotId (optional explicit baseline; an evicted/unknown id is an error), " +
			"includeSleep (default false, same bias as db_connection_summary), minTime (seconds, narrows to " +
			"long-running). Scan capped at 5000 rows; truncated:true flags an incomplete aggregation on either " +
			"side. Read-only, uses the ops_ro pool (needs the PROCESS privilege to see other users' connections). " +
			"Pairs with db_connection_summary (point-in-time detail), db_processlist (per-row drill-down on the " +
			"bucket that grew) and db_global_status (rate-derived view).",
		InputSchema: json.RawMessage(`{"type":"object","properties":{
			"topN":{"type":"integer","description":"Max entries per facet list (default 10, hard cap 50)"},
			"sortBy":{"type":"string","enum":["growth","shrink","count"],"description":"growth=delta desc (default), shrink=delta asc (biggest drains first), count=current count desc"},
			"snapshotId":{"type":"string","description":"Compare against this stored snapshot id (from storedSnapshots) instead of the most recent comparable one"},
			"includeSleep":{"type":"boolean","description":"Include idle Sleep connections (default false); only snapshots sharing this setting are comparable"},
			"minTime":{"type":"integer","description":"Minimum connection age in seconds (default 0); only snapshots sharing this setting are comparable"}
		}}`),
		Annotations: AnnRead(),
		Handler: func(ctx context.Context, raw json.RawMessage, _ string) any {
			var a struct {
				TopN         int    `json:"topN"`
				SortBy       string `json:"sortBy"`
				SnapshotID   string `json:"snapshotId"`
				IncludeSleep bool   `json:"includeSleep"`
				MinTime      int64  `json:"minTime"`
			}
			if len(raw) > 0 && string(raw) != "null" {
				if err := json.Unmarshal(raw, &a); err != nil {
					return map[string]any{"error": "decode args: " + err.Error()}
				}
			}
			if deps.QueryDB == nil {
				return map[string]any{"error": "ops_ro pool not configured (DB_DSN empty?)"}
			}
			topN := a.TopN
			if topN <= 0 {
				topN = dbConnGrowthDefaultTop
			}
			if topN > dbConnGrowthHardMaxTop {
				topN = dbConnGrowthHardMaxTop
			}
			// sortBy resolves to an allowlist key; empty -> "growth". An
			// unknown key is rejected before the scan so a typo reads as a bad
			// argument rather than a silently re-ordered leaderboard.
			sortKey := a.SortBy
			if sortKey == "" {
				sortKey = "growth"
			}
			if !dbConnGrowthSortKeys[sortKey] {
				return map[string]any{"error": fmt.Sprintf("invalid sortBy: %q (valid: growth, shrink, count)", a.SortBy)}
			}
			if a.MinTime < 0 {
				a.MinTime = 0
			}
			c, cancel := context.WithTimeout(ctx, dbConnGrowthTimeout)
			defer cancel()
			out, err := collectConnectionGrowth(c, deps.QueryDB, dbConnGrowthRing, time.Now(),
				a.IncludeSleep, a.MinTime, topN, sortKey, a.SnapshotID)
			if err != nil {
				return map[string]any{"error": err.Error()}
			}
			return out
		},
	})
}

// collectConnectionGrowth scans PROCESSLIST, records the snapshot in the ring,
// and diffs it against the resolved baseline. Split out (and taking the ring +
// a fixed `now`) so tests can drive the whole path with sqlmock and their own
// isolated history.
func collectConnectionGrowth(ctx context.Context, db *sql.DB, ring *dbConnSnapshotRing, now time.Time,
	includeSleep bool, minTime int64, topN int, sortKey, wantID string) (map[string]any, error) {

	facets, err := collectConnectionFacets(ctx, db, includeSleep, minTime)
	if err != nil {
		return nil, err
	}
	snap := &dbConnStoredSnapshot{
		TakenAt:      now,
		IncludeSleep: includeSleep,
		MinTime:      minTime,
		Facets:       facets,
	}
	baseline, err := ring.record(snap, wantID)
	if err != nil {
		if errors.Is(err, errDBConnSnapshotNotFound) {
			// The scan already happened and the snapshot was NOT recorded, so
			// say so plainly — otherwise the operator retries against an id
			// that will never appear.
			return nil, fmt.Errorf("%w (ring holds the last %d snapshots; call without snapshotId to seed a baseline)", err, ring.cap)
		}
		return nil, err
	}

	notes := []string{}
	// An empty baseline diffs as all-zeros, which makes every delta equal the
	// current count. That is arithmetically right but reads like growth, so
	// the first call says so explicitly instead of implying a spike.
	var baseFacets *dbConnFacetSnapshot
	out := map[string]any{
		"snapshotId":             snap.ID,
		"takenAt":                now.UTC().Format(time.RFC3339),
		"total":                  facets.Total,
		"truncated":              facets.Truncated,
		"scanCap":                dbConnSummaryScanCap,
		"includeSleep":           includeSleep,
		"minTime":                minTime,
		"topN":                   topN,
		"sortBy":                 sortKey,
		"ringCap":                ring.cap,
		"firstSnapshot":          baseline == nil,
		"baselineFilterMismatch": false,
	}
	if baseline == nil {
		out["comparedTo"] = nil
		out["baselineTakenAt"] = nil
		out["baselineTotal"] = 0
		out["baselineTruncated"] = false
		out["intervalSeconds"] = 0
		out["totalDelta"] = facets.Total
		notes = append(notes, "no comparable prior snapshot: deltas equal current counts. Call again after an interval to get real movement.")
	} else {
		baseFacets = baseline.Facets
		mismatch := baseline.filterKey() != snap.filterKey()
		out["comparedTo"] = baseline.ID
		out["baselineTakenAt"] = baseline.TakenAt.UTC().Format(time.RFC3339)
		out["baselineTotal"] = baseFacets.Total
		out["baselineTruncated"] = baseFacets.Truncated
		out["intervalSeconds"] = int(now.Sub(baseline.TakenAt).Seconds())
		out["totalDelta"] = facets.Total - baseFacets.Total
		out["baselineFilterMismatch"] = mismatch
		if mismatch {
			notes = append(notes, fmt.Sprintf("baseline %s was taken with includeSleep=%t minTime=%d; deltas are not apples-to-apples",
				baseline.ID, baseline.IncludeSleep, baseline.MinTime))
		}
		if baseFacets.Truncated || facets.Truncated {
			notes = append(notes, "a scan hit the row cap; deltas on truncated facets are incomplete")
		}
	}

	facetKeysTotal := map[string]int{}
	for _, name := range dbConnFacetNames {
		entries := diffConnFacet(facets.facet(name), baselineFacet(baseFacets, name), sortKey)
		facetKeysTotal[name] = len(entries)
		if len(entries) > topN {
			entries = entries[:topN]
		}
		out[name] = entries
	}
	out["facetKeysTotal"] = facetKeysTotal
	out["storedSnapshots"] = ring.list()
	out["notes"] = notes
	return out, nil
}

// baselineFacet returns a facet map from a possibly-nil snapshot, so the
// first-call path diffs against an empty baseline without a special case.
func baselineFacet(s *dbConnFacetSnapshot, name string) map[string]int {
	if s == nil {
		return nil
	}
	return s.facet(name)
}

// diffConnFacet joins one facet's current and baseline maps on their key UNION
// and sorts by the requested key. The union (not just the current keys) is what
// keeps a vanished bucket visible. Returns every key — the caller slices to
// topN after recording the honest pre-cut total.
func diffConnFacet(cur, prev map[string]int, sortKey string) []dbConnGrowthEntry {
	seen := make(map[string]bool, len(cur)+len(prev))
	out := make([]dbConnGrowthEntry, 0, len(cur)+len(prev))
	add := func(key string) {
		if seen[key] {
			return
		}
		seen[key] = true
		c, p := cur[key], prev[key]
		out = append(out, dbConnGrowthEntry{Key: key, Count: c, PrevCount: p, Delta: c - p})
	}
	for k := range cur {
		add(k)
	}
	for k := range prev {
		add(k)
	}
	// Every ordering ends in key-asc so ties are deterministic — without it
	// Go's map iteration randomization would reshuffle equal rows per call.
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		switch sortKey {
		case "shrink":
			// Biggest drains first; among equal drains the bucket that WAS
			// bigger matters more.
			if a.Delta != b.Delta {
				return a.Delta < b.Delta
			}
			if a.PrevCount != b.PrevCount {
				return a.PrevCount > b.PrevCount
			}
		case "count":
			if a.Count != b.Count {
				return a.Count > b.Count
			}
			if a.Delta != b.Delta {
				return a.Delta > b.Delta
			}
		default: // "growth"
			// Fastest growth first; among equal growth the bucket that is now
			// bigger is the more urgent one.
			if a.Delta != b.Delta {
				return a.Delta > b.Delta
			}
			if a.Count != b.Count {
				return a.Count > b.Count
			}
		}
		return a.Key < b.Key
	})
	return out
}
