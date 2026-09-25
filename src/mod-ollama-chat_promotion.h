#ifndef MOD_OLLAMA_CHAT_PROMOTION_H
#define MOD_OLLAMA_CHAT_PROMOTION_H

#include <cstdint>
#include <string>
#include <vector>

// Bot promotion — any playerbot the operator talks to gets the strategic lane.
//
// Only the bots listed in Gateway.BotGUIDs (Claude, Raz) have a gateway
// override, so every other playerbot answered chat with nothing. Promotion
// gives an arbitrary bot the same brain at runtime, in two tiers:
//
//   * chat  — a whitelisted human whispers the bot, or names it (whole word) in
//             party/raid/guild/say/yell/General. The bot keeps the strategic
//             lane for Gateway.Promote.ChatTtlSec after the LAST such message,
//             then drops back to a stock playerbot. At most
//             Gateway.Promote.MaxChatBots at once (least-recently-addressed
//             evicted).
//   * party — the bot is in the SAME group as a whitelisted human (the human
//             invited it). No timeout; its playerbots master is the human, it
//             joins the tactical loop, and the fleet leader's leader_* tools can
//             command it. Leaving the group (kick) drops everything, including
//             any chat window — "dumb until I speak to it again".
//
// Party membership is DERIVED from live group state every few seconds, never
// stored, so it survives a worldserver restart and a kick needs no hook.
//
// Promoted bots borrow a COPY of the template bot's gateway override
// (Gateway.Promote.TemplateBotGuid, default Fleet.LeaderGuid) — the global
// Gateway.Url is empty on this server. The registry has its own mutex:
// gateway worker threads read it, the world thread writes it.
//
// Gateway.BotGUIDs bots are never promoted (they are already gateway bots), and
// neither are the fleet (Fleet.LeaderGuid / MemberGuids) or the Botmaster.
struct GatewayBotConfig;
class Player;

namespace OllamaChat::Promotion
{
    // Any thread. True while the bot is promoted (chat window open or party guest).
    bool IsPromoted(uint64_t botGuid);

    // Any thread. True while the bot shares a group with a whitelisted human.
    bool IsPartyGuest(uint64_t botGuid);

    // Any thread. Snapshot of the current party guests (raw guids).
    std::vector<uint64_t> PartyGuests();

    // Any thread. Copy of the template gateway config when botGuid is promoted.
    bool LookupConfig(uint64_t botGuid, GatewayBotConfig& out);

    // World thread (chat hook). Promote / refresh the chat window for a bot a
    // whitelisted human just addressed. False when promotion is off, the bot is
    // not eligible, or no template config exists.
    bool TouchForChat(Player* bot);

    // World thread. A real (non-bot) player on a Gateway.WhitelistAccountIds
    // account. An EMPTY whitelist makes this false: promotion never opens to
    // the whole realm.
    bool IsOperator(Player* p);

    // Whole-word, case-insensitive: "Raz" matches "raz, heal" but not "crazy".
    bool MentionsName(const std::string& message, const std::string& name);

    // Chat source allowed to promote / reach a promoted bot (Gateway.Promote.Channels).
    bool IsSourceAllowed(int chatChannelSourceLocal);

    // World thread. Re-derive party guests + expire chat windows. Interval-gated.
    void Tick(uint32_t diffMs);

    // World thread, end of every config (re)load: re-copy the template config,
    // parse the channel list, drop state when disabled.
    void OnConfigLoaded();

    // Short status line for .ollama / ops output.
    std::string StatusLine();
}

#endif  // MOD_OLLAMA_CHAT_PROMOTION_H
