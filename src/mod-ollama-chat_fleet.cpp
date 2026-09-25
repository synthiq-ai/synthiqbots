#include "mod-ollama-chat_fleet.h"
#include "mod-ollama-chat_config.h"
#include "mod-ollama-chat_tactical.h"
#include "mod-ollama-chat_overlord.h"

#include "Group.h"
#include "GroupMgr.h"
#include "Log.h"
#include "ObjectAccessor.h"
#include "ObjectGuid.h"
#include "Player.h"
#include "PlayerbotAI.h"
#include "PlayerbotMgr.h"
#include "RandomPlayerbotMgr.h"

#include <algorithm>
#include <ctime>

namespace OllamaChat::Fleet
{
    namespace
    {
        uint32_t s_accumMs = 0;

        // One reconciliation pass. World thread only. Mirrors the world-thread
        // group formation mod-playerbots' GroupInviteOperation performs
        // (new Group / Create / sGroupMgr->AddGroup / AddMember) — no packets,
        // no invite round-trip, which is exactly right for headless bots.
        void EnsureParty()
        {
            ObjectGuid leaderGuid(HighGuid::Player, g_FleetLeaderGuid);
            Player* leader = ObjectAccessor::FindPlayer(leaderGuid);
            if (!leader || !leader->IsInWorld())
                return;  // not spawned yet (Overlord auto-spawn runs ~30s after boot) — retry next interval

            PlayerbotAI* leaderAI = PlayerbotsMgr::instance().GetPlayerbotAI(leader);
            if (!leaderAI)
                return;  // leader is not a playerbot (misconfig) — nothing to wire

            // 0. Operator mode. When a whitelisted human shares the leader's
            //    group (they invited the bots, or joined the fleet party), the
            //    HUMAN is every fleet bot's master: playerbots bots follow and
            //    obey their master, and a self-mastered leader / leader-mastered
            //    member ignore the human's "follow", "attack", etc. Measured
            //    2026-09-24: the operator partied with Claude + Raz and neither
            //    moved, and every 20 s this pass re-mastered them away from him.
            //    When the human leaves the group the next pass restores the
            //    agent-led fleet below.
            Group* shared = leader->GetGroup();
            if (shared && !IsBattleGroup(shared))
            {
                Player* human = nullptr;
                for (GroupReference* ref = shared->GetFirstMember(); ref; ref = ref->next())
                {
                    Player* p = ref->GetSource();
                    if (p && p->IsInWorld() && IsWhitelistedHumanPlayer(p, /*anyHumanWhenNoWhitelist=*/false))
                    {
                        human = p;
                        break;
                    }
                }
                if (human && IsSafeMaster(leader, human))
                {
                    if (leaderAI->GetMaster() != human)
                    {
                        leaderAI->SetMaster(human);
                        LOG_INFO("server.loading",
                                 "[Ollama Chat Fleet] {} is in the fleet group — leader {} (guid={}) now follows them",
                                 human->GetName(), leader->GetName(), g_FleetLeaderGuid);
                    }
                    for (uint32_t memberGuidLow : g_FleetMemberGuidList)
                    {
                        if (memberGuidLow == g_FleetLeaderGuid)
                            continue;
                        Player* member = ObjectAccessor::FindPlayer(ObjectGuid(HighGuid::Player, memberGuidLow));
                        if (!member || !member->IsInWorld())
                            continue;
                        PlayerbotAI* memberAI = PlayerbotsMgr::instance().GetPlayerbotAI(member);
                        if (!memberAI)
                            continue;
                        if (!IsSafeMaster(member, human))
                            continue;  // held by another player's PlayerbotMgr — leave it alone
                        if (!member->GetGroup() && !shared->IsFull())
                            ClearPendingInvite(member, shared);
                        if (!member->GetGroup() && !shared->IsFull() && shared->AddMember(member))
                        {
                            LOG_INFO("server.loading",
                                     "[Ollama Chat Fleet] added {} (guid={}) to {}'s group",
                                     member->GetName(), memberGuidLow, human->GetName());
                        }
                        if (member->GetGroup() == shared && memberAI->GetMaster() != human)
                        {
                            memberAI->SetMaster(human);
                            LOG_INFO("server.loading",
                                     "[Ollama Chat Fleet] {} (guid={}) now follows {}",
                                     member->GetName(), memberGuidLow, human->GetName());
                        }
                    }
                    return;
                }
            }

            // 1. Self-master the leader. Makes IsRealPlayer() true for it, which
            //    (a) qualifies it as a valid master for the member bots and
            //    (b) stops playerbots' random-bot sweeps from treating an
            //    agent-driven leader as expendable. UpdateAIGroupMaster clears
            //    this again if the group dissolves — step 2 plus the interval
            //    re-run makes the whole pass self-healing.
            if (leaderAI->GetMaster() != leader)
            {
                leaderAI->SetMaster(leader);
                LOG_INFO("server.loading",
                         "[Ollama Chat Fleet] leader {} (guid={}) self-mastered",
                         leader->GetName(), g_FleetLeaderGuid);
            }

            // 2. Ensure the leader owns a group.
            Group* group = leader->GetGroup();
            if (!group)
            {
                group = new Group();
                if (!group->Create(leader))
                {
                    delete group;
                    LOG_ERROR("server.loading",
                              "[Ollama Chat Fleet] Group::Create failed for leader guid={}",
                              g_FleetLeaderGuid);
                    return;
                }
                sGroupMgr->AddGroup(group);
                LOG_INFO("server.loading",
                         "[Ollama Chat Fleet] party formed under leader {} (guid={})",
                         leader->GetName(), g_FleetLeaderGuid);
            }

            // 3. Pull each member bot into the group and wire its master link.
            for (uint32_t memberGuidLow : g_FleetMemberGuidList)
            {
                if (memberGuidLow == g_FleetLeaderGuid)
                    continue;

                Player* member = ObjectAccessor::FindPlayer(ObjectGuid(HighGuid::Player, memberGuidLow));
                if (!member || !member->IsInWorld())
                    continue;  // offline / not spawned — next interval

                PlayerbotAI* memberAI = PlayerbotsMgr::instance().GetPlayerbotAI(member);
                if (!memberAI)
                    continue;

                Group* memberGroup = member->GetGroup();
                if (memberGroup == group)
                {
                    // Already partied — just keep the master link honest.
                    // UpdateAIGroupMaster can null it transiently (e.g. after a
                    // battleground) even while grouped.
                    if (memberAI->GetMaster() != leader)
                    {
                        memberAI->SetMaster(leader);
                        LOG_INFO("server.loading",
                                 "[Ollama Chat Fleet] re-mastered {} (guid={}) to leader {}",
                                 member->GetName(), memberGuidLow, leader->GetName());
                    }
                    continue;
                }

                if (memberGroup)
                {
                    // Never yank a bot out of a foreign group (could be the
                    // operator's own party or a battleground raid).
                    LOG_WARN("server.loading",
                             "[Ollama Chat Fleet] {} (guid={}) is in a different group — not moving it",
                             member->GetName(), memberGuidLow);
                    continue;
                }

                // 5-man cap; same literal check mod-playerbots uses before its
                // raid conversion. We do NOT auto-convert to raid — the fleet
                // is a party by design (leader + members + a free operator slot).
                if (group->GetMembersCount() >= 5)
                {
                    LOG_WARN("server.loading",
                             "[Ollama Chat Fleet] party full — cannot add {} (guid={})",
                             member->GetName(), memberGuidLow);
                    continue;
                }

                if (!group->AddMember(member))
                {
                    LOG_ERROR("server.loading",
                              "[Ollama Chat Fleet] Group::AddMember failed for {} (guid={})",
                              member->GetName(), memberGuidLow);
                    continue;
                }
                memberAI->SetMaster(leader);
                LOG_INFO("server.loading",
                         "[Ollama Chat Fleet] added {} (guid={}) to the fleet party, master={}",
                         member->GetName(), memberGuidLow, leader->GetName());
            }
        }
    }

