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
	gmActorsTimeout     = 10 * time.Second
	gmActorsScanCap     = 200000 // hard ceiling on guild_eventlog rows folded into one snapshot
	gmActorsDefaultTopN = 25
	gmActorsMaxTopN     = 200
	gmActorsDefaultMin  = 1        // surface only actors with at least one event
	gmActorsBotPrefix   = "RNDBOT" // mod-playerbots' default RandomBotAccountPrefix
)

// guild_eventlog.EventType values (AC Guild.h GuildEventLogTypes, source-anchored
// in cron-state research-cache/acore-guild-logs.json, re-verified against the
// azerothcore-wotlk master guild_eventlog.sql schema). The MEMBERSHIP log — a
// different table from guild_bank_eventlog. PlayerGuid1 is always the acting
// player; whether that action is on OTHERS (officer) or on SELF depends only on
// EventType: invite/promote/demote/uninvite are officer actions (PlayerGuid1 =
// officer, PlayerGuid2 = target); join/leave are self actions (PlayerGuid1 ==
// PlayerGuid2 == the joiner/leaver). Named with the gma* (guild_membership_actors)
// prefix so this file defines NO symbol an unmerged membership sibling
// (wow_guild_membership_churn, #359) also defines — both merge into the same package.
const (
	gmaInvitePlayer   = 1
	gmaJoinGuild      = 2
	gmaPromotePlayer  = 3
	gmaDemotePlayer   = 4
	gmaUninvitePlayer = 5
	gmaLeaveGuild     = 6
)

// gmActorsScanBase is the per-event scan over acore_characters.guild_eventlog.
// Only the columns this tool folds per actor — guildid (for the optional guild
// filter + distinct-guild count), EventType (officer-vs-self classification),
// PlayerGuid1 (the acting characters.guid), TimeStamp (unix seconds, for the
// window filter + recency). PlayerGuid2 / NewRank are NOT scanned: the actor is
// always PlayerGuid1 and the self-vs-officer split is a pure function of
// EventType, so comparing the two guids adds nothing. An optional WHERE (guildid /
// TimeStamp) and the trailing `LIMIT ?` (bound to scanCap+1 so truncation is
// detectable without a separate COUNT) are appended by the builder.
const gmActorsScanBase = "SELECT guildid, EventType, PlayerGuid1, TimeStamp FROM `guild_eventlog`"

// gmActor is one ranked guild-membership actor (a characters.guid appearing as
// guild_eventlog.PlayerGuid1). Officer events are actions the actor performed on
// OTHERS (invite/promote/demote/uninvite); self events are the actor's own
// join/leave. All fields here are COUNTS (the membership log carries no money or
// item columns). characterName / accountId / accountUsername / isBot are folded in
// from the two batch lookups (empty name + account 0 when the acting character row
// is gone — a deleted character whose membership events outlive it, counted in
// `unresolved`).
type gmActor struct {
	PlayerGuid       int64  `json:"playerGuid"`
	CharacterName    string `json:"characterName"`
	AccountID        int64  `json:"accountId"`
	AccountUsername  string `json:"accountUsername"`
	IsBot            bool   `json:"isBot"`
	TotalEvents      int    `json:"totalEvents"`
	OfficerEvents    int    `json:"officerEvents"`
	SelfEvents       int    `json:"selfEvents"`
	Invites          int    `json:"invites"`
	Promotes         int    `json:"promotes"`
	Demotes          int    `json:"demotes"`
	Uninvites        int    `json:"uninvites"`
	Joins            int    `json:"joins"`
	Leaves           int    `json:"leaves"`
	DistinctGuilds   int    `json:"distinctGuilds"`
	FirstActivityISO string `json:"firstActivity"`
	LastActivityISO  string `json:"lastActivity"`
	RecencyHuman     string `json:"recencyHuman"`

	// unexported fold state — the min/max event timestamps (recency sort + ISO
	// stamps) and the set of guilds this actor touched. Not serialized.
	firstTS int64
	lastTS  int64
	guilds  map[int64]struct{}
}

