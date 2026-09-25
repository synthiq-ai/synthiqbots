package mcpserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	dbGSGrowthTimeout    = 5 * time.Second
	dbGSGrowthDefaultTop = 15
	dbGSGrowthHardMaxTop = 100
	// dbGSGrowthRingCap bounds the in-process snapshot history. Each stored
	// snapshot is one map[string]int64 of ~450 entries (SHOW GLOBAL STATUS
	// cardinality is fixed by the server build, not by workload), so 20 of them
	// is a few hundred KB at worst.
	dbGSGrowthRingCap = 20
	// dbGSMaxVars caps how many status rows we will read. SHOW GLOBAL STATUS
	// takes no LIMIT, so the guard has to live on the scan loop. A stock MySQL 8
	// returns ~480 rows; 5000 leaves room for a plugin-heavy Percona build while
	// still bounding a pathological response.
	dbGSMaxVars = 5000
)

// dbGSGrowthSortKeys is the Go-side sortBy allowlist. Every list is sorted in
// Go over the diffed union, so there is no SQL ORDER BY to protect — the
// allowlist exists so a typo errors instead of silently falling back to an
// ordering the operator did not ask for.
var dbGSGrowthSortKeys = map[string]bool{
	"rate":  true,
	"delta": true,
	"drop":  true,
	"name":  true,
}

// errDBGSSnapshotNotFound is returned when an explicit snapshotId is not in the
// ring — either it was evicted or it never existed.
var errDBGSSnapshotNotFound = errors.New("snapshot not found")

// dbGSGaugeVars is the EXACT-MATCH allowlist of GLOBAL STATUS variables that
// are GAUGES (a current level) rather than CUMULATIVE COUNTERS (a lifetime
// total). Anything absent from this list is treated as a counter, which is the
// safe default: the overwhelming majority of status variables are counters, and
// misreading a counter as a gauge would merely omit its rate, whereas the
// reverse would publish a nonsense "gauge per second".
//
// This is deliberately exact-match rather than prefix-match, because the
// obvious prefix families are MIXED and a prefix rule silently misclassifies:
//   - "Threads_" — Threads_connected/running/cached are gauges, but
//     Threads_created is a cumulative counter (threads spawned since start).
//   - "Innodb_buffer_pool_pages_" — total/free/dirty/data/misc/latched are
//     gauges, but Innodb_buffer_pool_pages_flushed is a counter (flush requests
//     since start).
//   - "Qcache_" / "Slave_" — free_blocks and open_temp_tables are gauges while
//     hits and retried_transactions are counters.
//
// Both mixed families are covered by tests so a future edit that "simplifies"
// this into a prefix scan fails loudly.
var dbGSGaugeVars = map[string]bool{
	// Server / connection levels.
	"Uptime":                    true,
	"Uptime_since_flush_status": true,
	"Threads_connected":         true,
	"Threads_running":           true,
	"Threads_cached":            true,
	"Max_used_connections":      true,
	"Prepared_stmt_count":       true,
	// Open-object levels (the "_ed" siblings — Opened_tables, Opened_files —
	// are counters and are deliberately absent here).
	"Open_files":             true,
	"Open_streams":           true,
	"Open_tables":            true,
	"Open_table_definitions": true,
	// InnoDB buffer pool levels.
	"Innodb_buffer_pool_pages_total":   true,
	"Innodb_buffer_pool_pages_free":    true,
	"Innodb_buffer_pool_pages_dirty":   true,
	"Innodb_buffer_pool_pages_data":    true,
	"Innodb_buffer_pool_pages_misc":    true,
	"Innodb_buffer_pool_pages_latched": true,
	"Innodb_buffer_pool_bytes_data":    true,
	"Innodb_buffer_pool_bytes_dirty":   true,
	// InnoDB instantaneous levels.
	"Innodb_row_lock_current_waits": true,
	"Innodb_page_size":              true,
	"Innodb_num_open_files":         true,
	// Key cache levels.
	"Key_blocks_not_flushed": true,
	"Key_blocks_unused":      true,
	"Key_blocks_used":        true,
	// Query cache levels (MariaDB / MySQL 5.7).
	"Qcache_free_blocks":      true,
	"Qcache_free_memory":      true,
	"Qcache_queries_in_cache": true,
	"Qcache_total_blocks":     true,
	// Replication levels.
	"Slave_open_temp_tables": true,
	// TLS session cache level.
	"Ssl_session_cache_size": true,
}

