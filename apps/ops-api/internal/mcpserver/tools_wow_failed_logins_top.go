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
	wowFailedLoginsTimeout     = 10 * time.Second
	wowFailedLoginsDefaultTopN = 25
	wowFailedLoginsMaxTopN     = 200
	wowFailedLoginsDefaultMin  = 1 // surface only accounts with at least one failure
	wowFailedLoginsBotPrefix   = "RNDBOT"
)

// RegisterWowFailedLoginsTopTool registers `wow_failed_logins_top` — a read-only
// brute-force triage view: the accounts carrying the most outstanding
// acore_auth.account.failed_logins, ordered failed_logins desc.
//
// Counterpart to wow_recent_logins (PR #193): recent_logins answers "who logged
// in RECENTLY?" sorted by last_login desc and surfaces failed_logins as an
// incidental column; this tool inverts the lens — sort BY failed_logins so the
// most-targeted accounts float to the top without the operator scanning the
// whole roster. The killer use case during an outage / suspicious-traffic window
// is "is anyone hammering a GM account?", so the same account_access GM
// annotation is folded in (a brute-forced GM = the highest-severity signal).
//
// IMPORTANT semantic: account.failed_logins is a point-in-time counter that the
// auth server RESETS to 0 on the next successful login — so a high value means
// "this many failures since the last success (or since account creation if the
// account has never succeeded)", NOT a lifetime total. That's why there's no
// time window: an account that has ONLY ever failed has a NULL last_login and is
// the single most suspicious row; any window filter on last_login would hide it.
func RegisterWowFailedLoginsTopTool(reg *Registry, deps DBDeps) {
	reg.Register(Tool{
		Name: "wow_failed_logins_top",
		Description: "Brute-force triage view: the auth accounts carrying the most outstanding " +
			"failed_logins (acore_auth.account ordered by failed_logins desc). Returns {count, minFailed, " +
			"excludeBots, accounts:[{accountId, username, lastIp, lastLoginIso, online, failedLogins, " +
			"joinDateIso, expansion, gmLevel}]}. Two-stage composite over the ops_ro pool: SELECT the " +
			"top failed-login accounts from acore_auth.account, then batch-fetch GM rows from " +
			"account_access in one IN(...) query (RealmID = -1 wins over per-realm) so a brute-forced GM " +
			"account is flagged. Inverts wow_recent_logins (which sorts by last_login desc and shows " +
			"failedLogins incidentally) — this sorts BY failedLogins so the most-targeted accounts float " +
			"to the top. NOTE: failed_logins is a point-in-time counter the auth server RESETS on the " +
			"next successful login, so a high value = failures since the last success (or since signup if " +
			"never succeeded), not a lifetime total; an account that has only ever failed has a null " +
			"lastLoginIso and is the most suspicious row (no time window so it stays visible). Optional " +
			"filters: minFailed (default 1, clamps to >=1), excludeBots (default false; strips usernames " +
			"starting with \"RNDBOT\" per mod-playerbots' RandomBotAccountPrefix), topN (default 25, " +
			"max 200). Read-only, 10 s timeout.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{
			"minFailed":{"type":"integer","description":"Only surface accounts whose failed_logins is at least this value (default 1, clamps to >=1; raise to e.g. 10 to focus on serious brute-force)"},
			"excludeBots":{"type":"boolean","description":"Strip usernames starting with \"RNDBOT\" (mod-playerbots' default account prefix) so the roster shows only human accounts"},
			"topN":{"type":"integer","description":"Max rows (default 25, max 200)"}
		}}`),
		Annotations: AnnRead(),
		Handler: func(ctx context.Context, raw json.RawMessage, _ string) any {
			var a struct {
				MinFailed   int  `json:"minFailed"`
				ExcludeBots bool `json:"excludeBots"`
				TopN        int  `json:"topN"`
			}
			if err := json.Unmarshal(raw, &a); err != nil {
				return map[string]any{"error": "decode args: " + err.Error()}
			}
			if deps.QueryDB == nil {
				return map[string]any{"error": "ops_ro pool not configured (DB_DSN empty?)"}
			}
			topN := a.TopN
			if topN <= 0 {
				topN = wowFailedLoginsDefaultTopN
			}
			if topN > wowFailedLoginsMaxTopN {
				topN = wowFailedLoginsMaxTopN
			}
			minFailed := a.MinFailed
			if minFailed < wowFailedLoginsDefaultMin {
				minFailed = wowFailedLoginsDefaultMin
			}
			c, cancel := context.WithTimeout(ctx, wowFailedLoginsTimeout)
			defer cancel()
			out, err := collectFailedLoginsTop(c, deps.QueryDB, minFailed, a.ExcludeBots, topN)
			if err != nil {
				return map[string]any{"error": err.Error()}
			}
			return out
		},
	})
}

// failedLoginAccount mirrors the columns we surface from acore_auth.account.
// Struct (not a map) so tests compare typed fields without the
// int64-vs-float64 JSON-decode footgun the sibling wow_* tools also avoid.
// LastLoginISO is empty for accounts that have never logged in successfully
// (NULL last_login) — the pure-attack-target case.
type failedLoginAccount struct {
	AccountID    int64  `json:"accountId"`
	Username     string `json:"username"`
	LastIP       string `json:"lastIp"`
	LastLoginISO string `json:"lastLoginIso"`
	Online       bool   `json:"online"`
	FailedLogins int    `json:"failedLogins"`
	JoinDateISO  string `json:"joinDateIso"`
	Expansion    int    `json:"expansion"`
	GMLevel      int    `json:"gmLevel"`
}

// collectFailedLoginsTop runs the 2-stage composite. Split out so tests can
// drive it with sqlmock without going through the registry handler.
func collectFailedLoginsTop(ctx context.Context, db *sql.DB, minFailed int, excludeBots bool, topN int) (map[string]any, error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire conn: %w", err)
	}
	defer conn.Close()

	if _, err := conn.ExecContext(ctx, "USE `acore_auth`"); err != nil {
		return nil, fmt.Errorf("USE acore_auth: %w", err)
	}
	rows, err := scanFailedLoginAccounts(ctx, conn, minFailed, excludeBots, topN)
	if err != nil {
		return nil, err
	}
	if len(rows) > 0 {
		if err := annotateFailedLoginsGM(ctx, conn, rows); err != nil {
			return nil, err
		}
	}
	return map[string]any{
		"count":       len(rows),
		"minFailed":   minFailed,
		"excludeBots": excludeBots,
		"accounts":    rows,
	}, nil
}

// scanFailedLoginAccounts pulls the top failed-login roster from
// acore_auth.account. minFailed is the >= floor (already clamped to >=1 by the
// caller); topN caps the row count (already clamped to [1, wowFailedLoginsMaxTopN]).
//
// Deliberately NO `last_login IS NOT NULL` filter (unlike wow_recent_logins):
// an account that has only ever failed has a NULL last_login and is the single
// most suspicious row, so it must stay visible. ORDER BY failed_logins DESC is
// the dominant key; last_login DESC then id ASC are deterministic tiebreakers
// (MySQL sorts NULL last_login last under DESC — i.e. never-succeeded accounts
// sort after equal-count succeeded ones — and id ASC guarantees a total order so
// the golden test can't flake on ties).
func scanFailedLoginAccounts(ctx context.Context, conn *sql.Conn, minFailed int, excludeBots bool, topN int) ([]*failedLoginAccount, error) {
	const cols = "id, username, last_ip, last_login, online, joindate, failed_logins, expansion"
	q := "SELECT " + cols + " FROM `account` WHERE failed_logins >= ?"
	args := []any{minFailed}
	if excludeBots {
		q += " AND username NOT LIKE ?"
		args = append(args, wowFailedLoginsBotPrefix+"%")
	}
	q += " ORDER BY failed_logins DESC, last_login DESC, id ASC LIMIT ?"
	args = append(args, topN)
	rs, err := conn.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("account query: %w", err)
	}
	defer rs.Close()
	var out []*failedLoginAccount
	for rs.Next() {
		r := &failedLoginAccount{}
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

// annotateFailedLoginsGM batch-fetches account_access for the roster. The
// all-realms row (RealmID = -1) wins over per-realm rows — same precedence
// wow_player_lookup + wow_online_players + wow_recent_logins apply. We exploit
// ORDER BY + "first row per id wins" to avoid an extra GROUP BY round trip.
//
// Local IN(...) builder rather than reusing annotateRecentLoginsGM (typed to
// []*recentLogin) — sharing across struct types would need a generic helper or
// a type-shim slice; the local builder is the lower-drift choice for one extra
// call site, the same call the sibling tool made.
func annotateFailedLoginsGM(ctx context.Context, conn *sql.Conn, rows []*failedLoginAccount) error {
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
