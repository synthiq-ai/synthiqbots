#!/usr/bin/env python3
"""
vc_core.py — the voice-command pipeline, shared by the CLI (voice_command.py)
and the desktop app (app.py).

Pipeline (Route B — the leader bot's gateway brain is the interpreter):

    record mic -> local STT (faster-whisper / whisper.cpp)
        -> POST transcript to the `talk_to_leader` MCP tool (bearer auth)
        -> the leader's gateway brain (Claude) acts via game tools AND replies
        -> caller prints / speaks the reply (TTS)

This module holds NO LLM brain: microphone + speech-to-text + one HTTP call.
All command interpretation happens server-side in the leader bot's gateway.
See docs/voice-command.md and docs/voice-command-app.md.
"""

from __future__ import annotations

import json
import os
import platform
import shutil
import subprocess
import sys
import threading
import urllib.error
import urllib.request
from pathlib import Path
from typing import Callable, Optional

# --- tomllib (3.11+) with a tomli fallback ------------------------------------
try:
    import tomllib  # type: ignore
except ModuleNotFoundError:  # pragma: no cover - py<3.11
    try:
        import tomli as tomllib  # type: ignore
    except ModuleNotFoundError:
        print("Need Python 3.11+ (tomllib) or `pip install tomli`.", file=sys.stderr)
        raise

import numpy as np
import sounddevice as sd
from pynput import keyboard

SAMPLE_RATE = 16000  # whisper models expect 16 kHz mono
CHANNELS = 1

APP_NAME = "WoWVoiceCommand"  # user-config / user-data directory name


