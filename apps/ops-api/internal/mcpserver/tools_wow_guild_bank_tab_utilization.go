package mcpserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

const (
	wowGuildBankTabUtilTimeout    = 10 * time.Second
	wowGuildBankTabUtilDefaultTop = 50
	wowGuildBankTabUtilMaxTop     = 500
	// guildBankMaxSlots is the fixed slot capacity of a single guild-bank tab on
	// WotLK 3.3.5a — GUILD_BANK_MAX_SLOTS (enum GuildMisc, Guild.h:43). Every purchased
	// tab holds exactly this many item slots, so fillPercent = filled / guildBankMaxSlots.
	// A constant capacity is why the fillPercent and filled sorts order identically.
	guildBankMaxSlots = 98
)

// wowGuildBankTabUtilSortColumns is the sortBy allowlist. The VALUES are interpolated
// straight into the ORDER BY (a column list can't be a bind parameter), so this map is
// the injection boundary — only these fixed clauses ever reach the query, never operator
// input. `filled` is the SELECT alias (COUNT(gbi.SlotId)); every clause ends with a
// (guildid, TabId) tiebreaker so paging is deterministic. fillPercent and filled resolve
// to the SAME clause on purpose: capacity is a uniform 98, so ordering by percent is
// ordering by raw count — both keys are offered for caller intent clarity.
var wowGuildBankTabUtilSortColumns = map[string]string{
	"fillPercent": "filled DESC, gbt.guildid ASC, gbt.TabId ASC",
	"filled":      "filled DESC, gbt.guildid ASC, gbt.TabId ASC",
	"guild":       "g.name ASC, gbt.guildid ASC, gbt.TabId ASC",
}

// guildBankTabUtilRow is one guild-bank tab's occupancy line. filled is the count of
// occupied slots (guild_bank_item rows for this (guildid,TabId)); capacity is always
// guildBankMaxSlots (98); freeSlots is capacity-filled; fillPercent is filled/98 as a
// percentage rounded to one decimal. empty flags a purchased-but-unused tab (filled 0);
// full flags a maxed tab (filled 98). guildName is omitted only for an orphan tab whose
// guild row is missing (a LEFT JOIN NULL — an integrity anomaly, not the normal case).
type guildBankTabUtilRow struct {
	GuildID     int64   `json:"guildId"`
	GuildName   string  `json:"guildName,omitempty"`
	TabID       int     `json:"tabId"`
	TabName     string  `json:"tabName,omitempty"`
	Filled      int     `json:"filled"`
	Capacity    int     `json:"capacity"`
	FreeSlots   int     `json:"freeSlots"`
	FillPercent float64 `json:"fillPercent"`
	Empty       bool    `json:"empty"`
	Full        bool    `json:"full"`
}

