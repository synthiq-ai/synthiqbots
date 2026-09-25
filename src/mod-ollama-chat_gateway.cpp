#include "mod-ollama-chat_gateway.h"
#include "mod-ollama-chat_promotion.h"
#include "mod-ollama-chat_config.h"
#include "mod-ollama-chat_httpclient.h"
#include "mod-ollama-chat_llmwire.h"
#include "mod-ollama-chat_jev.h"
#include "mod-ollama-chat_handler.h"
#include "mod-ollama-chat_tools.h"
#include "mod-ollama-chat_worldtask.h"
#include "mod-ollama-chat-utilities.h"
#include "CharacterCache.h"
#include "DatabaseEnv.h"
#include "Group.h"
#include "Log.h"
#include "ObjectAccessor.h"
#include "Player.h"
#include "PlayerbotAI.h"
#include "PlayerbotMgr.h"
#include <nlohmann/json.hpp>
#include <algorithm>
#include <cctype>
#include <chrono>
#include <mutex>
#include <regex>
#include <sstream>
#include <utility>
#include <vector>

std::unordered_set<uint64_t> g_GatewayBotGUIDSet;
std::unordered_map<uint64_t, GatewayBotConfig> g_GatewayBotConfigs;
std::unordered_set<uint32_t> g_GatewayWhitelistAccountIds;
GatewayStats g_GatewayStats;

namespace
{
    std::atomic<uint32_t> s_activeGatewayRequests{0};

    std::mutex s_cooldownMutex;
    // bot → player → last-request-time
    std::unordered_map<uint64_t, std::unordered_map<uint64_t, time_t>> s_cooldownMap;

    // Replace every occurrence of `needle` in `haystack` with `replacement`.
    std::string ReplaceAll(std::string haystack, const std::string& needle, const std::string& replacement)
    {
        if (needle.empty()) return haystack;
        size_t pos = 0;
        while ((pos = haystack.find(needle, pos)) != std::string::npos)
        {
            haystack.replace(pos, needle.length(), replacement);
            pos += replacement.length();
        }
        return haystack;
    }

    std::string SafeLogSnippet(const std::string& text, size_t maxChars = 512)
    {
        std::string out = text.substr(0, std::min(maxChars, text.size()));
        for (char& c : out)
        {
            unsigned char uc = static_cast<unsigned char>(c);
            if (c == '{') c = '(';
            else if (c == '}') c = ')';
            else if (c == '\n' || c == '\r' || c == '\t' || uc < 0x20) c = ' ';
        }
        if (text.size() > maxChars)
            out += "...";
        return out;
    }
}

bool IsGatewayBot(uint64_t botGuid)
{
    return g_GatewayEnable && g_GatewayBotGUIDSet.count(botGuid) > 0;
}

bool FindGatewayBotConfig(uint64_t botGuid, GatewayBotConfig& out)
{
    auto it = g_GatewayBotConfigs.find(botGuid);
    if (it != g_GatewayBotConfigs.end())
    {
        out = it->second;
        return true;
    }
    // A promoted bot (Gateway.Promote.*) has no override of its own: it borrows
    // a copy of the template bot's, so it reaches the same strategic lane. The
    // global Gateway.Url is empty on this server — without the template the
    // request would go nowhere ("gateway returned empty response").
    return OllamaChat::Promotion::LookupConfig(botGuid, out);
}

bool ExtractGatewayMessage(const std::string& msg, const std::string& keyword, std::string& stripped)
{
    if (keyword.empty() || msg.empty())
        return false;

    // Case-insensitive search for keyword
    std::string msgLower = msg;
    std::string keyLower = keyword;
    std::transform(msgLower.begin(), msgLower.end(), msgLower.begin(),
                   [](unsigned char c) { return std::tolower(c); });
    std::transform(keyLower.begin(), keyLower.end(), keyLower.begin(),
                   [](unsigned char c) { return std::tolower(c); });

    size_t pos = msgLower.find(keyLower);
    if (pos == std::string::npos)
        return false;

    // Remove the keyword from the original message (preserving case of the rest)
    stripped = msg.substr(0, pos) + msg.substr(pos + keyword.length());

    // Trim whitespace
    size_t start = stripped.find_first_not_of(" \t");
    size_t end = stripped.find_last_not_of(" \t");
    if (start == std::string::npos)
    {
        stripped = "";
    }
    else
    {
        stripped = stripped.substr(start, end - start + 1);
    }

    return true;
}

bool TryAcquireGatewaySlot()
{
    if (g_GatewayMaxConcurrentRequests == 0)
    {
        s_activeGatewayRequests.fetch_add(1, std::memory_order_relaxed);
        return true;
    }

    uint32_t current = s_activeGatewayRequests.load(std::memory_order_relaxed);
    while (current < g_GatewayMaxConcurrentRequests)
    {
        if (s_activeGatewayRequests.compare_exchange_weak(current, current + 1,
                                                          std::memory_order_acq_rel))
            return true;
    }
    return false;
}

void ReleaseGatewaySlot()
{
    uint32_t prev = s_activeGatewayRequests.fetch_sub(1, std::memory_order_acq_rel);
    if (prev == 0)
    {
        // Should never happen — bump back to 0 to avoid underflow corruption.
        s_activeGatewayRequests.store(0, std::memory_order_relaxed);
    }
}

bool TryClaimGatewayCooldown(uint64_t botGuid, uint64_t playerGuid)
{
    if (g_GatewayMinSecondsBetweenRequests == 0)
        return true;

    time_t now = time(nullptr);
    std::lock_guard<std::mutex> lock(s_cooldownMutex);
    auto& playerMap = s_cooldownMap[botGuid];
    auto it = playerMap.find(playerGuid);
    if (it != playerMap.end())
    {
        if (now - it->second < static_cast<time_t>(g_GatewayMinSecondsBetweenRequests))
            return false;
    }
    playerMap[playerGuid] = now;
    return true;
}

void ResetGatewayRuntimeState()
{
    s_activeGatewayRequests.store(0, std::memory_order_relaxed);
    {
        std::lock_guard<std::mutex> lock(s_cooldownMutex);
        s_cooldownMap.clear();
    }
    g_GatewayStats.totalRequests.store(0, std::memory_order_relaxed);
    g_GatewayStats.totalErrors.store(0, std::memory_order_relaxed);
    g_GatewayStats.totalRateLimited.store(0, std::memory_order_relaxed);
    g_GatewayStats.totalFallbacks.store(0, std::memory_order_relaxed);
    g_GatewayStats.totalPromptTokens.store(0, std::memory_order_relaxed);
    g_GatewayStats.totalCompletionTokens.store(0, std::memory_order_relaxed);
    {
        std::lock_guard<std::mutex> lock(g_GatewayStats.perBotMutex);
        g_GatewayStats.perBotRequests.clear();
        g_GatewayStats.perBotLastRequest.clear();
    }
}

std::string BuildGatewaySessionKey(uint64_t botGuid, uint64_t playerGuid)
{
    std::string key = g_GatewaySessionKeyTemplate;
    key = ReplaceAll(key, "{botGuid}", std::to_string(botGuid));
    key = ReplaceAll(key, "{playerGuid}", std::to_string(playerGuid));
    return key;
}

std::vector<std::string> SplitForWhisper(const std::string& text, size_t maxChars)
{
    std::vector<std::string> chunks;
    if (text.empty())
        return chunks;

    if (maxChars == 0)
    {
        chunks.push_back(text);
        return chunks;
    }

    size_t pos = 0;
    while (pos < text.size())
    {
        size_t remaining = text.size() - pos;
        if (remaining <= maxChars)
        {
            chunks.push_back(text.substr(pos));
            break;
        }

        // Window of maxChars; try to break on a sentence boundary first, then whitespace.
        size_t windowEnd = pos + maxChars;
        size_t breakAt = std::string::npos;

        // Prefer sentence boundary (".", "!", "?") followed by whitespace or end.
        for (size_t i = windowEnd; i > pos + maxChars / 2; --i)
        {
            char c = text[i - 1];
            if (c == '.' || c == '!' || c == '?')
            {
                breakAt = i;
                break;
            }
        }

        // Fall back to last whitespace in the window.
        if (breakAt == std::string::npos)
        {
            for (size_t i = windowEnd; i > pos + maxChars / 2; --i)
            {
                if (std::isspace(static_cast<unsigned char>(text[i - 1])))
                {
                    breakAt = i;
                    break;
                }
            }
        }

        // Last resort: hard cut at maxChars.
        if (breakAt == std::string::npos)
            breakAt = windowEnd;

        std::string chunk = text.substr(pos, breakAt - pos);
        // Trim trailing whitespace from chunk.
        size_t lastNonSpace = chunk.find_last_not_of(" \t\r\n");
        if (lastNonSpace != std::string::npos)
            chunk.erase(lastNonSpace + 1);

        if (!chunk.empty())
            chunks.push_back(chunk);

        pos = breakAt;

        // Skip whitespace at the start of the next chunk.
        while (pos < text.size() && std::isspace(static_cast<unsigned char>(text[pos])))
            ++pos;
    }

    return chunks;
}

void ParseGatewayAgentByPersonality(const std::string& cfg)
{
    g_GatewayAgentByPersonalityMap.clear();
    if (cfg.empty()) return;

    std::stringstream ss(cfg);
    std::string entry;
    while (std::getline(ss, entry, '|'))
    {
        size_t eq = entry.find('=');
        if (eq == std::string::npos) continue;
        std::string key = entry.substr(0, eq);
        std::string val = entry.substr(eq + 1);

        // Trim
        auto trim = [](std::string& s) {
            size_t a = s.find_first_not_of(" \t");
            size_t b = s.find_last_not_of(" \t");
            if (a == std::string::npos) { s.clear(); return; }
            s = s.substr(a, b - a + 1);
        };
        trim(key);
        trim(val);
        if (key.empty() || val.empty()) continue;

        g_GatewayAgentByPersonalityMap[key] = val;
        LOG_INFO("server.loading", "[Ollama Chat Gateway] Personality '{}' → model '{}'", key, val);
    }
}

std::string LookupBotPersonality(uint64_t botGuid)
{
    auto it = g_BotPersonalityList.find(botGuid);
    if (it != g_BotPersonalityList.end() && !it->second.empty())
        return it->second;
    return "default";
}

std::string ResolveGatewayType(uint64_t botGuid)
{
    std::string t;
    GatewayBotConfig cfg;
    if (FindGatewayBotConfig(botGuid, cfg) && !cfg.gatewayType.empty())
        t = cfg.gatewayType;
    else
        t = g_GatewayType;

    std::transform(t.begin(), t.end(), t.begin(),
                   [](unsigned char c) { return std::tolower(c); });
    if (t != "openclaw" && t != "synthiq")
        t = "openclaw";
    return t;
}

std::string ResolveGatewayModel(uint64_t botGuid, const std::string& fallbackModel)
{
    // Per-bot override (URL/token/model) wins, handled by caller before this.
    // Personality preset is the next-most-specific.
    if (!g_GatewayAgentByPersonalityMap.empty())
    {
        std::string personality = LookupBotPersonality(botGuid);
        auto it = g_GatewayAgentByPersonalityMap.find(personality);
        if (it != g_GatewayAgentByPersonalityMap.end())
            return it->second;
    }
    return fallbackModel;
}

namespace
{
    // Returns true if `text` contains any Cyrillic codepoint (U+0400..U+04FF).
    // Heuristic only — covers Russian/Ukrainian/Belarusian etc.
    bool ContainsCyrillic(const std::string& utf8)
    {
        for (size_t i = 0; i < utf8.size();)
        {
            unsigned char c = static_cast<unsigned char>(utf8[i]);
            if (c < 0x80) { i += 1; continue; }
            if ((c & 0xE0) == 0xC0 && i + 1 < utf8.size())
            {
                uint32_t cp = ((c & 0x1F) << 6) |
                              (static_cast<unsigned char>(utf8[i + 1]) & 0x3F);
                if (cp >= 0x0400 && cp <= 0x04FF) return true;
                i += 2;
            }
            else if ((c & 0xF0) == 0xE0) { i += 3; }
            else if ((c & 0xF8) == 0xF0) { i += 4; }
            else                          { i += 1; }
        }
        return false;
    }

    std::string DetectLanguageLabel(const std::string& utf8)
    {
        if (ContainsCyrillic(utf8)) return "Russian";
        return "English";
    }
}

