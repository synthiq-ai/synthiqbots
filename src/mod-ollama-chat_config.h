#ifndef MOD_OLLAMA_CHAT_CONFIG_H
#define MOD_OLLAMA_CHAT_CONFIG_H

#include <string>
#include <cstdint>
#include <vector>
#include <deque>
#include <unordered_map>
#include <unordered_set>
#include <mutex>
#include <atomic>
#include <ctime>
#include "ScriptMgr.h"  // Ensure WorldScript is defined

// --------------------------------------------
// Distance/Range Configuration
// --------------------------------------------
extern float      g_SayDistance;
extern float      g_YellDistance;
extern float      g_RandomChatterRealPlayerDistance;
extern float      g_EventChatterRealPlayerDistance;

// --------------------------------------------
// Bot/Player Chatter Probability & Limits
// --------------------------------------------
// Per-channel-type reply chances
extern uint32_t   g_PlayerReplyChance_Say;
extern uint32_t   g_BotReplyChance_Say;
extern uint32_t   g_PlayerReplyChance_Channel;
extern uint32_t   g_BotReplyChance_Channel;
extern uint32_t   g_PlayerReplyChance_Party;
extern uint32_t   g_BotReplyChance_Party;
extern uint32_t   g_PlayerReplyChance_Guild;
extern uint32_t   g_BotReplyChance_Guild;

extern uint32_t   g_MaxBotsToPick;
extern uint32_t   g_RandomChatterBotCommentChance;
extern uint32_t   g_RandomChatterMaxBotsPerPlayer;
extern uint32_t   g_EventChatterBotCommentChance;
extern uint32_t   g_EventChatterBotSelfCommentChance;
extern uint32_t   g_EventChatterMaxBotsPerPlayer;

// --------------------------------------------
// Ollama LLM API Configuration
// --------------------------------------------
extern std::string g_OllamaUrl;
extern std::string g_OllamaModel;
extern uint32_t    g_OllamaNumPredict;
extern float       g_OllamaTemperature;
extern float       g_OllamaTopP;
extern float       g_OllamaRepeatPenalty;
extern uint32_t    g_OllamaNumCtx;
extern uint32_t    g_OllamaNumThreads;
extern std::string g_OllamaStop;
extern std::string g_OllamaSystemPrompt;
extern std::string g_OllamaSeed;

// --------------------------------------------
// Concurrency/Queueing
// --------------------------------------------
extern uint32_t    g_MaxConcurrentQueries;

// --------------------------------------------
// Feature Toggles & Core Settings
// --------------------------------------------
extern bool        g_Enable;
extern bool        g_DisableRepliesInCombat;
extern bool        g_DisableOllamaResponses;
extern bool        g_EnableRandomChatter;
extern bool        g_EnableEventChatter;
extern bool        g_EnableRPPersonalities;
extern bool        g_EnableWhisperReplies;
extern bool        g_DebugEnabled;
extern bool        g_DebugShowFullPrompt;

// --------------------------------------------
// Random Chatter Timing
// --------------------------------------------
extern uint32_t    g_MinRandomInterval;
extern uint32_t    g_MaxRandomInterval;

// --------------------------------------------
// Conversation History Settings
// --------------------------------------------
extern uint32_t    g_MaxConversationHistory;
extern uint32_t    g_ConversationHistorySaveInterval;

// --------------------------------------------
// Prompt Templates
// --------------------------------------------
extern std::string g_RandomChatterPromptTemplate;
extern std::vector<std::string> g_RandomChatterPromptVariations;
extern std::vector<std::string> g_RandomChatterQuestionVariations;
extern std::string g_EventChatterPromptTemplate;
extern std::string g_ChatPromptTemplate;
extern std::string g_ChatExtraInfoTemplate;

// --------------------------------------------
// Personality and Prompt Data
// --------------------------------------------
extern std::unordered_map<uint64_t, std::string> g_BotPersonalityList;
extern std::unordered_map<std::string, std::string> g_PersonalityPrompts;
extern std::vector<std::string> g_PersonalityKeys;
extern std::vector<std::string> g_PersonalityKeysRandomOnly; // Personalities that can be randomly assigned
extern std::string g_DefaultPersonalityPrompt;

// --------------------------------------------
// Chat History Templates and Toggles
// --------------------------------------------
extern bool        g_EnableChatHistory;
extern std::string g_ChatHistoryHeaderTemplate;
extern std::string g_ChatHistoryLineTemplate;
extern std::string g_ChatHistoryFooterTemplate;

// --------------------------------------------
// Chatbot Snapshot Template
// --------------------------------------------
extern bool        g_EnableChatBotSnapshotTemplate;
extern std::string g_ChatBotSnapshotTemplate;

// --------------------------------------------
// Conversation History Store and Mutex
// --------------------------------------------
extern std::unordered_map<uint64_t, std::unordered_map<uint64_t, std::deque<std::pair<std::string, std::string>>>> g_BotConversationHistory;
extern std::mutex   g_ConversationHistoryMutex;
extern time_t       g_LastHistorySaveTime;

