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
	wowGuildMembershipChurnTimeout    = 10 * time.Second
	wowGuildMembershipChurnDefaultTop = 25
	wowGuildMembershipChurnMaxTop     = 200
)

// guild_eventlog.EventType values (AC Guild.h GuildEventLogTypes, source-anchored
// in cron-state research-cache/acore-guild-logs.json, re-verified Guild.h:208-216):
// 1 INVITE_PLAYER, 2 JOIN_GUILD, 3 PROMOTE_PLAYER, 4 DEMOTE_PLAYER,
// 5 UNINVITE_PLAYER, 6 LEAVE_GUILD. This is the MEMBERSHIP log — a DIFFERENT table
// from guild_bank_eventlog, whose GuildBankEventLogTypes overlap the numbers 1-6
// with a totally different meaning. The values are inlined in the SUM(CASE ...)
// splits of wowGuildMembershipChurnBaseQuery (mirroring wow_guild_bank_item_flow's
// SQL-side split), so there is no Go-side EventType switch to name them for.

// wowGuildMembershipChurnSortKeys is the sortBy allowlist. Sorting runs Go-side
// over the folded slice (the wow_guild_bank_item_flow / wow_guild_bank_top_actors
// house style), so these keys never touch the SQL text — they select which folded
// aggregate the Go comparator reads. Keeping the sort off the interpolated ORDER BY
// entirely sidesteps the aggregate-alias footgun (an ORDER BY over an un-aliased
// SUM errors on real MySQL while sqlmock's string match stays green). All keys sort
// DESC: "netMembers" (joins − leaves, default) surfaces the fastest-GROWING guilds
// first (a bleeding guild has a negative net and sinks to the bottom); "leaves"
// directly answers "which guilds are bleeding the most members"; "events" ranks by
// raw membership-log churn (rank thrashing + invite spam included).
var wowGuildMembershipChurnSortKeys = map[string]bool{
	"netMembers": true, // default — joins − leaves, growth vs bleed
	"joins":      true, // most new members
	"leaves":     true, // most departures (bleeding guilds)
	"events":     true, // most total membership-log activity
}

// wowGuildMembershipChurnBaseQuery groups guild_eventlog by guild and splits the
// six membership event types with SUM(CASE ...) folds from one scan, joining
// acore_characters.guild for the guild name. It is the MEMBERSHIP-dynamics
// companion to the guild-BANK family (money #325, contents #334, capacity #341,
// activity #345, actor #349, item-flow #353) — none of which read guild_eventlog.
// Columns anchored on the canonical AzerothCore schema:
//   - guild_eventlog (CharacterDatabase.cpp CHAR_INS_GUILD_EVENTLOG): guildid,
//     EventType (tinyint — Guild.h GuildEventLogTypes 1-6), PlayerGuid1 (actor low
//     GUID), PlayerGuid2 (target low GUID; == actor for join/leave), NewRank
//     (meaningful only for promote/demote), TimeStamp (uint32 unix seconds).
//   - guild (data/sql/base/db_characters/guild.sql): guildid (PK), name.
//
// Unlike guild_bank_eventlog's ItemOrMoney (overloaded item-id/copper) column, no
// event type here carries a value that would garbage-join, so there is no
// EventType base guard — every row is a membership event and COUNT(*) is the total.
// The join is INNER (matching wow_guild_bank_item_flow's item_template join): a
// guild_eventlog row whose guild row is gone (a disbanded guild — AC's
// Guild::Disband deletes its eventlog rows too, so orphans are not expected) is
// dropped from BOTH the display and the honest totals, keeping the totals
// consistent with the named guilds shown. There is no LIMIT: guild count is
// bounded, so the grouped rows are folded whole (honest realm totals never
// truncate at topN), then sorted Go-side and cut to topN.
const wowGuildMembershipChurnBaseQuery = "SELECT g.guildid, g.name, " +
	"SUM(CASE WHEN e.EventType = 2 THEN 1 ELSE 0 END) AS joins, " +
	"SUM(CASE WHEN e.EventType = 6 THEN 1 ELSE 0 END) AS leaves, " +
	"SUM(CASE WHEN e.EventType = 3 THEN 1 ELSE 0 END) AS promotes, " +
	"SUM(CASE WHEN e.EventType = 4 THEN 1 ELSE 0 END) AS demotes, " +
	"SUM(CASE WHEN e.EventType = 1 THEN 1 ELSE 0 END) AS invites, " +
	"SUM(CASE WHEN e.EventType = 5 THEN 1 ELSE 0 END) AS uninvites, " +
	"COUNT(*) AS totalEvents, " +
	"MAX(e.TimeStamp) AS lastActivity " +
	"FROM `guild_eventlog` e " +
	"JOIN `guild` g ON e.guildid = g.guildid"

