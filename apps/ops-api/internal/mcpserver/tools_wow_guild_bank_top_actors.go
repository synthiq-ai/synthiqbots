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
	gbTopActorsTimeout     = 10 * time.Second
	gbTopActorsScanCap     = 200000 // hard ceiling on guild_bank_eventlog rows folded into one snapshot
	gbTopActorsDefaultTopN = 25
	gbTopActorsMaxTopN     = 200
	gbTopActorsDefaultMin  = 1        // surface only actors with at least one event
	gbTopActorsBotPrefix   = "RNDBOT" // mod-playerbots' default RandomBotAccountPrefix
)

// guild_bank_eventlog.EventType values (AC Guild.h GuildBankEventLogTypes,
// source-anchored in cron-state research-cache/acore-guild-logs.json). Money
// events {4,5,6} carry a copper amount in ItemOrMoney; item events {1,2,3,7}
// carry an item-template entry (a COUNT here, never summed). 8 (UNK1) / 9
// (BUY_SLOT) are neither a money move nor an actor item move — folded into
// totalEvents only. Named with the gbta* (guild_bank_top_actors) prefix so this
// file defines NO symbol an unmerged guild-bank sibling (wow_guild_bank_activity,
// #345) also defines — both merge into the same package.
const (
	gbtaDepositItem   = 1
	gbtaWithdrawItem  = 2
	gbtaMoveItem      = 3
	gbtaDepositMoney  = 4
	gbtaWithdrawMoney = 5
	gbtaRepairMoney   = 6
	gbtaMoveItem2     = 7
)

// gbTopActorsScanBase is the per-event scan over acore_characters.guild_bank_eventlog.
// Only the columns this tool folds per actor — guildid (for the optional guild
// filter + distinct-guild count), EventType (money-vs-item classification),
// PlayerGuid (the acting characters.guid), ItemOrMoney (copper for money events),
// TimeStamp (unix seconds, for the window filter + recency). An optional WHERE
// (guildid / TimeStamp) and the trailing `LIMIT ?` (bound to scanCap+1 so
// truncation is detectable without a separate COUNT) are appended by the builder.
const gbTopActorsScanBase = "SELECT guildid, EventType, PlayerGuid, ItemOrMoney, TimeStamp FROM `guild_bank_eventlog`"

// gbActor is one ranked guild-bank actor (a characters.guid appearing as
// guild_bank_eventlog.PlayerGuid). Money sums are copper (from the bank's
// perspective: moneyIn = deposits, moneyOut = withdrawals + repairs); the item
// event types are COUNTS. characterName / accountId / accountUsername / isBot are
// folded in from the two batch lookups (empty name + account 0 when the acting
// character row is gone — a deleted character whose bank events outlive it,
// counted in `unresolved`).
type gbActor struct {
	PlayerGuid        int64  `json:"playerGuid"`
	CharacterName     string `json:"characterName"`
	AccountID         int64  `json:"accountId"`
	AccountUsername   string `json:"accountUsername"`
	IsBot             bool   `json:"isBot"`
	TotalEvents       int    `json:"totalEvents"`
	ItemDeposits      int    `json:"itemDeposits"`
	ItemWithdrawals   int    `json:"itemWithdrawals"`
	ItemMoves         int    `json:"itemMoves"`
	MoneyInCopper     int64  `json:"moneyInCopper"`
	MoneyInGold       string `json:"moneyInGold"`
	MoneyOutCopper    int64  `json:"moneyOutCopper"`
	MoneyOutGold      string `json:"moneyOutGold"`
	RepairMoneyCopper int64  `json:"repairMoneyCopper"`
	RepairMoneyGold   string `json:"repairMoneyGold"`
	NetMoneyCopper    int64  `json:"netMoneyCopper"`
	NetMoneyGold      string `json:"netMoneyGold"`
	DistinctGuilds    int    `json:"distinctGuilds"`
	FirstActivityISO  string `json:"firstActivity"`
	LastActivityISO   string `json:"lastActivity"`
	RecencyHuman      string `json:"recencyHuman"`

	// unexported fold state — the min/max event timestamps (recency sort + ISO
	// stamps) and the set of guilds this actor touched. Not serialized.
	firstTS int64
	lastTS  int64
	guilds  map[int64]struct{}
}

