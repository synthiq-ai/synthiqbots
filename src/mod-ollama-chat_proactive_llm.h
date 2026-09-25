#ifndef MOD_OLLAMA_CHAT_PROACTIVE_LLM_H
#define MOD_OLLAMA_CHAT_PROACTIVE_LLM_H
//
// Proactive Leader Bot — LLM glue (Phase A: local-Ollama intent classifier).
//
// Reuses the same local Ollama endpoint as TryGatewayIntentClassify
// (`g_GatewayOllamaClassifierUrl` / `g_GatewayOllamaClassifierModel`, falling
// back to the Tactical.Url / Tactical.Model defaults). Returns IntentLabel::None
// on disabled / HTTP failure / parse error so the caller can fall through to
// the deterministic C++ matcher.
//
// Master gate: g_ProactiveIntentEnable (default ON — Phase A is a clear UX win
// and the deterministic fallback path is preserved).
//

#include <cstdint>
#include <string>

namespace ollamachat
{
namespace proactive
{
namespace llm
{

enum class IntentLabel : uint8_t
{
    None = 0,    // model returned unknown / confidence below threshold / error
    Approve,
    Reject,
    Done,
    Other,       // confident "not approve/reject/done" — caller falls through to regular chain
};

struct IntentResult
{
    IntentLabel label      = IntentLabel::None;
    float       confidence = 0.0f;
    uint32_t    latencyMs  = 0;
    std::string rawContent;  // model's raw JSON content (for audit) — empty when not consulted
    // Which tier produced the answer and the threshold it must clear. jev's
    // confidence is distribution-derived, the LLM's is self-reported, so the
    // caller compares against `minConfidence` (set here per tier), never a
    // fixed knob.
    const char* backend       = "llm";
    float       minConfidence = 0.0f;
    uint32_t    promptTokens  = 0;
};

// Returns IntentResult; on disabled/HTTP-failure/parse-error returns {None,0,0,""}.
// `hasOpenProposal` / `hasExecutingPlan` are passed through to the model as
// context flags so it can refuse approve/reject without an open proposal and
// refuse done without an executing plan.
IntentResult ClassifyIntent(uint64_t botGuid, uint64_t playerGuid,
                            std::string const& message,
                            bool hasOpenProposal, bool hasExecutingPlan);

// ---------------------------------------------------------------------------
// Phase B — gateway planner (paid). Agent veto + phrasing on top of the
// C++-picked candidate. Reuses the existing QueryGatewayAPIRaw entry point
// so it inherits gateway cooldown, slot limits, and per-bot routing.
// ---------------------------------------------------------------------------

enum class PlanDecision : uint8_t
{
    Unset = 0,    // function couldn't reach a verdict (disabled / HTTP failure / parse error)
    Wait,         // agent vetoed this tick — caller should NOT emit
    Propose,      // agent approved — caller emits the returned line as a proposal
};

struct PlannerResult
{
    PlanDecision decision   = PlanDecision::Unset;
    std::string  line;        // empty when decision != Propose
    uint32_t     latencyMs   = 0;
    std::string  rawContent;  // model's raw JSON content (for audit) — empty when not consulted
    const char*  backend      = "llm";   // "jev" when jev made the propose/wait decision
    uint32_t     promptTokens = 0;
};

// `snapshotJson` is the JSON-encoded user message described in
// prompts/proactive_planner.md. Returns Unset on any failure so the caller
// falls back to the deterministic SelectCandidate + PhraseLine path.
PlannerResult PlanProposalDecision(uint64_t botGuid, uint64_t playerGuid,
                                   std::string const& snapshotJson);

// ---------------------------------------------------------------------------
// Phase C — local-Ollama tick gate. Cheap upstream filter before the (paid)
// gateway planner. Throttled at the call site by ProactiveTickGate.IntervalSec
// so even a 10s heartbeat queries Ollama at most twice per minute per
// (bot, player).
// ---------------------------------------------------------------------------

enum class TickGateAction : uint8_t
{
    Unset = 0,    // disabled / HTTP failure / parse error / below threshold
    Propose,      // gate approves — caller proceeds to candidate select + planner
    Wait,         // gate vetoes — caller skips this tick
};

struct TickGateResult
{
    TickGateAction action     = TickGateAction::Unset;
    float          confidence = 0.0f;
    uint32_t       latencyMs  = 0;
    std::string    rawContent;  // model's raw JSON (for audit) — empty when not consulted
    const char*    backend       = "llm";
    float          minConfidence = 0.0f;   // threshold for THIS tier — see IntentResult
    uint32_t       promptTokens  = 0;
};

// `snapshotJson` is the JSON-encoded user message described in
// prompts/proactive_tick_gate.md. Returns Unset on any failure so the caller
// falls through to the deterministic propose path.
TickGateResult EvaluateTickGate(uint64_t botGuid, uint64_t playerGuid,
                                std::string const& snapshotJson);

}  // namespace llm
}  // namespace proactive
}  // namespace ollamachat

#endif  // MOD_OLLAMA_CHAT_PROACTIVE_LLM_H
