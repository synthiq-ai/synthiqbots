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

// TestWowBotsFleetStatus_DescriptionMentionsContext guards the description
// keywords — same intent as the other wow_* composite tests: the description
// shapes which tool the agent picks when an operator asks "how many bots are
// online?" or "which bots keep erroring?". Drop the composite framing and the
// agent falls back to chained db_query + ops_audit_summary calls.
func TestWowBotsFleetStatus_DescriptionMentionsContext(t *testing.T) {
	reg := NewRegistry()
	RegisterWowBotsFleetStatusTool(reg, DBDeps{})
	tool, ok := reg.Get("wow_bots_fleet_status")
	if !ok {
		t.Fatal("wow_bots_fleet_status not registered")
	}
	for _, kw := range []string{"Composite", "fleet", "RNDBOT", "accountPrefix", "errorWindow", "topErrored", "topN", "ops_audit_summary"} {
		if !strings.Contains(tool.Description, kw) {
			t.Errorf("description missing %q: %s", kw, tool.Description)
		}
	}
}

func TestWowBotsFleetStatus_NoPoolReturnsError(t *testing.T) {
	reg := NewRegistry()
	RegisterWowBotsFleetStatusTool(reg, DBDeps{}) // QueryDB nil
	tool, _ := reg.Get("wow_bots_fleet_status")
	resp := tool.Handler(context.Background(), json.RawMessage(`{}`), "")
	got, _ := resp.(map[string]any)
	if msg, _ := got["error"].(string); !strings.Contains(msg, "ops_ro pool not configured") {
		t.Errorf("expected pool-not-configured error, got %v", got)
	}
}

// TestWowBotsFleetStatus_ArgValidation covers the input-shape errors that
// don't need DB mocking: wildcard prefix, bad window, bad JSON.
func TestWowBotsFleetStatus_ArgValidation(t *testing.T) {
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	reg := NewRegistry()
	RegisterWowBotsFleetStatusTool(reg, DBDeps{QueryDB: db})
	tool, _ := reg.Get("wow_bots_fleet_status")

	cases := []struct {
		name    string
		args    string
		wantSub string
	}{
		{"wildcard prefix rejected (%)", `{"accountPrefix":"RND%"}`, "wildcards"},
		{"wildcard prefix rejected (_)", `{"accountPrefix":"R_NDB"}`, "wildcards"},
		{"negative window rejected", `{"errorWindow":"-5m"}`, "positive duration"},
		{"oversized window rejected", `{"errorWindow":"800h"}`, "positive duration"},
		{"malformed window rejected", `{"errorWindow":"forever"}`, "positive duration"},
		{"bad json rejected", `{"topN":"not a number"}`, "decode args"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resp := tool.Handler(context.Background(), json.RawMessage(c.args), "")
			got, _ := resp.(map[string]any)
			msg, _ := got["error"].(string)
			if !strings.Contains(msg, c.wantSub) {
				t.Errorf("error: %q want substring %q", msg, c.wantSub)
			}
		})
	}
}

// TestBucketForLevel exercises the level → bucket mapping at every edge that
// matters: 0 (unleveled garbage rows), 1 (bucket 0), 9/10 (boundary), 79/80
// (boundary into the level-cap bucket), and >80 (defensive ceiling clamp).
func TestBucketForLevel(t *testing.T) {
	cases := []struct {
		level int
		want  int
	}{
		{0, 0},   // defensive — characters never have level 0, but if a row does it goes to "1-9"
		{1, 0},   // floor of "1-9"
		{9, 0},   // ceiling of "1-9"
		{10, 1},  // floor of "10-19"
		{19, 1},  // ceiling of "10-19"
		{70, 7},  // floor of "70-79"
		{79, 7},  // ceiling of "70-79"
		{80, 8},  // level cap is its own bucket
		{81, 8},  // defensive — over-cap clamps to "80"
		{-3, 0},  // defensive — negative clamps to bucket 0
	}
	for _, c := range cases {
		if got := bucketForLevel(c.level); got != c.want {
			t.Errorf("bucketForLevel(%d) = %d, want %d", c.level, got, c.want)
		}
	}
}

// TestFormatClassDistribution verifies the sort order (desc count, ties by
// asc classId) AND the className translation — the snapshot's whole point is
// agent-readable class names instead of raw integers.
func TestFormatClassDistribution(t *testing.T) {
	counts := map[int]int{
		1: 5,  // Warrior
		2: 10, // Paladin
		8: 10, // Mage (tie with Paladin → Paladin first by classId asc)
		9: 3,  // Warlock
	}
	got := formatClassDistribution(counts)
	if len(got) != 4 {
		t.Fatalf("len: %d want 4", len(got))
	}
	if got[0].ClassName != "Paladin" || got[0].Count != 10 {
		t.Errorf("top: %+v want Paladin/10", got[0])
	}
	if got[1].ClassName != "Mage" || got[1].Count != 10 {
		t.Errorf("tie-2: %+v want Mage/10 (asc classId breaks tie)", got[1])
	}
	if got[2].ClassName != "Warrior" || got[3].ClassName != "Warlock" {
		t.Errorf("tail: %+v / %+v", got[2], got[3])
	}
}

