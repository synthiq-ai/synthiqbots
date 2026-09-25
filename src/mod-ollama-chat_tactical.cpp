#include "mod-ollama-chat_tactical.h"
#include "mod-ollama-chat_config.h"
#include "mod-ollama-chat_gateway.h"
#include "mod-ollama-chat_httpclient.h"
#include "mod-ollama-chat_llmwire.h"
#include "mod-ollama-chat_jev.h"
#include "mod-ollama-chat_tools.h"
#include "mod-ollama-chat-utilities.h"
#include "mod-ollama-chat_proactive.h"
#include "mod-ollama-chat_worldtask.h"
#include "mod-ollama-chat_promotion.h"
#include "Common.h"
#include "DatabaseEnv.h"
#include "CellImpl.h"
#include "Creature.h"
#include "GameObject.h"
#include "Group.h"
#include "GridNotifiers.h"
#include "GridNotifiersImpl.h"
#include "Map.h"
#include "ObjectAccessor.h"
#include "ObjectGuid.h"
#include "ObjectMgr.h"
#include "Player.h"
#include "Trainer.h"
#include "Item.h"
#include "Bag.h"
#include "QuestDef.h"
#include "PlayerbotAI.h"
#include "PlayerbotMgr.h"
#include "WorldSession.h"
#include "Language.h"
#include "Log.h"

#include <atomic>
#include <algorithm>
#include <cctype>
#include <chrono>
#include <condition_variable>
#include <ctime>
#include <deque>
#include <future>
#include <memory>
#include <mutex>
#include <list>
#include <queue>
#include <string>
#include <unordered_map>
#include <unordered_set>
#include <utility>
#include <vector>
#include <nlohmann/json.hpp>
#include <fmt/core.h>

// Phase 1 scaffold + Phase 2 inference adapter.
//
// Phase 2 adds:
//   - TacticalInference: minimal Ollama JSON-action adapter built on the
//     existing OllamaHttpClient. Parses Ollama's /api/generate response,
//     extracts the model reply, then parses that reply as a strict JSON
//     action object {action, args, escalate?}.
//   - Per-endpoint bounded-concurrency wrapper (PC and Mac Ollama hosts can run
//     concurrently, while bots sharing one URL queue behind that endpoint cap).
//   - Circuit breaker: if Ollama is unreachable, we STOP hammering it for
//     TACTICAL_BREAKER_COOLDOWN_SEC and keep the mod otherwise healthy. A
//     down tactical endpoint never degrades the rest of the module.
//   - Read-only tool whitelist: Phase 2 only dispatches safe read tools so we
//     can validate the full loop without in-game side effects. Phase 3 will
//     replace the probe heartbeat with the real presence-gated snapshot loop.

// ---------------------------------------------------------------------------
// Tactical inference adapter (Phase 2)
// ---------------------------------------------------------------------------

namespace
{
    std::string ReplaceAll(std::string haystack, const std::string& needle, const std::string& replacement)
    {
        if (needle.empty()) return haystack;
        size_t pos = 0;
        while ((pos = haystack.find(needle, pos)) != std::string::npos)
        {
            haystack.replace(pos, needle.length(), replacement);
            pos += replacement.length();
        }
        return haystack;
    }

    // Forward declarations — definitions live later in this file.
    TacticalDirective ReadDirective(uint64_t botGuid);
    void              WriteDirective(uint64_t botGuid, const std::string& goal, uint32_t ttlSec, const std::string& setBy);
    void              EnqueueEscalationImpl(uint64_t botGuid, const std::string& reason, const std::string& snapshot);
    void              WakeEscalationWorker();
    void              WriteAuditRow(uint64_t botGuid,
                                    const std::string& botName,
                                    const std::string& action,
                                    const std::string& resultKind,
                                    const std::string& errorMsg,
                                    bool escalated,
                                    const std::string& escalationReason,
                                    uint32_t latencyMs,
                                    bool inCombat,
                                    const char* backend = "llm",
                                    uint32_t promptTokens = 0);
    // Circuit-breaker constants. After N consecutive failures, we consider the
    // tactical Ollama endpoint unreachable and skip further calls for CD
    // seconds. Healthy calls reset the failure count. This keeps the mod
    // booting and running even if the user's Mac/PC is off or Ollama is down.
    constexpr int    kBreakerFailureThreshold = 3;
    constexpr time_t kBreakerCooldownSec      = 30;

    // Phase 3c tactical action whitelist — companion-mode menu.
    // Playerbots engine owns in-combat tactics; this set excludes raw
    // target-flipping and engine-exit actions during combat (enforced via
    // kCombatBlockedActions below).
    const std::unordered_set<std::string> kTacticalAllowedActions = {
        // No-op / quiet presence
        "tactical_idle",

        // Read-only observation
        "find_spirit_guide",
        "get_bot_state", "get_target",
        "get_nearby_creatures", "get_nearby_players", "get_nearby_gameobjects",
        // Which nearby herb/ore/fishing/lockpick nodes THIS bot can actually harvest —
        // the profession-gated companion to get_nearby_gameobjects, read before
        // committing a tick to bot_loot_nearby.
        "get_bot_gatherable_nodes",
        "find_battlemaster",
        "get_inventory", "get_group", "get_zone_info",
        "get_player_state", "get_player_gear", "get_player_quests",
        "find_nearby_quest_givers", "get_quest_details", "get_item_details", "find_stable_master",
        "get_bot_pet", "query_world_db_lookup", "get_bot_roster_by_account",
        "find_banker",
        "find_auctioneer",
        "get_fleet_status", "find_trainer", "find_vendor",
        // What the merchant find_vendor located actually SELLS, priced and gated for
        // this bot — read before spending a tick on bot_buy_item, which cannot tell
        // the agent whether the id it guessed is stocked.
        "get_bot_vendor_inventory",
        "find_flight_master", "get_bot_flight_destinations", "find_innkeeper", "get_bot_stats",
        "bot_mail_read", "get_bot_professions", "get_bot_recipes",
        "bot_ah_search", "bot_ah_my_listings", "bot_ah_my_bids",
        "find_spirit_healer",

        // Social (proactive chat)
        "whisper_player",
        "bot_emote", "bot_say", "bot_yell",
        // Pet stabling (hunter): park / retrieve the current pet + list stabled pets at a stable master
        "bot_stable_pet", "bot_unstable_pet", "get_bot_stabled_pets",

        // Movement / positioning. bot_flee is the deliberate save-the-bot escape:
        // unlike bot_stop_combat (engine-owned, lives in kCombatBlockedActions), this
        // is the tactical override the backlog called for — focus-fired healer / DPS
        // standing in AoE — so it intentionally stays callable mid-combat.
        "bot_follow", "bot_stay", "bot_disperse", "bot_flee", "bot_move_to", "bot_set_stance", "bot_taxi",

        // Strategic / squad coordination (flips engine MODE — not a per-tick override)
        "bot_set_strategy", "bot_set_range", "bot_set_formation", "bot_rti", "bot_reset_ai", "bot_list_strategies",

        // Pre-pull / support
        "bot_cast", "bot_pet_command", "bot_tame_pet", "bot_rename_pet", "bot_set_raid_target_icon",

        // Progression (sensitive actions are blocked by Tactical.RequireConfirmationFor)
        "bot_accept_quest", "bot_abandon_quest", "bot_turn_in_quest",
        "bot_choose_quest_reward", "bot_share_quest",
        "bot_equip_item", "bot_unequip_item", "bot_use_item",
        "bot_destroy_item", "bot_set_home", "bot_abandon_pet",
        "bot_use_gameobject",
        "bot_autogear", "bot_maintenance", "bot_train_spells",
        "bot_sell_junk", "bot_set_loot_filter", "bot_add_loot_item",
        "bot_remove_loot_item", "bot_bank_deposit", "bot_bank_withdraw",
        "bot_guild_bank_deposit", "bot_guild_bank_withdraw",
        "bot_guild_bank_deposit_money", "bot_guild_bank_withdraw_money",
        "bot_guild_promote", "bot_guild_demote", "bot_guild_set_note", "bot_guild_set_rank",
        "bot_guild_set_motd", "bot_guild_set_info", "bot_guild_set_leader",
        "bot_guild_add_rank", "bot_guild_remove_rank",
        "bot_guild_invite", "bot_guild_kick", "bot_guild_disband", "bot_accept_guild_invite", "bot_guild_leave",
        "bot_guild_buy_bank_tab", "bot_guild_set_bank_tab", "bot_guild_set_bank_tab_text",
        "bot_mail_send", "bot_trade_give", "bot_accept_trade", "bot_split_stack", "bot_stack_combine",
        "bot_ah_post", "bot_ah_bid", "bot_ah_cancel",
        // Hunter pet stabling: buy an extra stable slot (gold cost; the stable_full escape)
        "bot_buy_stable_slot",

        // PvP / battleground: queue for a battleground at a nearby battlemaster (the
        // write-side pair to the find_battlemaster locator; reversible -- leave the queue).
        "bot_queue_battleground",

        // Instance difficulty / lockout control
        "bot_set_difficulty",

        // Instance lockout maintenance -- reset the bot's Normal-difficulty 5-man
        // lockouts so a farm loop can re-run a cleared dungeon (read side:
        // get_bot_saved_instances). Dungeon-only; leader-gated when grouped.
        "instance_reset",

        // Raid coordination
        "bot_roll",

        // PvP / battleground queue accept — port the bot into a match once a
        // queue pop is pending (the counterpart to bot_queue_battleground /
        // bot_leave_battleground_queue). Placed here, away from the
        // bot_queue_battleground cluster above, to avoid a merge collision with
        // that still-open sibling PR.
        "bot_accept_battleground_pop",

        // Self-recovery (out-of-combat only — kCombatBlockedActions still gates it
        // mid-fight so we don't release out from under the engine). Lets tactical
        // ticks auto-revive a dead bot without human prompting. bot_release_spirit
        // is the corpse-run sibling of bot_revive — chooses ghost-form (no rez
        // sickness) when a healer is en route. bot_revive_target is the party-rez
        // analog (priest / paladin / druid / shaman cast on a dead party member);
        // out-of-combat only, kCombatBlockedActions still gates it.
        // bot_combat_rez (Druid Rebirth) is the in-combat sibling — deliberately
        // ABSENT from kCombatBlockedActions because Rebirth's whole value is the
        // mid-pull save. bot_self_reincarnate (Shaman Ankh / Warlock Soulstone) is
        // the consumable-driven in-place self-rez sibling — also deliberately NOT
        // in kCombatBlockedActions because Reincarnation is the mid-pull save when
        // the bot just died and the Ankh is sitting unused.
        // bot_accept_resurrect_request is the RECEIVE-side counterpart — ACKs an
        // inbound rez popup cast by a human healer or peer bot. Not in
        // kCombatBlockedActions: the rez itself fires the engine's full Teleport
        // chain regardless of combat state, and an unanswered popup is exactly
        // what we want this verb to clear.
        "bot_revive",
        "bot_release_spirit",
        "bot_revive_target",
        "bot_combat_rez",
        "bot_self_reincarnate",
        "bot_accept_resurrect_request",

        // PvP / battleground: leave/forfeit a battleground the bot is CURRENTLY
        // INSIDE (distinct from bot_leave_battleground_queue's queue-side exit).
        // Placed here, away from the bot_queue_battleground / accept / leave-queue
        // cluster above, to avoid a merge collision with those still-open sibling PRs.
        "bot_leave_active_battleground",

        // Squad orders (only for leader bots — gateway-side allowlist enforces)
        "leader_command", "leader_command_all",
        "leader_list_targets", "leader_get_command_help",

        // PvP / battleground queue exit — leave a BG/arena queue the bot has joined
        // (the reversible write-side pair to bot_queue_battleground). Placed here,
        // away from the bot_queue_battleground cluster, to avoid a merge collision
        // with that still-open sibling PR.
        "bot_leave_battleground_queue",
    };

    // Actions playerbots engine owns during active combat. Tactical picking
    // these mid-fight flips target / exits combat out from under the engine
    // and causes jank. Blocked unless AllowCombatOverride=1.
    const std::unordered_set<std::string> kCombatBlockedActions = {
        "bot_attack_target",   // engine's threat + assist picks targets
        "bot_stop_combat",     // engine exits combat naturally
        "bot_revive",          // only meaningful while dead
        "bot_release_spirit",  // only meaningful while dead
        "bot_revive_target",   // full-rez spells refuse to start while caster is in combat
    };

    const std::unordered_set<std::string> kSafeTacticalEmotes = {
        "wave", "nod", "cheer", "laugh", "shrug", "dance",
        "bow", "salute", "clap", "point", "smile", "sigh",
        "thank", "grats", "train", "roar", "flex", "kneel",
        "sleep", "confused"
    };

    bool IsSafeEmoteToken(const std::string& value)
    {
        return kSafeTacticalEmotes.find(value) != kSafeTacticalEmotes.end();
    }

    std::string NormalizeTacticalEmote(const std::string& raw)
    {
        std::string current;
        std::vector<std::string> tokens;
        for (unsigned char c : raw)
        {
            if (std::isalnum(c))
            {
                current.push_back(static_cast<char>(std::tolower(c)));
                continue;
            }

            if (!current.empty())
            {
                tokens.push_back(current);
                current.clear();
            }
        }
        if (!current.empty())
            tokens.push_back(current);

        for (const std::string& token : tokens)
        {
            if (IsSafeEmoteToken(token))
                return token;
        }

        return "wave";
    }

    std::string TrimCopy(std::string value)
    {
        auto notSpace = [](unsigned char c) { return !std::isspace(c); };
        value.erase(value.begin(), std::find_if(value.begin(), value.end(), notSpace));
        value.erase(std::find_if(value.rbegin(), value.rend(), notSpace).base(), value.end());
        return value;
    }

    bool IsWhitelistedOrAnyRealHuman(Player* p)
    {
        return IsWhitelistedHumanPlayer(p, /*anyHumanWhenNoWhitelist=*/true);
    }

    std::vector<Player*> CollectPresenceHumans()
    {
        std::vector<Player*> humans;
        for (auto const& pair : ObjectAccessor::GetPlayers())
        {
            Player* p = pair.second;
            if (IsWhitelistedOrAnyRealHuman(p))
                humans.push_back(p);
        }
        return humans;
    }

