# Implementation Plan: Proactive Leader Bot

**Branch**: `002-proactive-leader-bot` | **Date**: 2026-05-11 | **Spec**: [spec.md](spec.md)
**Input**: Feature specification from `/specs/002-proactive-leader-bot/spec.md`

## Summary

Invert the existing leader/follower dynamic so a designated gateway leader bot opens the conversation, proposes a concrete next activity (quest or dungeon — battleground deferred), waits for explicit player approval, executes via existing playerbot primitives, and stays in proximity. Idle nudges fill silence without spamming. All proactive output reuses the existing tactical heartbeat for cadence, the existing Ollama classifier for yes/no intent, the existing gateway path for natural-language phrasing, and `mod_ollama_chat_gateway_audit` for the lifecycle audit trail. No new SQL migration, no new MCP tool, no new movement planner.

## Technical Context

**Language/Version**: C++17 (AzerothCore worldserver module standard)
**Primary Dependencies**: AzerothCore WoTLK 3.3.5a, mod-playerbots (sister module — provides `accept *`, `talk *`, follow/stay, party invite, zone-in), this module's existing `_tactical.cpp` heartbeat, `_gateway.cpp` LLM/classifier path, `_events.cpp` event capture, `_handler.cpp` player-chat intercept, `nlohmann::json`, cpp-httplib
**Storage**: `acore_characters.mod_ollama_chat_gateway_audit` (reused, new `source_channel` labels) — no new SQL migration. In-memory only for open-proposal state and cadence (consistent with FR-016: state does NOT survive worldserver restart).
**Testing**: Live in-world smoke test via `game-host` deployment + `wow-admin` MCP tools + `wow-ops-api` audit queries. Unit tests are non-idiomatic for AzerothCore module code; integration is via observable bot behavior + audit-row inspection. A scripted "fake player" smoke harness lives in `quickstart.md`.
**Target Platform**: Linux container (`ac-worldserver` Docker image, deployed via GHA → game-host VM)
**Project Type**: C++ AzerothCore worldserver module (single `src/` tree). No frontend/backend split.
**Performance Goals**: Per spec SC-001/SC-005: first proposal ≤60s warm / ≤120s cold; proactive chat volume stays under existing per-bot ambient cap (`AmbientMaxVisibleActionsPerMinute`); zero unapproved executions; ≥95% proximity adherence outside active-travel windows. Tactical heartbeat already runs at `Tactical.HeartbeatMs=10000` — this feature adds no new clock.
**Constraints**: No new MCP tool/verb; no new playerbot strategy; no new SQL schema. Deploy only via CI/CD per project's `CLAUDE.md`. Feature kill-switchable via one config flag (FR-018). All proactive output rate-limited under the existing tactical ambient caps (FR-019).
**Scale/Scope**: One designated leader-voice per player. The current fleet (3 gateway bots: Claude/Clawd/Geek) means at most 3 leader-capable bots in range; v1 only ever elects one as the voice. Per-player open-proposal cardinality is exactly 0 or 1. Module-wide impact: one new `src/mod-ollama-chat_proactive.{cpp,h}` pair (~400-600 LOC est.) plus tap-in calls from `_tactical.cpp`, `_handler.cpp`, `_events.cpp`; one new prompts file `prompts/proactive_proposer.md`; ~10 new `OllamaChat.Proactive.*` config keys in `mod_ollama_chat.conf.dist`.

## Constitution Check

*GATE: Must pass before Phase 0 research. Re-check after Phase 1 design.*

`.specify/memory/constitution.md` is the unfilled placeholder template (all `[PRINCIPLE_*]` slots still literal). No project constitution has been ratified yet, so no project-specific gates apply.

**Working principles inherited from this repo's `CLAUDE.md` and the project's "personal project — no corporate systems" rules** that I will hold this plan to in place of a formal constitution:

- **Deploy only via CI/CD.** Never `ssh game-host "docker compose up -d --build"`. The plan adds source files + config keys only; deployment is the GHA `deploy.yml` workflow.
- **Reuse existing primitives.** Spec assumption is no new MCP tool / playerbot strategy / SQL migration in v1. The plan must satisfy that.
- **No new external dependencies.** All work happens inside the existing C++ module + existing prompts directory.
- **Worktree-slot discipline.** This plan is being written from the main checkout on branch `002-proactive-leader-bot`. Final implementation may move to a worktree slot; that's an execution detail, not a plan concern.
- **No corporate systems.** No Jira keys, no Confluence links, no cloud secret managers — tracking lives in this repo only.

**Gate result**: PASS (no real gates, no violations to record).

## Project Structure

### Documentation (this feature)

```text
specs/002-proactive-leader-bot/
├── plan.md                 # This file
├── spec.md                 # Feature spec (clarified 2026-05-11)
├── research.md             # Phase 0 output (this command)
├── data-model.md           # Phase 1 output (this command)
├── quickstart.md           # Phase 1 output (this command)
├── contracts/
│   ├── config-keys.md      # New OllamaChat.Proactive.* config schema
│   ├── audit-events.md     # New mod_ollama_chat_gateway_audit source_channel labels
│   └── prompt-contract.md  # Inputs/outputs for the proposer + classifier prompts
└── checklists/
    └── requirements.md     # Spec quality checklist (already passing)
```

### Source Code (repository root — existing tree)

```text
src/                                     # Existing C++ module sources
├── mod-ollama-chat_main.cpp             # (touch) — register OnPlayerLogin/OnPlayerLogout for proactive lifecycle
├── mod-ollama-chat_config.cpp           # (touch) — load new OllamaChat.Proactive.* keys
├── mod-ollama-chat_config.h             # (touch) — extern g_Proactive* globals
├── mod-ollama-chat_handler.cpp          # (touch) — call ProactiveOnPlayerChat() from PlayerBotChatHandler
├── mod-ollama-chat_events.cpp           # (touch) — call ProactiveOnActivityEvent() on quest turn-in / zone change
├── mod-ollama-chat_tactical.cpp         # (touch) — call ProactiveEvaluateTick(bot, player) once per heartbeat per (player, designated-voice bot)
├── mod-ollama-chat_gateway.cpp          # (touch only if classifier prompt needs a new intent class label)
├── mod-ollama-chat_proactive.cpp        # NEW — state machine, proposal lifecycle, candidate selection, voice election, proximity check, idle nudge, completion detection wiring, audit insert
└── mod-ollama-chat_proactive.h          # NEW — public surface for the new tap-in calls above

prompts/                                 # Existing prompts directory
├── proactive_proposer.md                # NEW — phrasing-only prompt for proposals/waiting lines/idle nudges
└── ollama_classifier.md                 # (touch) — add proactive_approve / proactive_reject / proactive_done intent classes

conf/
└── mod_ollama_chat.conf.dist            # (touch) — ~10 new OllamaChat.Proactive.* keys

docs/
├── proactive-leader.md                  # NEW — runbook (configuration, troubleshooting, how to interpret audit rows)
├── CHANGELOG.md                         # (touch) — feature entry
└── agent-context/ARCHITECTURE.md        # (touch) — short line under _proactive component in the source-layout table
```

**Structure Decision**: Existing single C++ module. One new source file pair (`_proactive.{cpp,h}`) following the established `mod-ollama-chat_<component>` naming. No new directory. Tap-in calls from existing components stay one-line forwarding stubs so each existing file's blast radius is small. The lifecycle owner is the new `_proactive.cpp`; everywhere else is a call site.

## Complexity Tracking

> No Constitution Check violations to justify. This section is intentionally empty.

| Violation | Why Needed | Simpler Alternative Rejected Because |
|-----------|------------|-------------------------------------|
| _none_ | — | — |
