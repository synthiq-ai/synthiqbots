package mcpserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

const (
	wowGuildRosterTimeout    = 10 * time.Second
	wowGuildRosterDefaultTop = 50
	wowGuildRosterMaxTop     = 500
	wowGuildRosterDaySeconds = 86400 // whole-day divisor for inactiveDays
)

// guildRosterHeader is the resolved guild the roster belongs to. leaderGuid is
// characters.guid of the guild master (guild.leaderguid) — used to flag the GM in
// the member list. createDate is the guild's founding unix time.
type guildRosterHeader struct {
	GuildID       int64  `json:"guildId"`
	Name          string `json:"name"`
	LeaderGuid    int64  `json:"leaderGuid"`
	CreateDate    int64  `json:"createDate"`
	CreateDateISO string `json:"createDateISO,omitempty"`
}

// guildRosterMember is one guild member joined to their character row. guid is
// characters.guid. rankName comes from guild_rank via a LEFT JOIN so a missing rank
// row surfaces as rank-<id> rather than dropping the member. inactiveDays is whole
// days since logout_time (0 for an online member, 0/unknown for a never-logged-out
// logout_time=0 row — NOT a bogus 40-year age from the epoch).
type guildRosterMember struct {
	Guid          int64  `json:"guid"`
	Name          string `json:"name"`
	Race          int    `json:"race"`
	RaceName      string `json:"raceName"`
	Class         int    `json:"class"`
	ClassName     string `json:"className"`
	Gender        int    `json:"gender"`
	Level         int    `json:"level"`
	RankID        int    `json:"rankId"`
	RankName      string `json:"rankName"`
	IsGuildMaster bool   `json:"isGuildMaster"`
	Online        bool   `json:"online"`
	LogoutTime    int64  `json:"logoutTime"`
	LogoutTimeISO string `json:"logoutTimeISO,omitempty"`
	InactiveDays  int64  `json:"inactiveDays"`
	InactiveHuman string `json:"inactiveHuman,omitempty"`
	ZoneID        int    `json:"zoneId"`
	AccountID     int64  `json:"accountId"`
	PublicNote    string `json:"publicNote,omitempty"`
	OfficerNote   string `json:"officerNote,omitempty"`
}

