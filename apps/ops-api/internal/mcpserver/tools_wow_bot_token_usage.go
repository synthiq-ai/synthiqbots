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
	wowBotTokenUsageTimeout       = 10 * time.Second
	wowBotTokenUsageDefaultTopN   = 25
	wowBotTokenUsageMaxTopN       = 200
	wowBotTokenUsageRowCap        = 50000
	wowBotTokenUsageDefaultWindow = "24h"
	wowBotTokenUsageMaxWindow     = 168 * time.Hour
)

// RegisterWowBotTokenUsageTool registers `wow_bot_token_usage` — a composite
// read-only tool that rolls up per-bot LLM token spend + request/response
// payload sizes from mod_ollama_chat_gateway_audit. The audit table carries
// prompt_tokens / completion_tokens / request_chars / response_chars on every
// row, but NO existing rollup surfaces them: wow_bot_latency_profile aggregates
// latency_ms, wow_bot_source_channel_breakdown aggregates source_channel, and
// wow_bot_lookup only dumps a single bot's raw recent rows. This is the missing
// "which bot is burning the most tokens?" / cost-attribution view.
//
// Completes the gateway_audit per-bot rollup trio: latency shape
// (wow_bot_latency_profile), channel mix (wow_bot_source_channel_breakdown),
// and now token + payload volume (this tool) — the same single-query-then-fold
// pattern across all three so the window/topN/botGuid/errorsOnly knobs behave
// identically.
func RegisterWowBotTokenUsageTool(reg *Registry, deps DBDeps) {
	reg.Register(Tool{
		Name: "wow_bot_token_usage",
		Description: "Composite per-bot LLM token + payload-size usage rolled up " +
			"from mod_ollama_chat_gateway_audit. Returns {window, since, " +
			"scannedRows, totals:{calls, promptTokens, completionTokens, " +
			"totalTokens, requestChars, responseChars}, bots:[{botGuid, calls, " +
			"errors, errorRatePct, promptTokens, completionTokens, totalTokens, " +
			"avgTotalTokensPerCall, requestChars, responseChars}]} with bots " +
			"sorted by totalTokens desc. Use when an operator asks \"which bot is " +
			"burning the most LLM tokens?\", to attribute token cost across the " +
			"fleet, or to spot a bot generating oversized prompts/responses. The " +
			"token sibling of wow_bot_latency_profile (latency) and " +
			"wow_bot_source_channel_breakdown (channel mix) — same single SQL " +
			"round trip over the ops_ro pool, sums computed server-side. Note: " +
			"proactive source_channels (proact_*) record 0 tokens in this schema, " +
			"so a quiet/proactive-only window legitimately reports zero token " +
			"totals while requestChars/responseChars still reflect payload size. " +
			"Bounded by a 24h default window (cap 168h/7d) and a 50000-row scan " +
			"cap; totals stay honest across the full scan even when topN trims the " +
			"bots list. Optional filters: botGuid (single-bot drilldown), " +
			"errorsOnly, topN (default 25, max 200). Read-only, 10s timeout.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{
			"botGuid":{"type":"integer","description":"Restrict to one bot (drilldown)"},
			"window":{"type":"string","description":"Lookback window (default 24h, cap 168h/7d)"},
			"topN":{"type":"integer","description":"Max bots returned, sorted by totalTokens desc (default 25, max 200)"},
			"errorsOnly":{"type":"boolean","description":"Only count rows with error = 1"}
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
				window = wowBotTokenUsageDefaultWindow
			}
			d, err := time.ParseDuration(window)
			if err != nil || d <= 0 {
				return map[string]any{"error": "window must be a positive duration (e.g. 1h, 24h, 168h)"}
			}
			if d > wowBotTokenUsageMaxWindow {
				return map[string]any{"error": "window cap is 168h (7d)"}
			}
			topN := a.TopN
			if topN <= 0 {
				topN = wowBotTokenUsageDefaultTopN
			}
			if topN > wowBotTokenUsageMaxTopN {
				topN = wowBotTokenUsageMaxTopN
			}
			botGuid := numToString(a.BotGuid)
			if botGuid != "" {
				if _, err := strconv.ParseUint(botGuid, 10, 64); err != nil {
					return map[string]any{"error": "botGuid must be a positive integer"}
				}
			}
			c, cancel := context.WithTimeout(ctx, wowBotTokenUsageTimeout)
			defer cancel()
			out, err := collectBotTokenUsage(c, deps.QueryDB, d, botGuid, topN, a.ErrorsOnly)
			if err != nil {
				return map[string]any{"error": err.Error()}
			}
			return out
		},
	})
}

