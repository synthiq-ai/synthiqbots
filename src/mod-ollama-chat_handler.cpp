#include "Log.h"
#include "Language.h"
#include "Player.h"
#include "Chat.h"
#include "Channel.h"
#include "PlayerbotAI.h"
#include "PlayerbotMgr.h"
#include "Config.h"
#include "Common.h"
#include "Guild.h"
#include "ObjectAccessor.h"
#include "ChannelMgr.h"
#include <unordered_set>
#include <vector>
#include <thread>
#include <algorithm>
#include <random>
#include <cctype>
#include <chrono>
#include "DatabaseEnv.h"
#include "mod-ollama-chat_handler.h"
#include "mod-ollama-chat_config.h"
#include "mod-ollama-chat-utilities.h"
#include "mod-ollama-chat_gateway.h"
#include "mod-ollama-chat_playerprefs.h"
#include "mod-ollama-chat_proactive.h"
#include "mod-ollama-chat_promotion.h"
#include "SharedDefines.h"
#include "Group.h"

// Forward declarations for internal helper functions.
static bool IsBotEligibleForChatChannelLocal(Player* bot, Player* player,
                                             ChatChannelSourceLocal source, Channel* channel = nullptr, Player* receiver = nullptr);

const char* ChatChannelSourceLocalStr[] =
{
    "Undefined",  // 0
    "Say",        // 1
    "Party",      // 2
    "Raid",       // 3
    "Guild",      // 4
    "Officer",    // 5
    "Yell",       // 6
    "Whisper",    // 7
    "Unknown8",   // 8
    "Unknown9",   // 9
    "Unknown10",  // 10
    "Unknown11",  // 11
    "Unknown12",  // 12
    "Unknown13",  // 13
    "Unknown14",  // 14
    "Unknown15",  // 15
    "Unknown16",  // 16
    "General"     // 17
};

std::string rtrim(const std::string& s)
{
    const std::string whitespace = " \t\n\r,.!?;:";
    size_t end = s.find_last_not_of(whitespace);
    return (end == std::string::npos) ? "" : s.substr(0, end + 1);
}

ChatChannelSourceLocal GetChannelSourceLocal(uint32_t type)
{
    switch (type)
    {
        case CHAT_MSG_SAY:
            return SRC_SAY_LOCAL;
        case CHAT_MSG_PARTY:
        case CHAT_MSG_PARTY_LEADER:
            return SRC_PARTY_LOCAL;
        case CHAT_MSG_RAID:
        case CHAT_MSG_RAID_LEADER:
        case CHAT_MSG_RAID_WARNING:
            return SRC_RAID_LOCAL;
        case CHAT_MSG_GUILD:
            return SRC_GUILD_LOCAL;
        case CHAT_MSG_OFFICER:
            return SRC_OFFICER_LOCAL;
        case CHAT_MSG_YELL:
            return SRC_YELL_LOCAL;
        case CHAT_MSG_WHISPER:
        case CHAT_MSG_WHISPER_FOREIGN:
        case CHAT_MSG_WHISPER_INFORM:
            return SRC_WHISPER_LOCAL;
        case CHAT_MSG_CHANNEL:
            return SRC_GENERAL_LOCAL;
        default:
            return SRC_UNDEFINED_LOCAL;
    }
}
Channel* GetValidChannel(uint32_t teamId, const std::string& channelName, Player* player)
{
    ChannelMgr* cMgr = ChannelMgr::forTeam(static_cast<TeamId>(teamId));
    Channel* channel = cMgr->GetChannel(channelName, player);
    if (!channel)
    {
        if(g_DebugEnabled)
        {
            LOG_ERROR("server.loading", "[Ollama Chat] Channel '{}' not found for team {}", channelName, teamId);
        }
    }
    return channel;
}

bool PlayerBotChatHandler::OnPlayerCanUseChat(Player* player, uint32_t type, uint32_t lang, std::string& msg)
{
    if (!g_Enable)
        return true;

    ChatChannelSourceLocal sourceLocal = GetChannelSourceLocal(type);
    ProcessChat(player, type, lang, msg, sourceLocal, nullptr, nullptr);
    return true;
}

bool PlayerBotChatHandler::OnPlayerCanUseChat(Player* player, uint32_t type, uint32_t lang, std::string& msg, Group* /*group*/)
{
    if (!g_Enable)
        return true;

    ChatChannelSourceLocal sourceLocal = GetChannelSourceLocal(type);
    ProcessChat(player, type, lang, msg, sourceLocal, nullptr, nullptr);
    return true;
}

bool PlayerBotChatHandler::OnPlayerCanUseChat(Player* player, uint32_t type, uint32_t lang, std::string& msg, Guild* /*guild*/)
{
    if (!g_Enable)
        return true;

    ChatChannelSourceLocal sourceLocal = GetChannelSourceLocal(type);
    ProcessChat(player, type, lang, msg, sourceLocal, nullptr, nullptr);
    return true;
}

bool PlayerBotChatHandler::OnPlayerCanUseChat(Player* player, uint32_t type, uint32_t lang, std::string& msg, Channel* channel)
{
    if (!g_Enable)
        return true;

    ChatChannelSourceLocal sourceLocal = GetChannelSourceLocal(type);
    ProcessChat(player, type, lang, msg, sourceLocal, channel, nullptr);
    return true;
}

bool PlayerBotChatHandler::OnPlayerCanUseChat(Player* player, uint32_t type, uint32_t lang, std::string& msg, Player* receiver)
{
    // Only process if our module is enabled
    if (!g_Enable)
        return true;

    if (type == CHAT_MSG_WHISPER)
    {
        // Check if this is a valid whisper to a bot
        if (!receiver || !player || player == receiver)
            return true;

        // Check if sender is a bot - if so, don't trigger Ollama responses for bot-to-bot whispers
        PlayerbotAI* senderAI = PlayerbotsMgr::instance().GetPlayerbotAI(player);
        if (senderAI && senderAI->IsBotAI())
        {
            return true;
        }

        PlayerbotAI* receiverAI = PlayerbotsMgr::instance().GetPlayerbotAI(receiver);
        if (!receiverAI || !receiverAI->IsBotAI())
            return true;

        // Skip playerbots-mod command introspection whispers (e.g. "co ?", "nc ?",
        // "stats", "help"). These are emitted by the user's session when they run
        // .playerbots commands and would otherwise fire a full gateway call that
        // returns a generic greeting — burns tokens and floods chat.
        {
            std::string trimmed = msg;
            while (!trimmed.empty() && std::isspace(static_cast<unsigned char>(trimmed.front()))) trimmed.erase(trimmed.begin());
            while (!trimmed.empty() && std::isspace(static_cast<unsigned char>(trimmed.back())))  trimmed.pop_back();
            std::string lower = trimmed;
            for (auto& c : lower) c = static_cast<char>(std::tolower(static_cast<unsigned char>(c)));
            static const std::unordered_set<std::string> kPlayerbotsCommandWhispers = {
                "co", "co ?", "co?", "nc", "nc ?", "nc?",
                "help", "help ?", "help?", "stats", "stats?",
                "ra", "ri", "ro", "rl", "rti", "rti?",
                "reset", "reset ?", "reset?", "revive", "release",
                "stay", "follow", "stop", "attack", "grind", "pull", "flee",
                "autogear", "gear", "maintenance", "disperse", "roll"
            };
            if (kPlayerbotsCommandWhispers.count(lower))
            {
                if (g_DebugEnabled)
                    LOG_INFO("server.loading",
                             "[Ollama Chat] Skipping playerbots-command whisper '{}' from {} to {}",
                             lower, player->GetName(), receiver->GetName());
                return true;
            }
        }
    }

    if (g_DebugEnabled)
    {
        LOG_INFO("server.loading", "[Ollama Chat] OnPlayerCanUseChat called: player={}, type={}, receiver={}",
            player->GetName(), type, receiver ? receiver->GetName() : "null");
    }

    // Process the chat immediately in OnPlayerCanUseChat to prevent double processing
    ChatChannelSourceLocal sourceLocal = GetChannelSourceLocal(type);
    ProcessChat(player, type, lang, msg, sourceLocal, nullptr, receiver);

    // Return false to prevent the message from being processed again in OnPlayerChat
    return true;
}

