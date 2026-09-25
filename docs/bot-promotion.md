# Bot promotion — any playerbot you talk to gets the strategic brain

Only the bots in `OllamaChat.Gateway.BotGUIDs` (Claude 20014, Raz 20015) have a gateway
override, so any other playerbot used to answer your chat with nothing. Promotion gives any
playerbot the same brain as those bots at runtime: the strategic lane (warcraft gateway), the
wow MCP tools, and its own name. You switch it on by talking to the bot, and it switches off
by itself.

Code: `src/mod-ollama-chat_promotion.{h,cpp}`. Config: the `OllamaChat.Gateway.Promote.*` block
in `conf/mod_ollama_chat.conf.dist`.

## The two tiers

| Tier | How a bot gets it | How long | What it gets |
|---|---|---|---|
| **chat** | You whisper it, or name it (whole word) in party, raid, guild, officer, say, yell or General/Trade/LFG | `Promote.ChatTtlSec` (600 s) after your **last** line that addressed it | Strategic-lane replies, all wow MCP tools on its own `botGuid` (gear, bags, loot, gold, skills, quests) |
| **party** | It is in **your** group (you invited it) | Until it leaves the group (you kick it). No timeout | Everything above, plus: you are its playerbots master (follow/stay/attack work), it joins the tactical loop, and the fleet leader's `leader_*` tools and `bot_set_master` can command it |

After a kick the bot is a stock playerbot again, **including** any open chat window. It stays
that way until you whisper it or name it again.

Rules that hold in both tiers:

- **Only you promote.** The line must come from a real (non-bot) player on a
  `Gateway.WhitelistAccountIds` account. An **empty** whitelist turns promotion off entirely;
  otherwise anyone on the realm could spend strategic-lane turns by naming bots.
- **Outside whisper, a promoted bot answers only lines that name it**, whatever
  `PublicChannelMode` says. Without this, the chance roll would make it answer random say or
  General lines during its window. The match is whole-word ("Raz" does not match "crazy").
  Party guests follow the same rule. Claude (`MentionExemptBots`) still hears every party line.
- **Replies:** whisper, party, raid, guild, say and yell are answered in the same channel.
  General, Trade and LFG are answered by **whisper**, so long LLM replies don't flood a
  realm-wide channel.
- **Never promoted:** `Gateway.BotGUIDs` bots (already gateway bots), the fleet
  (`Fleet.LeaderGuid`, `Fleet.MemberGuids`) and the Botmaster (`Mcp.Leader.SystemMasterGuid`).
  Promotion never fights `Fleet.EnsureParty`.
- **Cap:** at most `Promote.MaxChatBots` (5) chat promotions at once. Addressing one more demotes
  the bot you addressed longest ago. Party guests don't count toward the cap.

## Why each piece is shaped this way

- **Template config.** A promoted bot has no entry in `gateway_overrides.json`, and the global
  `Gateway.Url` is empty on this server. So it uses a **copy** of the template bot's override:
  url, token, model (`warcraft`), `gatewayType`, headers and `allowedTools`. The template is set
  by `Promote.TemplateBotGuid`, which defaults to `Fleet.LeaderGuid`. If the template has no
  override, the loader logs an ERROR and promotion stays off. It does not fall back to the empty
  URL, which would produce instant "gateway returned empty response" errors.
- **The registry has its own mutex; the conf maps are never mutated at runtime.** The detached
  gateway workers read `g_GatewayBotConfigs` unlocked, which is only safe because it changes at
  config reload and nowhere else. `FindGatewayBotConfig()` checks the conf map first, then the
  promotion registry, and always returns a copy.
- **Party membership is derived, not stored.** `Promotion::Tick` runs on the world thread every
  5 s. It walks every whitelisted human's (non-battleground) group, so a kick needs no hook and
  the state survives the nightly stop and a worldserver restart (groups persist in the DB).
- **The master rule matches `Fleet.EnsureParty` operator mode.** playerbots keeps a real-player
  master (`UpdateAIGroupMaster` only re-elects when the master is null or a non-self bot), so
  setting the human is stable and doesn't flap.
- **Persona.** The warcraft lane's persona is written for the fleet. For a promoted bot the
  identity hint adds a "WHO YOU ARE NOW" block: speak as *this* character, not the fleet leader,
  answer self-questions from tools, and say whether it is in your party.

## What a chat-only bot cannot do

A random bot outside your group has no master: mod-playerbots clears the master of an
**ungrouped** random bot every AI tick. So a chat-promoted bot can talk and answer about itself,
but movement orders ("follow me") won't stick until you invite it. The persona block tells the
model to say so instead of pretending. Invite it and it becomes a party guest.

## Enable it (live)

```ini
OllamaChat.Gateway.Promote.Enable = 1
# optional — the defaults are what we run:
OllamaChat.Gateway.Promote.ChatTtlSec = 600
OllamaChat.Gateway.Promote.MaxChatBots = 5
OllamaChat.Gateway.Promote.PartyEnable = 1
OllamaChat.Gateway.Promote.TemplateBotGuid = 0
OllamaChat.Gateway.Promote.Channels = "whisper,party,raid,guild,officer,say,yell,general"
```

`config_reload` applies it. `Gateway.AllowedChannels` stays as it is: it governs only the
configured bots, and `Promote.Channels` governs promoted ones.

## Check it

- `.ollama gateway status` (and the ops status JSON `gateway.promotion`) prints
  `promote: enabled=1, template=20014, chat=N, party=M`. `template=… (MISSING)` means no
  override for the template guid.
- Logs (`[Ollama Chat Promote]`): `promoted: chat (600 s idle window)`,
  `promoted: party guest (strategic lane, tactical, fleet)`, `now follows <you>`,
  `demoted: back to a stock playerbot`, `demoted: MaxChatBots=5 reached`.
- A promoted bot's turns land in `mod_ollama_chat_gateway_audit` under its own `bot_guid`.

## Cost

Each addressed line is one strategic-lane turn (DeepSeek Flash on the PI engine). Party guests
also add one tactical-loop bot each (DeepSeek Flash, per tick). A chat window costs nothing
while you're silent: promotion is a flag, not a loop.

Parity with Raz: if Claude says a promoted bot's name in party chat, that bot answers
on the paid lane too, since the fleet shares your account. A bot's line never opens or
refreshes a window, so this doesn't feed itself.
