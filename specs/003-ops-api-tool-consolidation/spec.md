# Spec 003 — ops-api MCP tool consolidation

**Status**: implemented (see "As built")
**Date**: 2026-08-10
**Scope**: `apps/ops-api/internal/mcpserver/` only. The C++ bot-facing MCP
(`src/mod-ollama-chat_tools.cpp`, 204 tools) is explicitly out of scope — see
"Deferred" at the end.

## Problem

`tools/list` on the running ops-api returns **125 tools** totalling
**201,114 characters** of name + description + `inputSchema` — roughly **50k
tokens** consumed before the model reads the operator's actual request.

Two consequences, both already live:

1. **Discovery truncation.** `wow-mcp-bridge` advertises only 74 of the 125.
   The remaining 51 are dispatchable by name but invisible to any client that
   enumerates tools honestly. They are maintained, tested, and unreachable.
2. **Per-call context tax.** Every ops turn pays the full 50k, whether it
   touches one tool or none.

The inventory grew this way because `wow-ops-api-improve` had an add step and
no consolidation step. 125 fires, 125 tools.

## Non-goals

- Changing any tool's behaviour, SQL, or output shape.
- Changing the destructive-action gate, rate limiter, audit log, or redaction.
- Reducing capability. All 125 operations remain callable.

## Design

### The load-bearing observation

`Registry.NamesSorted()` (advertisement) and `Registry.Get(name)` (dispatch)
are already decoupled — `server.go:146` lists, `server.go:175` dispatches, and
neither consults the other. **Legacy tool names therefore keep working for free
if we simply stop listing them.** No alias table, no shim handlers, no
deprecation break for the crons or playbooks that call tools by name.

That makes this a pure advertisement change plus a thin dispatcher, not a
rewrite.

### Facades

Add a `Facade string` and `Action string` field to `Tool`. Existing
registrations gain one line each. Eleven facade tools are registered and are
the only entries returned by `tools/list`; the 125 originals become
`Hidden: true` and stay dispatchable.

| Facade | Actions | Source prefixes |
|---|---:|---|
| `ah` | 18 | `ah_*` — auction analytics, all read-only |
| `db` | 25 | `db_*` — schema/index audit, `query`/`exec`/`explain` |
| `container` | 12 | `container_*` — lifecycle, stats, log grep ladder |
| `ops` | 15 | `ops_*` — audit tables, log histograms, file read/search |
| `deploy` | 9 | `file_*` (4), `git_*` (3), `sql_import_*` (2) |
| `wow_backup` | 9 | backup/restore/prune/list volumes + dirs + db |
| `wow_guild` | 11 | guild bank (6), membership (3), roster, summary |
| `wow_player` | 12 | accounts, logins, online, lookup, GM, reset, mail (4) |
| `wow_bots` | 5 | bot latency, lookup, token usage, fleet status |
| `wow_realm` | 5 | realmlist check, realm ip/port, worldserver health, events |
| `observer` | 4 | clip request/get, screenshot, tactical map render |

**11 facades, 125 actions, zero capability lost.**

### Facade schema

```json
{
  "type": "object",
  "required": ["action"],
  "properties": {
    "action": { "type": "string", "enum": ["market_summary", "undercutters", ...] },
    "params": { "type": "object", "description": "Action-specific arguments. Call action=\"describe\" for the schema." },
    "describe": { "type": "string", "description": "Return the full inputSchema for this action instead of executing it." }
  }
}
```

Dispatch: `ah{action: "market_summary", params: {...}}` →
`Registry.Get("ah_market_summary")` → existing handler, unchanged `params`
passed through as the raw `arguments` object it already expects.

### The tradeoff, stated plainly

Collapsing 125 typed schemas into 11 enums + a free-form `params` object is
where the token savings actually come from. **We lose client-side schema
validation on arguments.** Two mitigations:

1. Handlers already validate their own arguments and return structured errors —
   validation moves from advertisement-time to call-time, it does not vanish.
2. The `describe` action returns any single action's original `inputSchema` on
   demand, so a model that needs the exact parameter list can fetch just that
   one instead of carrying all 125.

Estimated advertisement cost after: ~11 × 2.5k chars ≈ **28k chars ≈ 7k
tokens**, down from 50k. **~86% reduction.**

### Accuracy risk

A flat `ah_item_price_dispersion` is an easier routing target for the model
than `ah(action: "item_price_dispersion")` — the tool name is a router LLMs are
well-tuned for. At 125 tools the context win dominates; at 15 it would not.
Mitigation is keeping enums tight and action names identical to the current
suffix, so the string the model must produce is one it has already seen in the
playbooks.

## Migration

Single PR, no deprecation window needed (legacy names keep dispatching):

1. `registry.go` — add `Facade`, `Action`, `Hidden` to `Tool`; add
   `FacadesSorted()`; make `NamesSorted()` skip `Hidden`.
2. `facade.go` — new. Builds the 11 facade `Tool`s from the registry at
   startup by grouping on `Facade`; generates the enum from registered
   actions so the list can never drift from reality.
3. `tools_*.go` — one added line per registration (`Facade: "ah", Action:
   "market_summary"`). Mechanical, 125 edits, no logic touched.
4. `facade_test.go` — assert every one of the 125 is reachable through exactly
   one facade, that no action is orphaned, and that `describe` round-trips the
   original schema.
5. `server_test.go` — assert `tools/list` returns 11, and that a legacy
   direct call to `ah_market_summary` still succeeds.

**Verification**: `go build ./... && go test ./...` on game-host. Then compare
`tools/list` payload size before/after against the 201,114-char baseline.

**Rollback**: one env flag, `OPS_MCP_FACADES=0`, restores flat advertisement.
Worth carrying for a release since the bridge's behaviour under an 11-tool list
is unverified.

## As built

Two deviations from the plan above, both simplifications:

1. **No per-registration edits.** The plan called for adding `Facade:` /
   `Action:` to all 125 `Register` call sites. Instead `classify(name)` derives
   the facade from the tool name at startup, so `tools_*.go` is untouched — the
   diff is 4 modified files, not 129, and a newly registered tool joins its
   facade automatically instead of needing a line nobody will remember to add.
   The cost is that classification is name-based, which is why
   `facade_production_test.go` pins the real 125-name snapshot and asserts the
   exact per-facade distribution.
2. **Facades carry no handler.** Resolution happens in
   `server.handleToolsCall` before the destructive gate, rate limiter and audit
   write, so all three see the resolved tool name and the resolved arguments.
   A facade cannot launder a destructive action past the gate —
   `TestFacadeDoesNotBypassActionGate` covers exactly that.

`Registry.NamesSorted` was left alone and `AdvertisedSorted` added beside it,
so health counts and `InstallFacades` still see the full table.

**Measured**: 11 facades over 125 actions, advertisement **84% smaller** at
production entry sizes (`TestFacadeAdvertisementIsSmaller`). `go build ./...`,
`go vet ./...` and `go test ./...` all clean on game-host.

Pre-existing `gofmt` drift in `server.go` (struct field alignment, predates
this change) was deliberately left unformatted to keep the diff readable.

## Deferred: the C++ side

`src/mod-ollama-chat_tools.cpp` registers **204** bot-facing tools at
`c076253`. That inventory is the more expensive one — bots pay it on every LLM
call inside the gameplay loop, not just on operator turns. The same facade
pattern applies (`r["name"] = {...}` has the same list/dispatch split), but it
lacks the Go side's test coverage, so it should follow this one rather than
land beside it.

Combined surface today: 329 tools. Only the 125 here are reachable at all
while the AC stack is down.
