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
	dbIndexKeyLenAuditTimeout = 10 * time.Second

	// dbIndexKeyLenAntelopeLimit is the InnoDB index key length limit (bytes) for
	// the legacy Antelope row formats (COMPACT / REDUNDANT). The famous "utf8mb4 +
	// VARCHAR(191)" rule falls straight out of it: 191*4 = 764 <= 767 < 768 = 192*4.
	dbIndexKeyLenAntelopeLimit = 767

	// dbIndexKeyLenBarracudaLimit is the InnoDB index key length limit (bytes) for
	// the modern Barracuda row formats (DYNAMIC / COMPRESSED), available when
	// innodb_large_prefix is on (the default since MySQL 5.7.7).
	dbIndexKeyLenBarracudaLimit = 3072

	// dbIndexKeyLenAuditMaxCandidates bounds the MCP envelope. AC's schemas
	// realistically yield a bounded set of over-limit indexes, so this is a guard
	// rail, not an expected limit; totals stays the honest pre-cap count.
	dbIndexKeyLenAuditMaxCandidates = 500
)

// indexKeyLenColRow is one raw per-column STATISTICS row joined to its COLUMNS
// metadata and the owning table's ROW_FORMAT. CharMaxLen / CharOctetLen are -1
// when NULL (a non-string column carries no character length); SubPart is 0 when
// the index uses the full column (no prefix).
type indexKeyLenColRow struct {
	Schema       string
	Table        string
	Index        string
	Seq          int64
	Column       string
	SubPart      int64
	NonUnique    int64
	IndexType    string
	RowFormat    string
	DataType     string
	CharMaxLen   int64
	CharOctetLen int64
}

// indexKeyLenRow is one flagged index whose total key byte length exceeds the
// legacy Antelope 767-byte InnoDB prefix limit. KeyBytes is the summed indexed
// byte length across the index's columns; EffectiveLimitBytes is the limit that
// applies under the table's current ROW_FORMAT.
type indexKeyLenRow struct {
	Database            string   `json:"database"`
	Table               string   `json:"table"`
	Index               string   `json:"index"`
	Columns             []string `json:"columns"`
	IndexType           string   `json:"indexType"`
	NonUnique           bool     `json:"nonUnique"`
	IsPrimary           bool     `json:"isPrimary"`
	RowFormat           string   `json:"rowFormat"`
	KeyBytes            int64    `json:"keyBytes"`
	EffectiveLimitBytes int64    `json:"effectiveLimitBytes"`
	OverAntelopeBytes   int64    `json:"overAntelopeBytes"`
	Severity            string   `json:"severity"`
	Reason              string   `json:"reason"`
	// KeyBytesLowerBound is set when one or more index columns are a type whose
	// indexed byte length this tool can't size (e.g. DECIMAL / BIT) — KeyBytes is
	// then a LOWER bound (those columns contribute 0), so the tool may under-flag
	// but never over-flags. UnsizedColumns names them.
	KeyBytesLowerBound bool     `json:"keyBytesLowerBound,omitempty"`
	UnsizedColumns     []string `json:"unsizedColumns,omitempty"`
}

// indexKeyLenPerDatabase rolls up how many InnoDB BTREE indexes were evaluated in
// each scanned schema and how many exceed the Antelope limit. Pre-seeded for every
// scanned schema so an empty / no-grant schema still surfaces (operators can't
// mistake "schema scanned, nothing flagged" for a tool bug).
type indexKeyLenPerDatabase struct {
	Database       string `json:"database"`
	IndexesScanned int    `json:"indexesScanned"`
	OverLimitCount int    `json:"overLimitCount"`
}

