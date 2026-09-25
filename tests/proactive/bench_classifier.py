#!/usr/bin/env python3
"""Bench the proactive yes/no/done classifier (SC-002 target: ≥95% per class).

Mirrors the C++ `ClassifyPlayerMessage` rules in src/mod-ollama-chat_proactive.cpp
so the JSON dataset can be scored without spinning up a worldserver. If you
change the C++ rules, update this script in lockstep.

Usage:
    python3 tests/proactive/bench_classifier.py
    python3 tests/proactive/bench_classifier.py --json tests/proactive/classifier_phrases.json
"""
from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path
from typing import Iterable

DIRECT_CMD_PHRASES = (
    "follow me", "follow", "stay", "stop", "wait here", "summon",
    "attack", "kill", "engage", "eat", "drink", "rest", "repair",
    "maintenance", "wipe", "back off", "reset", "release", "hearth",
    "go home", "leave group", "leave party", "revive", "loot",
)

APPROVE_TOKENS = (
    "yes", "yep", "yeah", "ok", "okay", "sure", "aight",
    "alright", "yup", "fine", "absolutely", "definitely", "kk",
)
APPROVE_PHRASES = (
    "let's", "lets", "do it", "go for", "go ahead", "lead the",
    "sounds good", "sounds great", "i'm in", "im in", "i'm down",
    "im down", "down for",
    # Agreement reading of the bare soft decline below — "i'm good TO GO" means
    # yes. Approve is tested first, so the idiom resolves positively instead of
    # merely being spared by is_bare_soft_decline's continuation guard.
    "i'm good to go", "im good to go",
)

REJECT_TOKENS = (
    "no", "nope", "nah", "skip", "pass", "different",
)
REJECT_PHRASES = (
    "not that", "not now", "something else", "different one",
    "not really", "maybe later", "later",
    # Soft declines with no leading reject token — mirror of the C++ additions;
    # keep in lockstep. The "not <state>" hedges (not done/finished/ready/sure)
    # still resolve to 'none' via the discourse-override pass, which runs above.
    "not interested", "not for me", "not right now",
    "rather not", "i'd rather not", "id rather not", "maybe not",
    # Was absent bare OR prefixed, so "not today" / "yeah not today" both landed
    # on 'none'. No benign reading as an answer to a proposal, unlike the
    # "not <state>" hedges the discourse-override pass keeps at 'none'.
    "not today",
)

# Bare soft declines carrying NO reject token — mirror of the C++
# IsBareSoftDecline stems; keep in lockstep.
BARE_DECLINE_STEMS = (
    "i'm good", "im good", "i'm all set", "im all set",
)

DONE_TOKENS = (
    "done", "finished", "completed", "next",
)
DONE_PHRASES = (
    "what's next", "whats next", "next one", "ok next",
    "we're done", "were done", "turned it", "i'm done", "im done",
    "i'm finished", "im finished",
)

# Discourse-override phrases: messages starting with these are returned as
# 'none' BEFORE the intent token matchers run. Mirror of the override list in
# C++ ClassifyPlayerMessage; keep in lockstep.
DISCOURSE_OVERRIDE_PHRASES = (
    # "no <distancing>"
    "no idea", "no problem", "no rush", "no clue",
    "no worries", "no offense", "no way",
    # "not <state>" (not done / finished / ready / sure)
    "not done", "not finished", "not ready", "not sure",
    "i'm not done", "im not done",
    "i'm not finished", "im not finished",
    # "let's <hedge>"
    "let's wait", "lets wait", "let's see", "lets see",
    "let's not", "lets not", "let's hold", "lets hold",
    "let's think", "lets think",
    # "next <gameplay-noun>"
    "next time", "next pull", "next mob",
    "next group", "next fight",
    # Discourse markers — "<approve-token> but ..." contrastive openers.
    # "ok but"/"okay but" complete the set (sure/alright/yeah were already here);
    # "yeah no"/"yeah nah" are the "yeah"-prefixed reversal idiom (still mean no).
    "ok so", "okay so", "sure but", "alright but", "yeah but",
    "ok but", "okay but",
    # Remaining approve tokens (yes/yep/yup/fine/aight) + sounds good/great.
    "yes but", "yep but", "yup but", "fine but", "aight but",
    "sounds good but", "sounds great but",
    "yeah no", "yeah nah",
)

