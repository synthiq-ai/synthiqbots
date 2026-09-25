# Tactical (companion-mode) agent

Hierarchical / cascading LLM agent on top of the existing gateway + MCP stack.
Local Ollama drives a low-frequency tactical heartbeat for each tactical-
enabled bot; the paid gateway is **purely reactive** and only fires when
tactical escalates or a human chats. The companion feel — bots that narrate,
ask, and defer — comes from the model's action menu, not from tick frequency.

Also sometimes called *fast+slow*, *System 1 / System 2*, or *LLM cascade*.

---

## Why this exists

The existing gateway was either answering human chat or running a paid leader-
tick sampler every 30s regardless of whether the state actually changed. Fine
when you only had 1-3 gateway bots playing on weekends, bad if you wanted a
bot that *acts like a teammate while you play* — proactive narration, squad
orders, contextual emotes, occasional short party/say lines, pre-pull buffs.
Doing that with the paid gateway would burn tokens nonstop.

Tactical adds a free-inference loop that handles the high-frequency decisions
locally. The paid gateway becomes an oracle the local model *asks when stuck*
instead of a timer-driven sampler.

---

## Architecture

```
┌─────────────────────────────────────────────────────────────┐
│ Strategic (paid gateway) — reactive, NEVER on a timer       │
│ StrategicEscalationWorker (condvar, idle by default)        │
│   • wakes ONLY when tactical emits {"escalate": "<reason>"} │
│   • fires ONE paid call → expects tactical_set_directive    │
│   • rate-capped by Strategic.MaxEscalationsPerBotPerHour    │
│ Also fires on human chat mention (existing gateway path).   │
└──────────────▲─────────────────────────────┬────────────────┘
     escalate  │                             │ directive
     (queued)  │                             ▼
┌─────────────────────────────────────────────────────────────┐
│ Tactical (NEW, free local Ollama) — only clock in system    │
│ TacticalLeaderTick thread, ~10s heartbeat per bot           │
│   • compact snapshot (≤2KB): self state + directive +       │
│     nearby ambient/event context                            │
│   • POST to local Ollama (Mac / PC, config-switchable)      │
│   • parse single JSON action {action, args, escalate?}      │
│   • dispatch via existing MCP tool registry (in-process)    │
│   • can choose tactical_idle, bot_emote, or short bot_say    │
│   • enqueues escalation fire-and-forget when model asks     │
│   • writes audit row per tick                               │
└──────────────┬──────────────────────────────────────────────┘
               │ existing tool calls
               ▼
┌─────────────────────────────────────────────────────────────┐
│ Reactive (existing) — PlayerbotAI engine, untouched         │
│ Threat, assist target, rotations, kiting — owns combat      │
└─────────────────────────────────────────────────────────────┘

For tactical-enabled bots, GatewayLeaderTick proactive sampling is
SUPPRESSED — strategic runs only on escalation or human chat.
Non-tactical bots keep leader-tick as before.
```

### The three roles

- **Reactive layer** — mod-playerbots engine. Untouched. Handles every-frame
  reflexes: focus target, follow, autocast, kiting. **Tactical never
  micromanages combat.**
- **Tactical layer (NEW)** — free local Ollama, per-bot, ~10s heartbeat.
  Compact snapshot in, single structured action out. Picks "what to do next"
  from a curated menu of social + strategic tools. The only clock in the
  system, and the clock is free.
- **Strategic layer (paid gateway)** — purely reactive. Fires **only** on
  (a) `escalate` from tactical, or (b) human chat mention (existing path).
  Updates a per-bot **Directive** (goal string + TTL) that tactical reads on
  its next tick.

---

## Behavior: the companion ethos

Tactical is a **teammate on voice chat**, not an autonomous player. By
design:

- Runs only while a whitelisted human is online and nearby (reuses
  `Gateway.AutoClaimAccountIds`). Log out → tactical bots go dormant. Zero
  ticks, zero Ollama calls, zero tokens.
- Runtime auto-enroll can tick nearby playerbots without hand-maintaining every
  GUID: `Tactical.AutoEnrollNearbyBots=1`, `Tactical.NearbyBotMax=5`.
