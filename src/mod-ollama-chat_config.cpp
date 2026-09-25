#include "mod-ollama-chat_config.h"
#include "mod-ollama-chat_sentiment.h"
#include "mod-ollama-chat_rag.h"
#include "mod-ollama-chat_gateway.h"
#include "mod-ollama-chat_playerprefs.h"
#include "mod-ollama-chat_mcpserver.h"
#include "mod-ollama-chat_leadertick.h"
#include "mod-ollama-chat_tactical.h"
#include "mod-ollama-chat_overlord.h"
#include "mod-ollama-chat_fleet.h"
#include "mod-ollama-chat_promotion.h"
#include "mod-ollama-chat_worldtask.h"
#include "mod-ollama-chat_personality.h"
#include "mod-ollama-chat_proactive.h"
#include "mod-ollama-chat_jev.h"
#include "Config.h"
#include "Log.h"
#include "mod-ollama-chat_api.h"
#include <fmt/core.h>
#include <sstream>
#include <fstream>
#include <algorithm>
#include <cctype>
#include <cstring>
#include <limits>
#include <nlohmann/json.hpp>

namespace
{
constexpr char kLegacyModulePromptPrefix[] = "../../../modules/mod-ollama-chat/";
constexpr char kModulePromptPrefix[] = "modules/mod-ollama-chat/";
constexpr char kAzerothCoreModulePromptPrefix[] = "/azerothcore/modules/mod-ollama-chat/";
constexpr char kDefaultGatewayLeaderPromptFile[] = "modules/mod-ollama-chat/prompts/gateway_leader.md";
constexpr char kDefaultTacticalPromptFile[] = "modules/mod-ollama-chat/prompts/ollama_tactical.md";
constexpr char kDefaultClassifierPromptFile[] = "modules/mod-ollama-chat/prompts/ollama_classifier.md";
constexpr char kDefaultTacticalRequireConfirmationFor[] =
    "bot_accept_quest,bot_abandon_quest,bot_choose_quest_reward,"
    "bot_equip_item,bot_unequip_item,bot_use_item,bot_destroy_item,"
    "bot_set_home,bot_bank_deposit,bot_bank_withdraw,"
    "bot_guild_bank_deposit,bot_guild_bank_withdraw,bot_mail_send,bot_yell";
}


// --------------------------------------------
// Distance/Range Configuration
// --------------------------------------------
float      g_SayDistance       = 30.0f;
float      g_YellDistance      = 100.0f;
float      g_RandomChatterRealPlayerDistance = 40.0f;
float      g_EventChatterRealPlayerDistance = 40.0f;

// --------------------------------------------
// Bot/Player Chatter Probability & Limits
// --------------------------------------------
// Per-channel-type reply chances
uint32_t   g_PlayerReplyChance_Say     = 90;
uint32_t   g_BotReplyChance_Say        = 10;
uint32_t   g_PlayerReplyChance_Channel = 50;
uint32_t   g_BotReplyChance_Channel    = 5;
uint32_t   g_PlayerReplyChance_Party   = 90;
uint32_t   g_BotReplyChance_Party      = 10;
uint32_t   g_PlayerReplyChance_Guild   = 70;
uint32_t   g_BotReplyChance_Guild      = 5;

uint32_t   g_MaxBotsToPick     = 2;
uint32_t   g_RandomChatterBotCommentChance   = 5;
uint32_t   g_RandomChatterMaxBotsPerPlayer   = 2;
uint32_t   g_EventChatterBotCommentChance    = 15;
uint32_t   g_EventChatterBotSelfCommentChance = 5;
uint32_t   g_EventChatterMaxBotsPerPlayer    = 2;

// --------------------------------------------
// Ollama LLM API Configuration
// --------------------------------------------
std::string g_OllamaUrl        = "http://localhost:11434/api/generate";
std::string g_OllamaModel      = "llama3.2:1b";
uint32_t    g_OllamaNumPredict = 40;
float       g_OllamaTemperature = 0.8f;
float       g_OllamaTopP = 0.95f;
float       g_OllamaRepeatPenalty = 1.1f;
uint32_t    g_OllamaNumCtx = 0;
uint32_t    g_OllamaNumThreads = 0;
std::string g_OllamaStop = "";
std::string g_OllamaSystemPrompt = "";
std::string g_OllamaSeed = "";

// --------------------------------------------
// Concurrency/Queueing
// --------------------------------------------
uint32_t    g_MaxConcurrentQueries = 0;

// --------------------------------------------
// Feature Toggles & Core Settings
// --------------------------------------------
bool        g_Enable                          = true;
bool        g_DisableRepliesInCombat          = true;
bool        g_DisableOllamaResponses          = false;
bool        g_EnableRandomChatter             = true;
bool        g_EnableEventChatter              = true;
bool        g_EnableRPPersonalities           = false;
bool        g_EnableWhisperReplies            = false;
bool        g_DebugEnabled                    = false;
bool        g_DebugShowFullPrompt             = false;

// --------------------------------------------
// Think Mode Support
// --------------------------------------------
bool g_ThinkModeEnableForModule = false;

// --------------------------------------------
// Random Chatter Timing
// --------------------------------------------
uint32_t    g_MinRandomInterval               = 45;
uint32_t    g_MaxRandomInterval               = 180;

// --------------------------------------------
// Conversation History Settings
// --------------------------------------------
uint32_t    g_MaxConversationHistory          = 5;
uint32_t    g_ConversationHistorySaveInterval = 10;

// --------------------------------------------
// Prompt Templates
// --------------------------------------------
std::string g_RandomChatterPromptTemplate;
std::vector<std::string> g_RandomChatterPromptVariations;
std::vector<std::string> g_RandomChatterQuestionVariations;
std::string g_EventChatterPromptTemplate;
std::string g_ChatPromptTemplate;
std::string g_ChatExtraInfoTemplate;

// --------------------------------------------
// Personality and Prompt Data
// --------------------------------------------
std::unordered_map<uint64_t, std::string> g_BotPersonalityList;
std::unordered_map<std::string, std::string> g_PersonalityPrompts;
std::vector<std::string> g_PersonalityKeys;
std::vector<std::string> g_PersonalityKeysRandomOnly;
std::string g_DefaultPersonalityPrompt;

// --------------------------------------------
// Chat History Templates and Toggles
// --------------------------------------------
bool        g_EnableChatHistory = true;
std::string g_ChatHistoryHeaderTemplate;
std::string g_ChatHistoryLineTemplate;
std::string g_ChatHistoryFooterTemplate;

// --------------------------------------------
// Chatbot Snapshot Template
// --------------------------------------------
bool        g_EnableChatBotSnapshotTemplate  = false;
std::string g_ChatBotSnapshotTemplate;

// --------------------------------------------
// Conversation History Store and Mutex
// --------------------------------------------
std::unordered_map<uint64_t, std::unordered_map<uint64_t, std::deque<std::pair<std::string, std::string>>>> g_BotConversationHistory;
std::mutex g_ConversationHistoryMutex;
time_t g_LastHistorySaveTime = 0;

// --------------------------------------------
// Bot-Player Sentiment Tracking System
// --------------------------------------------
bool        g_EnableSentimentTracking = true;
float       g_SentimentDefaultValue = 0.5f;              // Default sentiment value (0.5 = neutral)
float       g_SentimentAdjustmentStrength = 0.1f;        // How much to adjust sentiment per message
uint32_t    g_SentimentSaveInterval = 10;                // How often to save sentiment to DB (minutes)
std::string g_SentimentAnalysisPrompt = "Analyze the sentiment of this message: \"{message}\". Respond only with: POSITIVE, NEGATIVE, or NEUTRAL.";
std::string g_SentimentPromptTemplate = "Your relationship sentiment with {player_name} is {sentiment_value} (0.0=hostile, 0.5=neutral, 1.0=friendly). Use this to guide your tone and response.";

// In-memory sentiment storage and mutex
std::unordered_map<uint64_t, std::unordered_map<uint64_t, float>> g_BotPlayerSentiments;
std::mutex g_SentimentMutex;
time_t g_LastSentimentSaveTime = 0;

// --------------------------------------------
// RAG (Retrieval-Augmented Generation) System
// --------------------------------------------
bool        g_EnableRAG = false;
std::string g_RAGDataPath = "rag/";
uint32_t    g_RAGMaxRetrievedItems = 3;
float       g_RAGSimilarityThreshold = 0.3f;
std::string g_RAGPromptTemplate;

class OllamaRAGSystem;
OllamaRAGSystem* g_RAGSystem = nullptr;

// --------------------------------------------
// Blacklist: Prefixes for Commands (not chat)
// --------------------------------------------
std::vector<std::string> g_BlacklistCommands = {
    ".playerbots",
    "playerbot",
};

// --------------------------------------------
// Environment/Contextual Random Chatter Templates
// --------------------------------------------
std::vector<std::string> g_EnvCommentCreature;
std::vector<std::string> g_EnvCommentGameObject;
std::vector<std::string> g_EnvCommentEquippedItem;
std::vector<std::string> g_EnvCommentBagItem;
std::vector<std::string> g_EnvCommentBagItemSell;
std::vector<std::string> g_EnvCommentSpell;
std::vector<std::string> g_EnvCommentQuestArea;
std::vector<std::string> g_EnvCommentVendor;
std::vector<std::string> g_EnvCommentQuestgiver;
std::vector<std::string> g_EnvCommentBagSlots;
std::vector<std::string> g_EnvCommentDungeon;
std::vector<std::string> g_EnvCommentUnfinishedQuest;

// --------------------------------------------
// Guild-Specific Random Chatter Templates
// --------------------------------------------
std::vector<std::string> g_GuildEnvCommentGuildMember;
std::vector<std::string> g_GuildEnvCommentGuildRank;
std::vector<std::string> g_GuildEnvCommentGuildBank;
std::vector<std::string> g_GuildEnvCommentGuildMOTD;
std::vector<std::string> g_GuildEnvCommentGuildInfo;
std::vector<std::string> g_GuildEnvCommentGuildOnlineMembers;
std::vector<std::string> g_GuildEnvCommentGuildRaid;
std::vector<std::string> g_GuildEnvCommentGuildEndgame;
std::vector<std::string> g_GuildEnvCommentGuildStrategy;
std::vector<std::string> g_GuildEnvCommentGuildGroup;
std::vector<std::string> g_GuildEnvCommentGuildPvP;
std::vector<std::string> g_GuildEnvCommentGuildCommunity;

// --------------------------------------------
// Guild-Specific Random Chatter Configuration
// --------------------------------------------
bool        g_EnableGuildEventChatter             = true;
bool        g_EnableGuildRandomAmbientChatter      = true;
uint32_t    g_GuildRandomChatterChance             = 10;
uint32_t    g_GuildChatterBotCommentChance          = 25;
uint32_t    g_GuildChatterMaxBotsPerEvent           = 2;

// --------------------------------------------
// Guild-Specific Event Chatter Templates
// --------------------------------------------
std::string g_GuildEventTypeLevelUp = "";
std::string g_GuildEventTypeDungeonComplete = "";
std::string g_GuildEventTypeEpicGear = "";
std::string g_GuildEventTypeRareGear = "";
std::string g_GuildEventTypeGuildJoin = "";
std::string g_GuildEventTypeGuildLeave = "";
std::string g_GuildEventTypeGuildPromotion = "";
std::string g_GuildEventTypeGuildDemotion = "";
std::string g_GuildEventTypeGuildLogin = "";
std::string g_GuildEventTypeGuildAchievement = "";

// --------------------------------------------
// Event Chatter Templates
// --------------------------------------------
std::string g_EventTypeDefeated;           // "defeated"
std::string g_EventTypeDefeatedPlayer;     // "defeated player"
std::string g_EventTypePetDefeated;        // "pet defeated"
std::string g_EventTypeGotItem;            // "got item"
std::string g_EventTypeDied;               // "died"
std::string g_EventTypeCompletedQuest;     // "completed quest"
std::string g_EventTypeLearnedSpell;       // "learned spell"
std::string g_EventTypeRequestedDuel;      // "requested to duel"
std::string g_EventTypeStartedDueling;     // "started dueling"
std::string g_EventTypeWonDuel;            // "won duel against"
std::string g_EventTypeLeveledUp;          // "leveled up"
std::string g_EventTypeAchievement;        // "earned achievement"
std::string g_EventTypeUsedObject;         // "used object"

// Chance variables for normal events
int g_EventTypeDefeated_Chance = 0;
int g_EventTypeDefeatedPlayer_Chance = 0;
int g_EventTypePetDefeated_Chance = 0;
int g_EventTypeGotItem_Chance = 0;
int g_EventTypeDied_Chance = 0;
int g_EventTypeCompletedQuest_Chance = 0;
int g_EventTypeLearnedSpell_Chance = 0;
int g_EventTypeRequestedDuel_Chance = 0;
int g_EventTypeStartedDueling_Chance = 0;
int g_EventTypeWonDuel_Chance = 0;
int g_EventTypeLeveledUp_Chance = 0;
int g_EventTypeAchievement_Chance = 0;
int g_EventTypeUsedObject_Chance = 0;

// Chance variables for guild events
int g_GuildEventTypeEpicGear_Chance = 0;
int g_GuildEventTypeRareGear_Chance = 0;
int g_GuildEventTypeGuildJoin_Chance = 0;
int g_GuildEventTypeGuildLogin_Chance = 0;
int g_GuildEventTypeGuildLeave_Chance = 0;
int g_GuildEventTypeGuildPromotion_Chance = 0;
int g_GuildEventTypeGuildDemotion_Chance = 0;
int g_GuildEventTypeGuildAchievement_Chance = 0;
int g_GuildEventTypeLevelUp_Chance = 0;
int g_GuildEventTypeDungeonComplete_Chance = 0;

// Event Cooldown
uint32_t g_EventCooldownTime = 10;

// --------------------------------------------
// Channel Disable Settings
// --------------------------------------------
bool g_DisableForCustomChannels = false;
bool g_DisableForSayYell = false;
bool g_DisableForGuild = false;
bool g_DisableForParty = false;

// --------------------------------------------
// Typing Simulation Settings
// --------------------------------------------
bool g_EnableTypingSimulation = false;
uint32_t g_TypingSimulationBaseDelay = 1000;     // 1000ms base delay
uint32_t g_TypingSimulationDelayPerChar = 250;   // 250ms per character (4 chars/sec)

// --------------------------------------------
// Gateway Agent Bot Configuration
// --------------------------------------------
bool        g_GatewayEnable           = false;
std::string g_GatewayUrl              = "";
std::string g_GatewayBearerToken      = "";
std::string g_GatewayTriggerKeyword   = "claude";
std::string g_GatewaySystemPrompt     = "";
std::string g_GatewayModel            = "openclaw";
std::string g_GatewayScopes           = "operator.admin,operator.read,operator.write";
bool        g_GatewayFallbackToOllama = false;
uint32_t    g_GatewayMaxHistory       = 10;
std::string g_GatewayType             = "openclaw";

