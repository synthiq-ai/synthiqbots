package mcpserver

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// TestDBIndexDataRatio_DescriptionMentionsContext keeps the description
// anchored to the keywords the agent uses to pick this tool when an operator
// says "which tables are over-indexed?" / "which tables have more index than
// data?". Same shape as the db_table_bloat / db_size_summary description
// guards. Keywords must live in the registered Description STRING, not the Go
// doc-comment (the test asserts tool.Description).
func TestDBIndexDataRatio_DescriptionMentionsContext(t *testing.T) {
	reg := NewRegistry()
	RegisterDBIndexDataRatioTool(reg, DBDeps{})
	tool, ok := reg.Get("db_index_data_ratio")
	if !ok {
		t.Fatal("db_index_data_ratio not registered")
	}
	for _, kw := range []string{
		"information_schema.TABLES",
		"INDEX_LENGTH",
		"DATA_LENGTH",
		"indexDataRatio",
		"minBytes",
		"minRatio",
		"topN",
		"db_over_indexed_tables",
		"db_size_summary",
		"acore_playerbots",
		"ops_grant_playerbots_ro",
		// The skip counters are only useful if the agent knows they exist and
		// knows what the zero-data one does NOT separate.
		"tablesSkippedZeroDataLength",
		"tablesSkippedByMinBytes",
		"tablesSkippedByMinRatio",
		"eligibleTables",
		"scannedTables",
		"IFNULL",
	} {
		if !strings.Contains(tool.Description, kw) {
			t.Errorf("description missing %q", kw)
		}
	}
}

// TestDBIndexDataRatio_ReadOnlyAnnotation guards the AnnRead() annotation — a
// space auditor that ever flipped to read/write would let an agent invoke it
// under the OPS_ADMIN_ALLOW_ACTIONS gate, which is wrong.
func TestDBIndexDataRatio_ReadOnlyAnnotation(t *testing.T) {
	reg := NewRegistry()
	RegisterDBIndexDataRatioTool(reg, DBDeps{})
	tool, _ := reg.Get("db_index_data_ratio")
	var ann map[string]any
	if err := json.Unmarshal(tool.Annotations, &ann); err != nil {
		t.Fatalf("decode annotations: %v", err)
	}
	if ann["readOnlyHint"] != true {
		t.Errorf("readOnlyHint expected true, got %v", ann["readOnlyHint"])
	}
}

func TestDBIndexDataRatio_NoPoolReturnsError(t *testing.T) {
	reg := NewRegistry()
	RegisterDBIndexDataRatioTool(reg, DBDeps{})
	tool, _ := reg.Get("db_index_data_ratio")
	resp := tool.Handler(context.Background(), json.RawMessage(`{}`), "")
	got, _ := resp.(map[string]any)
	if msg, _ := got["error"].(string); !strings.Contains(msg, "ops_ro pool not configured") {
		t.Errorf("expected pool-not-configured error, got %v", got)
	}
}

// TestDBIndexDataRatio_InvalidDatabaseRejected — the `database` arg, when set,
// must be in the allowlist. A typo (or an injection attempt like "mysql")
// must be rejected before binding.
func TestDBIndexDataRatio_InvalidDatabaseRejected(t *testing.T) {
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	reg := NewRegistry()
	RegisterDBIndexDataRatioTool(reg, DBDeps{QueryDB: db})
	tool, _ := reg.Get("db_index_data_ratio")
	resp := tool.Handler(context.Background(), json.RawMessage(`{"database":"mysql"}`), "")
	got, _ := resp.(map[string]any)
	if msg, _ := got["error"].(string); !strings.Contains(msg, "acore_world/acore_characters/acore_auth") {
		t.Errorf("expected database-allowlist error, got %v", got)
	}
}

