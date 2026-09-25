# Third-party licenses

The `mod-ollama-chat` repo is licensed under [AGPL-3.0](LICENSE). The following third-party components are vendored or bundled in this tree under their own licenses, all of which are compatible with AGPL-3.0.

| Component | Version / Pin | Source | License | Location |
|---|---|---|---|---|
| nlohmann/json | (header-only, see file) | https://github.com/nlohmann/json | MIT | [`deps/nlohmann/json.hpp`](deps/nlohmann/json.hpp) |
| cpp-httplib | (single-header) | https://github.com/yhirose/cpp-httplib | MIT | [`src/httplib.h`](src/httplib.h) |
| MultiBot (vendored as `synthiqbots-ui`) | commit `ecee413dacfa93ee705ede83f308f3a0396e6ece` | https://github.com/Macx-Lio/MultiBot | GPL-3.0 | [`addons/synthiqbots-ui/`](addons/synthiqbots-ui/) — see [`UPSTREAM.md`](addons/synthiqbots-ui/UPSTREAM.md) for local changes |

## License compatibility note

GPL-3.0 and AGPL-3.0 are intentionally compatible: AGPL-3.0 is GPL-3.0 with an additional network-use clause. Combining GPL-3.0 code into this AGPL-3.0 work is permitted; the combined work is governed by AGPL-3.0 (the stricter license).

Each vendored component carries its upstream license text in-tree (e.g., `addons/synthiqbots-ui/LICENSE`). When you redistribute a fork of this repo, those files must be carried verbatim.
