#ifndef MOD_OLLAMA_CHAT_TACTICAL_H
#define MOD_OLLAMA_CHAT_TACTICAL_H

#include <atomic>
#include <cstdint>
#include <ctime>
#include <memory>
#include <string>
#include <thread>
#include <nlohmann/json.hpp>

// Companion-mode tactical agent layer.
//
// Runs a low-frequency heartbeat (event-triggered + slow fallback) that samples
// explicit tactical bots plus nearby auto-enrolled playerbots while a
// whitelisted human is online. A compact snapshot is sent to a local Ollama
// endpoint; the model returns a single JSON action that is dispatched through
// the existing MCP tool registry (no duplicate action layer, no HTTP self-loop).
//
// Design notes:
//   - The paid gateway is NEVER on a timer. Strategic decisions are requested
//     only when the tactical model emits an `escalate` field, or when a human
//     chats. The StrategicEscalationWorker below handles the former.
//   - Combat tactics stay with the playerbots engine. Tactical picks strategy
//     and social actions; the dispatcher rejects combat-override actions while
//     the bot is InCombat() unless Tactical.AllowCombatOverride=1.
//   - Everything here is a no-op until OllamaChat.Tactical.Enable=1 and either
//     a bot is listed in OllamaChat.Tactical.BotGUIDs or nearby auto-enroll is
//     enabled.

class TacticalLeaderTick
{
public:
    static TacticalLeaderTick& Instance();

    void Start();
    void Stop();
    bool IsRunning() const { return m_running.load(std::memory_order_acquire); }

private:
    TacticalLeaderTick() = default;
    TacticalLeaderTick(const TacticalLeaderTick&) = delete;
    TacticalLeaderTick& operator=(const TacticalLeaderTick&) = delete;

    void Run();

    std::atomic<bool> m_running{false};
    std::atomic<bool> m_stop{false};
    std::unique_ptr<std::thread> m_thread;
};

// Strategic escalation worker — condvar-signalled, idle by default.
// Wakes only when the tactical layer enqueues an escalation for a specific bot.
// Processes one escalation per wake (respecting per-bot hour cap + cooldown) by
// firing a single paid gateway call, whose response is expected to invoke
// tactical_set_directive via tool_use. No timer, no scheduler.
//
// Scaffolded here in Phase 1; wired in Phase 4.
class StrategicEscalationWorker
{
public:
    static StrategicEscalationWorker& Instance();

    void Start();
    void Stop();
    bool IsRunning() const { return m_running.load(std::memory_order_acquire); }

private:
    StrategicEscalationWorker() = default;
    StrategicEscalationWorker(const StrategicEscalationWorker&) = delete;
    StrategicEscalationWorker& operator=(const StrategicEscalationWorker&) = delete;

    void Run();

    std::atomic<bool> m_running{false};
    std::atomic<bool> m_stop{false};
    std::unique_ptr<std::thread> m_thread;
};

// ---------------------------------------------------------------------------
// Directive — the memory bridge between strategic (paid) and tactical (free).
// Strategic writes a goal + TTL via tactical_set_directive; tactical reads it
// on every tick and keeps acting toward it until it expires or gets replaced.
// ---------------------------------------------------------------------------
struct TacticalDirective
{
    std::string goal;        // short natural-language goal statement
    time_t      setAt   = 0;
    time_t      expiresAt = 0;
    std::string setBy;       // e.g. "strategic:claude" or "human:<name>"

    bool IsActive(time_t now) const { return !goal.empty() && expiresAt > now; }
};

// True while the CURRENT THREAD is inside a StrategicEscalationWorker gateway
// call. The gateway's tool_call loop executes tools synchronously on the
// calling thread, so tactical_set_directive can use this to tell an
// escalation-originated write apart from a robot/voice one (directive
// arbitration: operator intent wins over escalation).
bool InEscalationContext();

// The ONE definition of "a human is online" for presence gating (tactical
// HumanPresenceRequired, say-range checks) and proactive addressing. A human is
// an in-world player on a real client session: never a playerbot (has a
// PlayerbotAI — includes the self-mastered leader), never the Overlord's
// synthetic master session, never a fleet guid (Fleet.LeaderGuid /
// MemberGuids / Mcp.Leader.SystemMasterGuid) — and, when
// Gateway.AutoClaimAccountIds is set (NOT Gateway.WhitelistAccountIds, which
// gates chat), on one of those accounts. The fleet lives on account 1001,
// which is why the account test alone let Botmaster and Raz count as "the
// human" (2026-09-21).
// anyHumanWhenNoWhitelist: tactical treats an empty whitelist as "any real
// human"; proactive treats it as "nobody" (FR-012).
class Player;
bool IsWhitelistedHumanPlayer(Player* p, bool anyHumanWhenNoWhitelist);

// Returns the active directive for a bot, or a default-constructed directive
// (IsActive() == false) if none is set or it has expired.
TacticalDirective GetTacticalDirective(uint64_t botGuid);

// Writes a new directive. ttlSec is clamped to g_TacticalDirectiveMaxTtlSec.
// Passing an empty goal clears the directive. setBy is an arbitrary label for
// the audit trail (tool dispatcher sets this per call site).
void SetTacticalDirective(uint64_t botGuid,
                          const std::string& goal,
                          uint32_t ttlSec,
                          const std::string& setBy);

// Called by the tactical dispatcher when the local model emits `escalate` in
// its JSON reply. Non-blocking: enqueues the escalation for the strategic
// worker to pick up on its own thread; tactical continues with the current
// directive immediately.
void EnqueueTacticalEscalation(uint64_t botGuid,
                               const std::string& reason,
                               const std::string& snapshot);

// Records a short-lived world event for the next tactical tick. This replaces
// the legacy free-form event chatter path: tactical decides whether to speak,
// emote, act, or ignore the event through its normal structured loop.
// Debug view of a bot's last tactical tick: the snapshot the models saw, the
// actions in no-progress backoff, and when. Empty object when the bot has not
// ticked since the worldserver started. Thread-safe.
nlohmann::json GetTacticalDebugState(uint64_t botGuid);

// Test hook: the bot's next `ticks` (0..10, 0 clears) tactical ticks dispatch
// `action` instead of the model's pick. World-independent, thread-safe.
nlohmann::json SetTacticalForcedAction(uint64_t botGuid, const std::string& action, int ticks);

void RecordTacticalWorldEvent(uint64_t sourceGuid,
                              const std::string& sourceName,
                              uint32_t mapId,
                              float x,
                              float y,
                              float z,
                              const std::string& eventType,
                              const std::string& detail);

// Prune audit rows older than g_TacticalAuditRetentionDays. Called from
// OnStartup + ReloadOllamaChatRuntime.
void PruneTacticalAuditRows();

#endif // MOD_OLLAMA_CHAT_TACTICAL_H
