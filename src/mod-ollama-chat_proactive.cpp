// Proactive Leader Bot — feature 002 implementation.
//
// Reads as one file because the state-machine + audit + tap-ins all share the
// same private types and mutex. Public surface is in `_proactive.h` —
// EvaluateTick / OnPlayerChat / OnQuestComplete / OnZoneChange /
// OnPlayerLogin / OnPlayerLogout / OnConfigReloaded / IsEngaged.
//
// Design refs: specs/002-proactive-leader-bot/{spec,plan,data-model,research,
// contracts/*}.md.

#include "mod-ollama-chat_proactive.h"
#include "mod-ollama-chat_tactical.h"
#include "mod-ollama-chat_config.h"
#include "mod-ollama-chat_gateway.h"
#include "mod-ollama-chat_proactive_llm.h"
#include "mod-ollama-chat_jev.h"

#include "Chat.h"
#include "DatabaseEnv.h"
#include "DBCStores.h"
#include <fmt/format.h>
#include "Group.h"
#include "Log.h"
#include "ObjectAccessor.h"
#include "ObjectGuid.h"
#include "ObjectMgr.h"
#include "Player.h"
#include "PlayerbotAI.h"
#include "PlayerbotAIConfig.h"
#include "PlayerbotMgr.h"
#include "QuestDef.h"
#include "ScriptMgr.h"
#include "SharedDefines.h"
#include "World.h"
#include "WorldSession.h"
#include "Config.h"

#include <algorithm>
#include <atomic>
#include <chrono>
#include <cmath>
#include <cstring>
#include <fstream>
#include <random>
#include <sstream>
#include <unordered_set>

// ---------------------------------------------------------------------------
// New g_Proactive* globals — extern-declared in `_config.h`, populated by
// `_config.cpp` LoadOllamaChatConfig(). Defined here as the canonical home.
// ---------------------------------------------------------------------------

bool        g_ProactiveEnable                       = false;
uint32_t    g_ProactiveProposalCooldownSec          = 120;
uint32_t    g_ProactiveFirstProposalDelaySec        = 15;
uint32_t    g_ProactiveIdleThresholdSec             = 180;
uint32_t    g_ProactiveIdleNudgeCooldownSec         = 300;
uint32_t    g_ProactiveIdleNudgeMaxPerSession       = 3;
uint32_t    g_ProactiveProposalMaxPerSession        = 5;
float       g_ProactiveProximityYards               = 60.0f;
uint32_t    g_ProactiveProximityWarnCooldownSec     = 30;
uint32_t    g_ProactiveActivityCompleteTimeoutSec   = 1800;
uint32_t    g_ProactiveActivityCompleteNudgeWaitSec = 120;
uint32_t    g_ProactiveProposalExpirySec            = 600;
uint64_t    g_ProactivePreferredVoiceGuid           = 0;
std::string g_ProactiveProposerPromptFile;
std::string g_ProactiveProposerPromptText;
uint32_t    g_ProactiveMaxQuestRankSearch           = 50;
std::string g_ProactiveDungeonCandidates;
// Owns its own audit-write toggle so the cron audit-driven loop in
// cron-state/playbooks/wow-proactive.md does not depend on the gateway-wide
// EnableAudit toggle (which defaults to 0 on cost-sensitive deployments).
bool        g_ProactiveEnableAudit                  = true;
bool        g_ProactiveLevelUpCongratsEnable        = true;
uint32_t    g_ProactiveLevelUpCongratsCooldownSec   = 60;
// Diagnostic-only: surface the otherwise-invisible `ElectDesignatedVoice`
// returned-0 skip in Heartbeat (see kSrcNoVoice). Throttled per player so a
// permanently voice-less fleet cannot flood the audit table.
bool        g_ProactiveNoVoiceAuditEnable           = true;
uint32_t    g_ProactiveNoVoiceAuditCooldownSec      = 300;

// Proactive — LLM tiers (Phase A: intent classifier).
bool        g_ProactiveIntentEnable                 = true;
std::string g_ProactiveIntentPromptFile;
std::string g_ProactiveIntentPromptText;
float       g_ProactiveIntentMinConfidence          = 0.7f;
uint32_t    g_ProactiveIntentTimeoutMs              = 3000;

// Proactive — LLM tiers (Phase B: gateway planner). Default OFF (paid).
bool        g_ProactivePlannerEnable                = false;
std::string g_ProactivePlannerPromptFile;
std::string g_ProactivePlannerPromptText;

// Proactive — LLM tiers (Phase C: local-Ollama tick gate). Default OFF.
bool        g_ProactiveTickGateEnable               = false;
std::string g_ProactiveTickGatePromptFile;
std::string g_ProactiveTickGatePromptText;
uint32_t    g_ProactiveTickGateIntervalSec          = 30;
uint32_t    g_ProactiveTickGateTimeoutMs            = 2000;
float       g_ProactiveTickGateMinConfidence        = 0.6f;

