#!/usr/bin/env python3
"""Calibration bench for the jev decision tier.

Replays labelled phrases through the SAME questions the C++ sends (wording from
prompts/jev_questions.json, option sets mirroring the C++ allowlists) against the
Decisions API, then prints per-class accuracy, a confidence histogram and the
accuracy-vs-threshold curve so `OllamaChat.Jev.<Site>.MinConfidence` is READ OFF
THE CURVE rather than copied from the legacy self-reported knobs.

Defaults to TypeSafe's native endpoint with the key from TYPESAFE_API_KEY. Override with JEV_URL / JEV_MODEL / JEV_API_KEY or
--url / --model / --key-file (a bare key, or a NAME=value env file) — e.g. for the OpenRouter
route (`--url https://openrouter.ai/api/alpha/decisions --model typesafe/jev-1.13`; note that route
is geo-blocked from game-host's egress, run it from the Mac).

    python3 tests/jev/bench_jev.py --site intent                 # 340 proactive phrases
    python3 tests/jev/bench_jev.py --site classifier             # tests/jev/classifier_phrases.json
    python3 tests/jev/bench_jev.py --site intent --limit 40 --dry-run   # print requests only
    python3 tests/jev/bench_jev.py --site intent --replay rows.json      # re-score a saved run offline

Cost: ~350 input tokens per intent request at $0.042/M -> the full 340-phrase
run is ~$0.005. Requests are sequential to stay well under rate limits.
"""
from __future__ import annotations

import argparse
import collections
import json
import os
import sys
import time
import urllib.error
import urllib.request
from pathlib import Path

REPO = Path(__file__).resolve().parents[2]
QUESTIONS_FILE = REPO / "prompts" / "jev_questions.json"
DEFAULT_URL = "https://api.typesafe.ai/v1/systemone"   # mirrors OllamaChat.Jev.Url's default
DEFAULT_MODEL = "jev-1.13.0"                            # mirrors OllamaChat.Jev.Model's default

# Mirrors src/mod-ollama-chat_gateway.cpp kClassifierAllowedActions (50) — keep in lockstep.
CLASSIFIER_ACTIONS = sorted([
    "emergency_stop",
    "bot_follow", "bot_stay", "bot_emote", "bot_revive", "bot_revive_target", "bot_combat_rez",
    "bot_self_reincarnate", "bot_accept_resurrect_request",
    "bot_rti", "bot_set_raid_target_icon", "bot_disperse",
    "bot_attack_target", "bot_stop_combat", "bot_flee",
    "bot_accept_quest", "bot_turn_in_quest", "bot_autogear", "bot_maintenance",
    "bot_train_spells", "bot_sell_junk", "bot_pet_command",
    "bot_roll", "bot_set_loot_filter", "bot_add_loot_item",
    "bot_remove_loot_item", "bot_invite_to_group", "bot_convert_to_raid",
    "bot_set_group_leader", "bot_uninvite_from_group", "bot_disband_group", "bot_set_loot_method",
    "bot_set_raid_subgroup", "bot_set_raid_assistant", "bot_raid_ready_check", "bot_set_home",
    "bot_say", "bot_yell", "bot_trade_give", "bot_accept_trade", "bot_guild_create",
    "bot_accept_guild_invite", "bot_guild_leave", "bot_split_stack", "bot_stack_combine",
    "bot_ah_post", "bot_ah_bid", "bot_ah_cancel",
    "leader_command", "leader_command_all",
]) + ["unknown"]
COMMANDS = ["attack my target", "follow", "stay", "accept *", "talk *", "summon", "eat", "drink",
            "maintenance", "trainer learn", "u 6948", "release", "leave", "none"]
ICONS = ["skull", "cross", "circle", "star", "square", "triangle", "diamond", "moon", "none"]
LOOT = ["all", "normal", "gray", "disenchant"]
PET = ["follow", "stay", "passive", "defensive", "aggressive", "none"]


