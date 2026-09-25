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
	dbCharsetCollationAuditTimeout = 10 * time.Second

	// dbCharsetCollationAuditMaxCandidates bounds the MCP envelope. AC's schemas
	// realistically yield a bounded set of mismatched columns, so this is a guard
	// rail, not an expected limit; totals stays the honest pre-cap count.
	dbCharsetCollationAuditMaxCandidates = 500
)

// charsetColRow is one raw string column from information_schema.COLUMNS joined
// to TABLES (only columns with a non-NULL CHARACTER_SET_NAME — i.e. CHAR /
// VARCHAR / TEXT / ENUM / SET — survive the WHERE, so numeric/date/blob columns
// never appear).
type charsetColRow struct {
	Schema         string
	Table          string
	Column         string
	DataType       string
	ColumnType     string
	Charset        string
	Collation      string
	TableCollation string
	Engine         string
}

// charsetMismatchRow is one flagged column whose charset or collation deviates
// from its schema's dominant pairing — the implicit-conversion JOIN risk.
type charsetMismatchRow struct {
	Database             string `json:"database"`
	Table                string `json:"table"`
	Column               string `json:"column"`
	DataType             string `json:"dataType"`
	ColumnType           string `json:"columnType"`
	Charset              string `json:"charset"`
	Collation            string `json:"collation"`
	TableCollation       string `json:"tableCollation"`
	Engine               string `json:"engine"`
	DominantCharset      string `json:"dominantCharset"`
	DominantCollation    string `json:"dominantCollation"`
	ColumnOverridesTable bool   `json:"columnOverridesTable"`
	Severity             string `json:"severity"`
	Reason               string `json:"reason"`
}

// charsetPerDatabase rolls up the charset/collation landscape per scanned
// schema. Pre-seeded for every scanned schema so an empty / no-grant schema
// still surfaces (operators can't mistake "scanned, nothing flagged" for a bug).
type charsetPerDatabase struct {
	Database             string `json:"database"`
	StringColumnsScanned int    `json:"stringColumnsScanned"`
	DistinctCharsets     int    `json:"distinctCharsets"`
	DistinctCollations   int    `json:"distinctCollations"`
	DominantCharset      string `json:"dominantCharset"`
	DominantCollation    string `json:"dominantCollation"`
	MismatchCount        int    `json:"mismatchCount"`
}

// charsetSchemaAgg accumulates one schema's string-column charset/collation
// tallies during the scan so the dominant pairing can be computed before the
// flagging pass.
type charsetSchemaAgg struct {
	charsetCounts        map[string]int            // charset -> column count
	collationByCharset   map[string]map[string]int // charset -> collation -> count
	stringColumnsScanned int
}

func newCharsetSchemaAgg() *charsetSchemaAgg {
	return &charsetSchemaAgg{
		charsetCounts:      map[string]int{},
		collationByCharset: map[string]map[string]int{},
	}
}