// --------------------------------------------
// Blacklist: Prefixes for Commands (not chat)
// --------------------------------------------
extern std::vector<std::string> g_BlacklistCommands;

// --------------------------------------------
// Think Mode Support
// --------------------------------------------
extern bool g_ThinkModeEnableForModule;

// --------------------------------------------
// Environment/Contextual Random Chatter Templates
// --------------------------------------------
extern std::vector<std::string> g_EnvCommentCreature;
extern std::vector<std::string> g_EnvCommentGameObject;
extern std::vector<std::string> g_EnvCommentEquippedItem;
extern std::vector<std::string> g_EnvCommentBagItem;
extern std::vector<std::string> g_EnvCommentBagItemSell;
extern std::vector<std::string> g_EnvCommentSpell;
extern std::vector<std::string> g_EnvCommentQuestArea;
extern std::vector<std::string> g_EnvCommentVendor;
extern std::vector<std::string> g_EnvCommentQuestgiver;
extern std::vector<std::string> g_EnvCommentBagSlots;
extern std::vector<std::string> g_EnvCommentDungeon;
extern std::vector<std::string> g_EnvCommentUnfinishedQuest;

// --------------------------------------------
// Guild-Specific Random Chatter Templates
// --------------------------------------------
extern std::vector<std::string> g_GuildEnvCommentGuildMember;
extern std::vector<std::string> g_GuildEnvCommentGuildRank;
extern std::vector<std::string> g_GuildEnvCommentGuildBank;
extern std::vector<std::string> g_GuildEnvCommentGuildMOTD;
extern std::vector<std::string> g_GuildEnvCommentGuildInfo;
extern std::vector<std::string> g_GuildEnvCommentGuildOnlineMembers;
extern std::vector<std::string> g_GuildEnvCommentGuildRaid;
extern std::vector<std::string> g_GuildEnvCommentGuildEndgame;
extern std::vector<std::string> g_GuildEnvCommentGuildStrategy;
extern std::vector<std::string> g_GuildEnvCommentGuildGroup;
extern std::vector<std::string> g_GuildEnvCommentGuildPvP;
extern std::vector<std::string> g_GuildEnvCommentGuildCommunity;

// --------------------------------------------
// Guild-Specific Random Chatter Configuration
// --------------------------------------------
extern bool        g_EnableGuildEventChatter;
extern bool        g_EnableGuildRandomAmbientChatter;
extern uint32_t    g_GuildRandomChatterChance;
extern uint32_t    g_GuildChatterBotCommentChance;
extern uint32_t    g_GuildChatterMaxBotsPerEvent;

// --------------------------------------------
// Guild-Specific Event Chatter Templates
// --------------------------------------------
extern std::string g_GuildEventTypeLevelUp;
extern std::string g_GuildEventTypeDungeonComplete;
extern std::string g_GuildEventTypeEpicGear;
extern std::string g_GuildEventTypeRareGear;
extern std::string g_GuildEventTypeGuildJoin;
extern std::string g_GuildEventTypeGuildLeave;
extern std::string g_GuildEventTypeGuildPromotion;
extern std::string g_GuildEventTypeGuildDemotion;
extern std::string g_GuildEventTypeGuildLogin;
extern std::string g_GuildEventTypeGuildAchievement;

// Chance variables for normal events
extern int g_EventTypeDefeated_Chance;
extern int g_EventTypeDefeatedPlayer_Chance;
extern int g_EventTypePetDefeated_Chance;
extern int g_EventTypeGotItem_Chance;
extern int g_EventTypeDied_Chance;
extern int g_EventTypeCompletedQuest_Chance;
extern int g_EventTypeLearnedSpell_Chance;
extern int g_EventTypeRequestedDuel_Chance;
extern int g_EventTypeStartedDueling_Chance;
extern int g_EventTypeWonDuel_Chance;
extern int g_EventTypeLeveledUp_Chance;
extern int g_EventTypeAchievement_Chance;
extern int g_EventTypeUsedObject_Chance;

// Chance variables for guild events
extern int g_GuildEventTypeEpicGear_Chance;
extern int g_GuildEventTypeRareGear_Chance;
extern int g_GuildEventTypeGuildJoin_Chance;
extern int g_GuildEventTypeGuildLogin_Chance;
extern int g_GuildEventTypeGuildLeave_Chance;
extern int g_GuildEventTypeGuildPromotion_Chance;
extern int g_GuildEventTypeGuildDemotion_Chance;
extern int g_GuildEventTypeGuildAchievement_Chance;
extern int g_GuildEventTypeLevelUp_Chance;
extern int g_GuildEventTypeDungeonComplete_Chance;

