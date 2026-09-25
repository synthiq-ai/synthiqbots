# voice-command

Push-to-talk voice control for the WoW leader bot. Hold a hotkey, speak a
command, release — the leader bot's gateway brain interprets it, drives the
squad, and replies in character.

This is a **client-side companion** (like `apps/feedback-daemon`): it runs on
your Mac / gaming PC, not on the server, and is **not deployed** by CI/CD. It is
just microphone + local speech-to-text + one HTTP call to the `talk_to_leader`
MCP tool — no LLM brain of its own. The pipeline lives in `vc_core.py`, shared by
both front-ends below.

## Desktop app (recommended)

A simple dark UI with a Settings dialog (no hand-editing config) and first-run
model download. Download the installers from the **`voice-command-latest`**
GitHub release — `WoW-Voice-Command.dmg` (macOS) / `WoWVoiceCommand.exe`
(Windows, portable). Full guide: **[../../docs/voice-command-app.md](../../docs/voice-command-app.md)**.

Run the app from source:

```bash
python -m venv .venv && source .venv/bin/activate   # Windows: .\.venv\Scripts\Activate.ps1
pip install -r requirements.txt
python app.py
```

Build installers: `packaging/build-macos.sh` (DMG) / `packaging/build-windows.ps1` (EXE).

## CLI

```bash
python -m venv .venv && source .venv/bin/activate
pip install -r requirements.txt
cp config.toml.example config.toml                  # edit: mcp.url, bearer_token, command.player_name
python voice_command.py
```

Hold the hotkey (default **F9**), speak, release.

## Requires

The server-side tool, enabled with `OllamaChat.Mcp.AllowGatewayInjection = 1`.

Full setup, smoke test, STT-backend notes, and troubleshooting:
**[../../docs/voice-command.md](../../docs/voice-command.md)** (pipeline/CLI) and
**[../../docs/voice-command-app.md](../../docs/voice-command-app.md)** (the app).