uint32_t    g_GatewayTimeoutSeconds            = 300;
bool        g_GatewayFallbackOnError           = false;
uint32_t    g_GatewayMaxConcurrentRequests     = 4;
uint32_t    g_GatewayMinSecondsBetweenRequests = 5;
std::string g_GatewayUserPrefix                = "wow-bot-";
uint32_t    g_GatewayMaxWhisperChunkChars      = 250;
bool        g_GatewayUseSessionPersistence     = false;
std::string g_GatewaySessionKeyTemplate        = "wow-{botGuid}-{playerGuid}";
bool        g_GatewaySkipHistoryWhenSession    = true;
bool        g_GatewayUseStreaming              = false;
bool        g_GatewayStreamWhisperOnSentence   = true;

std::string g_GatewayAgentByPersonality        = "";
std::unordered_map<std::string, std::string> g_GatewayAgentByPersonalityMap;
bool        g_GatewayMergePersonalityPrompt    = true;
bool        g_GatewayDetectLanguage            = false;
std::string g_GatewayLanguageHintTemplate      = "Always respond to the user in {lang}.";

std::string g_GatewayAllowedChannels           = "whisper";
std::unordered_set<int> g_GatewayAllowedChannelsSet;
std::string g_GatewayPublicChannelMode         = "keyword";
bool        g_GatewayPrivateChannelMentionRequired = true;
std::string g_GatewayMentionExemptBots         = "";
std::unordered_set<std::string> g_GatewayMentionExemptBotsSet;

bool        g_GatewayAutoClaimOnLogin          = false;
std::string g_GatewayAutoClaimAccountIds       = "";
std::unordered_set<uint32_t> g_GatewayAutoClaimAccountIdsSet;

bool        g_GatewayEnableToolUse             = false;
uint32_t    g_GatewayMaxToolIterations         = 3;
std::string g_GatewayAllowedTools              = "";
std::vector<std::string> g_GatewayAllowedToolsList;
std::string g_GatewayToolFacadeGroups          = "*";
std::vector<std::string> g_GatewayToolFacadeGroupList;

bool        g_GatewayEnableAudit               = false;
uint32_t    g_GatewayAuditRetentionDays        = 30;
bool        g_GatewayEnableActionMarkers       = false;

bool        g_McpEnable                          = false;
std::string g_McpBindAddress                     = "0.0.0.0";
uint32_t    g_McpPort                            = 18790;
std::string g_McpBearerToken                     = "";
bool        g_McpInjectContextHint               = true;
bool        g_McpAllowActionTools                = false;
uint32_t    g_McpActionRateLimitPerBotPerMinute  = 6;
std::string g_McpAllowedToolsExtra               = "";
bool        g_McpAllowGatewayInjection           = false;

// ops-api proxy
bool        g_OpsEnable                          = false;
std::string g_OpsUrl                             = "http://host.docker.internal:18791";
std::string g_OpsBearerToken                     = "";
int         g_OpsTimeoutSeconds                  = 10;
// Tier 9 — leader bot
bool        g_McpLeaderEnable                    = false;
std::string g_McpLeaderBotGUIDs                  = "";
std::unordered_set<uint64_t> g_McpLeaderBotGUIDSet;
std::string g_McpLeaderScope                     = "group";
float       g_McpLeaderNearbyRadius              = 60.0f;
std::string g_McpLeaderDeniedCommands            = "";
std::vector<std::string> g_McpLeaderDeniedCommandsList;
uint32_t    g_McpLeaderRateLimitPerMinute        = 30;
bool        g_McpLeaderInjectPromptHint          = true;
std::string g_McpLeaderSystemPromptFile          = kDefaultGatewayLeaderPromptFile;
std::string g_McpLeaderSystemPromptText          = "";

// Tier 10 — autonomous tick
bool        g_McpLeaderAutoTickEnable            = false;
uint32_t    g_McpLeaderAutoTickIntervalSeconds   = 15;
uint32_t    g_McpLeaderAutoTickMaxPerHour        = 20;

// Tier 9.1 — GM admin commands via leader_admin_command
bool        g_McpLeaderAllowAdminCommands        = false;
std::string g_McpLeaderAllowedAdminCommands      = "add,addclass,addaccount,remove,list";
std::string g_McpGmAllowedCommands = "lookup,pinfo,tele name,server info";
std::vector<std::string> g_McpGmAllowedCommandsList;
std::vector<std::string> g_McpLeaderAllowedAdminCommandsList;

// Phase 1 — Overlord headless auto-login
bool        g_OverlordEnable                     = false;
uint32_t    g_OverlordCharacterGuid              = 0;
uint32_t    g_OverlordStartupDelaySeconds        = 30;
std::string g_OverlordAutoSpawnBots              = "";
std::vector<uint32_t> g_OverlordAutoSpawnBotList;

// Phase 1.2 — Fallback master for leader_admin_command
uint32_t    g_McpLeaderSystemMasterGuid          = 0;

// Fleet bootstrap — robot-first party/master reconciliation
bool        g_FleetEnsureParty                   = false;
uint32_t    g_FleetLeaderGuid                    = 0;
std::string g_FleetMemberGuids                   = "";
std::vector<uint32_t> g_FleetMemberGuidList;
uint32_t    g_FleetEnsureIntervalSec             = 20;

// Bot promotion — see promotion.h
bool        g_PromoteEnable                      = false;
uint32_t    g_PromoteChatTtlSec                  = 600;
uint32_t    g_PromoteMaxChatBots                 = 5;
bool        g_PromotePartyEnable                 = true;
uint32_t    g_PromoteTemplateBotGuid             = 0;
std::string g_PromoteChannels                    = "whisper,party,raid,guild,officer,say,yell,general";

// MCP config-edit tools
bool        g_McpConfigEditEnable                = false;
std::string g_McpConfigEditConfigRoot            = "";
std::string g_McpConfigEditAllowedFiles          = "";
std::unordered_set<std::string> g_McpConfigEditAllowedFilesSet;
std::string g_McpConfigEditAllowedKeys           = "";
std::unordered_map<std::string, std::vector<std::string>> g_McpConfigEditAllowedKeysMap;
std::string g_McpConfigEditDeniedKeys            = "*Token*,*Password*,*Secret*,*BearerToken*,*ApiKey*,*DatabaseInfo*";
std::vector<std::string> g_McpConfigEditDeniedKeysList;
std::string g_McpConfigEditBackupDir             = "";
uint32_t    g_McpConfigEditMaxFileSizeBytes      = 1048576;
uint32_t    g_McpConfigEditReloadAdminSessionGuid = 0;

// --------------------------------------------
// Tactical (companion-mode) agent layer
// --------------------------------------------
bool        g_TacticalEnable                     = true;
std::string g_TacticalBotGUIDs                   = "";
std::unordered_set<uint64_t> g_TacticalBotGUIDSet;
std::string g_TacticalUrl                        = "http://host.docker.internal:11434/api/generate";
std::string g_TacticalModel                      = "qwen3:14b";
std::string g_TacticalBotOverridesJson           = "";
std::string g_TacticalBotOverridesJsonFile       = "";
std::unordered_map<uint64_t, TacticalBotConfig> g_TacticalBotConfigs;
bool        g_TacticalHumanPresenceRequired      = true;
uint32_t    g_TacticalHeartbeatMs                = 10000;
uint32_t    g_TacticalBotCooldownMs              = 2000;
uint32_t    g_TacticalSnapshotCacheMs            = 500;
uint32_t    g_TacticalMaxConcurrentQueries       = 1;
uint32_t    g_TacticalMaxPerBotPerHour           = 600;
uint32_t    g_TacticalRequestTimeoutMs           = 5000;
uint32_t    g_TacticalNumCtx                     = 2048;
std::string g_TacticalSystemPromptFile           = kDefaultTacticalPromptFile;
std::string g_TacticalSystemPromptText           = "";
uint32_t    g_TacticalMinWhisperGapSec           = 120;
bool        g_TacticalAmbientEnable              = true;
uint32_t    g_TacticalAmbientMinSpeechGapSec     = 60;
uint32_t    g_TacticalAmbientMinEmoteGapSec      = 45;
uint32_t    g_TacticalAmbientMaxVisibleActionsPerMinute = 2;
uint32_t    g_TacticalAmbientEventReactionChance = 35;
bool        g_TacticalAutoEnrollNearbyBots       = true;
uint32_t    g_TacticalNearbyBotMax               = 5;
float       g_TacticalNearbyBotRadius            = 60.0f;
bool        g_GatewayOllamaClassifierEnable       = true;
std::string g_GatewayOllamaClassifierUrl          = "";
std::string g_GatewayOllamaClassifierModel        = "qwen3:8b";
uint32_t    g_GatewayOllamaClassifierTimeoutMs    = 5000;
std::string g_OpenAiCompatApiKey                  = "";
uint32_t    g_GatewayOllamaClassifierNumCtx       = 2048;
std::string g_GatewayOllamaClassifierSystemPromptFile = kDefaultClassifierPromptFile;
std::string g_GatewayOllamaClassifierSystemPromptText = "";
float       g_GatewayOllamaClassifierMinConfidence = 0.8f;
std::atomic<uint32_t> g_GatewayClassifierMaxWords{0};

// jev decision tier (OllamaChat.Jev.*)
bool        g_JevEnable                 = false;
std::string g_JevUrl                    = "https://api.typesafe.ai/v1/systemone";
std::string g_JevModel                  = "jev-1.13.0";
std::string g_JevApiKey                 = "";
std::string g_JevProxy                  = "";
std::string g_JevProxyUser              = "";
std::string g_JevProxyPassword          = "";
uint32_t    g_JevTimeoutMs              = 1500;
uint32_t    g_JevMaxConcurrent          = 2;
uint32_t    g_JevBreakerCooldownSec     = 60;
std::string g_JevQuestionsFile          = "modules/mod-ollama-chat/prompts/jev_questions.json";
bool        g_JevClassifierEnable       = false;
float       g_JevClassifierMinConfidence = 0.8f;
bool        g_JevIntentEnable           = false;
float       g_JevIntentMinConfidence    = 0.8f;
bool        g_JevTickGateEnable         = false;
float       g_JevTickGateMinConfidence  = 0.6f;
bool        g_JevPlannerEnable          = false;
float       g_JevPlannerMinConfidence   = 0.6f;
bool        g_JevTacticalEnable          = false;
float       g_JevTacticalMinConfidence  = 0.6f;
float       g_JevTacticalEscalateMin    = 0.8f;
bool        g_JevPlaybookEnable          = false;
float       g_JevPlaybookMinRelevance    = 0.3f;
uint32_t    g_JevPlaybookMinRows         = 4;
uint32_t    g_JevPlaybookMaxRows         = 12;
uint32_t    g_JevPlaybookTimeoutMs       = 4000;
uint32_t    g_TacticalPromptMaxBytes             = 2048;
bool        g_TacticalEnableAudit                = true;
uint32_t    g_TacticalAuditRetentionDays         = 7;
bool        g_TacticalAllowEscalation            = true;
uint32_t    g_TacticalDirectiveMaxTtlSec         = 600;
uint32_t    g_TacticalIdlePauseSec               = 300;
bool        g_TacticalAllowCombatOverride        = false;
std::string g_TacticalRequireConfirmationFor     = kDefaultTacticalRequireConfirmationFor;
std::unordered_set<std::string> g_TacticalRequireConfirmationSet;
uint32_t    g_TacticalConfirmationTimeoutSec     = 10;

bool        g_StrategicSuppressLeaderTickForTacticalBots = true;
uint32_t    g_StrategicMaxEscalationsPerBotPerHour       = 6;
uint32_t    g_StrategicQueueMaxSize                      = 32;
uint32_t    g_StrategicWorkerThreads                     = 1;
uint32_t    g_StrategicEscalationCooldownSec             = 30;


static std::vector<std::string> SplitString(const std::string& str, char delim)
{
    std::vector<std::string> tokens;
    std::stringstream ss(str);
    std::string token;
    while (std::getline(ss, token, delim))
    {
        // Trim whitespace from token
        size_t start = token.find_first_not_of(" \t");
        size_t end = token.find_last_not_of(" \t");
        if (start != std::string::npos && end != std::string::npos)
            tokens.push_back(token.substr(start, end - start + 1));
    }
    return tokens;
}

static bool StartsWith(const std::string& value, const char* prefix)
{
    return value.rfind(prefix, 0) == 0;
}

static void AddUniquePath(std::vector<std::string>& paths, const std::string& path)
{
    if (path.empty()) return;
    if (std::find(paths.begin(), paths.end(), path) == paths.end())
        paths.push_back(path);
}

static std::vector<std::string> BuildPromptPathCandidates(const std::string& path)
{
    std::vector<std::string> paths;
    AddUniquePath(paths, path);

    if (StartsWith(path, kLegacyModulePromptPrefix))
    {
        std::string suffix = path.substr(std::strlen(kLegacyModulePromptPrefix));
        AddUniquePath(paths, std::string(kModulePromptPrefix) + suffix);
        AddUniquePath(paths, std::string(kAzerothCoreModulePromptPrefix) + suffix);
    }
    else if (StartsWith(path, kModulePromptPrefix))
    {
        AddUniquePath(paths, std::string("/azerothcore/") + path);
    }

    return paths;
}

static std::string LoadPromptTextFile(const std::string& path, const char* label)
{
    if (path.empty())
        return "";

    for (const std::string& candidate : BuildPromptPathCandidates(path))
    {
        std::ifstream ifs(candidate);
        if (!ifs.is_open())
            continue;

        std::stringstream ss;
        ss << ifs.rdbuf();
        std::string text = ss.str();
        if (text.empty())
        {
            LOG_WARN("server.loading",
                     "[Ollama Chat] prompt file for {} is empty: '{}'",
                     label, candidate);
            return "";
        }

        LOG_INFO("server.loading",
                 "[Ollama Chat] loaded {} prompt file '{}' ({} bytes)",
                 label, candidate, text.size());
        return text;
    }

    LOG_WARN("server.loading",
             "[Ollama Chat] prompt file for {} could not be opened: '{}'",
             label, path);
    return "";
}

static TacticalBotConfig DefaultTacticalBotConfig()
{
    TacticalBotConfig cfg;
    cfg.url = g_TacticalUrl;
    cfg.model = g_TacticalModel;
    cfg.numCtx = g_TacticalNumCtx;
    cfg.timeoutMs = g_TacticalRequestTimeoutMs;
    cfg.maxConcurrentQueries = g_TacticalMaxConcurrentQueries;
    return cfg;
}

