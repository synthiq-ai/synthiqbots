#ifndef MOD_OLLAMA_CHAT_GATEWAY_H
#define MOD_OLLAMA_CHAT_GATEWAY_H

#include <string>
#include <unordered_set>
#include <unordered_map>
#include <atomic>
#include <mutex>
#include <vector>
#include <ctime>
#include <cstdint>
#include <functional>

// Per-bot gateway override (URL, token, model + optional custom headers)
struct GatewayBotConfig
{
    std::string url;
    std::string bearerToken;
    std::string model;
    // Backend type: "openclaw" (full-fat agent, ignores our tools[]) or "synthiq"
    // (Claude Agent SDK, honors tools[]). Empty = use g_GatewayType global default.
    std::string gatewayType;
    // Optional headers merged into the request (e.g., x-synthiq-agent, x-openclaw-scopes override).
    std::unordered_map<std::string, std::string> headers;
    // Optional list of tool names — per-bot allowlist for the tool-use bridge.
    std::vector<std::string> allowedTools;
};

// Aggregate runtime stats for the gateway feature.
// Updated by QueryGatewayAPI / handler hot path; read by `.ollama gateway status`.
struct GatewayStats
{
    std::atomic<uint64_t> totalRequests{0};
    std::atomic<uint64_t> totalErrors{0};
    std::atomic<uint64_t> totalRateLimited{0};      // dropped due to cooldown or concurrency cap
    std::atomic<uint64_t> totalFallbacks{0};        // legacy counter; local fallback removed
    std::atomic<uint64_t> totalPromptTokens{0};
    std::atomic<uint64_t> totalCompletionTokens{0};

    std::mutex perBotMutex;
    std::unordered_map<uint64_t, uint64_t> perBotRequests;
    std::unordered_map<uint64_t, time_t>   perBotLastRequest;
};

// Gateway bot GUID set (populated from config)
extern std::unordered_set<uint64_t> g_GatewayBotGUIDSet;

// Per-bot gateway overrides (GUID → config). Bots not in this map use global defaults.
extern std::unordered_map<uint64_t, GatewayBotConfig> g_GatewayBotConfigs;

// Whitelist of WoW account IDs allowed to trigger gateway bots.
// Empty set = allow all (permissive default; a warning is logged at load time).
extern std::unordered_set<uint32_t> g_GatewayWhitelistAccountIds;

// Aggregate runtime stats; reset on module reload.
extern GatewayStats g_GatewayStats;

// Check if a bot GUID is a gateway bot (also checks g_GatewayEnable)
bool IsGatewayBot(uint64_t botGuid);

// Per-bot gateway config: the bot's own override (gateway_overrides.json), else
// the promotion template when the bot is promoted (Gateway.Promote.*). Returns a
// COPY — safe on gateway worker threads. False = use the globals.
bool FindGatewayBotConfig(uint64_t botGuid, GatewayBotConfig& out);

// Check if message contains the trigger keyword and strip it.
// Returns true if keyword found; writes the stripped message to 'stripped'.
bool ExtractGatewayMessage(const std::string& msg, const std::string& keyword, std::string& stripped);

// Query the Synthiq Gateway API (OpenAI-compatible /v1/chat/completions).
// Includes conversation history from g_BotConversationHistory unless session persistence is on.
// Returns the assistant response text, or empty string on error (also bumps totalErrors).
std::string QueryGatewayAPI(uint64_t botGuid, uint64_t playerGuid, const std::string& message);

// Streaming variant: invokes onWhisperReady once per emitted chunk
// (sentence-boundary or maxChunkChars-bounded, depending on config).
// Returns the full accumulated assistant response (for history), or empty on error.
// onWhisperReady is invoked from inside the worker thread; the caller is responsible
// for any thread-affinity considerations (re-acquiring player pointers, etc.).
std::string QueryGatewayAPIStreaming(uint64_t botGuid, uint64_t playerGuid,
                                     const std::string& message,
                                     const std::function<bool(const std::string& chunk)>& onWhisperReady);

