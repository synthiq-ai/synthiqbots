package mcpserver

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

// mailRowCols is the column set wow_mail_summary scans, in order.
func mailRowCols() []string {
	return []string{"messageType", "has_items", "money", "cod", "expire_time", "receiver"}
}

// TestWowMailSummary_DescriptionMentionsContext guards the description keywords
// — they shape which tool the agent reaches for when an operator asks "is mail
// backing up?" / "how much gold is in the mail?". Drop the mail/queue framing and
// the agent falls back to a hand-written db_query.
func TestWowMailSummary_DescriptionMentionsContext(t *testing.T) {
	reg := NewRegistry()
	RegisterWowMailSummaryTool(reg, DBDeps{})
	tool, ok := reg.Get("wow_mail_summary")
	if !ok {
		t.Fatal("wow_mail_summary not registered")
	}
	for _, kw := range []string{
		"acore_characters.mail", "ops_ro", "byType", "messageType", "MailMessageType",
		"normal", "auction", "creature", "gameobject", "calendar",
		"expiredCount", "expire_time", "has_items", "COD", "distinctReceivers",
		"topReceivers", "wow_player_lookup", "mod-ah-bot", "ah_market_", "receiver",
		"Read-only", "truncated",
	} {
		if !strings.Contains(tool.Description, kw) {
			t.Errorf("description missing %q: %s", kw, tool.Description)
		}
	}
}

// TestWowMailSummary_ReadOnlyAnnotation pins readOnlyHint=true so a future
// copy-paste of a destructive sibling can't silently flip the gate.
func TestWowMailSummary_ReadOnlyAnnotation(t *testing.T) {
	reg := NewRegistry()
	RegisterWowMailSummaryTool(reg, DBDeps{})
	tool, _ := reg.Get("wow_mail_summary")
	if !strings.Contains(string(tool.Annotations), `"readOnlyHint":true`) {
		t.Errorf("annotations missing readOnlyHint=true: %s", tool.Annotations)
	}
}

func TestWowMailSummary_NoPoolReturnsError(t *testing.T) {
	reg := NewRegistry()
	RegisterWowMailSummaryTool(reg, DBDeps{}) // QueryDB nil
	tool, _ := reg.Get("wow_mail_summary")
	resp := tool.Handler(context.Background(), json.RawMessage(`{}`), "")
	got, _ := resp.(map[string]any)
	if msg, _ := got["error"].(string); !strings.Contains(msg, "ops_ro pool not configured") {
		t.Errorf("expected pool-not-configured error, got %v", got)
	}
}

// TestWowMailSummary_TopReceiversPresence verifies the *int unset-vs-explicit-0
// distinction: unset (and any positive value) yields the topReceivers list;
// explicit 0 or a negative (clamped to 0) omits the key. The LIMIT bind is always
// scanCap+1 regardless. distinctReceivers stays present in all cases.
func TestWowMailSummary_TopReceiversPresence(t *testing.T) {
	cases := []struct {
		name    string
		args    string
		present bool
	}{
		{"unset -> default 10 -> present", `{}`, true},
		{"positive -> present", `{"topReceivers":5}`, true},
		{"explicit zero -> omitted", `{"topReceivers":0}`, false},
		{"negative clamps to zero -> omitted", `{"topReceivers":-3}`, false},
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
			mock.ExpectQuery(regexp.QuoteMeta("FROM `mail` LIMIT ?")).
				WithArgs(int64(wowMailSummaryScanCap + 1)).
				WillReturnRows(sqlmock.NewRows(mailRowCols()))

			reg := NewRegistry()
			RegisterWowMailSummaryTool(reg, DBDeps{QueryDB: db})
			tool, _ := reg.Get("wow_mail_summary")
			resp := tool.Handler(context.Background(), json.RawMessage(c.args), "")
			got, _ := resp.(map[string]any)
			if got["error"] != nil {
				t.Fatalf("unexpected error: %v", got)
			}
			if _, ok := got["topReceivers"]; ok != c.present {
				t.Errorf("topReceivers present=%v want %v", ok, c.present)
			}
			// distinctReceivers is always reported, even when the list is omitted.
			totals, _ := got["totals"].(map[string]any)
			if _, ok := totals["distinctReceivers"]; !ok {
				t.Errorf("totals.distinctReceivers should always be present, got %v", totals)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Errorf("unmet mock expectations: %v", err)
			}
		})
	}
}

