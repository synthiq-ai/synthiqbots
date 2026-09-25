---
description: "Tasks for feature 002-proactive-leader-bot"
---

# Tasks: Proactive Leader Bot

**Input**: Design documents from `/specs/002-proactive-leader-bot/`
**Prerequisites**: plan.md, spec.md, research.md, data-model.md, contracts/, quickstart.md

**Tests**: NOT included as unit tests. Per `plan.md` Testing field, this AzerothCore module is validated by **in-world smoke tests on game-host + audit-row inspection via `wow-ops-api`**, not by unit tests. Each user story phase ends with a concrete smoke-test verification task that exercises the spec's acceptance scenarios. One **classifier-accuracy benchmark** (T060a) targets SC-002's ≥95% target on a hand-labelled phrase set.

**Task count**: 62 tasks (T001-T060 sequential + T023a + T060a inserted per analyze remediation 2026-05-12).

**Organization**: Three user-story phases mirror spec.md P1/P2/P3.

## Format: `[ID] [P?] [Story?] Description`

- **[P]**: Can run in parallel (different files, no dependencies on incomplete tasks)
- **[Story]**: User-story phases only; setup/foundational/polish phases have no story label
- All paths are repo-relative

## Path Conventions

Single C++ AzerothCore module. All source under `src/`, prompts under `prompts/`, runtime config under `conf/`, runbook docs under `docs/`. New source-file pair: `src/mod-ollama-chat_proactive.{cpp,h}`.

---

## Phase 1: Setup (Shared Infrastructure)

**Purpose**: Wire the new component into the existing module so it builds cleanly. Pure scaffolding — no behavior yet.

- [x] T001 Create empty `src/mod-ollama-chat_proactive.h` with header guard + minimal namespace block (`namespace ollamachat::proactive`)
- [x] T002 Create empty `src/mod-ollama-chat_proactive.cpp` that `#include`s its header + `mod-ollama-chat_config.h` and compiles as a no-op translation unit
- [x] T003 Add `src/mod-ollama-chat_proactive.cpp` to `CMakeLists.txt` (or the existing glob — verify the build picks it up locally via CI/CD on a throwaway PR if uncertain)
- [x] T004 [P] Add new `OllamaChat.Proactive.*` config keys to `conf/mod_ollama_chat.conf.dist` with the defaults and comments specified in `specs/002-proactive-leader-bot/contracts/config-keys.md` (15 keys total)

**Checkpoint**: `git_pull_module` followed by CI/CD build succeeds; no behavior change yet (`Proactive.Enable` defaults to 0).

---

## Phase 2: Foundational (Blocking Prerequisites)

**Purpose**: Everything every user story needs — config plumbing, state-machine entities, audit wiring, tap-in call sites (still no-op behind the flag), prompt assets.

**⚠️ CRITICAL**: No user-story phase can begin until this is complete.

