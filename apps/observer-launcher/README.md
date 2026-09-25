# observer-launcher

Auto-start WoW 3.3.5a and log in the GM **observer** character so the on-demand
real-screenshot loop (Phase 2 of the capture feature) can run hands-free. See
`docs/capture.md` for the full picture.

The WoW 3.3.5a login / realm / character screens run GlueXML and **cannot host
an addon**, so login must be driven by external input automation. This is an
**AutoHotkey v1.1** script (`observer-login.ahk`).

## What it does

```
power on (or Wake-on-LAN) -> Windows auto-logon -> Task Scheduler runs this
  -> Wow.exe launches -> types the observer password -> realm + Enter World
  -> SynthiqBotsUIObserver addon arms -> feedback-daemon polls ops-api
```

## One-time setup

1. **Install AutoHotkey v1.1** — https://www.autohotkey.com/ (the v1.1 line, not v2).
2. **Run WoW windowed or borderless — never exclusive-fullscreen.** Exclusive
   fullscreen breaks both `ImageSearch` clicks here *and* `ffmpeg gdigrab` / OBS
   window capture (Phase 3). Pin a fixed windowed resolution so the button
   images stay valid.
3. **Pre-save the account name** in `WTF/Config.wtf`:
   `SET accountName "YOUROBSERVERACCOUNT"` — then only the password is typed.
4. **Password file** — create `observer-password.txt` next to the script with the
   observer account password on one line. Restrict it to your user
   (Properties → Security, or store in Windows Credential Manager and adapt the
   script). Never hardcode the password in the `.ahk`.
5. **Make the observer a GM** so `.appear` works:
   `.account set gmlevel <account> 3 -1` (or via the wow-admin `wow_set_gm_level`
   tool). The addon also runs `.gm visible off` so the observer doesn't disturb
   the scene.
6. **Capture button images** (optional but more robust than the `{Enter}`
   fallback): screenshot the "Enter World" / realm buttons at your fixed
   resolution, crop tightly, and save as `enter-world.png` / `realm-enter.png`
   next to the script. Without them the script falls back to pressing Enter
   (works when the last realm/character is remembered).
7. **Edit the config block** at the top of `observer-login.ahk` — `WOW_EXE` path
   especially.

## Run it

- Manual: double-click `observer-login.ahk` (or `AutoHotkey.exe observer-login.ahk`).
- Unattended: **Task Scheduler → Create Task → Trigger: At log on → Action: start
  `AutoHotkey.exe` with argument the full path to `observer-login.ahk`.** Combine
  with Windows auto-logon and **Wake-on-LAN** to bring the box up remotely before
  a capture session.

## ops-api side

Set these in the ops-api compose `.env` on game-host so the MCP tool knows who to
whisper (see `docs/capture.md`):

```
OPS_OBSERVER_CHAR=<observer character name>
OPS_OBSERVER_WHISPER_BOT_GUID=20007   # optional, defaults to the leader
```

## Notes / caveats

- 3.3.5a Lua can set saved camera views and zoom/yaw but **not free pitch**
  (the mouse-look primitives are protected). Pre-`SaveView` a couple of useful
  angles in-game, then pass `view` (1–5) to `request_observer_screenshot`.
- Image automation is brittle to resolution/UI-scale changes — re-grab the
  button images if you change either.
- This launcher is **client-side** and is not built or deployed by CI/CD (same
  as the feedback-daemon and voice-command app).