void AppendBotConversation(uint64_t botGuid, uint64_t playerGuid, const std::string& playerMessage, const std::string& botReply)
{
    std::lock_guard<std::mutex> lock(g_ConversationHistoryMutex);
    auto& playerHistory = g_BotConversationHistory[botGuid][playerGuid];
    playerHistory.push_back({ playerMessage, botReply });
    while (playerHistory.size() > g_MaxConversationHistory)
    {
        playerHistory.pop_front();
    }

}

void SaveBotConversationHistoryToDB()
{
    std::lock_guard<std::mutex> lock(g_ConversationHistoryMutex);

    for (const auto& [botGuid, playerMap] : g_BotConversationHistory) {
        for (const auto& [playerGuid, history] : playerMap) {
            for (const auto& pair : history) {
                const std::string& playerMessage = pair.first;
                const std::string& botReply = pair.second;

                std::string escPlayerMsg = playerMessage;
                CharacterDatabase.EscapeString(escPlayerMsg);

                std::string escBotReply = botReply;
                CharacterDatabase.EscapeString(escBotReply);

                CharacterDatabase.Execute(SafeFormat(
                    "INSERT IGNORE INTO mod_ollama_chat_history (bot_guid, player_guid, timestamp, player_message, bot_reply) "
                    "VALUES ({}, {}, NOW(), '{}', '{}')",
                    botGuid, playerGuid, escPlayerMsg, escBotReply));
            }
        }
    }

    // Cleanup: keep only the N most recent entries per bot/player pair
    std::string cleanupQuery = R"SQL(
        WITH ranked_history AS (
            SELECT
                bot_guid,
                player_guid,
                timestamp,
                ROW_NUMBER() OVER (
                    PARTITION BY bot_guid, player_guid
                    ORDER BY timestamp DESC
                ) as rn
            FROM mod_ollama_chat_history
        )
        DELETE FROM mod_ollama_chat_history
        WHERE (bot_guid, player_guid, timestamp) IN (
            SELECT bot_guid, player_guid, timestamp
            FROM ranked_history
            WHERE rn > {}
        );
    )SQL";
    CharacterDatabase.Execute(SafeFormat(cleanupQuery, g_MaxConversationHistory));
}