# Mirror of the C++ StripAffirmativeNegationOpener sets; keep in lockstep.
# AFFIRMATIVE_OPENERS is deliberately identical to APPROVE_TOKENS — an opener
# only needs stripping because it would otherwise win the firstThree matcher.
AFFIRMATIVE_OPENERS = APPROVE_TOKENS
NEGATION_HEADS = ("not", "no", "nope", "nah")
BENIGN_AFTER_NEGATION = ("a", "an")


def tokenize_lower(s: str, max_tokens: int) -> list[str]:
    out: list[str] = []
    cur = ""
    for ch in s:
        if ch.isalnum() or ch == "'":
            cur += ch.lower()
        else:
            if cur:
                out.append(cur)
                cur = ""
            if len(out) >= max_tokens:
                break
    if cur:
        out.append(cur)
    return out[:max_tokens]


def contains_any_token(tokens: Iterable[str], wanted: Iterable[str]) -> bool:
    wanted_set = set(wanted)
    return any(t in wanted_set for t in tokens)


def starts_with_phrase(lower: str, phrases: Iterable[str]) -> bool:
    for p in phrases:
        if lower.startswith(p):
            if len(lower) == len(p):
                return True
            nxt = lower[len(p)]
            if not nxt.isalnum():
                return True
    return False


def collapse_punct_to_space(lower: str) -> str:
    """Mirror of C++ CollapsePunctToSpace: collapse every run of non-alnum
    (apostrophe preserved) to a single space, dropping leading/trailing
    separators. Used for the discourse-override pass so punctuated contrastive
    forms ('fine, but ...', 'yeah, no') match the single-space override entries
    instead of slipping through to the Approve matcher."""
    out: list[str] = []
    pending_space = False
    for ch in lower:
        if ch.isalnum() or ch == "'":
            if pending_space and out:
                out.append(" ")
            pending_space = False
            out.append(ch)
        else:
            pending_space = True
    return "".join(out)


def strip_affirmative_negation_opener(normalized: str) -> str:
    """Mirror of C++ StripAffirmativeNegationOpener.

    A message can OPEN with an agreement token and immediately negate it
    ('yeah, not right now' / 'ok not now' / 'absolutely no'). The firstThree
    Approve matcher runs before Reject, so the leading affirmative wins and the
    decline classifies as APPROVE — the bot executes an activity the player just
    refused. Enumerating the forms is a cross-product (13 approve tokens x every
    decline body), so strip the opener instead and let the existing vocabulary
    judge the remainder.

    Returns the remainder, or '' when the pattern does not apply (in which case
    every downstream matcher sees the original message, unchanged).

    'not a/an <noun>' is a benign agreement ('sure, not a problem'), so an
    article right after the negation suppresses the strip.
    """
    parts = normalized.split(" ")
    if len(parts) < 2:
        return ""
    if parts[0] not in AFFIRMATIVE_OPENERS:
        return ""
    if parts[1] not in NEGATION_HEADS:
        return ""
    if len(parts) >= 3 and parts[2] in BENIGN_AFTER_NEGATION:
        return ""
    return " ".join(parts[1:])


def strip_interrogative_negation_opener(normalized: str) -> tuple[bool, str]:
    """Mirror of C++ StripInterrogativeNegationOpener.

    'why not' is a rhetorical question that answers YES, but it carries neither
    an approve token nor an approve phrase, so it fell through to 'none' and the
    approval never registered. It also cannot be closed by adding 'not' to
    REJECT_TOKENS — contains_any_token scans the first three tokens, so a bare
    'not' would false-Reject this very phrase.

    Enumerating it as an approve PHRASE would be wrong the other way: the
    rhetorical question can carry a body that reverses it ('why not skip it'),
    and Approve is tested before Reject. So strip the opener and let the
    existing vocabulary judge the remainder; an UNMATCHED remainder (including
    the bare 'why not') is what makes it an approval, resolved last.

    Returns (fired, remainder). remainder may be '' for the bare form, which is
    why the fired flag is separate.
    """
    opener = "why not"
    if not normalized.startswith(opener):
        return False, ""
    if len(normalized) == len(opener):
        return True, ""
    if normalized[len(opener)] != " ":
        return False, ""            # word boundary — 'why nothing', 'why nots'
    return True, normalized[len(opener) + 1:]


