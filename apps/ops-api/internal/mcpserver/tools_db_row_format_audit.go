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
	dbRowFormatAuditTimeout = 10 * time.Second

	// dbRowFormatAuditMaxCandidates bounds the MCP envelope. AC's schemas
	// realistically yield a bounded set of legacy-format tables, so this is a
	// guard rail, not an expected limit; totals stays the honest pre-cap count.
	dbRowFormatAuditMaxCandidates = 500
)

// rowFormatTableRow is one raw InnoDB BASE TABLE row from information_schema.TABLES.
type rowFormatTableRow struct {
	Schema       string
	Table        string
	RowFormat    string
	RowsEstimate int64
	DataBytes    int64
	IndexBytes   int64
}

// rowFormatCount is one ROW_FORMAT's tally within a schema (the unit of the
// per-database rowFormatBreakdown).
type rowFormatCount struct {
	RowFormat    string `json:"rowFormat"`
	TableCount   int    `json:"tableCount"`
	RowsEstimate int64  `json:"rowsEstimate"`
	DataBytes    int64  `json:"dataBytes"`
	IndexBytes   int64  `json:"indexBytes"`
	TotalBytes   int64  `json:"totalBytes"`
	TotalHuman   string `json:"totalHuman"`
}

// rowFormatLegacyRow is one flagged InnoDB table sitting on a legacy Antelope row
// format. Bytes + rowsEstimate are surfaced so an operator can gauge the rebuild
// effort (an ALTER TABLE ... ROW_FORMAT=DYNAMIC rebuilds the whole table — a 2 GiB
// table is a much bigger window than a 4 KiB one).
type rowFormatLegacyRow struct {
	Database     string `json:"database"`
	Table        string `json:"table"`
	RowFormat    string `json:"rowFormat"`
	RowsEstimate int64  `json:"rowsEstimate"`
	DataBytes    int64  `json:"dataBytes"`
	IndexBytes   int64  `json:"indexBytes"`
	TotalBytes   int64  `json:"totalBytes"`
	TotalHuman   string `json:"totalHuman"`
	Severity     string `json:"severity"`
	Reason       string `json:"reason"`
}

// rowFormatPerDatabase rolls up the row-format landscape per scanned schema.
// Pre-seeded for every scanned schema so an empty / no-grant schema still
// surfaces (operators can't mistake "scanned, nothing flagged" for a bug). The
// rowFormatBreakdown is sorted tableCount-desc, so rowFormatBreakdown[0] is the
// schema's dominant format at a glance.
type rowFormatPerDatabase struct {
	Database           string           `json:"database"`
	TablesScanned      int              `json:"tablesScanned"`
	DistinctRowFormats int              `json:"distinctRowFormats"`
	RowFormatBreakdown []rowFormatCount `json:"rowFormatBreakdown"`
	LegacyCount        int              `json:"legacyCount"`
}

// rowFormatSchemaAgg accumulates one schema's per-row-format tallies during the
// scan.
type rowFormatSchemaAgg struct {
	byRowFormat   map[string]*rowFormatCount
	tablesScanned int
}

func newRowFormatSchemaAgg() *rowFormatSchemaAgg {
	return &rowFormatSchemaAgg{byRowFormat: map[string]*rowFormatCount{}}
}

