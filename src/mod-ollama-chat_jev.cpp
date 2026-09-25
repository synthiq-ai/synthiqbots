#include "mod-ollama-chat_jev.h"
#include "mod-ollama-chat_config.h"

#include <httplib.h>

#include "DatabaseEnv.h"
#include "Log.h"

#include <fmt/core.h>

#include <algorithm>
#include <atomic>
#include <chrono>
#include <condition_variable>
#include <fstream>
#include <memory>
#include <mutex>
#include <regex>
#include <sstream>
#include <future>
#include <thread>

namespace Jev
{
namespace
{
    int64_t NowMs()
    {
        return static_cast<int64_t>(std::chrono::duration_cast<std::chrono::milliseconds>(
            std::chrono::steady_clock::now().time_since_epoch()).count());
    }

    std::string SafeLogSnippet(const std::string& text, size_t maxChars = 240)
    {
        std::string out = text.substr(0, std::min(maxChars, text.size()));
        for (char& c : out)
        {
            unsigned char uc = static_cast<unsigned char>(c);
            if (c == '{') c = '(';
            else if (c == '}') c = ')';
            else if (c == '\n' || c == '\r' || c == '\t' || uc < 0x20) c = ' ';
        }
        if (text.size() > maxChars) out += "...";
        return out;
    }

    // -------------------------------------------------------------------------
    // Client pool. cpp-httplib serialises requests on one Client instance, so
    // the pool leases instances exclusively; keep_alive lets a leased instance
    // reuse its (proxied) TLS connection across calls when the far side keeps
    // it open. A transport failure drops the instance so the next lease
    // reconnects fresh.
    // -------------------------------------------------------------------------
    void DisposeAsync(std::unique_ptr<httplib::Client> cli);

    struct Pool
    {
        std::mutex m;
        std::condition_variable cv;
        struct Idle
        {
            std::unique_ptr<httplib::Client> cli;
            int64_t since = 0;   // NowMs() when it went back to the pool
        };
        std::vector<Idle> idle;
        uint32_t inUse = 0;
        uint32_t cap   = 1;

        // The old generation's pool dies on config_reload (world thread); its
        // idle clients must not run their blocking TLS teardown there either.
        ~Pool()
        {
            for (auto& e : idle) DisposeAsync(std::move(e.cli));
        }
    };

    struct SiteCfg
    {
        bool  enable        = false;
        float minConfidence = 0.0f;
    };

    // Breaker state, owned by the generation whose endpoint it describes, so a
    // request that started against the OLD endpoint cannot trip or reset the
    // NEW one's breaker after a config_reload.
    struct Health
    {
        std::mutex m;
        Breaker    breaker;
    };

    // One immutable snapshot of everything Decide() needs. Swapped atomically
    // on reload; callers mid-request hold their own shared_ptr.
    struct Generation
    {
        bool        enable = false;
        std::string url;              // as configured
        std::string schemeHostPort;   // "https://openrouter.ai"
        std::string path;             // "/api/alpha/decisions"
        std::string model;
        std::string apiKey;
        std::string proxyHost;
        int         proxyPort = 0;
        std::string proxyUser;
        std::string proxyPass;
        uint32_t    timeoutMs = 1500;
        std::map<std::string, SiteCfg> sites;
        nlohmann::json questions;     // parsed prompts/jev_questions.json (object) or null
        std::shared_ptr<Pool>   pool;
        std::shared_ptr<Health> health;
        std::string invalidReason;    // non-empty => Decide() refuses with this
        ParsedPlaybook playbook;      // Mcp.Leader.SystemPromptFile split into header/rows/footer (PR 3)
        size_t   playbookBytes   = 0; // size of the unfiltered text this generation parsed (log only)
        float    playbookMinRel  = 0.3f;
        uint32_t playbookMinRows = 4;
        uint32_t playbookMaxRows = 12;
        uint32_t playbookTimeoutMs = 4000;
    };

    std::mutex g_genMutex;
    std::shared_ptr<const Generation> g_gen;

    std::shared_ptr<const Generation> CurrentGen()
    {
        std::lock_guard<std::mutex> lock(g_genMutex);
        return g_gen;
    }

    std::atomic<bool> g_auditColumnsReady{false};

