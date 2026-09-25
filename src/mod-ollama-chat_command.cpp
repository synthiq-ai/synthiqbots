#include "mod-ollama-chat_command.h"
#include "mod-ollama-chat_config.h"
#include "mod-ollama-chat_sentiment.h"
#include "mod-ollama-chat_personality.h"
#include "mod-ollama-chat_gateway.h"
#include "mod-ollama-chat_promotion.h"
#include "mod-ollama-chat_playerprefs.h"
#include "mod-ollama-chat_mcpserver.h"
#include "mod-ollama-chat_tactical.h"
#include "mod-ollama-chat_jev.h"
#include "Chat.h"
#include "Config.h"
#include "ObjectAccessor.h"
#include "Player.h"
#include "PlayerbotMgr.h"
#include <fmt/core.h>
#include <algorithm>
#include <ctime>

using namespace Acore::ChatCommands;

OllamaChatConfigCommand::OllamaChatConfigCommand()
    : CommandScript("OllamaChatConfigCommand")
{
}

ChatCommandTable OllamaChatConfigCommand::GetCommands() const
{
    static ChatCommandTable ollamaSentimentCommandTable =
    {
        { "view",  HandleOllamaSentimentViewCommand,  SEC_ADMINISTRATOR, Console::Yes },
        { "set",   HandleOllamaSentimentSetCommand,   SEC_ADMINISTRATOR, Console::Yes },
        { "reset", HandleOllamaSentimentResetCommand, SEC_ADMINISTRATOR, Console::Yes }
    };

    static ChatCommandTable ollamaPersonalityCommandTable =
    {
        { "get",  HandleOllamaPersonalityGetCommand,  SEC_ADMINISTRATOR, Console::Yes },
        { "set",  HandleOllamaPersonalitySetCommand,  SEC_ADMINISTRATOR, Console::Yes },
        { "list", HandleOllamaPersonalityListCommand, SEC_ADMINISTRATOR, Console::Yes }
    };

    static ChatCommandTable ollamaGatewayCommandTable =
    {
        { "status", HandleOllamaGatewayStatusCommand, SEC_ADMINISTRATOR, Console::Yes },
        { "test",   HandleOllamaGatewayTestCommand,   SEC_ADMINISTRATOR, Console::Yes },
        { "costs",  HandleOllamaGatewayCostsCommand,  SEC_ADMINISTRATOR, Console::Yes },
        { "prune",  HandleOllamaGatewayPruneCommand,  SEC_ADMINISTRATOR, Console::Yes }
    };

    static ChatCommandTable ollamaTacticalCommandTable =
    {
        { "status", HandleOllamaTacticalStatusCommand, SEC_ADMINISTRATOR, Console::Yes }
    };

    static ChatCommandTable ollamaReloadCommandTable =
    {
        { "reload",      HandleOllamaReloadCommand,         SEC_ADMINISTRATOR, Console::Yes },
        { "sentiment",   ollamaSentimentCommandTable },
        { "personality", ollamaPersonalityCommandTable },
        { "gateway",     ollamaGatewayCommandTable },
        { "tactical",    ollamaTacticalCommandTable },
        { "optout",      HandleOllamaOptOutCommand,         SEC_PLAYER,        Console::No },
        { "optin",       HandleOllamaOptInCommand,          SEC_PLAYER,        Console::No },
        { "mute",        HandleOllamaMuteCommand,           SEC_PLAYER,        Console::No },
        { "unmute",      HandleOllamaUnmuteCommand,         SEC_PLAYER,        Console::No }
    };

    static ChatCommandTable commandTable =
    {
        { "ollama", ollamaReloadCommandTable }
    };

    return commandTable;
}

bool OllamaChatConfigCommand::HandleOllamaReloadCommand(ChatHandler* handler)
{
    ReloadOllamaChatRuntime();
    handler->SendSysMessage("OllamaChat: Configuration reloaded from conf!");
    return true;
}

// --- Player-facing commands (SEC_PLAYER) ---

