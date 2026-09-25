# Quickstart: Proactive Leader Bot

**Feature**: 002-proactive-leader-bot
**Audience**: developer or operator who wants to (a) enable the feature in production on game-host, or (b) smoke-test it end-to-end in a single play session.

This is the user-side runbook for v1. Implementation details live in `plan.md` / `research.md` / `data-model.md` / `contracts/`.

---

## Prerequisites

1. **mod-ollama-chat installed and running** on game-host (per `docs/agent-context/DEPLOYMENT.md`).
2. **Gateway fleet online**: at least one of Claude (20007) / Clawd (20008) / Geek (20009) must be in `Gateway.BotGUIDs` AND `Mcp.Leader.BotGUIDs`. See `docs/autonomous-bots.md` cold-boot sequence.
3. **Tactical mode enabled**: `OllamaChat.Tactical.Enable = 1` AND `OllamaChat.Tactical.HumanPresenceRequired = 1`. (Both are existing defaults.)
4. **Ollama classifier reachable**: `Gateway.OllamaClassifier.Enable = 1` AND the classifier model loaded (`qwen3:8b` by default).
5. **Your character must be auto-claim whitelisted**: your account must be in `Gateway.AutoClaimAccountIds` so the fleet attaches to your party on login.

---

## Enable the feature

Edit `/azerothcore/env/dist/etc/modules/mod_ollama_chat.conf` on game-host (do NOT edit `.dist`):

```ini
# ----- Proactive Leader Bot (feature 002) -----
OllamaChat.Proactive.Enable                       = 1
OllamaChat.Proactive.PreferredVoiceGuid           = 20007        # Claude (paladin). Set to 0 to use lowest-guid election.
OllamaChat.Proactive.FirstProposalDelaySec        = 15
OllamaChat.Proactive.ProposalCooldownSec          = 120
OllamaChat.Proactive.IdleThresholdSec             = 180
OllamaChat.Proactive.IdleNudgeCooldownSec         = 300
OllamaChat.Proactive.IdleNudgeMaxPerSession       = 3
OllamaChat.Proactive.ProximityYards               = 60.0
OllamaChat.Proactive.ActivityCompleteTimeoutSec   = 1800
OllamaChat.Proactive.ProposalExpirySec            = 600
OllamaChat.Proactive.ProposerPromptFile           = "modules/mod-ollama-chat/prompts/proactive_proposer.md"
```

Then either:

- redeploy via CI/CD (`gh workflow run deploy.yml`), or
- run `.ollama reload` in-game once.

Confirm in logs:

```bash
ssh game-host "grep -i 'proactive' /azerothcore/env/dist/logs/worldserver.log | tail -20"
```

You should see lines like `[Ollama Chat Proactive] enabled (voice=20007, ProposalCooldownSec=120, ...)`.

---

## Smoke-test the happy path (5 minutes)

1. Log in your character next to where Claude (20007) is.
2. Within ~15-25 seconds (login delay + heartbeat), Claude should whisper or party-chat a concrete proposal — for example:

   > **Claude whispers**: Hey Slayo, you've still got Fate of Yenniku open — want to go finish him?

3. Reply `yes` in the same channel.
4. Claude acknowledges and starts moving toward the quest objective (or toward the dungeon entrance, depending on the proposal).
5. Follow Claude. Verify you stay together; if you intentionally stop, Claude should stop within ~10s and whisper a waiting line.
6. Complete or skip the activity:
   - If you finish it: Claude detects the completion event (quest turn-in for quests, zone-out for dungeons) and proposes the next activity within ~`ProposalCooldownSec`.
   - If you want to bail: say `done` or `next` — Claude treats the activity as complete and offers something else.

---

## Smoke-test rejection + cooldown

1. Wait for the next proposal.
2. Reply `nah` or `skip`. Claude should acknowledge and (after the cooldown) propose a **different** activity.
3. Reject 2-3 in a row. Claude should keep finding new candidates and not loop back to the rejected ones immediately.

---

## Smoke-test idle nudge

1. After approving an activity, just stand still and ignore everything.
2. After `IdleThresholdSec` (3 min default) with no movement or chat, Claude should send one short idle nudge ("why are we standing around?").
3. Keep ignoring. Claude should send at most `IdleNudgeMaxPerSession` (3 by default) nudges total in the session, then go quiet.

---

## Smoke-test the kill switch

1. Edit `mod_ollama_chat.conf` and set `OllamaChat.Proactive.Enable = 0`.
2. Run `.ollama reload` in-game.
3. Claude should stop initiating proactive lines. All existing reactive behaviors (responding to your chat, executing leader commands) continue unchanged.

---

## Inspect the audit trail

After a play session, query the lifecycle from the Mac:

```bash
# via the existing wow-admin MCP — same tool the agent uses
mcp__wow-admin__ops_audit_gateway player_guid=<your guid> source_channel_like='proactive_%' limit=50
```

Or directly via SQL on game-host:

```sql
SELECT ts, source_channel, response_text
FROM   acore_characters.mod_ollama_chat_gateway_audit
WHERE  player_guid = <your guid>
  AND  source_channel LIKE 'proactive\_%'
ORDER  BY id DESC
LIMIT  50;
```

You should see a chain like:
```
proactive_proposal → proactive_approval → ... → proactive_complete →
proactive_proposal → proactive_rejection → proactive_proposal → ...
```

---

## Troubleshooting

| Symptom | Likely cause | Fix |
|---|---|---|
| No first proposal after login | `Proactive.Enable=0`, or no whitelisted human in `Gateway.AutoClaimAccountIds`, or no eligible leader bot in range | `grep 'Proactive\|AutoClaim' worldserver.log` |
| Proposals arrive but nothing executes on "yes" | Classifier confidence below `Gateway.OllamaClassifier.MinConfidence`, or "yes" misclassified | Check classifier audit rows; consider lowering `MinConfidence` or rephrasing tighter |
| Bot keeps proposing the same quest after a rejection | Cooldown logic / rejection-memory bug | File: see audit `proactive_rejection` rows; verify the `proposalId` differs from the next `proactive_proposal` |
| Bot wanders far from player | `ProximityYards` set too high, or proximity-check tick not running | Verify `Tactical.HeartbeatMs` and that the bot is in the tactical roster |
| Spammy nudges in capital city | Idle nudges firing while you AFK at the bank | Raise `IdleThresholdSec` or set `IdleNudgeMaxPerSession=0` |
| Activity never auto-completes after quest turn-in | `OnPlayerCompleteQuest` hook not firing or proposalId mismatch | Use `done` as a manual override, then check the audit row for `completionSource` |

---

## Disabling cleanly

```ini
OllamaChat.Proactive.Enable = 0
```

`.ollama reload`. Behavior reverts to today's reactive-only gateway. No DB cleanup required (state is in-memory; audit rows remain for `AuditRetentionDays`).