    bool SplitUrl(const std::string& url, std::string& schemeHostPort, std::string& path, std::string& err)
    {
        static const std::regex urlRegex(R"(^(https?)://([^:/]+)(?::(\d+))?(/.*)?$)");
        std::smatch m;
        if (!std::regex_match(url, m, urlRegex))
        {
            err = "invalid URL '" + url + "'";
            return false;
        }
        schemeHostPort = m[1].str() + "://" + m[2].str();
        if (m[3].matched)
        {
            // httplib::Client's own parser does std::stoi on this and throws on
            // overflow; refuse it here so an invalid conf never reaches a request.
            const std::string port = m[3].str();
            if (port.size() > 5 || std::stoul(port) == 0 || std::stoul(port) > 65535)
            {
                err = "invalid port in URL '" + url + "'";
                return false;
            }
            schemeHostPort += ":" + port;
        }
        path = m[4].matched && !m[4].str().empty() ? m[4].str() : std::string("/");
        return true;
    }

    // "http://host:port" | "host:port" | "host" -> host + port (default 8888, gluetun's).
    bool SplitProxy(const std::string& proxy, std::string& host, int& port, std::string& err)
    {
        std::string p = proxy;
        const auto scheme = p.find("://");
        if (scheme != std::string::npos) p = p.substr(scheme + 3);
        while (!p.empty() && p.back() == '/') p.pop_back();
        const auto colon = p.rfind(':');
        if (colon == std::string::npos)
        {
            host = p; port = 8888;
        }
        else
        {
            host = p.substr(0, colon);
            try { port = std::stoi(p.substr(colon + 1)); }
            catch (...) { err = "invalid proxy port in '" + proxy + "'"; return false; }
        }
        if (host.empty() || port <= 0 || port > 65535)
        {
            err = "invalid proxy '" + proxy + "'";
            return false;
        }
        return true;
    }

    nlohmann::json LoadQuestionsFile(const std::string& path)
    {
        if (path.empty()) return nullptr;
        // Same candidate roots the prompt files use (cwd-relative, /azerothcore/…).
        std::vector<std::string> candidates{path};
        if (!path.empty() && path.front() != '/')
            candidates.push_back("/azerothcore/" + path);
        for (const std::string& candidate : candidates)
        {
            std::ifstream ifs(candidate);
            if (!ifs.is_open()) continue;
            std::stringstream ss;
            ss << ifs.rdbuf();
            try
            {
                nlohmann::json j = nlohmann::json::parse(ss.str());
                if (!j.is_object())
                {
                    LOG_WARN("server.loading", "[Ollama Chat Jev] questions file '{}' is not a JSON object", candidate);
                    return nullptr;
                }
                LOG_INFO("server.loading", "[Ollama Chat Jev] loaded questions file '{}' ({} sites)", candidate, j.size());
                return j;
            }
            catch (const std::exception& e)
            {
                LOG_WARN("server.loading", "[Ollama Chat Jev] questions file '{}' parse error: {}", candidate, e.what());
                return nullptr;
            }
        }
        LOG_WARN("server.loading", "[Ollama Chat Jev] questions file '{}' not found", path);
        return nullptr;
    }

    std::unique_ptr<httplib::Client> MakeClient(const Generation& gen)
    {
        auto cli = std::make_unique<httplib::Client>(gen.schemeHostPort);
#ifdef CPPHTTPLIB_OPENSSL_SUPPORT
        // Matches the module-wide behaviour in OllamaHttpClient (self-signed /
        // ngrok endpoints); documented in docs/jev.md as a known trade-off.
        cli->enable_server_certificate_verification(false);
#endif
        cli->set_keep_alive(true);
        if (!gen.proxyHost.empty())
        {
            cli->set_proxy(gen.proxyHost, gen.proxyPort);
            if (!gen.proxyUser.empty())
                cli->set_proxy_basic_auth(gen.proxyUser, gen.proxyPass);
        }
        return cli;
    }

    // A pooled connection idle longer than this is not reused. Measured
    // 2026-09-23 once keep-alive actually worked: a connection idle ~25 s
    // between two talk_to_leader turns was dead ("Failed to read connection
    // after 8037ms") — the path from game-host silently drops idle flows (see the
    // ISP blackhole note), so httplib's FIN-based liveness check cannot see
    // it and the read waits out the whole budget. 12 s sits above tactical's
    // 10 s heartbeat with margin, so a running game keeps reusing one hot
    // connection; anything staler reconnects (~0.6 s TLS).
    constexpr int64_t kMaxIdleMs = 12000;

