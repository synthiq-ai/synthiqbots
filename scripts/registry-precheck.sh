#!/usr/bin/env bash
# Pre-build self-heal probe for docker.io reachability from the game-host runner.
#
# Two failure modes seen in production:
#   1. 2026-05-23 — systemd-resolved stub flake; DNS lookup itself returned
#      "no such host" until `resolvectl flush-caches` ran on the host.
#   2. 2026-05-31 — DNS resolved (AAAA record) but the docker daemon picked
#      the IPv6 address and hit `network is unreachable` while curl/v4 worked
#      fine. Plain `getent hosts` couldn't catch this — only an actual
#      connection attempt over docker's network path does.
#
# Strategy: probe with `docker buildx imagetools inspect alpine:3` (~no
# payload pulled, exercises the same registry-auth + resolver path the real
# `docker build` will use). Retry with linear backoff. Fail loud with an
# actionable error so the operator doesn't waste a 25-min build.
set -euo pipefail

PROBE_IMAGE="${PROBE_IMAGE:-registry-1.docker.io/library/alpine:3}"
MAX_ATTEMPTS="${MAX_ATTEMPTS:-5}"

for attempt in $(seq 1 "$MAX_ATTEMPTS"); do
  if docker buildx imagetools inspect "$PROBE_IMAGE" >/dev/null 2>&1; then
    echo "registry reachable on attempt ${attempt}"
    exit 0
  fi
  if [ "$attempt" -lt "$MAX_ATTEMPTS" ]; then
    backoff=$((attempt * 3))
    echo "::warning::registry-1.docker.io unreachable on attempt ${attempt}/${MAX_ATTEMPTS}; retrying after ${backoff}s"
    sleep "$backoff"
  fi
done

echo "::error::docker.io still unreachable after ${MAX_ATTEMPTS} attempts. Common causes:"
echo "::error::  1) Stale systemd-resolved cache — run on game-host: sudo resolvectl flush-caches && sudo systemctl restart systemd-resolved"
echo "::error::  2) IPv6 routing flake — add to /etc/docker/daemon.json: \"dns\": [\"8.8.8.8\", \"8.8.4.4\"] and restart docker"
exit 1