    bool HasRealHumanInSayRange(Player* bot)
    {
        if (!bot || !bot->IsInWorld()) return false;
        for (Player* p : CollectPresenceHumans())
        {
            if (p && p->GetMapId() == bot->GetMapId() && bot->GetDistance(p) <= g_SayDistance)
                return true;
        }
        return false;
    }

    struct TacticalRecentEvent
    {
        uint64_t sourceGuid = 0;
        std::string sourceName;
        uint32_t mapId = 0;
        float x = 0.0f;
        float y = 0.0f;
        float z = 0.0f;
        std::string eventType;
        std::string detail;
        time_t expiresAt = 0;
    };

    std::mutex s_recentEventsMutex;
    std::deque<TacticalRecentEvent> s_recentEvents;
    constexpr time_t kRecentEventTtlSec = 60;
    constexpr size_t kMaxRecentEvents = 64;

    std::string RecentEventForBot(Player* bot)
    {
        if (!bot || !bot->IsInWorld()) return "";
        if (g_TacticalAmbientEventReactionChance == 0) return "";
        if (g_TacticalAmbientEventReactionChance < 100
            && urand(1, 100) > g_TacticalAmbientEventReactionChance)
            return "";

        time_t now = time(nullptr);
        std::lock_guard<std::mutex> lock(s_recentEventsMutex);
        while (!s_recentEvents.empty() && s_recentEvents.front().expiresAt <= now)
            s_recentEvents.pop_front();

        for (auto it = s_recentEvents.rbegin(); it != s_recentEvents.rend(); ++it)
        {
            if (it->mapId != bot->GetMapId())
                continue;
            float dx = bot->GetPositionX() - it->x;
            float dy = bot->GetPositionY() - it->y;
            float dz = bot->GetPositionZ() - it->z;
            float dist2 = dx * dx + dy * dy + dz * dz;
            float radius = std::max<float>(g_TacticalNearbyBotRadius, g_SayDistance);
            if (dist2 > radius * radius)
                continue;

            std::string event = " recent_event_type=" + it->eventType;
            if (!it->sourceName.empty()) event += " recent_event_actor=" + it->sourceName;
            if (!it->detail.empty()) event += " recent_event_detail=" + it->detail;
            return event;
        }
        return "";
    }

    struct AmbientMemory
    {
        std::chrono::steady_clock::time_point lastSpeechAt{};
        std::chrono::steady_clock::time_point lastEmoteAt{};
        std::deque<std::chrono::steady_clock::time_point> visibleActions;
        std::deque<std::pair<std::string, std::chrono::steady_clock::time_point>> recentSpeech;
        std::deque<std::pair<std::string, std::chrono::steady_clock::time_point>> recentEmotes;
    };

    std::mutex s_ambientMutex;
    std::unordered_map<uint64_t, AmbientMemory> s_ambientByBot;
    constexpr uint32_t kRepeatSpeechSuppressSec = 600;
    constexpr uint32_t kRepeatEmoteSuppressSec = 300;

    nlohmann::json TacticalIdleResult(const std::string& reason)
    {
        return nlohmann::json{{"ok", true}, {"audit_action", "tactical_idle"}, {"noop", reason}};
    }

    bool IsRecentRepeated(std::deque<std::pair<std::string, std::chrono::steady_clock::time_point>>& values,
                          const std::string& value,
                          std::chrono::steady_clock::time_point now,
                          uint32_t ttlSec)
    {
        while (!values.empty()
               && std::chrono::duration_cast<std::chrono::seconds>(now - values.front().second).count() > ttlSec)
            values.pop_front();
        for (const auto& item : values)
            if (item.first == value)
                return true;
        values.emplace_back(value, now);
        return false;
    }

    nlohmann::json TryClaimAmbientVisibleAction(uint64_t botGuid,
                                                const std::string& actionName,
                                                const std::string& value)
    {
        if (!g_TacticalAmbientEnable)
            return TacticalIdleResult("ambient disabled");

        auto now = std::chrono::steady_clock::now();
        std::lock_guard<std::mutex> lock(s_ambientMutex);
        AmbientMemory& mem = s_ambientByBot[botGuid];

        while (!mem.visibleActions.empty()
               && std::chrono::duration_cast<std::chrono::seconds>(now - mem.visibleActions.front()).count() >= 60)
            mem.visibleActions.pop_front();
        if (g_TacticalAmbientMaxVisibleActionsPerMinute > 0
            && mem.visibleActions.size() >= g_TacticalAmbientMaxVisibleActionsPerMinute)
            return TacticalIdleResult("ambient visible action rate-limited");

        if (actionName == "bot_emote")
        {
            if (mem.lastEmoteAt.time_since_epoch().count() != 0)
            {
                auto gap = std::chrono::duration_cast<std::chrono::seconds>(now - mem.lastEmoteAt).count();
                if (gap < static_cast<int64_t>(g_TacticalAmbientMinEmoteGapSec))
                    return TacticalIdleResult("ambient emote gap");
            }
            if (IsRecentRepeated(mem.recentEmotes, value, now, kRepeatEmoteSuppressSec))
                return TacticalIdleResult("ambient repeated emote");
            mem.lastEmoteAt = now;
        }
        else if (actionName == "bot_say" || actionName == "bot_yell" || actionName == "whisper_player")
        {
            if (mem.lastSpeechAt.time_since_epoch().count() != 0)
            {
                auto gap = std::chrono::duration_cast<std::chrono::seconds>(now - mem.lastSpeechAt).count();
                if (gap < static_cast<int64_t>(g_TacticalAmbientMinSpeechGapSec))
                    return TacticalIdleResult("ambient speech gap");
            }
            if (!value.empty() && IsRecentRepeated(mem.recentSpeech, value, now, kRepeatSpeechSuppressSec))
                return TacticalIdleResult("ambient repeated speech");
            mem.lastSpeechAt = now;
        }

        mem.visibleActions.push_back(now);
        return nlohmann::json{};
    }

    bool ValidateAmbientSpeech(std::string& text, std::string& error)
    {
        text = TrimCopy(text);
        if (text.empty())
        {
            error = "missing 'text'";
            return false;
        }
        if (text.size() > 160)
            text.resize(160);
        for (char c : text)
        {
            unsigned char uc = static_cast<unsigned char>(c);
            if (uc < 32 && c != '\t')
            {
                error = "text contains control characters";
                return false;
            }
        }
        return true;
    }

    nlohmann::json DispatchTacticalSay(Player* bot, uint64_t botGuid, nlohmann::json& args)
    {
        std::string text = args.value("text", std::string{});
        std::string err;
        if (!ValidateAmbientSpeech(text, err))
            return nlohmann::json{{"error", err}};

        PlayerbotAI* ai = bot ? PlayerbotsMgr::instance().GetPlayerbotAI(bot) : nullptr;
        if (!bot || !bot->IsInWorld() || !ai)
            return nlohmann::json{{"error", "bot not in world"}};

        if (bot->GetGroup() && !g_DisableForParty)
        {
            nlohmann::json claim = TryClaimAmbientVisibleAction(botGuid, "bot_say", text);
            if (!claim.empty())
                return claim;
            ai->SayToParty(text);
            LOG_INFO("server.loading", "[Ollama Chat Tactical] bot_say party: bot={} text='{}'", botGuid, text);
            return nlohmann::json{{"ok", true}, {"channel", "party"}, {"text", text}};
        }

        if (!g_DisableForSayYell && HasRealHumanInSayRange(bot))
        {
            nlohmann::json claim = TryClaimAmbientVisibleAction(botGuid, "bot_say", text);
            if (!claim.empty())
                return claim;
            bot->Say(text, LANG_UNIVERSAL);
            LOG_INFO("server.loading", "[Ollama Chat Tactical] bot_say say: bot={} text='{}'", botGuid, text);
            return nlohmann::json{{"ok", true}, {"channel", "say"}, {"text", text}};
        }

        return TacticalIdleResult("no party or nearby say audience");
    }

    // Companion-mode system prompt. The model is explicitly told its role is
    // teammate + coordinator, NOT player; combat mechanics stay with the
    // engine; and it should prefer social actions (ask first, narrate) over
    // unilateral moves.
    std::string BuildTacticalSystemPrompt(const std::string& botName,
                                          const std::string& botClass,
                                          uint32_t botLevel,
                                          bool inCombat)
    {
        std::vector<std::string> allowed(kTacticalAllowedActions.begin(), kTacticalAllowedActions.end());
        std::sort(allowed.begin(), allowed.end());
        std::string allowedActions;
        for (size_t i = 0; i < allowed.size(); ++i)
        {
            if (i != 0) allowedActions += ", ";
            allowedActions += allowed[i];
        }

        std::string p;
        if (!g_TacticalSystemPromptText.empty())
            p += g_TacticalSystemPromptText;
        else
            p += "You are the local tactical companion layer for a World of Warcraft player-bot. Choose one allowed tool action from the current snapshot, or choose a quiet read action when nothing needs doing.\n";

        p = ReplaceAll(p, "{{botName}}", botName);
        p = ReplaceAll(p, "{{botClass}}", botClass);
        p = ReplaceAll(p, "{{botLevel}}", std::to_string(botLevel));
        p = ReplaceAll(p, "{{inCombat}}", inCombat ? "true" : "false");
        p = ReplaceAll(p, "{{allowedActions}}", allowedActions);

        if (!p.empty()) p += "\n\n";
        p += "Runtime context:\n";
        p += "- botName: " + botName + "\n";
        p += "- botClass: " + botClass + "\n";
        p += "- botLevel: " + std::to_string(botLevel) + "\n";
        p += "- inCombat: " + std::string(inCombat ? "true" : "false") + "\n";
        p += "- allowedActions: " + allowedActions + "\n";
        p += "- C++ will reject any action not listed in allowedActions.\n";
        p += "- Most tool args take botGuid or bot_guid; use your own bot guid from the snapshot.\n";
        if (inCombat && !g_TacticalAllowCombatOverride)
        {
            p += "- Combat override is disabled; direct combat override actions are blocked while inCombat=true.\n";
        }
        p += "Reply with EXACTLY ONE compact JSON object and nothing else:\n";
        p += "  {\"action\": \"<name>\", \"args\": {<object>}, \"escalate\": \"<reason>\" (optional)}\n";
        return p;
    }

    class TacticalInference
    {
    public:
        // Health snapshot for logging.
        struct Health {
            int    consecutiveFailures = 0;
            time_t downSince           = 0;
            bool   IsOpen(time_t now) const { return downSince != 0 && (now - downSince) < kBreakerCooldownSec; }
        };

        static TacticalInference& Instance()
        {
            static TacticalInference s;
            return s;
        }

        // Fires one Ollama inference. Returns parsed JSON action or empty on failure.
        // Safe to call when Ollama is unreachable — returns empty, never throws.
        // `deadline` is the ABSOLUTE point by which the whole call — endpoint
        // slot wait included — must be over. Default = now + cfg.timeoutMs
        // (today's behaviour); the jev tier passes the tick's original deadline
        // so its own attempt is charged against the same budget.
        nlohmann::json Query(const TacticalBotConfig& cfg,
                             const std::string& systemPrompt,
                             const std::string& userPrompt,
                             std::chrono::steady_clock::time_point deadline = std::chrono::steady_clock::time_point{})
        {
            if (cfg.url.empty() || cfg.model.empty())
            {
                LOG_DEBUG("server.loading",
                          "[Ollama Chat Tactical] inference skipped — resolved url/model is empty");
                return nlohmann::json{};
            }
            if (deadline == std::chrono::steady_clock::time_point{})
                deadline = std::chrono::steady_clock::now() + std::chrono::milliseconds(std::max<uint32_t>(1000u, cfg.timeoutMs));

            EndpointSlot slot = AcquireEndpointSlot(cfg, deadline);
            if (!slot)
                return nlohmann::json{};

            // Whatever the slot wait consumed comes off the HTTP budget; below a
            // useful floor the call is not started at all (not a breaker failure —
            // the endpoint did nothing wrong).
            const int64_t remainingMs = std::chrono::duration_cast<std::chrono::milliseconds>(
                deadline - std::chrono::steady_clock::now()).count();
            if (remainingMs < 500)
            {
                LOG_DEBUG("server.loading", "[Ollama Chat Tactical] inference skipped — {}ms left after slot wait", remainingMs);
                return nlohmann::json{};
            }

            // Request shape is chosen from cfg.url by LlmWire: an Ollama /api/generate URL
            // gets the historical body (stream=false, format=json, keep_alive=30m so the
            // model stays GPU-resident between ticks, think=false to suppress Gemma/qwen3
            // reasoning preambles, options.num_ctx to shrink the KV cache); an OpenAI
            // /chat/completions URL (DeepSeek etc.) gets messages[] + response_format
            // json_object with the Ollama-only knobs dropped. Tactical decisions are
            // latency-critical and a JSON-constrained action needs no chain-of-thought.
            nlohmann::json req = LlmWire::BuildRequest(cfg.url, cfg.model,
                                                       SanitizeUTF8(systemPrompt),
                                                       SanitizeUTF8(userPrompt),
                                                       cfg.numCtx, /*jsonMode=*/true, /*noThink=*/true);
            std::string body = req.dump();

            OllamaHttpClient client;
            client.SetTimeoutMs(static_cast<uint32_t>(remainingMs));

            if (!client.IsAvailable())
            {
                RecordFailure(slot.state, cfg.url, "http client unavailable");
                return nlohmann::json{};
            }

            std::string raw = client.Post(cfg.url, body, LlmWire::Headers(cfg.url));
            if (raw.empty())
            {
                RecordFailure(slot.state, cfg.url, fmt::format("empty response from {}", cfg.url));
                return nlohmann::json{};
            }

            // Unwrap the envelope for whichever protocol cfg.url speaks. Any failure
            // here means the endpoint is down, speaking the other protocol, or (for
            // OpenAI-compatible providers) returned {"error":{"message":...}} — that
            // message is surfaced verbatim so an invalid key reads as such.
            std::string replyText, wireErr;
            if (!LlmWire::ExtractText(cfg.url, raw, replyText, wireErr))
            {
                RecordFailure(slot.state, cfg.url, wireErr);
                return nlohmann::json{};
            }

            nlohmann::json action = ParseActionJson(replyText);
            if (action.is_null() || !action.is_object() || !action.contains("action"))
            {
                RecordFailure(slot.state, cfg.url,
                              fmt::format("model reply not a valid action object: '{}'",
                                          replyText.size() > 120 ? replyText.substr(0, 120) + "..." : replyText));
                return nlohmann::json{};
            }

            RecordSuccess(slot.state, cfg.url);
            return action;
        }