bool OllamaChatConfigCommand::HandleOllamaOptOutCommand(ChatHandler* handler)
{
    Player* p = handler->GetPlayer();
    if (!p) { handler->SendSysMessage("OllamaChat: this command is for in-game use."); return true; }
    uint32_t aid = p->GetSession() ? p->GetSession()->GetAccountId() : 0;
    if (aid == 0) { handler->SendSysMessage("OllamaChat: could not resolve your account."); return true; }
    SetAccountOptOut(aid, true);
    handler->SendSysMessage("OllamaChat: you are now opted out of all bot responses. Use .ollama optin to re-enable.");
    return true;
}

bool OllamaChatConfigCommand::HandleOllamaOptInCommand(ChatHandler* handler)
{
    Player* p = handler->GetPlayer();
    if (!p) return true;
    uint32_t aid = p->GetSession() ? p->GetSession()->GetAccountId() : 0;
    if (aid == 0) return true;
    SetAccountOptOut(aid, false);
    handler->SendSysMessage("OllamaChat: opt-in successful. Bots may respond to you again.");
    return true;
}

bool OllamaChatConfigCommand::HandleOllamaMuteCommand(ChatHandler* handler, std::string botName)
{
    Player* p = handler->GetPlayer();
    if (!p) return true;
    uint32_t aid = p->GetSession() ? p->GetSession()->GetAccountId() : 0;
    if (aid == 0) return true;

    uint64_t botGuid = LookupBotGuidByName(botName);
    if (botGuid == 0)
    {
        handler->SendSysMessage(fmt::format("OllamaChat: no character named '{}' found.", botName));
        return true;
    }
    SetBotMute(aid, botGuid, true);
    handler->SendSysMessage(fmt::format("OllamaChat: muted bot '{}' for your account. Use .ollama unmute {} to undo.", botName, botName));
    return true;
}

bool OllamaChatConfigCommand::HandleOllamaUnmuteCommand(ChatHandler* handler, std::string botName)
{
    Player* p = handler->GetPlayer();
    if (!p) return true;
    uint32_t aid = p->GetSession() ? p->GetSession()->GetAccountId() : 0;
    if (aid == 0) return true;

    uint64_t botGuid = LookupBotGuidByName(botName);
    if (botGuid == 0)
    {
        handler->SendSysMessage(fmt::format("OllamaChat: no character named '{}' found.", botName));
        return true;
    }
    SetBotMute(aid, botGuid, false);
    handler->SendSysMessage(fmt::format("OllamaChat: unmuted bot '{}'.", botName));
    return true;
}

bool OllamaChatConfigCommand::HandleOllamaGatewayTestCommand(ChatHandler* handler, std::string botName, Tail prompt)
{
    Player* bot = ObjectAccessor::FindPlayerByName(botName);
    if (!bot) { handler->SendSysMessage(fmt::format("OllamaChat: bot '{}' not online.", botName)); return true; }
    uint64_t botGuid = bot->GetGUID().GetRawValue();
    if (!IsGatewayBot(botGuid))
    {
        handler->SendSysMessage(fmt::format("OllamaChat: '{}' is not configured as a gateway bot.", botName));
        return true;
    }
    Player* gm = handler->GetPlayer();
    uint64_t gmGuid = gm ? gm->GetGUID().GetRawValue() : 0;

    std::string promptStr(prompt);
    handler->SendSysMessage(fmt::format("OllamaChat: dispatching test prompt to '{}' (async, see log)…", botName));

    std::thread([botGuid, gmGuid, promptStr]() {
        std::string resp = QueryGatewayAPI(botGuid, gmGuid, promptStr);
        if (resp.empty())
            LOG_INFO("server.loading", "[Ollama Chat Gateway TEST] empty/error response (bot {}, prompt: '{}')", botGuid, promptStr);
        else
            LOG_INFO("server.loading", "[Ollama Chat Gateway TEST] bot {} → {}", botGuid, resp);
    }).detach();
    return true;
}

