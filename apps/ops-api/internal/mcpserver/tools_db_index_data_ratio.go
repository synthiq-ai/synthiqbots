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
	dbIndexDataRatioTimeout = 10 * time.Second

	dbIndexDataRatioDefaultTopN = 10
	dbIndexDataRatioMaxTopN     = 50

	// dbIndexDataRatioDefaultMinBytes — the noise floor is applied to
	// INDEX_LENGTH (the metric of interest), not the table total: a table
	// whose secondary indexes are only a few hundred KiB isn't worth an
	// over-indexing review even if the ratio is high, because the absolute
	// disk cost is trivial. 1 MiB matches the db_table_bloat / db_size_summary
	// floor.
	dbIndexDataRatioDefaultMinBytes = 1 * 1024 * 1024

	// dbIndexDataRatioDefaultMinRatio — 1.0 means "secondary indexes weigh at
	// least as much as the clustered data". For InnoDB, DATA_LENGTH is the
	// clustered (PK) index + row data and INDEX_LENGTH is the sum of the
	// SECONDARY indexes, so a ratio >= 1 is the classic over-indexing space
	// signal: the table spends more disk indexing its rows than storing them.
	dbIndexDataRatioDefaultMinRatio = 1.0
)

// indexDataRatioRow is one entry in the index-vs-data report. `engine` is
// surfaced because the ratio only carries the "secondary indexes vs clustered
// data" meaning for InnoDB — for MyISAM, DATA_LENGTH and INDEX_LENGTH are the
// separate .MYD/.MYI file sizes, so a high ratio there is read differently
// (fat .MYI, not "over-indexed clustered table"). The operator can tell at a
// glance which interpretation applies.
type indexDataRatioRow struct {
	Database         string  `json:"database"`
	Table            string  `json:"table"`
	Engine           string  `json:"engine"`
	DataLengthBytes  uint64  `json:"dataLengthBytes"`
	IndexLengthBytes uint64  `json:"indexLengthBytes"`
	IndexDataRatio   float64 `json:"indexDataRatio"`
	RowsEstimate     int64   `json:"rowsEstimate"`
	DataLengthHuman  string  `json:"dataLengthHuman"`
	IndexLengthHuman string  `json:"indexLengthHuman"`
}

