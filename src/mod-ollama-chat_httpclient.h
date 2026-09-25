#ifndef OLLAMA_HTTP_CLIENT_H
#define OLLAMA_HTTP_CLIENT_H

#include <string>
#include <vector>
#include <utility>
#include <functional>

class OllamaHttpClient
{
public:
    OllamaHttpClient();
    ~OllamaHttpClient();

    // Make HTTP POST request to Ollama API
    std::string Post(const std::string& url, const std::string& jsonData);

    // Make HTTP POST request with extra headers (for gateway/custom endpoints)
    std::string Post(const std::string& url, const std::string& jsonData,
                     const std::vector<std::pair<std::string, std::string>>& extraHeaders);

    // Streaming POST: invokes onChunk for every byte block received.
    // Returns true when the request completed cleanly (HTTP 200), false on any error.
    // The callback should return true to continue receiving, false to abort.
    bool PostStream(const std::string& url, const std::string& jsonData,
                    const std::vector<std::pair<std::string, std::string>>& extraHeaders,
                    const std::function<bool(const char* data, size_t len)>& onChunk);

    // HTTP GET. Returns true if a response was received (any HTTP status).
    // outStatus = HTTP status code (0 on transport failure).
    // outBody   = response body (populated even on non-2xx; empty on transport failure).
    // Used by the ops-api proxy tools where the caller needs the upstream status to
    // surface in the MCP error reply.
    bool Get(const std::string& url,
             const std::vector<std::pair<std::string, std::string>>& extraHeaders,
             int& outStatus,
             std::string& outBody);

    // Set timeout for requests (in seconds). Whole-second granularity is what
    // the legacy callers used; new chain-budgeted callers use SetTimeoutMs.
    void SetTimeout(int seconds);

    // Set timeout for requests in milliseconds. Post() honours this exactly
    // (chrono timeouts); PostStream/Get round up to whole seconds.
    void SetTimeoutMs(uint32_t milliseconds);

    // Check if HTTP client is available
    bool IsAvailable() const;

private:
    int m_timeout;          // seconds, rounded up from m_timeoutMs (legacy paths)
    uint32_t m_timeoutMs;   // exact budget for Post()
    bool m_available;
};

#endif // OLLAMA_HTTP_CLIENT_H