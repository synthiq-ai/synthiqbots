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
	dbEngineDistributionTimeout = 10 * time.Second

	// dbEngineDistributionMaxCandidates bounds the MCP envelope. AC's schemas
	// realistically yield a bounded set of engine-deviating tables, so this is a
	// guard rail, not an expected limit; totals stays the honest pre-cap count.
	dbEngineDistributionMaxCandidates = 500
)

// engineTableRow is one raw BASE TABLE row from information_schema.TABLES.
type engineTableRow struct {
	Schema       string
	Table        string
	Engine       string
	RowsEstimate int64
	DataBytes    int64
	IndexBytes   int64
}

// engineCount is one storage engine's tally within a schema (the unit of the
// per-database engineBreakdown).
type engineCount struct {
	Engine       string `json:"engine"`
	TableCount   int    `json:"tableCount"`
	RowsEstimate int64  `json:"rowsEstimate"`
	DataBytes    int64  `json:"dataBytes"`
	IndexBytes   int64  `json:"indexBytes"`
	TotalBytes   int64  `json:"totalBytes"`
	TotalHuman   string `json:"totalHuman"`
}

// engineDeviationRow is one flagged table running an engine that differs from
// its schema's dominant engine. Bytes + rowsEstimate are surfaced so an operator
// can gauge the migration effort (a 2 GiB MyISAM table is a bigger ALTER than a
// 4 KiB one).
type engineDeviationRow struct {
	Database       string `json:"database"`
	Table          string `json:"table"`
	Engine         string `json:"engine"`
	DominantEngine string `json:"dominantEngine"`
	RowsEstimate   int64  `json:"rowsEstimate"`
	DataBytes      int64  `json:"dataBytes"`
	IndexBytes     int64  `json:"indexBytes"`
	TotalBytes     int64  `json:"totalBytes"`
	TotalHuman     string `json:"totalHuman"`
	Severity       string `json:"severity"`
	Reason         string `json:"reason"`
}

// enginePerDatabase rolls up the storage-engine landscape per scanned schema.
// Pre-seeded for every scanned schema so an empty / no-grant schema still
// surfaces (operators can't mistake "scanned, nothing flagged" for a bug).
type enginePerDatabase struct {
	Database        string        `json:"database"`
	TablesScanned   int           `json:"tablesScanned"`
	DistinctEngines int           `json:"distinctEngines"`
	DominantEngine  string        `json:"dominantEngine"`
	EngineBreakdown []engineCount `json:"engineBreakdown"`
	DeviatingCount  int           `json:"deviatingCount"`
}

// engineSchemaAgg accumulates one schema's per-engine tallies during the scan so
// the dominant engine can be computed before the flagging pass.
type engineSchemaAgg struct {
	byEngine      map[string]*engineCount
	tablesScanned int
}

func newEngineSchemaAgg() *engineSchemaAgg {
	return &engineSchemaAgg{byEngine: map[string]*engineCount{}}
}

