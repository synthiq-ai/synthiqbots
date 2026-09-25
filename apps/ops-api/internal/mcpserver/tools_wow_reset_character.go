package mcpserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

const wowResetCharacterTimeout = 30 * time.Second

// AzerothCore AtLoginFlags bits (see Player.h). Setting these on the characters
// row makes the worldserver reseed the race/class DEFAULT spell + skill set and
// reset talents the next time the character logs in — exactly what the
// `.reset spells` / `.reset talents` GM commands queue. We rely on the engine
// for that reseed instead of re-implementing character creation in SQL.
const (
	atLoginResetSpells  = 0x02
	atLoginResetTalents = 0x04
)

// characterWipeTables are the per-character progress/state tables a TOTAL reset
// clears, paired with the column holding the character GUID. Identity / social /
// account tables are deliberately NOT listed (see keep-list in the tool
// description): characters (updated in place), character_banned,
// character_declinedname, character_social, character_settings,
// character_account_data, character_homebind. item_instance and mail use
// different key columns and are handled separately.
var characterWipeTables = []struct{ table, col string }{
	{"character_achievement", "guid"},
	{"character_achievement_offline_updates", "guid"},
	{"character_achievement_progress", "guid"},
	{"character_action", "guid"},
	{"character_arena_stats", "guid"},
	{"character_aura", "guid"},
	{"character_battleground_random", "guid"},
	{"character_brew_of_the_month", "guid"},
	{"character_entry_point", "guid"},
	{"character_equipmentsets", "guid"},
	{"character_gifts", "guid"},
	{"character_glyphs", "guid"},
	{"character_instance", "guid"},
	{"character_inventory", "guid"},
	{"character_queststatus", "guid"},
	{"character_queststatus_daily", "guid"},
	{"character_queststatus_monthly", "guid"},
	{"character_queststatus_rewarded", "guid"},
	{"character_queststatus_seasonal", "guid"},
	{"character_queststatus_weekly", "guid"},
	{"character_reputation", "guid"},
	{"character_skills", "guid"},
	{"character_spell", "guid"},
	{"character_spell_cooldown", "guid"},
	{"character_stats", "guid"},
	{"character_talent", "guid"},
	{"character_pet", "owner"},
}

