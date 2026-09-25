package mcpserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

const (
	gmtTimeout     = 10 * time.Second
	gmtScanCap     = 200000 // hard ceiling on guild_eventlog officer-event rows folded into one snapshot
	gmtDefaultTopN = 25
	gmtMaxTopN     = 200
	gmtDefaultMin  = 1        // surface only targets with at least one officer event
	gmtBotPrefix   = "RNDBOT" // mod-playerbots' default RandomBotAccountPrefix
)

// guild_eventlog.EventType values (AC Guild.h GuildEventLogTypes, source-anchored
// in cron-state research-cache/acore-guild-logs.json). Only the OFFICER events —
// where PlayerGuid1 (actor) and PlayerGuid2 (target) diverge — are meaningful on
// the target axis; JOIN_GUILD(2)/LEAVE_GUILD(6) have PlayerGuid2==PlayerGuid1 (a
// self-action already covered by the actor-axis tool) and are excluded at the SQL
// WHERE clause, never scanned here. Named with the gmt (guild_membership_targets)
// prefix so this file defines NO symbol an eventual actor-axis sibling
// (wow_guild_membership_actors) also defines — both would merge into the same
// package.
const (
	gmtInvite   = 1
	gmtPromote  = 3
	gmtDemote   = 4
	gmtUninvite = 5
)

// gmtScanBase is the per-event scan over acore_characters.guild_eventlog, already
// restricted to the four officer event types (1 INVITE, 3 PROMOTE, 4 DEMOTE,
// 5 UNINVITE) where PlayerGuid2 is a genuine target distinct from the acting
// officer. An optional `AND` (guildid / TimeStamp) and the trailing `LIMIT ?`
// (bound to scanCap+1 so truncation is detectable without a separate COUNT) are
// appended by the builder.
const gmtScanBase = "SELECT guildid, EventType, PlayerGuid2, NewRank, TimeStamp FROM `guild_eventlog` WHERE EventType IN (1,3,4,5)"

// gmTarget is one ranked guild-membership target (a characters.guid appearing as
// guild_eventlog.PlayerGuid2 on an officer event). NewRank is only meaningful on
// PROMOTE/DEMOTE rows, so LatestNewRank/HasRankEvent are populated only from
// those event types (tracked by the most recent such event's TimeStamp), never
// from INVITE/UNINVITE rows. characterName / accountId / accountUsername / isBot
// are folded in from the two batch lookups (empty name + account 0 when the
// target character row is gone — a deleted character whose membership events
// outlive it, counted in `unresolved`).
type gmTarget struct {
	PlayerGuid         int64  `json:"playerGuid"`
	CharacterName      string `json:"characterName"`
	AccountID          int64  `json:"accountId"`
	AccountUsername    string `json:"accountUsername"`
	IsBot              bool   `json:"isBot"`
	TotalOfficerEvents int    `json:"totalOfficerEvents"`
	Invited            int    `json:"invited"`
	Promoted           int    `json:"promoted"`
	Demoted            int    `json:"demoted"`
	Uninvited          int    `json:"uninvited"`
	RankChurn          int    `json:"rankChurn"`
	HasRankEvent       bool   `json:"hasRankEvent"`
	LatestNewRank      int    `json:"latestNewRank"`
	DistinctGuilds     int    `json:"distinctGuilds"`
	FirstActivityISO   string `json:"firstActivity"`
	LastActivityISO    string `json:"lastActivity"`
	RecencyHuman       string `json:"recencyHuman"`

	// unexported fold state — the min/max event timestamps (recency sort + ISO
	// stamps), the timestamp of the most recent promote/demote row (drives
	// LatestNewRank), and the set of guilds this target was acted on within. Not
	// serialized.
	firstTS int64
	lastTS  int64
	rankTS  int64
	guilds  map[int64]struct{}
}

// fold adds one guild_eventlog officer-event row to the target's running totals.
// newRank is only meaningful for promote/demote events; it's ignored for
// invite/uninvite rows.
func (t *gmTarget) fold(eventType int, guildID int64, newRank int, ts int64) {
	t.TotalOfficerEvents++
	switch eventType {
	case gmtInvite:
		t.Invited++
	case gmtPromote:
		t.Promoted++
		if ts >= t.rankTS {
			t.rankTS = ts
			t.LatestNewRank = newRank
			t.HasRankEvent = true
		}
	case gmtDemote:
		t.Demoted++
		if ts >= t.rankTS {
			t.rankTS = ts
			t.LatestNewRank = newRank
			t.HasRankEvent = true
		}
	case gmtUninvite:
		t.Uninvited++
	}
	if t.guilds == nil {
		t.guilds = map[int64]struct{}{}
	}
	t.guilds[guildID] = struct{}{}
	if t.firstTS == 0 || (ts > 0 && ts < t.firstTS) {
		t.firstTS = ts
	}
	if ts > t.lastTS {
		t.lastTS = ts
	}
}

