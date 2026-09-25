# Proactive Leader Bot — User Guide

Phase 1+2 + US1 MVP live on game-host since 2026-05-12 (PR #112). Phase 3-6 follow-up (US2 proximity + US3 idle nudge + dungeon table + in-zone quest scan + generative phrasing) shipped in the same week.

This guide tells you how to **turn it on**, **see it work**, and **turn it off** — nothing more. Design rationale and full task list live under `specs/002-proactive-leader-bot/`; this doc is the operator runbook.

---

## What it does today

When enabled, a designated gateway leader bot (Claude / Clawd / Geek by default) will:

1. **Open the conversation** within ~15-60 seconds of you logging in by proposing a concrete next activity. Candidate selection runs four ranks in order: (1) quests in your log, preferring same-zone matches, (2) accept-eligible quests in your current zone via `quest_template` scan, (3) a level-appropriate WoTLK 5-man dungeon from the curated table (filtered by faction-side and your current continent), (4) a soft "let's head to <zone>" if everything above is empty.
2. **Wait for your reply.** Say "yes" / "ok" / "let's go" and the bot follows you. Say "no" / "skip" / "different" and it remembers the rejection; the next cycle proposes something else. Direct commands ("follow me", "stay", "attack", "kill it", "hearth", etc.) cancel the open proposal automatically — your direct order takes over.
3. **Stay close while travelling.** If you fall behind (default 60 yards), the bot stops and sends one "you coming?" line, then resumes silently when you close the gap. Hearth to a different continent and the bot abandons travel within one heartbeat; next cycle re-opens in your new context.
4. **Auto-recycle** when an activity finishes. Turn in the proposed quest and the bot detects it; zone out of a proposed dungeon's instance map and the bot detects it; or say "done" / "next" / "finished" at any time and the bot enters the next propose cycle.
5. **Nudge you if you stand around.** When you've been idle for `IdleThresholdSec` (default 3 min) with no open proposal and no executing plan, the bot pings you once. Capped at `IdleNudgeMaxPerSession` (default 3) per session; counter resets the moment you engage (move / chat / approve).
6. **Stop proposing if you stop listening.** After `ProposalMaxPerSession` (default 5) consecutive proposals expire without an answer, the bot stops opening new proposals for the rest of your session — chatting, finishing a quest, or zoning wakes the loop again. Tuned to keep the bot off your back when the party is auto-claim bots or you've alt-tabbed for hours.
6. **Safety-net** after 30 min on the same activity — the bot asks "are we still working on this one?" and aborts if you don't answer within `ActivityCompleteNudgeWaitSec`.
7. **Congratulate you on a level-up.** A short deterministic "nice work, level N!" line fires on a genuine in-play level-up (never at character creation), cooldown-guarded by `LevelUpCongratsCooldownSec` (default 60s) so a rapid multi-level burst doesn't spam it. Zero-cost — no LLM call.

Phrasing is **generative when a proposer prompt is loaded** (`OllamaChat.Proactive.ProposerPromptFile` — points at `prompts/proactive_proposer.md` by default) and the gateway is enabled; the bot will talk like an LLM, varying its phrasing each time. If the gateway is unavailable, or the prompt file path is left **empty**, the loop falls back to short deterministic templates so the propose-approve-execute path still works. A non-empty path that the worldserver *cannot open* is a different case: the loader forces `Proactive.Enable = 0` in-process and the loop does not run at all — see [Troubleshooting](#troubleshooting).

Everything is **opt-in** behind a single config flag. With the flag off, runtime behavior is byte-identical to what was there before.

### Not yet shipped (follow-up PRs)

- Accurate dungeon-entrance coordinates — the dungeon table ships with `0/0/0` placeholders because v1 dispatches `!follow` rather than a "walk to entrance" path. Once travel-to-coord is wired, the entrance coords will populate from `acore_world.areatrigger_teleport`.
- Cross-system ambient rate limiting — the existing tactical ambient counters (`AmbientMinSpeechGapSec` / `AmbientMaxVisibleActionsPerMinute`) are NOT yet consulted from the proactive emit funnel. Per-event `Proactive.*Cooldown*Sec` keys are the v1 spam control.
- Structured intent classifier prompt (`prompts/ollama_classifier.md` extension with `proactive_approve` / `proactive_reject` / `proactive_done` labels). v1 does the same classification with a deterministic C++ string-match path that hits ≥95% on the hand-labelled bench (`tests/proactive/`) without any LLM round-trip — so this follow-up is purely about contract uniformity.
- Battleground proposals — **out of scope for v1** (per Clarifications Q1).

---

## Turn it on

### 1. Edit the live config on game-host

```bash
ssh game-host
sudo -e /azerothcore/env/dist/etc/modules/mod_ollama_chat.conf
# Or any editor you prefer. Do NOT edit the .dist file.
```

Find the **Proactive Leader Bot** block (near the bottom of the file) and flip:

```ini
OllamaChat.Proactive.Enable               = 1

# Pin Claude (paladin) as your voice. 0 = lowest-guid election among
# leader-capable bots in Gateway.BotGUIDs ∩ Mcp.Leader.BotGUIDs.
OllamaChat.Proactive.PreferredVoiceGuid   = 20007
```

Leave every other `OllamaChat.Proactive.*` key at its default for the first session — the defaults are tuned for "single player, full play session". You can tighten/loosen later.

### 2. Reload without restarting the worldserver

In-game GM command:

```
.ollama reload
```

…or from the worldserver console / via the MCP tool:

```text
config_reload { targets: ["mod_ollama_chat"] }
```

You should see the following line in `worldserver.log`:

```
[Ollama Chat Proactive] enabled (voice=20007, ProposalCooldownSec=120, IdleThresholdSec=180, ProximityYards=60, ProposerPromptBytes=...)
```

If you see `disabled (Proactive.Enable=0)` instead, the config didn't reload — re-check the file path and rerun `.ollama reload`.

If you see **neither** line but a `proposer prompt file not found: <path>; disabling feature` error, `ProposerPromptFile` points at a file the worldserver cannot open: the loop has been switched off in-process for this reload. Fix the path — or set the key to `""` — and reload.

---

## Use it

1. Log in your character normally. Make sure your account is in `Gateway.AutoClaimAccountIds` (it is if the fleet auto-joins your party today).
2. Stand near Claude (or whichever bot you pinned as voice). Same map is enough — same zone if you want the proposal to be relevant.
3. **Wait 15-60 seconds** without typing anything. The bot will open with:

   > **Claude whispers**: Hey Slayo, how about we go knock out 'Fate of Yenniku'?

   (Or partied if you're in a group with the bot.)

4. **Reply natural language.** Try:
   - **Approve**: `yes`, `ok`, `let's go`, `do it`, `sure`, `yep`
   - **Reject**: `no`, `skip`, `not that`, `something else`, `nah`
   - **Done** (while an activity is executing): `done`, `next`, `what's next`

5. On `yes`, the bot replies with an ack ("Let's do it!") and starts `!follow`-ing you. You lead it to the quest objective / dungeon entrance — the bot keeps up; existing playerbot strategies handle the rest.

6. On `no`, the bot remembers your rejection. After `ProposalCooldownSec` (default 120s) it proposes a different option.

7. When you turn in the quest, the bot auto-detects completion and re-enters the propose cycle within the next heartbeat. If you instead say "done" or "we're done" mid-activity, that's recorded as `proact_done` with `request_text='playerDone'` (`request_chars=10`); a quest auto-turn-in records `questTurnIn` and a zone-out from a dungeon records `zoneOut`.

### Common variants

- **Player has no quests in log**: bot proposes "let's head to <your current zone>" as a soft fallback. Approve to have the bot follow you while you pick up new quests. If you reject (or silently let expire) that soft zone-suggestion, the bot remembers it and won't re-offer the **same** zone every cooldown — it stays quiet until you walk into a different zone, pick up a quest, or the rejection ages out of the 3-deep memory. (Before this, the fallback rank was the one candidate type with no rejection memory, so "let's head to Durotar" could repeat indefinitely on an explicit "no".)
- **Bot already in your group**: lines go on `/party`. Otherwise `/whisper` directly to you.
- **You ignore the proposal**: it expires after `ProposalExpirySec` (default 10 min) — bot won't pile on. Next heartbeat after expiry opens a fresh one with a **different** candidate (the silently-ignored option is treated as a soft-rejection so it isn't re-proposed in the immediately next cycle). **Hearth/portal to a different continent or instance** while a proposal is still open: the bot drops it immediately (`proact_abort` with `playerChangedMap`) so the next heartbeat re-opens for your new location instead of waiting out the 10-minute expiry. Works for every continent — including Eastern Kingdoms (mapId 0), where the cancel used to be silently suppressed by a stale "not-set" sentinel.
- **The bot can't deliver the proposal line** (its `PlayerbotAI` isn't registered yet right after a spawn/relog, or it briefly left the world): no proposal is opened and no `proact_propose` row is written — the loop just retries on a later heartbeat. You will never see a `proact_abort` `expired` row without a matching `proact_propose`, and a line the player never received does not count toward the per-session self-silence cap or get remembered as a soft-rejection.
- **You answer "yeah, not right now"** (or `ok not now` / `yup not really` / `absolutely no` — any agreement token followed straight by a negation): this is read as a **decline**, not an approval. The intent matcher looks for an approve token in your first three words and tests approve *before* reject, so a leading "yeah"/"ok"/"sure" used as a turn-opener used to win outright and the bot would ack and start executing the activity you had just refused — the worst-possible miss. The opener is now stripped and the rest of your sentence is judged on its own, so `yeah, not right now` → `not right now` → `proact_reject`, and the candidate is remembered as a soft rejection like any other "no". Same for the executing-plan side: `yeah not done yet` is no longer misread as "done". Two deliberate carve-outs: `not a/an <noun>` after the negation stays an **approval** (`sure, not a problem` means yes), and a bare `<token> not` with nothing after it (`absolutely not`) routes to normal chat rather than to reject — bare `not` can't be treated as a reject keyword because it would also fire on `why not`, which is agreement.
- **You start playing without answering** (e.g. you just want to go grind for a bit): existing direct commands still work. Type `follow me`, `stay`, etc. — those take precedence and cancel the open proposal (audit `proact_abort` with `request_text='directCommand'`, `request_chars=13`). The cancelled candidate is also remembered as a soft rejection so the next cycle picks something different.
- **Activity stalls past the safety-net check**: when the bot asks "are we still working on this?" and you don't answer within `ActivityCompleteNudgeWaitSec`, the plan aborts. The candidate is remembered as soft-rejected so the next proposal picks a different one.
- **Bot was waiting on you when the activity ended**: if the bot had stopped to wait for you to catch up ("you coming?") and then the plan ended (you said "done", a quest auto-completed, the safety-net aborted, etc.), the bot resumes following you on the next heartbeat (≤10s) — even when you're already standing right next to it. FR-008 says an idle leader bot stays near the player, so the proactive loop heals any leaked `stay` state from the previous plan instead of leaving the bot glued in place.
- **You approved while the bot was still spawning in** (its `PlayerbotAI` wasn't registered yet — right after a relog or daily fleet cycle): the bot acknowledges and the plan starts, but the opening `!follow` can't be dispatched until the bot's AI is live, so for a heartbeat or two the bot may stand still. The proactive loop heals this on the next tick once the AI is back and you're in range — it issues the deferred `!follow` so the activity actually begins, instead of leaving the plan stuck "executing" with the bot frozen until the 30-min completion-timeout safety net gives up. (Same root cause as the propose-line case above, on the approve→execute side; no chat line or audit row — it's an internal command re-dispatch.)
- **You log out mid-activity**: per FR-016 the bot drops everything on logout — any in-flight ActivityPlan is aborted, any Open or Approved proposal is cancelled (one `proact_abort` audit row per aborted plan + per cancelled proposal, with `request_text='logout'`), and all cadence counters reset. On your next login the cycle starts fresh (no "are we still working on this?" referring to a dungeon from yesterday).
- **The leader bot relogs/restarts mid-activity** (daily fleet cycle, worldserver reboot): same FR-016 cleanup mirrored on the bot side — any plan the bot was executing is aborted (audit `proact_abort` with `request_text='botLogout'`), any Open or Approved proposal it owned is cancelled, and all cadence rows keyed by that bot are wiped. When the bot reconnects (or `ElectDesignatedVoice` switches to a different leader bot), the cycle restarts cleanly instead of resurrecting a stale plan whose `startedAtMs` anchor would otherwise immediately trip the 30-min completion-timeout safety net and whisper about an activity from before the bot's downtime.
- **A second leader-capable bot becomes electable mid-activity** (a lower-guid leader bot wanders onto your map, or you re-point `PreferredVoiceGuid` while a proposal/plan is live): the voice stays **sticky** to the bot that owns the in-flight proposal/plan until it resolves (approve→complete/done, reject, expire, or abort) or that bot leaves your map. This keeps exactly one voice per FR-013 and stops a mid-activity handoff from stranding the owning bot's `follow`/`stay` state — the proximity/abort/audit machinery all key off the elected voice and assume it equals the plan's owner, so a voice that flipped to the newcomer mid-plan would drive the incumbent's plan through the wrong bot and leak the incumbent's cadence + `stay` command (the same shape PR #195 fixed for a bot relog, here for a voice switch with no logout).
- **The owning bot wanders off your map while its proposal is still open** (the one case FR-013 lets the voice re-elect mid-proposal — "the owner leaves range"): a different leader bot becomes your voice, but the absent owner's proposal keeps ticking toward its 10-min expiry (a bot *leaving* your map doesn't cancel its proposal — only *you* changing maps does). When that proposal finally expires, the `proact_abort` `expired` row is attributed to the **bot that opened it**, not whoever happens to be your voice at expiry time — so its `bot_guid` matches the original `proact_propose` `open` row for clean per-bot propose↔expire reconciliation, the self-silence counter accrues on the owner (an innocent newcomer isn't pushed toward a silence it never earned, and the owner can't dodge its own cap), and the soft-rejection is remembered on the owner so it won't re-offer the just-expired candidate the moment it returns. FR-013 requires that a voice handoff "cannot mis-attribute its proximity/abort audit rows to a different bot"; the expiry path now honors that even in the owner-left-range corner the stickiness rule (PR #211) deliberately leaves open.
- **The owning bot wanders off your map while it's mid-*activity*** (an Approved proposal whose plan is already Executing — the executing-plan twin of the open-proposal case just above): a different leader bot becomes your voice, but the plan belongs to the absent owner, the bot that was actually following/travelling you toward the approved goal. Rather than silently re-routing that plan's `follow`/`stay`, its proximity "you coming?" pings, and its complete-check through the bystander voice — which would limp the activity along under the wrong bot until the ~30-min completion-timeout safety net and stamp every proximity / complete-check / abort row with the wrong `bot_guid` — the next heartbeat aborts the plan attributed to the **owner**: one `proact_abort` row with `request_text='ownerLeftRange'` (`request_chars=14`) carrying the *owner's* `bot_guid`, the owner's leaked `stay` released, and a return to the proposal cycle so your new voice can offer a fresh activity (FR-021). FR-013 requires a voice handoff to neither strand the owning bot's `follow`/`stay` state nor mis-attribute its proximity/abort audit rows to a different bot; PR #255 honored this for the open-proposal expiry path (above), and this closes the executing-plan half it deferred (it could only land once the section-2a proximity rewrite of PR #251 had merged).
- **The presence/auto-claim set contains the leader bots themselves** (a test or staging deployment where the bot fleet drives the audit loop, rather than a real human): a leader-capable bot is **never** elected as its own designated voice. Election skips the player's own GUID and falls through to the next in-range leader bot (so a distinct bot proposes to it), or yields no voice at all — and the heartbeat simply skips that player — when it is the only leader-capable bot on its map. Without this guard the lowest-GUID bot would self-elect every heartbeat and whisper/say-to-party a proposal aimed at itself; nobody can answer it, so it just opens → expires → reopens forever, burning a paid planner/proposer gateway call on every generative tick. A human target is never affected (a human is never leader-capable, so it could not be self-elected anyway). The proposer and the target are always distinct entities (FR-013).
- **You log in and immediately go AFK** (no movement, no chat, no quest turn-in): the bot's idle-nudge clock is anchored at login, so after `IdleThresholdSec` (default 3 min) of total silence you still get one nudge — `proact_nudge` with `request_text='idle'`. Without that anchor the never-moved-since-login player would sit in a dead `lastPlayerActivityAtMs == 0` state and never be nudged, violating FR-010.
- **An idle nudge and a fresh proposal never land in the same breath**: the idle nudge is a strict **fallback** — the heartbeat first tries to open a concrete proposal, and only if nothing opens this tick (cooldown not elapsed, self-silenced after 5 expiries, no candidate in your zone, or a Phase B/C `wait` veto) does it fall back to a content-free "why are we standing around? Pick something." nudge. Before this, the nudge was evaluated **before** the proposal, so on the very heartbeat after an open proposal expired you'd get the generic nudge immediately followed by a concrete "how about we run \<X\>?" — two lines in one ~10s tick that read as the bot talking over itself, and the redundant nudge needlessly burned one of your `IdleNudgeMaxPerSession` (default 3) slots plus an extra proposer call. US3's independent test requires each idle nudge to be "clearly differentiated from a fresh activity proposal"; they are now mutually exclusive per heartbeat. (Nothing about *when* a nudge is eligible changed — same idle threshold, cooldown, cap, and no-open-proposal gate.)
- **You approve a dungeon and then hearth/portal away without ever entering it**: the plan is treated as **abandoned**, not completed — the next heartbeat aborts it with `proact_abort` `playerChangedMap` (`request_chars=16`; the bot is left behind on the original map, so the proximity check returns `DifferentMap`, which now emits the cross-map label rather than the coarser `playerChangedZone`). A travel abandonment that crosses only a zone boundary on the **same** map still emits `playerChangedZone` (`request_chars=17`), matching the cross-map label the Open-proposal cancel path uses. Earlier the cross-map `OnZoneChange` hook misread this as the FR-021 "zone-out from the instance map" completion signal and silently flipped the plan to `proact_done` `zoneOut`, falsifying the audit's completion stream for any dungeon proposal you bailed on. The hook now waits to see you actually step into the instance first.
- **You approve a dungeon and then actually enter it**: the plan is **not** aborted. Because v1 dispatches `!follow` (no walk-to-entrance), the bot is usually still on the outside continent the moment you zone in, so the proximity check momentarily reads `DifferentMap` — but now that `OnZoneChange` has marked you as having entered the approved instance, the executing-plan proximity branch **suppresses** that cross-map abort instead of mis-recording your run as a `playerChangedMap` abandonment. The plan stays `Executing` until you zone out (→ `proact_done` `zoneOut`) or the `ActivityCompleteTimeoutSec` safety net fires. Before this, the abort fired on entry and the dungeon `zoneOut` completion could never be reached for a genuine run.

---

## Inspect what the bot's doing

### From the Mac (via the `wow-admin` MCP)

```text
ops_audit_gateway {
  player_guid: <your character guid>,
  source_channel_like: "proact_%",
  limit: 50
}
```

You'll see one row per lifecycle event:

| `source_channel` | Meaning |
|---|---|
| `proact_propose` | A proposal was sent |
| `proact_approve` | You approved one |
| `proact_reject` | You rejected one |
| `proact_nudge` | An ambient line was sent: an idle nudge, a proximity "you coming?", an activity-complete-timeout check, or a level-up congratulation |
| `proact_done` | An activity was completed (quest turn-in, zone-out, or your "done" command) |
| `proact_abort` | A proposal expired/cancelled or a plan aborted (blocker, activity timeout, owner left range) |

The audit table stores only `chars` / `tokens` / `latency` + `source_channel` — not the verbatim chat lines. For the actual text, tail the worldserver log:

> **Which kind of abort was it?** `proact_abort` covers eight different terminal
> transitions, and since the table stores no text the reason is carried by
> `request_chars`. Break an abort spike down with
> `SELECT request_chars, COUNT(*) FROM mod_ollama_chat_gateway_audit WHERE source_channel='proact_abort' GROUP BY 1 ORDER BY 1;`
> and read it off this table:
>
> | `request_chars` | Reason | What it means |
> |---|---|---|
> | 6 | `logout` | You logged out with a proposal/plan in flight |
> | 7 | `expired` | An open proposal sat unanswered until its 10-min expiry |
> | 9 | `botLogout` | The **owning leader bot** relogged/restarted mid-cycle |
> | 13 | `directCommand` | You issued a direct bot command instead of answering |
> | 14 | `ownerLeftRange` | The owning bot left your map mid-activity (FR-013 handoff) |
> | 15 | `activityTimeout` | You approved, then the activity blew `ActivityCompleteTimeoutSec` and you ignored the complete-check nudge |
> | 16 | `playerChangedMap` | You crossed to another map (hearth/portal/zeppelin) |
> | 17 | `playerChangedZone` | You crossed a zone boundary on the **same** map |
>
> `activityTimeout` was called `timeout` before 2026-07-30, which made it 7 chars —
> indistinguishable from `expired`. If you have an older corpus, `request_chars=7`
> there is ambiguous between "ignored the offer" and "approved then stalled"; from
> the rename onward the two are cleanly separable. Distinctness of these lengths is
> now enforced at compile time, so a future reason can't silently collide.

> **Which kind of nudge was it?** `proact_nudge` covers four different ambient
> lines and, like `proact_abort`, carries the reason in `request_chars`:
>
> | `request_chars` | Reason | What it means |
> |---|---|---|
> | 4 | `idle` | You went quiet past `IdleThresholdSec` and the bot opened with something |
> | 7 | `levelUp` | Level-up congratulation (deterministic phrasing only, so `latency_ms` is always 0) |
> | 9 | `proximity` | "You coming?" — you drifted away while an approved plan was executing |
> | 14 | `complete_check` | The activity blew `ActivityCompleteTimeoutSec` and the bot asked whether you're done |
>
> Reading this split matters for two of the bug patterns this subsystem gets
> audited for: a *proximity flood* is many `request_chars=9` rows for one player in
> a short window, and an *idle nudge during combat* is a `request_chars=4` row —
> neither is visible if you only count `proact_nudge` totals. A `14` row that is
> shortly followed by a `proact_abort` `request_chars=15` (`activityTimeout`) is the
> normal give-up sequence, not two separate faults.

> **Which kind of completion was it?** `proact_done` has three paths, same decoder:
>
> | `request_chars` | Reason | What it means |
> |---|---|---|
> | 7 | `zoneOut` | You left the approved instance map — the dungeon-run completion signal |
> | 10 | `playerDone` | You said "done" / "we're done" mid-activity |
> | 11 | `questTurnIn` | The approved quest was auto-detected as turned in |
>
> `zoneOut` only fires once you were seen to actually **enter** the instance. If you
> hearth/portal away before entering, that is a `proact_abort` `playerChangedMap`
> (`request_chars=16`), not a completion — so a `proact_done` rate computed without
> this split used to over-report dungeon successes.

> Proactive owns its **own** audit-write toggle: `OllamaChat.Proactive.EnableAudit` (default `1`). It is **independent** of `OllamaChat.Gateway.EnableAudit` — proactive rows land in `mod_ollama_chat_gateway_audit` regardless of the gateway-wide toggle, so the audit-driven self-improvement loop works on cost-sensitive deployments that keep gateway audit off. Set `OllamaChat.Proactive.EnableAudit = 0` only if you want the `proact_*` rows suppressed.

> `latency_ms` on `proact_propose` and `proact_nudge` rows reflects the **proposer** gateway-call time that phrased the line — the same SLO signal already carried by `proact_planner` / `proact_intent` rows. It is `0` when the deterministic phrasing path produced the line (no LLM call, e.g. `ProactiveProposer.PromptFile` unset or the gateway disabled), and `0` on a propose row whose line came from the planner (that call's time is on its own `proact_planner` row, never double-counted). Track the proposer SLO with e.g. `SELECT source_channel, AVG(latency_ms), MAX(latency_ms) FROM mod_ollama_chat_gateway_audit WHERE source_channel IN ('proact_propose','proact_nudge') AND latency_ms > 0 GROUP BY source_channel`.


```bash
ssh game-host "docker logs ac-worldserver 2>&1 | grep '\[Ollama Chat Proactive\]' | tail -50"
```

…or via MCP:

```text
ops_logs_tail {
  container: "ac-worldserver",
  grep: "[Ollama Chat Proactive]",
  lines: 100
}
```

Sample line you'll see:

```
[Ollama Chat Proactive] emit ch=whisper bot=20007 player=20010 text='Hey Slayo, how about we go knock out 'Fate of Yenniku'?'
[Ollama Chat Proactive] audit src=proact_propose bot=20007 player=20010 req='open' resp='Hey Slayo, ...'
```

---

## Tuning knobs

All under `OllamaChat.Proactive.*` in the same conf file. Most useful ones:

| Key | Default | What it does |
|---|---|---|
| `ProposalCooldownSec` | 120 | Min gap between proposals. Raise to 300+ if you find the bot too talkative. |
| `FirstProposalDelaySec` | 15 | Wait after login. Raise if you want time to settle in before being pinged. |
| `ProposalExpirySec` | 600 | How long an unanswered proposal stays open. |
| `ActivityCompleteTimeoutSec` | 1800 | Bot waits this long on an executing activity before asking "are we done?". |
| `ActivityCompleteNudgeWaitSec` | 120 | After the check nudge, how long before the bot gives up and re-proposes. |
| `PreferredVoiceGuid` | 0 | Pin a specific bot. 0 = lowest-guid election. |
| `ProposerPromptFile` | `prompts/proactive_proposer.md` | Phrases every proactive line generatively when the file loads and `Gateway.Enable = 1`; otherwise deterministic templates. **Must resolve if set** — a non-empty path the worldserver cannot open forces `Proactive.Enable = 0` in-process and the whole loop goes silent. Set it to `""` to keep the loop running on deterministic phrasing only. See Troubleshooting. |
| `LevelUpCongratsEnable` | 1 | Turn the level-up congratulation line on/off. Deterministic-only — no LLM call either way. |
| `LevelUpCongratsCooldownSec` | 60 | Min gap between level-up congratulation lines per (player, elected-voice bot). |
| `NoVoiceAuditEnable` | 1 | Record a `proact_novoice` row when the heartbeat skips you because no voice could be elected. Diagnostic only — no chat line, no LLM call. See Troubleshooting. |
| `NoVoiceAuditCooldownSec` | 300 | Min gap between `proact_novoice` rows per player. Stops a permanently voice-less fleet from flooding the audit table. |

### Intent classifier (Phase A — LLM-ify the proactive loop)

The player-reply path can route through a local-Ollama intent classifier
before falling back to the deterministic substring matcher. Direct-command
tokens ("follow me", "stay", "attack" ...) always short-circuit in C++ —
no LLM round-trip on action commands. All keys are top-level
`OllamaChat.ProactiveIntent.*`:

| Key | Default | What it does |
|---|---|---|
| `ProactiveIntent.Enable` | 1 | Turn the classifier on/off. Off = pure C++ substring matcher (v1 behavior). |
| `ProactiveIntent.PromptFile` | `prompts/proactive_intent.md` | Prompt contract. JSON in, JSON out (`{"intent":"approve\|reject\|done\|other","confidence":0.0}`). |
| `ProactiveIntent.MinConfidence` | 0.7 | Below this confidence the C++ matcher is consulted instead. |
| `ProactiveIntent.TimeoutMs` | 3000 | HTTP timeout — now the budget of the whole chain (jev → LLM → C++ matcher). On timeout the C++ matcher takes over silently. |

**jev tier (2026-09-21).** With `OllamaChat.Jev.Enable=1` and `Jev.ProactiveIntent.Enable=1`,
TypeSafe's jev answers first (`intent` choice{approve,reject,done,other}, capped at 1000 ms because
this runs on the world thread); the LLM above is consulted only when jev is below
`Jev.ProactiveIntent.MinConfidence` *and* ≥1500 ms of the 3000 ms budget remain. The audit row's
`backend` column says which tier answered. See [jev.md](jev.md).

Uses the same local Ollama endpoint as `Gateway.OllamaClassifier.*` — one
Ollama serves both prompts. Audit rows for this path land in
`mod_ollama_chat_gateway_audit` with `source_channel='proact_intent'`.

### Gateway planner (Phase B — proposal decisions)

The propose-tick path can route through the gateway (paid Claude) for an
agent veto + phrasing on top of the C++-picked candidate. The agent reads
an enriched snapshot — player combat / hpPct / idleSec, recentRejectedCount,
sessionAgeSec — and either:

- approves the proposal and writes the chat line (`{"decision":"propose"}`),
- vetoes this tick (`{"decision":"wait"}`).

A `wait` verdict consumes the normal `Proactive.ProposalCooldownSec` so the
agent isn't queried more than once per cooldown window per (bot, player).

| Key | Default | What it does |
|---|---|---|
| `ProactivePlanner.Enable` | 0 | Default OFF (paid Claude). Flip to 1 to route propose ticks through the planner. |
| `ProactivePlanner.PromptFile` | `prompts/proactive_planner.md` | Prompt contract. Snapshot in, `{"decision":"propose\|wait","line":"..."}` out. |

Any failure (gateway down / parse error / Unset decision) falls through to
the v1 deterministic `SelectCandidateForPlayer` + `PhraseLine` path. Audit
rows for this path land in `mod_ollama_chat_gateway_audit` with
`source_channel='proact_planner'` — the `request_chars` column is the
snapshot length and `response_chars` is the raw planner JSON length, with
`latency_ms` for SLO tracking.

### Tick gate (Phase C — speak-now veto)

Above the planner sits an even cheaper layer: a local-Ollama gate that
reads a small player+cadence snapshot (combat, hpPct, idleSec,
recentRejectedCount, sessionAgeSec, lastProposalAgoSec) and decides
`propose` (proceed) or `wait` (skip this tick). Purpose: cheaply suppress
no-go ticks (mid-combat, low HP after a fight, just-logged-in, multiple
recent rejections) BEFORE the paid planner fires.

A `wait` verdict skips the tick but does NOT consume the proposal
cooldown — `IntervalSec` is the cost control for Ollama itself, the
proposal cooldown only applies when we actually emit.

| Key | Default | What it does |
|---|---|---|
| `ProactiveTickGate.Enable` | 0 | Default OFF — bot-spam risk requires opt-in. With `Jev.ProactiveTickGate.Enable=1` jev answers the gate first (`gate` choice{propose,wait}, ≤1200 ms of the 2000 ms budget); the LLM runs only on a low-confidence jev answer with ≥1000 ms left. See [jev.md](jev.md). |
| `ProactiveTickGate.PromptFile` | `prompts/proactive_tick_gate.md` | Snapshot in, `{"action":"propose\|wait","confidence":0.0}` out. |
| `ProactiveTickGate.IntervalSec` | 30 | Minimum seconds between gate calls per (bot, player). Throttle. |
| `ProactiveTickGate.TimeoutMs` | 2000 | HTTP timeout. On timeout we fall through. |
| `ProactiveTickGate.MinConfidence` | 0.6 | Below this the gate's `wait` is ignored; falls through. |

Reuses the same local Ollama endpoint as `Gateway.OllamaClassifier.*` —
one Ollama serves all three prompts (intent, planner-via-gateway, tick
gate). Audit rows for this path land in `mod_ollama_chat_gateway_audit`
with `source_channel='proact_tickgate'`.

Edit + `.ollama reload`. No restart required.

---

## Turn it off

```ini
OllamaChat.Proactive.Enable = 0
```

`.ollama reload`. Within ~5 seconds the bot stops initiating proactive lines. Existing reactive behavior (responding to your chat, leader commands, gateway routes) continues unchanged. No DB cleanup required — all proactive state is in-memory and intentionally non-persistent.

---

## Troubleshooting

| Symptom | Likely cause | Fix |
|---|---|---|
| No first proposal after login | `Proactive.Enable=0`, OR you're not in `Gateway.AutoClaimAccountIds`, OR no leader-capable bot is in range and same-map | **Check `proact_novoice` first** — it tells you which of these it is. `SELECT request_chars, COUNT(*) FROM mod_ollama_chat_gateway_audit WHERE source_channel='proact_novoice' GROUP BY 1;` → `13` = leader bots are online but **none is on your map** (co-locate them, or set `PreferredVoiceGuid` to one that is), `16` = all leader bots offline, `19` = `Mcp.Leader.BotGUIDs` unset, `8` = you ARE the only leader-capable bot. **No `proact_novoice` rows at all** means a voice WAS elected and the silence is downstream (cooldown / tick gate / candidate selection) — check `proact_propose` and `proact_tickgate` instead. Verbatim reason: `ssh game-host "grep '\[Ollama Chat Proactive\]' /azerothcore/env/dist/logs/worldserver.log \| tail"` |
| Nothing at all after login — no proposal, no nudge, and **no `proact_*` row of any channel** (not even `proact_novoice`) | `ProposerPromptFile` is set to a path the worldserver cannot open. The loader logs `proposer prompt file not found: <path>; disabling feature`, forces `Proactive.Enable = 0` in-process and returns **before** the heartbeat runs, so no audit row is written at all. The row above reads "no `proact_novoice` rows → the silence is downstream" — that inference does **not** hold in this case, because the subsystem never started | Check the log first: `ssh game-host "docker logs ac-worldserver 2>&1 \| grep 'proposer prompt file not found'"`. A relative path resolves against the worldserver working directory, so a path that looks right in the conf can still miss. Point the key at a file that exists, or set `ProposerPromptFile = ""` to run the loop on deterministic phrasing. Confirm recovery with the `enabled (… ProposerPromptBytes=<n> …)` line |
| First proposal arrives but nothing happens on "yes" | (Phase A) intent classifier confidence below `ProactiveIntent.MinConfidence` (default 0.7) AND the C++ substring matcher also missed the phrasing, OR your local Ollama (PC at `192.168.100.15:11434`) is offline AND the C++ matcher missed | Check `ops_audit_gateway` for a `proact_intent` row (LLM was consulted) and/or `proact_approve` row (state transitioned). Missing both = the C++ matcher didn't match either; lower `MinConfidence` or add the phrasing to the matcher set. To bypass the LLM entirely: `ProactiveIntent.Enable=0` and rely on C++ only. |
| Bot follows but doesn't pick up the quest | v1 only dispatches `!follow` on approve — quest pickup (`accept *`) is not automatic yet; lead the bot to the quest giver and use the existing leader command if needed | Expected for MVP; quest pickup automation is a follow-up |
| Bot proposes "let's head to <zone>" every time | Your quest log is empty AND in-zone scan (rank 2) isn't shipped yet | Pick up a quest manually; next heartbeat will propose from your log |
| Bot keeps proposing the same quest/dungeon/zone after I said "no" | Rejection-memory holds the last 3 rejected targets across all three candidate types (quest, dungeon, **and** the rank-4 soft zone-suggestion) — should NOT immediately repeat | Check `proact_reject` audit rows; if it's repeating, file a follow-up bug |
| "Spammy" — too many proposals | `ProposalCooldownSec` too low for your taste | Raise to 300+ |
| Bot proposes a quest I can't actually do (faction-wrong, level-locked) | v1 candidate selector doesn't yet filter by level/faction beyond what's in your log | Use `no` and the bot will skip it; selector improves in follow-up |

---

## Architecture (one paragraph for context)

The proactive loop rides on the existing tactical heartbeat — `TacticalLeaderTick::Run()` calls `ollamachat::proactive::Heartbeat()` once per tick. Heartbeat iterates whitelisted-online players, elects a designated voice (an in-flight proposal/plan keeps its owner sticky; otherwise preferred-guid override else lowest-guid among leader-capable in-world bots on the same map — never the player itself), and evaluates state transitions: expire stale proposals, send timeout-check nudges, open a fresh proposal when cooldown allows. Player chat is intercepted at the top of `ProcessChat` — if there's an open proposal or executing plan, the local Ollama classifier is invoked; on a confident approve/reject/done match, the message short-circuits and the state machine transitions. All lifecycle events get one row in `mod_ollama_chat_gateway_audit` with a `proact_*` `source_channel`. Proposals and activity plans are held in-memory only; once a proposal reaches a terminal state (rejected/expired/cancelled/completed/aborted) the same Heartbeat reaps it (and its plan) after a ~60s grace window, so the in-memory state stays bounded over long worldserver uptimes rather than accumulating one dead proposal per cooldown forever. No new threads, no new SQL migration, no new MCP tool — every primitive used here already existed before this feature.

For the full design, see [`specs/002-proactive-leader-bot/`](../specs/002-proactive-leader-bot/).
