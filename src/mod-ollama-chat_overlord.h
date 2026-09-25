#ifndef MOD_OLLAMA_CHAT_OVERLORD_H
#define MOD_OLLAMA_CHAT_OVERLORD_H

#include "Define.h"

// Overlord — a headless master character that auto-logs-in on server startup
// so the agentic bot fleet has a persistent "human" session to dispatch
// `.playerbots bot` admin commands through. Needed because:
//   - PlayerbotsMgr::GetPlayerbotMgr returns nullptr for bot sessions, so
//     the `bot` subcommand family (add/remove/list/addclass/...) rejects
//     bot-issued calls outright.
//   - We start the worldserver once a day and can't rely on a human keeper
//     staying logged-in 24/7.
//
// Strategy: piggyback on the same null-socket WorldSession construction path
// mod-playerbots already uses for its bots, but register the resulting Player
// as a MASTER (AddPlayerbotData isBotAI=false) rather than as AI. The session
// has WorldSession::IsBot()==true, which skips DoS checks and socket I/O, but
// its _playerbotsMgrMap entry is a PlayerbotMgr, so every "am I a master?"
// check in mod-playerbots sees Overlord as human.
//
// Registration is scheduled from OllamaChatConfigWorldScript::OnStartup after
// a configurable startup delay (world map containers need to be ready before
// we can load a Player into them).
namespace OllamaChat::Overlord
{
    // Queues the auto-login when Overlord config is enabled. Safe to call more
    // than once — a one-shot guard prevents concurrent retries.
    void ScheduleStartupLogin();

    // Spawns every guid in OllamaChat.Overlord.AutoSpawnBots that is not already
    // in-world, via the rndbot path (masterAccountId=0). Called at Overlord login and
    // by leader_admin_set_fleet after a reload. Returns how many spawns were issued.
    uint32 SpawnConfiguredBots();
}

#endif  // MOD_OLLAMA_CHAT_OVERLORD_H
