#include "mod-ollama-chat_mcpserver.h"
#include "mod-ollama-chat_config.h"
#include "mod-ollama-chat_tools.h"
#include "Log.h"

#include <httplib.h>
#include <nlohmann/json.hpp>
#include <algorithm>
#include <chrono>
#include <map>      // std::map — keeps the tools/list facade block alphabetical

namespace
{
    // MCP protocol version we advertise on `initialize`. Pin to a known revision
    // so future Anthropic SDK upgrades don't silently break us.
    constexpr const char* kMcpProtocolVersion = "2025-11-25";

    // JSON-RPC 2.0 error codes used here.
    constexpr int kRpcParseError     = -32700;
    constexpr int kRpcInvalidRequest = -32600;
    constexpr int kRpcMethodNotFound = -32601;
    constexpr int kRpcInvalidParams  = -32602;
    [[maybe_unused]] constexpr int kRpcInternalError = -32603;

    nlohmann::json MakeRpcError(const nlohmann::json& id, int code, const std::string& message)
    {
        return {
            {"jsonrpc", "2.0"},
            {"id", id.is_null() ? nlohmann::json(nullptr) : id},
            {"error", {{"code", code}, {"message", message}}}
        };
    }

    nlohmann::json MakeRpcResult(const nlohmann::json& id, nlohmann::json result)
    {
        return {
            {"jsonrpc", "2.0"},
            {"id", id},
            {"result", std::move(result)}
        };
    }

    // Pull botGuid + playerGuid out of the call arguments (model is responsible for
    // including them). Returns 0 for either if missing.
    //
    // Server-side safety net: when `botGuid` is absent or 0 AND exactly one
    // leader bot is configured (`Mcp.Leader.BotGUIDs`), default to that leader.
    // Rationale: the upstream LLM sometimes omits botGuid on tool calls despite
    // the schema + system-prompt hints. Without a fallback every omission fails
    // with "leader not in world" (for leader tools) or "missing required
    // argument 'botGuid'" (for bot-self-action tools). Since our typical deploy
    // has Claude as the single leader and the majority of MCP traffic
    // originates from his gateway path, defaulting to him on missing botGuid
    // recovers gracefully instead of asking the human to re-phrase.
    //
    // With >1 leader configured we skip the fallback because we can't
    // disambiguate without out-of-band identity (bearer-token→guid mapping,
    // X-Bot-Guid header, etc.) — those are future work.
    void ExtractIdentity(const nlohmann::json& args, uint64_t& botGuid, uint64_t& playerGuid)
    {
        botGuid    = 0;
        playerGuid = 0;
        if (args.is_object())
        {
            if (args.contains("botGuid")    && args["botGuid"].is_number())    botGuid    = args["botGuid"].get<uint64_t>();
            if (args.contains("playerGuid") && args["playerGuid"].is_number()) playerGuid = args["playerGuid"].get<uint64_t>();

            // Grouped facades ({"action":…,"params":{…}}) advertise only action/
            // params/describe, so a model puts the identity INSIDE params. Reading
            // only the top level sent every such call to the leader fallback below
            // ("Raz, follow me" moved Claude) — measured 2026-09-25.
            if (args.contains("params") && args["params"].is_object())
            {
                const auto& inner = args["params"];
                if (botGuid == 0 && inner.contains("botGuid") && inner["botGuid"].is_number())
                    botGuid = inner["botGuid"].get<uint64_t>();
                if (playerGuid == 0 && inner.contains("playerGuid") && inner["playerGuid"].is_number())
                    playerGuid = inner["playerGuid"].get<uint64_t>();
            }
        }
        if (botGuid == 0 && g_McpLeaderBotGUIDSet.size() == 1)
        {
            botGuid = *g_McpLeaderBotGUIDSet.begin();
            LOG_DEBUG("server.loading",
                      "[Ollama Chat MCP] botGuid missing in args — defaulting to configured leader {}",
                      botGuid);
        }
    }

