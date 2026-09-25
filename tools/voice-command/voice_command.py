#!/usr/bin/env python3
"""
voice_command.py — hands-free push-to-talk voice control for the WoW leader bot (CLI).

Pipeline (Route B — the leader bot's gateway brain is the interpreter):

    hold hotkey -> record mic -> local STT (faster-whisper / whisper.cpp)
        -> POST transcript to the `talk_to_leader` MCP tool (bearer auth)
        -> the leader's gateway brain (Claude) acts via game tools AND replies
        -> print + optionally speak the reply (TTS)

This is the terminal front-end. The pipeline lives in vc_core.py and is shared
with the desktop app (app.py). For the GUI, run `python app.py` instead.

Usage:
    python voice_command.py --config config.toml
"""

from __future__ import annotations

import argparse
import threading
import time
from pathlib import Path

from vc_core import (
    SAMPLE_RATE,
    Config,
    McpClient,
    Recorder,
    STT,
    TTS,
    resolve_key,
)


def main():
    ap = argparse.ArgumentParser(description="Push-to-talk voice control for the WoW leader bot.")
    ap.add_argument("--config", default=str(Path(__file__).with_name("config.toml")))
    args = ap.parse_args()

    cfg = Config.load(Path(args.config))
    cfg.validate()
    stt = STT(cfg)
    tts = TTS(cfg)
    mcp = McpClient(cfg)
    recorder = Recorder(cfg.input_device)
    hotkey = resolve_key(cfg.hotkey)
    from pynput import keyboard

    # Serialize transcription/POST off the keyboard thread so a fast double-tap
    # can't overlap two requests.
    busy = threading.Lock()
    held = {"down": False}

    def handle_release(audio):
        if audio.size < SAMPLE_RATE * 0.3:  # < 0.3s — accidental tap
            print("[skip] too short")
            return
        if not busy.acquire(blocking=False):
            print("[skip] still processing the previous command")
            return
        try:
            t0 = time.time()
            text = stt.transcribe(audio)
            stt_ms = int((time.time() - t0) * 1000)
            if not text:
                print(f"[stt] (empty)  {stt_ms}ms")
                return
            print(f"\n> you ({stt_ms}ms): {text}")
            res = mcp.talk_to_leader(text)
            if "error" in res:
                print(f"[error] {res['error']}")
                return
            reply = res.get("reply", "")
            extra = f"  [tools={res.get('used_tools')} {res.get('latency_ms')}ms via {res.get('gateway_type')}]"
            print(f"< claude: {reply}{extra}")
            tts.speak(reply)
        finally:
            busy.release()

    def on_press(key):
        if key == hotkey and not held["down"]:
            held["down"] = True
            recorder.start()

    def on_release(key):
        if key == hotkey and held["down"]:
            held["down"] = False
            audio = recorder.stop()
            threading.Thread(target=handle_release, args=(audio,), daemon=True).start()

    print("=" * 60)
    print(" Voice command ready.")
    print(f"   Hold [{cfg.hotkey.upper()}] and speak. Release to send.")
    print(f"   STT: {cfg.stt_backend}/{cfg.stt_model}  ->  {cfg.mcp_url}")
    print(f"   Speaking as: {cfg.player_name or cfg.player_guid}")
    print("   Ctrl+C to quit.")
    print("=" * 60)

    listener = keyboard.Listener(on_press=on_press, on_release=on_release)
    listener.start()
    try:
        listener.join()
    except KeyboardInterrupt:
        pass
    finally:
        recorder.close()
        print("\nbye.")


if __name__ == "__main__":
    main()