// dbGSCuratedSet is the `curatedOnly` filter's membership test, built once from
// db_global_status's curatedStatusKeys so the two tools cannot drift — the
// curated view here is by construction the same ~30 counters that tool already
// surfaces point-in-time.
var (
	dbGSCuratedOnce sync.Once
	dbGSCuratedSet  map[string]bool
)

func dbGSCurated() map[string]bool {
	dbGSCuratedOnce.Do(func() {
		dbGSCuratedSet = make(map[string]bool, len(curatedStatusKeys))
		for _, k := range curatedStatusKeys {
			dbGSCuratedSet[k] = true
		}
	})
	return dbGSCuratedSet
}

// dbGSKind classifies a status variable for rate purposes.
func dbGSKind(name string) string {
	if dbGSGaugeVars[name] {
		return "gauge"
	}
	return "counter"
}

// dbGSGrowthEntry is one status variable's before/after pair.
//
// Every numeric field is a POINTER because "unknown" and "zero" are different
// answers here and conflating them is the exact misreading this tool exists to
// prevent:
//   - Value nil       — the variable VANISHED between snapshots (plugin
//     unloaded, server rebuilt). It is not 0; we simply have no reading.
//   - PrevValue nil   — the variable is NEW since the baseline; its lifetime
//     total did not accrue during this interval.
//   - Delta nil       — not computable (first call, new/vanished variable, or a
//     counter across a server restart).
//   - PerSecond nil   — no rate: gauges never have one, and counters lose theirs
//     when the interval or the diff is unusable.
type dbGSGrowthEntry struct {
	Variable  string   `json:"variable"`
	Kind      string   `json:"kind"`
	Value     *int64   `json:"value"`
	PrevValue *int64   `json:"prevValue"`
	Delta     *int64   `json:"delta"`
	PerSecond *float64 `json:"perSecond"`
	PerHour   *float64 `json:"perHour"`
}

// dbGSStoredSnapshot is one recorded SHOW GLOBAL STATUS reading.
//
// Unlike db_connection_summary_growth's snapshot there is no filter signature
// to carry: SHOW GLOBAL STATUS takes no arguments, so the scan is identical on
// every call and EVERY snapshot is comparable to every other one. The tool's
// prefix / curatedOnly / includeUnchanged arguments filter the DISPLAY only —
// they are applied after the diff, never to the stored reading — so changing
// them between calls can never fabricate movement.
type dbGSStoredSnapshot struct {
	ID        string
	TakenAt   time.Time
	Uptime    int64
	Truncated bool
	Vars      map[string]int64
}

// dbGSSnapshotRing is the in-process snapshot history: a bounded, oldest-first
// ring guarded by a mutex, in the same shape as db_connection_summary_growth's
// dbConnSnapshotRing and the rateLimiter's per-process state. Deliberately
// global rather than per-client — read tools have no auth gate beyond the
// bearer token, so there is no durable client identity to key on. Snapshots are
// process-local and vanish on restart, which is fine for a "call twice N
// minutes apart" workflow and keeps the tool free of storage.
//
// This is a deliberate SIBLING of dbConnSnapshotRing rather than a shared
// generic: that ring is typed to *dbConnFacetSnapshot and carries a filter
// comparability signature that has no analogue here. Two call sites is not yet
// enough shape agreement to justify the abstraction; a third should trigger it.
type dbGSSnapshotRing struct {
	mu    sync.Mutex
	cap   int
	seq   int
	snaps []*dbGSStoredSnapshot // oldest first
}

func newDBGSSnapshotRing(capacity int) *dbGSSnapshotRing {
	if capacity <= 0 {
		capacity = dbGSGrowthRingCap
	}
	return &dbGSSnapshotRing{cap: capacity}
}

// dbGSGrowthRing is the process-wide history the registered tool records into.
// Tests drive the pure paths with their own ring so they never touch it.
var dbGSGrowthRing = newDBGSSnapshotRing(dbGSGrowthRingCap)

