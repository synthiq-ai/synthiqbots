# Phase 1 Data Model: Proactive Leader Bot

**Feature**: 002-proactive-leader-bot
**Date**: 2026-05-11

All entities below are **in-memory only** in `src/mod-ollama-chat_proactive.cpp` (no new SQL tables — per Clarifications Q5 and FR-016). Audit rows persist to the existing `mod_ollama_chat_gateway_audit` table (see `contracts/audit-events.md`).

---

## Entity: Proposal

A pending suggestion from the leader bot to a player.

| Field | Type | Notes |
|---|---|---|
| `proposalId` | `uint64_t` | Monotonic counter within the worldserver process. Resets on restart (consistent with FR-016). Used as a join key into audit rows. |
| `playerGuid` | `uint64_t` | Target player character GUID. |
| `botGuid` | `uint64_t` | Proposing (designated-voice) bot GUID. |
| `kind` | enum `quest \| dungeon \| fallbackZone` | v1 categories only (FR-002). `fallbackZone` is the rank-(4) suggestion the bot offers when ranks 1-3 are empty — phrased as "let's head to <zone>" rather than a hard quest/dungeon commit. |
| `target.questId` | `uint32_t` (when `kind=quest`) | From `acore_world.quest_template.entry`. |
| `target.title` | `std::string` (when `kind=quest`) | From `quest_template.LogTitle`. |
| `target.mapId` | `uint32_t` (when `kind=dungeon`) | WoTLK 3.3.5a map id. |
| `target.dungeonName` | `std::string` (when `kind=dungeon`) | Human-readable instance name. |
| `target.entranceX/Y/Z` | `float` (when `kind=dungeon`) | Travel target for the leader bot. |
| `target.zoneId` | `uint32_t` (when `kind=fallbackZone`) | From `AreaTable.dbc`. |
| `state` | enum `open \| approved \| rejected \| expired \| cancelled \| executing \| completed \| aborted` | Lifecycle. See state diagram below. |
| `createdAtMs` | `uint64_t` | Heartbeat-ms timestamp. |
| `answeredAtMs` | `uint64_t` | 0 until the player answers. |
| `expiryMs` | `uint64_t` | `createdAtMs + ProposalExpirySec*1000`. |
| `terminalAtMs` | `uint64_t` | 0 while the proposal is non-terminal (`open`/`approved`). Stamped lazily by the Heartbeat retention sweep (`ReapTerminalState`) the first time it observes the proposal in a terminal state, then used to drop the proposal (and its `ActivityPlan`) from the in-memory maps after the retention window below. Avoids per-transition-site bookkeeping (several terminal paths never touch `answeredAtMs`). |
| `hasProgressHint` | `bool` (when `kind=quest`) | True if the player has objectives already in progress. Phrased as "you've already got…" by the proposer prompt. |