# ==============================================================================
# Config
# ==============================================================================
class Config:
    """Typed view over config.toml. Round-trips via to_toml()/save() so the GUI
    Settings dialog can edit and persist without losing the helpful comments."""

    def __init__(self, data: dict):
        mcp = data.get("mcp", {})
        self.mcp_url: str = str(mcp.get("url", "")).rstrip("/")
        self.bearer_token: str = mcp.get("bearer_token", "")
        self.timeout_s: float = float(mcp.get("timeout_seconds", 180))

        cmd = data.get("command", {})
        self.player_name: str = cmd.get("player_name", "")
        self.player_guid: int = int(cmd.get("player_guid", 0) or 0)
        self.bot_guid: int = int(cmd.get("bot_guid", 0) or 0)  # 0 = server default leader
        self.emit_in_game: bool = bool(cmd.get("emit_in_game", False))

        stt = data.get("stt", {})
        self.stt_backend: str = stt.get("backend", "whispercpp")  # whispercpp | faster-whisper
        self.stt_model: str = stt.get("model", "base.en")
        self.stt_device: str = stt.get("device", "auto")  # cuda | cpu | auto (faster-whisper)
        self.stt_compute_type: str = stt.get("compute_type", "int8")
        self.stt_language: str = stt.get("language", "en")
        self.input_device: int = int(stt.get("input_device", -1))  # -1 = system default

        hk = data.get("hotkey", {})
        self.hotkey: str = hk.get("key", "f9")  # held to talk

        tts = data.get("tts", {})
        self.tts_enabled: bool = bool(tts.get("enabled", False))
        self.tts_voice: str = tts.get("voice", "")          # OS voice name; "" = system default
        self.tts_rate: int = int(tts.get("rate", 180))      # macOS `say` words-per-minute
        self.tts_windows_rate: int = int(tts.get("windows_rate", 0))  # Windows SAPI rate, -10..10

    # --- validity -------------------------------------------------------------
    def missing_required(self) -> list[str]:
        """Human-readable list of fields that must be set before the app can run.
        Used by the GUI to decide whether to open Settings on first launch."""
        missing = []
        if not self.mcp_url:
            missing.append("MCP URL")
        if not self.bearer_token or self.bearer_token.startswith("REPLACE_"):
            missing.append("bearer token")
        if not self.player_name and not self.player_guid:
            missing.append("player name or GUID")
        return missing

    def validate(self) -> None:
        missing = self.missing_required()
        if missing:
            raise ValueError("config incomplete — set: " + ", ".join(missing))

    # --- load/save ------------------------------------------------------------
    @classmethod
    def load(cls, path: Path) -> "Config":
        with open(path, "rb") as f:
            return cls(tomllib.load(f))

    @classmethod
    def load_or_default(cls, path: Optional[Path] = None) -> "Config":
        """Load the user config, falling back to a blank/default Config if the
        file doesn't exist yet (first run)."""
        path = path or cls.default_path()
        if path.exists():
            return cls.load(path)
        return cls({})

    @staticmethod
    def user_dir() -> Path:
        """Per-user config directory (survives outside the app bundle / portable exe)."""
        try:
            from platformdirs import user_config_dir  # type: ignore
            return Path(user_config_dir(APP_NAME))
        except ModuleNotFoundError:  # pragma: no cover - fallback if platformdirs absent
            system = platform.system()
            if system == "Windows":
                base = os.environ.get("APPDATA", str(Path.home() / "AppData" / "Roaming"))
                return Path(base) / APP_NAME
            if system == "Darwin":
                return Path.home() / "Library" / "Application Support" / APP_NAME
            base = os.environ.get("XDG_CONFIG_HOME", str(Path.home() / ".config"))
            return Path(base) / APP_NAME

    @classmethod
    def default_path(cls) -> Path:
        return cls.user_dir() / "config.toml"

    def to_toml(self) -> str:
        """Serialize back to a commented config.toml (stable key order)."""
        def b(v: bool) -> str:
            return "true" if v else "false"

        def s(v: str) -> str:
            return '"' + str(v).replace("\\", "\\\\").replace('"', '\\"') + '"'

        lines = [
            "# WoW Voice Command — saved by the app. config.toml carries the bearer token; never commit it.",
            "",
            "[mcp]",
            f"url = {s(self.mcp_url)}",
            f"bearer_token = {s(self.bearer_token)}",
            f"timeout_seconds = {int(self.timeout_s)}",
            "",
            "[command]",
            f"player_name = {s(self.player_name)}",
            f"player_guid = {self.player_guid}",
            f"bot_guid = {self.bot_guid}          # 0 = server's configured single leader (Claude 20007)",
            f"emit_in_game = {b(self.emit_in_game)}",
            "",
            "[stt]",
            f"backend = {s(self.stt_backend)}    # whispercpp | faster-whisper",
            f"model = {s(self.stt_model)}        # tiny.en | base.en | small.en",
            f"device = {s(self.stt_device)}      # cuda | cpu | auto (faster-whisper only)",
            f"compute_type = {s(self.stt_compute_type)}  # float16 (GPU) | int8 (CPU)",
            f"language = {s(self.stt_language)}",
            f"input_device = {self.input_device}        # -1 = system default mic",
            "",
            "[hotkey]",
            f"key = {s(self.hotkey)}             # hold to talk; f1-f12 or a single character",
            "",
            "[tts]",
            f"enabled = {b(self.tts_enabled)}",
            f"voice = {s(self.tts_voice)}        # \"\" = system default",
            f"rate = {self.tts_rate}            # macOS say words-per-minute",
            f"windows_rate = {self.tts_windows_rate}     # Windows SAPI rate, -10..10",
            "",
        ]
        return "\n".join(lines)

    def save(self, path: Optional[Path] = None) -> Path:
        path = path or self.default_path()
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(self.to_toml(), encoding="utf-8")
        return path