- Combat tactics stay with the playerbots engine. Dispatcher rejects combat-
  tactic actions during `IsInCombat()` unless `AllowCombatOverride=1`.
- Routine unchanged snapshots should use `tactical_idle`. Emotes/speech are
  reserved for nearby context, recent gameplay events, or material state
  changes, with dispatcher rate limits to prevent spam.
- HITL gate (Loose by default): certain actions (`bot_accept_quest`,
  `bot_equip_item`, `bot_use_item`, `bot_destroy_item`, `bot_mail_send`,
  etc.) require human OK before dispatch.
  *Phase 4-note: the whitelist recognises the set but the confirmation flow
  itself is planned — today tactical either executes or rejects.*
- System prompt ethos: "You are a teammate, not the player. The bot's AI
  engine handles combat mechanics. Ask before acting, narrate plans, defer
  to the human."

---

## Configuration (`OllamaChat.Tactical.*` + `.Strategic.*`)

All keys documented inline in `conf/mod_ollama_chat.conf.dist`. Defaults are
sensible; the knobs you actually touch:

| Key | Default | What it does |
|---|---|---|
| `Tactical.Enable` | `1` | Global kill switch. `0` → all code paths are no-ops |
| `Tactical.BotGUIDs` | `""` | csv of explicitly tactical guids. Empty is valid when nearby auto-enroll is on. |
| `Tactical.Url` | `http://host.docker.internal:11434/api/generate` | Tactical reasoning endpoint. **The URL picks the wire format:** `…/api/generate` is Ollama (LAN/Tailscale IP of a Mac or PC); `…/chat/completions` is any OpenAI-compatible provider (DeepSeek, OpenAI, LiteLLM, the Synthiq gateway) with the bearer from `OpenAiCompat.ApiKey`. See [Backend switching](#backend-switching-ollama--deepseek). |
| `Tactical.Model` | `qwen3:14b` | Model id for background tactical reasoning — an Ollama tag, or the provider's id (`deepseek-flash`). On Ollama keep it GPU-resident. |
| `OpenAiCompat.ApiKey` | `""` | Bearer for every fast-path URL that is `…/chat/completions` (tactical, classifier, per-bot overrides). Ollama URLs never see it. Secret — live conf only, edit over ssh, `config_reload` applies it. |
| `Tactical.HumanPresenceRequired` | `1` | Tactical dormant unless a whitelisted human is online. "Human" = `IsWhitelistedHumanPlayer` (tactical.h): a real client session (`!IsBot()`), no `PlayerbotAI`, not Botmaster / a fleet guid, and (if `Gateway.AutoClaimAccountIds` is set — that key, not `Gateway.WhitelistAccountIds`, which gates chat) on one of those accounts. Since 2026-09-21 the fleet — which lives on the whitelisted account 1001 — no longer satisfies this gate; before, Botmaster and Raz did, so tactical + proactive ran whenever the stack was up. |
| `Tactical.HeartbeatMs` | `10000` | Fallback tick period. Most wakes are event-triggered |
| `Tactical.BotCooldownMs` | `2000` | Min gap between decisions for the same bot |
| `Tactical.RequestTimeoutMs` | `5000` | HTTP timeout. **Bump higher for cold-load of any model ≥4B.** |
| `Tactical.NumCtx` | `2048` | Tactical context window; snapshots are compact, so 2048 is usually enough. |
| `Tactical.MaxConcurrentQueries` | `1` | Default per-endpoint tactical concurrency cap. |
| `Tactical.BotOverridesJson` | `""` | Inline per-bot URL/model/timeout/ctx/concurrency overrides for splitting bots across Ollama hosts. |
| `Tactical.BotOverridesJsonFile` | `""` | Preferred path to the same JSON array; wins over inline JSON and avoids AzerothCore quote stripping. |
| `Tactical.SystemPromptFile` | `modules/mod-ollama-chat/prompts/ollama_tactical.md` | External tactical playbook. Natural-language behavior lives here, not in C++. |
| `Tactical.AmbientEnable` | `1` | Lets tactical produce visible "alive" actions: `bot_emote`, contextual `bot_say`, and idle no-ops. |
| `Tactical.AmbientMinSpeechGapSec` | `60` | Per-bot minimum gap between ambient speech actions. |
| `Tactical.AmbientMinEmoteGapSec` | `45` | Per-bot minimum gap between emotes. |
| `Tactical.AmbientMaxVisibleActionsPerMinute` | `2` | Rolling per-bot visible-action cap. `0` = uncapped. |
| `Tactical.AmbientEventReactionChance` | `35` | Chance to expose nearby recent gameplay events to the tactical prompt. |
| `Tactical.AutoEnrollNearbyBots` | `1` | Tick nearby playerbots in addition to explicit `BotGUIDs`. |
| `Tactical.NearbyBotMax` | `5` | Max runtime auto-enrolled nearby bots per heartbeat. |
| `Tactical.NearbyBotRadius` | `60.0` | Radius from a whitelisted/real human for runtime auto-enroll. |
| `Gateway.OllamaClassifier.Url` | `""` | Empty inherits `Tactical.Url`; set separately for another host. Same URL-driven format rule as `Tactical.Url`. The proactive intent and tick-gate calls reuse this endpoint. |
| `Gateway.OllamaClassifier.Model` | `qwen3:8b` | Separate fast model for local intent routing before paid gateway fallback. |
| `Gateway.OllamaClassifier.TimeoutMs` | `5000` | Classifier timeout; missed calls fall through to gateway. Keep high enough for first-load scheduling, warm calls are normally sub-second. |
| `Gateway.OllamaClassifier.NumCtx` | `2048` | Small classifier context with enough room for the command playbook and runtime context. |
| `Gateway.OllamaClassifier.SystemPromptFile` | `modules/mod-ollama-chat/prompts/ollama_classifier.md` | External classifier playbook. C++ does not phrase-match commands. |
| `Tactical.AllowEscalation` | `1` | Model may set `"escalate"` to get strategic help |
| `Tactical.DirectiveMaxTtlSec` | `600` | Safety clamp on strategic-written directive TTLs |
| `Tactical.AllowCombatOverride` | `0` | Keep at 0 — combat is the engine's job |
| `Strategic.MaxEscalationsPerBotPerHour` | `6` | Hard cap on paid calls per bot |
| `Strategic.EscalationCooldownSec` | `30` | Min gap between paid calls per bot |
| `Strategic.SuppressLeaderTickForTacticalBots` | `1` | Skip the existing paid leader-tick for tactical bots |

### Picking a model

| Model | Size | Where it runs | Notes |
|---|---|---|---|
| `qwen3:8b` | ~5GB | 5080 comfortably | Recommended classifier model. Fast enough for reactive chat-command JSON routing. |
| `gemma4:e4b` | ~5GB | Mac M2 Max or 5080 | Good default. Google MoE-4B, fast, tool-capable. Needs `keep_alive` |
| `qwen3:14b` | ~9GB | 5080 comfortably, Mac M2 Max tight | Recommended tactical model on a 16GB 5080. |
| `qwen3.5:9b` | ~6GB | Both | Newer (Mar 2026), leaner than 14b, strong tool use |
| `qwen3.5:27b` | ~16GB | PC 5080 (tight, Q4), Mac M2 Max 32GB | Personality + reasoning up a notch |

Cold-load on any model >3B can exceed the runtime timeout → **bump
`RequestTimeoutMs` during first-load testing and keep `keep_alive=30m` in the request body
(hard-coded, not a config key)** so the model stays GPU-resident between
ticks. (Ollama only — the OpenAI path below has no cold-load and sends no
`keep_alive`.)

### jev tier in front of the executor (2026-09-21, `OllamaChat.Jev.Tactical.*`)

With `Jev.Enable=1` + `Jev.Tactical.Enable=1` (or a per-bot `"jev": true` in
`Tactical.BotOverridesJsonFile`), each tick asks TypeSafe's jev first — as a
**funnel**, not a 132-way choice: code prunes the menu to the actions that are
both jev-eligible (registry `required` = `{botGuid}`, plus `bot_emote` /
`bot_set_loot_filter` whose one arg a speculative answer fills) and dispatchable
*this tick* (combat block, HITL confirmation, ambient-in-combat, dead/alive),
appends `other`, and jev picks in ~1 s with a calibrated confidence. A confident
pick is dispatched through the same `DispatchTacticalAction`; `other`, low
confidence or any jev failure hands the tick to the LlmWire/DeepSeek call below
with the tick's **remaining** budget — so the LLM path is unchanged, it just runs
less often. Free-text and targeted actions (`bot_say`, `whisper_player`,
`bot_cast`, `bot_move_to`, squad orders, …) are never jev-eligible by
construction. Escalation has its own gate (`Jev.Tactical.EscalateMin`, a yes/no
probability) and never fires together with an emote. The audit row's `backend`
column says which tier decided (`jev` / `llm`). Full design, budget arithmetic
and rollout in [jev.md](jev.md#tactical-funnel-site-b-pr-2).

### Backend switching: Ollama ↔ DeepSeek

Since 2026-09-20 the four fast paths — tactical executor, gateway classifier,
proactive intent, proactive tick-gate — are backend-agnostic. `src/mod-ollama-chat_llmwire.*`
picks the wire format **from the URL**:

| URL ends in | Format | Request shape | Auth |
|---|---|---|---|
| `/api/generate` (or anything else) | Ollama | `model, system, prompt, stream:false, format:"json", keep_alive:"30m", think:false, options.num_ctx` — byte-identical to the pre-adapter body | none |
| `/chat/completions` | OpenAI-compatible | `model, messages[{system},{user}], stream:false, response_format:{type:"json_object"}` | `Authorization: Bearer <OpenAiCompat.ApiKey>` |

So switching is a conf edit + `config_reload`, no rebuild. The production
setting (all fast ops on DeepSeek, the PC/Mac Ollama no longer needed):

```ini
OllamaChat.Tactical.Url   = "https://api.deepseek.com/v1/chat/completions"
OllamaChat.Tactical.Model = "deepseek-flash"
OllamaChat.Gateway.OllamaClassifier.Url   = "https://api.deepseek.com/v1/chat/completions"
OllamaChat.Gateway.OllamaClassifier.Model = "deepseek-flash"
OllamaChat.OpenAiCompat.ApiKey = "sk-..."     # live conf only, never committed
```

To go back to a local Ollama, restore the `…/api/generate` URLs and Ollama
model tags and reload; the key is simply ignored for Ollama URLs. Per-bot
`tactical_overrides.json` entries follow the same rule per entry, so one bot
can stay on a local GPU while the rest use DeepSeek.

Things worth knowing about the OpenAI path:

- **JSON mode requires the word "json" in the prompt** on DeepSeek and OpenAI,
  or the request is rejected with a 400. Every shipped fast-path prompt
  (`ollama_tactical.md`, `ollama_classifier.md`, `proactive_intent.md`,
  `proactive_tick_gate.md`) already contains it — keep it there when editing them.
- `num_ctx` and `keep_alive` are Ollama-only and are dropped; the `NumCtx`
  config keys are inert on this path.
- **`deepseek-flash` reasons by default, and the adapter turns it off.** With
  the real tactical prompt, a default request produced 828 hidden reasoning
  tokens and took 6.2 s; with DeepSeek's `thinking:{type:"disabled"}` — which
  the adapter sends for any `*.deepseek.com` host when the call site asks for
  `noThink` (all four do) — it was 0 reasoning tokens, 14–15 completion tokens,
  1.6–1.8 s, same action. The classifier's 5 s timeout depends on this. Other
  OpenAI-compatible hosts get no thinking field (OpenAI proper rejects unknown
  params; its knob is `reasoning_effort`).
- A provider error is surfaced verbatim in the failure line (tactical
  `RecordFailure`, classifier `LOG_WARN`, proactive `LOG_DEBUG`) — an invalid key
  reads as `provider error: Authentication Fails …`, not as "envelope missing
  'response'". A non-200 from the provider is logged by the HTTP client with the
  status code (`HTTP request failed with status: 401`).
- The worldserver image has OpenSSL compiled in and `ca-certificates`
  installed; this path was the module's first production HTTPS use.
  **Certificate verification is OFF in `OllamaHttpClient`**
  (`enable_server_certificate_verification(false)`, a pre-existing setting for
  ngrok / self-signed endpoints). For DeepSeek that means the bearer travels
  over TLS that is encrypted but not authenticated — a MITM on game-host's egress
  could read it. Acceptable on a home LAN behind NAT for a low-value key; not
  acceptable if this ever carries a key you care about. Turning verification on
  is a one-line change plus the CA store that is already in the image.
- **game-host's egress drops ~1 in 5 SYNs to any public host** (measured 2026-09-20:
  8/10 host→DeepSeek, 8/10 container→DeepSeek, 5/10 container→GitHub; good
  connects are ~140 ms). The HTTP client now uses a 3 s connect timeout
  separate from the read timeout and retries **once** on a transport failure
  only (never on an HTTP status), so a lost SYN costs ~3 s, not the whole
  `RequestTimeoutMs`. Live tactical settings after the switch:
  `MaxConcurrentQueries = 3` (was 1 — three bots were serialised through one
  slot, and one hung connection blocked all three for 30 s) and
  `RequestTimeoutMs = 12000`.
- The tactical loop is per-bot per-tick and was free on Ollama. `deepseek-flash`
  is cheap but not zero — the tactical audit table and `ops audit_gateway` show
  volume.
- DeepSeek exposes exactly two model ids: `deepseek-flash` and `deepseek-v4-pro`
  (`curl https://api.deepseek.com/models -H 'Authorization: Bearer …'`).

### Tactical Ambient Mode

The old `OllamaBotRandomChatter` timer and old event-specific Ollama responder
are removed. Event hooks now record lightweight recent_event_* facts, and the
tactical tick decides whether to do anything with them.

Ambient uses the same Tactical model and action dispatcher:

- `tactical_idle` is the normal answer for unchanged state.
- `bot_emote` is a single safe in-game emote token, not prose.
- `bot_say` routes to party if grouped; otherwise it uses `/say` only when a
  real human is nearby. It stays silent when there is no audience.
- Visible actions are capped per bot by speech/emote gaps and
  `AmbientMaxVisibleActionsPerMinute`.
- Nearby playerbots can be auto-enrolled at runtime with:

```ini
OllamaChat.Tactical.AutoEnrollNearbyBots = 1
OllamaChat.Tactical.NearbyBotMax = 5
```

### Split bots across machines

Use `Tactical.BotOverridesJsonFile` when one bot should use PC Ollama and
another should use MacBook Ollama. Override GUIDs are tactical-enabled
automatically, so `Tactical.BotGUIDs` can stay empty if every tactical bot
appears in the JSON. The file form is preferred because AzerothCore's config
parser strips embedded quote characters from inline JSON values.

```ini
OllamaChat.Tactical.Url = "http://192.168.100.15:11434/api/generate"
OllamaChat.Tactical.Model = "qwen3:14b"
OllamaChat.Tactical.NumCtx = 2048
OllamaChat.Tactical.RequestTimeoutMs = 5000
OllamaChat.Tactical.MaxConcurrentQueries = 1

OllamaChat.Tactical.BotOverridesJsonFile = "/azerothcore/env/dist/etc/modules/tactical_overrides.json"

OllamaChat.Gateway.OllamaClassifier.Url = "http://192.168.100.15:11434/api/generate"
OllamaChat.Gateway.OllamaClassifier.Model = "qwen3:8b"
OllamaChat.Gateway.OllamaClassifier.NumCtx = 2048
OllamaChat.Gateway.OllamaClassifier.TimeoutMs = 5000
```

Example `tactical_overrides.json`:

```json
[
  {
    "guid": 20007,
    "url": "http://192.168.100.15:11434/api/generate",
    "model": "qwen3:14b",
    "numCtx": 2048,
    "timeoutMs": 5000,
    "maxConcurrentQueries": 1
  },
  {
    "guid": 20008,
    "url": "http://<mac-lan-ip>:11434/api/generate",
    "model": "qwen3:14b",
    "numCtx": 2048,
    "timeoutMs": 8000,
    "maxConcurrentQueries": 1
  }
]
```

Concurrency is enforced per resolved URL. With the example above, the PC and
Mac can each run one tactical request at the same time; two bots sharing the
same URL still queue behind that URL's `maxConcurrentQueries`.

---

## Action menu (what tactical can pick)

Defined in `kTacticalAllowedActions` (src/mod-ollama-chat_tactical.cpp).
This is a tool-name safety allowlist, not a phrase parser. Tactical dispatcher
rejects anything not in this set; natural-language examples live in
`prompts/ollama_tactical.md`.

**No-op / quiet presence:** `tactical_idle`

**Social (proactive chat):**
`whisper_player`, `bot_emote`, `bot_say`, `bot_yell`

`bot_emote` is intentionally narrow: the `emote` arg must be a single token,
such as `wave`, `nod`, `cheer`, `laugh`, `shrug`, `dance`, `bow`, `salute`,
`clap`, `point`, `smile`, `sigh`, `thank`, `grats`, `train`, `roar`, `flex`,
`kneel`, `sleep`, or `confused`. Tactical normalizes malformed model output to
a safe token before dispatch so emote choices do not create noisy tool errors.
The prompt tells the model to prefer `tactical_idle` for routine unchanged ticks,
so emotes are contextual rather than the default idle loop.

**Observation (read-only):**
`get_bot_state`, `get_target`, `get_nearby_creatures`, `get_nearby_players`,
`get_nearby_gameobjects`, `get_inventory`, `get_group`, `get_zone_info`,
`get_player_state`, `get_player_gear`, `get_player_quests`,
`find_nearby_quest_givers`, `get_quest_details`, `get_item_details`,
`get_bot_pet`, `query_world_db_lookup`, `get_bot_roster_by_account`,
`get_fleet_status`, `find_trainer`, `find_vendor`, `find_flight_master`,
`find_innkeeper`, `get_bot_stats`, `bot_mail_read`, `get_bot_professions`,
`get_bot_recipes`

**Movement:** `bot_follow`, `bot_stay`, `bot_disperse`

**Strategic (flips engine mode, NOT per-tick override):**
`bot_set_strategy`, `bot_rti`, `bot_reset_ai`, `bot_list_strategies`

**Pre-pull / support:** `bot_cast` (buffs outside combat),
`bot_pet_command`, `bot_set_raid_target_icon`

**Progression:**
`bot_turn_in_quest`, `bot_autogear`, `bot_maintenance`,
`bot_train_spells`, `bot_sell_junk`, `bot_set_loot_filter`,
`bot_add_loot_item`, `bot_remove_loot_item`

**Progression requiring HITL by default:**
`bot_accept_quest`, `bot_abandon_quest`, `bot_choose_quest_reward`,
`bot_equip_item`, `bot_unequip_item`, `bot_use_item`, `bot_destroy_item`,
`bot_set_home`, `bot_bank_deposit`, `bot_bank_withdraw`,
`bot_guild_bank_deposit`, `bot_guild_bank_withdraw`, `bot_mail_send`,
`bot_yell`

**Raid coordination:** `bot_roll`

**Squad orders (only for leader bots, gateway-side allowlist enforces):**
`leader_command`, `leader_command_all`, `leader_list_targets`,
`leader_get_command_help`

### Combat blocklist

While `bot->IsInCombat()` is true and `AllowCombatOverride=0` (default), the
dispatcher **rejects**:
- `bot_attack_target` — engine's threat + assist picks targets; forcing a
  mid-cast flip causes jank
- `bot_stop_combat` — engine exits combat naturally
- `bot_revive` — only meaningful while dead
- `bot_pet_command(command="attack")` — pet target forcing is also combat micro

Rejection returns `{"error": "action blocked during combat (engine owns combat tactics)"}`.
Audit row's `result` column records `blocked` (distinct from `error` so you
can filter rule-triggered rejections).

