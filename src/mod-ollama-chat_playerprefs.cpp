#include "mod-ollama-chat_playerprefs.h"
#include "DatabaseEnv.h"
#include "Log.h"
#include "ObjectAccessor.h"
#include "Player.h"
#include <mutex>
#include <unordered_map>
#include <unordered_set>
#include <fmt/core.h>

namespace
{
    std::mutex s_prefsMutex;
    std::unordered_set<uint32_t> s_optedOutAccounts;
    std::unordered_map<uint32_t, std::unordered_set<uint64_t>> s_botMutes;
}

void LoadPlayerPreferencesFromDB()
{
    std::lock_guard<std::mutex> lock(s_prefsMutex);
    s_optedOutAccounts.clear();
    s_botMutes.clear();

    QueryResult tableExists = CharacterDatabase.Query(
        "SELECT TABLE_NAME FROM information_schema.tables "
        "WHERE table_schema = 'acore_characters' "
        "AND table_name IN ('mod_ollama_chat_optouts', 'mod_ollama_chat_bot_mutes')");

    if (!tableExists)
    {
        LOG_ERROR("server.loading", "[Ollama Chat] Player-prefs tables missing — source data/sql/characters/base/2026_04_18_optouts_and_mutes.sql");
        return;
    }

    QueryResult outRes = CharacterDatabase.Query("SELECT account_id FROM mod_ollama_chat_optouts");
    if (outRes)
    {
        do {
            s_optedOutAccounts.insert((*outRes)[0].Get<uint32_t>());
        } while (outRes->NextRow());
    }

    QueryResult muteRes = CharacterDatabase.Query("SELECT account_id, bot_guid FROM mod_ollama_chat_bot_mutes");
    if (muteRes)
    {
        do {
            uint32_t aid = (*muteRes)[0].Get<uint32_t>();
            uint64_t bg  = (*muteRes)[1].Get<uint64_t>();
            s_botMutes[aid].insert(bg);
        } while (muteRes->NextRow());
    }

    LOG_INFO("server.loading", "[Ollama Chat] Player prefs loaded: {} opt-outs, {} accounts with mutes",
             s_optedOutAccounts.size(), s_botMutes.size());
}

bool IsAccountOptedOut(uint32_t accountId)
{
    if (accountId == 0) return false;
    std::lock_guard<std::mutex> lock(s_prefsMutex);
    return s_optedOutAccounts.count(accountId) > 0;
}

bool IsBotMutedByAccount(uint32_t accountId, uint64_t botGuid)
{
    if (accountId == 0) return false;
    std::lock_guard<std::mutex> lock(s_prefsMutex);
    auto it = s_botMutes.find(accountId);
    if (it == s_botMutes.end()) return false;
    return it->second.count(botGuid) > 0;
}

bool ShouldSuppressBotResponse(uint32_t accountId, uint64_t botGuid)
{
    if (accountId == 0) return false;
    std::lock_guard<std::mutex> lock(s_prefsMutex);
    if (s_optedOutAccounts.count(accountId) > 0) return true;
    auto it = s_botMutes.find(accountId);
    return it != s_botMutes.end() && it->second.count(botGuid) > 0;
}

void SetAccountOptOut(uint32_t accountId, bool optedOut)
{
    if (accountId == 0) return;
    {
        std::lock_guard<std::mutex> lock(s_prefsMutex);
        if (optedOut)
            s_optedOutAccounts.insert(accountId);
        else
            s_optedOutAccounts.erase(accountId);
    }

    if (optedOut)
        CharacterDatabase.Execute(
            "INSERT IGNORE INTO mod_ollama_chat_optouts (account_id) VALUES ({})", accountId);
    else
        CharacterDatabase.Execute(
            "DELETE FROM mod_ollama_chat_optouts WHERE account_id = {}", accountId);
}

void SetBotMute(uint32_t accountId, uint64_t botGuid, bool muted)
{
    if (accountId == 0 || botGuid == 0) return;
    {
        std::lock_guard<std::mutex> lock(s_prefsMutex);
        if (muted)
            s_botMutes[accountId].insert(botGuid);
        else
        {
            auto it = s_botMutes.find(accountId);
            if (it != s_botMutes.end())
            {
                it->second.erase(botGuid);
                if (it->second.empty())
                    s_botMutes.erase(it);
            }
        }
    }

    if (muted)
        CharacterDatabase.Execute(
            "INSERT IGNORE INTO mod_ollama_chat_bot_mutes (account_id, bot_guid) VALUES ({}, {})",
            accountId, botGuid);
    else
        CharacterDatabase.Execute(
            "DELETE FROM mod_ollama_chat_bot_mutes WHERE account_id = {} AND bot_guid = {}",
            accountId, botGuid);
}

uint64_t LookupBotGuidByName(const std::string& name)
{
    if (name.empty()) return 0;
    Player* p = ObjectAccessor::FindPlayerByName(name);
    if (p) return p->GetGUID().GetRawValue();

    // Fallback: query characters table by name (case-insensitive in MySQL utf8 collations).
    QueryResult res = CharacterDatabase.Query(
        "SELECT guid FROM characters WHERE name = '{}' LIMIT 1",
        name);
    if (!res) return 0;
    return (*res)[0].Get<uint64_t>();
}
