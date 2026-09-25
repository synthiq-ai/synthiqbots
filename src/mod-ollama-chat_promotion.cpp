#include "mod-ollama-chat_promotion.h"
#include "mod-ollama-chat_config.h"
#include "mod-ollama-chat_fleet.h"
#include "mod-ollama-chat_gateway.h"

#include "Group.h"
#include "Log.h"
#include "ObjectAccessor.h"
#include "ObjectGuid.h"
#include "Player.h"
#include "PlayerbotAI.h"
#include "PlayerbotMgr.h"
#include "WorldSession.h"

#include <algorithm>
#include <cctype>
#include <ctime>
#include <mutex>
#include <unordered_map>
#include <unordered_set>

namespace OllamaChat::Promotion
{
    namespace
    {
        struct Entry
        {
            time_t   chatExpiresAt = 0;   // 0 = no chat window
            bool     party         = false;
            uint64_t humanGuid     = 0;   // party guests: the human whose group they share
        };

        std::mutex                          s_mutex;
        std::unordered_map<uint64_t, Entry> s_entries;
        GatewayBotConfig                    s_template;
        bool                                s_haveTemplate = false;
        uint64_t                            s_templateGuid = 0;
        std::unordered_set<int>             s_channels;   // world thread only (config load + chat hook)
        uint32_t                            s_accumMs = 0;

        bool IsActive(const Entry& e, time_t now)
        {
            return e.party || e.chatExpiresAt > now;
        }

        // World thread. A playerbot that is not already one of the configured
        // agent bots. The fleet and the Botmaster have their own wiring
        // (Fleet.EnsureParty, Overlord) that promotion must never fight.
        bool IsEligibleBot(Player* bot)
        {
            if (!bot || !bot->IsInWorld())
                return false;
            PlayerbotAI* ai = PlayerbotsMgr::instance().GetPlayerbotAI(bot);
            if (!ai || !ai->IsBotAI())
                return false;
            const uint64_t raw = bot->GetGUID().GetRawValue();
            const uint32_t low = bot->GetGUID().GetCounter();
            if (g_GatewayBotGUIDSet.count(raw) > 0)
                return false;
            if (low == g_FleetLeaderGuid || low == g_McpLeaderSystemMasterGuid)
                return false;
            for (uint32_t m : g_FleetMemberGuidList)
                if (m == low)
                    return false;
            return true;
        }

        // World thread. A real, whitelisted human. An EMPTY gateway whitelist
        // disables promotion outright: an open whitelist would let any player
        // on the realm spend strategic-lane turns by naming a bot.
        bool IsOperatorImpl(Player* p)
        {
            if (!p || !p->IsInWorld())
                return false;
            if (PlayerbotAI* ai = PlayerbotsMgr::instance().GetPlayerbotAI(p); ai && ai->IsBotAI())
                return false;
            WorldSession* sess = p->GetSession();
            if (!sess || sess->IsBot())
                return false;
            if (g_GatewayWhitelistAccountIds.empty())
                return false;
            return g_GatewayWhitelistAccountIds.count(sess->GetAccountId()) > 0;
        }

        std::string NameOf(uint64_t raw)
        {
            if (Player* p = ObjectAccessor::FindPlayer(ObjectGuid(raw)))
                return p->GetName();
            return std::to_string(ObjectGuid(raw).GetCounter());
        }

