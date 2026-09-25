#!/usr/bin/env python3
"""
app.py — WoW Voice Command, the desktop app.

A small dark HUD around the voice-command pipeline (vc_core.py): hold the hotkey
(or the on-screen button), speak, release. The leader bot's gateway brain acts on
the squad and replies; the reply is shown and optionally spoken (TTS).

The window is a heads-up display — you don't click it mid-combat. The global
hotkey works while WoW is focused. Settings replaces hand-editing config.toml.

Run from source:   python app.py
Packaged:          launch "WoW Voice Command" (see docs/voice-command-app.md)
"""

from __future__ import annotations

import sys
import threading
import time
from pathlib import Path

try:
    import customtkinter as ctk
except ModuleNotFoundError:
    sys.stderr.write("Missing UI dependency. Install with:  pip install -r requirements.txt\n")
    raise

from vc_core import (
    APP_NAME,
    SAMPLE_RATE,
    Config,
    McpClient,
    Recorder,
    STT,
    TTS,
    list_input_devices,
    make_hotkey_listener,
    validate_hotkey,
)

ACCENT = "#3B8ED0"
GREEN = "#2EA043"
RED = "#D14B4B"
AMBER = "#D9A441"
GREY = "#6E7681"

MODELS = ["tiny.en", "base.en", "small.en"]


