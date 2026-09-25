#ifndef MOD_OLLAMA_CHAT_LLMWIRE_H
#define MOD_OLLAMA_CHAT_LLMWIRE_H

#include <string>
#include <utility>
#include <vector>
#include <nlohmann/json.hpp>

// Wire-format adapter for the module's four "fast" LLM paths — the tactical
// executor, the gateway classifier, and the proactive intent / tick-gate calls.
// Historically all four spoke only Ollama's /api/generate. This lets the SAME
// config knobs (OllamaChat.Tactical.Url, Gateway.OllamaClassifier.Url, per-bot
// tactical overrides) point at any OpenAI-compatible /chat/completions endpoint
// — DeepSeek, OpenAI, LiteLLM, the Synthiq gateway — with the request/response
// shape chosen FROM THE URL. Switching a backend is therefore a conf edit +
// config_reload, not a code change:
//
//   OllamaChat.Tactical.Url   = "https://api.deepseek.com/v1/chat/completions"
//   OllamaChat.Tactical.Model = "deepseek-flash"
//   OllamaChat.OpenAiCompat.ApiKey = "sk-..."      # bearer for OpenAI-format URLs
//
// NOT routed through here, deliberately: the high-reasoning gateway path
// (mod-ollama-chat_gateway.cpp's QueryGatewayAPI) already speaks OpenAI chat with
// tools; and the legacy chat path (mod-ollama-chat_api.cpp, OllamaChat.Url —
// disabled in prod) carries seven Ollama-only sampling knobs and no system
// prompt, so it stays Ollama-only rather than being half-translated.
namespace LlmWire
{
    enum class Format { Ollama, OpenAiChat };

    // A path ending in /chat/completions (trailing slash or query string allowed)
    // is OpenAiChat; anything else (/api/generate, /api/chat, ...) is Ollama.
    Format Detect(const std::string& url);

    // Build the request body in the shape `url` expects.
    //   system/prompt : already-sanitised text (callers keep their SanitizeUTF8).
    //   numCtx        : Ollama options.num_ctx; <= 0 omits it. No OpenAI equivalent.
    //   jsonMode      : Ollama `format:"json"` / OpenAI `response_format:{json_object}`.
    //                   NOTE: OpenAI-compatible providers (DeepSeek included) REJECT
    //                   json_object unless the word "json" appears in the prompt —
    //                   every fast-path prompt file already does; keep it that way.
    //   noThink       : Ollama `think:false` (Gemma/qwen3 reasoning toggle). On an
    //                   OpenAiChat URL whose host is *.deepseek.com this becomes
    //                   DeepSeek's `thinking:{type:"disabled"}` — deepseek-flash DOES
    //                   reason by default (measured: 828 hidden tokens / 6.2 s vs
    //                   0 / 1.8 s disabled, same action), and the classifier's 5 s
    //                   timeout cannot absorb that. Other OpenAI-compatible hosts
    //                   get no thinking field (OpenAI proper rejects unknown params).
    // Ollama-only knobs (keep_alive, options.num_ctx) are never sent to OpenAiChat.
    nlohmann::json BuildRequest(const std::string& url, const std::string& model,
                                const std::string& system, const std::string& prompt,
                                int numCtx, bool jsonMode, bool noThink);

    // Extra headers for a request to `url`. OpenAiChat with a configured
    // OllamaChat.OpenAiCompat.ApiKey -> {"Authorization", "Bearer <key>"}.
    // Ollama, or no key -> empty (the HTTP client adds Content-Type itself).
    std::vector<std::pair<std::string, std::string>> Headers(const std::string& url);

    // Extract the model's reply text from a raw response body. Returns false and
    // fills outErr on any envelope problem: unparseable body, missing field, or an
    // OpenAI-style {"error":{"message":...}} payload (surfaced verbatim so "invalid
    // api key" reads as that, not as "envelope missing 'response'").
    bool ExtractText(const std::string& url, const std::string& raw,
                     std::string& outText, std::string& outErr);

    // Human label for logs / health snapshots: "ollama" or "openai-chat".
    const char* FormatName(const std::string& url);
}

#endif // MOD_OLLAMA_CHAT_LLMWIRE_H