        // World thread. One party pass: every bot in a whitelisted human's
        // (non-battleground) group is a guest; its master is that human.
        void ReconcileParty(time_t now)
        {
            std::unordered_map<uint64_t, uint64_t> guests;   // bot -> human
            bool haveTemplate;
            {
                std::lock_guard<std::mutex> lock(s_mutex);
                haveTemplate = s_haveTemplate;
            }
            // No template = promotion is off (OnConfigLoaded logged why): do not
            // re-master anyone either, so "off" means off.
            if (g_PromoteEnable && g_PromotePartyEnable && haveTemplate)
            {
                for (auto const& pair : ObjectAccessor::GetPlayers())
                {
                    Player* human = pair.second;
                    if (!IsOperatorImpl(human))
                        continue;
                    Group* group = human->GetGroup();
                    if (!group || OllamaChat::Fleet::IsBattleGroup(group))
                        continue;
                    for (GroupReference* ref = group->GetFirstMember(); ref; ref = ref->next())
                    {
                        Player* member = ref->GetSource();
                        if (!member || member == human || !IsEligibleBot(member))
                            continue;
                        const uint64_t raw = member->GetGUID().GetRawValue();
                        if (guests.count(raw))
                            continue;
                        guests.emplace(raw, human->GetGUID().GetRawValue());

                        // Same master rule Fleet.EnsureParty applies to the fleet
                        // in operator mode. playerbots keeps a real-player master
                        // (UpdateAIGroupMaster only re-elects when the master is
                        // null or a non-self bot), so this is stable, not a flap.
                        PlayerbotAI* ai = PlayerbotsMgr::instance().GetPlayerbotAI(member);
                        if (ai && ai->GetMaster() != human && OllamaChat::Fleet::IsSafeMaster(member, human))
                        {
                            ai->SetMaster(human);
                            LOG_INFO("server.loading", "[Ollama Chat Promote] {} (guid={}) now follows {}",
                                     member->GetName(), member->GetGUID().GetCounter(), human->GetName());
                        }
                    }
                }
            }

            std::vector<std::pair<uint64_t, bool>> changes;   // guid, promoted(true)/demoted(false)
            {
                std::lock_guard<std::mutex> lock(s_mutex);
                for (auto it = s_entries.begin(); it != s_entries.end();)
                {
                    Entry& e = it->second;
                    if (e.party && !guests.count(it->first))
                    {
                        // Kicked (or the human left): drop the chat window too —
                        // it stays dumb until the operator speaks to it again.
                        changes.emplace_back(it->first, false);
                        it = s_entries.erase(it);
                        continue;
                    }
                    if (!e.party && e.chatExpiresAt <= now)
                    {
                        changes.emplace_back(it->first, false);
                        it = s_entries.erase(it);
                        continue;
                    }
                    ++it;
                }
                {
                    for (auto const& [raw, humanRaw] : guests)
                    {
                        Entry& e = s_entries[raw];
                        if (!e.party)
                            changes.emplace_back(raw, true);
                        e.party = true;
                        e.humanGuid = humanRaw;
                    }
                }
            }

            for (auto const& [raw, promoted] : changes)
                LOG_INFO("server.loading", "[Ollama Chat Promote] {} (guid={}) {}",
                         NameOf(raw), ObjectGuid(raw).GetCounter(),
                         promoted ? "promoted: party guest (strategic lane, tactical, fleet)"
                                  : "demoted: back to a stock playerbot");
        }
    }

    bool IsPromoted(uint64_t botGuid)
    {
        const time_t now = time(nullptr);
        std::lock_guard<std::mutex> lock(s_mutex);
        auto it = s_entries.find(botGuid);
        return it != s_entries.end() && IsActive(it->second, now);
    }

    bool IsPartyGuest(uint64_t botGuid)
    {
        std::lock_guard<std::mutex> lock(s_mutex);
        auto it = s_entries.find(botGuid);
        return it != s_entries.end() && it->second.party;
    }

    std::vector<uint64_t> PartyGuests()
    {
        std::vector<uint64_t> out;
        std::lock_guard<std::mutex> lock(s_mutex);
        for (auto const& [raw, e] : s_entries)
            if (e.party)
                out.push_back(raw);
        return out;
    }

    bool LookupConfig(uint64_t botGuid, GatewayBotConfig& out)
    {
        const time_t now = time(nullptr);
        std::lock_guard<std::mutex> lock(s_mutex);
        if (!s_haveTemplate)
            return false;
        auto it = s_entries.find(botGuid);
        if (it == s_entries.end() || !IsActive(it->second, now))
            return false;
        out = s_template;
        return true;
    }

    bool TouchForChat(Player* bot)
    {
        if (!g_PromoteEnable || !g_GatewayEnable || !IsEligibleBot(bot))
            return false;

        const time_t now = time(nullptr);
        const uint64_t raw = bot->GetGUID().GetRawValue();
        const time_t expires = now + static_cast<time_t>(std::max<uint32_t>(30, g_PromoteChatTtlSec));
        uint64_t evicted = 0;
        bool fresh = false;
        {
            std::lock_guard<std::mutex> lock(s_mutex);
            if (!s_haveTemplate)
                return false;
            auto it = s_entries.find(raw);
            if (it == s_entries.end() || !IsActive(it->second, now))
            {
                fresh = true;
                // Cap concurrent chat promotions (party guests do not count —
                // a party is bounded by its own size). Evict the bot addressed
                // least recently, i.e. the earliest-expiring window.
                uint32_t chatOnly = 0;
                auto oldest = s_entries.end();
                for (auto e = s_entries.begin(); e != s_entries.end(); ++e)
                {
                    if (e->second.party || !IsActive(e->second, now) || e->first == raw)
                        continue;
                    ++chatOnly;
                    if (oldest == s_entries.end() || e->second.chatExpiresAt < oldest->second.chatExpiresAt)
                        oldest = e;
                }
                const uint32_t cap = std::max<uint32_t>(1, g_PromoteMaxChatBots);
                if (chatOnly >= cap && oldest != s_entries.end())
                {
                    evicted = oldest->first;
                    s_entries.erase(oldest);
                }
            }
            s_entries[raw].chatExpiresAt = expires;
        }

        if (evicted)
            LOG_INFO("server.loading", "[Ollama Chat Promote] {} (guid={}) demoted: MaxChatBots={} reached",
                     NameOf(evicted), ObjectGuid(evicted).GetCounter(), g_PromoteMaxChatBots);
        if (fresh)
            LOG_INFO("server.loading", "[Ollama Chat Promote] {} (guid={}) promoted: chat ({} s idle window)",
                     bot->GetName(), bot->GetGUID().GetCounter(), g_PromoteChatTtlSec);
        return true;
    }