std::string BuildGatewaySystemPrompt(uint64_t botGuid, const std::string& playerMessage)
{
    return BuildGatewaySystemPromptEx(botGuid, /*playerGuid=*/0, playerMessage);
}

std::string BuildGatewaySystemPromptEx(uint64_t botGuid, uint64_t playerGuid, const std::string& playerMessage)
{
    std::string out;

    if (g_GatewayMergePersonalityPrompt)
    {
        std::string personality = LookupBotPersonality(botGuid);
        auto it = g_PersonalityPrompts.find(personality);
        if (it != g_PersonalityPrompts.end() && !it->second.empty())
            out += it->second;
        else if (!g_DefaultPersonalityPrompt.empty())
            out += g_DefaultPersonalityPrompt;
    }

    if (!g_GatewaySystemPrompt.empty())
    {
        if (!out.empty()) out += "\n\n";
        out += g_GatewaySystemPrompt;
    }

    // PR8a: when the MCP server is enabled, prime the agent with the live identity
    // so it can pass botGuid/playerGuid into MCP tool calls without guessing. The
    // wording is intentionally explicit because earlier (passive) phrasing caused
    // agents to call MCP tools with no arguments — every call returned
    // "player not found", and the agent fabricated stories about people being offline.
    //
    // The rules below are intentionally name-format-agnostic (apply to both bare
    // `whisper_player` and MCP-prefixed `mcp__wow-worldserver__whisper_player`
    // forms). Prior phrasing that scoped itself to `mcp__*` caused strategic
    // escalations — where the upstream exposes tools with bare names via its
    // own MCP client — to omit botGuid and land "missing required argument"
    // errors that the model would then narrate to the user.
    if (g_McpEnable && g_McpInjectContextHint && botGuid != 0)
    {
        std::string hint = "ACTIVE WoW SESSION — your bot's identity is:\n";
        // Names resolved ON THE WORLD THREAD (worldtask.h): this runs on the
        // gateway worker, where neither a Player* nor a CharacterCache read is
        // safe (the cache's map is mutated unlocked by create/delete/rename —
        // codex review). Without the player's NAME the model guessed one for
        // name-taking tools: 2026-09-24, "invite me to the party" invited Raz
        // (the only name in the prompt) and addressed the player as "Raz".
        // Bounded wait; on timeout the block simply carries no names.
        std::string botName, playerName;
        {
            nlohmann::json names = OllamaChat::WorldTask::Run([botGuid, playerGuid]() -> nlohmann::json
            {
                auto nameOf = [](uint64_t guid) {
                    std::string name;
                    if (guid != 0)
                        sCharacterCache->GetCharacterNameByGuid(ObjectGuid(HighGuid::Player, static_cast<uint32>(guid)), name);
                    return name;
                };
                return nlohmann::json{{"bot", nameOf(botGuid)}, {"player", nameOf(playerGuid)}};
            }, 500);
            botName = names.value("bot", std::string{});
            playerName = names.value("player", std::string{});
        }
        hint += "- botGuid = " + std::to_string(botGuid) + (botName.empty() ? "" : ", name = " + botName) + "  (you, the bot character)\n";
        if (playerGuid != 0)
            hint += "- playerGuid = " + std::to_string(playerGuid) + (playerName.empty() ? "" : ", name = " + playerName) +
                    "  (the player talking to you — when a tool needs the player's NAME, e.g. an invite, use this one)\n";
        hint += "\nRules (apply to EVERY tool call — whether the tool name is bare like `whisper_player` or MCP-prefixed like `mcp__wow-worldserver__whisper_player`):\n";
        hint += "1. If a tool's schema lists `botGuid` as required, you MUST include it with value " + std::to_string(botGuid) + ". Examples of such tools: whisper_player, bot_cast, bot_follow, bot_stay, bot_emote, get_bot_state, get_zone_info, get_player_gear, leader_command, leader_command_all, leader_admin_command.\n";
        if (playerGuid != 0)
            hint += "2. If a tool's schema lists `playerGuid` as required, you MUST include it with value " + std::to_string(playerGuid) + ".\n";
        hint += "3. If a tool returns 'player not found' or 'missing required argument', report the literal error to the user. NEVER fabricate a story about anyone being offline — those errors mean YOUR call was malformed, not that anyone is disconnected.\n";
        hint += "4. Prefer get_bot_state for bot questions and get_zone_info for zone questions; do not guess.";

        // Promoted bot (promotion.h): the lane's persona is written for the
        // configured fleet. Tell the model WHO it is now, or it answers as the
        // fleet leader instead of the random character the player addressed.
        if (!IsGatewayBot(botGuid) && OllamaChat::Promotion::IsPromoted(botGuid))
        {
            const std::string me = botName.empty() ? std::string("this character") : botName;
            hint += "\n\nWHO YOU ARE NOW: you are " + me + " — an adventurer of this realm, NOT the fleet leader or any other fleet bot, whatever your standing instructions say about who you are. "
                    "Speak in first person as " + me + ", in character for your own race and class. "
                    "Answer questions about yourself (gear, bags, loot, gold, skills, talents, quests, where you are) by calling tools with YOUR botGuid, never from imagination.";
            if (OllamaChat::Promotion::IsPartyGuest(botGuid))
                hint += " The player invited you into their party: you travel with them now as one of their companions, "
                        "following and obeying them alongside their fleet, until they remove you from the group.";
            else
                hint += " The player just started talking to you; you are not in their party. "
                        "You may not be able to follow or move on their orders until they invite you to their group — say so plainly if asked.";
        }
        if (!out.empty()) out += "\n\n";
        out += hint;
    }

    if (g_GatewayDetectLanguage)
    {
        std::string lang = DetectLanguageLabel(playerMessage);
        std::string hint = g_GatewayLanguageHintTemplate;
        size_t pos = hint.find("{lang}");
        if (pos != std::string::npos) hint.replace(pos, 6, lang);
        if (!out.empty()) out += "\n\n";
        out += hint;
    }

    // Snapshot of the bot's current situation, so the model doesn't hallucinate
    // party state / HP / zone when the player asks "u good?" or "where are we?".
    // Without this block Synthiq's Claude leans on the user's chat text alone and
    // invents plausible but wrong narration ("waiting on you to join the party"
    // when the user is already in the bot's group). Skipping when botGuid=0 (no
    // active bot context).
    if (botGuid != 0)
    {
        if (Player* bot = ObjectAccessor::FindPlayer(ObjectGuid(botGuid)))
        {
            std::string block = "[BOT STATE SNAPSHOT — authoritative, do not contradict]\n";
            block += "name=" + std::string(bot->GetName());
            block += " class=" + std::to_string(bot->getClass());
            block += " level=" + std::to_string(static_cast<uint32_t>(bot->GetLevel()));
            block += " hp_pct=" + std::to_string(static_cast<int>(bot->GetHealthPct()));
            block += " in_combat=" + std::string(bot->IsInCombat() ? "true" : "false");
            block += " zone_id=" + std::to_string(bot->GetZoneId());
            if (Group* g = bot->GetGroup())
            {
                block += " in_group=true group_size=" + std::to_string(g->GetMembersCount()) + "\ngroup_members:";
                uint32_t shown = 0;
                for (GroupReference const* ref = g->GetFirstMember(); ref && shown < 8; ref = ref->next())
                {
                    if (Player* m = ref->GetSource())
                    {
                        if (shown > 0) block += ",";
                        block += " " + std::string(m->GetName());
                        PlayerbotAI* ai = PlayerbotsMgr::instance().GetPlayerbotAI(m);
                        block += (ai && ai->IsBotAI()) ? "(bot)" : "(human)";
                        if (m->GetGUID().GetCounter() == botGuid) block += "<-you";
                        ++shown;
                    }
                }
            }
            else
            {
                block += " in_group=false";
            }
            if (!out.empty()) out += "\n\n";
            out += block;
        }
    }

    // Tier 9: when the active bot is a designated leader, surface the leader_* MCP toolset.
    // Only meaningful when the MCP server is also enabled (otherwise the agent has no transport).
    if (g_McpEnable && g_McpLeaderInjectPromptHint && IsLeaderBot(botGuid))
    {
        std::string hint;
        if (!g_McpLeaderSystemPromptText.empty())
        {
            // jev playbook filter (PR 3): the ~200 KB leader playbook is ~78
            // workflow rows; keep only the rows relevant to THIS message. A
            // human message is required (autonomous ticks have none) and any
            // jev failure sends the full text exactly as before.
            // No ObjectAccessor lookup here: this runs on the gateway worker
            // thread, where a raw Player* is not lifetime-safe (codex review);
            // the filter only needs the message.
            std::string filtered;
            size_t kept = 0, total = 0;
            if (playerGuid != 0 && !playerMessage.empty()
                && Jev::FilterLeaderPlaybook(playerMessage, filtered, kept, total))
                hint += filtered;
            else
                hint += g_McpLeaderSystemPromptText;
        }
        else
            hint += "You are in leader mode. Use MCP leader tools to coordinate the squad, and ask a short clarification when intent is unclear.\n";

        hint = ReplaceAll(hint, "{{botGuid}}", std::to_string(botGuid));
        hint = ReplaceAll(hint, "{{playerGuid}}", std::to_string(playerGuid));

        if (!hint.empty()) hint += "\n\n";
        hint += "Leader runtime context:\n";
        hint += "- botGuid: " + std::to_string(botGuid) + "\n";
        hint += "- playerGuid: " + std::to_string(playerGuid) + "\n";
        hint += "- Available leader tools include leader_list_targets, leader_command, leader_command_all, leader_get_command_help, and the normal bot state/action MCP tools.\n";
        hint += "- Include botGuid in leader tool calls.";
        if (!out.empty()) out += "\n\n";
        out += hint;
    }

    // Tier 10: autonomous-tick mode — there is no human whispering; the system check-in is
    // triggered by the leader-tick thread because something changed.
    if (playerGuid == 0 && IsLeaderBot(botGuid))
    {
        std::string tickHint = "[AUTONOMOUS TICK] No human is whispering. The system is checking in because your situation changed. ";
        tickHint += "Decide if action is warranted. If yes, use leader_command_* MCP tools to issue orders. ";
        tickHint += "If no action is needed, reply briefly.";
        if (!out.empty()) out += "\n\n";
        out += tickHint;
    }

    return out;
}

// -----------------------------------------------------------------------------
// Cheap-path intent classifier
//
// Sends short human messages to a fast local Ollama classifier instead of
// hardcoding language routing in C++. On confident classification, the selected
// allowlisted tool is dispatched in-process via DispatchGatewayTool and a
// one-line ack is returned so the existing response-send code delivers it
// unchanged.
//
// Cost model: ~0.5-1s local inference vs ~2-5s + $0.003 Sonnet call. For the
// clear gameplay commands, this cuts the per-call cost to zero.
// Nuanced/ambiguous orders fall through to Synthiq.
//
// Uses Gateway.OllamaClassifier.{Url,Model,TimeoutMs,NumCtx}, falling back to
// Tactical.Url/Tactical.Model when the classifier endpoint/model is empty.
// -----------------------------------------------------------------------------

