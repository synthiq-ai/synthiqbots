# wow-admin MCP

The **wow-admin** MCP server is a second MCP transport bolted onto the
`wow-ops-api` Go sidecar. It exposes ops/admin capabilities at
`POST /mcp` on the same chi listener that already serves `/v1/*` —
single binary, single bearer, single deploy job. It is intentionally
*separate* from the gameplay MCP at `:18790/mcp` (inside `ac-worldserver`)
so the two registries don't pollute each other's `tools/list`.

- **Public URL**: `https://ops.wow.example.com/mcp` (Traefik, IP-allowlisted)
- **LAN fast-path**: `http://192.168.100.11:18791/mcp` (no TLS, bearer only)
- **Auth**: bearer `OPS_BEARER_TOKEN` (constant-time compared)
- **Action gate**: destructive tools enabled by default; set env `OPS_ADMIN_ALLOW_ACTIONS=0` to gate them
- **Audit**: every `tools/call` writes one row to `mod_ollama_chat_admin_audit`

## Tool surface (35 tools)

### Migrated read-only (8 — same names/args as the C++ ops_* tools)

| Tool | Behavior |
|------|----------|
| `ops_status` | Worldserver inspect + MCP ping + DB ping + git SHA + embedded `ops_module_status`. |
| `ops_logs_tail` | Bounded log tail. `container` arg defaults to `ac-worldserver`. |
| `ops_files_list` | List under `OPS_FILE_ROOTS`. |
| `ops_files_read` | Read a file under an allowlisted root. |
| `ops_files_search` | Substring/regex search, NUL-byte sniff to skip binaries. |
| `ops_audit_gateway` | `mod_ollama_chat_gateway_audit` rows. |
| `ops_audit_tactical` | `mod_ollama_chat_tactical_audit` rows. |
| `ops_audit_summary` | Per-bot/per-action aggregates over a window. |

### Container control (5 — destructive, gated)

| Tool | Engine call | Idempotent | Notes |
|------|-------------|-----------|-------|
| `container_list` | `GET /containers/json?all=1` | yes | Read-only. |
| `container_inspect` | `GET /containers/{id}/json` | yes | Read-only. Defaults to `ac-worldserver`. |
| `container_restart` | `POST /containers/{id}/restart?t=10` | yes | **Restarting `ac-worldserver` severs the gameplay MCP for ~2 min.** |
| `container_stop` | `POST /containers/{id}/stop?t=10` | yes | 304 (already stopped) is success. |
| `container_start` | `POST /containers/{id}/start` | yes | 304 (already running) is success. |

No hardcoded allowlist — operator can target any container by name. Tool
*descriptions* steer the agent toward WoW containers
(`ac-worldserver` / `ac-database` / `ac-authserver` / `wow-ops-api`) by
default. To restart something else, the human asks explicitly.

### SQL (2 — db_exec is destructive, gated)

| Tool | DB user | Allowed statements | Per-call guards |
|------|---------|--------------------|-----------------|
| `db_query` | `ops_ro` | `SELECT`, `SHOW`, `EXPLAIN`, `DESCRIBE`, `DESC` | 5000-row hard cap, 10 s timeout. |
| `db_exec` | `ops_rw` | `INSERT`, `UPDATE`, `DELETE` | `confirm:true` required; UPDATE/DELETE need `WHERE`; banned keywords (DROP/TRUNCATE/ALTER/CREATE/GRANT/REVOKE/RENAME) rejected; multi-statement rejected; rate-limited to 5/min/IP by default. |

Both accept `database` ∈ `{acore_world, acore_characters, acore_auth}`,
positional `?` placeholders, and `params:[...]`.

### File edit (2 — destructive, gated)

| Tool | Behavior |
|------|----------|
| `file_set_key` | Replace a `key = value` line (or append at EOF) in an allowlisted `.conf` file. Per-file key allowlist + always-deny patterns. Atomic write-tmp+rename + timestamped backup. After success, the result includes a hint to call `config_reload` on the gameplay MCP. |
| `file_write` | Overwrite a file matching `OPS_ADMIN_WRITE_GLOBS[root]`. Atomic + backup. Optional octal `mode`. No delete tool — pass empty content + restore from backup if needed. |

### Git (3 — destructive, gated)

