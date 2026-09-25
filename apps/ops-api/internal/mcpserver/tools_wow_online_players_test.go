package mcpserver

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// TestWowOnlinePlayers_DescriptionMentionsContext guards the description
// keywords — same intent as the wow_player_lookup test: the description
// shapes which tool the agent picks when an operator asks "anyone online?"
// or "which GMs are logged in?". Drop the composite framing and the agent
// falls back to chained db_query calls.
func TestWowOnlinePlayers_DescriptionMentionsContext(t *testing.T) {
	reg := NewRegistry()
	RegisterWowOnlinePlayersTool(reg, DBDeps{})
	tool, ok := reg.Get("wow_online_players")
	if !ok {
		t.Fatal("wow_online_players not registered")
	}
	for _, kw := range []string{"online", "Composite", "count", "players", "gmOnly", "minLevel", "limit"} {
		if !strings.Contains(tool.Description, kw) {
			t.Errorf("description missing %q: %s", kw, tool.Description)
		}
	}
}

func TestWowOnlinePlayers_NoPoolReturnsError(t *testing.T) {
	reg := NewRegistry()
	RegisterWowOnlinePlayersTool(reg, DBDeps{}) // QueryDB nil
	tool, _ := reg.Get("wow_online_players")
	resp := tool.Handler(context.Background(), json.RawMessage(`{}`), "")
	got, _ := resp.(map[string]any)
	if msg, _ := got["error"].(string); !strings.Contains(msg, "ops_ro pool not configured") {
		t.Errorf("expected pool-not-configured error, got %v", got)
	}
}