namespace {

// Local classifier allowlist — the model's proposed tool name must be in this
// set before C++ dispatches it without the paid gateway. This is a structural
// safety gate, not natural-language intent logic.
const std::unordered_set<std::string> kClassifierAllowedActions = {
    "emergency_stop",
    "bot_follow", "bot_stay", "bot_emote", "bot_revive", "bot_revive_target", "bot_combat_rez", "bot_self_reincarnate", "bot_accept_resurrect_request",
    "bot_rti", "bot_set_raid_target_icon", "bot_disperse",
    "bot_attack_target", "bot_stop_combat", "bot_flee",
    "bot_accept_quest", "bot_turn_in_quest", "bot_autogear", "bot_maintenance",
    "bot_train_spells", "bot_sell_junk", "bot_pet_command",
    "bot_roll", "bot_set_loot_filter", "bot_add_loot_item",
    "bot_remove_loot_item", "bot_invite_to_group", "bot_convert_to_raid",
    "bot_set_group_leader", "bot_uninvite_from_group", "bot_disband_group", "bot_set_loot_method", "bot_set_raid_subgroup", "bot_set_raid_assistant", "bot_raid_ready_check", "bot_set_home",
    "bot_say", "bot_yell", "bot_trade_give", "bot_accept_trade", "bot_guild_create", "bot_accept_guild_invite", "bot_guild_leave", "bot_split_stack", "bot_stack_combine",
    "bot_ah_post", "bot_ah_bid", "bot_ah_cancel",
    "leader_command", "leader_command_all",
};

std::string ResolvePlayerName(uint64_t playerGuid)
{
    if (playerGuid == 0) return {};
    Player* player = ObjectAccessor::FindPlayer(ObjectGuid(playerGuid));
    if (!player || !player->IsInWorld()) return {};
    return std::string(player->GetName());
}

std::string LowerCopy(std::string value)
{
    std::transform(value.begin(), value.end(), value.begin(),
                   [](unsigned char c) { return static_cast<char>(std::tolower(c)); });
    return value;
}

std::string TrimCopy(std::string value)
{
    auto notSpace = [](unsigned char c) { return !std::isspace(c); };
    value.erase(value.begin(), std::find_if(value.begin(), value.end(), notSpace));
    value.erase(std::find_if(value.rbegin(), value.rend(), notSpace).base(), value.end());
    return value;
}

bool IsPlainSafeCommandArg(const std::string& value, size_t maxLen)
{
    if (value.empty() || value.size() > maxLen) return false;
    for (unsigned char c : value)
    {
        if (c < 0x20) return false;
        if (c == '|' || c == '\\') return false;
    }
    return true;
}

bool ContainsToken(const std::string& lowerHaystack, const std::string& lowerNeedle)
{
    if (lowerNeedle.empty()) return false;
    size_t pos = lowerHaystack.find(lowerNeedle);
    while (pos != std::string::npos)
    {
        bool leftOk = (pos == 0) ||
            !std::isalnum(static_cast<unsigned char>(lowerHaystack[pos - 1]));
        size_t end = pos + lowerNeedle.size();
        bool rightOk = (end >= lowerHaystack.size()) ||
            !std::isalnum(static_cast<unsigned char>(lowerHaystack[end]));
        if (leftOk && rightOk)
            return true;
        pos = lowerHaystack.find(lowerNeedle, pos + 1);
    }
    return false;
}

bool LeaderIsGroupedWithHuman(uint64_t botGuid)
{
    Player* bot = ObjectAccessor::FindPlayer(ObjectGuid(botGuid));
    if (!bot || !bot->IsInWorld()) return false;

    Group* group = bot->GetGroup();
    if (!group) return false;

    for (GroupReference const* ref = group->GetFirstMember(); ref; ref = ref->next())
    {
        Player* member = ref->GetSource();
        if (!member || !member->IsInWorld()) continue;

        PlayerbotAI* ai = PlayerbotsMgr::instance().GetPlayerbotAI(member);
        if (!ai || !ai->IsBotAI())
            return true;
    }
    return false;
}

std::string NormalizeClassifierLeaderCommand(uint64_t playerGuid, std::string command)
{
    command = TrimCopy(command);
    std::string lower = LowerCopy(command);

    static const std::unordered_set<std::string> attackCommands = {
        "attack", "attack it", "attack my target", "engage", "kill", "kill it",
        "kill this", "finish it", "burn it", "dps it", "nuke it", "delete it"
    };
    if (attackCommands.count(lower))
        return "attack my target";

    static const std::unordered_set<std::string> followCommands = {
        "follow", "follow me", "come here", "on me", "regroup"
    };
    if (followCommands.count(lower))
    {
        std::string humanName = ResolvePlayerName(playerGuid);
        return humanName.empty() ? std::string{"follow"} : ("follow " + humanName);
    }

    static const std::unordered_set<std::string> stayCommands = {
        "stay", "stay here", "wait", "wait here", "hold", "hold position"
    };
    if (stayCommands.count(lower))
        return "stay";

    if (lower == "accept" || lower == "accept quests" || lower == "accept quest")
        return "accept *";
    if (lower == "talk" || lower == "turn in" || lower == "turn in quests" || lower == "hand in quests")
        return "talk *";
    if (lower == "summon" || lower == "summon me" || lower == "summon to me" ||
        lower == "teleport to me" || lower == "tp to me")
        return "summon";

    return command;
}

std::string ClassifierDirectToolAsLeaderCommand(uint64_t playerGuid, const std::string& action,
                                                const nlohmann::json& args)
{
    if (action == "bot_attack_target")
    {
        std::string target = args.value("target_name", std::string{});
        if (!target.empty() && IsPlainSafeCommandArg(target, 60))
            return "attack " + target;
        return "attack my target";
    }
    if (action == "bot_follow")
    {
        std::string target = args.value("name", std::string{});
        if (target.empty())
            target = ResolvePlayerName(playerGuid);
        return target.empty() ? std::string{"follow"} : ("follow " + target);
    }
    if (action == "bot_stay")
        return "stay";
    if (action == "bot_maintenance")
        return "maintenance";
    if (action == "bot_train_spells")
        return "trainer learn";
    if (action == "bot_sell_junk")
        return "s *";
    if (action == "bot_autogear")
        return "autogear";
    if (action == "bot_set_home")
        return "home";
    if (action == "bot_accept_quest")
    {
        std::string quest = args.value("quest_name", std::string{});
        if (quest.empty())
            return "accept *";
        if (IsPlainSafeCommandArg(quest, 120))
            return "accept " + quest;
    }
    if (action == "bot_turn_in_quest")
        return "talk *";
    if (action == "bot_pet_command")
    {
        std::string command = LowerCopy(args.value("command", std::string{}));
        static const std::unordered_set<std::string> valid{
            "aggressive", "defensive", "passive", "stance", "attack", "follow", "stay"
        };
        if (valid.count(command))
            return "pet " + command;
    }
    if (action == "bot_roll")
    {
        if (args.contains("item_id") && args["item_id"].is_number_integer())
        {
            int64_t itemId = args["item_id"].get<int64_t>();
            if (itemId > 0)
                return "roll " + std::to_string(itemId);
        }
        std::string item = args.value("item_name", std::string{});
        if (!item.empty() && IsPlainSafeCommandArg(item, 80))
            return "roll " + item;
        return "roll";
    }
    if (action == "bot_set_loot_filter")
    {
        std::string mode = LowerCopy(args.value("mode", std::string{}));
        // Mirrors upstream LootStrategyValue: all / gray / disenchant / normal are
        // the real strategies; quest and skill fall through to normal there. Keep
        // `disenchant` here so a squad-scoped "Geek disenchant loot" stays a
        // leader_command and never degrades to a leader-only tool call.
        static const std::unordered_set<std::string> valid{"all", "normal", "gray", "quest", "skill", "disenchant"};
        if (valid.count(mode))
            return "ll " + mode;
    }
    if (action == "bot_rti")
    {
        std::string icon = LowerCopy(args.value("icon", std::string{}));
        std::string mode = LowerCopy(args.value("mode", std::string{"main"}));
        static const std::unordered_set<std::string> valid{
            "skull", "cross", "circle", "star", "square", "triangle", "diamond", "moon", "none"
        };
        if (valid.count(icon))
            return mode == "cc" ? ("rti cc " + icon) : ("rti " + icon);
    }
    if (action == "bot_disperse")
    {
        if (args.contains("yards") && args["yards"].is_number_integer())
        {
            int yards = args["yards"].get<int>();
            if (yards == 0)
                return "disperse disable";
            if (yards > 0 && yards <= 40)
                return "disperse set " + std::to_string(yards);
        }
    }

    return {};
}

std::string FindMentionedNonLeaderBotName(uint64_t leaderGuid, const std::string& message)
{
    std::string lowerMessage = LowerCopy(message);
    for (auto const& pair : ObjectAccessor::GetPlayers())
    {
        Player* player = pair.second;
        if (!player || !player->IsInWorld()) continue;
        if (player->GetGUID().GetCounter() == leaderGuid) continue;
        if (!PlayerbotsMgr::instance().GetPlayerbotAI(player)) continue;

        std::string name = std::string(player->GetName());
        if (ContainsToken(lowerMessage, LowerCopy(name)))
            return name;
    }
    return {};
}

bool MessageRequestsEatAndDrink(const std::string& message)
{
    std::string lower = LowerCopy(message);
    return ContainsToken(lower, "eat") &&
        (ContainsToken(lower, "drink") || ContainsToken(lower, "mana"));
}

bool DispatchClassifierLeaderSequence(uint64_t botGuid, uint64_t playerGuid,
                                      const std::string& action,
                                      const nlohmann::json& baseArgs,
                                      const std::vector<std::string>& commands,
                                      const std::string& reply, float confidence,
                                      const char* source, std::string& outReply)
{
    for (const std::string& command : commands)
    {
        nlohmann::json stepArgs = baseArgs;
        stepArgs["botGuid"] = botGuid;
        stepArgs["command"] = command;
        nlohmann::json result = DispatchGatewayTool(botGuid, playerGuid, action, stepArgs);
        if (result.is_object() && result.contains("error"))
        {
            LOG_INFO("server.loading",
                     "[Ollama Chat Gateway Classifier] leader command sequence action={} step='{}' errored: {} — falling through to Synthiq",
                     action, command, SafeLogSnippet(result["error"].dump(), 240));
            return false;
        }
    }

    LOG_INFO("server.loading",
             "[Ollama Chat Gateway Classifier] bot={} action={} sequence_len={} conf={:.2f} source={} reply=\"{}\" (saved a Synthiq call)",
             botGuid, action, commands.size(), confidence, source, SafeLogSnippet(reply, 80));
    outReply = reply.empty() ? std::string{"on it"} : reply;
    return true;
}

std::string BuildClassifierSystemPrompt(uint64_t botGuid, uint64_t playerGuid)
{
    std::string humanName = ResolvePlayerName(playerGuid);
    bool groupedWithHuman = LeaderIsGroupedWithHuman(botGuid);

    std::vector<std::string> allowed(kClassifierAllowedActions.begin(), kClassifierAllowedActions.end());
    std::sort(allowed.begin(), allowed.end());
    std::string allowedActions;
    for (size_t i = 0; i < allowed.size(); ++i)
    {
        if (i != 0) allowedActions += ", ";
        allowedActions += allowed[i];
    }

    std::string p;
    if (!g_GatewayOllamaClassifierSystemPromptText.empty())
        p += g_GatewayOllamaClassifierSystemPromptText;
    else
        p += "You are the local WoW bot command classifier. Interpret the human's message and choose one allowed tool call, or return unknown when unsure.\n";

    p = ReplaceAll(p, "{{botGuid}}", std::to_string(botGuid));
    p = ReplaceAll(p, "{{playerGuid}}", std::to_string(playerGuid));
    p = ReplaceAll(p, "{{humanName}}", humanName.empty() ? "unknown" : humanName);
    p = ReplaceAll(p, "{{leaderGroupedWithHuman}}", groupedWithHuman ? "true" : "false");
    p = ReplaceAll(p, "{{allowedActions}}", allowedActions);

    if (!p.empty()) p += "\n\n";
    p += "Runtime context:\n";
    p += "- botGuid: " + std::to_string(botGuid) + "\n";
    p += "- playerGuid: " + std::to_string(playerGuid) + "\n";
    p += "- humanName: " + (humanName.empty() ? std::string{"unknown"} : nlohmann::json(humanName).dump()) + "\n";
    p += "- leaderGroupedWithHuman: " + std::string(groupedWithHuman ? "true" : "false") + "\n";
    p += "- allowedActions: " + allowedActions + "\n";
    p += "- C++ will reject any action not listed in allowedActions.\n";
    p += "- Always include botGuid in args for tools that operate on the current bot or leader.\n";
    p += "Reply with EXACTLY ONE JSON object and nothing else:\n";
    p += "  {\"action\": \"<tool_name>\", \"args\": {...}, \"confidence\": <0.0-1.0>, \"reply\": \"<short ack, max 40 chars>\"}\n";
    return p;
}

std::string DispatchLocalClassifierPlan(uint64_t botGuid, uint64_t playerGuid,
                                        const std::string& message,
                                        std::string action, nlohmann::json args,
                                        std::string reply, float confidence,
                                        const char* source)
{
    if (action.empty() || action == "unknown") return {};
    if (!kClassifierAllowedActions.count(action))
    {
        LOG_DEBUG("server.loading",
                  "[Ollama Chat Gateway Classifier] rejecting non-allowlisted action '{}' from {} — falling through to Synthiq",
                  action, source);
        return {};
    }

    // Ensure botGuid is injected even if model forgot — defense in depth;
    // DispatchGatewayTool handlers require it for most tools and the MCP
    // server-side fallback only applies to the HTTP transport, not this
    // in-process call.
    if (!args.is_object())
        args = nlohmann::json::object();
    if (!args.contains("botGuid") || !args["botGuid"].is_number())
        args["botGuid"] = botGuid;

    // Claude is used as the command interface/leader. If the classifier selects
    // a direct single-bot tool for a command that has an equivalent raw
    // playerbot broadcast, rewrite the tool structurally instead of letting a
    // party-chat order affect Claude only. This is not phrase routing; the LLM
    // already chose the tool intent, and this layer only enforces leader scope.
    if (IsLeaderBot(botGuid))
    {
        std::string originalAction = action;
        std::string mentionedTarget = FindMentionedNonLeaderBotName(botGuid, message);

        if (action == "bot_revive")
        {
            action = mentionedTarget.empty() ? std::string{"leader_command_all"} : std::string{"leader_command"};
            args = nlohmann::json{{"botGuid", botGuid}};
            if (!mentionedTarget.empty())
                args["targetBotName"] = mentionedTarget;

            std::string sequenceReply;
            if (DispatchClassifierLeaderSequence(botGuid, playerGuid, action, args,
                                                 {"release", "revive"}, reply.empty() ? "Reviving." : reply,
                                                 confidence, source, sequenceReply))
                return sequenceReply;
            return {};
        }

        if (action == "leader_command" && args.value("targetBotName", std::string{}).empty())
        {
            if (!mentionedTarget.empty())
                args["targetBotName"] = mentionedTarget;
            else
                action = "leader_command_all";
        }

        if (action == "leader_command_all" && !mentionedTarget.empty())
        {
            action = "leader_command";
            args["targetBotName"] = mentionedTarget;
        }

        if (action == "leader_command" || action == "leader_command_all")
        {
            std::string command = args.value("command", std::string{});
            if (!command.empty())
            {
                args["command"] = NormalizeClassifierLeaderCommand(playerGuid, command);
                command = args["command"].get<std::string>();
                if (MessageRequestsEatAndDrink(message) && (command == "eat" || command == "drink"))
                {
                    std::string sequenceReply;
                    if (DispatchClassifierLeaderSequence(botGuid, playerGuid, action, args,
                                                         {"eat", "drink"}, reply, confidence,
                                                         source, sequenceReply))
                        return sequenceReply;
                    return {};
                }
            }
        }
        else
        {
            std::string command = ClassifierDirectToolAsLeaderCommand(playerGuid, action, args);
            if (!command.empty())
            {
                action = mentionedTarget.empty() ? std::string{"leader_command_all"} : std::string{"leader_command"};
                args = nlohmann::json{{"botGuid", botGuid}, {"command", command}};
                if (!mentionedTarget.empty())
                    args["targetBotName"] = mentionedTarget;
            }
        }

        if (action != originalAction)
        {
            LOG_DEBUG("server.loading",
                      "[Ollama Chat Gateway Classifier] rewrote leader default action {} -> {}",
                      originalAction, action);
        }
    }

    // Dispatch in-process. On error, fall through to Synthiq so the user
    // gets a proper in-character explanation rather than a silent no-op.
    nlohmann::json result = DispatchGatewayTool(botGuid, playerGuid, action, args);
    if (result.is_object() && result.contains("error"))
    {
        LOG_INFO("server.loading",
                 "[Ollama Chat Gateway Classifier] action={} conf={:.2f} source={} dispatched but errored: {} — falling through to Synthiq",
                 action, confidence, source, SafeLogSnippet(result["error"].dump(), 240));
        return {};
    }

    LOG_INFO("server.loading",
             "[Ollama Chat Gateway Classifier] bot={} action={} conf={:.2f} source={} reply=\"{}\" (saved a Synthiq call)",
             botGuid, action, confidence, source, SafeLogSnippet(reply, 80));

    return reply.empty() ? std::string{"on it"} : reply;
}

// -----------------------------------------------------------------------------
// jev tier of the classifier (site A). One parallel request: which allowed tool
// (or `unknown`), which raw squad command, which icon / loot mode / pet stance
// — read only when the chosen action needs them. jev cannot produce free text,
// so an action whose args the answers cannot fill is a FALL-THROUGH to the LLM
// tier below (which does produce args), never a dispatch with missing args.
// Eligibility is computed from the tool registry's `required` list against
// the slots this tier can fill, not hand-listed — so a schema change cannot
// silently make a jev pick dispatch with a hole in it.
// -----------------------------------------------------------------------------

// Audit label for classifier calls (both tiers). ≤15 chars for VARCHAR(16).
constexpr char const* kSrcGwClassifier = "gw_classifier";   // 13
static_assert(sizeof("gw_classifier") - 1 <= 15, "audit label exceeds VARCHAR(16) safety margin");

// Slots the jev answers can fill per action (botGuid is always injected).
const std::unordered_map<std::string, std::vector<std::string>> kJevClassifierFillable = {
    {"leader_command_all",          {"command"}},
    {"leader_command",              {"command", "targetBotName"}},
    {"emergency_stop",              {}},
    {"bot_revive",                  {}},
    // bot_set_raid_target_icon is deliberately ABSENT: its optional target_name
    // ("mark Ragnaros skull") cannot be filled from a Choice, and dispatching
    // without it marks the master's current selection — the wrong mob, silently.
    {"bot_rti",                     {"icon"}},
    {"bot_set_loot_filter",         {"mode"}},
    {"bot_pet_command",             {"command"}},
    {"bot_roll",                    {}},
    {"bot_sell_junk",               {}},
    {"bot_autogear",                {}},
    {"bot_set_home",                {}},
    {"bot_accept_resurrect_request",{}},
    {"bot_stop_combat",             {}},
    {"bot_flee",                    {}},
    {"bot_accept_quest",            {}},
    {"bot_turn_in_quest",           {}},
    {"bot_maintenance",             {}},
    {"bot_train_spells",            {}},
    {"bot_follow",                  {}},
    {"bot_stay",                    {}},
};

bool JevClassifierEligible(const std::string& action)
{
    if (!kClassifierAllowedActions.count(action)) return false;
    auto fill = kJevClassifierFillable.find(action);
    if (fill == kJevClassifierFillable.end()) return false;

    const auto& reg = GetGatewayToolRegistry();
    auto tool = reg.find(action);
    if (tool == reg.end()) return false;
    const nlohmann::json& schema = tool->second.parametersSchema;
    if (!schema.is_object() || !schema.contains("required") || !schema["required"].is_array())
        return true;
    for (const auto& req : schema["required"])
    {
        if (!req.is_string()) return false;
        const std::string key = req.get<std::string>();
        if (key == "botGuid") continue;
        if (std::find(fill->second.begin(), fill->second.end(), key) == fill->second.end())
            return false;   // the registry demands something this tier cannot supply
    }
    return true;
}

std::vector<std::string> CollectNonLeaderBotNames(uint64_t leaderGuid, size_t cap = 10)
{
    std::vector<std::string> names;
    for (auto const& pair : ObjectAccessor::GetPlayers())
    {
        Player* player = pair.second;
        if (!player || !player->IsInWorld()) continue;
        if (player->GetGUID().GetCounter() == leaderGuid) continue;
        if (!PlayerbotsMgr::instance().GetPlayerbotAI(player)) continue;
        names.emplace_back(player->GetName());
        if (names.size() >= cap) break;
    }
    std::sort(names.begin(), names.end());
    return names;
}

std::vector<std::string> CriteriaKeys(const nlohmann::json& tmpl, const std::vector<std::string>& fallback)
{
    if (!tmpl.is_object() || !tmpl.contains("criteria") || !tmpl["criteria"].is_object()) return fallback;
    std::vector<std::string> keys;
    for (auto it = tmpl["criteria"].begin(); it != tmpl["criteria"].end(); ++it) keys.push_back(it.key());
    return keys.empty() ? fallback : keys;
}

// Canned acknowledgement from the questions file's `reply` table; jev writes no text.
std::string JevCannedReply(const nlohmann::json& replyTable, const std::string& action,
                           const std::string& command, const std::string& humanName,
                           const std::string& targetBot)
{
    std::string reply;
    if (replyTable.is_object() && replyTable.contains(action))
    {
        const auto& r = replyTable[action];
        if (r.is_string()) reply = r.get<std::string>();
        else if (r.is_object() && r.contains(command) && r[command].is_string()) reply = r[command].get<std::string>();
    }
    if (reply.empty()) return reply;
    reply = ReplaceAll(reply, "{{humanName}}", humanName.empty() ? "you" : humanName);
    reply = ReplaceAll(reply, "{{target}}", targetBot.empty() ? "Squad" : targetBot);
    return reply;
}

void WriteClassifierAudit(uint64_t botGuid, uint64_t playerGuid, const std::string& message,
                          const std::string& reply, uint32_t latencyMs, const char* backend,
                          uint32_t promptTokens, bool error)
{
    Player* player = ObjectAccessor::FindPlayer(ObjectGuid(playerGuid));
    uint32_t accountId = (player && player->GetSession()) ? player->GetSession()->GetAccountId() : 0;
    WriteGatewayAuditRecord(botGuid, playerGuid, accountId,
                            static_cast<uint32_t>(message.size()),
                            static_cast<uint32_t>(reply.size()),
                            promptTokens, 0, latencyMs, kSrcGwClassifier, error, backend);
}

std::string TryJevIntentClassify(uint64_t botGuid, uint64_t playerGuid, const std::string& message)
{
    const std::string humanName = ResolvePlayerName(playerGuid);
    const std::string mentionedTarget = FindMentionedNonLeaderBotName(botGuid, message);

    nlohmann::json state{
        {"message",                message},
        {"humanName",              humanName.empty() ? "unknown" : humanName},
        {"leaderGroupedWithHuman", LeaderIsGroupedWithHuman(botGuid)},
        {"botNames",               CollectNonLeaderBotNames(botGuid)},
    };

    // Option SET = the live allowlist (+ unknown); the file only supplies wording.
    std::vector<std::string> actions(kClassifierAllowedActions.begin(), kClassifierAllowedActions.end());
    std::sort(actions.begin(), actions.end());
    actions.push_back("unknown");

    const nlohmann::json site = Jev::QuestionTemplate(Jev::kSiteClassifier);
    auto tmpl = [&](const char* id) -> nlohmann::json {
        return site.is_object() && site.contains(id) ? site[id] : nlohmann::json(nullptr);
    };
    static const std::vector<std::string> kCommands{
        "attack my target", "follow", "stay", "accept *", "talk *", "summon", "eat", "drink",
        "maintenance", "trainer learn", "u 6948", "release", "leave", "none"};
    static const std::vector<std::string> kIcons{
        "skull", "cross", "circle", "star", "square", "triangle", "diamond", "moon", "none"};
    static const std::vector<std::string> kLootModes{"all", "normal", "gray", "disenchant"};
    static const std::vector<std::string> kPetCommands{"follow", "stay", "passive", "defensive", "aggressive", "none"};

    nlohmann::json questions{
        {"action",      Jev::ChoiceFromTemplate(tmpl("action"),      actions)},
        {"command",     Jev::ChoiceFromTemplate(tmpl("command"),     CriteriaKeys(tmpl("command"), kCommands))},
        {"icon",        Jev::ChoiceFromTemplate(tmpl("icon"),        kIcons)},
        {"loot_filter", Jev::ChoiceFromTemplate(tmpl("loot_filter"), kLootModes)},
        {"pet_command", Jev::ChoiceFromTemplate(tmpl("pet_command"), kPetCommands)},
    };

    // A has no outer budget (the classifier timeout IS the budget, on a detached
    // per-message thread), so jev is simply prepended with its own cap and the
    // DeepSeek tier keeps its full 5 s.
    // 2000, not 1500: live round trips are 1.25-1.6 s (2026-09-21) and this path has no outer budget.
    const uint32_t cap = g_JevTimeoutMs == 0 ? 2000u : std::min<uint32_t>(g_JevTimeoutMs, 2000u);
    Jev::Result r = Jev::Decide(state, questions, cap, Jev::kSiteClassifier);
    if (!r.ok) return {};   // logged by Decide; LLM tier next

    const float minConf = r.minConfidence;   // the generation that answered, not a re-read knob
    const Jev::Answer& act = *r.Find("action");
    if (act.choice == "unknown")
    {
        LOG_INFO("server.loading", "[Ollama Chat Gateway Classifier] jev: unknown conf={:.2f} — LLM tier", act.confidence);
        return {};
    }
    if (act.confidence < minConf)
    {
        LOG_INFO("server.loading", "[Ollama Chat Gateway Classifier] jev: {} conf={:.2f} < {:.2f} — LLM tier",
                 act.choice, act.confidence, minConf);
        return {};
    }
    if (!JevClassifierEligible(act.choice))
    {
        LOG_INFO("server.loading", "[Ollama Chat Gateway Classifier] jev: {} conf={:.2f} needs args this tier cannot fill — LLM tier",
                 act.choice, act.confidence);
        return {};
    }

    // Fill args from the speculative answers; every consumed answer is gated on
    // its own confidence, not just `action`.
    std::string action = act.choice;
    nlohmann::json args{{"botGuid", botGuid}};
    std::string command;
    auto gated = [&](const char* id, std::string& outChoice) -> bool {
        const Jev::Answer* a = r.Find(id);
        if (!a || a->choice == "none" || a->confidence < minConf) return false;
        outChoice = a->choice;
        return true;
    };
    if (action == "leader_command" || action == "leader_command_all")
    {
        if (!gated("command", command)) return {};
        args["command"] = command == "follow" ? "follow " + (humanName.empty() ? std::string("me") : humanName) : command;
        if (action == "leader_command")
            args["targetBotName"] = mentionedTarget;   // empty -> DispatchLocalClassifierPlan resolves / broadens
    }
    else if (action == "bot_pet_command")
    {
        if (!gated("pet_command", command)) return {};
        args["command"] = command;
    }
    else if (action == "bot_rti")
    {
        const Jev::Answer* a = r.Find("icon");
        if (!a || a->confidence < minConf) return {};
        args["icon"] = a->choice;   // "none" is a valid rti value (clears the mark)
    }
    else if (action == "bot_set_loot_filter")
    {
        std::string mode;
        if (!gated("loot_filter", mode)) return {};
        args["mode"] = mode;
    }

    std::string reply = JevCannedReply(tmpl("reply"), action, command, humanName, mentionedTarget);
    std::string outReply = DispatchLocalClassifierPlan(botGuid, playerGuid, message, action, args,
                                                       reply, act.confidence, "jev");
    WriteClassifierAudit(botGuid, playerGuid, message, outReply, r.latencyMs,
                         Jev::kBackendJev, r.inputTokens, outReply.empty());
    return outReply;
}

// Whitespace-separated word count; the interceptor gate, not a tokenizer.
// Separators are ASCII whitespace plus the Unicode spaces a chat client can
// emit (NBSP U+00A0, U+1680, U+2000-200A, U+2028/2029, U+202F, U+205F,
// U+3000) — a NBSP-joined line must not slip past MaxWords as one "word".
bool IsWordSeparator(uint32_t cp)
{
    if (cp < 0x80) return std::isspace(static_cast<unsigned char>(cp)) != 0;
    return cp == 0x00A0 || cp == 0x1680 || (cp >= 0x2000 && cp <= 0x200A)
        || cp == 0x2028 || cp == 0x2029 || cp == 0x202F || cp == 0x205F || cp == 0x3000;
}

size_t CountWords(const std::string& s)
{
    size_t n = 0; bool inWord = false;
    for (size_t i = 0; i < s.size();)
    {
        unsigned char b = static_cast<unsigned char>(s[i]);
        uint32_t cp = b; size_t len = 1;
        if      ((b & 0xE0) == 0xC0 && i + 1 < s.size()) { cp = ((b & 0x1F) << 6)  | (s[i+1] & 0x3F); len = 2; }
        else if ((b & 0xF0) == 0xE0 && i + 2 < s.size()) { cp = ((b & 0x0F) << 12) | ((s[i+1] & 0x3F) << 6) | (s[i+2] & 0x3F); len = 3; }
        else if ((b & 0xF8) == 0xF0 && i + 3 < s.size()) { cp = 0x10000; len = 4; }   // never a separator
        i += len;
        if (IsWordSeparator(cp)) inWord = false;
        else if (!inWord) { inWord = true; ++n; }
    }
    return n;
}

} // namespace

