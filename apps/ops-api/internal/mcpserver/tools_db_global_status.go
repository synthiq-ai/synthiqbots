package mcpserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

const dbGlobalStatusTimeout = 5 * time.Second

// Thresholds used by the rolled-up `checks[]` in the response. Tuned for an
// ac-database / acore_* workload — mod-ollama-chat keeps a pool of idle Sleep
// connections per worldserver thread (typically <30), so 80% of max_connections
// is well into "something is leaking" territory. Buffer pool hit ratio floor is
// 95% — OLTP workloads usually sit at 99%+, but a recently-started server with
// a cold buffer pool can dip below 99% legitimately, so the floor allows for
// that without spamming false positives on every cron run.
const (
	connectionUsageWarnFraction = 0.80
	innodbBufferPoolHitFloor    = 0.95
	abortedConnectsWarnPerHour  = 30.0

	// dbGSCheckMinWindowSeconds is the shortest interval accepted as the basis
	// for the aborted-connects rate check. It is DERIVED from the threshold, not
	// picked: one aborted connect in a window of W seconds extrapolates to
	// 3600/W per hour, so any window shorter than
	// 3600/abortedConnectsWarnPerHour (= 120 s) lets a SINGLE event cross the
	// 30/hour line. The +1 is load-bearing rather than defensive rounding — the
	// comparison is a strict `<`, so at exactly 120 s one connection lands
	// exactly ON 30/hour and still fails. Under this floor the check reports the
	// lifetime average rather than manufacturing a burst out of one connection.
	dbGSCheckMinWindowSeconds = int64(3600/abortedConnectsWarnPerHour) + 1

	// dbGSBufferPoolMinReadRequests is the smallest interval sample the buffer
	// pool hit-ratio check will judge. Like the window floor above it is DERIVED
	// from its own threshold rather than picked: hit = 1 - misses/requests clears
	// a floor F only when requests >= misses/(1-F), so at F = 0.95 a single miss
	// needs 20 read requests to stay green. Note this derivation carries NO +1,
	// unlike the window floor — that comparison is a strict `<` and this one is a
	// `>=`, so at exactly 20 requests one miss lands on 0.95 and passes. Below
	// this sample a near-idle interval would report a cache emergency off a
	// single miss, which is the same fabricated finding the zero-request guard
	// exists to prevent, only harder to spot.
	dbGSBufferPoolMinReadRequests = int64(20)

	// dbGSCheckMaxWindowSeconds bounds how stale a baseline snapshot may be and
	// still be described as current load. Any finite window beats dividing by
	// Uptime, but a snapshot from last week averages over nearly as much history
	// as the lifetime figure while wearing an "interval" label, which is the
	// same false confidence in a smaller package.
	dbGSCheckMaxWindowSeconds = int64(6 * 60 * 60)
)

// Basis labels for the rate-sensitive checks. The first two mirror
// db_global_status_growth's rateBasis vocabulary so the two tools describe the
// same interval the same way; the third is this tool's fallback.
const (
	dbGSBasisUptime    = "uptime"
	dbGSBasisWallClock = "wallClock"
	dbGSBasisLifetime  = "lifetimeAverage"
)

// curatedStatusKeys is the diagnostic subset we pull from SHOW GLOBAL STATUS.
// MySQL 8 returns 450+ rows from the unfiltered SHOW; that's noise the agent
// has to wade through to find the four counters it actually cares about.
// Keeping a fixed list also makes the response shape stable across MySQL
// versions — Percona / MariaDB add and drop status vars freely, but the keys
// below are the long-stable core counters.
var curatedStatusKeys = []string{
	"Uptime",
	"Queries",
	"Questions",
	"Threads_connected",
	"Threads_running",
	"Threads_created",
	"Threads_cached",
	"Connections",
	"Aborted_clients",
	"Aborted_connects",
	"Slow_queries",
	"Com_select",
	"Com_insert",
	"Com_update",
	"Com_delete",
	"Com_commit",
	"Com_rollback",
	"Innodb_buffer_pool_reads",
	"Innodb_buffer_pool_read_requests",
	"Innodb_buffer_pool_pages_total",
	"Innodb_buffer_pool_pages_free",
	"Innodb_buffer_pool_pages_dirty",
	"Innodb_rows_read",
	"Innodb_rows_inserted",
	"Innodb_rows_updated",
	"Innodb_rows_deleted",
	"Table_locks_immediate",
	"Table_locks_waited",
	"Open_tables",
	"Opened_tables",
	"Created_tmp_tables",
	"Created_tmp_disk_tables",
}