        Health Snapshot(const std::string& url)
        {
            std::lock_guard<std::mutex> lock(m_mutex);
            auto it = m_endpoints.find(url);
            if (it == m_endpoints.end()) return Health{};
            return it->second->health;
        }

        // Called by TacticalLeaderTick::Start() so a config reload pointing at
        // new / recovered URLs doesn't inherit previous endpoint UNREACHABLE state.
        void Reset()
        {
            std::lock_guard<std::mutex> lock(m_mutex);
            m_endpoints.clear();
        }

    private:
        struct EndpointState
        {
            Health health;
            uint32_t inFlight = 0;
            std::condition_variable cv;
        };

        struct EndpointSlot
        {
            TacticalInference* owner = nullptr;
            std::shared_ptr<EndpointState> state;

            EndpointSlot() = default;
            EndpointSlot(TacticalInference* owner_, std::shared_ptr<EndpointState> state_)
                : owner(owner_), state(std::move(state_)) {}
            EndpointSlot(const EndpointSlot&) = delete;
            EndpointSlot& operator=(const EndpointSlot&) = delete;
            EndpointSlot(EndpointSlot&& other) noexcept
                : owner(other.owner), state(std::move(other.state))
            {
                other.owner = nullptr;
            }
            EndpointSlot& operator=(EndpointSlot&& other) noexcept
            {
                if (this != &other)
                {
                    Release();
                    owner = other.owner;
                    state = std::move(other.state);
                    other.owner = nullptr;
                }
                return *this;
            }
            ~EndpointSlot() { Release(); }
            explicit operator bool() const { return owner != nullptr && state != nullptr; }

        private:
            void Release()
            {
                if (owner && state)
                {
                    owner->ReleaseEndpointSlot(state);
                    owner = nullptr;
                    state.reset();
                }
            }
        };

        TacticalInference() = default;

        // Accept a clean JSON or try to locate the first {...} substring.
        // LLMs occasionally add leading/trailing whitespace or quote the JSON.
        nlohmann::json ParseActionJson(const std::string& raw)
        {
            try { return nlohmann::json::parse(raw); }
            catch (...) { /* fall through */ }

            size_t open = raw.find('{');
            size_t close = raw.rfind('}');
            if (open == std::string::npos || close == std::string::npos || close <= open)
                return nlohmann::json{};

            try { return nlohmann::json::parse(raw.substr(open, close - open + 1)); }
            catch (...) { return nlohmann::json{}; }
        }

        std::shared_ptr<EndpointState> GetEndpointStateLocked(const std::string& url)
        {
            auto it = m_endpoints.find(url);
            if (it != m_endpoints.end())
                return it->second;

            auto state = std::make_shared<EndpointState>();
            m_endpoints[url] = state;
            return state;
        }

        EndpointSlot AcquireEndpointSlot(const TacticalBotConfig& cfg,
                                         std::chrono::steady_clock::time_point deadline)
        {
            std::unique_lock<std::mutex> lock(m_mutex);
            auto state = GetEndpointStateLocked(cfg.url);
            time_t now = time(nullptr);
            if (state->health.IsOpen(now))
            {
                // Breaker open for this endpoint only — silent skip. No log spam.
                return EndpointSlot{};
            }

            uint32_t limit = cfg.maxConcurrentQueries;
            if (limit > 0)
            {
                // Bounded by the caller's deadline: a bot must not wait through
                // another bot's whole inference and then start with a budget it
                // no longer has (the wait used to be unbounded).
                if (!state->cv.wait_until(lock, deadline, [&] { return state->inFlight < limit; }))
                    return EndpointSlot{};
                now = time(nullptr);
                if (state->health.IsOpen(now))
                    return EndpointSlot{};
            }

            ++state->inFlight;
            return EndpointSlot(this, state);
        }

        void ReleaseEndpointSlot(const std::shared_ptr<EndpointState>& state)
        {
            std::lock_guard<std::mutex> lock(m_mutex);
            if (state->inFlight > 0)
                --state->inFlight;
            state->cv.notify_one();
        }

        void RecordFailure(const std::shared_ptr<EndpointState>& state,
                           const std::string& url,
                           const std::string& reason)
        {
            time_t now = time(nullptr);
            std::lock_guard<std::mutex> lock(m_mutex);
            ++state->health.consecutiveFailures;
            LOG_DEBUG("server.loading", "[Ollama Chat Tactical] inference failure endpoint='{}' ({}/{}): {}",
                      url, state->health.consecutiveFailures, kBreakerFailureThreshold, reason);
            if (state->health.consecutiveFailures >= kBreakerFailureThreshold)
            {
                bool wasOpen = (state->health.downSince != 0);
                // Always refresh the timestamp so the cooldown restarts on each
                // post-recovery failure; log only on state transitions.
                state->health.downSince = now;
                if (!wasOpen)
                {
                    LOG_INFO("server.loading",
                             "[Ollama Chat Tactical] endpoint \"{}\" marked UNREACHABLE — pausing tactical inference for {}s",
                             url, static_cast<long>(kBreakerCooldownSec));
                }
            }
        }

        void RecordSuccess(const std::shared_ptr<EndpointState>& state, const std::string& url)
        {
            std::lock_guard<std::mutex> lock(m_mutex);
            bool wasDown = (state->health.downSince != 0);
            state->health.consecutiveFailures = 0;
            state->health.downSince = 0;
            if (wasDown)
            {
                LOG_INFO("server.loading",
                         "[Ollama Chat Tactical] endpoint \"{}\" RECOVERED",
                         url);
            }
        }

        std::mutex m_mutex;
        std::unordered_map<std::string, std::shared_ptr<EndpointState>> m_endpoints;
    };

    // Per-bot last-whisper timestamp — used by DispatchTacticalAction to hard-
    // limit whisper_player frequency. Without this gate the tactical model gets
    // stuck in a narrator loop: every idle tick it picks whisper_player with a
    // generic greeting, which spams the human's chat AND triggers reactive
    // playerbots chatter ("Asked Claude for Combat-Strategies" → `co ?` loop).
    // The rate limit forces the model to diversify (get_bot_state, bot_emote)
    // when nothing material has changed.
    std::mutex s_lastWhisperMutex;
    std::unordered_map<uint64_t, std::chrono::steady_clock::time_point> s_lastWhisperAt;

    // Dispatch a parsed action object through the existing MCP tool registry.
    // Enforces the tactical companion whitelist, the HITL confirmation gate,
    // the combat-block rule (playerbots engine owns in-combat tactics unless
    // AllowCombatOverride=1), and the per-bot whisper rate limit.
    nlohmann::json DispatchTacticalAction(Player* bot, uint64_t botGuid, const nlohmann::json& actionObj)
    {
        if (!actionObj.is_object() || !actionObj.contains("action"))
            return nlohmann::json{{"error", "malformed action object"}};

        std::string name;
        try { name = actionObj.at("action").get<std::string>(); }
        catch (...) { return nlohmann::json{{"error", "action field not a string"}}; }

        if (kTacticalAllowedActions.find(name) == kTacticalAllowedActions.end())
        {
            LOG_DEBUG("server.loading",
                      "[Ollama Chat Tactical] rejecting action '{}' — not in tactical whitelist", name);
            return nlohmann::json{{"error", "action not in tactical whitelist"}, {"action", name}};
        }

        nlohmann::json args = nlohmann::json::object();
        if (actionObj.contains("args") && actionObj["args"].is_object())
            args = actionObj["args"];

        if (name == "tactical_idle")
            return TacticalIdleResult("model chose idle");

        if (bot && bot->IsInCombat() && g_DisableRepliesInCombat
            && (name == "bot_emote" || name == "bot_say" || name == "bot_yell" || name == "whisper_player"))
        {
            return TacticalIdleResult("ambient suppressed in combat");
        }

        if (name == "bot_emote")
        {
            std::string requested = args.value("emote", std::string{});
            std::string normalized = NormalizeTacticalEmote(requested);
            if (requested != normalized)
            {
                LOG_DEBUG("server.loading",
                          "[Ollama Chat Tactical] normalized bot_emote value '{}' -> '{}'",
                          requested, normalized);
            }
            args["emote"] = normalized;

            nlohmann::json claim = TryClaimAmbientVisibleAction(botGuid, name, normalized);
            if (!claim.empty())
                return claim;
        }

        if (name == "bot_say")
        {
            return DispatchTacticalSay(bot, botGuid, args);
        }

        if (name == "bot_yell")
        {
            std::string text = args.value("text", std::string{});
            std::string err;
            if (!ValidateAmbientSpeech(text, err))
                return nlohmann::json{{"error", err}};
            args["text"] = text;
            nlohmann::json claim = TryClaimAmbientVisibleAction(botGuid, name, text);
            if (!claim.empty())
                return claim;
        }

        // Whisper rate limit — see s_lastWhisperAt comment above.
        if (name == "whisper_player" && g_TacticalMinWhisperGapSec > 0)
        {
            auto now = std::chrono::steady_clock::now();
            std::lock_guard<std::mutex> lock(s_lastWhisperMutex);
            auto it = s_lastWhisperAt.find(botGuid);
            if (it != s_lastWhisperAt.end())
            {
                auto gapSec = std::chrono::duration_cast<std::chrono::seconds>(now - it->second).count();
                if (gapSec < static_cast<int64_t>(g_TacticalMinWhisperGapSec))
                {
                    LOG_DEBUG("server.loading",
                              "[Ollama Chat Tactical] blocking whisper_player — last whisper {}s ago < min {}s gap",
                              gapSec, g_TacticalMinWhisperGapSec);
                    return nlohmann::json{
                        {"error", fmt::format("whisper_player rate-limited: last whisper {}s ago, min gap {}s. Pick a different action (get_bot_state, bot_emote, bot_follow) — do NOT whisper again.",
                                              gapSec, g_TacticalMinWhisperGapSec)},
                        {"action", name}};
                }
            }
            s_lastWhisperAt[botGuid] = now;
        }

        // HITL confirmation gate: actions listed in Tactical.RequireConfirmationFor
        // must NOT silently execute. Until the interactive confirm/respond flow
        // lands (Phase 4.5), we reject them with a clear message so the local
        // model can fall back to a safer pick on its next tick. Silent-execute
        // would defeat the purpose of the safety knob.
        if (g_TacticalRequireConfirmationSet.count(name) > 0)
        {
            LOG_DEBUG("server.loading",
                      "[Ollama Chat Tactical] blocking action '{}' — requires HITL confirmation (unimplemented; ask the human)",
                      name);
            return nlohmann::json{
                {"error", "action requires HITL confirmation — ask the human via whisper_player first, then retry (confirmation flow TBD)"},
                {"action", name}};
        }

        // Combat-block: during active combat, the playerbots engine owns the
        // tick-level combat loop (threat, target selection, rotations). Letting
        // tactical flip targets or exit combat mid-fight causes jank.
        if (bot && bot->IsInCombat() && !g_TacticalAllowCombatOverride
            && kCombatBlockedActions.find(name) != kCombatBlockedActions.end())
        {
            LOG_DEBUG("server.loading",
                      "[Ollama Chat Tactical] blocking combat-tactic action '{}' during active combat — engine owns tactics",
                      name);
            return nlohmann::json{
                {"error", "action blocked during combat (engine owns combat tactics)"},
                {"action", name}};
        }

        if (bot && bot->IsInCombat() && !g_TacticalAllowCombatOverride && name == "bot_pet_command")
        {
            std::string command = args.value("command", std::string{});
            std::transform(command.begin(), command.end(), command.begin(),
                           [](unsigned char c) { return static_cast<char>(std::tolower(c)); });
            if (command == "attack")
            {
                LOG_DEBUG("server.loading",
                          "[Ollama Chat Tactical] blocking pet attack during active combat — engine owns combat tactics");
                return nlohmann::json{
                    {"error", "pet attack blocked during combat (engine owns combat tactics)"},
                    {"action", name}};
            }
        }

        // playerGuid=0 marks an autonomous (no-human-driver) dispatch — same
        // convention the leader-tick uses.
        return DispatchGatewayTool(botGuid, /*playerGuid=*/0, name, args);
    }

    // ---- Phase 3: presence gate, per-bot cooldown, compact snapshot, tick ----

    // Per-bot cooldown tracker. Records the last time we completed a tick for
    // a given bot guid. Second access is always through s_cooldownMutex.
    std::mutex s_cooldownMutex;
    std::unordered_map<uint64_t, std::chrono::steady_clock::time_point> s_lastTickAt;

    bool TryClaimBotCooldown(uint64_t guid)
    {
        using clock = std::chrono::steady_clock;
        auto now = clock::now();
        uint32_t cooldownMs = std::max<uint32_t>(100u, g_TacticalBotCooldownMs);
        std::lock_guard<std::mutex> lock(s_cooldownMutex);
        auto it = s_lastTickAt.find(guid);
        if (it != s_lastTickAt.end())
        {
            auto sinceMs = std::chrono::duration_cast<std::chrono::milliseconds>(now - it->second).count();
            if (sinceMs < static_cast<int64_t>(cooldownMs))
                return false;
        }
        s_lastTickAt[guid] = now;
        return true;
    }

    // Returns true if at least one whitelisted human (per
    // Gateway.AutoClaimAccountIds) is currently online and in-world.
    // Phase 3a: global presence gate. Phase 3d will tighten to per-bot
    // proximity (same party / same map within range).
    bool IsAnyWhitelistedHumanOnline()
    {
        if (!g_TacticalHumanPresenceRequired) return true;
        return !CollectPresenceHumans().empty();
    }