void PlayerBotChatHandler::ProcessChat(Player* player, uint32_t type, uint32_t lang, std::string& msg, ChatChannelSourceLocal sourceLocal, Channel* channel, Player* receiver)
{
    if (player == nullptr) {
        LOG_ERROR("server.loading", "[Ollama Chat] ProcessChat: player is null");
        return;
    }
    if (msg.empty()) {
        return;
    }
    if (lang == LANG_ADDON) return;

    // Feature 002 — proactive leader bot intent tap-in. Short-circuits the
    // regular chat chain when the player is answering an open proposal
    // (yes/no) or marking an executing activity done. Returns false when
    // not engaged, when no open proposal/plan exists, or on classifier
    // miss — in which case the regular chain continues unchanged.
    if (ollamachat::proactive::OnPlayerChat(player, msg, type)) {
        return;
    }
    std::string chanName = (channel != nullptr) ? channel->GetName() : "Unknown";
    uint32_t channelId = (channel != nullptr) ? channel->GetChannelId() : 0;
    std::string receiverName = (receiver != nullptr) ? receiver->GetName() : "None";
    if(g_DebugEnabled)
    {
        LOG_INFO("server.loading",
                "[Ollama Chat] Player {} sent msg: '{}' | Source: {} | Channel Name: {} | Channel ID: {} | Receiver: {}",
                player->GetName(), msg, (int)sourceLocal, chanName, channelId, receiverName);
    }


    auto startsWithWord = [](const std::string& text, const std::string& word) {
        if (text.size() < word.size()) return false;
        if (text.compare(0, word.size(), word) != 0) return false;
        // If exact length match or next char is whitespace/punctuation, it's a word
        return text.size() == word.size() || !std::isalnum((unsigned char)text[word.size()]);
    };

    std::string trimmedMsg = rtrim(msg);
    // Blacklist bypass for coordination channels (whisper/party/raid/guild/officer).
    // The blacklist protects public chatter from playerbot command verbs, but
    // coordination channels are deliberate squad-control surfaces. Let the
    // local/gateway model interpret those messages instead of short-circuiting.
    bool isCoordinationChannel =
        sourceLocal == SRC_WHISPER_LOCAL
        || sourceLocal == SRC_PARTY_LOCAL
        || sourceLocal == SRC_RAID_LOCAL
        || sourceLocal == SRC_GUILD_LOCAL
        || sourceLocal == SRC_OFFICER_LOCAL;
    if (!isCoordinationChannel)
    {
        for (const std::string& blacklist : g_BlacklistCommands)
        {
            if (startsWithWord(trimmedMsg, blacklist))
            {
                if (g_DebugEnabled)
                    LOG_INFO("server.loading",
                             "[Ollama Chat] Message starts with '{}' (blacklisted). Skipping bot responses.",
                             blacklist);
                return;
            }
        }
    }
    
    // Check if this channel type is disabled
    if (sourceLocal == SRC_GENERAL_LOCAL && g_DisableForCustomChannels)
    {
        if (g_DebugEnabled)
        {
            LOG_INFO("server.loading", "[Ollama Chat] Custom channels are disabled, skipping");
        }
        return;
    }
    
    if ((sourceLocal == SRC_SAY_LOCAL || sourceLocal == SRC_YELL_LOCAL) && g_DisableForSayYell)
    {
        if (g_DebugEnabled)
        {
            LOG_INFO("server.loading", "[Ollama Chat] Say/Yell channels are disabled, skipping");
        }
        return;
    }
    
    if ((sourceLocal == SRC_GUILD_LOCAL || sourceLocal == SRC_OFFICER_LOCAL) && g_DisableForGuild)
    {
        if (g_DebugEnabled)
        {
            LOG_INFO("server.loading", "[Ollama Chat] Guild channels are disabled, skipping");
        }
        return;
    }
    
    if ((sourceLocal == SRC_PARTY_LOCAL || sourceLocal == SRC_RAID_LOCAL) && g_DisableForParty)
    {
        if (g_DebugEnabled)
        {
            LOG_INFO("server.loading", "[Ollama Chat] Party/Raid channels are disabled, skipping");
        }
        return;
    }
             
    PlayerbotAI* senderAI = PlayerbotsMgr::instance().GetPlayerbotAI(player);
    bool senderIsBot = (senderAI && senderAI->IsBotAI());
    
    std::vector<Player*> eligibleBots;
    
    // Handle different chat sources differently
    if (sourceLocal == SRC_WHISPER_LOCAL && receiver != nullptr)
    {
        // Check if whisper replies are disabled
        if (!g_EnableWhisperReplies)
        {
            if(g_DebugEnabled)
            {
                LOG_INFO("server.loading", "[Ollama Chat] Whisper replies are disabled, skipping");
            }
            return;
        }
        
        if(g_DebugEnabled)
        {
            LOG_INFO("server.loading", "[Ollama Chat] Processing whisper from {} to {}", 
                    player->GetName(), receiver->GetName());
        }
        
        // Skip bot-to-bot whispers to prevent Ollama responses
        if (senderIsBot)
        {
            return;
        }
        
        // For whispers, only the receiver bot can respond (if it's a bot)
        PlayerbotAI* receiverAI = PlayerbotsMgr::instance().GetPlayerbotAI(receiver);
        if (receiverAI && receiverAI->IsBotAI())
        {
            eligibleBots.push_back(receiver);
            if(g_DebugEnabled)
            {
                LOG_INFO("server.loading", "[Ollama Chat] Found eligible bot {} for whisper", receiver->GetName());
            }
        }
        else if(g_DebugEnabled)
        {
            LOG_INFO("server.loading", "[Ollama Chat] Whisper target {} is not a bot or has no AI", receiver->GetName());
        }
    }
    else if (channel != nullptr)
    {
        // For channel chat, find all bots that are in the same channel instance
        if(g_DebugEnabled)
        {
            LOG_INFO("server.loading", "[Ollama Chat] Processing channel message in '{}' (ID: {})", 
                    channel->GetName(), channel->GetChannelId());
        }
        
        // Verify the original channel is valid before proceeding
        if (!channel)
        {
            if(g_DebugEnabled)
            {
                LOG_ERROR("server.loading", "[Ollama Chat] Channel is null, cannot process channel message");
            }
            return;
        }
        
        // For channel chat, simply find all bots in the same zone as the player
        auto const& allPlayers = ObjectAccessor::GetPlayers();
        for (auto const& itr : allPlayers)
        {
            Player* candidate = itr.second;
            if (!candidate || candidate == player)
                continue;
                
            // Skip non-bots early
            PlayerbotAI* candidateAI = PlayerbotsMgr::instance().GetPlayerbotAI(candidate);
            if (!candidateAI || !candidateAI->IsBotAI())
                continue;
            
            // Check if this is a local or global channel
            bool isLocalChannel = (channel->GetName().find("General -") != std::string::npos || 
                                  channel->GetName().find("Trade -") != std::string::npos ||
                                  channel->GetName().find("LocalDefense -") != std::string::npos);
            
            bool isGlobalChannel = (channel->GetName().find("World") != std::string::npos || channel->GetName().find("LookingForGroup") != std::string::npos);
        
            // For local channels, bot must be in same zone as player
            if (isLocalChannel)
            {
                // ZONE CHECK: Bot must be in exact same zone as player
                if (candidate->GetZoneId() != player->GetZoneId())
                {
                    if(g_DebugEnabled)
                    {
                        //LOG_ERROR("server.loading", "[Ollama Chat] Bot {} FAILED zone check - Bot zone: {}, Player zone: {}, Channel: '{}'", candidate->GetName(), candidate->GetZoneId(), player->GetZoneId(), channel->GetName());
                    }
                    continue; // SKIP this bot - wrong zone
                }
            }
            // For global channels like World, no zone restriction
            
            // CHANNEL MEMBERSHIP CHECK: Bot must actually be in the channel
            if (!candidate->IsInChannel(channel))
            {
                if(g_DebugEnabled)
                {
                    //LOG_INFO("server.loading", "[Ollama Chat] Bot {} not in channel '{}', skipping", candidate->GetName(), channel->GetName());
                }
                continue;
            }
            
            // FACTION CHECK: For non-global channels, ensure same faction
            if (candidate->GetTeamId() != player->GetTeamId())
            {
                if (!isGlobalChannel)
                {
                    if(g_DebugEnabled)
                    {
                        //LOG_ERROR("server.loading", "[Ollama Chat] Bot {} FAILED faction check - Bot: {}, Player: {}, Channel: '{}'", candidate->GetName(), (int)candidate->GetTeamId(), (int)player->GetTeamId(), channel->GetName());
                    }
                    continue; // SKIP this bot - wrong faction
                }
            }
            
            // CHANNEL MEMBERSHIP CHECK: Verify bot is actually in the channel
            if (!candidate->IsInChannel(channel))
            {
                if(g_DebugEnabled)
                {
                    //LOG_ERROR("server.loading", "[Ollama Chat] Bot {} FAILED channel membership check - Not in channel '{}'", candidate->GetName(), channel->GetName());
                }
                continue; // SKIP this bot - not in the channel
            }
            
            // REAL PLAYER CHECK: Channel must have at least one real player
            bool hasRealPlayerInChannel = false;
            for (auto const& playerItr : allPlayers)
            {
                Player* potentialRealPlayer = playerItr.second;
                if (potentialRealPlayer && potentialRealPlayer->IsInChannel(channel))
                {
                    PlayerbotAI* realPlayerAI = PlayerbotsMgr::instance().GetPlayerbotAI(potentialRealPlayer);
                    if (!realPlayerAI || !realPlayerAI->IsBotAI())
                    {
                        hasRealPlayerInChannel = true;
                        break;
                    }
                }
            }
            
            if (!hasRealPlayerInChannel)
            {
                if(g_DebugEnabled)
                {
                    //LOG_INFO("server.loading", "[Ollama Chat] Bot {} skipped - no real players in channel '{}'", candidate->GetName(), channel->GetName());
                }
                continue;
            }
            
            // ONLY add bots that passed ALL verifications
            eligibleBots.push_back(candidate);
            if(g_DebugEnabled)
            {
                // LOG_INFO("server.loading", "[Ollama Chat] VERIFIED eligible bot {} in channel '{}' - Distance: {:.2f}, Zone match: {}", candidate->GetName(), channel->GetName(), candidate->GetDistance(player), (candidate->GetZoneId() == player->GetZoneId()));
            }
        }
        
        if(g_DebugEnabled)
        {
            LOG_INFO("server.loading", "[Ollama Chat] Found {} bots in channel instance '{}'", 
                    eligibleBots.size(), channel->GetName());
        }
    }
    else
    {
        // For other chat types (say, yell, guild, party, etc.), use all players and filter by eligibility
        auto const& allPlayers = ObjectAccessor::GetPlayers();
        for (auto const& itr : allPlayers)
        {
            Player* candidate = itr.second;
            if (candidate->IsInWorld() && candidate != player)
            {
                PlayerbotAI* candidateAI = PlayerbotsMgr::instance().GetPlayerbotAI(candidate);
                if (candidateAI && candidateAI->IsBotAI())
                {
                    // For Guild/Party, verify there's a real player in that guild/party
                    if (sourceLocal == SRC_GUILD_LOCAL || sourceLocal == SRC_OFFICER_LOCAL)
                    {
                        if (candidate->GetGuildId() != 0)
                        {
                            // Check if any real player is online in this guild
                            bool hasRealPlayerInGuild = false;
                            for (auto const& guildPlayerItr : allPlayers)
                            {
                                Player* guildMember = guildPlayerItr.second;
                                if (guildMember && guildMember->GetGuildId() == candidate->GetGuildId())
                                {
                                    PlayerbotAI* memberAI = PlayerbotsMgr::instance().GetPlayerbotAI(guildMember);
                                    if (!memberAI || !memberAI->IsBotAI())
                                    {
                                        hasRealPlayerInGuild = true;
                                        break;
                                    }
                                }
                            }
                            if (!hasRealPlayerInGuild)
                                continue; // Skip bot - no real players in guild
                        }
                    }
                    else if (sourceLocal == SRC_PARTY_LOCAL || sourceLocal == SRC_RAID_LOCAL)
                    {
                        Group* group = candidate->GetGroup();
                        if (group)
                        {
                            // Check if any real player is in this group
                            bool hasRealPlayerInGroup = false;
                            for (GroupReference* ref = group->GetFirstMember(); ref; ref = ref->next())
                            {
                                Player* member = ref->GetSource();
                                if (member)
                                {
                                    PlayerbotAI* memberAI = PlayerbotsMgr::instance().GetPlayerbotAI(member);
                                    if (!memberAI || !memberAI->IsBotAI())
                                    {
                                        hasRealPlayerInGroup = true;
                                        break;
                                    }
                                }
                            }
                            if (!hasRealPlayerInGroup)
                                continue; // Skip bot - no real players in group
                        }
                    }
                    else if (sourceLocal == SRC_SAY_LOCAL || sourceLocal == SRC_YELL_LOCAL)
                    {
                        // For Say/Yell, require a real player within hearing distance
                        float threshold = (sourceLocal == SRC_SAY_LOCAL) ? g_SayDistance : g_YellDistance;
                        bool hasRealPlayerNearby = false;
                        
                        if (candidate->IsInWorld() && threshold > 0.0f)
                        {
                            for (auto const& nearbyPlayerItr : allPlayers)
                            {
                                Player* nearbyPlayer = nearbyPlayerItr.second;
                                if (nearbyPlayer && nearbyPlayer->IsInWorld())
                                {
                                    PlayerbotAI* nearbyAI = PlayerbotsMgr::instance().GetPlayerbotAI(nearbyPlayer);
                                    if (!nearbyAI || !nearbyAI->IsBotAI())
                                    {
                                        if (candidate->GetDistance(nearbyPlayer) <= threshold)
                                        {
                                            hasRealPlayerNearby = true;
                                            break;
                                        }
                                    }
                                }
                            }
                        }
                        
                        if (!hasRealPlayerNearby)
                            continue; // Skip bot - no real player can hear Say/Yell
                    }
                    
                    eligibleBots.push_back(candidate);
                }
            }
        }
    }
    
    std::vector<Player*> candidateBots;
    int notEligibleCount = 0;
    for (Player* bot : eligibleBots)
    {
        if (!bot)
        {
            continue;
        }
        
        // For channel messages, bots in eligibleBots have already passed STRICT channel checks
        // Only run additional eligibility checks for non-channel sources
        // EXCEPTION: If channel is nullptr but sourceLocal is a channel type (like GENERAL), 
        // treat it as a channel message (happens with bot-initiated messages)
        bool isChannelSource = (sourceLocal == SRC_GENERAL_LOCAL);
        
        if (channel != nullptr || isChannelSource)
        {
            // Channel bots have already been verified to be in EXACT same channel instance
            // OR this is a channel-type source (General) even without channel object
            candidateBots.push_back(bot);
        }
        else
        {
            // For non-channel sources (Say/Yell/Guild/Party/Whisper), run the full eligibility check
            if (IsBotEligibleForChatChannelLocal(bot, player, sourceLocal, channel, receiver))
                candidateBots.push_back(bot);
            else
                notEligibleCount++;
        }
    }
    
    if (g_DebugEnabled && notEligibleCount > 0)
    {
        LOG_INFO("server.loading", "[Ollama Chat] {} bots not eligible for {} (distance/guild/party checks failed)", 
                notEligibleCount, ChatChannelSourceLocalStr[sourceLocal]);
    }
    
    // Determine reply chance based on channel type
    uint32_t chance;
    if (sourceLocal == SRC_SAY_LOCAL || sourceLocal == SRC_YELL_LOCAL)
    {
        // Say/Yell channel type
        chance = senderIsBot ? g_BotReplyChance_Say : g_PlayerReplyChance_Say;
    }
    else if (sourceLocal == SRC_PARTY_LOCAL || sourceLocal == SRC_RAID_LOCAL)
    {
        // Party/Raid channel type
        chance = senderIsBot ? g_BotReplyChance_Party : g_PlayerReplyChance_Party;
    }
    else if (sourceLocal == SRC_GUILD_LOCAL || sourceLocal == SRC_OFFICER_LOCAL)
    {
        // Guild/Officer channel type
        chance = senderIsBot ? g_BotReplyChance_Guild : g_PlayerReplyChance_Guild;
    }
    else if (sourceLocal == SRC_GENERAL_LOCAL)
    {
        // General/Trade/Custom channel type
        chance = senderIsBot ? g_BotReplyChance_Channel : g_PlayerReplyChance_Channel;
    }
    else
    {
        // Default fallback (whispers, etc.) - use Say chances
        chance = senderIsBot ? g_BotReplyChance_Say : g_PlayerReplyChance_Say;
    }
    
    if(g_DebugEnabled)
    {
        LOG_INFO("server.loading", "[Ollama Chat] Sender: {} ({}), Channel: {}, Reply Chance: {}%, Candidate Bots: {}",
                player->GetName(), senderIsBot ? "BOT" : "PLAYER", ChatChannelSourceLocalStr[sourceLocal], chance, candidateBots.size());
    }
    
    std::vector<Player*> finalCandidates;
    
    // For whispers, handle directly - there should only be one receiver bot
    if (sourceLocal == SRC_WHISPER_LOCAL)
    {
        if (!candidateBots.empty())
        {
            Player* whisperBot = candidateBots[0]; // Should only be one bot for whispers
            if (!(g_DisableRepliesInCombat && whisperBot->IsInCombat()))
            {
                finalCandidates.push_back(whisperBot);
                if(g_DebugEnabled)
                {
                    LOG_INFO("server.loading", "[Ollama Chat] Whisper: Bot {} selected to respond", whisperBot->GetName());
                }
            }
        }
    }
    else
    {
        // Handle non-whisper chats with normal multi-bot logic
        std::vector<std::pair<size_t, Player*>> mentionedBots;

        // Helper to convert string to lowercase safely
        auto toLowerStr = [](const std::string& str) -> std::string {
            std::string result = str;
            for (char& c : result)
            {
                c = std::tolower(static_cast<unsigned char>(c));
            }
            return result;
        };

        // Helper to check if a bot name is mentioned as a complete word
        auto isBotNameMentioned = [&trimmedMsg, &toLowerStr](const std::string& botName) -> size_t {
            std::string lowerMsg = toLowerStr(trimmedMsg);
            std::string lowerBotName = toLowerStr(botName);
            
            size_t pos = 0;
            while ((pos = lowerMsg.find(lowerBotName, pos)) != std::string::npos)
            {
                // Check if it's a word boundary before the name
                bool validStart = (pos == 0 || !std::isalnum(static_cast<unsigned char>(lowerMsg[pos - 1])));
                // Check if it's a word boundary after the name
                size_t endPos = pos + lowerBotName.length();
                bool validEnd = (endPos >= lowerMsg.length() || !std::isalnum(static_cast<unsigned char>(lowerMsg[endPos])));
                
                if (validStart && validEnd)
                {
                    return pos; // Found a valid word-boundary match
                }
                pos++; // Continue searching
            }
            return std::string::npos;
        };

        for (Player* bot : candidateBots)
        {
            if (!bot)
            {
                continue;
            }
            if (g_DisableRepliesInCombat && bot->IsInCombat())
            {
                continue;
            }
            
            size_t pos = isBotNameMentioned(bot->GetName());
            if (pos != std::string::npos)
            {
                mentionedBots.emplace_back(pos, bot);
                if(g_DebugEnabled)
                {
                    LOG_INFO("server.loading", "[Ollama Chat] Bot {} mentioned at position {} in message", bot->GetName(), pos);
                }
            }
        }

        if (!mentionedBots.empty())
        {
            // Sort by position to get the first mentioned bot
            std::sort(mentionedBots.begin(), mentionedBots.end(),
                      [](const std::pair<size_t, Player*> &a, const std::pair<size_t, Player*> &b) { return a.first < b.first; });
            Player* chosen = mentionedBots.front().second;
            if (!(g_DisableRepliesInCombat && chosen->IsInCombat()))
            {
                finalCandidates.push_back(chosen);
                if(g_DebugEnabled)
                {
                    LOG_INFO("server.loading", "[Ollama Chat] Bot {} selected (mentioned first at position {})", 
                            chosen->GetName(), mentionedBots.front().first);
                }
            }
        }
        else
        {
            for (Player* bot : candidateBots)
            {
                if (g_DisableRepliesInCombat && bot->IsInCombat())
                {
                    if(g_DebugEnabled)
                    {
                        LOG_INFO("server.loading", "[Ollama Chat] Bot {} skipped - in combat", bot->GetName());
                    }
                    continue;
                }

                // MentionExempt bypass: gateway bots listed in
                // Gateway.MentionExemptBots (e.g., "Claude") are supposed to
                // respond to every coordination-channel line without needing
                // their name mentioned. The existing exempt-check at the
                // gateway-gating layer (further below) only applies AFTER
                // finalCandidates is populated — if the chance-roll drops the
                // exempt bot, he never gets there. Add him unconditionally so
                // the leader bot always hears every party-chat line.
                {
                    std::string botNameLc = bot->GetName();
                    for (auto& c : botNameLc) c = std::tolower(static_cast<unsigned char>(c));
                    if (!g_GatewayMentionExemptBotsSet.empty()
                        && g_GatewayMentionExemptBotsSet.count(botNameLc)
                        && IsGatewayBot(bot->GetGUID().GetRawValue()))
                    {
                        finalCandidates.push_back(bot);
                        if (g_DebugEnabled)
                            LOG_INFO("server.loading",
                                     "[Ollama Chat] Bot {} always-responds (MentionExempt)",
                                     bot->GetName());
                        continue;
                    }
                }

                uint32_t roll = urand(0, 99);
                if (roll < chance)
                {
                    finalCandidates.push_back(bot);
                    if(g_DebugEnabled)
                    {
                        LOG_INFO("server.loading", "[Ollama Chat] Bot {} PASSED chance roll ({} < {}%)", bot->GetName(), roll, chance);
                    }
                }
                else if(g_DebugEnabled)
                {
                    LOG_INFO("server.loading", "[Ollama Chat] Bot {} FAILED chance roll ({} >= {}%)", bot->GetName(), roll, chance);
                }
            }
        }
    }

    
    if (finalCandidates.empty())
    {
        if(g_DebugEnabled)
        {
            LOG_INFO("server.loading", "[Ollama Chat] *** NO BOTS RESPONDING *** to {} from {} in {} channel. "
                    "Eligible: {}, Candidates: {}, Final: 0, Chance: {}%",
                    senderIsBot ? "BOT" : "PLAYER", player->GetName(), ChatChannelSourceLocalStr[sourceLocal],
                    eligibleBots.size(), candidateBots.size(), chance);
            LOG_INFO("server.loading", "[Ollama Chat] No eligible bots found to respond to message '{}'. "
                    "Source: {}, Eligible bots: {}, Candidate bots: {}, Combat disabled: {}",
                    msg, ChatChannelSourceLocalStr[sourceLocal], eligibleBots.size(), 
                    candidateBots.size(), g_DisableRepliesInCombat);
        }
        return;
    }
    
    if (finalCandidates.size() > g_MaxBotsToPick)
    {
        std::vector<Player*> exemptBots;
        std::vector<Player*> otherBots;
        for (Player* bot : finalCandidates)
        {
            std::string nameLc = bot->GetName();
            for (auto& c : nameLc) c = std::tolower(static_cast<unsigned char>(c));
            if (!g_GatewayMentionExemptBotsSet.empty()
                && g_GatewayMentionExemptBotsSet.count(nameLc)
                && IsGatewayBot(bot->GetGUID().GetRawValue()))
                exemptBots.push_back(bot);
            else
                otherBots.push_back(bot);
        }
        std::random_device rd;
        std::mt19937 g(rd());
        std::shuffle(otherBots.begin(), otherBots.end(), g);
        uint32_t countToPick = urand(1, g_MaxBotsToPick);
        if (countToPick < exemptBots.size())
            countToPick = exemptBots.size();
        if(g_DebugEnabled)
        {
            LOG_INFO("server.loading", "[Ollama Chat] Limiting {} bots to {} (MaxBotsToPick, {} exempt pinned)",
                     finalCandidates.size(), countToPick, exemptBots.size());
        }
        finalCandidates = std::move(exemptBots);
        uint32_t remaining = (countToPick > finalCandidates.size()) ? countToPick - finalCandidates.size() : 0;
        if (remaining > otherBots.size()) remaining = otherBots.size();
        finalCandidates.insert(finalCandidates.end(), otherBots.begin(), otherBots.begin() + remaining);
    }
    
    if(g_DebugEnabled && !finalCandidates.empty())
    {
        std::string botNames;
        for (Player* bot : finalCandidates)
        {
            if (!botNames.empty()) botNames += ", ";
            botNames += bot->GetName();
        }
        LOG_INFO("server.loading", "[Ollama Chat] *** {} BOTS RESPONDING *** to {} from {} in {}: [{}]",
                finalCandidates.size(), senderIsBot ? "BOT" : "PLAYER", player->GetName(),
                ChatChannelSourceLocalStr[sourceLocal], botNames);
    }
    
    uint64_t senderGuid = player->GetGUID().GetRawValue();
    
    for (Player* bot : finalCandidates)
    {
        float distance = player->GetDistance(bot);
        if(g_DebugEnabled)
        {
            LOG_INFO("server.loading", "[Ollama Chat] Bot {} (distance: {}) is set to respond.", bot->GetName(), distance);
        }
        if (bot == nullptr) {
            continue;
        }
        uint64_t botGuid = bot->GetGUID().GetRawValue();

        // --- Player-facing suppression: opt-out / per-bot mute applies to ALL responses. ---
        uint32_t playerAccountId = player->GetSession() ? player->GetSession()->GetAccountId() : 0;
        if (ShouldSuppressBotResponse(playerAccountId, botGuid))
        {
            if (g_DebugEnabled)
                LOG_INFO("server.loading", "[Ollama Chat] Suppressing bot {} response — account {} opted out or muted this bot",
                         bot->GetName(), playerAccountId);
            continue;
        }

        // --- Bot promotion (promotion.h): a playerbot outside Gateway.BotGUIDs
        //     gets the strategic lane while the operator talks to it. A line
        //     ADDRESSES the bot when it is a whisper or names the bot as a whole
        //     word; only a whitelisted human's line opens or refreshes the window.
        //     Party guests are promoted by Promotion::Tick, not here.
        const bool configuredGatewayBot = IsGatewayBot(botGuid);
        bool promotedBot = false;
        if (!configuredGatewayBot && g_GatewayEnable && OllamaChat::Promotion::IsSourceAllowed(sourceLocal))
        {
            const bool addressed = (sourceLocal == SRC_WHISPER_LOCAL)
                                || OllamaChat::Promotion::MentionsName(msg, bot->GetName());
            if (addressed && !senderIsBot && OllamaChat::Promotion::IsOperator(player))
                promotedBot = OllamaChat::Promotion::TouchForChat(bot);
            else
                promotedBot = OllamaChat::Promotion::IsPromoted(botGuid);
        }
        const bool gatewayBot = configuredGatewayBot || promotedBot;

        // --- Gateway bot routing: this is the only player-chat LLM route. ---
        bool senderWhitelistedForGateway = true;
        bool gatewayChannelAllowed = configuredGatewayBot ? IsGatewaySourceAllowed(sourceLocal) : promotedBot;
        if (gatewayChannelAllowed && gatewayBot)
        {
            // Promoted bots: only the operator, never "empty whitelist = open".
            senderWhitelistedForGateway = configuredGatewayBot ? IsAccountAllowedForGateway(playerAccountId)
                                                               : OllamaChat::Promotion::IsOperator(player);
            if (!senderWhitelistedForGateway && g_DebugEnabled)
            {
                LOG_INFO("server.loading", "[Ollama Chat Gateway] Bot {} — sender '{}' account {} not whitelisted; legacy local-chat fallback is removed",
                         bot->GetName(), player->GetName(), playerAccountId);
            }
        }
        if (gatewayChannelAllowed && gatewayBot && senderWhitelistedForGateway)
        {
            // Gating layers (AND-combined):
            //   - private (whisper/party/raid/guild/officer): keyword required IF configured
            //   - non-whisper private (party/raid/guild/officer): when
            //     g_GatewayPrivateChannelMentionRequired=1, the message must also mention the
            //     bot's name. Without this, an empty TriggerKeyword would make the bot react
            //     to every line in party chat — bad UX, expensive.
            //   - public (say/yell/general): follows PublicChannelMode (keyword | mention | both)
            bool isPrivate     = IsPrivateChannel(sourceLocal);
            bool isWhisper     = (sourceLocal == SRC_WHISPER_LOCAL);
            bool isOtherPriv   = isPrivate && !isWhisper;
            const std::string& mode = g_GatewayPublicChannelMode;
            // Empty TriggerKeyword disables the keyword gate entirely — useful when the
            // operator wants the gateway to feel like a normal chatbot in whisper.
            bool keywordConfigured = !g_GatewayTriggerKeyword.empty();
            bool requireKeyword = keywordConfigured && (isPrivate || mode == "keyword" || mode == "both");
            bool requireMention = (!isPrivate && (mode == "mention" || mode == "both"))
                                || (isOtherPriv && g_GatewayPrivateChannelMentionRequired);

            // MentionExemptBots: whitelisted bot names skip the mention requirement in
            // non-whisper private channels. Lets agents like "Claude" reply to any party
            // line without needing their name typed every time. Keyword gate still applies.
            if (requireMention && isOtherPriv && !g_GatewayMentionExemptBotsSet.empty())
            {
                std::string nameLc = bot->GetName();
                std::transform(nameLc.begin(), nameLc.end(), nameLc.begin(),
                               [](unsigned char c){ return std::tolower(c); });
                if (g_GatewayMentionExemptBotsSet.count(nameLc))
                    requireMention = false;
            }

            // A promoted bot answers only lines that NAME it (or whisper it),
            // whatever PublicChannelMode says: the chance roll above can put an
            // un-named bot in finalCandidates, and during its window it would
            // otherwise answer every say/General line it happened to roll.
            // Whole-word match, so "Raz" does not fire on "crazy".
            bool promotedNotNamed = false;
            if (promotedBot && !isWhisper)
            {
                requireMention = false;
                promotedNotNamed = !OllamaChat::Promotion::MentionsName(msg, bot->GetName());
            }

            std::string strippedMessage = msg;
            bool gateOpen = true;

            if (requireKeyword)
            {
                if (!ExtractGatewayMessage(msg, g_GatewayTriggerKeyword, strippedMessage))
                    gateOpen = false;
            }
            if (gateOpen && requireMention && !MessageMentionsBot(msg, bot->GetName()))
                gateOpen = false;
            if (promotedNotNamed)
                gateOpen = false;

            if (gateOpen)
            {
                if (strippedMessage.empty())
                {
                    if (g_DebugEnabled)
                        LOG_INFO("server.loading", "[Ollama Chat Gateway] Bot {} — gate open but message empty after stripping", bot->GetName());
                    continue;
                }

                // Per-(bot, player) cooldown: protects gateway tokens from whisper spam.
                if (!TryClaimGatewayCooldown(botGuid, senderGuid))
                {
                    g_GatewayStats.totalRateLimited.fetch_add(1, std::memory_order_relaxed);
                    if (g_DebugEnabled)
                        LOG_INFO("server.loading", "[Ollama Chat Gateway] Bot {} dropping whisper from {} — cooldown active",
                                 bot->GetName(), player->GetName());
                    continue;
                }

                // Concurrency cap: avoid stampeding the gateway when many bots match at once.
                if (!TryAcquireGatewaySlot())
                {
                    g_GatewayStats.totalRateLimited.fetch_add(1, std::memory_order_relaxed);
                    if (g_DebugEnabled)
                        LOG_INFO("server.loading", "[Ollama Chat Gateway] Bot {} dropping whisper — concurrency cap reached ({} active)",
                                 bot->GetName(), g_GatewayMaxConcurrentRequests);
                    continue;
                }

                if (g_DebugEnabled)
                    LOG_INFO("server.loading", "[Ollama Chat Gateway] Bot {} routing to gateway: '{}'", bot->GetName(), strippedMessage);

                ChatChannelSourceLocal capturedSource = sourceLocal;
                uint32_t auditAccountId = playerAccountId;

                std::thread([botGuid, senderGuid, strippedMessage, msg, capturedSource, auditAccountId]() {
                    struct SlotGuard
                    {
                        ~SlotGuard() { ReleaseGatewaySlot(); }
                    } slotGuard;

                    try {
                        // Helper: send one chunk to the right channel for this request, re-acquiring
                        // pointers each time (the world thread may have invalidated them).
                        auto sendWhisper = [&](const std::string& chunk) -> bool {
                            Player* botPtr = ObjectAccessor::FindPlayer(ObjectGuid(botGuid));
                            if (!botPtr) return false;
                            PlayerbotAI* botAI = PlayerbotsMgr::instance().GetPlayerbotAI(botPtr);
                            if (!botAI) return false;
                            Player* senderPtr = ObjectAccessor::FindPlayer(ObjectGuid(senderGuid));
                            if (!senderPtr) return false;

                            switch (capturedSource)
                            {
                                case SRC_PARTY_LOCAL:   botAI->SayToParty(chunk); break;
                                case SRC_RAID_LOCAL:    botAI->SayToRaid(chunk); break;
                                case SRC_GUILD_LOCAL:
                                case SRC_OFFICER_LOCAL: botAI->SayToGuild(chunk); break;
                                case SRC_SAY_LOCAL:     botAI->Say(chunk); break;
                                case SRC_YELL_LOCAL:    botAI->Yell(chunk); break;
                                case SRC_WHISPER_LOCAL:
                                default:                botAI->Whisper(chunk, senderPtr->GetName()); break;
                            }
                            return true;
                        };

                        std::string response;
                        size_t streamedChunks = 0;
                        auto t0 = std::chrono::steady_clock::now();

                        // Only send request-body tools[] to Synthiq — OpenClaw ignores them
                        // (it has its own baked-in agent toolset). Streaming + tools remain
                        // mutually exclusive; the SSE parser doesn't accumulate tool_calls.
                        std::string gatewayType = ResolveGatewayType(botGuid);
                        bool useTools = g_GatewayEnableToolUse && gatewayType == "synthiq";

                        // Cheap-path intent classifier: ask local Ollama to pick
                        // an allowlisted tool for short gameplay commands. Empty
                        // result falls through to the paid gateway.
                        response = TryGatewayIntentClassify(botGuid, senderGuid, strippedMessage);

                        if (!response.empty())
                        {
                            // Classifier handled it. Skip the paid gateway call entirely.
                        }
                        else if (useTools)
                        {
                            response = QueryGatewayAPIWithTools(botGuid, senderGuid, strippedMessage);
                        }
                        else if (g_GatewayUseStreaming)
                        {
                            // Streaming path: emit each delta-bounded chunk as soon as it arrives.
                            response = QueryGatewayAPIStreaming(botGuid, senderGuid, strippedMessage,
                                [&](const std::string& chunk) -> bool {
                                    if (!sendWhisper(chunk)) return false;
                                    ++streamedChunks;
                                    // Brief pause so consecutive whispers don't clump in the client.
                                    std::this_thread::sleep_for(std::chrono::milliseconds(250));
                                    return true;
                                });
                        }
                        else
                        {
                            response = QueryGatewayAPI(botGuid, senderGuid, strippedMessage);
                        }

                        auto latencyMs = std::chrono::duration_cast<std::chrono::milliseconds>(
                            std::chrono::steady_clock::now() - t0).count();
                        const char* sourceLabel =
                            (capturedSource >= 0 && capturedSource < 18)
                            ? ChatChannelSourceLocalStr[capturedSource] : "Unknown";

                        if (response.empty())
                        {
                            WriteGatewayAuditRecord(botGuid, senderGuid, auditAccountId,
                                                    static_cast<uint32_t>(strippedMessage.size()),
                                                    0, 0, 0,
                                                    static_cast<uint32_t>(latencyMs),
                                                    sourceLabel, true);
                            if (g_DebugEnabled)
                                LOG_INFO("server.loading", "[Ollama Chat Gateway] Bot {} got empty response", botGuid);
                            return;
                        }

                        // Action markers ([emote:bow] etc.) are stripped from the user-visible text
                        // and dispatched separately. No-op when EnableActionMarkers = 0.
                        std::vector<GatewayAction> actions;
                        if (g_GatewayEnableActionMarkers)
                            actions = ExtractActionMarkers(response);

                        // Non-streaming path (or fallback after streaming failure): chunk + send now.
                        if (streamedChunks == 0)
                        {
                            if (g_EnableTypingSimulation)
                            {
                                uint32_t delay = g_TypingSimulationBaseDelay + (response.length() * g_TypingSimulationDelayPerChar);
                                if (g_DebugEnabled)
                                    LOG_INFO("server.loading", "[Ollama Chat Gateway] Bot {} typing delay: {}ms", botGuid, delay);
                                std::this_thread::sleep_for(std::chrono::milliseconds(delay));
                            }

                            std::vector<std::string> chunks = SplitForWhisper(response, g_GatewayMaxWhisperChunkChars);
                            for (size_t ci = 0; ci < chunks.size(); ++ci)
                            {
                                if (ci > 0)
                                    std::this_thread::sleep_for(std::chrono::milliseconds(400));
                                if (!sendWhisper(chunks[ci])) return;
                            }

                            if (g_DebugEnabled)
                                LOG_INFO("server.loading", "[Ollama Chat Gateway] Bot {} whispered ({} chunks, buffered)",
                                         botGuid, chunks.size());
                        }
                        else if (g_DebugEnabled)
                        {
                            LOG_INFO("server.loading", "[Ollama Chat Gateway] Bot {} streamed {} chunks ({} chars)",
                                     botGuid, streamedChunks, response.size());
                        }

                        AppendBotConversation(botGuid, senderGuid, msg, response);

                        for (const auto& a : actions)
                            DispatchGatewayAction(botGuid, a);

                        WriteGatewayAuditRecord(botGuid, senderGuid, auditAccountId,
                                                static_cast<uint32_t>(strippedMessage.size()),
                                                static_cast<uint32_t>(response.size()),
                                                0, 0,
                                                static_cast<uint32_t>(latencyMs),
                                                sourceLabel, false);
                    }
                    catch (const std::exception& ex)
                    {
                        if (g_DebugEnabled)
                            LOG_ERROR("server.loading", "[Ollama Chat Gateway] Exception in gateway thread: {}", ex.what());
                    }
                }).detach();
                continue;
            }
            else if (!g_GatewayFallbackToOllama)
            {
                if (g_DebugEnabled)
                    LOG_INFO("server.loading", "[Ollama Chat Gateway] Bot {} — no trigger keyword '{}' in message, no fallback", bot->GetName(), g_GatewayTriggerKeyword);
                continue;
            }
            if (g_DebugEnabled)
            {
                LOG_INFO("server.loading",
                         "[Ollama Chat Gateway] Bot {} — Gateway.FallbackToOllama is deprecated/no-op; legacy local-chat fallback has been removed",
                         bot->GetName());
            }
            // Personality and ambient reactions now live in Tactical Ambient Mode.
            continue;
        }

        // Legacy direct local Ollama replies were removed. Ordinary chat only
        // reaches gateway/classifier bots above; proactive life comes from
        // Tactical Ambient Mode.
        continue;

    }
}