// curatedVariableKeys is the diagnostic subset we pull from SHOW GLOBAL
// VARIABLES. Restricted to the values needed to compute % / ratios on the
// status counters (max_connections, innodb_buffer_pool_size) plus the
// slow-query knobs operators consult when slow_queries climbs.
var curatedVariableKeys = []string{
	"version",
	"version_comment",
	"max_connections",
	"innodb_buffer_pool_size",
	"slow_query_log",
	"long_query_time",
	"log_slow_admin_statements",
	"log_queries_not_using_indexes",
	"read_only",
	"super_read_only",
}

// RegisterDBGlobalStatusTool registers `db_global_status` — a curated
// SHOW GLOBAL STATUS + SHOW GLOBAL VARIABLES snapshot with derived metrics
// (qps, connection usage %, buffer pool hit ratio) and a `checks[]` roll-up.
//
// Operators previously had to issue one db_query per counter — `SHOW GLOBAL
// STATUS LIKE 'Threads_connected'`, `... LIKE 'Slow_queries'`, etc. — and the
// unfiltered SHOW returns 450+ rows under MySQL 8, drowning the diagnostic
// counters in noise. This tool returns the ~30 counters operators care about
// in one round-trip and computes the derived percentages that they would
// otherwise eyeball.
func RegisterDBGlobalStatusTool(reg *Registry, deps DBDeps) {
	reg.Register(Tool{
		Name: "db_global_status",
		Description: "Curated MySQL/MariaDB health snapshot. Combines SHOW GLOBAL STATUS and " +
			"SHOW GLOBAL VARIABLES, pulls ~30 diagnostic counters " +
			"(Threads_connected, Slow_queries, Aborted_*, Innodb_buffer_pool_*, " +
			"Com_select/insert/update/delete, Created_tmp_disk_tables, " +
			"Table_locks_waited, Open_tables, Opened_tables) plus version, " +
			"max_connections, innodb_buffer_pool_size, slow_query_log knobs. Derived " +
			"fields: queriesPerSecond (Queries/Uptime), connectionUsagePct " +
			"(Threads_connected/max_connections), innodbBufferPoolHitRatioPct " +
			"(1 - reads/read_requests), abortedConnectsPerHour (Aborted_connects/Uptime — a " +
			"LIFETIME average, see the checks note below). " +
			"Returns a `checks[]` array rolling connection_usage / buffer_pool_hit_ratio / " +
			"slow_queries_present / aborted_connects_rate / read_only_off into ok/fail with " +
			"an `allOk` boolean — same shape as worldserver_health. CHECK WINDOW: " +
			"no_slow_queries, aborted_connects_rate_under_threshold and " +
			"innodb_buffer_pool_hit_ratio_ok are measured against the " +
			"most recent snapshot db_global_status_growth recorded in this process, so they " +
			"describe the LAST INTERVAL rather than the whole of Uptime; each carries `basis` " +
			"(uptime | wallClock | lifetimeAverage), `windowSeconds` and, for the aborted-connects " +
			"check, `ratePerHour`, and the same pair is summarized top-level as checkWindowBasis / " +
			"checkWindowSeconds. The buffer-pool check additionally reports `hitRatioPct` with the " +
			"`reads` / `readRequests` it was computed from, and stays on the lifetime basis when the " +
			"interval served fewer than 20 read requests — an idle window has no hit ratio, and " +
			"under that sample one miss alone would fall below the floor. Note the top-level " +
			"innodbBufferPoolHitRatioPct field remains the LIFETIME ratio as labelled; the interval " +
			"reading lives on the check. When no snapshot exists (nothing has called " +
			"db_global_status_growth yet), the baseline is stale or shorter than 120 s, or the " +
			"server restarted since it was taken, the checks fall back to " +
			"basis=lifetimeAverage and say so in `detail` — a lifetime average over a long-lived " +
			"server can never fire on a recent burst (48213 aborted connects over ninety days " +
			"averages ~22/hour and passes even if 500 of them landed in the last minute), so " +
			"call db_global_status_growth to seed a baseline and then call this again. Replaces the " +
			"`SHOW GLOBAL STATUS LIKE '...'` per-counter sequence operators were running via " +
			"db_query. Read-only, ops_ro pool, 5 s timeout. ops_ro must have global PROCESS " +
			"or REPLICATION CLIENT privilege to see the global scope; otherwise MySQL returns " +
			"the session scope and counters will look near-zero.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
		Annotations: AnnRead(),
		Handler: func(ctx context.Context, _ json.RawMessage, _ string) any {
			if deps.QueryDB == nil {
				return map[string]any{"error": "ops_ro pool not configured (DB_DSN empty?)"}
			}
			c, cancel := context.WithTimeout(ctx, dbGlobalStatusTimeout)
			defer cancel()
			out, err := collectGlobalStatus(c, deps.QueryDB, dbGSGrowthRing, time.Now())
			if err != nil {
				return map[string]any{"error": err.Error()}
			}
			return out
		},
	})
}

