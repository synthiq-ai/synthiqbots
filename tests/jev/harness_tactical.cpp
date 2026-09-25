// Standalone g++ harness for the tactical funnel's pruning rules
// (Jev::PruneTacticalOptions in mod-ollama-chat_jev_core.h). The whole point of
// the funnel is that every option jev can pick is dispatchable THIS tick, so a
// confident answer is never "blocked" — these checks pin each prune rule, and
// each -DINJECT_* arm disables one rule and must FAIL.
//
//   g++ -std=c++17 -I src -I deps tests/jev/harness_tactical.cpp -o /tmp/ht && /tmp/ht
//   -DINJECT_NO_COMBAT_PRUNE     combat-blocked verbs offered mid-fight        -> FAIL
//   -DINJECT_NO_HITL_PRUNE       HITL-gated verbs offered                      -> FAIL
//   -DINJECT_NO_DEAD_PRUNE       a corpse offered maintenance verbs            -> FAIL
//   -DINJECT_NO_AMBIENT_PRUNE    bot_emote offered in combat                   -> FAIL
//   -DINJECT_NO_MASTER_PRUNE     bot_follow offered with no master             -> FAIL
//   -DINJECT_NO_REZREQ_PRUNE     accept_resurrect offered with no popup        -> FAIL
//   -DINJECT_NO_ACTIONTOOLS_PRUNE action verbs offered with Mcp.AllowActionTools=0 -> FAIL
//   -DINJECT_NO_ENDER_PRUNE      turn-in offered with no quest ender in range  -> FAIL
//   -DINJECT_NO_NEED_PRUNE       maintenance verbs offered with no need / no NPC -> FAIL
//   -DINJECT_NO_BACKOFF_PRUNE    an action in no-progress backoff offered      -> FAIL

#include "mod-ollama-chat_jev_core.h"

#include <algorithm>
#include <cstdio>

static int g_failures = 0;
#define CHECK(cond, msg) do { if (!(cond)) { std::printf("FAIL: %s (%s:%d)\n", msg, __FILE__, __LINE__); ++g_failures; } } while (0)

static bool has(const std::vector<std::string>& v, const char* s)
{
    return std::find(v.begin(), v.end(), std::string(s)) != v.end();
}

// Mirrors kJevTacticalCandidates in mod-ollama-chat_tactical.cpp (17 entries).
static const std::vector<std::string> kEligible = {
    "tactical_idle", "bot_emote", "bot_set_loot_filter",
    "bot_follow", "bot_stay", "bot_flee",
    "bot_revive", "bot_release_spirit", "bot_accept_resurrect_request",
    "bot_turn_in_quest", "bot_accept_quest",
    "bot_maintenance", "bot_train_spells", "bot_sell_junk", "bot_autogear", "bot_roll",
    "bot_reset_ai",
};
// Mirrors kCombatBlockedActions.
static const std::vector<std::string> kCombatBlocked = {
    "bot_attack_target", "bot_stop_combat", "bot_revive", "bot_release_spirit", "bot_revive_target"};
// The live Tactical.RequireConfirmationFor value on 2026-09-21.
static const std::vector<std::string> kConfirm = {
    "bot_accept_quest", "bot_abandon_quest", "bot_equip_item", "bot_unequip_item", "bot_use_item"};

static Jev::TacticalTickFacts Facts(bool inCombat, bool dead)
{
    Jev::TacticalTickFacts f;
    f.inCombat = inCombat;
    f.botIsDead = dead;
    f.hasMaster = true;
    f.rezRequested = false;
    f.questEnderNear = true;
    f.vendorNear = true;
    f.repairNear = true;
    f.trainableSpells = 2;
    f.greyItems = 3;
    f.bagFree = 1;
    f.durabilityMinPct = 30;
    f.ambientEnabled = true;
    f.actionToolsAllowed = true;
    f.allowCombatOverride = false;
    f.repliesDisabledInCombat = true;
    return f;
}

