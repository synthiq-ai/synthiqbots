package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// statsStalenessCols is the fixed column set every fixture returns — mirrors the
// SELECT in collectIndexStatsStaleness. Note NON_UNIQUE sits between INDEX_TYPE
// and ENGINE, and CARDINALITY is already IFNULL'd to -1 by the query, so
// fixtures pass int64(-1) to simulate a NULL estimate.
var statsStalenessCols = []string{
	"TABLE_SCHEMA", "TABLE_NAME", "INDEX_NAME", "SEQ_IN_INDEX", "COLUMN_NAME",
	"CARDINALITY", "INDEX_TYPE", "NON_UNIQUE", "ENGINE", "TABLE_ROWS",
}

func TestDBIndexStatsStaleness_DescriptionMentionsContext(t *testing.T) {
	reg := NewRegistry()
	RegisterDBIndexStatsStalenessTool(reg, DBDeps{})
	tool, ok := reg.Get("db_index_stats_staleness")
	if !ok {
		t.Fatal("db_index_stats_staleness not registered")
	}
	for _, kw := range []string{
		"information_schema.STATISTICS", "CARDINALITY", "TABLE_ROWS", "ANALYZE TABLE",
		"db_low_cardinality_indexes", "db_redundant_indexes", "PRIMARY", "UNIQUE", "BTREE",
		"acore_playerbots", "maxPrimaryKeyDrift", "minRows", "analyzeStatements",
		"missing", "zero_cardinality", "exceeds_rows", "primary_key_undercount",
	} {
		if !strings.Contains(tool.Description, kw) {
			t.Errorf("description missing %q", kw)
		}
	}
}

func TestDBIndexStatsStaleness_ReadOnlyAnnotation(t *testing.T) {
	reg := NewRegistry()
	RegisterDBIndexStatsStalenessTool(reg, DBDeps{})
	tool, _ := reg.Get("db_index_stats_staleness")
	if !strings.Contains(string(tool.Annotations), `"readOnlyHint":true`) {
		t.Errorf("expected readOnlyHint:true, got %s", tool.Annotations)
	}
	if tool.Destructive {
		t.Errorf("destructive should be false")
	}
}

func TestDBIndexStatsStaleness_NoPoolReturnsError(t *testing.T) {
	reg := NewRegistry()
	RegisterDBIndexStatsStalenessTool(reg, DBDeps{})
	tool, _ := reg.Get("db_index_stats_staleness")
	resp := tool.Handler(context.Background(), json.RawMessage(`{}`), "")
	got, _ := resp.(map[string]any)
	if msg, _ := got["error"].(string); !strings.Contains(msg, "ops_ro pool not configured") {
		t.Errorf("expected pool-not-configured error, got %v", got)
	}
}

func TestDBIndexStatsStaleness_InvalidDatabaseRejected(t *testing.T) {
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	reg := NewRegistry()
	RegisterDBIndexStatsStalenessTool(reg, DBDeps{QueryDB: db})
	tool, _ := reg.Get("db_index_stats_staleness")
	resp := tool.Handler(context.Background(), json.RawMessage(`{"database":"acore_playerbots"}`), "")
	got, _ := resp.(map[string]any)
	if msg, _ := got["error"].(string); !strings.Contains(msg, "acore_world/acore_characters/acore_auth") {
		t.Errorf("expected allowlist error, got %v", got)
	}
}

// TestDBIndexStatsStaleness_InvalidMaxPrimaryKeyDriftRejected — an out-of-range
// threshold is rejected (not clamped) with NO query run, so a typo'd value reads
// as "bad filter" rather than "every primary key is stale".
func TestDBIndexStatsStaleness_InvalidMaxPrimaryKeyDriftRejected(t *testing.T) {
	for _, body := range []string{
		`{"maxPrimaryKeyDrift":0}`,
		`{"maxPrimaryKeyDrift":-0.5}`,
		`{"maxPrimaryKeyDrift":1.5}`,
	} {
		db, mock, err := sqlmock.New()
		if err != nil {
			t.Fatal(err)
		}
		reg := NewRegistry()
		RegisterDBIndexStatsStalenessTool(reg, DBDeps{QueryDB: db})
		tool, _ := reg.Get("db_index_stats_staleness")
		resp := tool.Handler(context.Background(), json.RawMessage(body), "")
		got, _ := resp.(map[string]any)
		if msg, _ := got["error"].(string); !strings.Contains(msg, "maxPrimaryKeyDrift must be in") {
			t.Errorf("body %s: expected range-reject error, got %v", body, got)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("body %s: a query was issued despite the reject: %v", body, err)
		}
		db.Close()
	}
}

