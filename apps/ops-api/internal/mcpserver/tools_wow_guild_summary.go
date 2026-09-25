package mcpserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

const (
	wowGuildSummaryTimeout    = 10 * time.Second
	wowGuildSummaryDefaultTop = 50
	wowGuildSummaryMaxTop     = 500
	wowGuildSummaryScanCap    = 5000  // hard ceiling on grouped rows pulled into Go
	wowGuildSummaryDaySeconds = 86400 // whole-day divisor for inactiveDays
)

// wow_guild_summary sort keys — the allowlist is the single source of truth shared by
// the input-schema description, the normalizer, the sort fn, and the drift-guard test.
const (
	wowGuildSummarySortMembers  = "members"  // memberCount desc — biggest guild first (default)
	wowGuildSummarySortOnline   = "online"   // onlineMembers desc — most active right now
	wowGuildSummarySortInactive = "inactive" // inactiveDays desc — most dormant/abandoned first
	wowGuildSummarySortMaxLevel = "maxlevel" // maxLevel desc — highest-level roster first
	wowGuildSummarySortCreated  = "created"  // createDate asc — oldest guild first
)

var wowGuildSummarySortKeys = []string{
	wowGuildSummarySortMembers,
	wowGuildSummarySortOnline,
	wowGuildSummarySortInactive,
	wowGuildSummarySortMaxLevel,
	wowGuildSummarySortCreated,
}

// guildSummaryRow is one guild's aggregate rollup. lastActivity is the guild's most
// recent member activity as unix seconds: `now` when any member is online, else the
// most recent offline logout_time, else 0 (unknown — a guild whose offline members
// have never logged out). inactiveDays is whole days since lastActivity (0 for an
// active-now guild, 0/unknown for a lastActivity=0 guild — NOT a bogus epoch age).
type guildSummaryRow struct {
	GuildID         int64   `json:"guildId"`
	Name            string  `json:"name"`
	LeaderGuid      int64   `json:"leaderGuid"`
	LeaderName      string  `json:"leaderName,omitempty"`
	MemberCount     int64   `json:"memberCount"`
	OnlineMembers   int64   `json:"onlineMembers"`
	MaxLevel        int     `json:"maxLevel"`
	AvgLevel        float64 `json:"avgLevel"`
	CreateDate      int64   `json:"createDate"`
	CreateDateISO   string  `json:"createDateISO,omitempty"`
	LastActivity    int64   `json:"lastActivity"`
	LastActivityISO string  `json:"lastActivityISO,omitempty"`
	InactiveDays    int64   `json:"inactiveDays"`
	InactiveHuman   string  `json:"inactiveHuman,omitempty"`
}

// guildSummaryAggregateSQL rolls up every guild in one GROUP BY. lc is a second
// LEFT JOIN to characters on the guild's leaderguid so leaderName resolves even though
// the leader is only one of the fanned-out member rows (MAX over a single-valued column
// is a no-op that keeps it ONLY_FULL_GROUP_BY-safe; a deleted leader → NULL → ""). The
// lastLogout column is MAX(logout_time) over OFFLINE members only (online members
// contribute 0 via the CASE), so an all-online guild yields 0 and the handler treats it
// as active-now instead of a stale timestamp. ORDER BY guildid keeps the scan-cap slice
// deterministic; the operator-facing ordering is applied in Go per sortBy.
const guildSummaryAggregateSQL = "SELECT g.guildid, g.name, g.leaderguid, MAX(lc.name), " +
	"COUNT(c.guid), COALESCE(SUM(c.online = 1), 0), COALESCE(MAX(c.level), 0), COALESCE(AVG(c.level), 0), " +
	"g.createdate, COALESCE(MAX(CASE WHEN c.online = 0 THEN c.logout_time ELSE 0 END), 0) " +
	"FROM `guild` g " +
	"JOIN `guild_member` gm ON gm.guildid = g.guildid " +
	"JOIN `characters` c ON c.guid = gm.guid " +
	"LEFT JOIN `characters` lc ON lc.guid = g.leaderguid " +
	"GROUP BY g.guildid, g.name, g.leaderguid, g.createdate " +
	"ORDER BY g.guildid ASC LIMIT ?"