std::string TryGatewayIntentClassify(uint64_t botGuid, uint64_t playerGuid, const std::string& message)
{
    // Two independent tiers: jev (its own site flag) and the LLM classifier
    // (OllamaClassifier.Enable). Either alone is a valid interceptor; with the
    // LLM tier off a jev miss goes straight to the bot's brain (the gateway).
    const bool jevTier = Jev::EnabledFor(Jev::kSiteClassifier);
    const bool llmTier = g_GatewayOllamaClassifierEnable;
    if (!jevTier && !llmTier) return {};
    if (message.empty() || message.size() > 256) return {};  // skip long/conversational
    // Operator decision 2026-09-21: only terse imperative lines are intercepted
    // ("attack", "follow me", "stay"); anything longer is conversation for the
    // in-character gateway, tools included. 0 = no word gate.
    const uint32_t maxWords = g_GatewayClassifierMaxWords.load(std::memory_order_relaxed);   // one read; reload may race
    if (maxWords > 0 && CountWords(message) > maxWords) return {};

    // Tier 1 — jev (calibrated typed decision). Empty result = fall through to
    // the LLM tier with its full budget untouched.
    if (jevTier)
    {
        std::string jevReply = TryJevIntentClassify(botGuid, playerGuid, message);
        if (!jevReply.empty()) return jevReply;
    }
    if (!llmTier) return {};

    std::string classifierUrl = g_GatewayOllamaClassifierUrl.empty() ? g_TacticalUrl : g_GatewayOllamaClassifierUrl;
    std::string classifierModel = g_GatewayOllamaClassifierModel.empty() ? g_TacticalModel : g_GatewayOllamaClassifierModel;
    if (classifierUrl.empty() || classifierModel.empty()) return {};

    // Shape chosen from the URL by LlmWire (Ollama /api/generate or OpenAI
    // /chat/completions) — see mod-ollama-chat_llmwire.h.
    nlohmann::json req = LlmWire::BuildRequest(classifierUrl, classifierModel,
                                               SanitizeUTF8(BuildClassifierSystemPrompt(botGuid, playerGuid)),
                                               SanitizeUTF8(message),
                                               g_GatewayOllamaClassifierNumCtx,
                                               /*jsonMode=*/true, /*noThink=*/true);

    OllamaHttpClient client;
    uint32_t timeoutMs = g_GatewayOllamaClassifierTimeoutMs;
    uint32_t timeoutSec = timeoutMs == 0 ? 5u : std::max<uint32_t>(1u, (timeoutMs + 999u) / 1000u);
    client.SetTimeout(static_cast<int>(timeoutSec));  // classifier must be fast or we fall through
    if (!client.IsAvailable()) return {};

    const auto llmT0 = std::chrono::steady_clock::now();
    std::string raw = client.Post(classifierUrl, req.dump(), LlmWire::Headers(classifierUrl));
    const uint32_t llmLatencyMs = static_cast<uint32_t>(
        std::chrono::duration_cast<std::chrono::milliseconds>(std::chrono::steady_clock::now() - llmT0).count());
    if (raw.empty()) return {};

    // Unwrap whichever envelope the URL's protocol uses. The classifier is a
    // cheap pre-filter that silently falls through to the paid gateway on any
    // failure, so a provider error must at least be visible in the log.
    std::string content, wireErr;
    if (!LlmWire::ExtractText(classifierUrl, raw, content, wireErr))
    {
        LOG_WARN("server.loading", "[Ollama Chat Gateway] classifier ({}) {} — falling through to Synthiq",
                 LlmWire::FormatName(classifierUrl), wireErr);
        return {};
    }
    if (content.empty()) return {};

    // Parse the model's classification JSON
    nlohmann::json classification;
    try { classification = nlohmann::json::parse(content); }
    catch (...) { return {}; }
    if (!classification.is_object()) return {};

    std::string action = classification.value("action", std::string{});
    float confidence   = classification.value("confidence", 0.0f);
    std::string reply  = classification.value("reply", std::string{});
    nlohmann::json args = classification.value("args", nlohmann::json::object());

    if (action.empty() || action == "unknown") return {};
    if (confidence < g_GatewayOllamaClassifierMinConfidence) return {};
    std::string outReply = DispatchLocalClassifierPlan(botGuid, playerGuid, message, action, args, reply, confidence, "ollama");
    if (!outReply.empty())
        WriteClassifierAudit(botGuid, playerGuid, message, outReply, llmLatencyMs, Jev::kBackendLlm, 0, false);
    return outReply;
}