static std::vector<std::string> Prune(Jev::TacticalTickFacts f)
{
#ifdef INJECT_NO_COMBAT_PRUNE
    const std::vector<std::string> blocked;
#else
    const auto& blocked = kCombatBlocked;
#endif
#ifdef INJECT_NO_HITL_PRUNE
    const std::vector<std::string> confirm;
#else
    const auto& confirm = kConfirm;
#endif
#ifdef INJECT_NO_DEAD_PRUNE
    f.botIsDead = false;
#endif
#ifdef INJECT_NO_AMBIENT_PRUNE
    f.repliesDisabledInCombat = false;
#endif
#ifdef INJECT_NO_MASTER_PRUNE
    f.hasMaster = true;
#endif
#ifdef INJECT_NO_REZREQ_PRUNE
    f.rezRequested = true;
#endif
#ifdef INJECT_NO_ACTIONTOOLS_PRUNE
    f.actionToolsAllowed = true;
#endif
#ifdef INJECT_NO_ENDER_PRUNE
    f.questEnderNear = true;
#endif
#ifdef INJECT_NO_NEED_PRUNE
    f.vendorNear = true; f.repairNear = true; f.trainableSpells = 1; f.greyItems = 1; f.durabilityMinPct = 0; f.bagFree = 0;
#endif
#ifdef INJECT_NO_BACKOFF_PRUNE
    f.coolingDown.clear();
#endif
    return Jev::PruneTacticalOptions(kEligible, f, blocked, confirm);
}

