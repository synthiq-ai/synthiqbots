# Phase 0 Research: Proactive Leader Bot

**Feature**: 002-proactive-leader-bot
**Date**: 2026-05-11

This document resolves the open technical unknowns implied by the spec so Phase 1 can be designed without further clarification. There were **no `NEEDS CLARIFICATION` markers** left in the spec after the 2026-05-11 clarification session — the unknowns here are *integration* unknowns, not requirement unknowns.

---

## R-1: Heartbeat clock for the proactive evaluation loop

**Decision**: Reuse the existing `TacticalLeaderTick` heartbeat in `src/mod-ollama-chat_tactical.cpp`. Add a single call site `ProactiveEvaluateTick(botGuid, playerGuid)` at the top of each per-bot tick body, gated by `g_ProactiveEnable` and the existing `Tactical.HumanPresenceRequired` check.

**Rationale**:
- The spec assumption section already commits to this: *"The proactive proposal cadence borrows the existing tactical heartbeat / event-trigger mechanism rather than introducing a new clock."*
- `TacticalLeaderTick::Run()` already iterates per-bot at `Tactical.HeartbeatMs` (default 10s) and already gates on whitelisted human presence (`Gateway.AutoClaimAccountIds`). That is exactly the right shape.
- Adding a second background thread would (a) double the wake-up cost and (b) require its own presence-gating, defeating the existing dormancy guarantee.
- The new call is cheap: ~99% of ticks will exit early because no proposal is due. Only when the cadence state says "time to propose / nudge / check proximity / detect completion" does it do real work.

**Alternatives considered**:
- *New dedicated `ProactiveTick` thread* — rejected. Adds presence-gating duplication and a second wake-up budget without any benefit; we explicitly do NOT need a different cadence from tactical.
- *Event-only (no clock)* — rejected. Idle-nudge by definition requires "time has passed with no events." A clock is necessary.
- *Reuse `GatewayLeaderTick`* — rejected. `GatewayLeaderTick` is suppressed for tactical-enabled bots (`Strategic.SuppressLeaderTickForTacticalBots=1`), and our designated voice will be a tactical bot; the suppression would silence proactivity too.

---

## R-2: Yes/no/done intent classification

**Decision**: Reuse `TryGatewayIntentClassify` in `src/mod-ollama-chat_gateway.cpp`. Extend `prompts/ollama_classifier.md` with three additional intent labels: `proactive_approve`, `proactive_reject`, `proactive_done`. No new HTTP endpoint, no new model, no new C++ classifier code. When `ProactiveOnPlayerChat()` sees an open proposal for the speaking player, it calls the existing classifier and switches on the returned intent label.

**Rationale**:
- `TryGatewayIntentClassify` already exists, already uses the same local Ollama endpoint as tactical (`Tactical.Url` fallback), already has a min-confidence gate (`Gateway.OllamaClassifier.MinConfidence`, default 0.8), and already returns a structured intent string parsable in C++. Adding labels is a prompt edit, not new code.
- FR-004 calls out "natural-language approval/rejection variants" — exactly what an LLM classifier is good at and what regex would be brittle at.
- SC-002 sets ≥95% classification accuracy on hand-labelled sets. The existing classifier path is already in production at that quality level for other intents.
- If the classifier returns low confidence or no proactive intent, we fall through to the existing chat-handler path (so commands like "follow me" still work as today — FR-014).

**Alternatives considered**:
- *Pure keyword matching* — rejected. Spec FR-004 explicitly requires "yes" / "ok" / "let's go" / "sure" / "nah" / "skip" / etc. The handcrafted regex would either miss casual phrasing or false-positive on unrelated chat.
- *Paid gateway classifier* — rejected. Every player message would burn paid tokens for a yes/no, which violates the cost-shaping rationale that motivated the existing local classifier.
- *Dedicated new local model endpoint* — rejected. No reason to fragment endpoints; the classifier model is already loaded.

---

## R-3: Proposal phrasing (the LLM that writes the actual chat lines)

**Decision**: Reuse the existing gateway path (`QueryGatewayAPI`) with a new system prompt `prompts/proactive_proposer.md`. The prompt takes a structured snapshot — selected candidate (quest id+title OR dungeon id+name), player character name, player class/level, and a short list of allowable line types — and returns a single short chat line (no tool calls). The C++ caller emits it on the channel chosen per FR-017 (party-if-grouped, else whisper).