    // Identity arguments the dispatcher supplies for itself. ExtractIdentity above
    // defaults botGuid to the single configured leader when the model omits it, and
    // 168 of the 179 schemas that carry a `required` array name botGuid in it —
    // enforcing those here would reject precisely the calls that fallback exists to
    // rescue. Only these two camelCase spellings are exempt: the tactical family's
    // snake_case `bot_guid` names ANOTHER bot, so it is a real caller-supplied
    // argument and is enforced like any other key.
    bool IsDispatcherSuppliedIdentity(const std::string& key)
    {
        return key == "botGuid" || key == "playerGuid";
    }

    // Enforce the tool's own inputSchema `required` array before dispatch.
    //
    // Until now `required` was advisory: it was serialized into tools/list for the
    // model to read and gated NOTHING, so a call omitting a required argument
    // reached the handler and could land on a bare `args["key"]`. nlohmann's const
    // operator[] on an absent key is UNDEFINED BEHAVIOUR rather than an exception —
    // it JSON_ASSERTs (compiled out under NDEBUG) and then returns *end() — so the
    // catch around DispatchGatewayTool cannot contain it. PR #425 reached exactly
    // that read through bot_buy_item. Rejecting the malformed call here closes the
    // class at one site instead of depending on ~190 per-tool guards staying
    // correct forever.
    //
    // Returns a null json when the call is well-formed.
    nlohmann::json FindMissingRequiredArgument(const std::string& toolName,
                                               const nlohmann::json& args)
    {
        const auto& reg = GetGatewayToolRegistry();
        auto it = reg.find(toolName);
        if (it == reg.end())
            return nlohmann::json();   // unknown tool — DispatchGatewayTool owns that error

        const nlohmann::json& schema = it->second.parametersSchema;
        if (!schema.is_object() || !schema.contains("required"))
            return nlohmann::json();

        const nlohmann::json& required = schema["required"];
        if (!required.is_array())
            return nlohmann::json();

        for (const auto& entry : required)
        {
            if (!entry.is_string())
                continue;
            const std::string key = entry.get<std::string>();
            if (IsDispatcherSuppliedIdentity(key))
                continue;
            if (args.is_object() && args.contains(key))
                continue;

            return nlohmann::json{
                {"error",    "missing_required_argument"},
                {"tool",     toolName},
                {"argument", key},
                {"hint",     "This argument is listed in the tool's inputSchema.required. "
                             "Re-read the schema from tools/list and send it."}};
        }
        return nlohmann::json();
    }

    // Constant-time string compare to avoid leaking bearer-token timing.
    bool ConstTimeEq(const std::string& a, const std::string& b)
    {
        if (a.size() != b.size()) return false;
        unsigned char acc = 0;
        for (size_t i = 0; i < a.size(); ++i)
            acc |= static_cast<unsigned char>(a[i]) ^ static_cast<unsigned char>(b[i]);
        return acc == 0;
    }
}

GatewayMcpServer& GatewayMcpServer::Instance()
{
    static GatewayMcpServer s;
    return s;
}

GatewayMcpServer::~GatewayMcpServer()
{
    Stop();
}

void GatewayMcpServer::RecordToolCall(const std::string& toolName, uint64_t latencyMs,
                                      bool isError, const std::string& errorText)
{
    std::lock_guard<std::mutex> lock(m_toolStatsMutex);
    auto& s = m_toolStats[toolName];
    s.calls += 1;
    s.totalMs += latencyMs;
    if (latencyMs > s.maxMs) s.maxMs = latencyMs;
    if (isError)
    {
        s.errors += 1;
        // Truncate to keep the snapshot readable in-chat. 120 chars is enough to spot
        // "bot not found" / "rate-limited" / "playerbots threw: ..." without wrapping.
        s.lastError = errorText.size() > 120 ? errorText.substr(0, 120) + "…" : errorText;
    }
}