# ==============================================================================
# Speech-to-text backends
# ==============================================================================
class STT:
    """Lazy-loaded local STT. Transcribes a float32 mono 16 kHz numpy array."""

    def __init__(self, cfg: Config):
        self.cfg = cfg
        self._backend = None
        self._kind = cfg.stt_backend
        self._loaded_key: Optional[tuple] = None

    @staticmethod
    def cuda_available() -> bool:
        """Best-effort probe so the GUI can enable/disable the faster-whisper
        toggle. ctranslate2 ships with faster-whisper."""
        try:
            import ctranslate2  # type: ignore
            return ctranslate2.get_cuda_device_count() > 0
        except Exception:
            return False

    def _resolve_device(self) -> tuple[str, str]:
        device = self.cfg.stt_device
        compute = self.cfg.stt_compute_type
        if device == "auto":
            device = "cuda" if self.cuda_available() else "cpu"
        if device == "cpu" and compute == "float16":
            compute = "int8"  # float16 is GPU-only
        return device, compute

    def model_present(self) -> bool:
        """Best-effort: is the chosen model already cached locally? Only used to
        word the GUI status ('Downloading…' vs 'Loading…'); never authoritative."""
        try:
            if self._kind == "faster-whisper":
                from huggingface_hub import constants as hf_const  # type: ignore
                cache = Path(getattr(hf_const, "HF_HUB_CACHE", Path.home() / ".cache" / "huggingface" / "hub"))
                needle = self.cfg.stt_model.lower()
                return any(needle in p.name.lower() for p in cache.glob("models--*"))
            else:  # whispercpp
                from pywhispercpp import constants as wc_const  # type: ignore
                models_dir = Path(getattr(wc_const, "MODELS_DIR", Path.home() / ".local" / "share" / "pywhispercpp"))
                return any(self.cfg.stt_model in p.name for p in models_dir.glob("*"))
        except Exception:
            return False

    def ensure(self, status: Optional[Callable[[str], None]] = None):
        """Load (and download on first run) the model. Safe to call repeatedly;
        reloads only when the backend/model/device selection changed."""
        key = (self._kind, self.cfg.stt_model, self.cfg.stt_device, self.cfg.stt_compute_type)
        if self._backend is not None and self._loaded_key == key:
            return
        self._backend = None
        if self._kind == "faster-whisper":
            if status:
                verb = "Loading" if self.model_present() else "Downloading"
                status(f"{verb} speech model '{self.cfg.stt_model}' (faster-whisper)…")
            from faster_whisper import WhisperModel  # type: ignore
            device, compute = self._resolve_device()
            self._backend = WhisperModel(self.cfg.stt_model, device=device, compute_type=compute)
        elif self._kind == "whispercpp":
            from pywhispercpp.model import Model  # type: ignore
            # Download+validate the ggml file ourselves into pywhispercpp's models
            # dir, THEN let pywhispercpp load it. Because the complete file is
            # already present, pywhispercpp skips its own (race-prone) download —
            # this avoids loading a half-written file left by an interrupted
            # download (whisper.cpp aborts on a truncated model:
            # "not all tensors loaded — expected N, got M").
            models_dir = self._ensure_whispercpp_model(status)
            self._backend = Model(self.cfg.stt_model, models_dir=models_dir,
                                  redirect_whispercpp_logs_to=None)
        else:
            raise ValueError(f"unknown stt.backend '{self._kind}' (use whispercpp | faster-whisper)")
        self._loaded_key = key

    # --- whisper.cpp model download (atomic, validated) -----------------------
    @staticmethod
    def _whispercpp_models_dir() -> Path:
        try:
            from pywhispercpp import constants as wc_const  # type: ignore
            return Path(wc_const.MODELS_DIR)
        except Exception:
            try:
                from platformdirs import user_data_dir  # type: ignore
                return Path(user_data_dir("pywhispercpp")) / "models"
            except Exception:
                return Path.home() / "Library" / "Application Support" / "pywhispercpp" / "models"

    def _ensure_whispercpp_model(self, status: Optional[Callable[[str], None]] = None) -> str:
        """Ensure a COMPLETE ggml-<model>.bin exists in pywhispercpp's models dir,
        then return that dir. Downloads atomically (temp file + size validation +
        rename) and repairs a truncated file left by an interrupted/killed earlier
        download — pywhispercpp's own downloader writes non-atomically and can
        leave a partial that whisper.cpp then refuses to load.

        Uses `requests` (pywhispercpp's own HTTP dep, which bundles a CA store —
        plain urllib has no certs in the frozen app and fails TLS to HuggingFace).
        """
        import requests  # provided by pywhispercpp
        try:
            from pywhispercpp.constants import MODELS_BASE_URL, MODELS_PREFIX_URL  # type: ignore
            url = f"{MODELS_BASE_URL}/{MODELS_PREFIX_URL}-{self.cfg.stt_model}.bin"
        except Exception:
            url = f"https://huggingface.co/ggerganov/whisper.cpp/resolve/main/ggml-{self.cfg.stt_model}.bin"

        model = self.cfg.stt_model
        models_dir = self._whispercpp_models_dir()
        models_dir.mkdir(parents=True, exist_ok=True)
        target = models_dir / Path(url).name

        expected = self._remote_size(url)
        if target.exists():
            if expected is None or target.stat().st_size == expected:
                return str(models_dir)          # already complete
            target.unlink(missing_ok=True)       # truncated/stale → re-fetch

        if status:
            status(f"Downloading speech model '{model}'…")
        tmp = target.with_name(target.name + ".part")
        tmp.unlink(missing_ok=True)
        last_pct = -5
        with requests.get(url, stream=True, timeout=60) as resp:
            resp.raise_for_status()
            total = int(resp.headers.get("content-length") or 0) or expected or 0
            read = 0
            with open(tmp, "wb") as out:
                for chunk in resp.iter_content(chunk_size=1 << 16):
                    if not chunk:
                        continue
                    out.write(chunk)
                    read += len(chunk)
                    if status and total:
                        pct = int(read * 100 / total)
                        if pct >= last_pct + 5:
                            last_pct = pct
                            status(f"Downloading speech model '{model}'… {pct}%")
        # Validate completeness BEFORE the file is visible to the loader.
        size = tmp.stat().st_size if tmp.exists() else 0
        if (expected is not None and size != expected) or size == 0:
            tmp.unlink(missing_ok=True)
            raise IOError(f"model download incomplete ({size}/{expected or '?'} bytes) — "
                          "check your connection and retry")
        tmp.replace(target)                      # atomic publish
        if status:
            status(f"Loading speech model '{model}'…")
        return str(models_dir)

    @staticmethod
    def _remote_size(url: str) -> Optional[int]:
        """Remote Content-Length via requests (follows HF redirects). None on failure."""
        try:
            import requests
            r = requests.head(url, allow_redirects=True, timeout=20)
            cl = r.headers.get("content-length")
            return int(cl) if cl else None
        except Exception:
            return None

    def transcribe(self, audio: np.ndarray) -> str:
        self.ensure()
        audio = np.ascontiguousarray(audio, dtype=np.float32)
        if self._kind == "faster-whisper":
            segments, _ = self._backend.transcribe(audio, language=self.cfg.stt_language)
            return " ".join(seg.text.strip() for seg in segments).strip()
        else:  # whispercpp
            segments = self._backend.transcribe(audio)
            return "".join(getattr(s, "text", "") for s in segments).strip()