**Invariants**:
- For a given `playerGuid`, **at most one** Proposal is in `open` state at a time. New proposals while one is open are suppressed (the C++ caller checks state first).
- Once `state ∈ {rejected, expired, cancelled, completed, aborted}`, the Proposal is **terminal** and is dropped from the in-memory table after a short retention window (kept around for ~60s only so a slow audit-write doesn't lose context). Enforced by `ReapTerminalState`, called once per `Heartbeat()` under `s_mutex` before the per-player loop: it lazily stamps `terminalAtMs` on first sight of a terminal proposal and erases it (with its one-to-one `ActivityPlan`) once `now - terminalAtMs ≥ kTerminalRetentionMs` (60s). This bounds `s_proposalsById` / `s_plansByProposalId` and keeps the per-heartbeat O(N) lookup scans (`GetOpenProposalForPlayer`, `GetExecutingPlanForPlayer`, `ElectDesignatedVoice` stickiness) from growing without limit over the worldserver's uptime.

---

## State diagram: Proposal → ActivityPlan

```text
                                                  +---------+
                                                  | expired |  (no answer within ProposalExpirySec)
                                                  +----^----+
                                                       |
+--------+         player answers           +---------+|        +-----------+
|  open  | -----------------> classify ---> | ambiguous |  ---> (stay open, retry on next tick)
+--------+                       |          +-----------+
   ^ |                           |
   | |                           |  approve              +-----------+
   | |                           +----------------------> | approved  |
   | |                           |                       +-----+-----+
   | | direct command                                          |
   | | (FR-014) or new           |  reject                     |
   | | proposal                  +-------------------> +----------+
   | |                                                 | rejected | (terminal — next tick offers a different one)
   | v                                                 +----------+
+-----------+                                              |
| cancelled | <----- player approves a different proposal -+
+-----------+
                                                       |
                                                       v  (approval triggers Activity Plan creation)
                                                  +-----------+
                                                  | executing |
                                                  +-----+-----+
                                                        |
                                          ___________________________________
                                         |              |                    |
                                         v              v                    v
                                  +-----------+   +-----------+        +-----------+
                                  | completed |   | aborted   |        | aborted   |
                                  | (signal   |   | (timeout) |        | (blocker  |
                                  |  or       |   +-----------+        |  surfaced)|
                                  |  player   |                        +-----------+
                                  |  "done")  |
                                  +-----------+
```

---

## Entity: ActivityPlan

Created when a Proposal transitions to `approved`. One-to-one with its Proposal.

| Field | Type | Notes |
|---|---|---|
| `proposalId` | `uint64_t` | Foreign key into the Proposal. |
| `state` | enum `queued \| executing \| completed \| aborted` | Mirrors Proposal terminal states for the executing/post-approval half. |
| `startedAtMs` | `uint64_t` | When state moved to `executing`. |
| `lastProgressAtMs` | `uint64_t` | Updated on player movement / quest objective / zone change — used by the timeout safety net (R-5). |
| `commandsDispatched` | `std::vector<std::string>` | Auditing record of which playerbot verbs/MCP tools were dispatched on this plan's behalf. |
| `blockerText` | `std::string` | Empty unless `state == aborted` and we surfaced a blocker (FR-015). |

---

## Entity: CadenceState (per `(playerGuid, botGuid)`)

In-memory cadence + idle-tracking state for a single bot's leader voice over a single player.

| Field | Type | Notes |
|---|---|---|
| `sessionStartedAtMs` | `uint64_t` | Set on `OnPlayerLogin` (alongside `ResetSessionStateFor`). Used by the first-proposal-delay gate (`FirstProposalDelaySec`). Distinct from `lastPlayerActivityAtMs` — the latter advances on every player action, the former is pinned at the login instant. |
| `lastProposalAtMs` | `uint64_t` | Used for `ProposalCooldownSec` gating. |
| `lastIdleNudgeAtMs` | `uint64_t` | Used for `IdleNudgeCooldownSec` gating. |
| `ignoredNudgeCount` | `uint16_t` | Increments when an idle nudge passes with no player response (movement / chat / approve). Resets on player action. Self-silences when ≥ `IdleNudgeMaxPerSession` (FR-011). |
| `consecutiveExpiredProposals` | `uint16_t` | Increments on each `open → expired` proposal transition (silent-ignore path). Resets on player chat / quest turn-in / zone change — strong engagement signals. NOT bumped on explicit `reject` / `done` (those are answers, not silence). Self-silences the propose loop when ≥ `ProposalMaxPerSession` (FR-023). 0 = unlimited. |
| `lastPlayerActivityAtMs` | `uint64_t` | Updated on player chat / movement / quest event. Used to compute "idle ≥ IdleThresholdSec". |
| `lastProximityWarnAtMs` | `uint64_t` | Prevents proximity warning re-spam (FR-007: ≥30s gap). |
| `currentProposalId` | `uint64_t` | 0 if no open proposal. |
| `inProximityWaiting` | `bool` | True when the bot has paused travel because the player fell behind. |
| `recentRejectedTargets` | `std::deque<TargetKey>` (max 3) | Last few rejected target keys across all three candidate namespaces — questId, mapId (bit 32 set), or fallback zoneId (bit 33 set); the candidate selector skips these in the *immediately next* proposal so FR-005 is satisfied (including the rank-4 soft zone-suggestion, which otherwise re-offered the same zone every cooldown). Cleared on the next successful approval or session end. |

Reset entirely on bot relog / player logout (consistent with FR-016). `sessionStartedAtMs` is *set* on login (not zeroed) so the first-proposal-delay countdown starts from a known wall-clock.

---

## Entity: DesignatedVoice (per `playerGuid`)

The single bot that currently owns proactive output for a player. Computed each heartbeat; cached only for the duration of the tick.

| Field | Type | Notes |
|---|---|---|
| `playerGuid` | `uint64_t` | Key. |
| `botGuid` | `uint64_t` | Elected bot (per R-6: `PreferredVoiceGuid` if set and eligible, else lowest-guid among `Gateway.BotGUIDs ∩ Mcp.Leader.BotGUIDs` online + same-world). |
| `electedAtMs` | `uint64_t` | For audit only. |

---

## Validation rules (mapping to FR-IDs)

- **No execution without approval** (FR-003, SC-003): the `executing` state is only ever reached via `open → approved`. Any code path that dispatches a playerbot verb / MCP call on behalf of a Proposal must `assert(proposal.state == approved or executing)`.
- **One open proposal per player** (FR-002 implicit, edge case): on `OpenNewProposal`, if an existing one is `open`, transition that one to `cancelled` first and emit `proactive_abort` with reason `superseded`.
- **One designated voice** (FR-013): every `ProactiveEvaluateTick` that wants to speak goes through `DesignatedVoice` for that player. Non-elected bots silently exit at the top of the tick.
- **Presence-gated** (FR-012, FR-018): `g_ProactiveEnable=0` or no whitelisted human online → all entities are inert; nothing is computed, nothing is emitted, nothing is audited.

---

## Storage summary

- **In-memory only**: Proposal, ActivityPlan, CadenceState, DesignatedVoice. Stored in three `std::unordered_map`s in `mod-ollama-chat_proactive.cpp` keyed respectively by `proposalId`, `(playerGuid, botGuid)`, and `playerGuid`. Guarded by a single `std::mutex` (low contention — tactical tick is the only writer outside the chat-handler/event tap-ins).
- **Persisted**: only the audit-row stream into `mod_ollama_chat_gateway_audit`. No new SQL migration.