// record stores a freshly scanned snapshot and resolves its baseline in ONE
// critical section, so the baseline is always the ring state as it was BEFORE
// this snapshot landed — a concurrent caller can never become its own baseline.
// When wantID is empty the baseline is the most recent stored snapshot;
// otherwise it is that exact id.
func (r *dbGSSnapshotRing) record(snap *dbGSStoredSnapshot, wantID string) (*dbGSStoredSnapshot, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	var baseline *dbGSStoredSnapshot
	if wantID != "" {
		for _, s := range r.snaps {
			if s.ID == wantID {
				baseline = s
				break
			}
		}
		if baseline == nil {
			return nil, fmt.Errorf("%w: %q", errDBGSSnapshotNotFound, wantID)
		}
	} else if n := len(r.snaps); n > 0 {
		baseline = r.snaps[n-1]
	}

	r.seq++
	snap.ID = fmt.Sprintf("gs-%d", r.seq)
	r.snaps = append(r.snaps, snap)
	if len(r.snaps) > r.cap {
		// Drop the oldest. Copy into a fresh slice rather than resliceing so the
		// evicted pointer isn't kept alive by the backing array.
		trimmed := make([]*dbGSStoredSnapshot, r.cap)
		copy(trimmed, r.snaps[len(r.snaps)-r.cap:])
		r.snaps = trimmed
	}
	return baseline, nil
}

// list summarizes the stored snapshots newest-first so the operator can pick an
// explicit snapshotId for the next call without holding ids from prior
// responses in context.
func (r *dbGSSnapshotRing) list() []map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]map[string]any, 0, len(r.snaps))
	for i := len(r.snaps) - 1; i >= 0; i-- {
		s := r.snaps[i]
		out = append(out, map[string]any{
			"id":            s.ID,
			"takenAt":       s.TakenAt.UTC().Format(time.RFC3339),
			"uptimeSeconds": s.Uptime,
			"variables":     len(s.Vars),
		})
	}
	return out
}

// latest returns the most recently stored snapshot, or nil when the ring is
// empty. READ-ONLY on purpose: db_global_status consults this to check its
// counters against a real interval instead of the lifetime average, and it must
// NOT record — if both tools appended, an operator alternating between them
// would keep resetting the other's baseline and every reported rate would
// collapse to the gap between the two calls.
//
// The returned pointer is shared with the ring rather than copied. That is safe
// because a snapshot is immutable once recorded: collectGlobalStatusGrowth
// builds Vars, hands the snapshot to record, and from then on only reads it.
// Any future edit that mutates a stored snapshot in place must copy here first.
func (r *dbGSSnapshotRing) latest() *dbGSStoredSnapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	if n := len(r.snaps); n > 0 {
		return r.snaps[n-1]
	}
	return nil
}

