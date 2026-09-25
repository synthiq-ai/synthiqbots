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

// reSafeIdentifier matches a MySQL identifier we are willing to interpolate into
// a SHOW statement. SHOW CREATE TABLE / SHOW INDEX cannot use ? placeholders, so
// the table name is concatenated. We restrict to ASCII letters, digits, and
// underscore — strict subset of what MySQL allows in unquoted identifiers, but
// covers every table in acore_*. Anything else is rejected before reaching the
// driver.
var reSafeIdentifier = regexp.MustCompile(`^[A-Za-z0-9_]+$`)

const dbTableInfoTimeout = 10 * time.Second

// RegisterDBTableInfoTool registers `db_table_info` — a composite read-only
// tool that returns CREATE TABLE DDL, indexes, and TABLE STATUS for one table
// in a single call. Operators previously stitched together three sequential
// db_query calls (SHOW CREATE TABLE, SHOW INDEX FROM, SHOW TABLE STATUS LIKE)
// when investigating a slow query or auditing schema; this collapses that into
// one round trip and pre-parses the SHOW INDEX rowset into a per-key structure
// instead of the column-per-segment shape MySQL returns.
func RegisterDBTableInfoTool(reg *Registry, deps DBDeps) {
	reg.Register(Tool{
		Name: "db_table_info",
		Description: "Composite schema/index/size snapshot for one table in acore_world / " +
			"acore_characters / acore_auth. Returns parsed `createTable` DDL, an `indexes` " +
			"array (one entry per key with multi-column segments collapsed into `columns`), " +
			"`primaryKey` columns, row-count estimate, data/index length, and engine. " +
			"Replaces the SHOW CREATE TABLE + SHOW INDEX FROM + SHOW TABLE STATUS LIKE " +
			"sequence operators were running via three back-to-back db_query calls. " +
			"Read-only, uses the ops_ro pool, 10 s timeout.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{
			"database":{"type":"string","enum":["acore_world","acore_characters","acore_auth"]},
			"table":{"type":"string","description":"Table name (alphanumerics + underscore only)"}
		},"required":["database","table"]}`),
		Annotations: AnnRead(),
		Handler: func(ctx context.Context, raw json.RawMessage, _ string) any {
			var a struct {
				Database string `json:"database"`
				Table    string `json:"table"`
			}
			if err := json.Unmarshal(raw, &a); err != nil {
				return map[string]any{"error": "decode args: " + err.Error()}
			}
			if !allowedDatabases[a.Database] {
				return map[string]any{"error": "database must be one of acore_world/acore_characters/acore_auth"}
			}
			if !reSafeIdentifier.MatchString(a.Table) {
				return map[string]any{"error": "table must match ^[A-Za-z0-9_]+$ (got " + a.Table + ")"}
			}
			if deps.QueryDB == nil {
				return map[string]any{"error": "ops_ro pool not configured (DB_DSN empty?)"}
			}
			c, cancel := context.WithTimeout(ctx, dbTableInfoTimeout)
			defer cancel()
			out, err := collectTableInfo(c, deps.QueryDB, a.Database, a.Table)
			if err != nil {
				return map[string]any{"error": err.Error()}
			}
			return out
		},
	})
}

// collectTableInfo runs the three SHOW statements against the per-call
// connection and packs them into the response shape. Split out so tests can
// drive it with sqlmock without going through the registry handler.
func collectTableInfo(ctx context.Context, db *sql.DB, database, table string) (map[string]any, error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire conn: %w", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "USE `"+database+"`"); err != nil {
		return nil, fmt.Errorf("USE %s: %w", database, err)
	}

	createDDL, err := scanCreateTable(ctx, conn, table)
	if err != nil {
		return nil, err
	}
	indexes, primary, err := scanShowIndex(ctx, conn, table)
	if err != nil {
		return nil, err
	}
	status, err := scanTableStatus(ctx, conn, table)
	if err != nil {
		return nil, err
	}

	dataLen := status.DataLength
	idxLen := status.IndexLength
	totalLen := dataLen + idxLen

	out := map[string]any{
		"database":         database,
		"table":            table,
		"createTable":      createDDL,
		"engine":           status.Engine,
		"rowFormat":        status.RowFormat,
		"rowsEstimate":     status.Rows,
		"avgRowLength":     status.AvgRowLength,
		"dataLength":       dataLen,
		"dataLengthHuman":  humanBytes(dataLen),
		"indexLength":      idxLen,
		"indexLengthHuman": humanBytes(idxLen),
		"totalSizeBytes":   totalLen,
		"totalSizeHuman":   humanBytes(totalLen),
		"autoIncrement":    status.AutoIncrement,
		"collation":        status.Collation,
		"createTime":       status.CreateTime,
		"updateTime":       status.UpdateTime,
		"indexes":          indexes,
		"indexCount":       len(indexes),
		"primaryKey":       primary,
	}
	return out, nil
}

// scanCreateTable reads the second column of `SHOW CREATE TABLE`. The first is
// the table name (we already have it), the second is the DDL string — that's
// the only field operators care about for a schema audit.
func scanCreateTable(ctx context.Context, conn *sql.Conn, table string) (string, error) {
	row := conn.QueryRowContext(ctx, "SHOW CREATE TABLE `"+table+"`")
	var name, ddl string
	if err := row.Scan(&name, &ddl); err != nil {
		return "", fmt.Errorf("SHOW CREATE TABLE: %w", err)
	}
	return ddl, nil
}

// indexEntry is the per-key shape we surface — collapses the segment-per-row
// SHOW INDEX layout into one entry per index, with `columns` ordered by
// Seq_in_index. Cardinality is reported on the leading segment (MySQL's choice;
// it's a per-prefix estimate, not per-segment).
type indexEntry struct {
	KeyName     string   `json:"keyName"`
	Columns     []string `json:"columns"`
	Unique      bool     `json:"unique"`
	Type        string   `json:"type"`
	Cardinality int64    `json:"cardinality"`
}

// scanShowIndex parses SHOW INDEX rows into per-key entries. Returns the index
// list (insertion order = first-seen Key_name) and the PRIMARY columns
// separately for convenience — operators always ask "what's the primary key"
// first.
func scanShowIndex(ctx context.Context, conn *sql.Conn, table string) ([]indexEntry, []string, error) {
	rows, err := conn.QueryContext(ctx, "SHOW INDEX FROM `"+table+"`")
	if err != nil {
		return nil, nil, fmt.Errorf("SHOW INDEX FROM: %w", err)
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, nil, fmt.Errorf("SHOW INDEX columns: %w", err)
	}
	colIdx := map[string]int{}
	for i, c := range cols {
		colIdx[c] = i
	}

	// MySQL 5.7 / 8.0 both ship the columns we need. If a future MySQL renames
	// any of these we'd rather fail loudly than silently emit zeroed data.
	required := []string{"Key_name", "Non_unique", "Seq_in_index", "Column_name", "Index_type", "Cardinality"}
	for _, r := range required {
		if _, ok := colIdx[r]; !ok {
			return nil, nil, fmt.Errorf("SHOW INDEX missing required column %q", r)
		}
	}

	type segment struct {
		seq int64
		col string
	}
	byKey := map[string]*indexEntry{}
	segments := map[string][]segment{}
	keyOrder := []string{}
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, nil, fmt.Errorf("SHOW INDEX scan: %w", err)
		}
		key := stringFrom(vals[colIdx["Key_name"]])
		if _, seen := byKey[key]; !seen {
			byKey[key] = &indexEntry{
				KeyName:     key,
				Unique:      int64From(vals[colIdx["Non_unique"]]) == 0,
				Type:        stringFrom(vals[colIdx["Index_type"]]),
				Cardinality: int64From(vals[colIdx["Cardinality"]]),
			}
			keyOrder = append(keyOrder, key)
		}
		segments[key] = append(segments[key], segment{
			seq: int64From(vals[colIdx["Seq_in_index"]]),
			col: stringFrom(vals[colIdx["Column_name"]]),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("SHOW INDEX iter: %w", err)
	}

	out := make([]indexEntry, 0, len(keyOrder))
	var primary []string
	for _, k := range keyOrder {
		segs := segments[k]
		// Stable sort by Seq_in_index — small N (typically ≤4), insertion sort
		// inline avoids an import for sort.
		for i := 1; i < len(segs); i++ {
			for j := i; j > 0 && segs[j].seq < segs[j-1].seq; j-- {
				segs[j], segs[j-1] = segs[j-1], segs[j]
			}
		}
		cols := make([]string, len(segs))
		for i, s := range segs {
			cols[i] = s.col
		}
		entry := byKey[k]
		entry.Columns = cols
		out = append(out, *entry)
		if k == "PRIMARY" {
			primary = cols
		}
	}
	return out, primary, nil
}

// tableStatus mirrors the subset of SHOW TABLE STATUS LIKE we surface. Some
// columns are nullable (Rows on views, Update_time on InnoDB until you read it)
// — keep types loose and let the helpers tolerate nil.
type tableStatus struct {
	Engine        string
	RowFormat     string
	Rows          int64
	AvgRowLength  int64
	DataLength    uint64
	IndexLength   uint64
	AutoIncrement int64
	Collation     string
	CreateTime    string
	UpdateTime    string
}

func scanTableStatus(ctx context.Context, conn *sql.Conn, table string) (tableStatus, error) {
	// MySQL rejects ? placeholders inside SHOW commands ("Error 1064 (42000)
	// ... near '?' at line 1") — the prepared-statement protocol doesn't
	// expand parameters in admin statements. The table name is already
	// validated by reSafeIdentifier (^[A-Za-z0-9_]+$), so inlining as a
	// quoted literal is safe.
	rows, err := conn.QueryContext(ctx, "SHOW TABLE STATUS LIKE '"+table+"'")
	if err != nil {
		return tableStatus{}, fmt.Errorf("SHOW TABLE STATUS: %w", err)
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return tableStatus{}, fmt.Errorf("SHOW TABLE STATUS columns: %w", err)
	}
	colIdx := map[string]int{}
	for i, c := range cols {
		colIdx[c] = i
	}
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return tableStatus{}, fmt.Errorf("SHOW TABLE STATUS iter: %w", err)
		}
		return tableStatus{}, fmt.Errorf("table %q not found in current database", table)
	}
	vals := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	if err := rows.Scan(ptrs...); err != nil {
		return tableStatus{}, fmt.Errorf("SHOW TABLE STATUS scan: %w", err)
	}
	get := func(name string) any {
		if i, ok := colIdx[name]; ok {
			return vals[i]
		}
		return nil
	}
	st := tableStatus{
		Engine:        stringFrom(get("Engine")),
		RowFormat:     stringFrom(get("Row_format")),
		Rows:          int64From(get("Rows")),
		AvgRowLength:  int64From(get("Avg_row_length")),
		DataLength:    uint64From(get("Data_length")),
		IndexLength:   uint64From(get("Index_length")),
		AutoIncrement: int64From(get("Auto_increment")),
		Collation:     stringFrom(get("Collation")),
		CreateTime:    stringFrom(get("Create_time")),
		UpdateTime:    stringFrom(get("Update_time")),
	}
	return st, nil
}

// stringFrom coerces driver-decoded values into a string. The mysql driver
// returns []byte for VARCHAR/CHAR and time.Time for DATETIME — both have to
// land as strings in the JSON response.
func stringFrom(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case []byte:
		return string(x)
	case time.Time:
		if x.IsZero() {
			return ""
		}
		return x.Format("2006-01-02 15:04:05")
	default:
		return fmt.Sprintf("%v", x)
	}
}

func int64From(v any) int64 {
	switch x := v.(type) {
	case nil:
		return 0
	case int64:
		return x
	case uint64:
		return int64(x)
	case int:
		return int64(x)
	case []byte:
		return parseInt64(string(x))
	case string:
		return parseInt64(x)
	default:
		return 0
	}
}

func uint64From(v any) uint64 {
	switch x := v.(type) {
	case nil:
		return 0
	case uint64:
		return x
	case int64:
		if x < 0 {
			return 0
		}
		return uint64(x)
	case int:
		if x < 0 {
			return 0
		}
		return uint64(x)
	case []byte:
		return uint64(parseInt64(string(x)))
	case string:
		return uint64(parseInt64(x))
	default:
		return 0
	}
}

// parseInt64 is a forgiving signed-int parser — trims whitespace, returns 0
// on any malformed input. SHOW TABLE STATUS reports nullable numerics as the
// literal string "NULL" on some drivers; treat those as zero rather than
// surfacing a parse error to the caller.
func parseInt64(s string) int64 {
	s = strings.TrimSpace(s)
	if s == "" || s == "NULL" {
		return 0
	}
	var n int64
	neg := false
	i := 0
	if s[0] == '-' {
		neg = true
		i = 1
	}
	for ; i < len(s); i++ {
		c := s[i]
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int64(c-'0')
	}
	if neg {
		return -n
	}
	return n
}
