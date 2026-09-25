package mcpserver

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"
)

// The buffer-pool hit-ratio check used to divide two lifetime counters, so it
// reported the ratio since server start and could never move during the
// cold-cache episode it was written to catch. These are the numbers that make
// the point: a server that has served ten billion read requests at 99.9%
// absorbs an interval of nine million straight-from-disk reads without the
// lifetime ratio leaving the green zone.
const (
	gsBufLifetimeReads    = int64(10_000_000)
	gsBufLifetimeRequests = int64(10_000_000_000)

	// Movement inside the measured window: 9M of the 10M read requests served in
	// the last 300 s missed the pool. That is a 10% hit ratio.
	gsBufWindowReads    = int64(9_000_000)
	gsBufWindowRequests = int64(10_000_000)
)

// gsBufStatus is a CURRENT reading carrying the given buffer pool counters. The
// non-buffer counters are held at values that keep the other four checks quiet
// so a failure in this file can only come from the check under test.
func gsBufStatus(reads, readRequests int64) [][2]string {
	return [][2]string{
		{"Uptime", fmt.Sprint(gsUptime90d)},
		{"Queries", "100000000"},
		{"Threads_connected", "40"},
		{"Slow_queries", "0"},
		{"Aborted_connects", "0"},
		{"Innodb_buffer_pool_reads", fmt.Sprint(reads)},
		{"Innodb_buffer_pool_read_requests", fmt.Sprint(readRequests)},
	}
}

// gsBufRing seeds a baseline 300 s back whose buffer pool counters sit the given
// deltas below the current reading, so the resolved window is exactly those
// deltas. Aborted_connects and Slow_queries are carried at their current values
// so the sibling checks stay on the interval basis and out of the way.
func gsBufRing(t *testing.T, curReads, curRequests, readsDelta, requestsDelta int64) *dbGSSnapshotRing {
	t.Helper()
	return seedGSRing(t, gsCheckNow.Add(-300*time.Second), gsUptime90d-300, map[string]int64{
		"Uptime":                           gsUptime90d - 300,
		"Aborted_connects":                 0,
		"Slow_queries":                     0,
		"Innodb_buffer_pool_reads":         curReads - readsDelta,
		"Innodb_buffer_pool_read_requests": curRequests - requestsDelta,
	})
}

// TestBufferPoolHitRatio_LifetimeMissesIntervalCatches is the regression for the
// bug itself, asserted in both directions in one test so the fix cannot be read
// as a coincidence: the SAME current reading passes on the lifetime basis and
// fails on the interval basis.
func TestBufferPoolHitRatio_LifetimeMissesIntervalCatches(t *testing.T) {
	status := gsBufStatus(gsBufLifetimeReads, gsBufLifetimeRequests)

	// (1) No history: the old behaviour. 99.9% since server start, check passes
	// straight through a nine-million-read cache emergency.
	lifetimeOut := runGSCollect(t, nil, gsCheckNow, status, gsVars())
	lc := gsFindCheck(t, lifetimeOut, "innodb_buffer_pool_hit_ratio_ok")
	if lc["basis"] != dbGSBasisLifetime {
		t.Errorf("basis = %v, want %q", lc["basis"], dbGSBasisLifetime)
	}
	if !lc["ok"].(bool) {
		t.Errorf("the lifetime ratio must still pass — that IS the bug being fixed: %v", lc)
	}
	if got := lc["hitRatioPct"].(float64); got != 99.9 {
		t.Errorf("lifetime hitRatioPct = %v, want 99.9", got)
	}
	if got := lc["windowSeconds"].(int64); got != gsUptime90d {
		t.Errorf("lifetime windowSeconds = %d, want the whole of Uptime (%d)", got, gsUptime90d)
	}
	if d := lc["detail"].(string); !strings.Contains(d, "cannot move during a cold-cache episode") {
		t.Errorf("lifetime detail should say why it cannot fire: %q", d)
	}

	// (2) With a baseline 300 s back: the same reading, measured. 9M misses out
	// of 10M requests is a 10% hit ratio and the check fails.
	ring := gsBufRing(t, gsBufLifetimeReads, gsBufLifetimeRequests, gsBufWindowReads, gsBufWindowRequests)
	out := runGSCollect(t, ring, gsCheckNow, status, gsVars())
	c := gsFindCheck(t, out, "innodb_buffer_pool_hit_ratio_ok")
	if c["basis"] != dbGSBasisUptime {
		t.Errorf("basis = %v, want %q", c["basis"], dbGSBasisUptime)
	}
	if c["ok"].(bool) {
		t.Errorf("a 10%% interval hit ratio must fail the %.0f%% floor: %v", innodbBufferPoolHitFloor*100, c)
	}
	if got := c["hitRatioPct"].(float64); got != 10.0 {
		t.Errorf("interval hitRatioPct = %v, want 10.0", got)
	}
	if got := c["windowSeconds"].(int64); got != 300 {
		t.Errorf("windowSeconds = %d, want 300", got)
	}
	if got := c["reads"].(int64); got != gsBufWindowReads {
		t.Errorf("reads = %d, want the window delta %d", got, gsBufWindowReads)
	}
	if got := c["readRequests"].(int64); got != gsBufWindowRequests {
		t.Errorf("readRequests = %d, want the window delta %d", got, gsBufWindowRequests)
	}
	if out["allOk"].(bool) {
		t.Errorf("allOk must be false once the buffer pool check fails")
	}

	// The top-level derived field is documented as the lifetime ratio and must
	// stay that way — the interval reading lives on the check, so an operator
	// reading both sees a history and a right-now, not two disagreeing numbers.
	if got := out["innodbBufferPoolHitRatioPct"].(float64); got != 99.9 {
		t.Errorf("top-level innodbBufferPoolHitRatioPct = %v, want the lifetime 99.9", got)
	}
}

