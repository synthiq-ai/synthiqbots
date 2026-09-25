#include "mod-ollama-chat_leadertick.h"
#include "mod-ollama-chat_config.h"
#include "mod-ollama-chat_gateway.h"
#include "mod-ollama-chat_tools.h"
#include "Player.h"
#include "PlayerbotMgr.h"
#include "PlayerbotAI.h"
#include "ObjectAccessor.h"
#include "ObjectGuid.h"
#include "Group.h"
#include "Log.h"

#include <chrono>
#include <mutex>
#include <unordered_map>
#include <vector>
#include <algorithm>
#include <sstream>
#include <cmath>

namespace
{
    struct LeaderSnapshot
    {
        bool     valid          = false;
        bool     inCombat       = false;
        uint64_t targetGuid     = 0;
        uint8_t  selfHpBucket   = 100;        // rounded to nearest 25%
        uint8_t  targetHpBucket = 100;
        uint32_t zoneId         = 0;
        uint32_t groupSize      = 0;
        uint64_t groupHash      = 0;          // simple hash of sorted member guids
        uint32_t nearbyHostiles = 0;          // hostile creatures within 30y of leader

        // Returns true when the two snapshots represent the "same" situation.
        // hp buckets + group hash + combat boolean carry most of the signal.
        bool SameAs(const LeaderSnapshot& o) const
        {
            return valid == o.valid
                && inCombat == o.inCombat
                && targetGuid == o.targetGuid
                && selfHpBucket == o.selfHpBucket
                && targetHpBucket == o.targetHpBucket
                && zoneId == o.zoneId
                && groupSize == o.groupSize
                && groupHash == o.groupHash
                && nearbyHostiles == o.nearbyHostiles;
        }
    };

    uint8_t HpBucket(float pct)
    {
        if (pct >= 88.0f) return 100;
        if (pct >= 63.0f) return 75;
        if (pct >= 38.0f) return 50;
        if (pct >= 13.0f) return 25;
        return 0;
    }

    LeaderSnapshot SampleLeader(uint64_t leaderGuid)
    {
        LeaderSnapshot s;
        Player* leader = ObjectAccessor::FindPlayer(ObjectGuid(leaderGuid));
        if (!leader || !leader->IsInWorld()) return s;

        s.valid        = true;
        s.inCombat     = leader->IsInCombat();
        s.selfHpBucket = HpBucket(leader->GetHealthPct());
        s.zoneId       = leader->GetZoneId();

        if (Unit* target = leader->GetVictim())
        {
            s.targetGuid     = target->GetGUID().GetCounter();
            s.targetHpBucket = HpBucket(target->GetHealthPct());
        }

        if (Group* g = leader->GetGroup())
        {
            std::vector<uint64_t> guids;
            for (GroupReference const* ref = g->GetFirstMember(); ref; ref = ref->next())
            {
                Player* m = ref->GetSource();
                if (!m) continue;
                guids.push_back(m->GetGUID().GetCounter());
            }
            std::sort(guids.begin(), guids.end());
            s.groupSize = static_cast<uint32_t>(guids.size());
            // FNV-1a 64-bit
            uint64_t h = 1469598103934665603ULL;
            for (uint64_t g : guids)
            {
                h ^= g;
                h *= 1099511628211ULL;
            }
            s.groupHash = h;
        }

        // Nearby hostiles — cheap O(players) sweep; tools.cpp uses Cell::VisitObjects but we
        // intentionally stay coarse here (the agent can call get_nearby_creatures itself for
        // detail). We just need a "did the threat picture change" signal.
        for (auto const& pair : ObjectAccessor::GetPlayers())
        {
            Player* p = pair.second;
            if (!p || p == leader || !p->IsInWorld()) continue;
            if (leader->GetDistance(p) > 30.0f) continue;
            if (leader->IsHostileTo(p)) ++s.nearbyHostiles;
        }
        return s;
    }

