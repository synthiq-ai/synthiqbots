package mcpserver

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// TestWowFailedLoginsTop_DescriptionMentionsContext guards the description
// keywords — same intent as the sibling wow_* tests: the description shapes
// which tool the agent picks when an operator asks "who's being brute-forced?"
// or "is anyone hammering a GM account?". Drop the brute-force/failed_logins
// framing and the agent falls back to a raw db_query or to wow_recent_logins
// (the wrong sort order for this question).
func TestWowFailedLoginsTop_DescriptionMentionsContext(t *testing.T) {
	reg := NewRegistry()
	RegisterWowFailedLoginsTopTool(reg, DBDeps{})
	tool, ok := reg.Get("wow_failed_logins_top")
	if !ok {
		t.Fatal("wow_failed_logins_top not registered")
	}
	for _, kw := range []string{
		"Brute-force", "failed_logins", "Two-stage composite", "accounts",
		"minFailed", "excludeBots", "topN", "wow_recent_logins",
		"account_access", "RNDBOT", "ops_ro", "RESETS",
	} {
		if !strings.Contains(tool.Description, kw) {
			t.Errorf("description missing %q: %s", kw, tool.Description)
		}
	}
}

// TestWowFailedLoginsTop_ReadOnlyAnnotation pins the tool to readOnlyHint=true
// so a future copy-paste of a destructive sibling can't silently flip the gate.
func TestWowFailedLoginsTop_ReadOnlyAnnotation(t *testing.T) {
	reg := NewRegistry()
	RegisterWowFailedLoginsTopTool(reg, DBDeps{})
	tool, _ := reg.Get("wow_failed_logins_top")
	if !strings.Contains(string(tool.Annotations), `"readOnlyHint":true`) {
		t.Errorf("annotations missing readOnlyHint=true: %s", tool.Annotations)
	}
}

func TestWowFailedLoginsTop_NoPoolReturnsError(t *testing.T) {
	reg := NewRegistry()
	RegisterWowFailedLoginsTopTool(reg, DBDeps{}) // QueryDB nil
	tool, _ := reg.Get("wow_failed_logins_top")
	resp := tool.Handler(context.Background(), json.RawMessage(`{}`), "")
	got, _ := resp.(map[string]any)
	if msg, _ := got["error"].(string); !strings.Contains(msg, "ops_ro pool not configured") {
		t.Errorf("expected pool-not-configured error, got %v", got)
	}
}

// TestWowFailedLoginsTop_TopNClamps verifies the LIMIT placeholder matches what
// we expect (default + max), protecting against a tool that silently runs a
// 200-row SELECT because the caller passed topN=99999. minFailed defaults to 1
// so it's the leading WHERE arg in every case here.
func TestWowFailedLoginsTop_TopNClamps(t *testing.T) {
	cases := []struct {
		name     string
		args     string
		wantTopN int64
	}{
		{"unset uses default", `{}`, wowFailedLoginsDefaultTopN},
		{"negative falls back to default", `{"topN":-5}`, wowFailedLoginsDefaultTopN},
		{"oversized clamps to max", `{"topN":99999}`, wowFailedLoginsMaxTopN},
		{"in-range passes through", `{"topN":42}`, 42},
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
			mock.ExpectQuery(regexp.QuoteMeta("FROM `account` WHERE failed_logins >= ? ORDER BY failed_logins DESC, last_login DESC, id ASC LIMIT ?")).
				WithArgs(int64(wowFailedLoginsDefaultMin), c.wantTopN).
				WillReturnRows(sqlmock.NewRows([]string{
					"id", "username", "last_ip", "last_login", "online", "joindate", "failed_logins", "expansion",
				}))

			reg := NewRegistry()
			RegisterWowFailedLoginsTopTool(reg, DBDeps{QueryDB: db})
			tool, _ := reg.Get("wow_failed_logins_top")
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

// TestWowFailedLoginsTop_MinFailedClamps ensures the failed_logins floor never
// drops below 1 (a floor of 0 would surface every account and defeat the tool's
// purpose). Verifies the response echoes the post-clamp value and that the
// floor is bound as the leading WHERE arg.
func TestWowFailedLoginsTop_MinFailedClamps(t *testing.T) {
	cases := []struct {
		name          string
		args          string
		wantMinFailed int64
	}{
		{"unset = default 1", `{}`, wowFailedLoginsDefaultMin},
		{"zero clamps to 1", `{"minFailed":0}`, wowFailedLoginsDefaultMin},
		{"negative clamps to 1", `{"minFailed":-9}`, wowFailedLoginsDefaultMin},
		{"in-range passes through", `{"minFailed":10}`, 10},
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
			mock.ExpectQuery(regexp.QuoteMeta("FROM `account` WHERE failed_logins >= ?")).
				WithArgs(c.wantMinFailed, int64(wowFailedLoginsDefaultTopN)).
				WillReturnRows(sqlmock.NewRows([]string{
					"id", "username", "last_ip", "last_login", "online", "joindate", "failed_logins", "expansion",
				}))

			reg := NewRegistry()
			RegisterWowFailedLoginsTopTool(reg, DBDeps{QueryDB: db})
			tool, _ := reg.Get("wow_failed_logins_top")
			resp := tool.Handler(context.Background(), json.RawMessage(c.args), "")
			got, _ := resp.(map[string]any)
			if got["error"] != nil {
				t.Fatalf("unexpected error: %v", got)
			}
			if mf, _ := got["minFailed"].(int); int64(mf) != c.wantMinFailed {
				t.Errorf("response minFailed: %v want %d", got["minFailed"], c.wantMinFailed)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Errorf("unmet mock expectations: %v", err)
			}
		})
	}
}