// RegisterWowGuildRosterTool registers `wow_guild_roster` — a read-only, per-guild
// member drilldown over the ops_ro pool. It is the guild-side companion to
// wow_player_lookup (single character): resolve a guild by id or name and return its
// full membership joined to the character rows, ordered by rank.
//
// No merged ops-api tool reads the guild tables at all — get_bot_guild_info /
// get_guild_bank_log / get_guild_event_log are bot-facing (in-process, a single bot's
// perspective), not an operator query over acore_characters. This opens the ops-facing
// guild sub-family with the roster drilldown, and folds in an inactivity lens
// (minInactiveDays) so an operator can isolate the dormant/abandoned members the way
// wow_mail_unread isolates never-opened mail.
func RegisterWowGuildRosterTool(reg *Registry, deps DBDeps) {
	reg.Register(Tool{
		Name: "wow_guild_roster",
		Description: "Per-guild member roster drilldown over acore_characters — joins guild, guild_member, " +
			"guild_rank and characters on the ops_ro pool to return a guild's full membership in one round trip, " +
			"the guild-side companion to wow_player_lookup (single character). Resolve the guild by guildId OR " +
			"guildName (exactly one; a missing guild returns found:false). Each member carries guid, name, " +
			"race/raceName, class/className, gender, level, rankId + rankName (from guild_rank; a missing rank row " +
			"surfaces as rank-<id>), isGuildMaster (guid == the guild's leaderguid), online, logout_time + " +
			"logoutTimeISO, inactiveDays + inactiveHuman (whole days since logout_time — 0 for an online member, " +
			"0/unknown for a never-logged-out logout_time=0 row), zoneId, accountId, and publicNote/officerNote. " +
			"Ordered by guild rank ascending (Guild Master first), then level descending, then guid ascending — the " +
			"organizational roster order (no Go re-sort; the SQL ORDER BY is the source of truth). Headline counts are " +
			"honest and computed over the FULL guild: totalMembers, onlineMembers, and matchedMembers (members matching " +
			"the minInactiveDays filter, pre-limit) plus truncated. Args: guildId (positive integer) or guildName " +
			"(exact), topN (default 50, max 500), minInactiveDays (optional, default 0 = whole roster — only offline " +
			"members whose logout_time is more than N days old, isolating dormant/abandoned members the way " +
			"wow_mail_unread isolates never-opened mail; negative clamps to 0). Read-only, 10s timeout.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{
			"guildId":{"type":"integer","description":"Guild id (acore_characters.guild.guildid); alternative to guildName"},
			"guildName":{"type":"string","description":"Guild name (exact match); alternative to guildId"},
			"topN":{"type":"integer","description":"Max members returned, rank ascending then level descending (default 50, max 500)"},
			"minInactiveDays":{"type":"integer","description":"Only offline members whose logout_time is more than N days old (default 0 = whole roster); negative clamps to 0"}
		}}`),
		Annotations: AnnRead(),
		Handler: func(ctx context.Context, raw json.RawMessage, _ string) any {
			var a struct {
				GuildID         int64  `json:"guildId"`
				GuildName       string `json:"guildName"`
				TopN            int    `json:"topN"`
				MinInactiveDays int    `json:"minInactiveDays"`
			}
			if len(raw) > 0 && string(raw) != "null" {
				if err := json.Unmarshal(raw, &a); err != nil {
					return map[string]any{"error": "decode args: " + err.Error()}
				}
			}
			// Exactly one selector — a bad/absent guild is a typo, not "all guilds".
			if a.GuildID == 0 && a.GuildName == "" {
				return map[string]any{"error": "either guildId or guildName is required"}
			}
			if a.GuildID != 0 && a.GuildName != "" {
				return map[string]any{"error": "pass either guildId or guildName, not both"}
			}
			if a.GuildID < 0 {
				return map[string]any{"error": "guildId must be a positive integer"}
			}
			if deps.QueryDB == nil {
				return map[string]any{"error": "ops_ro pool not configured (DB_DSN empty?)"}
			}
			topN := a.TopN
			if topN <= 0 {
				topN = wowGuildRosterDefaultTop
			}
			if topN > wowGuildRosterMaxTop {
				topN = wowGuildRosterMaxTop
			}
			// minInactiveDays is a numeric threshold (like topN): a negative value is
			// clamped to 0 (whole roster) rather than rejected — asking for members
			// dormant a negative number of days is nonsensical, not a typo to bounce.
			minInactiveDays := a.MinInactiveDays
			if minInactiveDays < 0 {
				minInactiveDays = 0
			}
			c, cancel := context.WithTimeout(ctx, wowGuildRosterTimeout)
			defer cancel()
			out, err := collectWowGuildRoster(c, deps.QueryDB, time.Now(), a.GuildID, a.GuildName, topN, minInactiveDays)
			if err != nil {
				return map[string]any{"error": err.Error()}
			}
			return out
		},
	})
}

// collectWowGuildRoster resolves the guild, then runs the honest aggregate, the
// optional matched-count, and the roster list over the same per-call connection
// (USE acore_characters once — the wow_player_lookup pattern). Split out so tests can
// drive it with sqlmock and a fixed `now` (inactiveDays and the minInactiveDays cutoff
// are now-relative). The SQL ORDER BY is the source of truth for row order.
func collectWowGuildRoster(ctx context.Context, db *sql.DB, now time.Time, guildID int64, guildName string, topN int, minInactiveDays int) (map[string]any, error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire conn: %w", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "USE `acore_characters`"); err != nil {
		return nil, fmt.Errorf("USE acore_characters: %w", err)
	}

	// ---- resolve the guild (by id or name) ----
	hdr, err := scanGuildHeader(ctx, conn, guildID, guildName)
	if err != nil {
		return nil, err
	}
	if hdr == nil {
		// Structured not-found rather than an error — operators typo guild names, and
		// an explicit found:false is friendlier than parsing an error string.
		key := "guildName"
		val := any(guildName)
		if guildID != 0 {
			key, val = "guildId", any(guildID)
		}
		return map[string]any{"found": false, "lookupBy": key, "lookupValue": val}, nil
	}
	if hdr.CreateDate > 0 {
		hdr.CreateDateISO = formatUnixISO(hdr.CreateDate)
	}

	nowUnix := now.Unix()

	// ---- honest headline counts over the FULL guild (unfiltered) ----
	// COUNT(*) over the guild_member JOIN characters == members with a live character
	// row; SUM(online=1) == currently connected. Both are pre-filter/pre-limit so the
	// roster page is read against the true guild size.
	var totalMembers, onlineMembers int64
	if err := conn.QueryRowContext(ctx,
		"SELECT COUNT(*), COALESCE(SUM(c.online = 1), 0) "+
			"FROM `guild_member` gm JOIN `characters` c ON c.guid = gm.guid "+
			"WHERE gm.guildid = ?", hdr.GuildID).Scan(&totalMembers, &onlineMembers); err != nil {
		return nil, fmt.Errorf("guild-roster aggregate: %w", err)
	}

	// ---- optional inactivity filter, shared by the matched-count and the list ----
	// cutoff = now - minInactiveDays days. An offline member whose logout_time is at or
	// before the cutoff has been dormant at least that long. online members and
	// logout_time=0 (never logged out) rows are excluded by the filter.
	cutoff := nowUnix - int64(minInactiveDays)*wowGuildRosterDaySeconds
	filter := ""
	var filterArgs []any
	if minInactiveDays > 0 {
		filter = " AND c.online = 0 AND c.logout_time > 0 AND c.logout_time <= ?"
		filterArgs = append(filterArgs, cutoff)
	}

	// matchedMembers = members matching the filter, pre-limit — the honest denominator
	// for truncated. With no filter it equals totalMembers, so skip the extra round trip.
	matchedMembers := totalMembers
	if minInactiveDays > 0 {
		countArgs := append([]any{hdr.GuildID}, filterArgs...)
		if err := conn.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM `guild_member` gm JOIN `characters` c ON c.guid = gm.guid "+
				"WHERE gm.guildid = ?"+filter, countArgs...).Scan(&matchedMembers); err != nil {
			return nil, fmt.Errorf("guild-roster matched-count: %w", err)
		}
	}

	// ---- roster list ----
	listArgs := append([]any{hdr.GuildID}, filterArgs...)
	listArgs = append(listArgs, topN)
	rows, err := conn.QueryContext(ctx,
		"SELECT gm.guid, c.name, c.race, c.class, c.gender, c.level, c.online, c.logout_time, c.zone, "+
			"c.account, gm.rank, gr.rname, gm.pnote, gm.offnote "+
			"FROM `guild_member` gm "+
			"JOIN `characters` c ON c.guid = gm.guid "+
			"LEFT JOIN `guild_rank` gr ON gr.guildid = gm.guildid AND gr.rid = gm.rank "+
			"WHERE gm.guildid = ?"+filter+
			" ORDER BY gm.rank ASC, c.level DESC, gm.guid ASC LIMIT ?", listArgs...)
	if err != nil {
		return nil, fmt.Errorf("guild-roster query: %w", err)
	}
	defer rows.Close()

	members := make([]*guildRosterMember, 0, topN)
	for rows.Next() {
		var (
			m          guildRosterMember
			online     int
			logoutTime sql.NullInt64
			rankName   sql.NullString
			pnote      sql.NullString
			offnote    sql.NullString
		)
		if err := rows.Scan(&m.Guid, &m.Name, &m.Race, &m.Class, &m.Gender, &m.Level,
			&online, &logoutTime, &m.ZoneID, &m.AccountID, &m.RankID, &rankName, &pnote, &offnote); err != nil {
			return nil, fmt.Errorf("guild-roster scan: %w", err)
		}
		m.RaceName = wowRaceName(m.Race)
		m.ClassName = wowClassName(m.Class)
		m.Online = online != 0
		m.IsGuildMaster = m.Guid == hdr.LeaderGuid
		if rankName.Valid && rankName.String != "" {
			m.RankName = rankName.String
		} else {
			m.RankName = fmt.Sprintf("rank-%d", m.RankID)
		}
		if logoutTime.Valid {
			m.LogoutTime = logoutTime.Int64
			m.LogoutTimeISO = formatUnixISO(logoutTime.Int64)
		}
		// inactiveDays: online members are active now (0); an offline member with a real
		// logout_time gets whole days since (clamped >= 0 against clock skew); a 0
		// logout_time (never logged out) stays 0/"" (unknown) rather than reporting a
		// bogus multi-decade age measured from the unix epoch.
		if !m.Online && m.LogoutTime > 0 {
			inactiveSec := nowUnix - m.LogoutTime
			if inactiveSec < 0 {
				inactiveSec = 0
			}
			m.InactiveDays = inactiveSec / wowGuildRosterDaySeconds
			m.InactiveHuman = formatDuration(inactiveSec)
		}
		m.PublicNote = pnote.String
		m.OfficerNote = offnote.String
		members = append(members, &m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("guild-roster iter: %w", err)
	}

	return map[string]any{
		"asOf":            now.UTC().Format(time.RFC3339),
		"found":           true,
		"guild":           hdr,
		"totalMembers":    totalMembers,
		"onlineMembers":   onlineMembers,
		"matchedMembers":  matchedMembers,
		"topN":            topN,
		"minInactiveDays": minInactiveDays,
		"returned":        len(members),
		"truncated":       matchedMembers > int64(len(members)),
		"members":         members,
	}, nil
}

// scanGuildHeader resolves a guild by guildid or name. Returns (nil, nil) on no row —
// the caller turns that into a structured found:false response.
func scanGuildHeader(ctx context.Context, conn *sql.Conn, guildID int64, guildName string) (*guildRosterHeader, error) {
	const cols = "guildid, name, leaderguid, createdate"
	var row *sql.Row
	if guildID != 0 {
		row = conn.QueryRowContext(ctx, "SELECT "+cols+" FROM `guild` WHERE guildid = ?", guildID)
	} else {
		row = conn.QueryRowContext(ctx, "SELECT "+cols+" FROM `guild` WHERE name = ?", guildName)
	}
	var h guildRosterHeader
	err := row.Scan(&h.GuildID, &h.Name, &h.LeaderGuid, &h.CreateDate)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("guild scan: %w", err)
	}
	return &h, nil
}