# Mirrors kJevClassifierFillable in src/mod-ollama-chat_gateway.cpp — the actions the jev tier may
# dispatch on its own. Anything else jev picks is a fall-through to the LLM tier, so for the
# threshold curve it counts as NOT kept, exactly like a low-confidence answer.
CLASSIFIER_ELIGIBLE = {
    "leader_command_all", "leader_command", "emergency_stop", "bot_revive", "bot_rti",
    "bot_set_loot_filter", "bot_pet_command", "bot_roll", "bot_sell_junk", "bot_autogear",
    "bot_set_home", "bot_accept_resurrect_request", "bot_stop_combat", "bot_flee",
    "bot_accept_quest", "bot_turn_in_quest", "bot_maintenance", "bot_train_spells",
    "bot_follow", "bot_stay",
}
# Production per-call cap for site A (see TryJevIntentClassify): answers slower than this would
# have been a timeout in the worldserver, so the curve treats them as fall-throughs too.
CLASSIFIER_CAP_MS = 2000


def classifier_acceptance(row: dict, threshold: float) -> tuple[bool, bool]:
    """(acted, correct) under the C++ acceptance predicate, not just action equality.

    acted    = jev's answer would have been dispatched by TryJevIntentClassify at `threshold`:
               eligible action, action confidence >= threshold, every consumed slot (the raw
               command for leader_command*, the pet stance for bot_pet_command, the icon for
               bot_rti, the loot mode) also >= threshold and not `none`, and within the cap.
    correct  = acted AND the action matches AND (for leader_command*) the command matches.
    """
    got = row["got"]
    if got == "unknown" or got not in CLASSIFIER_ELIGIBLE or row["confidence"] < threshold:
        return False, False
    if row.get("latency_ms", 0) > CLASSIFIER_CAP_MS:
        return False, False
    slot_ok = True
    if got.startswith("leader_command"):
        slot_ok = row.get("command") not in (None, "none") and row.get("command_confidence", 0.0) >= threshold
    elif got == "bot_pet_command":
        slot_ok = row.get("pet_command") not in (None, "none") and row.get("pet_command_confidence", 0.0) >= threshold
    elif got == "bot_rti":
        slot_ok = row.get("icon_confidence", 0.0) >= threshold
    elif got == "bot_set_loot_filter":
        slot_ok = row.get("loot_filter_confidence", 0.0) >= threshold
    if not slot_ok:
        return False, False
    correct = got == row["expect"]
    if correct and got.startswith("leader_command"):
        correct = row.get("expect_command") in (None, row.get("command"))
    return True, correct


def choice_from_template(tmpl: dict | None, options: list[str]) -> dict:
    """Same rule as Jev::ChoiceFromTemplate: option SET from code, wording from the file."""
    instructions = (tmpl or {}).get("instructions", "Pick the best option.")
    crit = (tmpl or {}).get("criteria", {}) or {}
    return {"type": "choice", "instructions": instructions,
            "criteria": {o: crit.get(o, o) for o in options}}


def load_questions() -> dict:
    return json.loads(QUESTIONS_FILE.read_text(encoding="utf-8"))


def intent_cases() -> list[dict]:
    data = json.loads((REPO / "tests" / "proactive" / "classifier_phrases.json").read_text(encoding="utf-8"))
    cases = []
    for p in data["phrases"]:
        label = p["label"]
        # The dataset's _state_assumption: approve/reject -> open proposal; done -> executing plan;
        # none -> neither (the bench runs those with both flags true to exercise both sides);
        # directcmd -> the C++ short-circuits these above the LLM, expected `other` here.
        if label in ("approve", "reject"):
            state = {"message": p["text"], "hasOpenProposal": True, "hasExecutingPlan": False}
            expect = label
        elif label == "done":
            state = {"message": p["text"], "hasOpenProposal": False, "hasExecutingPlan": True}
            expect = "done"
        else:
            state = {"message": p["text"], "hasOpenProposal": True, "hasExecutingPlan": True}
            expect = "other"
        cases.append({"state": state, "expect": expect, "text": p["text"], "src_label": label})
    return cases


