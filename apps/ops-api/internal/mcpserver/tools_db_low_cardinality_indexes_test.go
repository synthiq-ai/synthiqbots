package mcpserver

import (
	"context"
	"encoding/json"
	"math"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

// statisticsCols is the fixed column set every fixture returns — mirrors the
// SELECT in collectLowCardinalityIndexes (CARDINALITY already IFNULL'd to -1 by
// the query, so fixtures pass int64(-1) to simulate a NULL estimate).
var statisticsCols = []string{
	"TABLE_SCHEMA", "TABLE_NAME", "INDEX_NAME", "SEQ_IN_INDEX", "COLUMN_NAME",
	"CARDINALITY", "INDEX_TYPE", "ENGINE", "TABLE_ROWS",
}

// TestDBLowCardinalityIndexes_DescriptionMentionsContext anchors the description
// to the keywords the agent uses to pick this tool. Same guard shape as the
// other db_* index-hygiene tools.
func TestDBLowCardinalityIndexes_DescriptionMentionsContext(t *testing.T) {
	reg := NewRegistry()
	RegisterDBLowCardinalityIndexesTool(reg, DBDeps{})
	tool, ok := reg.Get("db_low_cardinality_indexes")
	if !ok {
		t.Fatal("db_low_cardinality_indexes not registered")
	}
	for _, kw := range []string{
		"information_schema.STATISTICS", "CARDINALITY", "TABLE_ROWS", "selectivity",
		"db_size_summary", "db_table_bloat", "db_redundant_indexes", "PRIMARY",
		"UNIQUE", "BTREE", "ANALYZE TABLE", "acore_playerbots", "maxSelectivity", "minRows",
		// The could-not-evaluate counters: an operator has to be able to find
		// them (and the tool that diagnoses them) from the description alone,
		// otherwise the counts are back to being read as "healthy".
		"unmeasuredCount", "tablesSkippedByMinRows", "tablesSkippedZeroRows",
		"db_index_stats_staleness",
	} {
		if !strings.Contains(tool.Description, kw) {
			t.Errorf("description missing %q", kw)
		}
	}
}

func TestDBLowCardinalityIndexes_ReadOnlyAnnotation(t *testing.T) {
	reg := NewRegistry()
	RegisterDBLowCardinalityIndexesTool(reg, DBDeps{})
	tool, _ := reg.Get("db_low_cardinality_indexes")
	if !strings.Contains(string(tool.Annotations), `"readOnlyHint":true`) {
		t.Errorf("expected readOnlyHint:true, got %s", tool.Annotations)
	}
	if tool.Destructive {
		t.Errorf("destructive should be false")
	}
}

func TestDBLowCardinalityIndexes_NoPoolReturnsError(t *testing.T) {
	reg := NewRegistry()
	RegisterDBLowCardinalityIndexesTool(reg, DBDeps{})
	tool, _ := reg.Get("db_low_cardinality_indexes")
	resp := tool.Handler(context.Background(), json.RawMessage(`{}`), "")
	got, _ := resp.(map[string]any)
	if msg, _ := got["error"].(string); !strings.Contains(msg, "ops_ro pool not configured") {
		t.Errorf("expected pool-not-configured error, got %v", got)
	}
}

func TestDBLowCardinalityIndexes_InvalidDatabaseRejected(t *testing.T) {
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	reg := NewRegistry()
	RegisterDBLowCardinalityIndexesTool(reg, DBDeps{QueryDB: db})
	tool, _ := reg.Get("db_low_cardinality_indexes")
	resp := tool.Handler(context.Background(), json.RawMessage(`{"database":"acore_playerbots"}`), "")
	got, _ := resp.(map[string]any)
	if msg, _ := got["error"].(string); !strings.Contains(msg, "acore_world/acore_characters/acore_auth") {
		t.Errorf("expected allowlist error, got %v", got)
	}
}

// TestDBLowCardinalityIndexes_InvalidMaxSelectivityRejected — an out-of-range
// maxSelectivity is rejected (not clamped) with NO query run, so a typo'd value
// reads as "bad filter" rather than "everything is low-cardinality".
func TestDBLowCardinalityIndexes_InvalidMaxSelectivityRejected(t *testing.T) {
	for _, body := range []string{
		`{"maxSelectivity":0}`,
		`{"maxSelectivity":-0.5}`,
		`{"maxSelectivity":1.5}`,
	} {
		db, mock, err := sqlmock.New()
		if err != nil {
			t.Fatal(err)
		}
		// No ExpectQuery — a query firing would be an unmet-expectation failure
		// only if registered, but the real guard is: reject before touching DB.
		reg := NewRegistry()
		RegisterDBLowCardinalityIndexesTool(reg, DBDeps{QueryDB: db})
		tool, _ := reg.Get("db_low_cardinality_indexes")
		resp := tool.Handler(context.Background(), json.RawMessage(body), "")
		got, _ := resp.(map[string]any)
		if msg, _ := got["error"].(string); !strings.Contains(msg, "maxSelectivity must be in") {
			t.Errorf("body %s: expected range-reject error, got %v", body, got)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("body %s: a query was issued despite the reject: %v", body, err)
		}
		db.Close()
	}
}

// TestDBLowCardinalityIndexes_GoldenAggregation drives the default path
// (all three schemas, maxSelectivity 0.01, minRows 1000) and verifies the full
// fold: single + composite indexes, NULL-cardinality skip, minRows table skip,
// high/medium classification, worst-first sort, leading-vs-full cardinality,
// perDatabase counts, totals.
func TestDBLowCardinalityIndexes_GoldenAggregation(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	rs := sqlmock.NewRows(statisticsCols).
		// acore_auth.account.idx_email — selectivity ~0.998, NOT flagged (scanned).
		AddRow("acore_auth", "account", "idx_email", 1, "email", int64(4990), "BTREE", "InnoDB", int64(5000)).
		// acore_characters.characters: 3 indexes.
		AddRow("acore_characters", "characters", "idx_account", 1, "account", int64(50000), "BTREE", "InnoDB", int64(100000)). // sel 0.5, NOT flagged
		AddRow("acore_characters", "characters", "idx_race", 1, "race", int64(12), "BTREE", "InnoDB", int64(100000)).          // sel 0.00012, HIGH
		AddRow("acore_characters", "characters", "idx_zone_map", 1, "zone", int64(40), "BTREE", "InnoDB", int64(100000)).      // leading 40
		AddRow("acore_characters", "characters", "idx_zone_map", 2, "map", int64(300), "BTREE", "InnoDB", int64(100000)).      // full 300, sel 0.003, MEDIUM
		// acore_world.creature: idx_map low-card HIGH, idx_phasemask NULL (skip-flag).
		AddRow("acore_world", "creature", "idx_map", 1, "map", int64(1), "BTREE", "InnoDB", int64(2000)).              // sel 0.0005, HIGH
		AddRow("acore_world", "creature", "idx_phasemask", 1, "phaseMask", int64(-1), "BTREE", "InnoDB", int64(2000)). // NULL → scanned, not flagged
		// acore_world.tiny_config — rows 50 < minRows 1000 → table not even scanned.
		AddRow("acore_world", "tiny_config", "idx_x", 1, "x", int64(1), "BTREE", "InnoDB", int64(50))

	mock.ExpectQuery(regexp.QuoteMeta("FROM information_schema.STATISTICS")).
		WithArgs("acore_world", "acore_characters", "acore_auth").
		WillReturnRows(rs)

	reg := NewRegistry()
	RegisterDBLowCardinalityIndexesTool(reg, DBDeps{QueryDB: db})
	tool, _ := reg.Get("db_low_cardinality_indexes")
	resp := tool.Handler(context.Background(), json.RawMessage(`{}`), "")
	got, ok := resp.(map[string]any)
	if !ok {
		t.Fatalf("unexpected response shape: %T", resp)
	}
	if msg, hasErr := got["error"]; hasErr {
		t.Fatalf("unexpected error: %v", msg)
	}

	cands := got["candidates"].([]lowCardIndexRow)
	if len(cands) != 3 {
		t.Fatalf("expected 3 flagged indexes, got %d: %+v", len(cands), cands)
	}
	// Worst-first: HIGH idx_race (0.00012) < HIGH creature.idx_map (0.0005) < MEDIUM idx_zone_map (0.003).
	c0 := cands[0]
	if c0.Table != "characters" || c0.Index != "idx_race" || c0.Severity != "high" {
		t.Errorf("cands[0] should be characters/idx_race/high: %+v", c0)
	}
	if c0.IndexCardinality != 12 || c0.LeadingColumnCardinality != 12 || len(c0.Columns) != 1 || c0.Columns[0] != "race" {
		t.Errorf("cands[0] cardinality/columns wrong: %+v", c0)
	}
	if math.Abs(c0.Selectivity-0.00012) > 1e-9 {
		t.Errorf("cands[0] selectivity want 0.00012, got %v", c0.Selectivity)
	}
	c1 := cands[1]
	if c1.Database != "acore_world" || c1.Table != "creature" || c1.Index != "idx_map" || c1.Severity != "high" {
		t.Errorf("cands[1] should be world/creature/idx_map/high: %+v", c1)
	}
	if math.Abs(c1.Selectivity-0.0005) > 1e-9 {
		t.Errorf("cands[1] selectivity want 0.0005, got %v", c1.Selectivity)
	}
	c2 := cands[2]
	if c2.Table != "characters" || c2.Index != "idx_zone_map" || c2.Severity != "medium" {
		t.Errorf("cands[2] should be characters/idx_zone_map/medium: %+v", c2)
	}
	// Composite: leading column (zone) cardinality 40, full-index (zone,map) cardinality 300.
	if c2.LeadingColumnCardinality != 40 || c2.IndexCardinality != 300 {
		t.Errorf("cands[2] leading/full cardinality want 40/300: %+v", c2)
	}
	if len(c2.Columns) != 2 || c2.Columns[0] != "zone" || c2.Columns[1] != "map" {
		t.Errorf("cands[2] columns want [zone map]: %+v", c2.Columns)
	}
	if math.Abs(c2.Selectivity-0.003) > 1e-9 {
		t.Errorf("cands[2] selectivity want 0.003, got %v", c2.Selectivity)
	}

	totals := got["totals"].(map[string]any)
	// scanned: account(1) + characters{account,race,zone_map}(3) + creature{map,phasemask}(2) = 6; tiny_config excluded.
	if totals["indexesScanned"].(int) != 6 {
		t.Errorf("totals.indexesScanned want 6, got %v", totals["indexesScanned"])
	}
	if totals["lowCardinalityCount"].(int) != 3 {
		t.Errorf("totals.lowCardinalityCount want 3, got %v", totals["lowCardinalityCount"])
	}
	// creature.idx_phasemask has a NULL cardinality: still 1 of the 6 scanned
	// (the assertion above is the proof this stayed additive), but now also
	// visible as unmeasured instead of silently inflating the clean remainder.
	if totals["unmeasuredCount"].(int) != 1 {
		t.Errorf("totals.unmeasuredCount want 1 (creature.idx_phasemask), got %v", totals["unmeasuredCount"])
	}
	// tiny_config (50 rows) is the only table below minRows — counted, and NOT
	// counted as zero-rows, because 50 rows is a genuinely small table rather
	// than a missing estimate.
	if totals["tablesSkippedByMinRows"].(int) != 1 {
		t.Errorf("totals.tablesSkippedByMinRows want 1 (tiny_config), got %v", totals["tablesSkippedByMinRows"])
	}
	if totals["tablesSkippedZeroRows"].(int) != 0 {
		t.Errorf("totals.tablesSkippedZeroRows want 0 (tiny_config has 50 rows), got %v", totals["tablesSkippedZeroRows"])
	}

	perDB := got["perDatabase"].([]lowCardPerDatabase)
	if len(perDB) != 3 {
		t.Fatalf("expected 3 perDatabase rows, got %d", len(perDB))
	}
	// Sorted by lowCardinalityCount desc, schema asc: characters(2), world(1), auth(0).
	if perDB[0].Database != "acore_characters" || perDB[0].LowCardinalityCount != 2 || perDB[0].IndexesScanned != 3 {
		t.Errorf("perDB[0] want acore_characters 2/3: %+v", perDB[0])
	}
	if perDB[1].Database != "acore_world" || perDB[1].LowCardinalityCount != 1 || perDB[1].IndexesScanned != 2 {
		t.Errorf("perDB[1] want acore_world 1/2: %+v", perDB[1])
	}
	// Both could-not-evaluate counters land on acore_world, and the perDatabase
	// sort is deliberately NOT keyed on them — the existing order assertions
	// above are what proves that.
	if perDB[1].UnmeasuredCount != 1 || perDB[1].TablesSkippedByMinRows != 1 || perDB[1].TablesSkippedZeroRows != 0 {
		t.Errorf("perDB[1] acore_world want unmeasured 1 / skipped 1 / zeroRows 0: %+v", perDB[1])
	}
	if perDB[0].UnmeasuredCount != 0 || perDB[0].TablesSkippedByMinRows != 0 {
		t.Errorf("perDB[0] acore_characters should have nothing unevaluated: %+v", perDB[0])
	}
	if perDB[2].Database != "acore_auth" || perDB[2].LowCardinalityCount != 0 || perDB[2].IndexesScanned != 1 {
		t.Errorf("perDB[2] want acore_auth 0/1: %+v", perDB[2])
	}

	if got["truncated"].(bool) {
		t.Errorf("truncated should be false for 3 candidates")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestDBLowCardinalityIndexes_DatabaseFilterNarrowsScan verifies passing
// `database` narrows the IN clause to a single bind.
func TestDBLowCardinalityIndexes_DatabaseFilterNarrowsScan(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectQuery(regexp.QuoteMeta("FROM information_schema.STATISTICS")).
		WithArgs("acore_world"). // single bind — IN list narrowed to one schema
		WillReturnRows(sqlmock.NewRows(statisticsCols).
			AddRow("acore_world", "creature", "idx_map", 1, "map", int64(3), "BTREE", "InnoDB", int64(50000)))

	reg := NewRegistry()
	RegisterDBLowCardinalityIndexesTool(reg, DBDeps{QueryDB: db})
	tool, _ := reg.Get("db_low_cardinality_indexes")
	resp := tool.Handler(context.Background(), json.RawMessage(`{"database":"acore_world"}`), "")
	got := resp.(map[string]any)
	if msg, hasErr := got["error"]; hasErr {
		t.Fatalf("unexpected error: %v", msg)
	}
	scanned := got["scannedSchemas"].([]string)
	if len(scanned) != 1 || scanned[0] != "acore_world" {
		t.Errorf("scannedSchemas want [acore_world], got %v", scanned)
	}
	perDB := got["perDatabase"].([]lowCardPerDatabase)
	if len(perDB) != 1 || perDB[0].Database != "acore_world" {
		t.Errorf("perDatabase want one acore_world row, got %+v", perDB)
	}
	if len(got["candidates"].([]lowCardIndexRow)) != 1 {
		t.Errorf("expected 1 candidate, got %d", len(got["candidates"].([]lowCardIndexRow)))
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestDBLowCardinalityIndexes_MinRowsBoundary — a table whose rows == minRows is
// evaluated (inclusive); rows == minRows-1 is suppressed (not scanned).
func TestDBLowCardinalityIndexes_MinRowsBoundary(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectQuery(regexp.QuoteMeta("FROM information_schema.STATISTICS")).
		WithArgs("acore_world", "acore_characters", "acore_auth").
		WillReturnRows(sqlmock.NewRows(statisticsCols).
			AddRow("acore_world", "t_at", "idx_a", 1, "a", int64(1), "BTREE", "InnoDB", int64(1000)).   // rows == default minRows → kept
			AddRow("acore_world", "t_below", "idx_b", 1, "b", int64(1), "BTREE", "InnoDB", int64(999))) // rows < minRows → suppressed

	reg := NewRegistry()
	RegisterDBLowCardinalityIndexesTool(reg, DBDeps{QueryDB: db})
	tool, _ := reg.Get("db_low_cardinality_indexes")
	resp := tool.Handler(context.Background(), json.RawMessage(`{}`), "")
	got := resp.(map[string]any)
	cands := got["candidates"].([]lowCardIndexRow)
	if len(cands) != 1 || cands[0].Table != "t_at" {
		t.Fatalf("expected only t_at flagged (minRows boundary inclusive), got %+v", cands)
	}
	totals := got["totals"].(map[string]any)
	if totals["indexesScanned"].(int) != 1 {
		t.Errorf("indexesScanned want 1 (t_below not scanned), got %v", totals["indexesScanned"])
	}
	// t_below is still invisible to indexesScanned (unchanged, above) — but it
	// is no longer invisible to the response as a whole.
	if totals["tablesSkippedByMinRows"].(int) != 1 {
		t.Errorf("tablesSkippedByMinRows want 1 (t_below), got %v", totals["tablesSkippedByMinRows"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestDBLowCardinalityIndexes_EmptyResultNonNilSlice — when nothing is below the
// threshold, `candidates` is a non-nil empty slice (renders as []), and every
// scanned schema still surfaces in perDatabase.
func TestDBLowCardinalityIndexes_EmptyResultNonNilSlice(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectQuery(regexp.QuoteMeta("FROM information_schema.STATISTICS")).
		WithArgs("acore_world", "acore_characters", "acore_auth").
		WillReturnRows(sqlmock.NewRows(statisticsCols).
			// Highly selective index — not flagged.
			AddRow("acore_characters", "characters", "idx_name", 1, "name", int64(99000), "BTREE", "InnoDB", int64(100000)))

	reg := NewRegistry()
	RegisterDBLowCardinalityIndexesTool(reg, DBDeps{QueryDB: db})
	tool, _ := reg.Get("db_low_cardinality_indexes")
	resp := tool.Handler(context.Background(), json.RawMessage(`{}`), "")
	got := resp.(map[string]any)

	cands := got["candidates"].([]lowCardIndexRow)
	if cands == nil {
		t.Errorf("candidates should be non-nil empty slice")
	}
	if len(cands) != 0 {
		t.Errorf("expected 0 candidates, got %d", len(cands))
	}
	b, _ := json.Marshal(got)
	if !strings.Contains(string(b), `"candidates":[]`) {
		t.Errorf("candidates should marshal to [], got %s", b)
	}
	perDB := got["perDatabase"].([]lowCardPerDatabase)
	if len(perDB) != 3 {
		t.Errorf("all 3 scanned schemas should surface, got %d", len(perDB))
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestDBLowCardinalityIndexes_Truncation — more than the 500 cap flagged →
// candidates sliced to 500 + truncated:true, but totals stays the honest
// pre-cap count.
func TestDBLowCardinalityIndexes_Truncation(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	rs := sqlmock.NewRows(statisticsCols)
	const total = dbLowCardIndexesMaxCandidates + 100 // 600
	for i := 0; i < total; i++ {
		// Distinct big tables, each with one low-card (cardinality 1) index.
		tbl := "t_" + itoaSmall(i)
		rs.AddRow("acore_world", tbl, "idx_x", 1, "x", int64(1), "BTREE", "InnoDB", int64(100000))
	}
	mock.ExpectQuery(regexp.QuoteMeta("FROM information_schema.STATISTICS")).
		WithArgs("acore_world", "acore_characters", "acore_auth").
		WillReturnRows(rs)

	reg := NewRegistry()
	RegisterDBLowCardinalityIndexesTool(reg, DBDeps{QueryDB: db})
	tool, _ := reg.Get("db_low_cardinality_indexes")
	resp := tool.Handler(context.Background(), json.RawMessage(`{}`), "")
	got := resp.(map[string]any)

	cands := got["candidates"].([]lowCardIndexRow)
	if len(cands) != dbLowCardIndexesMaxCandidates {
		t.Errorf("candidates should be capped at %d, got %d", dbLowCardIndexesMaxCandidates, len(cands))
	}
	if !got["truncated"].(bool) {
		t.Errorf("truncated should be true")
	}
	totals := got["totals"].(map[string]any)
	if totals["lowCardinalityCount"].(int) != total {
		t.Errorf("totals.lowCardinalityCount should be honest pre-cap %d, got %v", total, totals["lowCardinalityCount"])
	}
	if totals["indexesScanned"].(int) != total {
		t.Errorf("totals.indexesScanned should be %d, got %v", total, totals["indexesScanned"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestDBLowCardinalityIndexes_PureHelpers covers the unit-testable helpers in
// isolation: selectivity div-guard, severity classification + ranking, rounding.
func TestDBLowCardinalityIndexes_PureHelpers(t *testing.T) {
	if sel, ok := lowCardSelectivity(50, 100000); !ok || math.Abs(sel-0.0005) > 1e-12 {
		t.Errorf("lowCardSelectivity(50,100000) want 0.0005,true got %v,%v", sel, ok)
	}
	if _, ok := lowCardSelectivity(5, 0); ok {
		t.Errorf("lowCardSelectivity with rows=0 should be not-computable")
	}
	if _, ok := lowCardSelectivity(5, -1); ok {
		t.Errorf("lowCardSelectivity with rows<0 should be not-computable")
	}

	// high cutoff = maxSelectivity/10; boundary (<=) is high.
	if got := classifyLowCardinality(0.0005, 0.01); got != "high" {
		t.Errorf("0.0005 vs 0.01 want high, got %s", got)
	}
	if got := classifyLowCardinality(0.001, 0.01); got != "high" {
		t.Errorf("0.001 (== maxSel/10 boundary) want high, got %s", got)
	}
	if got := classifyLowCardinality(0.005, 0.01); got != "medium" {
		t.Errorf("0.005 vs 0.01 want medium, got %s", got)
	}

	if lowCardSeverityRank("high") >= lowCardSeverityRank("medium") {
		t.Errorf("high should rank before medium")
	}
	if lowCardSeverityRank("medium") >= lowCardSeverityRank("other") {
		t.Errorf("medium should rank before unknown")
	}

	if got := roundSelectivity(0.123456789); math.Abs(got-0.123457) > 1e-12 {
		t.Errorf("roundSelectivity(0.123456789) want 0.123457, got %v", got)
	}
}

// lowCardDBByName picks one perDatabase row by schema. The perDatabase sort is
// by flagged-count desc then name, so positional indexing is fragile in the
// fixtures below where every count is zero.
func lowCardDBByName(t *testing.T, perDB []lowCardPerDatabase, name string) lowCardPerDatabase {
	t.Helper()
	for _, d := range perDB {
		if d.Database == name {
			return d
		}
	}
	t.Fatalf("no perDatabase row for %s in %+v", name, perDB)
	return lowCardPerDatabase{}
}

// TestDBLowCardinalityIndexes_UnmeasuredNotFoldedIntoCleanCount is the headline
// regression for this fix, reproducing the exact reading the tool used to make
// impossible: a schema where EVERY index has a NULL cardinality reported
// `indexesScanned: 600, lowCardinalityCount: 0` — identical output to 600
// indexes that were measured and found perfectly selective. The two differ by
// everything an operator would do next, so the counts must now disagree.
func TestDBLowCardinalityIndexes_UnmeasuredNotFoldedIntoCleanCount(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	rs := sqlmock.NewRows(statisticsCols)
	const total = 600
	for i := 0; i < total; i++ {
		// Big tables (well over minRows) whose statistics were never collected:
		// the post-bulk-import state ac-db-import leaves acore_world in.
		rs.AddRow("acore_world", "t_"+itoaSmall(i), "idx_x", 1, "x", int64(-1), "BTREE", "InnoDB", int64(100000))
	}
	mock.ExpectQuery(regexp.QuoteMeta("FROM information_schema.STATISTICS")).
		WithArgs("acore_world", "acore_characters", "acore_auth").
		WillReturnRows(rs)

	reg := NewRegistry()
	RegisterDBLowCardinalityIndexesTool(reg, DBDeps{QueryDB: db})
	tool, _ := reg.Get("db_low_cardinality_indexes")
	resp := tool.Handler(context.Background(), json.RawMessage(`{}`), "")
	got := resp.(map[string]any)
	if msg, hasErr := got["error"]; hasErr {
		t.Fatalf("unexpected error: %v", msg)
	}

	totals := got["totals"].(map[string]any)
	// Unchanged behaviour: they really were scanned, and nothing is flagged.
	if totals["indexesScanned"].(int) != total {
		t.Errorf("indexesScanned want %d, got %v", total, totals["indexesScanned"])
	}
	if totals["lowCardinalityCount"].(int) != 0 {
		t.Errorf("lowCardinalityCount want 0, got %v", totals["lowCardinalityCount"])
	}
	// The fix: that "0 flagged" is now qualified rather than reassuring.
	if totals["unmeasuredCount"].(int) != total {
		t.Fatalf("unmeasuredCount want %d — a clean count here is the bug, got %v",
			total, totals["unmeasuredCount"])
	}
	// Nothing was gated out at the table level; this end of the audit is clean.
	if totals["tablesSkippedByMinRows"].(int) != 0 {
		t.Errorf("tablesSkippedByMinRows want 0, got %v", totals["tablesSkippedByMinRows"])
	}
	if len(got["candidates"].([]lowCardIndexRow)) != 0 {
		t.Errorf("nothing is classifiable, so candidates must be empty")
	}
	world := lowCardDBByName(t, got["perDatabase"].([]lowCardPerDatabase), "acore_world")
	if world.UnmeasuredCount != total || world.IndexesScanned != total || world.LowCardinalityCount != 0 {
		t.Errorf("acore_world want scanned/unmeasured %d and 0 flagged: %+v", total, world)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestDBLowCardinalityIndexes_SkippedTablesCountedOncePerTable — the minRows
// gate is a TABLE property but finalize() runs per index group, so the counter
// must dedupe. A table with three indexes is one skipped table, not three.
func TestDBLowCardinalityIndexes_SkippedTablesCountedOncePerTable(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectQuery(regexp.QuoteMeta("FROM information_schema.STATISTICS")).
		WithArgs("acore_world", "acore_characters", "acore_auth").
		WillReturnRows(sqlmock.NewRows(statisticsCols).
			// One small table carrying three separate indexes.
			AddRow("acore_world", "t_multi", "idx_a", 1, "a", int64(1), "BTREE", "InnoDB", int64(100)).
			AddRow("acore_world", "t_multi", "idx_b", 1, "b", int64(1), "BTREE", "InnoDB", int64(100)).
			AddRow("acore_world", "t_multi", "idx_c", 1, "c", int64(1), "BTREE", "InnoDB", int64(100)).
			// A second small table, in a different schema.
			AddRow("acore_characters", "t_other", "idx_d", 1, "d", int64(1), "BTREE", "InnoDB", int64(200)))

	reg := NewRegistry()
	RegisterDBLowCardinalityIndexesTool(reg, DBDeps{QueryDB: db})
	tool, _ := reg.Get("db_low_cardinality_indexes")
	resp := tool.Handler(context.Background(), json.RawMessage(`{}`), "")
	got := resp.(map[string]any)

	totals := got["totals"].(map[string]any)
	if totals["tablesSkippedByMinRows"].(int) != 2 {
		t.Errorf("want 2 skipped TABLES (not 4 index groups), got %v", totals["tablesSkippedByMinRows"])
	}
	if totals["indexesScanned"].(int) != 0 {
		t.Errorf("skipped tables must not reach indexesScanned, got %v", totals["indexesScanned"])
	}
	if totals["unmeasuredCount"].(int) != 0 {
		t.Errorf("a gated-out table is not 'unmeasured' — that counter is for scanned indexes, got %v",
			totals["unmeasuredCount"])
	}
	perDB := got["perDatabase"].([]lowCardPerDatabase)
	if w := lowCardDBByName(t, perDB, "acore_world"); w.TablesSkippedByMinRows != 1 {
		t.Errorf("acore_world want 1 skipped table (t_multi, 3 indexes): %+v", w)
	}
	if c := lowCardDBByName(t, perDB, "acore_characters"); c.TablesSkippedByMinRows != 1 {
		t.Errorf("acore_characters want 1 skipped table: %+v", c)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestDBLowCardinalityIndexes_ZeroRowEstimateTrackedAsSubset — a table reporting
// 0 rows (TABLE_ROWS NULL is IFNULL'd to 0 in SQL) is either empty or never
// analyzed, which is a different diagnosis from "genuinely small". Both are
// skipped by the gate, so the zero-row case is tracked as a subset rather than
// folded in — otherwise this counter would repeat, one level down, the exact
// conflation unmeasuredCount exists to fix.
func TestDBLowCardinalityIndexes_ZeroRowEstimateTrackedAsSubset(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectQuery(regexp.QuoteMeta("FROM information_schema.STATISTICS")).
		WithArgs("acore_world", "acore_characters", "acore_auth").
		WillReturnRows(sqlmock.NewRows(statisticsCols).
			// No row estimate at all — empty, or never analyzed.
			AddRow("acore_world", "t_never", "idx_a", 1, "a", int64(-1), "BTREE", "InnoDB", int64(0)).
			AddRow("acore_world", "t_never", "idx_b", 1, "b", int64(-1), "BTREE", "InnoDB", int64(0)).
			// Genuinely small: a real estimate, just below the gate.
			AddRow("acore_world", "t_small", "idx_c", 1, "c", int64(1), "BTREE", "InnoDB", int64(50)))

	reg := NewRegistry()
	RegisterDBLowCardinalityIndexesTool(reg, DBDeps{QueryDB: db})
	tool, _ := reg.Get("db_low_cardinality_indexes")
	resp := tool.Handler(context.Background(), json.RawMessage(`{}`), "")
	got := resp.(map[string]any)

	totals := got["totals"].(map[string]any)
	skipped := totals["tablesSkippedByMinRows"].(int)
	zeroRows := totals["tablesSkippedZeroRows"].(int)
	if skipped != 2 {
		t.Errorf("tablesSkippedByMinRows want 2 (t_never, t_small), got %d", skipped)
	}
	if zeroRows != 1 {
		t.Errorf("tablesSkippedZeroRows want 1 (t_never only — t_small has a real estimate), got %d", zeroRows)
	}
	if zeroRows > skipped {
		t.Errorf("zero-row tables must be a SUBSET of skipped tables: %d > %d", zeroRows, skipped)
	}
	w := lowCardDBByName(t, got["perDatabase"].([]lowCardPerDatabase), "acore_world")
	if w.TablesSkippedByMinRows != 2 || w.TablesSkippedZeroRows != 1 {
		t.Errorf("acore_world want skipped 2 / zeroRows 1: %+v", w)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestDBLowCardinalityIndexes_MinRowsDisabledStillCountsUnmeasurable drives
// collectLowCardinalityIndexes directly with minRows=0 — a state the MCP handler
// cannot produce (it floors minRows at 1) but a direct caller can. Two things are
// pinned: the zero-row subset stays a subset by CONSTRUCTION (nothing is skipped,
// so both skip counters are 0 rather than disagreeing), and the selectivity
// div-guard that now becomes reachable still counts its drop as unmeasured
// instead of silently inflating the clean remainder again.
func TestDBLowCardinalityIndexes_MinRowsDisabledStillCountsUnmeasurable(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectQuery(regexp.QuoteMeta("FROM information_schema.STATISTICS")).
		WithArgs("acore_world").
		WillReturnRows(sqlmock.NewRows(statisticsCols).
			// Real cardinality, but no row estimate: selectivity is not
			// computable, so this reaches the div-guard rather than the
			// NULL-cardinality branch.
			AddRow("acore_world", "t_zero", "idx_a", 1, "a", int64(5), "BTREE", "InnoDB", int64(0)))

	out, err := collectLowCardinalityIndexes(
		context.Background(), db, time.Now(), []string{"acore_world"}, 0.01, 0)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	totals := out["totals"].(map[string]any)
	if totals["indexesScanned"].(int) != 1 {
		t.Errorf("minRows=0 lets the table through the gate: want scanned 1, got %v", totals["indexesScanned"])
	}
	if totals["unmeasuredCount"].(int) != 1 {
		t.Errorf("an uncomputable selectivity is unmeasured, not clean: want 1, got %v", totals["unmeasuredCount"])
	}
	if totals["lowCardinalityCount"].(int) != 0 {
		t.Errorf("nothing classifiable, want 0 flagged, got %v", totals["lowCardinalityCount"])
	}
	skipped := totals["tablesSkippedByMinRows"].(int)
	zeroRows := totals["tablesSkippedZeroRows"].(int)
	if skipped != 0 || zeroRows != 0 {
		t.Errorf("minRows=0 skips nothing: want 0/0, got %d/%d", skipped, zeroRows)
	}
	if zeroRows > skipped {
		t.Errorf("subset invariant broken at minRows=0: %d > %d", zeroRows, skipped)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}
