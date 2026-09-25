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
	dbRedundantIndexesTimeout = 10 * time.Second
	// dbRedundantIndexesMaxCandidates bounds the response envelope. On a mature
	// schema like AzerothCore the redundant-index count is near zero, but a
	// mis-migrated module could in theory produce many — cap the list and set
	// `truncated:true` rather than blow the 25 KB MCP envelope.
	dbRedundantIndexesMaxCandidates = 500
)

// indexMeta is one fully-assembled index: its ordered column list plus the
// uniqueness / type / cardinality facets needed to decide redundancy. Built by
// folding the per-column information_schema.STATISTICS rows (one row per index
// column) back into a single index entry.
type indexMeta struct {
	Schema      string
	Table       string
	Name        string
	Columns     []string
	Unique      bool
	Type        string
	Cardinality int64 // estimate at the full-index prefix (last SEQ_IN_INDEX)
}

// redundantIndexCandidate is one droppable-candidate index plus the dominant
// index that makes it redundant. `Reason` is "duplicate" (identical column
// list) or "prefix" (the redundant index's columns are a leading prefix of the
// dominant's).
type redundantIndexCandidate struct {
	Database            string   `json:"database"`
	Table               string   `json:"table"`
	RedundantIndex      string   `json:"redundantIndex"`
	RedundantColumns    []string `json:"redundantColumns"`
	RedundantUnique     bool     `json:"redundantUnique"`
	IndexType           string   `json:"indexType"`
	CardinalityEstimate int64    `json:"cardinalityEstimate"`
	DominantIndex       string   `json:"dominantIndex"`
	DominantColumns     []string `json:"dominantColumns"`
	DominantUnique      bool     `json:"dominantUnique"`
	Reason              string   `json:"reason"`
}

// redundantIndexPerDB is the per-schema rollup. Pre-seeded for every scanned
// schema so an empty / no-grant schema still surfaces (mirrors db_size_summary
// — a silent absence reads as a tool bug, not an empty schema).
type redundantIndexPerDB struct {
	Database       string `json:"database"`
	TablesScanned  int    `json:"tablesScanned"`
	IndexesScanned int    `json:"indexesScanned"`
	RedundantCount int    `json:"redundantCount"`
}

// RegisterDBRedundantIndexesTool registers `db_redundant_indexes` — an
// index-hygiene audit over information_schema.STATISTICS. It complements the
// two existing index-adjacent tools: db_size_summary (#172) reports index
// BYTES and db_table_bloat (#175) reports index FRAGMENTATION; neither answers
// "which indexes are structurally redundant and safe to drop?". Every
// redundant secondary index is dead weight — it slows every INSERT/UPDATE/
// DELETE and consumes disk + buffer-pool for zero read benefit, because a
// covering index already serves the same lookups.
func RegisterDBRedundantIndexesTool(reg *Registry, deps DBDeps) {
	reg.Register(Tool{
		Name: "db_redundant_indexes",
		Description: "Index-hygiene audit from information_schema.STATISTICS — finds " +
			"duplicate and prefix-redundant secondary indexes across acore_world / " +
			"acore_characters / acore_auth that are candidates for DROP. An index is " +
			"reported when another index in the same table covers it: `duplicate` " +
			"(identical column list + same uniqueness) or `prefix` (its columns are a " +
			"leading prefix of a longer index, so the longer index already serves the " +
			"same lookups). Each redundant index slows every write and wastes disk + " +
			"buffer pool for no read benefit. Complements db_size_summary (index " +
			"BYTES) and db_table_bloat (index FRAGMENTATION) — this is the structural " +
			"`which can I drop?` view. Correctness guards: a PRIMARY key is never " +
			"reported as redundant (it's the clustered key); a UNIQUE index is only " +
			"redundant against an identical UNIQUE index (a longer or non-unique index " +
			"can't enforce its constraint); indexes are only compared within the same " +
			"INDEX_TYPE (BTREE / FULLTEXT / SPATIAL / HASH). NOTE the comparison uses " +
			"the explicitly-declared columns only — it does NOT model InnoDB's implicit " +
			"clustered-PK suffix on secondary indexes, so verify with SHOW CREATE TABLE " +
			"before dropping. CARDINALITY is an optimizer estimate from the last " +
			"ANALYZE, surfaced as cardinalityEstimate. Returns `candidates` (each with " +
			"redundantIndex/redundantColumns + the dominantIndex/dominantColumns that " +
			"covers it + reason), `perDatabase` (tablesScanned / indexesScanned / " +
			"redundantCount per schema), and `totals`. `database` filters to one schema " +
			"(acore_world/acore_characters/acore_auth). acore_playerbots is OUT of scope " +
			"(ops_ro lacks SELECT). Read-only, ops_ro pool, 10 s timeout.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{
			"database":{"type":"string","description":"Filter to one schema (acore_world/acore_characters/acore_auth). Omit to scan all three.","enum":["acore_world","acore_characters","acore_auth"]}
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
			c, cancel := context.WithTimeout(ctx, dbRedundantIndexesTimeout)
			defer cancel()
			out, err := collectRedundantIndexes(c, deps.QueryDB, schemas, time.Now())
			if err != nil {
				return map[string]any{"error": err.Error()}
			}
			if a.Database != "" {
				out["database"] = a.Database
			}
			return out
		},
	})
}