static bool IsBotEligibleForChatChannelLocal(Player* bot, Player* player, ChatChannelSourceLocal source, Channel* channel, Player* receiver)
{
    if (!bot || !player || bot == player)
    {
        if (g_DebugEnabled)
            LOG_INFO("server.loading", "[Ollama Chat] IsBotEligible: FAILED basic check - bot={}, player={}, same={}", 
                    (void*)bot, (void*)player, (bot == player));
        return false;
    }
    if (!PlayerbotsMgr::instance().GetPlayerbotAI(bot))
    {
        if (g_DebugEnabled)
            LOG_INFO("server.loading", "[Ollama Chat] IsBotEligible: Bot {} FAILED - no PlayerbotAI", bot->GetName());
        return false;
    }
        
    // For whispers, only the specific receiver should respond
    if (source == SRC_WHISPER_LOCAL)
    {
        // Don't allow bot-to-bot whisper responses
        PlayerbotAI* senderAI = PlayerbotsMgr::instance().GetPlayerbotAI(player);
        if (senderAI && senderAI->IsBotAI())
        {
            return false;
        }
        
        return (receiver && bot == receiver);
    }
    
    // Check team compatibility for non-proximity chats (except channels which can be cross-faction)
    // Say and Yell are proximity-based and don't require same faction
    bool isProximityChatSource = (source == SRC_SAY_LOCAL || source == SRC_YELL_LOCAL);
    if (!channel && !isProximityChatSource && bot->GetTeamId() != player->GetTeamId())
        return false;
    
    // For channels, check if bot is in the specific channel instance
    if (channel)
    {
        // Verify the channel is valid before proceeding
        if (!channel)
        {
            if(g_DebugEnabled)
            {
                LOG_ERROR("server.loading", "[Ollama Chat] IsBotEligibleForChatChannelLocal: Channel is null");
            }
            return false;
        }
            
        // ONLY use exact channel instance check - NO Player::IsInChannel() anymore
        ChannelMgr* candidateCMgr = ChannelMgr::forTeam(bot->GetTeamId());
        if (!candidateCMgr)
            return false;
            
        Channel* candidateChannel = candidateCMgr->GetChannel(channel->GetName(), bot);
        // Verify both channels are valid and are the exact same instance
        if (!candidateChannel || candidateChannel != channel)
        {
            if(g_DebugEnabled)
            {
                LOG_INFO("server.loading", "[Ollama Chat] IsBotEligibleForChatChannelLocal: Bot {} not in same channel instance '{}' - Bot team: {}, Channel ptr: {} vs {}", 
                        bot->GetName(), channel->GetName(), (int)bot->GetTeamId(),
                        (void*)candidateChannel, (void*)channel);
            }
            return false;
        }
        
        // Additional team check for cross-faction channels - only allow same faction unless it's a global channel
        if (bot->GetTeamId() != player->GetTeamId())
        {
            // Allow cross-faction only for specific global channels
            bool isGlobalChannel = (channel->GetName().find("World") != std::string::npos || 
                                   channel->GetName().find("LookingForGroup") != std::string::npos);
            if (!isGlobalChannel)
            {
                if(g_DebugEnabled)
                {
                    LOG_INFO("server.loading", "[Ollama Chat] IsBotEligibleForChatChannelLocal: Bot {} different faction from player - Bot: {}, Player: {}, Channel: '{}'", bot->GetName(), (int)bot->GetTeamId(), (int)player->GetTeamId(), channel->GetName());
                }
                return false;
            }
        }
    }
    
    bool isInParty = (player->GetGroup() && bot->GetGroup() && (player->GetGroup() == bot->GetGroup()));
    float threshold = 0.0f;
    
    switch (source)
    {
        case SRC_SAY_LOCAL:    
            threshold = g_SayDistance;
            if (threshold > 0.0f)
            {
                if (!bot->IsInWorld() || !player->IsInWorld())
                    return false;
                    
                float distance = bot->GetDistance(player);
                return distance <= threshold;
            }
            return false;
            
        case SRC_YELL_LOCAL:   
            threshold = g_YellDistance;
            return (threshold > 0.0f && player->GetDistance(bot) <= threshold);
            
        case SRC_GUILD_LOCAL:
        case SRC_OFFICER_LOCAL:
            return (player->GetGuild() && bot->GetGuildId() == player->GetGuildId());
            
        case SRC_PARTY_LOCAL:
        case SRC_RAID_LOCAL:
            return isInParty;
            
        case SRC_WHISPER_LOCAL:
            // For whispers, the bot should only respond if it's the specific receiver
            return (receiver && bot == receiver);
            
        case SRC_GENERAL_LOCAL:
            // For channels like General, Trade, etc., no distance check - only channel membership matters
            // Channel membership was already checked above
            return true;
            
        default:
            return false;
    }
}
