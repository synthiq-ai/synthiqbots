# Gateway Agent Bots

The gateway feature routes designated bot whispers to an external OpenAI-compatible API
(typically a Synthiq- or OpenClaw-style agent gateway built on the Claude Agent SDK)
instead of the local Ollama LLM. Use it to give a small subset of player bots access to a
much more capable agent (with tools, memory, larger context) while keeping the rest of the
bot population on cheap local inference.

> **See also: `docs/bot-promotion.md`**: any *other* playerbot you whisper, name or invite
> gets this gateway at runtime (`OllamaChat.Gateway.Promote.*`), using a copy of the fleet
> leader's override.

> **See also: `docs/tactical.md`** — the companion-mode layer that sits on
> top of this gateway. Tactical uses a free local Ollama loop for fast
> decisions and escalates to *this* gateway only when the local model asks
> for strategic help. For tactical-enabled bots, the paid leader-tick
> proactive sampler is suppressed (the MCP tools `tactical_get_directive`
> and `tactical_set_directive` are the strategic→tactical bridge).

## How it works

```
Player whispers a bot
    │
    ▼
PlayerBotChatHandler::OnPlayerCanUseChat
    │
    ▼
Is bot in g_GatewayBotGUIDSet?  ──no──▶ silent for this path
    │ yes
    ▼
Sender account in whitelist?    ──no──▶ silent for this path
    │ yes
    ▼
Whisper contains trigger keyword? ──no──▶ silent
    │ yes
    ▼
Per-(bot, player) cooldown clear? ──no──▶ drop (rate-limited stat++)
    │ yes
    ▼
Concurrency cap available?       ──no──▶ drop (rate-limited stat++)
    │ yes
    ▼
POST /v1/chat/completions  (with optional x-synthiq-session-key)
    │
    ├─ on success ──▶ split response into whisper-sized chunks ──▶ Whisper
    └─ on error ────▶ audit/log error ──▶ silent
```

## Configuration

All settings live under `[worldserver]` in `mod_ollama_chat.conf`. Defaults below.

### Core

| Key | Default | Purpose |
|---|---|---|
| `OllamaChat.Gateway.Enable` | `0` | Master switch |
| `OllamaChat.Gateway.Url` | `""` | Default chat-completions endpoint |
| `OllamaChat.Gateway.BearerToken` | `""` | Default Authorization bearer |
| `OllamaChat.Gateway.Model` | `openclaw` | Default model name |
| `OllamaChat.Gateway.Scopes` | `operator.admin,operator.read,operator.write` | `x-openclaw-scopes` header |
| `OllamaChat.Gateway.SystemPrompt` | `""` | Optional system prompt for every request |
| `OllamaChat.Gateway.TriggerKeyword` | `claude` | Case-insensitive keyword (stripped from message) |
| `OllamaChat.Gateway.BotGUIDs` | `""` | Comma-separated GUIDs that should act as gateway bots |
| `OllamaChat.Gateway.BotOverrides` | `""` | `GUID:URL:TOKEN:MODEL\|...` per-bot overrides |
| `OllamaChat.Gateway.WhitelistAccountIds` | `""` | Comma-separated account IDs allowed to trigger gateway. Empty = all. |
| `OllamaChat.Gateway.FallbackToOllama` | `0` | Deprecated no-op; missing keyword stays silent |
| `OllamaChat.Gateway.MaxHistory` | `10` | Number of (user, assistant) pairs sent in each request |

### Hardening (PR1)

| Key | Default | Purpose |
|---|---|---|
| `OllamaChat.Gateway.TimeoutSeconds` | `300` | HTTP timeout |
| `OllamaChat.Gateway.FallbackOnError` | `0` | Deprecated no-op; API failure is audited/logged and stays silent |
| `OllamaChat.Gateway.MaxConcurrentRequests` | `4` | Cap on in-flight gateway calls. `0` = unlimited |
| `OllamaChat.Gateway.MinSecondsBetweenRequests` | `5` | Per-(bot, player) cooldown |
| `OllamaChat.Gateway.UserPrefix` | `wow-bot-` | Prefix on the OpenAI `user` field |
| `OllamaChat.Gateway.MaxWhisperChunkChars` | `250` | Whisper chunk size for long answers. `0` disables chunking |

