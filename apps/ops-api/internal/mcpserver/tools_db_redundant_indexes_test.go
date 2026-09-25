package mcpserver

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// TestDBRedundantIndexes_DescriptionMentionsContext anchors the description to
// the keywords the agent uses to pick this tool. Same shape guard as the other
// db_* tools.
func TestDBRedundantIndexes_DescriptionMentionsContext(t *testing.T) {
	reg := NewRegistry()
	RegisterDBRedundantIndexesTool(reg, DBDeps{})
	tool, ok := reg.Get("db_redundant_indexes")
	if !ok {
		t.Fatal("db_redundant_indexes not registered")
	}
	for _, kw := range []string{
		"information_schema.STATISTICS", "duplicate", "prefix", "PRIMARY",
		"UNIQUE", "INDEX_TYPE", "db_size_summary", "db_table_bloat",
		"candidates", "perDatabase", "cardinalityEstimate", "acore_playerbots",
		"clustered",
	} {
		if !strings.Contains(tool.Description, kw) {
			t.Errorf("description missing %q", kw)
		}
	}
}

func TestDBRedundantIndexes_ReadOnlyAnnotation(t *testing.T) {
	reg := NewRegistry()
	RegisterDBRedundantIndexesTool(reg, DBDeps{})
	tool, _ := reg.Get("db_redundant_indexes")
	if !strings.Contains(string(tool.Annotations), `"readOnlyHint":true`) {
		t.Errorf("expected readOnlyHint:true, got %s", tool.Annotations)
	}
	if tool.Destructive {
		t.Errorf("destructive should be false")
	}
}

func TestDBRedundantIndexes_NoPoolReturnsError(t *testing.T) {
	reg := NewRegistry()
	RegisterDBRedundantIndexesTool(reg, DBDeps{})
	tool, _ := reg.Get("db_redundant_indexes")
	resp := tool.Handler(context.Background(), json.RawMessage(`{}`), "")
	got, _ := resp.(map[string]any)
	if msg, _ := got["error"].(string); !strings.Contains(msg, "ops_ro pool not configured") {
		t.Errorf("expected pool-not-configured error, got %v", got)
	}
}

func TestDBRedundantIndexes_InvalidDatabaseRejected(t *testing.T) {
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	reg := NewRegistry()
	RegisterDBRedundantIndexesTool(reg, DBDeps{QueryDB: db})
	tool, _ := reg.Get("db_redundant_indexes")
	// acore_playerbots is intentionally NOT in the allowlist — ops_ro lacks the grant.
	resp := tool.Handler(context.Background(), json.RawMessage(`{"database":"acore_playerbots"}`), "")
	got, _ := resp.(map[string]any)
	if msg, _ := got["error"].(string); !strings.Contains(msg, "acore_world/acore_characters/acore_auth") {
		t.Errorf("expected allowlist error, got %v", got)
	}
}

func redundantStatsColumns() []string {
	return []string{
		"TABLE_SCHEMA", "TABLE_NAME", "INDEX_NAME", "SEQ_IN_INDEX",
		"COLUMN_NAME", "NON_UNIQUE", "INDEX_TYPE", "CARDINALITY",
	}
}

