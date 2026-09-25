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
	dbColumnTypeDriftTimeout = 10 * time.Second

	// dbColumnTypeDriftMaxCandidates bounds the MCP envelope. A mature schema
	// realistically yields only a handful of same-named columns whose type
	// actually diverges, so this is a guard rail, not an expected limit; totals
	// stays the honest pre-cap count.
	dbColumnTypeDriftMaxCandidates = 500

	// dbColumnTypeDriftMaxTablesPerVariant caps the example-table list carried by
	// each type variant so one column that drifts across dozens of tables can't
	// blow the response budget. The variant's tableCount stays the honest full
	// count even when the tables[] list is clipped.
	dbColumnTypeDriftMaxTablesPerVariant = 25
)

// columnTypeRow is one raw column from information_schema.COLUMNS joined to
// TABLES (BASE TABLE only). DataType is the base type without length/width
// (e.g. "int", "bigint", "varchar"); ColumnType is the full declaration
// (e.g. "int(10) unsigned"), which is what carries the signedness token.
type columnTypeRow struct {
	Schema     string
	Table      string
	Column     string
	DataType   string
	ColumnType string
}

// columnTypeVariant is one distinct full-COLUMN_TYPE declaration of a drifting
// column, with the tables that declare it that way. Tables is sorted asc and
// clipped to dbColumnTypeDriftMaxTablesPerVariant; TableCount is always the
// honest full count.
type columnTypeVariant struct {
	DataType   string   `json:"dataType"`
	ColumnType string   `json:"columnType"`
	Unsigned   bool     `json:"unsigned"`
	TableCount int      `json:"tableCount"`
	Tables     []string `json:"tables"`
}

// columnTypeDriftRow is one flagged column NAME whose declared type diverges
// across the tables of a single schema. DistinctSignatures counts the distinct
// (dataType, signedness) pairs — the drift metric that drives flagging and
// severity; TableCount is the total blast radius (tables the name appears in).
type columnTypeDriftRow struct {
	Database           string              `json:"database"`
	Column             string              `json:"column"`
	DistinctSignatures int                 `json:"distinctSignatures"`
	TableCount         int                 `json:"tableCount"`
	Variants           []columnTypeVariant `json:"variants"`
	Severity           string              `json:"severity"`
	Reason             string              `json:"reason"`
}

// columnTypeDriftPerDatabase rolls up the type-consistency landscape per scanned
// schema. Pre-seeded for every scanned schema so an empty / no-grant schema still
// surfaces (operators can't mistake "scanned, nothing flagged" for a bug).
type columnTypeDriftPerDatabase struct {
	Database            string `json:"database"`
	ColumnsScanned      int    `json:"columnsScanned"`
	DistinctColumnNames int    `json:"distinctColumnNames"`
	DriftCount          int    `json:"driftCount"`
}

// columnNameAgg accumulates the type variants of one (schema, columnName) group
// during the scan. variants is keyed by full COLUMN_TYPE so display detail keeps
// the exact declarations; signatures is the set of (dataType, unsigned) pairs
// that decides whether the group has drifted and how severe it is.
type columnNameAgg struct {
	variants   map[string]*columnTypeVariant // full COLUMN_TYPE -> variant
	signatures map[string]struct{}           // "dataType|unsigned" -> present
	baseTypes  map[string]struct{}           // dataType -> present
	tableCount int
}

func newColumnNameAgg() *columnNameAgg {
	return &columnNameAgg{
		variants:   map[string]*columnTypeVariant{},
		signatures: map[string]struct{}{},
		baseTypes:  map[string]struct{}{},
	}
}

