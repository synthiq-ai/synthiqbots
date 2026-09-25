# In-game capture → delivery (screenshots, tactical maps, video)

Lets the bots/agent produce a visual of what's happening in-game and deliver it
to you. This is built in phases; **Phase 0 (delivery substrate), Phase 1
(synthetic tactical map), Phase 2 (real observer screenshots), and Phase 3
(observer video clips) are implemented today.** The full design and rationale
live in the plan that drove this work; this doc is the operator/developer
reference.

## Why it's shaped this way

Two hard constraints determined the architecture:

1. **AzerothCore bots are server-side and rendering-blind** — no client, no
   camera, no framebuffer. A "screenshot from a bot's eyes" is impossible. The
   two things that *are* possible: a **synthetic top-down map** rendered from
   server-side coordinates (Phase 1), or a **real screenshot from an observer
   character on a real WoW client** co-located with a bot (Phase 2), plus
   **video** of that observer (Phase 3).
2. **An MCP tool result cannot show you an image.** The MCP spec allows image
   content blocks, but no Anthropic client (Claude Code / Desktop / API
   connector) renders a tool-result image inline to the human. So a capture
   reaches you **out-of-band** (Telegram / Slack push), and the MCP tool result
   carries only a **clickable URL**.

## Phase 0 — delivery substrate (implemented)

Everything later (tactical maps, observer screenshots, clips) hands its bytes to
the same two pieces:

### Artifact store + serving — `internal/artifact`, `handlers/artifacts.go`

- `artifact.Store.Save(kind, ext, data)` writes the file under
  `OPS_ARTIFACT_DIR/YYYY/MM/DD/<kind>_<token><ext>` and returns the
  storage-relative path plus a public URL
  `OPS_ARTIFACT_BASE_URL/v1/artifacts/<relpath>`.
- `GET /v1/artifacts/*` (route in `main.go`) serves the file. It is **outside
  the bearer group on purpose** so the URL is clickable in a browser. The
  security model is the **unguessable 32-hex-char token** in each filename plus
  the host-level **Traefik IP allowlist** that already gates
  `ops.wow.example.com` (see the wow-mcp allowlist doc). Path traversal is
  rejected in `Store.Open`.
- Unconfigured (`OPS_ARTIFACT_DIR` empty) → the serve route 503s; nothing else
  breaks.

### Out-of-band push — `internal/notify`

- `notify.Notifier.Push(ctx, Artifact)` fans the artifact out to every
  configured channel concurrently, best-effort. It returns which channels
  accepted it and any per-channel errors; **a push failure never fails the
  capture** (you still get the URL).
- **Telegram** (`telegram.go`): Bot API `sendPhoto` / `sendVideo`, multipart,
  caption = `<caption>\n<url>`. Returns the error description on logical
  failures (the Bot API returns HTTP 200 with `ok:false`).
- **Slack** (`slack.go`): the modern 3-step external-upload flow
  (`files.getUploadURLExternal` → POST bytes → `files.completeUploadExternal`).
  Needs a bot token with `files:write`; the bot must be in the target channel.
- A channel is enabled iff **both** of its env vars are set; with neither set, a
  capture just returns `pushed:[]`.

## Configuration (ops-api env)

All optional — the feature degrades gracefully when unset. These live in the
ops-api compose `.env` on game-host (personal project — not GCP Secret Manager).

| Env var | Purpose | Default |
|---|---|---|
| `OPS_ARTIFACT_DIR` | Base dir for hosted artifacts (bind-mount it in compose) | *(empty → serve 503s)* |
| `OPS_ARTIFACT_BASE_URL` | Public origin used to build the returned URL | `https://ops.wow.example.com` |
| `OPS_ARTIFACT_MAX_MB` | Upload/serve size cap (MiB) | `25` |
| `OPS_TELEGRAM_BOT_TOKEN` | Telegram bot token (from @BotFather) | *(empty → channel off)* |
| `OPS_TELEGRAM_CHAT_ID` | Destination chat id (numeric or `@channel`) | *(empty → channel off)* |
| `OPS_SLACK_BOT_TOKEN` | Slack bot token `xoxb-…` (`files:write`) | *(empty → channel off)* |
| `OPS_SLACK_CHANNEL` | Slack channel id, e.g. `C0123ABCD` | *(empty → channel off)* |

Compose also needs a writable bind mount for `OPS_ARTIFACT_DIR` (mirrors how
`OPS_FEEDBACK_STORAGE_DIR` is mounted).