// fold adds one guild_eventlog row to the actor's running totals, splitting the
// four officer actions from the two self actions per AC's GuildEventLogTypes.
func (a *gmActor) fold(eventType int, guildID, ts int64) {
	a.TotalEvents++
	switch eventType {
	case gmaInvitePlayer:
		a.Invites++
		a.OfficerEvents++
	case gmaPromotePlayer:
		a.Promotes++
		a.OfficerEvents++
	case gmaDemotePlayer:
		a.Demotes++
		a.OfficerEvents++
	case gmaUninvitePlayer:
		a.Uninvites++
		a.OfficerEvents++
	case gmaJoinGuild:
		a.Joins++
		a.SelfEvents++
	case gmaLeaveGuild:
		a.Leaves++
		a.SelfEvents++
	}
	if a.guilds == nil {
		a.guilds = map[int64]struct{}{}
	}
	a.guilds[guildID] = struct{}{}
	if a.firstTS == 0 || (ts > 0 && ts < a.firstTS) {
		a.firstTS = ts
	}
	if ts > a.lastTS {
		a.lastTS = ts
	}
}

// finalize renders the derived fields (distinct-guild count, activity ISO stamps +
// recency) once, after the top-N slice, so we don't format for actors that never
// make the cut. now anchors the recency delta.
func (a *gmActor) finalize(now time.Time) {
	a.DistinctGuilds = len(a.guilds)
	a.FirstActivityISO = formatUnixISO(a.firstTS)
	a.LastActivityISO = formatUnixISO(a.lastTS)
	if a.lastTS > 0 {
		a.RecencyHuman = formatDuration(now.Unix() - a.lastTS)
	}
}

