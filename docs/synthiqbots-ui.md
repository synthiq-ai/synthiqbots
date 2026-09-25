# synthiqbots-ui

Vendored WoW client UI for the mod-playerbots module, forked from
[Macx-Lio/MultiBot](https://github.com/Macx-Lio/MultiBot).

Lives at [`addons/synthiqbots-ui/`](../addons/synthiqbots-ui/) — see that
directory's [`README.md`](../addons/synthiqbots-ui/README.md) for the
end-user-facing install/usage doc and
[`UPSTREAM.md`](../addons/synthiqbots-ui/UPSTREAM.md) for the rename mapping
and forward-merge recipe.

## What it is

A client-side Lua addon that gives the human player a toolbar UI for driving
playerbot commands without typing them. Buttons cover bot lifecycle (add /
remove / spawn), movement (follow / stay), combat (attack / flee / mode),
inventory inspection, talent inspection, raid coordination, gem-portal memory,
and more.

It is **not** part of the AzerothCore CMake build — the C++ `mod-ollama-chat`
module is the server-side counterpart. The two are independent:
the addon talks to the server only via in-game chat (whispers, party, raid,
say, yell), exactly the same as a human player would.

The addon also drives the in-game feedback-capture flow (`/sb feedback`,
`Feedback` MultiBar button), which writes screenshots + chat + context into
SavedVariables for an external companion daemon to ship to the gateway agent.
Full design: [`feedback-loop.md`](feedback-loop.md).

## Why we vendor it

Upstream is GPLv3-licensed. We carry our own copy for three reasons:

1. **Branding.** Project namespace is `SynthiqBotsUI` / `synthiqbots-ui`.
   Renamed everything mechanically (folder, `.toc`, slash commands,
   SavedVariables, Lua global identifier, texture paths). Provenance pinned
   in `UPSTREAM.md`.
2. **Forward fixes.** Carrying the addon in-tree means we own the patch
   surface; no submodule dance, no "was this ever upstreamed" questions.
3. **One specific fix.** See "Auto-Chat toggle" below.

## Auto-Chat toggle

Upstream MultiBot's `CHAT_MSG_WHISPER` auto-handler reacts to incoming bot
whispers — when a bot says "Hello", the addon whispers `co ?` and `nc ?` back
to handshake. Same pattern for graveyard auto-summon, stats refresh, and
inventory-after-loot.

When the server-side tactical agent (`src/mod-ollama-chat_tactical.cpp`) is
active, it occasionally whispers the player from a bot. Upstream's auto-handler
treats that whisper like any other bot greeting and fires `co ?` back. The
tactical layer parses the bot's reply, re-engages, and the loop tightens.

The vendored fork adds a single persistent boolean,
`SynthiqBotsUISave["AutoChatCommands"]` (default **ON**), that gates the
auto-handler's outbound chat sends. UI button clicks (Attack, Follow, Stay,
Disperse, ...) are unaffected — only the reactive auto-fires are silenced.

### Toggle controls

| Surface | What it does |
|---|---|
| MultiBar Main-column button (icon `spell_holy_silence`) | Click flips ON ↔ OFF. Visual matches state. |
| `/sb chat on` | Force ON. |
| `/sb chat off` | Force OFF. |
| `/sb chat status` | Print current state. |
| Aliases: `/synthiqbots chat ...`, `/sbots chat ...` | Same as `/sb chat ...`. |

The toggle persists per-character via the `SynthiqBotsUISave` SavedVariables
table.

### What's gated, exactly

When the toggle is OFF, the helper `SynthiqBotsUI.sendBotChat()` short-circuits
17 outbound `SendChatMessage` call sites in `SynthiqBotsUIHandler.lua`:

- 15 sites inside `CHAT_MSG_WHISPER` (graveyard summon, stats reply, Hello
  handshake, `co ?` / `nc ?` chain, who, ss ?, items requests, ...).
- 1 site inside `CHAT_MSG_LOOT` (auto-`items` after a bot loots, when the
  inventory window is open).
- 1 site inside `TRADE_CLOSED` (auto-`items` after a trade closes).

The two `SendChatMessage` calls inside `tButton.doRight` / `tButton.doLeft`
button-click closures are intentionally **not** gated — those fire on
explicit user clicks, not auto-handler events.

`MultiBotEngine.lua` and `MultiBotInit.lua` send paths (the actual button
implementations) are also untouched — they bypass the gate so the UI keeps
working when chat is OFF.

## Install

```
cp -R addons/synthiqbots-ui <wow-client>/Interface/AddOns/synthiqbots-ui
```

Or symlink for live development:

```
ln -s "$(pwd)/addons/synthiqbots-ui" <wow-client>/Interface/AddOns/synthiqbots-ui
```

`/reload` in-game after either.

## Compatibility

- WoW client: 3.3.5a (interface 30300). Verified by upstream against American
  and German clients.
- Server: any AzerothCore build with [mod-playerbots](https://github.com/liyunfan1223/mod-playerbots).
  This addon is **client-only** — it does not require the `mod-ollama-chat`
  server module to function. With the server module installed, the tactical
  agent's whisper traffic is what motivates the Auto-Chat toggle.

## Verification recipe

After installing, confirm the toggle works:

1. `/reload`. Watch `<wow-client>/Logs/FrameXML.log` for parse errors.
2. `/sb chat status` → prints `Auto chat: ON` (default).
3. With a real bot in your party, whisper it "Hello" → addon registers it
   on the MultiBar and whispers `co ?` back.
4. `/sb chat off`.
5. Whisper the bot "Hello" again → MultiBar still creates the bot's button,
   but no `co ?` whisper goes out (verify in your chat log).
6. Click the new MultiBar Main-column toggle button → the visual flips back
   to enabled and `auto chat: ON` returns.
7. `/reload` → the saved state persists.

## Migration from upstream MultiBot

The rename `MultiBotSave` → `SynthiqBotsUISave` means existing per-character
SavedVariables don't carry over. Window positions, gem links, AutoRelease
state, etc., reset on first run.

If you used upstream MultiBot, the old save lives at:

```
<wow-client>/WTF/Account/<account>/<realm>/<character>/SavedVariables/MultiBot.lua
```

You can manually copy values from there into the corresponding
`synthiqbots-ui.lua` after the new addon's first run.

## License

The addon is GPLv3 (carried verbatim in `addons/synthiqbots-ui/LICENSE`). The
combined work in this repo is AGPL-3.0 (see top-level `LICENSE` and
[`THIRD_PARTY_LICENSES.md`](../THIRD_PARTY_LICENSES.md) for the compatibility
note). Original author: Nico Löbbert.