// --------------------------------------------
// Bot-Player Sentiment Tracking System
// --------------------------------------------
extern bool        g_EnableSentimentTracking;
extern float       g_SentimentDefaultValue;              // Default sentiment value (0.5 = neutral)
extern float       g_SentimentAdjustmentStrength;        // How much to adjust sentiment per message (0.1)
extern uint32_t    g_SentimentSaveInterval;              // How often to save sentiment to DB (minutes)
extern std::string g_SentimentAnalysisPrompt;            // Prompt template for sentiment analysis
extern std::string g_SentimentPromptTemplate;            // Template for including sentiment in bot prompts

// In-memory sentiment storage and mutex
extern std::unordered_map<uint64_t, std::unordered_map<uint64_t, float>> g_BotPlayerSentiments;
extern std::mutex g_SentimentMutex;
extern time_t g_LastSentimentSaveTime;

// --------------------------------------------
// RAG (Retrieval-Augmented Generation) System
// --------------------------------------------
extern bool        g_EnableRAG;                          // Enable/disable RAG feature
extern std::string g_RAGDataPath;                        // Path to RAG data files
extern uint32_t    g_RAGMaxRetrievedItems;               // Max items to retrieve
extern float       g_RAGSimilarityThreshold;             // Similarity threshold for retrieval
extern std::string g_RAGPromptTemplate;                  // Template for RAG info in prompts

class OllamaRAGSystem;
extern OllamaRAGSystem* g_RAGSystem;                     // Global RAG system instance

// --------------------------------------------
// Event Chatter: Event Type Strings
// These control the event type string sent to eventChatter for world event prompts.
// Values are loaded from conf (see mod_ollama_chat.conf.dist)
// --------------------------------------------
extern std::string g_EventTypeDefeated;           // "defeated"
extern std::string g_EventTypeDefeatedPlayer;     // "defeated player"
extern std::string g_EventTypePetDefeated;        // "pet defeated"
extern std::string g_EventTypeGotItem;            // "got item"
extern std::string g_EventTypeDied;               // "died"
extern std::string g_EventTypeCompletedQuest;     // "completed quest"
extern std::string g_EventTypeLearnedSpell;       // "learned spell"
extern std::string g_EventTypeRequestedDuel;      // "requested to duel"
extern std::string g_EventTypeStartedDueling;     // "started dueling"
extern std::string g_EventTypeWonDuel;            // "won duel against"
extern std::string g_EventTypeLeveledUp;          // "leveled up"
extern std::string g_EventTypeAchievement;        // "earned achievement"
extern std::string g_EventTypeUsedObject;         // "used object"

// Event Cooldown
extern uint32_t g_EventCooldownTime;

// --------------------------------------------
// Channel Disable Settings
// --------------------------------------------
extern bool g_DisableForCustomChannels;
extern bool g_DisableForSayYell;
extern bool g_DisableForGuild;
extern bool g_DisableForParty;

// --------------------------------------------
// Typing Simulation Settings
// --------------------------------------------
extern bool g_EnableTypingSimulation;
extern uint32_t g_TypingSimulationBaseDelay;      // Base delay in milliseconds
extern uint32_t g_TypingSimulationDelayPerChar;   // Delay per character in milliseconds

// --------------------------------------------
// Gateway Agent Bot Configuration
// --------------------------------------------
extern bool        g_GatewayEnable;
extern std::string g_GatewayUrl;
extern std::string g_GatewayBearerToken;
extern std::string g_GatewayTriggerKeyword;
extern std::string g_GatewaySystemPrompt;
extern std::string g_GatewayModel;
extern std::string g_GatewayScopes;
extern bool        g_GatewayFallbackToOllama;
extern uint32_t    g_GatewayMaxHistory;
extern std::unordered_set<uint32_t> g_GatewayWhitelistAccountIds;
extern std::string g_GatewayType; // "openclaw" | "synthiq" (lowercase); per-bot overrides win

// PR1: Hardening + warm sessions
extern uint32_t    g_GatewayTimeoutSeconds;            // HTTP timeout for gateway requests
extern bool        g_GatewayFallbackOnError;           // Deprecated no-op; legacy local fallback removed
extern uint32_t    g_GatewayMaxConcurrentRequests;     // Concurrency cap (0 = unlimited)
extern uint32_t    g_GatewayMinSecondsBetweenRequests; // Per-(bot,player) cooldown
extern std::string g_GatewayUserPrefix;                // Prefix for the OpenAI 'user' field
extern uint32_t    g_GatewayMaxWhisperChunkChars;      // Whisper chunk size for long responses
extern bool        g_GatewayUseSessionPersistence;     // Send x-synthiq-session-key header
extern std::string g_GatewaySessionKeyTemplate;        // Template: supports {botGuid} {playerGuid}
extern bool        g_GatewaySkipHistoryWhenSession;    // Skip local history when sessions are on

// PR2: Streaming responses
extern bool        g_GatewayUseStreaming;              // Use SSE streaming responses
extern bool        g_GatewayStreamWhisperOnSentence;   // Emit whispers at sentence boundaries