// fixedTypeKeyBytes maps a fixed-width numeric/temporal MySQL DATA_TYPE to its
// indexed byte length. String/binary/text/blob/enum/set columns are sized exactly
// from information_schema.COLUMNS.CHARACTER_OCTET_LENGTH instead, so they're absent
// here. Variable-width types this tool deliberately does NOT guess (DECIMAL depends
// on precision/scale, BIT on M) are absent too → treated as unsized (lower bound).
// DATA_TYPE is already lowercase in information_schema; we ToLower defensively.
var fixedTypeKeyBytes = map[string]int64{
	"tinyint":   1,
	"smallint":  2,
	"mediumint": 3,
	"int":       4,
	"integer":   4,
	"bigint":    8,
	"float":     4,
	"double":    8,
	"date":      3,
	"time":      3,
	"datetime":  8,
	"timestamp": 4,
	"year":      1,
}

// RegisterDBIndexKeyLengthAuditTool registers `db_index_key_length_audit` — the
// index-key-BYTE-LENGTH lens of the db_* suite. The index-hygiene tools reason
// about index COUNT/selectivity (db_redundant_indexes / db_low_cardinality_indexes
// / db_unindexed_tables) and db_row_format_audit (#300) flags the row FORMAT; this
// one computes each InnoDB index's total key byte length and flags the ones that
// exceed the legacy Antelope 767-byte prefix limit — the concrete indexes that
// make a table Barracuda-dependent (the actionable companion to the format audit).
// Reuses the long-merged `dbSizeKnownSchemas` + `isKnownSizeSchema` allowlist
// (db_size_summary, #172).
func RegisterDBIndexKeyLengthAuditTool(reg *Registry, deps DBDeps) {
	reg.Register(Tool{
		Name: "db_index_key_length_audit",
		Description: "InnoDB index key-length audit from information_schema.STATISTICS " +
			"joined to information_schema.COLUMNS (for per-column byte length) and " +
			"information_schema.TABLES (for ROW_FORMAT), across acore_world / " +
			"acore_characters / acore_auth. For every InnoDB BTREE index it sums the " +
			"indexed byte length of each key column — a string column contributes its " +
			"CHARACTER_OCTET_LENGTH (or SUB_PART * bytes-per-char for a prefix index), " +
			"a fixed numeric/temporal column its storage width — and flags indexes " +
			"whose total key length exceeds the legacy Antelope prefix limit of 767 " +
			"bytes. Why it matters: under the Antelope row formats (COMPACT/REDUNDANT) " +
			"an InnoDB index key is capped at 767 bytes; the modern Barracuda formats " +
			"(DYNAMIC/COMPRESSED, with innodb_large_prefix — the default since MySQL " +
			"5.7.7) raise it to 3072. So a single-column index on a utf8mb4 VARCHAR(192)+ " +
			"column (192*4 = 768 > 767) — or any multi-column key that sums past 767 — " +
			"is valid ONLY on a Barracuda table; it would FAIL to create on a legacy " +
			"Antelope row format or a pre-5.7.7 MySQL. This is the actionable companion " +
			"to db_row_format_audit: it names the concrete indexes that pin a table to " +
			"Barracuda. Severity: high = key length exceeds the table's CURRENT effective " +
			"limit (it could not be rebuilt under the present ROW_FORMAT / large_prefix " +
			"setting — a drift/anomaly signal); medium = exceeds the 767-byte Antelope " +
			"limit but fits the table's Barracuda limit (a portability / downgrade " +
			"hazard — do not ROW_FORMAT=COMPACT this table; a portable schema needs a " +
			"shorter column or a prefix index, e.g. col(191) for utf8mb4). PRIMARY and " +
			"UNIQUE BTREE indexes are included (the prefix limit applies to them too); " +
			"FULLTEXT/SPATIAL/HASH are skipped. Prefix indexes (a column indexed with " +
			"SUB_PART) are sized at the prefix, so a properly-prefixed long column is " +
			"NOT flagged. DECIMAL/BIT and other variable-width columns this tool can't " +
			"size are reported via keyBytesLowerBound + unsizedColumns (KeyBytes is then " +
			"a lower bound — it may under-flag, never over-flag). Each candidate carries " +
			"keyBytes, effectiveLimitBytes, the table's rowFormat and a `reason` (confirm " +
			"with SHOW CREATE TABLE / SHOW INDEX before ALTER). `database` narrows to one " +
			"schema (rejected, not ignored, when not in the allowlist). `minSeverity` " +
			"(medium/high, default medium = all flagged) trims the candidate LIST; " +
			"perDatabase and totals stay the honest full counts. acore_playerbots is " +
			"intentionally OUT of scope (ops_ro lacks SELECT there — blocked on " +
			"`ops_grant_playerbots_ro`). Complements db_row_format_audit (row layout) and " +
			"db_low_cardinality_indexes (index selectivity). Results sorted worst-first " +
			"(severity, then keyBytes desc), capped at 500 with `truncated`; `totals` " +
			"carries the honest pre-cap counts. Read-only, ops_ro pool, 10 s timeout.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{
			"database":{"type":"string","enum":["acore_world","acore_characters","acore_auth"],"description":"Limit to one schema (default: all three). acore_playerbots is out of scope (ops_ro lacks SELECT)."},
			"minSeverity":{"type":"string","enum":["medium","high"],"description":"Only list candidates at or above this severity (default medium = all flagged). perDatabase/totals stay the honest full counts."}
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
			// Only medium/high exist (every flagged index is over the 767 limit).
			minRank := indexKeyLenSeverityRank("medium")
			if a.MinSeverity != "" {
				switch a.MinSeverity {
				case "medium", "high":
					minRank = indexKeyLenSeverityRank(a.MinSeverity)
				default:
					return map[string]any{"error": "minSeverity must be one of medium/high"}
				}
			}
			c, cancel := context.WithTimeout(ctx, dbIndexKeyLenAuditTimeout)
			defer cancel()
			out, err := collectIndexKeyLengthAudit(c, deps.QueryDB, time.Now(), schemas, minRank)
			if err != nil {
				return map[string]any{"error": err.Error()}
			}
			return out
		},
	})
}

