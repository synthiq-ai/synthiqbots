package mcpserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"time"
)

const (
	wowMailSummaryTimeout        = 10 * time.Second
	wowMailSummaryScanCap        = 200000 // hard ceiling on rows folded into one snapshot
	wowMailSummaryDefaultTopRecv = 10
	wowMailSummaryMaxTopRecv     = 100
)

// wowMailSummaryQuery is the single scan. acore_characters.mail columns are
// anchored on the canonical AzerothCore schema
// (data/sql/base/db_characters/mail.sql): messageType (tinyint, the
// MailMessageType), has_items (0/1), money + cod (copper, int unsigned),
// expire_time (epoch seconds — when the mail is returned/deleted by the expiry
// sweep), receiver (character guid, indexed by idx_receiver). LIMIT is bound to
// scanCap+1 so truncation is detectable without a separate COUNT(*). The
// optional receiver filter is appended before LIMIT and rides the idx_receiver
// key.
const wowMailSummaryQuery = "SELECT messageType, has_items, money, cod, expire_time, receiver FROM `mail`"

// mailTypeName maps the persisted mail.messageType (tinyint) to a label.
// Anchored on AzerothCore's MailMessageType enum
// (src/server/game/Mails/Mail.h): {NORMAL=0, AUCTION=2, CREATURE=3,
// GAMEOBJECT=4, CALENDAR=5} — note there is no value 1. An unrecognized id is
// surfaced as "type-<id>" rather than dropped, so a future/unknown mail type
// won't vanish from the snapshot silently (mirrors ahHouseFaction).
func mailTypeName(t int) string {
	switch t {
	case 0:
		return "normal"
	case 2:
		return "auction"
	case 3:
		return "creature"
	case 4:
		return "gameobject"
	case 5:
		return "calendar"
	default:
		return fmt.Sprintf("type-%d", t)
	}
}

// mailTypeBucket is the per-messageType slice of the snapshot. Copper sums are
// machine-readable int64 with a human gold string alongside, so an operator can
// eyeball "the auction bucket holds 5000g in transit" without a second tool.
type mailTypeBucket struct {
	MessageType  int    `json:"messageType"`
	TypeName     string `json:"typeName"`
	Count        int    `json:"count"`
	WithItems    int    `json:"withItems"`
	ExpiredCount int    `json:"expiredCount"`
	MoneyCopper  int64  `json:"moneyCopper"`
	MoneyGold    string `json:"moneyGold"`
	CodCopper    int64  `json:"codCopper"`
	CodGold      string `json:"codGold"`
}

// mailReceiverBucket is one entry of the top-N receivers-by-mail-count list. The
// receiverGuid is a characters.guid — resolve a name with wow_player_lookup. A
// single mailbox holding thousands of expired COD mails is a stuck-delivery /
// runaway-bot signal, so surfacing guid + count + expiredCount makes it visible
// without a separate query.
type mailReceiverBucket struct {
	ReceiverGuid int64  `json:"receiverGuid"`
	MailCount    int    `json:"mailCount"`
	WithItems    int    `json:"withItems"`
	ExpiredCount int    `json:"expiredCount"`
	MoneyCopper  int64  `json:"moneyCopper"`
	MoneyGold    string `json:"moneyGold"`
	CodCopper    int64  `json:"codCopper"`
	CodGold      string `json:"codGold"`
}