// TestDBRedundantIndexes_GoldenFold drives the default all-schemas path with a
// fixture that exercises every redundancy verdict:
//   - acore_characters.character_inventory: a prefix-redundant index (idx_bag ⊂
//     idx_bag_slot), a true duplicate pair (idx_dup_a / idx_dup_b → one flagged),
//     and a PRIMARY that must NEVER be flagged.
//   - acore_world.creature: a UNIQUE index that is a prefix of a longer index
//     but must NOT be flagged (dropping it would lose the constraint).
//   - acore_auth: no rows — must still surface in perDatabase with zeroes.
func TestDBRedundantIndexes_GoldenFold(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	rows := sqlmock.NewRows(redundantStatsColumns()).
		// character_inventory — rows arrive index-name asc, SEQ asc (real ORDER BY).
		AddRow("acore_characters", "character_inventory", "PRIMARY", int64(1), "guid", int64(0), "BTREE", int64(1000)).
		AddRow("acore_characters", "character_inventory", "PRIMARY", int64(2), "slot", int64(0), "BTREE", int64(1000)).
		AddRow("acore_characters", "character_inventory", "idx_bag", int64(1), "bag", int64(1), "BTREE", int64(50)).
		AddRow("acore_characters", "character_inventory", "idx_bag_slot", int64(1), "bag", int64(1), "BTREE", int64(60)).
		AddRow("acore_characters", "character_inventory", "idx_bag_slot", int64(2), "slot", int64(1), "BTREE", int64(800)).
		AddRow("acore_characters", "character_inventory", "idx_dup_a", int64(1), "item", int64(1), "BTREE", int64(200)).
		AddRow("acore_characters", "character_inventory", "idx_dup_b", int64(1), "item", int64(1), "BTREE", int64(200)).
		// creature — unique index uq_name is a prefix of idx_name_zone but must survive.
		AddRow("acore_world", "creature", "PRIMARY", int64(1), "guid", int64(0), "BTREE", int64(5000)).
		AddRow("acore_world", "creature", "idx_name_zone", int64(1), "name", int64(1), "BTREE", int64(40)).
		AddRow("acore_world", "creature", "idx_name_zone", int64(2), "zone", int64(1), "BTREE", int64(4000)).
		AddRow("acore_world", "creature", "uq_name", int64(1), "name", int64(0), "BTREE", int64(5000))

	mock.ExpectQuery(regexp.QuoteMeta("FROM information_schema.STATISTICS")).
		WithArgs("acore_world", "acore_characters", "acore_auth").
		WillReturnRows(rows)

	reg := NewRegistry()
	RegisterDBRedundantIndexesTool(reg, DBDeps{QueryDB: db})
	tool, _ := reg.Get("db_redundant_indexes")
	resp := tool.Handler(context.Background(), json.RawMessage(`{}`), "")
	got, ok := resp.(map[string]any)
	if !ok {
		t.Fatalf("unexpected response shape: %T", resp)
	}
	if msg, hasErr := got["error"]; hasErr {
		t.Fatalf("unexpected error: %v", msg)
	}

	cands := got["candidates"].([]redundantIndexCandidate)
	if len(cands) != 2 {
		t.Fatalf("expected 2 candidates, got %d: %+v", len(cands), cands)
	}
	// Sorted by database, table, redundant index name: idx_bag then idx_dup_b.
	c0 := cands[0]
	if c0.Database != "acore_characters" || c0.Table != "character_inventory" ||
		c0.RedundantIndex != "idx_bag" || c0.Reason != "prefix" ||
		c0.DominantIndex != "idx_bag_slot" || c0.CardinalityEstimate != 50 {
		t.Errorf("candidate[0] wrong: %+v", c0)
	}
	if len(c0.RedundantColumns) != 1 || c0.RedundantColumns[0] != "bag" {
		t.Errorf("candidate[0] redundantColumns: %v", c0.RedundantColumns)
	}
	if len(c0.DominantColumns) != 2 || c0.DominantColumns[0] != "bag" || c0.DominantColumns[1] != "slot" {
		t.Errorf("candidate[0] dominantColumns: %v", c0.DominantColumns)
	}
	if c0.RedundantUnique || c0.DominantUnique || c0.IndexType != "BTREE" {
		t.Errorf("candidate[0] facets: %+v", c0)
	}
	c1 := cands[1]
	if c1.RedundantIndex != "idx_dup_b" || c1.Reason != "duplicate" ||
		c1.DominantIndex != "idx_dup_a" || c1.CardinalityEstimate != 200 {
		t.Errorf("candidate[1] wrong: %+v", c1)
	}

	perDB := got["perDatabase"].([]redundantIndexPerDB)
	if len(perDB) != 3 {
		t.Fatalf("expected 3 perDatabase entries, got %d", len(perDB))
	}
	// redundantCount desc → acore_characters(2) leads; ties schema-asc → auth before world.
	if perDB[0].Database != "acore_characters" || perDB[0].TablesScanned != 1 ||
		perDB[0].IndexesScanned != 5 || perDB[0].RedundantCount != 2 {
		t.Errorf("perDB[0]: %+v", perDB[0])
	}
	if perDB[1].Database != "acore_auth" || perDB[1].IndexesScanned != 0 || perDB[1].RedundantCount != 0 {
		t.Errorf("perDB[1] (empty acore_auth): %+v", perDB[1])
	}
	if perDB[2].Database != "acore_world" || perDB[2].TablesScanned != 1 ||
		perDB[2].IndexesScanned != 3 || perDB[2].RedundantCount != 0 {
		t.Errorf("perDB[2]: %+v", perDB[2])
	}

	totals := got["totals"].(map[string]any)
	if totals["tablesScanned"].(int) != 2 || totals["indexesScanned"].(int) != 8 || totals["redundantCount"].(int) != 2 {
		t.Errorf("totals: %+v", totals)
	}
	if got["truncated"].(bool) {
		t.Errorf("truncated should be false")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestDBRedundantIndexes_DatabaseFilterNarrowsScan verifies the `database` arg
// narrows the IN clause to a single bind and echoes the schema back.
func TestDBRedundantIndexes_DatabaseFilterNarrowsScan(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectQuery(regexp.QuoteMeta("FROM information_schema.STATISTICS")).
		WithArgs("acore_characters"). // single bind — IN narrowed to one schema
		WillReturnRows(sqlmock.NewRows(redundantStatsColumns()).
			AddRow("acore_characters", "guild_member", "idx_a", int64(1), "rank", int64(1), "BTREE", int64(5)).
			AddRow("acore_characters", "guild_member", "idx_b", int64(1), "rank", int64(1), "BTREE", int64(5)))

	reg := NewRegistry()
	RegisterDBRedundantIndexesTool(reg, DBDeps{QueryDB: db})
	tool, _ := reg.Get("db_redundant_indexes")
	resp := tool.Handler(context.Background(), json.RawMessage(`{"database":"acore_characters"}`), "")
	got := resp.(map[string]any)
	if got["database"].(string) != "acore_characters" {
		t.Errorf("database echo: %v", got["database"])
	}
	scanned := got["scannedSchemas"].([]string)
	if len(scanned) != 1 || scanned[0] != "acore_characters" {
		t.Errorf("scannedSchemas: %v", scanned)
	}
	perDB := got["perDatabase"].([]redundantIndexPerDB)
	if len(perDB) != 1 || perDB[0].Database != "acore_characters" {
		t.Fatalf("expected single-schema perDatabase, got %+v", perDB)
	}
	// idx_b is the later-named duplicate → flagged; idx_a kept.
	cands := got["candidates"].([]redundantIndexCandidate)
	if len(cands) != 1 || cands[0].RedundantIndex != "idx_b" || cands[0].DominantIndex != "idx_a" {
		t.Errorf("expected idx_b redundant w.r.t idx_a, got %+v", cands)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestDBRedundantIndexes_EmptyResultStillSurfaces — zero STATISTICS rows must
// still return a non-nil candidates slice and a zero-rollup entry per scanned
// schema (catches the silent-absence-looks-like-a-bug regression).
func TestDBRedundantIndexes_EmptyResultStillSurfaces(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectQuery(regexp.QuoteMeta("FROM information_schema.STATISTICS")).
		WithArgs("acore_world", "acore_characters", "acore_auth").
		WillReturnRows(sqlmock.NewRows(redundantStatsColumns()))

	reg := NewRegistry()
	RegisterDBRedundantIndexesTool(reg, DBDeps{QueryDB: db})
	tool, _ := reg.Get("db_redundant_indexes")
	resp := tool.Handler(context.Background(), json.RawMessage(`{}`), "")
	got := resp.(map[string]any)
	cands := got["candidates"].([]redundantIndexCandidate)
	if cands == nil {
		t.Errorf("candidates should be a non-nil empty slice")
	}
	if len(cands) != 0 {
		t.Errorf("expected 0 candidates, got %d", len(cands))
	}
	perDB := got["perDatabase"].([]redundantIndexPerDB)
	if len(perDB) != 3 {
		t.Errorf("expected 3 perDatabase entries, got %d", len(perDB))
	}
	totals := got["totals"].(map[string]any)
	if totals["redundantCount"].(int) != 0 || totals["indexesScanned"].(int) != 0 {
		t.Errorf("totals should be zeroed: %+v", totals)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

func TestColumnsArePrefix(t *testing.T) {
	cases := []struct {
		short, long []string
		want        bool
	}{
		{[]string{"a"}, []string{"a", "b"}, true},       // strict prefix
		{[]string{"a", "b"}, []string{"a", "b"}, true},  // identical
		{[]string{"a", "b"}, []string{"a"}, false},      // longer than long
		{[]string{"b"}, []string{"a", "b"}, false},      // first col differs
		{[]string{"a", "c"}, []string{"a", "b"}, false}, // second col differs
		{[]string{}, []string{"a"}, true},               // empty prefix of anything
	}
	for i, c := range cases {
		if got := columnsArePrefix(c.short, c.long); got != c.want {
			t.Errorf("case %d columnsArePrefix(%v,%v)=%v want %v", i, c.short, c.long, got, c.want)
		}
	}
}

func TestIndexKeeperPreferred(t *testing.T) {
	uniqA := indexMeta{Name: "a", Unique: true}
	nonUniqA := indexMeta{Name: "a", Unique: false}
	nonUniqB := indexMeta{Name: "b", Unique: false}
	primary := indexMeta{Name: "PRIMARY", Unique: true}
	// UNIQUE beats non-unique regardless of name.
	if !indexKeeperPreferred(uniqA, nonUniqB) {
		t.Errorf("unique keeper should win over non-unique")
	}
	if indexKeeperPreferred(nonUniqB, uniqA) {
		t.Errorf("non-unique should not win over unique")
	}
	// PRIMARY beats a named index at equal uniqueness.
	if !indexKeeperPreferred(primary, uniqA) {
		t.Errorf("PRIMARY should win over a named unique index")
	}
	// Same uniqueness, neither PRIMARY → name asc wins.
	if !indexKeeperPreferred(nonUniqA, nonUniqB) {
		t.Errorf("name-asc keeper a should win over b")
	}
	if indexKeeperPreferred(nonUniqB, nonUniqA) {
		t.Errorf("name-desc keeper b should not win over a")
	}
}

func TestIndexRedundantTo(t *testing.T) {
	bt := "BTREE"
	mk := func(name string, unique bool, cols ...string) indexMeta {
		return indexMeta{Name: name, Unique: unique, Type: bt, Columns: cols}
	}
	cases := []struct {
		name string
		r, d indexMeta
		want bool
	}{
		{"non-unique prefix is redundant", mk("idx_a", false, "a"), mk("idx_ab", false, "a", "b"), true},
		{"longer is not redundant against its own prefix", mk("idx_ab", false, "a", "b"), mk("idx_a", false, "a"), false},
		{"PRIMARY never redundant", mk("PRIMARY", true, "a"), mk("idx_a", false, "a"), false},
		{"unique strict-prefix preserves constraint", mk("uq_a", true, "a"), mk("idx_ab", false, "a", "b"), false},
		{"unique covered by non-unique dup is not redundant", mk("uq_a", true, "a"), mk("idx_a", false, "a"), false},
		{"non-unique dup of unique IS redundant (keep the unique)", mk("idx_a", false, "a"), mk("uq_a", true, "a"), true},
		{"identical non-unique: later name redundant", mk("idx_b", false, "x"), mk("idx_a", false, "x"), true},
		{"identical non-unique: earlier name kept", mk("idx_a", false, "x"), mk("idx_b", false, "x"), false},
		{"type mismatch not comparable", mk("ft", false, "a"), indexMeta{Name: "idx_a", Type: "FULLTEXT", Columns: []string{"a"}}, false},
		{"same index not self-redundant", mk("idx_a", false, "a"), mk("idx_a", false, "a"), false},
		{"disjoint columns not redundant", mk("idx_a", false, "a"), mk("idx_b", false, "b"), false},
	}
	for _, c := range cases {
		if got := indexRedundantTo(c.r, c.d); got != c.want {
			t.Errorf("%s: indexRedundantTo=%v want %v", c.name, got, c.want)
		}
	}
}

// TestBestDominantIndex — among multiple covering indexes, the longest wins,
// tiebroken by name asc.
func TestBestDominantIndex(t *testing.T) {
	r := &indexMeta{Name: "idx_a", Type: "BTREE", Columns: []string{"a"}}
	ab := &indexMeta{Name: "idx_ab", Type: "BTREE", Columns: []string{"a", "b"}}
	abc := &indexMeta{Name: "idx_abc", Type: "BTREE", Columns: []string{"a", "b", "c"}}
	got := bestDominantIndex(r, []*indexMeta{r, ab, abc})
	if got == nil || got.Name != "idx_abc" {
		t.Fatalf("expected longest dominant idx_abc, got %+v", got)
	}
	// Two equal-length dominants → name asc.
	abx := &indexMeta{Name: "idx_abx", Type: "BTREE", Columns: []string{"a", "z"}}
	_ = abx // (a,z) doesn't cover (a)? it does: prefix (a) matches → both ab and abx cover r at len 2
	got2 := bestDominantIndex(r, []*indexMeta{r, abx, ab})
	if got2 == nil || got2.Name != "idx_ab" {
		t.Fatalf("expected name-asc tiebreak idx_ab, got %+v", got2)
	}
	// No dominant → nil.
	lone := &indexMeta{Name: "idx_solo", Type: "BTREE", Columns: []string{"q"}}
	if got3 := bestDominantIndex(lone, []*indexMeta{lone}); got3 != nil {
		t.Fatalf("expected nil for no dominant, got %+v", got3)
	}
}
