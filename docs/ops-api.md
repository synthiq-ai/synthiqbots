# ops-api — Read-only ops/observability sidecar

Status: **MVP, internal use only.** Lives in `apps/ops-api/` (Go), runs on game-host as `wow-ops-api`. Reachable two ways:

- **Public TLS** — `https://ops.wow.example.com` (Cloudflare-issued cert via Traefik + IP allowlist). Default for everything off-LAN.
- **LAN fast-path** — `http://192.168.100.11:18791` (no TLS, bearer only). Bypasses Traefik for on-LAN curls and lives independent of the cert resolver.

The goal is to make routine inspection of the module trivial — `curl https://ops.wow.example.com/v1/status` instead of `ssh game-host`, `docker logs ac-worldserver`, `mysql -e '...'`, `cat ...`. Exposes four surfaces: status, logs, files, audit. No write endpoints.

## Architecture

```
[ remote client ]                    [ on-LAN client ]
        │ HTTPS                              │ HTTP
        │ bearer + IP allowlist              │ bearer
        ▼                                    ▼
   Traefik @ game-host:443  ──►  proxy net  ──►  wow-ops-api  ──►  /var/run/docker.sock  (logs)
                                                          ──►  /repo:ro              (files)
                                                          ──►  ac-database (ops_ro)  (audit)
                                                          ──►  host.docker.internal:18790/mcp  (status)
```

Single Go process. No state. No writes. The only module-side change is one new read-only MCP tool, `ops_module_status`, that returns the in-memory counters as JSON; the sidecar fetches it via the existing JSON-RPC endpoint.

## Endpoints

All endpoints are `GET`. Non-`/v1/health` endpoints require `Authorization: Bearer $OPS_BEARER_TOKEN`. Times are RFC3339 UTC unless noted.

### `/v1/health` (no auth)

```json
{ "ok": true }
```

### `/v1/status`

Combines four sources: `docker inspect ac-worldserver`, `MCP /mcp/health`, the `ops_module_status` MCP tool result, and a DB ping.

```bash
curl -fsS -H "Authorization: Bearer $T" http://game-host:18791/v1/status | jq
```

```json
{
  "schema": 1,
  "generatedAt": "2026-04-25T01:30:00Z",
  "gitSHA": "abc1234...",
  "worldserver": { "state": "running", "running": true, "startedAt": "...", "image": "sha256:...", "restartCount": 0, "exitCode": 0, "inspectedAt": "..." },
  "mcpHealth": "ok",
  "module": { "uptime_sec": 3600, "flags": {...}, "gateway": {...}, "mcp": {...}, "tactical": {...}, "personalities_loaded": 42 },
  "dbOk": true
}
```

### `/v1/logs`

Bounded tail with optional substring/regex filter.

| Query param | Type     | Default | Notes |
|-------------|----------|---------|-------|
| `lines`     | int 1-5000 | 200   | `tail` count from Docker |
| `since`     | duration or unix-sec | — | `15m`, `2h`, `1700000000` |
| `grep`      | string   | —       | substring match (or regex if `regex=1`) |
| `regex`     | `0`/`1`  | 0       | treat `grep` as Go regexp |

```bash
curl -fsS -H "Authorization: Bearer $T" "http://game-host:18791/v1/logs?lines=50&since=15m&grep=Ollama"
```

Response:
```json
{ "lines": [{"ts":"...","stream":"stdout","msg":"..."}, ...], "truncated": false }
```

### `/v1/logs/stream` — SSE

```bash
curl -N -H "Authorization: Bearer $T" "http://game-host:18791/v1/logs/stream?since=0s&grep=gateway"
```

Each event: `data: {"ts":"...","stream":"stdout","msg":"..."}` followed by `\n\n`. Heartbeat comments (`: keepalive`) every 25 s. Server-side hard timeout 5 minutes per connection.

### `/v1/files/list`, `/v1/files/read`, `/v1/files/search`

All take `root=<label>` referring to a configured allowlist root (see `OPS_FILE_ROOTS`). Path traversal is rejected (literal `..` segments, symlinks that escape the root).

```bash
curl -fsS -H "Authorization: Bearer $T" "http://game-host:18791/v1/files/list?root=repo&path=src/&glob=*.cpp"
curl -fsS -H "Authorization: Bearer $T" "http://game-host:18791/v1/files/read?root=repo&path=docs/gateway.md&start=1&end=200"
curl -fsS -H "Authorization: Bearer $T" "http://game-host:18791/v1/files/search?root=repo&q=g_GatewayBotGUIDs&path=src/"
```

`read` is capped at 1 MB by default (`maxBytes` query param, hard ceiling 5 MB). `search` walks the allowlisted root, skips `.git/` and binary files (NUL-byte sniff in first 512 B), max 200 hits, 10-second wall clock.