static bool ReadOptionalJsonString(const nlohmann::json& entry,
                                   const char* key,
                                   std::string& out,
                                   uint64_t guid)
{
    if (!entry.contains(key))
        return true;

    if (!entry[key].is_string())
    {
        LOG_ERROR("server.loading",
                  "[Ollama Chat Tactical] BotOverridesJson GUID={} field '{}' must be a string",
                  guid, key);
        return false;
    }

    std::string value = entry[key].get<std::string>();
    if (!value.empty())
        out = value;
    return true;
}

static bool ReadOptionalJsonUint32(const nlohmann::json& entry,
                                   const char* key,
                                   uint32_t& out,
                                   uint64_t guid)
{
    if (!entry.contains(key))
        return true;

    if (!entry[key].is_number_integer() && !entry[key].is_number_unsigned())
    {
        LOG_ERROR("server.loading",
                  "[Ollama Chat Tactical] BotOverridesJson GUID={} field '{}' must be an integer",
                  guid, key);
        return false;
    }

    uint64_t value = 0;
    if (entry[key].is_number_unsigned())
    {
        value = entry[key].get<uint64_t>();
    }
    else
    {
        int64_t signedValue = entry[key].get<int64_t>();
        if (signedValue < 0)
        {
            LOG_ERROR("server.loading",
                      "[Ollama Chat Tactical] BotOverridesJson GUID={} field '{}' out of uint32 range",
                      guid, key);
            return false;
        }
        value = static_cast<uint64_t>(signedValue);
    }
    if (value > std::numeric_limits<uint32_t>::max())
    {
        LOG_ERROR("server.loading",
                  "[Ollama Chat Tactical] BotOverridesJson GUID={} field '{}' out of uint32 range",
                  guid, key);
        return false;
    }

    out = static_cast<uint32_t>(value);
    return true;
}

// Optional boolean override; absent leaves `out` untouched (-1 = unset).
static bool ReadOptionalJsonBool(const nlohmann::json& entry,
                                 const char* key,
                                 int8_t& out,
                                 uint64_t guid)
{
    if (!entry.contains(key))
        return true;

    if (!entry[key].is_boolean())
    {
        LOG_ERROR("server.loading",
                  "[Ollama Chat Tactical] BotOverridesJson GUID={} field '{}' must be true/false",
                  guid, key);
        return false;
    }
    out = entry[key].get<bool>() ? 1 : 0;
    return true;
}

static bool ReadRequiredJsonGuid(const nlohmann::json& entry, uint64_t& guid)
{
    if (!entry.contains("guid"))
    {
        LOG_ERROR("server.loading", "[Ollama Chat Tactical] BotOverridesJson entry missing 'guid'");
        return false;
    }

    if (!entry["guid"].is_number_integer() && !entry["guid"].is_number_unsigned())
    {
        LOG_ERROR("server.loading", "[Ollama Chat Tactical] BotOverridesJson 'guid' must be an integer");
        return false;
    }

    if (entry["guid"].is_number_unsigned())
    {
        guid = entry["guid"].get<uint64_t>();
    }
    else
    {
        int64_t value = entry["guid"].get<int64_t>();
        if (value <= 0)
        {
            LOG_ERROR("server.loading", "[Ollama Chat Tactical] BotOverridesJson 'guid' must be positive");
            return false;
        }
        guid = static_cast<uint64_t>(value);
    }

    if (guid == 0)
    {
        LOG_ERROR("server.loading", "[Ollama Chat Tactical] BotOverridesJson 'guid' must be positive");
        return false;
    }
    return true;
}

static void ParseTacticalBotOverridesJson(const std::string& jsonStr)
{
    g_TacticalBotConfigs.clear();
    if (jsonStr.empty())
        return;

    nlohmann::json arr;
    try { arr = nlohmann::json::parse(jsonStr); }
    catch (const std::exception& e)
    {
        LOG_ERROR("server.loading", "[Ollama Chat Tactical] BotOverridesJson parse error: {}", e.what());
        return;
    }

    if (!arr.is_array())
    {
        LOG_ERROR("server.loading", "[Ollama Chat Tactical] BotOverridesJson must be a JSON array");
        return;
    }

    for (const auto& entry : arr)
    {
        if (!entry.is_object())
        {
            LOG_ERROR("server.loading", "[Ollama Chat Tactical] BotOverridesJson entry must be an object");
            continue;
        }

        uint64_t guid = 0;
        if (!ReadRequiredJsonGuid(entry, guid))
            continue;

        TacticalBotConfig cfg = DefaultTacticalBotConfig();
        if (!ReadOptionalJsonString(entry, "url", cfg.url, guid)
            || !ReadOptionalJsonString(entry, "model", cfg.model, guid)
            || !ReadOptionalJsonUint32(entry, "numCtx", cfg.numCtx, guid)
            || !ReadOptionalJsonUint32(entry, "timeoutMs", cfg.timeoutMs, guid)
            || !ReadOptionalJsonUint32(entry, "maxConcurrentQueries", cfg.maxConcurrentQueries, guid)
            || !ReadOptionalJsonBool(entry, "jev", cfg.jev, guid))
        {
            continue;
        }

        g_TacticalBotConfigs[guid] = cfg;
        g_TacticalBotGUIDSet.insert(guid);

        LOG_INFO("server.loading",
                 "[Ollama Chat Tactical] BotOverridesJson: GUID={}, URL={}, Model={}, numCtx={}, timeoutMs={}, maxConcurrentQueries={}, jev={}",
                 guid, cfg.url, cfg.model, cfg.numCtx, cfg.timeoutMs, cfg.maxConcurrentQueries,
                 cfg.jev < 0 ? "unset" : (cfg.jev ? "true" : "false"));
    }
}

static void LoadTacticalBotOverrides()
{
    if (!g_TacticalBotOverridesJsonFile.empty())
    {
        std::ifstream ifs(g_TacticalBotOverridesJsonFile);
        if (ifs)
        {
            std::string fileContents((std::istreambuf_iterator<char>(ifs)),
                                     std::istreambuf_iterator<char>());
            if (!fileContents.empty())
            {
                LOG_INFO("server.loading",
                         "[Ollama Chat Tactical] loaded BotOverridesJson from file '{}' ({} bytes)",
                         g_TacticalBotOverridesJsonFile, fileContents.size());
                if (!g_TacticalBotOverridesJson.empty())
                {
                    LOG_WARN("server.loading",
                             "[Ollama Chat Tactical] BotOverridesJsonFile takes precedence over BotOverridesJson");
                }
                ParseTacticalBotOverridesJson(fileContents);
                return;
            }

            LOG_ERROR("server.loading",
                      "[Ollama Chat Tactical] BotOverridesJsonFile '{}' is empty — falling back to BotOverridesJson",
                      g_TacticalBotOverridesJsonFile);
        }
        else
        {
            LOG_ERROR("server.loading",
                      "[Ollama Chat Tactical] BotOverridesJsonFile '{}' could not be opened — falling back to BotOverridesJson",
                      g_TacticalBotOverridesJsonFile);
        }
    }

    ParseTacticalBotOverridesJson(g_TacticalBotOverridesJson);
}

TacticalBotConfig ResolveTacticalBotConfig(uint64_t botGuid)
{
    auto it = g_TacticalBotConfigs.find(botGuid);
    if (it != g_TacticalBotConfigs.end())
        return it->second;
    return DefaultTacticalBotConfig();
}

// Load Bot Personalities from Database
void LoadBotPersonalityList()
{    
    // Let's make sure our user has sourced the required sql file to add the new table
    QueryResult tableExists = CharacterDatabase.Query("SELECT * FROM information_schema.tables WHERE table_schema = 'acore_characters' AND table_name = 'mod_ollama_chat_personality' LIMIT 1");
    if (!tableExists)
    {
        LOG_ERROR("server.loading", "[Ollama Chat] Please source the required database table first");
        return;
    }

    QueryResult result = CharacterDatabase.Query("SELECT guid,personality FROM mod_ollama_chat_personality");

    if (!result)
    {
        return;
    }
    if (result->GetRowCount() == 0)
    {
        return;
    }    

    if(g_DebugEnabled)
    {
        LOG_INFO("server.loading", "[Ollama Chat] Fetching Bot Personality List into array");
    }

    do
    {
        uint64_t personalityBotGUID = result->Fetch()[0].Get<uint64_t>();
        std::string personalityKey = result->Fetch()[1].Get<std::string>();
        g_BotPersonalityList[personalityBotGUID] = personalityKey;
    } while (result->NextRow());
}

std::string GetMultiLineConfigValue(const std::string& configFilePath, const std::string& key)
{
    std::ifstream infile(configFilePath);
    if (!infile) return "";

    std::string line;
    std::string value;
    bool foundKey = false;
    while (std::getline(infile, line))
    {
        std::string trimmed = line;
        trimmed.erase(0, trimmed.find_first_not_of(" \t\r\n"));
        if (trimmed.empty() || trimmed[0] == '#')
            continue;
        size_t pos = trimmed.find('=');
        if (!foundKey && pos != std::string::npos) {
            std::string possibleKey = trimmed.substr(0, pos);
            possibleKey.erase(possibleKey.find_last_not_of(" \t\r\n") + 1);
            if (possibleKey == key) {
                foundKey = true;
                std::string afterEq = trimmed.substr(pos + 1);
                afterEq.erase(0, afterEq.find_first_not_of(" \t\r\n"));
                value += afterEq;
                continue;
            }
        }
        else if (foundKey) {
            // New config key or section
            if (trimmed.find('=') != std::string::npos && trimmed.find('[') == std::string::npos)
                break;
            if (!value.empty()) value += "\n";
            value += trimmed;
        }
    }

    return value;
}

