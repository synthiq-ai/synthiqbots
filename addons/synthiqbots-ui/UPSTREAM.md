# Upstream provenance

This addon is a vendored fork of:

- **Upstream:** [Macx-Lio/MultiBot](https://github.com/Macx-Lio/MultiBot)
- **Pinned commit:** `ecee413dacfa93ee705ede83f308f3a0396e6ece`
- **License:** GNU General Public License v3 (see [`LICENSE`](LICENSE))
- **Original author:** Nico Löbbert

## Local changes vs upstream

### Rename (mechanical, length-ordered)

The entire codebase was renamed `MultiBot` → `SynthiqBotsUI` / `synthiqbots-ui` to match the project's branding. The full mapping:

| Token | Upstream | Vendored |
|---|---|---|
| Folder name | `MultiBot/` | `synthiqbots-ui/` |
| TOC filename | `MultiBot.toc` | `synthiqbots-ui.toc` |
| Lua file prefix | `MultiBot*.lua` | `SynthiqBotsUI*.lua` |
| Global Lua identifier | `MultiBot` | `SynthiqBotsUI` |
| SavedVariablesPerCharacter | `MultiBotSave` | `SynthiqBotsUISave` |
| SavedVariables | `MultiBotGlobalSave` | `SynthiqBotsUIGlobalSave` |
| Slash registry | `SLASH_MULTIBOT*`, `SlashCmdList["MULTIBOT"]` | `SLASH_SYNTHIQBOTS*`, `SlashCmdList["SYNTHIQBOTS"]` |
| Slash strings | `/multibot`, `/mbot`, `/mb` | `/synthiqbots`, `/sbots`, `/sb` |
| Texture path | `Interface\AddOns\MultiBot\...` | `Interface\AddOns\synthiqbots-ui\...` |
| Section comments | `-- MULTIBOT:FRAME --` etc. | `-- SYNTHIQBOTS:FRAME --` |

The rename was applied length-first via a Python script (longer tokens replaced before shorter ones to avoid double-substitution). Verification: `grep -irn "multibot" addons/synthiqbots-ui/` returns 0 hits outside this file.

### Auto-Chat toggle

A persistent boolean `SynthiqBotsUISave["AutoChatCommands"]` (default **true**) was added to gate the addon's reactive auto-handler chat sends. When **false**, the addon stops auto-firing `co ?`, `nc ?`, `summon`, `stats`, `items`, etc. on incoming bot whispers, loot events, and trade events. UI button-driven sends (Attack, Follow, Stay, ...) are unaffected.

Changes:

- `SynthiqBotsUI.lua` — added `SynthiqBotsUI.auto.botCommands = true` to the runtime-flags init block.
- `SynthiqBotsUIHandler.lua` — added the `SynthiqBotsUI.sendBotChat(msg, type, lang, target)` chokepoint helper. 17 `SendChatMessage(...)` call sites inside `CHAT_MSG_WHISPER` (15), `CHAT_MSG_LOOT` (1), and `TRADE_CLOSED` (1) now route through this helper. The two `SendChatMessage` calls inside `tButton.doRight` / `tButton.doLeft` button-click closures are intentionally left untouched. Save in `PLAYER_LOGOUT`, restore (with button-visual sync) in `ADDON_LOADED`.
- `SynthiqBotsUIInit.lua` — added a new MultiBar Main-column button at `y=408` (icon `spell_holy_silence`, started in the enabled state to match the default-ON flag), mirroring the existing `Release` toggle.
- `SynthiqBotsUI.lua` — added `SynthiqBotsUI.tips.main.autochat` tooltip text.
- `SynthiqBotsUIHandler.lua` — extended `SlashCmdList["SYNTHIQBOTS"]` to parse a `chat on|off|status` subcommand and update both the runtime flag and the SavedVariable.

The motivation is interaction with a server-side tactical agent (`mod-ollama-chat`'s `_tactical.cpp`). When the agent whispers a `Hello` to the player, upstream MultiBot reacts by whispering `co ?` back, which the tactical layer re-receives and re-triggers, forming a feedback loop. Turning the toggle OFF breaks the loop without disabling the rest of the UI.

### Feedback capture

A new `SynthiqBotsUIFeedback.lua` module (added to `synthiqbots-ui.toc` after `SynthiqBotsUIRaidus.lua`) implements `/sb feedback <note>` and the `inv_misc_note_01` Main-column button at `y=442`. It captures `Screenshot()` + a 200-line chat ring buffer + a context snapshot (zone, target, group, char identity) into `SynthiqBotsUISave.feedback_queue[]` for an external companion daemon to upload to the gateway agent. Full design: [`docs/feedback-loop.md`](../../docs/feedback-loop.md).

Changes:

- `SynthiqBotsUIFeedback.lua` — new file. Self-contained event frame for chat capture (does not piggyback on the main `SynthiqBotsUI` dispatcher) + `SynthiqBotsUI.feedback` namespace + ring buffer + queue.
- `synthiqbots-ui.toc` — appended `SynthiqBotsUIFeedback.lua` after `SynthiqBotsUIRaidus.lua`.
- `SynthiqBotsUIInit.lua` — added a `Feedback` button in the Main column at `y=442`, right after `AutoChat` (`y=408`).
- `SynthiqBotsUI.lua` — added `SynthiqBotsUI.tips.main.feedback` tooltip text.
- `SynthiqBotsUIHandler.lua` — added a `feedback` branch to the `SlashCmdList["SYNTHIQBOTS"]` dispatcher that re-parses the original `msg` so the user's note keeps its casing (the existing dispatcher lowercases `tRest`).

## Migration from upstream MultiBot

Upstream's per-character UI saves (`MultiBotSave`) are **not** auto-migrated to `SynthiqBotsUISave`. A returning user will see a fresh save table — window positions, gem links, and AutoRelease state reset. The addon will recreate its defaults on first run.

If you used upstream MultiBot previously, you can manually copy values from the old SavedVariables file (`<wow-client>/WTF/Account/<account>/<realm>/<character>/SavedVariables/MultiBot.lua`) into the new `synthiqbots-ui.lua` save file in the same directory after the first run.

## Future upstream merges

To pull a fix or feature from upstream:

1. `git -C /tmp clone https://github.com/Macx-Lio/MultiBot.git`
2. `git -C /tmp/MultiBot diff <pinned-sha> HEAD -- '*.lua' '*.toc' '*.md'`
3. Apply the diff manually, sed-mapping `MultiBot` → `SynthiqBotsUI` (and the related tokens above) as you go.
4. Re-run the phantom-name sweep: `grep -irn "multibot" addons/synthiqbots-ui/` → 0 hits outside `LICENSE` and this file.
5. Bump the pinned SHA at the top of this file.