// TestClassifyStaleStats_Table exercises every branch of the classifier,
// including the two boundaries where the comparison's strictness decides the
// answer: the PRIMARY-drift threshold is `>` (so exactly maxPKDrift is CLEAN),
// and a row estimate of zero routes to exceeds_rows rather than dividing by it.
func TestClassifyStaleStats_Table(t *testing.T) {
	const maxDrift = 0.20
	cases := []struct {
		name      string
		isPrimary bool
		card      int64
		rows      int64
		wantClass string
		wantRatio float64
		wantFlag  bool
	}{
		{"null cardinality on populated table", false, -1, 5000, statsClassMissing, 0, true},
		{"null cardinality on empty table is not actionable", false, -1, 0, "", 0, false},
		{"zero cardinality on populated table", false, 0, 5000, statsClassZeroCardinality, 0, true},
		{"zero cardinality on empty table is honest", false, 0, 0, "", 0, false},
		{"cardinality above rows is impossible", false, 1500, 1000, statsClassExceedsRows, 0.5, true},
		{"cardinality above a zero row estimate", false, 5, 0, statsClassExceedsRows, 5, true},
		{"healthy secondary index", false, 900, 1000, "", 0, false},
		{"primary key in agreement", true, 1000, 1000, "", 0, false},
		// Boundary: shortfall exactly at the threshold is NOT flagged (`>`).
		{"primary key exactly at the drift threshold", true, 800, 1000, "", 0, false},
		{"primary key one row past the threshold", true, 799, 1000, statsClassPrimaryKeyUndercount, 0.201, true},
		{"primary key far below rows", true, 400, 1000, statsClassPrimaryKeyUndercount, 0.6, true},
		// A PRIMARY key with a NULL/zero estimate takes the absent-estimate class,
		// not the drift class — those need no threshold to be wrong.
		{"primary key with null estimate", true, -1, 1000, statsClassMissing, 0, true},
		{"primary key with zero estimate", true, 0, 1000, statsClassZeroCardinality, 0, true},
		// A non-primary index below its row count is normal, not drift.
		{"secondary index far below rows is not drift", false, 1, 1000000, "", 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			class, ratio, flag := classifyStaleStats(tc.isPrimary, tc.card, tc.rows, maxDrift)
			if flag != tc.wantFlag {
				t.Fatalf("flagged: want %v got %v (class %q)", tc.wantFlag, flag, class)
			}
			if class != tc.wantClass {
				t.Errorf("class: want %q got %q", tc.wantClass, class)
			}
			if math.Abs(ratio-tc.wantRatio) > 1e-9 {
				t.Errorf("ratio: want %v got %v", tc.wantRatio, ratio)
			}
		})
	}
}

// TestStaleStatsSeverity_Boundaries pins both severity cut-offs, each of which
// is inclusive (`>=`). Getting either off by one would silently re-rank the
// whole response.
func TestStaleStatsSeverity_Boundaries(t *testing.T) {
	cases := []struct {
		name  string
		class string
		ratio float64
		rows  int64
		want  string
	}{
		{"missing at the row floor", statsClassMissing, 0, dbStatsStalenessHighRowsFloor, "high"},
		{"missing one row below the floor", statsClassMissing, 0, dbStatsStalenessHighRowsFloor - 1, "medium"},
		{"zero cardinality at the row floor", statsClassZeroCardinality, 0, dbStatsStalenessHighRowsFloor, "high"},
		{"zero cardinality below the floor", statsClassZeroCardinality, 0, 10, "medium"},
		{"exceeds rows at the ratio cutoff", statsClassExceedsRows, dbStatsStalenessHighRatio, 10, "high"},
		{"exceeds rows below the ratio cutoff", statsClassExceedsRows, dbStatsStalenessHighRatio - 0.01, 1000000, "medium"},
		{"primary drift at the ratio cutoff", statsClassPrimaryKeyUndercount, dbStatsStalenessHighRatio, 1, "high"},
		{"primary drift below the ratio cutoff", statsClassPrimaryKeyUndercount, 0.21, 1000000, "medium"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := staleStatsSeverity(tc.class, tc.ratio, tc.rows); got != tc.want {
				t.Errorf("want %q got %q", tc.want, got)
			}
		})
	}
}

