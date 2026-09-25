# Contract: Prompts for the proactive loop

**Feature**: 002-proactive-leader-bot
**Target files**: `prompts/proactive_proposer.md` (NEW), `prompts/ollama_classifier.md` (TOUCH)
**Reload behavior**: prompts are loaded at startup and re-read on `.ollama reload`.

This contract specifies the **inputs**, **outputs**, and **forbidden behaviors** for the two LLM touch-points in the proactive loop. The C++ owns the state machine and the candidate selection; the LLMs only classify a single intent and phrase a single short chat line.

---

## Prompt 1 — Proposer (`prompts/proactive_proposer.md`) — NEW

Used to phrase a single proactive chat line. Called via the existing `QueryGatewayAPI` path.

### Inputs (passed in the user message as a JSON snapshot)

```json
{
  "lineKind": "proposal | approval_ack | waiting | proximity_warn | idle_nudge | complete_check | complete_ack | abort_ack",
  "player": {
    "name": "Slayo",
    "class": "Priest",
    "level": 27,
    "zone": "Stranglethorn Vale"
  },
  "bot": {
    "name": "Claude",
    "class": "Paladin"
  },
  "candidate": {                              // present only when lineKind == "proposal"
    "kind": "quest | dungeon | fallbackZone",
    "questTitle": "The Fate of Yenniku",      // if quest
    "hasProgress": true,                      // if quest — "you've already got it" phrasing cue
    "dungeonName": "Wailing Caverns",         // if dungeon
    "zoneName": "Hillsbrad Foothills"         // if fallbackZone
  },
  "context": {                                // present for waiting/proximity/complete/abort
    "blockerText": "quest giver is in a hostile zone",   // for abort_ack
    "distanceYards": 78.4                                // for proximity_warn
  }
}
```

**Implementation status (verified at `55f84c71`, 2026-08-06).** The snapshot above
is the v1 design. `PhraseLine` (`src/mod-ollama-chat_proactive.cpp:1200-1223`) is the
only code path that ever sends `prompts/proactive_proposer.md`, and it emits a strict
subset of it:

- `player` carries `name`, `level`, `zone` only - **no `class`** (`:1204-1206`), and
  the whole block is omitted when the player object cannot be resolved (`:1202`).
- `bot` carries `name` only - **no `class`** (`:1208`).
- `candidate` is emitted only for `lineKind == "proposal"`, exactly as specified
  (`:1209-1220`).
- `context` is **never emitted**. The builder writes it only when the caller's
  `contextNote` is non-empty (`:1221-1222`), and all seven live `PhraseLine` call
  sites pass `""` (`:2068`, `:2141`, `:2328`, `:2378`, `:2453`, `:2488`, `:2614`).
  Its key would also be a single `note` string, not `blockerText` / `distanceYards`.
- `waiting` and `abort_ack` are **never sent**. Both exist in `LineKindToString` and
  `DeterministicPhrasing` but no `PhraseLine` call site passes them, so the live union
  is the six kinds `proposal | approval_ack | proximity_warn | idle_nudge |
  complete_check | complete_ack`. (`LineKind::LevelUp`, added later, is
  deterministic-only by design - it calls `DeterministicPhrasing` directly at `:2641`
  and never reaches the proposer.)

`prompts/proactive_proposer.md` documents the **emitted subset**, not this block - a
prompt that advertises fields the model never receives invites invented values in
player-facing chat. Re-sync the prompt if a future change starts emitting any of the
above.

### Output (REQUIRED — single short line)

A single chat line of **8-25 words**. No JSON wrapper. No quotes. No "Bot:" prefix. No emoji.

### Forbidden behaviors (system-prompt enforced)

- MUST NOT propose a different activity than the one in `candidate`. The C++ selected it; the LLM phrases it.
- MUST NOT call any tool, return JSON, or include URLs.
- MUST NOT exceed 25 words.
- MUST address the player by `player.name` at least once for `lineKind ∈ {proposal, idle_nudge, proximity_warn}`.
- MUST end with a question mark for `lineKind == proposal` (to make the yes/no expectation explicit).
- MUST NOT contain `/say`, `/yell`, `/party`, or any chat-channel slash — output is plain text, the C++ side decides the channel (FR-017).

### Example outputs

| `lineKind` | Example output |
|---|---|
| `proposal` (quest, hasProgress=true) | `Hey Slayo, you've still got Fate of Yenniku open — want to go finish him?` |
| `proposal` (dungeon) | `Slayo, you're the right level for Wailing Caverns — wanna run it?` |
| `proximity_warn` (distance=78y) | `Hold up, Slayo — you're falling behind, I'll wait here.` |
| `idle_nudge` | `Slayo, why are we standing around? Pick something — quest, dungeon, anything.` |
| `complete_check` (activity-complete timeout safety net) | `Slayo, are we still working on this one, or want to move on?` |
| `complete_ack` (after a successful turn-in / zone-out) | `Nice — that's done. What's next?` |
| `abort_ack` (blocker) | `Can't reach that quest giver right now — hostile zone. Want something else?` |

---

## Prompt 2 — Classifier extension (`prompts/ollama_classifier.md`) — TOUCH

The existing classifier prompt routes incoming player chat to an intent label. This feature adds **three new labels** (plus the existing ones continue to work unchanged):

| New label | Triggered by examples (non-exhaustive) |
|---|---|
| `proactive_approve` | "yes", "ok", "let's go", "do it", "sure", "yep", "aight", "sounds good", "lead the way", "go ahead" |
| `proactive_reject` | "no", "skip", "not that", "something else", "nah", "different one", "next", "not now" |
| `proactive_done` | "done", "we're done", "what's next", "next one", "ok next", "finished it", "turned it in" |

### Output contract (unchanged from existing)

The classifier returns a JSON object with at least `intent` (one of the labels including the three new ones) and `confidence` (0.0-1.0). The C++ caller already enforces `confidence ≥ Gateway.OllamaClassifier.MinConfidence` (default 0.8) and falls through if not met.

### Gating

The new labels are **only acted on** when the speaking player has an open Proposal (for approve/reject) or an executing Activity Plan (for done) at the moment the line is received. If neither is open, the classifier's response is ignored for these labels and the message flows through the existing handler chain (so "yes" said in normal conversation doesn't accidentally trigger anything).

### Backwards compatibility

All existing classifier intents are preserved. The three new labels are additive; if the classifier returns one of them in a context where the C++ side has no matching state, the message is treated as if the classifier returned `unknown` (existing fallback).

---

## Token / cost budget (informative)

- **Proposer**: 1 paid `QueryGatewayAPI` call per visible proactive chat line. With recommended defaults and one player, ≤30 paid calls per 2h session. Each call is small (≤200 input tokens, ≤40 output tokens).
- **Classifier**: 1 free local Ollama call per **player message** while a proposal is open. The existing classifier path already runs on every gateway-routed player chat — this feature does NOT add any new per-message calls beyond what the existing classifier handles today.

Net additional paid cost vs. baseline: under one cent per 2-hour player session at current Synthiq rates.
