#ifndef MOD_OLLAMA_CHAT_JEV_CORE_H
#define MOD_OLLAMA_CHAT_JEV_CORE_H

// Dependency-free core of the jev decision tier: wire-shape builders, the
// response parser, and the breaker / deadline arithmetic every call site
// shares. Deliberately free of AzerothCore, httplib and Log includes so
// tests/jev/harness_*.cpp can compile it with a bare `g++ -std=c++17` and
// prove the guards two-directionally (the house pattern). Everything that
// needs a socket, a config global or a logger lives in mod-ollama-chat_jev.cpp.
//
// Wire format (measured 2026-09-20 against OpenRouter's Decisions API, which
// is byte-identical to TypeSafe's native /v1/systemone apart from the model id
// and the extra usage.cost / provider fields):
//
//   POST {model, state, questions:{<id>:{type, instructions, criteria}}}
//   200  {model, answers:{<id>:{type, choice?, probabilities?, confidence?,
//                                noul?, score?, legend?}},
//         usage:{input_tokens, output_tokens, cost?}}
//   err  {"error":{"message":...}}            (OpenRouter)
//        {"detail":{"error_type","message"}}  (TypeSafe native)
//
// Noul answers carry ONLY `noul` (P(yes)) — no confidence field — which is why
// every gate that feeds an existing MinConfidence knob is a two-way Choice.

#include <nlohmann/json.hpp>

#include <algorithm>
#include <cstdint>
#include <map>
#include <string>
#include <utility>
#include <vector>

namespace Jev
{
    struct Answer
    {
        std::string type;                            // "choice" | "noul" | "score"
        std::string choice;                          // choice only
        std::map<std::string, float> probabilities;  // choice: per option; score: per level index
        float confidence = 0.0f;                     // choice / score only (noul has none)
        float noul       = -1.0f;                    // noul only, P(yes) in [0,1]
        float score      = -1.0f;                    // score only, weighted level mean
    };

    struct Result
    {
        bool        ok = false;
        std::string error;                           // set whenever ok == false
        std::map<std::string, Answer> answers;
        uint32_t    latencyMs    = 0;
        uint32_t    inputTokens  = 0;
        uint32_t    outputTokens = 0;
        double      costUsd      = 0.0;
        std::string model;                           // versioned id the provider actually served
        // The site's MinConfidence as configured in the generation that served
        // THIS request — callers gate on this, never on a knob re-read after the
        // call, so a config_reload mid-flight cannot lower the bar under an answer.
        float       minConfidence = 0.0f;

        const Answer* Find(const std::string& id) const
        {
            auto it = answers.find(id);
            return it == answers.end() ? nullptr : &it->second;
        }
    };

    // ---- question builders --------------------------------------------------
    // `instructions` / criteria descriptions may be strings, objects or arrays
    // (TypeSafe accepts all three); the builders just place them.

    inline nlohmann::json Choice(const nlohmann::json& instructions,
                                 const nlohmann::json& criteriaObject)
    {
        return nlohmann::json{{"type", "choice"},
                              {"instructions", instructions},
                              {"criteria", criteriaObject}};
    }

    // Same, from an ordered (option, description) list. Distinct name because a
    // string literal converts to both std::string and nlohmann::json.
    inline nlohmann::json ChoicePairs(const std::string& instructions,
                                      const std::vector<std::pair<std::string, std::string>>& criteria)
    {
        nlohmann::json c = nlohmann::json::object();
        for (const auto& kv : criteria) c[kv.first] = kv.second;
        return Choice(nlohmann::json(instructions), c);
    }

    inline nlohmann::json Noul(const nlohmann::json& instructions,
                               const std::string& trueDesc = std::string(),
                               const std::string& falseDesc = std::string())
    {
        nlohmann::json q{{"type", "noul"}, {"instructions", instructions}};
        if (!trueDesc.empty() || !falseDesc.empty())
            q["criteria"] = nlohmann::json{{"true", trueDesc}, {"false", falseDesc}};
        return q;
    }

    inline nlohmann::json Score(const nlohmann::json& instructions,
                                const std::vector<std::string>& levels)
    {
        return nlohmann::json{{"type", "score"},
                              {"instructions", instructions},
                              {"criteria", levels}};
    }