---

## Escalation & Directive flow

```
Tactical tick:
  snapshot = BuildBotSnapshot(bot)
  if active directive:
      prompt += "Active directive: ..." + directive.goal
  reply = local Ollama(system, prompt)
  if reply.escalate:
      EnqueueEscalation(botGuid, reply.escalate, snapshot)  # non-blocking
  dispatch(reply.action)
  write audit row

Strategic worker (condvar-signalled):
  wait until queue non-empty
  job = queue.pop_front()
  if per-bot hour cap or cooldown hit:  drop silently
  response = QueryGatewayAPI(botGuid, 0, escalation_prompt)  # paid call
  (response is expected to include tool-use of tactical_set_directive)
  log response
  loop back to wait
```

Key properties:

- **Tactical never blocks.** Enqueueing is fire-and-forget; tactical
  continues using the current (possibly stale) directive while the strategic
  worker handles the escalation. When the new directive lands, the next
  tactical tick reads it.
- **One paid call per escalation**, not per tick.
- **Budget hard cap**: `Strategic.MaxEscalationsPerBotPerHour` (default 6).
  Over-cap escalations drop; tactical keeps playing.

### The two new MCP tools

- **`tactical_get_directive(bot_guid)`** — read-only; returns
  `{active: true, goal, expires_at, set_by}` or `{active: false}`. Safe for
  any tool-use client or human operator.
