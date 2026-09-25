# Autonomous Bots

How to drive a WotLK playerbot fleet from outside the game — no human
character online, no real WoW client running. Pair this with Claude Desktop
(or any MCP-capable client) and you can run farming / questing / fleet
admin over an MCP endpoint.

> **Companion-mode alternative: `docs/tactical.md`.** If you want bots that
> are *active only when you're online* and act like teammates (narrating
> plans, asking you things, making squad calls) rather than autonomous
> farmers, tactical adds a free local-Ollama loop on top of this same MCP
> registry. The tool set overlaps heavily; the big difference is that
> tactical is presence-gated and the paid gateway is reactive-only.

---

## Architecture at a glance

```
+-------------------------+      MCP over HTTPS       +-------------------------+
|  Claude Desktop / CLI   |  bearer + IP allowlist    |  Traefik (proxy)        |
|  Agent prompt + tools   | -----------------------→  |  mcp.wow.example.com     |
+-------------------------+                           +-----------+-------------+
                                                                  |
                                                                  | socat bridge
                                                                  v
+---------------------------------------------------------------------------+
|  ac-worldserver                                                           |
|                                                                           |
|  +---------------------+  JSON-RPC 2.0   +----------------------------+   |
|  |  MCP server (18790) |  /mcp           |  tool registry             |   |
|  |  bearer-token auth  |  ←───────────── |  get_fleet_status,         |   |
|  +---------------------+                 |  leader_command_all, ...    |   |
|                                          +---+------------------+-----+   |
|                                              |                  |          |
|                                              v                  v          |
|                                     ObjectAccessor / PlayerbotsMgr         |
|                                              |                             |
|                                              v                             |
|   +----------+  +----------+  +----------+  +----------+                  |
|   | Overlord |  |  Claude  |  |  Clawd   |  |   Geek   |                  |
|   | master   |  | rndbot   |  | rndbot   |  | rndbot   |                  |
|   +----------+  +----------+  +----------+  +----------+                  |
|        ^              ^                                                    |
|        |              + dispatches via leader_admin_command →              |
|        +-- fallback master (Mcp.Leader.SystemMasterGuid)                   |
|                                                                            |
+---------------------------------------------------------------------------+
```

### Cold-boot sequence

1. `docker compose up -d ac-worldserver` → world init (~15-20s).
2. `OllamaChatConfigWorldScript::OnStartup` fires.
3. `GatewayLeaderTick` starts (no-op unless `Mcp.Leader.AutoTickEnable=1`).
4. Overlord scheduler spawns a background thread, sleeps `StartupDelaySeconds` (default 30).
5. Thread queues a DB query holder via `CharacterDatabase.DelayQueryHolder`.
6. On callback, the worldserver thread builds a null-socket `WorldSession` and calls `HandlePlayerLoginFromDB` — Overlord (e.g. Botmaster, guid 20010) appears in-world.
7. Overlord is registered as a `PlayerbotMgr` (master), NOT a `PlayerbotAI` (bot). `GetPlayerbotMgr(overlord)` now returns a real manager.
8. For each guid in `Overlord.AutoSpawnBots`, `sRandomPlayerbotMgr.AddPlayerBot(guid, 0)` runs the rndbot spawn path (skips account / guild / linked permission checks). Claude / Clawd / Geek come online.
9. Fleet is ready. Agent can call MCP tools immediately.

No human character ever logs in. On a healthy boot, 4 characters are online
~45 seconds after `docker compose up`.

### Who's a master vs. a bot?

| Entity  | Session kind             | PlayerbotsMgr map           | Can run `.playerbots bot`? |
|---------|--------------------------|-----------------------------|----------------------------|
| Raz (human) | Real socket, SEC_PLAYER | `PlayerbotMgr`              | Yes                        |
| Overlord    | Null socket, IsBot()=true | `PlayerbotMgr` (manual)     | Yes (the whole point)      |
| Claude      | Null socket, IsBot()=true | `PlayerbotAI`               | No — bots refuse self-calls |
| Random bots | Null socket, IsBot()=true | `PlayerbotAI`               | No                         |