// innodbKeyPrefixLimit returns the InnoDB index key length limit (bytes) that
// applies under a table's ROW_FORMAT: Barracuda (DYNAMIC/COMPRESSED) gets 3072,
// everything else (the Antelope COMPACT/REDUNDANT formats, and any unknown/empty
// value) gets the conservative 767. Case-insensitive.
func innodbKeyPrefixLimit(rowFormat string) int64 {
	switch strings.ToUpper(strings.TrimSpace(rowFormat)) {
	case "DYNAMIC", "COMPRESSED":
		return dbIndexKeyLenBarracudaLimit
	default:
		return dbIndexKeyLenAntelopeLimit
	}
}

// indexColumnKeyBytes returns the indexed byte length of one key column and
// whether it could be sized. String/binary/text/blob/enum/set columns carry a
// CHARACTER_OCTET_LENGTH (>= 0 here; -1 means NULL = a non-string column):
//   - a prefix index (SubPart > 0) costs SubPart * bytes-per-char, where
//     bytes-per-char = CHARACTER_OCTET_LENGTH / CHARACTER_MAXIMUM_LENGTH (4 for
//     utf8mb4, 3 for utf8mb3, 1 for latin1/binary), capped at the full-column
//     octet length;
//   - otherwise the full column costs CHARACTER_OCTET_LENGTH.
//
// Non-string columns are sized from fixedTypeKeyBytes; a type that isn't there
// (DECIMAL/BIT/unknown) returns (0, false) so the caller treats KeyBytes as a
// lower bound rather than guessing.
func indexColumnKeyBytes(dataType string, charMaxLen, charOctetLen, subPart int64) (int64, bool) {
	if charOctetLen >= 0 {
		if subPart > 0 {
			bpc := int64(1)
			if charMaxLen > 0 {
				if v := charOctetLen / charMaxLen; v > 1 {
					bpc = v
				}
			}
			b := subPart * bpc
			if b > charOctetLen {
				b = charOctetLen // a prefix can't index more than the whole column
			}
			return b, true
		}
		return charOctetLen, true
	}
	if b, ok := fixedTypeKeyBytes[strings.ToLower(strings.TrimSpace(dataType))]; ok {
		return b, true
	}
	return 0, false
}

