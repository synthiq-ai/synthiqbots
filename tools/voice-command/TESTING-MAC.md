# Testing the voice-command feature on a Mac (dev-first)

A Mac-first walkthrough for validating the voice-command path before moving it to
the Windows gaming PC. Everything here runs on an Apple Silicon Mac.

**You do NOT need WoW running on the Mac.** The bots live on the server; this
client is just *mic → speech-to-text → one HTTP call* to the MCP endpoint. You
verify the effect with MCP state tools (`get_group`, `get_fleet_status`) or the
`wow-admin` MCP — not by looking at a game window. The only thing that needs to
be "in-game" is the speaking player's identity, and only for some checks (below).

The one Mac-vs-Windows difference: **STT backend.** On Apple Silicon use
**whisper.cpp** (`pywhispercpp`, runs on the ANE/Metal). `faster-whisper` is
CPU-only on Mac (no CUDA) — don't use it here.

---

## 0. Prerequisite — the server tool must be live

`talk_to_leader` ships inside the worldserver, so it only exists after PR #261 is
merged + deployed. It's also gated off by default. So before any client testing:

1. **Merge PR #261 → deploy** (CI/CD; never a manual build on game-host).
2. **Enable the tool** — set `OllamaChat.Mcp.AllowGatewayInjection = 1`, then
   reload. Either via the `wow-admin` MCP (`file_set_key` + `config_reload`) or
   the in-game GM command `.ollama reload`.
