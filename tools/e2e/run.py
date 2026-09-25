#!/usr/bin/env python3
"""E2E runner: a real (headless) human logs in and exercises the fleet features.

Run ON game-host:  cd /srv/wow/e2e && .venv/bin/python run.py [--only S2,S3] [--keep-up]

Exit code 0 = every selected scenario passed (SKIP counts as not-failed).
Prints one JSON line per scenario, then a summary line.
"""
from __future__ import annotations

import argparse
import fcntl
import json
import re
import sys
import time
import traceback
from datetime import datetime, timezone

sys.path.insert(0, ".")
from harness import Mcp, dist, running_services, sql, start_stack, stop_services, wait_mcp_ready, \
    worldserver_log_since  # noqa: E402
from wowclient.auth import logon  # noqa: E402
from wowclient.world import CHAT_WHISPER, WorldClient  # noqa: E402

LEADER, MEMBER = ("Claude", 20014), ("Raz", 20015)
TESTER = "Qaprobe"
FLEET_TELE = "Deathknell"          # game_tele row ~180 yd from where the fleet parks
BOT_PARK_RACE = "Undead"           # Undead start ≈190 yd from the Deathknell tele: a real, safe follow
OPERATOR_ACCOUNT = 1001
BOT_GUIDS_ON_OPERATOR_ACCOUNT = {20010, 20014, 20015}   # Botmaster + fleet live on 1001
GOSSIP_TELLS = re.compile(r"\$n|greetings", re.I)


def log(msg: str) -> None:
    print(f"# {datetime.now().strftime('%H:%M:%S')} {msg}", flush=True)


class Skip(Exception):
    pass


def wait_for(pred, timeout: float, poll: float = 2.0):
    deadline = time.time() + timeout
    last = None
    while time.time() < deadline:
        last = pred()
        if last:
            return last
        time.sleep(poll)
    return last


# ---------------------------------------------------------------------------
def s1_session(ctx) -> dict:
    """Login, presence, cross-map teleport ACK, stay connected past idle."""
    wc, mcp = ctx["wc"], ctx["mcp"]
    g = mcp.gps(TESTER)
    assert "error" not in g, f"tester not visible: {g}"
    t = time.time()
    out = mcp.gm(f"tele name {TESTER} {FLEET_TELE}")
    assert out.get("ok"), f"teleport command failed: {out}"
    assert wc.wait_until(lambda: wc.events_since(t, "teleport_far") or wc.events_since(t, "teleport_near"), 20), \
        "no teleport packet reached the client"
    g2 = wait_for(lambda: (lambda x: x if x.get("map") == 0 else None)(mcp.gps(TESTER)), 30)
    assert g2 and g2.get("map") == 0, f"tester not on map 0 after teleport ACK: {g2}"
    time.sleep(70)   # past SocketTimeOutTimeActive-scale idle while only keepalive/time-sync flow
    assert wc.in_world, "disconnected while idle"
    return {"tester": {k: g2[k] for k in ("map", "zone")}}