// TestBufferPoolMinSampleDerivedFromFloor pins the arithmetic that makes the
// minimum sample a derivation rather than a guess: hit = 1 - misses/requests
// clears floor F only when requests >= misses/(1-F). If someone moves the floor
// without re-deriving the sample, this fails loudly.
func TestBufferPoolMinSampleDerivedFromFloor(t *testing.T) {
	floor := innodbBufferPoolHitFloor
	if want := int64(math.Ceil(1.0 / (1.0 - floor))); dbGSBufferPoolMinReadRequests != want {
		t.Fatalf("dbGSBufferPoolMinReadRequests = %d, want %d (1/(1-%.2f))",
			dbGSBufferPoolMinReadRequests, want, floor)
	}
	// One miss over the minimum sample must NOT trip the check — that is the
	// entire reason the sample floor exists. The comparison is a non-strict >=,
	// so landing exactly ON the floor passes and no +1 is needed here (unlike
	// dbGSCheckMinWindowSeconds, whose check is a strict <).
	atFloor := 1.0 - 1.0/float64(dbGSBufferPoolMinReadRequests)
	if atFloor < floor {
		t.Errorf("a single miss over the %d-request floor is %.4f, which trips the %.2f floor",
			dbGSBufferPoolMinReadRequests, atFloor, floor)
	}
	// ...and one request below the floor, it would have.
	if justUnder := 1.0 - 1.0/float64(dbGSBufferPoolMinReadRequests-1); justUnder >= floor {
		t.Errorf("the sample floor is one request too generous: at %d a single miss is still %.4f",
			dbGSBufferPoolMinReadRequests-1, justUnder)
	}
}

// TestBufferPoolHitRatio_ExactlyAtFloorPasses walks the boundary the derivation
// above rests on through the real collector: the smallest sample the check will
// judge, carrying the single miss it is sized for.
func TestBufferPoolHitRatio_ExactlyAtFloorPasses(t *testing.T) {
	reads, requests := gsBufLifetimeReads, gsBufLifetimeRequests
	ring := gsBufRing(t, reads, requests, 1, dbGSBufferPoolMinReadRequests)
	c := gsFindCheck(t, runGSCollect(t, ring, gsCheckNow, gsBufStatus(reads, requests), gsVars()),
		"innodb_buffer_pool_hit_ratio_ok")
	if c["basis"] != dbGSBasisUptime {
		t.Fatalf("basis = %v, want the interval basis at exactly the sample floor", c["basis"])
	}
	if got := c["hitRatioPct"].(float64); got != innodbBufferPoolHitFloor*100 {
		t.Errorf("hitRatioPct = %v, want exactly the floor %v", got, innodbBufferPoolHitFloor*100)
	}
	if !c["ok"].(bool) {
		t.Errorf("landing exactly on the floor must pass (the comparison is >=): %v", c)
	}
}

