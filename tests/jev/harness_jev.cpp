// Standalone g++ harness for the header-only jev core (builders, parser,
// validation). No AzerothCore, no httplib. Two-directional: the default build
// must PASS; each -DINJECT_* arm removes one guard and must FAIL, proving the
// guard catches what it claims to.
//
//   g++ -std=c++17 -I src -I deps tests/jev/harness_jev.cpp -o /tmp/harness_jev && /tmp/harness_jev
//   g++ -std=c++17 -I src -I deps -DINJECT_BAD_CHOICE tests/jev/harness_jev.cpp -o /tmp/h && /tmp/h   # expect FAIL
//   g++ -std=c++17 -I src -I deps -DINJECT_MISSING_ANSWER ...                                          # expect FAIL
//   g++ -std=c++17 -I src -I deps -DINJECT_UNSOLICITED ...                                             # expect FAIL

#include "mod-ollama-chat_jev_core.h"

#include <cstdio>
#include <cstdlib>

static int g_failures = 0;
#define CHECK(cond, msg) do { if (!(cond)) { std::printf("FAIL: %s (%s:%d)\n", msg, __FILE__, __LINE__); ++g_failures; } } while (0)

// Verbatim response captured 2026-09-20 from POST https://openrouter.ai/api/alpha/decisions.
static const char* kLiveResponse = R"({"model":"typesafe/jev-1.13-20260917","answers":{"intent":{"type":"choice","choice":"approve","probabilities":{"approve":0.99,"other":0.01,"done":0,"reject":0},"confidence":0.99},"speak_now":{"type":"noul","noul":0.94}},"usage":{"input_tokens":398,"output_tokens":64,"cost":0.000016716},"id":"gen-dec-0000000000-EXAMPLE0000000000000","provider":"TypeSafe"})";

static nlohmann::json LiveQuestions()
{
    return nlohmann::json{
        {"intent", Jev::ChoicePairs("What is the player replying?",
                               {{"approve", "accepted"}, {"reject", "declined"}, {"done", "finished"}, {"other", "anything else"}})},
        {"speak_now", Jev::Noul("Is the player explicitly agreeing?")},
    };
}