    std::string BuildAmbientWorldContext(Player* bot)
    {
        if (!bot || !bot->IsInWorld()) return "";
        std::string s;

        Unit* unitInRange = nullptr;
        Acore::AnyUnitInObjectRangeCheck unitCheck(bot, g_SayDistance);
        Acore::UnitSearcher<Acore::AnyUnitInObjectRangeCheck> unitSearcher(bot, unitInRange, unitCheck);
        Cell::VisitObjects(bot, unitSearcher, g_SayDistance);
        if (unitInRange && unitInRange->GetTypeId() == TYPEID_UNIT)
        {
            Creature* c = unitInRange->ToCreature();
            if (c)
            {
                s += " nearby_creature=" + c->GetName();
                if (c->HasNpcFlag(UNIT_NPC_FLAG_VENDOR)) s += " nearby_vendor=true";
                if (c->HasNpcFlag(UNIT_NPC_FLAG_QUESTGIVER)) s += " nearby_questgiver=true";
            }
        }

        GameObject* goInRange = nullptr;
        Acore::GameObjectInRangeCheck goCheck(bot->GetPositionX(), bot->GetPositionY(), bot->GetPositionZ(), g_SayDistance);
        Acore::GameObjectSearcher<Acore::GameObjectInRangeCheck> goSearcher(bot, goInRange, goCheck);
        Cell::VisitObjects(bot, goSearcher, g_SayDistance);
        if (goInRange)
            s += " nearby_object=" + goInRange->GetName();

        int freeSlots = 0;
        for (uint8 i = INVENTORY_SLOT_ITEM_START; i < INVENTORY_SLOT_ITEM_END; ++i)
            if (!bot->GetItemByPos(INVENTORY_SLOT_BAG_0, i)) ++freeSlots;
        s += " free_bag_slots=" + std::to_string(freeSlots);

        uint32_t incompleteQuestCount = 0;
        std::string firstIncompleteQuest;
        for (auto const& qs : bot->getQuestStatusMap())
        {
            if (qs.second.Status != QUEST_STATUS_INCOMPLETE)
                continue;
            ++incompleteQuestCount;
            if (firstIncompleteQuest.empty())
                if (Quest const* q = sObjectMgr->GetQuestTemplate(qs.first))
                    firstIncompleteQuest = q->GetTitle();
        }
        s += " quests_incomplete=" + std::to_string(incompleteQuestCount);
        if (!firstIncompleteQuest.empty())
            s += " quest_hint=" + firstIncompleteQuest;

        if (bot->GetMap() && bot->GetMap()->IsDungeon())
        {
            s += " in_dungeon=true dungeon=";
            s += bot->GetMap()->GetMapName();
        }

        s += RecentEventForBot(bot);
        return s;
    }

    std::vector<uint64_t> CollectTacticalBotGuids()
    {
        std::vector<uint64_t> out;
        std::unordered_set<uint64_t> seen;
        auto add = [&](uint64_t guid)
        {
            if (guid == 0 || seen.count(guid) > 0) return;
            seen.insert(guid);
            out.push_back(guid);
        };

        for (uint64_t guid : g_TacticalBotGUIDSet)
            add(guid);

        // Party guests (promotion.h): a bot the operator invited into their
        // group runs the tactical loop like the configured fleet, until kicked.
        for (uint64_t guid : OllamaChat::Promotion::PartyGuests())
            add(guid);

        if (!g_TacticalAutoEnrollNearbyBots || g_TacticalNearbyBotMax == 0)
            return out;

        std::vector<Player*> humans = CollectPresenceHumans();
        if (humans.empty())
            return out;

        uint32_t enrolled = 0;
        for (auto const& pair : ObjectAccessor::GetPlayers())
        {
            if (enrolled >= g_TacticalNearbyBotMax)
                break;
            Player* candidate = pair.second;
            if (!candidate || !candidate->IsInWorld() || candidate->IsBeingTeleported())
                continue;
            PlayerbotAI* ai = PlayerbotsMgr::instance().GetPlayerbotAI(candidate);
            if (!ai || !ai->IsBotAI())
                continue;

            uint64_t guid = candidate->GetGUID().GetRawValue();
            if (seen.count(guid) > 0)
                continue;

            bool nearHuman = false;
            for (Player* human : humans)
            {
                if (!human || human->GetMapId() != candidate->GetMapId())
                    continue;
                if (candidate->GetDistance(human) <= g_TacticalNearbyBotRadius)
                {
                    nearHuman = true;
                    break;
                }
            }
            if (!nearHuman)
                continue;

            add(guid);
            ++enrolled;
        }
        return out;
    }

    // Compact per-bot snapshot used as the "user prompt" for tactical
    // inference. Kept well under g_TacticalPromptMaxBytes. Includes the bot's
    // own state AND any whitelisted humans currently in the world so the
    // model has a concrete `name` to whisper / follow / ping. Without the
    // player block the local model kept picking whisper_player with empty
    // args because it had no target to reference.
    std::string BuildBotSnapshot(Player* bot)
    {
        if (!bot) return "";
        std::string s;
        s.reserve(768);
        s += "bot_name=" + std::string(bot->GetName());
        s += " class=" + std::to_string(bot->getClass());
        s += " race=" + std::to_string(bot->getRace());
        s += " level=" + std::to_string(static_cast<uint32_t>(bot->GetLevel()));
        s += " zone=" + std::to_string(bot->GetZoneId());
        s += " hp_pct=" + std::to_string(static_cast<int>(bot->GetHealthPct()));
        s += " in_combat=" + std::string(bot->IsInCombat() ? "true" : "false");
        if (Unit* tgt = bot->GetVictim())
        {
            s += " target=" + tgt->GetName();
            s += " target_hp_pct=" + std::to_string(static_cast<int>(tgt->GetHealthPct()));
        }
        Group* botGroup = bot->GetGroup();
        if (botGroup)
            s += " group_size=" + std::to_string(botGroup->GetMembersCount());
        else
            s += " group_size=0";

        // Quest turn-in signal — cheap scan of the local quest status map so
        // the model can notice "I have completed quests to hand in" without
        // calling get_player_quests / find_nearby_quest_givers every tick.
        uint32_t questsTurninReady = 0;
        for (auto const& q : bot->getQuestStatusMap())
        {
            if (q.second.Status == QUEST_STATUS_COMPLETE) ++questsTurninReady;
        }
        s += " quests_turnin_ready=" + std::to_string(questsTurninReady);

        // Whitelisted humans in-world → enrich so the model has a concrete
        // target for social actions. Cap at a few so we don't blow the budget.
        //
        // PRIORITIZE same-group humans: the acting raid-leader / whisperer is
        // almost always grouped with the bot. If we include out-of-group alts
        // (e.g. operator has a second whitelisted character idling elsewhere
        // in the world), the model narrates distance to the wrong human —
        // "feeling pretty far from Botmaster (145y)" when Botmaster is an alt
        // on an alt and Raz is right next to him. Two-pass: emit in-group
        // whitelisted humans first, then fall back to out-of-group whitelisted
        // if we haven't hit the cap. An in-group human makes the bot's social
        // picks obvious; out-of-group alts are almost always noise.
        uint32_t playersWritten = 0;
        constexpr uint32_t kMaxPlayers = 3;
        for (int pass = 0; pass < 2 && playersWritten < kMaxPlayers; ++pass)
        {
            bool preferInGroup = (pass == 0);
            for (auto const& pair : ObjectAccessor::GetPlayers())
            {
                if (playersWritten >= kMaxPlayers) break;
                Player* p = pair.second;
                // Bots, the Overlord session and fleet guids are never humans;
                // empty whitelist means "any real human counts" (matches
                // IsAnyWhitelistedHumanOnline).
                if (!IsWhitelistedHumanPlayer(p, /*anyHumanWhenNoWhitelist=*/true)) continue;

                // Pass 1: only same-group humans. Pass 2: only others.
                bool isInGroup = botGroup && p->GetGroup() == botGroup;
                if (preferInGroup != isInGroup) continue;

                std::string tag = " player" + std::to_string(playersWritten);
                s += tag + "_name=" + std::string(p->GetName());
                s += tag + "_level=" + std::to_string(static_cast<uint32_t>(p->GetLevel()));
                s += tag + "_class=" + std::to_string(p->getClass());
                s += tag + "_hp_pct=" + std::to_string(static_cast<int>(p->GetHealthPct()));
                s += tag + "_in_combat=" + std::string(p->IsInCombat() ? "true" : "false");
                s += tag + "_same_party=" + std::string(
                    (botGroup && p->GetGroup() == botGroup) ? "true" : "false");
                // Distance helps the model decide whether to follow / whisper.
                // Only meaningful if same map — otherwise skip the field.
                if (bot->GetMapId() == p->GetMapId())
                {
                    float d = bot->GetDistance(p);
                    s += tag + "_distance=" + std::to_string(static_cast<int>(d));
                }
                ++playersWritten;
            }
        } // end two-pass in-group-first loop
        s += " player_count=" + std::to_string(playersWritten);
        s += BuildAmbientWorldContext(bot);

        // Guard: truncate if a future caller accidentally pushes past budget.
        if (s.size() > g_TacticalPromptMaxBytes)
            s.resize(g_TacticalPromptMaxBytes);
        return s;
    }

    struct TacticalTickContext
    {
        uint64_t guid = 0;
        std::string botName;
        bool inCombat = false;
        bool isDead = false;
        bool hasMaster = false;      // PlayerbotAI::master online — bot_follow's precondition
        bool rezRequested = false;   // resurrection popup pending — bot_accept_resurrect_request's precondition
        bool questEnderNear = false; // an NPC in range ends a completed quest — bot_turn_in_quest's precondition
        bool vendorNear = false;     // a vendor within kQuestEnderSearchRange
        bool repairNear = false;     // an armorer (repair flag) in range
        uint32_t trainableSpells = 0;  // spells a class trainer in range can teach this bot now
        uint32_t greyItems = 0;      // sellable poor-quality items in the bags
        uint32_t bagFree = 0;
        uint32_t durabilityMinPct = 100;
        uint32_t questsTurninReady = 0;
        uint32_t questLog = 0;
        uint64_t gearSig = 0;
        uint64_t questSig = 0;
        uint64_t xp = 0;
        uint64_t money = 0;
        std::string stateSig;        // what "no progress" is measured against (see RecordActionOutcome)
        std::vector<std::string> coolingDown;  // actions in no-progress backoff for this bot
        std::string botClass;
        uint32_t botLevel = 0;
        std::string directive;   // active strategic directive text, empty when none
        std::chrono::steady_clock::time_point startTime;
        TacticalBotConfig config;
        std::string snapshot;
        std::string systemPrompt;
        std::string userPrompt;
    };

    // ---- grounding facts + no-progress backoff (2026-09-25) ------------------
    //
    // World thread only (called inside WorldTask::Run).
    nlohmann::json GatherGroundingFacts(Player* b)
    {
        nlohmann::json f = nlohmann::json::object();
        uint32_t ready = 0;
        for (auto const& q : b->getQuestStatusMap())
            if (q.second.Status == QUEST_STATUS_COMPLETE) ++ready;
        f["quests_turnin_ready"] = ready;
        // Progress evidence for bot_accept_quest (log grows) and bot_autogear
        // (equipped entries change) — not shown to the model (codex).
        f["quest_log"] = static_cast<uint32_t>(b->getQuestStatusMap().size());
        // Order-independent quest identity + the rewards a successful errand
        // moves (XP, money): back-to-back successful turn-ins are not "no
        // progress" just because the counts match (codex).
        uint64_t qsig = 0;
        for (auto const& q : b->getQuestStatusMap())
            qsig += (static_cast<uint64_t>(q.first) * 8 + q.second.Status) * 2654435761ULL;
        f["quest_sig"] = qsig;
        f["xp"] = b->GetUInt32Value(PLAYER_XP);
        f["money"] = b->GetMoney();
        uint64_t gear = 0;
        for (uint8 slot = EQUIPMENT_SLOT_START; slot < EQUIPMENT_SLOT_END; ++slot)
            if (Item* it = b->GetItemByPos(INVENTORY_SLOT_BAG_0, slot))
                gear = gear * 1000003ULL + it->GetEntry();
        f["gear_sig"] = gear;
        if (ready > 0)
        {
            if (WorldObject* e = FindQuestEnderNear(b, kQuestEnderSearchRange))
            {
                f["turnin_npc"] = e->GetName();
                f["turnin_npc_dist"] = static_cast<int>(b->GetDistance(e));
            }
        }

        f["bag_free"] = b->GetFreeInventorySpace();
        uint32_t grey = 0;
        auto countGrey = [&grey](Item* it)
        {
            if (!it) return;
            ItemTemplate const* t = it->GetTemplate();
            if (t && t->Quality == ITEM_QUALITY_POOR && t->SellPrice > 0) ++grey;
        };
        for (uint8 slot = INVENTORY_SLOT_ITEM_START; slot < INVENTORY_SLOT_ITEM_END; ++slot)
            countGrey(b->GetItemByPos(INVENTORY_SLOT_BAG_0, slot));
        for (uint8 bag = INVENTORY_SLOT_BAG_START; bag < INVENTORY_SLOT_BAG_END; ++bag)
            if (Bag* pBag = b->GetBagByPos(bag))
                for (uint32 j = 0; j < pBag->GetBagSize(); ++j)
                    countGrey(pBag->GetItemByPos(static_cast<uint8>(j)));
        f["grey_items"] = grey;

        uint32_t durMin = 100;
        for (uint8 slot = EQUIPMENT_SLOT_START; slot < EQUIPMENT_SLOT_END; ++slot)
        {
            Item* it = b->GetItemByPos(INVENTORY_SLOT_BAG_0, slot);
            if (!it) continue;
            const uint32 maxD = it->GetUInt32Value(ITEM_FIELD_MAXDURABILITY);
            if (maxD == 0) continue;
            const uint32 pct = it->GetUInt32Value(ITEM_FIELD_DURABILITY) * 100 / maxD;
            if (pct < durMin) durMin = pct;
        }
        f["durability_min_pct"] = durMin;

        std::list<Unit*> units;
        Acore::AnyUnitInObjectRangeCheck check(b, kQuestEnderSearchRange);
        Acore::UnitListSearcher<Acore::AnyUnitInObjectRangeCheck> searcher(b, units, check);
        Cell::VisitObjects(b, searcher, kQuestEnderSearchRange);
        for (Unit* u : units)
        {
            Creature* c = u ? u->ToCreature() : nullptr;
            if (!c || !c->IsAlive()) continue;
            // "near" = the engine would let the bot use it right now (the
            // playerbots sell/repair/train actions call GetNPCIfCanInteractWith
            // and never walk up) — a 30 yd flag match made impossible picks (codex).
            auto usable = [b, c](uint32 flag) { return b->GetNPCIfCanInteractWith(c->GetGUID(), flag) != nullptr; };
            if (!f.contains("vendor_near") && usable(UNIT_NPC_FLAG_VENDOR))
                f["vendor_near"] = c->GetName();
            if (!f.contains("repair_near") && usable(UNIT_NPC_FLAG_REPAIR))
                f["repair_near"] = c->GetName();
        }
        uint32_t affordable = 0;
        if (Creature* tr = FindUsableClassTrainer(b, kQuestEnderSearchRange, &affordable))
        {
            // The same trainer bot_train_spells teaches at; counts only spells
            // the bot can learn AND pay for.
            f["class_trainer_near"] = tr->GetName();
            f["trainable_spells"] = affordable;
        }
        return f;
    }

