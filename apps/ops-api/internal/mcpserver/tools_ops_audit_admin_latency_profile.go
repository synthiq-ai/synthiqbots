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
	opsAuditAdminLatencyProfileTimeout       = 10 * time.Second
	opsAuditAdminLatencyProfileDefaultTopN   = 25
	opsAuditAdminLatencyProfileMaxTopN       = 200
	opsAuditAdminLatencyProfileRowCap        = 50000
	opsAuditAdminLatencyProfileDefaultWindow = "24h"
	opsAuditAdminLatencyProfileMaxWindow     = 168 * time.Hour
)

// RegisterOpsAuditAdminLatencyProfileTool registers
// `ops_audit_admin_latency_profile` — the per-tool tail-latency companion to
// `ops_audit_admin_summary`. The summary tool returns min/avg/max duration per
// tool, which collapses the long-tail story — an ops-api tool with avg 50 ms
// can still spike to p95 = 4000 ms and the operator only sees "max 4000" with
// no idea how often the tail fires. p50/p95 close that gap.
//
// The shape mirrors `wow_bot_latency_profile` (gateway-audit per-bot
// percentile profile) one floor down for the admin-audit table: same
// nearest-rank percentile math, same 50000-row scan cap, same 168h/7d window
// cap, same DBDeps.QueryDB (ops_ro) pool. The choice to ship a sibling tool
// rather than fold p50/p95 columns into `ops_audit_admin_summary` matches
// PR #137's reasoning — the cheap summary path stays cheap (single
// pre-aggregated SQL round trip), and agent tool-selection latches on the
// distinct question shape ("what's the tail?" vs "what's the count?").
//
// Backlog provenance: covers the operator-facing half of the `ops_metrics`
// wedge ("P50/P95 for ops-api itself"). The other half — req/sec and error-
// rate dashboards — is better served by `ops_audit_admin_summary` (already
// shipped) or a future Prometheus-shaped sidecar.
func RegisterOpsAuditAdminLatencyProfileTool(reg *Registry, deps DBDeps) {
	reg.Register(Tool{
		Name: "ops_audit_admin_latency_profile",
		Description: "Composite per-tool duration profile aggregated from " +
			"mod_ollama_chat_admin_audit. Returns {window, since, scannedRows, " +
			"tools:[{tool, calls, errors, errorRatePct, minDurationMs, " +
			"avgDurationMs, maxDurationMs, p50DurationMs, p95DurationMs}]} " +
			"sorted by calls desc. Tail-latency companion to " +
			"ops_audit_admin_summary (which exposes min/avg/max only) — use this " +
			"to answer \"which ops-api tool is the worst tail offender?\" " +
			"without paging up to 1000 raw rows from ops_audit_admin. " +
			"Percentiles computed server-side via nearest-rank from one SQL " +
			"round trip over the ops_ro pool. Bounded by a 24h default window " +
			"(cap 168h/7d) and a 50000-row scan cap. Optional filters: tool " +
			"(single-tool drilldown), errorsOnly (result='error' only), topN " +
			"(default 25, max 200). Read-only, 10s timeout.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{
			"tool":{"type":"string","description":"Restrict to one ops-api tool name (drilldown)"},
			"window":{"type":"string","description":"Lookback window (default 24h, cap 168h/7d)"},
			"topN":{"type":"integer","description":"Max tools returned, sorted by calls desc (default 25, max 200)"},
			"errorsOnly":{"type":"boolean","description":"Only profile rows with result='error'"}
		}}`),
		Annotations: AnnRead(),
		Handler: func(ctx context.Context, raw json.RawMessage, _ string) any {
			var a struct {
				Tool       string `json:"tool"`
				Window     string `json:"window"`
				TopN       int    `json:"topN"`
				ErrorsOnly bool   `json:"errorsOnly"`
			}
			if err := json.Unmarshal(raw, &a); err != nil {
				return map[string]any{"error": "decode args: " + err.Error()}
			}
			if deps.QueryDB == nil {
				return map[string]any{"error": "ops_ro pool not configured (DB_DSN empty?)"}
			}
			window := a.Window
			if window == "" {
				window = opsAuditAdminLatencyProfileDefaultWindow
			}
			d, err := time.ParseDuration(window)
			if err != nil || d <= 0 {
				return map[string]any{"error": "window must be a positive duration (e.g. 1h, 24h, 168h)"}
			}
			if d > opsAuditAdminLatencyProfileMaxWindow {
				return map[string]any{"error": "window cap is 168h (7d)"}
			}
			topN := a.TopN
			if topN <= 0 {
				topN = opsAuditAdminLatencyProfileDefaultTopN
			}
			if topN > opsAuditAdminLatencyProfileMaxTopN {
				topN = opsAuditAdminLatencyProfileMaxTopN
			}
			c, cancel := context.WithTimeout(ctx, opsAuditAdminLatencyProfileTimeout)
			defer cancel()
			out, err := collectOpsAuditAdminLatencyProfile(c, deps.QueryDB, d, a.Tool, topN, a.ErrorsOnly)
			if err != nil {
				return map[string]any{"error": err.Error()}
			}
			return out
		},
	})
}

type opsAuditAdminLatencyBucket struct {
	Tool          string  `json:"tool"`
	Calls         uint64  `json:"calls"`
	Errors        uint64  `json:"errors"`
	ErrorRatePct  float64 `json:"errorRatePct"`
	MinDurationMs uint64  `json:"minDurationMs"`
	AvgDurationMs uint64  `json:"avgDurationMs"`
	MaxDurationMs uint64  `json:"maxDurationMs"`
	P50DurationMs uint64  `json:"p50DurationMs"`
	P95DurationMs uint64  `json:"p95DurationMs"`
}

type opsAuditAdminLatencyProfileResult struct {
	Window      string                       `json:"window"`
	Since       time.Time                    `json:"since"`
	ScannedRows uint64                       `json:"scannedRows"`
	Tools       []opsAuditAdminLatencyBucket `json:"tools"`
}

// collectOpsAuditAdminLatencyProfile pulls one window of admin-audit rows in a
// single query, then folds them into per-tool percentile buckets server-side.
// Split out so tests can drive it directly with sqlmock.
//
// SQL note: admin_audit marks errors via result='error' (string column), NOT a
// boolean `error` column like gateway_audit. The error flag is synthesized at
// SELECT time via CASE so the row scan stays a single uint8 — keeps the fold
// loop shape identical to collectBotLatencyProfile.
func collectOpsAuditAdminLatencyProfile(ctx context.Context, db *sql.DB, d time.Duration, toolFilter string, topN int, errorsOnly bool) (*opsAuditAdminLatencyProfileResult, error) {
	since := time.Now().UTC().Add(-d)
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire conn: %w", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "USE `acore_characters`"); err != nil {
		return nil, fmt.Errorf("USE acore_characters: %w", err)
	}

	conds := []string{"ts >= ?"}
	args := []any{since}
	if toolFilter != "" {
		conds = append(conds, "tool = ?")
		args = append(args, toolFilter)
	}
	if errorsOnly {
		conds = append(conds, "result = 'error'")
	}
	args = append(args, opsAuditAdminLatencyProfileRowCap)
	q := "SELECT tool, duration_ms, CASE WHEN result = 'error' THEN 1 ELSE 0 END FROM `mod_ollama_chat_admin_audit` WHERE " +
		strings.Join(conds, " AND ") + " ORDER BY tool LIMIT ?"

	rows, err := conn.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("query: %w", err)
	}
	defer rows.Close()

	durByTool := make(map[string][]uint64)
	errByTool := make(map[string]uint64)
	sumByTool := make(map[string]uint64)
	minByTool := make(map[string]uint64)
	maxByTool := make(map[string]uint64)
	var scanned uint64
	for rows.Next() {
		var tool string
		var ms uint64
		var errFlag uint8
		if err := rows.Scan(&tool, &ms, &errFlag); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		scanned++
		durByTool[tool] = append(durByTool[tool], ms)
		if errFlag != 0 {
			errByTool[tool]++
		}
		sumByTool[tool] += ms
		if mn, ok := minByTool[tool]; !ok || ms < mn {
			minByTool[tool] = ms
		}
		if mx, ok := maxByTool[tool]; !ok || ms > mx {
			maxByTool[tool] = ms
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iter: %w", err)
	}

	buckets := make([]opsAuditAdminLatencyBucket, 0, len(durByTool))
	for tool, durs := range durByTool {
		sort.Slice(durs, func(i, j int) bool { return durs[i] < durs[j] })
		calls := uint64(len(durs))
		b := opsAuditAdminLatencyBucket{
			Tool:          tool,
			Calls:         calls,
			Errors:        errByTool[tool],
			MinDurationMs: minByTool[tool],
			MaxDurationMs: maxByTool[tool],
			AvgDurationMs: sumByTool[tool] / calls,
			P50DurationMs: nearestRankPercentile(durs, 0.5),
			P95DurationMs: nearestRankPercentile(durs, 0.95),
		}
		// Round to 1 decimal — matches AdminAuditBucket / wow_bot_latency_profile
		// so operators don't see two different precisions across tools.
		b.ErrorRatePct = float64(int(float64(b.Errors)/float64(calls)*1000+0.5)) / 10
		buckets = append(buckets, b)
	}
	sort.Slice(buckets, func(i, j int) bool {
		if buckets[i].Calls != buckets[j].Calls {
			return buckets[i].Calls > buckets[j].Calls
		}
		// Tool-asc tiebreaker so equal-call counts stay deterministic across
		// test runs and across Go's randomized map-iteration order.
		return buckets[i].Tool < buckets[j].Tool
	})
	if len(buckets) > topN {
		buckets = buckets[:topN]
	}
	return &opsAuditAdminLatencyProfileResult{
		Window:      d.String(),
		Since:       since,
		ScannedRows: scanned,
		Tools:       buckets,
	}, nil
}
