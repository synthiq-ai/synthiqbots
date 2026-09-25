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
	wowGuildBankItemFlowTimeout    = 10 * time.Second
	wowGuildBankItemFlowDefaultTop = 15
	wowGuildBankItemFlowMaxTop     = 100
	wowGuildBankItemFlowMaxQuality = 7 // AzerothCore item Quality enum tops out at 7 (Heirloom)
)

// wowGuildBankItemFlowSortKeys is the sortBy allowlist. Sorting runs Go-side over
// the folded slice (matching the wow_guild_bank_top_actors house style), so these
// keys never touch the SQL text — they select which aggregate the Go comparator
// reads. Keeping the sort off the interpolated ORDER BY entirely sidesteps the
// aggregate-alias footgun (an ORDER BY over an un-aliased SUM errors on real MySQL
// while sqlmock's string match stays green). "events" ranks by total item-flow
// event count (deposits+withdrawals+moves) — NOT the narrower `moves` bucket
// (item-tab-to-tab transfers, event types 3/7) which is usually the rarest facet.
var wowGuildBankItemFlowSortKeys = map[string]bool{
	"quantity": true, // default — SUM(ItemStackCount), how much stock churned
	"events":   true, // total item events touching the item (the churn count)
	"actors":   true, // distinct players moving the item
}

// wowGuildBankItemFlowBaseQuery groups guild_bank_eventlog item events by the
// item they moved and joins acore_world.item_template for names. It is the item-
// FLOW companion to wow_guild_bank_summary's money census (that tool never reads
// the eventlog). Columns anchored on the canonical AzerothCore schema:
//   - guild_bank_eventlog (CharacterDatabase.cpp CHAR_INS_GUILD_BANK_EVENTLOG):
//     guildid, EventType (tinyint — Guild.h GuildBankEventLogTypes), PlayerGuid
//     (the actor's low GUID), ItemOrMoney (OVERLOADED: item_template.entry for
//     item events, a copper amount for money events), ItemStackCount (uint16, the
//     stack size moved), TimeStamp (uint32 unix seconds).
//   - item_template (data/sql/base/db_world/item_template.sql, casing per the
//     ObjectMgr.cpp loader SELECT): entry (PK), name, Quality (capital-Q tinyint
//     rarity 0-7), class (lowercase tinyint ItemClass 0-16).
//
// The `WHERE e.EventType IN (1, 2, 3, 7)` guard is LOAD-BEARING: ItemOrMoney holds
// an item entry ONLY for the item event types (1 DEPOSIT_ITEM, 2 WITHDRAW_ITEM,
// 3 MOVE_ITEM, 7 MOVE_ITEM2). The money event types (4/5/6) store a copper amount
// in the same column, so without the guard the item_template join would garbage-
// match copper values against item ids. The three SUM(CASE ...) folds split the
// deposit / withdrawal / move facets from the same scan; COUNT(*) is the total
// item-flow event count and COUNT(DISTINCT PlayerGuid) the actor spread. The join
// is INNER (dropping a dangling entry whose template was removed) to match
// ah_market_top_items / wow_mail_item_breakdown. There is no LIMIT: the grouped
// rows are folded whole so the honest realm totals never truncate at topN, then
// the slice is sorted Go-side and cut to topN.
const wowGuildBankItemFlowBaseQuery = "SELECT it.entry, it.name, it.Quality, it.class, " +
	"SUM(CASE WHEN e.EventType = 1 THEN 1 ELSE 0 END) AS deposits, " +
	"SUM(CASE WHEN e.EventType = 2 THEN 1 ELSE 0 END) AS withdrawals, " +
	"SUM(CASE WHEN e.EventType IN (3, 7) THEN 1 ELSE 0 END) AS moves, " +
	"COUNT(*) AS totalEvents, " +
	"COUNT(DISTINCT e.PlayerGuid) AS distinctActors, " +
	"SUM(e.ItemStackCount) AS quantityMoved " +
	"FROM `guild_bank_eventlog` e " +
	"JOIN `acore_world`.`item_template` it ON e.ItemOrMoney = it.entry " +
	"WHERE e.EventType IN (1, 2, 3, 7)"

// wowGuildBankItemFlowTailQuery closes the query after any extra WHERE filters are
// appended. ORDER BY it.entry ASC only fixes a deterministic FETCH order (real
// column, no alias) — the display order is decided Go-side by sortBy.
const wowGuildBankItemFlowTailQuery = " GROUP BY it.entry, it.name, it.Quality, it.class ORDER BY it.entry ASC"