// RegisterDBGlobalStatusGrowthTool registers `db_global_status_growth` — the
// rate lens over db_global_status.
//
// Almost everything in SHOW GLOBAL STATUS is a counter that has been
// accumulating since the server started, so a point-in-time read answers very
// little on its own: "Aborted_connects = 48213" is meaningless without knowing
// whether that accrued over ninety days or the last four minutes. The
// diagnostic an operator actually wants is the RATE, which today costs two
// calls, a remembered interval, and hand arithmetic across ~30 counters — the
// kind of subtraction an agent quietly gets wrong. This tool records each read
// in a small in-process ring and returns the movement against the previous read
// directly: call it, wait, call it again.
func RegisterDBGlobalStatusGrowthTool(reg *Registry, deps DBDeps) {
	reg.Register(Tool{
		Name: "db_global_status_growth",
		Description: "GLOBAL STATUS rate lens: takes a fresh SHOW GLOBAL STATUS reading and diffs it against a " +
			"previously recorded one, turning cumulative-since-start counters into per-interval rates. " +
			"db_global_status returns the raw lifetime totals, which answer almost nothing on their own " +
			"(\"Aborted_connects=48213\" could be ninety days or four minutes); this makes \"what is actually " +
			"happening RIGHT NOW\" one call instead of two calls plus hand arithmetic. Each entry carries " +
			"variable, kind, value, prevValue, delta, perSecond and perHour. `kind` is counter or gauge from an " +
			"exact-match gauge allowlist (Threads_connected/running/cached, Uptime, Open_*, " +
			"Innodb_buffer_pool_pages_total/free/dirty/data, Innodb_row_lock_current_waits, Key_blocks_*, " +
			"Qcache_free_*, Slave_open_temp_tables, Max_used_connections); a rate on a gauge is nonsense so " +
			"perSecond/perHour are emitted for COUNTERS only, while delta is emitted for both. Note the " +
			"allowlist is exact-match on purpose: Threads_created and Innodb_buffer_pool_pages_flushed are " +
			"counters living inside otherwise-gauge prefix families. Rates are computed against the server's " +
			"OWN Uptime delta when available (rateBasis \"uptime\") rather than the wall clock (rateBasis " +
			"\"wallClock\"), so client/server clock skew cannot distort them. SERVER RESTART DETECTION: if " +
			"Uptime went backwards, or any counter present in both readings decreased, serverRestarted:true is " +
			"set and every counter delta/rate is suppressed rather than reported as a negative or a garbage " +
			"spike (gauge deltas survive, being independent point-in-time levels). value/prevValue/delta are " +
			"null rather than 0 when unknown: a variable that VANISHED between readings has no current value, " +
			"and one that is NEW has no prior — neither is zero, and a vanished variable never trips the " +
			"restart heuristic. The FIRST call has no baseline: firstSnapshot:true, all deltas null, and the " +
			"operator should call again after an interval. Snapshots live in a process-local ring (last 20, " +
			"ids gs-N, lost on restart); every call records one and returns snapshotId plus storedSnapshots so " +
			"the next call can pin an explicit baseline via snapshotId. Unlike db_connection_summary_growth " +
			"there is no filter comparability to worry about — SHOW GLOBAL STATUS takes no arguments, so every " +
			"snapshot is comparable and the display filters are applied after the diff. Honest totals " +
			"(variablesScanned, countersTracked, gaugesTracked, variablesMoved, variablesNew, " +
			"variablesVanished, variablesMatched) are counted over the full diffed union before the topN cut. " +
			"Args: topN (default 15, hard cap 100), sortBy (rate [default, perSecond desc] | delta [delta " +
			"desc] | drop [delta asc, biggest decreases first] | name; an unknown key is rejected), snapshotId " +
			"(optional explicit baseline; an evicted/unknown id is an error), prefix (case-insensitive " +
			"variable-name prefix such as Com_ or Innodb_ — a Go-side string filter, never interpolated into " +
			"SQL), curatedOnly (restrict to the same ~30 diagnostic counters db_global_status surfaces), " +
			"includeUnchanged (default false — variables with a known delta of 0 are hidden; entries with an " +
			"unknown delta are always kept, since unknown is not unchanged). Non-numeric status rows " +
			"(Innodb_buffer_pool_dump_status, Ssl_cipher_list, Rsa_public_key) are skipped and counted in " +
			"variablesSkippedNonNumeric. Read-only, ops_ro pool, 5 s timeout; ops_ro needs global PROCESS or " +
			"REPLICATION CLIENT privilege to see the global scope, otherwise MySQL returns session-scope " +
			"counters and the rates will look near-zero. Pairs with db_global_status (point-in-time detail), " +
			"db_connection_summary_growth (the same two-calls-and-subtract lens over PROCESSLIST facets) and " +
			"db_processlist (per-connection drill-down once a rate points at a culprit).",
		InputSchema: json.RawMessage(`{"type":"object","properties":{
			"topN":{"type":"integer","description":"Max entries returned (default 15, hard cap 100)"},
			"sortBy":{"type":"string","enum":["rate","delta","drop","name"],"description":"rate=perSecond desc (default), delta=delta desc, drop=delta asc (biggest decreases first), name=variable name asc"},
			"snapshotId":{"type":"string","description":"Compare against this stored snapshot id (from storedSnapshots) instead of the most recent one"},
			"prefix":{"type":"string","description":"Case-insensitive variable-name prefix filter, e.g. Com_ / Innodb_ / Threads_"},
			"curatedOnly":{"type":"boolean","description":"Restrict to the ~30 diagnostic counters db_global_status curates (default false)"},
			"includeUnchanged":{"type":"boolean","description":"Include variables whose delta is a known 0 (default false)"}
		}}`),
		Annotations: AnnRead(),
		Handler: func(ctx context.Context, raw json.RawMessage, _ string) any {
			var a struct {
				TopN             int    `json:"topN"`
				SortBy           string `json:"sortBy"`
				SnapshotID       string `json:"snapshotId"`
				Prefix           string `json:"prefix"`
				CuratedOnly      bool   `json:"curatedOnly"`
				IncludeUnchanged bool   `json:"includeUnchanged"`
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
				topN = dbGSGrowthDefaultTop
			}
			if topN > dbGSGrowthHardMaxTop {
				topN = dbGSGrowthHardMaxTop
			}
			// sortBy resolves to an allowlist key; empty -> "rate". An unknown key
			// is rejected before the scan so a typo reads as a bad argument rather
			// than a silently re-ordered leaderboard.
			sortKey := a.SortBy
			if sortKey == "" {
				sortKey = "rate"
			}
			if !dbGSGrowthSortKeys[sortKey] {
				return map[string]any{"error": fmt.Sprintf("invalid sortBy: %q (valid: rate, delta, drop, name)", a.SortBy)}
			}
			c, cancel := context.WithTimeout(ctx, dbGSGrowthTimeout)
			defer cancel()
			out, err := collectGlobalStatusGrowth(c, deps.QueryDB, dbGSGrowthRing, time.Now(),
				dbGSGrowthOpts{
					TopN:             topN,
					SortKey:          sortKey,
					WantID:           a.SnapshotID,
					Prefix:           a.Prefix,
					CuratedOnly:      a.CuratedOnly,
					IncludeUnchanged: a.IncludeUnchanged,
				})
			if err != nil {
				return map[string]any{"error": err.Error()}
			}
			return out
		},
	})
}