void LoadOllamaChatConfig()
{
    g_SayDistance                     = sConfigMgr->GetOption<float>("OllamaChat.SayDistance", 30.0f);
    g_YellDistance                    = sConfigMgr->GetOption<float>("OllamaChat.YellDistance", 100.0f);
    
    // Load per-channel-type reply chances
    g_PlayerReplyChance_Say           = sConfigMgr->GetOption<uint32_t>("OllamaChat.PlayerReplyChance.Say", 90);
    g_BotReplyChance_Say              = sConfigMgr->GetOption<uint32_t>("OllamaChat.BotReplyChance.Say", 10);
    g_PlayerReplyChance_Channel       = sConfigMgr->GetOption<uint32_t>("OllamaChat.PlayerReplyChance.Channel", 50);
    g_BotReplyChance_Channel          = sConfigMgr->GetOption<uint32_t>("OllamaChat.BotReplyChance.Channel", 5);
    g_PlayerReplyChance_Party         = sConfigMgr->GetOption<uint32_t>("OllamaChat.PlayerReplyChance.Party", 90);
    g_BotReplyChance_Party            = sConfigMgr->GetOption<uint32_t>("OllamaChat.BotReplyChance.Party", 10);
    g_PlayerReplyChance_Guild         = sConfigMgr->GetOption<uint32_t>("OllamaChat.PlayerReplyChance.Guild", 70);
    g_BotReplyChance_Guild            = sConfigMgr->GetOption<uint32_t>("OllamaChat.BotReplyChance.Guild", 5);
    
    g_MaxBotsToPick                   = sConfigMgr->GetOption<uint32_t>("OllamaChat.MaxBotsToPick", 2);
    g_OllamaUrl                       = sConfigMgr->GetOption<std::string>("OllamaChat.Url", "http://localhost:11434/api/generate");
    g_OllamaModel                     = sConfigMgr->GetOption<std::string>("OllamaChat.Model", "llama3.2:1b");
    g_OllamaNumPredict                = sConfigMgr->GetOption<uint32_t>("OllamaChat.NumPredict", 40);
    g_OllamaTemperature               = sConfigMgr->GetOption<float>("OllamaChat.Temperature", 0.8f);
    g_OllamaTopP                      = sConfigMgr->GetOption<float>("OllamaChat.TopP", 0.95f);
    g_OllamaRepeatPenalty             = sConfigMgr->GetOption<float>("OllamaChat.RepeatPenalty", 1.1f);
    g_OllamaNumCtx                    = sConfigMgr->GetOption<uint32_t>("OllamaChat.NumCtx", 0);
    g_OllamaNumThreads                = sConfigMgr->GetOption<uint32_t>("OllamaChat.NumThreads", 0);
    g_OllamaStop                      = sConfigMgr->GetOption<std::string>("OllamaChat.Stop", "");
    g_OllamaSystemPrompt              = sConfigMgr->GetOption<std::string>("OllamaChat.SystemPrompt", "");
    g_OllamaSeed                      = sConfigMgr->GetOption<std::string>("OllamaChat.Seed", "");

    g_MaxConcurrentQueries            = sConfigMgr->GetOption<uint32_t>("OllamaChat.MaxConcurrentQueries", 0);

    g_Enable                          = sConfigMgr->GetOption<bool>("OllamaChat.Enable", true);
    g_DisableRepliesInCombat          = sConfigMgr->GetOption<bool>("OllamaChat.DisableRepliesInCombat", true);
    g_DisableOllamaResponses          = sConfigMgr->GetOption<bool>("OllamaChat.DisableOllamaResponses", false);
    g_EnableRandomChatter             = sConfigMgr->GetOption<bool>("OllamaChat.EnableRandomChatter", true);
    g_EnableEventChatter              = sConfigMgr->GetOption<bool>("OllamaChat.EnableEventChatter", true);
    g_EnableWhisperReplies            = sConfigMgr->GetOption<bool>("OllamaChat.EnableWhisperReplies", false);

    g_DebugEnabled                    = sConfigMgr->GetOption<bool>("OllamaChat.DebugEnabled", false);
    g_DebugShowFullPrompt             = sConfigMgr->GetOption<bool>("OllamaChat.DebugShowFullPrompt", false);

    g_MinRandomInterval               = sConfigMgr->GetOption<uint32_t>("OllamaChat.MinRandomInterval", 45);
    g_MaxRandomInterval               = sConfigMgr->GetOption<uint32_t>("OllamaChat.MaxRandomInterval", 180);
    g_RandomChatterRealPlayerDistance = sConfigMgr->GetOption<float>("OllamaChat.RandomChatterRealPlayerDistance", 40.0f);
    g_RandomChatterBotCommentChance   = sConfigMgr->GetOption<uint32_t>("OllamaChat.RandomChatterBotCommentChance", 25);
    g_RandomChatterMaxBotsPerPlayer   = sConfigMgr->GetOption<uint32_t>("OllamaChat.RandomChatterMaxBotsPerPlayer", 2);

    g_EnableGuildRandomAmbientChatter = sConfigMgr->GetOption<bool>("OllamaChat.EnableGuildRandomAmbientChatter", true);
    g_GuildRandomChatterChance        = sConfigMgr->GetOption<uint32_t>("OllamaChat.GuildRandomChatterChance", 10);

    g_EventChatterRealPlayerDistance = sConfigMgr->GetOption<float>("OllamaChat.EventChatterRealPlayerDistance", 40.0f);
    g_EventChatterBotCommentChance   = sConfigMgr->GetOption<uint32_t>("OllamaChat.EventChatterBotCommentChance", 15);
    g_EventChatterBotSelfCommentChance = sConfigMgr->GetOption<uint32_t>("OllamaChat.EventChatterBotSelfCommentChance", 5);
    g_EventChatterMaxBotsPerPlayer   = sConfigMgr->GetOption<uint32_t>("OllamaChat.EventChatterMaxBotsPerPlayer", 2);

    g_EnableRPPersonalities           = sConfigMgr->GetOption<bool>("OllamaChat.EnableRPPersonalities", false);

    g_RandomChatterPromptTemplate     = sConfigMgr->GetOption<std::string>("OllamaChat.RandomChatterPromptTemplate", "");

    // Load random chatter prompt variations
    std::string variationsStr = sConfigMgr->GetOption<std::string>("OllamaChat.RandomChatterPromptVariations", "");
    g_RandomChatterPromptVariations.clear();
    if (!variationsStr.empty())
    {
        std::stringstream ss(variationsStr);
        std::string variation;
        while (std::getline(ss, variation, '|'))
        {
            if (!variation.empty())
            {
                g_RandomChatterPromptVariations.push_back(variation);
            }
        }
    }

    // Load random chatter question variations
    std::string questionsStr = sConfigMgr->GetOption<std::string>("OllamaChat.RandomChatterQuestionVariations", "");
    g_RandomChatterQuestionVariations.clear();
    if (!questionsStr.empty())
    {
        std::stringstream ss(questionsStr);
        std::string question;
        while (std::getline(ss, question, '|'))
        {
            if (!question.empty())
            {
                g_RandomChatterQuestionVariations.push_back(question);
            }
        }
    }

    g_EventChatterPromptTemplate     = sConfigMgr->GetOption<std::string>("OllamaChat.EventChatterPromptTemplate", "");

    g_ChatPromptTemplate              = sConfigMgr->GetOption<std::string>("OllamaChat.ChatPromptTemplate", "");
    
    g_ChatExtraInfoTemplate           = sConfigMgr->GetOption<std::string>("OllamaChat.ChatExtraInfoTemplate", "");

    g_DefaultPersonalityPrompt        = sConfigMgr->GetOption<std::string>("OllamaChat.DefaultPersonalityPrompt", "");

    g_MaxConversationHistory          = sConfigMgr->GetOption<uint32_t>("OllamaChat.MaxConversationHistory", 5);
    g_ConversationHistorySaveInterval = sConfigMgr->GetOption<uint32_t>("OllamaChat.ConversationHistorySaveInterval", 10);

    g_ChatHistoryHeaderTemplate       = sConfigMgr->GetOption<std::string>("OllamaChat.ChatHistoryHeaderTemplate", "");
    g_ChatHistoryLineTemplate         = sConfigMgr->GetOption<std::string>("OllamaChat.ChatHistoryLineTemplate", "");
    g_ChatHistoryFooterTemplate       = sConfigMgr->GetOption<std::string>("OllamaChat.ChatHistoryFooterTemplate", "");

    g_EnableChatBotSnapshotTemplate   = sConfigMgr->GetOption<bool>("OllamaChat.EnableChatBotSnapshotTemplate", false);
    g_ChatBotSnapshotTemplate         = sConfigMgr->GetOption<std::string>("OllamaChat.ChatBotSnapshotTemplate", "");

    g_EnableChatHistory               = sConfigMgr->GetOption<bool>("OllamaChat.EnableChatHistory", true);

    // Bot-Player Sentiment Tracking
    g_EnableSentimentTracking         = sConfigMgr->GetOption<bool>("OllamaChat.EnableSentimentTracking", true);
    g_SentimentDefaultValue           = sConfigMgr->GetOption<float>("OllamaChat.SentimentDefaultValue", 0.5f);
    g_SentimentAdjustmentStrength     = sConfigMgr->GetOption<float>("OllamaChat.SentimentAdjustmentStrength", 0.1f);
    g_SentimentSaveInterval           = sConfigMgr->GetOption<uint32_t>("OllamaChat.SentimentSaveInterval", 10);
    g_SentimentAnalysisPrompt         = sConfigMgr->GetOption<std::string>("OllamaChat.SentimentAnalysisPrompt", "Analyze the sentiment of this message: \"{message}\". Respond only with: POSITIVE, NEGATIVE, or NEUTRAL.");
    g_SentimentPromptTemplate         = sConfigMgr->GetOption<std::string>("OllamaChat.SentimentPromptTemplate", "Your relationship sentiment with {player_name} is {sentiment_value} (0.0=hostile, 0.5=neutral, 1.0=friendly). Use this to guide your tone and response.");

    // RAG (Retrieval-Augmented Generation) System
    g_EnableRAG                       = sConfigMgr->GetOption<bool>("OllamaChat.EnableRAG", false);
    g_RAGDataPath                     = sConfigMgr->GetOption<std::string>("OllamaChat.RAGDataPath", "rag/");
    g_RAGMaxRetrievedItems            = sConfigMgr->GetOption<uint32_t>("OllamaChat.RAGMaxRetrievedItems", 3);
    g_RAGSimilarityThreshold          = sConfigMgr->GetOption<float>("OllamaChat.RAGSimilarityThreshold", 0.3f);
    g_RAGPromptTemplate               = sConfigMgr->GetOption<std::string>("OllamaChat.RAGPromptTemplate", "RELEVANT INFORMATION:\n{rag_info}\nUse this information to provide accurate and detailed responses when applicable.");

    g_ThinkModeEnableForModule        = sConfigMgr->GetOption<bool>("OllamaChat.ThinkModeEnableForModule", false);

    // Typing Simulation
    g_EnableTypingSimulation          = sConfigMgr->GetOption<bool>("OllamaChat.EnableTypingSimulation", false);
    g_TypingSimulationBaseDelay       = sConfigMgr->GetOption<uint32_t>("OllamaChat.TypingSimulationBaseDelay", 1000);
    g_TypingSimulationDelayPerChar    = sConfigMgr->GetOption<uint32_t>("OllamaChat.TypingSimulationDelayPerChar", 250);

    // Gateway Agent Bot
    g_GatewayEnable                   = sConfigMgr->GetOption<bool>("OllamaChat.Gateway.Enable", false);
    g_GatewayUrl                      = sConfigMgr->GetOption<std::string>("OllamaChat.Gateway.Url", "");
    g_GatewayBearerToken              = sConfigMgr->GetOption<std::string>("OllamaChat.Gateway.BearerToken", "");
    g_GatewayTriggerKeyword           = sConfigMgr->GetOption<std::string>("OllamaChat.Gateway.TriggerKeyword", "claude");
    g_GatewaySystemPrompt             = sConfigMgr->GetOption<std::string>("OllamaChat.Gateway.SystemPrompt", "");
    g_GatewayModel                    = sConfigMgr->GetOption<std::string>("OllamaChat.Gateway.Model", "openclaw");
    g_GatewayScopes                   = sConfigMgr->GetOption<std::string>("OllamaChat.Gateway.Scopes", "operator.admin,operator.read,operator.write");
    g_GatewayFallbackToOllama         = sConfigMgr->GetOption<bool>("OllamaChat.Gateway.FallbackToOllama", false);
    g_GatewayMaxHistory               = sConfigMgr->GetOption<uint32_t>("OllamaChat.Gateway.MaxHistory", 10);
    g_GatewayType                     = sConfigMgr->GetOption<std::string>("OllamaChat.Gateway.Type", "openclaw");

    g_GatewayTimeoutSeconds            = sConfigMgr->GetOption<uint32_t>("OllamaChat.Gateway.TimeoutSeconds", 300);
    g_GatewayFallbackOnError           = sConfigMgr->GetOption<bool>("OllamaChat.Gateway.FallbackOnError", false);
    g_GatewayMaxConcurrentRequests     = sConfigMgr->GetOption<uint32_t>("OllamaChat.Gateway.MaxConcurrentRequests", 4);
    g_GatewayMinSecondsBetweenRequests = sConfigMgr->GetOption<uint32_t>("OllamaChat.Gateway.MinSecondsBetweenRequests", 5);
    g_GatewayUserPrefix                = sConfigMgr->GetOption<std::string>("OllamaChat.Gateway.UserPrefix", "wow-bot-");
    g_GatewayMaxWhisperChunkChars      = sConfigMgr->GetOption<uint32_t>("OllamaChat.Gateway.MaxWhisperChunkChars", 250);
    g_GatewayUseSessionPersistence     = sConfigMgr->GetOption<bool>("OllamaChat.Gateway.UseSessionPersistence", false);
    g_GatewaySessionKeyTemplate        = sConfigMgr->GetOption<std::string>("OllamaChat.Gateway.SessionKeyTemplate", "wow-{botGuid}-{playerGuid}");
    g_GatewaySkipHistoryWhenSession    = sConfigMgr->GetOption<bool>("OllamaChat.Gateway.SkipHistoryWhenSession", true);
    g_GatewayUseStreaming              = sConfigMgr->GetOption<bool>("OllamaChat.Gateway.UseStreaming", false);
    g_GatewayStreamWhisperOnSentence   = sConfigMgr->GetOption<bool>("OllamaChat.Gateway.StreamWhisperOnSentence", true);

    g_GatewayAgentByPersonality        = sConfigMgr->GetOption<std::string>("OllamaChat.Gateway.AgentByPersonality", "");
    ParseGatewayAgentByPersonality(g_GatewayAgentByPersonality);
    g_GatewayMergePersonalityPrompt    = sConfigMgr->GetOption<bool>("OllamaChat.Gateway.MergePersonalityPrompt", true);
    g_GatewayDetectLanguage            = sConfigMgr->GetOption<bool>("OllamaChat.Gateway.DetectLanguage", false);
    g_GatewayLanguageHintTemplate      = sConfigMgr->GetOption<std::string>("OllamaChat.Gateway.LanguageHintTemplate", "Always respond to the user in {lang}.");

    g_GatewayAllowedChannels           = sConfigMgr->GetOption<std::string>("OllamaChat.Gateway.AllowedChannels", "whisper");
    ParseGatewayAllowedChannels(g_GatewayAllowedChannels);
    g_GatewayPublicChannelMode         = sConfigMgr->GetOption<std::string>("OllamaChat.Gateway.PublicChannelMode", "keyword");
    g_GatewayPrivateChannelMentionRequired = sConfigMgr->GetOption<bool>("OllamaChat.Gateway.PrivateChannelMentionRequired", true);
    g_GatewayMentionExemptBots         = sConfigMgr->GetOption<std::string>("OllamaChat.Gateway.MentionExemptBots", "");
    g_GatewayMentionExemptBotsSet.clear();
    for (auto tok : SplitString(g_GatewayMentionExemptBots, ','))
    {
        // Trim whitespace and case-fold so matching against bot->GetName() is tolerant.
        while (!tok.empty() && std::isspace(static_cast<unsigned char>(tok.front()))) tok.erase(tok.begin());
        while (!tok.empty() && std::isspace(static_cast<unsigned char>(tok.back())))  tok.pop_back();
        std::transform(tok.begin(), tok.end(), tok.begin(), [](unsigned char c){ return std::tolower(c); });
        if (!tok.empty()) g_GatewayMentionExemptBotsSet.insert(tok);
    }

    g_GatewayAutoClaimOnLogin          = sConfigMgr->GetOption<bool>("OllamaChat.Gateway.AutoClaimOnLogin", false);
    g_GatewayAutoClaimAccountIds       = sConfigMgr->GetOption<std::string>("OllamaChat.Gateway.AutoClaimAccountIds", "");
    g_GatewayAutoClaimAccountIdsSet.clear();
    for (auto tok : SplitString(g_GatewayAutoClaimAccountIds, ','))
    {
        while (!tok.empty() && std::isspace(static_cast<unsigned char>(tok.front()))) tok.erase(tok.begin());
        while (!tok.empty() && std::isspace(static_cast<unsigned char>(tok.back())))  tok.pop_back();
        if (tok.empty()) continue;
        try {
            uint32_t v = static_cast<uint32_t>(std::stoul(tok));
            if (v != 0) g_GatewayAutoClaimAccountIdsSet.insert(v);
        } catch (...) {
            LOG_WARN("server.loading",
                     "[Ollama Chat Gateway] ignoring invalid AutoClaimAccountIds token '{}'", tok);
        }
    }

    g_GatewayEnableToolUse             = sConfigMgr->GetOption<bool>("OllamaChat.Gateway.EnableToolUse", false);
    g_GatewayMaxToolIterations         = sConfigMgr->GetOption<uint32_t>("OllamaChat.Gateway.MaxToolIterations", 3);
    g_GatewayAllowedTools              = sConfigMgr->GetOption<std::string>("OllamaChat.Gateway.AllowedTools", "");
    g_GatewayAllowedToolsList          = SplitString(g_GatewayAllowedTools, ',');
    g_GatewayToolFacadeGroups          = sConfigMgr->GetOption<std::string>("OllamaChat.Gateway.ToolFacadeGroups", "*");
    g_GatewayToolFacadeGroupList       = SplitString(g_GatewayToolFacadeGroups, ',');

    g_GatewayEnableAudit               = sConfigMgr->GetOption<bool>("OllamaChat.Gateway.EnableAudit", false);
    g_GatewayAuditRetentionDays        = sConfigMgr->GetOption<uint32_t>("OllamaChat.Gateway.AuditRetentionDays", 30);
    g_GatewayEnableActionMarkers       = sConfigMgr->GetOption<bool>("OllamaChat.Gateway.EnableActionMarkers", false);

    g_McpEnable                          = sConfigMgr->GetOption<bool>("OllamaChat.Mcp.Enable", false);
    g_McpBindAddress                     = sConfigMgr->GetOption<std::string>("OllamaChat.Mcp.BindAddress", "0.0.0.0");
    g_McpPort                            = sConfigMgr->GetOption<uint32_t>("OllamaChat.Mcp.Port", 18790);
    g_McpBearerToken                     = sConfigMgr->GetOption<std::string>("OllamaChat.Mcp.BearerToken", "");
    g_McpInjectContextHint               = sConfigMgr->GetOption<bool>("OllamaChat.Mcp.InjectContextHint", true);
    g_McpAllowActionTools                = sConfigMgr->GetOption<bool>("OllamaChat.Mcp.AllowActionTools", false);
    g_McpActionRateLimitPerBotPerMinute  = sConfigMgr->GetOption<uint32_t>("OllamaChat.Mcp.ActionRateLimitPerBotPerMinute", 6);
    g_McpAllowedToolsExtra               = sConfigMgr->GetOption<std::string>("OllamaChat.Mcp.AllowedToolsExtra", "");
    g_McpAllowGatewayInjection           = sConfigMgr->GetOption<bool>("OllamaChat.Mcp.AllowGatewayInjection", false);

    // ops-api proxy (8 read-only MCP tools — see `docs/ops-api.md`)
    g_OpsEnable                          = sConfigMgr->GetOption<bool>("OllamaChat.Ops.Enable", false);
    g_OpsUrl                             = sConfigMgr->GetOption<std::string>("OllamaChat.Ops.Url", "http://host.docker.internal:18791");
    g_OpsBearerToken                     = sConfigMgr->GetOption<std::string>("OllamaChat.Ops.BearerToken", "");
    g_OpsTimeoutSeconds                  = sConfigMgr->GetOption<int>("OllamaChat.Ops.TimeoutSeconds", 10);
    // Tier 9 — leader bot
    g_McpLeaderEnable                    = sConfigMgr->GetOption<bool>("OllamaChat.Mcp.Leader.Enable", false);
    g_McpLeaderBotGUIDs                  = sConfigMgr->GetOption<std::string>("OllamaChat.Mcp.Leader.BotGUIDs", "");
    g_McpLeaderScope                     = sConfigMgr->GetOption<std::string>("OllamaChat.Mcp.Leader.Scope", "group");
    g_McpLeaderNearbyRadius              = sConfigMgr->GetOption<float>("OllamaChat.Mcp.Leader.NearbyRadius", 60.0f);
    g_McpLeaderDeniedCommands            = sConfigMgr->GetOption<std::string>("OllamaChat.Mcp.Leader.DeniedCommands", "logout,quit,delete,reset,suicide");
    g_McpLeaderRateLimitPerMinute        = sConfigMgr->GetOption<uint32_t>("OllamaChat.Mcp.Leader.RateLimitPerMinute", 30);
    g_McpLeaderInjectPromptHint          = sConfigMgr->GetOption<bool>("OllamaChat.Mcp.Leader.InjectPromptHint", true);
    g_McpLeaderSystemPromptFile          = sConfigMgr->GetOption<std::string>(
        "OllamaChat.Mcp.Leader.SystemPromptFile",
        kDefaultGatewayLeaderPromptFile);
    g_McpLeaderSystemPromptText          = LoadPromptTextFile(g_McpLeaderSystemPromptFile, "gateway leader");

    // Parse Leader.BotGUIDs csv → set
    g_McpLeaderBotGUIDSet.clear();
    for (const auto& tok : SplitString(g_McpLeaderBotGUIDs, ','))
    {
        try { g_McpLeaderBotGUIDSet.insert(std::stoull(tok)); }
        catch (const std::exception& e)
        {
            LOG_ERROR("server.loading", "[Ollama Chat MCP Leader] Invalid GUID in Mcp.Leader.BotGUIDs: '{}' — {}", tok, e.what());
        }
    }
    // Parse Leader.DeniedCommands csv → list (lowercase + trimmed prefix matches)
    g_McpLeaderDeniedCommandsList.clear();
    for (auto tok : SplitString(g_McpLeaderDeniedCommands, ','))
    {
        std::transform(tok.begin(), tok.end(), tok.begin(), [](unsigned char c){ return std::tolower(c); });
        if (!tok.empty()) g_McpLeaderDeniedCommandsList.push_back(tok);
    }

    // Tier 10 — autonomous tick
    g_McpLeaderAutoTickEnable            = sConfigMgr->GetOption<bool>("OllamaChat.Mcp.Leader.AutoTickEnable", false);
    g_McpLeaderAutoTickIntervalSeconds   = sConfigMgr->GetOption<uint32_t>("OllamaChat.Mcp.Leader.AutoTickIntervalSeconds", 15);
    g_McpLeaderAutoTickMaxPerHour        = sConfigMgr->GetOption<uint32_t>("OllamaChat.Mcp.Leader.AutoTickMaxPerHour", 20);

    // Tier 9.1 — admin commands
    g_McpLeaderAllowAdminCommands        = sConfigMgr->GetOption<bool>("OllamaChat.Mcp.Leader.AllowAdminCommands", false);
    g_McpLeaderAllowedAdminCommands      = sConfigMgr->GetOption<std::string>("OllamaChat.Mcp.Leader.AllowedAdminCommands", "add,addclass,addaccount,remove,list");
    g_McpGmAllowedCommands = sConfigMgr->GetOption<std::string>("OllamaChat.Mcp.Gm.AllowedCommands",
                                                                "lookup,pinfo,tele name,server info");

    // Phase 1 — Overlord headless auto-login
    g_OverlordEnable                     = sConfigMgr->GetOption<bool>("OllamaChat.Overlord.Enable", false);
    g_OverlordCharacterGuid              = sConfigMgr->GetOption<uint32_t>("OllamaChat.Overlord.CharacterGuid", 0);
    g_OverlordStartupDelaySeconds        = sConfigMgr->GetOption<uint32_t>("OllamaChat.Overlord.StartupDelaySeconds", 30);
    g_OverlordAutoSpawnBots              = sConfigMgr->GetOption<std::string>("OllamaChat.Overlord.AutoSpawnBots", "");
    g_OverlordAutoSpawnBotList.clear();
    for (auto tok : SplitString(g_OverlordAutoSpawnBots, ','))
    {
        while (!tok.empty() && std::isspace(static_cast<unsigned char>(tok.front()))) tok.erase(tok.begin());
        while (!tok.empty() && std::isspace(static_cast<unsigned char>(tok.back())))  tok.pop_back();
        if (tok.empty()) continue;
        try {
            uint32_t v = static_cast<uint32_t>(std::stoul(tok));
            if (v != 0) g_OverlordAutoSpawnBotList.push_back(v);
        } catch (...) {
            LOG_WARN("server.loading", "[Ollama Chat Overlord] ignoring invalid AutoSpawnBots token '{}'", tok);
        }
    }

    // Phase 1.2 — Fallback master for leader_admin_command
    g_McpLeaderSystemMasterGuid          = sConfigMgr->GetOption<uint32_t>("OllamaChat.Mcp.Leader.SystemMasterGuid", 0);

    // Fleet bootstrap — robot-first party/master reconciliation
    g_FleetEnsureParty                   = sConfigMgr->GetOption<bool>("OllamaChat.Fleet.EnsureParty", false);
    g_FleetLeaderGuid                    = sConfigMgr->GetOption<uint32_t>("OllamaChat.Fleet.LeaderGuid", 0);
    g_FleetMemberGuids                   = sConfigMgr->GetOption<std::string>("OllamaChat.Fleet.MemberGuids", "");
    g_FleetEnsureIntervalSec             = sConfigMgr->GetOption<uint32_t>("OllamaChat.Fleet.EnsureIntervalSec", 20);
    g_FleetMemberGuidList.clear();
    for (auto tok : SplitString(g_FleetMemberGuids, ','))
    {
        try {
            uint32_t v = static_cast<uint32_t>(std::stoul(tok));
            if (v != 0) g_FleetMemberGuidList.push_back(v);
        } catch (...) {
            LOG_WARN("server.loading", "[Ollama Chat Fleet] ignoring invalid MemberGuids token '{}'", tok);
        }
    }
    g_McpGmAllowedCommandsList.clear();
    for (auto tok : SplitString(g_McpGmAllowedCommands, ','))
    {
        std::transform(tok.begin(), tok.end(), tok.begin(), [](unsigned char c){ return std::tolower(c); });
        if (!tok.empty()) g_McpGmAllowedCommandsList.push_back(tok);
    }
    g_McpLeaderAllowedAdminCommandsList.clear();
    for (auto tok : SplitString(g_McpLeaderAllowedAdminCommands, ','))
    {
        std::transform(tok.begin(), tok.end(), tok.begin(), [](unsigned char c){ return std::tolower(c); });
        if (!tok.empty()) g_McpLeaderAllowedAdminCommandsList.push_back(tok);
    }

    if (g_McpLeaderEnable)
    {
        LOG_INFO("server.loading", "[Ollama Chat MCP Leader] enabled: leaders={}, scope={}, denied={}, rate={}/min, autoTick={} (interval={}s, max={}/hr)",
                 g_McpLeaderBotGUIDSet.size(), g_McpLeaderScope, g_McpLeaderDeniedCommandsList.size(),
                 g_McpLeaderRateLimitPerMinute, g_McpLeaderAutoTickEnable,
                 g_McpLeaderAutoTickIntervalSeconds, g_McpLeaderAutoTickMaxPerHour);
    }

    // MCP config-edit tools
    g_McpConfigEditEnable                = sConfigMgr->GetOption<bool>("OllamaChat.Mcp.ConfigEdit.Enable", false);
    g_McpConfigEditConfigRoot            = sConfigMgr->GetOption<std::string>("OllamaChat.Mcp.ConfigEdit.ConfigRoot", "");
    g_McpConfigEditAllowedFiles          = sConfigMgr->GetOption<std::string>("OllamaChat.Mcp.ConfigEdit.AllowedFiles", "");
    g_McpConfigEditAllowedKeys           = sConfigMgr->GetOption<std::string>("OllamaChat.Mcp.ConfigEdit.AllowedKeys", "");
    g_McpConfigEditDeniedKeys            = sConfigMgr->GetOption<std::string>("OllamaChat.Mcp.ConfigEdit.DeniedKeys",
                                            "*Token*,*Password*,*Secret*,*BearerToken*,*ApiKey*,*DatabaseInfo*");
    g_McpConfigEditBackupDir             = sConfigMgr->GetOption<std::string>("OllamaChat.Mcp.ConfigEdit.BackupDir", "");
    g_McpConfigEditMaxFileSizeBytes      = sConfigMgr->GetOption<uint32_t>("OllamaChat.Mcp.ConfigEdit.MaxFileSizeBytes", 1048576);
    g_McpConfigEditReloadAdminSessionGuid= sConfigMgr->GetOption<uint32_t>("OllamaChat.Mcp.ConfigEdit.ReloadAdminSessionGuid", 0);

    g_McpConfigEditAllowedFilesSet.clear();
    for (auto tok : SplitString(g_McpConfigEditAllowedFiles, ','))
    {
        while (!tok.empty() && std::isspace(static_cast<unsigned char>(tok.front()))) tok.erase(tok.begin());
        while (!tok.empty() && std::isspace(static_cast<unsigned char>(tok.back())))  tok.pop_back();
        if (!tok.empty()) g_McpConfigEditAllowedFilesSet.insert(tok);
    }

    g_McpConfigEditAllowedKeysMap.clear();
    for (auto tok : SplitString(g_McpConfigEditAllowedKeys, ','))
    {
        while (!tok.empty() && std::isspace(static_cast<unsigned char>(tok.front()))) tok.erase(tok.begin());
        while (!tok.empty() && std::isspace(static_cast<unsigned char>(tok.back())))  tok.pop_back();
        auto colon = tok.find(':');
        if (colon == std::string::npos || colon == 0 || colon + 1 >= tok.size()) continue;
        std::string f = tok.substr(0, colon);
        std::string k = tok.substr(colon + 1);
        g_McpConfigEditAllowedKeysMap[f].push_back(k);
    }

    g_McpConfigEditDeniedKeysList.clear();
    for (auto tok : SplitString(g_McpConfigEditDeniedKeys, ','))
    {
        while (!tok.empty() && std::isspace(static_cast<unsigned char>(tok.front()))) tok.erase(tok.begin());
        while (!tok.empty() && std::isspace(static_cast<unsigned char>(tok.back())))  tok.pop_back();
        if (!tok.empty()) g_McpConfigEditDeniedKeysList.push_back(tok);
    }

    if (g_McpConfigEditEnable)
    {
        LOG_INFO("server.loading", "[Ollama Chat MCP ConfigEdit] enabled: root={}, files={}, keyGlobs={}, deny={}, backupDir={}",
                 g_McpConfigEditConfigRoot, g_McpConfigEditAllowedFilesSet.size(),
                 g_McpConfigEditAllowedKeysMap.size(), g_McpConfigEditDeniedKeysList.size(),
                 g_McpConfigEditBackupDir);
    }

    // --------------------------------------------
    // Tactical (companion-mode) agent layer
    // --------------------------------------------
    g_TacticalEnable                     = sConfigMgr->GetOption<bool>("OllamaChat.Tactical.Enable", true);
    g_TacticalBotGUIDs                   = sConfigMgr->GetOption<std::string>("OllamaChat.Tactical.BotGUIDs", "");
    g_TacticalUrl                        = sConfigMgr->GetOption<std::string>("OllamaChat.Tactical.Url", "http://host.docker.internal:11434/api/generate");
    g_TacticalModel                      = sConfigMgr->GetOption<std::string>("OllamaChat.Tactical.Model", "qwen3:14b");
    g_TacticalBotOverridesJson           = sConfigMgr->GetOption<std::string>("OllamaChat.Tactical.BotOverridesJson", "");
    g_TacticalBotOverridesJsonFile       = sConfigMgr->GetOption<std::string>("OllamaChat.Tactical.BotOverridesJsonFile", "");
    g_TacticalHumanPresenceRequired      = sConfigMgr->GetOption<bool>("OllamaChat.Tactical.HumanPresenceRequired", true);
    g_TacticalHeartbeatMs                = sConfigMgr->GetOption<uint32_t>("OllamaChat.Tactical.HeartbeatMs", 10000);
    g_TacticalBotCooldownMs              = sConfigMgr->GetOption<uint32_t>("OllamaChat.Tactical.BotCooldownMs", 2000);
    g_TacticalSnapshotCacheMs            = sConfigMgr->GetOption<uint32_t>("OllamaChat.Tactical.SnapshotCacheMs", 500);
    g_TacticalMaxConcurrentQueries       = sConfigMgr->GetOption<uint32_t>("OllamaChat.Tactical.MaxConcurrentQueries", 1);
    g_TacticalMaxPerBotPerHour           = sConfigMgr->GetOption<uint32_t>("OllamaChat.Tactical.MaxPerBotPerHour", 600);
    g_TacticalRequestTimeoutMs           = sConfigMgr->GetOption<uint32_t>("OllamaChat.Tactical.RequestTimeoutMs", 5000);
    g_TacticalNumCtx                     = sConfigMgr->GetOption<uint32_t>("OllamaChat.Tactical.NumCtx", 2048);
    g_TacticalSystemPromptFile           = sConfigMgr->GetOption<std::string>(
        "OllamaChat.Tactical.SystemPromptFile",
        kDefaultTacticalPromptFile);
    g_TacticalSystemPromptText           = LoadPromptTextFile(g_TacticalSystemPromptFile, "tactical");
    g_TacticalMinWhisperGapSec           = sConfigMgr->GetOption<uint32_t>("OllamaChat.Tactical.MinWhisperGapSec", 120);
    g_TacticalAmbientEnable              = sConfigMgr->GetOption<bool>("OllamaChat.Tactical.AmbientEnable", true);
    g_TacticalAmbientMinSpeechGapSec     = sConfigMgr->GetOption<uint32_t>("OllamaChat.Tactical.AmbientMinSpeechGapSec", 60);
    g_TacticalAmbientMinEmoteGapSec      = sConfigMgr->GetOption<uint32_t>("OllamaChat.Tactical.AmbientMinEmoteGapSec", 45);
    g_TacticalAmbientMaxVisibleActionsPerMinute = sConfigMgr->GetOption<uint32_t>("OllamaChat.Tactical.AmbientMaxVisibleActionsPerMinute", 2);
    g_TacticalAmbientEventReactionChance = sConfigMgr->GetOption<uint32_t>("OllamaChat.Tactical.AmbientEventReactionChance", 35);
    g_TacticalAutoEnrollNearbyBots       = sConfigMgr->GetOption<bool>("OllamaChat.Tactical.AutoEnrollNearbyBots", true);
    g_TacticalNearbyBotMax               = sConfigMgr->GetOption<uint32_t>("OllamaChat.Tactical.NearbyBotMax", 5);
    g_TacticalNearbyBotRadius            = sConfigMgr->GetOption<float>("OllamaChat.Tactical.NearbyBotRadius", 60.0f);
    g_GatewayOllamaClassifierEnable      = sConfigMgr->GetOption<bool>("OllamaChat.Gateway.OllamaClassifier.Enable", true);
    g_GatewayOllamaClassifierUrl         = sConfigMgr->GetOption<std::string>("OllamaChat.Gateway.OllamaClassifier.Url", "");
    g_GatewayOllamaClassifierModel       = sConfigMgr->GetOption<std::string>("OllamaChat.Gateway.OllamaClassifier.Model", "qwen3:8b");
    g_GatewayOllamaClassifierTimeoutMs   = sConfigMgr->GetOption<uint32_t>("OllamaChat.Gateway.OllamaClassifier.TimeoutMs", 5000);
    // One shared bearer for every fast-path URL that LlmWire detects as OpenAI
    // chat (Tactical.Url, OllamaClassifier.Url, per-bot tactical overrides).
    // Ollama-format URLs never see it. Read at startup and on config_reload, so
    // a key change needs no restart.
    g_OpenAiCompatApiKey                 = sConfigMgr->GetOption<std::string>("OllamaChat.OpenAiCompat.ApiKey", "");
    g_GatewayOllamaClassifierNumCtx      = sConfigMgr->GetOption<uint32_t>("OllamaChat.Gateway.OllamaClassifier.NumCtx", 2048);
    g_GatewayOllamaClassifierSystemPromptFile = sConfigMgr->GetOption<std::string>(
        "OllamaChat.Gateway.OllamaClassifier.SystemPromptFile",
        kDefaultClassifierPromptFile);
    g_GatewayOllamaClassifierSystemPromptText = LoadPromptTextFile(
        g_GatewayOllamaClassifierSystemPromptFile, "ollama classifier");
    g_GatewayOllamaClassifierMinConfidence = sConfigMgr->GetOption<float>("OllamaChat.Gateway.OllamaClassifier.MinConfidence", 0.8f);
    g_GatewayClassifierMaxWords            = sConfigMgr->GetOption<uint32>("OllamaChat.Gateway.Classifier.MaxWords", 0);
    g_TacticalPromptMaxBytes             = sConfigMgr->GetOption<uint32_t>("OllamaChat.Tactical.PromptMaxBytes", 2048);
    g_TacticalEnableAudit                = sConfigMgr->GetOption<bool>("OllamaChat.Tactical.EnableAudit", true);
    g_TacticalAuditRetentionDays         = sConfigMgr->GetOption<uint32_t>("OllamaChat.Tactical.AuditRetentionDays", 7);
    g_TacticalAllowEscalation            = sConfigMgr->GetOption<bool>("OllamaChat.Tactical.AllowEscalation", true);
    g_TacticalDirectiveMaxTtlSec         = sConfigMgr->GetOption<uint32_t>("OllamaChat.Tactical.DirectiveMaxTtlSec", 600);
    g_TacticalIdlePauseSec               = sConfigMgr->GetOption<uint32_t>("OllamaChat.Tactical.IdlePauseSec", 300);
    g_TacticalAllowCombatOverride        = sConfigMgr->GetOption<bool>("OllamaChat.Tactical.AllowCombatOverride", false);
    g_TacticalRequireConfirmationFor     = sConfigMgr->GetOption<std::string>(
        "OllamaChat.Tactical.RequireConfirmationFor",
        kDefaultTacticalRequireConfirmationFor);
    g_TacticalConfirmationTimeoutSec     = sConfigMgr->GetOption<uint32_t>("OllamaChat.Tactical.ConfirmationTimeoutSec", 10);

    g_TacticalBotGUIDSet.clear();
    for (auto const& tok : SplitString(g_TacticalBotGUIDs, ','))
    {
        if (tok.empty()) continue;
        try
        {
            uint64_t guid = std::stoull(tok);
            if (guid != 0) g_TacticalBotGUIDSet.insert(guid);
        }
        catch (...) { /* ignore malformed */ }
    }
    LoadTacticalBotOverrides();

    g_TacticalRequireConfirmationSet.clear();
    for (auto const& tok : SplitString(g_TacticalRequireConfirmationFor, ','))
    {
        if (!tok.empty()) g_TacticalRequireConfirmationSet.insert(tok);
    }

    // Strategic (paid) — purely reactive, never on a timer
    g_StrategicSuppressLeaderTickForTacticalBots = sConfigMgr->GetOption<bool>("OllamaChat.Strategic.SuppressLeaderTickForTacticalBots", true);
    g_StrategicMaxEscalationsPerBotPerHour       = sConfigMgr->GetOption<uint32_t>("OllamaChat.Strategic.MaxEscalationsPerBotPerHour", 6);
    g_StrategicQueueMaxSize                      = sConfigMgr->GetOption<uint32_t>("OllamaChat.Strategic.QueueMaxSize", 32);
    g_StrategicWorkerThreads                     = sConfigMgr->GetOption<uint32_t>("OllamaChat.Strategic.WorkerThreads", 1);
    g_StrategicEscalationCooldownSec             = sConfigMgr->GetOption<uint32_t>("OllamaChat.Strategic.EscalationCooldownSec", 30);

    // Proactive Leader Bot (feature 002) — default OFF; opt-in via Proactive.Enable=1.
    g_ProactiveEnable                       = sConfigMgr->GetOption<bool>("OllamaChat.Proactive.Enable", false);
    g_ProactiveProposalCooldownSec          = sConfigMgr->GetOption<uint32_t>("OllamaChat.Proactive.ProposalCooldownSec", 120);
    g_ProactiveFirstProposalDelaySec        = sConfigMgr->GetOption<uint32_t>("OllamaChat.Proactive.FirstProposalDelaySec", 15);
    g_ProactiveIdleThresholdSec             = sConfigMgr->GetOption<uint32_t>("OllamaChat.Proactive.IdleThresholdSec", 180);
    g_ProactiveIdleNudgeCooldownSec         = sConfigMgr->GetOption<uint32_t>("OllamaChat.Proactive.IdleNudgeCooldownSec", 300);
    g_ProactiveIdleNudgeMaxPerSession       = sConfigMgr->GetOption<uint32_t>("OllamaChat.Proactive.IdleNudgeMaxPerSession", 3);
    g_ProactiveProposalMaxPerSession        = sConfigMgr->GetOption<uint32_t>("OllamaChat.Proactive.ProposalMaxPerSession", 5);
    g_ProactiveProximityYards               = sConfigMgr->GetOption<float>   ("OllamaChat.Proactive.ProximityYards", 60.0f);
    g_ProactiveProximityWarnCooldownSec     = sConfigMgr->GetOption<uint32_t>("OllamaChat.Proactive.ProximityWarnCooldownSec", 30);
    g_ProactiveActivityCompleteTimeoutSec   = sConfigMgr->GetOption<uint32_t>("OllamaChat.Proactive.ActivityCompleteTimeoutSec", 1800);
    g_ProactiveActivityCompleteNudgeWaitSec = sConfigMgr->GetOption<uint32_t>("OllamaChat.Proactive.ActivityCompleteNudgeWaitSec", 120);
    g_ProactiveProposalExpirySec            = sConfigMgr->GetOption<uint32_t>("OllamaChat.Proactive.ProposalExpirySec", 600);
    g_ProactivePreferredVoiceGuid           = sConfigMgr->GetOption<uint64_t>("OllamaChat.Proactive.PreferredVoiceGuid", 0);
    g_ProactiveProposerPromptFile           = sConfigMgr->GetOption<std::string>(
        "OllamaChat.Proactive.ProposerPromptFile",
        "modules/mod-ollama-chat/prompts/proactive_proposer.md");
    g_ProactiveMaxQuestRankSearch           = sConfigMgr->GetOption<uint32_t>("OllamaChat.Proactive.MaxQuestRankSearch", 50);
    g_ProactiveDungeonCandidates            = sConfigMgr->GetOption<std::string>("OllamaChat.Proactive.DungeonCandidates", "");
    g_ProactiveEnableAudit                  = sConfigMgr->GetOption<bool>("OllamaChat.Proactive.EnableAudit", true);
    g_ProactiveLevelUpCongratsEnable        = sConfigMgr->GetOption<bool>("OllamaChat.Proactive.LevelUpCongratsEnable", true);
    g_ProactiveLevelUpCongratsCooldownSec   = sConfigMgr->GetOption<uint32_t>("OllamaChat.Proactive.LevelUpCongratsCooldownSec", 60);
    g_ProactiveNoVoiceAuditEnable           = sConfigMgr->GetOption<bool>("OllamaChat.Proactive.NoVoiceAuditEnable", true);
    g_ProactiveNoVoiceAuditCooldownSec      = sConfigMgr->GetOption<uint32_t>("OllamaChat.Proactive.NoVoiceAuditCooldownSec", 300);

    // Proactive — LLM tiers (Phase A: intent classifier).
    g_ProactiveIntentEnable                 = sConfigMgr->GetOption<bool>("OllamaChat.ProactiveIntent.Enable", true);
    g_ProactiveIntentPromptFile             = sConfigMgr->GetOption<std::string>(
        "OllamaChat.ProactiveIntent.PromptFile",
        "modules/mod-ollama-chat/prompts/proactive_intent.md");
    g_ProactiveIntentMinConfidence          = sConfigMgr->GetOption<float>("OllamaChat.ProactiveIntent.MinConfidence", 0.7f);
    g_ProactiveIntentTimeoutMs              = sConfigMgr->GetOption<uint32_t>("OllamaChat.ProactiveIntent.TimeoutMs", 3000);

    // Proactive — LLM tiers (Phase B: gateway planner). Default OFF (paid).
    g_ProactivePlannerEnable                = sConfigMgr->GetOption<bool>("OllamaChat.ProactivePlanner.Enable", false);
    g_ProactivePlannerPromptFile            = sConfigMgr->GetOption<std::string>(
        "OllamaChat.ProactivePlanner.PromptFile",
        "modules/mod-ollama-chat/prompts/proactive_planner.md");

    // Proactive — LLM tiers (Phase C: local-Ollama tick gate). Default OFF.
    g_ProactiveTickGateEnable               = sConfigMgr->GetOption<bool>("OllamaChat.ProactiveTickGate.Enable", false);
    g_ProactiveTickGatePromptFile           = sConfigMgr->GetOption<std::string>(
        "OllamaChat.ProactiveTickGate.PromptFile",
        "modules/mod-ollama-chat/prompts/proactive_tick_gate.md");
    g_ProactiveTickGateIntervalSec          = sConfigMgr->GetOption<uint32_t>("OllamaChat.ProactiveTickGate.IntervalSec", 30);
    g_ProactiveTickGateTimeoutMs            = sConfigMgr->GetOption<uint32_t>("OllamaChat.ProactiveTickGate.TimeoutMs", 2000);
    g_ProactiveTickGateMinConfidence        = sConfigMgr->GetOption<float>("OllamaChat.ProactiveTickGate.MinConfidence", 0.6f);

    // jev decision tier. Default OFF everywhere; the code path is inert until
    // Jev.Enable=1 AND a site flag is on. Startup and `config_reload` both run
    // through here, so Jev::OnConfigReloaded() below is the single rebuild point.
    g_JevEnable                  = sConfigMgr->GetOption<bool>("OllamaChat.Jev.Enable", false);
    g_JevUrl                     = sConfigMgr->GetOption<std::string>("OllamaChat.Jev.Url", "https://api.typesafe.ai/v1/systemone");
    g_JevModel                   = sConfigMgr->GetOption<std::string>("OllamaChat.Jev.Model", "jev-1.13.0");
    g_JevApiKey                  = sConfigMgr->GetOption<std::string>("OllamaChat.Jev.ApiKey", "");
    g_JevProxy                   = sConfigMgr->GetOption<std::string>("OllamaChat.Jev.Proxy", "");
    g_JevProxyUser               = sConfigMgr->GetOption<std::string>("OllamaChat.Jev.ProxyUser", "");
    g_JevProxyPassword           = sConfigMgr->GetOption<std::string>("OllamaChat.Jev.ProxyPassword", "");   // ggignore: empty default, the live value lives only in the host bind-mount conf
    g_JevTimeoutMs               = sConfigMgr->GetOption<uint32_t>("OllamaChat.Jev.TimeoutMs", 1500);
    g_JevMaxConcurrent           = sConfigMgr->GetOption<uint32_t>("OllamaChat.Jev.MaxConcurrent", 2);
    g_JevBreakerCooldownSec      = sConfigMgr->GetOption<uint32_t>("OllamaChat.Jev.BreakerCooldownSec", 60);
    g_JevQuestionsFile           = sConfigMgr->GetOption<std::string>("OllamaChat.Jev.QuestionsFile", "modules/mod-ollama-chat/prompts/jev_questions.json");
    g_JevClassifierEnable        = sConfigMgr->GetOption<bool>("OllamaChat.Jev.Classifier.Enable", false);
    g_JevClassifierMinConfidence = sConfigMgr->GetOption<float>("OllamaChat.Jev.Classifier.MinConfidence", 0.8f);
    g_JevIntentEnable            = sConfigMgr->GetOption<bool>("OllamaChat.Jev.ProactiveIntent.Enable", false);
    g_JevIntentMinConfidence     = sConfigMgr->GetOption<float>("OllamaChat.Jev.ProactiveIntent.MinConfidence", 0.8f);
    g_JevTickGateEnable          = sConfigMgr->GetOption<bool>("OllamaChat.Jev.ProactiveTickGate.Enable", false);
    g_JevTickGateMinConfidence   = sConfigMgr->GetOption<float>("OllamaChat.Jev.ProactiveTickGate.MinConfidence", 0.6f);
    g_JevPlannerEnable           = sConfigMgr->GetOption<bool>("OllamaChat.Jev.ProactivePlanner.Enable", false);
    g_JevPlannerMinConfidence    = sConfigMgr->GetOption<float>("OllamaChat.Jev.ProactivePlanner.MinConfidence", 0.6f);
    g_JevTacticalEnable          = sConfigMgr->GetOption<bool>("OllamaChat.Jev.Tactical.Enable", false);
    g_JevTacticalMinConfidence   = sConfigMgr->GetOption<float>("OllamaChat.Jev.Tactical.MinConfidence", 0.6f);
    g_JevTacticalEscalateMin     = sConfigMgr->GetOption<float>("OllamaChat.Jev.Tactical.EscalateMin", 0.8f);
    g_JevPlaybookEnable          = sConfigMgr->GetOption<bool>("OllamaChat.Jev.Playbook.Enable", false);
    g_JevPlaybookMinRelevance    = sConfigMgr->GetOption<float>("OllamaChat.Jev.Playbook.MinRelevance", 0.3f);
    g_JevPlaybookMinRows         = sConfigMgr->GetOption<uint32_t>("OllamaChat.Jev.Playbook.MinRows", 4);
    g_JevPlaybookMaxRows         = sConfigMgr->GetOption<uint32_t>("OllamaChat.Jev.Playbook.MaxRows", 12);
    g_JevPlaybookTimeoutMs       = sConfigMgr->GetOption<uint32_t>("OllamaChat.Jev.Playbook.TimeoutMs", 4000);

    ollamachat::proactive::OnConfigReloaded();
    Jev::OnConfigReloaded();

    if (g_TacticalEnable)
    {
        LOG_INFO("server.loading",
                 "[Ollama Chat Tactical] config loaded: bots={}, url=\"{}\", model=\"{}\", heartbeat={}ms, "
                 "cooldown={}ms, overrides={}, humanPresenceRequired={}, allowCombatOverride={}, "
                 "ambient={}, autoNearby={} max={} radius={}, requireConfirmationFor={}, "
                 "classifierModel=\"{}\", classifierTimeout={}ms, escalationCap={}/hr/bot, escalationCooldown={}s",
                 g_TacticalBotGUIDSet.size(), g_TacticalUrl, g_TacticalModel,
                 g_TacticalHeartbeatMs, g_TacticalBotCooldownMs,
                 g_TacticalBotConfigs.size(),
                 g_TacticalHumanPresenceRequired, g_TacticalAllowCombatOverride,
                 g_TacticalAmbientEnable, g_TacticalAutoEnrollNearbyBots,
                 g_TacticalNearbyBotMax, g_TacticalNearbyBotRadius,
                 g_TacticalRequireConfirmationSet.size(),
                 g_GatewayOllamaClassifierModel.empty() ? g_TacticalModel : g_GatewayOllamaClassifierModel,
                 g_GatewayOllamaClassifierTimeoutMs,
                 g_StrategicMaxEscalationsPerBotPerHour,
                 g_StrategicEscalationCooldownSec);
    }

    std::string gatewayBotGUIDs       = sConfigMgr->GetOption<std::string>("OllamaChat.Gateway.BotGUIDs", "");
    ParseGatewayBotGUIDs(gatewayBotGUIDs);

    // PR5: prefer JSON overrides if provided; fall back to legacy colon-split otherwise.
    // Tier 9: also support BotOverridesJsonFile — reads JSON from an external file. Necessary
    // because AC's Config strips ALL `"` from values (Config.cpp:327), making inline JSON
    // unusable. Priority: JsonFile > inline JSON > legacy colon string.
    std::string gatewayBotOverridesJsonFile = sConfigMgr->GetOption<std::string>("OllamaChat.Gateway.BotOverridesJsonFile", "");
    std::string gatewayBotOverridesJson     = sConfigMgr->GetOption<std::string>("OllamaChat.Gateway.BotOverridesJson", "");
    std::string gatewayBotOverrides         = sConfigMgr->GetOption<std::string>("OllamaChat.Gateway.BotOverrides", "");

    if (!gatewayBotOverridesJsonFile.empty())
    {
        std::ifstream ifs(gatewayBotOverridesJsonFile);
        if (ifs.is_open())
        {
            std::stringstream ss;
            ss << ifs.rdbuf();
            std::string fileContents = ss.str();
            if (!fileContents.empty())
            {
                LOG_INFO("server.loading", "[Ollama Chat] Gateway: loaded BotOverridesJson from file '{}' ({} bytes)",
                         gatewayBotOverridesJsonFile, fileContents.size());
                if (!gatewayBotOverridesJson.empty() || !gatewayBotOverrides.empty())
                    LOG_WARN("server.loading", "[Ollama Chat] Gateway: BotOverridesJsonFile takes precedence over BotOverridesJson and BotOverrides");
                ParseGatewayBotOverridesJson(fileContents);
            }
            else
            {
                LOG_ERROR("server.loading", "[Ollama Chat] Gateway: BotOverridesJsonFile '{}' is empty — falling back",
                          gatewayBotOverridesJsonFile);
                gatewayBotOverridesJsonFile.clear();
            }
        }
        else
        {
            LOG_ERROR("server.loading", "[Ollama Chat] Gateway: BotOverridesJsonFile '{}' could not be opened — falling back",
                      gatewayBotOverridesJsonFile);
            gatewayBotOverridesJsonFile.clear();
        }
    }
    if (gatewayBotOverridesJsonFile.empty())
    {
        if (!gatewayBotOverridesJson.empty())
        {
            if (!gatewayBotOverrides.empty())
                LOG_WARN("server.loading", "[Ollama Chat] Gateway: both BotOverridesJson and BotOverrides set — using JSON");
            ParseGatewayBotOverridesJson(gatewayBotOverridesJson);
        }
        else
        {
            ParseGatewayBotOverrides(gatewayBotOverrides);
        }
    }

    std::string gatewayWhitelist = sConfigMgr->GetOption<std::string>("OllamaChat.Gateway.WhitelistAccountIds", "");
    ParseGatewayWhitelist(gatewayWhitelist);

    // Bot promotion. After the overrides + whitelist + fleet keys: the template
    // is a copy of a bot's override, and eligibility excludes the fleet.
    g_PromoteEnable          = sConfigMgr->GetOption<bool>("OllamaChat.Gateway.Promote.Enable", false);
    g_PromoteChatTtlSec      = sConfigMgr->GetOption<uint32_t>("OllamaChat.Gateway.Promote.ChatTtlSec", 600);
    g_PromoteMaxChatBots     = sConfigMgr->GetOption<uint32_t>("OllamaChat.Gateway.Promote.MaxChatBots", 5);
    g_PromotePartyEnable     = sConfigMgr->GetOption<bool>("OllamaChat.Gateway.Promote.PartyEnable", true);
    g_PromoteTemplateBotGuid = sConfigMgr->GetOption<uint32_t>("OllamaChat.Gateway.Promote.TemplateBotGuid", 0);
    g_PromoteChannels        = sConfigMgr->GetOption<std::string>("OllamaChat.Gateway.Promote.Channels",
                                                                   "whisper,party,raid,guild,officer,say,yell,general");
    OllamaChat::Promotion::OnConfigLoaded();

    // Validate gateway config
    if (g_GatewayEnable)
    {
        // Global URL/token can be empty if all bots have per-bot overrides
        bool hasGlobalConfig = !g_GatewayUrl.empty() && !g_GatewayBearerToken.empty();
        bool hasPerBotConfigs = !g_GatewayBotConfigs.empty();

        if (!hasGlobalConfig && !hasPerBotConfigs)
        {
            LOG_ERROR("server.loading", "[Ollama Chat] Gateway enabled but no global URL/Token and no per-bot overrides — disabling gateway");
            g_GatewayEnable = false;
        }
        else
        {
            LOG_INFO("server.loading", "[Ollama Chat] Gateway enabled: GlobalURL={}, Model={}, TriggerKeyword='{}', BotGUIDs count={}, BotOverrides count={}, legacyFallbackToOllama(no-op)={}, Whitelist count={}",
                     g_GatewayUrl, g_GatewayModel, g_GatewayTriggerKeyword, g_GatewayBotGUIDSet.size(), g_GatewayBotConfigs.size(), g_GatewayFallbackToOllama, g_GatewayWhitelistAccountIds.size());

            if (g_GatewayWhitelistAccountIds.empty())
            {
                LOG_WARN("server.loading", "[Ollama Chat] Gateway enabled WITHOUT account whitelist — ALL players on this server can trigger gateway bots. Set OllamaChat.Gateway.WhitelistAccountIds to restrict.");
            }
        }
    }

    g_EventTypeDefeated           = sConfigMgr->GetOption<std::string>("OllamaChat.EventTypeDefeated", "");
    g_EventTypeDefeatedPlayer     = sConfigMgr->GetOption<std::string>("OllamaChat.EventTypeDefeatedPlayer", "");
    g_EventTypePetDefeated        = sConfigMgr->GetOption<std::string>("OllamaChat.EventTypePetDefeated", "");
    g_EventTypeGotItem            = sConfigMgr->GetOption<std::string>("OllamaChat.EventTypeGotItem", "");
    g_EventTypeDied               = sConfigMgr->GetOption<std::string>("OllamaChat.EventTypeDied", "");
    g_EventTypeCompletedQuest     = sConfigMgr->GetOption<std::string>("OllamaChat.EventTypeCompletedQuest", "");
    g_EventTypeLearnedSpell       = sConfigMgr->GetOption<std::string>("OllamaChat.EventTypeLearnedSpell", "");
    g_EventTypeRequestedDuel      = sConfigMgr->GetOption<std::string>("OllamaChat.EventTypeRequestedDuel", "");
    g_EventTypeStartedDueling     = sConfigMgr->GetOption<std::string>("OllamaChat.EventTypeStartedDueling", "");
    g_EventTypeWonDuel            = sConfigMgr->GetOption<std::string>("OllamaChat.EventTypeWonDuel", "");
    g_EventTypeLeveledUp          = sConfigMgr->GetOption<std::string>("OllamaChat.EventTypeLeveledUp", "");
    g_EventTypeAchievement        = sConfigMgr->GetOption<std::string>("OllamaChat.EventTypeAchievement", "");
    g_EventTypeUsedObject         = sConfigMgr->GetOption<std::string>("OllamaChat.EventTypeUsedObject", "");


    // Load extra blacklist commands from config (comma-separated list)
    std::string extraBlacklist = sConfigMgr->GetOption<std::string>("OllamaChat.BlacklistCommands", "");
    if (!extraBlacklist.empty())
    {
        std::vector<std::string> extraList = SplitString(extraBlacklist, ',');
        for (const auto& cmd : extraList)
        {
            g_BlacklistCommands.push_back(cmd);
        }
    }

    LoadPersonalityTemplatesFromDB();

    g_queryManager.setMaxConcurrentQueries(g_MaxConcurrentQueries);

    // Loads the environment random chatter message templates for each type.
    // Each config option is a pipe-separated list of string templates,
    // using {} as a placeholder for named substitutions.
    // Helper to load a multi-line config option into a std::vector<std::string>
    auto LoadEnvCommentVector = [](const char* key, const std::vector<std::string>& defaults = {}) -> std::vector<std::string>
    {
        std::string val = sConfigMgr->GetOption<std::string>(key, "");
        std::vector<std::string> result;
        std::istringstream iss(val);
        std::string token;
        while (std::getline(iss, token, '|')) { // Split by '|'
            // Trim whitespace from token
            size_t start = token.find_first_not_of(" \t\r\n");
            size_t end = token.find_last_not_of(" \t\r\n");
            if (start != std::string::npos && end != std::string::npos)
                result.push_back(token.substr(start, end - start + 1));
        }
        if (result.empty() && !defaults.empty())
            return defaults;
        return result;
    };

    g_EnvCommentCreature        = LoadEnvCommentVector("OllamaChat.EnvCommentCreature", { "" });
    g_EnvCommentGameObject      = LoadEnvCommentVector("OllamaChat.EnvCommentGameObject", { "" });
    g_EnvCommentEquippedItem    = LoadEnvCommentVector("OllamaChat.EnvCommentEquippedItem", { "" });
    g_EnvCommentBagItem         = LoadEnvCommentVector("OllamaChat.EnvCommentBagItem", { "" });
    g_EnvCommentBagItemSell     = LoadEnvCommentVector("OllamaChat.EnvCommentBagItemSell", { "" });
    g_EnvCommentSpell           = LoadEnvCommentVector("OllamaChat.EnvCommentSpell", { "" });
    g_EnvCommentQuestArea       = LoadEnvCommentVector("OllamaChat.EnvCommentQuestArea", { "" });
    g_EnvCommentVendor          = LoadEnvCommentVector("OllamaChat.EnvCommentVendor", { "" });
    g_EnvCommentQuestgiver      = LoadEnvCommentVector("OllamaChat.EnvCommentQuestgiver", { "" });
    g_EnvCommentBagSlots        = LoadEnvCommentVector("OllamaChat.EnvCommentBagSlots", { "" });
    g_EnvCommentDungeon         = LoadEnvCommentVector("OllamaChat.EnvCommentDungeon", { "" });
    g_EnvCommentUnfinishedQuest = LoadEnvCommentVector("OllamaChat.EnvCommentUnfinishedQuest", { "" });

    // Guild-specific random chatter templates
    g_GuildEnvCommentGuildMember = LoadEnvCommentVector("OllamaChat.GuildEnvCommentGuildMember", { "" });
    g_GuildEnvCommentGuildRank = LoadEnvCommentVector("OllamaChat.GuildEnvCommentGuildRank", { "" });
    g_GuildEnvCommentGuildBank = LoadEnvCommentVector("OllamaChat.GuildEnvCommentGuildBank", { "" });
    g_GuildEnvCommentGuildMOTD = LoadEnvCommentVector("OllamaChat.GuildEnvCommentGuildMOTD", { "" });
    g_GuildEnvCommentGuildInfo = LoadEnvCommentVector("OllamaChat.GuildEnvCommentGuildInfo", { "" });
    g_GuildEnvCommentGuildOnlineMembers = LoadEnvCommentVector("OllamaChat.GuildEnvCommentGuildOnlineMembers", { "" });
    g_GuildEnvCommentGuildRaid = LoadEnvCommentVector("OllamaChat.GuildEnvCommentGuildRaid", { "" });
    g_GuildEnvCommentGuildEndgame = LoadEnvCommentVector("OllamaChat.GuildEnvCommentGuildEndgame", { "" });
    g_GuildEnvCommentGuildStrategy = LoadEnvCommentVector("OllamaChat.GuildEnvCommentGuildStrategy", { "" });
    g_GuildEnvCommentGuildGroup = LoadEnvCommentVector("OllamaChat.GuildEnvCommentGuildGroup", { "" });
    g_GuildEnvCommentGuildPvP = LoadEnvCommentVector("OllamaChat.GuildEnvCommentGuildPvP", { "" });
    g_GuildEnvCommentGuildCommunity = LoadEnvCommentVector("OllamaChat.GuildEnvCommentGuildCommunity", { "" });

    // Guild-specific configuration
    g_EnableGuildEventChatter = sConfigMgr->GetOption<bool>("OllamaChat.EnableGuildEventChatter", true);
    g_GuildChatterBotCommentChance = sConfigMgr->GetOption<uint32_t>("OllamaChat.GuildChatterBotCommentChance", 25);
    g_GuildChatterMaxBotsPerEvent = sConfigMgr->GetOption<uint32_t>("OllamaChat.GuildChatterMaxBotsPerEvent", 2);

    // Guild-specific event templates
    g_GuildEventTypeLevelUp = sConfigMgr->GetOption<std::string>("OllamaChat.GuildEventTypeLevelUp", "");
    g_GuildEventTypeDungeonComplete = sConfigMgr->GetOption<std::string>("OllamaChat.GuildEventTypeDungeonComplete", "");
    g_GuildEventTypeEpicGear = sConfigMgr->GetOption<std::string>("OllamaChat.GuildEventTypeEpicGear", "");
    g_GuildEventTypeRareGear = sConfigMgr->GetOption<std::string>("OllamaChat.GuildEventTypeRareGear", "");
    g_GuildEventTypeGuildJoin = sConfigMgr->GetOption<std::string>("OllamaChat.GuildEventTypeGuildJoin", "");
    g_GuildEventTypeGuildLogin = sConfigMgr->GetOption<std::string>("OllamaChat.GuildEventTypeGuildLogin", "");
    g_GuildEventTypeGuildLeave = sConfigMgr->GetOption<std::string>("OllamaChat.GuildEventTypeGuildLeave", "");
    g_GuildEventTypeGuildPromotion = sConfigMgr->GetOption<std::string>("OllamaChat.GuildEventTypeGuildPromotion", "");
    g_GuildEventTypeGuildDemotion = sConfigMgr->GetOption<std::string>("OllamaChat.GuildEventTypeGuildDemotion", "");

    // Load chance variables for normal events
    g_EventTypeDefeated_Chance = sConfigMgr->GetOption<int>("OllamaChat.EventTypeDefeated_Chance", 0);
    g_EventTypeDefeatedPlayer_Chance = sConfigMgr->GetOption<int>("OllamaChat.EventTypeDefeatedPlayer_Chance", 0);
    g_EventTypePetDefeated_Chance = sConfigMgr->GetOption<int>("OllamaChat.EventTypePetDefeated_Chance", 0);
    g_EventTypeGotItem_Chance = sConfigMgr->GetOption<int>("OllamaChat.EventTypeGotItem_Chance", 0);
    g_EventTypeDied_Chance = sConfigMgr->GetOption<int>("OllamaChat.EventTypeDied_Chance", 0);
    g_EventTypeCompletedQuest_Chance = sConfigMgr->GetOption<int>("OllamaChat.EventTypeCompletedQuest_Chance", 0);
    g_EventTypeLearnedSpell_Chance = sConfigMgr->GetOption<int>("OllamaChat.EventTypeLearnedSpell_Chance", 0);
    g_EventTypeRequestedDuel_Chance = sConfigMgr->GetOption<int>("OllamaChat.EventTypeRequestedDuel_Chance", 0);
    g_EventTypeStartedDueling_Chance = sConfigMgr->GetOption<int>("OllamaChat.EventTypeStartedDueling_Chance", 0);
    g_EventTypeWonDuel_Chance = sConfigMgr->GetOption<int>("OllamaChat.EventTypeWonDuel_Chance", 0);
    g_EventTypeLeveledUp_Chance = sConfigMgr->GetOption<int>("OllamaChat.EventTypeLeveledUp_Chance", 0);
    g_EventTypeAchievement_Chance = sConfigMgr->GetOption<int>("OllamaChat.EventTypeAchievement_Chance", 0);
    g_EventTypeUsedObject_Chance = sConfigMgr->GetOption<int>("OllamaChat.EventTypeUsedObject_Chance", 0);

    // Load chance variables for guild events
    g_GuildEventTypeEpicGear_Chance = sConfigMgr->GetOption<int>("OllamaChat.GuildEventTypeEpicGear_Chance", 0);
    g_GuildEventTypeRareGear_Chance = sConfigMgr->GetOption<int>("OllamaChat.GuildEventTypeRareGear_Chance", 0);
    g_GuildEventTypeGuildJoin_Chance = sConfigMgr->GetOption<int>("OllamaChat.GuildEventTypeGuildJoin_Chance", 0);
    g_GuildEventTypeGuildLogin_Chance = sConfigMgr->GetOption<int>("OllamaChat.GuildEventTypeGuildLogin_Chance", 0);
    g_GuildEventTypeGuildLeave_Chance = sConfigMgr->GetOption<int>("OllamaChat.GuildEventTypeGuildLeave_Chance", 0);
    g_GuildEventTypeGuildPromotion_Chance = sConfigMgr->GetOption<int>("OllamaChat.GuildEventTypeGuildPromotion_Chance", 0);
    g_GuildEventTypeGuildDemotion_Chance = sConfigMgr->GetOption<int>("OllamaChat.GuildEventTypeGuildDemotion_Chance", 0);
    g_GuildEventTypeGuildAchievement_Chance = sConfigMgr->GetOption<int>("OllamaChat.GuildEventTypeGuildAchievement_Chance", 0);
    g_GuildEventTypeLevelUp_Chance = sConfigMgr->GetOption<int>("OllamaChat.GuildEventTypeLevelUp_Chance", 0);
    g_GuildEventTypeDungeonComplete_Chance = sConfigMgr->GetOption<int>("OllamaChat.GuildEventTypeDungeonComplete_Chance", 0);


    // Cooldown time for events
    g_EventCooldownTime = sConfigMgr->GetOption<uint32_t>("OllamaChat.EventCooldownTime", 10);

    // Channel disable settings
    g_DisableForCustomChannels = sConfigMgr->GetOption<bool>("OllamaChat.DisableForCustomChannels", false);
    g_DisableForSayYell = sConfigMgr->GetOption<bool>("OllamaChat.DisableForSayYell", false);
    g_DisableForGuild = sConfigMgr->GetOption<bool>("OllamaChat.DisableForGuild", false);
    g_DisableForParty = sConfigMgr->GetOption<bool>("OllamaChat.DisableForParty", false);

    LOG_INFO("server.loading",
             "[Ollama Chat] Config loaded: Enabled = {}, SayDistance = {}, YellDistance = {}, "
             "Reply Chances - Say: P{}%/B{}%, Channel: P{}%/B{}%, Party: P{}%/B{}%, Guild: P{}%/B{}%, MaxBotsToPick = {}, "
             "legacy Url = {}, legacy Model = {}, legacy MaxConcurrentQueries = {}, Tactical.Enable = {}, "
             "Tactical.AmbientEnable = {}, Tactical.AutoEnrollNearbyBots = {}, Tactical.NearbyBotMax = {}, "
             "Extra blacklist commands: {}",
             g_Enable, g_SayDistance, g_YellDistance,
             g_PlayerReplyChance_Say, g_BotReplyChance_Say,
             g_PlayerReplyChance_Channel, g_BotReplyChance_Channel,
             g_PlayerReplyChance_Party, g_BotReplyChance_Party,
             g_PlayerReplyChance_Guild, g_BotReplyChance_Guild,
             g_MaxBotsToPick,
             g_OllamaUrl, g_OllamaModel, g_MaxConcurrentQueries,
             g_TacticalEnable, g_TacticalAmbientEnable,
             g_TacticalAutoEnrollNearbyBots, g_TacticalNearbyBotMax,
             extraBlacklist);
}