def s2_operator_mode(ctx) -> dict:
    """#457: the fleet leader invites the human (direct join); every fleet bot takes him as master
    and actually runs to him."""
    wc, mcp = ctx["wc"], ctx["mcp"]
    # Start apart, or "the bots came to him" proves nothing (codex): a previous
    # run leaves them parked next to the tester.
    me = mcp.gps(TESTER)
    already = LEADER[0] in (me.get("group") or {}).get("members", [])
    gap = [dist(mcp.gps(n), me) for n, _ in (LEADER, MEMBER)]
    # Normalise the start to a real but runnable follow: 40–250 yd. After a
    # restart the bots can wake 1400 yd away (measured), which no 150 s window
    # covers; right next to the tester, arrival proves nothing.
    if not already and (min(gap) < 40 or max(gap) > 250):
        # Park the bots at the Undead start. Moving the tester 1400 yd instead
        # got Claude killed on the run over (2026-09-25).
        for n, _ in (LEADER, MEMBER):
            r = mcp.act("leader_admin_teleport", {"botGuid": LEADER[1], "name": n, "to_race_start": BOT_PARK_RACE})
            assert "error" not in r, f"could not park {n}: {r}"
        time.sleep(5)
        me = mcp.gps(TESTER)
    d0 = {n: round(dist(mcp.gps(n), me), 1) for n, _ in (LEADER, MEMBER)}
    assert already or 40 <= min(d0.values()) and max(d0.values()) <= 250, \
        f"could not stage the fleet 40–250 yd from the tester: {d0}"
    t = time.time()
    if not already:
        r = mcp.act("bot_invite_to_group", {"botGuid": LEADER[1], "name": TESTER})
        assert "error" not in r, f"invite failed: {r}"
        assert wc.wait_until(lambda: wc.events_since(t, "group_list"), 20), "no SMSG_GROUP_LIST"
    masters = wait_for(lambda: (lambda a, b: (a, b) if a.get("master") == TESTER and b.get("master") == TESTER
                                else None)(mcp.gps(LEADER[0]), mcp.gps(MEMBER[0])), 60, poll=5)
    assert masters, f"masters not the human within 60 s: {mcp.gps(LEADER[0]).get('master')}, " \
                    f"{mcp.gps(MEMBER[0]).get('master')}"
    me = mcp.gps(TESTER)
    near = wait_for(lambda: all(dist(mcp.gps(n), me) < 25 for n, _ in (LEADER, MEMBER)), 150, poll=5)
    d = {n: round(dist(mcp.gps(n), me), 1) for n, _ in (LEADER, MEMBER)}
    assert near, f"bots did not come to the human within 150 s: start {d0}, now {d}"
    ctx["party"] = True
    return {"masters": TESTER, "distance_yd": {"start": d0, "end": d}}


def s3_facade_identity(ctx) -> dict:
    """#458: a grouped call with botGuid INSIDE params acts on that bot, not the leader."""
    mcp = ctx["mcp"]
    before_leader = mcp.gps(LEADER[0]).get("master")
    r = mcp.act("bot_set_master", {"botGuid": MEMBER[1], "name": TESTER})
    assert r.get("bot") == MEMBER[0], f"routed to {r.get('bot')!r}, not {MEMBER[0]}: {r}"
    after_leader = mcp.gps(LEADER[0]).get("master")
    assert after_leader == before_leader, f"leader master changed {before_leader} -> {after_leader}"
    return {"routed_to": r.get("bot")}


def s4_no_gossip(ctx) -> dict:
    """#459: with the human as master, an explicit turn-in never whispers gossip; and 90 s of
    tactical ticks produce no gossip whispers either."""
    wc, mcp = ctx["wc"], ctx["mcp"]
    t = time.time()
    r = mcp.act("bot_turn_in_quest", {"botGuid": MEMBER[1]})
    if r.get("error") == "rate-limited":
        time.sleep(61)
        r = mcp.act("bot_turn_in_quest", {"botGuid": MEMBER[1]})
    # Only two outcomes exercise the new path: a clean refusal (no ender in
    # range) or a turn-in at a named ender. Anything else never tested it (codex).
    refused = "no NPC within range ends" in str(r.get("error", ""))
    turned_in = bool(r.get("npc"))
    assert refused or turned_in, f"turn-in call did not exercise the grounded path: {r}"
    time.sleep(90)
    # Silence only counts if a session was there to hear it (codex).
    assert wc.in_world and not wc.events_since(t, "disconnected"), "client lost its session during the window"
    errs = wc.events_since(t, "parse_error")
    assert not errs, f"packet parse errors during the window: {[e.data for e in errs][:3]}"
    tells = [c for c in wc.chat_since(t) if c.chat_type == CHAT_WHISPER]
    bad = [c.text for c in tells if GOSSIP_TELLS.search(c.text)]
    assert not bad, f"gossip whispers reached the human: {bad[:3]}"
    return {"turn_in_result": r, "whispers_seen": len(tells)}


