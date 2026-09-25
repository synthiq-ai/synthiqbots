#!/usr/bin/env python3
"""E2E for bot promotion (PR #461): a random online Horde playerbot, addressed by the
headless human, answers on the strategic lane; invited, it becomes a party guest; after
the human leaves the group it is demoted.

Run ON game-host:  cd /srv/wow/e2e && .venv/bin/python promo_run.py
"""
from __future__ import annotations

import fcntl
import json
import sys
import time
from datetime import datetime, timezone

sys.path.insert(0, ".")
from harness import Mcp, sql, wait_mcp_ready, worldserver_log_since  # noqa: E402
from run import TESTER, log, operator_online, wait_for  # noqa: E402
from wowclient.auth import logon  # noqa: E402
from wowclient.world import CHAT_PARTY, CHAT_PARTY_LEADER, CHAT_WHISPER, WorldClient  # noqa: E402

HORDE = "2,5,6,8,10"
EXCLUDE_ACCOUNTS = "1001,2006"


def pick_bot() -> tuple[int, str, int]:
    # playerbots logs every random bot out while no real player is online; they
    # come back a few minutes after the tester logs in.
    q = (f"select guid,name,level from acore_characters.characters where online=1 "
         f"and race in ({HORDE}) and account not in ({EXCLUDE_ACCOUNTS}) order by rand() limit 1;")
    rows = wait_for(lambda: sql(q), 480, poll=15)
    assert rows, "no online Horde rndbot within 8 min"
    g, n, lv = rows[0]
    return int(g), n, int(lv)


def main() -> int:
    lock = open("/tmp/wow-e2e.lock", "w")
    fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
    mcp = Mcp()
    wait_mcp_ready(mcp)
    if operator_online():
        print(json.dumps({"summary": "operator is playing — not running"}))
        return 3
    since = datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")
    res: dict = {}
    u, p = open(".creds").read().split()
    key, realms = logon("127.0.0.1", 3724, u, p)
    wc = WorldClient("127.0.0.1", int(realms[0].address.split(":")[1]), u, key, realm_id=realms[0].id, log=log)
    try:
        wc.enter_world([c for c in wc.characters() if c.name == TESTER][0])
        wc.leave_group()
        time.sleep(3)
        guid, name, lvl = pick_bot()
        res["bot"] = {"guid": guid, "name": name, "level": lvl}
        log(f"picked {name} ({guid}, lvl {lvl})")

        def tells(t, types):
            return [c.text for c in wc.chat_since(t) if c.chat_type in types and c.sender_guid == guid]

        # 1. whisper -> chat promotion + strategic reply
        t = time.time()
        wc.whisper(name, f"Hey {name}, quick question: what class are you, what level, and what weapon are you holding?")
        # playerbots' own security replies ("You are too low level: ...") are
        # colour-coded system text and arrive in <1 s; wait for the LLM's.
        hit = wc.wait_until(lambda: [x for x in tells(t, {CHAT_WHISPER}) if "|c" not in x], 120)
        res["whisper_reply"] = (hit or ["<none>"])[0][:220]
        res["whisper_latency_s"] = round(time.time() - t, 1) if hit else None
        log(f"whisper reply ({res['whisper_latency_s']} s): {res['whisper_reply']}")

        # 2. invite -> party guest (direct join fallback if the bot does not accept)
        t = time.time()
        wc.invite(name)
        joined = wait_for(lambda: mcp.gps(name).get("group"), 30, poll=3)
        if not joined:
            log("invite not accepted — admin_group_join fallback")
            res["invite_fallback"] = mcp.call("admin", "admin_group_join", {"name": name, "group_of": TESTER})
            joined = wait_for(lambda: mcp.gps(name).get("group"), 30, poll=3)
        res["joined"] = bool(joined)
        master = wait_for(lambda: mcp.gps(name).get("master") == TESTER, 30, poll=3)
        res["master_is_tester"] = bool(master)
        t = time.time()
        wc.party(f"{name}, what armor are you wearing and how much gold do you have?")
        hit = wc.wait_until(lambda: tells(t, {CHAT_PARTY, CHAT_PARTY_LEADER}), 120)
        res["party_reply"] = (hit or ["<none>"])[0][:220]

        # 3. leave -> demoted
        wc.leave_group()
        time.sleep(15)
        logtxt = worldserver_log_since(since)
        promo = [l.split("] ", 1)[-1] for l in logtxt.splitlines() if "Ollama Chat Promote" in l and name in l]
        res["promote_log"] = promo[-6:]
        res["ok"] = bool(res["whisper_latency_s"]) and res["joined"] and res["master_is_tester"] \
            and res["party_reply"] != "<none>" and any("demoted" in l for l in promo)
    finally:
        try:
            wc.leave_group()
            wc.logout()
        except Exception:
            pass
        wc.close()
    print(json.dumps(res, ensure_ascii=False, indent=1))
    return 0 if res.get("ok") else 1


if __name__ == "__main__":
    sys.exit(main())
