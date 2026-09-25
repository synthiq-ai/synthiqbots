#!/usr/bin/env bash
# Build the macOS .app and package it into a drag-to-Applications .dmg.
#
#   cd tools/voice-command
#   python -m venv .venv && source .venv/bin/activate
#   pip install -r requirements-build.txt
#   bash packaging/build-macos.sh
#
# Output: dist/WoW-Voice-Command.dmg  (+ dist/WoW Voice Command.app)
#
# The app is UNSIGNED (personal use). First launch: right-click the app in
# /Applications -> Open -> Open to get past Gatekeeper, then grant Microphone
# and Accessibility (global hotkey) permissions when prompted.
set -euo pipefail

cd "$(dirname "$0")/.."   # tool dir: tools/voice-command

APP_NAME="WoW Voice Command"
DMG_NAME="WoW-Voice-Command"
APP_PATH="dist/${APP_NAME}.app"
DMG_PATH="dist/${DMG_NAME}.dmg"

echo "==> Cleaning previous build"
rm -rf build "dist/${APP_NAME}.app" "${DMG_PATH}"

echo "==> PyInstaller"
pyinstaller packaging/voice-command.spec --noconfirm

[ -d "${APP_PATH}" ] || { echo "ERROR: ${APP_PATH} not produced"; exit 1; }

echo "==> Building DMG"
if command -v create-dmg >/dev/null 2>&1; then
  # Pretty DMG with an Applications symlink for drag-install.
  create-dmg \
    --volname "${APP_NAME}" \
    --window-pos 200 120 \
    --window-size 640 400 \
    --icon-size 110 \
    --icon "${APP_NAME}.app" 160 200 \
    --app-drop-link 480 200 \
    --no-internet-enable \
    "${DMG_PATH}" \
    "${APP_PATH}" || {
      echo "create-dmg reported a non-zero exit; verifying the DMG exists anyway…"
      [ -f "${DMG_PATH}" ] || { echo "ERROR: DMG not produced"; exit 1; }
    }
else
  echo "create-dmg not found; falling back to dmgbuild (hdiutil)."
  python - "$APP_PATH" "$DMG_PATH" "$APP_NAME" <<'PY'
import subprocess, sys, tempfile, os, plistlib
app_path, dmg_path, vol = sys.argv[1:4]
# Minimal dmgbuild settings: the .app plus an /Applications symlink.
settings = f'''
app = {app_path!r}
files = [app]
symlinks = {{"Applications": "/Applications"}}
icon_locations = {{ {os.path.basename(app_path)!r}: (160, 200), "Applications": (480, 200) }}
window_rect = ((200, 120), (640, 400))
'''
with tempfile.NamedTemporaryFile("w", suffix=".py", delete=False) as f:
    f.write(settings)
    settings_file = f.name
subprocess.check_call(["dmgbuild", "-s", settings_file, vol, dmg_path])
os.unlink(settings_file)
PY
fi

echo "==> Done: ${DMG_PATH}"
ls -lh "${DMG_PATH}"