// classifyIndexKeyLength buckets an index by its total key byte length against the
// table's current effective limit. high = exceeds the CURRENT limit (can't be
// rebuilt as-is — drift/anomaly); medium = exceeds the 767-byte Antelope limit but
// fits the current (Barracuda) limit (portability/downgrade hazard). Returns
// ("", false) for a key within the universally-safe 767-byte ceiling.
func classifyIndexKeyLength(keyBytes, effectiveLimit int64) (string, bool) {
	if keyBytes > effectiveLimit {
		return "high", true
	}
	if keyBytes > dbIndexKeyLenAntelopeLimit {
		return "medium", true
	}
	return "", false
}

// indexKeyLenSeverityRank orders severities worst-first for the candidate sort and
// the minSeverity threshold comparison (lower rank = more severe).
func indexKeyLenSeverityRank(sev string) int {
	switch sev {
	case "high":
		return 0
	case "medium":
		return 1
	default:
		return 2
	}
}

// indexKeyLenReason builds the human-facing explanation + remediation hint for one
// flagged index.
func indexKeyLenReason(sev string, keyBytes, effectiveLimit int64, rowFormat string, lowerBound bool) string {
	var b strings.Builder
	if sev == "high" {
		fmt.Fprintf(&b, "index key length %d bytes EXCEEDS the table's current InnoDB limit of %d bytes (ROW_FORMAT=%s) — it could not be (re)built under the present row format / innodb_large_prefix setting, so this signals drift (the ROW_FORMAT or large_prefix changed since the index was created). Investigate before any rebuild.",
			keyBytes, effectiveLimit, rowFormatOrUnknown(rowFormat))
	} else {
		fmt.Fprintf(&b, "index key length %d bytes exceeds the legacy Antelope prefix limit of %d bytes and is valid only because the table uses a Barracuda ROW_FORMAT=%s (%d-byte limit) — it would FAIL to create on a legacy Antelope row format (COMPACT/REDUNDANT) or a pre-5.7.7 MySQL without innodb_large_prefix. Do not ROW_FORMAT=COMPACT this table; a portable schema needs a shorter column or a prefix index (e.g. col(191) for utf8mb4).",
			keyBytes, dbIndexKeyLenAntelopeLimit, rowFormatOrUnknown(rowFormat), effectiveLimit)
	}
	if lowerBound {
		b.WriteString(" NOTE: one or more key columns are a type this audit can't size (e.g. DECIMAL/BIT), so the reported length is a LOWER bound — the real key is at least this long.")
	}
	b.WriteString(" Confirm with SHOW CREATE TABLE / SHOW INDEX before altering.")
	return b.String()
}

// rowFormatOrUnknown keeps the reason readable when ROW_FORMAT came back empty.
func rowFormatOrUnknown(rowFormat string) string {
	if strings.TrimSpace(rowFormat) == "" {
		return "?"
	}
	return rowFormat
}