# ==============================================================================
# Text-to-speech (optional) — native OS voice, no extra deps
# ==============================================================================
class TTS:
    """Speaks via the OS's built-in synthesizer:
       - macOS   -> `say` (selectable Enhanced/Siri voice; rate = words/min)
       - Windows -> SAPI5 via PowerShell System.Speech (rate = -10..10)
       - other   -> pyttsx3 if installed, else espeak (best-effort)
    Text is passed on stdin (macOS/Windows) so it's never interpolated into a
    shell/PowerShell command — no quoting or injection issues with the reply.
    """

    def __init__(self, cfg: Config):
        self.cfg = cfg
        self.system = platform.system()        # 'Darwin' | 'Windows' | 'Linux'
        self._pyttsx_engine = None

    @staticmethod
    def list_voices() -> list[str]:
        """Enumerate installed OS voices for the Settings dropdown (best-effort)."""
        system = platform.system()
        try:
            if system == "Darwin":
                out = subprocess.run(["say", "-v", "?"], capture_output=True, text=True, check=False)
                names = []
                for line in out.stdout.splitlines():
                    # "Samantha            en_US    # Hello, ..."
                    name = line.split("  ")[0].strip()
                    if name:
                        names.append(name)
                return names
            if system == "Windows":
                ps = (
                    "Add-Type -AssemblyName System.Speech;"
                    "(New-Object System.Speech.Synthesis.SpeechSynthesizer)."
                    "GetInstalledVoices() | ForEach-Object { $_.VoiceInfo.Name }"
                )
                out = subprocess.run(["powershell", "-NoProfile", "-Command", ps],
                                     capture_output=True, text=True, check=False)
                return [l.strip() for l in out.stdout.splitlines() if l.strip()]
        except Exception:
            pass
        return []

    def speak(self, text: str):
        if not self.cfg.tts_enabled or not text:
            return
        try:
            if self.system == "Darwin":
                self._speak_macos(text)
            elif self.system == "Windows":
                self._speak_windows(text)
            else:
                self._speak_fallback(text)
        except Exception as exc:  # TTS must never crash the loop
            print(f"[tts] skipped ({exc})")

    def _speak_macos(self, text: str):
        cmd = ["say"]
        if self.cfg.tts_voice:
            cmd += ["-v", self.cfg.tts_voice]
        if self.cfg.tts_rate:
            cmd += ["-r", str(self.cfg.tts_rate)]   # words per minute
        cmd += ["-f", "-"]                            # read text from stdin
        subprocess.run(cmd, input=text, text=True, check=False)

    def _speak_windows(self, text: str):
        ps = [
            "Add-Type -AssemblyName System.Speech;",
            "$s = New-Object System.Speech.Synthesis.SpeechSynthesizer;",
        ]
        if self.cfg.tts_voice:
            safe = self.cfg.tts_voice.replace("'", "''")
            ps.append(f"try {{ $s.SelectVoice('{safe}') }} catch {{}};")
        ps.append(f"$s.Rate = {int(self.cfg.tts_windows_rate)};")
        ps.append("$s.Speak([Console]::In.ReadToEnd());")
        subprocess.run(
            ["powershell", "-NoProfile", "-Command", " ".join(ps)],
            input=text, text=True, check=False,
        )

    def _speak_fallback(self, text: str):
        try:
            import pyttsx3  # type: ignore
            if self._pyttsx_engine is None:
                self._pyttsx_engine = pyttsx3.init()
                if self.cfg.tts_rate:
                    self._pyttsx_engine.setProperty("rate", self.cfg.tts_rate)
            self._pyttsx_engine.say(text)
            self._pyttsx_engine.runAndWait()
        except Exception:
            if shutil.which("espeak"):
                subprocess.run(["espeak", text], check=False)
            else:
                raise