// collectGlobalStatus runs SHOW GLOBAL STATUS and SHOW GLOBAL VARIABLES,
// extracts the curated subset, computes derived metrics, and packs the
// response. Split out from the handler (and taking the snapshot ring plus a
// fixed `now`) so tests can drive the whole path with sqlmock and their own
// isolated history. A nil ring means "no history available" and forces the
// lifetime basis, which is exactly the pre-interval behaviour.
func collectGlobalStatus(ctx context.Context, db *sql.DB, ring *dbGSSnapshotRing, now time.Time) (map[string]any, error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire conn: %w", err)
	}
	defer conn.Close()

	status, err := readShowKV(ctx, conn, "SHOW GLOBAL STATUS")
	if err != nil {
		return nil, err
	}
	vars, err := readShowKV(ctx, conn, "SHOW GLOBAL VARIABLES")
	if err != nil {
		return nil, err
	}

	uptime := parseInt64(status["Uptime"])
	threadsConnected := parseInt64(status["Threads_connected"])
	threadsRunning := parseInt64(status["Threads_running"])
	maxConnections := parseInt64(vars["max_connections"])
	queries := parseInt64(status["Queries"])
	slowQueries := parseInt64(status["Slow_queries"])
	abortedConnects := parseInt64(status["Aborted_connects"])
	abortedClients := parseInt64(status["Aborted_clients"])
	bufReads := parseInt64(status["Innodb_buffer_pool_reads"])
	bufReadRequests := parseInt64(status["Innodb_buffer_pool_read_requests"])
	bufPoolSize := parseInt64(vars["innodb_buffer_pool_size"])
	pagesTotal := parseInt64(status["Innodb_buffer_pool_pages_total"])
	pagesFree := parseInt64(status["Innodb_buffer_pool_pages_free"])
	pagesDirty := parseInt64(status["Innodb_buffer_pool_pages_dirty"])
	createdTmpDisk := parseInt64(status["Created_tmp_disk_tables"])
	createdTmp := parseInt64(status["Created_tmp_tables"])
	tableLocksWaited := parseInt64(status["Table_locks_waited"])
	tableLocksImmediate := parseInt64(status["Table_locks_immediate"])
	readOnly := isOn(vars["read_only"])
	superReadOnly := isOn(vars["super_read_only"])

	// window resolves ONCE, before the checks, so both rate-sensitive checks
	// describe the same interval. It reads the growth tool's ring; it never
	// writes to it.
	window := resolveGSCheckWindow(ring, now, dbGSParseInts(status), uptime)

	out := map[string]any{
		"generatedAt":                  now.UTC(),
		"version":                      vars["version"],
		"versionComment":               vars["version_comment"],
		"uptimeSeconds":                uptime,
		"uptimeHuman":                  humanDuration(time.Duration(uptime) * time.Second),
		"queries":                      queries,
		"questions":                    parseInt64(status["Questions"]),
		"slowQueries":                  slowQueries,
		"slowQueryLog":                 isOn(vars["slow_query_log"]),
		"longQueryTime":                parseFloat64(vars["long_query_time"]),
		"connectionsTotal":             parseInt64(status["Connections"]),
		"threadsConnected":             threadsConnected,
		"threadsRunning":               threadsRunning,
		"threadsCreated":               parseInt64(status["Threads_created"]),
		"threadsCached":                parseInt64(status["Threads_cached"]),
		"maxConnections":               maxConnections,
		"abortedClients":               abortedClients,
		"abortedConnects":              abortedConnects,
		"comSelect":                    parseInt64(status["Com_select"]),
		"comInsert":                    parseInt64(status["Com_insert"]),
		"comUpdate":                    parseInt64(status["Com_update"]),
		"comDelete":                    parseInt64(status["Com_delete"]),
		"comCommit":                    parseInt64(status["Com_commit"]),
		"comRollback":                  parseInt64(status["Com_rollback"]),
		"innodbBufferPoolSize":         bufPoolSize,
		"innodbBufferPoolSizeHuman":    humanBytes(uint64Of(bufPoolSize)),
		"innodbBufferPoolReads":        bufReads,
		"innodbBufferPoolReadRequests": bufReadRequests,
		"innodbBufferPoolPagesTotal":   pagesTotal,
		"innodbBufferPoolPagesFree":    pagesFree,
		"innodbBufferPoolPagesDirty":   pagesDirty,
		"innodbRowsRead":               parseInt64(status["Innodb_rows_read"]),
		"innodbRowsInserted":           parseInt64(status["Innodb_rows_inserted"]),
		"innodbRowsUpdated":            parseInt64(status["Innodb_rows_updated"]),
		"innodbRowsDeleted":            parseInt64(status["Innodb_rows_deleted"]),
		"tableLocksImmediate":          tableLocksImmediate,
		"tableLocksWaited":             tableLocksWaited,
		"openTables":                   parseInt64(status["Open_tables"]),
		"openedTables":                 parseInt64(status["Opened_tables"]),
		"createdTmpTables":             createdTmp,
		"createdTmpDiskTables":         createdTmpDisk,
		"readOnly":                     readOnly,
		"superReadOnly":                superReadOnly,
		"checkWindowBasis":             window.basis,
		"checkWindowSeconds":           window.seconds,
	}

	// Derived metrics — only emit when the denominator is meaningful. Avoids
	// surfacing "NaN" or wildly misleading values on a server that hasn't run
	// long enough to accumulate stats (Uptime=0) or has never served a buffer
	// pool read request.
	if uptime > 0 {
		out["queriesPerSecond"] = roundTo(float64(queries)/float64(uptime), 2)
		out["abortedConnectsPerHour"] = roundTo(float64(abortedConnects)*3600.0/float64(uptime), 2)
	}
	if maxConnections > 0 {
		out["connectionUsagePct"] = roundTo(float64(threadsConnected)*100.0/float64(maxConnections), 2)
	}
	if bufReadRequests > 0 {
		hitRatio := 100.0 * (1.0 - float64(bufReads)/float64(bufReadRequests))
		out["innodbBufferPoolHitRatioPct"] = roundTo(hitRatio, 4)
	}
	if createdTmp > 0 {
		out["createdTmpDiskFraction"] = roundTo(float64(createdTmpDisk)/float64(createdTmp), 4)
	}
	if tableLocksImmediate+tableLocksWaited > 0 {
		out["tableLockContentionPct"] = roundTo(
			float64(tableLocksWaited)*100.0/float64(tableLocksImmediate+tableLocksWaited), 4)
	}

	// Rolled-up checks. Mirrors worldserver_health.checks[] so the agent can
	// branch on `allOk` without re-reading every counter.
	checks := make([]map[string]any, 0, 5)

	// 1. Connection usage. Only meaningful when max_connections is set.
	if maxConnections > 0 {
		ratio := float64(threadsConnected) / float64(maxConnections)
		checks = append(checks, map[string]any{
			"name": "connection_usage_under_threshold",
			"ok":   ratio < connectionUsageWarnFraction,
			"detail": fmt.Sprintf("%d / %d connections used (threshold %.0f%%)",
				threadsConnected, maxConnections, connectionUsageWarnFraction*100),
		})
	}

	// 2. Buffer pool hit ratio. Only meaningful once read_requests > 0.
	//
	// This used to divide the two lifetime counters, which reported the LIFETIME
	// hit ratio and so could never move during the episode it was written to
	// catch: a server sitting at 99.9% over 10^10 read requests absorbs ten
	// million straight-from-disk reads with a ~0.1pp dent, and the check stays
	// green for the whole incident. The threshold's own justification is the tell
	// — a 95% floor chosen to tolerate a cold buffer pool at startup cannot also
	// detect a cold buffer pool later. Against the interval the same reading is a
	// miss rate, not a historical average.
	//
	// Unlike the two rate checks below this is a ratio of TWO counters, so it
	// needs both deltas and one extra guard they do not: a zero read-request
	// delta is an IDLE interval, not a 0% hit ratio. Dividing by it yields NaN,
	// which json.Marshal refuses — that would take the entire response down over
	// a server doing nothing wrong.
	if bufReadRequests > 0 {
		bufCheck := map[string]any{"name": "innodb_buffer_pool_hit_ratio_ok"}
		readsDelta, readsOK := window.delta("Innodb_buffer_pool_reads", bufReads)
		reqDelta, reqOK := window.delta("Innodb_buffer_pool_read_requests", bufReadRequests)
		if readsOK && reqOK && reqDelta >= dbGSBufferPoolMinReadRequests && readsDelta <= reqDelta {
			hit := 1.0 - float64(readsDelta)/float64(reqDelta)
			bufCheck["basis"] = window.basis
			bufCheck["windowSeconds"] = window.seconds
			bufCheck["reads"] = readsDelta
			bufCheck["readRequests"] = reqDelta
			bufCheck["hitRatioPct"] = roundTo(hit*100, 4)
			bufCheck["ok"] = hit >= innodbBufferPoolHitFloor
			bufCheck["detail"] = fmt.Sprintf(
				"%.4f%% over the last %s (%d disk reads / %d read requests; floor %.0f%%)",
				hit*100, humanDuration(time.Duration(window.seconds)*time.Second),
				readsDelta, reqDelta, innodbBufferPoolHitFloor*100)
		} else {
			hit := 1.0 - float64(bufReads)/float64(bufReadRequests)
			bufCheck["basis"] = dbGSBasisLifetime
			bufCheck["windowSeconds"] = uptime
			bufCheck["reads"] = bufReads
			bufCheck["readRequests"] = bufReadRequests
			bufCheck["hitRatioPct"] = roundTo(hit*100, 4)
			bufCheck["ok"] = hit >= innodbBufferPoolHitFloor
			bufCheck["detail"] = fmt.Sprintf(
				"%.4f%% since server start (%d disk reads / %d read requests; floor %.0f%%) — a lifetime ratio cannot move during a cold-cache episode (%s); call db_global_status_growth to seed a baseline",
				hit*100, bufReads, bufReadRequests, innodbBufferPoolHitFloor*100,
				dbGSBufferPoolFallbackReason(window, readsOK, reqOK, readsDelta, reqDelta))
		}
		checks = append(checks, bufCheck)
	}

	// 3. Slow queries. `Slow_queries` is cumulative since server start, so the
	// original bare `slowQueries == 0` meant any server that had ever run one
	// slow ALTER reported ok:false forever — a check that can never go back to
	// green trains operators to ignore allOk entirely. With a baseline the check
	// asks the question that can actually change: did a slow query happen in the
	// measured window? Without one it keeps the lifetime reading and says so.
	slowCheck := map[string]any{
		"name":          "no_slow_queries",
		"basis":         window.basis,
		"windowSeconds": window.seconds,
	}
	if d, ok := window.delta("Slow_queries", slowQueries); ok {
		slowCheck["ok"] = d == 0
		slowCheck["detail"] = fmt.Sprintf("%d new slow queries in the last %s (%d since server start)",
			d, humanDuration(time.Duration(window.seconds)*time.Second), slowQueries)
	} else {
		slowCheck["basis"] = dbGSBasisLifetime
		slowCheck["windowSeconds"] = uptime
		slowCheck["ok"] = slowQueries == 0
		slowCheck["detail"] = fmt.Sprintf(
			"%d slow queries since server start — a lifetime counter, not a current rate (%s); call db_global_status_growth to seed a baseline",
			slowQueries, window.fallbackReason("Slow_queries"))
	}
	checks = append(checks, slowCheck)

	// 4. Aborted connects rate. Catches a thundering-herd of half-open TCP
	// connections (firewall flapping, gateway crash loop, etc.).
	//
	// This used to divide the lifetime counter by Uptime, which made it
	// structurally incapable of firing on the very burst it was written to
	// catch: 48213 aborted connects accrued over ninety days averages ~22/hour
	// and passes the 30/hour threshold even if 500 of them landed in the last
	// minute. Against a real interval the same 500 reads as thousands per hour
	// and fails. The lifetime average survives only as the labelled fallback,
	// and is still skipped entirely when Uptime is 0 to avoid dividing by zero.
	abortedCheck := map[string]any{"name": "aborted_connects_rate_under_threshold"}
	if d, ok := window.delta("Aborted_connects", abortedConnects); ok {
		rate := float64(d) * 3600.0 / float64(window.seconds)
		abortedCheck["basis"] = window.basis
		abortedCheck["windowSeconds"] = window.seconds
		abortedCheck["ratePerHour"] = roundTo(rate, 2)
		abortedCheck["ok"] = rate < abortedConnectsWarnPerHour
		abortedCheck["detail"] = fmt.Sprintf("%.2f/hour over the last %s (%d aborted connects; threshold %.0f/hour)",
			rate, humanDuration(time.Duration(window.seconds)*time.Second), d, abortedConnectsWarnPerHour)
		checks = append(checks, abortedCheck)
	} else if uptime > 0 {
		rate := float64(abortedConnects) * 3600.0 / float64(uptime)
		abortedCheck["basis"] = dbGSBasisLifetime
		abortedCheck["windowSeconds"] = uptime
		abortedCheck["ratePerHour"] = roundTo(rate, 2)
		abortedCheck["ok"] = rate < abortedConnectsWarnPerHour
		abortedCheck["detail"] = fmt.Sprintf(
			"%.2f/hour averaged over the whole of Uptime (threshold %.0f/hour) — a lifetime average cannot fire on a recent burst (%s); call db_global_status_growth to seed a baseline",
			rate, abortedConnectsWarnPerHour, window.fallbackReason("Aborted_connects"))
		checks = append(checks, abortedCheck)
	}

	// 5. read_only flag. Catches the case where someone (or a startup script)
	// has flipped the server into read-only mode — db_exec writes would fail
	// even though the ops-api looks otherwise healthy.
	checks = append(checks, map[string]any{
		"name":   "writes_enabled",
		"ok":     !readOnly,
		"detail": fmt.Sprintf("read_only=%v super_read_only=%v", readOnly, superReadOnly),
	})

	out["checks"] = checks
	out["allOk"] = checksAllOK(checks)
	return out, nil
}