// Full runtime reload: re-reads every .conf through sConfigMgr, re-populates our
// globals, refreshes DB-backed caches, and restarts the MCP server. Called by
// both the `.ollama reload` chat command and the `config_reload` MCP tool.
void ReloadOllamaChatRuntime()
{
    // Stop tactical threads BEFORE mutating Tactical.* globals. Otherwise the
    // running tick can observe half-updated config (e.g. new URL paired with
    // old bot allowlist) while LoadOllamaChatConfig() rewrites them without
    // synchronization.
    TacticalLeaderTick::Instance().Stop();
    StrategicEscalationWorker::Instance().Stop();

    sConfigMgr->Reload();
    LoadOllamaChatConfig();

    if (!g_EnableRPPersonalities)
    {
        ClearAllBotPersonalities();
    }

    LoadBotPersonalityList();
    LoadBotConversationHistoryFromDB();
    LoadPlayerPreferencesFromDB();
    InitializeSentimentTracking();
    ResetGatewayRuntimeState();

    // NOTE: do NOT Stop+Start the MCP HTTP listener here. Reload is most often
    // called via the `config_reload` MCP tool itself — i.e. ON an MCP request
    // thread. Stopping the server while one of its handler threads is mid-flight
    // hangs `m_thread.join()` indefinitely (cpp-httplib's stop() does not kill
    // active handler threads), the reload thread blocks, and from the outside
    // the entire MCP server appears dead. Verified by `/proc/net/tcp` inside
    // ac-worldserver showing no 18790 listener after a reload, plus the missing
    // "MCP stopped"/"MCP starting" log lines that Stop()/Start() would emit.
    //
    // Everything that *can* be reloaded without rebinding the socket already is:
    // tool registries, leader allowlists, ops settings, deny patterns, etc. all
    // read from globals at request dispatch time. The only conf keys that need
    // a fresh listener are Mcp.BindAddress and Mcp.Port — those are rare and
    // require a full worldserver restart (`gh workflow run deploy.yml`).
    // Mcp.BearerToken is also read on every request, so token rotation works
    // through this reload path without any restart.

    // Safe to start tactical threads again now that every g_Tactical* /
    // g_Strategic* global reflects the reloaded .conf.
    TacticalLeaderTick::Instance().Start();
    StrategicEscalationWorker::Instance().Start();
}

