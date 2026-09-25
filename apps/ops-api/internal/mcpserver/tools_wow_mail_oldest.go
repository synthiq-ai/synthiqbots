package mcpserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"
	"time"
)

const (
	wowMailOldestTimeout         = 10 * time.Second
	wowMailOldestDefaultTop      = 15
	wowMailOldestMaxTop          = 100
	wowMailOldestSubjectMaxBytes = 120 // defensive clip — subject is longtext
)

// wowMailOldestBaseQuery scans the oldest mails by deliver_time. Single-table on
// acore_characters.mail (no joins) — the per-mail-ROW drilldown wow_mail_summary
// (aggregate queue totals) and wow_mail_item_breakdown (per-item GROUP BY) can't
// give. Columns anchored on the canonical AzerothCore schema
// (data/sql/base/db_characters/mail.sql): id (PK), messageType (tinyint, the
// MailMessageType), sender + receiver (character guids; receiver KEY idx_receiver),
// subject (longtext, nullable), has_items (0/1), deliver_time (epoch seconds — when
// the mail became deliverable; the age clock), expire_time (epoch seconds — when the
// expiry sweep returns/deletes it), money + cod (copper, int unsigned).
//
// `deliver_time > 0` is ALWAYS in the WHERE and is load-bearing: it is the SORT KEY,
// and a malformed deliver_time=0 row would sort to the very top of an ascending list
// (epoch 0 = 1970, "infinitely old") and dominate the oldest-N with phantom ancient
// mail. Excluding it keeps the oldest list honest — the same expire_time>0 reasoning
// wow_mail_summary applies to its expired count, here applied to the sort key itself.
const wowMailOldestBaseQuery = "SELECT id, messageType, sender, receiver, subject, has_items, deliver_time, expire_time, money, cod " +
	"FROM `mail` WHERE deliver_time > 0"

// wowMailOldestTailQuery closes the query after any AND filters are appended. The
// SQL ORDER BY is the source of truth for row order (oldest first); the handler does
// NOT re-sort. id ASC is a deterministic tiebreaker on equal deliver_time.
const wowMailOldestTailQuery = " ORDER BY deliver_time ASC, id ASC LIMIT ?"

// mailOldestRow is one mail row of the oldest-N list. senderGuid/receiverGuid are
// characters.guid — resolve names with wow_player_lookup. ageSeconds is now-relative
// (now - deliver_time); it can be negative for a mail scheduled for FUTURE delivery
// (an AH/COD delivery delay), in which case ageHuman floors at "0m" — such mails sort
// last and only surface in a near-empty mailbox. expired is the now-relative
// expire_time check (the expire_time>0 guard keeps a malformed zero-time row from
// reading as expired), independent of the age sort.
type mailOldestRow struct {
	MailID         int64  `json:"mailId"`
	MessageType    int    `json:"messageType"`
	TypeName       string `json:"typeName"`
	SenderGuid     int64  `json:"senderGuid"`
	ReceiverGuid   int64  `json:"receiverGuid"`
	Subject        string `json:"subject"`
	HasItems       bool   `json:"hasItems"`
	DeliverTime    int64  `json:"deliverTime"`
	DeliverTimeISO string `json:"deliverTimeISO"`
	AgeSeconds     int64  `json:"ageSeconds"`
	AgeHuman       string `json:"ageHuman"`
	ExpireTime     int64  `json:"expireTime"`
	ExpireTimeISO  string `json:"expireTimeISO,omitempty"`
	Expired        bool   `json:"expired"`
	MoneyCopper    int64  `json:"moneyCopper"`
	MoneyGold      string `json:"moneyGold"`
	CodCopper      int64  `json:"codCopper"`
	CodGold        string `json:"codGold"`
}

