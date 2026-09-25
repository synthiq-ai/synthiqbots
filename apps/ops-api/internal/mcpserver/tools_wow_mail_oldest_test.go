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

// mailOldestRowCols is the column set wow_mail_oldest scans, in order.
func mailOldestRowCols() []string {
	return []string{"id", "messageType", "sender", "receiver", "subject", "has_items", "deliver_time", "expire_time", "money", "cod"}
}

// TestWowMailOldest_DescriptionMentionsContext guards the description keywords —
// they shape which tool the agent reaches for when an operator asks "what mail has
// been stuck the longest?". Drop the oldest/deliver_time framing or the sibling
// cross-references and the agent falls back to a hand-written db_query.
func TestWowMailOldest_DescriptionMentionsContext(t *testing.T) {
	reg := NewRegistry()
	RegisterWowMailOldestTool(reg, DBDeps{})
	tool, ok := reg.Get("wow_mail_oldest")
	if !ok {
		t.Fatal("wow_mail_oldest not registered")
	}
	for _, kw := range []string{
		"acore_characters.mail", "ops_ro", "deliver_time", "wow_mail_summary",
		"wow_mail_item_breakdown", "wow_player_lookup", "messageType", "MailMessageType",
		"normal", "auction", "creature", "gameobject", "calendar", "type-<id>",
		"mod-ah-bot", "ah_market_", "expire_time", "expired", "ageSeconds", "ageHuman",
		"deliverTime", "sender", "subject", "idx_receiver", "receiver", "topN", "Read-only",
	} {
		if !strings.Contains(tool.Description, kw) {
			t.Errorf("description missing %q: %s", kw, tool.Description)
		}
	}
}

// TestWowMailOldest_ReadOnlyAnnotation pins readOnlyHint=true so a future
// copy-paste of a destructive sibling can't silently flip the gate.
func TestWowMailOldest_ReadOnlyAnnotation(t *testing.T) {
	reg := NewRegistry()
	RegisterWowMailOldestTool(reg, DBDeps{})
	tool, _ := reg.Get("wow_mail_oldest")
	if !strings.Contains(string(tool.Annotations), `"readOnlyHint":true`) {
		t.Errorf("annotations missing readOnlyHint=true: %s", tool.Annotations)
	}
}

func TestWowMailOldest_NoPoolReturnsError(t *testing.T) {
	reg := NewRegistry()
	RegisterWowMailOldestTool(reg, DBDeps{}) // QueryDB nil
	tool, _ := reg.Get("wow_mail_oldest")
	resp := tool.Handler(context.Background(), json.RawMessage(`{}`), "")
	got, _ := resp.(map[string]any)
	if msg, _ := got["error"].(string); !strings.Contains(msg, "ops_ro pool not configured") {
		t.Errorf("expected pool-not-configured error, got %v", got)
	}
}