void LoadPersonalityTemplatesFromDB()
{
    g_PersonalityPrompts.clear();
    g_PersonalityKeys.clear();
    g_PersonalityKeysRandomOnly.clear();

    QueryResult result = CharacterDatabase.Query("SELECT `key`, `prompt`, `manual_only` FROM `mod_ollama_chat_personality_templates`");
    if (!result)
    {
        LOG_ERROR("server.loading", "[Ollama Chat] No personality templates found in the database!");
        return;
    }

    do
    {
        std::string key = (*result)[0].Get<std::string>();
        std::string prompt = (*result)[1].Get<std::string>();
        bool manualOnly = (*result)[2].Get<bool>();
        
        g_PersonalityPrompts[key] = prompt;
        g_PersonalityKeys.push_back(key);
        
        // Only add to random pool if not manual_only
        if (!manualOnly)
        {
            g_PersonalityKeysRandomOnly.push_back(key);
        }
    } while (result->NextRow());

    LOG_INFO("server.loading", "[Ollama Chat] Cached {} personalities ({} available for random assignment).", 
             g_PersonalityKeys.size(), g_PersonalityKeysRandomOnly.size());
}

void LoadBotConversationHistoryFromDB()
{
    QueryResult result = CharacterDatabase.Query(
        "SELECT bot_guid, player_guid, player_message, bot_reply FROM mod_ollama_chat_history ORDER BY timestamp ASC"
    );
    if (!result)
        return;

    std::lock_guard<std::mutex> lock(g_ConversationHistoryMutex);
    g_BotConversationHistory.clear();

    do {
        uint64_t botGuid = (*result)[0].Get<uint64_t>();
        uint64_t playerGuid = (*result)[1].Get<uint64_t>();
        std::string playerMsg = (*result)[2].Get<std::string>();
        std::string botReply = (*result)[3].Get<std::string>();

        auto& playerHistory = g_BotConversationHistory[botGuid][playerGuid];
        playerHistory.push_back({ playerMsg, botReply });
        while (playerHistory.size() > g_MaxConversationHistory)
        {
            playerHistory.pop_front();
        }

    } while (result->NextRow());

}