// readShowKV runs a SHOW GLOBAL STATUS or SHOW GLOBAL VARIABLES statement and
// returns the two-column rowset as a name→value map. Both SHOW statements
// share the same `Variable_name, Value` rowset shape, so one helper covers
// both.
func readShowKV(ctx context.Context, conn *sql.Conn, stmt string) (map[string]string, error) {
	rows, err := conn.QueryContext(ctx, stmt)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", stmt, err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var k, v sql.NullString
		if err := rows.Scan(&k, &v); err != nil {
			return nil, fmt.Errorf("scan %s: %w", stmt, err)
		}
		out[k.String] = v.String
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate %s: %w", stmt, err)
	}
	return out, nil
}

// isOn normalizes the string values MySQL hands back for boolean variables.
// SHOW GLOBAL VARIABLES returns "ON"/"OFF" for most booleans, but a handful
// (slow_query_log on older MariaDB) emit "1"/"0". Treat either form as truthy.
func isOn(v string) bool {
	switch v {
	case "ON", "on", "1", "TRUE", "true":
		return true
	default:
		return false
	}
}

// dbGSCheckWindow is the resolved measurement window for the rate-sensitive
// checks.
//
// basis is "uptime" / "wallClock" when a usable interval was found (the same
// vocabulary db_global_status_growth uses for the same two denominators) and
// "lifetimeAverage" when the checks had to fall back to a counter divided by
// Uptime. reason is non-empty ONLY on that fallback: an operator reading a
// passing check needs to know whether it was measured or merely averaged, and
// a green check computed over ninety days is not the same claim as a green
// check computed over the last five minutes.
type dbGSCheckWindow struct {
	basis   string
	seconds int64
	prev    map[string]int64
	reason  string
}

