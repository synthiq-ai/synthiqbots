# App icons (optional)

Drop platform icons here to brand the packaged app. Builds work without them
(PyInstaller falls back to a default icon).

- `icon.icns` — macOS app icon (1024×1024 source → `iconutil`/Image2icon).
- `icon.ico` — Windows exe icon (multi-size 16–256 px `.ico`).

The PyInstaller spec (`packaging/voice-command.spec`) uses each file only if it
exists, so missing icons never break the build.