std::vector<McpToolStatSnapshot> GatewayMcpServer::ToolStatsSnapshot() const
{
    std::vector<McpToolStatSnapshot> out;
    {
        std::lock_guard<std::mutex> lock(m_toolStatsMutex);
        out.reserve(m_toolStats.size());
        for (const auto& [name, s] : m_toolStats)
        {
            out.push_back({
                name,
                s.calls,
                s.errors,
                s.calls ? (s.totalMs / s.calls) : 0u,
                s.maxMs,
                s.lastError
            });
        }
    }
    // Sort by calls desc, then by name asc for stable tie-breaking.
    std::sort(out.begin(), out.end(),
              [](const McpToolStatSnapshot& a, const McpToolStatSnapshot& b) {
                  if (a.calls != b.calls) return a.calls > b.calls;
                  return a.name < b.name;
              });
    return out;
}

void GatewayMcpServer::Start()
{
    if (!g_McpEnable)
    {
        if (g_DebugEnabled)
            LOG_INFO("server.loading", "[Ollama Chat MCP] disabled (Mcp.Enable=0)");
        return;
    }

    if (g_McpBearerToken.empty())
    {
        LOG_ERROR("server.loading", "[Ollama Chat MCP] refusing to start: Mcp.Enable=1 but Mcp.BearerToken is empty");
        return;
    }

    if (m_running.load(std::memory_order_acquire))
    {
        if (g_DebugEnabled)
            LOG_INFO("server.loading", "[Ollama Chat MCP] already running, ignoring Start()");
        return;
    }

    m_server = std::make_unique<httplib::Server>();
    m_running.store(true, std::memory_order_release);
    m_thread = std::thread([this]() { RunListener(); });

    LOG_INFO("server.loading", "[Ollama Chat MCP] starting on {}:{}, tool registry size={}",
             g_McpBindAddress, g_McpPort, GetGatewayToolRegistry().size());
}

void GatewayMcpServer::Stop()
{
    if (!m_running.load(std::memory_order_acquire))
        return;

    m_running.store(false, std::memory_order_release);
    if (m_server) m_server->stop();
    if (m_thread.joinable()) m_thread.join();
    m_server.reset();
    LOG_INFO("server.loading", "[Ollama Chat MCP] stopped");
}