// RegisterDBColumnTypeDriftTool registers `db_column_type_drift` — the column
// TYPE-consistency lens of the db_* suite. db_charset_collation_audit (#280)
// finds string columns whose CHARSET/COLLATION deviates from the schema norm (a
// JOIN implicit-conversion footgun on the charset axis); this tool asks the same
// question on the TYPE axis: which same-named columns are declared with a
// different base type or signedness across the tables of one schema. When such a
// column is JOINed or compared against its differently-typed namesake (a `guid`
// that is `int unsigned` in one table but `bigint` or signed `int` in another),
// MySQL must implicitly convert one side — defeating the index on the converted
// column (a full-scan JOIN) and, for signed/unsigned, silently misordering values
// near the sign boundary. Reuses the long-merged `dbSizeKnownSchemas` +
// `isKnownSizeSchema` allowlist (db_size_summary, #172) and the `severityName`
// helper (db_engine_distribution, #287).
func RegisterDBColumnTypeDriftTool(reg *Registry, deps DBDeps) {
	reg.Register(Tool{
		Name: "db_column_type_drift",
		Description: "Column-type-consistency audit from information_schema.COLUMNS " +
			"joined to information_schema.TABLES, across acore_world / " +
			"acore_characters / acore_auth. Groups columns by NAME within each " +
			"schema and flags every name declared with more than one " +
			"(DATA_TYPE, signedness) signature across its tables — e.g. a `guid` " +
			"column that is `int(10) unsigned` in one table but `bigint(20)` or a " +
			"signed `int` in another. Why it matters: JOINing or comparing two " +
			"columns of different types forces MySQL to implicitly convert one side, " +
			"which defeats the index on the converted column (turning an indexed " +
			"JOIN into a full scan) and, for a SIGNED-vs-UNSIGNED integer mix, can " +
			"silently misorder values near the sign boundary. It is the TYPE-axis " +
			"companion to db_charset_collation_audit's charset/collation JOIN-hazard " +
			"lens, and distinct from db_table_info (which shows ONE table's columns " +
			"via SHOW CREATE TABLE but never compares a name across tables). Severity: " +
			"high = the base DATA_TYPE itself differs across tables (int vs bigint, " +
			"number vs varchar — a hard implicit conversion); medium = the base type " +
			"matches but the column mixes SIGNED and UNSIGNED (the subtler signedness " +
			"index-defeat). Integer display width alone (int(10) vs int(11)) and pure " +
			"VARCHAR length (varchar(64) vs varchar(255)) are deliberately NOT flagged " +
			"— they compare natively and would only add noise, so the signature " +
			"ignores them. Each candidate carries its `variants` (per distinct " +
			"COLUMN_TYPE: dataType / columnType / unsigned / tableCount / example " +
			"tables), `distinctSignatures`, total `tableCount` (blast radius), and a " +
			"`reason` (confirm with SHOW CREATE TABLE before ALTER TABLE ... MODIFY). " +
			"`database` narrows to one schema (rejected, not ignored, when not in the " +
			"allowlist). `minSeverity` (low/medium/high, default low=all) trims the " +
			"candidate LIST to at-or-above that severity; perDatabase and totals stay " +
			"the honest full counts. acore_playerbots is intentionally OUT of scope " +
			"(ops_ro lacks SELECT there — blocked on `ops_grant_playerbots_ro`). " +
			"`perDatabase` reports each schema's columnsScanned / distinctColumnNames " +
			"/ driftCount; results sorted worst-first (severity, then widest blast " +
			"radius), capped at 500 with `truncated`; `totals` carries the honest " +
			"pre-cap counts. Read-only, ops_ro pool, 10 s timeout.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{
			"database":{"type":"string","enum":["acore_world","acore_characters","acore_auth"],"description":"Limit to one schema (default: all three). acore_playerbots is out of scope (ops_ro lacks SELECT)."},
			"minSeverity":{"type":"string","enum":["low","medium","high"],"description":"Only list candidates at or above this severity (default low = all). perDatabase/totals stay the honest full counts."}
		}}`),
		Annotations: AnnRead(),
		Handler: func(ctx context.Context, raw json.RawMessage, _ string) any {
			var a struct {
				Database    string `json:"database"`
				MinSeverity string `json:"minSeverity"`
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
			// minSeverity is rejected (not clamped) when out of range: a typo'd
			// value should read as "bad filter", not silently widen to "all".
			minRank := columnTypeDriftSeverityRank("low")
			if a.MinSeverity != "" {
				switch a.MinSeverity {
				case "low", "medium", "high":
					minRank = columnTypeDriftSeverityRank(a.MinSeverity)
				default:
					return map[string]any{"error": "minSeverity must be one of low/medium/high"}
				}
			}
			c, cancel := context.WithTimeout(ctx, dbColumnTypeDriftTimeout)
			defer cancel()
			out, err := collectColumnTypeDrift(c, deps.QueryDB, time.Now(), schemas, minRank)
			if err != nil {
				return map[string]any{"error": err.Error()}
			}
			return out
		},
	})
}

// columnTypeUnsigned reports whether a full COLUMN_TYPE declares an unsigned
// numeric column. The "unsigned" token only appears on integer/decimal/float
// types, so string/temporal columns always return false.
func columnTypeUnsigned(columnType string) bool {
	return strings.Contains(strings.ToLower(columnType), "unsigned")
}

// columnTypeSignature is the (base DATA_TYPE, signedness) key that decides
// whether a column-name group has drifted. Display width (int(10) vs int(11))
// and string length (varchar(64) vs varchar(255)) are intentionally excluded —
// they compare natively and would only add noise.
func columnTypeSignature(dataType string, unsigned bool) string {
	if unsigned {
		return strings.ToLower(dataType) + "|u"
	}
	return strings.ToLower(dataType) + "|s"
}

// classifyColumnTypeDrift maps a flagged group's distinct-base-type count to a
// severity. Only called for already-drifted groups (>= 2 distinct signatures):
//
//	high   = >= 2 distinct base DATA_TYPEs (int vs bigint, number vs varchar) —
//	         a hard implicit conversion that defeats the index on the JOIN.
//	medium = a single base type but mixed signedness (all int, some UNSIGNED some
//	         SIGNED) — the subtler signed/unsigned index-defeat + sign-boundary
//	         misorder.
func classifyColumnTypeDrift(distinctBaseTypes int) string {
	if distinctBaseTypes >= 2 {
		return "high"
	}
	return "medium"
}

// columnTypeDriftSeverityRank orders severities worst-first for the candidate
// sort and the minSeverity threshold comparison (lower rank = more severe).
// Mirrors engineSeverityRank so the shared severityName inverse stays valid.
func columnTypeDriftSeverityRank(sev string) int {
	switch sev {
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

// collectColumnTypeDrift runs one parameterized pass over
// information_schema.COLUMNS joined to information_schema.TABLES (BASE TABLE
// only), groups columns by (schema, name), and flags every name whose declared
// (base type, signedness) signature is not uniform across its tables. The IN
// clause is built from ? placeholders — no interpolation of caller input.
func collectColumnTypeDrift(ctx context.Context, db *sql.DB, now time.Time, schemas []string, minRank int) (map[string]any, error) {
	if len(schemas) == 0 {
		return nil, fmt.Errorf("no schemas specified")
	}
	placeholders := make([]string, len(schemas))
	args := make([]any, len(schemas))
	for i, s := range schemas {
		placeholders[i] = "?"
		args[i] = s
	}
	// The JOIN to TABLES scopes to BASE TABLEs (drops VIEWs, whose columns would
	// inflate the drift set without ever participating in a base-table JOIN).
	// ORDER BY groups a column name's rows together and keeps output deterministic.
	q := `SELECT c.TABLE_SCHEMA, c.TABLE_NAME, c.COLUMN_NAME, c.DATA_TYPE, c.COLUMN_TYPE
		FROM information_schema.COLUMNS c
		JOIN information_schema.TABLES t
		  ON c.TABLE_SCHEMA = t.TABLE_SCHEMA AND c.TABLE_NAME = t.TABLE_NAME
		WHERE t.TABLE_TYPE = 'BASE TABLE'
		  AND c.TABLE_SCHEMA IN (` + strings.Join(placeholders, ",") + `)
		ORDER BY c.TABLE_SCHEMA, c.COLUMN_NAME, c.TABLE_NAME`
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("query information_schema.COLUMNS: %w", err)
	}
	defer rows.Close()

	// byDBCol[schema][columnName] accumulates the type variants of that name.
	byDBCol := make(map[string]map[string]*columnNameAgg, len(schemas))
	scannedPerDB := make(map[string]int, len(schemas))
	for _, s := range schemas {
		byDBCol[s] = map[string]*columnNameAgg{}
		scannedPerDB[s] = 0
	}

	for rows.Next() {
		var r columnTypeRow
		if err := rows.Scan(&r.Schema, &r.Table, &r.Column, &r.DataType, &r.ColumnType); err != nil {
			return nil, fmt.Errorf("scan COLUMNS row: %w", err)
		}
		cols := byDBCol[r.Schema]
		if cols == nil {
			// A schema outside the pre-seeded set should be impossible (the IN
			// list bounds the result), but stay defensive rather than panic.
			cols = map[string]*columnNameAgg{}
			byDBCol[r.Schema] = cols
		}
		scannedPerDB[r.Schema]++
		g := cols[r.Column]
		if g == nil {
			g = newColumnNameAgg()
			cols[r.Column] = g
		}
		unsigned := columnTypeUnsigned(r.ColumnType)
		v := g.variants[r.ColumnType]
		if v == nil {
			v = &columnTypeVariant{DataType: r.DataType, ColumnType: r.ColumnType, Unsigned: unsigned}
			g.variants[r.ColumnType] = v
		}
		v.TableCount++
		v.Tables = append(v.Tables, r.Table)
		g.signatures[columnTypeSignature(r.DataType, unsigned)] = struct{}{}
		g.baseTypes[strings.ToLower(r.DataType)] = struct{}{}
		g.tableCount++
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate COLUMNS: %w", err)
	}

	// Pre-seed perDatabase for every scanned schema so an empty schema still
	// surfaces. driftCount is filled in the flagging pass below.
	byDB := make(map[string]*columnTypeDriftPerDatabase, len(schemas))
	for _, s := range schemas {
		byDB[s] = &columnTypeDriftPerDatabase{
			Database:            s,
			ColumnsScanned:      scannedPerDB[s],
			DistinctColumnNames: len(byDBCol[s]),
		}
	}

	// Flag drifted column-name groups. driftCount (per-schema + totals) counts
	// ALL drifted names honestly; the minSeverity filter only trims the LIST.
	candidates := make([]columnTypeDriftRow, 0, 32)
	totalDrift := 0
	for schema, cols := range byDBCol {
		for name, g := range cols {
			if len(g.signatures) < 2 {
				continue // uniform type across all tables — not a drift
			}
			totalDrift++
			if d := byDB[schema]; d != nil {
				d.DriftCount++
			}
			sev := classifyColumnTypeDrift(len(g.baseTypes))
			if columnTypeDriftSeverityRank(sev) > minRank {
				continue // below the operator's minSeverity threshold
			}
			candidates = append(candidates, columnTypeDriftRow{
				Database:           schema,
				Column:             name,
				DistinctSignatures: len(g.signatures),
				TableCount:         g.tableCount,
				Variants:           finalizeColumnTypeVariants(g.variants),
				Severity:           sev,
				Reason:             columnTypeDriftReason(sev, name, g),
			})
		}
	}

	// Worst-first: severity, then widest blast radius (most tables affected),
	// then most distinct signatures, then database/column asc for determinism
	// (defends against Go's randomized map-iteration order).
	sort.SliceStable(candidates, func(i, j int) bool {
		if ri, rj := columnTypeDriftSeverityRank(candidates[i].Severity), columnTypeDriftSeverityRank(candidates[j].Severity); ri != rj {
			return ri < rj
		}
		if candidates[i].TableCount != candidates[j].TableCount {
			return candidates[i].TableCount > candidates[j].TableCount
		}
		if candidates[i].DistinctSignatures != candidates[j].DistinctSignatures {
			return candidates[i].DistinctSignatures > candidates[j].DistinctSignatures
		}
		if candidates[i].Database != candidates[j].Database {
			return candidates[i].Database < candidates[j].Database
		}
		return candidates[i].Column < candidates[j].Column
	})

	truncated := false
	if len(candidates) > dbColumnTypeDriftMaxCandidates {
		candidates = candidates[:dbColumnTypeDriftMaxCandidates]
		truncated = true
	}

	var totalScanned int
	perDB := make([]columnTypeDriftPerDatabase, 0, len(byDB))
	for _, d := range byDB {
		totalScanned += d.ColumnsScanned
		perDB = append(perDB, *d)
	}
	sort.Slice(perDB, func(i, j int) bool {
		if perDB[i].DriftCount != perDB[j].DriftCount {
			return perDB[i].DriftCount > perDB[j].DriftCount
		}
		return perDB[i].Database < perDB[j].Database
	})

	out := map[string]any{
		"asOf":           now.UTC().Format(time.RFC3339),
		"scannedSchemas": schemas,
		"candidates":     candidates,
		"perDatabase":    perDB,
		"truncated":      truncated,
		"totals": map[string]any{
			"columnsScanned": totalScanned,
			"driftCount":     totalDrift,
		},
	}
	if minRank != columnTypeDriftSeverityRank("low") {
		out["minSeverity"] = severityName(minRank)
	}
	return out, nil
}

// finalizeColumnTypeVariants turns the per-COLUMN_TYPE map into a deterministic,
// size-bounded slice: each variant's example tables sorted asc and clipped to
// dbColumnTypeDriftMaxTablesPerVariant (TableCount stays honest), and the
// variants themselves sorted dataType-asc then columnType-asc.
func finalizeColumnTypeVariants(variants map[string]*columnTypeVariant) []columnTypeVariant {
	out := make([]columnTypeVariant, 0, len(variants))
	for _, v := range variants {
		sort.Strings(v.Tables)
		if len(v.Tables) > dbColumnTypeDriftMaxTablesPerVariant {
			v.Tables = v.Tables[:dbColumnTypeDriftMaxTablesPerVariant]
		}
		out = append(out, *v)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].DataType != out[j].DataType {
			return out[i].DataType < out[j].DataType
		}
		return out[i].ColumnType < out[j].ColumnType
	})
	return out
}

// columnTypeDriftReason builds the human-facing explanation + remediation hint
// for one flagged column, naming the specific type variants so the operator sees
// the divergence inline without cross-referencing the variants list.
func columnTypeDriftReason(sev, column string, g *columnNameAgg) string {
	types := make([]string, 0, len(g.variants))
	for ct := range g.variants {
		types = append(types, ct)
	}
	sort.Strings(types)
	joined := strings.Join(types, ", ")
	if sev == "high" {
		return fmt.Sprintf("column %q is declared with %d different base types across %d tables in this schema (%s) — JOINing or comparing these columns forces MySQL to implicitly convert one side (number-vs-string, or a wider/narrower integer), which defeats the index on the converted column and can change comparison semantics. Align the type across tables with ALTER TABLE ... MODIFY (confirm with SHOW CREATE TABLE first).",
			column, len(g.baseTypes), g.tableCount, joined)
	}
	return fmt.Sprintf("column %q shares one base type across %d tables in this schema but mixes SIGNED and UNSIGNED (%s) — a signed/unsigned JOIN makes MySQL promote both sides to a wider type, defeating the index on the join column, and can misorder values near the sign boundary. Make the signedness consistent with ALTER TABLE ... MODIFY (confirm with SHOW CREATE TABLE first).",
		column, g.tableCount, joined)
}