// PR3: Per-agent presets, personality merge, language detection
extern std::string g_GatewayAgentByPersonality;        // raw config value, e.g. "hostile=agent:roast|friendly=agent:helpful"
extern std::unordered_map<std::string, std::string> g_GatewayAgentByPersonalityMap; // parsed
extern bool        g_GatewayMergePersonalityPrompt;    // Merge personality prompt into system prompt
extern bool        g_GatewayDetectLanguage;            // Inject language hint based on whisper script
extern std::string g_GatewayLanguageHintTemplate;      // Template, supports {lang}

// PR4: Channel expansion, mention mode
extern std::string g_GatewayAllowedChannels;           // raw config: "whisper,party,guild,..."
extern std::unordered_set<int> g_GatewayAllowedChannelsSet; // parsed (ChatChannelSourceLocal values)
extern std::string g_GatewayPublicChannelMode;         // "keyword" | "mention" | "both" — for non-private channels
extern bool        g_GatewayPrivateChannelMentionRequired; // For party/raid/guild/officer (NOT whisper):
                                                           // require the bot's own name in the message.
                                                           // Without this, an empty TriggerKeyword would
                                                           // make the bot react to every party-chat line.
extern std::string g_GatewayMentionExemptBots;             // raw csv config, e.g. "Claude,Clawd"
extern std::unordered_set<std::string> g_GatewayMentionExemptBotsSet; // parsed (lowercase bot names);
                                                           // these bots bypass the PrivateChannelMentionRequired
                                                           // check in non-whisper private channels.

// Auto-claim: when a whitelisted real player logs in, gateway bots' master is
// reassigned to them; on that player's logout, bots are released back to
// rndbot status so the fleet stays online for Overlord's SystemMasterGuid
// fallback to keep driving admin commands.
extern bool        g_GatewayAutoClaimOnLogin;              // Hard switch
extern std::string g_GatewayAutoClaimAccountIds;           // raw csv of account IDs
extern std::unordered_set<uint32_t> g_GatewayAutoClaimAccountIdsSet; // parsed

// PR6: Tool use bridge
extern bool        g_GatewayEnableToolUse;             // Send tools[] in request and dispatch tool_calls
extern uint32_t    g_GatewayMaxToolIterations;         // Recursion guard for the tool-call loop
extern std::string g_GatewayAllowedTools;              // raw csv config, e.g. "get_zone_info,whisper_player"
extern std::vector<std::string> g_GatewayAllowedToolsList; // parsed
extern std::string g_GatewayToolFacadeGroups;          // csv of capability groups collapsed into one {action,params} tool
extern std::vector<std::string> g_GatewayToolFacadeGroupList; // parsed

// PR7: Audit + action markers
extern bool        g_GatewayEnableAudit;               // Persist every gateway call to mod_ollama_chat_gateway_audit
extern uint32_t    g_GatewayAuditRetentionDays;        // Prune rows older than this (0 = never prune)
extern bool        g_GatewayEnableActionMarkers;       // Parse [emote:bow] etc. in responses

// PR8a: MCP server (in-game tools exposed via Model Context Protocol)
extern bool        g_McpEnable;                        // Run the embedded MCP HTTP server
extern std::string g_McpBindAddress;                   // Bind address (default 0.0.0.0)
extern uint32_t    g_McpPort;                          // Bind port (default 18790)
extern std::string g_McpBearerToken;                   // Required when Mcp.Enable=1
extern bool        g_McpInjectContextHint;             // Prepend bot/player identity hint to gateway system prompt
extern bool        g_McpAllowActionTools;              // Hard switch for write-action tools (PR8c)
extern uint32_t    g_McpActionRateLimitPerBotPerMinute;// Independent rate limit for action tools
extern std::string g_McpAllowedToolsExtra;             // csv allowlist; empty = all tools
extern bool        g_McpAllowGatewayInjection;         // Hard switch for talk_to_leader (voice/external chat injection into the gateway brain)

// ops-api proxy: 8 read-only MCP tools that GET /v1/* on the Go sidecar
// (`wow-ops-api`) and pass the JSON through. Lets the agent self-diagnose
// (logs, files, audit) without ssh. See `docs/ops-api.md`.
extern bool        g_OpsEnable;                        // Master switch for ops_* MCP tools
extern std::string g_OpsUrl;                           // Base URL, e.g. http://host.docker.internal:18791
extern std::string g_OpsBearerToken;                   // Must equal OPS_BEARER_TOKEN on the sidecar
extern int         g_OpsTimeoutSeconds;                // Per-call HTTP timeout
// Tier 9: Leader Bot — one designated agent commands other bots in real time
extern bool        g_McpLeaderEnable;                  // Master switch for leader_* tools
extern std::string g_McpLeaderBotGUIDs;                // raw csv config
extern std::unordered_set<uint64_t> g_McpLeaderBotGUIDSet; // parsed bots authorized as leaders
extern std::string g_McpLeaderScope;                   // "group" | "nearby" | "owner" | "all"
extern float       g_McpLeaderNearbyRadius;            // yards (used when Scope=nearby)
extern std::string g_McpLeaderDeniedCommands;          // raw csv prefix denylist
extern std::vector<std::string> g_McpLeaderDeniedCommandsList; // parsed
extern uint32_t    g_McpLeaderRateLimitPerMinute;      // per-leader rate limit (separate from action rate)
extern bool        g_McpLeaderInjectPromptHint;        // Append leader hint to gateway system prompt
extern std::string g_McpLeaderSystemPromptFile;        // external leader-mode playbook prompt file
extern std::string g_McpLeaderSystemPromptText;        // loaded leader-mode playbook prompt text

