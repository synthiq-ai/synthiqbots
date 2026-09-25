package mcpserver

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

// growthRows is the PROCESSLIST column set every fixture below returns.
func growthRows() *sqlmock.Rows {
	return sqlmock.NewRows([]string{"USER", "HOST", "DB", "COMMAND", "STATE"})
}

// expectGrowthScan queues one PROCESSLIST scan. includeSleep=false binds the
// "Sleep" exclusion ahead of the LIMIT, matching collectConnectionFacets.
func expectGrowthScan(mock sqlmock.Sqlmock, includeSleep bool, rows *sqlmock.Rows) {
	e := mock.ExpectQuery(regexp.QuoteMeta("FROM information_schema.PROCESSLIST"))
	if includeSleep {
		e = e.WithArgs(dbConnSummaryScanCap + 1)
	} else {
		e = e.WithArgs("Sleep", dbConnSummaryScanCap+1)
	}
	e.WillReturnRows(rows)
}

func entriesByKey(t *testing.T, out map[string]any, facet string) map[string]dbConnGrowthEntry {
	t.Helper()
	list, ok := out[facet].([]dbConnGrowthEntry)
	if !ok {
		t.Fatalf("%s is not []dbConnGrowthEntry: %T", facet, out[facet])
	}
	m := map[string]dbConnGrowthEntry{}
	for _, e := range list {
		m[e.Key] = e
	}
	return m
}

// TestDBConnGrowth_DescriptionMentionsContext anchors the description to the
// keywords an agent latches on to for "what's growing / is something leaking
// connections?", plus the cross-references that route it to the point-in-time
// and per-row tools instead. Same guard shape as db_connection_summary's.
func TestDBConnGrowth_DescriptionMentionsContext(t *testing.T) {
	reg := NewRegistry()
	RegisterDBConnectionSummaryGrowthTool(reg, DBDeps{})
	tool, ok := reg.Get("db_connection_summary_growth")
	if !ok {
		t.Fatal("db_connection_summary_growth not registered")
	}
	for _, kw := range []string{
		"PROCESSLIST", "db_connection_summary", "db_processlist", "byUser", "byHost",
		"prevCount", "delta", "snapshotId", "storedSnapshots", "firstSnapshot",
		"baselineFilterMismatch", "facetKeysTotal", "topN", "ops_ro", "Read-only",
	} {
		if !strings.Contains(tool.Description, kw) {
			t.Errorf("description missing %q: %s", kw, tool.Description)
		}
	}
}

// TestDBConnGrowth_ReadOnlyAnnotation — recording a snapshot is in-process
// bookkeeping, not a server mutation; the tool must stay read-only or it would
// be gated behind OPS_ADMIN_ALLOW_ACTIONS.
func TestDBConnGrowth_ReadOnlyAnnotation(t *testing.T) {
	reg := NewRegistry()
	RegisterDBConnectionSummaryGrowthTool(reg, DBDeps{})
	tool, _ := reg.Get("db_connection_summary_growth")
	if tool.Destructive {
		t.Errorf("db_connection_summary_growth marked destructive — should be read-only")
	}
	if !strings.Contains(string(tool.Annotations), "readOnlyHint") {
		t.Errorf("missing readOnlyHint annotation: %s", string(tool.Annotations))
	}
}

func TestDBConnGrowth_NoPoolReturnsError(t *testing.T) {
	reg := NewRegistry()
	RegisterDBConnectionSummaryGrowthTool(reg, DBDeps{}) // QueryDB nil
	tool, _ := reg.Get("db_connection_summary_growth")
	resp := tool.Handler(context.Background(), json.RawMessage(`{}`), "")
	got, _ := resp.(map[string]any)
	if msg, _ := got["error"].(string); !strings.Contains(msg, "ops_ro pool not configured") {
		t.Errorf("expected pool-not-configured error, got %v", got)
	}
}

