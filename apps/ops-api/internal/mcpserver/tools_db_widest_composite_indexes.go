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
	dbWidestCompositeIndexesTimeout = 10 * time.Second

	dbWidestCompositeIndexesDefaultTopN = 10
	dbWidestCompositeIndexesMaxTopN     = 50

	// dbWidestCompositeDefaultMinColumns is the default report floor AND the
	// medium/high band boundary: an index spanning this many columns is worth a
	// look. Heuristic, not a hard engine limit (InnoDB allows up to 16 columns
	// per index) — tunable via `minColumns`.
	dbWidestCompositeDefaultMinColumns = 4
	// dbWidestCompositeHighThreshold promotes an index to `high` severity: a
	// 6+-column composite is very wide — the trailing columns almost never earn
	// their write + storage cost.
	dbWidestCompositeHighThreshold = 6
	// dbWidestCompositeMinAllowedColumns is the smallest floor a caller may set.
	// A single-column index has no "width" concern and a composite needs >= 2
	// columns by definition, so a `minColumns` below this resets to the default
	// (a floor of 1 would flag every index in the schema).
	dbWidestCompositeMinAllowedColumns = 2
)

// wideIndexRow is one index whose column count meets the width floor. `Columns`
// is the ordered key (leading column first) so the operator sees exactly which
// trailing columns to consider trimming. `IsPrimary` separates the unavoidable
// wide PRIMARY KEY (InnoDB copies it into every secondary index) from a wide
// SECONDARY index (the real smell). `Unique` (NON_UNIQUE=0) flags indexes whose
// trailing columns can't be dropped without changing a uniqueness constraint.
type wideIndexRow struct {
	Database    string   `json:"database"`
	Table       string   `json:"table"`
	IndexName   string   `json:"indexName"`
	IsPrimary   bool     `json:"isPrimary"`
	Unique      bool     `json:"unique"`
	IndexType   string   `json:"indexType,omitempty"`
	ColumnCount int      `json:"columnCount"`
	Columns     []string `json:"columns"`
	Severity    string   `json:"severity"`
	Reason      string   `json:"reason"`
}

// wideIndexPerDatabase is the per-schema rollup. `IndexesScanned` is the
// denominator (every index in the schema, all widths); `WideCount` is how many
// reported candidates (post-minColumns / includePrimary filter) it contributed.
// Pre-seeded for all scanned schemas so a schema with no wide index still
// surfaces with zeroes (same silent-absence guard as db_over_indexed_tables).
type wideIndexPerDatabase struct {
	Database       string `json:"database"`
	IndexesScanned int    `json:"indexesScanned"`
	WideCount      int    `json:"wideCount"`
}

