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
	dbOverIndexedTablesTimeout       = 10 * time.Second
	dbOverIndexedTablesMaxCandidates = 500
	// dbOverIndexedDefaultMinIndexes is the default report floor AND the
	// medium/high band boundary: a table with this many secondary (non-PRIMARY)
	// indexes is worth surfacing. Heuristic, not a hard engine limit (unlike the
	// 767-byte cap in db_index_key_length_audit) — tunable via `minIndexes`.
	dbOverIndexedDefaultMinIndexes = 5
	// dbOverIndexedHighThreshold promotes a table to `high` severity: 8+ secondary
	// indexes is heavy write amplification on a hot table.
	dbOverIndexedHighThreshold = 8
)

// overIndexedTableRow is one base table carrying enough secondary (non-PRIMARY)
// indexes to be a write-amplification concern: every INSERT/UPDATE/DELETE must
// maintain each secondary index, and each secondary index additionally stores a
// copy of the PRIMARY KEY (InnoDB). `IndexCount` includes PRIMARY;
// `SecondaryIndexCount` (the flag driver) excludes it. `IndexToDataPercent` is
// INDEX_LENGTH*100/DATA_LENGTH (-1 when DATA_LENGTH is 0 — empty/unknown) as a
// storage-side corroboration of the count. `RowsEstimate` is
// information_schema.TABLES' TABLE_ROWS (an InnoDB optimizer estimate, not
// COUNT(*)).
type overIndexedTableRow struct {
	Database            string `json:"database"`
	Table               string `json:"table"`
	Engine              string `json:"engine,omitempty"`
	RowsEstimate        int64  `json:"rowsEstimate"`
	IndexCount          int    `json:"indexCount"`
	SecondaryIndexCount int    `json:"secondaryIndexCount"`
	IndexBytes          int64  `json:"indexBytes"`
	IndexSizeHuman      string `json:"indexSizeHuman"`
	DataBytes           int64  `json:"dataBytes"`
	DataSizeHuman       string `json:"dataSizeHuman"`
	IndexToDataPercent  int64  `json:"indexToDataPercent"`
	Severity            string `json:"severity"`
	Reason              string `json:"reason"`
}

// overIndexedPerDatabase is the per-schema rollup. `TablesScanned` is the
// denominator (every base table in the schema); `OverIndexedCount` is how many
// reported candidates (post-minIndexes filter) it contributed. Pre-seeded for
// all scanned schemas so an empty / no-finding schema still surfaces with
// zeroes (same silent-absence guard as db_size_summary / db_unindexed_tables).
type overIndexedPerDatabase struct {
	Database         string `json:"database"`
	TablesScanned    int    `json:"tablesScanned"`
	OverIndexedCount int    `json:"overIndexedCount"`
}

