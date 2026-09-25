#ifndef MOD_OLLAMA_CHAT_LEADERTICK_H
#define MOD_OLLAMA_CHAT_LEADERTICK_H

#include <atomic>
#include <thread>
#include <memory>

// Tier 10: Autonomous Leader Tick
//
// A background thread that periodically samples each leader bot's situation
// (combat / target / group / nearby threats / hp bucket / zone) and, when the
// state has materially changed since the last tick, fires a synthetic gateway
// request with `playerGuid=0`. The agent receives a "[AUTONOMOUS TICK]" system-
// prompt note (added by BuildGatewaySystemPromptEx) and may issue leader_*
// MCP tool calls to coordinate the squad.
//
// Guardrails:
//   - State-diff filter: identical snapshots skip the gateway call entirely.
//   - Hourly budget per leader (g_McpLeaderAutoTickMaxPerHour) — prevents
//     runaway token spend.
//   - Spawned only when g_McpLeaderEnable && g_McpLeaderAutoTickEnable.

class GatewayLeaderTick
{
public:
    static GatewayLeaderTick& Instance();

    void Start();
    void Stop();
    bool IsRunning() const { return m_running.load(std::memory_order_acquire); }

private:
    GatewayLeaderTick() = default;
    GatewayLeaderTick(const GatewayLeaderTick&) = delete;
    GatewayLeaderTick& operator=(const GatewayLeaderTick&) = delete;

    void Run();

    std::atomic<bool> m_running{false};
    std::atomic<bool> m_stop{false};
    std::unique_ptr<std::thread> m_thread;
};

#endif // MOD_OLLAMA_CHAT_LEADERTICK_H