int main()
{
    // --- builders produce the documented wire shape ---------------------------
    {
        nlohmann::json q = Jev::ChoicePairs("pick", {{"a", "first"}, {"b", "second"}});
        CHECK(q["type"] == "choice", "choice type");
        CHECK(q["criteria"]["a"] == "first" && q["criteria"]["b"] == "second", "choice criteria");
        nlohmann::json n = Jev::Noul("yes?", "it is", "it is not");
        CHECK(n["type"] == "noul" && n["criteria"]["true"] == "it is", "noul criteria");
        nlohmann::json s = Jev::Score("how bad", {"lo", "mid", "hi"});
        CHECK(s["type"] == "score" && s["criteria"].size() == 3, "score levels");
        nlohmann::json body = Jev::BuildRequestBody("typesafe/jev-1.13", nlohmann::json{{"message", "hi"}}, LiveQuestions());
        CHECK(body["model"] == "typesafe/jev-1.13" && body["state"]["message"] == "hi" && body["questions"].size() == 2, "request body");
    }

    // --- ChoiceFromTemplate: option SET comes from the caller, wording from the file
    {
        nlohmann::json tmpl = nlohmann::json::parse(R"({"instructions":"which?","criteria":{"a":{"what":"A"},"zzz":"never offered"}})");
        nlohmann::json q = Jev::ChoiceFromTemplate(tmpl, {"a", "b"});
        CHECK(q["criteria"].size() == 2, "template restricted to offered options");
        CHECK(!q["criteria"].contains("zzz"), "template cannot add an option");
        CHECK(q["criteria"]["b"] == "b", "undescribed option falls back to its name");
        CHECK(q["instructions"] == "which?", "template instructions kept");
        nlohmann::json q2 = Jev::ChoiceFromTemplate(nullptr, {"x"});
        CHECK(q2["criteria"]["x"] == "x", "null template still builds");
    }

    // --- live response parses --------------------------------------------------
    {
        Jev::Result r;
        CHECK(Jev::ParseResponse(kLiveResponse, r), "live response parses");
        CHECK(r.ok, "ok");
        CHECK(r.model == "typesafe/jev-1.13-20260917", "model id");
        CHECK(r.inputTokens == 398 && r.outputTokens == 64, "usage tokens");
        CHECK(r.costUsd > 0.0000167 && r.costUsd < 0.0000168, "usage cost");
        const Jev::Answer* a = r.Find("intent");
        CHECK(a && a->type == "choice" && a->choice == "approve", "choice answer");
        CHECK(a && a->confidence > 0.98f && a->probabilities.at("approve") > 0.98f, "choice confidence + probabilities");
        const Jev::Answer* n = r.Find("speak_now");
        CHECK(n && n->type == "noul" && n->noul > 0.93f && n->confidence == 0.0f, "noul has probability, no confidence");
        CHECK(Jev::ValidateAgainstQuestions(LiveQuestions(), r), "live answers validate against the asked questions");
    }

    // --- error envelopes ---------------------------------------------------------
    {
        Jev::Result r;
        CHECK(!Jev::ParseResponse(R"({"error":{"message":"Model typesafe/jev-latest does not exist","code":400}})", r), "openrouter error rejected");
        CHECK(r.error.find("jev-latest") != std::string::npos, "openrouter error message surfaced");
        CHECK(!Jev::ParseResponse(R"({"detail":{"error_type":"authentication_error","message":"Must supply an API key!"}})", r), "typesafe error rejected");
        CHECK(r.error.find("API key") != std::string::npos, "typesafe error message surfaced");
        CHECK(!Jev::ParseResponse("not json", r), "garbage rejected");
        CHECK(!Jev::ParseResponse(R"({"answers":{"q":{"type":"choice"}}})", r), "choice without 'choice' rejected");
        CHECK(!Jev::ParseResponse(R"({"answers":{"q":{"type":"weird","x":1}}})", r), "unknown answer type rejected");
    }

    // --- validation guards (the injectable ones) --------------------------------
    {
        Jev::Result r;
        std::string body = kLiveResponse;
#ifdef INJECT_BAD_CHOICE
        // A choice outside the criteria set — the type guarantee says impossible; the parser must not trust it.
        body.replace(body.find("\"choice\":\"approve\""), sizeof("\"choice\":\"approve\"") - 1, "\"choice\":\"maybe\"");
#endif
#ifdef INJECT_MISSING_ANSWER
        body.replace(body.find("\"speak_now\""), sizeof("\"speak_now\"") - 1, "\"renamed\"");
#endif
        CHECK(Jev::ParseResponse(body, r), "parses");
        bool valid = Jev::ValidateAgainstQuestions(LiveQuestions(), r);
        CHECK(valid, "answers validate against questions");   // FAILS under INJECT_BAD_CHOICE / INJECT_MISSING_ANSWER
    }
    {
        nlohmann::json qs = LiveQuestions();
#ifdef INJECT_UNSOLICITED
        qs.erase("speak_now");   // response still carries speak_now -> unsolicited
#endif
        Jev::Result r;
        Jev::ParseResponse(kLiveResponse, r);
        CHECK(Jev::ValidateAgainstQuestions(qs, r), "no unsolicited answers");   // FAILS under INJECT_UNSOLICITED
    }

    // --- breaker -----------------------------------------------------------------
    {
        Jev::Breaker b; b.threshold = 3; b.cooldownMs = 1000;
        CHECK(!b.IsOpen(0), "starts closed");
        CHECK(!b.RecordFailure(10) && !b.RecordFailure(20), "two failures stay closed");
        CHECK(b.RecordFailure(30), "third failure trips");
        CHECK(b.IsOpen(500) && !b.IsOpen(1031), "open for cooldown then closes");
        CHECK(!b.RecordFailure(600), "failures while open do not re-trip");
        b.RecordSuccess();
        CHECK(!b.IsOpen(700) && b.failures == 0, "success resets");
        Jev::Breaker c; c.RecordFailure(0); c.RecordFailure(1); c.RecordSuccess(); c.RecordFailure(2); c.RecordFailure(3);
        CHECK(!c.IsOpen(4), "success in between resets the count");
    }

    // --- deadline arithmetic -------------------------------------------------------
    {
        Jev::Deadline d = Jev::Deadline::FromNow(1000, 3000);
        CHECK(d.RemainingMs(1000) == 3000 && d.RemainingMs(3500) == 500 && d.RemainingMs(5000) == -1000, "remaining");
        CHECK(Jev::JevCapMs(1500, 3000) == 1500, "cap = configured when budget is larger");
        CHECK(Jev::JevCapMs(1500, 700) == 700, "cap clamps to remainder");
        CHECK(Jev::JevCapMs(1500, 0) == 0 && Jev::JevCapMs(1500, -5) == 0 && Jev::JevCapMs(0, 3000) == 0, "cap 0 when nothing left");
        CHECK(Jev::LlmTierFits(1500, 1500) && !Jev::LlmTierFits(1499, 1500), "LLM tier floor is inclusive");
    }

    if (g_failures == 0) { std::printf("PASS: harness_jev\n"); return 0; }
    std::printf("FAIL: harness_jev (%d)\n", g_failures);
    return 1;
}