    inline nlohmann::json BuildRequestBody(const std::string& model,
                                           const nlohmann::json& state,
                                           const nlohmann::json& questions)
    {
        return nlohmann::json{{"model", model}, {"state", state}, {"questions", questions}};
    }

    // Build a Choice from a template object ({"instructions":..,"criteria":{opt:desc}})
    // restricted to `options`. Options the template does not describe get their
    // own name as the description, so the option SET always comes from the live
    // allowlist and the file can never advertise something the dispatcher
    // rejects — it only supplies wording.
    inline nlohmann::json ChoiceFromTemplate(const nlohmann::json& tmpl,
                                             const std::vector<std::string>& options)
    {
        nlohmann::json instructions = tmpl.is_object() && tmpl.contains("instructions")
                                        ? tmpl["instructions"] : nlohmann::json("Pick the best option.");
        const nlohmann::json* crit = (tmpl.is_object() && tmpl.contains("criteria") && tmpl["criteria"].is_object())
                                        ? &tmpl["criteria"] : nullptr;
        nlohmann::json c = nlohmann::json::object();
        for (const std::string& opt : options)
        {
            if (crit && crit->contains(opt)) c[opt] = (*crit)[opt];
            else                             c[opt] = opt;
        }
        return Choice(instructions, c);
    }

    // ---- error extraction ---------------------------------------------------

    // Pull the human-readable message out of either provider error envelope.
    inline std::string ExtractErrorMessage(const nlohmann::json& envelope)
    {
        if (!envelope.is_object()) return {};
        for (const char* key : {"error", "detail"})
        {
            if (!envelope.contains(key)) continue;
            const auto& e = envelope[key];
            if (e.is_string()) return e.get<std::string>();
            if (e.is_object() && e.contains("message") && e["message"].is_string())
                return e["message"].get<std::string>();
            return e.dump();
        }
        return {};
    }

    // ---- response parser ----------------------------------------------------

    // Parse a 200 body. Returns false (and sets out.error) on any envelope
    // problem: unparseable, provider error payload, missing/ill-typed answers.
    // Tolerant of extra fields (OpenRouter adds id/provider/usage.cost).
    inline bool ParseResponse(const std::string& raw, Result& out)
    {
        out.ok = false;
        out.error.clear();
        out.answers.clear();

        nlohmann::json env;
        try { env = nlohmann::json::parse(raw); }
        catch (const std::exception& e)
        {
            out.error = std::string("envelope parse error: ") + e.what();
            return false;
        }
        if (!env.is_object())
        {
            out.error = "envelope is not an object";
            return false;
        }
        std::string providerErr = ExtractErrorMessage(env);
        if (!providerErr.empty())
        {
            out.error = "provider error: " + providerErr;
            return false;
        }
        if (!env.contains("answers") || !env["answers"].is_object())
        {
            out.error = "envelope missing 'answers' object";
            return false;
        }

        if (env.contains("model") && env["model"].is_string())
            out.model = env["model"].get<std::string>();
        if (env.contains("usage") && env["usage"].is_object())
        {
            const auto& u = env["usage"];
            if (u.contains("input_tokens")  && u["input_tokens"].is_number())  out.inputTokens  = u["input_tokens"].get<uint32_t>();
            if (u.contains("output_tokens") && u["output_tokens"].is_number()) out.outputTokens = u["output_tokens"].get<uint32_t>();
            if (u.contains("cost")          && u["cost"].is_number())          out.costUsd      = u["cost"].get<double>();
        }

        for (auto it = env["answers"].begin(); it != env["answers"].end(); ++it)
        {
            const auto& a = it.value();
            if (!a.is_object() || !a.contains("type") || !a["type"].is_string())
            {
                out.error = "answer '" + it.key() + "' is not a typed object";
                return false;
            }
            Answer ans;
            ans.type = a["type"].get<std::string>();
            if (ans.type == "choice")
            {
                if (!a.contains("choice") || !a["choice"].is_string())
                {
                    out.error = "choice answer '" + it.key() + "' missing 'choice'";
                    return false;
                }
                ans.choice = a["choice"].get<std::string>();
            }
            else if (ans.type == "noul")
            {
                if (!a.contains("noul") || !a["noul"].is_number())
                {
                    out.error = "noul answer '" + it.key() + "' missing 'noul'";
                    return false;
                }
                ans.noul = a["noul"].get<float>();
            }
            else if (ans.type == "score")
            {
                if (!a.contains("score") || !a["score"].is_number())
                {
                    out.error = "score answer '" + it.key() + "' missing 'score'";
                    return false;
                }
                ans.score = a["score"].get<float>();
            }
            else
            {
                out.error = "answer '" + it.key() + "' has unknown type '" + ans.type + "'";
                return false;
            }
            if (a.contains("confidence") && a["confidence"].is_number())
                ans.confidence = a["confidence"].get<float>();
            if (a.contains("probabilities") && a["probabilities"].is_object())
                for (auto p = a["probabilities"].begin(); p != a["probabilities"].end(); ++p)
                    if (p.value().is_number()) ans.probabilities[p.key()] = p.value().get<float>();
            out.answers[it.key()] = std::move(ans);
        }
        out.ok = true;
        return true;
    }

