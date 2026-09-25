# E2E harness — a headless human for the fleet features

The fleet, proactive and tactical features are gated on
`IsWhitelistedHumanPlayer`: a real, non-bot WorldSession on a whitelisted
account. A playerbot cannot stand in for the operator, so testing them used to
need the operator at a client. `tools/e2e/` is a **clientless 3.3.5a client** (no
WoW.exe, no graphics) that logs a real character in, chats, joins groups and
records every chat and group packet it receives, plus a runner that drives
scenarios and asserts on both sides: packets the "human" saw, and server state
via the MCP tools.

## Identity

| | |
|---|---|
| Account | a dedicated test account (e.g. id **2006**) |
| Character | `Qaprobe`, guid 20016, Orc Warrior (Horde, like the fleet) |
| Whitelist | `Gateway.WhitelistAccountIds` and `Gateway.AutoClaimAccountIds` = `"1001,2006"` |
| Password | generated on the server host and stored in a mode-600 `.creds` file next to the harness; insert the SRP6 verifier directly so the password never leaves the host |

A separate account means a run never kicks the operator off account 1001. But
**auto-claim hands the gateway bots to Qaprobe on login**, so the runner
refuses to start while a human on account 1001 is online (override:
`--allow-operator-online`).

## After every deploy (CI)

`deploy.yml` runs the whole suite after the 20-min canary ("E2E — headless human").
It runs in its own `python:3.12-slim` container (stdlib only — SRP6 is pure
Python in `wowclient/srp.py`; the runner has no pip) on the host network, as uid
1000 with the docker socket's group, reading the creds from a mounted secrets directory and the MCP bearer from the
module conf. Exit 3 (operator online) passes with a notice; `skip_e2e=true` skips it;
an `always()` step removes the container if the job is cancelled. Host and CI
runs share the lock in `/srv/wow/docker/wow/e2e-lock/`.

## Test tools