def s5_tactical_facts(ctx) -> dict:
    """#460: the tactical snapshot carries the grounding facts."""
    mcp = ctx["mcp"]
    try:
        st = mcp.act("admin_tactical_state", {"botGuid": MEMBER[1]})
    except KeyError:
        raise Skip("admin_tactical_state not deployed yet")
    st = wait_for(lambda: (lambda x: x if x.get("age_sec", 999) < 60 else None)(
        mcp.act("admin_tactical_state", {"botGuid": MEMBER[1]})), 60, poll=5)
    assert st, "no tactical tick in the last 60 s — the loop is stalled or gated off"
    snap = st.get("snapshot", "")
    missing = [k for k in ("bag_free=", "grey_items=", "durability_min_pct=") if k not in snap]
    assert not missing, f"snapshot lacks {missing}: {snap[:300]}"
    return {"cooling_down": st.get("cooling_down"), "last_action": st.get("last_action"),
            "facts": re.findall(r"(bag_free=\S+|grey_items=\S+|durability_min_pct=\S+|turnin_npc=\S+|"
                                r"vendor_near=\S+|class_trainer_near=\S+)", snap)}


def s6_identity_chat(ctx) -> dict:
    """#457 identity block, through the real chat path: the leader knows the human's name."""
    wc = ctx["wc"]
    t = time.time()
    wc.whisper(LEADER[0], "Quick check, no actions: what is my character name?")
    def leader_tells():
        return [c.text for c in wc.chat_since(t) if c.chat_type == CHAT_WHISPER and c.sender_guid == LEADER[1]]
    # Proactive lines can arrive by whisper too, so wait for ANY leader whisper
    # naming the human rather than judging the first one (codex).
    hit = wc.wait_until(lambda: [x for x in leader_tells() if TESTER.lower() in x.lower()], 90)
    assert hit, f"no leader whisper named the human within 90 s; got {leader_tells()[:3]!r}"
    return {"reply": hit[0][:160]}


SEED_QUEST = (363, "Shadow Priest Sarvis", (0, 1843.0, 1636.0, 98.0))   # Rude Awakening, Deathknell church


def s8_real_turn_in(ctx) -> dict:
    """#459 positive path: a completed quest is handed in at the NPC that ends it — rewarded,
    and the human gets no gossip whisper."""
    wc, mcp = ctx["wc"], ctx["mcp"]
    qid, ender, (m, x, y, z) = SEED_QUEST
    seed = mcp.act("admin_quest_state", {"name": MEMBER[0], "quest_id": qid, "mode": "complete"})
    assert seed.get("after") == "complete", f"could not seed quest {qid}: {seed}"
    r = mcp.act("leader_admin_teleport", {"botGuid": LEADER[1], "name": MEMBER[0], "map": m, "x": x, "y": y, "z": z})
    assert "error" not in r, f"could not move {MEMBER[0]} to {ender}: {r}"
    t = time.time()
    t_iso = datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")
    time.sleep(3)
    r = mcp.act("bot_turn_in_quest", {"botGuid": MEMBER[1]})
    if r.get("error") == "rate-limited":
        time.sleep(61)
        r = mcp.act("bot_turn_in_quest", {"botGuid": MEMBER[1]})
    # Raz's own tactical loop may hand the quest in first (it sees turnin_npc=Sarvis
    # now) — that is the feature working, not a failure (codex).
    autonomous = "no NPC within range ends" in str(r.get("error", "")) and re.search(
        r"tick bot=%d .*action='bot_turn_in_quest' ok" % MEMBER[1], worldserver_log_since(t_iso))
    assert (r.get("ok") and r.get("npc") == ender) or autonomous, f"turn-in did not go to {ender}: {r}"
    st = wait_for(lambda: (lambda x: x if x.get("after") == "rewarded" else None)(
        mcp.act("admin_quest_state", {"name": MEMBER[0], "quest_id": qid})), 15, poll=2)
    assert st, f"quest {qid} not rewarded after the turn-in: {mcp.act('admin_quest_state', {'name': MEMBER[0], 'quest_id': qid})}"
    time.sleep(5)
    assert wc.in_world and not wc.events_since(t, "disconnected"), "client lost its session during the window"
    assert not wc.events_since(t, "parse_error"), "packet parse errors during the window"
    bad = [c.text for c in wc.chat_since(t) if c.chat_type == CHAT_WHISPER and GOSSIP_TELLS.search(c.text)]
    assert not bad, f"gossip whispers reached the human: {bad[:3]}"
    return {"npc": r.get("npc") or ender, "by": "tactical loop" if autonomous else "explicit call",
            "quest": "rewarded"}


