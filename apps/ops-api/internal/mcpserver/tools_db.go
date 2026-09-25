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

type DBDeps struct {
	QueryDB *sql.DB // ops_ro pool — used by db_query
	ExecDB  *sql.DB // ops_rw pool — used by db_exec
}

const (
	dbQueryHardCapRows = 5000
	dbQueryTimeout     = 10 * time.Second
	dbExecTimeout      = 15 * time.Second
)

var (
	// Allowed read statements: SELECT, SHOW, EXPLAIN, DESCRIBE, DESC.
	reReadStatement = regexp.MustCompile(`(?is)^\s*(SELECT|SHOW|EXPLAIN|DESCRIBE|DESC)\b`)
	// Allowed write statements: INSERT, UPDATE, DELETE.
	reWriteStatement = regexp.MustCompile(`(?is)^\s*(INSERT|UPDATE|DELETE)\b`)
	// Banned write keywords (DDL / privilege ops).
	reBannedWrite = regexp.MustCompile(`(?is)\b(DROP|TRUNCATE|ALTER|CREATE|GRANT|REVOKE|RENAME)\b`)
	// UPDATE/DELETE without WHERE — coarse but catches the obvious footgun.
	reUpdateOrDelete = regexp.MustCompile(`(?is)^\s*(UPDATE|DELETE)\b`)
	reHasWhere       = regexp.MustCompile(`(?is)\bWHERE\b`)
	// Defense-in-depth: reject `mysql.*`, `information_schema.*`, etc. The
	// ops_ro/ops_rw GRANTs already block these, but the regex catches a
	// misconfigured GRANT before it hits the server. Match optional backtick
	// quoting around the schema name.
	reForbiddenSchema = regexp.MustCompile("(?is)" + "`?(mysql|information_schema|performance_schema|sys)`?\\.")
	// Allowed databases. We hardcode this rather than honoring an env var so
	// db_exec can never escape into mysql/sys/performance_schema even if the
	// ops_rw GRANT is mis-applied.
	allowedDatabases = map[string]bool{
		"acore_world":      true,
		"acore_characters": true,
		"acore_auth":       true,
	}
)