- **`tactical_set_directive(bot_guid, goal, ttl_sec)`** — write a new goal.
  Callers: the operator's external MCP agent (robot), the voice fallback
  (`talk_to_leader`-driven gateway calls), and the paid gateway responding to
  an escalation. Gated:
  - Requires `Mcp.AllowActionTools=1`
  - Target bot must be in `Tactical.BotGUIDs`
  - `ttl_sec` clamped to `Tactical.DirectiveMaxTtlSec`
  - Empty `goal` clears the directive

### Directive arbitration — operator wins

Every write is labeled with its source at the dispatch boundary:

| `set_by` | Origin | How it's detected |
|---|---|---|
| `mcp` | the operator's external agent (robot) | HTTP MCP call with no player identity (`playerGuid=0`, not escalation context) |
| `player:<guid>` | voice fallback / an in-world player | `playerGuid` carried through the tool loop (`talk_to_leader` requires it) |
| `escalation` | the strategic escalation worker's gateway call | thread-local flag set around `QueryGatewayAPIWithTools` in the worker (`InEscalationContext()`) — the gateway tool loop runs tools synchronously on the worker thread |

Precedence: **`mcp` / `player:` writes always win** and replace anything. An
`escalation` write (set *or* clear) against an **active** `mcp`/`player`
directive is rejected with:

```json
{"ok": false, "reason": "superseded_by_operator_directive",
 "active_goal": "...", "set_by": "mcp", "expires_at": 1790000000}
```

so the gateway agent knows to work toward the operator's goal instead. Expired
operator directives do not block (`IsActive()` decides). Escalation-over-
escalation replacement is allowed. The in-memory directive map is empty after a
worldserver restart, so no legacy `strategic`-labeled rows survive to confuse
the rule.

---

## Operations

### GM command

```
.ollama tactical status [botGuid]
```

No arg = every configured tactical bot. Dumps: config snapshot, per-bot
online/in-combat state, active directive + remaining TTL.

### Audit table

`mod_ollama_chat_tactical_audit` (migration:
`data/sql/characters/base/2026_04_24_tactical_audit.sql`). One row per
tactical tick when `EnableAudit=1`.

Columns: `id`, `bot_guid`, `bot_name`, `ts`, `action`, `result`
(`ok`|`error`|`blocked`), `error`, `escalated`, `escalation_reason`,
`latency_ms`, `in_combat`.

Useful queries:

```sql
-- What's gemma picking over the last hour?
SELECT action, result, COUNT(*) n, AVG(latency_ms) avg_ms
FROM acore_characters.mod_ollama_chat_tactical_audit
WHERE ts >= NOW() - INTERVAL 1 HOUR
GROUP BY action, result
ORDER BY n DESC;

-- How many escalations per bot today?
SELECT bot_name, COUNT(*) n
FROM acore_characters.mod_ollama_chat_tactical_audit
WHERE ts >= CURDATE() AND escalated = 1
GROUP BY bot_name;

-- p95 latency by action
SELECT action,
       MIN(latency_ms)                                        AS min_ms,
       AVG(latency_ms)                                        AS avg_ms,
       MAX(latency_ms)                                        AS max_ms
FROM acore_characters.mod_ollama_chat_tactical_audit
WHERE ts >= NOW() - INTERVAL 1 HOUR
GROUP BY action;
```