namespace ollamachat
{
namespace proactive
{
namespace
{

// ---------------------------------------------------------------------------
// WoTLK 3.3.5a 5-man dungeon catalogue (R-4 / T027). Hand-curated single source
// of truth — no Map.dbc / areatrigger_teleport fallback. factionFlag: 0=both,
// 1=alliance-only entrance, 2=horde-only entrance. Entrance coords are
// informational in v1 (StartActivityPlan dispatches `follow` rather than
// travel-to-coord); populated as 0/0/0 with the understanding that a follow-up
// will wire accurate coords from acore_world.areatrigger_teleport once the
// dispatch path actually walks the bot to an entrance.
// ---------------------------------------------------------------------------
struct DungeonRow
{
    uint32_t    mapId;
    uint32_t    minLevel;
    uint32_t    maxLevel;
    uint32_t    continentMapId;
    float       entranceX;
    float       entranceY;
    float       entranceZ;
    uint8_t     factionFlag;    // 0=both, 1=alliance, 2=horde
    char const* name;
};

// Continents: 0 = Eastern Kingdoms, 1 = Kalimdor, 530 = Outland, 571 = Northrend.
constexpr DungeonRow kDungeonTable[] = {
    // Vanilla 5-mans
    { 389, 15, 25,   1, 0.0f, 0.0f, 0.0f, 2, "Ragefire Chasm"        },
    {  43, 17, 24,   1, 0.0f, 0.0f, 0.0f, 0, "Wailing Caverns"       },
    {  36, 18, 23,   0, 0.0f, 0.0f, 0.0f, 1, "Deadmines"             },
    {  33, 22, 30,   0, 0.0f, 0.0f, 0.0f, 0, "Shadowfang Keep"       },
    {  48, 24, 32,   1, 0.0f, 0.0f, 0.0f, 0, "Blackfathom Deeps"     },
    {  34, 24, 32,   0, 0.0f, 0.0f, 0.0f, 1, "The Stockade"          },
    {  90, 24, 33,   0, 0.0f, 0.0f, 0.0f, 0, "Gnomeregan"            },
    {  47, 30, 40,   1, 0.0f, 0.0f, 0.0f, 0, "Razorfen Kraul"        },
    { 129, 36, 46,   1, 0.0f, 0.0f, 0.0f, 0, "Razorfen Downs"        },
    { 189, 28, 38,   0, 0.0f, 0.0f, 0.0f, 0, "Scarlet Monastery"     },
    {  70, 41, 51,   0, 0.0f, 0.0f, 0.0f, 0, "Uldaman"               },
    { 209, 46, 55,   1, 0.0f, 0.0f, 0.0f, 0, "Zul'Farrak"            },
    { 349, 46, 55,   1, 0.0f, 0.0f, 0.0f, 0, "Maraudon"              },
    { 109, 50, 60,   0, 0.0f, 0.0f, 0.0f, 0, "Sunken Temple"         },
    { 230, 52, 60,   0, 0.0f, 0.0f, 0.0f, 0, "Blackrock Depths"      },
    { 229, 55, 60,   0, 0.0f, 0.0f, 0.0f, 0, "Blackrock Spire"       },
    { 329, 58, 60,   0, 0.0f, 0.0f, 0.0f, 0, "Stratholme"            },
    { 289, 58, 60,   0, 0.0f, 0.0f, 0.0f, 0, "Scholomance"           },
    { 429, 56, 60,   1, 0.0f, 0.0f, 0.0f, 0, "Dire Maul"             },
    // TBC 5-mans (Outland, continent 530)
    { 543, 60, 65, 530, 0.0f, 0.0f, 0.0f, 0, "Hellfire Ramparts"     },
    { 542, 61, 68, 530, 0.0f, 0.0f, 0.0f, 0, "The Blood Furnace"     },
    { 547, 62, 70, 530, 0.0f, 0.0f, 0.0f, 0, "The Slave Pens"        },
    { 546, 63, 70, 530, 0.0f, 0.0f, 0.0f, 0, "The Underbog"          },
    { 545, 65, 70, 530, 0.0f, 0.0f, 0.0f, 0, "The Steamvault"        },
    { 557, 64, 70, 530, 0.0f, 0.0f, 0.0f, 0, "Mana-Tombs"            },
    { 558, 65, 70, 530, 0.0f, 0.0f, 0.0f, 0, "Auchenai Crypts"       },
    { 556, 67, 70, 530, 0.0f, 0.0f, 0.0f, 0, "Sethekk Halls"         },
    { 555, 67, 70, 530, 0.0f, 0.0f, 0.0f, 0, "Shadow Labyrinth"      },
    { 554, 67, 70, 530, 0.0f, 0.0f, 0.0f, 0, "The Mechanar"          },
    { 553, 67, 70, 530, 0.0f, 0.0f, 0.0f, 0, "The Botanica"          },
    { 552, 67, 70, 530, 0.0f, 0.0f, 0.0f, 0, "The Arcatraz"          },
    { 540, 67, 70, 530, 0.0f, 0.0f, 0.0f, 0, "The Shattered Halls"   },
    { 585, 68, 70, 530, 0.0f, 0.0f, 0.0f, 0, "Magisters' Terrace"    },
    // WotLK 5-mans (Northrend, continent 571)
    { 574, 68, 72, 571, 0.0f, 0.0f, 0.0f, 0, "Utgarde Keep"          },
    { 576, 69, 72, 571, 0.0f, 0.0f, 0.0f, 0, "The Nexus"             },
    { 601, 70, 73, 571, 0.0f, 0.0f, 0.0f, 0, "Azjol-Nerub"           },
    { 619, 70, 73, 571, 0.0f, 0.0f, 0.0f, 0, "Ahn'kahet"             },
    { 600, 73, 75, 571, 0.0f, 0.0f, 0.0f, 0, "Drak'Tharon Keep"      },
    { 608, 73, 75, 571, 0.0f, 0.0f, 0.0f, 0, "Violet Hold"           },
    { 604, 74, 76, 571, 0.0f, 0.0f, 0.0f, 0, "Gundrak"               },
    { 599, 75, 77, 571, 0.0f, 0.0f, 0.0f, 0, "Halls of Stone"        },
    { 602, 76, 78, 571, 0.0f, 0.0f, 0.0f, 0, "Halls of Lightning"    },
    { 578, 77, 79, 571, 0.0f, 0.0f, 0.0f, 0, "The Oculus"            },
    { 575, 77, 80, 571, 0.0f, 0.0f, 0.0f, 0, "Utgarde Pinnacle"      },
    { 650, 80, 80, 571, 0.0f, 0.0f, 0.0f, 0, "Trial of the Champion" },
    { 632, 80, 80, 571, 0.0f, 0.0f, 0.0f, 0, "The Forge of Souls"    },
    { 658, 80, 80, 571, 0.0f, 0.0f, 0.0f, 0, "Pit of Saron"          },
    { 668, 80, 80, 571, 0.0f, 0.0f, 0.0f, 0, "Halls of Reflection"   },
};

// Parse `g_ProactiveDungeonCandidates` (csv of mapIds) into a set on demand.
// Empty csv means "no whitelist — all rows eligible".
std::unordered_set<uint32_t> ParseDungeonWhitelist(std::string const& csv)
{
    std::unordered_set<uint32_t> out;
    if (csv.empty()) return out;
    std::stringstream ss(csv);
    std::string tok;
    while (std::getline(ss, tok, ','))
    {
        // trim
        while (!tok.empty() && (tok.front() == ' ' || tok.front() == '\t')) tok.erase(tok.begin());
        while (!tok.empty() && (tok.back()  == ' ' || tok.back()  == '\t')) tok.pop_back();
        if (tok.empty()) continue;
        try { out.insert(static_cast<uint32_t>(std::stoul(tok))); }
        catch (...) { /* ignore non-numeric tokens */ }
    }
    return out;
}

// ---------------------------------------------------------------------------
// Audit `source_channel` labels. mod_ollama_chat_gateway_audit's column is
// VARCHAR(16), so all labels MUST be ≤15 chars (1-char safety margin). New
// channels MUST be declared here AND guarded by the static_assert block
// below; raw string literals at call sites silently truncate on insert.
// ---------------------------------------------------------------------------
constexpr char const* kSrcPropose   = "proact_propose";   // 14
constexpr char const* kSrcApprove   = "proact_approve";   // 14
constexpr char const* kSrcReject    = "proact_reject";    // 13
constexpr char const* kSrcNudge     = "proact_nudge";     // 12
constexpr char const* kSrcDone      = "proact_done";      // 11
constexpr char const* kSrcAbort     = "proact_abort";     // 12
constexpr char const* kSrcPlanner   = "proact_planner";   // 14
constexpr char const* kSrcTickGate  = "proact_tickgate";  // 15
constexpr char const* kSrcIntent    = "proact_intent";    // 13
constexpr char const* kSrcNoVoice   = "proact_novoice";   // 14

constexpr std::size_t AuditLabelLen(char const* s)
{
    std::size_t n = 0;
    while (s[n]) ++n;
    return n;
}
// VARCHAR(16) accommodates up to 16 chars; keep 1-char safety margin (≤15).
static_assert(AuditLabelLen(kSrcPropose)   <= 15, "audit label exceeds VARCHAR(16) safety margin");
static_assert(AuditLabelLen(kSrcApprove)   <= 15, "audit label exceeds VARCHAR(16) safety margin");
static_assert(AuditLabelLen(kSrcReject)    <= 15, "audit label exceeds VARCHAR(16) safety margin");
static_assert(AuditLabelLen(kSrcNudge)     <= 15, "audit label exceeds VARCHAR(16) safety margin");
static_assert(AuditLabelLen(kSrcDone)      <= 15, "audit label exceeds VARCHAR(16) safety margin");
static_assert(AuditLabelLen(kSrcAbort)     <= 15, "audit label exceeds VARCHAR(16) safety margin");
static_assert(AuditLabelLen(kSrcPlanner)   <= 15, "audit label exceeds VARCHAR(16) safety margin");
static_assert(AuditLabelLen(kSrcTickGate)  <= 15, "audit label exceeds VARCHAR(16) safety margin");
static_assert(AuditLabelLen(kSrcIntent)    <= 15, "audit label exceeds VARCHAR(16) safety margin");
static_assert(AuditLabelLen(kSrcNoVoice)   <= 15, "audit label exceeds VARCHAR(16) safety margin");

// ---------------------------------------------------------------------------
// Audit `requestText` REASON literals for the multi-reason channels.
//
// The audit schema stores NO verbatim text (audit-events.md "No request_text /
// response_text columns exist") — only `request_chars`. So for any channel that
// carries several distinct reasons under ONE `source_channel`, the reason is
// recoverable ONLY from its LENGTH, and the whole operator-query convention the
// runbook documents (`request_chars=13` = directCommand, `=16` = playerChangedMap,
// ...) rests on those lengths being PAIRWISE DISTINCT per channel.
//
// That was a comment-only convention, and it silently broke: `proact_abort`
// emitted BOTH "expired" (an Open proposal nobody answered) and "timeout" (an
// EXECUTING plan that blew the FR-021 completion deadline) at 7 chars each. Two
// semantically opposite terminal states — "the player ignored the offer" vs "the
// player said yes and then the activity stalled" — collapsed onto the same
// `request_chars=7`, with no other discriminator: both carry response_chars=0,
// latency_ms=0, and in the common voice==owner case the same bot_guid. Any
// abort-rate breakdown silently merged them.
//
// Naming them here makes the reason set enumerable, and the static_assert below
// makes the distinctness a COMPILE-TIME invariant instead of a comment a future
// change can miss (the 2-day-old `levelUp` addition to `proact_nudge` was a
// near-miss: 7 chars, unique only by luck).
// ---------------------------------------------------------------------------
constexpr char const* kRsnExpired           = "expired";              //  7
constexpr char const* kRsnActivityTimeout   = "activityTimeout";      // 15
constexpr char const* kRsnPlayerChangedZone = "playerChangedZone";    // 17
constexpr char const* kRsnPlayerChangedMap  = "playerChangedMap";     // 16
constexpr char const* kRsnOwnerLeftRange    = "ownerLeftRange";       // 14
constexpr char const* kRsnLogout            = "logout";               //  6
constexpr char const* kRsnBotLogout         = "botLogout";            //  9
constexpr char const* kRsnDirectCommand     = "directCommand";        // 13

constexpr char const* kRsnIdle              = "idle";                 //  4
constexpr char const* kRsnProximity         = "proximity";            //  9
constexpr char const* kRsnCompleteCheck     = "complete_check";       // 14
constexpr char const* kRsnLevelUp           = "levelUp";              //  7

constexpr char const* kRsnPlayerDone        = "playerDone";           // 10
constexpr char const* kRsnQuestTurnIn       = "questTurnIn";          // 11
constexpr char const* kRsnZoneOut           = "zoneOut";              //  7

constexpr char const* kRsnNoLeadersConfig   = "noLeadersConfigured";  // 19
constexpr char const* kRsnNoLeadersInWorld  = "noLeadersInWorld";     // 16
constexpr char const* kRsnNoLeaderOnMap     = "noLeaderOnMap";        // 13
constexpr char const* kRsnSelfOnly          = "selfOnly";             //  8

// One array per multi-reason channel. A new reason MUST be added to its
// channel's array, or the guard below cannot see it.
constexpr char const* kAbortReasons[] = {
    kRsnExpired, kRsnActivityTimeout, kRsnPlayerChangedZone, kRsnPlayerChangedMap,
    kRsnOwnerLeftRange, kRsnLogout, kRsnBotLogout, kRsnDirectCommand,
};
constexpr char const* kNudgeReasons[] = {
    kRsnIdle, kRsnProximity, kRsnCompleteCheck, kRsnLevelUp,
};
constexpr char const* kDoneReasons[] = {
    kRsnPlayerDone, kRsnQuestTurnIn, kRsnZoneOut,
};
constexpr char const* kNoVoiceReasons[] = {
    kRsnNoLeadersConfig, kRsnNoLeadersInWorld, kRsnNoLeaderOnMap, kRsnSelfOnly,
};

// True iff every reason in `arr` has a length no other reason in `arr` shares,
// i.e. `request_chars` uniquely identifies the reason within that channel.
constexpr bool ReasonLengthsDistinct(char const* const* arr, std::size_t n)
{
    for (std::size_t i = 0; i < n; ++i)
        for (std::size_t j = i + 1; j < n; ++j)
            if (AuditLabelLen(arr[i]) == AuditLabelLen(arr[j]))
                return false;
    return true;
}

// Adding a same-length reason to any of these channels is now a BUILD failure,
// not a silently undecodable audit row. Fix by renaming the new reason (keep it
// descriptive) and updating the request_chars decoder in
// specs/002-proactive-leader-bot/contracts/audit-events.md + docs/proactive-leader.md.
static_assert(ReasonLengthsDistinct(kAbortReasons, sizeof(kAbortReasons) / sizeof(kAbortReasons[0])),
              "proact_abort reason lengths must be pairwise distinct — request_chars is the only decoder");
static_assert(ReasonLengthsDistinct(kNudgeReasons, sizeof(kNudgeReasons) / sizeof(kNudgeReasons[0])),
              "proact_nudge reason lengths must be pairwise distinct — request_chars is the only decoder");
static_assert(ReasonLengthsDistinct(kDoneReasons, sizeof(kDoneReasons) / sizeof(kDoneReasons[0])),
              "proact_done reason lengths must be pairwise distinct — request_chars is the only decoder");
static_assert(ReasonLengthsDistinct(kNoVoiceReasons, sizeof(kNoVoiceReasons) / sizeof(kNoVoiceReasons[0])),
              "proact_novoice reason lengths must be pairwise distinct — request_chars is the only decoder");

// ---------------------------------------------------------------------------
// In-memory state.
// ---------------------------------------------------------------------------
std::mutex                                    s_mutex;
std::unordered_map<uint64_t, Proposal>        s_proposalsById;
std::unordered_map<uint64_t, ActivityPlan>    s_plansByProposalId;
// Key for cadence map: (playerGuid << 0) ^ (botGuid << 32) — keeps it cheap.
struct CadenceKey
{
    uint64_t playerGuid;
    uint64_t botGuid;
    bool operator==(CadenceKey const& o) const { return playerGuid == o.playerGuid && botGuid == o.botGuid; }
};
struct CadenceKeyHash
{
    std::size_t operator()(CadenceKey const& k) const noexcept
    {
        return std::hash<uint64_t>{}(k.playerGuid) ^ (std::hash<uint64_t>{}(k.botGuid) << 1);
    }
};
std::unordered_map<CadenceKey, CadenceState, CadenceKeyHash> s_cadenceByKey;

std::atomic<uint64_t> s_nextProposalId{1};

// ---------------------------------------------------------------------------
// Time helpers
// ---------------------------------------------------------------------------
uint64_t NowMs()
{
    using namespace std::chrono;
    return static_cast<uint64_t>(duration_cast<milliseconds>(steady_clock::now().time_since_epoch()).count());
}

// ---------------------------------------------------------------------------
// Cadence lookup (caller MUST hold s_mutex)
// ---------------------------------------------------------------------------
CadenceState& GetCadence(uint64_t playerGuid, uint64_t botGuid)
{
    return s_cadenceByKey[CadenceKey{playerGuid, botGuid}];
}

// ---------------------------------------------------------------------------
// Presence gating — replicates the public-private split in `_tactical.cpp`:
// a real human is online and in the auto-claim whitelist.
// ---------------------------------------------------------------------------
bool IsWhitelistedHumanOnline(uint64_t playerGuid)
{
    // Shared predicate (tactical.h): not a playerbot, not the Overlord session,
    // not a fleet guid, on a whitelisted account. No whitelist → no
    // proactivity (FR-012).
    Player* player = ObjectAccessor::FindPlayer(ObjectGuid(playerGuid));
    return IsWhitelistedHumanPlayer(player, /*anyHumanWhenNoWhitelist=*/false);
}

// ---------------------------------------------------------------------------
// Designated-voice election (FR-013, spec.md:81/129). Caller MUST hold
// s_mutex. A leader-capable bot is *eligible* when it is gateway-routed +
// leader-capable (Mcp.Leader.BotGUIDs), in-world, on the player's current map,
// AND is not the player itself (a bot never proposes to itself — the proposer
// and the target player MUST be distinct entities). Preference order:
//   1. Stickiness — the bot that already owns this player's in-flight proposal
//      (Open) or executing plan (its proposal sits at Approved), if eligible.
//   2. Operator's PreferredVoiceGuid override, if eligible.
//   3. Lowest-GUID eligible leader bot.
//
// Stickiness MUST come FIRST — even ahead of the preferred override. Reason:
// `Heartbeat()` feeds this function's result into `EvaluateTickLocked()` as the
// `botGuid`, and the entire executing-plan half of that function (section 2 —
// the `stay`/`follow` dispatch at ~_proactive.cpp:1333/1355, `CheckProximity`,
// the ProximityWarn phrasing, the playerChangedZone/timeout aborts, and every
// `proact_abort` / `proact_nudge` EmitAudit) keys off that `botGuid`, while the
// plan/proposal store their REAL owner in `Proposal::botGuid`. The code
// silently assumes voice == owner. spec.md:81 in fact requires the voice be
// "deterministic per session ... so the player doesn't get three simultaneous
// 'let's quest!' lines" — but a plain lowest-GUID re-election every heartbeat
// does NOT stay stable per session: the instant a second leader-capable bot
// becomes electable mid-plan (a lower-GUID leader bot wanders into the player's
// map, or the operator re-points PreferredVoiceGuid), the next heartbeat would
// elect the newcomer and `EvaluateTickLocked(newcomer, player)` would then
// drive the INCUMBENT's executing plan through the wrong bot: `stay`/`follow`
// go to the newcomer, proximity is measured against the newcomer's position,
// the warn line is phrased as the newcomer, and the abort/nudge audit rows
// carry the newcomer's `bot_guid` — while the incumbent's (player, incumbent)
// cadence row and its playerbot `stay` command leak with nothing left to manage
// or clean them (OnPlayerLogout only fires on a real logout, not a voice
// switch — the same leak class PR #195 closed for the bot-relog path, here for
// the voice-switch path). Keeping the voice sticky to the in-flight owner
// preserves voice == owner across the whole PROPOSE -> AWAITING_RESPONSE ->
// EXECUTING -> COMPLETED lifecycle, so every downstream consumer keeps
// targeting the right bot with zero other change. The preferred override and
// lowest-GUID election still govern every fresh cycle (no in-flight proposal)
// and resume the moment the incumbent becomes ineligible (left the map / logged
// out) or its proposal reaches a terminal state — which also satisfies the
// glossary's "re-evaluated when the in-range set changes" (spec.md:129): the
// re-evaluation runs every tick, it just resolves to the active-lifecycle owner
// instead of thrashing the voice mid-activity.
// ---------------------------------------------------------------------------
uint64_t ElectDesignatedVoice(uint64_t playerGuid)
{
    Player* player = ObjectAccessor::FindPlayer(ObjectGuid(playerGuid));
    if (!player || !player->IsInWorld()) return 0;
    uint32_t playerMap = player->GetMapId();

    auto eligible = [&](uint64_t guid) -> bool
    {
        // A player can NEVER be its own designated voice. The proactive
        // PROPOSE -> AWAITING_RESPONSE -> approve/reject cycle needs a DISTINCT
        // responder: a leader-capable bot proposing an activity to itself can
        // never have that proposal answered, so the cycle just opens -> expires
        // -> reopens forever (and burns a paid planner/proposer gateway call on
        // every generative tick). This guard is a no-op for the intended
        // human-target case — a human is never in g_McpLeaderBotGUIDSet, so it
        // could not be elected anyway — but a deployment that lists the leader
        // bots THEMSELVES in the FR-012 auto-claim presence set (so the bot
        // fleet drives the audit-feeding loop) would otherwise self-elect the
        // lowest-GUID bot as its own voice every heartbeat. With the guard,
        // election falls through to the next eligible leader bot on the player's
        // map, or returns 0 (Heartbeat skips the tick) when this player is the
        // only leader-capable bot in range — both correct: you do not propose to
        // yourself, and with no OTHER leader bot present there is no voice to
        // speak. Mirrors FR-013's "proactive voice FOR THAT PLAYER" framing,
        // which presupposes the voice and the target are distinct entities.
        if (guid == 0 || guid == playerGuid) return false;
        if (g_McpLeaderBotGUIDSet.count(guid) == 0) return false;
        Player* bot = ObjectAccessor::FindPlayer(ObjectGuid(guid));
        return bot && bot->IsInWorld() && bot->GetMapId() == playerMap;
    };

    // 1) Stickiness — an in-flight proposal/plan pins the voice to its owner.
    // A player carries at most one non-terminal proposal at a time (section 3
    // of EvaluateTickLocked never pipelines a second), so the first Open/
    // Approved match IS the incumbent; scanning to the end rather than breaking
    // on a first ineligible match just keeps us robust if that invariant ever
    // loosens. An executing plan's proposal is at State::Approved (StartActivity
    // Plan flips the PLAN to Executing but leaves the proposal Approved until a
    // terminal transition), so {Open, Approved} is exactly the voice-owning set.
    for (auto const& kv : s_proposalsById)
    {
        if (kv.second.playerGuid != playerGuid) continue;
        if (kv.second.state != State::Open && kv.second.state != State::Approved)
            continue;
        if (eligible(kv.second.botGuid))
            return kv.second.botGuid;
    }

    // 2) Operator's preferred-voice override.
    if (g_ProactivePreferredVoiceGuid != 0 && eligible(g_ProactivePreferredVoiceGuid))
        return g_ProactivePreferredVoiceGuid;

    // 3) Lowest-GUID eligible leader bot.
    uint64_t bestGuid = 0;
    for (uint64_t guid : g_McpLeaderBotGUIDSet)
    {
        if (!eligible(guid)) continue;
        if (bestGuid == 0 || guid < bestGuid)
            bestGuid = guid;
    }
    return bestGuid;
}

// ---------------------------------------------------------------------------
// Why did ElectDesignatedVoice return 0? Runs ONLY on the voice==0 path, so it
// costs nothing on a healthy tick. Mirrors `eligible()` above step-for-step —
// if that predicate ever changes, change this in lockstep or the diagnostic
// starts lying.
//
// Motivation: `Heartbeat` skipped a voice-less player with a bare `continue` —
// no log, no audit row, no counter. A leader-bot fleet dispersed across three
// continents therefore looked EXACTLY like a disabled subsystem, a broken
// config, or a code regression, and the operator/cron loop had to SSH in and
// hand-join `characters.online` against `Mcp.Leader.BotGUIDs` to tell those
// apart. Emitting the reason turns that multi-step manual probe into one
// `source_channel='proact_novoice'` query.
//
// Reason strings are LENGTH-DISTINCT on purpose: the audit schema stores only
// `request_chars`, never the text (audit-events.md "No request_text ...
// columns exist"), so the operator recovers the reason from the char count —
// 19 / 16 / 13 / 8 below. That distinctness is now enforced at compile time by
// the `kNoVoiceReasons` static_assert, so a new reason with a colliding length
// fails the build instead of shipping an undecodable row.
char const* DiagnoseNoVoice(uint64_t playerGuid)
{
    if (g_McpLeaderBotGUIDSet.empty())
        return kRsnNoLeadersConfig;     // 19 — Mcp.Leader.BotGUIDs unset

    bool anyOther = false;      // a leader-capable bot that is not the target
    bool anyInWorld = false;    // ... and is actually in world right now
    for (uint64_t guid : g_McpLeaderBotGUIDSet)
    {
        if (guid == 0 || guid == playerGuid) continue;
        anyOther = true;
        Player* bot = ObjectAccessor::FindPlayer(ObjectGuid(guid));
        if (bot && bot->IsInWorld()) { anyInWorld = true; break; }
    }

    if (!anyOther)   return kRsnSelfOnly;          //  8 — target IS the only leader
    if (!anyInWorld) return kRsnNoLeadersInWorld;  // 16 — all configured leaders offline
    // Leaders are online but none shares the target's map — the FR-013
    // eligibility rule that produced the 2026-06-15 → 2026-07-28 silence.
    return kRsnNoLeaderOnMap;                      // 13
}

// ---------------------------------------------------------------------------
// Audit emit — inserts one row into mod_ollama_chat_gateway_audit per state
// transition / chat line, gated on the proactive-owned EnableAudit toggle so
// the cron self-improvement loop survives a deployment with the gateway-wide
// toggle off. The schema only stores chars/tokens/latency counts +
// source_channel — verbatim chat text goes to worldserver.log via LOG_INFO.
// ---------------------------------------------------------------------------
// `backend` ("jev" | "llm" | "det" | nullptr) and `promptTokens` are recorded
// only for the rows a model produced; state-transition rows leave them NULL/0.
// The `backend` column exists only after Jev::EnsureAuditBackendColumns ran,
// so the INSERT widens its column list only once that is confirmed.
void EmitAudit(uint64_t playerGuid, uint64_t botGuid, const char* sourceChannel,
               std::string const& requestText, std::string const& responseText,
               uint32_t latencyMs, bool error = false,
               const char* backend = nullptr, uint32_t promptTokens = 0)
{
    Player* player = ObjectAccessor::FindPlayer(ObjectGuid(playerGuid));
    uint32_t accountId = 0;
    if (player && player->GetSession()) accountId = player->GetSession()->GetAccountId();

    // Inline insert (mirrors WriteGatewayAuditRecord in _gateway.cpp) so the
    // proactive subsystem is decoupled from the gateway-wide EnableAudit
    // toggle. Without this the audit-driven cron loop sees an empty table on
    // any deployment that keeps OllamaChat.Gateway.EnableAudit = 0 (its
    // default), even though proactive itself fired correctly.
    if (g_ProactiveEnableAudit)
    {
        if (Jev::AuditColumnsReady())
        {
            CharacterDatabase.Execute(
                "INSERT INTO mod_ollama_chat_gateway_audit "
                "(bot_guid, player_guid, account_id, request_chars, response_chars, prompt_tokens, "
                " completion_tokens, latency_ms, source_channel, error, backend) "
                "VALUES ({}, {}, {}, {}, {}, {}, {}, {}, '{}', {}, {})",
                botGuid, playerGuid, accountId,
                static_cast<uint32_t>(requestText.size()),
                static_cast<uint32_t>(responseText.size()),
                promptTokens, /*completionTokens=*/0,
                latencyMs, sourceChannel, error ? 1 : 0,
                backend ? std::string("'") + backend + "'" : std::string("NULL"));
        }
        else
        {
            CharacterDatabase.Execute(
                "INSERT INTO mod_ollama_chat_gateway_audit "
                "(bot_guid, player_guid, account_id, request_chars, response_chars, prompt_tokens, "
                " completion_tokens, latency_ms, source_channel, error) "
                "VALUES ({}, {}, {}, {}, {}, {}, {}, {}, '{}', {})",
                botGuid, playerGuid, accountId,
                static_cast<uint32_t>(requestText.size()),
                static_cast<uint32_t>(responseText.size()),
                promptTokens, /*completionTokens=*/0,
                latencyMs, sourceChannel, error ? 1 : 0);
        }
    }

    LOG_INFO("server.loading",
             "[Ollama Chat Proactive] audit src={} bot={} player={} backend={} req='{}' resp='{}'",
             sourceChannel, botGuid, playerGuid, backend ? backend : "-", requestText, responseText);
}

// ---------------------------------------------------------------------------
// Emit the throttled `proact_novoice` diagnostic for a player Heartbeat had to
// skip because no voice could be elected. Caller MUST hold s_mutex.
//
// Throttled on the (playerGuid, 0) SENTINEL cadence row — the same row
// OnPlayerLogin seeds for the session anchor — because there is by definition
// no bot to key a normal (player, bot) row on. Without the throttle a
// permanently dispersed fleet would write one row per player per heartbeat
// (~8.6k rows/player/day at the 10s tactical tick), swamping the very table
// the operator reads to diagnose it.
//
// `bot_guid` is 0: no bot was elected, so there is no owning/nudging bot to
// attribute this to. That is the honest value and is documented as the single
// exception in audit-events.md's bot_guid column semantics.
void EmitNoVoiceDiagnostic(uint64_t playerGuid, uint64_t now)
{
    if (!g_ProactiveNoVoiceAuditEnable) return;

    CadenceState& sentinel = GetCadence(playerGuid, 0);
    uint64_t cdMs = static_cast<uint64_t>(g_ProactiveNoVoiceAuditCooldownSec) * 1000;
    if (sentinel.lastNoVoiceAuditAtMs != 0 && now - sentinel.lastNoVoiceAuditAtMs < cdMs)
        return;
    sentinel.lastNoVoiceAuditAtMs = now;

    char const* reason = DiagnoseNoVoice(playerGuid);
    EmitAudit(playerGuid, /*botGuid=*/0, kSrcNoVoice, reason, "", 0);
}

// ---------------------------------------------------------------------------
// Channel selection (FR-017): party if both share a Group, else whisper.
// ---------------------------------------------------------------------------
EmitTarget PickChannel(Player* player, Player* bot)
{
    if (!player || !bot) return EmitTarget::Whisper;
    Group* pg = player->GetGroup();
    Group* bg = bot->GetGroup();
    if (pg && bg && pg == bg) return EmitTarget::Party;
    return EmitTarget::Whisper;
}

// ---------------------------------------------------------------------------
// The single emit funnel for every proactive chat line (T023a). Routes
// through the existing tactical ambient counters so a misbehaving proactive
// loop cannot exceed the module-wide spam budget (FR-019, SC-005).
//
// Returns true on success. False if suppressed by ambient cap (no chat sent)
// OR bot/player not in world.
// ---------------------------------------------------------------------------
bool EmitProactiveLine(uint64_t playerGuid, uint64_t botGuid, LineKind /*kind*/, std::string const& text)
{
    if (text.empty()) return false;

    Player* player = ObjectAccessor::FindPlayer(ObjectGuid(playerGuid));
    Player* bot    = ObjectAccessor::FindPlayer(ObjectGuid(botGuid));
    if (!player || !bot || !player->IsInWorld() || !bot->IsInWorld())
        return false;

    PlayerbotAI* botAI = PlayerbotsMgr::instance().GetPlayerbotAI(bot);
    if (!botAI)
    {
        LOG_DEBUG("server.loading",
                  "[Ollama Chat Proactive] emit: bot={} has no PlayerbotAI; dropping line",
                  botGuid);
        return false;
    }

    // NOTE on rate limiting: the existing tactical ambient counters
    // (g_TacticalAmbientMinSpeechGapSec / g_TacticalAmbientMaxVisibleActionsPerMinute)
    // live in `_tactical.cpp` anonymous namespace and are not exposed here.
    // The new Proactive.*Cooldown*Sec keys (ProposalCooldownSec /
    // IdleNudgeCooldownSec / ProximityWarnCooldownSec) enforce per-event
    // cooldowns at the caller; that's the v1 spam control. A follow-up will
    // wire the cross-system counters via an exposed helper in _tactical.cpp.

    EmitTarget channel = PickChannel(player, bot);
    std::string capped = text;
    if (capped.size() > 200) capped.resize(200);

    if (channel == EmitTarget::Party)
        botAI->SayToParty(capped);
    else
        botAI->Whisper(capped, player->GetName());

    LOG_INFO("server.loading",
             "[Ollama Chat Proactive] emit ch={} bot={} player={} text='{}'",
             channel == EmitTarget::Party ? "party" : "whisper",
             botGuid, playerGuid, capped);
    return true;
}

// ---------------------------------------------------------------------------
// Intent classification — C++-side string-matching on the player's message.
// The proactive yes/no/done set is small enough that an LLM round-trip would
// be wasteful; whole-token + first-N-token heuristics give us ≥95% accuracy
// (SC-002 target) on the hand-labelled bench (tests/proactive/) for free.
//
// The existing `TryGatewayIntentClassify` is action-oriented (it dispatches
// playerbot tool calls and returns a pre-canned ack); reusing it here would
// require either an LLM prompt rewrite to a label-based contract (T019,
// still deferred) or a fragile substring match on the LLM's free-form reply.
// String-side classification keeps the proactive loop deterministic, free,
// reproducible across model swaps, and unit-testable via the T060a bench.
// ---------------------------------------------------------------------------
enum class Intent : uint8_t
{
    None = 0,
    Approve,
    Reject,
    Done,
    DirectCommand,   // FR-014 takeover: "follow me", "stay", "attack" etc.
};

// Lower-case + strip punctuation; first-K tokenizer.
std::vector<std::string> TokenizeLowerStripped(std::string const& s, std::size_t maxTokens)
{
    std::vector<std::string> tokens;
    std::string cur;
    auto flush = [&]() {
        if (!cur.empty()) { tokens.push_back(cur); cur.clear(); }
    };
    for (char ch : s)
    {
        unsigned char uc = static_cast<unsigned char>(ch);
        if (std::isalpha(uc) || std::isdigit(uc) || ch == '\'')
            cur.push_back(static_cast<char>(std::tolower(uc)));
        else
            flush();
        if (tokens.size() >= maxTokens) break;
    }
    flush();
    if (tokens.size() > maxTokens) tokens.resize(maxTokens);
    return tokens;
}

bool ContainsAnyToken(std::vector<std::string> const& tokens, std::initializer_list<char const*> set)
{
    for (auto const& t : tokens)
        for (auto const* k : set)
            if (t == k) return true;
    return false;
}

// Array overload, so a hoisted vocabulary constant can be passed directly
// instead of being re-spelled as a brace list at the call site.
template <std::size_t N>
bool ContainsAnyToken(std::vector<std::string> const& tokens, char const* const (&set)[N])
{
    for (auto const& t : tokens)
        for (auto const* k : set)
            if (t == k) return true;
    return false;
}

bool StartsWithPhrase(std::string const& lower, std::initializer_list<char const*> set)
{
    for (auto const* k : set)
    {
        std::size_t n = std::strlen(k);
        if (lower.size() >= n && lower.compare(0, n, k) == 0)
        {
            // Require word boundary (end-of-string or non-alnum next).
            if (lower.size() == n) return true;
            char next = lower[n];
            if (!std::isalnum(static_cast<unsigned char>(next))) return true;
        }
    }
    return false;
}

// The Approve token vocabulary — ONE definition with THREE consumers: the
// firstThree Approve matcher in ClassifyPlayerMessage, and the two
// affirmative-opener strips below. An opener only ever needs stripping BECAUSE
// it would otherwise win that matcher, so the three must agree by construction.
// They previously agreed only by a "keep in lockstep" comment over separately
// maintained copies — the bench mirror has always modelled this correctly
// (`AFFIRMATIVE_OPENERS = APPROVE_TOKENS` in bench_classifier.py); this brings
// the C++ side to the same single-definition shape.
constexpr char const* kAffirmativeOpeners[] = {
    "yes", "yep", "yeah", "ok", "okay", "sure", "aight",
    "alright", "yup", "fine", "absolutely", "definitely", "kk",
};

// Collapse every run of non-alphanumeric characters (apostrophe preserved) to a
// single space, dropping leading/trailing separators. The discourse-override
// list below is matched with StartsWithPhrase, which compares a CONTIGUOUS
// prefix of the raw lowercased string — so its single-space entries ("fine but",
// "ok but", "sounds good but", "yeah no", …) silently MISSED the punctuated
// forms a real player types: "fine, but not that one" / "ok... but later" /
// "yeah, no" / "sounds good — but maybe tomorrow". Each of those then fell
// through to the firstThree Approve matcher and classified as Approve — the
// worst-possible miss, since the bot would START EXECUTING an activity the
// player was explicitly declining. Normalizing here fixes EVERY override entry
// at once (comma / ellipsis / em-dash / any separator), completing the
// contrastive-opener arc PRs #129 → #231 → #239 began. Apostrophes are kept so
// "i'm"/"let's"/"we're" survive, mirroring TokenizeLowerStripped's char class.
std::string CollapsePunctToSpace(std::string const& lower)
{
    std::string out;
    out.reserve(lower.size());
    bool pendingSpace = false;
    for (char ch : lower)
    {
        unsigned char uc = static_cast<unsigned char>(ch);
        if (std::isalnum(uc) || ch == '\'')
        {
            if (pendingSpace && !out.empty()) out.push_back(' ');
            pendingSpace = false;
            out.push_back(ch);
        }
        else
        {
            pendingSpace = true;   // defer: collapses runs, drops leading/trailing
        }
    }
    return out;
}

// Discourse override — phrases that contain an intent token but are
// conversational filler in context ("no idea", "next mob", "let's wait",
// "i'm not done yet"). Without this, the firstThree / firstFive token
// matchers below false-positive these into Reject / Done / Approve and
// burn a proposal slot or trigger a spurious done transition. Returning
// None routes them through the normal chat path while leaving the
// proposal/plan state intact.
//
// Always matched against a PUNCTUATION-NORMALIZED view (CollapsePunctToSpace) so
// a comma/ellipsis/em-dash between the approve token and "but" no longer slips
// past — "fine, but …" / "ok... but …" / "yeah, no" route here, not to Approve.
//
// Hoisted into a named predicate because it is consulted TWICE: once on the
// message as typed, and once on the remainder left by
// StripAffirmativeNegationOpener below (so "yeah, not done yet" reaches the
// "not done" hedge). One list, one definition — a future entry lands on both
// call sites automatically.
bool IsDiscourseOverride(std::string const& normalized)
{
    return StartsWithPhrase(normalized, {
            // "no <distancing>" — not a rejection.
            "no idea", "no problem", "no rush", "no clue",
            "no worries", "no offense", "no way",
            // "not <state>" — not a completion / not a rejection of the
            // current activity. ("not that"/"not now"/"not really" stay as
            // reject phrases below.)
            "not done", "not finished", "not ready", "not sure",
            "i'm not done", "im not done",
            "i'm not finished", "im not finished",
            // "let's <hedge>" — not an approval.
            "let's wait", "lets wait", "let's see", "lets see",
            "let's not", "lets not", "let's hold", "lets hold",
            "let's think", "lets think",
            // "next <gameplay-noun>" — referring to the next in-game thing,
            // not "give me the next activity".
            "next time", "next pull", "next mob",
            "next group", "next fight",
            // Discourse markers — approve-tokens used as turn-openers. The
            // "<approve-token> but ..." contrastive opener reads as approval
            // (the leading token wins the firstThree matcher) but the "but"
            // flips it to a soft decline / hedge / question, so it must route
            // to normal chat, not burn the proposal. "ok"/"okay" are the two
            // highest-frequency approve tokens, so "ok but"/"okay but" are the
            // MOST common of these openers — yet they were the two missing from
            // the set ("sure but"/"alright but"/"yeah but" were already here).
            // That incomplete enumeration let "ok but not that one" classify as
            // Approve and start executing an activity the player was explicitly
            // declining (the worst-possible miss). Same incomplete-enumeration
            // family as the audit-events.md list fixes in PR #191/#200.
            "ok so", "okay so", "sure but", "alright but", "yeah but",
            "ok but", "okay but",
            // Completing the approve-token enumeration: the remaining firstThree
            // Approve tokens ("yes"/"yep"/"yup"/"fine"/"aight") and the
            // "sounds good"/"sounds great" Approve phrases had no contrastive
            // override either, so "fine but not that one" / "yep but later" /
            // "sounds good but maybe tomorrow" still false-classified as Approve
            // (same worst-possible miss). ("absolutely"/"definitely"/"kk" left
            // out — emphatic-agreement tokens whose "<tok> but" form is rare.)
            "yes but", "yep but", "yup but", "fine but", "aight but",
            "sounds good but", "sounds great but",
            // "yeah <negation>" — the spoken-English reversal idiom where a
            // "yeah"-prefixed reply still means no ("yeah no", "yeah nah").
            // The leading "yeah" token alone would otherwise win the Approve
            // matcher below; the negation that follows makes it a non-approval.
            // These two are pinned to None HERE (ahead of the generalized
            // reversal strip below) purely to preserve their established
            // labels — a bare "yeah no" carries no decline body, so None and
            // Reject are equally safe and neither executes anything.
            "yeah no", "yeah nah",
        });
}

// Affirmative-opener reversal idiom. A message can OPEN with a token the Approve
// matcher treats as agreement and then immediately NEGATE it — "yeah, not right
// now", "ok not now", "yup not really", "absolutely no". `ContainsAnyToken`
// scans the first THREE tokens and Approve is tested before Reject, so the
// leading affirmative wins unconditionally and the whole message classifies as
// Approve: the bot acks and starts executing an activity the player just
// declined. That is the worst-possible miss — the same one the
// "<approve-token> but …" contrastive-opener arc (PRs #129/#231/#239/#244) and
// the bare soft-decline wedge (PR #364) each closed for their own shape.
//
// The override list above could not close THIS shape because it is a CROSS
// PRODUCT — 13 approve tokens x every decline body the reject vocabulary knows —
// which is exactly why only two of its members ("yeah no", "yeah nah") were ever
// enumerated. So instead of listing forms, strip the opener and let the EXISTING
// vocabulary judge the remainder:
//     "yeah, not right now" -> "not right now" -> Reject   (reject phrase)
//     "absolutely no"       -> "no"            -> Reject   (reject token)
//     "yeah not done yet"   -> "not done yet"  -> None     (override hedge)
//     "absolutely not"      -> "not"           -> None     (see below)
//
// A bare trailing "not" resolves to None rather than Reject on purpose: "not" is
// deliberately absent from the reject TOKEN list, because ContainsAnyToken scans
// anywhere in the first three tokens and would then fire on "why not" — which is
// an APPROVAL. None is the correct ceiling for a decline body the vocabulary does
// not recognise: it routes to normal chat instead of executing.
//
// Returns the remainder when the pattern matches, or an EMPTY string when it does
// not — in which case every matcher below reads the message exactly as before and
// behaviour is byte-identical. That inertness is the safety property: the rule
// cannot perturb any phrase that does not literally open
// "<affirmative> <negation>".
//
// Guard: "<affirmative> not a/an <noun>" is a benign AGREEMENT, not a decline
// ("sure, not a problem" / "yeah not an issue"), so an article directly after the
// negation suppresses the strip. Same motivation as the "no problem" / "no
// worries" / "no offense" entries in the override list above.
//
// `normalized` MUST be the CollapsePunctToSpace view, so "yeah, not right now"
// and "yeah not right now" take the same path.
std::string StripAffirmativeNegationOpener(std::string const& normalized)
{
    // Openers come from the file-scope kAffirmativeOpeners above — the same
    // list the firstThree Approve matcher tests, by construction.
    static constexpr char const* kNegationHeads[]      = { "not", "no", "nope", "nah" };
    static constexpr char const* kBenignAfterNegation[] = { "a", "an" };

    auto tokenAt = [](std::string const& s, std::size_t from, std::size_t& next) -> std::string
    {
        std::size_t sp = s.find(' ', from);
        next = (sp == std::string::npos) ? std::string::npos : sp + 1;
        return s.substr(from, sp == std::string::npos ? std::string::npos : sp - from);
    };

    std::size_t afterFirst = std::string::npos;
    std::string first = tokenAt(normalized, 0, afterFirst);
    if (afterFirst == std::string::npos) return {};   // single token — nothing to negate
    bool affirmative = false;
    for (auto const* t : kAffirmativeOpeners)
        if (first == t) { affirmative = true; break; }
    if (!affirmative) return {};

    std::size_t afterSecond = std::string::npos;
    std::string second = tokenAt(normalized, afterFirst, afterSecond);
    bool negated = false;
    for (auto const* n : kNegationHeads)
        if (second == n) { negated = true; break; }
    if (!negated) return {};

    if (afterSecond != std::string::npos)
    {
        std::size_t afterThird = std::string::npos;
        std::string third = tokenAt(normalized, afterSecond, afterThird);
        for (auto const* b : kBenignAfterNegation)
            if (third == b) return {};   // "not a problem" / "not an issue"
    }

    return normalized.substr(afterFirst);
}

// Interrogative-negation agreement idiom — the MIRROR of the reversal above.
// "why not" is a rhetorical question that answers YES, but it carries neither an
// approve token nor an approve phrase, so it fell through every matcher to None
// and the approval never registered: the proposal sat Open until the ~600s
// expiry while the player believed they had said yes.
//
// It also cannot be closed by adding "not" to the reject TOKEN list —
// ContainsAnyToken scans anywhere in the first three tokens, so a bare "not"
// would false-Reject this very phrase. That constraint is why "why not" is
// called out explicitly in StripAffirmativeNegationOpener's comment above.
//
// Enumerating it as an approve PHRASE would be wrong in the other direction: the
// rhetorical question can carry a body that reverses it ("why not skip it" /
// "why not something else" are declines), and Approve is tested before Reject,
// so a plain "why not" approve entry would swallow them into the
// worst-possible miss. So use the same rule the reversal uses — strip the opener
// and let the EXISTING vocabulary judge the remainder:
//     "why not"                -> ""               -> Approve (nothing declines it)
//     "why not, let's go"      -> "let's go"       -> Approve (approve phrase)
//     "why not skip it"        -> "skip it"        -> Reject  (reject token)
//     "why not something else" -> "something else" -> Reject  (reject phrase)
//     "why not next time"      -> "next time"      -> None    (override hedge)
//
// An UNMATCHED remainder is what makes the bare form an approval, so the caller
// returns Approve only after every reject matcher has declined to fire.
//
// Returns true when the idiom opens the message, writing the remainder (possibly
// EMPTY — the bare "why not") to `remainder`. Returns false and leaves
// `remainder` untouched otherwise, so every matcher reads the message
// byte-identically to before. Same inertness property as the reversal rule.
//
// `normalized` MUST be the CollapsePunctToSpace view, so "why not?" and
// "why not, let's go" take the same path as the unpunctuated forms.
bool StripInterrogativeNegationOpener(std::string const& normalized, std::string& remainder)
{
    static constexpr char const kOpener[] = "why not";
    std::size_t const n = sizeof(kOpener) - 1;

    if (normalized.size() < n || normalized.compare(0, n, kOpener) != 0)
        return false;
    if (normalized.size() == n)   // bare "why not"
    {
        remainder.clear();
        return true;
    }
    if (normalized[n] != ' ')     // word boundary — "why nothing", "why nots"
        return false;

    remainder = normalized.substr(n + 1);
    return true;
}

// Bare soft declines — a polite refusal carrying NONE of the reject tokens
// (no/nope/nah/skip/pass/different) and none of the "not <x>" reject phrases, so
// it fell through to None: the reject never registered, the proposal lingered
// Open to the ~600s expiry, and the same candidate stayed re-proposable because
// RememberRejection never ran. That is the FR-005 same-proposal-twice family PR
// #364 closed for its own shape.
//
// This one needs its own predicate rather than another REJECT_PHRASES entry
// because a naive "i'm good" entry ALSO captures "i'm good to go" — an
// APPROVAL — turning a silent miss into an actively wrong Reject. A "to <verb>"
// continuation reverses the reading, so it suppresses the decline exactly the
// way "not a/an <noun>" suppresses the reversal strip above.
//
// `normalized` MUST be the CollapsePunctToSpace view: the continuation has to be
// inspected as a TOKEN, and on the raw lowercase string "i'm good, thanks" and
// "i'm good to go" are both just "i'm good" + a non-alnum byte.
bool IsBareSoftDecline(std::string const& normalized)
{
    static constexpr char const* kBareDeclineStems[] = {
        "i'm good", "im good", "i'm all set", "im all set",
    };

    for (auto const* stem : kBareDeclineStems)
    {
        std::size_t n = std::strlen(stem);
        if (normalized.size() < n || normalized.compare(0, n, stem) != 0)
            continue;
        if (normalized.size() == n)
            return true;              // exact — "i'm good"
        if (normalized[n] != ' ')
            continue;                 // word boundary — "im goods"
        // "to <verb>" reverses the decline: "i'm good to go" / "i'm good to start".
        if (StartsWithPhrase(normalized.substr(n + 1), { "to" }))
            return false;
        return true;                  // "i'm good thanks" / "i'm all set sorry"
    }
    return false;
}

// Lone-affirmative-opener strip — the THIRD member of the opener family, and
// the one that closes the last reachable worst-possible miss in it.
//
// IsBareSoftDecline above resolves a BARE polite refusal ("i'm good" / "i'm all
// set") to Reject. But the moment a player puts a turn-opener in front of it —
// "yeah i'm good", "ok im all set", "sure i'm good thanks" — the leading token
// wins the firstThree Approve matcher, and Approve is tested before that
// predicate ever runs. So the bot acks and STARTS EXECUTING the activity the
// player just politely refused.
//
// Neither existing rule can reach it. StripAffirmativeNegationOpener requires
// the SECOND token to be a negation head (not/no/nope/nah), and "i'm" is not
// one. The discourse-override list enumerated exactly ONE member of the family
// ("yeah no"), which is precisely why the gap looked covered for so long: the
// single enumerated form hid the remaining 13 openers x 4 stems.
//
// Same strip-and-re-judge shape as its two siblings, but with the STRICTEST
// firing condition of the three: this one does not offer the remainder to the
// whole vocabulary, only to IsBareSoftDecline. That is deliberate. A lone
// affirmative opener carries no negation of its own, so stripping it and
// re-judging broadly would start reinterpreting ordinary approvals ("ok lets
// go" -> "lets go") — a no-op at best, an over-suppression at worst. Gating on
// the one predicate that is itself already guarded keeps this rule inert by
// construction: it can fire ONLY on "<affirmative> <bare soft decline>", a
// shape that today resolves Approve in 100% of cases and is wrong in 100% of
// them.
//
// The continuation carve-out is inherited for free rather than re-implemented:
// "yeah i'm good to go" leaves remainder "i'm good to go", IsBareSoftDecline
// returns false on the "to <verb>" continuation, the rule does not fire, and
// the message reaches the Approve matcher exactly as before (where "i'm good to
// go" is also an explicit Approve phrase).
//
// Returns true and writes the remainder when the message opens with a lone
// affirmative token; returns false and leaves `remainder` untouched otherwise,
// so every matcher below reads the message byte-identically to before.
//
// `normalized` MUST be the CollapsePunctToSpace view — both because
// IsBareSoftDecline needs to inspect the continuation as a TOKEN, and so that
// "yeah, i'm good" and "yeah i'm good" take the same path.
bool StripAffirmativeOpener(std::string const& normalized, std::string& remainder)
{
    std::size_t const sp = normalized.find(' ');
    if (sp == std::string::npos) return false;   // single token — nothing follows

    std::string const first = normalized.substr(0, sp);
    for (auto const* t : kAffirmativeOpeners)
    {
        if (first == t)
        {
            remainder = normalized.substr(sp + 1);
            return true;
        }
    }
    return false;
}

Intent ClassifyPlayerMessage(uint64_t botGuid, uint64_t playerGuid, std::string const& message,
                             bool hasOpenProposal, bool hasExecutingPlan)
{
    if (message.empty()) return Intent::None;
    if (message.size() > 256) return Intent::None;   // skip long conversational text

    // Lower-case the whole string for startswith checks.
    std::string lower = message;
    for (auto& c : lower) c = static_cast<char>(std::tolower(static_cast<unsigned char>(c)));

    // Punctuation-normalized view for the discourse-override pass — see
    // CollapsePunctToSpace: it lets the single-space override entries catch the
    // punctuated contrastive forms ("fine, but …" / "yeah, no") that the raw
    // `lower` prefix-match misses and which would otherwise false-Approve.
    std::string const normalized = CollapsePunctToSpace(lower);

    // Direct-command keywords always cancel the proposal regardless of state
    // (FR-014). MUST stay above any LLM round-trip: a "follow me" should not
    // wait on Ollama before reaching the playerbot command handler.
    if (StartsWithPhrase(lower, {
            "follow me", "follow", "stay", "stop", "wait here", "summon",
            "attack", "kill", "engage", "eat", "drink", "rest", "repair",
            "maintenance", "wipe", "back off", "reset", "release", "hearth",
            "go home", "leave group", "leave party", "revive", "loot"
        }))
    {
        return Intent::DirectCommand;
    }

    // Discourse override — conversational filler that merely CONTAINS an intent
    // token ("no idea", "next mob", "let's wait", "yeah but …"). See
    // IsDiscourseOverride for the full list + rationale.
    if (IsDiscourseOverride(normalized))
        return Intent::None;

    // Affirmative-opener reversal — "yeah, not right now" / "ok not now" /
    // "absolutely no". Strip the agreement token that would otherwise win the
    // firstThree Approve matcher and judge the REMAINDER with the same
    // vocabulary; see StripAffirmativeNegationOpener. The override list is
    // re-consulted on the remainder so "yeah not done yet" still reaches the
    // "not done" hedge. When the idiom does not fire, `reversal` is empty and
    // `view` IS the original message — every matcher below is unchanged.
    std::string const reversal = StripAffirmativeNegationOpener(normalized);
    if (!reversal.empty() && IsDiscourseOverride(reversal))
        return Intent::None;

    // Interrogative-negation agreement — "why not" answers YES. Same
    // strip-and-re-judge rule as the reversal above, so a body that flips it
    // ("why not skip it") still reaches the reject vocabulary; the bare form is
    // resolved to Approve at the BOTTOM of the open-proposal block, only once
    // nothing else has claimed it. See StripInterrogativeNegationOpener.
    std::string interrogative;
    bool const whyNot = reversal.empty()
                     && StripInterrogativeNegationOpener(normalized, interrogative);
    if (whyNot && IsDiscourseOverride(interrogative))
        return Intent::None;

    // `view` stays the RAW lowercase message in the common case so the existing
    // prefix matchers are byte-identical; `normalizedView` is the same text with
    // punctuation collapsed, for matchers that must inspect a CONTINUATION token
    // rather than just a prefix (IsBareSoftDecline). They differ only when
    // neither opener rule fired.
    std::string const view = !reversal.empty() ? reversal
                           : whyNot            ? interrogative
                                               : lower;
    std::string const normalizedView = !reversal.empty() ? reversal
                                     : whyNot            ? interrogative
                                                         : normalized;

    // Phase A — local-Ollama intent classifier in front of the deterministic
    // substring matcher. Gated by ProactiveIntent.Enable (default ON); falls
    // through silently on disabled / HTTP timeout / parse error / low
    // confidence so the C++ matcher below is the unconditional safety net.
    if (g_ProactiveIntentEnable && !g_ProactiveIntentPromptText.empty()
        && (hasOpenProposal || hasExecutingPlan))
    {
        llm::IntentResult r = llm::ClassifyIntent(botGuid, playerGuid, message,
                                                   hasOpenProposal, hasExecutingPlan);
        if (!r.rawContent.empty())
        {
            EmitAudit(playerGuid, botGuid, kSrcIntent,
                      message, r.rawContent, r.latencyMs, /*error=*/false,
                      r.backend, r.promptTokens);
        }
        // r.minConfidence is the threshold for the tier that answered (jev's
        // calibrated knob vs the LLM's self-report knob) — never a fixed global.
        if (r.confidence >= r.minConfidence)
        {
            switch (r.label)
            {
                case llm::IntentLabel::Approve:
                    if (hasOpenProposal) return Intent::Approve;
                    break;
                case llm::IntentLabel::Reject:
                    if (hasOpenProposal) return Intent::Reject;
                    break;
                case llm::IntentLabel::Done:
                    if (hasExecutingPlan) return Intent::Done;
                    break;
                case llm::IntentLabel::Other:
                    // A CONFIDENT "not about the proposal" is final. Falling
                    // through let the keyword matcher overrule it: "Quick
                    // check, no actions: what is my character name?" hit "no"
                    // in its first three tokens, became a Reject, and the
                    // question was swallowed — Claude never answered (E2E
                    // harness S6, 2026-09-25). The matcher is the fallback for
                    // a failed or unsure classifier, not a second opinion.
                    // None stays a fallthrough: it is also the parse-failure
                    // sentinel (an unknown label keeps its confidence) — codex.
                    return Intent::None;
                case llm::IntentLabel::None:
                default:
                    break;   // fall through to C++ matcher
            }
        }
    }

    // Every matcher below reads `view`, not the raw message: it is the message
    // verbatim in the overwhelmingly common case, and the negation remainder when
    // the affirmative-opener reversal fired. TokenizeLowerStripped lowercases
    // internally, so tokenizing the already-lowered `view` is equivalent.
    auto firstThree = TokenizeLowerStripped(view, 3);
    auto firstFive  = TokenizeLowerStripped(view, 5);

    // Approve — must appear in the first ~3 tokens to avoid false positives.
    if (hasOpenProposal)
    {
        // Affirmative opener in front of a BARE SOFT DECLINE — "yeah i'm good"
        // / "ok im all set". MUST precede the Approve matcher: the whole defect
        // is that the leading token wins firstThree before the decline is ever
        // examined. Composed from two predicates rather than folded into either
        // one so it fires ONLY on that exact shape; see StripAffirmativeOpener.
        std::string softDecline;
        if (StripAffirmativeOpener(normalizedView, softDecline)
            && IsBareSoftDecline(softDecline))
            return Intent::Reject;

        if (ContainsAnyToken(firstThree, kAffirmativeOpeners))
            return Intent::Approve;
        if (StartsWithPhrase(view, {
                "let's", "lets", "do it", "go for", "go ahead", "lead the",
                "sounds good", "sounds great", "i'm in", "im in", "i'm down",
                "im down", "down for",
                // The agreement reading of the bare soft decline below —
                // "i'm good TO GO" means yes. Listed here (Approve is tested
                // first) so the idiom resolves positively instead of merely
                // being spared by IsBareSoftDecline's continuation guard.
                "i'm good to go", "im good to go"
            }))
            return Intent::Approve;

        // Reject — first tokens.
        if (ContainsAnyToken(firstThree, {
                "no", "nope", "nah", "skip", "pass", "different"
            }))
            return Intent::Reject;
        if (StartsWithPhrase(view, {
                "not that", "not now", "something else", "different one",
                "not really", "maybe later", "later",
                // Soft declines carrying NO leading reject token (no/nope/nah/
                // skip/pass/different) — they fell through to None, so the
                // reject never registered: the proposal lingered Open until the
                // ~600s expiry and the same candidate could be re-proposed
                // before RememberRejection ran (FR-005 same-proposal-twice
                // family). "not right now" is the natural filler variant of
                // "not now" above (the inserted "right" broke the prefix match);
                // "not interested"/"not for me" are the bare forms of the
                // dataset's already-covered "nah not interested"/"no not
                // interested". The "not <state>" hedges (not done/finished/
                // ready/sure) stay None via the discourse-override pass, which
                // runs ABOVE this — see the "not <state>" block there.
                "not interested", "not for me", "not right now",
                "rather not", "i'd rather not", "id rather not", "maybe not",
                // Absent from the vocabulary entirely — bare OR prefixed — so
                // "not today" and "yeah not today" both landed on None and the
                // reject never registered. It has no benign reading as an
                // answer to a proposal, unlike the "not <state>" hedges that
                // the discourse-override pass deliberately keeps at None.
                "not today"
            }))
            return Intent::Reject;

        // Polite refusals with no reject token at all — "i'm good" / "i'm all
        // set". Guarded against the "i'm good to go" agreement reading; see
        // IsBareSoftDecline.
        if (IsBareSoftDecline(normalizedView))
            return Intent::Reject;

        // "why not" with nothing that declines it IS the approval. Deliberately
        // LAST in the block: the remainder was offered to every reject matcher
        // above first, so "why not skip it" / "why not something else" have
        // already returned Reject and only the genuinely affirmative forms
        // reach here.
        if (whyNot)
            return Intent::Approve;
    }

    // Done — anywhere in the first 5 tokens. `next` is included as a token
    // because in the executing-plan context, a bare "next" / "next one" /
    // "next quest" reliably means "I'm done with this, move on."
    if (hasExecutingPlan)
    {
        if (ContainsAnyToken(firstFive, {
                "done", "finished", "completed", "next"
            }))
            return Intent::Done;
        if (StartsWithPhrase(view, {
                "what's next", "whats next", "next one", "ok next",
                "we're done", "were done", "turned it", "i'm done", "im done",
                "i'm finished", "im finished"
            }))
            return Intent::Done;
    }

    return Intent::None;
}

// ---------------------------------------------------------------------------
// Phrasing — generative via QueryGatewayAPIRaw when the proposer prompt is
// loaded and gateway is enabled; deterministic fallback otherwise. Returns
// the trimmed single line; empty on total failure (caller skips emitting).
// ---------------------------------------------------------------------------

// Forward decl — defined alongside the candidate-selection helpers below.
std::string ZoneNameById(uint32_t zoneId);

char const* LineKindToString(LineKind k)
{
    switch (k)
    {
        case LineKind::Proposal:      return "proposal";
        case LineKind::ApprovalAck:   return "approval_ack";
        case LineKind::Waiting:       return "waiting";
        case LineKind::ProximityWarn: return "proximity_warn";
        case LineKind::IdleNudge:     return "idle_nudge";
        case LineKind::CompleteCheck: return "complete_check";
        case LineKind::CompleteAck:   return "complete_ack";
        case LineKind::AbortAck:      return "abort_ack";
        case LineKind::LevelUp:       return "level_up";
    }
    return "proposal";
}

std::string JsonEscape(std::string const& s)
{
    std::string out;
    out.reserve(s.size() + 8);
    for (char ch : s)
    {
        switch (ch)
        {
            case '\\': out += "\\\\"; break;
            case '"':  out += "\\\""; break;
            case '\n': out += "\\n"; break;
            case '\r': out += "\\r"; break;
            case '\t': out += "\\t"; break;
            default:
                if (static_cast<unsigned char>(ch) < 0x20)
                {
                    char buf[8];
                    std::snprintf(buf, sizeof(buf), "\\u%04x", static_cast<unsigned>(ch));
                    out += buf;
                }
                else out += ch;
        }
    }
    return out;
}

// Strip whitespace, surrounding quotes/backticks, and an optional "Bot:" prefix
// from a generative response. Caps at 200 chars (per EmitProactiveLine cap).
std::string SanitizeGeneratedLine(std::string s)
{
    while (!s.empty() && std::isspace(static_cast<unsigned char>(s.front()))) s.erase(s.begin());
    while (!s.empty() && std::isspace(static_cast<unsigned char>(s.back())))  s.pop_back();
    // Strip surrounding quotes.
    if (s.size() >= 2 && ((s.front() == '"' && s.back() == '"')
                       || (s.front() == '\'' && s.back() == '\'')))
    {
        s = s.substr(1, s.size() - 2);
    }
    if (s.size() > 200) s.resize(200);
    return s;
}

std::string DeterministicPhrasing(LineKind kind, Candidate const& candidate,
                                   std::string const& playerName, std::string const& contextNote)
{
    std::string const& name = playerName.empty() ? std::string("friend") : playerName;
    switch (kind)
    {
        case LineKind::Proposal:
            if (candidate.kind == Kind::Quest)
                return "Hey " + name + ", how about we go knock out '" + candidate.questTitle + "'?";
            if (candidate.kind == Kind::Dungeon)
                return "Hey " + name + ", you're the right level for " + candidate.dungeonName + " - wanna run it?";
            if (candidate.kind == Kind::FallbackZone)
                return "Hey " + name + ", let's head to " + candidate.zoneName + " - anything sound good there?";
            return "Hey " + name + ", what do you want to do next?";
        case LineKind::ApprovalAck:   return std::string("Let's do it!");
        case LineKind::Waiting:       return "Hold up " + name + " - I'll wait here.";
        case LineKind::ProximityWarn: return name + ", you coming? I'll wait.";
        case LineKind::IdleNudge:     return name + ", why are we standing around? Pick something.";
        case LineKind::CompleteCheck: return name + ", are we still working on this one, or want to move on?";
        case LineKind::CompleteAck:   return std::string("Nice - that's done. What's next?");
        case LineKind::AbortAck:      return std::string("Can't do that - ") + contextNote + ". Want something else?";
        case LineKind::LevelUp:       return "Nice work, " + name + " - level " + contextNote + "! Onward.";
    }
    return {};
}

// NOTE on locking: callers hold s_mutex while PhraseLine runs. The
// deterministic path returns in microseconds; the generative path may block
// the heartbeat thread for ~1-5s waiting on the gateway HTTP. That window
// also blocks `OnPlayerChat` if a player chat arrives mid-phrasing. v1
// accepts this — heartbeat ticks at 10s, so contention is brief and only
// hits when the player actually says something during the phrasing call.
// Follow-up: capture-then-drop-lock-then-emit pattern, similar to how
// async DB queries are dispatched elsewhere in the module.
// `outLatencyMs` (optional): receives the wall-clock time spent inside the
// generative proposer gateway call, so the caller can record it as the
// `proact_propose` / `proact_nudge` row's `latency_ms` per audit-events.md L54
// ("latency_ms = milliseconds spent inside the LLM call (classifier or
// proposer)"). Set to 0 on every deterministic path (no LLM call ran), and to
// the measured value whenever the gateway call is attempted — including the
// raw-empty / sanitize-empty fallbacks, since the proposer time was still spent
// even when we fall back to the deterministic line. Pass nullptr (the default)
// for the ack lines that don't emit their own audit row.
std::string PhraseLine(uint64_t botGuid, uint64_t playerGuid, LineKind kind,
                       Candidate const& candidate, std::string const& contextNote,
                       uint32_t* outLatencyMs = nullptr)
{
    if (outLatencyMs) *outLatencyMs = 0;

    Player* player = ObjectAccessor::FindPlayer(ObjectGuid(playerGuid));
    Player* bot    = ObjectAccessor::FindPlayer(ObjectGuid(botGuid));
    std::string playerName = player ? std::string(player->GetName()) : std::string();

    std::string deterministic = DeterministicPhrasing(kind, candidate, playerName, contextNote);

    // Generative path is opt-in via a loaded proposer prompt AND gateway being
    // enabled. The default is deterministic — cheap, reproducible, and good
    // enough to smoke-test the loop on game-host.
    if (g_ProactiveProposerPromptText.empty() || !g_GatewayEnable || !bot)
        return deterministic;

    // Build the JSON snapshot per contracts/prompt-contract.md. We send the
    // proposer prompt as the system role and the snapshot as the user role.
    std::ostringstream snap;
    snap << "{\"lineKind\":\"" << LineKindToString(kind) << "\"";
    if (player)
    {
        snap << ",\"player\":{\"name\":\"" << JsonEscape(playerName) << "\""
             << ",\"level\":" << player->GetLevel()
             << ",\"zone\":\"" << JsonEscape(ZoneNameById(player->GetZoneId())) << "\"}";
    }
    snap << ",\"bot\":{\"name\":\"" << JsonEscape(bot->GetName()) << "\"}";
    if (kind == LineKind::Proposal)
    {
        snap << ",\"candidate\":{";
        if (candidate.kind == Kind::Quest)
            snap << "\"kind\":\"quest\",\"questTitle\":\"" << JsonEscape(candidate.questTitle)
                 << "\",\"hasProgress\":" << (candidate.hasProgress ? "true" : "false");
        else if (candidate.kind == Kind::Dungeon)
            snap << "\"kind\":\"dungeon\",\"dungeonName\":\"" << JsonEscape(candidate.dungeonName) << "\"";
        else
            snap << "\"kind\":\"fallbackZone\",\"zoneName\":\"" << JsonEscape(candidate.zoneName) << "\"";
        snap << "}";
    }
    if (!contextNote.empty())
        snap << ",\"context\":{\"note\":\"" << JsonEscape(contextNote) << "\"}";
    snap << "}";

    auto t0 = std::chrono::steady_clock::now();
    std::string raw = QueryGatewayAPIRaw(botGuid, playerGuid,
                                          g_ProactiveProposerPromptText, snap.str());
    auto t1 = std::chrono::steady_clock::now();
    uint32_t latencyMs = static_cast<uint32_t>(
        std::chrono::duration_cast<std::chrono::milliseconds>(t1 - t0).count());
    if (outLatencyMs) *outLatencyMs = latencyMs;

    if (raw.empty())
    {
        LOG_DEBUG("server.loading",
                  "[Ollama Chat Proactive] generative phrasing returned empty (latency={}ms); "
                  "falling back to deterministic", latencyMs);
        return deterministic;
    }

    std::string line = SanitizeGeneratedLine(raw);
    if (line.empty()) return deterministic;
    LOG_DEBUG("server.loading",
              "[Ollama Chat Proactive] generative phrasing ok (kind={}, latency={}ms, chars={})",
              LineKindToString(kind), latencyMs, line.size());
    return line;
}

// Build the JSON snapshot the gateway planner expects. Same shape as the
// proposer-prompt snapshot, plus richer player state (combat, hp%, idleSec)
// and context (recentRejectedCount, sessionAgeSec) so the agent can veto a
// proposal when the situation says "leave them alone."
std::string BuildPlannerSnapshot(Player* player, Player* bot, Candidate const& cand,
                                 CadenceState const& c, uint64_t now)
{
    std::ostringstream snap;
    snap << "{\"lineKind\":\"proposal\"";
    if (player)
    {
        uint64_t idleMs = (c.lastPlayerActivityAtMs > 0 && now > c.lastPlayerActivityAtMs)
                            ? (now - c.lastPlayerActivityAtMs) : 0;
        snap << ",\"player\":{\"name\":\"" << JsonEscape(player->GetName()) << "\""
             << ",\"level\":" << static_cast<unsigned>(player->GetLevel())
             << ",\"zone\":\"" << JsonEscape(ZoneNameById(player->GetZoneId())) << "\""
             << ",\"inCombat\":" << (player->IsInCombat() ? "true" : "false")
             << ",\"hpPct\":" << static_cast<int>(player->GetHealthPct())
             << ",\"idleSec\":" << (idleMs / 1000)
             << "}";
    }
    if (bot)
    {
        snap << ",\"bot\":{\"name\":\"" << JsonEscape(bot->GetName()) << "\"}";
    }
    snap << ",\"candidate\":{";
    if (cand.kind == Kind::Quest)
        snap << "\"kind\":\"quest\",\"questTitle\":\"" << JsonEscape(cand.questTitle)
             << "\",\"hasProgress\":" << (cand.hasProgress ? "true" : "false");
    else if (cand.kind == Kind::Dungeon)
        snap << "\"kind\":\"dungeon\",\"dungeonName\":\"" << JsonEscape(cand.dungeonName) << "\"";
    else
        snap << "\"kind\":\"fallbackZone\",\"zoneName\":\"" << JsonEscape(cand.zoneName) << "\"";
    snap << "}";
    uint64_t sessionAgeSec = (c.sessionStartedAtMs > 0 && now > c.sessionStartedAtMs)
                              ? (now - c.sessionStartedAtMs) / 1000 : 0;
    snap << ",\"context\":{\"recentRejectedCount\":" << c.recentRejectedTargets.size()
         << ",\"sessionAgeSec\":" << sessionAgeSec << "}";
    snap << "}";
    return snap.str();
}

// Phase C — tick-gate snapshot. Player + cadence context only; no candidate
// (the gate runs BEFORE SelectCandidateForPlayer to suppress no-go ticks
// before the more expensive paths fire).
std::string BuildTickGateSnapshot(Player* player, Player* bot, CadenceState const& c, uint64_t now)
{
    std::ostringstream snap;
    snap << "{";
    if (player)
    {
        uint64_t idleMs = (c.lastPlayerActivityAtMs > 0 && now > c.lastPlayerActivityAtMs)
                            ? (now - c.lastPlayerActivityAtMs) : 0;
        snap << "\"player\":{\"name\":\"" << JsonEscape(player->GetName()) << "\""
             << ",\"level\":" << static_cast<unsigned>(player->GetLevel())
             << ",\"zone\":\"" << JsonEscape(ZoneNameById(player->GetZoneId())) << "\""
             << ",\"inCombat\":" << (player->IsInCombat() ? "true" : "false")
             << ",\"hpPct\":" << static_cast<int>(player->GetHealthPct())
             << ",\"idleSec\":" << (idleMs / 1000)
             << "}";
    }
    if (bot)
    {
        snap << ",\"bot\":{\"name\":\"" << JsonEscape(bot->GetName()) << "\"}";
    }
    uint64_t sessionAgeSec = (c.sessionStartedAtMs > 0 && now > c.sessionStartedAtMs)
                              ? (now - c.sessionStartedAtMs) / 1000 : 0;
    uint64_t lastProposalAgoSec = (c.lastProposalAtMs > 0 && now > c.lastProposalAtMs)
                                    ? (now - c.lastProposalAtMs) / 1000 : 0;
    snap << ",\"context\":{\"recentRejectedCount\":" << c.recentRejectedTargets.size()
         << ",\"sessionAgeSec\":" << sessionAgeSec
         << ",\"lastProposalAgoSec\":" << lastProposalAgoSec << "}";
    snap << "}";
    return snap.str();
}

// Build the rejection-key for a candidate (used to skip the immediately-next
// proposal per FR-005). Three non-colliding namespaces, distinguished by the
// two bits above bit 31 (questId / mapId / zoneId each fit in the low 32):
//   - Quest:        bare questId          (high bits clear)
//   - Dungeon:      (1 << 32) | mapId     (bit 32 set)
//   - FallbackZone: (2 << 32) | zoneId    (bit 33 set)
// FallbackZone is keyed so a rejected/ignored soft zone-suggestion ("let's head
// to <zone>") is remembered like the two primary categories. Before this it
// returned 0 → RememberRejection no-op'd → and SelectCandidateForPlayer's rank-4
// (which never consulted recentRejectedTargets) re-offered the identical zone
// every ProposalCooldownSec. A zoneId of 0 (unknown / transient area) yields no
// key — the generic "this zone" fallback isn't worth pinning, and a 0 key would
// be indistinguishable from "no candidate" in RecentlyRejected.
uint64_t MakeRejectionKey(Candidate const& c)
{
    if (c.kind == Kind::Quest)        return static_cast<uint64_t>(c.questId);
    if (c.kind == Kind::Dungeon)      return (uint64_t{1} << 32) | static_cast<uint64_t>(c.mapId);
    if (c.kind == Kind::FallbackZone) return c.zoneId != 0
                                          ? ((uint64_t{2} << 32) | static_cast<uint64_t>(c.zoneId))
                                          : 0;
    return 0;
}

bool RecentlyRejected(CadenceState const& cadence, uint64_t key)
{
    if (key == 0) return false;
    for (uint64_t rk : cadence.recentRejectedTargets)
        if (rk == key) return true;
    return false;
}

// Look up a zone's English name via DBC; empty on miss.
std::string ZoneNameById(uint32_t zoneId)
{
    if (zoneId == 0) return {};
    if (auto const* z = sAreaTableStore.LookupEntry(zoneId))
    {
        char const* nm = z->area_name[LocaleConstant::LOCALE_enUS];
        if (nm && *nm) return std::string(nm);
    }
    return {};
}

// ---------------------------------------------------------------------------
// Candidate selection — the deterministic ranked query.
// Rank 1: quests already in the player's log (preferring same-zone first).
// Rank 2: in-zone accept-eligible quests via quest_template scan.
// Rank 3: level-appropriate dungeons from the hand-curated table.
// Rank 4: fallback zone suggestion ("let's head to <zone>").
// FR-005: skip targets in cadence.recentRejectedTargets in the immediately
// next selection.
// ---------------------------------------------------------------------------
Candidate SelectCandidateForPlayer(Player* player, CadenceState const& cadence)
{
    Candidate out;
    if (!player) return out;

    uint32_t playerZone = player->GetZoneId();
    uint32_t playerMap  = player->GetMapId();
    uint32_t playerLvl  = player->GetLevel();

    // -----------------------------------------------------------------------
    // Rank 1: quests in the player's log. Prefer those whose ZoneOrSort
    // matches the current zone; fall back to any non-rejected log quest.
    // -----------------------------------------------------------------------
    uint32_t fallbackLogQuestId = 0;
    for (uint8_t slot = 0; slot < MAX_QUEST_LOG_SIZE; ++slot)
    {
        uint32_t qid = player->GetQuestSlotQuestId(slot);
        if (!qid) continue;
        if (RecentlyRejected(cadence, static_cast<uint64_t>(qid))) continue;

        Quest const* qt = sObjectMgr->GetQuestTemplate(qid);
        if (!qt) continue;

        // Zone match preferred. Quest_template.ZoneOrSort > 0 means it's a zone
        // id; negative values are quest categories.
        int32_t zoneOrSort = qt->GetZoneOrSort();
        if (zoneOrSort > 0 && static_cast<uint32_t>(zoneOrSort) == playerZone)
        {
            out.kind        = Kind::Quest;
            out.questId     = qid;
            out.questTitle  = qt->GetTitle();
            out.hasProgress = (player->GetQuestStatus(qid) == QUEST_STATUS_INCOMPLETE
                               || player->GetQuestStatus(qid) == QUEST_STATUS_COMPLETE);
            return out;
        }
        if (fallbackLogQuestId == 0) fallbackLogQuestId = qid;
    }
    if (fallbackLogQuestId != 0)
    {
        Quest const* qt = sObjectMgr->GetQuestTemplate(fallbackLogQuestId);
        if (qt)
        {
            out.kind        = Kind::Quest;
            out.questId     = fallbackLogQuestId;
            out.questTitle  = qt->GetTitle();
            out.hasProgress = (player->GetQuestStatus(fallbackLogQuestId) == QUEST_STATUS_INCOMPLETE
                               || player->GetQuestStatus(fallbackLogQuestId) == QUEST_STATUS_COMPLETE);
            return out;
        }
    }

    // -----------------------------------------------------------------------
    // Rank 2: in-zone accept-eligible quests (quest_template scan).
    // Bounded by g_ProactiveMaxQuestRankSearch. Filter via Player::CanTakeQuest
    // so we never propose something the player can't actually pick up.
    // -----------------------------------------------------------------------
    if (playerZone != 0 && g_ProactiveMaxQuestRankSearch > 0)
    {
        uint32_t lvlHi = playerLvl + 5;
        uint32_t lvlLo = playerLvl > 10 ? playerLvl - 10 : 0;
        // AC modern DB API: Query(string). Params are integers from internal
        // sources, so direct interpolation is safe (no untrusted input).
        std::string sql = fmt::format(
            "SELECT ID FROM quest_template "
            "WHERE QuestSortID = {} AND MinLevel <= {} "
            "AND (QuestLevel = -1 OR QuestLevel >= {}) "
            "ORDER BY MinLevel ASC, ID ASC LIMIT {}",
            playerZone, lvlHi, lvlLo, g_ProactiveMaxQuestRankSearch);
        QueryResult qres = WorldDatabase.Query(sql);
        if (qres)
        {
            do
            {
                Field* fields = qres->Fetch();
                if (!fields) continue;
                uint32_t qid = fields[0].Get<uint32>();
                if (RecentlyRejected(cadence, static_cast<uint64_t>(qid))) continue;
                Quest const* q = sObjectMgr->GetQuestTemplate(qid);
                if (!q) continue;
                if (player->GetQuestStatus(qid) != QUEST_STATUS_NONE) continue;
                if (!player->CanTakeQuest(q, /*msg=*/false)) continue;

                out.kind        = Kind::Quest;
                out.questId     = qid;
                out.questTitle  = q->GetTitle();
                out.hasProgress = false;
                return out;
            } while (qres->NextRow());
        }
    }

    // -----------------------------------------------------------------------
    // Rank 3: dungeons from the hand-curated table. Filter by level bracket,
    // continent (same as the player as a v1 simplification), faction-side
    // entrance flag, and the operator whitelist if non-empty.
    // -----------------------------------------------------------------------
    std::unordered_set<uint32_t> whitelist = ParseDungeonWhitelist(g_ProactiveDungeonCandidates);
    uint8_t playerFactionFlag = (player->GetTeamId() == TEAM_ALLIANCE) ? 1 : 2;

    for (DungeonRow const& row : kDungeonTable)
    {
        if (playerLvl + 3 < row.minLevel) continue;   // far too low
        if (playerLvl     > row.maxLevel + 5) continue; // outgrown
        if (row.factionFlag != 0 && row.factionFlag != playerFactionFlag) continue;
        if (row.continentMapId != playerMap) continue;
        if (!whitelist.empty() && whitelist.count(row.mapId) == 0) continue;
        if (RecentlyRejected(cadence, (uint64_t{1} << 32) | static_cast<uint64_t>(row.mapId))) continue;

        out.kind          = Kind::Dungeon;
        out.mapId         = row.mapId;
        out.continentMapId = row.continentMapId;
        out.entranceX     = row.entranceX;
        out.entranceY     = row.entranceY;
        out.entranceZ     = row.entranceZ;
        out.dungeonName   = row.name;
        return out;
    }

    // -----------------------------------------------------------------------
    // Rank 4: fallback zone suggestion (soft commit, no specific target).
    //
    // FR-005 anti-repeat, extended to the fallback rank. Ranks 1 and 3 each
    // skip a recently-rejected target (RecentlyRejected above), but rank 4 used
    // to return the current zone's soft suggestion UNCONDITIONALLY. So a player
    // who rejected (or silently let expire) "let's head to <zone>" got the
    // identical line re-proposed every ProposalCooldownSec: the fallback's
    // MakeRejectionKey was 0 (no-op remember) AND this rank never consulted the
    // deque. Mirror the other ranks — if THIS zone's fallback sits in
    // recentRejectedTargets, return Kind::None so EvaluateTickLocked stays quiet
    // this tick rather than re-offering the one option the player just declined.
    // There is genuinely nothing fresh to propose (no log/in-zone quest, no
    // level-appropriate dungeon on-continent, and the generic zone nudge was
    // rejected). The suppression is per-zone and recency-bounded by the 3-deep
    // deque: walking into a new zone (different zoneId → fresh key), picking up
    // a quest (rank 1 wins), or the key aging out all re-enable a proposal.
    // Same "extend rejection memory to a rank that lacked it" family as PR #146
    // (quest-log re-propose) / #149 (cancelled-aborted); rank 4 was the last gap.
    if (playerZone != 0
        && RecentlyRejected(cadence, (uint64_t{2} << 32) | static_cast<uint64_t>(playerZone)))
        return out;   // out.kind == Kind::None — nothing to propose this tick

    out.kind     = Kind::FallbackZone;
    out.zoneId   = playerZone;
    out.zoneName = ZoneNameById(playerZone);
    if (out.zoneName.empty()) out.zoneName = "this zone";
    return out;
}

// ---------------------------------------------------------------------------
// Proposal lifecycle primitives. Caller MUST hold s_mutex.
// ---------------------------------------------------------------------------
Proposal* GetOpenProposalForPlayer(uint64_t playerGuid)
{
    for (auto& kv : s_proposalsById)
    {
        if (kv.second.playerGuid == playerGuid && kv.second.state == State::Open)
            return &kv.second;
    }
    return nullptr;
}

ActivityPlan* GetExecutingPlanForPlayer(uint64_t playerGuid)
{
    for (auto& kv : s_plansByProposalId)
    {
        if (kv.second.state == State::Executing)
        {
            auto pit = s_proposalsById.find(kv.first);
            if (pit != s_proposalsById.end() && pit->second.playerGuid == playerGuid)
                return &kv.second;
        }
    }
    return nullptr;
}

uint64_t OpenNewProposal(uint64_t playerGuid, uint64_t botGuid, Candidate const& cand)
{
    uint64_t id = s_nextProposalId.fetch_add(1);
    Proposal p;
    p.proposalId  = id;
    p.playerGuid  = playerGuid;
    p.botGuid     = botGuid;
    p.candidate   = cand;
    p.state       = State::Open;
    p.createdAtMs = NowMs();
    p.expiryMs    = p.createdAtMs + static_cast<uint64_t>(g_ProactiveProposalExpirySec) * 1000;
    if (Player* opener = ObjectAccessor::FindPlayer(ObjectGuid(playerGuid)))
        p.originMapId = opener->GetMapId();
    s_proposalsById[id] = p;

    CadenceState& c = GetCadence(playerGuid, botGuid);
    c.currentProposalId = id;
    c.lastProposalAtMs  = p.createdAtMs;
    return id;
}

void TransitionProposal(uint64_t proposalId, State newState)
{
    auto it = s_proposalsById.find(proposalId);
    if (it == s_proposalsById.end()) return;
    it->second.state        = newState;
    it->second.answeredAtMs = NowMs();

    CadenceState& c = GetCadence(it->second.playerGuid, it->second.botGuid);
    if (newState == State::Open || newState == State::Approved)
        c.currentProposalId = proposalId;
    else
        c.currentProposalId = 0;
}

void StartActivityPlan(Proposal& prop)
{
    ActivityPlan plan;
    plan.proposalId     = prop.proposalId;
    plan.state          = State::Executing;
    plan.startedAtMs    = NowMs();
    plan.lastProgressAtMs = plan.startedAtMs;
    s_plansByProposalId[prop.proposalId] = plan;

    CadenceState& c = GetCadence(prop.playerGuid, prop.botGuid);
    c.currentActivityPlanId = prop.proposalId;

    // Dispatch existing playerbot verbs. For v1 we do the minimum: tell the
    // bot to follow the player so the player can lead the bot to the quest
    // giver / instance entrance. The "accept *" / "talk *" dispatch on
    // arriving at the quest giver is handled by the existing playerbot
    // strategy stack — no new MCP verb introduced (FR-022).
    Player* player = ObjectAccessor::FindPlayer(ObjectGuid(prop.playerGuid));
    Player* bot    = ObjectAccessor::FindPlayer(ObjectGuid(prop.botGuid));
    if (player && bot)
    {
        PlayerbotAI* botAI = PlayerbotsMgr::instance().GetPlayerbotAI(bot);
        if (botAI)
        {
            // The existing leader_command_all path does "follow <human name>"
            // via the leader's master session. Here we issue it locally so
            // the bot follows the player. v1: simple HandleCommand("follow")
            // — the bot already has the human as master via auto-claim.
            std::string cmd = "follow";
            botAI->HandleCommand(CHAT_MSG_WHISPER, cmd, player);
            plan.commandsDispatched.push_back(cmd);
            s_plansByProposalId[prop.proposalId] = plan;
        }
    }
}

void AbortActivityPlan(uint64_t proposalId, std::string const& blockerText)
{
    auto pit = s_plansByProposalId.find(proposalId);
    if (pit == s_plansByProposalId.end()) return;
    pit->second.state       = State::Aborted;
    pit->second.blockerText = blockerText;

    auto rit = s_proposalsById.find(proposalId);
    if (rit != s_proposalsById.end())
    {
        rit->second.state = State::Aborted;
        CadenceState& c = GetCadence(rit->second.playerGuid, rit->second.botGuid);
        c.currentActivityPlanId = 0;
        c.currentProposalId = 0;
    }
}

void CancelOpenProposalIfAny(uint64_t playerGuid, char const* /*reason*/)
{
    Proposal* p = GetOpenProposalForPlayer(playerGuid);
    if (!p) return;
    p->state        = State::Cancelled;
    p->answeredAtMs = NowMs();
    CadenceState& c = GetCadence(p->playerGuid, p->botGuid);
    c.currentProposalId = 0;
}

void RememberRejection(CadenceState& c, Candidate const& cand)
{
    uint64_t key = MakeRejectionKey(cand);
    if (key == 0) return;
    c.recentRejectedTargets.push_back(key);
    while (c.recentRejectedTargets.size() > 3) c.recentRejectedTargets.pop_front();
}

// ---------------------------------------------------------------------------
// Retention sweep — drop terminal proposals (and their one-to-one ActivityPlan)
// from the in-memory maps after a short grace window. Caller MUST hold s_mutex.
//
// data-model.md pins the contract: "Once state ∈ {rejected, expired, cancelled,
// completed, aborted}, the Proposal is terminal and is dropped from the
// in-memory table after a short retention window (kept around for ~60s only so a
// slow audit-write doesn't lose context)." Nothing was ever erasing
// s_proposalsById / s_plansByProposalId, so every proposal ever opened lingered
// for the whole worldserver uptime — an unbounded memory leak AND a steadily
// growing cost on the three per-heartbeat O(N) scans that walk these maps:
// GetOpenProposalForPlayer, GetExecutingPlanForPlayer, and ElectDesignatedVoice
// stickiness. The live auto-claim propose→expire loop manufactures one terminal
// proposal per pair every cooldown, so this grows without bound on a long-lived
// worldserver even with zero humans present.
//
// The terminal timestamp is stamped lazily HERE on first sight rather than at
// each of the scattered terminal-transition sites (TransitionProposal,
// CancelOpenProposalIfAny, AbortActivityPlan, the Done/zoneOut/questTurnIn
// completion paths, and OnPlayerLogout's four cleanup loops). One choke point =
// no risk of a missed site, and `answeredAtMs` (which several terminal paths
// never set) is left to its own meaning. A terminal proposal is never read again
// by any runtime path — every lookup matches only Open/Approved/Executing, and
// c.currentProposalId / c.currentActivityPlanId are zeroed on every terminal
// transition — so dropping it after the grace window is side-effect free.
// ---------------------------------------------------------------------------
constexpr uint64_t kTerminalRetentionMs = 60000;   // ~60s, per data-model.md

bool IsTerminalState(State s)
{
    return s == State::Rejected || s == State::Expired || s == State::Cancelled
        || s == State::Completed || s == State::Aborted;
}

void ReapTerminalState(uint64_t now)
{
    for (auto it = s_proposalsById.begin(); it != s_proposalsById.end(); )
    {
        Proposal& p = it->second;
        if (!IsTerminalState(p.state))
        {
            ++it;
            continue;
        }
        if (p.terminalAtMs == 0)
        {
            // First heartbeat to observe this proposal terminal — start its
            // retention clock; do NOT reap yet (preserve the ~60s grace).
            p.terminalAtMs = now;
            ++it;
            continue;
        }
        if (now - p.terminalAtMs < kTerminalRetentionMs)
        {
            ++it;
            continue;
        }
        // Grace elapsed — drop the proposal and its ActivityPlan. The plan is
        // created only from an approved proposal and is driven terminal in
        // lockstep with it (AbortActivityPlan / the Completed paths flip both),
        // so erasing it alongside is safe; the erase is a no-op when no plan was
        // ever started for this proposal.
        s_plansByProposalId.erase(it->first);
        it = s_proposalsById.erase(it);
    }
}

// ---------------------------------------------------------------------------
// Proximity check (US2 / R-7). Returns one of four states. Threshold is
// `g_ProactiveProximityYards` (3D). Different-zone/map cause an abandon of
// any in-flight Activity Plan rather than a wait.
// ---------------------------------------------------------------------------
enum class ProximityState : uint8_t
{
    WithinRange = 0,
    TooFar,
    DifferentZone,
    DifferentMap,
};

ProximityState CheckProximity(Player* player, Player* bot)
{
    if (!player || !bot) return ProximityState::WithinRange;
    if (player->GetMapId() != bot->GetMapId()) return ProximityState::DifferentMap;
    if (player->GetZoneId() != bot->GetZoneId()) return ProximityState::DifferentZone;
    float dx = player->GetPositionX() - bot->GetPositionX();
    float dy = player->GetPositionY() - bot->GetPositionY();
    float dz = player->GetPositionZ() - bot->GetPositionZ();
    float dist = std::sqrt(dx * dx + dy * dy + dz * dz);
    if (dist > g_ProactiveProximityYards) return ProximityState::TooFar;
    return ProximityState::WithinRange;
}

// Track player activity for US3 idle-nudge eligibility. Movement is detected
// by sampling position and comparing against the previous tick's snapshot.
// Quest/zone/chat events tap their own activity update inline.
void NoteMovementIfAny(CadenceState& c, Player* player)
{
    if (!player) return;
    float px = player->GetPositionX();
    float py = player->GetPositionY();
    float pz = player->GetPositionZ();
    // First sample: just record the position, don't fire activity (avoids
    // spurious "moved" on session start).
    if (c.lastPlayerX == 0.0f && c.lastPlayerY == 0.0f && c.lastPlayerZ == 0.0f)
    {
        c.lastPlayerX = px;
        c.lastPlayerY = py;
        c.lastPlayerZ = pz;
        return;
    }
    float dx = px - c.lastPlayerX;
    float dy = py - c.lastPlayerY;
    float dz = pz - c.lastPlayerZ;
    if (dx * dx + dy * dy + dz * dz > 1.0f)   // moved at least 1 yard total
    {
        c.lastPlayerActivityAtMs = NowMs();
        c.lastPlayerX = px;
        c.lastPlayerY = py;
        c.lastPlayerZ = pz;
        c.ignoredNudgeCount = 0;   // FR-011 — player engaged after a nudge
    }
}

// Idle-nudge eligibility (FR-010, FR-011). All inputs from cadence except
// combat status from Player*.
bool IsPlayerIdleForNudge(Player* player, CadenceState const& c, uint64_t now)
{
    if (!player) return false;
    if (player->IsInCombat()) return false;
    if (g_ProactiveIdleNudgeMaxPerSession == 0) return false;
    if (c.ignoredNudgeCount >= g_ProactiveIdleNudgeMaxPerSession) return false;
    if (c.currentProposalId != 0) return false;
    if (c.currentActivityPlanId != 0) return false;
    if (c.lastPlayerActivityAtMs == 0) return false;   // never seen player move

    uint64_t idleMs = static_cast<uint64_t>(g_ProactiveIdleThresholdSec) * 1000;
    if (now - c.lastPlayerActivityAtMs < idleMs) return false;

    if (c.lastIdleNudgeAtMs != 0)
    {
        uint64_t cdMs = static_cast<uint64_t>(g_ProactiveIdleNudgeCooldownSec) * 1000;
        if (now - c.lastIdleNudgeAtMs < cdMs) return false;
    }
    return true;
}

// ---------------------------------------------------------------------------
// EvaluateTick — the per-(bot, player) heartbeat body.
// ---------------------------------------------------------------------------
void EvaluateTickLocked(uint64_t botGuid, uint64_t playerGuid)
{
    Player* player = ObjectAccessor::FindPlayer(ObjectGuid(playerGuid));
    if (!player || !player->IsInWorld()) return;
    Player* bot = ObjectAccessor::FindPlayer(ObjectGuid(botGuid));

    CadenceState& c = GetCadence(playerGuid, botGuid);
    uint64_t now = NowMs();

    // Inherit session-start anchor from the (playerGuid, 0) sentinel seeded
    // by OnPlayerLogin. Without this hand-off, the freshly created
    // (player, voice) row stays at sessionStartedAtMs == 0 forever, the
    // first-proposal-delay gate at section 4 always returns early, and no
    // proposal ever fires — which is why mod_ollama_chat_gateway_audit has
    // been empty since the proactive subsystem shipped. Also inherit
    // lastPlayerActivityAtMs so the idle-nudge gate at section 3a has a
    // baseline — without it, a player who logs in and never moves/chats/
    // quests/zones leaves lastPlayerActivityAtMs at 0 indefinitely, the
    // "never seen player move" sentinel fires every tick in
    // IsPlayerIdleForNudge, and FR-010's "stationary for IdleThresholdSec"
    // path is dead code for the AFK-on-login case.
    if (c.sessionStartedAtMs == 0)
    {
        auto sit = s_cadenceByKey.find(CadenceKey{playerGuid, 0});
        if (sit != s_cadenceByKey.end() && sit->second.sessionStartedAtMs != 0)
        {
            c.sessionStartedAtMs = sit->second.sessionStartedAtMs;
            if (c.lastPlayerActivityAtMs == 0)
                c.lastPlayerActivityAtMs = sit->second.lastPlayerActivityAtMs;
        }
    }

    // Sample player movement for idle-nudge tracking (US3).
    NoteMovementIfAny(c, player);

    // 1) Expire any open proposal whose timer ran out.
    if (Proposal* p = GetOpenProposalForPlayer(playerGuid); p)
    {
        if (now > p->expiryMs)
        {
            uint64_t pid = p->proposalId;
            // Attribute the expiry to the proposal's OWNER, not the elected
            // voice driving this tick. The two are the SAME bot in the common
            // case — ElectDesignatedVoice's stickiness pins voice == owner while
            // a proposal is in flight (PR #211) — but FR-013 (spec.md:106) names
            // exactly one divergence: re-election fires "when ... the owner
            // leaves range", and a bot leaving the player's map does NOT cancel
            // its still-Open proposal (only the PLAYER changing maps does, via
            // OnZoneChange). So a lower-GUID leader bot can become this player's
            // voice while the absent owner's proposal sits Open until its 10-min
            // expiry — and that expiry then lands HERE under the new voice. That
            // is precisely the mis-attribution FR-013 forbids: a voice handoff
            // "cannot ... mis-attribute its proximity/abort audit rows to a
            // different bot." Three steps were keyed off the voice `botGuid`/`c`:
            //   (a) the proact_abort `expired` row's bot_guid — but
            //       audit-events.md L67 defines bot_guid as "the proposing ...
            //       bot", and the operator's per-bot propose-vs-terminal
            //       reconciliation needs the expired row to carry the SAME
            //       bot_guid as its matching proact_propose `open` (the owner's),
            //       not an unrelated newcomer's;
            //   (b) consecutiveExpiredProposals — FR-023 (spec.md:122) pins this
            //       "per (player, bot) Cadence row, ... increment on
            //       Proposal::state transition open → expired"; the (player, bot)
            //       row is the OWNER's, so bumping the voice's row both lets the
            //       owner dodge its own self-silence cap and pushes an innocent
            //       newcomer toward a silence it never earned;
            //   (c) RememberRejection — the FR-005 anti-repeat deque is the
            //       owner's; recording the just-expired candidate there is what
            //       stops the owner re-offering it the instant it returns to map.
            // TransitionProposal already keys off the proposal's own botGuid (it
            // zeroes the OWNER's currentProposalId), so it was the lone correctly-
            // attributed step — mirror it for the other three by resolving the
            // owner cadence `oc` from p->botGuid. In the common voice == owner
            // case `oc` IS `c` (GetCadence returns the same row), so this is a
            // no-op there. Same voice/owner-coherence family as PR #211; same
            // audit operator-query convention as PR #195/#200/#203.
            uint64_t ownerGuid = p->botGuid;
            CadenceState& oc = GetCadence(playerGuid, ownerGuid);
            // Treat silent-ignore as a soft-rejection so the next cycle does
            // not re-pick the identical candidate from the player's quest log
            // (FR-005 spirit extended to the no-answer path). Without this,
            // SelectCandidate keeps surfacing the same log quest every cycle.
            RememberRejection(oc, p->candidate);
            // FR-023 — count consecutive silent-ignores so the propose loop
            // self-silences when the player has clearly stopped listening
            // (auto-claim bots, AFK, etc.). Reset by any chat/quest/zone
            // engagement signal (see OnPlayerChat / OnQuestComplete /
            // OnZoneChange handlers); not bumped on explicit reject/done.
            oc.consecutiveExpiredProposals += 1;
            TransitionProposal(pid, State::Expired);
            EmitAudit(playerGuid, ownerGuid, kSrcAbort, kRsnExpired, "", 0);
        }
    }

    // 2) Proximity + completion-timeout safety net for any executing plan.
    if (ActivityPlan* plan = GetExecutingPlanForPlayer(playerGuid); plan)
    {
        // 2.0) Owner-left-range handoff (FR-013). An executing plan is owned by
        // the bot stored on its Proposal (s_proposalsById[plan->proposalId]
        // .botGuid) — the bot that's actually following/travelling the player on
        // the approved activity. ElectDesignatedVoice pins the voice to that
        // owner for as long as the owner stays eligible (in world + on the
        // player's map + leader-capable — the #211 stickiness, tier 1), so a
        // divergence here (voice botGuid != ownerGuid) can mean only one thing:
        // the owner went INELIGIBLE — it left the player's map, dropped out of
        // world, or was de-listed as a leader — and a different in-range leader
        // bot is now the elected voice driving this tick. Running the rest of
        // section 2 under that bystander voice is exactly what FR-013 (spec.md:
        // 106) forbids: "a mid-activity voice handoff cannot strand the owning
        // bot's follow/stay state or mis-attribute its proximity/abort audit
        // rows to a different bot." Concretely, with the voice WithinRange the
        // section-2a WithinRange branch would silently re-dispatch `follow` to
        // the bystander and limp the plan along under it until the ~30-min
        // completion timeout, stamping every proximity-nudge / complete-check /
        // abort row with the wrong bot_guid (audit-events.md L67 keys bot_guid
        // to the proposing/owning bot so the operator's per-bot propose-vs-
        // terminal reconciliation lines up), and the owner's leaked `stay` would
        // never be released. #255 closed this same divergence for the Open-
        // proposal expiry path (section 1); this is its executing-plan twin,
        // deferred there until PR #251 landed section 2a. Per FR-021 an
        // abandoned activity returns to the proposal cycle, so: release the
        // owner's stranded stay (mirrors section 2c's leaked-stay heal), abort
        // the plan attributed to the OWNER (AbortActivityPlan already clears the
        // owner's cadence via the proposal's botGuid), and let section 3 open a
        // fresh proposal under the new voice this very heartbeat (hasExecuting
        // is recomputed below, after the abort). In the common voice == owner
        // case this guard is inert and section 2 runs byte-identically.
        auto ownerIt = s_proposalsById.find(plan->proposalId);
        uint64_t ownerGuid = (ownerIt != s_proposalsById.end())
                                 ? ownerIt->second.botGuid : botGuid;
        if (ownerGuid != botGuid)
        {
            CadenceState& oc = GetCadence(playerGuid, ownerGuid);
            if (oc.inProximityWaiting)
            {
                if (Player* ownerBot = ObjectAccessor::FindPlayer(ObjectGuid(ownerGuid)))
                    if (PlayerbotAI* ownerAI = PlayerbotsMgr::instance().GetPlayerbotAI(ownerBot))
                        ownerAI->HandleCommand(CHAT_MSG_WHISPER, "follow", player);
                oc.inProximityWaiting = false;
            }
            AbortActivityPlan(plan->proposalId, "owner left range");
            EmitAudit(playerGuid, ownerGuid, kSrcAbort, kRsnOwnerLeftRange, "", 0);
        }
        // 2a) Proximity guardrails (US2 — T043/T044/T045/T046). Gated on
        // ownerGuid == botGuid so the voice-driven proximity machinery never
        // runs for a plan whose owner has left range (handled above); after the
        // 2.0 abort GetExecutingPlanForPlayer below also returns null, so 2b
        // self-skips too.
        else if (bot)
        {
            ProximityState ps = CheckProximity(player, bot);
            // Election invariant — this 2a block runs only under ownerGuid ==
            // botGuid (the `else` above), so `bot` is BOTH the elected voice and
            // the plan owner. ElectDesignatedVoice returns only a bot on the
            // player's CURRENT map: every return path clears its `eligible()`
            // map-equality filter (`bot->GetMapId() == playerMap`), and Heartbeat
            // elects the voice and calls EvaluateTickLocked in the SAME synchronous
            // world-thread tick (no map transition runs in between). So
            // CheckProximity cannot report DifferentMap for the voice here — both
            // DifferentMap consumers below (the enteredApprovedDungeon suppression
            // from PR #298 and the `playerChangedMap` disjunct) are a DORMANT
            // defensive backstop for the elected voice, not the reached path. The
            // real dungeon entry->zoneOut lifecycle AND the cross-map open-proposal
            // abort are both driven by OnZoneChange (a PlayerScript map-change hook,
            // NOT voice-gated), which is why #298's dungeon-arrival suppression is a
            // no-op belt over OnZoneChange's suspenders rather than dead-on-arrival.
            // Retained deliberately — this arm re-arms the instant the voice can
            // diverge from the player's map (e.g. a travel-to-coord dispatch
            // replacing v1's `follow`); do NOT delete it on the strength of this note.
            // Dungeon-arrival exception (FR-021): when the player has zoned INTO
            // the approved dungeon's own instance map, CheckProximity reports
            // DifferentMap only because the bot — v1 dispatches `follow`, never a
            // travel-to-coord — is still on the outside continent. That is ARRIVAL
            // at the approved destination, NOT a travel-abandonment. OnZoneChange
            // already owns the in-instance lifecycle: it sets plan->enteredCandidateMap
            // on this same entry and emits the proact_done "zoneOut" completion when
            // the player later leaves the instance. Aborting here would (a) mis-record
            // a genuine dungeon run as a `playerChangedMap` abandonment and (b) make
            // that enteredCandidateMap->zoneOut completion path unreachable for EVERY
            // real run — GetExecutingPlanForPlayer returns null the instant we abort,
            // so the later zone-out can never complete the plan (the very path the
            // enteredCandidateMap flag exists to gate). So when the player is currently
            // inside the approved instance, suppress the abort and let the plan ride
            // Executing — section 2b's ActivityCompleteTimeoutSec net still bounds it
            // if the player never exits. Resolve via find() (never operator[]) for the
            // same phantom-insert guard as the proximity-warn / timeout siblings; a
            // missing proposal leaves the flag false and falls through to the abort.
            bool enteredApprovedDungeon = false;
            if (ps == ProximityState::DifferentMap && plan->enteredCandidateMap)
            {
                auto dit = s_proposalsById.find(plan->proposalId);
                if (dit != s_proposalsById.end()
                    && dit->second.candidate.kind == Kind::Dungeon
                    && dit->second.candidate.mapId == player->GetMapId())
                    enteredApprovedDungeon = true;
            }
            if (enteredApprovedDungeon)
            {
                // Player is inside the approved instance — not abandoning. Keep the
                // plan Executing; OnZoneChange completes it on zone-out, the section
                // 2b timeout aborts it otherwise. No abort, no audit row.
            }
            else if (ps == ProximityState::DifferentMap || ps == ProximityState::DifferentZone)
            {
                // Player zoned out while the bot was travelling toward the
                // approved destination (FR-009) — abandon travel; the next
                // heartbeat opens a fresh proposal for the new context.
                AbortActivityPlan(plan->proposalId, "player changed zone/map");
                // Distinguish a cross-MAP abandonment (hearth / portal /
                // zeppelin / instance entry — a different continent or
                // instance map) from a cross-ZONE one (a zone-line walk on the
                // SAME map). The audit table stores only request_text's LENGTH
                // (request_chars), so the disposition is encoded by label
                // length: "playerChangedMap"=16 vs "playerChangedZone"=17. The
                // OnZoneChange Open-proposal cancel path already emits
                // `playerChangedMap` for the cross-map case (see OnZoneChange
                // below), and the operator-query convention PRs #155/#177
                // established filters cross-map aborts via request_chars=16.
                // This executing-plan abort predates the `playerChangedMap`
                // label (introduced in PR #155 for the Open-proposal path
                // ONLY) and has emitted `playerChangedZone` for BOTH proximity
                // states ever since — so every travel abandonment caused by a
                // continent hearth was indistinguishable from a same-continent
                // zone walk, and the request_chars=16 cross-map query returned
                // zero executing-plan rows. audit-events.md's column-semantics
                // list already names `playerChangedMap` and `playerChangedZone`
                // as distinct pure-state-transition reasons; emit the matching
                // one so the C++ lines up with the contract. Same "this site
                // silently disagrees with the audit-events.md operator-query
                // convention" family as PR #163 / #191 / #195 / #200.
                char const* reason = (ps == ProximityState::DifferentMap)
                                         ? kRsnPlayerChangedMap : kRsnPlayerChangedZone;
                EmitAudit(playerGuid, botGuid, kSrcAbort, reason, "", 0);
                // Fall through — there's no executing plan now.
            }
            else if (ps == ProximityState::TooFar)
            {
                if (!c.inProximityWaiting)
                {
                    PlayerbotAI* botAI = PlayerbotsMgr::instance().GetPlayerbotAI(bot);
                    if (botAI) botAI->HandleCommand(CHAT_MSG_WHISPER, "stay", player);
                    c.inProximityWaiting = true;
                }
                uint64_t cdMs = static_cast<uint64_t>(g_ProactiveProximityWarnCooldownSec) * 1000;
                if (c.lastProximityWarnAtMs == 0 || now - c.lastProximityWarnAtMs >= cdMs)
                {
                    // Resolve the proposal with find(), NOT operator[]. The plan's
                    // proposal is present by the data-model invariant — GetExecuting-
                    // PlanForPlayer only returns a plan whose proposal it located in
                    // s_proposalsById — but operator[] would default-INSERT a phantom
                    // Proposal (leaking a map entry that ReapTerminalState never keys,
                    // and phrasing a garbage ProximityWarn off a default candidate) the
                    // moment that invariant is ever broken by a mid-tick erase. Mirror
                    // the safe sibling at the timeout-abort path below; if the proposal
                    // is somehow gone, skip the proximity warn this tick.
                    auto pit = s_proposalsById.find(plan->proposalId);
                    if (pit != s_proposalsById.end())
                    {
                        Candidate const& cand = pit->second.candidate;
                        uint32_t phraseLatencyMs = 0;
                        std::string text = PhraseLine(botGuid, playerGuid, LineKind::ProximityWarn, cand, "", &phraseLatencyMs);
                        if (EmitProactiveLine(playerGuid, botGuid, LineKind::ProximityWarn, text))
                        {
                            c.lastProximityWarnAtMs = now;
                            EmitAudit(playerGuid, botGuid, kSrcNudge, kRsnProximity, text, phraseLatencyMs);
                        }
                    }
                }
            }
            else  // WithinRange
            {
                PlayerbotAI* botAI = PlayerbotsMgr::instance().GetPlayerbotAI(bot);
                if (c.inProximityWaiting)
                {
                    // Player closed the gap — resume travel silently. No chat
                    // line; the resumed movement is the signal.
                    if (botAI) botAI->HandleCommand(CHAT_MSG_WHISPER, "follow", player);
                    c.inProximityWaiting = false;
                    // If this plan never got its opening follow (see the heal
                    // below), the resume we just issued IS that opening follow —
                    // record it so the heal does not re-fire next heartbeat.
                    if (botAI && plan->commandsDispatched.empty())
                        plan->commandsDispatched.push_back("follow");
                }
                else if (plan->commandsDispatched.empty())
                {
                    // Heal a half-started plan (FR-022 dispatch robustness).
                    // StartActivityPlan marks the plan Executing but can only
                    // dispatch the opening `follow` when the bot has a
                    // PlayerbotAI at approve time. If the player approved during a
                    // bot spawn/relog window — the SAME no-PlayerbotAI case PR
                    // #222 fixed for the propose path (EmitProactiveLine and the
                    // StartActivityPlan dispatch both no-op without a PlayerbotAI)
                    // — the plan reached Executing with commandsDispatched empty
                    // and the bot never followed: it would sit idle, holding the
                    // player's one executing-plan slot (blocking every new
                    // proposal via section 3's hasExecuting) for the full ~30-min
                    // FR-021 completion timeout before the safety net finally
                    // aborted it. Now that the player is WithinRange and the AI is
                    // back, issue the deferred opening follow so the approved
                    // activity actually begins. Mirrors section 2c's leaked-stay
                    // heal: a single state-driven re-dispatch of an internal
                    // playerbot verb — no chat line, no audit row (exactly like
                    // the resume-follow above). commandsDispatched is the
                    // discriminator: it only ever holds the opening follow, so
                    // empty() == "the approve-time dispatch was skipped", and
                    // pushing it here makes the heal idempotent.
                    if (botAI)
                    {
                        botAI->HandleCommand(CHAT_MSG_WHISPER, "follow", player);
                        plan->commandsDispatched.push_back("follow");
                    }
                }
            }
        }

        // 2b) Completion-timeout safety net (FR-021).
        // Re-acquire the plan since AbortActivityPlan above may have toggled it.
        if (ActivityPlan* p2 = GetExecutingPlanForPlayer(playerGuid); p2)
        {
            uint64_t timeoutMs = static_cast<uint64_t>(g_ProactiveActivityCompleteTimeoutSec) * 1000;
            if (now - p2->startedAtMs > timeoutMs)
            {
                if (p2->completeCheckSentAtMs == 0)
                {
                    // find() not operator[] — same phantom-insert guard as the
                    // proximity-warn path above; a missing proposal must skip the
                    // complete-check, never default-insert a Proposal.
                    auto pit = s_proposalsById.find(p2->proposalId);
                    if (pit != s_proposalsById.end())
                    {
                        Candidate const& cand = pit->second.candidate;
                        uint32_t phraseLatencyMs = 0;
                        std::string text = PhraseLine(botGuid, playerGuid, LineKind::CompleteCheck, cand, "", &phraseLatencyMs);
                        // Gate audit on actual line delivery — else completeCheckSentAtMs
                        // stays 0, this branch re-fires every tick, the else-branch
                        // timeout-abort never runs, and the audit log spams duplicates.
                        if (EmitProactiveLine(playerGuid, botGuid, LineKind::CompleteCheck, text))
                        {
                            p2->completeCheckSentAtMs = now;
                            EmitAudit(playerGuid, botGuid, kSrcNudge, kRsnCompleteCheck, text, phraseLatencyMs);
                        }
                    }
                }
                else
                {
                    uint64_t waitMs = static_cast<uint64_t>(g_ProactiveActivityCompleteNudgeWaitSec) * 1000;
                    if (now - p2->completeCheckSentAtMs > waitMs)
                    {
                        // Completion-timeout = stalled activity. Soft-reject
                        // the candidate so we don't restart the same one.
                        if (auto pit = s_proposalsById.find(p2->proposalId); pit != s_proposalsById.end())
                            RememberRejection(c, pit->second.candidate);
                        AbortActivityPlan(p2->proposalId, "no response to complete check");
                        EmitAudit(playerGuid, botGuid, kSrcAbort, kRsnActivityTimeout, "", 0);
                    }
                }
            }
        }
    }
    else
    {
        // 2c) Idle-anchor (US2 FR-008). When there's no executing plan and the
        // bot drifted out of range, silently re-home onto the player. No chat.
        //
        // Also heal a leaked stay-mode from a prior executing plan that ended
        // (abort/complete/done/quest-turn-in) while `c.inProximityWaiting` was
        // true. Section 2a's TooFar branch dispatched `stay` to the playerbot
        // AI and set the flag; the flag and the playerbot AI's stay-state both
        // persist across the plan teardown (AbortActivityPlan / Complete paths
        // clear `currentActivityPlanId` + `currentProposalId` but neither
        // touches `c.inProximityWaiting` nor re-dispatches `follow`). Without
        // this heal, when the player has already closed the gap (WithinRange)
        // the bot stays glued in place by the lingering `stay` command —
        // violating FR-008 ("an idle leader bot stays near the player"). The
        // TooFar branch is the original anchor case; the WithinRange + flag
        // case is the new heal. DifferentZone/DifferentMap skip both: section
        // 2c can't recover cross-zone/map bots regardless.
        if (bot)
        {
            ProximityState ps = CheckProximity(player, bot);
            bool needRehome   = (ps == ProximityState::TooFar);
            bool needStayHeal = c.inProximityWaiting && ps == ProximityState::WithinRange;
            if (needRehome || needStayHeal)
            {
                PlayerbotAI* botAI = PlayerbotsMgr::instance().GetPlayerbotAI(bot);
                if (botAI) botAI->HandleCommand(CHAT_MSG_WHISPER, "follow", player);
                c.inProximityWaiting = false;
            }
        }
    }

    // 3) Skip if there's still an open proposal OR an executing plan — we
    // don't pipeline a new proposal on top of an active one.
    bool hasOpen      = GetOpenProposalForPlayer(playerGuid)    != nullptr;
    bool hasExecuting = GetExecutingPlanForPlayer(playerGuid)   != nullptr;

    if (hasOpen || hasExecuting) return;

    // 4-6) Attempt a fresh proposal this tick, wrapped in an immediately-invoked
    //      lambda so the idle nudge (US3) below can run as a genuine FALLBACK —
    //      emitted only when NO proposal opened this tick. Pre-fix, section 3a's
    //      idle nudge ran BEFORE this block, so on the heartbeat right after an
    //      open proposal expired (section 1 cleared currentProposalId) BOTH an
    //      idle nudge AND a fresh proposal fired in the same ~10s tick: the bot
    //      whispered a content-free "why are we standing around? Pick something."
    //      and then immediately a concrete "how about we run <X>?". That violates
    //      US3's Independent Test ("each idle nudge clearly differentiated from a
    //      fresh activity proposal") and FR-010's "no active proposal" spirit,
    //      consumes one of the FR-011 nudge-cap slots on a redundant line, and
    //      fires an extra proposer gateway call. The lambda returns true iff a
    //      proposal was actually opened; every no-proposal path (cooldown not
    //      elapsed, FR-023 self-silence, tick-gate/planner `wait`, no candidate,
    //      delivery failure) returns false so the idle nudge still fills genuine
    //      silence exactly as before — only the redundant same-tick pairing of a
    //      nudge with a concrete proposal is removed.
    auto tryOpenProposal = [&]() -> bool {
    // 4) Cooldown gates.
    if (c.sessionStartedAtMs == 0) return false;  // Login hook hasn't anchored us yet.
    uint64_t firstDelayMs = static_cast<uint64_t>(g_ProactiveFirstProposalDelaySec) * 1000;
    if (now - c.sessionStartedAtMs < firstDelayMs) return false;
    if (c.lastProposalAtMs != 0)
    {
        uint64_t cdMs = static_cast<uint64_t>(g_ProactiveProposalCooldownSec) * 1000;
        if (now - c.lastProposalAtMs < cdMs) return false;
    }
    // FR-023 — self-silence the propose loop after N consecutive expired
    // proposals with no player engagement. Mirrors FR-011's idle-nudge cap.
    // Counter is reset on chat / quest / zone activity, so the next genuine
    // engagement wakes the loop again. 0 = unlimited (proposals never self-
    // silence). Gate placed BEFORE the tick-gate + planner + candidate-select
    // so silenced pairs don't burn LLM calls every heartbeat.
    if (g_ProactiveProposalMaxPerSession != 0
        && c.consecutiveExpiredProposals >= g_ProactiveProposalMaxPerSession)
        return false;

    // 4a) Phase C — local-Ollama tick gate. Cheap upstream filter that
    // vetoes obvious no-go ticks before SelectCandidate + paid planner.
    // Throttled per (bot, player) by ProactiveTickGate.IntervalSec so even
    // a 10s heartbeat queries Ollama at most twice per minute. On any
    // failure (disabled / HTTP timeout / parse error / sub-threshold
    // confidence) we silently fall through to the next gate.
    if (g_ProactiveTickGateEnable && !g_ProactiveTickGatePromptText.empty())
    {
        uint64_t intervalMs = static_cast<uint64_t>(g_ProactiveTickGateIntervalSec) * 1000;
        bool throttled = (c.lastTickGateAtMs != 0 && now - c.lastTickGateAtMs < intervalMs);
        if (!throttled)
        {
            c.lastTickGateAtMs = now;
            Player* botPlayerTg = ObjectAccessor::FindPlayer(ObjectGuid(botGuid));
            std::string snap = BuildTickGateSnapshot(player, botPlayerTg, c, now);
            llm::TickGateResult tg = llm::EvaluateTickGate(botGuid, playerGuid, snap);
            if (!tg.rawContent.empty())
            {
                EmitAudit(playerGuid, botGuid, kSrcTickGate,
                          snap, tg.rawContent, tg.latencyMs, /*error=*/false,
                          tg.backend, tg.promptTokens);
            }
            if (tg.action == llm::TickGateAction::Wait &&
                tg.confidence >= tg.minConfidence)   // per-tier threshold, see IntentResult
            {
                // Skip this tick. Do NOT consume the proposal cooldown — the
                // gate's throttle (IntervalSec) is the cost control for the
                // Ollama call itself; the proposal cooldown only fires when
                // we actually emit.
                return false;
            }
            // Propose / Unset / sub-threshold all fall through to candidate
            // selection + emission.
        }
    }

    // 5) Select a candidate.
    Candidate cand = SelectCandidateForPlayer(player, c);
    if (cand.kind == Kind::None) return false;   // Nothing to propose this tick.

    // 5a) Phase B — gateway planner. The agent reads the snapshot (player
    // combat / hp / idle / recent rejections / session age) and either vetoes
    // the proposal (`wait`) or approves it and writes the line (`propose`).
    // On any failure we fall through to the deterministic PhraseLine path.
    // Cost control: a `wait` verdict still consumes the cooldown so we don't
    // hammer the gateway with re-queries.
    std::string plannerLine;
    bool plannerProvidedLine = false;
    if (g_ProactivePlannerEnable && !g_ProactivePlannerPromptText.empty())
    {
        Player* botPlayer = ObjectAccessor::FindPlayer(ObjectGuid(botGuid));
        std::string snapshot = BuildPlannerSnapshot(player, botPlayer, cand, c, now);
        llm::PlannerResult pr = llm::PlanProposalDecision(botGuid, playerGuid, snapshot);
        if (!pr.rawContent.empty())
        {
            EmitAudit(playerGuid, botGuid, kSrcPlanner,
                      snapshot, pr.rawContent, pr.latencyMs, /*error=*/false,
                      pr.backend, pr.promptTokens);
        }
        if (pr.decision == llm::PlanDecision::Wait)
        {
            // Apply normal proposal cooldown so we don't burn gateway calls
            // querying the planner every tick.
            c.lastProposalAtMs = now;
            return false;
        }
        if (pr.decision == llm::PlanDecision::Propose && !pr.line.empty())
        {
            plannerLine = pr.line;
            plannerProvidedLine = true;
        }
        // Unset → fall through to deterministic PhraseLine below.
    }

    // 6) Open the proposal and phrase the line.
    // `phraseLatencyMs` carries the proposer gateway-call time for the
    // proact_propose row per audit-events.md L54. It stays 0 when the planner
    // already supplied the line (plannerProvidedLine) — in that case no proposer
    // call ran, and the planner's own time is already recorded on its separate
    // proact_planner row (the kSrcPlanner EmitAudit in section 5a above), so
    // attributing it here too would double-count. When we fall through to
    // PhraseLine, it sets the real value (0 if the deterministic path ran,
    // measured ms if the gateway was called).
    uint32_t phraseLatencyMs = 0;
    std::string line = plannerProvidedLine
                         ? plannerLine
                         : PhraseLine(botGuid, playerGuid, LineKind::Proposal, cand, "", &phraseLatencyMs);

    // Gate the proposal's STATE MUTATION on actual line delivery, mirroring the
    // three sibling emit sites (IdleNudge / ProximityWarn / CompleteCheck) and
    // the convention PR #163 established ("audit only when the line is actually
    // delivered"). The propose path was the lone emit site that mutated state
    // BEFORE the delivery gate: OpenNewProposal adds the Open proposal to
    // s_proposalsById, sets c.currentProposalId, and stamps c.lastProposalAtMs.
    // When EmitProactiveLine then returned false (empty text; bot/player not in
    // world; or — the reachable case in this auto-claim fleet — a leader bot
    // that IS in-world but has no PlayerbotAI yet during a spawn/relog window),
    // the player never saw the line yet a PHANTOM proposal lingered:
    //   - it occupied the player's one-proposal slot for ProposalExpirySec,
    //     blocking idle nudges (IsPlayerIdleForNudge checks currentProposalId)
    //     AND the next proposal (section 3 hasOpen),
    //   - it emitted NO proact_propose row, then section 1 expired it ~600s
    //     later — a proact_abort `expired` row with NO matching proact_propose,
    //     which corrupts the operator's propose-vs-terminal reconciliation,
    //   - the expiry unfairly bumped consecutiveExpiredProposals toward the
    //     FR-023 self-silence cap and RememberRejection'd a candidate the player
    //     never declined.
    // None of that should happen for a line that was never delivered.
    if (!EmitProactiveLine(playerGuid, botGuid, LineKind::Proposal, line))
    {
        // Apply the normal proposal cooldown — same cost-control move as the
        // planner-`wait` branch above — so a persistent delivery failure (e.g.
        // a freshly spawned/relogged bot whose PlayerbotAI hasn't registered)
        // does not re-run the tick-gate + paid planner on every heartbeat.
        c.lastProposalAtMs = now;
        return false;
    }

    uint64_t pid = OpenNewProposal(playerGuid, botGuid, cand);
    s_proposalsById[pid].lastLineText = line;
    EmitAudit(playerGuid, botGuid, kSrcPropose, "open", line, phraseLatencyMs);
    return true;
    };  // tryOpenProposal

    bool proposalOpened = tryOpenProposal();

    // 3a) Idle nudge (US3 — T049/T050/T051/T053). Runs ONLY as a fallback when
    // the proposal attempt above opened nothing this tick, so a content-free
    // "pick something" nudge is never emitted in the same heartbeat as a concrete
    // proposal (US3 Independent Test: each nudge clearly differentiated from a
    // fresh activity proposal). The eligibility gate (idle threshold, nudge
    // cooldown, FR-011 cap, no open proposal/plan) is unchanged.
    if (!proposalOpened && IsPlayerIdleForNudge(player, c, now))
    {
        Candidate stub;   // Idle nudge has no specific candidate.
        uint32_t phraseLatencyMs = 0;
        std::string text = PhraseLine(botGuid, playerGuid, LineKind::IdleNudge, stub, "", &phraseLatencyMs);
        if (EmitProactiveLine(playerGuid, botGuid, LineKind::IdleNudge, text))
        {
            c.lastIdleNudgeAtMs = now;
            c.ignoredNudgeCount += 1;
            EmitAudit(playerGuid, botGuid, kSrcNudge, kRsnIdle, text, phraseLatencyMs);
        }
    }
}

// ---------------------------------------------------------------------------
// OnPlayerChat helper — applies intent classification to an open proposal /
// executing plan and short-circuits the regular chain when handled.
// ---------------------------------------------------------------------------
bool OnPlayerChatLocked(uint64_t playerGuid, std::string const& message)
{
    Proposal* open = GetOpenProposalForPlayer(playerGuid);
    ActivityPlan* exec = GetExecutingPlanForPlayer(playerGuid);
    if (!open && !exec) return false;

    uint64_t botGuid = 0;
    if (open)
        botGuid = open->botGuid;
    else if (exec)
    {
        // find() not operator[]: GetExecutingPlanForPlayer already located this
        // proposal in s_proposalsById, so it is present in normal flow — but never
        // phantom-insert a default Proposal if that invariant is ever broken.
        auto pit = s_proposalsById.find(exec->proposalId);
        if (pit != s_proposalsById.end())
            botGuid = pit->second.botGuid;
    }
    if (botGuid == 0) return false;

    Intent intent = ClassifyPlayerMessage(botGuid, playerGuid, message,
                                          open != nullptr, exec != nullptr);

    // FR-014 — direct playerbot command takeover. Cancel any open proposal,
    // log it, and return false so the regular handler chain still processes
    // the command (we don't intercept the actual dispatch).
    if (intent == Intent::DirectCommand && open)
    {
        uint64_t pid = open->proposalId;
        // Soft-reject the cancelled candidate so SelectCandidateForPlayer
        // doesn't immediately re-pick what the player just steered away from.
        CadenceState& c = GetCadence(playerGuid, botGuid);
        RememberRejection(c, open->candidate);
        CancelOpenProposalIfAny(playerGuid, kRsnDirectCommand);
        // Disposition literal in requestText, response_chars=0 — matches the
        // 9 sibling kSrcAbort sites (expired/ownerLeftRange/playerChangedZone/
        // activityTimeout/playerChangedMap/logout×2/botLogout×2) and the audit-events.md L53
        // contract ("response_chars = length of the chat line emitted (0 when
        // no chat line)"). The previous arg order shoved the player's chat
        // (variable length) into request_chars and the 13-char "directCommand"
        // literal into response_chars, breaking operator queries of the form
        // `WHERE source_channel='proact_abort' AND request_chars=13` (the
        // request_chars=N operator-filter convention PR #195 established —
        // 13 = len("directCommand"), so it selects exactly this site).
        // Player message length is already captured upstream by
        // the proact_intent classifier row when ProactiveIntent.Enable=1; the
        // verbatim text always lives in worldserver.log via LOG_INFO.
        EmitAudit(playerGuid, botGuid, kSrcAbort, kRsnDirectCommand, "", 0);
        LOG_INFO("server.loading",
                 "[Ollama Chat Proactive] proposal {} cancelled by direct player command",
                 pid);
        return false;   // let the regular chain handle the actual command
    }

    if (intent == Intent::Approve && open)
    {
        Proposal copy = *open;   // capture before mutate
        TransitionProposal(open->proposalId, State::Approved);
        EmitAudit(playerGuid, botGuid, kSrcApprove, message, "", 0);

        // Phrase ack + dispatch ActivityPlan.
        std::string ack = PhraseLine(botGuid, playerGuid, LineKind::ApprovalAck, copy.candidate, "");
        EmitProactiveLine(playerGuid, botGuid, LineKind::ApprovalAck, ack);
        // find() not operator[]: TransitionProposal(Approved) above only flips state
        // (Approved is non-terminal, never reaped) so the proposal is still mapped —
        // but never default-insert a phantom Proposal into StartActivityPlan if it
        // somehow is not.
        if (auto pit = s_proposalsById.find(copy.proposalId); pit != s_proposalsById.end())
            StartActivityPlan(pit->second);
        return true;
    }
    if (intent == Intent::Reject && open)
    {
        CadenceState& c = GetCadence(playerGuid, botGuid);
        RememberRejection(c, open->candidate);
        TransitionProposal(open->proposalId, State::Rejected);
        EmitAudit(playerGuid, botGuid, kSrcReject, message, "", 0);
        return true;
    }
    if (intent == Intent::Done && exec)
    {
        uint64_t pid = exec->proposalId;
        exec->state = State::Completed;
        auto pit = s_proposalsById.find(pid);
        if (pit != s_proposalsById.end()) pit->second.state = State::Completed;
        CadenceState& c = GetCadence(playerGuid, botGuid);
        c.currentActivityPlanId = 0;
        c.currentProposalId     = 0;
        // Disposition literal in requestText, response_chars=0 — matches the
        // 2 sibling kSrcDone sites (the questTurnIn and zoneOut paths)
        // and the audit-events.md L70 contract (proact_done is a "pure state
        // transition" with latency_ms=0). See the directCommand site above
        // for the full operator-query-convention rationale.
        EmitAudit(playerGuid, botGuid, kSrcDone, kRsnPlayerDone, "", 0);

        Candidate const& cand = pit != s_proposalsById.end() ? pit->second.candidate : Candidate{};
        std::string ack = PhraseLine(botGuid, playerGuid, LineKind::CompleteAck, cand, "");
        EmitProactiveLine(playerGuid, botGuid, LineKind::CompleteAck, ack);
        return true;
    }
    return false;
}

}  // namespace (anonymous)

// ---------------------------------------------------------------------------
// Public tap-in surface
// ---------------------------------------------------------------------------

bool IsEngaged(uint64_t playerGuid)
{
    if (!g_ProactiveEnable) return false;
    if (!g_TacticalEnable) return false;
    if (!g_TacticalHumanPresenceRequired) return false;
    return IsWhitelistedHumanOnline(playerGuid);
}

void Heartbeat()
{
    if (!g_ProactiveEnable) return;
    if (!g_TacticalEnable) return;
    if (!g_TacticalHumanPresenceRequired) return;
    if (g_GatewayAutoClaimAccountIdsSet.empty()) return;

    // Enumerate whitelisted-online players. We don't have a direct iterator
    // exposed from `_tactical.cpp` IsAnyWhitelistedHumanOnline(); walk the
    // World's session map.
    std::vector<uint64_t> playerGuids;
    {
        // ObjectAccessor::GetPlayers() returns a HashMapHolder map; iterate.
        // (Read-only access; no lock needed beyond what AC provides.)
        auto const& players = ObjectAccessor::GetPlayers();
        playerGuids.reserve(players.size());
        for (auto const& kv : players)
        {
            Player* p = kv.second;
            // Same predicate as tactical's presence gate: bots, the Overlord
            // session and fleet guids never count — before 2026-09-21 the
            // account test alone made the leader propose plans to Raz.
            if (!IsWhitelistedHumanPlayer(p, /*anyHumanWhenNoWhitelist=*/false)) continue;
            playerGuids.push_back(p->GetGUID().GetRawValue());
        }
    }
    if (playerGuids.empty()) return;

    std::lock_guard<std::mutex> lock(s_mutex);
    // Drop terminal proposals/plans past their retention window BEFORE the
    // per-player loop, so the O(N) lookup scans inside EvaluateTickLocked /
    // ElectDesignatedVoice walk only live (open/approved/executing) + freshly-
    // terminal entries rather than the whole-uptime accumulation (data-model.md).
    ReapTerminalState(NowMs());
    for (uint64_t pg : playerGuids)
    {
        uint64_t voice = ElectDesignatedVoice(pg);
        if (voice == 0)
        {
            EmitNoVoiceDiagnostic(pg, NowMs());
            continue;
        }
        EvaluateTickLocked(voice, pg);
    }
}

bool OnPlayerChat(Player* player, std::string const& message, uint32_t /*channelType*/)
{
    if (!player) return false;
    uint64_t playerGuid = player->GetGUID().GetRawValue();
    if (!IsEngaged(playerGuid)) return false;
    std::lock_guard<std::mutex> lock(s_mutex);

    // Any chat is player activity for idle-nudge tracking (US3 T049/T052).
    // Also a strong engagement signal for the proposal self-silence counter
    // (FR-023) — chatting wakes a silenced propose loop even if the chat
    // wasn't a direct answer to a proposal.
    uint64_t now = NowMs();
    for (auto& kv : s_cadenceByKey)
    {
        if (kv.first.playerGuid == playerGuid)
        {
            kv.second.lastPlayerActivityAtMs = now;
            kv.second.ignoredNudgeCount = 0;
            kv.second.consecutiveExpiredProposals = 0;
        }
    }
    return OnPlayerChatLocked(playerGuid, message);
}

void OnQuestComplete(Player* player, uint32_t questId)
{
    if (!player) return;
    uint64_t playerGuid = player->GetGUID().GetRawValue();
    if (!IsEngaged(playerGuid)) return;
    std::lock_guard<std::mutex> lock(s_mutex);

    // Quest completion is player activity (US3 T049/T052) and a strong
    // engagement signal for the proposal self-silence counter (FR-023).
    uint64_t nowAct = NowMs();
    for (auto& kv : s_cadenceByKey)
    {
        if (kv.first.playerGuid == playerGuid)
        {
            kv.second.lastPlayerActivityAtMs = nowAct;
            kv.second.ignoredNudgeCount = 0;
            kv.second.consecutiveExpiredProposals = 0;
        }
    }

    ActivityPlan* exec = GetExecutingPlanForPlayer(playerGuid);
    if (!exec) return;
    auto pit = s_proposalsById.find(exec->proposalId);
    if (pit == s_proposalsById.end()) return;
    if (pit->second.candidate.kind != Kind::Quest) return;
    if (pit->second.candidate.questId != questId) return;

    uint64_t botGuid = pit->second.botGuid;
    exec->state = State::Completed;
    pit->second.state = State::Completed;
    CadenceState& c = GetCadence(playerGuid, botGuid);
    c.currentActivityPlanId = 0;
    c.currentProposalId     = 0;
    EmitAudit(playerGuid, botGuid, kSrcDone, kRsnQuestTurnIn, "", 0);

    std::string ack = PhraseLine(botGuid, playerGuid, LineKind::CompleteAck, pit->second.candidate, "");
    EmitProactiveLine(playerGuid, botGuid, LineKind::CompleteAck, ack);
}

// Ambient level-up congratulation (playbook wedge #1). Deliberately
// deterministic-only — see LineKind::LevelUp's DeterministicPhrasing case and
// the header comment on this function. Reuses `kSrcNudge` (audit-events.md:45
// already documents that channel as covering idle/proximity/complete-check
// ambient lines; a level-up congrats is the same shape, differentiated by the
// "levelUp" requestText reason rather than a new source_channel).
void OnPlayerLevelChanged(Player* player, uint8_t /*oldLevel*/)
{
    if (!g_ProactiveLevelUpCongratsEnable) return;
    if (!player) return;
    uint64_t playerGuid = player->GetGUID().GetRawValue();
    if (!IsEngaged(playerGuid)) return;

    std::lock_guard<std::mutex> lock(s_mutex);
    uint64_t voice = ElectDesignatedVoice(playerGuid);
    if (voice == 0) return;

    uint64_t now = NowMs();
    CadenceState& c = GetCadence(playerGuid, voice);
    uint64_t cdMs = static_cast<uint64_t>(g_ProactiveLevelUpCongratsCooldownSec) * 1000;
    if (c.lastLevelUpCongratsAtMs != 0 && now - c.lastLevelUpCongratsAtMs < cdMs)
        return;

    std::string line = DeterministicPhrasing(LineKind::LevelUp, Candidate{}, player->GetName(),
                                              std::to_string(player->GetLevel()));
    if (EmitProactiveLine(playerGuid, voice, LineKind::LevelUp, line))
    {
        c.lastLevelUpCongratsAtMs = now;
        EmitAudit(playerGuid, voice, kSrcNudge, kRsnLevelUp, line, 0);
    }
}

void OnZoneChange(Player* player, uint32_t /*newZoneId*/, uint32_t newMapId)
{
    if (!player) return;
    uint64_t playerGuid = player->GetGUID().GetRawValue();
    if (!IsEngaged(playerGuid)) return;
    std::lock_guard<std::mutex> lock(s_mutex);

    // Zone change is player activity for idle-nudge tracking (US3 T049/T052)
    // and a strong engagement signal for the proposal self-silence counter
    // (FR-023) — the player actively chose to go somewhere.
    uint64_t nowAct = NowMs();
    for (auto& kv : s_cadenceByKey)
    {
        if (kv.first.playerGuid == playerGuid)
        {
            kv.second.lastPlayerActivityAtMs = nowAct;
            kv.second.ignoredNudgeCount = 0;
            kv.second.consecutiveExpiredProposals = 0;
        }
    }

    // FR-009 — on a cross-map transition (hearth, portal, zeppelin, instance
    // entry/exit), any Open proposal that was anchored to the previous map is
    // now stale: the candidate's zone / dungeon-entrance / in-zone-quest
    // context doesn't apply to the new location, and waiting the full 10-min
    // expiry leaves the bot mute when it should be re-opening for the new
    // context. Cancel it so the next heartbeat picks a fresh candidate. NOT
    // a candidate-quality signal — deliberately skip RememberRejection, same
    // exclusion PR #149 applied to the executing-plan playerChangedZone abort.
    // No `originMapId != 0` "not-set" sentinel: mapId 0 is Eastern Kingdoms
    // (Stormwind/Westfall/Hillsbrad/...), not "unset" — the original guard
    // silently suppressed the cancel for every EK-originating proposal,
    // leaving the bot mute for the full 10-min expiry on the most populated
    // Alliance continent. opener is guaranteed in-world by the EvaluateTick
    // entry check + s_mutex held across OnPlayerLogout, so originMapId is
    // always set to a real map ID by OpenNewProposal. Same sentinel/
    // legitimate-zero conflation pattern as PR #174 (lastPlayerActivityAtMs).
    if (Proposal* open = GetOpenProposalForPlayer(playerGuid);
        open && open->originMapId != newMapId)
    {
        uint64_t pid       = open->proposalId;
        uint64_t botGuid   = open->botGuid;
        uint32_t originMap = open->originMapId;
        CancelOpenProposalIfAny(playerGuid, kRsnPlayerChangedMap);
        EmitAudit(playerGuid, botGuid, kSrcAbort, kRsnPlayerChangedMap, "", 0);
        LOG_INFO("server.loading",
                 "[Ollama Chat Proactive] open proposal {} cancelled — player crossed "
                 "maps (originMapId={} newMapId={})",
                 pid, originMap, newMapId);
    }

    ActivityPlan* exec = GetExecutingPlanForPlayer(playerGuid);
    if (!exec) return;
    auto pit = s_proposalsById.find(exec->proposalId);
    if (pit == s_proposalsById.end()) return;
    if (pit->second.candidate.kind != Kind::Dungeon) return;
    if (pit->second.candidate.mapId == 0) return;

    // FR-021 entry/exit detection split. The "primary — per-category server
    // signals" clause says dungeon completion is "zone-out from the instance
    // map" — which presupposes the player was IN the instance map first.
    // Two transitions matter:
    //   (1) newMapId == candidate.mapId — player just zoned INTO the
    //       instance. Mark the plan so a later cross-map can be classified
    //       as a real completion.
    //   (2) newMapId != candidate.mapId — player crossed off the instance
    //       map. Only a completion if (1) ever fired; otherwise it's a
    //       pre-entry abandonment (player hearthed/portaled to a third map
    //       before stepping into the instance).
    //
    // Pre-fix, the exit branch fired on any cross-map regardless of whether
    // the player ever entered the instance. Net effect: every dungeon
    // proposal that the player approved-then-hearthed-from emitted a false
    // `proact_done` "zoneOut" row, AND preempted the next heartbeat's
    // section 2a DifferentMap branch (GetExecutingPlanForPlayer returns
    // nullptr once the plan flips to Completed) which would otherwise have
    // emitted the correct `proact_abort` "playerChangedZone". Audit table
    // recorded fake completions for activities that never began.
    //
    // Same single-site state-driven shape as PR #171 (stay-mode heal at
    // section 2c) and PR #174 (lastPlayerActivityAtMs anchor inheritance):
    // one new flag, gate the existing branch on it, defer the
    // non-flagged case to the existing path that already handles it.
    if (pit->second.candidate.mapId == newMapId)
    {
        exec->enteredCandidateMap = true;
        return;
    }
    if (!exec->enteredCandidateMap) return;
    if (exec->state == State::Executing)
    {
        uint64_t botGuid = pit->second.botGuid;
        exec->state = State::Completed;
        pit->second.state = State::Completed;
        CadenceState& c = GetCadence(playerGuid, botGuid);
        c.currentActivityPlanId = 0;
        c.currentProposalId     = 0;
        EmitAudit(playerGuid, botGuid, kSrcDone, kRsnZoneOut, "", 0);
    }
}

void OnPlayerLogin(uint64_t playerGuid)
{
    std::lock_guard<std::mutex> lock(s_mutex);
    // Clear any stale state for this player (no persistence — FR-016).
    for (auto it = s_cadenceByKey.begin(); it != s_cadenceByKey.end(); )
    {
        if (it->first.playerGuid == playerGuid) it = s_cadenceByKey.erase(it);
        else ++it;
    }
    // Anchor session start time so first-proposal-delay countdown begins now.
    // We don't know the designated voice yet — EvaluateTick will lazily
    // create the CadenceState for the (player, electedVoice) pair on its
    // first tick and inherit this timestamp via a fresh row. To support
    // that we also set a "any cadence row for this player" sentinel: drop a
    // row keyed by (playerGuid, 0) just to mark session-start time. Anchor
    // lastPlayerActivityAtMs at the same instant so the idle-nudge gate has
    // a baseline for a player who logs in and never moves/chats — without
    // this, FR-010's "stationary for IdleThresholdSec" nudge path never
    // fires for AFK-on-login because IsPlayerIdleForNudge treats
    // lastPlayerActivityAtMs == 0 as "never seen, do not nudge."
    uint64_t now = NowMs();
    CadenceState& sentinel = GetCadence(playerGuid, 0);
    sentinel.sessionStartedAtMs     = now;
    sentinel.lastPlayerActivityAtMs = now;
}

void OnPlayerLogout(uint64_t playerGuid)
{
    std::lock_guard<std::mutex> lock(s_mutex);

    // FR-016 — proactive state MUST NOT survive a player relog. The cleanup
    // below covers three buckets: cadence rows (idle counters, cooldown
    // anchors), in-flight ActivityPlans, and non-terminal proposals.
    //
    // Aborting the executing plan + cancelling its Approved proposal is the
    // load-bearing part: GetExecutingPlanForPlayer's `state == Executing`
    // predicate does NOT consult the linked proposal's state, so a stale plan
    // left behind here gets re-discovered by the next Heartbeat after relog.
    // NowMs() is a steady_clock — it does NOT pause across logout — so any
    // session that relogs >ActivityCompleteTimeoutSec (default 30 min) later
    // immediately trips the completion-timeout safety net at
    // _proactive.cpp:1349 and whispers a CompleteCheck referring to an
    // activity the player no longer remembers. Cancelling Approved (but not
    // yet Executing) proposals closes the tiny window where OnPlayerChat's
    // approve branch ran but StartActivityPlan didn't complete; same FR-016
    // intent — fresh proposal cycle on relog, not a partially-built one.
    // Deliberately skip RememberRejection on this path, mirroring PR #149's
    // playerChangedZone + PR #155's playerChangedMap exclusions: logout is a
    // player-presence signal, not a candidate-quality signal.
    //
    // Emit `proact_abort` `logout` for each aborted plan and each cancelled
    // proposal — audit-events.md:47 promises "proact_abort: An Activity Plan
    // transitions to `aborted`, OR an open Proposal is expired/cancelled/
    // superseded" and every other state-transition site (expired/timeout/
    // playerChangedZone/playerChangedMap/directCommand) emits the matching
    // row. Without these emits the audit table understates the abort rate
    // (logout-driven aborts/cancels are invisible — they don't surface in
    // the operator's `ops_audit_gateway` replay and they cause a numeric
    // gap in the cron's `propose vs. terminal-state` reconciliation that
    // forces fallback to source-read diagnosis even when the table is up).
    // Same audit-completeness shape as PR #155 (playerChangedMap emission
    // for the cross-map cancel path) and PR #163 (gating complete_check
    // audit on actual line delivery).
    for (auto& kv : s_plansByProposalId)
    {
        if (kv.second.state != State::Executing) continue;
        auto pit = s_proposalsById.find(kv.first);
        if (pit == s_proposalsById.end() || pit->second.playerGuid != playerGuid) continue;
        kv.second.state   = State::Aborted;
        pit->second.state = State::Aborted;
        EmitAudit(playerGuid, pit->second.botGuid, kSrcAbort, kRsnLogout, "", 0);
    }

    // Drop all cadence rows for this player.
    for (auto it = s_cadenceByKey.begin(); it != s_cadenceByKey.end(); )
    {
        if (it->first.playerGuid == playerGuid) it = s_cadenceByKey.erase(it);
        else ++it;
    }
    // Cancel any non-terminal proposal (Open or Approved). The Approved
    // proposals linked to executing plans were already flipped to Aborted
    // (and audited) by the loop above, so they fall through the predicate
    // here and aren't double-counted.
    for (auto& kv : s_proposalsById)
    {
        if (kv.second.playerGuid == playerGuid
            && (kv.second.state == State::Open || kv.second.state == State::Approved))
        {
            kv.second.state = State::Cancelled;
            EmitAudit(playerGuid, kv.second.botGuid, kSrcAbort, kRsnLogout, "", 0);
        }
    }

    // FR-016 spec.md:127 ("Reset on bot relog") + spec.md:84 edge case
    // ("Server restart / bot relog ... Pending proposals do not survive ...
    // fresh proposal cycle"). PlayerScript fires OnPlayerLogout for ANY
    // Player object — and mod-playerbots' leader bots ARE Player objects, so
    // this same hook runs when a leader bot drops session (daily fleet cycle,
    // worldserver restart, in-world bot logout). The player-side cleanup
    // above filters on `playerGuid` (the human in (human, bot) state keys),
    // so when the leader BOT logs out, every loop walks past every entry and
    // nothing gets cleaned. After the bot reconnects (or a different leader
    // bot is elected via ElectDesignatedVoice), the (human, leader-bot)
    // cadence row still pointed to a stale Approved proposal, the linked
    // ActivityPlan still said Executing, and `_proactive.cpp:1359`'s
    // completion-timeout safety net (`now - p2->startedAtMs > timeoutMs`)
    // fired a CompleteCheck on the next heartbeat whispering about an
    // activity the player had moved on from — exact mirror of PR #169's
    // player-relog plan leak, this time on the bot side of the pair. Audit
    // rows are emitted per state transition per audit-events.md:47
    // ("proact_abort: An Activity Plan transitions to `aborted`, OR an open
    // Proposal is expired/cancelled/superseded"). No double-counting: the
    // plan-abort loop flips its proposal to Aborted, so the proposal-cancel
    // loop's Open||Approved predicate skips them (same argument as PR #191's
    // loop-2 comment). Loops walk small unordered_maps; no fast-path guard
    // on `g_McpLeaderBotGUIDSet` membership — a stale row left by a bot that
    // was once a leader and later removed from the set still needs cleaning.
    uint64_t botGuid = playerGuid;
    for (auto& kv : s_plansByProposalId)
    {
        if (kv.second.state != State::Executing) continue;
        auto pit = s_proposalsById.find(kv.first);
        if (pit == s_proposalsById.end() || pit->second.botGuid != botGuid) continue;
        kv.second.state   = State::Aborted;
        pit->second.state = State::Aborted;
        EmitAudit(pit->second.playerGuid, botGuid, kSrcAbort, kRsnBotLogout, "", 0);
    }
    // Drop all cadence rows where this bot was the speaker.
    for (auto it = s_cadenceByKey.begin(); it != s_cadenceByKey.end(); )
    {
        if (it->first.botGuid == botGuid) it = s_cadenceByKey.erase(it);
        else ++it;
    }
    // Cancel any remaining non-terminal proposal whose speaker was this bot.
    for (auto& kv : s_proposalsById)
    {
        if (kv.second.botGuid != botGuid) continue;
        if (kv.second.state != State::Open && kv.second.state != State::Approved) continue;
        kv.second.state = State::Cancelled;
        EmitAudit(kv.second.playerGuid, botGuid, kSrcAbort, kRsnBotLogout, "", 0);
    }
}

// ---------------------------------------------------------------------------
// PlayerScript registration — wires OnPlayerLogin / OnLogout into AC's
// ScriptMgr without adding boilerplate to `_main.cpp`. RegisterScripts() is
// the one entry point `_main.cpp` calls; the class is file-local.
// ---------------------------------------------------------------------------
namespace
{
class ProactivePlayerScript : public PlayerScript
{
public:
    ProactivePlayerScript() : PlayerScript("ProactivePlayerScript") {}

