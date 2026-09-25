# Build the portable Windows .exe (no installer).
#
#   cd tools\voice-command
#   python -m venv .venv ; .\.venv\Scripts\Activate.ps1
#   pip install -r requirements-build.txt
#   powershell -ExecutionPolicy Bypass -File packaging\build-windows.ps1
#
# Output: dist\WoWVoiceCommand.exe  (double-click to run; no admin, no install)
#
# The .exe is UNSIGNED (personal use). First launch: SmartScreen may warn —
# click "More info" -> "Run anyway". Grant microphone access if Windows prompts.
$ErrorActionPreference = "Stop"

# Move to the tool dir (parent of this script's folder).
Set-Location (Join-Path $PSScriptRoot "..")

Write-Host "==> Cleaning previous build"
Remove-Item -Recurse -Force build, dist\WoWVoiceCommand.exe -ErrorAction SilentlyContinue

Write-Host "==> PyInstaller"
pyinstaller packaging\voice-command.spec --noconfirm

$exe = "dist\WoWVoiceCommand.exe"
if (-not (Test-Path $exe)) { throw "ERROR: $exe not produced" }

Write-Host "==> Done: $exe"
Get-Item $exe | Select-Object Name, Length, LastWriteTime
