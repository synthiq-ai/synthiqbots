# ops-api

Go sidecar exposing module status, worldserver logs, file browsing, and audit-table queries over a single bearer-token HTTP API — plus an embedded MCP server (POST `/mcp`) carrying read-only ops tools, container control, scoped SQL, and allowlisted file edits. See `docs/ops-api.md` and `docs/admin-mcp.md` for the full reference.

## Local build

```bash
cd apps/ops-api
go build ./...
```

## Run (locally, against a remote MCP/DB)

```bash
OPS_BEARER_TOKEN=$(openssl rand -hex 16) \
MCP_BEARER_TOKEN=... \
MCP_URL=http://game-host:18790 \
DB_DSN="ops_ro:pw@tcp(game-host:3306)/acore_characters?parseTime=true" \
OPS_FILE_ROOTS="repo=$(git rev-parse --show-toplevel)" \
WORLDSERVER_CONTAINER=ac-worldserver \
OPS_API_LISTEN=:8080 \
go run .
```

The Docker socket path defaults to `/var/run/docker.sock` (override by editing `main.go` if needed).

## Endpoints

REST surface (existing):

- `GET /v1/health` — no auth
- `GET /v1/status`
- `GET /v1/logs?lines=N&since=DURATION&grep=TEXT&regex=0|1`
- `GET /v1/logs/stream?since=...&grep=...` — SSE
- `GET /v1/files/list?root=LABEL&path=...&glob=*.cpp`
- `GET /v1/files/read?root=LABEL&path=...&start=N&end=M&maxBytes=N`
- `GET /v1/files/search?root=LABEL&path=...&q=...&regex=0|1&max=N`
- `GET /v1/audit/gateway?botGuid=&accountId=&from=&to=&errors=0|1&limit=&offset=`
- `GET /v1/audit/tactical?botGuid=&action=&result=&escalated=0|1&from=&to=&limit=&offset=`
- `GET /v1/audit/summary?window=24h`

Admin MCP surface (new):

- `GET /mcp/health` — no auth
- `OPTIONS /mcp` — CORS preflight
- `POST /mcp` — JSON-RPC 2.0 (`initialize`, `tools/list`, `tools/call`, `notifications/*`)

All non-health endpoints require `Authorization: Bearer $OPS_BEARER_TOKEN`.

## Admin MCP env vars

| Var | Default | Purpose |
|-----|---------|---------|
| `OPS_ADMIN_ALLOW_ACTIONS` | `1` | Master gate (default ENABLED — set to `0` to gate destructive tools). Read tools always work; destructive tools (container_*, db_exec, file_*) refuse when `0`. |
| `OPS_ADMIN_CONTAINER_DEFAULT` | `ac-worldserver` | Default container name when omitted from container_* tool args. |
| `DB_EXEC_DSN` | (empty) | DSN for the `ops_rw` user. Empty disables `db_exec`. |
| `OPS_ADMIN_CONFIG_ROOT` | `/configs` | Directory under which `OPS_ADMIN_ALLOWED_FILES` resolve for `file_set_key`. **For AC deployments set this to `/configs/modules` since module conf files (e.g. `mod_ollama_chat.conf`) live one level deeper than the worldserver.conf.** |
| `OPS_ADMIN_ALLOWED_FILES` | (empty) | CSV of allowlisted basenames for `file_set_key` (e.g. `mod_ollama_chat.conf,worldserver.conf`). |
| `OPS_ADMIN_ALLOWED_KEYS_JSON` | (empty) | JSON `{"file":["KeyGlob.*"]}` mapping per-file editable key globs. |
| `OPS_ADMIN_DENIED_KEYS` | `*Token*,*Password*,*Secret*,*BearerToken*,*ApiKey*,*DatabaseInfo*` | Always-deny key globs (wins over allowlist). |
| `OPS_ADMIN_BACKUP_DIR` | `/backups/wow-conf` | Where `<file>.<unix-ts>.bak` is written before each edit. |
| `OPS_ADMIN_WRITE_GLOBS` | (empty) | `root=glob1\|glob2;root2=glob3` allowlist for `file_write`. |
| `OPS_ADMIN_RATE_LIMITS_JSON` | `{}` | JSON `{"db_exec":5,"db_query":30}` overrides the default 30 calls/min/IP. |
| `DOCKER_HOST` | (empty) | When set to `tcp://host:port`, dockerd is reached over TCP instead of `/var/run/docker.sock`. Required for the wow-admin deployment on game-host. |
