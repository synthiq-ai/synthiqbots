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
	wowMailUnreadTimeout         = 10 * time.Second
	wowMailUnreadDefaultTop      = 15
	wowMailUnreadMaxTop          = 100
	wowMailUnreadSubjectMaxBytes = 120 // defensive clip — subject is longtext

	// `checked` is a tinyint-unsigned BITMASK (AzerothCore MailCheckMask, Mail.h) —
	// NOT a boolean. These are the bits this tool reads. MAIL_CHECK_MASK_READ is the
	// load-bearing one: it is set the moment the receiver OPENS the mail, so a row
	// with that bit CLEAR is one the receiver has never read.
	wowMailReadCheckBit       = 0x01 // MAIL_CHECK_MASK_READ — cleared => never opened by the receiver
	wowMailReturnedCheckBit   = 0x02 // MAIL_CHECK_MASK_RETURNED — an expired/returned-to-sender mail
	wowMailCodPaymentCheckBit = 0x08 // MAIL_CHECK_MASK_COD_PAYMENT — a COD payment was collected
)

// wowMailUnreadWhereBase is the WHERE fragment shared by the honest-total COUNT and
// the top-N list. `(checked & 1) = 0` is the core predicate — MAIL_CHECK_MASK_READ
// clear means the receiver has never opened the mail (delivered-but-never-read, a
// different "stuck" axis than wow_mail_oldest's deliver_time AGE). `deliver_time > 0`
// drops malformed zero-time rows; `deliver_time <= ?` (the cutoff bind) restricts to
// mail that has ACTUALLY been delivered — a future-dated mail (an AH/COD delivery
// delay) is not "unread", it simply has not arrived yet, so it must not count. The
// cutoff also carries the minAgeHours refinement: cutoff = now - minAgeHours*3600, so
// a larger minAgeHours surfaces only the mail that has been sitting unread the longest.
const wowMailUnreadWhereBase = "FROM `mail` WHERE (checked & 1) = 0 AND deliver_time > 0 AND deliver_time <= ?"

// wowMailUnreadListCols is the column set the list query scans, in order. `checked` is
// included (over wow_mail_oldest's set) so the response can decode the bitmask.
const wowMailUnreadListCols = "id, messageType, sender, receiver, subject, has_items, deliver_time, expire_time, money, cod, checked"

// wowMailUnreadTailQuery closes the list query after any AND filters are appended. The
// SQL ORDER BY is the source of truth for row order (oldest-delivered first — the mail
// that has been unread the longest); the handler does NOT re-sort. id ASC is a
// deterministic tiebreaker on equal deliver_time.
const wowMailUnreadTailQuery = " ORDER BY deliver_time ASC, id ASC LIMIT ?"

// mailUnreadRow is one delivered-but-never-read mail. senderGuid/receiverGuid are
// characters.guid — resolve names with wow_player_lookup. ageSeconds (now -
// deliver_time) is always >= 0 here: the cutoff filter excludes not-yet-delivered
// mail, so unlike wow_mail_oldest there is no future-delivery negative-age case.
// checkedMask is the raw bitmask; returned/codPayment decode its non-READ bits so an
// operator can tell an unread expired-auction return (RETURNED) from a plain unread
// fresh delivery. The READ bit is not surfaced — every row here is unread by
// construction.
type mailUnreadRow struct {
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
	CheckedMask    int    `json:"checkedMask"`
	Returned       bool   `json:"returned"`
	CodPayment     bool   `json:"codPayment"`
	MoneyCopper    int64  `json:"moneyCopper"`
	MoneyGold      string `json:"moneyGold"`
	CodCopper      int64  `json:"codCopper"`
	CodGold        string `json:"codGold"`
}

