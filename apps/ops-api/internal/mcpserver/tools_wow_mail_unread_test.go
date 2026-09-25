package mcpserver

import (
	"context"
	"database/sql/driver"
	"encoding/json"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

// mailUnreadRowCols is the column set wow_mail_unread's list query scans, in order.
// It carries `checked` (over wow_mail_oldest's set) so the bitmask can be decoded.
func mailUnreadRowCols() []string {
	return []string{"id", "messageType", "sender", "receiver", "subject", "has_items", "deliver_time", "expire_time", "money", "cod", "checked"}
}

// TestWowMailUnread_DescriptionMentionsContext guards the description keywords — they
// shape which tool the agent reaches for when an operator asks "what mail is being
// ignored / never opened?". Drop the checked/MAIL_CHECK_MASK_READ framing or the
// sibling cross-references and the agent falls back to wow_mail_oldest (a different
// axis) or a hand-written db_query.
func TestWowMailUnread_DescriptionMentionsContext(t *testing.T) {
	reg := NewRegistry()
	RegisterWowMailUnreadTool(reg, DBDeps{})
	tool, ok := reg.Get("wow_mail_unread")
	if !ok {
		t.Fatal("wow_mail_unread not registered")
	}
	for _, kw := range []string{
		"acore_characters.mail", "ops_ro", "checked", "MailCheckMask", "MAIL_CHECK_MASK_READ",
		"(checked & 1) = 0", "deliver_time", "wow_mail_oldest", "wow_mail_summary",
		"wow_mail_item_breakdown", "wow_player_lookup", "messageType", "MailMessageType",
		"normal", "auction", "creature", "gameobject", "calendar", "type-<id>",
		"ageSeconds", "ageHuman", "expired", "checkedMask", "returned", "MAIL_CHECK_MASK_RETURNED",
		"codPayment", "totalUnread", "truncated", "minAgeHours", "idx_receiver", "receiver",
		"topN", "mod-ah-bot", "ah_market_", "Read-only",
	} {
		if !strings.Contains(tool.Description, kw) {
			t.Errorf("description missing %q: %s", kw, tool.Description)
		}
	}
}

// TestWowMailUnread_ReadOnlyAnnotation pins readOnlyHint=true so a future copy-paste of
// a destructive sibling can't silently flip the gate.
func TestWowMailUnread_ReadOnlyAnnotation(t *testing.T) {
	reg := NewRegistry()
	RegisterWowMailUnreadTool(reg, DBDeps{})
	tool, _ := reg.Get("wow_mail_unread")
	if !strings.Contains(string(tool.Annotations), `"readOnlyHint":true`) {
		t.Errorf("annotations missing readOnlyHint=true: %s", tool.Annotations)
	}
}

func TestWowMailUnread_NoPoolReturnsError(t *testing.T) {
	reg := NewRegistry()
	RegisterWowMailUnreadTool(reg, DBDeps{}) // QueryDB nil
	tool, _ := reg.Get("wow_mail_unread")
	resp := tool.Handler(context.Background(), json.RawMessage(`{}`), "")
	got, _ := resp.(map[string]any)
	if msg, _ := got["error"].(string); !strings.Contains(msg, "ops_ro pool not configured") {
		t.Errorf("expected pool-not-configured error, got %v", got)
	}
}

// TestWowMailUnread_TopNClamps asserts the post-clamp topN echo (unset->default,
// zero/negative->default, oversized->max, in-range passthrough) and that the LIMIT bind
// matches the clamped value. The leading cutoff bind is now-relative (time.Now() in the
// handler path) so it is matched with AnyArg; the COUNT query precedes the list.
func TestWowMailUnread_TopNClamps(t *testing.T) {
	cases := []struct {
		name     string
		args     string
		wantTopN int
	}{
		{"unset uses default", `{}`, wowMailUnreadDefaultTop},
		{"zero falls back to default", `{"topN":0}`, wowMailUnreadDefaultTop},
		{"negative falls back to default", `{"topN":-7}`, wowMailUnreadDefaultTop},
		{"oversized clamps to max", `{"topN":99999}`, wowMailUnreadMaxTop},
		{"in-range passes through", `{"topN":42}`, 42},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			mock.ExpectExec(regexp.QuoteMeta("USE `acore_characters`")).
				WillReturnResult(sqlmock.NewResult(0, 0))
			mock.ExpectQuery(regexp.QuoteMeta("SELECT COUNT(*) FROM `mail` WHERE (checked & 1) = 0")).
				WithArgs(sqlmock.AnyArg()).
				WillReturnRows(sqlmock.NewRows([]string{"cnt"}).AddRow(int64(0)))
			mock.ExpectQuery(regexp.QuoteMeta("ORDER BY deliver_time ASC, id ASC LIMIT ?")).
				WithArgs(sqlmock.AnyArg(), int64(c.wantTopN)).
				WillReturnRows(sqlmock.NewRows(mailUnreadRowCols()))

			reg := NewRegistry()
			RegisterWowMailUnreadTool(reg, DBDeps{QueryDB: db})
			tool, _ := reg.Get("wow_mail_unread")
			resp := tool.Handler(context.Background(), json.RawMessage(c.args), "")
			got, _ := resp.(map[string]any)
			if got["error"] != nil {
				t.Fatalf("unexpected error: %v", got)
			}
			if n, _ := got["topN"].(int); n != c.wantTopN {
				t.Errorf("topN: %v want %d", got["topN"], c.wantTopN)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Errorf("unmet mock expectations: %v", err)
			}
		})
	}
}

