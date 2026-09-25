-- ============================================================================
-- SynthiqBotsUI Feedback Capture
-- ============================================================================
-- Captures user feedback (screenshot + chat ring buffer + game context) into
-- SavedVariables so a server-side companion daemon can upload it to the
-- gateway agent for module-improvement decisions.
--
-- WoW 3.3.5a Lua addons cannot make HTTP calls and cannot read their own
-- screenshot files; egress is handled by an external daemon watching
-- SavedVariables + the Screenshots/ folder. Design: docs/feedback-loop.md.
-- ============================================================================

SynthiqBotsUI.feedback = {}
local FB = SynthiqBotsUI.feedback

-- The daemon-written results sidecar table. WoW loads it from
-- SynthiqBotsUIResults.lua before any addon code runs. Kept as a global
-- per the .toc declaration so we don't have to do anything special on
-- load. Default to empty table for fresh installs.
if(SynthiqBotsUIResults == nil) then SynthiqBotsUIResults = {} end

FB.HISTORY_MAX = 200   -- ring buffer size (lines)
FB.QUEUE_MAX   = 50    -- cap on unsent feedback entries
FB.history     = {}    -- list of {ts, channel, sender, text}

-- ----------------------------------------------------------------------------
-- Chat ring buffer
-- ----------------------------------------------------------------------------

FB.recordLine = function(channel, sender, text)
    if(text == nil or text == "") then return end
    table.insert(FB.history, {
        ts      = time(),
        channel = channel or "?",
        sender  = sender or "?",
        text    = text,
    })
    while(table.getn(FB.history) > FB.HISTORY_MAX) do
        table.remove(FB.history, 1)
    end
end

-- ----------------------------------------------------------------------------
-- Context snapshot (zone, target, group, char identity)
-- ----------------------------------------------------------------------------

FB.snapshotCtx = function()
    local zone = GetRealZoneText() or ""
    local subzone = GetSubZoneText() or ""

    local target = ""
    if(UnitExists("target")) then
        local tname = UnitName("target") or ""
        local tclass = UnitClass("target") or ""
        local tlvl = UnitLevel("target") or 0
        local thp = UnitHealth("target") or 0
        local thpmax = UnitHealthMax("target") or 1
        if(thpmax < 1) then thpmax = 1 end
        local thp_pct = math.floor(thp / thpmax * 100)
        target = tname .. "|" .. tclass .. "|" .. tlvl .. "|" .. thp_pct
    end

    local group = {}
    local n = GetNumPartyMembers() or 0
    for i = 1, n do
        local pname = UnitName("party" .. i)
        if(pname ~= nil) then table.insert(group, pname) end
    end

    return {
        ts         = time(),
        char       = (UnitName("player") or "") .. "-" .. (GetRealmName() or ""),
        char_class = UnitClass("player") or "",
        char_lvl   = UnitLevel("player") or 0,
        zone       = zone,
        subzone    = subzone,
        target     = target,
        group      = group,
        addon_ver  = "2.0.0",
    }
end

-- ----------------------------------------------------------------------------
-- Capture
-- ----------------------------------------------------------------------------

FB.capture = function(note)
    -- Screenshot is async; the file appears a few hundred ms later in
    -- <wow-install>/Screenshots/. The companion daemon matches by mtime.
    Screenshot()

    local entry = {
        id              = time() .. "-" .. math.random(1000, 9999),
        ts              = time(),
        note            = note or "",
        screenshot_hint = date("%Y-%m-%d %H:%M:%S"),
        chat_history    = {},
        ctx             = FB.snapshotCtx(),
        status          = "pending",
    }

    for _, line in ipairs(FB.history) do
        table.insert(entry.chat_history, line)
    end

    if(SynthiqBotsUISave.feedback_queue == nil) then
        SynthiqBotsUISave.feedback_queue = {}
    end
    table.insert(SynthiqBotsUISave.feedback_queue, entry)

    while(table.getn(SynthiqBotsUISave.feedback_queue) > FB.QUEUE_MAX) do
        table.remove(SynthiqBotsUISave.feedback_queue, 1)
    end

    print("|cff00ff00[SynthiqBotsUI]|r Feedback captured (id=" .. entry.id ..
          "). |cffffff00/reload or logout|r to flush to disk.")
    return entry.id
end

-- ----------------------------------------------------------------------------
-- /sb feedback log
-- ----------------------------------------------------------------------------

