# AGENTS.md

Guidance for coding agents (Claude Code, Codex, Cursor) working in this repository. `CLAUDE.md` is a symlink to this file — edit here.

## Quick Ref

C++ AzerothCore module integrating LLM chat (Ollama or any OpenAI-compatible endpoint) and gateway-routed agent bots (Synthiq/OpenClaw) with mod-playerbots. Built inside AzerothCore's CMake tree from `modules/mod-ollama-chat`. Side services live in `apps/` (Go: `ops-api`, `feedback-daemon`), client tools in `tools/`, WoW client addons in `addons/`.

This repository is the **public mirror** of the Synthiq fork. Deployment workflows, host runbooks and operator-specific configuration are kept in a private repository and are not published here.

## Documentation Index

Pick the doc that matches your task:

| Need | Go to |
|------|-------|
| Build / dependencies | [docs/agent-context/BUILD.md](docs/agent-context/BUILD.md) |
| Source layout, gateway/Ollama data flow, DB tables, concurrency | [docs/agent-context/ARCHITECTURE.md](docs/agent-context/ARCHITECTURE.md) |
| Deployment notes (CI/CD lives in the operator's private repo) | [docs/agent-context/DEPLOYMENT.md](docs/agent-context/DEPLOYMENT.md) |
| Coding conventions (ScriptMgr, fmt, log macros) | [docs/agent-context/CODING-CONVENTIONS.md](docs/agent-context/CODING-CONVENTIONS.md) |
| Gateway feature reference | [docs/gateway.md](docs/gateway.md) |
| Tactical mode runbook | [docs/tactical.md](docs/tactical.md) |
| Proactive leader bot runbook | [docs/proactive-leader.md](docs/proactive-leader.md) |
| Voice-command the leader bot (talk_to_leader + push-to-talk client) | [docs/voice-command.md](docs/voice-command.md) |
| Voice-command desktop app (Mac DMG / Windows EXE, CustomTkinter UI) | [docs/voice-command-app.md](docs/voice-command-app.md) |
| Ops API runbook | [docs/ops-api.md](docs/ops-api.md) |
| In-game capture (tactical maps / observer screenshots / video) | [docs/capture.md](docs/capture.md) |
| Autonomous bots | [docs/autonomous-bots.md](docs/autonomous-bots.md) |
| Any playerbot you talk to / invite gets the strategic brain (bot promotion) | [docs/bot-promotion.md](docs/bot-promotion.md) |
| jev decision tier (TypeSafe System One in front of the fast paths) | [docs/jev.md](docs/jev.md) |
| E2E harness: a headless human logs in and tests the fleet features | [docs/e2e.md](docs/e2e.md) |

## Build

Built as part of AzerothCore from `modules/mod-ollama-chat`. No standalone build. Full instructions and dependencies: [docs/agent-context/BUILD.md](docs/agent-context/BUILD.md).

## Architecture

Source layout (per-component table), gateway-vs-Ollama data flow, configuration, DB tables, concurrency model, global state: [docs/agent-context/ARCHITECTURE.md](docs/agent-context/ARCHITECTURE.md).

## Coding Conventions

AzerothCore ScriptMgr pattern, `LOG_INFO`/`LOG_DEBUG`/`LOG_ERROR` macros, `fmt::format` (not `printf`/`std::format`), `OllamaChat.SectionName` config keys, `ChatChannelSourceLocal` enum for channel routing. Full list: [docs/agent-context/CODING-CONVENTIONS.md](docs/agent-context/CODING-CONVENTIONS.md).

A few engine traps worth knowing before you touch the code:

- **Gateway worker threads read the per-bot config maps unlocked.** Never mutate `g_GatewayBotGUIDSet` / `g_GatewayBotConfigs` outside config (re)load; runtime state (e.g. bot promotion) needs its own mutex-guarded registry, read through `FindGatewayBotConfig()`.
- **`ObjectAccessor::FindPlayer` is not lifetime-safe off the world thread.** Resolve names/state on the world thread (`worldtask.h`) and pass values to worker threads.
- **`sWorld->get<Type>Config(KEY)` with the wrong type is a runtime ASSERT, not a compile error.** Match `getBoolConfig` / `getIntConfig` / `getFloatConfig` to the key's declared type in `WorldConfig.cpp`.
- **mod-playerbots clears the master of an ungrouped random bot every AI tick.** Anything that needs a bot to follow or obey must put it in a group first.

## Agent Workflow

- Check `git status --short` before editing and preserve unrelated changes.
- Read the relevant source and docs before changing behavior.
- Write docs for new features under `docs/`.
- Never commit secrets: tokens, API keys and passwords belong in your local `mod_ollama_chat.conf` / `gateway_overrides.json`, never in `conf/*.dist` or docs.