std::string QueryGatewayAPI(uint64_t botGuid, uint64_t playerGuid, const std::string& message)
{
    static OllamaHttpClient httpClient;
    httpClient.SetTimeout(static_cast<int>(g_GatewayTimeoutSeconds));

    if (!httpClient.IsAvailable())
    {
        LOG_ERROR("server.loading", "[Ollama Chat Gateway] HTTP client not available");
        g_GatewayStats.totalErrors.fetch_add(1, std::memory_order_relaxed);
        return "";
    }

    g_GatewayStats.totalRequests.fetch_add(1, std::memory_order_relaxed);
    {
        std::lock_guard<std::mutex> lock(g_GatewayStats.perBotMutex);
        g_GatewayStats.perBotRequests[botGuid] += 1;
        g_GatewayStats.perBotLastRequest[botGuid] = time(nullptr);
    }

    // Build messages array (OpenAI chat completions format)
    nlohmann::json messages = nlohmann::json::array();

    // System prompt: optionally merges personality + base + language hint.
    std::string sysPrompt = BuildGatewaySystemPromptEx(botGuid, playerGuid, message);
    if (!sysPrompt.empty())
    {
        messages.push_back({
            {"role", "system"},
            {"content", SanitizeUTF8(sysPrompt)}
        });
    }

    // Conversation history (skipped when session persistence handles continuity server-side)
    bool sendHistory = (g_GatewayMaxHistory > 0) &&
                       !(g_GatewayUseSessionPersistence && g_GatewaySkipHistoryWhenSession);
    if (sendHistory)
    {
        std::lock_guard<std::mutex> lock(g_ConversationHistoryMutex);
        auto botIt = g_BotConversationHistory.find(botGuid);
        if (botIt != g_BotConversationHistory.end())
        {
            auto playerIt = botIt->second.find(playerGuid);
            if (playerIt != botIt->second.end())
            {
                const auto& history = playerIt->second;
                size_t startIdx = 0;
                if (history.size() > g_GatewayMaxHistory)
                    startIdx = history.size() - g_GatewayMaxHistory;

                for (size_t i = startIdx; i < history.size(); ++i)
                {
                    messages.push_back({
                        {"role", "user"},
                        {"content", SanitizeUTF8(history[i].first)}
                    });
                    messages.push_back({
                        {"role", "assistant"},
                        {"content", SanitizeUTF8(history[i].second)}
                    });
                }
            }
        }
    }

    // Current user message
    messages.push_back({
        {"role", "user"},
        {"content", SanitizeUTF8(message)}
    });

    // Resolve per-bot config or fall back to globals
    std::string url = g_GatewayUrl;
    std::string token = g_GatewayBearerToken;
    std::string model = g_GatewayModel;

    bool hadModelOverride = false;
    GatewayBotConfig overrideCfg;
    const bool hasOverride = FindGatewayBotConfig(botGuid, overrideCfg);
    if (hasOverride)
    {
        const auto& cfg = overrideCfg;
        if (!cfg.url.empty()) url = cfg.url;
        if (!cfg.bearerToken.empty()) token = cfg.bearerToken;
        if (!cfg.model.empty()) { model = cfg.model; hadModelOverride = true; }
    }

    // Personality preset overrides the global model (but never the per-bot override).
    if (!hadModelOverride)
        model = ResolveGatewayModel(botGuid, model);

    nlohmann::json requestData = {
        {"model", model},
        {"messages", messages},
        {"stream", false},
        {"user", g_GatewayUserPrefix + std::to_string(botGuid)}
    };

    std::string requestDataStr = requestData.dump();

    if (g_DebugEnabled)
    {
        LOG_INFO("server.loading", "[Ollama Chat Gateway] Sending request to {}, model={}, messages count={}",
                 url, model, messages.size());
        if (g_DebugShowFullPrompt)
        {
            LOG_INFO("server.loading", "[Ollama Chat Gateway] Request body chars={}, preview=\"{}\"",
                     requestDataStr.size(), SafeLogSnippet(requestDataStr));
        }
    }

    // Build auth headers
    std::vector<std::pair<std::string, std::string>> extraHeaders = {
        {"Authorization", "Bearer " + token},
        {"x-openclaw-scopes", g_GatewayScopes}
    };

    if (g_GatewayUseSessionPersistence)
    {
        std::string sessionKey = BuildGatewaySessionKey(botGuid, playerGuid);
        if (!sessionKey.empty())
            extraHeaders.emplace_back("x-synthiq-session-key", sessionKey);
    }

    // Per-bot custom headers (from JSON overrides) — appended last so they can override defaults.
    if (hasOverride)
        for (const auto& kv : overrideCfg.headers)
            extraHeaders.emplace_back(kv.first, kv.second);

    std::string responseBuffer = httpClient.Post(url, requestDataStr, extraHeaders);

    if (responseBuffer.empty())
    {
        LOG_ERROR("server.loading", "[Ollama Chat Gateway] Empty response from gateway at {}", url);
        g_GatewayStats.totalErrors.fetch_add(1, std::memory_order_relaxed);
        return "";
    }

    try
    {
        nlohmann::json jsonResponse = nlohmann::json::parse(responseBuffer);

        // Token usage accounting (OpenAI-style)
        if (jsonResponse.contains("usage") && jsonResponse["usage"].is_object())
        {
            const auto& usage = jsonResponse["usage"];
            uint64_t pTok = usage.value("prompt_tokens", 0ULL);
            uint64_t cTok = usage.value("completion_tokens", 0ULL);
            g_GatewayStats.totalPromptTokens.fetch_add(pTok, std::memory_order_relaxed);
            g_GatewayStats.totalCompletionTokens.fetch_add(cTok, std::memory_order_relaxed);
            LOG_INFO("server.loading",
                     "[Ollama Chat Gateway] Tokens — bot={}, player={}, prompt={}, completion={}, total={}",
                     botGuid, playerGuid, pTok, cTok, usage.value("total_tokens", pTok + cTok));
        }

        if (jsonResponse.contains("choices") &&
            jsonResponse["choices"].is_array() &&
            !jsonResponse["choices"].empty())
        {
            auto& firstChoice = jsonResponse["choices"][0];
            if (firstChoice.contains("message") &&
                firstChoice["message"].contains("content"))
            {
                std::string content = firstChoice["message"]["content"].get<std::string>();

                if (g_DebugEnabled)
                {
                    LOG_INFO("server.loading", "[Ollama Chat Gateway] Response chars={}, preview=\"{}\"",
                             content.size(), SafeLogSnippet(content));
                }

                return content;
            }
        }

        LOG_ERROR("server.loading", "[Ollama Chat Gateway] Unexpected response format — missing choices[0].message.content");
        if (g_DebugEnabled)
        {
            LOG_INFO("server.loading", "[Ollama Chat Gateway] Response body chars={}, preview=\"{}\"",
                     responseBuffer.size(), SafeLogSnippet(responseBuffer));
        }
    }
    catch (const std::exception& e)
    {
        LOG_ERROR("server.loading", "[Ollama Chat Gateway] JSON parse error: {}", e.what());
        if (g_DebugEnabled)
        {
            LOG_INFO("server.loading", "[Ollama Chat Gateway] Response body chars={}, preview=\"{}\"",
                     responseBuffer.size(), SafeLogSnippet(responseBuffer));
        }
    }

    g_GatewayStats.totalErrors.fetch_add(1, std::memory_order_relaxed);
    return "";
}