    // Destroying a client that holds a TLS socket runs a graceful
    // SSL_shutdown that blocks on the peer's close_notify up to the socket
    // read timeout — seconds, on a dead or unresponsive peer. Never pay that
    // on the caller's thread (world thread for intent, tick thread for
    // tactical, the chat worker for the playbook): hand it to a detached
    // thread that owns nothing else.
    void DisposeAsync(std::unique_ptr<httplib::Client> cli)
    {
        if (!cli) return;
        try { std::thread([c = std::move(cli)]() mutable { c.reset(); }).detach(); }
        catch (...) { /* thread creation failed: the unique_ptr destroys it here, synchronously */ }
    }

    void Release(const Generation& gen, std::unique_ptr<httplib::Client> cli, bool keep)
    {
        Pool& pool = *gen.pool;
        {
            std::lock_guard<std::mutex> lock(pool.m);
            if (pool.inUse > 0) --pool.inUse;
            if (keep && cli) pool.idle.push_back(Pool::Idle{std::move(cli), NowMs()});
        }
        pool.cv.notify_one();
        DisposeAsync(std::move(cli));   // no-op when it was pooled above
    }

    // RAII lease: the slot is ALWAYS given back — on every return path and on
    // any exception thrown by client construction, serialisation or the request
    // itself. `keep=false` drops the instance so the next lease reconnects.
    struct LeaseGuard
    {
        const Generation& gen;
        std::unique_ptr<httplib::Client> cli;
        bool keep = false;
        bool held = false;

        LeaseGuard(const Generation& g, uint32_t waitMs) : gen(g)
        {
            Pool& pool = *gen.pool;
            std::unique_lock<std::mutex> lock(pool.m);
            if (!pool.cv.wait_for(lock, std::chrono::milliseconds(waitMs),
                                  [&] { return pool.inUse < pool.cap; }))
                return;
            ++pool.inUse;
            held = true;
            const int64_t now = NowMs();
            std::vector<std::unique_ptr<httplib::Client>> stale;
            while (!pool.idle.empty())
            {
                Pool::Idle entry = std::move(pool.idle.back());
                pool.idle.pop_back();
                if (now - entry.since <= kMaxIdleMs) { cli = std::move(entry.cli); break; }
                stale.push_back(std::move(entry.cli));
            }
            lock.unlock();
            for (auto& s : stale) DisposeAsync(std::move(s));
            if (cli) return;
            try { cli = MakeClient(gen); }
            catch (...) { cli.reset(); }   // slot still released by the destructor
        }
        ~LeaseGuard()
        {
            // Decide `keep` BEFORE the move. Function arguments are evaluated in
            // an unspecified order, and both clang and gcc move-construct the
            // by-value unique_ptr parameter first — so `cli != nullptr` read the
            // moved-from pointer, `keep` was always false, and every call threw
            // its pooled client away. Destroying a live TLS client runs a
            // graceful SSL_shutdown that waits for the peer's close_notify up to
            // the socket read timeout (= the call budget), so each jev call
            // blocked its thread for its WHOLE budget after the answer had
            // already arrived: measured 2026-09-23, playbook filter answer at
            // +0.82 s, request sent to the gateway at +4.8 s (budget 4000 ms).
            if (!held) return;
            const bool reuse = keep && cli != nullptr;
            Release(gen, std::move(cli), reuse);
        }
        LeaseGuard(const LeaseGuard&) = delete;
        LeaseGuard& operator=(const LeaseGuard&) = delete;
    };

    void NoteFailure(const Generation& gen, const char* site, const std::string& why)
    {
        bool tripped = false;
        int64_t cooldownMs = 0;
        {
            std::lock_guard<std::mutex> lock(gen.health->m);
            tripped = gen.health->breaker.RecordFailure(NowMs());
            cooldownMs = gen.health->breaker.cooldownMs;
        }
        if (tripped)
            LOG_WARN("server.loading",
                     "[Ollama Chat Jev] breaker OPEN for {}s after repeated failures (last: site={} {}) — every path falls through to its LLM tier at zero cost",
                     cooldownMs / 1000, site, SafeLogSnippet(why));
    }