// RegisterWowGuildMembershipActorsTool registers `wow_guild_membership_actors` — a
// read-only ranking of the players driving guild-membership churn by event count,
// with character-name + bot-vs-player resolution folded in.
//
// The WHO-drives-guild-churn view of the membership log. wow_guild_membership_churn
// rolls guild_eventlog up to GUILD grain (counts per guild) but names no actor; yet
// guild_eventlog.PlayerGuid1 IS the acting player (the officer who invited /
// promoted / kicked, or the member who joined / left). This promotes the ACTOR axis
// into a dedicated leaderboard: GROUP BY PlayerGuid1 over guild_eventlog, splitting
// officer actions (invite/promote/demote/uninvite — PlayerGuid1 is the officer) from
// self actions (join/leave — PlayerGuid1 == PlayerGuid2 is the member) exactly as
// AC's Guild.h GuildEventLogTypes classifies them, joined to characters +
// acore_auth.account so an operator sees the top churn-drivers realm-wide and
// whether bots (RNDBOT prefix) are mass-inviting or thrashing ranks — the
// membership-log counterpart to wow_guild_bank_top_actors' WHO view of the bank.
func RegisterWowGuildMembershipActorsTool(reg *Registry, deps DBDeps) {
	reg.Register(Tool{
		Name: "wow_guild_membership_actors",
		Description: "Top-N guild-MEMBERSHIP actors by event churn, aggregated from " +
			"acore_characters.guild_eventlog over the ops_ro pool, with character-name + " +
			"bot-vs-player resolution. Three-stage composite: (1) fold every membership event per " +
			"PlayerGuid1 (the acting player) in Go from one scan, splitting officer events (EventType " +
			"1/3/4/5 → invites / promotes / demotes / uninvites, actions ON OTHERS) from self events " +
			"(EventType 2/6 → joins / leaves, where PlayerGuid1==PlayerGuid2) per AC Guild.h " +
			"GuildEventLogTypes; (2) batch-resolve the top-N PlayerGuids to characters.name + account " +
			"via an IN(...) query on acore_characters.characters; (3) batch-resolve those accounts to " +
			"acore_auth.account.username in one IN(...) query and flag isBot when the username starts " +
			"with \"RNDBOT\" (mod-playerbots' RandomBotAccountPrefix). The WHO-drives-churn view of the " +
			"membership log: wow_guild_membership_churn counts joins / leaves / promotes per guild but " +
			"names no actor; this ranks the busiest officers / members realm-wide (or within one guildId) " +
			"with per-actor officerEvents (invites + promotes + demotes + uninvites), selfEvents (joins + " +
			"leaves), the six event-type counts, distinct guilds touched, and first/last activity + " +
			"recency. On a mod-playerbots server isBot makes visible whether bots are mass-inviting or " +
			"rank-thrashing guilds, without a separate wow_bot_lookup — spot a rogue promoter or a bot " +
			"inviting-spammer; the membership-log counterpart to wow_guild_bank_top_actors' WHO-moves-" +
			"the-gold view of the guild bank. An actor whose acting character row is gone (deleted character with " +
			"surviving membership events) surfaces with an empty characterName + accountId 0 and is " +
			"counted in `unresolved`. Args: topN (default 25, max 200), sortBy (officerEvents|invites|" +
			"promotes|uninvites|events, default officerEvents), guildId (optional — restrict to one " +
			"guild's actors; must be positive), sinceHours (optional window — only events with TimeStamp " +
			"within the last N hours; 0 = the whole bounded log), minEvents (default 1, clamps to >=1). " +
			"Scan capped at 200000 rows; truncated:true flags an incomplete fold. Read-only, 10 s timeout.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{
			"topN":{"type":"integer","description":"Max actors to rank (default 25, max 200)"},
			"sortBy":{"type":"string","description":"Ranking key: officerEvents (default, invites+promotes+demotes+uninvites desc), invites (INVITE_PLAYER count desc), promotes (PROMOTE_PLAYER count desc), uninvites (UNINVITE_PLAYER count desc), events (totalEvents desc)"},
			"guildId":{"type":"integer","description":"Restrict to one guild's actors by guildid (must be positive); omit for all guilds"},
			"sinceHours":{"type":"integer","description":"Only fold events whose TimeStamp is within the last N hours; 0 (default) = the whole bounded log; negative clamps to 0"},
			"minEvents":{"type":"integer","description":"Only rank actors with at least this many events in the window (default 1, clamps to >=1)"}
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
				sortBy = "officerEvents"
			}
			if !gmActorsValidSort(sortBy) {
				return map[string]any{"error": "sortBy must be one of: officerEvents, invites, promotes, uninvites, events"}
			}
			if a.GuildID != nil && *a.GuildID <= 0 {
				return map[string]any{"error": "guildId must be a positive integer"}
			}
			topN := a.TopN
			if topN <= 0 {
				topN = gmActorsDefaultTopN
			}
			if topN > gmActorsMaxTopN {
				topN = gmActorsMaxTopN
			}
			minEvents := a.MinEvents
			if minEvents < gmActorsDefaultMin {
				minEvents = gmActorsDefaultMin
			}
			sinceHours := a.SinceHours
			if sinceHours < 0 {
				sinceHours = 0
			}
			c, cancel := context.WithTimeout(ctx, gmActorsTimeout)
			defer cancel()
			out, err := collectWowGuildMembershipActors(c, deps.QueryDB, time.Now(), a.GuildID, sortBy, sinceHours, minEvents, topN)
			if err != nil {
				return map[string]any{"error": err.Error()}
			}
			return out
		},
	})
}

// gmActorsValidSort is the sortBy allowlist. Validated up front (before any query
// fires) so an unknown key is a clean argument error, not a silent fall-through.
// The sort itself runs in Go over the folded actors (no ORDER BY interpolation →
// sidesteps the aggregate-alias ORDER BY footgun). `invites` is included beyond the
// backlog's named set because the tool's stated use case ("a bot mass-inviting")
// wants an invite-count ranking directly.
func gmActorsValidSort(s string) bool {
	switch s {
	case "officerEvents", "invites", "promotes", "uninvites", "events":
		return true
	}
	return false
}