std::string QueryGatewayAPIRaw(uint64_t botGuid, uint64_t playerGuid,
                               const std::string& systemPrompt,
                               const std::string& userMessage)
{
    static OllamaHttpClient httpClient;
    httpClient.SetTimeout(static_cast<int>(g_GatewayTimeoutSeconds));
    if (!httpClient.IsAvailable())
    {
        LOG_ERROR("server.loading", "[Ollama Chat Gateway Raw] HTTP client not available");
        return "";
    }

    g_GatewayStats.totalRequests.fetch_add(1, std::memory_order_relaxed);

    nlohmann::json messages = nlohmann::json::array();
    if (!systemPrompt.empty())
        messages.push_back({{"role", "system"}, {"content", SanitizeUTF8(systemPrompt)}});
    messages.push_back({{"role", "user"}, {"content", SanitizeUTF8(userMessage)}});

    // Resolve per-bot URL/token/model (mirror of the regular QueryGatewayAPI).
    std::string url   = g_GatewayUrl;
    std::string token = g_GatewayBearerToken;
    std::string model = g_GatewayModel;
    GatewayBotConfig overrideCfg;
    const bool hasOverride = FindGatewayBotConfig(botGuid, overrideCfg);
    if (hasOverride)
    {
        const auto& cfg = overrideCfg;
        if (!cfg.url.empty())         url   = cfg.url;
        if (!cfg.bearerToken.empty()) token = cfg.bearerToken;
        if (!cfg.model.empty())       model = cfg.model;
    }

    nlohmann::json requestData = {
        {"model",    model},
        {"messages", messages},
        {"stream",   false},
        {"user",     g_GatewayUserPrefix + std::to_string(botGuid)},
    };
    std::string requestDataStr = requestData.dump();

    std::vector<std::pair<std::string, std::string>> extraHeaders = {
        {"Authorization",    "Bearer " + token},
        {"x-openclaw-scopes", g_GatewayScopes},
    };
    if (hasOverride)
        for (const auto& kv : overrideCfg.headers)
            extraHeaders.emplace_back(kv.first, kv.second);

    std::string responseBuffer = httpClient.Post(url, requestDataStr, extraHeaders);
    if (responseBuffer.empty())
    {
        g_GatewayStats.totalErrors.fetch_add(1, std::memory_order_relaxed);
        return "";
    }

    try
    {
        nlohmann::json jsonResponse = nlohmann::json::parse(responseBuffer);
        if (jsonResponse.contains("usage") && jsonResponse["usage"].is_object())
        {
            const auto& usage = jsonResponse["usage"];
            g_GatewayStats.totalPromptTokens.fetch_add(usage.value("prompt_tokens", 0ULL), std::memory_order_relaxed);
            g_GatewayStats.totalCompletionTokens.fetch_add(usage.value("completion_tokens", 0ULL), std::memory_order_relaxed);
        }
        if (jsonResponse.contains("choices") && jsonResponse["choices"].is_array() && !jsonResponse["choices"].empty())
        {
            auto& firstChoice = jsonResponse["choices"][0];
            if (firstChoice.contains("message") && firstChoice["message"].contains("content"))
                return firstChoice["message"]["content"].get<std::string>();
        }
    }
    catch (const std::exception& e)
    {
        LOG_ERROR("server.loading", "[Ollama Chat Gateway Raw] JSON parse error: {}", e.what());
    }

    (void)playerGuid;   // keep signature parallel with QueryGatewayAPI even though Raw skips history
    g_GatewayStats.totalErrors.fetch_add(1, std::memory_order_relaxed);
    return "";
}

namespace
{
    // Parse one SSE chunk's JSON payload and extract the OpenAI delta text.
    // Returns "" if no content delta is present in this frame (e.g. role frame, finish_reason).
    std::string ExtractStreamingDelta(const std::string& jsonText)
    {
        try
        {
            auto j = nlohmann::json::parse(jsonText);
            if (!j.contains("choices") || !j["choices"].is_array() || j["choices"].empty())
                return "";
            const auto& choice = j["choices"][0];
            if (!choice.contains("delta") || !choice["delta"].is_object())
                return "";
            const auto& delta = choice["delta"];
            if (!delta.contains("content") || delta["content"].is_null())
                return "";
            return delta["content"].get<std::string>();
        }
        catch (...) { return ""; }
    }

    // Emit accumulated text on sentence boundaries (or at maxChunkChars), keeping any trailing
    // partial sentence in `pending` for the next call. Returns true if onWhisperReady asked to abort.
    bool DrainPending(std::string& pending, size_t maxChunkChars, bool sentenceMode,
                      const std::function<bool(const std::string&)>& onWhisperReady)
    {
        if (pending.empty()) return false;

        if (sentenceMode)
        {
            // Find the last sentence terminator we can flush at.
            size_t lastBoundary = std::string::npos;
            for (size_t i = pending.size(); i > 0; --i)
            {
                char c = pending[i - 1];
                if (c == '.' || c == '!' || c == '?')
                {
                    lastBoundary = i;
                    break;
                }
            }
            if (lastBoundary != std::string::npos)
            {
                std::string out = pending.substr(0, lastBoundary);
                pending.erase(0, lastBoundary);
                // Trim leading space on remainder.
                while (!pending.empty() && std::isspace(static_cast<unsigned char>(pending.front())))
                    pending.erase(pending.begin());
                if (!out.empty() && !onWhisperReady(out))
                    return true;
            }
        }

        // If pending grew past the chunk cap, force-flush via the splitter.
        if (maxChunkChars > 0 && pending.size() >= maxChunkChars)
        {
            std::vector<std::string> chunks = SplitForWhisper(pending, maxChunkChars);
            // Keep the last chunk pending (it might still be growing); emit the rest.
            for (size_t i = 0; i + 1 < chunks.size(); ++i)
            {
                if (!onWhisperReady(chunks[i]))
                    return true;
            }
            pending = chunks.empty() ? std::string{} : chunks.back();
        }

        return false;
    }
}