// TestFormatLevelDistribution checks the increasing-level order and the
// empty-bucket omission (a fleet of just 80s should produce one bucket, not
// nine zero-counts).
func TestFormatLevelDistribution(t *testing.T) {
	got := formatLevelDistribution(map[int]int{0: 2, 7: 4, 8: 15})
	if len(got) != 3 {
		t.Fatalf("len: %d want 3", len(got))
	}
	if got[0].Bucket != "1-9" || got[0].Count != 2 {
		t.Errorf("got[0]: %+v want 1-9/2", got[0])
	}
	if got[1].Bucket != "70-79" || got[1].Count != 4 {
		t.Errorf("got[1]: %+v want 70-79/4", got[1])
	}
	if got[2].Bucket != "80" || got[2].Count != 15 {
		t.Errorf("got[2]: %+v want 80/15", got[2])
	}

	// All-level-80 fleet — confirm the omission rule (no zero buckets).
	got = formatLevelDistribution(map[int]int{8: 500})
	if len(got) != 1 || got[0].Bucket != "80" {
		t.Errorf("all-80 fleet: %+v want single 80 bucket", got)
	}
}

// TestFormatZoneDistribution caps to 10 and orders ties by zoneId asc.
func TestFormatZoneDistribution(t *testing.T) {
	counts := map[int]int{}
	for z := 1; z <= 15; z++ {
		counts[z] = z // zone 15 → 15 bots, zone 1 → 1 bot
	}
	got := formatZoneDistribution(counts)
	if len(got) != 10 {
		t.Errorf("len: %d want 10 (capped)", len(got))
	}
	if got[0].ZoneID != 15 || got[0].Count != 15 {
		t.Errorf("top: %+v want zone 15", got[0])
	}
	// Tail of top-10 should be zone 6 (15-9=6, since 15..6 = 10 zones).
	if got[9].ZoneID != 6 {
		t.Errorf("tail: %+v want zone 6", got[9])
	}
}