### `/v1/audit/gateway`, `/v1/audit/tactical`, `/v1/audit/summary`

Pre-canned read-only queries against `mod_ollama_chat_gateway_audit` and `mod_ollama_chat_tactical_audit`. All filters are `?` placeholders — no raw SQL surface.

```bash
curl -fsS -H "Authorization: Bearer $T" "http://game-host:18791/v1/audit/gateway?botGuid=20007&from=2026-04-25T00:00:00&limit=50"
curl -fsS -H "Authorization: Bearer $T" "http://game-host:18791/v1/audit/tactical?action=bot_emote&result=ok&limit=20"
curl -fsS -H "Authorization: Bearer $T" "http://game-host:18791/v1/audit/summary?window=24h"
```

### `POST /v1/feedback` (write)

In-game feedback ingest. The SynthiqBotsUI addon captures a screenshot + chat history + game context; a Windows companion daemon uploads both as `multipart/form-data` (text part `meta` + optional file part `screenshot`). One row inserted into `mod_ollama_chat_feedback` with `status='pending'`. Image stored under `OPS_FEEDBACK_STORAGE_DIR/YYYY/MM/DD/`.

When `FEEDBACK_DISPATCH_ENABLE=1`, an in-process goroutine drains pending rows via a vision-capable Synthiq agent (text + base64 screenshot in OpenAI content-blocks format) and writes the agent's structured response back into `agent_response` / `agent_actions` / `agent_pr_url`. See [`feedback-loop.md`](feedback-loop.md) PR 4 for full state machine + env vars.

```bash
curl -sS -X POST -H "Authorization: Bearer $T" \
  -F 'meta={"id":"smoke-1","addonTs":1745673600,"note":"hello"}' \
  -F 'screenshot=@/tmp/test.tga;type=image/x-tga' \
  http://game-host:18791/v1/feedback
```

Caps via env: `OPS_FEEDBACK_MAX_IMAGE_MB` (default 12), `OPS_FEEDBACK_MAX_META_KB` (default 256). Endpoint 503s when `OPS_FEEDBACK_STORAGE_DIR` or `DB_EXEC_DSN` is unset.

## Setup

### 1. Read-only MySQL user (one-time, on game-host)

```bash
docker exec -it ac-database mysql -uroot -ppassword <<'SQL'
CREATE USER IF NOT EXISTS 'ops_ro'@'%' IDENTIFIED BY 'CHANGE_ME';
GRANT SELECT ON acore_characters.mod_ollama_chat_% TO 'ops_ro'@'%';
FLUSH PRIVILEGES;
SQL
```

### 2. Compose service in `<your compose dir>/compose.yml`

```yaml
services:
  wow-ops-api:
    image: mod-ollama-chat-ops-api:latest
    container_name: wow-ops-api
    restart: unless-stopped
    extra_hosts: ["host.docker.internal:host-gateway"]
    group_add: ["999"]              # docker group GID on game-host (lets distroless nonroot use docker.sock)
    networks: [default, proxy]      # `proxy` is Traefik's external network
    ports:
      - host_ip: ${OPS_BIND_ADDR}   # LAN fast-path — bind to game-host's LAN IP, NOT 0.0.0.0
        target: 8080
        published: 18791
        protocol: tcp
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock:ro
      - /srv/wow/docker/wow/azerothcore-wotlk/modules/mod-ollama-chat:/repo:ro
      - /srv/wow/docker/wow/azerothcore-wotlk/env/dist/etc:/configs:ro
    environment:
      OPS_BEARER_TOKEN: ${OPS_BEARER_TOKEN}
      MCP_BEARER_TOKEN: ${MCP_BEARER_TOKEN}
      MCP_URL: http://host.docker.internal:18790
      WORLDSERVER_CONTAINER: ac-worldserver
      DB_DSN: "ops_ro:${DB_OPS_RO_PASSWORD}@tcp(host.docker.internal:3306)/acore_characters?parseTime=true"
      OPS_FILE_ROOTS: "/repo,/configs"
      LOG_LEVEL: info
    labels:
      - traefik.enable=true
      - traefik.docker.network=proxy
      - traefik.http.routers.wow-ops.rule=Host(`ops.wow.example.com`)
      - traefik.http.routers.wow-ops.entrypoints=https
      - traefik.http.routers.wow-ops.tls=true
      - traefik.http.routers.wow-ops.tls.certresolver=cloudflare
      - traefik.http.routers.wow-ops.service=wow-ops
      - traefik.http.services.wow-ops.loadbalancer.server.port=8080
      - traefik.http.routers.wow-ops.middlewares=wow-ops-allowlist
      - traefik.http.middlewares.wow-ops-allowlist.ipallowlist.sourcerange=203.0.113.20/32,192.168.0.0/16,10.0.0.0/8,172.16.0.0/12
```