    std::string Describe(const LeaderSnapshot& cur, const LeaderSnapshot& prev)
    {
        std::stringstream ss;
        ss << "Current state — ";
        ss << "hp:" << static_cast<int>(cur.selfHpBucket) << "%";
        ss << " combat:" << (cur.inCombat ? "yes" : "no");
        if (cur.targetGuid != 0)
            ss << " target_hp:" << static_cast<int>(cur.targetHpBucket) << "%";
        ss << " group:" << cur.groupSize;
        ss << " hostiles_near:" << cur.nearbyHostiles;

        ss << "\nChanged since last tick: ";
        bool any = false;
        if (cur.inCombat != prev.inCombat)
        {
            ss << (cur.inCombat ? "ENGAGED COMBAT" : "DISENGAGED");
            any = true;
        }
        if (cur.targetGuid != prev.targetGuid)
        {
            if (any) ss << ", ";
            ss << (cur.targetGuid != 0 ? "new target" : "lost target");
            any = true;
        }
        if (cur.selfHpBucket != prev.selfHpBucket)
        {
            if (any) ss << ", ";
            ss << "self_hp " << static_cast<int>(prev.selfHpBucket) << "→" << static_cast<int>(cur.selfHpBucket);
            any = true;
        }
        if (cur.targetHpBucket != prev.targetHpBucket && cur.targetGuid != 0)
        {
            if (any) ss << ", ";
            ss << "target_hp " << static_cast<int>(prev.targetHpBucket) << "→" << static_cast<int>(cur.targetHpBucket);
            any = true;
        }
        if (cur.zoneId != prev.zoneId)
        {
            if (any) ss << ", ";
            ss << "zone changed";
            any = true;
        }
        if (cur.groupHash != prev.groupHash)
        {
            if (any) ss << ", ";
            ss << "group composition changed (size " << prev.groupSize << "→" << cur.groupSize << ")";
            any = true;
        }
        if (cur.nearbyHostiles != prev.nearbyHostiles)
        {
            if (any) ss << ", ";
            ss << "nearby_hostiles " << prev.nearbyHostiles << "→" << cur.nearbyHostiles;
            any = true;
        }
        if (!any) ss << "(no significant change)";
        return ss.str();
    }

    // Per-leader hourly budget log (drops timestamps older than 1h on each claim).
    std::mutex s_budgetMutex;
    std::unordered_map<uint64_t, std::vector<time_t>> s_budgetLog;

    bool ClaimTickBudget(uint64_t leaderGuid)
    {
        if (g_McpLeaderAutoTickMaxPerHour == 0) return true;
        time_t now = time(nullptr);
        std::lock_guard<std::mutex> lock(s_budgetMutex);
        auto& bucket = s_budgetLog[leaderGuid];
        bucket.erase(std::remove_if(bucket.begin(), bucket.end(),
                                    [now](time_t t){ return now - t > 3600; }),
                     bucket.end());
        if (bucket.size() >= g_McpLeaderAutoTickMaxPerHour) return false;
        bucket.push_back(now);
        return true;
    }
}

GatewayLeaderTick& GatewayLeaderTick::Instance()
{
    static GatewayLeaderTick s;
    return s;
}

void GatewayLeaderTick::Start()
{
    if (!g_McpEnable || !g_McpLeaderEnable || !g_McpLeaderAutoTickEnable)
    {
        LOG_INFO("server.loading", "[Ollama Chat MCP Leader Tick] not started (Mcp.Enable={}, Leader.Enable={}, AutoTickEnable={})",
                 g_McpEnable, g_McpLeaderEnable, g_McpLeaderAutoTickEnable);
        return;
    }
    if (m_running.load(std::memory_order_acquire))
    {
        LOG_WARN("server.loading", "[Ollama Chat MCP Leader Tick] Start() called while already running");
        return;
    }
    m_stop.store(false, std::memory_order_release);
    m_running.store(true, std::memory_order_release);
    m_thread = std::make_unique<std::thread>([this]{ Run(); });
    LOG_INFO("server.loading", "[Ollama Chat MCP Leader Tick] started: leaders={}, interval={}s, max={}/hr",
             g_McpLeaderBotGUIDSet.size(), g_McpLeaderAutoTickIntervalSeconds, g_McpLeaderAutoTickMaxPerHour);
}

