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
	wowGuildBankSummaryTimeout    = 10 * time.Second
	wowGuildBankSummaryDefaultTop = 50
	wowGuildBankSummaryMaxTop     = 500
	// wowGuildBankTabSep is the GROUP_CONCAT separator for tab names. Tab labels are
	// player-chosen varchar(16) and can contain commas/spaces, so a printable delimiter
	// would be ambiguous; the ASCII unit-separator (0x1f) never appears in a tab name, so
	// splitting on it round-trips exactly. Kept in sync with the SQL `SEPARATOR 0x1f`.
	wowGuildBankTabSep = "\x1f"
)

// wowGuildBankSummarySortColumns is the sortBy allowlist. The VALUES are interpolated
// straight into the ORDER BY (a column list can't be a bind parameter), so this map is
// the injection boundary — only these fixed clauses ever reach the query, never operator
// input. Every clause ends with a guildid tiebreaker so paging is deterministic.
var wowGuildBankSummarySortColumns = map[string]string{
	"bankMoney":  "g.BankMoney DESC, g.guildid ASC",
	"tabs":       "tabCount DESC, g.BankMoney DESC, g.guildid ASC",
	"name":       "g.name ASC, g.guildid ASC",
	"guildId":    "g.guildid ASC",
	"createDate": "g.createdate ASC, g.guildid ASC",
}

// guildBankSummaryRow is one guild's economic census line. bankMoneyCopper is the raw
// guild.BankMoney (copper); bankMoney is the same value rendered "Xg Ys Zc". tabCount is
// the number of purchased guild-bank tabs (guild_bank_tab rows); tabNames are their
// labels in TabId order (omitted when the guild has bought no tabs). createDate is the
// guild's founding unix time.
type guildBankSummaryRow struct {
	GuildID         int64    `json:"guildId"`
	Name            string   `json:"name"`
	LeaderGuid      int64    `json:"leaderGuid"`
	BankMoneyCopper int64    `json:"bankMoneyCopper"`
	BankMoney       string   `json:"bankMoney"`
	TabCount        int      `json:"tabCount"`
	TabNames        []string `json:"tabNames,omitempty"`
	CreateDate      int64    `json:"createDate"`
	CreateDateISO   string   `json:"createDateISO,omitempty"`
}