def is_bare_soft_decline(normalized: str) -> bool:
    """Mirror of C++ IsBareSoftDecline.

    Polite refusals with none of REJECT_TOKENS and none of the 'not <x>'
    REJECT_PHRASES, so they fell through to 'none': the reject never registered
    and the proposal lingered Open to the ~600s expiry (FR-005
    same-proposal-twice family).

    Needs its own predicate rather than another REJECT_PHRASES entry because a
    naive "i'm good" entry also captures "i'm good to go" — an APPROVAL. A
    'to <verb>' continuation reverses the reading and suppresses the decline,
    the same way 'not a/an <noun>' suppresses the reversal strip above.
    """
    for stem in BARE_DECLINE_STEMS:
        if not normalized.startswith(stem):
            continue
        if len(normalized) == len(stem):
            return True             # exact — "i'm good"
        if normalized[len(stem)] != " ":
            continue                # word boundary — "im goods"
        # "to <verb>" reverses it: "i'm good to go" / "i'm good to start".
        if starts_with_phrase(normalized[len(stem) + 1:], ("to",)):
            return False
        return True                 # "i'm good thanks" / "i'm all set sorry"
    return False


def strip_affirmative_opener(normalized: str) -> tuple[bool, str]:
    """Mirror of the C++ StripAffirmativeOpener; keep in lockstep.

    Strips a LONE affirmative turn-opener (no negation head required, unlike
    strip_affirmative_negation_opener). The caller gates the result on
    is_bare_soft_decline, so the rule fires only on '<affirmative> i'm good'
    and friends; 'yeah i'm good to go' keeps the continuation carve-out for
    free and stays Approve.
    """
    sp = normalized.find(" ")
    if sp == -1:
        return False, ""
    if normalized[:sp] not in AFFIRMATIVE_OPENERS:
        return False, ""
    return True, normalized[sp + 1:]


def classify(message: str, has_open_proposal: bool, has_executing_plan: bool) -> str:
    """Return one of: 'approve', 'reject', 'done', 'directcmd', 'none'."""
    if not message or len(message) > 256:
        return "none"
    lower = message.lower()
    # Punctuation-normalized view for the discourse-override pass (mirrors C++).
    normalized = collapse_punct_to_space(lower)

    if starts_with_phrase(lower, DIRECT_CMD_PHRASES):
        return "directcmd"

    if starts_with_phrase(normalized, DISCOURSE_OVERRIDE_PHRASES):
        return "none"

    # Affirmative-opener reversal. The override list is re-consulted on the
    # remainder so 'yeah not done yet' still reaches the 'not done' hedge.
    reversal = strip_affirmative_negation_opener(normalized)
    if reversal and starts_with_phrase(reversal, DISCOURSE_OVERRIDE_PHRASES):
        return "none"

    # Interrogative-negation agreement — 'why not' answers YES. Same
    # strip-and-re-judge rule, so a body that flips it ('why not skip it') still
    # reaches the reject vocabulary; the bare form resolves at the BOTTOM of the
    # open-proposal block, once nothing else has claimed it.
    why_not, interrogative = (False, "")
    if not reversal:
        why_not, interrogative = strip_interrogative_negation_opener(normalized)
    if why_not and starts_with_phrase(interrogative, DISCOURSE_OVERRIDE_PHRASES):
        return "none"

    # `view` keeps the raw lowercase message in the common case so the prefix
    # matchers are byte-identical; `normalized_view` is the punctuation-collapsed
    # form, for matchers that inspect a CONTINUATION token (is_bare_soft_decline).
    if reversal:
        view = normalized_view = reversal
    elif why_not:
        view = normalized_view = interrogative
    else:
        view, normalized_view = lower, normalized

    first_three = tokenize_lower(view, 3)
    first_five = tokenize_lower(view, 5)

    if has_open_proposal:
        # Affirmative opener in front of a bare soft decline ('yeah i'm good').
        # MUST precede the Approve matcher — the leading token would otherwise
        # win first_three before the decline is examined.
        opened, soft_decline = strip_affirmative_opener(normalized_view)
        if opened and is_bare_soft_decline(soft_decline):
            return "reject"
        if contains_any_token(first_three, APPROVE_TOKENS):
            return "approve"
        if starts_with_phrase(view, APPROVE_PHRASES):
            return "approve"
        if contains_any_token(first_three, REJECT_TOKENS):
            return "reject"
        if starts_with_phrase(view, REJECT_PHRASES):
            return "reject"
        if is_bare_soft_decline(normalized_view):
            return "reject"
        # 'why not' with nothing that declines it IS the approval — last, so the
        # remainder was offered to every reject matcher above first.
        if why_not:
            return "approve"

    if has_executing_plan:
        if contains_any_token(first_five, DONE_TOKENS):
            return "done"
        if starts_with_phrase(view, DONE_PHRASES):
            return "done"

    return "none"