// dbGSGrowthOpts bundles the resolved arguments. The display filters
// (Prefix / CuratedOnly / IncludeUnchanged) are applied AFTER the diff, never to
// the stored reading, so they can never manufacture movement.
type dbGSGrowthOpts struct {
	TopN             int
	SortKey          string
	WantID           string
	Prefix           string
	CuratedOnly      bool
	IncludeUnchanged bool
}

// collectGlobalStatusGrowth reads SHOW GLOBAL STATUS, records the snapshot in
// the ring, and diffs it against the resolved baseline. Split out (and taking
// the ring plus a fixed `now`) so tests can drive the whole path with sqlmock
// and their own isolated history.
func collectGlobalStatusGrowth(ctx context.Context, db *sql.DB, ring *dbGSSnapshotRing, now time.Time,
	opts dbGSGrowthOpts) (map[string]any, error) {

	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire conn: %w", err)
	}
	defer conn.Close()

	vars, skipped, truncated, err := readGlobalStatusInts(ctx, conn)
	if err != nil {
		return nil, err
	}

	snap := &dbGSStoredSnapshot{
		TakenAt:   now,
		Uptime:    vars["Uptime"],
		Truncated: truncated,
		Vars:      vars,
	}
	baseline, err := ring.record(snap, opts.WantID)
	if err != nil {
		if errors.Is(err, errDBGSSnapshotNotFound) {
			// The read already happened and the snapshot was NOT recorded, so say
			// so plainly — otherwise the operator retries against an id that will
			// never appear.
			return nil, fmt.Errorf("%w (ring holds the last %d snapshots; call without snapshotId to seed a baseline)", err, ring.cap)
		}
		return nil, err
	}

	notes := []string{}
	out := map[string]any{
		"snapshotId":                 snap.ID,
		"takenAt":                    now.UTC().Format(time.RFC3339),
		"uptimeSeconds":              snap.Uptime,
		"uptimeHuman":                humanDuration(time.Duration(snap.Uptime) * time.Second),
		"variablesScanned":           len(vars),
		"variablesSkippedNonNumeric": skipped,
		"truncated":                  truncated,
		"scanCap":                    dbGSMaxVars,
		"topN":                       opts.TopN,
		"sortBy":                     opts.SortKey,
		"ringCap":                    ring.cap,
		"firstSnapshot":              baseline == nil,
		"serverRestarted":            false,
		"curatedOnly":                opts.CuratedOnly,
		"includeUnchanged":           opts.IncludeUnchanged,
	}
	if opts.Prefix != "" {
		out["prefix"] = opts.Prefix
	}
	if truncated {
		notes = append(notes, fmt.Sprintf("SHOW GLOBAL STATUS exceeded %d rows; the reading is incomplete and deltas may be missing variables", dbGSMaxVars))
	}

	var prev map[string]int64
	// rateSeconds is the denominator for every counter rate. Zero means "no
	// usable interval", which suppresses rates entirely rather than dividing by
	// something arbitrary.
	var rateSeconds float64
	restarted := false

	if baseline == nil {
		out["comparedTo"] = nil
		out["baselineTakenAt"] = nil
		out["baselineUptimeSeconds"] = nil
		out["intervalSeconds"] = 0
		out["uptimeDeltaSeconds"] = nil
		out["rateBasis"] = nil
		out["rateIntervalSeconds"] = int64(0)
		notes = append(notes, "no prior snapshot: every delta and rate is null because a cumulative counter's raw value says nothing about this interval. Call again after an interval to get real movement.")
	} else {
		prev = baseline.Vars
		interval := int(now.Sub(baseline.TakenAt).Seconds())
		uptimeDelta := snap.Uptime - baseline.Uptime
		restarted = dbGSDetectRestart(vars, prev, snap.Uptime, baseline.Uptime)

		out["comparedTo"] = baseline.ID
		out["baselineTakenAt"] = baseline.TakenAt.UTC().Format(time.RFC3339)
		out["baselineUptimeSeconds"] = baseline.Uptime
		out["intervalSeconds"] = interval
		out["uptimeDeltaSeconds"] = uptimeDelta
		out["serverRestarted"] = restarted

		// rateIntervalSeconds is always an int64 regardless of which branch sets
		// it, so a caller (or a test) can read it without type-switching.
		switch {
		case restarted:
			out["rateBasis"] = nil
			out["rateIntervalSeconds"] = int64(0)
			notes = append(notes, fmt.Sprintf("server restarted between snapshots (uptime %ds -> %ds): counter deltas and all rates are suppressed because the counters reset. Gauge deltas are still comparable.",
				baseline.Uptime, snap.Uptime))
		case uptimeDelta > 0:
			// The server's own clock is the better denominator: it is immune to
			// skew between this process and the DB host.
			rateSeconds = float64(uptimeDelta)
			out["rateBasis"] = "uptime"
			out["rateIntervalSeconds"] = uptimeDelta
		case interval > 0:
			rateSeconds = float64(interval)
			out["rateBasis"] = "wallClock"
			out["rateIntervalSeconds"] = int64(interval)
			notes = append(notes, "Uptime did not advance between snapshots; rates fall back to the wall-clock interval")
		default:
			out["rateBasis"] = nil
			out["rateIntervalSeconds"] = int64(0)
			notes = append(notes, "the two snapshots are less than a second apart: deltas are reported but rates are suppressed")
		}
		if baseline.Truncated || truncated {
			notes = append(notes, "a reading hit the row cap; deltas on missing variables are unavailable")
		}
	}

	entries, totals := dbGSDiff(vars, prev, baseline != nil, restarted, rateSeconds)
	out["countersTracked"] = totals.counters
	out["gaugesTracked"] = totals.gauges
	out["variablesMoved"] = totals.moved
	out["variablesNew"] = totals.newVars
	out["variablesVanished"] = totals.vanished

	entries = dbGSFilter(entries, opts)
	out["variablesMatched"] = len(entries)
	dbGSSort(entries, opts.SortKey)
	if len(entries) > opts.TopN {
		entries = entries[:opts.TopN]
	}
	out["variables"] = entries
	out["storedSnapshots"] = ring.list()
	out["notes"] = notes
	return out, nil
}