    bool IsOperator(Player* p)
    {
        return IsOperatorImpl(p);
    }

    bool MentionsName(const std::string& message, const std::string& name)
    {
        if (message.empty() || name.empty())
            return false;
        auto lower = [](std::string v)
        {
            std::transform(v.begin(), v.end(), v.begin(), [](unsigned char c) { return std::tolower(c); });
            return v;
        };
        const std::string msg = lower(message);
        const std::string needle = lower(name);
        for (size_t pos = msg.find(needle); pos != std::string::npos; pos = msg.find(needle, pos + 1))
        {
            const bool startOk = pos == 0 || !std::isalnum(static_cast<unsigned char>(msg[pos - 1]));
            const size_t end = pos + needle.size();
            const bool endOk = end >= msg.size() || !std::isalnum(static_cast<unsigned char>(msg[end]));
            if (startOk && endOk)
                return true;
        }
        return false;
    }

    bool IsSourceAllowed(int chatChannelSourceLocal)
    {
        return s_channels.count(chatChannelSourceLocal) > 0;
    }

    void Tick(uint32_t diffMs)
    {
        s_accumMs += diffMs;
        if (s_accumMs < 5000)
            return;
        s_accumMs = 0;

        {
            std::lock_guard<std::mutex> lock(s_mutex);
            if (!g_PromoteEnable && s_entries.empty())
                return;
        }
        ReconcileParty(time(nullptr));
    }

    void OnConfigLoaded()
    {
        ParseGatewayChannelList(g_PromoteChannels, s_channels, "Gateway.Promote.Channels");

        uint64_t templateGuid = g_PromoteTemplateBotGuid ? g_PromoteTemplateBotGuid : g_FleetLeaderGuid;
        GatewayBotConfig tpl;
        bool have = false;
        if (templateGuid)
        {
            auto it = g_GatewayBotConfigs.find(templateGuid);
            if (it != g_GatewayBotConfigs.end())
            {
                tpl = it->second;
                have = true;
            }
        }

        {
            std::lock_guard<std::mutex> lock(s_mutex);
            s_template = tpl;
            s_haveTemplate = have;
            s_templateGuid = templateGuid;
            // Empty whitelist = off: an open whitelist would let anyone use a
            // still-open window (codex review).
            if (!g_PromoteEnable || !have || g_GatewayWhitelistAccountIds.empty())
                s_entries.clear();
        }

        if (g_PromoteEnable && !have)
            LOG_ERROR("server.loading",
                      "[Ollama Chat Promote] enabled but template bot guid={} has no gateway override "
                      "(set Gateway.Promote.TemplateBotGuid to a bot in gateway_overrides.json) — promotion is off",
                      templateGuid);
        else if (g_PromoteEnable)
            LOG_INFO("server.loading",
                     "[Ollama Chat Promote] enabled: template guid={} (model '{}'), chat window {} s, max {} chat bots, party={}, channels='{}'",
                     templateGuid, tpl.model, g_PromoteChatTtlSec, g_PromoteMaxChatBots,
                     g_PromotePartyEnable, g_PromoteChannels);
    }

    std::string StatusLine()
    {
        const time_t now = time(nullptr);
        uint32_t chat = 0, party = 0;
        std::lock_guard<std::mutex> lock(s_mutex);
        for (auto const& [raw, e] : s_entries)
        {
            if (e.party) ++party;
            else if (e.chatExpiresAt > now) ++chat;
        }
        return "promote: enabled=" + std::to_string(g_PromoteEnable) +
               ", template=" + std::to_string(s_templateGuid) + (s_haveTemplate ? "" : " (MISSING)") +
               ", chat=" + std::to_string(chat) + ", party=" + std::to_string(party);
    }
}