// Tier 10: Autonomous tick — leader periodically reassesses situation and may issue orders
extern bool        g_McpLeaderAutoTickEnable;          // Run a background tick thread per leader
extern uint32_t    g_McpLeaderAutoTickIntervalSeconds; // Tick period
extern uint32_t    g_McpLeaderAutoTickMaxPerHour;      // Hourly cap on autonomous gateway calls per leader

// Tier 9.1: GM admin commands via .playerbots bot ... (separate gate from regular leader_command)
extern bool        g_McpLeaderAllowAdminCommands;       // Hard switch
extern std::string g_McpLeaderAllowedAdminCommands;     // raw csv allowlist (e.g. "add,remove,addclass,addaccount,list")
extern std::vector<std::string> g_McpLeaderAllowedAdminCommandsList; // parsed

// GM console commands via MCP (admin_gm_command). First-token allowlist; the
// warcraft model sees every MCP tool, so destructive verbs stay out by default.
extern std::string g_McpGmAllowedCommands;                  // raw csv
extern std::vector<std::string> g_McpGmAllowedCommandsList; // parsed, lower-case

// Phase 1: Overlord — headless master auto-login. Required for "server-up-once-a-day"
// workflows where no human keeps a session alive and the agent still needs to spawn
// bots via `.playerbots bot ...` through a human-master session.
extern bool        g_OverlordEnable;                    // Hard switch
extern uint32_t    g_OverlordCharacterGuid;             // character guid (low part, uint32) to log in as Overlord
extern uint32_t    g_OverlordStartupDelaySeconds;       // Delay post-OnStartup before triggering login (world warm-up)
extern std::string g_OverlordAutoSpawnBots;             // raw csv of bot guids to spawn after Overlord login
extern std::vector<uint32_t> g_OverlordAutoSpawnBotList; // parsed

// Phase 1.2: Fallback master for leader_admin_command when the leader bot has no live
// human master (e.g. the leader is a rndbot spawned by Overlord's AutoSpawnBots list).
// Points at a master-registered player guid — typically the Overlord character.
extern uint32_t    g_McpLeaderSystemMasterGuid;         // 0 = no fallback (behavior before this feature)

// --------------------------------------------
// Fleet bootstrap — robot-first party/master reconciliation (mod-ollama-chat_fleet.cpp).
// Periodically self-masters the leader bot, forms its party, and pulls the member
// bots in with the leader as their playerbot master. See fleet.h for rationale.
// --------------------------------------------
extern bool        g_FleetEnsureParty;                  // master switch (default 0 — inert until enabled)
extern uint32_t    g_FleetLeaderGuid;                   // party leader / master bot guid (e.g. 20007)
extern std::string g_FleetMemberGuids;                  // raw csv of member bot guids (e.g. "20008,20009")
extern std::vector<uint32_t> g_FleetMemberGuidList;     // parsed
extern uint32_t    g_FleetEnsureIntervalSec;            // reconcile interval (default 20, floor 5)

// Bot promotion (promotion.h) — any playerbot the operator addresses gets the strategic lane
extern bool        g_PromoteEnable;                     // master switch (default 0)
extern uint32_t    g_PromoteChatTtlSec;                 // chat window after the last addressed line (default 600)
extern uint32_t    g_PromoteMaxChatBots;                // concurrent chat promotions (default 5; party guests not counted)
extern bool        g_PromotePartyEnable;                // bots in a whitelisted human's group are promoted (default 1)
extern uint32_t    g_PromoteTemplateBotGuid;            // whose gateway override promoted bots copy (0 = Fleet.LeaderGuid)
extern std::string g_PromoteChannels;                   // chat sources that promote / reach promoted bots