bool OllamaChatConfigCommand::HandleOllamaGatewayCostsCommand(ChatHandler* handler, Optional<uint32> hours)
{
    QueryResult tableExists = CharacterDatabase.Query(
        "SELECT TABLE_NAME FROM information_schema.tables "
        "WHERE table_schema = 'acore_characters' AND table_name = 'mod_ollama_chat_gateway_audit'");
    if (!tableExists)
    {
        handler->SendSysMessage("OllamaChat: audit table missing (source 2026_04_18_gateway_audit.sql).");
        return true;
    }

    uint32 windowHours = hours.value_or(24);
    QueryResult totals = CharacterDatabase.Query(
        "SELECT COUNT(*), SUM(error), SUM(latency_ms), SUM(request_chars), SUM(response_chars) "
        "FROM mod_ollama_chat_gateway_audit "
        "WHERE ts >= NOW() - INTERVAL {} HOUR", windowHours);

    if (totals)
    {
        uint64 reqs   = (*totals)[0].Get<uint64>();
        uint64 errs   = (*totals)[1].Get<uint64>();
        uint64 lat    = (*totals)[2].Get<uint64>();
        uint64 reqCh  = (*totals)[3].Get<uint64>();
        uint64 respCh = (*totals)[4].Get<uint64>();
        handler->SendSysMessage(fmt::format(
            "OllamaChat Gateway last {}h: {} requests ({} errors), avg latency {} ms, chars req/resp {}/{}",
            windowHours, reqs, errs,
            reqs ? lat / reqs : 0, reqCh, respCh));
    }

    QueryResult perBot = CharacterDatabase.Query(
        "SELECT bot_guid, COUNT(*), SUM(response_chars) "
        "FROM mod_ollama_chat_gateway_audit "
        "WHERE ts >= NOW() - INTERVAL {} HOUR "
        "GROUP BY bot_guid ORDER BY COUNT(*) DESC LIMIT 10", windowHours);
    if (!perBot)
    {
        handler->SendSysMessage("  (no rows in window)");
        return true;
    }

    handler->SendSysMessage("  per-bot (top 10 by count):");
    do {
        uint64 guid   = (*perBot)[0].Get<uint64>();
        uint64 count  = (*perBot)[1].Get<uint64>();
        uint64 chars  = (*perBot)[2].Get<uint64>();
        Player* bp = ObjectAccessor::FindPlayer(ObjectGuid(guid));
        std::string name = bp ? bp->GetName() : ("guid=" + std::to_string(guid));
        handler->SendSysMessage(fmt::format("    {} — {} requests, {} response chars", name, count, chars));
    } while (perBot->NextRow());

    return true;
}

bool OllamaChatConfigCommand::HandleOllamaGatewayPruneCommand(ChatHandler* handler)
{
    PruneGatewayAuditRows();
    handler->SendSysMessage(fmt::format(
        "OllamaChat: pruned audit rows older than {} days.", g_GatewayAuditRetentionDays));
    return true;
}