// RegisterWowResetCharacterTool registers `wow_reset_character` — a total,
// in-place "start over from level 1" reset that PRESERVES the character GUID.
// Preserving the GUID is the whole point: gateway/tactical/leader bots are wired
// by GUID in mod_ollama_chat.conf, so deleting + recreating a character would
// break the wiring, but resetting it in place does not. The tool therefore works
// on ordinary player characters AND Ollama-wired bots alike.
//
// Engine-cooperative: rather than hand-reseed spells/skills (fragile), it sets
// the AT_LOGIN_RESET_SPELLS|RESET_TALENTS flags so the worldserver reseeds the
// race/class defaults on next login. Level/stat recompute also happens engine
// side on login (UpdateAllStats clamps current health/power to the level-1 max).
//
// Writes go through the ops_rw pool (DBDeps.ExecDB) inside a single transaction,
// so a mid-wipe failure rolls back rather than leaving a half-reset character.
func RegisterWowResetCharacterTool(reg *Registry, deps DBDeps) {
	reg.Register(Tool{
		Name: "wow_reset_character",
		Description: "TOTAL in-place character reset — back to a fresh level 1 while KEEPING the " +
			"character GUID (so Ollama-wired bots stay wired). Takes `guid` OR `name`. " +
			"Requires `confirm:true` to write; pass `dry_run:true` to preview the row counts " +
			"that WOULD be cleared without changing anything. Refuses if the character is " +
			"online (set `force_online:true` only after you despawn it — for a wired bot, " +
			"`.playerbots bot remove <name>` via leader_admin_command first). " +
			"WIPES: all quests, inventory + item_instance, mail, achievements, reputation, " +
			"skills, spells, talents, glyphs, auras, action bars, instance lockouts, equipment " +
			"sets, pets, cached stats, arena/honor. RESETS characters: level→1, xp→0, money→0, " +
			"honor/kills→0, position→race/class start, replays the race intro cinematic, and sets " +
			"AT_LOGIN_RESET_SPELLS|RESET_TALENTS so the worldserver reseeds default spells/skills/talents " +
			"on next login. " +
			"KEEPS: name, bans, friends/ignore (character_social), per-char settings, UI/macros " +
			"(character_account_data). The character must log in once after the reset to trigger " +
			"the engine-side spell/talent reseed and stat recompute.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{
			"guid":{"type":"integer","description":"Character GUID (alternative to name)"},
			"name":{"type":"string","description":"Character name (exact, case-insensitive)"},
			"dry_run":{"type":"boolean","description":"Preview counts only; no writes. Does not require confirm."},
			"confirm":{"type":"boolean","description":"MUST be true to actually perform the reset"},
			"force_online":{"type":"boolean","description":"Allow resetting an online character (unsafe — engine overwrites on save). Despawn first."}
		}}`),
		Annotations: AnnAction(false),
		Destructive: true,
		Handler: func(ctx context.Context, raw json.RawMessage, _ string) any {
			var a struct {
				Guid        int64  `json:"guid"`
				Name        string `json:"name"`
				DryRun      bool   `json:"dry_run"`
				Confirm     bool   `json:"confirm"`
				ForceOnline bool   `json:"force_online"`
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
			if !a.DryRun && !a.Confirm {
				return map[string]any{"error": "confirm:true required (or pass dry_run:true to preview)"}
			}
			if deps.ExecDB == nil {
				return map[string]any{"error": "ops_rw pool not configured (DB_EXEC_DSN empty?)"}
			}

			c, cancel := context.WithTimeout(ctx, wowResetCharacterTimeout)
			defer cancel()

			conn, err := deps.ExecDB.Conn(c)
			if err != nil {
				return map[string]any{"error": "acquire conn: " + err.Error()}
			}
			defer conn.Close()
			if _, err := conn.ExecContext(c, "USE `acore_characters`"); err != nil {
				return map[string]any{"error": "USE acore_characters: " + err.Error()}
			}

			// Resolve the character.
			var (
				guid               int64
				account            int64
				name               string
				race, class, level int
				online             int
			)
			sel := "SELECT guid, account, name, race, class, level, online FROM characters WHERE "
			var selArg any
			if a.Guid != 0 {
				sel += "guid = ?"
				selArg = a.Guid
			} else {
				sel += "name = ?"
				selArg = a.Name
			}
			if err := conn.QueryRowContext(c, sel, selArg).
				Scan(&guid, &account, &name, &race, &class, &level, &online); err != nil {
				if err == sql.ErrNoRows {
					return map[string]any{"error": "character not found"}
				}
				return map[string]any{"error": "lookup character: " + err.Error()}
			}
			if online == 1 && !a.ForceOnline {
				return map[string]any{
					"error": "character is online — refusing to reset",
					"guid":  guid,
					"name":  name,
					"hint":  "Log the character out first. For an Ollama-wired bot, despawn it (e.g. `.playerbots bot remove " + name + "` via leader_admin_command), then retry. Pass force_online:true to override (unsafe — the worldserver overwrites the row on save).",
				}
			}

			// Race/class creation start point (acore_world.playercreateinfo).
			var (
				startMap                       int64
				startZone                      int64
				startX, startY, startZ, startO float64
				havePos                        bool
			)
			if err := conn.QueryRowContext(c,
				"SELECT map, zone, position_x, position_y, position_z, orientation "+
					"FROM acore_world.playercreateinfo WHERE race = ? AND class = ?",
				race, class).Scan(&startMap, &startZone, &startX, &startY, &startZ, &startO); err != nil {
				if err != sql.ErrNoRows {
					return map[string]any{"error": "lookup start position: " + err.Error()}
				}
				// No row (unusual) — leave position untouched rather than guessing.
			} else {
				havePos = true
			}

			// Build the full work list: progress tables + item_instance + mail.
			type op struct {
				label  string
				delete string
				count  string
				arg    any
			}
			ops := make([]op, 0, len(characterWipeTables)+3)
			for _, t := range characterWipeTables {
				ops = append(ops, op{
					label:  t.table,
					delete: fmt.Sprintf("DELETE FROM `%s` WHERE `%s` = ?", t.table, t.col),
					count:  fmt.Sprintf("SELECT COUNT(*) FROM `%s` WHERE `%s` = ?", t.table, t.col),
					arg:    guid,
				})
			}
			ops = append(ops,
				op{
					label:  "item_instance",
					delete: "DELETE FROM `item_instance` WHERE `owner_guid` = ?",
					count:  "SELECT COUNT(*) FROM `item_instance` WHERE `owner_guid` = ?",
					arg:    guid,
				},
				op{
					label:  "mail_items",
					delete: "DELETE FROM `mail_items` WHERE `receiver` = ?",
					count:  "SELECT COUNT(*) FROM `mail_items` WHERE `receiver` = ?",
					arg:    guid,
				},
				op{
					label:  "mail",
					delete: "DELETE FROM `mail` WHERE `receiver` = ?",
					count:  "SELECT COUNT(*) FROM `mail` WHERE `receiver` = ?",
					arg:    guid,
				},
			)

			// Dry run: just count, no writes, no transaction.
			if a.DryRun {
				counts := map[string]int64{}
				var total int64
				for _, o := range ops {
					var n int64
					if err := conn.QueryRowContext(c, o.count, o.arg).Scan(&n); err != nil {
						return map[string]any{"error": fmt.Sprintf("count %s: %v", o.label, err)}
					}
					if n > 0 {
						counts[o.label] = n
						total += n
					}
				}
				out := map[string]any{
					"dry_run":           true,
					"guid":              guid,
					"name":              name,
					"account":           account,
					"current_level":     level,
					"online":            online == 1,
					"rows_would_delete": counts,
					"total_rows":        total,
					"position_reset_to": positionSummary(havePos, startMap, startZone),
					"note":              "no changes made; re-run with confirm:true to perform the reset",
				}
				return out
			}

			// Real reset — one transaction so a partial failure rolls back.
			tx, err := conn.BeginTx(c, nil)
			if err != nil {
				return map[string]any{"error": "begin tx: " + err.Error()}
			}
			deleted := map[string]int64{}
			for _, o := range ops {
				res, err := tx.ExecContext(c, o.delete, o.arg)
				if err != nil {
					_ = tx.Rollback()
					return map[string]any{"error": fmt.Sprintf("delete %s: %v (rolled back)", o.label, err)}
				}
				if n, _ := res.RowsAffected(); n > 0 {
					deleted[o.label] = n
				}
			}

			// Reset the characters row itself.
			upd := "UPDATE characters SET " +
				"level = 1, xp = 0, money = 0, " +
				"totaltime = 0, leveltime = 0, " +
				"arenaPoints = 0, totalHonorPoints = 0, todayHonorPoints = 0, yesterdayHonorPoints = 0, " +
				"totalKills = 0, todayKills = 0, yesterdayKills = 0, " +
				"talentGroupsCount = 1, activeTalentGroup = 0, resettalents_cost = 0, resettalents_time = 0, " +
				"extraBonusTalentCount = 0, grantableLevels = 0, " +
				"equipmentCache = '', exploredZones = '', taximask = '', " +
				"chosenTitle = 0, knownTitles = '', knownCurrencies = 0, watchedFaction = 0, drunk = 0, " +
				// cinematic = 0 makes the race intro movie play again on next login,
				// exactly like a brand-new character's first login.
				"cinematic = 0, " +
				"at_login = (at_login | ?) "
			args := []any{atLoginResetSpells | atLoginResetTalents}
			if havePos {
				upd += ", map = ?, zone = ?, position_x = ?, position_y = ?, position_z = ?, orientation = ?, instance_id = 0 "
				args = append(args, startMap, startZone, startX, startY, startZ, startO)
			}
			upd += "WHERE guid = ?"
			args = append(args, guid)
			if _, err := tx.ExecContext(c, upd, args...); err != nil {
				_ = tx.Rollback()
				return map[string]any{"error": "update characters: " + err.Error() + " (rolled back)"}
			}

			if err := tx.Commit(); err != nil {
				return map[string]any{"error": "commit: " + err.Error()}
			}

			return map[string]any{
				"ok":                true,
				"guid":              guid,
				"name":              name,
				"account":           account,
				"previous_level":    level,
				"new_level":         1,
				"tables_cleared":    deleted,
				"position_reset_to": positionSummary(havePos, startMap, startZone),
				"reseed":            "AT_LOGIN_RESET_SPELLS|RESET_TALENTS set — log the character in once to reseed default spells/skills/talents and recompute stats",
			}
		},
	})
}

func positionSummary(have bool, mapID, zone int64) any {
	if !have {
		return "unchanged (no playercreateinfo row for race/class)"
	}
	return map[string]any{"map": mapID, "zone": zone}
}