Retention: `Tactical.AuditRetentionDays` (default 7). Prune runs on startup
and on `.ollama reload`. Set to `0` to disable pruning.

### Hot reload

```
.ollama reload
```

Re-reads config, restarts tactical + strategic workers, resets the circuit
breaker. Use to swap model, URL, or bot allowlist without a worldserver
restart.

---

## Troubleshooting

**Tactical isn't firing at all.**
- `.ollama tactical status` — check `enable=1`, and either configured
  `BotGUIDs` or `AutoEnrollNearbyBots=1` with playerbots near the human.
- Check `humanPresenceRequired=0` OR a whitelisted account is online in-world.
- No whitelisted account in `Gateway.AutoClaimAccountIds`? Tactical falls
  back to "any real player online" — still needs at least one non-bot
  character in the world.

**Endpoint "UNREACHABLE — pausing tactical inference for 30s".**
- Circuit breaker opened after 3 failed HTTP calls. Check:
  - Ollama is actually running on the Mac/PC (`curl -sS <Url>` should
    return JSON)
  - Mac firewall isn't blocking the port (default binds to 127.0.0.1 — set
    `OLLAMA_HOST=0.0.0.0:11434` in your shell env)
  - Model is pulled (`ollama list` on the Mac/PC)
  - The docker container can reach the Mac's LAN IP