// usable reports whether an interval basis was resolved.
func (w dbGSCheckWindow) usable() bool { return w.basis != dbGSBasisLifetime }

// delta returns a counter's movement across the window and whether that
// movement is computable at all. A counter absent from the baseline reading (a
// truncated snapshot, or a status variable the previous server build did not
// expose) and one that somehow decreased without tripping restart detection are
// both NOT computable — reporting either as movement would invent a burst that
// did not happen, or a negative rate that reads as healthy.
func (w dbGSCheckWindow) delta(name string, cur int64) (int64, bool) {
	if !w.usable() {
		return 0, false
	}
	prev, ok := w.prev[name]
	if !ok || cur < prev {
		return 0, false
	}
	return cur - prev, true
}

// fallbackReason explains, in a clause that reads inside a detail string, why a
// check is on the lifetime basis. A window-level reason covers both checks; the
// per-counter case only arises when the window itself was fine.
func (w dbGSCheckWindow) fallbackReason(counter string) string {
	if w.reason != "" {
		return w.reason
	}
	return fmt.Sprintf("no comparable %s reading in the baseline snapshot", counter)
}

// dbGSBufferPoolFallbackReason explains why the hit-ratio check is on the
// lifetime basis. It needs its own reason set rather than fallbackReason's
// because a ratio of two counters has failure modes a single counter does not:
// either operand can be missing from the baseline, and even with both present
// the interval can be too quiet to judge. Ordered most-general first, so the
// window-level reason wins over any per-counter detail.
func dbGSBufferPoolFallbackReason(w dbGSCheckWindow, readsOK, reqOK bool, readsDelta, reqDelta int64) string {
	switch {
	case !w.usable():
		return w.reason
	case !reqOK:
		return w.fallbackReason("Innodb_buffer_pool_read_requests")
	case !readsOK:
		return w.fallbackReason("Innodb_buffer_pool_reads")
	case reqDelta == 0:
		return "the interval served no buffer pool read requests at all, so it has no hit ratio to report — an idle window is not a cache miss"
	case reqDelta < dbGSBufferPoolMinReadRequests:
		return fmt.Sprintf(
			"the interval served only %d buffer pool read requests, and under %d a single miss already falls below the %.0f%% floor",
			reqDelta, dbGSBufferPoolMinReadRequests, innodbBufferPoolHitFloor*100)
	case readsDelta > reqDelta:
		return fmt.Sprintf(
			"the interval reports %d disk reads against %d read requests, which cannot happen and would yield a negative hit ratio",
			readsDelta, reqDelta)
	}
	return ""
}

