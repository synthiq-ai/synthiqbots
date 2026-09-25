// Standalone g++ harness for the fallback-chain logic every jev call site
// shares: one absolute deadline, jev capped to its slice and the remainder,
// the LLM tier started only when the remainder can fit it, the breaker kept
// out of the confidence path, and exactly one dispatch per chain. The chain
// is modelled here with fake tiers so the ARITHMETIC and the ORDERING are the
// things under test — the real sockets live in mod-ollama-chat_jev.cpp.
//
//   g++ -std=c++17 -I src -I deps tests/jev/harness_fallback.cpp -o /tmp/hf && /tmp/hf
//   -DINJECT_NO_DEADLINE       jev ignores the remainder            -> FAIL
//   -DINJECT_LLM_ALWAYS        LLM tier runs even with no room      -> FAIL
//   -DINJECT_LOWCONF_TRIPS     low confidence counts as a failure   -> FAIL
//   -DINJECT_DOUBLE_DISPATCH   low-conf jev answer is acted on too  -> FAIL

#include "mod-ollama-chat_jev_core.h"

#include <cstdio>
#include <functional>
#include <string>

static int g_failures = 0;
#define CHECK(cond, msg) do { if (!(cond)) { std::printf("FAIL: %s (%s:%d)\n", msg, __FILE__, __LINE__); ++g_failures; } } while (0)

// A fake jev: returns after `latencyMs` (bounded by the cap it was given) with a
// choice + confidence, or a transport failure.
struct FakeJev
{
    uint32_t latencyMs = 900;
    bool     transportFail = false;
    float    confidence = 0.95f;
    std::string choice = "approve";
    uint32_t lastCapMs = 0;
    int calls = 0;

    Jev::Result Decide(uint32_t capMs, int64_t& clock)
    {
        ++calls;
        lastCapMs = capMs;
        Jev::Result r;
        if (capMs == 0) { r.error = "no budget left"; return r; }
        uint32_t spent = std::min(latencyMs, capMs);
        clock += spent;
        r.latencyMs = spent;
        if (transportFail || latencyMs > capMs) { r.error = "transport: timeout"; return r; }
        Jev::Answer a; a.type = "choice"; a.choice = choice; a.confidence = confidence;
        r.answers["intent"] = a;
        r.ok = true;
        return r;
    }
};

struct FakeLlm
{
    uint32_t latencyMs = 1300;
    int calls = 0;
    uint32_t lastTimeoutMs = 0;
    std::string Run(uint32_t timeoutMs, int64_t& clock)
    {
        ++calls;
        lastTimeoutMs = timeoutMs;
        clock += std::min(latencyMs, timeoutMs);
        return latencyMs <= timeoutMs ? "reject" : "";
    }
};

struct Chain
{
    uint32_t budgetMs      = 3000;   // the path's existing timeout (intent = 3000)
    uint32_t jevSiteCapMs  = 1000;   // intent's slice
    uint32_t llmFloorMs    = 1500;   // below this the LLM tier is not started
    float    jevMinConf    = 0.7f;
    Jev::Breaker breaker;
};

// Mirrors the call-site shape in mod-ollama-chat_proactive_llm.cpp: returns
// the label acted on ("" = fell to deterministic) and counts dispatches.
struct Outcome { std::string label; std::string backend; int dispatches = 0; };

static Outcome RunChain(Chain& c, FakeJev& jev, FakeLlm& llm, int64_t& clock, bool jevEnabled = true)
{
    Outcome o;
    const Jev::Deadline deadline = Jev::Deadline::FromNow(clock, c.budgetMs);

    if (jevEnabled && !c.breaker.IsOpen(clock))
    {
#ifdef INJECT_NO_DEADLINE
        const uint32_t cap = c.jevSiteCapMs;
#else
        const uint32_t cap = Jev::JevCapMs(c.jevSiteCapMs, deadline.RemainingMs(clock));
#endif
        Jev::Result r = jev.Decide(cap, clock);
        if (!r.ok)
        {
            c.breaker.RecordFailure(clock);
        }
        else
        {
            const Jev::Answer& a = r.answers["intent"];
            if (a.confidence >= c.jevMinConf)
            {
                o.label = a.choice; o.backend = "jev"; ++o.dispatches;
                return o;
            }
#ifdef INJECT_LOWCONF_TRIPS
            c.breaker.RecordFailure(clock);
#endif
#ifdef INJECT_DOUBLE_DISPATCH
            o.label = a.choice; o.backend = "jev"; ++o.dispatches;   // acted on despite low confidence
#endif
        }
#ifdef INJECT_LLM_ALWAYS
        const bool llmFits = true;
#else
        const bool llmFits = Jev::LlmTierFits(deadline.RemainingMs(clock), c.llmFloorMs);
#endif
        if (!llmFits) return o;   // deterministic tier (caller's matcher) — not modelled
    }

    const int64_t remaining = deadline.RemainingMs(clock);
    if (remaining <= 0) return o;
    std::string label = llm.Run(static_cast<uint32_t>(remaining), clock);
    if (!label.empty()) { o.label = label; o.backend = "llm"; ++o.dispatches; }
    return o;
}