// RegisterDBOverIndexedTablesTool registers `db_over_indexed_tables` — the
// too-MANY-indexes corner of the db_* index-count trilogy, completing the set
// with db_unindexed_tables (ZERO indexes / no PRIMARY KEY) and
// db_redundant_indexes (#246 — prefix-DUPLICATE droppable indexes). Where those flag "add a PRIMARY KEY" and
// "drop a redundant index", this flags the write-side cost of carrying many
// secondary indexes: every row change maintains all of them, and each stores a
// copy of the PRIMARY KEY. High-signal on the hot acore_characters write tables
// and on module-created tables (mod-playerbots / mod-ollama-chat) the operator
// actually owns and can prune.
//
// One LEFT JOIN of information_schema.TABLES against information_schema.
// STATISTICS, GROUP BY table with two COUNT(DISTINCT INDEX_NAME) aggregates
// (all indexes vs non-PRIMARY) — same ~1-row-per-table volume and direct-pool,
// fully-qualified, no-USE path as db_unindexed_tables. Filtered and rolled up
// in Go.
func RegisterDBOverIndexedTablesTool(reg *Registry, deps DBDeps) {
	reg.Register(Tool{
		Name: "db_over_indexed_tables",
		Description: "Find base tables carrying MANY secondary (non-PRIMARY) " +
			"indexes across acore_world / acore_characters / acore_auth — the " +
			"too-many-indexes corner of the index-count trilogy with " +
			"db_unindexed_tables (zero indexes / no PRIMARY KEY) and " +
			"db_redundant_indexes (droppable prefix-duplicate indexes). Every " +
			"secondary index must be maintained on every INSERT/UPDATE/DELETE " +
			"(write amplification), and each secondary index also stores a copy of " +
			"the PRIMARY KEY (InnoDB), so a table with 8+ of them pays a real " +
			"write + storage cost. One LEFT JOIN of information_schema.TABLES " +
			"against information_schema.STATISTICS, GROUP BY table with two " +
			"COUNT(DISTINCT INDEX_NAME) aggregates (same direct-pool, " +
			"fully-qualified, no-USE path as db_unindexed_tables). Returns " +
			"`candidates` (each {database, table, engine, rowsEstimate, " +
			"indexCount, secondaryIndexCount, indexBytes, dataBytes, " +
			"indexToDataPercent, severity, reason}, sorted worst-first: severity " +
			"then secondaryIndexCount desc), `perDatabase` ({database, " +
			"tablesScanned, overIndexedCount} — pre-seeded so every scanned schema " +
			"surfaces), and `totals`. Severity is heuristic (NOT a hard engine " +
			"limit): `high` at 8+ secondary indexes, `medium` at 5-7, `low` below " +
			"5 (only shown when you lower `minIndexes`). indexToDataPercent is " +
			"INDEX_LENGTH*100/DATA_LENGTH (-1 when the table has no data) — a " +
			"storage-side corroboration; TABLE_ROWS is an InnoDB estimate, not " +
			"COUNT(*), surfaced as rowsEstimate. `database` filters to one schema " +
			"(acore_world/acore_characters/acore_auth); `minIndexes` is the report " +
			"floor on secondaryIndexCount (default 5; values <1 reset to the " +
			"default). Pair with db_redundant_indexes / db_low_cardinality_indexes " +
			"to decide which of the many indexes can actually be dropped. " +
			"acore_playerbots is intentionally OUT of scope — ops_ro lacks SELECT " +
			"there (see ops_grant_playerbots_ro). Read-only, ops_ro pool, 10 s " +
			"timeout.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{
			"database":{"type":"string","description":"Filter to one schema (acore_world/acore_characters/acore_auth). Omit to scan all three.","enum":["acore_world","acore_characters","acore_auth"]},
			"minIndexes":{"type":"integer","description":"Only report tables whose secondary (non-PRIMARY) index count is >= this (default 5). Lower it to surface medium/low-index tables; values <1 reset to the default."}
		}}`),
		Annotations: AnnRead(),
		Handler: func(ctx context.Context, raw json.RawMessage, _ string) any {
			var a struct {
				Database   string `json:"database"`
				MinIndexes int    `json:"minIndexes"`
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
			if a.MinIndexes < 1 {
				a.MinIndexes = dbOverIndexedDefaultMinIndexes
			}
			c, cancel := context.WithTimeout(ctx, dbOverIndexedTablesTimeout)
			defer cancel()
			out, err := collectDBOverIndexedTables(c, deps.QueryDB, schemas, time.Now(), a.MinIndexes)
			if err != nil {
				return map[string]any{"error": err.Error()}
			}
			return out
		},
	})
}

// classifyOverIndexed returns the severity + human reason for a table with the
// given secondary-index count. Pure, so the heuristic banding is unit-tested in
// isolation. Bands: high >= dbOverIndexedHighThreshold (8), medium >=
// dbOverIndexedDefaultMinIndexes (5), else low (only reachable when the caller
// lowers minIndexes below the default).
func classifyOverIndexed(secondary int) (severity, reason string) {
	switch {
	case secondary >= dbOverIndexedHighThreshold:
		return "high", fmt.Sprintf("%d secondary indexes — heavy write amplification: every INSERT/UPDATE/DELETE maintains all of them, and each stores a copy of the PRIMARY KEY. Review with db_redundant_indexes / db_low_cardinality_indexes for droppable ones", secondary)
	case secondary >= dbOverIndexedDefaultMinIndexes:
		return "medium", fmt.Sprintf("%d secondary indexes — moderate write amplification; every row change updates each one. Check db_redundant_indexes for prefix-duplicate indexes that can be dropped", secondary)
	default:
		return "low", fmt.Sprintf("%d secondary indexes — below the default over-indexing threshold; reported because minIndexes was lowered", secondary)
	}
}

// overIndexedSeverityRank orders severities for the worst-first candidate sort:
// high < medium < low. Unknown severities sort last (defensive).
func overIndexedSeverityRank(s string) int {
	switch s {
	case "high":
		return 0
	case "medium":
		return 1
	case "low":
		return 2
	default:
		return 3
	}
}

// collectDBOverIndexedTables issues one parameterized LEFT JOIN of
// information_schema.TABLES against information_schema.STATISTICS, scoped to
// TABLE_TYPE='BASE TABLE'. The GROUP BY collapses the per-index-column
// STATISTICS rows into one row per table; COUNT(DISTINCT INDEX_NAME) folds the
// total index count and a CASE-guarded COUNT(DISTINCT ...) folds the
// non-PRIMARY count server-side. A table with zero indexes still produces one
// row (the LEFT JOIN's NULL index side → both counts 0), so it is scanned (and
// counted in the denominator) even though it can never meet the threshold. The
// IN clause is built from ? placeholders — no string interpolation of caller
// input. Filtering + rollups happen in Go.
func collectDBOverIndexedTables(ctx context.Context, db *sql.DB, schemas []string, now time.Time, minIndexes int) (map[string]any, error) {
	if len(schemas) == 0 {
		return nil, fmt.Errorf("no schemas specified")
	}
	placeholders := make([]string, len(schemas))
	args := make([]any, len(schemas))
	for i, s := range schemas {
		placeholders[i] = "?"
		args[i] = s
	}
	q := `SELECT t.TABLE_SCHEMA, t.TABLE_NAME, IFNULL(t.ENGINE,''),
			IFNULL(t.TABLE_ROWS, 0),
			IFNULL(t.INDEX_LENGTH, 0),
			IFNULL(t.DATA_LENGTH, 0),
			COUNT(DISTINCT s.INDEX_NAME),
			COUNT(DISTINCT CASE WHEN s.INDEX_NAME <> 'PRIMARY' THEN s.INDEX_NAME END)
		FROM information_schema.TABLES t
		LEFT JOIN information_schema.STATISTICS s
		  ON s.TABLE_SCHEMA = t.TABLE_SCHEMA AND s.TABLE_NAME = t.TABLE_NAME
		WHERE t.TABLE_TYPE = 'BASE TABLE'
		  AND t.TABLE_SCHEMA IN (` + strings.Join(placeholders, ",") + `)
		GROUP BY t.TABLE_SCHEMA, t.TABLE_NAME, t.ENGINE, t.TABLE_ROWS, t.INDEX_LENGTH, t.DATA_LENGTH
		ORDER BY t.TABLE_SCHEMA, t.TABLE_NAME`
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("query information_schema over-indexed-tables join: %w", err)
	}
	defer rows.Close()

	// Pre-seed per-schema rollup so a schema with no findings (or that ops_ro
	// can't see) still appears with zeroes rather than silently vanishing.
	byDB := make(map[string]*overIndexedPerDatabase, len(schemas))
	for _, s := range schemas {
		byDB[s] = &overIndexedPerDatabase{Database: s}
	}
	candidates := make([]overIndexedTableRow, 0, 16)
	totalScanned := 0
	for rows.Next() {
		var (
			schema, name, engine  string
			rowsEst, idxBytes     int64
			dataBytes             int64
			indexCount, secondary int
		)
		if err := rows.Scan(&schema, &name, &engine, &rowsEst, &idxBytes, &dataBytes, &indexCount, &secondary); err != nil {
			return nil, fmt.Errorf("scan over-indexed-tables row: %w", err)
		}
		totalScanned++
		d := byDB[schema]
		if d != nil {
			d.TablesScanned++
		}
		if secondary < minIndexes {
			continue // not over-indexed by the caller's threshold
		}
		pct := int64(-1)
		if dataBytes > 0 {
			pct = idxBytes * 100 / dataBytes
		}
		sev, reason := classifyOverIndexed(secondary)
		candidates = append(candidates, overIndexedTableRow{
			Database:            schema,
			Table:               name,
			Engine:              engine,
			RowsEstimate:        rowsEst,
			IndexCount:          indexCount,
			SecondaryIndexCount: secondary,
			IndexBytes:          idxBytes,
			IndexSizeHuman:      humanBytes(uint64Of(idxBytes)),
			DataBytes:           dataBytes,
			DataSizeHuman:       humanBytes(uint64Of(dataBytes)),
			IndexToDataPercent:  pct,
			Severity:            sev,
			Reason:              reason,
		})
		if d != nil {
			d.OverIndexedCount++
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate over-indexed-tables rows: %w", err)
	}

	// Worst-first: severity (high before medium before low), then most indexes
	// (secondaryIndexCount desc), then biggest blast radius (rowsEstimate desc,
	// indexBytes desc), then database+table asc so ties stay deterministic
	// across runs (Go map iteration is randomized).
	sort.Slice(candidates, func(i, j int) bool {
		ri, rj := overIndexedSeverityRank(candidates[i].Severity), overIndexedSeverityRank(candidates[j].Severity)
		if ri != rj {
			return ri < rj
		}
		if candidates[i].SecondaryIndexCount != candidates[j].SecondaryIndexCount {
			return candidates[i].SecondaryIndexCount > candidates[j].SecondaryIndexCount
		}
		if candidates[i].RowsEstimate != candidates[j].RowsEstimate {
			return candidates[i].RowsEstimate > candidates[j].RowsEstimate
		}
		if candidates[i].IndexBytes != candidates[j].IndexBytes {
			return candidates[i].IndexBytes > candidates[j].IndexBytes
		}
		if candidates[i].Database != candidates[j].Database {
			return candidates[i].Database < candidates[j].Database
		}
		return candidates[i].Table < candidates[j].Table
	})

	// Cross-schema total reflects ALL findings (pre-truncation) so the count is
	// honest even when the candidate list is capped for the MCP envelope.
	totalOverIndexed := 0
	for _, d := range byDB {
		totalOverIndexed += d.OverIndexedCount
	}

	truncated := false
	if len(candidates) > dbOverIndexedTablesMaxCandidates {
		candidates = candidates[:dbOverIndexedTablesMaxCandidates]
		truncated = true
	}

	// Per-database rollup, sorted overIndexedCount desc with schema-asc tiebreak.
	perDB := make([]overIndexedPerDatabase, 0, len(byDB))
	for _, d := range byDB {
		perDB = append(perDB, *d)
	}
	sort.Slice(perDB, func(i, j int) bool {
		if perDB[i].OverIndexedCount != perDB[j].OverIndexedCount {
			return perDB[i].OverIndexedCount > perDB[j].OverIndexedCount
		}
		return perDB[i].Database < perDB[j].Database
	})

	return map[string]any{
		"asOf":           now.UTC().Format(time.RFC3339),
		"scannedSchemas": schemas,
		"candidates":     candidates,
		"perDatabase":    perDB,
		"truncated":      truncated,
		"minIndexes":     minIndexes,
		"totals": map[string]any{
			"tablesScanned":    totalScanned,
			"overIndexedCount": totalOverIndexed,
		},
	}, nil
}