# ==============================================================================
# MCP client — one stateless JSON-RPC POST to talk_to_leader
# ==============================================================================
class McpClient:
    def __init__(self, cfg: Config):
        self.cfg = cfg
        self._id = 0

    def talk_to_leader(self, message: str) -> dict:
        self._id += 1
        arguments: dict = {"message": message, "emit_in_game": self.cfg.emit_in_game}
        if self.cfg.player_guid:
            arguments["playerGuid"] = self.cfg.player_guid
        elif self.cfg.player_name:
            arguments["playerName"] = self.cfg.player_name
        if self.cfg.bot_guid:
            arguments["botGuid"] = self.cfg.bot_guid

        payload = {
            "jsonrpc": "2.0",
            "id": self._id,
            "method": "tools/call",
            "params": {"name": "talk_to_leader", "arguments": arguments},
        }
        headers = {
            "Content-Type": "application/json",
            "Authorization": f"Bearer {self.cfg.bearer_token}",
        }
        # Use requests (bundles a CA store via certifi) — urllib in the frozen app
        # has no certs and fails TLS verification against the HTTPS MCP endpoint.
        try:
            import requests
            resp = requests.post(self.cfg.mcp_url, json=payload, headers=headers,
                                 timeout=self.cfg.timeout_s)
            if resp.status_code >= 400:
                return {"error": f"HTTP {resp.status_code}: {resp.text[:200]}"}
            raw = resp.text
        except Exception as exc:
            return {"error": f"connection failed: {exc}"}

        try:
            envelope = json.loads(raw)
        except json.JSONDecodeError:
            return {"error": f"non-JSON response: {raw[:200]}"}

        if "error" in envelope:
            return {"error": f"jsonrpc error: {envelope['error']}"}

        # MCP wraps the tool result in result.content[0].text (a JSON string).
        result = envelope.get("result", {})
        content = result.get("content", [])
        if content and isinstance(content, list):
            text = content[0].get("text", "")
            try:
                return json.loads(text)
            except json.JSONDecodeError:
                return {"reply": text}
        return result if result else {"error": f"unexpected response shape: {raw[:200]}"}