// --------------------------------------------
// MCP config-edit tools — lets the agent edit AzerothCore .conf files in place
// via the config_list_files / config_get_file / config_get_value / config_set_value
// / config_reload MCP tool family. Every write is backup+atomic-rename and audited.
// --------------------------------------------
extern bool        g_McpConfigEditEnable;               // Hard switch for the whole config_* tool family
extern std::string g_McpConfigEditConfigRoot;           // Canonical root for allowed paths (e.g. "/azerothcore/env/dist/etc")
extern std::string g_McpConfigEditAllowedFiles;         // raw csv of basenames relative to ConfigRoot
extern std::unordered_set<std::string> g_McpConfigEditAllowedFilesSet; // parsed
extern std::string g_McpConfigEditAllowedKeys;          // raw csv of "basename:KEY_OR_GLOB" pairs
extern std::unordered_map<std::string, std::vector<std::string>> g_McpConfigEditAllowedKeysMap; // file → list<glob>
extern std::string g_McpConfigEditDeniedKeys;           // raw csv of key globs that are NEVER readable or writable
extern std::vector<std::string> g_McpConfigEditDeniedKeysList; // parsed
extern std::string g_McpConfigEditBackupDir;            // Where to drop <basename>.<unix-ts>.bak before writes
extern uint32_t    g_McpConfigEditMaxFileSizeBytes;     // Read size cap (1 MiB default)
extern uint32_t    g_McpConfigEditReloadAdminSessionGuid; // GUID of session to dispatch .reload config / .playerbots bot reload (Overlord by default)

// --------------------------------------------
// Tactical (companion-mode) agent layer
//
// Runs a low-frequency heartbeat that wakes a local-Ollama decision loop for
// each tactical-enabled bot while a whitelisted human is online and nearby.
// Free local inference drives social + strategic actions; the paid gateway is
// invoked ONLY on explicit escalation from tactical or on human chat (handled
// by the existing gateway path). No paid-side timer.
// --------------------------------------------
extern bool        g_TacticalEnable;
extern std::string g_TacticalBotGUIDs;                  // raw csv
extern std::unordered_set<uint64_t> g_TacticalBotGUIDSet; // parsed
extern std::string g_TacticalUrl;
extern std::string g_TacticalModel;
struct TacticalBotConfig
{
    std::string url;
    std::string model;
    uint32_t    numCtx = 0;
    uint32_t    timeoutMs = 0;
    uint32_t    maxConcurrentQueries = 0;
    // Per-bot jev canary. -1 = unset (follow OllamaChat.Jev.Tactical.Enable),
    // 0 = this bot stays on the LLM tier, 1 = this bot uses jev even if the
    // global site flag is off. OllamaChat.Jev.Enable=0 wins over everything.
    int8_t      jev = -1;
};
extern std::string g_TacticalBotOverridesJson;           // JSON array of per-bot Ollama overrides
extern std::string g_TacticalBotOverridesJsonFile;       // path to JSON array of per-bot Ollama overrides
extern std::unordered_map<uint64_t, TacticalBotConfig> g_TacticalBotConfigs; // GUID -> resolved config
extern bool        g_TacticalHumanPresenceRequired;     // gate on a whitelisted human being online
extern uint32_t    g_TacticalHeartbeatMs;               // fallback tick period (events drive most wakes)
extern uint32_t    g_TacticalBotCooldownMs;             // min gap between ticks per bot
extern uint32_t    g_TacticalSnapshotCacheMs;           // cached snapshot TTL
extern uint32_t    g_TacticalMaxConcurrentQueries;      // default per-endpoint concurrency cap (Ollama side)
extern uint32_t    g_TacticalMaxPerBotPerHour;          // hour cap on free Ollama calls per bot
extern uint32_t    g_TacticalRequestTimeoutMs;          // HTTP timeout for local Ollama
extern uint32_t    g_TacticalNumCtx;                    // Ollama context window override (0 = model default)
extern std::string g_TacticalSystemPromptFile;          // external tactical playbook prompt file
extern std::string g_TacticalSystemPromptText;          // loaded tactical playbook prompt text
extern uint32_t    g_TacticalMinWhisperGapSec;          // min seconds between tactical whisper_player calls per bot
extern bool        g_TacticalAmbientEnable;             // allow tactical to use visible personality actions
extern uint32_t    g_TacticalAmbientMinSpeechGapSec;    // min seconds between tactical bot_say/bot_yell per bot
extern uint32_t    g_TacticalAmbientMinEmoteGapSec;     // min seconds between tactical bot_emote per bot
extern uint32_t    g_TacticalAmbientMaxVisibleActionsPerMinute; // per-bot social action cap
extern uint32_t    g_TacticalAmbientEventReactionChance; // chance to expose a recent event to the model
extern bool        g_TacticalAutoEnrollNearbyBots;      // runtime-enroll nearby playerbots in addition to BotGUIDs
extern uint32_t    g_TacticalNearbyBotMax;              // max runtime-enrolled nearby bots
extern float       g_TacticalNearbyBotRadius;           // yards from whitelisted/real human for auto-enroll
extern bool        g_GatewayOllamaClassifierEnable;     // cheap-path: ask local Ollama for allowlisted tool calls
extern std::string g_GatewayOllamaClassifierUrl;        // classifier Ollama endpoint (empty = inherit Tactical.Url)
extern std::string g_GatewayOllamaClassifierModel;      // classifier Ollama model (empty = inherit Tactical.Model)
extern uint32_t    g_GatewayOllamaClassifierTimeoutMs;  // classifier HTTP timeout in ms (0 = legacy 5s fallback)
extern std::string g_OpenAiCompatApiKey;                // bearer sent by LlmWire to any fast-path URL that is /chat/completions (DeepSeek etc.)
extern uint32_t    g_GatewayOllamaClassifierNumCtx;     // classifier context window override (0 = model/default)
extern std::string g_GatewayOllamaClassifierSystemPromptFile; // external classifier playbook prompt file
extern std::string g_GatewayOllamaClassifierSystemPromptText; // loaded classifier playbook prompt text
extern float       g_GatewayOllamaClassifierMinConfidence; // required classifier confidence to short-circuit
extern std::atomic<uint32_t> g_GatewayClassifierMaxWords; // both classifier tiers skip lines longer than this (0 = no gate); atomic: read on chat workers during reload
extern uint32_t    g_TacticalPromptMaxBytes;            // prompt size budget
extern bool        g_TacticalEnableAudit;
extern uint32_t    g_TacticalAuditRetentionDays;
extern bool        g_TacticalAllowEscalation;           // tactical may set escalate flag
extern uint32_t    g_TacticalDirectiveMaxTtlSec;        // clamp on strategic-written directive TTLs
extern uint32_t    g_TacticalIdlePauseSec;              // no human activity for this long => pause this bot
extern bool        g_TacticalAllowCombatOverride;       // if 0, dispatcher blocks combat-tactics actions during combat
extern std::string g_TacticalRequireConfirmationFor;    // raw csv of action names
extern std::unordered_set<std::string> g_TacticalRequireConfirmationSet; // parsed
extern uint32_t    g_TacticalConfirmationTimeoutSec;    // HITL wait before abort

