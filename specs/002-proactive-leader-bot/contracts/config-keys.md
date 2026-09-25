# Contract: `OllamaChat.Proactive.*` config keys

**Feature**: 002-proactive-leader-bot
**Target file**: `conf/mod_ollama_chat.conf.dist` (and the live `mod_ollama_chat.conf` on game-host)
**Reload behavior**: picked up by `.ollama reload` GM command and by `config_reload { targets: ["mod_ollama_chat"] }` MCP tool. The new keys land in the existing `g_*` global block in `_config.h`/`_config.cpp` following the established pattern.

All keys are **additive**. No existing key is renamed or repurposed.

---

## Schema

| Key | Type | Default | Min | Max | Description |
|---|---|---|---|---|---|
| `OllamaChat.Proactive.Enable` | bool (0/1) | `0` | 0 | 1 | Master kill switch (FR-018). When 0, `Heartbeat` and `IsEngaged` return immediately, so no proposal, nudge, chat line or `proact_*` row is produced by the loop. NOT literally "every code path": `RegisterScripts()` is called unconditionally (`_main.cpp:28`) and neither `OnPlayerLogin` (`_proactive.cpp:2751`) nor `OnPlayerLogout` (`:2777`) consults this flag — login still seeds the `(player, 0)` sentinel cadence row, and logout still runs its six cleanup loops and emits `proact_abort` `logout` / `botLogout` rows for any proposal left non-terminal by a mid-session `Enable` 1→0 reload. Audit INSERTs are gated by `Proactive.EnableAudit` (`:552`), not by this key. |
| `OllamaChat.Proactive.ProposalCooldownSec` | uint | `120` | 30 | 3600 | Minimum gap between consecutive proposals to the same player. |
| `OllamaChat.Proactive.FirstProposalDelaySec` | uint | `15` | 0 | 600 | Wait after player login before opening with the first proposal. Gives the player time to load in. |
| `OllamaChat.Proactive.IdleThresholdSec` | uint | `180` | 30 | 3600 | Player-idle threshold (no movement / chat / quest event) before an idle nudge is eligible. |
| `OllamaChat.Proactive.IdleNudgeCooldownSec` | uint | `300` | 60 | 3600 | Minimum gap between consecutive idle nudges. |
| `OllamaChat.Proactive.IdleNudgeMaxPerSession` | uint | `3` | 0 | 100 | Self-silence after N consecutive ignored idle nudges in one session (FR-011). 0 = never nudge. |
| `OllamaChat.Proactive.ProposalMaxPerSession` | uint | `5` | 0 | 100 | Self-silence the propose loop after N consecutive `open → expired` (unanswered) proposals (FR-023) — the proposal analogue of `IdleNudgeMaxPerSession`. The counter (`consecutiveExpiredProposals`) lives on the (player, bot) cadence row and resets on player chat / quest turn-in / zone change; it is NOT bumped by an explicit reject or done, which are answers rather than silence. 0 = never self-silence. Despite the name this is a **consecutive-silence** cap, not a per-session proposal budget: a player who answers every proposal never trips it. |
| `OllamaChat.Proactive.ProximityYards` | float | `60.0` | 5.0 | 500.0 | 3D-distance threshold beyond which the leader bot stops travel and waits (FR-007). |
| `OllamaChat.Proactive.ProximityWarnCooldownSec` | uint | `30` | 5 | 600 | Minimum gap between consecutive "you coming?" lines (FR-007 implicit). |
| `OllamaChat.Proactive.ActivityCompleteTimeoutSec` | uint | `1800` | 60 | 86400 | Hard timeout for an `executing` Activity Plan (FR-021, default 30 min). |
| `OllamaChat.Proactive.ActivityCompleteNudgeWaitSec` | uint | `120` | 30 | 3600 | After the activity-complete-timeout nudge fires, wait this long for an answer before marking `aborted`. |
| `OllamaChat.Proactive.ProposalExpirySec` | uint | `600` | 60 | 86400 | Open proposal auto-expires if not answered. |
| `OllamaChat.Proactive.PreferredVoiceGuid` | uint64 | `0` | 0 | ∞ | Optional designated-voice override. 0 = use lowest-guid election (R-6). When set, MUST be a guid listed in **`OllamaChat.Mcp.Leader.BotGUIDs`** — that is the only set `ElectDesignatedVoice`'s `eligible()` predicate consults (`_proactive.cpp:453`, fed from `_config.cpp:1065`). A guid absent from it fails eligibility at `:476` and the override is **silently ignored**: election falls through to the lowest-guid path, a voice *is* elected, so nothing is logged and no `proact_novoice` row is written. Ranks BELOW stickiness — an in-flight proposal/plan pins the voice to its owner until that proposal reaches a terminal state, so re-pointing this key mid-activity does not take effect until the current cycle ends (`docs/proactive-leader.md:115`). |
| `OllamaChat.Proactive.ProposerPromptFile` | string | `modules/mod-ollama-chat/prompts/proactive_proposer.md` | — | — | Path to the proposer system prompt (R-3). |
| `OllamaChat.Proactive.MaxQuestRankSearch` | uint | `50` | 5 | 500 | Cap on how many in-log / in-zone quest candidates the ranking step considers per tick. |
| `OllamaChat.Proactive.DungeonCandidates` | string (csv of mapIds) | `""` (empty = all eligible) | — | — | Optional whitelist of dungeon mapIds to ever propose. Empty = no whitelist (all level-appropriate dungeons in scope). Non-numeric tokens are skipped silently. Applied AFTER the level / faction / continent filters, so it can only narrow the candidate set, never widen it — see the fallback rule below. |
| `OllamaChat.Proactive.EnableAudit` | bool (0/1) | `1` | 0 | 1 | Persist every proactive state transition and chat line to `mod_ollama_chat_gateway_audit` (`source_channel = 'proact_*'`). Deliberately independent of `OllamaChat.Gateway.EnableAudit` so the audit-driven improvement loop still has data on deployments that keep gateway-wide audit off for cost. Gates the INSERT only (`_proactive.cpp:552`); the `LOG_INFO` mirror of each row is unconditional. |
| `OllamaChat.Proactive.LevelUpCongratsEnable` | bool (0/1) | `1` | 0 | 1 | Send a short congratulation when a whitelisted human levels up in play (`OnPlayerLevelChanged`; fires only on a real `Player::GiveLevel` transition, never at character creation). Deterministic-only by design — the line is built by `DeterministicPhrasing` directly, never reaches `PhraseLine`, and makes no LLM call. Audited on `proact_nudge` with reason `levelUp`. |
| `OllamaChat.Proactive.LevelUpCongratsCooldownSec` | uint | `60` | 0 | 3600 | Minimum gap between level-up congratulations per (player, elected-voice) pair. Guards a rapid multi-level burst (rested-XP dump, quest-turn-in chain, GM `.levelup`) from spamming the line. |
| `OllamaChat.Proactive.NoVoiceAuditEnable` | bool (0/1) | `1` | 0 | 1 | Write a diagnostic `proact_novoice` row when the heartbeat has to skip a whitelisted-online player because `ElectDesignatedVoice` returned 0. Emits no chat line and makes no LLM call. The reason is carried by `request_chars` — decoder in `contracts/audit-events.md`. |
| `OllamaChat.Proactive.NoVoiceAuditCooldownSec` | uint | `300` | 0 | 86400 | Minimum gap between `proact_novoice` rows per player, throttled on the `(player, 0)` sentinel cadence row (a voice-less tick has no bot guid to key a normal row on). A permanently voice-less fleet would otherwise write one row per player per heartbeat — ~8.6k rows/player/day at the 10 s tactical tick. |