// TestDBConnGrowth_InvalidSortByRejected — an unknown sortBy must error BEFORE
// the scan (no ExpectQuery is queued, so a stray query would fail the mock).
// Case matters: "Growth" is not "growth".
func TestDBConnGrowth_InvalidSortByRejected(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	reg := NewRegistry()
	RegisterDBConnectionSummaryGrowthTool(reg, DBDeps{QueryDB: db})
	tool, _ := reg.Get("db_connection_summary_growth")

	for _, bad := range []string{`{"sortBy":"Growth"}`, `{"sortBy":"delta"}`, `{"sortBy":"count; DROP TABLE x"}`} {
		resp := tool.Handler(context.Background(), json.RawMessage(bad), "")
		got, _ := resp.(map[string]any)
		if msg, _ := got["error"].(string); !strings.Contains(msg, "invalid sortBy") {
			t.Errorf("args %s: expected invalid sortBy error, got %v", bad, got)
		}
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestDBConnGrowth_TopNClamping — topN<=0 falls back to 10, topN>50 clamps to
// 50, and the post-clamp value is echoed so the agent can see the cut.
func TestDBConnGrowth_TopNClamping(t *testing.T) {
	cases := []struct {
		args string
		want int
	}{
		{`{}`, dbConnGrowthDefaultTop},
		{`{"topN":0}`, dbConnGrowthDefaultTop},
		{`{"topN":-5}`, dbConnGrowthDefaultTop},
		{`{"topN":7}`, 7},
		{`{"topN":99999}`, dbConnGrowthHardMaxTop},
	}
	for _, c := range cases {
		db, mock, err := sqlmock.New()
		if err != nil {
			t.Fatal(err)
		}
		reg := NewRegistry()
		RegisterDBConnectionSummaryGrowthTool(reg, DBDeps{QueryDB: db})
		tool, _ := reg.Get("db_connection_summary_growth")
		expectGrowthScan(mock, false, growthRows())

		resp := tool.Handler(context.Background(), json.RawMessage(c.args), "")
		got, _ := resp.(map[string]any)
		if msg, ok := got["error"]; ok {
			t.Fatalf("args %s: unexpected error: %v", c.args, msg)
		}
		if got["topN"].(int) != c.want {
			t.Errorf("args %s: topN = %v, want %d", c.args, got["topN"], c.want)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("args %s: unmet mock expectations: %v", c.args, err)
		}
		db.Close()
	}
}

// TestDBConnGrowth_FirstSnapshotHasNoBaseline — the very first call on a fresh
// ring has nothing to diff against. Deltas equal the raw counts (arithmetically
// right against an empty baseline), so the response must say firstSnapshot and
// carry a note; otherwise the agent reads a cold start as a connection spike.
func TestDBConnGrowth_FirstSnapshotHasNoBaseline(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	expectGrowthScan(mock, false, growthRows().
		AddRow("acore", "10.0.0.1:100", "acore_world", "Query", "executing").
		AddRow("acore", "10.0.0.1:101", "acore_world", "Query", "executing"))

	ring := newDBConnSnapshotRing(dbConnGrowthRingCap)
	out, err := collectConnectionGrowth(context.Background(), db, ring, time.Now(), false, 0, 10, "growth", "")
	if err != nil {
		t.Fatalf("collectConnectionGrowth: %v", err)
	}
	if !out["firstSnapshot"].(bool) {
		t.Errorf("firstSnapshot should be true on a fresh ring")
	}
	if out["comparedTo"] != nil || out["baselineTakenAt"] != nil {
		t.Errorf("comparedTo/baselineTakenAt should be nil: %v / %v", out["comparedTo"], out["baselineTakenAt"])
	}
	if out["snapshotId"].(string) != "snap-1" {
		t.Errorf("snapshotId = %v, want snap-1", out["snapshotId"])
	}
	if out["total"].(int) != 2 || out["totalDelta"].(int) != 2 || out["baselineTotal"].(int) != 0 {
		t.Errorf("total/totalDelta/baselineTotal = %v/%v/%v, want 2/2/0", out["total"], out["totalDelta"], out["baselineTotal"])
	}
	if out["intervalSeconds"].(int) != 0 {
		t.Errorf("intervalSeconds should be 0 with no baseline: %v", out["intervalSeconds"])
	}
	byUser := entriesByKey(t, out, "byUser")
	if e := byUser["acore"]; e.Count != 2 || e.PrevCount != 0 || e.Delta != 2 {
		t.Errorf("acore entry = %+v, want count 2 prev 0 delta 2", e)
	}
	notes := out["notes"].([]string)
	if len(notes) != 1 || !strings.Contains(notes[0], "no comparable prior snapshot") {
		t.Errorf("expected a first-snapshot note, got %v", notes)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestDBConnGrowth_GoldenDeltaAgainstPriorSnapshot — the whole point of the
// tool. Two scans five minutes apart over the same ring: one bucket grows
// (acore 2->5), one is flat (ops_ro 1->1), one is brand new (newbie 0->1) and
// one VANISHES (legacy 1->0). The vanished bucket must still appear with a
// negative delta, and growth ordering must put the fastest riser first.
func TestDBConnGrowth_GoldenDeltaAgainstPriorSnapshot(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// Baseline: 4 connections.
	expectGrowthScan(mock, false, growthRows().
		AddRow("acore", "10.0.0.1:100", "acore_world", "Query", "executing").
		AddRow("acore", "10.0.0.1:101", "acore_world", "Query", "executing").
		AddRow("ops_ro", "10.0.0.2:200", "acore_characters", "Query", "Sending data").
		AddRow("legacy", "10.0.0.3:300", "acore_auth", "Query", "executing"))
	// Current: 7 connections — acore fans out, legacy is gone, newbie appears.
	expectGrowthScan(mock, false, growthRows().
		AddRow("acore", "10.0.0.1:110", "acore_world", "Query", "executing").
		AddRow("acore", "10.0.0.1:111", "acore_world", "Query", "executing").
		AddRow("acore", "10.0.0.1:112", "acore_world", "Query", "executing").
		AddRow("acore", "10.0.0.1:113", "acore_world", "Query", "executing").
		AddRow("acore", "10.0.0.1:114", "acore_world", "Query", "executing").
		AddRow("ops_ro", "10.0.0.2:210", "acore_characters", "Query", "Sending data").
		AddRow("newbie", "10.0.0.4:400", "acore_world", "Query", "executing"))

	ring := newDBConnSnapshotRing(dbConnGrowthRingCap)
	t0 := time.Date(2026, 7, 28, 16, 0, 0, 0, time.UTC)
	if _, err := collectConnectionGrowth(context.Background(), db, ring, t0, false, 0, 10, "growth", ""); err != nil {
		t.Fatalf("baseline scan: %v", err)
	}
	out, err := collectConnectionGrowth(context.Background(), db, ring, t0.Add(5*time.Minute), false, 0, 10, "growth", "")
	if err != nil {
		t.Fatalf("current scan: %v", err)
	}

	if out["firstSnapshot"].(bool) {
		t.Errorf("second call should have a baseline")
	}
	if out["snapshotId"].(string) != "snap-2" || out["comparedTo"].(string) != "snap-1" {
		t.Errorf("snapshotId/comparedTo = %v/%v, want snap-2/snap-1", out["snapshotId"], out["comparedTo"])
	}
	if out["baselineTakenAt"].(string) != "2026-07-28T16:00:00Z" {
		t.Errorf("baselineTakenAt = %v", out["baselineTakenAt"])
	}
	if out["intervalSeconds"].(int) != 300 {
		t.Errorf("intervalSeconds = %v, want 300", out["intervalSeconds"])
	}
	if out["total"].(int) != 7 || out["baselineTotal"].(int) != 4 || out["totalDelta"].(int) != 3 {
		t.Errorf("total/baselineTotal/totalDelta = %v/%v/%v, want 7/4/3",
			out["total"], out["baselineTotal"], out["totalDelta"])
	}
	if out["baselineFilterMismatch"].(bool) {
		t.Errorf("same filters must not flag a mismatch")
	}

	// byUser union = acore, ops_ro, newbie, legacy.
	byUser := out["byUser"].([]dbConnGrowthEntry)
	wantOrder := []dbConnGrowthEntry{
		{Key: "acore", Count: 5, PrevCount: 2, Delta: 3},
		{Key: "newbie", Count: 1, PrevCount: 0, Delta: 1},
		{Key: "ops_ro", Count: 1, PrevCount: 1, Delta: 0},
		{Key: "legacy", Count: 0, PrevCount: 1, Delta: -1},
	}
	if len(byUser) != len(wantOrder) {
		t.Fatalf("byUser len = %d, want %d: %+v", len(byUser), len(wantOrder), byUser)
	}
	for i, w := range wantOrder {
		if byUser[i] != w {
			t.Errorf("byUser[%d] = %+v, want %+v", i, byUser[i], w)
		}
	}

	// Host ports are normalized away, so the five new acore connections land in
	// ONE bucket instead of five per-port keys.
	byHost := entriesByKey(t, out, "byHost")
	if e := byHost["10.0.0.1"]; e.Count != 5 || e.PrevCount != 2 || e.Delta != 3 {
		t.Errorf("byHost 10.0.0.1 = %+v, want count 5 prev 2 delta 3", e)
	}
	if e := byHost["10.0.0.3"]; e.Count != 0 || e.PrevCount != 1 || e.Delta != -1 {
		t.Errorf("vanished host should surface negative: %+v", e)
	}
	byDb := entriesByKey(t, out, "byDb")
	if e := byDb["acore_world"]; e.Count != 6 || e.PrevCount != 2 || e.Delta != 4 {
		t.Errorf("byDb acore_world = %+v, want count 6 prev 2 delta 4", e)
	}
	byState := entriesByKey(t, out, "byState")
	if e := byState["executing"]; e.Count != 6 || e.PrevCount != 3 || e.Delta != 3 {
		t.Errorf("byState executing = %+v, want count 6 prev 3 delta 3", e)
	}
	byCommand := entriesByKey(t, out, "byCommand")
	if e := byCommand["Query"]; e.Count != 7 || e.PrevCount != 4 || e.Delta != 3 {
		t.Errorf("byCommand Query = %+v, want count 7 prev 4 delta 3", e)
	}

	keys := out["facetKeysTotal"].(map[string]int)
	for facet, want := range map[string]int{"byUser": 4, "byHost": 4, "byDb": 3, "byCommand": 1, "byState": 2} {
		if keys[facet] != want {
			t.Errorf("facetKeysTotal[%s] = %d, want %d", facet, keys[facet], want)
		}
	}
	if len(out["notes"].([]string)) != 0 {
		t.Errorf("clean comparable diff should carry no notes: %v", out["notes"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestDBConnGrowth_SortOrders drives diffConnFacet directly over one fixed
// before/after pair so each ordering is provable in isolation, including the
// secondary tiebreakers that keep equal rows deterministic under Go's
// randomized map iteration.
func TestDBConnGrowth_SortOrders(t *testing.T) {
	cur := map[string]int{"grew": 9, "flat": 4, "new": 2, "shrank": 1}
	prev := map[string]int{"grew": 4, "flat": 4, "shrank": 6, "gone": 3}
	cases := []struct {
		sortKey string
		want    []string
	}{
		// delta: grew +5, new +2, flat 0, gone -3, shrank -5
		{"growth", []string{"grew", "new", "flat", "gone", "shrank"}},
		{"shrink", []string{"shrank", "gone", "flat", "new", "grew"}},
		// count desc: grew 9, flat 4, new 2, shrank 1, gone 0
		{"count", []string{"grew", "flat", "new", "shrank", "gone"}},
	}
	for _, c := range cases {
		got := diffConnFacet(cur, prev, c.sortKey)
		if len(got) != len(c.want) {
			t.Fatalf("%s: len = %d, want %d: %+v", c.sortKey, len(got), len(c.want), got)
		}
		for i, k := range c.want {
			if got[i].Key != k {
				t.Errorf("%s: position %d = %q, want %q (full: %+v)", c.sortKey, i, got[i].Key, k, got)
			}
		}
	}
}

// TestDBConnGrowth_SortTiebreakersDeterministic — rows tied on the primary key
// must fall through to the documented secondary, then to key-asc. Without this
// the same data would come back in a different order every call.
func TestDBConnGrowth_SortTiebreakersDeterministic(t *testing.T) {
	// All three grew by exactly 1; "big" is now largest, "mid" next, and
	// "aaa"/"zzz" tie on both delta and count so key-asc decides.
	cur := map[string]int{"big": 11, "mid": 6, "aaa": 3, "zzz": 3}
	prev := map[string]int{"big": 10, "mid": 5, "aaa": 2, "zzz": 2}
	got := diffConnFacet(cur, prev, "growth")
	want := []string{"big", "mid", "aaa", "zzz"}
	for i, k := range want {
		if got[i].Key != k {
			t.Errorf("growth tiebreak position %d = %q, want %q (full: %+v)", i, got[i].Key, k, got)
		}
	}
	// shrink ties fall through to prevCount desc: both drained by 2, the one
	// that WAS bigger comes first.
	got = diffConnFacet(map[string]int{"small": 1, "large": 8}, map[string]int{"small": 3, "large": 10}, "shrink")
	if got[0].Key != "large" {
		t.Errorf("shrink tiebreak should prefer the larger prior bucket: %+v", got)
	}
}

// TestDBConnGrowth_TopNSlicesAfterHonestTotal — facetKeysTotal counts the full
// diffed union while the returned list is cut to topN, so the agent can tell a
// 3-of-12 view from a complete one.
func TestDBConnGrowth_TopNSlicesAfterHonestTotal(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// 12 distinct users, user_a..user_l; user_i holds (12-i) connections.
	rows := growthRows()
	for i := 0; i < 12; i++ {
		user := "user_" + string(rune('a'+i))
		for j := 0; j < (12 - i); j++ {
			rows.AddRow(user, "h", "d", "Query", "executing")
		}
	}
	expectGrowthScan(mock, false, rows)

	ring := newDBConnSnapshotRing(dbConnGrowthRingCap)
	out, err := collectConnectionGrowth(context.Background(), db, ring, time.Now(), false, 0, 3, "growth", "")
	if err != nil {
		t.Fatalf("collectConnectionGrowth: %v", err)
	}
	byUser := out["byUser"].([]dbConnGrowthEntry)
	if len(byUser) != 3 {
		t.Fatalf("byUser should be cut to topN=3: %+v", byUser)
	}
	if byUser[0].Key != "user_a" || byUser[0].Delta != 12 {
		t.Errorf("top grower = %+v, want user_a delta 12", byUser[0])
	}
	if got := out["facetKeysTotal"].(map[string]int)["byUser"]; got != 12 {
		t.Errorf("facetKeysTotal[byUser] = %d, want the full pre-cut union of 12", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestDBConnGrowth_ExplicitSnapshotIdBaseline — pinning an older id compares
// against THAT snapshot, not the most recent one. This is how an operator
// measures against the start of an incident rather than the last poll.
func TestDBConnGrowth_ExplicitSnapshotIdBaseline(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	expectGrowthScan(mock, false, growthRows().AddRow("acore", "h", "d", "Query", "executing"))
	expectGrowthScan(mock, false, growthRows().
		AddRow("acore", "h", "d", "Query", "executing").
		AddRow("acore", "h", "d", "Query", "executing").
		AddRow("acore", "h", "d", "Query", "executing"))
	expectGrowthScan(mock, false, growthRows().
		AddRow("acore", "h", "d", "Query", "executing").
		AddRow("acore", "h", "d", "Query", "executing").
		AddRow("acore", "h", "d", "Query", "executing").
		AddRow("acore", "h", "d", "Query", "executing"))

	ring := newDBConnSnapshotRing(dbConnGrowthRingCap)
	t0 := time.Date(2026, 7, 28, 16, 0, 0, 0, time.UTC)
	for i := 0; i < 2; i++ {
		if _, err := collectConnectionGrowth(context.Background(), db, ring, t0.Add(time.Duration(i)*time.Minute), false, 0, 10, "growth", ""); err != nil {
			t.Fatalf("seed scan %d: %v", i, err)
		}
	}
	// Third scan pinned to snap-1 (total 1) rather than the default snap-2 (3).
	out, err := collectConnectionGrowth(context.Background(), db, ring, t0.Add(2*time.Minute), false, 0, 10, "growth", "snap-1")
	if err != nil {
		t.Fatalf("pinned scan: %v", err)
	}
	if out["comparedTo"].(string) != "snap-1" {
		t.Errorf("comparedTo = %v, want snap-1", out["comparedTo"])
	}
	if out["baselineTotal"].(int) != 1 || out["totalDelta"].(int) != 3 {
		t.Errorf("baselineTotal/totalDelta = %v/%v, want 1/3 (against snap-1, not snap-2)",
			out["baselineTotal"], out["totalDelta"])
	}
	if out["intervalSeconds"].(int) != 120 {
		t.Errorf("intervalSeconds = %v, want 120 (back to snap-1)", out["intervalSeconds"])
	}
	if out["baselineFilterMismatch"].(bool) {
		t.Errorf("same filters must not flag a mismatch")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestDBConnGrowth_UnknownSnapshotIdErrors — an evicted or typo'd id must be a
// hard error naming the ring size, not a silent fallback to the latest
// snapshot (which would report a delta the operator never asked for).
func TestDBConnGrowth_UnknownSnapshotIdErrors(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	expectGrowthScan(mock, false, growthRows().AddRow("acore", "h", "d", "Query", "executing"))

	ring := newDBConnSnapshotRing(dbConnGrowthRingCap)
	_, err = collectConnectionGrowth(context.Background(), db, ring, time.Now(), false, 0, 10, "growth", "snap-404")
	if err == nil {
		t.Fatal("expected an error for an unknown snapshotId")
	}
	if !strings.Contains(err.Error(), "snapshot not found") || !strings.Contains(err.Error(), "snap-404") {
		t.Errorf("error should name the missing id: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestDBConnGrowth_IncomparableFiltersGetOwnBaseline — the auto-baseline only
// matches snapshots taken under the SAME filters. Diffing an includeSleep=true
// scan against an includeSleep=false one would report the entire idle Sleep
// population as growth, which is the single most misleading thing this tool
// could do.
func TestDBConnGrowth_IncomparableFiltersGetOwnBaseline(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	expectGrowthScan(mock, false, growthRows().AddRow("acore", "h", "d", "Query", "executing"))
	expectGrowthScan(mock, true, growthRows().
		AddRow("acore", "h", "d", "Query", "executing").
		AddRow("acore", "h", "d", "Sleep", ""))
	expectGrowthScan(mock, true, growthRows().
		AddRow("acore", "h", "d", "Query", "executing").
		AddRow("acore", "h", "d", "Sleep", "").
		AddRow("acore", "h", "d", "Sleep", ""))

	ring := newDBConnSnapshotRing(dbConnGrowthRingCap)
	now := time.Now()
	if _, err := collectConnectionGrowth(context.Background(), db, ring, now, false, 0, 10, "growth", ""); err != nil {
		t.Fatalf("no-sleep scan: %v", err)
	}
	// includeSleep=true has no comparable prior snapshot -> first, not a diff
	// against the no-sleep one.
	out, err := collectConnectionGrowth(context.Background(), db, ring, now.Add(time.Minute), true, 0, 10, "growth", "")
	if err != nil {
		t.Fatalf("sleep scan: %v", err)
	}
	if !out["firstSnapshot"].(bool) || out["comparedTo"] != nil {
		t.Errorf("includeSleep=true must not diff against a no-sleep baseline: comparedTo=%v", out["comparedTo"])
	}
	// A second includeSleep=true scan DOES find its comparable predecessor.
	out, err = collectConnectionGrowth(context.Background(), db, ring, now.Add(2*time.Minute), true, 0, 10, "growth", "")
	if err != nil {
		t.Fatalf("second sleep scan: %v", err)
	}
	if out["comparedTo"].(string) != "snap-2" {
		t.Errorf("comparedTo = %v, want snap-2 (the matching-filter predecessor)", out["comparedTo"])
	}
	if out["totalDelta"].(int) != 1 {
		t.Errorf("totalDelta = %v, want 1", out["totalDelta"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestDBConnGrowth_ExplicitIdAcrossFiltersFlagsMismatch — pinning an id whose
// filters differ is allowed (occasionally deliberate) but must be flagged and
// noted so the numbers aren't read as apples-to-apples.
func TestDBConnGrowth_ExplicitIdAcrossFiltersFlagsMismatch(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	expectGrowthScan(mock, false, growthRows().AddRow("acore", "h", "d", "Query", "executing"))
	expectGrowthScan(mock, true, growthRows().
		AddRow("acore", "h", "d", "Query", "executing").
		AddRow("acore", "h", "d", "Sleep", ""))

	ring := newDBConnSnapshotRing(dbConnGrowthRingCap)
	now := time.Now()
	if _, err := collectConnectionGrowth(context.Background(), db, ring, now, false, 0, 10, "growth", ""); err != nil {
		t.Fatalf("no-sleep scan: %v", err)
	}
	out, err := collectConnectionGrowth(context.Background(), db, ring, now.Add(time.Minute), true, 0, 10, "growth", "snap-1")
	if err != nil {
		t.Fatalf("pinned cross-filter scan: %v", err)
	}
	if !out["baselineFilterMismatch"].(bool) {
		t.Errorf("cross-filter comparison must set baselineFilterMismatch")
	}
	notes := out["notes"].([]string)
	if len(notes) == 0 || !strings.Contains(notes[0], "not apples-to-apples") {
		t.Errorf("expected a mismatch note, got %v", notes)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestDBConnGrowth_RingEvictsOldest — the history is bounded. Once the ring is
// full the oldest snapshot is dropped and pinning it errors, which is exactly
// why storedSnapshots is echoed on every response.
func TestDBConnGrowth_RingEvictsOldest(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	for i := 0; i < 3; i++ {
		expectGrowthScan(mock, false, growthRows().AddRow("acore", "h", "d", "Query", "executing"))
	}
	// The 4th call pins the evicted snap-1.
	expectGrowthScan(mock, false, growthRows().AddRow("acore", "h", "d", "Query", "executing"))

	ring := newDBConnSnapshotRing(2)
	now := time.Now()
	var out map[string]any
	for i := 0; i < 3; i++ {
		out, err = collectConnectionGrowth(context.Background(), db, ring, now.Add(time.Duration(i)*time.Minute), false, 0, 10, "growth", "")
		if err != nil {
			t.Fatalf("scan %d: %v", i, err)
		}
	}
	if out["ringCap"].(int) != 2 {
		t.Errorf("ringCap = %v, want 2", out["ringCap"])
	}
	stored := out["storedSnapshots"].([]map[string]any)
	if len(stored) != 2 {
		t.Fatalf("ring should hold 2 snapshots, got %d: %v", len(stored), stored)
	}
	// Newest first, and snap-1 has been evicted.
	if stored[0]["id"].(string) != "snap-3" || stored[1]["id"].(string) != "snap-2" {
		t.Errorf("storedSnapshots should be newest-first [snap-3 snap-2], got %v", stored)
	}
	if _, err := collectConnectionGrowth(context.Background(), db, ring, now.Add(3*time.Minute), false, 0, 10, "growth", "snap-1"); err == nil {
		t.Error("pinning an evicted snapshot should error")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestDBConnGrowth_TruncationSurfacesNote — when either side hit the scan cap
// the deltas are computed over incomplete facets, so truncated must propagate
// AND be called out; a silently-partial diff is worse than none.
func TestDBConnGrowth_TruncationSurfacesNote(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	expectGrowthScan(mock, false, growthRows().AddRow("acore", "h", "d", "Query", "executing"))
	flood := growthRows()
	for i := 0; i < dbConnSummaryScanCap+50; i++ {
		flood.AddRow("acore", "h", "d", "Query", "executing")
	}
	expectGrowthScan(mock, false, flood)

	ring := newDBConnSnapshotRing(dbConnGrowthRingCap)
	now := time.Now()
	if _, err := collectConnectionGrowth(context.Background(), db, ring, now, false, 0, 10, "growth", ""); err != nil {
		t.Fatalf("baseline scan: %v", err)
	}
	out, err := collectConnectionGrowth(context.Background(), db, ring, now.Add(time.Minute), false, 0, 10, "growth", "")
	if err != nil {
		t.Fatalf("flood scan: %v", err)
	}
	if !out["truncated"].(bool) {
		t.Errorf("truncated should be true when the scan cap is hit")
	}
	if out["baselineTruncated"].(bool) {
		t.Errorf("baselineTruncated should be false — the baseline scan was small")
	}
	if out["total"].(int) != dbConnSummaryScanCap {
		t.Errorf("total = %v, want the scan cap %d", out["total"], dbConnSummaryScanCap)
	}
	notes := out["notes"].([]string)
	if len(notes) == 0 || !strings.Contains(notes[len(notes)-1], "row cap") {
		t.Errorf("expected a truncation note, got %v", notes)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestDBConnGrowth_RegisteredInGlobalRing — the handler path must record into
// the process-wide ring so consecutive real calls actually diff. Driven twice
// through the registered tool: the second call must find the first.
func TestDBConnGrowth_RegisteredInGlobalRing(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	reg := NewRegistry()
	RegisterDBConnectionSummaryGrowthTool(reg, DBDeps{QueryDB: db})
	tool, _ := reg.Get("db_connection_summary_growth")

	// A distinctive minTime keeps this test's snapshots in their own
	// filter-signature lane, so other tests sharing the global ring can't
	// become this one's baseline. It also binds a third placeholder, which
	// pins the minTime predicate's position between the Sleep filter and the
	// LIMIT.
	for i := 0; i < 2; i++ {
		mock.ExpectQuery(regexp.QuoteMeta("AND TIME >= ?")).
			WithArgs("Sleep", int64(4242), dbConnSummaryScanCap+1).
			WillReturnRows(growthRows().AddRow("acore", "h", "d", "Query", "executing"))
	}

	first := tool.Handler(context.Background(), json.RawMessage(`{"minTime":4242}`), "").(map[string]any)
	if msg, ok := first["error"]; ok {
		t.Fatalf("first call errored: %v", msg)
	}
	second := tool.Handler(context.Background(), json.RawMessage(`{"minTime":4242}`), "").(map[string]any)
	if msg, ok := second["error"]; ok {
		t.Fatalf("second call errored: %v", msg)
	}
	if second["comparedTo"] != first["snapshotId"] {
		t.Errorf("second call should diff against the first: comparedTo=%v, first snapshotId=%v",
			second["comparedTo"], first["snapshotId"])
	}
	if second["totalDelta"].(int) != 0 {
		t.Errorf("identical scans should show zero movement: %v", second["totalDelta"])
	}
	if second["minTime"].(int64) != 4242 {
		t.Errorf("minTime echo = %v, want 4242", second["minTime"])
	}
}
