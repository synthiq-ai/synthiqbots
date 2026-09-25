#include "mod-ollama-chat_httpclient.h"
#include "mod-ollama-chat_config.h"

// Include cpp-httplib for HTTP functionality
#include <httplib.h>

#include "Log.h"
#include <algorithm>
#include <chrono>
#include <sstream>
#include <regex>
#include <memory>

namespace
{
    std::string SafeLogSnippet(const std::string& text, size_t maxChars = 512)
    {
        std::string out = text.substr(0, std::min(maxChars, text.size()));
        for (char& c : out)
        {
            unsigned char uc = static_cast<unsigned char>(c);
            if (c == '{') c = '(';
            else if (c == '}') c = ')';
            else if (c == '\n' || c == '\r' || c == '\t' || uc < 0x20) c = ' ';
        }
        if (text.size() > maxChars)
            out += "...";
        return out;
    }
}

OllamaHttpClient::OllamaHttpClient()
    : m_timeout(120), m_timeoutMs(120000), m_available(true)
{
    // Default 120 second timeout
}

OllamaHttpClient::~OllamaHttpClient()
{
}

std::string OllamaHttpClient::Post(const std::string& url, const std::string& jsonData)
{
    return Post(url, jsonData, {});
}

std::string OllamaHttpClient::Post(const std::string& url, const std::string& jsonData,
                                    const std::vector<std::pair<std::string, std::string>>& extraHeaders)
{
    try
    {
        // Parse URL to extract host and path
        std::regex urlRegex(R"(^(https?)://([^:/]+)(?::(\d+))?(/.*)?$)");
        std::smatch match;

        if (!std::regex_match(url, match, urlRegex))
        {
            LOG_INFO("server.loading", "[Ollama Chat] Invalid URL format: {}", url);
            return "";
        }

        std::string protocol = match[1].str();
        std::string host = match[2].str();
        int port = 11434;  // Default Ollama port
        if (match[3].matched)
        {
            port = std::stoi(match[3].str());
        }
        else if (protocol == "https")
        {
            port = 443;  // HTTPS default
        }
        else if (protocol == "http")
        {
            port = 11434;  // Ollama default port for HTTP
        }

        std::string path = match[4].matched ? match[4].str() : "/";

        if(g_DebugEnabled)
        {
            LOG_INFO("server.loading", "[Ollama Chat] HTTP Request - Protocol: {}, Host: {}, Port: {}, Path: {}",
                protocol, host, port, path);
        }

        // Build headers
        httplib::Headers headers = {
            {"Content-Type", "application/json"},
            {"User-Agent", "AzerothCore-OllamaChat/1.0"},
            {"Accept", "application/json"}
        };

        // Add ngrok bypass header if this is an ngrok URL
        if (host.find("ngrok") != std::string::npos || host.find("ngrok-free.app") != std::string::npos) {
            headers.emplace("ngrok-skip-browser-warning", "true");
            if(g_DebugEnabled) {
                LOG_INFO("server.loading", "[Ollama Chat] Added ngrok bypass header");
            }
        }

        // Merge extra headers
        for (const auto& h : extraHeaders)
        {
            headers.emplace(h.first, h.second);
        }

        // Connect timeout is deliberately SHORT and separate from the read timeout.
        // Measured 2026-09-20 on game-host: a good TCP connect to api.deepseek.com is
        // ~140 ms, but the host's egress drops roughly 1 in 5 SYNs to any public
        // host (8/10 host-side, 5/10 container-side to github). With connect ==
        // read timeout, one lost SYN silently ate the whole 12-30 s budget and the
        // tactical loop logged it as inference_failed. 3 s is ~20x a healthy
        // connect and leaves the read timeout for the model's actual thinking.
        // Millisecond-exact so a chain-budgeted caller (jev tier -> this LLM
        // tier -> deterministic) can hand over a sub-second remainder instead
        // of rounding it up to a whole second it does not have.
        const std::chrono::milliseconds totalTimeout(std::max<uint32_t>(1u, m_timeoutMs));
        const std::chrono::milliseconds connectTimeout(std::max<uint32_t>(100u, std::min<uint32_t>(3000u, m_timeoutMs)));

        // Retry ONCE, and only on a TRANSPORT failure (no response at all: lost
        // SYN, reset, connect timeout). Never on an HTTP status — the request may
        // have been executed and a POST is not assumed idempotent. Ollama and
        // OpenAI-style completions are stateless, so a second connect attempt
        // after a dropped SYN is safe and is exactly what a lossy network needs.
        // The whole call — both attempts, connect + TLS + request each — stays
        // inside m_timeoutMs: set_max_timeout bounds one attempt end to end, and
        // the retry only gets whatever the first attempt left over. Without this
        // a lost SYN followed by a slow read could take ~2x the budget, which on
        // the intent path means the world thread.
        const auto deadline = std::chrono::steady_clock::now() + totalTimeout;
        httplib::Result response;
        for (int attempt = 1; attempt <= 2; ++attempt)
        {
            const auto remaining = std::chrono::duration_cast<std::chrono::milliseconds>(
                deadline - std::chrono::steady_clock::now());
            if (attempt == 2 && remaining < std::chrono::milliseconds(200))
            {
                LOG_WARN("server.loading", "[Ollama Chat] HTTP transport failure to {}:{}{} — no budget left for a retry ({} ms)",
                         host, port, path, remaining.count());
                break;
            }
            const auto attemptTimeout = std::max(std::chrono::milliseconds(1), remaining);
            const auto attemptConnect = std::min(connectTimeout, attemptTimeout);

            if (protocol == "https") {
#ifdef CPPHTTPLIB_OPENSSL_SUPPORT
                httplib::SSLClient sslClient(host, port);
                // Disable SSL verification for ngrok and self-signed certificates
                sslClient.enable_server_certificate_verification(false);
                sslClient.set_connection_timeout(attemptConnect);
                sslClient.set_read_timeout(attemptTimeout);
                sslClient.set_write_timeout(attemptTimeout);
                sslClient.set_max_timeout(attemptTimeout);

                if(g_DebugEnabled) {
                    LOG_INFO("server.loading", "[Ollama Chat] Using SSL client for HTTPS connection");
                }

                // Make POST request with SSL client
                response = sslClient.Post(path, headers, jsonData, "application/json");
#else
                LOG_ERROR("server.loading", "[Ollama Chat] HTTPS requested but SSL support not available.");
                LOG_ERROR("server.loading", "[Ollama Chat] Please rebuild with OpenSSL support enabled.");
                LOG_ERROR("server.loading", "[Ollama Chat] See CMake output for OpenSSL installation instructions.");
                return "";
#endif
            } else {
                httplib::Client client(host, port);
                client.set_connection_timeout(attemptConnect);
                client.set_read_timeout(attemptTimeout);
                client.set_write_timeout(attemptTimeout);
                client.set_max_timeout(attemptTimeout);

                if(g_DebugEnabled) {
                    LOG_INFO("server.loading", "[Ollama Chat] Using standard HTTP client");
                }

                // Make POST request with regular client
                response = client.Post(path, headers, jsonData, "application/json");
            }

            if (response) break;   // got an HTTP response (any status) — do not retry
            if (attempt == 1)
                LOG_WARN("server.loading", "[Ollama Chat] HTTP transport failure to {}:{}{} (attempt 1/2, err={}) — retrying once",
                         host, port, path, httplib::to_string(response.error()));
        }

        if (!response)
        {
            LOG_ERROR("server.loading", "[Ollama Chat] HTTP request failed - no response from {}:{}{} after 2 attempts (err={})",
                      host, port, path, httplib::to_string(response.error()));
            return "";
        }

        if (response->status != 200)
        {
            LOG_ERROR("server.loading", "[Ollama Chat] HTTP request failed with status: {} for {}:{}{}",
                response->status, host, port, path);
            if(g_DebugEnabled)
            {
                LOG_INFO("server.loading", "[Ollama Chat] Response body chars={}, preview=\"{}\"",
                    response->body.size(), SafeLogSnippet(response->body));
            }
            return "";
        }

        if(g_DebugEnabled)
        {
            LOG_INFO("server.loading", "[Ollama Chat] HTTP request successful, response length: {}", response->body.length());
        }

        return response->body;
    }
    catch (const std::exception& e)
    {
        LOG_ERROR("server.loading", "[Ollama Chat] HTTP client exception: {}", e.what());
        return "";
    }
}