bool OllamaChatConfigCommand::HandleOllamaGatewayStatusCommand(ChatHandler* handler)
{
    handler->SendSysMessage(fmt::format(
        "OllamaChat Gateway: enabled={}, model='{}', keyword='{}', timeout={}s, legacyFallbackOnError(no-op)={}, useSessions={}",
        g_GatewayEnable, g_GatewayModel, g_GatewayTriggerKeyword,
        g_GatewayTimeoutSeconds, g_GatewayFallbackOnError, g_GatewayUseSessionPersistence));

    handler->SendSysMessage(fmt::format(
        "  bots configured={} (overrides={}), whitelist={}, concurrencyCap={}, cooldown={}s",
        g_GatewayBotGUIDSet.size(), g_GatewayBotConfigs.size(),
        g_GatewayWhitelistAccountIds.empty() ? "OPEN (all accounts)" : std::to_string(g_GatewayWhitelistAccountIds.size()) + " accounts",
        g_GatewayMaxConcurrentRequests, g_GatewayMinSecondsBetweenRequests));
    handler->SendSysMessage("  " + OllamaChat::Promotion::StatusLine());

    uint64_t reqs       = g_GatewayStats.totalRequests.load(std::memory_order_relaxed);
    uint64_t errs       = g_GatewayStats.totalErrors.load(std::memory_order_relaxed);
    uint64_t rateLim    = g_GatewayStats.totalRateLimited.load(std::memory_order_relaxed);
    uint64_t fallbacks  = g_GatewayStats.totalFallbacks.load(std::memory_order_relaxed);
    uint64_t pTok       = g_GatewayStats.totalPromptTokens.load(std::memory_order_relaxed);
    uint64_t cTok       = g_GatewayStats.totalCompletionTokens.load(std::memory_order_relaxed);

    handler->SendSysMessage(fmt::format(
        "  totals: requests={}, errors={}, rate-limited={}, legacyFallbacks={}, tokens prompt/completion={}/{}",
        reqs, errs, rateLim, fallbacks, pTok, cTok));

    // PR8a: include MCP server stats in the same status block.
    auto& mcp = GatewayMcpServer::Instance();
    handler->SendSysMessage(fmt::format(
        "  mcp: enabled={}, running={}, bind={}:{}, requests={}, tool-calls={}, unauthorized={}",
        g_McpEnable, mcp.IsRunning(), g_McpBindAddress, g_McpPort,
        mcp.TotalRequests(), mcp.TotalToolCalls(), mcp.TotalUnauthorized()));

    // Per-tool usage/latency so we can see which tools are hot and which are failing.
    // Top 8 by calls; error details (truncated) printed on the second line when present.
    auto toolStats = mcp.ToolStatsSnapshot();
    if (toolStats.empty())
    {
        handler->SendSysMessage("  mcp-tools: (no tool calls since startup)");
    }
    else
    {
        handler->SendSysMessage(fmt::format("  mcp-tools: top {} of {} by calls",
                                            std::min<size_t>(8, toolStats.size()), toolStats.size()));
        size_t shown = 0;
        for (const auto& t : toolStats)
        {
            if (shown++ >= 8) break;
            handler->SendSysMessage(fmt::format(
                "    {} — calls={}, errors={}, avg={}ms, max={}ms",
                t.name, t.calls, t.errors, t.avgMs, t.maxMs));
            if (!t.lastError.empty())
                handler->SendSysMessage(fmt::format("      lastErr: {}", t.lastError));
        }
    }

    std::lock_guard<std::mutex> lock(g_GatewayStats.perBotMutex);
    if (g_GatewayStats.perBotRequests.empty())
    {
        handler->SendSysMessage("  per-bot: (no requests since last reload)");
        return true;
    }

    handler->SendSysMessage("  per-bot:");
    time_t now = time(nullptr);
    for (const auto& [guid, count] : g_GatewayStats.perBotRequests)
    {
        Player* bot = ObjectAccessor::FindPlayer(ObjectGuid(guid));
        std::string botName = bot ? bot->GetName() : ("guid=" + std::to_string(guid));

        std::string lastSeen = "never";
        auto lastIt = g_GatewayStats.perBotLastRequest.find(guid);
        if (lastIt != g_GatewayStats.perBotLastRequest.end())
            lastSeen = std::to_string(static_cast<long long>(now - lastIt->second)) + "s ago";

        handler->SendSysMessage(fmt::format("    {} — requests={}, last={}", botName, count, lastSeen));
    }
    return true;
}

