#include "mod-ollama-chat_autoclaim.h"
#include "mod-ollama-chat_config.h"
#include "mod-ollama-chat_gateway.h"  // for g_GatewayBotGUIDSet

#include "Log.h"
#include "ObjectAccessor.h"
#include "ObjectGuid.h"
#include "PlayerbotAI.h"
#include "PlayerbotMgr.h"
#include "RandomPlayerbotMgr.h"

#include <mutex>
#include <unordered_set>

namespace
{
    // Players whose OnPlayerLogin fired but whose PlayerbotMgr wasn't yet
    // registered (cross-module hook-ordering race). We poll them on
    // OnPlayerBeforeUpdate and claim as soon as mgr shows up.
    std::mutex               s_pendingMutex;
    std::unordered_set<uint32> s_pendingAutoClaim;

    bool PlayerAccountIsWhitelisted(Player* player)
    {
        if (!player || !player->GetSession()) return false;
        if (!g_GatewayAutoClaimOnLogin) return false;
        uint32 accountId = player->GetSession()->GetAccountId();
        return g_GatewayAutoClaimAccountIdsSet.count(accountId) > 0;
    }

    // Skip bot sessions. The headless Overlord character logs itself in via
    // the overlord module — we shouldn't reclaim bots for Overlord or any
    // other bot-flavored session.
    bool ShouldSkip(Player* player)
    {
        if (!player || !player->GetSession()) return true;
        if (player->GetSession()->IsBot()) return true;
        return false;
    }

    // Decide which PlayerbotHolder currently owns this bot so we can logout
    // through the right one. LogoutPlayerBot() in PlayerbotHolder uses
    // GetPlayerBot(guid) — a holder-local map lookup — so calling it on the
    // wrong holder is a silent no-op.
    PlayerbotHolder* FindHolderFor(Player* bot)
    {
        if (!bot) return nullptr;
        PlayerbotAI* ai = PlayerbotsMgr::instance().GetPlayerbotAI(bot);
        if (!ai) return nullptr;

        Player* master = ai->GetMaster();
        if (!master)
        {
            // No human master — it's a rndbot. Always owned by
            // sRandomPlayerbotMgr.
            return &sRandomPlayerbotMgr;
        }

        if (PlayerbotMgr* mgr = PlayerbotsMgr::instance().GetPlayerbotMgr(master))
            return mgr;

        return nullptr;
    }

    void DoClaim(Player* player, PlayerbotMgr* newMgr)
    {
        uint32 accountId = player->GetSession()->GetAccountId();
        LOG_INFO("server.loading",
                 "[Ollama Chat AutoClaim] {} (account={}) claiming {} gateway bots",
                 player->GetName(), accountId, g_GatewayBotGUIDSet.size());

        for (uint64_t botGuidLow : g_GatewayBotGUIDSet)
        {
            ObjectGuid botGuid(HighGuid::Player, static_cast<uint32>(botGuidLow));
            Player* bot = ObjectAccessor::FindConnectedPlayer(botGuid);

            if (bot && bot->IsInWorld())
            {
                if (PlayerbotAI* ai = PlayerbotsMgr::instance().GetPlayerbotAI(bot))
                {
                    if (ai->GetMaster() == player)
                    {
                        LOG_DEBUG("server.loading",
                                  "[Ollama Chat AutoClaim] bot {} already owned by {} — skipping",
                                  bot->GetName(), player->GetName());
                        continue;
                    }
                }

                if (PlayerbotHolder* holder = FindHolderFor(bot))
                {
                    LOG_INFO("server.loading",
                             "[Ollama Chat AutoClaim] despawning bot guid={} to re-home under {}",
                             static_cast<uint32>(botGuidLow), player->GetName());
                    holder->LogoutPlayerBot(botGuid);
                }
            }

            newMgr->AddPlayerBot(botGuid, accountId);
        }
    }
}

GatewayAutoClaimScript::GatewayAutoClaimScript() : PlayerScript("GatewayAutoClaimScript") {}

void GatewayAutoClaimScript::OnPlayerLogin(Player* player)
{
    if (ShouldSkip(player)) return;
    if (!PlayerAccountIsWhitelisted(player)) return;
    if (g_GatewayBotGUIDSet.empty()) return;

    // Defer to the first update tick so mod-playerbots' hook has run and
    // PlayerbotsMgr::GetPlayerbotMgr(player) returns a real mgr.
    uint32 guidLow = player->GetGUID().GetCounter();
    std::lock_guard<std::mutex> lock(s_pendingMutex);
    s_pendingAutoClaim.insert(guidLow);
    LOG_DEBUG("server.loading",
              "[Ollama Chat AutoClaim] queued pending claim for {} (guid={})",
              player->GetName(), guidLow);
}

void GatewayAutoClaimScript::OnPlayerBeforeUpdate(Player* player, uint32 /*p_time*/)
{
    if (!player) return;
    uint32 guidLow = player->GetGUID().GetCounter();

    // Fast-path: if not in pending set, skip with just the mutex-free read.
    {
        std::lock_guard<std::mutex> lock(s_pendingMutex);
        if (!s_pendingAutoClaim.count(guidLow)) return;
    }

    PlayerbotMgr* mgr = PlayerbotsMgr::instance().GetPlayerbotMgr(player);
    if (!mgr) return;  // mod-playerbots still hasn't registered — try next tick

    // Grab-and-clear. If DoClaim somehow re-enters (it shouldn't), we've
    // already removed ourselves from the pending set.
    {
        std::lock_guard<std::mutex> lock(s_pendingMutex);
        s_pendingAutoClaim.erase(guidLow);
    }

    DoClaim(player, mgr);
}

void GatewayAutoClaimScript::OnPlayerLogout(Player* player)
{
    if (ShouldSkip(player)) return;

    // Always clear any pending claim for a logging-out player so we don't try
    // to run it after the Player* is gone. Cheap regardless of whitelist.
    {
        std::lock_guard<std::mutex> lock(s_pendingMutex);
        s_pendingAutoClaim.erase(player->GetGUID().GetCounter());
    }

    if (!PlayerAccountIsWhitelisted(player)) return;
    if (g_GatewayBotGUIDSet.empty()) return;

    LOG_INFO("server.loading",
             "[Ollama Chat AutoClaim] {} logging out — releasing gateway bots back to rndbot pool",
             player->GetName());

    for (uint64_t botGuidLow : g_GatewayBotGUIDSet)
    {
        ObjectGuid botGuid(HighGuid::Player, static_cast<uint32>(botGuidLow));
        Player* bot = ObjectAccessor::FindConnectedPlayer(botGuid);
        if (!bot || !bot->IsInWorld()) continue;

        PlayerbotAI* ai = PlayerbotsMgr::instance().GetPlayerbotAI(bot);
        if (!ai) continue;

        // Only release bots that THIS player currently masters. Another
        // whitelisted player could be logged in and claiming, and we don't
        // want this logout to yank bots away from them.
        if (ai->GetMaster() != player) continue;

        if (PlayerbotMgr* mgr = PlayerbotsMgr::instance().GetPlayerbotMgr(player))
        {
            LOG_INFO("server.loading",
                     "[Ollama Chat AutoClaim] despawning bot guid={} from {} before re-adding as rndbot",
                     static_cast<uint32>(botGuidLow), player->GetName());
            mgr->LogoutPlayerBot(botGuid);
        }

        // Immediately re-add as rndbot so the fleet stays online for
        // Overlord's SystemMasterGuid fallback to drive via MCP.
        sRandomPlayerbotMgr.AddPlayerBot(botGuid, /*masterAccountId=*/0);
    }
}