// RegisterWowGuildBankTabUtilizationTool registers `wow_guild_bank_tab_utilization` — a
// read-only, realm-wide per-guild-per-tab slot-occupancy census over the ops_ro pool. It
// is the CAPACITY companion to wow_guild_bank_summary (which counts a guild's money + how
// many tabs it owns): this measures how FULL each purchased tab is, collapsing "which
// guild-bank tabs are near-full / sitting empty" into one round trip.
func RegisterWowGuildBankTabUtilizationTool(reg *Registry, deps DBDeps) {
	reg.Register(Tool{
		Name: "wow_guild_bank_tab_utilization",
		Description: "Per-guild-per-tab guild-bank slot-occupancy census over acore_characters on the ops_ro pool — one " +
			"row per purchased guild-bank tab joining `guild_bank_tab` (the tab) with a `guild_bank_item` filled-slot " +
			"COUNT, the CAPACITY companion to wow_guild_bank_summary's money+tab census. Every tab holds a fixed 98 slots " +
			"(GUILD_BANK_MAX_SLOTS), so an operator sees which tabs are near-full (nudge a cleanout) or purchased-but-empty " +
			"(wasted gold) in one round trip. Each tab carries guildId, guildName, tabId, tabName, filled (occupied slots), " +
			"capacity (always 98), freeSlots, fillPercent (filled/98, one decimal), empty (filled 0) and full (filled 98). " +
			"Headline totals are honest and realm-wide (pre-limit): totalTabs, guildsWithTabs, totalFilledSlots, " +
			"totalCapacity, totalFreeSlots, overallFillPercent, emptyTabs, fullTabs, and matchedTabs (tabs passing the " +
			"filters, pre-limit) plus truncated. Args: topN (default 50, max 500), sortBy (one of fillPercent [default, " +
			"fullest first], filled, guild — validated against an allowlist; an unknown value is rejected; fillPercent and " +
			"filled order identically because capacity is uniform), guildId (drill down to one guild; 0 = all guilds, " +
			"negative clamps to 0), minFillPercent (only tabs at least this percent full, 0-100; default 0 = all tabs, " +
			"negative clamps to 0, above 100 clamps to 100). Read-only, 10s timeout.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{
			"topN":{"type":"integer","description":"Max tabs returned (default 50, max 500)"},
			"sortBy":{"type":"string","enum":["fillPercent","filled","guild"],"description":"Sort key (default fillPercent = fullest first)"},
			"guildId":{"type":"integer","description":"Drill down to a single guild (0 = all guilds; negative clamps to 0)"},
			"minFillPercent":{"type":"integer","description":"Only tabs at least this percent full (0-100; default 0 = all tabs; negative clamps to 0, above 100 clamps to 100)"}
		}}`),
		Annotations: AnnRead(),
		Handler: func(ctx context.Context, raw json.RawMessage, _ string) any {
			var a struct {
				TopN           int    `json:"topN"`
				SortBy         string `json:"sortBy"`
				GuildID        int64  `json:"guildId"`
				MinFillPercent int    `json:"minFillPercent"`
			}
			if len(raw) > 0 && string(raw) != "null" {
				if err := json.Unmarshal(raw, &a); err != nil {
					return map[string]any{"error": "decode args: " + err.Error()}
				}
			}
			if deps.QueryDB == nil {
				return map[string]any{"error": "ops_ro pool not configured (DB_DSN empty?)"}
			}
			topN := a.TopN
			if topN <= 0 {
				topN = wowGuildBankTabUtilDefaultTop
			}
			if topN > wowGuildBankTabUtilMaxTop {
				topN = wowGuildBankTabUtilMaxTop
			}
			// sortBy is an enum, not a numeric knob: default when empty, but reject an
			// unknown non-empty value as a typo (silently defaulting would hide it) — and
			// only allowlisted values ever reach the interpolated ORDER BY.
			sortBy := a.SortBy
			if sortBy == "" {
				sortBy = "fillPercent"
			}
			orderBy, ok := wowGuildBankTabUtilSortColumns[sortBy]
			if !ok {
				return map[string]any{"error": fmt.Sprintf("invalid sortBy %q (valid: fillPercent, filled, guild)", a.SortBy)}
			}
			// guildId is an equality drilldown, not a range: a negative id is nonsense, so
			// clamp it to 0 (= all guilds). guildid is never 0 in acore_characters (the
			// AUTO_INCREMENT starts at 1), so 0 is a safe "no filter" sentinel.
			guildID := a.GuildID
			if guildID < 0 {
				guildID = 0
			}
			// minFillPercent is a percentage floor: clamp to [0,100] (a floor above full
			// matches nothing meaningful; a negative floor means "all").
			minFillPercent := a.MinFillPercent
			if minFillPercent < 0 {
				minFillPercent = 0
			}
			if minFillPercent > 100 {
				minFillPercent = 100
			}
			c, cancel := context.WithTimeout(ctx, wowGuildBankTabUtilTimeout)
			defer cancel()
			out, err := collectWowGuildBankTabUtilization(c, deps.QueryDB, time.Now(), topN, sortBy, orderBy, guildID, minFillPercent)
			if err != nil {
				return map[string]any{"error": err.Error()}
			}
			return out
		},
	})
}

// collectWowGuildBankTabUtilization runs the honest realm-wide occupancy aggregate, the
// optional matched-count, and the per-tab list over one per-call connection (USE
// acore_characters once — the wow_guild_bank_summary pattern). Split out so tests drive it
// with sqlmock and a fixed `now`. orderBy is a pre-resolved allowlist clause; sortBy is
// echoed back for the caller. minFillPercent is the caller-facing percent; it is converted
// to an integer slot floor (minFilled) so the HAVING stays integer-only (no SQL float).
func collectWowGuildBankTabUtilization(ctx context.Context, db *sql.DB, now time.Time, topN int, sortBy, orderBy string, guildID int64, minFillPercent int) (map[string]any, error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire conn: %w", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "USE `acore_characters`"); err != nil {
		return nil, fmt.Errorf("USE acore_characters: %w", err)
	}

	// minFilled is the integer slot floor for minFillPercent: ceil(minFillPercent% of 98)
	// so "fillPercent >= minFillPercent" becomes a clean integer HAVING with no SQL float.
	// 0% -> 0 (HAVING >= 0 matches every tab, including empty), 100% -> 98.
	minFilled := 0
	if minFillPercent > 0 {
		minFilled = (minFillPercent*guildBankMaxSlots + 99) / 100 // ceil division
	}

	// ---- honest realm-wide occupancy totals (unfiltered, pre-limit) ----
	// Aggregate over a per-tab derived table: one sub-row per purchased tab carrying its
	// filled-slot count (LEFT JOIN so an empty tab still contributes filled 0). The
	// `filled >= 98` literal is guildBankMaxSlots (Guild.h:43).
	var totalTabs, totalFilled, emptyTabs, fullTabs, guildsWithTabs int64
	if err := conn.QueryRowContext(ctx,
		"SELECT COUNT(*), COALESCE(SUM(filled), 0), COALESCE(SUM(filled = 0), 0), "+
			"COALESCE(SUM(filled >= 98), 0), COUNT(DISTINCT guildid) FROM ("+
			"SELECT gbt.guildid AS guildid, COUNT(gbi.SlotId) AS filled "+
			"FROM `guild_bank_tab` gbt "+
			"LEFT JOIN `guild_bank_item` gbi ON gbi.guildid = gbt.guildid AND gbi.TabId = gbt.TabId "+
			"GROUP BY gbt.guildid, gbt.TabId) t").
		Scan(&totalTabs, &totalFilled, &emptyTabs, &fullTabs, &guildsWithTabs); err != nil {
		return nil, fmt.Errorf("guild-bank tab aggregate: %w", err)
	}
	totalCapacity := totalTabs * guildBankMaxSlots

	// matchedTabs = tabs passing the filters, pre-limit — the honest denominator for
	// truncated. With no filter it equals totalTabs, so skip the extra round trip. The
	// `? = 0` guildid sentinel matches every guild when guildID is 0.
	matchedTabs := totalTabs
	filtered := guildID > 0 || minFillPercent > 0
	if filtered {
		if err := conn.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM ("+
				"SELECT gbt.guildid AS guildid, COUNT(gbi.SlotId) AS filled "+
				"FROM `guild_bank_tab` gbt "+
				"LEFT JOIN `guild_bank_item` gbi ON gbi.guildid = gbt.guildid AND gbi.TabId = gbt.TabId "+
				"WHERE (gbt.guildid = ? OR ? = 0) "+
				"GROUP BY gbt.guildid, gbt.TabId "+
				"HAVING COUNT(gbi.SlotId) >= ?) t", guildID, guildID, minFilled).
			Scan(&matchedTabs); err != nil {
			return nil, fmt.Errorf("guild-bank tab matched-count: %w", err)
		}
	}

	// ---- per-tab list ----
	// LEFT JOIN guild for the name (an orphan tab still lists, guildName ""), LEFT JOIN
	// guild_bank_item for the filled-slot COUNT (an empty tab still lists, filled 0). The
	// guildid sentinel + minFilled HAVING mirror the matched-count exactly so returned and
	// matchedTabs are the same population. ORDER BY is a validated allowlist clause.
	rows, err := conn.QueryContext(ctx,
		"SELECT gbt.guildid, g.name, gbt.TabId, gbt.TabName, COUNT(gbi.SlotId) AS filled "+
			"FROM `guild_bank_tab` gbt "+
			"LEFT JOIN `guild` g ON g.guildid = gbt.guildid "+
			"LEFT JOIN `guild_bank_item` gbi ON gbi.guildid = gbt.guildid AND gbi.TabId = gbt.TabId "+
			"WHERE (gbt.guildid = ? OR ? = 0) "+
			"GROUP BY gbt.guildid, g.name, gbt.TabId, gbt.TabName "+
			"HAVING COUNT(gbi.SlotId) >= ? "+
			"ORDER BY "+orderBy+" LIMIT ?", guildID, guildID, minFilled, topN)
	if err != nil {
		return nil, fmt.Errorf("guild-bank tab query: %w", err)
	}
	defer rows.Close()

	tabs := make([]*guildBankTabUtilRow, 0, topN)
	for rows.Next() {
		var (
			r         guildBankTabUtilRow
			guildName sql.NullString
		)
		if err := rows.Scan(&r.GuildID, &guildName, &r.TabID, &r.TabName, &r.Filled); err != nil {
			return nil, fmt.Errorf("guild-bank tab scan: %w", err)
		}
		r.GuildName = guildName.String // "" for an orphan tab (LEFT JOIN NULL)
		r.Capacity = guildBankMaxSlots
		r.FreeSlots = guildBankMaxSlots - r.Filled
		r.FillPercent = pctOf(r.Filled, guildBankMaxSlots)
		r.Empty = r.Filled == 0
		r.Full = r.Filled >= guildBankMaxSlots
		tabs = append(tabs, &r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("guild-bank tab iter: %w", err)
	}

	return map[string]any{
		"asOf":               now.UTC().Format(time.RFC3339),
		"totalTabs":          totalTabs,
		"guildsWithTabs":     guildsWithTabs,
		"totalFilledSlots":   totalFilled,
		"totalCapacity":      totalCapacity,
		"totalFreeSlots":     totalCapacity - totalFilled,
		"overallFillPercent": pctOf(int(totalFilled), int(totalCapacity)),
		"emptyTabs":          emptyTabs,
		"fullTabs":           fullTabs,
		"slotsPerTab":        guildBankMaxSlots,
		"matchedTabs":        matchedTabs,
		"guildId":            guildID,
		"minFillPercent":     minFillPercent,
		"sortBy":             sortBy,
		"topN":               topN,
		"returned":           len(tabs),
		"truncated":          matchedTabs > int64(len(tabs)),
		"tabs":               tabs,
	}, nil
}
