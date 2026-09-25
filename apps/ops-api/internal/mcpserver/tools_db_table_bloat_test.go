package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// TestDBTableBloat_DescriptionMentionsContext keeps the description anchored
// to the keywords the agent uses to pick this tool when an operator says
// "which tables are bloated?" / "what should I OPTIMIZE TABLE?". Same shape
// as the db_processlist / db_innodb_status description guards.
func TestDBTableBloat_DescriptionMentionsContext(t *testing.T) {
	reg := NewRegistry()
	RegisterDBTableBloatTool(reg, DBDeps{})
	tool, ok := reg.Get("db_table_bloat")
	if !ok {
		t.Fatal("db_table_bloat not registered")
	}
	for _, kw := range []string{
		"information_schema.TABLES",
		"DATA_FREE",
		"bloatFraction",
		"OPTIMIZE TABLE",
		"minBytes",
		"minFraction",
		"topN",
		"acore_playerbots",
		"ops_grant_playerbots_ro",
		"dataFreeBasis",
		"dataFreeScope",
		"innodb_file_per_table",
		"rowsDataFreeTablespaceWide",
		"rowsDataFreeScopeUnknown",
	} {
		if !strings.Contains(tool.Description, kw) {
			t.Errorf("description missing %q", kw)
		}
	}
}

// TestDBTableBloat_ReadOnlyAnnotation guards the AnnRead() annotation — a
// fragmentation reporter that ever flipped to read/write would let an agent
// invoke it under the OPS_ADMIN_ALLOW_ACTIONS gate, which is wrong.
func TestDBTableBloat_ReadOnlyAnnotation(t *testing.T) {
	reg := NewRegistry()
	RegisterDBTableBloatTool(reg, DBDeps{})
	tool, _ := reg.Get("db_table_bloat")
	var ann map[string]any
	if err := json.Unmarshal(tool.Annotations, &ann); err != nil {
		t.Fatalf("decode annotations: %v", err)
	}
	if ann["readOnlyHint"] != true {
		t.Errorf("readOnlyHint expected true, got %v", ann["readOnlyHint"])
	}
}

func TestDBTableBloat_NoPoolReturnsError(t *testing.T) {
	reg := NewRegistry()
	RegisterDBTableBloatTool(reg, DBDeps{})
	tool, _ := reg.Get("db_table_bloat")
	resp := tool.Handler(context.Background(), json.RawMessage(`{}`), "")
	got, _ := resp.(map[string]any)
	if msg, _ := got["error"].(string); !strings.Contains(msg, "ops_ro pool not configured") {
		t.Errorf("expected pool-not-configured error, got %v", got)
	}
}

// TestDBTableBloat_InvalidDatabaseRejected — the `database` arg, when set,
// must be in the allowlist. A typo (or worse, an injection attempt like
// "mysql") must be rejected before binding.
func TestDBTableBloat_InvalidDatabaseRejected(t *testing.T) {
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	reg := NewRegistry()
	RegisterDBTableBloatTool(reg, DBDeps{QueryDB: db})
	tool, _ := reg.Get("db_table_bloat")
	resp := tool.Handler(context.Background(), json.RawMessage(`{"database":"mysql"}`), "")
	got, _ := resp.(map[string]any)
	if msg, _ := got["error"].(string); !strings.Contains(msg, "acore_world/acore_characters/acore_auth") {
		t.Errorf("expected database-allowlist error, got %v", got)
	}
}