**Min / Max are advisory.** No clamping exists: every key above is read with a bare `sConfigMgr->GetOption<T>(key, default)` (`_config.cpp:1266-1288`) and no post-read validation runs. An out-of-range value is used verbatim, so e.g. `ProximityYards = 10000` disables the proximity guardrail rather than being pinned to 500. The columns record the range the design was written against, not an enforced bound.

---

## Sibling namespaces — the LLM tiers

Phases A/B/C ship under their own key prefixes, NOT under `OllamaChat.Proactive.*`. They are part of this feature and are listed here so this contract covers the whole shipped surface.

| Key | Type | Default | Description |
|---|---|---|---|
| `OllamaChat.ProactiveIntent.Enable` | bool (0/1) | `1` | Phase A — run a local-Ollama intent classifier ahead of the deterministic matcher in `ClassifyPlayerMessage`. Direct-command tokens still short-circuit in C++ first. |
| `OllamaChat.ProactiveIntent.PromptFile` | string | `modules/mod-ollama-chat/prompts/proactive_intent.md` | Phase A system prompt. Unlike `Proactive.ProposerPromptFile`, a missing file does NOT disable the feature — it logs `LOG_ERROR` and falls back to the deterministic substring matcher. |
| `OllamaChat.ProactiveIntent.MinConfidence` | float | `0.7` | Below this the classifier verdict is discarded and the deterministic matcher decides. |
| `OllamaChat.ProactiveIntent.TimeoutMs` | uint | `3000` | HTTP timeout for the Phase A call. 0 is treated as the 3000 default. |
| `OllamaChat.ProactivePlanner.Enable` | bool (0/1) | `0` | Phase B — paid gateway planner that can veto a proposal (`wait`) or approve it and write the line (`propose`). Default OFF because it bills per tick. |
| `OllamaChat.ProactivePlanner.PromptFile` | string | `modules/mod-ollama-chat/prompts/proactive_planner.md` | Phase B system prompt. Missing file logs `LOG_ERROR` and falls back to `SelectCandidateForPlayer` + `PhraseLine`; it does not disable the feature. |
| `OllamaChat.ProactiveTickGate.Enable` | bool (0/1) | `0` | Phase C — cheap local-Ollama gate that vetoes obvious no-go ticks before candidate selection and the paid planner. |
| `OllamaChat.ProactiveTickGate.PromptFile` | string | `modules/mod-ollama-chat/prompts/proactive_tick_gate.md` | Phase C system prompt. Missing file falls through to the deterministic propose path; never disables the feature. |
| `OllamaChat.ProactiveTickGate.IntervalSec` | uint | `30` | Per-(bot, player) throttle on the Phase C call, so a 10 s heartbeat queries Ollama at most twice a minute. |
| `OllamaChat.ProactiveTickGate.TimeoutMs` | uint | `2000` | HTTP timeout for the Phase C call. 0 is treated as the 2000 default. |
| `OllamaChat.ProactiveTickGate.MinConfidence` | float | `0.6` | A `wait` verdict below this threshold is ignored and the tick proceeds to candidate selection. |