// RegisterDBCharsetCollationAuditTool registers `db_charset_collation_audit` —
// the charset/collation-consistency lens of the db_* suite. The index-hygiene
// tools (db_size_summary / db_table_bloat / db_redundant_indexes /
// db_unindexed_tables / db_low_cardinality_indexes) all reason about indexes;
// this one finds a different, subtler performance footgun: a string column whose
// character set or collation differs from the bulk of its schema. When such a
// column is compared or JOINed against a dominant-charset/collation column,
// MySQL must implicitly convert one side ("Illegal mix of collations" errors, or
// a silent conversion that makes the index on the converted side unusable).
// Reuses the long-merged `dbSizeKnownSchemas` + `isKnownSizeSchema` allowlist
// (db_size_summary, #172).
func RegisterDBCharsetCollationAuditTool(reg *Registry, deps DBDeps) {
	reg.Register(Tool{
		Name: "db_charset_collation_audit",
		Description: "Character-set / collation-consistency audit from " +
			"information_schema.COLUMNS joined to information_schema.TABLES, across " +
			"acore_world / acore_characters / acore_auth. Scans every string column " +
			"(CHAR/VARCHAR/TEXT/ENUM/SET — the ones with a non-NULL " +
			"CHARACTER_SET_NAME) and flags those whose CHARACTER_SET_NAME or " +
			"COLLATION_NAME deviates from the schema's DOMINANT (most-common) " +
			"pairing. Why it matters: comparing or JOINing two string columns with " +
			"different collations forces MySQL to implicitly convert one side — " +
			"either an outright `Illegal mix of collations` error or a silent " +
			"conversion that makes the index on the converted side unusable (a " +
			"full-scan JOIN). AzerothCore is a classic case: legacy core tables are " +
			"often utf8mb3 while newer module tables default to utf8mb4, so " +
			"cross-module JOINs hit exactly this. Severity: high = charset differs " +
			"from the schema's dominant charset (cross-charset conversion); medium " +
			"= same charset but a different collation within it. Each candidate " +
			"carries its `charset`, `collation`, the owning table's `tableCollation` " +
			"(with `columnOverridesTable` when the column has an explicit COLLATE " +
			"override), the schema's `dominantCharset`/`dominantCollation`, and a " +
			"`reason`. This flags DEVIATION from the per-schema majority as a " +
			"heuristic — a schema split between two large populations will flag the " +
			"smaller; confirm the specific JOIN with SHOW CREATE TABLE before ALTER " +
			"... CONVERT TO CHARACTER SET. Complements db_size_summary (index bytes) " +
			"and db_table_info (which shows ONE table's collation via SHOW TABLE " +
			"STATUS but never compares across the schema). `database` narrows to one " +
			"schema (rejected, not ignored, when not in the allowlist). " +
			"acore_playerbots is intentionally OUT of scope (ops_ro lacks SELECT " +
			"there — blocked on `ops_grant_playerbots_ro`). `perDatabase` reports " +
			"each schema's stringColumnsScanned / distinctCharsets / " +
			"distinctCollations / dominant pairing / mismatchCount; results sorted " +
			"worst-first (severity, then database/table/column), capped at 500 with " +
			"`truncated`; `totals` carries the honest pre-cap counts. Read-only, " +
			"ops_ro pool, 10 s timeout.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{
			"database":{"type":"string","enum":["acore_world","acore_characters","acore_auth"],"description":"Limit to one schema (default: all three). acore_playerbots is out of scope (ops_ro lacks SELECT)."}
		}}`),
		Annotations: AnnRead(),
		Handler: func(ctx context.Context, raw json.RawMessage, _ string) any {
			var a struct {
				Database string `json:"database"`
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
			c, cancel := context.WithTimeout(ctx, dbCharsetCollationAuditTimeout)
			defer cancel()
			out, err := collectCharsetCollationAudit(c, deps.QueryDB, time.Now(), schemas)
			if err != nil {
				return map[string]any{"error": err.Error()}
			}
			return out
		},
	})
}

// dominantString returns the map key with the highest count, breaking ties by
// the lexicographically smallest key so the choice is deterministic (Go map
// iteration order is randomized). Empty map → "".
func dominantString(counts map[string]int) string {
	best := ""
	bestN := -1
	for k, n := range counts {
		if n > bestN || (n == bestN && k < best) {
			best, bestN = k, n
		}
	}
	return best
}

// classifyCharsetCollation buckets a column against its schema's dominant
// pairing. high = charset differs from the dominant charset (a cross-charset
// implicit conversion). medium = same charset but a different collation within
// it (collation coercion). Returns ("", false) when the column matches the
// dominant pairing (healthy, not flagged).
func classifyCharsetCollation(colCharset, colCollation, domCharset, domCollation string) (string, bool) {
	if colCharset != domCharset {
		return "high", true
	}
	if colCollation != domCollation {
		return "medium", true
	}
	return "", false
}

// charsetSeverityRank orders severities worst-first for the candidate sort.
func charsetSeverityRank(sev string) int {
	switch sev {
	case "high":
		return 0
	case "medium":
		return 1
	default:
		return 2
	}
}

// collectCharsetCollationAudit runs one parameterized pass over
// information_schema.COLUMNS joined to information_schema.TABLES, tallies each
// schema's charset/collation populations, computes the dominant pairing per
// schema (dominant collation is scoped to the dominant charset so the two stay
// consistent), then flags every column that deviates. The IN clause is built
// from ? placeholders — no interpolation of caller input.
func collectCharsetCollationAudit(ctx context.Context, db *sql.DB, now time.Time, schemas []string) (map[string]any, error) {
	if len(schemas) == 0 {
		return nil, fmt.Errorf("no schemas specified")
	}
	placeholders := make([]string, len(schemas))
	args := make([]any, len(schemas))
	for i, s := range schemas {
		placeholders[i] = "?"
		args[i] = s
	}
	// Only string columns carry a character set; CHARACTER_SET_NAME IS NOT NULL
	// restricts to exactly CHAR/VARCHAR/TEXT/ENUM/SET (numeric/date/blob columns
	// have NULL charset). The JOIN to TABLES scopes to BASE TABLEs (drops VIEWs)
	// and carries the owning table's default collation + engine. ORDER BY keeps
	// output deterministic and groups a table's columns together.
	q := `SELECT c.TABLE_SCHEMA, c.TABLE_NAME, c.COLUMN_NAME,
			c.DATA_TYPE, c.COLUMN_TYPE, c.CHARACTER_SET_NAME, c.COLLATION_NAME,
			IFNULL(t.TABLE_COLLATION, ''), IFNULL(t.ENGINE, '')
		FROM information_schema.COLUMNS c
		JOIN information_schema.TABLES t
		  ON c.TABLE_SCHEMA = t.TABLE_SCHEMA AND c.TABLE_NAME = t.TABLE_NAME
		WHERE t.TABLE_TYPE = 'BASE TABLE'
		  AND c.CHARACTER_SET_NAME IS NOT NULL
		  AND c.TABLE_SCHEMA IN (` + strings.Join(placeholders, ",") + `)
		ORDER BY c.TABLE_SCHEMA, c.TABLE_NAME, c.ORDINAL_POSITION`
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("query information_schema.COLUMNS: %w", err)
	}
	defer rows.Close()

	agg := make(map[string]*charsetSchemaAgg, len(schemas))
	for _, s := range schemas {
		agg[s] = newCharsetSchemaAgg()
	}
	all := make([]charsetColRow, 0, 256)

	for rows.Next() {
		var r charsetColRow
		if err := rows.Scan(&r.Schema, &r.Table, &r.Column, &r.DataType, &r.ColumnType,
			&r.Charset, &r.Collation, &r.TableCollation, &r.Engine); err != nil {
			return nil, fmt.Errorf("scan COLUMNS row: %w", err)
		}
		all = append(all, r)
		a := agg[r.Schema]
		if a == nil {
			// A schema outside the pre-seeded set should be impossible (the IN
			// list bounds the result), but stay defensive rather than panic.
			a = newCharsetSchemaAgg()
			agg[r.Schema] = a
		}
		a.stringColumnsScanned++
		a.charsetCounts[r.Charset]++
		if a.collationByCharset[r.Charset] == nil {
			a.collationByCharset[r.Charset] = map[string]int{}
		}
		a.collationByCharset[r.Charset][r.Collation]++
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate COLUMNS: %w", err)
	}

	// Compute the dominant pairing per schema. Dominant collation is the most
	// common collation AMONG columns of the dominant charset — keeping the two
	// consistent (a collation always belongs to exactly one charset), so a
	// column with the dominant charset but a minority collation reads as a true
	// within-charset split, not a phantom mismatch.
	domCharset := make(map[string]string, len(agg))
	domCollation := make(map[string]string, len(agg))
	for s, a := range agg {
		dc := dominantString(a.charsetCounts)
		domCharset[s] = dc
		domCollation[s] = dominantString(a.collationByCharset[dc])
	}

	candidates := make([]charsetMismatchRow, 0, 32)
	byDB := make(map[string]*charsetPerDatabase, len(schemas))
	for _, s := range schemas {
		a := agg[s]
		distinctCollations := 0
		for _, m := range a.collationByCharset {
			distinctCollations += len(m)
		}
		byDB[s] = &charsetPerDatabase{
			Database:             s,
			StringColumnsScanned: a.stringColumnsScanned,
			DistinctCharsets:     len(a.charsetCounts),
			DistinctCollations:   distinctCollations,
			DominantCharset:      domCharset[s],
			DominantCollation:    domCollation[s],
		}
	}

	for _, r := range all {
		dc, dcoll := domCharset[r.Schema], domCollation[r.Schema]
		sev, mismatch := classifyCharsetCollation(r.Charset, r.Collation, dc, dcoll)
		if !mismatch {
			continue
		}
		if d := byDB[r.Schema]; d != nil {
			d.MismatchCount++
		}
		overrides := r.TableCollation != "" && r.Collation != r.TableCollation
		candidates = append(candidates, charsetMismatchRow{
			Database:             r.Schema,
			Table:                r.Table,
			Column:               r.Column,
			DataType:             r.DataType,
			ColumnType:           r.ColumnType,
			Charset:              r.Charset,
			Collation:            r.Collation,
			TableCollation:       r.TableCollation,
			Engine:               r.Engine,
			DominantCharset:      dc,
			DominantCollation:    dcoll,
			ColumnOverridesTable: overrides,
			Severity:             sev,
			Reason:               charsetMismatchReason(sev, r, dc, dcoll, overrides),
		})
	}

	// Worst-first: severity (high before medium), then schema/table/column asc
	// for determinism (groups a schema's mismatches together, readable).
	sort.SliceStable(candidates, func(i, j int) bool {
		if ri, rj := charsetSeverityRank(candidates[i].Severity), charsetSeverityRank(candidates[j].Severity); ri != rj {
			return ri < rj
		}
		if candidates[i].Database != candidates[j].Database {
			return candidates[i].Database < candidates[j].Database
		}
		if candidates[i].Table != candidates[j].Table {
			return candidates[i].Table < candidates[j].Table
		}
		return candidates[i].Column < candidates[j].Column
	})

	totalMismatch := len(candidates)
	truncated := false
	if len(candidates) > dbCharsetCollationAuditMaxCandidates {
		candidates = candidates[:dbCharsetCollationAuditMaxCandidates]
		truncated = true
	}

	var totalScanned int
	perDB := make([]charsetPerDatabase, 0, len(byDB))
	for _, d := range byDB {
		totalScanned += d.StringColumnsScanned
		perDB = append(perDB, *d)
	}
	sort.Slice(perDB, func(i, j int) bool {
		if perDB[i].MismatchCount != perDB[j].MismatchCount {
			return perDB[i].MismatchCount > perDB[j].MismatchCount
		}
		return perDB[i].Database < perDB[j].Database
	})

	return map[string]any{
		"asOf":           now.UTC().Format(time.RFC3339),
		"scannedSchemas": schemas,
		"candidates":     candidates,
		"perDatabase":    perDB,
		"truncated":      truncated,
		"totals": map[string]any{
			"stringColumnsScanned": totalScanned,
			"mismatchCount":        totalMismatch,
		},
	}, nil
}

// charsetMismatchReason builds the human-facing explanation + remediation hint
// for one flagged column. High = cross-charset; medium = within-charset
// collation split. Appends a note when the column has an explicit COLLATE
// override of its table default.
func charsetMismatchReason(sev string, r charsetColRow, domCharset, domCollation string, overrides bool) string {
	var b strings.Builder
	if sev == "high" {
		fmt.Fprintf(&b, "column charset %q differs from this schema's dominant charset %q — comparing or JOINing it against a dominant-charset column forces an implicit character-set conversion (\"Illegal mix of collations\" errors, or a silent conversion that makes the index on the converted side unusable).",
			r.Charset, domCharset)
	} else {
		fmt.Fprintf(&b, "column collation %q differs from this schema's dominant collation %q (same charset %q) — comparisons/JOINs against dominant-collation columns coerce collation and can defeat index use.",
			r.Collation, domCollation, r.Charset)
	}
	if overrides {
		fmt.Fprintf(&b, " The column has an explicit COLLATE override of its table default %q.", r.TableCollation)
	}
	b.WriteString(" Confirm with SHOW CREATE TABLE before ALTER ... CONVERT TO CHARACTER SET.")
	return b.String()
}