    void ApplyGroundingFacts(TacticalTickContext& ctx, const nlohmann::json& f)
    {
        auto u = [&f](const char* k, uint32_t d) { return f.contains(k) && f[k].is_number() ? f[k].get<uint32_t>() : d; };
        ctx.questsTurninReady = u("quests_turnin_ready", 0);
        ctx.questLog = u("quest_log", 0);
        auto u64 = [&f](const char* k) { return f.contains(k) && f[k].is_number() ? f[k].get<uint64_t>() : uint64_t(0); };
        ctx.gearSig = u64("gear_sig");
        ctx.questSig = u64("quest_sig");
        ctx.xp = u64("xp");
        ctx.money = u64("money");
        ctx.bagFree = u("bag_free", 0);
        ctx.greyItems = u("grey_items", 0);
        ctx.durabilityMinPct = u("durability_min_pct", 100);
        ctx.trainableSpells = u("trainable_spells", 0);
        ctx.questEnderNear = f.contains("turnin_npc");
        ctx.vendorNear = f.contains("vendor_near");
        ctx.repairNear = f.contains("repair_near");

        std::string& s = ctx.snapshot;
        if (ctx.questsTurninReady > 0)
            s += ctx.questEnderNear
                 ? " turnin_npc=" + f["turnin_npc"].get<std::string>() +
                   " turnin_npc_dist=" + std::to_string(f["turnin_npc_dist"].get<int>())
                 : std::string(" turnin_npc=none_nearby");
        s += " bag_free=" + std::to_string(ctx.bagFree);
        s += " grey_items=" + std::to_string(ctx.greyItems);
        s += " durability_min_pct=" + std::to_string(ctx.durabilityMinPct);
        if (ctx.vendorNear) s += " vendor_near=" + f["vendor_near"].get<std::string>();
        if (ctx.repairNear) s += " repair_near=" + f["repair_near"].get<std::string>();
        if (f.contains("class_trainer_near"))
            s += " class_trainer_near=" + f["class_trainer_near"].get<std::string>() +
                 " trainable_spells=" + std::to_string(ctx.trainableSpells);
    }

    // The state a self-maintenance action is meant to change. If it is the
    // same the next time the SAME action is chosen, that action did nothing.
    std::string StateSignature(const TacticalTickContext& ctx)
    {
        return fmt::format("lvl={} xp={} money={} q={} log={} qsig={} gear={} grey={} bag={} dur={} train={} dead={}",
                           ctx.botLevel, ctx.xp, ctx.money, ctx.questsTurninReady, ctx.questLog, ctx.questSig,
                           ctx.gearSig, ctx.greyItems, ctx.bagFree, ctx.durabilityMinPct, ctx.trainableSpells,
                           ctx.isDead ? 1 : 0);
    }

    // Actions whose effect shows up in StateSignature. Movement / social /
    // combat verbs are excluded: repeating them is normal.
    const std::unordered_set<std::string> kBackoffActions = {
        "bot_turn_in_quest", "bot_accept_quest", "bot_sell_junk", "bot_maintenance",
        "bot_train_spells", "bot_autogear", "bot_reset_ai",
    };
    constexpr int kNoProgressLimit = 2;
    constexpr auto kNoProgressCooldown = std::chrono::minutes(5);

    struct ActionHistory
    {
        std::string sig;      // state signature the last time THIS action was dispatched
        int noProgress = 0;
    };
    struct ActionMemo
    {
        std::unordered_map<std::string, ActionHistory> history;   // per action (codex)
        std::unordered_map<std::string, std::chrono::steady_clock::time_point> until;
    };
    std::mutex g_ActionMemoMutex;

    // Test hook for admin_tactical_force: the next N ticks of a bot dispatch a
    // fixed action instead of the model's pick, so the no-progress backoff can
    // be proven deterministically (the E2E harness's backoff scenario). It goes
    // through the same cooldown withholding + RecordActionOutcome as a real pick.
    struct ForcedAction { std::string action; int remaining = 0; };
    std::mutex g_TacticalForceMutex;
    std::unordered_map<uint64_t, ForcedAction> g_TacticalForce;

    // Last tick per bot, for admin_tactical_state (E2E harness + debugging).
    struct TacticalDebugEntry
    {
        std::string snapshot;
        std::vector<std::string> coolingDown;
        std::string lastAction;
        std::string lastResult;
        time_t at = 0;
    };
    std::mutex g_TacticalDebugMutex;
    std::unordered_map<uint64_t, TacticalDebugEntry> g_TacticalDebug;
    std::unordered_map<uint64_t, ActionMemo> g_ActionMemo;

    std::vector<std::string> ActiveCooldowns(uint64_t guid)
    {
        std::vector<std::string> out;
        const auto now = std::chrono::steady_clock::now();
        std::lock_guard<std::mutex> lock(g_ActionMemoMutex);
        auto it = g_ActionMemo.find(guid);
        if (it == g_ActionMemo.end()) return out;
        for (auto c = it->second.until.begin(); c != it->second.until.end();)
        {
            if (c->second <= now) c = it->second.until.erase(c);
            else { out.push_back(c->first); ++c; }
        }
        std::sort(out.begin(), out.end());
        return out;
    }

    // Called after every dispatch. An error, or choosing the same action again
    // while its StateSignature is unchanged, counts as no progress; at
    // kNoProgressLimit the action is withheld for kNoProgressCooldown. This is
    // what would have stopped the Mordo loop on the third tick whatever the
    // root cause, and it catches the next loop of this kind.
    void RecordActionOutcome(uint64_t guid, const std::string& botName, const std::string& action,
                             const std::string& sig, bool ok)
    {
        if (!kBackoffActions.count(action)) return;
        std::lock_guard<std::mutex> lock(g_ActionMemoMutex);
        ActionMemo& memo = g_ActionMemo[guid];
        ActionHistory& h = memo.history[action];
        const bool unchanged = !h.sig.empty() && h.sig == sig;
        h.noProgress = (!ok || unchanged) ? h.noProgress + 1 : 0;
        h.sig = sig;
        if (h.noProgress >= kNoProgressLimit)
        {
            memo.until[action] = std::chrono::steady_clock::now() + kNoProgressCooldown;
            memo.history.erase(action);
            LOG_INFO("server.loading",
                     "[Ollama Chat Tactical] bot={} '{}' action='{}' made no progress {}x — withheld for {}s",
                     guid, botName, action, kNoProgressLimit,
                     std::chrono::duration_cast<std::chrono::seconds>(kNoProgressCooldown).count());
        }
    }

    // ---- jev tier for the tactical executor (PR 2) --------------------------
    //
    // The article's funnel: RULES cut the obvious, jev picks among what is left.
    // Per tick the option set is the static jev-eligible list (below) pruned by
    // the live gates the dispatcher would apply anyway (combat block, HITL
    // confirmation, ambient-in-combat, dead/alive), plus `other`. A confident
    // pick is dispatched with args the speculative answers fill; `other`, low
    // confidence, or any failure hands the tick to the LlmWire tier with the
    // remaining budget — exactly today's behaviour. Free-text actions
    // (bot_say, whisper_player), targeted ones (bot_cast, bot_move_to, rez a
    // party member), squad orders and every arg-taking verb are deliberately
    // NOT eligible: jev cannot fill their args, and a "successful classification
    // followed by a failed dispatch" is the regression both plan reviewers named.

    // Candidate set: every member must be in kTacticalAllowedActions AND have a
    // registry `required` of exactly {botGuid} — except the two whose single
    // extra arg a speculative answer fills. Validated at first use; anything
    // that fails validation is logged and dropped, never dispatched.
    // Deliberately absent: bot_stop_combat (not in kTacticalAllowedActions — the
    // engine owns combat exit), bot_self_reincarnate (needs an Ankh / a
    // pre-applied Soulstone the snapshot cannot see; the LLM tier keeps it).
    const std::vector<std::string> kJevTacticalCandidates = {
        "tactical_idle",
        "bot_emote",                 // emote  <- `emote` answer
        "bot_set_loot_filter",       // mode   <- `loot_filter` answer
        "bot_follow", "bot_stay", "bot_flee",
        "bot_revive", "bot_release_spirit", "bot_accept_resurrect_request",
        "bot_turn_in_quest", "bot_accept_quest",
        "bot_maintenance", "bot_train_spells", "bot_sell_junk", "bot_autogear", "bot_roll",
        "bot_reset_ai",
    };

    const std::vector<std::string>& JevTacticalEligible()
    {
        static const std::vector<std::string> eligible = []
        {
            std::vector<std::string> out;
            const auto& reg = GetGatewayToolRegistry();
            for (const std::string& name : kJevTacticalCandidates)
            {
                if (name == "tactical_idle") { out.push_back(name); continue; }   // handled inline by the dispatcher
                if (!kTacticalAllowedActions.count(name))
                {
                    LOG_WARN("server.loading", "[Ollama Chat Tactical] jev candidate '{}' is not tactical-allowed — dropped", name);
                    continue;
                }
                auto it = reg.find(name);
                if (it == reg.end())
                {
                    LOG_WARN("server.loading", "[Ollama Chat Tactical] jev candidate '{}' has no registry entry — dropped", name);
                    continue;
                }
                const char* fillable = name == "bot_emote" ? "emote"
                                     : name == "bot_set_loot_filter" ? "mode" : nullptr;
                bool ok = true;
                const nlohmann::json& schema = it->second.parametersSchema;
                if (schema.is_object() && schema.contains("required") && schema["required"].is_array())
                {
                    for (const auto& req : schema["required"])
                    {
                        const std::string key = req.is_string() ? req.get<std::string>() : std::string("?");
                        if (key == "botGuid") continue;
                        if (fillable && key == fillable) continue;
                        ok = false;
                        LOG_WARN("server.loading",
                                 "[Ollama Chat Tactical] jev candidate '{}' requires '{}' which no answer can fill — dropped",
                                 name, key);
                        break;
                    }
                }
                if (ok) out.push_back(name);
            }
            LOG_INFO("server.loading", "[Ollama Chat Tactical] jev eligible set: {} of {} candidates", out.size(), kJevTacticalCandidates.size());
            return out;
        }();
        return eligible;
    }

    // Master switch, then the per-bot canary, then the site flag.
    bool JevTacticalEnabledFor(const TacticalBotConfig& cfg)
    {
        if (!Jev::Enabled()) return false;
        if (cfg.jev == 0) return false;
        if (cfg.jev == 1) return true;
        return Jev::EnabledFor(Jev::kSiteTactical);
    }

    // Runs on the per-bot async thread (no world access — everything it needs
    // was captured in ctx by PrepareBotTick). Returns an action object of the
    // same shape the LLM tier returns, with two audit-only keys FinishBotTick
    // strips: `_backend` and `_prompt_tokens`. Null = fall through.
    nlohmann::json JevTacticalDecide(const TacticalTickContext& ctx, uint32_t capMs, uint32_t& spentMs)
    {
        spentMs = 0;
        const std::vector<std::string>& eligible = JevTacticalEligible();
        if (eligible.empty() || capMs == 0) return nlohmann::json{};

        std::vector<std::string> combatBlocked(kCombatBlockedActions.begin(), kCombatBlockedActions.end());
        std::vector<std::string> confirm(g_TacticalRequireConfirmationSet.begin(), g_TacticalRequireConfirmationSet.end());
        Jev::TacticalTickFacts facts;
        facts.inCombat = ctx.inCombat;
        facts.botIsDead = ctx.isDead;
        facts.hasMaster = ctx.hasMaster;
        facts.rezRequested = ctx.rezRequested;
        facts.questEnderNear = ctx.questEnderNear;
        facts.vendorNear = ctx.vendorNear;
        facts.repairNear = ctx.repairNear;
        facts.trainableSpells = ctx.trainableSpells;
        facts.greyItems = ctx.greyItems;
        facts.bagFree = ctx.bagFree;
        facts.durabilityMinPct = ctx.durabilityMinPct;
        facts.coolingDown = ctx.coolingDown;
        facts.ambientEnabled = g_TacticalAmbientEnable;
        facts.actionToolsAllowed = g_McpAllowActionTools;
        facts.allowCombatOverride = g_TacticalAllowCombatOverride;
        facts.repliesDisabledInCombat = g_DisableRepliesInCombat;
        std::vector<std::string> options = Jev::PruneTacticalOptions(eligible, facts, combatBlocked, confirm);
        if (options.size() <= 1) return nlohmann::json{};   // only idle survives — not worth a request; LLM tier decides
        options.push_back("other");

        nlohmann::json state{
            {"snapshot",  ctx.snapshot},        // key=value text, exactly what the LLM tier reads
            {"botName",   ctx.botName},
            {"botClass",  ctx.botClass},
            {"botLevel",  ctx.botLevel},
            {"inCombat",  ctx.inCombat},
            {"botIsDead", ctx.isDead},
        };
        if (!ctx.directive.empty()) state["directive"] = ctx.directive;

        const nlohmann::json site = Jev::QuestionTemplate(Jev::kSiteTactical);
        auto tmpl = [&](const char* id) -> nlohmann::json {
            return site.is_object() && site.contains(id) ? site[id] : nlohmann::json(nullptr);
        };
        static const std::vector<std::string> kEmotes(kSafeTacticalEmotes.begin(), kSafeTacticalEmotes.end());
        static const std::vector<std::string> kLootModes{"all", "normal", "gray", "disenchant"};
        nlohmann::json escalateTmpl = tmpl("escalate");
        nlohmann::json escalateQ = escalateTmpl.is_object()
            ? Jev::Noul(escalateTmpl.value("instructions", nlohmann::json("Does this need the strategic layer?")),
                        escalateTmpl.contains("criteria") && escalateTmpl["criteria"].is_object() ? escalateTmpl["criteria"].value("true", "") : "",
                        escalateTmpl.contains("criteria") && escalateTmpl["criteria"].is_object() ? escalateTmpl["criteria"].value("false", "") : "")
            : Jev::Noul("Does this situation need the expensive strategic layer rather than a local action?");

        nlohmann::json questions{
            {"action",      Jev::ChoiceFromTemplate(tmpl("action"),      options)},
            {"escalate",    escalateQ},
            {"emote",       Jev::ChoiceFromTemplate(tmpl("emote"),       kEmotes)},
            {"loot_filter", Jev::ChoiceFromTemplate(tmpl("loot_filter"), kLootModes)},
        };

        Jev::Result r = Jev::Decide(state, questions, capMs, Jev::kSiteTactical);
        spentMs = r.latencyMs;
        if (!r.ok) return nlohmann::json{};   // logged by Decide; LLM tier next

        const Jev::Answer& act = *r.Find("action");
        const float minConf = r.minConfidence;
        if (act.choice == "other" || act.confidence < minConf)
        {
            LOG_DEBUG("server.loading", "[Ollama Chat Tactical] jev bot={} {} conf={:.2f} (min {:.2f}) — LLM tier",
                      ctx.guid, act.choice, act.confidence, minConf);
            return nlohmann::json{};
        }

        nlohmann::json action{{"action", act.choice}, {"args", nlohmann::json{{"botGuid", ctx.guid}}},
                              {"_backend", Jev::kBackendJev}, {"_prompt_tokens", r.inputTokens}};
        if (act.choice == "bot_emote")
        {
            const Jev::Answer* e = r.Find("emote");
            if (!e || e->confidence < minConf) return nlohmann::json{};   // no confident token — LLM tier
            action["args"]["emote"] = e->choice;
        }
        else if (act.choice == "bot_set_loot_filter")
        {
            const Jev::Answer* m = r.Find("loot_filter");
            if (!m || m->confidence < minConf) return nlohmann::json{};
            action["args"]["mode"] = m->choice;
        }

        // Escalation is its own calibrated gate, and never together with an
        // emote (ollama_tactical.md: "Never pair bot_emote with escalate").
        const Jev::Answer* esc = r.Find("escalate");
        if (esc && esc->noul >= g_JevTacticalEscalateMin && act.choice != "bot_emote")
            action["escalate"] = fmt::format("jev escalate p={:.2f}", esc->noul);
        return action;
    }