// guildBankItemFlowRow is one grouped item's flow line. deposits/withdrawals/moves
// split the item event types; totalEvents is their sum (the churn count);
// distinctActors is how many players touched the item; quantityMoved sums the
// stack sizes moved (SUM(ItemStackCount)).
type guildBankItemFlowRow struct {
	ItemEntry      int64  `json:"itemEntry"`
	ItemName       string `json:"itemName"`
	Quality        int    `json:"quality"`
	QualityName    string `json:"qualityName"`
	Class          int    `json:"class"`
	ClassName      string `json:"className"`
	Deposits       int    `json:"deposits"`
	Withdrawals    int    `json:"withdrawals"`
	Moves          int    `json:"moves"`
	TotalEvents    int    `json:"totalEvents"`
	DistinctActors int    `json:"distinctActors"`
	QuantityMoved  int64  `json:"quantityMoved"`
}

// RegisterWowGuildBankItemFlowTool registers `wow_guild_bank_item_flow` — a read-
// only, item-level view of what churns THROUGH guild banks over the ops_ro pool.
//
// wow_guild_bank_summary folds guild-bank MONEY + tab counts but never reads the
// eventlog. This tool does the item-FLOW join wow_guild_bank_summary reserves:
// acore_characters.guild_bank_eventlog -> acore_world.item_template, scoped to
// item events (types 1/2/3/7) and grouped by the item, so an operator sees WHICH
// items are hotly deposited/withdrawn/moved — the recent-activity item lens (the
// eventlog is AzerothCore's bounded per-tab ring, so it is a RECENT window, not
// full history). Companion to wow_mail_item_breakdown (the mailbox item side) and
// ah_market_top_items (the auction listing side).
func RegisterWowGuildBankItemFlowTool(reg *Registry, deps DBDeps) {
	reg.Register(Tool{
		Name: "wow_guild_bank_item_flow",
		Description: "Top-N items churning THROUGH guild banks, joining " +
			"acore_characters.guild_bank_eventlog -> acore_world.item_template over the ops_ro pool (the item-flow " +
			"join wow_guild_bank_summary skips — it folds guild-bank money + tab counts but never reads the eventlog). " +
			"Scoped to item events (EventType 1 deposit, 2 withdraw, 3/7 move) so ItemStackCount + the ItemOrMoney " +
			"item id are meaningful (money events store copper there and are excluded). Each item carries itemEntry, " +
			"itemName, quality + qualityName (0 Poor .. 7 Heirloom — an unknown value surfaces as quality-<q>), class " +
			"+ className, deposits, withdrawals, moves, totalEvents (the churn count), distinctActors (players who " +
			"moved it), and quantityMoved (summed ItemStackCount). Headline totals are honest and realm-wide " +
			"(pre-topN): distinctItems, totalItemEvents, totalQuantityMoved. The eventlog is AzerothCore's bounded " +
			"per-tab ring, so this is a RECENT-activity window, not full history. Args: topN (default 15, max 100), " +
			"sortBy (quantity [default] | events | actors — validated against an allowlist, sorted Go-side; an " +
			"unknown key is rejected), minQuality (optional 0-7 — only items at or above this rarity; out-of-range is " +
			"rejected, not clamped), guildId (optional — drill into one guild's bank; non-numeric/negative is " +
			"rejected), sinceHours (optional — only events in the last N hours; default 0 = the whole ring). The " +
			"item-flow companion to wow_mail_item_breakdown (mailbox items) and ah_market_top_items (AH listings). " +
			"Read-only, 10 s timeout.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{
			"topN":{"type":"integer","description":"Max items returned (default 15, max 100)"},
			"sortBy":{"type":"string","enum":["quantity","events","actors"],"description":"Sort key (default quantity = most stock moved)"},
			"minQuality":{"type":"integer","description":"Only items at or above this rarity (0=Poor .. 7=Heirloom); omit for all qualities"},
			"guildId":{"type":"integer","description":"Restrict to one guild's bank by guildid (drilldown)"},
			"sinceHours":{"type":"integer","description":"Only events whose TimeStamp is within the last N hours (default 0 = the whole bounded ring)"}
		}}`),
		Annotations: AnnRead(),
		Handler: func(ctx context.Context, raw json.RawMessage, _ string) any {
			var a struct {
				TopN       int    `json:"topN"`
				SortBy     string `json:"sortBy"`
				MinQuality *int   `json:"minQuality"`
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
				topN = wowGuildBankItemFlowDefaultTop
			}
			if topN > wowGuildBankItemFlowMaxTop {
				topN = wowGuildBankItemFlowMaxTop
			}
			// sortBy defaults to quantity and is rejected (not silently coerced) when
			// unknown — a typo'd key would otherwise return a plausibly-ordered list
			// that reads as "sorted by X" when it isn't.
			sortBy := a.SortBy
			if sortBy == "" {
				sortBy = "quantity"
			}
			if !wowGuildBankItemFlowSortKeys[sortBy] {
				return map[string]any{"error": fmt.Sprintf("invalid sortBy %q (valid: quantity, events, actors)", sortBy)}
			}
			// minQuality is rejected (not clamped) when outside the 0-7 enum range —
			// a typo'd 99 would otherwise return an empty list that reads as "no
			// items" rather than "bad filter".
			if a.MinQuality != nil && (*a.MinQuality < 0 || *a.MinQuality > wowGuildBankItemFlowMaxQuality) {
				return map[string]any{"error": fmt.Sprintf("minQuality out of range: %d (valid 0-%d)", *a.MinQuality, wowGuildBankItemFlowMaxQuality)}
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
			c, cancel := context.WithTimeout(ctx, wowGuildBankItemFlowTimeout)
			defer cancel()
			out, err := collectWowGuildBankItemFlow(c, deps.QueryDB, time.Now(), topN, sortBy, guildID, a.MinQuality, sinceHours)
			if err != nil {
				return map[string]any{"error": err.Error()}
			}
			return out
		},
	})
}

// collectWowGuildBankItemFlow builds the (optionally filtered) join query, folds
// every grouped item, computes the honest realm totals from the full set, then
// sorts Go-side by sortBy and cuts to topN. Split out so tests can drive it with
// sqlmock and a fixed `now` (the sinceHours cutoff is now-relative).
//
// Arg order follows query text order: the base WHERE (EventType IN) carries no
// bind; the optional filters bind in the order they are appended — guildId, then
// the sinceHours cutoff, then minQuality. TimeStamp is a uint32 unix-seconds
// column, so the cutoff binds as an int64 unix second (cutoff.Unix()), NOT a
// time.Time (which the driver would render as a DATETIME string and mis-compare).
func collectWowGuildBankItemFlow(ctx context.Context, db *sql.DB, now time.Time, topN int, sortBy, guildID string, minQuality *int, sinceHours int) (map[string]any, error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire conn: %w", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "USE `acore_characters`"); err != nil {
		return nil, fmt.Errorf("USE acore_characters: %w", err)
	}

	query := wowGuildBankItemFlowBaseQuery
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
	if minQuality != nil {
		conds = append(conds, "it.Quality >= ?")
		args = append(args, *minQuality)
	}
	if len(conds) > 0 {
		query += " AND " + strings.Join(conds, " AND ")
	}
	query += wowGuildBankItemFlowTailQuery

	rows, err := conn.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("item-flow query: %w", err)
	}
	defer rows.Close()

	all := make([]*guildBankItemFlowRow, 0, 64)
	var totalItemEvents, totalQuantityMoved int64
	for rows.Next() {
		var (
			entry, quantityMoved                                    int64
			name                                                    string
			quality, class, deposits, withdrawals, moves, totalEvts int
			distinctActors                                          int
		)
		if err := rows.Scan(&entry, &name, &quality, &class, &deposits, &withdrawals, &moves, &totalEvts, &distinctActors, &quantityMoved); err != nil {
			return nil, fmt.Errorf("item-flow scan: %w", err)
		}
		all = append(all, &guildBankItemFlowRow{
			ItemEntry:      entry,
			ItemName:       name,
			Quality:        quality,
			QualityName:    itemQualityName(quality),
			Class:          class,
			ClassName:      itemClassName(class),
			Deposits:       deposits,
			Withdrawals:    withdrawals,
			Moves:          moves,
			TotalEvents:    totalEvts,
			DistinctActors: distinctActors,
			QuantityMoved:  quantityMoved,
		})
		totalItemEvents += int64(totalEvts)
		totalQuantityMoved += quantityMoved
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("item-flow iter: %w", err)
	}

	// Rank Go-side by the chosen aggregate, itemEntry asc as the stable tiebreak,
	// then cut to topN. Totals above are computed over the FULL set so they stay
	// honest regardless of topN.
	sort.SliceStable(all, func(i, j int) bool {
		var vi, vj int64
		switch sortBy {
		case "events":
			vi, vj = int64(all[i].TotalEvents), int64(all[j].TotalEvents)
		case "actors":
			vi, vj = int64(all[i].DistinctActors), int64(all[j].DistinctActors)
		default: // quantity
			vi, vj = all[i].QuantityMoved, all[j].QuantityMoved
		}
		if vi != vj {
			return vi > vj
		}
		return all[i].ItemEntry < all[j].ItemEntry
	})
	display := all
	if len(display) > topN {
		display = display[:topN]
	}

	out := map[string]any{
		"asOf":               now.UTC().Format(time.RFC3339),
		"topN":               topN,
		"sortBy":             sortBy,
		"distinctItems":      len(all),
		"totalItemEvents":    totalItemEvents,
		"totalQuantityMoved": totalQuantityMoved,
		"items":              display,
	}
	// Filters are echoed only when set, so an absent key == no filter applied.
	if guildID != "" {
		n, _ := strconv.ParseUint(guildID, 10, 64)
		out["guildId"] = n
	}
	if minQuality != nil {
		out["minQuality"] = *minQuality
	}
	if sinceHours > 0 {
		out["sinceHours"] = sinceHours
		out["sinceCutoff"] = cutoff.UTC().Format(time.RFC3339)
	}
	return out, nil
}