def intent_questions(q: dict) -> dict:
    return {"intent": choice_from_template(q["intent"]["intent"], ["approve", "reject", "done", "other"])}


def classifier_cases() -> list[dict]:
    data = json.loads((REPO / "tests" / "jev" / "classifier_phrases.json").read_text(encoding="utf-8"))
    cases = []
    for p in data["phrases"]:
        state = {"message": p["text"], "humanName": data.get("_humanName", "Slayo"),
                 "leaderGroupedWithHuman": True, "botNames": data.get("_botNames", ["Clawd", "Geek"])}
        cases.append({"state": state, "expect": p["action"], "expect_command": p.get("command"),
                      "text": p["text"], "src_label": p["action"]})
    # Sanity: the eligible set must be a subset of the option set the C++ sends.
    assert CLASSIFIER_ELIGIBLE <= set(CLASSIFIER_ACTIONS), CLASSIFIER_ELIGIBLE - set(CLASSIFIER_ACTIONS)
    return cases


def classifier_questions(q: dict) -> dict:
    c = q["classifier"]
    return {
        "action": choice_from_template(c["action"], CLASSIFIER_ACTIONS),
        "command": choice_from_template(c["command"], COMMANDS),
        "icon": choice_from_template(c["icon"], ICONS),
        "loot_filter": choice_from_template(c["loot_filter"], LOOT),
        "pet_command": choice_from_template(c["pet_command"], PET),
    }


def call(url: str, key: str, model: str, state: dict, questions: dict, timeout: float) -> tuple[dict, float]:
    body = json.dumps({"model": model, "state": state, "questions": questions}).encode()
    req = urllib.request.Request(url, data=body, method="POST", headers={
        "Authorization": f"Bearer {key}", "Content-Type": "application/json",
        "HTTP-Referer": "https://github.com/synthiq-ai/synthiqbots", "X-Title": "mod-ollama-chat bench"})
    t0 = time.perf_counter()
    for attempt in range(3):
        try:
            with urllib.request.urlopen(req, timeout=timeout) as resp:
                return json.loads(resp.read().decode()), (time.perf_counter() - t0) * 1000
        except urllib.error.HTTPError as e:
            if e.code in (429, 529) and attempt < 2:
                time.sleep(2 ** attempt)
                continue
            raise SystemExit(f"HTTP {e.code}: {e.read().decode()[:300]}")
    raise SystemExit("unreachable")


PLAYBOOK_FILE = REPO / "prompts" / "gateway_leader.md"
PLAYBOOK_DEFAULT_MESSAGES = [
    "attack", "everyone follow me", "Clawd hearth", "where is Raz", "mark skull",
    "share your quest with the party", "sell your junk and repair", "how are you doing today",
]


def parse_playbook(text: str) -> tuple[str, list[dict], str]:
    """Mirror of Jev::ParsePlaybook: header, rows [{prefix, full}], footer."""
    header, rows, footer, seen = [], [], [], False
    for line in text.split("\n"):
        line = line.rstrip("\r")
        if line.startswith('- "'):
            seen = True
            arrow = line.find(" -> ")
            prefix = line[2:arrow] if arrow >= 0 else line[2:]
            rows.append({"prefix": prefix, "full": line})
        elif not seen:
            header.append(line)
        elif line:
            footer.append(line)
    return "\n".join(header), rows, "\n".join(footer)


