package mcpserver

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

// gsCheckNow is the fixed clock every case in this file runs against, so wall
// clock intervals are exact rather than "about".
var gsCheckNow = time.Date(2026, 8, 1, 16, 0, 0, 0, time.UTC)

// A ninety-day-old server carrying a large lifetime Aborted_connects total.
// These are the numbers from the bug report: 48213 aborted connects over ninety
// days averages 22.32/hour, comfortably under the 30/hour threshold, so the
// lifetime basis reports ok:true no matter what just happened.
const (
	gsUptime90d      = int64(7776000)
	gsAbortedTotal   = int64(48213)
	gsSlowQueryTotal = int64(42)
)

// runGSCollect drives collectGlobalStatus against a mock connection.
func runGSCollect(t *testing.T, ring *dbGSSnapshotRing, now time.Time, status, vars [][2]string) map[string]any {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	statusRows := sqlmock.NewRows([]string{"Variable_name", "Value"})
	for _, r := range status {
		statusRows.AddRow(r[0], r[1])
	}
	varRows := sqlmock.NewRows([]string{"Variable_name", "Value"})
	for _, r := range vars {
		varRows.AddRow(r[0], r[1])
	}
	mock.ExpectQuery(regexp.QuoteMeta("SHOW GLOBAL STATUS")).WillReturnRows(statusRows)
	mock.ExpectQuery(regexp.QuoteMeta("SHOW GLOBAL VARIABLES")).WillReturnRows(varRows)

	out, err := collectGlobalStatus(context.Background(), db, ring, now)
	if err != nil {
		t.Fatalf("collectGlobalStatus: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
	return out
}

// gsBurstStatus is the CURRENT reading for the burst scenario: ninety days of
// uptime, a big lifetime aborted total, and a non-zero lifetime slow counter.
func gsBurstStatus() [][2]string {
	return [][2]string{
		{"Uptime", fmt.Sprint(gsUptime90d)},
		{"Queries", "100000000"},
		{"Threads_connected", "40"},
		{"Slow_queries", fmt.Sprint(gsSlowQueryTotal)},
		{"Aborted_connects", fmt.Sprint(gsAbortedTotal)},
		{"Innodb_buffer_pool_reads", "100"},
		{"Innodb_buffer_pool_read_requests", "10000"},
	}
}

func gsVars() [][2]string {
	return [][2]string{
		{"max_connections", "200"},
		{"read_only", "OFF"},
	}
}

// seedGSRing builds an isolated ring holding exactly one baseline snapshot, via
// the real record() path so the test never hand-assembles ring internals.
func seedGSRing(t *testing.T, takenAt time.Time, uptime int64, vars map[string]int64) *dbGSSnapshotRing {
	t.Helper()
	r := newDBGSSnapshotRing(dbGSGrowthRingCap)
	if _, err := r.record(&dbGSStoredSnapshot{TakenAt: takenAt, Uptime: uptime, Vars: vars}, ""); err != nil {
		t.Fatalf("seed ring: %v", err)
	}
	return r
}

// gsFindCheck pulls one entry out of the checks[] roll-up.
func gsFindCheck(t *testing.T, out map[string]any, name string) map[string]any {
	t.Helper()
	checks, ok := out["checks"].([]map[string]any)
	if !ok {
		t.Fatalf("checks missing or wrong type: %T", out["checks"])
	}
	for _, c := range checks {
		if c["name"] == name {
			return c
		}
	}
	t.Fatalf("check %q not found in %v", name, checks)
	return nil
}

// TestGSCheckMinWindowDerivedFromThreshold pins the arithmetic that makes the
// minimum window a derivation rather than a guess: one aborted connect in a
// window of W seconds extrapolates to 3600/W per hour, so the floor has to be
// 3600/threshold or a single connection trips the check. If someone lowers the
// threshold without re-deriving the floor, this fails loudly.
func TestGSCheckMinWindowDerivedFromThreshold(t *testing.T) {
	if want := int64(3600/abortedConnectsWarnPerHour) + 1; dbGSCheckMinWindowSeconds != want {
		t.Fatalf("dbGSCheckMinWindowSeconds = %d, want %d (3600/%.0f, +1 because the check is a strict <)",
			dbGSCheckMinWindowSeconds, want, abortedConnectsWarnPerHour)
	}
	// One event over the minimum window must NOT trip the check — that is the
	// entire reason the floor exists.
	oneEventRate := 1.0 * 3600.0 / float64(dbGSCheckMinWindowSeconds)
	if oneEventRate >= abortedConnectsWarnPerHour {
		t.Errorf("a single aborted connect over the %ds floor extrapolates to %.2f/hour, which trips the %.0f/hour threshold",
			dbGSCheckMinWindowSeconds, oneEventRate, abortedConnectsWarnPerHour)
	}
	// ...and one second below the floor, it would have.
	if justUnder := 1.0 * 3600.0 / float64(dbGSCheckMinWindowSeconds-1); justUnder < abortedConnectsWarnPerHour {
		t.Errorf("the floor is one second too generous: at %ds a single event is only %.2f/hour",
			dbGSCheckMinWindowSeconds-1, justUnder)
	}
	if dbGSCheckMaxWindowSeconds <= dbGSCheckMinWindowSeconds {
		t.Errorf("max window %d must exceed min window %d", dbGSCheckMaxWindowSeconds, dbGSCheckMinWindowSeconds)
	}
}

// TestAbortedConnectsBurst_LifetimeMissesIntervalCatches is the regression for
// the bug itself, asserted in both directions in one test so the fix cannot be
// read as a coincidence: the SAME reading passes on the lifetime basis and
// fails on the interval basis. 500 aborted connects in the last 300 s is
// 6000/hour; spread over ninety days the same counter averages 22.32/hour.
func TestAbortedConnectsBurst_LifetimeMissesIntervalCatches(t *testing.T) {
	// (1) No history: the old behaviour. Lifetime average, check passes.
	lifetimeOut := runGSCollect(t, nil, gsCheckNow, gsBurstStatus(), gsVars())
	lc := gsFindCheck(t, lifetimeOut, "aborted_connects_rate_under_threshold")
	if lc["basis"] != dbGSBasisLifetime {
		t.Errorf("basis = %v, want %q", lc["basis"], dbGSBasisLifetime)
	}
	if ok := lc["ok"].(bool); !ok {
		t.Errorf("lifetime average of %d over %ds should pass; got fail: %v", gsAbortedTotal, gsUptime90d, lc)
	}
	if got := lc["ratePerHour"].(float64); got < 22.0 || got > 23.0 {
		t.Errorf("lifetime ratePerHour = %v, want ~22.32", got)
	}
	if d := lc["detail"].(string); !strings.Contains(d, "db_global_status_growth") {
		t.Errorf("lifetime detail must point at the growth tool: %q", d)
	}

	// (2) Same reading, but a baseline from 300 s ago showing 500 of those
	// aborted connects landed in that window.
	ring := seedGSRing(t, gsCheckNow.Add(-300*time.Second), gsUptime90d-300, map[string]int64{
		"Uptime":           gsUptime90d - 300,
		"Aborted_connects": gsAbortedTotal - 500,
		"Slow_queries":     gsSlowQueryTotal,
	})
	intervalOut := runGSCollect(t, ring, gsCheckNow, gsBurstStatus(), gsVars())
	ic := gsFindCheck(t, intervalOut, "aborted_connects_rate_under_threshold")
	if ic["basis"] != dbGSBasisUptime {
		t.Errorf("basis = %v, want %q", ic["basis"], dbGSBasisUptime)
	}
	if got := ic["windowSeconds"].(int64); got != 300 {
		t.Errorf("windowSeconds = %d, want 300", got)
	}
	if got := ic["ratePerHour"].(float64); got != 6000.0 {
		t.Errorf("interval ratePerHour = %v, want 6000", got)
	}
	if ok := ic["ok"].(bool); ok {
		t.Errorf("500 aborted connects in 300s (6000/hour) must FAIL the %.0f/hour check: %v",
			abortedConnectsWarnPerHour, ic)
	}
	if intervalOut["allOk"].(bool) {
		t.Errorf("allOk must be false when the burst check fails")
	}
	// The top-level summary must agree with the check it summarizes.
	if intervalOut["checkWindowBasis"] != dbGSBasisUptime {
		t.Errorf("checkWindowBasis = %v, want %q", intervalOut["checkWindowBasis"], dbGSBasisUptime)
	}
	if got := intervalOut["checkWindowSeconds"].(int64); got != 300 {
		t.Errorf("checkWindowSeconds = %d, want 300", got)
	}
}

// TestSlowQueries_IntervalClearsStaleLifetimeCounter covers the other half of
// the item: `Slow_queries` is cumulative, so one slow ALTER months ago pinned
// the check to ok:false forever and trained operators to ignore allOk. With a
// baseline showing no NEW slow queries the check goes green again while the
// lifetime total stays visible in the detail.
func TestSlowQueries_IntervalClearsStaleLifetimeCounter(t *testing.T) {
	ring := seedGSRing(t, gsCheckNow.Add(-300*time.Second), gsUptime90d-300, map[string]int64{
		"Uptime":           gsUptime90d - 300,
		"Aborted_connects": gsAbortedTotal,
		"Slow_queries":     gsSlowQueryTotal, // unchanged: no new slow queries
	})
	out := runGSCollect(t, ring, gsCheckNow, gsBurstStatus(), gsVars())
	c := gsFindCheck(t, out, "no_slow_queries")
	if !c["ok"].(bool) {
		t.Errorf("no NEW slow queries in the window must pass despite a lifetime total of %d: %v", gsSlowQueryTotal, c)
	}
	if c["basis"] != dbGSBasisUptime {
		t.Errorf("basis = %v, want %q", c["basis"], dbGSBasisUptime)
	}
	detail := c["detail"].(string)
	if !strings.Contains(detail, "0 new slow queries") {
		t.Errorf("detail should report the interval movement: %q", detail)
	}
	if !strings.Contains(detail, fmt.Sprintf("%d since server start", gsSlowQueryTotal)) {
		t.Errorf("detail must still disclose the lifetime total: %q", detail)
	}

	// Same setup with the lifetime basis: still fails, as it always did.
	lifetime := runGSCollect(t, nil, gsCheckNow, gsBurstStatus(), gsVars())
	if lc := gsFindCheck(t, lifetime, "no_slow_queries"); lc["ok"].(bool) {
		t.Errorf("with no baseline a non-zero lifetime counter must still fail: %v", lc)
	}
}

// TestSlowQueries_IntervalFailsOnNewSlowQuery is the direction that proves the
// check still has teeth — a green result must be earned, not structural.
func TestSlowQueries_IntervalFailsOnNewSlowQuery(t *testing.T) {
	ring := seedGSRing(t, gsCheckNow.Add(-300*time.Second), gsUptime90d-300, map[string]int64{
		"Uptime":           gsUptime90d - 300,
		"Aborted_connects": gsAbortedTotal,
		"Slow_queries":     gsSlowQueryTotal - 3, // 3 new slow queries in the window
	})
	out := runGSCollect(t, ring, gsCheckNow, gsBurstStatus(), gsVars())
	c := gsFindCheck(t, out, "no_slow_queries")
	if c["ok"].(bool) {
		t.Errorf("3 new slow queries in the window must fail: %v", c)
	}
	if d := c["detail"].(string); !strings.Contains(d, "3 new slow queries") {
		t.Errorf("detail should name the movement: %q", d)
	}
}

// TestResolveGSCheckWindow_Fallbacks walks every reason the interval basis is
// refused. Each one must land on the lifetime basis WITH a stated reason —
// falling back silently would leave a check that reads as "measured" when it
// was averaged, which is the whole defect being fixed.
func TestResolveGSCheckWindow_Fallbacks(t *testing.T) {
	cur := map[string]int64{"Uptime": gsUptime90d, "Aborted_connects": gsAbortedTotal, "Slow_queries": gsSlowQueryTotal}

	cases := []struct {
		name        string
		ring        func(t *testing.T) *dbGSSnapshotRing
		wantBasis   string
		wantSeconds int64
		reasonHas   string
	}{
		{
			name:        "nil ring",
			ring:        func(*testing.T) *dbGSSnapshotRing { return nil },
			wantBasis:   dbGSBasisLifetime,
			wantSeconds: gsUptime90d,
			reasonHas:   "no snapshot history",
		},
		{
			name:        "empty ring",
			ring:        func(*testing.T) *dbGSSnapshotRing { return newDBGSSnapshotRing(dbGSGrowthRingCap) },
			wantBasis:   dbGSBasisLifetime,
			wantSeconds: gsUptime90d,
			reasonHas:   "db_global_status_growth",
		},
		{
			name: "server restarted since baseline",
			ring: func(t *testing.T) *dbGSSnapshotRing {
				// Baseline uptime HIGHER than current: the server bounced.
				return seedGSRing(t, gsCheckNow.Add(-300*time.Second), gsUptime90d+1000, map[string]int64{
					"Uptime": gsUptime90d + 1000, "Aborted_connects": gsAbortedTotal + 10,
				})
			},
			wantBasis:   dbGSBasisLifetime,
			wantSeconds: gsUptime90d,
			reasonHas:   "restarted",
		},
		{
			name: "window shorter than the derived floor",
			ring: func(t *testing.T) *dbGSSnapshotRing {
				return seedGSRing(t, gsCheckNow.Add(-60*time.Second), gsUptime90d-60, map[string]int64{
					"Uptime": gsUptime90d - 60, "Aborted_connects": gsAbortedTotal - 1,
				})
			},
			wantBasis:   dbGSBasisLifetime,
			wantSeconds: gsUptime90d,
			reasonHas:   "single event read as a burst",
		},
		{
			name: "baseline too stale",
			ring: func(t *testing.T) *dbGSSnapshotRing {
				stale := dbGSCheckMaxWindowSeconds + 3600
				return seedGSRing(t, gsCheckNow.Add(-time.Duration(stale)*time.Second), gsUptime90d-stale, map[string]int64{
					"Uptime": gsUptime90d - stale, "Aborted_connects": gsAbortedTotal - 5,
				})
			},
			wantBasis:   dbGSBasisLifetime,
			wantSeconds: gsUptime90d,
			reasonHas:   "too stale",
		},
		{
			name: "uptime advanced: uptime basis",
			ring: func(t *testing.T) *dbGSSnapshotRing {
				return seedGSRing(t, gsCheckNow.Add(-300*time.Second), gsUptime90d-300, map[string]int64{
					"Uptime": gsUptime90d - 300, "Aborted_connects": gsAbortedTotal - 1,
				})
			},
			wantBasis:   dbGSBasisUptime,
			wantSeconds: 300,
		},
		{
			name: "uptime flat: wall clock basis",
			ring: func(t *testing.T) *dbGSSnapshotRing {
				return seedGSRing(t, gsCheckNow.Add(-600*time.Second), gsUptime90d, map[string]int64{
					"Uptime": gsUptime90d, "Aborted_connects": gsAbortedTotal - 1,
				})
			},
			wantBasis:   dbGSBasisWallClock,
			wantSeconds: 600,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := resolveGSCheckWindow(tc.ring(t), gsCheckNow, cur, gsUptime90d)
			if w.basis != tc.wantBasis {
				t.Errorf("basis = %q, want %q (reason %q)", w.basis, tc.wantBasis, w.reason)
			}
			if w.seconds != tc.wantSeconds {
				t.Errorf("seconds = %d, want %d", w.seconds, tc.wantSeconds)
			}
			if tc.reasonHas == "" {
				if w.reason != "" {
					t.Errorf("a usable window must carry no fallback reason, got %q", w.reason)
				}
				if !w.usable() {
					t.Errorf("window should be usable")
				}
				return
			}
			if w.usable() {
				t.Errorf("window should not be usable")
			}
			if !strings.Contains(w.reason, tc.reasonHas) {
				t.Errorf("reason %q should mention %q", w.reason, tc.reasonHas)
			}
		})
	}
}

// TestGSCheckWindow_DeltaRefusesUncomputableCounters pins the per-counter
// guards. A counter absent from the baseline has no movement, and one that
// decreased without tripping restart detection would otherwise yield a negative
// rate — which compares as comfortably under the threshold and reads green.
func TestGSCheckWindow_DeltaRefusesUncomputableCounters(t *testing.T) {
	w := dbGSCheckWindow{basis: dbGSBasisUptime, seconds: 300, prev: map[string]int64{"Aborted_connects": 100}}

	if d, ok := w.delta("Aborted_connects", 150); !ok || d != 50 {
		t.Errorf("delta = (%d,%v), want (50,true)", d, ok)
	}
	if _, ok := w.delta("Slow_queries", 7); ok {
		t.Errorf("a counter missing from the baseline must not be computable")
	}
	if _, ok := w.delta("Aborted_connects", 90); ok {
		t.Errorf("a counter that went backwards must not be computable")
	}
	lifetime := dbGSCheckWindow{basis: dbGSBasisLifetime, seconds: 1000}
	if _, ok := lifetime.delta("Aborted_connects", 150); ok {
		t.Errorf("the lifetime basis has no interval to diff against")
	}
	// The reason clause differs by cause, so a reader can tell "no baseline at
	// all" from "baseline lacked this counter".
	if got := w.fallbackReason("Slow_queries"); !strings.Contains(got, "Slow_queries") {
		t.Errorf("per-counter fallback reason should name the counter: %q", got)
	}
	if got := (dbGSCheckWindow{reason: "seeded reason"}).fallbackReason("X"); got != "seeded reason" {
		t.Errorf("window-level reason should win: %q", got)
	}
}

// TestCollectGlobalStatus_PerCheckFallbackIsIndependent covers the mixed case:
// the window is fine, but the baseline snapshot predates one of the counters.
// That check alone falls back; the other keeps its interval.
func TestCollectGlobalStatus_PerCheckFallbackIsIndependent(t *testing.T) {
	ring := seedGSRing(t, gsCheckNow.Add(-300*time.Second), gsUptime90d-300, map[string]int64{
		"Uptime":           gsUptime90d - 300,
		"Aborted_connects": gsAbortedTotal - 2,
		// Slow_queries deliberately absent.
	})
	out := runGSCollect(t, ring, gsCheckNow, gsBurstStatus(), gsVars())

	aborted := gsFindCheck(t, out, "aborted_connects_rate_under_threshold")
	if aborted["basis"] != dbGSBasisUptime {
		t.Errorf("aborted check should keep the interval basis, got %v", aborted["basis"])
	}
	slow := gsFindCheck(t, out, "no_slow_queries")
	if slow["basis"] != dbGSBasisLifetime {
		t.Errorf("slow check should fall back, got %v", slow["basis"])
	}
	if got := slow["windowSeconds"].(int64); got != gsUptime90d {
		t.Errorf("fallen-back check must report the lifetime window, got %d", got)
	}
	if d := slow["detail"].(string); !strings.Contains(d, "no comparable Slow_queries reading") {
		t.Errorf("detail should explain the per-counter fallback: %q", d)
	}
}

// TestCollectGlobalStatus_CheckFieldTypesAreStable guards the map[string]any
// type-erasure trap: one response key assigned int64 on one branch and int on
// another compiles, vets, and passes any golden that drives only one branch,
// then panics in a caller's type assertion. Every branch is exercised here.
func TestCollectGlobalStatus_CheckFieldTypesAreStable(t *testing.T) {
	rings := map[string]*dbGSSnapshotRing{
		"lifetime": nil,
		"interval": seedGSRing(t, gsCheckNow.Add(-300*time.Second), gsUptime90d-300, map[string]int64{
			"Uptime": gsUptime90d - 300, "Aborted_connects": gsAbortedTotal - 1, "Slow_queries": gsSlowQueryTotal,
		}),
	}
	for name, ring := range rings {
		t.Run(name, func(t *testing.T) {
			out := runGSCollect(t, ring, gsCheckNow, gsBurstStatus(), gsVars())
			if _, ok := out["checkWindowSeconds"].(int64); !ok {
				t.Errorf("checkWindowSeconds is %T, want int64", out["checkWindowSeconds"])
			}
			if _, ok := out["checkWindowBasis"].(string); !ok {
				t.Errorf("checkWindowBasis is %T, want string", out["checkWindowBasis"])
			}
			for _, cname := range []string{"no_slow_queries", "aborted_connects_rate_under_threshold"} {
				c := gsFindCheck(t, out, cname)
				if _, ok := c["windowSeconds"].(int64); !ok {
					t.Errorf("%s.windowSeconds is %T, want int64", cname, c["windowSeconds"])
				}
				if _, ok := c["basis"].(string); !ok {
					t.Errorf("%s.basis is %T, want string", cname, c["basis"])
				}
				if _, ok := c["ok"].(bool); !ok {
					t.Errorf("%s.ok is %T, want bool", cname, c["ok"])
				}
				if _, ok := c["detail"].(string); !ok {
					t.Errorf("%s.detail is %T, want string", cname, c["detail"])
				}
			}
			ac := gsFindCheck(t, out, "aborted_connects_rate_under_threshold")
			if _, ok := ac["ratePerHour"].(float64); !ok {
				t.Errorf("ratePerHour is %T, want float64", ac["ratePerHour"])
			}
		})
	}
}

// TestCollectGlobalStatus_DoesNotRecordIntoRing pins the invariant the whole
// design rests on. If db_global_status also recorded, an operator alternating
// the two tools would keep resetting the other's baseline and every rate would
// collapse to the gap between the two calls.
func TestCollectGlobalStatus_DoesNotRecordIntoRing(t *testing.T) {
	ring := seedGSRing(t, gsCheckNow.Add(-300*time.Second), gsUptime90d-300, map[string]int64{
		"Uptime": gsUptime90d - 300, "Aborted_connects": gsAbortedTotal - 1,
	})
	before := ring.list()

	runGSCollect(t, ring, gsCheckNow, gsBurstStatus(), gsVars())
	runGSCollect(t, ring, gsCheckNow.Add(time.Minute), gsBurstStatus(), gsVars())

	after := ring.list()
	if len(after) != len(before) {
		t.Fatalf("ring grew from %d to %d snapshots: db_global_status must not record", len(before), len(after))
	}
	if after[0]["id"] != before[0]["id"] {
		t.Errorf("newest snapshot changed from %v to %v", before[0]["id"], after[0]["id"])
	}
	if after[0]["takenAt"] != before[0]["takenAt"] {
		t.Errorf("baseline timestamp moved: %v -> %v", before[0]["takenAt"], after[0]["takenAt"])
	}
}

// TestDBGSSnapshotRing_Latest checks the accessor itself: newest-first, nil on
// empty, and no mutation of the ring.
func TestDBGSSnapshotRing_Latest(t *testing.T) {
	ring := newDBGSSnapshotRing(dbGSGrowthRingCap)
	if got := ring.latest(); got != nil {
		t.Fatalf("latest on an empty ring = %v, want nil", got)
	}
	for i := 1; i <= 3; i++ {
		if _, err := ring.record(&dbGSStoredSnapshot{
			TakenAt: gsCheckNow.Add(time.Duration(i) * time.Minute),
			Uptime:  int64(1000 + i),
			Vars:    map[string]int64{"Uptime": int64(1000 + i)},
		}, ""); err != nil {
			t.Fatalf("record %d: %v", i, err)
		}
	}
	got := ring.latest()
	if got == nil || got.Uptime != 1003 {
		t.Fatalf("latest = %v, want the newest (uptime 1003)", got)
	}
	if n := len(ring.list()); n != 3 {
		t.Errorf("latest must not consume snapshots: ring holds %d, want 3", n)
	}
	if again := ring.latest(); again != got {
		t.Errorf("repeated latest returned a different snapshot")
	}
}

// TestDBGSSnapshotRing_LatestConcurrentWithRecord exercises the shared state
// under genuine concurrency. `-race` only reports races the run actually
// executes, and the rest of this suite is sequential, so without a test that
// really does read the ring while another goroutine appends to it a `-race`
// pass would prove nothing about the new reader taking the mutex.
func TestDBGSSnapshotRing_LatestConcurrentWithRecord(t *testing.T) {
	ring := newDBGSSnapshotRing(8)
	if _, err := ring.record(&dbGSStoredSnapshot{
		TakenAt: gsCheckNow, Uptime: 1000, Vars: map[string]int64{"Uptime": 1000, "Aborted_connects": 1},
	}, ""); err != nil {
		t.Fatalf("seed: %v", err)
	}

	const writers, readers, perGoroutine = 8, 8, 25
	var wg sync.WaitGroup
	var mu sync.Mutex
	var failures []string
	fail := func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		failures = append(failures, fmt.Sprintf(format, args...))
	}

	ids := make(chan string, writers*perGoroutine)
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perGoroutine; i++ {
				snap := &dbGSStoredSnapshot{
					TakenAt: gsCheckNow.Add(time.Duration(w*perGoroutine+i) * time.Second),
					Uptime:  int64(1000 + w*perGoroutine + i),
					Vars:    map[string]int64{"Uptime": int64(1000 + w*perGoroutine + i), "Aborted_connects": 1},
				}
				if _, err := ring.record(snap, ""); err != nil {
					fail("record: %v", err)
					return
				}
				ids <- snap.ID
			}
		}(w)
	}
	for r := 0; r < readers; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perGoroutine; i++ {
				got := ring.latest()
				if got == nil {
					fail("latest returned nil while the ring was non-empty")
					return
				}
				// Read the shared map the same way resolveGSCheckWindow does.
				if got.Vars["Uptime"] != got.Uptime {
					fail("snapshot %s is internally inconsistent: Vars[Uptime]=%d Uptime=%d",
						got.ID, got.Vars["Uptime"], got.Uptime)
					return
				}
			}
		}()
	}
	wg.Wait()
	close(ids)

	// Every concurrently recorded snapshot must have received a distinct id —
	// proof the seq++ and the append share one critical section.
	seen := map[string]bool{}
	for id := range ids {
		if seen[id] {
			fail("duplicate snapshot id %q", id)
		}
		seen[id] = true
	}
	if len(seen) != writers*perGoroutine {
		fail("got %d distinct ids, want %d", len(seen), writers*perGoroutine)
	}
	if n := len(ring.list()); n != 8 {
		fail("ring should be capped at 8, holds %d", n)
	}
	for _, f := range failures {
		t.Error(f)
	}
}