// TestBufferPoolHitRatio_FallbackCases covers every way the interval reading can
// be refused. Each one must fall back to the labelled lifetime ratio with a
// stated reason rather than report a number it cannot stand behind — a check
// that disappears is invisible in allOk, and a fabricated one is worse.
func TestBufferPoolHitRatio_FallbackCases(t *testing.T) {
	reads, requests := gsBufLifetimeReads, gsBufLifetimeRequests
	cases := []struct {
		name       string
		readsDelta int64
		reqDelta   int64
		wantReason string
	}{
		{
			// The guard the two rate checks do not need: an interval that served
			// no read requests is idle, not a 0% hit ratio. 1 - 0/0 is NaN, and
			// json.Marshal refuses NaN — this fallback is what keeps a quiet
			// server from taking the whole response down.
			name: "idle interval", readsDelta: 0, reqDelta: 0,
			wantReason: "served no buffer pool read requests at all",
		},
		{
			// One miss in a 19-request interval is 94.74% — under the floor. The
			// server is fine; the sample is too small to say anything.
			name: "sample below the floor", readsDelta: 1, reqDelta: dbGSBufferPoolMinReadRequests - 1,
			wantReason: "served only 19 buffer pool read requests",
		},
		{
			// Cannot happen on a healthy server, and would yield a negative hit
			// ratio if it were divided anyway.
			name: "more disk reads than read requests", readsDelta: 50, reqDelta: 40,
			wantReason: "which cannot happen and would yield a negative hit ratio",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ring := gsBufRing(t, reads, requests, tc.readsDelta, tc.reqDelta)
			out := runGSCollect(t, ring, gsCheckNow, gsBufStatus(reads, requests), gsVars())
			c := gsFindCheck(t, out, "innodb_buffer_pool_hit_ratio_ok")
			if c["basis"] != dbGSBasisLifetime {
				t.Errorf("basis = %v, want %q", c["basis"], dbGSBasisLifetime)
			}
			if got := c["hitRatioPct"].(float64); got != 99.9 {
				t.Errorf("hitRatioPct = %v, want the lifetime 99.9", got)
			}
			if got := c["readRequests"].(int64); got != requests {
				t.Errorf("readRequests = %d, want the lifetime counter %d", got, requests)
			}
			if d := c["detail"].(string); !strings.Contains(d, tc.wantReason) {
				t.Errorf("detail should name the reason %q, got %q", tc.wantReason, d)
			}
			// The window itself was perfectly usable — only this check fell back,
			// which is what per-check basis buys over a global one.
			if out["checkWindowBasis"] != dbGSBasisUptime {
				t.Errorf("the window is fine; only the check fell back. checkWindowBasis = %v",
					out["checkWindowBasis"])
			}
			if ac := gsFindCheck(t, out, "aborted_connects_rate_under_threshold"); ac["basis"] != dbGSBasisUptime {
				t.Errorf("the sibling rate check must keep the interval basis, got %v", ac["basis"])
			}
		})
	}
}