// TestWowMailUnread_ReceiverRejectsBadGuid — a non-numeric / negative receiver is
// rejected BEFORE any query is issued (no SQL round trip on a bad filter).
func TestWowMailUnread_ReceiverRejectsBadGuid(t *testing.T) {
	for _, args := range []string{`{"receiver":"abc"}`, `{"receiver":-5}`} {
		db, mock, err := sqlmock.New()
		if err != nil {
			t.Fatal(err)
		}
		// No ExpectExec / ExpectQuery — the handler must bail before touching the DB.
		reg := NewRegistry()
		RegisterWowMailUnreadTool(reg, DBDeps{QueryDB: db})
		tool, _ := reg.Get("wow_mail_unread")
		resp := tool.Handler(context.Background(), json.RawMessage(args), "")
		got, _ := resp.(map[string]any)
		if msg, _ := got["error"].(string); !strings.Contains(msg, "receiver must be a positive integer") {
			t.Errorf("args %s: expected receiver reject, got %v", args, got)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("args %s: a query was issued on a bad receiver: %v", args, err)
		}
		db.Close()
	}
}

// TestWowMailUnread_MessageTypeRejectsNegative — a negative messageType is rejected
// BEFORE any query (no SQL round trip). A non-negative unknown type is NOT rejected (it
// rides through and matches no rows — honest empty), exercised in the Filters test.
func TestWowMailUnread_MessageTypeRejectsNegative(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	reg := NewRegistry()
	RegisterWowMailUnreadTool(reg, DBDeps{QueryDB: db})
	tool, _ := reg.Get("wow_mail_unread")
	resp := tool.Handler(context.Background(), json.RawMessage(`{"messageType":-1}`), "")
	got, _ := resp.(map[string]any)
	if msg, _ := got["error"].(string); !strings.Contains(msg, "messageType must be non-negative") {
		t.Errorf("expected messageType reject, got %v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("a query was issued on a negative messageType: %v", err)
	}
}

// TestWowMailUnread_Filters drives each filter combination and asserts the WHERE shape
// (filters ANDed onto the always-present unread + delivered predicate), the bound args
// (cutoff first — AnyArg since it is now-relative — then filters, then LIMIT for the
// list), and the echoed receiver/messageType/messageTypeName keys. The explicit
// messageType=0 case proves the *int distinguishes "filter to normal mail" from "no
// type filter".
func TestWowMailUnread_Filters(t *testing.T) {
	cases := []struct {
		name       string
		args       string
		topN       int
		filterArgs []driver.Value // receiver / messageType binds (between cutoff and LIMIT)
		whereSub   string
		wantRecv   int64  // -1 = key absent
		wantType   int    // -1 = key absent
		wantTName  string // "" = key absent
	}{
		{
			name:       "receiver only",
			args:       `{"receiver":777}`,
			topN:       wowMailUnreadDefaultTop,
			filterArgs: []driver.Value{uint64(777)},
			whereSub:   "AND deliver_time <= ? AND receiver = ? ORDER BY",
			wantRecv:   777, wantType: -1,
		},
		{
			name:       "messageType only (auction)",
			args:       `{"messageType":2}`,
			topN:       wowMailUnreadDefaultTop,
			filterArgs: []driver.Value{int64(2)},
			whereSub:   "AND deliver_time <= ? AND messageType = ? ORDER BY",
			wantRecv:   -1,
			wantType:   2,
			wantTName:  "auction",
		},
		{
			name:       "messageType explicit zero (normal) is a real filter",
			args:       `{"messageType":0}`,
			topN:       wowMailUnreadDefaultTop,
			filterArgs: []driver.Value{int64(0)},
			whereSub:   "AND deliver_time <= ? AND messageType = ? ORDER BY",
			wantRecv:   -1,
			wantType:   0,
			wantTName:  "normal",
		},
		{
			name:       "both filters + topN",
			args:       `{"receiver":123,"messageType":3,"topN":5}`,
			topN:       5,
			filterArgs: []driver.Value{uint64(123), int64(3)},
			whereSub:   "AND deliver_time <= ? AND receiver = ? AND messageType = ? ORDER BY",
			wantRecv:   123,
			wantType:   3,
			wantTName:  "creature",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			countArgs := append([]driver.Value{sqlmock.AnyArg()}, c.filterArgs...)
			listArgs := append(append([]driver.Value{}, countArgs...), int64(c.topN))
			mock.ExpectExec(regexp.QuoteMeta("USE `acore_characters`")).
				WillReturnResult(sqlmock.NewResult(0, 0))
			mock.ExpectQuery(regexp.QuoteMeta("SELECT COUNT(*) FROM `mail` WHERE (checked & 1) = 0")).
				WithArgs(countArgs...).
				WillReturnRows(sqlmock.NewRows([]string{"cnt"}).AddRow(int64(0)))
			mock.ExpectQuery(regexp.QuoteMeta(c.whereSub)).
				WithArgs(listArgs...).
				WillReturnRows(sqlmock.NewRows(mailUnreadRowCols()))

			reg := NewRegistry()
			RegisterWowMailUnreadTool(reg, DBDeps{QueryDB: db})
			tool, _ := reg.Get("wow_mail_unread")
			resp := tool.Handler(context.Background(), json.RawMessage(c.args), "")
			got, _ := resp.(map[string]any)
			if got["error"] != nil {
				t.Fatalf("unexpected error: %v", got)
			}
			if c.wantRecv >= 0 {
				if r, _ := got["receiver"].(uint64); r != uint64(c.wantRecv) {
					t.Errorf("receiver echo: %v want %d", got["receiver"], c.wantRecv)
				}
			} else if _, ok := got["receiver"]; ok {
				t.Errorf("receiver should be absent, got %v", got["receiver"])
			}
			if c.wantType >= 0 {
				if mt, _ := got["messageType"].(int); mt != c.wantType {
					t.Errorf("messageType echo: %v want %d", got["messageType"], c.wantType)
				}
				if tn, _ := got["messageTypeName"].(string); tn != c.wantTName {
					t.Errorf("messageTypeName echo: %q want %q", got["messageTypeName"], c.wantTName)
				}
			} else if _, ok := got["messageType"]; ok {
				t.Errorf("messageType should be absent, got %v", got["messageType"])
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Errorf("unmet mock expectations: %v", err)
			}
		})
	}
}

