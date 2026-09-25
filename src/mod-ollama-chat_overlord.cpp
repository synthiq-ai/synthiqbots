#include "mod-ollama-chat_overlord.h"
#include "mod-ollama-chat_config.h"

#include "CharacterCache.h"
#include "DatabaseEnv.h"
#include "Log.h"
#include "ObjectAccessor.h"
#include "ObjectGuid.h"
#include "Player.h"
#include "PlayerbotMgr.h"
#include "RandomPlayerbotMgr.h"
#include "SharedDefines.h"
#include "World.h"
#include "WorldSession.h"

#include <atomic>
#include <chrono>
#include <thread>

namespace OllamaChat::Overlord
{
    namespace
    {
        // Reset on shutdown path via ResetStartGuard (see bottom of this file) so a
        // subsequent `.ollama reload` that toggles Enable=1 can re-arm the login.
        std::atomic<bool> s_scheduled{false};

        // Run on the world thread (via AfterComplete) once character data has been
        // fetched from the DB. Creates the headless WorldSession, loads the Player
        // into the world, and registers a PlayerbotMgr for the guid — which is the
        // step that lets `.playerbots bot ...` find a non-bot master through
        // GetPlayerbotMgr.
        void CompleteLogin(LoginQueryHolder const& holder)
        {
            ObjectGuid guid = holder.GetGuid();
            uint32 accountId = holder.GetAccountId();

            // Another hook may have pulled this char into the world between our DB
            // dispatch and this callback (unlikely but not impossible — e.g. someone
            // logged in as Overlord from a real client during the startup delay).
            // Short-circuit so we don't double-load the Player.
            if (Player* existing = ObjectAccessor::FindConnectedPlayer(guid))
            {
                if (existing->IsInWorld())
                {
                    if (!PlayerbotsMgr::instance().GetPlayerbotMgr(existing))
                        PlayerbotsMgr::instance().AddPlayerbotData(existing, /*isBotAI=*/false);
                    LOG_INFO("server.loading",
                             "[Ollama Chat Overlord] already in-world (account={}, guid={}); ensured master registration",
                             accountId, guid.GetCounter());
                    return;
                }
            }

            // Null socket + is_bot=true mirrors PlayerbotMgr.cpp:197. is_bot flips
            // WorldSession::_isBot but does NOT touch the PlayerbotsMgr map — we
            // register Overlord as master below.
            WorldSession* session = new WorldSession(
                accountId, "", 0x0, nullptr, SEC_PLAYER, EXPANSION_WRATH_OF_THE_LICH_KING,
                time_t(0), sWorld->GetDefaultDbcLocale(), 0, false, false, 0, /*is_bot=*/true);

            session->HandlePlayerLoginFromDB(holder);

            Player* overlord = session->GetPlayer();
            if (!overlord)
            {
                LOG_ERROR("server.loading",
                          "[Ollama Chat Overlord] HandlePlayerLoginFromDB produced no Player (account={}, guid={})",
                          accountId, guid.GetCounter());
                session->LogoutPlayer(true);
                delete session;
                return;
            }

            // The key line: register a PlayerbotMgr (not a PlayerbotAI) for Overlord's
            // guid. After this, PlayerbotsMgr::GetPlayerbotMgr(overlord) returns a real
            // manager and Tier 9.1's leader_admin_command can dispatch
            // `.playerbots bot ...` through Overlord's session.
            PlayerbotsMgr::instance().AddPlayerbotData(overlord, /*isBotAI=*/false);

            LOG_INFO("server.loading",
                     "[Ollama Chat Overlord] auto-login complete: {} (account={}, guid={}) registered as master session",
                     overlord->GetName(), accountId, guid.GetCounter());

            // Phase 1.1 — auto-spawn the configured leader bots (see SpawnConfiguredBots).
            SpawnConfiguredBots();
        }