- Breaker auto-probes every 30s; logs `RECOVERED` when the endpoint comes
  back. No manual reset needed.

**500 from Ollama with "context canceled".**
- `RequestTimeoutMs` is shorter than the cold-load time of the model. Bump
  to 30000. `keep_alive=30m` in the request body (hard-coded) keeps the
  model GPU-resident between ticks so subsequent calls are fast.

**Model picks `whisper_player` but no one gets the message.**
- Check the audit row — `result=error`, `error='name and message are
  required'` means the model didn't supply args. It's reaching for a social
  action but no human target name is in its prompt. Log in a whitelisted
  character or wait until the snapshot includes `get_nearby_players` output.

**Strategic escalations never fire.**
- `Tactical.AllowEscalation` must be `1`.
- Local model must emit `{"escalate": "<reason>"}`. Small models may not do
  this reliably — consider a bigger model (qwen3:14b+).
- Per-bot hour cap hit? `.ollama tactical status` or check
  `mod_ollama_chat_tactical_audit` for `escalated=1` rows.

**Double-billing** (tactical + leader-tick both firing on Claude).
- `Strategic.SuppressLeaderTickForTacticalBots=1` (default) fixes this.
  Set to `0` only if you explicitly want both layers running.

---

## Performance notes

- Per-tick cost: one HTTP round-trip to Ollama (~100-1500ms depending on
  model + prompt) + one in-process tool dispatch (~sub-ms for read tools,
  up to tens of ms for DB-backed ones).
