package mcpserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
)

const dbExplainTimeout = 10 * time.Second

// reBareSelect matches a top-level SELECT statement we're willing to wrap in
// EXPLAIN. Operators must NOT pre-prepend EXPLAIN — the tool prepends it
// itself, so an "EXPLAIN SELECT ..." input would result in "EXPLAIN EXPLAIN
// SELECT ..." which MySQL rejects. CTEs (WITH ...) are intentionally rejected
// for now to keep the read-only validation surface narrow; if needed they can
// be added later behind a clearer parse.
var reBareSelect = regexp.MustCompile(`(?is)^\s*SELECT\b`)

// RegisterDBExplainTool registers `db_explain` — runs both `EXPLAIN <stmt>`
// and `EXPLAIN FORMAT=JSON <stmt>` against a SELECT in one round trip and
// surfaces a `warnings` array flagging the slow-query patterns operators
// otherwise have to scan the rowset for by hand (full scan, filesort,
// temporary table).
//
// Operators can technically run `EXPLAIN SELECT ...` via db_query, but they
// then have to (1) remember to prepend EXPLAIN, (2) only get the tabular form
// without MySQL 8's cost-based JSON plan, and (3) eyeball the Extra/type/key
// columns themselves to find the bottleneck. db_explain collapses that into
// one composite call.
func RegisterDBExplainTool(reg *Registry, deps DBDeps) {
	reg.Register(Tool{
		Name: "db_explain",
		Description: "Run both EXPLAIN and EXPLAIN FORMAT=JSON on a SELECT against " +
			"acore_world / acore_characters / acore_auth (ops_ro user) in one round trip. " +
			"Returns the tabular plan as `rows` (one entry per join/subquery step with " +
			"id/selectType/table/type/key/rows/filtered/extra), the MySQL 8 cost-based " +
			"FORMAT=JSON plan parsed as `json`, and a `warnings` array flagging full " +
			"scans (`type=ALL`), `Using filesort`, and `Using temporary`. Pass the bare " +
			"SELECT — do NOT prepend EXPLAIN. Read-only, 10 s timeout.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{
			"database":{"type":"string","enum":["acore_world","acore_characters","acore_auth"]},
			"sql":{"type":"string","description":"Bare SELECT statement (NOT 'EXPLAIN SELECT ...')"},
			"params":{"type":"array","description":"Positional values for ? placeholders","items":{}}
		},"required":["database","sql"]}`),
		Annotations: AnnRead(),
		Handler: func(ctx context.Context, raw json.RawMessage, _ string) any {
			var a struct {
				Database string        `json:"database"`
				SQL      string        `json:"sql"`
				Params   []interface{} `json:"params"`
			}
			if err := json.Unmarshal(raw, &a); err != nil {
				return map[string]any{"error": "decode args: " + err.Error()}
			}
			if !allowedDatabases[a.Database] {
				return map[string]any{"error": "database must be one of acore_world/acore_characters/acore_auth"}
			}
			if !reBareSelect.MatchString(a.SQL) {
				return map[string]any{"error": "only bare SELECT statements allowed (do not prepend EXPLAIN)"}
			}
			if reBannedWrite.MatchString(a.SQL) {
				return map[string]any{"error": "statement contains a banned write keyword"}
			}
			if reForbiddenSchema.MatchString(a.SQL) {
				return map[string]any{"error": "queries against mysql/information_schema/performance_schema/sys are not allowed"}
			}
			// Reject obvious multi-statement attempts. A single trailing ; is
			// fine (caller habit) — anything else is rejected.
			if strings.Contains(strings.TrimRight(a.SQL, " \t\r\n;"), ";") {
				return map[string]any{"error": "multiple statements not allowed"}
			}
			if deps.QueryDB == nil {
				return map[string]any{"error": "ops_ro pool not configured (DB_DSN empty?)"}
			}
			c, cancel := context.WithTimeout(ctx, dbExplainTimeout)
			defer cancel()
			out, err := collectExplain(c, deps.QueryDB, a.Database, a.SQL, a.Params)
			if err != nil {
				return map[string]any{"error": err.Error()}
			}
			return out
		},
	})
}

// explainRow is the per-step shape we surface from `EXPLAIN <stmt>`. Field
// naming is lowerCamelCase to match db_table_info's indexEntry. MySQL 8's
// `filtered` column is a percentage (0-100) reported as a float.
type explainRow struct {
	ID           int64   `json:"id"`
	SelectType   string  `json:"selectType"`
	Table        string  `json:"table"`
	Partitions   string  `json:"partitions,omitempty"`
	Type         string  `json:"type"`
	PossibleKeys string  `json:"possibleKeys,omitempty"`
	Key          string  `json:"key,omitempty"`
	KeyLen       string  `json:"keyLen,omitempty"`
	Ref          string  `json:"ref,omitempty"`
	Rows         int64   `json:"rows"`
	Filtered     float64 `json:"filtered"`
	Extra        string  `json:"extra,omitempty"`
}

// collectExplain runs both EXPLAIN forms on the same per-call connection so
// they share the USE-database setting. The JSON plan is best-effort — if
// MySQL refuses (rare optimizer corners with materialized derived tables) we
// surface `json: null` and continue rather than aborting the whole call.
func collectExplain(ctx context.Context, db *sql.DB, database, query string, params []interface{}) (map[string]any, error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire conn: %w", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "USE `"+database+"`"); err != nil {
		return nil, fmt.Errorf("USE %s: %w", database, err)
	}

	rows, err := scanExplainRows(ctx, conn, query, params)
	if err != nil {
		return nil, err
	}

	plan, planErr := scanExplainJSON(ctx, conn, query, params)

	totalRows := int64(0)
	for _, r := range rows {
		totalRows += r.Rows
	}

	out := map[string]any{
		"database":      database,
		"sql":           query,
		"rows":          rows,
		"rowCount":      len(rows),
		"estimatedRows": totalRows,
		"json":          plan,
		"warnings":      detectExplainWarnings(rows),
	}
	if planErr != nil {
		out["jsonError"] = planErr.Error()
	}
	return out, nil
}

func scanExplainRows(ctx context.Context, conn *sql.Conn, query string, params []interface{}) ([]explainRow, error) {
	rs, err := conn.QueryContext(ctx, "EXPLAIN "+query, params...)
	if err != nil {
		return nil, fmt.Errorf("EXPLAIN: %w", err)
	}
	defer rs.Close()
	cols, err := rs.Columns()
	if err != nil {
		return nil, fmt.Errorf("EXPLAIN columns: %w", err)
	}
	colIdx := map[string]int{}
	for i, c := range cols {
		colIdx[c] = i
	}
	// MySQL 5.7 / 8.0 / 8.4 all return at least these columns. If a future
	// MySQL renames any of them we'd rather fail loudly than silently emit
	// zeroed data — same defensive shape as scanShowIndex.
	required := []string{"id", "select_type", "table", "type", "rows", "Extra"}
	for _, r := range required {
		if _, ok := colIdx[r]; !ok {
			return nil, fmt.Errorf("EXPLAIN missing required column %q", r)
		}
	}
	out := []explainRow{}
	for rs.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rs.Scan(ptrs...); err != nil {
			return nil, fmt.Errorf("EXPLAIN scan: %w", err)
		}
		get := func(name string) any {
			if i, ok := colIdx[name]; ok {
				return vals[i]
			}
			return nil
		}
		out = append(out, explainRow{
			ID:           int64From(get("id")),
			SelectType:   stringFrom(get("select_type")),
			Table:        stringFrom(get("table")),
			Partitions:   stringFrom(get("partitions")),
			Type:         stringFrom(get("type")),
			PossibleKeys: stringFrom(get("possible_keys")),
			Key:          stringFrom(get("key")),
			KeyLen:       stringFrom(get("key_len")),
			Ref:          stringFrom(get("ref")),
			Rows:         int64From(get("rows")),
			Filtered:     float64From(get("filtered")),
			Extra:        stringFrom(get("Extra")),
		})
	}
	if err := rs.Err(); err != nil {
		return nil, fmt.Errorf("EXPLAIN iter: %w", err)
	}
	return out, nil
}

// scanExplainJSON parses MySQL 8's `EXPLAIN FORMAT=JSON` output. The driver
// hands back one row, one column ("EXPLAIN") whose value is a JSON document.
// Returns the parsed plan as a tree of map[string]any so the caller can drill
// into cost_info / nested_loop / query_block without re-parsing.
func scanExplainJSON(ctx context.Context, conn *sql.Conn, query string, params []interface{}) (any, error) {
	rs, err := conn.QueryContext(ctx, "EXPLAIN FORMAT=JSON "+query, params...)
	if err != nil {
		return nil, fmt.Errorf("EXPLAIN FORMAT=JSON: %w", err)
	}
	defer rs.Close()
	if !rs.Next() {
		if err := rs.Err(); err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("EXPLAIN FORMAT=JSON returned no rows")
	}
	// The JSON column lands as []byte under the go-sql-driver/mysql, but a
	// string scan also works — accept either to stay portable across driver
	// versions.
	var raw any
	if err := rs.Scan(&raw); err != nil {
		return nil, fmt.Errorf("EXPLAIN FORMAT=JSON scan: %w", err)
	}
	var s string
	switch v := raw.(type) {
	case []byte:
		s = string(v)
	case string:
		s = v
	default:
		return nil, fmt.Errorf("EXPLAIN FORMAT=JSON unexpected type %T", raw)
	}
	var parsed any
	if err := json.Unmarshal([]byte(s), &parsed); err != nil {
		// Surface raw + parseError instead of failing — a malformed JSON plan
		// is still useful to look at, and it's a soft failure not a hard one.
		return map[string]any{"raw": s, "parseError": err.Error()}, nil
	}
	return parsed, nil
}

// detectExplainWarnings flags the slow-query patterns operators otherwise have
// to scan the rowset for by hand. Kept intentionally short — the goal is to
// nudge attention at the worst offenders, not classify every plan.
func detectExplainWarnings(rows []explainRow) []string {
	out := []string{}
	for _, r := range rows {
		t := r.Table
		if t == "" {
			t = fmt.Sprintf("step %d", r.ID)
		}
		if r.Type == "ALL" {
			out = append(out, fmt.Sprintf("full table scan on %s (rows=%d)", t, r.Rows))
		}
		if strings.Contains(r.Extra, "Using filesort") {
			out = append(out, fmt.Sprintf("filesort on %s", t))
		}
		if strings.Contains(r.Extra, "Using temporary") {
			out = append(out, fmt.Sprintf("temporary table required for %s", t))
		}
	}
	return out
}

// float64From mirrors stringFrom / int64From for MySQL's `filtered` column.
// The driver reports it as float64 in some build configs and as []byte
// ("100.00") in others — both have to land as a Go float.
func float64From(v any) float64 {
	switch x := v.(type) {
	case nil:
		return 0
	case float64:
		return x
	case float32:
		return float64(x)
	case int64:
		return float64(x)
	case []byte:
		return parseFloat64(string(x))
	case string:
		return parseFloat64(x)
	default:
		return 0
	}
}

// parseFloat64 is the float twin of parseInt64 — manual to match the existing
// no-stdlib-strconv style and to tolerate the literal string "NULL" the
// driver hands back for a nullable filtered cell.
func parseFloat64(s string) float64 {
	s = strings.TrimSpace(s)
	if s == "" || s == "NULL" {
		return 0
	}
	neg := false
	i := 0
	if s[0] == '-' {
		neg = true
		i = 1
	}
	whole := 0.0
	for ; i < len(s) && s[i] != '.'; i++ {
		c := s[i]
		if c < '0' || c > '9' {
			return 0
		}
		whole = whole*10 + float64(c-'0')
	}
	if i < len(s) && s[i] == '.' {
		i++
		div := 10.0
		for ; i < len(s); i++ {
			c := s[i]
			if c < '0' || c > '9' {
				return 0
			}
			whole += float64(c-'0') / div
			div *= 10
		}
	}
	if neg {
		return -whole
	}
	return whole
}
