#ifndef MOD_OLLAMA_CHAT_FLEET_H
#define MOD_OLLAMA_CHAT_FLEET_H

#include <cstdint>

// Fleet bootstrap — periodic, re-entrant party/master reconciliation for the
// agent-driven bot fleet (robot-first operating model).
//
// Problem this solves: Overlord spawns the gateway bots via the rndbot path
// (sRandomPlayerbotMgr.AddPlayerBot, masterAccountId=0) but never groups them.
// mod-playerbots' PlayerbotAI::UpdateAIGroupMaster() clears PlayerbotAI::master
// on EVERY AI tick for an ungrouped random bot, so the fleet is permanently
// ownerless: SummonAction/FollowAction resolve GetMaster() to null and silently
// no-op while the MCP tool layer reports ok.
//
// Strategy: make the LEADER bot (Claude, Mcp.Leader.BotGUIDs) a self-mastered
// party leader and pull the member bots into its group with the leader as their
// master. Self-mastering is load-bearing: playerbots accepts a master whose AI
// is self-mastered (IsRealPlayer() == master == bot), and FindNewMaster
// re-election only runs when the current master is null or not-real — so once
// formed, the topology is stable. Botmaster/Overlord deliberately stays OUT of
// the party (it remains only the `.playerbots bot` admin session), keeping the
// fifth party slot free for the human operator.
//
// The pass must be RE-ENTRANT: if the group ever dissolves, playerbots clears
// the masters again on its next tick, so we reconcile on an interval rather
// than bootstrapping once. Every step is idempotent.
class Group;
class Player;

namespace OllamaChat::Fleet
{
    // Called from OllamaChatConfigWorldScript::OnUpdate — world thread, so
    // direct Group/SetMaster calls are safe (same context mod-playerbots'
    // own GroupInviteOperation uses). No-op unless OllamaChat.Fleet.EnsureParty
    // is enabled and the interval has elapsed.
    void Tick(uint32_t diffMs);

    // World thread only. True when making `master` the playerbots master of
    // `bot` cannot leave a dangling pointer: the random-bot holder owns the bot
    // (RandomPlayerbotMgr clears masters on the master's logout), or `master`'s
    // own PlayerbotMgr holds it. A bot held by ANOTHER player's PlayerbotMgr
    // (e.g. gateway auto-claim) keeps its master pointer after the new master
    // logs out — codex review of the operator-mode change.
    bool IsSafeMaster(Player* bot, Player* master);

    // World thread only. Drop a pending party invite before a direct
    // Group::Create/AddMember: AddMember clears the player's invite pointer but
    // not the inviting group's invitee set (stale Player* after logout).
    // `keep` is the group p is about to join: its own invite is just removed,
    // never disbanded (it may have a single member and be deleted otherwise).
    void ClearPendingInvite(Player* p, Group* keep = nullptr);

    // A battleground/battlefield raid, never an ordinary party: direct joins
    // must not add anyone to it (they would join a match they never entered).
    bool IsBattleGroup(::Group* g);
}

#endif  // MOD_OLLAMA_CHAT_FLEET_H