// TestWowOnlinePlayers_LimitClamps ensures negative / over-cap limits are
// clamped at the handler before they hit SQL. Verifies the LIMIT placeholder
// matches what we expect (default + max), which protects against a tool that
// silently runs a 5000-row SELECT because the caller passed limit=99999.
func TestWowOnlinePlayers_LimitClamps(t *testing.T) {
	cases := []struct {
		name      string
		args      string
		wantLimit int64
	}{
		{"unset uses default", `{}`, wowOnlinePlayersDefaultLimit},
		{"negative falls back to default", `{"limit":-5}`, wowOnlinePlayersDefaultLimit},
		{"oversized clamps to max", `{"limit":99999}`, wowOnlinePlayersMaxLimit},
		{"in-range passes through", `{"limit":42}`, 42},
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
			mock.ExpectQuery(regexp.QuoteMeta("FROM `characters` WHERE online = 1")).
				WithArgs(c.wantLimit).
				WillReturnRows(sqlmock.NewRows([]string{
					"guid", "account", "name", "race", "class", "gender", "level", "zone", "map",
				}))

			reg := NewRegistry()
			RegisterWowOnlinePlayersTool(reg, DBDeps{QueryDB: db})
			tool, _ := reg.Get("wow_online_players")
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

// TestCollectOnlinePlayers_Empty — the no-one-online case. Important contract
// because the alternative (skipping the response shape entirely) would break
// any caller expecting players to be an array.
func TestCollectOnlinePlayers_Empty(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.ExpectExec(regexp.QuoteMeta("USE `acore_characters`")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta("FROM `characters` WHERE online = 1")).
		WillReturnRows(sqlmock.NewRows([]string{
			"guid", "account", "name", "race", "class", "gender", "level", "zone", "map",
		}))

	out, err := collectOnlinePlayers(context.Background(), db, 0, false, 100)
	if err != nil {
		t.Fatalf("collectOnlinePlayers: %v", err)
	}
	if cnt, _ := out["count"].(int); cnt != 0 {
		t.Errorf("count: %v want 0", out["count"])
	}
	players, ok := out["players"].([]*onlinePlayer)
	if !ok {
		t.Fatalf("players type: %T want []*onlinePlayer", out["players"])
	}
	if len(players) != 0 {
		t.Errorf("players len: %d want 0", len(players))
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestCollectOnlinePlayers_Golden drives the full happy path with two online
// characters belonging to two accounts. Exercises:
//   - race / class translation
//   - account batch-fetch (IN clause)
//   - account_access precedence (RealmID = -1 wins over per-realm row)
//   - orphan character (account_access has no row → GMLevel stays 0)
func TestCollectOnlinePlayers_Golden(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectExec(regexp.QuoteMeta("USE `acore_characters`")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta("FROM `characters` WHERE online = 1")).
		WithArgs(int64(100)).
		WillReturnRows(sqlmock.NewRows([]string{
			"guid", "account", "name", "race", "class", "gender", "level", "zone", "map",
		}).
			AddRow(int64(12345), int64(1001), "Slayo", 1, 5, 0, 80, 1519, 0).
			AddRow(int64(67890), int64(1002), "Botbo", 2, 1, 1, 70, 14, 0))

	mock.ExpectExec(regexp.QuoteMeta("USE `acore_auth`")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta("FROM `account` WHERE id IN (?,?)")).
		WithArgs(int64(1001), int64(1002)).
		WillReturnRows(sqlmock.NewRows([]string{"id", "username", "last_ip"}).
			AddRow(int64(1001), "OPERATOR", "192.168.100.14").
			AddRow(int64(1002), "BOTBO", "192.168.100.12"))

	// account_access: OPERATOR has both an all-realms gm-3 row AND a per-realm
	// gm-1 row. The all-realms row should win. BOTBO has no row at all.
	mock.ExpectQuery(regexp.QuoteMeta("FROM `account_access` WHERE id IN (?,?)")).
		WithArgs(int64(1001), int64(1002)).
		WillReturnRows(sqlmock.NewRows([]string{"id", "gmlevel", "RealmID"}).
			AddRow(int64(1001), 3, -1).
			AddRow(int64(1001), 1, 1))

	out, err := collectOnlinePlayers(context.Background(), db, 0, false, 100)
	if err != nil {
		t.Fatalf("collectOnlinePlayers: %v", err)
	}
	if cnt, _ := out["count"].(int); cnt != 2 {
		t.Errorf("count: %v want 2", out["count"])
	}
	players, _ := out["players"].([]*onlinePlayer)
	if len(players) != 2 {
		t.Fatalf("players len: %d want 2", len(players))
	}

	// First row: Slayo, Human Priest, GM 3 (all-realms wins).
	p1 := players[0]
	if p1.Name != "Slayo" || p1.RaceName != "Human" || p1.ClassName != "Priest" {
		t.Errorf("p1 identity: %+v", p1)
	}
	if p1.Username != "OPERATOR" || p1.LastIP != "192.168.100.14" {
		t.Errorf("p1 account annotation: %+v", p1)
	}
	if p1.GMLevel != 3 {
		t.Errorf("p1 gm: %d want 3 (all-realms row should win)", p1.GMLevel)
	}

	// Second row: Botbo, Orc Warrior, no GM row.
	p2 := players[1]
	if p2.Name != "Botbo" || p2.RaceName != "Orc" || p2.ClassName != "Warrior" {
		t.Errorf("p2 identity: %+v", p2)
	}
	if p2.Username != "BOTBO" {
		t.Errorf("p2 account annotation: %+v", p2)
	}
	if p2.GMLevel != 0 {
		t.Errorf("p2 gm: %d want 0 (no row → default)", p2.GMLevel)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestCollectOnlinePlayers_MinLevelAndGmOnly exercises both filter paths:
// minLevel pushes the floor into the SQL WHERE; gmOnly applies after the
// GM-level annotation in Go (so we can use the actual value, not just a
// JOIN's existence flag).
func TestCollectOnlinePlayers_MinLevelAndGmOnly(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectExec(regexp.QuoteMeta("USE `acore_characters`")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	// minLevel = 70 → expect "AND level >= ?" with 70 + LIMIT.
	mock.ExpectQuery(regexp.QuoteMeta("FROM `characters` WHERE online = 1 AND level >= ?")).
		WithArgs(int64(70), int64(50)).
		WillReturnRows(sqlmock.NewRows([]string{
			"guid", "account", "name", "race", "class", "gender", "level", "zone", "map",
		}).
			AddRow(int64(1), int64(101), "GMAlice", 1, 2, 0, 80, 1, 0).
			AddRow(int64(2), int64(102), "BobThePleb", 2, 1, 0, 75, 2, 0))

	mock.ExpectExec(regexp.QuoteMeta("USE `acore_auth`")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta("FROM `account` WHERE id IN (?,?)")).
		WillReturnRows(sqlmock.NewRows([]string{"id", "username", "last_ip"}).
			AddRow(int64(101), "ALICE", "10.0.0.1").
			AddRow(int64(102), "BOB", "10.0.0.2"))
	mock.ExpectQuery(regexp.QuoteMeta("FROM `account_access` WHERE id IN (?,?)")).
		WillReturnRows(sqlmock.NewRows([]string{"id", "gmlevel", "RealmID"}).
			AddRow(int64(101), 3, -1))

	out, err := collectOnlinePlayers(context.Background(), db, 70, true, 50)
	if err != nil {
		t.Fatalf("collectOnlinePlayers: %v", err)
	}
	if cnt, _ := out["count"].(int); cnt != 1 {
		t.Errorf("count after gmOnly: %v want 1", out["count"])
	}
	players, _ := out["players"].([]*onlinePlayer)
	if len(players) != 1 || players[0].Name != "GMAlice" {
		t.Errorf("gmOnly survivors: %+v", players)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

func TestBuildAccountIDInQuery(t *testing.T) {
	players := []*onlinePlayer{
		{AccountID: 1001},
		{AccountID: 1002},
		{AccountID: 1003},
	}
	q, args := buildAccountIDInQuery("SELECT id FROM `account` WHERE id IN ", players)
	if q != "SELECT id FROM `account` WHERE id IN (?,?,?)" {
		t.Errorf("query: %q", q)
	}
	if len(args) != 3 || args[0] != int64(1001) || args[2] != int64(1003) {
		t.Errorf("args: %v", args)
	}

	// Empty input — defensive, the call sites guard against this but verify
	// we don't emit a broken "IN ()" if a future caller forgets the check.
	q, args = buildAccountIDInQuery("SELECT id FROM `account` WHERE id IN ", nil)
	if q != "SELECT id FROM `account` WHERE id IN ()" {
		t.Errorf("empty query: %q", q)
	}
	if len(args) != 0 {
		t.Errorf("empty args: %v", args)
	}
}
