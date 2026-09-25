#ifndef MOD_OLLAMA_CHAT_PROACTIVE_H
#define MOD_OLLAMA_CHAT_PROACTIVE_H
//
// Proactive Leader Bot — feature 002.
// Inverts the leader/follower dynamic: a designated gateway leader bot opens
// the conversation by proposing a concrete quest/dungeon, waits for explicit
// player approval, executes via existing playerbot primitives, and stays in
// proximity. Idle nudges fill silence without spamming.
//
// All proactive output reuses the existing tactical heartbeat for cadence,
// the existing Ollama classifier for yes/no intent, the existing gateway path
// for natural-language phrasing, and `mod_ollama_chat_gateway_audit` for the
// lifecycle audit trail. No new SQL migration, no new MCP tool, no new
// movement planner. See `specs/002-proactive-leader-bot/` for the full design.
//
// Master gate: g_ProactiveEnable (default OFF — opt-in).
//

#include <cstdint>
#include <deque>
#include <mutex>
#include <string>
#include <unordered_map>
#include <vector>

class Player;

namespace ollamachat
{
namespace proactive
{
// ---------------------------------------------------------------------------
// Tap-in surface for existing components
// ---------------------------------------------------------------------------

// Called once per tactical heartbeat from `_tactical.cpp` TickAllBots(). No-op
// when the master gate is off. Internally enumerates whitelisted-online
// players and elects a designated voice per player before evaluating any
// state transitions. Cheap when nothing is due.
void Heartbeat();

// Called from `_handler.cpp` PlayerBotChatHandler::OnPlayerCanUseChat for
// every player chat message. Returns true if the message has been fully
// handled by the proactive path (approval/rejection/done) and the regular
// gateway chat chain MUST be skipped. Returns false otherwise so the existing
// chain continues.
bool OnPlayerChat(Player* player, std::string const& message, uint32_t channelType);

// Called from `_events.cpp` on quest completion / zone transition. Used for
// hybrid completion detection (FR-021).
void OnQuestComplete(Player* player, uint32_t questId);
void OnZoneChange(Player* player, uint32_t newZoneId, uint32_t newMapId);

// Called from the `ProactivePlayerScript::OnPlayerLevelChanged` hook (this
// file) on a genuine in-play level-up (never fires at character creation).
// Deterministic-only ambient line — deliberately bypasses `PhraseLine`'s
// generative path (see DeterministicPhrasing's LineKind::LevelUp case).
void OnPlayerLevelChanged(Player* player, uint8_t oldLevel);

// Called from `_main.cpp` PlayerScript hooks. Resets all per-session state
// for this player so cadence counters / idle-nudge counts / open proposals
// don't survive a logout. On login, anchors `sessionStartedAtMs` for the
// first-proposal-delay gate.
void OnPlayerLogin(uint64_t playerGuid);
void OnPlayerLogout(uint64_t playerGuid);

// Registers the proactive PlayerScript so the login/logout hooks above fire.
// Called once from `_main.cpp` `Addmod_ollama_chatScripts()` alongside the
// other PlayerScript / WorldScript registrations.
void RegisterScripts();

// Called from `_config.cpp` LoadOllamaChatConfig() after the new
// g_Proactive* keys have been populated. Loads the proposer prompt file.
// If the file is missing, logs an error and forces g_ProactiveEnable=false.
void OnConfigReloaded();

// ---------------------------------------------------------------------------
// Internal types (header-visible because the .cpp uses them across functions)
// ---------------------------------------------------------------------------

enum class Kind : uint8_t
{
    None = 0,
    Quest,
    Dungeon,
    FallbackZone,
};

enum class State : uint8_t
{
    Open = 0,
    Approved,
    Rejected,
    Expired,
    Cancelled,
    Executing,
    Completed,
    Aborted,
};

enum class LineKind : uint8_t
{
    Proposal = 0,
    ApprovalAck,
    Waiting,
    ProximityWarn,
    IdleNudge,
    CompleteCheck,
    CompleteAck,
    AbortAck,
    LevelUp,
};

enum class EmitTarget : uint8_t
{
    Party = 0,
    Whisper,
};

struct Candidate
{
    Kind        kind = Kind::None;
    uint32_t    questId = 0;
    std::string questTitle;
    bool        hasProgress = false;
    uint32_t    mapId = 0;
    uint32_t    continentMapId = 0;
    float       entranceX = 0.0f;
    float       entranceY = 0.0f;
    float       entranceZ = 0.0f;
    std::string dungeonName;
    uint32_t    zoneId = 0;
    std::string zoneName;
};

struct Proposal
{
    uint64_t  proposalId = 0;
    uint64_t  playerGuid = 0;
    uint64_t  botGuid    = 0;
    Candidate candidate;
    State     state          = State::Open;
    uint64_t  createdAtMs    = 0;
    uint64_t  answeredAtMs   = 0;
    uint64_t  expiryMs       = 0;
    // Snapshot of player's mapId at proposal-open time. OnZoneChange uses
    // this to detect a hearth/portal/zeppelin to a different map and cancel
    // the now-stale Open proposal before its 10-min expiry (FR-009).
    uint32_t  originMapId    = 0;
    // Wall-clock (NowMs steady_clock) when this proposal first entered a
    // TERMINAL state (rejected/expired/cancelled/completed/aborted). 0 while
    // non-terminal (open/approved). Stamped lazily by ReapTerminalState the
    // first heartbeat it observes the terminal proposal, then used to drop it
    // (and its one-to-one ActivityPlan) from the in-memory maps after
    // kTerminalRetentionMs — see data-model.md "the Proposal is terminal and is
    // dropped from the in-memory table after a short retention window (~60s)".
    // Without this, s_proposalsById / s_plansByProposalId were never reaped:
    // every proposal ever opened lingered for the whole worldserver uptime (an
    // unbounded leak that also grows the per-heartbeat O(N) lookup scans).
    uint64_t  terminalAtMs   = 0;
    std::string  lastLineText;       // for debug/log; not persisted
};

struct ActivityPlan
{
    uint64_t proposalId = 0;
    State    state                  = State::Executing;
    uint64_t startedAtMs            = 0;
    uint64_t lastProgressAtMs       = 0;
    uint64_t completeCheckSentAtMs  = 0;     // 0 until the timeout-check nudge has been emitted
    // Set to true the first time OnZoneChange observes newMapId == the
    // dungeon candidate's mapId. Drives BOTH halves of the FR-021 dungeon
    // entry/exit lifecycle: (1) when FALSE, a cross-map hearth/portal BEFORE
    // entering the instance is treated as abandonment — OnZoneChange skips
    // the zone-out completion and EvaluateTickLocked section 2a's DifferentMap
    // branch aborts the plan; (2) when TRUE and the player is currently on
    // candidate.mapId, that same section-2a DifferentMap branch SUPPRESSES the
    // abort (the bot just hasn't followed into the instance — v1 dispatches
    // `follow`, not travel-to-coord), keeping the plan Executing so the later
    // zone-out can emit the proact_done "zoneOut" completion. Without (2) the
    // section-2a abort fires on entry and that completion path is unreachable
    // for every real run. Only meaningful for Kind::Dungeon plans; remains
    // false for Quest/FallbackZone.
    bool     enteredCandidateMap    = false;
    std::string blockerText;
    std::vector<std::string> commandsDispatched;
};

struct CadenceState
{
    uint64_t sessionStartedAtMs     = 0;
    uint64_t lastProposalAtMs       = 0;
    uint64_t lastIdleNudgeAtMs      = 0;
    uint16_t ignoredNudgeCount      = 0;
    // Increments each time a proposal expires unanswered (silent-ignore path).
    // Resets on player chat/quest/zone engagement. Self-silences the propose
    // loop when ≥ ProposalMaxPerSession — proposal analogue of FR-011's idle
    // nudge cap.
    uint16_t consecutiveExpiredProposals = 0;
    uint64_t lastPlayerActivityAtMs = 0;
    uint64_t lastProximityWarnAtMs  = 0;
    // Cooldown anchor for the level-up congratulation line, keyed per
    // (player,bot) like the fields above. Guards a rapid multi-level burst
    // (rested-XP dump, quest-turn-in chain, GM `.levelup`) from spamming the
    // line — 0 means "never sent this session."
    uint64_t lastLevelUpCongratsAtMs = 0;
    // Throttle anchor for the `proact_novoice` diagnostic. Only ever read/written
    // on the (playerGuid, 0) SENTINEL row — a voice-less tick has no bot guid to
    // key a normal (player,bot) row on. 0 means "not yet emitted this session."
    uint64_t lastNoVoiceAuditAtMs   = 0;
    uint64_t currentProposalId      = 0;     // 0 when none open
    uint64_t currentActivityPlanId  = 0;     // 0 when none executing (equals proposalId of executing plan)
    bool     inProximityWaiting     = false;
    // Phase C — last time the local-Ollama tick gate was consulted for this
    // (bot, player). Throttle key — re-queries are suppressed for
    // ProactiveTickGate.IntervalSec.
    uint64_t lastTickGateAtMs       = 0;
    // Last few rejected target keys (questId or mapId) so the candidate
    // selector skips them in the immediately next proposal (FR-005).
    std::deque<uint64_t> recentRejectedTargets;
    // Last known player position for movement detection (US3 activity tracking).
    float    lastPlayerX            = 0.0f;
    float    lastPlayerY            = 0.0f;
    float    lastPlayerZ            = 0.0f;
};

// ---------------------------------------------------------------------------
// Test/debug introspection (small, optional surface used by GM commands or
// audit-friendly logging). Implemented inline-trivially so far.
// ---------------------------------------------------------------------------

// Returns true iff the master gate + tactical gate + presence gate all pass
// for this player guid. Centralizes FR-012 / FR-018 so every code path can
// short-circuit on a single call.
bool IsEngaged(uint64_t playerGuid);

}  // namespace proactive
}  // namespace ollamachat

#endif  // MOD_OLLAMA_CHAT_PROACTIVE_H