- `admin_quest_state` — status / complete / reset a quest (changes only fleet bots and playerbots).
- `admin_tactical_force` — the next N tactical ticks of a bot dispatch a fixed action (a test hook; arming resets that action's backoff baseline).
- `admin_tactical_state` — the last tactical snapshot, cooldowns and last action.

## Running

On game-host (the client talks to `127.0.0.1:3724`/`:8085`):

```bash
cd /srv/wow/docker/wow/azerothcore-wotlk/modules/mod-ollama-chat/tools/e2e
E2E_CREDS=/srv/wow/docker/wow/e2e-secrets/.creds python3 run.py                    # all scenarios
E2E_CREDS=/srv/wow/docker/wow/e2e-secrets/.creds python3 run.py --only S8 --keep-up  # a subset
```

The deployed module checkout already has the files: `cd /srv/wow/docker/wow/azerothcore-wotlk/modules/mod-ollama-chat/tools/e2e && E2E_CREDS=/srv/wow/docker/wow/e2e-secrets/.creds python3 run.py`.
No third-party packages: plain `python3` works (the earlier `wow-srp`
dependency only shipped an sdist that needs Rust to build).

Power: if the stack is down the runner starts it with `docker compose up -d
--no-deps <svc>` (never a bare `up`, which re-runs db-import) and stops only
what it started. A file lock (`/tmp/wow-e2e.lock`) keeps runs from overlapping.

One JSON line per scenario, then a summary; exit 0 = no FAIL/ERROR.

## Scenarios

| | Proves |
|---|---|
| S1 | login, presence, cross-map GM teleport + ACK, stays connected past idle |
| S2 | #457: leader direct-joins the human; both bots take him as master **and run to him** (measured 180 yd → 16 yd) |
| S3 | #458: a grouped call with `botGuid` inside `params` acts on that bot, not the leader |
| S4 | #459: an explicit turn-in with no quest ender in range is refused; 90 s of tactical ticks send the human no gossip whispers |
| S5 | #460: `admin_tactical_state` shows `bag_free` / `grey_items` / `durability_min_pct` in the live snapshot |
| S6 | identity through the real chat path: whisper the leader "what is my character name?" → the reply names the human |
| S7 | #457: when the human leaves, leader self-master and member→leader are restored |
| S8 | #459 positive path: seed quest 363 complete on Raz (`admin_quest_state`), put him at Shadow Priest Sarvis, turn in → rewarded, no gossip whisper (the tactical loop handing it in first also counts) |
| S9 | party orders reach the named bot: "Raz stay", then "Raz follow" |
| S10 | proactive: the leader proposes, the human says yes → `proact_approve` (SKIP if no proposal in 5 min) |
| S11 | #460 backoff, deterministic: `admin_tactical_force` makes Raz's next ticks a guaranteed-failing turn-in → "made no progress 2x", `cooling_down`, and the third forced tick withheld |

## Bot promotion (`promo_run.py`)

`.venv/bin/python promo_run.py` picks a random online Horde rndbot. It whispers the bot and waits
for the LLM reply, skipping playerbots' own colour-coded security replies ("You are too low
level"). Then it groups the bot with the tester and asks about gear in party chat, and finally
leaves the group. It asserts on the chat replies, on the master switch and on the
`[Ollama Chat Promote]` promoted/demoted log lines. playerbots logs random bots out while no
human is online, so the script waits up to 8 min for them after login. Rndbots ignore a level-1
tester's invite, so the script falls back to `admin_group_join`. First pass 2026-09-25:
a Tauren druid (level 42) named its real staff in 6.5 s.

## Protocol traps (each one hit or verified on 2026-09-25)

- **OS tag `OSX`**, not `Win`: `WorldSession::InitWarden` only creates Warden
  for `Win`; an unanswered Warden check kicks after `Warden.ClientResponseDelay`.
- **`CMSG_PING` no faster than ~30 s** — at 25 s the session was kicked for
  "over-speed pings" (`WorldSocket::HandlePing`). Keepalive + ping every 32 s.
- **Header cipher**: ARC4-drop1024; the client decrypts with
  `HMAC-SHA1(CC98AE04…, K)` and encrypts with `HMAC-SHA1(C2B3723C…, K)`
  (`AuthCrypt.cpp`). Decrypt the first header byte alone: bit 0x80 means a
  5-byte header. `wow-srp`'s own header crypto needs the length up front, so the
  harness runs its own byte-wise RC4.
- **Teleports must be ACKed** even though the client never moves:
  `MSG_MOVE_WORLDPORT_ACK` after `SMSG_NEW_WORLD`, `MSG_MOVE_TELEPORT_ACK`
  (packed guid, counter, time) for near teleports — otherwise the character
  never finishes arriving.
- **`LANG_UNIVERSAL` chat is refused as a cheat**; the Horde tester speaks Orcish (1).
- **`CMSG_GROUP_ACCEPT` carries a u32**, not an empty body.
- **Group membership survives logout** — the runner leaves any group first.
- Opcode values come from the deployed `Opcodes.h`, not memory
  (`SMSG_GROUP_DESTROYED` is `0x07C`).

## What it found

- "Raz stay" then "Raz follow" left Raz standing: `bot_follow`'s two dedups
  (within 20 yd / followed < 90 s ago) assumed the bot was already following.
  A dispatched `stay` now disables both until the next follow.
- The fleet vanished after the last human logged out (playerbots'
  `DisabledWithoutRealPlayer`); the fleet tick re-spawns it.

- The runner's own ping cadence (above).
- A real product bug: a question whispered right after a proactive proposal
  ("Quick check, **no** actions: what is my character name?") was classified
  `other` by jev with confidence 1.0, then the keyword fallback saw "no" in the
  first three words, turned it into a Reject and swallowed the message — the
  leader never answered. Fixed: a confident `other`/`none` from the classifier
  is final; the keyword matcher only runs when the classifier failed or was unsure.

## Not covered yet

Real-client visuals (the observer launcher on the gaming PC, `docs/capture.md`),
combat, dungeons/battlegrounds, and voice (`talk_to_leader`) flows.