# ==============================================================================
# Push-to-talk recorder
# ==============================================================================
class Recorder:
    """Captures mic frames while .active is True (driven by the hotkey/button)."""

    def __init__(self, device: Optional[int] = None):
        self._frames: list[np.ndarray] = []
        self._lock = threading.Lock()
        self.active = False
        kwargs = {}
        if device is not None and device >= 0:
            kwargs["device"] = device
        self._stream = sd.InputStream(
            samplerate=SAMPLE_RATE, channels=CHANNELS, dtype="float32",
            callback=self._on_audio, **kwargs,
        )
        self._stream.start()

    def _on_audio(self, indata, frames, time_info, status):  # noqa: ARG002
        if self.active:
            with self._lock:
                self._frames.append(indata.copy())

    def start(self):
        with self._lock:
            self._frames.clear()
        self.active = True

    def stop(self) -> np.ndarray:
        self.active = False
        with self._lock:
            if not self._frames:
                return np.zeros(0, dtype=np.float32)
            audio = np.concatenate(self._frames, axis=0).flatten()
            self._frames.clear()
        return audio

    def close(self):
        try:
            self._stream.stop()
            self._stream.close()
        except Exception:
            pass


def list_input_devices() -> list[tuple[int, str]]:
    """(index, name) for input-capable audio devices — for the Settings mic picker."""
    devices = []
    try:
        for idx, dev in enumerate(sd.query_devices()):
            if dev.get("max_input_channels", 0) > 0:
                devices.append((idx, dev.get("name", f"device {idx}")))
    except Exception:
        pass
    return devices


def resolve_key(name: str):
    name = name.strip().lower()
    special = getattr(keyboard.Key, name, None)
    if special is not None:
        return special
    if len(name) == 1:
        return keyboard.KeyCode.from_char(name)
    raise ValueError(f"unknown hotkey '{name}' (use f1-f12 or a single character)")


# ==============================================================================
# Global push-to-talk hotkey
#
# macOS: a Quartz CGEventTap that reads ONLY the raw keycode. We deliberately
# avoid pynput's keyboard.Listener on macOS — it calls a main-thread-only
# HIToolbox input-source API (TISCopyCurrentKeyboardInputSource) from its
# background listener thread, which SIGTRAPs on modern macOS. The event tap
# touches none of that, so it can't hit that crash.
# Windows/Linux: pynput's listener (works fine there).
# ==============================================================================

# US-layout virtual keycodes — enough to cover F-keys plus common keys without
# ever consulting the (crash-prone) input-source layout API.
_MAC_KEYCODES = {
    "f1": 0x7A, "f2": 0x78, "f3": 0x63, "f4": 0x76, "f5": 0x60, "f6": 0x61,
    "f7": 0x62, "f8": 0x64, "f9": 0x65, "f10": 0x6D, "f11": 0x67, "f12": 0x6F,
    "space": 0x31, "return": 0x24, "enter": 0x24, "tab": 0x30, "escape": 0x35, "esc": 0x35,
    "a": 0x00, "s": 0x01, "d": 0x02, "f": 0x03, "h": 0x04, "g": 0x05, "z": 0x06, "x": 0x07,
    "c": 0x08, "v": 0x09, "b": 0x0B, "q": 0x0C, "w": 0x0D, "e": 0x0E, "r": 0x0F, "y": 0x10,
    "t": 0x11, "1": 0x12, "2": 0x13, "3": 0x14, "4": 0x15, "6": 0x16, "5": 0x17, "9": 0x19,
    "7": 0x1A, "8": 0x1C, "0": 0x1D, "o": 0x1F, "u": 0x20, "i": 0x22, "p": 0x23, "l": 0x25,
    "j": 0x26, "k": 0x28, "n": 0x2D, "m": 0x2E, "`": 0x32, "grave": 0x32,
}


def mac_keycode(name: str) -> int:
    kc = _MAC_KEYCODES.get(name.strip().lower())
    if kc is None:
        raise ValueError(f"hotkey '{name}' not supported on macOS (use f1-f12 or a US-layout key)")
    return kc