// fold adds one guild_bank_eventlog row to the actor's running totals. amount is
// ItemOrMoney: a copper value for money events {4,5,6}, an item-template entry
// for item events {1,2,3,7} (ignored — only the event is counted).
func (a *gbActor) fold(eventType int, guildID, amount, ts int64) {
	a.TotalEvents++
	switch eventType {
	case gbtaDepositItem:
		a.ItemDeposits++
	case gbtaWithdrawItem:
		a.ItemWithdrawals++
	case gbtaMoveItem, gbtaMoveItem2:
		a.ItemMoves++
	case gbtaDepositMoney:
		a.MoneyInCopper += amount
	case gbtaWithdrawMoney:
		a.MoneyOutCopper += amount
	case gbtaRepairMoney:
		a.MoneyOutCopper += amount
		a.RepairMoneyCopper += amount
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

// finalize renders the derived fields (net money, gold strings, distinct-guild
// count, activity ISO stamps + recency) once, after the top-N slice, so we don't
// format copper for actors that never make the cut. now anchors the recency delta.
func (a *gbActor) finalize(now time.Time) {
	a.NetMoneyCopper = a.MoneyInCopper - a.MoneyOutCopper
	a.MoneyInGold = formatMoney(a.MoneyInCopper)
	a.MoneyOutGold = formatMoney(a.MoneyOutCopper)
	a.RepairMoneyGold = formatMoney(a.RepairMoneyCopper)
	a.NetMoneyGold = formatMoney(a.NetMoneyCopper)
	a.DistinctGuilds = len(a.guilds)
	a.FirstActivityISO = formatUnixISO(a.firstTS)
	a.LastActivityISO = formatUnixISO(a.lastTS)
	if a.lastTS > 0 {
		a.RecencyHuman = formatDuration(now.Unix() - a.lastTS)
	}
}

// RegisterWowGuildBankTopActorsTool registers `wow_guild_bank_top_actors` — a
// read-only ranking of the busiest guild-bank actors by event churn, with
// character-name + bot-vs-player resolution folded in.
//
// The WHO-moves-the-gold view of the guild bank. wow_guild_bank_summary counts
// money + tabs per GUILD but names no actor; the guild-bank family's activity
// tool rolls the eventlog up to guild grain (distinctActors is a COUNT, not a
// list). This promotes the ACTOR axis into a dedicated leaderboard: GROUP BY
// PlayerGuid over guild_bank_eventlog, money events {4,5,6} split from item
// events {1,2,3,7} exactly as AC's Guild.h IsMoneyEvent classifies them, joined
// to characters + acore_auth.account so an operator sees the top depositors /
// withdrawers realm-wide and whether bots (RNDBOT prefix) are churning guild
// banks — the guild-bank counterpart to ah_market_top_sellers' WHO view of the AH.
func RegisterWowGuildBankTopActorsTool(reg *Registry, deps DBDeps) {
	reg.Register(Tool{
		Name: "wow_guild_bank_top_actors",
		Description: "Top-N guild-bank actors by deposit/withdraw churn, aggregated from " +
			"acore_characters.guild_bank_eventlog over the ops_ro pool, with character-name + " +
			"bot-vs-player resolution. Three-stage composite: (1) fold every guild-bank event per " +
			"PlayerGuid in Go from one scan, splitting money events (EventType 4/5/6 → moneyIn / " +
			"moneyOut / repair, copper summed) from item events (1/2/3/7 → itemDeposits / " +
			"itemWithdrawals / itemMoves, counted never summed) per AC Guild.h; (2) batch-resolve the " +
			"top-N PlayerGuids to characters.name + account via an IN(...) query on " +
			"acore_characters.characters; (3) batch-resolve those accounts to acore_auth.account.username " +
			"in one IN(...) query and flag isBot when the username starts with \"RNDBOT\" (mod-playerbots' " +
			"RandomBotAccountPrefix). The WHO-moves-the-gold view of the guild bank: wow_guild_bank_summary " +
			"counts money + tabs per guild but names no actor; this ranks the busiest depositors / " +
			"withdrawers realm-wide (or within one guildId) with per-actor moneyIn / moneyOut / repair / " +
			"netMoney (copper + human gold strings), item deposit/withdraw/move counts, distinct guilds, " +
			"and first/last activity + recency. On a mod-ah-bot / mod-playerbots server isBot makes " +
			"visible whether bots are churning guild banks, without a separate wow_bot_lookup. An actor " +
			"whose acting character row is gone (deleted character with surviving bank events) surfaces " +
			"with an empty characterName + accountId 0 and is counted in `unresolved`. Args: topN " +
			"(default 25, max 200), sortBy (events|moneyOut|moneyIn|recency, default events), guildId " +
			"(optional — restrict to one guild's actors; must be positive), sinceHours (optional window — " +
			"only events with TimeStamp within the last N hours; 0 = the whole bounded log), minEvents " +
			"(default 1, clamps to >=1). Scan capped at 200000 rows; truncated:true flags an incomplete " +
			"fold. Read-only, 10 s timeout.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{
			"topN":{"type":"integer","description":"Max actors to rank (default 25, max 200)"},
			"sortBy":{"type":"string","description":"Ranking key: events (default, totalEvents desc), moneyOut (withdrawals+repairs copper desc), moneyIn (deposits copper desc), recency (most-recent activity first)"},
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
				sortBy = "events"
			}
			if !gbTopActorsValidSort(sortBy) {
				return map[string]any{"error": "sortBy must be one of: events, moneyOut, moneyIn, recency"}
			}
			if a.GuildID != nil && *a.GuildID <= 0 {
				return map[string]any{"error": "guildId must be a positive integer"}
			}
			topN := a.TopN
			if topN <= 0 {
				topN = gbTopActorsDefaultTopN
			}
			if topN > gbTopActorsMaxTopN {
				topN = gbTopActorsMaxTopN
			}
			minEvents := a.MinEvents
			if minEvents < gbTopActorsDefaultMin {
				minEvents = gbTopActorsDefaultMin
			}
			sinceHours := a.SinceHours
			if sinceHours < 0 {
				sinceHours = 0
			}
			c, cancel := context.WithTimeout(ctx, gbTopActorsTimeout)
			defer cancel()
			out, err := collectWowGuildBankTopActors(c, deps.QueryDB, time.Now(), a.GuildID, sortBy, sinceHours, minEvents, topN)
			if err != nil {
				return map[string]any{"error": err.Error()}
			}
			return out
		},
	})
}

