# feedback-daemon

Windows companion process for the SynthiqBotsUI addon's `/sb feedback` capture flow. Tails the addon's `SavedVariables` Lua file, matches WoW `Screenshots/` files by timestamp, and POSTs both to the `wow-ops-api` `POST /v1/feedback` endpoint as `multipart/form-data`. Dedup state lives in `%APPDATA%\synthiq-feedback\state.json` so re-runs don't re-upload.

PR 3 of the feedback-loop project. Full design + 5-PR phasing: [`docs/feedback-loop.md`](../../docs/feedback-loop.md) at the repo root.

## Why a daemon

WoW 3.3.5a Lua addons cannot make HTTP calls and cannot read their own screenshot files. The only way to ship a screenshot + chat snapshot off the client is to run a small process on the same machine that watches the addon's output files and pushes them to the server.

## Build

```bash
# From the repo root, cross-compile to a single Windows .exe:
cd apps/feedback-daemon
GOOS=windows GOARCH=amd64 go build -o feedback-daemon.exe .
```

The result is a ~11 MB statically-linked binary with no DLL dependencies. Drop it anywhere; it does not need an installer.

For dev / CI on the server side:

```bash
go build ./...
go test ./...
```

## Install (Windows)

1. Copy `feedback-daemon.exe` somewhere stable, e.g. `%LOCALAPPDATA%\synthiq-feedback\`.
2. Copy [`config.example.toml`](config.example.toml) to `%APPDATA%\synthiq-feedback\config.toml`.
3. Fill in the four required fields:
   - `api_url` — `https://ops.wow.example.com` (or LAN fast-path on game-host)
   - `bearer_token` — must match `OPS_BEARER_TOKEN` in `<your secrets .env>` on game-host
   - `wow_root` — path to your WoW install (the dir containing `WoW.exe`)
   - `account_name` — only required if you have multiple WoW accounts on this PC
4. Test it once:
   ```cmd
   feedback-daemon.exe --once -v
   ```
   You should see "loaded config", "state loaded", and either "no entries" (if the addon hasn't captured anything yet) or one or more "uploaded" lines.
5. To run it continuously, register a Scheduled Task:
   ```cmd
   schtasks /Create /SC ONLOGON /TN "synthiq-feedback" /TR "%LOCALAPPDATA%\synthiq-feedback\feedback-daemon.exe" /RL HIGHEST
   ```

## CLI

```
feedback-daemon [--config PATH] [--once] [-v]

  --config PATH  TOML config file (default: %APPDATA%\synthiq-feedback\config.toml)
  --once         scan, upload pending entries, then exit
  -v             verbose logging (debug-level slog output)
```

By default the daemon runs in watch mode, polling every 5s (configurable). Send SIGTERM / SIGINT to shut down cleanly.

## Configuration

See [`config.example.toml`](config.example.toml) for the full annotated config. Required fields: `api_url`, `bearer_token`, `wow_root`. Everything else has sensible defaults.

`dry_run = true` logs what would upload without making HTTP calls — useful for first-run validation.

## What gets uploaded

Per feedback entry the addon captured:

- **`meta` (JSON)** — the entry's id, addon timestamp, note, char/realm/class/lvl/zone, full `chatHistory[]` (last 200 lines from the ring buffer), full `ctx` blob.
- **`screenshot` (file)** — the WoW `WoWScrnShot_*.tga|jpg|png` whose mtime is closest to `addon_ts` within the configured ±window. Skipped when no candidate is found within the window (e.g. user emptied `Screenshots/`).

The server returns `{id, dbId, status:"pending", imagePath, imageBytes}`. The daemon records the addon-side id in `state.json` so the same entry is never re-uploaded.

## Observer video clips (Phase 3)

The daemon also records video for `request_observer_clip`. This needs **ffmpeg**:

```powershell
winget install Gyan.FFmpeg      # or drop ffmpeg.exe next to feedback-daemon.exe
```

Resolution order is `ffmpeg_path` in `config.toml` → `PATH` → `ffmpeg.exe` beside the daemon. If none resolve the daemon logs `observer: ffmpeg not found; clip requests will fail (screenshots unaffected)` at startup and keeps running.

Two client-side requirements:

- **WoW must be windowed or borderless.** Exclusive fullscreen cannot be captured by `gdigrab` (nor by the observer-launcher's `ImageSearch` clicks).
- The window title must match `capture_window_title` (default `World of Warcraft`). If the window grab yields black frames — a known quirk of capturing a hardware-accelerated D3D9 window on some drivers — the recorder retries automatically with a full-desktop grab and logs which input succeeded.

Smoke-test the whole loop with `feedback-daemon.exe --once -v` while a clip request is pending; you should see `observer: in position, recording` then `observer: shipped clip`.

## Troubleshooting

- **"wow_root not accessible"** — the path in `config.toml` is wrong or doesn't exist. Forward or back slashes both work but the directory itself must exist.
- **`request_observer_clip` always times out / `get_observer_clip` says `expired`** — check the daemon log. `ffmpeg not found` means install it (above). `no ready-marker screenshot within 20s` means the observer isn't in-world with the addon armed, or the `.appear` failed (is it a GM?).
- **The clip is a black rectangle** — WoW is in exclusive fullscreen, or the window grab hit the D3D9 driver quirk and the desktop fallback captured a covered window. Run WoW windowed and keep it unobscured.
- **Slack shows the clip as a downloadable file, not a player** — the artifact didn't get a `.mp4` extension. That means the daemon shipped an unexpected MIME; check `Content-Type: video/mp4` on the upload.
- **`409 Conflict` on upload** — your daemon binary predates Phase 3 and posted a clip's marker screenshot to the screenshot endpoint. Replace the exe.
- **"N accounts in <path>; set account_name to disambiguate"** — you have multiple WoW accounts. Pick one and set it explicitly in `account_name`.
- **`api_url` 401** — bearer token mismatch. Compare `bearer_token` in your config against `OPS_BEARER_TOKEN` in `<your secrets .env>` on game-host.
- **"upload failed: server 503"** — game-host-side `OPS_FEEDBACK_STORAGE_DIR` not configured or `DB_EXEC_DSN` missing. See `docs/feedback-loop.md` PR 2 setup.
- **No upload happening even though I captured feedback** — SavedVariables only flush to disk on `/reload` or logout. Run `/reload` in-game, then watch the daemon log.

## Development

```bash
# All tests:
go test ./...

# A specific package:
go test ./internal/saves -v

# Cross-compile + smoke test against a real ops-api:
GOOS=windows GOARCH=amd64 go build -o /tmp/feedback-daemon.exe .
# (Then test on a Windows VM or using wine.)
```

Tests cover: config loader (defaults, validation, account-dir resolution), SavedVariables Lua parser (happy path, missing file, no queue, malformed entries, char-realm split), Screenshots matcher (closest-in-window selection, non-image rejection, missing dir tolerance), state store (round-trip, reload persistence, atomic rename), upload client (with-image, meta-only, server-error bubbling, raw multipart shape), the observer poller (mtime correlation, used-file dedup, screenshot vs clip routing, non-blocking recording, in-flight guard, 503 tolerance), and the ffmpeg recorder (arg construction, desktop fallback, missing-binary error).