    bool IsSafeMaster(Player* bot, Player* master)
    {
        if (!bot || !master) return false;
        if (sRandomPlayerbotMgr.GetPlayerBot(bot->GetGUID()))
            return true;
        if (PlayerbotMgr* mgr = PlayerbotsMgr::instance().GetPlayerbotMgr(master))
            return mgr->GetPlayerBot(bot->GetGUID()) != nullptr;
        return false;
    }

    void ClearPendingInvite(Player* p, Group* keep)
    {
        if (!p) return;
        Group* inv = p->GetGroupInvite();
        if (!inv) return;
        if (inv == keep)
        {
            inv->RemoveInvite(p);   // the destination group: never disband it
            return;
        }
        // p LEADS a pending (uncreated) group: cancel it entirely, or another
        // invitee could still accept and Group::Create(p) would put p into a
        // second group (codex review).
        if (!inv->IsCreated() && inv->GetLeaderGUID() == p->GetGUID())
        {
            inv->RemoveAllInvites();
            delete inv;
            return;
        }
        // Ordinary invitee: the core's own cleanup (also disbands/deletes a
        // group left with nobody).
        p->UninviteFromGroup();
    }

    bool IsBattleGroup(Group* g)
    {
        return g && (g->isBGGroup() || g->isBFGroup());
    }