| Tool | Wraps | Notes |
|------|-------|-------|
| `git_pull_core` | `git -C $OPS_WOW_ROOT pull --ff-only` | Returns hash before/after + git stdout/stderr. Refuses any non-fast-forward. |
| `git_pull_module` | `git -C $OPS_WOW_ROOT/modules/<name> pull --ff-only` | `name` is regex-validated (`$OPS_MODULE_NAME_PATTERN`, default `^mod-[a-z0-9-]+$`) BEFORE filepath.Join — defends against `../` traversal at the schema layer. With `import_sql:true`, post-pull walks the module's `data/sql/` tree and auto-imports standard buckets (see below). |
| `git_pull_all` | core + every dir under `modules/` matching the name regex | Per-repo result array, partial failures don't abort. Per-repo timeout = 60 s; total = 6× per-repo. With `import_sql:true`, each successfully-pulled module gets its standard SQL imported and optional/unrecognized files listed under `imports[<module>]`. Core repo is never SQL-imported. |

Requires `git` binary in the runtime image. The current Dockerfile uses `debian:bookworm-slim` + `apt install git ca-certificates` for this — distroless static doesn't include git.

**`import_sql:true` extension** (added 2026-05-05). Automates the post-pull SQL import for `git_pull_module` and `git_pull_all`:

- Standard tree (`data/sql/{auth,characters,world}/{base,updates}/*.sql`, plus mod-ah-bot's legacy `db-world/`) → auto-imported via `ops_rw` in alphabetical order.
- Optional files — anything matching `optional/` subtree, `zz_optional_*.sql` filename, or `custom/`/`archive/`/`experimental/` parent dir — listed under `optional_available` for explicit opt-in. To import a specific optional file, pass its basename in `optional_includes`.
- Unrecognized paths (e.g. mod-playerbots' `data/sql/playerbots/*` — that's a 4th DB we don't have an enum for) are listed under `unrecognized` and never auto-imported.
- `databases:[acore_world,...]` filter narrows which DBs are touched (default = all 3).

Two-step convention for optional-aware modules: first call with `import_sql:true, optional_includes:[]` to enumerate options, present the list to the user, then call again with the chosen basenames in `optional_includes` to import them.

### SQL import (2 — destructive, gated)

| Tool | Behavior |
|------|----------|
| `sql_import_file` | Import a single `.sql` file into one of `acore_world`/`acore_characters`/`acore_auth` via `ops_rw`. Path resolved under `$OPS_WOW_ROOT` (symlink-evaluated, escape-checked). 25 MB per-file cap, 60 s timeout. `confirm:true` required. |
| `sql_import_dir` | Walk a directory (NOT recursive), import every `*.sql` alphabetically. **Supports `exclude_files: [basenames]` and `exclude_glob` (filepath.Match against basename) — the gap master_wow.sh has, since modules ship optional SQL.** `continue_on_error:false` (default) halts on first failure; `=true` reports per-file failures and keeps going. Per-file result: `{file, ok, rows_affected, bytes_imported, duration_ms, error, skipped, skip_reason}`. |

**DSN requirement**: the pool used by these tools (`OPS_DB_IMPORT_DSN`, falls back to `DB_EXEC_DSN`) MUST include `multiStatements=true` — otherwise any `.sql` with more than one statement will fail at the driver. The default `db_exec` DSN explicitly sets `multiStatements=false` so a single `db_exec` call can't accidentally execute multiple statements; for sql_import use a separate DSN with `multiStatements=true`.

**Required GRANTs on `ops_rw`** (one-time, on `ac-database`):
```sql
GRANT CREATE, ALTER, DROP, INDEX, REFERENCES ON acore_world.*       TO 'ops_rw'@'%';
GRANT CREATE, ALTER, DROP, INDEX, REFERENCES ON acore_characters.*  TO 'ops_rw'@'%';
GRANT CREATE, ALTER, DROP, INDEX, REFERENCES ON acore_auth.*        TO 'ops_rw'@'%';
FLUSH PRIVILEGES;
```
This is a deliberate blast-radius increase: the same `ops_rw` user underpins `db_exec`, so once these grants are applied, an agent that gets a `db_exec` past the regex/banned-keyword guards could in principle ALTER schemas. The guards in `tools_db.go:30` (banned-keyword regex matches DROP/TRUNCATE/ALTER/CREATE/GRANT/REVOKE/RENAME) are the application-layer defence; the grants are the database-layer one. Both must hold.

### wow_* (14 — most destructive, gated)

Master_wow.sh-equivalents. All destructive tools require `confirm:true`. Passwords (`password`, `db_pass`) are auto-redacted in the audit row by `redact_args.go`.

#### Account / realm
| Tool | Wraps | Notes |
|------|-------|-------|
| `wow_create_account` | `INSERT INTO acore_auth.account` with SRP6 verifier computed in-process (mirrors AzerothCore `SRP6.cpp::MakeRegistrationData`) | Username/password 4–16 chars, `[a-zA-Z0-9._-]`. Username uppercased before insert. Password redacted in audit. Does NOT touch the running worldserver — picked up on next login. Requires `OPS_DB_AUTH_DSN`. (Original `worldserver-cli` exec was a dead end — that binary doesn't ship with AzerothCore.) |
| `wow_set_gm_level` | `REPLACE INTO acore_auth.account_access (id, gmlevel, RealmID, '')` (level ≥ 1) or `DELETE FROM account_access` (level 0) | Level 0–4. realm_id default -1. Level 0 deletes the row to match AzerothCore's "no row == player" semantic. Requires `OPS_DB_AUTH_DSN`. |
| `wow_check_realmlist` | `SELECT … FROM realmlist` on acore_auth | Read-only. Requires `OPS_DB_AUTH_DSN`. |
| `wow_update_realm_ip` | `UPDATE realmlist SET address=? WHERE id=?` | Address validated against `[a-zA-Z0-9._-]` AND IPv4-or-hostname check. |
| `wow_update_realm_port` | `UPDATE realmlist SET port=? WHERE id=?` | Port 1024–65535. |

#### Character
| Tool | Wraps | Notes |
|------|-------|-------|
| `wow_reset_character` | One transaction over `acore_characters`: `DELETE` from ~27 per-character progress tables + `item_instance` + `mail`/`mail_items`, then `UPDATE characters` (level→1, xp/money/honor/kills→0, position→race/class start from `acore_world.playercreateinfo`, `at_login \|= RESET_SPELLS\|RESET_TALENTS`, `cinematic=0` so the race intro replays) | Total "start over from level 1" that **preserves the GUID** — so Ollama-wired bots (wired by GUID in `mod_ollama_chat.conf`) stay wired. Takes `guid` OR `name`. `dry_run:true` previews row counts (no writes, no `confirm` needed); `confirm:true` required to execute. **Refuses online characters** (set `force_online:true` only after despawning — for a wired bot, `.playerbots bot remove <name>` via the gameplay MCP's `leader_admin_command` first). The character must log in once afterward so the worldserver reseeds default spells/skills/talents and recomputes stats. KEEPS name, bans, friends/ignore, per-char settings, UI/macros. Uses `ops_rw` (`DBDeps.ExecDB`). |

#### Backup
| Tool | Wraps | Notes |
|------|-------|-------|
| `wow_backup_dir` | `tar -czf <bk>/wow-dir-<ts>[-<label>].tar.gz --exclude=.git -C <parent> <basename>` | Pure-Go tar+gzip. Optional `label` suffix. |
| `wow_backup_db` | `mysqldump --add-drop-table --databases <db>` inside $OPS_DB_CONTAINER, gzipped on the fly | `database` ∈ `{acore_world, acore_characters, acore_auth, all}`. `db_pass` redacted. |
| `wow_backup_volumes` | `tar -czf <bk>/wow-volume-<vol>-<ts>.tar.gz <volume mount>` | `volume` ∈ `{db, client}` — keys defined by OPS_WOW_VOLUME_MOUNTS_JSON. **Unsafe for live mysql data unless the database is stopped first.** Prefer wow_backup_db for the database. |

#### Restore (highest blast radius)
| Tool | Wraps | Notes |
|------|-------|-------|
| `wow_restore_dir` | `tar -xzf <bk>/<file> -C <parent of $OPS_WOW_ROOT>` | Backup file must be a `wow-*.tar.gz` basename in $OPS_WOW_BACKUP_DIR — no path traversal. |
| `wow_restore_db` | `gunzip -c <bk>/<file> \| mysql -u<user> -p<pass> <database>` inside $OPS_DB_CONTAINER | `db_pass` redacted. |
| `wow_restore_volume_db` | Stop ac-database → wipe volume mount dir → tar -xzf → start ac-database | Container is auto-stopped (and restarted on success). Restore failures restart the container before bailing. |
| `wow_restore_volume_client` | Same, against $OPS_WOW_VOLUME_OWNERS_JSON.client | Default owner = ac-worldserver. |

#### Housekeeping
| Tool | Behavior |
|------|----------|
| `wow_list_backups` | List `wow-*.{tar,sql}.gz` files in $OPS_WOW_BACKUP_DIR, newest-first. Read-only. |
| `wow_prune_old_backups` | Delete `wow-*` files older than `keep_days` (default 7). `keep_days:0` deletes ALL matching files (equivalent to `master_wow.sh --prune-now`). Use `dry_run:true` first. `confirm:true` required when `dry_run=false`. |

## Auth + gate matrix

```
Read tools:     bearer ✔  →  always run
Action tools:   bearer ✔  →  OPS_ADMIN_ALLOW_ACTIONS != "0"  →  per-tool rate limit  →  run
Bearer wrong:   401 immediately
Bearer right + AllowActions=0 + destructive: tools/call returns isError=true with
                                              "action tools disabled (set OPS_ADMIN_ALLOW_ACTIONS=1)"
```

**Default is ENABLED.** This is a personal-server deployment behind a Traefik
IP allowlist + bearer; the per-process opt-out window pattern from the
original spec was overkill for a single-operator setup. Set
`OPS_ADMIN_ALLOW_ACTIONS=0` and redeploy `wow-ops-api` to gate destructive
tools when you want a strict read-only window.

`ops_status` calls into the C++ MCP via `internal/mcp/client.go` to fetch
`ops_module_status` (the only ops-side tool that stays C++-resident, since
it reads in-process module state). All other migrated read tools call Go
helper functions directly — no HTTP self-loop.

## Operator setup (one-time on game-host)

### 1. dockerd TCP listener

`wow-ops-api` runs as a container; mounting `/var/run/docker.sock` works
for inspect+logs but is fragile when the container itself is being
restarted by the agent. Switch to TCP:

```jsonc
// /etc/docker/daemon.json — additive, keep existing keys
{
  "hosts": ["unix:///var/run/docker.sock", "tcp://192.168.100.11:2375"]
}
```

The systemd unit ships `-H fd://` on `ExecStart`; that conflicts with
`hosts` in the daemon.json. Override via drop-in:

```ini
# /etc/systemd/system/docker.service.d/override.conf
[Service]
ExecStart=
ExecStart=/usr/bin/dockerd
```

```bash
sudo systemctl daemon-reload
sudo systemctl restart docker
ss -tnlp | grep 2375    # expect a single LISTEN on 192.168.100.11:2375
```

Lock the port to RFC1918 sources only:

```bash
sudo iptables -I INPUT -p tcp --dport 2375 ! -s 192.168.0.0/16 -j REJECT
sudo iptables -I INPUT -p tcp --dport 2375 ! -s 10.0.0.0/8     -j REJECT
sudo netfilter-persistent save
```

### 2. MySQL `ops_rw` user

```sql
CREATE USER 'ops_rw'@'%' IDENTIFIED BY '<from .env>';
GRANT SELECT, INSERT, UPDATE, DELETE ON acore_auth.account                       TO 'ops_rw'@'%';
GRANT SELECT, INSERT, UPDATE, DELETE ON acore_auth.account_access                TO 'ops_rw'@'%';
GRANT SELECT, INSERT, UPDATE, DELETE ON acore_characters.characters              TO 'ops_rw'@'%';
GRANT SELECT, INSERT, UPDATE, DELETE ON acore_characters.character_inventory     TO 'ops_rw'@'%';
GRANT SELECT, INSERT, UPDATE         ON acore_world.creature_template            TO 'ops_rw'@'%';
GRANT SELECT, INSERT, UPDATE, DELETE ON acore_characters.mod_ollama_chat_personality          TO 'ops_rw'@'%';
GRANT SELECT, INSERT, UPDATE, DELETE ON acore_characters.mod_ollama_chat_personality_templates TO 'ops_rw'@'%';
GRANT INSERT                          ON acore_characters.mod_ollama_chat_admin_audit          TO 'ops_rw'@'%';
FLUSH PRIVILEGES;
```

The audit-table grant is what lets the MCP server log its own writes.

### 3. compose updates (`<your compose dir>/compose.yml`)

Drop the docker socket mount and `group_add` (no longer needed when
talking TCP), add `DOCKER_HOST`, switch `/configs` to read-write, and
mount the backup dir:

```yaml
services:
  wow-ops-api:
    environment:
      - DOCKER_HOST=tcp://192.168.100.11:2375
      - DB_EXEC_DSN=ops_rw:${DB_EXEC_PASSWORD}@tcp(ac-database:3306)/?parseTime=true&multiStatements=false
      # OPS_ADMIN_ALLOW_ACTIONS defaults to 1 (enabled). Uncomment to gate writes.
      # - OPS_ADMIN_ALLOW_ACTIONS=0
      # AzerothCore module confs live under /configs/modules (one level
      # below the worldserver.conf root). file_set_key joins CONFIG_ROOT +
      # ALLOWED_FILES basenames, so point CONFIG_ROOT at the deeper dir.
      - OPS_ADMIN_CONFIG_ROOT=/configs/modules
      - OPS_ADMIN_BACKUP_DIR=/backups/wow-conf
      - OPS_ADMIN_ALLOWED_FILES=mod_ollama_chat.conf,worldserver.conf
      - OPS_ADMIN_ALLOWED_KEYS_JSON={"mod_ollama_chat.conf":["OllamaChat.Gateway.*","OllamaChat.Tactical.*","OllamaChat.Mcp.AllowActionTools","OllamaChat.Strategic.*"],"worldserver.conf":["LogLevel","Updates.EnableDatabases"]}
      - OPS_ADMIN_DENIED_KEYS=*Token*,*Password*,*Secret*,*BearerToken*,*ApiKey*,*DatabaseInfo*
      - OPS_ADMIN_WRITE_GLOBS=
      - OPS_ADMIN_RATE_LIMITS_JSON={"db_exec":5,"db_query":30,"container_restart":3}
      # master_wow.sh-equivalent tools — git_pull_*, sql_import_*, wow_*
      - OPS_WOW_ROOT=/wow-root
      - OPS_WOW_BACKUP_DIR=/backups
      - OPS_DB_CONTAINER=ac-database
      - OPS_WORLD_CONTAINER=ac-worldserver
      - OPS_DB_AUTH_DSN=ops_rw:${DB_EXEC_PASSWORD}@tcp(ac-database:3306)/acore_auth?parseTime=true
      # multiStatements=true required for sql_import_*. Separate from DB_EXEC_DSN
      # which keeps multiStatements=false so db_exec can't run multi-statement SQL.
      - OPS_DB_IMPORT_DSN=ops_rw:${DB_EXEC_PASSWORD}@tcp(ac-database:3306)/?parseTime=true&multiStatements=true
      - OPS_MODULE_NAME_PATTERN=^mod-[a-z0-9-]+$
      - OPS_WOW_VOLUMES_JSON={"db":"azerothcore-wotlk_ac-database-data","client":"azerothcore-wotlk_ac-client-data"}
      - OPS_WOW_VOLUME_MOUNTS_JSON={"db":"/volumes/db","client":"/volumes/client"}
      - OPS_WOW_VOLUME_OWNERS_JSON={"db":"ac-database","client":"ac-worldserver"}
    volumes:
      - ${HOME}/docker/wow/azerothcore-wotlk/modules/mod-ollama-chat:/repo:ro
      - ${HOME}/docker/wow/azerothcore-wotlk/env/dist/etc:/configs:rw
      - ${HOME}/wow-conf-backups:/backups/wow-conf:rw
      # AzerothCore tree — RW so git_pull_* can fast-forward on demand.
      - ${HOME}/docker/wow/azerothcore-wotlk:/wow-root:rw
      # Backup dir for wow_backup_* / wow_restore_* / wow_prune_old_backups.
      # 65532:65532 chown required so the nonroot user inside the container
      # can create files: sudo chown -R 65532:65532 /opt/backups/wow
      - /opt/backups/wow:/backups:rw
      # Docker volumes bind-mounted for wow_backup_volumes / wow_restore_volume_*.
      # The volume names must match what `docker volume ls` reports.
      - azerothcore-wotlk_ac-database-data:/volumes/db
      - azerothcore-wotlk_ac-client-data:/volumes/client
    # NOTE: docker socket mount + group_add removed — TCP makes them redundant.
```

Add the matching values to `<your secrets .env>`:

```dotenv
DB_EXEC_PASSWORD=<long-random>
```

### 4. Synthiq `.mcp.json`

Add a second MCP server entry. Recommend the LAN fast-path inside the cloud
VM, public URL elsewhere:

```jsonc
{
  "mcpServers": {
    "wow-worldserver": { "url": "https://mcp.wow.example.com/mcp", "headers": {"Authorization": "Bearer …"} },
    "wow-admin":       { "url": "https://ops.wow.example.com/mcp", "headers": {"Authorization": "Bearer …"} }
  }
}
```

### 5. (Optional) Locking write tools off

Destructive tools are enabled by default. If you want a strict read-only
window:

```bash
echo "OPS_ADMIN_ALLOW_ACTIONS=0" >> <your secrets .env>
docker compose -f <your compose dir>/compose.yml up -d --no-deps wow-ops-api
# … operate …
sed -i.bak '/^OPS_ADMIN_ALLOW_ACTIONS=/d' <your secrets .env>
docker compose -f <your compose dir>/compose.yml up -d --no-deps wow-ops-api
```

The action gate is process-level, not request-level, so re-deploy is the
only way to flip it. That's intentional: a misbehaving agent can't talk
the gate open mid-session.

## Smoke checks

```bash
T=$OPS_BEARER_TOKEN; H=http://192.168.100.11:18791

curl -fsS $H/mcp/health | jq           # {"status":"ok","tools":35,"allowActions":true}

curl -fsS -H "Authorization: Bearer $T" -H "Content-Type: application/json" \
     -d '{"jsonrpc":"2.0","id":1,"method":"tools/list"}' $H/mcp \
     | jq '.result.tools | length'      # 35

# Read tool — works regardless of action gate.
curl -fsS -H "Authorization: Bearer $T" -H "Content-Type: application/json" \
     -d '{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"container_list","arguments":{}}}' \
     $H/mcp | jq '.result.isError'      # false

# Action tool runs by default. To verify the gate works, set OPS_ADMIN_ALLOW_ACTIONS=0,
# redeploy, then:
curl -fsS -H "Authorization: Bearer $T" -H "Content-Type: application/json" \
     -d '{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"container_restart","arguments":{"name":"ac-worldserver"}}}' \
     $H/mcp | jq -r '.result.content[0].text'
# {"error":"action tools disabled (set OPS_ADMIN_ALLOW_ACTIONS=1)"}
```

Audit verification:

```bash
docker exec ac-database mysql -uroot -ppassword acore_characters \
  -e "SELECT id, ts, client_ip, tool, result, duration_ms FROM mod_ollama_chat_admin_audit ORDER BY id DESC LIMIT 10"
```

## Risks (and how they are bounded)

1. **Restarting `ac-worldserver` mid-conversation.** Tool result carries
   an explicit hint; agent system prompt should poll `ops_status` until
   `worldserver.running=true && mcpHealth=ok`. The admin MCP itself stays
   reachable (separate process).
2. **db_exec footgun.** Coarse regex blocks `UPDATE`/`DELETE` without
   `WHERE`; `confirm:true` required; rate-limit 5/min/IP by default;
   `rowsAffected > 1000` returns a `warning` field. The audit row stores
   the full SQL.
3. **Concurrent .conf writes between admin MCP and the C++
   `config_set_value` tool.** Both writers use uniquely-named tmp files
   and `os.Rename` for atomicity; a backup is taken before each write so
   recovery is one `cp` away. Document the admin MCP as the canonical
   write path.
4. **TCP dockerd without TLS.** Bound to game-host LAN IP, host iptables
   restricts source CIDRs. Never expose `tcp/2375` on a public NIC.
5. **Public exposure with bearer + IP allowlist still feels broad.**
   `OPS_ADMIN_ALLOW_ACTIONS=0` keeps writes inert by default; flip it for
   short, intentional windows only. Quarterly bearer rotation.