// RegisterWowGuildSummaryTool registers `wow_guild_summary` — a read-only, realm-wide
// guild census over the ops_ro pool. It is the discovery companion to wow_guild_roster
// (which drills into ONE guild): this aggregates guild ⋈ guild_member ⋈ characters into
// one ranked row per guild so an operator can answer "which guilds are biggest / most
// active / most abandoned?" in a single round trip, then hand a guildId to
// wow_guild_roster for the member drilldown.
func RegisterWowGuildSummaryTool(reg *Registry, deps DBDeps) {
	reg.Register(Tool{
		Name: "wow_guild_summary",
		Description: "Realm-wide guild census over acore_characters — aggregates guild, guild_member and characters " +
			"on the ops_ro pool into one ranked row per guild, the discovery companion to wow_guild_roster (which " +
			"drills into a single guild's membership). Each row carries guildId, name, leaderGuid + leaderName " +
			"(resolved from the guild's leaderguid; empty if that character was deleted), memberCount, onlineMembers, " +
			"maxLevel + avgLevel over the members, createDate + createDateISO, and a dormancy lens: lastActivity " +
			"(unix seconds — now when any member is online, else the most recent offline logout_time), lastActivityISO, " +
			"inactiveDays + inactiveHuman (whole days since lastActivity — 0 for a guild with someone online, 0/unknown " +
			"for a guild whose offline members never logged out). Headline counts are honest: totalGuilds (raw guild " +
			"count on the realm) and matchedGuilds (guilds passing the filters, pre-limit) plus truncated. Args: sortBy " +
			"(members = memberCount desc [default], online = onlineMembers desc, inactive = inactiveDays desc [most " +
			"abandoned first], maxlevel = maxLevel desc, created = createDate asc [oldest first]); topN (default 50, " +
			"max 500); minMembers (default 0 = all guilds; only guilds with at least N members); minInactiveDays " +
			"(default 0 = all guilds; only dormant guilds with nobody online whose lastActivity is more than N days " +
			"old, isolating abandoned guilds the way wow_mail_unread isolates never-opened mail; negative clamps to 0). " +
			"Read-only, 10s timeout.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{
			"sortBy":{"type":"string","enum":["members","online","inactive","maxlevel","created"],"description":"Ranking key (default members = memberCount descending)"},
			"topN":{"type":"integer","description":"Max guilds returned after sorting (default 50, max 500)"},
			"minMembers":{"type":"integer","description":"Only guilds with at least this many members (default 0 = all); negative clamps to 0"},
			"minInactiveDays":{"type":"integer","description":"Only dormant guilds (nobody online) whose lastActivity is more than N days old (default 0 = all); negative clamps to 0"}
		}}`),
		Annotations: AnnRead(),
		Handler: func(ctx context.Context, raw json.RawMessage, _ string) any {
			var a struct {
				SortBy          string `json:"sortBy"`
				TopN            int    `json:"topN"`
				MinMembers      int    `json:"minMembers"`
				MinInactiveDays int    `json:"minInactiveDays"`
			}
			if len(raw) > 0 && string(raw) != "null" {
				if err := json.Unmarshal(raw, &a); err != nil {
					return map[string]any{"error": "decode args: " + err.Error()}
				}
			}
			sortKey, err := normalizeWowGuildSummarySortBy(a.SortBy)
			if err != nil {
				return map[string]any{"error": err.Error()}
			}
			if deps.QueryDB == nil {
				return map[string]any{"error": "ops_ro pool not configured (DB_DSN empty?)"}
			}
			topN := a.TopN
			if topN <= 0 {
				topN = wowGuildSummaryDefaultTop
			}
			if topN > wowGuildSummaryMaxTop {
				topN = wowGuildSummaryMaxTop
			}
			// minMembers / minInactiveDays are numeric thresholds (like topN): a negative
			// value is clamped to 0 (no filter) rather than rejected — asking for a
			// negative count is nonsensical, not a typo worth bouncing.
			minMembers := a.MinMembers
			if minMembers < 0 {
				minMembers = 0
			}
			minInactiveDays := a.MinInactiveDays
			if minInactiveDays < 0 {
				minInactiveDays = 0
			}
			c, cancel := context.WithTimeout(ctx, wowGuildSummaryTimeout)
			defer cancel()
			out, err := collectWowGuildSummary(c, deps.QueryDB, time.Now(), sortKey, topN, minMembers, minInactiveDays)
			if err != nil {
				return map[string]any{"error": err.Error()}
			}
			return out
		},
	})
}

// collectWowGuildSummary runs the honest total then the grouped aggregate over the same
// per-call connection (USE acore_characters once — the wow_guild_roster pattern), then
// filters, sorts and slices in Go. Split out so tests can drive it with sqlmock and a
// fixed `now` (inactiveDays and the dormancy filter are now-relative).
func collectWowGuildSummary(ctx context.Context, db *sql.DB, now time.Time, sortKey string, topN, minMembers, minInactiveDays int) (map[string]any, error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire conn: %w", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "USE `acore_characters`"); err != nil {
		return nil, fmt.Errorf("USE acore_characters: %w", err)
	}

	// totalGuilds — the raw realm-wide guild count, the honest denominator. The ranked
	// list and matchedGuilds cover guilds with at least one live member (the JOIN
	// universe); in practice these coincide because AzerothCore drops guild_member rows
	// on character delete and auto-disbands emptied guilds.
	var totalGuilds int64
	if err := conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM `guild`").Scan(&totalGuilds); err != nil {
		return nil, fmt.Errorf("guild-summary total: %w", err)
	}

	nowUnix := now.Unix()

	rows, err := conn.QueryContext(ctx, guildSummaryAggregateSQL, wowGuildSummaryScanCap+1)
	if err != nil {
		return nil, fmt.Errorf("guild-summary aggregate: %w", err)
	}
	defer rows.Close()

	all := make([]*guildSummaryRow, 0, wowGuildSummaryDefaultTop)
	for rows.Next() {
		var (
			r          guildSummaryRow
			leaderName sql.NullString
			avg        sql.NullFloat64
			lastLogout int64
		)
		if err := rows.Scan(&r.GuildID, &r.Name, &r.LeaderGuid, &leaderName, &r.MemberCount,
			&r.OnlineMembers, &r.MaxLevel, &avg, &r.CreateDate, &lastLogout); err != nil {
			return nil, fmt.Errorf("guild-summary scan: %w", err)
		}
		r.LeaderName = leaderName.String
		if avg.Valid {
			r.AvgLevel = math.Round(avg.Float64*10) / 10 // one decimal place
		}
		if r.CreateDate > 0 {
			r.CreateDateISO = formatUnixISO(r.CreateDate)
		}
		// Dormancy: someone online → active now (inactiveDays 0). Otherwise the most
		// recent offline logout drives it; a 0 lastLogout (no member has ever logged out)
		// stays unknown rather than reporting a multi-decade age from the unix epoch.
		switch {
		case r.OnlineMembers > 0:
			r.LastActivity = nowUnix
			r.LastActivityISO = formatUnixISO(nowUnix)
		case lastLogout > 0:
			r.LastActivity = lastLogout
			r.LastActivityISO = formatUnixISO(lastLogout)
			inactiveSec := nowUnix - lastLogout
			if inactiveSec < 0 {
				inactiveSec = 0
			}
			r.InactiveDays = inactiveSec / wowGuildSummaryDaySeconds
			r.InactiveHuman = formatDuration(inactiveSec)
		}
		all = append(all, &r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("guild-summary iter: %w", err)
	}

	// scanCap+1 probe: if we pulled more than the cap, the realm has more guilds than we
	// scan — trim to the cap and flag it so the counts read as "of the scanned set".
	scanTruncated := false
	if len(all) > wowGuildSummaryScanCap {
		all = all[:wowGuildSummaryScanCap]
		scanTruncated = true
	}

	// filter (Go-side): minMembers floor + the dormancy lens. inactiveDays >= N is exactly
	// equivalent to logout_time <= now-N*86400 (floor(x/day) >= N ⟺ x >= N*day), so this
	// matches wow_guild_roster's cutoff semantics without recomputing it.
	matched := make([]*guildSummaryRow, 0, len(all))
	for _, r := range all {
		if r.MemberCount < int64(minMembers) {
			continue
		}
		if minInactiveDays > 0 {
			if r.OnlineMembers > 0 || r.LastActivity == 0 || r.InactiveDays < int64(minInactiveDays) {
				continue
			}
		}
		matched = append(matched, r)
	}
	matchedGuilds := len(matched)

	sortWowGuildSummary(matched, sortKey)

	if len(matched) > topN {
		matched = matched[:topN]
	}

	return map[string]any{
		"asOf":            now.UTC().Format(time.RFC3339),
		"totalGuilds":     totalGuilds,
		"matchedGuilds":   matchedGuilds,
		"sortBy":          sortKey,
		"topN":            topN,
		"minMembers":      minMembers,
		"minInactiveDays": minInactiveDays,
		"returned":        len(matched),
		"truncated":       matchedGuilds > len(matched),
		"scanTruncated":   scanTruncated,
		"guilds":          matched,
	}, nil
}

// sortWowGuildSummary orders the guilds by the chosen key with deterministic
// tiebreakers ending in guildId asc — without that final key Go's map-iter-driven scan
// order could flake tied rows across runs. The default branch mirrors the `members`
// ranking so a validation drift on sortBy still yields a stable order, not scan order.
func sortWowGuildSummary(rows []*guildSummaryRow, key string) {
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		switch key {
		case wowGuildSummarySortOnline:
			if a.OnlineMembers != b.OnlineMembers {
				return a.OnlineMembers > b.OnlineMembers
			}
			if a.MemberCount != b.MemberCount {
				return a.MemberCount > b.MemberCount
			}
		case wowGuildSummarySortInactive:
			if a.InactiveDays != b.InactiveDays {
				return a.InactiveDays > b.InactiveDays
			}
			if a.MemberCount != b.MemberCount {
				return a.MemberCount > b.MemberCount
			}
		case wowGuildSummarySortMaxLevel:
			if a.MaxLevel != b.MaxLevel {
				return a.MaxLevel > b.MaxLevel
			}
			if a.MemberCount != b.MemberCount {
				return a.MemberCount > b.MemberCount
			}
		case wowGuildSummarySortCreated:
			if a.CreateDate != b.CreateDate {
				return a.CreateDate < b.CreateDate // oldest guild first
			}
		default: // wowGuildSummarySortMembers
			if a.MemberCount != b.MemberCount {
				return a.MemberCount > b.MemberCount
			}
		}
		return a.GuildID < b.GuildID
	})
}

// normalizeWowGuildSummarySortBy trims/lowercases the requested key: empty → the default
// `members`; a known key passes through; anything else is a self-correcting error that
// echoes the canonical allowlist.
func normalizeWowGuildSummarySortBy(s string) (string, error) {
	k := strings.ToLower(strings.TrimSpace(s))
	if k == "" {
		return wowGuildSummarySortMembers, nil
	}
	for _, valid := range wowGuildSummarySortKeys {
		if k == valid {
			return k, nil
		}
	}
	return "", fmt.Errorf("invalid sortBy %q (allowed: %s)", s, strings.Join(wowGuildSummarySortKeys, ", "))
}
