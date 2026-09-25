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
	dbTableBloatTimeout = 10 * time.Second

	dbTableBloatDefaultTopN = 10
	dbTableBloatMaxTopN     = 50

	// dbTableBloatDefaultMinBytes — below 1 MiB, a 90% bloat fraction is
	// just a few hundred KiB, not worth surfacing into a triage list. The
	// operator wants tables where OPTIMIZE TABLE would actually reclaim
	// disk.
	dbTableBloatDefaultMinBytes = 1 * 1024 * 1024

	// dbTableBloatDefaultMinFraction — 10% is the conventional rule of
	// thumb where InnoDB bloat starts to matter; anything below is
	// noise from normal page-fill churn.
	dbTableBloatDefaultMinFraction = 0.10
)

// DATA_FREE scope labels. information_schema.TABLES.DATA_FREE does NOT mean
// the same thing for every row, and the difference decides whether
// bloatFraction is a per-table figure at all. Per the MySQL manual's own
// wording for the column: "InnoDB tables report the free space of the
// tablespace to which the table belongs. For a table located in the shared
// tablespace, this is the free space of the shared tablespace. If you are
// using multiple tablespaces and the table has its own tablespace, the free
// space is for only that table."
//
// So for an InnoDB table living in the shared system tablespace, DATA_FREE is
// a SERVER-WIDE number repeated identically on every such row — and
// `bloatFraction = DATA_FREE / (DATA_LENGTH + DATA_FREE)` then degenerates
// into "1 / (1 + DATA_LENGTH/F)", i.e. a strictly decreasing function of
// DATA_LENGTH. Sorting by it desc returns the SMALLEST tables first, exactly
// inverting the ranking the tool exists to produce, and `minBytes` (which
// gates on DATA_LENGTH + DATA_FREE) stops filtering anything because F alone
// clears the 1 MiB floor. OPTIMIZE TABLE on those rows reclaims nothing.
// Non-InnoDB engines (MyISAM/Aria) count the per-file gap left by deletes, so
// their DATA_FREE is genuinely per-table regardless of the InnoDB setting.
const (
	dbTableBloatScopePerTable         = "perTable"
	dbTableBloatScopeSharedTablespace = "sharedTablespace"
	dbTableBloatScopeUnknown          = "unknown"
)

// tableBloatRow is one entry in the bloat report. `engine` is surfaced so the
// operator can tell at a glance whether DATA_FREE's meaning is the InnoDB
// "free pages in the tablespace" or the MyISAM "gap left by deletes" — they
// look the same in this column but recover differently (InnoDB needs OPTIMIZE
// TABLE which rebuilds; MyISAM has OPTIMIZE TABLE + REPAIR TABLE paths).
//
// `dataFreeScope` says whether THIS row's dataFreeBytes is a per-table figure
// or the shared tablespace's server-wide free space — see the scope constants
// above. Without it a caller cannot tell a genuinely 60%-fragmented table
// from a small table sharing a roomy system tablespace: both print the same
// bloatFraction with the same confidence.
type tableBloatRow struct {
	Database        string  `json:"database"`
	Table           string  `json:"table"`
	Engine          string  `json:"engine"`
	DataLengthBytes uint64  `json:"dataLengthBytes"`
	DataFreeBytes   uint64  `json:"dataFreeBytes"`
	TotalBytes      uint64  `json:"totalBytes"`
	BloatFraction   float64 `json:"bloatFraction"`
	DataFreeScope   string  `json:"dataFreeScope"`
	DataLengthHuman string  `json:"dataLengthHuman"`
	DataFreeHuman   string  `json:"dataFreeHuman"`
	TotalHuman      string  `json:"totalHuman"`
}

