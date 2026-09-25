# Changelog

All notable changes to mod-ollama-chat's user-visible surface (config keys, GM
commands, DB schema, new features). Dates in `Asia/Vladivostok` (VLAT).

Format inspired by [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

---

## [Unreleased] — 2026-09-25 (VLAT) — grouped MCP calls reach the bot they name

### Fixed

- **Every grouped (facade) MCP call ran on the fleet leader.** `tools/call` read `botGuid` /
  `playerGuid` only from the top level of `arguments`, but the facade schema advertises just
  `action` / `params` / `describe`, so the model nests them in `params` — and the single-leader
  fallback silently picked Claude. "Raz, follow me" moved Claude; `player` logged as 0. Identity
  is now also read from `params` (top level still wins).

## [Unreleased] — 2026-09-24 (VLAT) — bots follow the human in their party; GM + master tools

### Fixed

- **Fleet bots ignored the operator in a shared party.** `Fleet.EnsureParty` always made the
  leader self-mastered and the members leader-mastered, so after Slayo partied with Claude + Raz
  neither moved on "follow", and every 20 s the pass re-mastered them away. Now: when a
  whitelisted human shares the fleet leader's group, that human is every fleet bot's master
  (and free fleet members are pulled into that group); when the human leaves, the agent-led
  fleet is restored.
- **`bot_invite_to_group` reported ok and sent nothing** from a self-mastered leader (the
  playerbots `invite` text command). For a whitelisted human it now adds them to the bot's group
  directly (creating the group if needed); other targets keep the playerbots invite.
- **The model had the player's guid but not their name**, so name-taking tools guessed: "invite
  me" invited Raz and the reply called the player "Raz". The identity block now carries both
  names (from `CharacterCache`, never a `Player*` on the worker thread).

### Added

- World-thread task queue (`worldtask.{h,cpp}`, drained from the world `OnUpdate`): every new
  mutation below runs on the world thread; the MCP listener only waits for the result.
- `bot_set_master` (movement) — point a bot's master at a named online player.
- `admin_group_join` (admin) — put an online character into another's group (`name=Slayo,
  group_of=Claude`); the console `group join` is `Console::No` on this core.
- `admin_player_gps` (admin, read) — map/zone/area/coords, group members and playerbots master.
- `admin_gm_command` (admin) — one worldserver console command via the core CLI queue, output
  returned. Whole-word prefix allowlist `OllamaChat.Mcp.Gm.AllowedCommands` (default
  `lookup,pinfo,tele name,server info` — `tele name` does not admit `tele del`).
- All four respect `OllamaChat.Mcp.AllowActionTools` (except the read-only gps).

---

## [Unreleased] — 2026-09-24 (VLAT) — jev playbook filter no longer stalls on large bodies

### Fixed

- **The playbook filter failed intermittently after ~9.4 s and fell back to the full 197 KB
  playbook.** Root cause (the #455 diagnosis was wrong): from game-host's direct ISP route, POST
  bodies above ~21 KB to `api.typesafe.ai` never get an answer (17/21 KB fine, 25/29 KB stall;
  fine from the Mac and via the gluetun egress; not MSS; gzip rejected). The 78-row question set
  is ~26 KB. `FilterLeaderPlaybook` now sends the rows as ≤16 KB chunks, at most 2 in parallel,
  and merges the answers — same wall-clock as one call.

---

## [Unreleased] — 2026-09-24 (VLAT) — jev calls fail fast inside their budget

### Fixed

- **A jev call on a flapping network path took ~9.4 s against a 4 s budget.** httplib bounds
  the TCP connect wait and each TLS-handshake wait with the *connection* timeout, not
  with `set_max_timeout`; jev used 3000 ms there. In a burst of game-host→AWS flaps (8 of ~20
  playbook calls on 2026-09-24) each failure cost ~9.4 s and fell back to sending the whole
  197 KB playbook. The connection timeout is now `min(budget, clamp(budget/4, 300, 1000))` ms — healthy
  connect and TLS measure ~0.3 s each. Reproduced with a standalone client against a
  blackholed address: connect phase 3003 ms before, 1001 ms after.

---

## [Unreleased] — 2026-09-23 (VLAT) — jev pool: expire idle connections, never tear down on the caller's thread

### Fixed

- **A pooled jev connection idle ~25 s was dead and cost a whole budget.** Once #450
  made keep-alive real, the second of two `talk_to_leader` turns 25 s apart hit
  `site=playbook transport: Failed to read connection after 8037ms` and fell back to
  the full playbook (15.7 s turn). The path from game-host silently drops idle flows, so
  httplib's liveness check can't see it. Pooled clients idle > 12 s are now discarded
  and reconnected (~0.6 s TLS); 12 s sits above tactical's 10 s tick, so a running
  game keeps one hot connection.
- **Discarded clients are destroyed on a detached thread.** Their graceful TLS
  shutdown blocks up to the read timeout; that no longer lands on the world, tick or
  chat-worker thread — including the old pool's idle clients on `config_reload`.

---

## [Unreleased] — 2026-09-23 (VLAT) — jev calls no longer stall for their whole budget

### Fixed

- **Every jev call blocked its thread for its full timeout after the answer arrived.**
  `LeaseGuard`'s destructor passed `keep && cli != nullptr` alongside
  `std::move(cli)`; argument evaluation order is unspecified and clang (the live
  compiler) moves first, so `keep` was always false. The pooled keep-alive client
  was destroyed on every call, and its graceful TLS shutdown waited for the peer's
  close_notify up to the socket read timeout — the call's budget. Measured on a
  `talk_to_leader` turn: jev playbook answer at +0.82 s, gateway request at +4.8 s
  (`Jev.Playbook.TimeoutMs` 4000). Same stall on every site: intent 1.7 s,
  tick-gate 1.8 s, classifier 2.0 s (the terse-order chat path), tactical 2.5 s.
  Keep-alive reuse now actually happens, so later calls also skip the TLS handshake.

---

## [Unreleased] — 2026-09-21 (VLAT) — human presence excludes the fleet (PR 5)

### Fixed

- **"A whitelisted human is online" was an account test, and the fleet is on the whitelisted
  account.** Botmaster (20010, a null-socket Overlord session with no `PlayerbotAI`) satisfied
  tactical's presence gate, and proactive's per-player loop had no bot exclusion at all — so with
  `HumanPresenceRequired=1` and nobody playing, tactical ticked (221 in 25 min) and the leader
  composed Sonnet proposals *for Raz*. One shared predicate now, `IsWhitelistedHumanPlayer`
  (tactical.h): `!sess->IsBot()`, no `PlayerbotAI`, not `Mcp.Leader.SystemMasterGuid` /
  `Fleet.LeaderGuid` / `Fleet.MemberGuids`, then the account whitelist. Used by tactical's
  `IsAnyWhitelistedHumanOnline` / say-range / snapshot-human checks and by proactive's
  `IsWhitelistedHumanOnline` + heartbeat enumeration. Empty-whitelist semantics unchanged
  (tactical: any real human; proactive: nobody, FR-012). Side effect: a character logged in
  through the Overlord (`.playerbots bot add`) no longer counts as the human for proofs — log in
  with a real client.

---

## [Unreleased] — 2026-09-21 (VLAT) — chat interceptor: jev-only, terse orders only (PR 4)

### Changed

- **Operator decision (2026-09-21):** when a human talks to a bot (whisper/party), only *terse
  imperative* lines are intercepted by the classifier fast path; everything else reaches the
  in-character gateway (Sonnet, tools included). `TryGatewayIntentClassify` now has two
  independent tiers — jev (`Jev.Classifier.Enable`) and the LLM classifier
  (`Gateway.OllamaClassifier.Enable`) — either alone is a valid interceptor; previously
  `OllamaClassifier.Enable=0` disabled the jev tier too. New
  `OllamaChat.Gateway.Classifier.MaxWords` (default 0 = off; live 3): lines with more words skip
  both tiers. `Jev.Classifier.MinConfidence` → **0.8** (conf.dist had 0.6, the missing-key default 0.7).
- **jev per-site caps raised to fit the measured network floor** (live 2026-09-21: 1.25–1.6 s per
  call from the worldserver, not the 0.96 s a bare curl showed): intent 1000 → 1700 ms, tick-gate
  1200 → 1800 ms, classifier 1500 → 2000 ms. With the old caps tick-gate timed out on *every*
  heartbeat and its timeouts opened the shared breaker for all sites every ~30 s. Live
  `Jev.TimeoutMs=2500` (tactical: 20/20 ticks on `backend='jev'` afterwards); tick-gate + intent
  jev sites are OFF live until this deploys.
- Live config after this lands: `OllamaClassifier.Enable=0`, `Jev.Classifier.Enable=1`,
  `Jev.Classifier.MinConfidence=0.8`, `Classifier.MaxWords=3` — "attack" / "follow me" / "everyone
  hearth" stay on the ~1 s jev path; "share your quest with the party", "put a cross on it" and
  lines > 3 words go to the bot's brain (intended policy; ≤ 3-word conversation still depends on
  jev answering `unknown` / below 0.8, which the bench shows it does for the 9 such lines).

---

## [Unreleased] — 2026-09-21 (VLAT) — jev leader-playbook relevance filter (PR 3)

### Added

- **`Jev::FilterLeaderPlaybook`** (`src/mod-ollama-chat_jev.cpp`; pure parts `ParsePlaybook` /
  `SelectPlaybookRows` / `RenderPlaybook` in `_jev_core.h`) — the 197 KB, 78-row leader playbook
  (`Mcp.Leader.SystemPromptFile`) that rides in every leader gateway request is filtered per human
  message: one jev request with one `noul` per row (shared framing once in the state), rows with
  P(relevant) ≥ `MinRelevance` kept (≥ `MinRows`, ≤ `MaxRows`, file order), header + footer always.
  Measured: 6.5k input tokens, 1.8–2.6 s, ≈ $0.0003 per message; a 4–12-row render is typically
  10–30 k of the 197 k chars (first-12 fixture 3.4 %, largest-12 34 %). Codex (gpt-6-astra) review
  folded in: no `ObjectAccessor` lookup on the gateway worker thread, type-checked question-template
  reads, immutable per-generation byte count in the log line, trigger prefixes never truncated
  (r49 is 509 chars), harness asserts `render(all)` equality and both 12-row fixtures.
  Wired in `BuildGatewaySystemPromptEx` (Tier 9); autonomous ticks and any jev failure send the
  full text as before.
- **Config** `OllamaChat.Jev.Playbook.Enable` (0), `.MinRelevance` (0.3), `.MinRows` (4),
  `.MaxRows` (12), `.TimeoutMs` (4000). `prompts/jev_questions.json` gains the `playbook` site.
- `tests/jev/harness_playbook.cpp` (parser + selection, run against the real playbook; 3 injected
  arms fail as designed); `bench_jev.py --site playbook [--message …]` scores the live rows.

---

## [Unreleased] — 2026-09-21 (VLAT) — jev tactical funnel (PR 2: site B)

### Added

- **jev in front of the tactical executor, as a funnel** (`src/mod-ollama-chat_tactical.cpp`
  `JevTacticalDecide`, `Jev::PruneTacticalOptions` in `_jev_core.h`). Per tick: code prunes the
  menu to the registry-validated jev-eligible actions (`required` = `{botGuid}`, plus `bot_emote` /
  `bot_set_loot_filter` filled by speculative answers) that the dispatcher would accept *now*
  (combat block, HITL, ambient-in-combat, dead/alive) + `other`; one jev request answers `action`,
  `escalate`, `emote`, `loot_filter`; a confident non-`other` pick dispatches, anything else falls
  to the LlmWire/DeepSeek call with the remaining budget. Runs inside the per-bot async lambda.
- **Config** `OllamaChat.Jev.Tactical.Enable` (0), `.MinConfidence` (0.6), `.EscalateMin` (0.8);
  per-bot canary `"jev": true|false` in `Tactical.BotOverridesJsonFile` entries
  (`TacticalBotConfig.jev`, new `ReadOptionalJsonBool`). Precedence: `Jev.Enable=0` > per-bot >
  site flag.
- `prompts/jev_questions.json` gains the `tactical` site (action criteria mirror
  `ollama_tactical.md`'s policy; `escalate` / `emote` / `loot_filter` questions).
- `tests/jev/harness_tactical.cpp` — prune rules, 7 injected arms fail as designed.
- Post-implementation codex (gpt-6-astra) review folded in: the LLM tier's endpoint-slot wait is
  now bounded by the tick's absolute deadline (was unbounded); `bot_follow` / `bot_accept_resurrect_request`
  are offered only when their dispatch preconditions hold (master online / popup pending);
  `bot_emote` only with ambient enabled; nothing but idle when `Mcp.AllowActionTools=0`;
  `bot_stop_combat` (not tactical-allowed) and `bot_self_reincarnate` (consumable the snapshot cannot
  see) dropped from the candidates; jev-tier exceptions degrade to the LLM tier; a jev miss with no
  budget left is audited under `backend='jev'` as `jev_miss_no_budget`.

### Changed

- `TacticalInference::Query` takes an optional `timeoutOverrideMs` (the chain remainder) and uses
  `SetTimeoutMs`; `WriteAuditRow` / the tactical audit INSERT now record the deciding tier in
  `backend` (`jev` / `llm`) and jev's `prompt_tokens`; the tick INFO line carries `backend=`.
- `TacticalTickContext` carries `isDead`, `botClass`, `botLevel`, `directive` so the async lambda
  needs no world access.

---

## [Unreleased] — 2026-09-21 (VLAT) — jev decision tier in front of the fast paths (PR 1: A/C/D/E)

### Added

- **jev decision tier** (`src/mod-ollama-chat_jev.{h,cpp}`, `src/mod-ollama-chat_jev_core.h`,
  `docs/jev.md`) — TypeSafe's System One model answers the module's *decision-shaped* fast paths
  first: gateway command classifier (A), proactive intent (C), proactive tick-gate (D), and the
  planner's propose/wait veto (E). Typed `choice` answers with calibrated confidence, no text.
  Chain per call: jev → the unchanged LlmWire/DeepSeek call → deterministic code, under one
  absolute deadline (intent 3000 / tick-gate 2000 ms; jev capped to 1000 / 1200 / 1500 ms per site),
  a jev-side breaker (3 failures → 60 s open → LLM tier at zero cost), a keep-alive proxy-aware
  client pool, and exactly one dispatch per chain. Default access is TypeSafe's native endpoint
  (`https://api.typesafe.ai/v1/systemone`, model `jev-1.13.0` — reachable from game-host directly,
  0.96 s/call measured); OpenRouter's Decisions API (`typesafe/jev-1.13`, same wire format) is the
  alternate route and needs `Jev.Proxy` because it is geo-blocked from game-host's egress.
