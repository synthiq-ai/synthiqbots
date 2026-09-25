# Contract: Audit events in `mod_ollama_chat_gateway_audit`

**Feature**: 002-proactive-leader-bot
**Target table**: `acore_characters.mod_ollama_chat_gateway_audit` (existing — **no schema change**)
**Insertion path**: existing `WriteGatewayAuditRecord(...)` helper in `src/mod-ollama-chat_gateway.cpp:1489`

This contract pins the exact `source_channel` values the feature uses, so the operator can replay the lifecycle via existing observability tools (`.ollama gateway prune`, `wow-ops-api`'s `ops_audit_gateway` MCP tool, and direct SQL queries on game-host).

---

## Schema reality check (correction 2026-05-12)

The actual `mod_ollama_chat_gateway_audit` schema is leaner than the earlier contract assumed:

| Column | Type | Notes |
|---|---|---|
| `id` | BIGINT UNSIGNED auto-inc | PK |
| `bot_guid` | BIGINT UNSIGNED | |
| `player_guid` | BIGINT UNSIGNED | |
| `account_id` | INT UNSIGNED | |
| `ts` | DATETIME | NOW() at insert |
| `request_chars` | INT UNSIGNED | length of request text in chars (NOT the text itself) |
| `response_chars` | INT UNSIGNED | length of response text in chars (NOT the text itself) |
| `prompt_tokens` | INT UNSIGNED | |
| `completion_tokens` | INT UNSIGNED | |
| `latency_ms` | INT UNSIGNED | |
| `source_channel` | **VARCHAR(16)** | label string; **MUST be ≤15 chars** (1-char safety margin — the longest implemented label, `proact_tickgate`, is 15; matches the `_proactive.cpp` static_assert) |
| `error` | TINYINT(1) | 1 if error |

**No `request_text` / `response_text` columns exist**. Verbatim chat lines and JSON snapshots are NOT persisted in the audit table — they go to `worldserver.log` via `LOG_INFO("server.loading", ...)` instead, which the operator can grep / tail.

For replayability:
- "Did the bot send proposal X at time T?" — answered by the audit row's timestamp + `source_channel` + char count.
- "What text did the bot actually send?" — read from `worldserver.log` filtered by `[Ollama Chat Proactive]`.

---

## `source_channel` values (final, ≤15 chars per VARCHAR(16))

| Value | Emitted when |
|---|---|
| `proact_propose` | A proposal is sent to the player. |
| `proact_approve` | The player approved an open proposal. |
| `proact_reject` | The player rejected an open proposal. |
| `proact_nudge` | An idle nudge OR a proximity "you coming?" line OR a complete-check timeout nudge OR a level-up congratulation line is sent. The differentiation is not in `source_channel` — like `proact_abort` / `proact_novoice`, the reason is carried by `request_chars` (the schema stores no text): `4` = `idle` (silence past `IdleThresholdSec`), `7` = `levelUp` (ambient level-up congratulation, deterministic phrasing only), `9` = `proximity` (the "you coming?" line while an approved plan is executing), `14` = `complete_check` (the FR-021 completion-deadline nudge sent before the plan is aborted as `activityTimeout`). |
| `proact_done` | An Activity Plan transitions to `completed`. Three distinct completion paths, again carried by `request_chars`: `7` = `zoneOut` (the player left the approved instance map — the dungeon-run completion signal), `10` = `playerDone` (the player said "done" / "we're done" mid-activity and the classifier resolved it to Done), `11` = `questTurnIn` (the approved quest was auto-detected as turned in). Note `zoneOut` fires only after the player was seen to actually ENTER the instance; a hearth/portal away before entry is a `proact_abort` `playerChangedMap`, not a completion. |
| `proact_abort` | An Activity Plan transitions to `aborted`, OR an open Proposal is expired/cancelled/superseded. Includes the FR-013 owner-left-range handoff (`ownerLeftRange`): an executing plan whose owning bot leaves the player's range mid-activity is aborted and attributed to that owner, not to the bot newly elected as voice. Session-end teardown also aborts in-flight proposals/plans: `logout` when the **player** logs out, `botLogout` when the **owning leader bot** logs out (both attributed to the owning bot, `latency_ms` 0). Like `proact_novoice`, the reason is carried by `request_chars` (the schema stores no text): `6` = `logout`, `7` = `expired` (an open Proposal nobody answered before its 10-min expiry), `9` = `botLogout`, `13` = `directCommand`, `14` = `ownerLeftRange`, `15` = `activityTimeout` (an EXECUTING plan blew the FR-021 `ActivityCompleteTimeoutSec` deadline and then ignored the complete-check nudge), `16` = `playerChangedMap`, `17` = `playerChangedZone`. |
| `proact_planner` | Phase B gateway planner LLM round — agent emitted a `wait` veto or a `propose` line with phrasing. One row per planner call (success or empty result). |
| `proact_intent` | Phase A local-Ollama intent classifier was consulted while a proposal/plan was open. One row per non-empty classifier response, regardless of whether the label was applied (low-confidence falls through to the C++ matcher). |
| `proact_novoice` | The heartbeat had to SKIP a whitelisted-online player because `ElectDesignatedVoice` returned 0 — no leader-capable bot was eligible to speak. Diagnostic only: no chat line is sent and no LLM call is made. The reason is carried by `request_chars` (the schema stores no text): `19` = `noLeadersConfigured` (`Mcp.Leader.BotGUIDs` unset), `16` = `noLeadersInWorld` (all configured leaders offline), `13` = `noLeaderOnMap` (leaders online but none shares the player's map — the FR-013 eligibility rule), `8` = `selfOnly` (the target IS the only leader-capable bot; see the self-exclusion guard). Throttled to once per `Proactive.NoVoiceAuditCooldownSec` per player, gated by `Proactive.NoVoiceAuditEnable`. |
| `proact_tickgate` | Phase C local-Ollama "speak-now" tick gate was consulted before the paid planner (gated by `ProactiveTickGate.Enable`, default OFF; throttled to once per `IntervalSec` per (bot, player)). One row per non-empty gate response — empty / timeout / unreachable responses emit no row, mirroring `proact_intent`. The `propose`/`wait` verdict is in the worldserver log, not in `source_channel`. |

Per emit, the C++ side passes:
- `request_chars` = the length of whatever string the emit site passed as `requestText`, which is **not** the same kind of thing on every channel. Three groups:
  - **Reason-label channels** (`proact_nudge`, `proact_done`, `proact_abort`, `proact_novoice`) — the requestText IS the reason label, so `request_chars` is the reason DECODER documented per-channel above. This is the only discriminator these rows have: they carry `response_chars` 0 (or the chat line, for nudges), `latency_ms` 0, and in the common voice==owner case the same `bot_guid`.
  - **Snapshot / message channels** (`proact_planner`, `proact_tickgate`, `proact_intent`, `proact_approve`, `proact_reject`) — the length of the contextual snapshot (planner / tick-gate) or of the player's message (intent / approve / reject) that drove the event.
  - **`proact_propose`** — a fixed literal `open`, so `request_chars` is **always 4** and carries no information. Do not read a proposal's snapshot size off it; the proposal's phrased line length is in `response_chars`.
- `response_chars` = length of the chat line emitted (0 when no chat line).
- `latency_ms` = milliseconds spent inside the LLM call (classifier or proposer); 0 for purely state-machine transitions.
- `error` = 1 if a path failed (currently never set in v1; reserved).

**The reason decoders above are a compile-time invariant, not a convention.** Each multi-reason channel owns an array in `src/mod-ollama-chat_proactive.cpp` (`kAbortReasons`, `kNudgeReasons`, `kDoneReasons`, `kNoVoiceReasons`) built from named `kRsn*` constants, and a `static_assert(ReasonLengthsDistinct(...))` per array fails the build if two reasons on one channel ever share a length. Adding a reason therefore means: add the `kRsn*` constant, add it to its channel's array, and add the row to that channel's decoder here. A reason emitted on a channel whose array does not list it is outside the guard — the assert can only see what the array names.

---

## Column semantics

| Column (existing) | Value for these new events |
|---|---|
| `id` | auto-inc PK (existing). |
| `ts` | row insertion time (existing). |
| `account_id` | account that owns `playerGuid`. |
| `player_guid` | the target player. |
| `bot_guid` | the proposing / nudging / acknowledging leader-voice bot. **Exception:** `proact_novoice` carries `0` — that row exists precisely because no bot could be elected, so there is no bot to attribute it to. For a Proposal/Plan **terminal** row (`proact_abort` `expired` / `ownerLeftRange`, `proact_done`) this is the OWNING bot recorded on the Proposal — which equals the elected voice in the common case (FR-013 stickiness pins voice == owner while the owner stays in range) but stays the owner, not the newly-elected voice, on an owner-left-range handoff, so per-bot propose-vs-terminal reconciliation lines up. |
| `source_channel` | one of the ten `proact_*` values above. |
| `request_chars` / `response_chars` | character lengths of the snapshot/message and the emitted chat line. Verbatim text lives in `worldserver.log` only. |
| `latency_ms` | for `proact_planner` / `proact_intent` / `proact_tickgate`: time spent in the LLM call (phrasing, classification, or gate verdict). For `proact_propose` / `proact_nudge`: time spent in the **proposer** gateway call that phrased the line (per L54) — the measured ms when the generative path ran, `0` when the deterministic phrasing path produced the line, and `0` when the planner already supplied the line (that call's time is recorded on its own `proact_planner` row, not double-counted here). For pure state transitions (`proact_done`, `proact_abort` with `expired`/`activityTimeout`/`playerChangedZone`/`playerChangedMap`/`ownerLeftRange`/`logout`/`botLogout`/`directCommand`): 0. |
| `prompt_tokens` / `completion_tokens` | gateway-reported usage when a paid call ran; 0 for local-Ollama events (`proact_intent`, `proact_tickgate`). |
| `error` | reserved; currently never set in v1. |

---

## Querying

Read recent proactive activity for one player:

```sql
SELECT id, ts, source_channel, response_chars
FROM   mod_ollama_chat_gateway_audit
WHERE  player_guid = <playerGuid>
  AND  source_channel LIKE 'proact\\_%'
ORDER  BY id DESC
LIMIT  100;
```

Via MCP (operator workflow):

```text
ops_audit_gateway { player_guid: <playerGuid>, source_channel_like: 'proact_%', limit: 100 }
```

The existing `wow-admin` `ops_audit_gateway` tool already supports `source_channel_like` as a filter param, so no new MCP tool is required (per spec assumption — reuse only).

---

## Retention

Existing `AuditRetentionDays` + `.ollama gateway prune` apply unchanged. No separate retention policy for proactive rows.

---

## Toggle (correction 2026-05-19)

Proactive audit emission is gated on its **own** config knob — `OllamaChat.Proactive.EnableAudit` (default `1`) — **not** on the gateway-wide `OllamaChat.Gateway.EnableAudit`.

The initial T009 wording in `tasks.md` said *"No-op when `g_GatewayEnableAudit=false`"*, which left the proactive audit loop dependent on a toggle that defaults to `0` and is commonly kept off on cost-sensitive deployments (every gateway call writes a row when the gateway-wide toggle is on, which is expensive for high-traffic servers). The wow-proactive-improve cron requires `proact_*` rows to drive its bug-hunting loop, so proactive now writes the audit row directly (mirroring `WriteGatewayAuditRecord`'s INSERT) when its own toggle is on. The two toggles are independent; either or both may be set.

Operator note: setting `OllamaChat.Proactive.EnableAudit = 0` disables only the ten `proact_*` source-channel rows. All other gateway-call rows are still controlled by `OllamaChat.Gateway.EnableAudit`.

---

## Cardinality budget (informative)

For one player playing for a 2-hour session with the recommended defaults:

| `source_channel` | Expected count over 2h |
|---|---|
| `proact_propose` | 5-15 (one per completed/abandoned activity, capped by `ProposalCooldownSec=120`) |
| `proact_approve` | 0.5-1× the proposal count |
| `proact_reject` | the rest of the proposal count |
| `proact_nudge` | 0-3 idle/proximity/complete-check nudges (capped by `IdleNudgeMaxPerSession`), plus 0-few level-up congratulations (one per level gained, cooldown-guarded by `LevelUpCongratsCooldownSec=60`) |
| `proact_done` | matches approval count minus aborts |
| `proact_abort` | small — superseded proposals + timeouts |
| `proact_planner` | up to 1 per proposal attempt (gated by `ProactivePlanner.Enable`, paid gateway call). |
| `proact_intent` | up to 1 per player message during AWAITING_RESPONSE / EXECUTING (gated by `ProactiveIntent.Enable`, local Ollama — no token cost). |
| `proact_tickgate` | 0 when disabled (`ProactiveTickGate.Enable` default OFF); when enabled, ≤1 per `IntervalSec` (=30s) per player while idle and eligible to propose — bounded well under the propose rate by the throttle (local Ollama — no token cost). |

Total: ~20-50 rows / 2h / player. Well under the existing audit-write rate from the rest of the gateway path.