// TestBufferPoolHitRatio_MissingOperandFallsBack covers the two per-counter
// fallbacks: a baseline snapshot that carries one buffer pool counter but not
// the other cannot produce a ratio, and naming WHICH operand is missing is the
// difference between a diagnosable message and a shrug.
func TestBufferPoolHitRatio_MissingOperandFallsBack(t *testing.T) {
	reads, requests := gsBufLifetimeReads, gsBufLifetimeRequests
	cases := []struct {
		name       string
		baseline   map[string]int64
		wantReason string
	}{
		{
			name: "read_requests missing",
			baseline: map[string]int64{
				"Uptime": gsUptime90d - 300, "Aborted_connects": 0, "Slow_queries": 0,
				"Innodb_buffer_pool_reads": reads - gsBufWindowReads,
			},
			wantReason: "no comparable Innodb_buffer_pool_read_requests reading",
		},
		{
			name: "reads missing",
			baseline: map[string]int64{
				"Uptime": gsUptime90d - 300, "Aborted_connects": 0, "Slow_queries": 0,
				"Innodb_buffer_pool_read_requests": requests - gsBufWindowRequests,
			},
			wantReason: "no comparable Innodb_buffer_pool_reads reading",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ring := seedGSRing(t, gsCheckNow.Add(-300*time.Second), gsUptime90d-300, tc.baseline)
			c := gsFindCheck(t, runGSCollect(t, ring, gsCheckNow, gsBufStatus(reads, requests), gsVars()),
				"innodb_buffer_pool_hit_ratio_ok")
			if c["basis"] != dbGSBasisLifetime {
				t.Errorf("basis = %v, want %q", c["basis"], dbGSBasisLifetime)
			}
			if d := c["detail"].(string); !strings.Contains(d, tc.wantReason) {
				t.Errorf("detail should name the missing operand %q, got %q", tc.wantReason, d)
			}
		})
	}
}

// TestBufferPoolCheck_FieldTypesAreStable guards the map[string]any
// type-erasure trap on the new fields: a key assigned int64 on the interval
// branch and int on the lifetime branch compiles, vets, and passes any golden
// that drives only one of them, then panics in a caller's type assertion. Both
// branches are exercised here.
func TestBufferPoolCheck_FieldTypesAreStable(t *testing.T) {
	reads, requests := gsBufLifetimeReads, gsBufLifetimeRequests
	rings := map[string]*dbGSSnapshotRing{
		"lifetime": nil,
		"interval": gsBufRing(t, reads, requests, gsBufWindowReads, gsBufWindowRequests),
	}
	for name, ring := range rings {
		t.Run(name, func(t *testing.T) {
			out := runGSCollect(t, ring, gsCheckNow, gsBufStatus(reads, requests), gsVars())
			c := gsFindCheck(t, out, "innodb_buffer_pool_hit_ratio_ok")
			for _, k := range []string{"basis", "detail"} {
				if _, ok := c[k].(string); !ok {
					t.Errorf("%s is %T, want string", k, c[k])
				}
			}
			for _, k := range []string{"windowSeconds", "reads", "readRequests"} {
				if _, ok := c[k].(int64); !ok {
					t.Errorf("%s is %T, want int64", k, c[k])
				}
			}
			if _, ok := c["hitRatioPct"].(float64); !ok {
				t.Errorf("hitRatioPct is %T, want float64", c["hitRatioPct"])
			}
			if _, ok := c["ok"].(bool); !ok {
				t.Errorf("ok is %T, want bool", c["ok"])
			}
			// Both branches must survive serialization: a NaN or +Inf ratio is
			// not a bad number here, it is a marshal error that takes the entire
			// response down.
			if _, err := json.Marshal(out); err != nil {
				t.Errorf("response is not serializable: %v", err)
			}
		})
	}
}

// TestBufferPoolCheck_DescriptionMentionsIntervalBasis keeps the agent-facing
// contract honest. The keywords have to live in the registered Description
// STRING, not the doc-comment above the func — the perennial gotcha this suite
// already guards for the two rate checks.
func TestBufferPoolCheck_DescriptionMentionsIntervalBasis(t *testing.T) {
	reg := NewRegistry()
	RegisterDBGlobalStatusTool(reg, DBDeps{})
	tool, ok := reg.Get("db_global_status")
	if !ok {
		t.Fatal("db_global_status not registered")
	}
	for _, kw := range []string{
		"innodb_buffer_pool_hit_ratio_ok",
		"hitRatioPct",
		"readRequests",
		"lifetimeAverage",
		"20 read requests",
		"innodbBufferPoolHitRatioPct",
	} {
		if !strings.Contains(tool.Description, kw) {
			t.Errorf("description missing %q", kw)
		}
	}
}
