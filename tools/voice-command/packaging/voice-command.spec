# -*- mode: python ; coding: utf-8 -*-
"""
PyInstaller spec for the WoW Voice Command desktop app.

Invoke from the tool dir (so relative paths resolve):
    cd tools/voice-command
    pyinstaller packaging/voice-command.spec --noconfirm

Produces:
    macOS    -> dist/WoW Voice Command.app   (build-macos.sh wraps it into a .dmg)
    Windows  -> dist/WoWVoiceCommand.exe      (onefile, portable; no installer)

Both STT backends are bundled. faster-whisper needs CUDA/cuDNN present on the
*user's* machine — those runtime libs are NOT shipped (kept off the default
whisper.cpp path); the Settings toggle only surfaces faster-whisper when CUDA is
detected.
"""

import os
import sys

from PyInstaller.utils.hooks import (
    collect_data_files,
    collect_dynamic_libs,
    collect_submodules,
)

IS_MAC = sys.platform == "darwin"

# Paths are anchored to the spec's own location so the build works regardless of
# the invoking cwd. SPECPATH is the directory containing this spec (packaging/);
# its parent is the tool dir (tools/voice-command/) where app.py + assets live.
TOOL_DIR = os.path.dirname(SPECPATH)
APP_SCRIPT = os.path.join(TOOL_DIR, "app.py")

datas = []
binaries = []
hiddenimports = []

# CustomTkinter ships theme/asset JSON it loads at runtime.
datas += collect_data_files("customtkinter")

# sounddevice is a single module; PortAudio ships in the _sounddevice_data
# package. pyinstaller-hooks-contrib also has a hook-sounddevice that covers this,
# but collect it explicitly too (harmless if the package layout varies).
try:
    binaries += collect_dynamic_libs("_sounddevice_data")
    datas += collect_data_files("_sounddevice_data")
except Exception:
    pass

# whisper.cpp bindings: native lib (+ any bundled data).
binaries += collect_dynamic_libs("pywhispercpp")
datas += collect_data_files("pywhispercpp")
hiddenimports += collect_submodules("pywhispercpp")

# faster-whisper / ctranslate2 native libs (CPU; CUDA libs are a machine prereq).
binaries += collect_dynamic_libs("ctranslate2")
hiddenimports += collect_submodules("faster_whisper")

# macOS global hotkey uses a Quartz CGEventTap (imported lazily in vc_core), so
# PyInstaller's static analysis needs it named explicitly.
if IS_MAC:
    hiddenimports += ["Quartz", "Cocoa", "CoreFoundation", "objc"]

# Icons are optional — use only if present so the build never fails on a missing file.
_icns = os.path.join(TOOL_DIR, "assets", "icon.icns")
_ico = os.path.join(TOOL_DIR, "assets", "icon.ico")
icon = _icns if (IS_MAC and os.path.exists(_icns)) else (_ico if os.path.exists(_ico) else None)

block_cipher = None

a = Analysis(
    [APP_SCRIPT],
    pathex=[TOOL_DIR],
    binaries=binaries,
    datas=datas,
    hiddenimports=hiddenimports,
    hookspath=[],
    runtime_hooks=[],
    excludes=["tkinter.test", "test", "unittest"],
    cipher=block_cipher,
    noarchive=False,
)
pyz = PYZ(a.pure, a.zipped_data, cipher=block_cipher)

if IS_MAC:
    # onedir + .app bundle.
    exe = EXE(
        pyz, a.scripts, [],
        exclude_binaries=True,
        name="WoW Voice Command",
        console=False,
        icon=icon,
    )
    coll = COLLECT(
        exe, a.binaries, a.zipfiles, a.datas,
        name="WoW Voice Command",
    )
    app = BUNDLE(
        coll,
        name="WoW Voice Command.app",
        icon=icon,
        bundle_identifier="ru.synthiq.wowvoicecommand",
        info_plist={
            "CFBundleName": "WoW Voice Command",
            "CFBundleDisplayName": "WoW Voice Command",
            "NSMicrophoneUsageDescription":
                "Microphone access is used to capture your push-to-talk voice commands.",
            "NSHighResolutionCapable": True,
            "LSMinimumSystemVersion": "11.0",
        },
    )
else:
    # Windows: single portable .exe (no installer). All binaries/datas folded in.
    exe = EXE(
        pyz, a.scripts, a.binaries, a.zipfiles, a.datas, [],
        name="WoWVoiceCommand",
        console=False,
        icon=icon,
        upx=False,
    )
