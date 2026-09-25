package mcpserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

const (
	wowGuildBankItemsTimeout    = 10 * time.Second
	wowGuildBankItemsDefaultTop = 15
	wowGuildBankItemsMaxTop     = 100
	wowGuildBankItemsMaxQuality = 7 // AzerothCore item Quality enum tops out at 7 (Heirloom)
)

// wowGuildBankItemsSortColumns is the sortBy allowlist. The VALUES are interpolated
// straight into the ORDER BY (a column list can't be a bind parameter), so this map is
// the injection boundary — only these fixed clauses ever reach the query, never operator
// input. Every clause ends with an it.entry tiebreaker so paging is deterministic even
// when the primary metric ties (the ah_market_top_items / wow_guild_bank_summary idiom).
var wowGuildBankItemsSortColumns = map[string]string{
	"stacks":   "stacks DESC, it.entry ASC",
	"quantity": "totalQuantity DESC, it.entry ASC",
	"guilds":   "distinctGuilds DESC, stacks DESC, it.entry ASC",
}

// wowGuildBankItemsBaseQuery is the item-level join wow_guild_bank_summary deliberately
// left out (it stayed on guild + guild_bank_tab — the money/tab census, not the CONTENTS).
// guild_bank_item is the bridge from a guild bank slot to the item instance parked in it;
// the rest is the same item_instance -> acore_world.item_template hop
// wow_mail_item_breakdown / ah_market_top_items establish. Columns anchored on the
// canonical AzerothCore schema:
//   - guild_bank_item (data/sql/base/db_characters/guild_bank_item.sql): guildid, TabId,
//     SlotId (PK triple — one occupied bank slot), item_guid (KEY Idx_item_guid, the
//     parked item instance).
//   - item_instance (data/sql/base/db_characters/item_instance.sql): guid (PK), itemEntry
//     (the template), count (stack size).
//   - item_template (loader SELECT in ObjectMgr.cpp): entry (PK), name, Quality (tinyint,
//     rarity 0-7, capitalized), class (tinyint, ItemClass 0-16).
//
// Aggregation runs SQL-side (GROUP BY) rather than a Go-side fold: the join repeats the
// item name (varchar(255)) on every slot row, so folding in Go would transfer the name
// once per slot — the GROUP BY collapses it to one row per item before transfer (the same
// reasoning as ah_market_top_items). GROUP BY lists all four non-aggregate columns so the
// query stays valid under ONLY_FULL_GROUP_BY on both MySQL 8 and MariaDB. INNER JOINs drop
// any dangling guild_bank_item row whose item_instance was already deleted (a stale slot
// pointer), so only items that actually exist are counted — the same choice the mail tool
// makes. distinctTabs counts distinct (guildid,TabId) pairs via CONCAT because a tab is
// keyed by BOTH columns (TabId is only unique within a guild).
const wowGuildBankItemsBaseQuery = "SELECT it.entry, it.name, it.Quality, it.class, " +
	"COUNT(*) AS stacks, " +
	"COUNT(DISTINCT gbi.guildid) AS distinctGuilds, " +
	"COUNT(DISTINCT CONCAT(gbi.guildid, '-', gbi.TabId)) AS distinctTabs, " +
	"SUM(ii.count) AS totalQuantity " +
	"FROM `guild_bank_item` gbi " +
	"JOIN `item_instance` ii ON gbi.item_guid = ii.guid " +
	"JOIN `acore_world`.`item_template` it ON ii.itemEntry = it.entry"

// wowGuildBankItemsGroupBy closes the query after any WHERE filters are appended; the
// resolved allowlist ORDER BY clause and the LIMIT bind are concatenated after it.
const wowGuildBankItemsGroupBy = " GROUP BY it.entry, it.name, it.Quality, it.class ORDER BY "

// guildBankItemRow is one grouped item row of the breakdown. stacks counts the occupied
// bank slots holding this item (guild_bank_item rows — an item split across many
// slots/tabs/guilds); distinctGuilds and distinctTabs fold the spread; totalQuantity sums
// the stack sizes (item_instance.count), so a hoard of 200 stacked Frostweave shows up as
// totalQuantity 200 across however few slots hold it.
type guildBankItemRow struct {
	ItemEntry      int64  `json:"itemEntry"`
	ItemName       string `json:"itemName"`
	Quality        int    `json:"quality"`
	QualityName    string `json:"qualityName"`
	Class          int    `json:"class"`
	ClassName      string `json:"className"`
	Stacks         int    `json:"stacks"`
	DistinctGuilds int    `json:"distinctGuilds"`
	DistinctTabs   int    `json:"distinctTabs"`
	TotalQuantity  int64  `json:"totalQuantity"`
}