// Strategic escalation worker — purely reactive, never on a timer.
extern bool        g_StrategicSuppressLeaderTickForTacticalBots; // skip leader-tick for tactical bots
extern uint32_t    g_StrategicMaxEscalationsPerBotPerHour;       // HARD cap on paid calls/bot/hr
extern uint32_t    g_StrategicQueueMaxSize;                      // drop-oldest bound
extern uint32_t    g_StrategicWorkerThreads;                     // serial worker count (1 is fine)
extern uint32_t    g_StrategicEscalationCooldownSec;             // min gap between paid calls per bot
TacticalBotConfig ResolveTacticalBotConfig(uint64_t botGuid);

// --------------------------------------------
// Proactive Leader Bot (feature 002)
// Master gate is g_ProactiveEnable (default OFF — opt-in). Full design lives
// in specs/002-proactive-leader-bot/. Keys mirror tactical-style naming.
// --------------------------------------------
extern bool        g_ProactiveEnable;
extern uint32_t    g_ProactiveProposalCooldownSec;
extern uint32_t    g_ProactiveFirstProposalDelaySec;
extern uint32_t    g_ProactiveIdleThresholdSec;
extern uint32_t    g_ProactiveIdleNudgeCooldownSec;
extern uint32_t    g_ProactiveIdleNudgeMaxPerSession;
extern uint32_t    g_ProactiveProposalMaxPerSession;
extern float       g_ProactiveProximityYards;
extern uint32_t    g_ProactiveProximityWarnCooldownSec;
extern uint32_t    g_ProactiveActivityCompleteTimeoutSec;
extern uint32_t    g_ProactiveActivityCompleteNudgeWaitSec;
extern uint32_t    g_ProactiveProposalExpirySec;
extern uint64_t    g_ProactivePreferredVoiceGuid;
extern std::string g_ProactiveProposerPromptFile;
extern std::string g_ProactiveProposerPromptText;   // loaded from file at startup/reload
extern uint32_t    g_ProactiveMaxQuestRankSearch;
extern std::string g_ProactiveDungeonCandidates;    // raw csv whitelist of dungeon mapIds
extern bool        g_ProactiveEnableAudit;          // default true — independent of gateway-wide EnableAudit
extern bool        g_ProactiveLevelUpCongratsEnable;        // default true — zero-cost deterministic-only, no LLM call
extern uint32_t    g_ProactiveLevelUpCongratsCooldownSec;   // default 60 — guards a rapid multi-level burst
extern bool        g_ProactiveNoVoiceAuditEnable;           // default true — diagnostic-only, no LLM call, no chat line
extern uint32_t    g_ProactiveNoVoiceAuditCooldownSec;      // default 300 — per-player throttle for the novoice row

// Proactive — LLM tiers. Phase A wires the intent classifier (local Ollama)
// in front of the deterministic substring matcher inside ClassifyPlayerMessage.
// The classifier reuses the existing Gateway.OllamaClassifier.* endpoint /
// model (one local Ollama serves multiple prompts).
extern bool        g_ProactiveIntentEnable;          // default true — clear UX win
extern std::string g_ProactiveIntentPromptFile;      // path on disk (relative to AC working dir)
extern std::string g_ProactiveIntentPromptText;      // loaded at startup/reload
extern float       g_ProactiveIntentMinConfidence;   // min confidence to accept the label
extern uint32_t    g_ProactiveIntentTimeoutMs;       // HTTP timeout (0 = legacy 3000ms fallback)

