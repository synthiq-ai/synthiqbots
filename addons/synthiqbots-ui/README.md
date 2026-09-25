# synthiqbots-ui

WoW client UI for the [mod-playerbots](https://github.com/liyunfan1223/mod-playerbots) module, vendored into this repo as a fork of [Macx-Lio/MultiBot](https://github.com/Macx-Lio/MultiBot).

Provides a toolbar of buttons that drive playerbot commands client-side: attack, follow, stay, flee, masters, beast-master, summon, raid coordination, inventory, talent inspection, and more.

## Install

Copy or symlink this directory into your WoW client's `Interface/AddOns/`:

```
<wow-client>/Interface/AddOns/synthiqbots-ui/
```

The addon targets WoW 3.3.5a (interface 30300).

## Usage

In-game slash commands (any of):

```
/synthiqbots
/sbots
/sb
```

Bare command toggles the MultiBar on/off. Subcommands:

```
/sb chat status       — print current Auto-Chat state
/sb chat on           — enable addon's reactive chat sends
/sb chat off          — disable (recommended when running a server-side tactical agent)

/sb feedback <note>   — capture screenshot + chat + context for the gateway agent
/sb feedback log      — list recent feedback entries + agent replies
/sb feedback help     — usage
```

There are two UI buttons on the MultiBar's Main column:

- `spell_holy_silence` — toggles Auto-Chat (above)
- `inv_misc_note_01` — captures feedback with no note (same as bare `/sb feedback`)

## Auto-Chat toggle

The `AutoChat` toggle gates the addon's outbound chat sends inside its **auto-handler** — i.e., when the addon reacts to incoming bot whispers, loot events, or trade events and auto-fires `co ?`, `nc ?`, `summon`, `stats`, `items`, etc. back at the bot.

| State | Behaviour |
|---|---|
| ON (default) | Upstream behaviour preserved: addon auto-handshakes new bots on `Hello`, auto-summons on graveyard whispers, auto-refreshes inventory after loot, etc. |
| OFF | The addon stops auto-replying. UI button clicks (Attack, Follow, Stay, etc.) keep working normally — only the reactive auto-fires are silenced. |

When ON in combination with a server-side tactical agent that whispers the player, the auto-handler creates a feedback loop (`Hello` whisper → addon whispers `co ?` → tactical layer re-triggers). Turn OFF to break the loop.

The flag is persisted per-character via the `SynthiqBotsUISave` SavedVariables table.

## Feedback capture

`/sb feedback <note>` (or the `inv_misc_note_01` Main-column button) captures a screenshot, a 200-line chat ring buffer, and a snapshot of game context (zone, target, group, char identity) into `SynthiqBotsUISave.feedback_queue[]`. A server-side companion daemon (separate PR) watches that table + the WoW `Screenshots/` folder, uploads pending entries to the gateway, and the gateway agent decides whether to apply a live config tweak or open a PR with a fix.

SavedVariables flushes only on `/reload` or logout, so queued entries do not hit disk immediately. Run `/reload` after capturing if you want the daemon to pick the entry up right away.

Full design: [`docs/feedback-loop.md`](../../docs/feedback-loop.md) at the repo root.

## Provenance

Forked from [Macx-Lio/MultiBot](https://github.com/Macx-Lio/MultiBot). See [`UPSTREAM.md`](UPSTREAM.md) for the pinned upstream commit SHA and a list of local changes (rename + auto-chat toggle).

Original author: Nico Löbbert. License: GPLv3 (carried verbatim in [`LICENSE`](LICENSE)).
