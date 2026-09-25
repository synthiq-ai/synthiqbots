# In-game feedback loop

Capture screenshots + chat + game context from the WoW client, deliver them to
a gateway agent, and let the agent improve `mod-ollama-chat` on the fly
(config tweaks via existing MCP tools, or full PRs via the agent's own
gh/git tooling).

End-to-end pipeline:

```
WoW client (Windows)                  user's PC                   game-host
─────────────────────                 ─────────                   ─────
[SynthiqBotsUI addon]                 [companion daemon]          [wow-ops-api]
  /sb feedback <note>      writes      reads SavedVariables  POSTs   /v1/feedback
  Screenshot()             SavedVars   matches Screenshots/  multipart
  capture chat ringbuf  ──────────►   uploads pending entries ──────►  stores
                                                                       row + img
                                                                          │
                                                                          ▼
                                                              [feedback dispatch worker]
                                                              builds multimodal prompt
                                                              POSTs to gateway agent (Geek)
                                                                          │
                                                                          ▼
                                                              [gateway agent]
                                                              reads feedback + screenshot
                                                              uses MCP config_set_value, OR
                                                              clones repo + opens PR via gh
                                                                          │
                                  ◄─────── whispers reply ───────────────┘
                                  in-game (whisper_player MCP)
                                  + writes to SavedVariables companion sidecar file
                                  → /sb feedback log shows status + PR url
```

## Why this shape

The hard constraint: WoW 3.3.5a Lua addons cannot make HTTP calls and cannot
read their own screenshot files. Anything network-bound has to live outside
the game. The simplest egress path on Windows is a small companion daemon
watching SavedVariables + the `Screenshots/` folder.

SavedVariables flushes only on `/reload`, logout, or addon `OnDisable`, so
queued entries do not hit disk immediately. The companion daemon polls and
the user must `/reload` (or just log out) for the queue to flush. We surface
this in the addon UI so users know to reload.

## Phasing — 5 PRs

| PR | Scope | Status |
|----|-------|--------|
| **PR 1** | Addon: `/sb feedback` slash command, capture button, SavedVariables schema, chat ring buffer, context snapshot | merged |
| **PR 2** | `wow-ops-api` `POST /v1/feedback` (multipart) + `mod_ollama_chat_feedback` table + storage volume + `ops_rw` grant + `curl`-smoke recipe | merged |
| **PR 3** | Windows companion daemon (Go .exe): tail SavedVariables, match `Screenshots/`, multipart-upload to PR 2 endpoint | merged |
| **PR 4** | Dispatch worker: poll pending rows, build multimodal Synthiq prompt, route to a vision-capable bot, persist `agent_response` + `agent_pr_url` | merged |
| **PR 5** | Loopback: companion writes results back to addon's sidecar SavedVariables file → `/sb feedback log` shows status / PR links / agent replies | this PR |

Each PR is independently testable — PR 1 already piles up SavedVariables
entries; PR 2 can be exercised with `curl` before the daemon lands; PR 3
closes the client-side egress; PR 4 is what makes the agent actually
*do* something with the feedback.

## PR 5 — loopback (this PR)

### What it closes

Final slice. The agent's response now reaches the user back in-game via `/sb feedback log` — closing the capture → upload → dispatch → response → display loop.

### Server side: GET /v1/feedback/results

New bearer-gated endpoint on `wow-ops-api`. Returns rows with `status IN ('resolved','error')` since a cursor:

```bash
curl -fsS -H "Authorization: Bearer $T" \
  "http://192.168.100.11:18791/v1/feedback/results?since=10&limit=100"
```

Response:

```json
{
  "rows": [
    { "id": 11, "addonId": "abc-1", "addonTs": 1745673600,
      "status": "resolved", "agentResponse": "raise heartbeat",
      "agentActions": "{\"action\":\"config_tweak\",...}",
      "agentPrUrl": "", "resolvedAt": "2026-04-26T14:00:03Z" }
  ],
  "nextSince": 11,
  "count": 1
}
```