// resolveGSCheckWindow picks the interval the rate-sensitive checks are measured
// over, reading (never writing) the snapshot ring db_global_status_growth
// maintains.
//
// Every rejection falls back to the lifetime average with a stated reason
// rather than suppressing the check: a missing check is invisible in `allOk`,
// whereas a labelled lifetime average is at least honest about what it measured.
func resolveGSCheckWindow(ring *dbGSSnapshotRing, now time.Time, cur map[string]int64, uptime int64) dbGSCheckWindow {
	lifetime := func(reason string) dbGSCheckWindow {
		return dbGSCheckWindow{basis: dbGSBasisLifetime, seconds: uptime, reason: reason}
	}
	if ring == nil {
		return lifetime("no snapshot history in this process")
	}
	base := ring.latest()
	if base == nil {
		return lifetime("nothing has called db_global_status_growth in this process yet, so there is no baseline")
	}
	// Counters reset on restart, so a diff across one is meaningless. Reuse the
	// growth tool's heuristic rather than re-deriving it, so the two tools can
	// never disagree about whether the server bounced.
	if dbGSDetectRestart(cur, base.Vars, uptime, base.Uptime) {
		return lifetime("the server restarted since the baseline snapshot, so its counters reset")
	}

	// Prefer the server's own clock: it is immune to skew between this process
	// and the DB host. Uptime cannot have gone backwards here — that is a
	// restart, caught above.
	basis := dbGSBasisUptime
	seconds := uptime - base.Uptime
	if seconds <= 0 {
		basis = dbGSBasisWallClock
		seconds = int64(now.Sub(base.TakenAt).Seconds())
	}
	switch {
	case seconds < dbGSCheckMinWindowSeconds:
		return lifetime(fmt.Sprintf(
			"the newest snapshot is only %ds old and a window under %ds would let a single event read as a burst",
			seconds, dbGSCheckMinWindowSeconds))
	case seconds > dbGSCheckMaxWindowSeconds:
		return lifetime(fmt.Sprintf("the newest snapshot is %s old, too stale to describe current load",
			humanDuration(time.Duration(seconds)*time.Second)))
	}
	return dbGSCheckWindow{basis: basis, seconds: seconds, prev: base.Vars}
}

// dbGSParseInts keeps the integer-valued rows of a SHOW GLOBAL STATUS reading,
// applying the same parse rule as db_global_status_growth's
// readGlobalStatusInts so the restart heuristic the two tools share compares
// like with like. The non-numeric rows a real server returns
// (Innodb_buffer_pool_dump_status is a sentence, Ssl_cipher_list a list) have no
// delta and are simply dropped.
func dbGSParseInts(kv map[string]string) map[string]int64 {
	out := make(map[string]int64, len(kv))
	for k, v := range kv {
		if n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64); err == nil {
			out[k] = n
		}
	}
	return out
}

// uint64Of converts a signed int64 to uint64 with a zero-floor — humanBytes
// takes uint64. Negative status counters shouldn't happen, but if they do
// (driver quirk) we'd rather render "0 B" than wrap around to 18 EiB.
func uint64Of(n int64) uint64 {
	if n < 0 {
		return 0
	}
	return uint64(n)
}