### Audit + admin commands (PR7)

| Key | Default | Purpose |
|---|---|---|
| `OllamaChat.Gateway.EnableAudit` | `0` | Persist every gateway call into `mod_ollama_chat_gateway_audit` |
| `OllamaChat.Gateway.AuditRetentionDays` | `30` | Auto-prune older rows on startup and `.ollama gateway prune` |
| `OllamaChat.Gateway.EnableActionMarkers` | `0` | Parse `[emote:bow]` etc. markers (currently a logging stub) |

**Schema**: source `data/sql/characters/base/2026_04_18_gateway_audit.sql` before enabling.

**GM commands** (in addition to PR1's `status`):

| Command | What it does |
|---|---|
| `.ollama gateway test <BotName> <prompt>` | Async-fire a test gateway request as the GM. Response is logged to worldserver log (not whispered) |
| `.ollama gateway costs [hours=24]` | Aggregate audit table for the last N hours: requests, errors, avg latency, char totals, top-10 bots |
| `.ollama gateway prune` | Force-prune audit rows older than `AuditRetentionDays` |

### Embedded MCP server (PR8a) — the working tool-use transport

Both upstream gateways (OpenClaw and Synthiq) drop request-body `tools[]` on the
floor — they have their own baked-in tool handling and never surface OpenAI-style
`tool_calls` back through `/v1/chat/completions`. So the PR6 path is dead in
production.

PR8a flips the polarity: instead of pushing tools at the gateway, the worldserver
**runs an MCP HTTP server** that the upstream gateway loads via its `.mcp.json`
config. The agent sees our tools alongside its built-ins (`mcp__wow-worldserver__get_bot_state`).

| Key | Default | Purpose |
|---|---|---|
| `OllamaChat.Mcp.Enable` | `0` | Master switch |
| `OllamaChat.Mcp.BindAddress` | `0.0.0.0` | HTTP bind address |
| `OllamaChat.Mcp.Port` | `18790` | HTTP listen port |
| `OllamaChat.Mcp.BearerToken` | `""` | Required when `Enable=1`. `openssl rand -hex 32` |
| `OllamaChat.Mcp.InjectContextHint` | `1` | Prepend MCP-context hint (botGuid/playerGuid) to gateway system prompt |
| `OllamaChat.Mcp.AllowActionTools` | `0` | Reserved for PR8c |
| `OllamaChat.Mcp.ActionRateLimitPerBotPerMinute` | `6` | Reserved for PR8c |
| `OllamaChat.Mcp.AllowedToolsExtra` | `""` | Reserved for PR8b/PR8c |

**Smoke test** (run on the worldserver host, replace `$TOKEN` with `Mcp.BearerToken`):

```bash
# Health (no auth)
curl -sS http://localhost:18790/mcp/health

# tools/list
curl -sS -X POST http://localhost:18790/mcp \
  -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/list"}'

# tools/call (botGuid is required by the per-tool argument schema)
curl -sS -X POST http://localhost:18790/mcp \
  -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -d '{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"get_bot_state","arguments":{"botGuid":20007}}}'

# Auth check (no header → 401)
curl -sS -i -X POST http://localhost:18790/mcp -d '{}'
```

**Wire it into Synthiq** by adding to `~/.synthiq/.mcp.json` on the host
running the upstream gateway:

```json
{
  "mcpServers": {
    "wow-worldserver": {
      "type": "http",
      "url": "http://<worldserver-host>:18790/mcp",
      "headers": { "Authorization": "Bearer <Mcp.BearerToken value>" }
    }
  }
}
```

Restart the Synthiq gateway. Tools surface as `mcp__wow-worldserver__<name>` in
the agent's prompt. `.ollama gateway status` includes a new `mcp:` line with the
listener state and request counters.

#### Read-only tools (PR8b)

The MCP server exposes the following read-only tools (all reachable from the
agent as `mcp__wow-worldserver__<name>`):

| Tool | Args | Returns |
|---|---|---|
| `get_bot_state` | `botGuid` | name, level, class, race, zone, area, hp_pct |
| `get_player_state` | `botGuid`, `playerGuid?` or `name?` | same shape, for any player |
| `get_zone_info` | `botGuid` | zone/area names, map id, x/y/z |
| `get_player_gear` | `playerGuid?` or `name?` | per-slot item id/name/iLvl/quality + avg item level |
| `get_player_quests` | `playerGuid?` or `name?` | active quest log entries with title/level/zone |
| `get_nearby_players` | `botGuid`, `radius?` (yards, default 60) | list of nearby players w/ class/level/distance/is_bot |
| `get_group` | `botGuid` | bot's party/raid composition |
| `get_target` | `botGuid` | bot's current target name/level/hp%/distance |
| `get_inventory` | `botGuid` | bag contents aggregated by item_id |
| `query_world_db_lookup` | `table` (`item_template` \| `quest_template`), `id` | row from a whitelisted world-db table |
| `whisper_player` | `botGuid`, `name`, `message` (≤200 chars) | sends a whisper from the bot |

The agent does NOT have to call `get_player_state` first to know the whisperer —
when `Mcp.InjectContextHint=1`, the active `botGuid` and `playerGuid` are placed
in the system prompt so the agent passes them as arguments directly.

`whisper_player` is always available as a write tool. The richer action tools
listed below ship in PR8c but require `OllamaChat.Mcp.AllowActionTools=1`.

#### Action tools (PR8c — opt-in via `Mcp.AllowActionTools=1`)

These dispatch a textual command through `PlayerbotAI::HandleCommand` (the same
entry point the chat parser uses for `.playerbot OPERATOR follow`).

| Tool | Args | Effect |
|---|---|---|
| `bot_follow` | `botGuid`, `name?` | Bot follows the named player (or the whisperer if omitted) |
| `bot_stay` | `botGuid` | Bot stops moving |
| `bot_emote` | `botGuid`, `emote` (alphanumeric, e.g. `bow`, `wave`, `cheer`, `dance`) | Bot performs the emote |
| `bot_invite_to_group` | `botGuid`, `name?` | Bot invites the named player (or the whisperer) to its group |
| `bot_use_item` | `botGuid`, `item_id` (int) | Bot uses the named item from inventory |
| `bot_cast` | `botGuid`, `spell_id` (int) | Bot casts the named spell on its current target |
| `bot_attack_target` | `botGuid`, `target_name?` | Bot attacks the named target (or its current selection if omitted) — engage |
| `bot_stop_combat` | `botGuid` | Bot clears current AI actions incl. combat engagement (`reset`) — disengage |
| `bot_revive` | `botGuid` | If bot is dead, release spirit and revive at nearest graveyard — recover |
| `bot_maintenance` | `botGuid` | One-shot prep: learn spells/skills from nearest trainer, buy consumables, repair durability — needs trainer/vendor in range |
| `bot_train_spells` | `botGuid` | Learn every available spell from the nearest class trainer (no vendor/repair side-effects) |

**Guardrails:**
- `OllamaChat.Mcp.AllowActionTools=0` (default) → every action tool returns
  `{"error":"action tools disabled ..."}` immediately. Keeps a malicious / hallucinating
  agent from flailing the bot.
- `OllamaChat.Mcp.ActionRateLimitPerBotPerMinute=6` → per-(bot, tool) rate limit
  in a trailing 60s window. Independent of the chat-message cooldown.
- Every action call is logged to `mod_ollama_chat_gateway_audit` with
  `source_channel='mcp:action'` when `EnableAudit=1`. `.ollama gateway costs`
  picks them up automatically.
- Emote names are sanitised to `[a-zA-Z0-9_]+` to block command-injection back
  through the playerbots chat parser.

**What an "action" looks like end-to-end** (when fully wired):
```
Player: /w Slayobot claude follow me, then bow
  Synthiq agent calls mcp__wow-worldserver__bot_follow(botGuid=20007)
    → PlayerbotAI::HandleCommand("follow Slayo")
    → bot starts following, audit row logged
  Synthiq agent calls mcp__wow-worldserver__bot_emote(botGuid=20007, emote="bow")
    → bot bows
  Synthiq agent whispers back: "On my way."
```

#### Config-edit tools (opt-in via `Mcp.ConfigEdit.Enable=1`)

| Tool | Effect |
|---|---|
| `config_list_files` | Enumerate allowlisted `.conf` files + reload targets (read) |
| `config_get_file` | Read full file content (read) |
| `config_get_value` | Read one key (read) |
| `config_set_value` | Atomic in-place edit with backup (destructive) |
| `config_reload` | Reload `mod_ollama_chat` / `playerbots` / `worldserver` / `all` (destructive) |

Gated by `Mcp.ConfigEdit.Enable=1` plus a per-file key allowlist (`Mcp.ConfigEdit.AllowedKeys`) with a deny sweep (`*Token*`, `*Password*`, `*DatabaseInfo*`, …) that always wins. Full docs and the recommended game-host config live in `docs/autonomous-bots.md` → §Self-reconfiguration.

#### Voice / chat-injection tool (opt-in via `Mcp.AllowGatewayInjection=1`)

| Tool | Args | Effect |
|---|---|---|
| `talk_to_leader` | `message`, `playerName?` / `playerGuid?`, `botGuid?`, `emit_in_game?` | Deliver `message` to the leader's gateway brain as if the player whispered it; run the full gateway tool-loop; return the brain's in-character reply |

This is the **inbound** counterpart to the gateway's normal flow: instead of a human typing a whisper in the WoW client (`OnPlayerCanUseChat` → `ProcessChat`), an authenticated MCP client POSTs the text here. The handler resolves the speaking player (by `playerName` or `playerGuid` — their identity drives the system prompt, the master-session command routing via AutoClaim, and the optional reply whisper), then calls the same `QueryGatewayAPIWithTools` (Synthiq) / `QueryGatewayAPI` (OpenClaw) the chat path uses. The leader **acts** (the brain calls game tools in the loop) **and answers**; the reply text is returned in the tool result (`{reply, gateway_type, used_tools, latency_ms, …}`) for a voice client to speak via TTS. Set `emit_in_game=true` to also whisper the reply into the player's WoW chat.

It runs on the MCP server's worker thread — already off the world thread, the same off-thread context the in-game whisper path uses — so `DispatchGatewayTool`'s per-call pointer revalidation holds unchanged. `botGuid` defaults to the configured single leader.

**Guardrails:** `Mcp.AllowGatewayInjection=0` (default) → the tool returns `{"error":"gateway injection disabled ..."}`. It is **not** offered to the gateway brain (absent from every gateway tool allowlist), so the brain can't call it on itself — no recursion. This is the server side of the hands-free voice-command path; full setup in **[docs/voice-command.md](voice-command.md)**.

### Backend type: openclaw vs synthiq (PR6+)

Two upstream gateway types are supported, and they behave differently around tool use:

| Type | Tool-use semantics |
|---|---|
| `openclaw` (default) | Full-fat agent with its own baked-in tools. Request-body `tools[]` is **ignored**. `EnableToolUse` has no effect for these bots. |
| `synthiq` | Thin Claude Agent SDK wrapper. Request-body `tools[]` is honored. `EnableToolUse` drives the in-game tool-call loop. |

Select per-bot in `BotOverridesJson`:

```json
{"guid": 12345, "gatewayType": "synthiq", "url": "...", "token": "...", "model": "..."}
```

Or globally via `OllamaChat.Gateway.Type`. Per-bot wins over global.

### Tool use bridge (PR6)

| Key | Default | Purpose |
|---|---|---|
| `OllamaChat.Gateway.EnableToolUse` | `0` | Send `tools[]` in requests, dispatch returned `tool_calls` (synthiq bots only) |
| `OllamaChat.Gateway.MaxToolIterations` | `3` | Recursion guard on the tool-call loop |
| `OllamaChat.Gateway.AllowedTools` | `""` | csv allowlist; empty = all built-in tools enabled |

Built-in tools dispatched by C++ on the worldserver:

| Tool | Input | Output |
|---|---|---|
| `get_bot_state` | _none_ | bot's name, level, class, race, zone, area, hp_pct |
| `get_player_state` | `name` (optional) | same for a player; defaults to the whisperer |
| `get_zone_info` | _none_ | bot's zone/area names, map id, x/y/z |
| `whisper_player` | `name`, `message` | sends a whisper from the bot to another online player (max 200 chars) |

**Per-bot override**: list specific tool names in `BotOverridesJson`'s `tools` array to
restrict that bot to a subset.

**Mutual exclusivity**: tool use and streaming both can't be on at once — `EnableToolUse`
wins. The SSE parser doesn't accumulate `tool_calls` across chunks (a future iteration).

### JSON per-bot overrides (PR5)

| Key | Default | Purpose |
|---|---|---|
| `OllamaChat.Gateway.BotOverridesJson` | `""` | JSON array of per-bot overrides. Wins over legacy `BotOverrides` |

The legacy `BotOverrides = "GUID:URL:TOKEN:MODEL\|..."` format is fragile (URLs contain colons,
the parser had to special-case re-joining). JSON solves that and adds `headers` (custom HTTP
headers per bot) and `tools` (reserved for PR6 — tool-use bridge).

Example:

```
OllamaChat.Gateway.BotOverridesJson = "[{\"guid\":12345,\"url\":\"http://gw1:18789/v1/chat/completions\",\"token\":\"tok\",\"model\":\"agent:lore-master\",\"headers\":{\"x-synthiq-agent\":\"lore\"}}]"
```

(Yes — escape the inner double quotes. AzerothCore's config parser is single-line.)

### Channels & mention mode (PR4)

| Key | Default | Purpose |
|---|---|---|
| `OllamaChat.Gateway.Classifier.MaxWords` | `0` | Word gate for BOTH classifier tiers (jev + LLM): a chat line with more whitespace-separated words skips the interceptors and goes to the in-character gateway (tools included). Live `3` since 2026-09-21 — "attack" / "follow me" stay on the ~1 s fast path, "share your quest with the party" gets a real answer. `Gateway.OllamaClassifier.Enable` gates only the LLM tier since PR 4; `Jev.Classifier.Enable` is independent. |
| `OllamaChat.Gateway.AllowedChannels` | `whisper` | csv of channel sources: `whisper, party, raid, guild, officer, say, yell, general` — or the sentinel `none` to disable ALL chat-triggered gateway+classifier traffic (robot-first mode). An **empty** value is NOT off: it falls back to `whisper` for back-compat. `none` does not affect strategic escalations or `talk_to_leader` injection — neither consults this list |
| `OllamaChat.Gateway.PublicChannelMode` | `keyword` | Gating on say/yell/general: `keyword`, `mention`, `both` |
| `OllamaChat.Gateway.PrivateChannelMentionRequired` | `1` | On party/raid/guild/officer, require the bot's name in the message (AND with keyword). Whisper is unaffected. |
| `OllamaChat.Gateway.MentionExemptBots` | `""` | csv of bot names (case-insensitive) that skip the mention check on party/raid/guild/officer. Keyword gate still applies. |

Private channels (whisper, party, raid, guild, officer) always require the trigger
keyword. Public channels follow `PublicChannelMode`. Non-whisper private channels also
require a mention of the bot's name unless the bot is listed in `MentionExemptBots` —
which is the knob to designate an "always-listening" agent (e.g. Claude) that responds
to any party-chat line. The gateway thread routes the response to the same channel the
player wrote on (whisper → Whisper, party → SayToParty, guild → SayToGuild, etc.).

### Player-facing controls (PR4)

Players manage their own preferences via in-game commands (no GM access required):

| Command | What it does |
|---|---|
| `.ollama optout` | Suppress all bot responses (gateway and Ollama) for this account |
| `.ollama optin` | Undo the opt-out |
| `.ollama mute <BotName>` | Suppress responses from one specific bot |
| `.ollama unmute <BotName>` | Undo the mute |

Stored in `mod_ollama_chat_optouts` and `mod_ollama_chat_bot_mutes` (account-keyed,
so the preference travels across alts). Loaded on startup and on `.ollama reload`.

### Per-agent presets / personality / language (PR3)

| Key | Default | Purpose |
|---|---|---|
| `OllamaChat.Gateway.AgentByPersonality` | `""` | Map personality → model. Format: `hostile=agent:roast\|friendly=agent:helpful` |
| `OllamaChat.Gateway.MergePersonalityPrompt` | `1` | Prepend bot's personality prompt to gateway system prompt |
| `OllamaChat.Gateway.DetectLanguage` | `0` | Heuristic Cyrillic detection → inject "respond in Russian" |
| `OllamaChat.Gateway.LanguageHintTemplate` | `"Always respond to the user in {lang}."` | Template for the language hint |

**Resolution order for the model field on each request:**

1. Per-bot override (`Gateway.BotOverrides` for this exact GUID)
2. Personality preset (`AgentByPersonality[bot.personality]`)
3. Global `Gateway.Model`

Defining presets requires the upstream gateway (Synthiq) to actually have those agent IDs
configured server-side — define them in `agents.<id>.system` etc. on the gateway side.

### Streaming (PR2)

When the upstream gateway supports OpenAI-style SSE (Synthiq does), enable streaming so
each sentence is whispered as it arrives instead of buffering the whole response.

| Key | Default | Purpose |
|---|---|---|
| `OllamaChat.Gateway.UseStreaming` | `0` | Send `stream: true` and consume SSE |
| `OllamaChat.Gateway.StreamWhisperOnSentence` | `1` | Emit at sentence boundaries (`.`, `!`, `?`). When `0`, only emit when the buffer hits `MaxWhisperChunkChars` |

Pairs naturally with `MaxWhisperChunkChars` — even in sentence mode, an unusually long
sentence is force-split when it crosses the chunk cap.

### Warm sessions (PR1)

When the upstream gateway supports a session-key header (Synthiq does, via
`x-synthiq-session-key`), enable it to keep the agent subprocess warm between
whispers. The agent retains full context server-side without the WoW module
re-sending history each time.

| Key | Default | Purpose |
|---|---|---|
| `OllamaChat.Gateway.UseSessionPersistence` | `0` | Send `x-synthiq-session-key` header |
| `OllamaChat.Gateway.SessionKeyTemplate` | `wow-{botGuid}-{playerGuid}` | Template; supports `{botGuid}` and `{playerGuid}` |
| `OllamaChat.Gateway.SkipHistoryWhenSession` | `1` | When sessions are on, skip sending local history (saves tokens) |

## Setup

1. **Choose which bots are gateway bots.** Find their GUIDs:
   ```bash
   docker exec ac-database mysql -uroot -ppassword acore_characters \
     -e "SELECT guid, name FROM characters WHERE name IN ('Slayobot', 'Helper')"
   ```
2. **Whitelist the human accounts who can talk to them.** Find your account ID:
   ```bash
   docker exec ac-database mysql -uroot -ppassword acore_auth \
     -e "SELECT id, username FROM account WHERE username = 'OPERATOR'"
   ```
3. **Set the conf**:
   ```ini
   OllamaChat.Gateway.Enable               = 1
   OllamaChat.Gateway.Url                  = http://192.168.100.10:18789/v1/chat/completions
   OllamaChat.Gateway.BearerToken          = your-bearer-token
   OllamaChat.Gateway.Model                = openclaw
   OllamaChat.Gateway.BotGUIDs             = 12345,67890
   OllamaChat.Gateway.WhitelistAccountIds  = 1001
   OllamaChat.Gateway.UseSessionPersistence = 1
   ```
4. **Reload** in-game with `.ollama reload` (or from server console: `ollama reload`).
5. **Whisper** one of the gateway bots: `/w Slayobot claude what's up?`. The keyword is
   stripped from the message.

## GM commands

| Command | What it does |
|---|---|
| `.ollama reload` | Re-read `mod_ollama_chat.conf`, reload personalities, **reset gateway runtime stats and cooldowns** |
| `.ollama gateway status` | Print enabled flag, configured bots, whitelist, totals (requests / errors / rate-limited / legacyFallbacks / tokens), and per-bot request counts with last-seen times |

## Tuning

- **Throttle gateway cost without disabling it**: Lower `MaxConcurrentRequests` and raise
  `MinSecondsBetweenRequests`.
- **Make gateway bots feel snappier on long replies**: Drop `MaxWhisperChunkChars` to ~180
  so the first whisper arrives in fewer characters.
- **Survive the gateway being down**: Keep the local classifier/tactical path
  healthy. The old local chat fallback for gateway errors has been removed, so
  gateway request failures are logged/audited and stay silent.
- **Multi-turn agent quality**: Set `UseSessionPersistence = 1`. The agent will remember
  the entire conversation regardless of `MaxHistory`. If you sometimes restart the gateway,
  set `SkipHistoryWhenSession = 0` to also send local history as a safety net.

## Troubleshooting

| Symptom | Likely cause | Check |
|---|---|---|
| Bot stays silent on every whisper | `Gateway.Enable = 0` or bot GUID not in `BotGUIDs` | `.ollama gateway status` |
| `Gateway enabled WITHOUT account whitelist` warning at startup | Whitelist is empty (permissive) | Set `Gateway.WhitelistAccountIds` |
| Bot only answers if you say "claude" first | That's the trigger keyword | Change `Gateway.TriggerKeyword`; `FallbackToOllama` is a deprecated no-op |
| Lots of rate-limited drops in status | Cooldown or concurrency cap is too tight | Bump `MinSecondsBetweenRequests` down or `MaxConcurrentRequests` up |
| Bot replies are cut off mid-sentence | Whispers exceed WoW's per-message limit | Lower `MaxWhisperChunkChars` or check the gateway model isn't returning enormous text |
| Token counts in logs don't match the upstream gateway | The upstream may not return `usage` on every response | Inspect raw response with `OllamaChat.DebugShowFullPrompt = 1` |
| `JSON parse error` on every request | URL is wrong or the gateway returned HTML (e.g., auth redirect) | Check the URL ends in `/v1/chat/completions` and the bearer token is valid |

## Logs to watch

The gateway path logs to `server.loading` with the `[Ollama Chat Gateway]` prefix.

```bash
ssh game-host "docker logs ac-worldserver 2>&1 | grep -iE 'gateway' | tail -40"
```

Useful lines:
- `Sending request to <url>, model=<m>, messages count=<n>` — request fired
- `Tokens — bot=..., player=..., prompt=N, completion=N, total=N` — usage accounting
- `dropping whisper from <player> — cooldown active` — rate limited
- `dropping whisper — concurrency cap reached` — concurrency limited
- `got empty response` — gateway/classifier returned no text; request is audited as an error
- `Whispered gateway response to <player> (N chunks)` — success