bool OllamaHttpClient::PostStream(const std::string& url, const std::string& jsonData,
                                  const std::vector<std::pair<std::string, std::string>>& extraHeaders,
                                  const std::function<bool(const char* data, size_t len)>& onChunk)
{
    try
    {
        std::regex urlRegex(R"(^(https?)://([^:/]+)(?::(\d+))?(/.*)?$)");
        std::smatch match;
        if (!std::regex_match(url, match, urlRegex))
        {
            LOG_ERROR("server.loading", "[Ollama Chat] PostStream: invalid URL '{}'", url);
            return false;
        }

        std::string protocol = match[1].str();
        std::string host = match[2].str();
        int port = (protocol == "https") ? 443 : 11434;
        if (match[3].matched)
            port = std::stoi(match[3].str());

        std::string path = match[4].matched ? match[4].str() : "/";

        httplib::Headers headers = {
            {"Content-Type", "application/json"},
            {"User-Agent", "AzerothCore-OllamaChat/1.0"},
            {"Accept", "text/event-stream"}
        };

        if (host.find("ngrok") != std::string::npos)
            headers.emplace("ngrok-skip-browser-warning", "true");

        for (const auto& h : extraHeaders)
            headers.emplace(h.first, h.second);

        auto contentReceiver = [&onChunk](const char* data, size_t length) -> bool {
            return onChunk(data, length);
        };

        httplib::Result response;
        if (protocol == "https")
        {
#ifdef CPPHTTPLIB_OPENSSL_SUPPORT
            httplib::SSLClient client(host, port);
            client.enable_server_certificate_verification(false);
            client.set_connection_timeout(m_timeout);
            client.set_read_timeout(m_timeout);
            client.set_write_timeout(m_timeout);
            response = client.Post(path, headers, jsonData, "application/json", contentReceiver);
#else
            LOG_ERROR("server.loading", "[Ollama Chat] PostStream: HTTPS requested but SSL support not built");
            return false;
#endif
        }
        else
        {
            httplib::Client client(host, port);
            client.set_connection_timeout(m_timeout);
            client.set_read_timeout(m_timeout);
            client.set_write_timeout(m_timeout);
            response = client.Post(path, headers, jsonData, "application/json", contentReceiver);
        }

        if (!response)
        {
            LOG_ERROR("server.loading", "[Ollama Chat] PostStream: no response from {}:{}{}", host, port, path);
            return false;
        }
        if (response->status != 200)
        {
            LOG_ERROR("server.loading", "[Ollama Chat] PostStream: status {} from {}:{}{}",
                      response->status, host, port, path);
            return false;
        }
        return true;
    }
    catch (const std::exception& e)
    {
        LOG_ERROR("server.loading", "[Ollama Chat] PostStream exception: {}", e.what());
        return false;
    }
}