// TestCollectWowMailUnread_MinAgeHoursCutoff pins the load-bearing cutoff math: with a
// fixed now the cutoff bind is exactly now - minAgeHours*3600 (shared by the COUNT and
// the list), proving minAgeHours tightens the delivered-window bound. It also fixes the
// full bind order for a receiver filter: cutoff, receiver, then (list only) LIMIT.
func TestCollectWowMailUnread_MinAgeHoursCutoff(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	now := time.Unix(1_700_000_000, 0)
	cutoff := now.Unix() - 3*3600 // minAgeHours=3

	mock.ExpectExec(regexp.QuoteMeta("USE `acore_characters`")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT COUNT(*) FROM `mail` WHERE (checked & 1) = 0 AND deliver_time > 0 AND deliver_time <= ? AND receiver = ?")).
		WithArgs(cutoff, uint64(500)).
		WillReturnRows(sqlmock.NewRows([]string{"cnt"}).AddRow(int64(0)))
	mock.ExpectQuery(regexp.QuoteMeta("AND receiver = ? ORDER BY deliver_time ASC, id ASC LIMIT ?")).
		WithArgs(cutoff, uint64(500), int64(15)).
		WillReturnRows(sqlmock.NewRows(mailUnreadRowCols()))

	out, err := collectWowMailUnread(context.Background(), db, now, 15, "500", nil, 3)
	if err != nil {
		t.Fatalf("collectWowMailUnread: %v", err)
	}
	if ma, _ := out["minAgeHours"].(int); ma != 3 {
		t.Errorf("minAgeHours echo: %v want 3", out["minAgeHours"])
	}
	if r, _ := out["receiver"].(uint64); r != 500 {
		t.Errorf("receiver echo: %v want 500", out["receiver"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestCollectWowMailUnread_Empty — the empty-result case. mails must be a non-nil empty
// slice (callers expect arrays), returned 0, totalUnread 0, truncated false.
func TestCollectWowMailUnread_Empty(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	now := time.Unix(1_700_000_000, 0)
	mock.ExpectExec(regexp.QuoteMeta("USE `acore_characters`")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT COUNT(*) FROM `mail` WHERE (checked & 1) = 0")).
		WithArgs(now.Unix()).
		WillReturnRows(sqlmock.NewRows([]string{"cnt"}).AddRow(int64(0)))
	mock.ExpectQuery(regexp.QuoteMeta("ORDER BY deliver_time ASC, id ASC LIMIT ?")).
		WithArgs(now.Unix(), int64(15)).
		WillReturnRows(sqlmock.NewRows(mailUnreadRowCols()))

	out, err := collectWowMailUnread(context.Background(), db, now, 15, "", nil, 0)
	if err != nil {
		t.Fatalf("collectWowMailUnread: %v", err)
	}
	mails, ok := out["mails"].([]*mailUnreadRow)
	if !ok {
		t.Fatalf("mails type: %T want []*mailUnreadRow", out["mails"])
	}
	if len(mails) != 0 {
		t.Errorf("mails len: %d want 0", len(mails))
	}
	if r, _ := out["returned"].(int); r != 0 {
		t.Errorf("returned: %v want 0", out["returned"])
	}
	if tu, _ := out["totalUnread"].(int64); tu != 0 {
		t.Errorf("totalUnread: %v want 0", out["totalUnread"])
	}
	if tr, _ := out["truncated"].(bool); tr {
		t.Errorf("truncated: %v want false", out["truncated"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestCollectWowMailUnread_Golden drives the full per-row fold, exercising: ORDER-BY-trust
// (rows echoed in DB order, no Go re-sort), typeName mapping incl. unknown->type-99, age
// math (now - deliver_time, always >= 0 here since not-yet-delivered mail is filtered out),
// the expired flag (past expire flagged, future expire and the expire_time=0 guard not
// flagged), expireTimeISO omitted when expire_time=0, subject clip + NULL->"", hasItems,
// the checked BITMASK decode (returned bit / codPayment bit), the human gold strings, and
// the honest totalUnread (5) exceeding the returned list (3) -> truncated true.
func TestCollectWowMailUnread_Golden(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	now := time.Unix(1_700_000_000, 0)
	longSubject := strings.Repeat("A", 200) // > wowMailUnreadSubjectMaxBytes (120)

	mock.ExpectExec(regexp.QuoteMeta("USE `acore_characters`")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	// Honest total 5 > the 3 rows the list returns -> truncated.
	mock.ExpectQuery(regexp.QuoteMeta("SELECT COUNT(*) FROM `mail` WHERE (checked & 1) = 0")).
		WithArgs(now.Unix()).
		WillReturnRows(sqlmock.NewRows([]string{"cnt"}).AddRow(int64(5)))
	mock.ExpectQuery(regexp.QuoteMeta("ORDER BY deliver_time ASC, id ASC LIMIT ?")).
		WithArgs(now.Unix(), int64(3)).
		WillReturnRows(sqlmock.NewRows(mailUnreadRowCols()).
			// oldest: auction (2), has items, delivered 100000s ago, expired (now-50),
			// checked=0x02 (RETURNED bit set, READ clear -> an unread expired-auction
			// return), money 50000, receiver 100, sender 5.
			AddRow(int64(10), 2, int64(5), int64(100), "Auction expired: Thunderfury", 1, now.Unix()-100000, now.Unix()-50, int64(50000), int64(0), 0x02).
			// normal (0), no items, delivered 1h ago, expire_time=0 (never -> guarded from
			// expired), checked=0x00, cod 12000, NULL subject, receiver 200.
			AddRow(int64(20), 0, int64(0), int64(200), nil, 0, now.Unix()-3600, int64(0), int64(0), int64(12000), 0x00).
			// unknown type (99 -> type-99), has items, delivered 10m ago, future expire
			// (now+10000 -> not expired), checked=0x08 (COD_PAYMENT bit, READ clear),
			// oversized subject -> clipped, receiver 200.
			AddRow(int64(30), 99, int64(7), int64(200), longSubject, 1, now.Unix()-600, now.Unix()+10000, int64(1), int64(0), 0x08))

	out, err := collectWowMailUnread(context.Background(), db, now, 3, "", nil, 0)
	if err != nil {
		t.Fatalf("collectWowMailUnread: %v", err)
	}

	if r, _ := out["returned"].(int); r != 3 {
		t.Errorf("returned: %v want 3", out["returned"])
	}
	if tu, _ := out["totalUnread"].(int64); tu != 5 {
		t.Errorf("totalUnread: %v want 5", out["totalUnread"])
	}
	if tr, _ := out["truncated"].(bool); !tr {
		t.Errorf("truncated: %v want true (totalUnread 5 > returned 3)", out["truncated"])
	}
	if n, _ := out["topN"].(int); n != 3 {
		t.Errorf("topN: %v want 3", out["topN"])
	}
	mails, _ := out["mails"].([]*mailUnreadRow)
	if len(mails) != 3 {
		t.Fatalf("mails len: %d want 3", len(mails))
	}

	// mails[0]: oldest auction, expired, RETURNED bit, money 5g, age 27h 46m.
	m0 := mails[0]
	if m0.MailID != 10 || m0.MessageType != 2 || m0.TypeName != "auction" {
		t.Errorf("m0 identity: %+v want id=10 auction", m0)
	}
	if m0.SenderGuid != 5 || m0.ReceiverGuid != 100 {
		t.Errorf("m0 guids: sender=%d receiver=%d want 5/100", m0.SenderGuid, m0.ReceiverGuid)
	}
	if m0.Subject != "Auction expired: Thunderfury" || !m0.HasItems {
		t.Errorf("m0 subject/hasItems: %q / %v", m0.Subject, m0.HasItems)
	}
	if m0.AgeSeconds != 100000 || m0.AgeHuman != "27h 46m" {
		t.Errorf("m0 age: %d / %q want 100000 / 27h 46m", m0.AgeSeconds, m0.AgeHuman)
	}
	if !m0.Expired || m0.ExpireTimeISO == "" {
		t.Errorf("m0 expired=%v expireISO=%q want true / non-empty", m0.Expired, m0.ExpireTimeISO)
	}
	if m0.DeliverTimeISO == "" {
		t.Errorf("m0 deliverTimeISO should always be set (deliver_time>0)")
	}
	if m0.CheckedMask != 0x02 || !m0.Returned || m0.CodPayment {
		t.Errorf("m0 checked decode: mask=%d returned=%v cod=%v want 2/true/false", m0.CheckedMask, m0.Returned, m0.CodPayment)
	}
	if m0.MoneyCopper != 50000 || m0.MoneyGold != "5g" {
		t.Errorf("m0 money: %d / %q want 50000 / 5g", m0.MoneyCopper, m0.MoneyGold)
	}

	// mails[1]: normal, NULL subject -> "", expire_time=0 -> not expired + ISO omitted,
	// checked=0 -> no bits set, cod 1g20s, age 1h 0m.
	m1 := mails[1]
	if m1.MailID != 20 || m1.TypeName != "normal" {
		t.Errorf("m1 identity: %+v want id=20 normal", m1)
	}
	if m1.Subject != "" || m1.HasItems {
		t.Errorf("m1 subject/hasItems: %q / %v want ''/false", m1.Subject, m1.HasItems)
	}
	if m1.Expired {
		t.Errorf("m1 expired: %v want false (expire_time=0 guarded)", m1.Expired)
	}
	if m1.ExpireTimeISO != "" {
		t.Errorf("m1 expireTimeISO: %q want '' (expire_time=0)", m1.ExpireTimeISO)
	}
	if m1.CheckedMask != 0 || m1.Returned || m1.CodPayment {
		t.Errorf("m1 checked decode: mask=%d returned=%v cod=%v want 0/false/false", m1.CheckedMask, m1.Returned, m1.CodPayment)
	}
	if m1.CodCopper != 12000 || m1.CodGold != "1g20s" {
		t.Errorf("m1 cod: %d / %q want 12000 / 1g20s", m1.CodCopper, m1.CodGold)
	}
	if m1.AgeHuman != "1h 0m" {
		t.Errorf("m1 ageHuman: %q want 1h 0m", m1.AgeHuman)
	}

	// mails[2]: unknown type-99, oversized subject clipped to 120 bytes + "~", future
	// expire -> not expired, checked=0x08 -> COD_PAYMENT bit, age 10m.
	m2 := mails[2]
	if m2.MailID != 30 || m2.TypeName != "type-99" {
		t.Errorf("m2 identity: %+v want id=30 type-99", m2)
	}
	if len(m2.Subject) != wowMailUnreadSubjectMaxBytes+1 || !strings.HasSuffix(m2.Subject, "~") {
		t.Errorf("m2 subject clip: len=%d want len=%d + '~'", len(m2.Subject), wowMailUnreadSubjectMaxBytes+1)
	}
	if m2.Expired {
		t.Errorf("m2 expired: %v want false (future expire)", m2.Expired)
	}
	if m2.CheckedMask != 0x08 || m2.Returned || !m2.CodPayment {
		t.Errorf("m2 checked decode: mask=%d returned=%v cod=%v want 8/false/true", m2.CheckedMask, m2.Returned, m2.CodPayment)
	}
	if m2.AgeSeconds != 600 || m2.AgeHuman != "10m" {
		t.Errorf("m2 age: %d / %q want 600 / 10m", m2.AgeSeconds, m2.AgeHuman)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}