- Model eviction: Ollama unloads idle models after ~5min. `keep_alive=30m`
  in the request body keeps the tactical model hot across ticks. Without
  this, every tick after a 5min idle hits cold load.
- Concurrency cap: `Tactical.MaxConcurrentQueries` (default 1) bounds each
  resolved Ollama endpoint URL. PC and Mac endpoints can run concurrently;
  bots sharing one URL queue behind that URL's cap. Strategic is serial
  (1 worker thread is enough — escalations are rare by design).
- Snapshot budget: `Tactical.PromptMaxBytes` (default 2048). Keep it low;
  the local model sees this every tick. Big prompts slow inference
  disproportionately.

---

## Cross-references

- `docs/gateway.md` — the paid gateway this rides on top of.
- `docs/autonomous-bots.md` — MCP fleet runbook; tactical is one of the
  clients hitting the same MCP registry.
- `conf/mod_ollama_chat.conf.dist` — the authoritative config reference
  with every key documented inline.
- `src/mod-ollama-chat_tactical.{cpp,h}` — everything tactical.
- `src/mod-ollama-chat_tools.cpp` — `tactical_get_directive` /
  `tactical_set_directive` tool handlers.
- `src/mod-ollama-chat_leadertick.cpp` — now honours
  `Strategic.SuppressLeaderTickForTacticalBots`.
