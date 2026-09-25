-- ============================================================================
-- SynthiqBotsUI Observer
-- ============================================================================
-- Turns this WoW client into an on-demand "observer camera" for the bots.
--
-- A server-side bot whispers this character a structured command:
--     [SBSHOT] <reqId> <targetBot> <view>            -- still screenshot
--     [SBCLIP] <reqId> <targetBot> <view> <seconds>  -- video clip
-- On receipt we teleport to the target bot (.appear), optionally pick a saved
-- camera view, and fire the client's Screenshot(). The screenshot file lands in
-- Screenshots/ immediately; the feedback-daemon spots it and ships it back to
-- ops-api tagged with <reqId>, which resolves the request_observer_screenshot
-- MCP call. See docs/capture.md.
--
-- A clip works the same way up to the Screenshot(), which there serves only as a
-- "the camera is in position" MARKER: the addon can't record video, so the
-- daemon watches for that file and rolls ffmpeg for <seconds>. Nothing about the
-- recording happens in Lua.
--
-- WoW 3.3.5a addons can't take a screenshot from the login/glue screens and
-- can't do network I/O, but Screenshot() IS callable from event handlers (no
-- hardware event needed), which is what makes this autonomous. The character
-- running this addon must be a GM (so .appear works).
--
-- PROTECTED FUNCTIONS: every call below (.appear, .gm visible off, SetView,
-- CameraZoomOut, Screenshot) is unprotected and therefore legal from this
-- whisper-driven, non-hardware path. Do NOT add /target (protected TargetUnit()),
-- FollowUnit(), or CameraOrSelectOrMoveStart() — WoW raises ADDON_ACTION_BLOCKED
-- and the whole addon stops working. No UI option overrides that.
-- ============================================================================

SynthiqBotsUI = SynthiqBotsUI or {}
SynthiqBotsUI.observer = {}
local OB = SynthiqBotsUI.observer

