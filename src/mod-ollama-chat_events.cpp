#include "mod-ollama-chat_events.h"
#include "mod-ollama-chat_config.h"
#include "mod-ollama-chat_tactical.h"
#include "mod-ollama-chat_proactive.h"
#include "AchievementMgr.h"
#include "Creature.h"
#include "GameObject.h"
#include "Item.h"
#include "Log.h"
#include "Map.h"
#include "Player.h"
#include "QuestDef.h"
#include "SpellMgr.h"

namespace
{
    void RecordEvent(Player* source, const std::string& type, const std::string& detail)
    {
        if (!source || !source->IsInWorld() || type.empty())
            return;

        RecordTacticalWorldEvent(source->GetGUID().GetRawValue(),
                                 source->GetName(),
                                 source->GetMapId(),
                                 source->GetPositionX(),
                                 source->GetPositionY(),
                                 source->GetPositionZ(),
                                 type,
                                 detail);

        if (g_DebugEnabled)
        {
            LOG_INFO("server.loading",
                     "[Ollama Chat Tactical] recorded world event source={} type={} detail={}",
                     source->GetName(), type, detail);
        }
    }

    std::string SpellNameOrId(uint32 spellId)
    {
        SpellInfo const* spellInfo = sSpellMgr->GetSpellInfo(spellId);
        if (spellInfo && spellInfo->SpellName[0])
            return spellInfo->SpellName[0];
        return std::to_string(spellId);
    }
}

ChatOnKill::ChatOnKill() : PlayerScript("ChatOnKill") {}

void ChatOnKill::OnPlayerCreatureKill(Player* killer, Creature* victim)
{
    if (killer && victim)
        RecordEvent(killer, g_EventTypeDefeated.empty() ? "defeated" : g_EventTypeDefeated, victim->GetName());
}

void ChatOnKill::OnPlayerPVPKill(Player* killer, Player* killed)
{
    if (killer && killed)
        RecordEvent(killer, g_EventTypeDefeatedPlayer.empty() ? "defeated player" : g_EventTypeDefeatedPlayer, killed->GetName());
}

void ChatOnKill::OnPlayerCreatureKilledByPet(Player* owner, Creature* victim)
{
    if (owner && victim)
        RecordEvent(owner, g_EventTypePetDefeated.empty() ? "pet defeated" : g_EventTypePetDefeated, victim->GetName());
}

ChatOnLoot::ChatOnLoot() : PlayerScript("ChatOnLoot") {}

void ChatOnLoot::OnPlayerStoreNewItem(Player* player, Item* item, uint32 /*count*/)
{
    if (!player || !item || !item->GetTemplate())
        return;
    if (item->GetTemplate()->Quality >= ITEM_QUALITY_UNCOMMON)
        RecordEvent(player, g_EventTypeGotItem.empty() ? "got item" : g_EventTypeGotItem, item->GetTemplate()->Name1);
}

ChatOnDeath::ChatOnDeath() : PlayerScript("ChatOnDeath") {}

void ChatOnDeath::OnPlayerJustDied(Player* player)
{
    if (player)
        RecordEvent(player, g_EventTypeDied.empty() ? "died" : g_EventTypeDied, "");
}

ChatOnQuest::ChatOnQuest() : PlayerScript("ChatOnQuest") {}

void ChatOnQuest::OnPlayerCompleteQuest(Player* player, Quest const* quest)
{
    if (player && quest)
    {
        RecordEvent(player, g_EventTypeCompletedQuest.empty() ? "completed quest" : g_EventTypeCompletedQuest, quest->GetTitle());
        // Feature 002 — proactive completion detection (FR-021).
        ollamachat::proactive::OnQuestComplete(player, quest->GetQuestId());
    }
}

ChatOnLearn::ChatOnLearn() : PlayerScript("ChatOnLearn") {}

void ChatOnLearn::OnPlayerLearnSpell(Player* player, uint32 spellID)
{
    if (player)
        RecordEvent(player, g_EventTypeLearnedSpell.empty() ? "learned spell" : g_EventTypeLearnedSpell, SpellNameOrId(spellID));
}

ChatOnDuel::ChatOnDuel() : PlayerScript("ChatOnDuel") {}

void ChatOnDuel::OnPlayerDuelRequest(Player* target, Player* challenger)
{
    if (challenger && target)
        RecordEvent(challenger, g_EventTypeRequestedDuel.empty() ? "requested to duel" : g_EventTypeRequestedDuel, target->GetName());
}

void ChatOnDuel::OnPlayerDuelStart(Player* player1, Player* player2)
{
    if (player1 && player2)
        RecordEvent(player1, g_EventTypeStartedDueling.empty() ? "started dueling" : g_EventTypeStartedDueling, player2->GetName());
}

void ChatOnDuel::OnPlayerDuelEnd(Player* winner, Player* loser, DuelCompleteType /*type*/)
{
    if (winner && loser)
        RecordEvent(winner, g_EventTypeWonDuel.empty() ? "won duel against" : g_EventTypeWonDuel, loser->GetName());
}

ChatOnLevelUp::ChatOnLevelUp() : PlayerScript("ChatOnLevelUp") {}

void ChatOnLevelUp::OnPlayerLevelChanged(Player* player, uint8 /*oldLevel*/)
{
    if (player)
        RecordEvent(player, g_EventTypeLeveledUp.empty() ? "leveled up" : g_EventTypeLeveledUp, std::to_string(player->GetLevel()));
}

ChatOnAchievement::ChatOnAchievement() : PlayerScript("ChatOnAchievement") {}

void ChatOnAchievement::OnPlayerCompleteAchievement(Player* player, AchievementEntry const* achievement)
{
    if (player && achievement)
        RecordEvent(player, g_EventTypeAchievement.empty() ? "earned achievement" : g_EventTypeAchievement, achievement->name[0]);
}

ChatOnGameObjectUse::ChatOnGameObjectUse() : PlayerScript("ChatOnGameObjectUse") {}

void ChatOnGameObjectUse::OnGameObjectUse(Player* player, GameObject* go)
{
    if (player && go)
        RecordEvent(player, g_EventTypeUsedObject.empty() ? "used object" : g_EventTypeUsedObject, go->GetName());
}
