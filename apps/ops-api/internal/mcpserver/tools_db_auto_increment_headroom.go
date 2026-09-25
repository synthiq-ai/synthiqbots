package mcpserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

const (
	dbAutoIncHeadroomTimeout = 10 * time.Second

	// dbAutoIncHeadroomDefaultMinUsage — flag an AUTO_INCREMENT column once its
	// counter has consumed this fraction of the column-type's max value. 0.70
	// (70%) is an early-warning floor: there's still plenty of runway to plan
	// an ALTER TABLE to a wider type before inserts start failing.
	dbAutoIncHeadroomDefaultMinUsage = 0.70

	// dbAutoIncHeadroomHighCutoff — usage at/above this is `high` severity
	// (the rest of the flagged set is `medium`). 0.90 is an absolute danger
	// line: at 90% of a counter's range, exhaustion is imminent and there's
	// no longer comfortable runway to schedule a migration window. Unlike a
	// selectivity ratio (which has no intrinsic meaning), AUTO_INCREMENT usage
	// IS an absolute scale, so a fixed cutoff is meaningful regardless of the
	// operator's chosen minUsage threshold.
	dbAutoIncHeadroomHighCutoff = 0.90

	// dbAutoIncHeadroomMaxCandidates — envelope guard. AC's schema realistically
	// yields a handful of at-risk counters at most; the cap stops a pathological
	// result from blowing the 25 KB MCP response budget. totals.atRiskCount stays
	// honest (pre-truncation) so the operator knows the list was clipped.
	dbAutoIncHeadroomMaxCandidates = 500
)

// autoIncCandidate is one AUTO_INCREMENT column approaching its type ceiling.
// `currentAutoIncrement` is information_schema.TABLES.AUTO_INCREMENT — the NEXT
// value the engine will assign, so it slightly over-counts the highest live id
// (conservative: we'd rather warn one id early than one late). `maxValue` is the
// column type's max for the declared signedness. `rowsEstimate` is surfaced for
// context but is deliberately NOT a filter (see the tool description): a churny
// table can have few live rows yet a near-max counter because deletes and rolled-
// back inserts burn ids without ever resetting AUTO_INCREMENT.
type autoIncCandidate struct {
	Database             string  `json:"database"`
	Table                string  `json:"table"`
	Column               string  `json:"column"`
	Engine               string  `json:"engine,omitempty"`
	ColumnType           string  `json:"columnType"`
	Unsigned             bool    `json:"unsigned"`
	CurrentAutoIncrement uint64  `json:"currentAutoIncrement"`
	MaxValue             uint64  `json:"maxValue"`
	UsageRatio           float64 `json:"usageRatio"`
	RowsEstimate         int64   `json:"rowsEstimate"`
	Severity             string  `json:"severity"`
	Reason               string  `json:"reason"`
}

// autoIncPerDatabase is the per-schema rollup. `autoIncrementTablesScanned` is
// the honest denominator: every base table in scope that owns an AUTO_INCREMENT
// column AND whose column type we could classify (an unrecognized type is left
// out of the denominator since we can't reason about its headroom).
type autoIncPerDatabase struct {
	Database                   string `json:"database"`
	AutoIncrementTablesScanned int    `json:"autoIncrementTablesScanned"`
	AtRiskCount                int    `json:"atRiskCount"`
}