bool OllamaChatConfigCommand::HandleOllamaSentimentViewCommand(ChatHandler* handler, Optional<std::string> botName, Optional<std::string> playerName)
{
    if (!g_EnableSentimentTracking)
    {
        handler->SendSysMessage("OllamaChat: Sentiment tracking is disabled.");
        return true;
    }

    if (!botName && !playerName)
    {
        // Show all sentiment data
        std::lock_guard<std::mutex> lock(g_SentimentMutex);
        if (g_BotPlayerSentiments.empty())
        {
            handler->SendSysMessage("OllamaChat: No sentiment data found.");
            return true;
        }

        handler->SendSysMessage("OllamaChat: All sentiment data:");
        for (const auto& [botGuid, playerMap] : g_BotPlayerSentiments)
        {
            Player* bot = ObjectAccessor::FindPlayer(ObjectGuid(botGuid));
            std::string botNameStr = bot ? bot->GetName() : std::to_string(botGuid);
            
            for (const auto& [playerGuid, sentiment] : playerMap)
            {
                Player* player = ObjectAccessor::FindPlayer(ObjectGuid(playerGuid));
                std::string playerNameStr = player ? player->GetName() : std::to_string(playerGuid);
                
                handler->SendSysMessage(fmt::format("  Bot '{}' -> Player '{}': {:.3f}", 
                                        botNameStr, playerNameStr, sentiment));
            }
        }
        return true;
    }

    // Find specific bot or player
    Player* targetBot = nullptr;
    Player* targetPlayer = nullptr;

    if (botName)
    {
        targetBot = ObjectAccessor::FindPlayerByName(*botName);
        if (!targetBot)
        {
            handler->SendSysMessage(fmt::format("OllamaChat: Bot '{}' not found.", *botName));
            return true;
        }
        if (!PlayerbotsMgr::instance().GetPlayerbotAI(targetBot))
        {
            handler->SendSysMessage(fmt::format("OllamaChat: Player '{}' is not a bot.", *botName));
            return true;
        }
    }

    if (playerName)
    {
        targetPlayer = ObjectAccessor::FindPlayerByName(*playerName);
        if (!targetPlayer)
        {
            handler->SendSysMessage(fmt::format("OllamaChat: Player '{}' not found.", *playerName));
            return true;
        }
    }

    // Show sentiment for specific bot-player pair or all pairs involving a specific bot/player
    if (targetBot && targetPlayer)
    {
        float sentiment = GetBotPlayerSentiment(targetBot->GetGUID().GetRawValue(), targetPlayer->GetGUID().GetRawValue());
        handler->SendSysMessage(fmt::format("OllamaChat: Bot '{}' -> Player '{}': {:.3f}", 
                                targetBot->GetName(), targetPlayer->GetName(), sentiment));
    }
    else if (targetBot)
    {
        // Show all sentiments for this bot
        uint64_t botGuid = targetBot->GetGUID().GetRawValue();
        std::lock_guard<std::mutex> lock(g_SentimentMutex);
        
        auto botIt = g_BotPlayerSentiments.find(botGuid);
        if (botIt == g_BotPlayerSentiments.end() || botIt->second.empty())
        {
            handler->SendSysMessage(fmt::format("OllamaChat: No sentiment data found for bot '{}'.", targetBot->GetName()));
            return true;
        }

        handler->SendSysMessage(fmt::format("OllamaChat: Sentiment data for bot '{}':", targetBot->GetName()));
        for (const auto& [playerGuid, sentiment] : botIt->second)
        {
            Player* player = ObjectAccessor::FindPlayer(ObjectGuid(playerGuid));
            std::string playerNameStr = player ? player->GetName() : std::to_string(playerGuid);
            handler->SendSysMessage(fmt::format("  -> Player '{}': {:.3f}", playerNameStr, sentiment));
        }
    }
    else if (targetPlayer)
    {
        // Show all sentiments involving this player
        uint64_t playerGuid = targetPlayer->GetGUID().GetRawValue();
        std::lock_guard<std::mutex> lock(g_SentimentMutex);
        
        bool found = false;
        handler->SendSysMessage(fmt::format("OllamaChat: Sentiment data involving player '{}':", targetPlayer->GetName()));
        
        for (const auto& [botGuid, playerMap] : g_BotPlayerSentiments)
        {
            auto playerIt = playerMap.find(playerGuid);
            if (playerIt != playerMap.end())
            {
                Player* bot = ObjectAccessor::FindPlayer(ObjectGuid(botGuid));
                std::string botNameStr = bot ? bot->GetName() : std::to_string(botGuid);
                handler->SendSysMessage(fmt::format("  Bot '{}' -> {:.3f}", botNameStr, playerIt->second));
                found = true;
            }
        }
        
        if (!found)
        {
            handler->SendSysMessage(fmt::format("OllamaChat: No sentiment data found involving player '{}'.", targetPlayer->GetName()));
        }
    }

    return true;
}

bool OllamaChatConfigCommand::HandleOllamaSentimentSetCommand(ChatHandler* handler, std::string botName, std::string playerName, float sentimentValue)
{
    if (!g_EnableSentimentTracking)
    {
        handler->SendSysMessage("OllamaChat: Sentiment tracking is disabled.");
        return true;
    }

    Player* bot = ObjectAccessor::FindPlayerByName(botName);
    if (!bot)
    {
        handler->SendSysMessage(fmt::format("OllamaChat: Bot '{}' not found.", botName));
        return true;
    }
    if (!PlayerbotsMgr::instance().GetPlayerbotAI(bot))
    {
        handler->SendSysMessage(fmt::format("OllamaChat: Player '{}' is not a bot.", botName));
        return true;
    }

    Player* player = ObjectAccessor::FindPlayerByName(playerName);
    if (!player)
    {
        handler->SendSysMessage(fmt::format("OllamaChat: Player '{}' not found.", playerName));
        return true;
    }

    if (sentimentValue < 0.0f || sentimentValue > 1.0f)
    {
        handler->SendSysMessage("OllamaChat: Sentiment value must be between 0.0 and 1.0.");
        return true;
    }

    SetBotPlayerSentiment(bot->GetGUID().GetRawValue(), player->GetGUID().GetRawValue(), sentimentValue);
    handler->SendSysMessage(fmt::format("OllamaChat: Set sentiment between bot '{}' and player '{}' to {:.3f}.", 
                            botName, playerName, sentimentValue));
    return true;
}

