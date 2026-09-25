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
	wowBotsFleetStatusTimeout       = 15 * time.Second
	wowBotsFleetStatusDefaultPrefix = "RNDBOT"
	wowBotsFleetStatusDefaultWindow = "24h"
	wowBotsFleetStatusMaxWindow     = 30 * 24 * time.Hour
	wowBotsFleetStatusDefaultTopN   = 10
	wowBotsFleetStatusMaxTopN       = 50
	wowBotsFleetStatusMaxRoster     = 20000 // hard ceiling on rows scanned for the roster
)

// RegisterWowBotsFleetStatusTool registers `wow_bots_fleet_status` — a composite
// snapshot of the mod-playerbots fleet. wow_online_players (PR #125) covers
// ALL online characters; this scopes to bot-owned characters and pre-aggregates
// the class / level / zone distributions plus the audit-derived top-errored
// list. The composite replaces the operator pattern of `db_query` for bot
// roster + per-bot `wow_player_lookup` + `ops_audit_summary` and folding the
// rows in memory.
//
// Bots are identified by account-name prefix (default `RNDBOT`, matching
// mod-playerbots' default `playerbots.RandomBotAccountPrefix`). The ops_ro
// pool does NOT have SELECT on `acore_playerbots`, so we cannot use the
// canonical `playerbots_account_type` table — the username prefix is the
// reliable fallback (and is what operators recognize in the auth DB anyway).
//
// Use cases:
//   - "fleet snapshot before deploy" (count online vs offline, class spread)
//   - "which bots keep failing the LLM calls?" (topErrored)
//   - "where is the fleet right now?" (topZones for online subset)
func RegisterWowBotsFleetStatusTool(reg *Registry, deps DBDeps) {
	reg.Register(Tool{
		Name: "wow_bots_fleet_status",
		Description: "Composite mod-playerbots fleet snapshot. Returns {accountPrefix, totalBots, " +
			"onlineBots, offlineBots, classDistribution, levelDistribution, topZones, topErrored:" +
			"{window, since, bots:[{botGuid, name, totalCalls, errorCount}]}}. Bots are identified " +
			"by the owning-account username prefix (default \"RNDBOT\", per mod-playerbots' " +
			"playerbots.RandomBotAccountPrefix). Replaces the operator pattern of \"db_query bot " +
			"roster\" + per-bot wow_player_lookup + ops_audit_summary aggregation. Use before a " +
			"worldserver restart (online count), for class/level spread audits, or to find " +
			"misbehaving bots (topErrored). Read-only over the ops_ro pool; pulls roster from " +
			"acore_characters.characters JOIN acore_auth.account and audit deltas from " +
			"acore_characters.mod_ollama_chat_gateway_audit. Optional: accountPrefix (default " +
			"\"RNDBOT\"), errorWindow (default \"24h\", max 720h), topN (default 10, max 50). " +
			"15 s timeout, roster scan capped at 20000 rows.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{
			"accountPrefix":{"type":"string","description":"Username prefix that identifies bot accounts (default \"RNDBOT\"). Bare prefix only — '%' is appended internally."},
			"errorWindow":{"type":"string","description":"Lookback window for top-errored bots (e.g. \"1h\", \"24h\", \"7d\"; default 24h, max 720h)"},
			"topN":{"type":"integer","description":"Max top-errored rows (default 10, max 50)"}
		}}`),
		Annotations: AnnRead(),
		Handler: func(ctx context.Context, raw json.RawMessage, _ string) any {
			var a struct {
				AccountPrefix string `json:"accountPrefix"`
				ErrorWindow   string `json:"errorWindow"`
				TopN          int    `json:"topN"`
			}
			if err := json.Unmarshal(raw, &a); err != nil {
				return map[string]any{"error": "decode args: " + err.Error()}
			}
			if deps.QueryDB == nil {
				return map[string]any{"error": "ops_ro pool not configured (DB_DSN empty?)"}
			}
			prefix := strings.TrimSpace(a.AccountPrefix)
			if prefix == "" {
				prefix = wowBotsFleetStatusDefaultPrefix
			}
			if strings.ContainsAny(prefix, "%_") {
				return map[string]any{"error": "accountPrefix must not contain LIKE wildcards ('%' or '_')"}
			}
			window := a.ErrorWindow
			if window == "" {
				window = wowBotsFleetStatusDefaultWindow
			}
			d, err := time.ParseDuration(window)
			if err != nil || d <= 0 || d > wowBotsFleetStatusMaxWindow {
				return map[string]any{"error": "errorWindow must be a positive duration up to 720h"}
			}
			topN := a.TopN
			if topN <= 0 {
				topN = wowBotsFleetStatusDefaultTopN
			}
			if topN > wowBotsFleetStatusMaxTopN {
				topN = wowBotsFleetStatusMaxTopN
			}
			c, cancel := context.WithTimeout(ctx, wowBotsFleetStatusTimeout)
			defer cancel()
			out, err := collectBotsFleetStatus(c, deps.QueryDB, prefix, d, window, topN)
			if err != nil {
				return map[string]any{"error": err.Error()}
			}
			return out
		},
	})
}

// botClassBucket / botLevelBucket / botZoneBucket / botErroredRow shape the
// public JSON. Kept as structs (not maps) so tests can assert typed fields
// without the int64-vs-float64 JSON-decode dance.
type botClassBucket struct {
	Class     int    `json:"class"`
	ClassName string `json:"className"`
	Count     int    `json:"count"`
}

type botLevelBucket struct {
	Bucket string `json:"bucket"` // "1-9", "10-19", ..., "70-79", "80"
	Count  int    `json:"count"`
}

type botZoneBucket struct {
	ZoneID int `json:"zoneId"`
	Count  int `json:"count"`
}

type botErroredRow struct {
	BotGuid    int64  `json:"botGuid"`
	Name       string `json:"name"`
	TotalCalls int    `json:"totalCalls"`
	ErrorCount int    `json:"errorCount"`
}

// collectBotsFleetStatus runs the two-stage composite. Split out so tests can
// drive it with sqlmock without going through the registry handler.
func collectBotsFleetStatus(ctx context.Context, db *sql.DB, prefix string, window time.Duration, windowLabel string, topN int) (map[string]any, error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire conn: %w", err)
	}
	defer conn.Close()

	classCounts, levelCounts, onlineZoneCounts, total, online, err := scanBotRoster(ctx, conn, prefix)
	if err != nil {
		return nil, err
	}

	since := time.Now().UTC().Add(-window)
	topErrored, err := scanTopErroredBots(ctx, conn, since, topN)
	if err != nil {
		return nil, err
	}
	if err := annotateErroredBotNames(ctx, conn, topErrored); err != nil {
		return nil, err
	}

	return map[string]any{
		"accountPrefix":     prefix,
		"totalBots":         total,
		"onlineBots":        online,
		"offlineBots":       total - online,
		"classDistribution": formatClassDistribution(classCounts),
		"levelDistribution": formatLevelDistribution(levelCounts),
		"topZones":          formatZoneDistribution(onlineZoneCounts),
		"topErrored": map[string]any{
			"window": windowLabel,
			"since":  since.Format(time.RFC3339),
			"bots":   topErrored,
		},
	}, nil
}

// scanBotRoster walks acore_characters.characters JOIN acore_auth.account in a
// single query, accumulating per-class / per-level-bucket / per-online-zone
// counts as we go. The accumulator approach keeps memory O(buckets) regardless
// of fleet size — a 20k-bot fleet only allocates ~50 ints, not 20k row structs.
// Hard cap on the roster scan protects against a pathological prefix matching
// every character.
func scanBotRoster(ctx context.Context, conn *sql.Conn, prefix string) (
	classCounts map[int]int,
	levelCounts map[int]int,
	onlineZoneCounts map[int]int,
	total int,
	online int,
	err error,
) {
	classCounts = make(map[int]int, 11)
	levelCounts = make(map[int]int, 9)
	onlineZoneCounts = make(map[int]int, 16)

	q := "SELECT c.class, c.level, c.online, c.zone " +
		"FROM `acore_characters`.`characters` c " +
		"INNER JOIN `acore_auth`.`account` a ON c.account = a.id " +
		"WHERE a.username LIKE ? LIMIT ?"
	rows, err := conn.QueryContext(ctx, q, prefix+"%", wowBotsFleetStatusMaxRoster)
	if err != nil {
		return nil, nil, nil, 0, 0, fmt.Errorf("roster query: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var class, level, isOnline, zone int
		if err := rows.Scan(&class, &level, &isOnline, &zone); err != nil {
			return nil, nil, nil, 0, 0, fmt.Errorf("roster scan: %w", err)
		}
		total++
		classCounts[class]++
		levelCounts[bucketForLevel(level)]++
		if isOnline != 0 {
			online++
			onlineZoneCounts[zone]++
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, nil, 0, 0, fmt.Errorf("roster iter: %w", err)
	}
	return classCounts, levelCounts, onlineZoneCounts, total, online, nil
}

// scanTopErroredBots pulls the top-N most-errored bot_guids from the gateway
// audit table inside the window. Filter on errors > 0 keeps the result tight
// (a quiet fleet returns []), and the (errors DESC, total DESC) ordering
// surfaces the worst offenders first.
func scanTopErroredBots(ctx context.Context, conn *sql.Conn, since time.Time, topN int) ([]*botErroredRow, error) {
	q := "SELECT bot_guid, COUNT(*), SUM(error) " +
		"FROM `acore_characters`.`mod_ollama_chat_gateway_audit` " +
		"WHERE ts >= ? GROUP BY bot_guid HAVING SUM(error) > 0 " +
		"ORDER BY SUM(error) DESC, COUNT(*) DESC LIMIT ?"
	rows, err := conn.QueryContext(ctx, q, since, topN)
	if err != nil {
		return nil, fmt.Errorf("audit query: %w", err)
	}
	defer rows.Close()
	var out []*botErroredRow
	for rows.Next() {
		b := &botErroredRow{}
		if err := rows.Scan(&b.BotGuid, &b.TotalCalls, &b.ErrorCount); err != nil {
			return nil, fmt.Errorf("audit scan: %w", err)
		}
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("audit iter: %w", err)
	}
	return out, nil
}

// annotateErroredBotNames batch-fetches character names for the top-errored
// guids using a single IN(...) query. Bots whose character row was deleted
// (rare but possible if the GUID was recycled / wiped) keep an empty name —
// same friendly-degradation choice the online-players composite makes.
func annotateErroredBotNames(ctx context.Context, conn *sql.Conn, bots []*botErroredRow) error {
	if len(bots) == 0 {
		return nil
	}
	var b strings.Builder
	b.Grow(80 + len(bots)*2)
	b.WriteString("SELECT guid, name FROM `acore_characters`.`characters` WHERE guid IN (")
	args := make([]any, 0, len(bots))
	for i, bot := range bots {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteByte('?')
		args = append(args, bot.BotGuid)
	}
	b.WriteByte(')')
	rows, err := conn.QueryContext(ctx, b.String(), args...)
	if err != nil {
		return fmt.Errorf("name query: %w", err)
	}
	defer rows.Close()
	names := make(map[int64]string, len(bots))
	for rows.Next() {
		var guid int64
		var name string
		if err := rows.Scan(&guid, &name); err != nil {
			return fmt.Errorf("name scan: %w", err)
		}
		names[guid] = name
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("name iter: %w", err)
	}
	for _, bot := range bots {
		bot.Name = names[bot.BotGuid]
	}
	return nil
}

// bucketForLevel maps a character level to a 10-bucket index. Levels 1-9 →
// "1-9", 10-19 → "10-19", ..., 70-79 → "70-79", 80 → "80" (level cap gets
// its own bucket so a fleet of mostly 80s doesn't get lumped into "70-79").
func bucketForLevel(level int) int {
	if level <= 0 {
		return 0
	}
	if level >= 80 {
		return 8
	}
	return level / 10
}

var levelBucketLabels = [9]string{
	"1-9", "10-19", "20-29", "30-39", "40-49", "50-59", "60-69", "70-79", "80",
}

// formatClassDistribution sorts by descending count (ties: ascending classId
// for a stable order across runs). Tests rely on the determinism.
func formatClassDistribution(counts map[int]int) []botClassBucket {
	out := make([]botClassBucket, 0, len(counts))
	for class, count := range counts {
		out = append(out, botClassBucket{Class: class, ClassName: wowClassName(class), Count: count})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Class < out[j].Class
	})
	return out
}

// formatLevelDistribution returns buckets in increasing level order. Empty
// buckets are omitted to keep the payload compact for a low-level-only fleet.
func formatLevelDistribution(counts map[int]int) []botLevelBucket {
	out := make([]botLevelBucket, 0, len(counts))
	for i := 0; i < len(levelBucketLabels); i++ {
		if c := counts[i]; c > 0 {
			out = append(out, botLevelBucket{Bucket: levelBucketLabels[i], Count: c})
		}
	}
	return out
}

// formatZoneDistribution caps the response to the top 10 zones (descending
// count, ties by ascending zoneId). A worldwide-scattered fleet otherwise
// produces a long zone list that adds noise to the snapshot.
func formatZoneDistribution(counts map[int]int) []botZoneBucket {
	out := make([]botZoneBucket, 0, len(counts))
	for zone, count := range counts {
		out = append(out, botZoneBucket{ZoneID: zone, Count: count})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].ZoneID < out[j].ZoneID
	})
	if len(out) > 10 {
		out = out[:10]
	}
	return out
}