std::string QueryGatewayAPIStreaming(uint64_t botGuid, uint64_t playerGuid,
                                     const std::string& message,
                                     const std::function<bool(const std::string& chunk)>& onWhisperReady)
{
    static OllamaHttpClient httpClient;
    httpClient.SetTimeout(static_cast<int>(g_GatewayTimeoutSeconds));

    if (!httpClient.IsAvailable())
    {
        LOG_ERROR("server.loading", "[Ollama Chat Gateway] HTTP client not available (streaming)");
        g_GatewayStats.totalErrors.fetch_add(1, std::memory_order_relaxed);
        return "";
    }

    g_GatewayStats.totalRequests.fetch_add(1, std::memory_order_relaxed);
    {
        std::lock_guard<std::mutex> lock(g_GatewayStats.perBotMutex);
        g_GatewayStats.perBotRequests[botGuid] += 1;
        g_GatewayStats.perBotLastRequest[botGuid] = time(nullptr);
    }

    nlohmann::json messages = nlohmann::json::array();
    {
        std::string sysPrompt = BuildGatewaySystemPromptEx(botGuid, playerGuid, message);
        if (!sysPrompt.empty())
            messages.push_back({{"role", "system"}, {"content", SanitizeUTF8(sysPrompt)}});
    }

    bool sendHistory = (g_GatewayMaxHistory > 0) &&
                       !(g_GatewayUseSessionPersistence && g_GatewaySkipHistoryWhenSession);
    if (sendHistory)
    {
        std::lock_guard<std::mutex> lock(g_ConversationHistoryMutex);
        auto botIt = g_BotConversationHistory.find(botGuid);
        if (botIt != g_BotConversationHistory.end())
        {
            auto playerIt = botIt->second.find(playerGuid);
            if (playerIt != botIt->second.end())
            {
                const auto& history = playerIt->second;
                size_t startIdx = (history.size() > g_GatewayMaxHistory)
                                  ? history.size() - g_GatewayMaxHistory : 0;
                for (size_t i = startIdx; i < history.size(); ++i)
                {
                    messages.push_back({{"role", "user"},      {"content", SanitizeUTF8(history[i].first)}});
                    messages.push_back({{"role", "assistant"}, {"content", SanitizeUTF8(history[i].second)}});
                }
            }
        }
    }
    messages.push_back({{"role", "user"}, {"content", SanitizeUTF8(message)}});

    std::string url = g_GatewayUrl, token = g_GatewayBearerToken, model = g_GatewayModel;
    bool hadModelOverride = false;
    GatewayBotConfig overrideCfg;
    const bool hasOverride = FindGatewayBotConfig(botGuid, overrideCfg);
    if (hasOverride)
    {
        if (!overrideCfg.url.empty())         url   = overrideCfg.url;
        if (!overrideCfg.bearerToken.empty()) token = overrideCfg.bearerToken;
        if (!overrideCfg.model.empty())       { model = overrideCfg.model; hadModelOverride = true; }
    }
    if (!hadModelOverride)
        model = ResolveGatewayModel(botGuid, model);

    nlohmann::json requestData = {
        {"model", model},
        {"messages", messages},
        {"stream", true},
        {"user", g_GatewayUserPrefix + std::to_string(botGuid)}
    };
    std::string body = requestData.dump();

    std::vector<std::pair<std::string, std::string>> headers = {
        {"Authorization", "Bearer " + token},
        {"x-openclaw-scopes", g_GatewayScopes}
    };
    if (g_GatewayUseSessionPersistence)
    {
        std::string sk = BuildGatewaySessionKey(botGuid, playerGuid);
        if (!sk.empty()) headers.emplace_back("x-synthiq-session-key", sk);
    }
    if (hasOverride)
        for (const auto& kv : overrideCfg.headers)
            headers.emplace_back(kv.first, kv.second);

    if (g_DebugEnabled)
        LOG_INFO("server.loading", "[Ollama Chat Gateway] Streaming POST {}, model={}", url, model);

    // SSE parser state.
    std::string sseBuffer;     // raw bytes from the wire
    std::string pendingText;   // assistant text awaiting whisper emission
    std::string fullResponse;  // accumulated full text (returned & stored in history)
    bool aborted = false;

    auto handleLine = [&](const std::string& rawLine) {
        // SSE comment / heartbeat
        if (rawLine.empty() || rawLine[0] == ':') return;
        // Only "data:" lines carry payload for OpenAI-style SSE.
        if (rawLine.rfind("data:", 0) != 0) return;
        std::string payload = rawLine.substr(5);
        // Trim leading space.
        while (!payload.empty() && (payload.front() == ' ' || payload.front() == '\t'))
            payload.erase(payload.begin());

        if (payload == "[DONE]") return;

        std::string delta = ExtractStreamingDelta(payload);
        if (delta.empty()) return;

        fullResponse += delta;
        pendingText += delta;

        if (DrainPending(pendingText, g_GatewayMaxWhisperChunkChars,
                         g_GatewayStreamWhisperOnSentence, onWhisperReady))
            aborted = true;
    };

    bool ok = httpClient.PostStream(url, body, headers,
        [&](const char* data, size_t len) -> bool {
            if (aborted) return false;
            sseBuffer.append(data, len);
            // Split on '\n' (we tolerate "\r\n" by stripping trailing \r).
            size_t pos = 0;
            while (true)
            {
                size_t nl = sseBuffer.find('\n', pos);
                if (nl == std::string::npos) break;
                std::string line = sseBuffer.substr(pos, nl - pos);
                if (!line.empty() && line.back() == '\r') line.pop_back();
                handleLine(line);
                if (aborted) return false;
                pos = nl + 1;
            }
            sseBuffer.erase(0, pos);
            return true;
        });

    if (!ok)
    {
        g_GatewayStats.totalErrors.fetch_add(1, std::memory_order_relaxed);
        return "";
    }

    // Flush whatever's left as the final whisper (may be a partial sentence).
    if (!pendingText.empty())
    {
        // Trim leading whitespace.
        while (!pendingText.empty() && std::isspace(static_cast<unsigned char>(pendingText.front())))
            pendingText.erase(pendingText.begin());
        if (!pendingText.empty())
            onWhisperReady(pendingText);
        pendingText.clear();
    }

    if (g_DebugEnabled)
        LOG_INFO("server.loading", "[Ollama Chat Gateway] Streaming complete — {} chars total", fullResponse.size());

    return fullResponse;
}

void ParseGatewayChannelList(const std::string& cfg, std::unordered_set<int>& out, const char* keyName)
{
    out.clear();

    auto addByName = [&out, keyName](std::string name) {
        std::transform(name.begin(), name.end(), name.begin(),
                       [](unsigned char c) { return std::tolower(c); });
        if      (name == "whisper") out.insert(SRC_WHISPER_LOCAL);
        else if (name == "party")   out.insert(SRC_PARTY_LOCAL);
        else if (name == "raid")    out.insert(SRC_RAID_LOCAL);
        else if (name == "guild")   out.insert(SRC_GUILD_LOCAL);
        else if (name == "officer") out.insert(SRC_OFFICER_LOCAL);
        else if (name == "say")     out.insert(SRC_SAY_LOCAL);
        else if (name == "yell")    out.insert(SRC_YELL_LOCAL);
        else if (name == "general" || name == "channel")
                                    out.insert(SRC_GENERAL_LOCAL);
        else if (name == "none")
        {
            // Explicit chat-off sentinel: disables ALL chat-triggered gateway +
            // classifier traffic (robot-first mode). Inserts nothing, on purpose.
            // Needed because an EMPTY AllowedChannels falls back to "whisper"
            // below (back-compat), so empty ≠ off. Escalations and talk_to_leader
            // are unaffected — neither consults the channel allowlist.
        }
        else if (!name.empty())
            LOG_ERROR("server.loading", "[Ollama Chat Gateway] Unknown channel name '{}' in {}", name, keyName);
    };

    if (cfg.empty()) { addByName("whisper"); return; }

    std::stringstream ss(cfg);
    std::string entry;
    while (std::getline(ss, entry, ','))
    {
        size_t a = entry.find_first_not_of(" \t");
        size_t b = entry.find_last_not_of(" \t");
        if (a == std::string::npos) continue;
        addByName(entry.substr(a, b - a + 1));
    }
}

void ParseGatewayAllowedChannels(const std::string& cfg)
{
    ParseGatewayChannelList(cfg, g_GatewayAllowedChannelsSet, "AllowedChannels");
}

bool IsGatewaySourceAllowed(int chatChannelSourceLocal)
{
    return g_GatewayAllowedChannelsSet.count(chatChannelSourceLocal) > 0;
}

bool IsPrivateChannel(int chatChannelSourceLocal)
{
    switch (chatChannelSourceLocal)
    {
        case SRC_WHISPER_LOCAL:
        case SRC_PARTY_LOCAL:
        case SRC_RAID_LOCAL:
        case SRC_GUILD_LOCAL:
        case SRC_OFFICER_LOCAL:
            return true;
        default:
            return false;
    }
}

bool MessageMentionsBot(const std::string& message, const std::string& botName)
{
    if (message.empty() || botName.empty()) return false;
    auto toLower = [](std::string s) {
        std::transform(s.begin(), s.end(), s.begin(),
                       [](unsigned char c) { return std::tolower(c); });
        return s;
    };
    return toLower(message).find(toLower(botName)) != std::string::npos;
}

void WriteGatewayAuditRecord(uint64_t botGuid, uint64_t playerGuid, uint32_t accountId,
                             uint32_t requestChars, uint32_t responseChars,
                             uint32_t promptTokens, uint32_t completionTokens,
                             uint32_t latencyMs, const std::string& sourceChannel, bool error,
                             const char* backend)
{
    if (!g_GatewayEnableAudit) return;

    // Async insert; if the table is missing the error is logged once per row but we don't care.
    // The `backend` column is added by Jev::EnsureAuditBackendColumns at startup; widen the
    // column list only once that is confirmed so an unmigrated DB keeps writing rows.
    if (Jev::AuditColumnsReady())
    {
        CharacterDatabase.Execute(
            "INSERT INTO mod_ollama_chat_gateway_audit "
            "(bot_guid, player_guid, account_id, request_chars, response_chars, prompt_tokens, "
            " completion_tokens, latency_ms, source_channel, error, backend) "
            "VALUES ({}, {}, {}, {}, {}, {}, {}, {}, '{}', {}, {})",
            botGuid, playerGuid, accountId, requestChars, responseChars,
            promptTokens, completionTokens, latencyMs, sourceChannel, error ? 1 : 0,
            backend ? std::string("'") + backend + "'" : std::string("NULL"));
        return;
    }
    CharacterDatabase.Execute(
        "INSERT INTO mod_ollama_chat_gateway_audit "
        "(bot_guid, player_guid, account_id, request_chars, response_chars, prompt_tokens, "
        " completion_tokens, latency_ms, source_channel, error) "
        "VALUES ({}, {}, {}, {}, {}, {}, {}, {}, '{}', {})",
        botGuid, playerGuid, accountId, requestChars, responseChars,
        promptTokens, completionTokens, latencyMs, sourceChannel, error ? 1 : 0);
}

void PruneGatewayAuditRows()
{
    if (g_GatewayAuditRetentionDays == 0) return;

    QueryResult tableExists = CharacterDatabase.Query(
        "SELECT TABLE_NAME FROM information_schema.tables "
        "WHERE table_schema = 'acore_characters' AND table_name = 'mod_ollama_chat_gateway_audit'");
    if (!tableExists) return;

    CharacterDatabase.Execute(
        "DELETE FROM mod_ollama_chat_gateway_audit WHERE ts < (NOW() - INTERVAL {} DAY)",
        g_GatewayAuditRetentionDays);
}

std::vector<GatewayAction> ExtractActionMarkers(std::string& text)
{
    // Match [name:arg] where name and arg are bounded: name is alpha, arg is anything except ']'.
    static const std::regex re(R"(\[([a-zA-Z_]+):([^\]\n]{1,64})\])");

    std::vector<GatewayAction> out;
    std::string cleaned;
    cleaned.reserve(text.size());

    std::sregex_iterator it(text.begin(), text.end(), re), end;
    size_t lastPos = 0;
    for (; it != end; ++it)
    {
        const auto& m = *it;
        cleaned.append(text, lastPos, m.position() - lastPos);
        out.push_back({m[1].str(), m[2].str()});
        lastPos = m.position() + m.length();
    }
    cleaned.append(text, lastPos, std::string::npos);

    // Trim consecutive spaces left from removed markers.
    if (!out.empty())
    {
        std::string compact;
        compact.reserve(cleaned.size());
        bool prevSpace = false;
        for (char c : cleaned)
        {
            bool sp = (c == ' ' || c == '\t');
            if (sp && prevSpace) continue;
            compact.push_back(c);
            prevSpace = sp;
        }
        text.swap(compact);
    }

    return out;
}

bool DispatchGatewayAction(uint64_t botGuid, const GatewayAction& action)
{
    // First-pass implementation: log the action and let downstream consumers (mod-playerbots
    // hooks, future bridges) react. We don't call into Player emote APIs directly here because
    // the relevant entry points vary across AzerothCore branches; binding them belongs in a
    // follow-up that locks down build-time guarantees.
    Player* bot = ObjectAccessor::FindPlayer(ObjectGuid(botGuid));
    LOG_INFO("server.loading", "[Ollama Chat Gateway] Action marker '{}={}' from bot {} (handler stub)",
             action.name, action.arg, bot ? bot->GetName() : std::to_string(botGuid));
    return false;
}