bool OllamaChatConfigCommand::HandleOllamaSentimentResetCommand(ChatHandler* handler, Optional<std::string> botName, Optional<std::string> playerName)
{
    if (!g_EnableSentimentTracking)
    {
        handler->SendSysMessage("OllamaChat: Sentiment tracking is disabled.");
        return true;
    }

    if (!botName && !playerName)
    {
        // Reset all sentiment data
        std::lock_guard<std::mutex> lock(g_SentimentMutex);
        uint32_t count = 0;
        for (const auto& [botGuid, playerMap] : g_BotPlayerSentiments)
        {
            count += playerMap.size();
        }
        g_BotPlayerSentiments.clear();
        handler->SendSysMessage(fmt::format("OllamaChat: Reset all sentiment data ({} records).", count));
        return true;
    }

    Player* targetBot = nullptr;
    Player* targetPlayer = nullptr;

    if (botName)
    {
        targetBot = ObjectAccessor::FindPlayerByName(*botName);
        if (!targetBot)
        {
            handler->SendSysMessage(fmt::format("OllamaChat: Bot '{}' not found.", *botName));
            return true;
        }
        if (!PlayerbotsMgr::instance().GetPlayerbotAI(targetBot))
        {
            handler->SendSysMessage(fmt::format("OllamaChat: Player '{}' is not a bot.", *botName));
            return true;
        }
    }

    if (playerName)
    {
        targetPlayer = ObjectAccessor::FindPlayerByName(*playerName);
        if (!targetPlayer)
        {
            handler->SendSysMessage(fmt::format("OllamaChat: Player '{}' not found.", *playerName));
            return true;
        }
    }

    if (targetBot && targetPlayer)
    {
        // Reset specific bot-player sentiment
        SetBotPlayerSentiment(targetBot->GetGUID().GetRawValue(), targetPlayer->GetGUID().GetRawValue(), g_SentimentDefaultValue);
        handler->SendSysMessage(fmt::format("OllamaChat: Reset sentiment between bot '{}' and player '{}' to default ({:.3f}).", 
                                targetBot->GetName(), targetPlayer->GetName(), g_SentimentDefaultValue));
    }
    else if (targetBot)
    {
        // Reset all sentiments for this bot
        uint64_t botGuid = targetBot->GetGUID().GetRawValue();
        std::lock_guard<std::mutex> lock(g_SentimentMutex);
        
        auto botIt = g_BotPlayerSentiments.find(botGuid);
        if (botIt != g_BotPlayerSentiments.end())
        {
            uint32_t count = botIt->second.size();
            g_BotPlayerSentiments.erase(botIt);
            handler->SendSysMessage(fmt::format("OllamaChat: Reset all sentiment data for bot '{}' ({} records).", 
                                    targetBot->GetName(), count));
        }
        else
        {
            handler->SendSysMessage(fmt::format("OllamaChat: No sentiment data found for bot '{}'.", targetBot->GetName()));
        }
    }
    else if (targetPlayer)
    {
        // Reset all sentiments involving this player
        uint64_t playerGuid = targetPlayer->GetGUID().GetRawValue();
        std::lock_guard<std::mutex> lock(g_SentimentMutex);
        
        uint32_t count = 0;
        for (auto& [botGuid, playerMap] : g_BotPlayerSentiments)
        {
            auto playerIt = playerMap.find(playerGuid);
            if (playerIt != playerMap.end())
            {
                playerMap.erase(playerIt);
                count++;
            }
        }
        
        handler->SendSysMessage(fmt::format("OllamaChat: Reset all sentiment data involving player '{}' ({} records).", 
                                targetPlayer->GetName(), count));
    }

    return true;
}