// Definition of the configuration WorldScript.
OllamaChatConfigWorldScript::OllamaChatConfigWorldScript() : WorldScript("OllamaChatConfigWorldScript") { }

void OllamaChatConfigWorldScript::OnStartup()
{
    LoadOllamaChatConfig();
    LoadBotPersonalityList();
    LoadBotConversationHistoryFromDB();
    LoadPlayerPreferencesFromDB();
    InitializeSentimentTracking();
    PruneGatewayAuditRows();
    PruneTacticalAuditRows();
    Jev::EnsureAuditBackendColumns();   // idempotent; the image never auto-applies data/sql updates

    // Initialize RAG system if enabled
    if (g_EnableRAG) {
        if (g_RAGSystem) {
            delete g_RAGSystem;
        }
        g_RAGSystem = new OllamaRAGSystem();
        if (!g_RAGSystem->Initialize()) {
            LOG_ERROR("server.loading", "[Ollama Chat] Failed to initialize RAG system");
            delete g_RAGSystem;
            g_RAGSystem = nullptr;
        } else {
            LOG_INFO("server.loading", "[Ollama Chat] RAG system initialized successfully");
        }
    }

    // Start the embedded MCP server (no-op if Mcp.Enable=0).
    GatewayMcpServer::Instance().Start();
    // Tier 10 — autonomous leader tick (no-op unless Mcp.Leader.AutoTickEnable=1).
    GatewayLeaderTick::Instance().Start();
    // Companion-mode tactical layer (no-op unless Tactical.Enable=1).
    TacticalLeaderTick::Instance().Start();
    StrategicEscalationWorker::Instance().Start();
    // Phase 1 — Overlord headless master auto-login (no-op unless Overlord.Enable=1).
    OllamaChat::Overlord::ScheduleStartupLogin();
}

