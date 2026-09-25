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

// mailItemBreakdownRowCols is the column set wow_mail_item_breakdown scans, in
// order.
func mailItemBreakdownRowCols() []string {
	return []string{"entry", "name", "Quality", "mailItems", "distinctMails", "distinctReceivers", "totalQuantity", "expiredItems"}
}

// TestWowMailItemBreakdown_DescriptionMentionsContext guards the keywords that
// steer the agent here vs wow_mail_summary (queue totals) or a hand-written
// db_query — the cross-DB join framing + item/attachment vocabulary is
// load-bearing for tool selection.
func TestWowMailItemBreakdown_DescriptionMentionsContext(t *testing.T) {
	reg := NewRegistry()
	RegisterWowMailItemBreakdownTool(reg, DBDeps{})
	tool, ok := reg.Get("wow_mail_item_breakdown")
	if !ok {
		t.Fatal("wow_mail_item_breakdown not registered")
	}
	for _, kw := range []string{
		"acore_characters.mail_items", "item_instance", "acore_world.item_template", "cross-DB",
		"wow_mail_summary", "ah_market_top_items", "ops_ro", "qualityName", "Heirloom",
		"mailItems", "distinctMails", "distinctReceivers", "totalQuantity", "expiredItems",
		"expire_time", "expiredOnly", "minQuality", "receiver", "topN", "Read-only",
	} {
		if !strings.Contains(tool.Description, kw) {
			t.Errorf("description missing %q: %s", kw, tool.Description)
		}
	}
}

// TestWowMailItemBreakdown_ReadOnlyAnnotation pins readOnlyHint=true so a future
// copy-paste of a destructive sibling can't silently flip the gate.
func TestWowMailItemBreakdown_ReadOnlyAnnotation(t *testing.T) {
	reg := NewRegistry()
	RegisterWowMailItemBreakdownTool(reg, DBDeps{})
	tool, _ := reg.Get("wow_mail_item_breakdown")
	if !strings.Contains(string(tool.Annotations), `"readOnlyHint":true`) {
		t.Errorf("annotations missing readOnlyHint=true: %s", tool.Annotations)
	}
}

func TestWowMailItemBreakdown_NoPoolReturnsError(t *testing.T) {
	reg := NewRegistry()
	RegisterWowMailItemBreakdownTool(reg, DBDeps{}) // QueryDB nil
	tool, _ := reg.Get("wow_mail_item_breakdown")
	resp := tool.Handler(context.Background(), json.RawMessage(`{}`), "")
	got, _ := resp.(map[string]any)
	if msg, _ := got["error"].(string); !strings.Contains(msg, "ops_ro pool not configured") {
		t.Errorf("expected pool-not-configured error, got %v", got)
	}
}