`limit` defaults to 100, clamped to [1, 500]. Ordered by `id ASC` so the daemon can advance its cursor monotonically.

### Daemon side: results poller

Each scan tick (same cadence as the upload poll), the daemon:

1. `GET /v1/feedback/results?since=<state.lastResultsId>` — pull the next batch.
2. Merge new rows into an in-memory cache (capped at 100 entries — drop oldest by `db_id`).
3. Serialize the cache as a Lua SavedVariables file:
   ```lua
   SynthiqBotsUIResults = {
       ["abc-1"] = { id="abc-1", db_id=11, status="resolved",
                     summary="raise heartbeat", pr_url="",
                     resolved_at="2026-04-26T14:00:03Z",
                     actions = [[ {"action":"config_tweak",...} ]] },
   }
   ```
4. Atomic write (tmp + rename) to:
   ```
   <wow>/WTF/Account/<acc>/SavedVariables/SynthiqBotsUIResults.lua
   ```
5. Persist the new cursor in `state.json`.

The actions JSON is wrapped in Lua long-bracket form (`[[ ... ]]` or `[=[ ... ]=]`) so embedded double-quotes don't need escaping; the helper picks an equals-count that doesn't appear inside the JSON.

### Addon side: read + render

The addon's `.toc` declares `## SavedVariables: SynthiqBotsUIGlobalSave, SynthiqBotsUIResults` so WoW loads the daemon-written file automatically on game start. The addon never mutates the global, which means `/reload` writes the same content back — no race with the daemon's last write.

`/sb feedback log` now renders each queued entry alongside its agent reply (status, summary, optional PR URL):

```
[SynthiqBotsUI] Feedback queue: 3 entries
  [1] ts=1745673600 note="bot pulled twice" — resolved: raise tactical heartbeat https://github.com/.../pull/123
  [2] ts=1745673700 note="" — error: synthiq 503
  [3] ts=1745673800 note="loot bug" — pending
```

### Why a sidecar file (not the addon's own SavedVariable)

Mixing the daemon's writes with the addon's own SavedVariables would race: WoW serializes the addon's in-memory state on `/reload` and would overwrite the daemon's recent writes with a stale snapshot. Keeping `SynthiqBotsUIResults` as a daemon-only owned table — addon reads but never writes — sidesteps this. The only race left is "daemon writing during the millisecond WoW spends serializing", which is rare and self-correcting (next tick re-fetches the same rows).

### Tests

7 unit tests added across the new code: results client `Fetch` (happy path with cursor + bearer, 4xx bubbling), `WriteSavedVariables` (Lua-quoted format, control-char escapes, long-bracket selection for actions JSON containing `]]`, atomic rename), and ops-api handler tests for `GET /v1/feedback/results` (happy path, 503-no-DB, limit-clamping).

## PR 4 — dispatch worker (merged)

### What it does

A goroutine inside `wow-ops-api` polls `mod_ollama_chat_feedback` for `status='pending'` rows, builds an OpenAI-compatible chat-completions payload (text body + base64-encoded screenshot as an `image_url` content block), POSTs it to a vision-capable Synthiq agent, and writes the agent's structured response back into the same DB row. Disabled by default — set `FEEDBACK_DISPATCH_ENABLE=1` plus the URL/Bearer/Model trio to start.

### Where it runs

Inside the existing `wow-ops-api` Go binary (`apps/ops-api/internal/dispatch/`). Sharing the process gives it free DB access via the `ops_rw` pool and direct filesystem reads from the screenshot mount — no second container, no second deploy job, no per-bot routing baggage from the in-game gateway path.

### State machine

```
pending  ──ClaimNext──►  dispatched (dispatched_at = NOW)
                          │
                ┌─────────┴─────────┐
              success             error
                │                   │
                ▼                   ▼
            resolved            error
            agent_response      agent_response = "<error>"
            agent_actions       resolved_at = NOW
            agent_pr_url
            resolved_at
```