Two reachability paths:
- **HTTPS via Traefik** (`https://ops.wow.example.com`) — Cloudflare-issued cert, IP allowlist (cloud Synthiq + RFC1918), bearer required. Default for off-LAN clients.
- **LAN fast-path** (`http://${OPS_BIND_ADDR}:18791`) — bypasses Traefik. Bind to game-host's LAN IP, **not `0.0.0.0`**. Useful when the cert resolver / proxy network is degraded.

### 3. `.env` next to the compose file

```ini
OPS_BIND_ADDR=100.x.y.z          # Tailscale IP, or 192.168.x.y
OPS_BEARER_TOKEN=$(openssl rand -hex 32)
MCP_BEARER_TOKEN=...              # same as Mcp.BearerToken in mod_ollama_chat.conf
DB_OPS_RO_PASSWORD=...
```

### 4. First deploy

The CI `deploy-ops-api` job builds and `docker compose up -d`s on every push to `main` whose diff touches `apps/ops-api/**`. **The compose service must already exist** — CI will fail loudly otherwise. Initial bring-up is manual:

```bash
cd <your compose dir>/
docker compose up -d wow-ops-api
docker logs --tail=30 wow-ops-api      # expect "listening" line
curl -fsS http://127.0.0.1:18791/v1/health
```

## Threat model

- **Docker socket = root on game-host.** Mitigated by: distroless image (no shell), `nonroot` user, single hardcoded container ID (the handler only ever calls `ContainerLogs`/`ContainerInspect` against `ac-worldserver`). Future hardening: replace the bind-mount with `tecnativa/docker-socket-proxy` whitelisting only `GET /containers/{id}/{logs,json}`.
- **File traversal.** Three layers of defense: reject literal `..` segments, `filepath.EvalSymlinks` after `filepath.Abs`, prefix check that the resolved path stays under the allowlisted root. Mounts are `:ro` and limited to the module repo and the conf dir.
- **MCP token shared with public edge.** The sidecar reuses `Mcp.BearerToken` to call into the in-process MCP server. Acceptable for the MVP single-operator setup; a leak compromises both the public MCP edge and the ops-api's status fetch.
- **MySQL credential.** Dedicated `ops_ro` user with `SELECT` on `mod_ollama_chat_*` only — leak-blast-radius is "read audit/history rows," not "drop tables."
- **No TLS.** LAN/Tailscale termination only. Do not expose the host port to public IPs. The bearer token is the only application-layer auth — assume the network layer is your perimeter.
- **SSE follow connections.** 5-minute server-side hard timeout caps idle/leaked clients.

## Verification

After a deploy, set `T=$OPS_BEARER_TOKEN` and pick a host: `H=https://ops.wow.example.com` (public, IP-allowlisted) or `H=http://192.168.100.11:18791` (LAN fast-path):

```bash
curl -fsS $H/v1/health
curl -fsS -H "Authorization: Bearer $T" $H/v1/status | jq
curl -fsS -H "Authorization: Bearer $T" "$H/v1/logs?lines=50&grep=Ollama"
curl -N    -H "Authorization: Bearer $T" "$H/v1/logs/stream"      # in another terminal: tail live
curl -fsS -H "Authorization: Bearer $T" "$H/v1/files/list?root=repo&path=src/&glob=*.cpp"
curl -fsS -H "Authorization: Bearer $T" "$H/v1/files/read?root=repo&path=docs/gateway.md&start=1&end=20"
curl -fsS -H "Authorization: Bearer $T" "$H/v1/files/read?root=repo&path=../etc/passwd"   # expect 400 'path outside allowlist'
curl -fsS -H "Authorization: Bearer $T" "$H/v1/audit/gateway?limit=5"
curl -fsS -H "Authorization: Bearer $T" "$H/v1/audit/summary?window=24h" | jq
curl -i $H/v1/status     # expect 401 (no bearer)
```

## Operating model

- **Logs of the sidecar itself**: `docker logs --tail=80 wow-ops-api`. The sidecar uses Go's `log/slog` text handler to stderr.
- **Restart**: `cd <your compose dir>/ && docker compose restart wow-ops-api`. State-free; safe at any time.
- **Roll back**: `docker compose up -d wow-ops-api --force-recreate` after retagging `mod-ollama-chat-ops-api:latest` to a previous SHA.
- **CI**: every push to `main` whose diff touches `apps/ops-api/**` rebuilds the image and recreates the container. The marker file `~/.cache/wow-ops-api/last-deployed-sha` records the last successful deploy.

## MCP tools