`leader_admin_command` handles the "Claude has no human master" case by
falling back to `Mcp.Leader.SystemMasterGuid` (= Overlord's guid), so admin
commands dispatch through Overlord's session even when nobody spawned the
leader manually.

---

## One-time setup

### 1. Create the Overlord account

Account first. The character can't exist until the account does.

```sql
-- Precompute SRP6 salt + verifier with the helper in scripts/srp6.py
-- (or use the `.account create` worldserver console command if you have it)
INSERT INTO acore_auth.account (username, salt, verifier, email, reg_mail, expansion, joindate)
VALUES ('BOTMASTER',
        UNHEX('<salt hex>'),
        UNHEX('<verifier hex>'),
        '', '', 2, NOW());
```

No GM access row — Overlord should run as `SEC_PLAYER`. That's the principle
of least privilege: the allowlist in `Mcp.Leader.AllowedAdminCommands`
guards the verbs, and SEC_PLAYER makes any `initself`/`tweak`/`reload`
escape rejected by playerbots' own security gate.

### 2. Create Overlord's character

SQL-inserting a character directly touches 20+ tables and is fragile. Use
the real WoW client instead — takes ~60 seconds:

1. Log out to character select.
2. "Add account" → username `BOTMASTER`, a password of your choice.
3. Create a character (any race/class/name — class + level don't matter;
   this character will never fight).
4. Log in briefly so AC finalizes the character's row set, then log out.
5. Grab the guid:
   ```sql
   SELECT guid, name FROM acore_characters.characters
   WHERE account = (SELECT id FROM acore_auth.account WHERE username = 'BOTMASTER');
   ```

### 3. Wire up module config

Append to `~/docker/wow/azerothcore-wotlk/env/dist/etc/modules/mod_ollama_chat.conf`:

```ini
# ----- Phase 1: Overlord headless master auto-login -----
OllamaChat.Overlord.Enable                   = 1
OllamaChat.Overlord.CharacterGuid            = <overlord guid>
OllamaChat.Overlord.StartupDelaySeconds      = 30
OllamaChat.Overlord.AutoSpawnBots            = "20007,20008,20009"  # gateway bots

# ----- Phase 1.2: Admin-command fallback master -----
OllamaChat.Mcp.Leader.AllowAdminCommands     = 1
OllamaChat.Mcp.Leader.SystemMasterGuid       = <overlord guid>

# ----- Fleet bootstrap: party/master reconciliation (robot-first) -----
OllamaChat.Fleet.EnsureParty                 = 1
OllamaChat.Fleet.LeaderGuid                  = 20007          # Claude — self-mastered party leader
OllamaChat.Fleet.MemberGuids                 = "20008,20009"  # Clawd, Geek
OllamaChat.Fleet.EnsureIntervalSec           = 20
```

Restart:

```bash
cd ~/docker/wow/azerothcore-wotlk
docker compose restart ac-worldserver
```

Confirm in logs:

```
[Ollama Chat Overlord] auto-login complete: Botmaster (account=..., guid=...) registered as master session
[Ollama Chat Overlord] auto-spawning rndbot guid=20007
[Ollama Chat Overlord] auto-spawning rndbot guid=20008
[Ollama Chat Overlord] auto-spawning rndbot guid=20009
```

Confirm in DB:

```sql
SELECT guid, name, online FROM characters
WHERE guid IN (<overlord guid>, 20007, 20008, 20009);
-- all should show online = 1
```

---

## Fleet bootstrap — party/master reconciliation (robot-first mode)

Overlord spawns the fleet but never groups it, and mod-playerbots'
`UpdateAIGroupMaster()` clears `PlayerbotAI::master` on every AI tick for an
**ungrouped random bot**. Result: the fleet is online but ownerless —
`bot_summon` and bare `bot_follow` resolve their destination from the
registered master, find null, and silently no-op (historically while still
returning `ok:true`).

With `OllamaChat.Fleet.EnsureParty = 1`, a periodic pass on the world thread
(`src/mod-ollama-chat_fleet.cpp`, driven from
`OllamaChatConfigWorldScript::OnUpdate`, log prefix `[Ollama Chat Fleet]`):

1. **Self-masters the leader bot** (`Fleet.LeaderGuid`, typically Claude
   20007). A self-mastered AI passes playerbots' master-validity test
   (`IsRealPlayer()` = `master == bot`), so it can own the member bots, and
   the random-bot manager's sweeps treat it as a real player.
2. **Forms a party** under the leader if none exists (same world-thread
   `Group::Create` + `GroupMgr::AddGroup` path playerbots itself uses).
3. **Pulls each `Fleet.MemberGuids` bot in** and sets the leader as its
   playerbot master.

Design decisions worth knowing:

- **Re-entrant by design.** If the group breaks, playerbots nulls the masters
  again — the next pass (every `Fleet.EnsureIntervalSec`, default 20 s)
  re-forms everything. All steps are idempotent.
- **Botmaster/Overlord stays OUT of the party.** It remains only the
  `.playerbots bot` admin session (`Mcp.Leader.SystemMasterGuid`). This keeps
  the 5th party slot free for the human operator and means `bot_summon`
  gathers the fleet **on the leader**, which is the semantics an external
  agent wants.
- **Foreign groups are never touched.** A member bot found in a different
  group (the operator's own party, a battleground raid) is skipped with a
  warning, not yanked out.
- Once the master link exists, `Mcp.Leader.Scope = "group"` resolves the full
  party for `leader_command_all`, and per-bot `bot_summon`/`bot_follow`
  finally have a destination.

## Robot-first operating model

The primary interface to the fleet is an **external MCP agent** (a desktop
robot with MCP clients attached to both the gameplay and admin MCPs):

- **In-game chat is retired as an interface** —
  `OllamaChat.Gateway.AllowedChannels = "none"` disables all chat-triggered
  gateway/classifier traffic. Bots no longer answer whispers/party chat.
- **The robot is the strategic layer**: it observes via read tools, commands
  via action/leader tools, and sets per-bot goals via `tactical_set_directive`
  (source label `mcp`).
- **Voice fallback stays**: `talk_to_leader` + the push-to-talk app
  (`tools/voice-command/`) inject into the leader's gateway brain via
  `Mcp.AllowGatewayInjection` — independent of the retired chat path. Its
  directive writes carry `player:<guid>`.
- **Local Ollama remains the executor clock** (tactical loop). With
  (note: with `=1` the gate wants a *real* whitelisted client — bots, Botmaster and fleet guids
  never count, even on the whitelisted account; see `IsWhitelistedHumanPlayer` in tactical.h)
  `Tactical.HumanPresenceRequired = 0` the fleet plays autonomously whenever
  the tactical Ollama host is up; when it's off, the circuit breaker idles the
  loop and the bots stay commandable via MCP — that's the intended "off"
  state, not a fault.
- **Escalation is the fallback strategist**: the local model can still buy one
  paid gateway call when stuck, but an escalation-written directive never
  overrides an active operator directive (see `docs/tactical.md`
  §Directive arbitration).

---

## MCP endpoint

```
URL:    https://mcp.wow.example.com/mcp
Method: POST (JSON-RPC 2.0)
Auth:   Authorization: Bearer <OllamaChat.Mcp.BearerToken>
```

**IP allowlist** (Traefik middleware `wow-mcp-allowlist`): Synthiq cloud, Tailscale
subnet, LAN. Add your Claude Desktop's public IP if connecting from outside
those ranges.

Smoke test from anywhere that passes the IP check:

```bash
curl -s -X POST https://mcp.wow.example.com/mcp \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/list"}' | jq '.result.tools | length'
# -> 31
```

---

## Claude Desktop / Cowork config

Claude Desktop MCP config (`~/Library/Application Support/Claude/claude_desktop_config.json`):

```json
{
  "mcpServers": {
    "wow-worldserver": {
      "transport": {
        "type": "http",
        "url": "https://mcp.wow.example.com/mcp",
        "headers": {
          "Authorization": "Bearer REPLACE_WITH_TOKEN"
        }
      }
    }
  }
}
```

Restart Claude Desktop. In a new chat you should see `wow-worldserver`
connected with 31 tools available.

---

## The Overlord system prompt

Paste this into a new Claude Desktop chat (or as a Cowork system prompt).
It tells the model what the fleet looks like and which tools to reach for
first.

```
You are the Overlord of a World of Warcraft playerbot fleet running on an
AzerothCore WoTLK private server. You control bots remotely via the
mcp__wow-worldserver__* tools. No human is present at the keyboard.

Fleet topology:
- Overlord (Botmaster, guid 20010) is a headless master session. You do NOT
  control Overlord directly — Overlord is the session that leader_admin_command
  dispatches through. Think of Overlord as your "hand" for .playerbots bot X
  commands.
- Gateway bots (your squad): Claude (paladin, guid 20007), Clawd (shaman, guid
  20008), Geek (warlock, guid 20009). These are the ones you actually drive.
  All three are rndbots spawned by Overlord at boot.

Tool reach-for order:
1. get_fleet_status — first call of every turn. Confirms which bots are online,
   where, at what level.
2. leader_list_targets(botGuid=<one of 20007/20008/20009>) — see the squad the
   chosen leader can command (group/nearby/owner/all based on Mcp.Leader.Scope).
3. leader_command_all(botGuid=<leader>, command="<playerbot verb>") — broadcast
   an order. Valid verbs: follow, stay, attack, cast <spell_id>, flee, @tank,
   @heal, @dps, nc +/- <strategy>, co +/- <strategy>, e <item>, u <item>, s *.
4. leader_command(botGuid=<leader>, targetBotName=<name>, command=...) — order
   a single squadmate.
5. leader_admin_command(botGuid=<leader>, command="list") — list every
   character on the leader's master's account (currently = Overlord's account).
   Also supports add/addclass/addaccount/remove. Dispatches through
   Overlord's session via the SystemMasterGuid fallback.
6. emergency_stop(botGuid=<leader>) — panic button. Broadcasts "stay" to the
   whole squad. Use when something is about to go wrong.

Rules of engagement:
- Always call get_fleet_status first. If your intended leader is offline, stop
  and report — do NOT try to issue commands into an empty world.
- Never invent guids. Use only the ones in the fleet topology above.
- When leader_admin_command returns via_system_fallback: true, that's
  expected — you dispatched through Overlord, not through a human master.
- When a tool returns "error": ..., report the exact error to the user.
  Don't invent a cover story.

Common playbook:
- "Farm copper in Durotar": get_fleet_status → leader_command_all(20007,
  "nc +grind") → leader_command_all(20007, "nc +loot") → periodically
  get_bot_state to check progress.
- "Spawn Dia": leader_admin_command(20007, "add Dia").
- "What bots can I spawn from OPERATOR?": get_bot_roster_by_account(OPERATOR).
- "Bots are out of control": emergency_stop(20007).
```

---

## Common workflows

### Is my fleet up?

```json
{ "method":"tools/call",
  "params":{ "name":"get_fleet_status", "arguments":{} } }
```

Returns online/offline + position for every bot in `Gateway.BotGUIDs` plus
Overlord. First call of every agent turn.

### Show me what bots exist on account X

```json
{ "method":"tools/call",
  "params":{ "name":"get_bot_roster_by_account",
             "arguments":{ "accountName":"BOTMASTER" } } }
```

No in-world dependency — works even on a just-booted server with nothing
online yet.

### Spawn or despawn a specific bot

```json
{ "method":"tools/call",
  "params":{ "name":"leader_admin_command",
             "arguments":{ "botGuid":20007,
                           "command":"add CharName" } } }
```

`via_system_fallback: true` is the expected response shape when the leader
has no human master (the normal case for rndbot fleet).

### Broadcast a squad order

```json
{ "method":"tools/call",
  "params":{ "name":"leader_command_all",
             "arguments":{ "botGuid":20007, "command":"nc +grind" } } }
```

Playerbot grammar reference is available via `leader_get_command_help`.

### Emergency stop

```json
{ "method":"tools/call",
  "params":{ "name":"emergency_stop",
             "arguments":{ "botGuid":20007 } } }
```

Halts the squad immediately (broadcasts `stay`).

### Delete a character permanently

```json
{ "method":"tools/call",
  "params":{ "name":"leader_admin_delete_character",
             "arguments":{ "botGuid":20007, "name":"Geek", "dry_run":true } } }
```

`dry_run` previews `{guid, name, level, account, online}`. Re-send with
`"confirm":true` to delete. This goes through **`Player::DeleteFromDB`** — the
same call `.character erase` makes — so every table is cleaned and the
**character-name cache is cleared immediately**, which is the whole point: the
name can be re-created in the client right away. A raw SQL delete leaves the
cache stale and the client refuses the name as "taken" until a worldserver
restart. Guards: refuses an **online** character (despawn with
`leader_admin_command "remove <name>"` first), refuses the
`SystemMasterGuid` / `Overlord.CharacterGuid` (admin infrastructure), and
bypasses `CharDelete.Method` (no soft-delete to restore from — **back up first**).

Its non-destructive sibling is ops-api's `wow_reset_character`, which wipes a
character back to level 1 **keeping the guid** so a wired bot stays wired. Use
that for "fresh start, same bots"; use delete for "new bots".

### Reroll the whole party from scratch (new characters, new guids)

Done 2026-09-20 (VLAT), fully MCP-driven — three `leader_admin_*` tools cover the
lifecycle, so nothing needs the WoW client or a hand edit of the conf:

| Step | Tool | Notes |
|---|---|---|
| delete | `leader_admin_delete_character` | engine path (`Player::DeleteFromDB`), name reusable immediately; refuses online chars and the Overlord |
| create | `leader_admin_create_character` | engine path (`Player::Create` + `SaveToDB` + cache insert), level 1, random-valid looks |
| rewire | `leader_admin_set_fleet` | rewrites the six guid keys + `gateway_overrides.json`, reloads, spawns the new roster |
| despawn / spawn | `leader_admin_despawn` / `leader_admin_spawn` | log fleet bots out / in through their real owner (the rndbot holder) — `.playerbots bot remove` cannot |
| move | `leader_admin_teleport` | `TeleportTo` online, `SavePositionInDB` offline; `to_race_start` / `to_character` / explicit coords |

All three take `botGuid` = the **current** `Mcp.Leader.BotGUIDs` leader, need
`Mcp.Leader.AllowAdminCommands = 1`, and are `dry_run` / `confirm:true` gated
(`set_fleet` additionally needs `Mcp.ConfigEdit.Enable = 1` with
`mod_ollama_chat.conf` in `ConfigEdit.AllowedFiles`).

1. **Backup:** dump `acore_characters` (`wow-db-backup.sh` or the ops-api backup).
   Deletion is permanent — `CharDelete.Method` is bypassed.
2. **Despawn:** `leader_admin_despawn {"all":true}` (dry-run first — the
   report names each bot's `owner`). Do NOT use `leader_admin_command "remove *"`
   for this, with or without `via_system_master`: `Overlord.AutoSpawnBots` spawns
   through the rndbot path, so the bots are owned by the `RandomPlayerbotMgr`
   and a `.playerbots bot remove` from the leader *or* the Overlord session
   returns `ok:true` while removing nothing (measured 2026-09-20, both ways).
   The bots stay down until `leader_admin_spawn` / `set_fleet` / the Overlord's
   next login, so no `AutoSpawnBots` edit is needed.
3. **Delete** each old character: `leader_admin_delete_character
   {"name":"Raz","dry_run":true}` for every target first (all must come back
   `would_delete:true, online:false`), then `confirm:true`. Keep the
   Overlord/Botmaster character. Note the `account` in the dry-run output —
   you will want the human's new character on the same one.
4. **Create** the new party — same faction, level 1:
   ```json
   {"botGuid":20007,"name":"Slayo","race":"undead","class":"rogue","first_login":true,"confirm":true}
   {"botGuid":20007,"name":"Dia","race":"tauren","class":"warrior","gender":"male","first_login":false,"confirm":true}
   {"botGuid":20007,"name":"Raz","race":"undead","class":"priest","first_login":false,"confirm":true}
   ```
   `account_id` defaults to the account owning `Mcp.Leader.SystemMasterGuid`
   (the operator's own account here). `first_login:true` keeps the intro
   cinematic + `AT_LOGIN_FIRST` for the human; bots get `false`. Death Knights
   are refused (hero-class start path). Each call returns the new `guid`.
5. **Rewire** in one call — the leader is the character `talk_to_leader` and the
   gateway chat lane address, members are the rest:
   ```json
   {"botGuid":20007,"leader_guid":20011,"member_guids":[20012],"dry_run":true}
   {"botGuid":20007,"leader_guid":20011,"member_guids":[20012],"confirm":true}
   ```
   The dry run prints the resolved names, every key value and the gateway
   template (`url`/`model`/`gatewayType`/token length, copied from the
   current `Fleet.LeaderGuid` entry — pass `gateway:{...}` to override). The
   write backs up both files, rewrites `Gateway.BotGUIDs`,
   `Overlord.AutoSpawnBots`, `Tactical.BotGUIDs`, `Mcp.Leader.BotGUIDs`,
   `Fleet.LeaderGuid`, `Fleet.MemberGuids`, blanks the retired pipe-format
   `Gateway.BotOverrides`, regenerates `gateway_overrides.json`, reloads and
   spawns the roster through the Overlord's own `AutoSpawnBots` path
   (`spawn:false` to skip). **From here on `botGuid` is the NEW leader.**
   If the party is mixed-race, gather it at one start zone before the human
   logs in: `leader_admin_teleport {"name":"Claude","to_race_start":"undead"}`
   (online → `TeleportTo`; offline → the saved row is rewritten).
6. **Verify:** `bots_fleet_status` / `get_group` shows the party under the
   new leader; `talk_to_leader` (new `botGuid`) answers in character; the
   tactical audit table shows `ok` rows; a whisper/party line naming a bot
   gets a reply. Update the guids in `CLAUDE.md` / `docs/` that name them.

---

## Troubleshooting

### `leader_admin_command` returns `ok: false`

Before chasing code, confirm the dispatched command actually made it into
`playerbots`:

```bash
docker logs --since 2m ac-worldserver | grep "Leader Admin"
```

Look for `dispatched: leader=... master=... command='<cmd>' ok=<true|false>`.
- `ok=false` with a printed dispatch line → playerbots ran the handler and
  rejected it (bad verb, permission, bot already in-world). Check the
  `.playerbots bot` messages in-game for Overlord — they're routed to
  Overlord's session.
- No dispatch line at all → `ChatHandler::ParseCommands` rejected the string
  early. Most common cause: missing `.` prefix (fixed April 2026, make
  sure you're past commit `f4e21da`).

### Overlord didn't come online

In this order:

1. `docker logs --since 5m ac-worldserver | grep Overlord` — is the
   scheduler running?
2. Confirm `Overlord.Enable = 1` and `Overlord.CharacterGuid` are set
   in the live config (NOT just the `.dist`).
3. Confirm the guid resolves to an account:
   ```sql
   SELECT c.guid, a.username FROM acore_characters.characters c
   JOIN acore_auth.account a ON c.account = a.id WHERE c.guid = <guid>;
   ```
4. Check for `ERROR: [Ollama Chat Overlord]` lines — typical failures:
   - `cannot resolve account for guid=X` → wrong guid, or the character
     was deleted.
   - `LoginQueryHolder::Initialize failed` → corrupted character row;
     try loading the char manually through a client to validate.

### Gateway bots don't auto-spawn

Check the log for:

```
[Ollama Chat Overlord] auto-spawning rndbot guid=20007
```

If the line is missing, either `AutoSpawnBots` is empty / malformed, or
the guids are already in-world. If the line is present but the bot doesn't
come online, the rndbot factory rejected them — usually because
`playerbots.RandomBotLoginCharacters` or a guild/realm filter excluded
them. Test with a known-good rndbot guid first.

### MCP call returns 401 / 403

- 401: bearer token mismatch. Check `OllamaChat.Mcp.BearerToken` vs what
  Claude Desktop is sending.
- 403: IP allowlist. Traefik middleware `wow-mcp-allowlist` is
  restrictive. Add your source IP or connect through Tailscale / the
  Synthiq cloud.

### Rate limiting

Defaults (from `mod_ollama_chat.conf.dist`):

- `Mcp.Leader.RateLimitPerMinute = 30` — per-leader-guid, includes all
  `leader_*` tools.
- `Mcp.ActionRateLimitPerBotPerMinute = 6` — for `bot_*` self-action tools.

For agentic 24/7 play, 30/min is generally enough — tune up if you see
`"rate-limited"` returns. Don't disable them; they're the only backstop
against a runaway agent loop.

---

## Self-reconfiguration (config_* tools)

Once the fleet is driveable via MCP, the next step for real autonomy is
**letting the agent change its own configuration**. Five tools expose the
server's `.conf` files — tight allowlist, atomic edits, timestamped backups,
fully audited:

| Tool | What it does |
|---|---|
| `config_list_files` | Enumerates allowlisted files + reload target |
| `config_get_file` | Returns full file content (bounded by `MaxFileSizeBytes`) |
| `config_get_value` | Reads one key; returns value, line number, commented/live flag |
| `config_set_value` | Atomic in-place edit; backup-first; preserves whitespace + inline comments |
| `config_reload` | Reloads one or more subsystems: `mod_ollama_chat`, `playerbots`, `worldserver`, or `all` |

### Safety gates (all three must pass)

1. `OllamaChat.Mcp.ConfigEdit.Enable = 1`
2. For writes: `OllamaChat.Mcp.AllowActionTools = 1`
3. For writes: `(basename, key)` must glob-match an entry in `Mcp.ConfigEdit.AllowedKeys`

Plus a deny-pattern list (`DeniedKeys`, default `*Token*,*Password*,*Secret*,*BearerToken*,*ApiKey*,*DatabaseInfo*`) that rejects both reads AND writes — so credentials can't leak even if the allowlist is misconfigured.

### Recommended opt-in config (on game-host)

```
OllamaChat.Mcp.ConfigEdit.Enable = 1
OllamaChat.Mcp.ConfigEdit.ConfigRoot = "/azerothcore/env/dist/etc"
OllamaChat.Mcp.ConfigEdit.AllowedFiles = "mod_ollama_chat.conf"
OllamaChat.Mcp.ConfigEdit.AllowedKeys = "mod_ollama_chat.conf:OllamaChat.Gateway.AutoClaimOnLogin,mod_ollama_chat.conf:OllamaChat.Gateway.MentionExempt*,mod_ollama_chat.conf:OllamaChat.Mcp.Leader.AutoTick*"
OllamaChat.Mcp.ConfigEdit.BackupDir = "/azerothcore/env/dist/etc/backups"
OllamaChat.Mcp.ConfigEdit.ReloadAdminSessionGuid = 20010   # Overlord
```

Start with 1 file + a handful of keys. Expand to `playerbots.conf` and
`worldserver.conf` only after the audit table shows a clean run — those two
files have much broader blast radius, and `worldserver.conf` has DB
credentials that the default deny list protects but which one typo in your
allowlist could accidentally expose.

### Example agent flow (pause auto-claim)

1. `config_list_files` → agent sees `mod_ollama_chat.conf` with reload target
   `mod_ollama_chat`.
2. `config_get_value { file: "mod_ollama_chat.conf", key: "OllamaChat.Gateway.AutoClaimOnLogin" }` → `{ value: "1", line: 2103, found: true }`.
3. `config_set_value { file: ..., key: ..., value: "0" }` → `{ ok: true, mode: "replaced", old_value: "1", new_value: "0", backup_path: "/azerothcore/env/dist/etc/backups/mod_ollama_chat.conf.1777000000.bak" }`.
4. `config_reload { targets: ["mod_ollama_chat"] }` → `{ targets: { mod_ollama_chat: { ok: true, ... } }, any_error: false }`.

Next real-player login: bots do NOT auto-claim.

### Audit

Every `config_set_value` and `config_reload` writes a row to
`mod_ollama_chat_gateway_audit` with `source_channel = 'mcp:config_edit'` or
`'mcp:config_reload'`. Prune periodically via `.ollama gateway prune` (same
pruner used for other audit rows).

### Verify ownership and restart safety

- **Ownership:** the server process usually runs as root inside the container,
  so new files are root-owned. `chown` is attempted to preserve the original
  owner but is best-effort; if your host maps a non-root UID, double-check
  after the first write.
- **Container restart:** `config_reload` re-reads on the live server only. If
  you change `Mcp.*` settings themselves (port, bearer token, enable flags),
  the MCP server is restarted as part of the reload — expect a 1-2s gap
  before `tools/list` works again.

---

## What this does NOT give you

- **Movement planning** — bots move themselves based on playerbot strategies
  (`nc +grind`, quest AI, combat AI). There is no `travel_to(x,y,z)` tool.
  If you need specific positioning, use the leader's `stay` / `follow` and
  manually move the leader (the leader bot IS autonomous; just set a travel
  target via strategy).
- **Client-side visuals** — you're driving the server. No screenshots, no
  vision model. State comes from MCP tool returns.
- **Auto-login a human character** — Overlord is explicitly NOT human-flavored.
  Don't use your main character's guid; dedicated Overlord character is
  important for session-separation and security.

---

## Next steps

- Cowork / Claude Desktop Remote Control (Pro/Max) — drive this same MCP
  from your phone. See `docs/gateway.md` for the broader gateway feature
  set.
- Autonomous strategies — once the fleet runs itself, consider enabling
  `Mcp.Leader.AutoTickEnable=1` so the leader re-assesses the situation
  on a cadence and fires gateway requests without you prompting.