// RegisterWowGuildBankSummaryTool registers `wow_guild_bank_summary` — a read-only,
// realm-wide per-guild bank census over the ops_ro pool. It is the ECONOMIC companion to
// wow_guild_roster (which drills a single guild's membership): this collapses "which
// guilds are hoarding gold / have the most bank tabs" into one round trip, ranked richest
// first, folding guild.BankMoney together with a guild_bank_tab rollup.
func RegisterWowGuildBankSummaryTool(reg *Registry, deps DBDeps) {
	reg.Register(Tool{
		Name: "wow_guild_bank_summary",
		Description: "Realm-wide per-guild bank census over acore_characters — one row per guild joining `guild` " +
			"(BankMoney, the guild's banked gold in copper) with a `guild_bank_tab` rollup on the ops_ro pool, the " +
			"economic companion to wow_guild_roster's membership drilldown. Ranked richest-first by default so an " +
			"operator sees which guilds are hoarding gold / have the most bank tabs in one round trip. Each guild " +
			"carries guildId, name, leaderGuid, bankMoneyCopper (raw copper) + bankMoney (formatted gold/silver/copper), " +
			"tabCount (purchased guild-bank tabs) + tabNames (tab labels in TabId order, omitted when none bought), and " +
			"createDate + createDateISO. Headline totals are honest and realm-wide (pre-limit): totalGuilds, " +
			"guildsWithBank (BankMoney > 0), totalBankMoneyCopper + totalBankMoney, totalTabs, and matchedGuilds (guilds " +
			"passing minBankGold, pre-limit) plus truncated. Args: topN (default 50, max 500), sortBy (one of bankMoney " +
			"[default, richest first], tabs, name, guildId, createDate — validated against an allowlist; an unknown " +
			"value is rejected), minBankGold (only guilds whose bank holds at least this many gold, default 0 = all " +
			"guilds; negative clamps to 0). Read-only, 10s timeout.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{
			"topN":{"type":"integer","description":"Max guilds returned (default 50, max 500)"},
			"sortBy":{"type":"string","enum":["bankMoney","tabs","name","guildId","createDate"],"description":"Sort key (default bankMoney = richest first)"},
			"minBankGold":{"type":"integer","description":"Only guilds whose bank holds at least this many gold (default 0 = all guilds); negative clamps to 0"}
		}}`),
		Annotations: AnnRead(),
		Handler: func(ctx context.Context, raw json.RawMessage, _ string) any {
			var a struct {
				TopN        int    `json:"topN"`
				SortBy      string `json:"sortBy"`
				MinBankGold int64  `json:"minBankGold"`
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
				topN = wowGuildBankSummaryDefaultTop
			}
			if topN > wowGuildBankSummaryMaxTop {
				topN = wowGuildBankSummaryMaxTop
			}
			// sortBy is an enum, not a numeric knob: default when empty, but reject an
			// unknown non-empty value as a typo (silently defaulting would hide it) — and
			// only allowlisted values ever reach the interpolated ORDER BY.
			sortBy := a.SortBy
			if sortBy == "" {
				sortBy = "bankMoney"
			}
			orderBy, ok := wowGuildBankSummarySortColumns[sortBy]
			if !ok {
				return map[string]any{"error": fmt.Sprintf("invalid sortBy %q (valid: bankMoney, tabs, name, guildId, createDate)", a.SortBy)}
			}
			// minBankGold is a numeric threshold (like topN): a negative value is clamped
			// to 0 (all guilds) rather than rejected.
			minBankGold := a.MinBankGold
			if minBankGold < 0 {
				minBankGold = 0
			}
			c, cancel := context.WithTimeout(ctx, wowGuildBankSummaryTimeout)
			defer cancel()
			out, err := collectWowGuildBankSummary(c, deps.QueryDB, time.Now(), topN, sortBy, orderBy, minBankGold)
			if err != nil {
				return map[string]any{"error": err.Error()}
			}
			return out
		},
	})
}

// collectWowGuildBankSummary runs the honest realm-wide aggregates, the optional
// matched-count, and the per-guild list over one per-call connection (USE acore_characters
// once — the wow_guild_roster pattern). Split out so tests drive it with sqlmock and a
// fixed `now` (createDateISO is now-independent, but the asOf stamp is). orderBy is a
// pre-resolved allowlist clause; sortBy is echoed back for the caller.
func collectWowGuildBankSummary(ctx context.Context, db *sql.DB, now time.Time, topN int, sortBy, orderBy string, minBankGold int64) (map[string]any, error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire conn: %w", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "USE `acore_characters`"); err != nil {
		return nil, fmt.Errorf("USE acore_characters: %w", err)
	}

	// minBankGold is in gold; the column is copper. copperPerGold is the shared AH-tool
	// constant (10000). minBankGold=0 -> 0 copper -> `BankMoney >= 0` matches every guild
	// (BankMoney is unsigned), so the list keeps one code path whether or not filtered.
	minCopper := minBankGold * copperPerGold

	// ---- honest realm-wide totals (unfiltered, pre-limit) ----
	var totalGuilds, guildsWithBank, totalBankMoneyCopper int64
	if err := conn.QueryRowContext(ctx,
		"SELECT COUNT(*), COALESCE(SUM(BankMoney > 0), 0), COALESCE(SUM(BankMoney), 0) FROM `guild`").
		Scan(&totalGuilds, &guildsWithBank, &totalBankMoneyCopper); err != nil {
		return nil, fmt.Errorf("guild-bank aggregate: %w", err)
	}

	// totalTabs = every guild_bank_tab row realm-wide (one row per purchased tab).
	var totalTabs int64
	if err := conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM `guild_bank_tab`").Scan(&totalTabs); err != nil {
		return nil, fmt.Errorf("guild-bank tab count: %w", err)
	}

	// matchedGuilds = guilds passing minBankGold, pre-limit — the honest denominator for
	// truncated. With no filter it equals totalGuilds, so skip the extra round trip.
	matchedGuilds := totalGuilds
	if minBankGold > 0 {
		if err := conn.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM `guild` WHERE BankMoney >= ?", minCopper).Scan(&matchedGuilds); err != nil {
			return nil, fmt.Errorf("guild-bank matched-count: %w", err)
		}
	}

	// ---- per-guild list ----
	// LEFT JOIN guild_bank_tab so a bank-less guild still lists (tabCount 0, tabNames NULL).
	// COUNT(gbt.TabId) is the authoritative tab count; GROUP_CONCAT(...SEPARATOR 0x1f)
	// carries the labels. ORDER BY is a validated allowlist clause (never operator input).
	rows, err := conn.QueryContext(ctx,
		"SELECT g.guildid, g.name, g.leaderguid, g.BankMoney, g.createdate, "+
			"COUNT(gbt.TabId), GROUP_CONCAT(gbt.TabName ORDER BY gbt.TabId SEPARATOR 0x1f) "+
			"FROM `guild` g "+
			"LEFT JOIN `guild_bank_tab` gbt ON gbt.guildid = g.guildid "+
			"WHERE g.BankMoney >= ? "+
			"GROUP BY g.guildid, g.name, g.leaderguid, g.BankMoney, g.createdate "+
			"ORDER BY "+orderBy+" LIMIT ?", minCopper, topN)
	if err != nil {
		return nil, fmt.Errorf("guild-bank query: %w", err)
	}
	defer rows.Close()

	guilds := make([]*guildBankSummaryRow, 0, topN)
	for rows.Next() {
		var (
			r        guildBankSummaryRow
			tabNames sql.NullString
		)
		if err := rows.Scan(&r.GuildID, &r.Name, &r.LeaderGuid, &r.BankMoneyCopper, &r.CreateDate,
			&r.TabCount, &tabNames); err != nil {
			return nil, fmt.Errorf("guild-bank scan: %w", err)
		}
		r.BankMoney = formatMoney(r.BankMoneyCopper)
		if r.CreateDate > 0 {
			r.CreateDateISO = formatUnixISO(r.CreateDate)
		}
		// GROUP_CONCAT over an all-NULL group (no tabs) is NULL -> nil slice (omitted).
		// A valid string (>=1 tab) splits back to exactly tabCount labels.
		if tabNames.Valid {
			r.TabNames = strings.Split(tabNames.String, wowGuildBankTabSep)
		}
		guilds = append(guilds, &r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("guild-bank iter: %w", err)
	}

	return map[string]any{
		"asOf":                 now.UTC().Format(time.RFC3339),
		"totalGuilds":          totalGuilds,
		"guildsWithBank":       guildsWithBank,
		"totalBankMoneyCopper": totalBankMoneyCopper,
		"totalBankMoney":       formatMoney(totalBankMoneyCopper),
		"totalTabs":            totalTabs,
		"matchedGuilds":        matchedGuilds,
		"minBankGold":          minBankGold,
		"sortBy":               sortBy,
		"topN":                 topN,
		"returned":             len(guilds),
		"truncated":            matchedGuilds > int64(len(guilds)),
		"guilds":               guilds,
	}, nil
}