## Verifying Phase 0

```bash
# In the ops-api container/host (with OPS_ARTIFACT_DIR set), drop a PNG and
# confirm the serve route returns it:
curl -s -o /tmp/got.png https://ops.wow.example.com/v1/artifacts/<yyyy>/<mm>/<dd>/<file>.png
# Unit tests:
cd apps/ops-api && go test ./internal/notify/... ./internal/artifact/...
```

A live Telegram/Slack smoke test needs real tokens in the env; with them set,
the next phase's `render_tactical_map` is the first tool that actually exercises
the push path end to end.

## Phase 1 — synthetic tactical map (implemented)

Autonomous, server-only, no real client. Two tools chained by the agent:

1. **`get_scene_snapshot`** (gameplay MCP, `src/mod-ollama-chat_tools.cpp`) —
   one call returns the anchor bot's `{name,x,y,z,o,map_id,zone_id,zone_name}`
   plus every nearby player/creature/questgiver/gameobject with world
   coordinates. (The three `get_nearby_*` tools also now emit `x/y/z`.)
2. **`render_tactical_map`** (ops-api MCP, `internal/tacmap` +
   `internal/mcpserver/tools_tactical_map.go`) — takes that scene, renders a
   top-down "radar" PNG (cyan anchor + facing arrow, red hostiles, green
   players/bots, gold questgivers, grey objects, range rings), hosts it via the
   Phase-0 store, pushes it to Telegram/Slack, and returns
   `{ok, artifact_url, pushed, entity_count}`.

It's a **schematic** — dots on a flat plane, no terrain/minimap art (the server
has no terrain data). `render_tactical_clip` (animate frames → gif/mp4) is a
planned follow-up, not yet built.

## Phase 2 — real observer screenshots (implemented)

On-demand real game visuals. You power up the gaming-PC WoW client with a GM
**observer** character logged in; an agent then asks for a shot framed on a bot.

**Flow** (all orchestration is server-side in ops-api — the C++ module is
unchanged, it just exposes the existing `whisper_player` tool):

1. `request_observer_screenshot{target_bot, view?}` (ops-api MCP tool) registers
   a pending capture (`internal/observer` queue) and **whispers** the observer
   `"[SBSHOT] <reqId> <target> <view>"` via the gameplay `whisper_player` tool,
   then blocks waiting for delivery.
2. The **observer addon** (`addons/synthiqbots-ui/SynthiqBotsUIObserver.lua`)
   catches the whisper, runs `.gm visible off` + `.appear <target>`, optionally
   `SetView(view)`, and calls `Screenshot()` — a file lands in `Screenshots/`.
3. The **feedback-daemon** (`internal/observer` poller) polls
   `GET /v1/observer/pending`, matches the new screenshot by mtime, and POSTs it
   to `POST /v1/observer/screenshot` tagged with `reqId`.
4. ops-api hosts the artifact, pushes it to Telegram/Slack, and resolves the
   queue — the waiting MCP tool returns `{ok, reqId, artifact_url, pushed}`.

The **SavedVariables flush trap is avoided**: correlation is by the immediate
screenshot-file write + the `reqId` the daemon learns from the pending endpoint,
never SavedVariables (which only flush on `/reload`). Because the MCP tool
blocks until resolved, at most one capture is in flight, so newest-after-request
matching is unambiguous.

Set `screenshotFormat "jpeg"` on the observer client (`/console screenshotFormat
jpeg`) for small, Telegram/vision-friendly files (no TGA→PNG transcode exists).