void GatewayLeaderTick::Stop()
{
    if (!m_running.load(std::memory_order_acquire))
        return;
    m_stop.store(true, std::memory_order_release);
    if (m_thread && m_thread->joinable())
        m_thread->join();
    m_thread.reset();
    m_running.store(false, std::memory_order_release);
    LOG_INFO("server.loading", "[Ollama Chat MCP Leader Tick] stopped");
}

void GatewayLeaderTick::Run()
{
    // Per-leader rolling snapshot. Local to this thread — never accessed elsewhere.
    std::unordered_map<uint64_t, LeaderSnapshot> last;

    // Stagger first tick a bit so we don't fire instantly on startup.
    std::this_thread::sleep_for(std::chrono::seconds(5));

    while (!m_stop.load(std::memory_order_acquire))
    {
        uint32_t interval = std::max<uint32_t>(5u, g_McpLeaderAutoTickIntervalSeconds);
        // Sleep in 1s chunks so Stop() unblocks quickly on shutdown.
        for (uint32_t i = 0; i < interval && !m_stop.load(std::memory_order_acquire); ++i)
            std::this_thread::sleep_for(std::chrono::seconds(1));
        if (m_stop.load(std::memory_order_acquire)) break;

        // Snapshot the current leader set so config reloads don't surprise us mid-iteration.
        std::vector<uint64_t> leaders(g_McpLeaderBotGUIDSet.begin(), g_McpLeaderBotGUIDSet.end());
        for (uint64_t leaderGuid : leaders)
        {
            // Companion-mode: if this leader is also tactical-enabled and the
            // suppression flag is on, skip it entirely — the tactical loop
            // covers the decision space and fires paid gateway only on
            // explicit escalation. Skipping here prevents double-billing on
            // the same bot.
            //
            // g_TacticalEnable is checked too: when tactical is globally off
            // (rollback / debugging) leaving a bot guid in Tactical.BotGUIDs
            // must NOT also suppress the classic leader-tick — otherwise the
            // bot loses both proactive paths.
            if (g_TacticalEnable
                && g_StrategicSuppressLeaderTickForTacticalBots
                && g_TacticalBotGUIDSet.count(leaderGuid) > 0)
            {
                continue;
            }

            // Bot offline / not yet loaded? Skip silently.
            Player* leader = ObjectAccessor::FindPlayer(ObjectGuid(leaderGuid));
            if (!leader || !leader->IsInWorld()) continue;
            // Real bot? (defense — leader allowlist could be misconfigured)
            if (!PlayerbotsMgr::instance().GetPlayerbotAI(leader)) continue;

            LeaderSnapshot cur = SampleLeader(leaderGuid);
            if (!cur.valid) continue;

            auto it = last.find(leaderGuid);
            if (it != last.end() && cur.SameAs(it->second))
                continue;  // no material change — don't spend tokens

            if (!ClaimTickBudget(leaderGuid))
            {
                LOG_DEBUG("server.loading", "[Ollama Chat MCP Leader Tick] leader {} hourly budget exhausted", leaderGuid);
                continue;
            }

            std::string desc = Describe(cur, it != last.end() ? it->second : LeaderSnapshot{});
            std::string prompt = "[AUTONOMOUS TICK] " + desc;

            LOG_INFO("server.loading", "[Ollama Chat MCP Leader Tick] firing leader={} prompt='{}'",
                     leaderGuid, desc);

            // Fire the gateway in a detached thread — keep the tick loop snappy and isolated
            // from upstream latency. The response text is logged; tool calls (if any) execute
            // synchronously inside QueryGatewayAPI before it returns.
            std::thread([leaderGuid, prompt]() {
                try {
                    std::string response = QueryGatewayAPI(leaderGuid, /*playerGuid=*/0, prompt);
                    if (!response.empty())
                    {
                        // Truncate for log — full response is in the gateway debug log if enabled.
                        std::string truncated = response.size() > 200 ? response.substr(0, 200) + "..." : response;
                        LOG_INFO("server.loading", "[Ollama Chat MCP Leader Tick] leader={} response='{}'",
                                 leaderGuid, truncated);
                    }
                } catch (const std::exception& e) {
                    LOG_WARN("server.loading", "[Ollama Chat MCP Leader Tick] leader={} threw: {}", leaderGuid, e.what());
                }
            }).detach();

            last[leaderGuid] = cur;
        }
    }
}