// collectRedundantIndexes issues one parameterized SELECT against
// information_schema.STATISTICS (the same direct-pool, fully-qualified path
// db_size_summary uses against information_schema.TABLES — no USE needed, and
// the ops_ro grant exposes only rows for tables it can SELECT). Rows arrive one
// per index column, ORDER BY ... SEQ_IN_INDEX so each index's columns assemble
// in declaration order. Aggregation + redundancy detection happen in Go.
func collectRedundantIndexes(ctx context.Context, db *sql.DB, schemas []string, now time.Time) (map[string]any, error) {
	if len(schemas) == 0 {
		return nil, fmt.Errorf("no schemas specified")
	}
	placeholders := make([]string, len(schemas))
	args := make([]any, len(schemas))
	for i, s := range schemas {
		placeholders[i] = "?"
		args[i] = s
	}
	q := `SELECT TABLE_SCHEMA, TABLE_NAME, INDEX_NAME, SEQ_IN_INDEX,
			IFNULL(COLUMN_NAME, ''), NON_UNIQUE, IFNULL(INDEX_TYPE, ''), IFNULL(CARDINALITY, 0)
		FROM information_schema.STATISTICS
		WHERE TABLE_SCHEMA IN (` + strings.Join(placeholders, ",") + `)
		ORDER BY TABLE_SCHEMA, TABLE_NAME, INDEX_NAME, SEQ_IN_INDEX`
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("query information_schema.STATISTICS: %w", err)
	}
	defer rows.Close()

	// Fold per-column rows back into one indexMeta per (schema, table, index).
	// Map key is "schema\x00table\x00index" — a NUL separator can't collide with
	// any identifier. Cardinality is overwritten on each column row; rows arrive
	// SEQ_IN_INDEX-ascending so the last write is the full-index estimate.
	type idxKey struct{ schema, table, index string }
	idxByKey := make(map[idxKey]*indexMeta, 256)
	order := make([]idxKey, 0, 256) // preserve first-seen order for determinism
	for rows.Next() {
		var (
			schema, table, index, col, itype string
			seq, nonUnique, cardinality      int64
		)
		if err := rows.Scan(&schema, &table, &index, &seq, &col, &nonUnique, &itype, &cardinality); err != nil {
			return nil, fmt.Errorf("scan STATISTICS row: %w", err)
		}
		k := idxKey{schema, table, index}
		m, ok := idxByKey[k]
		if !ok {
			m = &indexMeta{
				Schema: schema,
				Table:  table,
				Name:   index,
				Unique: nonUnique == 0,
				Type:   itype,
			}
			idxByKey[k] = m
			order = append(order, k)
		}
		m.Columns = append(m.Columns, col)
		m.Cardinality = cardinality
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate STATISTICS: %w", err)
	}

	// Group assembled indexes by (schema, table). Iterate in first-seen order so
	// the per-table index slices are deterministic without an extra sort.
	type tblKey struct{ schema, table string }
	byTable := make(map[tblKey][]*indexMeta, 128)
	tblOrder := make([]tblKey, 0, 128)
	perDB := make(map[string]*redundantIndexPerDB, len(schemas))
	for _, s := range schemas {
		perDB[s] = &redundantIndexPerDB{Database: s}
	}
	for _, k := range order {
		m := idxByKey[k]
		tk := tblKey{m.Schema, m.Table}
		if _, seen := byTable[tk]; !seen {
			tblOrder = append(tblOrder, tk)
			if d, ok := perDB[m.Schema]; ok {
				d.TablesScanned++
			}
		}
		byTable[tk] = append(byTable[tk], m)
		if d, ok := perDB[m.Schema]; ok {
			d.IndexesScanned++
		}
	}

	candidates := make([]redundantIndexCandidate, 0, 16)
	for _, tk := range tblOrder {
		idxs := byTable[tk]
		for _, r := range idxs {
			best := bestDominantIndex(r, idxs)
			if best == nil {
				continue
			}
			reason := "prefix"
			if len(best.Columns) == len(r.Columns) {
				reason = "duplicate"
			}
			candidates = append(candidates, redundantIndexCandidate{
				Database:            r.Schema,
				Table:               r.Table,
				RedundantIndex:      r.Name,
				RedundantColumns:    r.Columns,
				RedundantUnique:     r.Unique,
				IndexType:           r.Type,
				CardinalityEstimate: r.Cardinality,
				DominantIndex:       best.Name,
				DominantColumns:     best.Columns,
				DominantUnique:      best.Unique,
				Reason:              reason,
			})
			if d, ok := perDB[r.Schema]; ok {
				d.RedundantCount++
			}
		}
	}

	// Stable output order: schema, table, redundant index name.
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].Database != candidates[j].Database {
			return candidates[i].Database < candidates[j].Database
		}
		if candidates[i].Table != candidates[j].Table {
			return candidates[i].Table < candidates[j].Table
		}
		return candidates[i].RedundantIndex < candidates[j].RedundantIndex
	})
	truncated := false
	if len(candidates) > dbRedundantIndexesMaxCandidates {
		candidates = candidates[:dbRedundantIndexesMaxCandidates]
		truncated = true
	}

	// Per-database rollup as a sorted slice — redundantCount desc, then schema
	// asc so the noisiest schema leads and ties stay deterministic.
	perDBList := make([]redundantIndexPerDB, 0, len(perDB))
	for _, d := range perDB {
		perDBList = append(perDBList, *d)
	}
	sort.Slice(perDBList, func(i, j int) bool {
		if perDBList[i].RedundantCount != perDBList[j].RedundantCount {
			return perDBList[i].RedundantCount > perDBList[j].RedundantCount
		}
		return perDBList[i].Database < perDBList[j].Database
	})

	var totalTables, totalIndexes, totalRedundant int
	for _, d := range perDBList {
		totalTables += d.TablesScanned
		totalIndexes += d.IndexesScanned
		totalRedundant += d.RedundantCount
	}

	return map[string]any{
		"asOf":           now.UTC().Format(time.RFC3339),
		"scannedSchemas": schemas,
		"candidates":     candidates,
		"perDatabase":    perDBList,
		"truncated":      truncated,
		"totals": map[string]any{
			"tablesScanned":  totalTables,
			"indexesScanned": totalIndexes,
			"redundantCount": totalRedundant,
		},
	}, nil
}