// RegisterDBRowFormatAuditTool registers `db_row_format_audit` — the InnoDB
// on-disk-row-layout lens of the db_* suite. db_engine_distribution (#287) audits
// WHICH storage engine a table runs; this one drills INTO the InnoDB tables and
// asks a different question: which of them still use a legacy Antelope ROW_FORMAT
// (Compact / Redundant) instead of the modern Barracuda formats (Dynamic /
// Compressed). Unlike the engine audit's per-schema dominant-deviation heuristic
// (acore_world is MyISAM by design, so its engine norm is relative), row format
// has a single objective target — DYNAMIC — so this tool uses ABSOLUTE
// classification: every Antelope-format InnoDB table is flagged regardless of what
// the schema's majority format is (a schema that is entirely Compact is the WORST
// case, not the "uniform, nothing to do" case). Reuses the long-merged
// `dbSizeKnownSchemas` + `isKnownSizeSchema` allowlist (db_size_summary, #172) and
// the `severityName` helper (db_engine_distribution, #287).
func RegisterDBRowFormatAuditTool(reg *Registry, deps DBDeps) {
	reg.Register(Tool{
		Name: "db_row_format_audit",
		Description: "InnoDB ROW_FORMAT audit from information_schema.TABLES, " +
			"across acore_world / acore_characters / acore_auth. Scans only InnoDB " +
			"BASE TABLEs and flags every one still using a legacy Antelope row " +
			"format (Compact or Redundant) rather than a modern Barracuda format " +
			"(Dynamic or Compressed). Why it matters: the Antelope formats cap an " +
			"index key prefix at 767 bytes — a single-column index on a utf8mb4 " +
			"VARCHAR(192)+ column overflows it (192*4 = 768 > 767) and fails to " +
			"create — and store only the first 768 bytes of a long variable-length " +
			"column (VARCHAR/TEXT/BLOB) inline; Barracuda (Dynamic) raises the prefix " +
			"to 3072 bytes and stores long columns fully off-page with a 20-byte " +
			"pointer. AzerothCore's canonical dumps + years of module migrations " +
			"straddle the MySQL 5.6->5.7 default change, so format drift is real. " +
			"Severity: high = Redundant (the oldest format, least storage-efficient " +
			"— no compact NULL bitmap); medium = Compact (Antelope but the 5.0.3+ " +
			"default, still 767-byte-limited). Dynamic and Compressed are NOT flagged. " +
			"Each candidate carries data/index bytes + rowsEstimate to gauge the " +
			"rebuild window and a `reason` naming the fix: ALTER TABLE ... " +
			"ROW_FORMAT=DYNAMIC (confirm with SHOW TABLE STATUS / SHOW CREATE TABLE " +
			"first). `database` narrows to one schema (rejected, not ignored, when not " +
			"in the allowlist). `minSeverity` (low/medium/high, default low=all) trims " +
			"the candidate LIST to at-or-above that severity; perDatabase and totals " +
			"stay the honest full counts. acore_playerbots is intentionally OUT of " +
			"scope (ops_ro lacks SELECT there — blocked on `ops_grant_playerbots_ro`). " +
			"Complements db_engine_distribution (storage engine, not row layout) and " +
			"db_table_info (one table's status). Results sorted worst-first (severity, " +
			"then biggest table first), capped at 500 with `truncated`; `totals` " +
			"carries the honest pre-cap counts. Read-only, ops_ro pool, 10 s timeout.",
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
			minRank := rowFormatSeverityRank("low")
			if a.MinSeverity != "" {
				switch a.MinSeverity {
				case "low", "medium", "high":
					minRank = rowFormatSeverityRank(a.MinSeverity)
				default:
					return map[string]any{"error": "minSeverity must be one of low/medium/high"}
				}
			}
			c, cancel := context.WithTimeout(ctx, dbRowFormatAuditTimeout)
			defer cancel()
			out, err := collectRowFormatAudit(c, deps.QueryDB, time.Now(), schemas, minRank)
			if err != nil {
				return map[string]any{"error": err.Error()}
			}
			return out
		},
	})
}

// classifyRowFormat buckets an InnoDB table's ROW_FORMAT by storage-format
// modernity. The legacy Antelope formats (Redundant, Compact) cap the index key
// prefix at 767 bytes (a single-column index on a utf8mb4 VARCHAR(192)+ column
// overflows it: 192*4 = 768) and store only the first 768 bytes of a long
// variable-length column inline. The Barracuda formats (Dynamic, Compressed)
// raise the prefix to 3072 bytes and store long columns fully off-page. Severity:
//
//	high   = Redundant — the oldest format (pre-5.0.3), least storage-efficient
//	         (no compact NULL bitmap, fixed-width row header); migrate first.
//	medium = Compact — Antelope but the 5.0.3+ default; still 767-byte-limited.
//
// Returns ("", false) for the modern Barracuda formats (Dynamic, Compressed) and
// for any unrecognized/empty value — don't flag a layout we can't classify.
// Case-insensitive (information_schema reports "Dynamic" / "Compact" / ...).
func classifyRowFormat(rowFormat string) (string, bool) {
	switch strings.ToUpper(strings.TrimSpace(rowFormat)) {
	case "REDUNDANT":
		return "high", true
	case "COMPACT":
		return "medium", true
	default:
		// DYNAMIC, COMPRESSED, "", or anything unrecognized → not flagged.
		return "", false
	}
}