// TestDBGlobalStatus_DescriptionMentionsCheckWindow keeps the new semantics in
// the registered Description STRING, which is what the agent reads — a
// cross-reference that lives only in a Go doc-comment is invisible to it.
func TestDBGlobalStatus_DescriptionMentionsCheckWindow(t *testing.T) {
	reg := NewRegistry()
	RegisterDBGlobalStatusTool(reg, DBDeps{})
	tool, ok := reg.Get("db_global_status")
	if !ok {
		t.Fatal("db_global_status not registered")
	}
	for _, kw := range []string{
		"db_global_status_growth",
		"lifetimeAverage",
		"windowSeconds",
		"ratePerHour",
		"checkWindowBasis",
		"no_slow_queries",
		"aborted_connects_rate_under_threshold",
	} {
		if !strings.Contains(tool.Description, kw) {
			t.Errorf("description missing %q", kw)
		}
	}
}

// TestDBGSParseInts mirrors readGlobalStatusInts' parse rule: the restart
// heuristic is shared between the two tools, so the readings it compares have
// to be filtered the same way. The non-numeric rows named here are the ones a
// real MySQL 8 actually returns.
func TestDBGSParseInts(t *testing.T) {
	got := dbGSParseInts(map[string]string{
		"Uptime":                         "3600",
		"Aborted_connects":               " 42 ",
		"Innodb_buffer_pool_dump_status": "Dumping of buffer pool not started",
		"Ssl_cipher_list":                "TLS_AES_256_GCM_SHA384:TLS_CHACHA20",
		"Innodb_rows_read":               "-5",
		"Threads_cached":                 "",
	})
	want := map[string]int64{"Uptime": 3600, "Aborted_connects": 42, "Innodb_rows_read": -5}
	if len(got) != len(want) {
		t.Fatalf("parsed %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %d, want %d", k, got[k], v)
		}
	}
}

// compile-time guard: collectGlobalStatus keeps the signature the handler and
// the tests depend on.
var _ = func(ctx context.Context, db *sql.DB, r *dbGSSnapshotRing, now time.Time) (map[string]any, error) {
	return collectGlobalStatus(ctx, db, r, now)
}