// RegisterDBWidestCompositeIndexesTool registers `db_widest_composite_indexes` —
// the COLUMN-COUNT (width) axis of the db_* index-hygiene family. Its siblings
// each flag a different index cost: db_over_indexed_tables COUNTS the indexes on
// a table, db_index_key_length_audit sums each index's BYTE length,
// db_index_data_ratio is index-bytes / data-bytes, db_redundant_indexes finds
// duplicate/prefix indexes. None answers "how many COLUMNS does a single index
// span?" — a 5+-column composite is expensive to maintain and its trailing
// columns are often dead weight (a BTREE can only range-scan the leading
// column(s); columns after the first inequality predicate don't narrow the
// scan). One GROUP BY table,index over information_schema.STATISTICS:
// MAX(SEQ_IN_INDEX) is the width, GROUP_CONCAT(COLUMN_NAME ORDER BY
// SEQ_IN_INDEX) is the ordered column list so the operator sees WHICH columns to
// consider trimming.
func RegisterDBWidestCompositeIndexesTool(reg *Registry, deps DBDeps) {
	reg.Register(Tool{
		Name: "db_widest_composite_indexes",
		Description: "Find the WIDEST indexes (most columns) across acore_world / " +
			"acore_characters / acore_auth — the column-COUNT axis of the " +
			"index-hygiene family, complementing db_over_indexed_tables (how MANY " +
			"indexes per table), db_index_key_length_audit (index BYTE length), and " +
			"db_index_data_ratio (index bytes vs data bytes). A wide composite index " +
			"is expensive: every INSERT/UPDATE/DELETE maintains all its columns, and " +
			"a BTREE can only range-scan on the leading column(s) — columns after the " +
			"first inequality predicate are dead weight for filtering. One GROUP BY " +
			"table,index over information_schema.STATISTICS: MAX(SEQ_IN_INDEX) is the " +
			"column count (width), GROUP_CONCAT(COLUMN_NAME ORDER BY SEQ_IN_INDEX) is " +
			"the ordered key so you see which trailing columns to trim. Returns `rows` " +
			"(each {database, table, indexName, isPrimary, unique, indexType, " +
			"columnCount, columns, severity, reason}, sorted widest-first: severity " +
			"then columnCount desc, secondary before PRIMARY within a tie), " +
			"`perDatabase` ({database, indexesScanned, wideCount} — pre-seeded so " +
			"every scanned schema surfaces), and `totals`. Severity is heuristic (NOT " +
			"a hard engine limit — InnoDB allows 16 columns per index): `high` at 6+ " +
			"columns, `medium` at 4-5, `low` below 4 (only shown when you lower " +
			"`minColumns`). PRIMARY KEYs are included and flagged `isPrimary` (a wide " +
			"PK is usually unavoidable but InnoDB copies it into every secondary " +
			"index, so its width inflates all of them) — pass `includePrimary:false` " +
			"to hide them and focus on droppable secondary indexes. `database` filters " +
			"to one schema; `minColumns` is the width floor (default 4; values <2 " +
			"reset to the default); `topN` (default 10, hard cap 50) bounds the " +
			"response. Pair with db_redundant_indexes / db_low_cardinality_indexes to " +
			"decide which wide index can actually be dropped. acore_playerbots is " +
			"intentionally OUT of scope — ops_ro lacks SELECT there (see " +
			"ops_grant_playerbots_ro). Read-only, ops_ro pool, 10 s timeout.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{
			"database":{"type":"string","enum":["acore_world","acore_characters","acore_auth"],"description":"Filter to one schema (default: all three)"},
			"minColumns":{"type":"integer","description":"Only report indexes spanning >= this many columns (default 4). Lower it (min 2) to surface narrower composites; values <2 reset to the default."},
			"includePrimary":{"type":"boolean","description":"Include PRIMARY KEYs (default true; they are flagged isPrimary). Pass false to hide them and focus on droppable secondary indexes."},
			"topN":{"type":"integer","description":"Max rows after filtering, sorted widest-first (default 10, hard cap 50)"}
		}}`),
		Annotations: AnnRead(),
		Handler: func(ctx context.Context, raw json.RawMessage, _ string) any {
			var a struct {
				Database       string `json:"database"`
				MinColumns     int    `json:"minColumns"`
				IncludePrimary *bool  `json:"includePrimary"`
				TopN           int    `json:"topN"`
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
			minCols := dbWidestCompositeDefaultMinColumns
			if a.MinColumns >= dbWidestCompositeMinAllowedColumns {
				minCols = a.MinColumns
			}
			includePrimary := true
			if a.IncludePrimary != nil {
				includePrimary = *a.IncludePrimary
			}
			topN := dbWidestCompositeIndexesDefaultTopN
			if a.TopN > 0 {
				topN = a.TopN
			}
			if topN > dbWidestCompositeIndexesMaxTopN {
				topN = dbWidestCompositeIndexesMaxTopN
			}
			c, cancel := context.WithTimeout(ctx, dbWidestCompositeIndexesTimeout)
			defer cancel()
			out, err := collectDBWidestCompositeIndexes(c, deps.QueryDB, schemas, minCols, includePrimary, topN, time.Now())
			if err != nil {
				return map[string]any{"error": err.Error()}
			}
			return out
		},
	})
}

// classifyIndexWidth returns the severity + human reason for an index of the
// given column count. Pure, so the heuristic banding is unit-tested in
// isolation. Bands: high >= dbWidestCompositeHighThreshold (6), medium >=
// dbWidestCompositeDefaultMinColumns (4), else low (only reachable when the
// caller lowers minColumns below the default).
func classifyIndexWidth(columnCount int) (severity, reason string) {
	switch {
	case columnCount >= dbWidestCompositeHighThreshold:
		return "high", fmt.Sprintf("%d-column composite — very wide: a BTREE only range-scans the leading column(s), so columns after the first inequality predicate don't narrow the scan yet cost write + storage on every row change. Review whether the trailing columns earn their keep", columnCount)
	case columnCount >= dbWidestCompositeDefaultMinColumns:
		return "medium", fmt.Sprintf("%d-column composite — moderately wide; the planner uses the leading column(s) for range scans, and trailing columns only help when every earlier predicate is equality. Check db_redundant_indexes for a narrower index that already covers the leading prefix", columnCount)
	default:
		return "low", fmt.Sprintf("%d-column index — below the default width threshold; reported because minColumns was lowered", columnCount)
	}
}

// wideIndexSeverityRank orders severities for the widest-first sort: high <
// medium < low. Unknown severities sort last (defensive). Kept local so a
// future change to this tool's banding doesn't silently move a sibling's sort.
func wideIndexSeverityRank(s string) int {
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

// collectDBWidestCompositeIndexes issues one parameterized aggregate over
// information_schema.STATISTICS. Indexes only exist on base tables, so no
// TABLES join is needed. The GROUP BY collapses the per-index-column STATISTICS
// rows into one row per index; MAX(SEQ_IN_INDEX) yields the column count and
// GROUP_CONCAT(COLUMN_NAME ORDER BY SEQ_IN_INDEX) yields the ordered key.
// NON_UNIQUE and INDEX_TYPE are constant per index, so grouping by them is
// ONLY_FULL_GROUP_BY-safe and never fragments a group. The IN clause is built
// from ? placeholders — no string interpolation of caller input. Filtering +
// rollups happen in Go.
func collectDBWidestCompositeIndexes(ctx context.Context, db *sql.DB, schemas []string, minColumns int, includePrimary bool, topN int, now time.Time) (map[string]any, error) {
	if len(schemas) == 0 {
		return nil, fmt.Errorf("no schemas specified")
	}
	placeholders := make([]string, len(schemas))
	args := make([]any, len(schemas))
	for i, s := range schemas {
		placeholders[i] = "?"
		args[i] = s
	}
	// ORDER BY SEQ_IN_INDEX inside GROUP_CONCAT assembles the key in leading-to-
	// trailing order. group_concat_max_len (default 1024) is ample for the short
	// column names + narrow indexes in the acore schemas.
	q := `SELECT s.TABLE_SCHEMA, s.TABLE_NAME, s.INDEX_NAME, s.NON_UNIQUE, IFNULL(s.INDEX_TYPE,''),
			MAX(s.SEQ_IN_INDEX),
			GROUP_CONCAT(s.COLUMN_NAME ORDER BY s.SEQ_IN_INDEX SEPARATOR ',')
		FROM information_schema.STATISTICS s
		WHERE s.TABLE_SCHEMA IN (` + strings.Join(placeholders, ",") + `)
		GROUP BY s.TABLE_SCHEMA, s.TABLE_NAME, s.INDEX_NAME, s.NON_UNIQUE, s.INDEX_TYPE
		ORDER BY s.TABLE_SCHEMA, s.TABLE_NAME, s.INDEX_NAME`
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("query information_schema widest-composite-indexes: %w", err)
	}
	defer rows.Close()

	// Pre-seed per-schema rollup so a schema with no findings (or that ops_ro
	// can't see) still appears with zeroes rather than silently vanishing.
	byDB := make(map[string]*wideIndexPerDatabase, len(schemas))
	for _, s := range schemas {
		byDB[s] = &wideIndexPerDatabase{Database: s}
	}
	candidates := make([]wideIndexRow, 0, 16)
	totalScanned := 0
	for rows.Next() {
		var (
			schema, name, indexName, indexType, cols string
			nonUnique, columnCount                   int
		)
		if err := rows.Scan(&schema, &name, &indexName, &nonUnique, &indexType, &columnCount, &cols); err != nil {
			return nil, fmt.Errorf("scan widest-composite-indexes row: %w", err)
		}
		totalScanned++
		d := byDB[schema]
		if d != nil {
			d.IndexesScanned++
		}
		isPrimary := indexName == "PRIMARY"
		if isPrimary && !includePrimary {
			continue // caller asked to hide PRIMARY KEYs
		}
		if columnCount < minColumns {
			continue // not wide enough by the caller's floor
		}
		sev, reason := classifyIndexWidth(columnCount)
		unique := nonUnique == 0
		if isPrimary {
			reason += ". This is the PRIMARY KEY — InnoDB copies the full PK into every secondary index, so its width inflates all of them; usually unavoidable, but confirm it is minimal"
		} else if unique {
			reason += ". UNIQUE index — trimming trailing columns changes the uniqueness constraint, so review the data model before dropping any"
		}
		columnList := []string{}
		if cols != "" {
			columnList = strings.Split(cols, ",")
		}
		candidates = append(candidates, wideIndexRow{
			Database:    schema,
			Table:       name,
			IndexName:   indexName,
			IsPrimary:   isPrimary,
			Unique:      unique,
			IndexType:   indexType,
			ColumnCount: columnCount,
			Columns:     columnList,
			Severity:    sev,
			Reason:      reason,
		})
		if d != nil {
			d.WideCount++
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate widest-composite-indexes rows: %w", err)
	}

	// Widest-first: severity (high before medium before low), then most columns
	// (columnCount desc), then secondary before PRIMARY (the droppable smell
	// ranks above the unavoidable PK at equal width), then database+table+index
	// asc so ties stay deterministic across runs (Go map iteration is
	// randomized).
	sort.Slice(candidates, func(i, j int) bool {
		ri, rj := wideIndexSeverityRank(candidates[i].Severity), wideIndexSeverityRank(candidates[j].Severity)
		if ri != rj {
			return ri < rj
		}
		if candidates[i].ColumnCount != candidates[j].ColumnCount {
			return candidates[i].ColumnCount > candidates[j].ColumnCount
		}
		if candidates[i].IsPrimary != candidates[j].IsPrimary {
			return !candidates[i].IsPrimary // secondary (false) before PRIMARY (true)
		}
		if candidates[i].Database != candidates[j].Database {
			return candidates[i].Database < candidates[j].Database
		}
		if candidates[i].Table != candidates[j].Table {
			return candidates[i].Table < candidates[j].Table
		}
		return candidates[i].IndexName < candidates[j].IndexName
	})

	// Cross-schema total reflects ALL findings (pre-truncation) so the count is
	// honest even when the candidate list is capped to topN for the envelope.
	totalWide := 0
	for _, d := range byDB {
		totalWide += d.WideCount
	}

	truncated := false
	if len(candidates) > topN {
		candidates = candidates[:topN]
		truncated = true
	}

	// Per-database rollup, sorted wideCount desc with schema-asc tiebreak.
	perDB := make([]wideIndexPerDatabase, 0, len(byDB))
	for _, d := range byDB {
		perDB = append(perDB, *d)
	}
	sort.Slice(perDB, func(i, j int) bool {
		if perDB[i].WideCount != perDB[j].WideCount {
			return perDB[i].WideCount > perDB[j].WideCount
		}
		return perDB[i].Database < perDB[j].Database
	})

	return map[string]any{
		"asOf":           now.UTC().Format(time.RFC3339),
		"scannedSchemas": schemas,
		"rows":           candidates,
		"perDatabase":    perDB,
		"truncated":      truncated,
		"minColumns":     minColumns,
		"includePrimary": includePrimary,
		"topN":           topN,
		"totals": map[string]any{
			"indexesScanned": totalScanned,
			"wideCount":      totalWide,
		},
	}, nil
}