// TestDBTableBloat_DefaultAllSchemasAggregation verifies the default path
// (no database filter) binds all three schemas and sorts by bloatFraction
// desc. Also exercises the filtering: a 5 KiB row is below minBytes; a 100
// MiB row at 5% is below minFraction. Both are dropped, leaving the two
// candidates sorted highest-bloat first.
func TestDBTableBloat_DefaultAllSchemasAggregation(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectQuery(regexp.QuoteMeta("FROM information_schema.TABLES")).
		WithArgs("acore_auth", "acore_characters", "acore_world").
		WillReturnRows(sqlmock.NewRows([]string{"TABLE_SCHEMA", "TABLE_NAME", "ENGINE", "DATA_LENGTH", "DATA_FREE"}).
			// big + very bloated: 100 MiB data, 100 MiB free → 50% bloat
			AddRow("acore_characters", "mail", "InnoDB", uint64(100*1024*1024), uint64(100*1024*1024)).
			// big + moderately bloated: 50 MiB data, 10 MiB free → ~16.7% bloat
			AddRow("acore_world", "creature_template", "InnoDB", uint64(50*1024*1024), uint64(10*1024*1024)).
			// big but low bloat: 100 MiB data, 5 MiB free → ~4.8% — below minFraction
			AddRow("acore_auth", "account", "InnoDB", uint64(100*1024*1024), uint64(5*1024*1024)).
			// tiny but high fraction: below minBytes — dropped
			AddRow("acore_auth", "uptime", "InnoDB", uint64(2*1024), uint64(8*1024)).
			// zero-byte table: skipped by the totalBytes==0 guard
			AddRow("acore_world", "empty_stub", "InnoDB", uint64(0), uint64(0)))

	out, err := collectTableBloat(
		context.Background(),
		db,
		[]string{"acore_auth", "acore_characters", "acore_world"},
		dbTableBloatDefaultMinBytes,
		dbTableBloatDefaultMinFraction,
		dbTableBloatDefaultTopN,
		nil,
	)
	if err != nil {
		t.Fatalf("collectTableBloat: %v", err)
	}
	rows := out["rows"].([]tableBloatRow)
	if len(rows) != 2 {
		t.Fatalf("rowCount: %d (rows=%+v)", len(rows), rows)
	}
	if rows[0].Database != "acore_characters" || rows[0].Table != "mail" {
		t.Errorf("expected mail at top, got %+v", rows[0])
	}
	if rows[1].Database != "acore_world" || rows[1].Table != "creature_template" {
		t.Errorf("expected creature_template second, got %+v", rows[1])
	}
	if rows[0].BloatFraction <= rows[1].BloatFraction {
		t.Errorf("sort wrong: row[0].frac %v should be > row[1].frac %v", rows[0].BloatFraction, rows[1].BloatFraction)
	}
	if rows[0].TotalHuman == "" || rows[0].DataFreeHuman == "" || rows[0].DataLengthHuman == "" {
		t.Errorf("human-bytes fields not populated: %+v", rows[0])
	}
	if !strings.Contains(rows[0].DataFreeHuman, "MiB") {
		t.Errorf("DataFreeHuman missing MiB suffix: %q", rows[0].DataFreeHuman)
	}
	if scanned := out["scannedTables"].(int); scanned != 5 {
		t.Errorf("scannedTables: %d", scanned)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// expectInnodbFilePerTable seeds the handler-only `SHOW GLOBAL VARIABLES LIKE
// 'innodb_file_per_table'` lookup. Kept as its OWN helper, deliberately not
// folded into the shared information_schema expectation, because the
// collector-level goldens never issue this statement — adding it there would
// leave the expectation unmet in every one of them. sqlmock matches in order,
// so call this BEFORE the information_schema.TABLES expectation, matching the
// handler's resolve-then-collect order.
func expectInnodbFilePerTable(mock sqlmock.Sqlmock, value string) {
	mock.ExpectQuery(regexp.QuoteMeta("SHOW GLOBAL VARIABLES LIKE 'innodb_file_per_table'")).
		WillReturnRows(sqlmock.NewRows([]string{"Variable_name", "Value"}).
			AddRow("innodb_file_per_table", value))
}

// TestDBTableBloat_DatabaseFilterNarrowsBindings — when a single database is
// passed, the IN clause must bind only that one schema. WithArgs() asserts
// the exact binding tuple so an accidental fall-through to all three would
// fail the test.
func TestDBTableBloat_DatabaseFilterNarrowsBindings(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	reg := NewRegistry()
	RegisterDBTableBloatTool(reg, DBDeps{QueryDB: db})
	tool, _ := reg.Get("db_table_bloat")

	expectInnodbFilePerTable(mock, "ON")
	mock.ExpectQuery(regexp.QuoteMeta("FROM information_schema.TABLES")).
		WithArgs("acore_characters").
		WillReturnRows(sqlmock.NewRows([]string{"TABLE_SCHEMA", "TABLE_NAME", "ENGINE", "DATA_LENGTH", "DATA_FREE"}).
			AddRow("acore_characters", "characters", "InnoDB", uint64(200*1024*1024), uint64(50*1024*1024)))

	resp := tool.Handler(context.Background(), json.RawMessage(`{"database":"acore_characters"}`), "")
	got, _ := resp.(map[string]any)
	if msg, ok := got["error"]; ok {
		t.Fatalf("unexpected error: %v", msg)
	}
	schemas := got["schemas"].([]string)
	if len(schemas) != 1 || schemas[0] != "acore_characters" {
		t.Errorf("schemas not narrowed: %v", schemas)
	}
	rows := got["rows"].([]tableBloatRow)
	if len(rows) != 1 || rows[0].Table != "characters" {
		t.Errorf("expected one row characters, got %+v", rows)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestDBTableBloat_MinFractionFilterExcludesLowBloat — explicit verification
// that a row at exactly minFraction passes (>=) and a row below is dropped.
// Boundary correctness matters because operators tune minFraction to surface
// borderline candidates.
func TestDBTableBloat_MinFractionFilterExcludesLowBloat(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectQuery(regexp.QuoteMeta("FROM information_schema.TABLES")).
		WithArgs("acore_world").
		WillReturnRows(sqlmock.NewRows([]string{"TABLE_SCHEMA", "TABLE_NAME", "ENGINE", "DATA_LENGTH", "DATA_FREE"}).
			// 75% bloat — passes 0.25 threshold
			AddRow("acore_world", "hot", "InnoDB", uint64(10*1024*1024), uint64(30*1024*1024)).
			// 20% bloat — below 0.25 threshold, dropped
			AddRow("acore_world", "cool", "InnoDB", uint64(80*1024*1024), uint64(20*1024*1024)))

	out, err := collectTableBloat(
		context.Background(),
		db,
		[]string{"acore_world"},
		1*1024*1024,
		0.25,
		10,
		nil,
	)
	if err != nil {
		t.Fatalf("collectTableBloat: %v", err)
	}
	rows := out["rows"].([]tableBloatRow)
	if len(rows) != 1 || rows[0].Table != "hot" {
		t.Errorf("expected only 'hot' to survive, got %+v", rows)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestDBTableBloat_MinBytesFilterExcludesTiny — a 100 KiB table at 99% bloat
// is uninteresting (saves <100 KiB on optimize). minBytes is the noise floor.
func TestDBTableBloat_MinBytesFilterExcludesTiny(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectQuery(regexp.QuoteMeta("FROM information_schema.TABLES")).
		WithArgs("acore_auth").
		WillReturnRows(sqlmock.NewRows([]string{"TABLE_SCHEMA", "TABLE_NAME", "ENGINE", "DATA_LENGTH", "DATA_FREE"}).
			// 100 KiB total at 90% bloat — below 1 MiB threshold, dropped
			AddRow("acore_auth", "stub", "InnoDB", uint64(10*1024), uint64(90*1024)).
			// 5 MiB total at 60% bloat — passes
			AddRow("acore_auth", "audit", "InnoDB", uint64(2*1024*1024), uint64(3*1024*1024)))

	out, err := collectTableBloat(
		context.Background(),
		db,
		[]string{"acore_auth"},
		1*1024*1024,
		0.10,
		10,
		nil,
	)
	if err != nil {
		t.Fatalf("collectTableBloat: %v", err)
	}
	rows := out["rows"].([]tableBloatRow)
	if len(rows) != 1 || rows[0].Table != "audit" {
		t.Errorf("expected only 'audit' to survive, got %+v", rows)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestDBTableBloat_TopNClamping verifies topN clamps to the hard max (50)
// and slices the survivors. Mirrors the db_processlist limit-clamping guard.
func TestDBTableBloat_TopNClamping(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	reg := NewRegistry()
	RegisterDBTableBloatTool(reg, DBDeps{QueryDB: db})
	tool, _ := reg.Get("db_table_bloat")

	expectInnodbFilePerTable(mock, "ON")
	rs := sqlmock.NewRows([]string{"TABLE_SCHEMA", "TABLE_NAME", "ENGINE", "DATA_LENGTH", "DATA_FREE"})
	// 100 bloated tables, descending fraction so the slice keeps the worst 50
	for i := 0; i < 100; i++ {
		bytesFree := uint64(100-i) * 1024 * 1024
		rs.AddRow("acore_world", "t"+itoaPad(i), "InnoDB", uint64(10*1024*1024), bytesFree)
	}
	mock.ExpectQuery(regexp.QuoteMeta("FROM information_schema.TABLES")).
		WithArgs("acore_auth", "acore_characters", "acore_world").
		WillReturnRows(rs)

	resp := tool.Handler(context.Background(), json.RawMessage(`{"topN":99999}`), "")
	got, _ := resp.(map[string]any)
	if msg, ok := got["error"]; ok {
		t.Fatalf("unexpected error: %v", msg)
	}
	if topN, _ := got["topN"].(int); topN != dbTableBloatMaxTopN {
		t.Errorf("topN not clamped to max: %v", got["topN"])
	}
	rows := got["rows"].([]tableBloatRow)
	if len(rows) != dbTableBloatMaxTopN {
		t.Errorf("rows not truncated to topN: %d", len(rows))
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestDBTableBloat_ZeroBytesRowSkipped guards the divide-by-zero defense.
// A test that passed minBytes=0 with a zero-data zero-free row would have
// hit `0/0` without the explicit guard.
func TestDBTableBloat_ZeroBytesRowSkipped(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectQuery(regexp.QuoteMeta("FROM information_schema.TABLES")).
		WithArgs("acore_world").
		WillReturnRows(sqlmock.NewRows([]string{"TABLE_SCHEMA", "TABLE_NAME", "ENGINE", "DATA_LENGTH", "DATA_FREE"}).
			AddRow("acore_world", "empty", "InnoDB", uint64(0), uint64(0)).
			AddRow("acore_world", "real", "InnoDB", uint64(10*1024*1024), uint64(5*1024*1024)))

	out, err := collectTableBloat(context.Background(), db, []string{"acore_world"}, 0, 0, 10, nil)
	if err != nil {
		t.Fatalf("collectTableBloat: %v", err)
	}
	rows := out["rows"].([]tableBloatRow)
	if len(rows) != 1 || rows[0].Table != "real" {
		t.Errorf("expected only 'real' to survive zero-byte guard, got %+v", rows)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestDBTableBloat_SortTiebreakerDeterministic — two rows with identical
// bloat fractions must sort by schema-asc then table-asc. Without the
// stable tiebreaker the map iteration order would make this test flaky.
func TestDBTableBloat_SortTiebreakerDeterministic(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// Three rows all at exactly 50% bloat. Tiebreak: acore_auth.zeta < acore_characters.alpha < acore_world.alpha
	mock.ExpectQuery(regexp.QuoteMeta("FROM information_schema.TABLES")).
		WithArgs("acore_auth", "acore_characters", "acore_world").
		WillReturnRows(sqlmock.NewRows([]string{"TABLE_SCHEMA", "TABLE_NAME", "ENGINE", "DATA_LENGTH", "DATA_FREE"}).
			AddRow("acore_world", "alpha", "InnoDB", uint64(10*1024*1024), uint64(10*1024*1024)).
			AddRow("acore_auth", "zeta", "InnoDB", uint64(10*1024*1024), uint64(10*1024*1024)).
			AddRow("acore_characters", "alpha", "InnoDB", uint64(10*1024*1024), uint64(10*1024*1024)))

	out, err := collectTableBloat(
		context.Background(),
		db,
		[]string{"acore_auth", "acore_characters", "acore_world"},
		1*1024*1024,
		0.10,
		10,
		nil,
	)
	if err != nil {
		t.Fatalf("collectTableBloat: %v", err)
	}
	rows := out["rows"].([]tableBloatRow)
	if len(rows) != 3 {
		t.Fatalf("rowCount: %d", len(rows))
	}
	if rows[0].Database != "acore_auth" || rows[0].Table != "zeta" {
		t.Errorf("tiebreak [0]: %+v", rows[0])
	}
	if rows[1].Database != "acore_characters" || rows[1].Table != "alpha" {
		t.Errorf("tiebreak [1]: %+v", rows[1])
	}
	if rows[2].Database != "acore_world" || rows[2].Table != "alpha" {
		t.Errorf("tiebreak [2]: %+v", rows[2])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestBloatSchemaFilter unit-tests the helper directly. An empty arg
// returns the full three-schema allowlist; a specified arg passes through.
// The sorted ordering is load-bearing for the parameterized IN-clause
// determinism asserted in the aggregation tests above.
func TestBloatSchemaFilter(t *testing.T) {
	all := bloatSchemaFilter("")
	if len(all) != 3 {
		t.Fatalf("default schemas: %v", all)
	}
	if all[0] != "acore_auth" || all[1] != "acore_characters" || all[2] != "acore_world" {
		t.Errorf("default schemas not sorted: %v", all)
	}
	one := bloatSchemaFilter("acore_world")
	if len(one) != 1 || one[0] != "acore_world" {
		t.Errorf("single-schema passthrough wrong: %v", one)
	}
}

// itoaPad — small helper local to this test file (avoids importing strconv
// solely for the topN-clamping fixture). Pads to 3 digits so sort is
// lexicographically stable.
func itoaPad(n int) string {
	const digits = "0123456789"
	if n < 0 {
		n = -n
	}
	if n == 0 {
		return "000"
	}
	buf := []byte("000")
	for i := 2; i >= 0 && n > 0; i-- {
		buf[i] = digits[n%10]
		n /= 10
	}
	return string(buf)
}

// --- DATA_FREE scope: what bloatFraction actually measured -------------------

// TestCollectTableBloat_ScopePerTableWhenFilePerTableOn — the healthy realm.
// Every InnoDB row is labelled perTable and BOTH qualifier counters stay at
// zero, so a clean report adds nothing for the operator to read.
func TestCollectTableBloat_ScopePerTableWhenFilePerTableOn(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectQuery(regexp.QuoteMeta("FROM information_schema.TABLES")).
		WithArgs("acore_world").
		WillReturnRows(sqlmock.NewRows([]string{"TABLE_SCHEMA", "TABLE_NAME", "ENGINE", "DATA_LENGTH", "DATA_FREE"}).
			AddRow("acore_world", "creature", "InnoDB", uint64(10*1024*1024), uint64(10*1024*1024)))

	on := true
	out, err := collectTableBloat(context.Background(), db, []string{"acore_world"}, 1*1024*1024, 0.10, 10, &on)
	if err != nil {
		t.Fatalf("collectTableBloat: %v", err)
	}
	rows := out["rows"].([]tableBloatRow)
	if len(rows) != 1 {
		t.Fatalf("rowCount: %d", len(rows))
	}
	if rows[0].DataFreeScope != dbTableBloatScopePerTable {
		t.Errorf("dataFreeScope: %q, want %q", rows[0].DataFreeScope, dbTableBloatScopePerTable)
	}
	if n := out["rowsDataFreeTablespaceWide"].(int); n != 0 {
		t.Errorf("rowsDataFreeTablespaceWide: %d, want 0 on a per-table realm", n)
	}
	if n := out["rowsDataFreeScopeUnknown"].(int); n != 0 {
		t.Errorf("rowsDataFreeScopeUnknown: %d, want 0", n)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestCollectTableBloat_ScopeSharedTablespaceWhenFilePerTableOff — the bug's
// home. With innodb_file_per_table OFF, every InnoDB row's DATA_FREE is the
// tablespace-wide figure, and the response has to say so.
func TestCollectTableBloat_ScopeSharedTablespaceWhenFilePerTableOff(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectQuery(regexp.QuoteMeta("FROM information_schema.TABLES")).
		WithArgs("acore_world").
		WillReturnRows(sqlmock.NewRows([]string{"TABLE_SCHEMA", "TABLE_NAME", "ENGINE", "DATA_LENGTH", "DATA_FREE"}).
			AddRow("acore_world", "creature", "InnoDB", uint64(10*1024*1024), uint64(500*1024*1024)).
			AddRow("acore_world", "gameobject", "InnoDB", uint64(20*1024*1024), uint64(500*1024*1024)))

	off := false
	out, err := collectTableBloat(context.Background(), db, []string{"acore_world"}, 1*1024*1024, 0.10, 10, &off)
	if err != nil {
		t.Fatalf("collectTableBloat: %v", err)
	}
	rows := out["rows"].([]tableBloatRow)
	if len(rows) != 2 {
		t.Fatalf("rowCount: %d", len(rows))
	}
	for _, r := range rows {
		if r.DataFreeScope != dbTableBloatScopeSharedTablespace {
			t.Errorf("%s.%s dataFreeScope: %q, want %q", r.Database, r.Table, r.DataFreeScope, dbTableBloatScopeSharedTablespace)
		}
	}
	if n := out["rowsDataFreeTablespaceWide"].(int); n != 2 {
		t.Errorf("rowsDataFreeTablespaceWide: %d, want 2", n)
	}
	if n := out["rowsDataFreeScopeUnknown"].(int); n != 0 {
		t.Errorf("rowsDataFreeScopeUnknown: %d, want 0 — OFF is resolved, not unknown", n)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestCollectTableBloat_SharedTablespaceInvertsTheRanking is the measurement
// the whole change rests on, not an assertion about it. A shared tablespace
// hands every InnoDB row the SAME DATA_FREE, so bloatFraction becomes a
// strictly decreasing function of DATA_LENGTH: the tool returns the SMALLEST
// tables first — the exact inverse of "which tables should I OPTIMIZE" — and
// minBytes stops filtering because the tablespace's free space alone clears
// the 1 MiB floor. Both halves are asserted here so the qualifier fields can
// never be dismissed as cosmetic.
func TestCollectTableBloat_SharedTablespaceInvertsTheRanking(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	const tablespaceFree = uint64(500 * 1024 * 1024)
	mock.ExpectQuery(regexp.QuoteMeta("FROM information_schema.TABLES")).
		WithArgs("acore_world").
		WillReturnRows(sqlmock.NewRows([]string{"TABLE_SCHEMA", "TABLE_NAME", "ENGINE", "DATA_LENGTH", "DATA_FREE"}).
			// The genuinely huge, genuinely churny table an operator wants first.
			AddRow("acore_world", "big", "InnoDB", uint64(400*1024*1024), tablespaceFree).
			AddRow("acore_world", "medium", "InnoDB", uint64(40*1024*1024), tablespaceFree).
			// A 4 KiB config table: nothing to reclaim, and far below minBytes
			// on its own DATA_LENGTH.
			AddRow("acore_world", "tiny_config", "InnoDB", uint64(4*1024), tablespaceFree))

	off := false
	out, err := collectTableBloat(context.Background(), db, []string{"acore_world"}, dbTableBloatDefaultMinBytes, dbTableBloatDefaultMinFraction, 10, &off)
	if err != nil {
		t.Fatalf("collectTableBloat: %v", err)
	}
	rows := out["rows"].([]tableBloatRow)

	// minBytes stopped filtering: the 4 KiB table clears a 1 MiB floor purely
	// on the tablespace's free space.
	if len(rows) != 3 {
		t.Fatalf("rowCount: %d — want all 3, minBytes cannot filter when DATA_FREE alone clears it", len(rows))
	}
	// And the ranking is inverted: smallest table on top.
	if rows[0].Table != "tiny_config" {
		t.Errorf("top row %q — the inversion this qualifier exists to disclose puts the SMALLEST table first", rows[0].Table)
	}
	if rows[2].Table != "big" {
		t.Errorf("bottom row %q — the biggest table ranks LAST under a shared tablespace", rows[2].Table)
	}
	// Every one of those rows is flagged, so nothing above is silent.
	if n := out["rowsDataFreeTablespaceWide"].(int); n != 3 {
		t.Errorf("rowsDataFreeTablespaceWide: %d, want 3 — an inverted ranking must not be reported unqualified", n)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestCollectTableBloat_ScopeUnknownWhenUnresolved — nil setting means the
// scope could not be established. That is NOT the same as per-table, and
// counting it as clean is the exact conflation this change exists to remove.
func TestCollectTableBloat_ScopeUnknownWhenUnresolved(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectQuery(regexp.QuoteMeta("FROM information_schema.TABLES")).
		WithArgs("acore_world").
		WillReturnRows(sqlmock.NewRows([]string{"TABLE_SCHEMA", "TABLE_NAME", "ENGINE", "DATA_LENGTH", "DATA_FREE"}).
			AddRow("acore_world", "creature", "InnoDB", uint64(10*1024*1024), uint64(10*1024*1024)))

	out, err := collectTableBloat(context.Background(), db, []string{"acore_world"}, 1*1024*1024, 0.10, 10, nil)
	if err != nil {
		t.Fatalf("collectTableBloat: %v", err)
	}
	rows := out["rows"].([]tableBloatRow)
	if rows[0].DataFreeScope != dbTableBloatScopeUnknown {
		t.Errorf("dataFreeScope: %q, want %q", rows[0].DataFreeScope, dbTableBloatScopeUnknown)
	}
	if n := out["rowsDataFreeScopeUnknown"].(int); n != 1 {
		t.Errorf("rowsDataFreeScopeUnknown: %d, want 1", n)
	}
	if n := out["rowsDataFreeTablespaceWide"].(int); n != 0 {
		t.Errorf("rowsDataFreeTablespaceWide: %d, want 0 — unknown is not the same finding as shared", n)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestCollectTableBloat_NonInnoDBRowAlwaysPerTable — MyISAM DATA_FREE is the
// per-file gap left by deletes and owes nothing to the InnoDB tablespace
// setting, so it must stay perTable even when InnoDB is shared or unresolved.
// Labelling it otherwise would be a fabricated doubt.
func TestCollectTableBloat_NonInnoDBRowAlwaysPerTable(t *testing.T) {
	for _, tc := range []struct {
		name        string
		filePerTab  *bool
		wantInnodb  string
		wantMyISAM  string
		wantFlagged int
	}{
		{"innodbShared", boolPtrForBloat(false), dbTableBloatScopeSharedTablespace, dbTableBloatScopePerTable, 1},
		{"innodbUnknown", nil, dbTableBloatScopeUnknown, dbTableBloatScopePerTable, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()

			mock.ExpectQuery(regexp.QuoteMeta("FROM information_schema.TABLES")).
				WithArgs("acore_characters").
				WillReturnRows(sqlmock.NewRows([]string{"TABLE_SCHEMA", "TABLE_NAME", "ENGINE", "DATA_LENGTH", "DATA_FREE"}).
					AddRow("acore_characters", "innodb_tbl", "InnoDB", uint64(10*1024*1024), uint64(10*1024*1024)).
					AddRow("acore_characters", "myisam_tbl", "MyISAM", uint64(10*1024*1024), uint64(10*1024*1024)))

			out, err := collectTableBloat(context.Background(), db, []string{"acore_characters"}, 1*1024*1024, 0.10, 10, tc.filePerTab)
			if err != nil {
				t.Fatalf("collectTableBloat: %v", err)
			}
			byTable := map[string]string{}
			for _, r := range out["rows"].([]tableBloatRow) {
				byTable[r.Table] = r.DataFreeScope
			}
			if byTable["innodb_tbl"] != tc.wantInnodb {
				t.Errorf("innodb_tbl scope: %q, want %q", byTable["innodb_tbl"], tc.wantInnodb)
			}
			if byTable["myisam_tbl"] != tc.wantMyISAM {
				t.Errorf("myisam_tbl scope: %q, want %q — MyISAM DATA_FREE is per-file regardless", byTable["myisam_tbl"], tc.wantMyISAM)
			}
			if n := out["rowsDataFreeTablespaceWide"].(int); n != tc.wantFlagged {
				t.Errorf("rowsDataFreeTablespaceWide: %d, want %d", n, tc.wantFlagged)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Errorf("unmet mock expectations: %v", err)
			}
		})
	}
}

// TestCollectTableBloat_EmptySchemasCarriesCounters — the early-return path
// must carry the same keys as the full path, or a caller reading
// rowsDataFreeScopeUnknown finds it missing on an empty schema filter.
func TestCollectTableBloat_EmptySchemasCarriesCounters(t *testing.T) {
	out, err := collectTableBloat(context.Background(), nil, nil, 0, 0, 10, nil)
	if err != nil {
		t.Fatalf("collectTableBloat: %v", err)
	}
	for _, k := range []string{"rows", "rowCount", "scannedTables", "rowsDataFreeTablespaceWide", "rowsDataFreeScopeUnknown"} {
		if _, ok := out[k]; !ok {
			t.Errorf("empty-schema response missing %q", k)
		}
	}
}

// TestRowDataFreeScope covers the per-row mapping in isolation, including the
// case-insensitive engine match (information_schema reports "InnoDB", but a
// MariaDB build can report "INNODB").
func TestRowDataFreeScope(t *testing.T) {
	for _, tc := range []struct {
		engine, innodbScope, want string
	}{
		{"InnoDB", dbTableBloatScopePerTable, dbTableBloatScopePerTable},
		{"InnoDB", dbTableBloatScopeSharedTablespace, dbTableBloatScopeSharedTablespace},
		{"InnoDB", dbTableBloatScopeUnknown, dbTableBloatScopeUnknown},
		{"INNODB", dbTableBloatScopeSharedTablespace, dbTableBloatScopeSharedTablespace},
		{"MyISAM", dbTableBloatScopeSharedTablespace, dbTableBloatScopePerTable},
		{"Aria", dbTableBloatScopeUnknown, dbTableBloatScopePerTable},
		{"", dbTableBloatScopeUnknown, dbTableBloatScopePerTable},
	} {
		if got := rowDataFreeScope(tc.engine, tc.innodbScope); got != tc.want {
			t.Errorf("rowDataFreeScope(%q, %q) = %q, want %q", tc.engine, tc.innodbScope, got, tc.want)
		}
	}
}

// TestReadInnodbFilePerTable covers both boolean spellings MySQL/MariaDB use,
// plus the ways the lookup can fail to produce an answer. A failure must yield
// (nil, err) — never a false, which would be indistinguishable from a genuine
// OFF and would fabricate the very finding this change reports.
func TestReadInnodbFilePerTable(t *testing.T) {
	for _, tc := range []struct {
		name   string
		value  string
		wantOn bool
	}{
		{"ON", "ON", true},
		{"OFF", "OFF", false},
		{"one", "1", true},
		{"zero", "0", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			expectInnodbFilePerTable(mock, tc.value)
			got, err := readInnodbFilePerTable(context.Background(), db)
			if err != nil {
				t.Fatalf("readInnodbFilePerTable: %v", err)
			}
			if got == nil {
				t.Fatal("resolved value is nil")
			}
			if *got != tc.wantOn {
				t.Errorf("value %q resolved to %v, want %v", tc.value, *got, tc.wantOn)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Errorf("unmet mock expectations: %v", err)
			}
		})
	}

	t.Run("noRowsIsUnknownNotFalse", func(t *testing.T) {
		db, mock, err := sqlmock.New()
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		mock.ExpectQuery(regexp.QuoteMeta("SHOW GLOBAL VARIABLES LIKE 'innodb_file_per_table'")).
			WillReturnRows(sqlmock.NewRows([]string{"Variable_name", "Value"}))
		got, err := readInnodbFilePerTable(context.Background(), db)
		if got != nil {
			t.Errorf("no row must be unknown, got %v", *got)
		}
		if err == nil {
			t.Error("expected a reason to report alongside the unknown")
		}
	})

	t.Run("queryErrorIsUnknownNotFalse", func(t *testing.T) {
		db, mock, err := sqlmock.New()
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		mock.ExpectQuery(regexp.QuoteMeta("SHOW GLOBAL VARIABLES LIKE 'innodb_file_per_table'")).
			WillReturnError(errors.New("access denied"))
		got, err := readInnodbFilePerTable(context.Background(), db)
		if got != nil {
			t.Errorf("query error must be unknown, got %v", *got)
		}
		if err == nil {
			t.Fatal("expected the query error to be returned")
		}
		if !strings.Contains(err.Error(), "access denied") {
			t.Errorf("error should carry the driver reason, got %v", err)
		}
	})

	t.Run("nilPoolIsUnknown", func(t *testing.T) {
		got, err := readInnodbFilePerTable(context.Background(), nil)
		if got != nil || err == nil {
			t.Errorf("nil pool: got %v, err %v", got, err)
		}
	})
}

// TestTableBloatDataFreeBasis — each branch must name what was measured, and
// the unknown branch must carry the reason rather than an unexplained null.
func TestTableBloatDataFreeBasis(t *testing.T) {
	on, off := true, false

	b := tableBloatDataFreeBasis(&on, nil)
	if b["innodbScope"] != dbTableBloatScopePerTable {
		t.Errorf("ON scope: %v", b["innodbScope"])
	}
	if got := b["innodbFilePerTable"].(*bool); got == nil || !*got {
		t.Errorf("ON must report the resolved setting, got %v", b["innodbFilePerTable"])
	}
	if !strings.Contains(b["detail"].(string), "INNODB_TABLESPACES") {
		t.Errorf("the ON detail must not overclaim — it has to say per-table placement is unproven: %v", b["detail"])
	}

	b = tableBloatDataFreeBasis(&off, nil)
	if b["innodbScope"] != dbTableBloatScopeSharedTablespace {
		t.Errorf("OFF scope: %v", b["innodbScope"])
	}
	for _, kw := range []string{"smallest-table-first", "minBytes", "OPTIMIZE TABLE"} {
		if !strings.Contains(b["detail"].(string), kw) {
			t.Errorf("OFF detail missing %q: %v", kw, b["detail"])
		}
	}

	b = tableBloatDataFreeBasis(nil, errors.New("access denied"))
	if b["innodbScope"] != dbTableBloatScopeUnknown {
		t.Errorf("unknown scope: %v", b["innodbScope"])
	}
	if got := b["innodbFilePerTable"].(*bool); got != nil {
		t.Errorf("unknown must serialize as null, not a bool: %v", *got)
	}
	if !strings.Contains(b["detail"].(string), "access denied") {
		t.Errorf("unknown detail must carry the reason: %v", b["detail"])
	}
}

// TestDBTableBloat_HandlerEmitsDataFreeBasis — end-to-end through the handler:
// the resolved setting reaches both the response-level basis block and every
// row's scope.
func TestDBTableBloat_HandlerEmitsDataFreeBasis(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	reg := NewRegistry()
	RegisterDBTableBloatTool(reg, DBDeps{QueryDB: db})
	tool, _ := reg.Get("db_table_bloat")

	expectInnodbFilePerTable(mock, "OFF")
	mock.ExpectQuery(regexp.QuoteMeta("FROM information_schema.TABLES")).
		WithArgs("acore_world").
		WillReturnRows(sqlmock.NewRows([]string{"TABLE_SCHEMA", "TABLE_NAME", "ENGINE", "DATA_LENGTH", "DATA_FREE"}).
			AddRow("acore_world", "creature", "InnoDB", uint64(10*1024*1024), uint64(500*1024*1024)))

	resp := tool.Handler(context.Background(), json.RawMessage(`{"database":"acore_world"}`), "")
	got, _ := resp.(map[string]any)
	if msg, ok := got["error"]; ok {
		t.Fatalf("unexpected error: %v", msg)
	}
	basis, ok := got["dataFreeBasis"].(map[string]any)
	if !ok {
		t.Fatalf("dataFreeBasis missing from the response: %v", got["dataFreeBasis"])
	}
	if basis["innodbScope"] != dbTableBloatScopeSharedTablespace {
		t.Errorf("innodbScope: %v", basis["innodbScope"])
	}
	rows := got["rows"].([]tableBloatRow)
	if len(rows) != 1 || rows[0].DataFreeScope != dbTableBloatScopeSharedTablespace {
		t.Errorf("row scope did not follow the resolved setting: %+v", rows)
	}
	if n := got["rowsDataFreeTablespaceWide"].(int); n != 1 {
		t.Errorf("rowsDataFreeTablespaceWide: %d", n)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestDBTableBloat_HandlerDegradesWhenVariableUnreadable — an unreadable
// setting must NOT take the report down. The sizes are still true; only their
// scope is unknown, and the response says which and why.
func TestDBTableBloat_HandlerDegradesWhenVariableUnreadable(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	reg := NewRegistry()
	RegisterDBTableBloatTool(reg, DBDeps{QueryDB: db})
	tool, _ := reg.Get("db_table_bloat")

	mock.ExpectQuery(regexp.QuoteMeta("SHOW GLOBAL VARIABLES LIKE 'innodb_file_per_table'")).
		WillReturnError(errors.New("access denied"))
	mock.ExpectQuery(regexp.QuoteMeta("FROM information_schema.TABLES")).
		WithArgs("acore_world").
		WillReturnRows(sqlmock.NewRows([]string{"TABLE_SCHEMA", "TABLE_NAME", "ENGINE", "DATA_LENGTH", "DATA_FREE"}).
			AddRow("acore_world", "creature", "InnoDB", uint64(10*1024*1024), uint64(10*1024*1024)))

	resp := tool.Handler(context.Background(), json.RawMessage(`{"database":"acore_world"}`), "")
	got, _ := resp.(map[string]any)
	if msg, ok := got["error"]; ok {
		t.Fatalf("an unreadable variable must not fail the whole report: %v", msg)
	}
	rows := got["rows"].([]tableBloatRow)
	if len(rows) != 1 {
		t.Fatalf("rows lost on the degraded path: %+v", rows)
	}
	if rows[0].DataFreeScope != dbTableBloatScopeUnknown {
		t.Errorf("row scope: %q, want %q", rows[0].DataFreeScope, dbTableBloatScopeUnknown)
	}
	basis := got["dataFreeBasis"].(map[string]any)
	if !strings.Contains(basis["detail"].(string), "access denied") {
		t.Errorf("basis detail must name why the scope is unknown: %v", basis["detail"])
	}
	if n := got["rowsDataFreeScopeUnknown"].(int); n != 1 {
		t.Errorf("rowsDataFreeScopeUnknown: %d", n)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// boolPtrForBloat — table-driven cases need addressable bools; named for this
// file so it cannot collide with a sibling tool's helper in the same package.
func boolPtrForBloat(v bool) *bool { return &v }