A Phase C `wait` veto does NOT consume the proposal cooldown (the `IntervalSec` throttle is the cost control for the Ollama call); a Phase B `wait` verdict DOES, so the gateway is not re-queried every heartbeat.

---

## Inheritance / fallback rules

- `Proactive.Enable=1` AND `Tactical.Enable=1` AND `Tactical.HumanPresenceRequired=1` (existing default) are **all required** for proactivity to engage. The proactive path piggybacks on tactical's presence gate; if the tactical gate fails, the proactive gate fails by construction.
- `Proactive.ProposerPromptFile` MUST exist on disk at startup. If missing, log `LOG_ERROR` once and set `Proactive.Enable` to 0 in-process (refuse to engage rather than emit broken chat lines).
- Empty `Proactive.DungeonCandidates` means "no operator-configured whitelist" — the candidate selector falls back to all WoTLK dungeons the player is currently eligible for. "Eligible" is FOUR filters, not two (`_proactive.cpp:1478-1482`, in order): level bracket with slack (`playerLvl + 3 >= row.minLevel` AND `playerLvl <= row.maxLevel + 5`), faction-side entrance (`factionFlag` 0 = both, 1 = alliance, 2 = horde), **same continent as the player** (`row.continentMapId == player->GetMapId()`), and finally the whitelist. The continent filter is the one with a surprising operator consequence: a whitelist that names only off-continent dungeons yields NO rank-3 candidate at all, and selection silently drops to the rank-4 soft zone hint. Set the whitelist to cover the continents the player actually plays on, or leave it empty.
- `Proactive.DungeonCandidates` cannot make an otherwise-ineligible dungeon proposable. It is a narrowing filter applied last, so whitelisting a level-80 instance for a level-20 character has no effect.

---

## Backwards compatibility