// TestDBIndexStatsStaleness_GoldenAggregation drives the default path (all three
// schemas, maxPrimaryKeyDrift 0.20, minRows 0) and verifies the full fold: all
// four classes, the deliberate inclusion of PRIMARY/UNIQUE indexes, the
// composite-index leading-vs-full cardinality walk, the not-flagged-but-scanned
// paths, worst-first ordering, perDatabase counters, totals.byClass and the
// deduped ANALYZE remedy list.
func TestDBIndexStatsStaleness_GoldenAggregation(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	rs := sqlmock.NewRows(statsStalenessCols).
		// acore_auth — both indexes healthy, scanned and not flagged.
		AddRow("acore_auth", "account", "PRIMARY", 1, "id", int64(5000), "BTREE", int64(0), "InnoDB", int64(5000)).
		AddRow("acore_auth", "account", "idx_username", 1, "username", int64(4990), "BTREE", int64(0), "InnoDB", int64(5000)).
		// acore_characters.characters — PK undercount (0.6) + a NULL estimate.
		AddRow("acore_characters", "characters", "PRIMARY", 1, "guid", int64(40000), "BTREE", int64(0), "InnoDB", int64(100000)).
		AddRow("acore_characters", "characters", "idx_account", 1, "account", int64(-1), "BTREE", int64(1), "InnoDB", int64(100000)).
		// Composite secondary, healthy: leading 40, full 300 — exercises the walk.
		AddRow("acore_characters", "characters", "idx_zone_map", 1, "zone", int64(40), "BTREE", int64(1), "InnoDB", int64(100000)).
		AddRow("acore_characters", "characters", "idx_zone_map", 2, "map", int64(300), "BTREE", int64(1), "InnoDB", int64(100000)).
		// acore_world.creature — PK claims more distinct values than rows (0.5) + a zero estimate.
		AddRow("acore_world", "creature", "PRIMARY", 1, "guid", int64(150000), "BTREE", int64(0), "InnoDB", int64(100000)).
		AddRow("acore_world", "creature", "idx_map", 1, "map", int64(0), "BTREE", int64(1), "InnoDB", int64(100000)).
		// Zero estimate under the high row floor → medium.
		AddRow("acore_world", "quest_template", "PRIMARY", 1, "ID", int64(0), "BTREE", int64(0), "InnoDB", int64(9000)).
		// Empty table, no estimate → scanned, nothing for ANALYZE to find.
		AddRow("acore_world", "small_lookup", "idx_x", 1, "x", int64(-1), "BTREE", int64(1), "InnoDB", int64(0)).
		// Distinct values claimed in a table the row estimate calls empty.
		AddRow("acore_world", "gameobject", "idx_y", 1, "y", int64(5), "BTREE", int64(1), "InnoDB", int64(0))

	mock.ExpectQuery(regexp.QuoteMeta("FROM information_schema.STATISTICS")).
		WithArgs("acore_world", "acore_characters", "acore_auth").
		WillReturnRows(rs)

	reg := NewRegistry()
	RegisterDBIndexStatsStalenessTool(reg, DBDeps{QueryDB: db})
	tool, _ := reg.Get("db_index_stats_staleness")
	resp := tool.Handler(context.Background(), json.RawMessage(`{}`), "")
	got, ok := resp.(map[string]any)
	if !ok {
		t.Fatalf("unexpected response shape: %T", resp)
	}
	if msg, hasErr := got["error"]; hasErr {
		t.Fatalf("unexpected error: %v", msg)
	}

	f := got["findings"].([]staleStatsFinding)
	if len(f) != 6 {
		t.Fatalf("expected 6 findings, got %d: %+v", len(f), f)
	}

	// Worst-first: all five HIGH before the one MEDIUM; within HIGH the three
	// arithmetic-impossibility classes rank ahead of the threshold-based drift,
	// and inside a class the bigger disagreement wins.
	want := []struct {
		db, table, index, class, severity string
		ratio                             float64
	}{
		{"acore_characters", "characters", "idx_account", statsClassMissing, "high", 0},
		{"acore_world", "creature", "idx_map", statsClassZeroCardinality, "high", 0},
		{"acore_world", "gameobject", "idx_y", statsClassExceedsRows, "high", 5},
		{"acore_world", "creature", "PRIMARY", statsClassExceedsRows, "high", 0.5},
		{"acore_characters", "characters", "PRIMARY", statsClassPrimaryKeyUndercount, "high", 0.6},
		{"acore_world", "quest_template", "PRIMARY", statsClassZeroCardinality, "medium", 0},
	}
	for i, w := range want {
		if f[i].Database != w.db || f[i].Table != w.table || f[i].Index != w.index {
			t.Errorf("findings[%d] identity want %s.%s/%s, got %s.%s/%s", i, w.db, w.table, w.index, f[i].Database, f[i].Table, f[i].Index)
		}
		if f[i].Class != w.class || f[i].Severity != w.severity {
			t.Errorf("findings[%d] want %s/%s, got %s/%s", i, w.class, w.severity, f[i].Class, f[i].Severity)
		}
		if math.Abs(f[i].Ratio-w.ratio) > 1e-9 {
			t.Errorf("findings[%d] ratio want %v, got %v", i, w.ratio, f[i].Ratio)
		}
	}

	// PRIMARY keys are INCLUDED here — the deliberate divergence from
	// db_low_cardinality_indexes, which filters them out in SQL.
	var primaries int
	for _, x := range f {
		if x.IsPrimary {
			primaries++
			if x.NonUnique {
				t.Errorf("PRIMARY %s.%s must not be marked nonUnique", x.Database, x.Table)
			}
		}
	}
	if primaries != 3 {
		t.Errorf("expected 3 PRIMARY-key findings, got %d", primaries)
	}

	// The NULL estimate survives to the response as the -1 sentinel rather than
	// being flattened to 0, which would be indistinguishable from a real zero.
	if f[0].IndexCardinality != -1 || f[0].LeadingColumnCardinality != -1 {
		t.Errorf("missing-class finding should carry the -1 NULL sentinel: %+v", f[0])
	}
	if !strings.Contains(f[0].Reason, "NULL") {
		t.Errorf("missing-class reason should name the NULL estimate: %q", f[0].Reason)
	}

	totals := got["totals"].(map[string]any)
	// Scanned groups: auth{PRIMARY,idx_username}=2, characters{PRIMARY,idx_account,idx_zone_map}=3,
	// world{creature.PRIMARY,creature.idx_map,quest_template.PRIMARY,small_lookup.idx_x,gameobject.idx_y}=5 → 10.
	if totals["indexesScanned"].(int) != 10 {
		t.Errorf("totals.indexesScanned want 10, got %v", totals["indexesScanned"])
	}
	if totals["flaggedCount"].(int) != 6 {
		t.Errorf("totals.flaggedCount want 6, got %v", totals["flaggedCount"])
	}
	// characters, creature, gameobject, quest_template — small_lookup is scanned
	// but has nothing to analyze.
	if totals["tablesAffected"].(int) != 4 {
		t.Errorf("totals.tablesAffected want 4, got %v", totals["tablesAffected"])
	}
	byClass := totals["byClass"].(map[string]int)
	for class, wantN := range map[string]int{
		statsClassMissing:              1,
		statsClassZeroCardinality:      2,
		statsClassExceedsRows:          2,
		statsClassPrimaryKeyUndercount: 1,
	} {
		if byClass[class] != wantN {
			t.Errorf("byClass[%s] want %d, got %d", class, wantN, byClass[class])
		}
	}

	perDB := got["perDatabase"].([]staleStatsPerDatabase)
	if len(perDB) != 3 {
		t.Fatalf("expected 3 perDatabase rows, got %d", len(perDB))
	}
	if perDB[0].Database != "acore_world" || perDB[0].FlaggedCount != 4 || perDB[0].IndexesScanned != 5 {
		t.Errorf("perDB[0] want acore_world 4/5: %+v", perDB[0])
	}
	if perDB[0].ZeroCardinalityCount != 2 || perDB[0].ExceedsRowsCount != 2 {
		t.Errorf("perDB[0] class counters want zero=2 exceeds=2: %+v", perDB[0])
	}
	if perDB[1].Database != "acore_characters" || perDB[1].FlaggedCount != 2 || perDB[1].IndexesScanned != 3 {
		t.Errorf("perDB[1] want acore_characters 2/3: %+v", perDB[1])
	}
	if perDB[1].MissingCount != 1 || perDB[1].PrimaryKeyUndercountCount != 1 {
		t.Errorf("perDB[1] class counters want missing=1 pkUndercount=1: %+v", perDB[1])
	}
	// Scanned-but-clean schema still surfaces instead of vanishing.
	if perDB[2].Database != "acore_auth" || perDB[2].FlaggedCount != 0 || perDB[2].IndexesScanned != 2 {
		t.Errorf("perDB[2] want acore_auth 0/2: %+v", perDB[2])
	}

	// One statement per affected TABLE, deduped, in worst-first order.
	analyze := got["analyzeStatements"].([]string)
	wantAnalyze := []string{
		"ANALYZE TABLE `acore_characters`.`characters`;",
		"ANALYZE TABLE `acore_world`.`creature`;",
		"ANALYZE TABLE `acore_world`.`gameobject`;",
		"ANALYZE TABLE `acore_world`.`quest_template`;",
	}
	if len(analyze) != len(wantAnalyze) {
		t.Fatalf("expected %d analyze statements, got %d: %v", len(wantAnalyze), len(analyze), analyze)
	}
	for i, w := range wantAnalyze {
		if analyze[i] != w {
			t.Errorf("analyzeStatements[%d] want %q, got %q", i, w, analyze[i])
		}
	}
	if got["analyzeStatementsTruncated"].(bool) {
		t.Errorf("analyzeStatementsTruncated should be false for 4 tables")
	}
	if got["truncated"].(bool) {
		t.Errorf("truncated should be false for 6 findings")
	}
	if got["minRows"].(int64) != 0 {
		t.Errorf("minRows should default to 0 (evaluate all), got %v", got["minRows"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestDBIndexStatsStaleness_DatabaseFilterNarrowsScan verifies passing
// `database` narrows the IN clause to a single bind.
func TestDBIndexStatsStaleness_DatabaseFilterNarrowsScan(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectQuery(regexp.QuoteMeta("FROM information_schema.STATISTICS")).
		WithArgs("acore_world"). // single bind — IN list narrowed to one schema
		WillReturnRows(sqlmock.NewRows(statsStalenessCols).
			AddRow("acore_world", "creature", "idx_map", 1, "map", int64(0), "BTREE", int64(1), "InnoDB", int64(50000)))

	reg := NewRegistry()
	RegisterDBIndexStatsStalenessTool(reg, DBDeps{QueryDB: db})
	tool, _ := reg.Get("db_index_stats_staleness")
	resp := tool.Handler(context.Background(), json.RawMessage(`{"database":"acore_world"}`), "")
	got, _ := resp.(map[string]any)
	if msg, hasErr := got["error"]; hasErr {
		t.Fatalf("unexpected error: %v", msg)
	}
	schemas := got["scannedSchemas"].([]string)
	if len(schemas) != 1 || schemas[0] != "acore_world" {
		t.Errorf("scannedSchemas want [acore_world], got %v", schemas)
	}
	perDB := got["perDatabase"].([]staleStatsPerDatabase)
	if len(perDB) != 1 {
		t.Errorf("expected 1 perDatabase row when filtered, got %d", len(perDB))
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestDBIndexStatsStaleness_MinRowsGate — minRows defaults to 0 so a small table
// is still audited (a stale table often UNDER-reports its row count, so gating on
// it by default would hide the target); an explicit minRows removes the table
// from the scan entirely, not just from the flagged set.
func TestDBIndexStatsStaleness_MinRowsGate(t *testing.T) {
	fixture := func() *sqlmock.Rows {
		return sqlmock.NewRows(statsStalenessCols).
			AddRow("acore_world", "tiny_config", "idx_x", 1, "x", int64(0), "BTREE", int64(1), "InnoDB", int64(50)).
			AddRow("acore_world", "creature", "idx_map", 1, "map", int64(0), "BTREE", int64(1), "InnoDB", int64(50000))
	}

	t.Run("default evaluates the small table", func(t *testing.T) {
		db, mock, err := sqlmock.New()
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		mock.ExpectQuery(regexp.QuoteMeta("FROM information_schema.STATISTICS")).
			WithArgs("acore_world").WillReturnRows(fixture())

		reg := NewRegistry()
		RegisterDBIndexStatsStalenessTool(reg, DBDeps{QueryDB: db})
		tool, _ := reg.Get("db_index_stats_staleness")
		got, _ := tool.Handler(context.Background(), json.RawMessage(`{"database":"acore_world"}`), "").(map[string]any)
		totals := got["totals"].(map[string]any)
		if totals["indexesScanned"].(int) != 2 || totals["flaggedCount"].(int) != 2 {
			t.Errorf("default should scan and flag both, got scanned=%v flagged=%v", totals["indexesScanned"], totals["flaggedCount"])
		}
	})

	t.Run("explicit minRows removes the small table from the scan", func(t *testing.T) {
		db, mock, err := sqlmock.New()
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		mock.ExpectQuery(regexp.QuoteMeta("FROM information_schema.STATISTICS")).
			WithArgs("acore_world").WillReturnRows(fixture())

		reg := NewRegistry()
		RegisterDBIndexStatsStalenessTool(reg, DBDeps{QueryDB: db})
		tool, _ := reg.Get("db_index_stats_staleness")
		got, _ := tool.Handler(context.Background(), json.RawMessage(`{"database":"acore_world","minRows":1000}`), "").(map[string]any)
		totals := got["totals"].(map[string]any)
		if totals["indexesScanned"].(int) != 1 || totals["flaggedCount"].(int) != 1 {
			t.Errorf("minRows=1000 should scan and flag only creature, got scanned=%v flagged=%v", totals["indexesScanned"], totals["flaggedCount"])
		}
		if got["minRows"].(int64) != 1000 {
			t.Errorf("minRows should be echoed back as 1000, got %v", got["minRows"])
		}
	})

	t.Run("negative minRows is normalised to zero", func(t *testing.T) {
		db, mock, err := sqlmock.New()
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		mock.ExpectQuery(regexp.QuoteMeta("FROM information_schema.STATISTICS")).
			WithArgs("acore_world").WillReturnRows(fixture())

		reg := NewRegistry()
		RegisterDBIndexStatsStalenessTool(reg, DBDeps{QueryDB: db})
		tool, _ := reg.Get("db_index_stats_staleness")
		got, _ := tool.Handler(context.Background(), json.RawMessage(`{"database":"acore_world","minRows":-5}`), "").(map[string]any)
		if got["minRows"].(int64) != 0 {
			t.Errorf("negative minRows should normalise to 0, got %v", got["minRows"])
		}
		totals := got["totals"].(map[string]any)
		if totals["indexesScanned"].(int) != 2 {
			t.Errorf("negative minRows should scan both, got %v", totals["indexesScanned"])
		}
	})
}

// TestDBIndexStatsStaleness_EmptyResultKeepsShape — a schema with nothing to say
// still returns typed empty slices (not nil) plus its pre-seeded perDatabase row,
// so an operator can't read "clean" as "tool broke".
func TestDBIndexStatsStaleness_EmptyResultKeepsShape(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectQuery(regexp.QuoteMeta("FROM information_schema.STATISTICS")).
		WithArgs("acore_auth").
		WillReturnRows(sqlmock.NewRows(statsStalenessCols))

	reg := NewRegistry()
	RegisterDBIndexStatsStalenessTool(reg, DBDeps{QueryDB: db})
	tool, _ := reg.Get("db_index_stats_staleness")
	got, _ := tool.Handler(context.Background(), json.RawMessage(`{"database":"acore_auth"}`), "").(map[string]any)
	if f := got["findings"].([]staleStatsFinding); f == nil || len(f) != 0 {
		t.Errorf("findings should be an empty non-nil slice, got %#v", f)
	}
	if a := got["analyzeStatements"].([]string); a == nil || len(a) != 0 {
		t.Errorf("analyzeStatements should be an empty non-nil slice, got %#v", a)
	}
	perDB := got["perDatabase"].([]staleStatsPerDatabase)
	if len(perDB) != 1 || perDB[0].Database != "acore_auth" || perDB[0].IndexesScanned != 0 {
		t.Errorf("empty schema should still surface a zeroed perDatabase row: %+v", perDB)
	}
	totals := got["totals"].(map[string]any)
	if totals["tablesAffected"].(int) != 0 {
		t.Errorf("tablesAffected want 0, got %v", totals["tablesAffected"])
	}
}

// TestDBIndexStatsStaleness_AnalyzeStatementsCap — the remedy list is built from
// the FULL pre-cap finding set and capped independently, so the worst tables
// still surface when the list is trimmed. tablesAffected stays honest.
func TestDBIndexStatsStaleness_AnalyzeStatementsCap(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	const tables = dbStatsStalenessMaxAnalyzeStatements + 1
	rs := sqlmock.NewRows(statsStalenessCols)
	for i := 0; i < tables; i++ {
		// Identical class/severity/ratio, so ordering falls through to table name.
		rs = rs.AddRow("acore_world", fmt.Sprintf("t%02d", i), "idx_x", 1, "x", int64(-1), "BTREE", int64(1), "InnoDB", int64(100000))
	}
	mock.ExpectQuery(regexp.QuoteMeta("FROM information_schema.STATISTICS")).
		WithArgs("acore_world").WillReturnRows(rs)

	reg := NewRegistry()
	RegisterDBIndexStatsStalenessTool(reg, DBDeps{QueryDB: db})
	tool, _ := reg.Get("db_index_stats_staleness")
	got, _ := tool.Handler(context.Background(), json.RawMessage(`{"database":"acore_world"}`), "").(map[string]any)

	analyze := got["analyzeStatements"].([]string)
	if len(analyze) != dbStatsStalenessMaxAnalyzeStatements {
		t.Fatalf("analyzeStatements want %d, got %d", dbStatsStalenessMaxAnalyzeStatements, len(analyze))
	}
	if !got["analyzeStatementsTruncated"].(bool) {
		t.Errorf("analyzeStatementsTruncated should be true at %d affected tables", tables)
	}
	if analyze[0] != "ANALYZE TABLE `acore_world`.`t00`;" {
		t.Errorf("worst table should lead the remedy list, got %q", analyze[0])
	}
	totals := got["totals"].(map[string]any)
	if totals["tablesAffected"].(int) != tables {
		t.Errorf("tablesAffected must stay honest at %d, got %v", tables, totals["tablesAffected"])
	}
	// Findings themselves are well under their own 500 cap.
	if got["truncated"].(bool) {
		t.Errorf("findings truncated should be false at %d findings", tables)
	}
}

// TestDBIndexStatsStaleness_QueryError surfaces a DB failure as an error rather
// than an empty (and reassuring) result set.
func TestDBIndexStatsStaleness_QueryError(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectQuery(regexp.QuoteMeta("FROM information_schema.STATISTICS")).
		WithArgs("acore_world", "acore_characters", "acore_auth").
		WillReturnError(fmt.Errorf("SELECT command denied to user 'ops_ro'"))

	reg := NewRegistry()
	RegisterDBIndexStatsStalenessTool(reg, DBDeps{QueryDB: db})
	tool, _ := reg.Get("db_index_stats_staleness")
	got, _ := tool.Handler(context.Background(), json.RawMessage(`{}`), "").(map[string]any)
	if msg, _ := got["error"].(string); !strings.Contains(msg, "query information_schema.STATISTICS") {
		t.Errorf("expected a wrapped query error, got %v", got)
	}
}
