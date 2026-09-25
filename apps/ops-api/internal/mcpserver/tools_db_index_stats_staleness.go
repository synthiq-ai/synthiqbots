package mcpserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

const (
	dbStatsStalenessTimeout = 10 * time.Second

	// dbStatsStalenessDefaultMaxPKDrift — both CARDINALITY and TABLE_ROWS are
	// sampled estimates, so a PRIMARY key's distinct count and its table's row
	// count never match to the unit even on freshly analyzed tables. Only flag a
	// shortfall bigger than 20%.
	dbStatsStalenessDefaultMaxPKDrift = 0.20

	// dbStatsStalenessHighRatio — within the magnitude-bearing classes
	// (exceeds_rows, primary_key_undercount), a disagreement of half the row
	// estimate or more is "high": at that size every ratio the index-hygiene
	// suite derives from these columns is wrong by a factor, not a rounding.
	dbStatsStalenessHighRatio = 0.5

	// dbStatsStalenessHighRowsFloor — the two absent-estimate classes (missing,
	// zero_cardinality) carry no magnitude of their own, so table size stands in
	// for impact: an uncollected estimate on a big table is what actually skews
	// the optimizer and the audit tools.
	dbStatsStalenessHighRowsFloor = 10000

	// dbStatsStalenessMaxFindings bounds the MCP envelope. A freshly imported
	// acore_world can legitimately produce thousands of never-analyzed indexes,
	// so unlike the sibling audits this cap is expected to bite; `totals` and
	// `perDatabase` stay the honest pre-cap counts.
	dbStatsStalenessMaxFindings = 500

	// dbStatsStalenessMaxAnalyzeStatements bounds the copy-paste remedy list.
	// Built from the full pre-cap finding set so the top tables still surface
	// when `findings` itself was truncated.
	dbStatsStalenessMaxAnalyzeStatements = 50
)

// Statistics-inconsistency classes. The first three are arithmetic
// impossibilities for EXACT values — a distinct-value count cannot be absent
// for a populated index, cannot be zero over a non-empty table, and cannot
// exceed the row count — so they need no threshold. Only the fourth is a
// judgement call against `maxPrimaryKeyDrift`.
const (
	statsClassMissing              = "missing"
	statsClassZeroCardinality      = "zero_cardinality"
	statsClassExceedsRows          = "exceeds_rows"
	statsClassPrimaryKeyUndercount = "primary_key_undercount"
)

// staleStatsFinding is one index whose optimizer statistics disagree with the
// table's own row estimate. IndexCardinality is -1 when information_schema
// reported NULL (the query's IFNULL sentinel) — a real CARDINALITY is never
// negative, so the sentinel is unambiguous.
type staleStatsFinding struct {
	Database                 string   `json:"database"`
	Table                    string   `json:"table"`
	Index                    string   `json:"index"`
	Columns                  []string `json:"columns"`
	IndexType                string   `json:"indexType"`
	Engine                   string   `json:"engine"`
	IsPrimary                bool     `json:"isPrimary"`
	NonUnique                bool     `json:"nonUnique"`
	RowsEstimate             int64    `json:"rowsEstimate"`
	IndexCardinality         int64    `json:"indexCardinality"`
	LeadingColumnCardinality int64    `json:"leadingColumnCardinality"`
	Class                    string   `json:"class"`
	Ratio                    float64  `json:"ratio"`
	Severity                 string   `json:"severity"`
	Reason                   string   `json:"reason"`
}

// staleStatsPerDatabase rolls up per scanned schema. Pre-seeded for every
// scanned schema so an empty / no-grant schema still surfaces with zero
// counters instead of vanishing (an absence would read as a tool bug).
type staleStatsPerDatabase struct {
	Database                  string `json:"database"`
	IndexesScanned            int    `json:"indexesScanned"`
	FlaggedCount              int    `json:"flaggedCount"`
	MissingCount              int    `json:"missingCount"`
	ZeroCardinalityCount      int    `json:"zeroCardinalityCount"`
	ExceedsRowsCount          int    `json:"exceedsRowsCount"`
	PrimaryKeyUndercountCount int    `json:"primaryKeyUndercountCount"`
}