The feature adds **32 keys as shipped** — 21 under `OllamaChat.Proactive.*` plus 11 across the three LLM-tier namespaces (4 `ProactiveIntent.*`, 2 `ProactivePlanner.*`, 5 `ProactiveTickGate.*`). The original design named 15; the other 17 arrived with later increments (FR-023 self-silence, the proactive-owned audit toggle, the level-up wedge, the `proact_novoice` diagnostic, and Phases A/B/C). Keep this count in step with `_config.cpp:1266-1311` — it is the cheapest tell that this contract has drifted from the shipped surface again.

No existing key changes meaning. Server operators who do NOT set `Proactive.Enable=1` get **behavior indistinguishable from today** from the player's point of view — no proposal, nudge, line or `proact_*` row from the loop. See the `Proactive.Enable` row for the two hooks that still run unconditionally; neither is player-visible.

---

## Implementation status (verified at `55f84c71`)

Read this before citing a row above as the arbiter of a code-vs-doc dispute — this contract is used that way (PR #407 settled the `ProposerPromptFile` hard-disable off it), so where it is aspirational rather than shipped, it has to say so.

**Enforced exactly as written**: the `Proactive.Enable` + `Tactical.Enable` + `Tactical.HumanPresenceRequired` conjunction (`IsEngaged`, `_proactive.cpp:2501-2507`, per T011), and the `ProposerPromptFile` hard-disable (`OnConfigReloaded`, `:2962-2972`).

**Enforced more narrowly than the design describes — voice eligibility.** Five surfaces state the eligible set as `Gateway.BotGUIDs ∩ Mcp.Leader.BotGUIDs`: `data-model.md:124`, `research.md:112`, `tasks.md` T010, `quickstart.md:13`, `conf/mod_ollama_chat.conf.dist:2229-2230` and `docs/proactive-leader.md:51`. The shipped `eligible()` predicate checks only the `Mcp.Leader.BotGUIDs` half (`_proactive.cpp:453`); `g_GatewayBotGUIDSet` is never referenced anywhere in `_proactive.cpp`, and the downstream `QueryGatewayAPIRaw` (`_gateway.cpp:1196`) does not gate on it either — it resolves a per-bot URL/token/model and falls back to the defaults. Net effect: a leader-listed bot that is NOT in `Gateway.BotGUIDs` **can** be elected as a voice and will get generative phrasing through the default gateway config. That is permissive, not unsafe (`PhraseLine` falls back to `DeterministicPhrasing` on an empty response, `:1233-1239`), which is why it is recorded here rather than patched: tightening `eligible()` is a live behavior change that would silence the loop outright on any deployment whose `Gateway.BotGUIDs` is unset. The design block above is deliberately left as the design. Decide the direction before changing either side.

**Not enforced at all**: the Min / Max columns (see the note under the schema table). No clamp, no warning, no rejection.

When a later fire changes any of this, update this section in the same PR — the row descriptions above are what get copied into `conf/` and `docs/`, so a fiction left here propagates.

---

## Operator playbook (informative)

Enable proactivity for one player on the live server:

```ini
# /azerothcore/env/dist/etc/modules/mod_ollama_chat.conf
OllamaChat.Proactive.Enable                       = 1
OllamaChat.Proactive.PreferredVoiceGuid           = 20007        # Claude (paladin) — change to taste
OllamaChat.Proactive.FirstProposalDelaySec        = 10
OllamaChat.Proactive.ProposalCooldownSec          = 120
OllamaChat.Proactive.IdleThresholdSec             = 180
OllamaChat.Proactive.IdleNudgeMaxPerSession       = 3
OllamaChat.Proactive.ProposalMaxPerSession        = 5
OllamaChat.Proactive.ProximityYards               = 60.0

# REQUIRED for the PreferredVoiceGuid line above to do anything — the guid must
# be in this set or the override is silently ignored (see the schema row).
OllamaChat.Mcp.Leader.BotGUIDs                    = "20007,20008,20009"
```

Then either restart the worldserver via CI/CD redeploy, or run `.ollama reload` in-game once and verify with:

```bash
# from game-host SSH (read-only inspection)
grep -i "proactive" /azerothcore/env/dist/logs/worldserver.log | tail
```

Disabling reverts behavior with one config edit:

```ini
OllamaChat.Proactive.Enable = 0
```

Followed by `.ollama reload` (no rebuild needed).