    void NoteSuccess(const Generation& gen)
    {
        std::lock_guard<std::mutex> lock(gen.health->m);
        gen.health->breaker.RecordSuccess();
    }
} // namespace

bool Enabled()
{
    auto gen = CurrentGen();
    return gen && gen->enable && gen->invalidReason.empty();
}

bool EnabledFor(const char* site)
{
    auto gen = CurrentGen();
    if (!gen || !gen->enable || !gen->invalidReason.empty() || !site) return false;
    auto it = gen->sites.find(site);
    return it != gen->sites.end() && it->second.enable;
}

float MinConfidenceFor(const char* site)
{
    auto gen = CurrentGen();
    if (!gen || !site) return 0.0f;
    auto it = gen->sites.find(site);
    return it == gen->sites.end() ? 0.0f : it->second.minConfidence;
}

nlohmann::json QuestionTemplate(const char* site, const char* id)
{
    auto gen = CurrentGen();
    if (!gen || !gen->questions.is_object() || !site) return nullptr;
    if (!gen->questions.contains(site)) return nullptr;
    const nlohmann::json& s = gen->questions[site];
    if (!id) return s;
    if (!s.is_object() || !s.contains(id)) return nullptr;
    return s[id];
}

Result Decide(const nlohmann::json& state, const nlohmann::json& questions,
              uint32_t timeoutMs, const char* site)
{
    Result out;
    const char* siteLabel = site ? site : "?";

    auto gen = CurrentGen();
    if (!gen || !gen->enable)          { out.error = "disabled"; return out; }
    if (!gen->invalidReason.empty())   { out.error = gen->invalidReason; return out; }
    if (timeoutMs == 0)                { out.error = "no budget left"; return out; }
    {
        auto s = gen->sites.find(siteLabel);
        out.minConfidence = s == gen->sites.end() ? 0.0f : s->second.minConfidence;
    }

    {
        std::lock_guard<std::mutex> lock(gen->health->m);
        if (gen->health->breaker.IsOpen(NowMs())) { out.error = "breaker_open"; return out; }
    }

    // The caller's cap is the WHOLE call: lease wait + connect + TLS + request.
    // Lease is bounded (a saturated pool is a fast fall-through, not a stall on
    // whatever thread the caller is on — intent runs on the world thread) and
    // whatever it consumed comes off the transport budget; set_max_timeout then
    // bounds the request end-to-end so connect+read cannot serially exceed it.
    const int64_t t0 = NowMs();
    LeaseGuard lease(*gen, std::min<uint32_t>(200u, timeoutMs));
    if (!lease.cli)
    {
        out.error = lease.held ? "client_unavailable" : "pool_busy";
        out.latencyMs = static_cast<uint32_t>(NowMs() - t0);
        return out;
    }
    const int64_t leaseWaitMs = NowMs() - t0;
    if (leaseWaitMs >= static_cast<int64_t>(timeoutMs))
    {
        out.error = "no budget left after lease";
        out.latencyMs = static_cast<uint32_t>(leaseWaitMs);
        return out;
    }
    const uint32_t budgetMs  = timeoutMs - static_cast<uint32_t>(leaseWaitMs);
    // httplib bounds the TCP connect wait AND each SSL_connect wait with the
    // connection timeout, NOT with set_max_timeout (which only caps the
    // request/response phase). With 3000 ms here a flapping path to AWS
    // (api.typesafe.ai: 2 A records) spent ~9.4 s before failing a 4000 ms
    // playbook budget — measured 2026-09-24, 8 of ~20 calls in one burst — and
    // the fallback then sent the full 197 KB playbook to the gateway. Healthy
    // connect and TLS from game-host are ~0.3 s each, so 1000 ms is 3x headroom
    // and bounds a dead path to roughly the budget instead of 2.3x it.
    // Never above the remaining budget (a small cap must stay small).
    const uint32_t connectMs = std::min<uint32_t>(
        budgetMs, std::min<uint32_t>(std::max<uint32_t>(300u, budgetMs / 4), 1000u));

    httplib::Result res;
    try
    {
        lease.cli->set_connection_timeout(std::chrono::milliseconds(connectMs));
        lease.cli->set_read_timeout(std::chrono::milliseconds(budgetMs));
        lease.cli->set_write_timeout(std::chrono::milliseconds(budgetMs));
        lease.cli->set_max_timeout(std::chrono::milliseconds(budgetMs));

        httplib::Headers headers{
            {"Authorization", "Bearer " + gen->apiKey},
            {"HTTP-Referer", "https://github.com/synthiq-ai/synthiqbots"},
            {"X-Title", "mod-ollama-chat"},
        };
        const std::string body = BuildRequestBody(gen->model, state, questions).dump();
        res = lease.cli->Post(gen->path, headers, body, "application/json");
    }
    catch (const std::exception& e)
    {
        // "Never throws" is part of the contract: an invalid-UTF-8 dump(), a
        // client-side parse quirk, anything — becomes a failed result, the slot
        // is released by the guard, and the breaker sees it like a transport error.
        out.error = std::string("exception: ") + e.what();
        out.latencyMs = static_cast<uint32_t>(NowMs() - t0);
        NoteFailure(*gen, siteLabel, out.error);
        LOG_WARN("server.loading", "[Ollama Chat Jev] site={} {} after {}ms", siteLabel, out.error, out.latencyMs);
        return out;
    }
    out.latencyMs = static_cast<uint32_t>(NowMs() - t0);

    if (!res)
    {
        out.error = std::string("transport: ") + httplib::to_string(res.error());
        lease.keep = false;   // reconnect next time
        NoteFailure(*gen, siteLabel, out.error);
        LOG_WARN("server.loading", "[Ollama Chat Jev] site={} {} after {}ms", siteLabel, out.error, out.latencyMs);
        return out;
    }
    lease.keep = true;

    if (res->status != 200)
    {
        nlohmann::json env;
        try { env = nlohmann::json::parse(res->body); } catch (...) {}
        std::string msg = ExtractErrorMessage(env);
        out.error = "http " + std::to_string(res->status) + (msg.empty() ? "" : ": " + msg);
        NoteFailure(*gen, siteLabel, out.error);
        LOG_WARN("server.loading", "[Ollama Chat Jev] site={} {} after {}ms body=\"{}\"",
                 siteLabel, out.error, out.latencyMs, SafeLogSnippet(res->body));
        return out;
    }

    const uint32_t latency = out.latencyMs;
    const float    minConf = out.minConfidence;
    if (!ParseResponse(res->body, out) || !ValidateAgainstQuestions(questions, out))
    {
        out.latencyMs = latency;
        out.minConfidence = minConf;
        NoteFailure(*gen, siteLabel, out.error);
        LOG_WARN("server.loading", "[Ollama Chat Jev] site={} bad response: {} body=\"{}\"",
                 siteLabel, out.error, SafeLogSnippet(res->body));
        return out;
    }
    out.latencyMs = latency;
    out.minConfidence = minConf;
    NoteSuccess(*gen);

    // One line per call: enough to reconstruct the decision, never the state text.
    std::string summary;
    for (const auto& kv : out.answers)
    {
        if (!summary.empty()) summary += " ";
        const Answer& a = kv.second;
        if (a.type == "choice")      summary += kv.first + "=" + a.choice + "(" + std::to_string(a.confidence).substr(0, 4) + ")";
        else if (a.type == "noul")   summary += kv.first + "=p" + std::to_string(a.noul).substr(0, 4);
        else                         summary += kv.first + "=s" + std::to_string(a.score).substr(0, 4);
    }
    LOG_INFO("server.loading",
             "[Ollama Chat Jev] site={} {} latency={}ms in_tok={} cost=${:.6f} model={}",
             siteLabel, summary, out.latencyMs, out.inputTokens, out.costUsd, out.model);
    return out;
}

void OnConfigReloaded()
{
    auto gen = std::make_shared<Generation>();
    gen->enable    = g_JevEnable;
    gen->url       = g_JevUrl;
    gen->model     = g_JevModel;
    gen->apiKey    = g_JevApiKey;
    gen->proxyUser = g_JevProxyUser;
    gen->proxyPass = g_JevProxyPassword;
    gen->timeoutMs = g_JevTimeoutMs == 0 ? 1500u : g_JevTimeoutMs;
    gen->sites[kSiteClassifier] = SiteCfg{g_JevClassifierEnable, g_JevClassifierMinConfidence};
    gen->sites[kSiteIntent]     = SiteCfg{g_JevIntentEnable,     g_JevIntentMinConfidence};
    gen->sites[kSiteTickGate]   = SiteCfg{g_JevTickGateEnable,   g_JevTickGateMinConfidence};
    gen->sites[kSitePlanner]    = SiteCfg{g_JevPlannerEnable,    g_JevPlannerMinConfidence};
    gen->sites[kSiteTactical]   = SiteCfg{g_JevTacticalEnable,   g_JevTacticalMinConfidence};
    gen->sites[kSitePlaybook]   = SiteCfg{g_JevPlaybookEnable,   0.0f};
    gen->playbook        = ParsePlaybook(g_McpLeaderSystemPromptText);   // loaded earlier in LoadOllamaChatConfig
    gen->playbookBytes   = g_McpLeaderSystemPromptText.size();
    gen->playbookMinRel  = g_JevPlaybookMinRelevance;
    gen->playbookMinRows = g_JevPlaybookMinRows;
    gen->playbookMaxRows = g_JevPlaybookMaxRows;
    gen->playbookTimeoutMs = g_JevPlaybookTimeoutMs == 0 ? 4000u : g_JevPlaybookTimeoutMs;

    gen->pool = std::make_shared<Pool>();
    gen->pool->cap = std::max<uint32_t>(1u, g_JevMaxConcurrent);

    // Fresh breaker per generation: a reload that fixes the endpoint must not
    // inherit the old one's open state, and an in-flight request against the
    // old endpoint must not trip the new one's (it holds the old Health).
    gen->health = std::make_shared<Health>();
    gen->health->breaker.cooldownMs = static_cast<int64_t>(std::max<uint32_t>(5u, g_JevBreakerCooldownSec)) * 1000;

    if (gen->enable)
    {
        std::string err;
        if (gen->apiKey.empty())                                     gen->invalidReason = "Jev.ApiKey is empty";
        else if (gen->model.empty())                                 gen->invalidReason = "Jev.Model is empty";
        else if (!SplitUrl(gen->url, gen->schemeHostPort, gen->path, err)) gen->invalidReason = err;
        else if (!g_JevProxy.empty() && !SplitProxy(g_JevProxy, gen->proxyHost, gen->proxyPort, err)) gen->invalidReason = err;

        gen->questions = LoadQuestionsFile(g_JevQuestionsFile);
        if (gen->invalidReason.empty() && !gen->questions.is_object())
            gen->invalidReason = "questions file missing or invalid (" + g_JevQuestionsFile + ")";
    }

    {
        std::lock_guard<std::mutex> lock(g_genMutex);
        g_gen = gen;   // old generation lives on in any in-flight caller's shared_ptr
    }

    if (!gen->enable)
    {
        LOG_INFO("server.loading", "[Ollama Chat Jev] disabled (Jev.Enable=0)");
        return;
    }
    if (!gen->invalidReason.empty())
    {
        LOG_ERROR("server.loading", "[Ollama Chat Jev] enabled but unusable: {} — every path stays on its LLM tier",
                  gen->invalidReason);
        return;
    }
    LOG_INFO("server.loading",
             "[Ollama Chat Jev] config loaded: url={} model={} proxy={} timeout={}ms concurrency={} "
             "sites: classifier={}({:.2f}) intent={}({:.2f}) tickgate={}({:.2f}) planner={}({:.2f}) tactical={}({:.2f}, escalate>={:.2f}) "
             "playbook={}(rel>={:.2f}, rows {}..{} of {})",
             gen->url, gen->model, gen->proxyHost.empty() ? "none" : gen->proxyHost + ":" + std::to_string(gen->proxyPort),
             gen->timeoutMs, gen->pool->cap,
             g_JevClassifierEnable, g_JevClassifierMinConfidence,
             g_JevIntentEnable, g_JevIntentMinConfidence,
             g_JevTickGateEnable, g_JevTickGateMinConfidence,
             g_JevPlannerEnable, g_JevPlannerMinConfidence,
             g_JevTacticalEnable, g_JevTacticalMinConfidence, g_JevTacticalEscalateMin,
             g_JevPlaybookEnable, g_JevPlaybookMinRelevance, g_JevPlaybookMinRows, g_JevPlaybookMaxRows, gen->playbook.rows.size());
}

bool FilterLeaderPlaybook(const std::string& message,
                          std::string& out, size_t& keptRows, size_t& totalRows)
{
    keptRows = 0;
    totalRows = 0;
    auto gen = CurrentGen();
    if (!gen || !gen->enable || !gen->invalidReason.empty()) return false;
    auto site = gen->sites.find(kSitePlaybook);
    if (site == gen->sites.end() || !site->second.enable) return false;
    const ParsedPlaybook& pb = gen->playbook;
    totalRows = pb.rows.size();
    if (pb.rows.empty() || message.empty()) return false;

    // One Noul per row. The shared framing (question wording, what counts as
    // yes/no) goes ONCE into the state — every question can read it — and each
    // Noul carries only its row's trigger phrases. Measured 2026-09-21: with the
    // framing repeated per Noul the request was 18.5k input tokens; this shape
    // is what keeps it in the low thousands. Wording comes from the questions
    // file so it can be tuned without a rebuild.
    nlohmann::json tmpl = gen->questions.is_object() && gen->questions.contains(kSitePlaybook)
                              && gen->questions[kSitePlaybook].is_object()
                              && gen->questions[kSitePlaybook].contains("row")
                            ? gen->questions[kSitePlaybook]["row"] : nlohmann::json(nullptr);
    // Type-checked reads: json::value() throws on a same-key non-string
    // ("true": true), and that would escape before Decide's exception guard.
    auto str = [&](const char* key, const char* dflt) -> std::string {
        if (tmpl.is_object() && tmpl.contains(key) && tmpl[key].is_string())
            return tmpl[key].get<std::string>();
        return dflt;
    };
    std::string question  = str("question", "Is the workflow triggered by these example phrases what the player is asking for, or closely related to it?");
    std::string trueDesc  = str("true",     "the message asks for this workflow, a variant of it, or something this workflow is needed to answer");
    std::string falseDesc = str("false",    "unrelated: a different order, small talk, a question about something else");

    nlohmann::json state{
        {"message",   message},
        {"task",      question},
        {"yes_means", trueDesc},
        {"no_means",  falseDesc},
    };
    // Split the rows into request chunks whose whole serialized body stays
    // under kPlaybookMaxBodyBytes, sent in parallel (bounded by the pool). Measured 2026-09-24: from game-host's direct
    // ISP route, POST bodies above ~21 KB to api.typesafe.ai stall until the
    // read timeout (21 KB ok in 1.4 s, 25 KB never answered), while the same
    // body works from other networks. It is not
    // MSS (a 1300-byte MSS still stalls) and TypeSafe rejects gzip bodies.
    // The full 78-row question set is ~26 KB, so a single request failed
    // intermittently at ~9.4 s and fell back to the whole 197 KB playbook.
    // Budget against the WHOLE serialized request (state + envelope + model),
    // with margin under the ~21 KB stall. A turn whose shared state alone, or a
    // single row, cannot fit falls back like any failed filter (codex on the
    // first cut: a 6 KB player message pushed a chunk to ~22.5 KB).
    constexpr size_t kPlaybookMaxBodyBytes = 18 * 1024;
    std::vector<Result> results;
    try
    {
        const size_t baseBytes = BuildRequestBody(gen->model, state, nlohmann::json::object()).dump().size();
        if (baseBytes + 1024 > kPlaybookMaxBodyBytes)
        {
            LOG_WARN("server.loading", "[Ollama Chat Jev] playbook filter skipped: shared state is {} bytes, no room for questions", baseBytes);
            return false;
        }
        const size_t perChunk = kPlaybookMaxBodyBytes - baseBytes;
        std::vector<nlohmann::json> chunks(1, nlohmann::json::object());
        size_t chunkBytes = 0;
        for (size_t i = 0; i < pb.rows.size(); ++i)
        {
            std::string id = "r" + std::to_string(i);
            nlohmann::json q = Noul(nlohmann::json{{"workflow_triggers", pb.rows[i].prefix}});
            const size_t qBytes = q.dump().size() + id.size() + 4;   // "id":…,
            if (qBytes > perChunk)
            {
                LOG_WARN("server.loading", "[Ollama Chat Jev] playbook filter skipped: row {} alone is {} bytes", i, qBytes);
                return false;
            }
            if (chunkBytes > 0 && chunkBytes + qBytes > perChunk)
            {
                chunks.emplace_back(nlohmann::json::object());
                chunkBytes = 0;
            }
            chunks.back()[id] = std::move(q);
            chunkBytes += qBytes;
        }

        // Never more chunks in flight than the pool has slots: a lease that
        // finds none free fails as pool_busy after 200 ms. 1 slot -> sequential.
        const size_t parallel = std::max<size_t>(1, std::min<size_t>(2, gen->pool ? gen->pool->cap : 1));
        // ONE deadline for the whole filter: sequential batches share the
        // playbook budget instead of each getting a fresh one, and the first
        // failed batch stops dispatch (the filter falls back regardless).
        const int64_t deadline = NowMs() + static_cast<int64_t>(gen->playbookTimeoutMs);
        for (size_t start = 0; start < chunks.size(); start += parallel)
        {
            const int64_t remaining = deadline - NowMs();
            if (remaining <= 0)
            {
                LOG_WARN("server.loading", "[Ollama Chat Jev] playbook filter out of budget after {} of {} chunks", start, chunks.size());
                return false;
            }
            const uint32_t budgetMs = static_cast<uint32_t>(remaining);
            const size_t end = std::min(chunks.size(), start + parallel);
            const size_t firstOfBatch = results.size();
            if (end - start == 1)
            {
                results.push_back(Decide(state, chunks[start], budgetMs, kSitePlaybook));
            }
            else
            {
                std::vector<std::future<Result>> pending;
                for (size_t c = start; c < end; ++c)
                    pending.push_back(std::async(std::launch::async,
                        [&state, &chunks, c, budgetMs] { return Decide(state, chunks[c], budgetMs, kSitePlaybook); }));
                for (auto& f : pending) results.push_back(f.get());
            }
            for (size_t k = firstOfBatch; k < results.size(); ++k)
                if (!results[k].ok) return false;   // logged by Decide; caller sends the full playbook
        }
    }
    catch (const std::exception& e)
    {
        // Invalid UTF-8 in a trigger (dump() throws) or thread creation failure:
        // behave like any failed filter so the caller keeps the full playbook.
        LOG_WARN("server.loading", "[Ollama Chat Jev] playbook filter failed before/at dispatch: {}", e.what());
        return false;
    }
    uint32_t latencyMs = 0, inputTokens = 0;
    for (const auto& r : results)
    {
        if (!r.ok) return false;   // logged by Decide; caller sends the full playbook
        latencyMs = std::max(latencyMs, r.latencyMs);
        inputTokens += r.inputTokens;
    }

    std::vector<float> relevance(pb.rows.size(), 0.0f);
    for (size_t i = 0; i < pb.rows.size(); ++i)
        for (const auto& r : results)
            if (const Answer* a = r.Find("r" + std::to_string(i))) { relevance[i] = a->noul; break; }
    std::vector<size_t> keep = SelectPlaybookRows(relevance, gen->playbookMinRel,
                                                  gen->playbookMinRows, gen->playbookMaxRows);
    out = RenderPlaybook(pb, keep);
    keptRows = keep.size();

    std::string top;
    for (size_t k = 0; k < keep.size() && k < 4; ++k)
    {
        if (!top.empty()) top += " ";
        top += "r" + std::to_string(keep[k]) + "=p" + std::to_string(relevance[keep[k]]).substr(0, 4);
    }
    LOG_INFO("server.loading", "[Ollama Chat Jev] playbook filter kept {}/{} rows ({} -> {} chars) latency={}ms in_tok={} chunks={} top: {}",
             keptRows, totalRows, gen->playbookBytes, out.size(), latencyMs, inputTokens, results.size(), top);
    return true;
}

// ---------------------------------------------------------------------------
// Audit-column self-migration. The module's SQL under data/sql/characters/
// updates/ is documentation only: the worldserver image bakes
// AC_UPDATES_ENABLE_DATABASES=0 (db-import owns updates and only runs at first
// setup), so an ALTER shipped there never reaches a live DB. Feature-detect
// via INFORMATION_SCHEMA and apply idempotently instead.
// ---------------------------------------------------------------------------
namespace
{
    bool ColumnExists(const char* table, const char* column)
    {
        // Identifiers are compile-time literals from EnsureAuditBackendColumns, never user input.
        QueryResult r = CharacterDatabase.Query(fmt::format(
            "SELECT COUNT(*) FROM INFORMATION_SCHEMA.COLUMNS "
            "WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = '{}' AND COLUMN_NAME = '{}'",
            table, column));
        if (!r) return false;
        return (*r)[0].Get<uint64>() > 0;
    }

