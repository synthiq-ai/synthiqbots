package mcpserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

const (
	wowGuildBankActivityTimeout    = 10 * time.Second
	wowGuildBankActivityDefaultTop = 50
	wowGuildBankActivityMaxTop     = 500
	wowGuildBankActivityHourSecs   = 3600 // sinceHours -> seconds for the TimeStamp window
)

// wowGuildBankActivitySortColumns is the sortBy allowlist. The VALUES are interpolated
// straight into the ORDER BY (a sort clause can't be a bind parameter), so this map is the
// injection boundary — only these fixed clauses ever reach the query, never operator input.
// totalEvents / lastActivity are SELECT-list aliases (declared with AS in the list query) so
// ORDER BY resolves them on real MySQL, not just against sqlmock's string match. Every clause
// ends with a guildid tiebreaker so paging is deterministic.
var wowGuildBankActivitySortColumns = map[string]string{
	"events":  "totalEvents DESC, lastActivity DESC, e.guildid ASC",
	"recency": "lastActivity DESC, totalEvents DESC, e.guildid ASC",
	"guild":   "e.guildid ASC",
}

// guildBankActivityRow is one guild's guild-bank churn line over the queried window.
//
// The load-bearing subtlety is guild_bank_eventlog.ItemOrMoney, which is OVERLOADED: for the
// three MONEY events it is a copper amount, for every ITEM event it is an item_template.entry.
// AzerothCore's Guild::BankEventLogEntry::IsMoneyEvent (Guild.h) classifies ONLY
// GUILD_BANK_LOG_DEPOSIT_MONEY(4) / WITHDRAW_MONEY(5) / REPAIR_MONEY(6) as money events, so
// this tool sums ItemOrMoney EXCLUSIVELY over {4,5,6} and never mixes it with item events —
// item events are reported as COUNTS, not as summed entry ids (which would be meaningless).
//
// moneyInCopper = deposits (4); moneyOutCopper = withdrawals (5) + repairs (6) — repairs pull
// guild-bank gold to pay a member's repair bill, so they leave the bank. repairMoneyCopper is
// broken out (a distinct operational signal: a bank bleeding gold to auto-repair). netMoneyCopper
// = in - out (can be negative → formatMoney renders a leading minus). itemMoves folds
// MOVE_ITEM(3) + MOVE_ITEM2(7) (intra-bank shuffles, neither in nor out).
type guildBankActivityRow struct {
	GuildID           int64  `json:"guildId"`
	Name              string `json:"name"`
	TotalEvents       int64  `json:"totalEvents"`
	DistinctActors    int64  `json:"distinctActors"`
	ItemDeposits      int64  `json:"itemDeposits"`
	ItemWithdrawals   int64  `json:"itemWithdrawals"`
	ItemMoves         int64  `json:"itemMoves"`
	MoneyInCopper     int64  `json:"moneyInCopper"`
	MoneyIn           string `json:"moneyIn"`
	MoneyOutCopper    int64  `json:"moneyOutCopper"`
	MoneyOut          string `json:"moneyOut"`
	RepairMoneyCopper int64  `json:"repairMoneyCopper"`
	RepairMoney       string `json:"repairMoney,omitempty"`
	NetMoneyCopper    int64  `json:"netMoneyCopper"`
	NetMoney          string `json:"netMoney"`
	FirstActivity     int64  `json:"firstActivity"`
	FirstActivityISO  string `json:"firstActivityISO,omitempty"`
	LastActivity      int64  `json:"lastActivity"`
	LastActivityISO   string `json:"lastActivityISO,omitempty"`
	RecencyHuman      string `json:"recencyHuman,omitempty"`
}