// TestDBIndexDataRatio_DefaultAllSchemasAggregation verifies the default path
// (no database filter) binds all three schemas and sorts by indexDataRatio
// desc. Also exercises the filtering: a table with tiny indexes is below
// minBytes; a data-dominant table (ratio < 1) is below minRatio. Both are
// dropped, leaving the two over-indexed candidates sorted heaviest-index
// first.
func TestDBIndexDataRatio_DefaultAllSchemasAggregation(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectQuery(regexp.QuoteMeta("FROM information_schema.TABLES")).
		WithArgs("acore_auth", "acore_characters", "acore_world").
		WillReturnRows(sqlmock.NewRows([]string{"TABLE_SCHEMA", "TABLE_NAME", "ENGINE", "DATA_LENGTH", "INDEX_LENGTH", "TABLE_ROWS"}).
			// heavily over-indexed: 10 MiB data, 30 MiB index → ratio 3.0
			AddRow("acore_characters", "character_inventory", "InnoDB", uint64(10*1024*1024), uint64(30*1024*1024), int64(500000)).
			// moderately over-indexed: 20 MiB data, 30 MiB index → ratio 1.5
			AddRow("acore_world", "creature", "InnoDB", uint64(20*1024*1024), uint64(30*1024*1024), int64(300000)).
			// data-dominant: 100 MiB data, 20 MiB index → ratio 0.2 — below minRatio
			AddRow("acore_auth", "account", "InnoDB", uint64(100*1024*1024), uint64(20*1024*1024), int64(2000)).
			// tiny index: 512 KiB index — below minBytes, dropped despite ratio 4.0
			AddRow("acore_world", "spell_stub", "InnoDB", uint64(128*1024), uint64(512*1024), int64(10)).
			// zero-data table: skipped by the dataLength==0 guard
			AddRow("acore_world", "empty_stub", "InnoDB", uint64(0), uint64(4*1024*1024), int64(0)))

	out, err := collectIndexDataRatio(
		context.Background(),
		db,
		[]string{"acore_auth", "acore_characters", "acore_world"},
		dbIndexDataRatioDefaultMinBytes,
		dbIndexDataRatioDefaultMinRatio,
		dbIndexDataRatioDefaultTopN,
	)
	if err != nil {
		t.Fatalf("collectIndexDataRatio: %v", err)
	}
	rows := out["rows"].([]indexDataRatioRow)
	if len(rows) != 2 {
		t.Fatalf("rowCount: %d (rows=%+v)", len(rows), rows)
	}
	if rows[0].Database != "acore_characters" || rows[0].Table != "character_inventory" {
		t.Errorf("expected character_inventory at top, got %+v", rows[0])
	}
	if rows[1].Database != "acore_world" || rows[1].Table != "creature" {
		t.Errorf("expected creature second, got %+v", rows[1])
	}
	if rows[0].IndexDataRatio <= rows[1].IndexDataRatio {
		t.Errorf("sort wrong: row[0].ratio %v should be > row[1].ratio %v", rows[0].IndexDataRatio, rows[1].IndexDataRatio)
	}
	// ratio band checks (tolerance, not exact float literals): 3.0 and 1.5
	if rows[0].IndexDataRatio < 2.99 || rows[0].IndexDataRatio > 3.01 {
		t.Errorf("row[0] ratio out of band (want ~3.0): %v", rows[0].IndexDataRatio)
	}
	if rows[1].IndexDataRatio < 1.49 || rows[1].IndexDataRatio > 1.51 {
		t.Errorf("row[1] ratio out of band (want ~1.5): %v", rows[1].IndexDataRatio)
	}
	if rows[0].RowsEstimate != 500000 {
		t.Errorf("row[0] rowsEstimate: %d", rows[0].RowsEstimate)
	}
	if rows[0].DataLengthHuman == "" || rows[0].IndexLengthHuman == "" {
		t.Errorf("human-bytes fields not populated: %+v", rows[0])
	}
	if !strings.Contains(rows[0].IndexLengthHuman, "MiB") {
		t.Errorf("IndexLengthHuman missing MiB suffix: %q", rows[0].IndexLengthHuman)
	}
	if scanned := out["scannedTables"].(int); scanned != 5 {
		t.Errorf("scannedTables: %d", scanned)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestDBIndexDataRatio_DatabaseFilterNarrowsBindings — when a single database
// is passed, the IN clause must bind only that one schema. WithArgs() asserts
// the exact binding tuple so an accidental fall-through to all three fails.
func TestDBIndexDataRatio_DatabaseFilterNarrowsBindings(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	reg := NewRegistry()
	RegisterDBIndexDataRatioTool(reg, DBDeps{QueryDB: db})
	tool, _ := reg.Get("db_index_data_ratio")

	mock.ExpectQuery(regexp.QuoteMeta("FROM information_schema.TABLES")).
		WithArgs("acore_characters").
		WillReturnRows(sqlmock.NewRows([]string{"TABLE_SCHEMA", "TABLE_NAME", "ENGINE", "DATA_LENGTH", "INDEX_LENGTH", "TABLE_ROWS"}).
			AddRow("acore_characters", "characters", "InnoDB", uint64(50*1024*1024), uint64(80*1024*1024), int64(120000)))

	resp := tool.Handler(context.Background(), json.RawMessage(`{"database":"acore_characters"}`), "")
	got, _ := resp.(map[string]any)
	if msg, ok := got["error"]; ok {
		t.Fatalf("unexpected error: %v", msg)
	}
	schemas := got["schemas"].([]string)
	if len(schemas) != 1 || schemas[0] != "acore_characters" {
		t.Errorf("schemas not narrowed: %v", schemas)
	}
	rows := got["rows"].([]indexDataRatioRow)
	if len(rows) != 1 || rows[0].Table != "characters" {
		t.Errorf("expected one row characters, got %+v", rows)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestDBIndexDataRatio_MinRatioFilterExcludesDataDominant — a row at exactly
// minRatio passes (>=) and a row below is dropped. Boundary correctness
// matters because operators tune minRatio to surface borderline candidates.
func TestDBIndexDataRatio_MinRatioFilterExcludesDataDominant(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectQuery(regexp.QuoteMeta("FROM information_schema.TABLES")).
		WithArgs("acore_world").
		WillReturnRows(sqlmock.NewRows([]string{"TABLE_SCHEMA", "TABLE_NAME", "ENGINE", "DATA_LENGTH", "INDEX_LENGTH", "TABLE_ROWS"}).
			// ratio exactly 2.0 — passes the >= 2.0 threshold
			AddRow("acore_world", "hot", "InnoDB", uint64(10*1024*1024), uint64(20*1024*1024), int64(1)).
			// ratio 1.5 — below the 2.0 threshold, dropped
			AddRow("acore_world", "cool", "InnoDB", uint64(20*1024*1024), uint64(30*1024*1024), int64(1)))

	out, err := collectIndexDataRatio(
		context.Background(),
		db,
		[]string{"acore_world"},
		1*1024*1024,
		2.0,
		10,
	)
	if err != nil {
		t.Fatalf("collectIndexDataRatio: %v", err)
	}
	rows := out["rows"].([]indexDataRatioRow)
	if len(rows) != 1 || rows[0].Table != "hot" {
		t.Errorf("expected only 'hot' to survive, got %+v", rows)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestDBIndexDataRatio_MinBytesFilterExcludesTinyIndexes — a table with a
// 256 KiB index at ratio 8.0 is uninteresting (dropping the index reclaims
// <256 KiB). minBytes is the noise floor, applied to INDEX_LENGTH.
func TestDBIndexDataRatio_MinBytesFilterExcludesTinyIndexes(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectQuery(regexp.QuoteMeta("FROM information_schema.TABLES")).
		WithArgs("acore_auth").
		WillReturnRows(sqlmock.NewRows([]string{"TABLE_SCHEMA", "TABLE_NAME", "ENGINE", "DATA_LENGTH", "INDEX_LENGTH", "TABLE_ROWS"}).
			// 256 KiB index — below 1 MiB threshold, dropped despite ratio 8.0
			AddRow("acore_auth", "stub", "InnoDB", uint64(32*1024), uint64(256*1024), int64(5)).
			// 4 MiB index at ratio 2.0 — passes
			AddRow("acore_auth", "account", "InnoDB", uint64(2*1024*1024), uint64(4*1024*1024), int64(2000)))

	out, err := collectIndexDataRatio(
		context.Background(),
		db,
		[]string{"acore_auth"},
		1*1024*1024,
		1.0,
		10,
	)
	if err != nil {
		t.Fatalf("collectIndexDataRatio: %v", err)
	}
	rows := out["rows"].([]indexDataRatioRow)
	if len(rows) != 1 || rows[0].Table != "account" {
		t.Errorf("expected only 'account' to survive, got %+v", rows)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestDBIndexDataRatio_ZeroDataRowSkipped guards the divide-by-zero defense.
// A zero-DATA_LENGTH row whose INDEX_LENGTH clears minBytes would hit `n/0`
// without the explicit guard — verified here by passing minBytes=0.
func TestDBIndexDataRatio_ZeroDataRowSkipped(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectQuery(regexp.QuoteMeta("FROM information_schema.TABLES")).
		WithArgs("acore_world").
		WillReturnRows(sqlmock.NewRows([]string{"TABLE_SCHEMA", "TABLE_NAME", "ENGINE", "DATA_LENGTH", "INDEX_LENGTH", "TABLE_ROWS"}).
			AddRow("acore_world", "empty", "InnoDB", uint64(0), uint64(4*1024*1024), int64(0)).
			AddRow("acore_world", "real", "InnoDB", uint64(10*1024*1024), uint64(15*1024*1024), int64(1)))

	out, err := collectIndexDataRatio(context.Background(), db, []string{"acore_world"}, 0, 0, 10)
	if err != nil {
		t.Fatalf("collectIndexDataRatio: %v", err)
	}
	rows := out["rows"].([]indexDataRatioRow)
	if len(rows) != 1 || rows[0].Table != "real" {
		t.Errorf("expected only 'real' to survive zero-data guard, got %+v", rows)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestDBIndexDataRatio_TopNClamping verifies topN clamps to the hard max (50)
// and slices the survivors. Mirrors the db_table_bloat clamping guard.
func TestDBIndexDataRatio_TopNClamping(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	reg := NewRegistry()
	RegisterDBIndexDataRatioTool(reg, DBDeps{QueryDB: db})
	tool, _ := reg.Get("db_index_data_ratio")

	rs := sqlmock.NewRows([]string{"TABLE_SCHEMA", "TABLE_NAME", "ENGINE", "DATA_LENGTH", "INDEX_LENGTH", "TABLE_ROWS"})
	// 100 over-indexed tables, descending ratio so the slice keeps the worst 50.
	for i := 0; i < 100; i++ {
		indexBytes := uint64(100-i) * 1024 * 1024
		rs.AddRow("acore_world", "t"+itoaPad(i), "InnoDB", uint64(1*1024*1024), indexBytes, int64(1))
	}
	mock.ExpectQuery(regexp.QuoteMeta("FROM information_schema.TABLES")).
		WithArgs("acore_auth", "acore_characters", "acore_world").
		WillReturnRows(rs)

	resp := tool.Handler(context.Background(), json.RawMessage(`{"topN":99999}`), "")
	got, _ := resp.(map[string]any)
	if msg, ok := got["error"]; ok {
		t.Fatalf("unexpected error: %v", msg)
	}
	if topN, _ := got["topN"].(int); topN != dbIndexDataRatioMaxTopN {
		t.Errorf("topN not clamped to max: %v", got["topN"])
	}
	rows := got["rows"].([]indexDataRatioRow)
	if len(rows) != dbIndexDataRatioMaxTopN {
		t.Errorf("rows not truncated to topN: %d", len(rows))
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestDBIndexDataRatio_SortTiebreakerDeterministic — three rows with identical
// ratios must sort by schema-asc then table-asc. Without the stable tiebreaker
// Go's map-iteration order would make this flaky.
func TestDBIndexDataRatio_SortTiebreakerDeterministic(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// Three rows all at exactly ratio 2.0. Tiebreak: acore_auth.zeta < acore_characters.alpha < acore_world.alpha
	mock.ExpectQuery(regexp.QuoteMeta("FROM information_schema.TABLES")).
		WithArgs("acore_auth", "acore_characters", "acore_world").
		WillReturnRows(sqlmock.NewRows([]string{"TABLE_SCHEMA", "TABLE_NAME", "ENGINE", "DATA_LENGTH", "INDEX_LENGTH", "TABLE_ROWS"}).
			AddRow("acore_world", "alpha", "InnoDB", uint64(10*1024*1024), uint64(20*1024*1024), int64(1)).
			AddRow("acore_auth", "zeta", "InnoDB", uint64(10*1024*1024), uint64(20*1024*1024), int64(1)).
			AddRow("acore_characters", "alpha", "InnoDB", uint64(10*1024*1024), uint64(20*1024*1024), int64(1)))

	out, err := collectIndexDataRatio(
		context.Background(),
		db,
		[]string{"acore_auth", "acore_characters", "acore_world"},
		1*1024*1024,
		1.0,
		10,
	)
	if err != nil {
		t.Fatalf("collectIndexDataRatio: %v", err)
	}
	rows := out["rows"].([]indexDataRatioRow)
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

// TestDBIndexDataRatio_EmptyResultShape — a schema filter that matches no
// over-indexed tables returns a non-nil empty slice (not nil), so JSON
// encodes `[]` and not `null`.
func TestDBIndexDataRatio_EmptyResultShape(t *testing.T) {
	out, err := collectIndexDataRatio(context.Background(), nil, nil, 1*1024*1024, 1.0, 10)
	if err != nil {
		t.Fatalf("collectIndexDataRatio: %v", err)
	}
	rows := out["rows"].([]indexDataRatioRow)
	if rows == nil {
		t.Error("rows should be non-nil empty slice")
	}
	if len(rows) != 0 {
		t.Errorf("expected empty, got %+v", rows)
	}
	if out["scannedTables"].(int) != 0 {
		t.Errorf("scannedTables should be 0, got %v", out["scannedTables"])
	}
}

// TestIndexDataRatioSchemaFilter unit-tests the helper directly. An empty arg
// returns the full three-schema allowlist SORTED (load-bearing for the
// parameterized IN-clause binding determinism the aggregation tests assert);
// a specified arg passes through.
func TestIndexDataRatioSchemaFilter(t *testing.T) {
	all := indexDataRatioSchemaFilter("")
	if len(all) != 3 {
		t.Fatalf("default schemas: %v", all)
	}
	if all[0] != "acore_auth" || all[1] != "acore_characters" || all[2] != "acore_world" {
		t.Errorf("default schemas not sorted: %v", all)
	}
	one := indexDataRatioSchemaFilter("acore_world")
	if len(one) != 1 || one[0] != "acore_world" {
		t.Errorf("single-schema passthrough wrong: %v", one)
	}
}

// idrMiB keeps the fixtures below readable — every size in this tool's tests is
// a whole number of MiB, and the literal arithmetic otherwise dominates the
// AddRow lines and hides which column is being varied.
func idrMiB(n uint64) uint64 { return n * 1024 * 1024 }

// idrRows builds the six-column information_schema.TABLES result set
// collectIndexDataRatio scans, in the order it expects them.
func idrRows() *sqlmock.Rows {
	return sqlmock.NewRows([]string{"TABLE_SCHEMA", "TABLE_NAME", "ENGINE", "DATA_LENGTH", "INDEX_LENGTH", "TABLE_ROWS"})
}

// idrCounter type-asserts one of the skip counters to int, failing loudly if it
// is absent or a different Go type. This guards the "one response key, one Go
// type across every branch that sets it" rule: the counters are written by two
// separate map literals (the empty-schema early return and the main path), and
// map[string]any erases the difference, so a consumer type-asserting int would
// panic on whichever branch drifted. Nothing in the toolchain catches that —
// go vet does not look inside map[string]any.
func idrCounter(t *testing.T, out map[string]any, key string) int {
	t.Helper()
	v, ok := out[key]
	if !ok {
		t.Fatalf("response missing %q", key)
	}
	n, ok := v.(int)
	if !ok {
		t.Fatalf("%q is %T, want int", key, v)
	}
	return n
}

// TestDBIndexDataRatio_ZeroDataLengthRowsAreCountedNotDiscarded is the
// measurement this change rests on. Three tables clear the 1 MiB minBytes floor
// on INDEX_LENGTH and report DATA_LENGTH == 0 — an unbounded ratio, i.e. the
// most extreme members of the very set this tool ranks — alongside one table
// that is genuinely over-indexed. Before the counter existed the response read
// `rowCount: 1, scannedTables: 4` and the three extremes left no trace at all.
//
// Both halves are asserted because each is a separate promise: the three rows
// are still NOT ranked (an undefined ratio is not a large ratio, and +Inf would
// break json.Marshal and float an empty table above a real offender), AND they
// are now visible as a count.
func TestDBIndexDataRatio_ZeroDataLengthRowsAreCountedNotDiscarded(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectQuery(regexp.QuoteMeta("FROM information_schema.TABLES")).
		WithArgs("acore_world").
		WillReturnRows(idrRows().
			AddRow("acore_world", "zero_a", "InnoDB", uint64(0), idrMiB(4), int64(0)).
			AddRow("acore_world", "zero_b", "MyISAM", uint64(0), idrMiB(8), int64(0)).
			AddRow("acore_world", "zero_c", "InnoDB", uint64(0), idrMiB(16), int64(0)).
			AddRow("acore_world", "real_offender", "InnoDB", idrMiB(10), idrMiB(15), int64(4200)))

	out, err := collectIndexDataRatio(context.Background(), db, []string{"acore_world"}, idrMiB(1), 1.0, 10)
	if err != nil {
		t.Fatalf("collectIndexDataRatio: %v", err)
	}

	rows := out["rows"].([]indexDataRatioRow)
	if len(rows) != 1 || rows[0].Table != "real_offender" {
		t.Fatalf("undefined-ratio rows must not be ranked; got %+v", rows)
	}
	if got := idrCounter(t, out, "tablesSkippedZeroDataLength"); got != 3 {
		t.Errorf("tablesSkippedZeroDataLength: got %d, want 3 — undefined-ratio tables must not vanish silently", got)
	}
	if got := idrCounter(t, out, "scannedTables"); got != 4 {
		t.Errorf("scannedTables: got %d, want 4", got)
	}
	if got := idrCounter(t, out, "eligibleTables"); got != 1 {
		t.Errorf("eligibleTables: got %d, want 1", got)
	}
	// The zero-data rows cleared minBytes, so they must be attributed to the
	// zero-data guard and to nothing else.
	if got := idrCounter(t, out, "tablesSkippedByMinBytes"); got != 0 {
		t.Errorf("tablesSkippedByMinBytes: got %d, want 0", got)
	}
	if got := idrCounter(t, out, "tablesSkippedByMinRatio"); got != 0 {
		t.Errorf("tablesSkippedByMinRatio: got %d, want 0", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestDBIndexDataRatio_ZeroRowsDistinguishesNoFindingFromNotEvaluated pins the
// conflation itself. Both fixtures below produced a byte-identical
// `rows: [], rowCount: 0, scannedTables: 3` before this change, and they mean
// opposite things: the first is a healthy schema where nothing is over-indexed,
// the second is one where NOTHING COULD BE EVALUATED. Without the counters an
// operator reads "clean" off a tool that measured nothing.
func TestDBIndexDataRatio_ZeroRowsDistinguishesNoFindingFromNotEvaluated(t *testing.T) {
	// World A — every table measured fine, none is over-indexed.
	dbA, mockA, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer dbA.Close()
	mockA.ExpectQuery(regexp.QuoteMeta("FROM information_schema.TABLES")).
		WithArgs("acore_world").
		WillReturnRows(idrRows().
			AddRow("acore_world", "healthy_a", "InnoDB", idrMiB(100), idrMiB(4), int64(9)).
			AddRow("acore_world", "healthy_b", "InnoDB", idrMiB(200), idrMiB(8), int64(9)).
			AddRow("acore_world", "healthy_c", "InnoDB", idrMiB(400), idrMiB(16), int64(9)))
	outA, err := collectIndexDataRatio(context.Background(), dbA, []string{"acore_world"}, idrMiB(1), 1.0, 10)
	if err != nil {
		t.Fatalf("world A: %v", err)
	}

	// World B — every table has real indexes and no clustered data at all.
	dbB, mockB, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer dbB.Close()
	mockB.ExpectQuery(regexp.QuoteMeta("FROM information_schema.TABLES")).
		WithArgs("acore_world").
		WillReturnRows(idrRows().
			AddRow("acore_world", "opaque_a", "InnoDB", uint64(0), idrMiB(4), int64(0)).
			AddRow("acore_world", "opaque_b", "InnoDB", uint64(0), idrMiB(8), int64(0)).
			AddRow("acore_world", "opaque_c", "InnoDB", uint64(0), idrMiB(16), int64(0)))
	outB, err := collectIndexDataRatio(context.Background(), dbB, []string{"acore_world"}, idrMiB(1), 1.0, 10)
	if err != nil {
		t.Fatalf("world B: %v", err)
	}

	// The fields that used to be the WHOLE answer are identical...
	for _, f := range []string{"rowCount", "scannedTables"} {
		if idrCounter(t, outA, f) != idrCounter(t, outB, f) {
			t.Fatalf("fixture invalid: %s already differs (%v vs %v) — the two worlds must be indistinguishable without the counters",
				f, outA[f], outB[f])
		}
	}
	if idrCounter(t, outA, "rowCount") != 0 {
		t.Fatalf("fixture invalid: expected an empty findings list, got %v", outA["rows"])
	}

	// ...and the counters are what tell them apart.
	if got := idrCounter(t, outA, "tablesSkippedZeroDataLength"); got != 0 {
		t.Errorf("world A tablesSkippedZeroDataLength: got %d, want 0 — every table was measurable", got)
	}
	if got := idrCounter(t, outA, "tablesSkippedByMinRatio"); got != 3 {
		t.Errorf("world A tablesSkippedByMinRatio: got %d, want 3", got)
	}
	if got := idrCounter(t, outB, "tablesSkippedZeroDataLength"); got != 3 {
		t.Errorf("world B tablesSkippedZeroDataLength: got %d, want 3 — nothing here was evaluable", got)
	}
	if got := idrCounter(t, outB, "tablesSkippedByMinRatio"); got != 0 {
		t.Errorf("world B tablesSkippedByMinRatio: got %d, want 0 — no ratio was ever computed", got)
	}
	if err := mockA.ExpectationsWereMet(); err != nil {
		t.Errorf("world A unmet mock expectations: %v", err)
	}
	if err := mockB.ExpectationsWereMet(); err != nil {
		t.Errorf("world B unmet mock expectations: %v", err)
	}
}

// TestDBIndexDataRatio_SkipCountersAccountForEveryScannedTable pins the
// decomposition the Description promises:
//
//	scannedTables == tablesSkippedByMinBytes + tablesSkippedZeroDataLength +
//	                 tablesSkippedByMinRatio + eligibleTables
//
// Each counter is incremented only inside its own guard branch, so this holds
// by construction; the test exists so a future edit that adds a fourth silent
// `continue` fails here instead of quietly re-opening the conflation. The
// fixture exercises all four outcomes at once, and topN is set below the
// eligible count so truncation cannot be confused with the accounting.
func TestDBIndexDataRatio_SkipCountersAccountForEveryScannedTable(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectQuery(regexp.QuoteMeta("FROM information_schema.TABLES")).
		WithArgs("acore_world").
		WillReturnRows(idrRows().
			// 2 below the minBytes index floor
			AddRow("acore_world", "tiny_a", "InnoDB", idrMiB(50), uint64(4096), int64(3)).
			AddRow("acore_world", "tiny_b", "InnoDB", idrMiB(50), uint64(8192), int64(3)).
			// 3 undefined ratio
			AddRow("acore_world", "zero_a", "InnoDB", uint64(0), idrMiB(4), int64(0)).
			AddRow("acore_world", "zero_b", "InnoDB", uint64(0), idrMiB(5), int64(0)).
			AddRow("acore_world", "zero_c", "MyISAM", uint64(0), idrMiB(6), int64(0)).
			// 2 measured, data still dominates
			AddRow("acore_world", "fine_a", "InnoDB", idrMiB(100), idrMiB(4), int64(7)).
			AddRow("acore_world", "fine_b", "InnoDB", idrMiB(100), idrMiB(8), int64(7)).
			// 4 genuinely over-indexed
			AddRow("acore_world", "bad_a", "InnoDB", idrMiB(10), idrMiB(40), int64(11)).
			AddRow("acore_world", "bad_b", "InnoDB", idrMiB(10), idrMiB(30), int64(11)).
			AddRow("acore_world", "bad_c", "InnoDB", idrMiB(10), idrMiB(20), int64(11)).
			AddRow("acore_world", "bad_d", "InnoDB", idrMiB(10), idrMiB(11), int64(11)))

	out, err := collectIndexDataRatio(context.Background(), db, []string{"acore_world"}, idrMiB(1), 1.0, 2)
	if err != nil {
		t.Fatalf("collectIndexDataRatio: %v", err)
	}

	scanned := idrCounter(t, out, "scannedTables")
	byBytes := idrCounter(t, out, "tablesSkippedByMinBytes")
	byZero := idrCounter(t, out, "tablesSkippedZeroDataLength")
	byRatio := idrCounter(t, out, "tablesSkippedByMinRatio")
	eligible := idrCounter(t, out, "eligibleTables")

	if scanned != 11 {
		t.Errorf("scannedTables: got %d, want 11", scanned)
	}
	if byBytes != 2 {
		t.Errorf("tablesSkippedByMinBytes: got %d, want 2", byBytes)
	}
	if byZero != 3 {
		t.Errorf("tablesSkippedZeroDataLength: got %d, want 3", byZero)
	}
	if byRatio != 2 {
		t.Errorf("tablesSkippedByMinRatio: got %d, want 2", byRatio)
	}
	if eligible != 4 {
		t.Errorf("eligibleTables: got %d, want 4", eligible)
	}
	if sum := byBytes + byZero + byRatio + eligible; sum != scanned {
		t.Errorf("accounting broken: %d+%d+%d+%d = %d, scannedTables = %d — every scanned table must land in exactly one bucket",
			byBytes, byZero, byRatio, eligible, sum, scanned)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestDBIndexDataRatio_EligibleTablesRevealsTopNTruncation — rowCount alone
// cannot tell a complete answer from a truncated one, since a response with
// rowCount == topN is exactly what both look like. eligibleTables is captured
// before the cut, so eligibleTables > rowCount is the truncation signal.
func TestDBIndexDataRatio_EligibleTablesRevealsTopNTruncation(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	rows := idrRows()
	for _, n := range []struct {
		name string
		idx  uint64
	}{{"bad_a", 40}, {"bad_b", 30}, {"bad_c", 20}, {"bad_d", 15}, {"bad_e", 11}} {
		rows = rows.AddRow("acore_world", n.name, "InnoDB", idrMiB(10), idrMiB(n.idx), int64(5))
	}
	mock.ExpectQuery(regexp.QuoteMeta("FROM information_schema.TABLES")).
		WithArgs("acore_world").
		WillReturnRows(rows)

	out, err := collectIndexDataRatio(context.Background(), db, []string{"acore_world"}, idrMiB(1), 1.0, 2)
	if err != nil {
		t.Fatalf("collectIndexDataRatio: %v", err)
	}
	if got := idrCounter(t, out, "rowCount"); got != 2 {
		t.Errorf("rowCount: got %d, want 2 (topN)", got)
	}
	if got := idrCounter(t, out, "eligibleTables"); got != 5 {
		t.Errorf("eligibleTables: got %d, want 5 — the pre-truncation survivor count", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestDBIndexDataRatio_BothReturnPathsShareOneKeySet — the empty-schema early
// return and the main path are two independent map literals, so they can drift
// apart silently and force a consumer to branch on which shape it received.
// Compare the key sets directly rather than spot-checking fields.
func TestDBIndexDataRatio_BothReturnPathsShareOneKeySet(t *testing.T) {
	empty, err := collectIndexDataRatio(context.Background(), nil, nil, idrMiB(1), 1.0, 10)
	if err != nil {
		t.Fatalf("empty path: %v", err)
	}

	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.ExpectQuery(regexp.QuoteMeta("FROM information_schema.TABLES")).
		WithArgs("acore_world").
		WillReturnRows(idrRows().
			AddRow("acore_world", "bad_a", "InnoDB", idrMiB(10), idrMiB(40), int64(5)))
	main, err := collectIndexDataRatio(context.Background(), db, []string{"acore_world"}, idrMiB(1), 1.0, 10)
	if err != nil {
		t.Fatalf("main path: %v", err)
	}

	for k := range main {
		if _, ok := empty[k]; !ok {
			t.Errorf("empty-schema path is missing %q", k)
		}
	}
	for k := range empty {
		if _, ok := main[k]; !ok {
			t.Errorf("main path is missing %q", k)
		}
	}
	// Every counter must be a real int on BOTH branches, and zero on the empty
	// one — an untyped 0 constant lands as int here, but an explicit int64 on
	// one branch only would panic a consumer type-asserting the other.
	for _, k := range []string{"scannedTables", "tablesSkippedByMinBytes", "tablesSkippedZeroDataLength", "tablesSkippedByMinRatio", "eligibleTables", "rowCount"} {
		if got := idrCounter(t, empty, k); got != 0 {
			t.Errorf("empty path %s: got %d, want 0", k, got)
		}
		_ = idrCounter(t, main, k)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}