type botTokenUsageBucket struct {
	BotGuid               uint64  `json:"botGuid"`
	Calls                 uint64  `json:"calls"`
	Errors                uint64  `json:"errors"`
	ErrorRatePct          float64 `json:"errorRatePct"`
	PromptTokens          uint64  `json:"promptTokens"`
	CompletionTokens      uint64  `json:"completionTokens"`
	TotalTokens           uint64  `json:"totalTokens"`
	AvgTotalTokensPerCall uint64  `json:"avgTotalTokensPerCall"`
	RequestChars          uint64  `json:"requestChars"`
	ResponseChars         uint64  `json:"responseChars"`
}

type botTokenUsageTotals struct {
	Calls            uint64 `json:"calls"`
	PromptTokens     uint64 `json:"promptTokens"`
	CompletionTokens uint64 `json:"completionTokens"`
	TotalTokens      uint64 `json:"totalTokens"`
	RequestChars     uint64 `json:"requestChars"`
	ResponseChars    uint64 `json:"responseChars"`
}

type botTokenUsageResult struct {
	Window      string                `json:"window"`
	Since       time.Time             `json:"since"`
	ScannedRows uint64                `json:"scannedRows"`
	Totals      botTokenUsageTotals   `json:"totals"`
	Bots        []botTokenUsageBucket `json:"bots"`
}

// collectBotTokenUsage pulls one window of audit rows in a single query, then
// folds them into per-bot token/char buckets server-side. Split out so tests
// can drive it directly with sqlmock. Fleet-wide totals accumulate across EVERY
// scanned row (before the topN slice) so the denominator a caller would use to
// compute a bot's share of fleet token spend stays honest even when topN trims
// the displayed bots — the AH-tool honest-pre-truncation-totals convention.
func collectBotTokenUsage(ctx context.Context, db *sql.DB, d time.Duration, botGuid string, topN int, errorsOnly bool) (*botTokenUsageResult, error) {
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
	args = append(args, wowBotTokenUsageRowCap)
	q := "SELECT bot_guid, prompt_tokens, completion_tokens, request_chars, response_chars, error " +
		"FROM `mod_ollama_chat_gateway_audit` WHERE " +
		strings.Join(conds, " AND ") + " ORDER BY bot_guid LIMIT ?"

	rows, err := conn.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("query: %w", err)
	}
	defer rows.Close()

	type tokenAgg struct {
		calls            uint64
		errors           uint64
		promptTokens     uint64
		completionTokens uint64
		requestChars     uint64
		responseChars    uint64
	}
	byBot := make(map[uint64]*tokenAgg)
	var scanned uint64
	var totals botTokenUsageTotals
	for rows.Next() {
		var guid, pt, ct, rc, rsc uint64
		var errFlag uint8
		if err := rows.Scan(&guid, &pt, &ct, &rc, &rsc, &errFlag); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		scanned++
		agg, ok := byBot[guid]
		if !ok {
			agg = &tokenAgg{}
			byBot[guid] = agg
		}
		agg.calls++
		agg.promptTokens += pt
		agg.completionTokens += ct
		agg.requestChars += rc
		agg.responseChars += rsc
		if errFlag != 0 {
			agg.errors++
		}
		totals.Calls++
		totals.PromptTokens += pt
		totals.CompletionTokens += ct
		totals.RequestChars += rc
		totals.ResponseChars += rsc
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iter: %w", err)
	}
	totals.TotalTokens = totals.PromptTokens + totals.CompletionTokens

	buckets := make([]botTokenUsageBucket, 0, len(byBot))
	for guid, agg := range byBot {
		b := botTokenUsageBucket{
			BotGuid:          guid,
			Calls:            agg.calls,
			Errors:           agg.errors,
			PromptTokens:     agg.promptTokens,
			CompletionTokens: agg.completionTokens,
			TotalTokens:      agg.promptTokens + agg.completionTokens,
			RequestChars:     agg.requestChars,
			ResponseChars:    agg.responseChars,
		}
		// calls > 0 always here (a bucket exists only because a row landed in it).
		b.AvgTotalTokensPerCall = b.TotalTokens / agg.calls
		// Round to 1 decimal — matches the AdminAuditBucket / sibling-tool
		// convention so operators don't see two precisions across tools.
		b.ErrorRatePct = float64(int(float64(b.Errors)/float64(agg.calls)*1000+0.5)) / 10
		buckets = append(buckets, b)
	}
	// Worst-first: most token spend on top, botGuid asc as a stable tiebreaker
	// (Go map iteration order is randomized — without the tiebreak two bots with
	// equal token totals would swap places across calls).
	sort.Slice(buckets, func(i, j int) bool {
		if buckets[i].TotalTokens != buckets[j].TotalTokens {
			return buckets[i].TotalTokens > buckets[j].TotalTokens
		}
		return buckets[i].BotGuid < buckets[j].BotGuid
	})
	if len(buckets) > topN {
		buckets = buckets[:topN]
	}
	return &botTokenUsageResult{
		Window:      d.String(),
		Since:       since,
		ScannedRows: scanned,
		Totals:      totals,
		Bots:        buckets,
	}, nil
}
