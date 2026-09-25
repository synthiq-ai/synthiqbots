package mcpserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

const wowPlayerLookupTimeout = 10 * time.Second

// RegisterWowPlayerLookupTool registers `wow_player_lookup` — a composite
// read-only tool that resolves a character name OR guid into a single response
// containing the character row, the owning account, GM access, and any active
// bans (account- AND character-level).
//
// Operators previously assembled this from 3-5 sequential db_query calls:
//
//	SELECT … FROM acore_characters.characters WHERE name = ? / guid = ?
//	SELECT … FROM acore_auth.account WHERE id = ?
//	SELECT … FROM acore_auth.account_access WHERE id = ?
//	SELECT … FROM acore_auth.account_banned WHERE id = ? AND active = 1
//	SELECT … FROM acore_characters.character_banned WHERE guid = ? AND active = 1
//
// The tool runs all five against the same per-call connection, switching
// schemas with USE — same pattern db_table_info uses. Read-only (ops_ro pool),
// 10 s timeout. Race / class IDs are translated to names for readability;
// money is rendered both as raw copper and as g/s/c.
func RegisterWowPlayerLookupTool(reg *Registry, deps DBDeps) {
	reg.Register(Tool{
		Name: "wow_player_lookup",
		Description: "Composite player snapshot: takes character `name` (exact) OR `guid` and " +
			"returns {character, account, gm, bans} in one round trip. Joins " +
			"acore_characters.characters, acore_auth.account, account_access, account_banned, " +
			"and character_banned via per-call USE switches on the ops_ro pool. " +
			"Replaces the 3-5 sequential db_query calls operators run when investigating " +
			"a player report (\"who is X\", \"is Y banned\", \"what's Z's GM level\"). " +
			"Race/class IDs are translated to names; money is rendered as both raw copper " +
			"and gold/silver/copper. Read-only, 10 s timeout.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{
			"name":{"type":"string","description":"Character name (exact match, case-insensitive via MySQL's default collation)"},
			"guid":{"type":"integer","description":"Character GUID (alternative to name)"}
		}}`),
		Annotations: AnnRead(),
		Handler: func(ctx context.Context, raw json.RawMessage, _ string) any {
			var a struct {
				Name string `json:"name"`
				Guid int64  `json:"guid"`
			}
			if err := json.Unmarshal(raw, &a); err != nil {
				return map[string]any{"error": "decode args: " + err.Error()}
			}
			if a.Name == "" && a.Guid == 0 {
				return map[string]any{"error": "either name or guid is required"}
			}
			if a.Name != "" && a.Guid != 0 {
				return map[string]any{"error": "pass either name or guid, not both"}
			}
			if deps.QueryDB == nil {
				return map[string]any{"error": "ops_ro pool not configured (DB_DSN empty?)"}
			}
			c, cancel := context.WithTimeout(ctx, wowPlayerLookupTimeout)
			defer cancel()
			out, err := collectPlayerLookup(c, deps.QueryDB, a.Name, a.Guid)
			if err != nil {
				return map[string]any{"error": err.Error()}
			}
			return out
		},
	})
}

// playerCharacter mirrors the columns we surface from acore_characters.characters.
// Kept as a struct (not just a map) so the test golden can compare typed fields
// without the int64-vs-float64 JSON-decode footgun.
type playerCharacter struct {
	Guid           int64   `json:"guid"`
	Name           string  `json:"name"`
	AccountID      int64   `json:"accountId"`
	Race           int     `json:"race"`
	RaceName       string  `json:"raceName"`
	Class          int     `json:"class"`
	ClassName      string  `json:"className"`
	Gender         int     `json:"gender"`
	Level          int     `json:"level"`
	Online         bool    `json:"online"`
	Money          int64   `json:"moneyCopper"`
	MoneyHuman     string  `json:"moneyHuman"`
	TotalTimeSec   int64   `json:"totalTimeSec"`
	TotalTimeHuman string  `json:"totalTimeHuman"`
	LevelTimeSec   int64   `json:"levelTimeSec"`
	LogoutTime     int64   `json:"logoutTimeUnix"`
	LogoutTimeISO  string  `json:"logoutTimeIso"`
	Zone           int     `json:"zoneId"`
	Map            int     `json:"mapId"`
	PositionX      float64 `json:"positionX"`
	PositionY      float64 `json:"positionY"`
	PositionZ      float64 `json:"positionZ"`
}

type playerAccount struct {
	ID           int64  `json:"id"`
	Username     string `json:"username"`
	Email        string `json:"email"`
	LastIP       string `json:"lastIp"`
	LastLoginISO string `json:"lastLoginIso"`
	Locked       bool   `json:"locked"`
	Online       bool   `json:"online"`
	JoinDateISO  string `json:"joinDateIso"`
	Expansion    int    `json:"expansion"`
}

type playerGM struct {
	Level   int `json:"level"`
	RealmID int `json:"realmId"`
}

// playerBan is shared between the account-ban and character-ban shapes — the
// columns line up (acore mirrors the schema between the two tables).
type playerBan struct {
	BanDateUnix   int64  `json:"banDateUnix"`
	BanDateISO    string `json:"banDateIso"`
	UnbanDateUnix int64  `json:"unbanDateUnix"`
	UnbanDateISO  string `json:"unbanDateIso"`
	Permanent     bool   `json:"permanent"`
	BannedBy      string `json:"bannedBy"`
	BanReason     string `json:"banReason"`
}

// collectPlayerLookup runs the 5-statement composite. Split out so tests can
// exercise it with sqlmock without going through the registry handler.
func collectPlayerLookup(ctx context.Context, db *sql.DB, name string, guid int64) (map[string]any, error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire conn: %w", err)
	}
	defer conn.Close()

	// ---- characters DB ----
	if _, err := conn.ExecContext(ctx, "USE `acore_characters`"); err != nil {
		return nil, fmt.Errorf("USE acore_characters: %w", err)
	}
	ch, err := scanCharacter(ctx, conn, name, guid)
	if err != nil {
		return nil, err
	}
	if ch == nil {
		// No matching character. Return a structured "not found" rather than
		// erroring — operators frequently typo names and an explicit found:false
		// is friendlier than parsing an error string.
		key := "name"
		val := any(name)
		if guid != 0 {
			key, val = "guid", any(guid)
		}
		return map[string]any{"found": false, "lookupBy": key, "lookupValue": val}, nil
	}
	charBan, err := scanCharacterBan(ctx, conn, ch.Guid)
	if err != nil {
		return nil, err
	}

	// ---- auth DB ----
	if _, err := conn.ExecContext(ctx, "USE `acore_auth`"); err != nil {
		return nil, fmt.Errorf("USE acore_auth: %w", err)
	}
	acct, err := scanAccount(ctx, conn, ch.AccountID)
	if err != nil {
		return nil, err
	}
	gm, err := scanAccountAccess(ctx, conn, ch.AccountID)
	if err != nil {
		return nil, err
	}
	acctBan, err := scanAccountBan(ctx, conn, ch.AccountID)
	if err != nil {
		return nil, err
	}

	out := map[string]any{
		"found":     true,
		"character": ch,
		"account":   acct,
		"gm":        gm,
		"bans": map[string]any{
			"accountBanned":      acctBan != nil,
			"characterBanned":    charBan != nil,
			"activeAccountBan":   acctBan,
			"activeCharacterBan": charBan,
		},
	}
	return out, nil
}

// scanCharacter resolves either by name or guid. Returns (nil, nil) on no row
// — the caller turns that into a structured "not found" response.
func scanCharacter(ctx context.Context, conn *sql.Conn, name string, guid int64) (*playerCharacter, error) {
	const cols = "guid, account, name, race, class, gender, level, online, money, " +
		"totaltime, leveltime, logout_time, zone, map, position_x, position_y, position_z"
	var (
		row *sql.Row
	)
	if guid != 0 {
		row = conn.QueryRowContext(ctx, "SELECT "+cols+" FROM `characters` WHERE guid = ?", guid)
	} else {
		row = conn.QueryRowContext(ctx, "SELECT "+cols+" FROM `characters` WHERE name = ?", name)
	}
	var (
		ch         playerCharacter
		online     int
		logoutTime sql.NullInt64
	)
	err := row.Scan(
		&ch.Guid, &ch.AccountID, &ch.Name, &ch.Race, &ch.Class, &ch.Gender, &ch.Level,
		&online, &ch.Money, &ch.TotalTimeSec, &ch.LevelTimeSec, &logoutTime,
		&ch.Zone, &ch.Map, &ch.PositionX, &ch.PositionY, &ch.PositionZ,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("characters scan: %w", err)
	}
	ch.Online = online != 0
	ch.RaceName = wowRaceName(ch.Race)
	ch.ClassName = wowClassName(ch.Class)
	ch.MoneyHuman = formatMoney(ch.Money)
	ch.TotalTimeHuman = formatDuration(ch.TotalTimeSec)
	if logoutTime.Valid {
		ch.LogoutTime = logoutTime.Int64
		ch.LogoutTimeISO = formatUnixISO(logoutTime.Int64)
	}
	return &ch, nil
}

func scanCharacterBan(ctx context.Context, conn *sql.Conn, guid int64) (*playerBan, error) {
	row := conn.QueryRowContext(ctx,
		"SELECT bandate, unbandate, bannedby, banreason FROM `character_banned` "+
			"WHERE guid = ? AND active = 1 ORDER BY bandate DESC LIMIT 1", guid)
	return scanBanRow(row, "character_banned")
}

func scanAccount(ctx context.Context, conn *sql.Conn, id int64) (*playerAccount, error) {
	row := conn.QueryRowContext(ctx,
		"SELECT id, username, email, last_ip, last_login, locked, online, joindate, expansion "+
			"FROM `account` WHERE id = ?", id)
	var (
		a              playerAccount
		locked, online int
		lastLogin      sql.NullString
		joinDate       sql.NullString
		email          sql.NullString
	)
	err := row.Scan(&a.ID, &a.Username, &email, &a.LastIP, &lastLogin, &locked, &online, &joinDate, &a.Expansion)
	if err == sql.ErrNoRows {
		// Orphan character (account row deleted but characters row remained).
		// Surface a synthetic "missing" account so the rest of the response is
		// still useful to the operator — they can then go fix the orphan.
		return &playerAccount{ID: id, Username: "(account row missing)"}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("account scan: %w", err)
	}
	a.Email = email.String
	a.Locked = locked != 0
	a.Online = online != 0
	if lastLogin.Valid {
		a.LastLoginISO = lastLogin.String
	}
	if joinDate.Valid {
		a.JoinDateISO = joinDate.String
	}
	return &a, nil
}

func scanAccountAccess(ctx context.Context, conn *sql.Conn, id int64) (*playerGM, error) {
	// Match wow_set_gm_level's convention: "no row" == player (level 0) on the
	// all-realms entry. Prefer the all-realms row (-1) if present, otherwise
	// the highest-level row across realms.
	rows, err := conn.QueryContext(ctx,
		"SELECT gmlevel, RealmID FROM `account_access` WHERE id = ? ORDER BY RealmID = -1 DESC, gmlevel DESC",
		id)
	if err != nil {
		return nil, fmt.Errorf("account_access query: %w", err)
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("account_access iter: %w", err)
		}
		return &playerGM{Level: 0, RealmID: -1}, nil
	}
	var gm playerGM
	if err := rows.Scan(&gm.Level, &gm.RealmID); err != nil {
		return nil, fmt.Errorf("account_access scan: %w", err)
	}
	return &gm, nil
}

func scanAccountBan(ctx context.Context, conn *sql.Conn, id int64) (*playerBan, error) {
	row := conn.QueryRowContext(ctx,
		"SELECT bandate, unbandate, bannedby, banreason FROM `account_banned` "+
			"WHERE id = ? AND active = 1 ORDER BY bandate DESC LIMIT 1", id)
	return scanBanRow(row, "account_banned")
}

// scanBanRow is the shared scan path for account_banned and character_banned —
// both tables expose the same four columns we surface. Returns (nil, nil) on
// no active ban so the caller can set bans.accountBanned=false plainly.
func scanBanRow(row *sql.Row, tableForErr string) (*playerBan, error) {
	var (
		ban          playerBan
		bannedBy     sql.NullString
		banReason    sql.NullString
		bandate      int64
		unbandate    int64
	)
	err := row.Scan(&bandate, &unbandate, &bannedBy, &banReason)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("%s scan: %w", tableForErr, err)
	}
	ban.BanDateUnix = bandate
	ban.UnbanDateUnix = unbandate
	ban.BanDateISO = formatUnixISO(bandate)
	if unbandate > 0 {
		ban.UnbanDateISO = formatUnixISO(unbandate)
	}
	// AzerothCore convention: bandate == unbandate (or unbandate == 0) means
	// permanent. Some banning code paths set unbandate=bandate, others leave it
	// at 0; treat both as permanent.
	ban.Permanent = unbandate == 0 || unbandate == bandate
	ban.BannedBy = bannedBy.String
	ban.BanReason = banReason.String
	return &ban, nil
}

// formatMoney renders a copper amount as "Xg Ys Zc". WoW uses 100c = 1s,
// 100s = 1g, so 12345 copper = 1g 23s 45c. Negative amounts (shouldn't
// happen but the column is signed in some forks) get a leading minus.
func formatMoney(copper int64) string {
	if copper == 0 {
		return "0c"
	}
	neg := copper < 0
	if neg {
		copper = -copper
	}
	gold := copper / 10000
	silver := (copper % 10000) / 100
	cop := copper % 100
	sign := ""
	if neg {
		sign = "-"
	}
	switch {
	case gold > 0:
		return fmt.Sprintf("%s%dg %ds %dc", sign, gold, silver, cop)
	case silver > 0:
		return fmt.Sprintf("%s%ds %dc", sign, silver, cop)
	default:
		return fmt.Sprintf("%s%dc", sign, cop)
	}
}

// formatDuration renders a seconds count as "Xh Ym" — operators want at-a-glance
// time-played, not microsecond precision. Hours can run into the thousands on
// long-lived characters; print as-is rather than rolling into days.
func formatDuration(sec int64) string {
	if sec <= 0 {
		return "0m"
	}
	hours := sec / 3600
	minutes := (sec % 3600) / 60
	if hours == 0 {
		return fmt.Sprintf("%dm", minutes)
	}
	return fmt.Sprintf("%dh %dm", hours, minutes)
}

// formatUnixISO turns a unix-seconds epoch into RFC3339 UTC. Returns "" for 0
// — characters that have never logged out have logout_time=0 in the schema.
func formatUnixISO(sec int64) string {
	if sec <= 0 {
		return ""
	}
	return time.Unix(sec, 0).UTC().Format(time.RFC3339)
}

// wowRaceName maps the WotLK race ID (Player.h::Races) to its in-game name.
// Restricted to the 10 playable races in 3.3.5a — anything outside returns
// the raw integer as a fallback so a future expansion's race ID surfaces
// distinguishably instead of "Unknown".
func wowRaceName(id int) string {
	switch id {
	case 1:
		return "Human"
	case 2:
		return "Orc"
	case 3:
		return "Dwarf"
	case 4:
		return "Night Elf"
	case 5:
		return "Undead"
	case 6:
		return "Tauren"
	case 7:
		return "Gnome"
	case 8:
		return "Troll"
	case 10:
		return "Blood Elf"
	case 11:
		return "Draenei"
	default:
		return fmt.Sprintf("race_%d", id)
	}
}

// wowClassName maps the WotLK class ID (Player.h::Classes) to its in-game
// name. Same fallback strategy as wowRaceName.
func wowClassName(id int) string {
	switch id {
	case 1:
		return "Warrior"
	case 2:
		return "Paladin"
	case 3:
		return "Hunter"
	case 4:
		return "Rogue"
	case 5:
		return "Priest"
	case 6:
		return "Death Knight"
	case 7:
		return "Shaman"
	case 8:
		return "Mage"
	case 9:
		return "Warlock"
	case 11:
		return "Druid"
	default:
		return fmt.Sprintf("class_%d", id)
	}
}
