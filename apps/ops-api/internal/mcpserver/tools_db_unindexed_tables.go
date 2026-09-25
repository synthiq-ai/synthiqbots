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
	dbUnindexedTablesTimeout       = 10 * time.Second
	dbUnindexedTablesMaxCandidates = 500
)

// unindexedTableRow is one base table that lacks a PRIMARY KEY. `HasUniqueKey`
// records whether the table has at least one UNIQUE (non-PRIMARY) secondary
// index — InnoDB promotes the first all-NOT-NULL UNIQUE index to the clustered
// key, so a table with one is less severe than one InnoDB has to put on a
// hidden GEN_CLUST_INDEX rowid. `RowsEstimate` is information_schema.TABLES'
// TABLE_ROWS (an InnoDB optimizer estimate, not COUNT(*)).
type unindexedTableRow struct {
	Database       string `json:"database"`
	Table          string `json:"table"`
	Engine         string `json:"engine,omitempty"`
	RowsEstimate   int64  `json:"rowsEstimate"`
	TotalSizeBytes int64  `json:"totalSizeBytes"`
	TotalSizeHuman string `json:"totalSizeHuman"`
	HasUniqueKey   bool   `json:"hasUniqueKey"`
	Severity       string `json:"severity"`
	Reason         string `json:"reason"`
}

// unindexedPerDatabase is the per-schema rollup. `TablesScanned` is the
// denominator (every base table in the schema); `UnindexedCount` is how many
// reported candidates (post-minRows filter) it contributed. Pre-seeded for all
// scanned schemas so an empty / no-finding schema still surfaces with zeroes
// (same silent-absence guard as db_size_summary).
type unindexedPerDatabase struct {
	Database       string `json:"database"`
	TablesScanned  int    `json:"tablesScanned"`
	UnindexedCount int    `json:"unindexedCount"`
}

