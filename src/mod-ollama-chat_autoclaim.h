#ifndef MOD_OLLAMA_CHAT_AUTOCLAIM_H
#define MOD_OLLAMA_CHAT_AUTOCLAIM_H

#include "ScriptMgr.h"
#include "Player.h"

// Auto-claim gateway bots on whitelisted player login, auto-release on logout.
//
// Problem: gateway bots spawn as rndbots (via Overlord AutoSpawn) so they stay
// in-world 24/7 but have no human master. When the operator logs in as their
// real character (e.g. Raz), bots don't join their party or respond to party
// chat — party-chat gating requires being in the same group.
//
// Design A: on a whitelisted player's login, iterate Gateway.BotGUIDs; for each
// bot, despawn from whatever holder currently has it (rndbot manager or
// another player's PlayerbotMgr) and re-spawn under the logging-in player as
// master. On the same player's logout, do the reverse: release bots back to
// rndbot status (masterAccountId=0) so the fleet stays online while Overlord
// is the only master in-world.
class GatewayAutoClaimScript : public PlayerScript
{
public:
    GatewayAutoClaimScript();
    // We register the player as "pending claim" on login — the actual claim
    // runs on the first OnPlayerBeforeUpdate tick so mod-playerbots'
    // PlayerbotsScript::OnPlayerLogin (which creates the player's
    // PlayerbotMgr via AddPlayerbotData) has had a chance to fire first.
    // Script order across modules isn't deterministic.
    void OnPlayerLogin(Player* player) override;
    void OnPlayerBeforeUpdate(Player* player, uint32 p_time) override;
    void OnPlayerLogout(Player* player) override;
};

#endif  // MOD_OLLAMA_CHAT_AUTOCLAIM_H
