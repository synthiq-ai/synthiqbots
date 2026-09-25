#ifndef MOD_OLLAMA_CHAT_MCPSERVER_H
#define MOD_OLLAMA_CHAT_MCPSERVER_H

#include <atomic>
#include <map>
#include <memory>
#include <mutex>
#include <string>
#include <thread>
#include <vector>

namespace httplib { class Server; }

// Per-tool call accounting. Populated on every tools/call dispatch so operators
// can see which tools actually get used, which are error-heavy, and their latency
// profile — the 2026 MCP production guide calls this out as the minimum useful
// observability. Histograms would be nicer but avg/max is plenty for a private server.
struct McpToolStat
{
    uint64_t    calls       = 0;
    uint64_t    errors      = 0;
    uint64_t    totalMs     = 0;
    uint64_t    maxMs       = 0;
    std::string lastError;      // truncated; last non-success result's "error" field (if any)
};

// Flat snapshot of a single tool's stats, used by `.ollama gateway status`.
struct McpToolStatSnapshot
{
    std::string name;
    uint64_t    calls;
    uint64_t    errors;
    uint64_t    avgMs;
    uint64_t    maxMs;
    std::string lastError;
};

// Embedded HTTP MCP (Model Context Protocol) server that exposes the gateway tool
// registry over JSON-RPC 2.0 so external Claude Agent SDK gateways (e.g. Synthiq)
// can load it via their .mcp.json config and call the tools natively.
//
// Why this exists: both production gateways (OpenClaw and Synthiq) drop request-body
// `tools[]` on the floor — they have their own baked-in tool handling. To actually
// expose WoW game state to the agent we have to register as an external MCP server
// that the agent's SDK loads alongside its built-in tools.
//
// The server runs on a detached background thread spawned from the module's
// OnStartup hook. It listens on g_McpBindAddress:g_McpPort, requires bearer-token
// auth on every request, and wraps the existing tool registry from
// mod-ollama-chat_tools.{h,cpp} — no changes to the registry are needed.
class GatewayMcpServer
{
public:
    static GatewayMcpServer& Instance();

    // Start the HTTP server on a background thread. Idempotent.
    // Reads g_McpEnable / g_McpBindAddress / g_McpPort / g_McpBearerToken at call time.
    void Start();

    // Stop the listener and join the background thread. Idempotent.
    // Safe to call from OnShutdown or before re-Start on config reload.
    void Stop();

    // True if the listener thread is running. Used by `.ollama gateway status`.
    bool IsRunning() const { return m_running.load(std::memory_order_acquire); }

    // Aggregate counters (also exposed to `.ollama gateway status`).
    uint64_t TotalRequests() const  { return m_totalRequests.load(std::memory_order_relaxed); }
    uint64_t TotalToolCalls() const { return m_totalToolCalls.load(std::memory_order_relaxed); }
    uint64_t TotalUnauthorized() const { return m_totalUnauth.load(std::memory_order_relaxed); }

    // Snapshot of per-tool stats sorted by call count descending. Never references
    // the underlying storage — safe to outlive the server.
    std::vector<McpToolStatSnapshot> ToolStatsSnapshot() const;

private:
    // Record one tools/call completion. Called on the request thread after dispatch.
    void RecordToolCall(const std::string& toolName, uint64_t latencyMs, bool isError, const std::string& errorText);

    GatewayMcpServer() = default;
    ~GatewayMcpServer();
    GatewayMcpServer(const GatewayMcpServer&) = delete;
    GatewayMcpServer& operator=(const GatewayMcpServer&) = delete;

    void RunListener(); // body of m_thread

    std::unique_ptr<httplib::Server> m_server;
    std::thread                      m_thread;
    std::atomic<bool>                m_running{false};

    std::atomic<uint64_t> m_totalRequests{0};
    std::atomic<uint64_t> m_totalToolCalls{0};
    std::atomic<uint64_t> m_totalUnauth{0};

    // std::map (not unordered_map) keeps the snapshot deterministically ordered before
    // the caller sorts by calls — plus the registry has ~17 entries, so tree-vs-hash
    // overhead is irrelevant. Guarded by m_toolStatsMutex on every access.
    mutable std::mutex                     m_toolStatsMutex;
    std::map<std::string, McpToolStat>     m_toolStats;
};

#endif // MOD_OLLAMA_CHAT_MCPSERVER_H
