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
	wowMailItemsTimeout    = 10 * time.Second
	wowMailItemsDefaultTop = 15
	wowMailItemsMaxTop     = 100
	wowMailItemsMaxQuality = 7 // AzerothCore item Quality enum tops out at 7 (Heirloom)
)

// wowMailItemsBaseQuery is the cross-DB join wow_mail_summary deliberately left
// out (it stayed single-table on acore_characters.mail). mail_items is the
// bridge from a mail to the item instances attached to it; the rest is the same
// item_instance -> acore_world.item_template hop ah_market_top_items establishes.
// Columns anchored on the canonical AzerothCore schema:
//   - mail_items (data/sql/base/db_characters/mail_items.sql): mail_id (the mail
//     row, KEY idx_mail_id), item_guid (PK, the attached item instance), receiver
//     (character guid, KEY idx_receiver — denormalized so a mailbox drilldown
//     rides the index without joining mail).
//   - item_instance (data/sql/base/db_characters/item_instance.sql): guid (PK),
//     itemEntry (the template), count (stack size).
//   - item_template (data/sql/base/db_world/item_template.sql): entry (PK), name,
//     Quality (tinyint, rarity 0-7; capitalized in the schema).
//   - mail (data/sql/base/db_characters/mail.sql): id (PK), expire_time (epoch
//     seconds — when the mail is returned/deleted by the expiry sweep).
//
// Aggregation runs SQL-side (GROUP BY) rather than a Go-side fold: the join
// repeats the item name (varchar(255)) on every attachment row, so folding in Go
// would transfer the name once per attachment — the GROUP BY collapses it to one
// row per item before transfer (the same reasoning as ah_market_top_items).
// GROUP BY lists all three non-aggregate columns so the query stays valid under
// ONLY_FULL_GROUP_BY on both MySQL 8 and MariaDB. expiredItems is folded in SQL
// via SUM(CASE ...) against a now bind; the expire_time>0 guard keeps a malformed
// zero-time row out of the count (the same guard wow_mail_summary uses). The now
// bind is the FIRST positional arg because the CASE precedes any WHERE.
const wowMailItemsBaseQuery = "SELECT it.entry, it.name, it.Quality, " +
	"COUNT(*) AS mailItems, " +
	"COUNT(DISTINCT mi.mail_id) AS distinctMails, " +
	"COUNT(DISTINCT mi.receiver) AS distinctReceivers, " +
	"SUM(ii.count) AS totalQuantity, " +
	"SUM(CASE WHEN m.expire_time > 0 AND m.expire_time <= ? THEN 1 ELSE 0 END) AS expiredItems " +
	"FROM `mail_items` mi " +
	"JOIN `item_instance` ii ON mi.item_guid = ii.guid " +
	"JOIN `acore_world`.`item_template` it ON ii.itemEntry = it.entry " +
	"JOIN `mail` m ON mi.mail_id = m.id"

// wowMailItemsTailQuery closes the query after any WHERE filters are appended.
const wowMailItemsTailQuery = " GROUP BY it.entry, it.name, it.Quality ORDER BY mailItems DESC, it.entry ASC LIMIT ?"

// mailItemBreakdownRow is one grouped item row of the breakdown. mailItems counts
// the attachment rows (an item can be split across many mails); distinctMails and
// distinctReceivers fold the spread; totalQuantity sums the stack sizes; and
// expiredItems flags how many of those attachments sit in mail that is already
// past expire_time (overdue for the sweep — about to be returned/deleted).
type mailItemBreakdownRow struct {
	ItemEntry         int64  `json:"itemEntry"`
	ItemName          string `json:"itemName"`
	Quality           int    `json:"quality"`
	QualityName       string `json:"qualityName"`
	MailItems         int    `json:"mailItems"`
	DistinctMails     int    `json:"distinctMails"`
	DistinctReceivers int    `json:"distinctReceivers"`
	TotalQuantity     int64  `json:"totalQuantity"`
	ExpiredItems      int    `json:"expiredItems"`
}