def main() -> int:
    parser = argparse.ArgumentParser()
    default_path = Path(__file__).parent / "classifier_phrases.json"
    parser.add_argument("--json", type=Path, default=default_path)
    parser.add_argument("--verbose", action="store_true", help="Show per-phrase results.")
    args = parser.parse_args()

    if not args.json.exists():
        print(f"error: dataset not found: {args.json}", file=sys.stderr)
        return 2

    data = json.loads(args.json.read_text())
    phrases = data.get("phrases", [])

    per_label: dict[str, dict[str, int]] = {}
    miscls: list[tuple[str, str, str]] = []

    for item in phrases:
        label = item["label"]
        text = item["text"]

        if label == "approve" or label == "reject":
            has_open, has_exec = True, False
        elif label == "done":
            has_open, has_exec = False, True
        elif label == "directcmd":
            has_open, has_exec = True, True
        else:  # none
            has_open, has_exec = True, True

        got = classify(text, has_open, has_exec)
        stat = per_label.setdefault(label, {"total": 0, "correct": 0})
        stat["total"] += 1
        if got == label:
            stat["correct"] += 1
            if args.verbose:
                print(f"  OK  {label:9}  '{text}'")
        else:
            miscls.append((label, got, text))
            if args.verbose:
                print(f"  --  {label:9}  expected={label} got={got}  '{text}'")

    print(f"\n{args.json.name} — proactive classifier accuracy\n")
    overall_total = 0
    overall_correct = 0
    fail_classes = []
    for lbl in ("approve", "reject", "done", "directcmd", "none"):
        s = per_label.get(lbl)
        if not s:
            continue
        pct = (s["correct"] / s["total"]) * 100 if s["total"] else 0
        marker = "PASS" if pct >= 95.0 else "FAIL"
        if pct < 95.0:
            fail_classes.append(lbl)
        print(f"  {lbl:9}  {s['correct']:3}/{s['total']:3}  {pct:6.2f}%  {marker}")
        overall_total += s["total"]
        overall_correct += s["correct"]

    overall_pct = (overall_correct / overall_total * 100) if overall_total else 0
    print(f"\n  overall   {overall_correct:3}/{overall_total:3}  {overall_pct:6.2f}%")

    if miscls and not args.verbose:
        print(f"\n  {len(miscls)} misclassified phrases (rerun with --verbose to list all):")
        for lbl, got, text in miscls[:10]:
            print(f"    expected={lbl} got={got}  '{text}'")

    if fail_classes:
        print(f"\nFAIL: SC-002 (≥95%) missed for: {', '.join(fail_classes)}")
        return 1
    print("\nPASS: all classes ≥95% (SC-002)")
    return 0


if __name__ == "__main__":
    sys.exit(main())