    // The type guarantee says a choice is always one of the criteria keys; the
    // network is not trusted to honour it. Also rejects answers for questions
    // that were never asked and missing answers for questions that were.
    inline bool ValidateAgainstQuestions(const nlohmann::json& questions, Result& out)
    {
        if (!questions.is_object()) { out.ok = false; out.error = "questions is not an object"; return false; }
        for (auto q = questions.begin(); q != questions.end(); ++q)
        {
            const Answer* a = out.Find(q.key());
            if (!a)
            {
                out.ok = false; out.error = "no answer for question '" + q.key() + "'";
                return false;
            }
            const auto& qv = q.value();
            std::string qtype = qv.is_object() && qv.contains("type") && qv["type"].is_string()
                                  ? qv["type"].get<std::string>() : std::string();
            if (qtype != a->type)
            {
                out.ok = false; out.error = "answer '" + q.key() + "' type '" + a->type + "' != asked '" + qtype + "'";
                return false;
            }
            if (qtype == "choice")
            {
                // contains() before operator[] — on a const json an absent key is UB, not a throw.
                if (!qv.contains("criteria") || !qv["criteria"].is_object() || !qv["criteria"].contains(a->choice))
                {
                    out.ok = false; out.error = "choice '" + a->choice + "' for '" + q.key() + "' is not one of the criteria";
                    return false;
                }
            }
        }
        for (const auto& kv : out.answers)
        {
            if (!questions.contains(kv.first))
            {
                out.ok = false; out.error = "unsolicited answer '" + kv.first + "'";
                return false;
            }
        }
        return true;
    }

    // ---- tactical funnel: rules cut the obvious, jev picks among what is left --
    // The article's discipline ("rules cut, Score ranks, Choice picks") applied
    // to the tactical executor: the caller hands in the STATIC jev-eligible set
    // (actions whose registry `required` is {botGuid}, plus the emote/loot-mode
    // actions whose one arg a speculative answer fills) and the tick's live
    // state; this returns the options jev is allowed to choose from THIS tick.
    // Everything the dispatcher would refuse anyway is removed here so a
    // confident jev answer can never be "blocked" — a blocked pick is a wasted
    // tick, and the whole point of the funnel is that the choice set is small
    // and every member is actionable. `other` is appended by the caller.
    // Live facts about the bot this tick that decide which eligible verbs are
    // actually dispatchable. Captured by PrepareBotTick on the world thread.
    struct TacticalTickFacts
    {
        bool inCombat          = false;
        bool botIsDead         = false;
        bool hasMaster         = false;   // PlayerbotAI::master online + in world (bot_follow needs one)
        bool rezRequested      = false;   // a resurrection popup is pending (bot_accept_resurrect_request)
        bool questEnderNear    = false;   // an NPC in range ends one of the bot's completed quests (bot_turn_in_quest)
        bool vendorNear        = false;   // a vendor in range (bot_sell_junk, bot_maintenance restock)
        bool repairNear        = false;   // an armorer in range (bot_maintenance repair)
        uint32_t trainableSpells = 0;     // a class trainer in range can teach this many now (bot_train_spells)
        uint32_t greyItems     = 0;       // sellable junk in the bags
        uint32_t bagFree       = 0;
        uint32_t durabilityMinPct = 100;
        std::vector<std::string> coolingDown;   // no-progress backoff (tactical.cpp RecordActionOutcome)
        bool ambientEnabled    = true;    // Tactical.Ambient.Enable — bot_emote is converted to idle otherwise
        bool actionToolsAllowed = true;   // Mcp.AllowActionTools — every action tool is refused otherwise
        bool allowCombatOverride = false;
        bool repliesDisabledInCombat = true;
    };

