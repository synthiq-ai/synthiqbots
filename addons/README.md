# Vendored client addons

This directory holds WoW client Lua addons we ship alongside the `mod-ollama-chat` server module.

These are **not** part of the AzerothCore CMake build. They are vendored here so the project can carry forward fixes and rebrands in-tree, without depending on git submodules or external addon registries.

## Install

Copy or symlink each subdirectory into your WoW client's `Interface/AddOns/`:

```
<wow-client>/Interface/AddOns/<addon-name>/
```

## Addons

| Addon | Purpose |
|---|---|
| [`synthiqbots-ui/`](synthiqbots-ui/) | Toolbar UI for driving playerbot commands client-side (fork of [Macx-Lio/MultiBot](https://github.com/Macx-Lio/MultiBot), GPLv3). |

Each addon directory contains its own `README.md`, `UPSTREAM.md` (provenance), and `LICENSE`. Third-party license tracking lives at the repo root in [`THIRD_PARTY_LICENSES.md`](../THIRD_PARTY_LICENSES.md).

## CI/CD

Client addons are not deployed by `.github/workflows/deploy.yml` — that workflow only rebuilds the C++ server module on game-host. Changes under `addons/**` ship via the source tree only; users update by re-copying or `git pull`-ing the addon directory.
