#ifndef MOD_OLLAMA_CHAT_TOOLS_H
#define MOD_OLLAMA_CHAT_TOOLS_H

#include <string>
#include <vector>
#include <unordered_map>
#include <functional>
#include <cstdint>
#include <nlohmann/json.hpp>

// Gateway tool-use bridge.
//
// We send OpenAI-style tool definitions to the upstream gateway. When the model decides
// to call one, the response carries `choices[0].message.tool_calls`. The gateway loop
// dispatches each call to a C++ handler here, appends the result as a `role:tool`
// message, and re-queries the gateway. This repeats up to `g_GatewayMaxToolIterations`
// times before giving up.

struct GatewayTool
{
    std::string name;
    std::string description;
    nlohmann::json parametersSchema; // JSON Schema for arguments (OpenAI format)
    // Handler signature: (botGuid, playerGuid, args) → result JSON.
    // Result is stringified and returned to the model as the tool message content.
    std::function<nlohmann::json(uint64_t botGuid, uint64_t playerGuid, const nlohmann::json& args)> handler;
    // MCP 2025-11-25 tool annotations (readOnlyHint, destructiveHint, idempotentHint, openWorldHint).
    // Surfaced on `tools/list` so clients can auto-approve safe reads and gate destructive actions.
    // Not forwarded to the OpenAI tool-use endpoint (not part of that schema).
    nlohmann::json annotations;
};

// Returns the singleton tool registry (initialized lazily).
const std::unordered_map<std::string, GatewayTool>& GetGatewayToolRegistry();

// Returns tool names sorted alphabetically. Used by `tools/list` and the OpenAI tools array
// to guarantee deterministic ordering across process restarts — the MCP spec says tools
// SHOULD be returned deterministically, and stable order lets upstream LLMs hit their
// prompt cache on the tools block.
std::vector<std::string> GetGatewayToolNamesSorted();

// Tool scope — which callers a tool is reachable by. Derived from the tool name
// (see GetGatewayToolScope) rather than stored per-registration, so a newly
// registered tool picks up its scope automatically.
//
// This exists because the ops_* and leader_* families are gated at *handler
// execution* time (g_OpsEnable / IsLeaderBot) but were still advertised to
// every bot's LLM. An ordinary bot was paying ~2.1k tokens per gateway call
// for 13 tools whose handlers can only ever return an error for it — and could
// burn a whole tool-call round trip discovering that.
enum class GatewayToolScope
{
    Gameplay, // available to every bot
    Ops,      // ops-api proxies — handler returns an error unless g_OpsEnable
    Leader,   // leader-bot control — handler errors / resolves an empty target set for non-leaders
};

GatewayToolScope GetGatewayToolScope(const std::string& name);

// Capability group a tool belongs to — "combat", "guild", "economy", … Derived
// from the tool name so no registration block carries a group and a newly added
// tool is grouped automatically.
//
// Groups exist so an allowlist can be written as `@combat,@movement` instead of
// naming 70 tools. The full registry is ~58k tokens on every gateway call; a
// combat-only bot needs ~16k of it.
//
// Every tool lands in exactly one group; unrecognised names fall through to
// "core" (the always-on situational-awareness set).
std::string GetGatewayToolGroup(const std::string& name);

// Every group name, sorted. Used for config validation and diagnostics.
std::vector<std::string> GetGatewayToolGroups();

// Expand `@group` selectors in a tool list into concrete tool names. Plain tool
// names pass through untouched, so a list may mix both:
//   {"@combat", "@movement", "bot_say"}
//
// An unknown `@group` is logged and skipped. If the input is non-empty but
// nothing resolves (e.g. every selector was a typo) this returns an empty
// vector, which the callers treat as "all tools" — failing open rather than
// leaving a bot with no tools at all. The error log is the signal.
std::vector<std::string> ExpandToolSelectors(const std::vector<std::string>& selectors);