// Tool-using gateway loop. Builds messages with tools[], reads back, dispatches any tool_calls,
// appends results, re-queries. Stops when the model returns plain content or iteration cap is hit.
std::string QueryGatewayAPIWithTools(uint64_t botGuid, uint64_t playerGuid, const std::string& message)
{
    static OllamaHttpClient httpClient;
    httpClient.SetTimeout(static_cast<int>(g_GatewayTimeoutSeconds));

    if (!httpClient.IsAvailable())
    {
        LOG_ERROR("server.loading", "[Ollama Chat Gateway] HTTP client not available (tools)");
        g_GatewayStats.totalErrors.fetch_add(1, std::memory_order_relaxed);
        return "";
    }

    g_GatewayStats.totalRequests.fetch_add(1, std::memory_order_relaxed);
    {
        std::lock_guard<std::mutex> lock(g_GatewayStats.perBotMutex);
        g_GatewayStats.perBotRequests[botGuid] += 1;
        g_GatewayStats.perBotLastRequest[botGuid] = time(nullptr);
    }

    nlohmann::json messages = nlohmann::json::array();
    {
        std::string sysPrompt = BuildGatewaySystemPromptEx(botGuid, playerGuid, message);
        if (!sysPrompt.empty())
            messages.push_back({{"role", "system"}, {"content", SanitizeUTF8(sysPrompt)}});
    }

    bool sendHistory = (g_GatewayMaxHistory > 0) &&
                       !(g_GatewayUseSessionPersistence && g_GatewaySkipHistoryWhenSession);
    if (sendHistory)
    {
        std::lock_guard<std::mutex> lock(g_ConversationHistoryMutex);
        auto botIt = g_BotConversationHistory.find(botGuid);
        if (botIt != g_BotConversationHistory.end())
        {
            auto playerIt = botIt->second.find(playerGuid);
            if (playerIt != botIt->second.end())
            {
                const auto& history = playerIt->second;
                size_t startIdx = (history.size() > g_GatewayMaxHistory)
                                  ? history.size() - g_GatewayMaxHistory : 0;
                for (size_t i = startIdx; i < history.size(); ++i)
                {
                    messages.push_back({{"role", "user"},      {"content", SanitizeUTF8(history[i].first)}});
                    messages.push_back({{"role", "assistant"}, {"content", SanitizeUTF8(history[i].second)}});
                }
            }
        }
    }
    messages.push_back({{"role", "user"}, {"content", SanitizeUTF8(message)}});

    std::string url = g_GatewayUrl, token = g_GatewayBearerToken, model = g_GatewayModel;
    bool hadModelOverride = false;
    GatewayBotConfig overrideCfg;
    const bool hasOverride = FindGatewayBotConfig(botGuid, overrideCfg);
    if (hasOverride)
    {
        if (!overrideCfg.url.empty())         url   = overrideCfg.url;
        if (!overrideCfg.bearerToken.empty()) token = overrideCfg.bearerToken;
        if (!overrideCfg.model.empty())       { model = overrideCfg.model; hadModelOverride = true; }
    }
    if (!hadModelOverride)
        model = ResolveGatewayModel(botGuid, model);

    std::vector<std::pair<std::string, std::string>> headers = {
        {"Authorization", "Bearer " + token},
        {"x-openclaw-scopes", g_GatewayScopes}
    };
    if (g_GatewayUseSessionPersistence)
    {
        std::string sk = BuildGatewaySessionKey(botGuid, playerGuid);
        if (!sk.empty()) headers.emplace_back("x-synthiq-session-key", sk);
    }
    if (hasOverride)
        for (const auto& kv : overrideCfg.headers)
            headers.emplace_back(kv.first, kv.second);

    // Pass botGuid so out-of-scope families (ops_* with the sidecar disabled,
    // leader_* for a non-leader bot) are dropped before the array is billed to
    // the upstream model — their handlers would only return an error for this bot.
    nlohmann::json toolsArr = BuildOpenAiToolsArray(ResolveAllowedTools(botGuid), botGuid);

    for (uint32_t iter = 0; iter <= g_GatewayMaxToolIterations; ++iter)
    {
        nlohmann::json requestData = {
            {"model", model},
            {"messages", messages},
            {"stream", false},
            {"user", g_GatewayUserPrefix + std::to_string(botGuid)}
        };
        if (!toolsArr.empty())
        {
            requestData["tools"] = toolsArr;
            requestData["tool_choice"] = "auto";
        }

        std::string body = requestData.dump();
        std::string resp = httpClient.Post(url, body, headers);
        if (resp.empty())
        {
            g_GatewayStats.totalErrors.fetch_add(1, std::memory_order_relaxed);
            return "";
        }

        nlohmann::json j;
        try { j = nlohmann::json::parse(resp); }
        catch (const std::exception& e)
        {
            LOG_ERROR("server.loading", "[Ollama Chat Gateway] Tool loop JSON parse: {}", e.what());
            g_GatewayStats.totalErrors.fetch_add(1, std::memory_order_relaxed);
            return "";
        }

        // Token accounting.
        if (j.contains("usage") && j["usage"].is_object())
        {
            uint64_t pTok = j["usage"].value("prompt_tokens", 0ULL);
            uint64_t cTok = j["usage"].value("completion_tokens", 0ULL);
            g_GatewayStats.totalPromptTokens.fetch_add(pTok, std::memory_order_relaxed);
            g_GatewayStats.totalCompletionTokens.fetch_add(cTok, std::memory_order_relaxed);
        }

        if (!j.contains("choices") || !j["choices"].is_array() || j["choices"].empty())
        {
            LOG_ERROR("server.loading", "[Ollama Chat Gateway] Tool loop: no choices in response");
            g_GatewayStats.totalErrors.fetch_add(1, std::memory_order_relaxed);
            return "";
        }
        const auto& choice = j["choices"][0];
        const auto& msg    = choice.contains("message") ? choice["message"] : nlohmann::json::object();

        // If the model called tools, dispatch each and loop.
        if (msg.contains("tool_calls") && msg["tool_calls"].is_array() && !msg["tool_calls"].empty())
        {
            // Append the assistant message verbatim (must contain tool_calls for the model to track them).
            messages.push_back(msg);

            for (const auto& tc : msg["tool_calls"])
            {
                std::string callId = tc.value("id", std::string{});
                std::string fnName;
                nlohmann::json args = nlohmann::json::object();
                if (tc.contains("function") && tc["function"].is_object())
                {
                    fnName = tc["function"].value("name", std::string{});
                    std::string argsRaw = tc["function"].value("arguments", std::string{"{}"});
                    try { args = nlohmann::json::parse(argsRaw); }
                    catch (...) { args = nlohmann::json::object(); }
                }

                nlohmann::json result = DispatchGatewayTool(botGuid, playerGuid, fnName, args);
                if (g_DebugEnabled)
                    LOG_INFO("server.loading", "[Ollama Chat Gateway] Tool '{}' → {}", fnName, result.dump());

                messages.push_back({
                    {"role", "tool"},
                    {"tool_call_id", callId},
                    {"name", fnName},
                    {"content", result.dump()}
                });
            }
            continue; // re-query with tool results in context
        }

        // Otherwise we have the final answer.
        std::string content;
        if (msg.contains("content") && msg["content"].is_string())
            content = msg["content"].get<std::string>();

        if (g_DebugEnabled)
            LOG_INFO("server.loading", "[Ollama Chat Gateway] Tool loop done after {} iter(s), {} chars",
                     iter, content.size());
        return content;
    }

    LOG_WARN("server.loading", "[Ollama Chat Gateway] Tool loop hit MaxToolIterations ({}) without final answer",
             g_GatewayMaxToolIterations);
    g_GatewayStats.totalErrors.fetch_add(1, std::memory_order_relaxed);
    return "";
}

void ParseGatewayBotOverridesJson(const std::string& jsonStr)
{
    g_GatewayBotConfigs.clear();
    if (jsonStr.empty()) return;

    nlohmann::json arr;
    try { arr = nlohmann::json::parse(jsonStr); }
    catch (const std::exception& e)
    {
        LOG_ERROR("server.loading", "[Ollama Chat Gateway] BotOverridesJson parse error: {}", e.what());
        return;
    }

    if (!arr.is_array())
    {
        LOG_ERROR("server.loading", "[Ollama Chat Gateway] BotOverridesJson must be a JSON array");
        return;
    }

    for (const auto& entry : arr)
    {
        if (!entry.is_object()) continue;
        if (!entry.contains("guid")) { LOG_ERROR("server.loading", "[Ollama Chat Gateway] override missing 'guid'"); continue; }

        uint64_t guid = entry["guid"].get<uint64_t>();
        GatewayBotConfig cfg;
        cfg.url         = entry.value("url", std::string{});
        cfg.bearerToken = entry.value("token", std::string{});
        cfg.model       = entry.value("model", std::string{});
        cfg.gatewayType = entry.value("gatewayType", std::string{});

        if (entry.contains("headers") && entry["headers"].is_object())
        {
            for (auto it = entry["headers"].begin(); it != entry["headers"].end(); ++it)
                if (it.value().is_string())
                    cfg.headers[it.key()] = it.value().get<std::string>();
        }

        if (entry.contains("tools") && entry["tools"].is_array())
        {
            for (const auto& t : entry["tools"])
                if (t.is_string()) cfg.allowedTools.push_back(t.get<std::string>());
        }

        g_GatewayBotConfigs[guid] = cfg;
        g_GatewayBotGUIDSet.insert(guid);

        LOG_INFO("server.loading", "[Ollama Chat Gateway] BotOverridesJson: GUID={}, URL={}, Model={}, headers={}, tools={}",
                 guid, cfg.url, cfg.model, cfg.headers.size(), cfg.allowedTools.size());
    }
}

void ParseGatewayBotOverrides(const std::string& overrideList)
{
    g_GatewayBotConfigs.clear();
    if (overrideList.empty())
        return;

    // Format: "GUID:URL:TOKEN:MODEL|GUID:URL:TOKEN:MODEL"
    std::stringstream ss(overrideList);
    std::string entry;
    while (std::getline(ss, entry, '|'))
    {
        // Trim
        size_t s = entry.find_first_not_of(" \t");
        size_t e = entry.find_last_not_of(" \t");
        if (s == std::string::npos) continue;
        entry = entry.substr(s, e - s + 1);

        // Split by ':'
        std::vector<std::string> parts;
        std::stringstream ps(entry);
        std::string part;
        while (std::getline(ps, part, ':'))
            parts.push_back(part);

        if (parts.size() < 4)
        {
            LOG_ERROR("server.loading", "[Ollama Chat Gateway] Invalid bot override (need GUID:URL:TOKEN:MODEL): '{}'", entry);
            continue;
        }

        try
        {
            uint64_t guid = std::stoull(parts[0]);

            // Reassemble URL from parts[1]..parts[N-3] since "http://host:port" splits on ':'.
            // Format is GUID:SCHEME://HOST:PORT/PATH:TOKEN:MODEL — token & model are the last two.
            std::string token = parts[parts.size() - 2];
            std::string model = parts[parts.size() - 1];

            std::string url;
            for (size_t i = 1; i < parts.size() - 2; ++i)
            {
                if (!url.empty()) url += ":";
                url += parts[i];
            }

            GatewayBotConfig cfg;
            cfg.url = url;
            cfg.bearerToken = token;
            cfg.model = model;
            g_GatewayBotConfigs[guid] = cfg;

            // Also add to GUID set
            g_GatewayBotGUIDSet.insert(guid);

            LOG_INFO("server.loading", "[Ollama Chat Gateway] Bot override: GUID={}, URL={}, Model={}", guid, url, model);
        }
        catch (const std::exception& ex)
        {
            LOG_ERROR("server.loading", "[Ollama Chat Gateway] Failed to parse bot override '{}': {}", entry, ex.what());
        }
    }
}

void ParseGatewayBotGUIDs(const std::string& guidList)
{
    g_GatewayBotGUIDSet.clear();
    if (guidList.empty())
        return;

    std::stringstream ss(guidList);
    std::string token;
    while (std::getline(ss, token, ','))
    {
        // Trim whitespace
        size_t start = token.find_first_not_of(" \t");
        size_t end = token.find_last_not_of(" \t");
        if (start == std::string::npos)
            continue;
        token = token.substr(start, end - start + 1);

        try
        {
            uint64_t guid = std::stoull(token);
            g_GatewayBotGUIDSet.insert(guid);
        }
        catch (const std::exception& e)
        {
            LOG_ERROR("server.loading", "[Ollama Chat Gateway] Invalid GUID in Gateway.BotGUIDs: '{}' — {}", token, e.what());
        }
    }
}

bool IsAccountAllowedForGateway(uint32_t accountId)
{
    if (g_GatewayWhitelistAccountIds.empty())
        return true; // Empty whitelist = allow all
    return g_GatewayWhitelistAccountIds.count(accountId) > 0;
}

void ParseGatewayWhitelist(const std::string& list)
{
    g_GatewayWhitelistAccountIds.clear();
    if (list.empty())
        return;

    std::stringstream ss(list);
    std::string token;
    while (std::getline(ss, token, ','))
    {
        size_t start = token.find_first_not_of(" \t");
        size_t end = token.find_last_not_of(" \t");
        if (start == std::string::npos)
            continue;
        token = token.substr(start, end - start + 1);

        try
        {
            uint32_t accountId = static_cast<uint32_t>(std::stoul(token));
            g_GatewayWhitelistAccountIds.insert(accountId);
        }
        catch (const std::exception& e)
        {
            LOG_ERROR("server.loading", "[Ollama Chat Gateway] Invalid account ID in Gateway.WhitelistAccountIds: '{}' — {}", token, e.what());
        }
    }
}