// gbTopActorsValidSort is the sortBy allowlist. Validated up front (before any
// query fires) so an unknown key is a clean argument error, not a silent
// fall-through. The sort itself runs in Go over the folded actors (no ORDER BY
// interpolation → sidesteps the aggregate-alias ORDER BY footgun).
func gbTopActorsValidSort(s string) bool {
	switch s {
	case "events", "moneyOut", "moneyIn", "recency":
		return true
	}
	return false
}

// collectWowGuildBankTopActors runs the three-stage composite. Split out so tests
// can drive it with sqlmock and a fixed `now`. guildID is *int so "unset" (all
// guilds) is distinguishable from an explicit filter.
func collectWowGuildBankTopActors(ctx context.Context, db *sql.DB, now time.Time, guildID *int, sortBy string, sinceHours, minEvents, topN int) (map[string]any, error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire conn: %w", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "USE `acore_characters`"); err != nil {
		return nil, fmt.Errorf("USE acore_characters: %w", err)
	}

	// Stage 1: scan + fold per actor. guildId + sinceHours narrow the scan in SQL.
	q := gbTopActorsScanBase
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
		q += " WHERE " + strings.Join(where, " AND ")
	}
	q += " LIMIT ?"
	args = append(args, gbTopActorsScanCap+1)

	rows, err := conn.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("guild_bank_eventlog query: %w", err)
	}
	defer rows.Close()

	actors := map[int64]*gbActor{}
	scanned := 0
	truncated := false
	var totalMoneyMoved int64
	itemEvents := 0
	moneyEvents := 0
	for rows.Next() {
		if scanned >= gbTopActorsScanCap {
			truncated = true
			break
		}
		var eventType int
		var guildRow, playerGuid, amount, ts int64
		if err := rows.Scan(&guildRow, &eventType, &playerGuid, &amount, &ts); err != nil {
			return nil, fmt.Errorf("guild_bank_eventlog scan: %w", err)
		}
		scanned++
		switch eventType {
		case gbtaDepositMoney, gbtaWithdrawMoney, gbtaRepairMoney:
			moneyEvents++
			totalMoneyMoved += amount
		case gbtaDepositItem, gbtaWithdrawItem, gbtaMoveItem, gbtaMoveItem2:
			itemEvents++
		}
		act := actors[playerGuid]
		if act == nil {
			act = &gbActor{PlayerGuid: playerGuid}
			actors[playerGuid] = act
		}
		act.fold(eventType, guildRow, amount, ts)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("guild_bank_eventlog iter: %w", err)
	}

	// Stage 2: minEvents filter, sort by the chosen key, slice to topN. distinctActors
	// is the honest realm-wide total (every folded actor); matchedActors is the pool
	// that met the minEvents floor and was ranked from.
	distinctActors := len(actors)
	top, matched := topNGuildBankActors(actors, sortBy, minEvents, topN)

	// Stage 3: resolve names (still in acore_characters) then accounts (acore_auth).
	unresolved := 0
	if len(top) > 0 {
		if err := annotateActorCharacters(ctx, conn, top); err != nil {
			return nil, err
		}
		if _, err := conn.ExecContext(ctx, "USE `acore_auth`"); err != nil {
			return nil, fmt.Errorf("USE acore_auth: %w", err)
		}
		if err := annotateActorAccounts(ctx, conn, top); err != nil {
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
		"asOf":                  now.UTC().Format(time.RFC3339),
		"topN":                  topN,
		"sortBy":                sortBy,
		"minEvents":             minEvents,
		"sinceHours":            sinceHours,
		"distinctActors":        distinctActors,
		"matchedActors":         matched,
		"scanned":               scanned,
		"scanCap":               gbTopActorsScanCap,
		"truncated":             truncated,
		"unresolved":            unresolved,
		"totalEvents":           scanned,
		"totalItemEvents":       itemEvents,
		"totalMoneyEvents":      moneyEvents,
		"totalMoneyMovedCopper": totalMoneyMoved,
		"totalMoneyMovedGold":   formatMoney(totalMoneyMoved),
		"actors":                top,
	}
	if guildID != nil {
		out["guildId"] = *guildID
	}
	if sinceHours > 0 {
		out["sinceCutoff"] = formatUnixISO(cutoff)
	}
	return out, nil
}

