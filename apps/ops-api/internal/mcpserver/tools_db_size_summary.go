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
	dbSizeSummaryTimeout  = 5 * time.Second
	dbSizeSummaryDefaultN = 10
	dbSizeSummaryMaxN     = 50
)

// dbSizeKnownSchemas is the fixed schema allowlist db_size_summary scans.
// Mirrors db_query's allowedDatabases (acore_world/acore_characters/acore_auth)
// — the three schemas ops_ro is guaranteed to have SELECT on. acore_playerbots
// is intentionally excluded: ops_ro lacks SELECT there (see backlog item
// `ops_grant_playerbots_ro`), and information_schema.TABLES with missing
// privileges silently returns NULL for the size columns rather than erroring,
// which would produce a "playerbots is 0 B" line that's wrong AND
// indistinguishable from a truly-empty schema.
var dbSizeKnownSchemas = []string{"acore_world", "acore_characters", "acore_auth"}

// tableSizeRow is one row of the aggregated information_schema.TABLES output.
// `RowsEstimate` is INFORMATION_SCHEMA.TABLES.TABLE_ROWS — for InnoDB tables
// that's an inexact estimate (the optimizer's sample). Surface it as `Estimate`
// in the JSON shape so operators don't treat it as authoritative.
type tableSizeRow struct {
	Database       string `json:"database"`
	Table          string `json:"table"`
	Engine         string `json:"engine,omitempty"`
	RowsEstimate   int64  `json:"rowsEstimate"`
	DataSizeBytes  int64  `json:"dataSizeBytes"`
	IndexSizeBytes int64  `json:"indexSizeBytes"`
	TotalSizeBytes int64  `json:"totalSizeBytes"`
	TotalSizeHuman string `json:"totalSizeHuman"`
}

// dbSizePerDatabase is the per-schema rollup. Pre-sorted by totalSizeBytes
// desc so the largest database surfaces first in the response.
type dbSizePerDatabase struct {
	Database        string `json:"database"`
	TableCount      int    `json:"tableCount"`
	RowsEstimate    int64  `json:"rowsEstimate"`
	DataSizeBytes   int64  `json:"dataSizeBytes"`
	IndexSizeBytes  int64  `json:"indexSizeBytes"`
	TotalSizeBytes  int64  `json:"totalSizeBytes"`
	DataSizeHuman   string `json:"dataSizeHuman"`
	IndexSizeHuman  string `json:"indexSizeHuman"`
	TotalSizeHuman  string `json:"totalSizeHuman"`
}