    void OnPlayerLogin(Player* player) override
    {
        if (!player) return;
        ollamachat::proactive::OnPlayerLogin(player->GetGUID().GetRawValue());
    }

    void OnPlayerLogout(Player* player) override
    {
        if (!player) return;
        ollamachat::proactive::OnPlayerLogout(player->GetGUID().GetRawValue());
    }

    // Wire FR-009 cross-map abandon. Spec T017/T039/T046 ship `OnZoneChange`
    // logic in `_proactive.cpp` but the PlayerScript hook was never attached,
    // so the dungeon-zone-out completion and the new Open-proposal cancel
    // both lived as dead code. By the time this fires the player is already
    // on the new map, so `player->GetMapId()` is the post-change value.
    void OnPlayerMapChanged(Player* player) override
    {
        if (!player) return;
        ollamachat::proactive::OnZoneChange(player,
                                            player->GetZoneId(),
                                            player->GetMapId());
    }

    // Wire the level-up-congratulation-cycle wedge. Confirmed via the
    // mod-playerbots/azerothcore-wotlk fork's PlayerScript.h that the real
    // hook name is `OnPlayerLevelChanged` (not `OnLevelChanged`), and that it
    // fires only on a genuine `Player::GiveLevel` transition — never at
    // character creation.
    void OnPlayerLevelChanged(Player* player, uint8 oldLevel) override
    {
        if (!player) return;
        ollamachat::proactive::OnPlayerLevelChanged(player, oldLevel);
    }
};
}  // namespace

void RegisterScripts()
{
    new ProactivePlayerScript();
}

void OnConfigReloaded()
{
    if (!g_ProactiveEnable)
    {
        LOG_INFO("server.loading", "[Ollama Chat Proactive] disabled (Proactive.Enable=0)");
        return;
    }
    g_ProactiveProposerPromptText.clear();
    if (g_ProactiveProposerPromptFile.empty())
    {
        LOG_INFO("server.loading",
                 "[Ollama Chat Proactive] proposer prompt file path empty; using deterministic fallback phrasing");
    }
    else
    {
        std::ifstream f(g_ProactiveProposerPromptFile);
        if (!f.good())
        {
            LOG_ERROR("server.loading",
                      "[Ollama Chat Proactive] proposer prompt file not found: {}; disabling feature",
                      g_ProactiveProposerPromptFile);
            g_ProactiveEnable = false;
            return;
        }
        std::stringstream buf;
        buf << f.rdbuf();
        g_ProactiveProposerPromptText = buf.str();
    }

    // Phase A — intent classifier prompt. Missing file does NOT disable the
    // feature: ClassifyPlayerMessage's deterministic substring matcher remains
    // as the fallback so the loop still works.
    g_ProactiveIntentPromptText.clear();
    if (g_ProactiveIntentEnable && !g_ProactiveIntentPromptFile.empty())
    {
        std::ifstream f(g_ProactiveIntentPromptFile);
        if (!f.good())
        {
            LOG_ERROR("server.loading",
                      "[Ollama Chat Proactive] intent prompt file not found: {}; "
                      "falling back to deterministic substring matcher",
                      g_ProactiveIntentPromptFile);
        }
        else
        {
            std::stringstream buf;
            buf << f.rdbuf();
            g_ProactiveIntentPromptText = buf.str();
        }
    }

    // Phase B — planner prompt. Same fail-safe: a missing file logs an error
    // and falls back to SelectCandidateForPlayer + PhraseLine inside
    // EvaluateTickLocked, but does NOT disable the feature.
    g_ProactivePlannerPromptText.clear();
    if (g_ProactivePlannerEnable && !g_ProactivePlannerPromptFile.empty())
    {
        std::ifstream f(g_ProactivePlannerPromptFile);
        if (!f.good())
        {
            LOG_ERROR("server.loading",
                      "[Ollama Chat Proactive] planner prompt file not found: {}; "
                      "falling back to deterministic SelectCandidate + PhraseLine path",
                      g_ProactivePlannerPromptFile);
        }
        else
        {
            std::stringstream buf;
            buf << f.rdbuf();
            g_ProactivePlannerPromptText = buf.str();
        }
    }

    // Phase C — tick-gate prompt. Missing file falls through to the
    // deterministic-then-planner path; never disables the feature.
    g_ProactiveTickGatePromptText.clear();
    if (g_ProactiveTickGateEnable && !g_ProactiveTickGatePromptFile.empty())
    {
        std::ifstream f(g_ProactiveTickGatePromptFile);
        if (!f.good())
        {
            LOG_ERROR("server.loading",
                      "[Ollama Chat Proactive] tick-gate prompt file not found: {}; "
                      "falling through to deterministic propose path",
                      g_ProactiveTickGatePromptFile);
        }
        else
        {
            std::stringstream buf;
            buf << f.rdbuf();
            g_ProactiveTickGatePromptText = buf.str();
        }
    }

    LOG_INFO("server.loading",
             "[Ollama Chat Proactive] enabled (voice={}, ProposalCooldownSec={}, IdleThresholdSec={}, "
             "ProximityYards={}, ProposerPromptBytes={}, IntentEnable={}, IntentPromptBytes={}, "
             "IntentMinConf={}, PlannerEnable={}, PlannerPromptBytes={}, TickGateEnable={}, "
             "TickGatePromptBytes={}, TickGateIntervalSec={}, TickGateMinConf={})",
             g_ProactivePreferredVoiceGuid,
             g_ProactiveProposalCooldownSec,
             g_ProactiveIdleThresholdSec,
             g_ProactiveProximityYards,
             g_ProactiveProposerPromptText.size(),
             g_ProactiveIntentEnable,
             g_ProactiveIntentPromptText.size(),
             g_ProactiveIntentMinConfidence,
             g_ProactivePlannerEnable,
             g_ProactivePlannerPromptText.size(),
             g_ProactiveTickGateEnable,
             g_ProactiveTickGatePromptText.size(),
             g_ProactiveTickGateIntervalSec,
             g_ProactiveTickGateMinConfidence);
}

}  // namespace proactive
}  // namespace ollamachat
