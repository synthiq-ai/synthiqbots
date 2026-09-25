# jev decision tier

**Status (2026-09-21 VLAT):** PR 1 (#442, live) — sites A (gateway classifier), C (proactive
intent), D (proactive tick-gate) and E (planner veto), each behind its own flag. **PR 2 — site B,
the tactical executor, as a funnel** (this document's "Tactical funnel" section), behind
`OllamaChat.Jev.Tactical.Enable` + a per-bot `jev` canary.

## What jev is, in one paragraph

[jev](https://typesafe.ai) is TypeSafe's first *System One* model: you send a **state** (text or
structured JSON) plus a set of typed **questions**, and it returns typed answers with calibrated
probabilities in one parallel pass — a `choice` (one option out of up to 255, with a probability
per option and a `confidence`), a `score` (position on 2–10 ordered levels) or a `noul`
(P(yes)). It generates **no text**, so it cannot write a chat line, an item name or an argument
string. That makes it a clean fit for the module's *decision-shaped* fast paths and useless for the
*generation-shaped* ones (chat replies, proposal phrasing) — which stay on LLMs.

Why bother when deepseek-flash already answers in 1.3–4 s: calibrated confidence you can gate on
(the legacy `MinConfidence` knobs compare against a model's **self-report**), zero JSON-parse
failures (the answer is typed, not a string to parse), ~free cost ($0.042 / M input tokens, output
free) and several questions answered in one request.

## Access path (measured 2026-09-20/21)

Two routes speak the same wire format; the **native one is the shipped default** since a TypeSafe
key exists (2026-09-21).

| | TypeSafe native (default) | OpenRouter Decisions API (alternate) |
|---|---|---|
| Endpoint | `POST https://api.typesafe.ai/v1/systemone` | `POST https://openrouter.ai/api/alpha/decisions` (alpha) |
| Model | `jev-1.13.0` (`jev-latest` / `jev-preview` are aliases — pin the version, thresholds are per model) | `typesafe/jev-1.13` — **`typesafe/jev-latest` is rejected there** (400) |
| Auth | `Authorization: Bearer apikey_…` (`TYPESAFE_API_KEY`) | `Authorization: Bearer sk-or-…` |
| From game-host | **usually reachable directly** (~1 s per call measured). If your route is flaky, the jev breaker falls back to the LLM tier. | May be blocked from some networks (`403 Access denied by security policy`). If so, set `Jev.Proxy` to an HTTP proxy whose exit can reach it (e.g. a gluetun container with `HTTPPROXY=on`). |
| Response extras | — | `usage.cost`, `id`, `provider` |
| Cost | 358–398 input tokens per intent request → $0.000015–0.000017 per call (same list price on both) | |
| Limits | 32k tokens of state + longest question; 255 options per choice; 2–10 score levels; 1,200 req/min account-wide (dynamic). | |

This is *not* the 70–500 ms TypeSafe quotes — that is inference in us-west; we pay the route
(~0.6 s of the 0.96 s is connect + TLS, which the keep-alive pool can amortise).

Request/response, verbatim from a live call:

```json
POST /api/alpha/decisions
{"model":"typesafe/jev-1.13",
 "state":{"message":"yeah lets go","hasOpenProposal":true,"hasExecutingPlan":false},
 "questions":{"intent":{"type":"choice","instructions":"...","criteria":{"approve":"...","reject":"...","done":"...","other":"..."}}}}

200
{"model":"typesafe/jev-1.13-20260917",
 "answers":{"intent":{"type":"choice","choice":"approve","probabilities":{"approve":0.99,"other":0.01,"done":0,"reject":0},"confidence":0.99}},
 "usage":{"input_tokens":398,"output_tokens":64,"cost":0.000016716},"id":"gen-dec-…","provider":"TypeSafe"}
```

A `noul` answer carries **only** `noul` (P(yes)) — no `confidence`. That is why every gate that
feeds an existing `MinConfidence` knob is a two-way `choice`, never a `noul`.

## Architecture

```
chat line / tick  ──►  jev tier  ──(low confidence, ineligible, breaker open, no budget)──►  LlmWire tier (DeepSeek)  ──►  deterministic code
                        Jev::Decide()                                                        unchanged                    unchanged
```

| File | Role |
|---|---|
| `src/mod-ollama-chat_jev_core.h` | Header-only, dependency-free: question builders, response parser + validator, `Breaker`, `Deadline` arithmetic. Compiled by the harnesses with a bare `g++`. |
| `src/mod-ollama-chat_jev.{h,cpp}` | Runtime: immutable config generation (swapped atomically on `config_reload`), keep-alive client pool (`httplib::Client`, leased exclusively, proxy-aware), jev-side breaker, one-line-per-call logging, audit-column self-migration. |
| `prompts/jev_questions.json` | Question **wording** per site (`instructions` + contrastive `criteria`). The option **sets** always come from the live C++ allowlists — the file can change how an option is described, never add one the dispatcher would reject. Re-read on `config_reload`. |
| `src/mod-ollama-chat_proactive_llm.cpp` | Sites C, D, E. |
| `src/mod-ollama-chat_gateway.cpp` | Site A (`TryJevIntentClassify` in front of `TryGatewayIntentClassify`) + the new `gw_classifier` audit label. |
| `src/mod-ollama-chat_httpclient.{h,cpp}` | `SetTimeoutMs()` — the LLM tier can now be handed a sub-second remainder instead of rounding to whole seconds. |
| `tests/jev/` | `harness_jev.cpp`, `harness_fallback.cpp` (two-directional g++ harnesses), `bench_jev.py` + `classifier_phrases.json` (calibration). |

`LlmWire` is untouched and stays the fall-through tier; jev sits *above* it as a typed interface
rather than being bolted on as a text-in/text-out `Format::Jev`.

### Per-site mapping

| Site | State | Question(s) | Result |
|---|---|---|---|
| **C** intent (`llm::ClassifyIntent`) | `{message, hasOpenProposal, hasExecutingPlan}` — the same envelope the LLM prompt gets | `intent` choice{approve, reject, done, other} | Label + `confidence`; the C++ post-rules (no approve/reject without an open proposal, no done without an executing plan) still apply. |
| **D** tick-gate (`llm::EvaluateTickGate`) | the existing `BuildTickGateSnapshot` JSON | `gate` choice{propose, wait} | `wait` honoured only at ≥ `Jev.ProactiveTickGate.MinConfidence`; everything else falls through exactly as today. |
| **E** planner (`llm::PlanProposalDecision`) | the existing planner snapshot | `decision` choice{propose, wait} | A confident `wait` **skips the paid gateway call**; a confident `propose` still asks the gateway to write the line (jev cannot). Low confidence → the gateway decides as before. |
| **A** classifier (`TryJevIntentClassify`) | `{message, humanName, leaderGroupedWithHuman, botNames[]}` | `action` choice over `kClassifierAllowedActions` (50) **+ `unknown`**; speculative `command` (raw squad verb), `icon`, `loot_filter`, `pet_command` — read only when the chosen action needs them | Args are filled from the answers; the canned `reply` comes from the questions file (`{{humanName}}`, `{{target}}` substituted); target bot resolution reuses `FindMentionedNonLeaderBotName` in `DispatchLocalClassifierPlan`, so all the leader-scope rewriting is unchanged. |

**Which chat lines reach A at all (PR 4, operator decision 2026-09-21):** the interceptor exists
for *terse imperative orders* — "attack", "follow me", "everyone hearth" — not for conversation.
`TryGatewayIntentClassify` therefore has two independent tiers (jev via `Jev.Classifier.Enable`,
the LLM classifier via `Gateway.OllamaClassifier.Enable`; either alone is a valid interceptor —
before PR 4 the LLM flag gated both) and a shared word gate `Gateway.Classifier.MaxWords`
(0 = off; **live 3**): a line with more words skips both tiers and goes to the in-character
gateway with tools. On the 79-line classifier bench, 66 lines are ≤ 3 words (production predicate at 0.8: 44 acted,
0 wrong); the 13 longer ones are 9 conversational lines plus 4 orders ("Clawd summon to me", "put a cross on it", "always loot
linen cloth", "stop looting copper ore") that now get an in-character answer instead of "On it".
Live: LLM tier **off**, jev tier on at `MinConfidence 0.8` — a jev miss on a ≤ 3-word line goes to
the brain too, never to DeepSeek.

**Eligibility for A is computed, not hand-listed.** `JevClassifierEligible` reads each tool's
`required` array from the registry (`GetGatewayToolRegistry()`) and allows the jev tier to
dispatch only when `required ⊆ {botGuid} ∪ slots the answers fill` *and* the action is in the
fillable table (`bot_say` declares only `botGuid` as required but needs free text — the table is
the belt to the registry's braces). Anything else jev picks (`bot_say`, `bot_yell`, item-name
verbs, invites, targets, `bot_disperse`'s `yards`, and `bot_set_raid_target_icon` — its *optional*
`target_name` means "mark Ragnaros skull" would silently mark the master's current selection
instead) is a **fall-through to the LLM tier** (which does produce args) when
`Gateway.OllamaClassifier.Enable=1`, otherwise straight to the in-character gateway. Every consumed slot
answer is gated on its **own** confidence, not just `action`'s. The leader-rewrite loot vocabulary
(`ClassifierDirectToolAsLeaderCommand`) now includes `disenchant`, so a squad-scoped
"Geek disenchant loot" stays a `leader_command` instead of degrading to a leader-only tool call.

### Tactical funnel (site B, PR 2)

The tactical executor fires per bot per tick (10 s heartbeat) and today spends 1.3–4 s on
deepseek-flash choosing among 132 actions with free-form args. Both plan reviewers flagged the
naive port — a 132-way Choice — as a regression: probability mass spreads, confidence rarely clears
the gate, every tick pays jev *and* DeepSeek. So site B is built the way the "Jev Engineering"
article prescribes: **rules cut the obvious, jev picks among what is left.**

1. **Static eligible set** (`kJevTacticalCandidates` → `JevTacticalEligible()`, validated once at
   first use against the tool registry): actions in `kTacticalAllowedActions` whose registry
   `required` is exactly `{botGuid}` — `tactical_idle`, `bot_follow`, `bot_stay`, `bot_flee`,
   `bot_revive`, `bot_release_spirit`, `bot_accept_resurrect_request`, `bot_turn_in_quest`,
   `bot_accept_quest`, `bot_maintenance`, `bot_train_spells`, `bot_sell_junk`, `bot_autogear`,
   `bot_roll`, `bot_reset_ai` — plus `bot_emote` and `bot_set_loot_filter`, whose one arg a
   speculative answer fills. Deliberately absent: `bot_stop_combat` (not tactical-allowed — the
   engine owns combat exit) and `bot_self_reincarnate` (needs an Ankh / pre-applied Soulstone the
   snapshot cannot see). A candidate whose schema demands anything else is logged and dropped,
   never dispatched.
2. **Per-tick pruning** (`Jev::PruneTacticalOptions` over `TacticalTickFacts`, header-only,
   harness-proven with 7 injected arms): remove `Tactical.RequireConfirmationFor` members (the HITL
   gate would refuse them), combat-blocked verbs while in combat unless `AllowCombatOverride`,
   ambient verbs in combat when `DisableRepliesInCombat` or whenever `Tactical.Ambient.Enable=0`
   (the dispatcher converts them to idle), `bot_follow` without a registered online master
   (`Tool_BotFollow` errors), `bot_accept_resurrect_request` without a pending popup (no-op), rez
   verbs for a live bot, everything but rez + idle for a corpse, and every action tool when
   `Mcp.AllowActionTools=0`. Then append **`other`**; if only idle survived, jev is not asked at all.
   Result: 13 options alive/out-of-combat, 3 for a corpse, 1 for a corpse mid-fight with no popup.
3. **One request, four questions**: `action` Choice over that set; `escalate` Noul; speculative
   `emote` (20 tokens) and `loot_filter` (4 modes), read only when the action needs them.
4. **Dispatch or fall through**: `action` ≥ `Jev.Tactical.MinConfidence` (0.6) and not `other` →
   dispatched with `botGuid` (+ `emote`/`mode` from their own confidence-gated answers) through the
   unchanged `DispatchTacticalAction`; escalation only when `escalate` ≥ `Jev.Tactical.EscalateMin`
   (0.8) and the action is not an emote (the prompt's "never emote + escalate" rule, now in code).
   `other` / low confidence / any failure → the LlmWire/DeepSeek call with the tick's **remaining**
   budget, only if ≥1500 ms remain.

What stays with the LLM tier by construction: everything free-text or targeted — `bot_say`,
`whisper_player`, `bot_cast`, `bot_move_to`, `bot_revive_target`, pet stances, squad orders,
bank/guild/AH/mail, `bot_use_gameobject`. The state jev reads is the same key=value snapshot the
LLM reads (plus `botIsDead`, `directive`), so no second snapshot builder to keep in lockstep.

Budget per tick = the bot's `timeoutMs` (live 12000) as one **absolute deadline** that now also
bounds the LLM tier's endpoint-slot wait (`AcquireEndpointSlot` used to wait unbounded, so a bot
could sit through another bot's inference and then start with a budget it no longer had); jev ≤
min(`Jev.TimeoutMs`, 1500, remaining). A jev miss with <1500 ms left is audited as
`jev_miss_no_budget` under `backend='jev'` — never as an LLM failure that did not happen. The whole
jev attempt is exception-safe (a malformed questions-file override degrades to the LLM tier).
The jev call runs **inside** the per-bot `std::async` lambda, so bots still fan out in parallel;
with `Jev.MaxConcurrent = 2` a third simultaneous bot waits ≤200 ms for a pool slot and otherwise
falls to DeepSeek — raise to 3 for a 3-bot fleet. Cost: ~1k input tokens per tick ≈ $0.00004 →
about **$0.03 per hour for two bots**.

**Canary**: `Tactical.BotOverridesJsonFile` entries take `"jev": true|false`. Precedence:
`Jev.Enable=0` wins; then the per-bot value if set; then `Jev.Tactical.Enable`. Roll out one bot
first and read `mod_ollama_chat_tactical_audit` — `backend='jev'` rows with their `latency_ms`
next to the `'llm'` rows, and the `action` distribution (a healthy tick stream is mostly
`tactical_idle`; a jev tier that emotes every tick is a wording problem in
`prompts/jev_questions.json → tactical.action.criteria`, fixable with `config_reload`).

### Leader playbook relevance filter (PR 3)

The one place the article's "compaction is a relevance filter" applies here is not the chat
history (capped at 5 turns) but **`prompts/gateway_leader.md`: 197 KB, 78 workflow rows of
~2.7k chars, injected into every leader gateway request** (`BuildGatewaySystemPromptEx`,
`gateway.cpp` Tier 9) — roughly 45k tokens of system prompt per whisper, of which a handful of
rows ever apply to the line the player just typed.

With `Jev.Playbook.Enable=1`, `Jev::FilterLeaderPlaybook` runs once per human message before the
gateway call:

1. `ParsePlaybook` (header-only, harness-proven against the real file) splits the text into header
   (everything before the first `- "` row), the rows (each with its trigger-phrase `prefix` — the
   complete text before ` -> `, never truncated: two live rows run 420 and 509 chars and a cap cut
   one row's exit-phrase triggers) and the footer (non-row lines after the first row). Parsed once
   per config generation; `render(all)` reproduces the file exactly (harness-asserted).
2. **One request, one `noul` per row**: the shared framing (`task`, `yes_means`, `no_means` from
   `prompts/jev_questions.json → playbook.row`) goes once into the *state* next to `message`
   (nothing else — the filter runs on the gateway worker thread, where a `Player*` lookup is not
   lifetime-safe, so it deliberately takes no player data); each noul carries only its row's
   `workflow_triggers`. Measured 2026-09-21 on the
   native endpoint: **6.5k input tokens, 1.8–2.6 s, ≈ $0.0003 per message** (18.5k tokens when the
   framing was repeated per question — that is why it lives in the state).
3. `SelectPlaybookRows`: keep rows with P(relevant) ≥ `Jev.Playbook.MinRelevance` (0.3), padded up
   to `MinRows` (4) with the next-best, capped at `MaxRows` (12), returned in **file order**;
   `RenderPlaybook` = header + kept rows + footer. `MinRows=0` is legal and means small talk gets
   header + footer only (the reviewer's preference); 4 is the shipped default because the padded
   rows are the highest-scoring ones, not a fixed "generic" set, and cost ~10 k chars.
4. Any failure (breaker open, timeout — the dedicated `Jev.Playbook.TimeoutMs` is 4000 because
   this call is 78 questions wide and precedes a 5–20 s gateway call), or an autonomous leader tick
   (no human message) → the full playbook, exactly as before.

Measured relevance on 8 leader orders (top row's P): `attack` → the attack row 0.77;
`everyone follow me` → follow 0.96; `Clawd hearth` → hearth 0.73; `share your quest with the party`
→ share 0.95; `sell your junk and repair` → repair 0.72; `how are you doing today` → nothing ≥ 0.06
(the 4-row floor sends the top generic rows); `where is Raz` → nothing ≥ 0.10 (no row for it — the
tool registry, not the playbook, carries locate verbs). Render size depends on *which* rows: the
mean row is 2.5 k chars, so a typical 4–12-row render is 10–30 k chars (5–15 % of the 197 k file);
the harness fixtures are the first 12 rows = 6.7 k (3.4 %) and the 12 largest = 67 k (34 %).

Log line per message: `[Ollama Chat Jev] playbook filter kept 5/78 rows (197017 -> 8412 chars)
latency=1912ms in_tok=6484 top: r0=p0.77 …`. There is no audit row — the gateway turn's own
channel row already records the request; watch the log line and the gateway lane's per-turn cost.

### Fallback chain and the budget rule

Each chain gets **one absolute deadline** = the path's existing timeout. jev's cap is
`min(Jev.TimeoutMs, site cap, remaining)`; the LLM tier is started only when the remainder can
still fit a useful call.

| Site | Path budget | jev cap | LLM tier runs if remaining ≥ | Thread |
|---|---|---|---|---|
| C intent | `ProactiveIntent.TimeoutMs` 3000 | 1000 | 1500 | **world thread**, inside the chat hook under the proactive mutex — the reason for the tightest cap |
| D tick-gate | `ProactiveTickGate.TimeoutMs` 2000 | 1200 | 1000 | tactical tick thread (shared with B's future ticks) |
| E planner | gateway's 300 s | 1500 | always (gateway) | tactical tick thread |
| A classifier | none — the classifier timeout *is* the budget, on a detached per-message thread | 1500 | always, with its full 5000 | detached thread |

Other rules, each proven by `tests/jev/harness_fallback.cpp` (the corresponding `-DINJECT_*`
arm removes the guard and the harness fails):

- **Single attempt** per jev call, **bounded end-to-end**: the lease wait comes off the budget and
  httplib's `set_max_timeout` caps connect+TLS+request together, so connect and read timeouts can
  never add up serially past the cap. `OllamaHttpClient::Post` (the LLM tier) got the same
  treatment: `set_max_timeout` per attempt and a retry that only gets what the first attempt left.
- **jev-side breaker, per config generation**: 3 consecutive transport/HTTP failures → open for
  `Jev.BreakerCooldownSec` (60) → every site skips straight to its LLM tier with the **full**
  budget at zero cost. Low confidence and ineligible actions are fall-throughs and never count. It
  lives inside the generation (a request that started against the old endpoint cannot trip or
  reset the new one's after `config_reload`), and it is *not* the `TacticalInference` URL-keyed
  breaker. Likewise `Result.minConfidence` is the threshold of the generation that served the
  request — a reload mid-flight cannot lower the bar under an answer.
- **Never throws**: the lease is RAII (slot returned on every path, including exceptions from
  client construction or `dump()`), and any exception inside the request becomes a failed result
  that feeds the breaker.
- **Exactly one dispatch** per chain: a low-confidence jev answer is audited but never acted on.
- **Per-tier thresholds**: `IntentResult.minConfidence` / `TickGateResult.minConfidence` carry the
  threshold of the tier that answered (`Jev.<Site>.MinConfidence` vs the legacy self-report knob),
  and the callers compare against that — a calibrated 0.75 is not rejected by a legacy 0.8.
- **Pool**: `Jev.MaxConcurrent` persistent `httplib::Client`s (keep-alive, proxy-aware), leased
  exclusively (cpp-httplib serialises requests per instance), acquisition bounded at 200 ms
  (`pool_busy` → fall-through). Whether keep-alive survives through the proxy's CONNECT tunnel
  between 10 s ticks is unproven — the audit table's `latency_ms` by `backend` is the measurement.
- **TLS verification is off** for the pool, matching `OllamaHttpClient` (`enable_server_certificate_verification(false)`) — a pre-existing module-wide trade-off, now also applying to a third-party API. Revisit separately.

### Audit

Both audit tables gain a `backend VARCHAR(8) NULL` column (`'jev'` / `'llm'` / `'det'` / NULL);
`mod_ollama_chat_tactical_audit` also gains `prompt_tokens`. jev's `usage.input_tokens` lands in
`prompt_tokens`, its latency in `latency_ms`.

**The columns are added by the module itself at startup** (`Jev::EnsureAuditBackendColumns`,
INFORMATION_SCHEMA check + `ALTER TABLE`, idempotent, one `LOG_INFO` per applied ALTER). The SQL
under `data/sql/characters/updates/2026_09_21_00_jev_backend.sql` is documentation for a fresh
install: the worldserver image bakes `AC_UPDATES_ENABLE_DATABASES=0`, so nothing under `updates/`
is ever applied automatically. Until the migration is confirmed the INSERTs keep their legacy
column list, so an unmigrated DB never loses rows.

Site A finally gets its own label: `source_channel = 'gw_classifier'` (gated by
`Gateway.EnableAudit` like every gateway row). The chat turn's channel-labelled row from
`handler.cpp` still exists — the classifier row records the *classifier call*, the channel row
the *chat turn*. Sites C/D/E keep `proact_intent` / `proact_tickgate` / `proact_planner`.

Log line per call (never the state text):
`[Ollama Chat Jev] site=intent intent=approve(0.99) latency=912ms in_tok=398 cost=$0.000017 model=typesafe/jev-1.13-20260917`

## Configuration

See the `JEV DECISION TIER` block at the end of `conf/mod_ollama_chat.conf.dist`. All defaults are
off/inert. Every key is read in `LoadOllamaChatConfig()`, so `config_reload` (gameplay MCP) is the
whole rollout mechanism — no rebuild. Precedence: `Jev.Enable=0` beats everything; then the site's
`Enable`; a site whose key/url/model are unusable logs `enabled but unusable: <reason>` and stays on
its LLM tier.

## Calibration

jev's confidence is derived from the probability distribution; the legacy `MinConfidence` values
(0.8 / 0.7 / 0.6) were tuned on the LLM's self-reported number and **do not carry over**. Read the
thresholds off the bench:

```bash
JEV_API_KEY=… python3 tests/jev/bench_jev.py --site intent        # 340 labelled phrases (tests/proactive/classifier_phrases.json)
JEV_API_KEY=… python3 tests/jev/bench_jev.py --site classifier    # 79 lines (tests/jev/classifier_phrases.json)
```

It replays the labelled sets through the **same** questions the C++ sends (wording from
`prompts/jev_questions.json`, option sets mirroring the allowlists), then prints per-class accuracy,
a confidence histogram and the accuracy-vs-threshold curve (`kept%`, `acc(kept)`, `wrong-acted`).
Pick the lowest threshold whose `wrong-acted` you can live with; everything below it falls through
to the LLM tier, which is today's behaviour. Runs from the Mac (OpenRouter is reachable there);
~$0.005 per full run. D and E have deterministic rules (four veto conditions) and no dataset — their
thresholds are the conservative 0.6 until live rows say otherwise.

Measured results are recorded in the *Bench results* section below once a run has been done for
the release in question.

## Rollout

1. Merge → `deploy.yml` (jev off; zero behaviour change; the startup migration adds the audit
   columns — look for `[Ollama Chat Jev] audit backend columns ready`).
2. **No infra prerequisite on the native route** — `api.typesafe.ai` answers from game-host directly
   (measured 0.96 s/call with the real key). Verify from the caller once the stack is up:
   `docker exec ac-worldserver bash -c 'echo > /dev/tcp/api.typesafe.ai/443'`.
   *Only if you switch `Jev.Url` to OpenRouter:* an HTTP proxy reachable from
   `ac-worldserver`, then `Jev.Proxy=http://<proxy-host>:<port>` (+ user/pass). Verify with
   `curl -x http://user:pass@<proxy-host>:<port> https://openrouter.ai/api/v1/models` → 200.
3. Conf on game-host (host bind-mount `env/dist/etc/modules/mod_ollama_chat.conf`, back it up first):
   `Jev.Enable=1`, `Jev.ApiKey=apikey_…` (the TypeSafe key), defaults for Url/Model, then
   `config_reload`. Expect `[Ollama Chat Jev] config loaded: url=https://api.typesafe.ai/… proxy=none …`.
4. Flip sites one at a time, reading the audit table between flips: **D → C → A → E**.
   ```sql
   SELECT source_channel, backend, COUNT(*), AVG(latency_ms), AVG(prompt_tokens)
     FROM mod_ollama_chat_gateway_audit WHERE ts > NOW() - INTERVAL 1 DAY GROUP BY 1,2;
   ```
5. Rollback is the site flag → `config_reload`. Breaker drill: stop the proxy for 2 min → three
   `[Ollama Chat Jev] site=… transport:` warnings, then `breaker OPEN for 60s`, every row on
   `backend='llm'` at unchanged latency, recovery after the cooldown.

## Known limits / gotchas

- **A per-site cap below the network floor disables that site AND poisons the others.** Measured
  live 2026-09-21: jev round trips from `ac-worldserver` are 1.25–1.6 s (a bare `curl` from the host
  showed 0.96 s — don't size caps from curl). Tick-gate's 1200 ms cap timed out on every heartbeat;
  each timeout counts toward the shared breaker, which then opened for 60 s and pushed tactical to
  the LLM tier. Caps are now intent 1700 / tick-gate 1800 / classifier 2000 / planner 1500, tactical
  = `Jev.TimeoutMs` (live 2500). Tell: `site=<x> transport: Failed to read connection after <cap>ms`
  repeating at exactly the cap, followed by `breaker OPEN`.

- `typesafe/jev-latest` → 400 on OpenRouter; use `typesafe/jev-1.13`. Native ids: `jev-1.13.0`.
- `noul` has no `confidence`; use a two-way `choice` for anything that feeds a threshold.
- OpenRouter is geo-blocked from game-host's egress; gluetun ships with `HTTPPROXY` **off** by default.
  The native endpoint is not blocked — prefer it; the proxy knobs exist for the alternate route only.
- The jev tier cannot fill free-text args — by design those lines take the LLM tier when it is enabled, else the gateway.
- Thresholds are per model version; re-run the bench when `Jev.Model` changes.
- Site B (tactical executor) is not wired: PR 2.

## Bench results (2026-09-21 VLAT, `typesafe/jev-1.13-20260917`, from the dev Mac via OpenRouter)

**Classifier (site A), 79 lines** — `tests/jev/bench_jev.py --site classifier`:

| | |
|---|---|
| Raw accuracy | 76/79 = 96.2 %; `command` slot 79/79 |
| Misses | `hand in` → `bot_turn_in_quest` (0.44), `set home` / `bind hearth here` → `leader_command_all` (0.49 / 0.57) — all below threshold, i.e. fall-throughs |
| Threshold curve (production predicate: eligible action + every consumed slot ≥ threshold + ≤1500 ms) | **0.6 → 64.6 % acted, 100 % of acted correct, 0 wrong-acted**; 0.7 → 62.0 % acted, 0 wrong-acted. The other ~35 % are fall-throughs by design: the 22 lines whose action is jev-ineligible (`bot_say`, `bot_disperse`, item-name verbs, invites, `bot_set_raid_target_icon`, …) plus the sub-threshold ones — all handled by the DeepSeek tier as today. Two runs, identical action/command results. |
| Latency / tokens / cost | p50 848–850 ms, p90 967–1019 ms; avg 3,718 input tokens (51 action criteria + 4 speculative questions) → $0.00016 / call |

→ `Jev.Classifier.MinConfidence = 0.6` was the PR 1 default; **PR 4 ships 0.8** (operator's
call: with the DeepSeek tier off, a jev miss is an in-character answer, not a silent fall-through,
so the bar only trades speed for immersion). Replayed on the same rows through the production
predicate (`classifier_acceptance`: eligible action + every consumed slot ≥ threshold + ≤ 1500 ms):
**all 79 → 0.8 acts on 45 (57.0 %), 0 wrong-acted**; the **66-line ≤ 3-word subset** the word gate
lets through → **0.8 acts on 44 (66.7 %), 0 wrong-acted** (0.6: 50 / 75.8 %). The 22 non-acted
short lines are ineligible actions, sub-threshold answers and the 9 conversational lines — all of
which reach the gateway.

**Intent (site C), 340 phrases** (`tests/proactive/classifier_phrases.json`; `none`/`directcmd` →
expected `other`) — two runs:

| | run 1 (first wording) | run 2 (shipped wording) |
|---|---|---|
| approve | 72/73 | 71/73 |
| reject | 103/117 (88.0 %) | **113/117 (96.6 %)** |
| done | 51/51 | 51/51 |
| other | 56/99 | 56/99 |
| Latency / tokens / cost | p50 827 ms, p90 949 ms; avg 1,005 tokens; $0.014 / run | p50 823 ms, p90 967 ms; avg 1,103 tokens; $0.016 / run |

What moved between the runs: the polite-decline idiom family (`i'm all set`, `sure i'm all set`,
`yes im all set`, …, 10 phrases) was read as **approve at 0.76–0.99** in run 1 — a genuine jev error
and a regression against the C++ matcher, which handles them. Adding the idiom to the `reject`
examples and to `approve`'s `not_for` fixed all ten (now `reject` at 0.92–1.00). That is the shape
of fix the questions file exists for: a rule, not a phrase list.

What did **not** move, and should not be read as jev error: `other` stays at 56/99 because 33 of the
43 misses are `none`-labelled phrases of the affirmative-opener-then-decline shape (`ok but not that
one`, `yeah no`, `sounds good but maybe later`, `absolutely not`, `let's hold off`) which jev calls
**reject at 0.84–1.00**. The dataset labels them `none` because the C++ matcher deliberately returns
*no* intent there (the proposal stays open and the line goes to normal chat); the intent prompt
contract itself (`prompts/proactive_intent.md`) defines exactly these as `reject`. So on the jev tier
those lines **close the proposal as declined** — a behaviour change, and the one the contract asks
for. Under the production predicate (only approve/reject/done ≥ threshold *act*; `other` always
falls to the C++ matcher), the ≥0.8 slice is **256/340 acted (75.3 %)** with 34 "wrong-acted" of
which 33 are that contract class and **1 is a genuine miss** (`yeah im goods` → reject 0.87, a
typo edge the matcher reads as approve). `different` → `other` at 0.80 is harmless for the same
reason.

→ `Jev.ProactiveIntent.MinConfidence = 0.8` shipped as the default (the 0.7–0.8 band adds 3 correct
for 1 wrong). D and E have no dataset; their 0.6 defaults are the conservative placeholder until
live `backend='jev'` rows exist. Re-score a saved run offline with
`bench_jev.py --site <site> --replay <rows.json>`.