-- The companion daemon writes resolved/error rows from the server side
-- into a sibling SavedVariables file `SynthiqBotsUIResults.lua`. We only
-- READ that table here; the daemon owns the writes. Both addon save and
-- daemon save coexist in the same SavedVariables/ directory.
--
-- Keys are addon-side ids (e.g. "1745673600-4823"), values are tables
-- with: status, summary, pr_url, resolved_at, actions (JSON string).
FB.printLog = function()
    local q = SynthiqBotsUISave.feedback_queue or {}
    -- Prefer the daemon-written sidecar; fall back to legacy in-save
    -- table for backwards compat with any pre-PR-5 builds.
    local results = SynthiqBotsUIResults or SynthiqBotsUISave.feedback_results or {}
    local n = table.getn(q)
    print("|cff00ff00[SynthiqBotsUI]|r Feedback queue: " .. n .. " entries")
    if(n == 0) then return end
    local first = n - 9
    if(first < 1) then first = 1 end
    for i = first, n do
        local e = q[i]
        local resp = results[e.id]
        local respShown = ""
        if(resp ~= nil) then
            local statusStr = resp.status or "?"
            local summaryStr = resp.summary or ""
            if(string.len(summaryStr) > 80) then
                summaryStr = string.sub(summaryStr, 1, 80) .. "..."
            end
            respShown = " — |cffaaffaa" .. statusStr .. "|r: " .. summaryStr
            if(resp.pr_url ~= nil and resp.pr_url ~= "") then
                respShown = respShown .. " |cff66ccff" .. resp.pr_url .. "|r"
            end
        else
            respShown = " — |cff999999pending|r"
        end
        local note = e.note or ""
        if(string.len(note) > 40) then note = string.sub(note, 1, 40) .. "..." end
        print("  [" .. i .. "] ts=" .. (e.ts or 0) .. " note=\"" .. note .. "\"" .. respShown)
    end
end

-- ----------------------------------------------------------------------------
-- Slash command branch (called from SynthiqBotsUIHandler.lua dispatcher)
-- The dispatcher lowercases the rest, but for our note we want the original
-- casing — so we receive the raw `msg` and re-parse here.
-- ----------------------------------------------------------------------------

FB.handleSlash = function(rawMsg)
    -- rawMsg is the full original /sb line, e.g. "feedback Geek wiped us"
    -- Strip the leading "feedback" token and any whitespace.
    local rest = ""
    if(rawMsg ~= nil) then
        local _, _, after = string.find(rawMsg, "^%s*[Ff][Ee][Ee][Dd][Bb][Aa][Cc][Kk]%s*(.*)$")
        if(after ~= nil) then rest = after end
    end
    -- Trim trailing whitespace
    rest = string.gsub(rest, "%s+$", "")

    local lowered = string.lower(rest)
    if(lowered == "log") then
        FB.printLog()
        return
    end
    if(lowered == "help" or lowered == "?") then
        print("|cff00ff00[SynthiqBotsUI]|r Feedback commands:")
        print("  /sb feedback <note>   — capture screenshot + chat + context")
        print("  /sb feedback log      — list recent feedback entries + agent replies")
        return
    end
    -- Default: treat the rest as a freeform note (may be empty)
    FB.capture(rest)
end

-- ----------------------------------------------------------------------------
-- Event frame (independent of the main SynthiqBotsUI dispatcher)
-- ----------------------------------------------------------------------------

local f = CreateFrame("Frame", nil, UIParent)
f:RegisterEvent("CHAT_MSG_SAY")
f:RegisterEvent("CHAT_MSG_YELL")
f:RegisterEvent("CHAT_MSG_PARTY")
f:RegisterEvent("CHAT_MSG_PARTY_LEADER")
f:RegisterEvent("CHAT_MSG_RAID")
f:RegisterEvent("CHAT_MSG_RAID_LEADER")
f:RegisterEvent("CHAT_MSG_GUILD")
f:RegisterEvent("CHAT_MSG_OFFICER")
f:RegisterEvent("CHAT_MSG_CHANNEL")
f:RegisterEvent("CHAT_MSG_WHISPER")
f:RegisterEvent("CHAT_MSG_WHISPER_INFORM")
f:RegisterEvent("CHAT_MSG_EMOTE")

-- 3.3.5a OnEvent uses globals: `event`, `arg1`..`argN`. Match the existing
-- addon's style (see SynthiqBotsUIHandler.lua).
f:SetScript("OnEvent", function()
    if(event == nil) then return end
    local channel = string.gsub(event, "^CHAT_MSG_", "")
    FB.recordLine(channel, arg2, arg1)
end)