// TestWowMailItemBreakdown_TopNClamps asserts the post-clamp topN echo
// (unset->default, zero/negative->default, oversized->max, in-range passthrough)
// and that the LIMIT bind matches the clamped value. The leading now bind from
// the SELECT CASE is matched with AnyArg (the handler uses the real clock).
func TestWowMailItemBreakdown_TopNClamps(t *testing.T) {
	cases := []struct {
		name     string
		args     string
		wantTopN int
	}{
		{"unset uses default", `{}`, wowMailItemsDefaultTop},
		{"zero falls back to default", `{"topN":0}`, wowMailItemsDefaultTop},
		{"negative falls back to default", `{"topN":-7}`, wowMailItemsDefaultTop},
		{"oversized clamps to max", `{"topN":99999}`, wowMailItemsMaxTop},
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
			mock.ExpectQuery(regexp.QuoteMeta("ORDER BY mailItems DESC, it.entry ASC LIMIT ?")).
				WithArgs(sqlmock.AnyArg(), int64(c.wantTopN)).
				WillReturnRows(sqlmock.NewRows(mailItemBreakdownRowCols()))

			reg := NewRegistry()
			RegisterWowMailItemBreakdownTool(reg, DBDeps{QueryDB: db})
			tool, _ := reg.Get("wow_mail_item_breakdown")
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

// TestWowMailItemBreakdown_MinQualityRejected — out-of-range minQuality is
// rejected (no DB call) so a typo can't masquerade as an empty mailbox. QueryDB
// is non-nil to prove the reject fires AFTER the pool check but before any query.
func TestWowMailItemBreakdown_MinQualityRejected(t *testing.T) {
	for _, bad := range []string{`{"minQuality":-1}`, `{"minQuality":8}`, `{"minQuality":99}`} {
		db, mock, err := sqlmock.New()
		if err != nil {
			t.Fatal(err)
		}
		reg := NewRegistry()
		RegisterWowMailItemBreakdownTool(reg, DBDeps{QueryDB: db})
		tool, _ := reg.Get("wow_mail_item_breakdown")
		resp := tool.Handler(context.Background(), json.RawMessage(bad), "")
		got, _ := resp.(map[string]any)
		if msg, _ := got["error"].(string); !strings.Contains(msg, "minQuality out of range") {
			t.Errorf("args %s: expected out-of-range error, got %v", bad, got)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("args %s: no query should run on reject: %v", bad, err)
		}
		db.Close()
	}
}

// TestWowMailItemBreakdown_ReceiverRejectsBadGuid — a non-numeric / negative
// receiver is rejected BEFORE any query is issued (no SQL round trip on a bad
// filter).
func TestWowMailItemBreakdown_ReceiverRejectsBadGuid(t *testing.T) {
	for _, args := range []string{`{"receiver":"abc"}`, `{"receiver":-5}`} {
		db, mock, err := sqlmock.New()
		if err != nil {
			t.Fatal(err)
		}
		// No ExpectExec / ExpectQuery — the handler must bail before touching the DB.
		reg := NewRegistry()
		RegisterWowMailItemBreakdownTool(reg, DBDeps{QueryDB: db})
		tool, _ := reg.Get("wow_mail_item_breakdown")
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

// TestWowMailItemBreakdown_Filters drives the handler with each filter
// combination and asserts the WHERE clause shape, the bound args (now bind first,
// then filters, then LIMIT), and the echoed receiver/minQuality/expiredOnly keys.
// The now binds are matched with AnyArg (the handler uses the real clock).
func TestWowMailItemBreakdown_Filters(t *testing.T) {
	cases := []struct {
		name        string
		args        string
		whereSub    string
		wantArgs    []driver.Value
		wantRecv    int64 // -1 = key absent
		wantMinQ    int   // -1 = key absent
		wantExpired bool
	}{
		{
			name:     "receiver only",
			args:     `{"receiver":777}`,
			whereSub: "WHERE mi.receiver = ? GROUP BY",
			// now bind, receiver bind, LIMIT bind.
			wantArgs: []driver.Value{sqlmock.AnyArg(), uint64(777), int64(wowMailItemsDefaultTop)},
			wantRecv: 777, wantMinQ: -1,
		},
		{
			name:     "minQuality only",
			args:     `{"minQuality":4}`,
			whereSub: "WHERE it.Quality >= ? GROUP BY",
			wantArgs: []driver.Value{sqlmock.AnyArg(), int64(4), int64(wowMailItemsDefaultTop)},
			wantRecv: -1, wantMinQ: 4,
		},
		{
			name:     "expiredOnly only",
			args:     `{"expiredOnly":true}`,
			whereSub: "WHERE m.expire_time > 0 AND m.expire_time <= ? GROUP BY",
			// now bind (CASE), now bind (WHERE expired), LIMIT bind.
			wantArgs:    []driver.Value{sqlmock.AnyArg(), sqlmock.AnyArg(), int64(wowMailItemsDefaultTop)},
			wantRecv:    -1,
			wantMinQ:    -1,
			wantExpired: true,
		},
		{
			name:     "all three filters",
			args:     `{"receiver":123,"minQuality":2,"expiredOnly":true,"topN":5}`,
			whereSub: "WHERE mi.receiver = ? AND it.Quality >= ? AND m.expire_time > 0 AND m.expire_time <= ? GROUP BY",
			// now (CASE), receiver, minQuality, now (WHERE expired), LIMIT.
			wantArgs:    []driver.Value{sqlmock.AnyArg(), uint64(123), int64(2), sqlmock.AnyArg(), int64(5)},
			wantRecv:    123,
			wantMinQ:    2,
			wantExpired: true,
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
				WillReturnRows(sqlmock.NewRows(mailItemBreakdownRowCols()))

			reg := NewRegistry()
			RegisterWowMailItemBreakdownTool(reg, DBDeps{QueryDB: db})
			tool, _ := reg.Get("wow_mail_item_breakdown")
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
			if c.wantMinQ >= 0 {
				if q, _ := got["minQuality"].(int); q != c.wantMinQ {
					t.Errorf("minQuality: %v want %d", got["minQuality"], c.wantMinQ)
				}
			} else if _, ok := got["minQuality"]; ok {
				t.Errorf("minQuality should be absent, got %v", got["minQuality"])
			}
			if c.wantExpired {
				if e, _ := got["expiredOnly"].(bool); !e {
					t.Errorf("expiredOnly should be true, got %v", got["expiredOnly"])
				}
			} else if _, ok := got["expiredOnly"]; ok {
				t.Errorf("expiredOnly should be absent, got %v", got["expiredOnly"])
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Errorf("unmet mock expectations: %v", err)
			}
		})
	}
}

// TestCollectWowMailItemBreakdown_Golden folds three grouped rows and verifies
// the qualityName mapping, the scan field mapping (counts + distinct folds +
// summed quantity + expired count), distinctItems, the leading now bind + LIMIT
// bind, and that the handler trusts the SQL ORDER BY (no Go-side re-sort).
func TestCollectWowMailItemBreakdown_Golden(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	now := time.Unix(1_700_000_000, 0)
	mock.ExpectExec(regexp.QuoteMeta("USE `acore_characters`")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta("ORDER BY mailItems DESC, it.entry ASC LIMIT ?")).
		WithArgs(int64(1_700_000_000), int64(15)).
		WillReturnRows(sqlmock.NewRows(mailItemBreakdownRowCols()).
			// Common cloth: 8 attachments across 8 mails, 3 receivers, 160 total, 2 expired.
			AddRow(int64(21877), "Netherweave Cloth", 1, 8, 8, 3, int64(160), 2).
			// Epic badge: 3 attachments / 3 mails / 1 receiver, 3 total, all 3 expired.
			AddRow(int64(29434), "Badge of Justice", 4, 3, 3, 1, int64(3), 3).
			// Rare emblem: a single attachment, not expired.
			AddRow(int64(40752), "Emblem of Heroism", 3, 1, 1, 1, int64(1), 0))

	out, err := collectWowMailItemBreakdown(context.Background(), db, now, 15, "", nil, false)
	if err != nil {
		t.Fatalf("collectWowMailItemBreakdown: %v", err)
	}
	if di, _ := out["distinctItems"].(int); di != 3 {
		t.Errorf("distinctItems: %v want 3", out["distinctItems"])
	}
	for _, k := range []string{"receiver", "minQuality", "expiredOnly"} {
		if _, ok := out[k]; ok {
			t.Errorf("%s should be absent with no filter, got %v", k, out[k])
		}
	}
	items, ok := out["items"].([]*mailItemBreakdownRow)
	if !ok || len(items) != 3 {
		t.Fatalf("items: %T len %d want []*mailItemBreakdownRow len 3", out["items"], len(items))
	}

	it0 := items[0]
	if it0.ItemEntry != 21877 || it0.ItemName != "Netherweave Cloth" || it0.Quality != 1 || it0.QualityName != "Common" {
		t.Errorf("items[0] identity: %+v", it0)
	}
	if it0.MailItems != 8 || it0.DistinctMails != 8 || it0.DistinctReceivers != 3 || it0.TotalQuantity != 160 || it0.ExpiredItems != 2 {
		t.Errorf("items[0] folds: %+v", it0)
	}

	it1 := items[1]
	if it1.QualityName != "Epic" || it1.MailItems != 3 || it1.DistinctReceivers != 1 || it1.ExpiredItems != 3 {
		t.Errorf("items[1]: %+v want Epic/3 attachments/1 receiver/3 expired", it1)
	}

	it2 := items[2]
	if it2.QualityName != "Rare" || it2.MailItems != 1 || it2.ExpiredItems != 0 {
		t.Errorf("items[2]: %+v want Rare/1 attachment/0 expired", it2)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestCollectWowMailItemBreakdown_Empty — an empty mailbox yields a non-nil empty
// items slice (callers expect an array) and distinctItems 0.
func TestCollectWowMailItemBreakdown_Empty(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.ExpectExec(regexp.QuoteMeta("USE `acore_characters`")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta("ORDER BY mailItems DESC, it.entry ASC LIMIT ?")).
		WithArgs(int64(1_700_000_000), int64(15)).
		WillReturnRows(sqlmock.NewRows(mailItemBreakdownRowCols()))

	out, err := collectWowMailItemBreakdown(context.Background(), db, time.Unix(1_700_000_000, 0), 15, "", nil, false)
	if err != nil {
		t.Fatalf("collectWowMailItemBreakdown: %v", err)
	}
	items, ok := out["items"].([]*mailItemBreakdownRow)
	if !ok {
		t.Fatalf("items type: %T want []*mailItemBreakdownRow", out["items"])
	}
	if items == nil || len(items) != 0 {
		t.Errorf("items: %v want non-nil empty slice", items)
	}
	if di, _ := out["distinctItems"].(int); di != 0 {
		t.Errorf("distinctItems: %v want 0", out["distinctItems"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}