    struct InFlightTacticalTick
    {
        std::unique_ptr<TacticalTickContext> context;
        std::future<nlohmann::json> future;

        InFlightTacticalTick(std::unique_ptr<TacticalTickContext> context_,
                             std::future<nlohmann::json> future_)
            : context(std::move(context_)), future(std::move(future_)) {}
    };

    // Prepares one tactical decision for a single bot. This keeps world/bot
    // snapshot reads on the tactical loop thread; only the Ollama HTTP call is
    // fanned out in parallel.
    std::unique_ptr<TacticalTickContext> PrepareBotTick(uint64_t guid)
    {
        Player* bot = ObjectAccessor::FindPlayer(ObjectGuid(guid));
        if (!bot || !bot->IsInWorld()) return nullptr;
        if (!PlayerbotsMgr::instance().GetPlayerbotAI(bot)) return nullptr;

        if (!TryClaimBotCooldown(guid))
        {
            LOG_DEBUG("server.loading", "[Ollama Chat Tactical] bot={} '{}' cooling down", guid, bot->GetName());
            return nullptr;
        }

        auto ctx = std::make_unique<TacticalTickContext>();
        ctx->guid = guid;
        ctx->startTime = std::chrono::steady_clock::now();
        ctx->botName = bot->GetName();
        ctx->inCombat = bot->IsInCombat();
        ctx->isDead = !bot->IsAlive();
        // Dispatch preconditions the jev prune needs (mirrors Tool_BotFollow's
        // master check and Tool_BotAcceptResurrectRequest's popup check).
        if (PlayerbotAI* ai = PlayerbotsMgr::instance().GetPlayerbotAI(bot))
        {
            Player* master = ai->GetMaster();
            ctx->hasMaster = master && master->IsInWorld();
        }
        ctx->rezRequested = ctx->isDead && bot->isResurrectRequested();
        ctx->config = ResolveTacticalBotConfig(guid);

        TacticalDirective directive = ReadDirective(guid);

        std::string botClass = std::to_string(bot->getClass());
        uint32_t botLevel = bot->GetLevel();
        ctx->botClass = botClass;
        ctx->botLevel = botLevel;
        ctx->snapshot = BuildBotSnapshot(bot);
        // Grounding facts the tactical rules assume but the snapshot never
        // carried: who ends a completed quest (Raz read Undertaker Mordo as the
        // turn-in NPC and picked bot_turn_in_quest every tick, 2026-09-25), bag
        // space, junk, durability, and which service NPCs stand nearby. Grid
        // walks run on the world thread (this loop has its own); only copied
        // values come back.
        if (!ctx->isDead)
        {
            nlohmann::json f = OllamaChat::WorldTask::Run([guid]() -> nlohmann::json
            {
                Player* b = ObjectAccessor::FindPlayer(ObjectGuid(guid));
                if (!b || !b->IsInWorld()) return nlohmann::json{{"error", "gone"}};
                return GatherGroundingFacts(b);
            }, 500);
            if (!f.contains("error"))
                ApplyGroundingFacts(*ctx, f);
        }
        ctx->stateSig = StateSignature(*ctx);
        ctx->coolingDown = ActiveCooldowns(guid);
        if (!ctx->coolingDown.empty())
        {
            std::string list;
            for (const auto& a : ctx->coolingDown) list += (list.empty() ? "" : ",") + a;
            ctx->snapshot += " cooling_down=" + list;
        }
        ctx->systemPrompt = BuildTacticalSystemPrompt(ctx->botName, botClass, botLevel, ctx->inCombat);
        ctx->userPrompt = "Current state: " + ctx->snapshot;
        {
            std::lock_guard<std::mutex> lock(g_TacticalDebugMutex);
            TacticalDebugEntry& d = g_TacticalDebug[guid];
            d.snapshot = ctx->snapshot;
            d.coolingDown = ctx->coolingDown;
            d.at = time(nullptr);
        }
        if (directive.IsActive(time(nullptr)))
        {
            ctx->directive = directive.goal;
            ctx->userPrompt += "\nActive directive (from strategic layer, stay aligned): "
                             +  directive.goal;
        }
        ctx->userPrompt += "\nChoose a SINGLE action that fits the situation. "
                           "For social actions (whisper_player, bot_follow targeted, etc.), use the `playerN_name` from the snapshot as the `name` argument — do NOT invent names and do NOT leave them blank. "
                           "Use tactical_idle when state is routine, unchanged, or no useful action is needed. Do not use emotes as the default idle action. "
                           "For bot_emote, use exactly one lowercase token from: wave, nod, cheer, laugh, shrug, dance, bow, salute, clap, point, smile, sigh, thank, grats, train, roar, flex, kneel, sleep, confused. No spaces, punctuation, slash commands, unicode emoji, or prose. "
                           "For bot_say, use args.text with a short in-character line only when nearby context or recent_event_* makes it worthwhile. "
                           "If player_count=0 there is no human to whisper; pick tactical_idle or a quiet read tool instead. "
                           "Avoid combat-tactics actions while in combat — let the engine do its job. "
                           "Set \"escalate\" when you face a novel or high-stakes situation that needs the paid strategic layer.";

        return ctx;
    }

    // Finishes a prepared tactical decision after Ollama returns. Dispatch stays
    // serial in the tactical loop, while inference may run concurrently by URL.
    bool FinishBotTick(const TacticalTickContext& ctx, const nlohmann::json& action)
    {
        if (action.is_null() || action.empty())
        {
            // Circuit breaker may be open, or parse failed. Inference logs at
            // DEBUG / state-transition INFO on its own — we don't re-log here.
            // Still record an audit row so operators can see the silence.
            auto   tEnd      = std::chrono::steady_clock::now();
            uint32_t latency = static_cast<uint32_t>(
                std::chrono::duration_cast<std::chrono::milliseconds>(tEnd - ctx.startTime).count());
            WriteAuditRow(ctx.guid, ctx.botName, /*action=*/"", "error", "inference_failed",
                          false, /*escReason=*/"", latency, ctx.inCombat);
            return false;
        }

        // Which tier decided this tick (the jev tier tags its actions; the LLM
        // tier's are untagged). Audit-only keys — stripped before dispatch.
        const char* backend = Jev::kBackendLlm;
        uint32_t promptTokens = 0;
        nlohmann::json actionForDispatch = action;
        if (action.contains("_backend") && action["_backend"].is_string() && action["_backend"] == Jev::kBackendJev)
            backend = Jev::kBackendJev;
        if (action.contains("_prompt_tokens") && action["_prompt_tokens"].is_number())
            promptTokens = action["_prompt_tokens"].get<uint32_t>();
        actionForDispatch.erase("_backend");
        actionForDispatch.erase("_prompt_tokens");
        std::string forcedName;
        {
            std::lock_guard<std::mutex> lock(g_TacticalForceMutex);
            auto f = g_TacticalForce.find(ctx.guid);
            if (f != g_TacticalForce.end() && f->second.remaining > 0 && action.contains("action"))
            {
                LOG_INFO("server.loading", "[Ollama Chat Tactical] bot={} '{}' forced action='{}' ({} left) — test hook",
                         ctx.guid, ctx.botName, f->second.action, f->second.remaining - 1);
                actionForDispatch = nlohmann::json{{"action", f->second.action}};
                forcedName = f->second.action;
                if (--f->second.remaining == 0) g_TacticalForce.erase(f);
            }
        }

        // A tagged object WITHOUT an action is the jev tier reporting that it
        // missed and no budget was left for the LLM tier: audit it under the
        // tier that actually ran, and do not pretend an LLM request happened.
        if (!action.contains("action"))
        {
            auto   tEnd      = std::chrono::steady_clock::now();
            uint32_t latency = static_cast<uint32_t>(
                std::chrono::duration_cast<std::chrono::milliseconds>(tEnd - ctx.startTime).count());
            std::string why = action.value("_failed", std::string("inference_failed"));
            WriteAuditRow(ctx.guid, ctx.botName, /*action=*/"", "error", why,
                          false, /*escReason=*/"", latency, ctx.inCombat, backend, promptTokens);
            return false;
        }

        // Fire-and-forget escalation if the model requested one. Tactical
        // continues with the current directive while the strategic worker
        // handles it — no gameplay stall.
        bool        escalated      = false;
        std::string escalationReason;
        if (g_TacticalAllowEscalation
            && action.contains("escalate") && action["escalate"].is_string())
        {
            escalationReason = action["escalate"].get<std::string>();
            if (!escalationReason.empty())
            {
                escalated = true;
                EnqueueEscalationImpl(ctx.guid, escalationReason, ctx.snapshot);
            }
        }

        std::string actionName = action.contains("action") && action["action"].is_string()
                                 ? action["action"].get<std::string>() : std::string("<unknown>");
        if (!forcedName.empty())
            actionName = forcedName;   // cooldown check + audit use the action actually dispatched (codex)

        // Re-resolve the Player* — the bot may have logged out, changed map,
        // or otherwise unloaded during the Ollama round-trip. The `bot`
        // pointer captured above is not safe to dereference after a network
        // call that can take seconds. If the bot is gone, skip the dispatch
        // (write an audit row so we can see why it went quiet).
        Player* botNow = ObjectAccessor::FindPlayer(ObjectGuid(ctx.guid));
        if (!botNow || !botNow->IsInWorld())
        {
            auto   tEnd      = std::chrono::steady_clock::now();
            uint32_t latency = static_cast<uint32_t>(
                std::chrono::duration_cast<std::chrono::milliseconds>(tEnd - ctx.startTime).count());
            WriteAuditRow(ctx.guid, ctx.botName, actionName, "error", "bot_unloaded_during_inference",
                          escalated, escalationReason, latency, ctx.inCombat, backend, promptTokens);
            return false;
        }

        // An action in no-progress backoff is not dispatched even when the LLM
        // tier picks it (jev never sees it — the prune drops it).
        // Live check, not ctx.coolingDown: the tick's snapshot can predate a
        // cooldown reset or a new cooldown (codex).
        const std::vector<std::string> coolingNow = ActiveCooldowns(ctx.guid);
        if (std::find(coolingNow.begin(), coolingNow.end(), actionName) != coolingNow.end())
        {
            LOG_INFO("server.loading", "[Ollama Chat Tactical] bot={} '{}' withheld '{}' (no-progress backoff)",
                      ctx.guid, ctx.botName, actionName);
            actionForDispatch = nlohmann::json{{"action", "tactical_idle"}};
        }
        nlohmann::json result = DispatchTacticalAction(botNow, ctx.guid, actionForDispatch);
        {
            const std::string dispatched = actionForDispatch.value("action", std::string{});
            // An explicit ok:false (e.g. the turn-in action refused) is a failure too.
            const bool ok = result.is_object() && !result.contains("error") &&
                            !(result.contains("ok") && result["ok"].is_boolean() && !result["ok"].get<bool>());
            RecordActionOutcome(ctx.guid, ctx.botName, dispatched, ctx.stateSig, ok);
            std::lock_guard<std::mutex> lock(g_TacticalDebugMutex);
            TacticalDebugEntry& d = g_TacticalDebug[ctx.guid];
            d.lastAction = dispatched;
            d.lastResult = ok ? "ok" : "error";
        }
        if (result.is_object() && result.contains("audit_action") && result["audit_action"].is_string())
            actionName = result["audit_action"].get<std::string>();

        // Compact INFO summary — AC's log pipeline chokes on JSON braces even
        // through fmt::format wrappers, so we elide the raw JSON at INFO level
        // and emit it at DEBUG (where the warning is acceptable for debugging).
        std::string resultKind;
        std::string errorMsg;
        if (result.is_object() && result.contains("error") && result["error"].is_string())
        {
            errorMsg = result["error"].get<std::string>();
            // Distinguish "blocked" (our own whitelist / combat-block) from
            // a tool-internal error so audit can tell them apart at a glance.
            if (errorMsg.find("blocked during combat") != std::string::npos
                || errorMsg.find("not in tactical whitelist") != std::string::npos)
                resultKind = "blocked";
            else
                resultKind = "error";
        }
        else if (result.is_object())
        {
            resultKind = "ok";
        }
        else
        {
            resultKind = "error";
            errorMsg = "no-result";
        }

        std::string summary = resultKind;
        if (!errorMsg.empty()) summary += ": " + errorMsg;

        LOG_INFO("server.loading",
                 "[Ollama Chat Tactical] tick bot={} '{}' action='{}' {} backend={}",
                 ctx.guid, ctx.botName, actionName, summary, backend);

        if (g_DebugEnabled)
        {
            std::string resultStr = result.is_discarded() ? std::string("<discarded>") : result.dump();
            if (resultStr.size() > 400) resultStr = resultStr.substr(0, 400) + "...";
            LOG_DEBUG("server.loading",
                      "[Ollama Chat Tactical] tick bot={} result-raw: {}",
                      ctx.guid, resultStr);
        }

        auto   tEnd      = std::chrono::steady_clock::now();
        uint32_t latency = static_cast<uint32_t>(
            std::chrono::duration_cast<std::chrono::milliseconds>(tEnd - ctx.startTime).count());
        WriteAuditRow(ctx.guid, ctx.botName, actionName, resultKind, errorMsg,
                      escalated, escalationReason, latency, ctx.inCombat, backend, promptTokens);
        return true;
    }