    inline std::vector<std::string> PruneTacticalOptions(
        const std::vector<std::string>& eligible,
        const TacticalTickFacts& f,
        const std::vector<std::string>& combatBlocked,
        const std::vector<std::string>& requireConfirmation)
    {
        auto contains = [](const std::vector<std::string>& v, const std::string& s) {
            for (const auto& x : v) if (x == s) return true;
            return false;
        };
        std::vector<std::string> out;
        out.reserve(eligible.size());
        for (const std::string& a : eligible)
        {
            if (a == "tactical_idle") { out.push_back(a); continue; }       // always dispatchable
            if (!f.actionToolsAllowed) continue;                            // every action tool refused
            if (contains(requireConfirmation, a)) continue;                 // HITL gate refuses these
            if (f.inCombat && !f.allowCombatOverride && contains(combatBlocked, a)) continue;
            if (a == "bot_emote" || a == "bot_say" || a == "bot_yell")
            {
                if (!f.ambientEnabled) continue;                            // converted to idle by the dispatcher
                if (f.inCombat && f.repliesDisabledInCombat) continue;      // ambient suppressed in combat
            }
            if (a == "bot_follow" && !f.hasMaster) continue;                // follows the registered master — none, error
            if (a == "bot_accept_resurrect_request" && !f.rezRequested) continue;   // no popup — no-op
            // "quests_turnin_ready > 0" alone put a turn-in on every tick at an NPC
            // that could not take it (2026-09-25); offer it only when one can.
            if (a == "bot_turn_in_quest" && (!f.questEnderNear || f.inCombat)) continue;   // the tool refuses in combat, override or not
            // The maintenance verbs' own rules ("only when the snapshot shows the
            // need AND the right NPC is near") — now that the snapshot carries it.
            if (a == "bot_sell_junk" && !(f.vendorNear && f.greyItems > 0)) continue;
            if (a == "bot_maintenance" &&
                !((f.repairNear && f.durabilityMinPct < 50) || (f.vendorNear && f.bagFree <= 2))) continue;
            if (a == "bot_train_spells" && (f.trainableSpells == 0 || f.inCombat)) continue;   // the tool refuses in combat
            if (contains(f.coolingDown, a)) continue;                       // did nothing twice — backing off
            // Rez verbs only mean something while dead; offering them to a live
            // bot invites a confident-but-inert pick every tick.
            const bool rezVerb = a == "bot_revive" || a == "bot_release_spirit" ||
                                 a == "bot_self_reincarnate" || a == "bot_accept_resurrect_request";
            if (rezVerb && !f.botIsDead) continue;
            if (!rezVerb && f.botIsDead) continue;                          // a corpse can only rez or wait
            out.push_back(a);
        }
        return out;
    }

    // ---- leader playbook relevance filter (PR 3) ------------------------------
    // prompts/gateway_leader.md is ~200 KB: a short header, ~78 workflow rows
    // (each `- "trigger", "trigger", ... -> tool(...) ...`), a short footer —
    // and the whole thing rides in EVERY leader gateway request. The article's
    // "compaction is a relevance filter" applied: score each row against the
    // player's message with one Noul per row (one request), keep the rows that
    // clear a threshold (bounded below and above), always keep header + footer.

    struct PlaybookRow
    {
        std::string prefix;   // the trigger phrases before " -> " — what jev judges
        std::string full;     // the whole row — what the model gets when kept
    };

    struct ParsedPlaybook
    {
        std::string header;              // everything before the first row
        std::vector<PlaybookRow> rows;
        std::string footer;              // non-row lines after the first row, in order
    };

    inline bool IsPlaybookRowLine(const std::string& line)
    {
        return line.size() >= 3 && line[0] == '-' && line[1] == ' ' && line[2] == '"';
    }