// bestDominantIndex returns the index in `idxs` that most strongly makes `r`
// redundant, or nil if `r` is not redundant. "Most strongly" = longest covering
// column list (an index covered by both (a,b) and (a,b,c) reports the longer
// (a,b,c) as the survivor), tiebroken by index name asc for determinism.
func bestDominantIndex(r *indexMeta, idxs []*indexMeta) *indexMeta {
	var best *indexMeta
	for _, d := range idxs {
		if d == r || !indexRedundantTo(*r, *d) {
			continue
		}
		if best == nil ||
			len(d.Columns) > len(best.Columns) ||
			(len(d.Columns) == len(best.Columns) && d.Name < best.Name) {
			best = d
		}
	}
	return best
}

// indexRedundantTo reports whether index `r` is made redundant by index `d`
// (both in the same table). Encodes the standard duplicate/prefix-index rule
// (cf. pt-duplicate-key-checker / MySQL sys.schema_redundant_indexes):
//
//   - PRIMARY is never redundant — it's the clustered key, not droppable.
//   - Only same-type indexes are comparable (a BTREE prefix doesn't cover a
//     FULLTEXT, etc.).
//   - `r`'s columns must be a leading prefix of (or equal to) `d`'s.
//   - A UNIQUE `r` is only redundant against an identical UNIQUE `d`: a longer
//     index — even unique — doesn't enforce r's constraint, and a non-unique d
//     enforces nothing.
//   - When columns are identical, exactly one of the pair is flagged (the keeper
//     is chosen by indexKeeperPreferred) so a true duplicate isn't double-counted.
func indexRedundantTo(r, d indexMeta) bool {
	if r.Name == d.Name {
		return false
	}
	if r.Name == "PRIMARY" {
		return false
	}
	if r.Type != d.Type {
		return false
	}
	if !columnsArePrefix(r.Columns, d.Columns) {
		return false
	}
	eq := len(r.Columns) == len(d.Columns)
	if r.Unique && (!eq || !d.Unique) {
		return false
	}
	if eq {
		return indexKeeperPreferred(d, r)
	}
	return true
}

// indexKeeperPreferred reports whether `keep` should be retained over `drop`
// when the two have identical column lists. Precedence: UNIQUE beats
// non-unique (keep the constraint), then PRIMARY beats a named index, then the
// lexically-smaller name wins (deterministic, and ensures exactly one of an
// identical pair is flagged redundant).
func indexKeeperPreferred(keep, drop indexMeta) bool {
	if keep.Unique != drop.Unique {
		return keep.Unique
	}
	if (keep.Name == "PRIMARY") != (drop.Name == "PRIMARY") {
		return keep.Name == "PRIMARY"
	}
	return keep.Name < drop.Name
}

// columnsArePrefix reports whether `short` is a leading prefix of `long`
// (equal-length identical lists return true). Column comparison is
// order-sensitive — index column order is semantically load-bearing.
func columnsArePrefix(short, long []string) bool {
	if len(short) > len(long) {
		return false
	}
	for i := range short {
		if short[i] != long[i] {
			return false
		}
	}
	return true
}
