package mcpserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

const (
	dbLowCardIndexesTimeout = 10 * time.Second

	// dbLowCardIndexesDefaultMaxSelectivity — flag a secondary index whose
	// CARDINALITY/TABLE_ROWS sits below 1%. Below this ratio a single key value
	// still matches a large slice of the table, so the optimizer almost always
	// prefers a full scan and the index just taxes every write.
	dbLowCardIndexesDefaultMaxSelectivity = 0.01

	// dbLowCardIndexesDefaultMinRows — selectivity is statistical noise on a
	// tiny table (a 5-row lookup table's every index looks "low cardinality"
	// yet dropping it reclaims nothing and changes no plan). Only evaluate
	// tables with at least this many estimated rows.
	dbLowCardIndexesDefaultMinRows = 1000

	// dbLowCardIndexesHighCutoffRatio — within the flagged set, selectivity at
	// or below maxSelectivity/10 is "high" severity (boolean/enum-grade: a
	// handful of distinct values, the optimizer never range-scans it).
	dbLowCardIndexesHighCutoffRatio = 0.1

	// dbLowCardIndexesMaxCandidates bounds the MCP envelope. AC's schema yields
	// only a handful of genuinely low-selectivity indexes, so this is a guard
	// rail, not an expected limit; totals stays the honest pre-cap count.
	dbLowCardIndexesMaxCandidates = 500
)

// lowCardIndexRow is one flagged secondary index. Every flagged index is a
// non-unique BTREE secondary (PRIMARY + UNIQUE + non-BTREE are filtered out in
// SQL — see the query comment), so there's no nonUnique field: it's always true
// by construction.
type lowCardIndexRow struct {
	Database                 string   `json:"database"`
	Table                    string   `json:"table"`
	Index                    string   `json:"index"`
	Columns                  []string `json:"columns"`
	IndexType                string   `json:"indexType"`
	Engine                   string   `json:"engine"`
	RowsEstimate             int64    `json:"rowsEstimate"`
	IndexCardinality         int64    `json:"indexCardinality"`
	LeadingColumnCardinality int64    `json:"leadingColumnCardinality"`
	Selectivity              float64  `json:"selectivity"`
	Severity                 string   `json:"severity"`
	Reason                   string   `json:"reason"`
}

// lowCardPerDatabase rolls up how many secondary indexes were evaluated in each
// scanned schema and how many were flagged. Pre-seeded for every scanned schema
// so an empty / no-grant schema still surfaces (operators can't mistake "schema
// scanned, nothing flagged" for "tool bug").
//
// The last three fields exist because the first two cannot, alone, be read
// honestly. IndexesScanned counts indexes the audit looked at, but a NULL
// CARDINALITY is unclassifiable, so folding those into the clean remainder made
// `indexesScanned: 600, lowCardinalityCount: 0` indistinguishable from "600
// indexes nobody has ever analyzed" — two readings that differ by everything an
// operator would do next. UnmeasuredCount is that subset of IndexesScanned.
// TablesSkippedByMinRows covers the other end: tables the row gate dropped
// BEFORE they were ever counted as scanned, so they appear in no count at all.
// TablesSkippedZeroRows is in turn the subset of those reporting zero rows —
// either genuinely empty or never analyzed — kept separate so this counter does
// not repeat, one level down, the very conflation UnmeasuredCount fixes.
type lowCardPerDatabase struct {
	Database               string `json:"database"`
	IndexesScanned         int    `json:"indexesScanned"`
	LowCardinalityCount    int    `json:"lowCardinalityCount"`
	UnmeasuredCount        int    `json:"unmeasuredCount"`
	TablesSkippedByMinRows int    `json:"tablesSkippedByMinRows"`
	TablesSkippedZeroRows  int    `json:"tablesSkippedZeroRows"`
}

