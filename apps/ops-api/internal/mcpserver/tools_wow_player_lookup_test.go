package mcpserver

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// TestWowPlayerLookup_DescriptionMentionsContext is the same kind of guard as
// db_table_info: the description shapes which tool the agent picks when an
// operator asks "who is X". Drop the "composite" framing and the agent will
// fall back to chained db_query calls.
func TestWowPlayerLookup_DescriptionMentionsContext(t *testing.T) {
	reg := NewRegistry()
	RegisterWowPlayerLookupTool(reg, DBDeps{})
	tool, ok := reg.Get("wow_player_lookup")
	if !ok {
		t.Fatal("wow_player_lookup not registered")
	}
	for _, kw := range []string{"character", "account", "gm", "bans", "Composite", "name", "guid"} {
		if !strings.Contains(tool.Description, kw) {
			t.Errorf("description missing %q: %s", kw, tool.Description)
		}
	}
}

func TestWowPlayerLookup_NoPoolReturnsError(t *testing.T) {
	reg := NewRegistry()
	RegisterWowPlayerLookupTool(reg, DBDeps{}) // QueryDB nil
	tool, _ := reg.Get("wow_player_lookup")
	resp := tool.Handler(context.Background(),
		json.RawMessage(`{"name":"Slayo"}`), "")
	got, _ := resp.(map[string]any)
	if msg, _ := got["error"].(string); !strings.Contains(msg, "ops_ro pool not configured") {
		t.Errorf("expected pool-not-configured error, got %v", got)
	}
}

func TestWowPlayerLookup_RequiresOneIdentifier(t *testing.T) {
	reg := NewRegistry()
	db, _, _ := sqlmock.New()
	defer db.Close()
	RegisterWowPlayerLookupTool(reg, DBDeps{QueryDB: db})
	tool, _ := reg.Get("wow_player_lookup")

	resp := tool.Handler(context.Background(), json.RawMessage(`{}`), "")
	got, _ := resp.(map[string]any)
	if msg, _ := got["error"].(string); !strings.Contains(msg, "either name or guid is required") {
		t.Errorf("expected name-or-guid error, got %v", got)
	}

	resp = tool.Handler(context.Background(),
		json.RawMessage(`{"name":"Slayo","guid":42}`), "")
	got, _ = resp.(map[string]any)
	if msg, _ := got["error"].(string); !strings.Contains(msg, "pass either name or guid, not both") {
		t.Errorf("expected mutual-exclusivity error, got %v", got)
	}
}