void OllamaChatConfigWorldScript::OnUpdate(uint32 diff)
{
    // Fleet party/master reconciliation — world thread, interval-gated inside
    // Tick, no-op unless OllamaChat.Fleet.EnsureParty=1.
    OllamaChat::Fleet::Tick(diff);

    // Bot promotion: party guests re-derived from live groups, chat windows
    // expired. World thread, interval-gated inside Tick.
    OllamaChat::Promotion::Tick(diff);

    // World-thread tasks posted by MCP tools / the gateway worker (worldtask.h).
    OllamaChat::WorldTask::Drain();
}

void OllamaChatConfigWorldScript::OnShutdown()
{
    // Stop tactical layer first — its HTTP calls must not outlive globals.
    StrategicEscalationWorker::Instance().Stop();
    TacticalLeaderTick::Instance().Stop();
    // Stop the leader tick — its detached gateway requests must not fire after shutdown.
    GatewayLeaderTick::Instance().Stop();
    // Stop the MCP listener so in-flight tool calls don't outlive globals.
    GatewayMcpServer::Instance().Stop();

    // Clean up RAG system
    if (g_RAGSystem) {
        delete g_RAGSystem;
        g_RAGSystem = nullptr;
        LOG_INFO("server.loading", "[Ollama Chat] RAG system cleaned up");
    }
}
