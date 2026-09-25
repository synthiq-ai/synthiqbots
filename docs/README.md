# mod-ollama-chat docs

Operator and developer documentation for mod-ollama-chat, the AzerothCore module
that lets player bots speak via Ollama and/or an external OpenAI-compatible agent
gateway.

## Contents

- **[gateway.md](./gateway.md)** — Gateway agent bot feature. Config reference,
  setup walkthrough, GM commands, tuning, troubleshooting. Covers all seven
  feature tiers (hardening, streaming, per-agent presets, channels, JSON config,
  tool use, audit).
  `warcraft` model sentinel on the Synthiq `cloud` gateway: a dedicated low-latency
  in-game-bot profile (lean context + `wow-*` tools). Endpoint, per-bot override
  config, model resolution, verification.
- **[CHANGELOG.md](./CHANGELOG.md)** — Chronological summary of notable changes
  and new features per release.

## Where else to look

- `conf/mod_ollama_chat.conf.dist` — full commented config reference. Every
  setting has a description and a default. The `.dist` file is the source of
  truth; the running config in `env/dist/etc/modules/mod_ollama_chat.conf` is a
  deploy-time copy.
- `data/sql/characters/base/*.sql` — schema migrations. Applied automatically by
  the AzerothCore `ac-db-import` container on `docker compose up -d`.
- `src/` — C++ source. Key files:
  - `mod-ollama-chat_gateway.{h,cpp}` — gateway routing, streaming, tool loop
  - `mod-ollama-chat_tools.{h,cpp}` — built-in tool registry
  - `mod-ollama-chat_playerprefs.{h,cpp}` — opt-out / mute storage
  - `mod-ollama-chat_handler.cpp` — chat interception, bot selection, dispatch
  - `mod-ollama-chat_command.cpp` — `.ollama` GM and player commands