// TestCollectBotsFleetStatus_Empty — the no-bots case. Important: response
// shape must still contain the distribution arrays (empty), so consumers can
// iterate them without nil-checking.
func TestCollectBotsFleetStatus_Empty(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectQuery(regexp.QuoteMeta("FROM `acore_characters`.`characters` c INNER JOIN `acore_auth`.`account` a")).
		WithArgs("RNDBOT%", int64(wowBotsFleetStatusMaxRoster)).
		WillReturnRows(sqlmock.NewRows([]string{"class", "level", "online", "zone"}))
	// Audit query still runs even with no roster — a quiet fleet may still have
	// historic audit rows. Return empty.
	mock.ExpectQuery(regexp.QuoteMeta("FROM `acore_characters`.`mod_ollama_chat_gateway_audit`")).
		WillReturnRows(sqlmock.NewRows([]string{"bot_guid", "total", "errors"}))

	out, err := collectBotsFleetStatus(context.Background(), db, "RNDBOT", 24*time.Hour, "24h", 10)
	if err != nil {
		t.Fatalf("collectBotsFleetStatus: %v", err)
	}
	if got := out["totalBots"]; got != 0 {
		t.Errorf("totalBots: %v want 0", got)
	}
	if got := out["onlineBots"]; got != 0 {
		t.Errorf("onlineBots: %v want 0", got)
	}
	cd, _ := out["classDistribution"].([]botClassBucket)
	if cd == nil || len(cd) != 0 {
		t.Errorf("classDistribution: %v want empty (non-nil)", out["classDistribution"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestCollectBotsFleetStatus_Golden walks the full happy path with three bots
// across two classes, one online, plus two errored entries in the audit table
// — one with a known character name, one orphan (no characters row → empty
// name, but still surfaces).
func TestCollectBotsFleetStatus_Golden(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectQuery(regexp.QuoteMeta("FROM `acore_characters`.`characters` c INNER JOIN `acore_auth`.`account` a")).
		WithArgs("RNDBOT%", int64(wowBotsFleetStatusMaxRoster)).
		WillReturnRows(sqlmock.NewRows([]string{"class", "level", "online", "zone"}).
			AddRow(1, 80, 1, 14).  // Warrior, online in Durotar
			AddRow(1, 75, 0, 0).   // Warrior, offline
			AddRow(8, 10, 1, 1519)) // Mage, online in Stormwind

	mock.ExpectQuery(regexp.QuoteMeta("FROM `acore_characters`.`mod_ollama_chat_gateway_audit`")).
		WillReturnRows(sqlmock.NewRows([]string{"bot_guid", "total", "errors"}).
			AddRow(int64(20007), 100, 17).
			AddRow(int64(99999), 50, 5)) // orphan — no row in characters

	mock.ExpectQuery(regexp.QuoteMeta("FROM `acore_characters`.`characters` WHERE guid IN (?,?)")).
		WithArgs(int64(20007), int64(99999)).
		WillReturnRows(sqlmock.NewRows([]string{"guid", "name"}).
			AddRow(int64(20007), "Botbo"))

	out, err := collectBotsFleetStatus(context.Background(), db, "RNDBOT", 24*time.Hour, "24h", 10)
	if err != nil {
		t.Fatalf("collectBotsFleetStatus: %v", err)
	}
	if got := out["totalBots"]; got != 3 {
		t.Errorf("totalBots: %v want 3", got)
	}
	if got := out["onlineBots"]; got != 2 {
		t.Errorf("onlineBots: %v want 2", got)
	}
	if got := out["offlineBots"]; got != 1 {
		t.Errorf("offlineBots: %v want 1", got)
	}

	cd, _ := out["classDistribution"].([]botClassBucket)
	if len(cd) != 2 || cd[0].Class != 1 || cd[0].Count != 2 || cd[1].Class != 8 {
		t.Errorf("classDistribution: %+v want Warrior(2), Mage(1)", cd)
	}

	ld, _ := out["levelDistribution"].([]botLevelBucket)
	// Expect 3 buckets in increasing order: 10-19 (the lvl 10 Mage), 70-79 (the
	// 75 Warrior), 80 (the 80 Warrior). The level cap gets its own bucket.
	if len(ld) != 3 {
		t.Fatalf("levelDistribution len: %d want 3 (got %+v)", len(ld), ld)
	}
	if ld[0].Bucket != "10-19" || ld[1].Bucket != "70-79" || ld[2].Bucket != "80" {
		t.Errorf("levelDistribution buckets: %+v", ld)
	}

	zd, _ := out["topZones"].([]botZoneBucket)
	if len(zd) != 2 {
		t.Errorf("topZones len: %d want 2 (online-only)", len(zd))
	}

	te, _ := out["topErrored"].(map[string]any)
	if te == nil {
		t.Fatalf("topErrored missing")
	}
	if w, _ := te["window"].(string); w != "24h" {
		t.Errorf("topErrored.window: %q want 24h", w)
	}
	bots, _ := te["bots"].([]*botErroredRow)
	if len(bots) != 2 {
		t.Fatalf("topErrored.bots len: %d want 2", len(bots))
	}
	if bots[0].BotGuid != 20007 || bots[0].Name != "Botbo" || bots[0].ErrorCount != 17 {
		t.Errorf("topErrored[0]: %+v want guid=20007 name=Botbo errors=17", bots[0])
	}
	// Orphan stays in the list with empty name — operator can still investigate
	// why the GUID no longer resolves.
	if bots[1].BotGuid != 99999 || bots[1].Name != "" {
		t.Errorf("topErrored[1]: %+v want guid=99999 name='' (orphan)", bots[1])
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestCollectBotsFleetStatus_CustomPrefixAndTopN verifies the prefix flows
// through to the LIKE binding AND that topN clamps before hitting SQL.
func TestCollectBotsFleetStatus_CustomPrefixAndTopN(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectQuery(regexp.QuoteMeta("FROM `acore_characters`.`characters` c INNER JOIN `acore_auth`.`account` a")).
		WithArgs("AHBOT%", int64(wowBotsFleetStatusMaxRoster)).
		WillReturnRows(sqlmock.NewRows([]string{"class", "level", "online", "zone"}))
	mock.ExpectQuery(regexp.QuoteMeta("FROM `acore_characters`.`mod_ollama_chat_gateway_audit`")).
		WithArgs(sqlmock.AnyArg(), int64(5)).
		WillReturnRows(sqlmock.NewRows([]string{"bot_guid", "total", "errors"}))

	reg := NewRegistry()
	RegisterWowBotsFleetStatusTool(reg, DBDeps{QueryDB: db})
	tool, _ := reg.Get("wow_bots_fleet_status")
	resp := tool.Handler(context.Background(), json.RawMessage(`{"accountPrefix":"AHBOT","topN":5,"errorWindow":"1h"}`), "")
	if got, _ := resp.(map[string]any); got["error"] != nil {
		t.Fatalf("unexpected error: %v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestCollectBotsFleetStatus_TopNClampHandler — confirms an oversized topN
// gets clamped at the handler before reaching SQL (defense against a caller
// pasting topN=99999 hoping to dump the whole audit table).
func TestCollectBotsFleetStatus_TopNClampHandler(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectQuery(regexp.QuoteMeta("FROM `acore_characters`.`characters` c INNER JOIN `acore_auth`.`account` a")).
		WillReturnRows(sqlmock.NewRows([]string{"class", "level", "online", "zone"}))
	mock.ExpectQuery(regexp.QuoteMeta("FROM `acore_characters`.`mod_ollama_chat_gateway_audit`")).
		WithArgs(sqlmock.AnyArg(), int64(wowBotsFleetStatusMaxTopN)).
		WillReturnRows(sqlmock.NewRows([]string{"bot_guid", "total", "errors"}))

	reg := NewRegistry()
	RegisterWowBotsFleetStatusTool(reg, DBDeps{QueryDB: db})
	tool, _ := reg.Get("wow_bots_fleet_status")
	resp := tool.Handler(context.Background(), json.RawMessage(`{"topN":99999}`), "")
	if got, _ := resp.(map[string]any); got["error"] != nil {
		t.Fatalf("unexpected error: %v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}