bool OllamaHttpClient::Get(const std::string& url,
                            const std::vector<std::pair<std::string, std::string>>& extraHeaders,
                            int& outStatus,
                            std::string& outBody)
{
    outStatus = 0;
    outBody.clear();

    try
    {
        std::regex urlRegex(R"(^(https?)://([^:/]+)(?::(\d+))?(/.*)?$)");
        std::smatch match;
        if (!std::regex_match(url, match, urlRegex))
        {
            LOG_ERROR("server.loading", "[Ollama Chat] Get: invalid URL '{}'", url);
            return false;
        }

        std::string protocol = match[1].str();
        std::string host = match[2].str();
        int port = (protocol == "https") ? 443 : 80;
        if (match[3].matched)
            port = std::stoi(match[3].str());

        std::string path = match[4].matched ? match[4].str() : "/";

        httplib::Headers headers = {
            {"User-Agent", "AzerothCore-OllamaChat/1.0"},
            {"Accept", "application/json"}
        };
        for (const auto& h : extraHeaders)
            headers.emplace(h.first, h.second);

        httplib::Result response;
        if (protocol == "https")
        {
#ifdef CPPHTTPLIB_OPENSSL_SUPPORT
            httplib::SSLClient client(host, port);
            client.enable_server_certificate_verification(false);
            client.set_connection_timeout(m_timeout);
            client.set_read_timeout(m_timeout);
            client.set_write_timeout(m_timeout);
            response = client.Get(path, headers);
#else
            LOG_ERROR("server.loading", "[Ollama Chat] Get: HTTPS requested but SSL support not built");
            return false;
#endif
        }
        else
        {
            httplib::Client client(host, port);
            client.set_connection_timeout(m_timeout);
            client.set_read_timeout(m_timeout);
            client.set_write_timeout(m_timeout);
            response = client.Get(path, headers);
        }

        if (!response)
        {
            LOG_ERROR("server.loading", "[Ollama Chat] Get: no response from {}:{}{}", host, port, path);
            return false;
        }

        outStatus = response->status;
        outBody = response->body;
        return true;
    }
    catch (const std::exception& e)
    {
        LOG_ERROR("server.loading", "[Ollama Chat] Get exception: {}", e.what());
        return false;
    }
}

void OllamaHttpClient::SetTimeout(int seconds)
{
    m_timeout   = seconds;
    m_timeoutMs = seconds <= 0 ? 0u : static_cast<uint32_t>(seconds) * 1000u;
}

void OllamaHttpClient::SetTimeoutMs(uint32_t milliseconds)
{
    m_timeoutMs = milliseconds;
    // Legacy PostStream/Get paths still think in whole seconds — round UP so a
    // 1500 ms budget never becomes a 1 s stream/GET timeout.
    m_timeout   = static_cast<int>(std::max<uint32_t>(1u, (milliseconds + 999u) / 1000u));
}

bool OllamaHttpClient::IsAvailable() const
{
    return m_available;
}
