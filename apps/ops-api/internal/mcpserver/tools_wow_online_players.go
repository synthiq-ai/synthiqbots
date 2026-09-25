package mcpserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

const (
	wowOnlinePlayersTimeout      = 10 * time.Second
	wowOnlinePlayersDefaultLimit = 100
	wowOnlinePlayersMaxLimit     = 500
)

// RegisterWowOnlinePlayersTool registers `wow_online_players` — a composite
// read-only tool that returns the roster of currently-online characters
// (characters.online = 1) joined with their owning account and GM access.
//
// Operators previously assembled this from at least two sequential calls —
// one db_query against acore_characters.characters, then per-player
// wow_player_lookup or another db_query against acore_auth. With dozens of
// online characters that fan-out becomes painful; this composite collapses
// it into one tool call.
//
// Use cases:
//   - "anyone online?" before a worldserver restart (count + names)
//   - "which GMs are logged in right now?" (gmOnly filter)
//   - "snapshot of the high-level roster" (minLevel filter + level-desc sort)
func RegisterWowOnlinePlayersTool(reg *Registry, deps DBDeps) {
	reg.Register(Tool{
		Name: "wow_online_players",
		Description: "Composite list of currently-online players (characters.online = 1). Returns " +
			"{count, players:[{guid, name, level, race, raceName, class, className, gender, zoneId, " +
			"mapId, accountId, username, lastIp, gmLevel}]}, ordered by level desc then name asc. " +
			"Two-stage composite over the ops_ro pool: SELECT online roster from " +
			"acore_characters.characters, then batch-fetch account + GM rows from acore_auth in two " +
			"IN(...) queries. Replaces the operator pattern of \"db_query online characters\" + per-" +
			"player db_query for account / GM. Use before restarts (\"anyone online?\"), for player " +
			"snapshots, or to find GMs currently logged in. Optional filters: minLevel, gmOnly, limit " +
			"(default 100, max 500). Read-only, 10 s timeout.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{
			"minLevel":{"type":"integer","description":"Filter to characters with level >= this (1-80)"},
			"gmOnly":{"type":"boolean","description":"Only return characters whose account has gmlevel > 0 (RealmID = -1 preferred over per-realm)"},
			"limit":{"type":"integer","description":"Max rows (default 100, max 500)"}
		}}`),
		Annotations: AnnRead(),
		Handler: func(ctx context.Context, raw json.RawMessage, _ string) any {
			var a struct {
				MinLevel int  `json:"minLevel"`
				GmOnly   bool `json:"gmOnly"`
				Limit    int  `json:"limit"`
			}
			if err := json.Unmarshal(raw, &a); err != nil {
				return map[string]any{"error": "decode args: " + err.Error()}
			}
			if deps.QueryDB == nil {
				return map[string]any{"error": "ops_ro pool not configured (DB_DSN empty?)"}
			}
			limit := a.Limit
			if limit <= 0 {
				limit = wowOnlinePlayersDefaultLimit
			}
			if limit > wowOnlinePlayersMaxLimit {
				limit = wowOnlinePlayersMaxLimit
			}
			c, cancel := context.WithTimeout(ctx, wowOnlinePlayersTimeout)
			defer cancel()
			out, err := collectOnlinePlayers(c, deps.QueryDB, a.MinLevel, a.GmOnly, limit)
			if err != nil {
				return map[string]any{"error": err.Error()}
			}
			return out
		},
	})
}

// onlinePlayer mirrors the fields we surface for each online character. Kept
// as a struct (not a map) so tests can compare typed fields without the
// int64-vs-float64 JSON-decode footgun wow_player_lookup also avoids.
type onlinePlayer struct {
	Guid      int64  `json:"guid"`
	Name      string `json:"name"`
	AccountID int64  `json:"accountId"`
	Race      int    `json:"race"`
	RaceName  string `json:"raceName"`
	Class     int    `json:"class"`
	ClassName string `json:"className"`
	Gender    int    `json:"gender"`
	Level     int    `json:"level"`
	Zone      int    `json:"zoneId"`
	Map       int    `json:"mapId"`
	Username  string `json:"username"`
	LastIP    string `json:"lastIp"`
	GMLevel   int    `json:"gmLevel"`
}

// collectOnlinePlayers runs the 2-stage composite. Split out so tests can
// drive it with sqlmock without going through the registry handler.
func collectOnlinePlayers(ctx context.Context, db *sql.DB, minLevel int, gmOnly bool, limit int) (map[string]any, error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire conn: %w", err)
	}
	defer conn.Close()

	if _, err := conn.ExecContext(ctx, "USE `acore_characters`"); err != nil {
		return nil, fmt.Errorf("USE acore_characters: %w", err)
	}
	players, err := scanOnlineCharacters(ctx, conn, minLevel, limit)
	if err != nil {
		return nil, err
	}
	if len(players) == 0 {
		return map[string]any{"count": 0, "players": []*onlinePlayer{}}, nil
	}

	if _, err := conn.ExecContext(ctx, "USE `acore_auth`"); err != nil {
		return nil, fmt.Errorf("USE acore_auth: %w", err)
	}
	if err := annotateOnlineAccounts(ctx, conn, players); err != nil {
		return nil, err
	}
	if err := annotateOnlineGM(ctx, conn, players); err != nil {
		return nil, err
	}

	// gmOnly applies after GM-level lookup so we can use the actual value.
	if gmOnly {
		filtered := players[:0]
		for _, p := range players {
			if p.GMLevel > 0 {
				filtered = append(filtered, p)
			}
		}
		players = filtered
	}

	return map[string]any{"count": len(players), "players": players}, nil
}

// scanOnlineCharacters pulls the online roster from acore_characters.characters.
// minLevel == 0 disables the level floor; limit caps the row count (already
// clamped to [1, wowOnlinePlayersMaxLimit] by the caller).
func scanOnlineCharacters(ctx context.Context, conn *sql.Conn, minLevel, limit int) ([]*onlinePlayer, error) {
	const cols = "guid, account, name, race, class, gender, level, zone, map"
	q := "SELECT " + cols + " FROM `characters` WHERE online = 1"
	args := []any{}
	if minLevel > 0 {
		q += " AND level >= ?"
		args = append(args, minLevel)
	}
	q += " ORDER BY level DESC, name ASC LIMIT ?"
	args = append(args, limit)
	rows, err := conn.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("characters query: %w", err)
	}
	defer rows.Close()
	var out []*onlinePlayer
	for rows.Next() {
		p := &onlinePlayer{}
		if err := rows.Scan(&p.Guid, &p.AccountID, &p.Name, &p.Race, &p.Class, &p.Gender, &p.Level, &p.Zone, &p.Map); err != nil {
			return nil, fmt.Errorf("characters scan: %w", err)
		}
		p.RaceName = wowRaceName(p.Race)
		p.ClassName = wowClassName(p.Class)
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("characters iter: %w", err)
	}
	return out, nil
}

// annotateOnlineAccounts batch-fetches account.username / last_ip for the
// roster using a single IN(...) query. Accounts that no longer exist (orphan
// characters) leave the username + lastIp fields empty — same friendly-
// degradation choice wow_player_lookup makes for the single-character path.
func annotateOnlineAccounts(ctx context.Context, conn *sql.Conn, players []*onlinePlayer) error {
	if len(players) == 0 {
		return nil
	}
	q, args := buildAccountIDInQuery("SELECT id, username, last_ip FROM `account` WHERE id IN ", players)
	rows, err := conn.QueryContext(ctx, q, args...)
	if err != nil {
		return fmt.Errorf("account query: %w", err)
	}
	defer rows.Close()
	type acctRow struct {
		username string
		lastIP   string
	}
	accts := make(map[int64]acctRow, len(players))
	for rows.Next() {
		var id int64
		var username, lastIP string
		if err := rows.Scan(&id, &username, &lastIP); err != nil {
			return fmt.Errorf("account scan: %w", err)
		}
		accts[id] = acctRow{username, lastIP}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("account iter: %w", err)
	}
	for _, p := range players {
		if a, ok := accts[p.AccountID]; ok {
			p.Username = a.username
			p.LastIP = a.lastIP
		}
	}
	return nil
}

// annotateOnlineGM batch-fetches account_access for the roster. The
// all-realms row (RealmID = -1) wins over per-realm rows — same precedence
// wow_player_lookup applies for the single-character path. We exploit ORDER
// BY + "first row per id wins" to avoid an extra GROUP BY round trip.
func annotateOnlineGM(ctx context.Context, conn *sql.Conn, players []*onlinePlayer) error {
	if len(players) == 0 {
		return nil
	}
	q, args := buildAccountIDInQuery("SELECT id, gmlevel, RealmID FROM `account_access` WHERE id IN ", players)
	q += " ORDER BY RealmID = -1 DESC, gmlevel DESC"
	rows, err := conn.QueryContext(ctx, q, args...)
	if err != nil {
		return fmt.Errorf("account_access query: %w", err)
	}
	defer rows.Close()
	gm := make(map[int64]int, len(players))
	for rows.Next() {
		var id int64
		var level, realm int
		if err := rows.Scan(&id, &level, &realm); err != nil {
			return fmt.Errorf("account_access scan: %w", err)
		}
		if _, seen := gm[id]; !seen {
			gm[id] = level
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("account_access iter: %w", err)
	}
	for _, p := range players {
		if lvl, ok := gm[p.AccountID]; ok {
			p.GMLevel = lvl
		}
	}
	return nil
}

// buildAccountIDInQuery composes "<prefix>(?,?,?...)" alongside the matching
// args slice. Shared by the account + account_access fan-outs so they emit
// the placeholder list identically (and tests can match it with one regex).
func buildAccountIDInQuery(prefix string, players []*onlinePlayer) (string, []any) {
	var b strings.Builder
	b.Grow(len(prefix) + len(players)*2 + 2)
	b.WriteString(prefix)
	b.WriteByte('(')
	args := make([]any, 0, len(players))
	for i, p := range players {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteByte('?')
		args = append(args, p.AccountID)
	}
	b.WriteByte(')')
	return b.String(), args
}