// TestCollectPlayerLookup_NotFound — explicit found:false response on a typo'd
// name. Important contract because the alternative (an "error" string) would
// derail the operator's recovery path; they're mid-debugging and want to
// retry, not handle a missing-row error.
func TestCollectPlayerLookup_NotFound(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectExec(regexp.QuoteMeta("USE `acore_characters`")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta("FROM `characters` WHERE name = ?")).
		WithArgs("NoSuch").
		WillReturnRows(sqlmock.NewRows([]string{
			"guid", "account", "name", "race", "class", "gender", "level",
			"online", "money", "totaltime", "leveltime", "logout_time",
			"zone", "map", "position_x", "position_y", "position_z",
		}))

	out, err := collectPlayerLookup(context.Background(), db, "NoSuch", 0)
	if err != nil {
		t.Fatalf("collectPlayerLookup: %v", err)
	}
	if found, _ := out["found"].(bool); found {
		t.Errorf("expected found=false, got %v", out)
	}
	if got := out["lookupBy"]; got != "name" {
		t.Errorf("lookupBy: %v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestCollectPlayerLookup_Golden drives the full happy path with a mock
// connection — exercises the 5-statement composite end to end. Picks the
// edge cases that would silently break the parser:
//   - logout_time is NULL on a never-logged-out character
//   - account_access has no row (player, level=0)
//   - account_banned has an active row (with permanent flag inferred)
//   - character_banned has no row
//   - money has all three currency components (gold + silver + copper)
func TestCollectPlayerLookup_Golden(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectExec(regexp.QuoteMeta("USE `acore_characters`")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta("FROM `characters` WHERE name = ?")).
		WithArgs("Slayo").
		WillReturnRows(sqlmock.NewRows([]string{
			"guid", "account", "name", "race", "class", "gender", "level",
			"online", "money", "totaltime", "leveltime", "logout_time",
			"zone", "map", "position_x", "position_y", "position_z",
		}).AddRow(
			int64(12345), int64(1001), "Slayo", 1, 5, 0, 80,
			int(1), int64(12345), int64(360000), int64(7200), nil,
			1519, 0, -8829.4, 624.5, 94.0,
		))

	mock.ExpectQuery(regexp.QuoteMeta("FROM `character_banned` WHERE guid = ? AND active = 1")).
		WithArgs(int64(12345)).
		WillReturnRows(sqlmock.NewRows([]string{"bandate", "unbandate", "bannedby", "banreason"}))

	mock.ExpectExec(regexp.QuoteMeta("USE `acore_auth`")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta("FROM `account` WHERE id = ?")).
		WithArgs(int64(1001)).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "username", "email", "last_ip", "last_login",
			"locked", "online", "joindate", "expansion",
		}).AddRow(
			int64(1001), "OPERATOR", "game-host@example.com", "192.168.100.14",
			"2026-05-09 18:00:00", int(0), int(1), "2025-01-01 00:00:00", 2,
		))

	mock.ExpectQuery(regexp.QuoteMeta("FROM `account_access` WHERE id = ?")).
		WithArgs(int64(1001)).
		WillReturnRows(sqlmock.NewRows([]string{"gmlevel", "RealmID"}))

	mock.ExpectQuery(regexp.QuoteMeta("FROM `account_banned` WHERE id = ? AND active = 1")).
		WithArgs(int64(1001)).
		WillReturnRows(sqlmock.NewRows([]string{"bandate", "unbandate", "bannedby", "banreason"}).
			AddRow(int64(1700000000), int64(0), "GM_Admin", "test ban"))

	out, err := collectPlayerLookup(context.Background(), db, "Slayo", 0)
	if err != nil {
		t.Fatalf("collectPlayerLookup: %v", err)
	}

	if found, _ := out["found"].(bool); !found {
		t.Fatalf("expected found=true, got %v", out)
	}

	ch, _ := out["character"].(*playerCharacter)
	if ch == nil {
		t.Fatal("character missing from response")
	}
	if ch.RaceName != "Human" {
		t.Errorf("raceName: %q want Human", ch.RaceName)
	}
	if ch.ClassName != "Priest" {
		t.Errorf("className: %q want Priest", ch.ClassName)
	}
	if ch.MoneyHuman != "1g 23s 45c" {
		t.Errorf("moneyHuman: %q", ch.MoneyHuman)
	}
	if ch.TotalTimeHuman != "100h 0m" {
		t.Errorf("totalTimeHuman: %q", ch.TotalTimeHuman)
	}
	if !ch.Online {
		t.Errorf("online flag did not coerce: %+v", ch)
	}
	if ch.LogoutTimeISO != "" {
		t.Errorf("logoutTimeIso for NULL logout should be empty, got %q", ch.LogoutTimeISO)
	}

	acct, _ := out["account"].(*playerAccount)
	if acct == nil || acct.Username != "OPERATOR" {
		t.Errorf("account: %+v", acct)
	}
	if !acct.Online || acct.Locked {
		t.Errorf("account flags: online=%v locked=%v", acct.Online, acct.Locked)
	}

	gm, _ := out["gm"].(*playerGM)
	if gm == nil || gm.Level != 0 || gm.RealmID != -1 {
		t.Errorf("gm default-when-no-row: %+v", gm)
	}

	bans, _ := out["bans"].(map[string]any)
	if bans == nil {
		t.Fatal("bans missing")
	}
	if bans["accountBanned"] != true || bans["characterBanned"] != false {
		t.Errorf("ban flags: %+v", bans)
	}
	acctBan, _ := bans["activeAccountBan"].(*playerBan)
	if acctBan == nil {
		t.Fatal("activeAccountBan missing")
	}
	if !acctBan.Permanent {
		t.Errorf("expected permanent ban (unbandate=0), got %+v", acctBan)
	}
	if acctBan.BannedBy != "GM_Admin" || acctBan.BanReason != "test ban" {
		t.Errorf("ban details: %+v", acctBan)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestCollectPlayerLookup_GuidPathAndGmRow exercises the second lookup path
// (by guid) and the case where account_access has a real row — together they
// cover the branches the golden test skipped.
func TestCollectPlayerLookup_GuidPathAndGmRow(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectExec(regexp.QuoteMeta("USE `acore_characters`")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta("FROM `characters` WHERE guid = ?")).
		WithArgs(int64(12345)).
		WillReturnRows(sqlmock.NewRows([]string{
			"guid", "account", "name", "race", "class", "gender", "level",
			"online", "money", "totaltime", "leveltime", "logout_time",
			"zone", "map", "position_x", "position_y", "position_z",
		}).AddRow(
			int64(12345), int64(1001), "Slayo", 10, 6, 1, 80,
			int(0), int64(0), int64(0), int64(0), int64(1715283600),
			0, 1, 0.0, 0.0, 0.0,
		))
	mock.ExpectQuery(regexp.QuoteMeta("FROM `character_banned` WHERE guid = ? AND active = 1")).
		WithArgs(int64(12345)).
		WillReturnRows(sqlmock.NewRows([]string{"bandate", "unbandate", "bannedby", "banreason"}))

	mock.ExpectExec(regexp.QuoteMeta("USE `acore_auth`")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta("FROM `account` WHERE id = ?")).
		WithArgs(int64(1001)).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "username", "email", "last_ip", "last_login",
			"locked", "online", "joindate", "expansion",
		}).AddRow(
			int64(1001), "OPERATOR", nil, "192.168.100.14",
			"2026-05-09 18:00:00", int(0), int(0), "2025-01-01 00:00:00", 2,
		))
	mock.ExpectQuery(regexp.QuoteMeta("FROM `account_access` WHERE id = ?")).
		WithArgs(int64(1001)).
		WillReturnRows(sqlmock.NewRows([]string{"gmlevel", "RealmID"}).
			AddRow(3, -1))
	mock.ExpectQuery(regexp.QuoteMeta("FROM `account_banned` WHERE id = ? AND active = 1")).
		WithArgs(int64(1001)).
		WillReturnRows(sqlmock.NewRows([]string{"bandate", "unbandate", "bannedby", "banreason"}))

	out, err := collectPlayerLookup(context.Background(), db, "", 12345)
	if err != nil {
		t.Fatalf("collectPlayerLookup: %v", err)
	}

	ch, _ := out["character"].(*playerCharacter)
	if ch.RaceName != "Blood Elf" || ch.ClassName != "Death Knight" {
		t.Errorf("race/class: %q/%q", ch.RaceName, ch.ClassName)
	}
	if ch.MoneyHuman != "0c" {
		t.Errorf("moneyHuman zero: %q", ch.MoneyHuman)
	}
	if ch.LogoutTimeISO == "" {
		t.Errorf("logoutTimeIso should be populated when logout_time is set")
	}

	gm, _ := out["gm"].(*playerGM)
	if gm.Level != 3 || gm.RealmID != -1 {
		t.Errorf("gm row: %+v", gm)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

func TestFormatMoney(t *testing.T) {
	cases := []struct {
		copper int64
		want   string
	}{
		{0, "0c"},
		{45, "45c"},
		{100, "1s 0c"},
		{12345, "1g 23s 45c"},
		{-12345, "-1g 23s 45c"},
		{99, "99c"},
		{10000, "1g 0s 0c"},
	}
	for _, c := range cases {
		if got := formatMoney(c.copper); got != c.want {
			t.Errorf("formatMoney(%d)=%q want %q", c.copper, got, c.want)
		}
	}
}

func TestFormatDuration(t *testing.T) {
	cases := []struct {
		sec  int64
		want string
	}{
		{0, "0m"},
		{59, "0m"},
		{60, "1m"},
		{3600, "1h 0m"},
		{3660, "1h 1m"},
		{360000, "100h 0m"},
		{-5, "0m"},
	}
	for _, c := range cases {
		if got := formatDuration(c.sec); got != c.want {
			t.Errorf("formatDuration(%d)=%q want %q", c.sec, got, c.want)
		}
	}
}

func TestWowRaceClassName_FallbackForUnknownIDs(t *testing.T) {
	if got := wowRaceName(99); got != "race_99" {
		t.Errorf("wowRaceName fallback: %q", got)
	}
	if got := wowClassName(99); got != "class_99" {
		t.Errorf("wowClassName fallback: %q", got)
	}
}
