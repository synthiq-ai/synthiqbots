package mcpserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	wowBotLatencyProfileTimeout       = 10 * time.Second
	wowBotLatencyProfileDefaultTopN   = 25
	wowBotLatencyProfileMaxTopN       = 200
	wowBotLatencyProfileRowCap        = 50000
	wowBotLatencyProfileDefaultWindow = "24h"
	wowBotLatencyProfileMaxWindow     = 168 * time.Hour
)

// RegisterWowBotLatencyProfileTool registers `wow_bot_latency_profile` — a
// composite read-only tool that aggregates per-bot latency from
// mod_ollama_chat_gateway_audit into a percentile profile (min/avg/max/p50/
// p95) plus call + error counts. Operators triaging "this bot feels slow"
// previously had to pull raw audit rows via ops_audit_gateway and compute
// percentiles manually — this collapses that into one tool call with the
// percentile math pushed server-side.
//
// Pairs with wow_bot_lookup (per-bot triage snapshot) and ops_audit_summary
// (counts-only fleet overview): this tool sits between them, giving fleet-
// wide latency shape without forcing a per-bot drilldown.
func RegisterWowBotLatencyProfileTool(reg *Registry, deps DBDeps) {
	reg.Register(Tool{
		Name: "wow_bot_latency_profile",
		Description: "Composite per-bot latency profile aggregated from " +
			"mod_ollama_chat_gateway_audit. Returns {window, since, scannedRows, " +
			"bots:[{botGuid, calls, errors, errorRatePct, minLatencyMs, " +
			"avgLatencyMs, maxLatencyMs, p50LatencyMs, p95LatencyMs}]} sorted by " +
			"calls desc. Use when an operator says \"this bot feels slow\" or to " +
			"compare hot bots against the fleet median. Replaces the manual " +
			"\"ops_audit_gateway + spreadsheet\" workflow. One SQL round trip " +
			"over the ops_ro pool — percentiles computed server-side. Bounded by " +
			"a 24h default window (cap 168h/7d) and a 50000-row scan cap. " +
			"Optional filters: botGuid (single-bot drilldown), errorsOnly, topN " +
			"(default 25, max 200). Read-only, 10s timeout.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{
			"botGuid":{"type":"integer","description":"Restrict to one bot (drilldown)"},
			"window":{"type":"string","description":"Lookback window (default 24h, cap 168h/7d)"},
			"topN":{"type":"integer","description":"Max bots returned, sorted by calls desc (default 25, max 200)"},
			"errorsOnly":{"type":"boolean","description":"Only profile rows with error = 1"}
		}}`),
		Annotations: AnnRead(),
		Handler: func(ctx context.Context, raw json.RawMessage, _ string) any {
			var a struct {
				BotGuid    any    `json:"botGuid"`
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
				window = wowBotLatencyProfileDefaultWindow
			}
			d, err := time.ParseDuration(window)
			if err != nil || d <= 0 {
				return map[string]any{"error": "window must be a positive duration (e.g. 1h, 24h, 168h)"}
			}
			if d > wowBotLatencyProfileMaxWindow {
				return map[string]any{"error": "window cap is 168h (7d)"}
			}
			topN := a.TopN
			if topN <= 0 {
				topN = wowBotLatencyProfileDefaultTopN
			}
			if topN > wowBotLatencyProfileMaxTopN {
				topN = wowBotLatencyProfileMaxTopN
			}
			botGuid := numToString(a.BotGuid)
			if botGuid != "" {
				if _, err := strconv.ParseUint(botGuid, 10, 64); err != nil {
					return map[string]any{"error": "botGuid must be a positive integer"}
				}
			}
			c, cancel := context.WithTimeout(ctx, wowBotLatencyProfileTimeout)
			defer cancel()
			out, err := collectBotLatencyProfile(c, deps.QueryDB, d, botGuid, topN, a.ErrorsOnly)
			if err != nil {
				return map[string]any{"error": err.Error()}
			}
			return out
		},
	})
}

type botLatencyBucket struct {
	BotGuid      uint64  `json:"botGuid"`
	Calls        uint64  `json:"calls"`
	Errors       uint64  `json:"errors"`
	ErrorRatePct float64 `json:"errorRatePct"`
	MinLatencyMs uint64  `json:"minLatencyMs"`
	AvgLatencyMs uint64  `json:"avgLatencyMs"`
	MaxLatencyMs uint64  `json:"maxLatencyMs"`
	P50LatencyMs uint64  `json:"p50LatencyMs"`
	P95LatencyMs uint64  `json:"p95LatencyMs"`
}

type botLatencyProfileResult struct {
	Window      string             `json:"window"`
	Since       time.Time          `json:"since"`
	ScannedRows uint64             `json:"scannedRows"`
	Bots        []botLatencyBucket `json:"bots"`
}

// collectBotLatencyProfile pulls one window of audit rows in a single query,
// then folds them into per-bot percentile buckets server-side. Split out so
// tests can drive it directly with sqlmock.
func collectBotLatencyProfile(ctx context.Context, db *sql.DB, d time.Duration, botGuid string, topN int, errorsOnly bool) (*botLatencyProfileResult, error) {
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
	if botGuid != "" {
		n, _ := strconv.ParseUint(botGuid, 10, 64)
		conds = append(conds, "bot_guid = ?")
		args = append(args, n)
	}
	if errorsOnly {
		conds = append(conds, "error = 1")
	}
	args = append(args, wowBotLatencyProfileRowCap)
	q := "SELECT bot_guid, latency_ms, error FROM `mod_ollama_chat_gateway_audit` WHERE " +
		strings.Join(conds, " AND ") + " ORDER BY bot_guid LIMIT ?"

	rows, err := conn.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("query: %w", err)
	}
	defer rows.Close()

	latByBot := make(map[uint64][]uint64)
	errByBot := make(map[uint64]uint64)
	sumByBot := make(map[uint64]uint64)
	minByBot := make(map[uint64]uint64)
	maxByBot := make(map[uint64]uint64)
	var scanned uint64
	for rows.Next() {
		var guid, ms uint64
		var errFlag uint8
		if err := rows.Scan(&guid, &ms, &errFlag); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		scanned++
		latByBot[guid] = append(latByBot[guid], ms)
		if errFlag != 0 {
			errByBot[guid]++
		}
		sumByBot[guid] += ms
		if mn, ok := minByBot[guid]; !ok || ms < mn {
			minByBot[guid] = ms
		}
		if mx, ok := maxByBot[guid]; !ok || ms > mx {
			maxByBot[guid] = ms
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iter: %w", err)
	}

	buckets := make([]botLatencyBucket, 0, len(latByBot))
	for guid, lats := range latByBot {
		sort.Slice(lats, func(i, j int) bool { return lats[i] < lats[j] })
		calls := uint64(len(lats))
		b := botLatencyBucket{
			BotGuid:      guid,
			Calls:        calls,
			Errors:       errByBot[guid],
			MinLatencyMs: minByBot[guid],
			MaxLatencyMs: maxByBot[guid],
			AvgLatencyMs: sumByBot[guid] / calls,
			P50LatencyMs: nearestRankPercentile(lats, 0.5),
			P95LatencyMs: nearestRankPercentile(lats, 0.95),
		}
		// Round to 1 decimal — matches the AdminAuditBucket convention so
		// operators don't see two different precisions across tools.
		b.ErrorRatePct = float64(int(float64(b.Errors)/float64(calls)*1000+0.5)) / 10
		buckets = append(buckets, b)
	}
	sort.Slice(buckets, func(i, j int) bool {
		if buckets[i].Calls != buckets[j].Calls {
			return buckets[i].Calls > buckets[j].Calls
		}
		return buckets[i].BotGuid < buckets[j].BotGuid
	})
	if len(buckets) > topN {
		buckets = buckets[:topN]
	}
	return &botLatencyProfileResult{
		Window:      d.String(),
		Since:       since,
		ScannedRows: scanned,
		Bots:        buckets,
	}, nil
}

// nearestRankPercentile returns the value at the given p in [0,1] of an
// ascending-sorted slice using the nearest-rank method (rank = ceil(p*N),
// 1-indexed). Chosen over linear interpolation because operators reason
// about discrete latency values from the audit table, not interpolated ones.
// Empty input returns 0 (matches "no data" — caller already guards Calls > 0
// for any meaningful field).
func nearestRankPercentile(sorted []uint64, p float64) uint64 {
	n := len(sorted)
	if n == 0 {
		return 0
	}
	if n == 1 {
		return sorted[0]
	}
	// ceil(p*n) without math.Ceil — sorted is uint64; +0.9999... avoids the
	// float-rounding edge where p*n lands exactly on an integer.
	rank := int(float64(n)*p + 0.9999999)
	if rank < 1 {
		rank = 1
	}
	if rank > n {
		rank = n
	}
	return sorted[rank-1]
}
