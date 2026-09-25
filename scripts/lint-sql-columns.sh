#!/usr/bin/env bash
# Lint for known AzerothCore table/column gotchas in module SQL strings.
#
# Background: PR #158 (2026-05-23) — `mod-ollama-chat_proactive.cpp:974`
# issued `SELECT entry FROM quest_template …`, but AC's `quest_template`
# uses column `ID`, not `entry`. The bad SQL trips `[1054] Unknown column`
# → `Acore::Abort` → worldserver kill every ~15 min. This script greps for
# those specific known-bad patterns. It is a CHEAP, NARROW backstop — the
# real defense is `scripts/smoke-sql-against-live-db.sh` (live schema
# validation). Keep this list short and high-signal.
#
# Implemented in Python so the multi-line regex handling works identically
# on macOS (BSD grep) and Linux CI runners (GNU grep).
#
# Exit 0 = clean. Exit 1 = at least one bad pattern found.

set -euo pipefail
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

exec python3 - "$REPO_ROOT" <<'PY'
import os
import re
import sys

REPO_ROOT = sys.argv[1]
SRC_DIR = os.path.join(REPO_ROOT, "src")

# (description, pattern_with_re.DOTALL_semantics)
# Patterns must be compiled with re.DOTALL | re.IGNORECASE so they can match
# SQL split across multiple C++ string literals (the typical case is one
# `"... " "..."` chain inside a fmt::format() call).
CHECKS = [
    (
        "quest_template uses column 'ID', not 'entry' "
        "(this killed worldserver on 2026-05-23 — PR #158)",
        r"(SELECT\s+entry\s+FROM\s+quest_template"
        r"|FROM\s+quest_template[^;]{0,400}ORDER\s+BY[^;]{0,200}\bentry\b"
        r"|FROM\s+quest_template[^;]{0,400}WHERE[^;]{0,200}\bentry\s*=)",
    ),
    (
        "gossip_menu uses column 'MenuID', not 'id' or 'entry'",
        r"(SELECT\s+(id|entry)\s+FROM\s+gossip_menu\b"
        r"|FROM\s+gossip_menu\b[^;]{0,400}WHERE[^;]{0,200}\b(id|entry)\s*=)",
    ),
    (
        "quest_template uses column 'QuestSortID', not 'ZoneOrSort' "
        "(killed worldserver on 2026-05-23 — PR #159, same incident family as #158)",
        r"FROM\s+quest_template\b[^;]{0,400}WHERE[^;]{0,200}\bZoneOrSort\s*=",
    ),
]

# Walk only the files we care about. SQL lives in .cpp/.h/.hpp/.sql.
def walk():
    for dirpath, dirnames, filenames in os.walk(SRC_DIR):
        # Skip build artifacts and any hidden dirs.
        dirnames[:] = [d for d in dirnames if not d.startswith(".") and d != "build"]
        for name in filenames:
            if name.endswith((".cpp", ".h", ".hpp", ".sql")):
                yield os.path.join(dirpath, name)

fail = False
for desc, pat in CHECKS:
    rx = re.compile(pat, re.DOTALL | re.IGNORECASE)
    for path in walk():
        try:
            with open(path, "r", encoding="utf-8", errors="replace") as f:
                src = f.read()
        except OSError:
            continue
        m = rx.search(src)
        if m:
            if not fail:
                print(f"::error::Lint failure — {desc}", flush=True)
            else:
                print(f"::error::Also: {desc}", flush=True)
            snippet = m.group(0).replace("\n", " ⏎ ")[:200]
            line_no = src.count("\n", 0, m.start()) + 1
            rel = os.path.relpath(path, REPO_ROOT)
            print(f"  {rel}:{line_no}")
            print(f"      ↳ {snippet}")
            fail = True

if not fail:
    print("lint-sql-columns: no known bad patterns found")
sys.exit(1 if fail else 0)
PY