# ==============================================================================
# Settings dialog
# ==============================================================================
class SettingsDialog(ctk.CTkToplevel):
    """Form over every Config field. On Save, builds a new Config, persists it to
    the user config dir, and invokes on_save(cfg)."""

    def __init__(self, master, cfg: Config, on_save):
        super().__init__(master)
        self.title("Settings — WoW Voice Command")
        self.geometry("520x720")
        self.cfg = cfg
        self.on_save = on_save
        self._cuda = STT.cuda_available()

        self.grid_columnconfigure(0, weight=1)
        frame = ctk.CTkScrollableFrame(self)
        frame.grid(row=0, column=0, sticky="nsew", padx=12, pady=(12, 6))
        self.grid_rowconfigure(0, weight=1)
        frame.grid_columnconfigure(1, weight=1)
        self._row = 0
        self._frame = frame

        # --- Connection -------------------------------------------------------
        self._section("Connection")
        self.url = self._entry("MCP URL", cfg.mcp_url, placeholder="http://192.168.100.11:18790/mcp")
        self.token = self._entry("Bearer token", cfg.bearer_token, show="•")
        self.timeout = self._entry("Timeout (s)", str(int(cfg.timeout_s)))

        # --- Who you are ------------------------------------------------------
        self._section("Speaking as")
        self.player_name = self._entry("Player name", cfg.player_name, placeholder="Slayo")
        self.player_guid = self._entry("…or player GUID", str(cfg.player_guid or ""))
        self.bot_guid = self._entry("Leader GUID (0 = default)", str(cfg.bot_guid))
        self.emit_in_game = self._switch("Also whisper reply in WoW chat", cfg.emit_in_game)

        # --- Speech-to-text ---------------------------------------------------
        self._section("Speech-to-text")
        backends = ["whispercpp"]
        if self._cuda or cfg.stt_backend == "faster-whisper":
            backends.append("faster-whisper")
        self.backend = self._option("Backend", backends,
                                     cfg.stt_backend if cfg.stt_backend in backends else "whispercpp")
        if not self._cuda:
            self._hint("faster-whisper (CUDA) needs an NVIDIA GPU — not detected here. "
                       "whisper.cpp is recommended.")
        self.model = self._option("Model", MODELS, cfg.stt_model if cfg.stt_model in MODELS else "base.en")
        self.language = self._entry("Language", cfg.stt_language or "en")
        mics = [("-1", "System default")] + [(str(i), n) for i, n in list_input_devices()]
        self._mic_map = {label: idx for idx, label in mics}
        cur_mic = next((lbl for idx, lbl in mics if idx == str(cfg.input_device)), "System default")
        self.mic = self._option("Microphone", [lbl for _, lbl in mics], cur_mic)

        # --- Hotkey -----------------------------------------------------------
        self._section("Hotkey")
        self.hotkey = self._entry("Push-to-talk key", cfg.hotkey, placeholder="f9")
        self._hint("f1–f12 or a single character. Held to talk.")

        # --- Voice (TTS) ------------------------------------------------------
        self._section("Spoken reply (TTS)")
        self.tts_enabled = self._switch("Speak Claude's reply", cfg.tts_enabled)
        voices = TTS.list_voices()
        voice_opts = ["(system default)"] + voices
        cur_voice = cfg.tts_voice if cfg.tts_voice in voices else "(system default)"
        self.tts_voice = self._option("Voice", voice_opts, cur_voice)
        self.tts_rate = self._entry("macOS rate (wpm)", str(cfg.tts_rate))
        self.tts_windows_rate = self._entry("Windows rate (-10..10)", str(cfg.tts_windows_rate))

        # --- buttons ----------------------------------------------------------
        self.err = ctk.CTkLabel(self, text="", text_color=RED, wraplength=480)
        self.err.grid(row=1, column=0, sticky="ew", padx=12)
        btns = ctk.CTkFrame(self, fg_color="transparent")
        btns.grid(row=2, column=0, sticky="ew", padx=12, pady=10)
        btns.grid_columnconfigure((0, 1), weight=1)
        ctk.CTkButton(btns, text="Cancel", fg_color=GREY, command=self.destroy).grid(
            row=0, column=0, padx=(0, 6), sticky="ew")
        ctk.CTkButton(btns, text="Save", command=self._save).grid(
            row=0, column=1, padx=(6, 0), sticky="ew")

        self.after(80, self.lift)
        self.grab_set()

    # --- tiny form helpers ----------------------------------------------------
    def _section(self, title: str):
        lbl = ctk.CTkLabel(self._frame, text=title, font=ctk.CTkFont(size=13, weight="bold"),
                           text_color=ACCENT, anchor="w")
        lbl.grid(row=self._row, column=0, columnspan=2, sticky="ew", pady=(12, 2))
        self._row += 1

    def _entry(self, label, value, placeholder="", show=None):
        ctk.CTkLabel(self._frame, text=label, anchor="w").grid(
            row=self._row, column=0, sticky="w", padx=(0, 8), pady=3)
        e = ctk.CTkEntry(self._frame, placeholder_text=placeholder)
        if show:
            e.configure(show=show)
        if value:
            e.insert(0, value)
        e.grid(row=self._row, column=1, sticky="ew", pady=3)
        self._row += 1
        return e

    def _option(self, label, values, current):
        ctk.CTkLabel(self._frame, text=label, anchor="w").grid(
            row=self._row, column=0, sticky="w", padx=(0, 8), pady=3)
        var = ctk.StringVar(value=current)
        om = ctk.CTkOptionMenu(self._frame, values=values, variable=var)
        om.grid(row=self._row, column=1, sticky="ew", pady=3)
        self._row += 1
        return var

    def _switch(self, label, value):
        var = ctk.BooleanVar(value=value)
        sw = ctk.CTkSwitch(self._frame, text=label, variable=var)
        sw.grid(row=self._row, column=0, columnspan=2, sticky="w", pady=4)
        self._row += 1
        return var

    def _hint(self, text):
        ctk.CTkLabel(self._frame, text=text, text_color=GREY, anchor="w",
                     font=ctk.CTkFont(size=11), wraplength=460).grid(
            row=self._row, column=0, columnspan=2, sticky="w", pady=(0, 4))
        self._row += 1

    # --- save -----------------------------------------------------------------
    def _save(self):
        data = {
            "mcp": {
                "url": self.url.get().strip(),
                "bearer_token": self.token.get().strip(),
                "timeout_seconds": _int(self.timeout.get(), 90),
            },
            "command": {
                "player_name": self.player_name.get().strip(),
                "player_guid": _int(self.player_guid.get(), 0),
                "bot_guid": _int(self.bot_guid.get(), 0),
                "emit_in_game": bool(self.emit_in_game.get()),
            },
            "stt": {
                "backend": self.backend.get(),
                "model": self.model.get(),
                "device": "auto",
                "compute_type": "int8",
                "language": self.language.get().strip() or "en",
                "input_device": _int(self._mic_map.get(self.mic.get(), "-1"), -1),
            },
            "hotkey": {"key": self.hotkey.get().strip() or "f9"},
            "tts": {
                "enabled": bool(self.tts_enabled.get()),
                "voice": "" if self.tts_voice.get() == "(system default)" else self.tts_voice.get(),
                "rate": _int(self.tts_rate.get(), 180),
                "windows_rate": _int(self.tts_windows_rate.get(), 0),
            },
        }
        cfg = Config(data)
        missing = cfg.missing_required()
        if missing:
            self.err.configure(text="Still need: " + ", ".join(missing))
            return
        try:
            validate_hotkey(cfg.hotkey)
        except ValueError as exc:
            self.err.configure(text=str(exc))
            return
        cfg.save()
        self.on_save(cfg)
        self.destroy()