- **Config** `OllamaChat.Jev.*` (`Enable`, `Url`, `Model`, `ApiKey`, `Proxy`, `ProxyUser`,
  `ProxyPassword`, `TimeoutMs`, `MaxConcurrent`, `BreakerCooldownSec`, `QuestionsFile`, and per-site
  `Classifier` / `ProactiveIntent` / `ProactiveTickGate` / `ProactivePlanner` `.Enable` +
  `.MinConfidence`). **All default off** — zero behaviour change until flipped via `config_reload`.
- **`prompts/jev_questions.json`** — question wording per site; option sets come from the live
  C++ allowlists, never the file.
- **Audit `backend` column** (`'jev'|'llm'|'det'|NULL`) on `mod_ollama_chat_gateway_audit` and
  `mod_ollama_chat_tactical_audit` (+ `prompt_tokens` on the latter), added by a **startup
  self-migration** (`Jev::EnsureAuditBackendColumns`) because the image never applies
  `data/sql/characters/updates/`; INSERTs widen only once the columns are confirmed. The classifier
  gets its own `source_channel = 'gw_classifier'` label at last.
- **`OllamaHttpClient::SetTimeoutMs`** — millisecond-exact `Post()` timeouts so the LLM tier can
  be handed a sub-second chain remainder (legacy `SetTimeout(int)` unchanged).
- **`tests/jev/`** — `harness_jev.cpp` (parser/validator/breaker/deadline, 3 injected arms) and
  `harness_fallback.cpp` (chain ordering + budget, 4 injected arms), both two-directional with bare
  `g++`; `bench_jev.py` + `classifier_phrases.json` (79 lines) for threshold calibration against
  the live API, replaying the 340-phrase proactive set for intent. **Measured 2026-09-21**
  (`typesafe/jev-1.13-20260917`, from the Mac): classifier 96.2 % raw, 0 wrong-acted at 0.6 →
  `Jev.Classifier.MinConfidence = 0.6`; intent reject 96.6 % / approve 97.3 % / done 100 % after one
  wording fix (the "I'm all set" idiom), p50 ~825 ms, ~$0.00005 per call →
  `Jev.ProactiveIntent.MinConfidence = 0.8`. Note the contract-level behaviour change: affirmative-
  opener declines ("ok but not that one", "yeah no") become `reject` on the jev tier where the C++
  matcher returned no intent. Details + curves in `docs/jev.md`.

### Changed

- `llm::IntentResult` / `TickGateResult` gained `backend`, `minConfidence`, `promptTokens`;
  `PlannerResult` gained `backend`, `promptTokens`. Callers now gate on the **per-tier**
  `minConfidence` instead of the global knob (jev's confidence is calibrated, the LLM's is a
  self-report — the numbers are not interchangeable).
- `EmitAudit` / `WriteGatewayAuditRecord` take an optional `backend` (+ `promptTokens`).
- `OllamaHttpClient::Post` is now bounded end-to-end: `set_max_timeout` per attempt and the
  transport retry only gets the budget the first attempt left (previously connect + read + a
  retry could serially exceed the configured timeout — on the intent path that is the world
  thread).
- `ClassifierDirectToolAsLeaderCommand` accepts `disenchant` as a loot-filter mode (the fourth
  real upstream strategy), so squad-scoped loot orders no longer degrade to a leader-only call.