// RegisterDBAutoIncrementHeadroomTool registers `db_auto_increment_headroom` —
// a capacity-exhaustion audit that flags AUTO_INCREMENT counters nearing their
// column-type ceiling. It is the id-capacity sibling of `db_size_summary`'s
// disk-capacity view: a long-running realm churns ids (item_instance, mail, log
// tables) and an INT (or worse, a SMALLINT/MEDIUMINT) primary key that hits its
// max takes inserts down with "Out of range value" — a hard outage with no
// graceful degradation. One information_schema pass joins TABLES (AUTO_INCREMENT
// + ENGINE + row estimate) to COLUMNS (the auto_increment column's declared type)
// so the headroom is computed without a hand-written query. Distinct from
// `db_table_info`, which surfaces ONE table's raw `autoIncrement` value via
// SHOW TABLE STATUS but never compares it to the type ceiling or sweeps schemas.
func RegisterDBAutoIncrementHeadroomTool(reg *Registry, deps DBDeps) {
	reg.Register(Tool{
		Name: "db_auto_increment_headroom",
		Description: "AUTO_INCREMENT exhaustion audit across acore_world / " +
			"acore_characters / acore_auth. One information_schema pass joins " +
			"TABLES to COLUMNS (the column whose EXTRA is 'auto_increment') and " +
			"flags counters whose value has consumed `minUsage` (default 0.70 = " +
			"70%) or more of the column type's maximum — INT signed 2147483647, " +
			"INT UNSIGNED 4294967295, SMALLINT/MEDIUMINT/TINYINT/BIGINT scaled " +
			"likewise. When a counter reaches the type max the next insert fails " +
			"with 'Out of range value for column' — a hard outage — so this is " +
			"the id-capacity companion to `db_size_summary`'s disk-capacity view " +
			"and the cross-schema counterpart to `db_table_info`, which shows only " +
			"one table's raw counter via SHOW TABLE STATUS. " +
			"Returns `candidates` (worst-first: severity then usageRatio desc, " +
			"each with database / table / column / columnType / unsigned / " +
			"currentAutoIncrement / maxValue / usageRatio / rowsEstimate / " +
			"severity / reason), `perDatabase` rollups, and `totals`. Severity is " +
			"`high` at/above 0.90 (exhaustion imminent — migrate the column to a " +
			"wider type NOW) else `medium`. `minUsage` is REJECTED (not clamped) " +
			"when outside (0,1] so a typo'd 70 reads as a bad filter, not an " +
			"empty all-healthy result. There is deliberately NO minRows filter: " +
			"AUTO_INCREMENT never resets on delete/rollback, so a low-row table " +
			"can still be near exhaustion (rowsEstimate is surfaced for context " +
			"only). currentAutoIncrement is the NEXT id to assign (an InnoDB " +
			"estimate that can drift across restarts), so confirm with " +
			"SHOW TABLE STATUS before scheduling a migration. acore_playerbots " +
			"is intentionally OUT of scope — ops_ro lacks SELECT there (blocked " +
			"on infra wedge `ops_grant_playerbots_ro`). `database` narrows to one " +
			"schema. Read-only, ops_ro pool, 10 s timeout.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{
			"database":{"type":"string","enum":["acore_world","acore_characters","acore_auth"],"description":"Limit to one schema (default: all three)"},
			"minUsage":{"type":"number","description":"Flag counters at/above this fraction of the column-type max (default 0.70). Must be in (0,1] — rejected, not clamped, if outside."}
		}}`),
		Annotations: AnnRead(),
		Handler: func(ctx context.Context, raw json.RawMessage, _ string) any {
			var a struct {
				Database string   `json:"database"`
				MinUsage *float64 `json:"minUsage"`
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
			minUsage := dbAutoIncHeadroomDefaultMinUsage
			if a.MinUsage != nil {
				if *a.MinUsage <= 0 || *a.MinUsage > 1 {
					return map[string]any{"error": "minUsage must be in (0,1] (e.g. 0.70 for 70%)"}
				}
				minUsage = *a.MinUsage
			}
			c, cancel := context.WithTimeout(ctx, dbAutoIncHeadroomTimeout)
			defer cancel()
			out, err := collectAutoIncrementHeadroom(c, deps.QueryDB, schemas, minUsage)
			if err != nil {
				return map[string]any{"error": err.Error()}
			}
			return out
		},
	})
}

// autoIncrementMax returns the maximum value an AUTO_INCREMENT column of the
// given base data type can hold for the declared signedness, and whether the
// type was recognized. MySQL/MariaDB only permit integer types on an
// AUTO_INCREMENT column, so the five integer widths are exhaustive; anything
// else (a misconfiguration) returns ok=false and is left unclassified rather
// than guessed. BIGINT UNSIGNED's max equals math.MaxUint64 exactly, so uint64
// holds every ceiling without overflow.
func autoIncrementMax(dataType string, unsigned bool) (uint64, bool) {
	switch strings.ToLower(strings.TrimSpace(dataType)) {
	case "tinyint":
		if unsigned {
			return 255, true
		}
		return 127, true
	case "smallint":
		if unsigned {
			return 65535, true
		}
		return 32767, true
	case "mediumint":
		if unsigned {
			return 16777215, true
		}
		return 8388607, true
	case "int", "integer":
		if unsigned {
			return 4294967295, true
		}
		return 2147483647, true
	case "bigint":
		if unsigned {
			return math.MaxUint64, true
		}
		return math.MaxInt64, true
	default:
		return 0, false
	}
}

// autoIncUsageRatio is current/max as a float64 with a max==0 div-guard (an
// unrecognized type yields max 0). float64 loses precision on uint64 values
// past 2^53, but the result feeds only a threshold comparison and a rounded
// display, where that drift is immaterial.
func autoIncUsageRatio(current, max uint64) (float64, bool) {
	if max == 0 {
		return 0, false
	}
	return float64(current) / float64(max), true
}

// classifyAutoIncHeadroom maps a usage ratio to a severity. Only called for
// already-flagged rows (ratio >= the operator's minUsage), so it never labels
// a healthy counter. `high` once usage crosses the absolute 0.90 danger line;
// everything else flagged is `medium`.
func classifyAutoIncHeadroom(ratio float64) string {
	if ratio >= dbAutoIncHeadroomHighCutoff {
		return "high"
	}
	return "medium"
}

// autoIncSeverityRank orders severities worst-first for the candidate sort.
func autoIncSeverityRank(sev string) int {
	switch sev {
	case "high":
		return 0
	case "medium":
		return 1
	default:
		return 2
	}
}

// roundAutoIncUsage rounds a usage ratio to 6 decimal places for stable JSON
// output (so 0.8862341… doesn't render a noisy 17-digit float).
func roundAutoIncUsage(r float64) float64 {
	return math.Round(r*1e6) / 1e6
}

// collectAutoIncrementHeadroom runs the single information_schema sweep, joining
// TABLES to COLUMNS to recover each base table's AUTO_INCREMENT counter and the
// declared type of its auto_increment column. The JOIN on EXTRA LIKE
// '%auto_increment%' restricts the result to exactly the tables that own such a
// column (at most one per table per the engine), and `AUTO_INCREMENT IS NOT NULL`
// drops tables whose counter the server hasn't populated. Classification,
// filtering, and rollups happen in Go.
func collectAutoIncrementHeadroom(ctx context.Context, db *sql.DB, schemas []string, minUsage float64) (map[string]any, error) {
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
			IFNULL(t.AUTO_INCREMENT,0), IFNULL(t.TABLE_ROWS,0),
			c.COLUMN_NAME, c.DATA_TYPE, c.COLUMN_TYPE
		FROM information_schema.TABLES t
		JOIN information_schema.COLUMNS c
		  ON c.TABLE_SCHEMA = t.TABLE_SCHEMA
		 AND c.TABLE_NAME = t.TABLE_NAME
		 AND c.EXTRA LIKE '%auto_increment%'
		WHERE t.TABLE_TYPE = 'BASE TABLE'
		  AND t.AUTO_INCREMENT IS NOT NULL
		  AND t.TABLE_SCHEMA IN (` + strings.Join(placeholders, ",") + `)
		ORDER BY t.TABLE_SCHEMA, t.TABLE_NAME`
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("query information_schema auto_increment: %w", err)
	}
	defer rows.Close()

	// Pre-seed perDatabase so every scanned schema surfaces even with zero
	// at-risk counters (mirrors db_size_summary) — a silent-absence regression
	// would otherwise look like a tool bug rather than an empty result.
	byDB := make(map[string]*autoIncPerDatabase, len(schemas))
	for _, s := range schemas {
		byDB[s] = &autoIncPerDatabase{Database: s}
	}

	candidates := make([]autoIncCandidate, 0, 16)
	atRiskTotal := 0
	scannedTotal := 0
	for rows.Next() {
		var (
			schema, name, engine     string
			current                  uint64
			rowsEst                  int64
			column, dataType, colTyp string
		)
		if err := rows.Scan(&schema, &name, &engine, &current, &rowsEst, &column, &dataType, &colTyp); err != nil {
			return nil, fmt.Errorf("scan auto_increment row: %w", err)
		}
		unsigned := strings.Contains(strings.ToLower(colTyp), "unsigned")
		maxVal, ok := autoIncrementMax(dataType, unsigned)
		if !ok {
			// Unrecognized AUTO_INCREMENT column type — can't reason about its
			// headroom, so leave it out of the denominator entirely rather than
			// counting it as a "scanned but healthy" table.
			continue
		}
		if d := byDB[schema]; d != nil {
			d.AutoIncrementTablesScanned++
		}
		scannedTotal++
		ratio, okR := autoIncUsageRatio(current, maxVal)
		if !okR || ratio < minUsage {
			continue
		}
		sev := classifyAutoIncHeadroom(ratio)
		candidates = append(candidates, autoIncCandidate{
			Database:             schema,
			Table:                name,
			Column:               column,
			Engine:               engine,
			ColumnType:           colTyp,
			Unsigned:             unsigned,
			CurrentAutoIncrement: current,
			MaxValue:             maxVal,
			UsageRatio:           roundAutoIncUsage(ratio),
			RowsEstimate:         rowsEst,
			Severity:             sev,
			Reason: fmt.Sprintf(
				"%s AUTO_INCREMENT at %d of %d (%.1f%%) — %s. Inserts fail with 'Out of range value' once the counter hits the max; migrate the column to a wider integer type before then. AUTO_INCREMENT is the next-id estimate and never resets on delete/rollback.",
				colTyp, current, maxVal, ratio*100, autoIncHeadroomAdvice(sev),
			),
		})
		atRiskTotal++
		if d := byDB[schema]; d != nil {
			d.AtRiskCount++
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate auto_increment rows: %w", err)
	}

	// Worst-first: severity rank, then usage desc, then current desc, then
	// database/table asc for deterministic ties (defends against Go's
	// randomized map-iteration order surfacing flaky test output).
	sort.SliceStable(candidates, func(i, j int) bool {
		ri, rj := autoIncSeverityRank(candidates[i].Severity), autoIncSeverityRank(candidates[j].Severity)
		if ri != rj {
			return ri < rj
		}
		if candidates[i].UsageRatio != candidates[j].UsageRatio {
			return candidates[i].UsageRatio > candidates[j].UsageRatio
		}
		if candidates[i].CurrentAutoIncrement != candidates[j].CurrentAutoIncrement {
			return candidates[i].CurrentAutoIncrement > candidates[j].CurrentAutoIncrement
		}
		if candidates[i].Database != candidates[j].Database {
			return candidates[i].Database < candidates[j].Database
		}
		return candidates[i].Table < candidates[j].Table
	})

	truncated := false
	if len(candidates) > dbAutoIncHeadroomMaxCandidates {
		candidates = candidates[:dbAutoIncHeadroomMaxCandidates]
		truncated = true
	}

	// perDatabase rollup — sort at-risk-count desc, schema asc as tiebreaker.
	perDB := make([]autoIncPerDatabase, 0, len(byDB))
	for _, d := range byDB {
		perDB = append(perDB, *d)
	}
	sort.Slice(perDB, func(i, j int) bool {
		if perDB[i].AtRiskCount != perDB[j].AtRiskCount {
			return perDB[i].AtRiskCount > perDB[j].AtRiskCount
		}
		return perDB[i].Database < perDB[j].Database
	})

	return map[string]any{
		"asOf":           time.Now().UTC().Format(time.RFC3339),
		"scannedSchemas": schemas,
		"minUsage":       minUsage,
		"candidates":     candidates,
		"perDatabase":    perDB,
		"truncated":      truncated,
		"totals": map[string]any{
			"autoIncrementTablesScanned": scannedTotal,
			"atRiskCount":                atRiskTotal,
		},
	}, nil
}

// autoIncHeadroomAdvice phrases the urgency inline in the reason string so the
// operator reads the call to action without cross-referencing the severity enum.
func autoIncHeadroomAdvice(severity string) string {
	if severity == "high" {
		return "exhaustion is imminent, migrate the column NOW"
	}
	return "approaching exhaustion, plan a migration window"
}