// RegisterDBIndexStatsStalenessTool registers `db_index_stats_staleness` — the
// TRUSTWORTHINESS lens of the db_* index-hygiene suite. Every other member of
// that suite reads information_schema.STATISTICS.CARDINALITY and
// information_schema.TABLES.TABLE_ROWS as ground truth: `db_low_cardinality_indexes`
// divides one by the other, `db_redundant_indexes` compares cardinalities
// between index pairs. Neither can tell a genuinely coarse index from one whose
// estimate was simply never collected — `db_low_cardinality_indexes` maps a NULL
// CARDINALITY to its -1 sentinel and drops it from the flagged set, and its
// `minRows` gate skips any table whose row estimate reads low, which is exactly
// what a stale estimate looks like. This tool audits those two columns against
// each other so operators know whether the rest of the suite is standing on real
// numbers before acting on it.
func RegisterDBIndexStatsStalenessTool(reg *Registry, deps DBDeps) {
	reg.Register(Tool{
		Name: "db_index_stats_staleness",
		Description: "Optimizer-statistics freshness audit from information_schema.STATISTICS " +
			"joined to information_schema.TABLES, across acore_world / acore_characters / " +
			"acore_auth. Flags BTREE indexes whose CARDINALITY estimate is absent or " +
			"contradicts the table's own TABLE_ROWS estimate, in four `class`es: " +
			"`missing` (CARDINALITY is NULL on a non-empty table — never collected or " +
			"reset), `zero_cardinality` (CARDINALITY 0 over a non-empty table — an index " +
			"on at least one row has at least one distinct value), `exceeds_rows` " +
			"(CARDINALITY greater than TABLE_ROWS — a distinct-value count can never " +
			"exceed the row count, so the two estimates disagree in an impossible " +
			"direction) and `primary_key_undercount` (a PRIMARY key's CARDINALITY sits " +
			"more than `maxPrimaryKeyDrift` below TABLE_ROWS — a PRIMARY key is NOT NULL " +
			"and unique, so its true distinct count IS the row count and the two " +
			"estimates should agree). The first three need no threshold; they are " +
			"arithmetic impossibilities for exact values. This is the trust lens for the " +
			"rest of the index-hygiene suite rather than another finding of its own: " +
			"`db_low_cardinality_indexes` divides CARDINALITY by TABLE_ROWS and silently " +
			"skips indexes whose estimate is NULL, and `db_redundant_indexes` compares " +
			"cardinalities between index pairs — both read as confident output over " +
			"numbers that may never have been measured, which is the normal state of a " +
			"schema straight out of a bulk import. Remedy is ANALYZE TABLE; the response " +
			"carries a deduped `analyzeStatements` list for the affected tables (ops_ro " +
			"cannot run them — use db_exec with DB_EXEC_DSN, or the mysql CLI). Returns " +
			"per finding the ordered `columns`, `rowsEstimate`, `indexCardinality` (-1 " +
			"means information_schema reported NULL), `leadingColumnCardinality`, a " +
			"`ratio` (size of the disagreement relative to the row estimate; 0 for the " +
			"two absent-estimate classes) and a `severity` (high = ratio >= 0.5, or an " +
			"absent estimate on a table of 10000+ rows). PRIMARY and UNIQUE indexes are " +
			"deliberately INCLUDED here — unlike db_low_cardinality_indexes, which " +
			"excludes them because a low estimate there is unactionable; for staleness a " +
			"PRIMARY key is the single sharpest signal, since uniqueness pins what the " +
			"true answer must be. FULLTEXT/SPATIAL/HASH are skipped (a NULL cardinality " +
			"is normal for them). `minRows` defaults to 0 (evaluate every table) rather " +
			"than the sibling audits' 1000, because gating on the row estimate would " +
			"suppress precisely the tables whose row estimate is the thing in doubt. " +
			"`maxPrimaryKeyDrift` must be in (0,1] and is rejected, not clamped, when out " +
			"of range; `database` narrows to one schema. acore_playerbots is out of scope " +
			"(ops_ro lacks SELECT — blocked on `ops_grant_playerbots_ro`). Sorted " +
			"worst-first, capped at 500 with `truncated`; `totals` + `perDatabase` carry " +
			"the honest pre-cap counts. Read-only, ops_ro pool, 10 s timeout.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{
			"database":{"type":"string","enum":["acore_world","acore_characters","acore_auth"],"description":"Limit to one schema (default: all three). acore_playerbots is out of scope (ops_ro lacks SELECT)."},
			"maxPrimaryKeyDrift":{"type":"number","description":"Flag a PRIMARY key whose CARDINALITY is more than this fraction below TABLE_ROWS (default 0.20 = 20%). Both are sampled estimates so they never match exactly. Must be in (0, 1]; out-of-range is rejected, not clamped, so a typo doesn't flag every primary key."},
			"minRows":{"type":"integer","description":"Only evaluate tables with at least this many estimated rows (default 0 = evaluate all). Deliberately unlike db_low_cardinality_indexes' 1000 default: a stale table often reports a low row estimate, so a row-count gate hides the very tables this tool exists to find."}
		}}`),
		Annotations: AnnRead(),
		Handler: func(ctx context.Context, raw json.RawMessage, _ string) any {
			var a struct {
				Database           string   `json:"database"`
				MaxPrimaryKeyDrift *float64 `json:"maxPrimaryKeyDrift"`
				MinRows            int64    `json:"minRows"`
			}
			if err := json.Unmarshal(raw, &a); err != nil {
				return map[string]any{"error": "decode args: " + err.Error()}
			}
			if deps.QueryDB == nil {
				return map[string]any{"error": "ops_ro pool not configured (DB_DSN empty?)"}
			}
			schemas := dbSizeKnownSchemas
			if a.Database != "" {
				if !isKnownSizeSchema(a.Database) {
					return map[string]any{"error": "database must be one of acore_world/acore_characters/acore_auth"}
				}
				schemas = []string{a.Database}
			}
			maxDrift := dbStatsStalenessDefaultMaxPKDrift
			if a.MaxPrimaryKeyDrift != nil {
				if *a.MaxPrimaryKeyDrift <= 0 || *a.MaxPrimaryKeyDrift > 1.0 {
					return map[string]any{"error": "maxPrimaryKeyDrift must be in (0, 1] (e.g. 0.20 = 20%)"}
				}
				maxDrift = *a.MaxPrimaryKeyDrift
			}
			minRows := a.MinRows
			if minRows < 0 {
				minRows = 0
			}
			c, cancel := context.WithTimeout(ctx, dbStatsStalenessTimeout)
			defer cancel()
			out, err := collectIndexStatsStaleness(c, deps.QueryDB, time.Now(), schemas, maxDrift, minRows)
			if err != nil {
				return map[string]any{"error": err.Error()}
			}
			return out
		},
	})
}

// classifyStaleStats decides whether one index's statistics are internally
// inconsistent, returning the class, the magnitude of the disagreement relative
// to the row estimate, and whether it is flagged at all.
//
// Order is load-bearing. `cardinality > rows` is tested before the zero case so
// that a rows==0 / cardinality>0 pair reports exceeds_rows ("the estimate claims
// distinct values in a table the row estimate calls empty") rather than falling
// through; and the NULL sentinel is tested first because -1 would otherwise
// satisfy neither branch honestly.
//
// A table reporting no rows AND no usable estimate is not flagged: there is
// nothing for ANALYZE TABLE to discover, so surfacing it would be noise.
func classifyStaleStats(isPrimary bool, cardinality, rows int64, maxPKDrift float64) (string, float64, bool) {
	switch {
	case cardinality < 0:
		if rows <= 0 {
			return "", 0, false
		}
		return statsClassMissing, 0, true
	case cardinality > rows:
		// Relative to the row estimate; rows==0 would divide by zero, and the
		// disagreement is then simply the whole cardinality.
		denom := rows
		if denom <= 0 {
			denom = 1
		}
		return statsClassExceedsRows, float64(cardinality-rows) / float64(denom), true
	case cardinality == 0:
		if rows <= 0 {
			return "", 0, false
		}
		return statsClassZeroCardinality, 0, true
	case isPrimary && rows > 0:
		// Reached only when cardinality <= rows (the greater-than case is claimed
		// above), so this is always a shortfall, never an excess.
		shortfall := float64(rows-cardinality) / float64(rows)
		if shortfall > maxPKDrift {
			return statsClassPrimaryKeyUndercount, shortfall, true
		}
	}
	return "", 0, false
}

// staleStatsSeverity ranks a finding. The two absent-estimate classes carry no
// magnitude of their own, so table size stands in for impact; the other two are
// ranked by how far apart the estimates are.
func staleStatsSeverity(class string, ratio float64, rows int64) string {
	switch class {
	case statsClassMissing, statsClassZeroCardinality:
		if rows >= dbStatsStalenessHighRowsFloor {
			return "high"
		}
		return "medium"
	default:
		if ratio >= dbStatsStalenessHighRatio {
			return "high"
		}
		return "medium"
	}
}

// staleStatsSeverityRank orders severities worst-first for the sort.
func staleStatsSeverityRank(sev string) int {
	switch sev {
	case "high":
		return 0
	case "medium":
		return 1
	default:
		return 2
	}
}

// staleStatsClassRank breaks ties within a severity. The three arithmetic
// impossibilities come before the threshold-based drift class, because they are
// true regardless of how `maxPrimaryKeyDrift` was set.
func staleStatsClassRank(class string) int {
	switch class {
	case statsClassMissing:
		return 0
	case statsClassZeroCardinality:
		return 1
	case statsClassExceedsRows:
		return 2
	case statsClassPrimaryKeyUndercount:
		return 3
	default:
		return 4
	}
}

// staleStatsReason renders the operator-facing explanation for one finding.
func staleStatsReason(class string, cardinality, rows int64, ratio float64) string {
	switch class {
	case statsClassMissing:
		return fmt.Sprintf(
			"no CARDINALITY estimate (information_schema reported NULL) while the table reports ~%d row(s) — statistics were never collected for this index or were reset. Every db_* audit that divides CARDINALITY by TABLE_ROWS skips this index silently, so it is invisible rather than clean. Run ANALYZE TABLE to collect them.",
			rows)
	case statsClassZeroCardinality:
		return fmt.Sprintf(
			"CARDINALITY is 0 while the table reports ~%d row(s) — an index over a non-empty table has at least one distinct value, so 0 is a missing measurement rather than a measured result. Run ANALYZE TABLE to collect it.",
			rows)
	case statsClassExceedsRows:
		return fmt.Sprintf(
			"CARDINALITY (~%d distinct) exceeds the table row estimate (~%d) by %.2f%% — a distinct-value count can never exceed the row count, so the two estimates disagree in a direction that is impossible for exact values and at least one of them is stale. Run ANALYZE TABLE to re-sample both.",
			cardinality, rows, ratio*100)
	case statsClassPrimaryKeyUndercount:
		return fmt.Sprintf(
			"PRIMARY key CARDINALITY (~%d) sits %.2f%% below the table row estimate (~%d) — a PRIMARY key is NOT NULL and unique, so its true distinct count IS the row count and the two estimates should agree to within sampling noise. Run ANALYZE TABLE to re-sample both.",
			cardinality, ratio*100, rows)
	}
	return ""
}

// collectIndexStatsStaleness runs one parameterized pass over
// information_schema.STATISTICS joined to information_schema.TABLES, assembles
// per-column rows into ordered indexes in Go, and classifies each index's
// statistics. The IN clause is built from ? placeholders — no interpolation of
// caller input.
//
// The whole classification is folded in Go rather than expressed as SQL
// predicates on purpose: sqlmock string-matches the query without executing it,
// so logic pushed into SQL would be unverifiable by the deterministic tests,
// while logic in Go is genuinely exercised by them.
func collectIndexStatsStaleness(ctx context.Context, db *sql.DB, now time.Time, schemas []string, maxPKDrift float64, minRows int64) (map[string]any, error) {
	if len(schemas) == 0 {
		return nil, fmt.Errorf("no schemas specified")
	}
	placeholders := make([]string, len(schemas))
	args := make([]any, len(schemas))
	for i, s := range schemas {
		placeholders[i] = "?"
		args[i] = s
	}
	// Deliberately WITHOUT db_low_cardinality_indexes' `NON_UNIQUE = 1` and
	// `INDEX_NAME <> 'PRIMARY'` filters: for a staleness audit the PRIMARY key is
	// the sharpest signal available, because uniqueness pins what the true
	// distinct count must be. Non-BTREE indexes are still excluded — a NULL
	// CARDINALITY is normal for FULLTEXT/SPATIAL/HASH, so including them would
	// manufacture `missing` findings that no ANALYZE TABLE can fix.
	//
	// Per-column STATISTICS rows are ORDER BY'd by SEQ_IN_INDEX so one walk reads
	// the ordered column list, the leading-column cardinality (seq 1) and the
	// full-index cardinality (highest seq). CARDINALITY may be NULL →
	// IFNULL(-1) sentinel; a real cardinality is never negative.
	q := `SELECT s.TABLE_SCHEMA, s.TABLE_NAME, s.INDEX_NAME, s.SEQ_IN_INDEX, s.COLUMN_NAME,
			IFNULL(s.CARDINALITY, -1), s.INDEX_TYPE, s.NON_UNIQUE, IFNULL(t.ENGINE, ''), IFNULL(t.TABLE_ROWS, 0)
		FROM information_schema.STATISTICS s
		JOIN information_schema.TABLES t
		  ON s.TABLE_SCHEMA = t.TABLE_SCHEMA AND s.TABLE_NAME = t.TABLE_NAME
		WHERE t.TABLE_TYPE = 'BASE TABLE'
		  AND s.INDEX_TYPE = 'BTREE'
		  AND s.TABLE_SCHEMA IN (` + strings.Join(placeholders, ",") + `)
		ORDER BY s.TABLE_SCHEMA, s.TABLE_NAME, s.INDEX_NAME, s.SEQ_IN_INDEX`
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("query information_schema.STATISTICS: %w", err)
	}
	defer rows.Close()

	byDB := make(map[string]*staleStatsPerDatabase, len(schemas))
	for _, s := range schemas {
		byDB[s] = &staleStatsPerDatabase{Database: s}
	}

	findings := make([]staleStatsFinding, 0, 32)
	byClass := map[string]int{
		statsClassMissing:              0,
		statsClassZeroCardinality:      0,
		statsClassExceedsRows:          0,
		statsClassPrimaryKeyUndercount: 0,
	}
	var totalScanned, totalFlagged int

	// Group accumulator. A group is one (schema, table, index); rows arrive
	// ordered so a group is a contiguous run, finalized when the key changes.
	var (
		curKey       string
		curSchema    string
		curTable     string
		curIndex     string
		curType      string
		curEngine    string
		curNonUnique bool
		curRows      int64
		curLeading   int64
		curFull      int64
		curColumns   []string
		haveGroup    bool
	)

	finalize := func() {
		if !haveGroup {
			return
		}
		// minRows defaults to 0, so by default nothing is gated out here — see the
		// Description for why a row-count floor is the wrong default for this tool.
		if minRows > 0 && curRows < minRows {
			return
		}
		if d := byDB[curSchema]; d != nil {
			d.IndexesScanned++
		}
		totalScanned++

		isPrimary := curIndex == "PRIMARY"
		class, ratio, flagged := classifyStaleStats(isPrimary, curFull, curRows, maxPKDrift)
		if !flagged {
			return
		}
		sev := staleStatsSeverity(class, ratio, curRows)
		if d := byDB[curSchema]; d != nil {
			d.FlaggedCount++
			switch class {
			case statsClassMissing:
				d.MissingCount++
			case statsClassZeroCardinality:
				d.ZeroCardinalityCount++
			case statsClassExceedsRows:
				d.ExceedsRowsCount++
			case statsClassPrimaryKeyUndercount:
				d.PrimaryKeyUndercountCount++
			}
		}
		byClass[class]++
		totalFlagged++
		cols := make([]string, len(curColumns))
		copy(cols, curColumns)
		findings = append(findings, staleStatsFinding{
			Database:                 curSchema,
			Table:                    curTable,
			Index:                    curIndex,
			Columns:                  cols,
			IndexType:                curType,
			Engine:                   curEngine,
			IsPrimary:                isPrimary,
			NonUnique:                curNonUnique,
			RowsEstimate:             curRows,
			IndexCardinality:         curFull,
			LeadingColumnCardinality: curLeading,
			Class:                    class,
			// roundSelectivity (db_low_cardinality_indexes) is a general
			// round-a-ratio-to-6dp helper — reused rather than duplicated so both
			// tools emit ratios with the same stable precision.
			Ratio:    roundSelectivity(ratio),
			Severity: sev,
			Reason:   staleStatsReason(class, curFull, curRows, ratio),
		})
	}

	for rows.Next() {
		var (
			schema, table, index, column, idxType, engine string
			seq, cardinality, nonUnique, rowsEst          int64
		)
		if err := rows.Scan(&schema, &table, &index, &seq, &column, &cardinality, &idxType, &nonUnique, &engine, &rowsEst); err != nil {
			return nil, fmt.Errorf("scan STATISTICS row: %w", err)
		}
		key := schema + "\x00" + table + "\x00" + index
		if !haveGroup || key != curKey {
			finalize()
			curKey = key
			curSchema = schema
			curTable = table
			curIndex = index
			curType = idxType
			curEngine = engine
			curNonUnique = nonUnique == 1
			curRows = rowsEst
			curLeading = cardinality // first row of the group is SEQ_IN_INDEX=1
			curFull = cardinality
			curColumns = curColumns[:0]
			haveGroup = true
		}
		curColumns = append(curColumns, column)
		curFull = cardinality // highest seq seen wins → full-index cardinality
	}
	finalize()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate STATISTICS: %w", err)
	}

	// Worst-first: severity, then the impossibility classes before the
	// threshold-based one, then the bigger disagreement, then the bigger table
	// (more skew for everything downstream), then schema/table/index asc for
	// determinism (defends against Go map-iter flake on ties).
	sort.SliceStable(findings, func(i, j int) bool {
		if ri, rj := staleStatsSeverityRank(findings[i].Severity), staleStatsSeverityRank(findings[j].Severity); ri != rj {
			return ri < rj
		}
		if ri, rj := staleStatsClassRank(findings[i].Class), staleStatsClassRank(findings[j].Class); ri != rj {
			return ri < rj
		}
		if findings[i].Ratio != findings[j].Ratio {
			return findings[i].Ratio > findings[j].Ratio
		}
		if findings[i].RowsEstimate != findings[j].RowsEstimate {
			return findings[i].RowsEstimate > findings[j].RowsEstimate
		}
		if findings[i].Database != findings[j].Database {
			return findings[i].Database < findings[j].Database
		}
		if findings[i].Table != findings[j].Table {
			return findings[i].Table < findings[j].Table
		}
		return findings[i].Index < findings[j].Index
	})

	// Remedy list + affected-table census, both built from the FULL pre-cap
	// finding set so a truncated `findings` still yields the worst tables. The
	// remedy is per TABLE, not per index — one ANALYZE re-samples every index on
	// it — so dedupe by (schema, table) in worst-first order.
	seenTable := make(map[string]bool, len(findings))
	analyze := make([]string, 0, dbStatsStalenessMaxAnalyzeStatements)
	analyzeTruncated := false
	for _, f := range findings {
		tk := f.Database + "\x00" + f.Table
		if seenTable[tk] {
			continue
		}
		seenTable[tk] = true
		if len(analyze) >= dbStatsStalenessMaxAnalyzeStatements {
			analyzeTruncated = true
			continue
		}
		analyze = append(analyze, fmt.Sprintf("ANALYZE TABLE `%s`.`%s`;", f.Database, f.Table))
	}
	tablesAffected := len(seenTable)

	truncated := false
	if len(findings) > dbStatsStalenessMaxFindings {
		findings = findings[:dbStatsStalenessMaxFindings]
		truncated = true
	}

	perDB := make([]staleStatsPerDatabase, 0, len(byDB))
	for _, d := range byDB {
		perDB = append(perDB, *d)
	}
	sort.Slice(perDB, func(i, j int) bool {
		if perDB[i].FlaggedCount != perDB[j].FlaggedCount {
			return perDB[i].FlaggedCount > perDB[j].FlaggedCount
		}
		return perDB[i].Database < perDB[j].Database
	})

	return map[string]any{
		"asOf":                       now.UTC().Format(time.RFC3339),
		"scannedSchemas":             schemas,
		"maxPrimaryKeyDrift":         maxPKDrift,
		"minRows":                    minRows,
		"findings":                   findings,
		"perDatabase":                perDB,
		"truncated":                  truncated,
		"analyzeStatements":          analyze,
		"analyzeStatementsTruncated": analyzeTruncated,
		"totals": map[string]any{
			"indexesScanned": totalScanned,
			"flaggedCount":   totalFlagged,
			"tablesAffected": tablesAffected,
			"byClass":        byClass,
		},
	}, nil
}