// RegisterDBLowCardinalityIndexesTool registers `db_low_cardinality_indexes` —
// the index-SELECTIVITY lens of the db_* index-hygiene suite. `db_size_summary`
// finds index BYTES, `db_table_bloat` finds fragmentation, `db_redundant_indexes`
// finds duplicate/prefix indexes; this finds indexes that are structurally fine
// but too coarse to help reads (a single value matches a big slice of rows), so
// they only tax writes + buffer pool. Reuses the long-merged `dbSizeKnownSchemas`
// + `isKnownSizeSchema` allowlist (db_size_summary, #172).
func RegisterDBLowCardinalityIndexesTool(reg *Registry, deps DBDeps) {
	reg.Register(Tool{
		Name: "db_low_cardinality_indexes",
		Description: "Index-selectivity audit from information_schema.STATISTICS joined to " +
			"information_schema.TABLES, across acore_world / acore_characters / " +
			"acore_auth. Flags non-unique secondary BTREE indexes whose selectivity " +
			"(index CARDINALITY / table TABLE_ROWS) is below `maxSelectivity` " +
			"(default 0.01 = 1%) — a near-useless index the optimizer rarely picks " +
			"because any single key value still matches a large slice of the table, " +
			"so it mostly taxes every INSERT/UPDATE/DELETE and wastes buffer-pool. " +
			"The remaining index-hygiene lens after `db_size_summary` (index BYTES), " +
			"`db_table_bloat` (fragmentation) and `db_redundant_indexes` " +
			"(duplicate/prefix indexes): this one finds indexes that exist and are " +
			"structurally sound to keep but are too coarse to help reads. PRIMARY " +
			"keys and UNIQUE indexes are never flagged (PRIMARY is the clustered " +
			"key; a UNIQUE index is selectivity ~1.0 by definition AND enforces a " +
			"constraint, so a stale low estimate is unactionable); FULLTEXT/SPATIAL/" +
			"HASH are skipped (selectivity is a BTREE-range concept). For each " +
			"flagged index returns its ordered `columns`, `indexCardinality` " +
			"(distinct combinations of the full index — the highest-SEQ_IN_INDEX " +
			"estimate), `leadingColumnCardinality` (the first column alone), " +
			"`rowsEstimate`, `selectivity`, and a `severity` (high = selectivity " +
			"<= maxSelectivity/10, boolean/enum-grade; medium = below the threshold " +
			"but above that). CARDINALITY is the optimizer's last-ANALYZE estimate " +
			"(sampled for InnoDB), not exact — `reason` tells operators to confirm " +
			"with SHOW INDEX / ANALYZE TABLE before dropping. `minRows` (default " +
			"1000) skips tiny tables where selectivity is statistical noise; " +
			"`maxSelectivity` must be in (0,1] and is rejected (not clamped) when " +
			"out of range so a typo doesn't silently flag every index; `database` " +
			"narrows to one schema. acore_playerbots is intentionally OUT of scope " +
			"(ops_ro lacks SELECT there — blocked on `ops_grant_playerbots_ro`). " +
			"`totals` + `perDatabase` also report what this audit could NOT " +
			"evaluate, so a clean-looking result cannot be mistaken for a healthy " +
			"one: `unmeasuredCount` is the subset of `indexesScanned` whose " +
			"CARDINALITY was NULL (never analyzed, or an engine that does not " +
			"report it) and which could therefore be neither flagged nor cleared; " +
			"`tablesSkippedByMinRows` counts tables the `minRows` gate dropped " +
			"before they were scanned at all, and `tablesSkippedZeroRows` is the " +
			"subset of those whose row estimate is 0 (empty, or never analyzed). " +
			"A high `unmeasuredCount` alongside `lowCardinalityCount: 0` means " +
			"nothing could be judged, NOT that nothing is wrong; and because a " +
			"stale row estimate reads exactly like a small table, a bulk-imported " +
			"schema (ac-db-import reloads acore_world on every stack rebuild) is " +
			"under-reported at both ends at once. When either counter is non-zero, " +
			"run `db_index_stats_staleness` for the diagnosis and the ANALYZE " +
			"TABLE plan before trusting `lowCardinalityCount`. " +
			"Results sorted worst-first (severity, then selectivity asc, then " +
			"rowsEstimate desc), capped at 500 with `truncated`; `totals` + " +
			"`perDatabase` carry the honest pre-cap counts. Read-only, ops_ro pool, " +
			"10 s timeout.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{
			"database":{"type":"string","enum":["acore_world","acore_characters","acore_auth"],"description":"Limit to one schema (default: all three). acore_playerbots is out of scope (ops_ro lacks SELECT)."},
			"maxSelectivity":{"type":"number","description":"Flag indexes whose CARDINALITY/TABLE_ROWS is strictly below this (default 0.01 = 1%). Must be in (0, 1]; out-of-range is rejected, not clamped, so a typo doesn't flag every index."},
			"minRows":{"type":"integer","description":"Only evaluate tables with at least this many estimated rows — selectivity is noise on tiny tables (default 1000; pass <=0 to use default)."}
		}}`),
		Annotations: AnnRead(),
		Handler: func(ctx context.Context, raw json.RawMessage, _ string) any {
			var a struct {
				Database       string   `json:"database"`
				MaxSelectivity *float64 `json:"maxSelectivity"`
				MinRows        int64    `json:"minRows"`
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
			maxSel := dbLowCardIndexesDefaultMaxSelectivity
			if a.MaxSelectivity != nil {
				if *a.MaxSelectivity <= 0 || *a.MaxSelectivity > 1.0 {
					return map[string]any{"error": "maxSelectivity must be in (0, 1] (e.g. 0.01 = 1%)"}
				}
				maxSel = *a.MaxSelectivity
			}
			minRows := int64(dbLowCardIndexesDefaultMinRows)
			if a.MinRows > 0 {
				minRows = a.MinRows
			}
			c, cancel := context.WithTimeout(ctx, dbLowCardIndexesTimeout)
			defer cancel()
			out, err := collectLowCardinalityIndexes(c, deps.QueryDB, time.Now(), schemas, maxSel, minRows)
			if err != nil {
				return map[string]any{"error": err.Error()}
			}
			return out
		},
	})
}

// lowCardSelectivity returns cardinality/rows and whether it's computable. rows
// <= 0 (empty or never-analyzed table) is not computable — guard div-by-zero.
func lowCardSelectivity(cardinality, rows int64) (float64, bool) {
	if rows <= 0 {
		return 0, false
	}
	return float64(cardinality) / float64(rows), true
}

// classifyLowCardinality buckets a flagged index. high = selectivity at or below
// maxSelectivity/10 (boolean/enum-grade — essentially never range-scanned);
// medium = below the threshold but above that high cutoff.
func classifyLowCardinality(selectivity, maxSelectivity float64) string {
	if selectivity <= maxSelectivity*dbLowCardIndexesHighCutoffRatio {
		return "high"
	}
	return "medium"
}

// lowCardSeverityRank orders severities worst-first for the candidate sort.
func lowCardSeverityRank(sev string) int {
	switch sev {
	case "high":
		return 0
	case "medium":
		return 1
	default:
		return 2
	}
}

// roundSelectivity rounds to 6 decimal places so the JSON output is stable
// across runs/platforms (raw float64 division emits long mantissa tails).
func roundSelectivity(f float64) float64 {
	return math.Round(f*1e6) / 1e6
}

// collectLowCardinalityIndexes runs one parameterized pass over
// information_schema.STATISTICS joined to information_schema.TABLES (for the row
// estimate + engine), assembles per-column rows into ordered indexes in Go,
// computes selectivity, and flags the indexes below maxSelectivity. The IN
// clause is built from ? placeholders — no interpolation of caller input.
func collectLowCardinalityIndexes(ctx context.Context, db *sql.DB, now time.Time, schemas []string, maxSelectivity float64, minRows int64) (map[string]any, error) {
	if len(schemas) == 0 {
		return nil, fmt.Errorf("no schemas specified")
	}
	placeholders := make([]string, len(schemas))
	args := make([]any, len(schemas))
	for i, s := range schemas {
		placeholders[i] = "?"
		args[i] = s
	}
	// One pass over STATISTICS joined to TABLES. We deliberately exclude in SQL:
	//   - PRIMARY: the clustered key, not a droppable secondary index.
	//   - NON_UNIQUE=0 (UNIQUE): selectivity is ~1.0 by definition AND a unique
	//     index enforces a constraint, so a low estimate is never actionable.
	//   - non-BTREE (FULLTEXT/SPATIAL/HASH): selectivity is a B-tree range
	//     concept; those use different access paths.
	// Per-column STATISTICS rows are ORDER BY'd by SEQ_IN_INDEX so Go assembles
	// the ordered column list and reads the leading-column cardinality (seq 1)
	// + the full-index cardinality (highest seq) in one walk. CARDINALITY may be
	// NULL (engines/never-analyzed) → IFNULL(-1) sentinel, treated as "unknown,
	// scanned-but-not-flagged" below.
	q := `SELECT s.TABLE_SCHEMA, s.TABLE_NAME, s.INDEX_NAME, s.SEQ_IN_INDEX, s.COLUMN_NAME,
			IFNULL(s.CARDINALITY, -1), s.INDEX_TYPE, IFNULL(t.ENGINE, ''), IFNULL(t.TABLE_ROWS, 0)
		FROM information_schema.STATISTICS s
		JOIN information_schema.TABLES t
		  ON s.TABLE_SCHEMA = t.TABLE_SCHEMA AND s.TABLE_NAME = t.TABLE_NAME
		WHERE t.TABLE_TYPE = 'BASE TABLE'
		  AND s.NON_UNIQUE = 1
		  AND s.INDEX_NAME <> 'PRIMARY'
		  AND s.INDEX_TYPE = 'BTREE'
		  AND s.TABLE_SCHEMA IN (` + strings.Join(placeholders, ",") + `)
		ORDER BY s.TABLE_SCHEMA, s.TABLE_NAME, s.INDEX_NAME, s.SEQ_IN_INDEX`
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("query information_schema.STATISTICS: %w", err)
	}
	defer rows.Close()

	byDB := make(map[string]*lowCardPerDatabase, len(schemas))
	for _, s := range schemas {
		byDB[s] = &lowCardPerDatabase{Database: s}
	}

	candidates := make([]lowCardIndexRow, 0, 32)
	var totalScanned, totalLowCard, totalUnmeasured int
	var totalSkippedTables, totalSkippedZeroRows int

	// The minRows gate is a TABLE property but finalize() runs once per index
	// group, so a skipped table with four indexes would be counted four times.
	// Dedupe on (schema, table) to keep the counter in tables, as its name says.
	skippedTables := make(map[string]struct{}, 16)

	// Group accumulator. A group is one (schema, table, index); rows arrive
	// ordered so a group is a contiguous run, finalized when the key changes.
	var (
		curKey     string
		curSchema  string
		curTable   string
		curIndex   string
		curType    string
		curEngine  string
		curRows    int64
		curLeading int64
		curFull    int64
		curColumns []string
		haveGroup  bool
	)

	finalize := func() {
		if !haveGroup {
			return
		}
		// minRows gate at the table level — don't even count indexes on tiny
		// tables; selectivity there is statistical noise. Boundary is inclusive
		// (rows == minRows is evaluated).
		if curRows < minRows {
			// Record what the gate declined to look at. A skipped table lands
			// in NO other count, so without this the response cannot show that
			// it evaluated less than the schema contains — and a stale row
			// estimate reads exactly like a small table, so a bulk-imported
			// schema can be under-reported at both ends at once.
			// curRows <= 0 (TABLE_ROWS NULL is IFNULL'd to 0 in SQL) is the
			// never-analyzed / empty signature and is tracked as a subset.
			// Counting the subset only inside this branch keeps it a subset by
			// construction for any minRows, including 0 (nothing is skipped, so
			// both stay 0) — the invariant does not rely on the handler's
			// minRows >= 1 floor.
			tk := curSchema + "\x00" + curTable
			if _, seen := skippedTables[tk]; !seen {
				skippedTables[tk] = struct{}{}
				zeroRows := curRows <= 0
				totalSkippedTables++
				if zeroRows {
					totalSkippedZeroRows++
				}
				if d := byDB[curSchema]; d != nil {
					d.TablesSkippedByMinRows++
					if zeroRows {
						d.TablesSkippedZeroRows++
					}
				}
			}
			return
		}
		if d := byDB[curSchema]; d != nil {
			d.IndexesScanned++
		}
		totalScanned++
		// Unknown cardinality (NULL → -1): scanned (counted above) but can't be
		// classified, so leave it out of the flagged set — and count it, because
		// "not flagged" here means "unknowable", not "checked and fine". It is
		// deliberately NOT removed from IndexesScanned: that count is the
		// long-standing denominator and its existing regression assertions are
		// the proof this change stayed additive. `db_index_stats_staleness`
		// diagnoses the underlying condition and emits the ANALYZE TABLE plan.
		if curFull < 0 {
			if d := byDB[curSchema]; d != nil {
				d.UnmeasuredCount++
			}
			totalUnmeasured++
			return
		}
		sel, ok := lowCardSelectivity(curFull, curRows)
		if !ok {
			// Unreachable through the MCP handler, which floors minRows at 1 so
			// curRows >= 1 by the time we get here — but reachable by a direct
			// caller passing minRows <= 0. Counted rather than dropped silently
			// because it is the same unclassifiable state as a NULL cardinality,
			// and leaving it uncounted would quietly reintroduce this very bug
			// the moment that floor changes.
			if d := byDB[curSchema]; d != nil {
				d.UnmeasuredCount++
			}
			totalUnmeasured++
			return
		}
		if sel >= maxSelectivity {
			return
		}
		sev := classifyLowCardinality(sel, maxSelectivity)
		if d := byDB[curSchema]; d != nil {
			d.LowCardinalityCount++
		}
		totalLowCard++
		cols := make([]string, len(curColumns))
		copy(cols, curColumns)
		candidates = append(candidates, lowCardIndexRow{
			Database:                 curSchema,
			Table:                    curTable,
			Index:                    curIndex,
			Columns:                  cols,
			IndexType:                curType,
			Engine:                   curEngine,
			RowsEstimate:             curRows,
			IndexCardinality:         curFull,
			LeadingColumnCardinality: curLeading,
			Selectivity:              roundSelectivity(sel),
			Severity:                 sev,
			Reason: fmt.Sprintf(
				"non-unique secondary index spans ~%d distinct value(s) across ~%d rows (selectivity %.4f%%) — too unselective for the optimizer to prefer over a full scan; it mostly taxes writes and buffer-pool. Confirm with SHOW INDEX / ANALYZE TABLE before dropping.",
				curFull, curRows, sel*100),
		})
	}

	for rows.Next() {
		var (
			schema, table, index, column, idxType, engine string
			seq, cardinality, rowsEst                     int64
		)
		if err := rows.Scan(&schema, &table, &index, &seq, &column, &cardinality, &idxType, &engine, &rowsEst); err != nil {
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

	// Worst-first: severity (high before medium), then lower selectivity, then
	// bigger table (more wasted write overhead), then schema/table/index asc for
	// determinism (defends against Go map-iter flake on ties).
	sort.SliceStable(candidates, func(i, j int) bool {
		if ri, rj := lowCardSeverityRank(candidates[i].Severity), lowCardSeverityRank(candidates[j].Severity); ri != rj {
			return ri < rj
		}
		if candidates[i].Selectivity != candidates[j].Selectivity {
			return candidates[i].Selectivity < candidates[j].Selectivity
		}
		if candidates[i].RowsEstimate != candidates[j].RowsEstimate {
			return candidates[i].RowsEstimate > candidates[j].RowsEstimate
		}
		if candidates[i].Database != candidates[j].Database {
			return candidates[i].Database < candidates[j].Database
		}
		if candidates[i].Table != candidates[j].Table {
			return candidates[i].Table < candidates[j].Table
		}
		return candidates[i].Index < candidates[j].Index
	})

	truncated := false
	if len(candidates) > dbLowCardIndexesMaxCandidates {
		candidates = candidates[:dbLowCardIndexesMaxCandidates]
		truncated = true
	}

	perDB := make([]lowCardPerDatabase, 0, len(byDB))
	for _, d := range byDB {
		perDB = append(perDB, *d)
	}
	sort.Slice(perDB, func(i, j int) bool {
		if perDB[i].LowCardinalityCount != perDB[j].LowCardinalityCount {
			return perDB[i].LowCardinalityCount > perDB[j].LowCardinalityCount
		}
		return perDB[i].Database < perDB[j].Database
	})

	return map[string]any{
		"asOf":           now.UTC().Format(time.RFC3339),
		"scannedSchemas": schemas,
		"maxSelectivity": maxSelectivity,
		"minRows":        minRows,
		"candidates":     candidates,
		"perDatabase":    perDB,
		"truncated":      truncated,
		"totals": map[string]any{
			"indexesScanned":         totalScanned,
			"lowCardinalityCount":    totalLowCard,
			"unmeasuredCount":        totalUnmeasured,
			"tablesSkippedByMinRows": totalSkippedTables,
			"tablesSkippedZeroRows":  totalSkippedZeroRows,
		},
	}, nil
}