// RegisterDBTools registers db_query (read) and db_exec (write).
func RegisterDBTools(reg *Registry, deps DBDeps) {
	reg.Register(Tool{
		Name: "db_query",
		Description: "Run a read-only SQL statement against acore_world / acore_characters / " +
			"acore_auth using the ops_ro user. Allowed statements: SELECT, SHOW, EXPLAIN, " +
			"DESCRIBE. Use ? placeholders and pass values via `params`. Returns " +
			"{columns:[], rows:[[...],...], rowCount}. Hard cap 5000 rows, 10 s timeout.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{
			"database":{"type":"string","enum":["acore_world","acore_characters","acore_auth"]},
			"sql":{"type":"string","description":"SELECT/SHOW/EXPLAIN/DESCRIBE only"},
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
			if !reReadStatement.MatchString(a.SQL) {
				return map[string]any{"error": "only SELECT/SHOW/EXPLAIN/DESCRIBE allowed"}
			}
			if reBannedWrite.MatchString(a.SQL) {
				return map[string]any{"error": "statement contains a banned write keyword"}
			}
			if reForbiddenSchema.MatchString(a.SQL) {
				return map[string]any{"error": "queries against mysql/information_schema/performance_schema/sys are not allowed"}
			}
			if deps.QueryDB == nil {
				return map[string]any{"error": "ops_ro pool not configured (DB_DSN empty?)"}
			}
			c, cancel := context.WithTimeout(ctx, dbQueryTimeout)
			defer cancel()
			fullSQL := "USE `" + a.Database + "`; " + a.SQL
			// driver multi-statement requires explicit conn; instead, qualify table names.
			// Use the simpler approach: prepend "USE" only as a session change is awkward.
			// Practical alternative: rely on the database= parameter to qualify the
			// statement's tables (caller's responsibility). The driver pool may not
			// be on the right default schema — open a new pool per database lazily.
			_ = fullSQL // kept for clarity; we drive selection through SetDefaultDatabase below

			rows, err := queryWithDB(c, deps.QueryDB, a.Database, a.SQL, a.Params)
			if err != nil {
				return map[string]any{"error": err.Error()}
			}
			defer rows.Close()
			cols, err := rows.Columns()
			if err != nil {
				return map[string]any{"error": "columns: " + err.Error()}
			}
			data := [][]any{}
			rowCount := 0
			for rows.Next() {
				if rowCount >= dbQueryHardCapRows {
					return map[string]any{
						"error":   fmt.Sprintf("row cap exceeded (%d); narrow your query", dbQueryHardCapRows),
						"columns": cols, "rows": data, "rowCount": rowCount,
					}
				}
				vals := make([]any, len(cols))
				ptrs := make([]any, len(cols))
				for i := range vals {
					ptrs[i] = &vals[i]
				}
				if err := rows.Scan(ptrs...); err != nil {
					return map[string]any{"error": "scan: " + err.Error()}
				}
				for i, v := range vals {
					if b, ok := v.([]byte); ok {
						vals[i] = string(b)
					}
				}
				data = append(data, vals)
				rowCount++
			}
			if err := rows.Err(); err != nil {
				return map[string]any{"error": "rows: " + err.Error()}
			}
			return map[string]any{"columns": cols, "rows": data, "rowCount": rowCount}
		},
	})

	reg.Register(Tool{
		Name: "db_exec",
		Description: "Run a single INSERT/UPDATE/DELETE against acore_* using the ops_rw user. " +
			"REQUIRES `confirm:true`. UPDATE/DELETE must include a WHERE clause (refused " +
			"otherwise). DDL keywords (DROP/TRUNCATE/ALTER/CREATE/GRANT/REVOKE/RENAME) are " +
			"rejected. Use ? placeholders. Returns {rowsAffected, lastInsertId}.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{
			"database":{"type":"string","enum":["acore_world","acore_characters","acore_auth"]},
			"sql":{"type":"string","description":"INSERT/UPDATE/DELETE only"},
			"params":{"type":"array","items":{}},
			"confirm":{"type":"boolean","description":"MUST be true to actually execute"}
		},"required":["database","sql","confirm"]}`),
		Annotations: AnnAction(false),
		Destructive: true,
		Handler: func(ctx context.Context, raw json.RawMessage, _ string) any {
			var a struct {
				Database string        `json:"database"`
				SQL      string        `json:"sql"`
				Params   []interface{} `json:"params"`
				Confirm  bool          `json:"confirm"`
			}
			if err := json.Unmarshal(raw, &a); err != nil {
				return map[string]any{"error": "decode args: " + err.Error()}
			}
			if !a.Confirm {
				return map[string]any{"error": "confirm:true required for db_exec"}
			}
			if !allowedDatabases[a.Database] {
				return map[string]any{"error": "database must be one of acore_world/acore_characters/acore_auth"}
			}
			if !reWriteStatement.MatchString(a.SQL) {
				return map[string]any{"error": "only INSERT/UPDATE/DELETE allowed"}
			}
			if reBannedWrite.MatchString(a.SQL) {
				return map[string]any{"error": "statement contains a banned DDL keyword"}
			}
			if reForbiddenSchema.MatchString(a.SQL) {
				return map[string]any{"error": "writes against mysql/information_schema/performance_schema/sys are not allowed"}
			}
			if reUpdateOrDelete.MatchString(a.SQL) && !reHasWhere.MatchString(a.SQL) {
				return map[string]any{"error": "UPDATE/DELETE require a WHERE clause"}
			}
			// Guard against multi-statement execution.
			if strings.Contains(strings.TrimRight(a.SQL, " \t\r\n;"), ";") {
				return map[string]any{"error": "multiple statements not allowed"}
			}
			if deps.ExecDB == nil {
				return map[string]any{"error": "ops_rw pool not configured (DB_EXEC_DSN empty?)"}
			}
			c, cancel := context.WithTimeout(ctx, dbExecTimeout)
			defer cancel()
			res, err := execWithDB(c, deps.ExecDB, a.Database, a.SQL, a.Params)
			if err != nil {
				return map[string]any{"error": err.Error()}
			}
			ra, _ := res.RowsAffected()
			lid, _ := res.LastInsertId()
			out := map[string]any{"ok": true, "rowsAffected": ra, "lastInsertId": lid}
			if ra > 1000 {
				out["warning"] = fmt.Sprintf("affected %d rows — wider blast radius than typical, double-check intent", ra)
			}
			return out
		},
	})
}

// queryWithDB runs a SELECT against a specific database. We open a per-call
// connection from the shared pool and switch its default schema with USE.
// This avoids needing one *sql.DB per database.
func queryWithDB(ctx context.Context, db *sql.DB, database, query string, params []interface{}) (*sql.Rows, error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire conn: %w", err)
	}
	if _, err := conn.ExecContext(ctx, "USE `"+database+"`"); err != nil {
		conn.Close()
		return nil, fmt.Errorf("USE %s: %w", database, err)
	}
	rows, err := conn.QueryContext(ctx, query, params...)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("query: %w", err)
	}
	// Wrap rows so the conn is released when rows are closed. *sql.Rows holds
	// the conn until Close; calling Close on the underlying conn after Rows.Close
	// returns it to the pool — matches database/sql's expected lifecycle.
	go func() {
		<-ctx.Done()
		_ = conn.Close()
	}()
	return rows, nil
}

func execWithDB(ctx context.Context, db *sql.DB, database, statement string, params []interface{}) (sql.Result, error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire conn: %w", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "USE `"+database+"`"); err != nil {
		return nil, fmt.Errorf("USE %s: %w", database, err)
	}
	res, err := conn.ExecContext(ctx, statement, params...)
	if err != nil {
		return nil, fmt.Errorf("exec: %w", err)
	}
	return res, nil
}