**Rationale**:
- The candidate selection logic is **deterministic in C++** (per Clarifications Q3 / FR-006). The LLM never picks the activity — it only phrases the proposal so it reads as natural conversation. This keeps the loop debuggable and cheap.
- Existing gateway machinery (`QueryGatewayAPI`, audit, rate limits, streaming on/off) already handles non-tool-using calls — we're using a strict subset.
- Phrasing variety per Spec FR-017 ("read as natural conversation, not command-shell output") requires generative output. Static templates would feel robotic by turn 3.
- Cost: at most one paid phrasing call per proactive line, capped by `AmbientMaxVisibleActionsPerMinute` and the explicit idle-nudge cooldowns. Should land in the same per-bot order as existing tactical ambient.

**Alternatives considered**:
- *Static template strings in C++* — rejected. Spec FR-017's natural-conversation bar is too high.
- *Use tactical's local Ollama for phrasing too* — viable but rejected for v1. The local tactical model is tuned for short structured JSON actions, not for charming chat lines; the paid path's quality is materially better for player-facing phrasing. Revisitable later if cost data justifies it.
- *Two-stage: local model drafts, paid model refines* — rejected. Over-engineered for v1.

---

## R-4: Quest log + zone candidate selection

**Decision**: Use AzerothCore's existing `Player::GetQuestStatus`/`Player::GetQuestSlotQuestId` for in-log quests, and `acore_world.quest_template` joined to the player's level/race/faction for in-zone candidates. Wrap the ranked query (per FR-006 / Clarifications Q3) into a single C++ helper `proactive::SelectCandidateForPlayer(Player const*)` that returns one of:
- `{kind=quest, questId, title, hasProgress, zoneId}` — for ranks (1) and (2)
- `{kind=dungeon, mapId, name, entranceCoords}` — for rank (3)
- `{kind=fallbackZone, zoneId, name, levelRange}` — for rank (4), used as a soft suggestion ("let's head to <zone>") rather than a concrete approve-this proposal
- `{kind=none}` — nothing applicable (bot stays silent that tick)

**Rationale**:
- Spec assumption: "no new schema." The existing `acore_world.quest_template` + `acore_world.quest_template_addon` + player-side in-memory quest log are sufficient.
- Quest log is in-memory on `Player*` — no DB hit per tick.
- Dungeon enumeration uses static map data the worldserver already loads; we can keep a small const table of WoTLK 3.3.5a instance IDs + entrance coords (or read `acore_world.areatrigger_teleport`).
- Deterministic ranking lives in C++, not in the LLM, so behavior is reproducible and unit-testable (the unit-test path is "feed a fake `Player*` snapshot into the candidate selector and assert the ranked result", suitable for headless test if/when we add one).

**Source of truth for the dungeon table (pinned)**:
The level-range + entrance-coord table for the WoTLK 3.3.5a instance set is **hand-curated as a `static constexpr` array** at the top of `src/mod-ollama-chat_proactive.cpp` (file-local, not exported). Rationale:

- Reading `Map.dbc` from a server-side module is uglier than just listing the ~35 entries (Classic + TBC + WotLK 5-mans). The set is fixed for 3.3.5a — it doesn't grow.
- `acore_world.areatrigger_teleport` gives entrance coordinates but doesn't carry the level-bracket or "this is a 5-man" semantics we need; combining DBC + DB at runtime adds complexity for a static result.
- Hand-curation lets each entry carry the recommended level range, faction-side flag (for cross-faction instances like Ramparts that have separate ally/horde entrances), and a continent id, all in one literal.

Each row: `{ uint32 mapId; uint32 minLevel; uint32 maxLevel; uint32 continentMapId; float entranceX, Y, Z; uint8 factionFlag; const char* name; }`. Filtered against the player's level, faction, and current-continent membership; further filtered against `g_ProactiveDungeonCandidates` (config csv whitelist) when non-empty. The array is the single source of truth — no fallback to DBC/DB.

**Alternatives considered**:
- *Hand-maintained config-driven activity list* — rejected. Doesn't scale, won't track player progress (for quests). Dungeons stay hand-curated because the dungeon set IS static.
- *Push selection into the LLM prompt* — rejected per Clarifications Q3 — LLM phrases, doesn't choose.
- *Defer dungeon support to a follow-up* — rejected; the user's request explicitly named dungeons, and Clarifications Q1 kept them in v1 scope.
- *Build the dungeon table from `Map.dbc` + `acore_world.areatrigger_teleport`* — rejected for the reasons above. Revisit only if Cataclysm or MoP support is ever bolted on.