    // playerbots logs EVERY random bot out once no real player has been on for
    // DisabledWithoutRealPlayerLogoutDelay ("Logout all bots due no real player
    // session"), and the fleet bots are random bots (Overlord AutoSpawnBots). It
    // logs its own random bots back in when a player returns, but the fleet is
    // not in its random-bot table — so after the operator logged out, Raz stayed
    // gone until a worldserver restart (found by the E2E harness, 2026-09-25).
    // Re-spawn missing fleet bots while a whitelisted human is online.
    void RespawnMissingFleetBots()
    {
        if (!g_OverlordEnable || g_OverlordAutoSpawnBotList.empty())
            return;
        bool humanOnline = false;
        for (auto const& [guid, p] : ObjectAccessor::GetPlayers())
            if (p && p->IsInWorld() && IsWhitelistedHumanPlayer(p, /*anyHumanWhenNoWhitelist=*/false))
            {
                humanOnline = true;
                break;
            }
        if (!humanOnline)
            return;   // they would be logged straight out again, and nobody is playing
        bool missing = false;
        for (uint32_t low : g_OverlordAutoSpawnBotList)
        {
            if (low == g_OverlordCharacterGuid)
                continue;
            // Missing = no connected Player at all. A bot mid-teleport is
            // connected but not in world; re-adding it would log in a second
            // Player over the live one (codex P1).
            if (!ObjectAccessor::FindConnectedPlayer(ObjectGuid(HighGuid::Player, low)))
            {
                missing = true;
                break;
            }
        }
        if (!missing)
            return;
        // A spawn logs in asynchronously; don't re-issue it while one is loading.
        static time_t lastSpawn = 0;
        time_t now = time(nullptr);
        if (now - lastSpawn < 60)
            return;
        lastSpawn = now;
        uint32_t n = OllamaChat::Overlord::SpawnConfiguredBots();
        LOG_INFO("server.loading", "[Ollama Chat Fleet] re-spawned {} fleet bot(s) logged out while no human was online", n);
    }

    void Tick(uint32_t diffMs)
    {
        if (!g_FleetEnsureParty || !g_FleetLeaderGuid)
            return;

        s_accumMs += diffMs;
        uint32_t intervalMs = std::max<uint32_t>(5, g_FleetEnsureIntervalSec) * 1000;
        if (s_accumMs < intervalMs)
            return;
        s_accumMs = 0;

        RespawnMissingFleetBots();
        EnsureParty();
    }
}