// RegisterDBTableBloatTool registers `db_table_bloat` — a fragmentation
// report sibling of `db_table_info` / `db_size_summary` (when it lands).
// information_schema.TABLES.DATA_FREE carries the bytes allocated to the
// table but currently unused; `DATA_FREE / (DATA_LENGTH + DATA_FREE)` is the
// bloat fraction. Operators target this list with `OPTIMIZE TABLE` after a
// churny delete-heavy workload (audit purges, expired auctions, mail
// clean-up). One round trip; filtering + sort + topN done in Go.
func RegisterDBTableBloatTool(reg *Registry, deps DBDeps) {
	reg.Register(Tool{
		Name: "db_table_bloat",
		Description: "Per-table fragmentation report from information_schema.TABLES " +
			"across acore_world / acore_characters / acore_auth. Returns the " +
			"top-N tables sorted by `bloatFraction = DATA_FREE / (DATA_LENGTH " +
			"+ DATA_FREE)` desc — the share of space allocated to the table " +
			"but currently unused. Operators use this list to pick " +
			"`OPTIMIZE TABLE` candidates after a churny delete-heavy " +
			"workload (audit purges, expired auctions, mail clean-up). " +
			"`minBytes` (default 1 MiB) skips tiny tables where 90% bloat is " +
			"a few hundred KiB; `minFraction` (default 0.10 = 10%) skips " +
			"already-tight tables; `topN` (default 10, hard cap 50) bounds " +
			"the response. READ `dataFreeBasis` BEFORE TRUSTING THE RANKING: " +
			"MySQL reports DATA_FREE for an InnoDB table in the shared system " +
			"tablespace as the free space of the WHOLE tablespace, identical " +
			"on every such row, so when `innodb_file_per_table` is OFF the " +
			"bloatFraction sort degenerates to smallest-table-first, minBytes " +
			"stops filtering, and OPTIMIZE TABLE reclaims nothing. Each row " +
			"carries `dataFreeScope` (perTable / sharedTablespace / unknown) " +
			"and the response carries `rowsDataFreeTablespaceWide` + " +
			"`rowsDataFreeScopeUnknown` so a clean report is distinguishable " +
			"from an unmeasurable one. MyISAM DATA_FREE is always per-table. " +
			"Pair with `db_size_summary` — that tool finds " +
			"the biggest tables, this one finds the bloated ones. " +
			"acore_playerbots is intentionally OUT of scope: ops_ro lacks " +
			"SELECT there, and information_schema returns NULL size columns " +
			"silently when grants are missing, which would surface as a " +
			"misleading 0-byte row (blocked on infra wedge " +
			"`ops_grant_playerbots_ro`). Read-only, uses the ops_ro pool, " +
			"10 s timeout.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{
			"database":{"type":"string","enum":["acore_world","acore_characters","acore_auth"],"description":"Limit to one schema (default: all three)"},
			"minBytes":{"type":"integer","description":"Skip tables where DATA_LENGTH + DATA_FREE is below this (default 1048576 = 1 MiB; pass <=0 to use default)"},
			"minFraction":{"type":"number","description":"Skip tables where bloatFraction is below this (default 0.10 = 10%; pass <=0 to use default; clamped to 1.0)"},
			"topN":{"type":"integer","description":"Max rows after filtering, sorted by bloatFraction desc (default 10, hard cap 50)"}
		}}`),
		Annotations: AnnRead(),
		Handler: func(ctx context.Context, raw json.RawMessage, _ string) any {
			var a struct {
				Database    string  `json:"database"`
				MinBytes    int64   `json:"minBytes"`
				MinFraction float64 `json:"minFraction"`
				TopN        int     `json:"topN"`
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
			minBytes := uint64(dbTableBloatDefaultMinBytes)
			if a.MinBytes > 0 {
				minBytes = uint64(a.MinBytes)
			}
			minFraction := dbTableBloatDefaultMinFraction
			if a.MinFraction > 0 {
				minFraction = a.MinFraction
			}
			if minFraction > 1.0 {
				minFraction = 1.0
			}
			topN := dbTableBloatDefaultTopN
			if a.TopN > 0 {
				topN = a.TopN
			}
			if topN > dbTableBloatMaxTopN {
				topN = dbTableBloatMaxTopN
			}
			schemas := bloatSchemaFilter(a.Database)
			c, cancel := context.WithTimeout(ctx, dbTableBloatTimeout)
			defer cancel()
			// Resolved in the handler, consumed by the collector: the
			// collector goldens then need only a trailing nil and issue no
			// extra query, so every pre-existing assertion keeps asserting
			// exactly what it always did.
			filePerTable, fptErr := readInnodbFilePerTable(c, deps.QueryDB)
			out, err := collectTableBloat(c, deps.QueryDB, schemas, minBytes, minFraction, topN, filePerTable)
			if err != nil {
				return map[string]any{"error": err.Error()}
			}
			out["dataFreeBasis"] = tableBloatDataFreeBasis(filePerTable, fptErr)
			out["minBytes"] = minBytes
			out["minFraction"] = minFraction
			out["topN"] = topN
			out["schemas"] = schemas
			return out
		},
	})
}

// bloatSchemaFilter resolves the optional `database` arg into the list of
// schemas the IN-clause will bind against. Returned slice is sorted so the
// parameterized query has a deterministic binding order across calls — keeps
// the query cache hot and makes test expectations stable.
func bloatSchemaFilter(database string) []string {
	if database == "" {
		return []string{"acore_auth", "acore_characters", "acore_world"}
	}
	return []string{database}
}

// collectTableBloat runs the information_schema.TABLES sweep, applies the
// minBytes + minFraction filters in Go, sorts the survivors by bloatFraction
// desc (schema-asc, table-asc as tiebreakers for test determinism), and
// truncates to topN. Returns a `scannedTables` counter so the operator can
// distinguish "no bloated tables" from "no tables matched the schema filter".
//
// filePerTable is the already-resolved @@innodb_file_per_table setting, or nil
// when it could not be read; it decides each row's dataFreeScope and is never
// queried here, so callers that do not care (every collector-level test) pass
// nil and issue no extra statement.
func collectTableBloat(ctx context.Context, db *sql.DB, schemas []string, minBytes uint64, minFraction float64, topN int, filePerTable *bool) (map[string]any, error) {
	if len(schemas) == 0 {
		return map[string]any{
			"rows":                       []tableBloatRow{},
			"rowCount":                   0,
			"scannedTables":              0,
			"rowsDataFreeTablespaceWide": 0,
			"rowsDataFreeScopeUnknown":   0,
		}, nil
	}
	innodbScope := innodbDataFreeScope(filePerTable)
	placeholders := make([]string, len(schemas))
	args := make([]any, len(schemas))
	for i, s := range schemas {
		placeholders[i] = "?"
		args[i] = s
	}
	q := "SELECT TABLE_SCHEMA, TABLE_NAME, IFNULL(ENGINE,''), IFNULL(DATA_LENGTH,0), IFNULL(DATA_FREE,0) " +
		"FROM information_schema.TABLES " +
		"WHERE TABLE_TYPE='BASE TABLE' " +
		"AND TABLE_SCHEMA IN (" + strings.Join(placeholders, ",") + ")"
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("query information_schema.TABLES: %w", err)
	}
	defer rows.Close()

	all := make([]tableBloatRow, 0, 64)
	scanned := 0
	for rows.Next() {
		scanned++
		var (
			r      tableBloatRow
			dl, df uint64
		)
		if err := rows.Scan(&r.Database, &r.Table, &r.Engine, &dl, &df); err != nil {
			return nil, fmt.Errorf("scan information_schema.TABLES: %w", err)
		}
		r.DataLengthBytes = dl
		r.DataFreeBytes = df
		r.TotalBytes = dl + df
		if r.TotalBytes < minBytes {
			continue
		}
		if r.TotalBytes == 0 {
			// Zero-byte tables (empty, just created, or NULL-leaking grant
			// gaps) would divide by zero. Skip explicitly so a test calling
			// collectTableBloat with minBytes=0 stays safe.
			continue
		}
		frac := float64(df) / float64(r.TotalBytes)
		if frac < minFraction {
			continue
		}
		r.BloatFraction = frac
		r.DataFreeScope = rowDataFreeScope(r.Engine, innodbScope)
		r.DataLengthHuman = humanBytes(dl)
		r.DataFreeHuman = humanBytes(df)
		r.TotalHuman = humanBytes(r.TotalBytes)
		all = append(all, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate information_schema.TABLES: %w", err)
	}

	sort.SliceStable(all, func(i, j int) bool {
		if all[i].BloatFraction != all[j].BloatFraction {
			return all[i].BloatFraction > all[j].BloatFraction
		}
		if all[i].Database != all[j].Database {
			return all[i].Database < all[j].Database
		}
		return all[i].Table < all[j].Table
	})
	if len(all) > topN {
		all = all[:topN]
	}

	// Counted over the RETURNED rows, not the whole scan: the qualifier is
	// about the list the operator is about to act on, and `rowCount` is the
	// denominator it pairs with. Both are zero on a healthy per-table realm,
	// so they add nothing to read when there is nothing to warn about.
	tablespaceWide, scopeUnknown := 0, 0
	for _, r := range all {
		switch r.DataFreeScope {
		case dbTableBloatScopeSharedTablespace:
			tablespaceWide++
		case dbTableBloatScopeUnknown:
			scopeUnknown++
		}
	}

	return map[string]any{
		"rows":                       all,
		"rowCount":                   len(all),
		"scannedTables":              scanned,
		"rowsDataFreeTablespaceWide": tablespaceWide,
		"rowsDataFreeScopeUnknown":   scopeUnknown,
	}, nil
}

// readInnodbFilePerTable resolves the server's `innodb_file_per_table`
// setting. Uses SHOW GLOBAL VARIABLES LIKE (the same surface db_global_status
// already reads through the ops_ro pool, so no new grant) rather than
// information_schema.INNODB_TABLESPACES, which would give the authoritative
// PER-TABLE answer but requires the PROCESS privilege — outside the confirmed
// ops_ro grant boundary, so it is an infra wedge, not something to build on
// here.
//
// A failure is NOT fatal: the size figures are still true, only their scope is
// then unknown, so the caller degrades to `unknown` and says so rather than
// taking the whole report down.
func readInnodbFilePerTable(ctx context.Context, db *sql.DB) (*bool, error) {
	if db == nil {
		return nil, fmt.Errorf("ops_ro pool not configured")
	}
	rows, err := db.QueryContext(ctx, "SHOW GLOBAL VARIABLES LIKE 'innodb_file_per_table'")
	if err != nil {
		return nil, fmt.Errorf("SHOW GLOBAL VARIABLES LIKE 'innodb_file_per_table': %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var name, value sql.NullString
		if err := rows.Scan(&name, &value); err != nil {
			return nil, fmt.Errorf("scan innodb_file_per_table: %w", err)
		}
		// isOn() normalizes both the "ON"/"OFF" and the "1"/"0" forms MySQL
		// and MariaDB hand back for boolean variables.
		on := isOn(value.String)
		return &on, nil
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate innodb_file_per_table: %w", err)
	}
	// No row at all — an engine build without InnoDB, or a variable the
	// server does not expose. Unknown, not false.
	return nil, fmt.Errorf("innodb_file_per_table not reported by this server")
}

// innodbDataFreeScope maps the resolved setting onto what DATA_FREE means for
// an InnoDB row.
func innodbDataFreeScope(filePerTable *bool) string {
	if filePerTable == nil {
		return dbTableBloatScopeUnknown
	}
	if *filePerTable {
		return dbTableBloatScopePerTable
	}
	return dbTableBloatScopeSharedTablespace
}

// rowDataFreeScope resolves one row's scope. Only InnoDB inherits the
// tablespace question — MyISAM/Aria DATA_FREE is the per-file gap left by
// deletes, so it is per-table whatever the InnoDB setting says, and labelling
// it `unknown` because InnoDB's setting could not be read would be a
// fabricated doubt.
func rowDataFreeScope(engine, innodbScope string) string {
	if !strings.EqualFold(engine, "InnoDB") {
		return dbTableBloatScopePerTable
	}
	return innodbScope
}

// tableBloatDataFreeBasis packs the response-level explanation of what
// DATA_FREE measured on this call. Mirrors the `basis` field shape
// db_global_status uses for its lifetime-vs-interval checks: the numbers keep
// their names, and the honesty about how to read them lives in a sibling
// block rather than in a name that changes with the measurement.
func tableBloatDataFreeBasis(filePerTable *bool, resolveErr error) map[string]any {
	scope := innodbDataFreeScope(filePerTable)
	out := map[string]any{
		"innodbFilePerTable": filePerTable,
		"innodbScope":        scope,
	}
	switch scope {
	case dbTableBloatScopeSharedTablespace:
		out["detail"] = "innodb_file_per_table is OFF: InnoDB tables in the shared system " +
			"tablespace all report the SAME tablespace-wide DATA_FREE, so bloatFraction is " +
			"not a per-table figure — the ranking degenerates to smallest-table-first, " +
			"minBytes stops filtering, and OPTIMIZE TABLE on these rows reclaims nothing. " +
			"Treat dataLengthBytes as the only trustworthy size here. Rows on other engines " +
			"(dataFreeScope=perTable) are unaffected."
	case dbTableBloatScopePerTable:
		out["detail"] = "innodb_file_per_table is ON, so InnoDB tables created under this " +
			"setting own their tablespace and DATA_FREE is per-table. A table created while " +
			"it was OFF still lives in the shared system tablespace and would still report " +
			"the tablespace-wide figure; proving placement per table needs " +
			"information_schema.INNODB_TABLESPACES, which requires the PROCESS privilege " +
			"that ops_ro does not hold."
	default:
		detail := "could not resolve innodb_file_per_table, so it is unknown whether InnoDB " +
			"DATA_FREE is per-table or the shared tablespace's server-wide free space; " +
			"bloatFraction on InnoDB rows may not be a per-table figure."
		if resolveErr != nil {
			detail += " Reason: " + resolveErr.Error() + "."
		}
		out["detail"] = detail
	}
	return out
}
