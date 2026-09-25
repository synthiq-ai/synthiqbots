#include "mod-ollama-chat_llmwire.h"
#include "mod-ollama-chat_config.h"

#include <algorithm>
#include <cctype>

namespace LlmWire
{
    namespace
    {
        // Strip query string and fragment, drop one trailing slash, lower-case.
        std::string NormalisedPath(const std::string& url)
        {
            std::string p = url;
            const auto q = p.find_first_of("?#");
            if (q != std::string::npos) p.erase(q);
            while (!p.empty() && p.back() == '/') p.pop_back();
            std::transform(p.begin(), p.end(), p.begin(),
                           [](unsigned char c) { return static_cast<char>(std::tolower(c)); });
            return p;
        }

        bool EndsWith(const std::string& s, const std::string& suffix)
        {
            return s.size() >= suffix.size() &&
                   s.compare(s.size() - suffix.size(), suffix.size(), suffix) == 0;
        }

        // Lower-cased host part of a URL ("https://API.DeepSeek.com:443/v1/x" -> "api.deepseek.com").
        std::string HostOf(const std::string& url)
        {
            std::string u = NormalisedPath(url);
            const auto scheme = u.find("://");
            std::string rest = scheme == std::string::npos ? u : u.substr(scheme + 3);
            const auto end = rest.find_first_of("/:");
            return end == std::string::npos ? rest : rest.substr(0, end);
        }

        // Provider-specific extras (DeepSeek's `thinking` field) are keyed on the host.
        bool IsDeepSeekHost(const std::string& url)
        {
            const std::string h = HostOf(url);
            return h == "deepseek.com" || EndsWith(h, ".deepseek.com");
        }
    } // namespace

    Format Detect(const std::string& url)
    {
        return EndsWith(NormalisedPath(url), "/chat/completions") ? Format::OpenAiChat
                                                                  : Format::Ollama;
    }

    const char* FormatName(const std::string& url)
    {
        return Detect(url) == Format::OpenAiChat ? "openai-chat" : "ollama";
    }

    nlohmann::json BuildRequest(const std::string& url, const std::string& model,
                                const std::string& system, const std::string& prompt,
                                int numCtx, bool jsonMode, bool noThink)
    {
        if (Detect(url) == Format::OpenAiChat)
        {
            nlohmann::json req = {
                {"model",  model},
                {"stream", false},
                {"messages", nlohmann::json::array({
                    {{"role", "system"}, {"content", system}},
                    {{"role", "user"},   {"content", prompt}}
                })}
            };
            if (jsonMode)
                req["response_format"] = nlohmann::json{{"type", "json_object"}};
            // DeepSeek's `thinking` toggle is the OpenAI-path twin of Ollama's
            // think:false. Measured 2026-09-20 on deepseek-flash with the real
            // tactical prompt: default = 828 hidden reasoning tokens, 6.2 s; with
            // {"type":"disabled"} = 0 reasoning tokens, 15 completion tokens, 1.8 s,
            // same action. The classifier timeout is 5 s, so without this every
            // whisper would fall through to the paid gateway. Host-gated because
            // the field is DeepSeek's — OpenAI proper rejects unknown params
            // (its own knob is reasoning_effort, a different field), so other
            // providers get no thinking field at all.
            if (noThink && IsDeepSeekHost(url))
                req["thinking"] = nlohmann::json{{"type", "disabled"}};
            // numCtx / keep_alive have no OpenAI equivalent: context is a property
            // of the hosted model and residency is the provider's problem.
            return req;
        }

        // Ollama /api/generate — byte-identical to what every call site built before
        // this adapter existed, so the Ollama path cannot regress.
        nlohmann::json req = {
            {"model",      model},
            {"system",     system},
            {"prompt",     prompt},
            {"stream",     false},
            {"keep_alive", "30m"},   // keep the model GPU-resident between ticks
        };
        if (jsonMode) req["format"] = "json";   // Ollama constrains output to JSON when supported
        if (noThink)  req["think"]  = false;    // no reasoning preamble (Gemma 4 / qwen3 default to on)
        if (numCtx > 0)
            req["options"] = nlohmann::json{{"num_ctx", numCtx}};   // shrink KV cache on borderline-fit models
        return req;
    }

    std::vector<std::pair<std::string, std::string>> Headers(const std::string& url)
    {
        std::vector<std::pair<std::string, std::string>> h;
        if (Detect(url) == Format::OpenAiChat && !g_OpenAiCompatApiKey.empty())
            h.emplace_back("Authorization", "Bearer " + g_OpenAiCompatApiKey);
        return h;
    }

    bool ExtractText(const std::string& url, const std::string& raw,
                     std::string& outText, std::string& outErr)
    {
        outText.clear();
        outErr.clear();

        nlohmann::json envelope;
        try { envelope = nlohmann::json::parse(raw); }
        catch (const std::exception& e)
        {
            outErr = std::string("envelope parse error: ") + e.what();
            return false;
        }

        // Both shapes can carry an error object; OpenAI-compatible providers put the
        // useful sentence in error.message ("Authentication Fails", "json_object
        // requires the word json in messages", ...). Surface it verbatim.
        if (envelope.contains("error"))
        {
            const auto& err = envelope["error"];
            if (err.is_object() && err.contains("message") && err["message"].is_string())
                outErr = "provider error: " + err["message"].get<std::string>();
            else if (err.is_string())
                outErr = "provider error: " + err.get<std::string>();
            else
                outErr = "provider error: " + err.dump();
            return false;
        }

        if (Detect(url) == Format::OpenAiChat)
        {
            if (!envelope.contains("choices") || !envelope["choices"].is_array() ||
                envelope["choices"].empty())
            {
                outErr = "envelope missing 'choices' array";
                return false;
            }
            const auto& first = envelope["choices"][0];
            if (!first.contains("message") || !first["message"].is_object() ||
                !first["message"].contains("content") || !first["message"]["content"].is_string())
            {
                outErr = "envelope missing 'choices[0].message.content' string";
                return false;
            }
            outText = first["message"]["content"].get<std::string>();
            return true;
        }

        if (!envelope.contains("response") || !envelope["response"].is_string())
        {
            outErr = "envelope missing 'response' string";
            return false;
        }
        outText = envelope["response"].get<std::string>();
        return true;
    }
}