class _QuartzHotkey:
    """macOS global push-to-talk via a CGEventTap (keycode-only, no TSM)."""

    def __init__(self, hotkey_name: str, on_press: Callable[[], None], on_release: Callable[[], None],
                 on_error: Optional[Callable[[str], None]] = None):
        self.keycode = mac_keycode(hotkey_name)
        self.on_press = on_press
        self.on_release = on_release
        self.on_error = on_error
        self._runloop = None
        self._tap = None
        self._thread = threading.Thread(target=self._run, daemon=True)

    def start(self):
        self._thread.start()

    def _run(self):
        import Quartz  # pyobjc (bundled)
        mask = (1 << Quartz.kCGEventKeyDown) | (1 << Quartz.kCGEventKeyUp)
        self._tap = Quartz.CGEventTapCreate(
            Quartz.kCGSessionEventTap,
            Quartz.kCGHeadInsertEventTap,
            Quartz.kCGEventTapOptionListenOnly,
            mask,
            self._callback,
            None,
        )
        if not self._tap:
            # No Accessibility permission — the on-screen button still works.
            if self.on_error:
                self.on_error("global hotkey needs Accessibility permission "
                              "(System Settings → Privacy & Security → Accessibility)")
            return
        src = Quartz.CFMachPortCreateRunLoopSource(None, self._tap, 0)
        self._runloop = Quartz.CFRunLoopGetCurrent()
        Quartz.CFRunLoopAddSource(self._runloop, src, Quartz.kCFRunLoopCommonModes)
        Quartz.CGEventTapEnable(self._tap, True)
        Quartz.CFRunLoopRun()

    def _callback(self, proxy, type_, event, refcon):
        import Quartz
        # Re-enable if the system disabled the tap (slow callback / user input).
        if type_ in (Quartz.kCGEventTapDisabledByTimeout, Quartz.kCGEventTapDisabledByUserInput):
            if self._tap:
                Quartz.CGEventTapEnable(self._tap, True)
            return event
        kc = Quartz.CGEventGetIntegerValueField(event, Quartz.kCGKeyboardEventKeycode)
        if kc == self.keycode:
            if type_ == Quartz.kCGEventKeyDown:
                # Ignore key-repeat while held.
                if not Quartz.CGEventGetIntegerValueField(event, Quartz.kCGKeyboardEventAutorepeat):
                    self.on_press()
            elif type_ == Quartz.kCGEventKeyUp:
                self.on_release()
        return event

    def stop(self):
        if self._runloop is not None:
            try:
                import Quartz
                Quartz.CFRunLoopStop(self._runloop)
            except Exception:
                pass


class _PynputHotkey:
    """Windows/Linux global push-to-talk via pynput's keyboard listener."""

    def __init__(self, hotkey_name: str, on_press: Callable[[], None], on_release: Callable[[], None],
                 on_error: Optional[Callable[[str], None]] = None):
        self._key = resolve_key(hotkey_name)
        self._on_press = on_press
        self._on_release = on_release
        self._down = False
        self._listener = keyboard.Listener(on_press=self._p, on_release=self._r)

    def _p(self, key):
        if key == self._key and not self._down:
            self._down = True
            self._on_press()

    def _r(self, key):
        if key == self._key and self._down:
            self._down = False
            self._on_release()

    def start(self):
        self._listener.start()

    def stop(self):
        self._listener.stop()


def make_hotkey_listener(hotkey_name: str, on_press: Callable[[], None],
                         on_release: Callable[[], None],
                         on_error: Optional[Callable[[str], None]] = None):
    """Platform-appropriate global push-to-talk listener. on_press/on_release
    take no args and fire only for the configured hotkey. on_error (optional) is
    called with a message if the listener can't start (e.g. no Accessibility).
    Has .start()/.stop(). Validates the hotkey name (raises ValueError on an
    unsupported key)."""
    if platform.system() == "Darwin":
        return _QuartzHotkey(hotkey_name, on_press, on_release, on_error)
    return _PynputHotkey(hotkey_name, on_press, on_release, on_error)


def validate_hotkey(name: str) -> None:
    """Raise ValueError if `name` isn't a usable hotkey on this platform."""
    if platform.system() == "Darwin":
        mac_keycode(name)
    else:
        resolve_key(name)