**Auto-start + login** (optional): `apps/observer-launcher/` ships an AutoHotkey
script that launches WoW and logs the observer in (the GlueXML login screens
can't host an addon, so login is external input automation). See its README.

### Observer config (ops-api env)

| Env var | Purpose | Default |
|---|---|---|
| `OPS_OBSERVER_CHAR` | Observer character name to whisper | *(empty → tool + endpoints refuse)* |
| `OPS_OBSERVER_WHISPER_BOT_GUID` | Bot that sends the trigger whisper | `20007` (leader) |
| `OPS_OBSERVER_TTL_SEC` | How long the tool waits for a shot | `90` |

The observer character must be a **GM** (so `.appear` works) and the gaming-PC
**feedback-daemon** must be running.

## Phase 3 — observer video clips (implemented)

Same observer, same teleport, but the artifact is an MP4 recorded by **ffmpeg**
on the gaming PC and pushed to Slack.

**Why forward-recording, not an OBS replay buffer.** A replay buffer is
retrospective — it holds the last N seconds so you can save what already
happened. This trigger is prospective: the tool fires, *then* the observer
teleports, *then* there is something worth filming. Flushing a buffer at trigger
time would capture the observer's old parking spot and a loading screen. ffmpeg
is also one subprocess rather than a long-running peer with its own websocket
auth and scene config to drift. The daemon's `internal/recorder.Recorder` is an
interface, so an OBS backend can be added later without touching the queue, the
artifact store, or the addon.

**Flow** (again, zero C++ changes):

1. `request_observer_clip{target_bot, seconds?, view?}` registers a pending
   capture with `kind:"clip"` and whispers
   `"[SBCLIP] <reqId> <target> <view> <seconds>"`. It **returns immediately** —
   see the async note below.
2. The addon's `OB.Clip()` runs `.gm visible off` + `.appear <target>`, optional
   `SetView(view)` + `CameraZoomOut`, then fires `Screenshot()`.
3. That screenshot is a **ready-marker**, not the artifact. The daemon sees
   `kind:"clip"` in `GET /v1/observer/pending`, watches `Screenshots/` every
   250 ms for the marker (up to 20 s), and on seeing it spawns
   `ffmpeg -f gdigrab … -t <seconds>` in a **goroutine** (so the feedback and
   results pollers keep ticking).
4. The daemon POSTs the MP4 to `POST /v1/observer/clip`; ops-api hosts it, pushes
   it to Slack/Telegram, and retains the result.
5. `get_observer_clip{req_id}` → `{status:"ready", artifact_url, pushed}`.

**Why the tool is async.** A 20 s clip needs roughly 40–70 s end to end (teleport
+ record + encode + upload) while the MCP transport gives up around 60 s. A
blocking tool would report a timeout for a clip that actually succeeded. So
`request_observer_clip` returns `{ok, reqId, status:"recording"}` at once; the
clip lands in Slack when ready and `get_observer_clip` retrieves the URL from a
15-minute result cache.

**Why the marker at all.** The daemon's outer poll is every 5 s and the `.appear`
lands on the addon's schedule, so starting the recording on "I noticed a pending
clip" would film a loading screen. The addon already knows when it is in
position, and its `Screenshot()` is the only signal it can emit that the daemon
can observe instantly (SavedVariables only flush on `/reload`). Correlation is by
mtime, exactly as in Phase 2. The marker file is marked consumed so the
screenshot path never ships it as a stray artifact.

### Clip prerequisites (gaming PC)

- **ffmpeg** on `PATH`, or `ffmpeg.exe` beside the daemon, or `ffmpeg_path` in
  `config.toml`. Missing ffmpeg only disables clips — screenshots keep working
  and the daemon logs a warning at startup.
- WoW **windowed or borderless**. Exclusive fullscreen cannot be captured.
- The daemon records `-i title="World of Warcraft"` (override with
  `capture_window_title`). If your driver returns black frames for a
  hardware-accelerated D3D9 window, the recorder **automatically retries with
  `-i desktop`** and logs which path won.

The encode is `libx264 -preset veryfast -crf 28 -pix_fmt yuv420p -movflags
+faststart -an`. `yuv420p` is mandatory (gdigrab emits BGRA, which QuickTime and
Slack refuse to play); `+faststart` lets Slack preview without a full download;
there's no audio because gdigrab captures none.

### Clip config (ops-api env)

| Env var | Purpose | Default |
|---|---|---|
| `OPS_OBSERVER_CLIP_TTL_SEC` | Queue lifetime for a clip (record + encode + upload) | `180` |
| `OPS_OBSERVER_CLIP_MAX_SEC` | Longest clip a caller may request | `30` |
| `OPS_CLIP_MAX_MB` | Clip upload size cap | `100` |

Clips reuse the `OPS_OBSERVER_CHAR` gate — set that and both tools appear.

### Cross-kind guard

Both upload endpoints check the pending capture's kind and return **409** on a
mismatch. This is what stops a **stale daemon** (one predating Phase 3, which
only knows `/v1/observer/screenshot`) from shipping a clip's marker screenshot
and resolving the clip with a still image. An *unknown* reqId is not a mismatch —
the capture may simply have expired, and a late delivery is still worth hosting.

## Later phases (not yet implemented)

- `render_tactical_clip` — animate Phase-1 map frames into a gif/mp4. Synthetic,
  needs no client. Distinct from the real-observer clip above.