def run_playbook(args, q: dict, key: str) -> int:
    """Score every playbook row against each message — exactly what Jev::FilterLeaderPlaybook sends."""
    _, rows, _ = parse_playbook(PLAYBOOK_FILE.read_text(encoding="utf-8"))
    tmpl = q.get("playbook", {}).get("row", {})
    question = tmpl.get("question", "Is this workflow what the player is asking for?")
    # Mirrors Jev::FilterLeaderPlaybook: shared framing ONCE in the state, one bare noul per row.
    questions = {f"r{i}": {"type": "noul", "instructions": {"workflow_triggers": r["prefix"]}}
                 for i, r in enumerate(rows)}
    framing = {"task": question, "yes_means": tmpl.get("true", ""), "no_means": tmpl.get("false", "")}
    msgs = args.message or PLAYBOOK_DEFAULT_MESSAGES
    print(f"{len(rows)} rows -> {len(questions)} nouls per request")
    if args.dry_run:
        print(json.dumps({"model": args.model, "state": {"message": msgs[0], **framing},
                          "questions": dict(list(questions.items())[:2])}, indent=2)[:2500])
        return 0
    for m in msgs:
        resp, ms = call(args.url, key, args.model, {"message": m, **framing}, questions, args.timeout)
        rel = [float(resp["answers"][f"r{i}"]["noul"]) for i in range(len(rows))]
        ranked = sorted(range(len(rows)), key=lambda i: -rel[i])
        n03 = sum(1 for x in rel if x >= 0.3)
        n05 = sum(1 for x in rel if x >= 0.5)
        usage = resp.get("usage", {})
        print(f"\n=== {m!r}  {ms:.0f}ms  in_tok={usage.get('input_tokens')}  rows>=0.3: {n03}  >=0.5: {n05}")
        for i in ranked[:5]:
            print(f"  p={rel[i]:.2f} r{i:<2d} {rows[i]['prefix'][:110]}")
    return 0


