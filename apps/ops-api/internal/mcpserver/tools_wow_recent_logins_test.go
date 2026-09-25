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

// TestWowRecentLogins_DescriptionMentionsContext guards the description
// keywords — same intent as the sibling wow_* tests: the description shapes
// which tool the agent picks when an operator asks "who logged in recently?"
// or "did $user come back after the outage?". Drop the composite framing and
// the agent falls back to chained db_query calls.
func TestWowRecentLogins_DescriptionMentionsContext(t *testing.T) {
	reg := NewRegistry()
	RegisterWowRecentLoginsTool(reg, DBDeps{})
	tool, ok := reg.Get("wow_recent_logins")
	if !ok {
		t.Fatal("wow_recent_logins not registered")
	}
	for _, kw := range []string{
		"recent", "Composite", "last_login", "accounts", "sinceHours",
		"excludeBots", "limit", "wow_online_players", "failedLogins",
		"RNDBOT", "ops_ro",
	} {
		if !strings.Contains(tool.Description, kw) {
			t.Errorf("description missing %q: %s", kw, tool.Description)
		}
	}
}

// TestWowRecentLogins_ReadOnlyAnnotation pins the tool to readOnlyHint=true so
// a future copy-paste of a destructive sibling can't silently flip the gate.
func TestWowRecentLogins_ReadOnlyAnnotation(t *testing.T) {
	reg := NewRegistry()
	RegisterWowRecentLoginsTool(reg, DBDeps{})
	tool, _ := reg.Get("wow_recent_logins")
	if !strings.Contains(string(tool.Annotations), `"readOnlyHint":true`) {
		t.Errorf("annotations missing readOnlyHint=true: %s", tool.Annotations)
	}
}

func TestWowRecentLogins_NoPoolReturnsError(t *testing.T) {
	reg := NewRegistry()
	RegisterWowRecentLoginsTool(reg, DBDeps{}) // QueryDB nil
	tool, _ := reg.Get("wow_recent_logins")
	resp := tool.Handler(context.Background(), json.RawMessage(`{}`), "")
	got, _ := resp.(map[string]any)
	if msg, _ := got["error"].(string); !strings.Contains(msg, "ops_ro pool not configured") {
		t.Errorf("expected pool-not-configured error, got %v", got)
	}
}

// TestWowRecentLogins_LimitClamps verifies the LIMIT placeholder matches what
// we expect (default + max), protecting against a tool that silently runs a
// 5000-row SELECT because the caller passed limit=99999.
func TestWowRecentLogins_LimitClamps(t *testing.T) {
	cases := []struct {
		name      string
		args      string
		wantLimit int64
	}{
		{"unset uses default", `{}`, wowRecentLoginsDefaultLimit},
		{"negative falls back to default", `{"limit":-5}`, wowRecentLoginsDefaultLimit},
		{"oversized clamps to max", `{"limit":99999}`, wowRecentLoginsMaxLimit},
		{"in-range passes through", `{"limit":42}`, 42},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			mock.ExpectExec(regexp.QuoteMeta("USE `acore_auth`")).
				WillReturnResult(sqlmock.NewResult(0, 0))
			mock.ExpectQuery(regexp.QuoteMeta("FROM `account` WHERE last_login IS NOT NULL")).
				WithArgs(c.wantLimit).
				WillReturnRows(sqlmock.NewRows([]string{
					"id", "username", "last_ip", "last_login", "online", "joindate", "failed_logins", "expansion",
				}))

			reg := NewRegistry()
			RegisterWowRecentLoginsTool(reg, DBDeps{QueryDB: db})
			tool, _ := reg.Get("wow_recent_logins")
			resp := tool.Handler(context.Background(), json.RawMessage(c.args), "")
			if got, _ := resp.(map[string]any); got["error"] != nil {
				t.Fatalf("unexpected error: %v", got)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Errorf("unmet mock expectations: %v", err)
			}
		})
	}
}