// RegisterWowMailUnreadTool registers `wow_mail_unread` — a read-only, per-mail-row
// view of DELIVERED mail the receiver has never opened (`checked & MAIL_CHECK_MASK_READ
// == 0`) over the ops_ro pool.
//
// wow_mail_oldest ranks the oldest queued mail by deliver_time regardless of read
// state; wow_mail_summary folds the queue into aggregate totals; wow_mail_item_breakdown
// groups by attached item. NONE of them isolate the "delivered but never READ" axis —
// mail that reached the mailbox and is being ignored (a bot that never processes its
// mail, auction returns piling up unopened). This tool returns the individual unread
// mails oldest-delivered first, plus an honest totalUnread count so the top-N is read
// against the full pile ("top 15 of 3,412 unread").
func RegisterWowMailUnreadTool(reg *Registry, deps DBDeps) {
	reg.Register(Tool{
		Name: "wow_mail_unread",
		Description: "Delivered-but-never-opened mail from acore_characters.mail over the ops_ro pool — the " +
			"per-mail-ROW drilldown of mail the receiver has never READ, a different stuck axis than wow_mail_oldest " +
			"(oldest by deliver_time regardless of read state), wow_mail_summary (aggregate queue totals) and " +
			"wow_mail_item_breakdown (per-item GROUP BY). The `checked` column is a tinyint BITMASK (MailCheckMask), " +
			"and MAIL_CHECK_MASK_READ (the 0x01 bit) is set the moment the receiver opens the mail; this tool selects " +
			"rows where that bit is CLEAR ((checked & 1) = 0), scoped to mail that has actually been delivered " +
			"(deliver_time > 0 AND deliver_time <= now) so a future-dated AH/COD delivery is not miscounted as unread. " +
			"Each mail carries mailId, messageType + typeName (0=normal, 2=auction, 3=creature, 4=gameobject, " +
			"5=calendar — the MailMessageType enum; an unknown id surfaces as type-<id>), senderGuid + receiverGuid " +
			"(resolve names via wow_player_lookup), subject (clipped), hasItems, deliverTime + deliverTimeISO, " +
			"ageSeconds + ageHuman (now minus deliver_time — how long it has sat unread), expireTime + expireTimeISO + " +
			"expired, checkedMask + returned (MAIL_CHECK_MASK_RETURNED — an unread expired-auction return) + codPayment " +
			"(COD), and money/cod copper + gold. On a mod-ah-bot server auction sales and expired-auction returns are " +
			"DELIVERED VIA MAIL, so a growing unread pile is a bot-not-processing-mail signal — the companion to " +
			"wow_mail_oldest, wow_mail_summary and the ah_market_* listing side. Ordered deliver_time asc (longest " +
			"unread first), mailId asc tiebreak. Alongside the top-N list it returns totalUnread (the honest full count " +
			"of delivered unread mail, pre-limit) and truncated. Args: topN (default 15, max 100), receiver (optional — " +
			"scope to one character's mailbox by guid, rides idx_receiver; non-numeric/negative is rejected), " +
			"messageType (optional — scope to one mail type 0/2/3/4/5; negative is rejected), minAgeHours (optional, " +
			"default 0 — only mail delivered more than N hours ago, isolating the genuinely-stale unread mail; negative " +
			"clamps to 0). Read-only, 10s timeout.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{
			"topN":{"type":"integer","description":"Max unread mails returned, longest-unread first (default 15, max 100)"},
			"receiver":{"type":"integer","description":"Restrict to one character's mailbox by receiver guid (drilldown)"},
			"messageType":{"type":"integer","description":"Restrict to one mail type (0=normal, 2=auction, 3=creature, 4=gameobject, 5=calendar); omit for all types"},
			"minAgeHours":{"type":"integer","description":"Only mail delivered more than N hours ago (default 0 = all delivered unread); negative clamps to 0"}
		}}`),
		Annotations: AnnRead(),
		Handler: func(ctx context.Context, raw json.RawMessage, _ string) any {
			var a struct {
				TopN        int  `json:"topN"`
				Receiver    any  `json:"receiver"`
				MessageType *int `json:"messageType"`
				MinAgeHours int  `json:"minAgeHours"`
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
				topN = wowMailUnreadDefaultTop
			}
			if topN > wowMailUnreadMaxTop {
				topN = wowMailUnreadMaxTop
			}
			// minAgeHours is a numeric threshold (like topN), so a negative value is
			// clamped to 0 (all delivered unread) rather than rejected — a negative age
			// would ask for mail delivered in the future, which is nonsensical.
			minAgeHours := a.MinAgeHours
			if minAgeHours < 0 {
				minAgeHours = 0
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
			// distinguishable from unset (no type filter). A negative value is rejected
			// (not ignored) — messageType is tinyint unsigned, so a negative is a typo;
			// an unrecognized non-negative passes through and simply matches no rows
			// (honest empty), labelled type-<id> in the echo.
			if a.MessageType != nil && *a.MessageType < 0 {
				return map[string]any{"error": fmt.Sprintf("messageType must be non-negative: %d", *a.MessageType)}
			}
			c, cancel := context.WithTimeout(ctx, wowMailUnreadTimeout)
			defer cancel()
			out, err := collectWowMailUnread(c, deps.QueryDB, time.Now(), topN, receiver, a.MessageType, minAgeHours)
			if err != nil {
				return map[string]any{"error": err.Error()}
			}
			return out
		},
	})
}

// collectWowMailUnread runs the honest-total COUNT and the top-N list over the same
// WHERE, then folds the rows. Split out so tests can drive it with sqlmock and a fixed
// `now` (the cutoff, ageSeconds and the expired flag are all now-relative). The SQL
// ORDER BY is the source of truth for row order — the handler does NOT re-sort.
//
// Bind order follows query text order: the cutoff bind first (it is the leading WHERE
// term), then any receiver/messageType filters, then — for the LIST only — the LIMIT
// bind last. The COUNT shares the WHERE binds without the LIMIT.
func collectWowMailUnread(ctx context.Context, db *sql.DB, now time.Time, topN int, receiver string, messageType *int, minAgeHours int) (map[string]any, error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire conn: %w", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "USE `acore_characters`"); err != nil {
		return nil, fmt.Errorf("USE acore_characters: %w", err)
	}

	nowUnix := now.Unix()
	cutoff := nowUnix - int64(minAgeHours)*3600

	where := wowMailUnreadWhereBase
	whereArgs := []any{cutoff}
	if receiver != "" {
		n, _ := strconv.ParseUint(receiver, 10, 64)
		where += " AND receiver = ?"
		whereArgs = append(whereArgs, n)
	}
	if messageType != nil {
		where += " AND messageType = ?"
		whereArgs = append(whereArgs, *messageType)
	}

	// Honest total: the full count of delivered unread mail matching the filters,
	// BEFORE the top-N truncation — so the caller can tell a handful from a flood.
	var totalUnread int64
	if err := conn.QueryRowContext(ctx, "SELECT COUNT(*) "+where, whereArgs...).Scan(&totalUnread); err != nil {
		return nil, fmt.Errorf("mail-unread count: %w", err)
	}

	listArgs := append(append([]any{}, whereArgs...), topN)
	rows, err := conn.QueryContext(ctx, "SELECT "+wowMailUnreadListCols+" "+where+wowMailUnreadTailQuery, listArgs...)
	if err != nil {
		return nil, fmt.Errorf("mail-unread query: %w", err)
	}
	defer rows.Close()

	mails := make([]*mailUnreadRow, 0, topN)
	for rows.Next() {
		var (
			id, sender, receiverGuid, deliverT, expireT, money, cod int64
			msgType, hasItems, checked                              int
			subject                                                 sql.NullString
		)
		if err := rows.Scan(&id, &msgType, &sender, &receiverGuid, &subject, &hasItems, &deliverT, &expireT, &money, &cod, &checked); err != nil {
			return nil, fmt.Errorf("mail-unread scan: %w", err)
		}
		subj := ""
		if subject.Valid {
			subj = clipBytes(subject.String, wowMailUnreadSubjectMaxBytes)
		}
		age := nowUnix - deliverT
		mails = append(mails, &mailUnreadRow{
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
			CheckedMask:    checked,
			Returned:       checked&wowMailReturnedCheckBit != 0,
			CodPayment:     checked&wowMailCodPaymentCheckBit != 0,
			MoneyCopper:    money,
			MoneyGold:      formatCopper(money),
			CodCopper:      cod,
			CodGold:        formatCopper(cod),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("mail-unread iter: %w", err)
	}

	out := map[string]any{
		"asOf":        now.UTC().Format(time.RFC3339),
		"topN":        topN,
		"minAgeHours": minAgeHours,
		"totalUnread": totalUnread,
		"returned":    len(mails),
		"truncated":   totalUnread > int64(len(mails)),
		"mails":       mails,
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