// Tool-using variant: includes tool definitions in the request and dispatches any
// tool_calls returned by the model, looping up to g_GatewayMaxToolIterations times.
// Returns the final assistant text (or empty on error / iteration cap).
std::string QueryGatewayAPIWithTools(uint64_t botGuid, uint64_t playerGuid, const std::string& message);

// One-shot variant with a caller-supplied system prompt. Skips conversation
// history, personality merging, language-hint injection, and tool definitions —
// just sends [system, user] to the gateway and returns the assistant text.
// Used by the proactive loop for short generative chat lines (proposer prompt
// + JSON snapshot) where the per-call system prompt is the proposer template
// rather than the bot's regular personality. Returns "" on error.
std::string QueryGatewayAPIRaw(uint64_t botGuid, uint64_t playerGuid,
                               const std::string& systemPrompt,
                               const std::string& userMessage);

// Cheap-path intent classifier — runs a fast local Ollama classifier on the
// incoming human message, asking it to map the phrase to a known MCP tool call.
// On confident match: dispatches the tool in-process and returns a short
// pre-canned ack string (so the existing response-sending code can deliver it
// unchanged). On miss / ambiguous / error: returns empty string, caller falls
// through to the paid Synthiq path. Uses Gateway.OllamaClassifier.* config,
// inheriting Tactical.Url/Tactical.Model when endpoint/model are empty.
std::string TryGatewayIntentClassify(uint64_t botGuid, uint64_t playerGuid, const std::string& message);

// Append one (player message, bot reply) turn to the in-memory per-(bot,player)
// conversation history that QueryGatewayAPI* reads back on the next call. Defined
// in mod-ollama-chat_handler.cpp; declared here so the MCP injection tool
// (talk_to_leader) can preserve conversation continuity for follow-up commands.
void AppendBotConversation(uint64_t botGuid, uint64_t playerGuid, const std::string& playerMessage, const std::string& botReply);

// --- PR7: audit + action markers ---

// Async insert into mod_ollama_chat_gateway_audit. No-op if g_GatewayEnableAudit is false
// or the table is missing. Caller passes the source channel name (e.g., "whisper", "guild").
// `backend` ("jev" | "llm" | "det" | nullptr) lands in the `backend` column once
// Jev::EnsureAuditBackendColumns has confirmed it exists; NULL otherwise.
void WriteGatewayAuditRecord(uint64_t botGuid, uint64_t playerGuid, uint32_t accountId,
                             uint32_t requestChars, uint32_t responseChars,
                             uint32_t promptTokens, uint32_t completionTokens,
                             uint32_t latencyMs, const std::string& sourceChannel, bool error,
                             const char* backend = nullptr);

// Drop audit rows older than g_GatewayAuditRetentionDays.
// Called once at startup (and on reload) to keep the table bounded without a background task.
void PruneGatewayAuditRows();

struct GatewayAction
{
    std::string name;  // "emote", "follow", ...
    std::string arg;   // emote name, target name, ...
};

// Effective gateway type for this bot: per-bot override wins, else g_GatewayType global.
// Returned value is lowercase ("openclaw" | "synthiq"); unrecognized values fall back to "openclaw".
std::string ResolveGatewayType(uint64_t botGuid);

// Strip any [name:arg] markers from `text` (modified in place) and return the parsed actions.
// Markers must be at most ~20 chars total; non-matching brackets are left alone.
std::vector<GatewayAction> ExtractActionMarkers(std::string& text);

// Best-effort dispatch of an action to the bot. Currently supports only "emote:<emoteName>".
// Returns true if dispatched, false if unknown/no-op.
bool DispatchGatewayAction(uint64_t botGuid, const GatewayAction& action);

// Parse comma-separated GUID list from config into g_GatewayBotGUIDSet
void ParseGatewayBotGUIDs(const std::string& guidList);

// Parse per-bot gateway overrides from config (pipe-separated: "GUID:URL:TOKEN:MODEL|GUID:URL:TOKEN:MODEL").
// Legacy format — kept for backward compatibility.
void ParseGatewayBotOverrides(const std::string& overrideList);

