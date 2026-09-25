#include "mod-ollama-chat_opsapi.h"
#include "mod-ollama-chat_config.h"
#include "mod-ollama-chat_httpclient.h"

#include "Log.h"

#include <cstdio>
#include <sstream>

namespace
{
    std::string PercentEncode(const std::string& v)
    {
        std::string out;
        out.reserve(v.size());
        for (unsigned char c : v)
        {
            bool unreserved = (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z')
                           || (c >= '0' && c <= '9')
                           || c == '-' || c == '_' || c == '.' || c == '~';
            if (unreserved)
            {
                out.push_back(static_cast<char>(c));
            }
            else
            {
                char buf[4];
                std::snprintf(buf, sizeof(buf), "%%%02X", c);
                out.append(buf);
            }
        }
        return out;
    }

    std::string BuildQueryString(const std::vector<std::pair<std::string, std::string>>& params)
    {
        std::string q;
        bool first = true;
        for (const auto& kv : params)
        {
            if (kv.second.empty())
                continue;
            q.append(first ? "?" : "&");
            first = false;
            q.append(PercentEncode(kv.first));
            q.push_back('=');
            q.append(PercentEncode(kv.second));
        }
        return q;
    }

    nlohmann::json MakeError(const std::string& msg)
    {
        return nlohmann::json{{"error", msg}};
    }

    std::string JoinUrlPath(std::string base, const std::string& path)
    {
        while (!base.empty() && base.back() == '/')
            base.pop_back();

        if (path.empty())
            return base;

        if (path.front() == '/')
            return base + path;

        return base + "/" + path;
    }
}

nlohmann::json CallOpsApi(const std::string& path,
                          const std::vector<std::pair<std::string, std::string>>& queryParams)
{
    if (g_OpsUrl.empty())
        return MakeError("opsapi: OllamaChat.Ops.Url is empty");
    if (g_OpsBearerToken.empty())
        return MakeError("opsapi: OllamaChat.Ops.BearerToken is empty");

    std::string url = JoinUrlPath(g_OpsUrl, path) + BuildQueryString(queryParams);

    OllamaHttpClient http;
    http.SetTimeout(g_OpsTimeoutSeconds > 0 ? g_OpsTimeoutSeconds : 10);

    std::vector<std::pair<std::string, std::string>> headers = {
        {"Authorization", "Bearer " + g_OpsBearerToken}
    };

    int status = 0;
    std::string body;
    bool ok = http.Get(url, headers, status, body);

    if (!ok)
        return MakeError("opsapi: transport failure (no response from " + url + ")");

    if (status < 200 || status >= 300)
    {
        std::string snippet = body.substr(0, 256);
        for (char& c : snippet)
            if (c == '\n' || c == '\r' || c == '\t') c = ' ';
        std::ostringstream err;
        err << "opsapi: HTTP " << status;
        if (!snippet.empty())
            err << " — " << snippet;
        return MakeError(err.str());
    }

    try
    {
        return nlohmann::json::parse(body);
    }
    catch (const std::exception& e)
    {
        LOG_ERROR("server.loading", "[Ollama Chat OpsAPI] JSON parse error: {}", e.what());
        return MakeError(std::string("opsapi: response was not valid JSON — ") + e.what());
    }
}