// TestWowRecentLogins_SinceHoursClamps ensures the sinceHours bound stays
// inside [0, 168]. Negative falls back to 0 (unbounded), over-cap clamps to 168.
// Verifies the response echoes the post-clamp value so the caller can see
// that their 9999 became 168 without guessing.
func TestWowRecentLogins_SinceHoursClamps(t *testing.T) {
	cases := []struct {
		name           string
		args           string
		wantSinceHours int
		wantWhereHrs   bool // true if "AND last_login >= ?" should appear
	}{
		{"unset = unbounded", `{}`, 0, false},
		{"negative falls back to 0", `{"sinceHours":-5}`, 0, false},
		{"in-range passes through", `{"sinceHours":24}`, 24, true},
		{"oversized clamps to 168", `{"sinceHours":9999}`, wowRecentLoginsMaxSinceHrs, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			mock.ExpectExec(regexp.QuoteMeta("USE `acore_auth`")).
				WillReturnResult(sqlmock.NewResult(0, 0))

			// Match args adaptively based on which filters apply.
			cols := []string{"id", "username", "last_ip", "last_login", "online", "joindate", "failed_logins", "expansion"}
			if c.wantWhereHrs {
				mock.ExpectQuery(regexp.QuoteMeta("FROM `account` WHERE last_login IS NOT NULL AND last_login >= ?")).
					WithArgs(sqlmock.AnyArg(), int64(wowRecentLoginsDefaultLimit)).
					WillReturnRows(sqlmock.NewRows(cols))
			} else {
				mock.ExpectQuery(regexp.QuoteMeta("FROM `account` WHERE last_login IS NOT NULL")).
					WithArgs(int64(wowRecentLoginsDefaultLimit)).
					WillReturnRows(sqlmock.NewRows(cols))
			}

			reg := NewRegistry()
			RegisterWowRecentLoginsTool(reg, DBDeps{QueryDB: db})
			tool, _ := reg.Get("wow_recent_logins")
			resp := tool.Handler(context.Background(), json.RawMessage(c.args), "")
			got, _ := resp.(map[string]any)
			if got["error"] != nil {
				t.Fatalf("unexpected error: %v", got)
			}
			if sh, _ := got["sinceHours"].(int); sh != c.wantSinceHours {
				t.Errorf("response sinceHours: %v want %d", got["sinceHours"], c.wantSinceHours)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Errorf("unmet mock expectations: %v", err)
			}
		})
	}
}