// readGlobalStatusInts runs SHOW GLOBAL STATUS and keeps only the rows whose
// value parses as an integer. The non-numeric rows a real server returns
// (Innodb_buffer_pool_dump_status is a sentence, Ssl_cipher_list is a list,
// Rsa_public_key is a PEM blob) have no delta and are counted, not stored — but
// they are counted so "why is Ssl_cipher_list missing?" has an answer.
func readGlobalStatusInts(ctx context.Context, conn *sql.Conn) (map[string]int64, int, bool, error) {
	rows, err := conn.QueryContext(ctx, "SHOW GLOBAL STATUS")
	if err != nil {
		return nil, 0, false, fmt.Errorf("SHOW GLOBAL STATUS: %w", err)
	}
	defer rows.Close()

	out := make(map[string]int64, 512)
	skipped := 0
	truncated := false
	for rows.Next() {
		if len(out)+skipped >= dbGSMaxVars {
			truncated = true
			break
		}
		var k, v sql.NullString
		if err := rows.Scan(&k, &v); err != nil {
			return nil, 0, false, fmt.Errorf("scan SHOW GLOBAL STATUS: %w", err)
		}
		n, err := strconv.ParseInt(strings.TrimSpace(v.String), 10, 64)
		if err != nil {
			skipped++
			continue
		}
		out[k.String] = n
	}
	if err := rows.Err(); err != nil {
		return nil, 0, false, fmt.Errorf("iterate SHOW GLOBAL STATUS: %w", err)
	}
	return out, skipped, truncated, nil
}