// Phase B — gateway planner. Agent veto + phrasing on top of the C++-picked
// candidate. Default OFF (paid Claude calls; opt-in). Reuses
// QueryGatewayAPIRaw so all the gateway slot / cooldown / per-bot routing
// primitives apply automatically.
extern bool        g_ProactivePlannerEnable;         // default false — paid, opt-in
extern std::string g_ProactivePlannerPromptFile;
extern std::string g_ProactivePlannerPromptText;     // loaded at startup/reload

// Phase C — local-Ollama tick gate. Cheap upstream filter that vetoes
// obvious no-go ticks BEFORE the paid planner fires. Throttled by
// IntervalSec so even a 10s heartbeat queries Ollama at most twice/min
// per (bot, player). Default OFF — bot-spam risk requires deliberate
// opt-in.
extern bool        g_ProactiveTickGateEnable;        // default false — opt-in
extern std::string g_ProactiveTickGatePromptFile;
extern std::string g_ProactiveTickGatePromptText;
extern uint32_t    g_ProactiveTickGateIntervalSec;   // min seconds between gate calls per (bot,player)
extern uint32_t    g_ProactiveTickGateTimeoutMs;     // HTTP timeout (0 = legacy 2000ms fallback)
extern float       g_ProactiveTickGateMinConfidence; // min confidence to accept the verdict

// --------------------------------------------
// jev decision tier (OllamaChat.Jev.*) — TypeSafe System One model in front
// of the decision-shaped fast paths. See src/mod-ollama-chat_jev.h.
// --------------------------------------------
extern bool        g_JevEnable;                     // master switch (default 0)
extern std::string g_JevUrl;                        // Decisions endpoint (OpenRouter alpha or TypeSafe native)
extern std::string g_JevModel;                      // typesafe/jev-1.13 (OpenRouter) | jev-1.13.0 (native)
extern std::string g_JevApiKey;                     // bearer; live value only in the host bind-mount conf
extern std::string g_JevProxy;                      // optional http proxy "host:port" (gluetun HTTPPROXY)
extern std::string g_JevProxyUser;
extern std::string g_JevProxyPassword;
extern uint32_t    g_JevTimeoutMs;                  // per-call cap before the chain's remaining budget clamps it
extern uint32_t    g_JevMaxConcurrent;              // client pool size (leased exclusively)
extern uint32_t    g_JevBreakerCooldownSec;         // jev-side breaker open time after 3 failures
extern std::string g_JevQuestionsFile;              // prompts/jev_questions.json
extern bool        g_JevClassifierEnable;           // site A — gateway command classifier
extern float       g_JevClassifierMinConfidence;
extern bool        g_JevIntentEnable;               // site C — proactive approve/reject/done
extern float       g_JevIntentMinConfidence;
extern bool        g_JevTickGateEnable;             // site D — proactive propose/wait
extern float       g_JevTickGateMinConfidence;
extern bool        g_JevPlannerEnable;              // site E — planner veto (decision half)
extern float       g_JevPlannerMinConfidence;
extern bool        g_JevTacticalEnable;               // site B — per-tick action funnel (PR 2)
extern float       g_JevTacticalMinConfidence;        // action Choice confidence to dispatch
extern float       g_JevTacticalEscalateMin;          // escalate Noul P(yes) to enqueue an escalation
extern bool        g_JevPlaybookEnable;               // leader playbook relevance filter (PR 3)
extern float       g_JevPlaybookMinRelevance;         // keep rows with P(relevant) >= this
extern uint32_t    g_JevPlaybookMinRows;              // always keep at least this many (top-N)
extern uint32_t    g_JevPlaybookMaxRows;              // never send more than this many rows
extern uint32_t    g_JevPlaybookTimeoutMs;            // this call precedes a 5-20 s gateway call; measured 2.0-2.8 s for 78 rows

// --------------------------------------------
// Loader Functions
// --------------------------------------------
void LoadOllamaChatConfig();
void LoadBotPersonalityList();
void LoadBotConversationHistoryFromDB();
void LoadPersonalityTemplatesFromDB();
// Full runtime reload — re-reads every .conf through sConfigMgr, repopulates
// globals, refreshes DB caches, and restarts the MCP server. Shared by the
// `.ollama reload` command and the `config_reload` MCP tool.
void ReloadOllamaChatRuntime();

// --------------------------------------------
// Declaration of the configuration WorldScript.
// --------------------------------------------
class OllamaChatConfigWorldScript : public WorldScript
{
public:
    OllamaChatConfigWorldScript();
    void OnStartup() override;
    void OnUpdate(uint32 diff) override;   // drives OllamaChat::Fleet::Tick (world thread)
    void OnShutdown() override;
};

#endif // MOD_OLLAMA_CHAT_CONFIG_H