// collectWowGuildMembershipActors runs the three-stage composite. Split out so
// tests can drive it with sqlmock and a fixed `now`. guildID is *int so "unset"
// (all guilds) is distinguishable from an explicit filter.
func collectWowGuildMembershipActors(ctx context.Context, db *sql.DB, now time.Time, guildID *int, sortBy string, sinceHours, minEvents, topN int) (map[string]any, error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire conn: %w", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "USE `acore_characters`"); err != nil {
		return nil, fmt.Errorf("USE acore_characters: %w", err)
	}

	// Stage 1: scan + fold per actor. guildId + sinceHours narrow the scan in SQL.
	q := gmActorsScanBase
	args := make([]any, 0, 3)
	where := make([]string, 0, 2)
	if guildID != nil {
		where = append(where, "guildid = ?")
		args = append(args, *guildID)
	}
	var cutoff int64
	if sinceHours > 0 {
		// TimeStamp is a uint32 UNIX-SECONDS column, so bind an int64 second count,
		// NOT a time.Time (which the driver would render as a DATETIME string and
		// mis-compare against the integer column).
		cutoff = now.Unix() - int64(sinceHours)*3600
		where = append(where, "TimeStamp >= ?")
		args = append(args, cutoff)
	}
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	q += " LIMIT ?"
	args = append(args, gmActorsScanCap+1)

	rows, err := conn.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("guild_eventlog query: %w", err)
	}
	defer rows.Close()

	actors := map[int64]*gmActor{}
	scanned := 0
	truncated := false
	officerEvents := 0
	selfEvents := 0
	for rows.Next() {
		if scanned >= gmActorsScanCap {
			truncated = true
			break
		}
		var eventType int
		var guildRow, playerGuid, ts int64
		if err := rows.Scan(&guildRow, &eventType, &playerGuid, &ts); err != nil {
			return nil, fmt.Errorf("guild_eventlog scan: %w", err)
		}
		scanned++
		switch eventType {
		case gmaInvitePlayer, gmaPromotePlayer, gmaDemotePlayer, gmaUninvitePlayer:
			officerEvents++
		case gmaJoinGuild, gmaLeaveGuild:
			selfEvents++
		}
		act := actors[playerGuid]
		if act == nil {
			act = &gmActor{PlayerGuid: playerGuid}
			actors[playerGuid] = act
		}
		act.fold(eventType, guildRow, ts)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("guild_eventlog iter: %w", err)
	}

	// Stage 2: minEvents filter, sort by the chosen key, slice to topN. distinctActors
	// is the honest realm-wide total (every folded actor); matchedActors is the pool
	// that met the minEvents floor and was ranked from.
	distinctActors := len(actors)
	top, matched := topNGuildMembershipActors(actors, sortBy, minEvents, topN)

	// Stage 3: resolve names (still in acore_characters) then accounts (acore_auth).
	unresolved := 0
	if len(top) > 0 {
		if err := annotateMembershipActorCharacters(ctx, conn, top); err != nil {
			return nil, err
		}
		if _, err := conn.ExecContext(ctx, "USE `acore_auth`"); err != nil {
			return nil, fmt.Errorf("USE acore_auth: %w", err)
		}
		if err := annotateMembershipActorAccounts(ctx, conn, top); err != nil {
			return nil, err
		}
		for _, act := range top {
			act.finalize(now)
			if act.CharacterName == "" {
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
		"distinctActors":     distinctActors,
		"matchedActors":      matched,
		"scanned":            scanned,
		"scanCap":            gmActorsScanCap,
		"truncated":          truncated,
		"unresolved":         unresolved,
		"totalEvents":        scanned,
		"totalOfficerEvents": officerEvents,
		"totalSelfEvents":    selfEvents,
		"actors":             top,
	}
	if guildID != nil {
		out["guildId"] = *guildID
	}
	if sinceHours > 0 {
		out["sinceCutoff"] = formatUnixISO(cutoff)
	}
	return out, nil
}

// topNGuildMembershipActors filters the folded actors to those with >= minEvents,
// sorts by the chosen key (all descending) with PlayerGuid-asc as a deterministic
// tiebreaker (without it Go's randomized map iteration would flake tests on ties),
// slices to n, and returns the slice plus the pre-slice matched count (so the
// caller can report "ranking the top 25 of 540 actors"). Always a non-nil slice.
func topNGuildMembershipActors(m map[int64]*gmActor, sortBy string, minEvents, n int) ([]*gmActor, int) {
	out := make([]*gmActor, 0, len(m))
	for _, a := range m {
		if a.TotalEvents >= minEvents {
			out = append(out, a)
		}
	}
	matched := len(out)
	sort.Slice(out, func(i, j int) bool {
		return gmActorLess(out[i], out[j], sortBy)
	})
	if len(out) > n {
		out = out[:n]
	}
	return out, matched
}

// gmActorLess ranks a before b for the given sort key (all descending), falling
// back to PlayerGuid-asc so ties are deterministic.
func gmActorLess(a, b *gmActor, sortBy string) bool {
	switch sortBy {
	case "invites":
		if a.Invites != b.Invites {
			return a.Invites > b.Invites
		}
	case "promotes":
		if a.Promotes != b.Promotes {
			return a.Promotes > b.Promotes
		}
	case "uninvites":
		if a.Uninvites != b.Uninvites {
			return a.Uninvites > b.Uninvites
		}
	case "events":
		if a.TotalEvents != b.TotalEvents {
			return a.TotalEvents > b.TotalEvents
		}
	default: // officerEvents
		if a.OfficerEvents != b.OfficerEvents {
			return a.OfficerEvents > b.OfficerEvents
		}
	}
	return a.PlayerGuid < b.PlayerGuid
}

// annotateMembershipActorCharacters batch-resolves the top actors' PlayerGuids to
// characters.name + account in one IN(...) query. Mirrors the wow_guild_bank_top_actors
// batch-IN idiom (whose annotateActorCharacters is bound to *gbActor, so this
// membership-actor variant is its own function): one round trip, a guid->row map,
// then a second pass to set the fields. A guid with no row (a deleted character
// whose membership events outlive it) is left with an empty name + account 0 —
// surfaced via the top-level `unresolved` count, not dropped.
func annotateMembershipActorCharacters(ctx context.Context, conn *sql.Conn, actors []*gmActor) error {
	if len(actors) == 0 {
		return nil
	}
	var b strings.Builder
	b.Grow(64 + len(actors)*2)
	b.WriteString("SELECT guid, name, account FROM `characters` WHERE guid IN (")
	args := make([]any, 0, len(actors))
	for i, a := range actors {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteByte('?')
		args = append(args, a.PlayerGuid)
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
	byGuid := make(map[int64]charRow, len(actors))
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
	for _, a := range actors {
		if c, ok := byGuid[a.PlayerGuid]; ok {
			a.CharacterName = c.name
			a.AccountID = c.account
		}
	}
	return nil
}

// annotateMembershipActorAccounts batch-resolves the resolved actors' account ids
// to acore_auth.account.username in one IN(...) query and flags isBot when the
// username carries the RNDBOT prefix (case-insensitive). Actors whose character row
// was missing (account 0) are skipped — they carry no username and isBot stays
// false. Distinct account ids are deduped so two actor-characters on the same
// account bind the id once.
func annotateMembershipActorAccounts(ctx context.Context, conn *sql.Conn, actors []*gmActor) error {
	ids := make([]int64, 0, len(actors))
	seen := make(map[int64]bool, len(actors))
	for _, a := range actors {
		if a.AccountID != 0 && !seen[a.AccountID] {
			seen[a.AccountID] = true
			ids = append(ids, a.AccountID)
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
	for _, a := range actors {
		if u, ok := byID[a.AccountID]; ok {
			a.AccountUsername = u
			a.IsBot = strings.HasPrefix(strings.ToUpper(u), gmActorsBotPrefix)
		}
	}
	return nil
}