> **Migration in progress (PR A live, PR B pending)**: The 8 `ops_*` tools are
> being moved off the gameplay MCP (`mcp.wow.example.com/mcp`) onto the new
> **wow-admin MCP** (`POST /mcp` on this same `wow-ops-api` binary, served at
> `https://ops.wow.example.com/mcp`). During the parallel-availability window
> both surfaces expose the read-only ops tools with the same names; once
> verified, PR B removes the C++ versions. See `docs/admin-mcp.md` for the new
> registry (17 tools incl. container control, scoped SQL, file edit).

The 8 ops-api endpoints above are also exposed as MCP tools on the worldserver's existing JSON-RPC server (`mcp.wow.example.com/mcp`). Each tool issues an HTTP `GET` against the matching `/v1/*` endpoint and returns the parsed JSON. This lets the agent self-diagnose without ssh — e.g. tail logs, read configs, query audit rows in response to in-game whispers.

| Tool                  | Endpoint              | Required args      | Optional args                                              |
|-----------------------|-----------------------|--------------------|------------------------------------------------------------|
| `ops_status`          | `/v1/status`          | _(none)_           | —                                                          |
| `ops_logs_tail`       | `/v1/logs`            | _(none)_           | `lines`, `since`, `grep`, `regex`                          |
| `ops_files_list`      | `/v1/files/list`      | `root`             | `path`, `glob`                                             |
| `ops_files_read`      | `/v1/files/read`      | `root`, `path`     | `start`, `end`, `maxBytes`                                 |
| `ops_files_search`    | `/v1/files/search`    | `root`, `q`        | `path`, `regex`, `max`                                     |
| `ops_audit_gateway`   | `/v1/audit/gateway`   | _(none)_           | `botGuid`, `accountId`, `from`, `to`, `errors`, `limit`, `offset` |
| `ops_audit_tactical`  | `/v1/audit/tactical`  | _(none)_           | `botGuid`, `action`, `result`, `escalated`, `from`, `to`, `limit`, `offset` |
| `ops_audit_summary`   | `/v1/audit/summary`   | _(none)_           | `window` (e.g. `24h`)                                      |

`/v1/logs/stream` (SSE follow) is intentionally **not** exposed — MCP is request/response.

### Wiring

The proxy lives in `src/mod-ollama-chat_opsapi.cpp` (`CallOpsApi`) and the eight `Tool_Ops*` handlers are in `src/mod-ollama-chat_tools.cpp`. They share four config keys under `[worldserver]`:

```ini
OllamaChat.Ops.Enable         = 1
OllamaChat.Ops.Url            = "http://host.docker.internal:18791"
OllamaChat.Ops.BearerToken    = "<same value as OPS_BEARER_TOKEN in <your secrets .env>>"
OllamaChat.Ops.TimeoutSeconds = 10
```

> **Token rotation:** the bearer token is shared with the Go sidecar — rotate `OPS_BEARER_TOKEN` in `<your secrets .env>` and `OllamaChat.Ops.BearerToken` in `mod_ollama_chat.conf` together, then `docker compose up -d wow-ops-api` and `.ollama reload`.

> **Compose prerequisite:** `ac-worldserver` must have `extra_hosts: ["host.docker.internal:host-gateway"]` so the C++ HTTP client can resolve the loopback alias. The existing socat bridge already requires this — no fresh action needed on a working stack.

### Examples

Status:
```bash
curl -fsS -X POST -H "Authorization: Bearer $MCP" -H "Content-Type: application/json" \
     https://mcp.wow.example.com/mcp \
     -d '{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"ops_status","arguments":{}}}'
```

Tail logs:
```bash
... '{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"ops_logs_tail","arguments":{"lines":50,"grep":"Ollama"}}}'
```

Read a file:
```bash
... '{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"ops_files_read","arguments":{"root":"repo","path":"docs/gateway.md","start":1,"end":40}}}'
```

Errors propagate from upstream — a 401, 400 or transport timeout from the sidecar surfaces as `{"result":{"content":[{"type":"text","text":"{\"error\":\"opsapi: HTTP 401 — ...\"}"}]}}` so the agent (and you) can see what the upstream actually said.

### Disabling

Set `OllamaChat.Ops.Enable = 0` in the worldserver conf and run `.ollama reload`. The tools stay registered (so `tools/list` still shows them) but every call returns `{"error":"ops-api tools disabled (OllamaChat.Ops.Enable=0)"}`.

## Out of scope (v1)

- Write/mutation endpoints
- HTML dashboard
- Multi-tenant ACLs / per-user tokens
- Public Traefik exposure (LAN-only by design)
- Full-text indexed code search
- Persistent log archive beyond Docker's default rotation
- SSE-over-MCP for log streaming — MCP is request/response, follow remains direct curl-only