---

## R-5: Quest completion / dungeon completion detection

**Decision**: Per Clarifications Q2 (hybrid model):
- **Quest turn-in event**: hook AzerothCore's `PlayerScript::OnPlayerCompleteQuest` (already used elsewhere in this module via `_events.cpp` event recording). When the player whose proposal is `executing` completes the proposed quest id, mark the Activity Plan `completed` and return to the proposal cycle.
- **Dungeon zone-out**: hook `PlayerScript::OnPlayerUpdateZone` / `OnPlayerMapChanged`. When the player leaves the instance map id we're tracking, mark `completed` (or `aborted` if the boss-kill flag wasn't set).
- **Explicit player command**: `ProactiveOnPlayerChat()` recognizes the existing classifier's `proactive_done` intent and immediately transitions the Activity Plan to `completed` regardless of signal state.
- **Timeout safety net** (FR-021 30 min default): a counter incremented in the existing tactical heartbeat; on expiry, send one chat nudge and, after a further short wait with no answer, mark `aborted`.

**Rationale**:
- Both `OnPlayerCompleteQuest` and `OnPlayerUpdateZone` are already wired into AzerothCore's PlayerScript surface; tap-in is one new hook each in `_events.cpp`.
- Hybrid (signal + explicit + timeout) was the explicitly chosen path; this maps each leg to a concrete tap-in.

**Alternatives considered**:
- *Poll player state on every tactical tick* — rejected. AzerothCore's event hooks exist precisely to avoid polling; no reason to pay that cost.
- *Final-boss kill via combat-log scrape* — viable but unnecessary; zone-out is a more robust signal for "we're done with this run."

---

## R-6: Designated-voice election among multiple leader-capable bots

**Decision**: Deterministic selection: among the gateway leader-capable bots (`Gateway.BotGUIDs ∩ Mcp.Leader.BotGUIDs`) currently online and within the same world as the player, choose the one with the **lowest guid**. Re-evaluated each heartbeat. Persisted only in-memory.

**Rationale**:
- Spec edge case requires exactly one voice — anything else risks 3 simultaneous "let's quest!" lines.
- Lowest-guid is stable across ticks (no flapping when the same bots are online) and trivially deterministic.
- An explicit override config key `Proactive.PreferredVoiceGuid` is included (optional; if set and online, wins over lowest-guid). Lets the operator pin Claude (the paladin) as the player's default voice if desired.

**Alternatives considered**:
- *Random per session* — rejected; non-deterministic, hard to audit, flapping.
- *Round-robin* — rejected; over-engineered for a 1-3 bot fleet.

---

## R-7: Proximity / distance check

**Decision**: Use AzerothCore's existing `Unit::GetDistance(Unit const*)` (or `Player::GetDistance(Player const*)`) in `ProactiveEvaluateTick`. Threshold defaults to `Proactive.ProximityYards = 60.0`. When the leader bot is "travelling" (Activity Plan in `executing` state and last leader-bot position has been moving), check distance every heartbeat; if over threshold, transition the leader bot to "waiting" (issue `bot_stay`-equivalent stop + send one party/whisper waiting line). On distance falling back under threshold, resume.

**Rationale**:
- `GetDistance` is the canonical AC API for this; cheap; already used throughout the codebase.
- 60 yards matches the spec default (FR-007) and is the same order as existing playerbot follow ranges.
- No new movement code is needed — we're just gating an existing playerbot `stay` / `follow` based on a distance check.

**Alternatives considered**:
- *2D vs 3D distance* — use 3D for instance-floor consistency (player goes downstairs, bot is upstairs at same x,y).
- *Region/zone-only check* — rejected; misses small-radius drift inside the same zone.

---

## R-8: Audit-row structure inside `mod_ollama_chat_gateway_audit`

**Decision**: Reuse the existing insert helper in `_gateway.cpp:1498` (the `INSERT INTO mod_ollama_chat_gateway_audit ...` statement). Pass new `source_channel` values per Clarifications Q5:

| `source_channel` | Written when |
|---|---|
| `proactive_proposal` | A proposal is sent to the player |
| `proactive_approval` | The player approved an open proposal |
| `proactive_rejection` | The player rejected an open proposal |
| `proactive_nudge` | An idle nudge (or a "you coming?" proximity line) is sent |
| `proactive_complete` | An Activity Plan transitions to `completed` |
| `proactive_abort` | An Activity Plan transitions to `aborted` (timeout, blocker, cancel) |

The existing audit row's `request_text` / `response_text` columns carry the chat line text and the candidate JSON respectively, so the operator can replay the full lifecycle with `wow-ops-api`'s `ops_audit_gateway` MCP tool already filtering by `source_channel`.

**Rationale**:
- Honors Clarifications Q5 (reuse, no migration).
- Reuses existing prune path (`.ollama gateway prune` / `AuditRetentionDays`).
- The `wow-ops-api` already supports filtering by `source_channel`, so observability is automatic.

**Alternatives considered**:
- *Dedicated table* — rejected per Clarifications Q5.
- *Log-only* — rejected; spec FR-020 requires audit trail.

---

## R-9: Player chat capture for approval/rejection/done answers

**Decision**: Tap into the existing `PlayerBotChatHandler::OnPlayerCanUseChat` in `_handler.cpp`. After the existing whisper/party gates, call `ProactiveOnPlayerChat(player, message, channel)`. If an open proposal exists for that player, the classifier is invoked; on a matching intent (`proactive_approve` / `proactive_reject` / `proactive_done`), the state transition is applied and the existing handler chain is **short-circuited** so the message isn't also routed to a regular gateway response (the bot's spoken acknowledgment is the response).

**Rationale**:
- `OnPlayerCanUseChat` is already the single funnel for every channel-type player chat; the spec channel-list (FR-003: party/whisper/public-capture path) aligns exactly.
- Short-circuiting avoids the awkward case where "yes" both approves the proposal AND triggers a gateway chat response that talks about something else.

**Alternatives considered**:
- *Polling the player's recent chat in `ProactiveEvaluateTick`* — rejected. Slow, racy, would miss messages between ticks.

---

## R-10: Config-key namespace

**Decision**: New keys live under `OllamaChat.Proactive.*` (parallel to existing `OllamaChat.Tactical.*` / `OllamaChat.Strategic.*` / `OllamaChat.Gateway.*` / `OllamaChat.Mcp.*`):

| Key | Default | Purpose |
|---|---|---|
| `Proactive.Enable` | `0` | Master kill switch (spec FR-018). Default off — feature is opt-in for safety. |
| `Proactive.ProposalCooldownSec` | `120` | Min gap between consecutive proposals to the same player. |
| `Proactive.FirstProposalDelaySec` | `15` | Wait after player login before opening with the first proposal. |
| `Proactive.IdleThresholdSec` | `180` | Player stationary + no UI activity threshold for idle-nudge eligibility (spec ~3 min default). |
| `Proactive.IdleNudgeCooldownSec` | `300` | Min gap between consecutive idle nudges (spec ~5 min default). |
| `Proactive.IdleNudgeMaxPerSession` | `3` | Spec FR-011 — self-silence after N ignored nudges in one session. |
| `Proactive.ProximityYards` | `60.0` | Spec FR-007 default. |
| `Proactive.ActivityCompleteTimeoutSec` | `1800` | Spec FR-021 hard timeout (30 min default). |
| `Proactive.ProposalExpirySec` | `600` | Open proposal auto-expires if not answered (default 10 min). |
| `Proactive.PreferredVoiceGuid` | `0` | Optional designated-voice override (R-6). 0 = use lowest-guid election. |
| `Proactive.ProposerPromptFile` | `modules/mod-ollama-chat/prompts/proactive_proposer.md` | Phrasing prompt path. |

**Rationale**:
- Mirrors existing config style and naming.
- Default-off + ten ints/floats keep the config surface auditable.
- File-loaded prompt matches existing pattern (`Tactical.SystemPromptFile`, `Gateway.OllamaClassifier.SystemPromptFile`).

**Alternatives considered**:
- *Nest under `Tactical.Proactive.*`* — rejected; conceptually parallel, not subordinate, even though it rides on the tactical clock.

---

## Open questions: none.

All Phase 0 unknowns resolved against existing primitives. Phase 1 can proceed with data-model + contracts.