// topNGuildBankActors filters the folded actors to those with >= minEvents, sorts
// by the chosen key (all descending) with PlayerGuid-asc as a deterministic
// tiebreaker (without it Go's randomized map iteration would flake tests on ties),
// slices to n, and returns the slice plus the pre-slice matched count (so the
// caller can report "ranking the top 25 of 540 actors"). Always a non-nil slice.
func topNGuildBankActors(m map[int64]*gbActor, sortBy string, minEvents, n int) ([]*gbActor, int) {
	out := make([]*gbActor, 0, len(m))
	for _, a := range m {
		if a.TotalEvents >= minEvents {
			out = append(out, a)
		}
	}
	matched := len(out)
	sort.Slice(out, func(i, j int) bool {
		return gbActorLess(out[i], out[j], sortBy)
	})
	if len(out) > n {
		out = out[:n]
	}
	return out, matched
}

// gbActorLess ranks a before b for the given sort key (all descending), falling
// back to PlayerGuid-asc so ties are deterministic. Money/recency sort on the
// unexported/copper fold state, which is populated before finalize runs.
func gbActorLess(a, b *gbActor, sortBy string) bool {
	switch sortBy {
	case "moneyOut":
		if a.MoneyOutCopper != b.MoneyOutCopper {
			return a.MoneyOutCopper > b.MoneyOutCopper
		}
	case "moneyIn":
		if a.MoneyInCopper != b.MoneyInCopper {
			return a.MoneyInCopper > b.MoneyInCopper
		}
	case "recency":
		if a.lastTS != b.lastTS {
			return a.lastTS > b.lastTS
		}
	default: // events
		if a.TotalEvents != b.TotalEvents {
			return a.TotalEvents > b.TotalEvents
		}
	}
	return a.PlayerGuid < b.PlayerGuid
}

// annotateActorCharacters batch-resolves the top actors' PlayerGuids to
// characters.name + account in one IN(...) query. Mirrors the ah_market_top_sellers
// batch-IN idiom: one round trip, a guid->row map, then a second pass to set the
// fields. A guid with no row (a deleted character whose bank events outlive it) is
// left with an empty name + account 0 — surfaced via the top-level `unresolved`
// count, not dropped.
func annotateActorCharacters(ctx context.Context, conn *sql.Conn, actors []*gbActor) error {
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

// annotateActorAccounts batch-resolves the resolved actors' account ids to
// acore_auth.account.username in one IN(...) query and flags isBot when the
// username carries the RNDBOT prefix (case-insensitive). Actors whose character
// row was missing (account 0) are skipped — they carry no username and isBot stays
// false. Distinct account ids are deduped so two actor-characters on the same
// account bind the id once.
func annotateActorAccounts(ctx context.Context, conn *sql.Conn, actors []*gbActor) error {
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
			a.IsBot = strings.HasPrefix(strings.ToUpper(u), gbTopActorsBotPrefix)
		}
	}
	return nil
}
