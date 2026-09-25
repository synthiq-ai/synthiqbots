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
	wowBotSrcChanBreakdownTimeout       = 10 * time.Second
	wowBotSrcChanBreakdownDefaultTopN   = 25
	wowBotSrcChanBreakdownMaxTopN       = 200
	wowBotSrcChanBreakdownRowCap        = 50000
	wowBotSrcChanBreakdownDefaultWindow = "24h"
	wowBotSrcChanBreakdownMaxWindow     = 168 * time.Hour
)

// RegisterWowBotSourceChannelBreakdownTool registers
// `wow_bot_source_channel_breakdown` — a per-bot distribution of
// mod_ollama_chat_gateway_audit rows grouped by source_channel, with avg
// latency + errors per channel. Pairs with wow_bot_latency_profile (where the
// bot is hot) to answer "what channel is the bot hot ON?".
func RegisterWowBotSourceChannelBreakdownTool(reg *Registry, deps DBDeps) {
	reg.Register(Tool{
		Name: "wow_bot_source_channel_breakdown",
		Description: "Composite per-bot source_channel distribution from " +
			"mod_ollama_chat_gateway_audit. Returns {window, since, scannedRows, " +
			"bots:[{botGuid, calls, channels:[{channel, calls, pct, errors, " +
			"avgLatencyMs}]}]} with bots sorted by total calls desc and channels " +
			"within each bot sorted by calls desc. Use when an operator asks " +
			"\"is this bot mostly yelling or whispering?\" or \"is the slowness " +
			"channel-specific?\". Companion to wow_bot_latency_profile: use the " +
			"latency profile to find a hot bot, then this to find what channel " +
			"they're hot about. One SQL round trip over the ops_ro pool. Bounded " +
			"by a 24h default window (cap 168h/7d) and a 50000-row scan cap. " +
			"Optional filters: botGuid (single-bot drilldown), errorsOnly, topN " +
			"(default 25, max 200). Read-only, 10s timeout.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{
			"botGuid":{"type":"integer","description":"Restrict to one bot (drilldown)"},
			"window":{"type":"string","description":"Lookback window (default 24h, cap 168h/7d)"},
			"topN":{"type":"integer","description":"Max bots returned, sorted by total calls desc (default 25, max 200)"},
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
				window = wowBotSrcChanBreakdownDefaultWindow
			}
			d, err := time.ParseDuration(window)
			if err != nil || d <= 0 {
				return map[string]any{"error": "window must be a positive duration (e.g. 1h, 24h, 168h)"}
			}
			if d > wowBotSrcChanBreakdownMaxWindow {
				return map[string]any{"error": "window cap is 168h (7d)"}
			}
			topN := a.TopN
			if topN <= 0 {
				topN = wowBotSrcChanBreakdownDefaultTopN
			}
			if topN > wowBotSrcChanBreakdownMaxTopN {
				topN = wowBotSrcChanBreakdownMaxTopN
			}
			botGuid := numToString(a.BotGuid)
			if botGuid != "" {
				if _, err := strconv.ParseUint(botGuid, 10, 64); err != nil {
					return map[string]any{"error": "botGuid must be a positive integer"}
				}
			}
			c, cancel := context.WithTimeout(ctx, wowBotSrcChanBreakdownTimeout)
			defer cancel()
			out, err := collectBotSourceChannelBreakdown(c, deps.QueryDB, d, botGuid, topN, a.ErrorsOnly)
			if err != nil {
				return map[string]any{"error": err.Error()}
			}
			return out
		},
	})
}

type botChannelBucket struct {
	Channel      string  `json:"channel"`
	Calls        uint64  `json:"calls"`
	Pct          float64 `json:"pct"`
	Errors       uint64  `json:"errors"`
	AvgLatencyMs uint64  `json:"avgLatencyMs"`
}

type botSrcChanBucket struct {
	BotGuid  uint64             `json:"botGuid"`
	Calls    uint64             `json:"calls"`
	Channels []botChannelBucket `json:"channels"`
}

type botSourceChannelBreakdownResult struct {
	Window      string             `json:"window"`
	Since       time.Time          `json:"since"`
	ScannedRows uint64             `json:"scannedRows"`
	Bots        []botSrcChanBucket `json:"bots"`
}

// collectBotSourceChannelBreakdown pulls one window of audit rows in a single
// query, then folds them into (bot, channel) buckets server-side. Split out so
// tests can drive it directly with sqlmock.
func collectBotSourceChannelBreakdown(ctx context.Context, db *sql.DB, d time.Duration, botGuid string, topN int, errorsOnly bool) (*botSourceChannelBreakdownResult, error) {
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
	args = append(args, wowBotSrcChanBreakdownRowCap)
	q := "SELECT bot_guid, source_channel, latency_ms, error FROM `mod_ollama_chat_gateway_audit` WHERE " +
		strings.Join(conds, " AND ") + " ORDER BY bot_guid LIMIT ?"

	rows, err := conn.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("query: %w", err)
	}
	defer rows.Close()

	// Two-level fold: bot -> channel -> {calls, errors, sumLatency}.
	type chanAgg struct {
		calls      uint64
		errors     uint64
		sumLatency uint64
	}
	byBot := make(map[uint64]map[string]*chanAgg)
	totalByBot := make(map[uint64]uint64)
	var scanned uint64
	for rows.Next() {
		var guid, ms uint64
		var channel string
		var errFlag uint8
		if err := rows.Scan(&guid, &channel, &ms, &errFlag); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		scanned++
		m, ok := byBot[guid]
		if !ok {
			m = make(map[string]*chanAgg)
			byBot[guid] = m
		}
		c, ok := m[channel]
		if !ok {
			c = &chanAgg{}
			m[channel] = c
		}
		c.calls++
		c.sumLatency += ms
		if errFlag != 0 {
			c.errors++
		}
		totalByBot[guid]++
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iter: %w", err)
	}

	buckets := make([]botSrcChanBucket, 0, len(byBot))
	for guid, chans := range byBot {
		total := totalByBot[guid]
		chanList := make([]botChannelBucket, 0, len(chans))
		for ch, agg := range chans {
			b := botChannelBucket{
				Channel: ch,
				Calls:   agg.calls,
				Errors:  agg.errors,
			}
			if agg.calls > 0 {
				b.AvgLatencyMs = agg.sumLatency / agg.calls
			}
			// Round to 1 decimal — matches the AdminAuditBucket convention so
			// operators don't see two different precisions across tools.
			b.Pct = float64(int(float64(agg.calls)/float64(total)*1000+0.5)) / 10
			chanList = append(chanList, b)
		}
		sort.Slice(chanList, func(i, j int) bool {
			if chanList[i].Calls != chanList[j].Calls {
				return chanList[i].Calls > chanList[j].Calls
			}
			return chanList[i].Channel < chanList[j].Channel
		})
		buckets = append(buckets, botSrcChanBucket{
			BotGuid:  guid,
			Calls:    total,
			Channels: chanList,
		})
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
	return &botSourceChannelBreakdownResult{
		Window:      d.String(),
		Since:       since,
		ScannedRows: scanned,
		Bots:        buckets,
	}, nil
}