// wowGuildMembershipChurnTailQuery closes the query after any optional WHERE
// filters are appended. ORDER BY g.guildid ASC only fixes a deterministic FETCH
// order (a real column, no alias) — the display order is decided Go-side by sortBy.
const wowGuildMembershipChurnTailQuery = " GROUP BY g.guildid, g.name ORDER BY g.guildid ASC"

// guildMembershipChurnRow is one guild's membership-dynamics line. joins/leaves/
// promotes/demotes/invites/uninvites split the six event types; netMembers is
// joins − leaves (can be negative for a bleeding guild); totalEvents is the total
// membership-log row count (the churn magnitude); lastActivity is the newest event
// TimeStamp in the window (the dormancy signal for spotting dying guilds).
type guildMembershipChurnRow struct {
	GuildID         int64  `json:"guildId"`
	Name            string `json:"name"`
	Joins           int    `json:"joins"`
	Leaves          int    `json:"leaves"`
	Promotes        int    `json:"promotes"`
	Demotes         int    `json:"demotes"`
	Invites         int    `json:"invites"`
	Uninvites       int    `json:"uninvites"`
	NetMembers      int    `json:"netMembers"`
	TotalEvents     int    `json:"totalEvents"`
	LastActivity    int64  `json:"lastActivity"`
	LastActivityISO string `json:"lastActivityISO,omitempty"`
}