int main()
{
    // 1. Happy path: jev confident -> one dispatch, LLM never called, within budget.
    {
        Chain c; FakeJev jev; FakeLlm llm; int64_t clock = 0;
        Outcome o = RunChain(c, jev, llm, clock);
        CHECK(o.label == "approve" && o.backend == "jev" && o.dispatches == 1, "confident jev answers alone");
        CHECK(llm.calls == 0, "LLM not called after confident jev");
        CHECK(jev.lastCapMs == 1000, "jev capped to its site slice");
        CHECK(clock <= 3000, "within budget");
    }
    // 2. Low confidence: LLM tier runs with the REMAINDER, exactly one dispatch.
    {
        Chain c; FakeJev jev; jev.confidence = 0.4f; FakeLlm llm; int64_t clock = 0;
        Outcome o = RunChain(c, jev, llm, clock);
        CHECK(o.label == "reject" && o.backend == "llm", "LLM decides after low-confidence jev");
        CHECK(o.dispatches == 1, "exactly one dispatch per chain");            // FAILS under INJECT_DOUBLE_DISPATCH
        CHECK(llm.lastTimeoutMs == 2100, "LLM gets the remainder (3000 - 900)");
        CHECK(c.breaker.failures == 0, "low confidence is not a failure");     // FAILS under INJECT_LOWCONF_TRIPS
    }
    // 3. jev slow: its cap bounds the damage and the LLM still fits.
    {
        Chain c; FakeJev jev; jev.latencyMs = 5000; FakeLlm llm; int64_t clock = 0;
        Outcome o = RunChain(c, jev, llm, clock);
        CHECK(jev.lastCapMs == 1000, "jev cap");
        CHECK(o.backend == "llm" && llm.lastTimeoutMs == 2000, "LLM runs with 2000 after a 1000 jev timeout");
        CHECK(c.breaker.failures == 1, "timeout is a failure");
    }
    // 4. Budget nearly gone before jev: jev clamps to the remainder, LLM is NOT started.
    {
        Chain c; c.budgetMs = 1200; FakeJev jev; jev.confidence = 0.4f; FakeLlm llm; int64_t clock = 0;
        Outcome o = RunChain(c, jev, llm, clock);
        CHECK(jev.lastCapMs == 1000, "jev slice within a 1200 budget");
        CHECK(llm.calls == 0, "LLM tier skipped: 300 ms left < 1500 floor");   // FAILS under INJECT_LLM_ALWAYS
        CHECK(o.dispatches == 0, "falls to deterministic");
    }
    // 4b. Tiny budget: jev itself is clamped below its slice.
    {
        Chain c; c.budgetMs = 400; FakeJev jev; FakeLlm llm; int64_t clock = 0;
        RunChain(c, jev, llm, clock);
        CHECK(jev.lastCapMs == 400, "jev clamped to the whole remainder");      // FAILS under INJECT_NO_DEADLINE
    }
    // 5. Breaker: three transport failures open it; then jev is skipped at zero cost.
    {
        Chain c; c.breaker.cooldownMs = 60000; FakeJev jev; jev.transportFail = true; FakeLlm llm;
        int64_t clock = 0;
        for (int i = 0; i < 3; ++i) RunChain(c, jev, llm, clock);
        CHECK(c.breaker.IsOpen(clock), "breaker open after 3 failures");
        int jevCallsBefore = jev.calls;
        int64_t before = clock;
        Outcome o = RunChain(c, jev, llm, clock);
        CHECK(jev.calls == jevCallsBefore, "jev not called while breaker open");
        CHECK(o.backend == "llm" && llm.lastTimeoutMs == 3000, "LLM gets the FULL budget while breaker open");
        CHECK(clock - before == 1300, "no jev latency paid while open");
        clock += 60001;
        RunChain(c, jev, llm, clock);
        CHECK(jev.calls == jevCallsBefore + 1, "breaker half-opens after cooldown");
    }
    // 6. jev disabled: chain is exactly today's behaviour.
    {
        Chain c; FakeJev jev; FakeLlm llm; int64_t clock = 0;
        Outcome o = RunChain(c, jev, llm, clock, /*jevEnabled=*/false);
        CHECK(jev.calls == 0 && o.backend == "llm" && llm.lastTimeoutMs == 3000, "disabled tier is inert");
    }

    if (g_failures == 0) { std::printf("PASS: harness_fallback\n"); return 0; }
    std::printf("FAIL: harness_fallback (%d)\n", g_failures);
    return 1;
}
