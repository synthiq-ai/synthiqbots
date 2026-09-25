# Voice-commanding the leader bot

Speak commands to the in-game leader bot ("Claude", GUID 20007) in real time
while you play — no robot, no Telegram audio messages. Your voice becomes text
on your own PC, the text goes to the leader's gateway brain, and the brain
**acts** (drives the squad via the game tools) **and answers in character**.

```
mic -> STT (local whisper, your PC) -> text
    -> talk_to_leader MCP tool (bearer auth)
        -> leader gateway brain (Synthiq/OpenClaw, Claude subscription)
            -> calls game tools (squad acts) + writes an in-character reply
        -> reply returned to the client -> printed / spoken (TTS)
    -> local Ollama tactical loop executes any directive underneath
```

The client (`tools/voice-command/`) holds **no LLM brain of its own** — it is
microphone + speech-to-text + one HTTP call. All interpretation happens
server-side in the leader's gateway. This is "the Stack-chan robot's brain,
minus the hardware," and it runs on the machine you already game on.

> **Prefer a GUI?** There's a desktop app (Mac DMG / Windows EXE) with a Settings
> dialog and first-run model download — see **[voice-command-app.md](voice-command-app.md)**.
> This page documents the pipeline and the CLI; both share `vc_core.py`.

---

## Two ways in

### Phase 0 — zero code, today

Make your existing **text**-whisper-to-Claude flow voice-driven:

- **Windows:** focus WoW's whisper-to-Claude input, press **Win+H** (built-in
  on-device Voice Typing), speak, press Enter. For a nicer hotkey/accuracy use a
  whisper-based tool (e.g. WhisperWriter) that types into the focused field.
- **Mac:** superwhisper (local, push-to-talk) or built-in Dictation.

Limitation: you must focus the chat box (awkward mid-combat). Phase 1 removes that.

### Phase 1 — hands-free push-to-talk (this feature)

Server side: the `talk_to_leader` MCP tool (below). Client side:
`tools/voice-command/voice_command.py` — hold a hotkey, speak, release; the
squad acts and Claude answers, hands never leaving the game.

---

## Server side — enable `talk_to_leader`

The tool is always registered but **gated off** by default. Turn it on:

```ini
OllamaChat.Mcp.AllowGatewayInjection = 1
```

Then `config_reload` (or restart the worldserver). It ships via the normal
CI/CD deploy — never a manual build on game-host.

`talk_to_leader` delivers a message to the leader's gateway brain exactly as an
in-game whisper would (`OnPlayerCanUseChat` → `ProcessChat` → `QueryGatewayAPIWithTools`),
runs the full gateway tool-loop, and returns the brain's reply. It runs on the
MCP server's worker thread — already off the world thread, the same off-thread
context the in-game whisper path uses — so the per-tool-call pointer
revalidation in `DispatchGatewayTool` holds. It is **not** offered to the
gateway brain (absent from every gateway tool allowlist), so there's no
recursion. Full reference: [docs/gateway.md → Voice / chat-injection tool](gateway.md).

| Arg | Required | Meaning |
|---|---|---|
| `message` | yes | The spoken command / sentence, verbatim |
| `playerName` or `playerGuid` | yes (one) | The speaking player — drives the system prompt, master-session routing (AutoClaim), and the optional reply whisper |
| `botGuid` | no | Leader to talk to; defaults to the configured single leader |
| `emit_in_game` | no | `true` also whispers the reply into the player's WoW chat (default `false`) |

Returns `{reply, gateway_type, used_tools, latency_ms, whispered_in_game, ...}`.

### Smoke test (no client needed)

On the LAN, hit the internal IP (`192.168.100.11:18790`) — it skips the public DNS +
TLS + IP-allowlist hop and uses the same bearer token. Off-LAN, swap in
`https://mcp.wow.example.com/mcp`.

```bash
curl -s http://192.168.100.11:18790/mcp \
  -H "Authorization: Bearer $MCP_BEARER_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{
        "name":"talk_to_leader",
        "arguments":{"playerName":"Slayo",
          "message":"convert us to a raid and put the tanks in group 1"}}}'
```

You should get Claude's reply back, and in-game the party should become a raid
with tanks in subgroup 1 (verify with `get_group`). If it returns
`gateway injection disabled`, the config flag isn't set/reloaded.

---

## Client side — `tools/voice-command/`

Runs on the **gaming PC** (Windows + RTX 5080 → fast CUDA STT) or a Mac.

```bash
cd tools/voice-command
python -m venv .venv && . .venv/Scripts/activate     # Windows
#   ... or:  python3 -m venv .venv && source .venv/bin/activate   # mac/linux
pip install -r requirements.txt
cp config.toml.example config.toml                   # then edit it
python voice_command.py
```

Edit `config.toml`:
- `[mcp] url` + `bearer_token` — LAN `http://192.168.100.11:18790/mcp` (same subnet
  as the PC) or public `https://mcp.wow.example.com/mcp`; token = the server's
  `OllamaChat.Mcp.BearerToken`. **`config.toml` is gitignored — never commit it.**
- `[command] player_name` — your character (must be online).
- `[stt]` — Windows: `backend = "faster-whisper"`, `device = "cuda"`. Mac:
  `backend = "whispercpp"` (install `pywhispercpp`), `device` ignored.
- `[hotkey] key` — hold-to-talk key (default `f9`).
- `[tts] enabled` — `true` to hear Claude's reply through your speakers (native
  OS voice: macOS `say`, Windows SAPI5 — no extra package). Set `[tts] voice` to
  pick a voice (macOS: `say -v '?'` to list; install Enhanced/Siri voices in
  System Settings → Spoken Content. Windows: e.g. `"Microsoft Zira Desktop"`).

Then **hold the hotkey, speak a command, release.** Examples:
"everyone follow me", "mark the boar as skull", "ready check", "convert us to a
raid and put healers in group 2".

### STT backend notes
- **faster-whisper + CUDA** is the pick on NVIDIA (the 5080). A short command
  transcribes in well under a second; it's a tiny push-to-talk burst that won't
  dent the game. On a Mac, faster-whisper is CPU-only — use `whispercpp`
  (CoreML/ANE) instead so it doesn't fight the game GPU.
- Bigger models (`small.en`) are more accurate but slower; `base.en` is the
  sweet spot for short commands.

---

## Cost / brain

Both Synthiq and OpenClaw back the gateway with a **Claude subscription** (flat
plan, not per-token), so voice-commanding the leader has no per-call token cost.
Synthiq honors request-body `tools[]` (the in-game tool loop fires directly);
OpenClaw ignores `tools[]` but drives the game through its own MCP connection —
either way the leader acts and replies.

## Security
- `talk_to_leader` lets a bearer-holding client make the leader act + speak on a
  player's behalf. Keep `AllowGatewayInjection = 0` until you're using it; the
  MCP endpoint is bearer-auth + IP-allowlisted regardless.
- The `.conf` and `config.toml` carry tokens — never commit either.

## Troubleshooting
| Symptom | Cause / fix |
|---|---|
| `gateway injection disabled` | Set `OllamaChat.Mcp.AllowGatewayInjection = 1` + `config_reload` |
| `player not found / offline` | Your character must be logged in; check `player_name` |
| `leader bot not in world` | The leader bot (Claude 20007) isn't spawned/online |
| `gateway returned empty response` | Gateway backend down or timed out — check worldserver gateway logs |
| Slow / no STT on Mac | You're on faster-whisper (CPU-only on Mac) — switch to `whispercpp` |
| Hotkey does nothing (Mac) | Grant Accessibility permission to your terminal; use cmd/shift, not ctrl/alt |