// TestCollectRecentLogins_Empty — the no-recent-logins case. Important
// contract because the alternative (skipping the response shape entirely)
// would break any caller expecting accounts to be an array.
func TestCollectRecentLogins_Empty(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.ExpectExec(regexp.QuoteMeta("USE `acore_auth`")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta("FROM `account` WHERE last_login IS NOT NULL")).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "username", "last_ip", "last_login", "online", "joindate", "failed_logins", "expansion",
		}))

	out, err := collectRecentLogins(context.Background(), db, 0, false, 50)
	if err != nil {
		t.Fatalf("collectRecentLogins: %v", err)
	}
	if cnt, _ := out["count"].(int); cnt != 0 {
		t.Errorf("count: %v want 0", out["count"])
	}
	accts, ok := out["accounts"].([]*recentLogin)
	if !ok {
		t.Fatalf("accounts type: %T want []*recentLogin", out["accounts"])
	}
	if len(accts) != 0 {
		t.Errorf("accounts len: %d want 0", len(accts))
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestCollectRecentLogins_Golden drives the full happy path with three accounts
// — one GM (all-realms row), one plain player (no account_access row), one
// account with failed logins (brute-force noise signal). Exercises:
//   - sort order (handler doesn't re-sort, SQL is the ORDER BY)
//   - account_access precedence (RealmID = -1 wins over per-realm)
//   - online flag, failed_logins, expansion surfacing
//   - GM annotation defaults to 0 for accounts without a row
func TestCollectRecentLogins_Golden(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectExec(regexp.QuoteMeta("USE `acore_auth`")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta("FROM `account` WHERE last_login IS NOT NULL ORDER BY last_login DESC LIMIT ?")).
		WithArgs(int64(50)).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "username", "last_ip", "last_login", "online", "joindate", "failed_logins", "expansion",
		}).
			AddRow(int64(1001), "OPERATOR", "192.168.100.14", "2026-06-01 14:00:00", 1, "2025-01-15 12:00:00", 0, 2).
			AddRow(int64(1002), "PLEB", "192.168.100.12", "2026-06-01 13:45:00", 0, "2025-02-20 09:30:00", 0, 2).
			AddRow(int64(1003), "TARGET", "10.0.0.50", "2026-06-01 13:00:00", 0, "2025-03-10 11:15:00", 17, 2))

	// account_access: OPERATOR has both an all-realms gm-3 row AND a per-realm
	// gm-1 row. The all-realms row should win. PLEB has no row at all.
	// TARGET has a per-realm gm-1 row (the moderation account on the targeted realm).
	mock.ExpectQuery(regexp.QuoteMeta("FROM `account_access` WHERE id IN (?,?,?)")).
		WithArgs(int64(1001), int64(1002), int64(1003)).
		WillReturnRows(sqlmock.NewRows([]string{"id", "gmlevel", "RealmID"}).
			AddRow(int64(1001), 3, -1).
			AddRow(int64(1001), 1, 1).
			AddRow(int64(1003), 1, 1))

	out, err := collectRecentLogins(context.Background(), db, 0, false, 50)
	if err != nil {
		t.Fatalf("collectRecentLogins: %v", err)
	}
	if cnt, _ := out["count"].(int); cnt != 3 {
		t.Errorf("count: %v want 3", out["count"])
	}
	if excl, _ := out["excludeBots"].(bool); excl {
		t.Errorf("excludeBots: %v want false", out["excludeBots"])
	}
	accts, _ := out["accounts"].([]*recentLogin)
	if len(accts) != 3 {
		t.Fatalf("accounts len: %d want 3", len(accts))
	}

	// First row: Slayo, GM 3 (all-realms wins), online, no failed logins.
	a1 := accts[0]
	if a1.Username != "OPERATOR" || a1.AccountID != 1001 {
		t.Errorf("a1 identity: %+v", a1)
	}
	if a1.LastLoginISO != "2026-06-01 14:00:00" {
		t.Errorf("a1 last_login: %q", a1.LastLoginISO)
	}
	if !a1.Online {
		t.Errorf("a1 online: %v want true", a1.Online)
	}
	if a1.GMLevel != 3 {
		t.Errorf("a1 gm: %d want 3 (all-realms row should win)", a1.GMLevel)
	}
	if a1.FailedLogins != 0 || a1.Expansion != 2 {
		t.Errorf("a1 misc: failedLogins=%d expansion=%d", a1.FailedLogins, a1.Expansion)
	}

	// Second row: Pleb, no account_access row, offline.
	a2 := accts[1]
	if a2.Username != "PLEB" {
		t.Errorf("a2 identity: %+v", a2)
	}
	if a2.GMLevel != 0 {
		t.Errorf("a2 gm: %d want 0 (no row → default)", a2.GMLevel)
	}
	if a2.Online {
		t.Errorf("a2 online: %v want false", a2.Online)
	}

	// Third row: Target, 17 failed logins (brute-force signal surfaced), GM 1 on per-realm.
	a3 := accts[2]
	if a3.Username != "TARGET" {
		t.Errorf("a3 identity: %+v", a3)
	}
	if a3.FailedLogins != 17 {
		t.Errorf("a3 failedLogins: %d want 17", a3.FailedLogins)
	}
	if a3.GMLevel != 1 {
		t.Errorf("a3 gm: %d want 1 (only per-realm row available)", a3.GMLevel)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestCollectRecentLogins_ExcludeBots verifies the NOT LIKE clause + arg is
// emitted exactly when excludeBots=true. The prefix is appended-with-% inside
// the tool, so the caller passes a bare bool. Asserts the SQL shape so a
// regression that drops the filter doesn't pass silently.
func TestCollectRecentLogins_ExcludeBots(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectExec(regexp.QuoteMeta("USE `acore_auth`")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta("FROM `account` WHERE last_login IS NOT NULL AND username NOT LIKE ? ORDER BY last_login DESC LIMIT ?")).
		WithArgs(wowRecentLoginsBotPrefix+"%", int64(50)).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "username", "last_ip", "last_login", "online", "joindate", "failed_logins", "expansion",
		}).
			AddRow(int64(1001), "OPERATOR", "192.168.100.14", "2026-06-01 14:00:00", 1, "2025-01-15", 0, 2))
	mock.ExpectQuery(regexp.QuoteMeta("FROM `account_access` WHERE id IN (?)")).
		WithArgs(int64(1001)).
		WillReturnRows(sqlmock.NewRows([]string{"id", "gmlevel", "RealmID"}))

	out, err := collectRecentLogins(context.Background(), db, 0, true, 50)
	if err != nil {
		t.Fatalf("collectRecentLogins: %v", err)
	}
	if cnt, _ := out["count"].(int); cnt != 1 {
		t.Errorf("count: %v want 1", out["count"])
	}
	if excl, _ := out["excludeBots"].(bool); !excl {
		t.Errorf("excludeBots: %v want true", out["excludeBots"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestCollectRecentLogins_SinceHoursArgIsTime ensures the sinceHours filter
// passes a time.Time arg within ~25h of now (loc=UTC per the ops_ro DSN),
// not a unix-seconds integer or a duration. A misencoded zero-value time.Time
// (the silent-failure mode the driver would otherwise tolerate) surfaces as
// "0001-01-01 00:00:00 UTC" — the matcher rejects that.
func TestCollectRecentLogins_SinceHoursArgIsTime(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectExec(regexp.QuoteMeta("USE `acore_auth`")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta("FROM `account` WHERE last_login IS NOT NULL AND last_login >= ?")).
		WithArgs(recentLoginsTimeWithinMatcher{maxAge: 25 * time.Hour}, int64(50)).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "username", "last_ip", "last_login", "online", "joindate", "failed_logins", "expansion",
		}))

	out, err := collectRecentLogins(context.Background(), db, 24, false, 50)
	if err != nil {
		t.Fatalf("collectRecentLogins: %v", err)
	}
	if cnt, _ := out["count"].(int); cnt != 0 {
		t.Errorf("count: %v want 0", out["count"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// recentLoginsTimeWithinMatcher implements sqlmock.Argument and asserts the
// bound arg is a time.Time inside [now-maxAge, now]. Rejects zero-value
// time.Time (which is what a misencoded raw-int fallback would surface as).
type recentLoginsTimeWithinMatcher struct {
	maxAge time.Duration
}

func (m recentLoginsTimeWithinMatcher) Match(v driver.Value) bool {
	t, ok := v.(time.Time)
	if !ok {
		return false
	}
	now := time.Now()
	return !t.IsZero() && !t.After(now) && t.After(now.Add(-m.maxAge))
}