def s9_party_commands(ctx) -> dict:
    """Party-chat orders reach the named bot: "Raz stay", then "Raz follow"."""
    wc = ctx["wc"]
    seen = {}
    for word in ("stay", "follow"):
        t = datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")
        wc.party(f"{MEMBER[0]} {word}")
        # Direct dispatch logs bot=<guid>; a relay through the leader logs target=<guid|name>.
        # The direct path dispatches "follow <requester>", so allow an argument.
        pat = re.compile(rf"(bot={MEMBER[1]}\b|target=(?:{MEMBER[1]}|{MEMBER[0]})\b).*text='{word}( [^']*)?'")
        hit = wait_for(lambda: pat.search(worldserver_log_since(t)), 75, poll=3)   # may fall to the gateway LLM (~10-40 s)
        assert hit, f'"{MEMBER[0]} {word}" in party never dispatched {word!r} to {MEMBER[0]}'
        seen[word] = hit.group(0)[:120]
        time.sleep(3)
    return seen


def s10_proactive_approve(ctx) -> dict:
    """Proactive: the leader proposes, the human says yes → the proposal is approved (not
    swallowed, not treated as chat). SKIP when no proposal arrives within 5 min."""
    wc = ctx["wc"]
    who = r"bot=%d player=%d" % (LEADER[1], ctx["tester_guid"])

    def open_proposal():
        # A proposal opened during an earlier scenario still counts (the heartbeat
        # won't open a second one while it waits) — codex. Open = the last
        # propose row since the run started has no later disposition row.
        text = worldserver_log_since(ctx["run_start"])
        # Only the proposal lifecycle counts; intent / nudge / tick-gate rows do
        # not close a proposal (expiry is logged as abort) — codex.
        last = None
        for m in re.finditer(r"audit src=proact_(propose|approve|reject|abort|done) " + who, text):
            last = m.group(1)
        return last == "propose"

    prop = wait_for(open_proposal, 300, poll=10)
    if not prop:
        raise Skip("the leader made no proposal in 5 min (tick-gate said wait)")
    t2 = datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")
    wc.party("yes, let's do it")
    ok = wait_for(lambda: re.search(r"audit src=proact_approve bot=%d player=%d" % (LEADER[1], ctx["tester_guid"]),
                                    worldserver_log_since(t2)), 60, poll=3)
    assert ok, "a plain yes to an open proposal was not approved"
    return {"approved": True}