// TestWowMailSummary_ReceiverRejectsBadGuid — a non-numeric / negative receiver is
// rejected BEFORE any query is issued (no SQL round trip on a bad filter).
func TestWowMailSummary_ReceiverRejectsBadGuid(t *testing.T) {
	for _, args := range []string{`{"receiver":"abc"}`, `{"receiver":-5}`} {
		db, mock, err := sqlmock.New()
		if err != nil {
			t.Fatal(err)
		}
		// No ExpectExec / ExpectQuery — the handler must bail before touching the DB.
		reg := NewRegistry()
		RegisterWowMailSummaryTool(reg, DBDeps{QueryDB: db})
		tool, _ := reg.Get("wow_mail_summary")
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

// TestWowMailSummary_ReceiverFilterSqlShape — a valid receiver appends
// `WHERE receiver = ?` with the guid bound before the scanCap+1 LIMIT, and the
// guid is echoed in the response.
func TestWowMailSummary_ReceiverFilterSqlShape(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.ExpectExec(regexp.QuoteMeta("USE `acore_characters`")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta("FROM `mail` WHERE receiver = ? LIMIT ?")).
		WithArgs(uint64(123), int64(wowMailSummaryScanCap+1)).
		WillReturnRows(sqlmock.NewRows(mailRowCols()))

	reg := NewRegistry()
	RegisterWowMailSummaryTool(reg, DBDeps{QueryDB: db})
	tool, _ := reg.Get("wow_mail_summary")
	resp := tool.Handler(context.Background(), json.RawMessage(`{"receiver":123}`), "")
	got, _ := resp.(map[string]any)
	if got["error"] != nil {
		t.Fatalf("unexpected error: %v", got)
	}
	if r, _ := got["receiver"].(uint64); r != 123 {
		t.Errorf("receiver echo: %v want 123", got["receiver"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestCollectMailSummary_Empty — the empty-mailbox case. byType must be a non-nil
// empty slice (callers expect arrays), totals all zero, distinctReceivers 0, and
// topReceivers a non-nil empty slice when requested.
func TestCollectMailSummary_Empty(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.ExpectExec(regexp.QuoteMeta("USE `acore_characters`")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta("FROM `mail` LIMIT ?")).
		WithArgs(int64(wowMailSummaryScanCap + 1)).
		WillReturnRows(sqlmock.NewRows(mailRowCols()))

	out, err := collectMailSummary(context.Background(), db, time.Unix(1_700_000_000, 0), "", 10)
	if err != nil {
		t.Fatalf("collectMailSummary: %v", err)
	}
	bt, ok := out["byType"].([]*mailTypeBucket)
	if !ok {
		t.Fatalf("byType type: %T want []*mailTypeBucket", out["byType"])
	}
	if len(bt) != 0 {
		t.Errorf("byType len: %d want 0", len(bt))
	}
	tr, ok := out["topReceivers"].([]*mailReceiverBucket)
	if !ok {
		t.Fatalf("topReceivers type: %T want []*mailReceiverBucket", out["topReceivers"])
	}
	if len(tr) != 0 {
		t.Errorf("topReceivers len: %d want 0", len(tr))
	}
	totals, _ := out["totals"].(map[string]any)
	if m, _ := totals["totalMail"].(int); m != 0 {
		t.Errorf("totals.totalMail: %v want 0", totals["totalMail"])
	}
	if d, _ := totals["distinctReceivers"].(int); d != 0 {
		t.Errorf("totals.distinctReceivers: %v want 0", totals["distinctReceivers"])
	}
	if g, _ := totals["moneyInTransitGold"].(string); g != "0c" {
		t.Errorf("totals.moneyInTransitGold: %q want 0c", g)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestCollectMailSummary_TopReceiversOmittedWhenZero — topReceivers=0 omits the
// list key but distinctReceivers is still computed and reported.
func TestCollectMailSummary_TopReceiversOmittedWhenZero(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.ExpectExec(regexp.QuoteMeta("USE `acore_characters`")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta("FROM `mail` LIMIT ?")).
		WithArgs(int64(wowMailSummaryScanCap + 1)).
		WillReturnRows(sqlmock.NewRows(mailRowCols()).
			AddRow(2, 1, int64(9000), int64(0), int64(1_700_000_500), int64(100)).
			AddRow(0, 0, int64(0), int64(0), int64(1_700_000_500), int64(200)))

	out, err := collectMailSummary(context.Background(), db, time.Unix(1_700_000_000, 0), "", 0)
	if err != nil {
		t.Fatalf("collectMailSummary: %v", err)
	}
	if _, ok := out["topReceivers"]; ok {
		t.Errorf("topReceivers should be omitted when topReceivers=0, got %v", out["topReceivers"])
	}
	totals, _ := out["totals"].(map[string]any)
	if d, _ := totals["distinctReceivers"].(int); d != 2 {
		t.Errorf("distinctReceivers: %v want 2 (still computed when list omitted)", totals["distinctReceivers"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestCollectMailSummary_Golden drives the full fold across all known types plus
// an unknown one, exercising: byType count-desc + messageType-asc tiebreak,
// per-type counts, the totals fold, expired math (past counts, future doesn't,
// time==0 guarded), has_items flags, money/cod sums, topReceivers count-desc +
// guid-asc tiebreaker, distinctReceivers, and the human gold strings.
func TestCollectMailSummary_Golden(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	now := time.Unix(1_700_000_000, 0)

	mock.ExpectExec(regexp.QuoteMeta("USE `acore_characters`")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta("FROM `mail` LIMIT ?")).
		WithArgs(int64(wowMailSummaryScanCap + 1)).
		WillReturnRows(sqlmock.NewRows(mailRowCols()).
			// auction (2): has items, money 50000, not expired (now+3600), receiver 100.
			AddRow(2, 1, int64(50000), int64(0), now.Unix()+3600, int64(100)).
			// auction (2): no items, COD 12000, expired (now-100), receiver 100.
			AddRow(2, 0, int64(0), int64(12000), now.Unix()-100, int64(100)).
			// normal (0): has items, money 9000, not expired (now+10000), receiver 200.
			AddRow(0, 1, int64(9000), int64(0), now.Unix()+10000, int64(200)).
			// normal (0): no items, malformed time==0 -> expired guard skips, receiver 100.
			AddRow(0, 0, int64(0), int64(0), int64(0), int64(100)).
			// creature (3): has items, money 100, expired (now-5), receiver 300.
			AddRow(3, 1, int64(100), int64(0), now.Unix()-5, int64(300)).
			// unknown (99 -> type-99): no items, money 1, expired (now-1), receiver 200.
			AddRow(99, 0, int64(1), int64(0), now.Unix()-1, int64(200)))

	out, err := collectMailSummary(context.Background(), db, now, "", 10)
	if err != nil {
		t.Fatalf("collectMailSummary: %v", err)
	}

	if s, _ := out["scanned"].(int); s != 6 {
		t.Errorf("scanned: %v want 6", out["scanned"])
	}
	if tr, _ := out["truncated"].(bool); tr {
		t.Errorf("truncated: %v want false", out["truncated"])
	}

	// byType order: count-desc then messageType-asc. counts: normal(0)=2,
	// auction(2)=2, creature(3)=1, type-99(99)=1 -> [0, 2, 3, 99].
	bt, _ := out["byType"].([]*mailTypeBucket)
	if len(bt) != 4 {
		t.Fatalf("byType len: %d want 4", len(bt))
	}
	if bt[0].MessageType != 0 || bt[0].TypeName != "normal" || bt[0].Count != 2 {
		t.Errorf("bt[0]: %+v want normal/0 count=2", bt[0])
	}
	if bt[1].MessageType != 2 || bt[1].TypeName != "auction" || bt[1].Count != 2 {
		t.Errorf("bt[1]: %+v want auction/2 count=2", bt[1])
	}
	if bt[2].MessageType != 3 || bt[2].TypeName != "creature" || bt[2].Count != 1 {
		t.Errorf("bt[2]: %+v want creature/3 count=1", bt[2])
	}
	if bt[3].MessageType != 99 || bt[3].TypeName != "type-99" || bt[3].Count != 1 {
		t.Errorf("bt[3]: %+v want type-99/99 count=1", bt[3])
	}

	// auction bucket: 2 mails, 1 with items, 1 expired, money 50000, cod 12000.
	au := bt[1]
	if au.WithItems != 1 || au.ExpiredCount != 1 || au.MoneyCopper != 50000 || au.CodCopper != 12000 {
		t.Errorf("auction bucket: %+v", au)
	}
	if au.MoneyGold != "5g" || au.CodGold != "1g20s" {
		t.Errorf("auction gold: money=%q cod=%q want 5g / 1g20s", au.MoneyGold, au.CodGold)
	}
	// normal bucket: malformed time==0 row excluded from expired (0 expired).
	nm := bt[0]
	if nm.ExpiredCount != 0 {
		t.Errorf("normal expiredCount: %d want 0 (time==0 row guarded)", nm.ExpiredCount)
	}

	totals, _ := out["totals"].(map[string]any)
	if m, _ := totals["totalMail"].(int); m != 6 {
		t.Errorf("totals.totalMail: %v want 6", totals["totalMail"])
	}
	if wi, _ := totals["withItems"].(int); wi != 3 {
		t.Errorf("totals.withItems: %v want 3", totals["withItems"])
	}
	if ec, _ := totals["expiredCount"].(int); ec != 3 {
		t.Errorf("totals.expiredCount: %v want 3 (auction-B + creature + type99; time==0 guarded)", totals["expiredCount"])
	}
	if dr, _ := totals["distinctReceivers"].(int); dr != 3 {
		t.Errorf("totals.distinctReceivers: %v want 3 (100,200,300)", totals["distinctReceivers"])
	}
	if mc, _ := totals["moneyInTransitCopper"].(int64); mc != 59101 {
		t.Errorf("totals.moneyInTransitCopper: %v want 59101", totals["moneyInTransitCopper"])
	}
	if cc, _ := totals["codInTransitCopper"].(int64); cc != 12000 {
		t.Errorf("totals.codInTransitCopper: %v want 12000", totals["codInTransitCopper"])
	}
	if g, _ := totals["moneyInTransitGold"].(string); g != "5g91s1c" {
		t.Errorf("totals.moneyInTransitGold: %q want 5g91s1c", g)
	}
	if g, _ := totals["codInTransitGold"].(string); g != "1g20s" {
		t.Errorf("totals.codInTransitGold: %q want 1g20s", g)
	}

	// topReceivers: 100 (3 mails) > 200 (2) > 300 (1).
	tr, _ := out["topReceivers"].([]*mailReceiverBucket)
	if len(tr) != 3 {
		t.Fatalf("topReceivers len: %d want 3", len(tr))
	}
	if tr[0].ReceiverGuid != 100 || tr[0].MailCount != 3 || tr[0].ExpiredCount != 1 || tr[0].MoneyCopper != 50000 || tr[0].CodCopper != 12000 {
		t.Errorf("topReceivers[0]: %+v want guid=100 mails=3 expired=1 money=50000 cod=12000", tr[0])
	}
	if tr[1].ReceiverGuid != 200 || tr[1].MailCount != 2 || tr[1].MoneyCopper != 9001 {
		t.Errorf("topReceivers[1]: %+v want guid=200 mails=2 money=9001", tr[1])
	}
	if tr[1].MoneyGold != "90s1c" {
		t.Errorf("topReceivers[1].moneyGold: %q want 90s1c", tr[1].MoneyGold)
	}
	if tr[2].ReceiverGuid != 300 || tr[2].MailCount != 1 {
		t.Errorf("topReceivers[2]: %+v want guid=300 mails=1", tr[2])
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestCollectMailSummary_ReceiverGuidTiebreak — equal mail counts break to
// receiverGuid asc, defending against Go map-iteration flake.
func TestCollectMailSummary_ReceiverGuidTiebreak(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.ExpectExec(regexp.QuoteMeta("USE `acore_characters`")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta("FROM `mail` LIMIT ?")).
		WithArgs(int64(wowMailSummaryScanCap + 1)).
		WillReturnRows(sqlmock.NewRows(mailRowCols()).
			AddRow(0, 0, int64(0), int64(0), int64(0), int64(30)).
			AddRow(0, 0, int64(0), int64(0), int64(0), int64(10)))

	out, err := collectMailSummary(context.Background(), db, time.Unix(1_700_000_000, 0), "", 10)
	if err != nil {
		t.Fatalf("collectMailSummary: %v", err)
	}
	tr, _ := out["topReceivers"].([]*mailReceiverBucket)
	if len(tr) != 2 {
		t.Fatalf("topReceivers len: %d want 2", len(tr))
	}
	if tr[0].ReceiverGuid != 10 || tr[1].ReceiverGuid != 30 {
		t.Errorf("tiebreak order: got [%d, %d] want [10, 30] (guid asc on equal counts)", tr[0].ReceiverGuid, tr[1].ReceiverGuid)
	}
}

func TestMailTypeName(t *testing.T) {
	cases := map[int]string{
		0:  "normal",
		2:  "auction",
		3:  "creature",
		4:  "gameobject",
		5:  "calendar",
		1:  "type-1", // no enum value 1 — surfaces rather than vanishing
		99: "type-99",
	}
	for in, want := range cases {
		if got := mailTypeName(in); got != want {
			t.Errorf("mailTypeName(%d) = %q want %q", in, got, want)
		}
	}
}