`ResetStale` runs at worker startup: rewinds any `dispatched` rows older than `FEEDBACK_DISPATCH_STALE_SEC` (default 600) back to `pending`, so a previous-process crash doesn't strand rows forever.

### Agent contract

System prompt locks the model into "module-improvement agent" framing. Per feedback the agent decides ONE action:

- `config_tweak` — invoke `file_set_key` + `container_restart` via the wow-admin MCP
- `code_pr` — branch from main, patch, push, open PR via gh
- `need_info` — ask a clarifying question
- `no_action` — explain why nothing should happen

The model returns a JSON object (with optional `config_changes[]`, `pr_url`, `follow_up_question`). The worker stores `summary` in `agent_response` and the structured fields as JSON in `agent_actions`.

### Multimodal payload

OpenAI-compatible content blocks:

```json
{
  "messages": [
    { "role": "system", "content": "<system prompt>" },
    { "role": "user", "content": [
        { "type": "text", "text": "## User feedback (id=...)..." },
        { "type": "image_url", "image_url": { "url": "data:image/x-tga;base64,..." } }
    ]}
  ]
}
```

Synthiq presents an OpenAI-compatible surface, so this should pass through. If a particular Synthiq deployment strips the image block, the agent still gets the text body (chat history + ctx + note) and can fall back to text-only reasoning.

### Required env vars

- `FEEDBACK_DISPATCH_ENABLE=1` — master gate (default 0)
- `FEEDBACK_DISPATCH_URL` — OpenAI-compat chat-completions base, e.g. `https://geek.example.com/`
- `FEEDBACK_DISPATCH_BEARER` — bearer for the agent endpoint
- `FEEDBACK_DISPATCH_MODEL` — vision-capable model id, e.g. `claude-sonnet-4-6`
- `FEEDBACK_DISPATCH_POLL_SEC` — poll interval (default 30)
- `FEEDBACK_DISPATCH_STALE_SEC` — dispatched-row rewind threshold (default 600)
- `FEEDBACK_DISPATCH_MAX_IMAGE_MB` — per-row image cap (default 16)

If any of `URL`/`BEARER`/`MODEL` is missing or `OPS_FEEDBACK_STORAGE_DIR`/`DB_EXEC_DSN` is empty, the worker logs a warning and skips startup — the rest of `wow-ops-api` continues normally.

### Smoke test

Capture in-game (`/sb feedback test`), let the daemon upload, then watch the dispatch worker pick it up:

```bash
docker compose logs -f wow-ops-api | grep dispatch
docker exec ac-database mysql -uroot -ppassword -e \
  "SELECT id, status, agent_response, agent_pr_url FROM mod_ollama_chat_feedback ORDER BY id DESC LIMIT 5" \
  acore_characters
```

You should see `dispatch: claimed` → `dispatch: resolved` log lines and the row's `status` flip from `pending` to `resolved`.

### Tests

17 unit tests across 4 files (`db_test.go`, `prompt_test.go`, `synthiq_test.go`, `worker_test.go`). DB transitions verified via `sqlmock`; Synthiq client tested against an `httptest` fake covering happy-path JSON, code-fence stripping, non-JSON soft-fail, 5xx bubbling, and unconfigured-client refusal; worker E2E runs the full claim → call → resolve flow with a fake upstream.

## PR 3 — Windows companion daemon (merged)

### What it is

A single ~11 MB Windows .exe (`feedback-daemon.exe`, cross-compiled from Go) that runs on the user's WoW client machine. Watches `WTF/Account/<acc>/SavedVariables/SynthiqBotsUI.lua` and the WoW `Screenshots/` folder; for each new feedback-queue entry that hasn't been uploaded yet, finds the closest screenshot by mtime within a ±15s window and POSTs both as `multipart/form-data` to PR 2's `/v1/feedback` endpoint.

