<!-- Extracted from CLAUDE.md by claude-md-refactor | Last verified: 2026-05-04 -->

# Build

This module is built as part of AzerothCore — it has no standalone build. It lives in the `modules/` directory of an AzerothCore source tree:

```bash
# Clone into AzerothCore modules dir
cd /path/to/azerothcore/modules
git clone <this-repo> mod-ollama-chat

# Build AzerothCore (which includes this module)
cd /path/to/azerothcore/build
cmake ..
make -j$(nproc)
```

**Dependencies**: `fmtlib` (system), `nlohmann/json` (bundled in `deps/`), `cpp-httplib` (bundled in `src/httplib.h`), OpenSSL (optional, for HTTPS). CMake config is in `mod-ollama-chat.cmake`.

**CI**: `apps/ci/ci-codestyle.sh` — code style check.

## Production worldserver image — `docker/Dockerfile.ollama`

The game-host worldserver is built from **`docker/Dockerfile.ollama` in this repo** (since 2026-09-20; previously an untracked copy at `apps/docker/Dockerfile.ollama` in the core checkout). The core's `docker-compose.override.yml` points at it as `modules/mod-ollama-chat/docker/Dockerfile.ollama` — the build context is the core repo root, so `COPY` paths inside the file are unchanged by the move. It is the upstream AzerothCore multi-stage Dockerfile plus the module's extra build deps (`libcurl4-openssl-dev`, `libfmt-dev`, `nlohmann-json3-dev`).

**The libmysqlclient version is resolved at build time, not pinned.** The compile stage (`-dev` headers) and the runtime stage (`libmysqlclient21.so`) must install the *same* version or the worldserver dies on boot with ACE00046. The build stage asks apt for the current candidate, writes it to `/mysqlclient.version`, and the runtime stage `COPY --from=build`s that file and installs exactly that — so the two cannot diverge, and a cold build (all cache pruned) resolves fresh instead of failing on a revision Ubuntu has since removed. Do not reintroduce a hardcoded `8.0.46-0ubuntu0.22.04.N`; that form broke three cold builds (2026-06-03, 2026-06-23, 2026-09-20). `MYSQLCLIENT_VERSION` remains as an empty-by-default emergency override. Inspect what a built image resolved with `docker run --rm acore/ac-wotlk-worldserver:master cat /mysqlclient.version`.

Changes to `docker/**` trigger the PR build (`build-mod-ollama-chat-pr.yml`), so a Dockerfile edit is compiled before merge.