// RegisterDBEngineDistributionTool registers `db_engine_distribution` — the
// storage-engine-consistency lens of the db_* suite. The index-hygiene tools
// (db_size_summary / db_table_bloat / db_redundant_indexes / db_unindexed_tables
// / db_low_cardinality_indexes / db_auto_increment_headroom) reason about size
// and indexes; the charset audit (db_charset_collation_audit) reasons about
// collations; this one answers a different question: which tables run a storage
// engine that differs from the bulk of their schema. A MyISAM table sitting in
// an otherwise-InnoDB schema (acore_characters / acore_auth) is a real liability
// — no crash recovery (corruption on unclean shutdown), table-level locking
// (concurrency killer on a write path), and no transactions/foreign keys.
// Reuses the long-merged `dbSizeKnownSchemas` + `isKnownSizeSchema` allowlist
// (db_size_summary, #172) and the `dominantString` helper
// (db_charset_collation_audit, #280).
func RegisterDBEngineDistributionTool(reg *Registry, deps DBDeps) {
	reg.Register(Tool{
		Name: "db_engine_distribution",
		Description: "Storage-engine distribution + deviation audit from " +
			"information_schema.TABLES, across acore_world / acore_characters / " +
			"acore_auth. Reports each schema's per-ENGINE breakdown (table count + " +
			"data/index bytes + rowsEstimate per engine) and its DOMINANT " +
			"(most-common) engine, then flags every BASE TABLE whose engine " +
			"deviates from that dominant. Why it matters: a MyISAM table in an " +
			"otherwise-InnoDB schema has no crash recovery (corruption on an " +
			"unclean shutdown), uses table-level locking instead of InnoDB " +
			"row-level locking (a concurrency bottleneck on a write path), and " +
			"supports no transactions or foreign keys. Severity encodes DIRECTION: " +
			"high = a non-crash-safe engine (e.g. MyISAM) where the schema norm is " +
			"crash-safe InnoDB (the textbook ALTER TABLE ... ENGINE=InnoDB " +
			"migration target); low = a crash-safe InnoDB table where the schema " +
			"norm is non-crash-safe (common in acore_world, which ships MyISAM by " +
			"design — SAFER than the norm, informational only); medium = any other " +
			"engine deviation. Because acore_world's dominant engine IS MyISAM, its " +
			"by-design MyISAM tables are never flagged — only the genuine stragglers " +
			"surface. `database` narrows to one schema (rejected, not ignored, when " +
			"not in the allowlist). `minSeverity` (low/medium/high, default low=all) " +
			"trims the candidate LIST to at-or-above that severity; perDatabase and " +
			"totals stay the honest full counts. acore_playerbots is intentionally " +
			"OUT of scope (ops_ro lacks SELECT there — blocked on " +
			"`ops_grant_playerbots_ro`). Complements db_size_summary (which surfaces " +
			"engine per table but never compares within a schema) and db_table_info " +
			"(one table's status). Each candidate carries bytes + rowsEstimate to " +
			"gauge migration effort and a `reason`; confirm with SHOW TABLE STATUS / " +
			"SHOW CREATE TABLE before any ALTER. Results sorted worst-first, capped " +
			"at 500 with `truncated`; `totals` carries the honest pre-cap counts. " +
			"Read-only, ops_ro pool, 10 s timeout.",
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
			minRank := engineSeverityRank("low")
			if a.MinSeverity != "" {
				switch a.MinSeverity {
				case "low", "medium", "high":
					minRank = engineSeverityRank(a.MinSeverity)
				default:
					return map[string]any{"error": "minSeverity must be one of low/medium/high"}
				}
			}
			c, cancel := context.WithTimeout(ctx, dbEngineDistributionTimeout)
			defer cancel()
			out, err := collectEngineDistribution(c, deps.QueryDB, time.Now(), schemas, minRank)
			if err != nil {
				return map[string]any{"error": err.Error()}
			}
			return out
		},
	})
}

// isCrashSafeEngine reports whether engine is a crash-safe, transactional
// storage engine. Among the engines AzerothCore uses only InnoDB qualifies;
// everything else (MyISAM, MEMORY, ARCHIVE, CSV, ...) lacks crash recovery
// and/or transactions. Case-insensitive (information_schema reports "InnoDB").
func isCrashSafeEngine(engine string) bool {
	return strings.EqualFold(engine, "InnoDB")
}

// classifyEngineDeviation buckets a table whose engine differs from its schema's
// dominant engine. Direction matters:
//
//	high   = a non-crash-safe engine (e.g. MyISAM) in a schema whose norm is
//	         crash-safe InnoDB — the textbook migration target (no crash
//	         recovery, table-level locks on a write path).
//	low    = a crash-safe InnoDB table in a schema whose norm is non-crash-safe
//	         (e.g. acore_world, MyISAM by design) — SAFER than the norm,
//	         informational only.
//	medium = any other engine deviation (e.g. MEMORY where the norm is MyISAM).
//
// Returns ("", false) when the table's engine matches its schema's dominant.
func classifyEngineDeviation(tableEngine, dominantEngine string) (string, bool) {
	if tableEngine == dominantEngine {
		return "", false
	}
	safeTable := isCrashSafeEngine(tableEngine)
	safeDominant := isCrashSafeEngine(dominantEngine)
	switch {
	case !safeTable && safeDominant:
		return "high", true
	case safeTable && !safeDominant:
		return "low", true
	default:
		return "medium", true
	}
}