// dbGSDetectRestart decides whether the server restarted between two readings.
//
// The primary signal is Uptime going backwards. The secondary is any COUNTER
// decreasing — counters are monotonic by definition, so a decrease means they
// were reset. Both checks are restricted to variables present in BOTH readings:
// a variable that merely VANISHED would otherwise read as a decrease to zero
// and falsely condemn a perfectly healthy server. Gauges are excluded outright,
// since Threads_connected dropping from 40 to 8 is a normal Tuesday.
func dbGSDetectRestart(cur, prev map[string]int64, curUptime, prevUptime int64) bool {
	if prev == nil {
		return false
	}
	if curUptime < prevUptime {
		return true
	}
	for name, prevVal := range prev {
		curVal, ok := cur[name]
		if !ok {
			continue // vanished: no reading, not a decrease
		}
		if dbGSKind(name) == "counter" && curVal < prevVal {
			return true
		}
	}
	return false
}

// dbGSTotals are the honest census counts, folded over the full diffed union
// before any display filter or topN cut.
type dbGSTotals struct {
	counters int
	gauges   int
	moved    int
	newVars  int
	vanished int
}

// dbGSDiff joins the current and baseline readings on their key UNION and
// computes each variable's delta and (for counters) rate. The union rather than
// just the current keys is what keeps a vanished variable visible — a status
// variable disappearing means a plugin unloaded or the binary changed, which is
// signal, not absence of signal.
//
// hasBaseline distinguishes "first call" from "baseline had no such variable":
// on the first call NOTHING is computable, whereas later a missing prior entry
// means the variable is genuinely new.
func dbGSDiff(cur, prev map[string]int64, hasBaseline, restarted bool, rateSeconds float64) ([]dbGSGrowthEntry, dbGSTotals) {
	var totals dbGSTotals
	seen := make(map[string]bool, len(cur)+len(prev))
	out := make([]dbGSGrowthEntry, 0, len(cur)+len(prev))

	add := func(name string) {
		if seen[name] {
			return
		}
		seen[name] = true

		kind := dbGSKind(name)
		if kind == "gauge" {
			totals.gauges++
		} else {
			totals.counters++
		}

		e := dbGSGrowthEntry{Variable: name, Kind: kind}
		curVal, haveCur := cur[name]
		prevVal, havePrev := prev[name]
		if haveCur {
			v := curVal
			e.Value = &v
		}
		if havePrev {
			v := prevVal
			e.PrevValue = &v
		}
		switch {
		case !hasBaseline:
			// First call: no prior reading at all, so nothing is computable.
		case haveCur && !havePrev:
			totals.newVars++
		case !haveCur && havePrev:
			totals.vanished++
		case restarted && kind == "counter":
			// Counters reset; a delta across the reset is meaningless.
		default:
			d := curVal - prevVal
			e.Delta = &d
			if d != 0 {
				totals.moved++
			}
			if kind == "counter" && rateSeconds > 0 {
				perSec := roundTo(float64(d)/rateSeconds, 3)
				perHour := roundTo(float64(d)*3600.0/rateSeconds, 2)
				e.PerSecond = &perSec
				e.PerHour = &perHour
			}
		}
		out = append(out, e)
	}

	for k := range cur {
		add(k)
	}
	for k := range prev {
		add(k)
	}
	return out, totals
}