    bool EnsureColumn(const char* table, const char* column, const char* ddl)
    {
        if (ColumnExists(table, column)) return true;
        LOG_INFO("server.loading", "[Ollama Chat Jev] migrating: ALTER TABLE {} ADD COLUMN {} {}", table, column, ddl);
        CharacterDatabase.DirectExecute(fmt::format("ALTER TABLE {} ADD COLUMN {} {}", table, column, ddl));
        const bool ok = ColumnExists(table, column);
        if (!ok)
            LOG_ERROR("server.loading", "[Ollama Chat Jev] migration of {}.{} did not take — audit rows keep the legacy column list",
                      table, column);
        return ok;
    }
}

void EnsureAuditBackendColumns()
{
    bool ok = true;
    ok = EnsureColumn("mod_ollama_chat_gateway_audit",  "backend",       "VARCHAR(8) NULL") && ok;
    ok = EnsureColumn("mod_ollama_chat_tactical_audit", "backend",       "VARCHAR(8) NULL") && ok;
    ok = EnsureColumn("mod_ollama_chat_tactical_audit", "prompt_tokens", "INT UNSIGNED NOT NULL DEFAULT 0") && ok;
    g_auditColumnsReady.store(ok, std::memory_order_release);
    LOG_INFO("server.loading", "[Ollama Chat Jev] audit backend columns {}", ok ? "ready" : "NOT ready");
}

bool AuditColumnsReady()
{
    return g_auditColumnsReady.load(std::memory_order_acquire);
}

} // namespace Jev
