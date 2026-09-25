# WoW Voice Command — desktop app

A small dark HUD for the [voice-command](voice-command.md) feature: hold a hotkey,
speak a command, release. The in-game **Claude leader bot** interprets it, drives
the squad, and replies in character — optionally spoken back to you.

This is the GUI front-end for the same pipeline as the `voice_command.py` CLI
(`tools/voice-command/`). It is a **personal companion app** — it runs on your
Mac or Windows gaming PC, **not** on the server, and is built/distributed via its
own GitHub release, never the server deploy.

```
hold hotkey → record mic → local speech-to-text
   → talk_to_leader MCP tool (bearer auth) → the leader's gateway brain (Claude)
   → squad acts + Claude replies → shown in the app, optionally spoken (TTS)
```

---

## Install

Download the latest installers from the **`voice-command-latest`** GitHub release
(auto-built by `.github/workflows/build-voice-command.yml`):

- **macOS** — `WoW-Voice-Command.dmg`
- **Windows** — `WoWVoiceCommand.exe`

The binaries are **unsigned** (personal use), so each OS shows a one-time warning.

### macOS (DMG)

1. Open `WoW-Voice-Command.dmg` and drag **WoW Voice Command** to **Applications**.
2. First launch: **right-click the app → Open → Open** (Gatekeeper blocks
   double-click for unsigned apps).
3. Grant permissions when prompted (or in **System Settings → Privacy & Security**):
   - **Microphone** — required to capture your voice.
   - **Accessibility** — required for the **global** push-to-talk hotkey to work
     while WoW is focused. Without it, the on-screen HOLD button still works.

### Windows (portable EXE)

1. Run `WoWVoiceCommand.exe` — it's portable, no install, no admin.
2. If **SmartScreen** warns: **More info → Run anyway**.
3. Allow microphone access if Windows prompts.

---

## First run

1. The app opens **Settings** automatically (no config yet). Fill in:
   - **MCP URL** — `http://192.168.100.11:18790/mcp` on the LAN, or
     `https://mcp.wow.example.com/mcp` (public, IP-allowlisted).
   - **Bearer token** — must equal `OllamaChat.Mcp.BearerToken` on the server.
   - **Player name** (e.g. `Slayo`) or **GUID** — who you speak as.
   - Pick the **hotkey** (default **F9**), **microphone**, and **TTS** voice if you
     want spoken replies.
2. On **Save**, the app downloads the chosen **speech model** on first use
   (`base.en`, ~75–150 MB) and shows *Downloading speech model…*. This happens
   once; the model is cached. The HOLD button is disabled until it's ready.
3. Config is saved to your user dir (survives app updates):
   - macOS: `~/Library/Application Support/WoWVoiceCommand/config.toml`
   - Windows: `%APPDATA%\WoWVoiceCommand\config.toml`

> The config file carries the bearer token. It lives outside the repo and is
> never committed.

---

## Using it

- **Click `Test connection`** first — green dot = the MCP endpoint is reachable and
  the token is valid. (It does **not** prove the WoW world is up; see below.)
- **Hold the hotkey** (or press-and-hold the on-screen button), speak a command
  ("everyone follow me", "mark the boar as skull", "convert us to a raid and put
  tanks in group 1"), release. The transcript shows `you:` / `claude:` with a
  `[tools=… latency via gateway]` line, and the reply is spoken if TTS is on.
- The window is a **HUD** — you don't click it mid-combat. The global hotkey fires
  while WoW is focused (needs macOS Accessibility permission).

---

## Settings reference

| Section | Field | Notes |
|---|---|---|
| Connection | MCP URL, Bearer token, Timeout | Token must match the server. |
| Speaking as | Player name / GUID, Leader GUID, Whisper-in-game | GUID overrides name. Leader `0` = server default (Claude 20007). |
| Speech-to-text | Backend, Model, Language, Microphone | **whisper.cpp** default. **faster-whisper** appears only when an NVIDIA **CUDA** GPU is detected. |
| Hotkey | Push-to-talk key | `f1`–`f12` or a single character; held to talk. |
| Spoken reply | Enable, Voice, Rate | Native OS voices (macOS `say` / Windows SAPI5). |

### faster-whisper vs whisper.cpp

The app ships **both**. **whisper.cpp** is the default everywhere — self-contained,
Metal-accelerated on Mac, CPU on Windows; ~1–2 s for a short clip. **faster-whisper**
is offered **only when CUDA is present** (sub-second on the RTX 5080); it needs the
NVIDIA **CUDA + cuDNN** runtime already installed on the machine — those libraries
are **not** bundled.

---

## Troubleshooting

| Symptom | Fix |
|---|---|
| HOLD button stuck on "Loading model…" | First-run model download; wait, or check internet. Status shows the model name. |
| Global hotkey does nothing while in WoW (macOS) | Grant **Accessibility** to the app in System Settings → Privacy & Security. |
| `connection failed` on Test | Wrong URL/token, or you're off the LAN. Try the public URL; confirm the bearer token matches the server. |
| Test is green but commands time out / "empty response" | The **WoW world server** is down (it runs on a daily up/down cycle) or `OllamaChat.Mcp.AllowGatewayInjection` is off. Test-connection only checks MCP reachability, not world liveness. |
| "too short — hold longer" | Hold the key ≥ ~0.3 s while speaking. |
| No spoken reply | Enable TTS in Settings; pick an installed voice. |

## Build from source

See `tools/voice-command/packaging/` — `build-macos.sh` (DMG) and
`build-windows.ps1` (EXE), both driving `voice-command.spec` (PyInstaller). The
CLI usage and pipeline internals are documented in [voice-command.md](voice-command.md).