def s11_backoff(ctx) -> dict:
    """#460: an action that fails twice in a row is withheld (deterministic: forced ticks)."""
    mcp = ctx["mcp"]
    t = datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")
    # A guaranteed failure: after S8 handed the seed quest in, no NPC in range ends
    # a completed quest of Raz's — verify that first (a Priest trainer stands in
    # Deathknell, so bot_train_spells could succeed — codex).
    pre = mcp.act("bot_turn_in_quest", {"botGuid": MEMBER[1]})
    if pre.get("error") == "rate-limited":
        time.sleep(61)
        pre = mcp.act("bot_turn_in_quest", {"botGuid": MEMBER[1]})
    if "no NPC within range ends" not in str(pre.get("error", "")):
        raise Skip(f"precondition not met — a turn-in is possible here: {pre}")
    t = datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")
    r = mcp.act("admin_tactical_force", {"botGuid": MEMBER[1], "action": "bot_turn_in_quest", "ticks": 3})
    assert r.get("ok"), f"force hook refused: {r}"
    try:
        hit = wait_for(lambda: re.search(r"bot=%d .*action='bot_turn_in_quest' made no progress" % MEMBER[1],
                                         worldserver_log_since(t)), 90, poll=5)
        assert hit, "two failed forced ticks did not trigger the no-progress backoff within 90 s"
        # The snapshot refreshes on the NEXT tick; poll for it (codex).
        st = wait_for(lambda: (lambda x: x if "bot_turn_in_quest" in (x.get("cooling_down") or []) else None)(
            mcp.act("admin_tactical_state", {"botGuid": MEMBER[1]})), 40, poll=3)
        assert st, "backoff logged but cooling_down never showed bot_turn_in_quest"
        # And the third forced tick must actually be withheld, not dispatched.
        def withheld_after_backoff():
            text = worldserver_log_since(t)
            b = re.search(r"bot=%d .*action='bot_turn_in_quest' made no progress" % MEMBER[1], text)
            return b and re.search(r"bot=%d .*withheld 'bot_turn_in_quest'" % MEMBER[1], text[b.end():])
        held = wait_for(withheld_after_backoff, 40, poll=3)
        assert held, "the third forced bot_turn_in_quest was not withheld during its cooldown"
        return {"backoff": hit.group(0)[:120], "withheld": True}
    finally:
        mcp.act("admin_tactical_force", {"botGuid": MEMBER[1], "action": "bot_turn_in_quest", "ticks": 0})


def s7_leave_restores(ctx) -> dict:
    """#457: when the human leaves, the fleet returns to leader self-master / member→leader."""
    wc, mcp = ctx["wc"], ctx["mcp"]
    before = (mcp.gps(LEADER[0]).get("master"), mcp.gps(MEMBER[0]).get("master"))
    assert before == (TESTER, TESTER), f"precondition: masters should be the tester, are {before}"
    wc.leave_group()
    ctx["party"] = False
    ok = wait_for(lambda: mcp.gps(LEADER[0]).get("master") == LEADER[0]
                  and mcp.gps(MEMBER[0]).get("master") == LEADER[0], 60, poll=5)
    assert ok, f"not restored: {mcp.gps(LEADER[0]).get('master')}, {mcp.gps(MEMBER[0]).get('master')}"
    return {"restored": True}


# Scenarios that only mean something with the tester in the fleet party as
# master. A filtered run (--only) establishes that first instead of silently
# testing the wrong state (codex).
NEEDS_PARTY = {"S3", "S4", "S7", "S8", "S9", "S10", "S11"}

SCENARIOS = [("S1", s1_session), ("S2", s2_operator_mode), ("S3", s3_facade_identity),
             ("S4", s4_no_gossip), ("S5", s5_tactical_facts), ("S6", s6_identity_chat),
             ("S8", s8_real_turn_in), ("S9", s9_party_commands), ("S11", s11_backoff),
             ("S10", s10_proactive_approve), ("S7", s7_leave_restores)]