// Parse per-bot gateway overrides from a JSON array string. Preferred over the legacy format —
// supports custom headers and per-bot tool allowlists, and doesn't break on URLs containing colons.
//
// Format:
//   [
//     {
//       "guid": 12345,
//       "url": "http://host:port/v1/chat/completions",
//       "token": "...",
//       "model": "agent:lore-master",
//       "headers": { "x-synthiq-agent": "lore", "x-openclaw-scopes": "operator.read" },
//       "tools": ["get_zone", "get_loot"]
//     }
//   ]
void ParseGatewayBotOverridesJson(const std::string& jsonStr);

// Check if a sender account is allowed to trigger gateway responses.
// Empty whitelist (default) permits all senders.
bool IsAccountAllowedForGateway(uint32_t accountId);

// Parse comma-separated account ID list from config into g_GatewayWhitelistAccountIds
void ParseGatewayWhitelist(const std::string& list);

// --- PR1: concurrency, cooldown, chunking ---

// Try to reserve a concurrent gateway slot. Returns true if acquired (caller must
// call ReleaseGatewaySlot when done), false if the cap is exceeded.
// When g_GatewayMaxConcurrentRequests == 0 the cap is disabled and this always returns true.
bool TryAcquireGatewaySlot();

// Release a previously-acquired gateway slot.
void ReleaseGatewaySlot();

// Check the per-(bot, player) cooldown defined by g_GatewayMinSecondsBetweenRequests.
// Returns true and updates the timestamp if the call is allowed.
// Returns false if the caller should drop the request.
bool TryClaimGatewayCooldown(uint64_t botGuid, uint64_t playerGuid);

// Reset all stats and cooldowns (used on reload to start with a clean slate).
void ResetGatewayRuntimeState();

// Render the configured session key template by substituting {botGuid} and {playerGuid}.
std::string BuildGatewaySessionKey(uint64_t botGuid, uint64_t playerGuid);

// Split a long response into whisper-friendly chunks (sentence-aware where possible).
// maxChars is a soft cap; the splitter prefers sentence/whitespace boundaries before that limit.
std::vector<std::string> SplitForWhisper(const std::string& text, size_t maxChars);

// --- PR3: per-agent presets, personality merge, language hints ---

// Parse "personality1=agent:id1|personality2=agent:id2" into g_GatewayAgentByPersonalityMap.
void ParseGatewayAgentByPersonality(const std::string& cfg);

// Lookup personality assignment for a bot GUID without needing a Player*.
// Returns "default" if not assigned.
std::string LookupBotPersonality(uint64_t botGuid);

// Pick the model name for this request: per-bot override > personality preset > global default.
std::string ResolveGatewayModel(uint64_t botGuid, const std::string& fallbackModel);

// Build the system prompt for this request, optionally merging personality prompt and a
// language hint inferred from the player's message.
std::string BuildGatewaySystemPrompt(uint64_t botGuid, const std::string& playerMessage);

// Same as BuildGatewaySystemPrompt but with the player's GUID also threaded through, so
// the optional MCP context hint can include it. Use this from any QueryGatewayAPI* caller
// that knows the player's GUID (all of them in the current handler).
std::string BuildGatewaySystemPromptEx(uint64_t botGuid, uint64_t playerGuid, const std::string& playerMessage);

// --- PR4: channel allowlist + public-channel mention mode ---

// Parse "whisper,party,guild,..." into g_GatewayAllowedChannelsSet (using ChatChannelSourceLocal).
void ParseGatewayAllowedChannels(const std::string& cfg);

// Shared parser behind it (also Gateway.Promote.Channels). Empty cfg = whisper;
// "none" = nothing. keyName only labels the unknown-name error.
void ParseGatewayChannelList(const std::string& cfg, std::unordered_set<int>& out, const char* keyName);

// Whether the gateway should consider this source channel at all.
bool IsGatewaySourceAllowed(int chatChannelSourceLocal);

// True if the source is a private channel (whisper/party/raid/guild/officer);
// false for public channels (say/yell/general). Used to pick gating mode.
bool IsPrivateChannel(int chatChannelSourceLocal);

// Returns true if the message references the bot by name (case-insensitive substring).
bool MessageMentionsBot(const std::string& message, const std::string& botName);

#endif // MOD_OLLAMA_CHAT_GATEWAY_H