// RegisterWowGuildBankItemBreakdownTool registers `wow_guild_bank_item_breakdown` — a
// read-only, item-level breakdown of what sits IN guild banks aggregated over the ops_ro
// pool. It is the item-level companion to wow_guild_bank_summary: that tool answers "how
// much gold + how many tabs per guild" (the money/tab census); this answers "WHAT'S in the
// tabs" by doing the item join its summary deliberately skipped —
// acore_characters.guild_bank_item -> item_instance -> acore_world.item_template — so an
// operator sees which items are hoarded across guild banks realm-wide (or, scoped to one
// guild, that guild's bank contents).
func RegisterWowGuildBankItemBreakdownTool(reg *Registry, deps DBDeps) {
	reg.Register(Tool{
		Name: "wow_guild_bank_item_breakdown",
		Description: "Top-N items sitting in guild banks, ranked by occupied-slot count, joining " +
			"acore_characters.guild_bank_item -> item_instance -> acore_world.item_template over the ops_ro pool " +
			"(the item-level companion to wow_guild_bank_summary, which folds per-guild BankMoney + tab counts but " +
			"can't say WHAT'S in the tabs). Each item carries itemEntry, itemName, quality + qualityName (0 Poor .. 7 " +
			"Heirloom — an unknown value surfaces as quality-<q>), class + className (0 Consumable .. 16 Glyph — " +
			"unknown surfaces as class-<c>), stacks (occupied guild_bank_item slot rows holding this item — an item " +
			"split across many slots/tabs/guilds), distinctGuilds (how many guilds hold it), distinctTabs (distinct " +
			"guild+tab locations), and totalQuantity (summed item_instance.count — so a hoard of 200 stacked reagents " +
			"reads as totalQuantity 200 across however few slots). Lets an operator see which guilds are hoarding " +
			"which epics / how many stacked consumables sit in guild banks — the guild-bank contents lens next to " +
			"wow_guild_bank_summary (money census) and wow_mail_item_breakdown (mail-attachment side). Args: topN " +
			"(default 15, max 100), sortBy (one of stacks [default, most occupied slots first], quantity [most total " +
			"items], guilds [most guilds holding it] — validated against an allowlist; an unknown value is rejected), " +
			"minQuality (optional 0-7 — only items at or above this rarity; out-of-range is rejected, not clamped), " +
			"guildId (optional — scope to one guild's bank by guildid; non-numeric/negative is rejected). Sorted by the " +
			"chosen key, itemEntry asc tiebreak. Read-only, 10s timeout.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{
			"topN":{"type":"integer","description":"Max items returned, ranked by the chosen sort key (default 15, max 100)"},
			"sortBy":{"type":"string","enum":["stacks","quantity","guilds"],"description":"Sort key (default stacks = most occupied bank slots first)"},
			"minQuality":{"type":"integer","description":"Only items at or above this rarity (0=Poor .. 7=Heirloom); omit for all qualities"},
			"guildId":{"type":"integer","description":"Restrict to one guild's bank by guildid (drilldown); omit for realm-wide"}
		}}`),
		Annotations: AnnRead(),
		Handler: func(ctx context.Context, raw json.RawMessage, _ string) any {
			var a struct {
				TopN       int    `json:"topN"`
				SortBy     string `json:"sortBy"`
				MinQuality *int   `json:"minQuality"`
				GuildID    any    `json:"guildId"`
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
				topN = wowGuildBankItemsDefaultTop
			}
			if topN > wowGuildBankItemsMaxTop {
				topN = wowGuildBankItemsMaxTop
			}
			// sortBy is an enum, not a numeric knob: default when empty, but reject an
			// unknown non-empty value as a typo (silently defaulting would hide it) — and
			// only allowlisted values ever reach the interpolated ORDER BY.
			sortBy := a.SortBy
			if sortBy == "" {
				sortBy = "stacks"
			}
			orderBy, ok := wowGuildBankItemsSortColumns[sortBy]
			if !ok {
				return map[string]any{"error": fmt.Sprintf("invalid sortBy %q (valid: stacks, quantity, guilds)", a.SortBy)}
			}
			// minQuality is rejected (not silently clamped) when outside the 0-7 enum range
			// — a typo'd 99 would otherwise return an empty list that reads as "no items"
			// rather than "bad filter".
			if a.MinQuality != nil && (*a.MinQuality < 0 || *a.MinQuality > wowGuildBankItemsMaxQuality) {
				return map[string]any{"error": fmt.Sprintf("minQuality out of range: %d (valid 0-%d)", *a.MinQuality, wowGuildBankItemsMaxQuality)}
			}
			// guildId is rejected (not ignored) when present-but-invalid, BEFORE any query —
			// a bad guildid is a typo, not "the whole realm".
			guildID := numToString(a.GuildID)
			if guildID != "" {
				if _, err := strconv.ParseUint(guildID, 10, 64); err != nil {
					return map[string]any{"error": "guildId must be a positive integer (guild id)"}
				}
			}
			c, cancel := context.WithTimeout(ctx, wowGuildBankItemsTimeout)
			defer cancel()
			out, err := collectWowGuildBankItemBreakdown(c, deps.QueryDB, time.Now(), topN, sortBy, orderBy, guildID, a.MinQuality)
			if err != nil {
				return map[string]any{"error": err.Error()}
			}
			return out
		},
	})
}

// collectWowGuildBankItemBreakdown builds the (optionally filtered) join query and folds
// the grouped rows into the items slice. Split out so tests can drive it with sqlmock and a
// fixed `now` (only the asOf stamp is now-relative). The SQL ORDER BY is the source of
// truth for row order — the handler does NOT re-sort (matching ah_market_top_items /
// wow_mail_item_breakdown). orderBy is a pre-resolved allowlist clause; sortBy is echoed
// back for the caller. Arg order follows query text order: any WHERE binds (guildId, then
// minQuality), then the LIMIT bind last (there is no leading SELECT-side bind, unlike the
// mail tool's expire CASE).
func collectWowGuildBankItemBreakdown(ctx context.Context, db *sql.DB, now time.Time, topN int, sortBy, orderBy, guildID string, minQuality *int) (map[string]any, error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire conn: %w", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "USE `acore_characters`"); err != nil {
		return nil, fmt.Errorf("USE acore_characters: %w", err)
	}

	query := wowGuildBankItemsBaseQuery
	var args []any
	var conds []string
	if guildID != "" {
		n, _ := strconv.ParseUint(guildID, 10, 64)
		conds = append(conds, "gbi.guildid = ?")
		args = append(args, n)
	}
	if minQuality != nil {
		conds = append(conds, "it.Quality >= ?")
		args = append(args, *minQuality)
	}
	if len(conds) > 0 {
		query += " WHERE " + strings.Join(conds, " AND ")
	}
	query += wowGuildBankItemsGroupBy + orderBy + " LIMIT ?"
	args = append(args, topN)

	rows, err := conn.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("guild-bank-items query: %w", err)
	}
	defer rows.Close()

	items := make([]*guildBankItemRow, 0, topN)
	for rows.Next() {
		var (
			entry, totalQty                        int64
			quality, class, stacks, dGuilds, dTabs int
			name                                   string
		)
		if err := rows.Scan(&entry, &name, &quality, &class, &stacks, &dGuilds, &dTabs, &totalQty); err != nil {
			return nil, fmt.Errorf("guild-bank-items scan: %w", err)
		}
		items = append(items, &guildBankItemRow{
			ItemEntry:      entry,
			ItemName:       name,
			Quality:        quality,
			QualityName:    itemQualityName(quality),
			Class:          class,
			ClassName:      itemClassName(class),
			Stacks:         stacks,
			DistinctGuilds: dGuilds,
			DistinctTabs:   dTabs,
			TotalQuantity:  totalQty,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("guild-bank-items iter: %w", err)
	}

	out := map[string]any{
		"asOf":          now.UTC().Format(time.RFC3339),
		"topN":          topN,
		"sortBy":        sortBy,
		"distinctItems": len(items),
		"items":         items,
	}
	// Filters are echoed only when set, so an absent key == no filter applied.
	if guildID != "" {
		n, _ := strconv.ParseUint(guildID, 10, 64)
		out["guildId"] = n
	}
	if minQuality != nil {
		out["minQuality"] = *minQuality
	}
	return out, nil
}