3. Have the **MCP bearer token** + endpoint handy. On the LAN use the internal IP
   (your Mac is on the same `192.168.1.x` subnet as game-host's `ens18`):
   - **Gameplay MCP** (`talk_to_leader`, `get_group`): `http://192.168.100.11:18790/mcp`
   - **Ops/admin MCP** (`wow-admin`): `http://192.168.100.11:18791/mcp`
   - Off-LAN fallbacks: `https://mcp.wow.example.com/mcp` / `https://ops.wow.example.com/mcp`
   - Token = the server's `OllamaChat.Mcp.BearerToken`.

> Until step 1–2 are done, the client will get `gateway injection disabled` (good —
> that proves the bearer + endpoint work, you just need to flip the flag).

---

## 1. Smoke-test the server tool with `curl` (no client yet)

Validate the seam end-to-end before touching Python. Slayo (10001) doesn't even
need to be online — pass `playerGuid` directly:

```bash
export MCP_URL="http://192.168.100.11:18790/mcp"          # LAN (off-LAN: https://mcp.wow.example.com/mcp)
export MCP_TOKEN="...your bearer..."

curl -s "$MCP_URL" \
  -H "Authorization: Bearer $MCP_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{
        "name":"talk_to_leader",
        "arguments":{"playerGuid":10001,
          "message":"everyone follow me"}}}' | python3 -m json.tool
```

Expect a JSON envelope whose `result.content[0].text` is the tool result
(`{"ok":true,"reply":"...","gateway_type":"synthiq","used_tools":true,...}`).
Then confirm the squad reacted with a read tool, e.g.:

```bash
curl -s "$MCP_URL" -H "Authorization: Bearer $MCP_TOKEN" -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{
        "name":"get_group","arguments":{"botGuid":20007}}}' | python3 -m json.tool
```

If you get `gateway injection disabled` → flag not set/reloaded. If
`leader bot not in world` → Claude (20007) isn't spawned/online.

---

## 2. Set up the Python client on the Mac

```bash
cd tools/voice-command
python3 -m venv .venv && source .venv/bin/activate

# Core + the Mac STT backend (whisper.cpp, NOT faster-whisper):
brew install ffmpeg
pip install sounddevice numpy pynput pywhispercpp
# TTS needs no package on macOS — it uses the built-in `say` command.
```

(That's `requirements.txt` minus `faster-whisper`, plus `pywhispercpp`. The
bundled `requirements.txt` defaults to the Windows backend.)

Create the config:

```bash
cp config.toml.example config.toml
```

Edit `config.toml` for the Mac:

```toml
[mcp]
url = "http://192.168.100.11:18790/mcp"      # LAN (off-LAN: https://mcp.wow.example.com/mcp)
bearer_token = "...your bearer..."         # config.toml is gitignored

[command]
player_name = "Slayo"        # online player; or use player_guid = 10001
bot_guid = 0                 # 0 = server's configured leader (Claude 20007)
emit_in_game = false         # true also whispers the reply into WoW chat (player must be online)

[stt]
backend = "whispercpp"       # <-- Mac: whisper.cpp on the ANE (NOT faster-whisper)
model = "base.en"            # tiny.en is faster, small.en more accurate
# device / compute_type are ignored by the whispercpp backend

[hotkey]
key = "f9"                   # hold-to-talk

[tts]
enabled = false              # true = speak Claude's reply via the built-in macOS `say`
# voice = "Ava (Enhanced)"   # `say -v '?'` to list; install Enhanced/Siri voices in
                             #  System Settings > Spoken Content > System Voice > Manage Voices
# rate = 180                 # words per minute
```

---

## 3. Grant the two macOS permissions (the usual gotchas)

Both are for the app you launch Python from (Terminal.app / iTerm / VS Code):

1. **Microphone** — System Settings → Privacy & Security → **Microphone** →
   enable your terminal. Without it `sounddevice` records silence (empty
   transcripts).
2. **Accessibility** — System Settings → Privacy & Security → **Accessibility** →
   enable your terminal. Without it the `pynput` global hotkey never fires.

Use a function key (`f9`) or a **cmd/shift** combo — `ctrl`/`alt` modifiers don't
fire globally via pynput on macOS. The first whisper.cpp run downloads/compiles
the model (slow once, fast after).

---

## 4. Run it

```bash
python voice_command.py
```

Hold **F9**, say *"everyone follow me"*, release. You should see:

```
> you (NNNms): everyone follow me
< claude: On my way!  [tools=True NNNms via synthiq]
```

Try a few: *"mark the boar as skull"*, *"ready check"*, *"convert us to a raid
and put the tanks in group 1"*. Verify the in-game effect via `get_group` /
`get_fleet_status` (or in WoW if you happen to be logged in).

---

## What's worth verifying on the Mac (before the PC)

- **The plumbing**: hotkey → record → transcribe → POST → reply printed. This is
  identical on Mac and PC; only the STT backend differs.
- **Transcription accuracy** of your common commands at `base.en` vs `small.en`.
- **The MCP round-trip + auth** against the live server.
- **TTS** (if you'll use it) — the built-in macOS `say` voice; confirm it speaks,
  and try a nicer voice via `[tts] voice` (e.g. an Enhanced voice).

What you **can't** fully judge on the Mac: STT latency (the PC's RTX 5080 with
CUDA `faster-whisper` is faster than the Mac ANE) and gaming-concurrency (the PC
runs the game + whisper together). Those are PC-only checks — but the logic is
the same, so a green Mac run means the PC just needs the backend swap
(`backend = "faster-whisper"`, `device = "cuda"`).

---

## Troubleshooting (Mac-specific)

| Symptom | Fix |
|---|---|
| Empty / silent transcript | Microphone permission for your terminal app |
| Hotkey does nothing | Accessibility permission; use `f9` or `cmd+shift`, not `ctrl`/`alt` |
| `pywhispercpp` install fails | `brew install ffmpeg cmake`, then `pip install pywhispercpp` |
| `gateway injection disabled` | Server flag `AllowGatewayInjection=1` not set/reloaded |
| `player not found / offline` | Use `player_guid` instead of `player_name`, or log the char in |
| `leader bot not in world` | Claude (20007) isn't spawned/online on the server |
| Slow first transcription | whisper.cpp compiles the model on first run — subsequent runs are fast; use `tiny.en`/`base.en` |
| TTS silent | Test the engine directly: `echo hi \| say -f -`. Check `[tts] voice` exists in `say -v '?'` |

See the full feature runbook: [../../docs/voice-command.md](../../docs/voice-command.md).