// finalize renders the derived fields (rank churn, distinct-guild count, activity
// ISO stamps + recency) once, after the top-N slice, so we don't format activity
// for targets that never make the cut. now anchors the recency delta.
func (t *gmTarget) finalize(now time.Time) {
	t.RankChurn = t.Promoted + t.Demoted
	t.DistinctGuilds = len(t.guilds)
	t.FirstActivityISO = formatUnixISO(t.firstTS)
	t.LastActivityISO = formatUnixISO(t.lastTS)
	if t.lastTS > 0 {
		t.RecencyHuman = formatDuration(now.Unix() - t.lastTS)
	}
}

// RegisterWowGuildMembershipTargetsTool registers `wow_guild_membership_targets` —
// a read-only ranking of the guild members most acted-UPON by officers (invited /
// promoted / demoted / uninvited), with character-name + bot-vs-player resolution
// folded in.
//
// The receiving-end view of guild_eventlog. Every officer event (INVITE_PLAYER=1,
// PROMOTE_PLAYER=3, DEMOTE_PLAYER=4, UNINVITE_PLAYER=5) names PlayerGuid1 (the
// acting officer) AND PlayerGuid2 (the target of the action); no existing ops-api
// tool reads the PlayerGuid2 axis. This promotes it into a dedicated leaderboard:
// GROUP BY PlayerGuid2 over the four officer event types, joined to characters +
// acore_auth.account so an operator sees who is being rank-yo-yo'd (promoted then
// demoted repeatedly), who's getting mass-invited or serially uninvited, and
// whether the target is a bot (RNDBOT prefix). JOIN_GUILD(2) / LEAVE_GUILD(6) are
// excluded — those have PlayerGuid2==PlayerGuid1 (a self-action, not something
// done TO a target) and are already covered by the actor-axis view. Complements
// wow_guild_bank_top_actors (the bank-log actor axis) and the guild_eventlog
// actor axis (who is DOING the inviting/promoting/kicking) with the mirror
// question: who is it being done TO.
func RegisterWowGuildMembershipTargetsTool(reg *Registry, deps DBDeps) {
	reg.Register(Tool{
		Name: "wow_guild_membership_targets",
		Description: "Top-N guild-membership targets by officer-action churn, aggregated from " +
			"acore_characters.guild_eventlog over the ops_ro pool, with character-name + bot-vs-player " +
			"resolution. Three-stage composite: (1) fold every OFFICER event (EventType 1 INVITE_PLAYER, " +
			"3 PROMOTE_PLAYER, 4 DEMOTE_PLAYER, 5 UNINVITE_PLAYER) per PlayerGuid2 (the target of the " +
			"action, NOT the acting officer) in Go from one scan — JOIN_GUILD(2)/LEAVE_GUILD(6) are " +
			"excluded in SQL since those are self-actions (PlayerGuid2==PlayerGuid1); (2) batch-resolve " +
			"the top-N PlayerGuids to characters.name + account via an IN(...) query on " +
			"acore_characters.characters; (3) batch-resolve those accounts to acore_auth.account.username " +
			"in one IN(...) query and flag isBot when the username starts with \"RNDBOT\" (mod-playerbots' " +
			"RandomBotAccountPrefix). The receiving-end view of guild membership churn: who is being " +
			"rank-yo-yo'd (promoted then demoted repeatedly — rankChurn = promoted+demoted), who's getting " +
			"mass-invited or serially uninvited, and is the target a bot. NewRank is only meaningful on " +
			"promote/demote rows, so hasRankEvent + latestNewRank reflect the most recent such event only " +
			"(false/0 when a target was only ever invited/uninvited). An target whose acting character row " +
			"is gone (deleted character with surviving membership events) surfaces with an empty " +
			"characterName + accountId 0 and is counted in `unresolved`. Args: topN (default 25, max 200), " +
			"sortBy (rankChurn|uninvited|promoted|invited|events, default rankChurn), guildId (optional — " +
			"restrict to one guild's targets; must be positive), sinceHours (optional window — only events " +
			"with TimeStamp within the last N hours; 0 = the whole bounded log), minEvents (default 1, " +
			"clamps to >=1). Scan capped at 200000 rows; truncated:true flags an incomplete fold. Read-only, " +
			"10 s timeout.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{
			"topN":{"type":"integer","description":"Max targets to rank (default 25, max 200)"},
			"sortBy":{"type":"string","description":"Ranking key: rankChurn (default, promoted+demoted desc), uninvited (desc), promoted (desc), invited (desc), events (totalOfficerEvents desc)"},
			"guildId":{"type":"integer","description":"Restrict to one guild's targets by guildid (must be positive); omit for all guilds"},
			"sinceHours":{"type":"integer","description":"Only fold events whose TimeStamp is within the last N hours; 0 (default) = the whole bounded log; negative clamps to 0"},
			"minEvents":{"type":"integer","description":"Only rank targets with at least this many officer events in the window (default 1, clamps to >=1)"}
		}}`),
		Annotations: AnnRead(),
		Handler: func(ctx context.Context, raw json.RawMessage, _ string) any {
			var a struct {
				TopN       int    `json:"topN"`
				SortBy     string `json:"sortBy"`
				GuildID    *int   `json:"guildId"`
				SinceHours int    `json:"sinceHours"`
				MinEvents  int    `json:"minEvents"`
			}
			if len(raw) > 0 && string(raw) != "null" {
				if err := json.Unmarshal(raw, &a); err != nil {
					return map[string]any{"error": "decode args: " + err.Error()}
				}
			}
			if deps.QueryDB == nil {
				return map[string]any{"error": "ops_ro pool not configured (DB_DSN empty?)"}
			}
			sortBy := strings.TrimSpace(a.SortBy)
			if sortBy == "" {
				sortBy = "rankChurn"
			}
			if !gmtValidSort(sortBy) {
				return map[string]any{"error": "sortBy must be one of: rankChurn, uninvited, promoted, invited, events"}
			}
			if a.GuildID != nil && *a.GuildID <= 0 {
				return map[string]any{"error": "guildId must be a positive integer"}
			}
			topN := a.TopN
			if topN <= 0 {
				topN = gmtDefaultTopN
			}
			if topN > gmtMaxTopN {
				topN = gmtMaxTopN
			}
			minEvents := a.MinEvents
			if minEvents < gmtDefaultMin {
				minEvents = gmtDefaultMin
			}
			sinceHours := a.SinceHours
			if sinceHours < 0 {
				sinceHours = 0
			}
			c, cancel := context.WithTimeout(ctx, gmtTimeout)
			defer cancel()
			out, err := collectWowGuildMembershipTargets(c, deps.QueryDB, time.Now(), a.GuildID, sortBy, sinceHours, minEvents, topN)
			if err != nil {
				return map[string]any{"error": err.Error()}
			}
			return out
		},
	})
}

// gmtValidSort is the sortBy allowlist. Validated up front (before any query
// fires) so an unknown key is a clean argument error, not a silent fall-through.
// The sort itself runs in Go over the folded targets (no ORDER BY interpolation
// -> sidesteps the aggregate-alias ORDER BY footgun).
func gmtValidSort(s string) bool {
	switch s {
	case "rankChurn", "uninvited", "promoted", "invited", "events":
		return true
	}
	return false
}

// collectWowGuildMembershipTargets runs the three-stage composite. Split out so
// tests can drive it with sqlmock and a fixed `now`. guildID is *int so "unset"
// (all guilds) is distinguishable from an explicit filter.
func collectWowGuildMembershipTargets(ctx context.Context, db *sql.DB, now time.Time, guildID *int, sortBy string, sinceHours, minEvents, topN int) (map[string]any, error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire conn: %w", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "USE `acore_characters`"); err != nil {
		return nil, fmt.Errorf("USE acore_characters: %w", err)
	}

	// Stage 1: scan + fold per target. guildId + sinceHours narrow the scan in SQL,
	// on top of the always-present officer-event-type restriction baked into
	// gmtScanBase.
	q := gmtScanBase
	args := make([]any, 0, 3)
	where := make([]string, 0, 2)
	if guildID != nil {
		where = append(where, "guildid = ?")
		args = append(args, *guildID)
	}
	var cutoff int64
	if sinceHours > 0 {
		cutoff = now.Unix() - int64(sinceHours)*3600
		where = append(where, "TimeStamp >= ?")
		args = append(args, cutoff)
	}
	if len(where) > 0 {
		q += " AND " + strings.Join(where, " AND ")
	}
	q += " LIMIT ?"
	args = append(args, gmtScanCap+1)

	rows, err := conn.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("guild_eventlog query: %w", err)
	}
	defer rows.Close()

	targets := map[int64]*gmTarget{}
	scanned := 0
	truncated := false
	totalInvited := 0
	totalPromoted := 0
	totalDemoted := 0
	totalUninvited := 0
	for rows.Next() {
		if scanned >= gmtScanCap {
			truncated = true
			break
		}
		var eventType int
		var guildRow, playerGuid2 int64
		var newRank int
		var ts int64
		if err := rows.Scan(&guildRow, &eventType, &playerGuid2, &newRank, &ts); err != nil {
			return nil, fmt.Errorf("guild_eventlog scan: %w", err)
		}
		scanned++
		switch eventType {
		case gmtInvite:
			totalInvited++
		case gmtPromote:
			totalPromoted++
		case gmtDemote:
			totalDemoted++
		case gmtUninvite:
			totalUninvited++
		}
		tgt := targets[playerGuid2]
		if tgt == nil {
			tgt = &gmTarget{PlayerGuid: playerGuid2}
			targets[playerGuid2] = tgt
		}
		tgt.fold(eventType, guildRow, newRank, ts)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("guild_eventlog iter: %w", err)
	}

	// Stage 2: minEvents filter, sort by the chosen key, slice to topN. distinctTargets
	// is the honest realm-wide total (every folded target); matchedTargets is the pool
	// that met the minEvents floor and was ranked from.
	distinctTargets := len(targets)
	top, matched := topNGuildMembershipTargets(targets, sortBy, minEvents, topN)

	// Stage 3: resolve names (still in acore_characters) then accounts (acore_auth).
	unresolved := 0
	if len(top) > 0 {
		if err := annotateTargetCharacters(ctx, conn, top); err != nil {
			return nil, err
		}
		if _, err := conn.ExecContext(ctx, "USE `acore_auth`"); err != nil {
			return nil, fmt.Errorf("USE acore_auth: %w", err)
		}
		if err := annotateTargetAccounts(ctx, conn, top); err != nil {
			return nil, err
		}
		for _, tgt := range top {
			tgt.finalize(now)
			if tgt.CharacterName == "" {
				unresolved++
			}
		}
	}

	out := map[string]any{
		"asOf":               now.UTC().Format(time.RFC3339),
		"topN":               topN,
		"sortBy":             sortBy,
		"minEvents":          minEvents,
		"sinceHours":         sinceHours,
		"distinctTargets":    distinctTargets,
		"matchedTargets":     matched,
		"scanned":            scanned,
		"scanCap":            gmtScanCap,
		"truncated":          truncated,
		"unresolved":         unresolved,
		"totalOfficerEvents": scanned,
		"totalInvited":       totalInvited,
		"totalPromoted":      totalPromoted,
		"totalDemoted":       totalDemoted,
		"totalUninvited":     totalUninvited,
		"targets":            top,
	}
	if guildID != nil {
		out["guildId"] = *guildID
	}
	if sinceHours > 0 {
		out["sinceCutoff"] = formatUnixISO(cutoff)
	}
	return out, nil
}

// topNGuildMembershipTargets filters the folded targets to those with >= minEvents,
// sorts by the chosen key (all descending) with PlayerGuid-asc as a deterministic
// tiebreaker (without it Go's randomized map iteration would flake tests on ties),
// slices to n, and returns the slice plus the pre-slice matched count (so the
// caller can report "ranking the top 25 of 540 targets"). Always a non-nil slice.
func topNGuildMembershipTargets(m map[int64]*gmTarget, sortBy string, minEvents, n int) ([]*gmTarget, int) {
	out := make([]*gmTarget, 0, len(m))
	for _, t := range m {
		if t.TotalOfficerEvents >= minEvents {
			out = append(out, t)
		}
	}
	matched := len(out)
	sort.Slice(out, func(i, j int) bool {
		return gmTargetLess(out[i], out[j], sortBy)
	})
	if len(out) > n {
		out = out[:n]
	}
	return out, matched
}

// gmTargetLess ranks a before b for the given sort key (all descending), falling
// back to PlayerGuid-asc so ties are deterministic. rankChurn is computed
// on-the-fly (promoted+demoted) since it's only assigned in finalize, which runs
// after this sort.
func gmTargetLess(a, b *gmTarget, sortBy string) bool {
	switch sortBy {
	case "uninvited":
		if a.Uninvited != b.Uninvited {
			return a.Uninvited > b.Uninvited
		}
	case "promoted":
		if a.Promoted != b.Promoted {
			return a.Promoted > b.Promoted
		}
	case "invited":
		if a.Invited != b.Invited {
			return a.Invited > b.Invited
		}
	case "events":
		if a.TotalOfficerEvents != b.TotalOfficerEvents {
			return a.TotalOfficerEvents > b.TotalOfficerEvents
		}
	default: // rankChurn
		ac := a.Promoted + a.Demoted
		bc := b.Promoted + b.Demoted
		if ac != bc {
			return ac > bc
		}
	}
	return a.PlayerGuid < b.PlayerGuid
}

// annotateTargetCharacters batch-resolves the top targets' PlayerGuids to
// characters.name + account in one IN(...) query. Mirrors the
// wow_guild_bank_top_actors batch-IN idiom: one round trip, a guid->row map, then
// a second pass to set the fields. A guid with no row (a deleted character whose
// membership events outlive it) is left with an empty name + account 0 —
// surfaced via the top-level `unresolved` count, not dropped.
func annotateTargetCharacters(ctx context.Context, conn *sql.Conn, targets []*gmTarget) error {
	if len(targets) == 0 {
		return nil
	}
	var b strings.Builder
	b.Grow(64 + len(targets)*2)
	b.WriteString("SELECT guid, name, account FROM `characters` WHERE guid IN (")
	args := make([]any, 0, len(targets))
	for i, t := range targets {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteByte('?')
		args = append(args, t.PlayerGuid)
	}
	b.WriteByte(')')
	rs, err := conn.QueryContext(ctx, b.String(), args...)
	if err != nil {
		return fmt.Errorf("characters query: %w", err)
	}
	defer rs.Close()
	type charRow struct {
		name    string
		account int64
	}
	byGuid := make(map[int64]charRow, len(targets))
	for rs.Next() {
		var guid, account int64
		var name string
		if err := rs.Scan(&guid, &name, &account); err != nil {
			return fmt.Errorf("characters scan: %w", err)
		}
		byGuid[guid] = charRow{name: name, account: account}
	}
	if err := rs.Err(); err != nil {
		return fmt.Errorf("characters iter: %w", err)
	}
	for _, t := range targets {
		if c, ok := byGuid[t.PlayerGuid]; ok {
			t.CharacterName = c.name
			t.AccountID = c.account
		}
	}
	return nil
}

// annotateTargetAccounts batch-resolves the resolved targets' account ids to
// acore_auth.account.username in one IN(...) query and flags isBot when the
// username carries the RNDBOT prefix (case-insensitive). Targets whose character
// row was missing (account 0) are skipped — they carry no username and isBot
// stays false. Distinct account ids are deduped so two target-characters on the
// same account bind the id once.
func annotateTargetAccounts(ctx context.Context, conn *sql.Conn, targets []*gmTarget) error {
	ids := make([]int64, 0, len(targets))
	seen := make(map[int64]bool, len(targets))
	for _, t := range targets {
		if t.AccountID != 0 && !seen[t.AccountID] {
			seen[t.AccountID] = true
			ids = append(ids, t.AccountID)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	var b strings.Builder
	b.Grow(48 + len(ids)*2)
	b.WriteString("SELECT id, username FROM `account` WHERE id IN (")
	args := make([]any, 0, len(ids))
	for i, id := range ids {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteByte('?')
		args = append(args, id)
	}
	b.WriteByte(')')
	rs, err := conn.QueryContext(ctx, b.String(), args...)
	if err != nil {
		return fmt.Errorf("account query: %w", err)
	}
	defer rs.Close()
	byID := make(map[int64]string, len(ids))
	for rs.Next() {
		var id int64
		var username string
		if err := rs.Scan(&id, &username); err != nil {
			return fmt.Errorf("account scan: %w", err)
		}
		byID[id] = username
	}
	if err := rs.Err(); err != nil {
		return fmt.Errorf("account iter: %w", err)
	}
	for _, t := range targets {
		if u, ok := byID[t.AccountID]; ok {
			t.AccountUsername = u
			t.IsBot = strings.HasPrefix(strings.ToUpper(u), gmtBotPrefix)
		}
	}
	return nil
}
