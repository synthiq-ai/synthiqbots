package mcpserver

import (
	"context"
	"encoding/json"
	"math"
	"regexp"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// TestDBAutoIncrementHeadroom_DescriptionMentionsContext keeps the description
// anchored to the keywords the agent matches when an operator asks "are any id
// counters about to run out?" / "which AUTO_INCREMENT is near its max?". Same
// guard shape as the db_size_summary / db_table_bloat description tests.
func TestDBAutoIncrementHeadroom_DescriptionMentionsContext(t *testing.T) {
	reg := NewRegistry()
	RegisterDBAutoIncrementHeadroomTool(reg, DBDeps{})
	tool, ok := reg.Get("db_auto_increment_headroom")
	if !ok {
		t.Fatal("db_auto_increment_headroom not registered")
	}
	for _, kw := range []string{
		"information_schema",
		"AUTO_INCREMENT",
		"minUsage",
		"db_size_summary",
		"db_table_info",
		"Out of range",
		"UNSIGNED",
		"BIGINT",
		"rowsEstimate",
		"exhaustion",
		"minRows",
		"acore_playerbots",
		"ops_grant_playerbots_ro",
		"ops_ro",
	} {
		if !strings.Contains(tool.Description, kw) {
			t.Errorf("description missing %q", kw)
		}
	}
}

// TestDBAutoIncrementHeadroom_ReadOnlyAnnotation — a capacity reporter that ever
// flipped to read/write would let an agent invoke it under the
// OPS_ADMIN_ALLOW_ACTIONS gate, which is wrong.
func TestDBAutoIncrementHeadroom_ReadOnlyAnnotation(t *testing.T) {
	reg := NewRegistry()
	RegisterDBAutoIncrementHeadroomTool(reg, DBDeps{})
	tool, _ := reg.Get("db_auto_increment_headroom")
	var ann map[string]any
	if err := json.Unmarshal(tool.Annotations, &ann); err != nil {
		t.Fatalf("decode annotations: %v", err)
	}
	if ann["readOnlyHint"] != true {
		t.Errorf("readOnlyHint expected true, got %v", ann["readOnlyHint"])
	}
}

func TestDBAutoIncrementHeadroom_NoPoolReturnsError(t *testing.T) {
	reg := NewRegistry()
	RegisterDBAutoIncrementHeadroomTool(reg, DBDeps{})
	tool, _ := reg.Get("db_auto_increment_headroom")
	resp := tool.Handler(context.Background(), json.RawMessage(`{}`), "")
	got, _ := resp.(map[string]any)
	if msg, _ := got["error"].(string); !strings.Contains(msg, "ops_ro pool not configured") {
		t.Errorf("expected pool-not-configured error, got %v", got)
	}
}

// TestDBAutoIncrementHeadroom_InvalidDatabaseRejected — a typo or injection
// attempt in `database` must be rejected before binding.
func TestDBAutoIncrementHeadroom_InvalidDatabaseRejected(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	reg := NewRegistry()
	RegisterDBAutoIncrementHeadroomTool(reg, DBDeps{QueryDB: db})
	tool, _ := reg.Get("db_auto_increment_headroom")
	resp := tool.Handler(context.Background(), json.RawMessage(`{"database":"mysql"}`), "")
	got, _ := resp.(map[string]any)
	if msg, _ := got["error"].(string); !strings.Contains(msg, "acore_world/acore_characters/acore_auth") {
		t.Errorf("expected database-allowlist error, got %v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("no query should have fired: %v", err)
	}
}

// TestDBAutoIncrementHeadroom_MinUsageRangeRejected — minUsage outside (0,1] is
// REJECTED (not clamped) with NO query issued, so a typo'd 70 surfaces as a bad
// filter instead of an empty "all healthy" result. Mirrors db_low_cardinality_
// indexes' maxSelectivity / ah_market_top_items' minQuality reject-not-clamp.
func TestDBAutoIncrementHeadroom_MinUsageRangeRejected(t *testing.T) {
	for _, bad := range []string{`{"minUsage":0}`, `{"minUsage":-0.1}`, `{"minUsage":1.5}`, `{"minUsage":70}`} {
		db, mock, err := sqlmock.New()
		if err != nil {
			t.Fatal(err)
		}
		reg := NewRegistry()
		RegisterDBAutoIncrementHeadroomTool(reg, DBDeps{QueryDB: db})
		tool, _ := reg.Get("db_auto_increment_headroom")
		resp := tool.Handler(context.Background(), json.RawMessage(bad), "")
		got, _ := resp.(map[string]any)
		if msg, _ := got["error"].(string); !strings.Contains(msg, "minUsage must be in (0,1]") {
			t.Errorf("args %s: expected minUsage range error, got %v", bad, got)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("args %s: no query should have fired: %v", bad, err)
		}
		db.Close()
	}
}

// TestDBAutoIncrementHeadroom_GoldenFold is the main aggregation test: a mix of
// high / medium / below-threshold / unrecognized-type rows verifying the
// classify + filter + worst-first sort + perDatabase rollup + honest denominators.
func TestDBAutoIncrementHeadroom_GoldenFold(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	cols := []string{"TABLE_SCHEMA", "TABLE_NAME", "ENGINE", "AUTO_INCREMENT", "TABLE_ROWS", "COLUMN_NAME", "DATA_TYPE", "COLUMN_TYPE"}
	mock.ExpectQuery(regexp.QuoteMeta("information_schema.TABLES t")).
		WithArgs("acore_world", "acore_characters", "acore_auth").
		WillReturnRows(sqlmock.NewRows(cols).
			// INT UNSIGNED at 3.9e9 / 4.29e9 = ~90.8% → high
			AddRow("acore_characters", "item_instance", "InnoDB", uint64(3900000000), int64(50000000), "guid", "int", "int(10) unsigned").
			// INT SIGNED at 1.6e9 / 2.147e9 = ~74.5% → medium
			AddRow("acore_characters", "mail", "InnoDB", uint64(1600000000), int64(200000), "id", "int", "int(11)").
			// TINYINT UNSIGNED 240/255 = ~94.1% → high (smallest type, worst usage)
			AddRow("acore_world", "tiny_cfg", "InnoDB", uint64(240), int64(200), "id", "tinyint", "tinyint(3) unsigned").
			// INT UNSIGNED 50000/4.29e9 ≈ 0.001% → below threshold, scanned-not-flagged
			AddRow("acore_auth", "account", "InnoDB", uint64(50000), int64(50000), "id", "int", "int(10) unsigned").
			// DECIMAL auto_increment (misconfiguration) → unrecognized type, excluded from denominator
			AddRow("acore_world", "weird_ai", "InnoDB", uint64(5), int64(5), "x", "decimal", "decimal(30,0)"))

	out, err := collectAutoIncrementHeadroom(
		context.Background(),
		db,
		[]string{"acore_world", "acore_characters", "acore_auth"},
		dbAutoIncHeadroomDefaultMinUsage,
	)
	if err != nil {
		t.Fatalf("collectAutoIncrementHeadroom: %v", err)
	}

	cands := out["candidates"].([]autoIncCandidate)
	if len(cands) != 3 {
		t.Fatalf("expected 3 candidates, got %d (%+v)", len(cands), cands)
	}
	// worst-first: high group by usage desc (tiny_cfg 0.941 > item_instance 0.908), then medium (mail)
	if cands[0].Table != "tiny_cfg" || cands[0].Severity != "high" {
		t.Errorf("candidate[0] expected tiny_cfg/high, got %+v", cands[0])
	}
	if cands[0].MaxValue != 255 || !cands[0].Unsigned || cands[0].CurrentAutoIncrement != 240 {
		t.Errorf("candidate[0] field mismatch: %+v", cands[0])
	}
	if cands[1].Table != "item_instance" || cands[1].Severity != "high" || cands[1].MaxValue != 4294967295 {
		t.Errorf("candidate[1] expected item_instance/high/4294967295, got %+v", cands[1])
	}
	if cands[2].Table != "mail" || cands[2].Severity != "medium" || cands[2].Unsigned || cands[2].MaxValue != 2147483647 {
		t.Errorf("candidate[2] expected mail/medium/signed/2147483647, got %+v", cands[2])
	}
	if !strings.Contains(cands[0].Reason, "Out of range") || !strings.Contains(cands[0].Reason, "tinyint(3) unsigned") || !strings.Contains(cands[0].Reason, "imminent") {
		t.Errorf("candidate[0] reason missing expected phrasing: %q", cands[0].Reason)
	}
	if d := cands[1].UsageRatio; d <= 0.90 || d >= 0.91 {
		t.Errorf("item_instance usageRatio = %v, want in (0.90,0.91) and rounded for stable JSON", d)
	}

	totals := out["totals"].(map[string]any)
	if totals["autoIncrementTablesScanned"].(int) != 4 {
		t.Errorf("scanned: %v (decimal row must be excluded, account included)", totals["autoIncrementTablesScanned"])
	}
	if totals["atRiskCount"].(int) != 3 {
		t.Errorf("atRiskCount: %v", totals["atRiskCount"])
	}
	if out["truncated"].(bool) {
		t.Errorf("truncated should be false")
	}

	perDB := out["perDatabase"].([]autoIncPerDatabase)
	if len(perDB) != 3 {
		t.Fatalf("perDatabase len: %d", len(perDB))
	}
	// sorted atRisk desc: characters(2) > world(1) > auth(0)
	if perDB[0].Database != "acore_characters" || perDB[0].AtRiskCount != 2 || perDB[0].AutoIncrementTablesScanned != 2 {
		t.Errorf("perDB[0] expected acore_characters 2/2, got %+v", perDB[0])
	}
	if perDB[1].Database != "acore_world" || perDB[1].AtRiskCount != 1 || perDB[1].AutoIncrementTablesScanned != 1 {
		t.Errorf("perDB[1] expected acore_world 1/1 (decimal excluded), got %+v", perDB[1])
	}
	if perDB[2].Database != "acore_auth" || perDB[2].AtRiskCount != 0 || perDB[2].AutoIncrementTablesScanned != 1 {
		t.Errorf("perDB[2] expected acore_auth 0/1, got %+v", perDB[2])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestDBAutoIncrementHeadroom_DatabaseFilterNarrowsBindings — a single database
// arg binds only that schema; the handler echoes scannedSchemas + the default
// minUsage.
func TestDBAutoIncrementHeadroom_DatabaseFilterNarrowsBindings(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	reg := NewRegistry()
	RegisterDBAutoIncrementHeadroomTool(reg, DBDeps{QueryDB: db})
	tool, _ := reg.Get("db_auto_increment_headroom")

	cols := []string{"TABLE_SCHEMA", "TABLE_NAME", "ENGINE", "AUTO_INCREMENT", "TABLE_ROWS", "COLUMN_NAME", "DATA_TYPE", "COLUMN_TYPE"}
	mock.ExpectQuery(regexp.QuoteMeta("information_schema.TABLES t")).
		WithArgs("acore_world").
		WillReturnRows(sqlmock.NewRows(cols).
			AddRow("acore_world", "tiny_cfg", "InnoDB", uint64(250), int64(10), "id", "tinyint", "tinyint(3) unsigned"))

	resp := tool.Handler(context.Background(), json.RawMessage(`{"database":"acore_world"}`), "")
	got, _ := resp.(map[string]any)
	if msg, ok := got["error"]; ok {
		t.Fatalf("unexpected error: %v", msg)
	}
	schemas := got["scannedSchemas"].([]string)
	if len(schemas) != 1 || schemas[0] != "acore_world" {
		t.Errorf("scannedSchemas not narrowed: %v", schemas)
	}
	if mu, _ := got["minUsage"].(float64); mu != dbAutoIncHeadroomDefaultMinUsage {
		t.Errorf("minUsage echo: %v", got["minUsage"])
	}
	cands := got["candidates"].([]autoIncCandidate)
	if len(cands) != 1 || cands[0].Table != "tiny_cfg" || cands[0].Severity != "high" {
		t.Errorf("expected one high tiny_cfg, got %+v", cands)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestDBAutoIncrementHeadroom_MinUsageBoundaryInclusive — a counter exactly at
// minUsage is flagged (>=), one just below is not. 204/255 == 0.8 exactly, a
// clean integer boundary on a TINYINT UNSIGNED.
func TestDBAutoIncrementHeadroom_MinUsageBoundaryInclusive(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	cols := []string{"TABLE_SCHEMA", "TABLE_NAME", "ENGINE", "AUTO_INCREMENT", "TABLE_ROWS", "COLUMN_NAME", "DATA_TYPE", "COLUMN_TYPE"}
	mock.ExpectQuery(regexp.QuoteMeta("information_schema.TABLES t")).
		WithArgs("acore_world").
		WillReturnRows(sqlmock.NewRows(cols).
			// 204/255 = 0.80 exactly → flagged at minUsage 0.80 (inclusive)
			AddRow("acore_world", "tiny_at", "InnoDB", uint64(204), int64(10), "id", "tinyint", "tinyint(3) unsigned").
			// 203/255 = 0.7960… < 0.80 → not flagged
			AddRow("acore_world", "tiny_below", "InnoDB", uint64(203), int64(10), "id", "tinyint", "tinyint(3) unsigned"))

	out, err := collectAutoIncrementHeadroom(context.Background(), db, []string{"acore_world"}, 0.80)
	if err != nil {
		t.Fatalf("collectAutoIncrementHeadroom: %v", err)
	}
	cands := out["candidates"].([]autoIncCandidate)
	if len(cands) != 1 || cands[0].Table != "tiny_at" {
		t.Errorf("expected only tiny_at at the inclusive boundary, got %+v", cands)
	}
	if out["totals"].(map[string]any)["autoIncrementTablesScanned"].(int) != 2 {
		t.Errorf("both rows should be scanned: %v", out["totals"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestDBAutoIncrementHeadroom_EmptyResultSurfacesAllSchemas — when nothing is at
// risk, candidates marshals to `[]` (not null) and every scanned schema still
// surfaces in perDatabase with zeroed counters.
func TestDBAutoIncrementHeadroom_EmptyResultSurfacesAllSchemas(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	cols := []string{"TABLE_SCHEMA", "TABLE_NAME", "ENGINE", "AUTO_INCREMENT", "TABLE_ROWS", "COLUMN_NAME", "DATA_TYPE", "COLUMN_TYPE"}
	mock.ExpectQuery(regexp.QuoteMeta("information_schema.TABLES t")).
		WithArgs("acore_world", "acore_characters", "acore_auth").
		WillReturnRows(sqlmock.NewRows(cols).
			AddRow("acore_auth", "account", "InnoDB", uint64(1000), int64(900), "id", "int", "int(10) unsigned"))

	out, err := collectAutoIncrementHeadroom(context.Background(), db, []string{"acore_world", "acore_characters", "acore_auth"}, dbAutoIncHeadroomDefaultMinUsage)
	if err != nil {
		t.Fatalf("collectAutoIncrementHeadroom: %v", err)
	}
	cands := out["candidates"].([]autoIncCandidate)
	if cands == nil || len(cands) != 0 {
		t.Errorf("expected non-nil empty candidates, got %+v", cands)
	}
	blob, _ := json.Marshal(out)
	if !strings.Contains(string(blob), `"candidates":[]`) {
		t.Errorf("candidates must marshal to [] not null: %s", blob)
	}
	perDB := out["perDatabase"].([]autoIncPerDatabase)
	if len(perDB) != 3 {
		t.Errorf("all three schemas must surface, got %d: %+v", len(perDB), perDB)
	}
	if out["totals"].(map[string]any)["atRiskCount"].(int) != 0 {
		t.Errorf("atRiskCount should be 0: %v", out["totals"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestDBAutoIncrementHeadroom_TruncationHonestTotals — more than the cap of
// at-risk counters truncates the candidate list but keeps totals.atRiskCount
// honest (pre-truncation), so the operator knows the list was clipped.
func TestDBAutoIncrementHeadroom_TruncationHonestTotals(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	cols := []string{"TABLE_SCHEMA", "TABLE_NAME", "ENGINE", "AUTO_INCREMENT", "TABLE_ROWS", "COLUMN_NAME", "DATA_TYPE", "COLUMN_TYPE"}
	rs := sqlmock.NewRows(cols)
	const total = dbAutoIncHeadroomMaxCandidates + 100 // 600
	for i := 0; i < total; i++ {
		// INT UNSIGNED at 4.0e9 / 4.29e9 ≈ 93% → all high
		rs.AddRow("acore_world", "t"+itoaPad(i), "InnoDB", uint64(4000000000), int64(1), "id", "int", "int(10) unsigned")
	}
	mock.ExpectQuery(regexp.QuoteMeta("information_schema.TABLES t")).
		WithArgs("acore_world").
		WillReturnRows(rs)

	out, err := collectAutoIncrementHeadroom(context.Background(), db, []string{"acore_world"}, dbAutoIncHeadroomDefaultMinUsage)
	if err != nil {
		t.Fatalf("collectAutoIncrementHeadroom: %v", err)
	}
	cands := out["candidates"].([]autoIncCandidate)
	if len(cands) != dbAutoIncHeadroomMaxCandidates {
		t.Errorf("candidates not capped: %d", len(cands))
	}
	if !out["truncated"].(bool) {
		t.Errorf("truncated should be true")
	}
	if got := out["totals"].(map[string]any)["atRiskCount"].(int); got != total {
		t.Errorf("atRiskCount must stay honest pre-truncation: got %d want %d", got, total)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestAutoIncrementMax unit-tests the type→ceiling map for every width and both
// signednesses, the `integer` alias, the BIGINT-UNSIGNED == MaxUint64 edge, and
// the unrecognized-type ok=false path.
func TestAutoIncrementMax(t *testing.T) {
	cases := []struct {
		dataType string
		unsigned bool
		want     uint64
		ok       bool
	}{
		{"tinyint", false, 127, true},
		{"tinyint", true, 255, true},
		{"smallint", false, 32767, true},
		{"smallint", true, 65535, true},
		{"mediumint", false, 8388607, true},
		{"mediumint", true, 16777215, true},
		{"int", false, 2147483647, true},
		{"int", true, 4294967295, true},
		{"integer", true, 4294967295, true},
		{"bigint", false, math.MaxInt64, true},
		{"bigint", true, math.MaxUint64, true},
		{"INT", true, 4294967295, true}, // case-insensitive
		{"decimal", false, 0, false},
		{"varchar", false, 0, false},
		{"", false, 0, false},
	}
	for _, c := range cases {
		got, ok := autoIncrementMax(c.dataType, c.unsigned)
		if got != c.want || ok != c.ok {
			t.Errorf("autoIncrementMax(%q,%v) = (%d,%v), want (%d,%v)", c.dataType, c.unsigned, got, ok, c.want, c.ok)
		}
	}
}

// TestAutoIncUsageRatio guards the div-by-zero (unrecognized type → max 0) path
// and a normal ratio.
func TestAutoIncUsageRatio(t *testing.T) {
	if _, ok := autoIncUsageRatio(100, 0); ok {
		t.Errorf("max==0 must return ok=false")
	}
	r, ok := autoIncUsageRatio(204, 255)
	if !ok || math.Abs(r-0.8) > 1e-9 {
		t.Errorf("autoIncUsageRatio(204,255) = (%v,%v), want (0.8,true)", r, ok)
	}
}

// TestClassifyAutoIncHeadroom — boundary at the 0.90 high cutoff (inclusive) and
// the medium band below it.
func TestClassifyAutoIncHeadroom(t *testing.T) {
	cases := []struct {
		ratio float64
		want  string
	}{
		{0.95, "high"},
		{0.90, "high"}, // boundary inclusive
		{0.8999, "medium"},
		{0.70, "medium"},
	}
	for _, c := range cases {
		if got := classifyAutoIncHeadroom(c.ratio); got != c.want {
			t.Errorf("classifyAutoIncHeadroom(%v) = %q, want %q", c.ratio, got, c.want)
		}
	}
}

// TestAutoIncSeverityRank — high sorts before medium sorts before anything else.
func TestAutoIncSeverityRank(t *testing.T) {
	if !(autoIncSeverityRank("high") < autoIncSeverityRank("medium") && autoIncSeverityRank("medium") < autoIncSeverityRank("")) {
		t.Errorf("severity rank order wrong: high=%d medium=%d other=%d",
			autoIncSeverityRank("high"), autoIncSeverityRank("medium"), autoIncSeverityRank(""))
	}
}

// TestRoundAutoIncUsage — rounds to 6 decimal places for stable JSON.
func TestRoundAutoIncUsage(t *testing.T) {
	if got := roundAutoIncUsage(0.9080382055); math.Abs(got-0.908038) > 1e-9 {
		t.Errorf("roundAutoIncUsage = %v, want 0.908038", got)
	}
}