int main()
{
    // Alive, out of combat, master present: the everyday menu.
    {
        auto o = Prune(Facts(false, false));
        CHECK(has(o, "tactical_idle") && has(o, "bot_emote") && has(o, "bot_maintenance") && has(o, "bot_turn_in_quest") && has(o, "bot_follow"), "everyday verbs offered");
        CHECK(!has(o, "bot_revive") && !has(o, "bot_release_spirit") && !has(o, "bot_accept_resurrect_request"), "rez verbs hidden while alive");
        CHECK(!has(o, "bot_accept_quest"), "HITL-gated verb hidden");            // FAILS under INJECT_NO_HITL_PRUNE
        CHECK(o.size() == 13, "13 options alive+ooc (17 - 3 rez - 1 HITL)");
    }
    // Completed quest but no NPC in range that ends it (Raz at Undertaker Mordo,
    // 2026-09-25): turn-in hidden, so jev cannot pick it every tick.
    {
        auto f = Facts(false, false);
        f.questEnderNear = false;
        auto o = Prune(f);
        CHECK(!has(o, "bot_turn_in_quest"), "turn-in hidden with no quest ender in range");   // FAILS under INJECT_NO_ENDER_PRUNE
        CHECK(has(o, "bot_maintenance") && has(o, "bot_follow"), "other everyday verbs unaffected");
    }
    // In combat next to the ender: the tool refuses mid-fight even with the
    // combat override on, so the prune must hide it (codex).
    {
        auto f = Facts(true, false);
        f.allowCombatOverride = true;
        CHECK(!has(Prune(f), "bot_turn_in_quest"), "turn-in hidden in combat, override or not");
        CHECK(!has(Prune(f), "bot_train_spells"), "training hidden in combat, override or not");
    }
    // No vendor / no junk / full durability / nothing to learn: the
    // maintenance verbs are not offered (their rules require the need).
    {
        auto f = Facts(false, false);
        f.vendorNear = false; f.repairNear = false; f.trainableSpells = 0;
        f.greyItems = 0; f.bagFree = 12; f.durabilityMinPct = 100;
        auto o = Prune(f);
        CHECK(!has(o, "bot_sell_junk") && !has(o, "bot_maintenance") && !has(o, "bot_train_spells"),
              "maintenance verbs hidden without need or NPC");                     // FAILS under INJECT_NO_NEED_PRUNE
        CHECK(has(o, "bot_follow") && has(o, "bot_turn_in_quest"), "unrelated verbs unaffected");
        f.vendorNear = true; f.greyItems = 2;
        CHECK(has(Prune(f), "bot_sell_junk"), "sell junk offered with junk + vendor");
        f.repairNear = true; f.durabilityMinPct = 20;
        CHECK(has(Prune(f), "bot_maintenance"), "maintenance offered when gear is worn + armorer near");
    }
    // An action that did nothing twice is withheld while it cools down.
    {
        auto f = Facts(false, false);
        f.coolingDown = {"bot_turn_in_quest"};
        auto o = Prune(f);
        CHECK(!has(o, "bot_turn_in_quest"), "cooling-down action hidden");        // FAILS under INJECT_NO_BACKOFF_PRUNE
        CHECK(has(o, "bot_sell_junk"), "other actions unaffected by one cooldown");
    }
    // No registered master: bot_follow would error ("no follow target") — hidden.
    {
        auto f = Facts(false, false); f.hasMaster = false;
        auto o = Prune(f);
        CHECK(!has(o, "bot_follow"), "bot_follow hidden without a master");       // FAILS under INJECT_NO_MASTER_PRUNE
        CHECK(has(o, "bot_stay"), "other movement verbs unaffected");
    }
    // Alive, in combat, override off: combat-blocked + ambient pruned.
    {
        auto o = Prune(Facts(true, false));
        CHECK(!has(o, "bot_emote"), "emote hidden in combat when replies are off");  // FAILS under INJECT_NO_AMBIENT_PRUNE
        CHECK(has(o, "bot_flee") && has(o, "tactical_idle"), "flee + idle stay callable mid-fight");
        CHECK(!has(o, "bot_revive"), "rez verbs hidden (alive)");
    }
    // Dead, in combat (died mid-pull): revive/release are combat-blocked; accept_resurrect needs a popup.
    {
        auto f = Facts(true, true);
        auto o = Prune(f);
        CHECK(!has(o, "bot_revive") && !has(o, "bot_release_spirit"), "graveyard rezzes blocked mid-fight");   // FAILS under INJECT_NO_COMBAT_PRUNE
        CHECK(!has(o, "bot_accept_resurrect_request"), "accept_resurrect hidden with no popup pending");       // FAILS under INJECT_NO_REZREQ_PRUNE
        CHECK(o.size() == 1 && o.front() == "tactical_idle", "a corpse mid-fight with nothing pending can only wait");
        f.rezRequested = true;
        auto p = Prune(f);
        CHECK(has(p, "bot_accept_resurrect_request"), "accept_resurrect offered once a popup is pending (even mid-fight)");
    }
    // Dead, out of combat: graveyard rezzes + idle (no popup).
    {
        auto o = Prune(Facts(false, true));
        CHECK(has(o, "bot_revive") && has(o, "bot_release_spirit") && has(o, "tactical_idle"), "corpse ooc: revive/release/idle");
        CHECK(!has(o, "bot_maintenance") && !has(o, "bot_follow") && !has(o, "bot_emote"), "a corpse gets no maintenance/movement/emote");   // FAILS under INJECT_NO_DEAD_PRUNE
        CHECK(o.size() == 3, "exactly revive + release + idle for a dead bot ooc with no popup");
    }
    // Ambient disabled: emote would be converted to idle by the dispatcher — hidden up front.
    {
        auto f = Facts(false, false); f.ambientEnabled = false;
        auto o = Prune(f);
        CHECK(!has(o, "bot_emote"), "emote hidden when Tactical.Ambient.Enable=0");
    }
    // Action tools globally disabled: only idle survives (every action tool is refused).
    {
        auto f = Facts(false, false); f.actionToolsAllowed = false;
        auto o = Prune(f);
        CHECK(o.size() == 1 && o.front() == "tactical_idle", "only idle when Mcp.AllowActionTools=0");   // FAILS under INJECT_NO_ACTIONTOOLS_PRUNE
    }
    // Combat override ON: combat-blocked verbs come back for a corpse mid-fight.
    {
        auto f = Facts(true, true); f.allowCombatOverride = true;
        auto o = Prune(f);
        CHECK(has(o, "bot_revive") && has(o, "bot_release_spirit"), "override re-admits combat-blocked rezzes");
    }
    // Order is preserved (stable prune) so the file's criteria order is what jev sees.
    {
        auto o = Prune(Facts(false, false));
        CHECK(o.front() == "tactical_idle", "tactical_idle stays first");
    }

    if (g_failures == 0) { std::printf("PASS: harness_tactical\n"); return 0; }
    std::printf("FAIL: harness_tactical (%d)\n", g_failures);
    return 1;
}