-- --- Tunables ---------------------------------------------------------------
-- Seconds to wait after .appear before framing/shooting (zone load + settle).
OB.APPEAR_DELAY = 2.0
-- Seconds to wait after SetView before Screenshot (camera glide).
OB.VIEW_DELAY = 0.5
-- Become invisible while observing so the GM doesn't disturb the scene.
OB.GM_INVISIBLE = true
-- Camera zoom-out steps applied before a clip (0 disables). A wider shot frames
-- the action better than a screenshot's default over-the-shoulder distance.
OB.CLIP_ZOOM_OUT = 2
-- Optional: only honour commands whispered by these character names. Leave
-- empty to accept the command from any whisper (it's structured + your realm).
OB.ALLOWED_SENDERS = {}  -- e.g. { ["Claude"] = true }

-- --- Minimal delayed-task runner (no C_Timer in 3.3.5a) ---------------------
local tasks = {}
local driver = CreateFrame("Frame")
driver:SetScript("OnUpdate", function()
    if #tasks == 0 then return end
    local now = GetTime()
    -- Run due tasks; iterate backwards so removal is safe.
    for i = #tasks, 1, -1 do
        if now >= tasks[i].at then
            local fn = tasks[i].fn
            table.remove(tasks, i)
            fn()
        end
    end
end)
local function After(delay, fn)
    table.insert(tasks, { at = GetTime() + delay, fn = fn })
end

-- Execute a server/GM command exactly as if the user typed it in the chat box
-- (the reliable way to run `.appear` etc. from an addon).
local function RunCommand(cmd)
    local eb = DEFAULT_CHAT_FRAME and DEFAULT_CHAT_FRAME.editBox
    if not eb then return end
    eb:SetText(cmd)
    ChatEdit_SendText(eb, 0)
    eb:SetText("")
end

local function Say(msg)
    DEFAULT_CHAT_FRAME:AddMessage("|cff66bbff[SBObserver]|r " .. msg)
end

-- --- Positioning ------------------------------------------------------------
-- Teleport to the bot and settle the camera, then hand off to `done`. Shared by
-- both the screenshot and the clip paths.
function OB.Position(target, view, done)
    if OB.GM_INVISIBLE then
        RunCommand(".gm visible off")
    end
    -- Teleport to the bot. NOTE: no /target — it maps to the protected
    -- TargetUnit() and triggers ADDON_ACTION_BLOCKED from this whisper-driven
    -- (non-hardware) path. .appear already positions us at the bot.
    RunCommand(".appear " .. target)

    After(OB.APPEAR_DELAY, function()
        -- Optionally pick a saved camera view (unprotected).
        if view and view >= 1 and view <= 5 then
            SetView(view)
        end
        After(OB.VIEW_DELAY, done)
    end)
end

-- --- The capture sequences ---------------------------------------------------
function OB.Capture(reqId, target, view)
    Say("capture " .. reqId .. " -> " .. target)
    OB.Position(target, view, function()
        Screenshot()
        Say("screenshot fired for " .. reqId)
    end)
end

-- A clip: same positioning, but the Screenshot() here is a READY-MARKER. The
-- daemon sees that file appear and starts recording for `seconds`. We can't
-- record from Lua, and we deliberately don't try to time anything here — the
-- marker's mtime is the only synchronisation signal either side needs.
function OB.Clip(reqId, target, view, seconds)
    Say("clip " .. reqId .. " -> " .. target .. " (" .. seconds .. "s)")
    OB.Position(target, view, function()
        if OB.CLIP_ZOOM_OUT and OB.CLIP_ZOOM_OUT > 0 then
            CameraZoomOut(OB.CLIP_ZOOM_OUT)
        end
        Screenshot()
        Say("recording " .. seconds .. "s for " .. reqId)
    end)
end

-- --- Whisper trigger --------------------------------------------------------
-- "[SBSHOT] <reqId> <target> <view?>" — view optional (defaults 0).
local function ParseShot(msg)
    local reqId, target, view = msg:match("^%[SBSHOT%]%s+(%S+)%s+(%S+)%s*(%d*)")
    if not reqId or not target then return nil end
    return reqId, target, tonumber(view) or 0
end

-- "[SBCLIP] <reqId> <target> <view> <seconds>" — both numbers required.
local function ParseClip(msg)
    local reqId, target, view, seconds = msg:match("^%[SBCLIP%]%s+(%S+)%s+(%S+)%s+(%d+)%s+(%d+)")
    if not reqId or not target then return nil end
    return reqId, target, tonumber(view) or 0, tonumber(seconds) or 0
end

-- Returns true when the whisper's author is allowed to drive this observer.
local function SenderAllowed(author)
    if next(OB.ALLOWED_SENDERS) == nil then return true end
    local who = author and author:match("^[^%-]+") or author
    return OB.ALLOWED_SENDERS[author] or OB.ALLOWED_SENDERS[who]
end

local listener = CreateFrame("Frame")
listener:RegisterEvent("CHAT_MSG_WHISPER")
listener:SetScript("OnEvent", function(self, event, msg, author)
    if event ~= "CHAT_MSG_WHISPER" or not msg then return end

    local reqId, target, view = ParseShot(msg)
    if reqId then
        if not SenderAllowed(author) then
            Say("ignored [SBSHOT] from untrusted sender " .. tostring(author))
            return
        end
        OB.Capture(reqId, target, view)
        return
    end

    local seconds
    reqId, target, view, seconds = ParseClip(msg)
    if reqId then
        if not SenderAllowed(author) then
            Say("ignored [SBCLIP] from untrusted sender " .. tostring(author))
            return
        end
        OB.Clip(reqId, target, view, seconds)
    end
end)

-- Confirm load so the user knows the observer is armed.
local boot = CreateFrame("Frame")
boot:RegisterEvent("PLAYER_LOGIN")
boot:SetScript("OnEvent", function()
    DEFAULT_CHAT_FRAME:AddMessage("|cff66bbff[SBObserver]|r armed — awaiting [SBSHOT] / [SBCLIP] whispers")
end)