# ---------------------------------------------------------------------------
def operator_online() -> list[str]:
    rows = sql(f"select guid,name from acore_characters.characters "
               f"where account={OPERATOR_ACCOUNT} and online=1;")
    return [name for guid, name in rows if int(guid) not in BOT_GUIDS_ON_OPERATOR_ACCOUNT]


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--only", default="", help="comma list, e.g. S2,S3")
    ap.add_argument("--keep-up", action="store_true", help="never stop the stack afterwards")
    ap.add_argument("--allow-operator-online", action="store_true")
    a = ap.parse_args()
    only = {s.strip() for s in a.only.split(",") if s.strip()}

    import os
    # Shared by host-side runs and the deploy step's container (it mounts this
    # directory): two runs must never log the same tester in — codex.
    lock_dir = os.environ.get("E2E_LOCK_DIR", "/srv/wow/docker/wow/e2e-lock")
    if not os.path.isdir(lock_dir):
        lock_dir = "/tmp"
    path_ = os.path.join(lock_dir, "wow-e2e.lock")
    fd = os.open(path_, os.O_RDWR | os.O_CREAT, 0o666)
    try:
        os.chmod(path_, 0o666)   # host (game-host) and deploy container must both open it
    except PermissionError:
        pass
    lock = os.fdopen(fd, "w")
    try:
        fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
    except BlockingIOError:
        print(json.dumps({"summary": "another E2E run holds the lock"}))
        return 2

    started: set[str] = set()
    results = []
    wc = None
    try:
        started = start_stack(log, started)   # fills `started` as it goes, so a partial start is cleaned up
        mcp = Mcp()
        wait_mcp_ready(mcp)
        humans = operator_online()
        if humans and not a.allow_operator_online:
            print(json.dumps({"summary": f"operator is playing ({humans}) — not running"}))
            return 3
        run_start = datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")

        import os
        u, p = open(os.environ.get("E2E_CREDS", ".creds")).read().split()
        key, realms = logon("127.0.0.1", 3724, u, p)
        port = int(realms[0].address.split(":")[1])
        wc = WorldClient("127.0.0.1", port, u, key, realm_id=realms[0].id, log=log)
        chars = [c for c in wc.characters() if c.name == TESTER]
        assert chars, f"{TESTER} missing on the E2E account"
        wc.enter_world(chars[0])
        ctx = {"wc": wc, "mcp": mcp, "tester_guid": chars[0].guid, "run_start": run_start}
        # The fleet must be alive and in world for anything below to mean something.
        for n, g in (LEADER, MEMBER):
            st = wait_for(lambda: (lambda x: x if "error" not in x else None)(mcp.gps(n)), 90, poll=10)
            assert st, f"{n} is not in world (fleet re-spawn needs a human online — see fleet.cpp)"
            if not st.get("alive", True):
                log(f"{n} is dead — reviving")
                mcp.act("bot_revive", {"botGuid": g})
                wait_for(lambda: mcp.gps(n).get("alive"), 30, poll=3)
        # Group membership survives logout: a crashed earlier run can leave the
        # tester in the fleet party, and S2's direct join then refuses.
        wc.leave_group()
        wait_for(lambda: not mcp.gps(TESTER).get("group"), 20)
        time.sleep(3)

        for sid, fn in SCENARIOS:
            if only and sid not in only:
                continue
            t0 = time.time()
            try:
                if sid in NEEDS_PARTY and not ctx.get("party"):
                    log(f"{sid} needs the party — running S2 setup first")
                    s2_operator_mode(ctx)
                detail = fn(ctx)
                status = "PASS"
            except Skip as e:
                status, detail = "SKIP", {"why": str(e)}
            except AssertionError as e:
                status, detail = "FAIL", {"why": str(e)}
            except Exception as e:
                status, detail = "ERROR", {"why": repr(e), "trace": traceback.format_exc()[-600:]}
            res = {"scenario": sid, "doc": (fn.__doc__ or "").strip().split("\n")[0], "status": status,
                   "secs": round(time.time() - t0, 1), "detail": detail}
            results.append(res)
            print(json.dumps(res, default=str), flush=True)

        text = worldserver_log_since(run_start)
        crashes = re.findall(r".*(ASSERT|Segmentation|Crash).*", text)
        backoffs = re.findall(r"made no progress \dx .*", text)
        print(json.dumps({"worldserver": {"assert_or_crash_lines": crashes[:3],
                                          "no_progress_backoffs": backoffs[:5]}}), flush=True)
    finally:
        if wc:
            try:
                wc.leave_group()
                wc.logout()
            except Exception:
                pass
            wc.close()
        if not a.keep_up and started:
            # The operator may have logged in during the run: never pull the
            # server out from under him (codex).
            try:
                now_playing = operator_online()
            except Exception:
                now_playing = ["<unknown — DB check failed>"]
            if now_playing:
                print(json.dumps({"stack": f"left running: operator online {now_playing}"}))
            else:
                stop_services(started, log)

    failed = [r["scenario"] for r in results if r["status"] in ("FAIL", "ERROR")]
    print(json.dumps({"summary": "PASS" if not failed else "FAIL", "failed": failed,
                      "ran": [r["scenario"] for r in results]}))
    return 0 if not failed else 1


if __name__ == "__main__":
    sys.exit(main())