    inline ParsedPlaybook ParsePlaybook(const std::string& text)
    {
        ParsedPlaybook out;
        bool seenRow = false;
        size_t pos = 0;
        while (pos <= text.size())
        {
            size_t nl = text.find('\n', pos);
            std::string line = text.substr(pos, nl == std::string::npos ? std::string::npos : nl - pos);
            if (!line.empty() && line.back() == '\r') line.pop_back();
            if (IsPlaybookRowLine(line))
            {
                seenRow = true;
                PlaybookRow r;
                r.full = line;
                size_t arrow = line.find(" -> ");
                // The whole trigger list, never truncated: two live rows exceed
                // 400 chars and a cut there dropped a row's exit-phrase triggers.
                r.prefix = arrow == std::string::npos ? line.substr(2) : line.substr(2, arrow - 2);
                out.rows.push_back(std::move(r));
            }
            else if (!seenRow)
                out.header += line + "\n";
            else if (!line.empty())
                out.footer += line + "\n";
            if (nl == std::string::npos) break;
            pos = nl + 1;
        }
        return out;
    }

    // Which rows to keep, in original order: every row with relevance >= minRel,
    // padded up to minRows with the next-best and capped at maxRows. A row list
    // shorter than minRows is returned whole.
    inline std::vector<size_t> SelectPlaybookRows(const std::vector<float>& relevance,
                                                  float minRel, size_t minRows, size_t maxRows)
    {
        std::vector<size_t> order(relevance.size());
        for (size_t i = 0; i < order.size(); ++i) order[i] = i;
        std::stable_sort(order.begin(), order.end(),
                         [&](size_t a, size_t b) { return relevance[a] > relevance[b]; });
        size_t keep = 0;
        while (keep < order.size() && relevance[order[keep]] >= minRel) ++keep;
        if (keep < minRows) keep = std::min(minRows, order.size());
        if (maxRows > 0 && keep > maxRows) keep = maxRows;
        std::vector<size_t> chosen(order.begin(), order.begin() + keep);
        std::sort(chosen.begin(), chosen.end());
        return chosen;
    }

    inline std::string RenderPlaybook(const ParsedPlaybook& pb, const std::vector<size_t>& keep)
    {
        std::string out = pb.header;
        for (size_t i : keep)
            if (i < pb.rows.size()) out += pb.rows[i].full + "\n";
        out += pb.footer;
        return out;
    }

    // ---- breaker ------------------------------------------------------------
    // jev's own breaker, separate from TacticalInference's URL-keyed one. Only
    // transport / HTTP failures feed it; low confidence and ineligible answers
    // are fall-throughs, never failures.
    struct Breaker
    {
        int     failures    = 0;
        int64_t openUntilMs = 0;
        int     threshold   = 3;
        int64_t cooldownMs  = 60000;

        bool IsOpen(int64_t nowMs) const { return openUntilMs != 0 && nowMs < openUntilMs; }

        // Returns true when this failure tripped the breaker open.
        bool RecordFailure(int64_t nowMs)
        {
            if (IsOpen(nowMs)) return false;
            ++failures;
            if (failures >= threshold)
            {
                openUntilMs = nowMs + cooldownMs;
                failures = 0;
                return true;
            }
            return false;
        }

        void RecordSuccess() { failures = 0; openUntilMs = 0; }
    };

    // ---- deadline arithmetic ------------------------------------------------
    // One absolute deadline per chain (jev -> LLM tier -> deterministic); each
    // tier gets what is left, and the LLM tier is skipped outright when the
    // remainder cannot fit a useful call.
    struct Deadline
    {
        int64_t deadlineMs = 0;
        static Deadline FromNow(int64_t nowMs, uint32_t budgetMs) { return Deadline{nowMs + static_cast<int64_t>(budgetMs)}; }
        int64_t RemainingMs(int64_t nowMs) const { return deadlineMs - nowMs; }
    };

    // jev's per-call cap: the configured timeout, clamped to what remains. 0 = skip.
    inline uint32_t JevCapMs(uint32_t configuredMs, int64_t remainingMs)
    {
        if (remainingMs <= 0 || configuredMs == 0) return 0;
        return remainingMs < static_cast<int64_t>(configuredMs) ? static_cast<uint32_t>(remainingMs) : configuredMs;
    }

    // Whether the LLM tier still fits after jev — minMs is the floor below
    // which a DeepSeek call is not worth starting (it would time out anyway).
    inline bool LlmTierFits(int64_t remainingMs, uint32_t minMs)
    {
        return remainingMs >= static_cast<int64_t>(minMs);
    }
}

#endif // MOD_OLLAMA_CHAT_JEV_CORE_H
