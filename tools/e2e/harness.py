"""Host-side plumbing for the E2E runner: MCP calls, SQL reads, log reads,
and game-stack power. Runs ON game-host (next to the containers)."""
from __future__ import annotations

import json
import math
import subprocess
import time
import urllib.request

COMPOSE_DIR = "/srv/wow/docker/wow/azerothcore-wotlk"
MCP_URL = "http://192.168.100.11:18790/mcp"
MCP_JSON = "/srv/wow/wow/.synthiq/.mcp.json"
# Only these, never a bare `compose up` (that re-runs db-import — AGENTS.md).
STACK_SERVICES = ["ac-database", "ac-authserver", "ac-worldserver"]


def sh(cmd: list[str], timeout: float = 120, input_: str | None = None, cwd: str | None = None) -> str:
    r = subprocess.run(cmd, capture_output=True, text=True, timeout=timeout, input=input_, cwd=cwd)
    if r.returncode != 0:
        raise RuntimeError(f"{' '.join(cmd[:4])}… rc={r.returncode}: {r.stderr.strip()[:300]}")
    return r.stdout


MODULE_CONF = f"{COMPOSE_DIR}/env/dist/etc/modules/mod_ollama_chat.conf"


def _mcp_auth() -> str:
    """Bearer for the worldserver MCP: env, else the gateway's .mcp.json, else the
    module conf (the deploy runner only mounts /srv/wow/docker)."""
    import os
    import re
    if os.environ.get("E2E_MCP_TOKEN"):
        return "Bearer " + os.environ["E2E_MCP_TOKEN"]
    try:
        return json.load(open(MCP_JSON))["mcpServers"]["wow-worldserver"]["headers"]["Authorization"]
    except OSError:
        pass
    m = re.search(r'^OllamaChat\.Mcp\.BearerToken\s*=\s*"([^"]+)"', open(MODULE_CONF).read(), re.M)
    if not m:
        raise RuntimeError("no MCP bearer: set E2E_MCP_TOKEN or make .mcp.json / the module conf readable")
    return "Bearer " + m.group(1)


class Mcp:
    def __init__(self):
        self.auth = _mcp_auth()

    def call(self, group: str, action: str, params: dict | None = None, top: dict | None = None,
             timeout: float = 240) -> dict:
        args = {"action": action, "params": params or {}}
        args.update(top or {})
        body = json.dumps({"jsonrpc": "2.0", "id": 1, "method": "tools/call",
                           "params": {"name": group, "arguments": args}}).encode()
        req = urllib.request.Request(MCP_URL, data=body, method="POST", headers={
            "Authorization": self.auth, "Content-Type": "application/json"})
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            out = json.load(resp)
        if "error" in out:
            raise RuntimeError(f"MCP {group}.{action}: {out['error']}")
        text = out["result"]["content"][0]["text"]
        try:
            return json.loads(text)
        except json.JSONDecodeError:
            return {"raw": text}

    _groups: dict[str, str] | None = None

    def act(self, action: str, params: dict | None = None, timeout: float = 240) -> dict:
        """Call an action by name; its facade group comes from tools/list."""
        if self._groups is None:
            body = json.dumps({"jsonrpc": "2.0", "id": 1, "method": "tools/list", "params": {}}).encode()
            req = urllib.request.Request(MCP_URL, data=body, method="POST", headers={
                "Authorization": self.auth, "Content-Type": "application/json"})
            with urllib.request.urlopen(req, timeout=60) as resp:
                tools = json.load(resp)["result"]["tools"]
            self._groups = {}
            for t in tools:
                for a in t["inputSchema"].get("properties", {}).get("action", {}).get("enum", []):
                    self._groups[a] = t["name"]
        return self.call(self._groups[action], action, params, timeout=timeout)

    def gps(self, name: str) -> dict:
        return self.call("admin", "admin_player_gps", {"name": name})

    def gm(self, command: str) -> dict:
        return self.call("admin", "admin_gm_command", {"command": command})

    def reload(self) -> dict:
        return self.call("admin", "config_reload")


def sql(query: str) -> list[list[str]]:
    out = sh(["docker", "exec", "-i", "ac-database", "sh", "-c",
              'mysql -uroot -p"$MYSQL_ROOT_PASSWORD" -N -B 2>/dev/null'], input_=query)
    return [line.split("\t") for line in out.splitlines() if line.strip()]


def worldserver_log_since(since_iso: str) -> str:
    r = subprocess.run(["docker", "logs", "--since", since_iso, "ac-worldserver"],
                       capture_output=True, text=True, timeout=60)
    import re
    return re.sub(r"\x1b\[[0-9;]*m", "", r.stdout + r.stderr)


def dist(a: dict, b: dict) -> float:
    if a.get("map") != b.get("map"):
        return math.inf
    return math.dist((a["x"], a["y"], a["z"]), (b["x"], b["y"], b["z"]))


# ---- stack power -----------------------------------------------------------
def running_services() -> set[str]:
    out = sh(["docker", "ps", "--format", "{{.Names}}"])
    return {n for n in out.split() if n in STACK_SERVICES}


def start_stack(log, owned: set[str]) -> set[str]:
    """Start whatever is down, one service at a time in dependency order,
    adding each to `owned` BEFORE starting it — so a failure halfway still
    leaves the caller knowing what to stop (codex)."""
    up = running_services()
    for svc in STACK_SERVICES:
        if svc in up:
            continue
        log(f"starting {svc} (targeted --no-deps, never a bare up)")
        owned.add(svc)
        sh(["docker", "compose", "up", "-d", "--no-deps", svc], timeout=600, cwd=COMPOSE_DIR)
    return owned


def stop_services(services: set[str], log) -> None:
    if not services:
        return
    order = [s for s in reversed(STACK_SERVICES) if s in services]
    log(f"stopping {order} (they were off before this run)")
    subprocess.run(["docker", "compose", "stop", *order], cwd=COMPOSE_DIR, timeout=300)


def wait_mcp_ready(mcp: Mcp, timeout: float = 600) -> None:
    deadline = time.time() + timeout
    while time.time() < deadline:
        try:
            mcp.call("admin", "admin_player_gps", {"name": "Claude"}, timeout=10)
            return
        except Exception:
            time.sleep(10)
    raise RuntimeError("worldserver MCP not ready")