// TestWowMailOldest_TopNClamps asserts the post-clamp topN echo (unset->default,
// zero/negative->default, oversized->max, in-range passthrough) and that the LIMIT
// bind matches the clamped value. There is no leading now bind (the now-relative
// math is done per row in Go), so the LIMIT is the only bind.
func TestWowMailOldest_TopNClamps(t *testing.T) {
	cases := []struct {
		name     string
		args     string
		wantTopN int
	}{
		{"unset uses default", `{}`, wowMailOldestDefaultTop},
		{"zero falls back to default", `{"topN":0}`, wowMailOldestDefaultTop},
		{"negative falls back to default", `{"topN":-7}`, wowMailOldestDefaultTop},
		{"oversized clamps to max", `{"topN":99999}`, wowMailOldestMaxTop},
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
			mock.ExpectQuery(regexp.QuoteMeta("ORDER BY deliver_time ASC, id ASC LIMIT ?")).
				WithArgs(int64(c.wantTopN)).
				WillReturnRows(sqlmock.NewRows(mailOldestRowCols()))

			reg := NewRegistry()
			RegisterWowMailOldestTool(reg, DBDeps{QueryDB: db})
			tool, _ := reg.Get("wow_mail_oldest")
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

// TestWowMailOldest_ReceiverRejectsBadGuid — a non-numeric / negative receiver is
// rejected BEFORE any query is issued (no SQL round trip on a bad filter).
func TestWowMailOldest_ReceiverRejectsBadGuid(t *testing.T) {
	for _, args := range []string{`{"receiver":"abc"}`, `{"receiver":-5}`} {
		db, mock, err := sqlmock.New()
		if err != nil {
			t.Fatal(err)
		}
		// No ExpectExec / ExpectQuery — the handler must bail before touching the DB.
		reg := NewRegistry()
		RegisterWowMailOldestTool(reg, DBDeps{QueryDB: db})
		tool, _ := reg.Get("wow_mail_oldest")
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

// TestWowMailOldest_MessageTypeRejectsNegative — a negative messageType is rejected
// BEFORE any query (no SQL round trip). A non-negative unknown type is NOT rejected
// (it rides through and matches no rows — honest empty), exercised in the Filters
// test; here we only pin the negative-reject guard.
func TestWowMailOldest_MessageTypeRejectsNegative(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	reg := NewRegistry()
	RegisterWowMailOldestTool(reg, DBDeps{QueryDB: db})
	tool, _ := reg.Get("wow_mail_oldest")
	resp := tool.Handler(context.Background(), json.RawMessage(`{"messageType":-1}`), "")
	got, _ := resp.(map[string]any)
	if msg, _ := got["error"].(string); !strings.Contains(msg, "messageType must be non-negative") {
		t.Errorf("expected messageType reject, got %v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("a query was issued on a negative messageType: %v", err)
	}
}

// TestWowMailOldest_Filters drives the handler with each filter combination and
// asserts the WHERE clause shape (filters ANDed onto the always-present
// deliver_time > 0), the bound args (filters first, then LIMIT — no now bind), and
// the echoed receiver/messageType/messageTypeName keys. The explicit messageType=0
// case proves the *int distinguishes "filter to normal mail" from "no type filter".
func TestWowMailOldest_Filters(t *testing.T) {
	cases := []struct {
		name      string
		args      string
		whereSub  string
		wantArgs  []driver.Value
		wantRecv  int64  // -1 = key absent
		wantType  int    // -1 = key absent
		wantTName string // "" = key absent
	}{
		{
			name:     "receiver only",
			args:     `{"receiver":777}`,
			whereSub: "WHERE deliver_time > 0 AND receiver = ? ORDER BY",
			wantArgs: []driver.Value{uint64(777), int64(wowMailOldestDefaultTop)},
			wantRecv: 777, wantType: -1,
		},
		{
			name:      "messageType only (auction)",
			args:      `{"messageType":2}`,
			whereSub:  "WHERE deliver_time > 0 AND messageType = ? ORDER BY",
			wantArgs:  []driver.Value{int64(2), int64(wowMailOldestDefaultTop)},
			wantRecv:  -1,
			wantType:  2,
			wantTName: "auction",
		},
		{
			name:      "messageType explicit zero (normal) is a real filter",
			args:      `{"messageType":0}`,
			whereSub:  "WHERE deliver_time > 0 AND messageType = ? ORDER BY",
			wantArgs:  []driver.Value{int64(0), int64(wowMailOldestDefaultTop)},
			wantRecv:  -1,
			wantType:  0,
			wantTName: "normal",
		},
		{
			name:      "both filters + topN",
			args:      `{"receiver":123,"messageType":3,"topN":5}`,
			whereSub:  "WHERE deliver_time > 0 AND receiver = ? AND messageType = ? ORDER BY",
			wantArgs:  []driver.Value{uint64(123), int64(3), int64(5)},
			wantRecv:  123,
			wantType:  3,
			wantTName: "creature",
		},
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
			mock.ExpectQuery(regexp.QuoteMeta(c.whereSub)).
				WithArgs(c.wantArgs...).
				WillReturnRows(sqlmock.NewRows(mailOldestRowCols()))

			reg := NewRegistry()
			RegisterWowMailOldestTool(reg, DBDeps{QueryDB: db})
			tool, _ := reg.Get("wow_mail_oldest")
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

// TestCollectWowMailOldest_Empty — the empty-result case. mails must be a non-nil
// empty slice (callers expect arrays) and returned 0.
func TestCollectWowMailOldest_Empty(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.ExpectExec(regexp.QuoteMeta("USE `acore_characters`")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta("ORDER BY deliver_time ASC, id ASC LIMIT ?")).
		WithArgs(int64(15)).
		WillReturnRows(sqlmock.NewRows(mailOldestRowCols()))

	out, err := collectWowMailOldest(context.Background(), db, time.Unix(1_700_000_000, 0), 15, "", nil)
	if err != nil {
		t.Fatalf("collectWowMailOldest: %v", err)
	}
	mails, ok := out["mails"].([]*mailOldestRow)
	if !ok {
		t.Fatalf("mails type: %T want []*mailOldestRow", out["mails"])
	}
	if len(mails) != 0 {
		t.Errorf("mails len: %d want 0", len(mails))
	}
	if r, _ := out["returned"].(int); r != 0 {
		t.Errorf("returned: %v want 0", out["returned"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestCollectWowMailOldest_Golden drives the full per-row fold across mail types and
// edge cases, exercising: ORDER-BY-trust (rows echoed in DB order, no Go re-sort),
// typeName mapping incl. unknown->type-99, age math (now - deliver_time), the
// expired flag (past expire flagged, future expire and the expire_time=0 guard not
// flagged), expireTimeISO omitted when expire_time=0, subject clip + NULL->"",
// hasItems, the human gold strings, and a future deliver_time -> negative age with
// ageHuman floored at "0m".
func TestCollectWowMailOldest_Golden(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	now := time.Unix(1_700_000_000, 0)
	longSubject := strings.Repeat("A", 200) // > wowMailOldestSubjectMaxBytes (120)

	mock.ExpectExec(regexp.QuoteMeta("USE `acore_characters`")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta("WHERE deliver_time > 0 ORDER BY deliver_time ASC, id ASC LIMIT ?")).
		WithArgs(int64(15)).
		WillReturnRows(sqlmock.NewRows(mailOldestRowCols()).
			// oldest: auction (2), has items, delivered 100000s ago, expired (now-50),
			// money 50000, receiver 100, sender 5.
			AddRow(int64(10), 2, int64(5), int64(100), "Auction successful: Thunderfury", 1, now.Unix()-100000, now.Unix()-50, int64(50000), int64(0)).
			// normal (0), no items, delivered 1h ago, expire_time=0 (never -> guarded
			// from expired), cod 12000, NULL subject, receiver 200.
			AddRow(int64(20), 0, int64(0), int64(200), nil, 0, now.Unix()-3600, int64(0), int64(0), int64(12000)).
			// unknown type (99 -> type-99), has items, delivered 10m ago, future expire
			// (now+10000 -> not expired), oversized subject -> clipped, receiver 200.
			AddRow(int64(30), 99, int64(7), int64(200), longSubject, 1, now.Unix()-600, now.Unix()+10000, int64(1), int64(0)).
			// FUTURE delivery (now+3600 -> negative age, ageHuman floors at 0m),
			// auction (2), expire_time=0, receiver 300.
			AddRow(int64(40), 2, int64(0), int64(300), "Pending sale", 0, now.Unix()+3600, int64(0), int64(0), int64(0)))

	out, err := collectWowMailOldest(context.Background(), db, now, 15, "", nil)
	if err != nil {
		t.Fatalf("collectWowMailOldest: %v", err)
	}

	if r, _ := out["returned"].(int); r != 4 {
		t.Errorf("returned: %v want 4", out["returned"])
	}
	if n, _ := out["topN"].(int); n != 15 {
		t.Errorf("topN: %v want 15", out["topN"])
	}
	mails, _ := out["mails"].([]*mailOldestRow)
	if len(mails) != 4 {
		t.Fatalf("mails len: %d want 4", len(mails))
	}

	// mails[0]: oldest auction, expired, money 5g, age 27h 46m.
	m0 := mails[0]
	if m0.MailID != 10 || m0.MessageType != 2 || m0.TypeName != "auction" {
		t.Errorf("m0 identity: %+v want id=10 auction", m0)
	}
	if m0.SenderGuid != 5 || m0.ReceiverGuid != 100 {
		t.Errorf("m0 guids: sender=%d receiver=%d want 5/100", m0.SenderGuid, m0.ReceiverGuid)
	}
	if m0.Subject != "Auction successful: Thunderfury" || !m0.HasItems {
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
	if m0.MoneyCopper != 50000 || m0.MoneyGold != "5g" {
		t.Errorf("m0 money: %d / %q want 50000 / 5g", m0.MoneyCopper, m0.MoneyGold)
	}

	// mails[1]: normal, NULL subject -> "", expire_time=0 -> not expired + ISO omitted,
	// cod 1g20s, age 1h 0m.
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
	if m1.AgeHuman != "1h 0m" {
		t.Errorf("m1 ageHuman: %q want 1h 0m", m1.AgeHuman)
	}
	if m1.CodCopper != 12000 || m1.CodGold != "1g20s" {
		t.Errorf("m1 cod: %d / %q want 12000 / 1g20s", m1.CodCopper, m1.CodGold)
	}

	// mails[2]: unknown type-99, oversized subject clipped to 120 bytes + "~",
	// future expire -> not expired.
	m2 := mails[2]
	if m2.MailID != 30 || m2.TypeName != "type-99" {
		t.Errorf("m2 identity: %+v want id=30 type-99", m2)
	}
	if len(m2.Subject) != wowMailOldestSubjectMaxBytes+1 || !strings.HasSuffix(m2.Subject, "~") {
		t.Errorf("m2 subject clip: len=%d suffix=%q want len=%d + '~'", len(m2.Subject), m2.Subject[len(m2.Subject)-1:], wowMailOldestSubjectMaxBytes+1)
	}
	if m2.Expired {
		t.Errorf("m2 expired: %v want false (future expire)", m2.Expired)
	}

	// mails[3]: future delivery -> negative age, ageHuman floors at 0m.
	m3 := mails[3]
	if m3.MailID != 40 {
		t.Errorf("m3 id: %d want 40", m3.MailID)
	}
	if m3.AgeSeconds != -3600 || m3.AgeHuman != "0m" {
		t.Errorf("m3 age: %d / %q want -3600 / 0m", m3.AgeSeconds, m3.AgeHuman)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}