def threshold_curve(rows: list[dict], site: str) -> None:
    """kept = would have been ACTED ON in production; wrong-acted is the regression count.

    For the classifier the predicate is classifier_acceptance (eligibility + every consumed
    slot's confidence + the cap); for intent an `other` answer never acts (the C++ matcher
    runs), so only approve/reject/done above the threshold count as acted.
    """
    print("\nthreshold  kept%  acc(kept)  wrong-acted  fell-through")
    for th in [0.0, 0.5, 0.6, 0.7, 0.8, 0.9, 0.95]:
        acted = wrong = 0
        for r in rows:
            if site == "classifier":
                a, ok = classifier_acceptance(r, th)
            else:
                a = r["got"] != "other" and r["confidence"] >= th
                ok = a and r["correct"]
            acted += a
            wrong += a and not ok
        acc = (acted - wrong) / acted if acted else 0.0
        print(f"  {th:4.2f}    {100*acted/len(rows):5.1f}   {100*acc:6.1f}      {wrong:4d}        {len(rows)-acted:4d}")


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--site", choices=["intent", "classifier", "playbook"], required=True)
    ap.add_argument("--message", action="append", default=None,
                    help="playbook site: message(s) to score the playbook rows against (repeatable); "
                         "default = a built-in set of 8 leader orders")
    ap.add_argument("--url", default=os.environ.get("JEV_URL", DEFAULT_URL))
    ap.add_argument("--model", default=os.environ.get("JEV_MODEL", DEFAULT_MODEL))
    ap.add_argument("--key-file", default=None, help="file holding the bearer key (0600)")
    ap.add_argument("--limit", type=int, default=0)
    ap.add_argument("--timeout", type=float, default=15.0)
    ap.add_argument("--dry-run", action="store_true")
    ap.add_argument("--json-out", default=None, help="write per-case rows here")
    ap.add_argument("--replay", default=None, help="re-score a saved --json-out file offline (no API calls)")
    args = ap.parse_args()

    if args.replay:
        rows = json.loads(Path(args.replay).read_text(encoding="utf-8"))
        threshold_curve(rows, args.site)
        return 0

    key = os.environ.get("JEV_API_KEY") or os.environ.get("TYPESAFE_API_KEY", "")
    if args.key_file:
        raw = Path(args.key_file).read_text().strip()
        # Accept a bare key or a NAME=value env file (~/.config/typesafe/env style).
        key = raw.split("=", 1)[1].strip() if "=" in raw.splitlines()[0] else raw
    if not key and not args.dry_run:
        raise SystemExit("no key: set TYPESAFE_API_KEY / JEV_API_KEY or --key-file")

    q = load_questions()
    if args.site == "playbook":
        return run_playbook(args, q, key)
    if args.site == "intent":
        cases, questions, answer_id = intent_cases(), intent_questions(q), "intent"
    else:
        cases, questions, answer_id = classifier_cases(), classifier_questions(q), "action"
    if args.limit:
        cases = cases[:args.limit]

    if args.dry_run:
        print(json.dumps({"model": args.model, "state": cases[0]["state"], "questions": questions}, indent=2)[:4000])
        print(f"\n{len(cases)} cases")
        return 0

    rows, latencies, tokens, cost = [], [], 0, 0.0
    for i, c in enumerate(cases, 1):
        resp, ms = call(args.url, key, args.model, c["state"], questions, args.timeout)
        a = resp["answers"][answer_id]
        got, conf = a["choice"], float(a.get("confidence", 0.0))
        correct = got == c["expect"]
        row = {"text": c["text"], "src_label": c["src_label"], "expect": c["expect"], "got": got,
               "confidence": conf, "correct": correct, "latency_ms": round(ms), "probabilities": a.get("probabilities")}
        if args.site == "classifier":
            for slot in ("command", "pet_command", "icon", "loot_filter"):
                a = resp["answers"][slot]
                row[slot] = a["choice"]
                row[f"{slot}_confidence"] = float(a.get("confidence", 0.0))
            row["expect_command"] = c.get("expect_command")
            row["command_correct"] = (c.get("expect_command") in (None, row["command"]))
        rows.append(row)
        latencies.append(ms)
        usage = resp.get("usage", {})
        tokens += int(usage.get("input_tokens", 0))
        cost += float(usage.get("cost", 0.0))
        mark = "ok " if correct else "XX "
        print(f"{mark}{i:3d}/{len(cases)} {c['expect']:9s} got={got:20s} conf={conf:.2f} {ms:6.0f}ms  {c['text'][:60]!r}")

    print("\n=== per-class accuracy (no threshold) ===")
    by = collections.defaultdict(lambda: [0, 0])
    for r in rows:
        by[r["expect"]][0] += r["correct"]
        by[r["expect"]][1] += 1
    for k, (ok, n) in sorted(by.items()):
        print(f"  {k:10s} {ok:4d}/{n:<4d} {100*ok/n:5.1f}%")
    total_ok = sum(r["correct"] for r in rows)
    print(f"  {'ALL':10s} {total_ok:4d}/{len(rows):<4d} {100*total_ok/len(rows):5.1f}%")
    if args.site == "classifier":
        cmd_ok = sum(1 for r in rows if r["command_correct"])
        print(f"  command slot: {cmd_ok}/{len(rows)}")

    print("\n=== confidence histogram (correct | wrong) ===")
    bins = [(0.0, 0.5), (0.5, 0.6), (0.6, 0.7), (0.7, 0.8), (0.8, 0.9), (0.9, 1.01)]
    for lo, hi in bins:
        inb = [r for r in rows if lo <= r["confidence"] < hi]
        print(f"  [{lo:.1f},{hi if hi <= 1 else 1.0:.1f}) {sum(r['correct'] for r in inb):4d} | {sum(not r['correct'] for r in inb):3d}")
    threshold_curve(rows, args.site)

    lat = sorted(latencies)
    print(f"\nlatency ms: p50={lat[len(lat)//2]:.0f} p90={lat[int(len(lat)*0.9)]:.0f} max={lat[-1]:.0f}  "
          f"input tokens total={tokens} (avg {tokens/len(rows):.0f})  cost=${cost:.5f}  model={resp.get('model')}")

    if args.json_out:
        Path(args.json_out).write_text(json.dumps(rows, indent=1), encoding="utf-8")
        print(f"rows -> {args.json_out}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
