#ifndef MOD_OLLAMA_CHAT_PLAYERPREFS_H
#define MOD_OLLAMA_CHAT_PLAYERPREFS_H

#include <cstdint>
#include <string>

// Player-facing opt-out and mute preferences.
//
// - Opt-out (account-wide): suppresses ALL bot responses (gateway AND Ollama) directed at
//   the player. Stored in mod_ollama_chat_optouts.
// - Mute (per-(account, bot)): suppresses responses from a specific bot. Stored in
//   mod_ollama_chat_bot_mutes.
//
// Both are keyed by account ID (one account → many characters), so the preference travels
// with the player across alts.

void LoadPlayerPreferencesFromDB();

// Returns true if the account has opted out of all bot responses.
bool IsAccountOptedOut(uint32_t accountId);

// Returns true if the account has muted this specific bot.
bool IsBotMutedByAccount(uint32_t accountId, uint64_t botGuid);

// Convenience: true if either opt-out OR per-bot mute applies.
bool ShouldSuppressBotResponse(uint32_t accountId, uint64_t botGuid);

void SetAccountOptOut(uint32_t accountId, bool optedOut);
void SetBotMute(uint32_t accountId, uint64_t botGuid, bool muted);

// Look up bot GUID by character name (returns 0 if not found, used by .ollama mute <name>).
uint64_t LookupBotGuidByName(const std::string& name);

#endif // MOD_OLLAMA_CHAT_PLAYERPREFS_H
