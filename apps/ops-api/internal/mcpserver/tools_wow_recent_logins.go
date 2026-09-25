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
	wowRecentLoginsTimeout      = 10 * time.Second
	wowRecentLoginsDefaultLimit = 50
	wowRecentLoginsMaxLimit     = 200
	wowRecentLoginsMaxSinceHrs  = 168 // 7d cap, matches the container_logs_grep family
	wowRecentLoginsBotPrefix    = "RNDBOT"
)

// RegisterWowRecentLoginsTool registers `wow_recent_logins` — a read-only
// roster of the most-recently-seen auth accounts, ordered by last_login desc.
//
// Operators previously assembled this from a db_query against
// acore_auth.account + per-account db_query against account_access. With dozens
// of recent accounts that fan-out becomes painful; this composite collapses
// it into one call.
//
// Companion to `wow_online_players` (PR #125): online_players answers "who is
// on RIGHT NOW", recent_logins answers "who was on RECENTLY?" (the natural
// follow-up after a worldserver restart, or for triaging "did $user log in
// after the outage?"). Surfaces failed_logins so brute-force noise is visible
// without a separate db_query.
func RegisterWowRecentLoginsTool(reg *Registry, deps DBDeps) {
	reg.Register(Tool{
		Name: "wow_recent_logins",
		Description: "Composite list of the most-recently-seen auth accounts (acore_auth.account " +
			"ordered by last_login desc). Returns {count, sinceHours, excludeBots, accounts:[{accountId, " +
			"username, lastIp, lastLoginIso, online, joinDateIso, failedLogins, expansion, gmLevel}]}. " +
			"Two-stage composite over the ops_ro pool: SELECT recent accounts from acore_auth.account, " +
			"then batch-fetch GM rows from account_access in one IN(...) query (RealmID = -1 wins over " +
			"per-realm). Companion to wow_online_players (who is on RIGHT NOW) — wow_recent_logins " +
			"answers \"who was on RECENTLY?\" (post-restart triage, \"did $user log in after the " +
			"outage?\"). Surfaces failedLogins so brute-force noise is visible without a separate " +
			"db_query. Optional filters: sinceHours (default 0 = unbounded, max 168 = 7d), " +
			"excludeBots (default false; strips usernames starting with \"RNDBOT\" per mod-playerbots' " +
			"playerbots.RandomBotAccountPrefix), limit (default 50, max 200). Read-only, 10 s timeout.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{
			"sinceHours":{"type":"integer","description":"Filter to accounts whose last_login is within the last N hours (default 0 = unbounded, max 168 = 7d)"},
			"excludeBots":{"type":"boolean","description":"Strip usernames starting with \"RNDBOT\" (mod-playerbots' default account prefix) so the roster shows only human accounts"},
			"limit":{"type":"integer","description":"Max rows (default 50, max 200)"}
		}}`),
		Annotations: AnnRead(),
		Handler: func(ctx context.Context, raw json.RawMessage, _ string) any {
			var a struct {
				SinceHours  int  `json:"sinceHours"`
				ExcludeBots bool `json:"excludeBots"`
				Limit       int  `json:"limit"`
			}
			if err := json.Unmarshal(raw, &a); err != nil {
				return map[string]any{"error": "decode args: " + err.Error()}
			}
			if deps.QueryDB == nil {
				return map[string]any{"error": "ops_ro pool not configured (DB_DSN empty?)"}
			}
			limit := a.Limit
			if limit <= 0 {
				limit = wowRecentLoginsDefaultLimit
			}
			if limit > wowRecentLoginsMaxLimit {
				limit = wowRecentLoginsMaxLimit
			}
			sinceHours := a.SinceHours
			if sinceHours < 0 {
				sinceHours = 0
			}
			if sinceHours > wowRecentLoginsMaxSinceHrs {
				sinceHours = wowRecentLoginsMaxSinceHrs
			}
			c, cancel := context.WithTimeout(ctx, wowRecentLoginsTimeout)
			defer cancel()
			out, err := collectRecentLogins(c, deps.QueryDB, sinceHours, a.ExcludeBots, limit)
			if err != nil {
				return map[string]any{"error": err.Error()}
			}
			return out
		},
	})
}

// recentLogin mirrors the columns we surface from acore_auth.account. Struct
// (not a map) so tests can compare typed fields without the int64-vs-float64
// JSON-decode footgun the other wow_* tools also avoid.
type recentLogin struct {
	AccountID    int64  `json:"accountId"`
	Username     string `json:"username"`
	LastIP       string `json:"lastIp"`
	LastLoginISO string `json:"lastLoginIso"`
	Online       bool   `json:"online"`
	JoinDateISO  string `json:"joinDateIso"`
	FailedLogins int    `json:"failedLogins"`
	Expansion    int    `json:"expansion"`
	GMLevel      int    `json:"gmLevel"`
}

// collectRecentLogins runs the 2-stage composite. Split out so tests can drive
// it with sqlmock without going through the registry handler.
func collectRecentLogins(ctx context.Context, db *sql.DB, sinceHours int, excludeBots bool, limit int) (map[string]any, error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire conn: %w", err)
	}
	defer conn.Close()

	if _, err := conn.ExecContext(ctx, "USE `acore_auth`"); err != nil {
		return nil, fmt.Errorf("USE acore_auth: %w", err)
	}
	rows, err := scanRecentLoginAccounts(ctx, conn, sinceHours, excludeBots, limit)
	if err != nil {
		return nil, err
	}
	if len(rows) > 0 {
		if err := annotateRecentLoginsGM(ctx, conn, rows); err != nil {
			return nil, err
		}
	}
	return map[string]any{
		"count":       len(rows),
		"sinceHours":  sinceHours,
		"excludeBots": excludeBots,
		"accounts":    rows,
	}, nil
}

// scanRecentLoginAccounts pulls the recent-login roster from acore_auth.account.
// sinceHours == 0 disables the window floor (any account with a non-null
// last_login is eligible); limit caps the row count (already clamped to
// [1, wowRecentLoginsMaxLimit] by the caller).
//
// The window arg is a Go-side time.Time so the comparison stays in the driver-
// declared location (the ops_ro DSN sets loc=UTC) — same pattern
// wow_bots_fleet_status uses for its audit-window scan. WHERE last_login IS
// NOT NULL is always present so brand-new accounts that have never logged in
// don't surface at the top of the roster with NULL timestamps.
func scanRecentLoginAccounts(ctx context.Context, conn *sql.Conn, sinceHours int, excludeBots bool, limit int) ([]*recentLogin, error) {
	const cols = "id, username, last_ip, last_login, online, joindate, failed_logins, expansion"
	q := "SELECT " + cols + " FROM `account` WHERE last_login IS NOT NULL"
	args := []any{}
	if sinceHours > 0 {
		q += " AND last_login >= ?"
		args = append(args, time.Now().UTC().Add(-time.Duration(sinceHours)*time.Hour))
	}
	if excludeBots {
		q += " AND username NOT LIKE ?"
		args = append(args, wowRecentLoginsBotPrefix+"%")
	}
	q += " ORDER BY last_login DESC LIMIT ?"
	args = append(args, limit)
	rs, err := conn.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("account query: %w", err)
	}
	defer rs.Close()
	var out []*recentLogin
	for rs.Next() {
		r := &recentLogin{}
		var (
			online    int
			lastLogin sql.NullString
			joinDate  sql.NullString
		)
		if err := rs.Scan(&r.AccountID, &r.Username, &r.LastIP, &lastLogin, &online,
			&joinDate, &r.FailedLogins, &r.Expansion); err != nil {
			return nil, fmt.Errorf("account scan: %w", err)
		}
		r.Online = online != 0
		if lastLogin.Valid {
			r.LastLoginISO = lastLogin.String
		}
		if joinDate.Valid {
			r.JoinDateISO = joinDate.String
		}
		out = append(out, r)
	}
	if err := rs.Err(); err != nil {
		return nil, fmt.Errorf("account iter: %w", err)
	}
	return out, nil
}

// annotateRecentLoginsGM batch-fetches account_access for the roster. The
// all-realms row (RealmID = -1) wins over per-realm rows — same precedence
// wow_player_lookup + wow_online_players apply. We exploit ORDER BY + "first
// row per id wins" to avoid an extra GROUP BY round trip.
//
// Local IN(...) builder rather than reusing buildAccountIDInQuery (which is
// typed to []*onlinePlayer) — sharing across types would need either a generic
// helper or a type-shim slice; the 10-line local builder is the lower-drift
// choice for one extra call site.
func annotateRecentLoginsGM(ctx context.Context, conn *sql.Conn, rows []*recentLogin) error {
	if len(rows) == 0 {
		return nil
	}
	var b strings.Builder
	b.Grow(80 + len(rows)*2)
	b.WriteString("SELECT id, gmlevel, RealmID FROM `account_access` WHERE id IN (")
	args := make([]any, 0, len(rows))
	for i, r := range rows {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteByte('?')
		args = append(args, r.AccountID)
	}
	b.WriteString(") ORDER BY RealmID = -1 DESC, gmlevel DESC")
	rs, err := conn.QueryContext(ctx, b.String(), args...)
	if err != nil {
		return fmt.Errorf("account_access query: %w", err)
	}
	defer rs.Close()
	gm := make(map[int64]int, len(rows))
	for rs.Next() {
		var id int64
		var level, realm int
		if err := rs.Scan(&id, &level, &realm); err != nil {
			return fmt.Errorf("account_access scan: %w", err)
		}
		if _, seen := gm[id]; !seen {
			gm[id] = level
		}
	}
	if err := rs.Err(); err != nil {
		return fmt.Errorf("account_access iter: %w", err)
	}
	for _, r := range rows {
		if lvl, ok := gm[r.AccountID]; ok {
			r.GMLevel = lvl
		}
	}
	return nil
}