// RegisterDBSizeSummaryTool registers `db_size_summary` — a one-call rollup
// over `information_schema.TABLES` that returns per-database totals + the
// top-N largest tables across the AC schemas. Operators previously had to
// either run a multi-line SUM(DATA_LENGTH+INDEX_LENGTH) GROUP BY query via
// db_query (which is forbidden — db_query blocks information_schema) or
// shell into mysql by hand. This is the canonical "disk pressure / backup
// planning" first call.
//
// Defaults are tuned for the common operator question: "which tables are
// eating disk?" — top-10 by total size, all three AC schemas in scope. The
// `database` arg narrows to one schema; `topN` clamps to 1..50.
func RegisterDBSizeSummaryTool(reg *Registry, deps DBDeps) {
	reg.Register(Tool{
		Name: "db_size_summary",
		Description: "Per-database size rollup from information_schema.TABLES — " +
			"data + index bytes summed across acore_world / acore_characters / " +
			"acore_auth, plus the top-N largest tables across those schemas. " +
			"Replaces the SUM(DATA_LENGTH+INDEX_LENGTH) GROUP BY pattern that " +
			"db_query can't run (information_schema is blocked there). Returns " +
			"`perDatabase` (one entry per schema with tableCount / dataSizeBytes " +
			"/ indexSizeBytes / totalSizeBytes / rowsEstimate + human-formatted " +
			"twins, sorted by totalSizeBytes desc), `topTables` (top-N largest " +
			"individual tables across all scanned schemas, sorted by " +
			"totalSizeBytes desc), and `totals` (sum across everything scanned). " +
			"TABLE_ROWS is an InnoDB-optimizer estimate, not an exact count — " +
			"surfaced as `rowsEstimate` so it's not confused with COUNT(*). " +
			"`database` filters to one schema (must be one of " +
			"acore_world/acore_characters/acore_auth); `topN` defaults to 10, " +
			"hard-capped at 50. acore_playerbots is intentionally OUT of scope " +
			"— ops_ro lacks SELECT there, which would surface as a misleading " +
			"0-byte row. Use for disk-pressure investigations, backup-size " +
			"planning, finding bloat candidates for OPTIMIZE TABLE. Read-only, " +
			"ops_ro pool, 5 s timeout. ops_ro must have SELECT on each scanned " +
			"schema to see its row data; missing-grant tables surface with NULL " +
			"size columns which collapse to 0 in the aggregation.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{
			"database":{"type":"string","description":"Filter to one schema (acore_world/acore_characters/acore_auth). Omit to scan all three.","enum":["acore_world","acore_characters","acore_auth"]},
			"topN":{"type":"integer","description":"Number of largest tables to return (default 10, hard cap 50)"}
		}}`),
		Annotations: AnnRead(),
		Handler: func(ctx context.Context, raw json.RawMessage, _ string) any {
			var a struct {
				Database string `json:"database"`
				TopN     int    `json:"topN"`
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
			if a.TopN <= 0 {
				a.TopN = dbSizeSummaryDefaultN
			}
			if a.TopN > dbSizeSummaryMaxN {
				a.TopN = dbSizeSummaryMaxN
			}
			c, cancel := context.WithTimeout(ctx, dbSizeSummaryTimeout)
			defer cancel()
			out, err := collectDBSizeSummary(c, deps.QueryDB, schemas, a.TopN)
			if err != nil {
				return map[string]any{"error": err.Error()}
			}
			return out
		},
	})
}

// isKnownSizeSchema returns true if name is in the dbSizeKnownSchemas allowlist.
// Linear scan over a 3-element slice — a map would be more code for no gain.
func isKnownSizeSchema(name string) bool {
	for _, s := range dbSizeKnownSchemas {
		if s == name {
			return true
		}
	}
	return false
}

// collectDBSizeSummary issues one parameterized SELECT against
// information_schema.TABLES, scoped to TABLE_TYPE='BASE TABLE' (drops VIEWs
// which have NULL size columns and would skew nothing but add noise). The IN
// clause is built from the schemas slice with ? placeholders — no string
// interpolation of caller input. Aggregation happens in Go to keep the SQL
// path trivial and let one query feed both rollups.
func collectDBSizeSummary(ctx context.Context, db *sql.DB, schemas []string, topN int) (map[string]any, error) {
	if len(schemas) == 0 {
		return nil, fmt.Errorf("no schemas specified")
	}
	placeholders := make([]string, len(schemas))
	args := make([]any, len(schemas))
	for i, s := range schemas {
		placeholders[i] = "?"
		args[i] = s
	}
	q := `SELECT TABLE_SCHEMA, TABLE_NAME, IFNULL(ENGINE,''),
			IFNULL(TABLE_ROWS, 0), IFNULL(DATA_LENGTH, 0), IFNULL(INDEX_LENGTH, 0)
		FROM information_schema.TABLES
		WHERE TABLE_TYPE = 'BASE TABLE'
		  AND TABLE_SCHEMA IN (` + strings.Join(placeholders, ",") + `)`
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("query information_schema.TABLES: %w", err)
	}
	defer rows.Close()

	// Pre-seed the per-database map so empty/non-matching schemas still surface
	// with zeroed counters. Without this, a schema that's empty (or that ops_ro
	// can't see) would silently vanish from perDatabase — operators would think
	// the tool was buggy when the schema was just empty.
	byDB := make(map[string]*dbSizePerDatabase, len(schemas))
	for _, s := range schemas {
		byDB[s] = &dbSizePerDatabase{Database: s}
	}
	all := make([]tableSizeRow, 0, 256)
	for rows.Next() {
		var (
			schema, name, engine string
			rowsEst, dataLen, idxLen int64
		)
		if err := rows.Scan(&schema, &name, &engine, &rowsEst, &dataLen, &idxLen); err != nil {
			return nil, fmt.Errorf("scan TABLES row: %w", err)
		}
		total := dataLen + idxLen
		t := tableSizeRow{
			Database:       schema,
			Table:          name,
			Engine:         engine,
			RowsEstimate:   rowsEst,
			DataSizeBytes:  dataLen,
			IndexSizeBytes: idxLen,
			TotalSizeBytes: total,
			TotalSizeHuman: humanBytes(uint64Of(total)),
		}
		all = append(all, t)
		if d, ok := byDB[schema]; ok {
			d.TableCount++
			d.RowsEstimate += rowsEst
			d.DataSizeBytes += dataLen
			d.IndexSizeBytes += idxLen
			d.TotalSizeBytes += total
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate TABLES: %w", err)
	}

	// Per-database rollup — humanize bytes, sort by total desc with schema-asc
	// as the deterministic tiebreaker (without it Go's map iteration order
	// would make tests flaky on equal totals).
	perDB := make([]dbSizePerDatabase, 0, len(byDB))
	for _, d := range byDB {
		d.DataSizeHuman = humanBytes(uint64Of(d.DataSizeBytes))
		d.IndexSizeHuman = humanBytes(uint64Of(d.IndexSizeBytes))
		d.TotalSizeHuman = humanBytes(uint64Of(d.TotalSizeBytes))
		perDB = append(perDB, *d)
	}
	sort.Slice(perDB, func(i, j int) bool {
		if perDB[i].TotalSizeBytes != perDB[j].TotalSizeBytes {
			return perDB[i].TotalSizeBytes > perDB[j].TotalSizeBytes
		}
		return perDB[i].Database < perDB[j].Database
	})

	// Top-N tables across all scanned schemas. Same tiebreaker shape: total
	// desc, then schema-asc + table-asc for determinism.
	sort.Slice(all, func(i, j int) bool {
		if all[i].TotalSizeBytes != all[j].TotalSizeBytes {
			return all[i].TotalSizeBytes > all[j].TotalSizeBytes
		}
		if all[i].Database != all[j].Database {
			return all[i].Database < all[j].Database
		}
		return all[i].Table < all[j].Table
	})
	if len(all) > topN {
		all = all[:topN]
	}

	// Cross-schema totals — sum of perDB. Compute from perDB rather than re-
	// summing `all` so the topN slice doesn't affect totals.
	var (
		totalTables, totalRows int64
		totalData, totalIdx, totalTot int64
	)
	for _, d := range perDB {
		totalTables += int64(d.TableCount)
		totalRows += d.RowsEstimate
		totalData += d.DataSizeBytes
		totalIdx += d.IndexSizeBytes
		totalTot += d.TotalSizeBytes
	}

	return map[string]any{
		"perDatabase": perDB,
		"topTables":   all,
		"totals": map[string]any{
			"tableCount":      totalTables,
			"rowsEstimate":    totalRows,
			"dataSizeBytes":   totalData,
			"indexSizeBytes":  totalIdx,
			"totalSizeBytes":  totalTot,
			"dataSizeHuman":   humanBytes(uint64Of(totalData)),
			"indexSizeHuman":  humanBytes(uint64Of(totalIdx)),
			"totalSizeHuman":  humanBytes(uint64Of(totalTot)),
		},
		"scannedSchemas": schemas,
		"topN":           topN,
	}, nil
}
