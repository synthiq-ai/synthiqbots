#ifndef MOD_OLLAMA_CHAT_OPSAPI_H
#define MOD_OLLAMA_CHAT_OPSAPI_H

#include <nlohmann/json.hpp>
#include <string>
#include <utility>
#include <vector>

// Issue a GET to `g_OpsUrl + path + ?k1=v1&k2=v2` with the configured bearer token,
// parse the body as JSON and return it. On any failure (transport, non-2xx,
// non-JSON body) returns {"error": "..."} matching the existing tool-handler
// error convention (see `mod-ollama-chat_mcpserver.cpp` dispatch path).
//
// Empty values in queryParams are skipped — callers can pass everything they
// have without pre-filtering.
nlohmann::json CallOpsApi(const std::string& path,
                          const std::vector<std::pair<std::string, std::string>>& queryParams);

#endif // MOD_OLLAMA_CHAT_OPSAPI_H