// RegisterWowGuildMembershipChurnTool registers `wow_guild_membership_churn` — a
// read-only, realm-wide per-guild view of membership dynamics over the ops_ro pool.
//
// The guild-BANK family reads guild_bank_eventlog exhaustively, but the guild
// MEMBERSHIP log — guild_eventlog — has no ops-api tool: the bot-facing
// get_guild_event_log (#221) reads it per-bot, with no realm-wide OPERATOR rollup.
// This does the membership-log join wow_guild_summary reserves (that tool folds
// guild_member headcount but never reads the eventlog): GROUP BY guildid over
// guild_eventlog, the six event types split with SUM(CASE ...) exactly as AC's
// Guild.h GuildEventLogTypes classifies them, joined to guild for the name, so an
// operator sees which guilds are growing vs bleeding members / thrashing ranks —
// the membership-audit axis, companion to wow_guild_bank_item_flow (the bank
// item-flow side). The eventlog is AzerothCore's bounded per-guild ring, so this is
// a RECENT-activity window, not full history.
func RegisterWowGuildMembershipChurnTool(reg *Registry, deps DBDeps) {
	reg.Register(Tool{
		Name: "wow_guild_membership_churn",
		Description: "Realm-wide per-guild MEMBERSHIP dynamics, joining " +
			"acore_characters.guild_eventlog -> guild over the ops_ro pool (the membership-log join " +
			"wow_guild_summary skips — it folds guild_member headcount but never reads the eventlog, and the deep " +
			"guild-bank family reads only guild_bank_eventlog). guild_eventlog is a DIFFERENT table from " +
			"guild_bank_eventlog: it logs who joins/leaves/gets promoted, the realm-wide OPERATOR rollup the " +
			"per-bot get_guild_event_log has no equivalent for. Each guild's six EventType facets are split with " +
			"SUM(CASE ...) per AC Guild.h GuildEventLogTypes: joins (JOIN_GUILD 2), leaves (LEAVE_GUILD 6), " +
			"promotes (PROMOTE_PLAYER 3), demotes (DEMOTE_PLAYER 4), invites (INVITE_PLAYER 1), uninvites " +
			"(UNINVITE_PLAYER 5); netMembers = joins − leaves (negative = a bleeding guild); totalEvents is the " +
			"churn magnitude; lastActivity + lastActivityISO flag dormancy. Headline totals are honest and " +
			"realm-wide (pre-topN): distinctGuilds, totalJoins, totalLeaves, totalEvents, netMembers. The eventlog " +
			"is AzerothCore's bounded per-guild ring, so this is a RECENT-activity window, not full history. Args: " +
			"topN (default 25, max 200), sortBy (netMembers [default, fastest-growing first] | joins | leaves " +
			"[bleeding guilds] | events — validated against an allowlist, sorted Go-side; an unknown key is " +
			"rejected), guildId (optional — drill into one guild by guildid; non-numeric/negative is rejected), " +
			"sinceHours (optional — only events in the last N hours; default 0 = the whole bounded ring). The " +
			"membership companion to wow_guild_summary (headcount census) and wow_guild_bank_item_flow (bank " +
			"item-flow). Read-only, 10 s timeout.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{
			"topN":{"type":"integer","description":"Max guilds returned (default 25, max 200)"},
			"sortBy":{"type":"string","enum":["netMembers","joins","leaves","events"],"description":"Sort key (default netMembers = fastest-growing first; leaves = bleeding guilds)"},
			"guildId":{"type":"integer","description":"Restrict to one guild by guildid (drilldown)"},
			"sinceHours":{"type":"integer","description":"Only events whose TimeStamp is within the last N hours (default 0 = the whole bounded ring)"}
		}}`),
		Annotations: AnnRead(),
		Handler: func(ctx context.Context, raw json.RawMessage, _ string) any {
			var a struct {
				TopN       int    `json:"topN"`
				SortBy     string `json:"sortBy"`
				GuildID    any    `json:"guildId"`
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
				topN = wowGuildMembershipChurnDefaultTop
			}
			if topN > wowGuildMembershipChurnMaxTop {
				topN = wowGuildMembershipChurnMaxTop
			}
			// sortBy defaults to netMembers and is rejected (not silently coerced)
			// when unknown — a typo'd key would otherwise return a plausibly-ordered
			// list that reads as "sorted by X" when it isn't.
			sortBy := a.SortBy
			if sortBy == "" {
				sortBy = "netMembers"
			}
			if !wowGuildMembershipChurnSortKeys[sortBy] {
				return map[string]any{"error": fmt.Sprintf("invalid sortBy %q (valid: netMembers, joins, leaves, events)", sortBy)}
			}
			// guildId is rejected (not ignored) when present-but-invalid, BEFORE any
			// query — a bad guildid is a typo, not "the whole realm".
			guildID := numToString(a.GuildID)
			if guildID != "" {
				if _, err := strconv.ParseUint(guildID, 10, 64); err != nil {
					return map[string]any{"error": "guildId must be a positive integer (guild id)"}
				}
			}
			sinceHours := a.SinceHours
			if sinceHours < 0 {
				sinceHours = 0
			}
			c, cancel := context.WithTimeout(ctx, wowGuildMembershipChurnTimeout)
			defer cancel()
			out, err := collectWowGuildMembershipChurn(c, deps.QueryDB, time.Now(), topN, sortBy, guildID, sinceHours)
			if err != nil {
				return map[string]any{"error": err.Error()}
			}
			return out
		},
	})
}

// collectWowGuildMembershipChurn builds the (optionally filtered) join query, folds
// every grouped guild, computes the honest realm totals from the full set, then
// sorts Go-side by sortBy and cuts to topN. Split out so tests can drive it with
// sqlmock and a fixed `now` (the sinceHours cutoff is now-relative).
//
// Arg order follows query text order: the optional filters bind in the order they
// are appended — guildId, then the sinceHours cutoff. TimeStamp is a uint32
// unix-seconds column, so the cutoff binds as an int64 unix second (cutoff.Unix()),
// NOT a time.Time (which the driver would render as a DATETIME string and
// mis-compare against the integer column).
func collectWowGuildMembershipChurn(ctx context.Context, db *sql.DB, now time.Time, topN int, sortBy, guildID string, sinceHours int) (map[string]any, error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire conn: %w", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "USE `acore_characters`"); err != nil {
		return nil, fmt.Errorf("USE acore_characters: %w", err)
	}

	query := wowGuildMembershipChurnBaseQuery
	var args []any
	var conds []string
	if guildID != "" {
		n, _ := strconv.ParseUint(guildID, 10, 64)
		conds = append(conds, "e.guildid = ?")
		args = append(args, n)
	}
	var cutoff time.Time
	if sinceHours > 0 {
		cutoff = now.UTC().Add(-time.Duration(sinceHours) * time.Hour)
		conds = append(conds, "e.TimeStamp >= ?")
		args = append(args, cutoff.Unix())
	}
	if len(conds) > 0 {
		query += " WHERE " + strings.Join(conds, " AND ")
	}
	query += wowGuildMembershipChurnTailQuery

	rows, err := conn.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("membership-churn query: %w", err)
	}
	defer rows.Close()

	all := make([]*guildMembershipChurnRow, 0, 64)
	var totalJoins, totalLeaves, totalEvents int64
	for rows.Next() {
		var (
			guildid, lastActivity                                        int64
			name                                                         string
			joins, leaves, promotes, demotes, invites, uninvites, totEvt int
		)
		if err := rows.Scan(&guildid, &name, &joins, &leaves, &promotes, &demotes, &invites, &uninvites, &totEvt, &lastActivity); err != nil {
			return nil, fmt.Errorf("membership-churn scan: %w", err)
		}
		all = append(all, &guildMembershipChurnRow{
			GuildID:         guildid,
			Name:            name,
			Joins:           joins,
			Leaves:          leaves,
			Promotes:        promotes,
			Demotes:         demotes,
			Invites:         invites,
			Uninvites:       uninvites,
			NetMembers:      joins - leaves,
			TotalEvents:     totEvt,
			LastActivity:    lastActivity,
			LastActivityISO: formatUnixISO(lastActivity),
		})
		totalJoins += int64(joins)
		totalLeaves += int64(leaves)
		totalEvents += int64(totEvt)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("membership-churn iter: %w", err)
	}

	// Rank Go-side by the chosen aggregate (all DESC), guildId asc as the stable
	// tiebreak, then cut to topN. Totals above are computed over the FULL set so
	// they stay honest regardless of topN.
	sort.SliceStable(all, func(i, j int) bool {
		var vi, vj int64
		switch sortBy {
		case "joins":
			vi, vj = int64(all[i].Joins), int64(all[j].Joins)
		case "leaves":
			vi, vj = int64(all[i].Leaves), int64(all[j].Leaves)
		case "events":
			vi, vj = int64(all[i].TotalEvents), int64(all[j].TotalEvents)
		default: // netMembers
			vi, vj = int64(all[i].NetMembers), int64(all[j].NetMembers)
		}
		if vi != vj {
			return vi > vj
		}
		return all[i].GuildID < all[j].GuildID
	})
	display := all
	if len(display) > topN {
		display = display[:topN]
	}

	out := map[string]any{
		"asOf":           now.UTC().Format(time.RFC3339),
		"topN":           topN,
		"sortBy":         sortBy,
		"distinctGuilds": len(all),
		"totalJoins":     totalJoins,
		"totalLeaves":    totalLeaves,
		"totalEvents":    totalEvents,
		"netMembers":     totalJoins - totalLeaves,
		"guilds":         display,
	}
	// Filters are echoed only when set, so an absent key == no filter applied.
	if guildID != "" {
		n, _ := strconv.ParseUint(guildID, 10, 64)
		out["guildId"] = n
	}
	if sinceHours > 0 {
		out["sinceHours"] = sinceHours
		out["sinceCutoff"] = cutoff.UTC().Format(time.RFC3339)
	}
	return out, nil
}