Lives at [`apps/feedback-daemon/`](../apps/feedback-daemon/). Standalone Go module — independent `go.mod` from `apps/ops-api/` so the deps don't bleed.

### How it parses Lua

We embed [`gopher-lua`](https://github.com/yuin/gopher-lua) (a pure-Go Lua 5.1 VM, ~2 MB) and just `dostring` the SavedVariables file. The format is a Lua script that assigns globals; running it in a sandbox VM is more robust than regex-parsing the table syntax (which has nested tables, escaped strings inside chat history, mixed key types, and quoted-vs-bareword keys). The runtime cost is ~5 ms for a 200 KB file — fine for a 5s poll loop.

### Dedup state

`%APPDATA%\synthiq-feedback\state.json` records `{uploaded: { "<addon-id>": "<upload-ts>" }}`. Every upload is keyed by the addon-side id; if the same id is seen again (because the user hasn't `/reload`-ed and the queue still contains it), the daemon skips. Atomic write (tmp + rename) so a crash mid-flush doesn't corrupt the state.

### Screenshot match

WoW writes `WoWScrnShot_MMDDYY_HHMMSS.tga|jpg`. The daemon scans `Screenshots/`, picks the file whose mtime is closest to `addon_ts` within `screenshot_match_window_sec` (default 15s). Files outside the window are ignored. Non-image files (`.txt`, etc.) are skipped via a small extension allowlist.

### Configuration

Single TOML file at `%APPDATA%\synthiq-feedback\config.toml` — see [`apps/feedback-daemon/config.example.toml`](../apps/feedback-daemon/config.example.toml). Four required fields: `api_url`, `bearer_token`, `wow_root`, `account_name` (auto-detected if there's only one). Optional knobs for poll interval, match window, dry-run.

### Run modes

- `feedback-daemon.exe --once` — scan, upload pending, exit. Useful for cron/Scheduled-Task one-shots.
- `feedback-daemon.exe` — watch mode, polls every `poll_interval_ms`. Recommended via Scheduled Task at logon.
- `feedback-daemon.exe --once -v` — first-run smoke test with debug logging.

### Tests

17 unit tests across 5 packages: config (defaults, validation, account auto-resolution), saves (Lua parser happy path, missing/empty/malformed inputs, char-realm split), screens (closest-in-window selection, mime detection, missing dir tolerance), state (round-trip, reload, atomic rename), upload (with image, meta-only, server-error bubbling, raw multipart shape). Run via `go test ./...` from `apps/feedback-daemon/`.

## PR 2 — server ingest (merged)

### Endpoint

`POST /v1/feedback` on the existing `wow-ops-api` Go binary. Sits inside the
bearer-auth group alongside `/v1/audit/*` and `/v1/files/*`. Reachable two
ways like the rest of the API:

- Public TLS: `https://ops.wow.example.com/v1/feedback`
  (Traefik IP-allowlist + bearer)
- LAN fast-path: `http://192.168.100.11:18791/v1/feedback` (bearer only)

### Request shape — `multipart/form-data`

Two parts:

| Name | Type | Required | Notes |
|------|------|----------|-------|
| `meta` | text/plain (UTF-8 JSON) | yes | `{id, addonTs, note, charName, charRealm, charClass, charLvl, zone, chatHistory, ctx}` |
| `screenshot` | image/x-tga, image/jpeg, image/png | optional | The companion daemon may upload meta-only when the screenshot file went missing between capture and upload |

Caps:

- Image size: `OPS_FEEDBACK_MAX_IMAGE_MB` (default 12 MiB)
- Meta JSON: `OPS_FEEDBACK_MAX_META_KB` (default 256 KiB)

### Response

```json
{
  "id": "abc-123",
  "dbId": 42,
  "status": "pending",
  "imagePath": "2026/04/26/1745673600_abc-123_a1b2c3d4.tga",
  "imageBytes": 18421
}
```

`imagePath` is relative to `OPS_FEEDBACK_STORAGE_DIR`. The dispatch worker
(PR 4) reads the image from disk by joining the storage root with this path.

### Storage layout on disk

```
$OPS_FEEDBACK_STORAGE_DIR/
└── 2026/
    └── 04/
        └── 26/
            └── 1745673600_abc-123_a1b2c3d4.tga
```

YYYY/MM/DD subdirs keep day-bucket counts manageable. Filename =
`{addonTs}_{safeID}_{4-byte-hex}.{ext}`. The addon-supplied id is sanitized
(non-alphanumeric → `_`, capped at 64 chars).

### DB schema — `mod_ollama_chat_feedback`

See `data/sql/characters/base/2026_04_26_feedback.sql`. Created with
`CREATE TABLE IF NOT EXISTS` so AzerothCore picks it up on next start.

The dispatch worker (PR 4) flips `status` from `pending` → `dispatched` →
`resolved` (or `error`) and fills `agent_response` + `agent_actions` +
`agent_pr_url`.

### Required setup on game-host (one-time)

Mount a host directory into the `wow-ops-api` container at the path you
chose for `OPS_FEEDBACK_STORAGE_DIR`, e.g. `/feedback` →
`<your compose dir>/feedback/`. Add to `<your compose dir>/compose.yml`:

```yaml
services:
  wow-ops-api:
    volumes:
      - ./feedback:/feedback
    environment:
      OPS_FEEDBACK_STORAGE_DIR: /feedback
```

Grant `ops_rw` write access to the new table:

```sql
GRANT SELECT, INSERT, UPDATE ON acore_characters.mod_ollama_chat_feedback
  TO 'ops_rw'@'%';
FLUSH PRIVILEGES;
```

(`ops_ro` already has SELECT via the `mod_ollama_chat_%` wildcard grant
documented in `docs/ops-api.md`.)

### Smoke test (curl)

```bash
TOKEN="$(grep OPS_BEARER_TOKEN <your secrets .env> | cut -d= -f2)"
curl -sS -X POST -H "Authorization: Bearer $TOKEN" \
  -F 'meta={"id":"smoke-1","addonTs":1745673600,"note":"hello"}' \
  -F 'screenshot=@/tmp/test.tga;type=image/x-tga' \
  http://192.168.100.11:18791/v1/feedback
```

Verify the row landed:

```bash
docker exec ac-database mysql -uroot -ppassword -e \
  "SELECT id, addon_id, image_path, status FROM mod_ollama_chat_feedback ORDER BY id DESC LIMIT 5" \
  acore_characters
```

## PR 1 — addon side (merged)

### Capture trigger


Two equivalent triggers:

- Slash command: `/sb feedback <optional note>` — takes a freeform note, the
  rest of the context is gathered automatically.
- UI button: new "Feedback" button in the MultiBar `Main` column (right after
  Auto-Chat at y=442). Left-click captures with no note.

Sub-commands:

- `/sb feedback` — capture with empty note
- `/sb feedback <text>` — capture with note (note is preserved verbatim,
  including casing — this is the only `/sb` subcommand that sees the original
  message because the existing parser lowercases everything)
- `/sb feedback log` — print recent queue + agent replies in chat (PR 4
  fills in the agent-reply side; PR 1 only shows queue state)
- `/sb feedback help` — usage

### Captured payload

Each feedback entry is a Lua table appended to
`SynthiqBotsUISave.feedback_queue[]`:

```lua
{
    id              = "1745673600-4823",       -- ts + random suffix
    ts              = 1745673600,              -- unix epoch (seconds)
    note            = "bot Geek pulled a second pack while I was at 30%",
    screenshot_hint = "2026-04-26 14:00:03",   -- daemon matches by mtime
    chat_history    = { {ts, channel, sender, text}, ... },  -- last 200 lines
    ctx             = {
        char        = "the operator-SynthiqEU",
        char_class  = "PRIEST",
        char_lvl    = 47,
        zone        = "Stranglethorn Vale",
        subzone     = "Grom'gol Base Camp",
        target      = "Bloodscalp Hunter|Hunter|46|82",  -- name|class|lvl|hp%
        group       = { "Geek", "Claude", "Clawd" },
        addon_ver   = "2.0.0",
    },
    status          = "pending",   -- daemon flips to "uploaded" via sidecar file in PR 4
}
```

### Chat ring buffer

The feedback module owns a separate `CreateFrame` listener (independent of
the main `SynthiqBotsUI` event dispatcher to keep changes scoped) that
records the last `HISTORY_MAX = 200` lines from these channels:

`CHAT_MSG_SAY`, `CHAT_MSG_YELL`, `CHAT_MSG_PARTY`, `CHAT_MSG_PARTY_LEADER`,
`CHAT_MSG_RAID`, `CHAT_MSG_RAID_LEADER`, `CHAT_MSG_GUILD`, `CHAT_MSG_OFFICER`,
`CHAT_MSG_CHANNEL`, `CHAT_MSG_WHISPER`, `CHAT_MSG_WHISPER_INFORM`,
`CHAT_MSG_EMOTE`.

Each line stores `{ts, channel, sender, text}`. On capture, the buffer is
copied into the entry — agents see the conversation that led up to the
feedback moment.

### Queue cap

`SynthiqBotsUISave.feedback_queue` is capped at `QUEUE_MAX = 50` entries.
On overflow the oldest is dropped. This protects against runaway capture
spam (e.g. holding a hotkey).

### Screenshot timing

`Screenshot()` is async — the file appears a few hundred ms later in
`<wow-install>/Screenshots/WoWScrnShot_MMDDYY_HHMMSS.tga|jpg`. The addon
records `screenshot_hint` (a human-readable timestamp) and `ts` (unix
epoch). The companion daemon (PR 2) matches by file mtime within ±10s of
`ts` and ties the closest screenshot to the entry.

### What the agent gets

PR 3 will deliver to the agent (Geek by default — runs on cloud Synthiq,
Sonnet 4.6 vision-capable):

- The screenshot as `image_url` content block (base64) — falls back to
  Anthropic-direct if Synthiq strips images
- The feedback note + chat history + ctx as user-message text
- A system prompt: *"You're a code-improvement agent for the
  mod-ollama-chat AzerothCore module. The user has flagged an issue.
  You have MCP tools for live config tweaks (`config_set_value`,
  `config_reload`, `ops_files_*`) and a checkout of the repo where you
  can open a PR via gh. Decide whether the issue calls for a config
  knob change, a code patch, or just a clarifying whisper back to the
  user."*

## Privacy

Per user direction, no scrubbing in v1. Feedback chat logs may contain
other players' messages verbatim. This is a personal-server / single-operator
setup so the blast radius is small, but worth flagging here for future-self.

## Risks / open questions

- **Vision support on Synthiq upstream** — we'll find out empirically in
  PR 3. If `image_url` blocks are stripped, we add an Anthropic-direct
  fallback worker.
- **Daemon install on Windows** — Go cross-compiled .exe is the obvious
  shape. Open question: does the user want a tray app with a UI, or a
  scheduled-task background process? Defaulting to scheduled task for
  simplicity; tray UI can come later if needed.
- **Agent commit identity** — when the agent opens a PR via gh, it'll
  do so under whatever identity its host (Synthiq runner) is configured
  with. Probably fine for a personal project but worth setting an
  explicit `git config user.email` on the agent side so PRs are
  traceable.
- **SavedVariables flush lag** — until the user `/reload`s, queue
  entries are RAM-only. Acceptable for v1; future improvement could
  trigger an explicit flush via UI logout/reload.