void GatewayMcpServer::RunListener()
{
    if (!m_server) return;

    auto& srv = *m_server;

    // Health probe — no auth required so external monitors can verify liveness.
    srv.Get("/mcp/health", [this](const httplib::Request& /*req*/, httplib::Response& res) {
        res.set_content(
            nlohmann::json({
                {"status", "ok"},
                {"running", m_running.load(std::memory_order_relaxed)},
                {"tools",   GetGatewayToolRegistry().size()},
                {"requests", m_totalRequests.load(std::memory_order_relaxed)}
            }).dump(),
            "application/json");
    });

    // Permissive CORS preflight for browser-based MCP clients (Anthropic Claude, others).
    // The bearer token is the actual gate; CORS open is fine.
    srv.Options("/mcp", [](const httplib::Request& /*req*/, httplib::Response& res) {
        res.set_header("Access-Control-Allow-Origin",  "*");
        res.set_header("Access-Control-Allow-Headers", "Authorization, Content-Type");
        res.set_header("Access-Control-Allow-Methods", "POST, OPTIONS");
        res.status = 204;
    });

    srv.Post("/mcp", [this](const httplib::Request& req, httplib::Response& res) {
        m_totalRequests.fetch_add(1, std::memory_order_relaxed);
        res.set_header("Access-Control-Allow-Origin", "*");

        // Bearer auth: required on every request. The token is checked in
        // constant time to avoid timing oracles.
        std::string presentedToken;
        auto authHeader = req.get_header_value("Authorization");
        const std::string bearerPrefix = "Bearer ";
        if (authHeader.rfind(bearerPrefix, 0) == 0)
            presentedToken = authHeader.substr(bearerPrefix.size());

        if (presentedToken.empty() || !ConstTimeEq(presentedToken, g_McpBearerToken))
        {
            m_totalUnauth.fetch_add(1, std::memory_order_relaxed);
            res.status = 401;
            res.set_content(R"({"error":"unauthorized"})", "application/json");
            return;
        }

        // Parse JSON-RPC 2.0 envelope.
        nlohmann::json req_json;
        try { req_json = nlohmann::json::parse(req.body); }
        catch (const std::exception&)
        {
            res.status = 200; // JSON-RPC errors travel in the body, not in HTTP status
            res.set_content(MakeRpcError(nullptr, kRpcParseError, "parse error").dump(), "application/json");
            return;
        }

        nlohmann::json id = req_json.value("id", nlohmann::json(nullptr));
        std::string method = req_json.value("method", std::string{});

        if (method.empty())
        {
            res.set_content(MakeRpcError(id, kRpcInvalidRequest, "missing method").dump(), "application/json");
            return;
        }

        // initialize — handshake. Reply with our tool capability + protocol version.
        if (method == "initialize")
        {
            nlohmann::json result = {
                {"protocolVersion", kMcpProtocolVersion},
                {"capabilities", {
                    {"tools", nlohmann::json::object()}
                }},
                {"serverInfo", {
                    {"name",    "mod-ollama-chat-wow"},
                    {"version", "0.1.0"}
                }}
            };
            res.set_content(MakeRpcResult(id, std::move(result)).dump(), "application/json");
            return;
        }

        // notifications/* are fire-and-forget. The SDK sends `notifications/initialized`
        // after the handshake; we acknowledge with a no-content response.
        if (method.rfind("notifications/", 0) == 0)
        {
            res.status = 204;
            return;
        }

        // tools/list — enumerate the registered tools as MCP tool descriptors.
        // Deterministic alphabetical order (MCP spec: servers SHOULD return tools in the
        // same order across requests) lets clients cache the tool list and improves
        // upstream LLM prompt-cache hit rates.
        if (method == "tools/list")
        {
            const auto& reg = GetGatewayToolRegistry();
            nlohmann::json tools = nlohmann::json::array();
            // Members of a collapsed group, keyed by group. std::map keeps the
            // facade block alphabetical, so the whole list stays deterministic
            // across requests exactly as the comment above requires.
            std::map<std::string, std::vector<std::string>> facadeMembers;
            for (const auto& name : GetGatewayToolNamesSorted())
            {
                // Drop the ops_* proxies when the sidecar is disabled — their
                // handlers return "ops-api tools disabled" unconditionally, so
                // advertising them just inflates a list that clients already
                // struggle to carry in full.
                //
                // leader_* is deliberately NOT filtered here: an MCP caller is not
                // a bot, and supplies the acting botGuid at tools/call time, so
                // leader-ness cannot be decided at list time.
                if (GetGatewayToolScope(name) == GatewayToolScope::Ops && !g_OpsEnable)
                    continue;

                auto it = reg.find(name);
                if (it == reg.end()) continue;

                // A collapsed group is advertised ONCE, as an {action,params}
                // facade named after the group, instead of N schema'd tools.
                // tools/call needs no counterpart change: DispatchGatewayTool
                // already resolves facade+action to the real tool before the
                // handler, rate limiter and audit write run, so all three still
                // see the real name. Every original name also remains callable.
                const std::string group = GetGatewayToolGroup(name);
                if (IsFacadeGroup(group))
                {
                    facadeMembers[group].push_back(name);
                    continue;
                }

                const auto& tool = it->second;

                nlohmann::json descriptor = {
                    {"name",        tool.name},
                    {"description", tool.description},
                    {"inputSchema", tool.parametersSchema}
                };
                // MCP 2025-11-25 annotations (readOnlyHint, destructiveHint, idempotentHint,
                // openWorldHint). Only emit when populated — empty object would still be
                // valid but adds noise.
                if (tool.annotations.is_object() && !tool.annotations.empty())
                    descriptor["annotations"] = tool.annotations;

                tools.push_back(std::move(descriptor));
            }

            // Emit one descriptor per collapsed group. BuildGroupFacade returns the
            // OpenAI function shape the bot-facing array wants; MCP names the same
            // schema `inputSchema`, so reshape rather than duplicate the definition.
            // No annotations: a facade spans many actions whose readOnly/destructive
            // hints differ, and a merged hint would be a lie in one direction or the
            // other. The real tool's own gates still run at dispatch.
            for (const auto& [group, members] : facadeMembers)
            {
                nlohmann::json f = BuildGroupFacade(group, members);
                tools.push_back(nlohmann::json{
                    {"name",        f["function"]["name"]},
                    {"description", f["function"]["description"]},
                    {"inputSchema", f["function"]["parameters"]}
                });
            }

            res.set_content(
                MakeRpcResult(id, nlohmann::json{{"tools", std::move(tools)}}).dump(),
                "application/json");
            return;
        }

        // tools/call — dispatch to the registry, wrap the result as MCP content.
        if (method == "tools/call")
        {
            if (!req_json.contains("params") || !req_json["params"].is_object())
            {
                res.set_content(MakeRpcError(id, kRpcInvalidParams, "params must be an object").dump(), "application/json");
                return;
            }
            const auto& params = req_json["params"];
            std::string toolName = params.value("name", std::string{});
            nlohmann::json args = params.value("arguments", nlohmann::json::object());

            uint64_t botGuid = 0, playerGuid = 0;
            ExtractIdentity(args, botGuid, playerGuid);

            m_totalToolCalls.fetch_add(1, std::memory_order_relaxed);
            auto t0 = std::chrono::steady_clock::now();

            // Validate before dispatch: a missing required argument must never reach a
            // handler, because the read it lands on is UB rather than a catchable throw.
            nlohmann::json result = FindMissingRequiredArgument(toolName, args);
            if (result.is_null())
            {
                try { result = DispatchGatewayTool(botGuid, playerGuid, toolName, args); }
                catch (const std::exception& e) { result = nlohmann::json{{"error", std::string("dispatch threw: ") + e.what()}}; }
            }

            auto latencyMs = std::chrono::duration_cast<std::chrono::milliseconds>(
                std::chrono::steady_clock::now() - t0).count();

            if (g_DebugEnabled)
                LOG_INFO("server.loading", "[Ollama Chat MCP] tools/call name={} bot={} player={} latency={}ms",
                         toolName, botGuid, playerGuid, latencyMs);

            // MCP wraps tool results in a content array. We send the JSON serialized as a
            // single text block — the agent reads it the same way it reads any other
            // text-bodied tool result.
            bool isError = result.is_object() && result.contains("error");

            // Per-tool metrics for `.ollama gateway status`.
            std::string errText;
            if (isError && result["error"].is_string()) errText = result["error"].get<std::string>();
            RecordToolCall(toolName.empty() ? "<unnamed>" : toolName,
                           static_cast<uint64_t>(latencyMs), isError, errText);
            nlohmann::json content = nlohmann::json::array();
            content.push_back({
                {"type", "text"},
                {"text", result.dump()}
            });

            res.set_content(MakeRpcResult(id, nlohmann::json{
                {"content", std::move(content)},
                {"isError", isError}
            }).dump(), "application/json");
            return;
        }

        res.set_content(MakeRpcError(id, kRpcMethodNotFound,
                                     "unknown method: " + method).dump(),
                        "application/json");
    });

    // listen() blocks until stop() is called from another thread. cpp-httplib
    // is thread-safe for this; we call stop() from Stop() above.
    if (!srv.listen(g_McpBindAddress.c_str(), static_cast<int>(g_McpPort)))
    {
        LOG_ERROR("server.loading", "[Ollama Chat MCP] listen({}:{}) failed — port in use?",
                  g_McpBindAddress, g_McpPort);
        m_running.store(false, std::memory_order_release);
    }
}