- [x] T005 [P] Declare `g_Proactive*` extern globals in `src/mod-ollama-chat_config.h` matching the 15 keys from T004 (mirror the existing `g_Tactical*` block pattern)
- [x] T006 Load the new keys in `src/mod-ollama-chat_config.cpp` `LoadOllamaConfig()` (or whatever existing function loads other module keys) — each key, default, validation against contract min/max
- [x] T007 [P] Define the four in-memory entities (`Proposal`, `ActivityPlan`, `CadenceState`, `DesignatedVoice`) as plain structs/enums inside `namespace ollamachat::proactive` in `src/mod-ollama-chat_proactive.h` per `specs/002-proactive-leader-bot/data-model.md`. **Include the `sessionStartedAtMs` field on `CadenceState`** (added 2026-05-12 per analyze remediation A4) — needed for the first-proposal-delay gate.
- [x] T008 Implement the three in-memory tables in `src/mod-ollama-chat_proactive.cpp` — `std::unordered_map`s keyed by `proposalId` / `(playerGuid, botGuid)` / `playerGuid`, guarded by one `std::mutex`. Expose getter/setter primitives only; no business logic yet.
- [x] T009 Implement audit-emit helper `proactive::EmitAudit(playerGuid, botGuid, sourceChannel, requestJson, responseText, latencyMs)` in `src/mod-ollama-chat_proactive.cpp` that inserts directly into `mod_ollama_chat_gateway_audit` (mirrors `WriteGatewayAuditRecord` in `src/mod-ollama-chat_gateway.cpp:1570`). Sets the 6 new `source_channel` values per `specs/002-proactive-leader-bot/contracts/audit-events.md`. Gated on `g_ProactiveEnableAudit` (default true) — independent of the gateway-wide `g_GatewayEnableAudit`. Corrected 2026-05-19: original design routed through `WriteGatewayAuditRecord` and inherited its gating, which silently dropped every proactive row on the default deployment config (Gateway.EnableAudit defaults to 0). See `contracts/audit-events.md#toggle-correction-2026-05-19`.
- [x] T010 [P] Implement `proactive::ElectDesignatedVoice(playerGuid) -> botGuid` in `src/mod-ollama-chat_proactive.cpp` per research R-6: `PreferredVoiceGuid` first if set + eligible, else lowest-guid among `Gateway.BotGUIDs ∩ Mcp.Leader.BotGUIDs` online + same-world. Returns 0 if none eligible.
- [x] T011 Implement the master-gate function `proactive::IsEngaged(playerGuid) -> bool` in `src/mod-ollama-chat_proactive.cpp` — returns true iff `g_ProactiveEnable && g_TacticalEnable && g_TacticalHumanPresenceRequired && playerIsWhitelistedAndOnline(playerGuid)`. Centralizes FR-012 / FR-018.
- [x] T012 Add tap-in declaration in `src/mod-ollama-chat_proactive.h` for `ProactiveEvaluateTick(uint64_t botGuid, uint64_t playerGuid)` and implement a no-op-when-disabled body in `src/mod-ollama-chat_proactive.cpp` that immediately returns if `!IsEngaged(playerGuid)` or `ElectDesignatedVoice(playerGuid) != botGuid`. Real work added in US1.
- [x] T013 Wire the tick tap-in: in `src/mod-ollama-chat_tactical.cpp` `TacticalLeaderTick::Run()`'s per-bot loop, call `proactive::ProactiveEvaluateTick(botGuid, whitelistedPlayerGuid)` once per heartbeat per (bot, player) pair. Per research R-1, this is one new line at the top of the existing per-bot body.
- [x] T014 Add tap-in declaration `ProactiveOnPlayerChat(Player* player, std::string const& message, uint32_t channelType) -> bool` in `src/mod-ollama-chat_proactive.h`. Implement no-op-when-disabled body in `src/mod-ollama-chat_proactive.cpp`. Returns `true` if the message was handled (and should be suppressed from the regular chat path), `false` otherwise. Real intent classification added in US1.
- [x] T015 Wire the chat tap-in: in `src/mod-ollama-chat_handler.cpp` `PlayerBotChatHandler::OnPlayerCanUseChat` (after existing whisper/party gates), call `proactive::ProactiveOnPlayerChat(player, msg, channelType)`; if it returns `true`, short-circuit before the regular gateway dispatch. Per research R-9.
- [x] T016 [P] Add tap-in declarations `ProactiveOnQuestComplete(Player*, uint32_t questId)` and `ProactiveOnZoneChange(Player*, uint32_t newZoneId, uint32_t newMapId)` in `src/mod-ollama-chat_proactive.h` with no-op-when-disabled bodies in `src/mod-ollama-chat_proactive.cpp`. Real completion detection added in US1.
- [x] T017 Wire the event tap-ins: in `src/mod-ollama-chat_events.cpp`, call `proactive::ProactiveOnQuestComplete(player, questId)` from the existing `OnPlayerCompleteQuest` hook (add the hook if not already registered for events) and `proactive::ProactiveOnZoneChange(player, newZone, newMap)` from `OnPlayerUpdateZone` / `OnPlayerMapChanged`.
- [x] T018 [P] Create `prompts/proactive_proposer.md` per `specs/002-proactive-leader-bot/contracts/prompt-contract.md` Prompt 1. Include all forbidden behaviors as explicit MUST/MUST NOT bullets, the seven `lineKind` cases, and 5+ example outputs.
- [ ] T019 [P] Edit `prompts/ollama_classifier.md` per `specs/002-proactive-leader-bot/contracts/prompt-contract.md` Prompt 2: add the three new intent labels (`proactive_approve` / `proactive_reject` / `proactive_done`) with example phrasings for each. Keep all existing intents untouched. (Deferred — v1 `ClassifyPlayerMessage` uses substring matching on the classifier's free-form response; label-based switching arrives in the follow-up that adds the structured-intent JSON contract.)
- [x] T020 Implement `proactive::LoadProposerPromptFromFile()` in `src/mod-ollama-chat_proactive.cpp` — read `g_ProactiveProposerPromptFile`, store in a string global, log + force `g_ProactiveEnable=false` if missing (per contract). Call from `LoadOllamaConfig()` so `.ollama reload` picks up edits.
- [x] T021 Implement `proactive::ClassifyPlayerMessage(message, openProposalId, executingPlanId) -> intent` in `src/mod-ollama-chat_proactive.cpp` that wraps `TryGatewayIntentClassify` from `src/mod-ollama-chat_gateway.cpp` and only returns one of the three new proactive labels if the C++-side state allows it (open proposal for approve/reject; executing plan for done). Otherwise returns `none` so the regular chat flow continues.
- [x] T022 Implement `proactive::PhraseLine(snapshot) -> string` in `src/mod-ollama-chat_proactive.cpp` — builds the JSON snapshot per contract, calls `QueryGatewayAPI` synchronously (or via the existing async path with a callback), returns the trimmed single line. Returns empty string on failure; caller treats empty as "skip emitting this tick."
- [x] T023 [P] Add `OnPlayerLogin` / `OnPlayerLogout` script registrations in `src/mod-ollama-chat_main.cpp` that call `proactive::ResetSessionStateFor(playerGuid)` so per-session counters (idle-nudge count, cadence state) reset on logout. On login, the same call MUST set `CadenceState.sessionStartedAtMs = now()` (per data-model.md as updated 2026-05-12) so the first-proposal-delay gate has a known wall-clock anchor. Ensures FR-011 ("per-session" semantics) and FR-016 (no persistence).
- [x] T023a Implement `proactive::EmitProactiveLine(playerGuid, botGuid, lineKind, text)` in `src/mod-ollama-chat_proactive.cpp` — the **single funnel for every proactive chat line** (proposal / approval_ack / waiting / proximity_warn / idle_nudge / complete_check / complete_ack / abort_ack). Before dispatching to the bot's say/whisper channel, this funnel MUST consult the existing tactical ambient counters from `src/mod-ollama-chat_tactical.cpp` (`Tactical.AmbientMinSpeechGapSec` per-bot speech gap, `Tactical.AmbientMaxVisibleActionsPerMinute` rolling cap) and refuse to emit when those counters are saturated — logging a `LOG_DEBUG` ("proactive emit suppressed: tactical ambient cap reached") and dropping the line. Per FR-019 (updated 2026-05-12). Every proactive emit site introduced in later phases (T033, T037, T041, T044, T046, T051) calls this funnel — direct `Whisper` / `SayToParty` calls from `_proactive.cpp` are forbidden.

**Checkpoint**: Feature builds with all tap-ins live but all behavior gated. With `Proactive.Enable=0` (default), `git diff`-equivalent runtime behavior on game-host is unchanged. Audit table is unchanged. With `Proactive.Enable=1`, the tick fires but does nothing useful yet (it elects a voice and exits).

---

## Phase 3: User Story 1 — Propose & Wait for Approval (P1) 🎯 MVP

**Story goal**: Player logs in, leader bot opens with a concrete quest-or-dungeon proposal within ≤60s, waits for explicit player approval, executes only on "yes" using existing playerbot verbs, surfaces blockers in plain language.

**Independent test** (per spec User Story 1 acceptance scenarios + SC-001/SC-002/SC-003): Log in your whitelisted player next to Claude. Within ~60s a proposal arrives in party/whisper. Reply "yes" → bot starts moving/executing within ~5s. Reply "no" → bot proposes something different next cycle. Across a 60-min session with ≥10 proposals, zero unapproved executions (audit `proactive_proposal` rows where state never reached `approved` MUST have no corresponding travel/accept dispatch).

### Implementation for User Story 1

- [x] T024 [P] [US1] Implement candidate-selection helper `proactive::SelectCandidateForPlayer(Player const* p) -> Candidate` in `src/mod-ollama-chat_proactive.cpp` per research R-4 / FR-006 / data-model `Proposal.kind`. Rank order: (1) in-log quests with progress in current zone, (2) accept-eligible quests in current zone, (3) level-appropriate dungeons with reachable entrance (filtered by `g_ProactiveDungeonCandidates` if non-empty), (4) fallback zone. Cap quest search by `g_ProactiveMaxQuestRankSearch`.
- [x] T025 [US1] Implement quest-log read (rank 1) inside `SelectCandidateForPlayer` using `Player::GetQuestSlotQuestId` / `Player::GetQuestStatus` — collect in-log quests, mark `hasProgress=true` when at least one objective has non-zero count, prefer ones whose `quest_template.ZoneOrSort` matches the player's current zone. **Phase 3-6 follow-up landed the in-zone preference** — rank-1 first scans for a same-zone match, then falls back to the first non-rejected log quest.
- [x] T026 [US1] Implement in-zone-acceptable quest enumeration (rank 2) inside `SelectCandidateForPlayer` — query `acore_world.quest_template` filtered by player level (±5), `ZoneOrSort == playerZoneId`, and prerequisite chain status via `Player::CanTakeQuest`. **Shipped 2026-05-12 Phase 3-6**: `WorldDatabase.PQuery` against `quest_template` bounded by `g_ProactiveMaxQuestRankSearch` (default 50); each candidate filtered via `Player::CanTakeQuest(q, false)` to never propose a faction/level/prereq-blocked quest.
- [x] T027 [US1] Implement dungeon enumeration (rank 3) inside `SelectCandidateForPlayer` using the pinned dungeon table per `research.md` R-4. **Shipped 2026-05-12 Phase 3-6**: 48-entry `static constexpr DungeonRow kDungeonTable[]` at the top of `_proactive.cpp` covering Vanilla + TBC + WotLK 5-mans. Filters: level (`minLevel-3 ≤ playerLvl ≤ maxLevel+5`), faction flag (0=both, 1=alliance, 2=horde), same-continent membership, and the `g_ProactiveDungeonCandidates` csv whitelist. Caveat: entrance coords are placeholder `0/0/0` because v1 dispatches `!follow` rather than travel-to-coord; a follow-up will populate them from `acore_world.areatrigger_teleport` once the dispatch path actually walks to an entrance.
- [x] T028 [US1] Implement fallback-zone candidate (rank 4) inside `SelectCandidateForPlayer` — pick the next zone whose level bracket covers the player's level for their faction.
- [x] T029 [US1] Implement Proposal-lifecycle primitives in `src/mod-ollama-chat_proactive.cpp`: `OpenProposal(playerGuid, botGuid, candidate)`, `TransitionProposal(proposalId, newState, reasonText)`, `GetOpenProposalForPlayer(playerGuid)`, `CancelOpenProposalIfAny(playerGuid, reason)`. Each terminal transition emits a `proactive_proposal` / `proactive_rejection` / etc. audit row via `EmitAudit`.
- [x] T030 [US1] Implement proposal-cooldown gate in `ProactiveEvaluateTick`: check `now - cadence.lastProposalAtMs >= g_ProactiveProposalCooldownSec * 1000` AND no open proposal AND no executing plan before opening a new one. First-proposal also gated by `g_ProactiveFirstProposalDelaySec` after `OnPlayerLogin`.
- [x] T031 [US1] Implement proposal expiry: in `ProactiveEvaluateTick`, transition any `open` proposal whose `now > expiryMs` to `expired` and emit `proactive_abort` with reason `"expired"`.
- [x] T032 [US1] Implement rejection-memory in `CadenceState`: remember the last 3 rejected candidate ids per (player, bot) and skip them in the *immediately next* `SelectCandidateForPlayer` call (FR-005). Memory clears on the next successful approval or session end.
- [x] T033 [US1] Implement proposal phrasing call in `ProactiveEvaluateTick` post-`OpenProposal`: build the `lineKind="proposal"` snapshot per `prompt-contract.md`, call `PhraseLine`, emit the resulting text on the channel chosen by `proactive::ChannelForPlayerBot(playerGuid, botGuid)` (party if same group, else whisper) — see T034. Audit-emit `proactive_proposal` with the proposal-id and chat line.
- [x] T034 [P] [US1] Implement `proactive::ChannelForPlayerBot(playerGuid, botGuid) -> enum {Party, Whisper}` in `src/mod-ollama-chat_proactive.cpp` — return `Party` iff both are alive in the same `Group*`, else `Whisper` (FR-017). Emit via the existing `bot->SayToParty(...)` / `bot->Whisper(player, ...)` paths used elsewhere in the module.
- [x] T035 [US1] Wire intent classification into `ProactiveOnPlayerChat` (T014): if `GetOpenProposalForPlayer(player)` returns a proposal, call `ClassifyPlayerMessage(msg, openId, 0)`. On `proactive_approve`: transition proposal to `approved`, emit audit `proactive_approval`, kick off Activity Plan (T036). On `proactive_reject`: transition to `rejected`, emit audit `proactive_rejection`. On ambiguous: leave state as-is, do not short-circuit. On `proactive_done` with no executing plan: ignore.
- [x] T036 [US1] Implement Activity-Plan dispatcher `proactive::StartActivityPlan(proposalId)` in `src/mod-ollama-chat_proactive.cpp`. Per research R-4 + spec FR-022: **quest** → reuse existing `leader_command_all command="accept *"` path + `bot_follow` (so the player can keep up); **dungeon** → if the player is not already in the bot's party (`player->GetGroup() != bot->GetGroup()`), dispatch the existing `bot_invite_to_group` MCP verb (per `docs/agent-context/ARCHITECTURE.md` 23-self-actions catalog) to bring the player into the leader bot's party — skip when already grouped; then reuse existing travel-via-follow to walk to the pinned entrance coords from T027. **No LFG/Random Dungeon Finder queue** — v1 only handles "specific named instance, walk in." NO new MCP verb introduced. Plan state transitions to `executing`; first dispatch failure surfaces blocker via `AbortActivityPlan(planId, blockerText)`.
- [x] T037 [US1] Implement `proactive::AbortActivityPlan(planId, blockerText)` — transitions plan to `aborted`, phrases an `abort_ack` line via `PhraseLine`, emits to the chosen channel, emits audit `proactive_abort` with reason `"blocker"` + the blocker text. Per FR-015.
- [x] T038 [US1] Implement direct-command takeover (FR-014) in `ProactiveOnPlayerChat`: if the existing handler chain detects a direct playerbot command (`follow me`, `stay`, etc.) and an open proposal exists, call `CancelOpenProposalIfAny(playerGuid, "directCommand")` BEFORE the existing chain runs. Return `false` so the regular chain still handles the actual command.
- [x] T039 [US1] Implement hybrid completion detection per Clarifications Q2 + research R-5: `ProactiveOnQuestComplete` (T016) marks any `executing` plan whose `proposalId` matches a `kind=quest` candidate with `target.questId == arrivedQuestId` as `completed`, emits `proactive_complete` with `completionSource="questTurnIn"`. `ProactiveOnZoneChange` similarly for `kind=dungeon` plans when the player leaves the instance map id — `completionSource="zoneOut"`. `proactive_done` intent (already classified in T035 — but in the "executing plan" branch) — `completionSource="playerDone"`.
- [x] T040 [US1] Implement the timeout safety net (FR-021): in `ProactiveEvaluateTick`, if a plan's `now - startedAtMs > g_ProactiveActivityCompleteTimeoutSec * 1000` and no completion arrived, send one timeout-check nudge via `PhraseLine(lineKind="complete_check")` (the `complete_check` enum value is now pinned in `contracts/prompt-contract.md` as of 2026-05-12) routed through the `EmitProactiveLine` funnel (T023a) and start a sub-timer; after `g_ProactiveActivityCompleteNudgeWaitSec` more with still no answer, transition to `aborted` with reason `"timeout"` and emit `proactive_abort` audit row.
- [x] T041 [US1] Implement bot acknowledgment lines: phrase an `approval_ack` (T033 follow-up after approval) and a `complete_ack` (after T039 completion) via `PhraseLine`, emit on the chosen channel. Both audit-rowed under the same `proactive_*` event as the state transition (per `audit-events.md` `response_text` column).
- [ ] T042 [US1] Smoke-test on game-host (per quickstart.md "Smoke-test the happy path" + "Smoke-test rejection + cooldown"): enable feature via CI/CD deploy, log in, verify first-proposal ≤60s, verify yes → executes, verify no → different next proposal, verify zero unapproved executions via `ops_audit_gateway source_channel_like='proactive_%'` query (SC-003).

**Checkpoint**: US1 is fully functional standalone. Player can have a complete propose → approve → execute → complete → propose-next loop. Bot does NOT yet stay within proximity (US2 problem) and does NOT yet idle-nudge (US3). Feature is shippable as MVP from this point.

---

## Phase 4: User Story 2 — Stay Close to the Player (P2)

**Story goal**: Once the bot starts travelling to an approved destination, it continuously checks player position; if the player falls behind beyond `ProximityYards` (default 60y), bot stops and sends one waiting line; resumes when player closes the gap; abandons travel if the player zones/changes continent. Bot never wanders far from the player when idle.

**Independent test** (per spec User Story 2 acceptance scenarios + SC-004): Approve a travel-bearing proposal. Stop following intentionally. Within ~15s the bot should stop and send one "you coming?" line. Close the gap — bot resumes. Hearth to a different continent — bot abandons travel within one heartbeat and either teleports to you or asks "where did you go?".

### Implementation for User Story 2

- [x] T043 [US2] Implement `proactive::CheckProximity(plan, player, bot) -> ProximityState` in `src/mod-ollama-chat_proactive.cpp` per research R-7 — returns `WithinRange | TooFar | DifferentZone | DifferentMap` using 3D distance and `Player::GetMapId()` / `Player::GetZoneId()`. Shipped 2026-05-12 Phase 3-6.
- [x] T044 [US2] Implement proximity-pause logic in `ProactiveEvaluateTick`: for any `executing` plan, call `CheckProximity`; on first `TooFar`, set `cadence.inProximityWaiting=true`, dispatch `!stay` via `botAI->HandleCommand`, phrase a `proximity_warn` line via the `EmitProactiveLine` funnel, emit audit `proact_nudge` with `"proximity"`. Re-spam suppressed by `g_ProactiveProximityWarnCooldownSec`. Shipped 2026-05-12 Phase 3-6.
- [x] T045 [US2] Implement proximity-resume: when state moves from `TooFar` back to `WithinRange` and `cadence.inProximityWaiting==true`, dispatch `!follow` to resume travel, set `inProximityWaiting=false`. No chat line on resume. Shipped 2026-05-12 Phase 3-6.
- [x] T046 [US2] Implement zone/continent abandon: on `DifferentZone` or `DifferentMap`, call `AbortActivityPlan` with `"player changed zone/map"`, emit audit `proact_abort` with `"playerChangedZone"`. Next heartbeat opens a fresh proposal. Shipped 2026-05-12 Phase 3-6.
- [x] T047 [US2] Implement idle-anchor (FR-008): in the no-executing-plan branch of `EvaluateTickLocked`, if `CheckProximity` returns `TooFar`, dispatch `!follow` silently. Shipped 2026-05-12 Phase 3-6.
- [ ] T048 [US2] Smoke-test on game-host per quickstart.md proximity scenarios: confirm bot stops within ~10-15s when you fall behind; resumes silently when you close the gap; abandons travel cleanly on a hearth to another continent; stays anchored when idle (SC-004). **In-world test — requires player to log in.**

**Checkpoint**: US1 + US2 work together. Proposals/approvals/execution still work; bot now respects proximity throughout.

---

## Phase 5: User Story 3 — Idle Nudge (P3)

**Story goal**: When the player is idle (no movement, no chat, no UI interaction, no open proposal, no executing plan) for at least `IdleThresholdSec`, the leader bot sends one short nudge. Bounded by `IdleNudgeCooldownSec`, capped at `IdleNudgeMaxPerSession`, self-silences after that, and suppresses while in combat / vendor / quest dialog / actively typing (where detectable).

**Independent test** (per spec User Story 3 acceptance scenarios + SC-006): Log in, stand still for `IdleThresholdSec`, verify exactly one nudge fires. Stand still longer — verify nudges respect cooldown. Stand still for hours — verify nudges stop after `IdleNudgeMaxPerSession`. Engage in combat or open a vendor — verify no nudge fires.

### Implementation for User Story 3

- [x] T049 [US3] Implement player-activity tracking in `src/mod-ollama-chat_proactive.cpp`: `cadence.lastPlayerActivityAtMs` is updated on (a) chat (from public `OnPlayerChat`), (b) movement (via `NoteMovementIfAny` sampling position diff in `EvaluateTickLocked`), (c) quest-complete (public `OnQuestComplete`), (d) zone change (public `OnZoneChange`). Shipped 2026-05-12 Phase 3-6.
- [x] T050 [US3] Implement idle-eligibility check `IsPlayerIdleForNudge(player, cadence, now)` — true iff idle threshold reached, cooldown elapsed, under per-session cap, no open proposal, no executing plan, and `!player->IsInCombat()`. UI-suppression (vendor/quest-dialog/mail) is skipped in v1 with a noted gap. Shipped 2026-05-12 Phase 3-6.
- [x] T051 [US3] Implement idle-nudge emission in `EvaluateTickLocked`: when `IsPlayerIdleForNudge` returns true, phrase `idle_nudge` via `PhraseLine`, emit via the `EmitProactiveLine` funnel, set `lastIdleNudgeAtMs`, increment `ignoredNudgeCount`, emit audit `proact_nudge` with `"idle"`. Shipped 2026-05-12 Phase 3-6.
- [x] T052 [US3] Implement ignored-counter reset: chat, movement, quest-complete, and zone-change all reset `ignoredNudgeCount` to 0. Shipped 2026-05-12 Phase 3-6.
- [x] T053 [US3] Implement self-silence (FR-011): `IsPlayerIdleForNudge` returns false once `ignoredNudgeCount >= g_ProactiveIdleNudgeMaxPerSession`. Counter resets on `OnPlayerLogin` / `OnPlayerLogout` via `ResetSessionStateFor`. Shipped 2026-05-12 Phase 3-6.
- [ ] T054 [US3] Smoke-test on game-host per quickstart.md "Smoke-test idle nudge" scenarios: verify first nudge fires ~3 min after standing still; verify cooldown holds; verify session caps at 3 nudges then goes silent; verify no nudge fires while in combat (SC-006). **In-world test — requires player to log in.**

**Checkpoint**: All three user stories work together. SC-001…SC-008 should now all hold in a representative play session.

---

## Phase 6: Polish & Cross-Cutting Concerns

**Purpose**: Documentation, changelog, light cleanup. No new behavior.

- [x] T055 [P] Write `docs/proactive-leader.md` runbook based on `specs/002-proactive-leader-bot/quickstart.md` (operator audience). Shipped in PR #113 (initial MVP scope); 2026-05-12 Phase 3-6 update folds the now-shipped follow-ups into the "What it does today" section and re-categorizes the "Not yet shipped" list.
- [x] T056 [P] Add `docs/CHANGELOG.md` entry. Initial MVP entry shipped in PR #112; 2026-05-12 Phase 3-6 update adds a second entry covering the dungeon table + in-zone quest scan + proximity + idle nudge + generative phrasing + classifier bench.
- [x] T057 [P] Add a one-line row to the source-layout table in `docs/agent-context/ARCHITECTURE.md` describing `_proactive.cpp/.h`. Shipped 2026-05-12 Phase 3-6.
- [x] T058 [P] Config-key comments in `conf/mod_ollama_chat.conf.dist` are operator-grade per the original Phase 1 task (T004). Verified 2026-05-12 — no changes needed.
- [ ] T059 Run the full quickstart.md validation pass on game-host end-to-end after a clean CI/CD deploy: happy path, rejection cooldown, idle nudge, **explicit kill-switch toggle (`Proactive.Enable=1` → `.ollama reload` → live behavior → `Proactive.Enable=0` → `.ollama reload` → proactive lines stop within ≤5s, reactive chat unchanged)** per SC-008. Capture before/after audit-row counts so the changelog entry can quote a real "added ~30 rows per 2h session" data point. **In-world test — requires player to log in.**
- [x] T060 Final code review pass on `src/mod-ollama-chat_proactive.cpp`. **Results 2026-05-12 Phase 3-6**: (a) every state transition emits exactly one audit row — verified by grep ✓; (b) every `HandleCommand` dispatch is gated by `state == Executing` OR is the FR-008 idle-anchor `!follow` (allowed by spec) ✓; (c) lock scope: see `PhraseLine` doc-comment — generative path can block heartbeat 1-5s; documented as known limitation, not a v1 blocker ✗ (acceptable for v1 single-player game-host); (d) `Proactive.Enable=0` truly bypasses every code path — verified at `Heartbeat()` entry + `IsEngaged` check on every public tap-in ✓; (e) every chat line goes through `EmitProactiveLine`, no direct `Whisper` / `SayToParty` calls ✓.
- [x] T060a [P] Create `tests/proactive/classifier_phrases.json` (188 hand-labelled phrases — 52 approve / 51 reject / 51 done / 24 directcmd / 10 none) + `bench_classifier.py` mirroring the C++ string-match rules + `bench_classifier.sh` wrapper. **Result 2026-05-12 Phase 3-6**: 100% per-class accuracy (188/188), comfortably exceeding the SC-002 ≥95% target. Rerunnable via `bash tests/proactive/bench_classifier.sh`. Replaces the original Ollama-based bench design since v1 implements C++-side string matching rather than label-based LLM classification.

---

## Dependencies & Execution Order

### Phase dependencies

- **Phase 1 (Setup)**: T001 → T002 → T003 sequential (file must exist before CMake picks it up). T004 [P] parallel with all of them.
- **Phase 2 (Foundational)**: requires Phase 1 complete. Heavy parallelism inside: T005/T007/T010/T016/T018/T019/T023 can all run in parallel; T006 needs T005; T008 needs T007; T009/T011/T012/T014 need T005-T008; T013/T015/T017 each touch a separate existing file and can land in parallel after their tap-in declaration is in T012/T014/T016; T020 needs T018; T021 needs T019; T022 needs T020. T023a depends on T009 (audit emit), T022 (PhraseLine), and on knowing the existing tactical ambient-counter API — it's the last Phase-2 task before US1 can start.
- **Phase 3 (US1)**: requires Phase 2 complete. T024-T028 (candidate selector pieces) can parallelize (T024 wraps the others). T029-T032 are sequential lifecycle. T033-T041 mostly sequential because they each add a new behavioral branch in the same file. T042 (smoke test) is last.
- **Phase 4 (US2)**: requires Phase 3 complete (uses ActivityPlan state from US1). T043-T047 mostly sequential. T048 (smoke test) last.
- **Phase 5 (US3)**: requires Phase 4 complete (idle nudge is suppressed when an open proposal or executing plan exists — both US1 features). T049-T053 sequential. T054 (smoke test) last.
- **Phase 6 (Polish)**: requires Phase 3/4/5 complete. T055-T058 and T060a fully parallel. T059 and T060 sequential at the end. T060a (classifier accuracy bench) is independent of T055-T060 and can land in parallel with the docs work.

### Within-story dependencies

- US1: candidate selector → proposal lifecycle → phrasing → intent routing → execution dispatch → completion detection → ack lines → smoke test.
- US2: proximity check → pause logic → resume logic → zone-abandon → idle-anchor → smoke test.
- US3: activity tracking → eligibility check → emission → reset → self-silence → smoke test.

### Parallel opportunities

- All `[P]`-marked tasks across the entire file.
- Phase 6 polish is almost entirely parallelizable.
- After Phase 2 ships, the three user stories are mostly independent at the file level (each touches a separate behavior branch in the same `_proactive.cpp`) — a second agent could pick up US2 or US3 in parallel with US1's smoke test if needed, though sequential P1 → P2 → P3 is the natural delivery order.

---

## Parallel Example: Phase 2 Foundational

```bash
# After T001-T004 land, these can be split across two parallel sessions:
Task T005: declare g_Proactive* externs in src/mod-ollama-chat_config.h
Task T007: define Proposal/ActivityPlan/CadenceState/DesignatedVoice structs in src/mod-ollama-chat_proactive.h
Task T010: implement ElectDesignatedVoice() in src/mod-ollama-chat_proactive.cpp
Task T016: declare ProactiveOnQuestComplete/ProactiveOnZoneChange in src/mod-ollama-chat_proactive.h
Task T018: write prompts/proactive_proposer.md
Task T019: edit prompts/ollama_classifier.md
Task T023: register OnPlayerLogin/OnPlayerLogout in src/mod-ollama-chat_main.cpp
```

---

## Implementation Strategy

### MVP First (User Story 1 only)

1. Complete Phase 1 (Setup) — T001-T004
2. Complete Phase 2 (Foundational) — T005-T023
3. Complete Phase 3 (US1) — T024-T042
4. **STOP and VALIDATE**: run the US1 smoke test (T042) end-to-end on game-host. If happy, ship as v1-MVP. The user explicitly said proactivity is the inversion they want — propose-and-wait alone meaningfully changes the play feel and is the headline value.

### Incremental delivery

1. Phase 1 + 2 → foundation ready (flag is off; runtime behavior unchanged)
2. + Phase 3 (US1) → propose/approve/execute working. **Ship.**
3. + Phase 4 (US2) → proximity guardrails added. **Ship.**
4. + Phase 5 (US3) → idle nudge added. **Ship.**
5. + Phase 6 (Polish) → docs and final audit.

Each ship is a `merge to main` → CI/CD deploys to game-host. The kill switch (`Proactive.Enable=0`) lets the operator roll back any of these without rebuilding.

### Parallel team strategy

Single-developer project; parallelism is "two Claude sessions in two worktree slots" not a head-count thing. After Phase 2 completes:

- Slot 1: T024-T042 (US1)
- Slot 2 *(only if Slot 1 is paused on game-host smoke-testing)*: T055-T058 (Polish docs that don't depend on the implementation details landing first)

---

## Notes

- `[P]` tasks = different files OR same-file but additive (e.g. independent enum entries in `_config.h`), no dependencies on incomplete tasks
- All paths repo-relative; absolute paths only inside CI/CD scripts and ssh game-host commands (not authored here)
- `Proactive.Enable=0` default means every PR up through Phase 5 is shippable to game-host without changing observable behavior for the user — opt-in via config edit + `.ollama reload`
- Stop at each smoke-test task and validate against the corresponding spec acceptance scenarios before opening the next phase's PR
- Per project's `CLAUDE.md`: no manual deploys; every merged PR auto-deploys via the GHA `deploy.yml` workflow on push to `main`