// dbGSFilter applies the display filters. Order matters only for readability;
// all three are independent predicates.
//
// includeUnchanged=false drops a variable ONLY when its delta is a KNOWN zero.
// An entry whose delta is nil (first call, new/vanished variable, counter across
// a restart) is always kept: unknown is not unchanged, and hiding those would
// make the first call return an empty list that reads like "nothing is
// happening" when the truth is "nothing is known yet".
func dbGSFilter(entries []dbGSGrowthEntry, opts dbGSGrowthOpts) []dbGSGrowthEntry {
	curated := dbGSCurated()
	lowerPrefix := strings.ToLower(opts.Prefix)
	out := entries[:0]
	for _, e := range entries {
		if opts.CuratedOnly && !curated[e.Variable] {
			continue
		}
		if lowerPrefix != "" && !strings.HasPrefix(strings.ToLower(e.Variable), lowerPrefix) {
			continue
		}
		if !opts.IncludeUnchanged && e.Delta != nil && *e.Delta == 0 {
			continue
		}
		out = append(out, e)
	}
	return out
}

// dbGSSort orders the entries by the requested key. Every ordering ends in
// variable-name-asc so ties are deterministic — without it Go's map iteration
// randomization would reshuffle equal rows on every call. Entries whose sort
// scalar is unknown (nil) always sort AFTER those that have one, so a first
// call (where every scalar is nil) degrades cleanly to alphabetical.
func dbGSSort(entries []dbGSGrowthEntry, sortKey string) {
	sort.Slice(entries, func(i, j int) bool {
		a, b := entries[i], entries[j]
		switch sortKey {
		case "delta":
			if c := cmpNilLastDesc(a.Delta, b.Delta); c != 0 {
				return c < 0
			}
			if c := cmpNilLastDesc(a.Value, b.Value); c != 0 {
				return c < 0
			}
		case "drop":
			// Biggest decreases first; among equal drops the variable that WAS
			// bigger matters more.
			if c := cmpNilLastAsc(a.Delta, b.Delta); c != 0 {
				return c < 0
			}
			if c := cmpNilLastDesc(a.PrevValue, b.PrevValue); c != 0 {
				return c < 0
			}
		case "name":
			// Name only; the shared tiebreak below does the work.
		default: // "rate"
			if c := cmpFloatNilLastDesc(a.PerSecond, b.PerSecond); c != 0 {
				return c < 0
			}
			// Gauges have no rate at all, so fall back to the magnitude of the
			// movement — a gauge that swung by 400 is more interesting than one
			// that swung by 1.
			if c := cmpNilLastDesc(absPtr(a.Delta), absPtr(b.Delta)); c != 0 {
				return c < 0
			}
		}
		return a.Variable < b.Variable
	})
}

// cmpNilLastDesc orders two optional int64s descending with nil last.
// Returns -1 when a sorts before b, +1 when after, 0 when indistinguishable.
func cmpNilLastDesc(a, b *int64) int {
	switch {
	case a == nil && b == nil:
		return 0
	case a == nil:
		return 1
	case b == nil:
		return -1
	case *a > *b:
		return -1
	case *a < *b:
		return 1
	}
	return 0
}

// cmpNilLastAsc orders two optional int64s ascending with nil last.
func cmpNilLastAsc(a, b *int64) int {
	switch {
	case a == nil && b == nil:
		return 0
	case a == nil:
		return 1
	case b == nil:
		return -1
	case *a < *b:
		return -1
	case *a > *b:
		return 1
	}
	return 0
}

// cmpFloatNilLastDesc orders two optional float64s descending with nil last.
func cmpFloatNilLastDesc(a, b *float64) int {
	switch {
	case a == nil && b == nil:
		return 0
	case a == nil:
		return 1
	case b == nil:
		return -1
	case *a > *b:
		return -1
	case *a < *b:
		return 1
	}
	return 0
}

// absPtr returns |*n| as a fresh pointer, or nil for a nil input — used so the
// rate ordering can fall back to movement magnitude for gauges without treating
// a -400 swing as smaller than a +1 one.
func absPtr(n *int64) *int64 {
	if n == nil {
		return nil
	}
	v := *n
	if v < 0 {
		v = -v
	}
	return &v
}
