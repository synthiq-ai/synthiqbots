; ============================================================================
; observer-login.ahk  —  auto-start WoW 3.3.5a and log in the GM observer
; ============================================================================
; AutoHotkey v1.1 (https://www.autohotkey.com/). The WoW 3.3.5a login/realm/
; character screens run GlueXML and CANNOT host an addon, so login has to be
; driven from outside the client — this script does that.
;
; Chain: power on (or Wake-on-LAN) -> Windows auto-logon -> Task Scheduler runs
; this -> WoW launches + the observer logs in -> the SynthiqBotsUIObserver addon
; + feedback-daemon take over.
;
; REQUIREMENTS / SETUP (read apps/observer-launcher/README.md):
;   * Run WoW WINDOWED or borderless — NEVER exclusive-fullscreen (it breaks
;     ImageSearch clicks AND ffmpeg/OBS capture).
;   * Pre-save the account name in WTF/Config.wtf (SET accountName "...") so only
;     the password is typed here.
;   * Put the observer password in a local file readable only by your user
;     (see PASSWORD_FILE below) — do NOT hardcode it in this script.
;   * Capture your own button images (realm-enter, enter-world) at your fixed
;     resolution and drop them next to this script (see ImageSearch calls).
; ============================================================================

#NoEnv
#SingleInstance Force
SetWorkingDir %A_ScriptDir%
SetTitleMatchMode, 2   ; match window titles by substring

; --- Config -----------------------------------------------------------------
WOW_EXE       := "C:\Games\World of Warcraft 3.3.5a\Wow.exe"
WOW_TITLE     := "World of Warcraft"
PASSWORD_FILE := A_ScriptDir . "\observer-password.txt"  ; one line, your user only
LOGIN_WAIT_MS := 20000   ; max wait for the login screen to appear
STEP_PAUSE_MS := 1500    ; settle time between UI steps

; --- Read the password (never hardcode) -------------------------------------
FileRead, PASSWORD, %PASSWORD_FILE%
PASSWORD := Trim(PASSWORD, " `t`r`n")
if (PASSWORD = "") {
    MsgBox, 48, observer-login, Password file empty or missing:`n%PASSWORD_FILE%
    ExitApp
}

; --- Launch the client ------------------------------------------------------
if !FileExist(WOW_EXE) {
    MsgBox, 48, observer-login, Wow.exe not found:`n%WOW_EXE%
    ExitApp
}
Run, %WOW_EXE%
WinWait, %WOW_TITLE%, , 60
if ErrorLevel {
    MsgBox, 48, observer-login, WoW window never appeared.
    ExitApp
}
WinActivate, %WOW_TITLE%

; --- Wait for the login screen, then type the password ----------------------
; The account name is pre-saved in Config.wtf, so the password field is focused
; on arrival. We give the client time to reach the login screen, then type.
Sleep, %LOGIN_WAIT_MS%
WinActivate, %WOW_TITLE%
; If the account field is focused instead of password, Tab once first. Adjust
; for your client build by uncommenting:
; Send, {Tab}
SendRaw, %PASSWORD%
Sleep, 300
Send, {Enter}

; --- Realm + character select -----------------------------------------------
; These steps are UI-image dependent. Capture screenshots of the buttons at YOUR
; fixed resolution and save them beside this script, then the ImageSearch calls
; below click them. Falling back to {Enter} works when the last realm/character
; is remembered (common for a dedicated observer).
Sleep, %STEP_PAUSE_MS%
ClickImageOrEnter("realm-enter.png")     ; pick realm
Sleep, %STEP_PAUSE_MS%
ClickImageOrEnter("enter-world.png")     ; enter world with the last character

; Done — the addon prints "[SBObserver] armed" once in-world.
ExitApp

; ----------------------------------------------------------------------------
ClickImageOrEnter(imageName) {
    global WOW_TITLE
    path := A_ScriptDir . "\" . imageName
    if FileExist(path) {
        ; Search the whole screen for the button image; click its centre.
        ImageSearch, fx, fy, 0, 0, A_ScreenWidth, A_ScreenHeight, *50 %path%
        if (ErrorLevel = 0) {
            MouseMove, fx + 10, fy + 8
            Click
            return
        }
    }
    ; Fallback: the client usually accepts Enter to advance when the last
    ; realm/character is remembered.
    WinActivate, %WOW_TITLE%
    Send, {Enter}
}
