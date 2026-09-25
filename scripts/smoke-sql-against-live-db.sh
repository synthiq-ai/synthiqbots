#!/usr/bin/env bash
# Validate every module SQL query string against the live AzerothCore schema.
#
# For each query emitted by `extract-module-sql.py`, run `EXPLAIN <sql>`
# against the ac-database container. EXPLAIN parses the query identically
# to a real SELECT/INSERT/UPDATE/DELETE (binding tables, columns, indexes)
# but executes nothing — so column-name typos, missing tables, and broken
# JOINs all surface as MySQL errors. Read-only, safe to run in CI.
#
# Runs on the game-host self-hosted runner where `ac-database` is already
# up and exposes mysql on the local docker socket. The runner picks the
# right database per query by inspecting the table names — most module
# tables live in `acore_characters`, the proactive in-zone scan and a few
# others hit `acore_world`. We make EXPLAIN explicit with `USE <db>;`
# prefixes derived from a hard-coded table → db map. Tables we don't
# recognize default to `acore_world`.
#
# Background: this exists because PR #158 (2026-05-23) shipped a query
# with the wrong PK column name (`entry` on `quest_template`, which uses
# `ID`). The bad SQL killed worldserver every ~15 min, and the existing
# CI compile + 4-min ready check missed it entirely. With this smoke
# step the bug would have failed the PR build instead of shipping.
#
# Exit 0 = every query parsed OK. Exit 1 = at least one MySQL error.

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
EXTRACTOR="$REPO_ROOT/scripts/extract-module-sql.py"
DB_CONTAINER="${DB_CONTAINER:-ac-database}"
DB_USER="${DB_USER:-root}"
DB_PASS="${DB_PASS:-password}"

if [ ! -f "$EXTRACTOR" ]; then
    echo "::error::extract-module-sql.py not found at $EXTRACTOR"
    exit 2
fi

# Skip silently if ac-database isn't reachable — this script is meant to
# run on the game-host runner where the DB is always up. In other contexts
# (local dev, GitHub-hosted runners) the smoke is a no-op rather than a
# false failure.
if ! docker inspect -f '{{.State.Status}}' "$DB_CONTAINER" 2>/dev/null | grep -q running; then
    echo "::notice::$DB_CONTAINER is not running; skipping smoke (no-op)."
    exit 0
fi

# Table → schema map. Anything not listed defaults to acore_world.
# Keep this short and explicit — it tracks the SCHEMA each module SQL
# actually queries, not every AC table in existence.
pick_db() {
    local sql="$1"
    # `characters`, `guild_*`, `mod_ollama_chat_*` etc. live in characters.
    # `\`?` after `FROM\s+` matches optional backtick-quoted identifiers
    # (e.g. `FROM \`mod_ollama_chat_personality_templates\``).
    if echo "$sql" | grep -qiE 'FROM\s+`?(characters|guild_rank|guild_member|guild|mod_ollama_chat_)'; then
        echo acore_characters
    # Auth-only tables.
    elif echo "$sql" | grep -qiE 'FROM\s+`?(account|account_access|realmlist)'; then
        echo acore_auth
    # `information_schema` queries explicitly pick the schema via WHERE clause;
    # any db works for the parser.
    elif echo "$sql" | grep -qiE 'FROM\s+`?information_schema\.'; then
        echo acore_characters
    else
        echo acore_world
    fi
}

run_explain() {
    local db="$1"
    local sql="$2"
    # No `-i` — passing SQL via `-e` flag. With `-i` docker exec inherits the
    # while-loop's stdin and drains the queue on the first iteration, so the
    # loop sees only one query. Took 10 min to figure out.
    docker exec "$DB_CONTAINER" \
        mysql --protocol=socket -u"$DB_USER" -p"$DB_PASS" \
              --skip-column-names --silent --batch \
              "$db" -e "EXPLAIN $sql" 2>&1
}

fail=0
total=0
ok=0

while IFS=$'\t' read -r loc sql; do
    [ -z "$loc" ] && continue
    total=$((total + 1))
    db=$(pick_db "$sql")

    output=$(run_explain "$db" "$sql" || true)
    if echo "$output" | grep -qiE '^ERROR\s+[0-9]+'; then
        echo "::error::SQL validation failed at $loc (against $db)"
        echo "  SQL: $sql"
        echo "  $output" | head -3 | sed 's/^/  /'
        fail=$((fail + 1))
    else
        ok=$((ok + 1))
    fi
done < <(python3 "$EXTRACTOR")

echo
echo "smoke-sql-against-live-db: $ok/$total queries parsed cleanly, $fail failed"
exit "$([ "$fail" -gt 0 ] && echo 1 || echo 0)"