// True when `group` is listed in OllamaChat.Gateway.ToolFacadeGroups, i.e. its
// members are advertised as ONE {action, params} tool named after the group
// instead of N individually-schema'd tools.
//
// Collapsing a group costs ~95% of its advertised tokens but drops the
// per-action parameter schemas, so the model may need a `describe` call to
// learn an action's arguments — and that consumes one of the
// g_GatewayMaxToolIterations (3 in prod) the bot gets per turn. Collapse the
// cold path (economy, guild, raid…), leave the hot path flat (combat,
// movement, core) where a wasted iteration costs a reaction.
bool IsFacadeGroup(const std::string& group);

// Resolve a facade call. `facadeName` is a group name, `args` carries
// {"action": "<tool>", "params": {...}} or {"describe": "<tool>"}.
// On success sets outTool/outArgs and returns true. On failure fills outError.
bool ResolveFacadeCall(const std::string& facadeName, const nlohmann::json& args,
                       std::string& outTool, nlohmann::json& outArgs,
                       nlohmann::json& outError);

// Build the collapsed descriptor for `group` over `members`, in the OpenAI
// function shape: {"type":"function","function":{name,description,parameters}}.
// The MCP surface reshapes function.parameters into `inputSchema`; both surfaces
// share this one definition so the action signatures can never drift apart.
nlohmann::json BuildGroupFacade(const std::string& group,
                                const std::vector<std::string>& members);

// True when `name` is worth advertising to this bot: its scope predicate holds.
// Advertisement-time mirror of the handler-time gate.
bool IsToolVisibleToBot(const std::string& name, uint64_t botGuid);

// Build the OpenAI-format tools array for the request body.
// `allowedTools` filters the registry; pass empty for "all enabled" (subject to global allowlist).
// `botGuid` additionally drops out-of-scope tools (ops/leader) for this bot; pass 0 to skip
// scope filtering entirely (used by callers that want the unfiltered array).
nlohmann::json BuildOpenAiToolsArray(const std::vector<std::string>& allowedTools, uint64_t botGuid = 0);

// Dispatch a tool call by name. Returns the result JSON (or an object with "error":"...").
nlohmann::json DispatchGatewayTool(uint64_t botGuid, uint64_t playerGuid,
                                   const std::string& name, const nlohmann::json& args);

// Combine the global g_GatewayAllowedTools with the per-bot override (cfg.allowedTools).
// Returns the effective allowlist for this request.
std::vector<std::string> ResolveAllowedTools(uint64_t botGuid);

// PR8c — action-tool rate limiter. Returns true if the (bot, tool) pair is under
// g_McpActionRateLimitPerBotPerMinute calls in the trailing 60s window and bumps
// the counter; false if the call should be dropped.
bool TryClaimActionRateSlot(uint64_t botGuid, const std::string& toolName);

// Tier 9 — leader bot helpers (also used by gateway system-prompt hint and the
// autonomous tick thread).
bool IsLeaderBot(uint64_t botGuid);
bool TryClaimLeaderRateSlot(uint64_t leaderGuid);

class Player;
class WorldObject;

// The creature or game object within `range` yards of `bot` that ENDS one of
// the bot's completed quests (nearest wins), or nullptr. A nearby quest giver is not
// enough: the tactical loop once turned "quests_turnin_ready=1 + some quest
// giver in sight" into bot_turn_in_quest every tick at the wrong NPC
// (2026-09-25). Reads world state — call it where the caller already may.
WorldObject* FindQuestEnderNear(Player* bot, float range);

class Creature;

// The nearest class trainer the bot can use RIGHT NOW (GetNPCIfCanInteractWith
// + valid for the bot's class) within `range`, or nullptr; `affordable` gets
// how many of its spells the bot could learn and pay for. World thread only.
Creature* FindUsableClassTrainer(Player* bot, float range, uint32_t* affordable = nullptr);

// How far the tactical loop looks for a quest ender. SyncQuestWithPlayer=1
// lets playerbots turn in without walking up, so this is a "same camp" radius,
// not interaction range.
constexpr float kQuestEnderSearchRange = 30.0f;

#endif // MOD_OLLAMA_CHAT_TOOLS_H
