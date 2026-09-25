#ifndef MOD_OLLAMA_CHAT_JEV_H
#define MOD_OLLAMA_CHAT_JEV_H

// jev decision tier — TypeSafe's System One model in front of the module's
// decision-shaped fast paths (gateway classifier, proactive intent, proactive
// tick-gate, proactive planner veto). State in, typed probabilities out; no
// text is ever generated here, so every path that must SAY something stays on
// an LLM and every path that must DECIDE something asks jev first and falls
// through to the existing LlmWire/DeepSeek call, then to deterministic code.
//
// This header is the runtime surface (config generation, HTTP pool, breaker,
// audit-column migration). The pure pieces — builders, parser, breaker and
// deadline arithmetic — live in mod-ollama-chat_jev_core.h so a bare g++ can
// compile them for the harnesses under tests/jev/.
//
// Access path measured 2026-09-20: OpenRouter's Decisions API
// (POST https://openrouter.ai/api/alpha/decisions, model typesafe/jev-1.13)
// speaks the native TypeSafe wire format; the same body goes unchanged to
// https://api.typesafe.ai/v1/systemone with model jev-1.13.0 if a native key
// ever exists. OpenRouter is geo-blocked from game-host's own egress, hence the
// HTTP-proxy knobs (a gluetun HTTPPROXY on the LAN).

#include "mod-ollama-chat_jev_core.h"

#include <cstdint>
#include <string>
#include <vector>

namespace Jev
{
    // Site ids, as used by EnabledFor / MinConfidenceFor / the questions file.
    constexpr const char* kSiteClassifier = "classifier";   // A — gateway command classifier
    constexpr const char* kSiteIntent     = "intent";       // C — proactive approve/reject/done
    constexpr const char* kSiteTickGate   = "tickgate";     // D — proactive propose/wait
    constexpr const char* kSitePlanner    = "planner";      // E — planner veto (decision half)
    constexpr const char* kSiteTactical   = "tactical";     // B — per-tick action funnel (PR 2)
    constexpr const char* kSitePlaybook   = "playbook";     // leader playbook relevance filter (PR 3)

    // Leader playbook compaction: score every workflow row of
    // Mcp.Leader.SystemPromptFile against the player's message (one Noul per
    // row, one request) and return header + the relevant rows + footer.
    // Returns false (and leaves `out` untouched) when disabled, when the
    // playbook has no rows, or on any jev failure — the caller then sends the
    // full text exactly as today. `keptRows`/`totalRows` are for the log line.
    bool FilterLeaderPlaybook(const std::string& message,
                              std::string& out, size_t& keptRows, size_t& totalRows);

    // Backend labels written to the audit tables' `backend` column.
    constexpr const char* kBackendJev = "jev";
    constexpr const char* kBackendLlm = "llm";
    constexpr const char* kBackendDet = "det";

    // Master switch resolved: Jev.Enable && key && url && model all present.
    bool Enabled();
    // Master switch && the site's own Enable flag.
    bool EnabledFor(const char* site);
    // The site's MinConfidence knob (0 when the site is unknown).
    float MinConfidenceFor(const char* site);
    // Question template for `site` (and optional `id` below it) from
    // prompts/jev_questions.json; null json when absent.
    nlohmann::json QuestionTemplate(const char* site, const char* id = nullptr);

    // One synchronous decision. `timeoutMs` is the caller's cap for THIS call
    // (already clamped to its chain's remaining budget — see Deadline in the
    // core header). Never throws; ok=false + error on any failure. Single HTTP
    // attempt, no transport retry; feeds the jev-side breaker.
    Result Decide(const nlohmann::json& state, const nlohmann::json& questions,
                  uint32_t timeoutMs, const char* site);

    // Rebuild the immutable config generation (questions file, pool, breaker)
    // from the g_Jev* globals. Called from LoadOllamaChatConfig() so startup
    // and `config_reload` share one path; in-flight calls keep the old
    // generation alive through their shared_ptr.
    void OnConfigReloaded();

    // Startup self-migration for the audit tables' `backend` (+ tactical
    // `prompt_tokens`) columns — the worldserver image bakes
    // AC_UPDATES_ENABLE_DATABASES=0, so data/sql/characters/updates/ is never
    // applied automatically. Idempotent; logs each ALTER it performs.
    void EnsureAuditBackendColumns();
    // True once EnsureAuditBackendColumns confirmed both tables carry the new
    // columns; the audit INSERTs switch to the wider column list only then, so
    // an unmigrated DB keeps writing today's rows instead of failing.
    bool AuditColumnsReady();
}

#endif // MOD_OLLAMA_CHAT_JEV_H