        void PerformLogin()
        {
            ObjectGuid guid(HighGuid::Player, g_OverlordCharacterGuid);

            if (Player* existing = ObjectAccessor::FindConnectedPlayer(guid))
            {
                if (existing->IsInWorld())
                {
                    if (!PlayerbotsMgr::instance().GetPlayerbotMgr(existing))
                        PlayerbotsMgr::instance().AddPlayerbotData(existing, /*isBotAI=*/false);
                    LOG_INFO("server.loading",
                             "[Ollama Chat Overlord] already in-world (guid={}); ensured master registration",
                             guid.GetCounter());
                    return;
                }
            }

            uint32 accountId = sCharacterCache->GetCharacterAccountIdByGuid(guid);
            if (!accountId)
            {
                LOG_ERROR("server.loading",
                          "[Ollama Chat Overlord] cannot resolve account for guid={} — check OllamaChat.Overlord.CharacterGuid",
                          guid.GetCounter());
                s_scheduled.store(false);
                return;
            }

            auto holder = std::make_shared<LoginQueryHolder>(accountId, guid);
            if (!holder->Initialize())
            {
                LOG_ERROR("server.loading",
                          "[Ollama Chat Overlord] LoginQueryHolder::Initialize failed (account={}, guid={})",
                          accountId, guid.GetCounter());
                s_scheduled.store(false);
                return;
            }

            // AfterComplete dispatches back to the world thread via the session-
            // update pump — safe for the new Player/session construction inside
            // CompleteLogin().
            sWorld->AddQueryHolderCallback(CharacterDatabase.DelayQueryHolder(holder))
                .AfterComplete([](SQLQueryHolderBase const& finished)
                {
                    CompleteLogin(static_cast<LoginQueryHolder const&>(finished));
                });
        }
    }

    uint32 SpawnConfiguredBots()
    {
        // Phase 1.1 — spawn the configured leader bots. masterAccountId=0 is the
        // rndbot path: skips same-account / guild / linked-account permission checks
        // so Overlord can spawn bots regardless of which account owns them. The bots
        // register as PlayerbotAI (rndbot), and leader_admin_command's SystemMasterGuid
        // fallback routes admin calls back through Overlord. Runs at Overlord login and
        // again on demand from leader_admin_set_fleet after a reroll, so a rewired
        // roster comes up without a worldserver restart.
        uint32 spawned = 0;
        for (uint32_t botGuidLow : g_OverlordAutoSpawnBotList)
        {
            if (botGuidLow == g_OverlordCharacterGuid)
                continue;   // never try to spawn Overlord as their own bot
            ObjectGuid botGuid(HighGuid::Player, botGuidLow);
            // Any connected Player counts — in world or mid-teleport. AddPlayerBot
            // only guards pending logins and in-world players, so re-adding a
            // teleporting bot would log in a second Player (codex P1, 2026-09-25).
            if (ObjectAccessor::FindConnectedPlayer(botGuid))
            {
                LOG_INFO("server.loading",
                         "[Ollama Chat Overlord] auto-spawn skipped (already connected): guid={}",
                         botGuidLow);
                continue;
            }
            LOG_INFO("server.loading", "[Ollama Chat Overlord] auto-spawning rndbot guid={}", botGuidLow);
            sRandomPlayerbotMgr.AddPlayerBot(botGuid, /*masterAccountId=*/0);
            ++spawned;
        }
        return spawned;
    }

    void ScheduleStartupLogin()
    {
        if (!g_OverlordEnable)
            return;
        if (!g_OverlordCharacterGuid)
        {
            LOG_ERROR("server.loading",
                      "[Ollama Chat Overlord] Enable=1 but CharacterGuid=0 — set OllamaChat.Overlord.CharacterGuid to a character guid");
            return;
        }

        // One-shot guard. Tripping it keeps a reload from spawning a second
        // sleep-then-login thread while the first is still mid-flight.
        bool expected = false;
        if (!s_scheduled.compare_exchange_strong(expected, true))
        {
            LOG_DEBUG("server.loading", "[Ollama Chat Overlord] schedule already armed; skipping duplicate");
            return;
        }

        uint32 delay = g_OverlordStartupDelaySeconds;
        uint32 guidLow = g_OverlordCharacterGuid;

        LOG_INFO("server.loading",
                 "[Ollama Chat Overlord] scheduling auto-login for guid={} in {}s",
                 guidLow, delay);

        std::thread([delay]()
        {
            if (delay > 0)
                std::this_thread::sleep_for(std::chrono::seconds(delay));
            PerformLogin();
            // Intentionally leave s_scheduled=true: we don't want repeat-ticks
            // to re-login on every reload. The login is idempotent inside
            // PerformLogin/CompleteLogin anyway.
        }).detach();
    }
}