    // Iterate all tactical-enabled bots, applying the global presence gate.
    // Returns the number of bots successfully ticked this pass.
    uint32_t TickAllBots()
    {
        if (!IsAnyWhitelistedHumanOnline())
        {
            LOG_DEBUG("server.loading", "[Ollama Chat Tactical] dormant — no whitelisted human online");
            return 0;
        }
        std::vector<uint64_t> botGuids = CollectTacticalBotGuids();
        if (botGuids.empty())
        {
            LOG_DEBUG("server.loading", "[Ollama Chat Tactical] no tactical or nearby auto-enrolled bots available");
            return 0;
        }
        uint32_t ticked = 0;
        std::vector<InFlightTacticalTick> pending;
        pending.reserve(botGuids.size());
        for (uint64_t guid : botGuids)
        {
            std::unique_ptr<TacticalTickContext> ctx = PrepareBotTick(guid);
            if (!ctx)
                continue;

            // The async lambda gets a full COPY of the context (it must not
            // touch the world, and `ctx` moves into `pending` below). One
            // absolute deadline per bot tick = the bot's timeoutMs; jev takes
            // min(Jev.TimeoutMs, 1500, remaining), the LLM tier gets what is left
            // — and only if that is still a useful budget.
            TacticalTickContext snapshotCtx = *ctx;
            std::future<nlohmann::json> future = std::async(
                std::launch::async,
                [snapshotCtx]
                {
                    const uint32_t budgetMs = std::max<uint32_t>(1000u, snapshotCtx.config.timeoutMs);
                    const auto t0 = std::chrono::steady_clock::now();
                    auto remainingMs = [&]() -> int64_t {
                        return static_cast<int64_t>(budgetMs) - std::chrono::duration_cast<std::chrono::milliseconds>(
                            std::chrono::steady_clock::now() - t0).count();
                    };
                    if (JevTacticalEnabledFor(snapshotCtx.config))
                    {
                        // The whole jev attempt is exception-safe: a malformed
                        // questions-file override must degrade to the LLM tier,
                        // not to the outer inference_exception path that skips it.
                        try
                        {
                            const uint32_t configured = g_JevTimeoutMs == 0 ? 1500u : std::min<uint32_t>(g_JevTimeoutMs, 1500u);
                            const uint32_t cap = Jev::JevCapMs(configured, remainingMs());
                            uint32_t spent = 0;
                            nlohmann::json decided = JevTacticalDecide(snapshotCtx, cap, spent);
                            if (!decided.is_null() && !decided.empty()) return decided;
                        }
                        catch (const std::exception& e)
                        {
                            LOG_WARN("server.loading", "[Ollama Chat Tactical] bot={} jev tier threw: {} — LLM tier", snapshotCtx.guid, e.what());
                        }
                        catch (...)
                        {
                            LOG_WARN("server.loading", "[Ollama Chat Tactical] bot={} jev tier threw (unknown) — LLM tier", snapshotCtx.guid);
                        }
                        if (!Jev::LlmTierFits(remainingMs(), 1500))
                        {
                            LOG_DEBUG("server.loading", "[Ollama Chat Tactical] bot={} jev miss and {}ms left — skipping LLM tier this tick",
                                      snapshotCtx.guid, remainingMs());
                            // Tagged, action-less: FinishBotTick audits it as a jev-tier miss.
                            return nlohmann::json{{"_backend", Jev::kBackendJev}, {"_failed", "jev_miss_no_budget"}};
                        }
                    }
                    // Absolute deadline: slot acquisition + connect + read all
                    // inside what remains, never the bot's full timeout again.
                    return TacticalInference::Instance().Query(snapshotCtx.config, snapshotCtx.systemPrompt, snapshotCtx.userPrompt,
                                                               t0 + std::chrono::milliseconds(budgetMs));
                });
            pending.emplace_back(std::move(ctx), std::move(future));
        }
        for (auto& item : pending)
        {
            try
            {
                nlohmann::json action = item.future.get();
                if (FinishBotTick(*item.context, action)) ++ticked;
            }
            catch (const std::exception& e)
            {
                LOG_WARN("server.loading", "[Ollama Chat Tactical] bot={} inference future threw: {}",
                         item.context->guid, e.what());
                auto   tEnd      = std::chrono::steady_clock::now();
                uint32_t latency = static_cast<uint32_t>(
                    std::chrono::duration_cast<std::chrono::milliseconds>(tEnd - item.context->startTime).count());
                WriteAuditRow(item.context->guid, item.context->botName, /*action=*/"", "error",
                              "inference_exception", false, /*escReason=*/"", latency, item.context->inCombat);
            }
            catch (...)
            {
                LOG_WARN("server.loading", "[Ollama Chat Tactical] bot={} inference future threw (unknown)",
                         item.context->guid);
                auto   tEnd      = std::chrono::steady_clock::now();
                uint32_t latency = static_cast<uint32_t>(
                    std::chrono::duration_cast<std::chrono::milliseconds>(tEnd - item.context->startTime).count());
                WriteAuditRow(item.context->guid, item.context->botName, /*action=*/"", "error",
                              "inference_exception", false, /*escReason=*/"", latency, item.context->inCombat);
            }
        }
        return ticked;
    }

    // ---- Phase 4: Directive storage + Strategic escalation queue ----

    // Per-bot directive map. Guarded by s_directiveMutex.
    std::mutex s_directiveMutex;
    std::unordered_map<uint64_t, TacticalDirective> s_directives;

    TacticalDirective ReadDirective(uint64_t botGuid)
    {
        time_t now = time(nullptr);
        std::lock_guard<std::mutex> lock(s_directiveMutex);
        auto it = s_directives.find(botGuid);
        if (it == s_directives.end()) return TacticalDirective{};
        if (!it->second.IsActive(now))
        {
            s_directives.erase(it);
            return TacticalDirective{};
        }
        return it->second;
    }

    void WriteDirective(uint64_t botGuid, const std::string& goal, uint32_t ttlSec, const std::string& setBy)
    {
        time_t now = time(nullptr);
        std::lock_guard<std::mutex> lock(s_directiveMutex);
        if (goal.empty())
        {
            s_directives.erase(botGuid);
            LOG_INFO("server.loading", "[Ollama Chat Tactical] directive cleared for bot={} (by {})", botGuid, setBy);
            return;
        }
        uint32_t clampedTtl = std::min<uint32_t>(ttlSec, g_TacticalDirectiveMaxTtlSec);
        TacticalDirective& d = s_directives[botGuid];
        d.goal      = goal;
        d.setAt     = now;
        d.expiresAt = now + clampedTtl;
        d.setBy     = setBy;
        LOG_INFO("server.loading",
                 "[Ollama Chat Tactical] directive set for bot={} ttl={}s by={}: \"{}\"",
                 botGuid, clampedTtl, setBy, goal);
    }

    // Escalation queue — condvar-signalled, bounded, drop-oldest on overflow.
    struct PendingEscalation
    {
        uint64_t    botGuid;
        std::string reason;
        std::string snapshot;
        time_t      queuedAt;
    };

    std::mutex                     s_queueMutex;
    std::condition_variable        s_queueCv;
    std::deque<PendingEscalation>  s_queue;
    std::atomic<bool>              s_queueWake{false};  // set true when Stop() wants worker to wake

    // Set while the worker thread is inside its gateway call. The gateway
    // tool_call loop runs tools synchronously on the calling thread, so any
    // tool dispatched during that window (tactical_set_directive in
    // particular) can identify itself as escalation-originated via
    // InEscalationContext(). thread_local: the flag never leaks to the MCP
    // worker threads serving the operator's agent.
    thread_local bool t_inEscalationContext = false;

    struct EscalationContextGuard
    {
        EscalationContextGuard()  { t_inEscalationContext = true; }
        ~EscalationContextGuard() { t_inEscalationContext = false; }
    };

    // Per-bot budget for paid escalation calls. Mirrors leader-tick's hour window.
    std::mutex                                    s_escBudgetMutex;
    std::unordered_map<uint64_t, std::vector<time_t>>  s_escBudgetLog;
    std::unordered_map<uint64_t, time_t>               s_escLastFireAt;

    bool ClaimEscalationBudget(uint64_t botGuid)
    {
        time_t now = time(nullptr);
        std::lock_guard<std::mutex> lock(s_escBudgetMutex);

        // Per-bot cooldown
        uint32_t cdSec = std::max<uint32_t>(1u, g_StrategicEscalationCooldownSec);
        auto lastIt = s_escLastFireAt.find(botGuid);
        if (lastIt != s_escLastFireAt.end() && (now - lastIt->second) < cdSec)
        {
            LOG_DEBUG("server.loading",
                      "[Ollama Chat Strategic Escalation] bot={} within cooldown ({}s) — dropping",
                      botGuid, cdSec);
            return false;
        }

        // Per-bot hour cap
        uint32_t cap = g_StrategicMaxEscalationsPerBotPerHour;
        if (cap > 0)
        {
            auto& bucket = s_escBudgetLog[botGuid];
            bucket.erase(std::remove_if(bucket.begin(), bucket.end(),
                                        [now](time_t t){ return now - t > 3600; }),
                         bucket.end());
            if (bucket.size() >= cap)
            {
                LOG_INFO("server.loading",
                         "[Ollama Chat Strategic Escalation] bot={} hourly cap reached ({}/hr) — dropping",
                         botGuid, cap);
                return false;
            }
            bucket.push_back(now);
        }
        s_escLastFireAt[botGuid] = now;
        return true;
    }

    // Per-bot dedup: (lastReasonPrefix, lastEnqueueTime). If the same reason
    // prefix is re-enqueued within kEscalationDedupSec, drop silently — this
    // prevents the "every-tick-same-escalation" pattern that burns Synthiq
    // tokens when the model latches onto one reason (e.g. "need human
    // confirmation to invite player") and repeats it 10 times in 2 minutes.
    // Complements the existing hour cap + cooldown at budget-claim time.
    constexpr time_t kEscalationDedupSec = 60;
    constexpr size_t kEscalationReasonPrefixLen = 40;
    std::unordered_map<uint64_t, std::pair<std::string, time_t>> s_lastEnqueueByBot;
    std::mutex s_lastEnqueueMutex;

    void EnqueueEscalationImpl(uint64_t botGuid, const std::string& reason, const std::string& snapshot)
    {
        // Dedup window — drop near-duplicate re-enqueues before we claim budget.
        {
            std::string prefix = reason.substr(0, std::min(reason.size(), kEscalationReasonPrefixLen));
            time_t now = time(nullptr);
            std::lock_guard<std::mutex> lock(s_lastEnqueueMutex);
            auto it = s_lastEnqueueByBot.find(botGuid);
            if (it != s_lastEnqueueByBot.end()
                && (now - it->second.second) < kEscalationDedupSec
                && it->second.first == prefix)
            {
                LOG_DEBUG("server.loading",
                          "[Ollama Chat Strategic Escalation] bot={} dedup-drop reason=\"{}\" (same prefix within {}s)",
                          botGuid, reason, kEscalationDedupSec);
                return;
            }
            s_lastEnqueueByBot[botGuid] = {prefix, now};
        }

        {
            std::lock_guard<std::mutex> lock(s_queueMutex);
            uint32_t maxSize = std::max<uint32_t>(1u, g_StrategicQueueMaxSize);
            while (s_queue.size() >= maxSize)
            {
                LOG_DEBUG("server.loading",
                          "[Ollama Chat Strategic Escalation] queue full ({}), dropping oldest",
                          maxSize);
                s_queue.pop_front();
            }
            s_queue.push_back(PendingEscalation{
                botGuid,
                reason,
                snapshot,
                time(nullptr),
            });
            LOG_INFO("server.loading",
                     "[Ollama Chat Strategic Escalation] enqueued bot={} reason=\"{}\" (queue depth={})",
                     botGuid, reason, s_queue.size());
        }
        s_queueCv.notify_one();
    }

    void WakeEscalationWorker()
    {
        s_queueWake.store(true, std::memory_order_release);
        s_queueCv.notify_all();
    }

    // ---- Audit (Phase 5) ----
    //
    // Writes one row per tactical tick when EnableAudit=1. Async insert via
    // CharacterDatabase.Execute so the heartbeat never blocks on DB. All
    // string fields that might contain model-authored text are EscapeString'd
    // and length-clamped.

    std::string ClampAndEscape(std::string s, size_t maxLen)
    {
        if (s.size() > maxLen) s.resize(maxLen);
        CharacterDatabase.EscapeString(s);
        return s;
    }

    void WriteAuditRow(uint64_t botGuid,
                       const std::string& botName,
                       const std::string& action,
                       const std::string& resultKind,   // "ok" | "error" | "blocked"
                       const std::string& errorMsg,
                       bool escalated,
                       const std::string& escalationReason,
                       uint32_t latencyMs,
                       bool inCombat,
                       const char* backend,
                       uint32_t promptTokens)
    {
        if (!g_TacticalEnableAudit) return;

        std::string name   = ClampAndEscape(botName,           32);
        std::string act    = ClampAndEscape(action,            64);
        std::string result = ClampAndEscape(resultKind,        16);
        std::string err    = ClampAndEscape(errorMsg,         255);
        std::string esRsn  = ClampAndEscape(escalationReason, 255);

        // `backend` / `prompt_tokens` exist only after Jev::EnsureAuditBackendColumns
        // confirmed the self-migration; until then keep the legacy column list so
        // an unmigrated DB never loses rows.
        if (Jev::AuditColumnsReady())
        {
            CharacterDatabase.Execute(
                "INSERT INTO mod_ollama_chat_tactical_audit "
                "(bot_guid, bot_name, action, result, error, escalated, escalation_reason, latency_ms, in_combat, backend, prompt_tokens) "
                "VALUES ({}, '{}', '{}', '{}', {}, {}, {}, {}, {}, '{}', {})",
                botGuid,
                name, act, result,
                err.empty()   ? std::string("NULL")            : std::string("'" + err + "'"),
                escalated ? 1 : 0,
                esRsn.empty() ? std::string("NULL")            : std::string("'" + esRsn + "'"),
                latencyMs,
                inCombat ? 1 : 0,
                backend ? backend : Jev::kBackendLlm, promptTokens);
        }
        else
        {
            CharacterDatabase.Execute(
                "INSERT INTO mod_ollama_chat_tactical_audit "
                "(bot_guid, bot_name, action, result, error, escalated, escalation_reason, latency_ms, in_combat) "
                "VALUES ({}, '{}', '{}', '{}', {}, {}, {}, {}, {})",
                botGuid,
                name, act, result,
                err.empty()   ? std::string("NULL")            : std::string("'" + err + "'"),
                escalated ? 1 : 0,
                esRsn.empty() ? std::string("NULL")            : std::string("'" + esRsn + "'"),
                latencyMs,
                inCombat ? 1 : 0);
        }
    }
} // namespace