def _int(s, default):
    try:
        return int(str(s).strip())
    except (ValueError, TypeError):
        return default


# ==============================================================================
# Main HUD window
# ==============================================================================
class VoiceApp(ctk.CTk):
    def __init__(self):
        super().__init__()
        self.title("WoW Voice Command")
        self.geometry("440x520")
        self.minsize(380, 460)

        self.cfg: Config = Config.load_or_default()
        self.stt: STT | None = None   # set in _rebuild_pipeline
        self.tts: TTS | None = None
        self.mcp: McpClient | None = None
        self.recorder: Recorder | None = None
        self.hotkey_listener = None
        self._held = False
        self._busy = threading.Lock()
        self._ready = False  # model loaded

        self._build_ui()
        self._rebuild_pipeline()

        self.protocol("WM_DELETE_WINDOW", self._on_close)

        # First run / incomplete config → open Settings.
        if self.cfg.missing_required():
            self.after(300, self.open_settings)
        else:
            self._warm_up_model()

    # --- UI -------------------------------------------------------------------
    def _build_ui(self):
        ctk.set_appearance_mode("dark")
        ctk.set_default_color_theme("blue")
        self.grid_columnconfigure(0, weight=1)
        self.grid_rowconfigure(3, weight=1)

        # Status row
        top = ctk.CTkFrame(self, fg_color="transparent")
        top.grid(row=0, column=0, sticky="ew", padx=16, pady=(14, 4))
        top.grid_columnconfigure(1, weight=1)
        self.dot = ctk.CTkLabel(top, text="●", text_color=GREY, font=ctk.CTkFont(size=18))
        self.dot.grid(row=0, column=0, padx=(0, 8))
        self.conn_label = ctk.CTkLabel(top, text="not configured", anchor="w")
        self.conn_label.grid(row=0, column=1, sticky="w")

        # Push-to-talk button
        self.talk = ctk.CTkButton(
            self, text="HOLD  F9  🎙", height=92,
            font=ctk.CTkFont(size=22, weight="bold"),
            fg_color=ACCENT, hover=False, command=None,
        )
        self.talk.grid(row=1, column=0, sticky="ew", padx=24, pady=(8, 4))
        self.talk.bind("<ButtonPress-1>", lambda e: self.start_capture())
        self.talk.bind("<ButtonRelease-1>", lambda e: self.stop_capture())

        # Status line
        self.status = ctk.CTkLabel(self, text="idle", text_color=GREY)
        self.status.grid(row=2, column=0, sticky="ew", padx=16, pady=(0, 6))

        # Transcript log
        self.log = ctk.CTkTextbox(self, wrap="word", font=ctk.CTkFont(size=13))
        self.log.grid(row=3, column=0, sticky="nsew", padx=16, pady=6)
        self.log.configure(state="disabled")

        # Bottom buttons
        bottom = ctk.CTkFrame(self, fg_color="transparent")
        bottom.grid(row=4, column=0, sticky="ew", padx=16, pady=(4, 14))
        bottom.grid_columnconfigure((0, 1), weight=1)
        ctk.CTkButton(bottom, text="⚙  Settings", fg_color=GREY, command=self.open_settings).grid(
            row=0, column=0, padx=(0, 6), sticky="ew")
        self.test_btn = ctk.CTkButton(bottom, text="Test connection", command=self.test_connection)
        self.test_btn.grid(row=0, column=1, padx=(6, 0), sticky="ew")

    # --- pipeline lifecycle ---------------------------------------------------
    def _rebuild_pipeline(self):
        """(Re)create pipeline objects from self.cfg. Called on start and after Save."""
        # Tear down old recorder/listener.
        if self.recorder:
            self.recorder.close()
            self.recorder = None
        if self.hotkey_listener:
            try:
                self.hotkey_listener.stop()
            except Exception:
                pass
            self.hotkey_listener = None

        self.stt = STT(self.cfg)
        self.tts = TTS(self.cfg)
        self.mcp = McpClient(self.cfg)

        label = self.cfg.player_name or (str(self.cfg.player_guid) if self.cfg.player_guid else "?")
        self.conn_label.configure(text=f"{label} → leader" if label != "?" else "not configured")
        self.talk.configure(text=f"HOLD  {self.cfg.hotkey.upper()}  🎙")

        if self.cfg.missing_required():
            self._set_dot(GREY)
            return

        try:
            self.recorder = Recorder(self.cfg.input_device)
        except Exception as exc:
            self._log_line("system", f"could not open microphone: {exc}")
            self._set_dot(RED)
            return
        try:
            # No-arg callbacks; the listener filters for the configured hotkey and
            # we marshal back onto the Tk thread.
            self.hotkey_listener = make_hotkey_listener(
                self.cfg.hotkey,
                lambda: self.after(0, self.start_capture),
                lambda: self.after(0, self.stop_capture),
                on_error=lambda m: self.after(0, lambda: self._log_line("system", m)),
            )
            self.hotkey_listener.start()
        except Exception as exc:
            # The on-screen HOLD button still works without the global hotkey.
            self._log_line("system", f"global hotkey unavailable ({exc}); use the on-screen button "
                                     "or grant Accessibility permission")
        self._set_dot(AMBER)

    def _warm_up_model(self):
        """Load (download on first run) the STT model up front so the first
        command isn't slow. Runs off the UI thread."""
        if self.cfg.missing_required():
            return
        self._ready = False
        self._set_talk_enabled(False)

        def work():
            try:
                self.stt.ensure(status=lambda m: self.after(0, lambda: self._set_status(m, AMBER)))
                self.after(0, self._model_ready)
            except Exception as exc:
                self.after(0, lambda: self._set_status(f"model load failed: {exc}", RED))

        threading.Thread(target=work, daemon=True).start()

    def _model_ready(self):
        self._ready = True
        self._set_talk_enabled(True)
        self._set_status("ready — hold the hotkey and speak", GREEN)
        self._set_dot(GREEN if not self.cfg.missing_required() else GREY)

    # --- capture --------------------------------------------------------------
    def start_capture(self):
        if not self._ready or not self.recorder or self._held:
            return
        self._held = True
        self.recorder.start()
        self.talk.configure(fg_color=GREEN)
        self._set_status("listening…", GREEN)

    def stop_capture(self):
        if not self._held or not self.recorder:
            return
        self._held = False
        audio = self.recorder.stop()
        self.talk.configure(fg_color=ACCENT)
        threading.Thread(target=self._process, args=(audio,), daemon=True).start()

    def _process(self, audio):
        if audio is None or audio.size < SAMPLE_RATE * 0.3:
            self.after(0, lambda: self._set_status("too short — hold longer", AMBER))
            return
        if not self._busy.acquire(blocking=False):
            self.after(0, lambda: self._set_status("still processing previous…", AMBER))
            return
        try:
            self.after(0, lambda: self._set_status("transcribing…", AMBER))
            t0 = time.time()
            text = self.stt.transcribe(audio)
            stt_ms = int((time.time() - t0) * 1000)
            if not text:
                self.after(0, lambda: self._set_status(f"(heard nothing)  {stt_ms}ms", AMBER))
                return
            self.after(0, lambda t=text: self._log_line("you", t))
            self.after(0, lambda: self._set_status("waiting for Claude…", AMBER))
            res = self.mcp.talk_to_leader(text)
            if "error" in res:
                self.after(0, lambda e=res["error"]: (self._log_line("error", e),
                                                       self._set_status("error", RED)))
                return
            reply = res.get("reply", "")
            meta = f"tools={res.get('used_tools')} · {res.get('latency_ms')}ms · {res.get('gateway_type')}"
            self.after(0, lambda r=reply, m=meta: (self._log_line("claude", r, m),
                                                   self._set_status("ready", GREEN)))
            if reply:
                self.after(0, lambda: self._set_status("speaking…", GREEN))
                self.tts.speak(reply)
                self.after(0, lambda: self._set_status("ready", GREEN))
        finally:
            self._busy.release()

    # --- actions --------------------------------------------------------------
    def open_settings(self):
        SettingsDialog(self, self.cfg, self._on_settings_saved)

    def _on_settings_saved(self, cfg: Config):
        self.cfg = cfg
        self._rebuild_pipeline()
        self._log_line("system", f"settings saved → {Config.default_path()}")
        self._warm_up_model()

    def test_connection(self):
        if self.cfg.missing_required():
            self.open_settings()
            return
        self.test_btn.configure(state="disabled", text="Testing…")
        self._set_status("testing connection…", AMBER)

        def work():
            res = self.mcp.talk_to_leader("connection test — reply with a short ok")
            ok = "error" not in res
            def done():
                self.test_btn.configure(state="normal", text="Test connection")
                if ok:
                    self._set_dot(GREEN)
                    self._set_status("connection ok", GREEN)
                    self._log_line("claude", res.get("reply", "(ok)"))
                else:
                    self._set_dot(RED)
                    self._set_status("connection failed", RED)
                    self._log_line("error", res.get("error", "unknown"))
            self.after(0, done)

        threading.Thread(target=work, daemon=True).start()

    # --- small UI utils -------------------------------------------------------
    def _set_dot(self, color):
        self.dot.configure(text_color=color)

    def _set_status(self, text, color=GREY):
        self.status.configure(text=text, text_color=color)

    def _set_talk_enabled(self, enabled: bool):
        self.talk.configure(state="normal" if enabled else "disabled",
                            text=(f"HOLD  {self.cfg.hotkey.upper()}  🎙" if enabled else "Loading model…"))

    def _log_line(self, who, text, meta=""):
        prefix = {"you": "🗣  you", "claude": "🤖  claude", "error": "⚠  error",
                  "system": "•  system"}.get(who, who)
        self.log.configure(state="normal")
        self.log.insert("end", f"{prefix}: {text}\n")
        if meta:
            self.log.insert("end", f"      [{meta}]\n")
        self.log.see("end")
        self.log.configure(state="disabled")

    def _on_close(self):
        if self.recorder:
            self.recorder.close()
        if self.hotkey_listener:
            try:
                self.hotkey_listener.stop()
            except Exception:
                pass
        self.destroy()


def main():
    app = VoiceApp()
    app.mainloop()


if __name__ == "__main__":
    main()