// RegisterDBIndexDataRatioTool registers `db_index_data_ratio` — a per-table
// over-indexing SPACE audit that completes the index-hygiene family. Its
// siblings each flag a different axis: db_over_indexed_tables COUNTS the
// secondary indexes on a table, db_index_key_length_audit sums each index's
// BYTE length, db_redundant_indexes finds duplicate/prefix indexes. None of
// them answers "which tables spend more disk on indexes than on the data
// itself?" — that's the INDEX_LENGTH / DATA_LENGTH ratio, read straight from
// information_schema.TABLES in one round trip (the same source db_size_summary
// and db_table_bloat use). Operators reach for it during disk-pressure
// triage: a table with a ratio well above 1 is a candidate for dropping a
// low-value secondary index rather than an OPTIMIZE TABLE (that's
// db_table_bloat's DATA_FREE lens).
func RegisterDBIndexDataRatioTool(reg *Registry, deps DBDeps) {
	reg.Register(Tool{
		Name: "db_index_data_ratio",
		Description: "Per-table over-indexing space audit from information_schema.TABLES " +
			"across acore_world / acore_characters / acore_auth. Returns the " +
			"top-N tables sorted by `indexDataRatio = INDEX_LENGTH / DATA_LENGTH` " +
			"desc — the ratio of secondary-index bytes to clustered-data bytes. " +
			"For InnoDB, DATA_LENGTH is the clustered (PK) index + rows and " +
			"INDEX_LENGTH is the sum of the SECONDARY indexes, so a ratio >= 1 " +
			"means the table spends more disk indexing its rows than storing " +
			"them — a candidate for dropping a low-value index. Distinct from " +
			"`db_over_indexed_tables` (which COUNTS indexes) and " +
			"`db_size_summary` (which finds the biggest tables): this finds " +
			"tables whose indexes are HEAVY relative to their data. `minBytes` " +
			"(default 1 MiB) is applied to INDEX_LENGTH so tiny indexes don't " +
			"surface however lopsided the ratio; `minRatio` (default 1.0) skips " +
			"tables where data still dominates; `topN` (default 10, hard cap " +
			"50) bounds the response. acore_playerbots is intentionally OUT of " +
			"scope: ops_ro lacks SELECT there, and information_schema returns " +
			"NULL size columns silently when grants are missing, which would " +
			"surface as a misleading 0-byte row (blocked on infra wedge " +
			"`ops_grant_playerbots_ro`). A table whose INDEX_LENGTH clears " +
			"`minBytes` but whose DATA_LENGTH is 0 has an undefined " +
			"(unbounded) ratio, so it is NOT ranked — an undefined ratio is " +
			"not a large one, and floating an empty table above a genuinely " +
			"over-indexed one would be worse than omitting it. It is instead " +
			"counted in `tablesSkippedZeroDataLength`, so `rowCount: 0` no " +
			"longer means both 'no table is over-indexed' and 'N tables " +
			"could not be evaluated'. That counter covers a genuinely empty " +
			"table AND a NULL DATA_LENGTH together: the query coalesces NULL " +
			"to 0 via IFNULL, so the two are not separable here and the " +
			"response does not pretend otherwise. `scannedTables` decomposes " +
			"exactly — it equals tablesSkippedByMinBytes + " +
			"tablesSkippedZeroDataLength + tablesSkippedByMinRatio + " +
			"eligibleTables. `eligibleTables` counts survivors BEFORE the " +
			"topN cut, so eligibleTables > rowCount means the response was " +
			"truncated. Read-only, uses the ops_ro pool, " +
			"10 s timeout.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{
			"database":{"type":"string","enum":["acore_world","acore_characters","acore_auth"],"description":"Limit to one schema (default: all three)"},
			"minBytes":{"type":"integer","description":"Skip tables where INDEX_LENGTH is below this (default 1048576 = 1 MiB; pass <=0 to use default)"},
			"minRatio":{"type":"number","description":"Skip tables where indexDataRatio is below this (default 1.0 = secondary indexes >= clustered data; pass <=0 to use default)"},
			"topN":{"type":"integer","description":"Max rows after filtering, sorted by indexDataRatio desc (default 10, hard cap 50)"}
		}}`),
		Annotations: AnnRead(),
		Handler: func(ctx context.Context, raw json.RawMessage, _ string) any {
			var a struct {
				Database string  `json:"database"`
				MinBytes int64   `json:"minBytes"`
				MinRatio float64 `json:"minRatio"`
				TopN     int     `json:"topN"`
			}
			if err := json.Unmarshal(raw, &a); err != nil {
				return map[string]any{"error": "decode args: " + err.Error()}
			}
			if a.Database != "" && !allowedDatabases[a.Database] {
				return map[string]any{"error": "database must be one of acore_world/acore_characters/acore_auth"}
			}
			if deps.QueryDB == nil {
				return map[string]any{"error": "ops_ro pool not configured (DB_DSN empty?)"}
			}
			minBytes := uint64(dbIndexDataRatioDefaultMinBytes)
			if a.MinBytes > 0 {
				minBytes = uint64(a.MinBytes)
			}
			minRatio := dbIndexDataRatioDefaultMinRatio
			if a.MinRatio > 0 {
				minRatio = a.MinRatio
			}
			topN := dbIndexDataRatioDefaultTopN
			if a.TopN > 0 {
				topN = a.TopN
			}
			if topN > dbIndexDataRatioMaxTopN {
				topN = dbIndexDataRatioMaxTopN
			}
			schemas := indexDataRatioSchemaFilter(a.Database)
			c, cancel := context.WithTimeout(ctx, dbIndexDataRatioTimeout)
			defer cancel()
			out, err := collectIndexDataRatio(c, deps.QueryDB, schemas, minBytes, minRatio, topN)
			if err != nil {
				return map[string]any{"error": err.Error()}
			}
			out["minBytes"] = minBytes
			out["minRatio"] = minRatio
			out["topN"] = topN
			out["schemas"] = schemas
			return out
		},
	})
}

// indexDataRatioSchemaFilter resolves the optional `database` arg into the
// list of schemas the IN-clause binds against. Returned slice is sorted so the
// parameterized query has a deterministic binding order across calls — keeps
// the query cache hot and makes the WithArgs() test expectations stable.
// Mirrors bloatSchemaFilter; kept local so the two space-audit tools stay
// independent (a future change to one's schema scope shouldn't silently move
// the other's).
func indexDataRatioSchemaFilter(database string) []string {
	if database == "" {
		return []string{"acore_auth", "acore_characters", "acore_world"}
	}
	return []string{database}
}

// collectIndexDataRatio runs the information_schema.TABLES sweep, applies the
// minBytes (on INDEX_LENGTH) + minRatio filters in Go, sorts the survivors by
// indexDataRatio desc (schema-asc, table-asc as tiebreakers for test
// determinism), and truncates to topN. Returns a `scannedTables` counter so
// the operator can distinguish "no over-indexed tables" from "no tables
// matched the schema filter" — plus a per-guard skip counter for each of the
// three `continue`s, so scannedTables decomposes and a zero-row answer says
// WHY it is zero. tablesSkippedZeroDataLength is the load-bearing one: those
// rows cleared the minBytes floor and have an unbounded ratio, making them
// the most extreme members of exactly the set this tool reports, and before
// the counters existed they were discarded without a trace.
func collectIndexDataRatio(ctx context.Context, db *sql.DB, schemas []string, minBytes uint64, minRatio float64, topN int) (map[string]any, error) {
	if len(schemas) == 0 {
		// Same key set as the main path — a consumer must not have to branch
		// on which shape it got.
		return map[string]any{
			"rows":                        []indexDataRatioRow{},
			"rowCount":                    0,
			"scannedTables":               0,
			"tablesSkippedByMinBytes":     0,
			"tablesSkippedZeroDataLength": 0,
			"tablesSkippedByMinRatio":     0,
			"eligibleTables":              0,
		}, nil
	}
	placeholders := make([]string, len(schemas))
	args := make([]any, len(schemas))
	for i, s := range schemas {
		placeholders[i] = "?"
		args[i] = s
	}
	q := "SELECT TABLE_SCHEMA, TABLE_NAME, IFNULL(ENGINE,''), IFNULL(DATA_LENGTH,0), IFNULL(INDEX_LENGTH,0), IFNULL(TABLE_ROWS,0) " +
		"FROM information_schema.TABLES " +
		"WHERE TABLE_TYPE='BASE TABLE' " +
		"AND TABLE_SCHEMA IN (" + strings.Join(placeholders, ",") + ")"
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("query information_schema.TABLES: %w", err)
	}
	defer rows.Close()

	all := make([]indexDataRatioRow, 0, 64)
	scanned := 0
	// Every `continue` below is a table the operator never sees. Counted
	// separately so `scannedTables` decomposes exactly (see the invariant
	// asserted at the return) and a zero-row answer is readable: "nothing is
	// over-indexed" and "nothing could be evaluated" are different findings.
	skippedMinBytes := 0
	skippedZeroData := 0
	skippedMinRatio := 0
	for rows.Next() {
		scanned++
		var (
			r      indexDataRatioRow
			dl, il uint64
		)
		if err := rows.Scan(&r.Database, &r.Table, &r.Engine, &dl, &il, &r.RowsEstimate); err != nil {
			return nil, fmt.Errorf("scan information_schema.TABLES: %w", err)
		}
		r.DataLengthBytes = dl
		r.IndexLengthBytes = il
		if il < minBytes {
			skippedMinBytes++
			continue
		}
		if dl == 0 {
			// A zero DATA_LENGTH row would divide by zero, so it is still
			// NOT ranked: an undefined ratio is not a large ratio, and
			// emitting +Inf would both break json.Marshal and float an empty
			// table above a genuinely over-indexed one.
			//
			// But this row already cleared minBytes — its secondary indexes
			// are real, and it is the most extreme member of the very set
			// this tool exists to find. Dropping it silently made "no table
			// is over-indexed" and "N tables could not be evaluated" the
			// same answer. Counting it is the whole fix.
			//
			// The counter deliberately covers BOTH causes: a genuinely empty
			// table, and a NULL DATA_LENGTH. The query coalesces NULL to 0
			// via IFNULL, so by the time the value reaches here the two are
			// already indistinguishable — splitting the counter would claim
			// a distinction this query does not carry.
			skippedZeroData++
			continue
		}
		ratio := float64(il) / float64(dl)
		if ratio < minRatio {
			skippedMinRatio++
			continue
		}
		r.IndexDataRatio = ratio
		r.DataLengthHuman = humanBytes(dl)
		r.IndexLengthHuman = humanBytes(il)
		all = append(all, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate information_schema.TABLES: %w", err)
	}

	sort.SliceStable(all, func(i, j int) bool {
		if all[i].IndexDataRatio != all[j].IndexDataRatio {
			return all[i].IndexDataRatio > all[j].IndexDataRatio
		}
		if all[i].Database != all[j].Database {
			return all[i].Database < all[j].Database
		}
		return all[i].Table < all[j].Table
	})
	// Captured BEFORE the topN cut: with only rowCount the operator cannot
	// tell a complete answer from a truncated one, and it is also the term
	// that closes the scannedTables accounting below.
	eligible := len(all)
	if len(all) > topN {
		all = all[:topN]
	}

	// Invariant (pinned by TestDBIndexDataRatio_SkipCountersAccountForEveryScannedTable):
	//   scannedTables == tablesSkippedByMinBytes + tablesSkippedZeroDataLength +
	//                    tablesSkippedByMinRatio + eligibleTables
	// Each counter is incremented only inside its own guard branch, so the
	// decomposition holds by construction rather than by an invariant
	// maintained somewhere else in the file.
	return map[string]any{
		"rows":                        all,
		"rowCount":                    len(all),
		"scannedTables":               scanned,
		"tablesSkippedByMinBytes":     skippedMinBytes,
		"tablesSkippedZeroDataLength": skippedZeroData,
		"tablesSkippedByMinRatio":     skippedMinRatio,
		"eligibleTables":              eligible,
	}, nil
}