bool OllamaChatConfigCommand::HandleOllamaPersonalityGetCommand(ChatHandler* handler, std::string botName)
{
    Player* bot = ObjectAccessor::FindPlayerByName(botName);
    if (!bot)
    {
        handler->SendSysMessage(fmt::format("OllamaChat: Bot '{}' not found.", botName));
        return true;
    }
    
    if (!PlayerbotsMgr::instance().GetPlayerbotAI(bot))
    {
        handler->SendSysMessage(fmt::format("OllamaChat: Player '{}' is not a bot.", botName));
        return true;
    }
    
    std::string personality = GetBotPersonality(bot);
    std::string prompt = GetPersonalityPromptAddition(personality);
    
    handler->SendSysMessage(fmt::format("OllamaChat: Bot '{}' has personality '{}'", botName, personality));
    handler->SendSysMessage(fmt::format("  Prompt: {}", prompt));
    
    return true;
}

bool OllamaChatConfigCommand::HandleOllamaPersonalitySetCommand(ChatHandler* handler, std::string botName, std::string personality)
{
    Player* bot = ObjectAccessor::FindPlayerByName(botName);
    if (!bot)
    {
        handler->SendSysMessage(fmt::format("OllamaChat: Bot '{}' not found.", botName));
        return true;
    }
    
    if (!PlayerbotsMgr::instance().GetPlayerbotAI(bot))
    {
        handler->SendSysMessage(fmt::format("OllamaChat: Player '{}' is not a bot.", botName));
        return true;
    }
    
    if (!PersonalityExists(personality))
    {
        handler->SendSysMessage(fmt::format("OllamaChat: Personality '{}' does not exist. Use '.ollama personality list' to see available personalities.", personality));
        return true;
    }
    
    if (SetBotPersonality(bot, personality))
    {
        std::string prompt = GetPersonalityPromptAddition(personality);
        handler->SendSysMessage(fmt::format("OllamaChat: Set bot '{}' personality to '{}'", botName, personality));
        handler->SendSysMessage(fmt::format("  Prompt: {}", prompt));
    }
    else
    {
        handler->SendSysMessage(fmt::format("OllamaChat: Failed to set personality for bot '{}'.", botName));
    }
    
    return true;
}

bool OllamaChatConfigCommand::HandleOllamaPersonalityListCommand(ChatHandler* handler)
{
    std::vector<std::string> personalities = GetAllPersonalityKeys();
    
    if (personalities.empty())
    {
        handler->SendSysMessage("OllamaChat: No personalities loaded.");
        return true;
    }
    
    handler->SendSysMessage(fmt::format("OllamaChat: Available personalities ({} total, {} random-assignable):", 
                            personalities.size(), g_PersonalityKeysRandomOnly.size()));
    
    for (const auto& personality : personalities)
    {
        std::string prompt = GetPersonalityPromptAddition(personality);
        
        // Check if this personality is manual-only
        bool isManualOnly = (std::find(g_PersonalityKeysRandomOnly.begin(), g_PersonalityKeysRandomOnly.end(), personality) 
                            == g_PersonalityKeysRandomOnly.end());
        
        std::string manualTag = isManualOnly ? " [MANUAL ONLY]" : "";

        handler->SendSysMessage(fmt::format("  - {}{}", personality, manualTag));
        handler->SendSysMessage(fmt::format("    {}", prompt));
    }

    return true;
}