// RegisterWowMailOldestTool registers `wow_mail_oldest` — a read-only, per-mail-row
// view of the oldest queued mails by deliver_time over the ops_ro pool.
//
// wow_mail_summary folds the mail queue into aggregate totals and
// wow_mail_item_breakdown groups by attached item, but neither names the SPECIFIC
// individual mails that have been sitting the longest. On a mod-ah-bot server the
// mail system is the delivery channel for auction sales, won items, and
// expired-auction returns, so the single oldest stuck mails are the "what's been
// undelivered the longest?" drilldown — the per-row companion to wow_mail_summary
// (queue totals) and wow_mail_item_breakdown (item-level), and to the ah_market_*
// listing side.
func RegisterWowMailOldestTool(reg *Registry, deps DBDeps) {
	reg.Register(Tool{
		Name: "wow_mail_oldest",
		Description: "Oldest queued mails by deliver_time, scanned from acore_characters.mail over the ops_ro pool " +
			"(single-table, no joins) — the per-mail-ROW drilldown wow_mail_summary (aggregate queue totals) and " +
			"wow_mail_item_breakdown (per-item GROUP BY) can't give: it names the SPECIFIC individual mails that have " +
			"been sitting undelivered the longest. Each mail carries mailId, messageType + typeName (0=normal, " +
			"2=auction, 3=creature, 4=gameobject, 5=calendar — the MailMessageType enum; an unknown id surfaces as " +
			"type-<id>), senderGuid + receiverGuid (resolve names via wow_player_lookup), subject (clipped), hasItems, " +
			"deliverTime + deliverTimeISO, ageSeconds + ageHuman (now minus deliver_time — how long it has been " +
			"stuck), expireTime + expireTimeISO + expired (now past expire_time — overdue for the expiry sweep), and " +
			"money/cod copper + gold. On a mod-ah-bot server auction sales and expired-auction returns are DELIVERED " +
			"VIA MAIL, so the oldest stuck mails are a stuck-delivery signal — the per-row companion to wow_mail_summary " +
			"and ah_market_* tools. Ordered deliver_time asc (oldest first), mailId asc tiebreak; rows with " +
			"deliver_time=0 are excluded so a malformed zero-time row can't masquerade as the oldest mail. Args: topN " +
			"(default 15, max 100), receiver (optional — scope to one character's mailbox by guid, rides idx_receiver; " +
			"non-numeric/negative is rejected), messageType (optional — scope to one mail type 0/2/3/4/5; negative is " +
			"rejected). Read-only, 10s timeout.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{
			"topN":{"type":"integer","description":"Max mails returned, oldest first (default 15, max 100)"},
			"receiver":{"type":"integer","description":"Restrict to one character's mailbox by receiver guid (drilldown)"},
			"messageType":{"type":"integer","description":"Restrict to one mail type (0=normal, 2=auction, 3=creature, 4=gameobject, 5=calendar); omit for all types"}
		}}`),
		Annotations: AnnRead(),
		Handler: func(ctx context.Context, raw json.RawMessage, _ string) any {
			var a struct {
				TopN        int  `json:"topN"`
				Receiver    any  `json:"receiver"`
				MessageType *int `json:"messageType"`
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
				topN = wowMailOldestDefaultTop
			}
			if topN > wowMailOldestMaxTop {
				topN = wowMailOldestMaxTop
			}
			// receiver is rejected (not ignored) when present-but-invalid, BEFORE any
			// query — a bad guid is a typo, not "the whole mailbox".
			receiver := numToString(a.Receiver)
			if receiver != "" {
				if _, err := strconv.ParseUint(receiver, 10, 64); err != nil {
					return map[string]any{"error": "receiver must be a positive integer (character guid)"}
				}
			}
			// messageType is a *int so an explicit 0 (filter to normal mail) is
			// distinguishable from unset (no type filter). A negative value is
			// rejected (not ignored) — messageType is tinyint unsigned, so a negative
			// is a typo; an unrecognized non-negative passes through and simply matches
			// no rows (honest empty), labelled type-<id> in the echo.
			if a.MessageType != nil && *a.MessageType < 0 {
				return map[string]any{"error": fmt.Sprintf("messageType must be non-negative: %d", *a.MessageType)}
			}
			c, cancel := context.WithTimeout(ctx, wowMailOldestTimeout)
			defer cancel()
			out, err := collectWowMailOldest(c, deps.QueryDB, time.Now(), topN, receiver, a.MessageType)
			if err != nil {
				return map[string]any{"error": err.Error()}
			}
			return out
		},
	})
}

// collectWowMailOldest builds the (optionally filtered) scan and folds the rows into
// the mails slice. Split out so tests can drive it with sqlmock and a fixed `now`
// (ageSeconds and the expired flag are now-relative). The SQL ORDER BY is the source
// of truth for row order — the handler does NOT re-sort (matching ah_market_top_items
// and wow_mail_item_breakdown).
//
// Arg order follows query text order: any WHERE filters (receiver, messageType) first,
// then the LIMIT bind last. There is no now-bind in SQL — the now-relative math is
// done per row in Go, so the oldest-N selection stays a pure deliver_time sort.
func collectWowMailOldest(ctx context.Context, db *sql.DB, now time.Time, topN int, receiver string, messageType *int) (map[string]any, error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire conn: %w", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "USE `acore_characters`"); err != nil {
		return nil, fmt.Errorf("USE acore_characters: %w", err)
	}

	query := wowMailOldestBaseQuery
	args := []any{}
	if receiver != "" {
		n, _ := strconv.ParseUint(receiver, 10, 64)
		query += " AND receiver = ?"
		args = append(args, n)
	}
	if messageType != nil {
		query += " AND messageType = ?"
		args = append(args, *messageType)
	}
	query += wowMailOldestTailQuery
	args = append(args, topN)

	rows, err := conn.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("mail-oldest query: %w", err)
	}
	defer rows.Close()

	nowUnix := now.Unix()
	mails := make([]*mailOldestRow, 0, topN)
	for rows.Next() {
		var (
			id, sender, receiverGuid, deliverT, expireT, money, cod int64
			msgType, hasItems                                       int
			subject                                                 sql.NullString
		)
		if err := rows.Scan(&id, &msgType, &sender, &receiverGuid, &subject, &hasItems, &deliverT, &expireT, &money, &cod); err != nil {
			return nil, fmt.Errorf("mail-oldest scan: %w", err)
		}
		subj := ""
		if subject.Valid {
			subj = clipBytes(subject.String, wowMailOldestSubjectMaxBytes)
		}
		age := nowUnix - deliverT
		mails = append(mails, &mailOldestRow{
			MailID:         id,
			MessageType:    msgType,
			TypeName:       mailTypeName(msgType),
			SenderGuid:     sender,
			ReceiverGuid:   receiverGuid,
			Subject:        subj,
			HasItems:       hasItems != 0,
			DeliverTime:    deliverT,
			DeliverTimeISO: formatUnixISO(deliverT),
			AgeSeconds:     age,
			AgeHuman:       formatDuration(age),
			ExpireTime:     expireT,
			ExpireTimeISO:  formatUnixISO(expireT),
			Expired:        expireT > 0 && expireT <= nowUnix,
			MoneyCopper:    money,
			MoneyGold:      formatCopper(money),
			CodCopper:      cod,
			CodGold:        formatCopper(cod),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("mail-oldest iter: %w", err)
	}

	out := map[string]any{
		"asOf":     now.UTC().Format(time.RFC3339),
		"topN":     topN,
		"returned": len(mails),
		"mails":    mails,
	}
	// Filters are echoed only when set, so an absent key == no filter applied.
	if receiver != "" {
		n, _ := strconv.ParseUint(receiver, 10, 64)
		out["receiver"] = n
	}
	if messageType != nil {
		out["messageType"] = *messageType
		out["messageTypeName"] = mailTypeName(*messageType)
	}
	return out, nil
}