- Post-implementation review by codex (gpt-6-astra, read-only) drove: per-generation breaker +
  threshold, RAII lease with exception translation, `bot_set_raid_target_icon` excluded from the
  jev tier (optional `target_name`), planner veto attribution, and a bench acceptance predicate
  that mirrors the C++ (eligibility + every consumed slot's confidence + the 1500 ms cap).

### Not in this release

- The **tactical executor (site B)** stays on LlmWire/DeepSeek — both independent plan reviewers
  recommended deferring it (no labelled data, wide-choice regression risk, shares the tick thread);
  design frozen for PR 2.

---

## [Unreleased] — 2026-09-20 (VLAT) — Fleet despawn / spawn / teleport from MCP

### Added

- **`leader_admin_despawn`** — logs fleet bots out through their real owner.
  `Overlord.AutoSpawnBots` spawns via `sRandomPlayerbotMgr.AddPlayerBot(guid, 0)`,
  so the bots belong to the `RandomPlayerbotMgr`, not to any master's
  `PlayerbotMgr`; `.playerbots bot remove` from the leader *or* the Overlord
  session (`via_system_master`) returns `ok:true` and removes nothing. The tool
  calls `LogoutPlayerBot` on the rndbot holder first, then every known master's
  `PlayerbotMgr`, and reports `owner` per bot. `guids` / `names` / `all`.
- **`leader_admin_spawn`** — the inverse, through the same rndbot path the
  Overlord uses; skips bots already in-world.
- **`leader_admin_teleport`** — `Player::TeleportTo` for an online character,
  `Player::SavePositionInDB` for an offline one; destination `to_race_start`
  (playercreateinfo), `to_character` (live position or saved row) or explicit
  `map,x,y,z[,o]`. Zone for the offline write comes from `sMapMgr->GetZoneId`.
  For a **bot** the tool also sends the teleport ACK itself: playerbots'
  `HandleTeleportAck` skips selfbots (master == self) and defers to "the
  player's client", so the self-mastered fleet leader otherwise sits mid-
  teleport forever — not in world, `SaveToDB` deferred, every tool answering
  "player not found" (measured 2026-09-20 on the first far teleport).
  `ack_pending:true` completes a bot already stuck that way.

### Changed

- `leader_admin_command` `via_system_master` description corrected: it selects
  the Overlord session for verbs like `add`, but cannot despawn AutoSpawnBots
  bots (see above).

### Docs

- `docs/autonomous-bots.md` reroll runbook: step 2 is now `leader_admin_despawn
  {"all":true}`; gather step via `leader_admin_teleport`.

---

## [Unreleased] — 2026-09-20 (VLAT) — Character lifecycle from MCP: delete / create / set_fleet

### Added

- **`leader_admin_delete_character`** (MCP, `admin` facade) — permanent
  deletion through `Player::DeleteFromDB(…, deleteFinally=true)` (what
  `.character erase` does), so every table is cleaned and the name cache is
  cleared immediately — the name is reusable without a restart. Refuses online
  characters and the `SystemMasterGuid` / `Overlord.CharacterGuid`. `dry_run`
  / `confirm:true` gated. Companion of ops-api `wow_reset_character`, which
  keeps the guid.
- **`leader_admin_create_character`** (MCP, `admin` facade) — creates a level-1
  character server-side through the engine path (`Player::Create` →
  `SaveToDB(create)` → `OnPlayerCreate` → character-cache insert →
  `UpdateRealmCharCount`), the same sequence the client's create screen and
  `RandomPlayerbotFactory` use. Args `name`, `race`, `class` (names or ids),
  optional `gender` (random), `account_id` (default: owner of
  `Mcp.Leader.SystemMasterGuid`), `first_login` (default true: intro cinematic
  + `AT_LOGIN_FIRST`; false for bots). Client-equivalent name validation;
  Death Knights refused. `dry_run` / `confirm:true` gated.
- **`leader_admin_set_fleet`** (MCP, `admin` facade) — one-call fleet rewire:
  writes `Gateway.BotGUIDs`, `Overlord.AutoSpawnBots`, `Tactical.BotGUIDs`,
  `Mcp.Leader.BotGUIDs`, `Fleet.LeaderGuid`, `Fleet.MemberGuids`, blanks the
  retired pipe-format `Gateway.BotOverrides`, regenerates
  `Gateway.BotOverridesJsonFile` from a template entry (default: the current
  leader's), then reloads in-process and spawns the roster via the Overlord's
  `AutoSpawnBots` path. Timestamped backups of both files; JSON written first
  so a failure leaves the conf untouched. Needs `Mcp.ConfigEdit.Enable=1`.
- **`leader_admin_command` `via_system_master:true`** — force dispatch through
  the `SystemMasterGuid` (Overlord) session. Needed for `remove *` once
  `Fleet.EnsureParty` has self-mastered the leader: the fleet lives in the
  Overlord's `PlayerbotMgr`, so the default leader-master dispatch returns
  `ok:true` and despawns nothing.
- **`Overlord::SpawnConfiguredBots()`** — the startup auto-spawn loop is now a
  reusable function (returns the spawn count); behaviour at login unchanged.

### Docs

- `docs/autonomous-bots.md` — "Reroll the whole party from scratch" rewritten
  around the three tools (delete → create → set_fleet), no client step.

---

## [Unreleased] — 2026-08-09 (VLAT) — Robot-first fleet: party bootstrap, directive arbitration, honest summon

### Added

- **Fleet bootstrap** (`src/mod-ollama-chat_fleet.{h,cpp}`, new
  `OllamaChat.Fleet.*` keys: `EnsureParty` [default 0], `LeaderGuid`,
  `MemberGuids`, `EnsureIntervalSec` [20]). Periodic world-thread pass that
  self-masters the leader bot, forms its party, and wires the member bots'
  `PlayerbotAI::master` to the leader. Fixes the fleet being permanently
  ownerless (Overlord spawns ungrouped rndbots; playerbots nulls their master
  every tick), which made `bot_summon`/bare `bot_follow` silent no-ops.
  Re-entrant/self-healing; never touches bots in foreign groups; Overlord
  stays out of the party. First `OnUpdate` override in the module's
  WorldScript.
- **`OllamaChat.Gateway.AllowedChannels = "none"` sentinel** — explicit
  chat-off for robot-first mode (empty value falls back to `whisper`, so
  empty ≠ off). Disables all chat-triggered gateway+classifier traffic;
  escalations and `talk_to_leader` are unaffected.
- **Directive arbitration — operator wins** (`tactical_set_directive`).
  Writes are labeled by source: `mcp` (external agent), `player:<guid>`
  (voice fallback), `escalation` (strategic worker, detected via a
  thread-local `InEscalationContext()` flag around the worker's gateway
  call). Escalation writes/clears against an active operator directive are
  rejected with `reason=superseded_by_operator_directive`; operator writes
  always win. Previous `setBy` labels (`strategic`) retired.

### Changed

- **`DispatchBotChatCommand` issuer hardening**: with no whisperer (the MCP
  bridge strips undeclared args, so `player=0` on virtually every call), the
  issuer now resolves bot's-own-master → `Mcp.Leader.SystemMasterGuid`
  session → bot (old behavior last). Previously it always self-mastered,
  which playerbot's security check silently rejected.
- **`bot_summon` / `bot_follow` honesty**: `bot_summon` now refuses with a
  structured error when the bot has no registered master (previously a lying
  `ok:true` no-op) and on success reports `master` + a verify-via
  `get_bot_state` hint; bare `bot_follow` gets the same master precondition.

## [Unreleased] — 2026-05-18 (VLAT) — `bot_loot_nearby` MCP tool

### Added

- **`bot_loot_nearby` MCP tool** (`src/mod-ollama-chat_tools.cpp`,
  registered in `BuildRegistry`). Wraps playerbot `add all loot`
  (AddAllLootAction): scans the bot's nearby corpses + lootable game
  objects (chests, herb/ore nodes, skinnable corpses) and pushes each
  onto the `available loot` LootObjectStack so the MoveToLootAction →
  OpenLootAction → StoreLootAction pipeline picks them up on the very
  next AI tick. Closes the loot loop the agent previously had to wait
  on playerbot's own ~5s "often" trigger to satisfy.

### Pre-flights

- `+loot` MUST be on the non-combat strategy list (read via
  `PlayerbotAI::HasStrategy("loot", BOT_STATE_NON_COMBAT)`). When
  missing the tool returns `loot_strategy_disabled` with the current
  non-combat strategies so the agent can call
  `bot_set_strategy(mode='non_combat', changes='+loot')` and retry
  instead of burning a dispatch on a silent no-op.
- Dead bots are rejected with the same message style as bot_summon —
  call `bot_release_spirit` or `bot_revive` first.

### Why

The bot's autonomous loot-strategy tick is slow enough that
post-combat skin/loot intents lagged noticeably. This wraps the
existing chat-callable verb that forces an immediate scan so the agent
can drive `attack → wait for combat end → loot now` as a coherent
sequence. WHAT gets picked up is still filter-gated by
`bot_set_loot_filter` and the `bot_add_loot_item` whitelist — this
tool only controls WHEN the scan runs.

---

## [Unreleased] — 2026-05-17 (VLAT) — proactive tick gate (Phase C)

### Added

- **Local-Ollama tick gate** in front of the (paid) planner. New
  `EvaluateTickGate` in `src/mod-ollama-chat_proactive_llm.{h,cpp}` calls
  the same local Ollama endpoint Phase A uses, returns
  `{Wait | Propose | Unset, confidence, latencyMs}`. Wired into
  `EvaluateTickLocked` between the cooldown gates and
  `SelectCandidateForPlayer`. A confident `Wait` skips the tick; anything
  else falls through to the existing C++ + planner path.
- **`prompts/proactive_tick_gate.md`** — small JSON-in/JSON-out prompt.
  Inputs: player combat / hpPct / idleSec, recentRejectedCount,
  sessionAgeSec, lastProposalAgoSec. Output:
  `{"action":"propose|wait","confidence":0.0}`. Hard-coded `wait`
  triggers: in-combat, low HP after combat, just-logged-in,
  three-or-more recent rejections.
- **New `lastTickGateAtMs` field on `CadenceState`** — throttle key so the
  gate fires at most once per `IntervalSec` per (bot, player), even on a
  10s heartbeat.
- **Five new config keys** under `OllamaChat.ProactiveTickGate.*`
  (`Enable=0` default OFF, `PromptFile`, `IntervalSec=30`,
  `TimeoutMs=2000`, `MinConfidence=0.6`). Reuses
  `Gateway.OllamaClassifier.Url` / `Model` so one local Ollama serves
  all three proactive prompts.
- **Audit channel `proact_tickgate`** in `mod_ollama_chat_gateway_audit`
  — one row per gate consultation, with `request_chars` = snapshot
  length, `response_chars` = raw verdict JSON length, `latency_ms` for
  SLO tracking.

### Why

Phase B made every propose tick a paid Claude call (gated by the
proposal cooldown). Phase C inserts a cheap local check ABOVE that:
"is this even worth asking the planner about?" When the gate vetoes,
we save a gateway call. When it approves, the planner still has the
final say. Two cheap calls per minute per player on the local Ollama
beats one paid call per cooldown window on every tick.

### Safety

- `Wait` verdicts do NOT consume the proposal cooldown — the `IntervalSec`
  throttle is the cost control for Ollama; the proposal cooldown only
  fires on actual emissions. If the gate is wrong about `wait` we just
  lose one tick, not the whole cooldown window.
- Sub-threshold confidence (default <0.6) is treated as if the gate
  didn't speak — caller falls through.
- Default OFF. Enabling requires deliberate operator action because the
  local Ollama can hallucinate `wait` and over-mute the bot.

---

## [Unreleased] — 2026-05-17 (VLAT) — proactive planner (Phase B)

### Added

- **Gateway planner** for the proactive propose-tick path. New
  `PlanProposalDecision` in `src/mod-ollama-chat_proactive_llm.{h,cpp}`
  wraps `QueryGatewayAPIRaw` with the new planner prompt and returns
  `{Wait | Propose | Unset, line, latencyMs}`. Wired into
  `EvaluateTickLocked` between candidate selection and emission: a `Wait`
  verdict consumes the normal proposal cooldown and skips emission; a
  `Propose` verdict uses the planner's line verbatim. `Unset` (any failure)
  falls through to the deterministic `PhraseLine` path.
- **`prompts/proactive_planner.md`** — system prompt that takes an enriched
  player/bot/candidate/context snapshot (combat, hpPct, idleSec,
  recentRejectedCount, sessionAgeSec) and returns
  `{"decision":"propose|wait","line":"<chat line>"}`. Hard rules: never
  invent a candidate not in the snapshot, never emit a line on `wait`,
  same 8-25 word + ends-with-question-mark contract as the proposer prompt.
- **Two new config keys** under `OllamaChat.ProactivePlanner.*`
  (`Enable=0` default OFF, `PromptFile`). Reuses `QueryGatewayAPIRaw` so
  gateway slot / cooldown / per-bot routing apply automatically.
- **Audit channel `proact_planner`** in `mod_ollama_chat_gateway_audit` —
  one row per planner consultation, with `request_chars` = snapshot length,
  `response_chars` = raw planner JSON length, `latency_ms` for SLO tracking.

### Why

Phase A made the agent smarter on the **player-reply** path (intent
classification). Phase B brings the same idea to the **propose** path:
the agent reads richer context than the C++ heuristics can express
("player just logged in", "player is in combat", "player has rejected
three in a row") and either approves the deterministic candidate or
vetoes the tick. The C++ side still picks the candidate (Phase B v2 will
move candidate selection itself to the agent); the agent's job for now is
veto + phrasing.

### Safety

`Wait` verdicts consume the normal `Proactive.ProposalCooldownSec` so the
agent can't be queried more than once per cooldown window per
(bot, player). Cost is bounded by the existing per-player proposal
cadence. Any failure path (planner disabled, missing prompt, gateway
down, malformed JSON, missing line on `propose`) falls through to v1's
`SelectCandidateForPlayer` + `PhraseLine`.

---

## [Unreleased] — 2026-05-17 (VLAT) — proactive intent classifier (Phase A)

### Added

- **Local-Ollama intent classifier** for the proactive-leader player-reply
  path. New translation unit `src/mod-ollama-chat_proactive_llm.{h,cpp}`
  exposes `ollamachat::proactive::llm::ClassifyIntent` which returns one of
  `{Approve, Reject, Done, Other, None}` with a confidence score. Wired into
  `ClassifyPlayerMessage` in `src/mod-ollama-chat_proactive.cpp` between the
  direct-command fast-path and the existing C++ substring matcher: confident
  Approve/Reject/Done short-circuits to the corresponding state transition;
  Other / None / low-confidence / HTTP failure all fall through to the
  deterministic matcher.
- **`prompts/proactive_intent.md`** — JSON-in/JSON-out classifier prompt with
  hard rules: never returns Approve/Reject without an open proposal, never
  returns Done without an executing plan, never reclassifies a direct
  command as an approval.
- **Four new config keys** under `OllamaChat.ProactiveIntent.*`
  (`Enable=1` default ON, `PromptFile`, `MinConfidence=0.7`,
  `TimeoutMs=3000`). Reuses the existing `Gateway.OllamaClassifier.Url` /
  `Model` so one local Ollama serves both prompts.
- **Audit channel `proact_intent_llm`** in `mod_ollama_chat_gateway_audit`
  — one row per LLM consultation, with `request_chars` = original message
  length, `response_chars` = raw classifier JSON length, and `latency_ms`
  for SLO tracking.

### Why

The proactive feature (002) shipped with pure-C++ heuristics for what to
propose, when to speak, and how to read the player's reply — only phrasing
went through the gateway. That doesn't cohere with the autonomous-agent
thesis. Phase A is the cheapest win: replacing the substring matcher with a
local-Ollama call robust to natural phrasings ("nah I'm farming", "wrapped
that quest already", "alright bet") at low latency and no paid-API cost.
Phases B (gateway planner for activity selection) and C (local-Ollama tick
gate for speak-now decisions) are queued behind their own config flags.

### Safety

The C++ substring matcher remains as the unconditional fallback path. If
`ProactiveIntent.Enable=0`, or the prompt file is missing, or Ollama times
out, or the model returns malformed JSON, or confidence is below threshold
— the C++ matcher runs as before. Direct-command tokens ("follow me",
"stay", "attack" ...) are checked in C++ above the LLM call: a stop command
never waits on an HTTP round-trip.

---

## 2026-05-12 (VLAT) — proactive leader bot phase 3-6

### Added

- **Phase 3 ranks 2 + 3 candidate selection**: `SelectCandidateForPlayer` in
  `src/mod-ollama-chat_proactive.cpp` now scans `acore_world.quest_template`
  for accept-eligible in-zone quests (rank 2, gated by `Player::CanTakeQuest`
  to never propose something faction/level/prereq-blocked) and walks a
  hand-curated 48-entry WoTLK 3.3.5a 5-man dungeon catalogue (rank 3) filtered
  by player level, faction-side flag, current-continent membership, and the
  `OllamaChat.Proactive.DungeonCandidates` csv whitelist. Rank-4 fallback now
  resolves the zone name via `sAreaTableStore`. Rank-1 (in-log quests) prefers
  same-zone matches before falling back to any non-rejected log quest.
- **Phase 4 proximity guardrails (US2)**: per-heartbeat `CheckProximity`
  (3D `Unit::GetDistance` against `Proactive.ProximityYards`) drives four
  states: in-range, too-far, different-zone, different-map. Too-far pauses
  travel (`!stay` dispatch) and emits one `proximity_warn` line per
  `ProximityWarnCooldownSec`; re-entry resumes silently. Zone/map change
  aborts the plan with `playerChangedZone`. Idle-anchor (no plan) silently
  refollows when the bot drifts beyond range.
- **Phase 5 idle nudge (US3)**: per-tick player-movement sampling drives
  `lastPlayerActivityAtMs`; `IsPlayerIdleForNudge` enforces threshold +
  cooldown + max-per-session + combat suppression. First action after a nudge
  resets `ignoredNudgeCount` so the bot self-silences only when truly ignored.
- **Generative phrasing**: new `QueryGatewayAPIRaw(botGuid, playerGuid,
  systemPrompt, userMessage)` in `src/mod-ollama-chat_gateway.{cpp,h}` —
  one-shot OpenAI-style chat completion that skips history / personality /
  language-hint merging. `PhraseLine` now sends the proposer prompt as the
  system role and a per-call JSON snapshot (`{lineKind, player, bot,
  candidate, context}` per `contracts/prompt-contract.md`) as the user role,
  with deterministic templates as fallback when generative returns empty.
- **C++-side intent classifier**: `ClassifyPlayerMessage` no longer routes
  through `TryGatewayIntentClassify` (which is action-dispatch oriented and
  returns free-form acks). It now does deterministic string/token matching
  in C++ with first-3-token approval/rejection sets, first-5-token done set,
  and a 24-entry direct-command takeover list (FR-014). Empirically 100% on
  the 188-phrase bench (`tests/proactive/`); SC-002 ≥95% target met
  without any LLM round-trip.
- **Classifier accuracy bench (T060a)**: `tests/proactive/classifier_phrases.json`
  (50+ phrases each across approve / reject / done / directcmd / none) +
  `bench_classifier.py` + `bench_classifier.sh`. Python implementation mirrors
  the C++ rules; rerun with `bash tests/proactive/bench_classifier.sh` after
  any rule change.

### Changed

- **`mod_ollama_chat_proactive.cpp` is now ~1600 LOC** (was ~950) — phase 4/5
  logic + dungeon table + tokenizer + generative wiring + zone-name helper.

### Not changed

- No new SQL migration. No new MCP tool. No new playerbot verb. No new
  background thread. Proactive heartbeat still rides
  `TacticalLeaderTick::Run()`.
- `Proactive.Enable` default still `0`. Phase 3-6 ship behind the same kill
  switch as Phase 1-2.

---

## [Unreleased] — 2026-05-04 (VLAT) — wow-admin master_wow.sh tool family

### Added

- **18 new wow-admin MCP tools** mirroring `master_wow.sh` operator capabilities,
  plus git pull and SQL import — turns the daily SSH workflow into agent-driven
  ops. Tool families:
  - `git_pull_core` / `git_pull_module` / `git_pull_all` — `git pull --ff-only`
    against the AzerothCore tree and modules. Module name regex-validated
    (`^mod-[a-z0-9-]+$`) before any path operation.
  - `sql_import_file` / `sql_import_dir` — bulk SQL import via TCP `ops_rw`.
    `sql_import_dir` adds the gap master_wow.sh has: `exclude_files` (basename
    list) and `exclude_glob` (filepath.Match against basename) so optional
    module SQL can be skipped.
  - `wow_create_account` / `wow_set_gm_level` — worldserver-cli wrappers via
    docker exec.
  - `wow_check_realmlist` / `wow_update_realm_ip` / `wow_update_realm_port` —
    realmlist read+update on acore_auth.
  - `wow_backup_dir` / `wow_backup_db` / `wow_backup_volumes` — pure-Go tar+gzip
    or mysqldump-via-docker-exec; output to `$OPS_WOW_BACKUP_DIR`.
  - `wow_restore_dir` / `wow_restore_db` / `wow_restore_volume_db` /
    `wow_restore_volume_client` — restore from a backup file in the same dir;
    volume restores auto-stop the owning container before wiping.
  - `wow_list_backups` / `wow_prune_old_backups` — housekeeping. `dry_run:true`
    available for prune.
  All destructive tools require `confirm:true`. Passwords (`password`,
  `db_pass`) auto-redacted in audit rows by `redact_args.go`.

### Changed

- ops-api Docker image base swapped from `gcr.io/distroless/static:nonroot` to
  `debian:bookworm-slim` (still uid 65532) so `git` is available for
  `git_pull_*`. Image grows ~70 MB; acceptable for an internal admin tool.
- New MCP server header advertises **35 tools** (was 17). Smoke-check command in
  `docs/admin-mcp.md` updated.

### New env vars (admin-mcp side)

- `OPS_WOW_ROOT` (default `/wow-root`) — AzerothCore tree mount inside ops-api.
- `OPS_WOW_BACKUP_DIR` (default `/backups`) — backup storage mount.
- `OPS_DB_CONTAINER` (default `ac-database`) — docker exec target for
  mysqldump / mysql restore.
- `OPS_WORLD_CONTAINER` (default `ac-worldserver`) — docker exec target for
  worldserver-cli.
- `OPS_DB_AUTH_DSN` — ops_rw DSN bound to acore_auth (realm tools).
- `OPS_DB_IMPORT_DSN` — ops_rw DSN with `multiStatements=true` (sql_import_*);
  falls back to `DB_EXEC_DSN` when unset.
- `OPS_MODULE_NAME_PATTERN` (default `^mod-[a-z0-9-]+$`).
- `OPS_WOW_VOLUMES_JSON` / `OPS_WOW_VOLUME_MOUNTS_JSON` /
  `OPS_WOW_VOLUME_OWNERS_JSON` — volume backup/restore config.

### Operator setup (one-time on game-host, applied AFTER merge)

1. Compose: add `/wow-root:rw`, `/backups:rw`, and the two volume bind mounts;
   set the new envs (full block in `docs/admin-mcp.md`).
2. MySQL: `GRANT CREATE, ALTER, DROP, INDEX, REFERENCES ON acore_world.* /
   acore_characters.* / acore_auth.* TO 'ops_rw'@'%';` (deliberate blast-radius
   increase for sql_import_* — broadens what `db_exec` could in principle do
   too; keyword regex in `tools_db.go:30` is the application-layer guard).
3. `chown -R 65532:65532 /opt/backups/wow` so the nonroot user inside ops-api
   can write backup files.

---

## [Unreleased] — 2026-04-26 (VLAT) — Tactical Ambient Mode

### Changed

- Removed the legacy direct local-Ollama chat/random/event responders. Ordinary
  player chat now reaches gateway/classifier bots only; proactive local behavior
  is handled by Tactical Ambient Mode.
- Gameplay event hooks now record lightweight recent_event_* context for the
  tactical snapshot instead of spawning separate event-specific Ollama replies.
- Gateway `FallbackToOllama` and `FallbackOnError` are deprecated no-ops because
  the old local chat fallback path no longer exists. Gateway errors are
  logged/audited and stay silent.

### Added

- Tactical can now choose `tactical_idle` for routine unchanged ticks. This is
  the expected no-op instead of forcing idle emotes.
- Tactical Ambient Mode adds contextual `bot_emote` and short `bot_say` actions
  with per-bot speech/emote gaps, rolling visible-action caps, and repeat
  suppression.
- Runtime nearby bot enrollment:
  `OllamaChat.Tactical.AutoEnrollNearbyBots = 1`,
  `OllamaChat.Tactical.NearbyBotMax = 5`, and
  `OllamaChat.Tactical.NearbyBotRadius = 60.0`.
- New ambient config keys:
  `Tactical.AmbientEnable`, `Tactical.AmbientMinSpeechGapSec`,
  `Tactical.AmbientMinEmoteGapSec`,
  `Tactical.AmbientMaxVisibleActionsPerMinute`, and
  `Tactical.AmbientEventReactionChance`.

---

## [Unreleased] — 2026-04-26 (VLAT) — feedback-daemon CI build

Adds a path-filtered GitHub Actions workflow that cross-compiles the
Windows feedback-daemon `.exe` and publishes a rolling pre-release tag
whenever `apps/feedback-daemon/**` changes on `main`. PRs run the same
build (vet + test + cross-compile) as a smoke check; only `push` to
main publishes the release.

### Added

- `.github/workflows/build-feedback-daemon.yml`. Triggers:
    - `push` to `main` matching `apps/feedback-daemon/**` or the
      workflow file itself → vet + test + cross-compile + publish
      `feedback-daemon-latest` rolling release.
    - `pull_request` matching the same paths → vet + test +
      cross-compile + upload as a 90-day workflow artifact (no release).
    - `workflow_dispatch` for manual runs.
- Rolling tag `feedback-daemon-latest` (auto-recreated each push) gives
  a stable URL:
  `https://github.com/synthiq-ai/synthiqbots/releases/download/feedback-daemon-latest/feedback-daemon-windows-amd64.exe`.
- Frozen point-in-time snapshot at `feedback-daemon-v0.1.0` is left
  alone — manual semver bumps for milestone releases continue to live
  as separate tags.
- Build flags `-trimpath -ldflags='-s -w -X main.commit=<short-sha>'`
  produce a reproducible ~7.6 MB statically-linked binary with the
  source SHA embedded.

### Why path-filtered

Most pushes to `main` touch the C++ module / classifier prompts /
docs and don't change the daemon code. Triggering a cross-compile on
those would waste CI minutes and republish identical bits. The
`paths:` filter on `apps/feedback-daemon/**` skips the workflow when
nothing relevant changed.

---

## [Unreleased] — 2026-04-26 (VLAT) — prompt path and OpenClaw model defaults

### Fixed

- Prompt-file defaults now resolve from `modules/mod-ollama-chat/prompts/...`
  instead of the stale `../../../modules/...` paths that miss the module directory
  when worldserver runs from the AzerothCore root. The loader also recognizes the
  old relative default and retries the correct module path, so existing configs
  with the stale value can recover on reload/deploy.
- The default OpenClaw gateway model is now `openclaw`, matching the current
  OpenClaw API contract (`openclaw` or `openclaw/<agentId>`). Examples no longer
  suggest OpenAI model names for OpenClaw-backed bot overrides.
- The local gateway classifier timeout default is now `5000` ms. A cold or
  queued `qwen3:8b` classifier request can exceed the previous `1500` ms window,
  causing simple commands like `attack` to fall through to the slower gateway
  path even though the local classifier would classify them correctly once warm.
- The local classifier prompt now treats `kill` / `kill it` / `nuke it` as
  attack intents and explicitly forbids mapping them to `emergency_stop`/`stay`.
  This prevents "kill it" party-chat commands from stopping the squad.
- The local classifier prompt now emits complete raw playerbot follow commands
  for `follow`, `follow me`, `come here`, `on me`, and `regroup` while grouped
  with the human, e.g. `leader_command_all(command="follow <human>")`, avoiding
  slow fallback to the gateway after incomplete local follow classifications.
- Bare `follow` and `stay` now classify as leader/squad broadcasts when they
  reach the leader bot, so players no longer need to add `all` for the rest of
  the party to follow or hold position.
- The classifier now treats `Claude` as the command leader/interface, not a
  single-bot target. Naming `Clawd`, `Geek`, or another non-Claude bot targets
  only that bot with `leader_command`; otherwise commands broadcast with
  `leader_command_all`.
- The local classifier playbook is shorter and the classifier context default is
  now `2048` tokens, so high-priority command routing survives the appended
  runtime context. Bare `attack` / `kill it` now classify as
  `leader_command_all(command="attack my target")` instead of direct
  `bot_attack_target`, making the whole squad engage immediately.
- The classifier dispatch path now structurally rewrites leader-default direct
  tools such as `bot_stay`, `bot_follow`, `bot_sell_junk`, `bot_autogear`, and
  `bot_roll` into `leader_command_all` when Claude is the leader interface, so
  unmentioned party-chat commands affect the squad instead of only Claude.
- `revive all` now uses the leader path as a compound squad action (`release`
  then `revive`) instead of reviving only Claude, and `summon to me` /
  `teleport to me` classify locally to the playerbot `summon` command instead
  of falling through to the gateway.
- Leader classifier repair now extracts named non-Claude bot targets from the
  message, so named quest phrases like `Geek hand in quests` stay single-target
  even if the classifier emits a broadcast. Compound `eat and drink` runs both
  `eat` and `drink` through the same leader scope.

---

## [Unreleased] — 2026-04-26 (VLAT) — wow-admin MCP fixes (post-deploy testing)

Six bug-fix bundle from end-to-end testing of the wow-admin MCP after PR
#53 went live. All caught by exercising the 17 tools against the deployed
endpoint.

### Fixed

- **`container_inspect` no longer leaks secrets.** Previously returned the
  full `.Config.Env` array verbatim, exposing `OPS_BEARER_TOKEN`,
  `MCP_BEARER_TOKEN`, and the `ops_ro` / `ops_rw` DB passwords. Now walks
  the inspect doc and replaces any env value (or label value) whose key
  matches `OPS_ADMIN_DENIED_KEYS` with `***REDACTED***`. Matching is
  case-insensitive so SCREAMING_SNAKE env keys (`DB_DSN`,
  `OPS_BEARER_TOKEN`) match TitleCase deny patterns (`*Token*`, `*DSN*`).
- **Audit inserts no longer silently drop.** `WriteAdminAudit` now grabs
  a connection and runs `USE \`acore_characters\`` before the INSERT,
  instead of relying on the default schema in `DB_EXEC_DSN`. Operator
  DSNs that omit the path segment (`...@tcp(host:3306)/?parseTime=true`)
  used to fail with `Error 1046 (3D000): No database selected` — every
  tools/call audit row was silently lost.
- **`clientIP()` no longer records junk.** Audit rows showed
  `client_ip="client"` because Go's net/http set `RemoteAddr` to that
  literal string in some proxy paths and the parser carried it through.
  Now validates each candidate (X-Forwarded-For → X-Real-IP →
  Cf-Connecting-IP → RemoteAddr) parses as an IP via `net.ParseIP`,
  falls back to `"unknown"` if nothing matches. Skips junk entries
  inside an XFF list. Bracketed IPv6 (`[2001:db8::1]:443`) handled.
- **`container_list` response no longer blows the tool-result cap.**
  40 containers × full Traefik labels = 62 KB, exceeding the 25 KB cap
  every call. Labels now omitted by default; pass `verbose:true` to
  include them.
- **`db_query` / `db_exec` reject queries against `mysql.*`,
  `information_schema.*`, `performance_schema.*`, `sys.*`.** Defense in
  depth on top of the `ops_ro` / `ops_rw` GRANTs (which already block
  these); the regex catches a mis-applied grant before it hits the
  server.
- **Default `OPS_ADMIN_DENIED_KEYS` expanded** to cover both
  TitleCase (`OllamaChat.Mcp.BearerToken`) and SCREAMING_SNAKE
  (`DB_DSN`, `DATABASE_INFO`) conventions:
  `*Token*,*Password*,*Passwd*,*Secret*,*BearerToken*,*ApiKey*,*Api_Key*,*DSN*,*Database*,*Credential*`.
  Operators with explicit `OPS_ADMIN_DENIED_KEYS` in their compose
  should expand similarly — the underscores in env keys mean a
  `*DatabaseInfo*` pattern doesn't match `DATABASE_INFO`.

### Operator follow-up

- Set `OPS_ADMIN_CONFIG_ROOT=/configs/modules` in
  `<your compose dir>/compose.yml` (was `/configs`). Module conf files
  live one level deeper than `worldserver.conf`, so `file_set_key` was
  failing with "no such file or directory" until pointed at the right
  root. Doc updated.
- Set `DB_EXEC_DSN=...@tcp(192.168.100.11:3306)/acore_characters?parseTime=true`
  (was `.../?parseTime=true`). Fixed in compose during testing; the code
  fix above is the durable defense.

### Tests added

- `mcpserver/redact_test.go` — case-insensitive env redaction across the
  default deny set (catches BEARER_TOKEN, DSN, PASSWORD, DATABASE).
- `mcpserver/clientip_test.go` — XFF / X-Real-IP / Cf-Connecting-IP /
  RemoteAddr precedence + junk fallback + bracketed IPv6 + XFF entry
  filtering.
- `mcpserver/redact_test.go::TestForbiddenSchemaRegex` — positive +
  negative cases for the schema-prefix block.

### Files

- Touched: `apps/ops-api/internal/mcpserver/{tools_container,audit,server,tools_db}.go`,
  `apps/ops-api/internal/config/config.go`,
  `apps/ops-api/main.go`,
  `apps/ops-api/README.md`,
  `docs/admin-mcp.md`
- New tests: `apps/ops-api/internal/mcpserver/{redact,clientip}_test.go`

---

## [Unreleased] — 2026-04-26 (VLAT) — feedback loop (PR 5: loopback)

Closes the in-game → gateway-agent feedback loop. The agent's response
now reaches the user back in-game via `/sb feedback log` — closing the
capture → upload → dispatch → response → display loop end-to-end.

Design: [`docs/feedback-loop.md`](feedback-loop.md) PR 5 section.

### Added

- **ops-api**: new `GET /v1/feedback/results` endpoint (bearer-gated,
  inside the existing auth group). Returns rows with `status IN
  ('resolved','error')` since a cursor, ordered by `id ASC`. Optional
  `since` (default 0) + `limit` (default 100, clamped to [1, 500]).
- **daemon**: new `internal/results/` package — `Client.Fetch` for
  the polling, `WriteSavedVariables` for the Lua serializer (atomic
  tmp+rename, double-quote escaping, long-bracket form for embedded
  JSON). New `state.LastResultsID` cursor + `Config.ResultsSavedVariablesPath`.
  Daemon main loop now ticks results-poll alongside the upload-scan.
  In-memory cache capped at 100 entries (oldest dropped by `db_id`).
- **addon**: `## SavedVariables` extended with `SynthiqBotsUIResults`.
  `/sb feedback log` now renders agent status + summary + PR URL per
  entry. Falls back to "pending" for unresolved rows.
- 7 unit tests: results client (cursor + bearer + 4xx), Lua serializer
  (escape, long-bracket, control chars, atomic write), ops-api handler
  (happy path, 503-no-DB, limit clamping).

### Why a sidecar file (and not the addon's own SavedVariables)

Mixing daemon writes with the addon's own SavedVariables would race:
WoW serializes the in-memory state on `/reload` and would overwrite
the daemon's recent writes with a stale snapshot. Keeping
`SynthiqBotsUIResults` as a daemon-only owned table — addon reads but
never writes — sidesteps this. The only race left is "daemon writing
during the millisecond WoW spends serializing", which is rare and
self-correcting (next tick re-fetches the same rows).

### What this completes

With PRs 1–5 merged + deployed:

```
WoW client                  user's PC               game-host
─────────                   ─────────               ─────
[/sb feedback]              [feedback-daemon]       [wow-ops-api]
  Screenshot()              tail SavedVariables     POST /v1/feedback ──► DB row + image file
  capture chat ringbuf  ──►  match Screenshots/  ──►                          │
  /reload                                                                     ▼
                                                                       [dispatch worker]
                                                                       multimodal Synthiq call
                                                                       (text + base64 image)
                                                                              │
                                                                              ▼
                                                                       UPDATE row: agent_response,
                                                                                   agent_actions,
                                                                                   agent_pr_url
                                                                              │
                            [feedback-daemon]                                 ▼
                            poll /v1/feedback/results ◄──────────────────────┘
                            write SynthiqBotsUIResults.lua
                                  │
[/reload]                         ▼
[/sb feedback log] ◄── reads SynthiqBotsUIResults global
  shows agent reply
```

Every captured `/sb feedback` now produces (a) a DB row + screenshot
file on the server, (b) an agent decision (config tweak / code PR /
clarifying question / no-op) persisted alongside the row, (c) a
visible status + summary + PR URL in `/sb feedback log` after `/reload`.

---

## [Unreleased] — 2026-04-26 (VLAT) — feedback loop (PR 4: dispatch worker)

Fourth slice of the in-game → gateway-agent feedback loop. Adds a goroutine
inside `wow-ops-api` that drains pending rows in `mod_ollama_chat_feedback`,
builds an OpenAI-compatible multimodal prompt (text body + base64
screenshot), POSTs to a vision-capable Synthiq agent, and writes the
structured response back into the row. Disabled by default; the gate is
the `FEEDBACK_DISPATCH_ENABLE=1` env var.

Design: [`docs/feedback-loop.md`](feedback-loop.md) PR 4 section.

### Added

- New `apps/ops-api/internal/dispatch/` package — 4 source files:
    - `db.go` — `Store.ClaimNext` (atomic SELECT...FOR UPDATE → flip
      to `dispatched`), `MarkResolved`, `MarkError`, `ResetStale`
      (rewind crashed-mid-flight rows).
    - `prompt.go` — locked system prompt (module-improvement agent
      framing) + `BuildUserContent` returning OpenAI-style content
      blocks (text + optional `image_url` with data-URL base64).
    - `synthiq.go` — `Client.Call` against `/v1/chat/completions`,
      strips `\`\`\`json` code fences, soft-fails on non-JSON
      responses (preserves `RawContent` for audit), bubbles upstream
      4xx/5xx as errors.
    - `worker.go` — `Worker.Run` poll-loop, claims one row per tick,
      reads image off disk, calls Synthiq, persists response.
      `StopOnEmpty` for one-shot test mode.
- 17 unit tests across the package: DB state transitions via
  `sqlmock` (claim/resolve/error/reset, empty-queue ErrNoRows path),
  prompt builder (image vs no-image, optional-field fallbacks, JSON
  embedding), Synthiq client (parse, fence-strip, non-JSON soft-fail,
  5xx bubble, refuse-unconfigured), worker E2E (full claim → call →
  resolve flow with `httptest` fake).

### New env vars (wow-ops-api)

- `FEEDBACK_DISPATCH_ENABLE` — master gate, set to `1` to start the
  worker (default `0`, disabled).
- `FEEDBACK_DISPATCH_URL` — OpenAI-compat chat-completions base for
  the dispatching agent, e.g. `https://geek.example.com/`.
- `FEEDBACK_DISPATCH_BEARER` — bearer token for that endpoint.
- `FEEDBACK_DISPATCH_MODEL` — vision-capable model id
  (e.g. `claude-sonnet-4-6`).
- `FEEDBACK_DISPATCH_POLL_SEC` — poll interval, default `30`.
- `FEEDBACK_DISPATCH_STALE_SEC` — rewind dispatched rows older than
  this back to pending at startup, default `600`.
- `FEEDBACK_DISPATCH_MAX_IMAGE_MB` — per-row image cap, default `16`.

### Operator setup

Wire the new env vars into `<your compose dir>/compose.yml` for the
`wow-ops-api` service and add the values to `<your secrets .env>`.
Bearer + model selection should target a vision-capable Synthiq agent
(Geek at `203.0.113.20:18789` is the project default — see CLAUDE.md
"Gateway bot routing").

### Notes

- The worker shares the existing `DB_EXEC_DSN` (`ops_rw`) connection
  pool with the wow-admin MCP — no new MySQL user needed; the
  `GRANT SELECT, INSERT, UPDATE` on `mod_ollama_chat_feedback` from
  PR 2 covers all worker queries.
- One row per tick is intentional. The agent latency dominates the
  total (~5–30 s for vision); concurrency would just give the agent
  more chances to step on its own commits if it's authoring PRs.
- The agent's response shape (`{action, summary, reasoning, ...}`) is
  permissive — fields the model didn't fill come through as zero-value.
  Non-JSON content soft-fails to `action="no_action"` with the raw
  text preserved in `agent_response` so operators can inspect what
  happened.
- Loopback (whisper agent's reply back to the user in-game, surface in
  `/sb feedback log`) lands in PR 5.

---

## [Unreleased] — 2026-04-26 (VLAT) — feedback loop (PR 3: Windows companion daemon)

Third slice of the in-game → gateway-agent feedback loop. Closes the
client-side egress: a single Windows .exe tails the addon's
SavedVariables file, matches `Screenshots/` files by mtime, and POSTs
both to PR 2's `/v1/feedback` endpoint. With this PR merged + deployed
to the user's PC, the in-game `/sb feedback <note>` button now produces
real DB rows + screenshot files server-side. PR 4 (dispatch worker) is
what makes the agent actually do something with them.

Design: [`docs/feedback-loop.md`](feedback-loop.md) PR 3 section.
Daemon README: [`apps/feedback-daemon/README.md`](../apps/feedback-daemon/README.md).

### Added

- New Go module `apps/feedback-daemon/`. Single binary, cross-compiled
  for Windows via `GOOS=windows GOARCH=amd64 go build`. ~11 MB,
  statically linked, no DLL deps.
- 5 internal packages:
    - `config` — TOML config loader (api_url, bearer_token, wow_root,
      account_name + 4 optional knobs). Validates the WoW dir exists,
      auto-picks the only account when ambiguous.
    - `saves` — embeds [`gopher-lua`](https://github.com/yuin/gopher-lua)
      (pure-Go Lua 5.1 VM) and `dostring`s the SavedVariables file.
      Handles nested tables, escaped strings in chat history, mixed
      number/string keys. ~5ms per parse on a 200KB file.
    - `screens` — scans `Screenshots/`, picks the file whose mtime is
      closest to the entry's `addon_ts` within ±15s.
    - `state` — atomic JSON state file at
      `%APPDATA%\synthiq-feedback\state.json` keyed by addon-side id.
      Tmp-write + rename so a mid-flush crash doesn't corrupt.
    - `upload` — multipart POST to ops-api `/v1/feedback`. Bearer auth.
- 17 unit tests across the 5 packages (config defaults / validation /
  account-dir resolution; saves happy + missing + malformed; screens
  match + skip + tolerance; state round-trip + reload + atomicity;
  upload with image + meta-only + server-error + multipart shape).
- `apps/feedback-daemon/config.example.toml` — annotated config template.
- `apps/feedback-daemon/README.md` — install + cross-compile + CLI +
  troubleshooting reference.

### CLI

```
feedback-daemon [--config PATH] [--once] [-v]
```

`--once` for one-shot scan-and-exit, no flag for watch-mode polling.
Recommended install: drop the .exe under `%LOCALAPPDATA%`, register
a Scheduled Task at logon (`schtasks /Create /SC ONLOGON ...`).

### Notes

- SavedVariables only flushes to disk on `/reload` or logout — captured
  feedback entries don't appear to the daemon until the user reloads.
  Surfaced in both the addon's capture toast and the daemon's
  troubleshooting docs.
- Dedup via the local state file means the same id is never re-uploaded.
  Multiple WoW accounts on one PC need explicit `account_name` in
  config.toml — the daemon refuses ambiguity rather than guessing.

---

## [Unreleased] — 2026-04-26 (VLAT) — feedback loop (PR 2: server ingest)

Second slice of the in-game → gateway-agent feedback loop. Adds the
server-side `POST /v1/feedback` endpoint on `wow-ops-api` plus the DB
table for the dispatch worker (PR 4) to drain. Reachable end-to-end via
`curl` even without the Windows companion daemon (PR 3).

Design: [`docs/feedback-loop.md`](feedback-loop.md). Endpoint reference:
[`docs/ops-api.md`](ops-api.md) §`/v1/feedback`.

### Added

- `POST /v1/feedback` on `wow-ops-api` (bearer-gated, inside the existing
  auth group). Accepts `multipart/form-data` with a `meta` JSON text part
  and an optional `screenshot` file part. Writes the screenshot to
  `OPS_FEEDBACK_STORAGE_DIR/YYYY/MM/DD/{addonTs}_{safeID}_{rand}.{ext}`
  and inserts one row into `mod_ollama_chat_feedback` (`status='pending'`).
- New table `mod_ollama_chat_feedback`
  (`data/sql/characters/base/2026_04_26_feedback.sql`). Columns track the
  addon-supplied id, captured context (char/zone/note), chat history JSON,
  ctx JSON, image metadata (path/bytes/mime), `status` enum, and the
  agent-fill columns (`agent_response`, `agent_actions`, `agent_pr_url`,
  `dispatched_at`, `resolved_at`) the dispatch worker uses.
- Re-scoped the project from 4 PRs to 5 — split the Windows companion
  daemon out of PR 2 into its own PR 3, so each PR is reviewable in
  isolation.

### New env vars (wow-ops-api)

- `OPS_FEEDBACK_STORAGE_DIR` — mounted host volume for screenshot files
  (e.g. `/feedback`). Empty → endpoint returns 503.
- `OPS_FEEDBACK_MAX_IMAGE_MB` — multipart image cap (default `12`).
- `OPS_FEEDBACK_MAX_META_KB` — meta JSON cap (default `256`).

### Required setup on game-host (one-time)

- Mount a host directory into `wow-ops-api` at the storage path (e.g.
  `<your compose dir>/feedback/` → `/feedback`) and set
  `OPS_FEEDBACK_STORAGE_DIR=/feedback` in
  `<your secrets .env>`.
- Grant `ops_rw` write access to the new table:
  `GRANT SELECT, INSERT, UPDATE ON acore_characters.mod_ollama_chat_feedback TO 'ops_rw'@'%'; FLUSH PRIVILEGES;`

### Tests

- `apps/ops-api/internal/handlers/feedback_test.go` — 7 cases:
  503 when storage dir empty, 503 when DBExec nil, 400 on missing/invalid
  meta, 400 on missing id, happy path with image (verifies disk write +
  expected SQL via `sqlmock`), meta-only happy path.

### Notes

- The endpoint accepts uploads with no screenshot — the daemon may need
  to upload meta-only when the screenshot file went missing between
  capture and upload (e.g. user emptied `Screenshots/`).
- `imagePath` in the response is relative to `OPS_FEEDBACK_STORAGE_DIR`;
  the dispatch worker (PR 4) will read the image by joining the storage
  root with this path.

---

## [Unreleased] — 2026-04-26 (VLAT) — feedback loop (PR 1: addon side)

First slice of an in-game → gateway-agent feedback loop. The agent gets a
screenshot + chat history + game context and decides whether to apply a live
config tweak via existing MCP tools or open a code-fix PR via its own gh
tooling. PR 1 ships the client-side capture only; the companion daemon,
ops-api ingest, dispatch worker, and loopback whisper land in PRs 2–4.

Design + 4-PR phasing: [`docs/feedback-loop.md`](feedback-loop.md).

### Added

- Addon: `/sb feedback <note>` slash command and `inv_misc_note_01`
  Main-column button (at `y=442`, right after Auto-Chat). Captures
  `Screenshot()` + a 200-line chat ring buffer
  (`CHAT_MSG_{SAY,YELL,PARTY,RAID,GUILD,OFFICER,CHANNEL,WHISPER,EMOTE}` +
  `_LEADER`/`_INFORM` variants) + a context snapshot (zone, target,
  group, char identity) into
  `SynthiqBotsUISave.feedback_queue[]`.
- `/sb feedback log` lists recent queue entries (and, post-PR-4, agent
  replies); `/sb feedback help` prints usage.
- Queue capped at 50 entries; ring buffer at 200 lines; both trim oldest
  on overflow.

### Notes

- The addon dispatcher lowercases the slash-command rest argument to
  support `chat on/off/status`. The new `feedback` branch re-parses the
  original `msg` so the user's note keeps its casing.
- SavedVariables flushes only on `/reload` or logout — queued entries
  are RAM-only until then. The addon prints a reload reminder on each
  capture.

### Files

- New: `addons/synthiqbots-ui/SynthiqBotsUIFeedback.lua`,
  `docs/feedback-loop.md`
- Touched: `addons/synthiqbots-ui/synthiqbots-ui.toc`,
  `addons/synthiqbots-ui/SynthiqBotsUI{Init,Handler}.lua`,
  `addons/synthiqbots-ui/SynthiqBotsUI.lua`,
  `addons/synthiqbots-ui/{README,UPSTREAM}.md`,
  `docs/synthiqbots-ui.md`

---

## [Unreleased] — 2026-04-26 (VLAT) — wow-admin MCP

Splits ops/admin out of the gameplay MCP onto a new transport at `POST /mcp`
on the existing `wow-ops-api` Go binary. Single binary, single bearer, single
deploy job. Gameplay MCP at `:18790` is unchanged; the 8 read-only `ops_*`
tools are now available on both surfaces during the parallel-availability
window (PR-B will remove the C++ versions once verified live).

### Added

- New `wow-admin` MCP server on `wow-ops-api` (`POST /mcp`, `GET /mcp/health`).
  Reachable as `https://ops.wow.example.com/mcp` and LAN fast-path
  `http://192.168.100.11:18791/mcp`. Bearer `OPS_BEARER_TOKEN`.
- 17 tools: 8 migrated read-only (`ops_status`, `ops_logs_tail`,
  `ops_files_{list,read,search}`, `ops_audit_{gateway,tactical,summary}`); 5
  container ops (`container_list`, `container_inspect`, `container_restart`,
  `container_stop`, `container_start`); 2 SQL (`db_query` against new
  `ops_ro` SELECT-only path; `db_exec` against new `ops_rw` user with
  INSERT/UPDATE/DELETE allowlist + `confirm:true` + WHERE-guard); 2 file
  edit (`file_set_key` for atomic `.conf` key-value edits with per-file
  allowlist + always-deny patterns; `file_write` for full overwrite under
  `OPS_ADMIN_WRITE_GLOBS`).
- Master gate `OPS_ADMIN_ALLOW_ACTIONS` (default `1`, ENABLED) — set to
  `0` and redeploy to refuse every destructive tool with
  `{"error":"action tools disabled..."}` for a strict read-only window.
  Personal-server deployment behind Traefik IP-allowlist + bearer makes
  default-on the right tradeoff for a single-operator setup.
- Per-tool token-bucket rate limit, configurable via
  `OPS_ADMIN_RATE_LIMITS_JSON` (defaults: 5/min/IP for `db_exec`,
  30/min/IP for everything else).
- New audit table `mod_ollama_chat_admin_audit` — one row per `tools/call`,
  carrying client IP, tool, args (truncated to 4 KiB), result, error, and
  duration in ms.
- Docker engine access switched from `/var/run/docker.sock` mount to TCP
  `DOCKER_HOST=tcp://192.168.100.11:2375` (LAN-bound, host-iptables-locked).
  Lets the admin MCP restart containers — including itself, with a
  warning safeguard when target == hostname.
- Deploy workflow: extra `/mcp/health` smoke check after `/v1/health`.

### New env vars (wow-ops-api)

- `OPS_ADMIN_ALLOW_ACTIONS`, `OPS_ADMIN_CONTAINER_DEFAULT`,
  `OPS_ADMIN_CONFIG_ROOT`, `OPS_ADMIN_BACKUP_DIR`, `OPS_ADMIN_ALLOWED_FILES`,
  `OPS_ADMIN_ALLOWED_KEYS_JSON`, `OPS_ADMIN_DENIED_KEYS`,
  `OPS_ADMIN_WRITE_GLOBS`, `OPS_ADMIN_RATE_LIMITS_JSON`,
  `DB_EXEC_DSN`, `DOCKER_HOST`.

### Operator setup

See `docs/admin-mcp.md` — dockerd TCP listener, `ops_rw` MySQL grants,
compose updates (drop docker-socket mount, add `DOCKER_HOST` env, mount
`/configs:rw` + backup dir), Synthiq `.mcp.json` second-server entry.

### Deprecated

- The 8 `ops_*` MCP tools served by the gameplay MCP at `:18790` will be
  removed in PR-B once the new admin-MCP path is verified in production.
  No behavior change yet — both surfaces respond identically.

---

## [Unreleased] — 2026-04-25 (VLAT) — ops-api: 8 proxy MCP tools

Exposes the existing `wow-ops-api` HTTP surface as MCP tools on the worldserver's
JSON-RPC server so the agent can self-diagnose (logs, files, audit) without ssh.
Each tool is a thin GET proxy — `CallOpsApi(path, params)` builds the URL,
percent-encodes values, attaches `Authorization: Bearer`, parses the JSON, and
returns it. Errors (transport failure, non-2xx, non-JSON body) propagate with
the upstream status + body snippet so failures are diagnosable in-band.

### Added

- 8 new read-only MCP tools (registry 51 → 59), all gated by `OllamaChat.Ops.Enable`:
    - `ops_status` → `GET /v1/status`
    - `ops_logs_tail` → `GET /v1/logs` (args: `lines`, `since`, `grep`, `regex`)
    - `ops_files_list` → `GET /v1/files/list` (required: `root`; optional: `path`, `glob`)
    - `ops_files_read` → `GET /v1/files/read` (required: `root`, `path`; optional: `start`, `end`, `maxBytes`)
    - `ops_files_search` → `GET /v1/files/search` (required: `root`, `q`; optional: `path`, `regex`, `max`)
    - `ops_audit_gateway` → `GET /v1/audit/gateway` (filters: `botGuid`, `accountId`, `from`, `to`, `errors`, `limit`, `offset`)
    - `ops_audit_tactical` → `GET /v1/audit/tactical` (filters: `botGuid`, `action`, `result`, `escalated`, `from`, `to`, `limit`, `offset`)
    - `ops_audit_summary` → `GET /v1/audit/summary` (`window`, default `24h`)
- New helper `OllamaHttpClient::Get(url, headers, &status, &body)` — richer return
  than `Post` because the proxy needs the upstream status code and body even on
  non-2xx to surface in the MCP error.
- New module file pair `src/mod-ollama-chat_opsapi.{h,cpp}` (single function
  `CallOpsApi` + percent-encoder + URL builder).
- New conf keys under `[worldserver]`:
    - `OllamaChat.Ops.Enable` (default `0`)
    - `OllamaChat.Ops.Url` (default `http://host.docker.internal:18791`)
    - `OllamaChat.Ops.BearerToken` — must equal `OPS_BEARER_TOKEN` in
      `<your secrets .env>` on game-host
    - `OllamaChat.Ops.TimeoutSeconds` (default `10`)

### Operator notes

- `ac-worldserver` must already have `extra_hosts: ["host.docker.internal:host-gateway"]`
  in compose — required for the loopback alias to resolve. The existing socat
  bridge depends on it; new feature does not change that requirement.
- Rotate `OPS_BEARER_TOKEN` and `OllamaChat.Ops.BearerToken` together, then
  `docker compose up -d wow-ops-api` + `.ollama reload`.
- `/v1/logs/stream` (SSE) is intentionally not exposed — MCP is request/response.
  Continue using direct `curl -N $H/v1/logs/stream` for live tailing.

### Out of scope

- Per-tool gate flags — the master switch is enough for the single-operator setup.
- A second bearer token distinct from the one ops-api uses — they share for v1.
- Pooled HTTP clients — defer until profiling shows it matters.

---

## [Unreleased] — 2026-04-25 (VLAT) — synthiqbots-ui: vendored MultiBot fork + Auto-Chat toggle

Vendors the [Macx-Lio/MultiBot](https://github.com/Macx-Lio/MultiBot) WoW client
addon into the repo as `addons/synthiqbots-ui/` (renamed surface + Lua identifiers).
Adds a single persistent toggle that gates the addon's reactive auto-handler chat
sends, so it can run alongside the server-side tactical agent without a feedback
loop. Client-only change — no server redeploy required.

### Added

- `addons/synthiqbots-ui/` — vendored fork at upstream commit
  `ecee413dacfa93ee705ede83f308f3a0396e6ece` (GPLv3, carried verbatim).
  Folder, `.toc`, slash commands (`/synthiqbots`, `/sbots`, `/sb`),
  SavedVariables, the global Lua identifier, and texture paths all renamed
  from `MultiBot` → `SynthiqBotsUI` / `synthiqbots-ui`.
- `Auto-Chat` toggle — new MultiBar Main-column button (icon
  `spell_holy_silence`, default ON). Persistent via
  `SynthiqBotsUISave["AutoChatCommands"]`. Slash equivalents:
  `/sb chat on|off|status`. Gates 17 `SendChatMessage` call sites in
  `SynthiqBotsUIHandler.lua`'s `CHAT_MSG_WHISPER` / `CHAT_MSG_LOOT` /
  `TRADE_CLOSED` blocks via a new `SynthiqBotsUI.sendBotChat()` chokepoint helper.
  UI button-driven sends (Attack, Follow, Stay, ...) are unaffected.
- `addons/README.md` — placement convention for vendored client addons
  (not part of the C++ CMake build).
- `addons/synthiqbots-ui/UPSTREAM.md` — provenance, rename mapping table,
  forward-merge recipe.
- `THIRD_PARTY_LICENSES.md` (repo root) — registry of vendored components and
  their upstream licenses.
- `docs/synthiqbots-ui.md` — feature reference + install instructions.

### Why

When the server-side tactical agent (`_tactical.cpp`) whispers the player,
upstream MultiBot's `CHAT_MSG_WHISPER` auto-handler reacts to "Hello" by
whispering `co ?` back at the bot. The tactical layer re-receives that whisper
and re-triggers, forming a feedback loop. The new toggle (default ON, preserves
upstream behaviour) lets the user break the loop without disabling the rest of
the UI.

### Upgrade notes

This is a client-side addon. Install or symlink `addons/synthiqbots-ui/` into
`<wow-client>/Interface/AddOns/synthiqbots-ui/` and `/reload` in-game.
Returning upstream MultiBot users get a fresh `SynthiqBotsUISave` table — old
window positions are not auto-migrated; see `addons/synthiqbots-ui/UPSTREAM.md`
for a manual migration recipe.

---

## [Unreleased] — 2026-04-25 (VLAT) — ops-api: published at https://ops.wow.example.com

Adds public TLS reachability for the ops-api sidecar. The LAN fast-path
(`http://192.168.100.11:18791`) stays as a bypass.

### Changed

- `docs/ops-api.md` — architecture diagram + setup snippet now describe the
  HTTPS path. Verification examples accept either host.

### Upgrade notes

The compose change lives in the operator's separately maintained deployment
configuration. Mirrors the existing `wow-mcp` pattern: `proxy` external network, Cloudflare-issued
TLS via Traefik, `wow-ops-allowlist` middleware (cloud Synthiq `203.0.113.20/32`
+ RFC1918). Bearer token remains the real auth gate.

---

## [Unreleased] — 2026-04-25 (VLAT) — ops-api: read-only sidecar over LAN

A small Go sidecar that exposes module status, worldserver Docker logs, file
browsing, and audit-table queries over a single bearer-token HTTP API. Replaces
the daily `ssh game-host` → `docker logs ac-worldserver` → `mysql ... | tail` loop
with `curl ops:18791/v1/...`. LAN-only by default (no Traefik, no public DNS) —
reach via VPN/Tailscale.

See `docs/ops-api.md` for the full reference.

### Added

- `apps/ops-api/` — Go 1.23 sidecar (chi router, stdlib HTTP, distroless image).
  Endpoints: `/v1/{health,status,logs,logs/stream,files/list,files/read,files/search,audit/gateway,audit/tactical,audit/summary}`.
  Talks to the Docker socket for log tailing, the in-process MCP server for
  module-side counters, and a read-only MySQL user for audit slices.
- `ops_module_status` MCP tool (`src/mod-ollama-chat_tools.cpp`) — read-only
  JSON snapshot of uptime, gateway/MCP/tactical counters, top tool-call stats,
  feature flags, personality count. Consumed by the ops-api sidecar; ignores
  `botGuid` and `playerGuid`.
- New `deploy-ops-api` job in `.github/workflows/deploy.yml`. Runs after the
  worldserver deploy regardless of its outcome (ops-api exists to inspect a
  broken worldserver, so it can't skip on failure). Rebuilds the image only
  when `apps/ops-api/**` changed since the last deploy.

### Upgrade notes

1. Add a `wow-ops-api` service to `<your compose dir>/compose.yml` (template
   in `docs/ops-api.md`). LAN-only — bind to game-host's LAN/Tailscale IP, not
   `0.0.0.0`.
2. Create the read-only DB user once: `CREATE USER 'ops_ro'@'%' IDENTIFIED BY '...'; GRANT SELECT ON acore_characters.mod_ollama_chat_% TO 'ops_ro'@'%';`.
3. Set `OPS_BEARER_TOKEN`, `MCP_BEARER_TOKEN`, `DB_OPS_RO_PASSWORD`, `OPS_BIND_ADDR` in game-host's `<your secrets .env>`.
4. Push to `main` (or `gh workflow run deploy.yml`) — CI builds the image and
   `docker compose up -d`s the new service. First run requires the compose
   service to already exist; CI will fail loudly if it doesn't.

---

## [Unreleased] — 2026-04-24 (VLAT) — Tactical: companion-mode local-Ollama agent

Hierarchical / cascading LLM agent. Free local Ollama runs the fast tactical
loop per configured bot; the paid gateway becomes purely reactive and fires
only on explicit escalation or human chat. No paid-side timer. Combat tactics
stay with the playerbots engine (dispatcher blocks `bot_attack_target` /
`bot_stop_combat` / `bot_revive` mid-combat). Companion ethos — bots narrate,
ask, and defer — comes from the action menu, not the tick frequency.

See `docs/tactical.md` for the full runbook.

### Added

- `src/mod-ollama-chat_tactical.{h,cpp}`:
  - `TacticalLeaderTick` singleton thread: heartbeat (default 10s) per bot
    with presence gate, cooldown, compact snapshot, local-Ollama inference,
    MCP dispatch. Circuit breaker (3-fail threshold, 30s cooldown) so a dead
    endpoint doesn't degrade the rest of the module.
  - `StrategicEscalationWorker`: condvar-signalled, bounded-queue, per-bot
    hour cap + cooldown. Fires ONE paid gateway call per escalation; the
    response is expected to invoke `tactical_set_directive` via tool-use.
  - Per-bot `TacticalDirective` (goal + TTL) read on every tick and injected
    into the user prompt. Tactical never blocks waiting for strategic.
- 22 new `OllamaChat.Tactical.*` config keys + 5 `OllamaChat.Strategic.*`.
- 2 new MCP tools: `tactical_get_directive`, `tactical_set_directive`
  (registered in `src/mod-ollama-chat_tools.cpp`; gated on
  `Mcp.AllowActionTools` + `Tactical.BotGUIDs`).
- New DB table `mod_ollama_chat_tactical_audit` (migration
  `data/sql/characters/base/2026_04_24_tactical_audit.sql`): one row per
  tactical tick with action, result kind, error, escalation flag, latency.
  Pruned by `Tactical.AuditRetentionDays` (default 7).
- New GM command `.ollama tactical status [botGuid]` — per-bot online/combat/
  directive dump + config snapshot.

### Changed

- `GatewayLeaderTick` now skips any leader whose guid is in
  `Tactical.BotGUIDs` when `Strategic.SuppressLeaderTickForTacticalBots=1`
  (default). Fixes double-billing — previously a tactical-enabled bot would
  fire both the free tactical tick AND the existing paid leader-tick on
  every material state change.
- Tactical tick log line uses compact `action=<name> ok|error: <msg>` summary
  at INFO (raw JSON result at DEBUG only) — AC's log pipeline re-parses
  format strings even through fmt::format wrappers, so any `{...}` in the
  final message triggers "Wrong format occurred" warnings. The INFO line now
  steers around that.

### Removed

- `mod-ollama-bot-buddy/` subdirectory — an upstream-abandoned sibling module
  evaluated for reuse. Its architecture (unthrottled per-world-tick full-
  state prompt rebuild + detached-thread pile-up + hardcoded bot name) was
  unsalvageable; the MCP tool registry already covers the same actions with
  better gating.

### Upgrade notes

1. **Apply the new SQL migration**:
   `data/sql/characters/base/2026_04_24_tactical_audit.sql` — creates
   `mod_ollama_chat_tactical_audit`. Auto-picked up on `docker compose up -d`;
   manual apply: `docker exec -i ac-database mysql -uroot -ppassword
   acore_characters < .../2026_04_24_tactical_audit.sql`.

2. **Default is safe for existing deployments.** `Tactical.Enable=1` ships on
   in the repo, but `Tactical.BotGUIDs=""` by default — nothing happens until
   you explicitly add a bot to the list. Drop-in upgrade.

3. **Ollama endpoint**: tactical expects a reachable Ollama HTTP endpoint.
   If you run Ollama on your Mac/PC, set `OLLAMA_HOST=0.0.0.0:11434` so it
   binds to LAN (default binds 127.0.0.1 only), then point `Tactical.Url`
   at the LAN IP.

4. **Cold-load timeout**: `Tactical.RequestTimeoutMs=3000` works only for
   already-loaded models. For any model ≥4B, bump to `30000` OR ensure the
   model is already hot before enabling tactical (`ollama run <model> "hi"`
   once is enough; the `keep_alive=30m` in the request body then keeps it
   GPU-resident between ticks).

---

## [Unreleased] — 2026-04-20 (VLAT) — Gateway: auto-claim bots on player login

Bridges the two autonomy modes for operators who sometimes want to play
alongside the fleet:

- Without this feature, Overlord keeps the fleet online 24/7 as rndbots,
  but when Raz logs in, the bots aren't in his party and party-chat
  gating has nothing to route to.
- With `AutoClaimOnLogin=1`: Raz's login triggers a reclaim — each bot in
  `Gateway.BotGUIDs` is logged out from its current holder (rndbot
  manager or another master's PlayerbotMgr) and re-spawned under Raz as
  master. Bots join his party, respond to party chat, follow him around.
- On Raz's logout, the bots are released back to rndbot status so
  Overlord's `SystemMasterGuid` fallback keeps driving admin commands
  via MCP. Fleet never leaves the world.

### Added

- `src/mod-ollama-chat_autoclaim.{h,cpp}` — `GatewayAutoClaimScript`
  (PlayerScript with `OnPlayerLogin` + `OnPlayerLogout`).
- 2 new config keys:
  - `OllamaChat.Gateway.AutoClaimOnLogin` (default 0)
  - `OllamaChat.Gateway.AutoClaimAccountIds` (default ""; csv of
    acore_auth account ids)
- Registered in `Addmod_ollama_chatScripts()`.

### How it works

1. `OnPlayerLogin(player)`: skip bot sessions (IsBot()) and non-whitelisted
   accounts. For each guid in `Gateway.BotGUIDs`: if already owned by
   this player, no-op; else find the current holder
   (`sRandomPlayerbotMgr` for rndbots, that master's `PlayerbotMgr`
   otherwise), call `holder->LogoutPlayerBot(guid)` synchronously, then
   call `newMgr->AddPlayerBot(guid, player.accountId)`.
2. `OnPlayerLogout(player)`: reverse — for each bot the player currently
   masters (`ai->GetMaster() == player`), `mgr->LogoutPlayerBot` then
   `sRandomPlayerbotMgr.AddPlayerBot(guid, 0)`.

---

## [Unreleased] — 2026-04-20 (VLAT) — Phase 3/4: emergency_stop + autonomous-bots runbook

Closes the planned autonomy work for this sprint.

### Added

- `emergency_stop(botGuid)` — panic-button MCP tool. Broadcasts `stay` to
  the leader's entire squad (same scope as `leader_command_all`). Wraps a
  fixed verb so the agent doesn't have to remember playerbot grammar
  during an incident. Tool registry grows 30 → 31.
- `docs/autonomous-bots.md` — end-to-end operator runbook covering cold-
  boot sequence diagram, one-time Overlord account/character setup,
  module config, MCP endpoint + Claude Desktop JSON template, an example
  Overlord system prompt, common workflows, and a troubleshooting
  section that maps `ok: false` and other failure modes back to their
  root causes.

---

## [Unreleased] — 2026-04-20 (VLAT) — Phase 2: agentic discovery tools

Two new read-only MCP tools that close the visibility gap between the
outside agent and the world. Both are one-shot observations — no
per-bot arguments, no in-world Player required.

### Added

- `get_bot_roster_by_account(accountName)` — DB read. Returns every
  character on an account: guid, name, class, race, level, zone, map,
  online status. Lets the agent discover which bots exist on an account
  before calling `leader_admin_command` to spawn from it (e.g. "what's
  on BOTMASTER? what's on OPERATOR?"). No world-state dependency — works
  even if nothing is online.
- `get_fleet_status()` — single-call snapshot of every gateway-routed
  bot (from `Gateway.BotGUIDs`) plus Overlord. For each: online/offline,
  class/race/level, zone/map/x/y/z, role (master/bot/unknown). Offline
  characters fall back to a DB read with last-known zone/level. First
  call of an agent turn — answers "is my fleet up, and where is
  everyone?" in one round-trip.

Tool registry grows from 28 → 30.

---

## [Unreleased] — 2026-04-20 (VLAT) — Fix: leader_admin_command needs leading '.'

`ChatHandler::ParseCommands` (`Chat.cpp:247`) refuses any input that doesn't
start with `.` or `!` and returns false immediately. `leader_admin_command`
was passing a bare `"playerbots bot <verb>"` string, so every call returned
`ok:false` regardless of master session, allowlist, or verb — the command
never reached the command table. Prepend `.` so the parser strips it and
dispatches normally. This is why both the Tier 9.1 master-session fix and
the Phase 1.2 system-fallback looked "dispatched but rejected" in the logs
— the real rejection was happening before any playerbots code ran.

---

## [Unreleased] — 2026-04-20 (VLAT) — Phase 1.1/1.2: autospawn fleet + fallback master

Closes the cold-boot loop. Overlord alone wasn't enough — on a fresh server
start, no leader bot is in-world and no human is available to spawn one, so
`leader_admin_command` had nowhere to dispatch. Two small additions make the
fleet come up end-to-end with zero human involvement:

### Added

- `OllamaChat.Overlord.AutoSpawnBots` — csv of character guids Overlord
  brings online right after registering as master. Uses `masterAccountId=0`
  (rndbot path) so same-account/guild/linked checks don't apply — Overlord
  can spawn leaders on any account. Example: `"20007,20008,20009"`.
- `OllamaChat.Mcp.Leader.SystemMasterGuid` — fallback master for
  `leader_admin_command`. When the leader's own `GetMaster()` is null (true
  for rndbots spawned by AutoSpawnBots), the tool dispatches through this
  character's session instead. Typically = `Overlord.CharacterGuid`.
- `leader_admin_command` response now includes `via_system_fallback`
  (bool) so the agent can tell when the fallback kicked in.

### Ops

On a fully autonomous deploy:
- `Overlord.Enable = 1`
- `Overlord.CharacterGuid = <Overlord's guid>`
- `Overlord.AutoSpawnBots = "<leader-1-guid>,<leader-2-guid>,..."`
- `Mcp.Leader.SystemMasterGuid = <Overlord's guid>` (same as CharacterGuid)

On every server boot: Overlord logs in → spawns leaders as rndbots →
agent (via MCP) can drive `leader_admin_command` and everything else.
No keeper, no real client.

---

## [Unreleased] — 2026-04-20 (VLAT) — Phase 1: Overlord headless master auto-login

Deployments that boot the worldserver on demand (not 24/7) can't rely on a
human keeper to stay logged in, but `.playerbots bot add/remove/list` via
`leader_admin_command` still needs a non-bot master session to dispatch
through. Overlord is that master: on startup, a background thread builds a
null-socket `WorldSession` (the same path mod-playerbots uses for its own
bots) and loads a configured character into the world, registering the Player
as a PlayerbotMgr rather than PlayerbotAI. From mod-playerbots' point of
view, Overlord looks like any other human-controlled master; from AC's point
of view, Overlord is a bot session with no socket I/O / DoS / Warden checks.

### Added

- `src/mod-ollama-chat_overlord.{h,cpp}` — scheduler + headless login logic.
- 3 new config keys under `OllamaChat.Overlord.*`:
  - `Enable` (default 0)
  - `CharacterGuid` (default 0 — low part of an existing character's guid)
  - `StartupDelaySeconds` (default 30 — wait for world warm-up)
- Hook into `OllamaChatConfigWorldScript::OnStartup` right after the leader
  tick starts.

### Ops

Create a dedicated character (e.g. "Overlord" on a `BOTMASTER` account)
through any real client, park it somewhere safe, look up its low guid in
`acore_characters.characters`, set `CharacterGuid` + `Enable=1`. On next
server start Overlord auto-logs-in and the agent's `leader_admin_command`
calls find a live master session without any human involvement.

---

## [Unreleased] — 2026-04-20 (VLAT) — Gateway: MentionExemptBots whitelist

New config key `OllamaChat.Gateway.MentionExemptBots` — comma-separated,
case-insensitive list of bot names that skip the `PrivateChannelMentionRequired`
gate on non-whisper private channels (party/raid/guild/officer). Whitelisted
bots respond to any party-chat line without their name being typed. The
keyword gate (if configured), account whitelist, and public-channel gating
are unchanged. Designed for designating an "always-listening" agent (e.g.
the squad leader) while keeping the rest of the roster quiet.

---

## [Unreleased] — 2026-04-19 (VLAT) — Tier 9.1 fix: leader_admin_command dispatches via master

`leader_admin_command` was dispatching `.playerbots bot <verb>` through the
leader bot's own WorldSession, which the playerbots command handler rejects:
`PlayerbotsMgr::GetPlayerbotMgr(bot)` returns `nullptr` for any player that
has `IsBotAI()==true`, so the handler returned "You cannot control bots yet"
and `ParseCommands` produced `ok: false`. Route the command through
`PlayerbotAI::GetMaster()`'s session instead — the human owner who spawned
the leader bot. If the master is offline, the tool now returns a specific
error pointing at that condition instead of silently failing.

Audit rows now record the master's account id (not the bot's), and the
returned JSON includes the master's name. Branch: `fix/leader-admin-dispatch-via-master`.

---

## [Unreleased] — 2026-04-19 (VLAT) — Tier 9/10: Leader Bot

Promote one designated gateway agent (e.g. Claude) to **squad leader**. The
leader can issue mod-playerbots commands at OTHER bots in real time via four
new MCP tools, and (optionally) runs a background tick that proactively
reassesses the situation and dispatches orders without needing to be whispered.
Branch: `feat/gateway-leader-tier9`.

### Added

- 4 new MCP tools (registry now 24 — 14 read-only + 10 action):
  `leader_list_targets`, `leader_command`, `leader_command_all`, `leader_get_command_help`.
  Dispatch reuses the proven `PlayerbotAI::HandleCommand(CHAT_MSG_WHISPER, text, leader)`
  pattern with the leader bot as the `fromPlayer` (issuer). Audit channel is
  `mcp:leader` so `.ollama gateway costs` sees leader activity separately.
- 10 new config keys (all `OllamaChat.Mcp.Leader.*`):
  `Enable`, `BotGUIDs`, `Scope` (`group`/`nearby`/`owner`/`all`), `NearbyRadius`,
  `DeniedCommands` (default: `logout,quit,delete,reset,suicide`),
  `RateLimitPerMinute` (default 30, separate from action-rate),
  `InjectPromptHint`, `AutoTickEnable`, `AutoTickIntervalSeconds`, `AutoTickMaxPerHour`.
- Tier 10: `GatewayLeaderTick` background thread that samples each leader's
  state every N seconds, diffs against the previous snapshot, and only fires
  a synthetic gateway request when something changed (combat in/out, target
  change, hp bucket, group composition, zone, nearby hostile count). Bounded
  by an hourly per-leader budget cap. Files: `src/mod-ollama-chat_leadertick.{h,cpp}`.
- New `BuildGatewaySystemPromptEx` branches: `[LEADER MODE]` block listing the
  `leader_*` tools when active bot is a leader; `[AUTONOMOUS TICK]` block when
  `playerGuid==0` (request came from the tick thread, not a whisper).
- New config key `OllamaChat.Gateway.BotOverridesJsonFile` — reads
  `BotOverridesJson` from an external file path. Necessary because AC's
  `Config.cpp:327` strips ALL `"` characters from inline values, breaking
  embedded JSON. The file path approach sidesteps that limitation. Priority
  order: `BotOverridesJsonFile > BotOverridesJson > BotOverrides` (legacy).

### Changed

- `BotOverridesJson` now used for `x-synthiq-model-override` header injection
  per-bot. Synthiq's openai-compat gateway IGNORES the request-body `model`
  field for actual Claude API calls (verified at
  the gateway source — body model is used solely for
  agent-id resolution); the only way to swap models is via this header. Wired
  through all 3 request paths (buffered, streaming, tool-use).

---

## [Unreleased] — 2026-04-19 (VLAT) — CI/CD: auto-deploy on merge to main

Branch: `ci/auto-deploy-on-main`.

### Added

- `.github/workflows/deploy.yml` — GitHub Actions workflow that runs on every push to
  `main` (and via manual `workflow_dispatch`). Uses the existing self-hosted
  runner on game-host to pull the new commit into
  `/srv/wow/docker/wow/azerothcore-wotlk/modules/mod-ollama-chat`, ensure
  `ac-database` is up, rebuild `ac-worldserver`, force-recreate it, then verify
  startup by polling container state and grepping recent logs for fatal errors.
  Fails the run on bootstrap crash, container exit, or fatal-class log lines —
  so the operator sees a red X in GitHub instead of having to SSH and tail logs
  manually. `concurrency` group prevents overlapping deploys.

---

## [Unreleased] — 2026-04-18 (VLAT) — Gateway expansion

Seven stacked feature batches on top of the existing whisper-only gateway path.
Branch: `feat/gateway-pr1-hardening-sessions`.

### Added

#### Hardening + warm sessions + status (PR1)

New config keys (all `OllamaChat.Gateway.*`):

| Key | Default | Purpose |
|---|---|---|
| `TimeoutSeconds` | `300` | HTTP timeout (was hardcoded) |
| `FallbackOnError` | `0` | Fall back to Ollama when the gateway errors |
| `MaxConcurrentRequests` | `4` | In-flight request cap (`0` = unlimited) |
| `MinSecondsBetweenRequests` | `5` | Per-(bot, player) cooldown |
| `UserPrefix` | `wow-bot-` | Prefix on the OpenAI `user` field (was hardcoded) |
| `MaxWhisperChunkChars` | `250` | Sentence-aware whisper chunk size; `0` disables chunking |
| `UseSessionPersistence` | `0` | Send `x-synthiq-session-key` for warm subprocess reuse |
| `SessionKeyTemplate` | `wow-{botGuid}-{playerGuid}` | Key template, supports two placeholders |
| `SkipHistoryWhenSession` | `1` | When sessions are on, don't re-send local history (saves tokens) |

New command: `.ollama gateway status` (SEC_ADMINISTRATOR) — prints enabled
flag, configured bots, whitelist size, request totals, per-bot request counts
with last-seen times. Runtime stats and cooldown map reset on `.ollama reload`.

Behavior change: every gateway request now logs prompt/completion token counts
on a dedicated `LOG_INFO` line.

#### Streaming responses (PR2)

| Key | Default | Purpose |
|---|---|---|
| `UseStreaming` | `0` | Send `stream: true`, consume SSE |
| `StreamWhisperOnSentence` | `1` | Emit at sentence boundaries (else size-only) |

Added `OllamaHttpClient::PostStream` for SSE consumption and a sibling
`QueryGatewayAPIStreaming` in the gateway module.

#### Per-agent presets, personality merge, language detection (PR3)

| Key | Default | Purpose |
|---|---|---|
| `AgentByPersonality` | `""` | `personality=model\|...` mapping (e.g. `hostile=agent:roastmaster`) |
| `MergePersonalityPrompt` | `1` | Prepend bot's personality prompt to the gateway system prompt |
| `DetectLanguage` | `0` | Heuristic Cyrillic detection → inject language hint |
| `LanguageHintTemplate` | `Always respond to the user in {lang}.` | Template for the hint |

Model resolution precedence: per-bot override > personality preset > global default.

#### Channels, mention mode, player controls (PR4)

| Key | Default | Purpose |
|---|---|---|
| `AllowedChannels` | `whisper` | csv of `whisper, party, raid, guild, officer, say, yell, general` |
| `PublicChannelMode` | `keyword` | Gating on say/yell/general: `keyword`, `mention`, `both` |

Gateway thread now routes responses to the matching channel
(`Whisper` / `SayToParty` / `SayToGuild` / `Say` / `Yell`).

New SQL migration `2026_04_18_optouts_and_mutes.sql`:
- `mod_ollama_chat_optouts (account_id, opt_out_at)`
- `mod_ollama_chat_bot_mutes (account_id, bot_guid, muted_at)`

New player commands (SEC_PLAYER):
- `.ollama optout` / `.ollama optin` — account-wide toggle
- `.ollama mute <BotName>` / `.ollama unmute <BotName>` — per-bot suppression

Handler now short-circuits ALL bot responses (gateway AND Ollama) when
`ShouldSuppressBotResponse(accountId, botGuid)` returns true.

#### JSON config refactor + per-bot custom headers (PR5)

New key: `OllamaChat.Gateway.BotOverridesJson`.

Legacy `BotOverrides = "GUID:URL:TOKEN:MODEL|..."` format still works (a
warning is logged if both keys are set). JSON format:

```json
[
  {
    "guid": 12345,
    "url": "http://host:port/v1/chat/completions",
    "token": "bearer-token",
    "model": "agent:lore-master",
    "headers": {"x-synthiq-agent": "lore"},
    "tools": ["get_zone_info", "whisper_player"]
  }
]
```

- `headers` — merged into every request for this bot
- `tools` — per-bot allowlist (consumed by PR6)

#### Backend type: openclaw vs synthiq (PR6 prereq)

New config key `OllamaChat.Gateway.Type` (`openclaw` | `synthiq`, default `openclaw`)
and a per-bot `gatewayType` field in `BotOverridesJson`.

Rationale: OpenClaw's `/v1/chat/completions` is a thin compatibility shim over a
full-fat agent that has its own baked-in tool registry; it ignores any `tools[]` we
pass in the request body. Synthiq is a direct Claude Agent SDK wrapper and honors
request-body `tools[]`. So `EnableToolUse` only fires when the resolved type is
`synthiq`; openclaw bots always use the plain / streaming path.

#### Tool use bridge (PR6)

| Key | Default | Purpose |
|---|---|---|
| `EnableToolUse` | `0` | Send OpenAI `tools[]`, dispatch `tool_calls` |
| `MaxToolIterations` | `3` | Recursion guard on the tool-call loop |
| `AllowedTools` | `""` | Global csv allowlist; empty = all registered tools |

Built-in tools (all implemented in C++, running on the worldserver thread):

| Tool | Input | Output |
|---|---|---|
| `get_bot_state` | _none_ | name, level, class, race, zone, area, hp_pct |
| `get_player_state` | `name?` | same for a named player; defaults to the whisperer |
| `get_zone_info` | _none_ | zone/area names, map id, x/y/z |
| `whisper_player` | `name`, `message` | whisper to another online player (max 200 chars) |

Streaming + tool use are mutually exclusive — `EnableToolUse` wins. Extending
the SSE parser to accumulate tool_calls is a follow-up.

#### Audit + admin commands + action markers (PR7)

New SQL migration `2026_04_18_gateway_audit.sql`:
- `mod_ollama_chat_gateway_audit (id, bot_guid, player_guid, account_id, ts,
  request_chars, response_chars, prompt_tokens, completion_tokens, latency_ms,
  source_channel, error)`

New config keys:

| Key | Default | Purpose |
|---|---|---|
| `EnableAudit` | `0` | Persist every call to the audit table |
| `AuditRetentionDays` | `30` | Auto-prune older rows on startup and `.ollama gateway prune` |
| `EnableActionMarkers` | `0` | Parse `[emote:bow]` etc. (currently a logging stub — playerbots action binding deferred) |

New admin commands:
- `.ollama gateway test <BotName> <prompt>` — async-fire a gateway call and log the response
- `.ollama gateway costs [hours=24]` — aggregate audit rows for the last N hours (total + top-10 bots)
- `.ollama gateway prune` — force-prune audit rows older than the retention window

### Changed

- Whisper-only constraint on gateway is lifted; `AllowedChannels` is now the
  source of truth. Default stays `whisper` → no behavior change unless opted in.
- Fallback semantics split across two keys: `FallbackToOllama` handles missing
  keyword (existing), `FallbackOnError` handles API errors (new).
- `OllamaHttpClient` now includes a `PostStream` API. Unchanged `Post` path still
  used when streaming is off.
- `.ollama reload` now also reloads player preferences and resets gateway runtime
  state (stats, cooldown map).

### New files

- `src/mod-ollama-chat_playerprefs.{h,cpp}` — opt-out / mute storage
- `src/mod-ollama-chat_tools.{h,cpp}` — gateway tool registry + dispatch
- `data/sql/characters/base/2026_04_18_optouts_and_mutes.sql`
- `data/sql/characters/base/2026_04_18_gateway_audit.sql`
- `docs/gateway.md` — full gateway feature documentation
- `docs/README.md` — docs directory index
- `docs/CHANGELOG.md` — this file

### Action tools via PlayerbotAI::HandleCommand (PR8c)

Adds 6 write-action tools that dispatch through `PlayerbotAI::HandleCommand`
(verified entry point at `mod-playerbots/src/Bot/PlayerbotAI.cpp:912`):

- `bot_follow`, `bot_stay` — movement
- `bot_emote` — text emote (alphanumeric, sanitised against injection)
- `bot_invite_to_group` — invite a player to the bot's group
- `bot_use_item`, `bot_cast` — inventory + spellbook activation by id

**Hard guardrails:**
- `OllamaChat.Mcp.AllowActionTools=0` (default) → all action tools return an
  error immediately. The tools are still surfaced via `tools/list` so the agent
  knows what's possible.
- `OllamaChat.Mcp.ActionRateLimitPerBotPerMinute=6` (default) → per-(bot, tool)
  trailing-60s window, dropped silently above the cap. `0` disables.
- Every action call writes a `mod_ollama_chat_gateway_audit` row with
  `source_channel='mcp:action'` when `EnableAudit=1`. `.ollama gateway costs`
  picks it up automatically — same view as gateway chat usage.
- Emote names are sanitised to `[a-zA-Z0-9_]+` to prevent command injection back
  through the playerbots chat parser.

`whisper_player` (PR8a) was the only write tool before; PR8c brings the count to 7.

### Read-only MCP tools (PR8b)

Adds 7 new tools to the gateway tool registry, all reachable through the MCP
server (`mcp__wow-worldserver__<name>` from the agent's view):

- `get_player_gear` — equipped items per slot + aggregate average iLvl
- `get_player_quests` — active quest log (incomplete + complete-not-turned-in)
- `get_nearby_players` — players within N yards of the bot, with `is_bot` flag
- `get_group` — bot's party/raid composition
- `get_target` — bot's current target
- `get_inventory` — bag contents aggregated by item id
- `query_world_db_lookup` — whitelisted world-db lookups (`item_template`,
  `quest_template`)

All read-only, no playerbots dependency; uses direct `Player*` C++ APIs.

### Embedded MCP server (PR8a)

PR6's request-body `tools[]` path was experimentally proven dead against both
upstream gateways. PR8a adds a **working** transport: the worldserver runs an
embedded HTTP MCP (Model Context Protocol) server that the upstream agent SDK
loads via its `.mcp.json` config.

New file: `src/mod-ollama-chat_mcpserver.{h,cpp}` (~290 LOC).

New config keys (all `OllamaChat.Mcp.*`, default off):
- `Enable`, `BindAddress`, `Port`, `BearerToken` — listener config
- `InjectContextHint` — prepend a one-line `botGuid`/`playerGuid` hint to the
  gateway system prompt so the agent passes them when calling MCP tools
- `AllowActionTools`, `ActionRateLimitPerBotPerMinute`, `AllowedToolsExtra` —
  reserved for PR8b/PR8c

Endpoints:
- `POST /mcp` — JSON-RPC 2.0: `initialize`, `tools/list`, `tools/call`, `notifications/*`
- `GET /mcp/health` — unauthenticated liveness probe

Reuses `GetGatewayToolRegistry()` and `DispatchGatewayTool()` from `_tools.cpp`
unchanged. The same 4 tools (`get_bot_state`, `get_player_state`, `get_zone_info`,
`whisper_player`) are now reachable via MCP. PR8b adds 6–8 more read-only tools.

Behavior changes:
- `.ollama reload` now also restarts the MCP listener (so Bearer-token rotation
  takes effect without a worldserver restart).
- `.ollama gateway status` includes a new `mcp:` line with listener state and
  request/tool-call/unauthorized counters.

Wiring: add a `wow-worldserver` entry to `~/.synthiq/.mcp.json` on the host
running the upstream agent gateway. See `docs/gateway.md`.

### Upgrade notes

1. **Source the new SQL migrations** (picked up automatically on `docker compose
   up -d` once the `ac-db-import` image is rebuilt):
   - `2026_04_18_optouts_and_mutes.sql`
   - `2026_04_18_gateway_audit.sql`

2. **Existing deployments keep working unchanged.** Every new feature is opt-in
   (off by default) except two light-touch protections turned on by default:
   - `MaxConcurrentRequests = 4`
   - `MinSecondsBetweenRequests = 5`

   If you need to disable either, set them to `0` in your conf.

3. **On-disk config churn**: at server startup you'll see `Missing property` lines
   for each new setting. Those are warnings only — defaults kick in. To silence,
   copy the updated `conf/mod_ollama_chat.conf.dist` over
   `env/dist/etc/modules/mod_ollama_chat.conf` and re-apply local edits.

4. **`BotOverrides` migration**: the legacy colon-split format still works.
   Migrating to `BotOverridesJson` is optional but recommended for any deployment
   that (a) uses URLs with explicit ports, or (b) wants per-bot custom headers or
   tool allowlists.

---

For older changes see `git log` — this file was introduced with the gateway
expansion batch.
