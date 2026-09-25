#!/usr/bin/env bash
# Apply pending CORE AzerothCore DB updates (data/sql/updates/db_*) before the
# worldserver restarts.
#
# WHY THIS EXISTS: the worldserver runtime image carries only the binary +
# modules/ SQL — NOT the core data/sql/updates tree — so it can apply MODULE
# updates on startup but NOT core db_world/db_auth/db_characters updates. Those
# are normally applied by ac-db-import. But deploy.yml restarts the worldserver
# with `docker compose up -d --no-deps`, which SKIPS ac-db-import. Result: after
# a core `git pull`, the freshly-built worldserver crash-loops on
# "Your database structure is not up to date ... Table 'acore_world.spell_cone'
# doesn't exist". (Happened in prod 2026-06-07 on a 282-commit core bump.)
#
# This script replicates ac-db-import's core behavior for the standard update
# dirs without needing a (slow, docker.io-flaky) db-import image rebuild: for
# each DB it applies every *.sql under data/sql/updates/db_<x> whose basename is
# NOT already a row in <db>.updates, in sorted (chronological) order, then
# records name + SHA1(uppercase) + state=RELEASED so a later real ac-db-import
# run won't re-apply them. Idempotent — a no-op when nothing is pending (the
# common module-only deploy). Stops on the first failing file (exit 1) so a bad
# migration fails the deploy loudly instead of booting onto a half-migrated DB.
set -euo pipefail

AC_ROOT="${AC_ROOT:-/srv/wow/docker/wow/azerothcore-wotlk}"
DB_CONTAINER="${DB_CONTAINER:-ac-database}"
DB_USER="${DB_USER:-root}"
DB_PASS="${DB_PASS:-password}"

mysql_x() { docker exec -i "$DB_CONTAINER" mysql -u"$DB_USER" -p"$DB_PASS" "$@"; }

applied_total=0
for pair in "acore_world:db_world" "acore_auth:db_auth" "acore_characters:db_characters"; do
  db="${pair%%:*}"; sub="${pair##*:}"; dir="$AC_ROOT/data/sql/updates/$sub"
  if [ ! -d "$dir" ]; then echo "[$db] no $dir — skip"; continue; fi

  tracked="$(mktemp)"
  mysql_x -N -e "SELECT name FROM ${db}.updates" 2>/dev/null | sort > "$tracked"

  pending=0
  while IFS= read -r f; do
    [ -n "$f" ] || continue
    base="$(basename "$f")"
    if grep -qxF "$base" "$tracked"; then continue; fi
    echo "[$db] applying $base"
    if ! mysql_x "$db" < "$f"; then
      echo "::error::[$db] failed applying $base — aborting (DB left partially migrated; fix the SQL)"
      rm -f "$tracked"; exit 1
    fi
    h="$(sha1sum "$f" | cut -c1-40 | tr 'a-z' 'A-Z')"
    mysql_x "$db" -e "INSERT INTO updates (name,hash,state,speed) VALUES ('$base','$h','RELEASED',0)
                      ON DUPLICATE KEY UPDATE hash=VALUES(hash),state=VALUES(state);"
    pending=$((pending+1)); applied_total=$((applied_total+1))
  done < <(find "$dir" -maxdepth 1 -name '*.sql' | sort)

  rm -f "$tracked"
  echo "[$db] applied $pending pending core update(s)"
done
echo "Core DB update reconcile complete — ${applied_total} file(s) applied."