// RegisterWowMailItemBreakdownTool registers `wow_mail_item_breakdown` — a
// read-only, item-level breakdown of items attached to in-game mail aggregated
// over the ops_ro pool.
//
// wow_mail_summary folds the mail QUEUE (per-type counts, money in transit, top
// receivers) but stayed single-table — it can't say WHICH items are sitting in
// the mail. This tool does the cross-DB join its doc comment reserved:
// acore_characters.mail_items -> item_instance -> acore_world.item_template ->
// mail. On a mod-ah-bot server won auction items and expired-auction returns are
// delivered as mail attachments, so a pile-up of one item is a stuck-delivery /
// runaway-bot signal — this is the item-level companion to wow_mail_summary (the
// queue side) and ah_market_top_items (the listing side).
func RegisterWowMailItemBreakdownTool(reg *Registry, deps DBDeps) {
	reg.Register(Tool{
		Name: "wow_mail_item_breakdown",
		Description: "Top-N items attached to in-game mail, ranked by attachment count, joining " +
			"acore_characters.mail_items -> item_instance -> acore_world.item_template -> mail over the ops_ro pool " +
			"(the cross-DB join wow_mail_summary deliberately skips — it folds the mail queue but can't say WHICH " +
			"items are sitting in it). Each item carries itemEntry, itemName, quality + qualityName (0 Poor .. 7 " +
			"Heirloom — an unknown value surfaces as quality-<q>), mailItems (attachment rows — an item split across " +
			"many mails), distinctMails, distinctReceivers, totalQuantity (summed stack sizes), and expiredItems " +
			"(attachments whose mail is past expire_time — overdue for the expiry sweep, about to be returned or " +
			"deleted). On a mod-ah-bot server won auction items and expired-auction returns are DELIVERED AS MAIL " +
			"ATTACHMENTS, so a pile-up of one item is a stuck-delivery signal — the item-level companion to " +
			"wow_mail_summary (queue totals) and ah_market_top_items (the listing side). Args: topN (default 15, " +
			"max 100), minQuality (optional 0-7 — only items at or above this rarity; out-of-range is rejected, not " +
			"clamped), receiver (optional — scope to one character's mailbox by guid, rides idx_receiver; " +
			"non-numeric/negative is rejected), expiredOnly (optional — true restricts to items in already-expired " +
			"mail). Sorted mailItems desc, itemEntry asc tiebreak. Read-only, 10s timeout.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{
			"topN":{"type":"integer","description":"Max items returned, ranked by attachment count (default 15, max 100)"},
			"minQuality":{"type":"integer","description":"Only items at or above this rarity (0=Poor .. 7=Heirloom); omit for all qualities"},
			"receiver":{"type":"integer","description":"Restrict to one character's mailbox by receiver guid (drilldown)"},
			"expiredOnly":{"type":"boolean","description":"When true, only count items in mail past its expire_time (overdue for the sweep)"}
		}}`),
		Annotations: AnnRead(),
		Handler: func(ctx context.Context, raw json.RawMessage, _ string) any {
			var a struct {
				TopN        int  `json:"topN"`
				MinQuality  *int `json:"minQuality"`
				Receiver    any  `json:"receiver"`
				ExpiredOnly bool `json:"expiredOnly"`
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
				topN = wowMailItemsDefaultTop
			}
			if topN > wowMailItemsMaxTop {
				topN = wowMailItemsMaxTop
			}
			// minQuality is rejected (not silently clamped) when outside the 0-7
			// enum range — a typo'd 99 would otherwise return an empty list that
			// reads as "no items" rather than "bad filter".
			if a.MinQuality != nil && (*a.MinQuality < 0 || *a.MinQuality > wowMailItemsMaxQuality) {
				return map[string]any{"error": fmt.Sprintf("minQuality out of range: %d (valid 0-%d)", *a.MinQuality, wowMailItemsMaxQuality)}
			}
			// receiver is rejected (not ignored) when present-but-invalid, BEFORE any
			// query — a bad guid is a typo, not "the whole mailbox".
			receiver := numToString(a.Receiver)
			if receiver != "" {
				if _, err := strconv.ParseUint(receiver, 10, 64); err != nil {
					return map[string]any{"error": "receiver must be a positive integer (character guid)"}
				}
			}
			c, cancel := context.WithTimeout(ctx, wowMailItemsTimeout)
			defer cancel()
			out, err := collectWowMailItemBreakdown(c, deps.QueryDB, time.Now(), topN, receiver, a.MinQuality, a.ExpiredOnly)
			if err != nil {
				return map[string]any{"error": err.Error()}
			}
			return out
		},
	})
}

// collectWowMailItemBreakdown builds the (optionally filtered) join query and
// folds the grouped rows into the items slice. Split out so tests can drive it
// with sqlmock and a fixed `now` (the expiredItems SUM is now-relative). The SQL
// ORDER BY is the source of truth for row order — the handler does NOT re-sort
// (matching ah_market_top_items).
//
// Arg order follows query text order: the SELECT CASE's now bind is FIRST, then
// any WHERE binds (receiver, minQuality, and a second now bind when expiredOnly
// is set), then the LIMIT bind last.
func collectWowMailItemBreakdown(ctx context.Context, db *sql.DB, now time.Time, topN int, receiver string, minQuality *int, expiredOnly bool) (map[string]any, error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire conn: %w", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "USE `acore_characters`"); err != nil {
		return nil, fmt.Errorf("USE acore_characters: %w", err)
	}

	query := wowMailItemsBaseQuery
	args := []any{now.Unix()} // SELECT CASE expire bind — first in text order
	var conds []string
	if receiver != "" {
		n, _ := strconv.ParseUint(receiver, 10, 64)
		conds = append(conds, "mi.receiver = ?")
		args = append(args, n)
	}
	if minQuality != nil {
		conds = append(conds, "it.Quality >= ?")
		args = append(args, *minQuality)
	}
	if expiredOnly {
		conds = append(conds, "m.expire_time > 0 AND m.expire_time <= ?")
		args = append(args, now.Unix())
	}
	if len(conds) > 0 {
		query += " WHERE " + strings.Join(conds, " AND ")
	}
	query += wowMailItemsTailQuery
	args = append(args, topN)

	rows, err := conn.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("mail-items query: %w", err)
	}
	defer rows.Close()

	items := make([]*mailItemBreakdownRow, 0, topN)
	for rows.Next() {
		var (
			entry, totalQty                                          int64
			quality, mailItems, distinctMails, distinctRecv, expired int
			name                                                     string
		)
		if err := rows.Scan(&entry, &name, &quality, &mailItems, &distinctMails, &distinctRecv, &totalQty, &expired); err != nil {
			return nil, fmt.Errorf("mail-items scan: %w", err)
		}
		items = append(items, &mailItemBreakdownRow{
			ItemEntry:         entry,
			ItemName:          name,
			Quality:           quality,
			QualityName:       itemQualityName(quality),
			MailItems:         mailItems,
			DistinctMails:     distinctMails,
			DistinctReceivers: distinctRecv,
			TotalQuantity:     totalQty,
			ExpiredItems:      expired,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("mail-items iter: %w", err)
	}

	out := map[string]any{
		"asOf":          now.UTC().Format(time.RFC3339),
		"topN":          topN,
		"distinctItems": len(items),
		"items":         items,
	}
	// Filters are echoed only when set, so an absent key == no filter applied.
	if receiver != "" {
		n, _ := strconv.ParseUint(receiver, 10, 64)
		out["receiver"] = n
	}
	if minQuality != nil {
		out["minQuality"] = *minQuality
	}
	if expiredOnly {
		out["expiredOnly"] = true
	}
	return out, nil
}