// TestCollectFailedLoginsTop_Empty — the no-brute-force case. Important contract
// because the alternative (skipping the response shape entirely) would break any
// caller expecting accounts to be an array.
func TestCollectFailedLoginsTop_Empty(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.ExpectExec(regexp.QuoteMeta("USE `acore_auth`")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta("FROM `account` WHERE failed_logins >= ?")).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "username", "last_ip", "last_login", "online", "joindate", "failed_logins", "expansion",
		}))

	out, err := collectFailedLoginsTop(context.Background(), db, 1, false, 25)
	if err != nil {
		t.Fatalf("collectFailedLoginsTop: %v", err)
	}
	if cnt, _ := out["count"].(int); cnt != 0 {
		t.Errorf("count: %v want 0", out["count"])
	}
	accts, ok := out["accounts"].([]*failedLoginAccount)
	if !ok {
		t.Fatalf("accounts type: %T want []*failedLoginAccount", out["accounts"])
	}
	if len(accts) != 0 {
		t.Errorf("accounts len: %d want 0", len(accts))
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestCollectFailedLoginsTop_Golden drives the full happy path with three
// accounts in failed_logins-desc order — a pure-attack-target GM (NULL
// last_login, never succeeded, highest count), a brute-forced human, and a
// plain account just over the floor. Exercises:
//   - handler does NOT re-sort (SQL ORDER BY is the source of truth)
//   - account_access precedence (RealmID = -1 wins over per-realm)
//   - NULL last_login surfaces as empty LastLoginISO (the most-suspicious row)
//   - online flag + GM annotation defaulting to 0 for accounts without a row
func TestCollectFailedLoginsTop_Golden(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectExec(regexp.QuoteMeta("USE `acore_auth`")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta("FROM `account` WHERE failed_logins >= ? ORDER BY failed_logins DESC, last_login DESC, id ASC LIMIT ?")).
		WithArgs(int64(1), int64(25)).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "username", "last_ip", "last_login", "online", "joindate", "failed_logins", "expansion",
		}).
			// ADMIN: 142 failures, NULL last_login (never succeeded) — pure attack target.
			AddRow(int64(2001), "ADMIN", "203.0.113.7", nil, 0, "2025-01-01 00:00:00", 142, 2).
			// OPERATOR: 31 failures but a real last_login (succeeded eventually).
			AddRow(int64(1001), "OPERATOR", "192.168.100.14", "2026-06-01 14:00:00", 1, "2025-01-15 12:00:00", 31, 2).
			// PLEB: just over the floor.
			AddRow(int64(1002), "PLEB", "192.168.100.12", "2026-05-30 09:00:00", 0, "2025-02-20 09:30:00", 3, 2))

	// account_access: ADMIN has an all-realms gm-3 row AND a per-realm gm-1
	// row — the all-realms row should win. OPERATOR has a per-realm gm-1 row.
	// PLEB has no row at all.
	mock.ExpectQuery(regexp.QuoteMeta("FROM `account_access` WHERE id IN (?,?,?)")).
		WithArgs(int64(2001), int64(1001), int64(1002)).
		WillReturnRows(sqlmock.NewRows([]string{"id", "gmlevel", "RealmID"}).
			AddRow(int64(2001), 3, -1).
			AddRow(int64(2001), 1, 1).
			AddRow(int64(1001), 1, 1))

	out, err := collectFailedLoginsTop(context.Background(), db, 1, false, 25)
	if err != nil {
		t.Fatalf("collectFailedLoginsTop: %v", err)
	}
	if cnt, _ := out["count"].(int); cnt != 3 {
		t.Errorf("count: %v want 3", out["count"])
	}
	if mf, _ := out["minFailed"].(int); mf != 1 {
		t.Errorf("minFailed: %v want 1", out["minFailed"])
	}
	if excl, _ := out["excludeBots"].(bool); excl {
		t.Errorf("excludeBots: %v want false", out["excludeBots"])
	}
	accts, _ := out["accounts"].([]*failedLoginAccount)
	if len(accts) != 3 {
		t.Fatalf("accounts len: %d want 3", len(accts))
	}

	// First row: ADMIN — highest failed count, NULL last_login (never
	// succeeded), GM 3 (all-realms wins). The flagship brute-force-against-GM
	// signal this tool exists to surface.
	a1 := accts[0]
	if a1.Username != "ADMIN" || a1.AccountID != 2001 {
		t.Errorf("a1 identity: %+v", a1)
	}
	if a1.FailedLogins != 142 {
		t.Errorf("a1 failedLogins: %d want 142", a1.FailedLogins)
	}
	if a1.LastLoginISO != "" {
		t.Errorf("a1 lastLoginIso: %q want empty (NULL last_login)", a1.LastLoginISO)
	}
	if a1.Online {
		t.Errorf("a1 online: %v want false", a1.Online)
	}
	if a1.GMLevel != 3 {
		t.Errorf("a1 gm: %d want 3 (all-realms row should win)", a1.GMLevel)
	}

	// Second row: OPERATOR — 31 failures, real last_login, GM 1 (only per-realm).
	a2 := accts[1]
	if a2.Username != "OPERATOR" || a2.FailedLogins != 31 {
		t.Errorf("a2 identity/count: %+v", a2)
	}
	if a2.LastLoginISO != "2026-06-01 14:00:00" {
		t.Errorf("a2 lastLoginIso: %q", a2.LastLoginISO)
	}
	if !a2.Online {
		t.Errorf("a2 online: %v want true", a2.Online)
	}
	if a2.GMLevel != 1 {
		t.Errorf("a2 gm: %d want 1 (only per-realm row available)", a2.GMLevel)
	}

	// Third row: PLEB — just over the floor, no account_access row → GM 0.
	a3 := accts[2]
	if a3.Username != "PLEB" || a3.FailedLogins != 3 {
		t.Errorf("a3 identity/count: %+v", a3)
	}
	if a3.GMLevel != 0 {
		t.Errorf("a3 gm: %d want 0 (no row → default)", a3.GMLevel)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestCollectFailedLoginsTop_ExcludeBots verifies the NOT LIKE clause + arg is
// emitted exactly when excludeBots=true, with the floor arg leading and the
// LIMIT arg trailing. Asserts the SQL shape so a regression that drops the
// filter doesn't pass silently.
func TestCollectFailedLoginsTop_ExcludeBots(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectExec(regexp.QuoteMeta("USE `acore_auth`")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta("FROM `account` WHERE failed_logins >= ? AND username NOT LIKE ? ORDER BY failed_logins DESC, last_login DESC, id ASC LIMIT ?")).
		WithArgs(int64(5), wowFailedLoginsBotPrefix+"%", int64(25)).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "username", "last_ip", "last_login", "online", "joindate", "failed_logins", "expansion",
		}).
			AddRow(int64(2001), "ADMIN", "203.0.113.7", nil, 0, "2025-01-01", 142, 2))
	mock.ExpectQuery(regexp.QuoteMeta("FROM `account_access` WHERE id IN (?)")).
		WithArgs(int64(2001)).
		WillReturnRows(sqlmock.NewRows([]string{"id", "gmlevel", "RealmID"}))

	out, err := collectFailedLoginsTop(context.Background(), db, 5, true, 25)
	if err != nil {
		t.Fatalf("collectFailedLoginsTop: %v", err)
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
