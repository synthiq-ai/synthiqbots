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
	wowBotLookupTimeout       = 10 * time.Second
	wowBotLookupDefaultPrefix = "RNDBOT"
	wowBotLookupDefaultAudit  = 10
	wowBotLookupMaxAudit      = 50
)

// RegisterWowBotLookupTool registers `wow_bot_lookup` — a bot-flavored
// counterpart to wow_player_lookup. Same character/account/gm/bans block,
// plus a `bot` block that classifies the row via the auth-DB username prefix
// (RNDBOT% by default, matching mod-playerbots' RandomBotAccountPrefix) and a
// `recentAudit` block with the last-N rows of mod_ollama_chat_gateway_audit
// AND mod_ollama_chat_tactical_audit for that bot_guid.
//
// Operators previously assembled this from:
//
//	wow_player_lookup (name|guid)            // character + account + gm + bans
//	ops_audit_gateway (botGuid=…, limit=10)  // last gateway calls
//	ops_audit_tactical (botGuid=…, limit=10) // last tactical ticks
//	manual username-prefix check             // "is this actually a bot?"
//
// Surfaced as a wedge during PR #130 (wow_bots_fleet_status) — that tool gave
// a fleet snapshot, but per-bot triage still required the 3-4 calls above.
//
// Read-only over the ops_ro pool. Stays within acore_characters +
// acore_auth — the canonical playerbots_account_type lives in
// acore_playerbots, which ops_ro can't read (tracked separately in
// ops_grant_playerbots_ro). Username prefix is mod-playerbots' own default
// and what shows up in `wow_create_account` outputs, so operators recognize it.
func RegisterWowBotLookupTool(reg *Registry, deps DBDeps) {
	reg.Register(Tool{
		Name: "wow_bot_lookup",
		Description: "Composite bot snapshot: takes character `name` (exact) OR `guid` and returns " +
			"{character, account, gm, bans, bot, recentAudit} in one round trip. Same character/" +
			"account/gm/bans shape as wow_player_lookup, plus a `bot` block that flags whether the " +
			"owning account's username starts with `accountPrefix` (default \"RNDBOT\", matching " +
			"mod-playerbots' playerbots.RandomBotAccountPrefix) and a `recentAudit` block with the " +
			"last-N gateway and tactical audit rows for the bot_guid. Replaces the operator pattern " +
			"of wow_player_lookup + ops_audit_gateway(botGuid=…) + ops_audit_tactical(botGuid=…) + " +
			"manual prefix check when triaging a single misbehaving bot. Source for the bot flag is " +
			"the auth-DB username prefix (ops_ro lacks SELECT on acore_playerbots, so the canonical " +
			"playerbots_account_type table is unreachable). Read-only, 10 s timeout. Optional: " +
			"accountPrefix (default \"RNDBOT\"), auditLimit (default 10, max 50).",
		InputSchema: json.RawMessage(`{"type":"object","properties":{
			"name":{"type":"string","description":"Character name (exact match, case-insensitive via MySQL's default collation)"},
			"guid":{"type":"integer","description":"Character GUID (alternative to name)"},
			"accountPrefix":{"type":"string","description":"Username prefix that identifies bot accounts (default \"RNDBOT\"). Bare prefix only — case-insensitive comparison, no LIKE wildcards."},
			"auditLimit":{"type":"integer","description":"Max audit rows per table (default 10, max 50)"}
		}}`),
		Annotations: AnnRead(),
		Handler: func(ctx context.Context, raw json.RawMessage, _ string) any {
			var a struct {
				Name          string `json:"name"`
				Guid          int64  `json:"guid"`
				AccountPrefix string `json:"accountPrefix"`
				AuditLimit    int    `json:"auditLimit"`
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
			prefix := strings.TrimSpace(a.AccountPrefix)
			if prefix == "" {
				prefix = wowBotLookupDefaultPrefix
			}
			if strings.ContainsAny(prefix, "%_") {
				return map[string]any{"error": "accountPrefix must not contain LIKE wildcards ('%' or '_')"}
			}
			limit := a.AuditLimit
			if limit <= 0 {
				limit = wowBotLookupDefaultAudit
			}
			if limit > wowBotLookupMaxAudit {
				limit = wowBotLookupMaxAudit
			}
			c, cancel := context.WithTimeout(ctx, wowBotLookupTimeout)
			defer cancel()
			out, err := collectBotLookup(c, deps.QueryDB, a.Name, a.Guid, prefix, limit)
			if err != nil {
				return map[string]any{"error": err.Error()}
			}
			return out
		},
	})
}

// botFlag is the public shape of the bot-identification block. `source` is
// hard-coded to "auth-username-prefix" so callers can branch on it once a
// future fire adds a canonical acore_playerbots check.
type botFlag struct {
	IsBot         bool   `json:"isBot"`
	AccountPrefix string `json:"accountPrefix"`
	Source        string `json:"source"`
	Note          string `json:"note"`
}

// gatewayAuditRow mirrors the operator-facing columns from
// mod_ollama_chat_gateway_audit. id is omitted on purpose — operators don't
// reference it directly; ts + bot_guid are the identifying tuple.
type gatewayAuditRow struct {
	TsISO            string `json:"tsIso"`
	PlayerGuid       int64  `json:"playerGuid"`
	AccountID        int64  `json:"accountId"`
	RequestChars     int    `json:"requestChars"`
	ResponseChars    int    `json:"responseChars"`
	PromptTokens     int    `json:"promptTokens"`
	CompletionTokens int    `json:"completionTokens"`
	LatencyMs        int    `json:"latencyMs"`
	SourceChannel    string `json:"sourceChannel"`
	Error            bool   `json:"error"`
}

// tacticalAuditRow mirrors the operator-facing columns from
// mod_ollama_chat_tactical_audit. error / escalation_reason are NULLable in
// the schema — surfaced as empty strings when null so the JSON shape is stable.
type tacticalAuditRow struct {
	TsISO            string `json:"tsIso"`
	BotName          string `json:"botName"`
	Action           string `json:"action"`
	Result           string `json:"result"`
	Error            string `json:"error"`
	Escalated        bool   `json:"escalated"`
	EscalationReason string `json:"escalationReason"`
	LatencyMs        int    `json:"latencyMs"`
	InCombat         bool   `json:"inCombat"`
}

// collectBotLookup runs the composite over a single ops_ro connection: USE
// acore_characters for the character + character_ban + both audit tables, then
// USE acore_auth for the account + GM + account_ban. Same USE-switching shape
// collectPlayerLookup uses; we reuse its scan helpers verbatim so a future
// schema change touches one place.
func collectBotLookup(ctx context.Context, db *sql.DB, name string, guid int64, prefix string, auditLimit int) (map[string]any, error) {
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
	gateway, err := scanGatewayAuditForBot(ctx, conn, ch.Guid, auditLimit)
	if err != nil {
		return nil, err
	}
	tactical, err := scanTacticalAuditForBot(ctx, conn, ch.Guid, auditLimit)
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
		"bot": classifyBot(acct.Username, prefix),
		"recentAudit": map[string]any{
			"auditLimit": auditLimit,
			"gateway":    gateway,
			"tactical":   tactical,
		},
	}
	return out, nil
}