// collectIndexKeyLengthAudit runs one parameterized pass over
// information_schema.STATISTICS joined to COLUMNS (per-column byte length) and
// TABLES (ROW_FORMAT, InnoDB scoping), assembles per-column rows into ordered
// indexes in Go, sums each index's key byte length, and flags the indexes whose
// key exceeds the 767-byte Antelope limit. The IN clause is built from ?
// placeholders — no interpolation of caller input.
func collectIndexKeyLengthAudit(ctx context.Context, db *sql.DB, now time.Time, schemas []string, minRank int) (map[string]any, error) {
	if len(schemas) == 0 {
		return nil, fmt.Errorf("no schemas specified")
	}
	placeholders := make([]string, len(schemas))
	args := make([]any, len(schemas))
	for i, s := range schemas {
		placeholders[i] = "?"
		args[i] = s
	}
	// One pass. ENGINE='InnoDB' scopes to the tables for which the 767/3072 prefix
	// limit applies (MyISAM's limit is a different 1000-byte rule, and
	// db_engine_distribution already audits non-InnoDB tables). INDEX_TYPE='BTREE'
	// drops FULLTEXT/SPATIAL/HASH (different key rules). The COLUMNS join is 1:1 —
	// (schema, table, column) is unique there. SUB_PART/CHARACTER_*_LENGTH are
	// IFNULL'd (0 = full column, -1 = non-string column). Per-column rows arrive
	// ORDER BY SEQ_IN_INDEX so Go assembles the ordered key in one walk.
	q := `SELECT s.TABLE_SCHEMA, s.TABLE_NAME, s.INDEX_NAME, s.SEQ_IN_INDEX, s.COLUMN_NAME,
			IFNULL(s.SUB_PART, 0), s.NON_UNIQUE, s.INDEX_TYPE, IFNULL(t.ROW_FORMAT, ''),
			IFNULL(c.DATA_TYPE, ''), IFNULL(c.CHARACTER_MAXIMUM_LENGTH, -1), IFNULL(c.CHARACTER_OCTET_LENGTH, -1)
		FROM information_schema.STATISTICS s
		JOIN information_schema.TABLES t
		  ON s.TABLE_SCHEMA = t.TABLE_SCHEMA AND s.TABLE_NAME = t.TABLE_NAME
		JOIN information_schema.COLUMNS c
		  ON s.TABLE_SCHEMA = c.TABLE_SCHEMA AND s.TABLE_NAME = c.TABLE_NAME AND s.COLUMN_NAME = c.COLUMN_NAME
		WHERE t.TABLE_TYPE = 'BASE TABLE'
		  AND t.ENGINE = 'InnoDB'
		  AND s.INDEX_TYPE = 'BTREE'
		  AND s.TABLE_SCHEMA IN (` + strings.Join(placeholders, ",") + `)
		ORDER BY s.TABLE_SCHEMA, s.TABLE_NAME, s.INDEX_NAME, s.SEQ_IN_INDEX`
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("query information_schema.STATISTICS: %w", err)
	}
	defer rows.Close()

	byDB := make(map[string]*indexKeyLenPerDatabase, len(schemas))
	for _, s := range schemas {
		byDB[s] = &indexKeyLenPerDatabase{Database: s}
	}

	candidates := make([]indexKeyLenRow, 0, 32)
	var totalScanned, totalOverLimit int

	// Group accumulator. A group is one (schema, table, index); rows arrive ordered
	// so a group is a contiguous run, finalized when the key changes.
	var (
		curKey        string
		curSchema     string
		curTable      string
		curIndex      string
		curType       string
		curRowFormat  string
		curNonUnique  bool
		curKeyBytes   int64
		curLowerBound bool
		curColumns    []string
		curUnsized    []string
		haveGroup     bool
	)

	finalize := func() {
		if !haveGroup {
			return
		}
		if d := byDB[curSchema]; d != nil {
			d.IndexesScanned++
		}
		totalScanned++
		effLimit := innodbKeyPrefixLimit(curRowFormat)
		sev, flagged := classifyIndexKeyLength(curKeyBytes, effLimit)
		if !flagged {
			return
		}
		totalOverLimit++
		if d := byDB[curSchema]; d != nil {
			d.OverLimitCount++
		}
		if indexKeyLenSeverityRank(sev) > minRank {
			return // below the operator's minSeverity threshold (totals already counted)
		}
		cols := make([]string, len(curColumns))
		copy(cols, curColumns)
		var unsized []string
		if len(curUnsized) > 0 {
			unsized = make([]string, len(curUnsized))
			copy(unsized, curUnsized)
		}
		candidates = append(candidates, indexKeyLenRow{
			Database:            curSchema,
			Table:               curTable,
			Index:               curIndex,
			Columns:             cols,
			IndexType:           curType,
			NonUnique:           curNonUnique,
			IsPrimary:           curIndex == "PRIMARY",
			RowFormat:           curRowFormat,
			KeyBytes:            curKeyBytes,
			EffectiveLimitBytes: effLimit,
			OverAntelopeBytes:   curKeyBytes - dbIndexKeyLenAntelopeLimit,
			Severity:            sev,
			Reason:              indexKeyLenReason(sev, curKeyBytes, effLimit, curRowFormat, curLowerBound),
			KeyBytesLowerBound:  curLowerBound,
			UnsizedColumns:      unsized,
		})
	}

	for rows.Next() {
		var r indexKeyLenColRow
		if err := rows.Scan(&r.Schema, &r.Table, &r.Index, &r.Seq, &r.Column,
			&r.SubPart, &r.NonUnique, &r.IndexType, &r.RowFormat,
			&r.DataType, &r.CharMaxLen, &r.CharOctetLen); err != nil {
			return nil, fmt.Errorf("scan STATISTICS row: %w", err)
		}
		key := r.Schema + "\x00" + r.Table + "\x00" + r.Index
		if !haveGroup || key != curKey {
			finalize()
			curKey = key
			curSchema = r.Schema
			curTable = r.Table
			curIndex = r.Index
			curType = r.IndexType
			curRowFormat = r.RowFormat
			curNonUnique = r.NonUnique != 0
			curKeyBytes = 0
			curLowerBound = false
			curColumns = curColumns[:0]
			curUnsized = curUnsized[:0]
			haveGroup = true
		}
		curColumns = append(curColumns, r.Column)
		b, known := indexColumnKeyBytes(r.DataType, r.CharMaxLen, r.CharOctetLen, r.SubPart)
		if known {
			curKeyBytes += b
		} else {
			curLowerBound = true
			curUnsized = append(curUnsized, r.Column)
		}
	}
	finalize()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate STATISTICS: %w", err)
	}

	// Worst-first: severity (high before medium), then bigger key first (closer to
	// or further past the limit), then schema/table/index asc for determinism
	// (defends against Go map-iter flake on ties — though rows are already ordered).
	sort.SliceStable(candidates, func(i, j int) bool {
		if ri, rj := indexKeyLenSeverityRank(candidates[i].Severity), indexKeyLenSeverityRank(candidates[j].Severity); ri != rj {
			return ri < rj
		}
		if candidates[i].KeyBytes != candidates[j].KeyBytes {
			return candidates[i].KeyBytes > candidates[j].KeyBytes
		}
		if candidates[i].Database != candidates[j].Database {
			return candidates[i].Database < candidates[j].Database
		}
		if candidates[i].Table != candidates[j].Table {
			return candidates[i].Table < candidates[j].Table
		}
		return candidates[i].Index < candidates[j].Index
	})

	truncated := false
	if len(candidates) > dbIndexKeyLenAuditMaxCandidates {
		candidates = candidates[:dbIndexKeyLenAuditMaxCandidates]
		truncated = true
	}

	perDB := make([]indexKeyLenPerDatabase, 0, len(byDB))
	for _, d := range byDB {
		perDB = append(perDB, *d)
	}
	sort.Slice(perDB, func(i, j int) bool {
		if perDB[i].OverLimitCount != perDB[j].OverLimitCount {
			return perDB[i].OverLimitCount > perDB[j].OverLimitCount
		}
		return perDB[i].Database < perDB[j].Database
	})

	out := map[string]any{
		"asOf":                now.UTC().Format(time.RFC3339),
		"scannedSchemas":      schemas,
		"antelopeLimitBytes":  int64(dbIndexKeyLenAntelopeLimit),
		"barracudaLimitBytes": int64(dbIndexKeyLenBarracudaLimit),
		"candidates":          candidates,
		"perDatabase":         perDB,
		"truncated":           truncated,
		"totals": map[string]any{
			"indexesScanned": totalScanned,
			"overLimitCount": totalOverLimit,
		},
	}
	if minRank != indexKeyLenSeverityRank("medium") {
		out["minSeverity"] = "high"
	}
	return out, nil
}