// ---------------------------------------------------------------------------
// .ollama tactical status [botGuid?] — per-bot companion-mode state dump.
// Without argument: lists every configured tactical bot.
// With argument: shows only that bot (useful when scaled up to multiple).
// ---------------------------------------------------------------------------
bool OllamaChatConfigCommand::HandleOllamaTacticalStatusCommand(ChatHandler* handler, Optional<uint64> botGuid)
{
    handler->SendSysMessage(fmt::format(
        "OllamaChat Tactical: enable={}, bots={}, overrides={}, humanPresenceRequired={}, model='{}', url='{}'",
        g_TacticalEnable, g_TacticalBotGUIDSet.size(),
        g_TacticalBotConfigs.size(), g_TacticalHumanPresenceRequired, g_TacticalModel, g_TacticalUrl));
    handler->SendSysMessage(fmt::format(
        "  heartbeat={}ms, cooldown={}ms, timeout={}ms, numCtx={}, maxConcurrent={}, promptMaxBytes={}, "
        "allowCombatOverride={}, allowEscalation={}",
        g_TacticalHeartbeatMs, g_TacticalBotCooldownMs, g_TacticalRequestTimeoutMs,
        g_TacticalNumCtx, g_TacticalMaxConcurrentQueries, g_TacticalPromptMaxBytes,
        g_TacticalAllowCombatOverride, g_TacticalAllowEscalation));
    std::string classifierUrl = g_GatewayOllamaClassifierUrl.empty() ? g_TacticalUrl : g_GatewayOllamaClassifierUrl;
    std::string classifierModel = g_GatewayOllamaClassifierModel.empty() ? g_TacticalModel : g_GatewayOllamaClassifierModel;
    handler->SendSysMessage(fmt::format(
        "  LLM classifier: enable={}, model='{}', url='{}', timeout={}ms, numCtx={}, minConfidence={:.2f}",
        g_GatewayOllamaClassifierEnable, classifierModel, classifierUrl,
        g_GatewayOllamaClassifierTimeoutMs, g_GatewayOllamaClassifierNumCtx,
        g_GatewayOllamaClassifierMinConfidence));
    handler->SendSysMessage(fmt::format(
        "  jev classifier: enable={} (effective={}), minConfidence={:.2f}, maxWords={} (0 = no word gate)",
        g_JevClassifierEnable, Jev::EnabledFor(Jev::kSiteClassifier), g_JevClassifierMinConfidence,
        g_GatewayClassifierMaxWords.load(std::memory_order_relaxed)));
    handler->SendSysMessage(fmt::format(
        "  prompts: tactical='{}' ({} bytes), classifier='{}' ({} bytes)",
        g_TacticalSystemPromptFile, g_TacticalSystemPromptText.size(),
        g_GatewayOllamaClassifierSystemPromptFile,
        g_GatewayOllamaClassifierSystemPromptText.size()));
    handler->SendSysMessage(fmt::format(
        "  strategic: suppressLeaderTick={}, maxEscalations={}/hr/bot, cooldown={}s, queueMax={}",
        g_StrategicSuppressLeaderTickForTacticalBots,
        g_StrategicMaxEscalationsPerBotPerHour,
        g_StrategicEscalationCooldownSec,
        g_StrategicQueueMaxSize));

    // Iterate either the requested guid or every configured bot.
    std::vector<uint64_t> targets;
    if (botGuid.has_value())
    {
        targets.push_back(*botGuid);
    }
    else
    {
        targets.assign(g_TacticalBotGUIDSet.begin(), g_TacticalBotGUIDSet.end());
    }

    if (targets.empty())
    {
        handler->SendSysMessage("  (no tactical bots configured — Tactical.BotGUIDs is empty)");
        return true;
    }

    time_t now = time(nullptr);
    for (uint64_t guid : targets)
    {
        Player* bot = ObjectAccessor::FindPlayer(ObjectGuid(guid));
        std::string name = bot ? bot->GetName() : "<offline>";
        bool tacticalEnabled = g_TacticalBotGUIDSet.count(guid) > 0;
        bool inCombat = bot ? bot->IsInCombat() : false;

        handler->SendSysMessage(fmt::format(
            "  bot={} '{}' — tactical={}, online={}, inCombat={}",
            guid, name,
            tacticalEnabled ? "yes" : "NO (not in BotGUIDs)",
            bot ? "yes" : "no",
            inCombat));
        TacticalBotConfig cfg = ResolveTacticalBotConfig(guid);
        handler->SendSysMessage(fmt::format(
            "    ollama: model='{}', url='{}', timeout={}ms, numCtx={}, maxConcurrent={}",
            cfg.model, cfg.url, cfg.timeoutMs, cfg.numCtx, cfg.maxConcurrentQueries));

        TacticalDirective d = GetTacticalDirective(guid);
        if (d.IsActive(now))
        {
            handler->SendSysMessage(fmt::format(
                "    directive: \"{}\" (set_by={}, expires in {}s)",
                d.goal, d.setBy,
                static_cast<long long>(d.expiresAt - now)));
        }
        else
        {
            handler->SendSysMessage("    directive: (none)");
        }
    }
    return true;
}