// RegisterDBUnindexedTablesTool registers `db_unindexed_tables` — the
// PRIMARY-KEY-presence half of the db_* index-hygiene suite (sibling to
// db_size_summary #172 / db_table_bloat #175 / db_redundant_indexes #246).
// db_redundant_indexes answers "which indexes can I DROP?"; this answers the
// inverse "which tables have NO PRIMARY KEY?" — an InnoDB table without a
// declared PRIMARY or all-NOT-NULL UNIQUE key is stored on a hidden 6-byte
// GEN_CLUST_INDEX rowid, which forces full scans for point lookups and makes
// row-based replication scan every row to locate a target. A schema-hygiene
// red flag that's invisible until something is slow.
//
// One LEFT JOIN of information_schema.TABLES against information_schema.
// STATISTICS, GROUP BY table (so the per-index-column rows collapse to one row
// per table — same ~1-row-per-table volume as db_size_summary), then filtered
// and rolled up in Go.
func RegisterDBUnindexedTablesTool(reg *Registry, deps DBDeps) {
	reg.Register(Tool{
		Name: "db_unindexed_tables",
		Description: "Find base tables with NO PRIMARY KEY across acore_world / " +
			"acore_characters / acore_auth — the inverse of db_redundant_indexes " +
			"(which finds droppable indexes). An InnoDB table without a declared " +
			"PRIMARY KEY (and without an all-NOT-NULL UNIQUE index InnoDB can " +
			"promote) is stored on a hidden 6-byte GEN_CLUST_INDEX rowid: point " +
			"lookups and row-based replication must full-scan, and the rowid is " +
			"not exposed to SQL. One LEFT JOIN of information_schema.TABLES " +
			"against information_schema.STATISTICS (same direct-pool, " +
			"fully-qualified, no-USE path as db_size_summary). Returns " +
			"`candidates` (each {database, table, engine, rowsEstimate, " +
			"totalSizeBytes, hasUniqueKey, severity, reason}, sorted worst-first: " +
			"severity then rowsEstimate desc), `perDatabase` ({database, " +
			"tablesScanned, unindexedCount} — pre-seeded so every scanned schema " +
			"surfaces), and `totals`. Severity is `high` when the table has " +
			"neither a PRIMARY nor any UNIQUE index (forced onto GEN_CLUST_INDEX), " +
			"`medium` when a UNIQUE index exists — InnoDB promotes the first " +
			"all-NOT-NULL UNIQUE index to the clustered key, but this tool does " +
			"NOT verify column nullability, so a `medium` table may already have " +
			"an effective clustered key (declare an explicit PRIMARY KEY anyway " +
			"for clarity and portability). TABLE_ROWS is an InnoDB estimate, not " +
			"COUNT(*) — surfaced as `rowsEstimate`. `database` filters to one " +
			"schema (acore_world/acore_characters/acore_auth); `minRows` " +
			"suppresses tables whose row estimate is below the threshold (default " +
			"0 = report all, so an unindexed empty staging table isn't hidden " +
			"unless you ask). acore_playerbots is intentionally OUT of scope — " +
			"ops_ro lacks SELECT there (see ops_grant_playerbots_ro). Read-only, " +
			"ops_ro pool, 10 s timeout.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{
			"database":{"type":"string","description":"Filter to one schema (acore_world/acore_characters/acore_auth). Omit to scan all three.","enum":["acore_world","acore_characters","acore_auth"]},
			"minRows":{"type":"integer","description":"Only report tables whose TABLE_ROWS estimate is >= this (default 0 = report all). Use to suppress tiny/empty tables where a missing PK is harmless."}
		}}`),
		Annotations: AnnRead(),
		Handler: func(ctx context.Context, raw json.RawMessage, _ string) any {
			var a struct {
				Database string `json:"database"`
				MinRows  int64  `json:"minRows"`
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
			if a.MinRows < 0 {
				a.MinRows = 0
			}
			c, cancel := context.WithTimeout(ctx, dbUnindexedTablesTimeout)
			defer cancel()
			out, err := collectDBUnindexedTables(c, deps.QueryDB, schemas, time.Now(), a.MinRows)
			if err != nil {
				return map[string]any{"error": err.Error()}
			}
			return out
		},
	})
}

// classifyUnindexed returns the severity + human reason for a table that has no
// PRIMARY KEY, given whether it has any UNIQUE (non-PRIMARY) index. Pure, so
// the InnoDB-clustered-key reasoning is unit-tested in isolation.
func classifyUnindexed(hasUnique bool) (severity, reason string) {
	if hasUnique {
		return "medium", "no PRIMARY KEY, but a UNIQUE index exists — InnoDB promotes the first all-NOT-NULL UNIQUE index to the clustered key (column nullability is NOT verified by this tool); declare an explicit PRIMARY KEY for clarity and cross-engine portability"
	}
	return "high", "no PRIMARY or UNIQUE key — InnoDB stores the table on a hidden 6-byte GEN_CLUST_INDEX rowid; point lookups and row-based replication must full-scan, and the rowid is not exposed to SQL"
}

// unindexedSeverityRank orders severities for the worst-first candidate sort:
// high before medium. Unknown severities sort last (defensive — current code
// only emits high/medium).
func unindexedSeverityRank(s string) int {
	switch s {
	case "high":
		return 0
	case "medium":
		return 1
	default:
		return 2
	}
}

// collectDBUnindexedTables issues one parameterized LEFT JOIN of
// information_schema.TABLES against information_schema.STATISTICS, scoped to
// TABLE_TYPE='BASE TABLE'. The GROUP BY collapses the per-index-column
// STATISTICS rows into one row per table; the two MAX(CASE ...) aggregates
// fold "does any index row name PRIMARY?" and "does any non-PRIMARY UNIQUE
// index exist?" server-side. A table with zero indexes still produces one row
// (the LEFT JOIN's NULL index side → both aggregates 0), so genuinely
// index-less tables are not lost. The IN clause is built from ? placeholders —
// no string interpolation of caller input. Filtering + rollups happen in Go.
func collectDBUnindexedTables(ctx context.Context, db *sql.DB, schemas []string, now time.Time, minRows int64) (map[string]any, error) {
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
			IFNULL(t.DATA_LENGTH, 0) + IFNULL(t.INDEX_LENGTH, 0),
			MAX(CASE WHEN s.INDEX_NAME = 'PRIMARY' THEN 1 ELSE 0 END),
			MAX(CASE WHEN s.NON_UNIQUE = 0 AND s.INDEX_NAME <> 'PRIMARY' THEN 1 ELSE 0 END)
		FROM information_schema.TABLES t
		LEFT JOIN information_schema.STATISTICS s
		  ON s.TABLE_SCHEMA = t.TABLE_SCHEMA AND s.TABLE_NAME = t.TABLE_NAME
		WHERE t.TABLE_TYPE = 'BASE TABLE'
		  AND t.TABLE_SCHEMA IN (` + strings.Join(placeholders, ",") + `)
		GROUP BY t.TABLE_SCHEMA, t.TABLE_NAME, t.ENGINE, t.TABLE_ROWS, t.DATA_LENGTH, t.INDEX_LENGTH
		ORDER BY t.TABLE_SCHEMA, t.TABLE_NAME`
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("query information_schema unindexed-tables join: %w", err)
	}
	defer rows.Close()

	// Pre-seed per-schema rollup so a schema with no findings (or that ops_ro
	// can't see) still appears with zeroes rather than silently vanishing.
	byDB := make(map[string]*unindexedPerDatabase, len(schemas))
	for _, s := range schemas {
		byDB[s] = &unindexedPerDatabase{Database: s}
	}
	candidates := make([]unindexedTableRow, 0, 16)
	totalScanned := 0
	for rows.Next() {
		var (
			schema, name, engine  string
			rowsEst, totalBytes   int64
			hasPrimary, hasUnique int64
		)
		if err := rows.Scan(&schema, &name, &engine, &rowsEst, &totalBytes, &hasPrimary, &hasUnique); err != nil {
			return nil, fmt.Errorf("scan unindexed-tables row: %w", err)
		}
		totalScanned++
		d := byDB[schema]
		if d != nil {
			d.TablesScanned++
		}
		if hasPrimary == 1 {
			continue // table has a PRIMARY KEY — fine
		}
		if minRows > 0 && rowsEst < minRows {
			continue // below the noise floor the operator asked to suppress
		}
		sev, reason := classifyUnindexed(hasUnique == 1)
		candidates = append(candidates, unindexedTableRow{
			Database:       schema,
			Table:          name,
			Engine:         engine,
			RowsEstimate:   rowsEst,
			TotalSizeBytes: totalBytes,
			TotalSizeHuman: humanBytes(uint64Of(totalBytes)),
			HasUniqueKey:   hasUnique == 1,
			Severity:       sev,
			Reason:         reason,
		})
		if d != nil {
			d.UnindexedCount++
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate unindexed-tables rows: %w", err)
	}

	// Worst-first: severity (high before medium), then biggest blast radius
	// (rowsEstimate desc, totalSizeBytes desc), then database+table asc so ties
	// stay deterministic across runs (Go map iteration is randomized).
	sort.Slice(candidates, func(i, j int) bool {
		ri, rj := unindexedSeverityRank(candidates[i].Severity), unindexedSeverityRank(candidates[j].Severity)
		if ri != rj {
			return ri < rj
		}
		if candidates[i].RowsEstimate != candidates[j].RowsEstimate {
			return candidates[i].RowsEstimate > candidates[j].RowsEstimate
		}
		if candidates[i].TotalSizeBytes != candidates[j].TotalSizeBytes {
			return candidates[i].TotalSizeBytes > candidates[j].TotalSizeBytes
		}
		if candidates[i].Database != candidates[j].Database {
			return candidates[i].Database < candidates[j].Database
		}
		return candidates[i].Table < candidates[j].Table
	})

	// Cross-schema total reflects ALL findings (pre-truncation) so the count is
	// honest even when the candidate list is capped for the MCP envelope.
	totalUnindexed := 0
	for _, d := range byDB {
		totalUnindexed += d.UnindexedCount
	}

	truncated := false
	if len(candidates) > dbUnindexedTablesMaxCandidates {
		candidates = candidates[:dbUnindexedTablesMaxCandidates]
		truncated = true
	}

	// Per-database rollup, sorted unindexedCount desc with schema-asc tiebreak.
	perDB := make([]unindexedPerDatabase, 0, len(byDB))
	for _, d := range byDB {
		perDB = append(perDB, *d)
	}
	sort.Slice(perDB, func(i, j int) bool {
		if perDB[i].UnindexedCount != perDB[j].UnindexedCount {
			return perDB[i].UnindexedCount > perDB[j].UnindexedCount
		}
		return perDB[i].Database < perDB[j].Database
	})

	return map[string]any{
		"asOf":           now.UTC().Format(time.RFC3339),
		"scannedSchemas": schemas,
		"candidates":     candidates,
		"perDatabase":    perDB,
		"truncated":      truncated,
		"minRows":        minRows,
		"totals": map[string]any{
			"tablesScanned":  totalScanned,
			"unindexedCount": totalUnindexed,
		},
	}, nil
}
