// Proactive Leader Bot — LLM glue (Phase A: local-Ollama intent classifier).
// Companion to `mod-ollama-chat_proactive.cpp`. See `_proactive_llm.h` for
// the public surface and rationale.

#include "mod-ollama-chat_proactive_llm.h"
#include "mod-ollama-chat_config.h"
#include "mod-ollama-chat_gateway.h"
#include "mod-ollama-chat_httpclient.h"
#include "mod-ollama-chat_jev.h"
#include "mod-ollama-chat_llmwire.h"
#include "mod-ollama-chat-utilities.h"

#include "Log.h"

#include <nlohmann/json.hpp>

#include <algorithm>
#include <cctype>
#include <chrono>

namespace ollamachat
{
namespace proactive
{
namespace llm
{

namespace
{

IntentLabel ParseIntentString(std::string const& s)
{
    // Tolerate case + leading/trailing whitespace.
    std::string lo;
    lo.reserve(s.size());
    for (char ch : s)
    {
        unsigned char uc = static_cast<unsigned char>(ch);
        if (std::isspace(uc)) continue;
        lo.push_back(static_cast<char>(std::tolower(uc)));
    }
    if (lo == "approve") return IntentLabel::Approve;
    if (lo == "reject")  return IntentLabel::Reject;
    if (lo == "done")    return IntentLabel::Done;
    if (lo == "other")   return IntentLabel::Other;
    return IntentLabel::None;
}

uint64_t NowMs()
{
    return static_cast<uint64_t>(
        std::chrono::duration_cast<std::chrono::milliseconds>(
            std::chrono::steady_clock::now().time_since_epoch()).count());
}

// jev per-site caps — each is the slice of the path's budget jev may spend so
// that a miss still leaves the LLM tier a useful remainder. Intent runs on the
// WORLD thread under the proactive mutex (handler chat hook), hence the
// tightest cap. Also clamped by Jev.TimeoutMs and by the live remainder.
// Measured live 2026-09-21: a jev round trip from the worldserver is 1.25-1.6 s,
// so the first caps (1000 / 1200) could NEVER succeed — tick-gate timed out on
// every heartbeat and its timeouts tripped the shared breaker for all sites.
// A cap below the network floor is worse than no jev tier. Both stay inside
// their chain budgets (intent 3000 ms on the world thread, tick-gate 2000 ms
// fail-open); after a jev timeout the LLM tier usually no longer fits and the
// chain drops to the deterministic tier.
constexpr uint32_t kJevIntentCapMs   = 1700;
constexpr uint32_t kJevTickGateCapMs = 1800;
constexpr uint32_t kJevPlannerCapMs  = 1500;
// Below these remainders the LLM tier is not started (it would only time out).
constexpr uint32_t kLlmTierMinIntentMs   = 1200;
constexpr uint32_t kLlmTierMinTickGateMs = 1000;

uint32_t JevCapFor(uint32_t siteCapMs, int64_t remainingMs)
{
    uint32_t configured = g_JevTimeoutMs == 0 ? siteCapMs : std::min(g_JevTimeoutMs, siteCapMs);
    return Jev::JevCapMs(configured, remainingMs);
}

// Compact audit string for a jev answer: the row stores only its length, the
// worldserver.log line carries it verbatim.
std::string JevAuditContent(const char* id, const Jev::Answer& a, const Jev::Result& r)
{
    nlohmann::json j{{id, a.choice}, {"confidence", a.confidence}, {"backend", Jev::kBackendJev},
                     {"model", r.model}, {"in_tok", r.inputTokens}};
    return j.dump();
}

}  // namespace

IntentResult ClassifyIntent(uint64_t /*botGuid*/, uint64_t /*playerGuid*/,
                            std::string const& message,
                            bool hasOpenProposal, bool hasExecutingPlan)
{
    IntentResult out;
    out.minConfidence = g_ProactiveIntentMinConfidence;

    if (!g_ProactiveIntentEnable) return out;
    if (g_ProactiveIntentPromptText.empty()) return out;
    if (message.empty() || message.size() > 256) return out;
    if (!hasOpenProposal && !hasExecutingPlan) return out;

    // One absolute deadline for the whole chain (jev -> LLM -> caller's
    // deterministic matcher). Never grows: this runs on the world thread.
    const uint32_t budgetMs = g_ProactiveIntentTimeoutMs == 0 ? 3000u : g_ProactiveIntentTimeoutMs;
    const Jev::Deadline deadline = Jev::Deadline::FromNow(static_cast<int64_t>(NowMs()), budgetMs);

    // Build the small JSON envelope the prompt contract expects in the user role.
    nlohmann::json userPayload = {
        {"message",          message},
        {"hasOpenProposal",  hasOpenProposal},
        {"hasExecutingPlan", hasExecutingPlan},
    };
    std::string userPayloadStr = userPayload.dump();

    // Tier 1 — jev. A confident answer returns here; a low-confidence one is
    // kept (so the caller can audit it) but the LLM tier still runs if it fits.
    if (Jev::EnabledFor(Jev::kSiteIntent))
    {
        nlohmann::json questions{
            {"intent", Jev::ChoiceFromTemplate(Jev::QuestionTemplate(Jev::kSiteIntent, "intent"),
                                               {"approve", "reject", "done", "other"})}};
        const uint32_t cap = JevCapFor(kJevIntentCapMs, deadline.RemainingMs(static_cast<int64_t>(NowMs())));
        Jev::Result r = Jev::Decide(userPayload, questions, cap, Jev::kSiteIntent);
        out.latencyMs += r.latencyMs;
        if (r.ok)
        {
            const Jev::Answer* a = r.Find("intent");
            out.backend       = Jev::kBackendJev;
            out.minConfidence = r.minConfidence;   // threshold of the generation that answered
            out.promptTokens  = r.inputTokens;
            out.confidence    = a->confidence;
            out.label         = ParseIntentString(a->choice);
            out.rawContent    = JevAuditContent("intent", *a, r);
            if (!hasOpenProposal && (out.label == IntentLabel::Approve || out.label == IntentLabel::Reject))
                out.label = IntentLabel::Other;
            if (!hasExecutingPlan && out.label == IntentLabel::Done)
                out.label = IntentLabel::Other;
            if (out.confidence >= out.minConfidence) return out;
            LOG_DEBUG("server.loading",
                      "[Ollama Chat Proactive] intent: jev {} conf={:.2f} < {:.2f} — trying LLM tier",
                      a->choice, out.confidence, out.minConfidence);
        }
        if (!Jev::LlmTierFits(deadline.RemainingMs(static_cast<int64_t>(NowMs())), kLlmTierMinIntentMs))
            return out;   // whatever jev gave (possibly None) — caller falls to its matcher
        // Reset to the LLM tier's contract before overwriting.
        out.backend = Jev::kBackendLlm;
        out.minConfidence = g_ProactiveIntentMinConfidence;
        out.promptTokens = 0;
        out.label = IntentLabel::None;
        out.confidence = 0.0f;
        out.rawContent.clear();
    }

    // Tier 2 — the existing classifier endpoint; same backend, different prompt.
    // Wire shape (Ollama vs OpenAI chat) is chosen from the URL by LlmWire.
    std::string url   = g_GatewayOllamaClassifierUrl.empty()
                            ? g_TacticalUrl   : g_GatewayOllamaClassifierUrl;
    std::string model = g_GatewayOllamaClassifierModel.empty()
                            ? g_TacticalModel : g_GatewayOllamaClassifierModel;
    if (url.empty() || model.empty()) return out;

    nlohmann::json req = LlmWire::BuildRequest(url, model,
                                               SanitizeUTF8(g_ProactiveIntentPromptText),
                                               SanitizeUTF8(userPayloadStr),
                                               g_GatewayOllamaClassifierNumCtx,
                                               /*jsonMode=*/true, /*noThink=*/true);

    OllamaHttpClient client;
    const int64_t remaining = deadline.RemainingMs(static_cast<int64_t>(NowMs()));
    if (remaining <= 0) return out;
    client.SetTimeoutMs(static_cast<uint32_t>(remaining));
    if (!client.IsAvailable()) return out;

    uint64_t t0 = NowMs();
    std::string raw = client.Post(url, req.dump(), LlmWire::Headers(url));
    out.latencyMs += static_cast<uint32_t>(NowMs() - t0);
    if (raw.empty())
    {
        LOG_DEBUG("server.loading",
                  "[Ollama Chat Proactive] intent classifier: empty response (latency={}ms)",
                  out.latencyMs);
        return out;
    }

    // Unwrap whichever envelope the URL's protocol uses.
    std::string content, wireErr;
    if (!LlmWire::ExtractText(url, raw, content, wireErr))
    {
        LOG_DEBUG("server.loading",
                  "[Ollama Chat Proactive] intent classifier ({}): {}",
                  LlmWire::FormatName(url), wireErr);
        return out;
    }
    if (content.empty()) return out;
    out.rawContent = content;

    // Parse the model's classification JSON.
    nlohmann::json classification;
    try { classification = nlohmann::json::parse(content); }
    catch (...)
    {
        LOG_DEBUG("server.loading",
                  "[Ollama Chat Proactive] intent classifier: content parse failed: {}",
                  content);
        return out;
    }
    if (!classification.is_object()) return out;

    std::string intentStr = classification.value("intent", std::string{});
    out.confidence        = classification.value("confidence", 0.0f);
    out.label             = ParseIntentString(intentStr);

    // Enforce the prompt's hard rules in case the model violated them.
    if (!hasOpenProposal &&
        (out.label == IntentLabel::Approve || out.label == IntentLabel::Reject))
    {
        out.label = IntentLabel::Other;
    }
    if (!hasExecutingPlan && out.label == IntentLabel::Done)
    {
        out.label = IntentLabel::Other;
    }

    return out;
}

// ---------------------------------------------------------------------------
// Phase B — gateway planner (paid). Reuses QueryGatewayAPIRaw so all the
// gateway slot / cooldown / per-bot routing primitives apply automatically.
// Strict JSON contract: parse failure / missing field / decision out of
// vocabulary all collapse to PlanDecision::Unset so the caller's deterministic
// fallback path runs.
// ---------------------------------------------------------------------------
namespace
{

PlanDecision ParseDecisionString(std::string const& s)
{
    std::string lo;
    lo.reserve(s.size());
    for (char ch : s)
    {
        unsigned char uc = static_cast<unsigned char>(ch);
        if (std::isspace(uc)) continue;
        lo.push_back(static_cast<char>(std::tolower(uc)));
    }
    if (lo == "propose") return PlanDecision::Propose;
    if (lo == "wait")    return PlanDecision::Wait;
    return PlanDecision::Unset;
}

// Match SanitizeGeneratedLine from _proactive.cpp — strip whitespace, optional
// surrounding quotes, and cap at 200 chars (emission cap).
std::string SanitizePlannerLine(std::string s)
{
    while (!s.empty() && std::isspace(static_cast<unsigned char>(s.front()))) s.erase(s.begin());
    while (!s.empty() && std::isspace(static_cast<unsigned char>(s.back())))  s.pop_back();
    if (s.size() >= 2 && ((s.front() == '"' && s.back() == '"')
                       || (s.front() == '\'' && s.back() == '\'')))
    {
        s = s.substr(1, s.size() - 2);
    }
    if (s.size() > 200) s.resize(200);
    return s;
}

}  // namespace

PlannerResult PlanProposalDecision(uint64_t botGuid, uint64_t playerGuid,
                                   std::string const& snapshotJson)
{
    PlannerResult out;

    if (!g_ProactivePlannerEnable) return out;
    if (g_ProactivePlannerPromptText.empty()) return out;
    if (!g_GatewayEnable) return out;
    if (snapshotJson.empty()) return out;

    // Tier 1 — jev decides propose/wait. A confident `wait` skips the paid
    // gateway call entirely; a confident `propose` still needs the gateway to
    // WRITE the line (jev cannot). Low confidence -> the gateway decides as
    // before. No deadline arithmetic here: the gateway tier has its own
    // 300 s budget and runs on the tick thread, not the world thread.
    bool jevDecidedPropose = false;
    if (Jev::EnabledFor(Jev::kSitePlanner))
    {
        nlohmann::json state;
        try { state = nlohmann::json::parse(snapshotJson); } catch (...) { state = snapshotJson; }
        nlohmann::json questions{
            {"decision", Jev::ChoiceFromTemplate(Jev::QuestionTemplate(Jev::kSitePlanner, "decision"),
                                                 {"propose", "wait"})}};
        const uint32_t cap = JevCapFor(kJevPlannerCapMs, kJevPlannerCapMs);
        Jev::Result r = Jev::Decide(state, questions, cap, Jev::kSitePlanner);
        out.latencyMs += r.latencyMs;
        if (r.ok)
        {
            const Jev::Answer* a = r.Find("decision");
            const float minConf = r.minConfidence;
            if (a->confidence >= minConf)
            {
                out.backend      = Jev::kBackendJev;
                out.promptTokens = r.inputTokens;
                out.rawContent   = JevAuditContent("decision", *a, r);
                if (ParseDecisionString(a->choice) == PlanDecision::Wait)
                {
                    out.decision = PlanDecision::Wait;
                    return out;
                }
                jevDecidedPropose = true;
            }
            else
            {
                LOG_DEBUG("server.loading",
                          "[Ollama Chat Proactive] planner: jev {} conf={:.2f} < {:.2f} — gateway decides",
                          a->choice, a->confidence, minConf);
            }
        }
    }

    uint64_t t0 = NowMs();
    std::string raw = QueryGatewayAPIRaw(botGuid, playerGuid,
                                          g_ProactivePlannerPromptText, snapshotJson);
    out.latencyMs += static_cast<uint32_t>(NowMs() - t0);

    if (raw.empty())
    {
        LOG_DEBUG("server.loading",
                  "[Ollama Chat Proactive] planner: empty gateway response (latency={}ms)",
                  out.latencyMs);
        return out;
    }
    // Keep jev's audit content when it made the decision; the gateway only
    // supplied the line in that case.
    if (!jevDecidedPropose) out.rawContent = raw;

    // QueryGatewayAPIRaw returns the model's content directly (not the
    // gateway envelope), so parse `raw` straight as JSON.
    nlohmann::json plan;
    try { plan = nlohmann::json::parse(raw); }
    catch (...)
    {
        LOG_DEBUG("server.loading",
                  "[Ollama Chat Proactive] planner: JSON parse failed: {}",
                  raw);
        return out;
    }
    if (!plan.is_object()) return out;

    std::string decisionStr = plan.value("decision", std::string{});
    out.decision = ParseDecisionString(decisionStr);
    if (jevDecidedPropose)
    {
        // jev already said propose; the gateway's job was the line. If it vetoed
        // anyway, honour the veto (it saw the same snapshot with more context) —
        // and attribute the row to the tier whose verdict actually stands.
        if (out.decision == PlanDecision::Unset) out.decision = PlanDecision::Propose;
        else if (out.decision == PlanDecision::Wait)
        {
            out.backend      = Jev::kBackendLlm;
            out.promptTokens = 0;
            out.rawContent   = raw;
        }
    }
    if (out.decision == PlanDecision::Unset) return out;

    if (out.decision == PlanDecision::Propose)
    {
        out.line = SanitizePlannerLine(plan.value("line", std::string{}));
        if (out.line.empty())
        {
            // Planner approved but gave us nothing to say — degrade to Unset
            // so the deterministic path produces a fallback line.
            out.decision = PlanDecision::Unset;
            return out;
        }
    }
    return out;
}

// ---------------------------------------------------------------------------
// Phase C — local-Ollama tick gate. Same Ollama HTTP shape as ClassifyIntent
// (reuses Gateway.OllamaClassifier endpoint), different prompt. Cheap upstream
// filter — vetoes obvious no-go ticks before the paid planner fires.
// ---------------------------------------------------------------------------
namespace
{

TickGateAction ParseTickGateString(std::string const& s)
{
    std::string lo;
    lo.reserve(s.size());
    for (char ch : s)
    {
        unsigned char uc = static_cast<unsigned char>(ch);
        if (std::isspace(uc)) continue;
        lo.push_back(static_cast<char>(std::tolower(uc)));
    }
    if (lo == "propose") return TickGateAction::Propose;
    if (lo == "wait")    return TickGateAction::Wait;
    return TickGateAction::Unset;
}

}  // namespace

TickGateResult EvaluateTickGate(uint64_t /*botGuid*/, uint64_t /*playerGuid*/,
                                std::string const& snapshotJson)
{
    TickGateResult out;
    out.minConfidence = g_ProactiveTickGateMinConfidence;

    if (!g_ProactiveTickGateEnable) return out;
    if (g_ProactiveTickGatePromptText.empty()) return out;
    if (snapshotJson.empty()) return out;

    const uint32_t budgetMs = g_ProactiveTickGateTimeoutMs == 0 ? 2000u : g_ProactiveTickGateTimeoutMs;
    const Jev::Deadline deadline = Jev::Deadline::FromNow(static_cast<int64_t>(NowMs()), budgetMs);

    // Tier 1 — jev. Two-way Choice (not noul) because the caller gates a
    // `wait` on confidence and noul answers carry none.
    if (Jev::EnabledFor(Jev::kSiteTickGate))
    {
        nlohmann::json state;
        try { state = nlohmann::json::parse(snapshotJson); } catch (...) { state = snapshotJson; }
        nlohmann::json questions{
            {"gate", Jev::ChoiceFromTemplate(Jev::QuestionTemplate(Jev::kSiteTickGate, "gate"),
                                             {"propose", "wait"})}};
        const uint32_t cap = JevCapFor(kJevTickGateCapMs, deadline.RemainingMs(static_cast<int64_t>(NowMs())));
        Jev::Result r = Jev::Decide(state, questions, cap, Jev::kSiteTickGate);
        out.latencyMs += r.latencyMs;
        if (r.ok)
        {
            const Jev::Answer* a = r.Find("gate");
            out.backend       = Jev::kBackendJev;
            out.minConfidence = r.minConfidence;
            out.promptTokens  = r.inputTokens;
            out.confidence    = a->confidence;
            out.action        = ParseTickGateString(a->choice);
            out.rawContent    = JevAuditContent("gate", *a, r);
            if (out.confidence >= out.minConfidence) return out;
        }
        if (!Jev::LlmTierFits(deadline.RemainingMs(static_cast<int64_t>(NowMs())), kLlmTierMinTickGateMs))
            return out;
        out.backend = Jev::kBackendLlm;
        out.minConfidence = g_ProactiveTickGateMinConfidence;
        out.promptTokens = 0;
        out.action = TickGateAction::Unset;
        out.confidence = 0.0f;
        out.rawContent.clear();
    }

    // Tier 2 — the existing classifier endpoint (one backend, many prompts).
    // Wire shape (Ollama vs OpenAI chat) is chosen from the URL by LlmWire.
    std::string url   = g_GatewayOllamaClassifierUrl.empty()
                            ? g_TacticalUrl   : g_GatewayOllamaClassifierUrl;
    std::string model = g_GatewayOllamaClassifierModel.empty()
                            ? g_TacticalModel : g_GatewayOllamaClassifierModel;
    if (url.empty() || model.empty()) return out;

    nlohmann::json req = LlmWire::BuildRequest(url, model,
                                               SanitizeUTF8(g_ProactiveTickGatePromptText),
                                               SanitizeUTF8(snapshotJson),
                                               g_GatewayOllamaClassifierNumCtx,
                                               /*jsonMode=*/true, /*noThink=*/true);

    OllamaHttpClient client;
    const int64_t remaining = deadline.RemainingMs(static_cast<int64_t>(NowMs()));
    if (remaining <= 0) return out;
    client.SetTimeoutMs(static_cast<uint32_t>(remaining));
    if (!client.IsAvailable()) return out;

    uint64_t t0 = NowMs();
    std::string raw = client.Post(url, req.dump(), LlmWire::Headers(url));
    out.latencyMs += static_cast<uint32_t>(NowMs() - t0);
    if (raw.empty())
    {
        LOG_DEBUG("server.loading",
                  "[Ollama Chat Proactive] tick gate: empty response (latency={}ms)",
                  out.latencyMs);
        return out;
    }

    std::string content, wireErr;
    if (!LlmWire::ExtractText(url, raw, content, wireErr))
    {
        LOG_DEBUG("server.loading",
                  "[Ollama Chat Proactive] tick gate ({}): {}",
                  LlmWire::FormatName(url), wireErr);
        return out;
    }
    if (content.empty()) return out;
    out.rawContent = content;

    nlohmann::json verdict;
    try { verdict = nlohmann::json::parse(content); }
    catch (...)
    {
        LOG_DEBUG("server.loading",
                  "[Ollama Chat Proactive] tick gate: content parse failed: {}", content);
        return out;
    }
    if (!verdict.is_object()) return out;

    std::string actionStr = verdict.value("action", std::string{});
    out.confidence        = verdict.value("confidence", 0.0f);
    out.action            = ParseTickGateString(actionStr);
    return out;
}

}  // namespace llm
}  // namespace proactive
}  // namespace ollamachat