// classifyBot returns the bot-flag block. Case-insensitive prefix match —
// mod-playerbots uppercases its account names at creation but operators
// frequently store the prefix in lowercase ("rndbot") in their config notes.
func classifyBot(username, prefix string) botFlag {
	isBot := strings.HasPrefix(strings.ToUpper(username), strings.ToUpper(prefix))
	return botFlag{
		IsBot:         isBot,
		AccountPrefix: prefix,
		Source:        "auth-username-prefix",
		Note:          "ops_ro lacks SELECT on acore_playerbots; using auth-DB username prefix (mod-playerbots' RandomBotAccountPrefix default is \"RNDBOT\")",
	}
}

// scanGatewayAuditForBot pulls the last-N gateway-audit rows for a bot_guid
// ordered most-recent-first. Returns an empty slice (NOT nil) when there are
// no rows — JSON-encoding `nil` produces `null` which clients then have to
// branch on; an empty array is friendlier.
func scanGatewayAuditForBot(ctx context.Context, conn *sql.Conn, botGuid int64, limit int) ([]gatewayAuditRow, error) {
	q := "SELECT ts, player_guid, account_id, request_chars, response_chars, " +
		"prompt_tokens, completion_tokens, latency_ms, source_channel, error " +
		"FROM `mod_ollama_chat_gateway_audit` WHERE bot_guid = ? " +
		"ORDER BY ts DESC LIMIT ?"
	rows, err := conn.QueryContext(ctx, q, botGuid, limit)
	if err != nil {
		return nil, fmt.Errorf("gateway_audit query: %w", err)
	}
	defer rows.Close()
	out := make([]gatewayAuditRow, 0)
	for rows.Next() {
		var (
			r       gatewayAuditRow
			ts      time.Time
			errFlag int
		)
		if err := rows.Scan(&ts, &r.PlayerGuid, &r.AccountID, &r.RequestChars, &r.ResponseChars,
			&r.PromptTokens, &r.CompletionTokens, &r.LatencyMs, &r.SourceChannel, &errFlag); err != nil {
			return nil, fmt.Errorf("gateway_audit scan: %w", err)
		}
		r.TsISO = ts.UTC().Format(time.RFC3339)
		r.Error = errFlag != 0
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("gateway_audit iter: %w", err)
	}
	return out, nil
}

// scanTacticalAuditForBot pulls the last-N tactical-tick rows for a bot_guid.
// error / escalation_reason are NULLable in the schema — coerced to empty
// strings so the JSON shape is uniform regardless of which path the tick took.
func scanTacticalAuditForBot(ctx context.Context, conn *sql.Conn, botGuid int64, limit int) ([]tacticalAuditRow, error) {
	q := "SELECT ts, bot_name, action, result, error, escalated, escalation_reason, " +
		"latency_ms, in_combat " +
		"FROM `mod_ollama_chat_tactical_audit` WHERE bot_guid = ? " +
		"ORDER BY ts DESC LIMIT ?"
	rows, err := conn.QueryContext(ctx, q, botGuid, limit)
	if err != nil {
		return nil, fmt.Errorf("tactical_audit query: %w", err)
	}
	defer rows.Close()
	out := make([]tacticalAuditRow, 0)
	for rows.Next() {
		var (
			r                            tacticalAuditRow
			ts                           time.Time
			escalatedFlag, inCombatFlag  int
			errStr, escalationReasonStr  sql.NullString
		)
		if err := rows.Scan(&ts, &r.BotName, &r.Action, &r.Result, &errStr,
			&escalatedFlag, &escalationReasonStr, &r.LatencyMs, &inCombatFlag); err != nil {
			return nil, fmt.Errorf("tactical_audit scan: %w", err)
		}
		r.TsISO = ts.UTC().Format(time.RFC3339)
		r.Error = errStr.String
		r.Escalated = escalatedFlag != 0
		r.EscalationReason = escalationReasonStr.String
		r.InCombat = inCombatFlag != 0
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("tactical_audit iter: %w", err)
	}
	return out, nil
}