// engineSeverityRank orders severities worst-first for the candidate sort and
// for the minSeverity threshold comparison (lower rank = more severe).
func engineSeverityRank(sev string) int {
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

// engineDeviationReason builds the human-facing explanation + remediation hint
// for one flagged table.
func engineDeviationReason(sev, tableEngine, dominantEngine string) string {
	switch sev {
	case "high":
		return fmt.Sprintf("table uses %s while this schema's dominant engine is %s — %s lacks InnoDB's crash recovery and uses table-level locking instead of row-level, a corruption and concurrency risk on a write path. Migrate with ALTER TABLE ... ENGINE=InnoDB (confirm with SHOW TABLE STATUS / SHOW CREATE TABLE first).",
			tableEngine, dominantEngine, tableEngine)
	case "low":
		return fmt.Sprintf("table uses crash-safe %s while this schema's dominant engine is %s — this table is SAFER than the schema norm (common for acore_world, which ships MyISAM by design); informational, no action needed.",
			tableEngine, dominantEngine)
	default:
		return fmt.Sprintf("table uses %s while this schema's dominant engine is %s — a non-dominant engine; confirm it's intentional with SHOW CREATE TABLE.",
			tableEngine, dominantEngine)
	}
}

// collectEngineDistribution runs one parameterized pass over
// information_schema.TABLES (BASE TABLEs only), tallies each schema's per-engine
// populations, computes the dominant engine per schema, then flags every table
// whose engine deviates. The IN clause is built from ? placeholders — no
// interpolation of caller input.
func collectEngineDistribution(ctx context.Context, db *sql.DB, now time.Time, schemas []string, minRank int) (map[string]any, error) {
	if len(schemas) == 0 {
		return nil, fmt.Errorf("no schemas specified")
	}
	placeholders := make([]string, len(schemas))
	args := make([]any, len(schemas))
	for i, s := range schemas {
		placeholders[i] = "?"
		args[i] = s
	}
	// BASE TABLE drops VIEWs (NULL engine). IFNULL guards the rare engine-less
	// row. ORDER BY keeps output deterministic and groups a schema's tables.
	q := `SELECT TABLE_SCHEMA, TABLE_NAME, IFNULL(ENGINE,''),
			IFNULL(TABLE_ROWS,0), IFNULL(DATA_LENGTH,0), IFNULL(INDEX_LENGTH,0)
		FROM information_schema.TABLES
		WHERE TABLE_TYPE = 'BASE TABLE'
		  AND TABLE_SCHEMA IN (` + strings.Join(placeholders, ",") + `)
		ORDER BY TABLE_SCHEMA, TABLE_NAME`
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("query information_schema.TABLES: %w", err)
	}
	defer rows.Close()

	agg := make(map[string]*engineSchemaAgg, len(schemas))
	for _, s := range schemas {
		agg[s] = newEngineSchemaAgg()
	}
	all := make([]engineTableRow, 0, 256)

	for rows.Next() {
		var r engineTableRow
		if err := rows.Scan(&r.Schema, &r.Table, &r.Engine, &r.RowsEstimate, &r.DataBytes, &r.IndexBytes); err != nil {
			return nil, fmt.Errorf("scan TABLES row: %w", err)
		}
		all = append(all, r)
		a := agg[r.Schema]
		if a == nil {
			// A schema outside the pre-seeded set should be impossible (the IN
			// list bounds the result), but stay defensive rather than panic.
			a = newEngineSchemaAgg()
			agg[r.Schema] = a
		}
		a.tablesScanned++
		ec := a.byEngine[r.Engine]
		if ec == nil {
			ec = &engineCount{Engine: r.Engine}
			a.byEngine[r.Engine] = ec
		}
		ec.TableCount++
		ec.RowsEstimate += r.RowsEstimate
		ec.DataBytes += r.DataBytes
		ec.IndexBytes += r.IndexBytes
		ec.TotalBytes += r.DataBytes + r.IndexBytes
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate TABLES: %w", err)
	}

	// Dominant engine per schema = the most-common engine by table count
	// (dominantString breaks ties lexicographically for determinism).
	domEngine := make(map[string]string, len(agg))
	for s, a := range agg {
		counts := make(map[string]int, len(a.byEngine))
		for eng, ec := range a.byEngine {
			counts[eng] = ec.TableCount
		}
		domEngine[s] = dominantString(counts)
	}

	// Pre-seed perDatabase for every scanned schema (empty schema still surfaces)
	// and build each schema's engineBreakdown sorted by table count desc.
	byDB := make(map[string]*enginePerDatabase, len(schemas))
	for _, s := range schemas {
		a := agg[s]
		breakdown := make([]engineCount, 0, len(a.byEngine))
		for _, ec := range a.byEngine {
			ec.TotalHuman = humanBytes(uint64Of(ec.TotalBytes))
			breakdown = append(breakdown, *ec)
		}
		sort.Slice(breakdown, func(i, j int) bool {
			if breakdown[i].TableCount != breakdown[j].TableCount {
				return breakdown[i].TableCount > breakdown[j].TableCount
			}
			return breakdown[i].Engine < breakdown[j].Engine
		})
		byDB[s] = &enginePerDatabase{
			Database:        s,
			TablesScanned:   a.tablesScanned,
			DistinctEngines: len(a.byEngine),
			DominantEngine:  domEngine[s],
			EngineBreakdown: breakdown,
		}
	}

	// Flag deviations. deviatingCount (per-schema + totals) counts ALL deviations
	// honestly; the minSeverity filter only trims the candidate LIST below.
	candidates := make([]engineDeviationRow, 0, 32)
	totalDeviating := 0
	for _, r := range all {
		dom := domEngine[r.Schema]
		sev, deviates := classifyEngineDeviation(r.Engine, dom)
		if !deviates {
			continue
		}
		totalDeviating++
		if d := byDB[r.Schema]; d != nil {
			d.DeviatingCount++
		}
		if engineSeverityRank(sev) > minRank {
			continue // below the operator's minSeverity threshold
		}
		total := r.DataBytes + r.IndexBytes
		candidates = append(candidates, engineDeviationRow{
			Database:       r.Schema,
			Table:          r.Table,
			Engine:         r.Engine,
			DominantEngine: dom,
			RowsEstimate:   r.RowsEstimate,
			DataBytes:      r.DataBytes,
			IndexBytes:     r.IndexBytes,
			TotalBytes:     total,
			TotalHuman:     humanBytes(uint64Of(total)),
			Severity:       sev,
			Reason:         engineDeviationReason(sev, r.Engine, dom),
		})
	}

	// Worst-first: severity, then bigger tables first (more impactful migration),
	// then schema/table asc for determinism.
	sort.SliceStable(candidates, func(i, j int) bool {
		if ri, rj := engineSeverityRank(candidates[i].Severity), engineSeverityRank(candidates[j].Severity); ri != rj {
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
	if len(candidates) > dbEngineDistributionMaxCandidates {
		candidates = candidates[:dbEngineDistributionMaxCandidates]
		truncated = true
	}

	var totalScanned int
	perDB := make([]enginePerDatabase, 0, len(byDB))
	for _, d := range byDB {
		totalScanned += d.TablesScanned
		perDB = append(perDB, *d)
	}
	sort.Slice(perDB, func(i, j int) bool {
		if perDB[i].DeviatingCount != perDB[j].DeviatingCount {
			return perDB[i].DeviatingCount > perDB[j].DeviatingCount
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
			"tablesScanned":  totalScanned,
			"deviatingCount": totalDeviating,
		},
	}
	if minRank != engineSeverityRank("low") {
		out["minSeverity"] = severityName(minRank)
	}
	return out, nil
}

// severityName is the inverse of engineSeverityRank for echoing the applied
// minSeverity back to the caller.
func severityName(rank int) string {
	switch rank {
	case 0:
		return "high"
	case 1:
		return "medium"
	default:
		return "low"
	}
}