// RegisterWowMailSummaryTool registers `wow_mail_summary` — a read-only,
// mailbox-wide snapshot of the in-game mail queue aggregated from
// acore_characters.mail over the ops_ro pool.
//
// No existing opsFacing tool reads the mail table (wow_reset_character only
// DELETEs one character's mail as part of a wipe). On a mod-ah-bot server the
// mail system is the delivery channel for auction sales, won items, and
// expired-auction returns, so a backed-up or expired-mail-heavy queue is a real
// ops signal — this is the mailbox companion to the ah_market_* family (which
// covers the listing side; mail covers the delivery + money-in-transit side).
//
// Single-table by design: item-name breakdowns would need a cross-DB join to
// mail_items + item_template and are left to a possible follow-up so this stays
// a clean, schema-confident snapshot. The expired computation is now-relative
// (expire_time in the past) and folded in Go from one scan.
func RegisterWowMailSummaryTool(reg *Registry, deps DBDeps) {
	reg.Register(Tool{
		Name: "wow_mail_summary",
		Description: "Mailbox-queue health snapshot aggregated from acore_characters.mail over the ops_ro pool — " +
			"every queued mail row folded in Go from one scan, no joins. Top-level `totals`: totalMail, " +
			"withItems (has_items != 0), expiredCount (expire_time in the past — overdue for the expiry sweep / " +
			"cleanup candidates), distinctReceivers, and copper sums for money + COD in transit (with human gold " +
			"strings). `byType` splits the same metrics per messageType — 0=normal, 2=auction, 3=creature, " +
			"4=gameobject, 5=calendar (the AzerothCore MailMessageType enum; an unknown id surfaces as type-<id> " +
			"rather than vanishing). On a mod-ah-bot server auction sales + expired-auction returns are DELIVERED " +
			"VIA MAIL, so the auction bucket tracks AH delivery volume + money in transit — the mailbox companion " +
			"to the ah_market_* tools. `topReceivers` lists the top-N receiver character guids by queued-mail " +
			"count (resolve names via wow_player_lookup — one mailbox holding thousands of expired COD mails is a " +
			"stuck-delivery signal). Use when an operator asks \"is mail backing up?\", \"how much gold is sitting " +
			"in the mail?\", or \"which mailbox is overflowing?\". Args: receiver (scope to one character guid — " +
			"drilldown), topReceivers (default 10, max 100; set 0 to omit the list, distinctReceivers still " +
			"reported). Scan capped at 200000 rows; truncated:true flags an incomplete fold. Read-only, 10s timeout.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{
			"receiver":{"type":"integer","description":"Restrict to one character's mailbox by receiver guid (drilldown)"},
			"topReceivers":{"type":"integer","description":"Max receivers in the topReceivers list, by queued-mail count (default 10, max 100; set 0 to omit the list)"}
		}}`),
		Annotations: AnnRead(),
		Handler: func(ctx context.Context, raw json.RawMessage, _ string) any {
			var a struct {
				Receiver     any  `json:"receiver"`
				TopReceivers *int `json:"topReceivers"`
			}
			if len(raw) > 0 && string(raw) != "null" {
				if err := json.Unmarshal(raw, &a); err != nil {
					return map[string]any{"error": "decode args: " + err.Error()}
				}
			}
			if deps.QueryDB == nil {
				return map[string]any{"error": "ops_ro pool not configured (DB_DSN empty?)"}
			}
			receiver := numToString(a.Receiver)
			if receiver != "" {
				if _, err := strconv.ParseUint(receiver, 10, 64); err != nil {
					return map[string]any{"error": "receiver must be a positive integer (character guid)"}
				}
			}
			// *int so an explicit 0 ("omit the list") is distinguishable from
			// unset ("use the default 10").
			topReceivers := wowMailSummaryDefaultTopRecv
			if a.TopReceivers != nil {
				topReceivers = *a.TopReceivers
			}
			if topReceivers < 0 {
				topReceivers = 0
			}
			if topReceivers > wowMailSummaryMaxTopRecv {
				topReceivers = wowMailSummaryMaxTopRecv
			}
			c, cancel := context.WithTimeout(ctx, wowMailSummaryTimeout)
			defer cancel()
			out, err := collectMailSummary(c, deps.QueryDB, time.Now(), receiver, topReceivers)
			if err != nil {
				return map[string]any{"error": err.Error()}
			}
			return out
		},
	})
}

// collectMailSummary runs the single scan and folds it into per-type buckets,
// per-receiver buckets, and totals. Split out so tests can drive it with sqlmock
// and a fixed `now` (the expired count is now-relative).
//
// Aggregation is in Go (not a SQL GROUP BY) so the byType split, the totals
// fold, the expired count, and the top-N receivers all come from ONE round trip
// — the same single-scan-fold pattern ah_market_summary uses. The receiver map
// is always built (distinctReceivers is a headline metric); only the
// topReceivers LIST is omitted when the arg is 0.
func collectMailSummary(ctx context.Context, db *sql.DB, now time.Time, receiver string, topReceivers int) (map[string]any, error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire conn: %w", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "USE `acore_characters`"); err != nil {
		return nil, fmt.Errorf("USE acore_characters: %w", err)
	}

	q := wowMailSummaryQuery
	args := []any{}
	if receiver != "" {
		n, _ := strconv.ParseUint(receiver, 10, 64)
		q += " WHERE receiver = ?"
		args = append(args, n)
	}
	q += " LIMIT ?"
	args = append(args, wowMailSummaryScanCap+1)

	rows, err := conn.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("mail query: %w", err)
	}
	defer rows.Close()

	nowUnix := now.Unix()
	byType := map[int]*mailTypeBucket{}
	byRecv := map[int64]*mailReceiverBucket{}
	scanned := 0
	truncated := false
	var totMoney, totCod int64
	totWithItems, totExpired := 0, 0
	for rows.Next() {
		if scanned >= wowMailSummaryScanCap {
			truncated = true
			break
		}
		var msgType, hasItems int
		var money, cod, expireT, receiverGuid int64
		if err := rows.Scan(&msgType, &hasItems, &money, &cod, &expireT, &receiverGuid); err != nil {
			return nil, fmt.Errorf("mail scan: %w", err)
		}
		scanned++

		// expired: expire_time in [1, now]. The expireT>0 guard keeps a malformed
		// zero-time row from inflating the count (0 would otherwise always be <=
		// now) — the same defensive guard ah_market_summary uses on its expire
		// column.
		expired := expireT > 0 && expireT <= nowUnix
		withItem := hasItems != 0

		tb := byType[msgType]
		if tb == nil {
			tb = &mailTypeBucket{MessageType: msgType, TypeName: mailTypeName(msgType)}
			byType[msgType] = tb
		}
		tb.Count++
		tb.MoneyCopper += money
		tb.CodCopper += cod
		if withItem {
			tb.WithItems++
		}
		if expired {
			tb.ExpiredCount++
		}

		rb := byRecv[receiverGuid]
		if rb == nil {
			rb = &mailReceiverBucket{ReceiverGuid: receiverGuid}
			byRecv[receiverGuid] = rb
		}
		rb.MailCount++
		rb.MoneyCopper += money
		rb.CodCopper += cod
		if withItem {
			rb.WithItems++
		}
		if expired {
			rb.ExpiredCount++
		}

		totMoney += money
		totCod += cod
		if withItem {
			totWithItems++
		}
		if expired {
			totExpired++
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("mail iter: %w", err)
	}

	// byType: count desc, messageType asc as a deterministic tiebreaker (Go map
	// iteration is randomized — without the tiebreak two equal-count types would
	// swap places across calls).
	typeList := make([]*mailTypeBucket, 0, len(byType))
	for _, b := range byType {
		b.MoneyGold = formatCopper(b.MoneyCopper)
		b.CodGold = formatCopper(b.CodCopper)
		typeList = append(typeList, b)
	}
	sort.Slice(typeList, func(i, j int) bool {
		if typeList[i].Count != typeList[j].Count {
			return typeList[i].Count > typeList[j].Count
		}
		return typeList[i].MessageType < typeList[j].MessageType
	})

	// topReceivers: mailCount desc, receiverGuid asc tiebreaker. The full sorted
	// list is built so distinctReceivers stays honest; the slice to N happens
	// after (and only when the list is wanted).
	recvList := make([]*mailReceiverBucket, 0, len(byRecv))
	for _, b := range byRecv {
		b.MoneyGold = formatCopper(b.MoneyCopper)
		b.CodGold = formatCopper(b.CodCopper)
		recvList = append(recvList, b)
	}
	sort.Slice(recvList, func(i, j int) bool {
		if recvList[i].MailCount != recvList[j].MailCount {
			return recvList[i].MailCount > recvList[j].MailCount
		}
		return recvList[i].ReceiverGuid < recvList[j].ReceiverGuid
	})
	distinctReceivers := len(recvList)
	if topReceivers > 0 && len(recvList) > topReceivers {
		recvList = recvList[:topReceivers]
	}

	out := map[string]any{
		"asOf":      now.UTC().Format(time.RFC3339),
		"scanned":   scanned,
		"scanCap":   wowMailSummaryScanCap,
		"truncated": truncated,
		"byType":    typeList,
		"totals": map[string]any{
			"totalMail":            scanned,
			"withItems":            totWithItems,
			"expiredCount":         totExpired,
			"distinctReceivers":    distinctReceivers,
			"moneyInTransitCopper": totMoney,
			"moneyInTransitGold":   formatCopper(totMoney),
			"codInTransitCopper":   totCod,
			"codInTransitGold":     formatCopper(totCod),
		},
	}
	if topReceivers > 0 {
		out["topReceivers"] = recvList
	}
	if receiver != "" {
		n, _ := strconv.ParseUint(receiver, 10, 64)
		out["receiver"] = n
	}
	return out, nil
}
