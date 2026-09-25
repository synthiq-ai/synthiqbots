package mcpserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

const (
	dbProcessListTimeout    = 5 * time.Second
	dbProcessListDefaultMax = 200
	dbProcessListHardMax    = 500
	// infoMaxBytes caps the per-row INFO column (the running SQL text). A
	// runaway INSERT or LOAD DATA can carry a multi-megabyte payload — we
	// don't want the entire MCP response to balloon when one row is huge.
	dbProcessListInfoMaxBytes = 800
)

// processListRow is the parsed shape of one row from
// information_schema.PROCESSLIST. JSON tags match the lowerCamelCase shape
// used by db_table_info / db_explain so the agent gets a consistent surface
// across db_* tools.
type processListRow struct {
	ID            int64  `json:"id"`
	User          string `json:"user"`
	Host          string `json:"host"`
	DB            string `json:"db,omitempty"`
	Command       string `json:"command"`
	Time          int64  `json:"time"`
	State         string `json:"state,omitempty"`
	Info          string `json:"info,omitempty"`
	InfoTruncated bool   `json:"infoTruncated,omitempty"`
}

// RegisterDBProcessListTool registers `db_processlist` — a curated wrapper
// over `SELECT ... FROM information_schema.PROCESSLIST`. Operators previously
// could not access PROCESSLIST via db_query (information_schema is in
// reForbiddenSchema), but it's the canonical first call when diagnosing a
// "worldserver feels slow" or "DB load spiked" report. db_processlist exposes
// it under structured filters with no user-supplied SQL surface.
//
// Defaults are tuned for the diagnostic use case: idle Sleep connections are
// excluded (they're 90%+ of PROCESSLIST during normal operation and crowd
// out the slow query that actually triggered the page), results are ordered
// by TIME DESC (oldest connections first — most likely to be stuck), and
// INFO is byte-capped so one runaway query can't blow up the response.
func RegisterDBProcessListTool(reg *Registry, deps DBDeps) {
	reg.Register(Tool{
		Name: "db_processlist",
		Description: "Snapshot of currently running MySQL connections from " +
			"information_schema.PROCESSLIST. Returns `rows` (one entry per " +
			"connection: id/user/host/db/command/time/state/info) ordered by " +
			"TIME DESC so the oldest / most likely stuck queries surface " +
			"first. Defaults exclude idle 'Sleep' connections — set " +
			"`includeSleep:true` to include them. `minTime` (seconds) filters " +
			"out connections younger than that, useful for surfacing only " +
			"long-running queries. `user` / `database` apply exact-match " +
			"filters. The `info` column (the running SQL text) is capped at " +
			"800 bytes per row with `infoTruncated:true` when elided. Also " +
			"returns `byCommand` (count of returned rows grouped by COMMAND) " +
			"for a one-glance summary. Read-only, uses the ops_ro pool — " +
			"sees all connections only if ops_ro has the PROCESS privilege; " +
			"without it MySQL returns only ops_ro's own connections (still " +
			"useful for verifying ops-api's own activity). 5 s timeout, hard " +
			"cap 500 rows.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{
			"includeSleep":{"type":"boolean","description":"Include idle Sleep connections (default false)"},
			"minTime":{"type":"integer","description":"Minimum connection age in seconds (default 0)"},
			"user":{"type":"string","description":"Filter to a specific MySQL user (exact match)"},
			"database":{"type":"string","description":"Filter to a specific db (exact match; rows with no db are excluded when set)"},
			"limit":{"type":"integer","description":"Max rows (default 200, hard cap 500)"}
		}}`),
		Annotations: AnnRead(),
		Handler: func(ctx context.Context, raw json.RawMessage, _ string) any {
			var a struct {
				IncludeSleep bool   `json:"includeSleep"`
				MinTime      int64  `json:"minTime"`
				User         string `json:"user"`
				Database     string `json:"database"`
				Limit        int    `json:"limit"`
			}
			if err := json.Unmarshal(raw, &a); err != nil {
				return map[string]any{"error": "decode args: " + err.Error()}
			}
			if deps.QueryDB == nil {
				return map[string]any{"error": "ops_ro pool not configured (DB_DSN empty?)"}
			}
			if a.Limit <= 0 {
				a.Limit = dbProcessListDefaultMax
			}
			if a.Limit > dbProcessListHardMax {
				a.Limit = dbProcessListHardMax
			}
			if a.MinTime < 0 {
				a.MinTime = 0
			}
			c, cancel := context.WithTimeout(ctx, dbProcessListTimeout)
			defer cancel()
			out, err := collectProcessList(c, deps.QueryDB, a.IncludeSleep, a.MinTime, a.User, a.Database, a.Limit)
			if err != nil {
				return map[string]any{"error": err.Error()}
			}
			return out
		},
	})
}

// collectProcessList builds a parameterized SELECT against
// information_schema.PROCESSLIST. The column list is fixed; user input only
// ever lands in ? placeholders. Returns the response shape ready to ship to
// the MCP client.
func collectProcessList(ctx context.Context, db *sql.DB, includeSleep bool, minTime int64, userFilter, dbFilter string, limit int) (map[string]any, error) {
	var q strings.Builder
	q.WriteString(`SELECT ID, USER, HOST, IFNULL(DB,''), COMMAND, TIME, IFNULL(STATE,''), IFNULL(INFO,'')
		FROM information_schema.PROCESSLIST
		WHERE 1=1`)
	args := []any{}
	if !includeSleep {
		q.WriteString(" AND COMMAND != ?")
		args = append(args, "Sleep")
	}
	if minTime > 0 {
		q.WriteString(" AND TIME >= ?")
		args = append(args, minTime)
	}
	if userFilter != "" {
		q.WriteString(" AND USER = ?")
		args = append(args, userFilter)
	}
	if dbFilter != "" {
		q.WriteString(" AND DB = ?")
		args = append(args, dbFilter)
	}
	q.WriteString(" ORDER BY TIME DESC LIMIT ?")
	args = append(args, limit)

	rows, err := db.QueryContext(ctx, q.String(), args...)
	if err != nil {
		return nil, fmt.Errorf("query processlist: %w", err)
	}
	defer rows.Close()

	out := make([]processListRow, 0, 32)
	byCommand := map[string]int{}
	for rows.Next() {
		var r processListRow
		if err := rows.Scan(&r.ID, &r.User, &r.Host, &r.DB, &r.Command, &r.Time, &r.State, &r.Info); err != nil {
			return nil, fmt.Errorf("scan processlist: %w", err)
		}
		if len(r.Info) > dbProcessListInfoMaxBytes {
			r.Info = r.Info[:dbProcessListInfoMaxBytes] + "…"
			r.InfoTruncated = true
		}
		byCommand[r.Command]++
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate processlist: %w", err)
	}
	return map[string]any{
		"rows":         out,
		"rowCount":     len(out),
		"byCommand":    byCommand,
		"includeSleep": includeSleep,
		"minTime":      minTime,
		"limit":        limit,
	}, nil
}