// RegisterWowGuildBankActivityTool registers `wow_guild_bank_activity` — a read-only,
// realm-wide per-guild guild-bank ACTIVITY census over the ops_ro pool. It is the audit-trail
// companion to the static guild-bank snapshots (wow_guild_bank_summary's money+tab census):
// this reads guild_bank_eventlog — the bank's deposit/withdraw/move/repair log — and rolls it
// up per guild so an operator sees which banks are actively used vs dormant, how much gold flows
// in vs out, and how many distinct members touch each bank, in one round trip.
//
// guild_bank_eventlog is a BOUNDED ring (AzerothCore keeps ~25 records per tab plus the money
// log), so this is a RECENT-activity window, not full history — the sinceHours arg narrows it
// further. No merged ops-api tool reads guild_bank_eventlog; the bot-facing get_guild_bank_log
// is a single bot's in-process view, not an operator query over acore_characters.
func RegisterWowGuildBankActivityTool(reg *Registry, deps DBDeps) {
	reg.Register(Tool{
		Name: "wow_guild_bank_activity",
		Description: "Realm-wide per-guild guild-bank ACTIVITY census over acore_characters — rolls up " +
			"`guild_bank_eventlog` (the bank's deposit/withdraw/move/repair audit trail: EventType, PlayerGuid, " +
			"ItemOrMoney, TimeStamp) per guild on the ops_ro pool, the activity companion to wow_guild_bank_summary's " +
			"static money+tab snapshot. Answers which guild banks are actively used vs dormant, who's moving gold, and " +
			"the net gold flow. NOTE guild_bank_eventlog is a bounded ring (~25 records per tab + the money log), so this " +
			"is a RECENT-activity window, not full history. ItemOrMoney is OVERLOADED (copper for money events, an item " +
			"entry for item events); money is summed ONLY over deposit/withdraw/repair money events, item events are " +
			"reported as counts. Each guild carries guildId, name, totalEvents, distinctActors, itemDeposits, " +
			"itemWithdrawals, itemMoves, moneyInCopper + moneyIn (deposits, formatted gold/silver/copper), moneyOutCopper " +
			"+ moneyOut (withdrawals + repairs), repairMoneyCopper + repairMoney (repair-bill gold pulled from the bank), " +
			"netMoneyCopper + netMoney (in - out, may be negative), and firstActivity/lastActivity + ISO + recencyHuman. " +
			"Headline totals are honest and realm-wide over the window (pre-limit): totalEvents, distinctGuilds, " +
			"distinctActors, totalMoneyInCopper/totalMoneyIn, totalMoneyOutCopper/totalMoneyOut, netMoneyCopper/netMoney, " +
			"and matchedGuilds (guilds passing the guildId drilldown) plus truncated. Args: topN (default 50, max 500), " +
			"sortBy (one of events [default, busiest first], recency [most recent activity first], guild — validated " +
			"against an allowlist; an unknown value is rejected), guildId (optional positive drilldown to a single " +
			"guild; a non-positive value is rejected), sinceHours (only events in the last N hours; default 0 = the whole " +
			"bounded ring; negative clamps to 0). Read-only, 10s timeout.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{
			"topN":{"type":"integer","description":"Max guilds returned (default 50, max 500)"},
			"sortBy":{"type":"string","enum":["events","recency","guild"],"description":"Sort key (default events = busiest first)"},
			"guildId":{"type":"integer","description":"Optional positive drilldown to a single guild; non-positive is rejected"},
			"sinceHours":{"type":"integer","description":"Only events in the last N hours (default 0 = whole bounded ring); negative clamps to 0"}
		}}`),
		Annotations: AnnRead(),
		Handler: func(ctx context.Context, raw json.RawMessage, _ string) any {
			var a struct {
				TopN       int    `json:"topN"`
				SortBy     string `json:"sortBy"`
				GuildID    int64  `json:"guildId"`
				SinceHours int    `json:"sinceHours"`
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
				topN = wowGuildBankActivityDefaultTop
			}
			if topN > wowGuildBankActivityMaxTop {
				topN = wowGuildBankActivityMaxTop
			}
			// sortBy is an enum, not a numeric knob: default when empty, but reject an unknown
			// non-empty value as a typo (silently defaulting would hide it) — and only
			// allowlisted values ever reach the interpolated ORDER BY.
			sortBy := a.SortBy
			if sortBy == "" {
				sortBy = "events"
			}
			orderBy, ok := wowGuildBankActivitySortColumns[sortBy]
			if !ok {
				return map[string]any{"error": fmt.Sprintf("invalid sortBy %q (valid: events, recency, guild)", a.SortBy)}
			}
			// guildId is an optional drilldown selector, NOT a numeric threshold: absent (0)
			// means all guilds, but a negative/zero-with-intent value is a typo, so a
			// non-positive value that was actually supplied is rejected. json omits the field
			// as 0 when unset, so 0 == "all guilds" and only an explicit negative is bounced.
			if a.GuildID < 0 {
				return map[string]any{"error": "guildId must be a positive integer"}
			}
			// sinceHours is a numeric window (like topN): a negative value is clamped to 0
			// (whole ring) rather than rejected — a negative lookback is nonsensical, not a typo.
			sinceHours := a.SinceHours
			if sinceHours < 0 {
				sinceHours = 0
			}
			c, cancel := context.WithTimeout(ctx, wowGuildBankActivityTimeout)
			defer cancel()
			out, err := collectWowGuildBankActivity(c, deps.QueryDB, time.Now(), topN, sortBy, orderBy, a.GuildID, sinceHours)
			if err != nil {
				return map[string]any{"error": err.Error()}
			}
			return out
		},
	})
}

// collectWowGuildBankActivity runs the honest realm-wide aggregates (window-scoped), the
// optional matched-count (only under a guildId drilldown), and the per-guild list over one
// per-call connection (USE acore_characters once — the wow_guild_bank_summary pattern). Split
// out so tests drive it with sqlmock and a fixed `now` (the sinceHours cutoff and each row's
// recencyHuman are now-relative). orderBy is a pre-resolved allowlist clause; sortBy is echoed.
//
// EventType semantics are source-anchored on AzerothCore master Guild.h (enum
// GuildBankEventLogTypes + IsMoneyEvent): 1=DEPOSIT_ITEM, 2=WITHDRAW_ITEM, 3=MOVE_ITEM,
// 4=DEPOSIT_MONEY, 5=WITHDRAW_MONEY, 6=REPAIR_MONEY, 7=MOVE_ITEM2. ONLY {4,5,6} carry copper in
// ItemOrMoney; the rest carry an item entry, so money is summed strictly over {4,5,6}.
func collectWowGuildBankActivity(ctx context.Context, db *sql.DB, now time.Time, topN int, sortBy, orderBy string, guildID int64, sinceHours int) (map[string]any, error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire conn: %w", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "USE `acore_characters`"); err != nil {
		return nil, fmt.Errorf("USE acore_characters: %w", err)
	}

	// cutoff = now - sinceHours hours; sinceHours=0 -> cutoff 0 -> `TimeStamp >= 0` matches
	// every row (TimeStamp is unsigned), so the window keeps one code path whether or not set.
	// A cutoff driven negative by clock skew is floored at 0 for the same reason.
	var cutoff int64
	if sinceHours > 0 {
		cutoff = now.Unix() - int64(sinceHours)*wowGuildBankActivityHourSecs
		if cutoff < 0 {
			cutoff = 0
		}
	}

	// ---- honest realm-wide totals (window-scoped, all guilds, pre-limit) ----
	// Over an empty table this single row has NULL sums -> COALESCE keeps them 0. moneyIn is
	// deposits (4); moneyOut is withdrawals (5) + repairs (6).
	var totalEvents, distinctGuilds, distinctActors, totalMoneyIn, totalMoneyOut int64
	if err := conn.QueryRowContext(ctx,
		"SELECT COUNT(*), COUNT(DISTINCT guildid), COUNT(DISTINCT PlayerGuid), "+
			"COALESCE(SUM(CASE WHEN EventType = 4 THEN ItemOrMoney ELSE 0 END), 0), "+
			"COALESCE(SUM(CASE WHEN EventType IN (5, 6) THEN ItemOrMoney ELSE 0 END), 0) "+
			"FROM `guild_bank_eventlog` WHERE TimeStamp >= ?", cutoff).
		Scan(&totalEvents, &distinctGuilds, &distinctActors, &totalMoneyIn, &totalMoneyOut); err != nil {
		return nil, fmt.Errorf("guild-bank-activity aggregate: %w", err)
	}

	// matchedGuilds = guilds with activity passing the guildId drilldown, pre-limit — the
	// honest denominator for truncated. With no drilldown it equals distinctGuilds (skip the
	// round trip); under a drilldown it is 0 or 1.
	matchedGuilds := distinctGuilds
	if guildID > 0 {
		if err := conn.QueryRowContext(ctx,
			"SELECT COUNT(DISTINCT guildid) FROM `guild_bank_eventlog` WHERE TimeStamp >= ? AND guildid = ?",
			cutoff, guildID).Scan(&matchedGuilds); err != nil {
			return nil, fmt.Errorf("guild-bank-activity matched-count: %w", err)
		}
	}

	// ---- per-guild list ----
	// LEFT JOIN guild so a log for a since-disbanded guild still surfaces (name NULL -> the
	// guild-<id> fallback below). The boolean SUMs (EventType = N) count matching rows; the
	// money CASE SUMs read ItemOrMoney only for money events. totalEvents/lastActivity are
	// aliased so the allowlist ORDER BY clause resolves them. GROUP BY includes g.name so the
	// query is ONLY_FULL_GROUP_BY-safe.
	where := "WHERE e.TimeStamp >= ?"
	listArgs := []any{cutoff}
	if guildID > 0 {
		where += " AND e.guildid = ?"
		listArgs = append(listArgs, guildID)
	}
	listArgs = append(listArgs, topN)
	rows, err := conn.QueryContext(ctx,
		"SELECT e.guildid, g.name, "+
			"COUNT(*) AS totalEvents, "+
			"COUNT(DISTINCT e.PlayerGuid) AS distinctActors, "+
			"COALESCE(SUM(e.EventType = 1), 0) AS itemDeposits, "+
			"COALESCE(SUM(e.EventType = 2), 0) AS itemWithdrawals, "+
			"COALESCE(SUM(e.EventType IN (3, 7)), 0) AS itemMoves, "+
			"COALESCE(SUM(CASE WHEN e.EventType = 4 THEN e.ItemOrMoney ELSE 0 END), 0) AS moneyInCopper, "+
			"COALESCE(SUM(CASE WHEN e.EventType = 5 THEN e.ItemOrMoney ELSE 0 END), 0) AS moneyWithdrawnCopper, "+
			"COALESCE(SUM(CASE WHEN e.EventType = 6 THEN e.ItemOrMoney ELSE 0 END), 0) AS repairMoneyCopper, "+
			"MIN(e.TimeStamp) AS firstActivity, MAX(e.TimeStamp) AS lastActivity "+
			"FROM `guild_bank_eventlog` e "+
			"LEFT JOIN `guild` g ON g.guildid = e.guildid "+
			where+" "+
			"GROUP BY e.guildid, g.name "+
			"ORDER BY "+orderBy+" LIMIT ?", listArgs...)
	if err != nil {
		return nil, fmt.Errorf("guild-bank-activity query: %w", err)
	}
	defer rows.Close()

	nowUnix := now.Unix()
	guilds := make([]*guildBankActivityRow, 0, topN)
	for rows.Next() {
		var (
			r                    guildBankActivityRow
			name                 sql.NullString
			moneyWithdrawnCopper int64
			firstTs, lastTs      sql.NullInt64
		)
		if err := rows.Scan(&r.GuildID, &name, &r.TotalEvents, &r.DistinctActors,
			&r.ItemDeposits, &r.ItemWithdrawals, &r.ItemMoves,
			&r.MoneyInCopper, &moneyWithdrawnCopper, &r.RepairMoneyCopper,
			&firstTs, &lastTs); err != nil {
			return nil, fmt.Errorf("guild-bank-activity scan: %w", err)
		}
		// A LEFT JOIN miss (disbanded guild with lingering log rows) or an empty name falls
		// back to guild-<id> so the row never surfaces nameless.
		if name.Valid && name.String != "" {
			r.Name = name.String
		} else {
			r.Name = fmt.Sprintf("guild-%d", r.GuildID)
		}
		// moneyOut folds withdrawals + repairs (both leave the bank); net = in - out (signed).
		r.MoneyOutCopper = moneyWithdrawnCopper + r.RepairMoneyCopper
		r.NetMoneyCopper = r.MoneyInCopper - r.MoneyOutCopper
		r.MoneyIn = formatMoney(r.MoneyInCopper)
		r.MoneyOut = formatMoney(r.MoneyOutCopper)
		r.NetMoney = formatMoney(r.NetMoneyCopper)
		if r.RepairMoneyCopper > 0 {
			r.RepairMoney = formatMoney(r.RepairMoneyCopper)
		}
		if firstTs.Valid && firstTs.Int64 > 0 {
			r.FirstActivity = firstTs.Int64
			r.FirstActivityISO = formatUnixISO(firstTs.Int64)
		}
		if lastTs.Valid && lastTs.Int64 > 0 {
			r.LastActivity = lastTs.Int64
			r.LastActivityISO = formatUnixISO(lastTs.Int64)
			// recencyHuman is age since the last event, clamped >= 0 against clock skew.
			age := nowUnix - lastTs.Int64
			if age < 0 {
				age = 0
			}
			r.RecencyHuman = formatDuration(age)
		}
		guilds = append(guilds, &r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("guild-bank-activity iter: %w", err)
	}

	out := map[string]any{
		"asOf":                now.UTC().Format(time.RFC3339),
		"windowHours":         sinceHours,
		"totalEvents":         totalEvents,
		"distinctGuilds":      distinctGuilds,
		"distinctActors":      distinctActors,
		"totalMoneyInCopper":  totalMoneyIn,
		"totalMoneyIn":        formatMoney(totalMoneyIn),
		"totalMoneyOutCopper": totalMoneyOut,
		"totalMoneyOut":       formatMoney(totalMoneyOut),
		"netMoneyCopper":      totalMoneyIn - totalMoneyOut,
		"netMoney":            formatMoney(totalMoneyIn - totalMoneyOut),
		"matchedGuilds":       matchedGuilds,
		"sortBy":              sortBy,
		"topN":                topN,
		"returned":            len(guilds),
		"truncated":           matchedGuilds > int64(len(guilds)),
		"guilds":              guilds,
	}
	// Echo the drilldown only when one was applied, so the default (realm-wide) response
	// stays clean.
	if guildID > 0 {
		out["guildId"] = guildID
	}
	return out, nil
}