// ---------------------------------------------------------------------------
// TacticalLeaderTick
// ---------------------------------------------------------------------------

TacticalLeaderTick& TacticalLeaderTick::Instance()
{
    static TacticalLeaderTick s;
    return s;
}

void TacticalLeaderTick::Start()
{
    if (!g_TacticalEnable)
    {
        LOG_INFO("server.loading", "[Ollama Chat Tactical] not started (Tactical.Enable=0)");
        return;
    }
    if (m_running.load(std::memory_order_acquire))
    {
        LOG_WARN("server.loading", "[Ollama Chat Tactical] Start() called while already running");
        return;
    }
    // Clear inference health state so a fresh URL / model after reload gets a
    // clean shot instead of inheriting the previous endpoint's UNREACHABLE.
    TacticalInference::Instance().Reset();

    m_stop.store(false, std::memory_order_release);
    m_running.store(true, std::memory_order_release);
    m_thread = std::make_unique<std::thread>([this]{ Run(); });
    LOG_INFO("server.loading",
             "[Ollama Chat Tactical] started: bots=\"{}\", heartbeat={}ms, "
             "cooldown={}ms, model=\"{}\", url=\"{}\", overrides={}",
             g_TacticalBotGUIDs,
             g_TacticalHeartbeatMs,
             g_TacticalBotCooldownMs,
             g_TacticalModel,
             g_TacticalUrl,
             g_TacticalBotConfigs.size());
}

void TacticalLeaderTick::Stop()
{
    if (!m_running.load(std::memory_order_acquire))
        return;
    m_stop.store(true, std::memory_order_release);
    if (m_thread && m_thread->joinable())
        m_thread->join();
    m_thread.reset();
    m_running.store(false, std::memory_order_release);
    LOG_INFO("server.loading", "[Ollama Chat Tactical] stopped");
}

void TacticalLeaderTick::Run()
{
    // Stagger first beat so the world has time to finish loading.
    std::this_thread::sleep_for(std::chrono::seconds(5));

    while (!m_stop.load(std::memory_order_acquire))
    {
        uint32_t intervalMs = std::max<uint32_t>(500u, g_TacticalHeartbeatMs);
        // Sleep in 500ms chunks so Stop() unblocks quickly on shutdown.
        for (uint32_t slept = 0; slept < intervalMs && !m_stop.load(std::memory_order_acquire); slept += 500)
            std::this_thread::sleep_for(std::chrono::milliseconds(500));
        if (m_stop.load(std::memory_order_acquire)) break;

        // Phase 3: presence-gated per-bot tick. Iterates tactical bots, applies
        // the human-presence gate + per-bot cooldown, runs snapshot → Ollama →
        // dispatch for each eligible bot. Never throws; if Ollama is down the
        // circuit breaker silences further calls for a cooldown window — the
        // heartbeat keeps ticking but does no network work.
        try { TickAllBots(); }
        catch (const std::exception& e)
        {
            LOG_WARN("server.loading", "[Ollama Chat Tactical] tick threw: {}", e.what());
        }
        catch (...)
        {
            LOG_WARN("server.loading", "[Ollama Chat Tactical] tick threw (unknown)");
        }

        // Feature 002 — proactive leader bot heartbeat. Cheap when disabled
        // (immediate return inside the function). Never throws (per spec).
        try { ollamachat::proactive::Heartbeat(); }
        catch (const std::exception& e)
        {
            LOG_WARN("server.loading", "[Ollama Chat Proactive] heartbeat threw: {}", e.what());
        }
        catch (...)
        {
            LOG_WARN("server.loading", "[Ollama Chat Proactive] heartbeat threw (unknown)");
        }
    }
}

// ---------------------------------------------------------------------------
// StrategicEscalationWorker
// ---------------------------------------------------------------------------

StrategicEscalationWorker& StrategicEscalationWorker::Instance()
{
    static StrategicEscalationWorker s;
    return s;
}

void StrategicEscalationWorker::Start()
{
    if (!g_TacticalEnable || !g_TacticalAllowEscalation)
    {
        LOG_INFO("server.loading",
                 "[Ollama Chat Strategic Escalation] not started (Tactical.Enable={}, AllowEscalation={})",
                 g_TacticalEnable, g_TacticalAllowEscalation);
        return;
    }
    if (m_running.load(std::memory_order_acquire))
    {
        LOG_WARN("server.loading", "[Ollama Chat Strategic Escalation] Start() called while already running");
        return;
    }
    m_stop.store(false, std::memory_order_release);
    m_running.store(true, std::memory_order_release);
    m_thread = std::make_unique<std::thread>([this]{ Run(); });
    LOG_INFO("server.loading",
             "[Ollama Chat Strategic Escalation] worker started (workers={}, cap={}/hr/bot, cooldown={}s, queue={})",
             g_StrategicWorkerThreads,
             g_StrategicMaxEscalationsPerBotPerHour,
             g_StrategicEscalationCooldownSec,
             g_StrategicQueueMaxSize);
}

void StrategicEscalationWorker::Stop()
{
    if (!m_running.load(std::memory_order_acquire))
        return;
    m_stop.store(true, std::memory_order_release);
    // Wake the worker if it's blocked on the condvar so it exits promptly.
    WakeEscalationWorker();
    if (m_thread && m_thread->joinable())
        m_thread->join();
    m_thread.reset();
    m_running.store(false, std::memory_order_release);
    LOG_INFO("server.loading", "[Ollama Chat Strategic Escalation] worker stopped");
}

void StrategicEscalationWorker::Run()
{
    while (!m_stop.load(std::memory_order_acquire))
    {
        PendingEscalation job;
        bool have = false;

        {
            std::unique_lock<std::mutex> lock(s_queueMutex);
            // Wait for a job, a stop signal, or a Stop()-issued wake flag.
            s_queueCv.wait(lock, []{
                return !s_queue.empty() || s_queueWake.load(std::memory_order_acquire);
            });

            s_queueWake.store(false, std::memory_order_release);

            if (m_stop.load(std::memory_order_acquire)) break;

            if (!s_queue.empty())
            {
                job = std::move(s_queue.front());
                s_queue.pop_front();
                have = true;
            }
        }

        if (!have) continue;

        if (!ClaimEscalationBudget(job.botGuid))
            continue;  // hourly cap or cooldown hit — drop silently

        // Build the strategic prompt. MUST go through QueryGatewayAPIWithTools
        // (not plain QueryGatewayAPI) because the whole point of the escalation
        // round-trip is for the paid model to *call* tactical_set_directive
        // through the OpenAI tools[] + tool_call loop. The plain path only
        // returns prose — tactical_set_directive would never fire and the bot
        // would keep running on its previous plan.
        std::string prompt =
            fmt::format("[TACTICAL ESCALATION] bot={} reason=\"{}\"\nsnapshot: {}\n"
                        "Plan a short goal for this bot's tactical loop and set it by calling "
                        "tactical_set_directive(bot_guid={}, goal=\"<concise plan>\", ttl_sec=<seconds>). "
                        "Keep the goal under ~20 words. ttl_sec should match how long the plan stays valid "
                        "(e.g. 180 for a single fight, 600 for a zone questing sprint). "
                        "If setting a directive isn't useful, reply with a single short word instead.",
                        job.botGuid, job.reason, job.snapshot, job.botGuid);

        try
        {
            // Mark this thread as escalation context for the duration of the
            // gateway call — its tool_call loop dispatches tools (notably
            // tactical_set_directive) synchronously on this thread, and the
            // directive arbitration needs to know the write came from an
            // escalation rather than from the operator's agent. RAII so the
            // flag clears on every exit path, including throws.
            EscalationContextGuard escalationScope;
            std::string response = QueryGatewayAPIWithTools(job.botGuid, /*playerGuid=*/0, prompt);
            std::string truncated = response.size() > 200 ? response.substr(0, 200) + "..." : response;
            LOG_INFO("server.loading",
                     "[Ollama Chat Strategic Escalation] bot={} fired response=\"{}\"",
                     job.botGuid, truncated);
        }
        catch (const std::exception& e)
        {
            LOG_WARN("server.loading",
                     "[Ollama Chat Strategic Escalation] bot={} gateway threw: {}",
                     job.botGuid, e.what());
        }
        catch (...)
        {
            LOG_WARN("server.loading",
                     "[Ollama Chat Strategic Escalation] bot={} gateway threw (unknown)", job.botGuid);
        }
    }
}

// ---------------------------------------------------------------------------
// Public accessors (declared in _tactical.h)
// ---------------------------------------------------------------------------

bool InEscalationContext()
{
    return t_inEscalationContext;
}

TacticalDirective GetTacticalDirective(uint64_t botGuid)
{
    return ReadDirective(botGuid);
}

void SetTacticalDirective(uint64_t botGuid,
                          const std::string& goal,
                          uint32_t ttlSec,
                          const std::string& setBy)
{
    WriteDirective(botGuid, goal, ttlSec, setBy);
}

void EnqueueTacticalEscalation(uint64_t botGuid,
                               const std::string& reason,
                               const std::string& snapshot)
{
    EnqueueEscalationImpl(botGuid, reason, snapshot);
}

void RecordTacticalWorldEvent(uint64_t sourceGuid,
                              const std::string& sourceName,
                              uint32_t mapId,
                              float x,
                              float y,
                              float z,
                              const std::string& eventType,
                              const std::string& detail)
{
    if (!g_TacticalEnable || !g_TacticalAmbientEnable || eventType.empty())
        return;

    TacticalRecentEvent ev;
    ev.sourceGuid = sourceGuid;
    ev.sourceName = sourceName;
    ev.mapId = mapId;
    ev.x = x;
    ev.y = y;
    ev.z = z;
    ev.eventType = eventType;
    ev.detail = detail;
    ev.expiresAt = time(nullptr) + kRecentEventTtlSec;

    std::lock_guard<std::mutex> lock(s_recentEventsMutex);
    s_recentEvents.push_back(std::move(ev));
    while (s_recentEvents.size() > kMaxRecentEvents)
        s_recentEvents.pop_front();
}

void PruneTacticalAuditRows()
{
    if (g_TacticalAuditRetentionDays == 0) return;

    QueryResult tableExists = CharacterDatabase.Query(
        "SELECT TABLE_NAME FROM information_schema.tables "
        "WHERE table_schema = 'acore_characters' AND table_name = 'mod_ollama_chat_tactical_audit'");
    if (!tableExists) return;

    CharacterDatabase.Execute(
        "DELETE FROM mod_ollama_chat_tactical_audit WHERE ts < (NOW() - INTERVAL {} DAY)",
        g_TacticalAuditRetentionDays);
}

bool IsWhitelistedHumanPlayer(Player* p, bool anyHumanWhenNoWhitelist)
{
    if (!p || !p->IsInWorld()) return false;
    if (PlayerbotsMgr::instance().GetPlayerbotAI(p)) return false;      // AI bots, incl. the self-mastered leader
    WorldSession* sess = p->GetSession();
    if (!sess || sess->IsBot()) return false;                              // null-socket sessions: Overlord/Botmaster, rndbots
    const uint32_t low = p->GetGUID().GetCounter();
    if (low != 0 && (low == g_McpLeaderSystemMasterGuid || low == g_FleetLeaderGuid)) return false;   // belt: Botmaster / leader by guid
    for (uint32_t member : g_FleetMemberGuidList)
        if (member == low) return false;
    if (g_GatewayAutoClaimAccountIdsSet.empty()) return anyHumanWhenNoWhitelist;
    return g_GatewayAutoClaimAccountIdsSet.count(sess->GetAccountId()) > 0;
}

nlohmann::json GetTacticalDebugState(uint64_t botGuid)
{
    std::lock_guard<std::mutex> lock(g_TacticalDebugMutex);
    auto it = g_TacticalDebug.find(botGuid);
    if (it == g_TacticalDebug.end()) return nlohmann::json::object();
    const auto& d = it->second;
    return nlohmann::json{{"snapshot", d.snapshot}, {"cooling_down", d.coolingDown},
                          {"last_action", d.lastAction}, {"last_result", d.lastResult},
                          {"age_sec", static_cast<int64_t>(time(nullptr) - d.at)}};
}

nlohmann::json SetTacticalForcedAction(uint64_t botGuid, const std::string& action, int ticks)
{
    if (!kTacticalAllowedActions.count(action))
        return nlohmann::json{{"error", "action not in the tactical whitelist"}, {"action", action}};
    ticks = std::clamp(ticks, 0, 10);
    {
        // A test hook needs a clean baseline: drop this action's no-progress
        // history and cooldown for the bot, both when arming and when clearing
        // (codex: a leftover cooldown made every forced tick idle).
        std::lock_guard<std::mutex> memoLock(g_ActionMemoMutex);
        auto m = g_ActionMemo.find(botGuid);
        if (m != g_ActionMemo.end())
        {
            m->second.history.erase(action);
            m->second.until.erase(action);
        }
    }
    std::lock_guard<std::mutex> lock(g_TacticalForceMutex);
    if (ticks == 0) { g_TacticalForce.erase(botGuid); return nlohmann::json{{"ok", true}, {"cleared", true}}; }
    g_TacticalForce[botGuid] = ForcedAction{action, ticks};
    return nlohmann::json{{"ok", true}, {"action", action}, {"ticks", ticks}};
}