// rowFormatSeverityRank orders severities worst-first for the candidate sort and
// for the minSeverity threshold comparison (lower rank = more severe). Mirrors
// engineSeverityRank so the shared severityName inverse stays valid.
func rowFormatSeverityRank(sev string) int {
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

// rowFormatReason builds the human-facing explanation + remediation hint for one
// flagged table.
func rowFormatReason(sev, rowFormat string) string {
	switch sev {
	case "high":
		return fmt.Sprintf("InnoDB table uses the legacy Antelope ROW_FORMAT=%s — the oldest, least storage-efficient InnoDB layout. Antelope caps the index key prefix at 767 bytes (a single-column index on a utf8mb4 VARCHAR(192)+ column won't fit) and stores only the first 768 bytes of a long variable-length column inline. Rebuild to the modern Barracuda layout with ALTER TABLE ... ROW_FORMAT=DYNAMIC (confirm with SHOW TABLE STATUS / SHOW CREATE TABLE first; the ALTER rebuilds the whole table).",
			rowFormat)
	default: // medium / Compact
		return fmt.Sprintf("InnoDB table uses the legacy Antelope ROW_FORMAT=%s — caps the index key prefix at 767 bytes (a utf8mb4 VARCHAR(192)+ single-column index overflows it) and stores only the first 768 bytes of a long variable-length column inline. Rebuild to the modern Barracuda layout with ALTER TABLE ... ROW_FORMAT=DYNAMIC (confirm with SHOW TABLE STATUS / SHOW CREATE TABLE first; the ALTER rebuilds the whole table).",
			rowFormat)
	}
}

// collectRowFormatAudit runs one parameterized pass over information_schema.TABLES
// (InnoDB BASE TABLEs only), tallies each schema's per-row-format populations, then
// flags every table on a legacy Antelope format. ABSOLUTE classification — unlike
// db_engine_distribution there is no per-schema dominant; the Antelope formats are
// flagworthy on their own merits regardless of the schema majority. The IN clause
// is built from ? placeholders — no interpolation of caller input.
func collectRowFormatAudit(ctx context.Context, db *sql.DB, now time.Time, schemas []string, minRank int) (map[string]any, error) {
	if len(schemas) == 0 {
		return nil, fmt.Errorf("no schemas specified")
	}
	placeholders := make([]string, len(schemas))
	args := make([]any, len(schemas))
	for i, s := range schemas {
		placeholders[i] = "?"
		args[i] = s
	}
	// ENGINE = 'InnoDB' scopes to exactly the tables for which ROW_FORMAT carries
	// the Antelope/Barracuda meaning (MyISAM's Fixed/Dynamic/Compressed formats are
	// an unrelated concept, and db_engine_distribution already audits non-InnoDB
	// tables). BASE TABLE drops VIEWs. IFNULL guards the rare format-less row.
	q := `SELECT TABLE_SCHEMA, TABLE_NAME, IFNULL(ROW_FORMAT,''),
			IFNULL(TABLE_ROWS,0), IFNULL(DATA_LENGTH,0), IFNULL(INDEX_LENGTH,0)
		FROM information_schema.TABLES
		WHERE TABLE_TYPE = 'BASE TABLE'
		  AND ENGINE = 'InnoDB'
		  AND TABLE_SCHEMA IN (` + strings.Join(placeholders, ",") + `)
		ORDER BY TABLE_SCHEMA, TABLE_NAME`
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("query information_schema.TABLES: %w", err)
	}
	defer rows.Close()

	agg := make(map[string]*rowFormatSchemaAgg, len(schemas))
	for _, s := range schemas {
		agg[s] = newRowFormatSchemaAgg()
	}
	all := make([]rowFormatTableRow, 0, 256)

	for rows.Next() {
		var r rowFormatTableRow
		if err := rows.Scan(&r.Schema, &r.Table, &r.RowFormat, &r.RowsEstimate, &r.DataBytes, &r.IndexBytes); err != nil {
			return nil, fmt.Errorf("scan TABLES row: %w", err)
		}
		all = append(all, r)
		a := agg[r.Schema]
		if a == nil {
			// A schema outside the pre-seeded set should be impossible (the IN
			// list bounds the result), but stay defensive rather than panic.
			a = newRowFormatSchemaAgg()
			agg[r.Schema] = a
		}
		a.tablesScanned++
		rc := a.byRowFormat[r.RowFormat]
		if rc == nil {
			rc = &rowFormatCount{RowFormat: r.RowFormat}
			a.byRowFormat[r.RowFormat] = rc
		}
		rc.TableCount++
		rc.RowsEstimate += r.RowsEstimate
		rc.DataBytes += r.DataBytes
		rc.IndexBytes += r.IndexBytes
		rc.TotalBytes += r.DataBytes + r.IndexBytes
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate TABLES: %w", err)
	}

	// Pre-seed perDatabase for every scanned schema (empty schema still surfaces)
	// and build each schema's rowFormatBreakdown sorted by table count desc.
	byDB := make(map[string]*rowFormatPerDatabase, len(schemas))
	for _, s := range schemas {
		a := agg[s]
		breakdown := make([]rowFormatCount, 0, len(a.byRowFormat))
		for _, rc := range a.byRowFormat {
			rc.TotalHuman = humanBytes(uint64Of(rc.TotalBytes))
			breakdown = append(breakdown, *rc)
		}
		sort.Slice(breakdown, func(i, j int) bool {
			if breakdown[i].TableCount != breakdown[j].TableCount {
				return breakdown[i].TableCount > breakdown[j].TableCount
			}
			return breakdown[i].RowFormat < breakdown[j].RowFormat
		})
		byDB[s] = &rowFormatPerDatabase{
			Database:           s,
			TablesScanned:      a.tablesScanned,
			DistinctRowFormats: len(a.byRowFormat),
			RowFormatBreakdown: breakdown,
		}
	}

	// Flag legacy-format tables. legacyCount (per-schema + totals) counts ALL
	// Antelope tables honestly; the minSeverity filter only trims the LIST below.
	candidates := make([]rowFormatLegacyRow, 0, 32)
	totalLegacy := 0
	for _, r := range all {
		sev, legacy := classifyRowFormat(r.RowFormat)
		if !legacy {
			continue
		}
		totalLegacy++
		if d := byDB[r.Schema]; d != nil {
			d.LegacyCount++
		}
		if rowFormatSeverityRank(sev) > minRank {
			continue // below the operator's minSeverity threshold
		}
		total := r.DataBytes + r.IndexBytes
		candidates = append(candidates, rowFormatLegacyRow{
			Database:     r.Schema,
			Table:        r.Table,
			RowFormat:    r.RowFormat,
			RowsEstimate: r.RowsEstimate,
			DataBytes:    r.DataBytes,
			IndexBytes:   r.IndexBytes,
			TotalBytes:   total,
			TotalHuman:   humanBytes(uint64Of(total)),
			Severity:     sev,
			Reason:       rowFormatReason(sev, r.RowFormat),
		})
	}

	// Worst-first: severity, then bigger tables first (a ROW_FORMAT ALTER rebuilds
	// the whole table, so the biggest is the longest window), then schema/table asc
	// for determinism.
	sort.SliceStable(candidates, func(i, j int) bool {
		if ri, rj := rowFormatSeverityRank(candidates[i].Severity), rowFormatSeverityRank(candidates[j].Severity); ri != rj {
			return ri < rj
		}
		if candidates[i].TotalBytes != candidates[j].TotalBytes {
			return candidates[i].TotalBytes > candidates[j].TotalBytes
		}
		if candidates[i].Database != candidates[j].Database {
			return candidates[i].Database < candidates[j].Database
		}
		return candidates[i].Table < candidates[j].Table
	})

	truncated := false
	if len(candidates) > dbRowFormatAuditMaxCandidates {
		candidates = candidates[:dbRowFormatAuditMaxCandidates]
		truncated = true
	}

	var totalScanned int
	perDB := make([]rowFormatPerDatabase, 0, len(byDB))
	for _, d := range byDB {
		totalScanned += d.TablesScanned
		perDB = append(perDB, *d)
	}
	sort.Slice(perDB, func(i, j int) bool {
		if perDB[i].LegacyCount != perDB[j].LegacyCount {
			return perDB[i].LegacyCount > perDB[j].LegacyCount
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
			"tablesScanned": totalScanned,
			"legacyCount":   totalLegacy,
		},
	}
	if minRank != rowFormatSeverityRank("low") {
		out["minSeverity"] = severityName(minRank)
	}
	return out, nil
}
